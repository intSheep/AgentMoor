"""Combine publication faults with the public-HTTP SaaS contract.

SaaS data uses client.request/download/archive (public HTTP). Only ProxyAdmin,
marker mutation and Pod retirement use trusted Kubernetes fixture access.
"""
from copy import deepcopy
from datetime import datetime
import hashlib
import json
import re
import time
import uuid


STAGES = ("begin", "manifest", "part", "commit")
MARKER_PATH = "/run/task/work/state.json"
DIGEST = re.compile(r"[a-f0-9]{64}\Z")
ADMIN_CODE = """import json,sys,urllib.request
method,path,body=sys.argv[1:]
assert path in ('/arm','/status','/ledger','/release')
data=None if body=='null' else body.encode()
request=urllib.request.Request('http://127.0.0.1:7444'+path,data=data,method=method,
 headers={'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=10) as response:
 print(response.read().decode())
"""
MUTATE_CODE = """import hashlib,json,sys
from pathlib import Path
expected,case_id=sys.argv[1:]
p=Path('/run/task/work/state.json')
assert p.is_file() and not p.is_symlink()
before=p.read_bytes()
state=json.loads(before)
assert state['marker']==expected and state['revision']==1
changed='workspace-only-'+hashlib.sha256(case_id.encode()).hexdigest()[:32]
assert changed!=expected
state['marker']=changed
p.write_text(json.dumps(state,sort_keys=True)+'\\n')
after=p.read_bytes()
assert json.loads(after)['marker']==changed and before!=after
print(json.dumps({'path':str(p),'before_sha256':hashlib.sha256(before).hexdigest(),
 'after_sha256':hashlib.sha256(after).hexdigest(),'changed':True}))
"""


def public(client, method, route, body=None):
    return json.loads(client.request(method, route, body))


def event_completed(event):
    # Go time.Time with json omitempty can still expose year-1 zero time.
    # A nonempty string alone is not proof that the proxy handler finished.
    if not event.get("completed_at"):
        return False
    return datetime.fromisoformat(event["completed_at"].replace("Z", "+00:00")) >= datetime.fromisoformat(event["at"].replace("Z", "+00:00"))


class ProxyAdmin:
    """Administration is reachable only inside the dedicated proxy Pod."""
    def __init__(self, kube):
        self.kube = kube

    def request(self, method, route, body=None):
        assert route in ("/arm", "/status", "/ledger", "/release")
        raw = self.kube("exec", "deployment/publication-proxy", "--", "python3", "-c",
                        ADMIN_CODE, method, route, json.dumps(body))
        return json.loads(raw)


def wait_until(read, accept, description, seconds=180, sleep=time.sleep):
    until = time.monotonic() + seconds
    while time.monotonic() < until:
        value = read()
        if accept(value):
            return value
        sleep(.2)
    raise TimeoutError(description)


def require_native_once(pod):
    """Completed containers cannot exec; read their retained termination message."""
    worker, = [s for s in pod.get("status", {}).get("containerStatuses", [])
               if s["name"] == "worker"]
    terminated = worker.get("state", {}).get("terminated")
    assert terminated is not None, "Worker has not terminated"
    message = json.loads(terminated.get("message", ""))
    assert set(message) == {"native_exec_count"} and type(message["native_exec_count"]) is int and message["native_exec_count"] == 1, message
    return 1


def retire_verified(kube, job, retire, *, failed=False, sleep=time.sleep):
    def completed():
        pods = json.loads(kube("get", "pods", "-l", "job-name=" + job, "-o", "json"))["items"]
        assert len(pods) <= 1, "duplicate Worker Pod"
        if not pods:
            return None
        pod, = pods
        statuses = [s for s in pod.get("status", {}).get("containerStatuses", [])
                    if s["name"] == "worker"]
        return pod if len(statuses) == 1 and "terminated" in statuses[0].get("state", {}) else None
    pod = wait_until(completed, bool, "Worker termination message unavailable", sleep=sleep)
    require_native_once(pod)
    worker, = [s for s in pod["status"]["containerStatuses"] if s["name"] == "worker"]
    code = worker["state"]["terminated"]["exitCode"]
    assert (code != 0) if failed else (code == 0), code
    uid = retire(job, failed=failed)
    assert uid == pod["metadata"]["uid"]
    return uid


def capture_history(client):
    """Keep public identities only, never platform DB rows or bucket contents."""
    result = []
    for row in public(client, "GET", "/v1/assets"):
        ref = {key: row[key] for key in ("kind", "id", "version", "sha256", "size", "content_path")}
        raw = client.download(ref)
        assert len(raw) == ref["size"]
        result.append(ref)
    return result


def verify_history(client, history):
    rows = {(r["kind"], r["id"], r["version"]): r for r in public(client, "GET", "/v1/assets")}
    for ref in history:
        actual = rows[ref["kind"], ref["id"], ref["version"]]
        assert all(actual[key] == value for key, value in ref.items())
        raw = client.download(ref)
        assert len(raw) == ref["size"] and hashlib.sha256(raw).hexdigest() == ref["sha256"]
    return len(history)


def validate_ledger(case, *, turn, stage, timing, limit, success, frozen):
    """Use Go-produced op/manifest identities; never emulate Fingerprint in Python."""
    assert case["target"] == {"kind": "workspace", "id": "turns/" + turn + "/root"}
    assert case["stage"] == stage and case["timing"] == timing and case["limit"] == limit
    assert not case["blocked"] and case["consistent"] and not case.get("error")
    assert case["change"] == frozen and DIGEST.fullmatch(case["operation"])
    assert DIGEST.fullmatch(case.get("manifest_digest", ""))
    assert DIGEST.fullmatch(case.get("wire_sha256", ""))
    events = case["events"]
    assert events and all(e["operation"] == case["operation"] for e in events)
    injected = [e for e in events if e["injected"]]
    assert len(injected) == case["hits"] == limit
    assert all(e["stage"] == stage and not e["cancelled"] and not e.get("error") for e in injected)
    for event in events:
        assert event_completed(event), "case ended with an in-flight ledger event"
        assert "?" not in event["path"]
        if event["stage"] == "begin":
            assert event["path"] == "/v1/changes"
        else:
            assert event["path"].startswith("/v1/changes/" + case["operation"] + "/")
        if event["stage"] == "manifest":
            assert event["manifest_digest"] == case["manifest_digest"]
            assert event["wire_sha256"] == case["wire_sha256"]
        if event["stage"] == "part":
            assert DIGEST.fullmatch(event.get("body_sha256", "")) and 0 < event["size"] <= 4 << 20
            assert event["path"].endswith("/parts/" + event["body_sha256"])
        if event in injected:
            code = event.get("upstream_status", 0)
            assert (code == 0) if timing == "before" else (200 <= code < 300), code
        elif event.get("error") or event.get("cancelled"):
            # Cancelling the other upload workers after a selected part fails
            # may interrupt siblings. Keep that proof; don't call it another hit.
            assert stage == "part" and event["stage"] == "part"
            assert event.get("error", "") in ("", "upstream_transport", "upstream_response_incomplete")
        else:
            assert 200 <= event.get("upstream_status", 0) < 300, event
    begins = [e for e in events if e["stage"] == "begin"]
    assert len(begins) == (2 if success else 3), "frozen publication retry count differs"
    if stage == "part":
        assert DIGEST.fullmatch(case.get("part_target", ""))
        assert all(e["body_sha256"] == case["part_target"] for e in injected)
        # after-part may be absent from missing on retry; don't demand two PUTs.
    if not success:
        assert stage == "manifest" and timing == "before" and limit == 3
        assert not any(e["stage"] in ("part", "commit") for e in events)
    return {"hit_count": len(injected),
            "interrupted_sibling_events": sum(bool(e.get("error") or e.get("cancelled")) for e in events if not e["injected"])}


def mutate_marker(kube, pod, marker, case_id):
    result = json.loads(kube("exec", pod["metadata"]["name"], "-c", "worker", "--",
                             "python3", "-c", MUTATE_CODE, marker, case_id))
    assert result["path"] == MARKER_PATH and result["changed"]
    assert result["before_sha256"] != result["after_sha256"]
    return result


def run(client, kube, *, checkpoint, session, marker, expected_operations,
        deliveries, pod_uids, collect, retire, job_for, admin=None,
        mutate=mutate_marker, new_uuid=uuid.uuid4, sleep=time.sleep):
    """After produce/revise, run eight faults, one exhaustion, then recovery.

    `collect` uses a separate scoped --gc credential and public POST /v1/gc.
    `retire(job, failed=False)` waits for the actual Job outcome and deletes it.
    This helper additionally verifies the terminated worker's native count.
    No native/runtime execution is performed by this module itself.
    """
    admin = admin or ProxyAdmin(kube)
    assert callable(collect), "publication faults require a separate public GC callback"
    cp = deepcopy(checkpoint)
    operations, deliveries, pod_uids = list(expected_operations), list(deliveries), list(pod_uids)
    assert operations == ["produce", "revise"]
    assert cp["session_id"] == session
    failed_uids, evidence, used_turns = [], [], set()
    history = capture_history(client)

    def public_checkpoint():
        return public(client, "GET", client.route + "/checkpoint")

    def gc_and_history():
        outcome = collect()
        assert isinstance(outcome, dict) and isinstance(outcome.get("removed"), int)
        return {"gc": outcome, "historical_revisions_verified": verify_history(client, history)}

    def exact_publication(turn, operation, *, succeeded, frozen):
        target = "turns/" + turn + "/root"
        versions = [r for r in public(client, "GET", "/v1/assets") if r["kind"] == "workspace" and r["id"] == target]
        jobs = [r for r in public(client, "GET", "/v1/status") if r["kind"] == "workspace" and r["id"] == target]
        job, = jobs
        assert job["operation"] == operation
        if succeeded:
            revision, = versions
            assert revision["version"] == 1 and job["state"] == "committed"
            assert revision["sha256"] == frozen["sha256"] and revision["size"] == frozen["size"]
            raw = client.download(revision)
            assert len(raw) == frozen["size"] and hashlib.sha256(raw).hexdigest() == frozen["sha256"]
        else:
            assert versions == [] and job["state"] == "pending"
        return versions

    for stage, timing, limit, success in [
            *((stage, timing, 1, True) for stage in STAGES for timing in ("before", "after")),
            ("manifest", "before", 3, False)]:
        turn = str(new_uuid())
        assert str(uuid.UUID(turn)) == turn and turn not in used_turns
        used_turns.add(turn)
        case_id = stage + "-" + timing + ("-exhausted" if not success else "")
        arm = {"case_id": case_id, "stage": stage, "timing": timing, "limit": limit,
               "target": {"kind": "workspace", "id": "turns/" + turn + "/root"}, "hold_first": True}
        assert admin.request("POST", "/arm", arm) == {"armed": True}
        route = client.route + "/turns/" + turn
        question = {"session_id": session, "message": "Audit the saved report after publication fault " + case_id + "."}
        accepted = public(client, "PUT", route, question)
        assert accepted["turn_id"] == turn and accepted["parent"] == cp
        job = job_for(turn)
        held = wait_until(lambda: admin.request("GET", "/status"),
                          lambda c: c and c["case_id"] == case_id and c["blocked"],
                          "publication fault barrier was not reached", sleep=sleep)
        assert held["hits"] == 1 and held["consistent"] and not held.get("error")
        frozen = deepcopy(held["change"])
        assert frozen["kind"] == "workspace" and frozen["id"] == arm["target"]["id"]
        assert frozen["format"] == "workspace-tar.v1" and frozen["media_type"] == "application/x-tar"
        assert DIGEST.fullmatch(frozen["sha256"]) and frozen["size"] > 0 and frozen["base_version"] == 0
        active = public(client, "GET", route)
        assert active["state"] == "running" and active["parent"] == cp and not active.get("assets")
        assert public_checkpoint() == cp
        # after-commit may already expose its revision; CP must still be old.
        pods = json.loads(kube("get", "pods", "-l", "job-name=" + job, "-o", "json"))["items"]
        pod, = pods
        assert pod.get("status", {}).get("phase") == "Running"
        try:
            mutation = mutate(kube, pod, marker, case_id)
            assert mutation["path"] == MARKER_PATH and mutation["changed"]
            assert mutation["before_sha256"] != mutation["after_sha256"]
            assert public_checkpoint() == cp
            during = gc_and_history()
        finally:
            assert admin.request("POST", "/release") == {"released": True}
        if success:
            row, _ = client.answer(turn)
            assert row["parent"] == cp and row["state"] == "succeeded"
        else:
            row = wait_until(lambda: public(client, "GET", route),
                             lambda r: r["state"] not in ("queued", "running"),
                             "exhausted publication did not terminate", sleep=sleep)
            assert row["state"] == "failed" and row["parent"] == cp and not row.get("assets")
            assert public_checkpoint() == cp
        case = wait_until(lambda: admin.request("GET", "/status"),
                          lambda c: c["case_id"] == case_id and not c["blocked"] and
                          bool(c["events"]) and
                          all(event_completed(e) for e in c["events"]),
                          "publication ledger still has in-flight events", sleep=sleep)
        proof = validate_ledger(case, turn=turn, stage=stage, timing=timing,
                                limit=limit, success=success, frozen=frozen)
        uid = retire_verified(kube, job, retire, failed=not success, sleep=sleep)
        exact_publication(turn, case["operation"], succeeded=success, frozen=frozen)
        if success:
            operations.append("audit")
            _, delivery = client.observe(row, session, "audit", marker,
                                          expected_operations=list(operations))
            files = client.archive(client.refs(row)["workspace"])
            assert json.loads(files["state.json"])["marker"] == marker
            next_cp = public_checkpoint()
            refs = client.refs(row)
            assert next_cp["session_id"] == session and next_cp["workspace"] == refs["workspace"]
            assert next_cp["session"] == refs["session"] and next_cp.get("skills") == cp.get("skills")
            assert next_cp != cp
            cp = next_cp
            deliveries.append((turn, delivery))
            pod_uids.append(uid)
        else:
            failed_uids.append(uid)
        verify_history(client, history)
        history = capture_history(client)
        evidence.append({"turn_id": turn, "case_id": case_id, "ledger": case,
                         "frozen_change": frozen, "marker_mutation": mutation,
                         "checkpoint_unchanged_at_barrier": True, "native_exec_count": 1,
                         "pod_uid": uid, "outcome": row["state"], "barrier_gc": during, **proof})

    # A failed audit's live tool-events must not enter the next restored CP.
    recovery = str(new_uuid())
    assert str(uuid.UUID(recovery)) == recovery and recovery not in used_turns
    question = {"session_id": session, "message": "Audit the saved report after exhausted publication recovered from the last checkpoint."}
    accepted = public(client, "PUT", client.route + "/turns/" + recovery, question)
    assert accepted["turn_id"] == recovery and accepted["parent"] == cp
    recovered, _ = client.answer(recovery)
    assert recovered["parent"] == cp and recovered["state"] == "succeeded"
    uid = retire_verified(kube, job_for(recovery), retire, sleep=sleep)
    operations.append("audit")
    _, delivery = client.observe(recovered, session, "audit", marker,
                                  expected_operations=list(operations))
    refs = client.refs(recovered)
    next_cp = public_checkpoint()
    assert next_cp["session_id"] == session and next_cp["workspace"] == refs["workspace"]
    assert next_cp["session"] == refs["session"] and next_cp.get("skills") == cp.get("skills")
    assert next_cp != cp
    cp = next_cp
    pod_uids.append(uid)
    deliveries.append((recovery, delivery))
    verify_history(client, history)
    history = capture_history(client)
    after = gc_and_history()
    ledger = admin.request("GET", "/ledger")
    assert len(ledger) == len(evidence) == 9
    assert {c["case_id"] for c in ledger} == {e["case_id"] for e in evidence}
    assert {c["case_id"]: c for c in ledger} == {e["case_id"]: e["ledger"] for e in evidence}, "retired case ledger changed after its proof was captured"
    assert len(pod_uids) == len(set(pod_uids))
    assert len(set(pod_uids + failed_uids)) == len(pod_uids) + len(failed_uids)
    result = {"checkpoint": cp, "expected_operations": operations, "deliveries": deliveries,
              "pod_uids": pod_uids, "failed_pod_uids": failed_uids, "history": history,
              "cases": evidence, "recovery_turn": recovery, "after_gc": after}
    return result

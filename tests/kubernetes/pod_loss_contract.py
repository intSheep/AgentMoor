"""Kill an active Worker without acknowledgement and wait for its real deadline.

Only the fault injector uses Kubernetes/container-runtime access. Business state,
checkpoints, native history, and file bytes are verified through public HTTP.
"""
from datetime import datetime, timezone
import hashlib
import json
from pathlib import PurePosixPath
import re
import subprocess
import time


def command(*args):
    return subprocess.check_output(args, timeout=30).strip()


def run(client, kube, temp, cert, wait_job, publication_hook=None, queued_restart=None, collect=None):
    if publication_hook:
        from publication_fault_contract import require_native_once, verify_history
    native_execution_counts = {}
    def stage(name, **values):
        print("POD_LOSS_STAGE " + json.dumps({"stage": name, **values}, sort_keys=True), flush=True)

    def record(route):
        return json.loads(client.request("GET", route))

    def checkpoint():
        return record(client.route + "/checkpoint")

    def job_for(turn):
        until = time.monotonic() + 90
        while time.monotonic() < until:
            jobs = json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
            assert len(jobs) <= 1, "duplicate Job for the same turn"
            if jobs:
                return jobs[0]["metadata"]["name"]
            time.sleep(.3)
        raise TimeoutError("follow-up Job was not dispatched")

    def pods_for(job):
        return json.loads(kube("get", "pods", "-l", "job-name=" + job, "-o", "json"))["items"]

    def retire(job, failed=False):
        if failed:
            until = time.monotonic() + 90
            while time.monotonic() < until:
                status = json.loads(kube("get", "job", job, "-o", "json")).get("status", {})
                if status.get("failed"):
                    break
                time.sleep(.3)
            else:
                raise TimeoutError("failed publication Worker did not exit")
        else:
            wait_job(kube, job)
        pod, = pods_for(job)
        if publication_hook:
            require_native_once(pod)
            native_execution_counts[pod["metadata"]["uid"]] = 1
        assert any(v["name"] == "private-work" and "emptyDir" in v for v in pod["spec"]["volumes"])
        kube("delete", "job", job, "--cascade=foreground", "--wait=true")
        assert not pods_for(job)
        return pod["metadata"]["uid"]

    def active_pod(job):
        until = time.monotonic() + 90
        while time.monotonic() < until:
            for pod in pods_for(job):
                if pod.get("status", {}).get("phase") != "Running":
                    continue
                try:
                    started = kube("exec", pod["metadata"]["name"], "--", "python3", "-c",
                                   "from pathlib import Path; print(Path('/run/task/work/.hold-started').is_file())")
                except subprocess.SubprocessError:
                    continue
                if started == "True":
                    return pod
            time.sleep(.4)
        raise TimeoutError("MCP render never entered its active hold")

    def kill_worker(pod):
        # SIGTERM lets the Worker acknowledge a failure, so inject SIGKILL from
        # the enclosing disposable kind node's PID namespace instead. Do not
        # use a shell-generated command or address a non-test Docker container.
        node = pod["spec"]["nodeName"]
        assert re.fullmatch(r"agentmoor-[0-9a-f]{12}-control-plane", node), node
        status, = [s for s in pod["status"]["containerStatuses"] if s["name"] == "worker"]
        container = status["containerID"].removeprefix("containerd://")
        assert re.fullmatch(r"[0-9a-f]{64}", container), container
        details = json.loads(command("docker", "exec", node, "crictl", "inspect", container))
        pid = int(details["info"]["pid"])
        assert pid > 1 and details["status"]["state"] == "CONTAINER_RUNNING"
        argv0 = command("docker", "exec", node, "cat", f"/proc/{pid}/cmdline").split(b"\0")[0].decode()
        assert PurePosixPath(argv0).name == "agentmoor", argv0
        command("docker", "exec", node, "kill", "-KILL", str(pid))
        until = time.monotonic() + 20
        while time.monotonic() < until:
            details = json.loads(command("docker", "exec", node, "crictl", "inspect", container))
            if details["status"]["state"] == "CONTAINER_EXITED":
                assert details["status"]["exitCode"] == 137, details["status"]
                return
            time.sleep(.2)
        raise TimeoutError("SIGKILL did not terminate the Worker container")

    deliveries, pod_uids = [], []
    failed_pod_uids, history, publication_cases = [], [], []
    expected_operations = ["produce"]

    def protect_history():
        if publication_hook:
            if collect:
                collect()
            verify_history(client, history)

    client.submit()
    client.await_result()
    pod_uids.append(retire("produce"))
    first = record(client.route)
    cp = checkpoint()
    session = cp["session_id"]
    marker, initial = client.observe(first, session, "produce")
    client.ingest(first, initial, temp, cert)
    deliveries.append(("", initial))
    stage("initial_report_verified", pod_uid=pod_uids[-1])

    revision_message = "Revise the saved report by appending a west refund of 9 cents."
    revision = queued_restart(client, session, revision_message) if queued_restart else client.ask(session, revision_message)
    revised, _ = client.answer(revision)
    assert revised["parent"] == cp
    pod_uids.append(retire(job_for(revision)))
    expected_operations.append("revise")
    _, revised_delivery = client.observe(revised, session, "revise", marker, expected_operations=list(expected_operations))
    client.ingest(revised, revised_delivery, temp, cert)
    deliveries.append((revision, revised_delivery))
    cp = checkpoint()
    stage("revised_checkpoint_verified", pod_uid=pod_uids[-1])

    if publication_hook:
        prior_operations, prior_pods = list(expected_operations), list(pod_uids)
        state = publication_hook(client, kube, checkpoint=cp, session=session, marker=marker,
                                 expected_operations=list(expected_operations), deliveries=list(deliveries),
                                 pod_uids=list(pod_uids), collect=collect, retire=retire, job_for=job_for)
        cp, expected_operations = state["checkpoint"], state["expected_operations"]
        deliveries, pod_uids = state["deliveries"], state["pod_uids"]
        failed_pod_uids, history, publication_cases = state["failed_pod_uids"], state["history"], state["cases"]
        assert cp == checkpoint() and cp["session_id"] == session
        assert expected_operations[:len(prior_operations)] == prior_operations
        assert pod_uids[:len(prior_pods)] == prior_pods
        assert len(pod_uids) == len(expected_operations) == len(deliveries)
        assert len(set([*pod_uids, *failed_pod_uids])) == len(pod_uids) + len(failed_pod_uids)
        protect_history()
        stage("publication_faults_verified", successful_rounds=len(pod_uids), failed_rounds=len(failed_pod_uids))

    message = "Lose the active report Pod during its tool call."
    lost = client.ask(session, message)
    route = client.route + "/turns/" + lost
    job = job_for(lost)
    pod = active_pod(job)
    active = record(route)
    assert active["state"] == "running" and active["parent"] == cp and not active.get("assets")
    declared_deadline = datetime.fromisoformat(active["deadline"].replace("Z", "+00:00"))
    remaining = (declared_deadline - datetime.now(timezone.utc)).total_seconds()
    assert 0 < remaining <= 900, remaining
    stage("tool_active", pod_uid=pod["metadata"]["uid"], deadline=active["deadline"])
    if publication_hook:
        # The active wrapper writes a decimal count outside the capture root.
        # Read it before SIGKILL; finished Pods use terminated.message instead.
        native_count = int(kube("exec", pod["metadata"]["name"], "-c", "worker", "--", "python3", "-c",
                                "from pathlib import Path; print(int(Path('/run/task/native-executions').read_text().strip()))"))
        assert native_count == 1, native_count
        native_execution_counts[pod["metadata"]["uid"]] = native_count
    kill_worker(pod)
    kube("delete", "job", job, "--cascade=foreground", "--wait=true", "--grace-period=0")
    assert not pods_for(job)
    assert record(route)["state"] == "running", "Worker acknowledged instead of disappearing"
    assert checkpoint() == cp
    client.verify_stored(deliveries, temp)
    protect_history()
    stage("worker_lost_without_acknowledgement", exit_code=137, deadline=active["deadline"])

    # There is no test clock, shortened timeout, direct database write, or
    # replacement Worker. The public deadline is the source of truth.
    until = time.monotonic() + remaining + 20
    next_progress = 0
    while time.monotonic() < until:
        row = record(route)
        assert row["parent"] == cp and not row.get("assets")
        now = datetime.now(timezone.utc)
        if row["state"] == "interrupted":
            assert now >= declared_deadline
            interrupted_at = now.isoformat()
            break
        assert row["state"] == "running", row["state"]
        assert checkpoint() == cp
        if time.monotonic() >= next_progress:
            stage("waiting_for_production_deadline", remaining_seconds=max(0, round((declared_deadline - now).total_seconds())),
                  state=row["state"])
            next_progress = time.monotonic() + 55
        time.sleep(2)
    else:
        raise TimeoutError("lost Worker did not become interrupted after its declared deadline")
    stage("interrupted", observed_at=interrupted_at)

    question = {"session_id": session, "message": message}
    for _ in range(2):
        replay = json.loads(client.request("PUT", route, question))
        assert replay == row
    assert not json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + lost, "-o", "json"))["items"]
    assert checkpoint() == cp
    client.verify_stored(deliveries, temp)

    protect_history()
    recovered_turn = client.ask(session, "Audit the saved report after the Worker Pod disappeared.")
    recovered, _ = client.answer(recovered_turn)
    assert recovered["parent"] == cp
    recovery_uid = retire(job_for(recovered_turn))
    recovery_operations = [*expected_operations, "audit"]
    _, recovered_delivery = client.observe(recovered, session, "audit", marker,
                                          expected_operations=recovery_operations)
    recovered_files = client.archive(client.refs(recovered)["workspace"])
    assert ".hold-started" not in recovered_files and "unpublished-draft.txt" not in recovered_files
    deliveries.append((recovered_turn, recovered_delivery))
    client.verify_stored(deliveries, temp)
    protect_history()
    all_pod_uids = [*pod_uids, *failed_pod_uids, pod["metadata"]["uid"], recovery_uid]
    assert len(pod_uids) == len(expected_operations)
    assert len(set(all_pod_uids)) == len(all_pod_uids) == len(expected_operations) + len(failed_pod_uids) + 2
    result = {
        "cli": "codex-cli 0.171.0-alpha.13", "model": "scripted-responses",
        "production_timeout_seconds": 900, "deadline": active["deadline"],
        "interrupted_observed_at": interrupted_at,
        "successful_pod_uids": pod_uids, "lost_pod_uid": pod["metadata"]["uid"],
        "recovery_pod_uid": recovery_uid, "worker_exit_code": 137,
        "original_uuid_replays": 2, "original_uuid_recreated_jobs": 0,
        "session_sha256": hashlib.sha256(session.encode()).hexdigest(),
        "checkpoint_preserved": True, "unpublished_files_restored": False,
        "business_rows": 2, "artifacts": [ref for _, (ref, _) in deliveries],
    }
    if publication_hook:
        assert set(native_execution_counts) == set(all_pod_uids)
        result.update(publication_faults=publication_cases, failed_publication_pod_uids=failed_pod_uids,
                      expected_operations=recovery_operations, native_execution_counts=native_execution_counts,
                      historical_revision_count=len(history),
                      gc_scope=("24-hour history/pending protection; orphan expiry positive control remains in service tests" if collect else "not_run"))
    print("POD_LOSS_RESULT " + json.dumps(result, sort_keys=True), flush=True)

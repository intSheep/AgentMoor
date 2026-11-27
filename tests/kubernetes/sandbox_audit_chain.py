"""Join a real isolated Codex SaaS delivery to separately read operator audit pages."""
import copy
import json
import ssl
import time

from gvisor_probe import require
from sandbox_provider_smoke import CONTROL_NS, ProviderSmoke, ROOT, main as provider_main, sha
from saas_contract import ReportClient


def audit_hash(domain, value):
    return sha(("agentmoor.audit.v1/" + domain + "\0" + value).encode())


def only(events, action):
    selected = [event for event in events if event["action"] == action]
    require(len(selected) == 1, "audit chain lacks a unique " + action)
    return selected[0]


def validate_chain(events, scenario, scope):
    """Compare identities from public receipts, never infer a Turn from a path."""
    require(events and all(event["scope"] == scope for event in events), "operator page crossed its exact scope")
    cursors = [event["cursor"] for event in events]
    require(cursors == sorted(set(cursors)), "operator cursors repeated or moved backwards")
    rounds = scenario["rounds"]
    accepted = [event for event in events if event["action"] == "sandbox.result_accepted"]
    require(len(accepted) == len(rounds) == 3, "successful isolated rounds do not have exactly three accepted Runs")
    session_digest = audit_hash("session-id", scenario["checkpoint"]["session_id"])
    joined = []
    for public in rounds:
        require(public["task_id"] == scope["runtime"] and public["execution_policy"] == "untrusted-isolated.v1",
                "public receipt was not this isolated allocation")
        turn = public["turn_id"]
        receipt = only([event for event in accepted if event.get("turn_id", "") == turn], "sandbox.result_accepted")
        require(receipt["task_id"] == public["task_id"] and receipt["pod_uid"] == public["pod_uid"] and
                receipt["component"] == "task-store" and receipt["actor"]["source"] == "http" and
                receipt["actor"]["role"] == "execute" and receipt["actor"].get("credential_sha256") and
                receipt["previous_state"] == "running" and receipt["state"] == "succeeded",
                "accepted Run does not bind the actual public receipt and observed Pod")
        attempt, run_id = receipt["attempt_sha256"], receipt["run_id"]
        execution = [event for event in events if event.get("run_id") == run_id]
        require(all(event.get("turn_id", "") == turn and event.get("attempt_sha256") == attempt for event in execution),
                "Run chain crossed attempts or Turns")
        lifecycle = {action: only(execution, "sandbox." + action) for action in
                     ("begun", "input_sealed", "create_fenced", "template_sealed", "pod_bound",
                      "output_declared", "output_sealed", "terminated", "deleted")}
        require(lifecycle["begun"]["digest"] == lifecycle["input_sealed"]["digest"] and
                lifecycle["output_declared"]["digest"] == lifecycle["output_sealed"]["digest"] == receipt["digest"],
                "input or output digest changed before receipt acceptance")
        for action in ("pod_bound", "terminated", "deleted"):
            require(lifecycle[action]["pod_uid"] == public["pod_uid"], "lifecycle adopted a different Pod UID")
        require(lifecycle["terminated"]["cursor"] < lifecycle["deleted"]["cursor"] < receipt["cursor"] and
                lifecycle["output_sealed"]["cursor"] < lifecycle["deleted"]["cursor"] and
                lifecycle["deleted"]["state"] == "ready", "success preceded its recorded terminal/delete/ready conditions")
        for action in ("output_declared", "output_sealed"):
            require(lifecycle[action]["actor"] == {"source": "guest-capability", "role": "output"},
                    "guest output was attributed to a controller or credential")
        reservations = [event for event in execution if event["action"] == "model.reserved"]
        require(reservations and all(event["actor"] == {"source": "guest-capability", "role": "model"}
                                     for event in reservations), "model delegation identity is missing or incorrect")
        task_events = [event for event in events if event.get("turn_id", "") == turn and
                       event.get("attempt_sha256") == attempt]
        success = only(task_events, "turn.succeeded" if turn else "task.succeeded")
        links = [event for event in task_events if event["action"] == "asset.linked"]
        require(len(links) == len(public["assets"]), "not every accepted asset has exactly one link")
        expected = {(ref["kind"], audit_hash("asset-id/" + ref["kind"], ref["id"]), ref["version"], ref["sha256"])
                    for ref in public["assets"]}
        actual = {(event["asset_kind"], event["asset_id_sha256"], event["version"], event["digest"]) for event in links}
        require(actual == expected and len(actual) == len(links), "asset links substituted a version, identity or digest")
        require(success["actor"] == receipt["actor"] and all(event["actor"] == receipt["actor"] and
                event["session_id_sha256"] == session_digest and event["cursor"] > receipt["cursor"] for event in links),
                "accepted result and asset links lack the same authenticated Finish identity")
        joined.append({"task_id": public["task_id"], "turn_id": turn, "run_id": run_id,
                       "pod_uid": public["pod_uid"], "attempt_sha256": attempt,
                       "sealed_output_sha256": receipt["digest"], "accepted_cursor": receipt["cursor"],
                       "asset_link_cursors": [event["cursor"] for event in links],
                       "model_reservation_cursors": [event["cursor"] for event in reservations]})
    require(len({row["run_id"] for row in joined}) == 3 and len({row["pod_uid"] for row in joined}) == 3,
            "continuations reused a Run or Pod")
    return joined


class AuditSmoke(ProviderSmoke):
    def build(self):
        relative = "tests/kubernetes/sandbox_audit_chain.py"
        require(not self.p.run("git", "-C", ROOT, "status", "--short", "--", relative).strip(),
                "commit the audit orchestrator before actual acceptance")
        super().build()
        self.result["fixture_sha256"][relative] = sha((ROOT / relative).read_bytes())

    def operator_pages(self, scope, limit=7):
        # This is the trusted cluster orchestrator, separate from ReportClient.
        # No SaaS network route, credential or private writer connection is used.
        events, after, pages, total = [], 0, [], None
        for _ in range(256):
            args = ["exec", "agentmoor", "-c", "api", "--", "agentmoor", "audit", "--data", "/data"]
            for key, value in scope.items():
                args.extend(("--" + key, value))
            args.extend(("--after", str(after), "--limit", str(limit)))
            page = json.loads(self.kube(*args, timeout=30))
            require(set(page) == {"events", "next", "total"} and len(page["events"]) <= limit,
                    "operator CLI returned an unexpected or unbounded page")
            if total is None:
                total = page["total"]
            require(total == page["total"], "audit unexpectedly mutated during fixed completed execution")
            pages.append({"after": after, "next": page["next"], "count": len(page["events"]), "total": total})
            if not page["events"]:
                require(page["next"] == after and len(events) == total, "pagination omitted audit rows or reset its cursor")
                return events, pages
            require(page["events"][0]["cursor"] > after and page["next"] == page["events"][-1]["cursor"],
                    "operator page moved backwards")
            events.extend(page["events"])
            after = page["next"]
        raise RuntimeError("audit fixture exceeded its bounded page count")

    def execute(self, scenarios):
        require(tuple(scenarios) == ("text",), "audit chain fixture supports only its independent text scenario")
        super().execute(scenarios)
        self.stage("operator_only_audit_chain_and_public_uuid_replay")
        scope, scenario = self.scopes["text"], self.result["scenarios"][0]
        events, pages = self.operator_pages(scope)
        joins = validate_chain(events, scenario, scope)
        require(len(pages) > 2, "audit pagination was not exercised")
        raw = json.dumps(events, sort_keys=True).encode()
        execute_token = (self.p.directory / "text-execute-token").read_text().strip()
        execute_identity = audit_hash("credential", execute_token)
        require(all(event["actor"].get("credential_sha256") == execute_identity for event in events
                    if event["action"] in ("sandbox.result_accepted", "asset.linked")),
                "successful receipts were attributed to a different platform credential")
        sensitive = [self.model_key.read_text().strip(), execute_token,
                     scenario["checkpoint"]["session_id"], "sbx1.", "BEGIN PRIVATE KEY", "Bearer ",
                     scenario["rounds"][1]["answer"]["confirmation"], scenario["rounds"][2]["answer"]["check"]]
        require(all(value and value.encode() not in raw for value in sensitive), "operator audit retained sensitive fixture content")
        foreign = {**scope, "tenant": "foreign-audit-fixture"}
        other, foreign_pages = self.operator_pages(foreign)
        require(not other, "operator exact-scope query included another allocation")
        # Repeat original public request UUIDs, not new questions or attempts.
        from contract import forward
        with forward(self.p.base, CONTROL_NS) as endpoint:
            client = ReportClient(endpoint, self.token(scope, "--submit"), scope["runtime"], ssl.create_default_context(cafile=str(self.cert)))
            initial = json.loads(client.request("GET", client.route))
            require(json.loads(client.request("PUT", client.route, initial["spec"])) == initial, "initial request replay changed its receipt")
            session = scenario["checkpoint"]["session_id"]
            for public in scenario["rounds"][1:]:
                answer = public["answer"]
                message = ("Repeat the totals from the original CSV. Confirmation " + answer["confirmation"] if public["operation"] == "confirmation"
                           else "Recall the last confirmation and totals. Check " + answer["check"])
                route = client.route + "/turns/" + public["turn_id"]
                before = json.loads(client.request("GET", route))
                require(json.loads(client.request("PUT", route, {"session_id": session, "message": message})) == before,
                        "original Turn UUID replay changed the committed receipt")
        time.sleep(2.2)
        after, replay_pages = self.operator_pages(scope)
        require(after == events, "public UUID replay appended a logical audit event")
        controls = []
        for action, field, value in (("sandbox.result_accepted", "run_id", joins[1]["run_id"]),
                                     ("asset.linked", "version", -1),
                                     ("sandbox.output_sealed", "actor", {"source": "controller", "role": "system"})):
            changed = copy.deepcopy(events)
            target = next(event for event in changed if event["action"] == action)
            target[field] = value
            try:
                validate_chain(changed, scenario, scope)
            except RuntimeError:
                controls.append(action + "/" + field)
            else:
                raise RuntimeError("proof verifier accepted substituted audit identity: " + action + "/" + field)
        self.result["audit_chain"] = {"scope": scope, "operator_reader": "agentmoor audit --data /data (read-only, no signing key/token)",
            "page_limit": 7, "pages": pages, "events": events, "joins": joins,
            "expected_execute_credential_sha256": execute_identity,
            "foreign_scope_pages": foreign_pages, "public_original_uuid_replays": 3,
            "replay_pages": replay_pages, "events_after_replay_unchanged": True,
            "proof_verifier_rejected_tampering": controls,
            "native_finish_ack_replay_boundary": "Finish ACK replay is tested separately in platform HTTP tests; these are public submit UUID replays.",
            "publication_boundary": "A trusted Worker derives the published refs; this proof does not claim server-side reconstruction of every output asset byte."}

if __name__ == "__main__":
    raise SystemExit(provider_main(AuditSmoke, ("text",), "text"))

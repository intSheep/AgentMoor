"""Cluster lifecycle and fault injection around the HTTP-only file report client."""
import hashlib
import json
import subprocess
import time

from saas_file_contract import FileReportClient


def run(client, kube, temp, cert, wait_job, negative, queued_restart=None):
    def retire(job, failed=False):
        if failed:
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                status = json.loads(kube("get", "job", job, "-o", "json")).get("status", {})
                if status.get("failed"):
                    break
                time.sleep(.3)
            assert status.get("failed"), "failed task worker did not exit"
        else:
            wait_job(kube, job)
        query = ("get", "pods", "-l", "job-name=" + job, "-o", "json")
        pod, = json.loads(kube(*query))["items"]
        assert any(v["name"] == "private-work" and "emptyDir" in v for v in pod["spec"]["volumes"])
        uid = pod["metadata"]["uid"]
        kube("delete", "job", job, "--cascade=foreground", "--wait=true")
        assert not json.loads(kube(*query))["items"]
        return uid

    def job_for(turn):
        jobs = json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
        job, = jobs
        return job["metadata"]["name"]

    def terminal(consumer, route, expected, seconds=90):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            row = json.loads(consumer.request("GET", route))
            if row["state"] not in ("queued", "running", "cancelling"):
                assert row["state"] == expected and not row.get("assets"), row
                return row
            time.sleep(.3)
        raise TimeoutError("task did not enter " + expected)

    def active_tool(turn):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            pods = json.loads(kube("get", "pods", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
            for pod in pods:
                if pod.get("status", {}).get("phase") != "Running":
                    continue
                # This reads only the fault-injection synchronization flag. All
                # business acceptance remains in the HTTP-only SaaS client.
                try:
                    started = kube("exec", pod["metadata"]["name"], "--", "python3", "-c",
                                   "from pathlib import Path; print(Path('/run/task/work/.hold-started').is_file())")
                except subprocess.SubprocessError:
                    continue
                if started == "True":
                    return
            time.sleep(.4)
        raise TimeoutError("MCP render never became active")

    deliveries, pod_uids = [], []
    client.submit()
    try:
        client.await_result()
        pod_uids.append(retire("produce"))
        first = json.loads(client.request("GET", client.route))
        cp = json.loads(client.request("GET", client.route + "/checkpoint"))
        session = cp["session_id"]
        marker, initial = client.observe(first, session, "produce")
        client.ingest(first, initial, temp, cert)
        deliveries.append(("", initial))
        for operation, message in (("revise", "Revise the saved report by appending a west refund of 9 cents."),
                                   ("audit", "Audit the saved report and the restored workspace marker.")):
            turn = queued_restart(client, session, message) if queued_restart and operation == "revise" else client.ask(session, message)
            row, _ = client.answer(turn)
            assert row["parent"] == cp
            pod_uids.append(retire(job_for(turn)))
            _, delivery = client.observe(row, session, operation, marker)
            deliveries.append((turn, delivery))
            if operation == "revise":
                client.ingest(row, delivery, temp, cert, fail_write=True)
            next_cp = json.loads(client.request("GET", client.route + "/checkpoint"))
            assert next_cp["session_id"] == session and next_cp["workspace"] != cp["workspace"]
            cp = next_cp
        assert len(set(pod_uids)) == 3
        client.ingest(first, initial, temp, cert, existing=True)
        client.verify_stored(deliveries, temp)
        logs = kube("logs", "deployment/model")
        for operation in ("produce", "revise", "audit"):
            assert "FILE_MODEL " + operation + " written" in logs

        # Keep the exact native parent session, but simulate missing restored
        # files inside the fixture before revision. This is not a production
        # Worker flag and does not pretend that conversation history is enough.
        lost = client.ask(session, "Simulate missing restored workspace and revise the saved report.")
        lost_row = terminal(client, client.route + "/turns/" + lost, "failed")
        assert lost_row["parent"] == cp
        retire(job_for(lost), failed=True)
        assert json.loads(client.request("GET", client.route + "/checkpoint")) == cp
        client.verify_stored(deliveries, temp)

        held = client.ask(session, "Hold the report tool until cancelled.")
        held_route = client.route + "/turns/" + held
        active_tool(held)
        begin = time.monotonic()
        assert json.loads(client.request("POST", held_route + "/cancel"))["state"] == "cancelling"
        cancelled = terminal(client, held_route, "cancelled", seconds=25)
        assert cancelled["parent"] == cp and time.monotonic() - begin < 25
        held_job = job_for(held)
        wait_job(kube, held_job, seconds=max(1, 25 - (time.monotonic() - begin)))
        assert time.monotonic() - begin < 25
        retire(held_job)
        assert json.loads(client.request("GET", client.route + "/checkpoint")) == cp

        # A new UUID and Pod must still restore the last committed files after
        # both failed attempts; dirty, uncommitted tool files must disappear.
        recovery = client.ask(session, "Audit the saved report after cancellation.")
        recovered, _ = client.answer(recovery)
        assert recovered["parent"] == cp
        recovery_uid = retire(job_for(recovery))
        _, recovered_delivery = client.observe(recovered, session, "audit", marker,
                                              expected_operations=["produce", "revise", "audit", "audit"])
        recovered_workspace = client.archive(client.refs(recovered)["workspace"])
        assert ".hold-started" not in recovered_workspace and "unpublished-draft.txt" not in recovered_workspace
        deliveries.append((recovery, recovered_delivery))
        client.verify_stored(deliveries, temp)

        # A separate fresh workspace must fail explicitly for missing input,
        # even when the model's final answer says done.
        missing = FileReportClient(client.endpoint, negative[0], "cancel-job", client.ssl_context)
        missing.spec.update(instruction="Missing input report: use missing_input.", input="")
        negative[1]()
        missing.submit()
        terminal(missing, missing.route, "failed")
        retire("missing-input", failed=True)
        print("FILE_REPORT_RESULT " + json.dumps({
            "pod_uids": pod_uids, "recovery_pod_uid": recovery_uid,
            "session_sha256": hashlib.sha256(session.encode()).hexdigest(),
            "artifacts": [ref for _, (ref, _) in deliveries], "business_rows": 2,
            "database_write_retry": "trigger-failure-then-imported-and-already-imported",
            "workspace_loss_simulation": "same-session-required-files-removed-before-tool",
            "active_mcp_cancel": "cancelled-with-checkpoint-preserved",
            "missing_input": "failed-without-assets", "model": "scripted-responses",
            "cli": "codex-cli 0.171.0-alpha.13"}, sort_keys=True), flush=True)
    except Exception:
        print(kube("logs", "deployment/model")[-2500:], flush=True)
        for pod in json.loads(kube("get", "pods", "-o", "json"))["items"]:
            if "job-name" in pod["metadata"].get("labels", {}):
                print(kube("logs", "pod/" + pod["metadata"]["name"])[-2500:], flush=True)
        raise

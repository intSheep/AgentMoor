"""File report SaaS acceptance. Agent state is read only through public versioned HTTP assets."""
import hashlib
import io
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tarfile

from saas_contract import ReportClient

INGEST = Path(__file__).resolve().parents[1] / "examples/report-consumer/ingest.py"


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


class FileReportClient(ReportClient):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.region = "east"
        self.spec.update(mode="file-turn.v1", instruction="Produce the file report from input.txt.",
                         input=f"region,amount_cents\neast,{self.amount}\nwest,31\neast,-7\n")

    def refs(self, row):
        prefix = "turns/" + row["turn_id"] + "/" if row.get("turn_id") else ""
        refs = {}
        for kind in ("artifact", "session", "workspace"):
            candidates = [ref for ref in row["assets"] if ref["kind"] == kind
                          and (kind != "artifact" or ref["id"] == prefix + "out/report.json")]
            ref, = candidates
            refs[kind] = ref
        return refs

    def download(self, ref):
        raw = self.request("GET", ref["content_path"])
        assert hashlib.sha256(raw).hexdigest() == ref["sha256"]
        return raw

    def verify(self, row):
        ref = self.refs(row)["artifact"]
        assert self.download(ref) == canonical({"totals_cents": {"east": self.amount - 7, "west": 31}})
        return ref

    def archive(self, ref):
        with tarfile.open(fileobj=io.BytesIO(self.download(ref))) as archive:
            return {entry.name.removeprefix("./"): archive.extractfile(entry).read()
                    for entry in archive if entry.isfile()}

    def observe(self, row, session, operation, marker=None, expected_operations=None, policy=None):
        refs = self.refs(row)
        turn = row.get("turn_id", "")
        prefix = "turns/" + turn + "/" if turn else ""
        assert refs["artifact"]["id"] == prefix + "out/report.json"
        assert refs["session"]["id"] == prefix + session
        workspace = self.archive(refs["workspace"])
        state = json.loads(workspace["state.json"])
        assert state["marker"].startswith("workspace-only-") and len(state["marker"]) == 47
        if marker is not None:
            assert state["marker"] == marker
        expected_revision = int(operation != "produce")
        assert state["revision"] == expected_revision
        expected_csv = self.spec["input"] + ("west,-9\n" if expected_revision else "")
        assert workspace["sales.csv"].decode() == expected_csv
        events = [json.loads(line) for line in workspace["tool-events.jsonl"].splitlines()]
        operations = ["produce", "revise", "audit"]
        assert [e["operation"] for e in events] == (expected_operations or operations[:operations.index(operation) + 1])
        assert all(e["tool"] == "report.render" and e["input"] == "sales.csv" for e in events)
        expected = {"totals_cents": {"east": self.amount - 7, "west": 22 if expected_revision else 31}}
        if operation == "audit":
            expected.update(workspace_marker=state["marker"], revision=1)
        raw = self.download(refs["artifact"])
        assert raw == canonical(expected) and raw == workspace["out/report.json"]
        artifacts = [ref for ref in row["assets"] if ref["kind"] == "artifact"]
        expected_artifacts = {prefix + "out/report.json", prefix + "out/tool-proof.json"}
        if policy is not None:
            expected_artifacts.add(prefix + "out/skill-proof.json")
        assert {ref["id"] for ref in artifacts} == expected_artifacts
        for ref in artifacts:
            assert self.download(ref) == workspace[ref["id"].removeprefix(prefix)]
        histories = self.archive(refs["session"])
        rollouts = [data for name, data in histories.items() if name.endswith(".jsonl")]
        assert len(rollouts) == 1 and state["marker"].encode() not in rollouts[0]
        native = [json.loads(line) for line in rollouts[0].splitlines()]
        assert native[0]["type"] == "session_meta" and native[0]["payload"]["id"] == session
        messages = [event["payload"] for event in native if event["type"] == "response_item"
                    and event["payload"].get("type") == "message"
                    and event["payload"].get("role") == "assistant"]
        assert messages[-1]["content"][-1]["text"] == "done"
        calls = [event["payload"] for event in native if event["type"] == "response_item"
                 and event["payload"].get("type") == "function_call"]
        expected_arguments = {"operation": operation}
        if policy is not None:
            expected_arguments["policy"] = policy
        assert any("render" in call.get("name", "") and json.loads(call["arguments"]) == expected_arguments
                   for call in calls)
        return state["marker"], (refs["artifact"], raw)

    def ingest(self, row, delivery, directory, cert, existing=False, fail_write=False):
        ref, raw = delivery
        expected = directory / "expected.json"
        # Compute the business expectation from SaaS-owned input, never the Agent delivery.
        expected.write_bytes(canonical({"totals_cents": {"east": self.amount - 7,
                                                       "west": 22 if row.get("turn_id") else 31}}))
        args = [sys.executable, str(INGEST), "--endpoint", self.endpoint, "--scope", "saas/reports",
                "--task", row["id"], "--expected", str(expected), "--db", str(directory / "business.db")]
        if row.get("turn_id"):
            args.extend(["--turn", row["turn_id"]])
        env = {"AGENTMOOR_READ_TOKEN": self.token, "SSL_CERT_FILE": str(cert)}
        if fail_write:
            db_path = directory / "business.db"
            with sqlite3.connect(db_path) as db:
                before = db.execute("SELECT * FROM imported_reports ORDER BY artifact_id").fetchall()
                db.execute("CREATE TRIGGER fail_report_insert BEFORE INSERT ON imported_reports "
                           "BEGIN SELECT RAISE(ABORT, 'fixture-write-failure'); END")
            failed = subprocess.run(args, env=env, capture_output=True, text=True, timeout=20)
            assert failed.returncode == 1 and "fixture-write-failure" in failed.stderr
            with sqlite3.connect(db_path) as db:
                assert db.execute("SELECT * FROM imported_reports ORDER BY artifact_id").fetchall() == before
                db.execute("DROP TRIGGER fail_report_insert")
        statuses = [json.loads(subprocess.check_output(args, env=env, timeout=20)) for _ in range(2)]
        assert [s["status"] for s in statuses] == (["already-imported"] * 2 if existing else ["imported", "already-imported"])
        assert all(s["sha256"] == ref["sha256"] for s in statuses)

    def verify_stored(self, deliveries, directory):
        with sqlite3.connect(directory / "business.db") as db:
            rows = db.execute("SELECT turn_id, artifact_id, version, sha256, report_json FROM imported_reports").fetchall()
        assert len(rows) == 2
        for turn, (ref, raw) in deliveries:
            assert self.download(ref) == raw
            if len(rows) and "workspace_marker" not in json.loads(raw):
                assert (turn, ref["id"], ref["version"], ref["sha256"], raw.decode().strip()) in rows

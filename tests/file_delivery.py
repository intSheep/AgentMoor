"""Business acceptance for two file deliveries, using only the public HTTP API."""
import hashlib
import io
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tarfile


INGEST = Path(__file__).resolve().parents[1] / "examples/report-consumer/ingest.py"


def accept(client, row, session, west, turn, directory, cert):
    refs = {ref["kind"]: ref for ref in row["assets"]}
    artifact = refs["artifact"]
    prefix = f"turns/{turn}/" if turn else ""
    assert artifact["id"] == prefix + "out/report.json" and refs["session"]["id"] == prefix + session
    raw = client.request("GET", artifact["content_path"])
    expected = {"totals_cents": {"east": client.amount - 7, "west": west}}
    canonical = (json.dumps(expected, sort_keys=True, separators=(",", ":")) + "\n").encode()
    assert raw == canonical and hashlib.sha256(raw).hexdigest() == artifact["sha256"]

    # Verify the actual native final message independently of the model fixture.
    history = client.request("GET", refs["session"]["content_path"])
    assert hashlib.sha256(history).hexdigest() == refs["session"]["sha256"]
    with tarfile.open(fileobj=io.BytesIO(history)) as archive:
        rollouts = [entry for entry in archive if entry.isfile() and entry.name.endswith(".jsonl")]
        assert len(rollouts) == 1
        events = [json.loads(line) for line in archive.extractfile(rollouts[0])]
    assert events[0]["type"] == "session_meta" and events[0]["payload"]["id"] == session
    messages = [event["payload"] for event in events if event["type"] == "response_item"
                and event["payload"].get("type") == "message" and event["payload"].get("role") == "assistant"]
    final = json.loads(messages[-1]["content"][-1]["text"])
    assert final == {"totals_cents": {"east": -999, "west": west}} and final != expected

    expected_path = directory / "expected.json"
    args = [sys.executable, str(INGEST), "--endpoint", client.endpoint, "--scope", "saas/reports",
            "--task", row["id"], "--expected", str(expected_path), "--db", str(directory / "business.db")]
    if turn:
        args.extend(["--turn", turn])
    env = {"AGENTMOOR_READ_TOKEN": client.token, "SSL_CERT_FILE": str(cert)}
    expected_path.write_text(json.dumps(final))
    reject = subprocess.run(args, env=env, capture_output=True, text=True, timeout=20)
    assert reject.returncode == 1 and "independent expected totals" in reject.stderr
    expected_path.write_text(json.dumps(expected))
    statuses = [json.loads(subprocess.check_output(args, env=env, timeout=20)) for _ in range(2)]
    assert [r["status"] for r in statuses] == ["imported", "already-imported"]
    assert all(r["sha256"] == artifact["sha256"] for r in statuses)
    return artifact, raw


def verify_stored(client, deliveries, directory):
    with sqlite3.connect(directory / "business.db") as db:
        rows = db.execute("SELECT turn_id, artifact_id, version, sha256, report_json FROM imported_reports").fetchall()
    assert len(rows) == len(deliveries)
    for turn, (ref, raw) in deliveries:
        assert client.request("GET", ref["content_path"]) == raw
        assert (turn, ref["id"], ref["version"], ref["sha256"], raw.decode().strip()) in rows

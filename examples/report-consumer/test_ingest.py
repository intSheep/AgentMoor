"""Exercise the consumer CLI over HTTP and inspect the resulting business database."""

import hashlib
import http.server
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.parse

INGEST = Path(__file__).with_name("ingest.py")
FIRST = "/v1/tasks/sample"
TURN = "00502e58-c2ca-482e-b741-a468024b29e0"
FOLLOWUP = FIRST + "/turns/" + TURN


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.requests.append((self.path, self.headers.get("Authorization")))
        status, value = self.server.routes.get(self.path, (404, b"not found"))
        self.send_response(status)
        if status == 302:
            self.send_header("Location", value.decode())
        self.end_headers()
        self.wfile.write(value)

    def log_message(self, *args):
        pass


class ConsumerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.db = self.directory / "reports.db"
        self.expected = self.directory / "expected.json"
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.routes = {}
        self.server.requests = []
        thread = threading.Thread(target=self.server.serve_forever)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"

    def receipt(self, cents, *, turn=None, version=1, raw=None):
        report = {"totals_cents": {"east": cents, "west": -4}}
        if raw is None:
            raw = json.dumps(report).encode()
        asset = "report.json" if turn is None else f"turns/{turn}/report.json"
        query = urllib.parse.urlencode({"kind": "artifact", "id": asset, "version": version})
        content = "/v1/content?" + query
        record = {"state": "succeeded", "id": "sample", "assets": [
            {"kind": "artifact", "id": asset, "version": version,
             "sha256": hashlib.sha256(raw).hexdigest(), "content_path": content},
            {"kind": "workspace", "id": "workspace"},
        ]}
        if turn:
            record["turn_id"] = turn
        route = FIRST if turn is None else FOLLOWUP
        self.server.routes[content] = (200, raw)
        self.server.routes[route] = (200, json.dumps(record).encode())
        self.expected.write_text(json.dumps(report), encoding="utf-8")
        return content, record

    def run_import(self, *, turn=None, scope="tenant-a", success=True, error=None):
        cmd = [sys.executable, str(INGEST), "--endpoint", self.endpoint, "--task", "sample",
               "--scope", scope, "--expected", str(self.expected), "--db", str(self.db)]
        if turn:
            cmd += ["--turn", turn]
        result = subprocess.run(cmd, env={**os.environ, "AGENTMOOR_READ_TOKEN": "fake-read-token"},
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0 if success else 1, result.stderr)
        if success:
            return json.loads(result.stdout)
        if error:
            self.assertIn(error, result.stderr)

    def rows(self):
        with sqlite3.connect(self.db) as conn:
            return conn.execute("""SELECT scope, turn_id, version, report_json
                FROM imported_reports ORDER BY scope, turn_id, version""").fetchall()

    def test_changed_followup_and_late_replay_preserve_both_versions(self):
        self.receipt(31)
        self.assertEqual(self.run_import()["status"], "imported")
        self.receipt(40, turn=TURN)
        self.assertEqual(self.run_import(turn=TURN)["status"], "imported")
        self.receipt(31)
        self.assertEqual(self.run_import()["status"], "already-imported")
        self.assertEqual([(scope, turn, version, json.loads(body)["totals_cents"]["east"])
                          for scope, turn, version, body in self.rows()],
                         [("tenant-a", "", 1, 31), ("tenant-a", TURN, 1, 40)])
        self.assertTrue(all(auth == "Bearer fake-read-token" for _, auth in self.server.requests))

    def test_same_version_different_verified_bytes_rejected(self):
        self.receipt(31)
        self.run_import()
        self.receipt(32)
        self.run_import(success=False, error="different report")
        self.assertEqual(json.loads(self.rows()[0][-1])["totals_cents"]["east"], 31)

    def test_distinct_local_scopes_do_not_collide(self):
        self.receipt(31)
        self.run_import()
        self.receipt(32)
        self.run_import(scope="tenant-b")
        self.assertEqual([row[0] for row in self.rows()], ["tenant-a", "tenant-b"])

    def test_correct_hash_does_not_bypass_business_validation(self):
        self.receipt(32)
        self.expected.write_text('{"totals_cents":{"east":31,"west":-4}}')
        self.run_import(success=False, error="independent expected")
        self.assertFalse(self.db.exists())

    def test_hash_mismatch_even_when_business_value_matches(self):
        content, _ = self.receipt(31)
        self.server.routes[content] = (200, b'{"totals_cents":{"east":31,"west":-4}}')
        self.run_import(success=False, error="SHA-256 mismatch")
        self.assertFalse(self.db.exists())

    def test_duplicate_keys_and_boolean_as_integer_are_rejected(self):
        self.receipt(31, raw=b'{"totals_cents":{"east":31,"east":31,"west":-4}}')
        self.run_import(success=False, error="duplicate JSON field")
        self.receipt(1, raw=b'{"totals_cents":{"east":true,"west":-4}}')
        self.run_import(success=False, error="integer cents")
        self.assertFalse(self.db.exists())

    def test_content_ref_must_select_exact_version_without_redirect(self):
        content, record = self.receipt(31)
        for unsafe in (self.endpoint + content, content.replace("version=1", "version=2"),
                       "/v1/content", content + "&version=1", "//elsewhere.invalid" + content):
            with self.subTest(unsafe=unsafe):
                record["assets"][0]["content_path"] = unsafe
                self.server.routes[FIRST] = (200, json.dumps(record).encode())
                self.server.requests.clear()
                self.run_import(success=False)
                self.assertEqual([route for route, _ in self.server.requests], [FIRST])
        content, _ = self.receipt(31)
        self.server.routes[content] = (302, (self.endpoint + "/no-redirect").encode())
        self.server.requests.clear()
        self.run_import(success=False, error="must not redirect")
        self.assertEqual([route for route, _ in self.server.requests], [FIRST, content])
        self.assertFalse(self.db.exists())

    def test_cancelled_record_never_imported(self):
        _, record = self.receipt(31)
        record["state"] = "cancelled"
        self.server.routes[FIRST] = (200, json.dumps(record).encode())
        self.run_import(success=False, error="successful record")
        self.assertFalse(self.db.exists())


if __name__ == "__main__":
    unittest.main()

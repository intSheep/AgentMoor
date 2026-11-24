"""Exercise the file report tool directly or through a local Codex Responses fixture."""

import argparse
import hashlib
import json
from http.server import ThreadingHTTPServer
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest

from fixture_provider import Handler

EXAMPLE = Path(__file__).resolve().parents[1] / "examples/file-report"
COMMAND = "python3 -I summarize.py sales.csv out/report.json"
PROMPT = "Use summarize.py to read sales.csv and create out/report.json. Reply with the status."


class FileToolModel(Handler):
    """Request one fixed command, acknowledge its result, and never open the workspace."""

    def do_POST(self):
        if self.path != "/v1/responses":
            return self.send_error(404)
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.server.calls == 0:
            names = [tool.get("name") for tool in body["tools"]]
            if "exec_command" not in names:
                self.server.error = f"Codex did not offer exec_command: {names}"
                return self.send_error(400)
            item = {"type": "function_call", "id": "fc_report", "call_id": "call_report",
                    "name": "exec_command", "arguments": json.dumps({"cmd": COMMAND, "login": False})}
        else:
            outputs = [part for part in body["input"] if part.get("type") == "function_call_output"
                       and part.get("call_id") == "call_report"]
            if len(outputs) != 1 or self.server.calls != 1:
                self.server.error = "expected one matching Codex tool result"
                return self.send_error(400)
            result = "failed" if "report:" in str(outputs[0]["output"]) else "done"
            item = {"type": "message", "id": "msg_status", "role": "assistant",
                    "content": [{"type": "output_text", "text": result, "annotations": []}]}
        self.server.calls += 1
        item["status"] = "completed"
        response = {"id": f"resp_{self.server.calls}", "object": "response", "created_at": 1,
                    "model": body["model"], "status": "completed", "output": [item]}
        events = [
            ("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
            ("response.output_item.done", {"output_index": 0, "item": item}),
            ("response.completed", {"response": response}),
        ]
        self.events([(name, {"type": name, "sequence_number": i, **value})
                     for i, (name, value) in enumerate(events)], named=True)


class ReportTest(unittest.TestCase):
    codex = None

    def setUp(self):
        tmp = tempfile.TemporaryDirectory(prefix="agentmoor-file-report-")
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        self.workspace = root / "workspace"
        self.workspace.mkdir()
        (self.workspace / "out").mkdir()
        self.source = self.workspace / "sales.csv"
        self.report = self.workspace / "out/report.json"
        shutil.copyfile(EXAMPLE / "summarize.py", self.workspace / "summarize.py")
        if self.codex:
            self.home = root / "home"
            (self.home / "codex").mkdir(parents=True)

    def invoke(self, expected_exit):
        if not self.codex:
            proc = subprocess.run([sys.executable, "-I", "summarize.py", "sales.csv", "out/report.json"],
                                  cwd=self.workspace, text=True, capture_output=True, timeout=10)
            self.assertEqual(proc.returncode, expected_exit, proc.stderr)
            return proc.stderr

        with ThreadingHTTPServer(("127.0.0.1", 0), FileToolModel) as server:
            server.calls = 0
            server.error = ""
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            endpoint = f"http://127.0.0.1:{server.server_port}/v1"
            args = [self.codex, "exec", "--json", "--ephemeral", "--skip-git-repo-check",
                    "--sandbox", "workspace-write", "--disable", "apps", "--disable", "hooks",
                    "--disable", "multi_agent", "-c", 'web_search="disabled"', "-m", "gpt-5",
                    "-c", 'model_provider="fixture"', "-c", 'model_providers.fixture.name="Fixture"',
                    "-c", f'model_providers.fixture.base_url="{endpoint}"',
                    "-c", 'model_providers.fixture.wire_api="responses"', PROMPT]
            env = {"HOME": str(self.home), "CODEX_HOME": str(self.home / "codex"),
                   "PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin", "LANG": "C",
                   "SHELL": "/bin/sh", "NO_PROXY": "127.0.0.1"}
            try:
                proc = subprocess.run(args, env=env, cwd=self.workspace, stdin=subprocess.DEVNULL,
                                      capture_output=True, text=True, timeout=60)
            finally:
                server.shutdown()
                thread.join()
        self.assertFalse(server.error, server.error)
        self.assertEqual(proc.returncode, 0, proc.stderr + proc.stdout)
        self.assertEqual(server.calls, 2, proc.stdout)
        events = [json.loads(line) for line in proc.stdout.splitlines()]
        completed = [event["item"] for event in events if event["type"] == "item.completed"]
        commands = [item for item in completed if item["type"] == "command_execution"]
        self.assertEqual(len(commands), 1, completed)
        self.assertIn(COMMAND, commands[0]["command"])
        self.assertEqual(commands[0]["exit_code"], expected_exit, commands[0])
        self.assertEqual([item["text"] for item in completed if item["type"] == "agent_message"],
                         ["failed" if expected_exit else "done"])
        return commands[0]["aggregated_output"]

    def test_sales_and_refund(self):
        extra = secrets.randbelow(900000) + 100000
        shutil.copyfile(EXAMPLE / "sales.csv", self.source)
        with self.source.open("a", encoding="ascii") as stream:
            stream.write(f"west,{extra}\n")
        self.source.chmod(0o444)
        self.invoke(0)
        actual = self.report.read_bytes()
        expected = ('{"totals_cents":{"east":98,"west":%d}}\n' % (31 + extra)).encode("ascii")
        self.assertEqual(actual, expected)
        print(f"verified {len(actual)} bytes, SHA-256 {hashlib.sha256(actual).hexdigest()}, "
              f"east=98 west={31 + extra}", flush=True)

    def test_missing_input(self):
        self.assertIn("sales.csv", self.invoke(1))
        self.assertFalse(self.report.exists())

    def test_malformed_row(self):
        self.source.write_text("region,amount_cents\neast,105\neast,1.5\n", encoding="ascii")
        self.assertIn("invalid sale on line 3", self.invoke(1))
        self.assertFalse(self.report.exists())

    def test_existing_report(self):
        shutil.copyfile(EXAMPLE / "sales.csv", self.source)
        self.report.write_bytes(b"prior report\n")
        self.assertIn("File exists", self.invoke(1))
        self.assertEqual(self.report.read_bytes(), b"prior report\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", type=Path, help="run the native CLI with a loopback scripted model")
    args, remaining = parser.parse_known_args()
    if args.codex:
        ReportTest.codex = args.codex.resolve(strict=True)
    unittest.main(argv=[sys.argv[0], *remaining])

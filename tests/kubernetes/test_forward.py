"""Offline subprocess tests: no Kubernetes cluster or network listener needed."""
import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import contract


FAKE_FORWARD = r'''
import json
import os
from pathlib import Path
import signal
import sys
import time

state, mode = Path(sys.argv[1]), sys.argv[2]
requested = sys.argv[sys.argv.index("port-forward") + 2].split(":")[0]
starts = state / "starts.jsonl"
with starts.open("a") as record:
    record.write(json.dumps({"pid": os.getpid(), "requested": requested}) + "\n")
number = len(starts.read_text().splitlines())
if mode == "fail" or mode == "fail-second" and number == 2:
    print("owned fixture stdout startup diagnostic", flush=True)
    print("owned fixture stderr startup diagnostic", file=sys.stderr, flush=True)
    sys.exit(7)
port = requested or "41234"
ready = "Forwarding from 127.0.0.1:" + port + " -> 7401\n"
if mode == "split":
    prefix = ready.index(":") + 3
    os.write(1, ready[:prefix].encode())
    time.sleep(.05)
    os.write(1, ready[prefix:].encode())
else:
    print(ready, end="", flush=True)
if mode == "flood":
    # Well beyond the pipe capacity on both macOS and Linux. A reader that
    # consumes only the readiness line leaves this child blocked before work.
    sys.stdout.buffer.write(b"Handling connection for 7401\n" * 150_000)
    sys.stdout.buffer.flush()
print("owned fixture stderr after readiness", file=sys.stderr, flush=True)
(state / ("work-" + str(os.getpid()))).write_text("subsequent work completed")
signal.pause()
'''


class ForwardTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="agentmoor-forward-test-")
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name)
        self.script = self.state / "fake_forward.py"
        self.script.write_text(FAKE_FORWARD)
        self.processes = []
        original = subprocess.Popen

        def start(*args, **kwargs):
            process = original(*args, **kwargs)
            self.processes.append(process)
            return process

        patcher = mock.patch.object(contract.subprocess, "Popen", side_effect=start)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.require_stopped)

    def require_stopped(self):
        for process in self.processes:
            self.assertIsNotNone(process.poll(), "owned fake forward child was not stopped")

    def base(self, mode):
        return [sys.executable, str(self.script), str(self.state), mode]

    def work_completed(self, process):
        marker = self.state / ("work-" + str(process.pid))
        deadline = time.monotonic() + 2
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(.01)
        self.assertTrue(marker.exists(), "fake child stalled after stdout exceeded pipe capacity")

    def test_chatty_child_keeps_progress_and_reconnects(self):
        with contract.forward(self.base("flood"), "owned-test") as endpoint:
            self.assertEqual(endpoint, "https://127.0.0.1:41234")
            first, = self.processes
            self.work_completed(first)
            endpoint.reconnect()
            self.assertEqual(endpoint, "https://127.0.0.1:41234")
            self.assertEqual(len(self.processes), 2)
            self.assertIsNotNone(first.poll())
            self.work_completed(self.processes[-1])
            starts = [json.loads(line) for line in (self.state / "starts.jsonl").read_text().splitlines()]
            self.assertEqual([row["requested"] for row in starts], ["", "41234"])

    def test_split_readiness_keeps_complete_port(self):
        with contract.forward(self.base("split"), "owned-test", scheme="http") as endpoint:
            self.assertEqual(endpoint, "http://127.0.0.1:41234")
            self.work_completed(self.processes[-1])

    def test_startup_exit_keeps_diagnostics(self):
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors), self.assertRaises(RuntimeError):
            with contract.forward(self.base("fail"), "owned-test"):
                self.fail("child that exited before readiness was accepted")
        self.assertIn("owned fixture stderr startup diagnostic", errors.getvalue())
        self.assertIn("owned fixture stdout startup diagnostic", errors.getvalue())

    def test_body_error_keeps_late_diagnostics(self):
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors), self.assertRaisesRegex(ValueError, "owned caller failure"):
            with contract.forward(self.base("quiet"), "owned-test"):
                self.work_completed(self.processes[-1])
                raise ValueError("owned caller failure")
        self.assertIn("owned fixture stderr after readiness", errors.getvalue())

    def test_reconnect_failure_stops_both_children(self):
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors), self.assertRaises(RuntimeError):
            with contract.forward(self.base("fail-second"), "owned-test") as endpoint:
                self.work_completed(self.processes[-1])
                endpoint.reconnect()
        self.assertEqual(len(self.processes), 2)
        self.assertIn("owned fixture stderr startup diagnostic", errors.getvalue())


if __name__ == "__main__":
    unittest.main()

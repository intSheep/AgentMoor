#!/usr/bin/env python3
"""Count only actual Codex exec calls inside this disposable guest workspace."""
import json
import os
from pathlib import Path
import socket
import sys
import time
import uuid

if sys.argv[1:2] == ["exec"]:
    path = Path(".agentmoor-native-count.json")
    pod = socket.gethostname()
    previous = json.loads(path.read_text()) if path.is_file() else {}
    record = {"pod": pod, "nonce": str(uuid.uuid4()), "count": 1}
    if previous.get("pod") == pod:
        record["count"] += previous["count"]
    path.write_text(json.dumps(record, sort_keys=True) + "\n")
    # Give the trusted observer time to record the real running CRI/Sentry.
    # The native command, its arguments and task deadline are unchanged.
    time.sleep(3)
os.execv("/usr/local/bin/codex", ["codex", *sys.argv[1:]])

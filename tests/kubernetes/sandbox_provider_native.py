#!/usr/bin/env python3
"""Count only actual Codex exec calls inside this disposable guest workspace."""
import json
import os
from pathlib import Path
import socket
import resource
import sys
import time
import uuid

if sys.argv[1:2] == ["exec"]:
    path = Path(".agentmoor-native-count.json")
    pod = socket.gethostname()
    previous = json.loads(path.read_text()) if path.is_file() else {}
    record = {"pod": pod, "nonce": str(uuid.uuid4()), "count": 1,
              "rlimit_nproc": list(resource.getrlimit(resource.RLIMIT_NPROC)),
              "forbidden_environment_present": [name for name in (
                  "AGENTMOOR_POSTGRES_DSN", "AGENTMOOR_S3_ENDPOINT", "AGENTMOOR_S3_BUCKET",
                  "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN") if name in os.environ]}
    if previous.get("pod") == pod:
        record["count"] += previous["count"]
    path.write_text(json.dumps(record, sort_keys=True) + "\n")
    # Give the trusted observer time to record the real running CRI/Sentry.
    # The native command, its arguments and task deadline are unchanged.
    time.sleep(3)
os.execv("/usr/local/bin/codex", ["codex", *sys.argv[1:]])

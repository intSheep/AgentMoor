#!/usr/bin/env python3
"""Measure real workspace uploads against an owned, loopback AgentMoor platform."""

import argparse
import base64
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import platform
import random
import re
import select
import shutil
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
               "te", "trailer", "transfer-encoding", "upgrade"}
UPLOAD = re.compile(r"/v1/changes/[a-f0-9]+/content")


def call(*args, cwd, env=None, timeout=180):
    result = subprocess.run(list(map(str, args)), cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Credentials are supplied through an external file, never argv/stdout.
        raise RuntimeError(f"{Path(str(args[0])).name} exited {result.returncode}: {result.stderr[-1000:]}")
    return result.stdout.strip()


def source_identity(binary):
    commit = call("git", "rev-parse", "HEAD", cwd=ROOT)
    if call("git", "status", "--porcelain", cwd=ROOT):
        raise ValueError("commit the source tree before measuring; write results outside the repository")
    lines = call("go", "version", "-m", binary, cwd=ROOT).splitlines()
    settings = {}
    for line in lines:
        if line.startswith("\tbuild\t") and "=" in line:
            key, value = line.removeprefix("\tbuild\t").split("=", 1)
            settings[key] = value
    if settings.get("vcs.revision") != commit or settings.get("vcs.modified") != "false":
        raise ValueError("binary does not match the clean source commit; run make build after committing")
    with binary.open("rb") as stream:
        binary_digest = hashlib.file_digest(stream, "sha256").hexdigest()
    return {"source_commit": commit, "platform_revision": settings["vcs.revision"],
            "binary_sha256": binary_digest,
            "go_toolchain": lines[0].rsplit(": ", 1)[-1]}


class UploadProxy(BaseHTTPRequestHandler):
    """Forward streaming bodies without interpreting private authorization headers."""

    protocol_version = "HTTP/1.1"

    def forward(self):
        if self.headers.get("Transfer-Encoding"):
            self.send_error(400, "the measurement requires fixed-length CLI requests")
            return
        origin = self.server.origin
        connection = http.client.HTTPConnection(origin.hostname, origin.port, timeout=180)
        uploaded = 0
        try:
            connection.putrequest(self.command, self.path, skip_host=True, skip_accept_encoding=True)
            connection.putheader("Host", origin.netloc)
            for key, value in self.headers.items():
                if key.lower() not in HOP_HEADERS | {"host"}:
                    connection.putheader(key, value)
            connection.putheader("Connection", "close")
            connection.endheaders()
            remaining = int(self.headers.get("Content-Length", "0"))
            while remaining:
                chunk = self.rfile.read(min(remaining, 1 << 20))
                if not chunk:
                    raise EOFError("incomplete CLI request body")
                connection.send(chunk)
                uploaded += len(chunk)
                remaining -= len(chunk)
            response = connection.getresponse()
            if self.command == "PUT" and UPLOAD.fullmatch(urllib.parse.urlsplit(self.path).path):
                with self.server.lock:
                    self.server.uploads.append({"payload_bytes": uploaded, "status": response.status})
            self.send_response(response.status)
            for key, value in response.getheaders():
                if key.lower() not in HOP_HEADERS:
                    self.send_header(key, value)
            self.send_header("Connection", "close")
            self.end_headers()
            while chunk := response.read(1 << 20):
                self.wfile.write(chunk)
        finally:
            self.close_connection = True
            connection.close()

    do_GET = do_POST = do_PUT = forward

    def log_message(self, *args):
        pass


@contextmanager
def local_platform(binary, directory, env):
    data = directory / "platform"
    call(binary, "init", "--data", data, cwd=directory, env=env)
    with (directory / "platform.log").open("w+") as log:
        process = subprocess.Popen([str(binary), "serve", "--data", str(data), "--listen", "127.0.0.1:0"],
                                   cwd=directory, env=env, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=log, text=True)
        try:
            if not select.select([process.stdout], [], [], 15)[0]:
                raise TimeoutError("temporary platform did not start")
            line = process.stdout.readline().strip()
            if not re.fullmatch(r"listening 127\.0\.0\.1:[0-9]+", line):
                raise RuntimeError("temporary platform did not report a loopback listener")
            origin = urllib.parse.urlsplit("http://" + line.split()[1])
            with ThreadingHTTPServer(("127.0.0.1", 0), UploadProxy) as proxy:
                proxy.origin, proxy.uploads, proxy.lock = origin, [], threading.Lock()
                thread = threading.Thread(target=proxy.serve_forever)
                thread.start()
                try:
                    yield data, "http://127.0.0.1:" + str(proxy.server_port), proxy
                finally:
                    proxy.shutdown()
                    thread.join()
        finally:
            if process.poll() is None:
                process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            process.stdout.close()


def fetch(endpoint, path, token):
    request = urllib.request.Request(endpoint + path, headers={"Authorization": "Bearer " + token})
    return urllib.request.urlopen(request, timeout=180)


def inventory(directory):
    return {p.relative_to(directory).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in directory.rglob("*") if p.is_file()}


def verify_archive(endpoint, revision, token, directory, expected):
    query = urllib.parse.urlencode({"kind": "workspace", "id": "root", "version": revision["version"]})
    archive_path = directory / f"workspace-v{revision['version']}.tar"
    with fetch(endpoint, "/v1/content?" + query, token) as response, archive_path.open("wb") as output:
        encoded = response.headers["X-Agentmoor-Revision"]
        metadata = json.loads(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)))
        assert metadata["format"] == "workspace-tar.v1" and metadata["version"] == revision["version"]
        shutil.copyfileobj(response, output, 1 << 20)
    with archive_path.open("rb") as stream:
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
    assert digest == revision["sha256"] and archive_path.stat().st_size == revision["size"]
    with tarfile.open(archive_path) as archive:
        actual = {}
        for member in archive:
            if member.isdir():
                continue
            assert member.isfile(), "synthetic workspace must contain regular files only"
            with archive.extractfile(member) as stream:
                actual[member.name.removeprefix("./")] = hashlib.file_digest(stream, "sha256").hexdigest()
    assert actual == expected, "fixed-version workspace content differs from the synthetic input"
    return digest


def measure(args):
    binary = args.binary.resolve()
    identity = source_identity(binary)
    began = time.perf_counter()
    with tempfile.TemporaryDirectory(prefix="agentmoor-workspace-baseline-") as temporary:
        root = Path(temporary).resolve()
        workspace, home, tmp = root / "workspace", root / "home", root / "tmp"
        for directory in (workspace, home, tmp):
            directory.mkdir()
        # No inherited database, object-storage, model or user credentials.
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home),
               "TMPDIR": str(tmp), "LANG": "C", "NO_PROXY": "127.0.0.1,localhost"}
        randomizer = random.Random(args.seed)
        expected = {}
        for index in range(args.files):
            name = f"data/{index // 100:05d}/file-{index:06d}.bin"
            path = workspace / name
            path.parent.mkdir(exist_ok=True, parents=True)
            raw = randomizer.randbytes(args.file_bytes)
            path.write_bytes(raw)
            expected[name] = hashlib.sha256(raw).hexdigest()
        original = dict(expected)
        first, last = next(iter(expected)), next(reversed(expected))
        stages, revisions, snapshots = [], {}, {}
        with local_platform(binary, root, env) as (data, endpoint, proxy):
            token = call(binary, "issue", "--data", data, "--tenant", "synthetic", "--project", "workspace-baseline",
                         "--runtime", "owned-input", "--adapter", "codex", "--write", cwd=root, env=env)
            credential = root / "credential"
            credential.write_text(token)
            credential.chmod(0o600)
            env["AGENTMOOR_TOKEN_FILE"] = str(credential)
            config = root / "worker.json"
            config.write_text(json.dumps({"scope": {"tenant": "synthetic", "project": "workspace-baseline",
                                                   "runtime": "owned-input", "adapter": "codex"},
                                          "endpoint": endpoint, "workspace": str(workspace),
                                          "state_dir": str(root / "journal")}))
            config.chmod(0o600)
            descriptors = json.loads(call(binary, "adapters", cwd=root, env=env))
            adapter_version = next(item["version"] for item in descriptors if item["id"] == "codex")
            for phase in ("initial", "unchanged", "single_file_update", "single_file_delete"):
                if phase == "single_file_update":
                    changed = bytes(value ^ 0xff for value in (workspace / first).read_bytes())
                    (workspace / first).write_bytes(changed)
                    expected[first] = hashlib.sha256(changed).hexdigest()
                elif phase == "single_file_delete":
                    (workspace / last).unlink()
                    del expected[last]
                with proxy.lock:
                    before = len(proxy.uploads)
                started = time.perf_counter()
                call(binary, "sync", "--config", config, cwd=root, env=env)
                elapsed = time.perf_counter() - started
                with fetch(endpoint, "/v1/heads", token) as response:
                    heads = json.load(response)
                revision, = heads
                assert revision["kind"] == "workspace" and revision["id"] == "root"
                assert revision["format"] == "workspace-tar.v1"
                with proxy.lock:
                    uploads = list(proxy.uploads[before:])
                assert all(item["status"] // 100 == 2 for item in uploads)
                wanted_version = {"initial": 1, "unchanged": 1, "single_file_update": 2, "single_file_delete": 3}[phase]
                assert revision["version"] == wanted_version
                payload_bytes = sum(item["payload_bytes"] for item in uploads)
                if phase == "unchanged":
                    assert uploads == [], "an unchanged workspace uploaded content"
                else:
                    assert len(uploads) == 1 and payload_bytes == revision["size"], "upload is not the complete tar payload"
                    assert payload_bytes > len(expected) * args.file_bytes
                revisions[wanted_version], snapshots[wanted_version] = revision, dict(expected)
                stages.append({"phase": phase, "source_file_count": len(expected),
                               "source_payload_bytes": len(expected) * args.file_bytes,
                               "changed_payload_bytes": args.file_bytes if phase != "unchanged" and phase != "initial" else
                                                        len(expected) * args.file_bytes if phase == "initial" else 0,
                               "upload_requests": len(uploads), "upload_payload_bytes": payload_bytes,
                               "sync_elapsed_seconds": round(elapsed, 6), "workspace_version": wanted_version,
                               "workspace_format": revision["format"], "workspace_sha256": revision["sha256"],
                               "workspace_archive_bytes": revision["size"]})
            verified = {str(version): verify_archive(endpoint, revision, token, root, snapshots[version])
                        for version, revision in revisions.items()}
            restored = root / "restored-original"
            call(binary, "restore", "--config", config, "--kind", "workspace", "--id", "root", "--version", "1",
                 "--to", restored, cwd=root, env=env)
            assert inventory(restored) == original, "CLI restore changed the original snapshot"
    return {"recorded_at_utc": datetime.now(timezone.utc).isoformat(), **identity,
            "host": {"platform": platform.platform(), "python": platform.python_version()},
            "storage": {"metadata": "sqlite", "objects": "local-disk"}, "adapter_version": adapter_version,
            "input": {"files": args.files, "file_bytes": args.file_bytes, "seed": args.seed,
                      "source_payload_bytes": args.files * args.file_bytes},
            "timing_scope": "CLI sync wall time through the loopback proxy; excludes input generation and download/restore checks",
            "run_elapsed_seconds": round(time.perf_counter() - began, 6),
            "sync_total_elapsed_seconds": round(sum(item["sync_elapsed_seconds"] for item in stages), 6),
            "stages": stages, "verification": {"fixed_version_tar_sha256": verified,
                                               "old_version_cli_restore": "all original file hashes matched",
                                               "unchanged_workspace": "zero upload requests",
                                               "single_file_mutations": "complete workspace tar uploaded"},
            "limitations": ["This measures workspace-tar.v1; it does not implement incremental workspace transfers.",
                            "Upload counts measure HTTP request-body bytes, excluding HTTP headers and transport overhead.",
                            "Synthetic local files, SQLite/local objects and a loopback proxy; not a production performance SLO.",
                            "No runtime CLI or model is executed. Only the real AgentMoor sync/restore CLI runs."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--files", type=int, default=5000)
    parser.add_argument("--bytes", "--file-bytes", dest="file_bytes", type=int, default=4096)
    parser.add_argument("--seed", type=int, default=0)
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/agentmoor")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.files < 2 or args.file_bytes < 1 or args.files > 100000 or args.files * args.file_bytes > 512 * 1024 * 1024:
        parser.error("use 2..100000 files, positive file bytes, and at most 512 MiB of synthetic source data")
    result = measure(args)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print("WORKSPACE_BASELINE_RESULT " + json.dumps({"source_commit": result["source_commit"],
                                                    "files": args.files, "output": str(args.output.resolve()),
                                                    "stages": result["stages"]}, sort_keys=True))


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Measure real workspace uploads against an owned, loopback AgentMoor platform."""

import argparse
import base64
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import hmac
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
import signal
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import urllib.error
import uuid
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
               "te", "trailer", "transfer-encoding", "upgrade"}
UPLOAD = re.compile(r"/v1/changes/[a-f0-9]+/content")
PART_UPLOAD = re.compile(r"/v1/changes/[a-f0-9]+/parts/[a-f0-9]{64}")
PART_MANIFEST = re.compile(r"/v1/changes/[a-f0-9]+/parts")
COMMIT = re.compile(r"/v1/changes/[a-f0-9]+/commit")
ACCESS, SECRET, BUCKET = "disposable-test", "disposable-test-password", "agentmoor-baseline"


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
        began = time.perf_counter()
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
            request_path = urllib.parse.urlsplit(self.path).path
            kind = None
            if self.command == "POST" and request_path == "/v1/changes":
                kind = "begin"
            elif self.command == "POST" and PART_MANIFEST.fullmatch(request_path):
                kind = "parts_manifest"
            elif self.command == "POST" and COMMIT.fullmatch(request_path):
                kind = "commit"
            elif self.command == "PUT" and PART_UPLOAD.fullmatch(request_path):
                kind = "part_body"
            elif self.command == "PUT" and UPLOAD.fullmatch(request_path):
                kind = "full_content"
            elif self.command in {"POST", "PUT", "PATCH", "DELETE"} and request_path.startswith("/v1/changes/"):
                kind = "other_change_mutation"
            if kind:
                event = {"kind": kind, "method": self.command, "payload_bytes": uploaded,
                         "status": response.status, "content_encoding": self.headers.get("Content-Encoding", ""),
                         "response_headers_elapsed_seconds": round(time.perf_counter() - began, 6)}
                with self.server.lock:
                    self.server.mutations.append(event)
                    if kind in {"full_content", "part_body"}:
                        self.server.uploads.append(event)
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

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = forward

    def log_message(self, *args):
        pass


@contextmanager
def local_platform(binary, directory, env, server_env=None):
    data = directory / "platform"
    call(binary, "init", "--data", data, cwd=directory, env=env)
    with (directory / "platform.log").open("w+") as log:
        process = subprocess.Popen([str(binary), "serve", "--data", str(data), "--listen", "127.0.0.1:0"],
                                   cwd=directory, env={**env, **(server_env or {})}, stdin=subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=log, text=True)
        try:
            if not select.select([process.stdout], [], [], 15)[0]:
                raise TimeoutError("temporary platform did not start")
            line = process.stdout.readline().strip()
            if not re.fullmatch(r"listening 127\.0\.0\.1:[0-9]+", line):
                raise RuntimeError("temporary platform did not report a loopback listener")
            origin = urllib.parse.urlsplit("http://" + line.split()[1])
            with ThreadingHTTPServer(("127.0.0.1", 0), UploadProxy) as proxy:
                proxy.origin, proxy.uploads, proxy.mutations, proxy.lock = origin, [], [], threading.Lock()
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


def signed_s3(endpoint, method, path, query=""):
    """Sign only empty-body operations against the owned disposable bucket."""
    now = datetime.now(timezone.utc)
    date, stamp = now.strftime("%Y%m%d"), now.strftime("%Y%m%dT%H%M%SZ")
    host = urllib.parse.urlsplit(endpoint).netloc
    digest = hashlib.sha256(b"").hexdigest()
    headers = {"host": host, "x-amz-content-sha256": digest, "x-amz-date": stamp}
    names = ";".join(sorted(headers))
    canonical_headers = "".join(key + ":" + headers[key] + "\n" for key in sorted(headers))
    canonical = "\n".join((method, path, query, canonical_headers, names, digest))
    scope = date + "/us-east-1/s3/aws4_request"
    message = "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashlib.sha256(canonical.encode()).hexdigest()
    key = ("AWS4" + SECRET).encode()
    for part in (date, "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, message.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = ("AWS4-HMAC-SHA256 Credential=" + ACCESS + "/" + scope
                                + ", SignedHeaders=" + names + ", Signature=" + signature)
    request = urllib.request.Request(endpoint + path + ("?" + query if query else ""), method=method, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=10) as response:
        return response.read()


@contextmanager
def storage_backend(name):
    if name == "local":
        yield {}, {"metadata": "sqlite", "objects": "local-disk"}, None
        return
    # Docker may need its normal client context. These variables are never
    # passed to AgentMoor; the server gets only the synthetic settings below.
    docker_env = dict(os.environ)
    try:
        call("docker", "compose", "version", cwd=ROOT, env=docker_env)
        command = ["docker", "compose"]
    except (OSError, RuntimeError):
        command = ["docker-compose"]
    project = "agentmoor-baseline-" + uuid.uuid4().hex[:12]
    command += ["-p", project, "-f", str(ROOT / "tests/services.compose.yml")]

    def compose(*args):
        return call(*command, *args, cwd=ROOT, env=docker_env, timeout=300)

    def sql(query):
        return compose("exec", "-T", "postgres", "psql", "-U", "postgres", "-d", "agentmoor", "-Atc", query)

    info = {"metadata": "postgresql", "objects": "s3-compatible-minio",
            "owned_compose_project": project, "cleanup_completed": False}
    try:
        compose("up", "-d", "--wait")
        pg = compose("port", "postgres", "5432")
        s3 = compose("port", "s3", "9000")
        assert all(re.fullmatch(r"127\.0\.0\.1:[0-9]+", item) for item in (pg, s3)), "services must use owned loopback ports"
        endpoint = "http://" + s3
        until = time.monotonic() + 60
        while True:
            try:
                signed_s3(endpoint, "PUT", "/" + BUCKET)
                break
            except urllib.error.HTTPError as error:
                if error.code == 409:
                    break
                if error.code < 500 or time.monotonic() >= until:
                    raise
            except (OSError, urllib.error.URLError):
                if time.monotonic() >= until:
                    raise
            time.sleep(.2)
        images = {}
        for service in ("postgres", "s3"):
            container = compose("ps", "-q", service)
            row, = json.loads(call("docker", "inspect", container, cwd=ROOT, env=docker_env))
            image, = json.loads(call("docker", "image", "inspect", row["Image"], cwd=ROOT, env=docker_env))
            images[service] = {"image": row["Config"]["Image"], "image_id": row["Image"],
                               "repo_digests": image.get("RepoDigests", [])}
        info.update({"postgres_version": sql("SHOW server_version"),
                     "minio_version": compose("exec", "-T", "s3", "minio", "--version").splitlines()[0],
                     "images": images})
        server_env = {"AGENTMOOR_POSTGRES_DSN": "postgres://postgres:" + SECRET + "@" + pg + "/agentmoor?sslmode=disable",
                      "AGENTMOOR_S3_ENDPOINT": s3, "AGENTMOOR_S3_BUCKET": BUCKET, "AGENTMOOR_S3_HTTP": "1",
                      "AWS_ACCESS_KEY_ID": ACCESS, "AWS_SECRET_ACCESS_KEY": SECRET}

        def attest(data, transfer):
            # Storage inspection proves selection only. Versions, tar bytes and
            # restores are still checked through the public HTTP/CLI interface.
            assert not (data / "catalog.sqlite").exists() and not (data / "objects").exists(), "local storage fallback detected"
            assert int(sql("SELECT count(*) FROM revisions")) == 3
            representations = int(sql("SELECT count(*) FROM revision_parts"))
            assert representations == (3 if transfer == "tar-parts.v1" else 0), "unexpected stored workspace representation"
            listing = ET.fromstring(signed_s3(endpoint, "GET", "/" + BUCKET, "list-type=2&max-keys=1"))
            assert listing.findall("{*}Contents/{*}Key"), "owned S3 bucket has no stored objects"
            info.update({"postgres_revision_rows": 3, "workspace_part_revisions": representations, "s3_objects_present": True,
                         "sqlite_or_local_objects_present": False})

        yield server_env, info, attest
    finally:
        compose("down", "--volumes", "--remove-orphans")
        info["cleanup_completed"] = True


def fetch(endpoint, path, token):
    request = urllib.request.Request(endpoint + path, headers={"Authorization": "Bearer " + token})
    return urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=180)


def inventory(directory):
    return {p.relative_to(directory).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in directory.rglob("*") if p.is_file()}


def verify_archive(endpoint, revision, token, directory, expected):
    query = urllib.parse.urlencode({"kind": "workspace", "id": "root", "version": revision["version"]})
    archive_path = directory / f"workspace-v{revision['version']}.tar"
    began = time.perf_counter()
    with fetch(endpoint, "/v1/content?" + query, token) as response, archive_path.open("wb") as output:
        encoded = response.headers["X-Agentmoor-Revision"]
        metadata = json.loads(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)))
        assert metadata["format"] == "workspace-tar.v1" and metadata["version"] == revision["version"]
        shutil.copyfileobj(response, output, 1 << 20)
    downloaded = time.perf_counter() - began
    began = time.perf_counter()
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
    return digest, {"download_elapsed_seconds": round(downloaded, 6),
                    "validation_elapsed_seconds": round(time.perf_counter() - began, 6)}


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
        if args.transfer == "tar-parts.v1" and len(set(expected.values())) != len(expected):
            raise ValueError("tar-parts measurement needs distinct file bodies; increase --bytes or choose another --seed")
        first, last = next(iter(expected)), next(reversed(expected))
        stages, revisions, snapshots = [], {}, {}
        with storage_backend(args.backend) as (server_env, storage, attest), \
                local_platform(binary, root, env, server_env) as (data, endpoint, proxy):
            token = call(binary, "issue", "--data", data, "--tenant", "synthetic", "--project", "workspace-baseline",
                         "--runtime", "owned-input", "--adapter", "codex", "--write", cwd=root, env=env)
            credential = root / "credential"
            credential.write_text(token)
            credential.chmod(0o600)
            env["AGENTMOOR_TOKEN_FILE"] = str(credential)
            config = root / "worker.json"
            settings = {"scope": {"tenant": "synthetic", "project": "workspace-baseline",
                                  "runtime": "owned-input", "adapter": "codex"},
                        "endpoint": endpoint, "workspace": str(workspace), "state_dir": str(root / "journal")}
            if args.transfer == "tar-parts.v1":
                settings["workspace_transfer"] = args.transfer
            config.write_text(json.dumps(settings))
            config.chmod(0o600)
            descriptors = json.loads(call(binary, "adapters", cwd=root, env=env))
            adapter_version = next(item["version"] for item in descriptors if item["id"] == "codex")
            for phase in ("initial", "unchanged", "single_file_update", "single_file_delete"):
                if phase == "single_file_update":
                    changed = bytes(value ^ 0xff for value in (workspace / first).read_bytes())
                    if args.transfer == "tar-parts.v1":
                        assert hashlib.sha256(changed).hexdigest() not in expected.values(), "changed measurement file must have a new body digest"
                    (workspace / first).write_bytes(changed)
                    expected[first] = hashlib.sha256(changed).hexdigest()
                elif phase == "single_file_delete":
                    (workspace / last).unlink()
                    del expected[last]
                with proxy.lock:
                    before = len(proxy.mutations)
                started = time.perf_counter()
                call(binary, "sync", "--config", config, cwd=root, env=env)
                elapsed = time.perf_counter() - started
                with fetch(endpoint, "/v1/heads", token) as response:
                    heads = json.load(response)
                revision, = heads
                assert revision["kind"] == "workspace" and revision["id"] == "root"
                assert revision["format"] == "workspace-tar.v1"
                with proxy.lock:
                    mutations = list(proxy.mutations[before:])
                assert all(item["status"] // 100 == 2 for item in mutations)
                uploads = [item for item in mutations if item["kind"] in {"full_content", "part_body"}]
                wanted_version = {"initial": 1, "unchanged": 1, "single_file_update": 2, "single_file_delete": 3}[phase]
                assert revision["version"] == wanted_version
                payload_bytes = sum(item["payload_bytes"] for item in uploads)
                if phase == "unchanged":
                    assert mutations == [], "an unchanged workspace sent a change mutation request"
                elif args.transfer == "full":
                    assert len(uploads) == 1 and payload_bytes == revision["size"], "upload is not the complete tar payload"
                    assert payload_bytes > len(expected) * args.file_bytes
                else:
                    count = {"initial": args.files, "single_file_update": 1, "single_file_delete": 0}[phase]
                    assert len(uploads) == count and payload_bytes == count * args.file_bytes, "unexpected missing file body transfers"
                    manifests = [item for item in mutations if item["kind"] == "parts_manifest"]
                    assert len(manifests) == 1 and manifests[0]["content_encoding"] == "gzip" and manifests[0]["payload_bytes"] > 0
                    assert all(item["kind"] != "full_content" for item in mutations)
                if phase != "unchanged":
                    assert sum(item["kind"] == "begin" for item in mutations) == 1
                    assert sum(item["kind"] == "commit" for item in mutations) == 1
                    assert not any(item["kind"] == "other_change_mutation" for item in mutations)
                revisions[wanted_version], snapshots[wanted_version] = revision, dict(expected)
                stage = {"phase": phase, "source_file_count": len(expected),
                         "source_payload_bytes": len(expected) * args.file_bytes,
                         "changed_payload_bytes": args.file_bytes if phase not in {"unchanged", "initial"} else
                                                  len(expected) * args.file_bytes if phase == "initial" else 0,
                         "upload_requests": len(uploads), "upload_payload_bytes": payload_bytes,
                         "change_mutation_requests": len(mutations),
                         "change_mutation_payload_bytes": sum(item["payload_bytes"] for item in mutations),
                         "sync_elapsed_seconds": round(elapsed, 6), "workspace_version": wanted_version,
                         "workspace_format": revision["format"], "workspace_sha256": revision["sha256"],
                         "workspace_archive_bytes": revision["size"]}
                for kind in ("begin", "full_content", "parts_manifest", "part_body", "commit"):
                    selected = [item for item in mutations if item["kind"] == kind]
                    stage[kind + "_requests"] = len(selected)
                    stage[kind + "_payload_bytes"] = sum(item["payload_bytes"] for item in selected)
                stage["parts_manifest_gzip_payload_bytes"] = sum(item["payload_bytes"] for item in mutations
                                                                 if item["kind"] == "parts_manifest" and item["content_encoding"] == "gzip")
                stage["commit_response_headers_elapsed_seconds"] = [item["response_headers_elapsed_seconds"]
                                                                    for item in mutations if item["kind"] == "commit"]
                stages.append(stage)
            verified, downloads = {}, {}
            for version, revision in revisions.items():
                verified[str(version)], downloads[str(version)] = verify_archive(endpoint, revision, token, root, snapshots[version])
            restored = root / "restored-original"
            began_restore = time.perf_counter()
            call(binary, "restore", "--config", config, "--kind", "workspace", "--id", "root", "--version", "1",
                 "--to", restored, cwd=root, env=env)
            restore_elapsed = time.perf_counter() - began_restore
            assert inventory(restored) == original, "CLI restore changed the original snapshot"
            if attest is not None:
                attest(data, args.transfer)
    return {"recorded_at_utc": datetime.now(timezone.utc).isoformat(), **identity,
            "host": {"platform": platform.platform(), "python": platform.python_version()},
            "storage": storage, "adapter_version": adapter_version, "workspace_transfer": args.transfer,
            "input": {"files": args.files, "file_bytes": args.file_bytes, "seed": args.seed,
                      "source_payload_bytes": args.files * args.file_bytes},
            "timing_scope": "CLI sync wall time through the loopback proxy; excludes input generation and download/restore checks",
            "request_byte_scope": "Actual change mutation request bodies: begin JSON + full tar or gzip manifest + missing part PUT bodies + commit body. Excludes headers, HTTP responses and transport overhead. upload_payload_bytes retains only PUT content/part bodies.",
            "commit_timing_scope": "Proxy request entry to upstream commit response headers, including forwarding its request body; excludes downstream response consumption.",
            "download_timing_scope": "Fixed-version public GET through the proxy to a local tar file; validation hashing and tar inventory are timed separately.",
            "restore_timing_scope": "Real AgentMoor CLI restore version 1 wall time, including download, validation and atomic publish; excludes the subsequent inventory comparison.",
            "run_elapsed_seconds": round(time.perf_counter() - began, 6),
            "sync_total_elapsed_seconds": round(sum(item["sync_elapsed_seconds"] for item in stages), 6),
            "fixed_version_downloads": downloads, "old_version_cli_restore_elapsed_seconds": round(restore_elapsed, 6),
            "stages": stages, "verification": {"fixed_version_tar_sha256": verified,
                                               "old_version_cli_restore": "all original file hashes matched",
                                               "unchanged_workspace": "zero change mutation requests",
                                               "single_file_mutations": "complete workspace tar uploaded" if args.transfer == "full" else
                                                                        "initial all file bodies; update one file body; delete zero file bodies; all manifests and metadata counted"},
            "limitations": ["Logical content remains workspace-tar.v1; transfer mode is recorded separately.",
                            "Upload counts measure HTTP request-body bytes, excluding HTTP headers and transport overhead.",
                            "Synthetic files and an owned loopback platform/storage fixture; not a production performance SLO.",
                            "tar-parts measurement requires distinct file bodies of at most 4 MiB to assert one body for a single-file update; larger product files may span multiple bodies.",
                            "changed_payload_bytes is source-file delta size, including removed bytes; it is not network transfer size.",
                            "No runtime CLI or model is executed. Only the real AgentMoor sync/restore CLI runs."]}


def terminate(*_):
    raise SystemExit("terminated; cleaning owned fixture")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--files", type=int, default=5000)
    parser.add_argument("--bytes", "--file-bytes", dest="file_bytes", type=int, default=4096)
    parser.add_argument("--seed", type=int, default=0)
    parser.add_argument("--transfer", choices=("full", "tar-parts.v1"), default="full")
    parser.add_argument("--backend", choices=("local", "postgres-s3"), default="local")
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/agentmoor")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.files < 2 or args.file_bytes < 1 or args.files > 100000 or args.files * args.file_bytes > 512 * 1024 * 1024:
        parser.error("use 2..100000 files, positive file bytes, and at most 512 MiB of synthetic source data")
    if args.transfer == "tar-parts.v1" and args.file_bytes > 4 * 1024 * 1024:
        parser.error("tar-parts single-body measurement requires --bytes <= 4 MiB")
    if args.output.resolve().is_relative_to(ROOT):
        parser.error("write the measurement output outside the source repository")
    signal.signal(signal.SIGTERM, terminate)
    result = measure(args)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + "\n")
    print("WORKSPACE_BASELINE_RESULT " + json.dumps({"source_commit": result["source_commit"],
                                                    "files": args.files, "output": str(args.output.resolve()),
                                                    "transfer": args.transfer, "backend": args.backend,
                                                    "stages": result["stages"]}, sort_keys=True))


if __name__ == "__main__":
    main()

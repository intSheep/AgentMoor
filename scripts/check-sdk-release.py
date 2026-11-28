#!/usr/bin/env python3
"""Verify an exact published 0.x SDK from an unrelated, cold-cache module."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time


MODULE = "github.com/intSheep/AgentMoor"
CONSUMER = "example.invalid/agentmoor-sdk-consumer"
TESTS = (
    "TestExternalRegistryAndInspect",
    "TestExternalNativeConformance",
    "TestExternalNativeConformance/external-native-custom-commands",
)
MAX_OUTPUT = 2 << 20


def exact_preview(value):
    number = r"(?:0|[1-9][0-9]*)"
    if not re.fullmatch(rf"v0\.{number}\.{number}(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", value):
        raise argparse.ArgumentTypeError("VERSION must be an exact v0.x.y version, optionally with a prerelease suffix")
    for part in value.partition("-")[2].split("."):
        if part.isdigit() and len(part) > 1 and part.startswith("0"):
            raise argparse.ArgumentTypeError("numeric prerelease identifiers cannot have leading zeroes")
    return value


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            result.update(chunk)
    return result.hexdigest()


def within(parent, child):
    return child.resolve().is_relative_to(parent.resolve())


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Check:
    def __init__(self, version, output, checkout):
        self.output = output
        self.root = Path(tempfile.mkdtemp(prefix="agentmoor-sdk-release-", dir="/tmp")).resolve()
        require(not within(checkout, self.root), "temporary consumer must be physically outside the checkout")
        self.cache = self.root / "module-cache"
        self.consumer = self.root / "consumer"
        for path in (self.cache, self.consumer, self.root / "home", self.root / "tmp"):
            path.mkdir()
        self.go = shutil.which("go")
        self.deadline = time.monotonic() + 1200
        self.env = {name: os.environ[name] for name in (
            "PATH", "LANG", "LC_ALL", "CC", "CXX", "SDKROOT", "DEVELOPER_DIR", "MACOSX_DEPLOYMENT_TARGET"
        ) if name in os.environ}
        self.env.update({
            "HOME": str(self.root / "home"), "TMPDIR": str(self.root / "tmp"),
            "TMP": str(self.root / "tmp"), "TEMP": str(self.root / "tmp"),
            "GOPATH": str(self.root / "gopath"), "GOMODCACHE": str(self.cache),
            "GOCACHE": str(self.root / "build-cache"), "GOENV": "off", "GOWORK": "off",
            "GOTOOLCHAIN": "local", "GO111MODULE": "on", "GOFLAGS": "-mod=mod -buildvcs=false",
            "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
            "GOPRIVATE": "", "GONOPROXY": "none", "GONOSUMDB": "none", "GOVCS": "*:off",
            "CGO_ENABLED": "1",
        })
        self.report = {
            "schema": "agentmoor.sdk-release-check.v1", "version": version, "status": "failed",
            "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "scope": "Maintainer-written synthetic consumer of an actual published module; not independent third-party, vendor CLI, model, or sandbox certification.",
            "checkout": str(checkout.resolve()), "temporary_root": str(self.root),
            "consumer_module": CONSUMER, "module_cache": str(self.cache),
            "module_cache_initially_empty": not any(self.cache.iterdir()),
            "environment": {name: self.env[name] for name in (
                "GOENV", "GOWORK", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GONOPROXY", "GONOSUMDB", "GOVCS", "GOMODCACHE"
            )},
            "source_hashes": {}, "steps": [],
        }

    def run(self, name, args, *, directory=None, cgo="1", timeout=300, allowed_failure=False, cleanup=False):
        require(self.go is not None, "go executable was not found on PATH")
        budget = timeout if cleanup else min(timeout, self.deadline - time.monotonic())
        require(budget > 0, "overall verification timeout exceeded")
        command = [self.go, *args]
        filename = f"{len(self.report['steps']) + 1:02d}-{name}.log"
        stderr_filename = filename.removesuffix(".log") + ".stderr.log"
        step = {"name": name, "command": command, "directory": str(directory or self.consumer),
                "cgo_enabled": cgo, "log": filename, "stderr_log": stderr_filename,
                "timeout_seconds": round(budget, 3)}
        self.report["steps"].append(step)
        started = time.monotonic()
        chunks, size, stopped = [], 0, None
        process = subprocess.Popen(command, cwd=directory or self.consumer,
                                   env={**self.env, "CGO_ENABLED": cgo}, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
        completed = False
        try:
            with (self.output / filename).open("xb") as log, \
                    (self.output / stderr_filename).open("xb") as stderr_log, \
                    selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ, (log, True))
                selector.register(process.stderr, selectors.EVENT_READ, (stderr_log, False))
                while selector.get_map():
                    if time.monotonic() - started >= budget:
                        stopped = "timeout"
                        break
                    for key, _ in selector.select(timeout=min(0.2, budget)):
                        chunk = os.read(key.fileobj.fileno(), 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        if size + len(chunk) > MAX_OUTPUT:
                            stopped = "output limit"
                            break
                        stream, stdout = key.data
                        if stdout:
                            chunks.append(chunk)
                        stream.write(chunk)
                        size += len(chunk)
                    if stopped:
                        break
                if not stopped:
                    try:
                        process.wait(timeout=max(0.01, budget - (time.monotonic() - started)))
                        completed = True
                    except subprocess.TimeoutExpired:
                        stopped = "timeout"
        finally:
            # Also stop descendants if Python is interrupted or reading a pipe
            # fails, before the caller removes this invocation's private cache.
            if not completed:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            process.stdout.close()
            process.stderr.close()
            returncode = process.wait(timeout=3)
        step.update({"exit_code": returncode, "duration_seconds": round(time.monotonic() - started, 3),
                     "output_bytes": size, "log_sha256": digest(self.output / filename),
                     "stderr_log_sha256": digest(self.output / stderr_filename)})
        if stopped:
            step["stopped"] = stopped
        require(stopped is None, f"{name} exceeded its {stopped}; see {filename}")
        require(returncode == 0 or allowed_failure, f"{name} exited {returncode}; see {filename}")
        return returncode, b"".join(chunks)

    def validate_module(self, module):
        require(module.get("Path") == MODULE and module.get("Version") == self.report["version"],
                "SDK did not resolve to the exact requested module version")
        require(not module.get("Replace"), "SDK resolution unexpectedly used a replacement")
        require(bool(module.get("Dir")) and within(self.cache, Path(module["Dir"])),
                "SDK source is not inside the isolated module cache")

    def verify(self, checkout):
        version = self.report["version"]
        fixture = checkout / "testdata" / "sdkconsumer"
        require(sorted(path.name for path in fixture.iterdir()) == ["adapter.go", "consumer_test.go", "go.mod"],
                "unexpected consumer fixture files")
        for path in [Path(__file__).resolve(), *(fixture / name for name in ("adapter.go", "consumer_test.go", "go.mod"))]:
            require(not path.is_symlink() and path.is_file() and path.stat().st_size <= 1 << 20,
                    "invalid bounded consumer source")
            self.report["source_hashes"][str(path.relative_to(checkout))] = digest(path)
        manifest = (fixture / "go.mod").read_text()
        require(manifest.startswith(f"module {CONSUMER}\n") and not CONSUMER.startswith(MODULE),
                "consumer must have an unrelated module path")
        require(manifest.count(f"require {MODULE} v0.0.0") == 1 and "replace " not in manifest,
                "unexpected fixture module requirements")
        manifest = manifest.replace(f"require {MODULE} v0.0.0", f"require {MODULE} {version}")
        (self.consumer / "go.mod").write_text(manifest)
        (self.output / "consumer-initial.go.mod").write_text(manifest)
        for name in ("adapter.go", "consumer_test.go"):
            shutil.copyfile(fixture / name, self.consumer / name)
        _, raw = self.run("go-environment", ["env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOMODCACHE", "GOWORK", "GOPROXY", "GOSUMDB", "GOENV", "GOTOOLCHAIN"])
        self.report["go_environment"] = json.loads(raw)
        self.run("get-published-sdk", ["get", f"{MODULE}/sdk@{version}"])
        _, raw = self.run("resolved-module", ["list", "-m", "-json", MODULE])
        module = json.loads(raw)
        self.validate_module(module)
        self.report["resolved_module"] = module
        _, raw = self.run("download-metadata", ["mod", "download", "-json", f"{MODULE}@{version}"])
        downloaded = json.loads(raw)
        self.validate_module(downloaded)
        require(not downloaded.get("Error") and all(str(downloaded.get(key, "")).startswith("h1:") for key in ("Sum", "GoModSum")),
                "download did not include verified module checksums")
        self.report["download"] = downloaded
        self.report["download_files"] = {}
        for key in ("Info", "GoMod", "Zip"):
            path = Path(downloaded.get(key, ""))
            require(path.is_file() and within(self.cache, path), f"download {key} is not in the isolated cache")
            self.report["download_files"][key] = {"sha256": digest(path), "bytes": path.stat().st_size}
        _, raw = self.run("resolved-sdk-package", ["list", "-json", f"{MODULE}/sdk"])
        package = json.loads(raw)
        self.validate_module(package.get("Module", {}))
        require(package.get("ImportPath") == MODULE + "/sdk" and
                Path(package.get("Dir", "")).resolve() == (Path(module["Dir"]) / "sdk").resolve(),
                "SDK package resolved outside the downloaded module")
        self.report["sdk_package"] = {key: package[key] for key in ("ImportPath", "Dir", "Module")}
        self.run("core-without-cgo", ["build", f"{MODULE}/sdk"], cgo="0")
        _, raw = self.run("external-race-contract", ["test", "-json", "-race", "-count=1", "-timeout=120s", "./..."])
        passed, package_passed, calls = set(), False, False
        for line in raw.splitlines():
            event = json.loads(line)
            require(event.get("Package") == CONSUMER and event.get("Action") != "fail",
                    "external suite returned a failure or a different package")
            if event.get("Action") == "pass":
                passed.add(event.get("Test", ""))
                package_passed |= not event.get("Test")
            output = event.get("Output", "")
            if "ADAPTER_CONTRACT mechanism=native-cli" in output and "List:3 Export:3 Import:1" in output:
                calls = True
        require(package_passed and all(name in passed for name in TESTS) and calls,
                "external suite did not actually pass the required tests and observed native calls")
        self.report["consumer_tests"] = {"passed": list(TESTS), "package_passed": True,
                                         "native_calls_observed": {"List": 3, "Export": 3, "Import": 1}}
        _, raw = self.run("final-resolved-module", ["list", "-m", "-json", MODULE])
        self.validate_module(json.loads(raw))
        for name in ("go.mod", "go.sum"):
            shutil.copyfile(self.consumer / name, self.output / ("consumer-final." + name))
        denied = self.root / "denied"
        denied.mkdir()
        (denied / "go.mod").write_text(manifest.replace(f"module {CONSUMER}\n", "module example.invalid/agentmoor-sdk-release-denied\n", 1))
        (denied / "denied_test.go").write_text(f'package denied\nimport _ "{MODULE}/internal/adapter"\n')
        code, raw = self.run("internal-import-denied", ["test", "-run=^$", "."], directory=denied, allowed_failure=True)
        stderr = (self.output / self.report["steps"][-1]["stderr_log"]).read_bytes()
        require(code != 0 and f"use of internal package {MODULE}/internal/adapter not allowed".encode() in raw + stderr,
                "direct internal import was not specifically rejected by Go's internal boundary")
        self.report["internal_import_denied"] = True
        self.report["status"] = "passed"

    def cleanup(self):
        self.report["cleanup"] = {"module_cache": str(self.cache), "global_cache_touched": False}
        try:
            self.run("cleanup-owned-module-cache", ["clean", "-modcache"], timeout=60, cleanup=True)
            require(not self.cache.exists() or not any(self.cache.iterdir()), "owned module cache was not emptied")
            self.report["cleanup"]["go_clean_succeeded"] = True
        except Exception as error:
            self.report["cleanup"]["error"] = str(error)
            self.report["status"] = "failed"
        # Only this invocation's newly created root is removed. The downloaded
        # module cache can contain read-only directories after a failed go clean.
        for directory, dirs, files in os.walk(self.root):
            os.chmod(directory, 0o700)
            for name in files:
                path = Path(directory) / name
                if not path.is_symlink():
                    path.chmod(0o600)
        shutil.rmtree(self.root)
        self.report["cleanup"]["temporary_root_removed"] = not self.root.exists()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version", type=exact_preview, metavar="VERSION")
    parser.add_argument("--output", required=True, type=Path, help="new directory for result.json and bounded step logs")
    args = parser.parse_args()
    output = args.output.expanduser().resolve()
    try:
        output.mkdir(parents=True, exist_ok=False)
    except FileExistsError:
        parser.error("--output must be a new directory; existing evidence is never overwritten")
    checkout = Path(__file__).resolve().parents[1]
    check = Check(args.version, output, checkout)
    try:
        check.verify(checkout)
    except KeyboardInterrupt:
        check.report["error"] = "verification interrupted"
    except Exception as error:
        check.report["error"] = str(error)
    finally:
        try:
            check.cleanup()
        except Exception as error:
            check.report["status"] = "failed"
            check.report.setdefault("cleanup", {})["error"] = str(error)
        check.report["finished_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        report_path = output / "result.json"
        report_path.write_text(json.dumps(check.report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"status": check.report["status"], "version": args.version, "report": str(report_path)}))
    return 0 if check.report["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())

"""Run the credentialless guest core with real Codex in three fresh gVisor Pods.

This fixture uses a scripted model and trusted kubectl exec transport. It does
not enable the application provider or assert production network isolation.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import tarfile
import tempfile
import time

from gvisor_probe import Probe, NAMESPACE, HANDLER, RUNTIME_TYPE, require


ROOT = Path(__file__).resolve().parents[2]
LIMIT = 64 << 20
VERSION = "codex-cli 0.171.0-alpha.13"
ADAPTER_VERSION = "0.5.0"


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def encode(metadata, workspace=None, session=None):
    entries = [("request.json", json.dumps(metadata, separators=(",", ":")).encode())]
    if workspace is not None:
        entries.extend([("workspace.tar", workspace), ("parent.session", session)])
    out = bytearray()
    for name, raw in entries:
        header = tarfile.TarInfo(name)
        header.size, header.mode, header.mtime = len(raw), 0o600, 0
        out.extend(header.tobuf(format=tarfile.USTAR_FORMAT))
        out.extend(raw)
        out.extend(b"\0" * ((-len(raw)) % 512))
    out.extend(b"\0" * 1024)
    require(len(out) <= LIMIT, "fixture input exceeds guest bound")
    return bytes(out)


def files(raw):
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
        members = archive.getmembers()
        names = [member.name for member in members]
        require(len(names) == len(set(names)), "duplicate archive member")
        return {member.name.removeprefix("./"): archive.extractfile(member).read()
                for member in members if member.isfile()}


def decode(raw, expected):
    require(0 < len(raw) <= LIMIT, "guest output is empty or over bound")
    with tarfile.open(fileobj=io.BytesIO(raw), mode="r:") as archive:
        members = archive.getmembers()
        require([member.name for member in members] == ["result.json", "workspace.tar", "session.blob"],
                "unexpected result envelope members")
        require(all(member.isfile() for member in members), "result envelope contains nonregular data")
        data = {member.name: archive.extractfile(member).read() for member in members}
    result = json.loads(data["result.json"])
    require(result["version"] == "native-guest.v1" and result["adapter_id"] == "codex" and
            result["adapter_version"] == ADAPTER_VERSION and result["runtime_version"] == VERSION,
            "result differs from assigned adapter/runtime")
    if expected.get("parent_session_id"):
        require(result["session_id"] == expected["parent_session_id"], "continuation session changed")
    for kind, name in (("workspace", "workspace.tar"), ("session", "session.blob")):
        require(result[kind]["size"] == len(data[name]) and result[kind]["sha256"] == sha(data[name]),
                "result blob size or checksum differs")
    return result, data["workspace.tar"], data["session.blob"]


class Smoke:
    def __init__(self, probe):
        self.probe = probe
        self.image = "agentmoor-nativeguest:" + probe.cluster
        self.pods = []
        self.result = {"status": "in_progress", "model": "scripted-responses",
                       "real_model_key_used": False, "scope": "guest core / native codec only",
                       "rounds": [], "limitations": [
                           "Test transport is one trusted kubectl exec per fresh sandbox, not the production controller.",
                           "No platform API, SaaS Worker publication/checkpoint, model capability gateway, NetworkPolicy or resource-exhaustion acceptance.",
                           "Outer untrusted task execution remains refused until the real provider is enabled and accepted."]}

    def stage(self, stage, **details):
        self.result["stage"] = stage
        print("NATIVE_GUEST_STAGE " + json.dumps({"stage": stage, "time_utc": datetime.now(timezone.utc).isoformat(), **details}), flush=True)

    def build(self):
        self.stage("build_native_image")
        dirty = self.probe.run("git", "-C", ROOT, "status", "--short", "--", "cmd", "internal")
        require(not dirty.strip(), "commit runtime code before building the smoke image")
        self.result["source_commit"] = self.probe.run("git", "-C", ROOT, "rev-parse", "HEAD").strip()
        self.result["smoke_script_sha256"] = sha(Path(__file__).read_bytes())
        self.result["source_status"] = self.probe.run("git", "-C", ROOT, "status", "--short")
        self.result["image"] = self.image
        # Keep build diagnostics out of the wire result while retaining them.
        log = self.probe.directory / "native-image-build.log"
        try:
            with log.open("w") as output:
                subprocess.run(["docker", "build", "-f", str(ROOT / "tests/kubernetes/Dockerfile"),
                                "--build-arg", "AGENT=codex", "-t", self.image, str(ROOT)],
                               stdout=output, stderr=subprocess.STDOUT, check=True, timeout=900)
        finally:
            self.result["build_log_tail"] = log.read_text()[-6000:]

        self.result["docker_image_id"] = json.loads(self.probe.run("docker", "image", "inspect", self.image))[0]["Id"]

    def configure_guest_filesystem(self):
        self.stage("configure_guest_tmpfs_overlays")
        # This is trusted operator configuration in this fixture's owned node.
        # Gofer bind mounts reject RENAME_NOREPLACE even when backed by host
        # tmpfs; the Sentry's tmpfs overlay preserves atomic no-replace restore.
        original = self.probe.node_run("cat", "/etc/containerd/runsc.toml")
        expected = self.probe.result["raw"]["runsc_config"]
        require(original == expected, "owned runtime configuration changed before guest setup")
        configured = original + '  overlay2 = "all:memory,size=512m"\n'
        path = self.probe.directory / "guest-runsc.toml"
        path.write_text(configured)
        self.probe.run("docker", "cp", path, self.probe.node + ":/etc/containerd/runsc.toml")
        require(self.probe.node_run("cat", "/etc/containerd/runsc.toml") == configured,
                "guest runtime configuration was not installed")
        self.result["guest_runtime_config"] = configured
        self.result["guest_runtime_config_sha256"] = sha(configured.encode())
        self.result["filesystem_requirement"] = "Sentry tmpfs overlay on writable bind mounts; host emptyDir medium=Memory alone is insufficient."
        self.result["filesystem_source"] = "https://gvisor.dev/docs/user_guide/filesystem/"

    def prepare_model(self):
        self.stage("prepare_scripted_model")
        self.probe.run("kind", "load", "docker-image", self.image, "--name", self.probe.cluster, timeout=180)
        model = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "native-model", "namespace": NAMESPACE,
                  "labels": {"app": "native-model"}}, "spec": {
                      "automountServiceAccountToken": False, "restartPolicy": "Never",
                      "securityContext": {"runAsNonRoot": True, "runAsUser": 65532},
                      "containers": [{"name": "model", "image": self.image, "imagePullPolicy": "Never",
                                      "command": ["python3", "/opt/test/file_model.py"]}]}}
        service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "model", "namespace": NAMESPACE},
                   "spec": {"selector": {"app": "native-model"}, "ports": [{"port": 8080, "targetPort": 8080}]}}
        self.probe.kube("apply", "-f", "-", data=json.dumps({"apiVersion": "v1", "kind": "List", "items": [model, service]}))
        self.probe.kube("wait", "pod/native-model", "--for=condition=Ready", "--timeout=120s")

    def pod(self, name):
        env = {"HOME": "/run/guest", "TMPDIR": "/tmp", "OPENAI_API_KEY": "fixture",
               "OPENAI_BASE_URL": "http://model:8080/v1", "CODEX_MODEL": "gpt-5",
               "CODEX_FILE_TOOL": "/opt/test/report_tool.py"}
        return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": NAMESPACE,
            "annotations": {"agentmoor-test/readonly-sentinel": "fixture-sentinel"}}, "spec": {
            "runtimeClassName": "agentmoor-gvisor", "automountServiceAccountToken": False,
            "enableServiceLinks": False, "restartPolicy": "Never", "activeDeadlineSeconds": 300,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                "fsGroup": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "guest", "image": self.image, "imagePullPolicy": "Never",
                "command": ["sleep", "600"], "env": [{"name": key, "value": value} for key, value in env.items()],
                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                    "capabilities": {"drop": ["ALL"]}},
                "resources": {"requests": {"cpu": "100m", "memory": "256Mi"},
                              "limits": {"cpu": "2", "memory": "1Gi", "ephemeral-storage": "1Gi"}},
                "volumeMounts": [{"name": "work", "mountPath": "/run/guest"},
                                 {"name": "scratch", "mountPath": "/tmp"},
                                 {"name": "counter", "mountPath": "/run/task"},
                                 {"name": "bootstrap", "mountPath": "/run/bootstrap", "readOnly": True}]}],
            "volumes": [{"name": "work", "emptyDir": {"medium": "Memory", "sizeLimit": "512Mi"}},
                        {"name": "scratch", "emptyDir": {"medium": "Memory", "sizeLimit": "128Mi"}},
                        {"name": "counter", "emptyDir": {"medium": "Memory", "sizeLimit": "1Mi"}},
                        {"name": "bootstrap", "downwardAPI": {"defaultMode": 0o666, "items": [
                            {"path": "sentinel", "fieldRef": {"fieldPath":
                                "metadata.annotations['agentmoor-test/readonly-sentinel']"}}]}}]}}

    def runtime(self, name):
        pod = json.loads(self.probe.kube("get", "pod", name, "-o", "json"))
        sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))
        sandbox, = [row for row in sandboxes["items"] if row["metadata"]["uid"] == pod["metadata"]["uid"]]
        observed = json.loads(self.probe.node_run("crictl", "inspectp", sandbox["id"]))
        require(observed["status"].get("runtimeHandler") == HANDLER and
                observed.get("info", {}).get("runtimeType") == RUNTIME_TYPE, "native Pod has the wrong runtime")
        processes = self.probe.node_run("ps", "-eo", "pid,ppid,comm,args")
        sentries = [line for line in processes.splitlines()
                    if "runsc-sandbox " in line and "--platform=systrap" in line and sandbox["id"] in line]
        require(sentries and all("--overlay2=all:memory,size=512m" in line for line in sentries),
                "native Pod has no matching systrap Sentry with the assigned tmpfs overlay")
        self.result.setdefault("guest_sentry_processes", []).append({"pod": name, "processes": sentries})
        mounts = pod["spec"]["volumes"]
        require({volume["name"] for volume in mounts} == {"work", "scratch", "counter", "bootstrap"} and
                all("emptyDir" in volume for volume in mounts if volume["name"] != "bootstrap"),
                "unexpected native Pod volume")
        bootstrap, = [volume for volume in mounts if volume["name"] == "bootstrap"]
        require(bootstrap.get("downwardAPI", {}).get("defaultMode") == 0o666 and
                bootstrap["downwardAPI"].get("items", [{}])[0].get("fieldRef", {}).get("fieldPath") ==
                "metadata.annotations['agentmoor-test/readonly-sentinel']", "unexpected bootstrap mount")
        require(not pod["spec"].get("automountServiceAccountToken"), "native Pod gained an SA token")
        return pod, sandbox["id"]

    def readonly(self, name):
        observed = json.loads(self.probe.kube("exec", name, "--", "python3", "-c", """
import errno,json,os,secrets,stat
image_directory='/var/tmp'
sentinel='/run/bootstrap/sentinel'
results={'rootfs_statvfs_readonly':bool(os.statvfs('/').f_flag & os.ST_RDONLY),
         'image_directory_mode':stat.S_IMODE(os.stat(image_directory).st_mode),
         'sentinel_mode':stat.S_IMODE(os.stat(sentinel).st_mode)}
def write_error(path,flags):
 try:
  fd=os.open(path,flags,0o600)
 except OSError as error:
  return error.errno
 else:
  os.close(fd)
  return 0
results['image_root_write_errno']=write_error(image_directory+'/agentmoor-readonly-'+secrets.token_hex(8),
                                             os.O_CREAT|os.O_EXCL|os.O_WRONLY)
results['bootstrap_write_errno']=write_error(sentinel,os.O_WRONLY|os.O_TRUNC)
results['bootstrap_preserved']=open(sentinel).read()=='fixture-sentinel'
print(json.dumps(results))
"""))
        self.result.setdefault("readonly_probes", []).append({"pod": name, **observed})
        require(observed["image_directory_mode"] == 0o1777 and observed["sentinel_mode"] == 0o666,
                "read-only probe needs otherwise writable image and mount targets")
        require(observed["rootfs_statvfs_readonly"] and observed["image_root_write_errno"] == 30 and
                observed["bootstrap_write_errno"] == 30 and observed["bootstrap_preserved"],
                "runtime overlay weakened image-root or bootstrap mount read-only protection")

    def diagnostics(self):
        diagnostics = {}
        for key, args in (("pods", ("get", "pods", "-o", "json")),
                          ("events", ("get", "events", "-o", "json")),
                          ("model_log", ("logs", "native-model"))):
            try:
                diagnostics[key] = self.probe.kube(*args)
            except Exception as error:
                diagnostics[key] = str(error)
        self.result["failure_diagnostics"] = diagnostics

    def execute(self):
        self.configure_guest_filesystem()
        self.prepare_model()
        amount = 100 + secrets.randbelow(20000)
        csv = f"region,amount_cents\neast,{amount}\nwest,31\neast,-7\n"
        previous, workspace, session, marker = None, None, None, None
        retained = []
        for number, instruction in enumerate(("Produce the file report from input.txt.",
                                             "Revise the saved report by appending a west refund of 9 cents.",
                                             "Audit the saved report and the restored workspace marker.")):
            name = "native-guest-" + str(number) + "-" + secrets.token_hex(3)
            request = {"version": "native-guest.v1", "adapter_id": "codex", "adapter_version": ADAPTER_VERSION,
                       "runtime_version": VERSION, "spec": {"mode": "file-turn.v1", "instruction": instruction,
                                                               "input": csv if number == 0 else "", "output": "report.json"}}
            if previous:
                request.update(parent_session_id=previous["session_id"], workspace=previous["workspace"], session=previous["session"])
            envelope = encode(request, workspace, session)
            self.stage("create_native_guest_pod", round=number, pod=name)
            self.pods.append(name)
            self.probe.kube("apply", "-f", "-", data=json.dumps(self.pod(name)))
            self.probe.kube("wait", "pod/" + name, "--for=condition=Ready", "--timeout=120s")
            pod, sandbox = self.runtime(name)
            self.readonly(name)
            observed_version = self.probe.kube("exec", name, "--", "/usr/local/bin/codex", "--version").strip()
            require(observed_version == VERSION, "native CLI version differs from the pinned image")
            filesystem = self.probe.kube("exec", name, "--", "python3", "-c", """
import ctypes,json,os,tempfile
root=tempfile.mkdtemp(prefix='fs-probe-',dir='/run/guest')
source,target=root+'/source',root+'/target'
os.mkdir(source)
libc=ctypes.CDLL(None,use_errno=True)
ctypes.set_errno(0)
ret=libc.renameat2(-100,source.encode(),-100,target.encode(),1)
error=ctypes.get_errno()
first_target_exists=os.path.isdir(target)
if first_target_exists:
 open(target+'/marker','w').write('original')
 os.mkdir(source)
 ctypes.set_errno(0)
 collision=libc.renameat2(-100,source.encode(),-100,target.encode(),1)
 collision_errno=ctypes.get_errno()
 preserved=open(target+'/marker').read()=='original'
else:
 collision,collision_errno,preserved=None,None,False
print(json.dumps({'renameat2_noreplace_result':ret,'errno':error,'target_exists':first_target_exists,
                  'collision_result':collision,'collision_errno':collision_errno,'target_preserved':preserved}))
import shutil; shutil.rmtree(root)
""").strip()
            filesystem = json.loads(filesystem)
            self.result.setdefault("filesystem_probes", []).append({"pod": name, **filesystem})
            require(filesystem["renameat2_noreplace_result"] == 0 and filesystem["target_exists"] and
                    filesystem["collision_result"] == -1 and filesystem["collision_errno"] == 17 and
                    filesystem["target_preserved"], "guest filesystem cannot atomically publish without overwriting")
            self.stage("execute_native_guest", round=number, pod=name)
            argv = [*self.probe.base, "-n", NAMESPACE, "exec", "-i", name, "--", "agentmoor", "guest-execute",
                    "--adapter", "codex", "--runtime-cli", "/opt/test/native_count.sh", "--root", "/run/guest/fresh-" + secrets.token_hex(6)]
            process = subprocess.run(argv, input=envelope, capture_output=True, timeout=180)
            if process.returncode != 0:
                self.result["failed_guest"] = {"pod": name, "returncode": process.returncode,
                    "stderr": process.stderr.decode(errors="replace")[-6000:], "stdout_bytes": len(process.stdout)}
                self.result["failed_guest"]["native_counter"] = self.probe.kube("exec", name, "--", "python3", "-c",
                    "from pathlib import Path; p=Path('/run/task/native-executions'); print(p.read_text() if p.exists() else 'absent')").strip()
                self.result["model_log"] = self.probe.kube("logs", "native-model")
            require(process.returncode == 0, "native guest core failed: " + process.stderr.decode(errors="replace")[-6000:])
            self.stage("verify_native_guest_result", round=number, pod=name)
            result, workspace, session = decode(process.stdout, request)
            native_count = self.probe.kube("exec", name, "--", "python3", "-c",
                                         "from pathlib import Path; print(int(Path('/run/task/native-executions').read_text().strip()))").strip()
            require(native_count == "1", "guest ran the native execution more than once")
            contents = files(workspace)
            state = json.loads(contents["state.json"])
            if marker is None:
                marker = state["marker"]
            require(state["marker"] == marker and state["revision"] == int(number > 0), "restored workspace state differs")
            require(contents["sales.csv"].decode() == csv + ("west,-9\n" if number else ""), "workspace modification was not restored")
            expected = {"totals_cents": {"east": amount - 7, "west": 22 if number else 31}}
            if number == 2:
                expected.update(workspace_marker=marker, revision=1)
            require(json.loads(contents["out/report.json"]) == expected, "tool file failed independent business verification")
            events = [json.loads(line) for line in contents["tool-events.jsonl"].splitlines()]
            require([row["operation"] for row in events] == ["produce", "revise", "audit"][:number+1], "native tool sequence differs")
            rollouts = [data for path, data in files(session).items() if path.endswith(".jsonl")]
            require(len(rollouts) == 1 and marker.encode() not in rollouts[0], "session archive or workspace-only marker differs")
            native = [json.loads(line) for line in rollouts[0].splitlines()]
            require(native[0]["payload"]["id"] == result["session_id"], "native history ID differs from handoff")
            require(result["text"] == "done", "unexpected fixture final text")
            self.result["rounds"].append({"operation": ["produce", "revise", "audit"][number],
                "pod_uid": pod["metadata"]["uid"], "sandbox_id": sandbox, "runtime_handler": HANDLER,
                "runtime_type": RUNTIME_TYPE, "platform": "systrap", "native_exec_count": 1,
                "observed_cli_version": observed_version,
                "session_id_sha256": sha(result["session_id"].encode()),
                "input_bundle_sha256": sha(envelope), "output_bundle_sha256": sha(process.stdout),
                "workspace_sha256": sha(workspace), "session_sha256": sha(session),
                "report_sha256": sha(contents["out/report.json"]), "report": expected})
            retained.append((sha(process.stdout), process.stdout))
            self.probe.kube("delete", "pod", name, "--wait=true", "--timeout=90s")
            require(all(sha(raw) == digest for digest, raw in retained), "earlier frozen handoff bytes changed")
            previous = result
        require(len({row["pod_uid"] for row in self.result["rounds"]}) == 3, "native rounds shared a Pod")
        require(len({row["session_id_sha256"] for row in self.result["rounds"]}) == 1, "native session changed")
        self.result["model_log"] = self.probe.kube("logs", "native-model")
        for operation in ("produce", "revise", "audit"):
            require("FILE_MODEL " + operation + " written" in self.result["model_log"], "model fixture did not observe tool confirmation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, help="reuse official runtime bundle after pinned checksum verification")
    parser.add_argument("--output", type=Path, default=os.environ.get("NATIVE_GUEST_OUTPUT"), help="new evidence JSON path (or NATIVE_GUEST_OUTPUT)")
    options = parser.parse_args()
    if options.output:
        require(not os.path.lexists(options.output) and options.output.parent.is_dir(), "evidence path must be new with an existing parent")
    def interrupted(*_):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    failure = None
    with tempfile.TemporaryDirectory(prefix="agentmoor-native-guest-smoke-") as directory:
        probe = Probe(directory, options.bundle)
        smoke = Smoke(probe)
        smoke.result["started_at_utc"] = datetime.now(timezone.utc).isoformat()
        try:
            smoke.build()
            smoke.stage("probe_real_runtime")
            probe.execute()
            smoke.execute()
        except BaseException as error:
            failure = error
            smoke.result["failure"] = {"type": type(error).__name__, "message": str(error)}
        finally:
            if failure and probe.created:
                smoke.diagnostics()
            try:
                probe.cleanup()
            except Exception as error:
                failure = failure or error
            # Remove only this fixture's image tag, after all sandbox Pods are gone.
            subprocess.run(["docker", "image", "rm", smoke.image], capture_output=True)
            image_left = subprocess.run(["docker", "image", "inspect", smoke.image], capture_output=True)
            smoke.result["owned_image_tag_removed"] = image_left.returncode != 0
            if image_left.returncode == 0:
                failure = failure or RuntimeError("owned fixture image tag remains after cleanup")
            smoke.result["runtime_probe"] = probe.result
            smoke.result["status"] = "failed" if failure else "native_guest_core_passed"
            smoke.result["finished_at_utc"] = datetime.now(timezone.utc).isoformat()
            if options.output:
                options.output.write_text(json.dumps(smoke.result, ensure_ascii=False, indent=2) + "\n")
            print("NATIVE_GUEST_SMOKE_RESULT " + json.dumps({key: smoke.result.get(key) for key in ("status", "source_commit", "failure")}), flush=True)
    return int(failure is not None)


if __name__ == "__main__":
    raise SystemExit(main())

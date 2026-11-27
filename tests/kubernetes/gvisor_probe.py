"""Probe pinned gVisor in an owned disposable kind node, without AgentMoor workloads."""
import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import tomllib
import uuid


RELEASE = "release-20261123.1"
RELEASE_COMMIT = "81f68cb7d4661a9313d891a1758a765abe8eb27e"
RELEASE_URL = "https://github.com/gvisor/gvisor/releases/tag/" + RELEASE
DOWNLOAD_BASE = "https://github.com/gvisor/gvisor/releases/download/" + RELEASE
BUNDLE_SHA512 = {
    "aarch64": "930967c01eaa8c91a90903ba866070dd165c580d3197287747bb1444ee11dab7350db11d8f90bf00efb2cdcfb2318602d3f329c522c9e9436a86fe6a93c7b7fc",
    "x86_64": "b8bcf8ebda5a0e3aa43026f26128759c3f74abc94d3f53839963fad01d152b8ec1c2c01cc0c84e8820aa5fb04fbc50df60332911f58884113a7984287737313a",
}
KIND_IMAGE = "kindest/node:v1.35.0"
IMAGE = "docker.io/library/busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
NAMESPACE = "agentmoor-gvisor-probe"
HANDLER = "runsc"
MISSING_HANDLER = "agentmoor-nonexistent-handler"
RUNTIME_TYPE = "io.containerd.runsc.v1"
CORE_BINARIES = ("runsc", "containerd-shim-runsc-v1", "gvisor-bin/gvisor_sentry")
LIMITATIONS = [
    "BusyBox runtime/handler feasibility only; no AgentMoor, native CLI or Skill acceptance.",
    "No Worker credential separation, egress/NetworkPolicy, resource exhaustion, cancellation, tenant escape or production HA acceptance.",
    "No KVM/VM boundary tested; systrap uses the Linux host kernel and the gVisor userspace kernel.",
    "RuntimeClass names alone are insufficient; this probe also checks node CRI type, running systrap process and installed binary hashes.",
]


def now():
    return datetime.now(timezone.utc).isoformat()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path, algorithm="sha512"):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, algorithm).hexdigest()


def verify_bundle(path, architecture):
    require(architecture in BUNDLE_SHA512, "unsupported gVisor bundle architecture")
    actual = digest(path)
    require(actual == BUNDLE_SHA512[architecture], "gVisor bundle SHA512 differs from the pinned official release")
    return actual


def extract_bundle(archive, target):
    with tarfile.open(archive, "r:bz2") as bundle:
        members = bundle.getmembers()
        names = {row.name.rstrip("/") for row in members}
        require(set(CORE_BINARIES) <= names, "incomplete multi-file gVisor bundle")
        require(all(row.isfile() or row.isdir() for row in members), "unexpected bundle link or special file")
        require(all(row.name == "gvisor-bin" or row.name in CORE_BINARIES[:2] or
                    row.name.startswith("gvisor-bin/") and ".." not in Path(row.name).parts
                    for row in members), "unexpected bundle path")
        bundle.extractall(target, filter="data")


def verify_observations(positive, negative, inspect, cri_pods, events, processes, log):
    require(positive["spec"]["runtimeClassName"] == "agentmoor-gvisor", "positive RuntimeClass changed")
    spec = positive["spec"]
    require(spec.get("automountServiceAccountToken") is False and spec.get("enableServiceLinks") is False,
            "positive Pod gained automatic credentials or service environment")
    security = spec["securityContext"]
    require(security.get("runAsNonRoot") is True and security.get("runAsUser") == 65532 and
            security.get("runAsGroup") == 65532 and security.get("seccompProfile") == {"type": "RuntimeDefault"},
            "positive Pod security context changed")
    security = spec["containers"][0]["securityContext"]
    require(security.get("allowPrivilegeEscalation") is False and security.get("readOnlyRootFilesystem") is True and
            security.get("capabilities") == {"drop": ["ALL"]}, "positive container security context changed")
    require(positive["status"]["phase"] == "Running", "positive probe is not running")
    workers = positive["status"].get("containerStatuses", [])
    require(len(workers) == 1, "positive probe must have one actual container")
    worker = workers[0]
    require(worker.get("ready") and worker["restartCount"] == 0 and "running" in worker["state"],
            "positive container is not a stable running instance")
    require(worker.get("user", {}).get("linux", {}).get("uid") == 65532 and
            worker["user"]["linux"].get("gid") == 65532, "actual sandbox user is not 65532")
    require(inspect["status"]["metadata"]["uid"] == positive["metadata"]["uid"], "CRI evidence identifies another Pod")
    require(inspect["status"].get("runtimeHandler") == HANDLER and
            inspect.get("info", {}).get("runtimeType") == RUNTIME_TYPE, "CRI is not using the real runsc runtime")
    sandbox_id = inspect["status"]["id"]
    require(any("runsc-sandbox " in line and "--platform=systrap" in line and sandbox_id in line
                for line in processes.splitlines()), "no running systrap Sentry matches the CRI sandbox")
    require("AGENTMOOR_GVISOR_PROBE" in log and "uid=65532 gid=65532" in log and "gvisor" in log,
            "positive workload did not report the expected user and virtual kernel")
    require(negative["spec"]["runtimeClassName"] == "agentmoor-missing" and
            negative["status"]["phase"] == "Pending", "missing handler unexpectedly ran")
    require(any(c["type"] == "PodScheduled" and c["status"] == "True"
                for c in negative["status"].get("conditions", [])), "negative Pod did not reach a node")
    require(all(not row.get("started") and not row.get("containerID")
                for row in negative["status"].get("containerStatuses", [])), "missing handler started a container")
    require(not any(row["metadata"]["uid"] == negative["metadata"]["uid"] for row in cri_pods["items"]),
            "missing handler created a CRI sandbox")
    failures = [row for row in events["items"]
                if row.get("involvedObject", {}).get("uid") == negative["metadata"]["uid"] and
                row.get("reason") == "FailedCreatePodSandBox" and
                'no runtime for "' + MISSING_HANDLER + '" is configured' in row.get("message", "")]
    require(sum(row.get("count", 1) for row in failures) >= 2, "missing handler has no repeated explicit rejection")
    return {"positive_pod_uid": positive["metadata"]["uid"], "sandbox_id": sandbox_id,
            "runtime_handler": HANDLER, "cri_runtime_type": RUNTIME_TYPE, "platform": "systrap",
            "actual_uid": 65532, "actual_gid": 65532, "restart_count": worker["restartCount"],
            "image_id": worker["imageID"], "negative_pod_uid": negative["metadata"]["uid"],
            "negative_phase": "Pending", "negative_container_started": False,
            "negative_sandbox_created": False, "negative_failure_count": sum(row.get("count", 1) for row in failures)}


class Probe:
    def __init__(self, directory, bundle=None):
        self.directory, self.bundle = Path(directory), bundle
        self.cluster = "agentmoor-" + uuid.uuid4().hex[:12]
        self.node = self.cluster + "-control-plane"
        self.kubeconfig = self.directory / "kubeconfig"
        self.base = ["kubectl", "--context", "kind-" + self.cluster, "--kubeconfig", str(self.kubeconfig)]
        self.created = False
        self.result = {"status": "in_progress", "started_at_utc": now(), "cluster": self.cluster,
                       "release_tag": RELEASE, "release_commit": RELEASE_COMMIT, "release_url": RELEASE_URL,
                       "kind_node_image": KIND_IMAGE, "busybox_image": IMAGE, "limitations": LIMITATIONS,
                       "raw": {}, "cleanup": {"completed": False}}

    def stage(self, name):
        print("GVISOR_PROBE_STAGE " + json.dumps({"stage": name, "time_utc": now()}, sort_keys=True), flush=True)

    def run(self, *args, data=None, timeout=60, record=None):
        process = subprocess.run(list(map(str, args)), input=data, text=True, capture_output=True, timeout=timeout)
        if record:
            self.result["raw"][record] = process.stdout + process.stderr
        if process.returncode:
            raise RuntimeError("command failed: " + " ".join(map(str, args)) + "\n" + (process.stdout + process.stderr)[-6000:])
        return process.stdout

    def node_run(self, *args):
        require(re.fullmatch(r"agentmoor-[0-9a-f]{12}-control-plane", self.node), "unsafe node name")
        return self.run("docker", "exec", self.node, *args)

    def kube(self, *args, data=None):
        return self.run(*self.base, "-n", NAMESPACE, *args, data=data)

    def execute(self):
        self.stage("preflight")
        for command in ("docker", "kind", "kubectl", "curl"):
            require(shutil.which(command), "missing prerequisite: " + command)
        require(sys.version_info >= (3, 12), "Python 3.12+ is required for safe tar extraction")
        host = json.loads(self.run("docker", "version", "--format", "{{json .}}"))
        require(host["Server"]["Os"] == "linux", "Docker server must be Linux")
        architecture = {"arm64": "aarch64", "amd64": "x86_64"}.get(host["Server"]["Arch"])
        require(architecture in BUNDLE_SHA512, "unsupported Docker server architecture")
        self.result["raw"]["docker_version"] = host
        self.result["architecture"] = architecture
        self.result["bundle_url"] = DOWNLOAD_BASE + "/gvisor-" + architecture + ".tar.bz2"
        archive = self.directory / "gvisor.tar.bz2"
        self.stage("freeze_and_verify_bundle")
        if self.bundle:
            shutil.copyfile(self.bundle, archive)
            self.result["bundle_source"] = "provided file, copied then checked against the pinned official SHA512"
        else:
            self.run("curl", "--fail", "--location", "--silent", "--show-error", "--connect-timeout", "15",
                     "--max-time", "180", "--output", archive, self.result["bundle_url"], timeout=190)
            self.result["bundle_source"] = "fixed official release download"
        self.result["bundle_sha512"] = verify_bundle(archive, architecture)
        self.result["bundle_size"] = archive.stat().st_size
        binary_dir = self.directory / "bundle"
        binary_dir.mkdir()
        extract_bundle(archive, binary_dir)
        require(not self.run("docker", "ps", "-a", "--filter", "name=^/" + self.node + "$", "--format", "{{.ID}}").strip(),
                "owned node name already exists")
        self.stage("create_owned_kind_cluster")
        self.created = True
        self.run("kind", "create", "cluster", "--name", self.cluster, "--image", KIND_IMAGE,
                 "--kubeconfig", self.kubeconfig, "--wait", "2m", timeout=180, record="kind_create")
        node = json.loads(self.run("docker", "inspect", self.node))[0]
        require(node["Config"]["Labels"].get("io.x-k8s.kind.cluster") == self.cluster, "node is not owned by this kind cluster")
        self.result["node_image_id"] = node["Image"]
        self.result["raw"]["node_uname"] = self.node_run("uname", "-a")
        require(self.node_run("uname", "-m").strip() == architecture, "node/bundle architecture differs")
        kernel = self.node_run("uname", "-r").strip()
        match = re.match(r"(\d+)\.(\d+)", kernel)
        require(match and tuple(map(int, match.groups())) >= (5, 6), "gVisor requires Linux 5.6+")
        require("trap" in self.node_run("cat", "/proc/sys/kernel/seccomp/actions_avail").split(), "systrap seccomp trap unavailable")
        self.result["raw"]["node_containerd_version"] = self.node_run("containerd", "--version")
        original = self.node_run("cat", "/etc/containerd/config.toml")
        config = tomllib.loads(original)
        require(config["version"] == 2, "this pinned kind probe expects containerd config v2")
        require("runsc" not in config["plugins"]["io.containerd.grpc.v1.cri"]["containerd"]["runtimes"],
                "fresh node already has a runsc handler")
        addition = '\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]\n  runtime_type = "io.containerd.runsc.v1"\n  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]\n    TypeUrl = "io.containerd.runsc.v1.options"\n    ConfigPath = "/etc/containerd/runsc.toml"\n'
        configured = original + addition
        tomllib.loads(configured)
        settings = '[runsc_config]\n  platform = "systrap"\n  debug = "true"\n  debug-log = "/run/runsc-probe/"\n'
        self.result["raw"].update({"containerd_before": original, "containerd_after": configured, "runsc_config": settings})
        self.stage("install_only_in_owned_node")
        self.run("docker", "cp", str(binary_dir) + "/.", self.node + ":/usr/local/bin/")
        for name, value, destination in (("containerd.toml", configured, "/etc/containerd/config.toml"),
                                         ("runsc.toml", settings, "/etc/containerd/runsc.toml")):
            path = self.directory / name
            path.write_text(value)
            self.run("docker", "cp", path, self.node + ":" + destination)
        self.node_run("mkdir", "-p", "/run/runsc-probe")
        version = self.node_run("/usr/local/bin/runsc", "--version")
        require(version.splitlines()[0] == "runsc version " + RELEASE, "installed runsc version differs")
        self.result["runsc_version"] = version
        sums = self.node_run("sha512sum", *("/usr/local/bin/" + name for name in CORE_BINARIES))
        for line, name in zip(sums.splitlines(), CORE_BINARIES):
            require(line.split()[0] == digest(binary_dir / name), "installed binary differs from verified bundle: " + name)
        require(len(sums.splitlines()) == len(CORE_BINARIES), "incomplete installed binary checksums")
        self.result["raw"]["installed_core_binary_sha512"] = sums
        self.node_run("systemctl", "restart", "containerd")
        require(self.node_run("systemctl", "is-active", "containerd").strip() == "active", "node containerd did not restart")
        self.stage("run_real_positive_and_missing_handler_negative")
        manifest = self.manifest()
        self.result["raw"]["manifest"] = manifest
        self.run(*self.base, "apply", "-f", "-", data=json.dumps(manifest))
        until = time.monotonic() + 120
        while time.monotonic() < until:
            pods = json.loads(self.kube("get", "pods", "-o", "json"))
            positive = next(row for row in pods["items"] if row["metadata"]["name"] == "gvisor-positive")
            if positive.get("status", {}).get("phase") == "Running":
                break
            require(positive.get("status", {}).get("phase") not in ("Failed", "Succeeded"), "positive Pod terminated before observation")
            time.sleep(.5)
        else:
            raise TimeoutError("real gVisor Pod did not run within 120 seconds")
        # Observe the unsupported handler across multiple kubelet attempts,
        # rather than accepting one transient Pending snapshot as isolation.
        time.sleep(10)
        pods = json.loads(self.kube("get", "pods", "-o", "json"))
        positive, = [row for row in pods["items"] if row["metadata"]["name"] == "gvisor-positive"]
        negative, = [row for row in pods["items"] if row["metadata"]["name"] == "missing-handler-negative"]
        events = json.loads(self.kube("get", "events", "-o", "json"))
        cri_pods = json.loads(self.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))
        sandbox, = [row for row in cri_pods["items"] if row["metadata"]["uid"] == positive["metadata"]["uid"]]
        require(re.fullmatch(r"[0-9a-f]{64}", sandbox["id"]), "invalid CRI sandbox ID")
        inspect = json.loads(self.node_run("crictl", "inspectp", sandbox["id"]))
        processes = "\n".join(line for line in self.node_run("ps", "-eo", "pid,ppid,comm,args").splitlines()
                               if any(marker in line for marker in ("runsc", "gvisor_sentry", "gvisor-bin")))
        log = self.kube("logs", "gvisor-positive")
        self.result["raw"].update({"pods": pods, "events": events, "cri_pods": cri_pods,
                                    "cri_inspect": inspect, "runtime_processes": processes, "positive_log": log})
        self.result["proof"] = verify_observations(positive, negative, inspect, cri_pods, events, processes, log)
        self.result["proof"]["negative_observation_seconds"] = 10
        self.stage("runtime_feasibility_passed")

    @staticmethod
    def manifest():
        pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {"namespace": NAMESPACE, "name": "gvisor-positive"},
               "spec": {"runtimeClassName": "agentmoor-gvisor", "restartPolicy": "Never", "activeDeadlineSeconds": 240,
                        "automountServiceAccountToken": False, "enableServiceLinks": False,
                        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                            "seccompProfile": {"type": "RuntimeDefault"}},
                        "containers": [{"name": "probe", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
                                        "command": ["sh", "-c", "echo AGENTMOOR_GVISOR_PROBE; id; uname -a; cat /proc/version; sleep 600"],
                                        "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                                            "capabilities": {"drop": ["ALL"]}},
                                        "resources": {"requests": {"cpu": "10m", "memory": "32Mi"},
                                                      "limits": {"cpu": "500m", "memory": "256Mi", "ephemeral-storage": "64Mi"}}}]}}
        negative = copy.deepcopy(pod)
        negative["metadata"]["name"] = "missing-handler-negative"
        negative["spec"]["runtimeClassName"] = "agentmoor-missing"
        negative["spec"]["containers"][0]["command"] = ["sh", "-c", "echo UNEXPECTED_RUNTIME_FALLBACK; sleep 600"]
        return {"apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": NAMESPACE}},
            {"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": {"name": "agentmoor-gvisor"}, "handler": HANDLER},
            {"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": {"name": "agentmoor-missing"}, "handler": MISSING_HANDLER},
            pod, negative]}

    def cleanup(self):
        if not self.created:
            self.result["cleanup"] = {"completed": True, "cluster_create_started": False}
            return
        self.stage("cleanup_owned_cluster")
        try:
            self.run("kind", "delete", "cluster", "--name", self.cluster, "--kubeconfig", self.kubeconfig,
                     timeout=120, record="kind_delete")
            left = self.run("docker", "ps", "-a", "--filter", "name=^/" + self.node + "$", "--format", "{{.Names}}").strip()
            require(not left, "owned kind node remains after cleanup")
            self.kubeconfig.unlink(missing_ok=True)
            self.result["cleanup"] = {"completed": True, "node_containers_remaining": [], "private_kubeconfig_removed": True}
        except Exception as error:
            self.result["cleanup"] = {"completed": False, "error": str(error), "owned_cluster": self.cluster}
            raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, help="reuse a local official bundle; its pinned SHA512 is still mandatory")
    parser.add_argument("--output", type=Path, default=os.environ.get("GVISOR_PROBE_OUTPUT"), help="write complete evidence to a new JSON path")
    options = parser.parse_args()
    if options.output:
        options.output = Path(options.output).resolve()
        require(not os.path.lexists(options.output), "evidence output must be a new path")
        require(options.output.parent.is_dir(), "evidence output parent must exist")
    script = Path(__file__).resolve()
    source = {"probe_script_sha256": digest(script, "sha256"), "agentmoor_workload_executed": False}
    repository = script.parents[2]
    for key, arguments in (("repository_commit", ["rev-parse", "HEAD"]),
                           ("repository_status", ["status", "--short", "--untracked-files=all"])):
        observed = subprocess.run(["git", "-C", str(repository), *arguments], text=True, capture_output=True)
        source[key] = observed.stdout.strip() if observed.returncode == 0 else None
    def interrupted(*_):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    with tempfile.TemporaryDirectory(prefix="agentmoor-gvisor-") as directory:
        probe = Probe(directory, options.bundle)
        probe.result["source"] = source
        failure = None
        try:
            probe.execute()
        except BaseException as error:
            failure = error
            probe.result["failure"] = {"type": type(error).__name__, "message": str(error)}
            if probe.created:
                for name, arguments in (("failure_pods", ["get", "pods", "-o", "json"]),
                                         ("failure_events", ["get", "events", "-o", "json"])):
                    try:
                        probe.result["raw"][name] = json.loads(probe.kube(*arguments))
                    except Exception:
                        pass
        finally:
            try:
                probe.cleanup()
            except Exception as error:
                failure = failure or error
            probe.result["status"] = "failed" if failure else "runtime_feasibility_passed"
            probe.result["finished_at_utc"] = now()
            if options.output:
                with options.output.open("x") as stream:
                    json.dump(probe.result, stream, indent=2)
                    stream.write("\n")
            summary = {key: probe.result.get(key) for key in ("status", "cluster", "release_tag", "architecture", "proof", "cleanup", "failure")}
            summary["evidence_output"] = str(options.output) if options.output else None
            print("GVISOR_FEASIBILITY_RESULT " + json.dumps(summary, sort_keys=True), flush=True)
        return 1 if failure else 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Probe real Calico/gVisor network and resource boundaries in an owned kind node.

Only synthetic fixture processes execute. This is not production guest-run,
Gateway/Codex/Skill acceptance, SaaS publication, or permission to open the gate.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import signal
import subprocess
import tempfile
import time

from gvisor_probe import Probe, NAMESPACE, require, HANDLER, RUNTIME_TYPE, IMAGE

ROOT = Path(__file__).resolve().parents[2]
FIXTURE_BASE = "python@sha256:a5a7017efc5969e00ae2b91c8eb0631049a569652aa4ffc4501769049ae45c72"
NODE_MEMORY_BYTES = 2 << 30
POD_CIDR = "10.244.0.0/16"
CALICO_VERSION = "v3.33.0"
CALICO_URL = "https://raw.githubusercontent.com/projectcalico/calico/v3.33.0/manifests/calico.yaml"
CALICO_SHA256 = "2de8f47595fb9c41b3f47d7b767a1f8e72ecf84057af834738ff12689a234da5"
CALICO_IMAGES = ("quay.io/calico/calico:v3.33.0", "quay.io/calico/third-party-cni-plugins:v3.33.0", "quay.io/calico/node:v3.33.0")
CALICO_DIGESTS = {
    "quay.io/calico/calico:v3.33.0": "4ebc20730127bc9e8bf1db5f5a2d8b58cf37fe0e891b3c7b5471992221d213e5",
    "quay.io/calico/third-party-cni-plugins:v3.33.0": "0f32f48279484c0d6f0e0d0e767b2d0e1a6c6ca34cb3421a61b989d992f3df41",
    "quay.io/calico/node:v3.33.0": "565d8db3200e443955557f8f28ff44ab8a4a456754bc74a19c4f2f3444af9b1c",
}

GUEST_NS, CONTROL_NS, TENANT_NS = "agentmoor-guests-fixture", "agentmoor-control-fixture", "agentmoor-tenant-fixture"
GUEST_LABEL = {"app.kubernetes.io/name": "agentmoor-sandbox"}
GATEWAY_LABEL = {"app.kubernetes.io/name": "agentmoor-platform-fixture"}
LIMITATIONS = [
    "Synthetic network/resource fixtures only; no vendor CLI, production Gateway, Skill or SaaS execution/publication.",
    "Finite deny canaries detect common missing-CNI/policy mistakes; they do not prove arbitrary Internet isolation.",
    "gVisor guest processes are not host processes. Guest RLIMIT_NPROC and host pids.max are measured separately.",
    "No host network or existing Kubernetes cluster is configured; all CNI privileges and metadata DNAT exist only in the owned kind node.",
]


def sha(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def network_policy():
    return {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": "gateway-only", "namespace": GUEST_NS}, "spec": {
                "podSelector": {"matchLabels": GUEST_LABEL}, "policyTypes": ["Ingress", "Egress"], "ingress": [],
                "egress": [{"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": CONTROL_NS}},
                                    "podSelector": {"matchLabels": GATEWAY_LABEL}}],
                            "ports": [{"protocol": "TCP", "port": 7443}]}]}}


def verify_network(observation):
    require(observation.get("cni_ready") is True, "real policy-enforcing CNI is not ready")
    gateway = observation["gateway_fixture"]
    uid, service, endpoints, log = gateway["pod"]["metadata"]["uid"], gateway["service"], gateway["endpoints"], gateway["log"]
    require(log["server_ports"] == [7443, 7401] and log["process_id"] > 0, "dual-port canary does not share one fixture process")
    require(service["spec"]["selector"] == GATEWAY_LABEL and {p["port"] for p in service["spec"]["ports"]} == {7443, 7401}, "gateway Service ports differ")
    actual_endpoints = [endpoint for item in endpoints["items"] for endpoint in item["endpoints"]]
    require(actual_endpoints and all(endpoint.get("targetRef", {}).get("uid") == uid for endpoint in actual_endpoints), "dual-port Service targets another Pod")
    cni_pods = observation["cni_pods"]["items"]
    fixed_node = "quay.io/calico/node@sha256:" + CALICO_DIGESTS["quay.io/calico/node:v3.33.0"]
    require(cni_pods and all(pod["status"]["phase"] == "Running" and
            any(c["image"] == fixed_node for c in pod["spec"]["containers"]) and pod["status"].get("containerStatuses") and
            all(c.get("ready") is True and "running" in c.get("state", {}) for c in pod["status"].get("containerStatuses", []))
            for pod in cni_pods), "policy CNI has no fixed running node process")
    rows = observation.get("rows", [])
    expected = {"gateway-tls", "main-api", "kubernetes-api", "other-tenant", "metadata-fixture", "outside-cidr", "dns-tcp", "dns-udp"}
    require({row["name"] for row in rows} == expected and len(rows) == len(expected), "network canary matrix incomplete")
    by_name = {row["name"]: row for row in rows}
    require(by_name["gateway-tls"]["guest"]["ip"] == by_name["main-api"]["guest"]["ip"], "main API canary differs from gateway Service IP")
    ports = {"gateway-tls": 7443, "main-api": 7401, "kubernetes-api": 443, "other-tenant": 8123,
             "metadata-fixture": 80, "outside-cidr": 8123, "dns-tcp": 53, "dns-udp": 53}
    require(by_name["metadata-fixture"]["guest"]["ip"] == "169.254.169.254", "metadata canary is not the owned link-local simulation")
    for row in rows:
        guest, control = row["guest"], row["control"]
        ipaddress.ip_address(guest["ip"])
        require(all(guest[field] == control[field] for field in ("name", "ip", "port", "protocol")), "control does not test the same destination")
        require(guest["name"] == row["name"] and guest["port"] == ports[row["name"]] and
                guest["protocol"] == ("UDP" if row["name"] == "dns-udp" else "TCP"), "canary port/protocol differs")
        require(row["control"]["reachable"] is True, "negative canary has no reachable control baseline")
        if row["name"] in ("gateway-tls", "main-api"):
            require(control.get("response", {}).get("fixture") == log["fixture"] and
                    control.get("response", {}).get("process_id") == log["process_id"], "dual-port traffic did not reach the same fixture process")
        if row["name"] == "gateway-tls":
            require(row["guest"]["reachable"] is True and row["guest"].get("tls_verified") is True and
                    row["guest"].get("response", {}).get("fixture") == log["fixture"] and row["guest"].get("response", {}).get("process_id") == log["process_id"],
                    "allowed gateway TLS failed")
        else:
            require(row["guest"]["reachable"] is False and row["guest"].get("timeout") is True,
                    "deny canary is reachable or inconclusive")
    return True


def guarded_fixture(observation, execute):
    verify_network(observation)
    execute()


def parse_counter(raw):
    return {key: int(value) for key, value in (line.split() for line in raw.splitlines() if line.strip())}


def verify_resource(mode, record):
    require(record.get("runtime_verified") is True, "resource fixture has no actual runsc Sentry")
    before, after = record["before"], record["after"]
    require(record.get("node_swap_max") == "0", "resource proof has no actual parent cgroup swap denial")
    require(any(row.get("pids.max") == "128" for row in before), "host Pod PID cgroup limit missing")
    if mode == "cpu":
        require(any(row.get("cpu.max") == "20000 100000" for row in before), "actual CPU quota differs")
        require(record["cpu_throttled_delta"] > 0 and record["cpu_usage_usec_delta"] > 0,
                "bounded CPU workload did not observe throttling")
    elif mode in ("memory", "file-memory"):
        require(any(row.get("memory.max") == str(128 << 20) for row in before), "actual memory cgroup differs")
        require(record["oom_kill_delta"] > 0 and record["terminated"] is True,
                "memory workload was not killed by its hard memory cgroup")
    elif mode == "tmpfs":
        fixture = record["fixture"]
        require(fixture.get("write_errno") == 28 and 0 < fixture["written_bytes"] <= fixture["filesystem_capacity_bytes"],
                "tmpfs did not demonstrate actual ENOSPC")
        require(record["oom_kill_delta"] == 0, "ENOSPC test was actually an OOM")
    elif mode == "pids":
        fixture = record["fixture"]
        require(fixture.get("guest_rlimit_nproc") == [32, 32] and fixture.get("hard_raise_denied") is True and
                fixture.get("fork_errno") == 11 and 0 < fixture["children"] <= 31, "guest PID hard limit did not deny fork")
    elif mode == "wall":
        require(record["pod_reason"] == "DeadlineExceeded" and record["terminated"] is True,
                "original Pod wall deadline did not terminate fixture")
    elif mode == "cancel":
        require(record.get("cancelled") is True and record.get("physical_cleanup") is True, "Pod cancellation left a live sandbox")
    else:
        raise RuntimeError("unknown resource proof")
    return True


class SecurityProbe(Probe):
    def __init__(self, directory, bundle, manifest, enforcing=True):
        super().__init__(directory, bundle)
        self.manifest_source = manifest
        self.enforcing = enforcing
        self.image = "agentmoor-security-fixture:" + self.cluster
        self.outside = self.cluster + "-outside-fixture"
        self.security = {"status": "in_progress", "limitations": LIMITATIONS, "resources": {}, "network": {}, "cleanup": {}}
        self.fixture_pods = []
        self.nonce = self.cluster + "-synthetic"

    def prepare(self):
        observed = subprocess.run(["docker", "image", "inspect", IMAGE], capture_output=True, text=True)
        if observed.returncode:
            super().run("docker", "pull", IMAGE, timeout=240)
        if not self.enforcing:
            self.security["cni"] = {"mechanism": "kindnet", "required_canary_enforcement": "not yet measured",
                                    "purpose": "default CNI negative control only; must refuse when a denied canary is reachable"}
            self.build_fixture()
            return
        self.stage("freeze_cni_manifest_and_images")
        original = self.directory / "calico.yaml"
        if self.manifest_source:
            shutil.copyfile(self.manifest_source, original)
        else:
            super().run("curl", "--fail", "--location", "--silent", "--show-error", "--max-time", "180", "--output", original, CALICO_URL, timeout=190)
        require(sha(original) == CALICO_SHA256, "Calico manifest differs from pinned official source")
        frozen = original.read_text()
        images = []
        for reference in CALICO_IMAGES:
            observed = subprocess.run(["docker", "image", "inspect", reference], capture_output=True, text=True)
            if observed.returncode:
                super().run("docker", "pull", reference, timeout=600)
                observed = subprocess.run(["docker", "image", "inspect", reference], capture_output=True, text=True, check=True)
            image = json.loads(observed.stdout)[0]
            repository = reference.rsplit(":", 1)[0]
            digests = [value for value in image["RepoDigests"] if value.startswith(repository + "@sha256:")]
            require(digests == [repository + "@sha256:" + CALICO_DIGESTS[reference]], "fixed Calico image differs from pinned digest")
            frozen = frozen.replace("image: " + reference, "image: " + digests[0])
            images.append({"version_reference": reference, "frozen_reference": digests[0], "image_id": image["Id"],
                           "architecture": image["Architecture"]})
        # Match the official kind VXLAN topology, with no BGP/IPIP need on one owned node.
        frozen = frozen.replace('name: CALICO_IPV4POOL_IPIP\n              value: "Always"', 'name: CALICO_IPV4POOL_IPIP\n              value: "Never"')
        frozen = frozen.replace('name: CALICO_IPV4POOL_VXLAN\n              value: "Never"', 'name: CALICO_IPV4POOL_VXLAN\n              value: "Always"')
        frozen = frozen.replace('# - name: CALICO_IPV4POOL_CIDR\n            #   value: "192.168.0.0/16"', '- name: CALICO_IPV4POOL_CIDR\n              value: "' + POD_CIDR + '"')
        require('name: CALICO_IPV4POOL_CIDR\n              value: "' + POD_CIDR + '"' in frozen, "Calico Pod pool override missing")
        self.frozen_calico = self.directory / "calico-frozen.yaml"
        self.frozen_calico.write_text(frozen)
        self.security["cni"] = {"version": CALICO_VERSION, "url": CALICO_URL, "original_sha256": CALICO_SHA256,
                                "applied_sha256": sha(self.frozen_calico), "images": images,
                                "transform": "resolved immutable image digests; IPIP Never / VXLAN Always; explicit nonoverlapping Pod CIDR " + POD_CIDR,
                                "privileges": "official Calico DaemonSet root/privileged plus cluster RBAC, only in owned disposable cluster"}
        self.build_fixture()

    def build_fixture(self):
        self.stage("build_synthetic_fixture_image")
        tls = self.directory / "tls"
        tls.mkdir()
        self.cert, self.key = tls / "tls.crt", tls / "tls.key"
        super().run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", self.key,
                    "-out", self.cert, "-days", "1", "-subj", "/CN=gateway.fixture",
                    "-addext", "subjectAltName=DNS:gateway.fixture", timeout=30)
        context = self.directory / "fixture-build"
        context.mkdir()
        shutil.copyfile(Path(__file__).with_name("gvisor_security_fixture.py"), context / "fixture.py")
        shutil.copyfile(self.cert, context / "ca.pem")
        observed = subprocess.run(["docker", "image", "inspect", FIXTURE_BASE], capture_output=True, text=True)
        if observed.returncode:
            super().run("docker", "pull", FIXTURE_BASE, timeout=600)
        base = json.loads(super().run("docker", "image", "inspect", FIXTURE_BASE))[0]
        require(FIXTURE_BASE in base["RepoDigests"], "fixture base differs from pinned digest")
        reference = FIXTURE_BASE
        environment = dict(os.environ, GOOS="linux", GOARCH=base["Architecture"], CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-o", str(context / "network-check"),
                        "./tests/kubernetes/gvisor_network_check"], cwd=ROOT, env=environment, check=True, timeout=120)
        subprocess.run(["go", "build", "-trimpath", "-o", str(context / "process-limit"),
                        "./tests/kubernetes/gvisor_process_limit"], cwd=ROOT, env=environment, check=True, timeout=120)
        self.security["production_process_limit"] = {"source_sha256": sha(ROOT / "internal/guesttransport/process_limit.go"),
                                                      "linux_sha256": sha(ROOT / "internal/guesttransport/process_limit_linux.go"),
                                                      "binary_sha256": sha(context / "process-limit")}
        self.security["production_preflight"] = {"source_sha256": sha(ROOT / "internal/guesttransport/network.go"),
                                                "binary_sha256": sha(context / "network-check"),
                                                "original_commit": "3707e25"}
        (context / "Dockerfile").write_text("FROM " + reference + "\nLABEL agentmoor.probe.owner=" + self.cluster + "\nCOPY fixture.py /opt/fixture.py\nCOPY network-check /opt/network-check\nCOPY process-limit /opt/process-limit\nCOPY ca.pem /opt/fixture-ca.pem\nENV PYTHONDONTWRITEBYTECODE=1\nUSER 65532:65532\n")
        log = self.directory / "fixture-build.log"
        try:
            with log.open("w") as output:
                subprocess.run(["docker", "build", "-t", self.image, str(context)], stdout=output, stderr=subprocess.STDOUT, check=True, timeout=180)
        finally:
            self.security["fixture_build_tail"] = log.read_text()[-3000:]
        image = json.loads(super().run("docker", "image", "inspect", self.image))[0]
        self.security["fixture_image"] = {"id": image["Id"], "base_reference": reference, "base_id": base["Id"],
                                          "contains_private_tls_key": False}

    def run(self, *args, data=None, timeout=60, record=None):
        if tuple(args[:3]) == ("kind", "create", "cluster"):
            values = list(args)
            values[values.index("--wait") + 1] = "0s"
            config = {"kind": "Cluster", "apiVersion": "kind.x-k8s.io/v1alpha4", "nodes": [{"role": "control-plane"}],
                      "networking": {"disableDefaultCNI": self.enforcing, "podSubnet": POD_CIDR},
                      "kubeadmConfigPatches": ["kind: KubeletConfiguration\npodPidsLimit: 128\n"]}
            path = self.directory / "kind.json"
            path.write_text(json.dumps(config))
            values.extend(["--config", path])
            result = super().run(*values, data=data, timeout=timeout, record=record)
            owned = json.loads(super().run("docker", "inspect", self.node))[0]
            require(owned["Config"]["Labels"].get("io.x-k8s.kind.cluster") == self.cluster, "CNI target is not owned by this probe")
            before = {"docker_memory": owned["HostConfig"].get("Memory"), "docker_memory_swap": owned["HostConfig"].get("MemorySwap"),
                      "memory.max": self.node_run("cat", "/sys/fs/cgroup/memory.max").strip(),
                      "memory.swap.max": self.node_run("cat", "/sys/fs/cgroup/memory.swap.max").strip()}
            self.security["node_memory"] = {"before": before, "scope": "owned kind Docker node parent cgroup only; host swap untouched"}
            super().run("docker", "update", "--memory", str(NODE_MEMORY_BYTES), "--memory-swap", str(NODE_MEMORY_BYTES), self.node)
            after = {"memory.max": self.node_run("cat", "/sys/fs/cgroup/memory.max").strip(),
                     "memory.swap.max": self.node_run("cat", "/sys/fs/cgroup/memory.swap.max").strip(),
                     "memory.swap.current": self.node_run("cat", "/sys/fs/cgroup/memory.swap.current").strip()}
            self.security["node_memory"]["after"] = after
            require(after["memory.max"] == str(NODE_MEMORY_BYTES) and after["memory.swap.max"] == "0", "owned node parent cgroup did not disable swap")
            nodes = json.loads(super().run(*self.base, "get", "nodes", "-o", "json"))
            require([row["metadata"]["name"] for row in nodes["items"]] == [self.node], "unexpected nodes in CNI install target")
            self.security["kind_config"] = config
            if not self.enforcing:
                super().run(*self.base, "wait", "nodes", "--all", "--for=condition=Ready", "--timeout=120s", timeout=130)
                super().run(*self.base, "-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s", timeout=130)
                self.security["cni"]["actual_pods"] = json.loads(super().run(*self.base, "-n", "kube-system", "get", "pods", "-l", "app=kindnet", "-o", "json"))
                require(self.security["cni"]["actual_pods"]["items"], "negative control kindnet process missing")
                self.load_busybox()
                return result
            ready = next(value for value in nodes["items"][0]["status"]["conditions"] if value["type"] == "Ready")
            require(ready["status"] != "True", "fresh disabled-CNI node unexpectedly ready")
            launched = []
            try:
                guarded_fixture({"cni_ready": False}, lambda: launched.append("not-a-native-cli"))
            except RuntimeError:
                pass
            require(not launched, "pre-CNI preflight launched a fixture")
            self.security["missing_cni_guard"] = {"scope": "actual NodeNotReady plus offline validator only; see kindnet-negative for real production-helper rejection", "node_ready": ready, "fixture_executions": 0, "vendor_cli_executions": 0}
            architecture = json.loads(super().run("docker", "version", "--format", "{{json .Server}}"))["Arch"]
            for index, row in enumerate(self.security["cni"]["images"]):
                archive = self.directory / ("cni-" + str(index) + ".tar")
                super().run("docker", "image", "save", "--output", archive, row["version_reference"], timeout=180)
                row["loaded_archive_sha256"] = sha(archive)
                super().run("docker", "cp", archive, self.node + ":/opt/agentmoor-cni.tar", timeout=180)
                copied = super().run("docker", "exec", self.node, "sha256sum", "/opt/agentmoor-cni.tar").split()[0]
                require(copied == row["loaded_archive_sha256"], "CNI archive did not reach the owned node intact")
                # kind's --all-platforms import tries absent foreign-platform
                # blobs from Docker's local single-platform pull. Import only
                # this owned Linux node's actual platform, retaining official
                # manifest/digest identity; no images are rebuilt/substituted.
                super().run("docker", "exec", self.node, "ctr", "--namespace=k8s.io", "images", "import", "--digests", "--platform", "linux/" + architecture,
                            "--snapshotter=overlayfs", "/opt/agentmoor-cni.tar", timeout=180)
                super().run("docker", "exec", self.node, "rm", "/opt/agentmoor-cni.tar")
                archive.unlink()
            self.stage("install_calico_only_into_owned_cluster")
            super().run(*self.base, "create", "-f", self.frozen_calico, timeout=120, record="calico_install")
            super().run(*self.base, "-n", "kube-system", "rollout", "status", "ds/calico-node", "--timeout=240s", timeout=250)
            super().run(*self.base, "wait", "nodes", "--all", "--for=condition=Ready", "--timeout=120s", timeout=130)
            super().run(*self.base, "-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s", timeout=130)
            actual = json.loads(super().run(*self.base, "-n", "kube-system", "get", "pods", "-l", "k8s-app=calico-node", "-o", "json"))
            require(actual["items"] and all(row.get("status", {}).get("phase") == "Running" for row in actual["items"]), "Calico is not actually running")
            self.security["cni"]["actual_pods"] = actual
            self.load_busybox()
            return result
        return super().run(*args, data=data, timeout=timeout, record=record)

    def load_busybox(self):
        archive = self.directory / "busybox.tar"
        image = json.loads(super().run("docker", "image", "inspect", IMAGE))[0]
        require(any(reference.endswith("@sha256:" + IMAGE.rsplit("@sha256:", 1)[1]) for reference in image["RepoDigests"]), "base runtime probe image differs")
        super().run("docker", "image", "save", "--output", archive, IMAGE, timeout=120)
        digest = sha(archive)
        super().run("docker", "cp", archive, self.node + ":/opt/agentmoor-busybox.tar", timeout=120)
        require(super().run("docker", "exec", self.node, "sha256sum", "/opt/agentmoor-busybox.tar").split()[0] == digest, "runtime probe image archive changed")
        super().run("docker", "exec", self.node, "ctr", "--namespace=k8s.io", "images", "import", "--digests", "--platform", "linux/" + image["Architecture"],
                    "--snapshotter=overlayfs", "/opt/agentmoor-busybox.tar", timeout=120)
        super().run("docker", "exec", self.node, "rm", "/opt/agentmoor-busybox.tar")
        archive.unlink()
        self.security["runtime_probe_image"] = {"reference": IMAGE, "architecture": image["Architecture"], "archive_sha256": digest, "image_id": image["Id"]}

    def kube(self, *args):
        raw = super().kube(*args)
        if tuple(args) == ("get", "events", "-o", "json"):
            # Preloading the positive image can make it run before kubelet's
            # second missing-runtime retry. Wait for actual repeated evidence
            # instead of weakening the shared runtime proof's count check.
            started = time.monotonic()
            until = started + 35
            while time.monotonic() < until:
                events = json.loads(raw)
                if any(event.get("reason") == "FailedCreatePodSandBox" and
                       "agentmoor-nonexistent-handler" in event.get("message", "") and event.get("count", 0) >= 2
                       for event in events["items"]):
                    self.security["missing_handler_extra_observation_seconds"] = time.monotonic() - started
                    break
                time.sleep(.5)
                raw = super().kube(*args)
        return raw

    def apply(self, value):
        return self.run(*self.base, "apply", "-f", "-", data=json.dumps(value))

    def wait_ready(self, namespace, name):
        return self.run(*self.base, "-n", namespace, "wait", "pod/" + name, "--for=condition=Ready", "--timeout=90s", timeout=100)

    def pod(self, namespace, name, command, labels=None, memory="256Mi", deadline=60, runtime=True):
        spec = {"restartPolicy": "Never", "terminationGracePeriodSeconds": 0, "activeDeadlineSeconds": deadline,
                "automountServiceAccountToken": False, "enableServiceLinks": False,
                "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532,
                                    "seccompProfile": {"type": "RuntimeDefault"}},
                "containers": [{"name": "fixture", "image": self.image, "imagePullPolicy": "Never", "command": command,
                                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                                "resources": {"limits": {"cpu": "200m", "memory": memory, "ephemeral-storage": "64Mi"},
                                              "requests": {"cpu": "200m", "memory": memory}},
                                "volumeMounts": [{"name": "work", "mountPath": "/run/work"}, {"name": "scratch", "mountPath": "/tmp"}]}],
                "volumes": [{"name": "work", "emptyDir": {"medium": "Memory", "sizeLimit": "4Mi"}},
                            {"name": "scratch", "emptyDir": {"medium": "Memory", "sizeLimit": "512Mi"}}]}
        if runtime:
            spec["runtimeClassName"] = "agentmoor-gvisor"
        return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": namespace, "labels": labels or {}}, "spec": spec}

    def runtime(self, namespace, name):
        pod = json.loads(self.run(*self.base, "-n", namespace, "get", "pod", name, "-o", "json"))
        cri = json.loads(self.node_run("crictl", "pods", "--namespace", namespace, "-o", "json"))
        sandbox, = [row for row in cri["items"] if row["metadata"]["uid"] == pod["metadata"]["uid"]]
        inspect = json.loads(self.node_run("crictl", "inspectp", sandbox["id"]))
        require(inspect["status"].get("runtimeHandler") == HANDLER and inspect.get("info", {}).get("runtimeType") == RUNTIME_TYPE, "fixture has no actual gVisor runtime")
        processes = self.node_run("ps", "-eo", "pid,comm,args")
        sentry, = [row for row in processes.splitlines() if "runsc-sandbox " in row and sandbox["id"] in row and "--platform=systrap" in row]
        require("--overlay2=all:memory,size=512m" in sentry, "fixture has no assigned Sentry tmpfs overlays")
        return {"pod_uid": pod["metadata"]["uid"], "sandbox_id": sandbox["id"], "sentry_pid": int(sentry.split()[0]),
                "sentry_process": sentry, "cri": inspect}

    def setup(self):
        self.execute()
        self.result["status"] = "runtime_feasibility_passed"
        self.result["proof"]["negative_observation_seconds"] += self.security.get("missing_handler_extra_observation_seconds", 0)
        config = self.node_run("cat", "/etc/containerd/runsc.toml") + '  overlay2 = "all:memory,size=512m"\n'
        path = self.directory / "guest-runsc.toml"
        path.write_text(config)
        self.run("docker", "cp", path, self.node + ":/etc/containerd/runsc.toml")
        self.security["runsc_config"] = config
        kubelet = self.node_run("cat", "/var/lib/kubelet/config.yaml")
        require(re.search(r"(?m)^podPidsLimit:\s*128\s*$", kubelet), "actual kubelet PodPidsLimit differs")
        self.security["kubelet_config"] = kubelet
        self.run("kind", "load", "docker-image", self.image, "--name", self.cluster, timeout=180)
        for namespace in (GUEST_NS, CONTROL_NS, TENANT_NS):
            self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": namespace}})
        self.run(*self.base, "-n", CONTROL_NS, "create", "secret", "tls", "fixture-tls", "--cert", self.cert, "--key", self.key)

    def network(self):
        self.stage("prepare_reachable_network_fixtures")
        gateway = self.pod(CONTROL_NS, "gateway", ["python3", "/opt/fixture.py", "serve", "--ports", "7443", "7401", "--nonce", self.nonce], GATEWAY_LABEL, runtime=False, deadline=240)
        gateway["spec"]["volumes"].append({"name": "tls", "secret": {"secretName": "fixture-tls"}})
        gateway["spec"]["containers"][0]["volumeMounts"].append({"name": "tls", "mountPath": "/run/tls", "readOnly": True})
        self.apply(gateway)
        service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "gateway", "namespace": CONTROL_NS},
                   "spec": {"selector": GATEWAY_LABEL, "ports": [{"name": "gateway", "port": 7443}, {"name": "main-api", "port": 7401}]}}
        self.apply(service)
        tenant = self.pod(TENANT_NS, "tenant", ["python3", "/opt/fixture.py", "serve", "--ports", "8123", "--nonce", self.nonce], runtime=False, deadline=240)
        self.apply(tenant)
        self.wait_ready(CONTROL_NS, "gateway")
        self.wait_ready(TENANT_NS, "tenant")
        gateway_service = json.loads(self.run(*self.base, "-n", CONTROL_NS, "get", "svc", "gateway", "-o", "json"))
        gateway_ip = gateway_service["spec"]["clusterIP"]
        tenant_ip = json.loads(self.run(*self.base, "-n", TENANT_NS, "get", "pod", "tenant", "-o", "json"))["status"]["podIP"]
        api_ip = json.loads(self.run(*self.base, "-n", "default", "get", "svc", "kubernetes", "-o", "json"))["spec"]["clusterIP"]
        dns_ip = json.loads(self.run(*self.base, "-n", "kube-system", "get", "svc", "kube-dns", "-o", "json"))["spec"]["clusterIP"]
        self.run("docker", "run", "-d", "--name", self.outside, "--network", "kind", "--label", "agentmoor.probe.owner=" + self.cluster, self.image, "python3", "/opt/fixture.py", "serve", "--ports", "8123", "--nonce", self.nonce)
        outside = json.loads(self.run("docker", "inspect", self.outside))[0]
        outside_ip = outside["NetworkSettings"]["Networks"]["kind"]["IPAddress"]
        docker_network = json.loads(self.run("docker", "network", "inspect", "kind"))[0]
        require(all(not ipaddress.ip_network(POD_CIDR).overlaps(ipaddress.ip_network(row["Subnet"]))
                    for row in docker_network["IPAM"]["Config"] if ":" not in row["Subnet"]), "Pod CIDR overlaps owned outside fixture network")
        self.security["network"]["outside_network"] = docker_network
        require(outside_ip and outside["Config"]["Image"] == self.image, "outside server is not the owned fixture")
        # This DNAT is inside the owned Docker kind node network namespace,
        # never the macOS/Linux host. It sends the simulated link-local request
        # only to our owned reachable outside fixture, never real metadata.
        self.node_run("iptables", "-t", "nat", "-A", "PREROUTING", "-d", "169.254.169.254/32", "-p", "tcp", "--dport", "80",
                      "-m", "comment", "--comment", self.cluster, "-j", "DNAT", "--to-destination", outside_ip + ":8123")
        self.security["metadata_simulation"] = {"request": "169.254.169.254:80", "target": outside_ip + ":8123", "network_namespace": self.node, "real_cloud_metadata_contacted": False}
        gateway_pod = json.loads(self.run(*self.base, "-n", CONTROL_NS, "get", "pod", "gateway", "-o", "json"))
        gateway_log = json.loads(self.run(*self.base, "-n", CONTROL_NS, "logs", "gateway").splitlines()[0])
        endpoints = json.loads(self.run(*self.base, "-n", CONTROL_NS, "get", "endpointslices", "-l", "kubernetes.io/service-name=gateway", "-o", "json"))
        require(gateway_log["server_ports"] == [7443, 7401] and gateway_log["fixture"] == self.nonce, "gateway ports do not share one fixture process")
        require(gateway_service["spec"]["selector"] == GATEWAY_LABEL and {port["port"] for port in gateway_service["spec"]["ports"]} == {7443, 7401}, "dual-port gateway Service differs")
        require(endpoints["items"] and all(endpoint.get("targetRef", {}).get("uid") == gateway_pod["metadata"]["uid"]
                    for item in endpoints["items"] for endpoint in item["endpoints"]), "gateway Service does not select only the dual-port Pod")
        self.security["network"]["gateway_fixture"] = {"pod": gateway_pod, "service": gateway_service, "endpoints": endpoints, "log": gateway_log}
        targets = [{"name": "gateway-tls", "ip": gateway_ip, "port": 7443, "tls": True},
                   {"name": "main-api", "ip": gateway_ip, "port": 7401, "http": True},
                   {"name": "kubernetes-api", "ip": api_ip, "port": 443},
                   {"name": "other-tenant", "ip": tenant_ip, "port": 8123, "http": True},
                   {"name": "metadata-fixture", "ip": "169.254.169.254", "port": 80, "http": True},
                   {"name": "outside-cidr", "ip": outside_ip, "port": 8123, "http": True},
                   {"name": "dns-tcp", "ip": dns_ip, "port": 53},
                   {"name": "dns-udp", "ip": dns_ip, "port": 53, "protocol": "UDP"}]
        for name, labels in (("selected-guest", GUEST_LABEL), ("unselected-control", {"app.kubernetes.io/name": "network-control"})):
            pod = self.pod(GUEST_NS, name, ["sleep", "240"], labels, deadline=240)
            pod["spec"]["hostAliases"] = [{"ip": gateway_ip, "hostnames": ["gateway.fixture"]}]
            self.apply(pod)
            self.wait_ready(GUEST_NS, name)
            self.security.setdefault("network_runtimes", {})[name] = self.runtime(GUEST_NS, name)
        def observe(name):
            raw = self.run(*self.base, "-n", GUEST_NS, "exec", name, "--", "python3", "/opt/fixture.py", "network", "--targets", json.dumps(targets), timeout=30)
            return json.loads(raw)
        for _ in range(4):
            baseline = observe("selected-guest")
            self.security["network"]["no_policy_baseline"] = baseline
            if all(row["reachable"] for row in baseline["targets"]):
                break
            time.sleep(.5)
        require(all(row["reachable"] for row in baseline["targets"]), "no-policy network baseline is not actually reachable")
        for name in ("gateway-tls", "main-api"):
            response = next(row for row in baseline["targets"] if row["name"] == name)["response"]
            require(response["fixture"] == self.nonce and response["process_id"] == gateway_log["process_id"], "dual-port actual traffic did not reach the same process")
        self.apply(network_policy())
        if not self.enforcing:
            self.stage("reject_kindnet_with_reachable_denied_canary")
            time.sleep(2)
            observed = observe("selected-guest")
            self.security["network"]["unenforced_policy"] = observed
            require(next(row for row in observed["targets"] if row["name"] == "gateway-tls")["reachable"] and
                    any(row["reachable"] for row in observed["targets"] if row["name"] in ("main-api", "kubernetes-api")),
                    "default CNI negative control has no reachable required-deny canary")
            self.security["cni"]["required_canary_enforcement"] = "insufficient: at least one required-deny canary is reachable"
            denied = self.production_guard(gateway_ip, api_ip)
            self.security["network"]["production_guard_refused"] = denied
            require(denied["returncode"] != 0 and denied["result"].get("allowed") is False, "incomplete default CNI isolation passed production preflight")
            self.run(*self.base, "-n", GUEST_NS, "exec", "selected-guest", "--", "sh", "-c", "test ! -e /run/work/preflight-count")
            self.security["network"].update(fixture_executions=0, vendor_cli_executions=0)
            return
        self.stage("measure_actual_selected_and_control_traffic")
        observation = None
        for _ in range(4):
            guest, control = observe("selected-guest"), observe("unselected-control")
            observation = {"cni_ready": True, "gateway_fixture": self.security["network"]["gateway_fixture"],
                           "cni_pods": self.security["cni"]["actual_pods"],
                           "rows": [{"name": row["name"], "guest": row, "control": control["targets"][index]} for index, row in enumerate(guest["targets"])]}
            self.security["network"]["enforced"] = observation
            try:
                verify_network(observation)
                break
            except RuntimeError:
                time.sleep(1)
        verify_network(observation)
        self.security["network"]["enforced"] = observation
        allowed = self.production_guard(gateway_ip, api_ip)
        self.security["network"]["production_guard_allowed"] = allowed
        require(allowed["returncode"] == 0 and allowed["result"].get("fixture_executions") == 1, "production preflight rejected enforced network")
        self.security["network"]["production_guard_allowed"] = allowed
        self.run(*self.base, "-n", GUEST_NS, "delete", "networkpolicy", "gateway-only", "--wait=true")
        for _ in range(6):
            restored = observe("selected-guest")
            if all(row["reachable"] for row in restored["targets"]):
                break
            time.sleep(.5)
        require(all(row["reachable"] for row in restored["targets"]), "policy removal did not restore reachable canaries")
        denied = self.production_guard(gateway_ip, api_ip)
        self.security["network"]["production_guard_refused"] = denied
        require(denied["returncode"] != 0 and denied["result"].get("allowed") is False, "missing policy passed production preflight")
        counter = self.run(*self.base, "-n", GUEST_NS, "exec", "selected-guest", "--", "cat", "/run/work/preflight-count").strip()
        require(counter == "1", "missing policy started another fixture execution")
        self.security["network"].update(policy_removed=restored, production_guard_refused=denied, denied_launch_delta=0,
                                       vendor_cli_executions=0, gateway_service=gateway_service)
        self.apply(network_policy())

    def production_guard(self, gateway_ip, api_ip):
        command = [*self.base, "-n", GUEST_NS, "exec", "selected-guest", "--", "/opt/network-check", "--gateway", "https://gateway.fixture:7443",
                   "--ca", "/opt/fixture-ca.pem", "--api", gateway_ip + ":7401", "--kube", api_ip + ":443"]
        result = subprocess.run(command, capture_output=True, text=True, timeout=10)
        return {"returncode": result.returncode, "result": json.loads(result.stdout), "stderr": result.stderr[-1000:]}

    def cgroup_paths(self, runtime):
        raw = self.node_run("cat", "/proc/" + str(runtime["sentry_pid"]) + "/cgroup")
        relative, = [row[3:] for row in raw.splitlines() if row.startswith("0::")]
        path = PurePosixPath(relative)
        marker = "pod" + runtime["pod_uid"].replace("-", "_")
        require(path.is_absolute() and ".." not in path.parts and any(marker in part for part in path.parts),
                "Sentry cgroup does not belong to the assigned Pod")
        paths = []
        while True:
            paths.append("/sys/fs/cgroup" + str(path))
            if marker in path.name:
                break
            path = path.parent
        return paths

    def cgroups(self, paths):
        command = 'for dir do printf "CGROUP %s\\n" "$dir"; for file in cpu.max cpu.stat memory.max memory.current memory.events memory.swap.max memory.swap.current memory.swap.events pids.max pids.current pids.events; do if [ -f "$dir/$file" ]; then printf "FILE %s\\n" "$file"; cat "$dir/$file"; printf "END\\n"; fi; done; done'
        raw = self.node_run("sh", "-c", command, "cgroups", *paths)
        rows, current, file, content = [], None, None, []
        for line in raw.splitlines():
            if line.startswith("CGROUP "):
                current = {"path": line[7:]}
                rows.append(current)
            elif line.startswith("FILE "):
                file, content = line[5:], []
            elif line == "END":
                current[file] = "\n".join(content)
            else:
                content.append(line)
        return rows

    def remove_pod(self, namespace, name, runtime, record):
        self.run(*self.base, "-n", namespace, "delete", "pod", name, "--wait=true", "--timeout=60s", timeout=70)
        for until in [time.monotonic() + 60]:
            while time.monotonic() < until:
                processes = self.node_run("ps", "-eo", "pid,stat,comm,args")
                live = [line for line in processes.splitlines() if runtime["sandbox_id"] in line and any(value in line for value in ("runsc-sandbox", "runsc-gofer", "containerd-shim-runsc"))]
                cri = json.loads(self.node_run("crictl", "pods", "--namespace", namespace, "-o", "json"))
                retained = [row for row in cri["items"] if row["metadata"]["uid"] == runtime["pod_uid"]]
                record["cleanup_last_observation"] = {"live_processes": live, "cri_sandboxes": retained}
                if not live:
                    # Kubelet may retain a stopped sandbox until its normal
                    # periodic GC. Never kill a live sandbox to manufacture a
                    # cancellation pass; only remove already-stopped records
                    # belonging to the deleted, attested Pod UID.
                    if retained and all(row.get("state") == "SANDBOX_NOTREADY" for row in retained):
                        for row in retained:
                            self.node_run("crictl", "rmp", row["id"])
                        record.setdefault("stopped_cri_records_removed", []).extend(retained)
                    if not retained:
                        return {"sentry_processes": [], "cri_sandboxes": [], "pod_uid": runtime["pod_uid"]}
                time.sleep(.2)
        diagnostics = {}
        for line in record.get("cleanup_last_observation", {}).get("live_processes", []):
            pid = line.split()[0]
            if pid.isdigit():
                for file in ("status", "wchan", "stack"):
                    try:
                        diagnostics[pid + "/" + file] = self.node_run("cat", "/proc/" + pid + "/" + file)
                    except Exception as error:
                        diagnostics[pid + "/" + file] = str(error)
        record["cleanup_process_diagnostics"] = diagnostics
        raise RuntimeError("deleted Pod left a live Sentry/gofer/shim or CRI sandbox")

    def resources(self):
        self.stage("run_bounded_resource_fixtures")
        failures = []
        for mode in ("cpu", "memory", "tmpfs", "file-memory", "pids", "wall", "cancel"):
            self.stage("resource_" + mode)
            try:
                self.resource(mode)
            except Exception as error:
                record = self.security["resources"].setdefault(mode, {})
                record["failure"] = {"type": type(error).__name__, "message": str(error)}
                failures.append(mode)
        require(not failures, "resource proofs failed: " + ", ".join(failures))

    def resource(self, mode):
        name = "resource-" + mode
        memory = "256Mi" if mode in ("tmpfs", "pids") else "128Mi"
        deadline = 8 if mode == "wall" else 30
        manifest = self.pod(GUEST_NS, name, ["/opt/process-limit", "python3", "/opt/fixture.py", "workload", "--mode", mode], GUEST_LABEL,
                            memory=memory, deadline=deadline)
        self.apply(manifest)
        self.wait_ready(GUEST_NS, name)
        runtime = self.runtime(GUEST_NS, name)
        paths = self.cgroup_paths(runtime)
        before = self.cgroups(paths)
        pod = json.loads(self.run(*self.base, "-n", GUEST_NS, "get", "pod", name, "-o", "json"))
        record = {"runtime_verified": True, "node_swap_max": self.security["node_memory"]["after"]["memory.swap.max"], "runtime": runtime, "manifest": manifest, "before": before,
                  "pod_creation": pod["metadata"]["creationTimestamp"], "pod_start_time": pod["status"]["startTime"], "original_active_deadline_seconds": deadline,
                  "observations": [], "terminated": False}
        self.security["resources"][mode] = record
        require(any(row.get("pids.max") == "128" for row in before), "resource Pod lacks actual host PID bound before load")
        self.run(*self.base, "-n", GUEST_NS, "exec", name, "--", "touch", "/run/work/start")
        for until in [time.monotonic() + 35]:
            while time.monotonic() < until:
                snapshot = self.cgroups(paths)
                record["observations"].append(snapshot)
                log = self.run(*self.base, "-n", GUEST_NS, "logs", name)
                events = [json.loads(line) for line in log.splitlines() if line.startswith("{")]
                finished = next((row for row in events if row.get("stage") == "finished"), None)
                current = json.loads(self.run(*self.base, "-n", GUEST_NS, "get", "pod", name, "-o", "json"))
                require(current["metadata"]["uid"] == runtime["pod_uid"] and current["metadata"]["creationTimestamp"] == record["pod_creation"] and current["spec"]["activeDeadlineSeconds"] == deadline and current["status"]["startTime"] == record["pod_start_time"],
                        "poll/retry replaced the Pod or refreshed its original deadline")
                statuses = current.get("status", {}).get("containerStatuses", [])
                record["terminated"] = bool(statuses) and all("terminated" in row["state"] for row in statuses)
                record["pod_reason"] = current.get("status", {}).get("reason")
                record["pod_status"] = current.get("status")
                record["log"] = log
                if mode == "cancel" and any(row.get("stage") == "started" for row in events):
                    record["cancelled"] = True
                    break
                if finished or record["terminated"]:
                    if finished:
                        record["fixture"] = finished
                    break
                time.sleep(.15)
            else:
                raise TimeoutError("bounded resource fixture did not reach its expected state")
        record["after"] = self.cgroups(paths)
        all_samples = record["observations"] + [record["after"]]
        def delta(file, key):
            baseline = {row["path"]: parse_counter(row.get(file, "")).get(key, 0) for row in before}
            return max([0] + [parse_counter(row.get(file, "")).get(key, 0) - baseline.get(row["path"], 0)
                              for sample in all_samples for row in sample])
        record["cpu_throttled_delta"] = delta("cpu.stat", "nr_throttled")
        record["cpu_usage_usec_delta"] = delta("cpu.stat", "usage_usec")
        record["oom_kill_delta"] = delta("memory.events", "oom_kill")
        record["host_pids_max_events_delta"] = delta("pids.events", "max")
        record["cleanup"] = self.remove_pod(GUEST_NS, name, runtime, record)
        record["physical_cleanup"] = True
        verify_resource(mode, record)

    def clean_security(self):
        observed = subprocess.run(["docker", "inspect", self.outside], capture_output=True, text=True)
        if observed.returncode == 0:
            owned = json.loads(observed.stdout)[0]
            require(owned["Config"]["Labels"].get("agentmoor.probe.owner") == self.cluster, "outside cleanup target is not owned")
            self.run("docker", "rm", "--force", self.outside)
        self.cleanup()
        observed = subprocess.run(["docker", "image", "inspect", self.image], capture_output=True, text=True)
        if observed.returncode == 0:
            owned = json.loads(observed.stdout)[0]
            require(owned["Config"]["Labels"].get("agentmoor.probe.owner") == self.cluster, "image cleanup target is not owned")
            self.run("docker", "image", "rm", self.image)
        containers = self.run("docker", "ps", "-a", "--filter", "label=agentmoor.probe.owner=" + self.cluster, "--format", "{{.Names}}").strip()
        images = self.run("docker", "image", "ls", "--filter", "reference=" + self.image, "--format", "{{.ID}}").strip()
        require(not containers and not images, "owned outside fixture or image tag remains after cleanup")
        self.security["cleanup"] = {"owned_node_removed": True, "owned_outside_fixture_removed": True, "owned_image_tag_removed": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path, default=os.environ.get("GVISOR_SECURITY_BUNDLE"))
    parser.add_argument("--calico-manifest", type=Path, default=os.environ.get("GVISOR_SECURITY_CALICO_MANIFEST"))
    parser.add_argument("--stage", choices=("bootstrap", "network", "resources", "kindnet-negative", "all"), default="all")
    parser.add_argument("--output", type=Path, default=os.environ.get("GVISOR_SECURITY_OUTPUT"))
    options = parser.parse_args()
    if options.output:
        require(not os.path.lexists(options.output) and options.output.parent.is_dir(), "evidence path must be new")
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    failure = None
    with tempfile.TemporaryDirectory(prefix="agentmoor-gvisor-security-") as directory:
        probe = SecurityProbe(directory, options.bundle, options.calico_manifest, enforcing=options.stage != "kindnet-negative")
        probe.security["started_at_utc"] = datetime.now(timezone.utc).isoformat()
        probe.security["source"] = {"commit": probe.run("git", "-C", ROOT, "rev-parse", "HEAD").strip(),
                                    "status": probe.run("git", "-C", ROOT, "status", "--short"),
                                    "script_sha256": sha(__file__), "fixture_sha256": sha(Path(__file__).with_name("gvisor_security_fixture.py"))}
        try:
            probe.prepare()
            probe.setup()
            if options.stage in ("network", "kindnet-negative", "all"):
                probe.network()
            if options.stage in ("resources", "all"):
                probe.resources()
        except BaseException as error:
            failure = error
            probe.security["failure"] = {"type": type(error).__name__, "message": str(error)}
            if probe.created:
                for key, args in (("pods", ("get", "pods", "-A", "-o", "json")), ("events", ("get", "events", "-A", "-o", "json"))):
                    try:
                        probe.security[key] = json.loads(probe.run(*probe.base, *args))
                    except Exception as diagnostic:
                        probe.security[key] = str(diagnostic)
                probe.security["fixture_logs"] = {}
                for namespace, name in ((CONTROL_NS, "gateway"), (TENANT_NS, "tenant"), *((GUEST_NS, "resource-" + mode) for mode in ("cpu", "memory", "tmpfs", "file-memory", "pids", "wall", "cancel"))):
                    diagnostic = subprocess.run([*probe.base, "-n", namespace, "logs", name], capture_output=True, text=True, timeout=10)
                    if diagnostic.returncode == 0:
                        probe.security["fixture_logs"][namespace + "/" + name] = diagnostic.stdout[-6000:]
        finally:
            try:
                probe.clean_security()
            except Exception as error:
                failure = failure or error
                probe.security["cleanup_failure"] = str(error)
            probe.security["runtime_probe"] = probe.result
            probe.security["finished_at_utc"] = datetime.now(timezone.utc).isoformat()
            probe.security["status"] = "failed" if failure else options.stage + "_passed"
            if options.output:
                options.output.write_text(json.dumps(probe.security, indent=2) + "\n")
            print("GVISOR_SECURITY_RESULT " + json.dumps({key: probe.security.get(key) for key in ("status", "failure")}), flush=True)
    return int(failure is not None)


if __name__ == "__main__":
    raise SystemExit(main())

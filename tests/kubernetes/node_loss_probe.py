"""Negative lifecycle probe: API deletion is not physical sandbox termination.

Only a bounded synthetic heartbeat child runs; native_vendor is always false.
The real production Catalog/Controller execute in the trusted host test driver.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import tempfile
import time

from gvisor_probe import IMAGE, NAMESPACE, require
from gvisor_security_probe import SecurityProbe
from sandbox_controller_smoke import ControllerSmoke, ROOT, redact_pod, safe_evidence

EVENT = "SANDBOX_NODE_LOSS_EVENT "
HEARTBEAT = "AGENTMOOR_NODE_LOSS_HEARTBEAT "


def now():
    return datetime.now(timezone.utc).isoformat()


def verify_orphan_window(row):
    """Prove one real child continued after API404, independently of kubelet."""
    require(row["metric"]["api_not_found"] is True, "API404 was not observed")
    require(row["kubelet"]["after"]["ActiveState"] == "inactive" and row["kubelet"]["after"]["MainPID"] == "0", "kubelet was not stopped")
    require(row["kubelet"]["containerd_before"] == row["kubelet"]["containerd_after"] and row["kubelet"]["containerd_after"]["ActiveState"] == "active", "containerd stopped or restarted")
    require(row["kubelet"]["api_ready_after_stop"] == "ok", "API did not survive kubelet stop")
    before, after = row["post_api404_samples"][0], row["post_api404_samples"][-1]
    for sample in (before, after):
        require(sample["same_sentry"] and sample["sandbox_ready"] and sample["container_running"], "same physical sandbox did not remain live")
    a, b = before["heartbeat"], after["heartbeat"]
    require(a["nonce"] == b["nonce"] and a["pid"] == b["pid"] and a["uid"] == b["uid"] == 65532, "heartbeat switched processes")
    require(b["sequence"] - a["sequence"] >= 3 and b["elapsed_nano"] > a["elapsed_nano"], "fewer than three fresh post-404 heartbeats")
    fresh = [item for item in after["recent_heartbeats"] if item["sequence"] > a["sequence"] and item["wall_unix_nano"] > before["node_unix_nano"]]
    require(len(fresh) >= 3 and all(item["nonce"] == a["nonce"] and item["pid"] == a["pid"] for item in fresh), "fewer than three child emissions after the node's post-404 clock sample")
    metric = row["metric"]
    require(metric["capabilities_denied"] and metric["checkpoint_unchanged"] and metric["identity_deadline_unchanged"], "authority/checkpoint/identity/deadline changed")
    return True


def verify_physical_cleanup_claim(row):
    verify_orphan_window(row)
    require(not row["metric"]["pod_deleted"], "PHYSICAL_CLEANUP_INVARIANT: API404 was declared physical cleanup while the same child/Sentry remained live")
    return True


class NodeLossSmoke(ControllerSmoke):
    def __init__(self, probe):
        super().__init__(probe)
        self.stopped = False
        self.cordoned = False
        self.active_runtime = None
        self.result.update(scope="node-loss physical cleanup invariant, component only", native_vendor=False,
            scenarios={}, expected_negative_reproduced=False,
            limitations=["Synthetic Go child only; no vendor CLI/model/SaaS acceptance.",
                "Single owned kind node; stopping kubelet is a partition surrogate, not cloud power fencing or HA acceptance.",
                "Host driver uses the production Catalog/Controller and a synthetic input TLS handler, not the production gateway.",
                "Calico is installed, but this probe does not claim the complete egress/resource matrix."])

    def configure(self):
        actual = self.probe.node_run("cat", "/etc/containerd/runsc.toml")
        require(actual == self.probe.security["runsc_config"] and 'overlay2 = "all:memory,size=512m"' in actual, "actual fixed runtime configuration differs")
        require(" v2.4.2 " in self.probe.node_run("containerd", "--version"), "official fixed containerd candidate missing")
        self.result["runtime_config"] = actual

    def prepare(self):
        relevant = ["cmd", "internal", "tests/kubernetes/controllerfixture", "tests/kubernetes/node_loss_probe.py", "tests/kubernetes/node_loss_probe_test.py"]
        require(not self.probe.run("git", "-C", ROOT, "status", "--short", "--", *relevant).strip(), "commit all node-loss source before acceptance")
        super().prepare()
        files = [Path(__file__), ROOT / "internal/sandboxkube/node_loss_kubernetes_test.go", ROOT / "tests/kubernetes/controllerfixture/main.go"]
        self.result["fixture_source_sha256"] = {str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest() for path in files}

    def signal_file(self, name):
        path = self.probe.directory / name
        with path.open("x") as stream:
            stream.write("observed\n")

    def owned(self):
        row, = json.loads(self.probe.run("docker", "inspect", self.probe.node))
        require(row["Config"]["Labels"].get("io.x-k8s.kind.cluster") == self.probe.cluster, "node fault target is not owned")

    def service(self, name):
        raw = self.probe.node_run("systemctl", "show", name, "-p", "ActiveState", "-p", "SubState", "-p", "MainPID")
        return dict(line.split("=", 1) for line in raw.splitlines() if "=" in line)

    def heartbeats(self, runtime):
        raw = self.probe.node_run("tail", "-n", "32", runtime["log_path"])
        rows = []
        for line in raw.splitlines():
            if HEARTBEAT in line:
                row = json.loads(line.split(HEARTBEAT, 1)[1])
                require(set(row) == {"nonce", "pid", "sequence", "elapsed_nano", "wall_unix_nano", "uid"} and re.fullmatch(r"[a-f0-9]{32}", row["nonce"]) and row["pid"] >= 2 and row["sequence"] > 0, "invalid bounded heartbeat record")
                rows.append(row)
        return rows

    def heartbeat(self, runtime):
        rows = self.heartbeats(runtime)
        return rows[-1] if rows else None

    def process_start(self, pid):
        raw = self.probe.node_run("cat", "/proc/" + str(pid) + "/stat")
        return raw.rsplit(")", 1)[1].split()[19]

    def snapshot(self, runtime):
        node_time = int(self.probe.node_run("date", "+%s%N").strip())
        sandbox = json.loads(self.probe.node_run("crictl", "inspectp", runtime["sandbox_id"]))
        container = json.loads(self.probe.node_run("crictl", "inspect", runtime["container_id"]))
        heartbeats = self.heartbeats(runtime)
        return {"observed_at_utc": now(), "observed_monotonic": time.monotonic(), "node_unix_nano": node_time,
                "same_sentry": self.process_start(runtime["sentry_pid"]) == runtime["sentry_start_ticks"],
                "sandbox_ready": sandbox["status"]["id"] == runtime["sandbox_id"] and sandbox["status"]["state"] == "SANDBOX_READY",
                "container_running": container["status"]["id"] == runtime["container_id"] and container["status"]["state"] == "CONTAINER_RUNNING",
                "heartbeat": heartbeats[-1] if heartbeats else None, "recent_heartbeats": heartbeats[-8:]}

    def wait_heartbeat(self, runtime, minimum=3, baseline=None, not_before_unix_nano=None):
        for until in [time.monotonic() + 15]:
            while time.monotonic() < until:
                rows = self.heartbeats(runtime)
                row = rows[-1] if rows else None
                if not_before_unix_nano is not None:
                    fresh = [item for item in rows if item["sequence"] > baseline["sequence"] and item["wall_unix_nano"] > not_before_unix_nano]
                    if len(fresh) >= minimum:
                        return fresh[-1]
                    time.sleep(.15)
                    continue
                if row and row["sequence"] >= minimum and (baseline is None or row["sequence"] >= baseline["sequence"] + minimum):
                    return row
                time.sleep(.15)
        raise TimeoutError("bounded child did not emit required fresh heartbeats")

    def observe_guest(self, metric):
        mode = metric["mode"]
        pod = json.loads(self.probe.kube("get", "pod", metric["pod_name"], "-o", "json"))
        require(pod["metadata"]["uid"] == metric["pod_uid"], "wrong Pod identity")
        if mode == "pending":
            require(pod["status"]["phase"] == "Pending" and not pod["spec"].get("nodeName"), "Pending control was already assigned")
            rows = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))["items"]
            require(not any(row["metadata"]["uid"] == metric["pod_uid"] for row in rows), "unscheduled control created a CRI sandbox")
            self.result["scenarios"][mode] = {"pod": redact_pod(pod), "before_cancel": {"node_name_empty": True, "no_cri_sandbox": True}}
            self.signal_file(mode + ".observation-ready")
            return
        require(pod["status"]["phase"] == "Running", "wrong running Pod phase")
        runtime = self.probe.runtime(NAMESPACE, metric["pod_name"])
        # Whitelist runtime evidence. Raw CRI metadata can contain delegated
        # Pod annotations and must never be copied into the public artifact.
        safe = {key: runtime[key] for key in ("pod_uid", "sandbox_id", "sentry_pid", "sentry_process")}
        safe.update(runtime_handler=runtime["cri"]["status"].get("runtimeHandler"), runtime_type=runtime["cri"].get("info", {}).get("runtimeType"))
        safe["sentry_start_ticks"] = self.process_start(safe["sentry_pid"])
        containers = json.loads(self.probe.node_run("crictl", "ps", "-a", "-o", "json"))["containers"]
        child, = [row for row in containers if row.get("podSandboxId") == safe["sandbox_id"] and row["metadata"]["name"] == "guest"]
        safe["container_id"] = child["id"]
        inspected = json.loads(self.probe.node_run("crictl", "inspect", child["id"]))
        path = inspected["status"].get("logPath", "")
        expected = "/var/log/pods/" + NAMESPACE + "_" + metric["pod_name"] + "_" + metric["pod_uid"] + "/guest/"
        require(path.startswith(expected) and re.fullmatch(r"[0-9]+\.log", path[len(expected):]), "CRI log is outside the exact owned Pod")
        safe["log_path"] = path
        row = self.result["scenarios"][mode] = {"pod": redact_pod(pod), "runtime": safe}
        self.active_runtime = safe
        self.signal_file(mode + ".release")
        row["initial_heartbeat"] = self.wait_heartbeat(safe)
        row["before_fault"] = self.snapshot(safe)
        require(row["before_fault"]["same_sentry"] and row["before_fault"]["sandbox_ready"] and row["before_fault"]["container_running"], "baseline runtime is not actually live")
        self.signal_file(mode + ".observation-ready")

    def cordon_node(self):
        self.owned()
        self.cordoned = True
        self.probe.run(*self.probe.base, "cordon", self.probe.node)
        # Wait for the scheduler's real FailedScheduling witness before
        # creating the controller Pod; never infer Pending just from YAML.
        self.probe.apply({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "unscheduled-witness", "namespace": NAMESPACE},
            "spec": {"restartPolicy": "Never", "automountServiceAccountToken": False,
                "containers": [{"name": "witness", "image": IMAGE, "command": ["sleep", "30"]}]}})
        for until in [time.monotonic() + 30]:
            while time.monotonic() < until:
                pod = json.loads(self.probe.kube("get", "pod", "unscheduled-witness", "-o", "json"))
                blocked = [row for row in pod.get("status", {}).get("conditions", []) if row.get("type") == "PodScheduled" and row.get("status") == "False" and row.get("reason") == "Unschedulable"]
                if blocked:
                    require(not pod["spec"].get("nodeName"), "cordon witness was assigned")
                    self.result["cordon_witness"] = redact_pod(pod)
                    self.signal_file("pending.node-cordoned")
                    return
                time.sleep(.2)
        raise TimeoutError("scheduler did not confirm the owned node is unschedulable")

    def uncordon_node(self):
        if self.cordoned:
            self.owned()
            self.probe.kube("delete", "pod", "unscheduled-witness", "--ignore-not-found", "--wait=true", "--timeout=10s")
            self.probe.run(*self.probe.base, "uncordon", self.probe.node)
            self.cordoned = False

    def stop_kubelet(self):
        self.owned()
        row = self.result["scenarios"]["lost"]
        before, containerd = self.service("kubelet"), self.service("containerd")
        require(before["ActiveState"] == "active" and containerd["ActiveState"] == "active", "services not initially active")
        self.stopped = True  # finally must recover even if systemctl fails halfway.
        self.probe.node_run("systemctl", "stop", "kubelet")
        after, containerd_after = self.service("kubelet"), self.service("containerd")
        ready = self.probe.run(*self.probe.base, "get", "--raw=/readyz").strip()
        row["kubelet"] = {"before": before, "after": after, "containerd_before": containerd, "containerd_after": containerd_after,
                          "api_ready_after_stop": ready, "observed_at_utc": now()}
        require(after["ActiveState"] == "inactive" and after["MainPID"] == "0" and containerd_after == containerd and ready == "ok", "kubelet-only failure did not preserve runtime/API")
        before_hb = self.heartbeat(self.active_runtime)
        row["heartbeat_after_stop"] = self.wait_heartbeat(self.active_runtime, baseline=before_hb)
        self.signal_file("lost.kubelet-stopped")

    def restore_kubelet(self):
        if not self.stopped:
            return
        self.owned()
        self.probe.node_run("systemctl", "start", "kubelet")
        state = self.service("kubelet")
        require(state["ActiveState"] == "active", "owned kubelet did not recover")
        self.stopped = False
        self.result["kubelet_recovery"] = {"state": state, "observed_at_utc": now(), "method": "restart own kubelet only; no CRI/Sentry kill or active GC"}

    def physical_cleanup(self, runtime):
        for until in [time.monotonic() + 60]:
            while time.monotonic() < until:
                processes = self.probe.node_run("ps", "-eo", "pid,stat,comm,args")
                live = [line for line in processes.splitlines() if runtime["sandbox_id"] in line and
                        any(word in line for word in ("runsc-sandbox", "runsc-gofer", "containerd-shim-runsc")) and not line.split()[1].startswith("Z")]
                sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))["items"]
                ready = [row for row in sandboxes if row["metadata"]["uid"] == runtime["pod_uid"] and row.get("state") == "SANDBOX_READY"]
                containers = json.loads(self.probe.node_run("crictl", "ps", "-a", "-o", "json"))["containers"]
                running = [row for row in containers if row["id"] == runtime["container_id"] and row["state"] == "CONTAINER_RUNNING"]
                if not live and not ready and not running:
                    return {"no_live_sentry_gofer_shim": True, "no_ready_cri": True, "no_running_child_container": True,
                            "observed_at_utc": now(), "method": "natural kubelet/runtime cleanup; no stop/rmp/signal"}
                time.sleep(.25)
        raise TimeoutError("owned sandbox did not naturally stop after kubelet recovery")

    def reconciled(self, metric):
        mode = metric["mode"]
        row = self.result["scenarios"][mode]
        row["metric"] = metric
        if mode == "pending":
            require(not self.probe.kube("get", "pod", metric["pod_name"], "--ignore-not-found", "-o", "json").strip(), "cancelled Pending API object remains")
            sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))["items"]
            require(not any(item["metadata"]["uid"] == metric["pod_uid"] for item in sandboxes), "cancelled Pending control created a sandbox")
            row["physical_cleanup"] = {"never_assigned": True, "no_cri_sandbox": True, "api_object_gone": True}
            self.uncordon_node()
            self.signal_file(mode + ".physically-cleaned")
            return
        if mode == "lost":
            # Baseline is read only AFTER API404 and production reconciliation.
            # Three further records cannot be buffered pre-deletion heartbeats.
            samples = row["post_api404_samples"] = [self.snapshot(self.active_runtime)]
            require(samples[0]["heartbeat"] is not None, "missing post-404 heartbeat baseline")
            self.wait_heartbeat(self.active_runtime, baseline=samples[0]["heartbeat"], not_before_unix_nano=samples[0]["node_unix_nano"])
            samples.append(self.snapshot(self.active_runtime))
            verify_orphan_window(row)
            self.result["expected_negative_reproduced"] = metric["pod_deleted"] is True
            self.restore_kubelet()
        row["physical_cleanup"] = self.physical_cleanup(self.active_runtime)
        self.signal_file(mode + ".physically-cleaned")

    def execute(self):
        self.configure()
        self.prepare()
        self.rbac()
        self.stage("run_real_controller_node_loss_negative")
        env = dict(os.environ, AGENTMOOR_NODE_LOSS="1", AGENTMOOR_CONTROLLER_API=self.api,
            AGENTMOOR_CONTROLLER_KUBE_CA=str(self.kube_ca), AGENTMOOR_CONTROLLER_TOKEN_FILE=str(self.token),
            AGENTMOOR_CONTROLLER_NAMESPACE=NAMESPACE, AGENTMOOR_CONTROLLER_IMAGE=self.manifest_image,
            AGENTMOOR_CONTROLLER_GATEWAY_IP=self.gateway_ip, AGENTMOOR_CONTROLLER_GATEWAY_CA=str(self.certificate),
            AGENTMOOR_CONTROLLER_GATEWAY_KEY=str(self.key), AGENTMOOR_CONTROLLER_DIRECTORY=str(self.probe.directory))
        self.driver = subprocess.Popen(["go", "test", "-tags", "integration", "-run", "^TestSandboxNodeLossKubernetes$", "-count=1", "-v", "-timeout", "8m", "./internal/sandboxkube"],
            cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        selector = selectors.DefaultSelector()
        selector.register(self.driver.stdout, selectors.EVENT_READ)
        os.set_blocking(self.driver.stdout.fileno(), False)
        pending = b""
        log = self.probe.directory / "node-loss-driver.log"
        until = time.monotonic() + 500
        try:
            with log.open("w") as stream:
                while self.driver.poll() is None or selector.get_map():
                    require(time.monotonic() < until, "node-loss driver exceeded bounded duration")
                    for key, _ in selector.select(timeout=.5):
                        chunk = os.read(key.fileobj.fileno(), 65536)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        pending += chunk
                        lines = pending.split(b"\n")
                        pending = lines.pop()
                        for raw in lines:
                            line = raw.decode("utf-8", errors="replace")
                            stream.write(line + "\n")
                            stream.flush()
                            if line.startswith(EVENT):
                                event = json.loads(line[len(EVENT):])
                                if event["stage"] == "observe":
                                    self.observe_guest(event["metric"])
                                elif event["stage"] == "cordon-node":
                                    self.cordon_node()
                                elif event["stage"] == "stop-kubelet":
                                    self.stop_kubelet()
                                elif event["stage"] == "reconciled":
                                    self.reconciled(event["metric"])
                code = self.driver.wait(timeout=5)
                self.result["driver_exit_code"] = code
        finally:
            selector.close()
            self.restore_kubelet()
            self.uncordon_node()
            if self.driver.poll() is None:
                os.killpg(self.driver.pid, signal.SIGTERM)
                try:
                    self.driver.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(self.driver.pid, signal.SIGKILL)
                    self.driver.wait(timeout=5)
            self.result["driver_log"] = log.read_text()[-16000:]
            self.result["driver_log_sha256"] = hashlib.sha256(log.read_bytes()).hexdigest()
        result = self.probe.directory / "node-loss-driver-result.json"
        if result.exists():
            self.result["driver_result"] = json.loads(result.read_text())
        require(set(self.result["scenarios"]) == {"pending", "normal", "lost"}, "actual scenario matrix incomplete")
        require(self.result["scenarios"]["normal"].get("physical_cleanup"), "normal cancellation control did not physically stop")
        verify_physical_cleanup_claim(self.result["scenarios"]["lost"])
        require(code == 0, "node-loss driver failed independently of the cleanup invariant")

    def cleanup(self):
        try:
            self.restore_kubelet()
            self.uncordon_node()
            if self.driver is not None and self.driver.poll() is None:
                os.killpg(self.driver.pid, signal.SIGTERM)
                try:
                    self.driver.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(self.driver.pid, signal.SIGKILL)
                    self.driver.wait(timeout=5)
            self.probe.clean_security()
        finally:
            subprocess.run(["docker", "image", "rm", self.image], capture_output=True)
            for path in self.private + [self.probe.directory / "tls/tls.key"]:
                path.unlink(missing_ok=True)
            remains = subprocess.run(["docker", "image", "inspect", self.image], capture_output=True)
            self.result["cleanup"] = {**self.probe.result["cleanup"], "controller_image_tag_removed": remains.returncode != 0,
                "private_fixture_credentials_removed": all(not p.exists() for p in self.private + [self.probe.directory / "tls/tls.key"])}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--calico-manifest", type=Path)
    parser.add_argument("--containerd-cache", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    options = parser.parse_args()
    output = options.output.resolve()
    require(not os.path.lexists(output) and output.parent.is_dir(), "new evidence output path required")
    directory = Path(tempfile.mkdtemp(prefix="agentmoor-node-loss-"))
    probe = SecurityProbe(directory, options.bundle, options.calico_manifest, containerd_cache=options.containerd_cache)
    smoke = NodeLossSmoke(probe)
    failure = None
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        probe.prepare()
        probe.setup()
        smoke.execute()
    except BaseException as error:
        failure = error
        smoke.result["failure"] = {"type": type(error).__name__, "message": str(error)}
        smoke.diagnostics()
    finally:
        try:
            smoke.restore_kubelet()
            if smoke.active_runtime and "physical_cleanup" not in smoke.result["scenarios"].get("lost", {}):
                smoke.result["final_recovery_observation"] = smoke.physical_cleanup(smoke.active_runtime)
        except BaseException as error:
            smoke.result["recovery_error"] = str(error)
            failure = failure or error
        try:
            smoke.cleanup()
        except BaseException as error:
            smoke.result["cleanup_error"] = str(error)
            failure = failure or error
        smoke.result.update(status="failed" if failure else "passed", finished_at_utc=now(), retained_work_directory=str(directory),
            runtime_feasibility=probe.result, runtime_security_bootstrap=probe.security)
        safe, redactions = safe_evidence(smoke.result)
        failure = failure or (RuntimeError("unexpected capability redaction") if redactions else None)
        with output.open("x") as stream:
            json.dump(safe, stream, indent=2)
            stream.write("\n")
        print("NODE_LOSS_PROBE_RESULT " + json.dumps({"status": safe["status"], "expected_negative_reproduced": safe.get("expected_negative_reproduced"),
            "failure": safe.get("failure"), "evidence": str(output), "cleanup": safe.get("cleanup")}), flush=True)
    return 1 if failure else 0


if __name__ == "__main__":
    raise SystemExit(main())

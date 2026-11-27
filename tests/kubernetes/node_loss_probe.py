"""Guest-stop proof and finalizer lifecycle probe on one owned real gVisor node.

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
from node_loss_network import NodeLossNetwork, OwnedAdmin, verify_canaries

EVENT = "SANDBOX_NODE_LOSS_EVENT "
HEARTBEAT = "AGENTMOOR_NODE_LOSS_HEARTBEAT "


class NodeLossSecurityProbe(SecurityProbe):
    # The controller RBAC fixture applies JSON via stdin; the resource probe's
    # read-only kube wrapper intentionally lacks that optional argument.
    def kube(self, *args, data=None):
        if data is None:
            return super().kube(*args)
        return self.run(*self.base, "-n", NAMESPACE, *args, data=data)


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


def verify_live_samples(window, targets):
    a, b = window["samples"]
    fresh = [item for item in b["recent_heartbeats"] if item["sequence"] > a["heartbeat"]["sequence"] and item["wall_unix_nano"] > a["node_unix_nano"]]
    require(len(fresh) >= 3 and all(item["nonce"] == a["heartbeat"]["nonce"] and item["pid"] == a["heartbeat"]["pid"] for item in fresh), "live window lacks three new same-child emissions")
    require(all(item["same_sentry"] and item["sandbox_ready"] and item["container_running"] for item in (a, b)), "live window lacks the same CRI/Sentry")
    for item in fresh:
        verify_canaries(item["canaries"], targets)
    control = window["same_target_control"]
    require(control["wall_unix_nano"] > a["node_unix_nano"], "control baseline is stale")
    verify_canaries(control["canaries"], targets, control=True)
    return True


def verify_guest_stop_matrix(result):
    rows = result["scenarios"]
    require(set(rows) == {"pending", "normal", "lost", "admin-missing"}, "guest stop matrix incomplete")
    for mode, row in rows.items():
        metric = row["metric"]
        require(metric["requests"]["posts"] == 1 and metric["api_not_found"] and metric["capabilities_denied"] and metric["checkpoint_unchanged"] and metric["identity_deadline_unchanged"], "fence or business invariants failed")
        require(row.get("physical_cleanup"), "independent runtime cleanup not observed")
        if mode == "admin-missing":
            verify_physical_cleanup_claim(row)
            require(metric["cleanup_unknown"] and not metric["guest_stopped"], "API404 was retroactively blessed")
            continue
        proof = metric.get("stop_proof", {})
        require(metric["guest_stopped"] and metric["pod_deleted"] and not metric["cleanup_unknown"] and proof.get("pod_uid") == metric["pod_uid"], "released object lacks its original guest stop proof")
        if mode == "pending":
            require(proof.get("kind") == "never-scheduled-cas.v1" and not proof.get("node_name") and not proof.get("container_id") and row["binding_after_delete"]["status"] == 409, "Pending CAS or authorized-binding rejection missing")
        else:
            require(proof.get("kind") == "terminated.v1" and proof.get("node_name") and proof.get("container_id") and proof.get("container_name") == "guest" and proof.get("restart_count") == 0 and isinstance(proof.get("exit_code"), int), "terminated proof lacks bound runtime evidence")
    lost = rows["lost"]
    for name in ("deleting_live_window", "phase_failed_live_window"):
        window = lost[name]
        require(not window["metric"]["guest_stopped"] and not window["metric"]["pod_deleted"], "live guest was released")
        verify_live_samples(window, result["network"]["targets"])
    quota = lost["object_quota"]
    require(quota["count_pods_named_in_rejection"] and quota["new_pod_status"] == 403 and quota["after_delete_admission_status"] == 201 and quota["new_witness_no_cri"], "terminal held object budget did not refuse then release")
    return True


class NodeLossSmoke(ControllerSmoke):
    def __init__(self, probe):
        super().__init__(probe)
        self.stopped = False
        self.cordoned = False
        self.active_runtime = None
        self.active_mode = None
        self.network = NodeLossNetwork(self)
        self.admin = None
        self.quota = None
        self.result.update(scope="node-loss guest stop-proof and API object deletion, component only", native_vendor=False,
            scenarios={}, expected_negative_reproduced=False,
            limitations=["Synthetic Go child only; no vendor CLI/model/SaaS acceptance.",
                "Single owned kind node; stopping kubelet is a partition surrogate, not cloud power fencing or HA acceptance.",
                "Host driver uses the production Catalog/Controller and a synthetic input TLS handler, not the production gateway.",
                "Finite direct TCP fault-window canaries and count/pods object quota only; no complete Internet, aggregate physical quota or HA guarantee.",
                "Terminated guest status proves runtime-reported guest stop; exact runtime process cleanup is observed separately."])

    def configure(self):
        actual = self.probe.node_run("cat", "/etc/containerd/runsc.toml")
        require(actual == self.probe.security["runsc_config"] and 'overlay2 = "all:memory,size=512m"' in actual, "actual fixed runtime configuration differs")
        require(" v2.4.2 " in self.probe.node_run("containerd", "--version"), "official fixed containerd candidate missing")
        self.result["runtime_config"] = actual

    def prepare(self):
        relevant = ["cmd", "internal", "tests/kubernetes/controllerfixture", "tests/kubernetes/node_loss_probe.py", "tests/kubernetes/node_loss_probe_test.py", "tests/kubernetes/node_loss_network.py"]
        require(not self.probe.run("git", "-C", ROOT, "status", "--short", "--", *relevant).strip(), "commit all node-loss source before acceptance")
        super().prepare()
        files = [Path(__file__), Path(__file__).with_name("node_loss_network.py"), ROOT / "internal/sandboxkube/node_loss_kubernetes_test.go", ROOT / "tests/kubernetes/controllerfixture/main.go"]
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
                require(set(row) == {"nonce", "pid", "sequence", "elapsed_nano", "wall_unix_nano", "uid", "canaries"} and re.fullmatch(r"[a-f0-9]{32}", row["nonce"]) and row["pid"] >= 2 and row["sequence"] > 0, "invalid bounded heartbeat record")
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
        self.active_mode = mode
        self.signal_file(mode + ".release")
        row["initial_heartbeat"] = self.wait_heartbeat(safe)
        row["before_fault"] = self.snapshot(safe)
        require(row["before_fault"]["same_sentry"] and row["before_fault"]["sandbox_ready"] and row["before_fault"]["container_running"], "baseline runtime is not actually live")
        verify_canaries(row["before_fault"]["heartbeat"]["canaries"], self.network.targets, require_allowed=True)
        row["before_fault"]["same_target_control"] = self.network.wait_control(row["before_fault"]["node_unix_nano"])
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

    def stop_kubelet(self, metric):
        self.owned()
        mode = metric["mode"]
        row = self.result["scenarios"][mode]
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
        self.signal_file(mode + ".kubelet-stopped")

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

    def pod_admin(self, metric):
        path = "/api/v1/namespaces/"+NAMESPACE+"/pods/"+metric["pod_name"]
        status, pod = self.admin.request("GET", path)
        require(status == 200 and pod["metadata"]["uid"] == metric["pod_uid"], "admin observation changed the owned Pod UID")
        return path, pod

    def pending_deleting(self, metric):
        path, pod = self.pod_admin(metric)
        require(pod["metadata"].get("deletionTimestamp") and "agentmoor.io/guest-stop" in pod["metadata"].get("finalizers", []) and not pod["spec"].get("nodeName"), "Pending cancellation is not retained and unbound")
        require(self.probe.run(*self.probe.base, "auth", "can-i", "create", "pods", "--subresource", "binding", "-n", NAMESPACE).strip() == "yes", "binding counterexample lacks admin authority")
        status, body = self.admin.request("POST", path+"/binding", {"apiVersion": "v1", "kind": "Binding", "metadata": {"name": metric["pod_name"], "namespace": NAMESPACE, "uid": metric["pod_uid"]}, "target": {"apiVersion": "v1", "kind": "Node", "name": self.probe.node}})
        require(status == 409 and body.get("reason") == "Conflict", "deleting Pending Pod did not reject an otherwise authorized binding")
        self.result["scenarios"]["pending"]["binding_after_delete"] = {"authorized_admin": True, "status": status, "reason": body.get("reason"), "pod_uid": metric["pod_uid"], "requested_node": self.probe.node}
        self.signal_file("pending.binding-rejected")

    def live_window(self, metric, name):
        row = self.result["scenarios"][metric["mode"]]
        samples = [self.snapshot(self.active_runtime)]
        window = row[name] = {"metric": metric, "samples": samples}
        require(samples[0]["heartbeat"] is not None, "missing live-window baseline")
        self.wait_heartbeat(self.active_runtime, baseline=samples[0]["heartbeat"], not_before_unix_nano=samples[0]["node_unix_nano"])
        samples.append(self.snapshot(self.active_runtime))
        window["same_target_control"] = self.network.wait_control(samples[0]["node_unix_nano"])
        verify_live_samples(window, self.network.targets)
        return window

    def held(self, metric):
        path, pod = self.pod_admin(metric)
        require(pod["metadata"].get("deletionTimestamp") and "agentmoor.io/guest-stop" in pod["metadata"].get("finalizers", []) and pod["status"]["phase"] == "Running", "live deleting Pod was not retained")
        require(not metric["guest_stopped"] and not metric["pod_deleted"] and not metric["cleanup_unknown"], "retained guest was falsely stopped")
        self.live_window(metric, "deleting_live_window")
        # Model the exact PodGC danger: a trusted control-plane status writer
        # may mark PhaseFailed without any kubelet-reported terminated state.
        patch = [{"op": "test", "path": "/metadata/uid", "value": pod["metadata"]["uid"]},
                 {"op": "test", "path": "/metadata/resourceVersion", "value": pod["metadata"]["resourceVersion"]},
                 {"op": "replace", "path": "/status/phase", "value": "Failed"}]
        status, after = self.admin.request("PATCH", path+"/status", patch, "application/json-patch+json")
        require(status == 200 and after["status"]["phase"] == "Failed" and after["status"]["containerStatuses"] == pod["status"]["containerStatuses"], "PhaseFailed surrogate changed container evidence")
        self.result["scenarios"]["lost"]["phase_failed_patch"] = {"status": status, "uid": pod["metadata"]["uid"], "container_status_unchanged": True, "source": "admin status-only PodGC surrogate; not a kubelet termination"}
        self.signal_file("lost.phase-failed")

    def quota_refusal(self, metric):
        prefix = "/api/v1/namespaces/"+NAMESPACE
        status, pods = self.admin.request("GET", prefix+"/pods")
        require(status == 200, "cannot count owned namespace objects")
        count = len(pods["items"])
        require(any(pod["metadata"]["uid"] == metric["pod_uid"] and pod["status"]["phase"] == "Failed" and "agentmoor.io/guest-stop" in pod["metadata"].get("finalizers", []) for pod in pods["items"]), "terminal held object not counted")
        quota = {"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "node-loss-object-budget", "namespace": NAMESPACE}, "spec": {"hard": {"pods": str(count), "count/pods": str(count)}}}
        status, _ = self.admin.request("POST", prefix+"/resourcequotas", quota)
        require(status == 201, "owned object budget was not created")
        self.quota = {"name": quota["metadata"]["name"], "hard_count": count}
        until = time.monotonic()+25
        while time.monotonic() < until:
            status, observed = self.admin.request("GET", prefix+"/resourcequotas/"+self.quota["name"])
            if status == 200 and observed.get("status", {}).get("used", {}).get("count/pods") == str(count):
                break
            time.sleep(.25)
        else:
            raise TimeoutError("object quota did not account for the held terminal Pod")
        dummy = self.quota_dummy()
        status, body = self.admin.request("POST", prefix+"/pods", dummy)
        require(status == 403 and body.get("reason") == "Forbidden" and "count/pods" in body.get("message", ""), "held terminal object did not refuse an additional Pod")
        self.result["scenarios"]["lost"]["object_quota"] = {"hard": quota["spec"]["hard"], "used_while_phase_failed": observed["status"]["used"], "new_pod_status": status, "reason": body.get("reason"), "count_pods_named_in_rejection": True}

    def quota_dummy(self):
        return {"apiVersion": "v1", "kind": "Pod", "metadata": {"namespace": NAMESPACE, "name": "node-loss-quota-admission"},
            "spec": {"restartPolicy": "Never", "nodeName": "agentmoor-fixture-never-exists", "automountServiceAccountToken": False,
                "containers": [{"name": "fixture", "image": self.probe.image, "imagePullPolicy": "Never", "command": ["sleep", "30"]}]}}

    def quota_released(self):
        if self.quota is None:
            return
        prefix = "/api/v1/namespaces/"+NAMESPACE
        until = time.monotonic()+25
        while time.monotonic() < until:
            status, observed = self.admin.request("GET", prefix+"/resourcequotas/"+self.quota["name"])
            if status == 200 and int(observed["status"]["used"]["count/pods"]) < self.quota["hard_count"]:
                break
            time.sleep(.25)
        else:
            raise TimeoutError("deleted object did not release the object quota")
        status, pod = self.admin.request("POST", prefix+"/pods", self.quota_dummy())
        require(status == 201 and pod["spec"]["nodeName"] == "agentmoor-fixture-never-exists", "same quota did not allow one new unscheduled object after deletion")
        uid = pod["metadata"]["uid"]
        sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))["items"]
        require(not any(row["metadata"]["uid"] == uid for row in sandboxes), "quota witness executed unexpectedly")
        self.result["scenarios"]["lost"]["object_quota"].update(used_after_api_deletion=observed["status"]["used"], after_delete_admission_status=status, new_witness_no_cri=True)
        self.admin.request("DELETE", prefix+"/pods/"+pod["metadata"]["name"], {"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": 0, "preconditions": {"uid": uid}})
        status, _ = self.admin.request("DELETE", prefix+"/resourcequotas/"+self.quota["name"], {"apiVersion": "v1", "kind": "DeleteOptions"})
        require(status == 200, "owned quota cleanup failed")
        self.quota = None

    def phase_failed(self, metric):
        _, pod = self.pod_admin(metric)
        require(pod["status"]["phase"] == "Failed" and "running" in pod["status"]["containerStatuses"][0]["state"], "PhaseFailed does not retain Running guest status")
        require(metric["pod_terminal"] and not metric["guest_stopped"] and not metric["pod_deleted"], "PhaseFailed fabricated a stop proof")
        self.live_window(metric, "phase_failed_live_window")
        self.quota_refusal(metric)
        self.restore_kubelet()
        self.signal_file("lost.kubelet-restored")

    def admin_remove(self, metric):
        path, pod = self.pod_admin(metric)
        finalizers = pod["metadata"].get("finalizers", [])
        require(finalizers == ["agentmoor.io/guest-stop"], "unexpected fixture finalizers")
        patch = [{"op": "test", "path": "/metadata/uid", "value": metric["pod_uid"]}, {"op": "test", "path": "/metadata/resourceVersion", "value": pod["metadata"]["resourceVersion"]}, {"op": "remove", "path": "/metadata/finalizers/0"}]
        status, _ = self.admin.request("PATCH", path, patch, "application/json-patch+json")
        require(status == 200, "admin-only finalizer removal failed")
        status, _ = self.admin.request("DELETE", path, {"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": 0, "preconditions": {"uid": metric["pod_uid"]}})
        require(status in (200, 202), "admin UID force-delete failed")
        status, _ = self.admin.request("GET", path)
        require(status == 404, "admin-deleted object still exists")
        self.result["scenarios"][metric["mode"]]["admin_override"] = {"finalizer_removed_by_test_admin": True, "uid_precondition": metric["pod_uid"], "api_status_after_delete": status}
        self.signal_file("admin-missing.api-removed")

    def unknown(self, metric):
        row = self.result["scenarios"][metric["mode"]]
        row["metric"] = metric
        window = self.live_window(metric, "post_api404_window")
        row["post_api404_samples"] = window["samples"]
        verify_orphan_window(row)
        verify_physical_cleanup_claim(row)
        require(metric["cleanup_unknown"] and not metric["guest_stopped"], "missing API was not held unknown")
        self.restore_kubelet()
        row["physical_cleanup"] = self.physical_cleanup(self.active_runtime)
        self.signal_file("admin-missing.recovery-complete")

    def reconciled(self, metric):
        mode = metric["mode"]
        row = self.result["scenarios"][mode]
        row["metric"] = metric
        require(metric["requests"]["posts"] == 1 and metric["api_not_found"], "original controller fence was re-POSTed or API deletion not observed")
        if mode == "pending":
            require(metric["guest_stopped"] and metric["pod_deleted"] and metric["stop_proof"]["kind"] == "never-scheduled-cas.v1", "Pending cancellation lacks its exact-CAS stop proof")
            sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))["items"]
            require(not any(item["metadata"]["uid"] == metric["pod_uid"] for item in sandboxes), "cancelled Pending control created a sandbox")
            row["physical_cleanup"] = {"never_assigned": True, "no_cri_sandbox": True, "api_object_gone": True}
            self.uncordon_node()
        elif mode != "admin-missing":
            require(metric["guest_stopped"] and metric["pod_deleted"] and metric["stop_proof"]["kind"] == "terminated.v1", "real guest termination was not persisted before API deletion")
            row["physical_cleanup"] = self.physical_cleanup(self.active_runtime)
            if mode == "lost":
                self.quota_released()
        else:
            require(metric["cleanup_unknown"] and not metric["guest_stopped"] and not metric["pod_deleted"], "missing metadata was retroactively blessed")
        self.signal_file(mode+".physically-cleaned")

    def execute(self):
        self.configure()
        self.prepare()
        self.rbac()
        self.admin = OwnedAdmin(self)
        self.network.prepare()
        self.stage("run_real_controller_guest_stop_matrix")
        env = dict(os.environ, AGENTMOOR_NODE_LOSS="1", AGENTMOOR_CONTROLLER_API=self.api,
            AGENTMOOR_CONTROLLER_KUBE_CA=str(self.kube_ca), AGENTMOOR_CONTROLLER_TOKEN_FILE=str(self.token),
            AGENTMOOR_CONTROLLER_NAMESPACE=NAMESPACE, AGENTMOOR_CONTROLLER_IMAGE=self.manifest_image,
            AGENTMOOR_CONTROLLER_GATEWAY_IP=self.gateway_ip, AGENTMOOR_CONTROLLER_GATEWAY_CA=str(self.certificate),
            AGENTMOOR_CONTROLLER_GATEWAY_KEY=str(self.key), AGENTMOOR_CONTROLLER_DIRECTORY=str(self.probe.directory),
            AGENTMOOR_NODE_LOSS_CANARIES=json.dumps(self.network.targets))
        self.driver = subprocess.Popen(["go", "test", "-tags", "integration", "-run", "^TestSandboxNodeLossKubernetes$", "-count=1", "-v", "-timeout", "10m", "./internal/sandboxkube"],
            cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        selector = selectors.DefaultSelector()
        selector.register(self.driver.stdout, selectors.EVENT_READ)
        os.set_blocking(self.driver.stdout.fileno(), False)
        pending = b""
        log = self.probe.directory / "node-loss-driver.log"
        until = time.monotonic() + 650
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
                                elif event["stage"] == "configure-network":
                                    self.network.configure(event["metric"]["fixture_input_port"])
                                    self.signal_file("network.configured")
                                elif event["stage"] == "cordon-node":
                                    self.cordon_node()
                                elif event["stage"] == "stop-kubelet":
                                    self.stop_kubelet(event["metric"])
                                elif event["stage"] == "pending-deleting":
                                    self.pending_deleting(event["metric"])
                                elif event["stage"] == "held":
                                    self.held(event["metric"])
                                elif event["stage"] == "phase-failed":
                                    self.phase_failed(event["metric"])
                                elif event["stage"] == "admin-remove":
                                    self.admin_remove(event["metric"])
                                elif event["stage"] == "unknown":
                                    self.unknown(event["metric"])
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
        require(set(self.result["scenarios"]) == {"pending", "normal", "lost", "admin-missing"}, "actual scenario matrix incomplete")
        require(self.result["scenarios"]["normal"].get("physical_cleanup"), "normal cancellation control did not physically stop")
        verify_physical_cleanup_claim(self.result["scenarios"]["admin-missing"])
        verify_guest_stop_matrix(self.result)
        self.result["fixed_guest_stop_matrix_passed"] = True
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
    parser.add_argument("--output", type=Path, default=os.environ.get("NODE_LOSS_OUTPUT"))
    options = parser.parse_args()
    output = options.output.resolve() if options.output else None
    if output:
        require(not os.path.lexists(output) and output.parent.is_dir(), "new evidence output path required")
    directory = Path(tempfile.mkdtemp(prefix="agentmoor-node-loss-"))
    probe = NodeLossSecurityProbe(directory, options.bundle, options.calico_manifest, containerd_cache=options.containerd_cache)
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
            if smoke.active_runtime and "physical_cleanup" not in smoke.result["scenarios"].get(smoke.active_mode, {}):
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
        if output:
            with output.open("x") as stream:
                json.dump(safe, stream, indent=2)
                stream.write("\n")
        print("NODE_LOSS_PROBE_RESULT " + json.dumps({"status": safe["status"], "expected_negative_reproduced": safe.get("expected_negative_reproduced"),
            "failure": safe.get("failure"), "evidence": str(output), "cleanup": safe.get("cleanup")}), flush=True)
    return 1 if failure else 0


if __name__ == "__main__":
    raise SystemExit(main())

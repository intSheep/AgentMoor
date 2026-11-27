"""Exercise the production isolated SaaS path with real fixed Codex and scripted TLS model."""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import ssl
import subprocess
import sys
import tempfile
import threading
import time

from gvisor_probe import require
from gvisor_security_probe import SecurityProbe, GUEST_NS, CONTROL_NS, GATEWAY_LABEL, network_policy
from sandbox_controller_smoke import redact_pod, safe_evidence
from contract import forward

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests"))
from saas_file_contract import FileReportClient
from skill_report_contract import SkillCatalog, SkillReportClient


def now():
    return datetime.now(timezone.utc).isoformat()


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def strip_private(value):
    if isinstance(value, dict):
        return {key: strip_private(item) for key, item in value.items()
                if key.lower() not in ("annotations", "managedfields")}
    if isinstance(value, list):
        return [strip_private(item) for item in value]
    return value


class ProviderSmoke:
    def __init__(self, probe):
        self.p = probe
        self.image = "agentmoor-provider:" + probe.cluster
        self.base_image = "agentmoor-provider-base:" + probe.cluster
        self.origin = "agentmoor." + CONTROL_NS + ".svc"
        self.model_origin = "model." + CONTROL_NS + ".svc"
        self.observed, self.observer_errors, self.seen_pods = {}, [], {}
        self.lock, self.stop = threading.Lock(), threading.Event()
        self.result = {"status": "in_progress", "started_at_utc": now(),
                       "scope": "production sandbox provider / real Codex / scripted private-CA TLS model",
                       "real_model_key_used": False, "scenarios": [], "limitations": [
                           "Single owned kind node with Calico and runsc/systrap; not production HA or arbitrary escape proof.",
                           "Model responses and report tools are synthetic fixtures; no live-model quality claim.",
                           "Finite pre-execution network canaries do not prove all possible destinations."]}

    def stage(self, value):
        self.result["stage"] = value
        print("SANDBOX_PROVIDER_STAGE " + json.dumps({"stage": value, "time_utc": now()}), flush=True)

    def kube(self, *args, namespace=CONTROL_NS, data=None, timeout=120):
        return self.p.run(*self.p.base, "-n", namespace, *args, data=data, timeout=timeout)

    def apply(self, *items):
        return self.p.apply({"apiVersion": "v1", "kind": "List", "items": list(items)})

    def build(self):
        self.stage("build_real_codex_with_public_ca_only")
        relevant = ["cmd", "internal", "tests/kubernetes/sandbox_provider_smoke.py",
                    "tests/kubernetes/sandbox_provider_native.py", "tests/kubernetes/sandbox_provider_model.py"]
        require(not self.p.run("git", "-C", ROOT, "status", "--short", "--", *relevant).strip(),
                "commit implementation and provider fixtures before acceptance")
        self.result["source_commit"] = self.p.run("git", "-C", ROOT, "rev-parse", "HEAD").strip()
        self.result["fixture_sha256"] = {name: sha((ROOT / name).read_bytes()) for name in relevant if (ROOT / name).is_file()}
        private = self.p.directory / "provider-private"
        private.mkdir()
        self.cert, self.key = private / "cert.pem", private / "key.pem"
        self.p.run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-sha256",
                   "-subj", "/CN=" + self.origin, "-addext",
                   "subjectAltName=DNS:" + self.origin + ",DNS:" + self.model_origin + ",IP:127.0.0.1",
                   "-addext", "basicConstraints=critical,CA:TRUE", "-keyout", self.key, "-out", self.cert)
        self.key.chmod(0o600)
        self.model_key = private / "model-key"
        self.model_key.write_text("synthetic-upstream-" + os.urandom(24).hex())
        self.model_key.chmod(0o600)
        context = self.p.directory / "provider-public-build"
        context.mkdir()
        shutil.copyfile(self.cert, context / "ca.crt")
        for name in ("sandbox_provider_native.py", "sandbox_provider_model.py"):
            shutil.copyfile(ROOT / "tests/kubernetes" / name, context / name)
        dockerfile = "\n".join(["FROM " + self.base_image, "USER 0:0",
            "COPY ca.crt /usr/local/share/ca-certificates/agentmoor-provider.crt",
            "COPY ca.crt /etc/agentmoor/gateway-ca.pem", "RUN update-ca-certificates",
            "COPY sandbox_provider_native.py sandbox_provider_model.py /opt/test/",
            "RUN chmod 755 /opt/test/sandbox_provider_native.py", "USER 65532:65532", ""])
        (context / "Dockerfile").write_text(dockerfile)
        build_log = self.p.directory / "provider-image-build.log"
        with build_log.open("w") as stream:
            for command in (["docker", "build", "-f", str(ROOT / "tests/kubernetes/Dockerfile"), "--build-arg", "AGENT=codex", "-t", self.base_image, str(ROOT)],
                            ["docker", "build", "-t", self.image, str(context)]):
                subprocess.run(command, stdout=stream, stderr=subprocess.STDOUT, check=True, timeout=900)
        self.result["build_log_sha256"] = sha(build_log.read_bytes())
        self.result["docker_image_id"] = json.loads(self.p.run("docker", "image", "inspect", self.image))[0]["Id"]
        self.p.run("kind", "load", "docker-image", self.image, "--name", self.p.cluster, timeout=180)
        reference = "docker.io/library/" + self.image
        rows = [row.split() for row in self.p.node_run("ctr", "-n", "k8s.io", "images", "list").splitlines()]
        row, = [row for row in rows if row and row[0] == reference]
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", row[2]), "missing actual containerd manifest identity")
        self.pinned_image = "docker.io/library/agentmoor-provider@" + row[2]
        self.p.node_run("ctr", "-n", "k8s.io", "images", "tag", reference, self.pinned_image)
        self.result["guest_image"] = self.pinned_image
        self.binary = self.p.directory / "agentmoor"
        self.p.run("go", "build", "-o", self.binary, "./cmd/agentmoor", timeout=180)
        self.platform = private / "platform"
        self.p.run(self.binary, "init", "--data", self.platform)

    def token(self, scope, *rights):
        args = [part for key, value in scope.items() for part in ("--" + key, value)]
        return self.p.run(self.binary, "issue", "--data", self.platform, *args, *rights).strip()

    def secret(self, name, files):
        self.kube("create", "secret", "generic", name,
                  *("--from-file=" + key + "=" + str(path) for key, path in files.items()))

    def configmap(self, name, data):
        self.apply({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": name, "namespace": CONTROL_NS}, "data": data})

    def configure(self):
        self.stage("configure_restricted_guest_and_trusted_services")
        self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": GUEST_NS,
                    "labels": {"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.35"}}},
            network_policy(),
            {"apiVersion": "scheduling.k8s.io/v1", "kind": "PriorityClass", "metadata": {"name": "agentmoor-sandbox"}, "value": 0, "globalDefault": False, "preemptionPolicy": "Never"},
            {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "sandbox-controller", "namespace": CONTROL_NS}},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"name": "sandbox-controller", "namespace": GUEST_NS}, "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "create", "delete"]}]},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"name": "sandbox-controller", "namespace": GUEST_NS}, "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "sandbox-controller"}, "subjects": [{"kind": "ServiceAccount", "name": "sandbox-controller", "namespace": CONTROL_NS}]},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"name": "turn-jobs", "namespace": CONTROL_NS}, "rules": [{"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["get", "create"]}]},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"name": "turn-jobs", "namespace": CONTROL_NS}, "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "turn-jobs"}, "subjects": [{"kind": "ServiceAccount", "name": "sandbox-controller", "namespace": CONTROL_NS}]})
        # All memory/swap changes remain on this one proven-owned Docker node.
        owned = json.loads(self.p.run("docker", "inspect", self.p.node))[0]
        require(owned["Config"]["Labels"].get("io.x-k8s.kind.cluster") == self.p.cluster, "memory target is not owned")
        self.p.run("docker", "update", "--memory", str(4 << 30), "--memory-swap", str(4 << 30), self.p.node)
        self.result["node_memory"] = {name: self.p.node_run("cat", "/sys/fs/cgroup/" + name).strip() for name in ("memory.max", "memory.swap.max")}
        require(self.result["node_memory"] == {"memory.max": str(4 << 30), "memory.swap.max": "0"}, "node swap was not denied")
        self.secret("provider-tls", {"cert.pem": self.cert, "key.pem": self.key})
        self.secret("provider-model-auth", {"key": self.model_key})
        self.secret("provider-signing", {"signing.key": self.platform / "signing.key"})
        self.apply({"apiVersion": "v1", "kind": "Service", "metadata": {"name": "agentmoor", "namespace": CONTROL_NS}, "spec": {"selector": GATEWAY_LABEL, "ports": [{"name": "api", "port": 7401}, {"name": "gateway", "port": 7443}]}},
                   {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "model", "namespace": CONTROL_NS}, "spec": {"selector": {"app": "provider-model"}, "ports": [{"port": 8443}]}})
        service = json.loads(self.kube("get", "service", "agentmoor", "-o", "json"))
        self.gateway_ip = service["spec"]["clusterIP"]
        self.scopes, allocations, jobs = {}, [], []
        for scenario in ("file", "skill", "no-skill"):
            scope = {"tenant": "saas", "project": "reports", "runtime": scenario + "-job", "adapter": "codex"}
            self.scopes[scenario] = scope
            config = {"scope": scope, "endpoint": "https://" + self.origin + ":7401", "workspace": "/run/task/work", "state_dir": "/run/task/state", "artifacts": "out", "execution_policy": "untrusted-isolated.v1", "isolated_provider": "kubernetes-gvisor.v1"}
            self.configmap(scenario + "-worker", {"worker.json": json.dumps(config)})
            credential = self.p.directory / (scenario + "-execute-token")
            credential.write_text(self.token(scope, "--execute"))
            credential.chmod(0o600)
            self.secret(scenario + "-execute", {"token": credential})
            jobs.append({"scope": scope, "pod_spec": self.worker_spec(scenario)})
            allocations.append({"execution": {"scope": scope, "namespace": GUEST_NS, "image": self.pinned_image,
                "runtime_class": "agentmoor-gvisor", "gateway_url": "https://" + self.origin + ":7443", "gateway_ip": self.gateway_ip,
                "gateway_ca_path": "/etc/agentmoor/gateway-ca.pem", "runtime_executable": "/opt/test/sandbox_provider_native.py", "model": "gpt-5",
                "file_tool": "/opt/test/report_tool.py" if scenario == "file" else "/opt/test/skill_tool.py",
                "filesystem_class": "gvisor-overlay2-memory.v1", "cpu_millis": 1000, "memory_mib": 1024, "workspace_mib": 512, "temp_mib": 128},
                "model": {"upstream_url": "https://" + self.model_origin + ":8443/v1/responses", "api_key_file": "/model-auth/key", "ca_file": "/tls/cert.pem", "max_requests": 32, "max_input_bytes": 8388608, "max_output_bytes": 8388608, "per_request_output_bytes": 524288, "concurrency": 1, "max_output_tokens": 2048}})
        self.configmap("provider-config", {"sandbox.json": json.dumps({"version": "sandbox-deployment.v1", "network_policy": "gateway-only.v1", "gateway_listen": "0.0.0.0:7443", "kubernetes": {}, "profiles": allocations}), "profiles.json": json.dumps(jobs)})
        security = {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532}
        self.apply({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "provider-model", "namespace": CONTROL_NS, "labels": {"app": "provider-model"}}, "spec": {"automountServiceAccountToken": False, "restartPolicy": "Never", "securityContext": security,
            "containers": [{"name": "model", "image": self.image, "imagePullPolicy": "Never", "command": ["python3", "/opt/test/sandbox_provider_model.py", "--tls-cert", "/model-tls/cert.pem", "--tls-key", "/model-tls/key.pem", "--upstream-key-file", "/model-auth/key"], "resources": {"limits": {"memory": "256Mi", "cpu": "500m"}}, "volumeMounts": [{"name": "tls", "mountPath": "/model-tls", "readOnly": True}, {"name": "auth", "mountPath": "/model-auth", "readOnly": True}]}], "volumes": [{"name": "tls", "secret": {"secretName": "provider-tls"}}, {"name": "auth", "secret": {"secretName": "provider-model-auth"}}]}})
        mounts = [{"name": n, "mountPath": p, "readOnly": n != "data"} for n, p in (("data", "/data"), ("tls", "/tls"), ("auth", "/model-auth"), ("signing", "/signing"), ("config", "/configuration"))]
        self.apply({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "agentmoor", "namespace": CONTROL_NS, "labels": GATEWAY_LABEL}, "spec": {"serviceAccountName": "sandbox-controller", "restartPolicy": "Never", "securityContext": security,
            "initContainers": [{"name": "signing", "image": self.image, "imagePullPolicy": "Never", "command": ["python3", "-c", "from pathlib import Path; p=Path('/data/signing.key'); p.write_bytes(Path('/signing/signing.key').read_bytes()); p.chmod(0o600)"], "volumeMounts": mounts}],
            "containers": [{"name": "api", "image": self.image, "imagePullPolicy": "Never", "command": ["agentmoor", "serve", "--data", "/data", "--listen", "0.0.0.0:7401", "--tls-cert", "/tls/cert.pem", "--tls-key", "/tls/key.pem", "--sandbox-config", "/configuration/sandbox.json", "--turn-jobs", "/configuration/profiles.json"], "resources": {"limits": {"memory": "512Mi", "cpu": "1"}}, "volumeMounts": mounts}],
            "volumes": [{"name": "data", "emptyDir": {}}, {"name": "tls", "secret": {"secretName": "provider-tls"}}, {"name": "auth", "secret": {"secretName": "provider-model-auth"}}, {"name": "signing", "secret": {"secretName": "provider-signing"}}, {"name": "config", "configMap": {"name": "provider-config"}}]}})
        self.p.wait_ready(CONTROL_NS, "provider-model")
        self.p.wait_ready(CONTROL_NS, "agentmoor")

    def worker_spec(self, scenario):
        return {"automountServiceAccountToken": False, "restartPolicy": "Never", "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "fsGroup": 65532},
            "containers": [{"name": "worker", "image": self.image, "imagePullPolicy": "Never", "command": ["agentmoor", "run-task", "--config", "/config/worker.json"], "env": [{"name": "AGENTMOOR_TOKEN_FILE", "value": "/access/token"}], "resources": {"limits": {"memory": "512Mi", "cpu": "1"}},
                "volumeMounts": [{"name": "work", "mountPath": "/run/task"}, {"name": "access", "mountPath": "/access", "readOnly": True}, {"name": "config", "mountPath": "/config", "readOnly": True}]}],
            "volumes": [{"name": "work", "emptyDir": {}}, {"name": "access", "secret": {"secretName": scenario + "-execute"}}, {"name": "config", "configMap": {"name": scenario + "-worker"}}]}

    def launch(self, scenario):
        name = scenario + "-produce"
        self.apply({"apiVersion": "batch/v1", "kind": "Job", "metadata": {"name": name, "namespace": CONTROL_NS}, "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 900, "template": {"spec": self.worker_spec(scenario)}}})
        return name

    def observe(self):
        while not self.stop.wait(.2):
            try:
                pods = json.loads(self.kube("get", "pods", "-l", "app.kubernetes.io/name=agentmoor-sandbox", "-o", "json", namespace=GUEST_NS, timeout=10))["items"]
                for pod in pods:
                    uid = pod["metadata"]["uid"]
                    with self.lock:
                        self.seen_pods[uid] = {"pod": redact_pod(pod), "annotation_keys": sorted(pod.get("metadata", {}).get("annotations", {}))}
                        if uid in self.observed:
                            continue
                    if pod.get("status", {}).get("phase") != "Running":
                        continue
                    runtime = self.p.runtime(GUEST_NS, pod["metadata"]["name"])
                    evidence = {"pod": redact_pod(pod), "runtime": strip_private(runtime)}
                    require("sbx1." not in json.dumps(evidence), "runtime evidence contains a capability")
                    with self.lock:
                        self.observed[uid] = evidence
            except Exception as error:
                with self.lock:
                    self.observer_errors.append(str(error))

    def deleted(self, counter):
        name = counter["pod"]
        require(counter["count"] == 1, "native CLI re-executed inside one guest")
        until = time.monotonic() + 10
        while time.monotonic() < until:
            with self.lock:
                records = [row for row in self.observed.values() if row["pod"]["metadata"]["name"] == name]
            if records:
                break
            time.sleep(.2)
        require(len(records) == 1, "no real CRI/Sentry observation for the completed native Pod")
        row = records[0]
        missing = subprocess.run([*self.p.base, "-n", GUEST_NS, "get", "pod", name], capture_output=True, text=True)
        require(missing.returncode and "NotFound" in missing.stderr, "completed guest Pod still exists")
        processes = self.p.node_run("ps", "-eo", "pid,comm,args")
        require(not any("runsc-sandbox " in line and row["runtime"]["sandbox_id"] in line for line in processes.splitlines()), "deleted guest retained a live Sentry")
        return {"pod_uid": row["pod"]["metadata"]["uid"], "pod_name": name, "counter": counter, "deleted": True, "no_live_sentry": True}

    def scenario(self, endpoint, scenario):
        self.stage(scenario + "_three_real_isolated_rounds")
        scope = self.scopes[scenario]
        context = ssl.create_default_context(cafile=str(self.cert))
        directory = self.p.directory / (scenario + "-business")
        directory.mkdir()
        catalog = SkillCatalog(endpoint, self.token(scope, "--write"), context) if scenario == "skill" else None
        if catalog:
            configuration = json.loads(self.kube("get", "configmap", scenario + "-worker", "-o", "json"))["data"]
            worker = json.loads(configuration["worker.json"])
            worker["skill_pins"] = [catalog.pin]
            self.configmap(scenario + "-worker", {"worker.json": json.dumps(worker)})
        cls = SkillReportClient if catalog else FileReportClient
        options = {"catalog": catalog} if catalog else {}
        client = cls(endpoint, self.token(scope, "--submit"), scope["runtime"], context, **options)
        self.launch(scenario)
        client.submit()
        client.await_result(seconds=240)
        row = json.loads(client.request("GET", client.route))
        cp = json.loads(client.request("GET", client.route + "/checkpoint"))
        session, marker = cp["session_id"], None
        deliveries, rounds = [], []
        newer = None
        for operation in ("produce", "revise", "audit"):
            turn = ""
            if operation != "produce":
                message = "Revise the saved report by appending a west refund of 9 cents." if operation == "revise" else "Audit the saved report and the restored workspace marker."
                turn = client.ask(session, message)
                row, _ = client.answer(turn, seconds=240)
                require(row["parent"] == cp, "follow-up did not use frozen successful checkpoint")
            marker, delivery = client.observe(row, session, operation, marker)
            deliveries.append((turn, delivery))
            workspace = client.archive(client.refs(row)["workspace"])
            counter = json.loads(workspace[".agentmoor-native-count.json"])
            rounds.append({"operation": operation, **self.deleted(counter), "artifact": delivery[0]})
            if operation != "audit":
                client.ingest(row, delivery, directory, self.cert, fail_write=operation == "revise")
            cp = json.loads(client.request("GET", client.route + "/checkpoint"))
            if catalog and operation == "produce":
                newer = catalog.advance()
        require(len({round["pod_uid"] for round in rounds}) == 3 and len({round["counter"]["nonce"] for round in rounds}) == 3, "native rounds reused a Pod or execution nonce")
        client.verify_stored(deliveries, directory)
        result = {"scenario": scenario, "rounds": rounds, "session_sha256": sha(session.encode()), "business_rows": 2, "checkpoint": cp}
        if catalog:
            require(catalog.request("GET", catalog.pin["source"]["content_path"]) == catalog.original, "old Skill package changed")
            result.update(skill_pin=catalog.pin, newer_unselected_skill=newer)
        self.result["scenarios"].append(result)
        return client, cp, marker, deliveries, directory

    def execute(self, scenarios):
        self.build()
        self.configure()
        observer = threading.Thread(target=self.observe, daemon=True)
        observer.start()
        try:
            with forward(self.p.base, CONTROL_NS) as endpoint:
                for scenario in scenarios:
                    self.scenario(endpoint, scenario)
        finally:
            self.stop.set()
            observer.join(timeout=15)
            with self.lock:
                self.result["running_guest_observations"] = list(self.observed.values())
                self.result["seen_guest_pods"] = list(self.seen_pods.values())
                self.result["observer_errors"] = self.observer_errors[-10:]
        self.result["status"] = "passed"

    def diagnostics(self):
        try:
            pods = json.loads(self.kube("get", "pods", "-o", "json", namespace=GUEST_NS))
            self.result["failure_guest_pods"] = [{"pod": redact_pod(pod), "annotation_keys": sorted(pod.get("metadata", {}).get("annotations", {}))} for pod in pods["items"]]
            workers = json.loads(self.kube("get", "pods", "-l", "job-name=file-produce", "-o", "json"))["items"]
            self.result["failure_worker_pods"] = [redact_pod(pod) for pod in workers]
            for pod in workers:
                text = self.kube("logs", pod["metadata"]["name"], "--all-containers=true", "--tail=100")
                require("sbx1." not in text, "worker diagnostics contain a capability")
                self.result.setdefault("logs", {})[pod["metadata"]["name"]] = text
        except Exception as error:
            self.result.setdefault("diagnostic_errors", []).append(str(error))
        for namespace, name in ((CONTROL_NS, "agentmoor"), (CONTROL_NS, "provider-model")):
            try:
                text = self.kube("logs", name, "--all-containers=true", "--tail=100", namespace=namespace)
                require("sbx1." not in text and self.model_key.read_text() not in text, "diagnostics contain a secret")
                self.result.setdefault("logs", {})[name] = text
            except Exception as error:
                self.result.setdefault("diagnostic_errors", []).append(str(error))

    def clean(self):
        for image in (self.image, self.base_image):
            subprocess.run(["docker", "image", "rm", image], capture_output=True, text=True, timeout=60)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--calico-manifest", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--scenario", choices=("file", "skill", "both"), default="file")
    options = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="agentmoor-provider-") as directory:
        probe = SecurityProbe(Path(directory), options.bundle, options.calico_manifest)
        smoke = ProviderSmoke(probe)
        code = 1
        try:
            probe.prepare()
            probe.setup()
            smoke.execute(("file", "skill") if options.scenario == "both" else (options.scenario,))
            code = 0
        except Exception as error:
            smoke.result["status"] = "failed"
            smoke.result["failure"] = {"type": type(error).__name__, "message": str(error)}
            smoke.diagnostics()
        finally:
            try:
                probe.clean_security()
                smoke.clean()
                smoke.result["cleanup"] = probe.security["cleanup"]
            except Exception as error:
                smoke.result["status"], code = "failed", 1
                smoke.result["cleanup_error"] = str(error)
            smoke.result["runtime_setup"] = probe.result
            smoke.result["cni_setup"] = probe.security
            smoke.result["finished_at_utc"] = now()
            result, redactions = safe_evidence(smoke.result)
            if redactions:
                code = 1
            options.output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
            print("SANDBOX_PROVIDER_RESULT " + json.dumps({key: value for key, value in result.items() if key not in ("runtime_setup", "cni_setup", "running_guest_observations")}, sort_keys=True), flush=True)
        return code


if __name__ == "__main__":
    raise SystemExit(main())

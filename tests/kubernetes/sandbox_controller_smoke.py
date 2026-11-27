"""Exercise the fixed sandbox controller on real kind/gVisor using synthetic guests.

The fixture is deliberately not Codex, a model, or the production capability
gateway. Only the trusted Go driver has a namespace-scoped Kubernetes token.
"""
import argparse
from datetime import datetime, timezone
import ipaddress
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import tempfile
import time

from gvisor_probe import Probe, NAMESPACE, HANDLER, RUNTIME_TYPE, require, digest


ROOT = Path(__file__).resolve().parents[2]
GATEWAY_NAME = "controller-gateway.agentmoor.test"
EVENT = "SANDBOX_CONTROLLER_EVENT "


def now():
    return datetime.now(timezone.utc).isoformat()


def redact_pod(pod):
    row = json.loads(json.dumps(pod))
    row.get("metadata", {}).pop("annotations", None)
    # managedFields contains annotation member names only, but is irrelevant.
    row.get("metadata", {}).pop("managedFields", None)
    return row


class ControllerSmoke:
    def __init__(self, probe):
        self.probe = probe
        self.image = "agentmoor-controller-fixture:" + probe.cluster
        self.private = []
        self.driver = None
        self.result = {"status": "in_progress", "started_at_utc": now(), "scope": "fixed Pod controller component only",
                       "fixture": "synthetic Go guest and capability TLS handler; no vendor CLI or model",
                       "platform_credentials_in_guest": False, "observations": [], "limitations": [
                           "No production gateway/model proxy, real guest-run model TLS, SaaS publication or provider enablement.",
                           "No tenant egress/NetworkPolicy or resource-exhaustion acceptance; the application gate remains closed.",
                           "The controller credential is a disposable namespace-scoped ServiceAccount held only by the trusted host test."]}

    def stage(self, stage):
        self.result["stage"] = stage
        print("SANDBOX_CONTROLLER_STAGE " + json.dumps({"stage": stage, "time_utc": now()}), flush=True)

    def configure(self):
        self.stage("configure_owned_runtime_overlay")
        original = self.probe.node_run("cat", "/etc/containerd/runsc.toml")
        require(original == self.probe.result["raw"]["runsc_config"], "owned runsc configuration changed")
        configured = original + '  overlay2 = "all:memory,size=512m"\n'
        path = self.probe.directory / "controller-runsc.toml"
        path.write_text(configured)
        self.probe.run("docker", "cp", path, self.probe.node + ":/etc/containerd/runsc.toml")
        require(self.probe.node_run("cat", "/etc/containerd/runsc.toml") == configured, "runtime overlay was not installed")
        self.result["runtime_config"] = configured
        self.result["filesystem_requirement"] = "actual atomic no-replace publication on Sentry memory overlay; host emptyDir tmpfs alone is insufficient"

    def prepare(self):
        self.stage("build_synthetic_guest_image")
        relevant = ["cmd", "internal", "tests/kubernetes/controllerfixture", "tests/kubernetes/sandbox_controller_smoke.py"]
        require(not self.probe.run("git", "-C", ROOT, "status", "--short", "--", *relevant).strip(),
                "commit implementation and fixture before building acceptance image")
        self.result["source_commit"] = self.probe.run("git", "-C", ROOT, "rev-parse", "HEAD").strip()
        self.result["script_sha256"] = digest(Path(__file__), "sha256")
        self.result["source_status"] = self.probe.run("git", "-C", ROOT, "status", "--short")
        certificate = self.probe.directory / "gateway-ca.pem"
        key = self.probe.directory / "gateway-key.pem"
        self.private.append(key)
        self.probe.run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-nodes", "-days", "1",
                       "-subj", "/CN=" + GATEWAY_NAME, "-addext", "subjectAltName=DNS:" + GATEWAY_NAME,
                       "-keyout", key, "-out", certificate)
        key.chmod(0o600)
        build = self.probe.directory / "public-image-context"
        build.mkdir()
        shutil.copyfile(certificate, build / "ca.pem")
        shutil.copyfile(ROOT / "tests/kubernetes/controllerfixture/Dockerfile", build / "Dockerfile")
        environment = dict(os.environ, GOOS="linux", GOARCH={"aarch64": "arm64", "x86_64": "amd64"}[self.probe.result["architecture"]], CGO_ENABLED="0")
        log = self.probe.directory / "fixture-image-build.log"
        with log.open("w") as stream:
            subprocess.run(["go", "build", "-trimpath", "-o", str(build / "controllerfixture"),
                            "./tests/kubernetes/controllerfixture"], cwd=ROOT, env=environment, stdout=stream,
                           stderr=subprocess.STDOUT, check=True, timeout=180)
            subprocess.run(["docker", "build", "-t", self.image, str(build)], stdout=stream,
                           stderr=subprocess.STDOUT, check=True, timeout=300)
        self.result["fixture_binary_sha256"] = digest(build / "controllerfixture", "sha256")
        self.result["image_build_log_tail"] = log.read_text()[-6000:]
        self.probe.run("kind", "load", "docker-image", self.image, "--name", self.probe.cluster, timeout=180)
        reference = "docker.io/library/" + self.image
        rows = self.probe.node_run("ctr", "-n", "k8s.io", "images", "list").splitlines()
        matches = [row.split() for row in rows if row.split() and row.split()[0] == reference]
        require(len(matches) == 1 and len(matches[0]) >= 3 and re.fullmatch(r"sha256:[a-f0-9]{64}", matches[0][2]),
                "loaded image has no actual containerd manifest digest")
        self.manifest_image = "docker.io/library/agentmoor-controller-fixture@" + matches[0][2]
        self.probe.node_run("ctr", "-n", "k8s.io", "images", "tag", reference, self.manifest_image)
        self.result["image"] = self.manifest_image
        self.result["manifest_media_type"] = matches[0][1]
        addresses = self.probe.node_run("getent", "ahostsv4", "host.docker.internal")
        self.gateway_ip = str(ipaddress.IPv4Address(addresses.split()[0]))
        self.result["gateway_ip"] = self.gateway_ip
        self.certificate, self.key = certificate, key

    def rbac(self):
        self.stage("provision_minimal_controller_serviceaccount")
        objects = {"apiVersion": "v1", "kind": "List", "items": [
            {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "sandbox-controller", "namespace": NAMESPACE},
             "automountServiceAccountToken": False},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"name": "sandbox-controller", "namespace": NAMESPACE},
             "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "create", "delete"]}]},
            {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"name": "sandbox-controller", "namespace": NAMESPACE},
             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "sandbox-controller"},
             "subjects": [{"kind": "ServiceAccount", "name": "sandbox-controller", "namespace": NAMESPACE}]},
            {"apiVersion": "scheduling.k8s.io/v1", "kind": "PriorityClass", "metadata": {"name": "agentmoor-sandbox"},
             "value": 0, "globalDefault": False, "preemptionPolicy": "Never"}]}
        self.probe.kube("apply", "-f", "-", data=json.dumps(objects))
        token = self.probe.directory / "controller-token"
        token.write_text(self.probe.kube("create", "token", "sandbox-controller", "--duration=10m").strip())
        token.chmod(0o600)
        self.private.append(token)
        config = json.loads(self.probe.run(*self.probe.base, "config", "view", "--raw", "--minify", "-o", "json"))
        cluster = config["clusters"][0]["cluster"]
        import base64
        ca = self.probe.directory / "kube-ca.pem"
        ca.write_bytes(base64.b64decode(cluster["certificate-authority-data"], validate=True))
        actor = self.probe.directory / "controller-kubeconfig"
        actor.write_text(json.dumps({"apiVersion": "v1", "kind": "Config", "clusters": [{"name": "owned", "cluster": cluster}],
            "users": [{"name": "controller", "user": {"tokenFile": str(token)}}],
            "contexts": [{"name": "controller", "context": {"cluster": "owned", "user": "controller", "namespace": NAMESPACE}}],
            "current-context": "controller"}))
        actor.chmod(0o600)
        self.private.append(actor)
        observed = []
        for verb, resource, allowed in [("get", "pods", True), ("create", "pods", True), ("delete", "pods", True),
                ("list", "pods", False), ("watch", "pods", False), ("get", "pods/log", False), ("create", "pods/exec", False),
                ("get", "secrets", False), ("list", "secrets", False), ("get", "serviceaccounts", False),
                ("create", "serviceaccounts/token", False), ("create", "roles", False)]:
            process = subprocess.run(["kubectl", "--kubeconfig", str(actor), "-n", NAMESPACE, "auth", "can-i", verb, resource],
                                     text=True, capture_output=True, timeout=15)
            actual = process.stdout.strip() == "yes"
            require(actual == allowed and process.returncode == (0 if allowed else 1), "controller RBAC differs for " + verb + " " + resource)
            observed.append({"verb": verb, "resource": resource, "allowed": actual})
        self.result["rbac"] = {"role": objects["items"][1]["rules"], "observations": observed}
        self.api, self.kube_ca, self.token = cluster["server"], ca, token

    def observe(self, event):
        name, uid, mode = event["pod_name"], event["pod_uid"], event["mode"]
        require(re.fullmatch(r"sandbox-[a-f0-9]{32}", name) and mode in ("success", "cancel", "missing"), "unexpected fixture identity")
        pod = json.loads(self.probe.kube("get", "pod", name, "-o", "json"))
        require(pod["metadata"]["uid"] == uid and pod["spec"]["containers"][0]["image"] == self.manifest_image,
                "observation identifies another Pod or unpinned image")
        observation = {"mode": mode, "pod": redact_pod(pod)}
        sandboxes = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))
        matches = [row for row in sandboxes["items"] if row["metadata"]["uid"] == uid]
        if mode == "missing":
            require(not matches and pod["status"]["phase"] == "Pending", "missing handler ran or created a sandbox")
            until = time.monotonic() + 40
            while time.monotonic() < until:
                events = json.loads(self.probe.kube("get", "events", "-o", "json"))
                failures = [row for row in events["items"] if row.get("involvedObject", {}).get("uid") == uid and
                    row.get("reason") == "FailedCreatePodSandBox" and 'no runtime for "agentmoor-nonexistent-handler" is configured' in row.get("message", "")]
                if sum(row.get("count", 1) for row in failures) >= 2:
                    break
                time.sleep(.5)
            else:
                raise TimeoutError("missing controller handler has no repeated explicit failure")
            observation["missing_handler_events"] = failures
            observation["sandbox_created"] = False
        else:
            require(len(matches) == 1 and pod["status"]["phase"] == "Running", "synthetic guest was not running")
            sandbox = matches[0]
            inspected = json.loads(self.probe.node_run("crictl", "inspectp", sandbox["id"]))
            require(inspected["status"].get("runtimeHandler") == HANDLER and inspected.get("info", {}).get("runtimeType") == RUNTIME_TYPE,
                    "controller Pod did not use real runsc")
            processes = self.probe.node_run("ps", "-eo", "pid,ppid,comm,args")
            sentries = [line for line in processes.splitlines() if "runsc-sandbox " in line and
                        "--platform=systrap" in line and sandbox["id"] in line]
            require(sentries and all("--overlay2=all:memory,size=512m" in line for line in sentries), "controller Pod lacks real systrap/overlay proof")
            observation.update({"sandbox_id": sandbox["id"], "runtime_handler": HANDLER, "cri_runtime_type": RUNTIME_TYPE,
                                "platform": "systrap", "sentry_processes": sentries})
        self.result["observations"].append(observation)
        (self.probe.directory / (mode + ".release")).write_text("observed")

    def execute(self):
        self.configure()
        self.prepare()
        self.rbac()
        self.stage("run_real_catalog_controller_with_minimal_actor")
        environment = dict(os.environ, AGENTMOOR_CONTROLLER_API=self.api, AGENTMOOR_CONTROLLER_KUBE_CA=str(self.kube_ca),
            AGENTMOOR_CONTROLLER_TOKEN_FILE=str(self.token), AGENTMOOR_CONTROLLER_NAMESPACE=NAMESPACE,
            AGENTMOOR_CONTROLLER_IMAGE=self.manifest_image, AGENTMOOR_CONTROLLER_GATEWAY_IP=self.gateway_ip,
            AGENTMOOR_CONTROLLER_GATEWAY_CA=str(self.certificate), AGENTMOOR_CONTROLLER_GATEWAY_KEY=str(self.key),
            AGENTMOOR_CONTROLLER_DIRECTORY=str(self.probe.directory))
        self.driver = subprocess.Popen(["go", "test", "-tags", "integration", "-run", "^TestSandboxControllerKubernetes$", "-count=1", "-v",
             "-timeout", "10m", "./internal/sandboxkube"], cwd=ROOT, env=environment, stdout=subprocess.PIPE,
             stderr=subprocess.STDOUT, bufsize=0, start_new_session=True)
        selector = selectors.DefaultSelector()
        selector.register(self.driver.stdout, selectors.EVENT_READ)
        os.set_blocking(self.driver.stdout.fileno(), False)
        pending = b""
        until = time.monotonic() + 650
        log = self.probe.directory / "controller-driver.log"
        try:
            with log.open("w") as stream:
                while self.driver.poll() is None or selector.get_map():
                    require(time.monotonic() < until, "controller driver exceeded its bounded test duration")
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
                            if line.startswith(EVENT):
                                event = json.loads(line[len(EVENT):])
                                if event.get("stage") == "observe":
                                    self.observe(event)
                                elif event.get("stage") == "done":
                                    self.deleted(event)
                        stream.flush()
                require(self.driver.wait(timeout=5) == 0, "actual controller driver failed; inspect retained redacted log")
        finally:
            selector.close()
            if self.driver.poll() is None:
                os.killpg(self.driver.pid, signal.SIGTERM)
                try:
                    self.driver.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(self.driver.pid, signal.SIGKILL)
                    self.driver.wait(timeout=5)
            self.result["driver_log"] = log.read_text()[-12000:]
        result = json.loads((self.probe.directory / "result.json").read_text())
        require({row["mode"] for row in self.result["observations"]} == {"success", "cancel", "missing"}, "component observation matrix is incomplete")
        self.result["driver_result"] = result
        self.stage("controller_component_passed")

    def deleted(self, event):
        observation, = [row for row in self.result["observations"] if row["mode"] == event["mode"]]
        name, uid = event["pod_name"], event["pod_uid"]
        require(observation["pod"]["metadata"]["uid"] == uid, "terminal observation changed Pod UID")
        require(not self.probe.kube("get", "pod", name, "--ignore-not-found", "-o", "json").strip(),
                "controller advertised deletion before the actual Pod was gone")
        sandbox = observation.get("sandbox_id")
        until = time.monotonic() + 15
        while sandbox and time.monotonic() < until:
            processes = self.probe.node_run("ps", "-eo", "pid,ppid,comm,args")
            if not any(sandbox in row and "runsc-sandbox " in row for row in processes.splitlines()):
                break
            time.sleep(.2)
        else:
            require(not sandbox, "the deleted controller Pod still has a live Sentry")
        remaining = json.loads(self.probe.node_run("crictl", "pods", "--namespace", NAMESPACE, "-o", "json"))
        require(not any(row["metadata"]["uid"] == uid and row.get("state") == "SANDBOX_READY" for row in remaining["items"]),
                "deleted controller Pod still has a ready CRI sandbox")
        observation["after_cleanup"] = {"pod_gone": True, "live_sentry": False, "ready_cri_sandbox": False}

    def diagnostics(self):
        try:
            pods = json.loads(self.probe.kube("get", "pods", "-o", "json"))
            self.result["failure_pods"] = [redact_pod(p) for p in pods["items"]]
            self.result["failure_events"] = json.loads(self.probe.kube("get", "events", "-o", "json"))
        except Exception:
            pass

    def cleanup(self):
        try:
            if self.driver is not None and self.driver.poll() is None:
                os.killpg(self.driver.pid, signal.SIGTERM)
                try:
                    self.driver.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(self.driver.pid, signal.SIGKILL)
                    self.driver.wait(timeout=5)
            self.probe.cleanup()
        finally:
            subprocess.run(["docker", "image", "rm", self.image], capture_output=True)
            for path in self.private:
                path.unlink(missing_ok=True)
            self.result["cleanup"] = {**self.probe.result["cleanup"], "private_fixture_credentials_removed": all(not p.exists() for p in self.private)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    options = parser.parse_args()
    output = options.output.resolve()
    require(not os.path.lexists(output) and output.parent.is_dir(), "evidence output must be a new path in an existing directory")
    directory = Path(tempfile.mkdtemp(prefix="agentmoor-controller-smoke-"))
    probe = Probe(directory, options.bundle)
    smoke = ControllerSmoke(probe)
    failure = None
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        probe.execute()
        smoke.execute()
    except BaseException as error:
        failure = error
        smoke.result["failure"] = {"type": type(error).__name__, "message": str(error)}
        smoke.diagnostics()
    finally:
        try:
            smoke.cleanup()
        except BaseException as error:
            failure = failure or error
            smoke.result["cleanup_error"] = str(error)
        smoke.result.update({"status": "failed" if failure else "controller_component_passed", "finished_at_utc": now(),
                             "retained_work_directory": str(directory), "runtime_feasibility": probe.result})
        encoded = json.dumps(smoke.result, indent=2)
        encoded, redactions = re.subn(r"sbx1\.[A-Za-z0-9_-]{43}", "[redacted-capability]", encoded)
        if redactions:
            # Never persist a live delegated credential even when a future
            # test diagnostic regresses. Such an acceptance run must fail.
            failure = failure or RuntimeError("acceptance diagnostics required capability redaction")
            smoke.result["status"] = "failed"
            encoded = encoded.replace('"status": "controller_component_passed"', '"status": "failed"', 1)
        with output.open("x") as stream:
            stream.write(encoded + "\n")
        print("SANDBOX_CONTROLLER_RESULT " + json.dumps({"status": smoke.result["status"], "evidence": str(output),
               "cleanup": smoke.result.get("cleanup"), "failure": smoke.result.get("failure")}), flush=True)
    return 1 if failure else 0


if __name__ == "__main__":
    raise SystemExit(main())

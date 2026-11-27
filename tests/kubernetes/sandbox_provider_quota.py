"""Bounded real Kubernetes Pod-count quota acceptance in an owned gVisor cluster.

Only one synthetic sleep Pod executes. This proves concurrent Pod admission,
not SaaS/native parallelism, workload pressure, fairness or production capacity.
"""
import argparse
import base64
import copy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import signal
import ssl
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from gvisor_probe import IMAGE, require
from gvisor_security_probe import SecurityProbe, sha

ROOT = Path(__file__).resolve().parents[2]
QUOTA_NS = "agentmoor-quota-fixture"
QUOTA_NAME = "one-sandbox"


def now():
    return datetime.now(timezone.utc).isoformat()


def quota_pod(name, image):
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": QUOTA_NS,
            "labels": {"app.kubernetes.io/name": "agentmoor-quota-sleep"}}, "spec": {
        "runtimeClassName": "agentmoor-gvisor", "restartPolicy": "Never", "activeDeadlineSeconds": 300,
        "terminationGracePeriodSeconds": 0, "automountServiceAccountToken": False, "enableServiceLinks": False,
        "dnsPolicy": "None", "dnsConfig": {"nameservers": ["127.0.0.1"]},
        "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                            "seccompProfile": {"type": "RuntimeDefault"}},
        "containers": [{"name": "sleep", "image": image, "imagePullPolicy": "Never",
            "command": ["/bin/sh", "-c", "echo AGENTMOOR_QUOTA_SYNTHETIC; id; sleep 600"],
            "resources": {"requests": {"cpu": "50m", "memory": "128Mi"},
                          "limits": {"cpu": "200m", "memory": "128Mi", "ephemeral-storage": "16Mi"}},
            "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                "capabilities": {"drop": ["ALL"]}}}]}}


def verify_rejection(status, body, name):
    require(status == 403 and body.get("kind") == "Status" and body.get("reason") == "Forbidden" and body.get("code") == 403,
            "second Pod was not refused by an actual HTTP 403 Forbidden")
    message = body.get("message", "")
    require("exceeded quota: " + QUOTA_NAME in message and all(text in message for text in ("pods=1", "used:", "limited:")),
            "HTTP rejection does not identify the saturated Pod quota")
    require(body.get("details", {}).get("name") == name and not body.get("metadata", {}).get("uid") and
            not body.get("details", {}).get("uid"), "rejected response returned a Pod UID or another identity")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_):
        return None


class PrivateKubernetesAPI:
    """Use only the owned kubeconfig's TLS client certificate; never emit it."""
    def __init__(self, probe):
        config = json.loads(probe.run(*probe.base, "config", "view", "--raw", "--minify", "-o", "json"))
        cluster, user = config["clusters"][0]["cluster"], config["users"][0]["user"]
        origin = urllib.parse.urlsplit(cluster["server"])
        require(origin.scheme == "https" and origin.hostname in ("127.0.0.1", "localhost") and origin.port and
                not origin.path and not origin.query and not origin.fragment and origin.username is None,
                "quota actor must use the owned loopback Kubernetes API")
        paths = []
        for name, encoded in (("quota-ca.pem", cluster["certificate-authority-data"]),
                              ("quota-client.pem", user["client-certificate-data"]),
                              ("quota-client-key.pem", user["client-key-data"])):
            path = probe.directory / name
            with path.open("xb") as stream:
                os.fchmod(stream.fileno(), 0o600)
                stream.write(base64.b64decode(encoded, validate=True))
            paths.append(path)
        context = ssl.create_default_context(cafile=str(paths[0]))
        context.load_cert_chain(paths[1], paths[2])
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), NoRedirect())
        self.origin = cluster["server"]

    def request(self, method, path, body=None):
        require(path.startswith("/api/v1/namespaces/" + QUOTA_NS + "/"), "quota actor tried another API surface")
        raw = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.origin + path, data=raw, method=method,
                                         headers={"Content-Type": "application/json"})
        try:
            response = self.opener.open(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read((1 << 20) + 1)
            require(len(raw) <= 1 << 20, "Kubernetes quota response exceeds its bound")
            return response.status, json.loads(raw)


class QuotaProbe(SecurityProbe):
    def __init__(self, directory, bundle, manifest):
        super().__init__(directory, bundle, manifest, enforcing=True)
        self.image = "agentmoor-quota-fixture:" + self.cluster
        self.acceptance = {"status": "in_progress", "started_at_utc": now(), "cluster": self.cluster,
            "scope": "one synthetic sleeping gVisor Pod and an actual second-Pod quota rejection",
            "limitations": ["Not SaaS/native concurrent-task acceptance or workload-pressure testing.",
                            "No vendor CLI, model, Skill, production Gateway or application execution here.",
                            "pods=1 is a deliberately tiny isolated test quota, not a production sizing recommendation."]}

    def build_fixture(self):
        # setup() expects a public certificate/private key for its unused
        # bootstrap fixture Secret. Neither is copied into this sleep image.
        self.cert, self.key = self.directory / "bootstrap-ca.pem", self.directory / "bootstrap-key.pem"
        self.run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=quota-bootstrap.fixture",
                 "-keyout", self.key, "-out", self.cert, timeout=30)
        base = json.loads(self.run("docker", "image", "inspect", IMAGE))[0]
        require(any(value.endswith("@sha256:" + IMAGE.rsplit("@sha256:", 1)[1]) for value in base["RepoDigests"]),
                "quota base differs from the pinned BusyBox digest")
        context = self.directory / "quota-image"
        context.mkdir()
        (context / "Dockerfile").write_text("FROM " + IMAGE + "\nLABEL agentmoor.probe.owner=" + self.cluster + "\nUSER 65532:65532\n")
        self.run("docker", "build", "--network", "none", "-t", self.image, context, timeout=120)
        image = json.loads(self.run("docker", "image", "inspect", self.image))[0]
        self.acceptance["image"] = {"base": IMAGE, "base_image_id": base["Id"], "image_id": image["Id"],
                                    "contains_credentials": False}

    def wait_quota(self, used):
        until = time.monotonic() + 20
        while time.monotonic() < until:
            quota = json.loads(self.run(*self.base, "-n", QUOTA_NS, "get", "resourcequota", QUOTA_NAME, "-o", "json"))
            status = quota.get("status", {})
            if status.get("hard", {}).get("pods") == "1" and status.get("used", {}).get("pods") == str(used):
                return quota
            time.sleep(.2)
        raise TimeoutError("real quota usage did not reach the expected value")

    def execute_quota(self):
        self.prepare()
        self.setup()
        self.stage("create_restricted_quota_namespace_before_any_test_pod")
        self.apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": QUOTA_NS, "labels": {
            "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.35"}}})
        self.apply({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": {"name": "deny-all", "namespace": QUOTA_NS},
                    "spec": {"podSelector": {}, "policyTypes": ["Ingress", "Egress"], "ingress": [], "egress": []}})
        self.apply({"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": QUOTA_NAME, "namespace": QUOTA_NS},
                    "spec": {"hard": {"pods": "1"}}})
        self.acceptance["quota_before"] = self.wait_quota(0)
        reference = "docker.io/library/" + self.image
        rows = [line.split() for line in self.node_run("ctr", "-n", "k8s.io", "images", "list").splitlines()]
        loaded, = [row for row in rows if row and row[0] == reference]
        require(len(loaded) >= 3 and re.fullmatch(r"sha256:[a-f0-9]{64}", loaded[2]), "quota image has no frozen containerd manifest")
        frozen = "docker.io/library/agentmoor-quota-fixture@" + loaded[2]
        self.node_run("ctr", "-n", "k8s.io", "images", "tag", reference, frozen)
        self.acceptance["image"].update(manifest_reference=frozen, manifest_media_type=loaded[1])
        actor = PrivateKubernetesAPI(self)
        route = "/api/v1/namespaces/" + QUOTA_NS + "/pods"
        first = quota_pod("quota-first", frozen)
        self.stage("admit_one_real_synthetic_gvisor_pod")
        status, admitted = actor.request("POST", route, first)
        require(status == 201 and admitted["metadata"]["name"] == "quota-first", "first identical Pod has no successful admission control")
        uid = admitted["metadata"]["uid"]
        require(str(uuid.UUID(uid)) == uid, "admitted Pod UID is not canonical")
        self.wait_ready(QUOTA_NS, "quota-first")
        runtime = self.runtime(QUOTA_NS, "quota-first")
        require(runtime["pod_uid"] == uid and runtime["cri"]["status"]["metadata"]["uid"] == uid, "quota runtime identifies another Pod")
        pod = json.loads(self.run(*self.base, "-n", QUOTA_NS, "get", "pod", "quota-first", "-o", "json"))
        container, = pod["status"]["containerStatuses"]
        require(container.get("restartCount") == 0 and container.get("user", {}).get("linux", {}).get("uid") == 65532,
                "first quota Pod is not the stable assigned non-root process")
        pod["metadata"].pop("annotations", None)
        pod["metadata"].pop("managedFields", None)
        self.acceptance["first"] = {"admission_status": status, "pod": pod, "runtime": runtime,
                                     "log": self.run(*self.base, "-n", QUOTA_NS, "logs", "quota-first")}
        self.acceptance["quota_saturated"] = self.wait_quota(1)
        second = copy.deepcopy(first)
        second["metadata"]["name"] = "quota-second"
        require(second["spec"] == first["spec"], "second quota request differs from the admitted positive control")
        self.stage("observe_real_second_pod_http403_and_no_runtime")
        status, rejected = actor.request("POST", route, second)
        verify_rejection(status, rejected, "quota-second")
        get_status, absent = actor.request("GET", route + "/quota-second")
        require(get_status == 404 and not absent.get("metadata", {}).get("uid"), "rejected Pod became an API object")
        # Observe two scheduling intervals rather than treating one snapshot as
        # evidence that no fallback sandbox was created after the HTTP error.
        samples = []
        for _ in range(3):
            pods = json.loads(self.run(*self.base, "-n", QUOTA_NS, "get", "pods", "-o", "json"))
            cri = json.loads(self.node_run("crictl", "pods", "--namespace", QUOTA_NS, "-o", "json"))
            require([(row["metadata"]["name"], row["metadata"]["uid"]) for row in pods["items"]] == [("quota-first", uid)],
                    "quota denial created another Pod UID")
            require(all(row["metadata"]["name"] != "quota-second" for row in cri["items"]), "rejected quota Pod created a CRI sandbox")
            samples.append({"pod_names_and_uids": [(row["metadata"]["name"], row["metadata"]["uid"]) for row in pods["items"]], "cri": cri})
            time.sleep(1)
        self.acceptance["second"] = {"request_spec_matches_first": True, "http_status": status, "response": rejected,
                                     "get_status": get_status, "get_response": absent, "pod_uid": None,
                                     "cri_sandbox_created": False, "observations": samples}
        self.acceptance["quota_after_denial"] = self.wait_quota(1)
        self.stage("delete_only_admitted_pod_and_confirm_no_live_sentry")
        cleanup = {}
        physical = self.remove_pod(QUOTA_NS, "quota-first", runtime, cleanup)
        self.acceptance["first_cleanup"] = {"physical": physical, "observations": cleanup}
        self.acceptance["quota_after_deletion"] = self.wait_quota(0)


def check():
    first = quota_pod("quota-first", "fixture@sha256:" + "a" * 64)
    second = copy.deepcopy(first)
    second["metadata"]["name"] = "quota-second"
    require(first["spec"] == second["spec"] and first["spec"]["runtimeClassName"] == "agentmoor-gvisor", "quota manifests drifted")
    require(first["spec"]["automountServiceAccountToken"] is False and first["spec"]["containers"][0]["securityContext"]["capabilities"] == {"drop": ["ALL"]},
            "quota guest gained privileged identity")
    valid = {"kind": "Status", "reason": "Forbidden", "code": 403,
             "details": {"name": "quota-second"},
             "message": "exceeded quota: one-sandbox, requested: pods=1, used: pods=1, limited: pods=1"}
    verify_rejection(403, valid, "quota-second")
    for status, change in ((422, {}), (403, {"reason": "Invalid"}), (403, {"message": "admission denied"}),
                           (403, {"metadata": {"uid": str(uuid.uuid4())}}), (403, {"details": {"name": "another-pod"}})):
        try:
            verify_rejection(status, {**valid, **change}, "quota-second")
        except RuntimeError:
            pass
        else:
            raise AssertionError("invalid quota evidence was accepted")
    print("sandbox Pod quota offline checks passed; no cluster started", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--bundle", type=Path)
    parser.add_argument("--calico-manifest", type=Path)
    parser.add_argument("--output", type=Path)
    options = parser.parse_args()
    if options.check:
        check()
        return 0
    require(options.output and options.output.parent.is_dir() and not os.path.lexists(options.output), "new evidence output path required")
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    failure = None
    with tempfile.TemporaryDirectory(prefix="agentmoor-sandbox-quota-") as directory:
        probe = QuotaProbe(directory, options.bundle, options.calico_manifest)
        probe.acceptance["source"] = {"commit": probe.run("git", "-C", ROOT, "rev-parse", "HEAD").strip(),
            "status": probe.run("git", "-C", ROOT, "status", "--short"), "script_sha256": sha(__file__),
            "security_probe_sha256": sha(Path(__file__).with_name("gvisor_security_probe.py"))}
        try:
            probe.execute_quota()
        except BaseException as error:
            failure = error
            probe.acceptance["failure"] = {"type": type(error).__name__, "message": str(error)}
        finally:
            try:
                probe.clean_security()
            except BaseException as error:
                failure = failure or error
                probe.acceptance["cleanup_failure"] = {"type": type(error).__name__, "message": str(error)}
            probe.acceptance["bootstrap"] = {"runtime": probe.result, "cni_and_node": probe.security}
            probe.acceptance["finished_at_utc"] = now()
            probe.acceptance["status"] = "failed" if failure else "synthetic_pod_quota_passed"
            encoded = json.dumps(probe.acceptance, indent=2)
            require("sbx1." not in encoded, "quota evidence contains a forbidden capability marker; refusing to save")
            with options.output.open("x") as stream:
                stream.write(encoded + "\n")
            print("SANDBOX_POD_QUOTA_RESULT " + json.dumps({"status": probe.acceptance["status"], "cluster": probe.cluster,
                  "output": str(options.output), "failure": probe.acceptance.get("failure")}, sort_keys=True), flush=True)
    return int(failure is not None)


if __name__ == "__main__":
    raise SystemExit(main())

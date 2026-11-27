"""Owned-node canaries for node-loss tests; no production network configuration.

The selected guest also needs one exact host TLS input-fixture exception. This
component exception is intentionally absent from the production reference policy.
"""
import base64
import http.client
import ipaddress
import json
import re
import ssl
import time
from pathlib import Path
from urllib.parse import urlsplit

from gvisor_probe import NAMESPACE, require
from gvisor_security_probe import CONTROL_NS, TENANT_NS, GATEWAY_LABEL, GUEST_LABEL

CONTROL_PREFIX = "NODE_LOSS_CONTROL "
CONTROL_CODE = r'''
import json, os, sys, time
sys.path.insert(0, "/opt")
from fixture import connect
targets = json.loads(sys.argv[1])
started = time.monotonic_ns()
for sequence in range(1, 1201):
    rows = [connect(target) for target in targets]
    print("NODE_LOSS_CONTROL " + json.dumps({"sequence": sequence, "pid": os.getpid(),
        "wall_unix_nano": time.time_ns(), "elapsed_nano": time.monotonic_ns()-started,
        "canaries": rows}), flush=True)
    time.sleep(.5)
'''


def verify_canaries(rows, targets, control=False, require_allowed=False):
    require(len(rows) == len(targets) == 4, "incomplete live TCP canary matrix")
    by_name = {row["name"]: row for row in rows}
    require(len(by_name) == 4, "duplicate live TCP canary")
    for target in targets:
        row = by_name[target["name"]]
        require(all(row[k] == target[k] for k in ("ip", "port")), "live canary target changed")
        if control:
            require(row.get("reachable") is True, "deny target has no actually reachable control")
            if target["allowed"]:
                require(row.get("tls_verified") is True, "control gateway TLS was not verified")
        elif not target["allowed"]:
            require(row.get("reachable") is False and row.get("timeout") is True,
                    "live guest canary was allowed or failed without an actual timeout")
        elif require_allowed:
            require(row.get("reachable") is True, "allowed guest gateway is not reachable before the fault")
    return True


class OwnedAdmin:
    """TLS client only for this disposable kind API, never passed to the guest."""
    def __init__(self, smoke):
        self.smoke = smoke
        p = smoke.probe
        config = json.loads(p.run(*p.base, "config", "view", "--raw", "--minify", "-o", "json"))
        cluster = config["clusters"][0]["cluster"]
        user = config["users"][0]["user"]
        address = urlsplit(cluster["server"])
        require(address.scheme == "https" and address.hostname in ("127.0.0.1", "localhost") and not address.path and not address.query,
                "admin fixture API is not the owned local kind endpoint")
        self.host, self.port = address.hostname, address.port
        paths = []
        for name, data in (("admin-ca.pem", cluster["certificate-authority-data"]),
                           ("admin-client.pem", user["client-certificate-data"]),
                           ("admin-client-key.pem", user["client-key-data"])):
            path = p.directory / name
            path.write_bytes(base64.b64decode(data, validate=True))
            path.chmod(0o600)
            smoke.private.append(path)
            paths.append(path)
        self.context = ssl.create_default_context(cafile=str(paths[0]))
        self.context.load_cert_chain(str(paths[1]), str(paths[2]))

    def request(self, method, path, body=None, content_type="application/json"):
        self.smoke.owned()
        require(path.startswith("/api/v1/namespaces/"+NAMESPACE+"/") and not any(c in path for c in "?#%\\\r\n"), "admin path is not in the owned test namespace")
        raw = None if body is None else json.dumps(body).encode()
        connection = http.client.HTTPSConnection(self.host, self.port, context=self.context, timeout=10)
        try:
            connection.request(method, path, body=raw, headers={"Content-Type": content_type})
            response = connection.getresponse()
            data = response.read((1 << 20)+1)
            require(len(data) <= 1 << 20, "oversize fixture admin response")
            return response.status, json.loads(data) if data else None
        finally:
            connection.close()


class NodeLossNetwork:
    def __init__(self, smoke):
        self.smoke, self.probe = smoke, smoke.probe
        self.targets = []
        self.control_log = None

    def prepare(self):
        p = self.probe
        gateway = p.pod(CONTROL_NS, "node-loss-gateway", ["python3", "/opt/fixture.py", "serve", "--ports", "7443", "7401", "--nonce", p.nonce], GATEWAY_LABEL, runtime=False, memory="96Mi", deadline=900)
        gateway["spec"]["volumes"].append({"name": "tls", "secret": {"secretName": "fixture-tls"}})
        gateway["spec"]["containers"][0]["volumeMounts"].append({"name": "tls", "mountPath": "/run/tls", "readOnly": True})
        p.apply(gateway)
        service = {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "node-loss-gateway", "namespace": CONTROL_NS},
                   "spec": {"selector": GATEWAY_LABEL, "ports": [{"name": "gateway", "port": 7443}, {"name": "main-api", "port": 7401}]}}
        p.apply(service)
        p.apply(p.pod(TENANT_NS, "node-loss-tenant", ["python3", "/opt/fixture.py", "serve", "--ports", "8123", "--nonce", p.nonce], runtime=False, memory="64Mi", deadline=900))
        p.wait_ready(CONTROL_NS, "node-loss-gateway")
        p.wait_ready(TENANT_NS, "node-loss-tenant")
        service = json.loads(p.run(*p.base, "-n", CONTROL_NS, "get", "svc", "node-loss-gateway", "-o", "json"))
        gateway_ip = service["spec"]["clusterIP"]
        tenant = json.loads(p.run(*p.base, "-n", TENANT_NS, "get", "pod", "node-loss-tenant", "-o", "json"))
        api_ip = json.loads(p.run(*p.base, "-n", "default", "get", "svc", "kubernetes", "-o", "json"))["spec"]["clusterIP"]
        self.targets = [{"name": name, "ip": ip, "port": port, "allowed": allowed} for name, ip, port, allowed in (
            ("gateway-tls", gateway_ip, 7443, True), ("main-api", gateway_ip, 7401, False),
            ("kubernetes-api", api_ip, 443, False), ("other-tenant", tenant["status"]["podIP"], 8123, False))]
        for target in self.targets:
            require(ipaddress.IPv4Address(target["ip"]).is_private, "canary is not a controlled private address")
        control_targets = [{**row, **({"tls": True} if row["allowed"] else {})} for row in self.targets]
        control = p.pod(TENANT_NS, "node-loss-control", ["python3", "-u", "-c", CONTROL_CODE, json.dumps(control_targets)], runtime=False, memory="64Mi", deadline=900)
        p.apply(control)
        p.wait_ready(TENANT_NS, "node-loss-control")
        pod = json.loads(p.run(*p.base, "-n", TENANT_NS, "get", "pod", "node-loss-control", "-o", "json"))
        containers = json.loads(p.node_run("crictl", "ps", "-a", "-o", "json"))["containers"]
        sandbox, = [row for row in json.loads(p.node_run("crictl", "pods", "--namespace", TENANT_NS, "-o", "json"))["items"] if row["metadata"]["uid"] == pod["metadata"]["uid"]]
        container, = [row for row in containers if row.get("podSandboxId") == sandbox["id"] and row["metadata"]["name"] == "fixture"]
        self.control_log = json.loads(p.node_run("crictl", "inspect", container["id"]))["status"]["logPath"]
        require(self.control_log.startswith("/var/log/pods/"+TENANT_NS+"_node-loss-control_"+pod["metadata"]["uid"]+"/fixture/"), "control log is outside the owned Pod")
        gateway_pod = json.loads(p.run(*p.base, "-n", CONTROL_NS, "get", "pod", "node-loss-gateway", "-o", "json"))
        gateway_log = json.loads(p.run(*p.base, "-n", CONTROL_NS, "logs", "node-loss-gateway").splitlines()[0])
        require(gateway_log["server_ports"] == [7443, 7401] and gateway_log["fixture"] == p.nonce, "gateway dual ports differ")
        self.smoke.result["network"] = {"scope": "finite direct TCP fault-window canaries; no complete Internet guarantee",
            "targets": self.targets, "gateway_service": service, "gateway_pod_uid": gateway_pod["metadata"]["uid"],
            "gateway_process": gateway_log, "control_pod_uid": pod["metadata"]["uid"],
            "synthetic_input_exception": "one literal host address and exact ephemeral TLS port, test only"}
        self.wait_control()

    def configure(self, port):
        require(1024 <= port <= 65535, "invalid synthetic input port")
        policy = {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": {"namespace": NAMESPACE, "name": "node-loss-gateway-only"},
            "spec": {"podSelector": {"matchLabels": GUEST_LABEL}, "policyTypes": ["Ingress", "Egress"], "ingress": [], "egress": [
                {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": CONTROL_NS}}, "podSelector": {"matchLabels": GATEWAY_LABEL}}], "ports": [{"protocol": "TCP", "port": 7443}]},
                {"to": [{"ipBlock": {"cidr": self.smoke.gateway_ip+"/32"}}], "ports": [{"protocol": "TCP", "port": port}]}]}}
        self.probe.apply(policy)
        self.smoke.result["network"]["component_policy"] = policy
        # Real pre-fault probes below validate enforcement, not this delay.
        time.sleep(1)

    def wait_control(self, after=0):
        until = time.monotonic()+15
        while time.monotonic() < until:
            raw = self.probe.node_run("tail", "-n", "16", self.control_log)
            rows = [json.loads(line.split(CONTROL_PREFIX, 1)[1]) for line in raw.splitlines() if CONTROL_PREFIX in line]
            fresh = [row for row in rows if row["wall_unix_nano"] > after]
            if fresh:
                verify_canaries(fresh[-1]["canaries"], self.targets, control=True)
                return fresh[-1]
            time.sleep(.15)
        raise TimeoutError("same-target control did not emit a fresh reachable baseline")

"""Offline evidence validation; no cluster, network or resource load."""
import copy
import json
from types import SimpleNamespace
from unittest import mock
import unittest

import gvisor_security_probe as probe


class NetworkProofTest(unittest.TestCase):
    def evidence(self):
        fixed = "quay.io/calico/node@sha256:" + probe.CALICO_DIGESTS["quay.io/calico/node:v3.33.0"]
        gateway = {"pod": {"metadata": {"uid": "owned-gateway"}},
                   "service": {"spec": {"selector": probe.GATEWAY_LABEL, "ports": [{"port": 7443}, {"port": 7401}]}},
                   "endpoints": {"items": [{"endpoints": [{"targetRef": {"uid": "owned-gateway"}}]}]},
                   "log": {"process_id": 1, "server_ports": [7443, 7401], "fixture": "synthetic-nonce"}}
        result = {"cni_ready": True, "gateway_fixture": gateway,
                  "cni_pods": {"items": [{"spec": {"containers": [{"image": fixed}]},
                                          "status": {"phase": "Running", "containerStatuses": [{"ready": True, "state": {"running": {}}}]}}]},
                  "rows": []}
        ports = {"gateway-tls": 7443, "main-api": 7401, "kubernetes-api": 443, "other-tenant": 8123,
                 "metadata-fixture": 80, "outside-cidr": 8123, "dns-tcp": 53, "dns-udp": 53}
        for name, port in ports.items():
            ip = "169.254.169.254" if name == "metadata-fixture" else "10.96.0.2"
            base = {"name": name, "ip": ip, "port": port, "protocol": "UDP" if name == "dns-udp" else "TCP"}
            control = dict(base, reachable=True, response={"fixture": "synthetic-nonce", "process_id": 1})
            guest = dict(base, reachable=name == "gateway-tls", timeout=name != "gateway-tls",
                         tls_verified=name == "gateway-tls", response=control["response"].copy())
            result["rows"].append({"name": name, "control": control, "guest": guest})
        return result

    def test_complete_actual_destination_proof(self):
        self.assertTrue(probe.verify_network(self.evidence()))

    def test_policy_is_default_deny_with_one_combined_namespace_and_pod_peer(self):
        spec = probe.network_policy()["spec"]
        self.assertEqual(spec["ingress"], [])
        self.assertEqual(set(spec["policyTypes"]), {"Ingress", "Egress"})
        self.assertEqual(spec["podSelector"]["matchLabels"], probe.GUEST_LABEL)
        self.assertEqual(len(spec["egress"]), 1)
        rule = spec["egress"][0]
        self.assertEqual(rule["ports"], [{"protocol": "TCP", "port": 7443}])
        self.assertEqual(len(rule["to"]), 1)
        self.assertEqual(set(rule["to"][0]), {"namespaceSelector", "podSelector"})

    def test_missing_or_inconclusive_proof_never_launches_fixture(self):
        for name in ("cni", "different-pod", "different-process", "different-port", "different-control-ip",
                     "unreachable-control", "refused-not-denied", "reachable-deny", "missing-canary", "wrong-metadata",
                     "unverified-tls", "mutable-cni", "pending-cni"):
            with self.subTest(name=name):
                value = self.evidence()
                if name == "cni":
                    value["cni_ready"] = False
                elif name == "different-pod":
                    value["gateway_fixture"]["endpoints"]["items"][0]["endpoints"][0]["targetRef"]["uid"] = "another"
                elif name == "different-process":
                    value["rows"][1]["control"]["response"]["process_id"] = 2
                elif name == "different-port":
                    value["rows"][1]["guest"]["port"] = 7443
                elif name == "different-control-ip":
                    value["rows"][1]["control"]["ip"] = "10.96.0.3"
                elif name == "unreachable-control":
                    value["rows"][1]["control"]["reachable"] = False
                elif name == "refused-not-denied":
                    value["rows"][1]["guest"]["timeout"] = False
                elif name == "reachable-deny":
                    value["rows"][1]["guest"]["reachable"] = True
                elif name == "missing-canary":
                    value["rows"].pop()
                elif name == "wrong-metadata":
                    for side in ("guest", "control"):
                        value["rows"][4][side]["ip"] = "10.0.0.1"
                elif name == "unverified-tls":
                    value["rows"][0]["guest"]["tls_verified"] = False
                elif name == "mutable-cni":
                    value["cni_pods"]["items"][0]["spec"]["containers"][0]["image"] = "calico/node:latest"
                else:
                    value["cni_pods"]["items"][0]["status"]["phase"] = "Pending"
                calls = []
                with self.assertRaises(RuntimeError):
                    probe.guarded_fixture(value, lambda: calls.append("synthetic"))
                self.assertEqual(calls, [])


class ResourceProofTest(unittest.TestCase):
    def record(self):
        return {"runtime_verified": True, "node_swap_max": "0", "before": [{"path": "/owned-pod", "pids.max": "128",
                 "cpu.max": "20000 100000", "memory.max": str(128 << 20)}], "after": [],
                "cpu_throttled_delta": 4, "cpu_usage_usec_delta": 100000, "oom_kill_delta": 1,
                "terminated": True, "pod_reason": "DeadlineExceeded", "cancelled": True, "physical_cleanup": True,
                "fixture": {"guest_rlimit_nproc": [32, 32], "hard_raise_denied": True, "fork_errno": 11, "children": 30}}

    def test_actual_limits_and_effects_required(self):
        for mode, mutation in (("cpu", "cpu_throttled_delta"), ("memory", "oom_kill_delta"),
                               ("file-memory", "terminated"), ("pids", "hard_raise_denied"),
                               ("wall", "pod_reason"), ("cancel", "physical_cleanup")):
            with self.subTest(mode=mode):
                record = self.record()
                self.assertTrue(probe.verify_resource(mode, record))
                if mutation == "hard_raise_denied":
                    record["fixture"][mutation] = False
                else:
                    record[mutation] = False
                with self.assertRaises(RuntimeError):
                    probe.verify_resource(mode, record)
        for mode in ("cpu", "memory", "pids", "cancel"):
            value = self.record()
            value["before"][0]["pids.max"] = "max"
            with self.subTest(mode=mode), self.assertRaises(RuntimeError):
                probe.verify_resource(mode, value)

    def test_tmpfs_enospc_is_separate_from_oom(self):
        value = self.record()
        value["oom_kill_delta"] = 0
        value["fixture"] = {"write_errno": 28, "written_bytes": 64 << 20, "filesystem_capacity_bytes": 64 << 20}
        self.assertTrue(probe.verify_resource("tmpfs", value))
        for mutation in ("oom", "wrong-error", "unbounded-write"):
            changed = copy.deepcopy(value)
            if mutation == "oom":
                changed["oom_kill_delta"] = 1
            elif mutation == "wrong-error":
                changed["fixture"]["write_errno"] = 12
            else:
                changed["fixture"]["written_bytes"] = 65 << 20
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                probe.verify_resource("tmpfs", changed)

    def test_cgroup_scope_must_belong_to_the_attested_pod(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        runtime = {"sentry_pid": 123, "pod_uid": "abc-def"}
        instance.node_run = lambda *args: "0::/kubepods.slice/kubepods-podabc_def.slice/owned.scope\n"
        self.assertEqual(len(instance.cgroup_paths(runtime)), 2)
        for path in ("0::/unrelated/owned.scope\n", "0::/kubepods-podabc_def.slice/../other\n"):
            instance.node_run = lambda *args: path
            with self.assertRaises(RuntimeError):
                instance.cgroup_paths(runtime)


class CleanupTest(unittest.TestCase):
    def test_cleanup_refuses_another_owners_container(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        instance.cluster, instance.outside = "owned", "owned-outside"
        calls = []
        instance.run = lambda *args: calls.append(args)
        wrong = SimpleNamespace(returncode=0, stdout=json.dumps([{"Config": {"Labels": {"agentmoor.probe.owner": "another"}}}]))
        with mock.patch.object(probe.subprocess, "run", return_value=wrong), self.assertRaises(RuntimeError):
            instance.clean_security()
        self.assertEqual(calls, [])

    def test_only_stopped_owned_cri_records_are_removed_after_natural_exit(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        instance.base = ["kubectl-owned"]
        instance.run = lambda *args, **kwargs: ""
        runtime = {"sandbox_id": "sandbox-own", "pod_uid": "pod-own"}
        state = {"round": 0}
        calls = []
        def node(*args):
            calls.append(args)
            if args[0] == "ps":
                return "123 runsc-sandbox sandbox-own" if state["round"] == 0 else ""
            if args[:2] == ("crictl", "pods"):
                turn = state["round"]
                state["round"] += 1
                records = [] if turn >= 2 else [{"id": "sandbox-own", "metadata": {"uid": "pod-own"},
                           "state": "SANDBOX_READY" if turn == 0 else "SANDBOX_NOTREADY"}]
                return json.dumps({"items": records})
            return ""
        instance.node_run = node
        with mock.patch.object(probe.time, "sleep"):
            result = instance.remove_pod("owned-ns", "owned-pod", runtime, {})
        self.assertEqual(result["sentry_processes"], [])
        self.assertEqual([args for args in calls if args[:2] == ("crictl", "rmp")], [("crictl", "rmp", "sandbox-own")])
        self.assertFalse(any("stopp" in args or "kill" in args for args in calls))


if __name__ == "__main__":
    unittest.main()

"""Offline evidence validation; no cluster, network or resource load."""
import copy
import json
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
from unittest import mock
import unittest

import gvisor_security_probe as probe
import gvisor_security_containerd as candidate


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
        return {"runtime_verified": True, "node_swap_max": "0", "before": [{"path": "/owned-pod", "pids.max": str(probe.POD_PIDS_LIMIT),
                 "cpu.max": "20000 100000", "memory.max": str(128 << 20)}], "after": [],
                "cpu_throttled_delta": 4, "cpu_usage_usec_delta": 100000, "oom_kill_delta": 1, "host_pids_max_events_delta": 0,
                "terminated": True, "pod_reason": "DeadlineExceeded", "cancelled": True, "physical_cleanup": True,
                "fixture": {"guest_rlimit_nproc": [64, 64], "hard_raise_denied": True, "fork_errno": 11, "children": 62}}

    def test_actual_limits_and_effects_required(self):
        for mode, mutation in (("cpu", "cpu_throttled_delta"), ("memory", "oom_kill_delta"),
                               ("file-memory", "terminated"), ("pids", "hard_raise_denied"),
                               ("wall", "pod_reason"), ("cancel", "physical_cleanup")):
            with self.subTest(mode=mode):
                record = self.record()
                if mode == "pids":
                    record["oom_kill_delta"] = 0
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

    def test_guest_process_proof_cannot_be_an_earlier_host_pid_or_memory_bound(self):
        value = self.record()
        value["oom_kill_delta"] = 0
        self.assertTrue(probe.verify_resource("pids", value))
        for key in ("host_pids_max_events_delta", "oom_kill_delta"):
            changed = copy.deepcopy(value)
            changed[key] = 1
            with self.subTest(key=key), self.assertRaises(RuntimeError):
                probe.verify_resource("pids", changed)

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

    def test_one_failure_keeps_other_bounded_resource_observations_but_run_fails(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        instance.security = {"resources": {}}
        instance.stage = lambda _: None
        calls = []
        def resource(mode):
            calls.append(mode)
            if mode == "memory":
                raise RuntimeError("synthetic evidence failure")
        instance.resource = resource
        with self.assertRaisesRegex(RuntimeError, "memory"):
            instance.resources()
        self.assertEqual(calls, ["cpu", "memory", "tmpfs", "file-memory", "pids", "wall", "cancel"])
        self.assertEqual(instance.security["resources"]["memory"]["failure"]["type"], "RuntimeError")

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

    def test_debug_signals_require_explicit_mode_and_owned_shim_only(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        instance.debug_orphans = False
        with mock.patch.object(probe.subprocess, "run") as run, self.assertRaises(RuntimeError):
            instance.diagnose_orphan({}, {})
        run.assert_not_called()
        instance.debug_orphans = True
        instance.cluster, instance.node = "owned", "owned-node"
        record = {"cleanup_last_observation": {"cri_sandboxes": [], "live_processes": ["123 runsc-sandbox owned-id"]}}
        instance.run = lambda *args: json.dumps([{"Config": {"Labels": {"io.x-k8s.kind.cluster": "another"}}}])
        with mock.patch.object(probe.subprocess, "run") as run, self.assertRaises(RuntimeError):
            instance.diagnose_orphan({}, record)
        run.assert_not_called()
        instance.run = lambda *args: json.dumps([{"Config": {"Labels": {"io.x-k8s.kind.cluster": "owned"}}}])
        with mock.patch.object(probe.subprocess, "run") as run, self.assertRaises(RuntimeError):
            instance.diagnose_orphan({}, record)
        run.assert_not_called()

    def test_debug_rechecks_pid_identity_before_any_signal(self):
        instance = probe.SecurityProbe.__new__(probe.SecurityProbe)
        instance.debug_orphans, instance.cluster, instance.node = True, "owned", "owned-node"
        instance.run = lambda *args: json.dumps([{"Config": {"Labels": {"io.x-k8s.kind.cluster": "owned"}}}])
        runtime = {"sandbox_id": "sandbox-own", "pod_uid": "pod-own"}
        line = "123 1 Sl containerd-shim-runsc-v1 -id sandbox-own"
        record = {"pod_delete_started_monotonic": 0, "cleanup_last_observation": {"cri_sandboxes": [], "live_processes": [line]}}
        calls = []
        def command(argv, **kwargs):
            calls.append(argv)
            if "pods" in argv:
                out = json.dumps({"items": []})
            elif "ps" in argv:
                out = line
            elif argv[-1].endswith("cmdline"):
                out = "/bin/another\0-id\0other-sandbox\0"
            else:
                out = ""
            return SimpleNamespace(returncode=0, stdout=out, stderr="")
        with mock.patch.object(probe.subprocess, "run", side_effect=command), self.assertRaises(RuntimeError):
            instance.diagnose_orphan(runtime, record)
        self.assertFalse(any("kill" in argv for argv in calls))

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


class CandidateBundleTest(unittest.TestCase):
    def archive(self, path, extra=None, missing=False):
        with tarfile.open(path, "w:gz") as output:
            for name in candidate.BINARIES[:-1] if missing else candidate.BINARIES:
                data = b"synthetic binary bytes; never execute"
                item = tarfile.TarInfo("bin/" + name)
                item.size = len(data)
                output.addfile(item, io.BytesIO(data))
            if extra:
                item = tarfile.TarInfo(extra)
                item.type = tarfile.SYMTYPE
                item.linkname = "/outside"
                output.addfile(item)

    def test_bundle_members_are_complete_regular_and_confined(self):
        for extra, missing in ((None, False), ("bin/link", False), ("../../escape", False), (None, True)):
            with self.subTest(extra=extra, missing=missing), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = root / "official-fixture.tar.gz"
                self.archive(archive, extra, missing)
                if extra or missing:
                    with self.assertRaises(RuntimeError):
                        candidate.extract(archive, root / "expanded")
                else:
                    candidate.extract(archive, root / "expanded")
                    self.assertTrue((root / "expanded/bin/containerd").is_file())

    def test_cached_bundle_still_requires_the_fixed_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / "cache"
            cache.mkdir()
            expected = {}
            for version in candidate.VERSIONS:
                archive = cache / ("containerd-" + version + "-linux-arm64.tar.gz")
                self.archive(archive)
                expected[version] = {"arm64": hashlib.sha256(archive.read_bytes()).hexdigest()}
            instance = SimpleNamespace(directory=root / "run", cluster="owned", run=lambda *args: "arm64")
            instance.directory.mkdir()
            with mock.patch.dict(candidate.DIGESTS, expected):
                self.assertEqual([value["version"] for value in candidate.prepare(instance, cache)], ["2.3.7", "2.4.2"])
                archive = cache / "containerd-2.3.7-linux-arm64.tar.gz"
                archive.write_bytes(archive.read_bytes() + b"mutated")
                with self.assertRaisesRegex(RuntimeError, "SHA256"):
                    candidate.prepare(instance, cache)

    def test_installer_does_not_touch_an_unowned_node(self):
        instance = SimpleNamespace(cluster="owned", node="owned-node", node_run=mock.Mock(),
            run=lambda *args: json.dumps([{"Config": {"Labels": {"io.x-k8s.kind.cluster": "another"}}}]))
        with self.assertRaises(RuntimeError):
            candidate.install(instance, [])
        instance.node_run.assert_not_called()


if __name__ == "__main__":
    unittest.main()

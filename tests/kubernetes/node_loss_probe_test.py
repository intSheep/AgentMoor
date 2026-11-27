"""Offline negative controls for the physical node-loss evidence validator."""
import copy
import unittest
from unittest.mock import Mock

from node_loss_probe import NAMESPACE, NodeLossSecurityProbe, verify_orphan_window, verify_physical_cleanup_claim, verify_guest_stop_matrix


def fixture():
    heartbeat = {"nonce": "1" * 32, "pid": 2, "uid": 65532, "sequence": 10, "elapsed_nano": 100, "wall_unix_nano": 900}
    before = {"same_sentry": True, "sandbox_ready": True, "container_running": True, "heartbeat": heartbeat, "node_unix_nano": 1000, "recent_heartbeats": [heartbeat]}
    after = copy.deepcopy(before)
    after["heartbeat"].update(sequence=13, elapsed_nano=200, wall_unix_nano=1300)
    after["recent_heartbeats"] = [{**after["heartbeat"], "sequence": n, "wall_unix_nano": 100 * n} for n in (11, 12, 13)]
    service = {"ActiveState": "active", "SubState": "running", "MainPID": "12"}
    return {"metric": {"api_not_found": True, "pod_deleted": True, "capabilities_denied": True,
                "checkpoint_unchanged": True, "identity_deadline_unchanged": True},
            "kubelet": {"after": {"ActiveState": "inactive", "MainPID": "0"}, "containerd_before": service,
                        "containerd_after": dict(service), "api_ready_after_stop": "ok"},
            "post_api404_samples": [before, after]}


def fixed_fixture():
    targets = [{"name": name, "ip": ip, "port": port, "allowed": allowed} for name, ip, port, allowed in (
        ("gateway-tls", "10.96.0.2", 7443, True), ("main-api", "10.96.0.2", 7401, False),
        ("kubernetes-api", "10.96.0.1", 443, False), ("other-tenant", "10.244.0.2", 8123, False))]
    guest = [{**row, "reachable": row["allowed"], "timeout": not row["allowed"]} for row in targets]
    control = [{**row, "reachable": True, "tls_verified": row["allowed"]} for row in targets]
    orphan = fixture()
    orphan["metric"].update(pod_deleted=False, guest_stopped=False, cleanup_unknown=True, requests={"posts": 1})
    orphan["physical_cleanup"] = {"no_live_sentry_gofer_shim": True}
    window = {"metric": {"guest_stopped": False, "pod_deleted": False}, "samples": copy.deepcopy(orphan["post_api404_samples"]),
              "same_target_control": {"wall_unix_nano": 1400, "canaries": control}}
    for sample in window["samples"]:
        sample["heartbeat"]["canaries"] = copy.deepcopy(guest)
        for heartbeat in sample["recent_heartbeats"]:
            heartbeat["canaries"] = copy.deepcopy(guest)
    metric = {"api_not_found": True, "capabilities_denied": True, "checkpoint_unchanged": True,
              "identity_deadline_unchanged": True, "requests": {"posts": 1}, "pod_uid": "owned-pod", "pod_deleted": True,
              "guest_stopped": True, "cleanup_unknown": False,
              "stop_proof": {"pod_uid": "owned-pod", "kind": "terminated.v1", "node_name": "owned-node", "container_id": "containerd://"+"a"*64,
                             "container_name": "guest", "restart_count": 0, "exit_code": 143}}
    normal = {"metric": copy.deepcopy(metric), "physical_cleanup": {"no_live_sentry_gofer_shim": True}}
    pending = copy.deepcopy(normal)
    pending["metric"]["stop_proof"] = {"kind": "never-scheduled-cas.v1", "pod_uid": "owned-pod"}
    pending["binding_after_delete"] = {"status": 409}
    lost = copy.deepcopy(normal)
    lost.update(deleting_live_window=copy.deepcopy(window), phase_failed_live_window=copy.deepcopy(window),
                object_quota={"count_pods_named_in_rejection": True, "new_pod_status": 403, "after_delete_admission_status": 201, "new_witness_no_cri": True})
    return {"network": {"targets": targets}, "scenarios": {"pending": pending, "normal": normal, "lost": lost, "admin-missing": orphan}}


class NodeLossEvidenceTest(unittest.TestCase):
    def test_fixed_matrix_requires_independent_runtime_and_business_evidence(self):
        self.assertTrue(verify_guest_stop_matrix(fixed_fixture()))
        for mode, field, value in (("normal", "guest_stopped", False), ("lost", "pod_deleted", False),
                                   ("admin-missing", "cleanup_unknown", False), ("pending", "api_not_found", False)):
            row = fixed_fixture()
            row["scenarios"][mode]["metric"][field] = value
            with self.subTest(mode=mode, field=field), self.assertRaises(RuntimeError):
                verify_guest_stop_matrix(row)

    def test_phase_failed_is_not_a_stop_proof_or_an_object_quota_release(self):
        for mutate in (lambda row: row["scenarios"]["lost"]["metric"].pop("stop_proof"),
                       lambda row: row["scenarios"]["lost"]["phase_failed_live_window"]["metric"].update(guest_stopped=True),
                       lambda row: row["scenarios"]["lost"]["object_quota"].update(count_pods_named_in_rejection=False),
                       lambda row: row["scenarios"]["lost"]["object_quota"].update(after_delete_admission_status=403)):
            row = fixed_fixture()
            mutate(row)
            with self.assertRaises(RuntimeError):
                verify_guest_stop_matrix(row)

    def test_live_network_denial_needs_same_reachable_targets_and_fresh_control(self):
        for mutate in (lambda window: window["same_target_control"].update(wall_unix_nano=999),
                       lambda window: window["same_target_control"]["canaries"][1].update(reachable=False),
                       lambda window: window["same_target_control"]["canaries"][1].update(ip="10.96.9.9"),
                       lambda window: window["samples"][-1]["recent_heartbeats"][0]["canaries"][1].update(reachable=True),
                       lambda window: window["samples"][-1]["recent_heartbeats"].pop()):
            row = fixed_fixture()
            mutate(row["scenarios"]["lost"]["phase_failed_live_window"])
            with self.assertRaises(RuntimeError):
                verify_guest_stop_matrix(row)

    def test_controller_rbac_stdin_apply_is_supported_by_probe_adapter(self):
        probe = object.__new__(NodeLossSecurityProbe)
        probe.base = ["kubectl", "--context", "owned-only"]
        probe.run = Mock(return_value="created")
        self.assertEqual(probe.kube("apply", "-f", "-", data="{}"), "created")
        probe.run.assert_called_once_with(*probe.base, "-n", NAMESPACE, "apply", "-f", "-", data="{}")

    def test_real_orphan_evidence_does_not_pass_physical_cleanup(self):
        row = fixture()
        self.assertTrue(verify_orphan_window(row))
        with self.assertRaisesRegex(RuntimeError, "PHYSICAL_CLEANUP_INVARIANT"):
            verify_physical_cleanup_claim(row)

    def test_fail_closed_claim_is_distinct_from_still_live_process(self):
        row = fixture()
        row["metric"]["pod_deleted"] = False
        self.assertTrue(verify_physical_cleanup_claim(row))

    def test_stale_log_and_replacement_process_cannot_prove_old_child_liveness(self):
        for field, value in [("sequence", 12), ("elapsed_nano", 100), ("nonce", "2" * 32), ("pid", 3)]:
            with self.subTest(field=field):
                row = fixture()
                row["post_api404_samples"][-1]["heartbeat"][field] = value
                with self.assertRaises(RuntimeError):
                    verify_orphan_window(row)

    def test_api404_or_gateway_loss_alone_is_not_runtime_evidence(self):
        for field in ("same_sentry", "sandbox_ready", "container_running"):
            with self.subTest(field=field):
                row = fixture()
                row["post_api404_samples"][-1][field] = False
                with self.assertRaises(RuntimeError):
                    verify_orphan_window(row)

    def test_paused_node_or_restarted_containerd_is_not_kubelet_only_fault(self):
        for field, value in [("ActiveState", "inactive"), ("MainPID", "14")]:
            row = fixture()
            row["kubelet"]["containerd_after"][field] = value
            with self.assertRaises(RuntimeError):
                verify_orphan_window(row)
        row = fixture()
        row["kubelet"]["api_ready_after_stop"] = "unavailable"
        with self.assertRaises(RuntimeError):
            verify_orphan_window(row)


if __name__ == "__main__":
    unittest.main()

"""Offline negative controls for the physical node-loss evidence validator."""
import copy
import unittest
from unittest.mock import Mock

from node_loss_probe import NAMESPACE, NodeLossSecurityProbe, verify_orphan_window, verify_physical_cleanup_claim


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


class NodeLossEvidenceTest(unittest.TestCase):
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

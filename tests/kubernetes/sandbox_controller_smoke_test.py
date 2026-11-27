"""Offline checks for acceptance evidence, with no cluster or credential use."""
import json
import unittest

from sandbox_controller_smoke import redact_pod, safe_evidence


class EvidenceTest(unittest.TestCase):
    def test_saved_evidence_and_printed_summary_share_sanitized_failure(self):
        cap = "sbx1." + "A" * 43
        original = {"status": "failed", "failure": {"type": "RuntimeError", "message": cap}, "driver_log": cap}
        safe, count = safe_evidence(original)
        summary = {key: safe.get(key) for key in ("status", "failure", "cleanup")}
        self.assertEqual(count, 2)
        self.assertEqual(safe["status"], "failed")
        self.assertNotIn(cap, json.dumps(safe))
        self.assertNotIn(cap, json.dumps(summary))
        self.assertIn(cap, json.dumps(original))

    def test_unexpected_redaction_cannot_leave_acceptance_passing(self):
        safe, count = safe_evidence({"status": "controller_component_passed", "driver_log": "sbx1." + "B" * 43})
        self.assertEqual(count, 1)
        self.assertEqual(safe["status"], "failed")
        self.assertIn("failure", safe)

    def test_pod_annotation_values_and_managed_fields_never_enter_evidence(self):
        original = {"metadata": {"uid": "fixture", "annotations": {"cap": "sbx1." + "C" * 43}, "managedFields": [{"ignored": "value"}]}, "spec": {"runtimeClassName": "gvisor"}}
        safe = redact_pod(original)
        self.assertNotIn("annotations", safe["metadata"])
        self.assertNotIn("managedFields", safe["metadata"])
        self.assertEqual(safe["metadata"]["uid"], "fixture")
        self.assertIn("annotations", original["metadata"])


if __name__ == "__main__":
    unittest.main()

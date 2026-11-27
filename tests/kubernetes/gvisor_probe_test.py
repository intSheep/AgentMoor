"""Offline proof-validation regressions; no Docker, runtime download or cluster."""
import copy
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

import gvisor_probe as probe


class ProbeEvidenceTest(unittest.TestCase):
    def observations(self):
        positive = copy.deepcopy(probe.Probe.manifest()["items"][-2])
        negative = copy.deepcopy(probe.Probe.manifest()["items"][-1])
        positive["metadata"]["uid"] = "owned-positive"
        negative["metadata"]["uid"] = "owned-negative"
        positive["status"] = {"phase": "Running", "containerStatuses": [
            {"ready": True, "restartCount": 0, "state": {"running": {}}, "imageID": probe.IMAGE,
             "user": {"linux": {"uid": 65532, "gid": 65532}}}]}
        negative["status"] = {"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "True"}],
                              "containerStatuses": [{"started": False}]}
        inspect = {"status": {"id": "owned-sandbox", "metadata": {"uid": "owned-positive"},
                              "runtimeHandler": "runsc"}, "info": {"runtimeType": probe.RUNTIME_TYPE}}
        cri = {"items": [{"metadata": {"uid": "owned-positive"}}]}
        events = {"items": [{"involvedObject": {"uid": "owned-negative"}, "reason": "FailedCreatePodSandBox",
                             "message": 'no runtime for "' + probe.MISSING_HANDLER + '" is configured', "count": 2}]}
        return [positive, negative, inspect, cri, events,
                "123 gvisor_sentry runsc-sandbox --platform=systrap owned-sandbox",
                "AGENTMOOR_GVISOR_PROBE\nuid=65532 gid=65532\nLinux 4.19.0-gvisor\n"]

    def test_complete_independent_runtime_proof(self):
        result = probe.verify_observations(*self.observations())
        self.assertEqual(result["cri_runtime_type"], "io.containerd.runsc.v1")
        self.assertFalse(result["negative_sandbox_created"])

    def test_runtime_name_and_guest_uname_cannot_substitute_for_real_handler(self):
        for field, value in (("runtimeHandler", "runc"), ("runtimeType", "io.containerd.runc.v2")):
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                observations = self.observations()
                observations[2]["status" if field == "runtimeHandler" else "info"][field] = value
                probe.verify_observations(*observations)

    def test_systrap_process_must_belong_to_the_same_sandbox(self):
        for processes in ("runsc-sandbox --platform=systrap another-sandbox",
                          "runsc-sandbox --platform=kvm owned-sandbox", ""):
            with self.subTest(processes=processes), self.assertRaises(RuntimeError):
                observations = self.observations()
                observations[5] = processes
                probe.verify_observations(*observations)

    def test_missing_handler_pending_must_be_explicit_and_unexecuted(self):
        for mutation in ("started", "sandbox", "wrong_event", "unscheduled", "wrong_uid", "one_attempt"):
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                observations = self.observations()
                if mutation == "started":
                    observations[1]["status"]["containerStatuses"][0]["containerID"] = "containerd://unexpected"
                elif mutation == "sandbox":
                    observations[3]["items"].append({"metadata": {"uid": "owned-negative"}})
                elif mutation == "wrong_event":
                    observations[4]["items"][0]["message"] = "image pull unavailable"
                elif mutation == "unscheduled":
                    observations[1]["status"]["conditions"] = []
                elif mutation == "wrong_uid":
                    observations[4]["items"][0]["involvedObject"]["uid"] = "another-pod"
                else:
                    observations[4]["items"][0]["count"] = 1
                probe.verify_observations(*observations)

    def test_runtime_evidence_cannot_hide_removed_pod_restrictions(self):
        for mutation in ("token", "root", "privilege", "restart"):
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                observations = self.observations()
                pod = observations[0]
                if mutation == "token":
                    pod["spec"]["automountServiceAccountToken"] = True
                elif mutation == "root":
                    pod["spec"]["securityContext"]["runAsUser"] = 0
                elif mutation == "privilege":
                    pod["spec"]["containers"][0]["securityContext"]["allowPrivilegeEscalation"] = True
                else:
                    pod["status"]["containerStatuses"][0]["restartCount"] = 1
                probe.verify_observations(*observations)


class BundleTest(unittest.TestCase):
    def test_changed_cached_bundle_cannot_pass_the_pinned_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "bundle"
            archive.write_bytes(b"owned fixture before mutation")
            expected = hashlib.sha512(archive.read_bytes()).hexdigest()
            with mock.patch.dict(probe.BUNDLE_SHA512, {"aarch64": expected}):
                self.assertEqual(probe.verify_bundle(archive, "aarch64"), expected)
                archive.write_bytes(b"owned fixture changed")
                with self.assertRaisesRegex(RuntimeError, "SHA512"):
                    probe.verify_bundle(archive, "aarch64")

    def test_incomplete_or_escaping_bundle_is_rejected(self):
        for extra in (None, "gvisor-bin/../../escape"):
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = root / "bundle.tar.bz2"
                with tarfile.open(archive, "w:bz2") as stream:
                    names = list(probe.CORE_BINARIES) if extra else ["runsc"]
                    if extra:
                        names.append(extra)
                    for name in names:
                        member = tarfile.TarInfo(name)
                        member.size = 1
                        stream.addfile(member, io.BytesIO(b"x"))
                with self.assertRaises(RuntimeError):
                    probe.extract_bundle(archive, root / "unpacked")
                self.assertFalse((root / "escape").exists())

    def test_verified_bundle_keeps_sidecar_layout(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "bundle.tar.bz2"
            with tarfile.open(archive, "w:bz2") as stream:
                for name in (*probe.CORE_BINARIES, "gvisor-bin/runsc-fd-parking"):
                    member = tarfile.TarInfo(name)
                    member.size = 1
                    stream.addfile(member, io.BytesIO(b"x"))
            probe.extract_bundle(archive, root / "unpacked")
            self.assertTrue((root / "unpacked/gvisor-bin/runsc-fd-parking").is_file())


if __name__ == "__main__":
    unittest.main()

"""Real isolated Codex, PostgreSQL/S3, operator audit and API Pod replacement."""
import copy
import json
import xml.etree.ElementTree as ET

from contract import forward
from gvisor_probe import require
from gvisor_security_probe import CONTROL_NS
from persistent_contract import ACCESS, BUCKET, SECRET, PersistentContract, signed_s3
from sandbox_audit_chain import AuditSmoke
from sandbox_provider_smoke import ROOT, main as provider_main, sha


class PersistentAuditSmoke(AuditSmoke):
    def build(self):
        paths = ("tests/kubernetes/sandbox_persistent_chain.py", "tests/kubernetes/persistent.yaml",
                 "tests/kubernetes/persistent_contract.py")
        require(not self.p.run("git", "-C", ROOT, "status", "--short", "--", *paths).strip(),
                "commit persistent acceptance fixtures before running")
        super().build()
        for path in paths:
            self.result["fixture_sha256"][path] = sha((ROOT / path).read_bytes())
        self.result["persistent_backend"] = {"metadata": "postgresql", "objects": "s3-compatible-minio",
            "api_replacements": [], "fixture_transport": "Private disposable cluster HTTP S3 and PostgreSQL sslmode=disable; no production transport claim."}

    def configure_storage(self):
        self.stage("configure_postgres_s3_without_local_fallback")
        images = ("postgres:18-alpine", "pgsty/minio:RELEASE.2026-04-17T00-00-00Z")
        for index, image in enumerate(images):
            # Match the existing persistent fixture's architecture-specific
            # archive import; a locally cached multi-platform index can be partial.
            self.p.run("docker", "image", "inspect", image)
            platform = self.p.run("docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image).strip()
            archive = self.p.directory / ("persistent-service-" + str(index) + ".tar")
            self.p.run("docker", "image", "save", "--platform", platform, "-o", archive, image, timeout=180)
            self.p.run("kind", "load", "image-archive", archive, "--name", self.p.cluster, timeout=180)
        self.kube("apply", "-f", ROOT / "tests/kubernetes/persistent.yaml")
        storage = PersistentContract(self.kube, self.p.base, CONTROL_NS, forward)
        storage.initialize()
        # Database and S3 credentials exist only in the trusted API environment.
        self.apply({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "provider-storage", "namespace": CONTROL_NS},
                    "stringData": {item["name"]: item["value"] for item in storage.api_environment()}})

    def platform_environment(self):
        return [{"name": item["name"], "valueFrom": {"secretKeyRef": {"name": "provider-storage", "key": item["name"]}}}
                for item in PersistentContract.api_environment()]

    def sql(self, query):
        # Trusted fixture backend inspection; business assertions use HTTP.
        return self.kube("exec", "deployment/postgres", "--", "psql", "-U", "postgres", "-d", "agentmoor", "-At", "-c", query).strip()

    def no_local_fallback(self):
        files = json.loads(self.kube("exec", "agentmoor", "-c", "api", "--", "python3", "-c",
            "import json; from pathlib import Path; print(json.dumps([Path('/data/catalog.sqlite').exists(),Path('/data/objects').exists()]))"))
        require(files == [False, False], "API silently used local metadata or object storage")

    def text_round_complete(self, operation, client, assets, checkpoint):
        if operation == "recall":
            return
        self.stage("replace_api_after_" + operation)
        scope = self.scopes["text"]
        record = json.loads(client.request("GET", client.route))
        frozen = [(ref, assets.download(ref)) for ref in record["assets"]]
        events, _ = self.operator_pages(scope)
        self.no_local_fallback()
        old = json.loads(self.kube("get", "pod", "agentmoor", "-o", "json"))["metadata"]["uid"]
        self.kube("delete", "pod", "agentmoor", "--wait=true", "--timeout=60s")
        self.apply(copy.deepcopy(self.platform_manifest))
        self.p.wait_ready(CONTROL_NS, "agentmoor")
        new = json.loads(self.kube("get", "pod", "agentmoor", "-o", "json"))["metadata"]["uid"]
        require(new != old, "API replacement retained its previous Pod UID")
        client.endpoint.reconnect()
        require(json.loads(client.request("GET", client.route)) == record, "API restart changed the public task receipt")
        require(json.loads(client.request("GET", client.route + "/checkpoint")) == checkpoint,
                "API restart changed the frozen checkpoint")
        for ref, raw in frozen:
            require(assets.download(ref) == raw, "API restart changed or lost an exact asset version")
        after, _ = self.operator_pages(scope)
        require(after == events, "API restart lost or appended a logical audit event")
        self.no_local_fallback()
        self.result["persistent_backend"]["api_replacements"].append({"after_round": operation,
            "api_pod_uids": [old, new], "audit_events_preserved": len(events),
            "immutable_downloads_rechecked": len(frozen), "checkpoint_preserved": True,
            "receipt_preserved": True, "no_local_fallback_before_and_after": True})

    def execute(self, scenarios):
        super().execute(scenarios)
        self.stage("verify_actual_backend_and_guest_credential_boundary")
        scenario = self.result["scenarios"][0]
        require(len(self.result["persistent_backend"]["api_replacements"]) == 2,
                "both API replacement boundaries were not exercised")
        for round_ in scenario["rounds"]:
            require(round_["counter"].get("forbidden_environment_present") == [],
                    "a storage environment variable reached native guest execution")
        self.no_local_fallback()
        keys = set(self.sql("SELECT DISTINCT scope || '/objects/' || digest FROM revisions").splitlines())
        require(keys, "PostgreSQL has no published immutable revisions")
        with forward(self.p.base, CONTROL_NS, service="s3", port=9000, scheme="http") as endpoint:
            listing = ET.fromstring(signed_s3(endpoint, "GET", "/" + BUCKET, "list-type=2"))
            require(listing.findtext("{*}IsTruncated") == "false", "bounded fixture object listing was truncated")
            stored = {entry.text for entry in listing.findall("{*}Contents/{*}Key")}
        require(keys <= stored, "a published PostgreSQL revision was absent from S3")
        images = {}
        for name in ("postgres", "s3"):
            pod, = json.loads(self.kube("get", "pods", "-l", "app=" + name, "-o", "json"))["items"]
            images[name] = [{"image": c["image"], "image_id": c["imageID"]} for c in pod["status"]["containerStatuses"]]
        self.result["persistent_backend"].update({"postgres_version": self.sql("SHOW server_version"),
            "images": images, "verified_revision_objects": len(keys), "listed_s3_objects": len(stored),
            "postgres_audit_rows": int(self.sql("SELECT count(*) FROM audit_events")),
            "guest_storage_environment_absent": True, "sqlite_or_local_objects_present": False,
            "retained_service_image_caches": list(images)})
        # These synthetic credentials must also be absent from the audit feed.
        raw = json.dumps(self.result["audit_chain"]["events"], sort_keys=True)
        require(ACCESS not in raw and SECRET not in raw, "operator audit retained storage credentials")


if __name__ == "__main__":
    raise SystemExit(provider_main(PersistentAuditSmoke, ("text",), "text"))

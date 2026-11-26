"""Storage selection and queued-turn restart probes around the public SaaS contract."""
from datetime import datetime, timezone
import hashlib
import hmac
import json
import time
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

BUCKET = "agentmoor-contract"
ACCESS = "disposable-test"
SECRET = "disposable-test-password"


def signed_s3(endpoint, method, path, query=""):
    """Minimal SigV4 for the disposable fixture's empty-body bucket operations."""
    now = datetime.now(timezone.utc)
    date, stamp = now.strftime("%Y%m%d"), now.strftime("%Y%m%dT%H%M%SZ")
    host = urllib.parse.urlsplit(endpoint).netloc
    digest = hashlib.sha256(b"").hexdigest()
    headers = {"host": host, "x-amz-content-sha256": digest, "x-amz-date": stamp}
    names = ";".join(sorted(headers))
    canonical_headers = "".join(key + ":" + headers[key] + "\n" for key in sorted(headers))
    canonical = "\n".join((method, path, query, canonical_headers, names, digest))
    scope = date + "/us-east-1/s3/aws4_request"
    message = "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashlib.sha256(canonical.encode()).hexdigest()
    key = ("AWS4" + SECRET).encode()
    for part in (date, "us-east-1", "s3", "aws4_request"):
        key = hmac.new(key, part.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, message.encode(), hashlib.sha256).hexdigest()
    headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + ACCESS + "/" + scope + ", SignedHeaders=" + names + ", Signature=" + signature
    request = urllib.request.Request(endpoint + path + ("?" + query if query else ""), method=method, headers=headers)
    with urllib.request.urlopen(request, timeout=10) as response:
        return response.read()


class PersistentContract:
    def __init__(self, kube, base, namespace, forward):
        self.kube, self.base, self.namespace, self.forward = kube, base, namespace, forward
        self.restart_evidence = None

    def initialize(self):
        for deployment in ("postgres", "s3"):
            self.kube("rollout", "status", "deployment/" + deployment, "--timeout=150s")
        with self.forward(self.base, self.namespace, service="s3", port=9000, scheme="http") as endpoint:
            signed_s3(endpoint, "PUT", "/" + BUCKET)
        print("PERSISTENT_STAGE postgres_and_s3_ready_bucket_created", flush=True)

    @staticmethod
    def api_environment():
        return [
            {"name": "AGENTMOOR_POSTGRES_DSN", "value": "postgres://postgres:disposable-test-password@postgres:5432/agentmoor?sslmode=disable"},
            {"name": "AGENTMOOR_S3_ENDPOINT", "value": "s3:9000"},
            {"name": "AGENTMOOR_S3_BUCKET", "value": BUCKET},
            {"name": "AGENTMOOR_S3_HTTP", "value": "1"},
            {"name": "AWS_ACCESS_KEY_ID", "value": ACCESS},
            {"name": "AWS_SECRET_ACCESS_KEY", "value": SECRET},
        ]

    def restart_queued_turn(self, client, session, message):
        # Quota denies Job admission before the normal HTTP client submits its
        # first follow-up. It does not change task state or scheduling code.
        quota = {"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "queue-restart"},
                 "spec": {"hard": {"count/jobs.batch": "0"}}}
        self.kube("apply", "-f", "-", data=json.dumps(quota))
        until = time.monotonic() + 20
        while time.monotonic() < until:
            row = json.loads(self.kube("get", "resourcequota", "queue-restart", "-o", "json"))
            if row.get("status", {}).get("hard", {}).get("count/jobs.batch") == "0":
                break
            time.sleep(.2)
        else:
            raise TimeoutError("Job quota was not enforced")
        checkpoint = json.loads(client.request("GET", client.route + "/checkpoint"))
        turn = client.ask(session, message)
        route = client.route + "/turns/" + turn
        accepted = json.loads(client.request("GET", route))
        assert accepted["state"] == "queued" and accepted["parent"] == checkpoint
        assert not json.loads(self.kube("get", "jobs", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
        old, = json.loads(self.kube("get", "pods", "-l", "app=agentmoor", "-o", "json"))["items"]
        self.kube("rollout", "restart", "deployment/agentmoor")
        self.kube("rollout", "status", "deployment/agentmoor", "--timeout=150s")
        new, = json.loads(self.kube("get", "pods", "-l", "app=agentmoor", "-o", "json"))["items"]
        assert old["metadata"]["uid"] != new["metadata"]["uid"]
        client.endpoint.reconnect()
        restored = json.loads(client.request("GET", route))
        assert restored == accepted and restored["state"] == "queued"
        assert json.loads(client.request("GET", client.route + "/checkpoint")) == checkpoint
        question = {"session_id": session, "message": message}
        for _ in range(2):
            assert json.loads(client.request("PUT", route, question)) == accepted
        self.kube("delete", "resourcequota", "queue-restart")
        self.restart_evidence = {"turn_id": turn, "api_pod_uids": [old["metadata"]["uid"], new["metadata"]["uid"]],
                                 "accepted_state": "queued", "restored_state": "queued", "same_uuid_replays": 2}
        print("PERSISTENT_STAGE queued_turn_survived_control_plane_restart", flush=True)
        return turn

    def verify(self, scenario):
        assert self.restart_evidence is not None
        # Physical backend inspection is only evidence of backend selection;
        # all business amounts, versions and checkpoints remain HTTP assertions.
        sql = lambda query: self.kube("exec", "deployment/postgres", "--", "psql", "-U", "postgres", "-d", "agentmoor", "-At", "-c", query)
        version = sql("SHOW server_version")
        counts = [int(value) for value in sql("SELECT (SELECT count(*) FROM tasks), (SELECT count(*) FROM turns), (SELECT count(*) FROM revisions), (SELECT count(*) FROM operations)").split("|")]
        assert counts[0] >= 2 and counts[1] >= 2 and counts[2] > 0 and counts[3] > 0
        object_keys = set(sql("SELECT DISTINCT scope || '/objects/' || digest FROM revisions").splitlines())
        with self.forward(self.base, self.namespace, service="s3", port=9000, scheme="http") as endpoint:
            listing = ET.fromstring(signed_s3(endpoint, "GET", "/" + BUCKET, "list-type=2"))
            assert listing.findtext("{*}IsTruncated") == "false"
            stored = {entry.text for entry in listing.findall("{*}Contents/{*}Key")}
        assert object_keys and object_keys <= stored
        fallback = self.kube("exec", "deployment/agentmoor", "--", "python3", "-c",
                             "import json; from pathlib import Path; print(json.dumps([Path('/store/catalog.sqlite').exists(), Path('/store/objects').exists()]))")
        assert json.loads(fallback) == [False, False], "storage fallback detected"
        images = {}
        for label in ("postgres", "s3"):
            pod, = json.loads(self.kube("get", "pods", "-l", "app=" + label, "-o", "json"))["items"]
            images[label] = [{"image": item["image"], "image_id": item["imageID"]} for item in pod["status"]["containerStatuses"]]
        evidence = {"scenario": scenario, "metadata_backend": "postgresql", "postgres_version": version,
                    "object_backend": "s3-compatible-minio", "minio_version": self.kube("exec", "deployment/s3", "--", "minio", "--version").splitlines()[0],
                    "images": images, "postgres_rows": dict(zip(("tasks", "turns", "revisions", "operations"), counts)),
                    "verified_object_count": len(object_keys), "sqlite_or_local_objects_present": False,
                    "queued_restart": self.restart_evidence,
                    "limitations": ["No abrupt Worker Pod-loss combination on PostgreSQL/S3; separate P0 acceptance covers it.", "No live model; fixed real Codex CLI uses a scripted Responses endpoint."]}
        print("PERSISTENT_RESULT " + json.dumps(evidence, sort_keys=True), flush=True)

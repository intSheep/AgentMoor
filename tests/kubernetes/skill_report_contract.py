"""Public API Skill seeding and three-Pod native loading acceptance."""
import hashlib
import io
import json
import secrets
import tarfile
import time
import urllib.parse
import urllib.request

from diagnostics import dump_failure_logs
from saas_file_contract import FileReportClient


class SkillCatalog:
    def __init__(self, endpoint, token, context):
        self.endpoint, self.token, self.context = endpoint, token, context
        self.key = "catalog/skills/pinned-report"
        self.policy = "policy-" + secrets.token_hex(16)
        self.original, self.members = self.package(self.policy)
        self.pin = {"name": "pinned-report", "source": self.publish(self.original, 0)}

    def request(self, method, path, body=None, raw=False):
        data = body if raw else json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.endpoint + path, data=data, method=method,
                                        headers={"Authorization": "Bearer " + self.token,
                                                 "Content-Type": "application/octet-stream" if raw else "application/json"})
        with urllib.request.urlopen(request, context=self.context, timeout=10) as response:
            return response.read()

    @staticmethod
    def package(policy):
        manifest = ("---\nname: pinned-report\ndescription: Required regional report policy.\n---\n"
                    "For every report.render invocation, pass the policy argument below exactly.\n"
                    f"SKILL_POLICY={policy}\n"
                    "The installed scripts/check.py and references/rule.json are required support files.\n").encode()
        script = ("import json\nfrom pathlib import Path\nimport sys\n"
                  "rule = json.loads((Path(__file__).resolve().parents[1] / 'references/rule.json').read_bytes())\n"
                  "if len(sys.argv) != 2 or sys.argv[1] != rule['policy']:\n    sys.exit('wrong Skill policy')\n"
                  "print('verified-skill-policy')\n").encode()
        members = {"SKILL.md": manifest, "scripts/check.py": script,
                   "references/rule.json": (json.dumps({"policy": policy}, sort_keys=True) + "\n").encode()}
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w") as archive:
            for name, data in members.items():
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode = len(data), 0o600
                archive.addfile(entry, io.BytesIO(data))
        return stream.getvalue(), members

    def publish(self, raw, base):
        digest = hashlib.sha256(raw).hexdigest()
        metadata = {"kind": "skill", "id": self.key, "base_version": base,
                    "adapter_version": "fixture-skill-publisher.v1", "runtime_version": "synthetic",
                    "format": "skill-tar.v1", "media_type": "application/x-tar", "name": "pinned-report",
                    "declared_version": str(base + 1), "origin": "platform-managed",
                    "completeness": "persisted-files", "sha256": digest, "size": len(raw)}
        ticket = json.loads(self.request("POST", "/v1/changes", metadata))
        route = "/v1/changes/" + ticket["id"]
        if ticket.get("upload"):
            self.request("PUT", route + "/content", raw, raw=True)
        committed = json.loads(self.request("POST", route + "/commit"))
        version = committed["version"]
        return {"kind": "skill", "id": self.key, "version": version, "sha256": digest,
                "content_path": "/v1/content?" + urllib.parse.urlencode({"id": self.key, "kind": "skill", "version": version})}

    def advance(self):
        updated, _ = self.package("policy-v2-" + secrets.token_hex(16))
        newer = self.publish(updated, 1)
        assert newer["version"] == 2 and newer["sha256"] != self.pin["source"]["sha256"]
        assert self.request("GET", self.pin["source"]["content_path"]) == self.original
        return newer


class SkillReportClient(FileReportClient):
    def __init__(self, *args, catalog, **kwargs):
        super().__init__(*args, **kwargs)
        self.catalog = catalog

    def observe(self, row, session, operation, marker=None):
        assert row["skills"] == [self.catalog.pin]
        if row.get("parent"):
            assert row["parent"]["skills"] == [self.catalog.pin]
        result = super().observe(row, session, operation, marker, policy=self.catalog.policy)
        prefix = "turns/" + row["turn_id"] + "/" if row.get("turn_id") else ""
        proof_ref, = [ref for ref in row["assets"] if ref["id"] == prefix + "out/skill-proof.json"]
        proof = json.loads(self.download(proof_ref))
        assert proof == {"policy": self.catalog.policy, "operation": operation,
                         "manifest_sha256": hashlib.sha256(self.catalog.members["SKILL.md"]).hexdigest(),
                         "script_sha256": hashlib.sha256(self.catalog.members["scripts/check.py"]).hexdigest(),
                         "reference_sha256": hashlib.sha256(self.catalog.members["references/rule.json"]).hexdigest()}
        return result


def run(client, catalog, kube, temp, cert, wait_job, negative, queued_restart=None):
    def retire(name, failed=False):
        if not failed:
            wait_job(kube, name)
        pods = json.loads(kube("get", "pods", "-l", "job-name=" + name, "-o", "json"))["items"]
        pod, = pods
        assert any(v["name"] == "private-work" and "emptyDir" in v for v in pod["spec"]["volumes"])
        uid = pod["metadata"]["uid"]
        kube("delete", "job", name, "--cascade=foreground", "--wait=true")
        assert not json.loads(kube("get", "pods", "-l", "job-name=" + name, "-o", "json"))["items"]
        return uid

    try:
        client.submit()
        client.await_result()
        uids = [retire("produce")]
        first = json.loads(client.request("GET", client.route))
        cp = json.loads(client.request("GET", client.route + "/checkpoint"))
        assert cp["skills"] == [catalog.pin]
        session = cp["session_id"]
        marker, first_delivery = client.observe(first, session, "produce")
        client.ingest(first, first_delivery, temp, cert)
        deliveries = [("", first_delivery)]
        newer = catalog.advance()
        for operation, message in (("revise", "Revise the saved report by appending a west refund of 9 cents."),
                                   ("audit", "Audit the saved report and the restored workspace marker.")):
            turn = queued_restart(client, session, message) if queued_restart and operation == "revise" else client.ask(session, message)
            row, _ = client.answer(turn)
            assert row["parent"] == cp
            jobs = json.loads(kube("get", "jobs", "-l", "agentmoor.io/turn=" + turn, "-o", "json"))["items"]
            job, = jobs
            uids.append(retire(job["metadata"]["name"]))
            _, delivery = client.observe(row, session, operation, marker)
            deliveries.append((turn, delivery))
            if operation == "revise":
                client.ingest(row, delivery, temp, cert, fail_write=True)
            cp = json.loads(client.request("GET", client.route + "/checkpoint"))
            assert cp["session_id"] == session and cp["skills"] == [catalog.pin]
        assert len(set(uids)) == 3
        client.ingest(first, first_delivery, temp, cert, existing=True)
        client.verify_stored(deliveries, temp)
        assert catalog.request("GET", catalog.pin["source"]["content_path"]) == catalog.original
        missing = FileReportClient(client.endpoint, negative[0], "cancel-job", client.ssl_context)
        # The same valid CSV/instruction reaches the same tool without a Skill.
        # Native injection cannot provide the policy and the required tool fails.
        missing.amount = client.amount
        missing.spec = dict(client.spec)
        missing.submit()
        negative[1]()
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            row = json.loads(missing.request("GET", missing.route))
            if row["state"] not in {"queued", "running"}:
                break
            time.sleep(.3)
        assert row["state"] == "failed" and not row.get("assets") and not row.get("skills")
        retire("no-skill", failed=True)
        print("SKILL_REPORT_RESULT " + json.dumps({
            "cli": "codex-cli 0.171.0-alpha.13", "model": "scripted-responses",
            "pod_uids": uids, "session_sha256": hashlib.sha256(session.encode()).hexdigest(),
            "skill_pin": catalog.pin, "newer_unselected_skill": newer,
            "member_sha256": {name: hashlib.sha256(raw).hexdigest() for name, raw in catalog.members.items()},
            "artifacts": [ref for _, (ref, _) in deliveries], "business_rows": 2,
            "no_skill_control": "failed-without-assets", "native_skill_rule": "verified-by-installed-script-and-reference",
        }, sort_keys=True), flush=True)
    except Exception:
        dump_failure_logs(kube)
        raise

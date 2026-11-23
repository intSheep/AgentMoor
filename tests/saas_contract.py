"""Black-box SaaS consumer: HTTP only. No filesystem access to the Agent or its store."""
import hashlib
import json
import secrets
import time
import uuid
import urllib.error
import urllib.request


class ReportClient:
    def __init__(self, endpoint, token, task_id, ssl_context=None):
        self.endpoint = endpoint
        self.token = token
        self.route = "/v1/tasks/" + task_id
        self.ssl_context = ssl_context
        # Negative adjustments and repeated regions exercise business acceptance.
        self.region = "east-" + secrets.token_hex(4)
        self.amount = 100 + secrets.randbelow(20000)
        self.spec = {
            "instruction": "Group region,amount_cents CSV by region and sum integer cents. "
                           "Return only a JSON object with totals_cents mapping each region to its total.",
            "input": f"region,amount_cents\n{self.region},{self.amount}\nwest,31\n{self.region},-7\n",
            "output": "report.json",
        }

    def request(self, method, path, body=None):
        encoded = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.endpoint + path, data=encoded, method=method,
            headers={"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=10, context=self.ssl_context) as response:
            return response.read()

    def submit(self):
        for _ in range(2):
            row = json.loads(self.request("PUT", self.route, self.spec))
            assert row["spec"] == self.spec
        try:
            self.request("PUT", self.route, {**self.spec, "input": "different input"})
        except urllib.error.HTTPError as exc:
            if exc.code != 409:
                raise
        else:
            raise AssertionError("conflicting submission replaced task")

    def await_result(self, seconds=180):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            row = json.loads(self.request("GET", self.route))
            if row["state"] == "succeeded":
                return self.verify(row)
            if row["state"] not in ("running", "queued"):
                raise AssertionError(f"unexpected task state {row['state']}")
            time.sleep(0.4)
        raise TimeoutError("task never published a result")

    def verify(self, row):
        refs = {r["kind"]: r for r in row["assets"]}
        assert set(refs) == {"artifact", "session", "workspace"}
        receipt = refs["artifact"]
        report = self.request("GET", receipt["content_path"])
        assert hashlib.sha256(report).hexdigest() == receipt["sha256"]
        assert json.loads(report) == {"totals_cents": {self.region: self.amount - 7, "west": 31}}
        return receipt

    def ask(self, session, message):
        turn = str(uuid.uuid4())
        route = self.route + "/turns/" + turn
        question = {"session_id": session, "message": message}
        for _ in range(2):
            row = json.loads(self.request("PUT", route, question))
            assert row["turn_id"] == turn and row["parent"]["session_id"] == session
        return turn

    def answer(self, turn, seconds=180):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            row = json.loads(self.request("GET", self.route + "/turns/" + turn))
            if row["state"] == "succeeded":
                refs = {r["kind"]: r for r in row["assets"]}
                assert set(refs) == {"workspace", "session", "artifact"}
                receipt = refs["artifact"]
                answer = self.request("GET", receipt["content_path"])
                assert hashlib.sha256(answer).hexdigest() == receipt["sha256"]
                return row, json.loads(answer)
            if row["state"] not in ("queued", "running"):
                raise AssertionError(f"follow-up {turn} ended as {row['state']}")
            time.sleep(0.4)
        raise TimeoutError("follow-up did not reply")

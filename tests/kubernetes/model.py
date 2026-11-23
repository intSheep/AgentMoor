"""Deterministic test-only model. Compute exclusively from the current API request."""
import csv
import io
import json
import time
import re
from http.server import ThreadingHTTPServer
from fixture_provider import Handler


class ReportModel(Handler):
    def answer_for(self, body):
        nonce = None
        original = None
        check = None
        for msg in body.get("messages", body.get("input", body.get("contents", []))):
            if msg.get("role") != "user":
                continue
            content = msg.get("content", msg.get("parts", []))
            blocks = [content] if isinstance(content, str) else [b.get("text", "") for b in content]
            for text in blocks:
                if text.strip() == "Hold this follow-up until cancelled.":
                    print("holding follow-up request", flush=True)
                    time.sleep(60)
                if text.strip() == "Generate a title for this conversation:":
                    return "Regional report"
                match = re.fullmatch(r"Repeat the totals from the original CSV\. Confirmation ([0-9a-f]{24})", text.strip())
                if match:
                    nonce = match[1]
                followup = re.fullmatch(r"Recall the last confirmation and totals\. Check ([0-9a-f]{24})", text.strip())
                if followup:
                    check = followup[1]
                try:
                    spec = json.loads(text)
                except (ValueError, TypeError):
                    continue
                if not isinstance(spec, dict) or "input" not in spec:
                    continue
                if spec.get("instruction") == "Wait for cancellation in the contract test.":
                    print("holding native request", flush=True)
                    time.sleep(60)
                original = spec["input"]
        if original is not None:
            totals = {}
            for row in csv.DictReader(io.StringIO(original)):
                region = row["region"]
                totals[region] = totals.get(region, 0) + int(row["amount_cents"])
            result = {"totals_cents": totals}
            if nonce:
                result["confirmation"] = nonce
            if check:
                result["check"] = check
            return json.dumps(result, sort_keys=True)
        return "Missing original CSV in this request."


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), ReportModel).serve_forever()

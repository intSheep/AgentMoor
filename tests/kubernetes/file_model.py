"""Deterministic Responses fixture. It can request a tool but cannot access its files."""
import json
from http.server import ThreadingHTTPServer
import uuid

from fixture_provider import Handler


class FileModel(Handler):
    def do_POST(self):
        if self.path != "/v1/responses":
            return self.send_error(404)
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        history = body["input"]
        users = [json.dumps(msg.get("content")) for msg in history if msg.get("role") == "user"]
        revision = any("Revise the saved report" in msg for msg in users)
        if not any("Produce the file report" in msg for msg in users):
            return self.send_error(400, "missing original session history")
        call = "call_revise" if revision else "call_produce"
        returned = [msg for msg in history if msg.get("type") == "function_call_output" and msg.get("call_id") == call]
        if returned:
            if "written" not in str(returned[0]["output"]):
                print("tool did not confirm file creation", flush=True)
                return self.send_error(400, "render failed")
            # Valid business JSON with exactly one deliberately incorrect amount.
            text = json.dumps({"totals_cents": {"east": -999, "west": 22 if revision else 31}})
            item = {"type": "message", "role": "assistant", "id": "msg_" + call,
                    "content": [{"type": "output_text", "text": text, "annotations": []}]}
        else:
            if not any(t.get("type") == "namespace" and t.get("name") == "mcp__report"
                       and any(f.get("name") == "render" for f in t["tools"]) for t in body["tools"]):
                return self.send_error(400, "Codex did not advertise the report tool")
            item = {"type": "function_call", "id": "fc_" + call, "call_id": call,
                    "namespace": "mcp__report", "name": "render",
                    "arguments": json.dumps({"adjust_west": -9 if revision else 0})}
        item["status"] = "completed"
        response = {"id": "resp_" + uuid.uuid4().hex, "object": "response", "created_at": 1,
                    "model": body["model"], "status": "completed", "output": [item]}
        events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
                  ("response.output_item.done", {"output_index": 0, "item": item}),
                  ("response.completed", {"response": response})]
        self.events([(name, {"type": name, "sequence_number": i, **value})
                     for i, (name, value) in enumerate(events)], named=True)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), FileModel).serve_forever()

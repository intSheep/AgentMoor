"""Responses fixture requests real tool RPCs; it never reads or computes report data."""
import json
from http.server import ThreadingHTTPServer
import uuid

from fixture_provider import Handler


class FileModel(Handler):
    def do_POST(self):
        if self.path != "/v1/responses":
            return self.send_error(404)
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        # A random value generated inside the tool must stay off the model wire.
        if "workspace-only-" in json.dumps(body):
            return self.send_error(400, "workspace marker leaked to model")
        history = body["input"]
        users = [str(msg.get("content")) for msg in history if msg.get("role") == "user"]
        latest = users[-1] if users else ""
        operation = next((op for text, op in (("Lose the active report Pod", "pod_loss"),
                                              ("Hold the report tool", "hold"),
                                              ("Simulate missing restored workspace", "lost_workspace"),
                                              ("Audit the saved report", "audit"),
                                              ("Revise the saved report", "revise"),
                                              ("Missing input report", "missing_input"))
                          if text in latest), "produce")
        if operation != "missing_input" and not any("Produce the file report" in msg for msg in users):
            return self.send_error(400, "original native history missing")
        call = "call_" + operation + "_" + str(len(users))
        returned = [msg for msg in history if msg.get("type") == "function_call_output"
                    and msg.get("call_id") == call]
        if returned:
            confirmed = "written" in str(returned[-1]["output"])
            print("FILE_MODEL " + operation + (" written" if confirmed else " failed"), flush=True)
            item = {"type": "message", "role": "assistant", "id": "msg_" + call,
                    "content": [{"type": "output_text", "text": "done", "annotations": []}]}
        else:
            if not any(t.get("type") == "namespace" and t.get("name") == "mcp__report"
                       and any(f.get("name") == "render" for f in t["tools"]) for t in body["tools"]):
                return self.send_error(400, "Codex did not advertise report.render")
            item = {"type": "function_call", "id": "fc_" + call, "call_id": call,
                    "namespace": "mcp__report", "name": "render",
                    "arguments": json.dumps({"operation": operation})}
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

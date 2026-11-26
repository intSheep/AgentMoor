"""Responses fixture derives its policy only from the current native Skill injection."""
import json
import re
from http.server import ThreadingHTTPServer
import uuid

from fixture_provider import Handler


def user_text(value):
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "\n".join(user_text(item) for item in value)
    return value.get("text", "") if isinstance(value, dict) else ""


class SkillModel(Handler):
    def do_POST(self):
        if self.path != "/v1/responses":
            return self.send_error(404)
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "workspace-only-" in json.dumps(body):
            return self.send_error(400, "workspace marker leaked to model")
        history = body["input"]
        users = [user_text(item.get("content", "")) for item in history if item.get("role") == "user"]
        prompts = [text for text in users if "<skill>" not in text and any(
            instruction in text for instruction in ("Produce the file report", "Revise the saved report", "Audit the saved report"))]
        latest = prompts[-1] if prompts else ""
        operation = "audit" if "Audit the saved report" in latest else "revise" if "Revise the saved report" in latest else "produce"
        mentions = re.findall(r"\[\$pinned-report\]\(([^)]+)\)", latest)
        selected_path = mentions[-1] if mentions else None
        policies = []
        for text in users:
            if "<skill>" in text and selected_path and "<path>" + selected_path + "</path>" in text:
                policies.extend(re.findall(r"SKILL_POLICY=([a-z0-9-]+)", text))
        # A missing native injection still requests a real tool call, whose
        # explicit failure must not become a successful report merely via done.
        policy = policies[-1] if policies else "missing-native-skill"
        call = "call_skill_" + operation + "_" + str(len(prompts))
        returned = [item for item in history if item.get("type") == "function_call_output" and item.get("call_id") == call]
        if returned:
            confirmed = "written" in str(returned[-1]["output"])
            print("SKILL_MODEL " + operation + (" written" if confirmed else " failed"), flush=True)
            item = {"type": "message", "role": "assistant", "id": "msg_" + call,
                    "content": [{"type": "output_text", "text": "done", "annotations": []}]}
        else:
            if not any(tool.get("type") == "namespace" and tool.get("name") == "mcp__report"
                       and any(fn.get("name") == "render" for fn in tool["tools"]) for tool in body["tools"]):
                return self.send_error(400, "report.render missing")
            item = {"type": "function_call", "id": "fc_" + call, "call_id": call,
                    "namespace": "mcp__report", "name": "render",
                    "arguments": json.dumps({"operation": operation, "policy": policy})}
        item["status"] = "completed"
        response = {"id": "resp_" + uuid.uuid4().hex, "object": "response", "created_at": 1,
                    "model": body["model"], "status": "completed", "output": [item]}
        events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
                  ("response.output_item.done", {"output_index": 0, "item": item}),
                  ("response.completed", {"response": response})]
        self.events([(name, {"type": name, "sequence_number": i, **value})
                     for i, (name, value) in enumerate(events)], named=True)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), SkillModel).serve_forever()

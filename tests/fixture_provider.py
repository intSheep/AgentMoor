"""Local deterministic provider used by native CLI acceptance tests."""
import json
import uuid
from http.server import BaseHTTPRequestHandler


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send_data(self, data, content_type="application/json"):
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def json_result(self, value):
        self.send_data(json.dumps(value).encode())

    def events(self, items, named=False, done=False):
        lines = []
        for event_type, item in items:
            if named:
                lines.append("event: " + event_type)
            lines.extend(["data: " + json.dumps(item), ""])
        if done:
            lines.extend(["data: [DONE]", ""])
        self.send_data(("\n".join(lines) + "\n").encode(), "text/event-stream")

    def do_GET(self):
        self.json_result({"object": "list", "data": [], "models": []})

    def answer_for(self, body):
        text = json.dumps(body, ensure_ascii=False)
        state = self.server.evidence
        if state["nonce"] in text:
            state["first_seen"] = True
            if state["followup"] in text:
                state["restored_history_seen"] = True
        return "History confirmed." if state["restored_history_seen"] else "Acknowledged."

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        ident = uuid.uuid4().hex
        path = self.path.split("?")[0]
        if path.endswith("/count_tokens"):
            return self.json_result({"input_tokens": 10})
        answer = self.answer_for(body)
        if "GenerateContent" in path or "generateContent" in path:
            item = {"candidates": [{"index": 0, "content": {"role": "model", "parts": [{"text": answer}]},
                                    "finishReason": "STOP"}],
                    "usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 3, "totalTokenCount": 13}}
            if "stream" in path:
                return self.events([("", item)])
            return self.json_result(item)
        if path.endswith("/messages"):
            usage = {"input_tokens": 10, "output_tokens": 3}
            msg = {"id": "msg_" + ident, "type": "message", "role": "assistant", "model": body["model"],
                   "content": [], "stop_reason": None, "stop_sequence": None, "usage": usage}
            if not body.get("stream"):
                return self.json_result({**msg, "content": [{"type": "text", "text": answer}], "stop_reason": "end_turn"})
            events = [("message_start", {"message": msg}),
                      ("content_block_start", {"index": 0, "content_block": {"type": "text", "text": ""}}),
                      ("content_block_delta", {"index": 0, "delta": {"type": "text_delta", "text": answer}}),
                      ("content_block_stop", {"index": 0}),
                      ("message_delta", {"delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": usage}),
                      ("message_stop", {})]
            return self.events([(name, {"type": name, **data}) for name, data in events], named=True)
        if path.endswith("/responses"):
            msg = {"type": "message", "id": "msg_" + ident, "role": "assistant", "status": "completed",
                   "content": [{"type": "output_text", "text": answer, "annotations": []}]}
            response = {"id": "resp_" + ident, "object": "response", "created_at": 1, "status": "completed",
                        "model": body["model"], "output": [msg],
                        "usage": {"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}
            if not body.get("stream"):
                return self.json_result(response)
            events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
                      ("response.output_item.added", {"output_index": 0, "item": {**msg, "content": [], "status": "in_progress"}}),
                      ("response.output_text.delta", {"output_index": 0, "content_index": 0, "item_id": msg["id"], "delta": answer}),
                      ("response.output_item.done", {"output_index": 0, "item": msg}),
                      ("response.completed", {"response": response})]
            return self.events([(name, {"type": name, "sequence_number": i, **data}) for i, (name, data) in enumerate(events)], named=True)
        if path.endswith("/chat/completions"):
            common = {"id": ident, "object": "chat.completion.chunk", "model": body["model"], "created": 1}
            chunks = [{"index": 0, "delta": {"role": "assistant", "content": answer}, "finish_reason": None},
                      {"index": 0, "delta": {}, "finish_reason": "stop"}]
            return self.events([("", {**common, "choices": [x]}) for x in chunks], done=True)
        self.send_error(404)

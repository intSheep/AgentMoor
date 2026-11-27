"""Private HTTPS scripted model for the real sandbox-provider acceptance.

The existing file/Skill models request actual native MCP calls and never read
or compute the report input. Only the trusted gateway knows this listener's
synthetic upstream key; it is unrelated to the guest's delegated capability.
"""
import argparse
import hmac
import io
import json
import os
from pathlib import Path
import ssl
import stat
import sys
import uuid
from http.server import ThreadingHTTPServer

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
from file_model import FileModel
from fixture_provider import Handler
from model import ReportModel
from skill_model import SkillModel

MAX_REQUEST_BYTES = 2 << 20


def shared_key(path):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NONBLOCK), "rb") as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ValueError("model fixture requires a regular private key file")
        raw = source.read(4097)
    if len(raw) > 4096:
        raise ValueError("model fixture key exceeds its bound")
    key = raw.strip()
    if not key or any(value < 33 or value > 126 for value in key):
        raise ValueError("model fixture key is invalid")
    return key.decode("ascii")


def skill_request(body):
    """Select the existing Skill fixture from native wire data, not task IDs.

    The advertised policy argument also selects SkillModel for the no-Skill
    negative control, where the required native Skill injection is absent.
    """
    wire = json.dumps(body.get("input", []), ensure_ascii=False)
    if "[$pinned-report](" in wire or "SKILL_POLICY=" in wire:
        return True
    for namespace in body.get("tools", []):
        if namespace.get("type") != "namespace" or namespace.get("name") != "mcp__report":
            continue
        for tool in namespace.get("tools", []):
            parameters = tool.get("parameters", tool.get("inputSchema", {}))
            if tool.get("name") == "render" and "policy" in parameters.get("properties", {}):
                return True
    return False


def text_request(body):
    # Only this known synthetic text spec selects the text fixture. A missing
    # report MCP namespace must still fail the file/Skill fixture as before.
    for message in body.get("input", []):
        if not isinstance(message, dict) or message.get("role") != "user":
            continue
        content = message.get("content", [])
        blocks = [content] if isinstance(content, str) else content
        for block in blocks:
            value = block if isinstance(block, str) else block.get("text", "") if isinstance(block, dict) else ""
            try:
                spec = json.loads(value)
            except (ValueError, TypeError):
                continue
            if isinstance(spec, dict) and isinstance(spec.get("instruction"), str) and spec["instruction"].startswith("Group region,amount_cents CSV") and isinstance(spec.get("input"), str) and spec["input"].startswith("region,amount_cents\n"):
                return True
    return False


class TextModel:
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        answer = ReportModel.answer_for(self, body)
        ident = uuid.uuid4().hex
        item = {"type": "message", "id": "msg_" + ident, "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": answer, "annotations": []}]}
        response = {"id": "resp_" + ident, "object": "response", "created_at": 1, "status": "completed",
                    "model": body["model"], "output": [item],
                    "usage": {"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}
        events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
                  ("response.output_item.added", {"output_index": 0, "item": {**item, "content": [], "status": "in_progress"}}),
                  ("response.output_text.delta", {"output_index": 0, "content_index": 0, "item_id": item["id"], "delta": answer}),
                  ("response.output_item.done", {"output_index": 0, "item": item}),
                  ("response.completed", {"response": response})]
        print("TEXT_MODEL written", flush=True)
        self.events([(name, {"type": name, "sequence_number": index, **value})
                     for index, (name, value) in enumerate(events)], named=True)


def text_format_shape(body):
    text = body.get("text")
    if text is None:
        return {"format": "absent", "verbosity": "absent"}
    if not isinstance(text, dict):
        return {"format": "invalid-text-object", "verbosity": "unknown"}
    value = text.get("format")
    if value is None:
        format_shape = "absent"
    elif isinstance(value, dict) and value == {"type": "text"}:
        format_shape = {"type": "text"}
    elif isinstance(value, dict):
        # Never print schema property names, descriptions, defaults or enums.
        known_type = value.get("type")
        format_shape = {"type": known_type if known_type in ("text", "json_schema") else "other",
                        "member_count": len(value)}
    else:
        format_shape = "invalid-format-object"
    verbosity = text.get("verbosity", "absent")
    return {"format": format_shape, "verbosity": verbosity if verbosity in ("absent", "low", "medium", "high") else "other"}


class SandboxProviderModel(Handler):
    def send_error(self, code, message=None, explain=None):
        # Record the branch without echoing headers, input, message or key.
        print("SANDBOX_MODEL_REJECT code=" + str(int(code)), flush=True)
        return super().send_error(code, message, explain)

    def do_GET(self):
        self.send_error(404)

    def do_POST(self):
        if self.path != "/v1/responses":
            return self.send_error(404)
        authorization = self.headers.get_all("Authorization", [])
        expected = "Bearer " + self.server.upstream_key
        if len(authorization) != 1 or not hmac.compare_digest(authorization[0].encode("utf-8"), expected.encode("ascii")):
            return self.send_error(401, "model fixture authentication rejected")
        lengths = self.headers.get_all("Content-Length", [])
        if len(lengths) != 1 or self.headers.get("Transfer-Encoding"):
            return self.send_error(400, "model fixture requires a bounded request")
        try:
            size = int(lengths[0])
        except ValueError:
            return self.send_error(400, "invalid request size")
        if not 0 < size <= MAX_REQUEST_BYTES:
            return self.send_error(413, "model fixture request exceeds its bound")
        raw = self.rfile.read(size)
        try:
            body = json.loads(raw)
            if len(raw) != size or not isinstance(body, dict) or not isinstance(body.get("input"), list) or not isinstance(body.get("tools"), list) or not isinstance(body.get("model"), str):
                raise ValueError("invalid fixture envelope")
            selected = TextModel if text_request(body) else SkillModel if skill_request(body) else FileModel
        except (ValueError, TypeError, AttributeError):
            return self.send_error(400, "invalid model fixture request")
        kind = "text" if selected is TextModel else "skill" if selected is SkillModel else "file"
        print("SANDBOX_MODEL_AUTH verified kind=" + kind, flush=True)
        advertised = any(isinstance(namespace, dict) and namespace.get("type") == "namespace" and
                         namespace.get("name") == "mcp__report" and isinstance(namespace.get("tools"), list) and
                         any(isinstance(tool, dict) and tool.get("name") == "render" for tool in namespace["tools"])
                         for namespace in body["tools"])
        shape = {"users": sum(isinstance(item, dict) and item.get("role") == "user" for item in body["input"]),
                 "function_outputs": sum(isinstance(item, dict) and item.get("type") == "function_call_output" for item in body["input"]),
                 "mcp_report_render": advertised, "tools": len(body["tools"])}
        if selected is TextModel:
            shape["text"] = text_format_shape(body)
        print("SANDBOX_MODEL_SHAPE " + json.dumps(shape, sort_keys=True), flush=True)
        original = self.rfile
        self.rfile = io.BytesIO(raw)
        try:
            selected.do_POST(self)
        finally:
            self.rfile = original


def make_server(address, key):
    server = ThreadingHTTPServer(address, SandboxProviderModel)
    server.daemon_threads = True
    server.upstream_key = key
    return server


def check():
    """Real loopback HTTP dispatch/auth smoke; TLS is checked by the E2E run."""
    import threading
    import urllib.error
    import urllib.request

    key = "explicit-synthetic-upstream-model-key"
    server = make_server(("127.0.0.1", 0), key)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    origin = "http://127.0.0.1:" + str(server.server_port)

    def request(body, token=key, path="/v1/responses"):
        raw = json.dumps(body).encode()
        call = urllib.request.Request(origin + path, data=raw, method="POST",
                                      headers={"Authorization": "Bearer " + token})
        try:
            with urllib.request.urlopen(call, timeout=3) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def body(users, policy=False):
        properties = {"operation": {"type": "string"}}
        if policy:
            properties["policy"] = {"type": "string"}
        return {"model": "synthetic-fixed-model", "input": [{"role": "user", "content": text} for text in users],
                "tools": [{"type": "namespace", "name": "mcp__report", "tools": [
                    {"type": "function", "name": "render", "parameters": {"type": "object", "properties": properties}}]}]}

    def arguments(raw):
        for line in raw.decode().splitlines():
            if line.startswith("data: "):
                event = json.loads(line[6:])
                if event.get("type") == "response.output_item.done":
                    return json.loads(event["item"]["arguments"])
        raise AssertionError("native tool request event missing")

    def text_answer(raw):
        frames = raw.decode().strip().split("\n\n")
        names = []
        response = None
        delta = ""
        for index, frame in enumerate(frames):
            event, data = frame.splitlines()
            assert event.startswith("event: ") and data.startswith("data: ")
            value = json.loads(data[6:])
            assert value["type"] == event[7:] and value["sequence_number"] == index
            names.append(value["type"])
            if value["type"] == "response.output_text.delta":
                delta += value["delta"]
            if value["type"] == "response.completed":
                response = value["response"]
        assert names == ["response.created", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"]
        assert response["status"] == "completed" and response["output"][0]["content"][0]["text"] == delta
        return json.loads(delta)

    try:
        schema = {"type": "json_schema", "name": "must-not-be-logged", "schema": {"description": "private-description"}}
        diagnostic = text_format_shape({"text": {"format": schema, "verbosity": "private-verbosity"}})
        assert diagnostic == {"format": {"type": "json_schema", "member_count": 3}, "verbosity": "other"}
        assert "private" not in json.dumps(diagnostic) and "must-not-be-logged" not in json.dumps(diagnostic)
        file_body = body(["Produce the file report from input.txt."])
        status, raw = request(file_body)
        assert status == 200 and arguments(raw) == {"operation": "produce"}
        path = "/run/guest/private/skills/pinned-report/SKILL.md"
        skill_body = body(["<skill><path>" + path + "</path>SKILL_POLICY=policy-synthetic</skill>",
                           "Produce the file report from input.txt. [$pinned-report](" + path + ")"], policy=True)
        status, raw = request(skill_body)
        assert status == 200 and arguments(raw) == {"operation": "produce", "policy": "policy-synthetic"}
        status, raw = request(body(["Produce the file report from input.txt."], policy=True))
        assert status == 200 and arguments(raw)["policy"] == "missing-native-skill"
        text_spec = {"instruction": "Group region,amount_cents CSV by region and sum integer cents. Return JSON.",
                     "input": "region,amount_cents\neast-synthetic,105\nwest,31\neast-synthetic,-7\n"}
        history = [json.dumps(text_spec)]
        for message, expected in [
                (None, {"totals_cents": {"east-synthetic": 98, "west": 31}}),
                ("Repeat the totals from the original CSV. Confirmation " + "a" * 24,
                 {"totals_cents": {"east-synthetic": 98, "west": 31}, "confirmation": "a" * 24}),
                ("Recall the last confirmation and totals. Check " + "b" * 24,
                 {"totals_cents": {"east-synthetic": 98, "west": 31}, "confirmation": "a" * 24, "check": "b" * 24})]:
            if message is not None:
                history.append(message)
            text_body = body(history)
            text_body["tools"] = []
            text_body["text"] = {"format": {"type": "text"}, "verbosity": "low"}
            status, raw = request(text_body)
            assert status == 200 and text_answer(raw) == expected
        status, raw = request(file_body, token="not-the-upstream-key")
        assert status == 401 and key.encode() not in raw
        assert request(file_body, path="/v1/tasks/private")[0] == 404
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
        assert not thread.is_alive()
    print("sandbox provider scripted model checks passed", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--listen", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8443)
    parser.add_argument("--tls-cert")
    parser.add_argument("--tls-key")
    parser.add_argument("--upstream-key-file")
    options = parser.parse_args()
    if options.check:
        return check()
    if not options.tls_cert or not options.tls_key or not options.upstream_key_file or not 1 <= options.port <= 65535:
        parser.error("TLS certificate/key, private upstream-key-file and a fixed port are required")
    key = shared_key(options.upstream_key_file)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.set_alpn_protocols(["http/1.1"])
    context.load_cert_chain(options.tls_cert, options.tls_key)
    with make_server((options.listen, options.port), key) as server:
        server.socket = context.wrap_socket(server.socket, server_side=True)
        print("SANDBOX_MODEL_READY tls=true port=" + str(options.port), flush=True)
        server.serve_forever()


if __name__ == "__main__":
    main()

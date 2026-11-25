#!/usr/local/bin/python3 -I
"""Synthetic stdio MCP tool. Only the Worker selects the input/output locations."""
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
from summarize import summarize


def handle(method, params):
    if method == "initialize":
        return {"protocolVersion": params["protocolVersion"], "capabilities": {"tools": {}},
                "serverInfo": {"name": "report", "version": "1"}}
    if method == "tools/list":
        return {"tools": [{"name": "render", "description": "Generate or revise the regional report.",
                           "inputSchema": {"type": "object", "properties": {"adjust_west": {"type": "integer"}},
                                           "required": ["adjust_west"], "additionalProperties": False}}]}
    if method == "ping":
        return {}
    if method != "tools/call" or params.get("name") != "render":
        raise ValueError("unsupported tool method")
    adjustment = params["arguments"]["adjust_west"]
    if type(adjustment) is not int or adjustment not in (0, -9):
        raise ValueError("invalid fixture adjustment")
    if adjustment:
        report = json.loads(Path(os.environ["REPORT_PREVIOUS"]).read_bytes())
        report["totals_cents"]["west"] += adjustment
    else:
        report = summarize(Path("input.txt"))
    raw = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode()
    with Path(os.environ["REPORT_RESULT"]).open("xb") as output:
        output.write(raw)
    return {"content": [{"type": "text", "text": "written"}]}


for line in sys.stdin:
    request = json.loads(line)
    if "id" not in request:
        continue
    reply = {"jsonrpc": "2.0", "id": request["id"]}
    try:
        reply["result"] = handle(request["method"], request.get("params", {}))
    except (OSError, ValueError, KeyError) as exc:
        reply["error"] = {"code": -32602, "message": str(exc)}
    print(json.dumps(reply), flush=True)

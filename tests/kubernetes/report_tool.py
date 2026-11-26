#!/usr/local/bin/python3 -I
"""Synthetic MCP report tool; all paths are selected by the Worker, never the model."""
import json
import os
from pathlib import Path
import secrets
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent))
from summarize import summarize


def render(operation):
    source, state_path = Path("sales.csv"), Path("state.json")
    previous = Path(os.environ["REPORT_PREVIOUS"])
    if operation == "lost_workspace":
        # Fault injection after the original native session and workspace were
        # restored: remove only the fixture's required files, then attempt work.
        assert source.is_file() and state_path.is_file()
        source.unlink()
        state_path.unlink()
        return render("revise")
    if operation in ("hold", "pod_loss"):
        assert source.is_file() and state_path.is_file()
        Path(".hold-started").write_text("active MCP render\n")
        Path("unpublished-draft.txt").write_text("must not advance the checkpoint\n")
        # The Pod-loss test kills this process. Its fixture must not finish
        # naturally before the unchanged production 15-minute allocation expires.
        time.sleep(1200 if operation == "pod_loss" else 90)
        raise ValueError("hold operation was not cancelled")
    if operation == "produce":
        if source.exists() or state_path.exists():
            raise ValueError("initial workspace was not empty")
        # input.txt is supplied by the Worker. The model sees only its pathname.
        source.write_bytes(Path("input.txt").read_bytes())
        report = summarize(source)
        state = {"marker": "workspace-only-" + secrets.token_hex(16), "revision": 0}
    elif operation == "missing_input":
        report = summarize(Path("missing-sales.csv"))
        raise AssertionError("missing input unexpectedly existed")
    else:
        state = json.loads(state_path.read_bytes())
        report = summarize(source)
        saved = json.loads(previous.read_bytes())
        assert saved["totals_cents"] == report["totals_cents"], "restored report and CSV differ"
        if operation == "revise":
            assert state["revision"] == 0
            with source.open("a", encoding="ascii") as stream:
                stream.write("west,-9\n")
            report = summarize(source)
            state["revision"] = 1
        elif operation == "audit":
            assert state["revision"] == 1
            report.update(workspace_marker=state["marker"], revision=state["revision"])
        else:
            raise ValueError("unsupported operation")
    raw = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode()
    with Path(os.environ["REPORT_RESULT"]).open("xb") as stream:
        stream.write(raw)
    state_path.write_text(json.dumps(state, sort_keys=True) + "\n")
    Path("out/tool-proof.json").write_text(json.dumps({"operation": operation, "revision": state["revision"]}, sort_keys=True) + "\n")
    # A downloaded workspace records the actual tool RPC and data read/write.
    with Path("tool-events.jsonl").open("a", encoding="ascii") as stream:
        stream.write(json.dumps({"tool": "report.render", "operation": operation,
                                 "input": "sales.csv", "output": os.environ["REPORT_RESULT"],
                                 "revision": state["revision"]}, sort_keys=True) + "\n")
    return {"content": [{"type": "text", "text": "written"}]}


def handle(method, params):
    if method == "initialize":
        return {"protocolVersion": params["protocolVersion"], "capabilities": {"tools": {}},
                "serverInfo": {"name": "report", "version": "2"}}
    if method == "tools/list":
        return {"tools": [{"name": "render", "description": "Generate, revise or audit the saved regional report.",
                           "inputSchema": {"type": "object", "properties": {"operation": {
                               "type": "string", "enum": ["produce", "revise", "audit", "missing_input", "lost_workspace", "hold", "pod_loss"]}},
                               "required": ["operation"], "additionalProperties": False}}]}
    if method == "ping":
        return {}
    if method != "tools/call" or params.get("name") != "render":
        raise ValueError("unsupported tool method")
    arguments = params.get("arguments", {})
    if set(arguments) != {"operation"}:
        raise ValueError("invalid tool arguments")
    return render(arguments["operation"])


def main():
    for line in sys.stdin:
        request = json.loads(line)
        if "id" not in request:
            continue
        reply = {"jsonrpc": "2.0", "id": request["id"]}
        try:
            reply["result"] = handle(request["method"], request.get("params", {}))
        except (OSError, ValueError, KeyError, AssertionError) as exc:
            reply["result"] = {"isError": True, "content": [{"type": "text", "text": str(exc)}]}
        print(json.dumps(reply), flush=True)


if __name__ == "__main__":
    main()

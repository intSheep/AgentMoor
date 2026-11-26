#!/usr/local/bin/python3 -I
"""Synthetic report tool checks a policy using the exact privately installed Skill package."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
import report_tool


def handle(method, params):
    if method == "tools/list":
        result = report_tool.handle(method, params)
        schema = result["tools"][0]["inputSchema"]
        schema["properties"]["policy"] = {"type": "string"}
        schema["required"].append("policy")
        return result
    if method != "tools/call":
        return report_tool.handle(method, params)
    arguments = params.get("arguments", {})
    if params.get("name") != "render" or set(arguments) != {"operation", "policy"}:
        raise ValueError("invalid Skill report arguments")
    if arguments["operation"] not in {"produce", "revise", "audit"}:
        raise ValueError("invalid Skill operation")
    manifests = json.loads(os.environ.get("AGENTMOOR_SKILLS", "{}"))
    if set(manifests) != {"pinned-report"}:
        raise ValueError("required private Skill is not installed")
    manifest = Path(manifests["pinned-report"])
    if not manifest.is_absolute() or not manifest.is_file():
        raise ValueError("invalid private Skill manifest")
    directory = manifest.resolve().parent
    script = directory / "scripts/check.py"
    reference = directory / "references/rule.json"
    checked = subprocess.run([sys.executable, "-I", str(script), arguments["policy"]],
                             capture_output=True, text=True, timeout=10)
    if checked.returncode or checked.stdout.strip() != "verified-skill-policy":
        raise ValueError("native Skill policy was not followed")
    result = report_tool.render(arguments["operation"])
    proof = {"policy": arguments["policy"], "operation": arguments["operation"],
             "manifest_sha256": hashlib.sha256(manifest.read_bytes()).hexdigest(),
             "script_sha256": hashlib.sha256(script.read_bytes()).hexdigest(),
             "reference_sha256": hashlib.sha256(reference.read_bytes()).hexdigest()}
    Path("out/skill-proof.json").write_text(json.dumps(proof, sort_keys=True) + "\n")
    return result


def main():
    for line in sys.stdin:
        request = json.loads(line)
        if "id" not in request:
            continue
        reply = {"jsonrpc": "2.0", "id": request["id"]}
        try:
            reply["result"] = handle(request["method"], request.get("params", {}))
        except (OSError, ValueError, KeyError, AssertionError, subprocess.SubprocessError) as exc:
            reply["result"] = {"isError": True, "content": [{"type": "text", "text": str(exc)}]}
        print(json.dumps(reply), flush=True)


if __name__ == "__main__":
    main()

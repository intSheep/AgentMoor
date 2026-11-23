"""Run one installed CLI against a local fixture provider, capture, replace, resume.

Usage: python3 tests/native_roundtrip.py codex /absolute/path/to/codex
Requires make build. Each run uses new temporary state and synthetic credentials.
"""
import argparse
import json
import os
import shutil
import subprocess
import tempfile
import threading
import uuid
from contextlib import contextmanager
from http.server import ThreadingHTTPServer
from pathlib import Path

from fixture_provider import Handler

MOOR = str(Path(__file__).resolve().parents[1] / "bin/agentmoor")


def call(*args, cwd, env, timeout=60, input_text=None):
    command = list(map(str, args))
    proc = subprocess.run(command, cwd=cwd, env=env, text=True, capture_output=True,
                          stdin=subprocess.DEVNULL if input_text is None else None, input=input_text, timeout=timeout)
    if proc.returncode:
        raise RuntimeError(f"{Path(command[0]).name} exited {proc.returncode}: {(proc.stderr + proc.stdout)[-1800:]}")
    return proc.stdout.strip()


@contextmanager
def platform_service(root, env):
    data = root / "platform"
    call(MOOR, "init", "--data", data, cwd=root, env=env)
    proc = subprocess.Popen([MOOR, "serve", "--data", str(data), "--listen", "127.0.0.1:0"],
                            cwd=root, env=env, stdout=subprocess.PIPE, text=True)
    try:
        addr = proc.stdout.readline().strip().split()[-1]
        yield data, "http://" + addr
    finally:
        proc.terminate()
        proc.wait(timeout=15)
        proc.stdout.close()


def runtime_env(state, endpoint):
    # Give the child process its own genuine user/config directories and no inherited credentials.
    state.mkdir(parents=True, exist_ok=True)
    env = {"PATH": os.environ["PATH"], "LANG": "en_US.UTF-8", "HOME": str(state),
           "TMPDIR": str(state / "tmp"), "CI": "true", "NO_COLOR": "1", "TERM": "dumb",
           "XDG_CONFIG_HOME": str(state / "config"), "XDG_STATE_HOME": str(state / "state"),
           "XDG_CACHE_HOME": str(state / "cache"), "XDG_DATA_HOME": str(state / "data"),
           "CLAUDE_CONFIG_DIR": str(state / "claude"), "CODEX_HOME": str(state / "codex"),
           "GEMINI_CLI_HOME": str(state / "gemini"), "GEMINI_API_KEY": "fixture",
           "ANTHROPIC_BASE_URL": endpoint, "ANTHROPIC_API_KEY": "fixture",
           "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "GOOGLE_GEMINI_BASE_URL": endpoint,
           "OPENAI_API_KEY": "fixture", "OPENCODE_DISABLE_AUTOUPDATE": "1",
           "OPENCODE_DISABLE_MODELS_FETCH": "1", "DISABLE_TELEMETRY": "1",
           "NO_PROXY": "127.0.0.1,localhost", "DO_NOT_TRACK": "1"}
    for key in ("TMPDIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "GEMINI_CLI_HOME"):
        Path(env[key]).mkdir(parents=True, exist_ok=True)
    settings = Path(env["GEMINI_CLI_HOME"]) / ".gemini/settings.json"
    settings.parent.mkdir(exist_ok=True)
    settings.write_text(json.dumps({"security": {"auth": {"selectedType": "gemini-api-key"}}}))
    return env


def turn(agent, exe, prompt, env, workspace, endpoint, session=None):
    if agent == "claude-code":
        args = ["--bare", "--setting-sources", "", "--tools", "", "--model", "claude-sonnet-4-6",
                "--output-format", "json", "-p", prompt]
        if session:
            args.extend(["--resume", session])
    elif agent == "codex":
        args = ["exec", "--skip-git-repo-check", "-m", "gpt-5", "-c", 'model_provider="operator"',
                "-c", 'model_providers.operator.name="Fixture"', "-c", f'model_providers.operator.base_url="{endpoint}/v1"',
                "-c", 'model_providers.operator.wire_api="responses"', "--json"]
        args += ["resume", session, prompt] if session else [prompt]
    elif agent == "gemini-cli":
        args = ["--skip-trust", "-m", "gemini-2.5-flash", "-p", prompt, "--output-format", "json"]
        if session:
            args.extend(["--resume", session])
    else:
        env["OPENCODE_CONFIG_CONTENT"] = json.dumps({"permission": "deny",
            "provider": {"openai": {"options": {"baseURL": endpoint + "/v1", "apiKey": "fixture"}}}})
        args = ["run", "--pure", "--model", "openai/gpt-4.1", "--format", "json"]
        if session:
            args += ["--session", session]
        # OpenCode quotes positional multi-word prompts. Stdin preserves the user's text.
        return call(exe, *args, cwd=workspace, env=env, input_text=prompt)
    return call(exe, *args, cwd=workspace, env=env)


def session_root(agent, state, workspace, exe, env):
    if agent == "codex":
        return state / "codex/sessions"
    if agent == "gemini-cli":
        return next(p.parent for p in (state / "gemini").rglob("session-*.json*") if p.parent.name == "chats")
    if agent == "claude-code":
        return next(p.parent for p in (state / "claude/projects").glob("*/*.jsonl"))
    return Path(env["XDG_DATA_HOME"])


def verify(agent, exe, root):
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    evidence = {"nonce": "first-" + uuid.uuid4().hex, "followup": "second-" + uuid.uuid4().hex,
                "first_seen": False, "restored_history_seen": False}
    provider.evidence = evidence
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    endpoint = f"http://127.0.0.1:{provider.server_port}"
    try:
        state, ws = root / "runtime", root / "work"
        ws.mkdir()
        (ws / "example.txt").write_text("workspace content")
        env = runtime_env(state, endpoint)
        version = call(exe, "--version", cwd=ws, env=env)
        first = turn(agent, exe, "Remember " + evidence["nonce"], env, ws, endpoint)
        if not evidence["first_seen"]:
            raise AssertionError("native CLI did not send the first turn to the fixture: " + first[-1200:])
        source = session_root(agent, state, ws, exe, env)
        relative_source = source.relative_to(state)
        with platform_service(root, env) as (data, url):
            scope = dict(tenant="fixture", project="project", runtime="agent", adapter=agent)
            flags = [part for key, value in scope.items() for part in ("--" + key, value)]
            token = call(MOOR, "issue", "--data", data, "--write", *flags, cwd=root, env=env)
            moor_env = dict(env, AGENTMOOR_TOKEN=token)
            cfg = root / "worker.json"
            config = dict(scope=scope, endpoint=url, workspace=str(ws), runtime_cli=exe,
                          state_dir=str(root / "journal"))
            if agent == "opencode":
                config["native_sessions"] = True
            else:
                config["sessions"] = str(source)
            cfg.write_text(json.dumps(config))

            def moor(command, *args):
                return call(MOOR, command, "--config", cfg, *args, cwd=root, env=moor_env)

            moor("sync")
            sessions = [row for row in json.loads(moor("assets")) if row["kind"] == "session"]
            session = sessions[0]["id"]
            shutil.rmtree(state)
            shutil.rmtree(ws)
            env = runtime_env(state, endpoint)
            moor("restore", "--kind", "workspace", "--id", "root", "--to", ws)
            dest = state / relative_source
            dest.parent.mkdir(parents=True, exist_ok=True)
            if agent == "opencode":
                moor("restore-native", "--id", session, "--to", dest)
            else:
                moor("restore", "--kind", "session", "--id", session, "--to", dest)
            result = turn(agent, exe, "Please acknowledge " + evidence["followup"], env, ws, endpoint, session)
            if not evidence["restored_history_seen"]:
                raise AssertionError("follow-up lacked prior history: " + result[-1200:])
        print(f"PASS {agent} {version}: new runtime sent both turns from restored session", flush=True)
    finally:
        provider.shutdown()
        provider.server_close()


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("agent", choices=("claude-code", "codex", "gemini-cli", "opencode"))
    p.add_argument("executable", type=Path)
    args = p.parse_args()
    with tempfile.TemporaryDirectory(prefix="agentmoor-native-") as tmp:
        verify(args.agent, str(args.executable.resolve()), Path(tmp).resolve())

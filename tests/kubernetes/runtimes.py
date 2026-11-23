"""Native settings for the Kubernetes contract's synthetic provider."""

ENDPOINT = "http://model:8080"
BINARIES = {"claude-code": "claude", "codex": "codex", "gemini-cli": "gemini", "opencode": "opencode"}


def environment(agent):
    if agent == "opencode":
        return {"XDG_DATA_HOME": "/run/task/native/data", "XDG_CONFIG_HOME": "/run/task/native/config",
                "XDG_STATE_HOME": "/run/task/native/state", "XDG_CACHE_HOME": "/run/task/native/cache",
                "OPENAI_API_KEY": "fixture", "OPENAI_BASE_URL": ENDPOINT + "/v1", "OPENCODE_MODEL": "openai/gpt-4.1",
                "OPENCODE_DISABLE_MODELS_FETCH": "1", "OPENCODE_DISABLE_AUTOUPDATE": "1"}
    if agent == "gemini-cli":
        return {"GEMINI_CLI_HOME": "/run/task/native", "GEMINI_API_KEY": "fixture",
                "GOOGLE_GEMINI_BASE_URL": ENDPOINT, "GEMINI_MODEL": "gemini-2.5-flash"}
    if agent == "codex":
        return {"OPENAI_API_KEY": "fixture", "OPENAI_BASE_URL": ENDPOINT + "/v1",
                "CODEX_HOME": "/run/task/native", "CODEX_MODEL": "gpt-5"}
    if agent == "claude-code":
        return {"CLAUDE_CONFIG_DIR": "/run/task/native", "ANTHROPIC_MODEL": "claude-sonnet-4-6",
                "ANTHROPIC_API_KEY": "fixture", "ANTHROPIC_BASE_URL": ENDPOINT,
                "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
    raise ValueError(agent)

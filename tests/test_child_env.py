"""Every child of the executor gets a scrubbed environment (MX-R-3)."""

from __future__ import annotations

from opendaisugi import agent_profiles
from opendaisugi.agentic_executor import child_env

ENV = {
    "PATH": "/usr/bin",
    "HOME": "/home/x",
    "ANTHROPIC_API_KEY": "k",
    "ANTHROPIC_AUTH_TOKEN": "k",
    "CLAUDE_CODE_OAUTH_TOKEN": "k",
    "OPENAI_API_KEY": "k",
    "XAI_API_KEY": "k",
    "GITHUB_TOKEN": "k",
    "MY_KEY_VAR": "k",
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:1",
    "SPRIG_BACKEND": "api",
    "SPRIG_MODEL": "m",
    "TOKENIZERS_PARALLELISM": "false",
}


def _profiles():
    return agent_profiles.resolve(
        {"desk": {"wire": "openai", "base_url": "http://x/v1", "key_env": "MY_KEY_VAR"}},
        ENV,
    )


def test_a_sprig_child_gets_no_key_and_no_redirect():
    got = child_env(ENV, _profiles(), sprig=True)
    assert sorted(got) == ["HOME", "PATH", "TOKENIZERS_PARALLELISM"]


def test_a_claude_child_gets_no_key_but_keeps_its_base_url():
    got = child_env(ENV, _profiles(), sprig=False)
    assert sorted(got) == [
        "ANTHROPIC_BASE_URL",
        "HOME",
        "PATH",
        "SPRIG_BACKEND",
        "SPRIG_MODEL",
        "TOKENIZERS_PARALLELISM",
    ]


def test_an_empty_table_still_scrubs():
    got = child_env(ENV, agent_profiles.resolve(None, ENV), sprig=False)
    assert "ANTHROPIC_API_KEY" not in got and "GITHUB_TOKEN" not in got

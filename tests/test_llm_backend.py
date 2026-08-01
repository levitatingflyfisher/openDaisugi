"""resolve_backend must WORK OUT OF THE BOX: when nothing is configured, pick a
backend that actually runs on this machine instead of defaulting to the API path
that dies without a key. See docs/plans/2026-08-26-daisugi-interface-two-lenses.md (W9).
"""

from opendaisugi.llm import resolve_backend


def test_explicit_backend_arg_wins(monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    assert resolve_backend("litellm") == "litellm"


def test_env_var_wins_over_autodetect(monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    assert resolve_backend() == "claude-code"


def test_autodetects_claude_code_when_no_key_but_claude_present(monkeypatch):
    # The reported bug: no key on this box, `claude` CLI available → it should
    # route through the keyless local backend, not die on a missing Anthropic key.
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/claude" if name == "claude" else None)
    assert resolve_backend() == "claude-code"


def test_autodetects_litellm_when_key_present(monkeypatch):
    # A configured key means the API path is intended — unchanged v0.11 behavior.
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/claude" if name == "claude" else None)
    assert resolve_backend() == "litellm"


def test_falls_back_to_litellm_when_no_key_and_no_claude(monkeypatch):
    # Nothing available: keep the historical default so its (improved) error can fire.
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert resolve_backend() == "litellm"

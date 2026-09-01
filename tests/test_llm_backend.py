"""resolve_backend must WORK OUT OF THE BOX: when nothing is configured, pick a
backend that actually runs on this machine instead of defaulting to the API path
that dies without a key. See docs/plans/2026-08-26-daisugi-interface-two-lenses.md (W9).
"""

from opendaisugi.llm import resolve_backend


def test_explicit_backend_arg_wins(monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    assert resolve_backend("api") == "api"


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
    monkeypatch.setattr(
        "shutil.which", lambda name: "/usr/bin/claude" if name == "claude" else None
    )
    assert resolve_backend() == "claude-code"


def test_autodetects_api_when_key_present(monkeypatch):
    # A configured key means the API path is intended — unchanged v0.11 behavior.
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    monkeypatch.setattr(
        "shutil.which", lambda name: "/usr/bin/claude" if name == "claude" else None
    )
    assert resolve_backend() == "api"


def test_falls_back_to_api_when_no_key_and_no_claude(monkeypatch):
    # Nothing available: keep the historical default so its (improved) error can fire.
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert resolve_backend() == "api"


# The backend was named litellm while litellm carried it. litellm is gone, so
# the name is gone too: the old value is refused with one line that names api.

import pytest  # noqa: E402

from opendaisugi.exceptions import LLMNotConfigured  # noqa: E402
from opendaisugi.llm import RENAMED_BACKEND_TEXT  # noqa: E402


def test_renamed_text_is_one_line_naming_api():
    assert "\n" not in RENAMED_BACKEND_TEXT
    assert "'litellm'" in RENAMED_BACKEND_TEXT and "'api'" in RENAMED_BACKEND_TEXT


def test_old_name_as_argument_is_refused(monkeypatch):
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    with pytest.raises(LLMNotConfigured) as ei:
        resolve_backend("litellm")
    assert str(ei.value) == RENAMED_BACKEND_TEXT


@pytest.mark.parametrize("value", ["litellm", " litellm "])
def test_old_name_in_env_is_refused(monkeypatch, value):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", value)
    with pytest.raises(LLMNotConfigured) as ei:
        resolve_backend()
    assert str(ei.value) == RENAMED_BACKEND_TEXT


def test_old_name_in_config_is_refused(monkeypatch, tmp_path):
    import opendaisugi

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("llm_backend: litellm\n", encoding="utf-8")
    with pytest.raises(LLMNotConfigured) as ei:
        resolve_backend()
    assert str(ei.value) == RENAMED_BACKEND_TEXT


def test_llm_check_fails_closed_on_the_old_name(monkeypatch):
    from opendaisugi.llm_check import run_llm_check

    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "litellm")
    res = run_llm_check("anything", {"x": 1})
    assert res.errored and not res.satisfied
    assert res.reason == "error: llm_check call failed: " + RENAMED_BACKEND_TEXT

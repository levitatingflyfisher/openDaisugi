"""v0.27.0 — a failed llm_check LLM call fails CLOSED, never silent-passes."""
from __future__ import annotations


def test_llm_check_network_error_fails_closed(monkeypatch):
    import opendaisugi.llm_check as lc
    def boom(*a, **k):
        raise ConnectionError("network down")
    # adapt to the real client entrypoint in llm_check.py
    monkeypatch.setattr(lc, "_invoke_model", boom, raising=False)
    result = lc.run_llm_check("any rule", context={})  # adapt to real signature
    # Fail-closed: a failed probabilistic check must NOT report satisfied.
    assert result.satisfied is False
    assert "error" in (result.reason or "").lower() or result.errored is True


def test_llm_check_rate_limit_handled_identically(monkeypatch):
    import opendaisugi.llm_check as lc
    def ratelimit(*a, **k):
        raise RuntimeError("429 rate limited")
    monkeypatch.setattr(lc, "_invoke_model", ratelimit, raising=False)
    result = lc.run_llm_check("any rule", context={})
    assert result.satisfied is False

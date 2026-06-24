"""v0.27.0 fixup — the PRODUCTION predicate-evaluation path must fail closed on
llm_check call errors. Task 10 added run_llm_check (fail-closed) but left
predicate_z3.evaluate_predicate calling the deprecated call_llm_check, so the
fix was dead code and the original test gave false assurance.

This test drives the real evaluate_predicate path and asserts the failure flows
through run_llm_check's contract (message "llm_check call failed"), which only
holds once the call site is migrated.
"""
from __future__ import annotations

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import evaluate_predicate


def _raise_conn(*_a, **_k):
    raise ConnectionError("network down")


def _env(stakes="medium"):
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=["ls"]),
                    stakes=stakes)


def _plan():
    return ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])


def test_evaluate_predicate_llm_check_failure_is_failclosed(monkeypatch):
    import opendaisugi.llm_check as lc
    monkeypatch.setattr(lc, "_invoke_model", _raise_conn)
    expr = parse_expression({"op": "llm_check", "rule": "the plan is safe"})
    # Must NOT return True (silent approve) and must go through run_llm_check.
    with pytest.raises(Exception) as exc:
        evaluate_predicate(expr, _plan(), _env())
    assert "llm_check call failed" in str(exc.value)


def test_rate_limit_handled_identically(monkeypatch):
    import opendaisugi.llm_check as lc
    def _ratelimit(*_a, **_k):
        raise RuntimeError("429 rate limited")
    monkeypatch.setattr(lc, "_invoke_model", _ratelimit)
    expr = parse_expression({"op": "llm_check", "rule": "ok?"})
    with pytest.raises(Exception) as exc:
        evaluate_predicate(expr, _plan(), _env())
    assert "llm_check call failed" in str(exc.value)

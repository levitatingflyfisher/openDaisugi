"""Local Tier-1 qualification check + config wiring (v0.30).

The qualification check is the load-bearing honesty mechanism: a hardware
recommendation is only a hypothesis, so before a local model is trusted as
Tier-1 it must prove — on the actual box — that it emits valid envelopes at an
acceptable rate over the REAL provider path. These tests exercise the warden's
counting/threshold/decision logic against a provider that FLAKES realistically
(valid / declined / error), never an always-valid stub.
"""

import asyncio

from opendaisugi.local_setup import (
    QualificationResult,
    load_configured_tier1,
    qualify_local_model,
    write_tier1_config,
)
from opendaisugi.models import Envelope, Permission


def _env():
    return Envelope(generated_by="probe", task="t", permissions=Permission(shell=True))


class _FlakyProvider:
    """Returns a scripted sequence of outcomes: Envelope | None | 'raise'."""

    def __init__(self, script):
        self.script = list(script)
        self.calls = 0

    async def generate_envelope(self, task, *, context=None):
        outcome = self.script[self.calls % len(self.script)]
        self.calls += 1
        if outcome == "raise":
            raise RuntimeError("local endpoint hiccup")
        return outcome  # Envelope() or None


def test_warden_counts_valid_declined_and_errors():
    prov = _FlakyProvider([_env(), _env(), None, "raise"])  # 1 task list of 4
    res = asyncio.run(qualify_local_model(prov, probe_tasks=["a", "b", "c", "d"], threshold=0.8))
    assert isinstance(res, QualificationResult)
    assert res.attempts == 4
    assert res.valid == 2
    assert res.pass_rate == 0.5
    assert res.passed is False  # 0.5 < 0.8 — a flaky model is NOT promoted
    kinds = sorted(k for _, k in res.outcomes)
    assert kinds == ["declined", "error", "valid", "valid"]


def test_warden_passes_only_above_threshold():
    # 3 of 5 valid = 0.6
    script = [_env(), _env(), _env(), None, None]
    below = asyncio.run(
        qualify_local_model(_FlakyProvider(script), probe_tasks=list("abcde"), threshold=0.8)
    )
    above = asyncio.run(
        qualify_local_model(_FlakyProvider(script), probe_tasks=list("abcde"), threshold=0.6)
    )
    assert below.passed is False
    assert above.passed is True
    assert below.pass_rate == above.pass_rate == 0.6


def test_warden_all_declines_fails_clean():
    res = asyncio.run(
        qualify_local_model(_FlakyProvider([None]), probe_tasks=list("abc"), threshold=0.5)
    )
    assert res.valid == 0 and res.pass_rate == 0.0 and res.passed is False


def test_warden_repeats_samples_each_task_multiple_times():
    # repeats multiplies attempts so a probabilistic model is sampled, not asked once
    res = asyncio.run(
        qualify_local_model(
            _FlakyProvider([_env()]), probe_tasks=["a", "b"], threshold=0.5, repeats=3
        )
    )
    assert res.attempts == 6


def test_default_probe_battery_is_used_when_none_given():
    res = asyncio.run(qualify_local_model(_FlakyProvider([_env()]), threshold=0.5))
    assert res.attempts >= 3  # a built-in battery spanning envelope shapes


# ---- config wiring ----


def test_write_then_load_tier1_config(tmp_path):
    write_tier1_config(
        tmp_path, model="qwen2.5:1.5b", base_url="http://localhost:8080/v1", host_kind="openai"
    )
    prov = load_configured_tier1(tmp_path)
    assert prov is not None
    assert getattr(prov, "model", None) and "qwen2.5" in prov.model
    assert prov.base_url == "http://localhost:8080/v1"


def test_load_tier1_config_absent_returns_none(tmp_path):
    assert load_configured_tier1(tmp_path) is None


def test_a_raised_error_reason_holds_no_key():
    class Raises:
        async def generate_envelope(self, task, *, context=None):
            raise RuntimeError("call failed at http://u:pw@h/v1 with sk-ABCDMIDDLEPART1234WXYZ")

    res = asyncio.run(qualify_local_model(Raises(), probe_tasks=["a"], threshold=0.8))
    assert res.outcomes == [("a", "error")]
    assert "MIDDLE" not in res.reasons[0] and "pw@" not in res.reasons[0]


def test_write_then_load_tier1_config_keeps_host_kind(tmp_path):
    import json

    from opendaisugi.local_setup import load_configured_tier1, write_tier1_config

    p = write_tier1_config(tmp_path, model="m", base_url="http://h:11434/v1", host_kind="ollama")
    assert json.loads(p.read_text()) == {
        "model": "m",
        "base_url": "http://h:11434/v1",
        "host_kind": "ollama",
    }
    assert list(json.loads(p.read_text())) == ["model", "base_url", "host_kind"]
    assert load_configured_tier1(tmp_path).reasoning_effort == "none"
    write_tier1_config(tmp_path, model="m", base_url="http://h:8080/v1", host_kind="openai")
    assert load_configured_tier1(tmp_path).reasoning_effort is None


def test_load_tier1_config_without_host_kind_sends_no_effort(tmp_path):
    from opendaisugi.local_setup import load_configured_tier1

    (tmp_path / "local_tier1.json").write_text('{"model": "m", "base_url": "http://h/v1"}')
    assert load_configured_tier1(tmp_path).reasoning_effort is None


def test_all_failures_truncated():
    from opendaisugi.local_setup import QualificationResult

    def r(kinds):
        return QualificationResult(
            attempts=len(kinds),
            valid=kinds.count("valid"),
            pass_rate=0.0,
            passed=False,
            threshold=0.8,
            outcomes=[("t", k) for k in kinds],
            reasons=[""] * len(kinds),
        )

    assert r(["truncated", "truncated"]).all_failures_truncated()
    assert r(["valid", "truncated"]).all_failures_truncated()
    assert not r(["truncated", "declined"]).all_failures_truncated()
    assert not r(["valid", "valid"]).all_failures_truncated()

"""Tests for token-tier accounting (v0.4.0)."""

from __future__ import annotations

import time

import pytest
from typer.testing import CliRunner

from opendaisugi.accounting import (
    classify_tier,
    tier1_provider_name,
    tier_stats,
)
from opendaisugi.cli import app
from opendaisugi.journal import DistillableTrace, Journal, TraceRecord
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    Postcondition,
    VerificationResult,
)


def test_classify_pathway() -> None:
    assert classify_tier("compiled-pathway:abc123") == "tier0"


def test_classify_tier1() -> None:
    assert classify_tier("tier1:ollama/llama3.2") == "tier1"


def test_classify_frontier() -> None:
    assert classify_tier("anthropic/claude-sonnet-4-20250514") == "tier2"
    assert classify_tier("distilled") == "tier2"


def test_tier1_provider_name() -> None:
    assert tier1_provider_name("tier1:my-box") == "my-box"
    assert tier1_provider_name("tier1:litellm:ollama/llama3.2:3b") == "litellm:ollama/llama3.2:3b"
    assert tier1_provider_name("anthropic/foo") is None


def _mk_envelope(generated_by: str) -> Envelope:
    return Envelope(
        generated_by=generated_by,
        task="t",
        permissions=Permission(file_read=[], file_write=[], network=False, shell=False),
        invariants=[],
        postconditions=[Postcondition(type="exit_code", expected=0)],
    )


class _FakeJournal:
    """Minimal journal shape: list_successful_traces + load_trace."""
    def __init__(self, traces: list[tuple[str, str]]) -> None:
        # (trace_id, generated_by)
        now = time.time()
        self._rows = [
            DistillableTrace(
                trace_id=tid,
                task="t",
                envelope_id="env",
                plan_id="pl",
                run_id="run",
                run_status="succeeded",
                created_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now)),
            )
            for tid, _ in traces
        ]
        self._bodies = {
            tid: TraceRecord(
                id=tid, created_at="2026-04-17T00:00:00Z",
                task="t", envelope=_mk_envelope(gb),
                plan=ActionPlan(id=tid, task="t", source="test", steps=[]),
                result=VerificationResult(
                    plan_id=tid, envelope_id="env", ok=True,
                    violations=[], warnings=[], duration_ms=1.0,
                ),
            )
            for tid, gb in traces
        }

    def list_successful_traces(self, *, since: float | None = None) -> list[DistillableTrace]:
        return self._rows

    def load_trace(self, trace_id: str) -> TraceRecord:
        return self._bodies[trace_id]


def test_tier_stats_buckets_correctly() -> None:
    journal = _FakeJournal([
        ("t1", "compiled-pathway:abc"),
        ("t2", "compiled-pathway:xyz"),
        ("t3", "tier1:my-box"),
        ("t4", "tier1:ollama"),
        ("t5", "anthropic/claude-sonnet-4-20250514"),
    ])
    stats = tier_stats(journal)
    assert stats.total == 5
    assert stats.by_tier["tier0"] == 2
    assert stats.by_tier["tier1"] == 2
    assert stats.by_tier["tier2"] == 1
    assert stats.by_tier1_provider == {"my-box": 1, "ollama": 1}
    # Tier-0 should contribute 0 tokens; tier-2 should dominate per-call.
    assert stats.estimated_tokens["tier0"] == 0
    assert stats.estimated_tokens["tier2"] > stats.estimated_tokens["tier1"]
    assert stats.pathway_hit_rate == pytest.approx(2 / 5)


def test_tier_stats_empty() -> None:
    journal = _FakeJournal([])
    stats = tier_stats(journal)
    assert stats.total == 0
    assert stats.pathway_hit_rate == 0.0


def test_tier_stats_missing_methods_returns_empty() -> None:
    """A journal-ish object that exposes neither listing method must not crash."""
    class Bare:
        pass
    assert tier_stats(Bare()).total == 0


def test_cli_tiers_stats_smoke(tmp_path) -> None:
    """End-to-end: real Journal → `daisugi tiers stats` works on an empty store."""
    runner = CliRunner()
    # Create an empty journal at tmp_path so the CLI invocation finds a valid db.
    Journal(data_dir=tmp_path)
    result = runner.invoke(app, ["tiers", "stats", "--data-dir", str(tmp_path), "--days", "7"])
    assert result.exit_code == 0, result.output
    assert "window: last 7d" in result.output
    assert "total traces: 0" in result.output
    assert "pathway hit rate" in result.output


def test_cli_tiers_stats_json_smoke(tmp_path) -> None:
    import json
    runner = CliRunner()
    Journal(data_dir=tmp_path)
    result = runner.invoke(app, ["tiers", "stats", "--data-dir", str(tmp_path), "--json"])
    assert result.exit_code == 0
    payload = json.loads(result.output)
    assert payload["window_days"] == 30
    assert payload["total"] == 0

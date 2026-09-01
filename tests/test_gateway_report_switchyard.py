"""The per-target share table for external-mode turns.

The table has no pass-rate column. No turn-level outcome is recorded
anywhere in this codebase, so a pass rate would be a made-up number.
"""

from __future__ import annotations

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord
from opendaisugi.gateway_report import TargetShare, build_target_share_table

runner = CliRunner()


def _rec(
    model: str,
    tier: str = "tier-switchyard",
    input_tokens: int = 100,
    output_tokens: int = 50,
    downgraded: bool = False,
    saved: int = 0,
):
    return GatewayTurnRecord(
        created_at="2026-09-23T00:00:00Z",
        signature="sig",
        task="t",
        tier=tier,
        requested_model="claude-sonnet-5",
        model=model,
        difficulty=0.0,
        downgraded=downgraded,
        estimated=downgraded,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        frontier_tokens_saved=saved,
        actual_dollars=0.0,
        counterfactual_dollars=0.0,
    )


def test_share_table_sums_to_100_percent():
    records = [_rec("claude-sonnet-5"), _rec("qwen3-coder:30b"), _rec("qwen3-coder:30b")]
    table = build_target_share_table(records)
    assert sum(row.share for row in table) == pytest.approx(1.0)
    assert {row.model: row.turns for row in table} == {"claude-sonnet-5": 1, "qwen3-coder:30b": 2}
    assert [row.model for row in table] == ["qwen3-coder:30b", "claude-sonnet-5"]


def test_share_table_ignores_rules_mode_turns():
    records = [_rec("claude-haiku-4-5", tier="tier1-cheap"), _rec("qwen3-coder:30b")]
    table = build_target_share_table(records)
    assert [row.model for row in table] == ["qwen3-coder:30b"]
    assert table[0].share == 1.0


def test_share_table_is_empty_with_no_switchyard_turns():
    assert build_target_share_table([_rec("claude-haiku-4-5", tier="tier1-cheap")]) == []
    assert build_target_share_table([]) == []


def test_share_table_keeps_unknown_turns_in_their_own_row():
    table = build_target_share_table([_rec("unknown"), _rec("claude-sonnet-5")])
    assert {row.model for row in table} == {"unknown", "claude-sonnet-5"}


def test_share_table_reports_token_totals_and_savings_per_target():
    records = [
        _rec("qwen3-coder:30b", input_tokens=100, output_tokens=20, downgraded=True, saved=120),
        _rec("qwen3-coder:30b", input_tokens=200, output_tokens=30, downgraded=True, saved=230),
    ]
    row = build_target_share_table(records)[0]
    assert row.input_tokens == 300
    assert row.output_tokens == 50
    assert row.frontier_tokens_saved == 350


def test_share_table_has_no_pass_rate_column():
    assert set(TargetShare.__dataclass_fields__) == {
        "model",
        "turns",
        "share",
        "input_tokens",
        "output_tokens",
        "frontier_tokens_saved",
    }


def test_gateway_report_prints_the_share_table_even_without_the_search_extra(tmp_path, monkeypatch):
    def raising_embed(texts):
        raise ImportError("needs the [search] extra")

    monkeypatch.setattr("opendaisugi.gateway_cluster._lazy_embed", raising_embed)
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    for rec in (_rec("qwen3-coder:30b"), _rec("qwen3-coder:30b"), _rec("claude-sonnet-5")):
        journal.append(rec)
    result = runner.invoke(app, ["gateway-report", "--data-dir", str(tmp_path)])
    assert "Switchyard targets" in result.output
    assert "qwen3-coder:30b" in result.output
    assert "66.7%" in result.output
    assert "33.3%" in result.output


def test_the_report_and_the_pipeline_name_the_same_tier():
    from opendaisugi.gateway_pipeline import EXTERNAL_TIER
    from opendaisugi.gateway_report import _EXTERNAL_TIER

    assert _EXTERNAL_TIER == EXTERNAL_TIER

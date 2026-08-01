"""The calibration report: realized routing saving + potential reuse ceiling, kept separate.

``gateway_journal.summarize`` measures what the gateway's cheap/frontier routing decision
actually saved on turns already run — the REALIZED group. ``gateway_distill.
rank_reuse_candidates`` mines repeat clusters out of the same journal — the substrate for a
POTENTIAL reuse CEILING (every repeat after the first, served from a perfect fresh cache).
This module combines both into one report without ever letting the ceiling masquerade as a
measured saving. Unit tests inject a deterministic fake ``embed`` so clustering is tested
without loading any real model.

All fixture text is generic/public — never real user work content.
"""

from __future__ import annotations

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord, summarize, turn_signature
from opendaisugi.gateway_report import CalibrationReport, build_report

runner = CliRunner()


def _record(
    task: str,
    *,
    ask: str | None = None,
    downgraded: bool = True,
    input_tokens: int = 100,
    output_tokens: int = 50,
    cache_read_tokens: int = 0,
    cache_creation_tokens: int = 0,
    actual_dollars: float = 0.01,
    counterfactual_dollars: float = 0.05,
) -> GatewayTurnRecord:
    """Build a minimal GatewayTurnRecord for calibration-report tests.

    Mirrors ``tests/test_gateway_distill.py``'s ``_record`` helper. ``ask`` defaults to
    ``task`` (a plain single-turn ask); pass ``ask=""`` to build a tool-loop continuation
    record (empty signature).
    """
    resolved_ask = task if ask is None else ask
    signature = turn_signature(resolved_ask) if resolved_ask.strip() else ""
    return GatewayTurnRecord(
        created_at="2026-08-01T00:00:00Z",
        signature=signature,
        task=task,
        tier="fast" if downgraded else "frontier",
        requested_model="claude-opus-4-8",
        model="claude-haiku-4-8" if downgraded else "claude-opus-4-8",
        difficulty=0.1 if downgraded else 0.9,
        downgraded=downgraded,
        estimated=False,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        frontier_tokens_saved=(input_tokens + output_tokens) if downgraded else 0,
        actual_dollars=actual_dollars,
        counterfactual_dollars=counterfactual_dollars,
        cache_read_tokens=cache_read_tokens,
        cache_creation_tokens=cache_creation_tokens,
    )


def _fake_embed(mapping: dict[str, list[float]]):
    """A deterministic injectable ``embed`` — looks up a fixed vector per text."""

    def embed(texts: list[str]) -> list[list[float]]:
        return [mapping[t] for t in texts]

    return embed


# ---- realized routing group mirrors summarize() exactly ----


def test_realized_routing_group_matches_summarize() -> None:
    task_x = "summarize the onboarding docs"
    task_y = "draft the weekly status update"
    task_z = "explain the deploy pipeline"
    records = [
        _record(task_x, downgraded=True, input_tokens=100, output_tokens=50),
        _record(task_y, downgraded=True, input_tokens=200, output_tokens=80),
        _record(task_z, downgraded=False, input_tokens=500, output_tokens=300),
    ]
    embed = _fake_embed({task_x: [1.0, 0.0, 0.0], task_y: [0.0, 1.0, 0.0], task_z: [0.0, 0.0, 1.0]})

    expected = summarize(records)
    report = build_report(records, embed=embed)

    assert isinstance(report, CalibrationReport)
    assert report.turns == expected.turns
    assert report.downgraded_turns == expected.downgraded_turns
    assert report.routing_frontier_tokens_saved == expected.frontier_tokens_saved
    assert report.routing_dollars_saved == expected.dollars_saved
    assert report.routing_multiplier == expected.blended_multiplier


# ---- reuse recoverable: (count - 1) / count of a cluster's spend, singletons contribute 0 ----


def test_reuse_recoverable_is_repeats_after_first_and_singleton_contributes_zero() -> None:
    repeated_task = "summarize the onboarding docs"  # 3 identical occurrences, T=100 each
    singleton_task = "a one-off unrelated ask"  # 1 occurrence — must not add anything

    records = [
        _record(repeated_task, input_tokens=80, output_tokens=20, actual_dollars=0.03),
        _record(repeated_task, input_tokens=80, output_tokens=20, actual_dollars=0.03),
        _record(repeated_task, input_tokens=80, output_tokens=20, actual_dollars=0.03),
        _record(singleton_task, input_tokens=999, output_tokens=999, actual_dollars=9.0),
    ]
    embed = _fake_embed({repeated_task: [1.0, 0.0], singleton_task: [0.0, 1.0]})

    report = build_report(records, embed=embed)

    # T = 100 tokens/occurrence; 3T total; recoverable = 3T * 2/3 = 2T = 200.
    assert report.reuse_recoverable_tokens == 200
    # Dollars: 3 * 0.03 = 0.09 total; recoverable = 0.09 * 2/3 = 0.06.
    assert report.reuse_recoverable_dollars == pytest.approx(0.06)
    assert report.repeat_clusters == 1


# ---- combined multiplier: >= routing multiplier with reuse, == without ----


def test_combined_multiplier_is_at_least_routing_multiplier_when_reuse_recoverable() -> None:
    repeated_task = "summarize the onboarding docs"
    records = [
        _record(
            repeated_task,
            input_tokens=80,
            output_tokens=20,
            actual_dollars=0.03,
            counterfactual_dollars=0.15,
        ),
        _record(
            repeated_task,
            input_tokens=80,
            output_tokens=20,
            actual_dollars=0.03,
            counterfactual_dollars=0.15,
        ),
        _record(
            repeated_task,
            input_tokens=80,
            output_tokens=20,
            actual_dollars=0.03,
            counterfactual_dollars=0.15,
        ),
    ]
    embed = _fake_embed({repeated_task: [1.0, 0.0]})

    report = build_report(records, embed=embed)

    assert report.reuse_recoverable_dollars > 0.0
    assert report.combined_multiplier >= report.routing_multiplier
    assert report.combined_frontier_tokens_saved == (
        report.routing_frontier_tokens_saved + report.reuse_recoverable_tokens
    )


def test_combined_multiplier_equals_routing_multiplier_when_no_repeats() -> None:
    task_x = "summarize the onboarding docs"
    task_y = "draft the weekly status update"
    records = [
        _record(task_x, input_tokens=100, output_tokens=50),
        _record(task_y, input_tokens=200, output_tokens=80),
    ]
    embed = _fake_embed({task_x: [1.0, 0.0], task_y: [0.0, 1.0]})

    report = build_report(records, embed=embed)

    assert report.reuse_recoverable_dollars == 0.0
    assert report.reuse_recoverable_tokens == 0
    assert report.repeat_clusters == 0
    assert report.combined_multiplier == pytest.approx(report.routing_multiplier)


# ---- empty input: all zeros, multipliers 1.0, no divide-by-zero ----


def test_empty_records_returns_all_zeros_with_multipliers_one() -> None:
    report = build_report([])

    assert report == CalibrationReport(
        turns=0,
        downgraded_turns=0,
        routing_frontier_tokens_saved=0,
        routing_dollars_saved=0.0,
        routing_multiplier=1.0,
        cache_read_tokens=0,
        cache_creation_tokens=0,
        cache_hit_rate=0.0,
        local_turns=0,
        repeat_clusters=0,
        reuse_recoverable_tokens=0,
        reuse_recoverable_dollars=0.0,
        combined_frontier_tokens_saved=0,
        combined_multiplier=1.0,
    )


# ---- CLI ----


def test_gateway_report_cli_help() -> None:
    result = runner.invoke(app, ["gateway-report", "--help"])
    assert result.exit_code == 0
    assert "gateway-report" in result.output or "calibrat" in result.output.lower()


def test_gateway_report_cli_prints_three_sections(tmp_path, monkeypatch) -> None:
    task_a = "summarize the onboarding docs"
    task_b = "give a summary of the onboarding documentation"

    def fake_embed(texts: list[str]) -> list[list[float]]:
        mapping = {task_a: [1.0, 0.0], task_b: [0.999, 0.001]}
        return [mapping[t] for t in texts]

    # Inject a deterministic embed in place of the real lazy-loaded model, mirroring
    # cluster_repeats's own default-wiring point (see test_gateway_distill.py's CLI test) —
    # keeps this test fast and model-free, no GPU/CUDA involvement.
    monkeypatch.setattr("opendaisugi.gateway_cluster._lazy_embed", fake_embed)

    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    journal.append(_record(task_a))
    journal.append(_record(task_a))
    journal.append(_record(task_b))

    result = runner.invoke(app, ["gateway-report", "--data-dir", str(tmp_path)])

    assert result.exit_code == 0, result.output
    assert "Routing (realized)" in result.output
    assert "Reuse opportunity (ceiling)" in result.output
    assert "Combined (ceiling)" in result.output
    # The honesty note: never let the ceiling read as a measured saving.
    assert "ceiling" in result.output.lower()
    assert "assum" in result.output.lower()  # "assumes perfect ..." / "assuming ..."


def test_gateway_report_cli_empty_journal_is_friendly(tmp_path) -> None:
    result = runner.invoke(app, ["gateway-report", "--data-dir", str(tmp_path)])

    assert result.exit_code == 0, result.output
    assert "no turns recorded yet" in result.output.lower()


def test_gateway_report_cli_missing_search_extra_exits_nonzero(tmp_path, monkeypatch) -> None:
    # Simulate the [search] extra missing — cluster_repeats' default embed (_lazy_embed) is
    # exactly where that ImportError, with its install hint, is raised; see gateway_cluster.py.
    def raising_embed(texts: list[str]) -> list[list[float]]:
        raise ImportError(
            "Embedding-based repeat clustering requires the [search] extra: "
            "uv add 'opendaisugi[search]'  (or: pip install 'opendaisugi[search]')"
        )

    monkeypatch.setattr("opendaisugi.gateway_cluster._lazy_embed", raising_embed)

    task = "summarize the onboarding docs"
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    journal.append(_record(task))
    journal.append(_record(task))

    result = runner.invoke(app, ["gateway-report", "--data-dir", str(tmp_path)])

    assert result.exit_code != 0
    # Portable across click versions: <8.2 folds stderr into output and makes the .stderr
    # property raise ("not separately captured"); >=8.2 captures it separately. Read output
    # always, then add stderr when it's available.
    combined = result.output.lower()
    try:
        combined += (result.stderr or "").lower()
    except ValueError:
        pass  # older click already mixed stderr into output above
    assert "uv add" in combined or "pip install" in combined


def test_gateway_report_importable_without_search_extra() -> None:
    # Import alone must never reach the embedder (mirrors gateway_distill's discipline).
    import opendaisugi.gateway_report  # noqa: F401

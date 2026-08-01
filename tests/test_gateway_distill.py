"""The reuse-candidate worklist: repeat clusters ranked by frontier spend.

``gateway_cluster.cluster_repeats`` groups repeated (paraphrase-included) asks into
``RepeatCluster``s. This module turns those into a ranked, actionable worklist —
which repeated ask is most worth turning into a reusable pathway. Tokens are the
headline (the frontier quota, not the dollar figure, is what rate-limits a
subscription harness), dollars ride alongside.

Unit tests inject a deterministic fake ``embed`` so ranking logic is tested without
loading any real model. The CLI test injects a deterministic fake in place of the
real lazy-loaded embedder for the same reason — fast, model-free, no GPU/CUDA
involvement.

All fixture text is generic/public — never real user work content.
"""

from __future__ import annotations

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gateway_distill import ReuseCandidate, rank_reuse_candidates
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord, turn_signature

runner = CliRunner()


def _record(
    task: str,
    *,
    ask: str | None = None,
    input_tokens: int = 100,
    output_tokens: int = 50,
    cache_read_tokens: int = 0,
    cache_creation_tokens: int = 0,
    actual_dollars: float = 0.01,
    counterfactual_dollars: float = 0.05,
) -> GatewayTurnRecord:
    """Build a minimal GatewayTurnRecord for distill tests.

    ``ask`` defaults to ``task`` (a plain single-turn ask); pass ``ask=""`` to build
    a tool-loop continuation record (empty signature), mirroring ``record_turn``'s
    own rule and ``test_gateway_cluster.py``'s helper.
    """
    resolved_ask = task if ask is None else ask
    signature = turn_signature(resolved_ask) if resolved_ask.strip() else ""
    return GatewayTurnRecord(
        created_at="2026-08-01T00:00:00Z",
        signature=signature,
        task=task,
        tier="fast",
        requested_model="claude-opus-4-8",
        model="claude-haiku-4-8",
        difficulty=0.1,
        downgraded=True,
        estimated=False,
        input_tokens=input_tokens,
        output_tokens=output_tokens,
        frontier_tokens_saved=input_tokens + output_tokens,
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


# ---- ranking is by spend (total_tokens), not occurrence count ----


def test_ranking_is_by_total_tokens_not_occurrence_count() -> None:
    cheap_task = "summarize the onboarding docs"
    expensive_task = "refactor the entire billing module"
    records = [
        # 5 cheap occurrences: 5 * (80 in + 20 out) = 500 tokens total.
        _record(cheap_task, input_tokens=80, output_tokens=20),
        _record(cheap_task, input_tokens=80, output_tokens=20),
        _record(cheap_task, input_tokens=80, output_tokens=20),
        _record(cheap_task, input_tokens=80, output_tokens=20),
        _record(cheap_task, input_tokens=80, output_tokens=20),
        # 2 expensive occurrences: 2 * (6000 in + 4000 out) = 20000 tokens total.
        _record(expensive_task, input_tokens=6000, output_tokens=4000),
        _record(expensive_task, input_tokens=6000, output_tokens=4000),
    ]
    embed = _fake_embed({cheap_task: [1.0, 0.0], expensive_task: [0.0, 1.0]})

    candidates = rank_reuse_candidates(records, threshold=0.55, embed=embed)

    assert [c.cluster.representative_task for c in candidates] == [
        expensive_task,
        cheap_task,
    ]
    assert candidates[0].cluster.count == 2  # fewer occurrences...
    assert candidates[0].total_tokens == 20_000  # ...but ranked first on spend
    assert candidates[1].cluster.count == 5
    assert candidates[1].total_tokens == 500


# ---- total_tokens sums all four buckets; total_dollars sums actual_dollars ----


def test_total_tokens_sums_all_four_buckets_and_total_dollars_sums_actual() -> None:
    task = "compile the release notes"
    records = [
        _record(
            task,
            input_tokens=100,
            output_tokens=50,
            cache_read_tokens=30,
            cache_creation_tokens=20,
            actual_dollars=0.02,
            counterfactual_dollars=0.10,
        ),
        _record(
            task,
            input_tokens=200,
            output_tokens=75,
            cache_read_tokens=10,
            cache_creation_tokens=5,
            actual_dollars=0.03,
            counterfactual_dollars=0.15,
        ),
    ]
    embed = _fake_embed({task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(records, threshold=0.55, embed=embed)

    assert len(candidates) == 1
    candidate = candidates[0]
    assert isinstance(candidate, ReuseCandidate)
    # (100+30+20+50) + (200+10+5+75) = 200 + 290 = 490
    assert candidate.total_tokens == 490
    # Only actual_dollars is summed, never counterfactual.
    assert candidate.total_dollars == 0.05


# ---- already_reusable: found / not found / store error, never a crash ----


class _FoundStore:
    def find(self, task: str, **kwargs):
        return "a-pathway-match"  # any non-None sentinel


class _NotFoundStore:
    def find(self, task: str, **kwargs):
        return None


class _RaisingStore:
    def find(self, task: str, **kwargs):
        raise RuntimeError("store is unavailable")


def _two_occurrence_records() -> list[GatewayTurnRecord]:
    task = "draft the weekly status update"
    return [_record(task), _record(task)]


def test_already_reusable_true_when_store_finds_a_match() -> None:
    records = _two_occurrence_records()
    embed = _fake_embed({records[0].task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(
        records, threshold=0.55, embed=embed, pathway_store=_FoundStore()
    )

    assert len(candidates) == 1
    assert candidates[0].already_reusable is True


def test_already_reusable_false_when_store_finds_nothing() -> None:
    records = _two_occurrence_records()
    embed = _fake_embed({records[0].task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(
        records, threshold=0.55, embed=embed, pathway_store=_NotFoundStore()
    )

    assert len(candidates) == 1
    assert candidates[0].already_reusable is False


def test_already_reusable_false_and_no_crash_when_store_raises() -> None:
    records = _two_occurrence_records()
    embed = _fake_embed({records[0].task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(
        records, threshold=0.55, embed=embed, pathway_store=_RaisingStore()
    )

    assert len(candidates) == 1
    assert candidates[0].already_reusable is False


def test_already_reusable_false_when_no_store_given() -> None:
    records = _two_occurrence_records()
    embed = _fake_embed({records[0].task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(records, threshold=0.55, embed=embed)

    assert len(candidates) == 1
    assert candidates[0].already_reusable is False


# ---- continuations (empty signature) are excluded ----


def test_continuations_are_excluded() -> None:
    task = "run the migration"
    records = [
        _record(task, ask=""),
        _record(task, ask=""),
        _record(task, ask=""),
    ]
    embed = _fake_embed({task: [1.0, 0.0]})

    candidates = rank_reuse_candidates(records, threshold=0.55, embed=embed)

    assert candidates == []


# ---- empty input ----


def test_empty_records_returns_empty_list() -> None:
    embed = _fake_embed({})

    assert rank_reuse_candidates([], threshold=0.55, embed=embed) == []


# ---- CLI ----


def test_distill_repeats_cli_help() -> None:
    result = runner.invoke(app, ["distill-repeats", "--help"])
    assert result.exit_code == 0
    assert "distill-repeats" in result.output or "reuse" in result.output.lower()


def test_distill_repeats_cli_prints_worklist(tmp_path, monkeypatch) -> None:
    task_a = "summarize the onboarding docs"
    task_b = "give a summary of the onboarding documentation"

    def fake_embed(texts: list[str]) -> list[list[float]]:
        mapping = {task_a: [1.0, 0.0], task_b: [0.999, 0.001]}
        return [mapping[t] for t in texts]

    # Inject a deterministic embed in place of the real lazy-loaded model, mirroring
    # ``cluster_repeats``'s own default-wiring point — keeps the CLI test fast and
    # model-free, no GPU/CUDA involvement.
    monkeypatch.setattr("opendaisugi.gateway_cluster._lazy_embed", fake_embed)

    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    journal.append(_record(task_a))
    journal.append(_record(task_a))
    journal.append(_record(task_b))

    result = runner.invoke(app, ["distill-repeats", "--data-dir", str(tmp_path)])

    assert result.exit_code == 0, result.output
    assert task_a in result.output


def test_distill_repeats_cli_empty_journal_is_friendly(tmp_path) -> None:
    result = runner.invoke(app, ["distill-repeats", "--data-dir", str(tmp_path)])

    assert result.exit_code == 0, result.output
    assert "no turns recorded yet" in result.output.lower()


def test_distill_repeats_cli_missing_search_extra_exits_nonzero(tmp_path, monkeypatch) -> None:
    # Simulate the [search] extra missing — cluster_repeats' default embed
    # (_lazy_embed) is exactly where that ImportError, with its install hint,
    # is raised; see gateway_cluster.py.
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

    result = runner.invoke(app, ["distill-repeats", "--data-dir", str(tmp_path)])

    assert result.exit_code != 0
    # Portable across click versions: <8.2 folds stderr into output and makes the
    # .stderr property raise ("not separately captured"); >=8.2 captures it separately.
    # Read output always, then add stderr when it's available.
    combined = result.output.lower()
    try:
        combined += (result.stderr or "").lower()
    except ValueError:
        pass  # older click already mixed stderr into output above
    assert "uv add" in combined or "pip install" in combined

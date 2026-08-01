"""2D — the answer store: freshness-gated RAG over past answers (ADR-0012 §2D).

A repeated plain-question ask ("explain OAuth") produces a text answer with no plan
to verify, so the assurance verifier gives nothing. This tier substitutes FRESHNESS
for verification: retrieve the nearest past answer by embedding, and serve it only
if it is confident, recent, and its ground hasn't shifted — otherwise fall open to
the model (never raise, never serve a stale/low-confidence/ground-shifted answer).

Unit tests inject a deterministic fake ``embed`` (mirrors ``test_gateway_cluster``),
so the gate logic is tested without loading any real model, and inject ``now``
for deterministic age math — no real clock inside the pure gate.

All fixture text is generic/public — never real user work content.
"""

from __future__ import annotations

import os
import time

import pytest

from opendaisugi import Daisugi
from opendaisugi.gateway_answers import (
    AnswerEntry,
    AnswerResult,
    AnswerStore,
    capture_answer,
    recall_answer,
)
from opendaisugi.gateway_journal import turn_signature

pytest.importorskip("mcp")

from opendaisugi.mcp_server import build_server  # noqa: E402


def _entry(
    task: str,
    *,
    answer: str = "a canned answer",
    created_at: float = 1_000_000.0,
    ground_hash: str | None = None,
) -> AnswerEntry:
    return AnswerEntry(
        signature=turn_signature(task),
        task=task,
        answer=answer,
        created_at=created_at,
        ground_hash=ground_hash,
    )


def _fake_embed(mapping: dict[str, list[float]]):
    """A deterministic injectable ``embed`` — looks up a fixed vector per text.

    A KeyError on an unexpected text is a feature, not a nuisance: it fails
    loudly if task-normalization ever changes what gets embedded.
    """

    def embed(texts: list[str]) -> list[list[float]]:
        return [mapping[t] for t in texts]

    return embed


# ----- the freshness gate -----


def test_fresh_similar_recent_answer_is_a_hit():
    query = "explain OAuth to me"
    stored_task = "explain OAuth"
    entry = _entry(
        stored_task,
        answer="OAuth is a delegated-authorization protocol.",
        created_at=1_000_000.0,
    )
    embed = _fake_embed(
        {
            query: [1.0, 0.0],
            stored_task: [0.999, 0.001],  # near-identical unit vector -> paraphrase
        }
    )

    result = recall_answer(query, [entry], now=1_000_100.0, embed=embed)

    assert isinstance(result, AnswerResult)
    assert result.hit is True
    assert result.reason is None
    assert result.answer == "OAuth is a delegated-authorization protocol."
    assert result.provenance is not None
    assert result.provenance.similarity > 0.9
    assert result.provenance.age_seconds == pytest.approx(100.0)
    assert result.provenance.created_at == 1_000_000.0
    assert result.provenance.ground_hash is None


def test_low_similarity_is_a_miss():
    query = "explain OAuth"
    other_task = "what does this regex do"
    entry = _entry(other_task, created_at=1_000_000.0)
    embed = _fake_embed(
        {
            query: [1.0, 0.0],
            other_task: [0.0, 1.0],  # orthogonal -> similarity 0, well under threshold
        }
    )

    result = recall_answer(query, [entry], now=1_000_100.0, embed=embed)

    assert result.hit is False
    assert result.reason is not None
    assert "similar" in result.reason.lower()
    assert result.answer is None
    assert result.provenance is None


def test_too_old_is_a_miss_even_when_similar():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=0.0)
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.999, 0.001]})
    default_max_age = 7 * 24 * 3600.0
    now = default_max_age + 3600.0  # one hour past the default 7-day ceiling

    result = recall_answer(query, [entry], now=now, embed=embed)

    assert result.hit is False
    assert result.reason is not None
    assert "old" in result.reason.lower()
    assert result.answer is None
    assert result.provenance is None


def test_ground_shift_is_a_miss_when_both_hashes_known_and_differ():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=1_000_000.0, ground_hash="hash-A")
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.999, 0.001]})

    result = recall_answer(
        query, [entry], now=1_000_050.0, embed=embed, current_ground_hash="hash-B"
    )

    assert result.hit is False
    assert result.reason is not None
    assert "ground" in result.reason.lower()
    assert result.answer is None
    assert result.provenance is None


def test_ground_shift_does_not_block_when_current_hash_is_unknown():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=1_000_000.0, ground_hash="hash-A")
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.999, 0.001]})

    result = recall_answer(query, [entry], now=1_000_050.0, embed=embed, current_ground_hash=None)

    assert result.hit is True
    assert result.provenance is not None
    assert result.provenance.ground_hash == "hash-A"


def test_ground_shift_does_not_block_when_stored_hash_is_unknown():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=1_000_000.0, ground_hash=None)
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.999, 0.001]})

    result = recall_answer(
        query, [entry], now=1_000_050.0, embed=embed, current_ground_hash="hash-B"
    )

    assert result.hit is True


def test_empty_store_is_a_miss():
    result = recall_answer("explain OAuth", [], now=1_000_000.0, embed=_fake_embed({}))

    assert result.hit is False
    assert result.reason is not None
    assert result.answer is None
    assert result.provenance is None


def test_similarity_exactly_at_threshold_is_a_hit():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=1_000_000.0)
    # [0.55, 0.8351646544245033] is a unit vector at exactly cos^-1(0.55) from [1, 0],
    # so cosine_similarity([1, 0], this) == 0.55 precisely (not just scaled magnitude).
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.55, 0.8351646544245033]})

    result = recall_answer(query, [entry], now=1_000_050.0, embed=embed, threshold=0.55)

    assert result.hit is True
    assert result.provenance.similarity == pytest.approx(0.55)


def test_age_exactly_at_max_age_is_a_hit():
    query = "explain OAuth"
    stored_task = "explain OAuth briefly"
    entry = _entry(stored_task, created_at=0.0)
    embed = _fake_embed({query: [1.0, 0.0], stored_task: [0.999, 0.001]})

    result = recall_answer(query, [entry], now=100.0, embed=embed, max_age_seconds=100.0)

    assert result.hit is True
    assert result.provenance.age_seconds == pytest.approx(100.0)


def test_runtime_embedder_failure_is_a_miss_not_a_raise():
    # On this fail-open saving tier a runtime embedding failure (a crashed model, a
    # malformed return) must degrade to a clean miss, never raise into the harness's
    # turn — the same discipline as GatewayJournal.load() and _already_reusable.
    def broken_embed(texts: list[str]) -> list[list[float]]:
        raise RuntimeError("model crashed mid-encode")

    entry = _entry("explain OAuth", created_at=1_000_000.0)
    result = recall_answer("explain OAuth", [entry], now=1_000_050.0, embed=broken_embed)

    assert result.hit is False
    assert result.answer is None
    assert result.provenance is None
    assert "unavailable" in result.reason.lower()


def test_missing_search_extra_propagates_for_the_install_hint():
    # ImportError is the ONE exception recall_answer does not swallow: the boundary
    # (the MCP tool / a CLI) turns it into an actionable "install [search]" hint, so
    # it must reach them rather than being flattened into an indistinguishable miss.
    def missing_extra(texts: list[str]) -> list[list[float]]:
        raise ImportError("opendaisugi[search] is not installed")

    entry = _entry("explain OAuth", created_at=1_000_000.0)
    with pytest.raises(ImportError):
        recall_answer("explain OAuth", [entry], now=1_000_050.0, embed=missing_extra)


# ---- integration: the real lazy-loaded embedder ----


def test_real_embedder_finds_an_obvious_paraphrase() -> None:
    pytest.importorskip("sentence_transformers")
    # This box's GPU crashes the embedder; force CPU before the model loads.
    os.environ["CUDA_VISIBLE_DEVICES"] = ""

    query = "explain OAuth to me"
    stored_task = "explain OAuth"
    unrelated_task = "delete all temp files"
    answer_text = "OAuth is a delegated-authorization protocol."
    entries = [
        _entry(stored_task, answer=answer_text, created_at=1_000_000.0),
        _entry(unrelated_task, answer="rm -rf /tmp/*", created_at=1_000_000.0),
    ]

    result = recall_answer(query, entries, now=1_000_050.0)

    assert result.hit is True
    assert result.answer == answer_text


# ----- AnswerStore: bounded ring, append-only JSONL -----


def test_answer_store_round_trips_through_jsonl(tmp_path):
    path = tmp_path / "gw" / "answers.jsonl"  # parent dir does not exist yet
    store = AnswerStore(path=path)
    store.append(_entry("first question", answer="first answer"))
    store.append(_entry("second question", answer="second answer"))

    reopened = AnswerStore(path=path)
    loaded = reopened.load()
    assert len(loaded) == 2
    assert loaded[0].task == "first question"
    assert loaded[1].answer == "second answer"


def test_answer_store_bounded_ring_drops_oldest(tmp_path):
    path = tmp_path / "answers.jsonl"
    store = AnswerStore(path=path, max_entries=3)
    for i in range(5):
        store.append(_entry(f"question {i}", answer=f"answer {i}", created_at=float(i)))

    loaded = store.load()
    assert [e.task for e in loaded] == ["question 2", "question 3", "question 4"]
    assert len(loaded) == 3

    # Reopening a fresh handle sees the same trimmed ring — the eviction persisted.
    reloaded = AnswerStore(path=path, max_entries=3).load()
    assert [e.task for e in reloaded] == ["question 2", "question 3", "question 4"]


def test_answer_store_skips_a_partial_line_from_a_crash(tmp_path):
    # A process killed mid-append leaves a truncated JSON line. That must not brick
    # every future load() — mirrors GatewayJournal's crash-tolerance test.
    path = tmp_path / "answers.jsonl"
    store = AnswerStore(path=path)
    store.append(_entry("say hi", answer="hi"))
    with path.open("a", encoding="utf-8") as fh:
        fh.write('{"signature": "abc", "task": "trunc')  # truncated, no closing brace

    loaded = AnswerStore(path=path).load()
    assert len(loaded) == 1
    assert loaded[0].task == "say hi"


def test_answer_store_skips_a_line_missing_fields(tmp_path):
    path = tmp_path / "answers.jsonl"
    store = AnswerStore(path=path)
    store.append(_entry("say hi", answer="hi"))
    with path.open("a", encoding="utf-8") as fh:
        fh.write('{"unrelated": "blob"}\n')

    assert len(AnswerStore(path=path).load()) == 1


def test_answer_store_load_on_missing_file_returns_empty(tmp_path):
    store = AnswerStore(path=tmp_path / "does-not-exist.jsonl")
    assert store.load() == []


# ----- capture_answer -----


def test_capture_answer_builds_entry_and_appends(tmp_path):
    store = AnswerStore(path=tmp_path / "answers.jsonl")

    entry = capture_answer(
        store,
        task="explain OAuth",
        answer="OAuth is a delegated-authorization protocol.",
        created_at=123.0,
        ground_hash="h1",
    )

    assert isinstance(entry, AnswerEntry)
    assert entry.signature == turn_signature("explain OAuth")
    assert entry.task == "explain OAuth"
    assert entry.answer == "OAuth is a delegated-authorization protocol."
    assert entry.created_at == 123.0
    assert entry.ground_hash == "h1"

    loaded = store.load()
    assert loaded == [entry]


# ----- MCP tool shape -----


async def test_mcp_recall_answer_tool_hit(tmp_path, monkeypatch):
    query = "explain OAuth to me"
    stored_task = "explain OAuth"

    import opendaisugi.gateway_answers as gateway_answers_mod

    def fake_lazy_embed(texts: list[str]) -> list[list[float]]:
        mapping = {query: [1.0, 0.0], stored_task: [0.999, 0.001]}
        return [mapping[t] for t in texts]

    monkeypatch.setattr(gateway_answers_mod, "_lazy_embed", fake_lazy_embed)

    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)
    capture_answer(
        d.answer_store,
        task=stored_task,
        answer="OAuth is a delegated-authorization protocol.",
        created_at=time.time() - 10.0,
    )

    server = build_server(d)
    _, structured = await server.call_tool("recall_answer", {"task": query})

    assert structured["hit"] is True
    assert structured["reason"] is None
    assert structured["answer"] == "OAuth is a delegated-authorization protocol."
    assert structured["provenance"]["similarity"] > 0.9
    assert structured["provenance"]["ground_hash"] is None


async def test_mcp_recall_answer_tool_no_store(tmp_path):
    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False, answer_store=False)

    server = build_server(d)
    _, structured = await server.call_tool("recall_answer", {"task": "anything"})

    assert structured == {
        "hit": False,
        "reason": "no answer store",
        "answer": None,
        "provenance": None,
    }


async def test_mcp_recall_answer_tool_falls_open_when_search_extra_missing(tmp_path, monkeypatch):
    """The 2D fail-open discipline at the MCP boundary: a missing ``[search]``
    extra (``_lazy_embed`` raising ``ImportError``) must surface as a clean
    miss, never a tool-call error, so the caller falls open to the model."""
    import opendaisugi.gateway_answers as gateway_answers_mod

    def raising_lazy_embed(texts: list[str]) -> list[list[float]]:
        raise ImportError("opendaisugi[search] is not installed")

    monkeypatch.setattr(gateway_answers_mod, "_lazy_embed", raising_lazy_embed)

    d = Daisugi(data_dir=tmp_path, cache=False, pathway_store=False)
    capture_answer(d.answer_store, task="explain OAuth", answer="a", created_at=time.time())

    server = build_server(d)
    _, structured = await server.call_tool("recall_answer", {"task": "explain OAuth"})

    assert structured == {
        "hit": False,
        "reason": "answer recall unavailable: install the [search] extra",
        "answer": None,
        "provenance": None,
    }

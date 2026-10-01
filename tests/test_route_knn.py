"""The knn router: the outcome table, the decision rule and the outcome labels.

Vectors here are small unit vectors, so each case is easy to check by hand. No model
and no network are used.
"""

from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import numpy as np
import pytest

from opendaisugi import route_knn as rk

OPUS = "claude-opus-4-8"
HAIKU = "claude-haiku-4-5"
PRICES = {OPUS: (15.0, 75.0), HAIKU: (1.0, 5.0)}
EMB = "lexical"


def _unit(*xs: float) -> np.ndarray:
    v = np.array(xs, dtype=np.float64)
    return (v / np.linalg.norm(v)).astype("<f4")


def _row(seq: int, vec, model: str, outcome: str, *, ts: float = 0.0, conv: str = "c") -> rk.Row:
    return rk.Row(seq, f"t{seq}", conv, ts, "sha", EMB, vec, model, 0.0, None, outcome)


Q = _unit(1, 0)
NEAR = _unit(1, 0.1)  # cosine about 0.995 to Q
MID = _unit(1, 1)  # cosine about 0.707 to Q
FAR = _unit(0, 1)  # cosine 0 to Q


def _decide(rows, **kw):
    args = {
        "requested": OPUS,
        "candidates": [OPUS, HAIKU],
        "prices": PRICES,
        "threshold": 0.25,
    }
    args.update(kw)
    return rk.decide(Q, rows, **args)


def test_vectors_round_trip_as_float32_little_endian() -> None:
    vec = _unit(1, 2, 3)
    blob = rk.pack_vec(vec)
    assert len(blob) == 12
    assert blob[:4] == np.float32(vec[0]).tobytes()
    assert rk.unpack_vec(blob).tolist() == vec.tolist()


def test_round6_is_floor_of_scaled_plus_half() -> None:
    assert rk.round6(0.1234565) == pytest.approx(0.123457)
    assert rk.round6(0.9999999) == 1.0


def test_similarity_of_a_length_mismatch_never_matches() -> None:
    assert rk.similarity(_unit(1, 0), _unit(1, 0, 0)) == -1.0


def test_fewer_than_three_known_neighbours_falls_back() -> None:
    rows = [
        _row(1, NEAR, HAIKU, rk.GOOD),
        _row(2, NEAR, HAIKU, rk.GOOD),
        _row(3, NEAR, HAIKU, rk.UNKNOWN),
    ]
    d = _decide(rows)
    assert d.fallback is True
    assert d.model == OPUS
    assert d.known == 2
    assert "2 of 3 neighbours have a known outcome (need 3)" in d.reason


def test_neighbours_below_the_threshold_are_ignored() -> None:
    rows = [_row(i, FAR, HAIKU, rk.GOOD) for i in range(1, 6)]
    d = _decide(rows)
    assert d.fallback is True
    assert d.neighbours == []


def test_a_cheaper_model_that_did_well_wins() -> None:
    rows = [_row(i, NEAR, HAIKU, rk.GOOD) for i in range(1, 5)]
    rows.append(_row(5, NEAR, OPUS, rk.GOOD))
    d = _decide(rows)
    assert d.fallback is False
    assert d.model == HAIKU
    assert d.known == 5
    assert "knn: claude-haiku-4-5; 5 neighbours, 5 known; scores " in d.reason


def test_a_cheaper_model_that_did_badly_loses_to_the_requested_one() -> None:
    rows = [_row(i, NEAR, HAIKU, rk.BAD) for i in range(1, 5)]
    rows.append(_row(5, NEAR, OPUS, rk.GOOD))
    d = _decide(rows)
    assert d.model == OPUS


def test_a_cheaper_model_with_no_known_neighbour_is_not_eligible() -> None:
    rows = [_row(i, NEAR, OPUS, rk.BAD) for i in range(1, 5)]
    d = _decide(rows)
    assert d.fallback is False
    assert d.model == OPUS
    assert [m for m, _ in d.scores] == [OPUS]


def test_knn_never_picks_a_model_above_the_requested_one() -> None:
    # Rows say opus did well. The request asks for haiku. Opus is not a candidate.
    rows = [_row(i, NEAR, OPUS, rk.GOOD) for i in range(1, 6)]
    cands = rk.candidate_models(HAIKU, cheap_model=HAIKU, local_model=None, prices=PRICES)
    assert cands == [HAIKU]
    d = _decide(rows, requested=HAIKU, candidates=cands)
    assert d.model == HAIKU


def test_candidates_are_the_requested_model_then_the_cheaper_rungs() -> None:
    prices = {**PRICES, "local": (0.0, 0.0)}
    assert rk.candidate_models(OPUS, cheap_model=HAIKU, local_model="local", prices=prices) == [
        OPUS,
        "local",
        HAIKU,
    ]
    # A rung with no price is not shown cheaper, so it is not a candidate.
    assert rk.candidate_models(OPUS, cheap_model="mystery", local_model=None, prices=PRICES) == [
        OPUS
    ]


def test_only_the_eight_nearest_count() -> None:
    rows = [_row(i, NEAR, HAIKU, rk.GOOD) for i in range(1, 9)]
    rows += [_row(i, MID, OPUS, rk.GOOD) for i in range(9, 20)]
    d = _decide(rows)
    assert len(d.neighbours) == 8
    assert {n.model for n in d.neighbours} == {HAIKU}


def test_the_newer_row_wins_a_similarity_tie() -> None:
    rows = [_row(i, NEAR, HAIKU, rk.GOOD) for i in range(1, 10)]
    d = _decide(rows)
    assert [n.seq for n in d.neighbours] == [9, 8, 7, 6, 5, 4, 3, 2]


def test_previous_turn_is_bad_on_a_retry_and_good_when_the_user_moves_on() -> None:
    prev = _row(1, NEAR, HAIKU, rk.UNKNOWN, ts=1000.0)
    kw = {"now": 1100.0, "threshold": 0.25}
    assert rk.label_previous(prev, NEAR, EMB, **kw) == rk.BAD
    assert rk.label_previous(prev, FAR, EMB, **kw) == rk.GOOD


def test_previous_turn_keeps_its_label_outside_the_window_or_when_known() -> None:
    kw = {"threshold": 0.25}
    old = _row(1, NEAR, HAIKU, rk.UNKNOWN, ts=1000.0)
    assert rk.label_previous(old, FAR, EMB, now=1000.0 + 601, **kw) is None
    assert rk.label_previous(old, FAR, EMB, now=1000.0 + 600, **kw) == rk.GOOD
    known = _row(1, NEAR, HAIKU, rk.BAD, ts=1000.0)
    assert rk.label_previous(known, FAR, EMB, now=1010.0, **kw) is None
    assert rk.label_previous(None, FAR, EMB, now=1010.0, **kw) is None
    other = _row(1, NEAR, HAIKU, rk.UNKNOWN, ts=1000.0)
    assert rk.label_previous(other, FAR, "potion", now=1010.0, **kw) is None


def test_store_keeps_the_vector_as_a_float32_blob_and_no_text(tmp_path: Path) -> None:
    store = rk.OutcomeStore(tmp_path / "gateway" / "routing.db")
    tid = store.add(
        conversation="c",
        ts=5.0,
        task_sha=rk.task_sha256("secret task"),
        embedder=EMB,
        vec=NEAR,
        model=HAIKU,
        cost=0.01,
        latency_ms=12.5,
    )
    assert tid == "t1"
    con = sqlite3.connect(tmp_path / "gateway" / "routing.db")
    vec, sha, outcome = con.execute("SELECT vec, task_sha256, outcome FROM turns").fetchone()
    assert isinstance(vec, bytes) and len(vec) == 8
    assert sha == rk.task_sha256("secret task")
    assert outcome == "unknown"
    dump = json.dumps(con.execute("SELECT * FROM turns").fetchall(), default=repr)
    assert "secret task" not in dump


def test_store_set_outcome_and_unknown_turn(tmp_path: Path) -> None:
    store = rk.OutcomeStore(tmp_path / "routing.db")
    assert store.set_outcome("t1", rk.GOOD) is False  # no table yet
    tid = store.add(
        conversation="c",
        ts=1.0,
        task_sha="s",
        embedder=EMB,
        vec=NEAR,
        model=HAIKU,
        cost=0.0,
        latency_ms=None,
    )
    assert store.set_outcome(tid, rk.BAD) is True
    assert store.set_outcome("t99", rk.BAD) is False
    assert store.recent(EMB)[0].outcome == rk.BAD
    with pytest.raises(ValueError):
        store.set_outcome(tid, "maybe")


def test_store_keeps_only_the_newest_rows(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setattr(rk, "MAX_ROWS", 3)
    store = rk.OutcomeStore(tmp_path / "routing.db")
    for i in range(5):
        store.add(
            conversation="c",
            ts=float(i),
            task_sha="s",
            embedder=EMB,
            vec=NEAR,
            model=HAIKU,
            cost=0.0,
            latency_ms=None,
        )
    assert [r.turn_id for r in store.recent(EMB)] == ["t5", "t4", "t3"]


def test_router_records_a_turn_and_labels_the_previous_one(tmp_path: Path) -> None:
    router = rk.KnnRouter(rk.OutcomeStore(tmp_path / "routing.db"), None, 0.25)

    def record(vec, now, failed=False):
        return router.record(
            conversation="c",
            task="x",
            vec=vec,
            embedder=EMB,
            model=HAIKU,
            cost=0.0,
            latency_ms=None,
            failed=failed,
            now=now,
        )

    first = record(NEAR, 100.0)
    second = record(NEAR, 160.0)  # a retry: the first was bad
    third = record(FAR, 200.0)  # moved on: the second was good
    by_id = {r.turn_id: r for r in router.store.recent(EMB)}
    assert by_id[first].outcome == rk.BAD
    assert by_id[second].outcome == rk.GOOD
    assert by_id[third].outcome == rk.UNKNOWN
    failed = record(FAR, 210.0, failed=True)
    assert {r.turn_id: r for r in router.store.recent(EMB)}[failed].outcome == rk.BAD


def test_outcome_source_is_stored_and_checked(tmp_path: Path) -> None:
    store = rk.OutcomeStore(tmp_path / "routing.db")
    tid = store.add(
        conversation="c",
        ts=1.0,
        task_sha="s",
        embedder=EMB,
        vec=NEAR,
        model=HAIKU,
        cost=0.0,
        latency_ms=None,
    )
    con = sqlite3.connect(tmp_path / "routing.db")
    assert con.execute("SELECT source FROM turns").fetchone() == ("auto",)
    assert store.set_outcome(tid, rk.BAD)  # a person by default
    assert con.execute("SELECT outcome, source FROM turns").fetchone() == ("bad", "human")
    assert not store.set_outcome(tid, rk.GOOD, "grader")  # a person's word is kept
    assert con.execute("SELECT source FROM turns").fetchone() == ("human",)
    with pytest.raises(ValueError):
        store.set_outcome(tid, rk.GOOD, "robot")


def test_the_scorer_is_a_pure_function_of_neighbours_and_candidates() -> None:
    nbrs = [rk.Neighbour(f"t{i}", 1.0, HAIKU, rk.GOOD, i) for i in range(4)]
    best, scores = rk.score_candidates(nbrs, [OPUS, HAIKU], OPUS, PRICES)
    assert best == HAIKU and [m for m, _ in scores] == [OPUS, HAIKU]

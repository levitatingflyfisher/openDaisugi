"""The knn router: send a turn to the model that did well on similar past tasks.

This is a fourth choice for the gateway's router slot (``gateway_router: knn``).
The gateway records one row per routed turn: a hash of the task, its vector, the
model, the cost, the latency and an outcome. To route a new turn it embeds the
task, finds the nearest past rows, and scores each candidate model by the
similarity-weighted share of good outcomes, less a cost term.

Rulings RT-R-1 to RT-R-7 in ``clients/ADJUDICATIONS.md`` fix every number here.
The Go and Rust gateways use the same rules, and the three are compared on a
fixed outcome table.

The text of a task is never stored. Only its SHA-256 and its vector are.
"""

from __future__ import annotations

import hashlib
import math
import sqlite3
import struct
import time
from collections.abc import Sequence
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

GOOD = "good"
BAD = "bad"
UNKNOWN = "unknown"
OUTCOMES = (GOOD, BAD, UNKNOWN)
# Who wrote an outcome: the gateway (auto), a person (human), or a later grader model.
SOURCES = ("auto", "human", "grader")
# Human feedback beats the grader, and the grader beats the automatic labels.
SOURCE_RANK = {"auto": 0, "grader": 1, "human": 2}

K_NEIGHBOURS = 8
MIN_KNOWN = 3
COST_WEIGHT = 0.2
MAX_ROWS = 2000
RETRY_WINDOW_S = 600.0
SIM_SCALE = 1_000_000.0

_SCHEMA = """
CREATE TABLE IF NOT EXISTS turns (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT UNIQUE,
    conversation TEXT NOT NULL,
    ts REAL NOT NULL,
    task_sha256 TEXT NOT NULL,
    embedder TEXT NOT NULL,
    vec BLOB NOT NULL,
    model TEXT NOT NULL,
    cost REAL NOT NULL,
    latency_ms REAL,
    outcome TEXT NOT NULL DEFAULT 'unknown',
    source TEXT NOT NULL DEFAULT 'auto'
)
"""


def pack_vec(vec: Sequence[float]) -> bytes:
    """A vector as float32, little endian."""
    return np.asarray(vec, dtype="<f4").tobytes()


def unpack_vec(blob: bytes) -> np.ndarray:
    """The float32 little-endian vector in ``blob``."""
    return np.frombuffer(blob[: len(blob) // 4 * 4], dtype="<f4")


def f32_vec(vec: Sequence[float]) -> np.ndarray:
    """A vector rounded to float32, little endian, as it is stored."""
    return np.asarray(vec, dtype="<f4")


def f32(x: float) -> float:
    """``x`` rounded to float32, as a Python float."""
    return struct.unpack("<f", struct.pack("<f", x))[0]


def round6(x: float) -> float:
    """Round to six places the same way in every port: floor(x * 1e6 + 0.5) / 1e6."""
    return math.floor(x * SIM_SCALE + 0.5) / SIM_SCALE


def _ordered_sum(prod: np.ndarray) -> float:
    """Sum left to right, as the Go and Rust ports do. BLAS may add in another order."""
    return float(np.cumsum(prod)[-1]) if len(prod) else 0.0


def similarity(a: Sequence[float], b: Sequence[float]) -> float:
    """Dot product of two float32 vectors in float64, rounded to six places.

    Both vectors are unit length, so this is the cosine. A length mismatch gives -1
    so a row from another embedder dimension never matches.
    """
    if len(a) != len(b):
        return -1.0
    prod = np.asarray(a, dtype=np.float64) * np.asarray(b, dtype=np.float64)
    return round6(_ordered_sum(prod))


def similarities(query: Sequence[float], rows: Sequence["Row"]) -> list[float]:
    """``similarity(query, row.vec)`` for every row, in one matrix product per dimension."""
    q = np.asarray(query, dtype=np.float64)
    out = [-1.0] * len(rows)
    idx = [i for i, r in enumerate(rows) if len(r.vec) == len(q)]
    if idx:
        m = np.stack([rows[i].vec for i in idx]).astype(np.float64)
        dots = np.cumsum(m * q, axis=1)[:, -1] if len(q) else np.zeros(len(idx))
        rounded = np.floor(dots * SIM_SCALE + 0.5) / SIM_SCALE
        for i, v in zip(idx, rounded, strict=True):
            out[i] = float(v)
    return out


def task_sha256(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


@dataclass
class Row:
    seq: int
    turn_id: str
    conversation: str
    ts: float
    task_sha256: str
    embedder: str
    vec: np.ndarray
    model: str
    cost: float
    latency_ms: float | None
    outcome: str
    source: str = "auto"


@dataclass
class Neighbour:
    turn_id: str
    similarity: float
    model: str
    outcome: str
    seq: int = 0


@dataclass
class KnnDecision:
    """What the knn router chose. ``fallback`` means: too few known outcomes, so
    the caller must use the rules router and carry ``reason`` in front of its own."""

    model: str
    fallback: bool
    reason: str
    neighbours: list[Neighbour] = field(default_factory=list)
    known: int = 0
    scores: list[tuple[str, float]] = field(default_factory=list)


class OutcomeStore:
    """The routing table, in ``<data-dir>/gateway/routing.db``."""

    def __init__(self, path: Path) -> None:
        self.path = Path(path)

    def _connect(self) -> sqlite3.Connection:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        con = sqlite3.connect(self.path)
        con.execute(_SCHEMA)
        return con

    def add(
        self,
        *,
        conversation: str,
        ts: float,
        task_sha: str,
        embedder: str,
        vec: Sequence[float],
        model: str,
        cost: float,
        latency_ms: float | None,
        outcome: str = UNKNOWN,
        source: str = "auto",
    ) -> str:
        """Insert one row and return its turn id, ``t<seq>``. Keeps the newest MAX_ROWS."""
        con = self._connect()
        try:
            with con:
                cur = con.execute(
                    "INSERT INTO turns (conversation, ts, task_sha256, embedder, vec, model,"
                    " cost, latency_ms, outcome, source) VALUES (?,?,?,?,?,?,?,?,?,?)",
                    (
                        conversation,
                        ts,
                        task_sha,
                        embedder,
                        pack_vec(vec),
                        model,
                        cost,
                        latency_ms,
                        outcome,
                        source,
                    ),
                )
                seq = cur.lastrowid
                turn_id = f"t{seq}"
                con.execute("UPDATE turns SET turn_id=? WHERE seq=?", (turn_id, seq))
                con.execute("DELETE FROM turns WHERE seq <= ?", (seq - MAX_ROWS,))
            return turn_id
        finally:
            con.close()

    def set_outcome(self, turn_id: str, outcome: str, source: str = "human") -> bool:
        """Set a row's outcome. False when no such turn id exists."""
        if outcome not in (GOOD, BAD):
            raise ValueError("outcome must be good or bad")
        if source not in SOURCES:
            raise ValueError("source must be auto, human or grader")
        if not self.path.exists():
            return False
        con = self._connect()
        try:
            with con:
                cur = con.execute(
                    "UPDATE turns SET outcome=?, source=? WHERE turn_id=? AND "
                    "CASE source WHEN 'human' THEN 2 WHEN 'grader' THEN 1 ELSE 0 END <= ?",
                    (outcome, source, turn_id, SOURCE_RANK[source]),
                )
            return cur.rowcount > 0
        finally:
            con.close()

    def _rows(self, where: str, args: tuple, limit: int) -> list[Row]:
        if not self.path.exists():
            return []
        con = self._connect()
        try:
            cur = con.execute(
                "SELECT seq, turn_id, conversation, ts, task_sha256, embedder, vec, model,"
                f" cost, latency_ms, outcome, source FROM turns WHERE {where} ORDER BY seq DESC LIMIT ?",
                (*args, limit),
            )
            return [
                Row(
                    seq=r[0],
                    turn_id=r[1] or f"t{r[0]}",
                    conversation=r[2],
                    ts=r[3],
                    task_sha256=r[4],
                    embedder=r[5],
                    vec=unpack_vec(r[6]),
                    model=r[7],
                    cost=r[8],
                    latency_ms=r[9],
                    outcome=r[10],
                    source=r[11],
                )
                for r in cur.fetchall()
            ]
        finally:
            con.close()

    def recent(self, embedder: str, limit: int = MAX_ROWS) -> list[Row]:
        """The newest rows made by ``embedder``, newest first."""
        return self._rows("embedder=?", (embedder,), limit)

    def last_in_conversation(self, conversation: str) -> Row | None:
        rows = self._rows("conversation=?", (conversation,), 1)
        return rows[0] if rows else None

    def count(self) -> int:
        if not self.path.exists():
            return 0
        con = self._connect()
        try:
            return con.execute("SELECT COUNT(*) FROM turns").fetchone()[0]
        finally:
            con.close()


def candidate_models(
    requested: str,
    *,
    cheap_model: str,
    local_model: str | None,
    prices: dict[str, tuple[float, float]],
) -> list[str]:
    """The models knn may pick: the requested one first, then the cheaper rungs.

    A rung is a candidate only when the price table shows it strictly cheaper than
    the requested model. knn never picks a model above the requested one.
    """
    from opendaisugi.gateway import _strictly_cheaper

    out = [requested]
    for rung in (local_model, cheap_model):
        if rung and rung not in out and _strictly_cheaper(rung, requested, prices):
            out.append(rung)
    return out


def _unit_price(model: str, prices: dict[str, tuple[float, float]]) -> float:
    from opendaisugi.gateway import _FALLBACK_PRICE

    per_in, per_out = prices.get(model, _FALLBACK_PRICE)
    return per_in + per_out


def _success(neighbours: Sequence[Neighbour], model: str) -> tuple[float, int, int]:
    """(success, known, good) for one model: (sum sim of good + 1) / (sum sim of known + 2)."""
    num = 0.0
    den = 0.0
    known = 0
    good = 0
    for n in neighbours:
        if n.model != model or n.outcome not in (GOOD, BAD):
            continue
        known += 1
        den += n.similarity
        if n.outcome == GOOD:
            good += 1
            num += n.similarity
    return (num + 1.0) / (den + 2.0), known, good


def score_candidates(
    neighbours: Sequence[Neighbour],
    candidates: Sequence[str],
    requested: str,
    prices: dict[str, tuple[float, float]],
) -> tuple[str, list[tuple[str, float]]]:
    """The default scorer: a pure function of the neighbours and the candidates.

    Returns the best model and every eligible (model, score). Another scorer
    can replace it: ``decide`` only needs this shape. A cheaper model is eligible
    only with a good neighbour and a success value (before the cost term) at or
    above the requested model's (RT-R-3a).
    """
    requested_price = _unit_price(requested, prices)
    requested_success = _success(neighbours, requested)[0]
    scores: list[tuple[str, float]] = []
    best_model = requested
    best_score = -math.inf
    for model in candidates:
        success, _, good = _success(neighbours, model)
        if model != requested and (good == 0 or success < requested_success):
            continue
        ratio = 0.0 if requested_price <= 0 else _unit_price(model, prices) / requested_price
        score = success - COST_WEIGHT * ratio
        scores.append((model, score))
        if score > best_score:
            best_score = score
            best_model = model
    return best_model, scores


def decide(
    query: Sequence[float],
    rows: Sequence[Row],
    *,
    requested: str,
    candidates: Sequence[str],
    prices: dict[str, tuple[float, float]],
    threshold: float,
    k: int = K_NEIGHBOURS,
    min_known: int = MIN_KNOWN,
) -> KnnDecision:
    """Pick a model for one turn from its nearest past rows.

    Take the ``k`` nearest rows at or above ``threshold`` (ties: the newer row first).
    With fewer than ``min_known`` of them known and made by a candidate model, answer
    ``fallback``. Otherwise score each candidate: (sum of similarity over its good
    rows + 1) / (sum of similarity over its known rows + 2), less COST_WEIGHT times
    its price over the requested model's price. The requested model is always
    eligible. A cheaper model is eligible only with a good neighbour and a success
    value at or above the requested model's. The best score wins; a tie goes to the
    earlier candidate.
    """
    scored = []
    for sim, row in zip(similarities(query, rows), rows, strict=True):
        if sim >= threshold:
            scored.append((sim, row))
    scored.sort(key=lambda p: (-p[0], -p[1].seq))
    near = scored[:k]
    neighbours = [Neighbour(r.turn_id, s, r.model, r.outcome, r.seq) for s, r in near]
    known = sum(1 for n in neighbours if n.model in candidates and n.outcome in (GOOD, BAD))
    if known < min_known:
        reason = (
            f"knn: {known} of {len(neighbours)} neighbours have a known outcome (need {min_known})"
        )
        return KnnDecision(requested, True, reason, neighbours, known, [])

    best_model, scores = score_candidates(neighbours, candidates, requested, prices)
    ranked = sorted(scores, key=lambda p: -p[1])
    shown = ", ".join(f"{m} {s:.2f}" for m, s in ranked[:3])
    reason = f"knn: {best_model}; {len(neighbours)} neighbours, {known} known; scores {shown}"
    return KnnDecision(best_model, False, reason, neighbours, known, ranked)


def label_previous(
    prev: Row | None,
    vec: Sequence[float],
    embedder: str,
    *,
    now: float,
    threshold: float,
) -> str | None:
    """The outcome the next human turn gives the previous turn, or None for no change.

    Only an unknown row made by the same embedder, in the last RETRY_WINDOW_S seconds,
    is labelled. A retry (similarity at or above ``threshold``) makes it bad. A turn
    that moves on makes it good.
    """
    if prev is None or prev.outcome != UNKNOWN or prev.embedder != embedder:
        return None
    if now - prev.ts > RETRY_WINDOW_S:
        return None
    return BAD if similarity(prev.vec, vec) >= threshold else GOOD


class KnnRouter:
    """What a gateway needs to route and to learn: a store, an embedder and a threshold.

    ``embed`` takes text and returns ``(vector, identity)``. It may raise; the gateway
    then falls back to the rules router for that turn.
    """

    def __init__(self, store: OutcomeStore, embed, threshold: float) -> None:
        self.store = store
        self.embed = embed
        self.threshold = threshold

    def record(
        self,
        *,
        conversation: str,
        task: str,
        vec: Sequence[float],
        embedder: str,
        model: str,
        cost: float,
        latency_ms: float | None,
        failed: bool,
        now: float | None = None,
    ) -> str:
        """Book one finished turn, and label the previous turn of this conversation."""
        when = time.time() if now is None else now
        prev = self.store.last_in_conversation(conversation)
        label = label_previous(prev, vec, embedder, now=when, threshold=self.threshold)
        if label is not None and prev is not None:
            self.store.set_outcome(prev.turn_id, label, "auto")
        return self.store.add(
            conversation=conversation,
            ts=when,
            task_sha=task_sha256(task),
            embedder=embedder,
            vec=vec,
            model=model,
            cost=cost,
            latency_ms=latency_ms,
            outcome=BAD if failed else UNKNOWN,
        )


def outcome_summary(store: OutcomeStore, recent: int = 10) -> dict:
    """Counts by outcome, and the last few booked turns, oldest first."""
    rows = store._rows("1=1", (), MAX_ROWS)
    counts = {GOOD: 0, BAD: 0, UNKNOWN: 0}
    for r in rows:
        counts[r.outcome] = counts.get(r.outcome, 0) + 1
    last = [
        {"turn_id": r.turn_id, "model": r.model, "outcome": r.outcome}
        for r in reversed(rows[:recent])
    ]
    return {
        "path": str(store.path),
        "booked": len(rows),
        "good": counts[GOOD],
        "bad": counts[BAD],
        "unknown": counts[UNKNOWN],
        "recent": last,
    }


def knn_view(
    task: str,
    data_dir: Path,
    *,
    requested: str,
    cheap_model: str,
    prices: dict[str, tuple[float, float]] | None = None,
    embed=None,
    threshold: float | None = None,
) -> dict:
    """The knn router's choice for ``task``, with its nearest past turns, as plain data.

    Read-only: it books nothing. ``route --knn`` prints it.
    """
    from opendaisugi._search import active_threshold
    from opendaisugi.gateway import _PRICES_PER_MTOK

    prices = _PRICES_PER_MTOK if prices is None else prices
    try:
        vec, identity = (embed or default_embed)(task)
        cut = active_threshold() if threshold is None else threshold
    except Exception:
        return {"error": "could not embed the task"}
    store = OutcomeStore(Path(data_dir) / "gateway" / "routing.db")
    cands = candidate_models(requested, cheap_model=cheap_model, local_model=None, prices=prices)
    kd = decide(
        vec,
        store.recent(identity),
        requested=requested,
        candidates=cands,
        prices=prices,
        threshold=cut,
    )
    return {
        "model": kd.model,
        "fallback": kd.fallback,
        "reason": kd.reason,
        "known": kd.known,
        "threshold": cut,
        "neighbours": [
            {
                "turn_id": n.turn_id,
                "similarity": n.similarity,
                "model": n.model,
                "outcome": n.outcome,
            }
            for n in kd.neighbours
        ],
        "scores": [{"model": m, "score": float(f"{s:.4f}")} for m, s in kd.scores],
    }


def knn_view_lines(view: dict) -> list[str]:
    """The text form of :func:`knn_view`."""
    if "error" in view:
        return [f"  knn:        {view['error']}"]
    reason = view["reason"].removeprefix("knn: ")
    if view["fallback"]:
        reason += "; the rules router decides"
    out = [f"  knn:        {reason}", f"  threshold:  {view['threshold']:.2f}"]
    if not view["neighbours"]:
        out.append("  neighbours: none at or above the threshold")
        return out
    out.append(f"  neighbours: {len(view['neighbours'])}")
    for n in view["neighbours"]:
        out.append(f"    {n['turn_id']}  {n['similarity']:.2f}  {n['model']}  {n['outcome']}")
    return out


def default_embed(text: str):
    """Embed ``text`` with the active matcher. Returns (float32-rounded vector, identity)."""
    from opendaisugi._search import _get_model, active_model_name

    identity = active_model_name()
    vec = _get_model().encode([text], convert_to_numpy=True)[0]
    return f32_vec(vec), identity

"""The answer store — freshness-gated RAG over past gateway answers (ADR-0012 §2D).

The recall tool (``gateway_recall``, ADR-0012 §2C) answers a repeated ask by
re-verifying a *plan* against the caller's envelope. Some repeated asks never had a
plan to begin with: "what does this function do?" or "explain OAuth" produce a plain
text answer, so the assurance verifier has nothing to check. For that tail, this
module substitutes **freshness** for verification: retrieve the nearest past answer
by embedding, and serve it only if it clears three gates — confidence, age, and
ground-shift — otherwise fall open to the model. This is deliberately the layer's
*only* un-verified reuse path (ADR-0012 §2D "Consequences"), and it is quarantined
accordingly: opt-in, provenance-stamped, structurally separate from
``pathway_store``, and it never overrides a 2C ``recall`` hit — a repeat that *does*
have a plan always goes the assured way.

Persistence mirrors ``gateway_journal.GatewayJournal``: append-only JSONL, best-effort
crash-tolerant reads that skip a corrupt line rather than raising. Unlike the turn
journal (unbounded, since it only ever grows a saving ledger), ``AnswerStore`` is a
**bounded ring**: it exists to serve answer content back to a caller, so an unbounded
store would mean unbounded retained response text. ``max_entries`` caps it; once full,
each further ``append`` evicts the oldest entries to make room, keeping only the most
recently captured answers.

Embedding follows ``gateway_cluster``'s discipline exactly: the shared repo embedder
behind ``opendaisugi._search._get_model`` is imported lazily inside the function that
needs it, so this module (and a bare ``Daisugi()``) stays importable without the
``[search]`` extra. ``embed`` is injectable, mirroring ``cluster_repeats``, so the gate
logic is fully testable with a deterministic fake and no real model.
"""

from __future__ import annotations

import json
import os
from collections.abc import Callable, Iterable
from dataclasses import asdict, dataclass, fields
from pathlib import Path
from typing import Any

from opendaisugi.gateway_journal import turn_signature
from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

EmbedFn = Callable[[list[str]], Any]

# 7 days: long enough that a normal work day's repeats stay servable, short enough
# that a stale answer does not linger indefinitely on nothing but a confidence match.
DEFAULT_ANSWER_MAX_AGE_SECONDS = 7 * 24 * 3600.0


@dataclass(frozen=True)
class AnswerEntry:
    """One captured answer: the ask that produced it, and what it relied on.

    ``ground_hash`` is a hash of the files/context the answer relied on, supplied by
    whoever captured it (the future proxy-capture step, or a caller of
    ``capture_answer`` directly). ``None`` means unknown/uncomputed — never treated as
    "no ground", just as "can't check", per the ground-shift gate below.
    """

    signature: str
    task: str
    answer: str
    created_at: float
    ground_hash: str | None = None


@dataclass(frozen=True)
class AnswerProvenance:
    """Travels with every hit so a caller never treats a freshness-served answer as
    fresh fact with no further checking — it is an estimate, not a proof."""

    similarity: float
    age_seconds: float
    created_at: float
    ground_hash: str | None


@dataclass(frozen=True)
class AnswerResult:
    """Either a freshness-gated answer (+ provenance) or a miss."""

    hit: bool
    reason: str | None
    answer: str | None
    provenance: AnswerProvenance | None


_FIELD_NAMES = {f.name for f in fields(AnswerEntry)}


class AnswerStore:
    """Append-only, crash-tolerant, BOUNDED-ring JSONL store of captured answers.

    One answer per line, mirroring ``GatewayJournal``. ``append`` is a true append
    (one ``"a"``-mode write of the new line) on the common path, so a crash mid-append
    loses at most that one line — exactly ``GatewayJournal``'s crash-tolerance
    property. Only when the store now exceeds ``max_entries`` does it rewrite: the
    oldest entries are dropped and the kept tail is written to a temp file, then
    atomically renamed over the store (``os.replace``), so a crash mid-eviction
    leaves either the old (over-cap) file or the new (trimmed) file intact — never a
    half-written one.

    Reads are best-effort: blank lines, a truncated line from a crash mid-append, or
    a line that isn't an answer entry are skipped (and counted in a warning) rather
    than raising, mirroring ``GatewayJournal.load()``.
    """

    def __init__(self, *, path: Path, max_entries: int = 1000) -> None:
        self.path = Path(path)
        self.max_entries = max_entries

    def append(self, entry: AnswerEntry) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.path.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(asdict(entry)) + "\n")

        # NB: this re-reads the whole file on every append to check the cap — O(n) per
        # append (n <= max_entries). Harmless while nothing populates the store, but it
        # becomes a per-turn cost the moment Phase 3 wires live capture; revisit then
        # (e.g. an in-memory line count) rather than inheriting it silently.
        entries = self.load()
        if len(entries) > self.max_entries:
            keep = entries[-self.max_entries :]
            tmp_path = self.path.with_suffix(self.path.suffix + ".tmp")
            tmp_path.write_text(
                "".join(json.dumps(asdict(e)) + "\n" for e in keep), encoding="utf-8"
            )
            os.replace(tmp_path, self.path)

    def load(self) -> list[AnswerEntry]:
        if not self.path.exists():
            return []
        out: list[AnswerEntry] = []
        skipped = 0
        for line in self.path.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                raw = json.loads(line)
                out.append(AnswerEntry(**{k: raw[k] for k in _FIELD_NAMES if k in raw}))
            except (json.JSONDecodeError, TypeError):
                # Truncated/partial line (crash mid-append) or valid JSON that isn't
                # an answer entry. Skip it; never let one bad line brick the store.
                skipped += 1
        if skipped:
            import logging

            logging.getLogger("opendaisugi.gateway_answers").warning(
                "answer store: skipped %d unreadable line(s) in %s", skipped, self.path
            )
        return out


def capture_answer(
    store: AnswerStore,
    *,
    task: str,
    answer: str,
    created_at: float,
    ground_hash: str | None = None,
) -> AnswerEntry:
    """Build an ``AnswerEntry`` for ``task``/``answer`` and append it to ``store``.

    The entry point a future proxy-capture step calls to persist a plan-less answer
    for later freshness-gated reuse (not wired into the live streaming proxy here —
    that is a Phase-3 follow-up; this is just the function + store).
    """
    entry = AnswerEntry(
        signature=turn_signature(task),
        task=task,
        answer=answer,
        created_at=created_at,
        ground_hash=ground_hash,
    )
    store.append(entry)
    return entry


def _lazy_embed(texts: list[str]) -> Any:
    """Embed ``texts`` with the repo's one shared sentence-transformer model.

    Mirrors ``gateway_cluster._lazy_embed`` exactly — ``opendaisugi._search`` itself
    imports fine without the ``[search]`` extra installed (its heavy import is
    deferred inside ``_get_model``), so the guard wraps the call that actually loads
    the model, not the module import.
    """
    from opendaisugi._search import _get_model

    try:
        model = _get_model()
    except ImportError:
        raise ImportError(
            "Freshness-gated answer recall requires the [search] extra: "
            "uv add 'opendaisugi[search]'  (or: pip install 'opendaisugi[search]')"
        ) from None
    return model.encode(texts, convert_to_numpy=True)


def recall_answer(
    task: str,
    entries: Iterable[AnswerEntry],
    *,
    now: float,
    embed: EmbedFn | None = None,
    threshold: float = DEFAULT_PATHWAY_THRESHOLD,
    max_age_seconds: float = DEFAULT_ANSWER_MAX_AGE_SECONDS,
    current_ground_hash: str | None = None,
) -> AnswerResult:
    """Find the nearest past answer to ``task`` and gate it on freshness.

    Only entries with a non-empty ``signature`` and ``answer`` are considered.
    Candidates are matched on the ASK, not the answer body: the query task and each
    candidate's ``task`` text are embedded (normalized like ``gateway_cluster``), and
    the nearest by cosine similarity is the sole candidate — ALL three gates below
    must pass on that one candidate, or the result is a miss:

    - **confidence**: best similarity ``>= threshold``, else "no sufficiently
      similar past answer".
    - **age**: ``now - entry.created_at <= max_age_seconds``, else "cached answer
      too old".
    - **ground-shift**: only evaluated when BOTH ``current_ground_hash`` and the
      entry's ``ground_hash`` are known; if they differ, "the answer's ground has
      changed". If either is ``None`` this gate cannot be evaluated and does NOT
      block — age and confidence alone decide.

    ``now`` is injected (epoch seconds) for deterministic tests; this function never
    calls ``time.time()`` itself. ``embed`` is injectable like ``cluster_repeats``;
    when omitted the shared lazy-loaded repo embedder is used (requires the
    ``[search]`` extra).

    Never raises on a miss — every gate failure returns ``AnswerResult(hit=False,
    reason=..., answer=None, provenance=None)`` so a caller falls open to the model.
    """
    candidates = [e for e in entries if e.signature and e.answer]
    if not candidates:
        return AnswerResult(hit=False, reason="no answers in store", answer=None, provenance=None)

    if embed is None:
        embed = _lazy_embed

    import numpy as np

    from opendaisugi._similarity import cosine_similarity
    from opendaisugi.distiller import _normalize_task_for_embedding

    texts = [_normalize_task_for_embedding(task)] + [
        _normalize_task_for_embedding(e.task) for e in candidates
    ]
    try:
        vecs = np.asarray(embed(texts), dtype=float)
        query_vec = vecs[0]
        candidate_vecs = vecs[1:]

        best_idx = -1
        best_sim = -1.0
        for i in range(len(candidates)):
            sim = cosine_similarity(query_vec, candidate_vecs[i])
            if sim > best_sim:
                best_sim = sim
                best_idx = i
    except ImportError:
        # A missing [search] extra is a config error, not a transient one: let it
        # reach the boundary (the MCP tool / a CLI) so it can surface an install hint
        # rather than a mute miss.
        raise
    except Exception:
        # Any other embedding failure (a crashed model, a malformed return) degrades
        # to a clean miss on this fail-open saving tier — never a raise into the
        # harness's turn, matching GatewayJournal.load() and _already_reusable.
        return AnswerResult(
            hit=False, reason="answer recall unavailable", answer=None, provenance=None
        )

    best_entry = candidates[best_idx]

    if best_sim < threshold:
        return AnswerResult(
            hit=False,
            reason="no sufficiently similar past answer",
            answer=None,
            provenance=None,
        )

    age_seconds = now - best_entry.created_at
    if age_seconds > max_age_seconds:
        return AnswerResult(hit=False, reason="cached answer too old", answer=None, provenance=None)

    if (
        current_ground_hash is not None
        and best_entry.ground_hash is not None
        and current_ground_hash != best_entry.ground_hash
    ):
        return AnswerResult(
            hit=False,
            reason="the answer's ground has changed",
            answer=None,
            provenance=None,
        )

    return AnswerResult(
        hit=True,
        reason=None,
        answer=best_entry.answer,
        provenance=AnswerProvenance(
            similarity=best_sim,
            age_seconds=age_seconds,
            created_at=best_entry.created_at,
            ground_hash=best_entry.ground_hash,
        ),
    )


__all__ = [
    "AnswerEntry",
    "AnswerProvenance",
    "AnswerResult",
    "AnswerStore",
    "DEFAULT_ANSWER_MAX_AGE_SECONDS",
    "capture_answer",
    "recall_answer",
]

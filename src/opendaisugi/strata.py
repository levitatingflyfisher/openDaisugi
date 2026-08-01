"""Stage 10 — the rationale-durability ledger (ADR-0011, the currency levers).

On an irreducible one-off — a heisenbug hunt, a novel design — there is no
internal repetition to compile (Stage 9) and no prior corpus to reuse (Stage 4).
This is plausibly the *modal* rare-hard task, and every other lever is inert on
it. What compaction drops there is the **deliberation**: discovered facts,
ruled-out hypotheses and why, mid-task constraints, the goal/subgoal stack.
openDaisugi already externalizes *enforcement* state — the envelope, the deed
ledger — but not *reasoning*, so a compacted agent re-explores dead branches and
re-discovers facts it already had.

This module is a **typed strata store plus a reconstruction API a harness calls**.
Like the deed ledger (ADR-0004), openDaisugi records; the harness acts —
openDaisugi does **not** rewrite its own prompts. Two hard boundaries:

- **Reconstruction is lossy.** A fact the relevance selector drops is a fact the
  agent will re-derive. The selector here is deliberately simple (pinned +
  open-constraints always, then tag/recency) — sophisticated relevance-selection is
  this stage's own landmine and is left to the harness; ``repage`` returns any
  dropped stratum verbatim so nothing is lost, only deprioritized.
- **Facts and hypotheses never gate actions.** They inform reasoning only. The one
  path that touches authority is ``promote_constraint`` (below), and it flows
  through ``verify_inheritance`` — a promoted constraint may only *tighten* the
  envelope, never loosen it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Literal
from uuid import uuid4

from pydantic import BaseModel, Field, computed_field

from opendaisugi.inheritance import verify_inheritance
from opendaisugi.models import ActionPlan, Envelope, Invariant, Violation

StratumKind = Literal["fact", "hypothesis", "constraint", "goal"]
StratumStatus = Literal["open", "ruled_out", "resolved", "promoted"]


# --------------------------------------------------------------------------- #
# the typed record
# --------------------------------------------------------------------------- #
class Stratum(BaseModel):
    """One typed unit of deliberation, with provenance.

    ``kind`` separates what *informs reasoning* (fact / hypothesis / goal) from what
    can *touch authority* (constraint, and only via ``promote_constraint``).
    ``status`` carries a hypothesis's ``ruled_out`` (kept so the branch is not
    re-explored), a goal's ``resolved``, and a constraint's ``promoted``. ``seq`` is
    a monotonic, store-assigned order — deterministic, no wall-clock.
    """

    id: str = Field(default_factory=lambda: f"stratum_{uuid4().hex[:8]}")
    kind: StratumKind
    content: str
    provenance: str = ""
    status: StratumStatus = "open"
    tags: list[str] = Field(default_factory=list)
    pinned: bool = False
    seq: int = 0


class ReconstructedContext(BaseModel):
    """Context rebuilt from the store instead of the transcript.

    ``pinned`` (pinned strata + open constraints) is always present; ``strata`` is
    that plus the highest-relevance fill under the budget; ``dropped_ids`` are the
    deprioritized strata, each re-pageable verbatim via ``StrataStore.repage``.
    """

    strata: list[Stratum]
    pinned: list[Stratum]
    dropped_ids: list[str]
    note: str


class RederivationLedger(BaseModel):
    """The re-derivation meter — output tokens spent re-deriving with the store off
    vs on. Labelled evidence, not proof: the token magnitudes are model-dependent
    and the real numbers wait on a model, as in Stage 4."""

    output_tokens_without_store: int = 0
    output_tokens_with_store: int = 0
    rederived_facts: int = 0
    reexplored_branches: int = 0
    evidence_not_proof: bool = True
    note: str = (
        "Store-on vs store-off output-token delta. Labelled evidence, not proof; the "
        "magnitudes are model-dependent and the at-scale numbers are deferred to a "
        "local model (Stage 4's dependency)."
    )

    @computed_field  # type: ignore[prop-decorator]
    @property
    def tokens_saved(self) -> int:
        return self.output_tokens_without_store - self.output_tokens_with_store


# --------------------------------------------------------------------------- #
# the store
# --------------------------------------------------------------------------- #
class _StoreState(BaseModel):
    seq: int
    strata: list[Stratum]


class StrataStore:
    """An in-memory typed strata store. Kept external to the transcript (that is the
    property that matters, not on-disk); ``to_json`` / ``from_json`` give durability
    across a compaction without pulling in a database."""

    def __init__(self) -> None:
        self._strata: list[Stratum] = []
        self._seq = 0

    def emit(
        self,
        kind: StratumKind,
        content: str,
        *,
        provenance: str = "",
        status: StratumStatus = "open",
        tags: list[str] | None = None,
        pinned: bool = False,
    ) -> Stratum:
        """The cheap structured-emission hook: record one typed stratum."""
        self._seq += 1
        stratum = Stratum(
            kind=kind,
            content=content,
            provenance=provenance,
            status=status,
            tags=list(tags or []),
            pinned=pinned,
            seq=self._seq,
        )
        self._strata.append(stratum)
        return stratum

    def set_status(self, stratum_id: str, status: StratumStatus) -> Stratum:
        s = self.get(stratum_id)
        if s is None:
            raise KeyError(stratum_id)
        s.status = status
        return s

    def all(self) -> list[Stratum]:
        return list(self._strata)

    def get(self, stratum_id: str) -> Stratum | None:
        for s in self._strata:
            if s.id == stratum_id:
                return s
        return None

    def by_kind(self, kind: StratumKind) -> list[Stratum]:
        return [s for s in self._strata if s.kind == kind]

    def repage(self, stratum_id: str) -> Stratum:
        """Return a stratum verbatim — the escape hatch for detail the relevance
        selector deprioritized. Raises KeyError if it never existed."""
        s = self.get(stratum_id)
        if s is None:
            raise KeyError(stratum_id)
        return s

    def _always_include(self, s: Stratum) -> bool:
        # Pinned strata and live constraints are never dropped — they carry the
        # invariants and active goals a compacted agent must not lose.
        return s.pinned or (s.kind == "constraint" and s.status in ("open", "promoted"))

    def _relevance(self, s: Stratum, tags: set[str], query: str | None) -> tuple:
        # Higher is better. A ruled-out hypothesis is kept but deprioritized: it
        # earns its place by stopping a re-exploration, not by crowding out facts.
        tag_hits = len(tags & set(s.tags)) if tags else 0
        query_hit = 1 if query and query.lower() in s.content.lower() else 0
        ruled_out_penalty = -1 if s.status == "ruled_out" else 0
        return (tag_hits, query_hit, ruled_out_penalty, s.seq)

    def reconstruct_context(
        self,
        *,
        budget: int | None = None,
        tags: list[str] | None = None,
        query: str | None = None,
    ) -> ReconstructedContext:
        """Rebuild context from the store: pinned + open constraints always, then the
        highest-relevance fill under ``budget``. Relevance = tag overlap, then
        query-substring, then recency; ruled-out hypotheses are kept but deprioritized.

        ``budget`` is a **floor, not a ceiling**: pinned strata and open constraints are
        never dropped, so when they alone exceed ``budget`` the result exceeds it (and the
        note says so). ``budget=None`` includes everything.
        """
        tagset = set(tags or [])
        pinned = [s for s in self._strata if self._always_include(s)]
        candidates = [s for s in self._strata if not self._always_include(s)]
        candidates.sort(key=lambda s: self._relevance(s, tagset, query), reverse=True)

        if budget is None:
            selected, dropped = candidates, []
        else:
            room = max(0, budget - len(pinned))
            selected, dropped = candidates[:room], candidates[room:]

        chosen = pinned + selected
        chosen.sort(key=lambda s: s.seq)  # present in the order they were discovered
        note = (
            f"Reconstruction is lossy: {len(dropped)} stratum(s) dropped — each a fact "
            f"the agent will re-derive unless re-paged. {len(pinned)} pinned/constraint "
            f"stratum(s) always retained."
        )
        if budget is not None and len(chosen) > budget:
            note += (
                f" Pinned/constraint strata ({len(pinned)}) exceed the budget ({budget}); "
                f"budget is a floor, not a ceiling — they are never dropped."
            )
        return ReconstructedContext(
            strata=chosen,
            pinned=pinned,
            dropped_ids=[s.id for s in dropped],
            note=note,
        )

    def to_json(self) -> str:
        return _StoreState(seq=self._seq, strata=self._strata).model_dump_json()

    @classmethod
    def from_json(cls, data: str) -> "StrataStore":
        state = _StoreState.model_validate_json(data)
        store = cls()
        store._seq = state.seq
        store._strata = list(state.strata)
        return store


# --------------------------------------------------------------------------- #
# constraint-promotion — the ONLY path from the store to authority
# --------------------------------------------------------------------------- #
@dataclass
class PromotionResult:
    ok: bool
    envelope: Envelope
    violations: list[Violation] = field(default_factory=list)
    reason: str = ""
    # True only when a ``deny_witness`` was supplied AND the tightened envelope
    # rejected it — i.e. enforcement was *proven*, not merely gated as a tightening.
    # A glob-removal promotion enforces structurally but leaves this False unless a
    # witness was given; a soft-compiled invariant can never set it True.
    enforcement_proven: bool = False


def _tightened(
    envelope: Envelope,
    add_invariant: Invariant | None,
    remove_file_write: list[str] | None,
) -> Envelope:
    candidate = envelope.model_copy(deep=True)
    if add_invariant is not None:
        # Append-only: verify_inheritance compares invariants by value, so rewriting
        # an existing one would read as a removal (loosening) and be rejected.
        candidate = candidate.model_copy(
            update={"invariants": [*candidate.invariants, add_invariant]}
        )
    if remove_file_write:
        remove = set(remove_file_write)
        kept = [g for g in candidate.permissions.file_write if g not in remove]
        candidate = candidate.model_copy(
            update={"permissions": candidate.permissions.model_copy(update={"file_write": kept})}
        )
    return candidate


def promote_constraint(
    envelope: Envelope,
    constraint: Stratum,
    *,
    add_invariant: Invariant | None = None,
    remove_file_write: list[str] | None = None,
    candidate: Envelope | None = None,
    deny_witness: ActionPlan | None = None,
) -> PromotionResult:
    """Promote a mid-task constraint into the enforcement envelope — the one path
    from deliberation to authority.

    Four fail-closed gates, any of which refuses:
    - **Kind.** Only a ``constraint`` stratum may touch authority; a ``fact`` /
      ``hypothesis`` / ``goal`` is refused — those inform reasoning, never gate actions.
    - **Only tightens.** The candidate — built from the delta (globs removed,
      invariants appended), or supplied explicitly via ``candidate`` — must pass
      ``verify_inheritance`` against the current envelope; a delta that would loosen is
      rejected.
    - **Actually tightens.** The candidate must be *strictly* tighter (the reverse
      inheritance check must fail) — a no-op promotion that constrains nothing is
      refused, not silently accepted.
    - **Actually enforces (opt-in).** If a ``deny_witness`` plan is supplied, the
      tightened envelope must *reject* it; a promoted constraint that verifies its own
      witness as OK is unenforced theater (e.g. an invariant that compiled to a soft
      node) and is refused. This is the fail-open this stage exists to prevent.

    On success the constraint's status becomes ``promoted`` and the tightened envelope
    is returned; on any refusal the original envelope is returned unchanged.
    """
    if constraint.kind != "constraint":
        return PromotionResult(
            ok=False,
            envelope=envelope,
            reason=(
                f"only a 'constraint' stratum may touch authority; got kind "
                f"'{constraint.kind}' — facts, hypotheses and goals inform reasoning, "
                f"never gate actions"
            ),
        )
    if constraint.status == "promoted":
        return PromotionResult(
            ok=False,
            envelope=envelope,
            reason=(
                "constraint is already promoted; re-promoting against the original "
                "envelope would build a second tightening that drops the first"
            ),
        )
    if candidate is None:
        candidate = _tightened(envelope, add_invariant, remove_file_write)
    loosening = verify_inheritance(candidate, envelope)
    if loosening:
        return PromotionResult(
            ok=False,
            envelope=envelope,
            violations=loosening,
            reason="promotion would loosen the envelope; a captured constraint may only tighten",
        )
    if not verify_inheritance(envelope, candidate):
        return PromotionResult(
            ok=False,
            envelope=envelope,
            reason="promotion has no enforceable effect (candidate equals the current envelope)",
        )
    enforcement_proven = False
    if deny_witness is not None:
        from opendaisugi.verify import verify

        if verify(deny_witness, candidate).ok:
            return PromotionResult(
                ok=False,
                envelope=envelope,
                reason=(
                    "promoted constraint does not actually deny its witness — an "
                    "unenforced (soft/uncompiled) constraint is refused, not accepted"
                ),
            )
        enforcement_proven = True
    constraint.status = "promoted"
    return PromotionResult(
        ok=True, envelope=candidate, reason="tightened", enforcement_proven=enforcement_proven
    )


__all__ = [
    "PromotionResult",
    "ReconstructedContext",
    "RederivationLedger",
    "Stratum",
    "StrataStore",
    "promote_constraint",
]

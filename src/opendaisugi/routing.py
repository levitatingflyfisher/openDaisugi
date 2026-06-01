"""Intelligent, token-saving model routing — the cheapest viable tier per task.

openDaisugi already routes *envelope generation* through a Tier-0 (pathway) →
Tier-1 (cheap) → Tier-2 (frontier) ladder. ``RouteAdvisor`` exposes that
decision as advice an agent (or a human) can act on for the *work itself*: given
a task, recommend which model to spend on.

Relationship to Anthropic's advisor tool
-----------------------------------------
The advisor tool (beta header ``advisor-tool-2026-03-01``) pairs a fixed cheap
*executor* with a smart *advisor* model consulted mid-generation. It is a
quality-at-lower-cost lever, but it (a) is not a per-task router — you choose the
pair up front — and (b) re-derives its plan every request, paying advisor tokens
each time, with no verification of the result. openDaisugi is complementary:

- A *repeat* task matches a distilled pathway → Tier-0 reuse: ~free and
  re-verified against its stored envelope. This is the differentiator — the
  advisor tool has no cross-request memory; the pathway store is that memory,
  and it is verified.
- An *easy novel* task → a cheap model (Tier-1).
- A *hard novel* task → the frontier, and we flag that the advisor-tool pairing
  (cheap executor + Opus advisor) is the better spend than a bare frontier call.

The difficulty estimate here is a transparent heuristic; it can later be
calibrated from the journal (historical step count / retries for similar tasks).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING

from opendaisugi.pathway_store import DEFAULT_PATHWAY_THRESHOLD

if TYPE_CHECKING:
    from opendaisugi.pathway_store import PathwayStore

_log = logging.getLogger("opendaisugi.routing")

_DEFAULT_CHEAP_MODEL = "claude-haiku-4-5"
_DEFAULT_FRONTIER_MODEL = "claude-opus-4-8"

# Lexical signals that a task involves non-obvious design / failure modes — the
# regime where a stronger model (or the advisor-tool pairing) earns its cost.
_HARD_SIGNALS = (
    "architect", "architecture", "design", "refactor", "migrat", "concurren",
    "deadlock", "race condition", "distributed", "consensus", "optimi",
    "security", "vulnerab", "schema", "algorithm", "prove", "proof", "debug",
    "root cause", "thread-saf", "scal",
)

# Difficulty at/above this is routed to the frontier (and advisor-pairing flagged).
_HARD_THRESHOLD = 0.5

# Harness identifiers on which Anthropic's advisor tool exists. The advisor tool
# is an Anthropic-API + Claude-models feature; it does not exist for Codex
# (OpenAI), Ollama/llamafile/local models, Hermes, or OpenClaw. openDaisugi is a
# cross-harness tool, so we only surface the advisor-tool pairing where it is
# actually available — dangling an Anthropic-only suggestion at a Codex/local
# user would be wrong.
_ADVISOR_TOOL_HARNESSES = frozenset({"claude-code", "claude", "anthropic"})


def advisor_tool_available_for_harness(harness: str) -> bool:
    """Whether Anthropic's advisor tool is available on the given harness.

    Returns True only for Claude/Anthropic harnesses (``claude-code``, and the
    generic ``claude``/``anthropic``). Everything else — ``codex``, ``ollama``,
    ``local``, ``llamafile``, ``hermes``, ``openclaw``, or any unknown id —
    returns False, the safe default that withholds advice we can't deliver.
    """
    return harness.strip().lower() in _ADVISOR_TOOL_HARNESSES


@dataclass
class RouteAdvice:
    """A routing recommendation for one task."""

    tier: str            # "tier0-pathway" | "tier1-cheap" | "tier2-frontier"
    model: str           # recommended model id (or "" for Tier-0 reuse)
    reason: str
    difficulty: float
    pathway_id: str | None = None
    advisor_pairing: bool = False


def estimate_difficulty(task: str) -> float:
    """Transparent 0–1 difficulty heuristic.

    Combines task length (a rough proxy for scope) with lexical signals of
    design/failure-mode complexity. Not a model — deliberately legible and cheap;
    a journal-calibrated estimator can replace it without changing the interface.
    """
    t = task.lower()
    length_component = min(len(t) / 400.0, 0.5)
    hits = sum(1 for s in _HARD_SIGNALS if s in t)
    signal_component = min(hits * 0.25, 0.6)
    return min(length_component + signal_component, 1.0)


class RouteAdvisor:
    """Recommend the cheapest viable tier/model for a task."""

    def __init__(
        self,
        *,
        pathway_store: "PathwayStore | None",
        cheap_model: str = _DEFAULT_CHEAP_MODEL,
        frontier_model: str = _DEFAULT_FRONTIER_MODEL,
        threshold: float = DEFAULT_PATHWAY_THRESHOLD,
        advisor_tool_available: bool = True,
    ) -> None:
        self.pathway_store = pathway_store
        self.cheap_model = cheap_model
        self.frontier_model = frontier_model
        self.threshold = threshold
        # Whether the host harness has Anthropic's advisor tool. Default True
        # preserves the common Claude case; set False for Codex/local/Hermes/
        # OpenClaw so a hard task recommends a bare frontier model instead of an
        # advisor-tool pairing the harness can't run.
        self.advisor_tool_available = advisor_tool_available

    def advise(self, task: str) -> RouteAdvice:
        difficulty = estimate_difficulty(task)

        # Tier-0: a distilled, verified pathway already covers this task — reuse it.
        if self.pathway_store is not None:
            try:
                match = self.pathway_store.find(task, threshold=self.threshold)
            except Exception as exc:  # store/embedder issues must not break routing
                _log.warning("route: pathway lookup failed: %s", exc)
                match = None
            if match is not None:
                return RouteAdvice(
                    tier="tier0-pathway",
                    model="",
                    reason=(
                        f"reuse distilled pathway {match.pathway.id} "
                        f"(similarity {match.similarity:.2f}); re-verified against its "
                        f"envelope — near-zero LLM cost, provably in policy"
                    ),
                    difficulty=difficulty,
                    pathway_id=match.pathway.id,
                    advisor_pairing=False,
                )

        # Tier-1: easy, novel task — a cheap model is enough.
        if difficulty < _HARD_THRESHOLD:
            return RouteAdvice(
                tier="tier1-cheap",
                model=self.cheap_model,
                reason=f"novel but low-difficulty ({difficulty:.2f}); cheap model suffices",
                difficulty=difficulty,
                advisor_pairing=False,
            )

        # Tier-2: hard, novel task — route to the frontier. Where the host
        # harness has Anthropic's advisor tool, flag that pairing a cheap
        # executor with an Opus advisor is the better spend than a bare frontier
        # call. Where it doesn't (Codex/local/Hermes/OpenClaw), recommend the
        # frontier model plainly — never dangle an Anthropic-only suggestion.
        if not self.advisor_tool_available:
            return RouteAdvice(
                tier="tier2-frontier",
                model=self.frontier_model,
                reason=(
                    f"novel and high-difficulty ({difficulty:.2f}); "
                    f"use the frontier model"
                ),
                difficulty=difficulty,
                advisor_pairing=False,
            )

        return RouteAdvice(
            tier="tier2-frontier",
            model=self.frontier_model,
            reason=(
                f"novel and high-difficulty ({difficulty:.2f}); use the frontier — "
                f"or pair a cheap executor with an Opus advisor (Anthropic advisor "
                f"tool, beta advisor-tool-2026-03-01) for comparable quality at lower cost"
            ),
            difficulty=difficulty,
            advisor_pairing=True,
        )

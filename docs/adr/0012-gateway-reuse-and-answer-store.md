# ADR-0012 — Gateway reuse: bridge to the pathway engine, plus a freshness-gated answer store

**Status:** Accepted. Implemented — Phase 2 (cluster · distill-repeats · `recall` · answer
store), Phase 3 (live-proxy answer capture · `gateway-report` calibration). Extends ADR-0008.

## Context

The token-saving gateway (an Anthropic-Messages-compatible `base_url` proxy any harness
points at) ships Phase 1: it **routes** an easy turn to the cheap model and journals what
that saved. Routing alone is ~3× blended over a real cached day. The larger multiplier lives
in **reuse** — answering a recurring ask without the frontier model at all.

Four facts constrain how reuse can be added:

1. **The reuse engine already exists** (ADR-0008). A `CompiledPathway` is an
   envelope-guarded callable: **frozen** (0-token, served verbatim) or **typed** (bind data
   holes to the current ask, then re-verify). `PathwayStore.find` matches by embedding;
   every reused plan is re-verified against the *caller's* envelope before it is trusted.
2. **But it mines the wrong journal.** The distiller distills the *assurance* `Journal` —
   traces that carry a plan. A raw `base_url` turn has no plan. The gateway's own
   `turns.jsonl` records *which asks repeat* but stores **no plan and no response**; it is a
   detection signal, not a cache. So the gateway turn-stream and the pathway engine do not
   currently touch.
3. **Exact matching does not fire.** `turn_signature` is a sha256 of normalized text. On
   real agentic work two asks are almost never byte-identical, so a repeat is rarely detected
   by string identity.
4. **A transparent cache is forbidden.** ADR-0004: the layer gates actions, it does not
   drive the harness. A proxy that silently returned a cached answer would be the gateway
   driving the loop. Reuse must be a tool the harness *opts into*.

We want gateway reuse that reaches the bigger multiplier **without** weakening any invariant,
**without** riding the assurance Journal (which would corrupt its replayable stats), and
**without** a silent intercept.

## Decision

Bridge the gateway turn-stream to the ADR-0008 engine, and add one new store for the repeats
that have no plan to verify. Four parts, all **opt-in**.

**2A — cluster, don't string-match.** Group journalled asks by embedding proximity (reusing
the distiller's embedder and the `[search]` extra), so paraphrases form one reuse candidate,
with the varying span extracted as a typed parameter via the existing diff. Divergent
*action structure* stays separate by construction (the ADR-0008 same-shape and
capability-head rules). This is the first deliverable, not a detail — with exact matching the
substrate is empty.

**2B — surface and rank repeats (opt-in).** `daisugi distill-repeats` clusters the journalled
repeats (2A) and ranks them by the frontier spend they represent — tokens first, dollars
alongside — and flags the ones that are *already reusable* (a matching pathway exists, so
there is nothing to do). It never distills on its own; this is the skill-gardening worklist
that says which repeated ask is worth reusing first, not an auto-promoter. Finer home-routing
— sending an openDaisugi-orchestratable ask to the existing distiller (`tend`), marking a
plan-less ask for the 2D answer store — enriches this worklist as 2C and 2D land. (A raw
routed ask carries no executor context to plan against, so minting a `CompiledPathway` stays
with the distiller, which mines the assurance journal where a plan actually exists.)

**2C — the recall tool (opt-in MCP).** `daisugi_recall(task)` = `find` → (frozen verbatim |
typed bind) → `verify` against the caller's envelope → return `{plan, provenance}`, or a
miss. The harness chooses to call it before the model; on a miss or a verify failure it
proceeds to the model. Frozen is 0-token; typed is one cheap-tier binding call. Provenance
(distilled-at, trace count, last-verified-against) travels with every hit.

**2D — the answer store, for plan-less repeats.** A repeat that produces a plain text answer
has no plan, so **freshness replaces verification** as its trust story. Retrieve the nearest
past answer by embedding; serve it only if confidence, age, and *ground-shift* (a hash of the
files/context it cited) all clear a bar; otherwise fall open to the model. Freshening starts
passive (expire by age or ground-shift, re-ask on the next hit). This store is opt-in, is
never mixed into the assured pathway store, and never overrides 2C — a repeat that *does*
have a plan always goes the assured way.

**The meter reports two accounting modes.** An MCP tool is called *by the model*, so on the
ordinary path the frontier model has already run; recall saved the *downstream* tool-loop
turns, not the calling turn — booked as an **estimated lower bound**. Only **pre-dispatch**
recall (an orchestrator or slash command that calls recall before the frontier model wakes)
saves a whole turn, and that is the reproducible number the meter defaults to.

## Consequences

- **A response-capture step is new.** The gateway must persist responses for candidate
  signatures so 2D has something to serve; the store is bounded and opt-in. This is the one
  place the proxy retains answer content.
- **2D is the only un-verified reuse path**, and it is quarantined: opt-in, freshness-gated,
  fail-open, provenance-stamped, and structurally separate from the pathway store. It buys
  the plain-question-and-answer tail that the assured path cannot reach; it pays for that
  with a trust story that is *estimated* (freshness) rather than *proven* (verify).
- **The honest ceiling is not 100×.** Routing gives ~3× blended; reuse adds on top only where
  work repeats — plausibly low tens× on a repetitive day, ~0 on a day of novel work. The
  "1/100th" target is a north star that Phase 3 tests on a real day and then earns or retires.
- **Clustering can over-merge.** Two asks that embed close but need different work would be a
  false candidate; the ADR-0008 structure-signature and capability-head split catch this at
  promotion (2B), so a bad cluster fails to compile rather than serving a wrong plan.
- **Model-invoked savings are not precisely attributable** — the journal cannot see the turns
  that never happened. Naming the two modes, and defaulting to the reproducible one, keeps the
  headline number honest for a project whose scorecards are the point.

## Invariants preserved (VISION §invariants)

Fail-closed for *safety* is untouched — this is the fail-*open* saving path, kept separate
from the call-time gate (ADR-0007) by design. Verify-before-execute holds on 2C (every reused
plan is re-verified against the caller's envelope). Envelope-as-ceiling holds (recall verifies
against the caller's envelope; ADR-0008 binding rules unchanged). The layer never drives the
harness (ADR-0004): recall and the answer store are tools the harness opts into, never a
transparent substitution.

## Alternatives considered

- **A transparent proxy cache** (intercept, return the cached answer inline). Rejected: that
  is the gateway driving the loop — ADR-0004. Opt-in MCP keeps the harness in control.
- **Route gateway turns through the assurance `Journal`.** Rejected: a raw turn has no plan to
  replay, so it would fabricate trivially-true envelopes and pollute `JournalStats` and the
  distiller's structure clustering. The gateway keeps its own lightweight store.
- **Exact-signature reuse only.** Rejected: near-zero hit rate on real paraphrased work.
  Clustering is the point of 2A.
- **Replay a past *decided* plan** (a "verified-plan replay" tier). Rejected: `verify` proves
  an action is *allowed*, not that it is *right for the current state* — a plan that edited
  `X:40` last week still verifies today, when line 40 is different code. ADR-0008's typed tier
  already re-derives the bindings against the current ask, so the safe reuse is the shape of
  the work, not a frozen verdict; a blind decided-plan replay is not added.

## Build order

2A cluster (also stands up a fixture corpus, since no journal has accumulated yet) → 2B
`distill-repeats` promotion → 2C `daisugi_recall` MCP tool + two-mode meter → 2D answer store
(shares 2A's embedder and the capture step). Each ships TDD, red-first.

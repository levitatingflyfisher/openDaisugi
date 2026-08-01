# The gauntlet and the synthesis

*Five adversarial critic agents, one per [blind design](blind-designs.md), each told
to steelman then attack. Then one synthesizer read all five designs and all five
critiques and produced the split, the convergence table, and the shared repairs. This
is where the convergence stops being a flattering coincidence and starts being
load-bearing — because it survived a hostile read.*

## The scores were sober

The critics were tough by design; a rubber-stamp gauntlet proves nothing. Overall
scores landed in the 4–6 range:

| Design | Cost | Correctness | Rare/hard | Composability | Novelty | Overall |
|---|---|---|---|---|---|---|
| Countinghouse | 6 | 7 | 5 | 7 | 6 | **6** |
| PORTCULLIS | 5 | 6 | 5 | 5 | 2 | **4** |
| PACT | 5 | 5 | 4 | 5 | 5 | **5** |
| Interlock | 5 | 6 | 4 | 5 | 4 | **5** |
| CONTINUO | 6 | 6 | 4 | 6 | 5 | **5** |

The low novelty scores are themselves the finding: the critics kept saying *"this is
sound but not new"* — which is exactly what you expect when five people independently
rebuild the canonical answer. Convergence reads as low novelty from the inside.

## What the critics agreed to attack

Because the five inputs were near-identical instantiations of one architecture, the
critiques converged on the same flaws. Six recurring attacks, each of which became a
repair baked into the synthesis:

1. **Wrong baseline.** The flagship output-savings number is measured against manual
   per-item repetition, but a competently scaffolded agent already *scripts* a bulk
   job. So the compiler's real marginal win is "a **verified** script instead of an
   **unverified** one" — real, but safety-shaped and smaller than the headline.
2. **Rationale, not just state, is what compaction drops.** Enforcement survives
   compaction by construction, but *task correctness* does not: a mid-task verbal
   constraint ("don't touch tenant X") that never gets promoted into the envelope is
   exactly what a summary erases, and none of the five protected it. Rollback restores
   disk, not the conversation — so the recovery-arc reasoning tokens are only partly
   saved.
3. **Self-certification creeps back in through postconditions.** The solver proves
   *allowed*, not *right*; a batch program's behavioral correctness rests on an
   LLM-authored postcondition sample-validated on k items — a heuristic, not a proof,
   and it must be labeled as one.
4. **Escalation must key on external signals, not the model's confidence.** A cheap
   model's self-report is the one thing that cannot be the gate.
5. **SMT was over-reached.** The solver earns its keep on the subset-proof (delta ⊆
   floor) and on write-set containment; it should not be doing path/glob checks that
   ordinary matching decides.
6. **Per-instance counterfactuals are evidence, not controlled proofs.** Sampling is
   stochastic and the mechanism-off replay prices an arc that might have unfolded
   differently — report bands, label as evidence.

## The convergence table

The synthesizer's structured convergence note. **"Reached by all five"** is the
canonical spine; the two lines flagged *not in the brief* are the clean signal — the
frame never hinted at them, so five-for-five is independent reinvention.

### Reached by all five

- **Enforcement outside the context window, at the tool-call boundary** — so
  compaction cannot drop the policy. The single strongest, most defensible shared
  claim.
- **Pre-execution, fail-closed gating** — verify before the side effect; refuse or
  escalate on unclassifiable shapes.
- **Monotone-narrowing authority lattice** *(not in the brief — clean)* — a
  human-authored floor plus a model-proposed delta that may only **tighten**, subset
  relation checked mechanically, no self-certification. This is openDaisugi's
  `envelope_subsumes`.
- **Copy-on-write / transactional execution with cheap rollback** — a wrong-but-allowed
  action costs a snapshot restore, not a recovery arc.
- **Within-instance program compilation** *(not in the brief — clean)* — harvest a
  single task's internal repetition into one verified program, O(N) → O(1). The cost
  lever, and the rare-hard-task escape.
- **An external deed/progress ledger** as ground-truth memory — licenses aggressive
  compaction and enables per-instance counterfactual metering.
- **Self-gating tiered triage with a no-op floor** — microsecond checks for reads,
  heavy machinery only on risky/batch actions, so trivial tasks pay ~zero.

### One-off bets (a single design each — the differentiated frontier)

- **Verification-underwritten cheap-model routing** (PORTCULLIS/Interlock named it,
  never as a core → promoted to **Underwriter**).
- **Attenuated sub-envelopes for delegation** (Interlock alone — the sub-agent
  authority-laundering hole the other four leave open).
- **Abstract-interpretation loop certification** — one soundness proof per loop body
  (CONTINUO alone; also its own sharpest risk).
- **Typed partial program with declared holes** reconstructed from a store, and
  **disposable-transcript resumption** after total session loss (CONTINUO alone).
- **Staged outbox** for irreversible effects, and **structural stop-loss** (k failures
  forces escalation, never the model's self-report) (Countinghouse alone).
- **A rationale/deliberation-durable memory plane** — *none of the five built this.*
  Every one stored only post-state digests and was critiqued for it. It became the
  deliberate new bet: **Palimpsest**.

## The synthesis — split the monolith along its currencies

The five inputs were one architecture over-claiming across four different currencies.
The synthesizer split them into four separately-honest bets, each with a distinct
primary thesis. **Bailiff is the substrate; the other three layer on it.**

### Bailiff — the transcript-independent enforcement sidecar *(safety-first)*
> The durable product is a correctness guarantee that survives compaction; cost
> savings are a bounded by-product of making aggressive compaction and cheap rollback
> safe. Sell the moat, not a multiplier.

Out-of-process sidecar on the tool-call boundary. A human-authored FLOOR evaluated by
deterministic matching, fail-closed; a solver used in **exactly one place** — proving a
model-proposed delta ⊆ the floor. Mutating actions run copy-on-write. A small external
ledger holds ground-truth state plus pinned active human-stated invariants. Sub-agents
get an attenuated sub-envelope proven ⊆ the parent's. **Saves safety-currency
primarily**; cost is claimed small and honest (~20–40% prefill on long tasks via safe
compaction; ~10–20% output via cheap rollback of the *file-repair* part of a recovery
arc — not the diagnosis). **Main risk:** under-ambition on cost.

### Foreman — agent-declared, blast-radius-proven batch execution *(output-collapse)*
> The token win on internally-repetitive work is not "write a script" — a competent
> agent already does that — it is turning an unverified script's unbounded blast radius
> into a proven one, monetized inside a single instance.

No loop-detector (the critics flagged anti-unification as a research problem dressed as
a milestone). Instead the **agent declares** a batch: program P, item set I, effect
footprint F, postcondition Q. The sidecar statically proves F ⊆ envelope *before any
iteration*, sample-validates Q on k=2–3 items in a CoW fork, then runs all N under a
kill-on-exit monitor with per-element rollback. Irreversible tools are non-batchable.
**Saves output-currency, against the honest baseline of "agent writes an unverified
script."** **Main risk:** that honest baseline shrinks the headline to a safety-shaped
benefit.

### Underwriter — blast-radius-bounded cheap-model routing *($/token)*
> A cheap model is unsafe to *trust*, not unsafe to *contain* — bound its blast radius
> with an external gate and you can route far more work to it, using gate-rejection and
> escrow-test failure as authority-independent escalation signals instead of the
> model's own confidence.

A cheap model drives envelope-covered steps by default; every action still transits the
gate and CoW escrow. Escalation to a strong model fires **only on external signals** —
gate rejection, escrow/postcondition failure, or a k-failure stop-loss — never the cheap
model's self-report. **Saves $/token** (~2–5× on covered steps). **Main risk:** on a
task hard at *every* step, wasted cheap attempts invert ROI — so an action-structure
triage cap (skip cheap attempts on high-risk steps) is mandatory, not optional.

### Palimpsest — a rationale-durable working-memory plane *(the novel bet)*
> On irreducible hard tasks there is no repetition to compile, so the real cost lever
> is never re-deriving reasoning that compaction dropped — externalize the
> **deliberation**, not just the enforcement state.

A structured external store holding four typed strata: discovered facts with
provenance, ruled-out hypotheses and why, open constraints (including mid-task
user-stated invariants captured at utterance and, where expressible, **promoted into
the enforcement envelope** — closing the "don't touch tenant X" gap all five left
open), and the goal/subgoal stack. Compaction then compacts the transcript hard, but
each model call's context is **reconstructed from the store**, so "compact aggressively"
becomes actually *costless* on hard tasks, not merely safe. Facts inform reasoning but
**never gate actions** — only constraint-promotion touches authority, through the same
monotone-narrowing check. **Saves input AND output** on long hard one-offs by
eliminating the re-derivation tax. **This is the bet that targets the modal rare-hard
task the others admit they are inert on.** **Main risk:** relevance-selection for
reconstruction is the hard, unglamorous core and can itself drop the load-bearing
stratum.

## The shared repairs, in one place

Baked into all four bets by the synthesizer:

- **Honest baselines** — batch value is "verified vs unverified script," not "N manual
  turns." (See the roadmap for the layered, two-ledger refinement of this.)
- **Rationale-not-just-state durability** — rollback restores disk, not the
  conversation, so recovery-arc tokens are only partly saved; the reasoning needs its
  own durable plane (Palimpsest).
- **External-signal escalation** instead of confidence self-report.
- **Capability attenuation for sub-agents.**
- **SMT confined** to the subset-proof and write-set containment, where it earns its
  keep.
- **Per-instance counterfactuals labeled as evidence, not controlled proofs**, given
  sampling stochasticity.

The four are a composable stack, but each is a standalone product bet with a different
primary currency — deliberately *not* variations of one design. What openDaisugi does
with all of this is [ADR-0011](../../adr/0011-verifiable-execution-substrate.md).

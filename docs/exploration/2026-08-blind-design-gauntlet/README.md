# The blind-design gauntlet — a convergence test for openDaisugi

*Exploration record, August 2026. This is not a spec and not a roadmap; it is the
raw material one strategic question was decided on. The decision it fed lives in
[ADR-0011](../../adr/0011-verifiable-execution-substrate.md); the work it scheduled
lives in [the roadmap](../../roadmap.md) (Stages 8–10). Read those for what we are
doing. Read this for how we convinced ourselves.*

## The question

openDaisugi is built on one bet: an LLM writes a checkable safety **envelope**, a
solver proves each proposed action stays inside it before the action runs. The bet
is sound for safety. It says less about *cost*, and that raised a real worry:

> Is the envelope-and-solver a **niche** — a robotics/safety curiosity — while the
> actual token-saving action is somewhere else entirely (caching, routing, plan
> reuse)? Or is it the load-bearing piece of a cost story too, and we just haven't
> built that part yet?

You cannot answer that by asking the people who built openDaisugi. They will find
their own architecture in the answer. So we asked people who had never heard of it.

## The method — and why it is shaped this way

A **blind-design gauntlet**: independent architects, told nothing about openDaisugi,
attack the same problem from scratch. If they route *around* the envelope and hit the
cost and safety goals without it, the envelope is a niche. If they keep rebuilding it,
it is convergent — what competent people arrive at when they attack this problem
honestly. Convergence you did not plant is the strongest evidence a design is right,
and the cheapest possible refutation if it is wrong.

Four stages, run as an automated multi-agent workflow:

1. **Frame.** One agent turned the worry into a neutral, vendor-free problem
   statement — dual-currency cost accounting, per-instance measurability, cold-start
   yield, authority-independent verification, compaction-durability, fail-closed
   gating — with **no mention of openDaisugi, envelopes, or solvers**. The frame is
   [`problem-statement.md`](problem-statement.md). Getting this clause right is the
   whole experiment: a leaky frame plants the answer.
2. **Design (blind).** **Five independent designer agents**, each **closed-book** —
   no shared memory, no sight of openDaisugi, no sight of each other — each given a
   different lens (cost-first, correctness-first, rare-task-first, composability-first,
   and a deliberate blank-slate contrarian told to look *unlike* the field). Five
   architectures came back: [`blind-designs.md`](blind-designs.md).
3. **Gauntlet.** **Five adversarial critic agents**, one per design, each told to
   steelman then attack — score buildability, refute the rare-task and compaction
   claims, hunt the net-token trap. Scores landed at a sober 4–6/10. The critics'
   job was to make the convergence *survive scrutiny*, not to flatter it.
4. **Synthesize.** One agent read all five designs and all five critiques and
   produced the split, the convergence table, and the shared repairs:
   [`gauntlet-and-synthesis.md`](gauntlet-and-synthesis.md).

The full structured output of every stage, verbatim, is
[`gauntlet-raw.json`](gauntlet-raw.json) — kept for audit and reproduction.

## What it found — the niche verdict

**Not a niche.** All five blind architects rebuilt openDaisugi's spine. Named
Countinghouse, PORTCULLIS, PACT ("Proof-Carrying Acts"), Interlock, and CONTINUO,
they converged on seven moves — and two of the seven were things the frame **never
hinted at**:

- **Monotone-narrowing authority** — a human-authored floor plus a model-proposed
  delta that may only *tighten*, subset-checked mechanically, no self-certification.
  This is openDaisugi's `envelope_subsumes`, rebuilt 5/5 with nobody told it existed.
- **Within-instance compilation** — harvest a *single* task's internal repetition
  into one verified program, collapsing O(N) turns to O(1). This is the cost lever,
  rebuilt 5/5 with nobody told it existed.

Those two clean 5/5s are the verdict. Independent minds, including the contrarian
sent looking for something different, arrive at the envelope. It is canonical.

The **rare-hard-task** worry resolved too: every design found the same escape.
Verification amortizes **per-action, not per-repetition** — so it pays on a task done
once, by trusting a cheap executor or a compiled batch whose output the envelope
proves in-bounds. No corpus, no recurrence required.

## What we changed because of it

The critics were harsh on purpose, and their harshness is the gift. Three repairs and
one reframe came out of it, all recorded in
[ADR-0011](../../adr/0011-verifiable-execution-substrate.md):

- **The reframe.** openDaisugi is not "a verifier." It is a **verifiable-execution
  substrate** with a small family of **cost levers** it underwrites. The synthesizer
  reached this independently — it split the one over-claiming stack into four
  separately-honest bets (a safety substrate plus three currency levers).
- **Honest, two-ledger baselines.** The within-instance win is small and
  safety-shaped; persistence and generalization are a *separate*, compounding win.
  Kept apart they both hold; merged, they become the over-claim the critics flagged.
- **Rationale-durable memory** — the one bet none of the five made, and the one that
  targets the irreducible one-off every design admitted it was inert on.
- **A reversibility path** — the layer records reversible deeds so the harness can
  roll back, rather than owning workspace snapshots itself (which
  [ADR-0004](../../adr/0004-layer-not-harness.md) forecloses).

## Reproducing it

The workflow is a self-contained script (frame → five closed-book designers → five
critics → one synthesizer, with a JSON schema per stage). The load-bearing
properties are: the frame carries no fingerprint of the thing being tested; the
designers are independent and cannot see each other; and the critics are told to
refute, not endorse. Any capable set of agents, blinded the same way, should be able
to run it and land near the same convergence — that is the point of blinding it.

## A note on provenance

Every artifact here was produced by AI agents and is recorded, per the workshop's
convention, as *an accurate account of what the exercise produced, offered with a
grain of salt* — not a proof, and not a spec. The design agents' outputs are their
own reasoning, faithfully transcribed and described in vendor-neutral terms; the
value is in the **method and the convergence**, not in any one model that ran it.

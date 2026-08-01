# North Star

> **This document is aspirational.** It describes where openDaisugi is *pointed*, not
> where it *is*. For the honest present — what is built, tested, and load-bearing today —
> read [VISION's scorecard](../VISION.md#honest-scorecard--built-vs-aspirational) and the
> near ground in the [roadmap](roadmap.md). Everything below is a direction, held with a
> grain of salt.

## One architecture, three scales

Three futures pull on this project. They look like three different products. They are the
same architecture at three scales — and it is the one the project is named after.

*Daisugi* (台杉) is the Japanese forestry art of growing many identical, straight,
purpose-perfect shoots from a single carefully-tended base stump, harvesting and regrowing
them for centuries without ever killing the root. Read the whole project through it:

- **The base stump** — the tended, persistent core that survives every harvest: the
  verified envelope, the pathway store, the gate.
- **The shoots** — distilled skills, each trimmed and sculpted to the exact shape of one
  purpose: cheap, repeatable, provably in-bounds, grown from the base.
- **The gardener** — distillation itself: the process that turns a journaled run into a
  new shoot and prunes the ones that stop paying.

The base loop underneath all three futures is openDaisugi's core loop:
**verify** (make it safe) → **route/size** (make it fast and cheap) → **journal** (make it
accountable) → **distill** (make it a repeatable skill). The dreams differ only in scale,
and in which one hard problem each one stresses hardest.

## The three dreams

### 1. The safe, fast, accountable pilot

A drone (or any real-time agent) with LLM-level judgment for novel situations, reflexes
fast enough to dodge and act at the speed of thought, and a record clear enough to explain
every action after the fact — including a proof that it took the *safest action available
at the time*.

Its deepest structure is already this project's ancestry: **Simplex runtime assurance**
(Sha, 1996) — a fast, high-performance controller that may do anything, supervised by a
slow, *proven* safety controller that can always take over. The pilot is Simplex with an
LLM as the performance controller and a **distilled, verified reflex** as the muscle
memory.

- **Stresses:** the skill spectrum (a reflex must run with no LLM call) and accountability.
- **Seeded by:** Tier-0 pathway reuse (a reflex is a pre-verified skill), the journal and
  deed ledger, and the recorded proof-backed denial. *"It took the safest action"* is the
  verifier proving in-envelope plus the rationale ledger holding *why*.
- **New hard problem:** perception-conditioned envelopes (the envelope is a function of
  live sensor state, not a static declaration) and verification *ahead of the tick* — a
  per-frame solver call is too slow, so the proof must be pre-compiled, which is the
  batch-compilation "prove once, run N" move applied to a control loop.

### 2. The 500-model household (daisugi incarnate)

A home where hundreds of narrow, cheap, purpose-trimmed models each tend one thing —
watering the mint, arming the alarm, balancing the calendar, working the front door and
the refrigerator — each one a shoot in perfect form for its role, all serving the household.

This is the metaphor made literal. The picture of *"perfectly honed, trimmed, sculpted
trees in the exact shape of an element, ornamental bushes in perfect form for their
purpose"* is a picture of daisugi.

- **Stresses:** routing at fleet scale and per-role authority.
- **Seeded by:** the model ladder, swarm deconfliction, and — critically —
  `intersect_permissions` and the *"don't touch tenant X"* constraint promotion. The home
  is a multi-tenant envelope problem: the mint-watering shoot can never reach the alarm;
  the door model can never touch the fridge. Each role gets a wall, and the household
  promotes the constraints.
- **New hard problem:** fleet orchestration and inter-agent contracts — the
  calendar-balancer negotiating with the fridge, each agent's output verified at the
  boundary before it becomes another's input.

### 3. Self-building, self-healing software

Massively complex software built from scratch by small teams: expert agents on each piece
of the codebase, summarizing up and taking direction down a chain to the humans, who act as
consumer, orchestrator, and invested chairman at once. When the work ships, it travels with
a small, honed expert model and its subordinates that can patch and morph the software in
real time — self-healing, adapting to the task in front of it.

- **Stresses:** decomposition and delegation, and shared rationale.
- **Seeded by:** `AgenticStep` / `AgenticExecutor` (sub-agents acting inside the *parent's*
  envelope — the authorization-ceiling invariant is the chain of command), the rationale
  ledger (the summary-to-the-boss is a reconstruction from strata), and the SKILL.nb
  direction (a verified deterministic cell that falls back to natural language when the
  environment drifts). The self-healing patcher is `adapt_plan`.
- **New hard problem:** self-modification under an envelope — can an agent rewrite code and
  *re-prove* it stays in policy? — bounded by [ADR-0004](adr/0004-layer-not-harness.md):
  openDaisugi verifies and records; it does not own the workspace, so self-morphing runs
  through the harness, not through us.

## The base layer every dream shares

At the base, all three need the same three capabilities. Each already maps to a subsystem:

| The capability | The subsystem | Where it stands |
|---|---|---|
| Strong token-saving / model routing | the model ladder (Tier-0 reuse → cheap → frontier), the per-step sizer, budget-gated downgrade, failure-signal escalation | routing works; escalation-on-failure is the open, buildable lever |
| Task recognition / decomposition / execution | the typed-step `ActionPlan`, the orchestrator, verify-per-step | working, tested |
| Distillation into repeatable, safe, fast skills (cron-bash ↔ bespoke-LLM) | distillation → signed pathway → Tier-0 reuse; the SKILL.nb deterministic-cell direction | the mechanism is real; its *value* (roadmap Stage 4) is the unmeasured linchpin |

That last row is the through-line. A reflex, a mint-waterer, and a self-healing patch are
the **same object** — a distilled skill — at three points on one spectrum, from a `cron`
line running `bash` to a bespoke LLM analysis. Nail distillation and the base of all three
dreams rises at once.

## Near ground and far horizon

The [roadmap](roadmap.md) is the near ground: the enforcement spine (Stages 1–3) and the
cost levers (Stages 8–10) are the base stump, and nothing here asks to abandon them. The
linchpin is **Stage 4** — proving distilled skills actually pay and stay safe — because
every dream rests on it, and it is gated on a reliable local model. Breaking that one
bottleneck unblocks the most.

The dreams add three far-horizon problems to [VISION's horizons](../VISION.md#horizons-problems-not-a-feature-list):
perception-conditioned envelopes and pre-tick verification (the pilot); fleet orchestration
and inter-agent boundary contracts (the household); self-modification under re-verification
(the software). They stay horizons until the near ground can hold them.

The through-line is the same as [the one idea](../VISION.md#the-one-idea), turned on the
project itself: don't ask anyone to trust the dream — build toward it in pieces each of
which is something you can check.

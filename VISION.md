# Vision

> The north star for openDaisugi. If you (person or agent) are about to change
> something load-bearing, read this first — it says what must stay true and why.
> For *how it's built*, see [docs/architecture/OVERVIEW.md](docs/architecture/OVERVIEW.md);
> for *why each decision was made*, [docs/adr/](docs/adr/).

## The one idea

**Separate what is *allowed* from what is *decided*.** What's *decided* comes from a
black box (an LLM, a neural policy, a VLA) — capable, useful, and fundamentally
unverifiable. What's *allowed* comes from a space of checkable calculations. The
black box proposes; a verifiable layer disposes.

The move that makes this work — and it is the whole contribution — is this:

> **An LLM closes the verification loop not by becoming verifiable, but by
> *generating* verifiable constraints. The generated spec is checkable even though
> the process that generated it is not.**

You cannot verify the model. You *can* verify a specification the model writes, and
you can verify that a proposed action stays inside it. So we shift the burden off
the black box and onto its output — a checkable artifact (the **envelope**) — and
we check *actions against the envelope* before anything runs.

The same separation pays a second dividend, one openDaisugi is only now building
toward. Because an action is *proven* in-bounds, it becomes safe to make it
*cheaper*: compile a whole batch of steps you would never run unverified, route a
step to a smaller model you would not otherwise trust, compact the context that no
longer needs to hold the policy. **The envelope is not itself the token-saver — it is
the trust substrate that makes the savers safe.** Cost is a dividend of the boundary,
not a second system bolted beside it. (This reframing — substrate plus the cost
*levers* it underwrites — is [ADR-0011](docs/adr/0011-verifiable-execution-substrate.md),
and it was reached independently by five blind architects in a
[convergence experiment](docs/exploration/2026-08-blind-design-gauntlet/).)

## What this is

A **runtime-assurance layer** for agent and robot actions. Not an agent, not a
harness — a layer that any action-proposing system plugs into:

```
   black box proposes          openDaisugi disposes            world
  ─────────────────────      ───────────────────────      ───────────
  LLM agent / Codex /          generate envelope            safe actions run
  Claude Code / a VLA   ──▶    verify(plan ⊆ envelope) ──▶  unsafe → rejected
  / π0 / a script              supervise each step          novel → full model
                               journal · distill
```

The lineage is **Runtime Assurance (RTA)** from aerospace control — Simplex
(Sha et al., 1996), verified envelopes, barrier certificates — applied somewhere it
never has been: LLM agents and robot foundation models. We didn't invent the
architecture. We're the first to point it at these black boxes.

## The invariants (do not break these)

These are the load-bearing beliefs. Breaking one is a design regression, not a
feature. Each is enforced in tests and recorded as an ADR.

1. **Fail closed.** Unprovable ⇒ rejected. Undeclared ⇒ denied. In a verification
   library a *fail-open* — saying "safe" when it isn't — is the worst possible bug.
   ([ADR-0001](docs/adr/0001-fail-closed-default.md))
2. **Verify before execute.** No effect happens before its plan is proven inside
   its envelope; each step is re-checked at run time.
3. **The envelope is the authorization ceiling.** Reused pathways, delegated
   skills, and externally-supplied plans are bounded by the *caller's* envelope —
   never their own. ([ADR-0003](docs/adr/0003-envelope-as-contract.md))
4. **Independent provenance.** The "allowed" spec must not come from the same
   untrusted source as the "decided" plan. If one LLM writes both the plan *and*
   its envelope, you've proven consistency, not safety — that's why envelopes carry
   a human-or-more-trusted parent and can only *tighten*, never loosen.
5. **Layer, not harness.** openDaisugi gates actions; it does not drive the model.
   The moment it grows a chat loop it's competing with the harnesses it should
   plug into. ([ADR-0004](docs/adr/0004-layer-not-harness.md))
6. **Verify actions, not understanding.** An envelope can prove the arm stayed
   under 5N. It can *never* prove the model understood you wanted the fork and not
   the knife. This gap doesn't close — it gets bounded. Don't claim otherwise.

## Honest scorecard — built vs. aspirational

A guiding light has to tell the truth about where the light reaches. This code and
its comments were written by an AI assistant; treat them as *what currently exists*,
not as gospel — verify a claim before you rely on it. As of v0.39.x:

**Real, tested, load-bearing:**
- The envelope → `verify` → supervise → journal → distill spine. This is the whole
  thesis and it holds. ~20k LOC, ~1750 tests, CI green.
- `verify(plan, envelope)` across shell / file / network / MCP / robot capabilities,
  compiled to Z3 — sound after a dedicated security campaign closed the fail-opens.
- `envelope_subsumes` (delegation safety), inheritance (tightening proofs), the
  Supervisor (per-step re-verify, receipts, integrity), distillation into signed
  reusable pathways, the orchestrator (decompose → size → verified execute →
  synthesize), and swarm airspace deconfliction — all real and tested.
- Runs standalone (CLI), as an MCP server for any harness, or as a library. Works
  with a Claude Code subscription and no API key. (The MCP layer and `daisugi
  orchestrate` serve *any* harness; but *learning from a harness's own sessions* —
  passive capture → distil — is Claude-deep today: one parser, with a registered
  extension point for the rest.)
- The **empirical thesis, first measured** (v0.39). The `jit_metrics` ruler over a
  real journal: the symbolic guard runs a sub-4 ms median at a 0% timeout rate and,
  on that corpus, **zero tokens** (100% symbolic); ~46% of interactions are
  compilable into reusable pathways (an explicit *upper bound*). Zero-token reuse of
  a distilled deterministic pathway is shown end to end, offline, in
  [`examples/reuse-receipt/`](examples/reuse-receipt/). Scoped honestly: the corpus
  is benchmark-generated, so read it as "what the ruler finds here," then point it
  at your own journal.

**Aspirational — still documents, not shipped:**
- The **reuse-fidelity bar**: does distilled reuse stay as correct as fresh work
  across ≥20 tasks × 5 repeats, *on a local model*? A reliable-executor run cleared
  it in *direction* at small n (warm = cold = 100%, no safety regression), but the
  full bar and a genuinely local model are still open
  ([Stage 4](docs/roadmap.md#stage-4--the-distillation-fidelity-problem)). This is
  the honest remainder of the empirical thesis now that the *cost* side is measured.
- **Robotics on hardware**: the code is sim-only and plan-level. No 100Hz CBF-QP,
  no real arm, no π0-in-the-loop. That needs a hardware collaborator, exactly as
  the founding docs flagged.
- **Papers, defense/SBIR revenue, a marketplace**: pure optionality. Nothing is
  built toward them.
- **The garden of many models**: openDaisugi tending a whole household or org of
  deployed, purpose-fit models (a doorbell VLM, a CI agent, an inbox triager…) as
  one bounded, mostly-free fleet. The single-agent loop is real and tested; the
  multi-model *garden* is a framing that points the roadmap, not a shipped fleet
  manager.
- **The cost levers**: the boundary is what makes cheaper execution *safe*, and
  turning that into measured savings is the newer axis ([ADR-0011](docs/adr/0011-verifiable-execution-substrate.md)).
  The first two levers are now **built and tested**. A **deed ledger** — each
  reversible action records how to undo it, so a harness rolls back a wrong-but-allowed
  step from the ledger alone instead of the agent re-deriving its way out
  ([Stage 8](docs/roadmap.md#stage-8--the-reversibility-problem)). And **within-instance
  batch compilation** — an agent declares a batch, the library proves the whole blast
  radius is authorized *before any iteration*, refuses irreversible work, and runs it
  over a task's own internal repetition under a monitor with per-element rollback
  ([Stage 9](docs/roadmap.md#stage-9--the-within-instance-compilation-problem-honestly-baselined)).
  Stage 9's *mechanism and meter* ship; only its cross-instance numbers *at scale* wait
  on Stage 4's local model. The third lever now ships its mechanism too — a **rationale
  ledger** (a typed strata store) that externalizes *deliberation* so a compacted agent
  never re-derives it, aimed at the irreducible one-off, with constraint-promotion the one
  gated path from a captured invariant to a tightened envelope
  ([Stage 10](docs/roadmap.md#stage-10--the-rationale-durability-problem)); its
  re-derivation *numbers* wait on a model. All three levers are now built to the mechanism
  line. The honest baseline stays two separate ledgers — a small within-instance win (the
  proven blast radius, not tokens) and a compounding cross-instance one — never merged.

The core is real. Anything with a *venue*, a *contract*, or a *robot arm* attached
is still a hope. Keep that line bright.

## Horizons (problems, not a feature list)

The roadmap is framed as *problems* on purpose: for this project, describing a
capability precisely enough to schedule it is most of the work of building it, so a
dated feature list self-destructs. What endures is the open problems.

- **Near** — Independent-DAG-branch parallelism now covers both step kinds
  (`Supervisor(max_parallel=…)`, `daisugi orchestrate --max-parallel`): deterministic
  steps always, and LLM *task* (subagent) steps too when the run is unbudgeted —
  where per-step model choice is order-independent, so concurrent is provably identical
  to sequential. The remaining latency win is parallelizing task steps *under a budget*,
  which needs a reserve-then-spend protocol so concurrent sizing can't overshoot a hard
  ceiling. Round out the standalone TUI for watching runs / browsing the journal (a
  *monitoring* surface, not a harness).
- **Mid** — A real (if small) VLA in a verified swarm sim: the black box proposes
  trajectories, openDaisugi deconflicts the airspace and rejects out-of-envelope
  moves. The architecturally-novel demo the founding docs promised — buildable on
  a modest box because the *verifier*, not the policy, is the star.
- **Far** — The unsolved one worth naming: **perception-conditioned envelopes**
  that tighten under uncertainty ("5N with a clear view, 2N when occluded, stop
  below confidence τ"). Every formal guarantee here is conditional on perception
  being right; adaptive envelopes are the principled response, and nobody has built
  them for foundation models.

## The garden (where this is going)

The name is not decoration. **Daisugi** is a cultivation technique, and the vision
is that technique scaled up: a household or a company is already growing a *garden
of models* — the VLM that watches the front door, the model that triages the inbox,
the agent that runs CI, the arm in the workshop. Each is a black box doing one job,
and each needs the same three things: to be **the right tool, kept in bounds, and
cheap to run.**

openDaisugi is the gardener. It does not grow the plants — it is not the models,
not the harness — it *tends* them: **plant** (distil a recurring success into a
callable), **prune** (retire pathways that stop paying their way), **promote**
(grow a frozen script into a typed skill the first time an input varies), **graft**
(compose small verified callables into higher-tier ones), and **fence** (hold every
one inside a verified envelope).

Not every plant is the same, and that is the point — *determinism, speed,
flexibility, and complexity want different tools at different times.* An **ancient
oak** is a frozen script: proven, stable, rarely touched. A **weekend annual** is a
one-off the model plans fresh and throws away. A **perennial that needs tending** is
a parameterized skill the gardener re-fits as its use drifts. The unifying line
holds all the way up: the tool that is *cheapest* for a job is usually the one that
is *safest*, because both come from not putting a general intelligence where a
specific tool belongs. A garden of purpose-fit, envelope-bounded, mostly-free tools
is what *separate allowed from decided* looks like once you zoom out from one action
to a whole home or org.

**Honestly:** this is a north star, not a shipped product. Today openDaisugi tends
*one* agent's work well; the garden of many models is the framing that points the
roadmap, not a fleet manager you can run (see the scorecard).

## Name

**Daisugi** (台杉) — a forestry technique where straight new timber is cultivated
from the trunk of an existing tree, without new seeds. The black box is the
rootstock; the verified pathways are the cultivated growth. Prune what's routine,
supervise what's compiled, reason about what's novel.

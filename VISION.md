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
not as gospel — verify a claim before you rely on it. As of v0.34.x:

**Real, tested, load-bearing:**
- The envelope → `verify` → supervise → journal → distill spine. This is the whole
  thesis and it holds. ~20k LOC, ~1600 tests, CI green.
- `verify(plan, envelope)` across shell / file / network / MCP / robot capabilities,
  compiled to Z3 — sound after a dedicated security campaign closed the fail-opens.
- `envelope_subsumes` (delegation safety), inheritance (tightening proofs), the
  Supervisor (per-step re-verify, receipts, integrity), distillation into signed
  reusable pathways, the orchestrator (decompose → size → verified execute →
  synthesize), and swarm airspace deconfliction — all real and tested.
- Runs standalone (CLI), as an MCP server for any harness, or as a library. Works
  with a Claude Code subscription and no API key.

**Aspirational — still documents, not shipped:**
- The **empirical thesis**: "what fraction of real usage is compilable / how much
  does compilation save?" The machinery exists; the *measurement* has never been
  run. This is the cheapest high-value thing still on the table — and we now have a
  real journal on real usage to measure it against.
- **Robotics on hardware**: the code is sim-only and plan-level. No 100Hz CBF-QP,
  no real arm, no π0-in-the-loop. That needs a hardware collaborator, exactly as
  the founding docs flagged.
- **Papers, defense/SBIR revenue, a marketplace**: pure optionality. Nothing is
  built toward them.

The core is real. Anything with a *venue*, a *contract*, or a *robot arm* attached
is still a hope. Keep that line bright.

## Horizons (problems, not a feature list)

The roadmap is framed as *problems* on purpose: for this project, describing a
capability precisely enough to schedule it is most of the work of building it, so a
dated feature list self-destructs. What endures is the open problems.

- **Near** — Measure the compilation thesis on the real journal. Parallelize
  independent DAG branches (the one known perf win). Round out the standalone TUI
  for watching runs / browsing the journal (a *monitoring* surface, not a harness).
- **Mid** — A real (if small) VLA in a verified swarm sim: the black box proposes
  trajectories, openDaisugi deconflicts the airspace and rejects out-of-envelope
  moves. The architecturally-novel demo the founding docs promised — buildable on
  a modest box because the *verifier*, not the policy, is the star.
- **Far** — The unsolved one worth naming: **perception-conditioned envelopes**
  that tighten under uncertainty ("5N with a clear view, 2N when occluded, stop
  below confidence τ"). Every formal guarantee here is conditional on perception
  being right; adaptive envelopes are the principled response, and nobody has built
  them for foundation models.

## Name

**Daisugi** (台杉) — a forestry technique where straight new timber is cultivated
from the trunk of an existing tree, without new seeds. The black box is the
rootstock; the verified pathways are the cultivated growth. Prune what's routine,
supervise what's compiled, reason about what's novel.

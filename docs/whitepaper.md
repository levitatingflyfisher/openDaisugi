# openDaisugi — White Paper

*Runtime assurance for the outputs of unverifiable models: separating what is
allowed from what is decided.*

**Status:** conceptual/strategic overview. For the invariants see
[VISION.md](../VISION.md); for the mechanics, [architecture/OVERVIEW.md](architecture/OVERVIEW.md);
for the formal semantics, the [yellow paper](spec/yellow-paper.md). This document is
honest about the line between what is built and what is aspirational — see §7.

---

## Abstract

Capable AI systems — LLM agents, robot foundation models — are *unverifiable* in the
formal sense: stochastic, opaque, with undefined behavior outside their training
distribution. Formal systems are *verifiable* but limited. This is the verification
gap. openDaisugi is a runtime-assurance layer that bridges it not by verifying the
model, but by having the model **generate a verifiable specification** — a safety
*envelope* — and then checking proposed actions against that envelope, with an SMT
solver, before anything executes. The generated spec is checkable even though the
generation process is not. The architecture is Runtime Assurance (RTA) from
aerospace control; the novelty is applying it to LLM agents and robot policies,
which today deploy with no such layer.

## 1. The problem

Every agent framework treats a model's output as trustworthy-enough to act on. It
is not. An LLM agent that decides to `rm -rf` the wrong directory just does it. A
vision-language-action (VLA) model that grips at the wrong angle just does it. There
is no gate between "the model decided X" and "X happens to the world." The two
honest options have each been unsatisfying:

- **Verify the model.** Intractable. You cannot prove properties of a 100B-parameter
  black box, and its most useful behavior is precisely the un-prespecified kind.
- **Constrain the model so tightly it can't surprise you.** Then you didn't need a
  model — the tension at the heart of every "DSL for agents": the more you can
  pre-specify, the less you need the agent.

## 2. The idea

**Separate what is _allowed_ from what is _decided._**

- *Decided* comes from the black box. Keep it black. Let it be as capable as it is.
- *Allowed* comes from a space of verifiable calculations — a specification.

The load-bearing move: **the black box generates the specification, and a
deterministic layer enforces it.** An LLM can't be verified, but a *safety envelope
an LLM writes* can be — and so can the claim that a proposed plan stays inside it.
We shift the burden of trust off the model and onto a checkable artifact.

This resolves the DSL tension. The specification isn't pre-written by a human
anticipating every case (impossible) nor is it the model's raw output (untrusted).
It's model-generated *and* independently checkable, with a human-set ceiling it can
only tighten.

## 3. The lineage: Runtime Assurance

This is not a new architecture. It is a 30-year-old one from a new angle. Runtime
Assurance (RTA) is the discipline of deploying an unverified advanced system safely
by monitoring and constraining it at runtime. Four established patterns:

1. **Simplex** (Sha et al., 1996) — an advanced (unverified) controller plus a
   conservative (verified) baseline, with a decision module that switches to the
   baseline when a safety boundary is approached.
2. **NN-tuned classical control** — a neural net adjusts a PID/MPC's parameters; the
   system inherits the classical controller's guarantees within bounds.
3. **Verified envelope** — define the permitted action space; reject anything
   outside it. Verify *actions*, not perception.
4. **Compiled policy / live DSL** — high-level reasoning generates a constraint spec
   dynamically; a simpler system executes within it. The generated constraints are
   verifiable even though generation isn't.

RTA is deployed in F-16 collision avoidance, spacecraft, and autonomous vehicles.
It has *not* been applied to LLM agents or robot foundation models. That gap is the
opportunity. openDaisugi maps each pattern onto AI action: the compiled pathway is
the advanced controller, full-model reasoning is the baseline, the supervisor is the
decision module, and the envelope is the verified action space.

## 4. The architecture

One pipeline carries the whole system:

```
task ─▶ generate envelope ─┐
                           ▼
        plan ─▶ verify(plan ⊆ envelope) ─▶ supervise each step ─▶ journal ─▶ distill
                    │ fail → reject           │ receipts, integrity        │
                    └ (fail closed)           └ per-step re-check          └ reusable pathway
```

- **Envelope** — a `Permission` spec (allowed file/network/shell/MCP/robot
  capabilities) + invariants + postconditions + stakes. A checkable artifact.
- **Verify** — a staged gate (permissions → skill-subsumption → Z3 → predicate
  algebra → DAG). The proofs are SMT (Z3), not string matching; an unprovable or
  unknown result is treated as a *violation*. Delegation safety is the same proof
  applied to two envelopes: `envelope_subsumes(caller, contract)`.
- **Supervise** — execute step-by-step; re-verify each step; write a tamper-evident
  receipt; check run-end integrity so a silently-skipped step is detectable.
- **Journal → distill** — successful runs become traces; repeated traces distill
  into signed, reusable *pathways* (a plan template + the envelope that provably
  covers it), so recurring work skips the expensive model — supervised by the same
  verification stack.

## 5. Positioning: a layer, not a harness

The gravity in this space pulls every tool toward *becoming the agent*. openDaisugi
deliberately doesn't. It is a layer that plugs into whatever drives the model:

- **vs. harnesses (Claude Code, Codex, OpenClaw, Hermes, Gas Town)** — they own the
  driving loop and the messaging/skills. openDaisugi is the assurance + budget +
  memory substrate they *lack*. It integrates via an MCP server (the universal
  control pathway) and per-harness install adapters. The honest overlap: harnesses
  can already generate their own skills from usage; what none of them do is
  *runtime assurance* — checkable envelopes, verified delegation, fail-closed gates.
- **vs. prompt caching** — caching cuts the cost of *context*; distillation cuts the
  cost of *reasoning* by removing calls entirely. Complementary.

Becoming a harness would mean rebuilding those tools, badly, and abandoning the one
defensible position: the shared verification substrate.

## 6. Two domains, one core

The same domain-agnostic core (envelope, verify, supervisor, journal, distiller)
serves two applications of very different magnitude:

- **LLM agents — real but incremental.** Makes something that exists cheaper and
  safer. Genuine value; not, on its own, a novel contribution.
- **Robot foundation models — novel.** Nobody has built a Simplex-style RTA layer
  for VLA models. π0 and its kin output motor commands from pixels with no quality
  gate, no fallback controller, no safety envelope. The same architecture — the VLA
  proposes a trajectory, a constraint layer checks joint/velocity/force/collision
  bounds and deconflicts a swarm's airspace, a conservative baseline takes over when
  the envelope is violated — is straightforward to describe, maps directly onto
  published aerospace patterns, and does not exist for robot foundation models today.

## 7. What is built, and what is not

A white paper that overclaims is marketing. Honestly, as of v0.34.x:

**Built, tested, load-bearing** (~20k LOC, ~1600 tests, CI green): the entire
envelope → verify → supervise → journal → distill spine; Z3-backed verification
across shell/file/network/MCP/robot capabilities, made *sound* by a dedicated
security campaign that closed a set of fail-opens; delegation subsumption;
inheritance tightening proofs; the supervisor with receipts and integrity; pathway
distillation with signing; the forward orchestrator; and swarm airspace
deconfliction (analytic geometry, plan-level). It runs standalone, as an MCP server,
or as a library — with no API key required.

**Aspirational — documents, not code:** the *empirical* claim (what fraction of real
usage is compilable, and how much compilation actually saves) has never been
measured, though the machinery to measure it now exists. Robotics is **sim-only and
plan-level** — no 100Hz CBF-QP, no hardware, no π0 in the loop; that needs a
collaborator with an arm. Papers, defense/SBIR revenue, and a pathway marketplace
are optionality with nothing built toward them.

The honest boundary matters: swarm deconfliction is analytic geometry, not a
flight-safety certificate (waypoint-in-box ≠ path-in-box; set margins accordingly).
The verification is *sound for what it checks*; it verifies **actions, not
understanding** — an envelope can prove the arm stayed under 5N, never that the model
understood the task. That gap is bounded, not closed.

## 8. Why it's worth doing

Because the alternative — deploying capable, unverifiable models with no gate between
decision and effect — is what everyone does now, and it doesn't scale to physical
stakes or to agents with real permissions. The contribution is not a new model or a
new solver. It is the architecture that lets a capable black box operate under a
formal safety bound *that the black box itself helped write* — and the demonstration
that this same architecture spans LLM agents and robot policies, which are the same
problem wearing different clothes.

---

## References

- Sha, L. (1996/2001). *Using Simplicity to Control Complexity.* IEEE Software — the foundational Simplex Architecture paper.
- Schierman, J. et al. (2015). *Runtime Assurance for aerospace systems.*
- Wood, G. *Ethereum Yellow Paper* — the formal-specification register this project's [yellow paper](spec/yellow-paper.md) borrows.
- Black, K. et al. (2024). *π0: A Vision-Language-Action Flow Model for General Robot Control.* Physical Intelligence.
- de Moura, L. & Bjørner, N. (2008). *Z3: An Efficient SMT Solver.* — the solver behind the verification core.
- Diátaxis (Procida, D.) — the documentation framework this project's [docs](README.md) follow.

*The code and comments referenced here were authored by an AI assistant and describe
what currently exists — take them with gratitude and a grain of salt, and verify
before relying.*

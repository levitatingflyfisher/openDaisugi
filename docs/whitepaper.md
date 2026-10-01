# openDaisugi: White Paper

*Runtime assurance for the outputs of unverifiable models: separating what is
allowed from what is decided.*

**Status:** conceptual/strategic overview, refreshed 2026-09-26 for v0.43.0. For the
invariants see [VISION.md](../VISION.md); for the mechanics,
[architecture/OVERVIEW.md](architecture/OVERVIEW.md); for the formal semantics, the
[yellow paper](spec/yellow-paper.md); for our terms in other fields' words,
[correspondence.md](correspondence.md). This document is honest about the line between
what is built and what is aspirational (§7). §9 gives the direction (dialects) and §10
the related work.

---

## Abstract

Capable AI systems (LLM agents, robot foundation models) are *unverifiable* in the
formal sense: stochastic, opaque, with undefined behavior outside their training
distribution. Formal systems are *verifiable* but limited. This is the verification
gap. openDaisugi is a runtime-assurance layer that bridges it not by verifying the
model, but by having the model **generate a verifiable specification** (a safety
*envelope*) and then checking proposed actions against that envelope, with an SMT
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
  model. This is the tension at the heart of every "DSL for agents": the more you can
  pre-specify, the less you need the agent.

## 2. The idea

**Separate what is _allowed_ from what is _decided._**

- *Decided* comes from the black box. Keep it a black box. Let it be as capable as it is.
- *Allowed* comes from a space of verifiable calculations: a specification.

The load-bearing move: **the black box generates the specification, and a
deterministic layer enforces it.** An LLM can't be verified, but a *safety envelope
an LLM writes* can be. So can the claim that a proposed plan stays inside it.
We shift the burden of trust off the model and onto a checkable artifact.

This resolves the DSL tension. The specification isn't pre-written by a human
anticipating every case (impossible) nor is it the model's raw output (untrusted).
It's model-generated *and* independently checkable, with a human-set ceiling it can
only tighten.

The boundary also carries a cost story, and it is worth stating precisely because it
is easy to get backwards. The envelope is not itself a token-saving trick. It is the
**trust substrate** that makes token-saving *safe*: once an action is proven
in-bounds, you can compile a batch you would never run unverified, route to a smaller
model you would not otherwise trust, and compact context that no longer needs to hold
the policy. So openDaisugi is a verifiable-execution substrate plus a small family of
cost *levers* it underwrites. Five blind
architects reached this framing independently in a [convergence experiment](exploration/2026-08-blind-design-gauntlet/)
and it is recorded as [ADR-0011](adr/0011-verifiable-execution-substrate.md). All three
levers are now built to the mechanism line: the deed ledger (v0.41), within-instance
batch compilation with a net-token meter (v0.42), and the rationale store (v0.43)
([roadmap.md](roadmap.md), Stages 8 to 10). Their savings at scale are not yet
measured (§7).

## 3. The lineage: Runtime Assurance

This is not a new architecture. It is a 30-year-old one from a new angle. Runtime
Assurance (RTA) is the discipline of deploying an unverified advanced system safely
by monitoring and constraining it at runtime. Four established patterns:

1. **Simplex** (Sha et al., 1996): an advanced (unverified) controller plus a
   conservative (verified) baseline, with a decision module that switches to the
   baseline when a safety boundary is approached.
2. **NN-tuned classical control**: a neural net adjusts a PID/MPC's parameters; the
   system inherits the classical controller's guarantees within bounds.
3. **Verified envelope**: define the permitted action space; reject anything
   outside it. Verify *actions*, not perception.
4. **Compiled policy / live DSL**: high-level reasoning generates a constraint spec
   dynamically; a simpler system executes within it. The generated constraints are
   verifiable even though generation isn't.

RTA is deployed in F-16 collision avoidance, spacecraft, and autonomous vehicles.
Few published LLM-agent systems name the Simplex lineage. Robotics work such as
RoboGuard is the closest, and solver-checked agent policies such as Progent and ActGov
are near neighbours (§10). We found no system that applies one RTA gate to both
software agents and robot foundation models. "Not found" is not "does not exist". That
gap is the opportunity. openDaisugi maps each pattern onto AI action: the compiled pathway is
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

- **Envelope**: a `Permission` spec (allowed file/network/shell/MCP/robot
  capabilities) + invariants + postconditions + stakes. A checkable artifact.
- **Verify**: a staged check (permissions → skill-subsumption → Z3 → predicate
  algebra → DAG). The proofs are SMT (Z3), not string matching; an unprovable or
  unknown result is treated as a *violation*. Delegation safety is the same proof
  applied to two envelopes: `envelope_subsumes(caller, contract)`.
- **Supervise**: execute step-by-step; re-verify each step; write a tamper-evident
  receipt; check run-end integrity so a silently-skipped step is detectable.
- **Journal → distill**: successful runs become traces; repeated traces distill
  into signed, reusable *pathways* (a plan template + the envelope that provably
  covers it), so recurring work skips the expensive model. The same
  verification stack supervises it.

The pipeline above checks a declared plan. Most agents today declare no plan; they
emit tool calls one at a time inside a host harness. For them, the **gate**
([ADR-0007](adr/0007-call-time-gate.md), `src/opendaisugi/gate.py`) checks each call
against a registered envelope before it runs, and answers allow, deny or ask. It runs
in audit mode by default: it logs what it would deny and allows. The yellow paper §8
states what a per-call check can and cannot promise.

Two cost parts sit beside the spine:

- **The gateway** ([ADR-0012](adr/0012-gateway-reuse-and-answer-store.md),
  [ADR-0015](adr/0015-cache-aware-sticky-routing.md), `src/opendaisugi/gateway.py`) is
  a `base_url` proxy. It routes whole turns to a model tier, meters each turn, and
  offers reuse of stored answers and pathways. It can hand the model choice to NVIDIA
  NeMo Switchyard and stay the meter ([how-to](how-to/router-switchyard.md)). The
  honest ceiling for routing is about 3× on a blended day, not 100×
  ([how-to](how-to/token-saving-gateway.md)).
- **Pathways** serve recurring work at Tier 0, with no model call. A reused pathway
  is re-verified against the **caller's** envelope, never its own (yellow paper §5).
  A router that learns from verified results is designed, not built
  ([design-router.md](plans/2026-09-24-omarchy/design-router.md)).

### 4.1 The JIT analogy

A just-in-time compiler interprets first, profiles, and compiles the hot paths.
openDaisugi does the same with agent work:

| JIT part | openDaisugi part | State |
|---|---|---|
| Interpreter (slow, general, always works) | the frontier model, reasoning from scratch | rented |
| Profiler | the journal: every action, verdict and token | Built (`journal.py`) |
| Tiered compilation | the routing ladder: pathway (0 tokens), local model, warm cache, cheap model, frontier | Built in part (`routing.py`, the gateway); the learned router is Designed |
| Profile-guided optimizer | the garden, which compiles repeated work into pathways | Built (`distiller.py`, `gardener/`) |
| Type checker | Z3 and the envelope, before anything runs | Built (`verify.py`) |
| Deoptimization | fail closed: a failed proof falls back to the frontier or to a human | Built as re-verification plus deny or ask; no explicit pathway guards yet ([correspondence.md](correspondence.md), Part C, gap 8) |
| The runtime's sandbox | the gate | Built (`gate.py`) |
| Debugger and console | coppice | Built (`harness/coppice/`) |

The pitch in one breath: *the first time, a frontier model does the work and a formal
proof checks every action before it runs. The tenth time, it runs as a verified script
for free.* The second sentence is a mechanism, not a measurement. How much real work
recurs, and how much reuse saves, is not measured yet (roadmap Stage 4; §7).

## 5. Positioning: daisugi is not a harness

openDaisugi is three parts ([ADR-0020](adr/0020-layer-floor-loop.md)). **daisugi** is
the gate, the verifier, the journal and the garden: a library that imports nothing
above it and runs alone in any harness. **coppice** is the floor: a Go daemon that
supervises panes of agents and takes every state it shows from the gate. The **loop**
is whatever harness runs in a pane; sprig is ours and is on hold with the owner, and
the rest are rented. The rule below applies to daisugi.

The gravity in this space pulls every tool toward *becoming the agent*. daisugi
deliberately doesn't. It plugs into whatever drives the model. sprig, our own loop,
sits above daisugi and uses it like any other harness; it is on hold with the owner.

- **vs. harnesses (Claude Code, Codex, OpenClaw, Hermes, Gas Town)**: they own the
  driving loop and the messaging/skills. daisugi is the assurance + budget +
  memory substrate they *lack*. It integrates mainly through the gate hook
  (`daisugi install --gate` for Claude Code and Codex, an extension for pi), and also
  through an MCP server and per-harness install adapters. The honest overlap: harnesses
  can already generate their own skills from usage, and they offer permission
  rules and hooks. What we did not find in them is *runtime assurance*: checkable
  envelopes, proved delegation, fail-closed gates. Policy engines such as AWS
  AgentCore on Cedar come closer (§10).
- **vs. prompt caching**: caching cuts the cost of *context*; distillation cuts the
  cost of *reasoning* by removing calls entirely. Complementary.

If daisugi became a harness, it would rebuild those tools, badly, and abandon the one
defensible position: the shared verification substrate.

## 6. Two domains, one core

The same domain-agnostic core (envelope, verify, supervisor, journal, distiller)
serves two applications of very different magnitude:

- **LLM agents (real but incremental).** Makes something that exists cheaper and
  safer. Genuine value; not, on its own, a novel contribution.
- **Robot foundation models (novel).** We found no Simplex-style RTA gate for
  VLA models; RoboGuard, the nearest, repairs LLM plans against temporal-logic
  rules (§10). π0 and its kin output motor commands from pixels with no quality
  gate, no fallback controller, no safety envelope. The same architecture applies. The VLA
  proposes a trajectory, a constraint layer checks joint/velocity/force/collision
  bounds and deconflicts a swarm's airspace, a conservative baseline takes over when
  the envelope is violated. It is straightforward to describe, maps directly onto
  published aerospace patterns, and we found no build of it for robot foundation models.

## 7. What is built, and what is not

A white paper that overclaims is marketing. Honestly, as of v0.43.0 (2026-09-26):

**Built and tested.**

- The spine: envelope → verify → supervise → journal → distill. Z3-backed checks
  cover shell, file, network, MCP and robot capabilities. Delegation subsumption,
  inheritance tightening, the supervisor with receipts and run integrity, pathway
  distillation with signing, the forward orchestrator, and swarm airspace
  deconfliction (analytic geometry, plan level). It runs as a library, as an MCP
  server, or from the CLI, with no API key.
- The call-time gate ([ADR-0007](adr/0007-call-time-gate.md)), audit mode by
  default, with ask tiers and a resident process.
- Security campaigns keep closing fail-opens. On 2026-09-26 alone: subsumption now
  also checks stakes, custom step types and budgets, and a Z3 timeout there denies in
  every mode; a hook payload's session id never selects an envelope; definition arguments
  are substituted in one pass and escaped in regex fields, and no definition can shadow a
  system word (commits `f9e202e7`, `31871c4d`, `92f87edf`).
- The gateway: whole-turn routing by rules or by NeMo Switchyard, a per-turn meter,
  and reuse of answers and pathways (ADR-0012, ADR-0015).
- The three cost levers of ADR-0011, to the mechanism line: the deed ledger (v0.41),
  batch compilation with a net-token meter (v0.42), and the rationale store with
  constraint promotion (v0.43). The within-instance meter already reports the honest
  finding: the win is the proven blast radius, not tokens ([roadmap.md](roadmap.md),
  Stage 9).
- **Three implementations.** Python is the oracle. Go and Rust each ship a
  standalone `daisugi` binary that never calls Python at run time. Both link a real
  Z3 (5.1.0, built from pinned source, linked statically) and parse shell with the
  same tree-sitter-bash grammar as the oracle ([clients/go/README.md](../clients/go/README.md),
  [clients/rust/README.md](../clients/rust/README.md)). The binaries are compared with
  the oracle on committed golden cases: 1,310 gate cases, 472 CLI cases, 223 gateway
  cases, 215 garden cases and 158 pathway command cases (the manifests in
  `clients/fixtures/`), plus a local verifier corpus of 11,450 cases that is never
  published because it holds real paths. The Rust README records 1,310 of 1,310 gate
  cases and 11,450 of 11,450 corpus cases in agreement. Where a binary may differ from
  the oracle, the ruling is in [clients/ADJUDICATIONS.md](../clients/ADJUDICATIONS.md).
  Full parity is the goal (owner ruling, 2026-09-26), not yet the state: a few CLI
  commands, and the parts that use pathways at run time and generate envelopes
  (stage K of the port plan), still need Python.
- **Install.** One script (`scripts/install.sh`) builds and installs `daisugi`,
  coppice and sprig. A floor then takes two commands: `daisugi install --gate`, then
  `coppice` (or `coppice web` for a browser or a phone).

These case counts come from synthetic cases. They show that the three
implementations agree and that the checks are correct on those cases. They do not
show that the gate is useful on real work: its false-deny rate, the gateway's savings
on real traffic, and coppice across many projects are the next measurements (the
"dogfood week" in the [strategy notes](plans/2026-09-24-omarchy/strategy-2026-09-26.md)).

**Designed, not built.** A router that learns from verified results
([design-router.md](plans/2026-09-24-omarchy/design-router.md)). Grafts, a rewrite
verdict proved at install ([design-grafts.md](plans/2026-09-24-omarchy/design-grafts.md)).
Ranking of N attempts as a primitive ([design-ranking.md](plans/2026-09-24-omarchy/design-ranking.md)).
Dialects (§9). The full edge rule for the delegation tree
([design-delegation-tree.md](plans/2026-09-24-omarchy/design-delegation-tree.md); part of it is
built, see yellow paper §5.5). sprig and weave are on hold with the owner
([ADR-0022](adr/0022-weave-runner-for-verified-plan-trees.md) is Proposed).

**Aspirational.** The *empirical* claim (what fraction of real usage is compilable,
and how much compilation actually saves) has never been measured, though the
machinery to measure it exists (roadmap Stage 4). Robotics is **sim-only and plan-level** (no 100Hz CBF-QP, no hardware, no π0
in the loop). That needs a collaborator with an arm. Papers, defense/SBIR revenue, and
a pathway marketplace are optionality with nothing built toward them.

The honest boundary matters: swarm deconfliction is analytic geometry, not a
flight-safety certificate (waypoint-in-box ≠ path-in-box; set margins accordingly).
The verification is *sound for what it checks*; it verifies **actions, not
understanding**. An envelope can prove the arm stayed under 5N, never that the model
understood the task. That gap is bounded, not closed.

## 8. Why it's worth doing

Because the alternative (deploying capable, unverifiable models with no gate between
decision and effect) is what everyone does now, and it doesn't scale to physical
stakes or to agents with real permissions. The contribution is not a new model or a
new solver. It is the architecture that lets a capable black box operate under a
formal safety bound *that the black box itself helped write*, and the demonstration
that this same architecture spans LLM agents and robot policies, which are the same
problem wearing different clothes.

There is a second reason, quieter but real: the architecture appears to be the
*convergent* one. Five independent architects, each given a neutral statement of the
problem and told nothing about openDaisugi, rebuilt its spine: enforcement outside
the context window, fail-closed pre-execution gating, a human floor a model may only
tighten. Two of the pieces they reinvented were never hinted at in their brief
(the monotone-narrowing subsumption law and within-instance compilation). Convergence
that was not planted is the cheapest strong evidence a design is right. The
[record](exploration/2026-08-blind-design-gauntlet/) is public, adversarial critiques
and sober scores included.

## 9. Direction: one kernel, many dialects

*Status: Designed ([design-dialects.md](plans/2026-09-24-omarchy/design-dialects.md)),
not built, except the definitions it grows from.*

RTA pattern 4 (§3) was the early dream: a model writes a small language for one
situation, and a simple checked system runs inside it. The envelope and the predicate
algebra are that language, and Z3 is the checker under it. But the language is fixed:
one grammar for a coding task and for a drone. The direction is to let models write
the **vocabulary** for a domain, while the grammar and the proofs stay fixed.

- **One kernel.** The `Permission` record and the predicate algebra stay small and
  fixed. The guarantees live there.
- **Many dialects.** A dialect is a list of named words, each defined only in kernel
  terms: `read_only_inspection` or `must_not_modify(target)` for a software team;
  `within_geofence(zone)` or `keep_separation(d)` for a swarm, once the kernel can say
  them. The checker never trusts a word. It **unfolds** each word to its kernel
  definition and checks the kernel term with the same code as today.
- **A definitional extension.** Because every word is an abbreviation that can be
  eliminated, a dialect proves nothing new about the kernel. Every soundness property
  of the verifier carries over unchanged. Only the unfolding function joins the
  trusted code (yellow paper §2.4).
- **Learned from the journal.** No person writes a dialect (owner ruling: the bots
  write the policy). The garden mines the journal for repeated sub-policies, a model
  names them, and Z3 proves two words equal so the vocabulary does not grow
  duplicates. This is library learning (§10) applied to the policy that governs
  actions. A word is promoted only when it is reused on held-out envelopes, the bar
  the library-learning sceptics set.
- **Checked names.** A name carries no meaning for the checker, but a misleading
  name misleads the human who reads a deny. A second model sees only the name and
  writes the claims it implies; Z3 proves each claim from the definition or returns a
  counterexample.
- **Dialects meet at the kernel.** Two dialects unfold to the same kernel, so their
  words can be compared by the relations that exist today: equivalence, subsumption
  and disjointness.

The honest limits:

- **The kernel is not decidable as a whole.** Set and glob membership over finite
  allowlists and linear arithmetic are decidable. String constraints with regular
  expressions and lengths are decidable only in restricted fragments, and our
  encoding is not shown to lie in one. Z3 can answer "unknown" on legal input, and
  unknown means deny. Unfolding adds no theory, so a question in a dialect is exactly as
  hard as the same question in the kernel.
- **Robot words need kernel work first.** Today the kernel has one bounding box per
  envelope, no force field on gripper steps, and no cross-agent predicate. Nonlinear
  dynamics may make Z3 answer unknown, and the gate then denies.
- **Shell writes are not visible to predicates.** `must_not_modify` can cover
  `file_write` steps only, until each shell step's decomposed write paths become a
  field the algebra can read.

**Evidence so far (in-sample, small).** A greedy pass over 294 real envelopes from
the owner's journal cut their permission sets from 2,769 atoms to 2,284, word
definitions included: 17.5% with 15 words that read like a dialect (read-only
inspection, build and test, code forges, config files). A second count found that
the models already write a dialect with no definitions: 0 of 322 invariants in those
envelopes carry a checkable expression, and `file_unchanged` alone appears 75 times.
Today such an invariant is not enforced at low or medium stakes: with no expression
and no handler, it is documentation, and strict mode (on by default only at high and
physical stakes) is what would refuse it. So the first payoff of dialects is not
compression. It is to give the invariants the
models already write one kernel definition, so they are enforced.

## 10. Related work

This section draws on two surveys made on 2026-09-26:
[research/prior-art-2026-09-26.md](research/prior-art-2026-09-26.md) and
[correspondence.md](correspondence.md), plus the gateway and grafts notes named below.
Most rows rest on abstracts; a claim here is no deeper than the survey that read it.
Where the survey marked a source "search listing", so does this section. For each
neighbour we say what we borrow and what differs.

### 10.1 Runtime assurance and shields

- **Simplex** (Sha 1996; Sha, *Using Simplicity to Control Complexity*, IEEE Software
  2001, [slides](https://engineering.purdue.edu/dcsl/reading/2007/foob-using_simplicity_to_control_complexity.pdf);
  [Black-Box Simplex](https://arxiv.org/abs/2102.12981)). We borrow the split: an
  untrusted, capable controller beside a simple, trusted check. What differs: Simplex
  keeps the plant where a safe controller can recover it over time. Our gate only
  prevents single actions and makes no recoverable-state promise (yellow paper §8.6).
- **ASTM F3269** ([store page](https://store.astm.org/f3269-21.html), search listing),
  the aviation RTA standard. We borrow the frame of bounding a function that cannot be
  certified by design. We have no certification claim.
- **Shields** (Alshiekh et al. 2017, [arXiv 1708.08611](https://arxiv.org/abs/1708.08611)).
  A shield enforces a temporal-logic specification on a learning agent, by listing safe
  actions first or by correcting an unsafe one. Our gate is a blocking shield, and a
  graft (designed) would be a correcting one. Our specification is per step, not
  temporal.
- **RoboGuard** ([arXiv 2503.07885](https://arxiv.org/abs/2503.07885)) grounds fixed
  safety rules into temporal logic for the current scene and repairs unsafe plans by
  control synthesis; unsafe plan execution fell from over 92% to below 3%. It is the
  closest robotics neighbour. We borrow the idea of grounding rules per scene. What
  differs: one gate for software agents and robots, and model-written envelopes.
  Reachability checks of LLM commands ([arXiv 2503.03911](https://arxiv.org/abs/2503.03911))
  compute reachable sets; our swarm check is a static box check, not reachability.

### 10.2 Reference monitors and capabilities

- **Reference monitor** ([Wikipedia](https://en.wikipedia.org/wiki/Reference_monitor))
  and **execution monitors** (Schneider 2000,
  [PDF](https://www.cs.cornell.edu/fbs/publications/EnfSecPols.pdf)). The gate is a
  reference monitor, and Schneider's result tells us exactly what it can enforce:
  safety properties (yellow paper §8.1). "Always invoked" holds only while the host
  fires the hook (yellow paper §8.5).
- **Fail-safe defaults** (Saltzer and Schroeder,
  [text](https://www.cs.virginia.edu/~evans/cs551/saltzer/)). "Fail closed" is this
  principle: base access on permission, not exclusion.
- **Capabilities and attenuation** (Macaroons,
  [Google Research](https://research.google/pubs/macaroons-cookies-with-contextual-caveats-for-decentralized-authorization-in-the-cloud/);
  [UCAN spec](https://github.com/ucan-wg/spec)). Each delegation must restate or
  narrow its authority. We borrow the rule for skills and for a tree of agents. What
  differs: containment is proved by Z3 over envelope languages (globs, shell heads,
  predicates, robot bounds), not checked by string match on a token list.

### 10.3 Skill libraries

- **Voyager** ([arXiv 2305.16291](https://arxiv.org/abs/2305.16291)) keeps a growing
  library of code skills that compose. **Agent Workflow Memory**
  ([arXiv 2409.07429](https://arxiv.org/abs/2409.07429)) and **Memp**
  ([arXiv 2508.06433](https://arxiv.org/abs/2508.06433)) induce reusable workflows and
  retire stale ones. We borrow online induction and deprecation. What differs: a
  pathway must fit an envelope before reuse, and the gate re-checks every step.
- **Newer learners.** SkillGen ([arXiv 2605.10999](https://arxiv.org/html/2605.10999v1))
  counts repairs against regressions on identical inputs before a skill goes live;
  SkillForge ([arXiv 2608.24747](https://arxiv.org/html/2608.24747)) and SkillOps
  ([arXiv 2605.13716](https://arxiv.org/html/2605.13716v1)) score and maintain skill
  banks. Their "verified" means "it helped the task succeed". We borrow SkillGen's
  paired count as a stronger test for the garden. What differs: our check is a
  safety check, not only a success check.
- **Anthropic Agent Skills**
  ([engineering post](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills))
  load a skill by progressive disclosure: name first, body on demand. We borrow that
  for listing dialect words in a prompt. What differs: the gate, not a skill's own
  metadata, decides what a skill may do. The need is measured: one study found a
  vulnerability in 26.1% of 31,132 marketplace skills
  ([arXiv 2601.10338](https://arxiv.org/abs/2601.10338)).

### 10.4 Library learning

- **DreamCoder** ([arXiv 2006.08381](https://arxiv.org/abs/2006.08381)) and **Stitch**
  ([arXiv 2211.16605](https://arxiv.org/abs/2211.16605)) compress a corpus of programs
  into named abstractions; Stitch is 3 to 4 orders of magnitude faster than DreamCoder's
  compressor. We plan to use the Stitch CLI, offline, for mining words over predicate
  and plan trees. What differs: our terms are policy, and Z3 proves two words equal.
- **LILO** ([arXiv 2310.19791](https://arxiv.org/abs/2310.19791)) has a model write
  names and docstrings for learned abstractions. We borrow that. What differs: we add
  a check of each name against its definition, which LILO does not have.
- **babble** ([arXiv 2212.04596](https://arxiv.org/abs/2212.04596)) learns libraries
  modulo an equational theory with e-graphs. We use Z3 equivalence over the kernel in
  place of a hand-written theory; babble is a later option for mining.
- **The sceptics.** "Library Learning Doesn't" ([arXiv 2410.20274](https://arxiv.org/abs/2410.20274))
  finds function reuse "extremely infrequent"; a LEGO-Prover study
  ([arXiv 2504.03048](https://arxiv.org/abs/2504.03048)) finds gains vanish once compute
  is counted; an EACL 2026 paper ([ACL Anthology](https://aclanthology.org/2026.eacl-long.163/))
  asks for equal budgets and behavioural checks. We adopt their bar: a word must be
  reused on held-out envelopes, with its cost counted, before we claim anything.

### 10.5 Model-written policies checked by a solver

- **Cedar and SymCC** ([arXiv 2403.04651](https://arxiv.org/abs/2403.04651);
  [cedar-policy-symcc](https://github.com/cedar-policy/cedar/tree/main/cedar-policy-symcc)).
  Cedar is designed for a sound and complete SMT encoding; SymCC checks always-allows,
  always-denies, subsumption, equivalence and disjointness with counterexamples, and
  its encoder is verified in Lean. AWS AgentCore uses Cedar for agent tool calls
  (finding 4 of [formal-methods-2026-09-23.md](research/formal-methods-2026-09-23.md)). We
  borrow the lint set and the small, analysable kernel. What differs: per-action
  proofs, model-written envelopes, learned pathways. Cedar is ahead on a verified
  encoder.
- **Progent** ([arXiv 2504.11703](https://arxiv.org/abs/2504.11703)). An LLM writes and
  updates policies; Z3 decides whether an update narrows (automatic) or widens (needs
  approval). We have the same narrowing law (`src/opendaisugi/subsumption.py`) and
  borrow the approval rule for dialect changes. What differs: shell effects, a journal,
  a garden.
- **ActGov** ([arXiv 2609.24446](https://arxiv.org/html/2609.24446v2)). An LLM drafts
  rules from tool specs, benign traces and attack failures; Z3 searches offline for a
  record that satisfies the policy and breaks a safety assertion. Attack success is at
  or below 0.007 on AgentDyn. We borrow learning rules from failures. What differs: its
  named rules are hand-organized over finite values; our words would be mined, named by
  a model, deduplicated by proof, and checked per action.
- **Others.** A solver-aided compliance checker runs Z3 per planned call and returns the
  unsat core as the reason ([arXiv 2603.20449](https://arxiv.org/html/2603.20449v1));
  VeriGuard proves the policy code itself ([arXiv 2510.05156](https://arxiv.org/html/2510.05156));
  Agent-C checks order constraints during decoding ([arXiv 2512.23738](https://arxiv.org/abs/2512.23738));
  C-Trace checks predicates over the event stream ([arXiv 2606.19242](https://arxiv.org/html/2606.19242v1));
  AgentSpec uses Python predicates, no solver ([arXiv 2503.18666](https://arxiv.org/abs/2503.18666));
  CaMeL tracks data flow against prompt injection ([arXiv 2503.18813](https://arxiv.org/abs/2503.18813)).
  We borrow the unsat core as a deny reason (not built). The others cover order,
  history and data flow, which our per-step envelope does not.

### 10.6 Token-saving routers

- **RouteLLM** ([arXiv 2406.18665](https://arxiv.org/abs/2406.18665)) and **FrugalGPT**
  ([arXiv 2305.05176](https://arxiv.org/abs/2305.05176)) route or cascade across model
  tiers. The designed router borrows "cheap first, escalate". What differs: it would
  escalate on external signals (a gate deny, a failed postcondition), never on the
  cheap model's own report.
- **NVIDIA NeMo Switchyard** ([GitHub](https://github.com/NVIDIA-NeMo/Switchyard), read
  at tag v0.3.0) is a trained chooser. We do not compete with it: the gateway can run
  Switchyard behind it and stay the meter, so each saving is tied to the model that
  served the turn ([how-to](how-to/router-switchyard.md)).
- **Spotify's shunt** ([post](https://engineering.atspotify.com/2026/9/portal-by-spotify-cut-my-claude-code-token-usage-by-90);
  [code](https://github.com/spotify/portal-ai-plugins/tree/main/plugins/shunt), Apache-2.0)
  blocks large file reads in Claude Code with a hook and redirects the model to a skill
  that asks a cheaper model. It denies and redirects; it does not rewrite the call. Its
  82% to 94% savings are the author's own, count tokens rather than billed cost, and
  report no task success rate ([grafts inputs](research/grafts-inputs-2026-09-26.md)).
  We borrow the pattern for grafts (designed): a rewrite verdict, proved against the
  envelope at install and learned from the journal.

### 10.7 Where neighbours are ahead

- **Envelope lint.** Cedar SymCC lints whole policies and has a Lean-verified encoder.
  We check one predicate at a time for vacuity (`src/opendaisugi/vacuity.py`), and our
  SMT encoders are trusted code, checked only by differential tests across three
  implementations.
- **Temporal properties.** Agent-C, C-Trace and ContrAgent (see
  [formal-methods-2026-09-23.md](research/formal-methods-2026-09-23.md)) check order
  and history.
  Our envelope is per step.
- **Benchmarks.** The neighbours report AgentDojo, AgentDyn and tau2-bench numbers. We
  report none; our numbers come from synthetic cases.
- **Adversarial skills.** We have not tested the gate against a corpus of malicious
  skills.

### 10.8 Where we may be first

We did not find these in the surveys. "Not found" is not "does not exist".

1. Learned, proved dialects for the policy that governs agent actions (§9).
2. Skills that must fit an envelope before reuse, with every step re-checked.
3. One verifier that gates both software agents and robots.
4. Per-action SMT proof across harnesses, with a journal that feeds a distiller.

---

## References

- Sha, L. (1996/2001). *Using Simplicity to Control Complexity.* IEEE Software. It is the foundational Simplex Architecture paper.
- Schierman, J. et al. (2015). *Runtime Assurance for aerospace systems.*
- Wood, G. *Ethereum Yellow Paper*. It is the formal-specification register this project's [yellow paper](spec/yellow-paper.md) borrows.
- Black, K. et al. (2024). *π0: A Vision-Language-Action Flow Model for General Robot Control.* Physical Intelligence.
- de Moura, L. & Bjørner, N. (2008). *Z3: An Efficient SMT Solver.* It is the solver behind the verification core.
- Diátaxis (Procida, D.): the documentation framework this project's [docs](README.md) follow.
- Schneider, F. B. (2000). *Enforceable Security Policies.* ACM TISSEC 3(1). What an execution monitor can enforce.
- Alshiekh, M. et al. (2017). *Safe Reinforcement Learning via Shielding.* [arXiv 1708.08611](https://arxiv.org/abs/1708.08611).
- Cutler, J. W. et al. (2024). *Cedar: A New Language for Expressive, Fast, Safe, and Analyzable Authorization.* [arXiv 2403.04651](https://arxiv.org/abs/2403.04651).
- Bowers, M. et al. (2023). *Top-Down Synthesis for Library Learning* (Stitch). [arXiv 2211.16605](https://arxiv.org/abs/2211.16605).
- The other works in §10 are cited inline, with the URL each survey read.

*The code and comments referenced here were authored by an AI assistant and describe
what currently exists. Take them with gratitude and a grain of salt, and verify
before relying.*

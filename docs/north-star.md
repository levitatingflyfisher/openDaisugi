# North Star

> **This document is aspirational.** It says where openDaisugi is *pointed*, not where it
> *is*. For the honest present, what is built, tested and load-bearing today, read the
> scorecard in [VISION.md](../VISION.md), the [feature status](feature-status.md) and the
> near ground in the [roadmap](roadmap.md). Everything below is a direction, held with a
> grain of salt.

## One architecture, three scales

Three futures pull on this project. They look like three products. They are one
architecture at three scales, and it is the one the project is named after.

*Daisugi* (台杉) is the Japanese forestry art of growing many straight shoots, each
shaped for one purpose, from one carefully tended base stump. The shoots are cut and grow
again for centuries, and the root never dies. Read the whole project through it:

- **The base stump** is the tended core that survives every harvest: the verified
  envelope, the pathway store, the gate.
- **The shoots** are distilled skills, each cut to the exact shape of one purpose: cheap,
  repeatable, provably in bounds, grown from the base.
- **The gardener** is distillation itself: it turns a journaled run into a new shoot and
  prunes the shoots that stop paying.

Under all three futures runs one loop: **verify** (make it safe), **route and size** (make
it fast and cheap), **journal** (make it accountable), **distill** (make it a repeatable
skill). The dreams differ in scale, and in which hard problem each one stresses most.

## The three dreams

### 1. The safe, fast, accountable pilot

A drone, or any real-time agent, with a model's judgment for new situations, reflexes fast
enough to act at the speed of thought, and a record clear enough to explain every action
after the fact, including a proof that it took *the safest action available at the time*.

Its deepest structure is already this project's ancestry: **Simplex runtime assurance**
(Sha, 1996). A fast, high-performance controller may do anything; a slow, *proven* safety
controller can always take over. The pilot is Simplex with a model as the performance
controller and a **distilled, verified reflex** as the muscle memory.

- **Stresses:** the skill spectrum (a reflex must run with no model call) and
  accountability.
- **Seeded by:** Tier-0 pathway reuse (a reflex is a pre-verified skill), the journal and
  the deed ledger, and the recorded, proof-backed denial. "It took the safest action" is the
  verifier proving the action in bounds, plus the rationale store holding *why*.
- **New hard problem:** envelopes that depend on live sensor state, and verification
  *ahead of the tick*. A solver call per frame is too slow, so the proof must be compiled in
  advance: the batch move, "prove once, run N", applied to a control loop.

### 2. The 500-model household (daisugi incarnate)

A home where hundreds of narrow, cheap models each tend one thing: watering the mint,
arming the alarm, balancing the calendar, working the front door and the refrigerator. Each
is a shoot in perfect form for its role, and all of them serve the household.

This is the metaphor made literal: trees cut and shaped to the exact form of one purpose.

- **Stresses:** routing at fleet scale, and authority per role.
- **Seeded by:** the model ladder, swarm deconfliction, `intersect_permissions`, and
  constraint promotion ("don't touch tenant X"). The home is a multi-tenant envelope
  problem: the mint-waterer can never reach the alarm; the door model can never touch the
  fridge. Each role gets a wall, and the household promotes the constraints.
- **New hard problem:** fleet orchestration and contracts between agents. The calendar
  model negotiates with the fridge, and each agent's output is verified at the boundary
  before it becomes another agent's input.

### 3. Self-building, self-healing software

Complex software built from scratch by small teams: expert agents on each piece of the
codebase, summarizing up and taking direction down a chain to the humans, who act as
consumer, orchestrator and invested chairman at once. The shipped work travels with a
small, honed expert model and its helpers, which patch and adapt the software in real
time.

- **Stresses:** decomposition and delegation, and shared rationale.
- **Seeded by:** `AgenticStep` and `AgenticExecutor` (sub-agents inside the *parent's*
  envelope: the authority ceiling is the chain of command), the proposed delegation tree (each
  parent-to-child edge proved by subsumption), the rationale store (the summary to the boss
  is rebuilt from strata), and verified deterministic cells that fall back to a model when
  the environment drifts. The self-healing patcher is `adapt_plan`.
- **New hard problem:** self-modification under an envelope. Can an agent rewrite code and
  *prove again* that it stays in policy? openDaisugi verifies and records; it does not own
  the workspace ([ADR-0020](adr/0020-layer-floor-loop.md)), so the rewriting runs through
  the harness, not through daisugi.

## The thread that joins the scales: one kernel, many dialects

For a long time the project was pictured two ways that did not seem to meet. One picture
was safe "muscle memory" for robot swarms, at the level of one robot and of the whole
population. The other was a few agents running a software startup. On 2026-09-26 the owner
saw how they join. Both get the same guarantees, because a proof is universal. What differs
between them is the **vocabulary**.

- **One kernel.** The permission record and the predicate algebra stay small and fixed.
  The kernel is where the guarantees live. It is the physics.
- **Many dialects.** Each domain gets a vocabulary of named words, each defined only in
  kernel terms. A software team says `safe_git_op` and `no_prod_write`. A swarm says
  `keep_separation(d)`, `within_geofence(zone)` and `grip_force_below(n)`. A household says
  `must_not_modify(tenant_x)`. A word unfolds to the kernel, so every question about a word
  is a question about the kernel, no harder in kind. The dialect carries the domain's
  assumptions, the way a good DSL does, so a reader sees what it can and cannot say.
- **The bots write the policy.** Models write and grow the dialect; humans read it; Z3
  checks the unfolded kernel. No one learns a DSL by hand. The garden mines the journal for
  repeated sub-predicates, a model names them in plain words, and Z3 proves two words equal
  so the vocabulary does not grow duplicates. This is library learning applied to the
  policy language.
- **Muscle memory at two levels.** A pathway written in the dialect is one agent's, or one
  robot's, learned skill. A word that proves useful across the population joins the shared
  dialect. **The dialect is the population's culture; the kernel is its physics.**
- **Dialects meet at the kernel.** A word from one dialect can be checked against a word
  from another, because both unfold to the same kernel. Two teams, or a drone team and a
  warehouse team, can prove their policies agree.

So the three dreams are one kernel with three dialects: the pilot's reflex words, the
household's role and tenant words, the software team's repository words. A dialect is also
what weave was reaching for: a graph whose small-model steps write in a constrained
grammar, each step verified. The word is the shoot; the kernel is the stump.

What is not new: fixed kernels, macros over them, and library learning are known ideas.
What may be new: learned, proved, per-domain dialects for the policy that governs agent
actions, with one kernel from a software team to a robot swarm.

**The honest limits.**
- The kernel must say enough for each domain. Today it does not say enough for a swarm:
  `keep_separation`, a geofence word on a waypoint and a grip-force bound all need kernel
  changes first ([design-dialects.md](plans/2026-09-24-omarchy/design-dialects.md), §3.4).
  The software team's dialect can start now.
- The kernel is not decidable everywhere. Robot dynamics bring nonlinear real arithmetic,
  and Z3's string and regex checks can also answer "unknown". The gate then fails closed:
  an unknown is a deny, never an allow.
- A word is only as trustworthy as its definition, which Z3 checks, and its name, which a
  model chose. A misleading name is a real risk, so names are checked against their
  definitions, and the checker never trusts a name.

**The first evidence.** A greedy pass over the permission sets of 294 real envelopes from
the owner's journal named 15 words and cut the size by 17.5%. The words read like a
dialect: read-only inspection, build and test, code forges, config files. It is a small,
in-sample test over flat sets. It says the dialects are there in the data; it does not yet
prove they generalize.

## The base every dream shares

At the base, all three dreams need the same three capabilities, and a fourth that joins
them. Each maps to a part of the system:

| The capability | Where it lives | Where it stands |
|---|---|---|
| Strong token saving and model routing | the model ladder (Tier-0 reuse, local, cheap, frontier), the gateway, the per-step sizer, and the designed router and grafts | routing works in Python, Go and Rust; a router that learns from verified results is designed, not built |
| Task recognition, decomposition and execution | the typed `ActionPlan`, the orchestrator, verify per step | working and tested in Python |
| Distillation into repeatable, safe, fast skills | distillation, verified pathways, Tier-0 reuse, the garden | the mechanism is real in all three languages; its *value* (roadmap stage 4) is the unmeasured linchpin |
| A shared language for policy | the kernel, definitions today, dialects next | definitions built; dialects designed |

The third row is the through-line for *actions*: a reflex, a mint-waterer and a
self-healing patch are the same object, a distilled skill, at three points on one
spectrum, from a `cron` line running `bash` to a bespoke model analysis. The fourth row is
the through-line for *policy*: the same kernel says what each of them may do, in the words
of its own domain.

## Near ground and far horizon

The [roadmap](roadmap.md) is the near ground: the floor people love, full parity across
three languages, the safety gaps, token saving that holds up on a bill, dialects, ranking
and the delegation tree. Nothing here asks to abandon it. The linchpin is still proof that
distilled skills pay and stay safe, because every dream rests on it.

The dreams add far-horizon problems: envelopes that depend on sensor state and proofs ahead
of the tick (the pilot); fleet orchestration and contracts between agents (the household);
self-modification under proof (the software); and a kernel strong enough for each domain's
dialect (all three). They stay horizons until the near ground can hold them.

The through-line is the same as the one idea, turned on the project itself: do not ask
anyone to trust the dream. Build toward it in pieces, each of which you can check.

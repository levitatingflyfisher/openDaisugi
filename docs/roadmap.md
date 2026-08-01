# Roadmap

> Problems, not promises. For this project, describing a capability precisely
> enough to schedule it is most of the work of building it — so a dated feature
> list would self-destruct on contact ([VISION § Horizons](../VISION.md#horizons-problems-not-a-feature-list)).
> Instead, each stage below is a **problem the library cannot yet solve**, paired
> with the **evidence that would prove it solved**. When the evidence exists, the
> stage is done; until then, it isn't — no matter what the commit log says.
>
> Stages are ordered by *dependency*, not importance. 1 → 2 → 3 is a spine; 4
> deliberately waits on it; 5–7 run alongside, bounded. Stages 8–10 are a **second
> axis** — the cost levers opened by [ADR-0011](adr/0011-verifiable-execution-substrate.md):
> 8 (the deed ledger) and 9 (within-instance batch compilation) are **done** — 9's
> mechanism and meter ship in v0.42, with only its *at-scale cross-instance* numbers
> still sharing 4's local-model dependency; 10 (the rationale ledger), the far novel
> bet, now has its mechanism too — the store, reconstruction, and constraint-promotion
> ship in v0.43, with only its re-derivation *numbers* deferred to a model. All three
> cost levers are now built to the mechanism line. The far-horizon problems (perception-conditioned
> envelopes, robotics on hardware) stay in [VISION](../VISION.md) — this document is
> the near ground.

## Where the line is today

The plan-verification spine is real: envelope → `verify` → supervise → journal →
distill, Z3-backed, fail-closed, ~1600 tests, CI green. What it verifies is
**plans it is handed** — typed, declared, submitted in advance.

As of v0.35.0 the call-time gate exists beside it: live tool calls are
verified against a registered per-session envelope, deny-by-default, shadow
mode first (see Stage 1's status note for the evidence). The passive capture
hook is unchanged — it journals and fails **open** by design, correct for
observation. What the library still cannot do is hand a *sub-agent* real
tools inside the parent's envelope (Stage 2), or back its fail-closed claim
with evidence someone else authored (Stage 3). Closing those gaps — without
ever pretending they are closed before the evidence exists — orders
everything below.

---

## Stage 1 — The gate problem
*Fail-closed enforcement at the live tool-call boundary.*

**The problem.** The library cannot yet take a live tool call — a shell command,
a file read, an MCP invocation — synthesize it into a one-step plan, prove it
inside the session's envelope *before it runs*, and deny by default when the
proof fails. Turning the passive hook into that gate is not a flag flip; it
inverts the hook's founding contract, and several sub-problems are honestly
unsolved:

- **The host's deny contract is unverified.** Exit codes vs. structured
  decisions, and whether hook configuration can be injected into non-interactive
  runs, must be established empirically against the real installed CLI — not
  assumed from documentation.
- **Fail-closed lives or dies in the timeout.** Host-side hook timeouts fail
  open on every harness we know of. The gate must own an *inner* timeout that
  itself denies, so the host's never fires.
- **The matcher must be total.** "Tool not matched ⇒ never evaluated ⇒ allowed"
  would silently defeat deny-by-default. Every call reaches the gate;
  classification happens inside it.
- **Sessions have no envelope channel.** Nothing today registers an envelope
  *per session* for a gate to check against. That channel has to exist first.
- **The host session's envelope has to come from somewhere.** For sub-agents
  the library spawns, the parent authors the envelope before the spawn. For an
  operator's *existing* session there is no author yet — a shipped default
  template, an interactive authoring step, or a drafted-then-reviewed envelope
  are the candidates, and until one of them exists, "protect your existing
  session" honestly means shadow mode.
- **Latency must be measured at the right boundary.** Solver time per check is
  small once a process is warm, but the number that matters is the full hook
  round trip, interpreter startup included. If it disappoints, a resident
  process is the fallback — measured, not hoped.

**Shadow first, and honestly labeled.** The gate ships observing by default: it
evaluates every call and reports what it *would* have denied; one flag flips it
to enforce. This is how an operator tunes the gate against a running fleet
before trusting it — and how compound-command false positives get found before
they get denied. **Shadow mode is observation, not protection.** The docs will
never blur that line.

**Solved when:**
- A committed test demonstrates a real tool call, in the real host CLI, denied
  by the gate — not a simulation of one.
- Every failure path — unknown tool, unparseable input, internal exception,
  slow verifier — resolves to DENY, each covered by a test.
- Verification strictness is taken from the envelope's stakes, never silently
  relaxed at the gate.
- Measured p95 full-round-trip latency is published, whatever it turns out to be.
- A shadow-mode report can be generated from a captured real session,
  false-positive candidates included.
- A bricked or over-denying gate can be disabled with one command that does not
  itself require an allowed tool call — the operator's exit is as engineered as
  the deny path.
- The existing suite is green with the passive-era "always allow" expectations
  *rewritten*, not deleted.

**Status (v0.35.0):** the criteria above have committed evidence —
`opendaisugi.gate` + `tests/test_gate.py` (every failure path denies),
`tests/test_hook_gate_contract.py` (the real gate denying a real Read in the
real host CLI, live-verified), `daisugi gate report` / `replay` (shadow
report with false-positive candidates), `daisugi gate disarm`, and the
measured round trip published in [docs/how-to/gate.md](how-to/gate.md). One
named sub-problem deliberately remains open: the **host-session envelope
authoring story** (shipped default template / interactive authoring /
drafted-then-reviewed). Until it lands with Stage 6, "protect your existing
session" honestly means: bring your own envelope, or run shadow.

## Stage 2 — The delegation problem
*Sub-agents that can act — inside the envelope.*

**The problem.** Delegated sub-agents today are pure-reasoning leaves: they run
in an empty directory, see no project files, touch no tools. That is safe
precisely because it is inert. The library cannot yet hand a sub-agent a real
working directory and real tools while proving that every action it takes stays
inside the *parent's* envelope ([invariant 3](../VISION.md#the-invariants-do-not-break-these):
the envelope is the authorization ceiling — the caller's, never the callee's).

The design is defense in depth: a static outer wall (the sub-agent's tool
allowlist *derived from* the envelope) plus a dynamic inner one (the Stage-1
gate wired into the sub-agent's own hook configuration, proving each call as it
happens). Neither alone is the enforcement story.

**Solved when:**
- The new step type is registered in the verifier's known-type set with a real
  permission-checking arm — it does not ride the pure-reasoning exemption. No
  silent pass.
- The allowlist handed to the sub-agent is computed from the envelope, and the
  hook settings are supplied from *outside* anything the sub-agent can write.
- A failed sub-agent surfaces as a failed step, never a swallowed one; the full
  transcript lands in the journal, where distillation can reach it.
- Every adversarial escape in Stage 3 is denied when attempted *from inside* a
  delegated sub-agent.

**Status (v0.36.0):** the mechanism is built and the first three criteria
have committed evidence — `AgenticStep` with a real permission arm in
`verify.py` (`tests/test_agentic_step.py`), `AgenticExecutor` computing the
tool wall from the envelope and registering the gate in a root outside the
workspace (`tests/test_agentic_executor.py`), failed sub-agents surfacing as
failed steps, and gated calls mirrored into the captures pipeline for
distillation. A live opt-in test (`tests/test_agentic_live.py`) shows a real
sub-agent's out-of-envelope read denied by the inner-wall gate, with a
benign in-workspace read succeeding as the causality control. The fourth
criterion — *every* Stage-3 adversarial escape denied from inside a
sub-agent — waits on Stage 3's corpus, by design.

## Stage 3 — The evidence problem
*A safety claim someone else can check.*

**The problem.** "Fail-closed" is currently an assertion backed by tests we
wrote against attacks we imagined. Necessary, and insufficient: a gate examined
only by its author has been examined by nobody. And the formal account is
incomplete — the [yellow paper](spec/yellow-paper.md) proves properties of
*plan-time* verification and says nothing yet about what a *call-time* gate can
and cannot guarantee.

Two deliverables, fused, because each keeps the other honest:

**The adversarial suite becomes the merge gate.** Nothing about the gate merges
until it holds against: reading credentials it was never granted, out-of-pattern
and compound shell commands, undeclared MCP tools, a sub-agent rewriting its own
hook configuration mid-session, and slow-verifier bypass attempts. The corpus is
not solely self-authored — it adapts published injection-attack suites, with
each case's provenance and adaptation recorded, and it runs in two layers that
are never conflated: a **deterministic replay layer** (recorded call/envelope
pairs through the gate offline — exactly reproducible, so any attack miss is by
definition a bug, which is what licenses "suite = merge gate") and a **live
layer** (genuinely injected sub-agents under the real hook, where
whether-the-model-takes-the-bait is stochastic and reported with confidence
intervals, separately from whether-the-gate-denies-it). Both error directions
are measured: a gate that denies everything scores a perfect attack-denial rate
and is useless, so the benign-task false-positive rate is a first-class
published number, with denied-call transcripts published verbatim so readers
can adjudicate for themselves. The same corpus runs through the host's built-in
permission patterns alone, and through no gate at all, so the claim is
comparative, not absolute. The harness is seeded and content-addressed, so a
rerun is a rerun — and its benign task suite is the same corpus Stage 4 will
reuse, built once.

**The two-checkpoint section of the yellow paper.** Verification at plan time,
over a declared plan, can establish structural and liveness-class properties.
Gating at call time, over an opaque agent, can enforce safety properties only —
*a call being inside the envelope does not make the trajectory benign*. That
split — anchored to the enforceable-policies literature and the Simplex
runtime-assurance lineage this project descends from — ships in the spec,
together with a stated threat model (what the attacker is assumed to control,
and what is explicitly out of scope) and the fail-open edges that remain outside
our control (a host-level outer timeout; a harness that silently stops firing
hooks). Those go in the public scorecard, not a footnote.

**Solved when:** the suite runs green as a required check on every merge; the
attack-denial and false-positive rates are published, per attack category, with
compound shell commands broken out; the yellow-paper section exists with its
threat model and non-guarantees stated plainly; and one recorded demonstration
exists of a live injection attempt being denied, with its proof-backed reason
on screen.

**Status (v0.37.0):** the deterministic layer is committed and green as a merge
check — `src/opendaisugi/adversarial.py` (13 attacks across 7 categories, 9
benign, content-addressed, per-attack provenance) and `tests/test_adversarial.py`
(every attack denied ⇒ the merge-gate invariant; unexpected false positives
break the build). `daisugi gate audit` publishes both rates and the comparison
arms (no-gate 0.00 / literal-glob 0.46 / gate 1.00 attack-denial; the gate's
0.33 FP rate is entirely the *known*, budgeted false positives — compound
commands and unmapped tools — reported, not hidden). The yellow paper's
[two-checkpoint section (§8)](spec/yellow-paper.md) states the safety-only
guarantee, the enforceability-class limits, the in-envelope-≠-benign gap with a
worked example, the threat model, and the fail-open edges. The recorded live
denial is [`examples/injection-denied/`](../examples/injection-denied/). The
**live layer's** stochastic bait-taking rate — reported with confidence
intervals, separately from whether-the-gate-denies — is the remaining piece;
the live *deny* behavior is pinned by `tests/test_agentic_live.py` and
`tests/test_hook_gate_contract.py`, but the model-takes-the-bait frequency is
not yet run at N for intervals.

## Stage 4 — The distillation-fidelity problem
*Does distillation actually pay? Measured, not asserted.*

**The problem.** The distillation machinery — journaled runs compiled into
signed reusable pathways — is real and tested. Its *value* has never been
measured, and this is the oldest honest gap in the
[scorecard](../VISION.md#honest-scorecard--built-vs-aspirational). This stage
**deliberately waits** on Stages 1–3: benchmarking distillation on toy
transcripts before real tool-using agentic runs exist would measure nothing.
The gate and delegated sub-agents are what produce transcripts worth
distilling; only then does the question have data.

**Solved when:** seeded, content-addressed paired runs — with and without
distilled pathways — over at least twenty repeated real tasks and at least five
seeded repeats each, for at least one local model; token, latency, and outcome
deltas published with confidence intervals in the honest scorecard, whether or
not they flatter; and the safety direction checked too — pathway-warm runs must
not increase denial or violation attempts relative to cold ones.

**Status (v0.39.0):** the *ruler* is built and tested — `opendaisugi.benchmark`
provides seeded, content-addressed paired runs (cold/warm share a seed), t-based
95% confidence intervals on the token/latency deltas, outcome rates, the
safety-direction check (warm must not attempt more denials/violations than
cold), and `meets_stage4_bar` (≥20 tasks × ≥5 repeats) so a thin sample can't be
published as settled. What is **still open — by the stage's own design** — is
running it: execution is an injected runner, and the real numbers wait on a
local-model-backed runner and real tool-using transcripts (which Stages 1–2 now
produce). The harness is verified with a deterministic fake runner; no numbers
are claimed until a real model is wired in.

**First real pilot (2026-07-25, `qwen3:4b-instruct`, 3×3 — see
[examples/distillation-benchmark/RESULTS.md](../examples/distillation-benchmark/)):**
the runner is wired and the question has run on real agentic transcripts for the
first time. Direction favors distillation — warm is cheaper in tokens (−632) and
latency (−2.6 s), consistent with skipping the decompose — but the 95% CIs span
zero (a 3×3 pilot is underpowered by design), reuse fires only 33%, and warm
success (56%) is *below* cold (67%). The limiting factor is small-model
*execution* reliability (~50–67% run success), not the reuse code — a pathway
only forms once a run succeeds. **Honest conclusion at this scale:**
distillation's savings are real in direction but unproven, and dominated by
model noise; the effort is not clearly justified with a 4B model. The full ≥20×5
is deliberately deferred until a more reliable (larger) local model clears
~90% run success — running it with this model would be a bigger, equally
noise-dominated pilot. The stage is *not* solved; the pilot is the honest
progress, not a claimed number.

**Reliable-executor run (2026-07-26, `claude-code` backend, 4×3 — see
[RESULTS-reliable-executor.md](../examples/distillation-benchmark/RESULTS-reliable-executor.md)):**
the pilot's "obvious next experiment" — rerun with a reliable executor so the
reuse effect is isolated from execution noise. Result: cold and warm success are
**both 100%** with no safety regression, warm ~8 s faster (CI still spans zero at
n=12), and the harness reported reuse firing on all 12 warm runs. The finding:
the pilot's *warm-below-cold* was 4B execution noise, **not** a reuse defect —
the feared "reused pathway drops a step" did not appear at this scale (a
clamp-fixed 3×1 follow-up then reused every task cleanly, `find-todos` included —
see the linked results). Two things this does *not*
settle, stated plainly: it is 4×3 (does **not** meet the ≥20×5 bar), and
`claude-code` is reliable but **not local** — so the *cheap-local-reuse* product
pitch still needs a local model clearing ~90% success. (The run also surfaced
and fixed a real cosine-overshoot bug in `pathway_store.find()`.) Separately, the
guard-cost and compilable-fraction halves of the empirical thesis now have their
own measured ruler — [`examples/jit-metrics/`](../examples/jit-metrics/).

## Stage 5 — The harness problem
*Honesty about where enforcement is possible.*

**The problem.** Hook semantics differ across hosts, and some hosts' hook
layers cannot hard-block a call today — their timeouts fail open, or their
blocking path may be unreliable at a given pinned version. The library cannot
yet tell an operator, per harness and per version, which of three things they
are getting: **hard enforcement**, **soft enforcement**, or **observation**.
Claiming the first while delivering the third would be the fail-open we exist
to prevent — committed at the level of documentation.

**Solved when:** per-harness contract tests run against pinned host versions;
findings are published even when negative ("this host's block path does not
block; here is the reproduction"); each integration doc states its enforcement
class in the first paragraph; and the passive journaling path remains supported
for hosts where observation is all that is honestly available.

**Status (v0.38.0):** the enforcement-class table leads
[integrations.md](integrations.md) (Claude Code = hard/contract-tested; Hermes
and OpenClaw = unverified, treat as observation), and the passive path stays
first-class. The committed contract test is Claude-Code-only
(`tests/test_hook_gate_contract.py`, pinned to the installed CLI version); a
Hermes/OpenClaw live contract test — with its findings published even if
negative — is the remaining piece, gated on having those hosts to pin against.

## Stage 6 — The onboarding problem
*Time-to-first-verified-plan.*

**The problem.** The distance between "heard of it" and "watched it deny
something on my machine" is too long. The operator most likely to need a
runtime-assurance layer reaches for it in a moment of alarm — an agent just did
something it shouldn't have — and that moment must meet one command, not an
afternoon of configuration. Runnable examples exist; a funnel does not.

**Solved when:** on a fresh machine, one command ends in a working shadow-mode
gate over an existing session in under five minutes (which depends on the
host-session envelope story named in Stage 1); a "protect your existing
session" tutorial exists in `docs/tutorials/` (closing the gap the
[docs hub](README.md#tutorials) already names); the README leads with the
recorded denial from Stage 3; and time-to-first-verified-plan is measured from
a clean environment and published as a number we defend.

**Status (v0.38.0):** `daisugi gate quickstart` is the one command — it
generates and registers a reviewable starter envelope (resolving Stage 1's open
host-session-envelope sub-problem via the drafted-then-reviewed candidate:
`starter_envelope` / `gate init`) and prints the whole shadow → report →
enforce → disarm flow. The tutorial
[protect-your-existing-session.md](tutorials/protect-your-existing-session.md)
exists and is linked from the docs hub; the README leads with the gate
quickstart and the recorded denial. The remaining piece is the *measured*
time-to-first — clocked on a clean machine and published as a defended number
(the mechanism is in place; the measurement is not yet run).

## Stage 7 — The trust problem
*Why a stranger should run this.*

**The problem.** A security layer asks for more trust than any other
dependency: it sits between an agent and everything the agent touches. Today
that trust rests on reading the source. That is a real option — it is why the
code is open — but it cannot be the only one.

**Solved when:** CI is public and green on every push, with the Stage-3
adversarial suite as a required check; release artifacts are signed (distilled
pathways already are — releases must meet the same bar); the evaluation and
benchmark harnesses are content-addressed and re-runnable by someone who isn't
us, with matching results; and the supply-chain posture — pinned dependencies,
allowlist-based model resolution, no telemetry of any kind — is documented in
one place a skeptic can audit in an afternoon.

**Status (v0.39.0):** four of five are met, and the fifth now has shipped
machinery. Met: public CI green on every push with the adversarial suite as an
explicit required step; the corpus is content-addressed and re-runnable by
anyone (`daisugi gate audit`); the supply-chain posture is documented in one
place ([security-model.md § supply-chain](security-model.md)); pathway bundles
are signed. The fifth — **release-artifact signing** — now has the machinery:
`daisugi release sign`/`verify` produce and check a signed SHA-256 manifest over
the artifacts, reusing the one ed25519 trust root (`opendaisugi.release`,
tested). It is *met* only once each published release actually ships a signed
manifest; until a given release does, install from a pinned git ref you have
read, not an unpinned index.

---

## The currency levers (Stages 8–10)

Stages 1–7 are the enforcement spine. Stages 8–10 are a second axis, opened by
[ADR-0011](adr/0011-verifiable-execution-substrate.md): openDaisugi is a
**verifiable-execution substrate** whose gate does not itself save tokens but makes
the token-savers safe. Each lever below is gated on the substrate and reports its own
currency honestly. They were reached independently in a
[blind-design convergence experiment](exploration/2026-08-blind-design-gauntlet/) —
five closed-book architects rebuilt this exact split — which is why they earn a place
on the roadmap rather than in the "not building" list.

One lever needs no new stage: **verification-underwritten cheap-model routing**
(escalate to a strong model on *external* signals — gate rejection, postcondition
failure, a k-failure stop-loss — never the cheap model's self-report) is an extension
of the existing model ladder, and lands with Stage 9's batch machinery.

## Stage 8 — The reversibility problem
*A wrong-but-allowed action should cost a rollback, not a recovery arc.*

**The problem.** An action can be provably in-envelope and still wrong for the task
(a correctly-scoped deletion of the wrong file). Today that costs a full recovery
arc — the agent reasons its way back from a bad state, burning output tokens. Every
one of the five blind designs answered this with copy-on-write rollback; openDaisugi
has none. But *owning* workspace snapshots is state management, which
[ADR-0004](adr/0004-layer-not-harness.md) forecloses — that is the harness's job. The
in-scope form is an **external deed ledger**: the layer *records* each reversible deed,
the *harness* reverts. The Supervisor already appends a per-step `Receipt`
(`_write_step_receipt`), but the receipt carries execution evidence, not a reversal
handle, and nothing consumes it to undo.

**Solved when:**
- Each executed side-effecting step appends a deed receipt carrying its effect class
  and, *where the effect is reversible*, a harness-consumable reversal handle; a
  committed test shows a harness rolling back a wrong-but-allowed step **from the
  ledger alone, with no model call**.
- Irreversible effect classes (send, purchase, external POST) are marked
  non-reversible and never claim a handle they do not have — the honest boundary,
  tested.
- The ledger is queryable to reconstruct the pre-state of the files a run touched
  (those with a captured reversal handle), so a compacted agent rehydrates from
  receipts rather than replayed history.

**Status (v0.41): solved.** The deed ledger ships with committed evidence. Each
executed step's `Receipt` now carries an `effect_class`, a `reversibility` verdict
(`none` / `reversible` / `irreversible`), and, when reversible, a `ReversalHandle`
(`src/opendaisugi/models.py`); the `FileWriteExecutor` captures the target's
pre-image before it mutates (and records any directories it creates), emitting the
verdict on every path (`src/opendaisugi/executor.py`). `deeds.rollback_run` undoes a
run's reversible deeds **from the ledger alone — no model, no executor, no re-run** —
and `deeds.touched_files` folds the ledger into the pre-state view
(`src/opendaisugi/deeds.py`). `tests/test_deed_ledger.py` proves it end to end,
including the honest boundary: a refused write claims `none` (never a false handle), an
oversized or non-UTF-8 prior image is marked `irreversible` rather than truncated, and
`reversibility` defaults to `irreversible` — never a silent `none` — for any
side-effecting step lacking a handle. The one thing deliberately out of scope: this
covers the Supervisor's *executed* steps; a deed ledger for the call-time **gate**
path (where the harness, not openDaisugi, performs the effect) would need the harness
to report the reversal handle, and rides with a later gate-side extension.

## Stage 9 — The within-instance-compilation problem, honestly baselined
*Collapse a single task's internal repetition into one proven program — and measure
it against the right baseline.*

**The problem.** A task's own internal repetition is paid for turn by turn. The
library cannot yet let the agent **declare a batch** — program P, item set I, effect
footprint F, acceptance postcondition Q — prove F ⊆ envelope *before any iteration*
(one proof covers all N), sample-validate Q on k=2–3 items in a copy-on-write fork,
then execute all N under a monitor that kills on first envelope exit, with per-element
rollback. (Agent-*declares*, deliberately not an anti-unification loop detector — the
gauntlet correctly flagged detection as a research problem dressed as a milestone.)

**The two-ledger baseline — the load-bearing honesty.** The savings must be reported in
two separate ledgers, never merged:

- **Within-instance ledger** — the win is a *verified* script instead of an
  *unverified* one. Frequency-independent, small, safety-shaped. This is the win that
  survives the [hardest requirement](exploration/2026-08-blind-design-gauntlet/problem-statement.md):
  a competent agent already scripts a bulk job, so the marginal contribution is the
  proven blast radius, not the script.
- **Cross-instance ledger** — persistence and generalization: the distilled,
  parameterized pathway is saved off and reused on similar-not-identical tasks. This
  is a real, *compounding* win — but it is the frequency-amortized family the hardest
  requirement excludes as a *complete* answer, so it is reported **apart** and never
  folded into the within-instance number. Whether it pays *at scale* is Stage 4's
  question, not this stage's.

**Solved when:** a batch-declaration API with static write-set proof (F ⊆ envelope,
reject on any unprovable write); irreversible tools marked non-batchable so they can
never enter a batch; a per-instance **net-token meter** — (output + calls saved) −
(spec input injected) < 0, the trap SKILL-DISCO measured, dual-currency and labeled
evidence not proof; and the two ledgers published separately, on internally-repetitive
one-offs, against the honest (not manual-turn) baseline.

**Status (v0.42): mechanism and meter shipped; at-scale cross-instance numbers
deferred.** The batch machinery ships with committed evidence in
[`src/opendaisugi/batch.py`](../src/opendaisugi/batch.py) and
[`tests/test_batch.py`](../tests/test_batch.py). A batch is a JSON-round-trippable
`BatchDeclaration` (program template + `PathwayParameter` holes + a *concrete* item
list + declared footprint F + acceptance Q) an agent authors like an envelope, provable
from the shell with `daisugi batch prove`. `prove_footprint` resolves every item up
front and proves each write is inside both the envelope and F using the **same concrete
matcher the runtime gate uses** (`verify._path_matches_any`), *not* the envelope↔envelope
Z3 glob encoding — which diverges from it and would let the proof admit-then-reject (the
divergence is itself now a pinned known-gap tripwire,
[`tests/test_subsumption_glob_known_gap.py`](../tests/test_subsumption_glob_known_gap.py)).
Irreversible programs can never enter a batch: a static kind check (only reversible
`file_write` and read-only `file_read`/GET-only `network`), a pre-flight reversibility
probe that rejects a target whose write would be un-undoable, **and** a runtime rule that
halts the instant a deed comes back `irreversible` — reversibility is not a property of
the type, so the type check alone is insufficient. `run_batch` sample-validates Q on k
items in a deed-ledger fork (`apply_reversal`, no CoW — ADR-0004 forecloses it), then
executes all N under the supervisor's per-step monitor with per-element rollback from the
ledger alone. The per-instance **net-token meter** (`NetTokenLedger`) computes
`(output + calls saved) − (spec input injected)` and surfaces the SKILL-DISCO trap when
net goes negative — labelled evidence, not proof; the **two ledgers** (`TwoLedgerReport`)
are kept separate and never merged. What is **deliberately deferred — by the stage's own
design** — is *publishing the cross-instance ledger's numbers at scale*: that is Stage
4's question and shares its local-model dependency. Within-instance, the meter already
reports the honest finding — the win is the proven blast radius, not tokens; the net
number is ≤ 0. The net-token ruler's other half has a measured primitive in
[`examples/jit-metrics/`](../examples/jit-metrics/).

## Stage 10 — The rationale-durability problem
*Never re-derive reasoning that compaction dropped.*

**The problem.** On an irreducible one-off — a heisenbug hunt, a novel design — there
is no internal repetition to compile and no prior corpus to reuse. This is plausibly
the *modal* rare-hard task, and every lever above is inert on it. What compaction drops
there is the **deliberation**: discovered facts, ruled-out hypotheses and why, mid-task
invariants. openDaisugi externalizes enforcement state (the envelope, the deed ledger)
but not reasoning, so a compacted agent re-explores dead branches and re-discovers
facts it already had. **None of the five blind designs built this** — it is the
deliberate new bet.

The library cannot yet hold a typed store — facts-with-provenance, ruled-out
hypotheses, open constraints, the goal/subgoal stack — reconstruct each model call's
context *from the store* instead of the transcript, and promote an expressible mid-task
user invariant ("don't touch tenant X") into the enforcement envelope. The guardrail
is strict: facts and hypotheses inform reasoning but **never gate actions**; only
constraint-promotion touches authority, and it flows through the same
`envelope_subsumes` monotone-narrowing check a captured invariant may only tighten.

**Solved when:** a typed strata store with a cheap structured-emission hook; context
reconstruction from relevant strata + pinned constraints, with a verbatim re-page API
for dropped detail; the "don't touch tenant X" scenario held end-to-end under *forced*
compaction via a promoted invariant; and a re-derivation meter — from an identical
compaction point on one instance, re-explored branches and re-discovered facts with the
store on versus off — publishing the output-token delta.

**Status (v0.43): mechanism shipped; re-derivation numbers deferred.** The typed strata
store ships with committed evidence in [`src/opendaisugi/strata.py`](../src/opendaisugi/strata.py)
and [`tests/test_strata.py`](../tests/test_strata.py). `StrataStore.emit` is the cheap
structured-emission hook over four kinds (`fact` / `hypothesis` / `constraint` / `goal`,
each with provenance and a monotonic `seq`); `reconstruct_context` rebuilds a model call's
context *from the store* — pinned strata and open constraints always retained, the rest
filled by tag/recency under a budget, ruled-out hypotheses kept so a branch is not
re-explored, and `repage` returns any dropped stratum verbatim. The honest boundary is
stated where it lives: **reconstruction is lossy — a dropped fact is one the agent will
re-derive** — and the relevance selector is deliberately simple, the sophisticated version
left to the harness (this stage's own landmine). Constraint-promotion is the *only* path
from deliberation to authority: `promote_constraint` refuses any stratum that is not a
`constraint` (facts / hypotheses / goals inform reasoning, never gate actions — a tested
tripwire), and gates every promotion through `verify_inheritance` on four fail-closed
checks — kind, only-tightens, *actually*-tightens (a no-op is refused), and *actually*-
enforces (an optional `deny_witness` the tightened envelope must reject, so a constraint
that compiled to a soft/unenforced node is refused rather than accepted). The **"don't
touch tenant X" scenario is held end-to-end under forced compaction**: the pinned
constraint survives a `to_json`/`from_json` round-trip and a punishing reconstruction
budget, and the promoted envelope still makes `verify()` deny the tenant-X write while
allowing tenant Y (`tests/test_strata.py::test_tenant_x_held_end_to_end_under_forced_compaction`).
The `RederivationLedger` (store-on vs store-off output-token delta) ships as the meter;
its real, at-scale numbers are **deliberately deferred** to a model, exactly as Stages 4
and 9 defer theirs. Like the deed ledger (ADR-0004), this is a store the harness consumes —
openDaisugi records and reconstructs; it does not rewrite its own prompts.

---

## What we are deliberately not building

An honest roadmap also names the closed doors, and what would reopen them.

- **Fine-tuning on distilled pathways.** There is no corpus worth training on
  until Stage 4 produces one and the numbers justify it. *Reopens when:* a
  measured corpus exists.
- **An HTTP-daemon variant of the hook.** It fails open on connection failure —
  structurally wrong for a fail-closed product. *Reopens when:* redesigned so
  that connection failure denies; there is no schedule for this, because it is
  a design problem, not a scheduling one.
- **Deep integration with harnesses that cannot block.** Until a host's
  blocking semantics verifiably work, integration effort stops at the contract
  test and the published finding (Stage 5). *Reopens when:* the contract test
  passes.
- **More demo recordings.** A gallery of scenarios already exists; past a
  point, demos substitute for evidence and the credibility risk runs the other
  way. Stage 3's single recorded denial is the exception, because it *is*
  evidence.

The through-line: every stage ends in something checkable — a test against a
real host, a published rate, a spec section with its non-guarantees stated, a
number in the scorecard. That is the same move as the
[one idea](../VISION.md#the-one-idea), applied to the project itself: don't ask
anyone to trust the process; hand them an artifact they can verify.

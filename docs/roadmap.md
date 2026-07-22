# Roadmap

> Problems, not promises. For this project, describing a capability precisely
> enough to schedule it is most of the work of building it — so a dated feature
> list would self-destruct on contact ([VISION § Horizons](../VISION.md#horizons-problems-not-a-feature-list)).
> Instead, each stage below is a **problem the library cannot yet solve**, paired
> with the **evidence that would prove it solved**. When the evidence exists, the
> stage is done; until then, it isn't — no matter what the commit log says.
>
> Stages are ordered by *dependency*, not importance. 1 → 2 → 3 is a spine; 4
> deliberately waits on it; 5–7 run alongside, bounded. The far-horizon problems
> (perception-conditioned envelopes, robotics on hardware) stay in
> [VISION](../VISION.md) — this document is the near ground.

## Where the line is today

The plan-verification spine is real: envelope → `verify` → supervise → journal →
distill, Z3-backed, fail-closed, ~1600 tests, CI green. What it verifies is
**plans it is handed** — typed, declared, submitted in advance.

As of v0.35.0 the call-time gate exists beside it: live tool calls are
verified against a registered per-session envelope, deny-by-default, shadow
mode first (see Stage 1's status note for the evidence). The passive capture
seam is unchanged — it journals and fails **open** by design, correct for
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
proof fails. Turning the passive seam into that gate is not a flag flip; it
inverts the seam's founding contract, and several sub-problems are honestly
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

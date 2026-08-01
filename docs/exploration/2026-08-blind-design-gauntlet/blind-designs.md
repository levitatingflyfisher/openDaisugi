# The five blind designs

*Five independent designer agents, each closed-book — no sight of openDaisugi, no
sight of each other — worked the [neutral problem statement](problem-statement.md)
under a different lens. These are their architectures, transcribed faithfully and
described in vendor-neutral terms. The striking fact is not any single design; it is
that all five arrived at the same spine. The convergence is tabulated in
[gauntlet-and-synthesis.md](gauntlet-and-synthesis.md).*

Every design independently landed on the same shape: **a deterministic gate on the
tool-call boundary, outside the context window, fail-closed, with a human floor a
model may only tighten, plus a way to collapse a single task's internal repetition
into one verified program.** They differ in emphasis and in one or two distinctive
moves each.

---

## 1. Countinghouse — *amortize inside the instance; keep the law outside the window*

**Lens:** cost-first.

A metered tool-proxy runtime. Every side-effecting action passes through a "teller's
window" — a deterministic proxy between the agent and the world — running four coupled
parts:

- **The Envelope** — a machine-checkable contract (workspace bounds, host allowlist,
  irreversibility classes, resource ceilings) in a small restricted predicate
  language, authored out-of-band before the task, compiled to deterministic checks
  plus solver queries, enforced pre-execution, fail-closed. Shape-triage keys the
  solver only for mutating actions; reads pass a microsecond allowlist.
- **The Deed Ledger** — every executed action emits a compact structured receipt into
  an external store, with raw observations kept addressable. The transcript is then
  compacted aggressively and safely, because neither ground-truth state nor policy
  lives in it.
- **The Within-Instance Compiler** — an anti-unification loop detector watches the
  ledger for isomorphic action sequences *within this one task*; at k=3 repeats it
  makes a single synthesis call producing one parameterized program for the remaining
  items, statically verifies its write-set against the Envelope, dry-run diff-tests it,
  then executes deterministically — O(n) turns into O(1).
- **Checkpointed execution** — all mutations run copy-on-write, with a staged outbox
  holding irreversible external effects until an irreversibility check or human ack.

**Saves:** both. Input via ledger-backed safe compaction (40–70% prefill on long
tasks) and a cache-stable prefix. Output via within-instance compilation (~97% on a
repetitive segment; a no-op when there is none) and cheap rollback of failures.
**Distinctive move:** the staged outbox and the structural stop-loss (same action
failing k times forces escalation — never the model's self-report).

---

## 2. PORTCULLIS — *warranted execution on a transactional substrate*

**Lens:** correctness-first. (A gate that, like its namesake, falls shut when anything
fails.)

Every side-effecting action must transit an out-of-process gate daemon on the tool
path — a proxy the model physically cannot bypass. To pass, an action carries a
**warrant**: a machine-readable claim of its effects in a small closed vocabulary
(paths, verbs, egress class, reversibility). The gate checks it against two layers:

1. a human-authored **Constitution** of ~20 generic invariants — a permission
   *ceiling*; and
2. an optional task envelope the model may propose, admitted **only if a solver proves
   it a strict refinement of the Constitution** — monotone narrowing, so the model can
   shrink its own permissions but never grant them.

It runs atop a copy-on-write workspace (snapshot-first, so a mistake costs a rollback,
not a recovery arc). For internally repetitive work, the agent submits one
contract-carrying program (code + declared footprint + postcondition); the gate
verifies it by static effect analysis plus a k-sample dry-run in a fork, then executes
all N under a monitor that kills on first envelope exit. Tiered triage self-gates:
reads pass in microseconds; irreversible external actions are constitutionally
non-delegable.

**Saves:** both. Input by deleting standing policy prose from every turn (~10–30%
prefill, compounding with caching). Output by collapsing mechanical phases and
converting recovery arcs into one reject+repair turn. **Distinctive move:** names
**verification-underwritten cheap-model routing** — because the gate bounds any
model's blast radius, a cheap model can be trusted more (kept as a composition idea).

---

## 3. PACT — *Proof-Carrying Acts under Chartered Transactions*

**Lens:** rare-hard-task-first.

Three durable planes live outside the context window: (1) a **Charter** — the task's
permission envelope in a small decidable predicate algebra (path prefixes, finite
tool/verb vocabularies, string patterns, linear resource bounds) **that compiles to
SMT**, seeded by a human standing charter and specialized per-task by a separate
"surveyor" model under a **monotone narrowing law** (LLM authorship may only shrink
the allowed set); (2) a deterministic **Gate** in the tool path, pre-execution,
fail-closed, with tiered effort (microsecond shape triage → predicate eval → SMT for
novel shapes and batches → refuse-or-escalate presenting the single undischarged
predicate), executing each act inside a copy-on-write transaction; (3) an append-only
**Ledger** of verified acts + post-state digests that becomes ground-truth memory and
*licenses* aggressive compaction.

Cost is then attacked inside the instance: a loop detector watches the ledger for ≥2–3
similar acts, compiles the remaining N−k into one parameterized program whose whole
parameter space is symbolically checked at the gate and whose postcondition is
sample-validated in the transaction before machine-speed apply.

**Saves:** both. 30–70% prefill (the gate makes aggressive compaction safe); >90% of a
repetitive fraction's output; a self-gating floor of ~0 cost / ~0 overhead on a
novel, non-repetitive, read-only task. **Distinctive move:** the most explicit
statement of the **decidable predicate algebra compiling to SMT-LIB2**, with monotone
narrowing enforced *by the type of the composition operator, not by review* — which is
openDaisugi's thesis, rebuilt blind, almost verbatim.

---

## 4. Interlock — *an effect-escrow sidecar*

**Lens:** composability-first. (A service-mesh proxy for tool traffic — the "Envoy" of
agent actions.)

A thin interposition shim at the tool-call interface. All side-effecting actions pass
through a sidecar that (1) checks each against a permission envelope living in the
sidecar, never the transcript; (2) executes risky or batched actions in **escrow** — a
copy-on-write shadow — then verifies the resulting *effect-trace* (paths, verbs,
egress) against the envelope **plus the task's own tests** before atomically
committing or rolling back; and (3) keeps a small machine-owned progress ledger
outside the window. Authority is two-tier and monotonic: a human FLOOR active from
action #1, plus task DELTAS that may only tighten it.

Explicitly *not a harness* — it supercharges existing levers: it makes aggressive
compaction safe, makes cheap-model routing safe (the gate catches the cheap model's
dangerous mistakes pre-commit; **rejection/test-failure is the external escalation
signal instead of self-reported confidence**), gives each **sub-agent an attenuated
sub-envelope** so delegation cannot launder authority, and pins a ~100-token advisory
stub at a cache-stable position.

**Saves:** both, with three per-instance meters (turn-collapse via checked
macro-execution; safe aggressive compaction; cheap failure/rollback), plus 2–5×
$/token where gate-protected routing lets a small model drive covered steps.
**Distinctive move:** the only design to solve **sub-agent authority attenuation** —
the delegation hole the other four leave open.

---

## 5. CONTINUO — *the Score-and-Warrant runtime*

**Lens:** blank-slate contrarian, told to look *unlike* the field. (It landed on the
same spine anyway. "The transcript is not the computer; the program is.")

Invert the loop: stop treating the LLM as the interpreter and the window as RAM. At
task start the model emits two artifacts into a runtime store: (1) a **Score** — a
typed *partial program* over a closed action vocabulary, in which confident-
deterministic stretches are fully compiled (a loop over 200 files is one loop, not 200
turns) and every point of genuine uncertainty is a typed **hole** with a dataflow
contract; and (2) a proposed **Warrant** that may only *narrow* a standing
operator-authored environment Warrant, its subset relation solver-checked (lattice
meet). A deterministic runtime interprets the Score: compiled stretches run for zero
model tokens after their loop body is **symbolically certified against the Warrant
once** (one abstract-interpretation proof covers all iterations); each hole is a
minimal-context model call carrying only its declared inputs, never the transcript.
Reversible effects run in CoW transactions; acceptance predicates at program joints
re-open a bounded hole with failure evidence instead of resuming a drifted transcript.

**Saves:** both — 60–90% input on long tasks (per-hole payload never grows with task
length, so cumulative prefill is not quadratic), 30–70% output on tasks with internal
structure. **Distinctive moves:** three the others did not build — **abstract-
interpretation loop certification** (one soundness proof over a loop body for all
iterations), **typed holes reconstructed from a store** rather than the transcript,
and **disposable-transcript resumption from the last certified joint after total
session loss**. It also honestly admits its floor: on a genuinely observation-driven
one-off, the Score degenerates to a chain of holes ≈ a plain gated reactive agent.

# Design: the delegation tree, each edge proved

Status: **Built in part**, 2026-09-30. The owner adopted every recommendation in "Open
questions" (owner rulings AT-1 to AT-7, marked there). Recommendation steps 1 to 3 are built in
Python, Go and Rust: `edge_ok`, the deadline field, proof at registration, the tree ledger with
its budgets and asks, and the agentic step's child envelope (rulings TR-R-1 to TR-R-12 in
`clients/ADJUDICATIONS.md`; see "Built, 2026-09-30" below). Step 4 (coppice) is not built: the
adversarial test of the session binding in coppice comes first. Step 5 (sprig) waits for sprig.
The code facts in the next sections were read on 2026-09-26; where a fact changed since, the text
says so.

Source: `strategy-2026-09-26.md`, section "A tree of agents, each edge proved".

## The problem

An agent that can start other agents is a new kind of risk. The parent has an envelope. The
child is a second model with its own tools. If the child can do anything the parent could not,
then starting a child is a way around the gate. One level of this is common today: coppice
already shows a harness's own subagents on the parent's row
(`harness/coppice/internal/server/children.go`), and the strategy notes record Grok Build running
up to 8 parallel agents (from the strategy notes; not verified here). Several levels are next: a planner starts workers, a worker starts
a reviewer, and so on.

The strategy notes say that sprig's main user is an agent, not a person. A sprig that starts
sprigs gives a tree of agents. The human cannot read every call in that tree. The human needs one
rule that holds at every edge, and a window that shows the tree and the few asks that need a
person.

## The rule: attenuation, proved per edge

**When a parent starts a child, the child's envelope must be proved to fit inside the parent's
envelope before the child runs. If the proof fails, times out, or cannot be made, the child does
not start.**

- Permissions can only shrink down the tree. By induction, every node is inside the root's
  envelope, and the root is the envelope the human set.
- The proof is the subsumption check daisugi already has (`src/opendaisugi/subsumption.py`,
  `envelope_subsumes`). The yellow paper already names this relation as the one behind
  "skills-as-contracts, safe sub-agents, inheritance, and pathway reuse" (`docs/spec/yellow-paper.md`
  sections 5 and 7).
- A failed proof gives a reason, and where Z3 finds one, a counterexample: the concrete shell
  command the child could run that the parent may not (`subsumption.Counterexample`). The parent
  model reads it and narrows its request. The bots write the policy; the proof is mechanical.
- The gate still checks every call of every child at run time against that child's envelope. The
  edge proof is the static wall; the gate is the dynamic wall. This is the same two walls
  `src/opendaisugi/agentic_executor.py` describes for one level.

This is known prior art in capability systems. Macaroons attenuate authority with caveats as it is
delegated (https://research.google/pubs/macaroons-cookies-with-contextual-caveats-for-decentralized-authorization-in-the-cloud/).
The UCAN spec states that "each direct delegation MUST either directly restate or attenuate
(diminish) its capabilities" (https://github.com/ucan-wg/spec). What daisugi adds is that the
containment is proved over envelope languages by Z3 (globs, shell heads, predicates, robot
bounds), not checked by string match on a token list.

## What exists today, and where it falls short

The strategy notes say the child's envelope is "proved by the subsumption check that already
guards skill delegation". That check exists, but the code has gaps against the tree rule. These
findings are the core of this design.

### Two containment relations, neither of them the full edge rule

| | `envelope_subsumes` (`subsumption.py`) | `verify_inheritance` (`inheritance.py`) |
|---|---|---|
| Method | Z3 over glob languages, shell heads, predicates | Plain set subset and `<=` |
| file_read, file_write, mcp_allowlist | Yes, as glob languages | Yes, as literal sets |
| shell, shell_allowlist, decomposition opt-in | Yes | Yes |
| network, network_hosts | Yes | Yes |
| Robot bounds | Yes (`_robot_capability_violation`) | Yes (reuses the same function) |
| Invariants | Yes, compiled; opaque ones surfaced, or denied under strict | Parent's set must be a subset of the child's, by value |
| Postconditions | No | Yes, superset |
| `max_execution_time_s`, `max_output_size_mb` | **No** (yes since f9e202e7) | Yes |
| `stakes` (no downgrade) | **No** (yes since f9e202e7) | Yes |
| `custom_step_allowlist` | **No** (yes since f9e202e7) | **No** |
| `shell_interpreter_policy` | Read from the outer only | No |
| Depth | Any | **Depth 1 only**: fails if `parent.parent_envelope` is set |
| Ported to Go and Rust | Yes (`clients/go/internal/verify/subsumption.go`, `clients/rust/src/subsumption.rs`) | **No** |

Consequences:

- **No single call proved a tree edge** (2026-09-26; `tree.edge_ok` is that call now). Z3 subsumption misses budgets and stakes. A child could
  lower its stakes from `high` to `low` and so turn strict mode off for itself
  (`verify.resolve_strict`). Inheritance catches that, but it compares globs as strings, so a
  child glob `src/app/**` under a parent `src/**` is refused though it is narrower.
- **Inheritance cannot go below two levels.** `verify_inheritance` refuses any parent that has a
  parent of its own. A tree of depth three fails today by construction.
- **`custom_step_allowlist` is a widening path.** Neither relation compares it. Under strict mode
  a custom step type runs only when this list names it (`verify.py`, around line 695), so a child
  that lists more custom types than its parent gets more authority, and no check sees it.
- **Sequence invariants are not proved.** `envelope_subsumes` reasons over one symbolic step. It
  collapses `ForallSteps` and `ExistsStep` to their inner predicate (`_compile_invariants`). An
  invariant about order or count over a whole plan is not compared.
- **Parity.** Only the Z3 half is ported. The full edge rule needs the Python oracle, then Go and
  Rust with the same golden cases.

### Lenient mode fails open on two paths (one left)

`verify.check_skill_delegations` lets two cases through as warnings in lenient mode:

- a `SkillStep` with no `contract_envelope` (an opaque skill), and
- a Z3 timeout during subsumption, when a `warnings_out` list is given. (Since fixed: a timeout
  is a violation in every mode, in all three clients.)

Also (2026-09-26; since fixed: it returns `holds=False, timed_out=True`), the main Z3 query in
`envelope_subsumes` raises `VerificationTimeout` on `unknown`; it
does not return `holds=False`. (The file, network and MCP axes, in `_patterns_subsume`, already
return a deny on `unknown`.) Callers must turn the raise into a deny.

The owner's ruling is fail closed. **The tree edge must deny on a timeout and deny a child that
declares no envelope, at every stakes level.** It must not reuse the lenient path of
`check_skill_delegations`.

### The existing one-level child paths

| Path | File | Does a proof run at the edge? |
|---|---|---|
| `SkillStep` in a plan | `verify.check_skill_delegations` via `contracts.verify_delegation` | Yes, Z3 subsumption. Lenient mode fails open as above. |
| `SafeSubagent.create` | `src/opendaisugi/subagent.py` | Yes, `verify_delegation`; raises `DelegationDenied`. Python only, plan level, dry run by default. This is the closest thing to the tree rule today. |
| `AgenticStep` via `AgenticExecutor` | `verify._check_agentic_step`, `agentic_executor.py` | **No.** The verifier maps each requested host tool to a capability by name. The executor registers the **parent's own envelope** for the child (`register_envelope(self.envelope, ...)`) in a private gate root, pinned to session `default`. The child is equal to the parent, not proved smaller. |
| `TaskStep` | `models.TaskStep` | Not needed: a pure-reasoning leaf with no capability field. |
| `DelegatingExecutor` | `delegating_executor.py` | Not needed: it prompts a model for one step; the step was already verified. |

`_check_delegation_safety` in `verify.py` refuses every `AgenticStep`, and every step with a
`preferred_model`, under `stakes='physical'`.

### The gate never proves anything when an envelope is registered

`gate.register_envelope` writes the envelope file for a session id, or as `default`.
`gate.load_envelope` loads the exact session, then falls back to `default` (2026-09-26; since
SEC-3 a pinned session gets its own envelope or none, and the payload never selects one). No containment check
runs at registration. The session id comes from the harness's hook payload, which is untrusted
input.

- `AgenticExecutor` avoids the risk: a fresh private root outside the workspace and a pinned
  session.
- coppice and sprig have no such pin. In a shared gate root, a child that names another session's
  id might get that session's envelope. **Not verified**; this needs an adversarial test.
- A child pane whose session has no registered envelope gets `default`, whatever the parent had
  (true only for an unpinned gate since SEC-3; a pinned one denies).

### coppice: the tree exists, with no envelopes on it

- **Tasks** form a tree (`harness/coppice/internal/server/tasks.go`, PROTOCOL.md "Tasks"): a label,
  a parent, a cwd or worktree, a model. A task with children is a team. **A task carries no
  envelope field.**
- **Panes can start panes.** `pane.create` is open to a pane connection (PROTOCOL.md "Roles": only
  the allow verbs, the foreman label, `task.set_foreman`, `task.move` and renames of other panes
  are refused). The call is recorded as a `note` before it runs. So any agent in a pane can start
  a child agent today: allowed and logged, with no edge check.
- **Harness-internal subagents** (`children.go`, `pane.report_child`) are shown read only on the
  parent's row. coppice cannot type into them or close them.
- **Asks already flow up one level** (see below).

### sprig: the unit, not yet a parent

- `harness/sprig/loop.go`: the loop is a model, a gated executor and a turn limit.
- `harness/sprig/tools.go`: four tools (read, write, edit, bash). **There is no tool to start a
  child.** A sprig can start a child only through `bash`, which the gate sees as a shell command.
- `harness/sprig/daisugi_gate.go`: every call goes to `daisugi gate check --mode enforce`, out of
  process, fail closed, with a session id when `--session-dir` is set.
- `harness/sprig/cli.go`: one task in, one answer out, then exit. coppice's sprig adapter
  (`harness/coppice/internal/adapters/sprig/sprig.go`) runs one process per prompt and tails the
  session tree.
- `harness/sprig/fleet/`: runs many sprigs in one process. `EscalatingGate` pauses a job on a gate
  deny and lets the operator override or uphold. It is flat: a board of jobs, no tree, no child
  envelopes. Its "approve" overrides the envelope, which is the opposite of attenuation.

## How a child is started

The rule is the same for every starter. Only the place the proof runs differs.

1. The parent (a model) writes the child's task and a proposed child envelope. The model writes
   it; no human writes envelope language.
2. The starter asks daisugi to prove the edge: `child ⊑ parent`, using the full edge rule below.
3. On a proof, the starter registers the child envelope in a gate root the child cannot write,
   bound to an id the starter chose, not the child. Then it starts the child.
4. On a refusal, the parent gets the reason and the counterexample, and may propose a narrower
   envelope. After a small number of tries it gives up and asks up the tree.

| Starter | Where the proof runs | Status |
|---|---|---|
| sprig, as a parent | A new `spawn` tool in sprig, or a `daisugi tree spawn` command sprig calls through `bash` | Designed here. Not built. |
| coppice, `pane.create` or `task.create` with an envelope | coppice asks daisugi before it starts the pane, and records the envelope on the task | Designed here. Not built. |
| The foreman | The foreman is a pane, so it uses the coppice path. It gets no extra rights. | Designed here. Not built. |
| A plan's `AgenticStep` | `AgenticExecutor`, on `claude -p` or on sprig (`--agent sprig`) | Built: the child envelope is proved (TR-R-10); the sprig runtime 2026-10-09 (SX-R-2). |

### The full edge rule (proposed)

`edge_ok(parent, child)` holds only when every part below holds:

1. `envelope_subsumes(parent, child, strict=True)` holds. A timeout is a deny.
2. The child's budgets are at most the parent's remaining budgets (see "Time and budget").
3. The child's stakes are at least the parent's.
4. `custom_step_allowlist`: the child's set is a subset of the parent's.
5. The child's invariants include the parent's enforced invariants, and the child's postconditions
   include the parent's (as `verify_inheritance` does today, by value).
6. The child's `shell_interpreter_policy` is not looser than the parent's.
7. The child declares an envelope. No envelope means no child.

Two registration rules close the paths around the edge:

- **Only the operator registers a root.** A registration with no parent is a root envelope, and
  only the operator may make one. Any other registration names a parent and runs `edge_ok`.
- **No `default` under a tree.** A child session with no exact registered envelope is denied. It
  never falls back to the `default` envelope (`gate.load_envelope` does fall back today).

There is no depth limit. Each edge is checked against its own parent, so depth follows by
induction. Strict mode is always on at an edge, whatever the stakes: an edge is a delegation of
authority to a second model, which is the case strict mode exists for.

This is one new function in daisugi, with golden cases, in Python first, then Go and Rust. It
reuses `envelope_subsumes` and the checks in `inheritance.py`; it does not replace them.

## What the proof checks, and what it cannot

**It checks** that every action the child's envelope admits, the parent's envelope admits too, on
the dimensions the yellow paper covers: shell heads and metacharacters, file globs, network hosts,
MCP tools, compiled invariants, robot bounds. With the rule above it also checks budgets, stakes
and custom step types.

**It cannot check:**

- **The quality of the child's work.** The envelope bounds actions, not outcomes. A child that
  stays inside its envelope can still write wrong code, waste its budget, or return a bad answer.
  That is the job of tests, a pass/fail feature list, and ranking.
- **What an interpreter does with its arguments.** `python -c`, `find -exec`, `make` pass the
  shell-head check. `envelope_subsumes` surfaces them as `shell_interpreter:<name>`; under a
  strict interpreter policy it refuses them.
- **Opaque invariants.** An invariant with no `expr` cannot be compared. Strict mode refuses it
  on the child side.
- **Sequence properties** over a whole plan (see above).
- **Information flow.** A child that may read a secret and may write a file inside its envelope
  can copy the secret there. Attenuation limits where actions go, not what data moves between
  allowed places.
- **Collusion across siblings.** Two children, each inside its envelope, can together do what
  the parent intended to prevent, if the parent's envelope admits both halves. The parent's
  envelope is the real bound. The tree never exceeds the root; it does not guarantee more than
  the root.
- **Escape outside the gate.** This is plan-level runtime assurance, not an OS sandbox
  (`subagent.py` says so). A process started outside the harness's hook is outside the gate.

## Time and budget down the tree

Today:

- `Permission.max_execution_time_s` (default 30) and `max_output_size_mb` (default 10) are per
  step, in the envelope (`models.py`). Only `verify_inheritance` compares them.
- There is **no token budget in the envelope.** `budget.BudgetTracker` (`budget.py`) keeps a
  running total for one orchestration run, from rough estimates, and can stop or downgrade a step.
- Turn limits: `AgenticStep.max_turns` and sprig's `--max-turns` (default 20).
- coppice holds last 120 s (`escalate.go`, `DefaultHoldFor`).

For a tree, a budget must be **split, not copied**: if each child gets the parent's full budget,
N children spend N times the parent's budget. The proposed rule is that the parent reserves each
child's budget from its own remaining budget when it starts the child, and gets the unspent part
back when the child ends. Wall-clock time is a deadline, not an amount: the child's deadline is at
or before the parent's.

**Open, budget location.**
- Option A: add token, turn and deadline fields to the envelope, and add them to the edge rule.
  The proof then covers them, and the journal records them with the envelope.
- Option B: keep budgets outside the envelope, in a tree ledger the starter keeps. The envelope
  stays about what is allowed; the ledger is about how much.
- Recommendation: B first, because token counts are estimates today and the envelope's other
  fields are exact. A deadline can go in the envelope (A) because it is exact.

## How asks flow up the tree

coppice already does this for one level, and it has the attenuation shape:

- A pane "can propose. It cannot allow." (PROTOCOL.md, Roles.)
- A task's foreman hears an undoable ask first, holds it for up to 120 s, and may **deny** it. It
  can never **allow** it (`escalate.go`, `escalateAsk`; PROTOCOL.md, Holds). A permanent ask goes
  to the operator at once; the foreman only gets a note.
- The nearest foreman up the task tree holds the ask (`foremanOf`).
- An operator's allow with `scope: task` is recorded as a proposed envelope edit that nothing
  applies (PROTOCOL.md, `agent.allow`).

The tree keeps these rules. A parent agent may deny a child's ask. Only the human may allow
something outside an envelope. An allow never widens the root; it grants one call, or it is a
proposal the human reviews.

**Open, multi-hop holds.** Each level that holds an ask adds up to 120 s, and each hold ends 30 s
before the ask's own deadline (`holdMargin` in `escalate.go`). So several levels either delay the
human by up to 120 s per level, or run out of deadline, for an ask no parent can allow anyway.
- Option A: nearest foreman, then the operator (today's rule). Simple, and the wait is bounded.
- Option B: each ancestor in turn, with the hold split so the total stays under the gate's
  deadline.
- Recommendation: A. A parent higher up adds little, since it can only deny.

`sprig/fleet`'s `EscalatingGate` lets the operator "approve" a call the envelope refused. That is
an override, not a narrowing. In a tree it should stay an operator-only action, and never a
parent's.

## How ranking picks among children's attempts

A parent may start several children on the same task and keep the best result. The strategy
notes make ranking a daisugi primitive: hard constraints eliminate (out of envelope, failed proof,
failed required test), measured scores rank the rest, and preferences break ties, with the owner's
pairwise picks trusted most. The ranking design is **to be written**. The tree needs only two
things from it: a call that takes N attempts and returns an order with a confidence, and a rule
that an attempt from a child whose edge proof failed is never ranked, because it never ran.

## Relation to physical swarms

The same edge relation serves a swarm, but not the whole rule. Say this plainly:

- **The same:** a coordinator's envelope bounds each robot's envelope, proved by
  `_robot_capability_violation` inside `envelope_subsumes`: workspace box inside the parent's,
  velocity and torque at most the parent's, joint ranges inside, and every obstacle the parent
  forbids also forbidden. An undeclared bound where the parent has one is a deny.
- **Different:** `_check_delegation_safety` refuses every `AgenticStep` under
  `stakes='physical'`. A physical-stakes parent cannot start an LLM-driven child at all today. So a
  swarm tree is a tree of envelopes over controllers or VLA steps, not a tree of agents that
  choose tools.
- The robot check is plan level and "NOT trajectory reachability" (`subsumption.py`). Keeping two
  robots apart is a property of their joint motion, which a per-edge check cannot see. It needs a
  check over the siblings together, or a run-time monitor.

## Summary: exists, designed, missing

| Item | State |
|---|---|
| Z3 envelope subsumption, Python, Go, Rust | Exists |
| Syntactic inheritance with budgets and stakes, Python, depth 1 | Exists |
| Proved child edge for skills and `SafeSubagent` | Exists, one level, Python |
| `AgenticExecutor` child runs under the parent's envelope, gated | Exists; no narrowing, no proof |
| coppice task tree, foreman holds, deny-only foreman | Exists |
| coppice read-only view of harness subagents | Exists |
| sprig loop, out-of-process gate, one task per process | Exists (on hold) |
| sprig fleet, flat board, operator override | Exists (on hold) |
| Full edge rule (`edge_ok`), any depth, strict, fail closed | **Built** 2026-09-30, Python, Go, Rust (TR-R-2, TR-R-3) |
| `custom_step_allowlist` in the edge check | **Built** (TR-R-2) |
| Proof at envelope registration, in a root the child cannot write | **Built**: `gate register --parent`, `tree spawn` (TR-R-8, TR-R-4); the gate denies an agent's registration (TR-R-9) |
| Session binding the child cannot choose | Built at the gate (the pin, SEC-3); the coppice risk is not verified yet |
| Envelope on a coppice task, check on `pane.create` from a pane | Missing (step 4) |
| sprig as the sub-agent of an `AgenticStep` | **Built** 2026-10-09, Python, Go, Rust (SX-R-1 to SX-R-10) |
| sprig spawn tool | Missing (step 5) |
| Budget split down the tree | **Built**: the tree ledger (TR-R-5) |
| Deadline down the tree | **Built**: an envelope field, proved per edge, not enforced at call time (TR-R-1) |
| `AgenticExecutor` child envelope, proved | **Built**, Python (TR-R-10) |
| Ranking primitive | To be written |
| Go and Rust ports of the edge rule | Missing |

## Options

### Option 1: sprig agent-first headless mode

sprig gets a mode built for an agent as its user: a JSONL protocol on stdin and stdout, a
long-lived process, and a `spawn` tool that proves the edge and starts a child sprig.

- For: the loop that starts children is ours, so the proof runs in the loop, before any process
  starts. It fits the strategy's list of sprig's possible edges (verified plan trees, router in
  the loop).
- Against: sprig is on hold. sprig today is one task per process (`cli.go`); a long-lived mode is
  new work. A second JSONL protocol would be one more thing to keep in parity.

### Option 2: sprig speaks pi's RPC protocol

pi has an RPC mode: `pi --mode rpc`, JSONL on stdin and stdout, commands such as `prompt`,
`get_state` and `set_model`, and events for runs, tool execution and the lifecycle, including
`agent_end` and `agent_settled` (https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/rpc.md).
`spec-04-pi.md` adds `steer`, `follow_up` and `switch_session`; the page read above did not list
those, so they are taken from spec-04 here. coppice's pi adapter (`adapters/pi/pi.go`) already
drives this protocol.

- For: one protocol for two harnesses. coppice could drive a sprig with the pi adapter, and any
  tool that drives pi could drive sprig.
- Against: pi's protocol has no notion of a child envelope, and the pi page read has no
  subagent section. `spec-04-pi.md` puts pi's own subagent extension out of scope. We would still
  add the edge proof on our side. We would also follow a protocol we do not own.

### Option 3: coppice panes as tree nodes

A coppice task carries an envelope. `task.create` with a parent task proves the edge against the
parent task's envelope. `pane.create` with a `task` registers the task's envelope for the pane's
session, in a root the pane cannot write, before the pane starts. A pane that starts a pane
without a task gets its own task's envelope at most.

- For: harness neutral. pi with the daisugi extension, Claude Code, Codex, OpenCode and a future
  sprig all get the rule, because the gate enforces whatever envelope coppice registered. It does
  not wait for sprig. The task tree, foremen and holds already exist.
- Against: coppice becomes a place that calls a proof, which ADR-0020 allows only if coppice does
  not decide: coppice asks daisugi, daisugi decides. It covers only children started through
  coppice. A harness's internal subagents (`children.go`) run under the harness's own hook, with
  whatever envelope their session resolves to.

## Recommendation

Put the rule in daisugi first, and make it harness neutral. This part does not need sprig.

1. **Built 2026-09-30.** **daisugi: `edge_ok(parent, child)`**, as defined above. Strict always, fail closed on timeout
   and on a missing envelope, no depth limit, `custom_step_allowlist` included. Python oracle
   first, with golden cases, then Go and Rust.
2. **Built 2026-09-30.** **daisugi: prove at registration.** A registration that names a parent envelope runs `edge_ok`
   and refuses on failure. Only the operator registers an envelope with no parent (a root). Under
   a tree, a child session with no exact registered envelope is denied, never given `default`.
   The starter, not the child, picks the session binding.
3. **Built 2026-09-30.** **Fix the one-level paths to match.** `AgenticExecutor` accepts a narrower child envelope and
   proves it; the lenient paths in `check_skill_delegations` do not apply at tree edges.
4. **Not built.** **coppice: Option 3.** An envelope on the task; the proof on `task.create` and `pane.create`;
   asks keep today's rule (nearest foreman may deny, only the operator may allow).
5. **Not built.** (sprig as an agentic executor is built, 2026-10-09, SX-R-2; the `spawn` tool is not.) **sprig: later.** When the owner resumes sprig, Option 1 (an agent-first mode with a `spawn`
   tool) on top of the same daisugi call. Option 2 is worth doing only if coppice or other tools
   need one protocol for pi and sprig; it does not help the edge rule.

Order: measure first, as the router design says. Before step 4, add an adversarial test of the
session-binding risk in coppice, because if it is real it is a gap today, with no tree at all.

## Open questions for the owner

The owner adopted every recommendation below on 2026-09-30 (owner rulings AT-1 to AT-7 in
`clients/ADJUDICATIONS.md`; each is marked).

1. **Owner ruling AT-1: yes, before sprig.** **Is the tree rule wanted before sprig resumes?** Steps 1 to 4 do not need sprig. Or does the
   whole design wait with sprig?
2. **Owner ruling AT-2: B for tokens and turns, A for the deadline.** **Budget location:** in the envelope (A) or a tree ledger (B)? Recommendation: B for tokens and
   turns, A for the deadline.
3. **Owner ruling AT-3: A.** **Multi-hop holds:** nearest foreman then operator (A), or each ancestor in turn (B)?
   Recommendation: A.
4. **Owner ruling AT-4: yes.** **Strict at every edge:** always, whatever the stakes? Recommendation: yes. It will refuse
   children whose envelopes use unsupported globs or opaque invariants, which the parent model must
   then rewrite.
5. **Owner ruling AT-5: keep it.** **Physical stakes:** keep the refusal of LLM-driven children under `stakes='physical'`?
   Recommendation: keep it. A swarm tree is a tree of envelopes over controllers, with a separate
   design for joint motion across siblings.
6. **Owner ruling AT-6: three, then ask.** **The failed-proof loop:** how many narrower proposals may a parent make before the child ask
   goes to a human? Recommendation: three, then ask.
7. **Owner ruling AT-7: fold it into coppice's rules when sprig resumes.** **sprig fleet's operator override:** keep it as an operator-only action, or remove it in favor
   of coppice's allow rules? Recommendation: fold it into coppice's rules when sprig resumes.

## Built, 2026-09-30

Recommendation steps 1 to 3, in Python (the oracle), Go and Rust; the rulings are TR-R-1 to
TR-R-12 in `clients/ADJUDICATIONS.md`.

- **The edge rule.** `tree.edge_ok(parent, child)` (`src/opendaisugi/tree.py`; Go
  `internal/tree`, Rust `src/tree.rs`) checks every part of "The full edge rule" in a fixed order
  and reports each failing part, strict, at any depth, failing closed on a missing envelope and on
  a proof that does not finish. Reasons never name a solver model, so all three clients print the
  same ones; Python callers still get Z3's counterexample. Z3 decides the shell and invariant
  query only when an invariant with an expr can change it; otherwise the head rule decides it
  exactly (TR-R-3).
- **The deadline.** `Envelope.deadline` (Unix seconds), hidden from the model's schema and left
  out of dumps when absent. A child's is at or before its parent's; a child with none takes the
  parent's. It is proved per edge, and since 2026-10-01 the gate denies a call after the
  deadline of the envelope that checks it (SW-4).
- **Registration.** `daisugi gate register --parent P` proves the edge before it registers.
  `daisugi tree root` (the operator) and `daisugi tree spawn` (the starter) register under a
  session the caller names, never `default`. The gate denies an agent's shell line that runs
  `gate register`, `gate init`, `start` or a tree verb that writes (TR-R-9), read per simple
  command (SW-2). `gate disarm` and the other verbs that change enforcement are denied too (SW-1).
- **The tree ledger.** `<data dir>/tree/ledger.jsonl`, append-only, under a lock: the starter
  reserves a child's tokens and turns from its own remaining budget, gets the unspent part back at
  `tree end`, and is refused a child that asks for more than remains. `daisugi tree status`
  shows the tree, text or `--json` (TR-R-5).
- **The failed-proof loop.** After three refused proposals from one parent, the next refused one
  is an ask; the operator answers with `daisugi tree answer`. An allowed child is marked not
  proved against its parent, and it must still fit inside the root of its branch, so an allow
  never widens the tree past its root (TR-R-7).
- **The one-level path.** `AgenticExecutor` proves an agentic step's `child_envelope` before the
  sub-agent starts, derives the tool wall from it and registers it under a session it picks
  (TR-R-10).

Evidence: 134 tree cases (`clients/tree_cases.py`) and 31 gate cases agree in Go and Rust;
every other compare still agrees. Not built: step 4 (coppice), whose first job is an adversarial
test of the session binding in coppice; step 5 (sprig); sequence invariants across a plan. The
deadline at call time was built on 2026-10-01 (SW-4).

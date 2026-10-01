# Design: grafts, a rewrite verdict, proved and learned

Status: proposal for the owner, 2026-09-26. Nothing here is built. The code facts were read on
2026-09-26. Outside facts come from `docs/research/grafts-inputs-2026-09-26.md`, which cites its
sources. This design did not fetch those pages again, so each outside claim below is "per the
research, not re-verified here".

Sources: `strategy-2026-09-26.md`, section "Token saving"; `design-router.md`, part 0;
`design-ranking.md`; `docs/research/token-saving-landscape-mapping.md`.
`design-dialects.md` does not exist yet, so this design does not depend on it.

## The problem

A frontier model spends tokens on routine I/O. It reads a 4,000-line file to find one function.
It runs `cat` on a log to see one error. Every one of those tokens enters the conversation
prefix and is read again on every later turn (at the cache-read price, but read again).

Spotify's shunt plugin for Claude Code attacks this (per the research): a hook stops a large
read, and a cheap worker model summarizes the file. The author reports 82% to 94% fewer Claude
read tokens on selected cases. Those are the author's numbers. They count tokens, not billed
cost, and they give no task success rate.

daisugi already sits on every tool call as the gate. So it can do what shunt does, with three
things shunt lacks: a proof that the cheaper call does no more than the original call and the
envelope allow, a journal that measures whether the saving is real, and a garden that learns the
rules from the owner's own work.

## The one idea

**A graft is a rule, held as data, that replaces an expensive tool call or its result with a
cheaper one. Each rule is proved safe at install, checked again on every call, run in audit
first, and kept only when the journal shows it saves billed cost with no loss of task success.**

The name: a graft joins a cheaper branch onto the tree, and the tree must accept it.

## The verdict

The gate returns three verdicts today: allow, deny, and ask (`GateDecision` in
`src/opendaisugi/gate.py`: the fields `allow`, `would_deny`, `ask`). Grafts add a fourth:

| Verdict | Meaning | Exists |
|---|---|---|
| allow | the call runs as proposed | Yes |
| deny | the call does not run; the reason goes back to the model | Yes |
| ask | an operator decides; the operator may edit the call (`updated_input`) | Yes |
| **rewrite** | "do this cheaper call instead": the call runs with a changed input, or its result is changed, by a named rule | Designed |

Rewrite is never a way to allow a call. A rewrite happens only after the gate has decided the
original call. The rules for that order are in "The two checks" below.

## The three shapes

Per the research, **no harness lets a hook swap one tool for another.** Every rewrite field
changes the arguments of the same tool. (The research infers this from each API's shape; no doc
says it outright.) So a graft has one of three shapes:

| Shape | What happens | Cost | Reaches built-in `Read` |
|---|---|---|---|
| **(a) shell rewrite** | A shell command's input is rewritten into a delegate command: `cat Big.java` becomes a delegate read of `Big.java`. The worker runs inside the delegate command, not inside the hook. | One turn. The worker's latency is inside the tool call. | No. A built-in read tool has no command to rewrite. The only same-tool change is adding `offset`/`limit`, which truncates and does not delegate. |
| **(b) output replacement** | The tool runs as proposed (free, local). A post-tool hook swaps its result for the worker's summary before the model reads it. | One turn. | Yes, where the harness's output channel accepts the tool's shape. |
| **(c) deny and redirect** | The gate denies the call. The reason names the delegate tool. The model calls the delegate itself. This is what shunt does. | One extra model turn, which re-reads the cached prefix. | Yes, on every harness. |

Shape (c) has one advantage: the model writes the question ("where is the retry loop?"), so the
worker answers it. In shape (a) and (b) the worker does not know the question. It returns a
general digest: an outline, the symbols with exact quotes, the size, and how to get the exact
text.

### Which harness supports which shape

From the research's table, with what daisugi wires today:

| Harness | (c) deny | (a) rewrite input | (b) replace output | daisugi today |
|---|---|---|---|---|
| Claude Code | Yes | Yes: `updatedInput`, with `allow` or `ask` | Yes: PostToolUse `updatedToolOutput`; a wrong shape is ignored for built-in tools | Gate wired (`install.py`, `_patch_claude_gate`). Emits `updatedInput` only for an operator edit (`gate.py`, `_outcome`, about lines 1245-1262). No PostToolUse hook. |
| Codex | Yes | Yes: `updatedInput` only with `allow`; for Bash it must hold a string `command` | Partly: PostToolUse `decision: "block"` replaces the result with the hook's text | Gate wired (`install.py`, `_patch_codex_gate`) with the same `--format claude` entry as Claude Code. Codex hooks fail open. |
| OpenCode | Yes: throw | Yes: mutate `output.args` | Yes: mutate `output.output` | Plugin only throws (`harness_opencode/plugin/daisugi-gate.ts`). |
| pi | Yes: `{block: true, reason}` | Yes: mutate `event.input`; no re-validation after | Yes: `tool_result` returns `content` | Extension blocks or returns nothing (`harness_pi/extension/index.ts`). |
| Hermes | Yes | Yes: `{"action":"modify","args":...}` | Yes: `transform_tool_result` | Block shape only, marked unverified (`hook.py`, `stdout_for_format`). Shell hooks fail open unless `fail_closed: true`. |
| OpenClaw | Yes | Yes: return `params` | Transcript only: `tool_result_persist`; whether the model reads it next is not verified | Block shape only, unverified. |

Two facts from the code make the pi and OpenCode work smaller than it looks:

- The resident gate's socket reply already carries `stdout` (`gate_server.py`, line 108:
  `{"v": 1, "stdout": ..., "stderr": ..., "exit_code": ...}`). The pi and OpenCode clients read
  only `exit_code` and `stderr`. To carry a rewrite, the clients must read `stdout`. The wire
  does not change.
- The Go and Rust gates already handle `updatedInput` for the operator edit
  (`clients/go/internal/gate/ask.go`, `clients/rust/src/gate/ask.rs`, `decide.rs`).

## Rules as data

A graft rule is a record, not code. Models write it (owner ruling: the bots write the policy).
The proof and the checks are mechanical. A person reads the rule, never writes a DSL.

```yaml
id: big-shell-read
version: 1
shape: shell_rewrite            # shell_rewrite | output_replace | deny_redirect
state: audit                    # proposed | audit | trial | active | retired
match:                          # a predicate over the gate's normalized record
  step_type: shell
  command_head: [cat, head, tail, less, more]
  single_simple_command: true   # no pipe, redirect, substitution or list
  operands: files_only          # every operand is a plain path the gate can resolve
  file_lines_over: 350          # measured on disk by the gate, not claimed by the model
conditions:
  stakes_max: high              # never under physical stakes
  harness: [claude, codex, pi, opencode]
  not_after_edit_intent: true   # see "Line numbers" below
replace:
  command: "daisugi-delegate read --digest -- {operands}"
worker:
  choose: router                # the router picks; the default is local
  allow_remote: false
provenance:
  proposed_by: garden           # or: owner, seed
  evidence: [trace ids and transcript turn ids]
  proof: {result: holds, checked_at: ..., envelope_id: ...}
```

Notes on the record:

- **The match is a predicate over the record the gate already builds** (`hook.py`,
  `_payload_to_record`: `step_type`, `command`, `path`, `url`). Where the predicate algebra
  (`predicate.py`) can say it, the match is written in that algebra, so Z3 can reason about it.
  `file_lines_over` is a measured condition: the gate stats the file. It cannot be proved at
  install, so it is only a condition on when the rule fires. A condition can only make a rule
  fire less, so it cannot widen anything.
- **The replacement is a template over the matched fields.** No free text from the model enters
  it. The operands pass through as they were, after the gate has already resolved them.
- **A rule names one shape.** The same intent on two harnesses may need two rules.
- **Rules live beside the envelopes in the gate root**, one file per rule, append-only history,
  like the pathway store. Their exact home is **Open** (below).

## The proof at install

A rule is installed only if a proof holds. The claim:

> For every call the rule matches, the replacement's effects fit inside the original call's
> effects plus what the envelope grants to the worker.

How it maps onto the code that exists:

1. **The delegate has a contract envelope.** The delegate command is not an opaque shell head.
   It ships a contract, the way a skill does (`contracts.py`, `verify_delegation`): file_read of
   exactly its operands, no file_write, and either no network (local worker) or network to the
   worker's host (remote worker).
2. **Build the outer envelope** from the original call's effects (file_read of the same operands)
   plus the worker grant that the session envelope holds.
3. **Prove outer subsumes the contract** with `envelope_subsumes` (`subsumption.py`). A failed
   proof returns a reason and, where Z3 finds one, a counterexample. The model that wrote the
   rule reads it and narrows the rule.
4. **Prove the session envelope subsumes the contract** too. A remote worker adds a network
   effect. The session envelope must grant it; a rule never grants it.
5. **Check the effect tier does not rise.** `effects.py` places each call in a class
   (`read`, `network_get`, `write_inside_workspace`, ...) and a tier (silent, undoable,
   permanent). The replacement's worst class must not be worse than the original's. Sending file
   text to a remote host is not a `network_get`. `effects.py` has no class for it today, so it is
   `unknown`, which is permanent. That is the right default: a remote worker needs an explicit
   grant.

### What the known subsumption gaps mean here

`design-delegation-tree.md` found that `envelope_subsumes` ignores `custom_step_allowlist`,
`max_execution_time_s`, `max_output_size_mb` and `stakes`. The fix is queued
(`.superpowers/sdd/omarchy/progress.md`, the security queue after the proxy work). For grafts:

- **Time.** A worker takes 10 to 30 seconds (per the research, from Spotify's post). A delegate
  can exceed `max_execution_time_s` and no proof notices. Until the fix lands, the graft proof
  compares the delegate's declared timeout with the envelope's limit itself.
- **Stakes.** The graft proof refuses any rule under `stakes: physical`, as
  `_check_delegation_safety` (`verify.py`) refuses delegated steps there. Until the fix lands,
  this is a direct check in the graft installer, not part of the Z3 call.
- **Output size.** A digest is smaller than the file, so this gap cannot widen a read. It is
  noted, not handled.
- **Custom step types.** A graft does not add step types, so this gap does not apply, unless a
  future rule targets a custom step. The installer refuses such a rule until the fix lands.
- **Timeouts.** `envelope_subsumes` raises `VerificationTimeout` on a Z3 `unknown`
  (`design-delegation-tree.md`, "Lenient mode fails open on two paths"). The installer turns the raise
  into a refusal. A rule that cannot be proved is not installed.

### The shell allowlist trap

The gate checks a shell command by its first word only (`verify.py`, `_head_allowed`,
`_extract_shell_head`). If the replacement is `daisugi delegate ...`, the envelope must allow the
head `daisugi`, and that grants every daisugi subcommand, including `run_plan`. So the delegate
is a separate executable with its own head, `daisugi-delegate`, and the gate maps that head to
the delegate's contract (step 1). The allowlist entry `daisugi-delegate` then grants only the
delegate.

## The two checks

The install proof covers the rule as a template. It is the static wall. The dynamic wall is the
gate on every call. For a rewrite:

1. The gate decides the **original** call as today (`_decide` in `gate.py`): the pane rule, the
   three hard-deny rules (`floor_config_hit`, `opencode_plugin_hit`, `search_above_secret_hit`),
   then the envelope.
2. If the original is denied, the answer is deny. **No graft turns a deny into an allow.**
3. If the original is allowed and an active rule matches, the gate builds the rewritten call and
   runs it through the **whole `_decide` again**. If the rewritten call is denied, the gate emits
   the original allow with no rewrite, and logs the failed graft. A failed graft costs tokens,
   never safety.
4. The verdict carries the rule id and version.

This matters because the operator edit path does not do step 3 today. In `_maybe_ask`
(`gate.py`, about lines 264-275) the operator's `updatedInput` is copied into the decision, and
`_outcome` emits it (about lines 1245-1262), with no second `_decide`. So an edited input is not
checked against the hard-deny rules. See "Conflicts found". Grafts must not copy that path.

### Not raising the approval level

- **Claude Code.** `permissionDecision: "allow"` with `updatedInput` skips the permission prompt
  (per the research). Today the gate's allow is `{"continue": true}` (`hook.py`), which grants
  nothing. So a rewrite omits `permissionDecision` and sends only `updatedInput`, as RTK's ask
  branch does, unless the original call was silent (envelope allowed, no ask).
- **Codex.** `updatedInput` works only with `allow`. So on Codex a graft applies only to calls
  the envelope already allows. This needs a separate `--format codex` output: today Codex shares
  `--format claude` (`install.py`, `_patch_codex_gate` reuses `gate_settings_json`), and one shape
  cannot follow two hosts' rules.
- **pi, OpenCode.** They have no permission prompt of their own that the gate's answer skips. A
  rewrite there changes only the arguments.

### Time budget

A Claude Code PreToolUse timeout lets the call run (per the research). Codex and Hermes shell
hooks also fail open. The pi client waits 5 s and the gate's verify budget is 4 s
(`harness_pi/extension/index.ts`). So **the worker never runs inside the gate**. The gate only
matches, stats a file and verifies. In shape (a) the worker runs inside `daisugi-delegate`. In
shape (b) it runs in the post-tool hook, whose timeout must be raised, and whose timeout fails
open on cost only (the full output reaches the model), never on safety.

## Other rewriting hooks

If RTK, shunt or another hook rewrites input beside the gate, the gate can verify input X while
Y runs (per the research: Claude Code runs matching hooks in parallel and does not say which
`updatedInput` wins; pi's later handlers see earlier mutations with no re-validation). This is a
fail-closed problem even with no grafts.

- **At install:** `daisugi install --gate` reads the harness's hook config. It refuses to install
  grafts beside any other PreToolUse hook it does not know to be read-only, and it warns about
  the gate itself. Known rewriters (RTK, shunt) are named in the warning.
- **At run time, where the harness allows it:** check the final input. On Claude Code a
  PostToolUse hook receives the `tool_input` that ran. The gate compares it with the input it
  verified and logs any difference as an incident. This detects, it does not prevent. On pi the
  daisugi handler should run last so it sees the final input; whether an extension can fix its
  order is not verified.

## The worker

- **Local by default.** The local rung exists: `tier1-local` in `route_turn` (`gateway.py`) and
  the `local` rung in `build_ladder` (`model_sizer.py`). A remote worker is opt-in and needs an
  envelope grant (the proof above).
- **The router picks it.** `design-router.md` makes the router one interface that takes a request
  and returns a model, a tier and a reason. A delegate call is one kind of request.
- **Output is untrusted data.** The worker reads untrusted file text and writes free text into
  the frontier's context. That is a prompt-injection path (per the research, citing
  Beurer-Kellner et al.). The digest goes back as data, fenced and labelled as a worker's
  summary. It is never spliced into a command, the same rule `TaskStep` follows (`models.py`).
  The journal marks each delegated result as untrusted. The gate still checks the next call.
- **Quotes, not line numbers.** Worker line numbers are unreliable (Spotify's own caveat, per the
  research). The digest carries exact quotes. `daisugi-delegate` checks each quote is an exact
  substring of the file and marks any that is not. It never returns line numbers.
- **Line numbers and edits.** A model that is about to edit needs the exact text. The digest ends
  with the command for a targeted read (`offset`/`limit`, or `daisugi-delegate read --raw`), and
  the rule condition `not_after_edit_intent` skips a graft when the same file was edited or named
  by an edit in the last few calls of the session. How "the last few" is measured is part of the
  audit tuning.
- **Refused under physical stakes.** As above.

## The learning loop

The garden already mines the journal for pathways (`distiller.py`). Grafts add a second product:
rules.

1. **Find the spend.** Mine transcripts for frontier tokens spent on routine I/O: tool results
   from read-like calls, their size, and the turns after them. The source on Claude Code is
   `claude_transcript.py`, which reads per-turn `usage` and `tool_result_ids`, joined to the
   gate's verdicts by `tool_use_id`. **Missing:** no code captures a tool result's size today.
   A grep for `tool_response` and `tool_output` over `src/opendaisugi` finds nothing. The
   transcript reader must add the result size. Other harnesses need their own reader (the Codex
   rollout parser exists for ingest; pi and OpenCode have none).
2. **Propose a rule.** A model writes a candidate rule from a cluster of such calls. The proof at
   install runs at once. A rule that fails is returned with the counterexample.
3. **Audit.** The rule matches but does not act. The gate logs "would rewrite" with the rule id.
   Audit gives match counts and an **estimated** saving only, marked `estimated=True` as the
   gateway meter marks its counterfactual (`gateway.py`, `TurnSaving`). Audit cannot measure
   extra turns or task success, because nothing changed.
4. **Trial (A/B with outcomes).** Built 2026-10-01 in its smallest form (SW-13 to SW-15): a rule
   in `trial` acts on the sessions whose seeded hash puts them in the graft arm, the arm is on
   the graft audit record, and `router status` reports each arm against the operator's labels.
   The rule acts on a random half of **sessions**, not of calls.
   Per-call splits are confounded by the cache and by extra turns later in the same session.
   Both arms are journaled with the measures below.
5. **Promote or retire.** Promote on measured lower billed cost per successful task, with no drop
   in task success. Retire a rule whose saving goes away or whose sessions fail more. Built
   2026-10-01 as a report only: `router status` prints what promotion would do; the operator
   changes the rule.

How it joins the other designs:

- **Ranking.** `design-ranking.md` recommends order (c): quality measures, then preferences,
  then cost as the last tie-break, with `quality_tiers` free of cost. A graft trial fits: the
  graft arm must share the top quality tier with the control arm; only then does its lower cost
  count. That design also found that `gardener/ab_test.py` has **no caller in `src/`** and its
  `tier2_tokens` is a placeholder 0. The graft A/B cannot reuse it as is. It compares envelopes,
  not outcomes.
- **Pathways.** A graft makes a call cheaper. A pathway removes the model call. When a graft rule
  fires on the same file for the same task often, that is a pathway candidate.

### Keep grafts out of the false-deny rate

The dogfood week measures the gate's false-deny rate from the audit log (`_log_audit`,
`gate.py`). Shape (c) denies a call the envelope allows. That is a cost deny, not a safety deny.
If the two mix, the metric is wrong. And `_deny` sets `allow = (mode == "audit")`, so in audit
mode a graft deny would silently allow, which makes the rule's own audit state and the gate
mode tangle. So:

- `GateDecision` gets a separate field, `graft: {rule_id, version, shape, state, applied}`.
- A graft is never built with `_deny`. Its state (audit, trial, active) is the rule's, apart
  from the gate mode. Overturned 2026-09-30 (RP-4): an audit-mode gate only records a graft,
  whatever the rule's state, so a cost deny never surprises an audit-mode user.
- `_log_shadow` and the shadow report count graft verdicts on their own line.

## Measuring honestly

Tokens are not cost. Per the research:

- An arXiv study (2607.12161) found the strongest tool-output compression on Claude Code cut
  delivered tool-output tokens by 38.4% and raised billed cost by 6.8%, because cache reads and
  writes dominate input cost and compression added turns.
- Quesma measured RTK on Terminal-Bench 2.1: 5% lower cost on Claude Code, 5% higher on
  OpenCode/DeepSeek, and did not recommend it as a generic saving.
- Spotify's and RTK's own figures count tokens.

So the journal records both, per session, for both arms:

| Measure | Source |
|---|---|
| Frontier tokens, split fresh, cache read, cache write, output | transcript `usage` (`claude_transcript.py`) or the gateway turn journal (`gateway_journal.py`) |
| Billed cost of those tokens | `price_turn` (`gateway.py`), which already prices the three input buckets |
| Worker tokens and cost (zero quota for local, but compute and latency) | `daisugi-delegate` |
| Turns per task, and extra turns after a graft (a targeted re-read) | transcript |
| Latency added | `daisugi-delegate` and the post hook |
| Task success | the feature list or tests, per `design-ranking.md`; where no label exists, the session does not count |

On a subscription the binding limit is quota, not dollars (the mapping doc's FinOps row). The
report shows quota tokens and dollars side by side, as `gateway-report` does. Promotion uses
billed cost per successful task, and the report also shows quota tokens so the owner can pick.

## Gateway and grafts

| | Gateway | Grafts |
|---|---|---|
| Level | the turn: which model answers the whole request | the tool call: what one call does or returns |
| Edits | nothing; non-downgraded turns are forwarded byte-untouched (mapping doc) | a tool call's input, or a tool result, by a proved rule |
| Cache | cache-aware routing (ADR-0015) | past results stay as they were; a graft changes only new content |

They stack. A grafted result is smaller content added to the prefix once. The gateway then
routes the turns that read that prefix. Shape (c) adds a turn, which the gateway meters like any
other.

### How grafts amend the mapping doc's rule

`docs/research/token-saving-landscape-mapping.md` rejects output caps and prompt compression at
the proxy: rewriting bodies "would perturb the cacheable prefix and put an assurance layer in the
business of editing requests". It adds that output caps belong harness-side.

Grafts are that harness-side placement. The amended rule:

- **daisugi never edits the model request body.** The gateway forwards it as today. So the prompt
  cache is kept.
- **daisugi may edit a tool call's input or a tool result, only by a named rule that is proved at
  install, re-checked by the gate on every call, and logged with its id.**

The mapping doc's second objection, that compression "changes meaning under a layer whose job is
verifying meaning", still applies to shape (b). That is why a graft is proved, logged, marked
untrusted, and promoted only on measured outcomes. The mapping doc should gain one row that says
this and links here.

## What exists and what is missing

| Piece | State | Where |
|---|---|---|
| Gate verdicts allow, deny, ask | Exists | `gate.py`, `GateDecision` |
| `updatedInput` on the Claude format, operator edit only | Exists (Python, Go, Rust) | `gate.py` `_outcome`; `clients/go/internal/gate/ask.go`; `clients/rust/src/gate/ask.rs` |
| Socket reply with `stdout` | Exists; pi and OpenCode clients ignore it | `gate_server.py` line 108 |
| Hard-deny rules before the envelope | Exists | `gate.py` `_decide`; `floor_config.py` |
| Effect classes and tiers | Exists; no class for "send file text to a host" | `effects.py` |
| Z3 subsumption | Exists (Python, Go, Rust); gaps queued | `subsumption.py` |
| Delegate contract checking pattern | Exists for skills | `contracts.py` |
| Local worker rung | Exists | `gateway.py` `route_turn`; `model_sizer.py` |
| Per-turn usage from Claude Code | Exists | `claude_transcript.py` |
| Rewrite verdict, rule store, rule schema | Missing | |
| Second `_decide` on a rewritten input | Missing | |
| `daisugi-delegate` executable and contract | Missing | |
| `delegate` MCP tool (router part 0) | Missing | `mcp_server.py` has none |
| `--format codex` | Missing | |
| PostToolUse or any output hook | Missing | `install.py` wires none |
| pi, OpenCode rewrite (reading `stdout`) | Missing | |
| Tool result size in any capture | Missing | |
| Graft A/B with outcomes | Built 2026-10-01, smallest form: arms per session, operator labels, cost per success (SW-13 to SW-15) | `router_report.py`, `gate.py` `_maybe_graft` |
| Rival rewriting hook detection | Missing | |

## Staged plan

Each stage is built in Python first as the oracle, then in Go and Rust with the same golden cases
(the conformance corpus, `docs/spec/conformance.md`). One heavy job at a time.

1. **Shape (c), the shunt equivalent.** This is router part 0. The rule store, the schema, the
   proof at install, and a `deny_redirect` rule whose reason names the `delegate` MCP tool. The
   `graft` field on `GateDecision`, kept apart from gate mode and the false-deny count. The
   `delegate` tool with quote checks, the local worker, untrusted marking. Measurement: result
   size in `claude_transcript.py`, and the table above in the journal. Works on every harness,
   because every harness can deny.
2. **Shape (a) for shell.** `daisugi-delegate` as its own head, with its contract. The second
   `_decide` on the rewritten input. The Claude output without `permissionDecision`, and
   `--format codex`. The pi and OpenCode clients read `stdout` and mutate the input. Rival hook
   detection at install. Hermes and OpenClaw stay deny-only until their shapes are verified
   against a real host.
3. **Shape (b), per tool.** A post-tool hook per harness. Each tool's output shape is pinned by a
   recorded golden case, because Claude Code ignores a wrong-shaped `updatedToolOutput` for
   built-in tools. Start with Claude Code `Read` and pi `read`. Codex only through
   `decision: "block"`, which the research marks partial.
4. **The learning loop.** Proposal from transcripts, audit, session-level trial, promotion
   through `design-ranking.md`'s order. This needs stage 1's measures and a few weeks of data.

Stage 1 and router part 0 are the same work. They should ship together, with router part 4's
measurement.

## Risks

- **Injection through the digest.** Quote checks catch invented text, not a wrong or planted
  conclusion. The gate still checks the next call; the journal marks the source.
- **Quality.** A cheap worker misses subtle bugs (Spotify's thread-safety case, per the
  research). Skill text keeps debugging, design and safety-critical code on the frontier. The
  trial measures task success, so a rule that hurts it is retired.
- **Extra turns erase savings.** A digest that sends the model back for a targeted read costs a
  turn. The trial measures turns.
- **Read-before-edit in shape (b).** A harness that requires a read before an edit records the
  read even though the model saw only a digest. The edit tool still needs an exact match, so a
  wrong edit fails, but the model may edit with less context. Not verified on any harness.
- **Silent no-op.** A wrong-shape output replacement is ignored and the full output reaches the
  model. That fails open on cost, not safety. Golden cases per tool catch it.
- **Rule drift.** A rule proved against yesterday's envelope may not hold against today's. The
  per-call second `_decide` is the guard; the proof is re-run when the envelope changes.

## Corrections to make

Comments in the code that the research showed are wrong or stale. Each is a small edit for the
build that touches the file.

- `src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts`, header: "OpenCode's
  tool.execute.before hook can only deny, by a throw, so this plugin can never grant anything."
  Wrong: per the research, mutating `output.args` changes the input that runs (OpenCode's
  `session/tools.ts` passes the same object to the tool). The plugin chooses to only throw; the
  hook can do more.
- `src/opendaisugi/install.py`, module docstring: "Gate: ... Claude Code only". Wrong: the same
  file wires the gate on Codex (`_patch_codex_gate`, line 1137, called at line 1099).
- `src/opendaisugi/tui_sessions.py`, line 375: "the harness's honoring of updatedInput is
  UNVERIFIED". Stale for Claude Code: its hooks docs document `updatedInput` (per the research),
  and `gate.py` already emits it for the operator edit. It is still true for the other harnesses.
  The comment should name the harness.

## Conflicts found

- **Operator edits skip the hard-deny rules.** `floor_config.py` says "No operator ask can turn
  that deny." A floor hit sets `pane_rule=True`, so it never reaches an ask. But an operator may
  answer any other ask with an edited input, and `_maybe_ask` passes that input to `_outcome`
  with no second `_decide` (`gate.py`, about lines 264-275 and 1624-1633). So an edit could name
  a floor path and run. Not tested here. The fix is the same second `_decide` that grafts need.
- **design-router.md names the plugin repository `sorantis/portal-ai-plugins`** ("not
  verified"). The research found it at `spotify/portal-ai-plugins`, Apache-2.0.
- **design-router.md says rewrite support in harness hooks "is not verified here".** The research
  has now answered it: see the harness table above.
- **Codex shares the Claude output format.** Correct for deny today, but not for rewrite, since
  Codex takes `updatedInput` only with `allow`.

## Open questions for the owner

1. **Which shape first.**
   - (a) first: one turn, but no built-in reads.
   - (c) first: works everywhere, the model states its question, one extra turn.
   - **Recommendation: (c) first**, as router part 0 already chose. It needs the least new
     plumbing, and it produces the measures every later stage needs.
2. **The worker.**
   - Local only; router-chosen with local default; any model the envelope grants.
   - **Recommendation: router-chosen, local by default**, remote only with an envelope grant and
     the effect tier at permanent.
3. **Rival rewriting hooks.**
   - Refuse to install; warn and install; install and check the final input after the call.
   - **Recommendation: refuse to install grafts beside an unknown PreToolUse hook, with a warning
     naming it**, and add the post-call input check on Claude Code as detection. The gate itself
     still installs; the warning says it cannot vouch for the final input.
4. **The delegate's form.**
   - `daisugi delegate` subcommand; a separate `daisugi-delegate` executable; the MCP tool only.
   - **Recommendation: `daisugi-delegate` for shapes (a) and (b), and the `delegate` MCP tool for
     shape (c)**, both calling one library. A subcommand would force the envelope to allow the
     `daisugi` head, which grants every subcommand.
5. **Where rules live.**
   - The gate root beside envelopes; the pathway store; a new store.
   - **Recommendation: the gate root**, one file per rule, since the gate reads them on every
     call and the pathway store is a garden product with its own lifecycle.
6. **What promotion measures.**
   - Tokens; billed cost; quota tokens; cost per successful task.
   - **Recommendation: billed cost per successful task, with no drop in success**, and quota
     tokens shown beside it. Token counts alone can hide a cost rise.
7. **Trial split.**
   - Per call; per session; per task.
   - **Recommendation: per session.** Per call is confounded by the cache and later turns.
8. **Fix the operator-edit gap now or with stage 2.**
   - **Recommendation: now, on the security queue.** It is a fail-closed gap today, apart from
     grafts.

# pi loop adapter + gate extension Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Two joins between openDaisugi and pi (`@earendil-works/pi-coding-agent`): coppice can
drive `pi --mode rpc` as a supervision-grade headless pane, and every pi tool call passes the
resident gate through a native in-process extension, fail-closed, whether pi runs under coppice,
under Herdr, or bare.

**Architecture:** A TypeScript extension (`index.ts`) answers pi's `tool_call` event by asking the
resident gate over its Unix socket — the same socket and wire shape `gate_client.py` already uses
(bare `opendaisugi.gate` argparse flags, not a `daisugi` subcommand line) — and answers
`session_start`/`agent_start`/`agent_end`/`agent_settled` by firing a best-effort
`daisugi hook report` over the same socket. `hook.py`'s tool classification gains a `fmt="pi"`
mode so pi's lowercase tool names (`bash`/`read`/`write`/`edit`) map the same way Claude's
TitleCase names do, and any other pi tool name becomes an MCP-style call the envelope's
`mcp_allowlist` can name — never an unconditional deny nothing can override. `gate.py` threads
that `fmt` through to the classifier and joins `"pi"` to `EXIT_CODE_FORMATS`, a set shared with
plan-05 (OpenCode), so `_outcome()` gives pi's extension the same exit-2-on-deny contract Claude
Code gets (pi's extension reads `exit_code` directly and has no stdout JSON channel to parse) —
without a second, divergent rewrite of the same function. A Go adapter
(`harness/coppice/internal/adapters/pi`) drives `pi --mode rpc`'s
JSONL protocol, turning its lifecycle events into `PaneStateEvent`s and its
`extension_ui_request` dialogs into a coppice `blocked` ask.

**Tech Stack:** Python 3.12 (hook.py/gate.py/install.py/cli.py/modules.py, stdlib), TypeScript run
directly by Node 22 (`--experimental-strip-types`, no build step, no npm install required for
tests or for pi itself since pi hot-reloads `.ts` extensions natively), Go 1.25
(`harness/coppice`).

**Spec:** `docs/plans/2026-09-08-workshop/spec-04-pi.md` (binding requirements) and
`docs/plans/2026-09-08-workshop/00-master-spec.md` §3.1, §3.4, §3.6, §5.2 (contracts this plan
implements against).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 1, 2, 3 (with the `_outcome` swap ruled below) and 7 are ready to build now** — the Node
check, the pin file, and the pi classification change are verified sound against the real
sources: `switch_session` really does resume by `sessionPath`, not by the bare `sessionId`
(https://pi.dev/docs/latest/rpc, "Commands"), `prompt` really does require `streamingBehavior`
while streaming, `confirm` really does carry `title` AND `message` with no field named `prompt`,
LF is really the only record delimiter, and pi's own fail-safe **"`tool_call` errors block the
tool (fail-safe)"** is verbatim at
`raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md:2931`
(the rendered docs site summarises this wrongly; the plan is right and the pin file is the
record). `gate.py`'s `evaluate_call` really does drop `fmt` on the floor, so the classification
change really is dead code without the gate.py half. The defects cluster in Tasks 4, 5, 6, 8.

- **BLOCKER (cross-plan ruling 1) — Task 3 Step 6: land plan-05's `_outcome()`, not this
  plan's `if fmt == "pi":` branch.** Both plans independently rewrite `gate.py:459-519` to fix
  the same real fail-open (`_outcome` gives exit-2-on-deny only to `fmt == "claude"`; every
  other format returns `exit_code 0`, and `stdout_for_format` (`hook.py:49-73`) has no `pi` or
  `opencode` case so it falls through to `{"continue": true}` — a deny fails open on *both*
  channels). **This plan lands the fix, once, in this task; plan-05 depends on it and adds no
  branch.** Delete this task's `if fmt == "pi":` block entirely and use plan-05 Task 5 Step 3-4's
  code verbatim, with the set widened to all three at once:
  `EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})` in `hook.py` after
  `stdout_for_format`, and `_outcome`'s first branch keyed on `if fmt in EXIT_CODE_FORMATS:`
  with the `hookSpecificOutput` body kept behind an inner `if fmt == "claude":`. Naming
  `opencode` before its plugin exists is inert (nothing sends `--format opencode` until plan-05)
  and it removes the merge conflict completely. Add `tests/test_hook_format_pi.py` assertions on
  membership, not on a `"pi"`-specific branch. If plan-05 lands first for any reason, it lands
  the identical three-element set and this task asserts membership instead — whichever lands
  first lands the whole set; the second adds nothing. — applied in Task 3 step 6
  (`EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})`, seeded with all three at once).
- **BLOCKER — Task 3 Step 6: the reconciled `_outcome()` still fails open for every format
  outside the two known sets.** `--format` is an unrestricted string (`gate.py:1019`, no
  `choices=`) and `cli.py:570` advertises `codex` as a value, so `--format codex` — or any typo —
  reaches `_outcome`'s tail, calls `stdout_for_format(fmt, block=True, ...)`, gets
  `{"continue": true}` back, and returns `exit_code 0`: a deny that reads as an allow. Fixing
  only `pi` and `opencode` fixes two instances of a class. Fix: after the `EXIT_CODE_FORMATS`
  branch, add `STDOUT_BLOCK_FORMATS = frozenset({"hermes", "openclaw"})` and make any `fmt` in
  neither set return `GateOutcome(stdout="", stderr="openDaisugi gate: DENIED — unknown host
  format 'X'. Use --format claude, pi, opencode, hermes, or openclaw.", exit_code=2)`. Put the
  guard in `_outcome` only — **not** in `stdout_for_format`, whose passive-capture caller
  `record_and_contract` must keep allowing on any format, and whose
  `tests/test_hook_format.py:40` (`test_unknown_format_defaults_to_claude_contract`) must stay
  green. Codex is not affected in production today: `install.py:970` builds the Codex hook from
  `gate_settings_json`'s default `fmt="claude"` (verified), so this closes a latent hole, not a
  live one. — applied in Task 3 step 6 (`STDOUT_BLOCK_FORMATS = frozenset({"hermes",
  "openclaw"})` plus the unconditional tail-deny branch in `_outcome`); regression tests
  `test_unknown_format_denies_instead_of_falling_through_to_continue_true` and
  `test_unknown_format_denies_even_when_the_underlying_decision_would_allow`.
- **BLOCKER (cross-plan ruling 2) — Task 8: the `internal/pane` types this plan invents will not
  compile.** The plan header calls `StartOpts`/`Proc`/`Event`/`EventKind` "this plan's
  invention" because plan-02 had not landed. It has: `plan-02-coppice-server.md` ≈3626-3681
  defines them, and `Ask`/`PaneStateEvent` live in `internal/state` at ≈1503-1520. **plan-02's
  shape is canonical, verbatim.** Delete this task's assumed `pane` block, import plan-02's, and
  read plan-02's own claude adapter (≈9174) and codex adapter (≈9628) for how
  `Start(ctx, o, g *Grid)` and `EvText` coexist. The deltas that force a rewrite of Task 8's
  code and tests: `Proc` is a **6-method interface** (`Prompt`/`Steer`/`WriteStdin`/`Events`/
  `SessionID`/`Stop`), so it is `proc.Prompt(text)`, never `a.Prompt(proc, text)` — this task's
  four `func (Adapter) Prompt/Steer/Events/SessionID(p pane.Proc, …)` methods all move onto the
  Proc value; `Adapter` is `Name()` + `Start(ctx, StartOpts, *Grid) (Proc, error)` only (this
  task defines no `Name()` at all, and passes no `*Grid`); fields are `Cwd`/`Argv`/`Resume`, not
  `CWD`/`ExtraArgv`/`ResumeID`; constants are `EvText`/`EvTool`/`EvState`/`EvEnd`/`EvError`, not
  `EventText`/…; `Ask` is `state.Ask`, not `pane.Ask`. **One amendment plan-02 owes this plan and
  plan-05:** `pane.Event` has no `Ask` field and `Event.State` is a plain string, so a `blocked`
  event cannot carry the ask that master §3.1 requires on it and that spec-04
  (`extension_ui_request` ⇒ blocked with `Ask{id, tool, summary, deadline}`) and spec-05
  (`permission.asked` ⇒ blocked with the permission id and title) both need. plan-02 adds
  `Ask *state.Ask` to `pane.Event`, keeping `State string`. No import cycle: `internal/state`
  imports only `internal/proto` (checked across plan-02), so `pane` → `state` is safe.
  — applied in Task 8, with one correction found while applying it: `Ask`/`PaneStateEvent` are
  verified (by reading plan-02's own Task 2 declaration and its `pumpAdapter`/`internal/state`
  code, both of which construct/import `proto.PaneStateEvent` and `proto.Ask` directly) to live
  in **`internal/proto`**, not `internal/state` as cited above — `internal/state` imports
  `proto.Ask` rather than declaring its own. Task 8 uses `proto.Ask`/`proto.PaneStateEvent`
  throughout and states the amendment (`Ask *proto.Ask` on `pane.Event`) against that package.
- **BLOCKER — Task 4 Step 3: `sendGateRequest` trusts any socket at the path, so a planted
  socket is a universal allow.** `gate_client.py:33-50` (`_socket_is_trustworthy`) exists for
  exactly this: it `os.lstat`s the path (never `exists()`, which follows symlinks), and refuses
  anything that is not a socket, not owned by `os.getuid()`, or not mode `0600` — its docstring
  names the attack ("a rogue process could otherwise plant a symlink to a socket it controls").
  `index.ts` does none of that, so the TypeScript client is strictly weaker than the Python one
  on the same socket: any local process that wins the path answers `{"exit_code":0}` and every
  tool call is allowed. Fix: add
  `function gateSocketTrustworthy(p: string): boolean` using `fs.lstatSync(p)` —
  `st.isSocket() && st.uid === process.getuid() && (st.mode & 0o777) === 0o600`, wrapped in
  try/catch returning false — and call it before `net.createConnection`; a false verdict returns
  the same `UNREACHABLE_REASON` block. Add the test named for the failure:
  `test_extension_blocks_when_the_socket_is_not_a_private_socket_we_own` (chmod the fake gate
  socket 0666 → block). Plan-05's `askGate` needs the identical helper; write it once here in
  shape and copy it there. — applied in Task 4 step 3 (`gateSocketTrustworthy`, called from
  `sendGateRequest` before `net.createConnection`, and from `reportState` in Task 5 for the same
  reason) plus the named test in step 1.
- **BLOCKER — Tasks 4 and 6: `configMode()` promotes an agent-writable file above the one file
  the gate refuses to trust.** `resolve_gate_mode` (`gate.py:51-70`) is explicit: "the installed
  hook command passes `--mode` (the one thing the agent cannot rewrite), and `config.yaml` is
  user-writable — so config must never be able to override the flag, especially to flip an
  installed `enforce` down to `shadow`." This plan puts the mode in
  `~/.pi/agent/extensions/daisugi-gate/daisugi-gate.config.json`, a file at least as writable as
  `config.yaml`, and sends it as `--mode` — which *wins* over `config.yaml`. An agent that can
  write one line (`{"mode":"shadow"}`) into its own extension directory turns every deny into an
  allow. Fix: **omit `--mode` from the argv entirely** and let `resolve_gate_mode(None, root)`
  read `config.yaml` server-side, which is the same trust level without adding a higher-priority
  override. Blast radius, all authoritative: delete `configMode()` from Task 4's exported
  interface (and Task 5's "must not redefine" note), delete
  `daisugi-gate.config.json.example` and Task 6's per-install config write; the argv in
  `test_argv_sent_is_the_bare_flags_gate_client_contract_not_a_cli_subcommand` becomes
  `["--format", "pi", "--root", str(tmp_path)]`; delete `_isolated_harness_with_config`,
  `test_shadow_config_puts_shadow_mode_on_the_wire`, `test_enforce_flag_reaches_the_config_file`
  and `test_mode_change_rewrites_only_the_config_file`;
  `test_unreachable_blocks_regardless_of_configured_mode` survives, renamed
  `test_unreachable_blocks_regardless_of_gate_mode` and run without the config harness. Task 6's
  `install_harness_extension` keeps its `enforce` parameter only if it writes
  `config.gate_mode`; simplest is to drop the parameter and print the flip command instead.
  — applied in Task 4 step 3 (no `--mode` in the argv, `configMode()` deleted, the
  `daisugi-gate.config.json.example` file deleted) and Task 6 (the `enforce` parameter dropped
  from `install_harness_extension`/`_install_pi_extension`; the CLI prints the `gate_mode:
  enforce` flip instruction instead). Task 4's argv test renamed
  `test_allow_and_argv_has_no_mode_but_has_verify_timeout` and asserts `"--mode" not in argv`.
- **SHOULD-FIX — Task 6: the install copy states two things that are false.** (a) "pi hot-reloads
  extensions from this directory -- no restart needed" — extensions.md:7 says extensions in
  auto-discovered locations "can be hot-reloaded with `/reload`", a manual slash command, and
  `--mode rpc` has no interactive slash commands at all. Say: restart pi, or run `/reload` in an
  interactive session. (b) With `--mode` omitted per the ruling above, a fresh install resolves
  to **shadow** (`resolve_gate_mode` returns `"shadow"` on a missing or unreadable
  `config.yaml`), so spec-04's line "or every tool call will block" is false as written. The
  honest STE100 line: "pi asks the gate in-process. The gate only watches until you set
  `gate_mode: enforce`. Start it with `daisugi start`." — applied verbatim in Task 6 step 5's
  CLI copy, with the restart/`/reload` line and pinned in step 6's test
  (`test_cli_install_harness_pi_prints_the_honest_restart_and_mode_lines`).
- **SHOULD-FIX — Task 4 Step 3: `package.json` and its `pi-package` keyword are unverified and
  unnecessary.** `pi-package` appears nowhere in extensions.md, and auto-discovery is by path —
  `~/.pi/agent/extensions/*/index.ts` (extensions.md:118), which the install already satisfies.
  `test_package_json_carries_the_pi_package_manifest_shape` pins an unverified fact as a passing
  assertion. Fix: either drop `package.json` and that test, or make it a discovery step in
  Task 2 that fetches pi's package/publishing doc and records the keyword with its source URL
  before the test asserts it. — applied by dropping `package.json` and its test entirely (Task 4);
  the extension directory now ships only `index.ts`, and Task 6's install/uninstall/idempotence
  tests check that one file.
- **SHOULD-FIX — Task 4: no `--verify-timeout` on the wire, so the gate outlives the client's
  budget.** The extension gives up at 5000 ms; the gate's default inner verify budget is 10.0 s
  (`gate.py:1021`). Every verify slower than 5 s becomes an "unreachable" block instead of the
  verdict the gate was about to produce — fail-closed, but wrong-reasoned and unnecessary. Fix:
  append `"--verify-timeout", "4"` to the argv, mirroring `gate_settings_json`'s own
  `inner = min(verify_timeout_s, max(1.0, hook_timeout_s - 5.0))` discipline. — applied in
  Task 4 step 3's argv construction (`["--format", "pi", "--root", …, "--verify-timeout", "4"]`)
  and pinned in step 1's argv assertion.
- **SHOULD-FIX — Task 5: `reportState` puts a pane id in the `session_id` field.** The fallback
  chain is `env.COPPICE_PANE || env.OPENDAISUGI_SESSION_ID || pi-<pid>`. Master §3.1 defines
  `session_id` as the daisugi session id (tree file stem) and carries the pane separately in
  `pane`; `COPPICE_PANE` is the pane. Fix: `session_id` is
  `env.OPENDAISUGI_SESSION_ID || "pi-" + process.pid`, and the pane comes from `COPPICE_PANE`
  (which plan-01's `report_state` reads from the env anyway). — applied in Task 5 step 3
  (`reportState`'s `session_id`/`pane` split) and pinned by
  `test_report_state_sends_hook_report_shape_with_session_id_pane_split`.
- **SHOULD-FIX — Task 4 Step 1: two fail-closed cases are untested.** There is no case for a
  reply whose `exit_code` is neither 0 nor 2 (the code blocks — prove it), and no garbage-reply
  case for `askGate` (only `reportState` has one). Add
  `test_extension_blocks_on_an_exit_code_it_does_not_recognise` (exit 1) and
  `test_extension_blocks_on_a_malformed_gate_reply`. — applied verbatim as both named tests in
  Task 4 step 1.
- **NOTE — `_escape_outcome`'s shadow branch hardcodes the claude body.** `gate.py` ≈1052 returns
  `stdout_for_format("claude", block=False)` regardless of `fmt`. Harmless for pi (the extension
  reads only `exit_code`) but it is the same class of format-blindness this task is fixing;
  worth one line while the function is open. — applied in Task 3 step 6: `_escape_outcome`
  gains an optional `fmt: str = "claude"` parameter, and a new `_fmt_from_argv(argv)` helper
  (mirroring `gate_client.py`'s `_root_from_argv` scanning pattern) recovers it for `run_argv`'s
  two call sites, since full argparse parsing has already failed by the time either is reached
  and `args.fmt` was never bound.
- **NOTE — Task 5's `["hook","report"]` argv is correct and depends on plan-01.** Today
  `gate_server._Handler` hands everything to `run_argv`, whose parser rejects the positional
  `hook` and returns a fail-closed exit-2 deny — so state reporting is inert until plan-01 widens
  the gate.sock dispatch, which plan-01 does (`plan-01-floor-contracts-and-herdr-hook.md` ≈263).
  The plan's own "if spec-01's dispatch has not landed yet" comment is accurate. Order Task 5
  after plan-01 or accept a no-op. — confirmed accurate as written; Task 5's own explanatory note
  already states this dependency and its fail-closed consequence (an honest `unknown` state, not
  a wrong `idle`), so no code change was needed, only this acknowledgment.
- **NOTE — Task 7's honesty tag lands in a different task from Task 6's install code.** Master
  §3.5 wants the tag in the same task that adds the code. Harmless here because Task 7 follows
  immediately, but do not let it slip further. — acknowledged; left as-is (Task 7 immediately
  follows Task 6 in this plan's execution order), no structural change made.

Re-review 2026-09-08: 13 of 13 verified applied; open: none.

## Global Constraints

(Copied verbatim from `00-master-spec.md` §4 — binds every task below.)

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo. (This plan's tests use
  pytest's own `tmp_path` fixture for Unix-socket paths — the same pattern
  `tests/test_gate_resident.py` already uses — which is short-lived, auto-cleaned test scratch,
  not the long-lived worktree/model-download scratch the house rule targets. A Unix socket path
  has a hard ~108-byte kernel limit, so it must live under a short path; `tmp_path` already is one.)
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

**Plan-04-specific additions to the above:**

- **This plan's own Files-list deviations from spec-04** (needed for correctness, verified against
  the real source — see the rulings in the final report): `src/opendaisugi/gate.py`,
  `pyproject.toml`, `src/opendaisugi/cli.py` are all Modified here even though spec-04's Files
  block does not name them.
- **`harness/coppice/internal/pane` types are normative, verbatim from
  `plan-02-coppice-server.md` (its landed Task 6, ≈3612-3673, and Task 2's `internal/proto`,
  ≈1503-1524) — not this plan's invention.** `pane.Proc` is a 6-method interface
  (`Prompt`/`Steer`/`WriteStdin`/`Events`/`SessionID`/`Stop`) implemented on the process value
  itself, never called as `adapter.Prompt(proc, text)`. `pane.Adapter` is `Name() string` plus
  `Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)` only. `StartOpts` fields are
  `Cwd`/`Env`/`Argv`/`Resume`/`Sock`/`PaneID`. `EventKind` constants are
  `EvText`/`EvTool`/`EvState`/`EvEnd`/`EvError`. `Ask` and `PaneStateEvent` live in
  `internal/proto` (verified by reading plan-02's own Task 2 and the `pumpAdapter` code in its
  Task 14, ≈8655-8686, which constructs `proto.PaneStateEvent{...}` and `proto.SrcHeadless`
  directly) — **not** `internal/state`, correcting an imprecise citation from an earlier review
  pass. Task 8 fills the `pi.go` slot plan-02 Task 14 already registers as `adapters.NotBuilt`; it
  does not invent a parallel `adapter.go`.
- **Amendment plan-02 owes this plan and plan-05: `pane.Event` needs an `Ask *proto.Ask` field.**
  The landed `Event` struct (`Kind`, `Text`, `Tool`, `State string`, `Detail`) has nowhere to carry
  the ask a `blocked` state implies, and `pumpAdapter` (`internal/server/panes.go`) does not copy
  one through today. Both spec-04 (`extension_ui_request` ⇒ blocked with `Ask{id, tool, summary,
  deadline}`) and spec-05 (`permission.asked` ⇒ blocked with the permission id/title) need this.
  Task 8 sets the field on every `pane.Event` it emits for a blocked state, on the assumption this
  amendment lands; until it does, `pumpAdapter` silently drops it (a state without an ask, not a
  wrong state — see master §3.6's fail-closed framing: an ask a client never sees just means the
  operator has to look at the pane directly, not that anything is allowed that should not be).
- **Node 22** is required for Tasks 4 and 5's tests (`--experimental-strip-types`, confirmed
  working on the reference dev box at Node v22.22.2) and is a soft prerequisite gated by Task 1 —
  those tests skip with a named reason when Node is absent or too old; nothing else in this plan
  needs it.
- **`EXIT_CODE_FORMATS` lands once, in this plan, with all three known members at once:**
  `frozenset({"claude", "pi", "opencode"})`. Naming `"opencode"` before its plugin exists is inert
  (nothing sends `--format opencode` until `plan-05-opencode.md` builds it) and removes the merge
  conflict between the two plans outright — whichever plan lands first lands the whole set; the
  second asserts membership and changes nothing. Neither plan may reintroduce a literal
  `fmt == "claude"` check in `_outcome()`, and both must keep a `STDOUT_BLOCK_FORMATS =
  frozenset({"hermes", "openclaw"})` tail guard so a format in neither set denies (exit 2) instead
  of silently reaching `stdout_for_format`'s `{"continue": true}` default.

---

### Task 1: Prerequisite check — Node ≥22 for the pi extension's TypeScript tests

**Files:**
- Create: `tests/harness_pi/__init__.py` (empty — makes the directory a normal test package)
- Create: `tests/harness_pi/_node_check.py`
- Test: `tests/harness_pi/test_node_check.py`

**Interfaces:**
- Produces: `node_status(min_major: int = 22) -> tuple[bool, str]` — `(True, "v22.22.2")` when a
  usable Node is on PATH, else `(False, "<reason a human can act on>")`. Tasks 4 and 5 import this
  and build `pytest.mark.skipif(not node_status()[0], reason=node_status()[1])`.

- [ ] **Step 1: Write the failing test**

```python
# tests/harness_pi/test_node_check.py
from __future__ import annotations

import subprocess

from tests.harness_pi._node_check import node_status


def test_reports_ok_when_node_version_meets_minimum(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/node")
    monkeypatch.setattr(
        "subprocess.run",
        lambda *a, **k: subprocess.CompletedProcess(a[0], 0, stdout="v22.22.2\n"),
    )
    ok, detail = node_status(min_major=22)
    assert ok is True
    assert detail == "v22.22.2"


def test_reports_absent_when_node_not_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    ok, detail = node_status()
    assert ok is False
    assert "not on PATH" in detail


def test_reports_too_old_when_major_below_minimum(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/node")
    monkeypatch.setattr(
        "subprocess.run",
        lambda *a, **k: subprocess.CompletedProcess(a[0], 0, stdout="v18.19.0\n"),
    )
    ok, detail = node_status(min_major=22)
    assert ok is False
    assert "older than required 22" in detail


def test_this_box_actually_has_a_usable_node():
    """Not mocked: confirms Tasks 4/5's tests will actually RUN here, not
    silently skip. If this fails on a real dev box, install Node 22+."""
    ok, detail = node_status()
    assert ok, f"Node 22+ required for pi extension tests: {detail}"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/harness_pi/test_node_check.py -q`
Expected: FAIL — `tests.harness_pi._node_check` does not exist yet.

- [ ] **Step 3: Write minimal implementation**

```python
# tests/harness_pi/__init__.py
```
(empty file)

```python
# tests/harness_pi/_node_check.py
"""Shared Node-availability check for the pi extension's TypeScript tests.

Node runs pi's own extension source directly (`--experimental-strip-types`,
no transpile step) so the TS logic tests in this test package need a real
Node 22+ on PATH. This is test infrastructure, not part of the shipped
package — it lives under tests/, never under src/opendaisugi/.
"""

from __future__ import annotations

import shutil
import subprocess


def node_status(min_major: int = 22) -> tuple[bool, str]:
    """Return (True, "vX.Y.Z") if a usable Node is on PATH, else (False, reason)."""
    exe = shutil.which("node")
    if exe is None:
        return False, f"node not on PATH (need Node >= {min_major} for the pi extension TS tests)"
    try:
        out = subprocess.run(
            [exe, "--version"], capture_output=True, text=True, timeout=5, check=True
        ).stdout.strip()
    except Exception as exc:  # noqa: BLE001 — any failure here is "not usable", not a crash
        return False, f"node --version failed: {exc}"
    version = out.lstrip("v")
    try:
        major = int(version.split(".")[0])
    except ValueError:
        return False, f"could not parse node --version output {out!r}"
    if major < min_major:
        return False, (
            f"node {out} is older than required {min_major} "
            "(pi extension TS tests need --experimental-strip-types)"
        )
    return True, out
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/harness_pi/test_node_check.py -q`
Expected: 4 passed.

- [ ] **Step 5: Commit**

```bash
git add tests/harness_pi/__init__.py tests/harness_pi/_node_check.py tests/harness_pi/test_node_check.py
git commit -m "test(harness_pi): add a Node-22 availability check for the pi extension's TS tests"
```

---

### Task 2: Discovery — pin pi's RPC + extension API facts

**Files:**
- Create: `src/opendaisugi/harness_pi/extension/PINS.md`
- Test: `tests/harness_pi/test_api_pins.py`

**Interfaces:**
- Produces: `PINS.md`, a committed record of exact facts fetched from pi's own docs. Tasks 4, 5,
  and 8 build against these facts and must not silently re-derive them from memory later.

- [ ] **Step 1: Write the failing test**

```python
# tests/harness_pi/test_api_pins.py
from __future__ import annotations

from pathlib import Path

PINS = (
    Path(__file__).parents[2] / "src" / "opendaisugi" / "harness_pi" / "extension" / "PINS.md"
).read_text(encoding="utf-8")


def test_pins_cite_both_source_urls():
    assert (
        "raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md"
        in PINS
    )
    assert (
        "raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/rpc.md" in PINS
    )


def test_pins_record_the_tool_call_return_shape():
    assert "block: true" in PINS
    assert "reason?" in PINS
    assert "terminate?" in PINS
    # pi's OWN fail-safe: a thrown error inside tool_call also blocks, independent
    # of our extension's explicit checks. Load-bearing for Task 4's design.
    assert "tool_call errors block the tool (fail-safe)" in PINS


def test_pins_record_lowercase_builtin_tool_names():
    for name in ("bash", "read", "write", "edit"):
        assert f'"{name}"' in PINS


def test_pins_record_the_rpc_framing_rule():
    assert "LF" in PINS and "only" in PINS  # LF-delimited JSONL, not a generic line reader


def test_pins_record_switch_session_takes_a_file_path_not_a_bare_id():
    assert "sessionPath" in PINS
    assert "sessionFile" in PINS


def test_pins_record_the_dialog_method_set():
    for method in ("select", "confirm", "input", "editor"):
        assert f'"{method}"' in PINS
    # confirm carries title AND message; select/input/editor carry title only —
    # there is no field literally named "prompt" (a spec-04 wording looseness
    # this plan's code must not blindly trust).
    assert "no field is literally named" in PINS
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/harness_pi/test_api_pins.py -q`
Expected: FAIL — `PINS.md` does not exist (FileNotFoundError at collection).

- [ ] **Step 3: Write the pin file**

```markdown
<!-- src/opendaisugi/harness_pi/extension/PINS.md -->
# pi RPC + extension API — pinned facts

Fetched 2026-09-08 directly from pi's own repository (raw markdown, not the rendered docs
site, so this is diffable against a future re-fetch):

- Source A (extension API): https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md
- Source B (RPC protocol): https://raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/rpc.md
- https://pi.dev/docs/latest/rpc renders the same content as Source B (confirmed by fetch,
  200 OK); the raw markdown is what is quoted below since it is what a future re-fetch diffs
  against cleanly.

Do not re-derive any of the following from memory when `index.ts` is next touched. Re-read this
file, or re-fetch the two sources above if this file itself is ever in doubt.

## tool_call handler (Source A, "Tool Events" > "tool_call")

- Fired after `tool_execution_start`, before the tool executes. **Can block.**
- `event.toolName` — built-in tool names are lowercase: `"bash"`, `"read"`, `"write"`, `"edit"`,
  etc. (NOT the TitleCase Claude Code uses for its own built-ins.)
- `event.toolCallId`, `event.input` (mutable in place; not used by this extension).
- Return `{ block: true, reason?: string, terminate?: boolean }` to block. Return `undefined`
  (or nothing) to allow.
- Error Handling section: **"tool_call errors block the tool (fail-safe)"** — pi's own runtime
  treats a THROWN error inside the handler as a block, independent of our extension's own
  explicit return value. Our extension still returns explicit blocks with a real reason rather
  than relying on this, but it is a second line of defense worth knowing about.

## Lifecycle events used for state (Source A, "Session Events" / "Agent Events")

- `session_start {reason: "startup"|"reload"|"new"|"resume"|"fork", previousSessionFile?}`.
- `agent_start {}` — a low-level agent run begins.
- `agent_end {messages, willRetry}` — one low-level run ends; pi MAY still auto-retry,
  auto-compact and retry, or continue with a queued follow-up.
- `agent_settled {}` — the full session-level run has settled; nothing further happens
  automatically. Use this, not `agent_end`, for "the agent is really idle now."
- `extension_error {extensionPath, event, error}` (RPC event stream only, per Source B).

## RPC commands used by the Go adapter (Source B, "Commands")

- Start: `pi --mode rpc [--provider <name>] [--model <pattern>] [--session-dir <path>]
  [--name <name>]`.
- Framing (Source B, "Framing"): **strict JSONL with LF (`\n`) as the only record delimiter.**
  "Do not use generic line readers that treat Unicode separators as newlines" — Node's own
  `readline` module is explicitly named as non-compliant (it also splits on U+2028/U+2029,
  which are valid inside JSON strings). Go's `bufio.Scanner` with the default `ScanLines` split
  function is LF/CRLF-based and does not have this problem.
- `{"id"?, "type":"prompt","message":str,"images"?}` → `{"type":"response","command":"prompt",
  "success":bool}`. **"If the agent is streaming and no `streamingBehavior` is specified, the
  command returns an error."** Valid values: `"steer"` (delivered after the current turn's tool
  calls, before the next LLM call) or `"followUp"` (delivered only once the agent is fully idle).
- `{"type":"steer","message":str}` → `{"type":"response","command":"steer","success":bool}`.
- `{"type":"get_state"}` → `data.sessionId` (a short display id), `data.sessionFile` (the
  session's JSONL path), `data.sessionName`, `data.isStreaming`, ...
- `{"type":"switch_session","sessionPath":str}` → `data.cancelled: bool`. **Resumes BY FILE
  PATH** (`sessionPath`, from `get_state`'s `sessionFile`) — NOT by the bare `sessionId`. Feeding
  it the short id instead silently fails the resume.

## Extension UI protocol (Source B, "Extension UI Protocol")

- Request shape: `{"type":"extension_ui_request","id":str,"method":"select"|"confirm"|"input"
  |"editor"|"notify"|"setStatus"|"setWidget"|"setTitle"|"set_editor_text", ...}`.
- Only the DIALOG methods (`select`, `confirm`, `input`, `editor`) expect an
  `extension_ui_response`; the rest (`notify`, `setStatus`, `setWidget`, `setTitle`,
  `set_editor_text`) are fire-and-forget and get no response.
- `select`/`input`/`editor` carry a `title` field. `confirm` carries BOTH `title` AND `message`.
  **No field is literally named `prompt`** in any of these — a summary built from
  `title || message` covers every dialog method; do not look for a `prompt` field.
- Response shape: `{"type":"extension_ui_response","id":str, value|confirmed|cancelled:true}`.

## Error Handling / Mode Behavior (Source A)

- "Extension errors are logged, agent continues" (applies to non-`tool_call` handlers).
- `ctx.mode` is `"rpc"` and `ctx.hasUI` is `true` in RPC mode — dialog/notify methods work over
  the wire even though there is no terminal.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/harness_pi/test_api_pins.py -q`
Expected: 6 passed.

- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/harness_pi/extension/PINS.md tests/harness_pi/test_api_pins.py
git commit -m "docs(harness_pi): pin pi's RPC and extension API facts before building against them"
```

---

### Task 3: pi tool classification reaches the real gate, and `_outcome()` stops failing open

**Files:**
- Modify: `src/opendaisugi/hook.py:96-124` (`_TOOL_TYPE_MAP`, `_classify_tool`)
- Modify: `src/opendaisugi/hook.py:159-201` (`_payload_to_record`)
- Modify: `src/opendaisugi/hook.py:49-73` (`EXIT_CODE_FORMATS`, `STDOUT_BLOCK_FORMATS`)
- Modify: `src/opendaisugi/gate.py:35-41` (import), `:262-298` (`evaluate_call`), `:459-519`
  (`_outcome`), `:569` (`_HARNESS_BY_FMT`), `:763-768` (the `evaluate_call(...)` call site inside
  `gate_and_contract`), `:1048-1067` (`_escape_outcome`), `:1082-1096` (`run_argv`'s two
  `_escape_outcome` call sites)
- Test: `tests/test_hook_format_pi.py`

**Why gate.py is touched even though spec-04's Files list does not name it:** spec-04 says pi's
"anything else" tool name becomes "an MCP-style tool ... denied unless the envelope names it."
`evaluate_call` (gate.py:282) calls `_payload_to_record(payload)` with no `fmt` argument, so
without this change hook.py's new pi-aware classification is dead code on the real gate path —
every actual pi tool call would still be classified by the `fmt="claude"` default. Verified by
reading gate.py directly, not inferred.

**Why this task lands the ENTIRE `_outcome()` rewrite once, not a `pi`-only branch (adversarial
review, blocker 1 — cross-plan ruling):** `_outcome` (gate.py:459-519) only gives
exit-code-2-on-deny to `fmt == "claude"`; every other format signals block via stdout JSON
instead, with exit code always 0. A pi (or OpenCode) call the gate DENIES would still exit 0 with
an allow-shaped body — the extension reads `exit_code`, never stdout, so this is a silent
fail-open, not a crash. `docs/plans/2026-09-08-workshop/plan-05-opencode.md` Task 5 fixes the
exact same function for `--format opencode`. Rather than landing two competing patches to the
same 60 lines, **this task lands the fix once, seeded with all three known members at once**:
`EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})`. Naming `"opencode"` here is inert
(nothing sends `--format opencode` until plan-05's plugin exists) and removes the merge conflict
outright: whichever of plan-04/plan-05 lands first lands the whole set and the full rewrite; the
second one runs `grep -n "EXIT_CODE_FORMATS"` (Step 6 below), finds it already there with its own
format already a member, and adds nothing.

**Why a second, unconditional guard is also needed (adversarial review, blocker 2):** the
reconciled `_outcome()` above still fails open for every format OUTSIDE the two known sets.
`--format` is an unrestricted string (gate.py's argparse has no `choices=`) and `cli.py`
advertises `codex` as a value, so `--format codex` — or any typo — would reach `_outcome`'s tail,
call `stdout_for_format(fmt, block=True, ...)`, get back `{"continue": true}`, and return
`exit_code 0`: a deny that reads as an allow. Fixing only the two known sets fixes two instances
of a class, not the class. Fix: `STDOUT_BLOCK_FORMATS = frozenset({"hermes", "openclaw"})`, and
any `fmt` in NEITHER set denies with `exit_code=2` and a reason that names the valid choices.
Codex is not affected in production today (`install.py` builds the Codex hook from
`gate_settings_json`'s default `fmt="claude"`), so this closes a latent hole, not a live one.

**Interfaces:**
- Consumes: `Envelope`, `Permission` (`src/opendaisugi/models.py`, existing).
- Produces:
  - `_classify_tool(name: str, *, fmt: str = "claude") -> str | None`
  - `_payload_to_record(payload: dict[str, Any], *, fmt: str = "claude") -> dict[str, Any] | None`
  - `evaluate_call(payload: Any, envelope: Envelope, *, mode: str = "shadow", verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S, fmt: str = "claude") -> GateDecision`
  - `opendaisugi.hook.EXIT_CODE_FORMATS: frozenset[str]` = `{"claude", "pi", "opencode"}`.
  - `opendaisugi.hook.STDOUT_BLOCK_FORMATS: frozenset[str]` = `{"hermes", "openclaw"}`.
  - `_outcome(decision: GateDecision, fmt: str) -> GateOutcome` — three-way branch:
    `fmt in EXIT_CODE_FORMATS` (exit-code contract), `fmt in STDOUT_BLOCK_FORMATS` (stdout-JSON
    contract), else deny with exit 2 and a reason naming the valid `--format` choices.
  - `_escape_outcome(mode: str, exc: BaseException, fmt: str = "claude") -> GateOutcome` — gains
    an optional `fmt` for its shadow-mode stdout body (see the NOTE this task also applies).

- [ ] **Step 1: Confirm the widened tool-name map cannot break an existing fixture**

Run: `grep -rn '"tool_name":[[:space:]]*"\(bash\|read\|write\|edit\)"' tests/`
Expected: no matches (already confirmed — no existing test uses a lowercase builtin name as its
"unknown tool" fixture). If this now finds a match, stop and inspect it before continuing; do not
widen the map underneath a fixture that relies on `bash`/`read`/`write`/`edit` being unclassified.

Run: `uv run --no-sync pytest tests/test_hook.py tests/test_hook_format.py tests/test_gate.py -q`
Expected: all pass (baseline, before any change in this task).

- [ ] **Step 2: Write the failing test**

```python
# tests/test_hook_format_pi.py
from __future__ import annotations

from opendaisugi.gate import GateDecision, evaluate_call
from opendaisugi.hook import _classify_tool, _payload_to_record
from opendaisugi.models import Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/allowed/**"], **perm_kwargs}
    return Envelope(generated_by="test", task="pi format test", permissions=Permission(**perms))


# --- bash/read/write/edit classify the same way Claude's Bash/Read/Write/Edit do -----------


def test_pi_bash_classifies_as_shell():
    assert _classify_tool("bash", fmt="pi") == "shell"


def test_pi_read_classifies_as_file_read():
    assert _classify_tool("read", fmt="pi") == "file_read"


def test_pi_write_classifies_as_file_write():
    assert _classify_tool("write", fmt="pi") == "file_write"


def test_pi_edit_classifies_as_file_write():
    assert _classify_tool("edit", fmt="pi") == "file_write"


def test_claude_format_is_unaffected_by_the_pi_additions():
    assert _classify_tool("Bash") == "shell"
    assert _classify_tool("Read") == "file_read"
    # lowercase pi names are NOT recognized under the claude default — no cross-format bleed
    assert _classify_tool("bash") is None


def test_payload_to_record_maps_pi_path_field_for_read():
    rec = _payload_to_record({"tool_name": "read", "tool_input": {"path": "/allowed/x"}}, fmt="pi")
    assert rec["step_type"] == "file_read"
    assert rec["path"] == "/allowed/x"


# --- "anything else" becomes MCP-style, not an unconditional deny --------------------------


def test_unknown_pi_tool_becomes_mcp_style_not_dropped():
    rec = _payload_to_record(
        {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}, fmt="pi"
    )
    assert rec is not None, "pi's fallback must not silently drop an unrecognized tool"
    assert rec["step_type"] == "mcp"
    assert rec["mcp_server"] == "pi"
    assert rec["mcp_tool"] == "grep_files"
    assert rec["arguments"] == {"pattern": "TODO"}


def test_same_unknown_tool_under_claude_fmt_is_still_dropped():
    assert _payload_to_record({"tool_name": "grep_files", "tool_input": {}}) is None


def test_unknown_pi_tool_is_denied_when_not_in_mcp_allowlist():
    payload = {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}
    d = evaluate_call(payload, _envelope(), mode="enforce", fmt="pi")
    assert isinstance(d, GateDecision)
    assert d.allow is False
    assert "pi/grep_files" in d.reason or "mcp_allowlist" in d.reason


def test_unknown_pi_tool_is_allowed_once_named_in_mcp_allowlist():
    payload = {"tool_name": "grep_files", "tool_input": {"pattern": "TODO"}}
    env = _envelope(mcp_allowlist=["pi/grep_files"])
    d = evaluate_call(payload, env, mode="enforce", fmt="pi")
    assert d.allow is True


# --- the real socket-facing exit-code contract ----------------------------------------------
# Named for the failure each proves (same convention plan-05's
# tests/test_hook_format_opencode.py uses for the sibling fix): a test that
# would let a bad exit-code format resolve to "allow" is the actual
# regression this file exists to catch.


def test_pi_is_an_exit_code_format():
    from opendaisugi.hook import EXIT_CODE_FORMATS

    assert {"claude", "pi", "opencode"} <= EXIT_CODE_FORMATS


def test_pi_format_denies_via_exit_code_not_json_stdout(tmp_path):
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(), session_id="s1", root=root)
    payload = json.dumps(
        {"tool_name": "bash", "tool_input": {"command": "rm -rf /"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="pi", mode="enforce")
    assert out.exit_code == 2
    assert "DENIED" in out.stderr
    assert out.stdout == ""


def test_pi_format_shadow_mode_never_denies_the_host(tmp_path):
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(), session_id="s1", root=root)
    payload = json.dumps(
        {"tool_name": "bash", "tool_input": {"command": "rm -rf /"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="pi", mode="shadow")
    assert out.exit_code == 0
    assert out.decision.would_deny is True  # observed and logged, just not blocked


def test_pi_format_allows_via_exit_code_zero(tmp_path):
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(), session_id="s1", root=root)
    payload = json.dumps(
        {"tool_name": "read", "tool_input": {"path": "/allowed/x"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="pi", mode="enforce")
    assert out.exit_code == 0
    assert out.stderr == ""
    # stdout is whatever stdout_for_format's default returns for an
    # unrecognized-by-name format (currently {"continue": true}) -- harmless,
    # since askGate (Task 4) reads only exit_code and never parses stdout.


def test_hermes_format_is_unaffected_by_the_pi_fix(tmp_path):
    """Regression guard: hermes keeps its own JSON-carries-the-verdict
    contract (exit_code 0 always) -- this fix must not widen
    EXIT_CODE_FORMATS beyond claude/opencode/pi."""
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(shell=False), session_id="s1", root=root)
    payload = json.dumps(
        {"tool_name": "Bash", "tool_input": {"command": "rm -rf /"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="hermes", mode="enforce")
    assert out.exit_code == 0
    body = json.loads(out.stdout)
    assert body.get("decision") == "block" or body.get("action") == "block"


def test_pi_harness_label_is_intentional_in_the_session_tree(tmp_path):
    from opendaisugi.gate import _HARNESS_BY_FMT

    assert _HARNESS_BY_FMT["pi"] == "pi"


def test_unknown_format_denies_instead_of_falling_through_to_continue_true(tmp_path):
    """The class of bug blocker 2 fixes: --format has no argparse choices=,
    so a typo (or codex's still-latent gap) must not read as an allow."""
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(), session_id="s1", root=root)
    payload = json.dumps(
        {"tool_name": "read", "tool_input": {"path": "/allowed/x"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="codex", mode="enforce")
    assert out.exit_code == 2
    assert "unknown host format" in out.stderr


def test_unknown_format_denies_even_when_the_underlying_decision_would_allow(tmp_path):
    import json

    from opendaisugi.gate import gate_and_contract, register_envelope

    root = tmp_path / "gate"
    register_envelope(_envelope(), session_id="s1", root=root)
    # a call the envelope genuinely admits -- the format guard must still deny,
    # since a name outside both known sets has no safe body to return either way.
    payload = json.dumps(
        {"tool_name": "Read", "tool_input": {"file_path": "/allowed/x"}, "session_id": "s1"}
    ).encode()
    out = gate_and_contract(payload, root=root, fmt="banana", mode="enforce")
    assert out.exit_code == 2
```

- [ ] **Step 3: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_hook_format_pi.py -q`
Expected: FAIL — `_classify_tool("bash", fmt="pi")` returns `None` (no `fmt` parameter exists
yet); `evaluate_call(..., fmt="pi")` raises `TypeError: unexpected keyword argument 'fmt'`.

- [ ] **Step 4: Implement the hook.py classification change**

Edit `src/opendaisugi/hook.py`, replacing the `_TOOL_TYPE_MAP` block (lines 96-109):

```python
# Map common tool names to step types. Mirrors the Claude Code parser's
# _TOOL_TYPE_MAP but accepts variants Hermes/OpenClaw/pi might emit too.
_TOOL_TYPE_MAP: dict[str, str] = {
    "Bash": "shell",
    "shell": "shell",
    "command": "shell",
    "Edit": "file_write",
    "Write": "file_write",
    "MultiEdit": "file_write",
    "Read": "file_read",
    "Glob": "file_read",
    "Grep": "file_read",
    "search": "file_read",
    "WebFetch": "network",
    "WebSearch": "network",
    # pi (spec-04): built-in tool names are lowercase and distinct from every
    # key above -- no collision with Claude's TitleCase or Hermes's
    # "shell"/"command"/"search". Pinned in
    # src/opendaisugi/harness_pi/extension/PINS.md.
    "bash": "shell",
    "read": "file_read",
    "write": "file_write",
    "edit": "file_write",
}
```

Replace `_classify_tool` (lines 112-124):

```python
def _classify_tool(name: str, *, fmt: str = "claude") -> str | None:
    """Return our step type for a host's tool name, or None for unknown.

    Unknown tools are dropped from captures rather than guessed at — keeps
    the post-hoc inference honest, EXCEPT under ``fmt="pi"``: pi has no
    ``mcp__``-prefix convention (its own docs: "No MCP built in"), so a name
    this function does not otherwise recognize is treated as MCP-style
    instead of unconditionally dropped — the gate's deny-by-default
    ``mcp_allowlist`` can then admit it, same as any other MCP tool. Host MCP
    tools follow the ``mcp__<server>__<tool>`` convention under every other
    format and classify as ``mcp`` regardless of ``fmt``.
    """
    if name.startswith("mcp__"):
        return "mcp"
    mapped = _TOOL_TYPE_MAP.get(name)
    if mapped is not None:
        return mapped
    if fmt == "pi":
        return "mcp"
    return None
```

Replace `_payload_to_record` (lines 159-201), threading `fmt` through and giving pi's mcp
fallback its `(server, tool)` pair when the name is not `mcp__`-prefixed:

```python
def _payload_to_record(payload: dict[str, Any], *, fmt: str = "claude") -> dict[str, Any] | None:
    """Convert a raw hook payload into a normalized capture record.

    Handles the shapes different hosts emit:
    - Claude Code: ``{tool_name, tool_input, session_id, ...}``
    - Hermes shell-hook: ``{event, tool, args, session_id, ...}``
    - pi (``fmt="pi"``): ``{tool_name, tool_input, session_id, cwd}`` with
      lowercase built-in names; anything not in ``_TOOL_TYPE_MAP`` becomes an
      MCP-style record with ``mcp_server="pi"``.

    Returns ``None`` for payloads that don't carry a recognizable tool
    call. Caller is expected to skip these (still emitting continue:true
    on stdout to keep the host runtime happy).
    """
    tool_name = payload.get("tool_name") or payload.get("tool") or payload.get("name")
    if not tool_name:
        return None
    step_type = _classify_tool(tool_name, fmt=fmt)
    if step_type is None:
        return None
    inp = payload.get("tool_input") or payload.get("args") or payload.get("input") or {}
    record = {
        "captured_at": time.time(),
        "session_id": _safe_session_id(payload.get("session_id")),
        "tool_name": tool_name,
        "step_type": step_type,
    }
    if step_type == "shell":
        record["command"] = inp.get("command") or inp.get("cmd") or ""
    elif step_type in ("file_read", "file_write"):
        record["path"] = inp.get("file_path") or inp.get("path") or inp.get("pattern") or ""
        if step_type == "file_write":
            # Don't store full content — captures are for distillation,
            # not exfil. Hash + length is enough.
            content = inp.get("content") or inp.get("new_string") or ""
            record["content_len"] = len(content)
    elif step_type == "network":
        record["url"] = inp.get("url") or inp.get("query") or ""
    elif step_type == "mcp":
        parsed = _parse_mcp_tool_name(tool_name)
        if parsed is None:
            if fmt == "pi":
                parsed = ("pi", tool_name)
            else:
                return None
        record["mcp_server"], record["mcp_tool"] = parsed
        record["arguments"] = inp if isinstance(inp, dict) else {}
    record.update(join_keys(payload))
    return record
```

- [ ] **Step 5: Run the hook.py tests to verify the classification half passes**

Run: `uv run --no-sync pytest tests/test_hook_format_pi.py -k "classif or payload_to_record or unknown_pi or same_unknown" -q`
Expected: 7 passed (the `evaluate_call`/`gate_and_contract` tests still fail — next step).

- [ ] **Step 6: Implement the gate.py threading + exit-code branch**

Edit `src/opendaisugi/gate.py`. In `evaluate_call` (lines 262-298), add the `fmt` parameter and
thread it to `_payload_to_record`:

```python
def evaluate_call(
    payload: Any,
    envelope: Envelope,
    *,
    mode: str = "shadow",
    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
    fmt: str = "claude",
) -> GateDecision:
    """Decide one raw hook payload against an envelope. Deny-by-default.

    Never raises: malformed payloads, unknown tools, verifier errors, and
    verifier timeouts all come back as deny decisions (allowed-but-flagged
    in shadow mode). ``fmt`` selects the host-specific tool-name
    classification (hook.py's ``_payload_to_record``); it does not change
    verification itself.
    """
    t0 = time.monotonic()
    try:
        if not isinstance(payload, dict):
            return _deny(mode, "hook payload is not a JSON object", t0=t0)
        tool_name = payload.get("tool_name") or payload.get("tool") or payload.get("name")
        if not tool_name:
            return _deny(mode, "no tool name in hook payload", t0=t0)
        record = _payload_to_record(payload, fmt=fmt)
        if record is None:
            return _deny(
                mode,
                f"unrecognized tool {tool_name!r} — not in the gate's "
                "classification map, denied by default",
                tool_name=str(tool_name),
                t0=t0,
            )
        return evaluate_record(
            record,
            envelope,
            mode=mode,
            verify_timeout_s=verify_timeout_s,
        )
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}", t0=t0)
```

**Before touching `_outcome`, check whether plan-05 Task 5 already landed:**

Run: `grep -n "EXIT_CODE_FORMATS" src/opendaisugi/hook.py src/opendaisugi/gate.py`

- **No matches (plan-05 Task 5 has not run yet):** this task creates `EXIT_CODE_FORMATS`,
  `STDOUT_BLOCK_FORMATS`, and the generalized `_outcome()` itself, using the exact shape below,
  seeded with all three known exit-code formats at once so plan-05 adds nothing further to this
  function later. Continue with the code below exactly as written.
- **Matches in both files (plan-05 Task 5 already ran):** plan-05 was written knowing this plan
  would land the identical three-member set, so its own `EXIT_CODE_FORMATS` literal should
  already read `frozenset({"claude", "pi", "opencode"})` — skip the code block below entirely.
  Run `uv run --no-sync pytest tests/test_hook_format_pi.py -k exit_code_format -q` to confirm
  membership and move straight to Step 7.

In `src/opendaisugi/hook.py`, immediately after `stdout_for_format` (after line 73):

```python
# Formats whose host reads the process exit code as the allow/deny signal
# (2 = deny) rather than parsing JSON on stdout. Claude Code's PreToolUse
# convention; pi's gate extension (spec-04) and OpenCode's plugin (spec-05)
# read the same signal from the resident gate's wrapped {"exit_code": …}
# socket reply and never parse stdout. Seeded with all three known
# exit-code formats at once (this task and plan-05-opencode.md Task 5 fix
# the same real fail-open in gate.py's _outcome() together, in one landing,
# rather than as two competing patches to the same function) — naming
# "opencode" here is inert until its plugin exists.
EXIT_CODE_FORMATS = frozenset({"claude", "pi", "opencode"})

# hermes/openclaw signal block via a JSON body on stdout, exit code always
# 0 — the opposite convention from EXIT_CODE_FORMATS. Together the two sets
# are every format gate.py actually speaks; a format in NEITHER (a typo, or
# a host with no wired contract yet, e.g. today's --format codex) must not
# fall through _outcome()'s stdout_for_format default ({"continue": true})
# and read as an allow. See _outcome()'s tail branch.
STDOUT_BLOCK_FORMATS = frozenset({"hermes", "openclaw"})
```

In `src/opendaisugi/gate.py`, add the import (lines 35-41):

```python
from opendaisugi.hook import (
    EXIT_CODE_FORMATS,
    STDOUT_BLOCK_FORMATS,
    _payload_to_record,
    _records_to_steps,
    _safe_session_id,
    join_keys,
    stdout_for_format,
)
```

Replace `_outcome` (lines 459-519):

```python
def _outcome(decision: GateDecision, fmt: str) -> GateOutcome:
    # ``not decision.allow`` rather than ``mode == "enforce" and would_deny``:
    # today the two are equivalent for every decision constructor (`_deny`
    # sets ``allow = (mode == "shadow")``, so mode selects allow directly),
    # but an operator-allowed decision (Task 8, `_maybe_ask`) is the first
    # case where they diverge — ``would_deny`` stays True (the report must
    # still show what enforce would have denied) while ``allow`` is True.
    # ``allow`` is the one field that must drive the host contract.
    deny_now = not decision.allow
    if fmt in EXIT_CODE_FORMATS:
        if deny_now:
            return GateOutcome(
                stdout="",
                stderr=f"openDaisugi gate: DENIED — {decision.reason}",
                exit_code=2,
                decision=decision,
            )
        if decision.updated_input:
            if fmt == "claude":
                # The operator edited the call before allowing it (Task 8):
                # Claude Code's PreToolUse contract for a modified-but-allowed
                # call is hookSpecificOutput.updatedInput, not plain
                # {"continue":true}.
                stdout = json.dumps(
                    {
                        "hookSpecificOutput": {
                            "hookEventName": "PreToolUse",
                            "permissionDecision": "allow",
                            "permissionDecisionReason": decision.reason,
                            "updatedInput": decision.updated_input,
                        }
                    }
                )
                return GateOutcome(stdout=stdout, stderr="", exit_code=0, decision=decision)
            # No channel to carry an operator edit through an exit-code-only
            # contract either — deny fail-closed rather than silently
            # running the original (already-denied) input.
            return GateOutcome(
                stdout="",
                stderr=(
                    f"openDaisugi gate: DENIED — {decision.reason} — operator edit cannot be "
                    f"carried on the {fmt!r} format (no updatedInput channel); denied "
                    "fail-closed rather than running the original input"
                ),
                exit_code=2,
                decision=decision,
            )
        return GateOutcome(
            stdout=stdout_for_format(fmt, block=False),
            stderr="",
            exit_code=0,
            decision=decision,
        )
    if fmt in STDOUT_BLOCK_FORMATS:
        if decision.updated_input:
            # An operator edited the call before allowing it (Task 8), but
            # only the claude contract (hookSpecificOutput.updatedInput,
            # above) has a channel to carry an edit through to the host.
            # Emitting a plain allow here would silently drop the edit and
            # let the ORIGINAL (denied) input run instead of what the
            # operator actually approved — deny fail-closed instead.
            return GateOutcome(
                stdout=stdout_for_format(
                    fmt,
                    block=True,
                    reason=f"{decision.reason} — operator edit cannot be carried on the "
                    f"{fmt!r} format (no updatedInput channel); denied fail-closed rather "
                    "than running the original input",
                ),
                stderr="",
                exit_code=0,
                decision=decision,
            )
        return GateOutcome(
            stdout=stdout_for_format(fmt, block=deny_now, reason=decision.reason),
            stderr="",
            exit_code=0,
            decision=decision,
        )
    # fmt is in neither set: an unrestricted --format string (no argparse
    # choices=) reached here. stdout_for_format's default is
    # {"continue": true} at exit 0 -- an allow body for what may well be a
    # real deny. Fail closed instead of guessing which way an unknown
    # format's host reads a body it was never taught to speak.
    return GateOutcome(
        stdout="",
        stderr=(
            f"openDaisugi gate: DENIED — unknown host format {fmt!r}. "
            "Use --format claude, pi, opencode, hermes, or openclaw."
        ),
        exit_code=2,
        decision=decision,
    )
```

Replace `_escape_outcome` (lines 1048-1067) — a NOTE from the adversarial review, applied while
this function is already open: its shadow branch hardcoded `"claude"` regardless of the format
the failing argv intended. `run_argv`'s two call sites reach `_escape_outcome` only when
`_build_parser().parse_args(argv)` itself raises (a malformed flag), which is BEFORE `args.fmt`
exists — so recovering `fmt` needs a manual scan of the raw argv, mirroring `gate_client.py`'s
own `_root_from_argv` pattern:

```python
def _fmt_from_argv(argv: list[str]) -> str:
    """Best-effort --format recovery for the escape path, where full argparse
    parsing already failed and args.fmt was never bound. Defaults to
    "claude" (today's existing behavior) when absent or unparseable."""
    for i, a in enumerate(argv):
        if a == "--format" and i + 1 < len(argv):
            return argv[i + 1]
        if a.startswith("--format="):
            return a.split("=", 1)[1]
    return "claude"


def _escape_outcome(mode: str, exc: BaseException, fmt: str = "claude") -> "GateOutcome":
    """Build the fail-closed GateOutcome for an escape from run_argv's try.

    enforce denies (exit 2); shadow has nothing to protect, so it allows —
    the same posture main()'s own try/except already used. ``fmt`` shapes
    only the shadow-mode stdout body (enforce's body is always empty).
    """
    t0 = time.monotonic()
    if mode == "enforce":
        return GateOutcome(
            stdout="",
            stderr=f"openDaisugi gate: DENIED (fail-closed on error): {exc}",
            exit_code=2,
            decision=_deny("enforce", f"gate escape: {exc}", t0=t0),
        )
    return GateOutcome(
        stdout=stdout_for_format(fmt, block=False),
        stderr="",
        exit_code=0,
        decision=_deny("shadow", f"gate escape: {exc}", t0=t0),
    )
```

Update `run_argv`'s two `_escape_outcome` call sites (lines 1082-1096) to pass the recovered
`fmt`:

```python
    except SystemExit as exc:
        if exc.code == 0:  # --help, --version, etc.: a real, intentional exit
            raise
        return _escape_outcome(mode, exc, fmt=_fmt_from_argv(argv))
    except BaseException as exc:  # noqa: BLE001 — deny-by-default on any escape
        return _escape_outcome(mode, exc, fmt=_fmt_from_argv(argv))
```

Add `"pi": "pi"` to `_HARNESS_BY_FMT` (line 569):

```python
_HARNESS_BY_FMT = {
    "claude": "claude-code",
    "codex": "codex",
    "hermes": "hermes",
    "openclaw": "openclaw",
    "pi": "pi",
}
```

Update the `evaluate_call(...)` call site inside `gate_and_contract` (lines 763-768) to pass
`fmt`:

```python
        else:
            decision = evaluate_call(
                payload,
                envelope,
                mode=mode,
                verify_timeout_s=verify_timeout_s,
                fmt=fmt,
            )
```

- [ ] **Step 7: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/test_hook_format_pi.py -q`
Expected: 18 passed.

- [ ] **Step 8: Run the full existing hook/gate suite to confirm no regression**

Run: `uv run --no-sync pytest tests/test_hook.py tests/test_hook_format.py tests/test_gate.py tests/test_gate_mode.py tests/test_gate_resident.py tests/test_gate_tree.py -q`
Expected: all pass, same counts as Step 1's baseline plus the new file. If
`tests/test_hook_format_opencode.py` also exists on disk (plan-05 already landed), run it too and
confirm it still passes unchanged — this task's `EXIT_CODE_FORMATS` widening must not touch
opencode's behavior, only add pi's.

- [ ] **Step 9: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/hook.py src/opendaisugi/gate.py tests/test_hook_format_pi.py`
Expected: clean.

- [ ] **Step 10: Commit**

```bash
git add src/opendaisugi/hook.py src/opendaisugi/gate.py tests/test_hook_format_pi.py
git commit -m "feat(gate): classify pi's tool calls through the real gate, not just the extension's own local check"
```

---

### Task 4: TypeScript extension — askGate and the tool_call handler

**Files:**
- Create: `src/opendaisugi/harness_pi/extension/index.ts`
- Create: `tests/harness_pi/run_tool_call.mjs`
- Create: `tests/harness_pi/test_extension_block_logic.py`

**No `package.json` (adversarial review, should-fix):** pi auto-discovers extensions by PATH
(`~/.pi/agent/extensions/*/index.ts`), which Task 6's install already satisfies. The
`pi-package`/`keywords` manifest convention appears nowhere in the fetched extensions.md and was
an unverified fact this plan should not have pinned as a passing test. Dropped; if pi's package
system ever needs a manifest, that is a discovery step for a later spec, sourced before it is
asserted.

**No `--mode` on the wire, and no `configMode()` (adversarial review, blocker — the install-time
config file this plan almost shipped would have let an agent-writable file outrank the one flag
the operator's installed hook command cannot rewrite):** `resolve_gate_mode` (`gate.py:51-70`)
is explicit that `--mode`, when present, always wins over `config.yaml`, specifically so an
agent cannot flip an installed `enforce` down to `shadow` by editing a config file it can reach.
Baking `--mode` into this extension's argv from a file inside its OWN installed directory
(`~/.pi/agent/extensions/daisugi-gate/`, at least as writable as `config.yaml`) recreates exactly
that hole: one line (`{"mode":"shadow"}`) written by the agent itself would turn every future deny
into an allow. Fix: **omit `--mode` from the argv entirely.** `resolve_gate_mode(None, root)` then
reads `config.yaml` server-side — the same trust level every other unset-mode caller already gets,
with no new override channel above it.

**`--verify-timeout` on the wire (adversarial review, should-fix):** the extension's own socket
timeout is 5000 ms; the gate's default inner verify budget is 10.0 s (`gate.py`). A verify slower
than 5 s would read as "gate unreachable" instead of the real verdict — fail-closed, but for the
wrong reason. Fix: send `--verify-timeout 4`, mirroring `gate_settings_json`'s own discipline of
keeping the inner budget under the outer one.

**Interfaces:**
- Consumes: `tests.harness_pi._node_check.node_status` (Task 1).
- Produces (from `index.ts`, all named exports): `askGate(event, ctx, opts?) -> Promise<Verdict>`
  where `Verdict = {allow: boolean, reason: string}`; `gateSocketPath(env) -> string`;
  `gateSocketTrustworthy(path) -> boolean`. Task 5 imports and extends this same file (adds
  `reportState` and the lifecycle `pi.on(...)` calls) — it must not redefine `gateSocketPath` or
  `gateSocketTrustworthy`.
- The wire request this sends over the gate socket is
  `{"v":1,"argv":["--format","pi","--root",<dirname of the socket>,"--verify-timeout","4"],
  "stdin_b64":<base64 of {tool_name, tool_input, cwd}>}` — the SAME bare-flags argv
  `gate_client.py` itself sends (verified against `gate_server.py`'s `_Handler`/`gate.py`'s
  `run_argv`/`_build_parser`), **not** a `daisugi` CLI subcommand line, and with no `--mode` (see
  above). spec-04's own text names `["hook","record","--format","pi"]`; that shape cannot reach a
  deny (`hook record` never blocks, confirmed by reading `hook.py`'s `record_and_contract`
  docstring: "Never blocks"), so this plan uses the verified working contract instead.
- `gateSocketTrustworthy(p: string) -> boolean` mirrors `gate_client.py`'s
  `_socket_is_trustworthy` (`:33-50`) exactly: `lstat` (never `stat`/`exists`, which follow
  symlinks), true only for a real socket, owned by our own uid, mode exactly `0600`.

- [ ] **Step 1: Write the failing test**

```python
# tests/harness_pi/test_extension_block_logic.py
from __future__ import annotations

import json
import os
import socketserver
import subprocess
import threading
from pathlib import Path

import pytest

from tests.harness_pi._node_check import node_status

HERE = Path(__file__).parent
_ok, _reason = node_status()
_NODE = pytest.mark.skipif(not _ok, reason=_reason)


class _FakeGateHandler(socketserver.StreamRequestHandler):
    reply: dict = {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}

    def handle(self) -> None:
        line = self.rfile.readline()
        json.loads(line)  # the request must at least be valid JSON
        self.wfile.write(json.dumps(self.reply).encode() + b"\n")


def _fake_gate(tmp_path: Path, reply: dict, mode: int = 0o600) -> Path:
    sock_path = tmp_path / "g.sock"
    handler = type("H", (_FakeGateHandler,), {"reply": reply})
    srv = socketserver.UnixStreamServer(str(sock_path), handler)
    os.chmod(sock_path, mode)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return sock_path


def _ask(sock_path: Path, tool_name: str, tool_input: dict, cwd: str = "/repo") -> dict:
    result = subprocess.run(
        [
            "node",
            "--experimental-strip-types",
            str(HERE / "run_tool_call.mjs"),
            tool_name,
            json.dumps(tool_input),
            cwd,
        ],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock_path), "PATH": "/usr/bin:/bin"},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    return json.loads(result.stdout.strip().splitlines()[-1])


@_NODE
def test_allow_and_argv_has_no_mode_but_has_verify_timeout(tmp_path):
    captured: list = []

    class _CapturingHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            line = self.rfile.readline()
            captured.append(json.loads(line))
            self.wfile.write(
                json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode() + b"\n"
            )

    sock_path = tmp_path / "g.sock"
    srv = socketserver.UnixStreamServer(str(sock_path), _CapturingHandler)
    os.chmod(sock_path, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    verdict = _ask(sock_path, "read", {"path": "/x"})
    assert verdict == {"allow": True, "reason": ""}
    argv = captured[0]["argv"]
    assert "--mode" not in argv
    assert argv == ["--format", "pi", "--root", str(tmp_path), "--verify-timeout", "4"]


@_NODE
def test_deny_carries_stderr_reason(tmp_path):
    sock = _fake_gate(
        tmp_path, {"v": 1, "stdout": "", "stderr": "openDaisugi gate: DENIED - no", "exit_code": 2}
    )
    verdict = _ask(sock, "bash", {"command": "rm -rf /"})
    assert verdict["allow"] is False
    assert "DENIED" in verdict["reason"]


@_NODE
def test_extension_blocks_on_an_exit_code_it_does_not_recognise(tmp_path):
    """Neither 0 (allow) nor 2 (deny) is a verdict this extension knows how
    to read -- a crashed gate, or a future gate.py bug, must not default to
    allow just because it isn't exit 2."""
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "crashed", "exit_code": 1})
    verdict = _ask(sock, "read", {})
    assert verdict["allow"] is False


@_NODE
def test_extension_blocks_on_a_malformed_gate_reply(tmp_path):
    sock_path = tmp_path / "g.sock"

    class _GarbageHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            self.rfile.readline()
            self.wfile.write(b"not json at all\n")

    srv = socketserver.UnixStreamServer(str(sock_path), _GarbageHandler)
    os.chmod(sock_path, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    verdict = _ask(sock_path, "read", {})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_socket_absent_blocks_with_teaching_reason(tmp_path):
    verdict = _ask(tmp_path / "nope.sock", "read", {"path": "/x"})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_extension_blocks_when_the_socket_is_not_a_private_socket_we_own(tmp_path):
    """A world-writable socket that happily answers exit_code 0 must still
    block: any local process that wins the path must not be able to grant
    an allow. Mirrors gate_client.py's _socket_is_trustworthy exactly."""
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}, mode=0o666)
    verdict = _ask(sock, "read", {"path": "/x"})
    assert verdict == {"allow": False, "reason": "openDaisugi gate unreachable: run daisugi start"}


@_NODE
def test_slow_socket_blocks_within_five_and_a_half_seconds(tmp_path):
    class _SlowHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            import time

            self.rfile.readline()
            time.sleep(6)
            try:
                self.wfile.write(
                    json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode()
                    + b"\n"
                )
            except OSError:
                pass  # the client already gave up, as it must

    sock_path = tmp_path / "g.sock"
    srv = socketserver.UnixStreamServer(str(sock_path), _SlowHandler)
    os.chmod(sock_path, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    import time

    started = time.monotonic()
    verdict = _ask(sock_path, "read", {"path": "/x"})
    elapsed = time.monotonic() - started
    assert verdict["allow"] is False
    assert elapsed < 5.5, f"took {elapsed:.2f}s, want < 5.5s"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/harness_pi/test_extension_block_logic.py -q`
Expected: FAIL — `run_tool_call.mjs` and `index.ts` do not exist yet (or, if Node is absent on
this runner, all skip with the Task 1 reason — check `node_status()` reports OK before treating a
skip as the baseline here).

- [ ] **Step 3: Write the harness script and the minimal implementation**

```javascript
// tests/harness_pi/run_tool_call.mjs
// Test-support harness: exercises askGate exactly as pi's own runtime would
// call it (real process.env, no test-only overrides), printing the verdict
// as one JSON line on stdout.
// Usage: node --experimental-strip-types run_tool_call.mjs <toolName> <inputJSON> <cwd>
import { askGate } from "../../src/opendaisugi/harness_pi/extension/index.ts";

const [, , toolName, inputJson, cwd] = process.argv;
const verdict = await askGate({ toolName, input: JSON.parse(inputJson) }, { cwd });
console.log(JSON.stringify(verdict));
```

```typescript
// src/opendaisugi/harness_pi/extension/index.ts
// The daisugi gate extension for pi (spec-04). Gates every tool_call through
// the resident gate over its Unix socket, fail-closed. Facts about pi's own
// wire formats are pinned in PINS.md next to this file (fetched 2026-09-08
// from pi's docs) -- do not re-derive them from memory when this file is
// touched again; re-read PINS.md or re-fetch the two sources it names.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";

interface Verdict {
  allow: boolean;
  reason: string;
}

const GATE_TIMEOUT_MS = 5000;
const REPORT_TIMEOUT_MS = 1000;
const UNREACHABLE_REASON = "openDaisugi gate unreachable: run daisugi start";

function gateSocketPath(env: NodeJS.ProcessEnv): string {
  if (env.OPENDAISUGI_GATE_SOCK) return env.OPENDAISUGI_GATE_SOCK;
  return path.join(env.HOME || "", ".opendaisugi", "gate", "gate.sock");
}

// Mirrors gate_client.py's _socket_is_trustworthy exactly: a private
// (0600, owned by us, a REAL socket -- never a symlink target we didn't
// verify) file at sockPath. lstat, never stat/exists: those follow
// symlinks, so a rogue process could plant a symlink to a socket it
// controls and have this trust it. Without this check the TypeScript
// client is strictly weaker than the Python one on the very same socket:
// any local process that wins the path answers {"exit_code":0} and every
// tool call is allowed.
function gateSocketTrustworthy(p: string): boolean {
  try {
    const st = fs.lstatSync(p);
    return st.isSocket() && st.uid === process.getuid!() && (st.mode & 0o777) === 0o600;
  } catch {
    return false;
  }
}

interface GateReply {
  exitCode: number;
  stderr: string;
}

function sendGateRequest(
  sockPath: string,
  argv: string[],
  payload: unknown,
  timeoutMs: number
): Promise<GateReply | null> {
  return new Promise((resolve) => {
    if (!gateSocketTrustworthy(sockPath)) {
      resolve(null);
      return;
    }
    let settled = false;
    const finish = (v: GateReply | null) => {
      if (settled) return;
      settled = true;
      resolve(v);
    };
    let sock: net.Socket;
    try {
      sock = net.createConnection(sockPath);
    } catch {
      finish(null);
      return;
    }
    const timer = setTimeout(() => {
      sock.destroy();
      finish(null);
    }, timeoutMs);
    let buf = "";
    sock.on("connect", () => {
      const req = {
        v: 1,
        argv,
        stdin_b64: Buffer.from(JSON.stringify(payload)).toString("base64"),
      };
      sock.write(JSON.stringify(req) + "\n");
    });
    sock.on("data", (chunk: Buffer) => {
      buf += chunk.toString("utf8");
    });
    sock.on("end", () => {
      clearTimeout(timer);
      try {
        const reply = JSON.parse(buf.trim());
        finish({ exitCode: reply.exit_code, stderr: reply.stderr || "" });
      } catch {
        finish(null);
      }
    });
    sock.on("error", () => {
      clearTimeout(timer);
      finish(null);
    });
  });
}

// Connects to the SAME resident-gate socket `daisugi gate_client` does
// (gate_client.py), sending the bare-flags argv `opendaisugi.gate`'s own
// parser reads (`run_argv` in gate.py) -- NOT a `daisugi` CLI subcommand
// line. This is the one path that actually reaches `evaluate_call` and
// returns a real allow/deny, verified against gate_server.py's `_Handler`.
//
// No --mode: baking a mode into this argv would let an agent-writable file
// (this extension's own directory) override the one flag the operator's
// installed hook command cannot rewrite (gate.py's resolve_gate_mode docs
// this explicitly). Omitting --mode lets the gate resolve it server-side
// from config.yaml, the same trust level every other unset-mode caller gets.
async function askGate(
  event: { toolName: string; input?: unknown },
  ctx: { cwd?: string },
  opts: { timeoutMs?: number; env?: NodeJS.ProcessEnv } = {}
): Promise<Verdict> {
  const env = opts.env || process.env;
  const timeoutMs = opts.timeoutMs ?? GATE_TIMEOUT_MS;
  const payload = {
    tool_name: event.toolName,
    tool_input: event.input ?? {},
    cwd: ctx.cwd || "",
  };
  const sockPath = gateSocketPath(env);
  const argv = ["--format", "pi", "--root", path.dirname(sockPath), "--verify-timeout", "4"];
  const reply = await sendGateRequest(sockPath, argv, payload, timeoutMs);
  if (reply === null) return { allow: false, reason: UNREACHABLE_REASON };
  if (reply.exitCode === 0) return { allow: true, reason: "" };
  if (reply.exitCode === 2) return { allow: false, reason: reply.stderr || "denied" };
  // Any other exit code (a crashed gate, a bug) is not a verdict we
  // recognize -- fail closed rather than guess which way it leans.
  return { allow: false, reason: UNREACHABLE_REASON };
}

export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event: any, ctx: any) => {
    const verdict = await askGate(event, ctx);
    if (!verdict.allow) return { block: true, reason: verdict.reason };
    return undefined;
  });
}

export { askGate, gateSocketPath, gateSocketTrustworthy };
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/harness_pi/test_extension_block_logic.py -q`
Expected: 7 passed (or 7 skipped with the Task 1 reason if Node is genuinely absent — on the
reference dev box, 7 passed).

- [ ] **Step 5: Register the new package data path**

Edit `pyproject.toml`'s `[tool.hatch.build.targets.wheel]` `artifacts` list:

```toml
artifacts = [
    "src/opendaisugi/skills/**/*.md",
    "src/opendaisugi/install_assets/**/*",
    "src/opendaisugi/viz_dag_template.html",
    "src/opendaisugi/harness_pi/**/*",
]
```

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/harness_pi/extension/index.ts tests/harness_pi/run_tool_call.mjs tests/harness_pi/test_extension_block_logic.py pyproject.toml
git commit -m "feat(harness_pi): gate every pi tool_call through the resident gate, fail-closed"
```

---

### Task 5: TypeScript extension — reportState and lifecycle wiring

**Files:**
- Modify: `src/opendaisugi/harness_pi/extension/index.ts` (adds `reportState` + lifecycle events)
- Create: `tests/harness_pi/run_report_state.mjs`
- Modify: `tests/harness_pi/test_extension_block_logic.py` (appends reportState tests)

**Why `hook report` reaching the gate over the socket is not a hidden assumption:** spec-01
(a prerequisite plan, not built here) is the one that widens `gate_server.py`'s dispatch to
recognize `{"argv":["hook","report"],...}`. Until that lands, a call to `reportState` here simply
gets an unparseable-argv reply from the CURRENT (narrower) dispatcher, or no reply at all if the
socket is absent — either way `reportState` is fire-and-forget and off the fail-closed path, so
the floor shows `state: unknown` (§3.1's honest default for "no source"), never a wrong "idle" or
"working". This is stated here explicitly rather than left implicit; Step 1's test proves the
"never throws" half regardless of which side of that dependency is true today.

**`session_id` must not carry the pane id (adversarial review, should-fix):** master §3.1 defines
`session_id` as the daisugi session id (the tree file stem) and carries the pane in a SEPARATE
`pane` field. The original fallback chain (`COPPICE_PANE || OPENDAISUGI_SESSION_ID || pi-<pid>`)
put a pane id straight into `session_id` whenever coppice set `COPPICE_PANE`. Fix: `session_id` is
`env.OPENDAISUGI_SESSION_ID || "pi-" + process.pid`; `pane` is set from `env.COPPICE_PANE`
separately when present (the same env var plan-01's own `report_state` reads).

**Interfaces:**
- Consumes: `gateSocketPath`, `gateSocketTrustworthy` (Task 4, same file).
- Produces: `reportState(sessionState: string, env?: NodeJS.ProcessEnv) -> void`, and the default
  export now also registers `session_start` → idle, `agent_start` → working, `agent_end` → idle,
  `agent_settled` → idle.

- [ ] **Step 1: Write the failing test**

Append to `tests/harness_pi/test_extension_block_logic.py`:

```python
class _CapturingHandler(socketserver.StreamRequestHandler):
    captured: list = []

    def handle(self) -> None:
        import base64

        line = self.rfile.readline()
        req = json.loads(line)
        payload = json.loads(base64.b64decode(req["stdin_b64"]))
        type(self).captured.append((req, payload))
        self.wfile.write(
            json.dumps({"v": 1, "stdout": "", "stderr": "", "exit_code": 0}).encode() + b"\n"
        )


@_NODE
def test_report_state_sends_hook_report_shape_with_session_id_pane_split(tmp_path):
    handler = type("H", (_CapturingHandler,), {"captured": []})
    sock_path = tmp_path / "g.sock"
    srv = socketserver.UnixStreamServer(str(sock_path), handler)
    os.chmod(sock_path, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "run_report_state.mjs"), "working"],
        cwd=HERE,
        env={
            "OPENDAISUGI_GATE_SOCK": str(sock_path),
            "COPPICE_PANE": "w1:p3",
            "PATH": "/usr/bin:/bin",
        },
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert len(handler.captured) == 1
    req, payload = handler.captured[0]
    assert req["argv"] == ["hook", "report"]
    assert payload["harness"] == "pi"
    assert payload["state"] == "working"
    assert payload["source"] == "headless"
    assert payload["pane"] == "w1:p3"
    assert payload["session_id"] != "w1:p3"  # the pane id must not leak into session_id
    assert payload["session_id"]  # never empty -- see reportState's fallback chain


@_NODE
def test_report_state_never_raises_when_gate_unreachable(tmp_path):
    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "run_report_state.mjs"), "idle"],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(tmp_path / "nope.sock"), "PATH": "/usr/bin:/bin"},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout


@_NODE
def test_report_state_never_raises_on_a_malformed_reply(tmp_path):
    """Simulates a gate socket that exists but does not yet understand
    ["hook","report"] (spec-01 not landed) -- must not throw or retry."""

    class _MalformedHandler(socketserver.StreamRequestHandler):
        def handle(self) -> None:
            self.rfile.readline()
            self.wfile.write(b"not json at all\n")

    sock_path = tmp_path / "g.sock"
    srv = socketserver.UnixStreamServer(str(sock_path), _MalformedHandler)
    os.chmod(sock_path, 0o600)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "run_report_state.mjs"), "idle"],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock_path), "PATH": "/usr/bin:/bin"},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout


@_NODE
def test_report_state_skips_an_untrustworthy_socket_without_raising(tmp_path):
    sock = _fake_gate(tmp_path, {"v": 1, "stdout": "", "stderr": "", "exit_code": 0}, mode=0o666)
    result = subprocess.run(
        ["node", "--experimental-strip-types", str(HERE / "run_report_state.mjs"), "idle"],
        cwd=HERE,
        env={"OPENDAISUGI_GATE_SOCK": str(sock), "PATH": "/usr/bin:/bin"},
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert "done" in result.stdout
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/harness_pi/test_extension_block_logic.py -k report_state -q`
Expected: FAIL — `reportState` is not exported and `run_report_state.mjs` does not exist.

- [ ] **Step 3: Write the harness script and extend index.ts**

```javascript
// tests/harness_pi/run_report_state.mjs
// Test-support harness: fires one reportState call and exits, exactly as a
// fire-and-forget lifecycle handler would.
// Usage: node --experimental-strip-types run_report_state.mjs <state>
import { reportState } from "../../src/opendaisugi/harness_pi/extension/index.ts";

reportState(process.argv[2]);
// give the fire-and-forget socket write a moment to reach the OS before exit
await new Promise((r) => setTimeout(r, 200));
console.log("done");
```

Edit `src/opendaisugi/harness_pi/extension/index.ts`: add a `REPORT_TIMEOUT_MS` constant next to
`GATE_TIMEOUT_MS`, add `reportState` after `askGate`, and extend the default export:

```typescript
const REPORT_TIMEOUT_MS = 1000;
```//  (placed next to `const GATE_TIMEOUT_MS = 5000;`)

```typescript
// Fire-and-forget: reaches `daisugi hook report` over the SAME socket, per
// spec-01's widened dispatch (`{"argv":["hook","report"],...}`). Never
// blocks the agent -- errors are swallowed, exactly like reportState's
// Python sibling (opendaisugi.floor.report / _state_report). If spec-01's
// dispatch has not landed yet, the gate replies with something this code
// cannot parse (or nothing at all); either way this function still returns
// without throwing, and the floor simply shows no state for this pane
// (§3.1's honest "unknown", never a guessed "idle").
function reportState(sessionState: string, env: NodeJS.ProcessEnv = process.env): void {
  const sockPath = gateSocketPath(env);
  // session_id is the daisugi session id (master §3.1); the pane id is a
  // SEPARATE field. COPPICE_PANE names the pane, not the session -- it
  // must not leak into session_id.
  const sessionId = env.OPENDAISUGI_SESSION_ID || `pi-${process.pid}`;
  const event: Record<string, unknown> = {
    v: 1,
    ts: Date.now() / 1000,
    session_id: sessionId,
    harness: "pi",
    state: sessionState,
    source: "headless",
    detail: "",
  };
  if (env.COPPICE_PANE) event.pane = env.COPPICE_PANE;
  if (!gateSocketTrustworthy(sockPath)) return;
  try {
    const sock = net.createConnection(sockPath);
    const timer = setTimeout(() => sock.destroy(), REPORT_TIMEOUT_MS);
    sock.on("connect", () => {
      const req = {
        v: 1,
        argv: ["hook", "report"],
        stdin_b64: Buffer.from(JSON.stringify(event)).toString("base64"),
      };
      sock.end(JSON.stringify(req) + "\n");
    });
    sock.on("close", () => clearTimeout(timer));
    sock.on("error", () => clearTimeout(timer));
  } catch {
    // never let a reporting failure touch the agent
  }
}
```

Replace the default export function body:

```typescript
export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event: any, ctx: any) => {
    const verdict = await askGate(event, ctx);
    if (!verdict.allow) return { block: true, reason: verdict.reason };
    return undefined;
  });
  pi.on("session_start", () => reportState("idle"));
  pi.on("agent_start", () => reportState("working"));
  pi.on("agent_end", () => reportState("idle"));
  pi.on("agent_settled", () => reportState("idle"));
}

export { askGate, reportState, gateSocketPath, gateSocketTrustworthy };
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/harness_pi/test_extension_block_logic.py -q`
Expected: 11 passed (Task 4's 7 plus this task's 4).

- [ ] **Step 5: Type-check the finished file**

Run: `node --experimental-strip-types --check src/opendaisugi/harness_pi/extension/index.ts`
Expected: no output, exit 0.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/harness_pi/extension/index.ts tests/harness_pi/run_report_state.mjs tests/harness_pi/test_extension_block_logic.py
git commit -m "feat(harness_pi): report pi's lifecycle state to the floor, fire-and-forget"
```

---

### Task 6: `daisugi install --harness pi`

**Files:**
- Modify: `src/opendaisugi/install.py:1195` (new functions added beside
  `_install_openclaw_plugin`: `_install_pi_extension`, `_remove_pi_extension`,
  `install_harness_extension`, `uninstall_harness_extension`, `SUPPORTED_HARNESSES`)
- Modify: `src/opendaisugi/cli.py:3545` (`install_cmd`'s `runtime` option — a new `harness`
  option is added right after it) and `src/opendaisugi/cli.py:3593` (the `if print_skill:`
  early-return — the new `if harness:` branch is added immediately after it)
- Test: `tests/test_install_pi.py`

**Why `install.py`'s existing `Runtime` protocol is not reused:** pi has none of the four default
layers (skill/mcp/capture/instructions) — no MCP, and skill/prompt packaging is explicitly out of
scope for this spec. Forcing pi through `Runtime.plan/apply` would mean four permanent "Not
wired" gap lines on every install for concepts pi's extension model does not have. `--harness` is
a narrower, honest surface: it installs exactly one thing, the gate extension.

**No per-install config file, and no `enforce` parameter (blocker 4's consequence):** since Task
4 dropped `--mode` from the extension's own argv, there is nothing left for an install-time
`--enforce` flag to write into. `install_harness_extension`/`_install_pi_extension` drop the
`enforce` parameter entirely; the extension's mode is whatever `config.yaml`'s `gate_mode` says,
exactly like every other unset-`--mode` caller.

**Two false statements this task must not print (adversarial review, should-fix):** (a) "pi
hot-reloads extensions from this directory — no restart needed" is false: pi's own docs say
auto-discovered extensions "can be hot-reloaded with `/reload`", a manual slash command that does
not exist in `--mode rpc` at all. Say "restart pi, or run `/reload` in an interactive session."
(b) With `--mode` gone, a fresh install always resolves to shadow (`resolve_gate_mode` returns
`"shadow"` on a missing/unreadable `config.yaml`), so spec-04's line "or every tool call will
block" is false the moment it is printed. The honest STE100 line: "pi asks the gate in-process.
The gate only watches until you set `gate_mode: enforce`. Start it with `daisugi start`."

**Interfaces:**
- Consumes: `src/opendaisugi/harness_pi/extension/index.ts` (Tasks 4-5), `_remove_dir`
  (`install.py:1316`, existing).
- Produces:
  - `install_harness_extension(name: str, *, home: Path) -> list[Path]`
  - `uninstall_harness_extension(name: str, *, home: Path) -> list[Path]`
  - `SUPPORTED_HARNESSES: tuple[str, ...]` (currently `("pi",)`)
  - CLI: `daisugi install --harness pi [--uninstall]`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_install_pi.py
from __future__ import annotations

from opendaisugi.install import install_harness_extension, uninstall_harness_extension


def test_installs_extension_file(tmp_path):
    modified = install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    assert (dest / "index.ts").exists()
    assert modified  # something was actually written


def test_two_installs_are_idempotent_same_content(tmp_path):
    install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    before = (dest / "index.ts").read_text()
    second = install_harness_extension("pi", home=tmp_path)
    after = (dest / "index.ts").read_text()
    assert before == after
    assert second == []  # nothing changed the second time


def test_uninstall_removes_the_directory(tmp_path):
    install_harness_extension("pi", home=tmp_path)
    dest = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    assert dest.exists()
    uninstall_harness_extension("pi", home=tmp_path)
    assert not dest.exists()


def test_uninstall_of_a_never_installed_harness_is_a_silent_no_op(tmp_path):
    assert uninstall_harness_extension("pi", home=tmp_path) == []
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_install_pi.py -q`
Expected: FAIL — `install_harness_extension` does not exist.

- [ ] **Step 3: Write the minimal implementation**

Add to `src/opendaisugi/install.py` (near `_install_openclaw_plugin`):

```python
SUPPORTED_HARNESSES: tuple[str, ...] = ("pi",)


def _install_pi_extension(home: Path) -> list[Path]:
    """Materialize the daisugi-gate pi extension into
    ~/.pi/agent/extensions/daisugi-gate/. pi does not hot-reload extensions
    in --mode rpc -- restart the pi process, or run /reload in an
    interactive session, for a freshly installed/updated extension to take
    effect. Idempotent: re-running writes nothing new.

    No per-install config: the extension carries no --mode of its own (see
    Task 4's ruling), so there is nothing here for an --enforce flag to
    write into. Its mode is config.yaml's gate_mode, shared with every
    other host.

    The cross-tenant uninstall ref-count residual (install.py's KNOWN
    LIMITATION note, ~line 1307) does NOT apply here: unlike
    ~/.agents/skills, ~/.pi/agent/extensions/daisugi-gate/ is exclusive to
    pi and shared with no other harness this project installs into.
    """
    import importlib.resources as _ir

    dest = home / ".pi" / "agent" / "extensions" / "daisugi-gate"
    dest.mkdir(parents=True, exist_ok=True)
    src = _ir.files("opendaisugi").joinpath("harness_pi", "extension")
    out = dest / "index.ts"
    if out.is_symlink():
        out.unlink()  # never write THROUGH a pre-planted symlink
    new_text = src.joinpath("index.ts").read_text(encoding="utf-8")
    if not out.exists() or out.read_text(encoding="utf-8") != new_text:
        out.write_text(new_text, encoding="utf-8")
        return [out]
    return []


def _remove_pi_extension(home: Path) -> list[Path]:
    return _remove_dir(home / ".pi" / "agent" / "extensions" / "daisugi-gate")


_HARNESS_INSTALLERS: dict[str, tuple] = {
    "pi": (_install_pi_extension, _remove_pi_extension),
}


def install_harness_extension(name: str, *, home: Path) -> list[Path]:
    installer, _ = _HARNESS_INSTALLERS[name]
    return installer(home)


def uninstall_harness_extension(name: str, *, home: Path) -> list[Path]:
    _, remover = _HARNESS_INSTALLERS[name]
    return remover(home)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/test_install_pi.py -q`
Expected: 4 passed.

- [ ] **Step 5: Wire the CLI flag**

Edit `src/opendaisugi/cli.py`'s `install_cmd`, adding a new parameter after `runtime`:

```python
harness: list[str] = (
    typer.Option(
        None,
        "--harness",
        help="Install a loop harness's gate extension (pi). Independent of --runtime.",
    ),
)
```

And, right after the existing `if print_skill:` early-return block, add:

```python
if harness:
    from opendaisugi.install import (
        SUPPORTED_HARNESSES,
        install_harness_extension,
        uninstall_harness_extension,
    )

    bad = [h for h in harness if h not in SUPPORTED_HARNESSES]
    if bad:
        typer.echo(
            f"error: unknown harness {bad[0]!r}. Supported: {', '.join(SUPPORTED_HARNESSES)}.",
            err=True,
        )
        raise typer.Exit(code=2)
    if do_uninstall:
        for h in harness:
            removed = uninstall_harness_extension(h, home=home)
            if removed:
                typer.echo(f"[{h}] removed {len(removed)} file(s).")
            else:
                typer.echo(f"[{h}] nothing was installed.")
        return
    for h in harness:
        install_harness_extension(h, home=home)
        typer.echo(
            f"[{h}] gate extension installed. Restart pi, or run /reload "
            "in an interactive session, for it to take effect."
        )
    typer.echo(
        "pi asks the gate in-process. The gate only watches until you set "
        "gate_mode: enforce. Start it with `daisugi start`."
    )
    return
```

Note: `home = Path.home()` is already computed earlier in `install_cmd` (existing line, before
the runtime-detection block) — this new branch reads that same variable, not a fresh one.

- [ ] **Step 6: Write a CLI-level test**

```python
# append to tests/test_install_pi.py
from typer.testing import CliRunner

from opendaisugi.cli import app


def test_cli_install_harness_pi_prints_the_honest_restart_and_mode_lines(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--yes"])
    assert result.exit_code == 0, result.output
    assert "Restart pi" in result.output or "/reload" in result.output
    assert "gate_mode: enforce" in result.output
    assert "will block" not in result.output  # false the moment --mode is gone
    assert (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()


def test_cli_install_unknown_harness_is_a_teaching_error(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    result = CliRunner().invoke(app, ["install", "--harness", "nope"])
    assert result.exit_code == 2
    assert "unknown harness" in result.output
    assert "pi" in result.output


def test_cli_install_harness_pi_uninstall(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    CliRunner().invoke(app, ["install", "--harness", "pi", "--yes"])
    result = CliRunner().invoke(app, ["install", "--harness", "pi", "--uninstall"])
    assert result.exit_code == 0, result.output
    assert not (tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate").exists()
```

- [ ] **Step 7: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/test_install_pi.py -q`
Expected: 7 passed.

- [ ] **Step 8: Run the full install suite to confirm no regression**

Run: `uv run --no-sync pytest tests/test_install.py tests/test_cli_install_help.py -q`
Expected: all pass, unchanged counts.

- [ ] **Step 9: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/install.py src/opendaisugi/cli.py tests/test_install_pi.py`
Expected: clean.

- [ ] **Step 10: Commit**

```bash
git add src/opendaisugi/install.py src/opendaisugi/cli.py tests/test_install_pi.py
git commit -m "feat(install): add 'daisugi install --harness pi' for the gate extension"
```

---

### Task 7: Honesty tags — pi in `daisugi modules`

**Files:**
- Modify: `src/opendaisugi/modules.py:55` (add helpers beside `_claude_hook_installed`),
  `src/opendaisugi/modules.py:122-126` (the `harness` stage's module list — insert the `pi` row
  after `harness("codex", "codex")`), `src/opendaisugi/modules.py:141` (the `gate` stage's module
  list — insert a `pi tool_call` row after the `codex hooks.json` row)
- Modify: `tests/test_modules.py`

**Interfaces:**
- Produces: `_pi_extension_installed(home: Path) -> bool`, `_pi_detected(home: Path) -> bool`
  (both pure, home-parameterized for testability — unlike the file's existing
  `_claude_hook_installed`, which hardcodes `Path.home()`).

- [ ] **Step 1: Write the failing test**

Append to `tests/test_modules.py`:

```python
def test_pi_harness_row_is_active_when_extension_installed(tmp_path, monkeypatch):
    from opendaisugi.modules import detect_stages

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    ext = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// installed")
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == "active"


def test_pi_harness_row_is_available_when_pi_present_but_not_installed(tmp_path, monkeypatch):
    from opendaisugi.modules import detect_stages

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".pi").mkdir()
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == "available"


def test_pi_harness_row_is_possible_when_pi_not_detected(tmp_path, monkeypatch):
    from opendaisugi.modules import detect_stages

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    stages = detect_stages(tmp_path)
    harness_stage = next(s for s in stages if s.key == "harness")
    pi_module = next(m for m in harness_stage.modules if m.name == "pi")
    assert pi_module.state == "possible"


def test_gate_stage_names_pis_in_process_enforcement_class(tmp_path, monkeypatch):
    from opendaisugi.modules import detect_stages

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    ext = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// installed")
    stages = detect_stages(tmp_path)
    gate_stage = next(s for s in stages if s.key == "gate")
    pi_gate = next(m for m in gate_stage.modules if "pi" in m.name)
    assert pi_gate.state == "active"
    assert "exit-2" in pi_gate.note or "fail-open" in pi_gate.note
```

- [ ] **Step 2: Run test to verify it fails**

Run: `uv run --no-sync pytest tests/test_modules.py -k pi_harness -q`
Expected: FAIL — no `Module(name="pi", ...)` row exists in the `harness` stage yet.

- [ ] **Step 3: Write the minimal implementation**

Edit `src/opendaisugi/modules.py`. Add two module-level helpers near `_claude_hook_installed`:

```python
def _pi_extension_installed(home: Path) -> bool:
    return (home / ".pi" / "agent" / "extensions" / "daisugi-gate" / "index.ts").exists()


def _pi_detected(home: Path) -> bool:
    return (home / ".pi").is_dir()
```

In `detect_stages`, add `home = Path.home()` at the top of the function body (before the `harness`
closure definition), and add a `pi` row to the `harness` stage's modules list (right after
`harness("codex", "codex")` — pi is a real, installable adapter now, not a "designed, not built"
stub like the two lines that follow it):

```python
(harness("claude-code", "claude-code"),)
(harness("codex", "codex"),)
(
    Module(
        "pi",
        ACTIVE
        if _pi_extension_installed(home)
        else (AVAILABLE if _pi_detected(home) else POSSIBLE),
        "gate extension installed"
        if _pi_extension_installed(home)
        else (
            "pi detected — run `daisugi install --harness pi`"
            if _pi_detected(home)
            else "not detected on this box"
        ),
    ),
)
(Module("hermes", POSSIBLE, "adapter designed, not built"),)
(Module("openclaw", POSSIBLE, "adapter designed, not built"),)
(Module("<any via AGENTS.md>", POSSIBLE, "vendor-neutral hook contract"),)
```

And add a row to the `gate` stage's modules list, after `Module("codex hooks.json", ...)`:

```python
(Module("codex hooks.json", AVAILABLE, "fail-open class — soft gate"),)
(
    Module(
        "pi tool_call (in-process)",
        ACTIVE if _pi_extension_installed(home) else POSSIBLE,
        "native block, no exit-2 convention, no fail-open outer timeout"
        if _pi_extension_installed(home)
        else "run `daisugi install --harness pi`",
    ),
)
(Module("shadow / off", AVAILABLE, "config: gate mode"),)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `uv run --no-sync pytest tests/test_modules.py -q`
Expected: all pass (existing tests plus the 4 new ones).

- [ ] **Step 5: Lint**

Run: `uv run --no-sync ruff check src/opendaisugi/modules.py tests/test_modules.py`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/modules.py tests/test_modules.py
git commit -m "feat(modules): mark pi's gate extension ACTIVE/AVAILABLE/POSSIBLE, honestly"
```

---

### Task 8: Go adapter — `pi --mode rpc` as a headless coppice pane

**Prerequisite check (this task only — the rest of this plan does not need Go or coppice):**
this task assumes plan-02 has already landed `harness/coppice/` including Task 14 (the headless
registry and the `pi`/`opencode` slots). If `harness/coppice/internal/adapters/pi/pi.go` does not
exist yet, STOP here — this task cannot be built or tested in isolation from that scaffold.

- [ ] **Step 0: Verify the prerequisite**

Run: `test -f harness/coppice/internal/adapters/pi/pi.go && grep -q "NotBuilt" harness/coppice/internal/adapters/pi/pi.go`
Expected: exit 0 (the slot exists and still says NotBuilt — this task replaces it). If it fails:
"harness/coppice's pi adapter slot is not built yet. Run plan-02
(docs/plans/2026-09-08-workshop/plan-02-coppice-server.md) Task 14 first, then re-run this task."

**The types below are normative, verbatim from `plan-02-coppice-server.md`'s landed code — not
this plan's invention (adversarial review, blocker — cross-plan ruling 2):** an earlier pass of
this plan wrote its own `StartOpts`/`Proc`/`Event`/`EventKind` before checking whether plan-02
had already landed them. It had. Reading plan-02's own Task 6 (`internal/pane`, ≈3612-3673),
Task 2 (`internal/proto`, ≈1462-1524), and Task 14 (the `pi`/`opencode` slots and `pumpAdapter`,
≈8238-8719) directly gives the real shapes:

```go
// harness/coppice/internal/pane (Task 6) -- already landed, DO NOT REDEFINE
package pane

type EventKind string

const (
	EvText  EventKind = "text"
	EvTool  EventKind = "tool"
	EvState EventKind = "state"
	EvEnd   EventKind = "end"
	EvError EventKind = "error"
)

type Event struct {
	Kind   EventKind
	Text   string
	Tool   string
	State  string // idle | working | blocked | done | unknown, for EvState and EvEnd
	Detail string
}

type StartOpts struct {
	Cwd    string
	Env    map[string]string
	Argv   []string // extra argv beyond the adapter's own
	Resume string   // a harness session id to resume, when one was recorded
	Sock   string
	PaneID string
}

// Proc is a 6-METHOD INTERFACE implemented on the running process value
// itself -- calls are `proc.Prompt(text)`, never `adapter.Prompt(proc, text)`.
type Proc interface {
	Prompt(text string) error
	Steer(text string) error
	WriteStdin(b []byte) error
	Events() <-chan Event
	SessionID() (string, bool)
	Stop() error
}

// Adapter is Name() plus Start(ctx, StartOpts, *Grid) only -- no Prompt/
// Steer/Events/SessionID methods live here; those are on Proc.
type Adapter interface {
	Name() string
	Start(ctx context.Context, o StartOpts, g *Grid) (Proc, error)
}
```

```go
// harness/coppice/internal/proto (Task 2) -- already landed, DO NOT REDEFINE
// (an earlier pass of this plan cited these as living in internal/state;
// verified false by reading plan-02's own pumpAdapter, which constructs
// proto.PaneStateEvent{...} and proto.SrcHeadless directly, and by reading
// internal/state's own code, which imports proto.PaneStateEvent/proto.Ask
// rather than declaring its own -- proto is correct.)
package proto

const (
	StateIdle = "idle"; StateWorking = "working"; StateBlocked = "blocked"
	StateDone = "done"; StateUnknown = "unknown"
)
const (
	SrcOperator = "operator"; SrcGate = "gate"; SrcHeadless = "headless"
	SrcProcess = "process"; SrcManifest = "manifest"
)

type Ask struct {
	ID       string  `json:"id"`
	Tool     string  `json:"tool"`
	Summary  string  `json:"summary"`
	Deadline float64 `json:"deadline"`
}

type PaneStateEvent struct {
	V int `json:"v"`; TS float64 `json:"ts"`
	SessionID string `json:"session_id"`; HarnessSessionID *string `json:"harness_session_id"`
	Harness string `json:"harness"`; Pane *string `json:"pane"`
	State string `json:"state"`; Source string `json:"source"`
	Ask *Ask `json:"ask,omitempty"`; Detail string `json:"detail"`
}
```

**Amendment plan-02 owes (see Global Constraints): `pane.Event` has no `Ask` field.** The landed
`Event` struct above has nowhere to carry the ask a `blocked` state implies, and
`pumpAdapter` (`internal/server/panes.go`, plan-02 Task 14) does not copy one through today
either. This task sets `Ask` on every `pane.Event` it emits for a blocked state (see the code
below), on the assumption plan-02 adds `Ask *proto.Ask` to `Event` and one line
(`e.Ask = ev.Ask`) to `pumpAdapter`. Until that lands, the field is silently dropped at the
server boundary — a state without a visible ask, not a wrong state.

**`SessionID()` returns `sessionFile`, not `sessionId` (verified against pi's own RPC docs, not
assumed):** `switch_session` resumes BY FILE PATH; feeding it the bare `sessionId` instead makes
resume silently fail. This trades a less legible `HarnessSessionID` display value for a resume
path that actually works — accepted, since `pane.Proc`'s `SessionID()` is the one hook plan-02
gives both roles.

**Files:**
- Modify: `harness/coppice/internal/adapters/pi/pi.go` (replaces plan-02's `NotBuilt` slot with
  the real adapter — this task does NOT create a new `adapter.go`)
- Create: `harness/coppice/internal/adapters/pi/pi_test.go`

**Interfaces:**
- Consumes: `pane.Adapter`, `pane.Proc`, `pane.StartOpts`, `pane.Event`, `pane.EventKind`
  (`internal/pane`, plan-02 Task 6); `proto.Ask`, `proto.PaneStateEvent`, `proto.State*`,
  `proto.SrcHeadless` (`internal/proto`, plan-02 Task 2); `adapters.Register`
  (`internal/adapters`, plan-02 Task 14).
- Produces: `func New() pane.Adapter` (replaces the `NotBuilt` slot plan-02 registered); an
  unexported `adapter` type implementing `pane.Adapter`; an unexported `proc` type implementing
  `pane.Proc`'s 6 methods.

- [ ] **Step 1: Write the failing tests**

```go
// harness/coppice/internal/adapters/pi/pi_test.go
package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/pane"
)

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stubPi writes a fake `pi --mode rpc`: it streams eventsFile to stdout and
// records everything written to its stdin into stdinLog, for assertions.
// `exec 3<&0` + reading from fd 3 in the backgrounded loop is required: bash
// redirects an async command's stdin from /dev/null by default in a
// non-interactive script (POSIX), so a plain `while read ... & ` never sees
// anything written to the real stdin pipe.
func stubPi(t *testing.T, eventsFile, stdinLog string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pi")
	body := "#!/bin/sh\n" +
		"exec 3<&0\n" +
		"while IFS= read -r line <&3; do printf '%s\\n' \"$line\" >> " + shQuote(stdinLog) + "; done &\n" +
		"cat " + shQuote(eventsFile) + "\n" +
		"wait\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func lastJSONLine(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("last line not JSON: %v (%q)", err, raw)
	}
	return got
}

func TestAdapterEmitsBlockedOnExtensionUIRequestAndAnswers(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	transcript := `{"type":"agent_start"}` + "\n" +
		`{"type":"extension_ui_request","id":"req-1","method":"confirm","title":"Dangerous!","message":"Allow rm -rf?"}` + "\n"
	if err := os.WriteFile(events, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var sawWorking, sawBlocked bool
	var askID string
	deadline := time.After(2 * time.Second)
	ch := p.Events()
	for !sawBlocked {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("event channel closed before a blocked state arrived")
			}
			if ev.Kind == pane.EvState {
				switch ev.State {
				case "working":
					sawWorking = true
				case "blocked":
					sawBlocked = true
					if ev.Ask == nil {
						t.Fatal("a blocked state must carry an ask")
					}
					askID = ev.Ask.ID
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for a blocked state")
		}
	}
	if !sawWorking {
		t.Fatal("expected a working state before blocked (agent_start)")
	}
	if askID != "req-1" {
		t.Fatalf("ask id = %q, want req-1", askID)
	}

	if err := p.Prompt("y"); err != nil {
		t.Fatalf("Prompt (answer): %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	resp := lastJSONLine(t, stdinLog)
	if resp["type"] != "extension_ui_response" || resp["id"] != "req-1" || resp["confirmed"] != true {
		t.Fatalf("unexpected extension_ui_response: %+v", resp)
	}
}

func TestPromptSendsExactJSON(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Prompt("run ls"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "run ls" {
		t.Fatalf("unexpected prompt JSON: %+v", got)
	}
}

func TestSessionIDRoundTripsThroughResume(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	transcript := `{"type":"response","command":"get_state","success":true,"data":{"sessionId":"abc123","sessionFile":"/home/x/.pi/agent/sessions/foo/1_abc.jsonl"}}` + "\n"
	if err := os.WriteFile(events, []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// get_state's response updates internal metadata directly; it emits no
	// pane.Event (only real state transitions do), so poll rather than wait
	// on the events channel.
	deadline := time.Now().Add(2 * time.Second)
	var resumeValue string
	for time.Now().Before(deadline) {
		if v, ok := p.SessionID(); ok {
			resumeValue = v
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resumeValue == "" {
		t.Fatal("timed out waiting for get_state response to be processed")
	}
	if resumeValue != "/home/x/.pi/agent/sessions/foo/1_abc.jsonl" {
		t.Fatalf("SessionID() = %q, want the sessionFile path, not the bare sessionId", resumeValue)
	}

	events2 := filepath.Join(dir, "events2.jsonl")
	if err := os.WriteFile(events2, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog2 := filepath.Join(dir, "stdin2.log")
	if err := os.WriteFile(stdinLog2, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a2 := adapter{Bin: stubPi(t, events2, stdinLog2)}
	if _, err := a2.Start(context.Background(), pane.StartOpts{Cwd: dir, Resume: resumeValue}, nil); err != nil {
		t.Fatalf("Start (resume): %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog2)
	if got["type"] != "switch_session" || got["sessionPath"] != resumeValue {
		t.Fatalf("unexpected switch_session JSON: %+v", got)
	}
}

func TestPromptDuringStreamingCarriesSteerBehavior(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte(`{"type":"agent_start"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.After(2 * time.Second)
	sawWorking := false
	for !sawWorking {
		select {
		case ev := <-p.Events():
			if ev.Kind == pane.EvState && ev.State == "working" {
				sawWorking = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for agent_start's working state")
		}
	}
	if err := p.Prompt("keep going"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	got := lastJSONLine(t, stdinLog)
	if got["type"] != "prompt" || got["message"] != "keep going" || got["streamingBehavior"] != "steer" {
		t.Fatalf("unexpected prompt JSON while streaming: %+v", got)
	}
}

func TestNewIsRegisteredUnderTheNamePi(t *testing.T) {
	if New().Name() != "pi" {
		t.Fatalf("Name() = %q, want %q", New().Name(), "pi")
	}
}

func TestStopKillsTheProcess(t *testing.T) {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stdinLog := filepath.Join(dir, "stdin.log")
	if err := os.WriteFile(stdinLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	a := adapter{Bin: stubPi(t, events, stdinLog)}
	p, err := a.Start(context.Background(), pane.StartOpts{Cwd: dir}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd harness/coppice && go test ./internal/adapters/pi/... -v`
Expected: FAIL to compile — `adapter{...}`, `pane.EvState`, `p.Events()` etc. don't match
`pi.go`'s current `NotBuilt`-only content.

- [ ] **Step 3: Replace the slot with the real implementation**

```go
// harness/coppice/internal/adapters/pi/pi.go
// Package pi drives `pi --mode rpc` as a headless coppice pane (spec-04),
// filling the slot plan-02 Task 14 registers.
//
// Facts pinned from pi's own docs (recorded in
// src/opendaisugi/harness_pi/extension/PINS.md, fetched 2026-09-08):
//   - Framing: LF-delimited JSONL only, one command/event per line.
//   - `{"type":"prompt","message":str}` -> `{"type":"response","command":"prompt","success":bool}`.
//   - `{"type":"get_state"}` -> data.sessionId, data.sessionFile, ...
//   - `{"type":"switch_session","sessionPath":str}` resumes BY FILE PATH, not by bare id.
//   - Dialog requests: `{"type":"extension_ui_request","id","method":"select"|"confirm"|"input"|"editor",...}`;
//     answered with `{"type":"extension_ui_response","id",...}`. select/input/editor carry
//     `title`; confirm carries `title` AND `message`. Fire-and-forget methods (notify,
//     setStatus, setWidget, setTitle, set_editor_text) never expect a response.
package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
	"github.com/opendaisugi/coppice/internal/proto"
)

// adapter drives `pi --mode rpc`. Bin overrides the binary name (tests point
// it at a fake script); empty means "pi" on PATH.
type adapter struct {
	Bin string
}

// New returns the real pi adapter, replacing plan-02's NotBuilt slot.
func New() pane.Adapter { return adapter{} }

func init() { adapters.Register(New()) }

func (adapter) Name() string { return "pi" }

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func (a adapter) Start(ctx context.Context, o pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	bin := a.Bin
	if bin == "" {
		bin = "pi"
	}
	argv := []string{"--mode", "rpc"}
	if o.PaneID != "" {
		// spec-04: --session-dir <coppice state>/pi/<pane>. The coppice state
		// root is Sock's directory (the socket and the server's state share
		// a parent per master §3.3); StartOpts has no separate state-root field.
		argv = append(argv, "--session-dir", o.Sock+"-state/pi/"+o.PaneID)
	}
	argv = append(argv, o.Argv...)

	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Dir = o.Cwd
	if len(o.Env) > 0 {
		cmd.Env = envSlice(o.Env)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi adapter: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pi adapter: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pi adapter: start %q: %w", bin, err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	p := &proc{cmd: cmd, stdin: stdin, stdout: sc, events: make(chan pane.Event, 32)}
	go p.pump()

	if o.Resume != "" {
		_ = p.send(map[string]any{"type": "switch_session", "sessionPath": o.Resume})
	} else {
		_ = p.send(map[string]any{"type": "get_state"})
	}
	return p, nil
}

// proc is coppice's pane.Proc for one running `pi --mode rpc` process.
type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	events chan pane.Event

	mu            sync.Mutex
	pendingID     string
	pendingMethod string
	sessionID     string
	sessionFile   string
	streaming     bool
}

func (p *proc) send(v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (p *proc) WriteStdin(b []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.stdin.Write(b)
	return err
}

func (p *proc) Prompt(text string) error {
	p.mu.Lock()
	pendingID := p.pendingID
	streaming := p.streaming
	p.mu.Unlock()
	if pendingID != "" {
		return p.answerPending(text)
	}
	// rpc.md: "If the agent is streaming and no streamingBehavior is
	// specified, the command returns an error." agent_start opens the
	// streaming window; agent_settled closes it (agent_end alone may still
	// auto-retry/compact/continue, per pi's own docs).
	req := map[string]any{"type": "prompt", "message": text}
	if streaming {
		req["streamingBehavior"] = "steer"
	}
	return p.send(req)
}

func (p *proc) Steer(text string) error {
	return p.send(map[string]any{"type": "steer", "message": text})
}

func (p *proc) Events() <-chan pane.Event { return p.events }

// SessionID returns the value Start's Resume must be fed back for a real
// resume: pi's `switch_session` RPC command takes a `sessionPath` (a file
// path from get_state's `sessionFile`), never the bare `sessionId` --
// feeding it the short id instead makes resume silently fail, so this
// deliberately returns `sessionFile`. `HarnessSessionID` on emitted state
// events still carries the short id (see emitState), matching master §3.1's
// "the harness's own id" more legibly for display, at the (accepted) cost
// that pumpAdapter's own `p.SessionID()` call for display purposes also
// sees the file path, not the short id -- plan-02's Proc interface gives
// only one hook for both roles, and correctness of resume wins.
func (p *proc) SessionID() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessionFile, p.sessionFile != ""
}

func (p *proc) Stop() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *proc) answerPending(text string) error {
	p.mu.Lock()
	id, method := p.pendingID, p.pendingMethod
	p.pendingID, p.pendingMethod = "", ""
	p.mu.Unlock()
	resp := map[string]any{"type": "extension_ui_response", "id": id}
	if method == "confirm" {
		lower := strings.ToLower(strings.TrimSpace(text))
		resp["confirmed"] = lower == "y" || lower == "yes"
	} else {
		resp["value"] = text
	}
	return p.send(resp)
}

func isDialogMethod(m string) bool {
	switch m {
	case "select", "confirm", "input", "editor":
		return true
	default:
		return false
	}
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// emitState sends a state transition. It does not set HarnessSessionID
// itself -- pumpAdapter (internal/server/panes.go) calls Proc.SessionID()
// once per event and stamps proto.PaneStateEvent.HarnessSessionID from
// that, so this adapter's own event has nothing to carry there.
func (p *proc) emitState(st, detail string, ask *proto.Ask) {
	p.events <- pane.Event{Kind: pane.EvState, State: st, Detail: detail, Ask: ask}
}

func (p *proc) pump() {
	defer close(p.events)
	for p.stdout.Scan() {
		line := p.stdout.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		typ, _ := raw["type"].(string)
		switch typ {
		case "agent_start":
			p.mu.Lock()
			p.streaming = true
			p.mu.Unlock()
			p.emitState(proto.StateWorking, "", nil)
		case "agent_end":
			p.emitState(proto.StateIdle, "", nil)
		case "agent_settled":
			p.mu.Lock()
			p.streaming = false
			p.mu.Unlock()
			p.emitState(proto.StateIdle, "", nil)
		case "extension_ui_request":
			method, _ := raw["method"].(string)
			id, _ := raw["id"].(string)
			if isDialogMethod(method) {
				summary := firstNonEmpty(strOf(raw["title"]), strOf(raw["message"]))
				p.mu.Lock()
				p.pendingID, p.pendingMethod = id, method
				p.mu.Unlock()
				p.emitState(proto.StateBlocked, "", &proto.Ask{
					ID: id, Tool: method, Summary: summary,
					Deadline: float64(time.Now().Unix()) + 90,
				})
			}
		case "extension_error":
			p.emitState(proto.StateUnknown, strOf(raw["error"]), nil)
		case "message_update":
			if ame, ok := raw["assistantMessageEvent"].(map[string]any); ok {
				if ame["type"] == "text_delta" {
					if delta, ok := ame["delta"].(string); ok && delta != "" {
						p.events <- pane.Event{Kind: pane.EvText, Text: delta}
					}
				}
			}
		case "response":
			if raw["command"] == "get_state" {
				if data, ok := raw["data"].(map[string]any); ok {
					p.mu.Lock()
					p.sessionID = strOf(data["sessionId"])
					p.sessionFile = strOf(data["sessionFile"])
					p.mu.Unlock()
				}
			}
		}
	}
	p.events <- pane.Event{Kind: pane.EvEnd}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd harness/coppice && go test ./internal/adapters/pi/... -v`
Expected: 6 passed.

- [ ] **Step 5: `go vet`, `gofmt`, and a race check**

Run: `cd harness/coppice && gofmt -l internal/adapters/pi/ && go vet ./internal/adapters/pi/... && go test ./internal/adapters/pi/... -race`
Expected: `gofmt -l` prints nothing (already formatted); `go vet` clean; race run passes.

- [ ] **Step 6: Confirm the registry test still passes with the real adapter in place**

Run: `cd harness/coppice && go test ./internal/adapters/... -v`
Expected: `TestNamesListsEverySlotIncludingTheUnbuiltOnes` still passes (unchanged — pi still
registers under the name "pi"); `TestUnbuiltAdaptersRefuseToStart` now iterates only over
`{"opencode"}`, not `{"pi", "opencode"}` — update that test in plan-02's own tree if it still
lists "pi", since pi is no longer a `NotBuilt` slot after this task lands.

- [ ] **Step 7: Add the skip-marked live test named in spec-04's Tests section**

```go
// append to harness/coppice/internal/adapters/pi/pi_test.go
func TestLivePiSpawnsAndReachesTheGateLog(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi not on PATH; this test needs a real pi install (spec-04 Tests: 'Live')")
	}
	t.Skip(
		"needs a running resident gate plus coppice's own gate-log reader to " +
			"assert against, neither of which exists in this Go-only package. " +
			"Wire this once plan-02's server and plan-01's gate log reader land " +
			"(tracked as a named, honest gap here, not a silent one).",
	)
}
```

Add `"os/exec"` to the import block.

- [ ] **Step 8: Run the full adapter package test one more time**

Run: `cd harness/coppice && go test ./internal/adapters/pi/... -v`
Expected: 6 passed, 1 skipped (`TestLivePiSpawnsAndReachesTheGateLog`, reason printed).

- [ ] **Step 9: Commit**

```bash
git add harness/coppice/internal/adapters/pi/pi.go harness/coppice/internal/adapters/pi/pi_test.go
git commit -m "feat(coppice): drive pi --mode rpc as a headless pane, filling plan-02's registered slot"
```

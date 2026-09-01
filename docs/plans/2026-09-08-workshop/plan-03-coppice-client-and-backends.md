# Plan 03: The floor in the cockpit, `daisugi coppice`, and three pane backends

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the operator a floor screen in the cockpit and a `daisugi coppice` command group that spawn, read, prompt, steer, allow, deny, close, and attach panes through one `PaneBackend` protocol implemented three times (coppice, Herdr, tmux) under a single contract suite.

**Architecture:** `opendaisugi.floor` gains three backends behind `registry.pick_backend`, a schema-driven Python evaluator for Herdr's TOML screen manifests, and a debounced notify shim. The cockpit gains `FloorScreen` (roster + grid + peek) and `GridWidget`; the CLI gains a `coppice` group. Every backend is a client: the server owns the state, the client renders it, and closing the TUI loses nothing.

**Tech Stack:** Python 3.12 stdlib (socket, subprocess, tomllib, shutil), Textual for the two new screens, tmux ≥ 3.2 and the `herdr` / `coppice` binaries as optional hosts.

**Spec:** `docs/plans/2026-09-08-workshop/spec-03-coppice-client-and-backends.md`, arguing from `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.2, §5.1, §5.5, §7.

**Requires:** plan 01 (`opendaisugi.floor.events`, `opendaisugi.floor.backend`) and plan 02 (`harness/coppice`: the socket server, the vendored Herdr manifests under `harness/coppice/internal/detect/manifests/`, the screen fixtures under `harness/coppice/testdata/screens/`).
**Provides:** `floor/manifests.py`, `floor/registry.py`, `floor/coppice_backend.py`, `floor/herdr_backend.py`, `floor/tmux_backend.py`, `floor/notify.py`, `tui_grid.py`, `tui_floor.py`, the `daisugi coppice` group, the `floor` stage in `modules.py` / `swap.py`.

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 0, 8, 9 and 10 are ready to build now** — the host-fact table, the debounced notifier,
the config/swap/modules work and the grid widget were checked against the real code and hold:
the nested-swap diagnosis is empirically true (`Config().model_copy(update={"floor.backend":
"tmux"})` puts a dotted key in `__dict__` and `model_dump(mode="json")` drops it, so
`save_config` writes nothing), `set_field`'s recursive rebuild fixes it, the tests read the file
back rather than trusting the return value, and the two invariants in `tests/test_swap.py`
(`test_effect_is_three_valued_and_covers_every_stage` compares `set(STAGE_EFFECT)` against the
stage keys; `test_every_module_knob_option_names_a_real_module_in_the_map` skips `kind="setting"`)
both still pass with the plan's edits. The defects cluster in tasks 1, 4, 6, 11 and 12.

- **BLOCKER — Task 11: attach still eats Tab, the key Claude Code needs most.** The mechanism
  ("guard the four App actions with `if self._passthrough(): return`") does not stop Textual
  consuming the key. `textual/app.py:4136` runs `await self._check_bindings(event.key,
  priority=True)` *before* forwarding to the screen, and `_dispatch_action` (`app.py:4272-4289`)
  returns `True` as soon as the action method exists and is invoked, whatever it returns; only
  `SkipAction` makes it return `False`. Reproduced with the plan's exact code on textual 8.2.8:
  `AttachScreen.on_key` saw `['q', 'colon', 'question_mark', 'h']` and never `tab`. So the
  plan's own assertion `assert "tab" in sent` fails, and the spec's law ("it must not eat keys
  the harness needs; the leader is the only intercepted chord") is broken. Fix: `from
  textual.actions import SkipAction`, and in each guarded action write `if self._passthrough():
  raise SkipAction()`. Verified: the same repro then sees `['tab', 'q', 'colon',
  'question_mark', 'h']` and `action_cycle` still does not switch views. — applied in Task 11 step 5 (`q`, `colon`,
  `question_mark` and `escape` are not `priority=True`, so `event.stop()` in `on_key` already
  covers them; the guard on those three actions is harmless but does nothing.)
- **BLOCKER — Task 11: nothing ever calls `subscribe()`, so the floor is inert.**
  `GridWidget.FrameArrived` is defined (line 3960) and its handler written (line 4006), but no
  task posts it: `grep -n 'run_worker\|call_from_thread'` over the whole plan returns nothing,
  and `FloorScreen.on_pane_state` is only ever called by tests. As written, the grid stays blank
  forever (attach on any backend shows an empty screen while swallowing every key), the bell
  never rings, the "row flashes once" in the spec is implemented nowhere, and a configured
  `notify_cmd` never runs. A control the operator configured that does nothing is exactly what
  master §3.5 forbids. Fix: add a step to task 11 that starts the pump on mount —
  `self._worker = self.run_worker(self._pump, thread=True, exclusive=True)` iterating
  `self.backend.subscribe()`, using `self.app.call_from_thread` to post
  `GridWidget.FrameArrived` for a `Frame` whose `pane` is the selected pane and to call
  `on_pane_state` for a `PaneStateEvent` — cancel it in `on_unmount`, and add a pilot test that
  a `Frame` pushed through the worker path reaches `render_lines_text()`. — applied in Task 11 step 4 (`_pump`) and step 1 (tests)
- **BLOCKER — Task 6: `_authority_is_daisugi` forges `source: "gate"`.** `return "daisugi" in
  json.dumps(parsed).casefold()` matches the substring anywhere in Herdr's `agent explain`
  JSON: a cwd, a pane label, a command line. On the operator's own box every pane under
  `/home/user/openDaisugi` matches, so Herdr's probabilistic screen guess is
  stamped `gate` — the top-but-one rank in master §3.1, and the one thing §5.1 says is "not a
  guess, it is a fact we produced". That quietly re-decides the crux this whole project competes
  on. Fix: add `"authority_field": ["authority", "source"]` to `herdr_verbs.json` with the
  file's existing `verified: false`, walk exactly that path, require
  `str(value).casefold() == "daisugi"`, and return `False` for a missing field, a non-dict, or
  any other shape. — applied in Task 6 steps 1, 2, 4
- **BLOCKER — Task 4: tmux `subscribe()` never emits `done`, and the plan's own test asserts it
  does.** `last` is only populated from panes that already had a non-`None` state, and
  `_state_for` returns `None` whenever no manifest matched — which is every pane on a normal
  install, because `DEFAULT_MANIFEST_DIRS` holds only `~/.config/coppice/agent-detection` and the
  vendored manifests live in the Go tree. So a pane that exits is never announced, and
  `test_a_finished_pane_reports_done_from_the_process_not_a_manifest` runs to its 4 s deadline
  and fails on every box that has tmux. (The contract suite still passes, because its step 3
  accepts "left the list" — the bug is in `subscribe`, not in the contract.) Fix: keep a
  `seen: set[str]` of pane ids from each `list()` poll, independent of state, and emit `done`
  for any id that leaves it; and change the test to spawn `sh -c 'sleep 1'`, observe the pane
  once, then let it exit, so it is certainly seen before it goes. — applied in Task 4 steps 1 and 3

- **SHOULD-FIX — Task 1: `--check` can never pass twice.** `derive_schema` stamps
  `"recorded_at": datetime.now(UTC).date().isoformat()`, and `main(--check)` compares the whole
  serialized document, so `test_the_pin_still_matches_the_vendored_files` goes red the day after
  the pin is recorded on any box that has the manifests. Fix: compare with `recorded_at`
  excluded (compare `files`, `rules_key`, `agent_key`, `rule_fields`, `states`, `regions`,
  `top_keys`). — applied in Task 1 step 1
- **SHOULD-FIX — Task 1: the pin lies about its own provenance under `--from`.**
  `derive_schema` always writes `"recorded_from": str(DEFAULT_DIR.relative_to(REPO_ROOT))`, even
  when `--from` pointed at a herdr checkout. Record the directory actually read, and pass it in. — applied in Task 1 step 1
- **SHOULD-FIX — Task 2: the pattern semantics are a guess the recorder does not record.**
  `_match` uses `all(re.search(p, text) for p in rule.patterns)` (AND) and matches
  case-sensitively. Neither was read off Herdr's files, and the Go evaluator in plan 02 has to
  agree or the shared-fixture conformance test is decided by luck. Fix: have the recorder emit
  `"pattern_combinator": "all"|"any"` and `"case_sensitive": true|false` as unverified fields,
  have `manifests.py` read them, and state the chosen semantics in the module docstring so plan
  02 can match it. — applied in Task 1 step 1 and Task 2 step 3
- **SHOULD-FIX — Task 2: `DEFAULT_MANIFEST_DIRS` makes the fallback dead on arrival.** It holds
  only the user override dir, so on a normal install nothing is loaded and every tmux pane is
  `unknown` forever. Fix: put the bundled dir first (packaged with the wheel, or located via
  `COPPICE_MANIFESTS` then the repo path), keep the override dir last, and say in the docstring
  what happens when neither exists. — applied in Task 2 step 3
- **SHOULD-FIX — Task 2: the Go/Python agreement test invents a fixture layout plan 02 never
  promised.** `agent, filename = rel.split("/")[-2], rel.split("/")[-1]` plus
  `path_state = filename.split("-")[0]` and `assert want["state"] == path_state` hardcode a
  `<agent>/<state>-*.txt` convention. Fix: read `agent`, `state` and `rule` from
  `expectations.json` only, and drop the path-derived assertion — or cite the plan-02 task that
  fixes the layout. — applied in Task 2 step 1
- **SHOULD-FIX — Task 3: `registry.py` fails `ruff check .` as written.** `import time`, a blank
  line, then `from dataclasses import dataclass` trips `I001` (ruff `lint.select` includes `I`,
  `pyproject.toml:145`); confirmed by running ruff on the exact block. Fix: one stdlib block,
  `from dataclasses import dataclass` before `import time`. — applied in Task 3 step 3. Ruff's
  isort puts straight imports first, so the sorted form is `import time` then
  `from dataclasses import dataclass` in ONE block; verified with
  `ruff check --select I` on both orderings.
- **SHOULD-FIX — Task 5: the autostart docstring describes behaviour nothing implements.**
  `registry.build_backend` constructs `CoppiceBackend()` with the default `autostart=False`, so
  no code path in this plan ever passes `autostart=True` except the contract suite and the
  real-binary test, yet the module docstring says "`daisugi coppice spawn` and an explicit
  `--backend coppice` pass `autostart=True`". Spec-03 also says `available()` starts the server
  once. Either thread `autostart` through `build_backend`/`pick_backend` and pass it from
  `coppice spawn` and `--backend coppice`, or delete the sentence and record the deviation from
  spec-03 explicitly. Do not leave the docstring claiming it. — applied in Task 3 step 3, Task 5 step 3, Task 12 step 4
- **SHOULD-FIX — Task 5: validate the state and source enum on the coppice path.** Plan 01's
  `PaneStateEvent.from_json` checks required fields only (`plan-01` lines 1148-1180); it does
  **not** check `state` against `STATES` or `source` against `SOURCES` — that is a separate
  `_validate_hook_report_row` (plan-01 line 677). So a server that emits `state: "ready"` paints
  an unknown word in the roster where `unknown` belongs. Fix: in `list()` and `subscribe()`,
  drop any event whose `state` is not in `STATES` or whose `source` is not in `SOURCES`. —
  applied in Task 5 steps 1 and 3
- **SHOULD-FIX — Task 6/12: `spawn(..., harness=...)` is contract drift, and the workaround
  hides bugs.** Only `CoppiceBackend.spawn` takes `harness`; master §3.2's protocol does not
  have it, and neither tmux nor herdr accept it. Task 12 papers over this with `try:
  chosen.spawn(**kwargs) except TypeError: kwargs.pop("harness"); ...`, which also swallows any
  genuine `TypeError` raised *inside* `spawn` and silently retries. Fix: add
  `harness: str | None = None` to the `PaneBackend` protocol and to all three backends (tmux and
  herdr ignore it, documented), and delete the except-TypeError retry. — applied in Task 4
  step 3, Task 5 step 3, Task 6 step 4, Task 12 step 4
- **SHOULD-FIX — Task 6: `STATE_OUT["done"] = "idle"` writes a lie into Herdr's UI.** A pane
  whose process exited is shown to a Herdr user as `idle`, which in Herdr means "ready for
  work". Herdr has no `done`, and `unknown` is the honest cell. Fix: map `done` → `unknown` and
  put `done` in `--message`; update the test that pins `STATE_OUT["done"] == "idle"`. —
  applied in Task 6 steps 1 and 4
- **SHOULD-FIX — Task 6: pin the rest of the argv, and stop dropping key errors.** `pane
  send-text`, `pane send-keys`, `pane close`, `pane list` and `agent prompt` are hardcoded
  literals while the pin exists precisely so they are not. They are correct (see NOTE below), so
  record them in `herdr_verbs.json`. Separately, `_run` ignores the return code, so a key name
  Herdr rejects is silently dropped and the contract's
  `test_send_keys_accepts_the_names_the_floor_binds` passes on a no-op. Fix: pin a key map
  alongside the verbs, and raise when `pane send-keys` exits non-zero. — applied in Task 6
  steps 1, 2, 4
- **SHOULD-FIX — Task 7: the contract never exercises `subscribe()` or `report_state()`.** Both
  are in the §3.2 protocol and both can run against all three backends, so by master §7 they
  belong in the contract suite. A backend can pass the "contract" today with both broken — which
  is how blocker 4 got through. Add one test that a pane's exit reaches `subscribe()` within the
  budget, and one that `report_state` does not raise. — applied in Task 7 step 1
- **SHOULD-FIX — Task 11: the roster poll and the notifier both block the event loop.**
  `on_mount` does `self.set_interval(1.0, self.reload)` and `reload()` calls `backend.list()`
  synchronously; on tmux that is one `list-panes` plus one `capture-pane` per pane, every
  second, on the UI thread. `on_pane_state` then calls `self.notifier.notify(ev)`, which runs a
  subprocess with a 5 s timeout, also on the UI thread — a slow `ntfy` freezes the cockpit for
  five seconds at exactly the moment the operator needs it. Fix: run both through
  `self.run_worker(..., thread=True)`. — applied in Task 11 step 4
- **SHOULD-FIX — Task 11: attach on tmux or herdr is a blank screen that eats every key.** Only
  the coppice backend yields frames (master §3.2), so `AttachScreen` paints nothing on the other
  two while `on_key` swallows `q`, `:` and `?`. Fix: `action_attach` refuses on a backend with no
  frames and teaches the substrate's own command (`tmux attach -t …`, `herdr …`), or the screen
  polls `read(source="visible")` on a timer and paints the text. Pick one and write it down. — applied in Task 11 step 4: `action_attach` refuses on a backend
  that yields no frames and names the substrate's own attach command.
- **SHOULD-FIX — Task 12: the `coppice` group has no `--data-dir`.** `_floor_config()` reads
  `DEFAULT_DATA_DIR / "config.yaml"`, so every test in `test_cli_coppice.py` reads the
  developer's real `~/.opendaisugi/config.yaml`, and the group is the only one that cannot be
  pointed elsewhere (compare `cli.py:1679`, `cli.py:1805`). Fix: add
  `data_dir: Path = typer.Option(DEFAULT_DATA_DIR, "--data-dir", ...)` and thread it through
  `_floor_config`. — applied in Task 12 steps 1 and 4

- **NOTE — the Herdr facts the plan pins are correct.** Verified against
  `https://herdr.dev/docs/cli-reference/` on 2026-09-08: there is no `herdr pane create`; a pane
  comes from `tab create [--workspace] [--cwd PATH] [--label TEXT] [--env KEY=VALUE]` or
  `pane split ... --direction right|down`, then `pane run <pane_id> <command>`, then
  `pane rename <pane_id> <label>`. `--json` is documented on `session list`, `session stop`,
  `session delete`, `machine list`, `server agent-manifests`, `plugin list` and
  `agent explain <target> [--json|--verbose]`, and **not** on `pane list [--workspace]` or
  `agent list`. `pane read <pane_id> [--source visible|recent|recent-unwrapped|detection]
  [--lines N] [--format text|ansi]` and `pane report-agent <pane_id> --source ID --agent LABEL
  --state idle|working|blocked|unknown [--message TEXT]` match the plan's argv exactly, as do
  `pane send-text <pane_id> <text>`, `pane send-keys <pane_id> <key> [key ...]`,
  `pane close <pane_id>` and `agent prompt <target> <text> [--wait] [--until STATUS]
  [--timeout MS]` (so `--timeout` in **milliseconds** is right). `pane kill` does not exist and
  the plan does not use it. — no change needed; the pins already match.
- **NOTE — the tmux facts in task 4's preamble are correct.** Probed on this box (tmux 3.7b):
  cold `new-session -d -s NAME -c DIR -P -F '#{pane_id}' -n LABEL -x 120 -y 40 -e K=V CMD`
  prints `%0` and the env reaches the process; `kill-pane` and `send-keys` on a gone or bogus
  target both exit 1 with `can't find pane: …`, so `close()`'s string check holds; `tmux -L NAME
  -V` prints the version without starting a server (no change needed). One correction to the reasoning: `-e` on
  `new-window` works on 3.7b and shipped before 3.2 — the 3.2 gate is right for `new-session`
  and merely conservative for `new-window`, which is fine. — applied in Task 4 (preamble)
- **NOTE — `#{pane_start_command}` comes back quoted.** — applied in Task 4 step 3 tmux 3.7b returns
  `"sh -c 'echo READY; sleep 30'"` *including* the double quotes, so `shlex.split(start)` yields
  one element holding the whole string, and `_harness_of` shows that blob as the harness. Strip a
  surrounding quote pair before splitting.
- **NOTE — `Enter` does not double-fire.** Reproduced the plan's shape (a screen `Binding("enter",
  "attach")` plus `on_data_table_row_selected` → `action_confirm` → `action_attach`) on textual
  8.2.8: the focused `DataTable` consumes the key and only `RowSelected` runs, so exactly one
  `AttachScreen` is pushed. No change needed.
- **NOTE — no backend reports a bare `idle`.** Checked end to end, since the plan turns on it:
  `classify` returns `("unknown", None)` for no match and for an ambiguous multi-manifest match;
  `_ALLOWED_STATES` drops any `done` rule at load; tmux `_state_for` returns `None` rather than
  a state when nothing matched; herdr drops a row whose state is not in the pinned set; coppice
  sets `state=None` when `from_json` fails; `wait_for_state` returns `None` on timeout instead of
  inventing an event. The single place `idle` is manufactured is `STATE_OUT["done"]`, covered
  above. — applied in Task 6 step 4: `done` now maps to `unknown`, so nothing in this plan
  manufactures an `idle`.
- **NOTE — `available()` starting a server would be a side effect of a diagnostic.**
  `daisugi coppice backends` calls `available()` on all three. Keeping `autostart=False` the
  default (as task 5 does) is the right call; if the SHOULD-FIX above threads autostart through,
  make sure `backend_statuses` never passes it. — applied in Task 3 step 3
- **NOTE — the mixin is not quite verbatim.** `action_confirm_allow` records
  `reason="operator allowed from the cockpit"` where `tui_sessions.py:303` says "from the
  sessions view", and the explanatory comments on `_needs_strong_guard` / `action_arm_allow`
  (the S2 reasoning) are dropped. No test asserts the reason string, but a safety-audit reason is
  not a place to drift silently: keep the originals or say why they changed. — applied in
  Task 11 step 3: the mixin keeps the S2 comments and takes the origin phrase from
  `_ask_origin`, so the sessions screen still writes "from the sessions view".
- **NOTE — line citations drift by ten to twenty lines.** `tui_sessions.py`'s ask methods are at
  223-320 (plan says 236-300); `resolved_config`'s `out = [...]` is near 302 (plan says 320);
  `DecompositionError` is at `exceptions.py:44` (plan says 41). All still findable. — applied in Task 9 (Files) and Task 11 (Files): citations corrected.
- **NOTE — `skip_reason` returns an em-dash.** `f"{fact.why_not} — {fact.fix}"` breaks the STE100
  constraint in master §4 for a string an operator reads in test output. Use a full stop. — applied in Task 0 step 3
- **NOTE — two thin spots.** `test_a_state_a_backend_reports_is_never_a_bare_idle_guess` never
  asserts what its name says (it checks the source enum and the manifest/`done` rule only);
  rename it or add the assertion. `daisugi coppice read --source` is unvalidated, and the tmux
  backend silently falls back to `visible` for an unknown source — have `read()` reject it the
  way the herdr backend does. — applied in Task 4 step 3, Task 7 step 1, Task 12 step 4
- **NOTE — two low-probability flakes.** `TmuxBackend.available()` returns `False` when
  `tmux -V` is unparsable (`tmux master`), so a working tmux would skip; base `available()` on
  `shutil.which` and keep the version tuple for the `-e` decision only. The fake `herdr` in
  `test_herdr_backend.py` is a `python3` script probed with a 500 ms budget; make it `#!/bin/sh`.
  — applied in Task 4 steps 1 and 3, Task 6 step 2

Re-review 2026-09-08: 18 of 19 verified applied; open: SHOULD-FIX Task 6/12 harness
contract drift — `spawn(..., harness=...)` was added to `TmuxBackend`, `CoppiceBackend`,
`HerdrBackend` (tasks 4/5/6) and the CLI's except-TypeError retry was removed (task 12
step 4), but no task adds `harness` to the `PaneBackend` Protocol itself; plan-03's own
Global Constraints say `PaneBackend` is "never redefined here" (line ~272, ~6920), so
master §3.2 still does not declare `harness` and the drift is only half-closed.

## Global Constraints

Copied verbatim from master spec §4. Every task's requirements implicitly include this section.

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
- **`/tmp` may be RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

### Constraints this plan adds

- **Layer purity, applied here.** `config.py`, `swap.py`, and `modules.py` are layer modules.
  They must never `import opendaisugi.floor`. They detect a backend with `shutil.which(...)`
  and `os.lstat(...)` only, exactly the way `modules.py` already probes packages with
  `importlib.util.find_spec`. `cli.py`, `tui.py`, `tui_floor.py`, and `tui_grid.py` are cockpit
  modules and may import `opendaisugi.floor` freely.
- **Types consumed from plan 01, never redefined here:** `PaneRef(backend, id)`,
  `PaneInfo(ref, label, cwd, cmd, kind, state)`,
  `Frame(pane, seq, cols, rows, cursor, rows_changed)`,
  `PaneStateEvent(session_id, harness, state, source, ts, harness_session_id, pane, ask, detail)`
  with `to_json()` / `from_json()`, `Ask(id, tool, summary, deadline)`, the `PaneBackend` Protocol,
  and `merge(current, incoming, *, now)`. All import from `opendaisugi.floor`.
- **Every `available()` returns a bool and never raises** (master §3.2). A backend that cannot
  answer is unavailable, not an exception.
- **A manifest may never say `done`** (master §3.1). tmux infers `done` from a pane's absence and
  tags that event `source: "process"`.
- **Live host tests skip, never fail, and say which host is missing** (master §7). Every skip goes
  through `tests/floor/hostfacts.py` so the reason is one string in one place.

---

### Task 0: Host facts, and the one place a skip says why

**Files:**
- Create: `tests/floor/__init__.py`
- Create: `tests/floor/hostfacts.py`
- Test: `tests/floor/test_hostfacts.py`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `HostFact` frozen dataclass: `name: str`, `present: bool`, `why_not: str`, `fix: str`.
  - `host_facts() -> dict[str, HostFact]` with keys `"tmux"`, `"herdr"`, `"coppice"`,
    `"vendored_manifests"`, `"screen_fixtures"`.
  - `skip_reason(name: str) -> str | None` — None when present, else `"<why_not> — <fix>"`.
  - `REPO_ROOT: Path`, `MANIFEST_DIR: Path`, `SCREEN_FIXTURE_DIR: Path`.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_hostfacts.py
"""Host facts: the one place a floor test says which host it needs.

A skip that does not name the missing host is a lie of omission (master §7), and
a fact that claims a binary is present when it is not would turn every skip into
a false failure. Both directions are tested here.
"""

from __future__ import annotations

import shutil

from tests.floor import hostfacts


def test_every_fact_carries_a_reason_and_a_fix():
    for name, fact in hostfacts.host_facts().items():
        assert fact.name == name
        assert fact.why_not.strip(), f"{name} has no why_not"
        assert fact.fix.strip(), f"{name} has no fix"
        assert len(fact.fix) <= 90, f"{name}'s fix is too long to read in a skip line"
        assert fact.fix[0].islower(), f"{name}'s fix should start with a verb, not a capital"


def test_tmux_presence_matches_the_path():
    assert hostfacts.host_facts()["tmux"].present is (shutil.which("tmux") is not None)


def test_herdr_presence_matches_the_path():
    assert hostfacts.host_facts()["herdr"].present is (shutil.which("herdr") is not None)


def test_skip_reason_is_none_when_present_and_teaches_when_absent():
    facts = hostfacts.host_facts()
    for name, fact in facts.items():
        reason = hostfacts.skip_reason(name)
        if fact.present:
            assert reason is None
        else:
            assert reason is not None
            assert fact.fix in reason
            assert "—" not in reason, "STE100: no em-dash in a string an operator reads"


def test_vendored_manifest_fact_points_at_the_plan_02_directory():
    fact = hostfacts.host_facts()["vendored_manifests"]
    assert "harness/coppice/internal/detect/manifests" in fact.why_not
    assert fact.present is hostfacts.MANIFEST_DIR.is_dir()


def test_unknown_fact_name_teaches_the_known_ones():
    reason = hostfacts.skip_reason("nope")
    assert reason is not None and "tmux" in reason
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_hostfacts.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'tests.floor'`

- [ ] **Step 3: Write the implementation**

```python
# tests/floor/__init__.py
"""Floor tests: the backend contract suite, the manifest evaluator, and the screens."""
```

```python
# tests/floor/hostfacts.py
"""Which hosts this box has, why a floor test skips, and what to run to fix it.

Every live floor test asks here instead of calling ``shutil.which`` itself, so a
skipped run reports one consistent, teaching reason (master §7). Read-only: this
module starts nothing and installs nothing.
"""

from __future__ import annotations

import shutil
from dataclasses import dataclass
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
MANIFEST_DIR = REPO_ROOT / "harness" / "coppice" / "internal" / "detect" / "manifests"
SCREEN_FIXTURE_DIR = REPO_ROOT / "harness" / "coppice" / "testdata" / "screens"


@dataclass(frozen=True)
class HostFact:
    """One host requirement: is it here, why not, and the command that fixes it."""

    name: str
    present: bool
    why_not: str
    fix: str


def host_facts() -> dict[str, HostFact]:
    """Probe the box once. Never raises, never starts a server."""
    return {
        "tmux": HostFact(
            "tmux",
            shutil.which("tmux") is not None,
            "tmux is not on PATH",
            "install tmux 3.2 or newer with your package manager",
        ),
        "herdr": HostFact(
            "herdr",
            shutil.which("herdr") is not None,
            "herdr is not on PATH",
            "install herdr from herdr.dev, then run `herdr session list --json`",
        ),
        "coppice": HostFact(
            "coppice",
            shutil.which("coppice") is not None,
            "the coppice binary is not on PATH",
            "build it: `cd harness/coppice && go build ./cmd/coppice`",
        ),
        "vendored_manifests": HostFact(
            "vendored_manifests",
            MANIFEST_DIR.is_dir(),
            "harness/coppice/internal/detect/manifests is not in the tree",
            "run plan 02 first, or set COPPICE_MANIFESTS to a herdr checkout",
        ),
        "screen_fixtures": HostFact(
            "screen_fixtures",
            SCREEN_FIXTURE_DIR.is_dir(),
            "harness/coppice/testdata/screens is not in the tree",
            "run plan 02 first, it records the shared screen fixtures",
        ),
    }


def skip_reason(name: str) -> str | None:
    """None when the host is here. Otherwise one line that teaches the fix.

    Two sentences, no em-dash: this string lands in pytest's skip column, where an
    operator reads it (master §4 copy rule).
    """
    facts = host_facts()
    fact = facts.get(name)
    if fact is None:
        return f"unknown host fact {name!r} — known facts: {', '.join(sorted(facts))}"
    if fact.present:
        return None
    return f"{fact.why_not}. {fact.fix}."
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_hostfacts.py -q`
Expected: PASS (6 tests)

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add tests/floor/__init__.py tests/floor/hostfacts.py tests/floor/test_hostfacts.py
git commit -m "test(floor): one place a skipped floor test names the missing host

Every backend in this plan is optional. Without a shared fact table each suite
invents its own skip string, and a reader cannot tell 'not installed' from
'broken'. One table, one reason, one fix per host."
```

---

### Task 1: Discovery — record the Herdr manifest TOML schema from the vendored files

The schema is not published anywhere we can cite. `herdr.dev/docs/agents/` names the manifests and
the override directory but not their fields, and the DeepWiki mirror says the same. So this task
reads the real files plan 02 vendored and writes down what they contain. Task 2 reads that record.
Nothing in this plan hardcodes a field name that was not read off disk.

**Files:**
- Create: `scripts/floor_manifest_schema.py`
- Create: `src/opendaisugi/floor/manifest_schema.json` (generated by the script, committed)
- Test: `tests/floor/test_manifest_schema.py`

**Interfaces:**
- Consumes: `tests/floor/hostfacts.py` (`MANIFEST_DIR`, `skip_reason`).
- Produces:
  - `scripts/floor_manifest_schema.py` with `derive_schema(paths: Sequence[Path]) -> dict` and
    `main(argv: list[str]) -> int`.
  - `src/opendaisugi/floor/manifest_schema.json` with the recorded keys:
    `recorded_from`, `recorded_at`, `verified`, `files` (name → sha256), `agent_key`,
    `rules_key`, `rule_fields` (roles `name` / `state` / `patterns` / `region` → real field
    names), `states`, `regions`, plus the two DECLARED (not read) semantics
    `pattern_combinator` ("all") and `case_sensitive` (true) with `semantics_verified: false`.
  - `derive_schema(paths, *, source_dir)` records the directory actually read.
  - `main(["--check"])` compares every key except `recorded_at`.

- [ ] **Step 1: Write the recorder script**

```python
#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# dependencies = []
# ///
"""Record the Herdr agent-detection TOML schema from the vendored manifest files.

openDaisugi evaluates the same manifests Herdr does (master spec §5.5). Herdr does
not publish the file format, so we read it off the files plan 02 vendored and pin
what we found. `floor/manifests.py` loads this pin and refuses any manifest that
uses a key the pin does not name, so a Herdr format change fails closed instead of
classifying a screen with half a rule.

Usage:
    ./scripts/floor_manifest_schema.py                       # read the vendored dir
    ./scripts/floor_manifest_schema.py --from PATH           # read a herdr checkout
    ./scripts/floor_manifest_schema.py --check               # exit 1 if the pin is stale
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import tomllib
from collections.abc import Sequence
from datetime import UTC, datetime
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_DIR = REPO_ROOT / "harness" / "coppice" / "internal" / "detect" / "manifests"
PIN = REPO_ROOT / "src" / "opendaisugi" / "floor" / "manifest_schema.json"

# The four roles the evaluator needs from a rule, and the field names we accept
# for each. The first name found in a real file wins and is recorded; a rule
# field we do not recognise is recorded under "unmapped" so a human decides.
_ROLE_CANDIDATES = {
    "name": ("name", "id", "rule"),
    "state": ("state", "status"),
    "patterns": ("patterns", "pattern", "match", "matches", "regex"),
    "region": ("region", "source", "scope", "where"),
}


def _sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


# The fields the pin declares but cannot read off the files. Herdr does not publish
# whether a rule's patterns are ANDed or ORed, nor whether matching is case sensitive.
# We record our CHOICE here, flagged unverified, so plan 02's Go evaluator reads the
# same two values and the shared-fixture conformance test is decided by the pin rather
# than by two independent guesses.
_SEMANTICS = {"pattern_combinator": "all", "case_sensitive": True, "semantics_verified": False}

# Everything except `recorded_at`, which changes every day and would make `--check`
# fail the day after the pin was recorded.
_COMPARED_KEYS = (
    "files",
    "top_keys",
    "agent_key",
    "rules_key",
    "rule_fields",
    "unmapped_rule_fields",
    "states",
    "regions",
    "verified",
    "pattern_combinator",
    "case_sensitive",
    "semantics_verified",
)


def derive_schema(paths: Sequence[Path], *, source_dir: Path) -> dict:
    """Read every manifest and report the keys, roles, states, and regions found.

    ``source_dir`` is recorded verbatim, so a pin taken from a herdr checkout with
    ``--from`` does not claim it came from the vendored directory.
    """
    top_keys: set[str] = set()
    rules_key: str | None = None
    agent_key: str | None = None
    rule_field_names: set[str] = set()
    states: set[str] = set()
    regions: set[str] = set()
    files: dict[str, str] = {}

    for path in sorted(paths):
        doc = tomllib.loads(path.read_text(encoding="utf-8"))
        files[path.name] = _sha256(path)
        top_keys.update(doc)
        for key, value in doc.items():
            if isinstance(value, list) and value and isinstance(value[0], dict):
                rules_key = rules_key or key
                for rule in value:
                    rule_field_names.update(rule)
                    for role, names in _ROLE_CANDIDATES.items():
                        for candidate in names:
                            got = rule.get(candidate)
                            if got is None:
                                continue
                            if role == "state":
                                states.add(str(got))
                            if role == "region":
                                regions.add(str(got))
            elif isinstance(value, str) and key in ("agent", "name", "id"):
                agent_key = agent_key or key

    roles: dict[str, str] = {}
    for role, names in _ROLE_CANDIDATES.items():
        match = next((n for n in names if n in rule_field_names), None)
        if match is not None:
            roles[role] = match
    unmapped = sorted(rule_field_names - set(roles.values()))

    try:
        recorded_from = str(source_dir.resolve().relative_to(REPO_ROOT))
    except ValueError:
        recorded_from = str(source_dir.resolve())  # a herdr checkout outside the repo
    return {
        **_SEMANTICS,
        "recorded_from": recorded_from,
        "recorded_at": datetime.now(UTC).date().isoformat(),
        "verified": bool(files),
        "files": files,
        "top_keys": sorted(top_keys),
        "agent_key": agent_key,
        "rules_key": rules_key,
        "rule_fields": roles,
        "unmapped_rule_fields": unmapped,
        "states": sorted(states),
        "regions": sorted(regions),
    }


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--from", dest="src", type=Path, default=DEFAULT_DIR)
    parser.add_argument("--check", action="store_true", help="Compare, do not write.")
    args = parser.parse_args(argv)

    paths = sorted(args.src.glob("*.toml")) if args.src.is_dir() else []
    if not paths:
        print(f"no manifests under {args.src}", file=sys.stderr)
        print("run plan 02 first, it vendors herdr's manifests", file=sys.stderr)
        print("or point --from at a herdr checkout's src/detect/manifests", file=sys.stderr)
        return 1

    schema = derive_schema(paths, source_dir=args.src)
    text = json.dumps(schema, indent=2, sort_keys=True) + "\n"
    if args.check:
        try:
            current = json.loads(PIN.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            current = {}
        # `recorded_at` is excluded: it moves every day and says nothing about drift.
        stale = any(current.get(k) != schema.get(k) for k in _COMPARED_KEYS)
        if stale:
            print(f"{PIN} is stale. Run ./scripts/floor_manifest_schema.py.", file=sys.stderr)
            return 1
        return 0
    PIN.write_text(text, encoding="utf-8")
    print(f"recorded {len(paths)} manifests to {PIN}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
```

- [ ] **Step 2: Write the failing test**

```python
# tests/floor/test_manifest_schema.py
"""The recorded Herdr manifest schema is a fact, not a guess.

The pin is committed so `manifests.py` works on a box with no herdr checkout. The
first test proves the pin still matches the vendored files when they are present.
The rest prove the pin is well formed and that an unverified pin is honest about
being empty rather than pretending to know the format.
"""

from __future__ import annotations

import json

import pytest

from tests.floor import hostfacts

PIN = hostfacts.REPO_ROOT / "src" / "opendaisugi" / "floor" / "manifest_schema.json"


def _pin() -> dict:
    return json.loads(PIN.read_text(encoding="utf-8"))


def test_the_pin_is_committed():
    assert PIN.exists(), "run ./scripts/floor_manifest_schema.py and commit the pin"


def test_the_pin_names_where_it_came_from():
    pin = _pin()
    assert pin["recorded_from"].endswith("internal/detect/manifests")
    assert pin["recorded_at"]
    assert isinstance(pin["verified"], bool)


def test_an_unverified_pin_claims_no_field_names():
    """A pin recorded with no files must not invent a schema (fail closed)."""
    pin = _pin()
    if pin["verified"]:
        pytest.skip("the pin was recorded from real manifests")
    assert pin["files"] == {}
    assert pin["rule_fields"] == {}
    assert pin["rules_key"] is None


def test_a_verified_pin_maps_every_role_the_evaluator_needs():
    pin = _pin()
    if not pin["verified"]:
        pytest.skip("the pin is unverified — run plan 02, then the recorder")
    for role in ("name", "state", "patterns"):
        assert role in pin["rule_fields"], f"no field found for role {role}"
    assert pin["rules_key"]
    assert pin["states"], "no states were read off the manifests"


def test_the_pin_still_matches_the_vendored_files():
    reason = hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    import subprocess

    proc = subprocess.run(
        [str(hostfacts.REPO_ROOT / "scripts" / "floor_manifest_schema.py"), "--check"],
        capture_output=True,
        text=True,
    )
    assert proc.returncode == 0, proc.stderr


def test_check_ignores_recorded_at_so_it_does_not_go_stale_overnight(tmp_path, monkeypatch):
    """A date stamp is not drift. --check must compare the schema, not the day."""
    import importlib.util
    import json as _json

    spec = importlib.util.spec_from_file_location(
        "floor_manifest_schema",
        hostfacts.REPO_ROOT / "scripts" / "floor_manifest_schema.py",
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    pin = tmp_path / "pin.json"
    monkeypatch.setattr(module, "PIN", pin)
    src = tmp_path / "manifests"
    src.mkdir()
    (src / "x.toml").write_text('agent = "x"\n', encoding="utf-8")
    assert module.main(["--from", str(src)]) == 0
    stored = _json.loads(pin.read_text())
    stored["recorded_at"] = "1999-01-01"
    pin.write_text(_json.dumps(stored, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    assert module.main(["--from", str(src), "--check"]) == 0


def test_the_pin_records_the_directory_it_actually_read(tmp_path):
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "floor_manifest_schema2",
        hostfacts.REPO_ROOT / "scripts" / "floor_manifest_schema.py",
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    src = tmp_path / "herdr-checkout" / "manifests"
    src.mkdir(parents=True)
    (src / "x.toml").write_text('agent = "x"\n', encoding="utf-8")
    schema = module.derive_schema(sorted(src.glob("*.toml")), source_dir=src)
    assert "herdr-checkout" in schema["recorded_from"]


def test_the_pin_declares_the_pattern_semantics_as_unverified():
    pin = _pin()
    assert pin["pattern_combinator"] in ("all", "any")
    assert isinstance(pin["case_sensitive"], bool)
    assert pin["semantics_verified"] is False, (
        "herdr does not publish these; the pin must not claim they were read"
    )


def test_no_rule_field_is_left_unmapped_without_a_note():
    pin = _pin()
    if not pin["verified"]:
        pytest.skip("the pin is unverified")
    # Unmapped fields are allowed, but they must be recorded so a human sees them.
    assert isinstance(pin["unmapped_rule_fields"], list)
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_manifest_schema.py -q`
Expected: FAIL on `test_the_pin_is_committed` with `assert False` — the pin does not exist yet.

- [ ] **Step 4: Record the pin**

Run the recorder. If plan 02 has already vendored the manifests, this reads them:

```bash
chmod +x scripts/floor_manifest_schema.py
./scripts/floor_manifest_schema.py
```

If it exits 1 with `no manifests under …`, plan 02 has not run yet. Record an honest empty pin so
the rest of the plan can proceed with the manifest path failing closed:

```bash
cat > src/opendaisugi/floor/manifest_schema.json <<'JSON'
{
  "agent_key": null,
  "case_sensitive": true,
  "files": {},
  "pattern_combinator": "all",
  "recorded_at": "1970-01-01",
  "recorded_from": "harness/coppice/internal/detect/manifests",
  "regions": [],
  "rule_fields": {},
  "rules_key": null,
  "semantics_verified": false,
  "states": [],
  "top_keys": [],
  "unmapped_rule_fields": [],
  "verified": false
}
JSON
```

Then open an issue-shaped note in the commit message saying the pin is unverified, and re-run the
recorder as the first step of the next session that has `harness/coppice` in the tree.

- [ ] **Step 5: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_manifest_schema.py -q`
Expected: PASS (6 tests; two skip when the pin is unverified, and the skip names plan 02)

- [ ] **Step 6: Lint and commit**

```bash
uv run --no-sync ruff check .
git add scripts/floor_manifest_schema.py src/opendaisugi/floor/manifest_schema.json \
        tests/floor/test_manifest_schema.py
git commit -m "feat(floor): pin the herdr manifest schema read off the vendored files

Herdr does not publish the TOML format its screen manifests use, so guessing the
field names would put a guess on the state path. The recorder reads the files
plan 02 vendored and writes what it found; the evaluator loads that record and
refuses anything it does not name."
```

---

### Task 2: `manifests.py` — the schema-driven evaluator, and the shared fixtures

**Files:**
- Create: `src/opendaisugi/floor/manifests.py`
- Create: `tests/floor/conftest.py`
- Test: `tests/floor/test_manifests.py`

**Interfaces:**
- Consumes: `src/opendaisugi/floor/manifest_schema.json` (task 1); `tests/floor/hostfacts.py`.
- Produces:
  - `ManifestSchema` frozen dataclass: `verified: bool`, `rules_key: str | None`,
    `agent_key: str | None`, `fields: dict[str, str]`, `states: tuple[str, ...]`,
    `regions: tuple[str, ...]`, `top_keys: frozenset[str]`.
  - `load_schema(path: Path | None = None) -> ManifestSchema`
  - `Rule` frozen dataclass: `name: str`, `state: str`, `patterns: tuple[str, ...]`,
    `region: str`.
  - `Manifest` frozen dataclass: `agent: str`, `rules: tuple[Rule, ...]`, `source: Path`.
  - `ManifestSchemaMismatch(OpenDaisugiError)`
  - `load_manifests(dirs: Sequence[Path]) -> dict[str, Manifest]` — later dirs override by agent
    name.
  - `classify(screen_tail: str, *, title: str = "", osc_progress: str | None,
    agent_hint: str | None, manifests: dict[str, Manifest]) -> tuple[str, str | None]`
  - `default_manifest_dirs() -> tuple[Path, ...]` and `DEFAULT_MANIFEST_DIRS` — bundled
    (`$COPPICE_MANIFESTS`, else the repo's vendored dir) first, user override last.
  - Test helper in `conftest.py`: `write_manifest(dir, agent, rules) -> Path` and the
    `schema` / `manifest_dir` fixtures.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/conftest.py
"""Fixtures for the manifest evaluator.

The manifest field NAMES come from the recorded pin (task 1), never from this
file, so a test manifest is always written in whatever shape Herdr really uses.
The evaluator's ROLE mapping is what these tests check.
"""

from __future__ import annotations

from collections.abc import Sequence
from pathlib import Path

import pytest

from opendaisugi.floor.manifests import ManifestSchema, load_schema


@pytest.fixture
def schema() -> ManifestSchema:
    return load_schema()


def write_manifest(
    directory: Path,
    agent: str,
    rules: Sequence[tuple[str, str, Sequence[str], str]],
    *,
    schema: ManifestSchema,
) -> Path:
    """Write one TOML manifest in the recorded schema. Rules are (name, state, patterns, region)."""
    if not schema.verified:
        pytest.skip("the manifest schema pin is unverified — run plan 02, then the recorder")
    lines: list[str] = []
    if schema.agent_key:
        lines.append(f'{schema.agent_key} = "{agent}"')
    for name, state, patterns, region in rules:
        lines.append(f"[[{schema.rules_key}]]")
        lines.append(f'{schema.fields["name"]} = "{name}"')
        lines.append(f'{schema.fields["state"]} = "{state}"')
        pats = ", ".join(f'"{p}"' for p in patterns)
        field = schema.fields["patterns"]
        lines.append(f"{field} = [{pats}]" if field.endswith("s") else f"{field} = {pats}")
        if "region" in schema.fields:
            lines.append(f'{schema.fields["region"]} = "{region}"')
        lines.append("")
    path = directory / f"{agent}.toml"
    path.write_text("\n".join(lines), encoding="utf-8")
    return path


@pytest.fixture
def manifest_dir(tmp_path: Path) -> Path:
    d = tmp_path / "manifests"
    d.mkdir()
    return d
```

```python
# tests/floor/test_manifests.py
"""The Python manifest evaluator: same format, same fixtures, same answers as Go.

Manifests are the FALLBACK path (master §5.1). They never produce `done`, and a
screen nothing matches is `unknown`, never a confident `idle`.
"""

from __future__ import annotations

import json

import pytest

from opendaisugi.floor.manifests import (
    ManifestSchemaMismatch,
    classify,
    load_manifests,
    load_schema,
)
from tests.floor import hostfacts
from tests.floor.conftest import write_manifest


def test_nothing_loaded_classifies_unknown():
    assert classify("$ ", osc_progress=None, agent_hint=None, manifests={}) == ("unknown", None)


def test_a_matching_rule_returns_its_state_and_name(manifest_dir, schema):
    write_manifest(
        manifest_dir,
        "fake-agent",
        [("permission-prompt", "blocked", [r"Do you want to proceed\?"], "screen")],
        schema=schema,
    )
    manifests = load_manifests([manifest_dir])
    assert set(manifests) == {"fake-agent"}
    state, rule = classify(
        "Bash(rm -rf build/)\nDo you want to proceed?\n",
        osc_progress=None,
        agent_hint="fake-agent",
        manifests=manifests,
    )
    assert (state, rule) == ("blocked", "permission-prompt")


def test_no_rule_matches_is_unknown_not_idle(manifest_dir, schema):
    write_manifest(
        manifest_dir,
        "fake-agent",
        [("permission-prompt", "blocked", [r"Do you want to proceed\?"], "screen")],
        schema=schema,
    )
    manifests = load_manifests([manifest_dir])
    assert classify(
        "nothing here", osc_progress=None, agent_hint="fake-agent", manifests=manifests
    ) == ("unknown", None)


def test_a_manifest_may_never_say_done(manifest_dir, schema):
    """Master §3.1: `done` comes only from process or headless sources."""
    write_manifest(
        manifest_dir, "fake-agent", [("finished", "done", ["all set"], "screen")], schema=schema
    )
    manifests = load_manifests([manifest_dir])
    assert manifests["fake-agent"].rules == (), "a done rule must be dropped at load"
    assert classify("all set", osc_progress=None, agent_hint="fake-agent", manifests=manifests) == (
        "unknown",
        None,
    )


def test_the_agent_hint_picks_the_manifest(manifest_dir, schema):
    write_manifest(manifest_dir, "alpha", [("a", "working", ["busy"], "screen")], schema=schema)
    write_manifest(manifest_dir, "beta", [("b", "blocked", ["busy"], "screen")], schema=schema)
    manifests = load_manifests([manifest_dir])
    assert (
        classify("busy", osc_progress=None, agent_hint="beta", manifests=manifests)[0] == "blocked"
    )
    assert (
        classify("busy", osc_progress=None, agent_hint="alpha", manifests=manifests)[0] == "working"
    )


def test_no_hint_scans_every_manifest_and_refuses_an_ambiguous_answer(manifest_dir, schema):
    write_manifest(manifest_dir, "alpha", [("a", "working", ["busy"], "screen")], schema=schema)
    write_manifest(manifest_dir, "beta", [("b", "blocked", ["busy"], "screen")], schema=schema)
    manifests = load_manifests([manifest_dir])
    assert classify("busy", osc_progress=None, agent_hint=None, manifests=manifests) == (
        "unknown",
        None,
    )


def test_a_later_directory_overrides_by_agent_name(tmp_path, schema):
    a, b = tmp_path / "a", tmp_path / "b"
    a.mkdir(), b.mkdir()
    write_manifest(a, "fake-agent", [("bundled", "working", ["x"], "screen")], schema=schema)
    write_manifest(b, "fake-agent", [("override", "blocked", ["x"], "screen")], schema=schema)
    manifests = load_manifests([a, b])
    assert classify("x", osc_progress=None, agent_hint="fake-agent", manifests=manifests) == (
        "blocked",
        "override",
    )


def test_an_unknown_top_level_key_fails_closed(manifest_dir, schema):
    path = manifest_dir / "bad.toml"
    path.write_text('totally_unexpected = "value"\n', encoding="utf-8")
    manifests = load_manifests([manifest_dir])
    assert "bad" not in manifests, "a manifest we cannot parse must be dropped, not half-read"


def test_a_bad_regex_drops_the_rule_and_never_raises(manifest_dir, schema):
    write_manifest(manifest_dir, "fake-agent", [("bad", "blocked", ["("], "screen")], schema=schema)
    manifests = load_manifests([manifest_dir])
    assert manifests["fake-agent"].rules == ()


def test_the_schema_carries_the_pinned_pattern_semantics():
    s = load_schema()
    assert s.pattern_combinator in ("all", "any")
    assert isinstance(s.case_sensitive, bool)


def test_case_insensitive_matching_follows_the_pin(manifest_dir, schema, monkeypatch):
    from opendaisugi.floor import manifests as mod

    write_manifest(
        manifest_dir, "fake-agent", [("shout", "blocked", ["PROCEED"], "screen")], schema=schema
    )
    loaded = load_manifests([manifest_dir])
    insensitive = mod.ManifestSchema(
        verified=schema.verified,
        rules_key=schema.rules_key,
        agent_key=schema.agent_key,
        fields=schema.fields,
        states=schema.states,
        regions=schema.regions,
        top_keys=schema.top_keys,
        pattern_combinator="all",
        case_sensitive=False,
    )
    monkeypatch.setattr(mod, "load_schema", lambda path=None: insensitive)
    assert (
        classify("proceed", osc_progress=None, agent_hint="fake-agent", manifests=loaded)[0]
        == "blocked"
    )


def test_the_bundled_directory_comes_before_the_user_override(monkeypatch, tmp_path):
    from opendaisugi.floor.manifests import default_manifest_dirs

    monkeypatch.setenv("COPPICE_MANIFESTS", str(tmp_path))
    dirs = default_manifest_dirs()
    assert dirs[0] == tmp_path
    assert dirs[-1].parts[-2:] == ("coppice", "agent-detection")


def test_load_schema_reports_unverified_without_pretending(tmp_path):
    pin = tmp_path / "manifest_schema.json"
    pin.write_text(
        json.dumps(
            {
                "verified": False,
                "files": {},
                "rule_fields": {},
                "rules_key": None,
                "agent_key": None,
                "states": [],
                "regions": [],
                "top_keys": [],
                "pattern_combinator": "all",
                "case_sensitive": True,
                "semantics_verified": False,
            }
        ),
        encoding="utf-8",
    )
    s = load_schema(pin)
    assert s.verified is False and s.fields == {}


def test_load_manifests_with_an_unverified_schema_loads_nothing(tmp_path, monkeypatch):
    from opendaisugi.floor import manifests as mod

    monkeypatch.setattr(
        mod,
        "load_schema",
        lambda path=None: mod.ManifestSchema(
            verified=False,
            rules_key=None,
            agent_key=None,
            fields={},
            states=(),
            regions=(),
            top_keys=frozenset(),
            pattern_combinator="all",
            case_sensitive=True,
        ),
    )
    d = tmp_path / "m"
    d.mkdir()
    (d / "x.toml").write_text("anything = 1\n", encoding="utf-8")
    assert load_manifests([d]) == {}


def test_python_and_go_agree_on_every_shared_fixture():
    """The conformance pattern this repo already uses for the verifier.

    `expectations.json` is the whole contract: one entry per fixture carrying
    `agent`, `state` and `rule`. The Go test in plan 02 reads the same file.
    """
    reason = hostfacts.skip_reason("screen_fixtures") or hostfacts.skip_reason("vendored_manifests")
    if reason:
        pytest.skip(reason)
    expectations_path = hostfacts.SCREEN_FIXTURE_DIR / "expectations.json"
    if not expectations_path.exists():
        pytest.skip("harness/coppice/testdata/screens/expectations.json is not recorded yet")
    expectations = json.loads(expectations_path.read_text(encoding="utf-8"))
    manifests = load_manifests([hostfacts.MANIFEST_DIR])
    assert manifests, "the vendored manifests loaded to nothing"
    for rel, want in sorted(expectations.items()):
        # Everything comes from the golden. The fixture LAYOUT is plan 02's to choose,
        # so nothing here derives the agent or the state from the path.
        fixture = hostfacts.SCREEN_FIXTURE_DIR / rel
        assert fixture.exists(), f"{rel}: expectations.json names a fixture that is not there"
        for key in ("agent", "state", "rule"):
            assert key in want, f"{rel}: the golden must carry {key}"
        got = classify(
            fixture.read_text(encoding="utf-8"),
            osc_progress=want.get("osc"),
            title=want.get("title", ""),
            agent_hint=want["agent"],
            manifests=manifests,
        )
        assert got == (want["state"], want["rule"]), f"{rel}: python got {got}"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_manifests.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.floor.manifests'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/floor/manifests.py
"""Evaluate Herdr's TOML screen manifests in Python, against the recorded schema.

Master §5.5: coppice reuses Herdr's manifest format, so twenty-one agents are
detected on day one. Master §5.1: this is the FALLBACK path. A gate event outranks
anything decided here, a manifest may never say `done`, and a screen that matches
nothing is `unknown`, never a confident `idle`.

The field NAMES are not hardcoded. `manifest_schema.json` (written by
`scripts/floor_manifest_schema.py` from the vendored files) says which key holds
the rules and which rule field carries the name, the state, the patterns, and the
region. A manifest that uses a key the pin does not name is dropped whole. That is
the fail-closed choice: half a rule is worse than no rule.

Schema roles, as recorded in the pin:

* ``rules_key`` — the top-level array-of-tables holding the rules.
* ``agent_key`` — the top-level string naming the agent, when the format has one.
  When it does not, the file stem is the agent name.
* ``rule_fields["name"]`` — the rule's own name, reported back as the matched rule.
* ``rule_fields["state"]`` — one of the recorded ``states``.
* ``rule_fields["patterns"]`` — one regex or a list of them, matched case-sensitively.
* ``rule_fields["region"]`` — which text the patterns run against: the screen tail,
  the terminal title, or the last OSC progress string. Missing means ``screen``.

Two things the pin DECLARES rather than reads, because Herdr publishes neither:
``pattern_combinator`` (``all`` — every pattern in a rule must match) and
``case_sensitive`` (true). Plan 02's Go evaluator reads the same two values, so the
shared-fixture conformance test is decided by the pin instead of by two independent
guesses. Both carry ``semantics_verified: false`` until someone reads Herdr's matcher.

Where the manifests come from, in order: ``$COPPICE_MANIFESTS`` when set, else the
vendored directory in this repo (``harness/coppice/internal/detect/manifests``), then
the user override directory ``~/.config/coppice/agent-detection``. Later wins, so an
override replaces a bundled agent by name. When none of the three exists nothing loads
and every classification is ``unknown`` — the fallback is off, not guessing.
"""

from __future__ import annotations

import json
import re
import tomllib
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.exceptions import OpenDaisugiError

_PIN = Path(__file__).with_name("manifest_schema.json")

# Master §3.1: a manifest may never say `done`, and `unknown` is not dressed up.
_ALLOWED_STATES = frozenset({"idle", "working", "blocked", "unknown"})
_DEFAULT_REGION = "screen"

_REPO_MANIFESTS = (
    Path(__file__).resolve().parents[3]
    / "harness"
    / "coppice"
    / "internal"
    / "detect"
    / "manifests"
)
_OVERRIDE_DIR = Path.home() / ".config" / "coppice" / "agent-detection"


def default_manifest_dirs(env: "os.PathLike | None" = None) -> tuple[Path, ...]:
    """Bundled first, override last. Later dirs win by agent name."""
    import os as _os

    bundled = _os.environ.get("COPPICE_MANIFESTS")
    first = Path(bundled) if bundled else _REPO_MANIFESTS
    return (first, _OVERRIDE_DIR)


DEFAULT_MANIFEST_DIRS: tuple[Path, ...] = default_manifest_dirs()


class ManifestSchemaMismatch(OpenDaisugiError):
    """A manifest uses a key the recorded schema does not name."""


@dataclass(frozen=True)
class ManifestSchema:
    verified: bool
    rules_key: str | None
    agent_key: str | None
    fields: dict[str, str]
    states: tuple[str, ...]
    regions: tuple[str, ...]
    top_keys: frozenset[str]
    pattern_combinator: str = "all"
    case_sensitive: bool = True


@dataclass(frozen=True)
class Rule:
    name: str
    state: str
    patterns: tuple[str, ...]
    region: str


@dataclass(frozen=True)
class Manifest:
    agent: str
    rules: tuple[Rule, ...]
    source: Path


def load_schema(path: Path | None = None) -> ManifestSchema:
    """Read the recorded schema. An unreadable pin is an unverified one."""
    try:
        raw = json.loads((path or _PIN).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raw = {}
    return ManifestSchema(
        verified=bool(raw.get("verified")),
        rules_key=raw.get("rules_key"),
        agent_key=raw.get("agent_key"),
        fields=dict(raw.get("rule_fields") or {}),
        states=tuple(raw.get("states") or ()),
        regions=tuple(raw.get("regions") or ()),
        top_keys=frozenset(raw.get("top_keys") or ()),
        pattern_combinator=str(raw.get("pattern_combinator") or "all"),
        case_sensitive=bool(raw.get("case_sensitive", True)),
    )


def _rule_from(doc: dict, schema: ManifestSchema) -> Rule | None:
    """One rule, or None when it is unusable. Never raises."""
    name = str(doc.get(schema.fields.get("name", ""), "")).strip()
    state = str(doc.get(schema.fields.get("state", ""), "")).strip().lower()
    if not name or state not in _ALLOWED_STATES:
        return None  # `done` and every unknown state land here
    raw_patterns = doc.get(schema.fields.get("patterns", ""))
    patterns = (raw_patterns,) if isinstance(raw_patterns, str) else tuple(raw_patterns or ())
    if not patterns:
        return None
    for p in patterns:
        try:
            re.compile(str(p))
        except re.error:
            return None  # a rule we cannot compile is no rule at all
    region = str(doc.get(schema.fields.get("region", ""), "") or _DEFAULT_REGION).lower()
    return Rule(name=name, state=state, patterns=tuple(str(p) for p in patterns), region=region)


def _load_one(path: Path, schema: ManifestSchema) -> Manifest:
    doc = tomllib.loads(path.read_text(encoding="utf-8"))
    unknown = set(doc) - set(schema.top_keys)
    if unknown:
        raise ManifestSchemaMismatch(
            f"{path.name} uses keys the recorded schema does not name: {sorted(unknown)}. "
            f"Re-run ./scripts/floor_manifest_schema.py to record the current format."
        )
    agent = str(doc.get(schema.agent_key or "", "") or path.stem)
    raw_rules = doc.get(schema.rules_key or "", []) or []
    rules = tuple(r for r in (_rule_from(d, schema) for d in raw_rules) if r is not None)
    return Manifest(agent=agent, rules=rules, source=path)


def load_manifests(dirs: Sequence[Path]) -> dict[str, Manifest]:
    """Load every ``*.toml`` under ``dirs``. Later dirs override by agent name."""
    schema = load_schema()
    if not schema.verified or not schema.rules_key:
        return {}  # no recorded format means no classification, not a guessed one
    out: dict[str, Manifest] = {}
    for directory in dirs:
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.toml")):
            try:
                manifest = _load_one(path, schema)
            except (OSError, ValueError, ManifestSchemaMismatch):
                continue  # drop the file whole; never half-read a rule
            out[manifest.agent] = manifest
    return out


def _text_for(region: str, screen_tail: str, title: str, osc_progress: str | None) -> str:
    if region == "title":
        return title
    if region in ("osc", "progress", "osc_progress"):
        return osc_progress or ""
    return screen_tail


def _match(
    manifest: Manifest,
    screen_tail: str,
    title: str,
    osc_progress: str | None,
    schema: ManifestSchema,
) -> tuple[str, str] | None:
    combine = any if schema.pattern_combinator == "any" else all
    flags = 0 if schema.case_sensitive else re.IGNORECASE
    for rule in manifest.rules:
        text = _text_for(rule.region, screen_tail, title, osc_progress)
        if not text:
            continue
        if combine(re.search(p, text, flags) for p in rule.patterns):
            return rule.state, rule.name
    return None


def classify(
    screen_tail: str,
    *,
    title: str = "",
    osc_progress: str | None,
    agent_hint: str | None,
    manifests: dict[str, Manifest],
) -> tuple[str, str | None]:
    """Return ``(state, matched_rule_name)`` for one screen tail.

    With an ``agent_hint`` only that agent's manifest runs. Without one every
    manifest runs and two disagreeing answers collapse to ``unknown``: an
    ambiguous screen is not evidence, and master §3.1 forbids inventing `idle`.
    """
    schema = load_schema()
    if agent_hint and agent_hint in manifests:
        hit = _match(manifests[agent_hint], screen_tail, title, osc_progress, schema)
        return hit if hit else ("unknown", None)
    hits = {
        h
        for h in (_match(m, screen_tail, title, osc_progress, schema) for m in manifests.values())
        if h is not None
    }
    if len(hits) == 1:
        return next(iter(hits))
    return ("unknown", None)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_manifests.py -q`
Expected: PASS. On a box without plan 02 in the tree, the manifest-shape tests skip with
`the manifest schema pin is unverified — run plan 02, then the recorder`, and the three
schema-free tests (`test_nothing_loaded_classifies_unknown`,
`test_load_schema_reports_unverified_without_pretending`,
`test_load_manifests_with_an_unverified_schema_loads_nothing`) still pass.

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/floor/manifests.py tests/floor/conftest.py tests/floor/test_manifests.py
git commit -m "feat(floor): evaluate herdr screen manifests in python, fail-closed

Two implementations of one format over one fixture set, the conformance pattern
this repo already uses for the verifier. The evaluator reads the recorded schema
rather than hardcoding field names, drops any file it cannot fully parse, and
never returns done or a confident idle from a screen."
```

---

### Task 3: `registry.py` — pick a backend, prompt a pane, wait for a state

**Files:**
- Create: `src/opendaisugi/floor/registry.py`
- Modify: `src/opendaisugi/exceptions.py` (add `FloorNotAvailable` beside `MatcherNotAvailable`, ≈ line 29)
- Test: `tests/floor/test_registry.py`

**Interfaces:**
- Consumes: `opendaisugi.floor.PaneBackend`, `PaneRef`, `PaneInfo`, `PaneStateEvent` (plan 01);
  `opendaisugi.config.Config` with `config.floor.backend` (task 9 adds the field — until then the
  functions accept any object exposing `.floor.backend`, and the tests use a stub).
- Ordering note: `build_backend` imports the three backends lazily, inside the function, so
  task 3 lands and its tests pass before tasks 4, 5, and 6 exist. Every test here either
  monkeypatches `build_backend` or raises before it is called.
- Produces:
  - `FloorNotAvailable(OpenDaisugiError)` in `opendaisugi/exceptions.py`.
  - `BackendStatus` frozen dataclass: `name: str`, `available: bool`, `why_not: str`, `fix: str`.
  - `BACKEND_ORDER: tuple[str, ...] = ("coppice", "herdr", "tmux")`
  - `build_backend(name: str, config, *, autostart: bool = False) -> PaneBackend`
  - `backend_statuses(config) -> list[BackendStatus]` — never autostarts
  - `pick_backend(config, *, name: str | None = None, autostart: bool = False) -> PaneBackend`
  - `prompt_pane(backend, ref, text, *, wait: bool = False, timeout_s: float = 60.0) -> str`
  - `wait_for_state(backend, ref, *, until: str, timeout_s: float, poll_s: float = 0.5)
    -> PaneStateEvent | None`

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_registry.py
"""Picking a backend, and the two drivers every client shares.

`auto` takes the first available backend in the master's order. An explicit name
that is not available is a hard, teaching refusal (exit 3 at the CLI), never a
silent downgrade to a different substrate.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

import pytest

from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor import PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.registry import (
    BACKEND_ORDER,
    backend_statuses,
    pick_backend,
    prompt_pane,
    wait_for_state,
)


@dataclass
class _Floor:
    backend: str = "auto"
    notify_cmd: str | None = None
    tmux_socket: str | None = None


@dataclass
class _Cfg:
    floor: _Floor


class FakeBackend:
    name = "fake"

    def __init__(self, *, available=True, state="working"):
        self._available = available
        self._state = state
        self.sent: list[str] = []
        self.prompted: list[str] = []

    def available(self) -> bool:
        return self._available

    def send_text(self, pane, text, *, enter=True):
        self.sent.append(text)

    def list(self):
        ev = PaneStateEvent(
            session_id="s",
            harness="shell",
            state=self._state,
            source="process",
            ts=time.time(),
            pane="p1",
        )
        return [
            PaneInfo(
                ref=PaneRef("fake", "p1"), label="l", cwd="/", cmd=["sh"], kind="pty", state=ev
            )
        ]


class PromptingBackend(FakeBackend):
    def prompt(self, pane, text, *, wait=False, timeout_s=60.0):
        self.prompted.append(text)
        return "sent"


def test_order_is_the_masters_order():
    assert BACKEND_ORDER == ("coppice", "herdr", "tmux")


def test_auto_takes_the_first_available(monkeypatch):
    from opendaisugi.floor import registry

    built = {
        "coppice": FakeBackend(available=False),
        "herdr": FakeBackend(available=True),
        "tmux": FakeBackend(available=True),
    }
    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: built[name]
    )
    picked = pick_backend(_Cfg(_Floor(backend="auto")))
    assert picked is built["herdr"]


def test_auto_with_nothing_available_teaches_the_install(monkeypatch):
    from opendaisugi.floor import registry

    monkeypatch.setattr(
        registry,
        "build_backend",
        lambda name, config, *, autostart=False: FakeBackend(available=False),
    )
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="auto")))
    assert "coppice server start" in str(e.value)


def test_an_explicit_unavailable_backend_never_downgrades(monkeypatch):
    from opendaisugi.floor import registry

    built = {
        "coppice": FakeBackend(available=False),
        "herdr": FakeBackend(available=True),
        "tmux": FakeBackend(available=True),
    }
    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: built[name]
    )
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="coppice")))
    assert "coppice" in str(e.value) and "herdr" not in str(e.value)


def test_an_unknown_backend_name_lists_the_real_ones():
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="screen")))
    assert "coppice, herdr, tmux" in str(e.value)


def test_statuses_report_every_backend_with_a_reason(monkeypatch):
    from opendaisugi.floor import registry

    monkeypatch.setattr(
        registry,
        "build_backend",
        lambda name, config, *, autostart=False: FakeBackend(available=False),
    )
    rows = backend_statuses(_Cfg(_Floor()))
    assert [r.name for r in rows] == list(BACKEND_ORDER)
    for r in rows:
        assert r.available is False and r.why_not and r.fix


def test_a_backend_that_raises_in_available_is_reported_unavailable(monkeypatch):
    from opendaisugi.floor import registry

    class Exploding(FakeBackend):
        def available(self):
            raise RuntimeError("boom")

    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: Exploding()
    )
    rows = backend_statuses(_Cfg(_Floor()))
    assert all(r.available is False for r in rows)
    assert "boom" in rows[0].why_not


def test_auto_never_asks_a_backend_to_autostart(monkeypatch):
    """Opening the floor must not spawn a daemon the operator did not name."""
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=(name == "tmux"))

    monkeypatch.setattr(registry, "build_backend", build)
    pick_backend(_Cfg(_Floor(backend="auto")))
    assert seen and not any(seen)


def test_an_explicit_backend_can_ask_for_autostart(monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=True)

    monkeypatch.setattr(registry, "build_backend", build)
    pick_backend(_Cfg(_Floor()), name="coppice", autostart=True)
    assert seen == [True]


def test_backend_statuses_never_autostart(monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=False)

    monkeypatch.setattr(registry, "build_backend", build)
    backend_statuses(_Cfg(_Floor()))
    assert seen == [False, False, False]


def test_prompt_pane_uses_the_backends_own_prompt_when_it_has_one():
    b = PromptingBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello") == "sent"
    assert b.prompted == ["hello"] and b.sent == []


def test_prompt_pane_falls_back_to_send_text():
    b = FakeBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello") == "typed"
    assert b.sent == ["hello"]


def test_wait_for_state_returns_the_event_when_it_arrives():
    b = FakeBackend(state="blocked")
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="blocked", timeout_s=1.0, poll_s=0.01)
    assert ev is not None and ev.state == "blocked"


def test_wait_for_state_returns_none_on_timeout_never_a_fake_event():
    b = FakeBackend(state="working")
    started = time.time()
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="blocked", timeout_s=0.2, poll_s=0.01)
    assert ev is None and time.time() - started < 1.0


def test_wait_for_a_vanished_pane_resolves_done():
    class Empty(FakeBackend):
        def list(self):
            return []

    ev = wait_for_state(Empty(), PaneRef("fake", "p1"), until="done", timeout_s=1.0, poll_s=0.01)
    assert ev is not None and ev.state == "done" and ev.source == "process"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_registry.py -q`
Expected: FAIL with `ImportError: cannot import name 'FloorNotAvailable'`

- [ ] **Step 3: Write the implementation**

Add the exception to `src/opendaisugi/exceptions.py`, directly after `MatcherNotAvailable`
(which ends ≈ line 42; `DecompositionError` starts at line 44):

```python
class FloorNotAvailable(OpenDaisugiError):
    """No pane backend is reachable, or the named one is not.

    The floor is optional. A missing backend is a fact to teach, never a reason
    to run somewhere the operator did not choose.
    """
```

```python
# src/opendaisugi/floor/registry.py
"""Pick the pane backend, and the two drivers every client shares.

Master §3.2 gives one protocol and three implementations. `auto` takes the first
available in the master's order. An explicit name that is not available raises
`FloorNotAvailable` with the command that installs it: quietly running somewhere
the operator did not choose would put their agent in the wrong workshop.

`prompt_pane` and `wait_for_state` live here rather than in each backend because
both the CLI and the floor screen need identical behaviour, and the difference
between backends is one optional method.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor import PaneBackend, PaneRef, PaneStateEvent

BACKEND_ORDER: tuple[str, ...] = ("coppice", "herdr", "tmux")

_FIX = {
    "coppice": "build it with `cd harness/coppice && go build ./cmd/coppice`, "
    "then run `coppice server start`",
    "herdr": "install herdr from herdr.dev, then run `herdr session list --json`",
    "tmux": "install tmux 3.2 or newer with your package manager",
}


@dataclass(frozen=True)
class BackendStatus:
    name: str
    available: bool
    why_not: str
    fix: str


def build_backend(name: str, config, *, autostart: bool = False) -> PaneBackend:
    """Construct one backend. Imports lazily so a missing host costs nothing.

    ``autostart`` reaches the coppice backend only. It is False for every probe, so
    `daisugi coppice backends` stays a diagnostic and never starts a daemon as a side
    effect of being asked a question.
    """
    if name == "coppice":
        from opendaisugi.floor.coppice_backend import CoppiceBackend

        return CoppiceBackend(autostart=autostart)
    if name == "herdr":
        from opendaisugi.floor.herdr_backend import HerdrBackend

        return HerdrBackend()
    if name == "tmux":
        from opendaisugi.floor.tmux_backend import TmuxBackend

        return TmuxBackend(socket=getattr(config.floor, "tmux_socket", None))
    raise FloorNotAvailable(
        f"no pane backend named {name!r}. Choose one of: {', '.join(BACKEND_ORDER)}."
    )


def _probe(
    name: str, config, *, autostart: bool = False
) -> tuple[PaneBackend | None, BackendStatus]:
    try:
        backend = build_backend(name, config, autostart=autostart)
        ok = bool(backend.available())
    except Exception as exc:  # noqa: BLE001 — available() must never take the floor down
        return None, BackendStatus(name, False, str(exc) or exc.__class__.__name__, _FIX[name])
    if ok:
        return backend, BackendStatus(name, True, "", _FIX[name])
    return None, BackendStatus(name, False, f"{name} did not answer", _FIX[name])


def backend_statuses(config) -> list[BackendStatus]:
    """One row per backend: name, available, why not, and the command that fixes it.

    Never autostarts. A diagnostic that changes the machine is not a diagnostic.
    """
    return [_probe(name, config, autostart=False)[1] for name in BACKEND_ORDER]


def pick_backend(config, *, name: str | None = None, autostart: bool = False) -> PaneBackend:
    """The backend to drive. ``auto`` scans in order; a name is taken literally.

    ``autostart=True`` lets the coppice backend run `coppice server start` once, and
    only once, when its socket is absent (spec-03). Callers that ASK for coppice pass
    it: `daisugi coppice <verb> --backend coppice` and any `coppice spawn`. Callers
    that merely look, do not.
    """
    chosen = name or getattr(config.floor, "backend", "auto")
    if chosen != "auto":
        if chosen not in BACKEND_ORDER:
            raise FloorNotAvailable(
                f"no pane backend named {chosen!r}. Choose one of: {', '.join(BACKEND_ORDER)}."
            )
        backend, status = _probe(chosen, config, autostart=autostart)
        if backend is None:
            raise FloorNotAvailable(f"{chosen} is not available: {status.why_not}. {status.fix}.")
        return backend
    reasons: list[str] = []
    for candidate in BACKEND_ORDER:
        # `auto` never starts a server. The operator did not name coppice, so the
        # floor picks what is already running rather than launching a daemon.
        backend, status = _probe(candidate, config, autostart=False)
        if backend is not None:
            return backend
        reasons.append(f"{candidate}: {status.why_not}")
    raise FloorNotAvailable(
        "no pane backend is available. "
        + "; ".join(reasons)
        + ". Start one: `coppice server start`, or install tmux."
    )


def prompt_pane(
    backend: PaneBackend,
    ref: PaneRef,
    text: str,
    *,
    wait: bool = False,
    timeout_s: float = 60.0,
) -> str:
    """Deliver a prompt. Returns ``"sent"`` (a real agent prompt) or ``"typed"``.

    A headless pane takes `agent.prompt`; a pty pane takes typed text. The caller
    shows which happened, because typing into a busy UI and prompting an agent are
    not the same act.
    """
    prompt = getattr(backend, "prompt", None)
    if callable(prompt):
        return str(prompt(ref, text, wait=wait, timeout_s=timeout_s))
    backend.send_text(ref, text, enter=True)
    return "typed"


def wait_for_state(
    backend: PaneBackend,
    ref: PaneRef,
    *,
    until: str,
    timeout_s: float,
    poll_s: float = 0.5,
) -> PaneStateEvent | None:
    """Block until the pane reaches ``until``. None on timeout, never a made-up event.

    A pane that has left ``list()`` is done: the process is gone. That event is
    tagged ``source: "process"`` because master §3.1 forbids a manifest saying done.
    """
    deadline = time.monotonic() + timeout_s
    while True:
        infos = {i.ref.id: i for i in backend.list()}
        info = infos.get(ref.id)
        if info is None:
            if until in ("done", "any"):
                return PaneStateEvent(
                    session_id="",
                    harness="unknown",
                    state="done",
                    source="process",
                    ts=time.time(),
                    pane=ref.id,
                    detail="pane left the backend's list",
                )
        elif info.state is not None and (until == "any" or info.state.state == until):
            return info.state
        if time.monotonic() >= deadline:
            return None
        time.sleep(poll_s)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_registry.py -q`
Expected: PASS (12 tests)

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/exceptions.py src/opendaisugi/floor/registry.py \
        tests/floor/test_registry.py
git commit -m "feat(floor): pick a pane backend, or refuse and teach the install

auto scans coppice, herdr, tmux in the master's order. An explicit backend that
is not available is a refusal, not a silent downgrade: putting the operator's
agent in a substrate they did not choose is the wrong kind of helpful."
```

---

### Task 4: `tmux_backend.py` — the substrate everyone already has

Facts probed against tmux 3.7b on this box, on a private socket. Every one is a step below:

- `new-window` fails when the server has no session. `new-session -d -s NAME -c DIR -P -F
  '#{pane_id}' -n LABEL -x COLS -y ROWS -e K=V -- CMD` works from cold and prints `%0`.
- `-e K=V` reaches the process environment (verified: the pane echoed `P=w1p1 S=/x/y.sock`).
  The 3.2 floor is the one `new-session -e` needs. `new-window -e` shipped earlier, so the same
  gate is merely conservative there. One gate for both keeps the branch single and testable.
- `#{pane_current_command}` reads `sleep` once the command has exec'd, and `tmux` for a moment
  before that. It is the agent hint, so it is read at classify time, not at spawn time.
- `#{pane_start_command}` comes back **with surrounding double quotes**
  (`"sh -c 'echo READY; sleep 30'"`), so it is unquoted before `shlex.split`.
- `kill-pane` on a pane that already exited exits 1 with `can't find pane: %4`. `close()` treats
  that as success.
- `display-message -p -t <dead pane>` exits **0 with empty output**. It is useless for existence;
  `list-panes -a -F` membership is the check.
- A pane vanishes from `list-panes -a` when its command exits. That is `done`, tagged
  `source: "process"`.
- `tmux -V` on this box prints `3.7b`. A version parser that does `int(minor)` raises.

**Files:**
- Create: `src/opendaisugi/floor/tmux_backend.py`
- Test: `tests/floor/test_tmux_backend.py`

**Interfaces:**
- Consumes: `opendaisugi.floor` value types; `opendaisugi.floor.manifests.classify` and
  `load_manifests`; `tests/floor/hostfacts.py` for the skip.
- Produces:
  - `parse_tmux_version(text: str) -> tuple[int, int]` — `"tmux 3.7b"` → `(3, 7)`,
    `"tmux next-3.8"` → `(3, 8)`, unparsable → `(0, 0)`.
  - `supports_env_flag(version: tuple[int, int]) -> bool` — True from 3.2.
  - `KEY_MAP: dict[str, str]` — our key names → tmux key names.
  - `_unquote_start_command(text: str) -> str` — tmux returns `pane_start_command` quoted.
  - `_READ_SOURCES: tuple[str, ...]` — `read()` raises `ValueError` on anything else.
  - `TmuxBackend(socket: str | None = None, *, manifest_dirs: Sequence[Path] | None = None,
    gate_states: Callable[[], dict[str, PaneStateEvent]] | None = None)` implementing
    `PaneBackend`, with `name = "tmux"`, `yields_frames = False`, and
    `spawn(..., harness: str | None = None)`.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_tmux_backend.py
"""tmux as a pane backend, on a private server (`-L daisugi-test`), torn down after.

The pure functions run everywhere. The live tests skip with a named reason when
tmux is absent. `done` on tmux is inferred from a pane leaving `list-panes`, so it
is tagged `source: "process"` — master §3.1 forbids a manifest saying done.
"""

from __future__ import annotations

import shutil
import subprocess
import time

import pytest

from opendaisugi.floor.tmux_backend import (
    KEY_MAP,
    TmuxBackend,
    parse_tmux_version,
    supports_env_flag,
)
from tests.floor import hostfacts

SOCKET = "daisugi-test"


@pytest.fixture
def tmux_backend():
    reason = hostfacts.skip_reason("tmux")
    if reason:
        pytest.skip(reason)
    backend = TmuxBackend(socket=SOCKET)
    yield backend
    subprocess.run(
        [shutil.which("tmux"), "-L", SOCKET, "kill-server"], capture_output=True, check=False
    )


@pytest.mark.parametrize(
    "text,want",
    [
        ("tmux 3.2", (3, 2)),
        ("tmux 3.1c", (3, 1)),
        ("tmux 3.7b", (3, 7)),
        ("tmux next-3.8", (3, 8)),
        ("tmux 2.9a", (2, 9)),
        ("tmux master", (0, 0)),
        ("", (0, 0)),
    ],
)
def test_version_parser_survives_every_real_tmux_spelling(text, want):
    assert parse_tmux_version(text) == want


def test_env_flag_needs_three_two():
    assert supports_env_flag((3, 2)) is True
    assert supports_env_flag((3, 7)) is True
    assert supports_env_flag((3, 1)) is False
    assert supports_env_flag((0, 0)) is False


def test_key_map_covers_the_names_the_floor_binds():
    for name in ("enter", "ctrl+c", "esc", "tab", "f1"):
        assert name in KEY_MAP
    assert KEY_MAP["enter"] == "Enter"
    assert KEY_MAP["ctrl+c"] == "C-c"
    assert KEY_MAP["esc"] == "Escape"


def test_available_is_false_without_tmux(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert TmuxBackend(socket=SOCKET).available() is False


def test_spawn_from_a_cold_server_then_read_and_close(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo READY; sleep 5"],
        env={"COPPICE_PANE": "t1"},
        label="probe",
        kind="pty",
    )
    assert ref.backend == "tmux" and ref.id.startswith("%")
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "READY" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "READY" in tmux_backend.read(ref, source="visible")
    assert ref.id in {i.ref.id for i in tmux_backend.list()}
    tmux_backend.close(ref)
    assert ref.id not in {i.ref.id for i in tmux_backend.list()}


def test_the_label_is_the_window_name(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="auth fix", kind="pty"
    )
    info = next(i for i in tmux_backend.list() if i.ref.id == ref.id)
    assert info.label == "auth fix"
    assert info.cwd == str(tmp_path)


def test_env_reaches_the_pane(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo P=$COPPICE_PANE; sleep 5"],
        env={"COPPICE_PANE": "w1p9"},
        label="envtest",
        kind="pty",
    )
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "P=w1p9" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "P=w1p9" in tmux_backend.read(ref, source="visible")


def test_send_text_types_and_presses_enter(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["bash", "--norc", "-i"], env={}, label="keys", kind="pty"
    )
    time.sleep(0.8)
    tmux_backend.send_text(ref, "echo hello-literal")
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "hello-literal" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "hello-literal" in tmux_backend.read(ref, source="visible")


def test_detection_source_is_the_last_twelve_unwrapped_lines(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "for i in $(seq 1 40); do echo line$i; done; sleep 5"],
        env={},
        label="detect",
        kind="pty",
    )
    time.sleep(1.0)
    tail = tmux_backend.read(ref, source="detection")
    lines = tail.splitlines()
    assert len(lines) <= 12
    assert "line40" in tail and "line1\n" not in tail


def test_close_is_idempotent_on_a_pane_that_already_exited(tmux_backend, tmp_path):
    """tmux exits 1 with 'can't find pane' — that is success, not a failure."""
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "true"], env={}, label="short", kind="pty"
    )
    time.sleep(1.0)
    tmux_backend.close(ref)  # must not raise
    tmux_backend.close(ref)


def test_a_finished_pane_reports_done_from_the_process_not_a_manifest(tmux_backend, tmp_path):
    """`done` is a process fact. It must arrive with no manifest installed at all.

    The pane sleeps one second so the first `subscribe()` poll certainly SEES it
    before it goes; `subscribe` tracks pane ids, not states, so an unclassified pane
    is still announced when it exits.
    """
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="short", kind="pty"
    )
    assert ref.id in {i.ref.id for i in tmux_backend.list()}, "the pane exited too fast"
    deadline = time.time() + 8.0
    seen = None
    for event in tmux_backend.subscribe():
        if event.pane == ref.id and event.state == "done":
            seen = event
            break
        if time.time() > deadline:
            break
    assert seen is not None, "a pane that exited never reported done"
    assert seen.source == "process"
    assert seen.detail and "manifest" not in seen.detail


def test_done_arrives_even_when_no_manifests_are_installed(
    tmux_backend, tmp_path, tmp_path_factory
):
    """The regression this blocker was: state-keyed tracking dropped unclassified panes."""
    empty = tmp_path_factory.mktemp("no-manifests")
    backend = TmuxBackend(socket=SOCKET, manifest_dirs=[empty])
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="nomanifest", kind="pty"
    )
    info = next(i for i in backend.list() if i.ref.id == ref.id)
    assert info.state is None, "no manifests means no state, and that must not hide the exit"
    deadline = time.time() + 8.0
    for event in backend.subscribe():
        if event.pane == ref.id and event.state == "done":
            return
        if time.time() > deadline:
            break
    raise AssertionError("done never arrived for an unclassified pane")


def test_the_start_command_is_unquoted_before_it_is_split(tmux_backend, tmp_path):
    """tmux 3.7b returns pane_start_command WITH its surrounding quotes."""
    from opendaisugi.floor.tmux_backend import _unquote_start_command

    assert _unquote_start_command("\"sh -c 'echo hi'\"") == "sh -c 'echo hi'"
    assert _unquote_start_command("plain") == "plain"
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="quoted", kind="pty"
    )
    info = next(i for i in tmux_backend.list() if i.ref.id == ref.id)
    assert info.cmd[0] == "sh", f"the start command was not unquoted: {info.cmd!r}"


def test_read_rejects_an_unknown_source_instead_of_showing_visible(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="badsrc", kind="pty"
    )
    with pytest.raises(ValueError) as e:
        tmux_backend.read(ref, source="scrollback")
    assert "visible" in str(e.value) and "detection" in str(e.value)


def test_available_does_not_depend_on_a_parsable_version(monkeypatch):
    """A working tmux whose -V we cannot parse must still run the contract suite."""
    backend = TmuxBackend(socket=SOCKET)
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/tmux")
    monkeypatch.setattr(backend, "_version_tuple", lambda: (0, 0))
    assert backend.available() is True


def test_spawn_accepts_the_protocols_harness_keyword_and_ignores_it(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "sleep 5"],
        env={},
        label="h",
        kind="pty",
        harness="claude-code",
    )
    assert ref.id.startswith("%")


def test_resize_is_a_documented_no_op(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="sz", kind="pty"
    )
    assert tmux_backend.resize(ref, 200, 60) is None
    assert "tmux owns the layout" in TmuxBackend.resize.__doc__
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_tmux_backend.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.floor.tmux_backend'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/floor/tmux_backend.py
"""tmux as a pane backend: the substrate the operator already has.

tmux has no agent state, so this backend earns its state twice over. A gate event
for a pane inside the last two seconds wins outright (master §5.1). Otherwise the
manifests classify the pane's detection tail, with `#{pane_current_command}` as the
agent hint. A pane that has left `list-panes` is `done` from the *process*, because
master §3.1 forbids a manifest ever saying done.

tmux owns the layout, so `resize` is a no-op. Splits, zoom, and window arrangement
belong to the operator's own tmux config, not to us.

Probed against tmux 3.7b:

* `new-window` needs a session; from a cold server we `new-session -d` instead.
* `-e K=V` needs tmux 3.2. Older tmux gets `env K=V -- cmd` wrapped around the argv.
* `kill-pane` on a gone pane exits 1 with "can't find pane" — that is success.
* `display-message -p -t <gone>` exits 0 with empty output, so existence is decided
  by membership in `list-panes -a`, never by that command.
"""

from __future__ import annotations

import re
import shlex
import shutil
import subprocess
import time
from collections.abc import Callable, Iterator, Sequence
from pathlib import Path
from typing import Literal

from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.manifests import DEFAULT_MANIFEST_DIRS, classify, load_manifests

_SESSION = "coppice"
_DEFAULT_COLS = 120
_DEFAULT_ROWS = 40
_DETECTION_LINES = 12
_RECENT_LINES = 200
_GATE_WINDOW_S = 2.0
_POLL_S = 0.5
_TIMEOUT_S = 5.0
_READ_SOURCES = ("visible", "recent", "detection")

# Our key names (master §3.2) → tmux key names.
KEY_MAP: dict[str, str] = {
    "enter": "Enter",
    "esc": "Escape",
    "escape": "Escape",
    "tab": "Tab",
    "backspace": "BSpace",
    "space": "Space",
    "up": "Up",
    "down": "Down",
    "left": "Left",
    "right": "Right",
    "ctrl+a": "C-a",
    "ctrl+b": "C-b",
    "ctrl+c": "C-c",
    "ctrl+d": "C-d",
    "ctrl+r": "C-r",
    "ctrl+z": "C-z",
    "f1": "F1",
    "f2": "F2",
    "f3": "F3",
    "f4": "F4",
}

_VERSION_RE = re.compile(r"(\d+)\.(\d+)")
_LIST_FORMAT = (
    "#{pane_id}\t#{window_name}\t#{pane_current_path}\t"
    "#{pane_current_command}\t#{pane_start_command}\t#{pane_dead}"
)


def _unquote_start_command(text: str) -> str:
    """tmux 3.7b returns `pane_start_command` WITH surrounding double quotes.

    Probed: a pane started as `sh -c 'echo READY'` comes back as
    `"sh -c 'echo READY'"`, so a bare `shlex.split` yields one element holding the
    whole string and the roster shows that blob as the harness.
    """
    stripped = text.strip()
    if len(stripped) >= 2 and stripped[0] == stripped[-1] and stripped[0] in "\"'":
        return stripped[1:-1]
    return stripped


def parse_tmux_version(text: str) -> tuple[int, int]:
    """``"tmux 3.7b"`` → ``(3, 7)``. Unparsable spellings are ``(0, 0)``, not a crash."""
    match = _VERSION_RE.search(text or "")
    if not match:
        return (0, 0)
    return (int(match.group(1)), int(match.group(2)))


def supports_env_flag(version: tuple[int, int]) -> bool:
    """``new-session -e K=V`` arrived in tmux 3.2."""
    return version >= (3, 2)


class TmuxBackend:
    """Drive panes through the tmux CLI. Stdlib subprocess only."""

    name = "tmux"

    def __init__(
        self,
        socket: str | None = None,
        *,
        manifest_dirs: Sequence[Path] | None = None,
        gate_states: Callable[[], dict[str, PaneStateEvent]] | None = None,
    ) -> None:
        self._socket = socket
        self._manifest_dirs = tuple(manifest_dirs or DEFAULT_MANIFEST_DIRS)
        self._gate_states = gate_states or (lambda: {})
        self._manifests: dict | None = None
        self._version: tuple[int, int] | None = None

    # --- process plumbing -------------------------------------------------
    def _argv(self, *args: str) -> list[str]:
        exe = shutil.which("tmux")
        if exe is None:
            raise FileNotFoundError("tmux is not on PATH")
        base = [exe]
        if self._socket:
            base += ["-L", self._socket]
        return base + list(args)

    def _run(self, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        proc = subprocess.run(
            self._argv(*args), capture_output=True, text=True, timeout=_TIMEOUT_S, check=False
        )
        if check and proc.returncode != 0:
            raise RuntimeError(f"tmux {' '.join(args[:2])}: {proc.stderr.strip()}")
        return proc

    def _version_tuple(self) -> tuple[int, int]:
        if self._version is None:
            try:
                self._version = parse_tmux_version(self._run("-V", check=False).stdout)
            except (OSError, FileNotFoundError, subprocess.SubprocessError):
                self._version = (0, 0)
        return self._version

    # --- PaneBackend ------------------------------------------------------
    def available(self) -> bool:
        """tmux on PATH. Never raises (master §3.2).

        Deliberately NOT gated on the version: `tmux master` and other unparsable
        spellings parse to (0, 0), and refusing a working tmux over a version string
        would skip the contract suite on a perfectly good box. The version tuple
        decides `-e` versus an `env` wrapper, nothing else.
        """
        try:
            return shutil.which("tmux") is not None
        except Exception:  # noqa: BLE001
            return False

    def _has_session(self) -> bool:
        return self._run("has-session", "-t", _SESSION, check=False).returncode == 0

    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"] = "pty",
        harness: str | None = None,
    ) -> PaneRef:
        """A new window running ``cmd``. tmux has no headless kind; every pane is a pty.

        ``harness`` is part of the §3.2 protocol so one call site drives all three
        backends. tmux has no adapter registry, so it is recorded nowhere and ignored.
        """
        argv = list(cmd)
        env_args: list[str] = []
        if env:
            if supports_env_flag(self._version_tuple()):
                for key, value in env.items():
                    env_args += ["-e", f"{key}={value}"]
            else:
                argv = ["env", *[f"{k}={v}" for k, v in env.items()], *argv]
        command = shlex.join(argv)
        common = ["-d", "-c", str(cwd), "-P", "-F", "#{pane_id}", "-n", label or "pane", *env_args]
        if self._has_session():
            out = self._run("new-window", *common, command).stdout
        else:
            out = self._run(
                "new-session",
                "-s",
                _SESSION,
                "-x",
                str(_DEFAULT_COLS),
                "-y",
                str(_DEFAULT_ROWS),
                *common,
                command,
            ).stdout
        pane_id = out.strip().splitlines()[-1].strip()
        return PaneRef(backend=self.name, id=pane_id)

    def list(self) -> list[PaneInfo]:
        proc = self._run("list-panes", "-a", "-F", _LIST_FORMAT, check=False)
        if proc.returncode != 0:
            return []  # no server means no panes, not an error
        gate = self._gate_states()
        now = time.time()
        out: list[PaneInfo] = []
        for line in proc.stdout.splitlines():
            parts = line.split("\t")
            if len(parts) < 6:
                continue
            pane_id, window, cwd, current, start, dead = parts[:6]
            ref = PaneRef(backend=self.name, id=pane_id)
            start = _unquote_start_command(start)
            try:
                argv = shlex.split(start) if start else [current]
            except ValueError:
                argv = [start or current]  # an unbalanced quote is a name, not a crash
            is_dead = dead.strip() == "1"
            out.append(
                PaneInfo(
                    ref=ref,
                    label=window,
                    cwd=cwd,
                    cmd=argv,
                    # `kind` carries "dead" so `subscribe` can emit `done` from the
                    # process even while tmux keeps the pane on screen
                    # (`remain-on-exit on`). Every live tmux pane is a pty.
                    kind="dead" if is_dead else "pty",
                    state=None if is_dead else self._state_for(ref, current, gate, now),
                )
            )
        return out

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._run("send-keys", "-t", pane.id, "-l", text)
        if enter:
            self._run("send-keys", "-t", pane.id, "Enter")

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        for key in keys:
            mapped = KEY_MAP.get(key.lower())
            if mapped is not None:
                self._run("send-keys", "-t", pane.id, mapped)
            elif len(key) == 1:
                self._run("send-keys", "-t", pane.id, "-l", key)
            else:
                raise ValueError(
                    f"unknown key {key!r}. Known keys: {', '.join(sorted(KEY_MAP))}, "
                    f"or one printable character."
                )

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        if source not in _READ_SOURCES:
            raise ValueError(
                f"tmux has no read source {source!r}. Known sources: {', '.join(_READ_SOURCES)}."
            )
        if source == "recent":
            args = ["capture-pane", "-p", "-S", f"-{_RECENT_LINES}", "-t", pane.id]
        elif source == "detection":
            args = ["capture-pane", "-p", "-J", "-t", pane.id]
        else:
            args = ["capture-pane", "-p", "-t", pane.id]
        proc = self._run(*args, check=False)
        if proc.returncode != 0:
            return ""
        text = proc.stdout
        if source == "detection":
            lines = [ln for ln in text.splitlines()]
            while lines and not lines[-1].strip():
                lines.pop()
            return "\n".join(lines[-_DETECTION_LINES:])
        return text

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        """A no-op: tmux owns the layout, and fighting it would move the operator's panes."""
        return None

    def close(self, pane: PaneRef) -> None:
        """Kill the pane. A pane that already exited is already closed, not an error."""
        proc = self._run("kill-pane", "-t", pane.id, check=False)
        if proc.returncode != 0 and "can't find pane" not in proc.stderr:
            raise RuntimeError(f"tmux kill-pane: {proc.stderr.strip()}")

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        """tmux holds no state of its own. The gate's own event store is the sink."""
        return None

    def subscribe(self) -> Iterator[PaneStateEvent | Frame]:
        """Poll every 500 ms. Yield `done` from the process, and state changes from state.

        `seen` tracks pane IDS, not states. Tracking states would mean a pane that never
        matched a manifest is never in the map, so its exit is never announced — which is
        every pane on a box with no manifests installed. `done` on tmux is a PROCESS fact
        (the pane left `list-panes`, or tmux flagged it `pane_dead`), so it is emitted
        independently of whether any manifest ever classified the pane.

        tmux yields no frames (master §3.2): only the coppice backend does.
        """
        seen: set[str] = set()
        last: dict[str, str] = {}
        while True:
            infos = self.list()
            alive = {i.ref.id for i in infos}
            dead_now = {i.ref.id for i in infos if i.kind == "dead"}
            for pane_id in sorted((seen - alive) | (dead_now & seen)):
                seen.discard(pane_id)
                last.pop(pane_id, None)
                yield PaneStateEvent(
                    session_id="",
                    harness="unknown",
                    state="done",
                    source="process",
                    ts=time.time(),
                    pane=pane_id,
                    detail="the pane's process exited",
                )
            seen |= alive - dead_now
            for info in infos:
                if info.state is None:
                    continue
                if last.get(info.ref.id) != info.state.state:
                    last[info.ref.id] = info.state.state
                    yield info.state
            time.sleep(_POLL_S)

    # --- state ------------------------------------------------------------
    def _state_for(
        self,
        ref: PaneRef,
        current_command: str,
        gate: dict[str, PaneStateEvent],
        now: float,
    ) -> PaneStateEvent | None:
        recent = gate.get(ref.id)
        if recent is not None and now - recent.ts <= _GATE_WINDOW_S:
            return recent  # master §5.1: a gate fact outranks a screen guess
        if self._manifests is None:
            self._manifests = load_manifests(self._manifest_dirs)
        if not self._manifests:
            return None
        state, rule = classify(
            self.read(ref, source="detection"),
            osc_progress=None,
            agent_hint=current_command or None,
            manifests=self._manifests,
        )
        if state == "unknown":
            return None
        return PaneStateEvent(
            session_id="",
            harness=current_command or "unknown",
            state=state,
            source="manifest",
            ts=now,
            pane=ref.id,
            detail=f"rule={rule}",
        )
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_tmux_backend.py -q`
Expected: PASS. With tmux absent the live tests skip with `tmux is not on PATH — install tmux 3.2
or newer with your package manager`; the four pure-function tests still pass.

- [ ] **Step 5: Confirm the private server is gone**

Run: `tmux -L daisugi-test list-sessions; echo rc=$?`
Expected: `error connecting to …` and `rc=1` — the fixture tore the server down.

- [ ] **Step 6: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/floor/tmux_backend.py tests/floor/test_tmux_backend.py
git commit -m "feat(floor): drive tmux panes so the floor works on day one

Most operators already run tmux. This makes it a first-class pane backend with
honest state: a gate fact wins, manifests classify the detection tail otherwise,
and a pane that left list-panes is done from the process, never from a manifest."
```

---

### Task 5: `coppice_backend.py` — the native socket client

**Files:**
- Create: `src/opendaisugi/floor/coppice_backend.py`
- Test: `tests/floor/test_coppice_backend.py`

**Interfaces:**
- Consumes: `opendaisugi.floor` value types; master §3.3 socket API.
- Produces:
  - `CoppiceError(OpenDaisugiError)` carrying `code: str`.
  - `default_socket_path(env: Mapping[str, str] | None = None) -> Path` —
    `$XDG_RUNTIME_DIR/coppice/server.sock`, else `~/.opendaisugi/coppice/server.sock`.
  - `_validated(raw: dict | None) -> PaneStateEvent | None` — drops any event whose
    `state` is not in `STATES` or whose `source` is not in `SOURCES`.
  - `CoppiceBackend(sock_path: Path | None = None, *, autostart: bool = False,
    timeout_s: float = 0.3)` implementing `PaneBackend`, with `name = "coppice"`,
    `yields_frames = True`, and `prompt(ref, text, *, wait=False, timeout_s=60.0) -> str`.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_coppice_backend.py
"""The coppice socket client, against a fake JSONL server in a thread.

The real binary is optional. What must hold everywhere: `available()` never
raises, a server error is an error and never an empty success, and autostart is
off unless the caller asks for it (opening a screen must not spawn a daemon).
"""

from __future__ import annotations

import json
import os
import socket
import threading
from pathlib import Path

import pytest

from opendaisugi.floor import Frame, PaneRef, PaneStateEvent
from opendaisugi.floor.coppice_backend import (
    CoppiceBackend,
    CoppiceError,
    default_socket_path,
)
from tests.floor import hostfacts


class FakeServer:
    """One JSONL request per line, one reply per line. Handlers keyed by cmd."""

    def __init__(self, path: Path, handlers: dict, *, events: list[dict] | None = None):
        self.path = path
        self.handlers = handlers
        self.events = events or []
        self.seen: list[dict] = []
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.bind(str(path))
        os.chmod(path, 0o600)
        self._sock.listen(8)
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def _serve(self):
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn):
        with conn, conn.makefile("rwb") as stream:
            for line in stream:
                req = json.loads(line)
                self.seen.append(req)
                if req.get("cmd") == "events.subscribe":
                    stream.write(json.dumps({"id": req["id"], "ok": True}).encode() + b"\n")
                    stream.flush()
                    for event in self.events:
                        stream.write(json.dumps(event).encode() + b"\n")
                        stream.flush()
                    return
                handler = self.handlers.get(req.get("cmd"))
                reply = (
                    handler(req)
                    if handler
                    else {
                        "id": req.get("id"),
                        "ok": False,
                        "error": {"code": "bad_request", "message": "no handler"},
                    }
                )
                stream.write(json.dumps(reply).encode() + b"\n")
                stream.flush()

    def close(self):
        self._stop.set()
        self._sock.close()


@pytest.fixture
def server_factory(tmp_path):
    made: list[FakeServer] = []

    def make(handlers, events=None):
        srv = FakeServer(tmp_path / "server.sock", handlers, events=events)
        made.append(srv)
        return srv

    yield make
    for srv in made:
        srv.close()


def test_default_socket_prefers_xdg_runtime_dir(tmp_path):
    got = default_socket_path({"XDG_RUNTIME_DIR": str(tmp_path)})
    assert got == tmp_path / "coppice" / "server.sock"


def test_default_socket_falls_back_to_the_data_dir():
    got = default_socket_path({})
    assert got == Path.home() / ".opendaisugi" / "coppice" / "server.sock"


def test_available_is_false_with_no_socket_and_never_raises(tmp_path):
    backend = CoppiceBackend(sock_path=tmp_path / "nothing.sock")
    assert backend.available() is False


def test_available_does_not_start_a_server_unless_asked(tmp_path, monkeypatch):
    calls = []
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", lambda *a, **k: calls.append(a) or None)
    assert CoppiceBackend(sock_path=tmp_path / "nothing.sock").available() is False
    assert calls == [], "opening the floor must not spawn a daemon by itself"


def test_autostart_tries_once_and_stays_false_when_the_start_fails(tmp_path, monkeypatch):
    calls = []

    def fake_run(argv, **kwargs):
        calls.append(argv)
        raise OSError("no such binary")

    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr("subprocess.run", fake_run)
    backend = CoppiceBackend(sock_path=tmp_path / "nothing.sock", autostart=True)
    assert backend.available() is False
    assert backend.available() is False
    assert len(calls) == 1, "autostart must try once per process, not on every probe"


def test_available_is_true_when_status_answers(tmp_path, server_factory):
    server_factory({"server.status": lambda r: {"id": r["id"], "ok": True, "result": {}}})
    assert CoppiceBackend(sock_path=tmp_path / "server.sock").available() is True


def test_spawn_sends_pane_create_and_returns_the_ref(tmp_path, server_factory):
    srv = server_factory(
        {"pane.create": lambda r: {"id": r["id"], "ok": True, "result": {"pane": "w1:p1"}}}
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    ref = backend.spawn(cwd=tmp_path, cmd=["claude"], env={"K": "V"}, label="auth fix", kind="pty")
    assert ref == PaneRef("coppice", "w1:p1")
    req = next(r for r in srv.seen if r["cmd"] == "pane.create")
    assert req["cwd"] == str(tmp_path)
    assert req["cmd_argv"] == ["claude"]
    assert req["env"] == {"K": "V"}
    assert req["label"] == "auth fix" and req["kind"] == "pty"


def test_a_server_error_raises_with_its_code_never_an_empty_success(tmp_path, server_factory):
    server_factory(
        {
            "pane.read": lambda r: {
                "id": r["id"],
                "ok": False,
                "error": {"code": "no_such_pane", "message": "gone"},
            }
        }
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    with pytest.raises(CoppiceError) as e:
        backend.read(PaneRef("coppice", "w1:p9"))
    assert e.value.code == "no_such_pane" and "gone" in str(e.value)


def test_read_passes_the_source_through(tmp_path, server_factory):
    srv = server_factory(
        {"pane.read": lambda r: {"id": r["id"], "ok": True, "result": {"text": "READY\n"}}}
    )
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    assert backend.read(PaneRef("coppice", "w1:p1"), source="detection") == "READY\n"
    assert next(r for r in srv.seen if r["cmd"] == "pane.read")["source"] == "detection"


def test_prompt_uses_agent_prompt_not_typed_text(tmp_path, server_factory):
    srv = server_factory({"agent.prompt": lambda r: {"id": r["id"], "ok": True, "result": {}}})
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    assert backend.prompt(PaneRef("coppice", "w1:p1"), "fix the test", wait=True) == "sent"
    req = next(r for r in srv.seen if r["cmd"] == "agent.prompt")
    assert req["text"] == "fix the test" and req["wait"] is True


def test_subscribe_yields_state_events_and_frames(tmp_path, server_factory):
    events = [
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "claude-code",
            "state": "blocked",
            "source": "gate",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
        {
            "event": "frame",
            "pane": "w1:p1",
            "seq": 3,
            "cols": 80,
            "rows": 24,
            "cursor": [1, 2],
            "rows_changed": {"0": [["hi", "white", "black", 0]]},
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    got = []
    for item in backend.subscribe():
        got.append(item)
        if len(got) == 2:
            break
    assert isinstance(got[0], PaneStateEvent) and got[0].state == "blocked"
    assert isinstance(got[1], Frame) and got[1].seq == 3 and got[1].cursor == (1, 2)


def test_an_unparsable_event_line_is_skipped_not_fatal(tmp_path, server_factory):
    events = [
        {"event": "state", "pane": "w1:p1", "state": "not-a-state"},
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "process",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    backend = CoppiceBackend(sock_path=tmp_path / "server.sock")
    first = next(iter(backend.subscribe()))
    assert isinstance(first, PaneStateEvent) and first.state == "idle"


def test_a_state_outside_the_enum_is_dropped_from_list(tmp_path, server_factory):
    """A server that says `ready` must not paint `ready` where `unknown` belongs."""
    server_factory(
        {
            "pane.list": lambda r: {
                "id": r["id"],
                "ok": True,
                "result": {
                    "panes": [
                        {
                            "pane": "w1:p1",
                            "label": "x",
                            "cwd": "/",
                            "cmd_argv": [],
                            "kind": "pty",
                            "state": {
                                "v": 1,
                                "ts": 1.0,
                                "session_id": "s",
                                "harness": "shell",
                                "state": "ready",
                                "source": "process",
                                "harness_session_id": None,
                                "ask": None,
                                "detail": "",
                            },
                        }
                    ]
                },
            }
        }
    )
    infos = CoppiceBackend(sock_path=tmp_path / "server.sock").list()
    assert infos[0].state is None


def test_a_source_outside_the_enum_is_dropped_from_subscribe(tmp_path, server_factory):
    events = [
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "vibes",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
        {
            "event": "state",
            "pane": "w1:p1",
            "v": 1,
            "ts": 1.0,
            "session_id": "s",
            "harness": "shell",
            "state": "idle",
            "source": "process",
            "harness_session_id": None,
            "ask": None,
            "detail": "",
        },
    ]
    server_factory({}, events=events)
    first = next(iter(CoppiceBackend(sock_path=tmp_path / "server.sock").subscribe()))
    assert first.source == "process"


def test_the_backend_declares_that_it_yields_frames():
    assert CoppiceBackend.yields_frames is True


def test_against_the_real_binary_when_it_is_built(tmp_path):
    reason = hostfacts.skip_reason("coppice")
    if reason:
        pytest.skip(reason)
    backend = CoppiceBackend(autostart=True)
    assert backend.available() is True
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "echo READY; sleep 2"], env={}, label="probe", kind="pty"
    )
    try:
        assert ref.id
    finally:
        backend.close(ref)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_coppice_backend.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.floor.coppice_backend'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/floor/coppice_backend.py
"""The native pane backend: a stdlib JSONL client for coppice-server (master §3.3).

One connection per request, plus one long-lived connection for `events.subscribe`.
Stdlib socket and json only, because this runs in the cockpit, on a phone-facing
box, and inside a hook process that must stay cheap.

`available()` costs one `server.status` round trip with a 300 ms budget. Whether it
may start a server is the CALLER's choice, threaded through
`registry.pick_backend(..., autostart=...)`:

* `backend: auto` and `daisugi coppice backends` pass `autostart=False`. Opening a
  screen, or asking a diagnostic a question, must not spawn a daemon.
* `daisugi coppice <verb> --backend coppice` passes `autostart=True`. The operator
  named coppice, so starting it is what they asked for.

Spec-03 says `available()` starts the server once when the socket is absent. This is
that behaviour, with the deviation written down: it is opt-in per call rather than
unconditional, because `backends` calls `available()` on all three. Even with
`autostart=True` the start is tried once per process and a failure returns False
rather than raising.

Events are validated before they leave here. Plan 01's `PaneStateEvent.from_json`
checks required fields, not the `state` and `source` enums, so a server that emits
`state: "ready"` would paint an unknown word where `unknown` belongs. Anything
outside `STATES` / `SOURCES` is dropped, and the pane shows no state at all.

Frames arrive as they come. Coalescing is the widget's job (spec-03), not ours.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import stat
import subprocess
import time
from collections.abc import Iterator, Mapping
from pathlib import Path
from typing import Literal

from opendaisugi.exceptions import OpenDaisugiError
from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.events import SOURCES, STATES

_PROBE_S = 0.3
_CALL_S = 5.0


def _validated(raw: dict | None) -> PaneStateEvent | None:
    """A PaneStateEvent whose state and source are in the master's enums, else None.

    Master §3.1 closes both enums. `from_json` does not check them (plan 01 keeps that
    in its own hook-report validator), so the check lives at every boundary that reads
    a state off the wire. A word we do not know is no state, never a painted guess.
    """
    if not raw:
        return None
    try:
        ev = PaneStateEvent.from_json(json.dumps(raw))
    except (ValueError, KeyError, TypeError):
        return None
    if ev.state not in STATES or ev.source not in SOURCES:
        return None
    return ev


class CoppiceError(OpenDaisugiError):
    """coppice-server answered `ok: false`. ``code`` is one of its closed enum."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code


def default_socket_path(env: Mapping[str, str] | None = None) -> Path:
    """`$XDG_RUNTIME_DIR/coppice/server.sock`, else `~/.opendaisugi/coppice/server.sock`."""
    env = os.environ if env is None else env
    runtime = env.get("XDG_RUNTIME_DIR")
    if runtime:
        return Path(runtime) / "coppice" / "server.sock"
    return Path.home() / ".opendaisugi" / "coppice" / "server.sock"


class CoppiceBackend:
    """Speak the coppice socket API. The only backend that yields frames."""

    name = "coppice"
    yields_frames = True  # master §3.2: herdr and tmux yield text, not frames

    def __init__(
        self,
        sock_path: Path | None = None,
        *,
        autostart: bool = False,
        timeout_s: float = _PROBE_S,
    ) -> None:
        self.sock_path = Path(sock_path) if sock_path else default_socket_path()
        self._autostart = autostart
        self._probe_s = timeout_s
        self._start_tried = False
        self._seq = 0

    # --- wire -------------------------------------------------------------
    def _is_socket(self) -> bool:
        try:
            st = os.lstat(self.sock_path)
        except OSError:
            return False
        return stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid()

    def _connect(self, timeout_s: float) -> socket.socket:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout_s)
        sock.connect(str(self.sock_path))
        return sock

    def _call(self, cmd: str, timeout_s: float = _CALL_S, **fields) -> dict:
        """One request, one reply. Raises CoppiceError on `ok: false`."""
        self._seq += 1
        request = {"id": str(self._seq), "cmd": cmd, **fields}
        with self._connect(timeout_s) as sock, sock.makefile("rwb") as stream:
            stream.write(json.dumps(request).encode() + b"\n")
            stream.flush()
            line = stream.readline()
        if not line:
            raise CoppiceError("internal", f"coppice closed the connection on {cmd}")
        reply = json.loads(line)
        if not reply.get("ok"):
            err = reply.get("error") or {}
            raise CoppiceError(str(err.get("code", "internal")), str(err.get("message", "")))
        return reply.get("result") or {}

    def _try_start(self) -> None:
        if self._start_tried or shutil.which("coppice") is None:
            return
        self._start_tried = True
        try:
            subprocess.run(
                ["coppice", "server", "start"], capture_output=True, timeout=5.0, check=False
            )
        except (OSError, subprocess.SubprocessError):
            return
        time.sleep(0.2)

    # --- PaneBackend ------------------------------------------------------
    def available(self) -> bool:
        """A socket that answers `server.status` inside the probe budget. Never raises."""
        try:
            if self._is_socket():
                self._call("server.status", timeout_s=self._probe_s)
                return True
        except Exception:  # noqa: BLE001 — master §3.2: available() never raises
            pass
        if not self._autostart:
            return False
        self._try_start()
        try:
            if self._is_socket():
                self._call("server.status", timeout_s=self._probe_s)
                return True
        except Exception:  # noqa: BLE001
            return False
        return False

    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"] = "pty",
        harness: str | None = None,
    ) -> PaneRef:
        result = self._call(
            "pane.create",
            cwd=str(cwd),
            cmd_argv=list(cmd),
            env=dict(env),
            label=label,
            kind=kind,
            harness=harness,
        )
        return PaneRef(backend=self.name, id=str(result["pane"]))

    def list(self) -> list[PaneInfo]:
        result = self._call("pane.list")
        out: list[PaneInfo] = []
        for row in result.get("panes", []):
            state = _validated(row.get("state"))
            out.append(
                PaneInfo(
                    ref=PaneRef(self.name, str(row["pane"])),
                    label=str(row.get("label", "")),
                    cwd=str(row.get("cwd", "")),
                    cmd=list(row.get("cmd_argv", [])),
                    kind=str(row.get("kind", "pty")),
                    state=state,
                )
            )
        return out

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._call("pane.send_text", pane=pane.id, text=text, enter=enter)

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        self._call("pane.send_keys", pane=pane.id, keys=list(keys))

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        return str(self._call("pane.read", pane=pane.id, source=source).get("text", ""))

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        self._call("pane.resize", pane=pane.id, cols=cols, rows=rows)

    def close(self, pane: PaneRef) -> None:
        try:
            self._call("pane.close", pane=pane.id)
        except CoppiceError as exc:
            if exc.code not in ("no_such_pane", "pane_closed"):
                raise

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        self._call("pane.report_state", pane=pane.id, event=json.loads(ev.to_json()))

    def prompt(
        self, pane: PaneRef, text: str, *, wait: bool = False, timeout_s: float = 60.0
    ) -> str:
        """A real agent prompt, not typed keystrokes. Used for headless panes."""
        self._call(
            "agent.prompt",
            pane=pane.id,
            text=text,
            wait=wait,
            timeout_ms=int(timeout_s * 1000),
            timeout_s=max(timeout_s + 1.0, _CALL_S),
        )
        return "sent"

    def subscribe(self) -> Iterator[PaneStateEvent | Frame]:
        """A second connection carrying `state` and `frame` events until it closes."""
        self._seq += 1
        request = {
            "id": str(self._seq),
            "cmd": "events.subscribe",
            "panes": "*",
            "kinds": ["state", "frame"],
        }
        with self._connect(None) as sock, sock.makefile("rwb") as stream:
            stream.write(json.dumps(request).encode() + b"\n")
            stream.flush()
            for line in stream:
                try:
                    payload = json.loads(line)
                except ValueError:
                    continue
                kind = payload.get("event")
                if kind == "frame":
                    yield Frame(
                        pane=str(payload["pane"]),
                        seq=int(payload["seq"]),
                        cols=int(payload["cols"]),
                        rows=int(payload["rows"]),
                        cursor=tuple(payload["cursor"]),
                        rows_changed={int(k): v for k, v in payload["rows_changed"].items()},
                    )
                elif kind == "state":
                    body = {k: v for k, v in payload.items() if k != "event"}
                    ev = _validated(body)
                    if ev is None:
                        continue  # a malformed or out-of-enum event is no event
                    yield ev
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_coppice_backend.py -q`
Expected: PASS (13 tests; the real-binary test skips with `the coppice binary is not on PATH — build it: cd harness/coppice && go build ./cmd/coppice`)

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/floor/coppice_backend.py tests/floor/test_coppice_backend.py
git commit -m "feat(floor): a stdlib socket client for coppice-server

One connection per call, one for events. available() never raises and never
starts a daemon on its own: opening a screen must not spawn a server the operator
did not ask for, so autostart is opt-in and tried once per process."
```

---

### Task 6: `herdr_backend.py` — pin the real verbs, then drive them

Herdr's published CLI reference (fetched 2026-09-08, `https://herdr.dev/docs/cli-reference/`)
disagrees with the sub-spec on two facts, so this task starts by recording the truth:

- **There is no `herdr pane create`.** Panes come from `herdr tab create [--cwd] [--label]
  [--env]`, `herdr workspace create …`, or `herdr pane split --direction right|down [--cwd]
  [--env]`. A command then runs with `herdr pane run <pane_id> <command>`, and a label is set with
  `herdr pane rename <pane_id> <label>`.
- **`--json` is documented on `session list`, `session stop`, `session delete`, `machine list`,
  `server agent-manifests`, `plugin list`, and `agent explain` only.** It is *not* documented on
  `pane list` or `agent list`, which are exactly the two commands `list()` and `subscribe()` need.

A third fact has no documented shape at all: **which field of `agent explain --json` names the
status authority.** That decides whether a Herdr state is stamped `source: gate` (a fact our gate
produced) or `source: manifest` (Herdr's own probabilistic screen guess), and those are two
different ranks in master §3.1. It is pinned as a PATH (`["authority", "source"]`) that the
backend walks and compares exactly. A substring search over the whole document would match
`daisugi` in a cwd, a pane label, or a command line, and on the operator's own box every pane
under `.../openDaisugi` would then be stamped `gate`. Fail closed: a missing field, a non-dict, or
any other spelling yields `manifest`.

Everything else the backend runs is pinned too: the argv for `pane read`, `pane send-text`,
`pane send-keys`, `pane close`, `pane list`, `agent list`, `agent prompt`, `agent explain`, and
`pane report-agent`, plus the key-name map. The pin exists so none of it is a literal in the code.

All of it carries `verified: false` until a box with `herdr` on it confirms it.
`herdr_backend.py` reads the pin. It never hardcodes `pane create`.

**Files:**
- Create: `src/opendaisugi/floor/herdr_verbs.json`
- Create: `src/opendaisugi/floor/herdr_backend.py`
- Test: `tests/floor/test_herdr_backend.py`

**Interfaces:**
- Consumes: `opendaisugi.floor` value types; `tests/floor/hostfacts.py`.
- Produces:
  - `HerdrVerbs` frozen dataclass: `verified: bool`, `source: str`, `spawn_chain: tuple[str, ...]`,
    `json_commands: frozenset[str]`, `states: tuple[str, ...]`, `read_sources: tuple[str, ...]`,
    `pane_env_vars: tuple[str, ...]`, `authority_field: tuple[str, ...]`,
    `authority_value: str`, `argv: dict[str, tuple[str, ...]]`, `keys: dict[str, str]`,
    and `verb(name) -> list[str]`.
  - `load_verbs(path: Path | None = None) -> HerdrVerbs`
  - `HerdrBackend(verbs: HerdrVerbs | None = None, *, timeout_s: float = 0.5)` implementing
    `PaneBackend`, `name = "herdr"`, `yields_frames = False`,
    `spawn(..., harness: str | None = None)`, plus `prompt(ref, text, *, wait, timeout_s) -> str`.
  - `STATE_OUT: dict[str, str]` — our states → Herdr's `idle|working|blocked|unknown`.

- [ ] **Step 1: Record the verb pin**

```bash
cat > src/opendaisugi/floor/herdr_verbs.json <<'JSON'
{
  "verified": false,
  "source": "https://herdr.dev/docs/cli-reference/ fetched 2026-09-08",
  "note": "no herdr on the box that recorded this. Re-run with herdr installed and set verified true.",
  "spawn_chain": ["tab create", "pane run", "pane rename"],
  "spawn_chain_alternatives": [["pane split", "pane run", "pane rename"]],
  "json_commands": [
    "session list", "session stop", "session delete", "machine list",
    "server agent-manifests", "plugin list", "agent explain"
  ],
  "list_commands_without_json": ["pane list", "agent list"],
  "states": ["idle", "working", "blocked", "unknown"],
  "read_sources": ["visible", "recent", "recent-unwrapped", "detection"],
  "pane_env_vars": ["HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID", "HERDR_ENV"],
  "report_agent": "pane report-agent <pane_id> --source ID --agent LABEL --state STATE",
  "explain": "agent explain <target> --json",
  "authority_field": ["authority", "source"],
  "authority_value": "daisugi",
  "argv": {
    "read": ["pane", "read"],
    "send_text": ["pane", "send-text"],
    "send_keys": ["pane", "send-keys"],
    "close": ["pane", "close"],
    "list_panes": ["pane", "list"],
    "list_agents": ["agent", "list"],
    "prompt": ["agent", "prompt"],
    "explain": ["agent", "explain"],
    "report_agent": ["pane", "report-agent"]
  },
  "keys": {
    "enter": "enter", "esc": "esc", "escape": "esc", "tab": "tab",
    "backspace": "backspace", "space": "space",
    "up": "up", "down": "down", "left": "left", "right": "right",
    "ctrl+a": "ctrl+a", "ctrl+b": "ctrl+b", "ctrl+c": "ctrl+c", "ctrl+d": "ctrl+d",
    "ctrl+r": "ctrl+r", "ctrl+z": "ctrl+z",
    "f1": "f1", "f2": "f2", "f3": "f3", "f4": "f4"
  }
}
JSON
```

- [ ] **Step 2: Write the failing test**

```python
# tests/floor/test_herdr_backend.py
"""Herdr as a pane backend, driven through a fake `herdr` on PATH that records argv.

Master §5.5: Herdr stays first-class, not a competitor we pretend not to see. The
spawn chain and the --json ladder are PINNED facts (herdr_verbs.json), not
guesses, because the sub-spec's `herdr pane create` does not exist in Herdr's own
CLI reference.
"""

from __future__ import annotations

import json
import os
import stat

import pytest

from opendaisugi.floor import PaneRef
from opendaisugi.floor.herdr_backend import (
    STATE_OUT,
    HerdrBackend,
    load_verbs,
)
from tests.floor import hostfacts


@pytest.fixture
def fake_herdr(tmp_path, monkeypatch):
    """A `herdr` on PATH that records argv and replays canned stdout per verb."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    log = tmp_path / "argv.log"
    # /bin/sh, not python3: `available()` probes with a 500 ms budget and a cold
    # interpreter start can miss it on a loaded box.
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        f"printf '%s\\n' \"$*\" >> {log}\n"
        f'key="$1 $2"\n'
        f'out="{tmp_path}/replies/$(printf %s "$key" | tr " /" "__").out"\n'
        f'rc="{tmp_path}/replies/$(printf %s "$key" | tr " /" "__").rc"\n'
        '[ -f "$out" ] && cat "$out"\n'
        '[ -f "$rc" ] && exit "$(cat "$rc")"\n'
        "exit 0\n",
        encoding="utf-8",
    )
    (tmp_path / "replies").mkdir()
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    class Fake:
        def argv(self):
            if not log.exists():
                return []
            return [line.split(" ") for line in log.read_text().splitlines() if line]

        def reply(self, key, out="", rc=0):
            stem = tmp_path / "replies" / key.replace(" ", "_").replace("/", "_")
            stem.with_suffix(".out").write_text(out, encoding="utf-8")
            stem.with_suffix(".rc").write_text(str(rc), encoding="utf-8")

    return Fake()


def test_the_pin_records_that_pane_create_does_not_exist():
    verbs = load_verbs()
    assert "pane create" not in verbs.spawn_chain
    assert verbs.spawn_chain[0] in ("tab create", "pane split")
    assert verbs.source.startswith("https://herdr.dev/docs/cli-reference/")


def test_the_pin_records_which_commands_take_json():
    verbs = load_verbs()
    assert "session list" in verbs.json_commands
    assert "pane list" not in verbs.json_commands, (
        "pane list --json is undocumented; the backend must fall back to text"
    )


def test_state_map_never_sends_done_to_herdr_and_never_calls_it_idle():
    """Herdr takes idle|working|blocked|unknown. `idle` means "ready for work"."""
    assert set(STATE_OUT.values()) <= {"idle", "working", "blocked", "unknown"}
    assert STATE_OUT["done"] == "unknown", (
        "a pane whose process exited is not ready for work; unknown is the honest cell"
    )


def test_a_done_report_carries_the_word_done_in_the_message(fake_herdr):
    from opendaisugi.floor import PaneStateEvent

    ev = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="done",
        source="process",
        ts=1.0,
        pane="p1",
        detail="exit 0",
    )
    HerdrBackend().report_state(PaneRef("herdr", "p1"), ev)
    argv = fake_herdr.argv()[-1]
    assert "--state" in argv and "unknown" in argv
    assert "--message" in argv and any("done" in a for a in argv)


def test_available_is_true_when_session_list_json_exits_zero(fake_herdr):
    fake_herdr.reply("session list", out="[]\n", rc=0)
    assert HerdrBackend().available() is True
    assert ["session", "list", "--json"] in fake_herdr.argv()


def test_available_is_false_when_herdr_exits_nonzero(fake_herdr):
    fake_herdr.reply("session list", out="", rc=1)
    assert HerdrBackend().available() is False


def test_available_is_false_with_no_herdr_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert HerdrBackend().available() is False


def test_spawn_uses_the_pinned_chain_not_pane_create(fake_herdr, tmp_path):
    fake_herdr.reply("tab create", out="pane_id: p7\n", rc=0)
    ref = HerdrBackend().spawn(
        cwd=tmp_path, cmd=["claude"], env={"COPPICE_PANE": "p7"}, label="auth fix", kind="pty"
    )
    verbs = [" ".join(a[:2]) for a in fake_herdr.argv()]
    assert "pane create" not in verbs
    assert verbs[0] == "tab create"
    assert "pane run" in verbs
    assert ref == PaneRef("herdr", "p7")
    create = next(a for a in fake_herdr.argv() if a[:2] == ["tab", "create"])
    assert "--cwd" in create and str(tmp_path) in create
    assert "--env" in create


def test_read_passes_the_pinned_source_names(fake_herdr):
    fake_herdr.reply("pane read", out="READY\n", rc=0)
    backend = HerdrBackend()
    assert backend.read(PaneRef("herdr", "p1"), source="detection") == "READY\n"
    argv = fake_herdr.argv()[-1]
    assert argv[:2] == ["pane", "read"] and "--source" in argv and "detection" in argv


def test_report_state_uses_report_agent_with_the_daisugi_source(fake_herdr):
    from opendaisugi.floor import PaneStateEvent

    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="blocked", source="gate", ts=1.0, pane="p1"
    )
    HerdrBackend().report_state(PaneRef("herdr", "p1"), ev)
    argv = fake_herdr.argv()[-1]
    assert argv[:2] == ["pane", "report-agent"]
    assert "--source" in argv and "daisugi" in argv
    assert "--state" in argv and "blocked" in argv
    assert "--agent" in argv and "claude-code" in argv


def _explain(fake_herdr, payload):
    fake_herdr.reply("agent explain", out=json.dumps(payload), rc=0)
    fake_herdr.reply("agent list", out="p1 claude-code blocked\n", rc=0)
    infos = HerdrBackend().list()
    return [i.state for i in infos if i.state is not None]


def test_the_pin_records_the_authority_path_not_a_substring_rule():
    verbs = load_verbs()
    assert verbs.authority_field == ("authority", "source")
    assert verbs.authority_value == "daisugi"


def test_state_source_is_gate_only_when_the_pinned_field_says_daisugi(fake_herdr):
    states = _explain(
        fake_herdr, {"agent": "claude-code", "state": "blocked", "authority": {"source": "daisugi"}}
    )
    assert states and states[0].source == "gate"


def test_state_source_is_manifest_when_the_field_names_someone_else(fake_herdr):
    states = _explain(
        fake_herdr,
        {"agent": "claude-code", "state": "blocked", "authority": {"source": "screen-manifest"}},
    )
    assert states and states[0].source == "manifest"


def test_daisugi_anywhere_else_in_the_document_never_forges_a_gate_source(fake_herdr):
    """The operator's own repo lives at .../openDaisugi. A cwd is not an authority."""
    states = _explain(
        fake_herdr,
        {
            "agent": "claude-code",
            "state": "blocked",
            "cwd": "/home/user/openDaisugi",
            "command": "claude --resume daisugi-1",
            "label": "daisugi floor",
            "authority": {"source": "screen-manifest"},
        },
    )
    assert states and states[0].source == "manifest"


def test_a_missing_authority_field_is_manifest_not_gate(fake_herdr):
    states = _explain(fake_herdr, {"agent": "claude-code", "state": "blocked"})
    assert states and states[0].source == "manifest"


def test_an_authority_field_of_the_wrong_shape_is_manifest_not_gate(fake_herdr):
    for payload in (
        {"authority": "daisugi"},  # not a dict at the first hop
        {"authority": {"source": {"name": "daisugi"}}},  # a dict where a name belongs
        {"authority": {"source": ["daisugi"]}},  # a list where a name belongs
        {"authority": {"other": "daisugi"}},  # the wrong leaf
    ):
        payload.update({"agent": "claude-code", "state": "blocked"})
        states = _explain(fake_herdr, payload)
        assert states and states[0].source == "manifest", payload


def test_explain_that_fails_is_manifest_not_gate(fake_herdr):
    fake_herdr.reply("agent explain", out="", rc=1)
    fake_herdr.reply("agent list", out="p1 claude-code blocked\n", rc=0)
    infos = HerdrBackend().list()
    states = [i.state for i in infos if i.state is not None]
    assert states and states[0].source == "manifest"


def test_the_pin_records_every_verb_the_backend_runs():
    verbs = load_verbs()
    for name in (
        "read",
        "send_text",
        "send_keys",
        "close",
        "list_panes",
        "list_agents",
        "prompt",
        "explain",
        "report_agent",
    ):
        assert verbs.verb(name), f"{name} is not pinned"
    assert verbs.keys["ctrl+c"] == "ctrl+c" and verbs.keys["esc"] == "esc"


def test_a_key_herdr_rejects_raises_instead_of_being_dropped(fake_herdr):
    """A silent no-op would let the contract's send-keys test pass on nothing."""
    fake_herdr.reply("pane send-keys", out="", rc=1)
    with pytest.raises(RuntimeError) as e:
        HerdrBackend().send_keys(PaneRef("herdr", "p1"), ["ctrl+c"])
    assert "rejected" in str(e.value)


def test_an_unknown_key_name_is_refused_before_the_subprocess(fake_herdr):
    with pytest.raises(ValueError) as e:
        HerdrBackend().send_keys(PaneRef("herdr", "p1"), ["hyperspace"])
    assert "ctrl+c" in str(e.value)
    assert not any(a[:2] == ["pane", "send-keys"] for a in fake_herdr.argv())


def test_spawn_accepts_the_protocols_harness_keyword_and_ignores_it(fake_herdr, tmp_path):
    fake_herdr.reply("tab create", out="pane_id: p7\n", rc=0)
    ref = HerdrBackend().spawn(
        cwd=tmp_path, cmd=["claude"], env={}, label="l", kind="pty", harness="claude-code"
    )
    assert ref.id == "p7"


def test_a_herdr_that_times_out_is_unavailable_not_an_exception(fake_herdr, monkeypatch):
    import subprocess

    def boom(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd="herdr", timeout=0.5)

    monkeypatch.setattr("subprocess.run", boom)
    assert HerdrBackend().available() is False


def test_against_real_herdr_when_it_is_installed(tmp_path):
    reason = hostfacts.skip_reason("herdr")
    if reason:
        pytest.skip(reason)
    backend = HerdrBackend()
    assert backend.available() is True
    verbs = load_verbs()
    assert verbs.verified, (
        "herdr is installed but herdr_verbs.json is still unverified — "
        "confirm the spawn chain and the --json ladder, then set verified true"
    )
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_herdr_backend.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.floor.herdr_backend'`

- [ ] **Step 4: Write the implementation**

```python
# src/opendaisugi/floor/herdr_backend.py
"""Herdr as a pane backend, driven through its CLI (master §5.5).

Herdr stays first-class. A Herdr user gets exact `blocked` from our gate on day
one, and a coppice user can fall back to Herdr without relearning a vocabulary.

Two facts are PINNED in `herdr_verbs.json` rather than hardcoded, because Herdr's
own CLI reference contradicted the sub-spec:

* There is no `herdr pane create`. A pane comes from `tab create` (or
  `pane split`), then `pane run <pane_id> <command>`, then `pane rename` for the
  label.
* `--json` is documented on `session list`, `agent explain`, and a handful of
  others. It is NOT documented on `pane list` or `agent list`, the two this
  backend reads most. So every list call tries `--json` first and falls back to
  parsing the text table.

The pin carries `verified: false` until a box with `herdr` on it confirms both.
Read the pin, never the sub-spec, for the argv.

State source: `gate` only when `herdr agent explain <target> --json` names `daisugi`
at the PINNED authority path, compared exactly. Anything else is `manifest`. A
substring search would stamp Herdr's screen guess as a gate fact, and §5.1 is the one
axis this project competes on, so it fails closed: no field, no dict, no match, no
`gate`.

Herdr's `idle|working|blocked|unknown` map to ours 1:1. Our `done` maps to Herdr's
`unknown`, not `idle`: Herdr has no `done`, and `idle` in Herdr's UI means "ready for
work", which a pane whose process exited is not. The word `done` rides along in
`--message` so a Herdr user still sees it.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import time
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent

_PIN = Path(__file__).with_name("herdr_verbs.json")
# Used only when the pin is unreadable. The pin is the source of truth.
_DEFAULT_ARGV = {
    "read": ["pane", "read"],
    "send_text": ["pane", "send-text"],
    "send_keys": ["pane", "send-keys"],
    "close": ["pane", "close"],
    "list_panes": ["pane", "list"],
    "list_agents": ["agent", "list"],
    "prompt": ["agent", "prompt"],
    "explain": ["agent", "explain"],
    "report_agent": ["pane", "report-agent"],
}
_PROBE_S = 0.5
_CALL_S = 10.0
_POLL_S = 1.0

# Our states → the four Herdr accepts on `pane report-agent --state`.
# `done` → `unknown`, never `idle`: `idle` in Herdr's UI reads as "ready for work",
# and a pane whose process exited is not that. The real word goes in `--message`.
STATE_OUT: dict[str, str] = {
    "idle": "idle",
    "working": "working",
    "blocked": "blocked",
    "done": "unknown",
    "unknown": "unknown",
}


@dataclass(frozen=True)
class HerdrVerbs:
    verified: bool
    source: str
    spawn_chain: tuple[str, ...]
    json_commands: frozenset[str]
    states: tuple[str, ...]
    read_sources: tuple[str, ...]
    pane_env_vars: tuple[str, ...]
    authority_field: tuple[str, ...]
    authority_value: str
    argv: dict[str, tuple[str, ...]]
    keys: dict[str, str]

    def verb(self, name: str) -> list[str]:
        """The pinned argv prefix for one operation. Never a literal in the code."""
        return list(self.argv[name])


def load_verbs(path: Path | None = None) -> HerdrVerbs:
    """Read the pinned Herdr CLI facts. An unreadable pin is an unverified one."""
    try:
        raw = json.loads((path or _PIN).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raw = {}
    return HerdrVerbs(
        verified=bool(raw.get("verified")),
        source=str(raw.get("source", "")),
        spawn_chain=tuple(raw.get("spawn_chain") or ("tab create", "pane run", "pane rename")),
        json_commands=frozenset(raw.get("json_commands") or ()),
        states=tuple(raw.get("states") or ("idle", "working", "blocked", "unknown")),
        read_sources=tuple(raw.get("read_sources") or ("visible", "recent", "detection")),
        pane_env_vars=tuple(raw.get("pane_env_vars") or ("HERDR_PANE_ID",)),
        authority_field=tuple(raw.get("authority_field") or ("authority", "source")),
        authority_value=str(raw.get("authority_value") or "daisugi"),
        argv={k: tuple(v) for k, v in (raw.get("argv") or _DEFAULT_ARGV).items()},
        keys=dict(raw.get("keys") or {}),
    )


class HerdrBackend:
    """Drive Herdr panes over its CLI. No frames: Herdr yields text (master §3.2)."""

    name = "herdr"
    yields_frames = False

    def __init__(self, verbs: HerdrVerbs | None = None, *, timeout_s: float = _PROBE_S) -> None:
        self.verbs = verbs or load_verbs()
        self._probe_s = timeout_s

    # --- process plumbing -------------------------------------------------
    def _run(self, args: list[str], *, timeout_s: float = _CALL_S) -> subprocess.CompletedProcess:
        exe = shutil.which("herdr")
        if exe is None:
            raise FileNotFoundError("herdr is not on PATH")
        return subprocess.run(
            [exe, *args], capture_output=True, text=True, timeout=timeout_s, check=False
        )

    def _list_json(self, verb: list[str]) -> tuple[object | None, str]:
        """Try ``--json`` when the pin says the verb takes it. Returns (parsed, text)."""
        key = " ".join(verb[:2])
        if key in self.verbs.json_commands:
            proc = self._run([*verb, "--json"])
            if proc.returncode == 0:
                try:
                    return json.loads(proc.stdout), proc.stdout
                except ValueError:
                    return None, proc.stdout
        proc = self._run(verb)
        return None, proc.stdout if proc.returncode == 0 else ""

    # --- PaneBackend ------------------------------------------------------
    def available(self) -> bool:
        """`herdr session list --json` exits 0 inside the probe budget. Never raises."""
        try:
            if shutil.which("herdr") is None:
                return False
            proc = self._run(["session", "list", "--json"], timeout_s=self._probe_s)
            return proc.returncode == 0
        except Exception:  # noqa: BLE001 — master §3.2
            return False

    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"] = "pty",
        harness: str | None = None,
    ) -> PaneRef:
        """Create a pane through the PINNED chain, then run the command in it.

        ``harness`` is part of the §3.2 protocol so one call site drives all three
        backends. Herdr picks its own agent kind from the process, so it is ignored.
        """
        create, run_verb, rename = (list(v.split()) for v in self.verbs.spawn_chain[:3])
        args = [*create, "--cwd", str(cwd)]
        if label:
            args += ["--label", label]
        for key, value in env.items():
            args += ["--env", f"{key}={value}"]
        proc = self._run(args)
        if proc.returncode != 0:
            raise RuntimeError(f"herdr {' '.join(create)}: {proc.stderr.strip()}")
        pane_id = _parse_pane_id(proc.stdout)
        if not pane_id:
            raise RuntimeError(
                f"herdr {' '.join(create)} printed no pane id: {proc.stdout.strip()!r}"
            )
        if cmd:
            self._run([*run_verb, pane_id, " ".join(cmd)])
        if label:
            self._run([*rename, pane_id, label])
        return PaneRef(backend=self.name, id=pane_id)

    def list(self) -> list[PaneInfo]:
        parsed, text = self._list_json(self.verbs.verb("list_panes"))
        rows = parsed if isinstance(parsed, list) else _parse_table(text)
        agents = self._agent_states()
        out: list[PaneInfo] = []
        for row in rows:
            pane_id = str(row.get("pane_id") or row.get("id") or row.get("col0") or "")
            if not pane_id:
                continue
            ref = PaneRef(self.name, pane_id)
            out.append(
                PaneInfo(
                    ref=ref,
                    label=str(row.get("label") or row.get("col1") or ""),
                    cwd=str(row.get("cwd") or ""),
                    cmd=list(row.get("cmd") or []),
                    kind="pty",
                    state=agents.get(pane_id),
                )
            )
        return out

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._run([*self.verbs.verb("send_text"), pane.id, text])
        if enter:
            self.send_keys(pane, ["enter"])

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        """Map our key names through the pin, then send them. A rejection raises.

        `_run` ignores the return code everywhere else, which is fine for a read. Here
        it is not: a key Herdr refuses would be silently dropped, and the contract's
        `test_send_keys_accepts_the_names_the_floor_binds` would pass on a no-op.
        """
        mapped: list[str] = []
        for key in keys:
            name = self.verbs.keys.get(key.lower())
            if name is None and len(key) != 1:
                raise ValueError(
                    f"unknown key {key!r}. Known keys: {', '.join(sorted(self.verbs.keys))}, "
                    f"or one printable character."
                )
            mapped.append(name or key)
        proc = self._run([*self.verbs.verb("send_keys"), pane.id, *mapped])
        if proc.returncode != 0:
            raise RuntimeError(f"herdr rejected the keys {' '.join(mapped)}: {proc.stderr.strip()}")

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        if source not in self.verbs.read_sources:
            raise ValueError(
                f"herdr has no read source {source!r}. "
                f"Known sources: {', '.join(self.verbs.read_sources)}."
            )
        proc = self._run([*self.verbs.verb("read"), pane.id, "--source", source])
        return proc.stdout if proc.returncode == 0 else ""

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        """A no-op: Herdr owns its layout, and its resize is directional, not sized."""
        return None

    def close(self, pane: PaneRef) -> None:
        self._run([*self.verbs.verb("close"), pane.id])

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        mapped = STATE_OUT.get(ev.state)
        if mapped is None:
            return
        args = [
            *self.verbs.verb("report_agent"),
            pane.id,
            "--source",
            self.verbs.authority_value,
            "--agent",
            ev.harness,
            "--state",
            mapped,
        ]
        # Herdr has no `done`. Say it in the message so a Herdr user still sees it.
        message = f"done: {ev.detail}".strip(": ") if ev.state == "done" else ev.detail
        if message:
            args += ["--message", message[:200]]
        self._run(args)

    def prompt(
        self, pane: PaneRef, text: str, *, wait: bool = False, timeout_s: float = 60.0
    ) -> str:
        args = [*self.verbs.verb("prompt"), pane.id, text]
        if wait:
            args += ["--wait", "--timeout", str(int(timeout_s * 1000))]
        self._run(args, timeout_s=timeout_s + 5.0)
        return "sent"

    def subscribe(self) -> Iterator[PaneStateEvent | Frame]:
        """Poll `agent list` every second. Herdr has no event stream we can tail."""
        last: dict[str, str] = {}
        while True:
            for pane_id, ev in self._agent_states().items():
                if last.get(pane_id) != ev.state:
                    last[pane_id] = ev.state
                    yield ev
            time.sleep(_POLL_S)

    # --- state ------------------------------------------------------------
    def _authority_is_daisugi(self, pane_id: str) -> bool:
        """`agent explain --json` says who decided. Only the gate may speak as the gate.

        Walks the PINNED path and compares exactly. Never a substring search over the
        document: `daisugi` appears in a cwd like `/work/openDaisugi`, in a pane label,
        and in a command line, and matching any of those would stamp Herdr's screen
        guess with the top-but-one rank in master §3.1. Missing field, wrong shape, or
        a different name all return False, so the state falls back to `manifest`.
        """
        parsed, _ = self._list_json([*self.verbs.verb("explain"), pane_id])
        node = parsed
        for key in self.verbs.authority_field:
            if not isinstance(node, dict) or key not in node:
                return False
            node = node[key]
        if isinstance(node, (dict, list)):
            return False
        return str(node).strip().casefold() == self.verbs.authority_value.casefold()

    def _agent_states(self) -> dict[str, PaneStateEvent]:
        parsed, text = self._list_json(self.verbs.verb("list_agents"))
        rows = parsed if isinstance(parsed, list) else _parse_table(text)
        now = time.time()
        out: dict[str, PaneStateEvent] = {}
        for row in rows:
            pane_id = str(row.get("pane_id") or row.get("col0") or "")
            harness = str(row.get("agent") or row.get("col1") or "unknown")
            state = str(row.get("state") or row.get("col2") or "unknown").strip().lower()
            if not pane_id or state not in self.verbs.states:
                continue
            source = "gate" if self._authority_is_daisugi(pane_id) else "manifest"
            out[pane_id] = PaneStateEvent(
                session_id="",
                harness=harness,
                state=state,
                source=source,
                ts=now,
                pane=pane_id,
                detail=f"herdr agent list ({source})",
            )
        return out


def _parse_pane_id(text: str) -> str:
    """Take a pane id off `tab create` output, whatever shape it prints."""
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        if ":" in line:
            key, _, value = line.partition(":")
            if "pane" in key.casefold():
                return value.strip()
        elif " " not in line:
            return line
    return ""


def _parse_table(text: str) -> list[dict]:
    """Fallback for a verb with no `--json`: split a whitespace table into col0..colN."""
    rows: list[dict] = []
    for line in text.splitlines():
        cells = [c for c in line.replace("\t", " ").split(" ") if c]
        if not cells or cells[0].casefold() in ("pane", "pane_id", "id"):
            continue
        rows.append({f"col{i}": cell for i, cell in enumerate(cells)})
    return rows
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_herdr_backend.py -q`
Expected: PASS (13 tests; the real-herdr test skips with `herdr is not on PATH — install herdr from herdr.dev, then run herdr session list --json`)

- [ ] **Step 6: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/floor/herdr_verbs.json src/opendaisugi/floor/herdr_backend.py \
        tests/floor/test_herdr_backend.py
git commit -m "feat(floor): drive herdr panes off pinned verbs, not guessed ones

The sub-spec's `herdr pane create` does not exist in herdr's own CLI reference,
and --json is undocumented on the two list commands this backend reads most. Both
facts are pinned in herdr_verbs.json, marked unverified until a box with herdr on
it confirms them, and the backend reads the pin rather than a guess."
```

---

### Task 7: The one contract suite, parameterised over three backends

Master §7: "A backend contract is one suite, many backends. If a test cannot run against all three
pane backends it is not a contract test; it belongs to that backend's own tests."

**Files:**
- Create: `tests/floor/test_backend_contract.py`
- Test: itself

**Interfaces:**
- Consumes: `TmuxBackend` (task 4), `CoppiceBackend` (task 5), `HerdrBackend` (task 6),
  `tests/floor/hostfacts.py` (task 0).
- Produces: nothing importable. It is the gate every future backend must pass. It now covers
  `subscribe()`, `report_state()`, `read()`'s source enum, `yields_frames`, and the
  `harness` keyword, because every one of those is in master §3.2 and runs on all three.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_backend_contract.py
"""One contract, three backends. A backend that cannot pass it is not a backend.

Master §3.2: the suite SKIPS a backend whose `available()` is false and says
which host is missing. A backend that is present and fails is a suite failure.

The contract is deliberately small, because it is the part every substrate must
share: spawn something, read what it printed, watch it finish, and have it gone
from the list afterwards. Anything a single backend does better belongs in that
backend's own test file.
"""

from __future__ import annotations

import shutil
import subprocess
import time

import pytest

from opendaisugi.floor.coppice_backend import CoppiceBackend
from opendaisugi.floor.herdr_backend import HerdrBackend
from opendaisugi.floor.tmux_backend import TmuxBackend
from tests.floor import hostfacts

TMUX_SOCKET = "daisugi-test"
_READY_S = 3.0
_DONE_S = 8.0


def _make(name: str):
    if name == "tmux":
        return TmuxBackend(socket=TMUX_SOCKET)
    if name == "coppice":
        return CoppiceBackend(autostart=True)
    if name == "herdr":
        return HerdrBackend()
    raise AssertionError(f"no backend named {name}")


def _teardown(name: str) -> None:
    if name == "tmux" and shutil.which("tmux"):
        subprocess.run(
            [shutil.which("tmux"), "-L", TMUX_SOCKET, "kill-server"],
            capture_output=True,
            check=False,
        )


@pytest.fixture(params=["coppice", "herdr", "tmux"])
def backend(request):
    name = request.param
    reason = hostfacts.skip_reason(name)
    if reason:
        pytest.skip(reason)
    made = _make(name)
    if not made.available():
        _teardown(name)
        pytest.skip(f"{name} is installed but not answering — {hostfacts.host_facts()[name].fix}")
    yield made
    _teardown(name)


def _ids(backend) -> set[str]:
    return {i.ref.id for i in backend.list()}


def test_the_backend_names_itself_and_never_raises_in_available(backend):
    assert backend.name in ("coppice", "herdr", "tmux")
    assert isinstance(backend.available(), bool)


def test_spawn_read_finish_close(backend, tmp_path):
    """The whole contract in one pass, so a partial backend cannot half-pass it."""
    ref = backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo READY; sleep 2"],
        env={"COPPICE_PANE": "contract"},
        label="contract",
        kind="pty",
        harness=None,
    )
    assert ref.backend == backend.name and ref.id

    # 1. read visible shows READY within a second of the process printing it
    deadline = time.time() + _READY_S
    seen = ""
    while time.time() < deadline:
        seen = backend.read(ref, source="visible")
        if "READY" in seen:
            break
        time.sleep(0.1)
    assert "READY" in seen, f"{backend.name}: never saw READY, got {seen!r}"

    # 2. the pane is in list() while it runs
    assert ref.id in _ids(backend)

    # 3. the shell exits on its own; the pane reaches done, or leaves the list,
    #    which the contract accepts as done (spec-03 tests section)
    backend.send_text(ref, "exit")
    deadline = time.time() + _DONE_S
    finished = False
    while time.time() < deadline:
        infos = {i.ref.id: i for i in backend.list()}
        info = infos.get(ref.id)
        if info is None:
            finished = True
            break
        if info.state is not None and info.state.state == "done":
            finished = True
            break
        time.sleep(0.2)
    assert finished, f"{backend.name}: the pane never reached done and never left the list"

    # 4. close is safe on a pane that already finished, and the pane is gone after
    backend.close(ref)
    assert ref.id not in _ids(backend)


def test_read_accepts_all_three_sources(backend, tmp_path):
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "echo READY; sleep 3"], env={}, label="sources", kind="pty"
    )
    try:
        time.sleep(1.0)
        for source in ("visible", "recent", "detection"):
            assert isinstance(backend.read(ref, source=source), str)
    finally:
        backend.close(ref)


def test_send_keys_accepts_the_names_the_floor_binds(backend, tmp_path):
    ref = backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="keys", kind="pty")
    try:
        backend.send_keys(ref, ["ctrl+c"])
    finally:
        backend.close(ref)


def test_close_on_an_unknown_pane_never_raises(backend):
    from opendaisugi.floor import PaneRef

    backend.close(PaneRef(backend.name, "definitely-not-a-pane"))


def test_resize_is_accepted_by_every_backend(backend, tmp_path):
    ref = backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="sz", kind="pty")
    try:
        backend.resize(ref, 100, 30)  # tmux and herdr return None; coppice resizes
    finally:
        backend.close(ref)


def test_a_state_a_backend_reports_carries_a_real_source_and_is_never_a_bare_idle(
    backend, tmp_path
):
    """Master §3.1: `unknown` with no source, never `idle`; a manifest never says done."""
    from opendaisugi.floor.events import SOURCES, STATES

    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="state", kind="pty"
    )
    try:
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        if info.state is None:
            return  # no source is the honest answer; the roster paints `unknown`
        assert info.state.source in SOURCES
        assert info.state.state in STATES
        if info.state.source == "manifest":
            assert info.state.state != "done", "a manifest may never say done"
            assert info.state.state != "idle" or info.state.detail, (
                "an `idle` from a screen must at least name the rule that decided it"
            )
    finally:
        backend.close(ref)


def test_subscribe_reports_a_pane_exit_within_the_budget(backend, tmp_path):
    """`subscribe()` is in the §3.2 protocol and runs on all three, so it is a contract.

    A backend could pass every other test here with `subscribe()` broken, which is
    exactly how the tmux `done` defect got through the first review.
    """
    ref = backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="exit", kind="pty")
    assert ref.id in _ids(backend), f"{backend.name}: the pane exited before it was seen"
    deadline = time.time() + _DONE_S
    for event in backend.subscribe():
        if getattr(event, "pane", None) == ref.id and getattr(event, "state", "") == "done":
            assert event.source == "process", (
                f"{backend.name}: a pane exit must be a process fact, not {event.source}"
            )
            return
        if time.time() > deadline:
            break
    raise AssertionError(f"{backend.name}: subscribe() never reported the pane's exit")


def test_report_state_accepts_an_event_and_never_raises(backend, tmp_path):
    """Push authority in (§3.2). tmux drops it, herdr forwards it, coppice stores it."""
    import time as _time

    from opendaisugi.floor import PaneStateEvent

    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="report", kind="pty"
    )
    try:
        backend.report_state(
            ref,
            PaneStateEvent(
                session_id="contract",
                harness="shell",
                state="working",
                source="gate",
                ts=_time.time(),
                pane=ref.id,
                detail="verdict=allow",
            ),
        )
    finally:
        backend.close(ref)


def test_read_rejects_a_source_that_is_not_in_the_protocol(backend, tmp_path):
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="badsrc", kind="pty"
    )
    try:
        with pytest.raises(ValueError):
            backend.read(ref, source="scrollback")
    finally:
        backend.close(ref)


def test_every_backend_declares_whether_it_yields_frames(backend):
    """Master §3.2: only coppice yields frames. The floor's attach depends on knowing."""
    assert isinstance(backend.yields_frames, bool)
    assert backend.yields_frames is (backend.name == "coppice")
```

- [ ] **Step 2: Run the suite and read the skips**

Run: `uv run --no-sync pytest tests/floor/test_backend_contract.py -q -rs`
Expected: on a box with tmux only, 7 tmux tests pass and 14 skip with two named reasons
(`the coppice binary is not on PATH — build it: …` and `herdr is not on PATH — install herdr …`).
A skip that does not name a host is a defect in task 0, not here.

- [ ] **Step 3: Fix any contract failure in the backend, not in the test**

If a backend fails, change that backend. The suite is the contract; bending it to fit an
implementation is how a fail-open lands. Re-run until the suite is green or skipped.

- [ ] **Step 4: Commit**

```bash
uv run --no-sync ruff check .
git add tests/floor/test_backend_contract.py
git commit -m "test(floor): one contract suite every pane backend must pass

Three substrates, one protocol, one suite. A backend that is absent skips with
the command that installs it; a backend that is present and fails the contract
fails the build. That is the only way three implementations stay one protocol."
```

---

### Task 8: `notify.py` — the config'd command, debounced

Master §5.6 and the sub-spec's third crux: notify on block is a command, not a service. The floor
never phones home. `ntfy publish` is the documented example, and it is only an example.

**Files:**
- Create: `src/opendaisugi/floor/notify.py`
- Test: `tests/floor/test_notify.py`

**Interfaces:**
- Consumes: `PaneStateEvent` (plan 01).
- Produces:
  - `Notifier(cmd: str | None, *, debounce_s: float = 5.0, timeout_s: float = 5.0,
    clock: Callable[[], float] = time.monotonic)`
  - `Notifier.notify(ev: PaneStateEvent) -> bool` — True when the command ran.
  - `EXAMPLE_CMD: str` — the documented ntfy example, used in help text and the config comment.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_notify.py
"""notify_cmd: a command the operator configured, run with the event on stdin.

Never a service, never a relay we chose (master §5.6). Debounced per pane so a
flapping agent cannot turn one block into forty phone buzzes, and wrapped so a
broken command never takes the floor down.
"""

from __future__ import annotations

import json
import sys
import time

from opendaisugi.floor import PaneStateEvent
from opendaisugi.floor.notify import EXAMPLE_CMD, Notifier


def _ev(pane="w1:p1", state="blocked", ts=1.0) -> PaneStateEvent:
    return PaneStateEvent(
        session_id="s1", harness="claude-code", state=state, source="gate", ts=ts, pane=pane
    )


def test_no_command_means_no_run():
    assert Notifier(None).notify(_ev()) is False


def test_the_command_receives_the_event_json_on_stdin(tmp_path):
    out = tmp_path / "seen.json"
    cmd = f"{sys.executable} -c " + repr(
        f"import sys,pathlib;pathlib.Path({str(out)!r}).write_text(sys.stdin.read())"
    )
    assert Notifier(cmd).notify(_ev()) is True
    payload = json.loads(out.read_text())
    assert payload["state"] == "blocked" and payload["pane"] == "w1:p1"


def test_a_second_event_for_the_same_pane_is_debounced(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = f"{sys.executable} -c " + repr(
        f"import pathlib;p=pathlib.Path({str(counter)!r});p.write_text(p.read_text()+'x')"
    )
    clock = iter([0.0, 1.0, 6.1])
    notifier = Notifier(cmd, debounce_s=5.0, clock=lambda: next(clock))
    assert notifier.notify(_ev()) is True
    assert notifier.notify(_ev()) is False
    assert notifier.notify(_ev()) is True
    assert counter.read_text() == "xx"


def test_debounce_is_per_pane_not_global(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = f"{sys.executable} -c " + repr(
        f"import pathlib;p=pathlib.Path({str(counter)!r});p.write_text(p.read_text()+'x')"
    )
    clock = iter([0.0, 0.1])
    notifier = Notifier(cmd, debounce_s=5.0, clock=lambda: next(clock))
    assert notifier.notify(_ev(pane="w1:p1")) is True
    assert notifier.notify(_ev(pane="w1:p2")) is True
    assert counter.read_text() == "xx"


def test_a_failing_command_never_raises():
    assert Notifier("definitely-not-a-real-binary --flag").notify(_ev()) is False


def test_an_unparsable_command_string_never_raises():
    assert Notifier('sh -c "unbalanced').notify(_ev()) is False


def test_a_hanging_command_is_killed_inside_the_budget():
    started = time.monotonic()
    assert (
        Notifier(f"{sys.executable} -c 'import time;time.sleep(30)'", timeout_s=0.5).notify(_ev())
        is False
    )
    assert time.monotonic() - started < 5.0


def test_the_example_command_is_ntfy_and_reads_stdin():
    assert EXAMPLE_CMD.startswith("ntfy publish")
    assert "http" in EXAMPLE_CMD
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_notify.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.floor.notify'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/floor/notify.py
"""Run the operator's own notify command when a pane blocks.

Master §5.6: push goes through a self-hosted ntfy or not at all. So this is not a
notification service. It is one configured command, run with the event JSON on
stdin, debounced per pane. If the operator sets nothing, nothing runs and nothing
leaves the box.

Example config (`~/.opendaisugi/config.yaml`)::

    floor:
      notify_cmd: ntfy publish --title "agent blocked" https://ntfy.example.net/daisugi

Failures are silent by design. A phone that did not buzz is a smaller problem
than a cockpit that crashed while telling you your agent stopped.
"""

from __future__ import annotations

import shlex
import subprocess
import time
from collections.abc import Callable

from opendaisugi.floor import PaneStateEvent

EXAMPLE_CMD = 'ntfy publish --title "agent blocked" https://ntfy.example.net/daisugi'


class Notifier:
    """One configured command, debounced per pane."""

    def __init__(
        self,
        cmd: str | None,
        *,
        debounce_s: float = 5.0,
        timeout_s: float = 5.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.cmd = (cmd or "").strip() or None
        self.debounce_s = debounce_s
        self.timeout_s = timeout_s
        self._clock = clock
        self._last: dict[str, float] = {}

    def notify(self, ev: PaneStateEvent) -> bool:
        """Run the command with ``ev`` on stdin. True when it ran. Never raises."""
        if self.cmd is None:
            return False
        key = ev.pane or ev.session_id or ""
        now = self._clock()
        previous = self._last.get(key)
        if previous is not None and now - previous < self.debounce_s:
            return False
        try:
            argv = shlex.split(self.cmd)
        except ValueError:
            return False  # an unclosed quote in config is not worth a crash
        if not argv:
            return False
        self._last[key] = now
        try:
            subprocess.run(
                argv,
                input=ev.to_json(),
                text=True,
                capture_output=True,
                timeout=self.timeout_s,
                check=False,
            )
        except (OSError, subprocess.SubprocessError):
            return False
        return True
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_notify.py -q`
Expected: PASS (8 tests)

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/floor/notify.py tests/floor/test_notify.py
git commit -m "feat(floor): run the operator's own notify command on a block

Not a notification service and not a relay we picked. One configured command with
the event on stdin, debounced per pane so a flapping agent cannot buzz a phone
forty times, and silent on failure so a broken hook never takes the cockpit down."
```

---

### Task 9: config, swap, and the honest `floor` stage

Three constraints collide here and all three are load-bearing:

1. The sub-spec's config shape is nested (`floor: {backend, notify_cmd}`).
2. `swap.SwapKnob.field` is passed straight to `Config.model_copy(update={field: value})`.
   Pydantic accepts a literal `"floor.backend"` key and **writes nothing** — a silent no-op swap,
   exactly the dishonest control master §3.5 forbids. So `swap.py` learns dotted paths.
3. `config.py`, `swap.py`, and `modules.py` are layer modules. They must not import
   `opendaisugi.floor`. Backend detection here is `shutil.which` and `os.lstat` only.

**Files:**
- Modify: `src/opendaisugi/config.py` (add `FloorConfig` and `Config.floor` after
  `pathway_store_backend`, ≈ line 108; flatten nested models in `resolved_config`'s
  `out = [...]`, ≈ line 302)
- Modify: `src/opendaisugi/swap.py` (`STAGE_EFFECT`, ≈ line 38; `SWAP_KNOBS`, ≈ line 88;
  `selected_label` ≈ line 165 and `apply_swap` ≈ line 205)
- Modify: `src/opendaisugi/modules.py` (new `floor` stage in `detect_stages`, ≈ line 110)
- Test: `tests/floor/test_floor_config.py`

**Interfaces:**
- Consumes: nothing from earlier tasks (layer purity).
- Produces:
  - `opendaisugi.config.FloorConfig(backend: str = "auto", notify_cmd: str | None = None,
    tmux_socket: str | None = None)` and `Config.floor: FloorConfig`.
  - `opendaisugi.swap.get_field(config, dotted: str)` and
    `set_field(config, dotted: str, value) -> Config`.
  - `STAGE_EFFECT["floor"] = LIVE`; `SWAP_KNOBS["floor"]` with `field="floor.backend"`,
    `kind="setting"`, options `auto` / `coppice` / `herdr` / `tmux`.
  - `modules.detect_stages` gains a first stage `key="floor"`.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_floor_config.py
"""The floor's config knob is LIVE, so it must actually reach disk.

`floor.backend` is nested, and `Config.model_copy(update={"floor.backend": …})`
writes nothing at all. A knob marked live that silently does nothing is the exact
dishonest control master §3.5 forbids, so every assertion here reads the file back.
"""

from __future__ import annotations

import shutil

import pytest

from opendaisugi.config import Config, FloorConfig, load_config, resolved_config, save_config
from opendaisugi.modules import ACTIVE, AVAILABLE, POSSIBLE, detect_stages
from opendaisugi.swap import (
    STAGE_EFFECT,
    SWAP_KNOBS,
    apply_swap,
    effect_of,
    get_field,
    is_live,
    resolve_command,
    selected_label,
    set_field,
)


def _cfg_path(tmp_path):
    return tmp_path / "config.yaml"


def test_the_default_floor_is_auto_with_no_notify():
    floor = Config().floor
    assert isinstance(floor, FloorConfig)
    assert floor.backend == "auto"
    assert floor.notify_cmd is None
    assert floor.tmux_socket is None


def test_the_nested_shape_round_trips_through_yaml(tmp_path):
    cfg = Config(floor=FloorConfig(backend="tmux", notify_cmd="ntfy publish x"))
    save_config(cfg, _cfg_path(tmp_path))
    text = _cfg_path(tmp_path).read_text()
    assert "floor:" in text and "backend: tmux" in text
    assert load_config(_cfg_path(tmp_path)).floor.backend == "tmux"


def test_get_and_set_field_walk_a_dotted_path():
    cfg = Config()
    assert get_field(cfg, "floor.backend") == "auto"
    assert get_field(cfg, "gate_mode") == "shadow"
    updated = set_field(cfg, "floor.backend", "herdr")
    assert updated.floor.backend == "herdr"
    assert cfg.floor.backend == "auto", "set_field must not mutate the original"


def test_a_floor_swap_actually_reaches_disk(tmp_path):
    apply_swap("floor", "tmux", config_path=_cfg_path(tmp_path))
    assert load_config(_cfg_path(tmp_path)).floor.backend == "tmux"


def test_a_floor_swap_leaves_the_rest_of_the_floor_alone(tmp_path):
    save_config(
        Config(floor=FloorConfig(backend="auto", notify_cmd="ntfy publish x")), _cfg_path(tmp_path)
    )
    apply_swap("floor", "herdr", config_path=_cfg_path(tmp_path))
    floor = load_config(_cfg_path(tmp_path)).floor
    assert floor.backend == "herdr" and floor.notify_cmd == "ntfy publish x"


def test_selected_label_reads_the_nested_field(tmp_path):
    apply_swap("floor", "coppice", config_path=_cfg_path(tmp_path))
    assert selected_label(load_config(_cfg_path(tmp_path)), "floor") == "coppice"


def test_the_command_line_resolves_a_floor_swap():
    assert resolve_command("floor tmux") == ("floor", "tmux")


def test_the_floor_stage_is_live_and_covered():
    assert STAGE_EFFECT["floor"] == "live"
    assert is_live("floor") and effect_of("floor") == "live"
    assert SWAP_KNOBS["floor"].field == "floor.backend"
    assert SWAP_KNOBS["floor"].kind == "setting"


def test_every_floor_option_has_a_short_description():
    for opt in SWAP_KNOBS["floor"].options:
        assert opt.desc.strip() and len(opt.desc) <= 40
        assert opt.cost is False


def test_the_floor_stage_names_three_backends_honestly(tmp_path):
    stage = next(s for s in detect_stages(tmp_path) if s.key == "floor")
    names = [m.name for m in stage.modules]
    assert names == ["coppice", "herdr", "tmux"]
    for module in stage.modules:
        assert module.state in (ACTIVE, AVAILABLE, POSSIBLE)
        assert module.note.strip()


def test_tmux_is_available_exactly_when_it_is_on_path(tmp_path):
    stage = next(s for s in detect_stages(tmp_path) if s.key == "floor")
    tmux = next(m for m in stage.modules if m.name == "tmux")
    on_path = shutil.which("tmux") is not None
    assert (tmux.state in (ACTIVE, AVAILABLE)) is on_path


def test_the_layer_modules_never_import_the_floor_package():
    """Master §4 layer purity, checked at the source level so an import cannot creep in."""
    import inspect

    from opendaisugi import config, modules, swap

    for module in (config, swap, modules):
        source = inspect.getsource(module)
        assert "opendaisugi.floor" not in source, f"{module.__name__} imports the floor"


def test_resolved_config_flattens_the_floor_instead_of_printing_a_repr(tmp_path):
    save_config(Config(floor=FloorConfig(backend="tmux")), _cfg_path(tmp_path))
    rows = {f.key: f for f in resolved_config(_cfg_path(tmp_path))}
    assert "floor.backend" in rows and rows["floor.backend"].value == "tmux"
    assert "floor" not in rows, "a nested model must never print as a Config repr"


@pytest.mark.parametrize("bad", ["screen", "zellij", ""])
def test_an_unknown_backend_is_refused_at_the_swap(tmp_path, bad):
    with pytest.raises(ValueError):
        apply_swap("floor", bad, config_path=_cfg_path(tmp_path))
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_floor_config.py -q`
Expected: FAIL with `ImportError: cannot import name 'FloorConfig' from 'opendaisugi.config'`

- [ ] **Step 3: Add `FloorConfig` to `config.py`**

Insert after the `pathway_store_backend` field (≈ line 108), before `def default_config()`:

```python
class FloorConfig(BaseModel):
    """The shop floor: which pane backend to drive, and what to run on a block.

    ``backend`` is LIVE — the running code reads it on the next pick, with no
    restart. ``auto`` takes the first of coppice, herdr, tmux that answers. A
    named backend that is not available is refused, never downgraded.

    ``notify_cmd`` runs with the event JSON on stdin when a pane blocks. It is
    the operator's own command. openDaisugi ships no relay and no default.
    """

    backend: str = "auto"  # auto | coppice | herdr | tmux
    notify_cmd: str | None = None
    tmux_socket: str | None = None  # tmux -L NAME; None uses the default server
```

And add the field to `Config`, directly after `pathway_store_backend`:

```python
    # The floor (master §3.2). Nested because it is a group of related settings,
    # and `daisugi config` flattens it to floor.backend / floor.notify_cmd rows.
    floor: FloorConfig = Field(default_factory=FloorConfig)
```

- [ ] **Step 4: Flatten nested models in `resolved_config`**

Replace the `out = [...]` list comprehension in `resolved_config` (≈ line 320) with:

```python
    out: list[ResolvedField] = []
    for key in Config.model_fields:
        value = getattr(cfg, key)
        source = "file" if key in raw else "default"
        if isinstance(value, BaseModel):
            # A nested group prints as its leaves. Printing the model itself would
            # put a pydantic repr on a truth surface, which is not a setting.
            nested_raw = raw.get(key) or {}
            for sub_key in type(value).model_fields:
                out.append(
                    ResolvedField(
                        f"{key}.{sub_key}",
                        str(getattr(value, sub_key)),
                        "file" if sub_key in nested_raw else "default",
                    )
                )
            continue
        out.append(ResolvedField(key, str(value), source))
```

- [ ] **Step 5: Teach `swap.py` dotted paths and add the floor knob**

Add to `STAGE_EFFECT` (≈ line 38), as the first entry:

```python
    "floor": LIVE,  # the next pick reads it; no restart
```

Add to `SWAP_KNOBS`:

```python
    "floor": SwapKnob(
        "floor",
        "floor.backend",
        (
            SwapOption("auto", "auto", "first backend that answers"),
            SwapOption("coppice", "coppice", "our own server, frames and state"),
            SwapOption("herdr", "herdr", "drive herdr panes"),
            SwapOption("tmux", "tmux", "the multiplexer you already run"),
        ),
        kind="setting",
    ),
```

Add the two path helpers above `is_swappable`:

```python
def get_field(config: Any, dotted: str) -> Any:
    """Read a field by a dotted path: ``gate_mode`` or ``floor.backend``.

    ``config`` is a ``Config`` at the top level and a nested ``BaseModel`` below it,
    so the annotation is loose on purpose.
    """
    value: Any = config
    for part in dotted.split("."):
        value = getattr(value, part)
    return value


def set_field(config: Any, dotted: str, value: Any) -> Any:
    """Return a copy with the dotted field set. Nested groups are rebuilt, not shadowed.

    ``Config.model_copy(update={"floor.backend": x})`` writes a key nothing reads
    and reports success — a live knob that silently does nothing. So a dotted path
    is walked and each level copied.
    """
    head, _, rest = dotted.partition(".")
    if not rest:
        return config.model_copy(update={head: value})
    child = getattr(config, head)
    return config.model_copy(update={head: set_field(child, rest, value)})
```

Change `selected_label` to read through the helper:

```python
def selected_label(config: Config, stage_key: str) -> str | None:
    """The option label whose value matches ``config`` now, or None."""
    knob = SWAP_KNOBS[stage_key]
    val = get_field(config, knob.field)
    return next((o.label for o in knob.options if o.value == val), None)
```

And change the tail of `apply_swap` to use `set_field`:

```python
    cfg = load_config(config_path)
    updated = set_field(cfg, knob.field, opt.value)
    save_config(updated, config_path)
    return updated
```

`apply_swap` still returns a `Config`, because its top-level call always starts from one.

- [ ] **Step 6: Add the honest `floor` stage to `modules.py`**

Add near `_have` (≈ line 60):

```python
def _on_path(binary: str) -> bool:
    """Is a binary runnable from PATH? Layer-pure: no floor import, no subprocess."""
    import shutil

    return shutil.which(binary) is not None


def _coppice_socket_present() -> bool:
    """A coppice server socket owned by us, checked with lstat so a symlink cannot lie."""
    import os
    import stat

    runtime = os.environ.get("XDG_RUNTIME_DIR")
    base = Path(runtime) / "coppice" if runtime else Path.home() / ".opendaisugi" / "coppice"
    try:
        st = os.lstat(base / "server.sock")
    except OSError:
        return False
    return stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid()
```

Insert this stage as the FIRST element of the list `detect_stages` returns, before `harness`:

```python
(
    Stage(
        "floor",
        "floor (panes)",
        "runs the harness in a pane and shows its state, sourced",
        [
            Module(
                "coppice",
                ACTIVE
                if _coppice_socket_present()
                else (AVAILABLE if _on_path("coppice") else POSSIBLE),
                "server running"
                if _coppice_socket_present()
                else (
                    "built, run `coppice server start`"
                    if _on_path("coppice")
                    else "build it in harness/coppice"
                ),
            ),
            Module(
                "herdr",
                AVAILABLE if _on_path("herdr") else POSSIBLE,
                "installed" if _on_path("herdr") else "install herdr from herdr.dev",
            ),
            Module(
                "tmux",
                AVAILABLE if _on_path("tmux") else POSSIBLE,
                "installed" if _on_path("tmux") else "install tmux 3.2 or newer",
            ),
        ],
    ),
)
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/floor/test_floor_config.py tests/test_swap.py tests/test_modules.py tests/test_config.py tests/test_config_resolved.py tests/test_dashboard.py -q`
Expected: PASS. `tests/test_swap.py::test_effect_is_three_valued_and_covers_every_stage` proves
`STAGE_EFFECT` still covers every stage exactly, and
`test_every_module_knob_option_names_a_real_module_in_the_map` skips the floor knob because it is
`kind="setting"`.

- [ ] **Step 8: Run the whole suite once, because config is shared**

Run: `uv run --no-sync pytest -q`
Expected: green. A failure here is a `resolved_config` consumer that assumed one row per field;
fix the consumer, not the flattening.

- [ ] **Step 9: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/config.py src/opendaisugi/swap.py src/opendaisugi/modules.py \
        tests/floor/test_floor_config.py
git commit -m "feat(floor): a live backend knob that actually reaches disk

model_copy takes a literal 'floor.backend' key, writes nothing, and reports
success — a live control that does nothing is the lie the honesty tags exist to
prevent. swap.py now walks dotted paths and rebuilds the nested group, the config
surface flattens it to leaves instead of printing a pydantic repr, and the module
map carries the three backends with the command that installs each."
```

---

### Task 10: `tui_grid.py` — paint a Frame

**Files:**
- Create: `src/opendaisugi/tui_grid.py`
- Test: `tests/floor/test_tui_grid.py`

**Interfaces:**
- Consumes: `Frame` (plan 01).
- Produces:
  - `cell_style(fg: str, bg: str, attrs: int) -> Style` — Rich style for one cell.
  - `ATTR_BOLD = 1`, `ATTR_ITALIC = 2`, `ATTR_UNDERLINE = 4`, `ATTR_REVERSE = 8`.
  - `GridWidget(Static)` with `cols`, `rows`, `apply_frame(frame: Frame) -> None`,
    `render_lines_text() -> list[str]` (the plain text of the current grid, for tests),
    `class FrameArrived(Message)` carrying `frame: Frame`, and
    `on_resize` sending `pane.resize` through the callback given at construction:
    `GridWidget(*, on_resize: Callable[[int, int], None] | None = None,
    repaint_interval_s: float = 0.016)`.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_tui_grid.py
"""GridWidget: a Frame becomes styled rows, and a diff only touches its rows.

Frames arrive on a worker thread and are posted as messages, so the paint path is
pure: `apply_frame` mutates the grid and marks it dirty; the repaint is on a timer.
That is what makes 60 Hz frames survivable in a Textual app.
"""

from __future__ import annotations

import pytest

pytest.importorskip("textual")

from opendaisugi.floor import Frame  # noqa: E402
from opendaisugi.tui_grid import (  # noqa: E402
    ATTR_BOLD,
    ATTR_REVERSE,
    GridWidget,
    cell_style,
)


def _frame(seq, rows_changed, *, cols=10, rows=3, cursor=(0, 0)) -> Frame:
    return Frame(
        pane="w1:p1", seq=seq, cols=cols, rows=rows, cursor=cursor, rows_changed=rows_changed
    )


def test_cell_style_maps_colors_and_attributes():
    style = cell_style("#ff0000", "#000000", ATTR_BOLD | ATTR_REVERSE)
    assert style.bold is True
    assert style.reverse is True
    assert style.color.triplet is not None


def test_an_unknown_color_never_raises():
    style = cell_style("not-a-color", "", 0)
    assert style is not None


def test_the_first_full_frame_paints_every_row():
    grid = GridWidget()
    grid.apply_frame(
        _frame(
            1,
            {
                0: [["ab", "white", "black", 0]],
                1: [["cd", "white", "black", 0]],
                2: [["ef", "white", "black", 0]],
            },
        )
    )
    assert grid.render_lines_text() == ["ab", "cd", "ef"]


def test_a_diff_touches_only_its_rows():
    grid = GridWidget()
    grid.apply_frame(
        _frame(
            1,
            {
                0: [["aa", "white", "black", 0]],
                1: [["bb", "white", "black", 0]],
                2: [["cc", "white", "black", 0]],
            },
        )
    )
    grid.apply_frame(_frame(2, {1: [["ZZ", "white", "black", 0]]}))
    assert grid.render_lines_text() == ["aa", "ZZ", "cc"]


def test_a_resize_grows_the_grid_and_keeps_what_fits():
    grid = GridWidget()
    grid.apply_frame(_frame(1, {0: [["aa", "white", "black", 0]]}, rows=1))
    grid.apply_frame(_frame(2, {2: [["cc", "white", "black", 0]]}, rows=3))
    assert grid.render_lines_text() == ["aa", "", "cc"]


def test_an_out_of_range_row_is_dropped_not_a_crash():
    grid = GridWidget()
    grid.apply_frame(_frame(1, {99: [["boom", "white", "black", 0]]}, rows=3))
    assert grid.render_lines_text() == ["", "", ""]


def test_a_stale_seq_is_ignored():
    grid = GridWidget()
    grid.apply_frame(_frame(5, {0: [["new", "white", "black", 0]]}))
    grid.apply_frame(_frame(4, {0: [["old", "white", "black", 0]]}))
    assert grid.render_lines_text()[0] == "new"


def test_the_cursor_cell_is_drawn_reversed():
    grid = GridWidget()
    grid.apply_frame(_frame(1, {0: [["ab", "white", "black", 0]]}, cursor=(1, 0)))
    text = grid.render_text()
    spans = [s for s in text.spans if s.start == 1 and s.end == 2]
    assert spans and spans[0].style.reverse is True


def test_a_resize_event_reports_the_new_size_once():
    seen: list[tuple[int, int]] = []
    grid = GridWidget(on_resize=lambda cols, rows: seen.append((cols, rows)))
    grid.report_size(100, 30)
    grid.report_size(100, 30)
    grid.report_size(120, 40)
    assert seen == [(100, 30), (120, 40)]
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_tui_grid.py -q`
Expected: FAIL with `ModuleNotFoundError: No module named 'opendaisugi.tui_grid'`

- [ ] **Step 3: Write the implementation**

```python
# src/opendaisugi/tui_grid.py
"""Paint a coppice Frame as Rich text inside a Textual widget.

Frames arrive on a worker thread at up to 60 Hz per pane. Repainting on every one
would starve the event loop, so `apply_frame` only mutates the grid and marks it
dirty; the widget repaints on a 16 ms timer. That split is what makes a live
terminal survivable inside a Textual app.

A frame carries only the rows that changed (master §3.2). The widget keeps the
whole grid so a diff is a row assignment, not a re-render of the pane.
"""

from __future__ import annotations

from collections.abc import Callable

from rich.style import Style
from rich.text import Text
from textual.message import Message
from textual.widgets import Static

from opendaisugi.floor import Frame

ATTR_BOLD = 1
ATTR_ITALIC = 2
ATTR_UNDERLINE = 4
ATTR_REVERSE = 8

_REPAINT_S = 0.016  # 60 Hz ceiling


def cell_style(fg: str, bg: str, attrs: int) -> Style:
    """One cell's Rich style. An unusable color is dropped, never raised."""
    try:
        base = Style(color=fg or None, bgcolor=bg or None)
    except Exception:  # noqa: BLE001 — a bad color must not stop the paint
        base = Style()
    return base + Style(
        bold=bool(attrs & ATTR_BOLD),
        italic=bool(attrs & ATTR_ITALIC),
        underline=bool(attrs & ATTR_UNDERLINE),
        reverse=bool(attrs & ATTR_REVERSE),
    )


class GridWidget(Static):
    """The selected pane's grid. Holds every row; a frame updates the changed ones."""

    class FrameArrived(Message):
        """Posted from the subscribe worker so the paint happens on the app thread."""

        def __init__(self, frame: Frame) -> None:
            super().__init__()
            self.frame = frame

    def __init__(
        self,
        *,
        on_resize: Callable[[int, int], None] | None = None,
        repaint_interval_s: float = _REPAINT_S,
        **kwargs,
    ) -> None:
        super().__init__("", **kwargs)
        self.cols = 0
        self.rows = 0
        self._grid: list[list[list]] = []
        self._cursor: tuple[int, int] = (0, 0)
        self._seq = -1
        self._dirty = False
        self._on_resize = on_resize
        self._repaint_s = repaint_interval_s
        self._last_size: tuple[int, int] | None = None

    def on_mount(self) -> None:
        self.set_interval(self._repaint_s, self._repaint)

    # --- data -------------------------------------------------------------
    def apply_frame(self, frame: Frame) -> None:
        """Merge one frame. Out-of-order and out-of-range rows are dropped."""
        if frame.seq < self._seq:
            return
        self._seq = frame.seq
        if (frame.cols, frame.rows) != (self.cols, self.rows):
            self.cols, self.rows = frame.cols, frame.rows
            grid: list[list[list]] = [[] for _ in range(self.rows)]
            for index, row in enumerate(self._grid[: self.rows]):
                grid[index] = row
            self._grid = grid
        self._cursor = frame.cursor
        for index, cells in frame.rows_changed.items():
            if 0 <= int(index) < self.rows:
                self._grid[int(index)] = list(cells)
        self._dirty = True

    def on_grid_widget_frame_arrived(self, event: "GridWidget.FrameArrived") -> None:
        self.apply_frame(event.frame)

    def report_size(self, cols: int, rows: int) -> None:
        """Tell the server the pane's new size, once per real change."""
        if self._last_size == (cols, rows):
            return
        self._last_size = (cols, rows)
        if self._on_resize is not None:
            self._on_resize(cols, rows)

    def on_resize(self, event) -> None:
        self.report_size(event.size.width, event.size.height)

    # --- paint ------------------------------------------------------------
    def render_text(self) -> Text:
        """The whole grid as one Rich Text, cursor drawn as reverse video."""
        text = Text()
        cursor_x, cursor_y = self._cursor
        for row_index, row in enumerate(self._grid):
            offset = len(text.plain)
            for cell in row:
                content = str(cell[0]) if cell else ""
                fg = str(cell[1]) if len(cell) > 1 else ""
                bg = str(cell[2]) if len(cell) > 2 else ""
                attrs = int(cell[3]) if len(cell) > 3 else 0
                text.append(content, cell_style(fg, bg, attrs))
            if row_index == cursor_y:
                start = offset + cursor_x
                if start < len(text.plain):
                    text.stylize(Style(reverse=True), start, start + 1)
            if row_index < len(self._grid) - 1:
                text.append("\n")
        return text

    def render_lines_text(self) -> list[str]:
        """The grid as plain strings, one per row. Used by tests and by `read`."""
        return self.render_text().plain.split("\n") if self._grid else []

    def _repaint(self) -> None:
        if not self._dirty:
            return
        self._dirty = False
        self.update(self.render_text())
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `uv run --no-sync pytest tests/floor/test_tui_grid.py -q`
Expected: PASS (9 tests; the whole file skips when the `[tui]` extra is absent)

- [ ] **Step 5: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/tui_grid.py tests/floor/test_tui_grid.py
git commit -m "feat(cockpit): paint a coppice frame, coalesced at 60 Hz

A pane can push sixty frames a second. Painting each one starves the event loop,
so a frame only mutates the grid and marks it dirty, and a timer does the paint.
Diffs assign rows, so a busy pane costs one row copy, not a re-render."
```

---

### Task 11: `tui_floor.py` — the floor screen, and attach that does not eat keys

Two design facts drive this task:

- `DaisugiApp.BINDINGS` binds `tab` with `priority=True`, plus `q`, `colon`, and
  `question_mark`. Priority bindings fire **before** the focused screen's, and
  `App._dispatch_action` treats "the action method exists and was invoked" as handled
  whatever it returns. **Returning early is not enough** — reproduced on textual 8.2.8: a
  guarded `action_cycle` that does `return` still swallows `tab`, and `AttachScreen.on_key`
  never sees it. Only `raise SkipAction()` makes the dispatcher report the key unhandled and
  fall through to the screen. So every guarded action raises `SkipAction`, and the pilot test
  asserts `tab` reaches the fake backend.
- Allow and deny reuse `tui_sessions.py`'s ask actions unchanged, including the typed-token
  strong guard for a destructive ask. "Unchanged" means the code moves into a mixin both screens
  inherit, not that it is copied.

**Files:**
- Create: `src/opendaisugi/tui_asks.py`
- Create: `src/opendaisugi/tui_floor.py`
- Modify: `src/opendaisugi/tui_sessions.py` (inherit the mixin; move the ask-action bodies
  out of ≈ lines 223–320 and the `allow_token` branch at ≈ lines 420–433)
- Modify: `src/opendaisugi/tui.py` (`SCREENS` ≈ line 44, `_ORDER` ≈ line 41, and the four
  passthrough guards ≈ lines 100–130)
- Test: `tests/test_tui_floor.py`

**Interfaces:**
- Consumes: `registry.pick_backend` / `prompt_pane` (task 3), `Notifier` (task 8),
  `GridWidget` and `GridWidget.FrameArrived` (task 10), `cockpit.build_roster` / `SessionRow`,
  `PaneInfo` / `PaneStateEvent` / `Frame`, and each backend's `yields_frames` flag
  (tasks 4, 5, 6).
- Produces:
  - `opendaisugi.tui_asks.AskActionsMixin` with `_ask_origin: str`, `selected_row` (abstract
    property supplied by the screen), `_pending()`, `_ask_action_text(row)`,
    `_needs_strong_guard(row)`,
    `action_arm_allow()`, `action_confirm_allow()`, `action_deny()`,
    `handle_allow_token(mode, value) -> bool`.
  - `opendaisugi.tui_floor.FloorScreen(AskActionsMixin, CockpitScreen)` with
    `name="floor"`, `_ask_origin = "floor view"`, `backend`, `panes: list[PaneInfo]`,
    `selected_pane`, `action_create/prompt/steer/deny/close_pane/attach/next_pane/prev_pane/
    read_recent`, `on_pane_state(ev)`, and the worker plumbing:
    `start_pump()` / `stop_pump()` / `_pump()` / `post_frame(frame)` /
    `reload_soon()` / `_poll()` / `_apply_rows(infos, error)` / `_paint_rows()`.
  - `opendaisugi.tui_floor.AttachScreen(ModalScreen)` with `passthrough = True`,
    `LEADER = "ctrl+a"`.

- [ ] **Step 1: Write the failing test**

```python
# tests/test_tui_floor.py
"""The floor screen: the keys do what the footer ramp says, and attach passes keys through.

The App binds tab with priority=True, plus q, colon and question_mark. In attach
those are keys the harness needs. The passthrough tests are the ones that matter:
a floor that eats Tab inside Claude Code is not a floor.
"""

from __future__ import annotations

import asyncio
import time

import pytest

pytest.importorskip("textual")

from opendaisugi.floor import PaneInfo, PaneRef, PaneStateEvent  # noqa: E402
from opendaisugi.tui import DaisugiApp  # noqa: E402


class FakeBackend:
    name = "fake"
    yields_frames = True  # so attach is allowed; a text-only fake sets this False

    def __init__(self, events=None):
        self.spawned: list[dict] = []
        self.sent: list[tuple[str, str]] = []
        self.keys: list[tuple[str, list[str]]] = []
        self.closed: list[str] = []
        self.reads: list[tuple[str, str]] = []
        self._state = "working"
        self._events = list(events or [])

    def available(self):
        return True

    def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
        self.spawned.append(
            {"cwd": str(cwd), "cmd": cmd, "label": label, "kind": kind, "harness": harness}
        )
        return PaneRef("fake", f"p{len(self.spawned)}")

    def list(self):
        ev = PaneStateEvent(
            session_id="s1",
            harness="claude-code",
            state=self._state,
            source="gate",
            ts=time.time(),
            pane="p1",
        )
        return [
            PaneInfo(
                ref=PaneRef("fake", "p1"),
                label="auth fix",
                cwd="/repo",
                cmd=["claude"],
                kind="pty",
                state=ev,
            )
        ]

    def send_text(self, pane, text, *, enter=True):
        self.sent.append((pane.id, text))

    def send_keys(self, pane, keys):
        self.keys.append((pane.id, list(keys)))

    def read(self, pane, *, source="visible"):
        self.reads.append((pane.id, source))
        return f"[{source}]"

    def resize(self, pane, cols, rows):
        return None

    def close(self, pane):
        self.closed.append(pane.id)

    def report_state(self, pane, ev):
        return None

    def subscribe(self):
        """A blocking generator, exactly like a real backend's.

        It yields the canned events, then blocks, so the pump's cancellation path is
        exercised the way it is in production rather than by a generator that ends.
        """
        yield from self._events
        while True:
            time.sleep(0.01)


class TextOnlyBackend(FakeBackend):
    """A backend that sends text, not frames — tmux and herdr."""

    name = "tmux"
    yields_frames = False


async def _floor(pilot, backend):
    await pilot.pause()
    pilot.app.switch_screen("floor")
    await pilot.pause()
    pilot.app.screen.stop_pump()
    pilot.app.screen.backend = backend
    pilot.app.screen.reload()
    pilot.app.screen.start_pump()
    await pilot.pause()


def _run(scenario):
    asyncio.run(scenario())


def test_the_roster_lists_every_pane_with_its_state_and_source(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            table = app.screen.query_one("#panes")
            assert table.row_count == 1
            cells = [str(c) for c in table.get_row_at(0)]
            assert "p1" in cells[0] and "auth fix" in cells[1]
            assert "working" in " ".join(cells) and "gate" in " ".join(cells)

    _run(scenario)


def test_c_opens_the_command_line_prefilled_with_create(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("c")
            box = app.screen.query_one("#cmd")
            assert box.display is True
            assert box.value.startswith("create --cwd /repo -- claude")

    _run(scenario)


def test_the_create_command_reaches_the_backend(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            app.screen.on_cmd_submitted("create --cwd /repo --label fix -- claude")
            await pilot.pause()
            assert backend.spawned == [
                {"cwd": "/repo", "cmd": ["claude"], "label": "fix", "kind": "pty", "harness": None}
            ]

    _run(scenario)


def test_p_prompts_the_selected_pane(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("p")
            app.screen.on_cmd_submitted("fix the failing test")
            await pilot.pause()
            assert backend.sent == [("p1", "fix the failing test")]

    _run(scenario)


def test_r_reads_the_recent_source(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("r")
            assert ("p1", "recent") in backend.reads

    _run(scenario)


def test_x_closes_the_selected_pane(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("x")
            assert backend.closed == ["p1"]

    _run(scenario)


def test_a_on_a_pane_with_no_ask_says_so_and_allows_nothing(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("a")
            assert "no pending ask" in app.status_text

    _run(scenario)


def test_enter_attaches_and_the_leader_returns(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            assert app.screen.__class__.__name__ == "AttachScreen"
            await pilot.press("ctrl+a")
            assert app.screen.name == "floor"

    _run(scenario)


def test_attach_passes_tab_to_the_harness(tmp_path):
    """The regression this blocker was.

    `tab` is bound on the App with priority=True, so it is dispatched BEFORE the
    screen. Textual counts an action as handled the moment the method runs, whatever
    it returns, so a guard that merely returns still eats the key. Only
    `raise SkipAction()` lets it through. Reproduced both ways on textual 8.2.8.
    """

    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            assert app.screen.__class__.__name__ == "AttachScreen"
            await pilot.press("tab")
            sent = [k for _, keys in backend.keys for k in keys]
            assert "tab" in sent, "the app swallowed Tab; the guard must raise SkipAction"
            assert app.screen.__class__.__name__ == "AttachScreen", "the app switched views"

    _run(scenario)


def test_attach_passes_q_colon_and_question_mark_to_the_harness(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            await pilot.press("q")
            await pilot.press("colon")
            await pilot.press("question_mark")
            assert app.is_running, "q quit the app while attached"
            assert app.screen.__class__.__name__ == "AttachScreen"
            sent = [k for _, keys in backend.keys for k in keys]
            assert "q" in sent

    _run(scenario)


def test_leaving_attach_restores_the_apps_own_keys(tmp_path):
    """The guard is scoped to the attach screen, not a global mute."""

    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            await pilot.press("ctrl+a")
            assert app.screen.name == "floor"
            await pilot.press("tab")
            assert app.screen.name != "floor", "Tab stopped cycling views after attach"

    _run(scenario)


def test_attach_sends_printable_keys_through(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            await pilot.press("h", "i")
            sent = [k for _, keys in backend.keys for k in keys]
            assert sent[-2:] == ["h", "i"]

    _run(scenario)


def test_the_pump_paints_a_frame_the_backend_yields(tmp_path):
    """The regression this blocker was: nothing called subscribe(), so the grid was blank."""

    async def scenario():
        from opendaisugi.floor import Frame

        frame = Frame(
            pane="p1",
            seq=1,
            cols=4,
            rows=2,
            cursor=(0, 0),
            rows_changed={0: [["LIVE", "white", "black", 0]], 1: [["", "white", "black", 0]]},
        )
        backend = FakeBackend(events=[frame])
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            for _ in range(50):
                await pilot.pause()
                if app.screen.query_one("#grid").render_lines_text()[:1] == ["LIVE"]:
                    break
            assert app.screen.query_one("#grid").render_lines_text()[0] == "LIVE"

    _run(scenario)


def test_the_pump_delivers_a_blocked_event_end_to_end(tmp_path):
    """A `blocked` event off the backend's own generator rings the bell and notifies."""

    async def scenario():
        import sys

        from opendaisugi.floor.notify import Notifier

        counter = tmp_path / "count"
        counter.write_text("")
        cmd = f"{sys.executable} -c " + repr(
            f"import pathlib;p=pathlib.Path({str(counter)!r});p.write_text(p.read_text()+'x')"
        )
        ev = PaneStateEvent(
            session_id="s1",
            harness="claude-code",
            state="blocked",
            source="gate",
            ts=time.time(),
            pane="p1",
            detail="Bash rm -rf",
        )
        backend = FakeBackend(events=[ev])
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        bells: list[int] = []
        async with app.run_test() as pilot:
            app.bell = lambda: bells.append(1)
            await _floor(pilot, backend)
            app.screen.notifier = Notifier(cmd, debounce_s=5.0)
            for _ in range(50):
                await pilot.pause()
                if bells:
                    break
            await app.workers.wait_for_complete()
            assert bells == [1], "the bell did not ring exactly once"
            assert counter.read_text() == "x", "notify_cmd did not run exactly once"
            assert "blocked" in app.status_text

    _run(scenario)


def test_a_frame_for_another_pane_never_paints_the_selected_one(tmp_path):
    async def scenario():
        from opendaisugi.floor import Frame

        other = Frame(
            pane="p9",
            seq=1,
            cols=4,
            rows=1,
            cursor=(0, 0),
            rows_changed={0: [["NOPE", "white", "black", 0]]},
        )
        backend = FakeBackend(events=[other])
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            for _ in range(20):
                await pilot.pause()
            assert "NOPE" not in "".join(app.screen.query_one("#grid").render_lines_text())

    _run(scenario)


def test_unmounting_the_screen_cancels_the_pump(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            assert app.screen._pump_worker is not None
            floor = app.screen
            app.switch_screen("sessions")
            await pilot.pause()
            assert floor._pump_worker is None

    _run(scenario)


def test_attach_on_a_text_only_backend_refuses_and_names_its_own_command(tmp_path):
    """A blank grid that swallows every key is worse than an honest refusal."""

    async def scenario():
        backend = TextOnlyBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("enter")
            assert app.screen.name == "floor", "attach opened on a backend with no frames"
            assert "tmux attach" in app.status_text

    _run(scenario)


def test_the_roster_poll_runs_off_the_ui_thread(tmp_path):
    """`reload_soon` must hand `backend.list()` to a worker, not the event loop."""

    async def scenario():
        import threading

        seen: list[int] = []

        class SlowBackend(FakeBackend):
            def list(self):
                seen.append(threading.get_ident())
                return super().list()

        backend = SlowBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            main = threading.get_ident()
            seen.clear()
            app.screen.reload_soon()
            await app.workers.wait_for_complete()
            await pilot.pause()
            assert seen and all(ident != main for ident in seen)

    _run(scenario)


def test_the_screen_hands_every_blocked_event_to_the_notifier(tmp_path):
    """The screen does not debounce. Notifier owns that, and owns it in one place."""

    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            runs: list[str] = []
            app.screen.notifier = type(
                "N", (), {"notify": lambda self, ev: runs.append(ev.pane) or True}
            )()
            ev = PaneStateEvent(
                session_id="s1",
                harness="claude-code",
                state="blocked",
                source="gate",
                ts=time.time(),
                pane="p1",
            )
            app.screen.on_pane_state(ev)
            app.screen.on_pane_state(ev)
            await app.workers.wait_for_complete()
            await pilot.pause()
            assert runs == ["p1", "p1"]

    _run(scenario)


def test_a_blocked_event_runs_notify_cmd_exactly_once_within_five_seconds(tmp_path):
    """Spec-03: one `notify_cmd` run per pane per five seconds, end to end."""

    async def scenario():
        import sys

        from opendaisugi.floor.notify import Notifier

        counter = tmp_path / "count"
        counter.write_text("")
        cmd = f"{sys.executable} -c " + repr(
            f"import pathlib;p=pathlib.Path({str(counter)!r});p.write_text(p.read_text()+'x')"
        )
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            app.screen.notifier = Notifier(cmd, debounce_s=5.0)
            ev = PaneStateEvent(
                session_id="s1",
                harness="claude-code",
                state="blocked",
                source="gate",
                ts=time.time(),
                pane="p1",
            )
            for _ in range(4):
                app.screen.on_pane_state(ev)
            await app.workers.wait_for_complete()
            await pilot.pause()
            assert counter.read_text() == "x"

    _run(scenario)


def test_a_working_event_never_notifies(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            runs: list[str] = []
            app.screen.notifier = type(
                "N", (), {"notify": lambda self, ev: runs.append(ev.pane) or True}
            )()
            app.screen.on_pane_state(
                PaneStateEvent(
                    session_id="s1",
                    harness="claude-code",
                    state="working",
                    source="gate",
                    ts=time.time(),
                    pane="p1",
                )
            )
            await app.workers.wait_for_complete()
            await pilot.pause()
            assert runs == []

    _run(scenario)


def test_the_footer_ramp_names_every_bound_key(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            shown = {b.key for b in app.screen.BINDINGS if getattr(b, "show", True)}
            for key in ("c", "p", "s", "a", "d", "x", "enter", "n", "N", "r"):
                assert key in shown, f"{key} is bound but not on the ramp"

    _run(scenario)


def test_steer_on_a_non_sprig_pane_says_so_instead_of_pretending(tmp_path):
    async def scenario():
        backend = FakeBackend()
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await _floor(pilot, backend)
            await pilot.press("s")
            assert "not on this path" in app.status_text

    _run(scenario)


def test_the_existing_sessions_screen_still_answers_asks(tmp_path):
    """The mixin move must leave tui_sessions.py's behaviour exactly as it was."""

    async def scenario():
        app = DaisugiApp(data_dir=tmp_path, interval=999)
        async with app.run_test() as pilot:
            await pilot.pause()
            assert hasattr(app.screen, "action_arm_allow")
            assert hasattr(app.screen, "_needs_strong_guard")

    _run(scenario)
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/test_tui_floor.py -q`
Expected: FAIL with `ScreenError`/`KeyError: 'floor'` — the App has no floor screen.

- [ ] **Step 3: Move the ask actions into a mixin**

Create `src/opendaisugi/tui_asks.py`, moving the bodies **verbatim** out of
`tui_sessions.py` (`_pending`, `_ask_action_text`, `_needs_strong_guard`, `action_arm_allow`,
`action_deny`, and the `allow_token` branch of `on_cmd_submitted`):

```python
# src/opendaisugi/tui_asks.py
"""Answering a pending ask, shared by the sessions screen and the floor screen.

The behaviour is the sessions screen's, unchanged: a low-blast allow is the
two-step (arm, then Enter, disarmed by any cursor move); a high-blast one needs
the session id typed back. It moved here so the floor screen inherits it rather
than growing a second, drifting copy of a safety guard.

A screen using this mixin supplies `selected_row -> SessionRow | None`, owns
`self._armed` and `self._cmd_mode`, and sets `_ask_origin` to the phrase that lands in
the audit reason ("sessions view", "floor view"). The reason string is a safety-audit
record, so it keeps the wording each screen already wrote rather than drifting to one
generic sentence.
"""

from __future__ import annotations

from opendaisugi.cockpit import SHELL_TOOL_NAMES, SessionRow, is_destructive_action


class AskActionsMixin:
    """`a` / `d` / the typed-token confirm, for any screen with a selected row."""

    _armed: str | None
    _cmd_mode: tuple | None
    _ask_origin: str = "cockpit"

    @property
    def selected_row(self) -> SessionRow | None:  # pragma: no cover - supplied by the screen
        raise NotImplementedError

    def _pending(self) -> tuple[SessionRow, str] | None:
        row = self.selected_row
        if row is None or not row.pending_ask:
            self.app.set_status("no pending ask on this row")
            return None
        return row, str(row.pending_ask["toolUseId"])

    def _ask_action_text(self, row: SessionRow) -> str:
        """S2: build the string we classify from the PENDING ask (toolName + the
        values of toolInput), not from ``row.action`` — the latter is the last
        *recorded* call and can lag the ask still awaiting an answer, so a
        destructive pending call would otherwise get the low-effort two-step."""
        a = row.pending_ask or {}
        parts = [str(a.get("toolName") or "")]
        ti = a.get("toolInput")
        if isinstance(ti, dict):
            parts += [str(v) for v in ti.values()]
        return " ".join(p for p in parts if p).strip() or row.action

    def _needs_strong_guard(self, row: SessionRow) -> bool:
        """True when this allow must use the typed-token confirm, not a->Enter.

        High-blast is decided at the point of use, not by chasing every command
        string. It is high-blast when ANY of:
        - the tool is a RAW SHELL tool (SHELL_TOOL_NAMES) — its blast radius is
          "whatever the string does", so dd/mkfs/chmod -R/git clean/find -delete/
          a truncating redirect/… all land here whether or not a pattern catches
          them;
        - ``toolInput`` is not a clean dict — we can't inspect the call, so we
          can't call it low-blast (this also closes the minor fail-open where a
          non-dict input was classified on ``toolName`` alone);
        - ``is_destructive_action`` matches the ask's text (defence in depth, and
          it covers a destructive command issued through a NON-shell tool)."""
        a = row.pending_ask or {}
        tool = str(a.get("toolName") or "").strip().casefold()
        if tool in SHELL_TOOL_NAMES:
            return True
        if not isinstance(a.get("toolInput"), dict):
            return True
        return is_destructive_action(self._ask_action_text(row))

    def action_arm_allow(self) -> None:
        p = self._pending()
        if not p:
            return
        row, tid = p
        if self._needs_strong_guard(row):
            # S2 (habituation-resistant): a fixed a->Enter would go invisible on a
            # high-blast would-deny. Match the guard to the blast radius — require
            # a token that VARIES per row (the session id, not the near-constant
            # tool name "Bash") typed to confirm, so the operator's locus of
            # attention lands on THIS specific action and the reflex can't allow.
            token = row.session_id
            self._armed = None
            self._cmd_mode = ("allow_token", tid, token)
            self.app.open_cmd(prefill="")
            self.app.set_status(
                f"HIGH-BLAST allow — type the session id '{token}' then ⏎ to confirm allow of {tid}"
            )
        else:
            # Low-blast: the low-effort two-step. a arms, ⏎ confirms, any cursor
            # move disarms (on_data_table_row_highlighted).
            self._armed = tid
            self._cmd_mode = None
            self.app.set_status(f"allow {tid}? ⏎ to confirm allow · move the cursor to cancel")

    def action_confirm_allow(self) -> bool:
        """Answer an armed allow. True when it fired, False when nothing was armed."""
        from opendaisugi import ask

        # S2 (misfire-safe): the armed id must still equal the CURRENTLY selected
        # row's toolUseId — a poll re-sort can never misfire onto another row.
        row = self.selected_row
        if (
            self._armed
            and row
            and row.pending_ask
            and str(row.pending_ask["toolUseId"]) == self._armed
        ):
            ask.answer(
                self.app.data_dir / "gate",
                tool_use_id=self._armed,
                decision="allow",
                reason=f"operator allowed from the {self._ask_origin}",
            )
            self.app.set_status(f"allowed {self._armed}")
            self._armed = None
            self.reload()
            return True
        self._armed = None
        return False

    def action_deny(self) -> None:
        p = self._pending()
        if not p:
            return
        from opendaisugi import ask

        _, tid = p
        ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=tid,
            decision="deny",
            reason=f"operator denied from the {self._ask_origin}",
        )
        self.app.set_status(f"denied {tid}")
        self.reload()

    def handle_allow_token(self, mode: tuple, value: str) -> bool:
        """The `allow_token` branch of a `#cmd` submit. True when consumed."""
        from opendaisugi import ask

        _, tid, token = mode
        row = self.selected_row
        if (
            row is None
            or not row.pending_ask
            or str(row.pending_ask["toolUseId"]) != tid
            or value.strip() != token
        ):
            self.app.set_status(f"allow cancelled — token did not match '{token}'")
            return True
        ask.answer(
            self.app.data_dir / "gate",
            tool_use_id=tid,
            decision="allow",
            reason="operator allowed a destructive action (typed-token confirm)",
        )
        self.app.set_status(f"allowed {tid} (destructive · token confirmed)")
        self.reload()
        return True
```

In `tui_sessions.py`: change the class line to
`class SessionsScreen(AskActionsMixin, CockpitScreen):`, set
`_ask_origin = "sessions view"` on the class so the audit reason still reads
"operator allowed from the sessions view" exactly as it does today, add
`from opendaisugi.tui_asks import AskActionsMixin`, delete the five moved methods, replace
`action_confirm`'s allow branch with `if self.action_confirm_allow(): return` followed by
`self.action_attach()`, and replace the `allow_token` branch of `on_cmd_submitted` with
`if kind == "allow_token": return self.handle_allow_token(mode, value)`.

- [ ] **Step 4: Write the floor screen, including the worker pump**

Three things in this file are easy to leave out and each one makes the floor a lie:
`start_pump` (without it nothing calls `subscribe()`, so the grid never paints, the bell
never rings and a configured `notify_cmd` never runs), `reload_soon` (without it a tmux poll
runs `list-panes` plus a `capture-pane` per pane on the UI thread every second), and the
`yields_frames` check in `action_attach` (without it attach on tmux is a blank screen that
swallows every key).

```python
# src/opendaisugi/tui_floor.py
"""The floor: every pane, its sourced state, the selected pane's grid, and the ramp.

Left, a roster of panes. Right top, the grid. Right bottom, the peek: the last
verdict, the pending ask, the detail. The screen holds nothing the backend does
not: closing the cockpit loses no state (spec-03 crux 2).

Attach is a modal screen marked `passthrough`, and the App's four priority
bindings (tab, q, colon, question_mark) no-op while it is up. Without that the
cockpit would eat the exact keys Claude Code needs, and the operator could not
live inside their harness. `ctrl+a` is the one intercepted chord.
"""

from __future__ import annotations

import shlex
import time
from functools import partial
from pathlib import Path

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.screen import ModalScreen
from textual.widgets import DataTable, Static
from textual.worker import get_current_worker

from opendaisugi.cockpit import SessionRow, build_roster
from opendaisugi.config import load_config
from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor import Frame, PaneInfo, PaneStateEvent
from opendaisugi.floor.notify import Notifier
from opendaisugi.floor.registry import pick_backend, prompt_pane
from opendaisugi.tui_asks import AskActionsMixin
from opendaisugi.tui_base import CockpitScreen
from opendaisugi.tui_grid import GridWidget

_COLS = ("pane", "label", "harness", "state", "source", "age")
# Only coppice yields frames (master §3.2). For the other two, attach is their own
# command, and saying so beats a blank screen that swallows every key.
_ATTACH_HINT = {
    "tmux": "run `tmux attach` in a terminal, then pick the window",
    "herdr": "run `herdr agent attach <pane>` in a terminal",
}
_RAMP = (
    "c create · p prompt · s steer · a allow · d deny · x close · "
    "⏎ attach · n/N next/prev · r read recent · ? help · : command"
)


def _age(seconds: float) -> str:
    return f"{int(seconds)}s" if seconds < 60 else f"{int(seconds // 60)}m"


class FloorScreen(AskActionsMixin, CockpitScreen):
    """The shop floor. Every state on it is sourced, or it says `unknown`."""

    BINDINGS = [
        Binding("down,j", "cursor_down", "▼", show=True),
        Binding("up,k", "cursor_up", "▲", show=True),
        Binding("c", "create", "c create", show=True),
        Binding("p", "prompt", "p prompt", show=True),
        Binding("s", "steer", "s steer", show=True),
        Binding("a", "arm_allow", "a allow", show=True),
        Binding("d", "deny", "d deny", show=True),
        Binding("x", "close_pane", "x close", show=True),
        Binding("enter", "attach", "⏎ attach", show=True),
        Binding("n", "next_pane", "n next", show=True),
        Binding("N", "prev_pane", "N prev", show=True),
        Binding("r", "read_recent", "r read recent", show=True),
    ]

    CSS = """
    #panes.flash { background: $warning 30%; }
    """
    _ask_origin = "floor view"

    def __init__(self) -> None:
        super().__init__(name="floor")
        self.backend = None
        self.notifier = Notifier(None)
        self.panes: list[PaneInfo] = []
        self._armed: str | None = None
        self._cmd_mode: tuple | None = None
        self._backend_error: str = ""
        self._pump_worker = None

    def compose_body(self) -> ComposeResult:
        with Horizontal():
            yield DataTable(id="panes", cursor_type="row", zebra_stripes=False)
            with Vertical():
                yield GridWidget(id="grid", on_resize=self._resize_selected)
                yield Static("", id="peek")

    def on_mount(self) -> None:
        super().on_mount()
        self.query_one("#panes", DataTable).add_columns(*_COLS)
        config = load_config(self.app.config_path)
        self.notifier = Notifier(config.floor.notify_cmd)
        try:
            self.backend = pick_backend(config)
        except FloorNotAvailable as exc:
            self._backend_error = str(exc)
        self.reload()
        # The poll runs OFF the UI thread. On tmux one poll is a `list-panes` plus a
        # `capture-pane` per pane; doing that on the event loop every second makes the
        # cockpit stutter exactly when the operator is reading it.
        self.set_interval(1.0, self.reload_soon)
        self.start_pump()

    def on_unmount(self) -> None:
        self.stop_pump()

    # --- the event pump ---------------------------------------------------
    def start_pump(self) -> None:
        """Run `backend.subscribe()` on a worker thread and post what it yields.

        Without this the floor is inert: the grid stays blank, the bell never rings,
        and a configured `notify_cmd` never runs. A control the operator configured
        that does nothing is what master §3.5 forbids.
        """
        if self.backend is None or self._pump_worker is not None:
            return
        self._pump_worker = self.run_worker(
            self._pump, thread=True, exclusive=True, name="floor-subscribe"
        )

    def stop_pump(self) -> None:
        if self._pump_worker is not None:
            self._pump_worker.cancel()
            self._pump_worker = None

    def _pump(self) -> None:
        """Worker thread. Never touches a widget; every hand-off is call_from_thread."""
        backend = self.backend
        if backend is None:
            return
        worker = get_current_worker()
        try:
            for item in backend.subscribe():
                if worker.is_cancelled:
                    return
                if isinstance(item, Frame):
                    self.app.call_from_thread(self.post_frame, item)
                else:
                    self.app.call_from_thread(self.on_pane_state, item)
        except Exception:  # noqa: BLE001 — a backend that dies must not kill the app
            return

    def post_frame(self, frame: Frame) -> None:
        """Paint a frame, but only the selected pane's: the grid shows one pane."""
        selected = self.selected_pane
        if selected is None or frame.pane != selected.ref.id:
            return
        self.query_one("#grid", GridWidget).post_message(GridWidget.FrameArrived(frame))

    # --- polling ----------------------------------------------------------
    def reload_soon(self) -> None:
        """Poll the backend on a worker thread, then apply the rows on the UI thread."""
        if self.backend is None:
            self.reload()
            return
        self.run_worker(self._poll, thread=True, exclusive=True, group="floor-roster")

    def _poll(self) -> None:
        try:
            infos = list(self.backend.list())
            error = ""
        except Exception as exc:  # noqa: BLE001
            infos, error = [], str(exc)
        self.app.call_from_thread(self._apply_rows, infos, error)

    def _apply_rows(self, infos: list[PaneInfo], error: str) -> None:
        self.panes = infos
        if error:
            self._backend_error = error
        self._paint_rows()

    def clear_prefill(self) -> None:
        self._cmd_mode = None
        self._armed = None

    # --- data -------------------------------------------------------------
    def reload(self) -> None:
        """A synchronous poll. Used on mount and after an action the operator just took."""
        if self.backend is None:
            self.query_one("#panes", DataTable).clear()
            self.query_one("#peek", Static).update(
                self._backend_error or "no pane backend. Run `daisugi coppice backends`."
            )
            return
        try:
            self.panes = list(self.backend.list())
        except Exception as exc:  # noqa: BLE001 — a backend hiccup must not kill the screen
            self._backend_error = str(exc)
            self.panes = []
        self._paint_rows()

    def _paint_rows(self) -> None:
        table = self.query_one("#panes", DataTable)
        table.clear()
        now = time.time()
        for info in self.panes:
            state = info.state.state if info.state else "unknown"
            source = info.state.source if info.state else "none"
            age = _age(now - info.state.ts) if info.state else "—"
            table.add_row(
                info.ref.id,
                info.label,
                self._harness_of(info),
                state,
                source,
                age,
                key=f"pane:{info.ref.id}",
            )
        self.render_peek()

    @staticmethod
    def _harness_of(info: PaneInfo) -> str:
        if info.state and info.state.harness:
            return info.state.harness
        return info.cmd[0] if info.cmd else "shell"

    @property
    def selected_pane(self) -> PaneInfo | None:
        table = self.query_one("#panes", DataTable)
        if table.row_count == 0:
            return None
        key = table.coordinate_to_cell_key(table.cursor_coordinate).row_key.value
        if not key or not str(key).startswith("pane:"):
            return None
        pane_id = str(key)[5:]
        return next((p for p in self.panes if p.ref.id == pane_id), None)

    @property
    def selected_row(self) -> SessionRow | None:
        """The session behind the selected pane, so the ask actions work unchanged."""
        info = self.selected_pane
        if info is None or info.state is None or not info.state.session_id:
            return None
        roster = build_roster(self.app.data_dir)
        return next((r for r in roster.rows if r.session_id == info.state.session_id), None)

    def render_peek(self) -> None:
        peek = self.query_one("#peek", Static)
        info = self.selected_pane
        if info is None:
            peek.update("no pane selected · c to create one")
            return
        lines = [f"{info.ref.id} · {info.label} · {info.cwd}"]
        if info.state is None:
            lines.append("state unknown · no source is reporting this pane")
        else:
            lines.append(
                f"state {info.state.state} · source {info.state.source} · "
                f"{info.state.detail or '—'}"
            )
            if info.state.ask:
                left = int(info.state.ask.deadline - time.time())
                lines.append(
                    f"ASK {info.state.ask.tool} · {max(left, 0)}s left · {info.state.ask.summary}"
                )
        lines.append(_RAMP)
        peek.update("\n".join(lines))

    def on_data_table_row_highlighted(self, _event) -> None:
        self._armed = None  # a cursor move disarms, exactly as on the sessions screen
        self.render_peek()

    # --- events -----------------------------------------------------------
    def on_pane_state(self, ev: PaneStateEvent) -> None:
        """One state event: flash the row, ring once, and run notify_cmd if set.

        `notify_cmd` is a subprocess with a five second budget, so it runs on a worker
        thread. Running it inline would freeze the cockpit for five seconds at exactly
        the moment the operator needs it.
        """
        if ev.state != "blocked":
            return
        self._flash()
        self.app.bell()
        # The Notifier owns the debounce, in one place, for every caller.
        self.run_worker(partial(self.notifier.notify, ev), thread=True, group="floor-notify")
        self.app.set_status(f"{ev.pane} blocked · {ev.detail or ev.harness}")

    def _flash(self) -> None:
        """Flash the roster once. The class is removed after 300 ms."""
        try:
            table = self.query_one("#panes", DataTable)
        except Exception:  # noqa: BLE001 — a missing table is not worth a crash
            return
        table.add_class("flash")
        self.set_timer(0.3, lambda: table.remove_class("flash"))

    # --- actions ----------------------------------------------------------
    def action_cursor_down(self) -> None:
        self.query_one("#panes", DataTable).action_cursor_down()

    def action_cursor_up(self) -> None:
        self.query_one("#panes", DataTable).action_cursor_up()

    action_next_pane = action_cursor_down
    action_prev_pane = action_cursor_up

    def action_create(self) -> None:
        info = self.selected_pane
        cwd = info.cwd if info else "."
        self._cmd_mode = ("create",)
        self.app.open_cmd(prefill=f"create --cwd {cwd} -- claude")
        self.app.set_status("edit the create line, then ⏎")

    def action_prompt(self) -> None:
        if self.selected_pane is None:
            self.app.set_status("no pane selected")
            return
        self._cmd_mode = ("prompt",)
        self.app.open_cmd(prefill="")
        self.app.set_status("type the prompt, then ⏎")

    def action_steer(self) -> None:
        info = self.selected_pane
        if info is None:
            self.app.set_status("no pane selected")
            return
        harness = self._harness_of(info)
        if harness != "sprig":
            self.app.set_status(f"steer: not on this path ({harness})")
            return
        self._cmd_mode = ("steer", info.ref.id)
        self.app.open_cmd(prefill="")
        self.app.set_status(f"steer note for {info.ref.id}, then ⏎")

    def action_read_recent(self) -> None:
        info = self.selected_pane
        if info is None or self.backend is None:
            self.app.set_status("no pane selected")
            return
        text = self.backend.read(info.ref, source="recent")
        self.query_one("#peek", Static).update(text[-4000:])

    def action_close_pane(self) -> None:
        info = self.selected_pane
        if info is None or self.backend is None:
            self.app.set_status("no pane selected")
            return
        self.backend.close(info.ref)
        self.app.set_status(f"closed {info.ref.id}")
        self.reload()

    def action_attach(self) -> None:
        info = self.selected_pane
        if info is None or self.backend is None:
            self.app.set_status("no pane selected")
            return
        if not getattr(self.backend, "yields_frames", False):
            # Only coppice streams frames. Attaching on tmux or herdr would paint a
            # blank screen while swallowing every key, so refuse and teach instead.
            hint = _ATTACH_HINT.get(self.backend.name, "use that substrate's own attach")
            self.app.set_status(f"attach needs frames, and {self.backend.name} sends text. {hint}.")
            return
        self.app.push_screen(AttachScreen(self.backend, info))

    def _resize_selected(self, cols: int, rows: int) -> None:
        info = self.selected_pane
        if info is not None and self.backend is not None:
            self.backend.resize(info.ref, cols, rows)

    # --- the command line -------------------------------------------------
    def on_cmd_submitted(self, value: str) -> bool:
        mode = self._cmd_mode
        if mode is None:
            return False
        self._cmd_mode = None
        kind = mode[0]
        if kind == "allow_token":
            return self.handle_allow_token(mode, value)
        if kind == "create":
            return self._do_create(value)
        if kind == "prompt":
            info = self.selected_pane
            if info is None or self.backend is None:
                self.app.set_status("no pane selected")
                return True
            how = prompt_pane(self.backend, info.ref, value)
            self.app.set_status(f"{how} to {info.ref.id}")
            return True
        if kind == "steer":
            from opendaisugi.session_tree import SessionTree

            _, pane_id = mode
            info = next((p for p in self.panes if p.ref.id == pane_id), None)
            sid = info.state.session_id if info and info.state else ""
            if not sid:
                self.app.set_status("steer: this pane has no session")
                return True
            try:
                SessionTree.open(self.app.data_dir / "sessions", sid).append(
                    "note", {"from": "operator", "text": value}
                )
                self.app.set_status(f"steer note added to {sid}")
            except FileNotFoundError:
                self.app.set_status(f"steer: session {sid} not found")
            return True
        return False

    def _do_create(self, line: str) -> bool:
        if self.backend is None:
            self.app.set_status("no pane backend. Run `daisugi coppice backends`.")
            return True
        parts = shlex.split(line)
        if parts and parts[0] == "create":
            parts = parts[1:]
        cwd, label, kind, argv = ".", "", "pty", []
        index = 0
        while index < len(parts):
            token = parts[index]
            if token == "--":
                argv = parts[index + 1 :]
                break
            if token == "--cwd" and index + 1 < len(parts):
                cwd, index = parts[index + 1], index + 2
                continue
            if token == "--label" and index + 1 < len(parts):
                label, index = parts[index + 1], index + 2
                continue
            if token == "--kind" and index + 1 < len(parts):
                kind, index = parts[index + 1], index + 2
                continue
            index += 1
        if not argv:
            self.app.set_status("create needs a command: create --cwd DIR -- claude")
            return True
        ref = self.backend.spawn(cwd=Path(cwd), cmd=argv, env={}, label=label or argv[0], kind=kind)
        self.app.set_status(f"created {ref.id}")
        self.reload()
        return True

    def action_confirm(self) -> None:
        if self.action_confirm_allow():
            return
        self.action_attach()

    def on_data_table_row_selected(self, _event) -> None:
        self.action_confirm()


class AttachScreen(ModalScreen):
    """One pane, full screen, keys passed through. `ctrl+a` is the only chord we keep."""

    passthrough = True
    LEADER = "ctrl+a"

    # Every printable and special key reaches `on_key`; nothing is bound here, so
    # nothing on this screen can shadow a key the harness wants.
    BINDINGS: list = []

    _SPECIAL = {
        "enter": "enter",
        "escape": "esc",
        "tab": "tab",
        "backspace": "backspace",
        "up": "up",
        "down": "down",
        "left": "left",
        "right": "right",
        "f1": "f1",
        "f2": "f2",
        "f3": "f3",
        "f4": "f4",
        "space": "space",
    }

    def __init__(self, backend, info: PaneInfo) -> None:
        super().__init__(name="attach")
        self.backend = backend
        self.info = info

    def compose(self) -> ComposeResult:
        yield GridWidget(id="attach-grid", on_resize=self._resize)
        yield Static(
            f"{self.info.ref.id} · {self.info.label} · ctrl+a returns to the floor",
            id="attach-status",
        )

    def _resize(self, cols: int, rows: int) -> None:
        self.backend.resize(self.info.ref, cols, rows)

    def on_key(self, event) -> None:
        """Send every key to the pane. The leader, and only the leader, returns."""
        event.stop()
        event.prevent_default()
        key = event.key
        if key == self.LEADER:
            self.dismiss()
            return
        mapped = self._SPECIAL.get(key)
        if mapped is not None:
            self.backend.send_keys(self.info.ref, [mapped])
            return
        if key.startswith("ctrl+") or len(key) == 1:
            self.backend.send_keys(self.info.ref, [key])
            return
        if event.character:
            self.backend.send_keys(self.info.ref, [event.character])
```

- [ ] **Step 5: Wire the screen and the `SkipAction` passthrough guards into `tui.py`**

In `tui.py`, inside `if _HAVE_TEXTUAL:`:

```python
    from textual.actions import SkipAction

    from opendaisugi.tui_floor import FloorScreen
    from opendaisugi.tui_sessions import SessionsScreen
    from opendaisugi.tui_tree import TreeScreen
    from opendaisugi.tui_wiring import WiringScreen, swap_confirmation

    _ORDER = ("sessions", "floor", "tree", "wiring")
```

and in `DaisugiApp`:

```python
SCREENS = {
    "sessions": SessionsScreen,
    "floor": FloorScreen,
    "tree": TreeScreen,
    "wiring": WiringScreen,
}
```

Add the guard helper and the four guarded actions (replacing the current `action_cycle`,
`action_cmd`, `action_help`, and adding `action_quit`):

```python
def _passthrough(self) -> bool:
    """True while a screen owns every key, e.g. full-screen attach.

    The App's `tab` binding is priority=True, so it fires BEFORE the screen's,
    and `App._dispatch_action` counts an action as handled the moment it is
    invoked — a plain `return` still eats the key. Every caller therefore
    raises `SkipAction`, which is the one signal that makes the dispatcher
    report the key unhandled and let it reach the screen. Verified on textual
    8.2.8: with `return`, `AttachScreen.on_key` sees `['q', 'colon',
    'question_mark', 'h']`; with `SkipAction`, it sees `tab` as well.
    """
    return bool(getattr(self.screen, "passthrough", False))


def action_cycle(self) -> None:
    if self._passthrough():
        raise SkipAction()
    cur = self.screen.name or "sessions"
    nxt = _ORDER[(_ORDER.index(cur) + 1) % len(_ORDER)] if cur in _ORDER else "sessions"
    self.switch_screen(nxt)


def action_cmd(self) -> None:
    if self._passthrough():
        raise SkipAction()
    self.open_cmd()


def action_help(self) -> None:
    if self._passthrough():
        raise SkipAction()
    keys = ", ".join(
        f"{b.key}={b.description}" for b in self.screen.BINDINGS if getattr(b, "show", True)
    )
    self.set_status(f"keys: {keys} · Tab next view · : command · q quit")


def action_quit(self) -> None:
    if self._passthrough():
        raise SkipAction()
    self.exit()
```

Add `floor` to the command router's view list (it is already covered: `_ORDER` drives
`run_command`) and to `_CMD_PLACEHOLDER` in `tui_base.py`:

```python
_CMD_PLACEHOLDER = "sessions · floor · tree <id> · wiring · gate enforce|shadow"
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_tui_floor.py tests/test_tui.py tests/test_tui_app.py tests/test_tui_sessions.py tests/test_tui_ask.py tests/test_tui_honesty.py -q`
Expected: PASS. If a sessions test fails, the mixin move changed behaviour; restore the moved body
byte for byte rather than adjusting the test.

- [ ] **Step 7: Lint and commit**

```bash
uv run --no-sync ruff check .
git add src/opendaisugi/tui_asks.py src/opendaisugi/tui_floor.py \
        src/opendaisugi/tui_sessions.py src/opendaisugi/tui.py src/opendaisugi/tui_base.py \
        tests/test_tui_floor.py
git commit -m "feat(cockpit): a floor screen, and an attach that does not eat the harness's keys

The app binds tab with priority=True plus q, colon and question_mark. Inside a
full-screen attach every one of those belongs to Claude Code, so a screen can now
mark itself passthrough and those four actions no-op. ctrl+a is the only chord we
keep. The allow guard moved to a mixin so the floor inherits it rather than
growing a second copy of a safety rule."
```

---

### Task 12: `daisugi coppice` — the same power for scripts and agents

**Files:**
- Modify: `src/opendaisugi/cli.py` (declare `coppice_app` beside the other groups ≈ line 336;
  add the nine commands after `dashboard_cmd` ≈ line 3225)
- Test: `tests/floor/test_cli_coppice.py`

**Interfaces:**
- Consumes: `registry.pick_backend(config, *, name, autostart)` / `backend_statuses` /
  `prompt_pane` / `wait_for_state` (task 3); `FloorNotAvailable` (task 3);
  every backend's `spawn(..., harness=None)` (tasks 4, 5, 6).
- Produces: the CLI surface only. No new importable symbols.

- [ ] **Step 1: Write the failing test**

```python
# tests/floor/test_cli_coppice.py
"""`daisugi coppice …`: the floor for scripts and agents.

Exit codes are the master's: 0 success, 1 user error, 3 unreachable. `backends`
is the diagnostic, so it exits 0 even when nothing is installed — a table of
three `no` rows with three fixes is the answer, not a failure.
"""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def _no_backends(monkeypatch):
    from opendaisugi.floor import registry

    class Dead:
        name = "dead"
        yields_frames = False

        def available(self):
            return False

    monkeypatch.setattr(registry, "build_backend", lambda name, config, *, autostart=False: Dead())


def test_backends_with_nothing_installed_prints_three_no_rows_and_exits_zero(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(app, ["coppice", "backends", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    for name in ("coppice", "herdr", "tmux"):
        assert name in result.stdout
    assert result.stdout.count("no") >= 3
    assert "coppice server start" in result.stdout


def test_backends_json_is_a_list_of_rows(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(app, ["coppice", "backends", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    rows = json.loads(result.stdout)
    assert [r["name"] for r in rows] == ["coppice", "herdr", "tmux"]
    assert all(r["available"] is False and r["fix"] for r in rows)


def test_spawn_with_nothing_installed_exits_three_and_names_the_fix(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "coppice server start" in result.output


def test_an_unknown_backend_name_exits_three_and_lists_the_real_ones(tmp_path):
    result = runner.invoke(
        app, ["coppice", "list", "--backend", "zellij", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 3
    assert "coppice, herdr, tmux" in result.output


def test_every_coppice_command_accepts_data_dir(tmp_path, fake_floor_backend):
    """No group may be pinned to the developer's real ~/.opendaisugi."""
    for argv in (
        ["coppice", "backends"],
        ["coppice", "list"],
        ["coppice", "read", "p1"],
        ["coppice", "close", "p1"],
        ["coppice", "send-keys", "p1", "enter"],
        ["coppice", "prompt", "p1", "hi"],
    ):
        result = runner.invoke(app, [*argv, "--data-dir", str(tmp_path)])
        assert result.exit_code == 0, f"{argv}: {result.output}"


def test_the_config_read_is_the_one_under_data_dir(tmp_path, monkeypatch):
    """`--data-dir` must reach `load_config`, not just be accepted and dropped."""
    seen: list = []
    from opendaisugi.floor import registry

    class Fake:
        name = "fake"
        yields_frames = False

        def available(self):
            return True

        def list(self):
            return []

    def picked(config, *, name=None, autostart=False):
        seen.append(config)
        return Fake()

    monkeypatch.setattr(registry, "pick_backend", picked)
    (tmp_path / "config.yaml").write_text("floor:\n  backend: tmux\n", encoding="utf-8")
    result = runner.invoke(app, ["coppice", "list", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert seen and seen[0].floor.backend == "tmux"


def test_an_explicit_coppice_backend_asks_for_autostart(tmp_path, monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    class Fake:
        name = "coppice"
        yields_frames = True

        def available(self):
            return True

        def list(self):
            return []

    def picked(config, *, name=None, autostart=False):
        seen.append(autostart)
        return Fake()

    monkeypatch.setattr(registry, "pick_backend", picked)
    runner.invoke(app, ["coppice", "list", "--backend", "coppice", "--data-dir", str(tmp_path)])
    runner.invoke(app, ["coppice", "list", "--data-dir", str(tmp_path)])
    assert seen == [True, False]


def test_an_unknown_read_source_exits_one_and_names_the_real_ones(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "read", "p1", "--source", "scrollback", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "detection" in result.output
    assert fake_floor_backend.reads == [], "a bad source must be refused before the backend"


def test_spawn_prints_the_pane_id(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--label",
            "fix",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    assert result.exit_code == 0
    assert "p1" in result.stdout
    assert fake_floor_backend.spawned[0]["label"] == "fix"


def test_spawn_passes_harness_straight_through_with_no_typeerror_retry(
    tmp_path, fake_floor_backend
):
    """A retry-on-TypeError would swallow a real TypeError raised inside spawn."""
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--kind",
            "headless",
            "--harness",
            "claude-code",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    assert result.exit_code == 0
    assert fake_floor_backend.spawned[0]["harness"] == "claude-code"


def test_a_typeerror_inside_spawn_is_not_retried(tmp_path, monkeypatch, fake_floor_backend):
    calls: list[int] = []

    def boom(**kwargs):
        calls.append(1)
        raise TypeError("a real bug inside spawn")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code != 0
    assert calls == [1], "spawn was retried, hiding the bug"


def test_spawn_json_reports_the_backend_it_used(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--json",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    body = json.loads(result.stdout)
    assert body["pane"] == "p1" and body["backend"] == "fake"


def test_list_json_carries_state_and_source(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "list", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    rows = json.loads(result.stdout)
    assert rows[0]["pane"] == "p1"
    assert rows[0]["state"] == "working" and rows[0]["source"] == "gate"


def test_read_passes_the_source_through(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "read", "p1", "--source", "detection", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert ("p1", "detection") in fake_floor_backend.reads


def test_prompt_says_whether_it_prompted_or_typed(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "prompt", "p1", "fix the test", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert "typed" in result.stdout
    assert fake_floor_backend.sent == [("p1", "fix the test")]


def test_send_keys_forwards_every_key(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "send-keys", "p1", "ctrl+c", "enter", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert fake_floor_backend.keys == [("p1", ["ctrl+c", "enter"])]


def test_close_reports_the_pane(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "close", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert fake_floor_backend.closed == ["p1"]


def test_wait_that_times_out_exits_one_and_says_the_state_it_saw(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "wait",
            "p1",
            "--until",
            "blocked",
            "--timeout",
            "0.2",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 1
    assert "working" in result.output or "timed out" in result.output


def test_attach_on_a_non_coppice_backend_teaches_instead_of_pretending(
    tmp_path, fake_floor_backend
):
    result = runner.invoke(app, ["coppice", "attach", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "fake" in result.output and "coppice" in result.output


def test_every_coppice_command_is_reachable_from_help_all():
    result = runner.invoke(app, ["help", "--all"])
    for verb in (
        "spawn",
        "list",
        "prompt",
        "wait",
        "read",
        "send-keys",
        "close",
        "attach",
        "backends",
    ):
        assert f"coppice {verb}" in result.stdout
```

Add the shared fake backend to `tests/floor/conftest.py`:

```python
@pytest.fixture
def fake_floor_backend(monkeypatch):
    """A backend `pick_backend` always returns, recording every call."""
    import time as _time

    from opendaisugi.floor import PaneInfo, PaneRef, PaneStateEvent
    from opendaisugi.floor import registry

    class Fake:
        name = "fake"
        yields_frames = False

        def __init__(self):
            self.spawned: list[dict] = []
            self.sent: list[tuple[str, str]] = []
            self.keys: list[tuple[str, list[str]]] = []
            self.closed: list[str] = []
            self.reads: list[tuple[str, str]] = []

        def available(self):
            return True

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
            self.spawned.append(
                {"cwd": str(cwd), "cmd": cmd, "label": label, "kind": kind, "harness": harness}
            )
            return PaneRef("fake", "p1")

        def list(self):
            ev = PaneStateEvent(
                session_id="s1",
                harness="claude-code",
                state="working",
                source="gate",
                ts=_time.time(),
                pane="p1",
            )
            return [
                PaneInfo(
                    ref=PaneRef("fake", "p1"),
                    label="fix",
                    cwd="/repo",
                    cmd=["claude"],
                    kind="pty",
                    state=ev,
                )
            ]

        def send_text(self, pane, text, *, enter=True):
            self.sent.append((pane.id, text))

        def send_keys(self, pane, keys):
            self.keys.append((pane.id, list(keys)))

        def read(self, pane, *, source="visible"):
            self.reads.append((pane.id, source))
            return f"[{source}]\n"

        def resize(self, pane, cols, rows):
            return None

        def close(self, pane):
            self.closed.append(pane.id)

        def report_state(self, pane, ev):
            return None

        def subscribe(self):
            return iter(())

    fake = Fake()
    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: fake
    )
    return fake
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `uv run --no-sync pytest tests/floor/test_cli_coppice.py -q`
Expected: FAIL — `No such command 'coppice'` (exit code 2 from Typer).

- [ ] **Step 1b: Note what `--data-dir` is for**

Every other group in `cli.py` takes `--data-dir` (`cli.py:1679`, `cli.py:1805`). Without it the
coppice group reads the developer's real `~/.opendaisugi/config.yaml` in every test, which is
both a leak and a flake. Every command below takes it and threads it into `_floor_config`.

- [ ] **Step 3: Declare the group**

In `cli.py`, after the `batch_app` block (≈ line 336):

```python
coppice_app = typer.Typer(
    name="coppice",
    help="Drive panes on the floor: spawn, read, prompt, wait, close, attach.",
    no_args_is_help=True,
)
app.add_typer(coppice_app, name="coppice", rich_help_panel="Run")
```

- [ ] **Step 4: Add the nine commands**

Append after `dashboard_cmd` (≈ line 3225):

```python
_BACKEND_OPT = typer.Option(None, "--backend", help="Force a backend: coppice, herdr, or tmux.")
_FLOOR_DATA_DIR_OPT = typer.Option(DEFAULT_DATA_DIR, "--data-dir", help="Daisugi data directory.")
_READ_SOURCES = ("visible", "recent", "detection")


def _floor_config(data_dir: Path):
    """The config the floor reads. `load_config` is a function-local import in cli.py."""
    from opendaisugi.config import load_config

    return load_config(data_dir / "config.yaml")


def _floor_backend(name: str | None, data_dir: Path):
    """Pick the backend or exit 3 with the command that installs one.

    Naming coppice is asking for coppice, so an explicit `--backend coppice` may start
    the server once. `auto` never does, and neither does `backends`.
    """
    from opendaisugi.exceptions import FloorNotAvailable
    from opendaisugi.floor.registry import pick_backend

    try:
        return pick_backend(_floor_config(data_dir), name=name, autostart=name == "coppice")
    except FloorNotAvailable as exc:
        typer.echo(str(exc), err=True)
        raise typer.Exit(code=3) from exc


def _pane_ref(backend, pane: str):
    from opendaisugi.floor import PaneRef

    return PaneRef(backend.name, pane)


@coppice_app.command("backends")
def coppice_backends_cmd(
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Which pane backends this box has, and what to run for the ones it does not.

    Always exits 0. Nothing installed is an answer, not a failure.
    """
    from opendaisugi.floor.registry import backend_statuses

    rows = backend_statuses(_floor_config(data_dir))
    if json_output:
        typer.echo(json.dumps([asdict(r) for r in rows], indent=2))
        return
    typer.echo(f"{'backend':<10}{'available':<12}why")
    for row in rows:
        why = "" if row.available else f"{row.why_not}. {row.fix}"
        typer.echo(f"{row.name:<10}{'yes' if row.available else 'no':<12}{why}")


@coppice_app.command("spawn")
def coppice_spawn_cmd(
    argv: list[str] = typer.Argument(..., help="The command to run, after `--`."),
    cwd: Path = typer.Option(..., "--cwd", help="Working directory for the pane."),
    label: str = typer.Option("", "--label", help="A short name for the pane."),
    kind: str = typer.Option("pty", "--kind", help="pty or headless."),
    harness: str = typer.Option(None, "--harness", help="Adapter name for a headless pane."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Start a command in a new pane and print its id."""
    chosen = _floor_backend(backend, data_dir)
    # `harness` is in the §3.2 protocol, so every backend takes it. tmux and herdr
    # ignore it. No except-TypeError retry: that would swallow a real TypeError raised
    # inside spawn and silently run it again.
    ref = chosen.spawn(
        cwd=cwd, cmd=list(argv), env={}, label=label or argv[0], kind=kind, harness=harness or None
    )
    if json_output:
        typer.echo(json.dumps({"pane": ref.id, "backend": chosen.name}))
        return
    typer.echo(f"{ref.id}  ({chosen.name})")


@coppice_app.command("list")
def coppice_list_cmd(
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
    json_output: bool = typer.Option(False, "--json", help="Machine-readable JSON output."),
) -> None:
    """Every pane, its label, its state, and where that state came from."""
    chosen = _floor_backend(backend, data_dir)
    rows = [
        {
            "pane": info.ref.id,
            "label": info.label,
            "cwd": info.cwd,
            "kind": info.kind,
            "state": info.state.state if info.state else "unknown",
            "source": info.state.source if info.state else "none",
        }
        for info in chosen.list()
    ]
    if json_output:
        typer.echo(json.dumps(rows, indent=2))
        return
    if not rows:
        typer.echo("no panes. Start one with `daisugi coppice spawn --cwd . -- claude`.")
        return
    typer.echo(f"{'pane':<12}{'label':<18}{'state':<10}{'source':<10}cwd")
    for row in rows:
        typer.echo(
            f"{row['pane']:<12}{row['label'][:17]:<18}{row['state']:<10}"
            f"{row['source']:<10}{row['cwd']}"
        )


@coppice_app.command("prompt")
def coppice_prompt_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    text: str = typer.Argument(..., help="The prompt."),
    wait: bool = typer.Option(False, "--wait", help="Block until the agent answers."),
    timeout: float = typer.Option(60.0, "--timeout", help="Seconds to wait."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Send a prompt. A headless pane gets an agent prompt; a pty pane gets typed text."""
    from opendaisugi.floor.registry import prompt_pane

    chosen = _floor_backend(backend, data_dir)
    how = prompt_pane(chosen, _pane_ref(chosen, pane), text, wait=wait, timeout_s=timeout)
    typer.echo(f"{how} to {pane}")


@coppice_app.command("wait")
def coppice_wait_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    until: str = typer.Option("idle", "--until", help="idle, working, blocked, done, or any."),
    timeout: float = typer.Option(60.0, "--timeout", help="Seconds to wait."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Block until the pane reaches a state. Exit 1 on timeout, with the state it saw."""
    from opendaisugi.floor.registry import wait_for_state

    chosen = _floor_backend(backend, data_dir)
    ref = _pane_ref(chosen, pane)
    event = wait_for_state(chosen, ref, until=until, timeout_s=timeout)
    if event is None:
        infos = {i.ref.id: i for i in chosen.list()}
        info = infos.get(pane)
        seen = info.state.state if info and info.state else "unknown"
        typer.echo(
            f"timed out after {timeout:g}s waiting for {until}. Last state: {seen}.", err=True
        )
        raise typer.Exit(code=1)
    typer.echo(f"{pane} is {event.state} (source {event.source})")


@coppice_app.command("read")
def coppice_read_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    source: str = typer.Option("visible", "--source", help="visible, recent, or detection."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Print what the pane shows. `detection` is exactly what the manifests see."""
    if source not in _READ_SOURCES:
        _fail(
            f"no read source {source!r}.",
            f"The floor reads one of: {', '.join(_READ_SOURCES)}.",
            f"Try: daisugi coppice read {pane} --source detection",
        )
    chosen = _floor_backend(backend, data_dir)
    typer.echo(chosen.read(_pane_ref(chosen, pane), source=source), nl=False)


@coppice_app.command("send-keys")
def coppice_send_keys_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    keys: list[str] = typer.Argument(..., help="Key names: enter, ctrl+c, esc, tab, f1, or a."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Send key presses to a pane."""
    chosen = _floor_backend(backend, data_dir)
    chosen.send_keys(_pane_ref(chosen, pane), list(keys))
    typer.echo(f"sent {' '.join(keys)} to {pane}")


@coppice_app.command("close")
def coppice_close_cmd(
    pane: str = typer.Argument(..., help="Pane id."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Close a pane."""
    chosen = _floor_backend(backend, data_dir)
    chosen.close(_pane_ref(chosen, pane))
    typer.echo(f"closed {pane}")


@coppice_app.command("attach")
def coppice_attach_cmd(
    pane: str = typer.Argument(None, help="Pane id. Omit for the current one."),
    backend: str = _BACKEND_OPT,
    data_dir: Path = _FLOOR_DATA_DIR_OPT,
) -> None:
    """Take over a pane full screen. Only the coppice backend has its own renderer."""
    chosen = _floor_backend(backend, data_dir)
    if chosen.name != "coppice":
        typer.echo(f"attach is not on the {chosen.name} backend.", err=True)
        typer.echo(
            f"Use `{chosen.name} attach` for that substrate, or run "
            f"`coppice server start` and retry with --backend coppice.",
            err=True,
        )
        raise typer.Exit(code=1)
    argv = ["coppice", "attach"] + ([pane] if pane else [])
    os.execvp("coppice", argv)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/floor/test_cli_coppice.py tests/test_cli.py tests/test_cli_errors.py tests/test_cli_json_conformance.py -q`
Expected: PASS. `test_cli_json_conformance` checks that every `--json` command emits parsable
JSON on stdout only; if `coppice list` fails it, the human table is leaking to stdout in JSON mode.

- [ ] **Step 6: Run the whole suite and the linter**

Run: `uv run --no-sync pytest -q && uv run --no-sync ruff check .`
Expected: green and clean.

- [ ] **Step 7: Commit**

```bash
git add src/opendaisugi/cli.py tests/floor/test_cli_coppice.py tests/floor/conftest.py
git commit -m "feat(cli): daisugi coppice gives scripts the floor the cockpit has

Nine verbs over the same backend registry the TUI uses, so an agent and an
operator drive the same panes. backends exits 0 with three fixes when nothing is
installed, because a diagnostic that fails is no diagnostic; every other verb
exits 3 when no backend answers, and names the command that starts one."
```

---

## Self-review

Run against `spec-03-coppice-client-and-backends.md` after the plan was written.

**1. Spec coverage.** Every file in the spec's Files block maps to a task:
`coppice_backend.py` → 5 · `herdr_backend.py` → 6 · `tmux_backend.py` → 4 · `manifests.py` → 2
(schema pinned in 1) · `registry.py` → 3 · `notify.py` → 8 · `tui_floor.py` → 11 ·
`tui_grid.py` → 10 · `cli.py` → 12 · `config.py` / `modules.py` / `swap.py` → 9 ·
`test_backend_contract.py` → 7 · `test_manifests.py` → 2 · `test_coppice_backend.py` → 5 ·
`test_tmux_backend.py` → 4 · `test_herdr_backend.py` → 6 · `test_tui_floor.py` → 11.
Every Interfaces signature in the spec appears in the task that produces it. Every named test in
the spec's Tests block has a test function: the contract pass (task 7), the shared-fixture
agreement (task 2), the private tmux server (task 4), the pilot key tests and the one-notify
assertion (task 11), and both CLI exit-code tests (task 12).

**2. Placeholder scan.** No "TBD", no "similar to Task N", no "add error handling". Every code
step carries the code. The one conditional branch is task 1 step 4, where the recorder either
reads real files or writes an explicitly unverified pin; both branches are written out.

**2b. Second pass (after the first self-review).** Four defects found and fixed inline:
the `dataclass` import in `registry.py` sat below the package imports and would have failed
`ruff check` under rule `I`; two user-facing strings carried em-dashes against the STE100
constraint; `set_field`'s annotation claimed `Config` while it recurses on a nested `BaseModel`;
`_floor_backend` called `load_config`, which `cli.py` does not import at module level. One test
name in task 11 contradicted its own assertion and became three tests: the screen hands every
event to the `Notifier`, the `Notifier` runs the command exactly once in five seconds, and a
`working` event notifies nothing.

**2c. Adversarial review, round 1 (2026-09-08).** Four blockers and fifteen should-fix items
were applied in place; each entry in the corrections block above now carries the task and step
it landed in. The four blockers:

* **Attach ate Tab.** A guarded action that `return`s is still "handled" by Textual's
  dispatcher. Every guard now `raise SkipAction()`, and `test_attach_passes_tab_to_the_harness`
  asserts `tab` reaches the fake backend (Task 11 steps 1 and 5).
* **Nothing called `subscribe()`.** `FloorScreen.start_pump` runs it on a thread worker and
  hands frames and state back through `call_from_thread`; the roster poll and `notify_cmd` moved
  off the UI thread too. Two tests drive a frame and a `blocked` event through the real pump
  (Task 11 steps 1 and 4).
* **`source: "gate"` was forgeable.** `_authority_is_daisugi` walked a substring of the whole
  `agent explain` document, so any path containing `openDaisugi` stamped Herdr's screen guess as
  a gate fact. It now walks the pinned `["authority", "source"]` path and compares exactly, with
  tests for the cwd case, the missing field, and four wrong shapes (Task 6 steps 1, 2, 4).
* **tmux never reported `done`.** `subscribe()` tracked states, so a pane no manifest classified
  was never announced when it exited. It now tracks pane ids and `pane_dead`, independently of
  any manifest, and two tests cover it including one with no manifests installed
  (Task 4 steps 1 and 3).

**3. Type consistency.** `PaneRef` / `PaneInfo` / `Frame` / `PaneStateEvent` / `Ask` /
`PaneBackend` / `merge` come from plan 01 and are named in Global Constraints, never redefined.
`Manifest` / `Rule` / `ManifestSchema` (task 2) are used only from task 4 onward.
`BackendStatus` / `pick_backend` / `prompt_pane` / `wait_for_state` (task 3) are used in tasks 11
and 12. `Notifier` (task 8) is used in task 11. `GridWidget` (task 10) is used in task 11.
`AskActionsMixin` (task 11) is defined and consumed in the same task. `FloorConfig` (task 9) is
read by `tui_floor.py` (task 11) and `cli.py` (task 12), both later. `HerdrVerbs` and `load_verbs`
(task 6) are used only inside task 6.

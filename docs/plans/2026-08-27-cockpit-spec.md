# daisugi operator interface: implementation spec

**Status:** design, ready to build. Nothing here is implemented.
**Date:** 2026-08-27
**Argues from:**
- `docs/plans/2026-08-26-daisugi-interface-two-lenses.md` (the nine workstreams W1 to W9)
- `docs/research/cockpit-gate-garden-research-2026-08-27.md` (the gate + garden research, §4 in particular)
- `docs/research/tool-interface-doctrine.md` (the bimodal doctrine)
- Three code scouts run on 2026-08-27 (facts quoted below with `path:line`).

This spec is the contract for five plans. Each plan is one commit series that leaves
the suite green and the tool usable. Build them in this order:

| Plan | Workstreams | File |
|---|---|---|
| 1 | W9 finish + W6: errors that teach | `2026-08-27-plan-1-errors-that-teach.md` |
| 2 | W1 + W2: pipe-safe output, fast start, resident gate | `2026-08-27-plan-2-cli-basics.md` |
| 3 | W8 data layer: session tree, join keys, structured verdicts, operator ask | `2026-08-27-plan-3-session-tree.md` |
| 4 | W8 + W5 + W7: the multi-session view, wiring demoted, tree view, rewind | `2026-08-27-plan-4-multi-session-view.md` |
| 5 | W4 + W3: one way in, a short command surface | `2026-08-27-plan-5-one-way-in.md` |

Why this order and not W9 → W8 → ... → W1: plan 2 gives the multi-session view a
pipe-safe output layer and a gate that answers in milliseconds. Build the view first and
you fix those twice. W3 goes last because the top-level command list depends on what
`start` and the view need.

---

## 1. Terms

- **Session.** One agent's thread of work: one transcript, one cached prompt prefix,
  one workspace state, one row on screen. A fork makes a new session.
- **Multi-session view.** The live screen that shows every session's current action and
  its verdict. The default screen of the TUI.
- **Tree view.** One session's prompt tree: rewind, fork, labels, checkpoints.
- **Garden.** The view over time: pathways, reuse, tokens saved. Out of scope for these
  five plans except where the header shows cache hit rate.
- **Verdict.** The gate's answer for one tool call: allow, deny, or ask.
- **Ask.** A would-deny that the gate hands to a present operator for a bounded time.
  No operator, or no answer in time, means deny.
- **Wiring.** The module map with swap knobs. Today's TUI leads with it. It becomes
  `:wiring`, reachable, never default.

Do not use "floor" or "lane" in code or prose. Use the words above.

---

## 2. Global constraints (every plan inherits these)

- Python `>=3.12`. Typer `>=0.12,<1` (0.23.1 installed). Ruff `line-length = 100`,
  `select = ["F", "E9", "B", "I"]`, `target-version = "py312"`.
- `uv run pytest -q` green and `uv run ruff check .` clean before every commit.
  2,436 tests collected today. `--strict-markers` and `asyncio_mode = "auto"` are on.
- TDD: failing test, then code, then green, then commit. Atomic commits, each stating
  the why. No `Co-Authored-By` or "Generated with" trailers. Never push.
- User-facing text: short sentences, plain words. Every error states three things in
  order: what was attempted, why it failed, the one next action. No stack trace to a
  human unless `-v` or `DAISUGI_DEBUG=1`.
- Fail-closed is not negotiable: in enforce mode, any internal error on any gate path
  is a deny (exit 2 on the Claude path). Every new gate path gets a test for this.
- Data dir is `~/.opendaisugi`. The gate root is `<data_dir>/gate`; the gate derives
  `data_dir` as `root.parent` (`gate.py:62` already does). New stores take an explicit
  `data_dir: Path`.
- The word "session" is already used for three things in code (capture files in
  `hook.py:229 list_sessions`, `RunSession` in `run_session.py:53`, gate envelope keys
  in `gate.py:319`). New code uses `SessionTree` and `SessionIndex` in module
  `session_tree.py`, and never adds a new `list_sessions`, `SessionRecord`, or
  `Session` symbol.
- Textual is the `[tui]` extra (`textual>=8.0`, `textual-serve>=1.1`). Both are
  already pinned in `uv.lock` (textual **8.2.8**, textual-serve 1.1.3) and the extra
  exists in `pyproject.toml`; what is missing is the extra in the *working venv*, so
  `tests/test_tui.py` collects nothing today. Plan 4's Task 0 syncs it
  (`uv sync --extra tui`, or `--extra dev` which pulls it). All of plan 4's
  flagged-uncertain Textual-8 names were verified present in 8.2.8 during review:
  `App.switch_screen`, `App.push_screen(callback=)`, `App.copy_to_clipboard`,
  `App.run_test`, `ModalScreen[str]`, `DataTable.update_cell`,
  `DataTable.coordinate_to_cell_key`, `Tree`.
- Public repo: no personal paths, no personal names, neutral persona.

---

## 3. What the scouts found that changes the plan

1. **The 0.8 s cold start is not Z3.** `python -X importtime` puts `opendaisugi/__init__.py`
   at 531 to 534 ms of the ~555 ms `import opendaisugi.cli`. It eagerly imports 54
   submodules (`__init__.py:22-272`); `agentic_executor` alone is 342 ms cumulative,
   `networkx` 106 ms, pydantic model building 44 ms. Z3 is 24 to 36 ms. `python -m
   opendaisugi.gate` pays the same package init. Real verification work is 0.6 to 4 ms.
   So W2's fix is a lazy package init (PEP 562) plus a resident gate process, not
   "lazy-load Z3".
2. **`GateDecision` (`gate.py:68-85`) throws structure away.** `VerificationResult`
   carries `violations[].stage/message/detail/suggested_remediation`, `envelope_id`,
   `plan_id` (`models.py:570-595`); `evaluate_record` flattens them into one string at
   `gate.py:174-176`. The verdict entry needs the structure back.
3. **The hook reads five payload fields and drops the join keys.** `_payload_to_record`
   (`hook.py:143-185`) reads `session_id`, `tool_name`, `tool_input`. It never reads
   `tool_use_id`, `agent_id`, `cwd`, `transcript_path`, `hook_event_name`. Zero hits
   for those names in the package. `transcript_path` is how a path-D session reaches
   Claude's own JSONL (usage counters, `parentUuid` tree).
4. **The mode is invisible at the point of action.** `_outcome` (`gate.py:386-408`)
   emits `{"continue": true}` in both modes; a shadow would-deny is indistinguishable
   from an allow on the wire. `gate status` prints armed/disarmed, not the mode.
   `gate check --mode` (`cli.py:777`) has no `choices` validation.
5. **There is no gate daemon.** Every gate call is a fresh process (`gate.py:686-693`).
   sprig calls it as a subprocess (`harness/sprig/daisugi_gate.go:66-70`), with a
   30 s timeout in `sprig-hook`/`sprig-mcp` but a hard-coded 10 s in `sprig`
   (`harness/sprig/cli.go:76`). sprig writes no transcript or session file.
6. **The `<failed_attempts>` XML is instructor's** (`instructor/core/exceptions.py:71-99`),
   surfaced because `decomposer.py:212-215` embeds `str(e)` and `cli.py:2429-2432`
   prints it verbatim. `DecompositionError` (`decomposer.py:65`) is not an
   `OpenDaisugiError`.
7. **Output is greenfield.** No `NO_COLOR`, no `--plain`, no `-q`/`-v`, no central
   output helper, `isatty` in two places (`approval.py:151`, `dashboard.py:282`).
   `--json` on 25 of 66 commands under two parameter names (`json_output`, `as_json`).
8. **The TUI is one screen.** `tui.py` has one `App`, no `Screen` subclasses, seven
   bindings, and `compose()` renders the wiring map. Tests never press keys.
9. **`--llm` auto-detection works.** The Typer default `"litellm"` does not set the env
   var (`cli.py:2411-2412`), so `_auto_backend()` (`llm.py:72-78`) is reached. The only
   gap: an explicit `--llm litellm` cannot be told from the default.

---

## 4. Plan 1: errors that teach (W9 finish + W6)

### 4.1 Design

- `llm.translate_llm_error` flattens an instructor retry exception into one line:
  the first attempt's exception class and message, then `(N attempts)`. Keys are
  redacted as today.
- `llm.preflight(backend)` runs before any LLM call. For `litellm` it requires
  `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN`; for `claude-code` it requires
  `claude` on `PATH`. It raises `LLMNotConfigured(OpenDaisugiError)` whose message is
  the three-part error. Deterministic errors are never retried because the call is
  never made.
- `DecompositionError` moves under `OpenDaisugiError`. A new subclass `NoStepsError`
  marks "decomposition produced no steps". The CLI turns it into a plain answer and
  exit 0: the prompt has no steps to run, and where to take a plain question.
- `--llm` defaults to `None` on `orchestrate`, `onboard`, `generate-envelope`, and
  `journal parse`. An explicit value sets the env var; `None` leaves auto-detection.
- Every command that spends tokens echoes the resolved state once, on stderr:
  `backend: claude-code · gate: shadow · data: ~/.opendaisugi`. The embedder logs its
  device once when it loads, in `_search._get_model`.
- `daisugi config` prints every `Config` field with its source (`file`, `default`) plus
  the derived lines `llm_backend` (`flag`, `env`, `auto`) and `gate_mode` (`hook`,
  `config`, `default`). `--json` gives the same as an object.
- `cli.main()` wraps `app()`; the console script points at it. `OpenDaisugiError`
  becomes one three-part message and exit 1; `DAISUGI_DEBUG=1` or `-v` shows the
  traceback. Typer's pretty exceptions are off.
- Error audit: the `orchestrate`, `run`, `verify`, and `tend` failure branches use a
  `_fail(what, why, fix, code)` helper. The gate's "no envelope" message already has
  the shape; keep it.

### 4.2 Acceptance

- `OPENDAISUGI_LLM_BACKEND=litellm daisugi orchestrate "x"` with no key: exit 1, stderr
  is three lines, contains `ANTHROPIC_API_KEY` and `--llm claude-code`, no `<failed_attempts>`,
  no `Traceback`.
- `daisugi orchestrate "Hello how are you doing?"` (mocked decomposer returning zero
  steps): exit 0, one plain sentence, no exception name.
- `daisugi config` lists `gate_mode`, `llm_backend`, and `data_dir` with sources.
- `tests/test_cli_errors.py` passes; the suite stays green.

---

## 5. Plan 2: CLI basics (W1 + W2)

### 5.1 Design

- `console.py` is the one output layer. `OutputMode(color, plain, quiet, verbose, json)`;
  `resolve_output(...)` reads `--plain`, `-q`, `-v`, `--no-color`, `NO_COLOR`,
  `TERM=dumb`, and `stdout.isatty()`. `say()` writes results to stdout; `note()` writes
  progress to stderr and is silent under `-q`; `warn()` and `fail()` write to stderr.
  `style()` returns plain text when color is off. `step(label)` is a context manager
  that prints `label…` and ` done (1.2 s)` on a TTY and nothing when piped or quiet.
- The root callback owns `--plain`, `-q/--quiet`, `-v/--verbose`, `--no-color`.
  Commands read `console.current()`.
- Box drawing (`modules.render_wiring`, `dashboard.render_dashboard`) has an ASCII
  variant under `plain`. `dashboard.run_live` never writes the clear-screen escape when
  plain or not a TTY.
- `--json` is added to every read command that lacks it: `gate status`, `hook list`,
  `pathways list`, `pathways show`, `registry status`. A conformance test lists the
  commands that must accept `--json` and fails if one drops it.
- Lazy package init. `Daisugi` moves to `facade.py`. `__init__.py` keeps logging setup,
  `DEFAULT_DATA_DIR`, `__version__`, `__all__`, and a `_LAZY` name-to-module map with a
  PEP 562 `__getattr__` that imports on first use and caches. `verify.py` imports
  `check_dag` inside the function so the gate does not pay for networkx.
- Resident gate. `daisugi gate serve` listens on `<root>/gate.sock` (mode 0600, inside
  the 0700 root). `gate_client.py` is stdlib-only: it sends the argv and the raw stdin,
  receives `stdout`, `stderr`, `exit_code`. If the socket is absent, refused, or
  answers with anything but a well-formed reply, the client falls back to the in-process
  `gate.main(argv)`. There is no path where a broken server allows. Install writes
  `python -m opendaisugi.gate_client ...`; the substring `opendaisugi.gate` still
  matches for idempotent installs.
- ADR-0017 records the measured before/after numbers (`## Measured`, like ADR-0016).

### 5.2 Acceptance

- `daisugi --help | cat` contains no `\x1b[`. `daisugi --plain modules` contains no
  box-drawing characters. `NO_COLOR=1` and `TERM=dumb` give the same.
- `python -c "import opendaisugi.cli, sys; assert 'z3' not in sys.modules"` passes.
  `daisugi --help` under 0.2 s warm on this box (recorded, not a test).
- `python -m opendaisugi.gate_client --mode enforce ...` with no server: same verdicts
  and exit codes as `python -m opendaisugi.gate`. With a server: same verdicts, and the
  round trip is under 50 ms after the first call (recorded).
- With a server that returns garbage: enforce mode exits 2.

---

## 6. Plan 3: the session tree (W8 data layer)

### 6.1 Store

One append-only JSONL per session at `<data_dir>/sessions/<session-id>.jsonl`. Every
entry carries `type`, `id` (8 hex chars), `parentId`, `ts` (unix seconds, float).
The first line is the session header; `head` lines carry no `id`.

```
{"type":"session","v":1,"id":"<session-id>","harness":"claude-code|codex|sprig","harnessSessionId":"…","cwd":"…","transcriptPath":"…","parentSession":null,"parentEntry":null,"cacheKey":"…","ts":…}
{"type":"prompt","id":"8hex","parentId":null,"ts":…,"text":"…"}
{"type":"assistant","id":"…","parentId":"…","ts":…,"model":"…","usage":{"fresh":0,"cacheRead":0,"cacheWrite":0,"out":0},"text":"…"}
{"type":"tool_call","id":"…","parentId":"…","ts":…,"toolUseId":"…","name":"Bash","stepType":"shell","detail":"rm -rf build/","agentId":null,"agentType":null}
{"type":"verdict","id":"…","parentId":"…","ts":…,"toolUseId":"…","decision":"allow|deny|ask","mode":"shadow|enforce","reason":"…","clause":"permissions: …","counterexample":{…},"envelopeId":"…","planId":"…","latencyMs":0.6,"answeredBy":null}
{"type":"tool_result","id":"…","parentId":"…","ts":…,"toolUseId":"…","ok":true,"summary":"…"}
{"type":"checkpoint","id":"…","parentId":"…","ts":…,"ref":"refs/daisugi/checkpoints/<session>/<id>","covers":["…"],"skipped":["…"]}
{"type":"compaction","id":"…","parentId":"…","ts":…,"summary":"…","retainedTail":["…"],"tokensBefore":0}
{"type":"branch_summary","id":"…","parentId":"…","ts":…,"fromId":"…","summary":"…"}
{"type":"label","id":"…","parentId":null,"ts":…,"target":"…","label":"…"}
{"type":"note","id":"…","parentId":"…","ts":…,"from":"operator|session:<id>","text":"…"}
{"type":"head","leafId":"…","ts":…}
```

Rules:
- `append()` links the new entry to the current head and moves the head, except for
  `label` and `head` entries. A `note` never carries a verdict.
- `fork(at)` copies the path root→`at` into a new file whose header carries
  `parentSession` and `parentEntry`; `cacheKey` is inherited.
- Readers ignore a torn last line. Writers append one line per `write()`.
- Nothing is ever deleted.

### 6.2 Join keys and structure

- `_payload_to_record` keeps `tool_use_id`, `agent_id`, `agent_type`, `cwd`,
  `transcript_path`, `hook_event_name` when present. `_log_shadow` writes them.
- `GateDecision` gains `violations: list[dict]`, `envelope_id`, `plan_id`, and a
  `clause` property (`"<stage>: <message>"` of the first violation, or the reason).
- The gate appends `tool_call` and `verdict` entries to `SessionTree` for the
  payload's session, best-effort, after the decision is final. A write failure never
  changes a verdict.
- `parsers/claude_code._read_messages` keeps `uuid`, `parentUuid`, `sessionId` on each
  flat message. `claude_transcript.py` reads a Claude Code JSONL into turns with
  `usage` and the `parentUuid` tree.

### 6.3 Operator ask

- Enforce mode only. `--ask` on the gate entry, `--ask-timeout N` (default 90),
  `gate_ask: bool` in config.
- The operator is present when `<root>/operator.json` exists, its `pid` is alive, and
  its mtime is under 15 s old. The multi-session view writes it every 5 s and removes it
  on exit.
- On a would-deny with `--ask` and a present operator: write
  `<root>/asks/<tool_use_id>.json`, then poll `<root>/answers/<tool_use_id>.json`
  every 0.2 s until the deadline. `{"decision":"allow","reason":"…"}` → allow.
  Anything else, a timeout, or a missing operator → deny with the reason.
- `gate_settings_json(ask=True, ask_timeout_s=N)` writes `timeout = N + verify + 5`.
- `remember` writes `<root>/proposals/<id>.json` with a scope and expiry. Nothing
  applies a proposal automatically.

### 6.4 sprig (path E)

- `harness/sprig/session_tree.go` writes the same JSONL: `prompt`, `assistant` (with
  the API usage fields), `tool_call`, `verdict` (from the in-process gate), `tool_result`.
  Enabled with `--session-dir <dir>`; the session id is a UUID unless `--session <id>`.
- `sprig --resume <id>` rebuilds the messages from the head path of that file and
  continues. Fork is `SessionTree.fork` on the Python side followed by `--resume` of
  the child.

### 6.5 Workspace checkpoints (opt-in)

- `checkpoints.py`: `snapshot(repo, session, entry_id) -> Checkpoint` writes a commit
  object under `refs/daisugi/checkpoints/<session>/<id>` using a temporary index
  (`GIT_INDEX_FILE`), never touching the user's index or HEAD. `covers` lists the paths
  captured; `skipped` lists ignored and oversize paths. `restore(repo, ref)` first
  snapshots the current state to `refs/daisugi/rollback/<session>/<id>`, then
  `read-tree` + `checkout-index -a -f`.
- The gate takes a snapshot at each new prompt boundary when `--checkpoints` is set
  (off by default). The boundary is detected from the transcript's last user prompt
  uuid.

### 6.6 Acceptance

- Tree round trip: create, append 5 entries, fork at entry 3, `path_to` on the child
  returns 3 entries plus the header; head persists across reopen.
- After one `gate_and_contract` with a full Claude payload, `sessions/<id>.jsonl`
  has the header, one `tool_call`, one `verdict` with `clause` and `toolUseId`.
- Ask flow: with an operator file and an answer written by a thread, allow; without an
  operator, deny within 0.5 s; with an operator and no answer, deny at the deadline.
- `go test ./...` in `harness/sprig` passes with the new writer tests.

---

## 7. Plan 4: the multi-session view (W8 + W5 + W7)

### 7.1 Screens

`DaisugiApp` has three screens, cycled with Tab: `sessions` (default), `tree`, `wiring`.
Each has `:` (command line) and `/` (filter). The footer is generated from the live
bindings; `?` shows the full list. The header line is:

```
 daisugi · sessions      gate: ENFORCE   cache 96% hit   7 sessions   ?=keys
```

`gate:` shows `ENFORCE`, `SHADOW`, or `DISARMED` (from `is_disarmed` and the installed
hook's `--mode`, falling back to config). `cache` comes from the transcripts' usage
fields of the live sessions; when there are none it shows the gateway journal's rate
and says `(gateway)`. Estimated numbers carry `?`.

### 7.2 Sessions screen

Rows grouped by what the operator must do: `NEEDS YOU`, `WORKING` (last entry under
60 s), `PARKED` (under 1 h), `DONE` (older). One row per session: id or agent, last
proposed action, verdict + clause, step count, `↑fresh ⟳cacheRead ✎cacheWrite`, seconds
since the last entry. The peek pane shows the proposal, verdict, clause, counterexample,
envelope id, and latency for the selected row.

Keys: `j/k` move, `Space` tag, `a` allow (then `Enter` to confirm; the status line says
so), `d` deny, `e` edit input (opens the input for editing; writes the answer with
`updatedInput`), `r` remember (writes a proposal), `t` tree, `Enter` attach. On path D,
attach shows the exact `claude --resume <id>` command and copies it; steer is shown as
`not on this path`. Alerts: a strip with counts by class; a per-class policy file
`<data_dir>/alerts.yaml` (log / pause / modal) with defaults that pause on denials of
destructive classes and on budget breaches.

Refresh: a 1 s poll of `sessions/` and `asks/` mtimes; rows are updated in place
(`DataTable.update_cell`), never rebuilt, so nothing reflows under the eye.

### 7.3 Tree screen

A Textual `Tree` over one session: for sprig sessions from `SessionTree`, for Claude
sessions from `claude_transcript.read_turns`. `Ctrl+O` cycles filters (all → no tools →
prompts only → labeled → verdicts only). `L` labels. `Enter` on a prompt puts its text
in the input box for edit-and-resend (path E) or shows the fork command (path D).
`Enter` elsewhere opens the rewind menu: Restore conversation / Restore workspace /
Restore both / Fork here / Never mind. Workspace rows appear only when a checkpoint
exists; the menu states what will not be restored (`skipped`).

### 7.4 Wiring screen

Today's `compose()` moves here unchanged in behaviour: the same buttons, the same
`[live]/[cfg]/[planned]` tags, the same swap tests. It is reachable by `:wiring` and
Tab, never on start.

### 7.5 Acceptance

- The app boots on the sessions screen; `:wiring` and Tab reach the wiring screen; the
  existing swap tests pass on it.
- With two seeded sessions (one with a pending ask), the roster shows `NEEDS YOU (1)`
  above `WORKING (1)`; `a` then `Enter` writes `answers/<tool_use_id>.json` with
  `decision: allow`; `a` then `k` does not.
- The header shows `gate: ENFORCE` when the installed hook says so, `SHADOW` otherwise,
  and `DISARMED` when the marker file exists.
- `operator.json` exists while the app runs and is gone after exit.

---

## 8. Plan 5: one way in (W4 + W3)

### 8.1 `daisugi start`

One command that gets a shadow-mode gate over the current directory and opens the
multi-session view:

1. Detect the harness: `claude` on `PATH` → Claude Code; else say what was looked for.
2. Install the gate hook in shadow mode if absent (`install --gate`); say what changed.
3. Register a starter envelope for the cwd if none is registered (`starter_envelope`).
4. Open the TUI (`dashboard --tui`) or, without the extra, the stdlib live view.

Flags: `--enforce`, `--no-ui`, `--dry-run` (print the four steps and their current
state, change nothing). Every step prints one line: done, skipped (why), or failed
(what, why, next action).

### 8.2 Command surface

- The bare run prints a short "start here": one sentence, five tasks with example
  invocations, and `daisugi help --all`. Under 25 lines.
- Visible top level: `start`, `status`, `dashboard`, `orchestrate`, `install`, `config`,
  `gate`, `pathways`, `journal`, `help`. Everything else is `hidden=True` and listed by
  `daisugi help --all` under panels (Run, Gate, Garden, Ops, Dev). Did-you-mean keeps
  working for hidden commands (click considers all commands).
- `quickstart` and `gate quickstart` are removed; their orientation text lives in
  `start --dry-run`. `setup` becomes `tiers setup`. `onboard` stays top level, hidden.
  No alias period: the repo has one real user and clean breaks are the house rule.
- The stale module docstring at `cli.py:1-15` is rewritten to the truth.

### 8.3 Acceptance

- `daisugi` prints under 25 lines, contains `daisugi start`, and no box drawing when
  piped. `daisugi help --all` lists every command.
- `daisugi start --dry-run` on a temp home lists four steps, changes nothing.
- `daisugi quickstart` is unknown and suggests `start`.

---

## 9. Shared interfaces (the names every plan uses)

```python
# console.py (plan 2)
@dataclass(frozen=True)
class OutputMode: color: bool; plain: bool; quiet: bool; verbose: bool; json: bool
def resolve_output(*, plain: bool, quiet: bool, verbose: bool, no_color: bool,
                   stream=None, env=None) -> OutputMode
def set_mode(mode: OutputMode) -> None
def current() -> OutputMode
def say(text: str) -> None
def note(text: str) -> None
def warn(text: str) -> None
def style(text: str, color: str) -> str
@contextmanager
def step(label: str): ...

# exceptions.py (plan 1)
class LLMNotConfigured(OpenDaisugiError): ...
class DecompositionError(OpenDaisugiError): plan: ActionPlan | None
class NoStepsError(DecompositionError): ...

# llm.py (plan 1)
def preflight(backend: str) -> None            # raises LLMNotConfigured
def translate_llm_error(exc) -> EnvelopeGenerationError

# config.py (plan 1)
@dataclass(frozen=True)
class ResolvedField: key: str; value: str; source: str   # file|default|flag|env|auto|hook
def resolved_config(path: Path | None = None, *, home: Path | None = None) -> list[ResolvedField]

# gate.py (plan 3)
@dataclass
class GateDecision:
    ... existing fields ...
    violations: list[dict] = field(default_factory=list)
    envelope_id: str | None = None
    plan_id: str | None = None
    ask: bool = False               # True when an operator answered
    @property
    def clause(self) -> str

# session_tree.py (plan 3)
def new_id() -> str                                      # 8 hex chars
@dataclass(frozen=True)
class Entry: type: str; id: str | None; parent_id: str | None; ts: float; data: dict
class SessionTree:
    path: Path
    @classmethod
    def create(cls, sessions_dir: Path, *, session_id: str, harness: str, cwd: str,
               harness_session_id: str | None = None, transcript_path: str | None = None,
               parent_session: str | None = None, parent_entry: str | None = None,
               cache_key: str | None = None) -> "SessionTree"
    @classmethod
    def open(cls, sessions_dir: Path, session_id: str) -> "SessionTree"   # FileNotFoundError
    @classmethod
    def open_or_create(cls, sessions_dir: Path, *, session_id: str, **header) -> "SessionTree"
    def meta(self) -> dict
    def append(self, type: str, data: dict, *, parent_id: str | None = "head") -> Entry
    def entries(self) -> list[Entry]
    def head(self) -> str | None
    def set_head(self, entry_id: str) -> None
    def path_to(self, entry_id: str) -> list[Entry]
    def children(self, entry_id: str | None) -> list[Entry]
    def fork(self, at_entry_id: str, *, new_session_id: str | None = None) -> "SessionTree"
@dataclass(frozen=True)
class SessionSummary: session_id: str; harness: str; cwd: str; last_ts: float;
                      entry_count: int; last_tool_call: dict | None; last_verdict: dict | None
class SessionIndex:
    def __init__(self, sessions_dir: Path) -> None
    def list(self) -> list[SessionSummary]

# claude_transcript.py (plan 3)
@dataclass(frozen=True)
class Turn: uuid: str; parent_uuid: str | None; kind: str; ts: str; model: str | None;
            usage: dict; text: str; tool_uses: list[dict]
def read_turns(path: Path) -> list[Turn]
def usage_totals(turns: list[Turn]) -> dict      # fresh, cacheRead, cacheWrite, out
def last_prompt_uuid(turns: list[Turn]) -> str | None

# ask.py (plan 3)
def operator_present(root: Path, *, now: float | None = None, max_age_s: float = 15.0) -> bool
def write_presence(root: Path, *, pid: int | None = None) -> Path
def clear_presence(root: Path) -> None
def post_ask(root: Path, *, tool_use_id: str, question: dict, deadline: float) -> Path
def wait_answer(root: Path, *, tool_use_id: str, deadline: float, poll_s: float = 0.2,
                sleep=time.sleep, clock=time.monotonic) -> dict | None
def answer(root: Path, *, tool_use_id: str, decision: str, reason: str = "",
           updated_input: dict | None = None) -> Path
def pending_asks(root: Path) -> list[dict]

# cockpit.py (plan 4)
@dataclass(frozen=True)
class SessionRow: session_id: str; group: str; agent: str; action: str; verdict: str;
                  clause: str; steps: int; fresh: int; cache_read: int; cache_write: int;
                  age_s: float; pending_ask: dict | None; harness: str
@dataclass(frozen=True)
class Roster: rows: list[SessionRow]; counts: dict[str, int]
def build_roster(data_dir: Path, *, now: float | None = None) -> Roster
@dataclass(frozen=True)
class HeaderState: gate_mode: str; gate_mode_source: str; cache_hit_rate: float | None;
                   cache_source: str; session_count: int
def header_state(data_dir: Path, *, home: Path | None = None) -> HeaderState

# start.py (plan 5)
@dataclass(frozen=True)
class StartStep: key: str; state: str; text: str      # state: done|skipped|failed|would
def plan_start(cwd: Path, *, home: Path, data_dir: Path, enforce: bool) -> list[StartStep]
def run_start(cwd: Path, *, home: Path, data_dir: Path, enforce: bool, dry_run: bool) -> list[StartStep]
```

---

## 10. What these plans do not do

- No garden screen. The header's cache number is the only garden fact shown.
- No keep-warm pings on the subscription path (Claude Code owns those requests).
- No `hookSpecificOutput` deny on the Claude path; deny stays exit 2 + stderr, which is
  the tested contract. `updatedInput` on an allow is emitted only when an operator
  edits, and its live behaviour must be checked against Claude Code by hand once.
- No Codex or Hermes session trees. Their hook payloads lack `tool_use_id`; the
  capture path records them as before.
- No XDG move of `~/.opendaisugi`.

---

## 11. Adversarial-review corrections (2026-08-27, authoritative)

Five plans were each put through an independent adversarial review that verified every
load-bearing `path:line` claim against the real source. The reviews confirmed the
designs are sound and the fail-closed cores hold, and they found concrete, fixable
defects. **Where this section or a plan's own "Corrections" block disagrees with an
older task body, the correction governs.** §9 below is a *non-authoritative summary*;
where §9 disagrees with a plan body, the plan body governs.

### 11.1 Cross-plan rules every plan now inherits

1. **Full suite before every commit, not a subset.** Each task's final verify step runs
   `uv run pytest -q` (the whole suite) and `uv run ruff check .`, not the two or three
   files the task touches. Rationale: plan 1's `preflight` lands in `get_instructor_client`
   — a choke point for nine modules — and would commit "green" against its own three test
   files while breaking `envelope`/`distiller`/`tier1`/`synthesizer`. A task that changes a
   shared function audits its callers and runs the whole suite.
2. **A test that only goes green in a later task is `@pytest.mark.xfail(strict=True,
   reason="green at Task N")`.** `strict=True` flips it to a failure the moment Task N lands,
   so the TDD signal is preserved and no task ever commits a red suite. (Fixes plan 5 B-3,
   plan 1's cross-task order, any similar case.)
3. **Declare cross-plan dependencies in the plan header; never dispatch a plan in
   isolation ahead of its dependencies.** Build order is 1→2→3→4→5. Plans 2–5 consume
   plan-1 symbols (`config.installed_hook_mode`, the `config` command, `_echo_resolved`);
   plans 4–5 consume plan-2 (`gate_server.SOCK_NAME`, the resident gate, the root output
   flags) and plan-3 (`_patch_claude_gate(ask=)`, `gate_settings_json(ask=)`). A coder who
   runs a later plan against today's `src/` hits ImportError at collection. Each plan's
   Corrections block states its "Requires:" line.
4. **Fail-closed means "malicious well-formed input denies," not only "broken input
   denies."** The reviews confirmed every *broken* path already denies. Two same-UID gaps
   remain, and both are cheap to close (the safe fallback is always available):
   - Plan 2 resident gate: the client trusts any well-formed reply, and the "0600 socket
     in a 0700 root" invariant is asserted but never enforced. Server `os.chmod(root,0o700)`
     after mkdir; client `os.lstat` the socket and fall back unless it is a socket owned by
     the current uid, mode 0600, not a symlink.
   - Plan 3 operator-ask: an `answers/<id>.json` is honored by existence alone and checked
     before the deadline, so a stale/pre-planted answer is accepted, and nothing is GC'd.
     `post_ask` writes a nonce; the answer must echo it; reject an answer older than its ask;
     delete the answer after consuming; sweep expired asks.
5. **On this box, `FORCE_COLOR=3` (and `COLORTERM=truecolor`) are set.** Rich honors
   `FORCE_COLOR` over `NO_COLOR`, so `--help` output is colorized and `"--json" in help`
   substring tests fail even when the flag exists. Assert flag presence by Click param
   introspection (`get_command(app)` → walk sub-commands → `"--json" in prm.opts`), and in
   subprocess tests pass an env with `FORCE_COLOR` removed and `NO_COLOR=1`. This governs
   every help-text assertion in plans 2 and 5.
6. **Trust the plans' code, not their line numbers.** Plan 1 edits `cli.py` first, shifting
   every later line reference. Each task already says "read the command first"; the numbers
   are advisory only.

### 11.2 §9 signature reconciliations (the summary was stale)

- `llm.preflight` is `preflight(backend=None, *, model=None, env=None) -> str` and requires a
  key for **litellm Anthropic models only** (the plan's narrowing is the correct behavior).
  §4.1's "unconditionally requires a key for litellm" is superseded by this.
- `ask.wait_answer` takes `timeout_s: float` (relative), not `deadline` — plan 3 implements
  and calls it that way throughout, and `post_ask` keeps the absolute `deadline` for
  `pending_asks`. §9's `deadline` is the stale form.
- `start.plan_start/run_start` take a single `StartOptions` dataclass with one `act` flag
  (dry-run chosen at the CLI). This is safer than §9's `run_start(..., dry_run)` (which would
  scatter the dry-run branch) — keep the plan's shape.
- `cockpit.header_state(data_dir, *, home=None, roster=None)` and
  `cockpit.alerts_for(roster, *, policy=None)` — §9 omits the extra params.
- `SessionSummary`, `Turn`, `SessionIndex`, `GateDecision` in plan 3 are supersets of the §9
  sketches (`harness_session_id`, `transcript_path`, `parent_session`, `tool_result_ids`,
  `last_model`, `mtime`, `updated_input`) — all defined-before-used in build order.
- `config.installed_hook_mode(settings_path) -> str | None` is a plan-1 deliverable (§9
  omits it); plans 2, 4, 5 consume it.

### 11.3 Per-plan readiness at review time

| Plan | State | Gating items |
|---|---|---|
| 1 | Tasks 1–6 ready after small fixes; **Task 7 rewrite** | B3 ruff `as e`; Task-7 tests target the wrong command shape; run full suite in Task 2 |
| 2 | Ready after fixes; no architecture change | B1 `step` sleep; B2 FORCE_COLOR; S1 socket hardening |
| 3 | **Tasks 1–7 ready to build now**; Tasks 8–10 need fixes | B1 `Observer` rename; **B2 destructive `restore()`**; S1 ask nonce; S7 concurrent gate |
| 4 | **Architecture revision first** | B1 per-screen chrome + screen-scoped queries; B2 swap tests; S2 habituation-safe allow; S3 fail-closed edit |
| 5 | Ready after fixes | B1 scope honesty; B2 `gate_server.SOCK_NAME`; B4 daemon liveness; SF5 reconcile visible-count to 10 |

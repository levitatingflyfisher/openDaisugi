# Spec 01 — Floor contracts in Python, and the gate reports state

**Master:** §3.1 PaneStateEvent, §3.2 PaneBackend, §5.1 · **Size:** S · **Depends on:** 00

## Purpose

Create the `opendaisugi.floor` package holding the two contracts every later sub-project builds
on, and make the gate *report state* after each verdict — to coppice-server when a pane is
listening, to Herdr when a Herdr pane is listening, to nobody otherwise. This is the Herdr
integration the operator approved, and it ships before coppice exists so Herdr users get exact
"blocked" state on day one.

## The crux

The gate is the only process that knows a tool call was blocked *and why*. Reporting is a
side effect that runs **after the verdict is final and fully wrapped**, exactly like the tree
write and the ask (cockpit spec §11): a reporting failure never changes a verdict, never delays
it beyond a 200 ms budget, and never raises. That ordering is the whole safety argument for
adding a network side effect to the hook path.

## Files

```
src/opendaisugi/floor/__init__.py       # exports PaneStateEvent, PaneBackend, PaneRef, PaneInfo, Frame
src/opendaisugi/floor/events.py         # dataclass + validation + merge precedence
src/opendaisugi/floor/backend.py        # Protocol + value types (no implementations here)
src/opendaisugi/floor/report.py         # report_state(): coppice socket / herdr CLI / no-op
src/opendaisugi/gate.py                 # call report after _log_tree / _maybe_ask (≈ lines 572–640)
src/opendaisugi/hook.py                 # `--event stop|notification` handling → idle/blocked events
src/opendaisugi/install.py              # `--report herdr` adds Stop + Notification hooks (Claude Code)
tests/floor/test_events.py
tests/floor/test_report.py
tests/test_hook_report_events.py
```

`opendaisugi.floor` is **not** a layer module (spec-00 test excludes it). `gate.py` and `hook.py`
*are* layer modules, so they may not import `opendaisugi.floor`. The report call therefore lives
in the layer as a tiny module `opendaisugi/_state_report.py` (stdlib only, no floor import) that
`floor/report.py` re-exports. Yes, this is one indirection for one rule; the rule is worth it.

## Interfaces

### events.py

```python
STATES = ("idle", "working", "blocked", "done", "unknown")
SOURCES = ("operator", "gate", "headless", "process", "manifest")   # precedence order, highest first

@dataclass(frozen=True)
class Ask: id: str; tool: str; summary: str; deadline: float

@dataclass(frozen=True)
class PaneStateEvent:
    session_id: str; harness: str; state: str; source: str; ts: float
    harness_session_id: str | None = None; pane: str | None = None
    ask: Ask | None = None; detail: str = ""
    v: int = 1
    def to_json(self) -> str
    @classmethod
    def from_json(cls, line: str) -> "PaneStateEvent"     # raises ValueError on any violation of §3.1

def merge(current: PaneStateEvent | None, incoming: PaneStateEvent, *, now: float) -> PaneStateEvent:
    """§3.1 as amended 2026-09-09: a `done` from process/headless is terminal and wins at once;
    a gate `blocked` (which must carry an ask) holds until a gate event clears it or
    ask.deadline < now; the 2 s precedence window holds back only a `manifest` incoming event,
    every other source is a fact and applies at once; `effective_state` reads any expired
    `blocked` as `working`; manifest never yields done."""
```

### backend.py

Exactly the Protocol in master §3.2, plus:

```python
@dataclass(frozen=True) class PaneRef: backend: str; id: str
@dataclass(frozen=True) class PaneInfo: ref: PaneRef; label: str; cwd: str; cmd: list[str]; kind: str; state: PaneStateEvent | None
@dataclass(frozen=True) class Frame: pane: str; seq: int; cols: int; rows: int; cursor: tuple[int,int]; rows_changed: dict[int, list[list]]
```

### _state_report.py (layer) / report.py (floor)

```python
def report_state(
    ev_json: str, *, env: Mapping[str, str] = os.environ, budget_s: float = 0.2
) -> str:
    """Deliver one PaneStateEvent line. Returns which sink took it: 'coppice' | 'herdr' | 'none'.
    coppice: if env COPPICE_SOCK and COPPICE_PANE are set → connect unix socket, send
             {"cmd":"pane.report_state","pane":…,"event":<ev>} with id "r", read one reply line, done.
    herdr:   elif env HERDR_PANE_ID is set (name confirmed in plan task 1; fallback names tried in
             order: HERDR_PANE_ID, HERDR_PANE) → subprocess ['herdr','pane','report-agent',pane,
             '--source','daisugi','--agent',harness,'--state',mapped], timeout=budget_s.
             State map: working→working, blocked→blocked, idle→idle, done→idle, unknown→(skip).
    Never raises. Any failure → 'none'. Logs at DEBUG only."""
```

### gate.py wiring

After the verdict is final (after `_log_tree`, `_maybe_ask`, `_maybe_checkpoint`):

- allow → `working`, detail `verdict=allow`
- deny (no ask) → `working`, detail `verdict=deny clause=…` (the agent continues; "blocked" means
  *waiting on a human*, not "was denied")
- ask posted → `blocked` with `Ask(id=tool_use_id, tool, summary, deadline)`
- ask resolved (allow or timeout) → `working`

### hook.py

`daisugi hook record --format claude --event stop` → emit `idle`.
`--event notification` (Claude Code's Notification hook: permission prompt or idle prompt) →
`blocked` with `Ask(id="harness", tool=payload.notification_type, summary=payload.message,
deadline=now+90)` when the payload is a permission request, else `idle`.
`session_id` comes from the payload as today; `harness_session_id` = Claude's uuid.

### `daisugi hook report` (used by the pi extension and the OpenCode plugin, specs 04 and 05)

`daisugi hook report [--pane P]` reads one PaneStateEvent JSON line on stdin, validates it with
`from_json`, **downgrades** `source` to `headless` if the caller claims `gate` or `operator`
(only the gate process itself may speak as the gate), appends it to the session tree as a
`state` entry, and calls `report_state`. Exit 0 always once stdin parsed; exit 1 on a malformed
event with the validation message on stderr. Also reachable through `gate.sock` as
`{"argv": ["hook", "report"], "stdin_b64": …}` so an in-process extension needs one socket, not
a subprocess.

### install.py

`daisugi install --gate … --report herdr` (and `--report coppice`, a no-op that only sets
`floor.report: coppice` in config) adds to Claude Code hooks: `Stop` → the stop command above,
`Notification` → the notification command. Idempotent by command substring, same pattern as
the PreToolUse patch at `install.py:459`. Uninstall removes both.

## Behaviour details

- `report_state` runs in the hook process; the 200 ms budget is enforced with socket timeouts and
  `subprocess.run(timeout=)`. A hit budget is `'none'`, not an error.
- The coppice sink is preferred when both env vars are present (a Herdr pane hosting coppice is
  not a supported nesting; first match wins).
- Events also append to the session tree as `type: "state"` entries (via `_log_tree`'s existing
  writer) so the cockpit's roster can read state without a live socket. Keep the tree the source
  of truth for history; the socket is for latency.

## Tests

- `test_events.py`: round-trip; every §3.1 rule as its own test named for the rule
  (`test_manifest_cannot_say_done`, `test_gate_blocked_holds_until_deadline`, …).
- `test_report.py`: fake unix socket server → `'coppice'`; fake `herdr` on PATH (a shell script
  recording argv) → `'herdr'` with the exact argv; both absent → `'none'`; budget exceeded → `'none'`
  in under 0.3 s; env with only `HERDR_PANE` (fallback name) → `'herdr'`.
- `test_hook_report_events.py`: gate allow/deny/ask each emit the right event *after* the tree
  write (assert file order); a raising sink never changes `exit_code`; `--event stop` → idle.
- Plan task 1 is a **discovery task**: run `herdr pane create` (if installed) and record the env
  var name Herdr exports in `report.py`'s docstring; if Herdr is absent, keep the fallback list
  and mark the module docstring "env var name unverified".

## Out of scope

Rendering, backends, the coppice binary. Any Codex Stop-equivalent (Codex hooks are fail-open
class and have no Stop event at the time of writing; the honesty tag stays).

# Spec 03 — The floor in the cockpit, `daisugi coppice`, and three pane backends

**Master:** §3.2, §5.1, §5.5, §7 · **Size:** L · **Depends on:** 01, 02

## Purpose

Give the operator the floor: a screen in the existing cockpit that shows every pane with its
sourced state, renders the selected pane's grid, and lets them prompt, steer, allow, deny, create,
close, and attach. Give scripts and agents the same power through `daisugi coppice …`. Implement
the PaneBackend protocol three times — coppice (native), Herdr, tmux — under one contract suite,
so the floor works on day one even where coppice-server is not installed, and so Herdr stays a
first-class substrate rather than a competitor we pretend not to see.

## The cruxes

- **One protocol, three backends, one test suite.** A backend that cannot pass the contract is
  not a backend. The suite skips a backend that is absent and says which.
- **The cockpit renders, the server owns.** The floor screen holds no state that the server does
  not; closing the TUI loses nothing.
- **Notify on block is a config'd command, not a service.** `notify_cmd` in config runs with the
  event JSON on stdin. `ntfy publish …` is the documented example. The floor never phones home.

## Files

```
src/opendaisugi/floor/coppice_backend.py   # socket client (stdlib socket + json), frames, subscribe
src/opendaisugi/floor/herdr_backend.py     # `herdr … --json` subprocess client; poll for state at 1 s
src/opendaisugi/floor/tmux_backend.py      # tmux CLI client; manifests for state
src/opendaisugi/floor/manifests.py         # Python evaluator for the Herdr TOML schema (tomllib)
src/opendaisugi/floor/registry.py          # pick_backend(config) → first available in [coppice, herdr, tmux]
src/opendaisugi/floor/notify.py            # run notify_cmd with event JSON on stdin; debounce 5 s per pane
src/opendaisugi/tui_floor.py               # FloorScreen
src/opendaisugi/tui_grid.py                # GridWidget: paints Frame cells with Rich styles
src/opendaisugi/cli.py                     # `coppice` command group; `dashboard --tui` gains the floor view
src/opendaisugi/config.py                  # floor: {backend: auto|coppice|herdr|tmux, notify_cmd: str|None}
src/opendaisugi/modules.py                 # new "floor" stage with the three backends, honest tags
src/opendaisugi/swap.py                    # STAGE_EFFECT["floor"] = LIVE; knob = backend
tests/floor/test_backend_contract.py       # parameterised over backends; skips when absent
tests/floor/test_manifests.py              # shares fixtures with harness/coppice/testdata/screens
tests/floor/test_coppice_backend.py        # against a fake JSONL server (Python) and, if built, the real binary
tests/floor/test_tmux_backend.py           # real tmux with a private socket (-L) when tmux is on PATH
tests/floor/test_herdr_backend.py          # fake `herdr` script recording argv; real herdr if present
tests/test_tui_floor.py                    # Textual pilot: keys do what the ramp says
```

## Interfaces

### registry.py

```python
def pick_backend(config: Config) -> PaneBackend:
    """config.floor.backend == 'auto' → first of coppice, herdr, tmux with available() True;
    an explicit name that is not available raises FloorNotAvailable teaching the install command."""
```

### coppice_backend.py

Stdlib only. `available()` = socket path exists and `server.status` answers within 300 ms; if the
socket is absent but the `coppice` binary is on PATH, `available()` starts the server once
(`coppice server start`) and retries. `subscribe()` opens a second connection with
`events.subscribe` and yields `PaneStateEvent | Frame`. `spawn()` = `pane.create`. Frames are
delivered as they arrive; the widget coalesces.

### herdr_backend.py

`available()` = `herdr session list --json` exits 0 within 500 ms. `spawn()`: Herdr's CLI
reference (fetched 2026-09-08) has **no `pane create`**; panes come from `tab create` or
`pane split`, then `pane run <id> <cmd>`, then `pane rename`. `--json` is documented only on a
short list of verbs (`session list`, `agent explain`, …), not on `pane list` or `agent list`.
The plan therefore pins the verb chain and the `--json` ladder in
`src/opendaisugi/floor/herdr_verbs.json` (with a `verified` flag per entry), and the backend reads
the pin and falls back to text-table parsing where `--json` is not documented.
`read()` = `herdr pane read <id> --source visible|recent|detection`. State: `herdr agent list`
polled every 1 s in `subscribe()`; Herdr's `working|blocked|done|idle|unknown` map 1:1; `source`
is `gate` if Herdr's status authority names `daisugi` (their explain output), else `manifest`.
`report_state()` = `herdr pane report-agent …` (same argv as spec-01).

### tmux_backend.py

Uses the user's default tmux server unless config sets `floor.tmux_socket`. `spawn()` =
`tmux new-window -d -c <cwd> -P -F '#{pane_id}' -n <label> -e COPPICE_PANE=… <cmd>` (tmux ≥ 3.2
for `-e`; older tmux → env exported via a wrapper `env K=V cmd`). `send_text` = `send-keys -l`
then `Enter`; `send_keys` maps our names (`enter`, `ctrl+c`, `esc`, `tab`, `f1`) to tmux key
names. `read visible` = `capture-pane -p`; `recent` = `capture-pane -p -S -200`; `detection` =
last 12 lines of `-J` (joined) output. State in `subscribe()`: every 500 ms, for each pane, if a
gate event for that pane arrived in the last 2 s (from the session tree's `state` entries,
spec-01) use it; else evaluate manifests on `read(detection)` with `pane_current_command` as the
agent hint. `resize` is a no-op that returns (tmux owns layout); documented.

### manifests.py

```python
def load_manifests(dirs: Sequence[Path]) -> dict[str, Manifest]     # later dirs override by agent name
def classify(screen_tail: str, *, title: str = "", osc_progress: str | None, agent_hint: str | None,
             manifests: dict[str, Manifest]) -> tuple[str, str | None]  # (state, matched_rule_name)
```

The schema is whatever Herdr's files use; plan task 1 reads the vendored files under
`harness/coppice/internal/detect/manifests/` and documents the fields in the module docstring.
Python and Go evaluate the same fixtures under `harness/coppice/testdata/screens/`; a test in
each language asserts the same `(state, rule)` for every fixture. Two implementations of one
format, one fixture set — the conformance pattern this repo already uses for the verifier.

### tui_floor.py — FloorScreen

Layout: left, a DataTable roster (pane id, label, harness, state chip, source, age); right top, the
GridWidget for the selected pane; right bottom, the peek pane (last verdict, pending ask, detail).
Bindings printed in the footer ramp (tool-interface-design law 6): `c` create · `p` prompt ·
`s` steer · `a` allow · `d` deny · `x` close · `Enter` attach full-screen · `n`/`N` next/prev ·
`r` read recent · `?` help · `:` command. Allow/deny reuse `tui_sessions.py`'s ask actions
unchanged (the strong-guard rule for destructive asks stays). Create opens the command line
prefilled `create --cwd <cwd of selected> -- claude`. Prompt sends `send_text`; on a headless pane
it is `agent.prompt`. When state flips to `blocked`, the row flashes once, the app bell rings,
and `notify.py` runs `notify_cmd` if set (debounced).

`GridWidget` paints the last Frame: one Rich `Text` per row, cells' fg/bg/attrs mapped to Rich
styles; cursor drawn as reverse video; resize sends `pane.resize`. Frames arrive on a worker
thread and are posted as messages; the widget repaints at most every 16 ms.

Attach full-screen (`Enter`) = the same widget maximised with keyboard passthrough: printable
keys and the mapped special keys go to `send_keys`; `ctrl+a` is the leader to return. This is
where the operator lives in Claude Code's own UI inside coppice, so it must not eat keys the
harness needs; the leader is the only intercepted chord.

### `daisugi coppice` command group

```
daisugi coppice spawn --cwd DIR [--label L] [--kind pty|headless] [--harness H] -- ARGV…
daisugi coppice list [--json]
daisugi coppice prompt PANE TEXT [--wait] [--timeout S]
daisugi coppice wait PANE [--until STATE] [--timeout S]
daisugi coppice read PANE [--source visible|recent|detection]
daisugi coppice send-keys PANE KEY…
daisugi coppice close PANE
daisugi coppice attach [PANE]              # execs `coppice attach` when the backend is coppice, else teaches
daisugi coppice backends                   # table: name, available, why-not
```

All commands pick the backend through `registry.pick_backend`; `--backend` overrides. Output is
human on a TTY, JSON with `--json`, exit 3 when no backend is available (with the install hint).

## Tests

- Contract suite: `spawn` a `sh -c 'echo READY; sleep 2'` pane, `read visible` contains `READY`
  within 1 s, `send_text('exit')` then state reaches `done` (or the pane disappears on tmux/herdr,
  which the contract accepts as `done`), `close` removes it from `list`. Parameterised over the
  three backends; each skips with its reason when `available()` is false.
- Manifests: the shared fixture set; Go/Python agreement test.
- tmux backend: private server `tmux -L daisugi-test`, torn down after.
- FloorScreen: Textual pilot presses `c`, `p`, `a`, `Enter`, `ctrl+a`; asserts the calls reaching
  a fake backend; a `blocked` event triggers exactly one `notify_cmd` run within 5 s.
- CLI: `daisugi coppice backends` with nothing installed prints three rows of `no` with reasons
  and exits 0; `spawn` with nothing installed exits 3 and names `coppice server start`.

## Out of scope

Multi-pane split layout in the TUI (a later pass); the PWA (spec-06); voice (spec-07).

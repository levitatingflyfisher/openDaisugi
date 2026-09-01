# Part 1 plan: the floor people love

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** coppice becomes one screen where the owner sees and drives many agents across projects, with every fix from the 2026-09-24 study.

**Architecture:** Server first (records, facts, projects), then the web floor, the phone, the TUI, voice and the foreman. Each task is test-first. The web client stays framework-free ES modules tested with `node --test` through `internal/web/js_test.go`. The TUI stays in `internal/tui`.

**Tech Stack:** Go (coppice server, CLI, TUI), vanilla ES modules and canvas (web), Python (the daisugi gate hook).

**Spec:** `docs/plans/2026-09-24-omarchy/spec-part1-floor.md`

## Global Constraints

- Tests never run a real claude, codex, pi, opencode or sprig, never own a TTY, use loopback only, CPU only.
- Go tests run with `go test -p 1`. Python runs with `uv run --no-sync`; never `uv sync`.
- No framework, bundler or external script in the web client. No new Go or Python dependency.
- Comments never cite plans, tasks or specs. Plain STE English in comments, docs and UI text: short sentences, no em dashes, never the word "seam".
- Fail closed: a fact the server does not know is absent, never guessed.
- `PROTOCOL.md` and `README.md` change in the same commit as the behaviour they describe.
- The Python floor backend (`src/opendaisugi/floor/coppice_backend.py`) and its tests keep passing: `uv run --no-sync pytest -q tests/floor`.
- Commit by explicit path. No attribution lines. Never commit CLAUDE.md, docs/superpowers/ or uv.lock.
- Code map of the floor: `~/opendaisugi-scratch/coppice-ui-map.md`.

---

### Task 1: Ended agents leave the floor (server and CLI)

Spec: S1, server and CLI parts only.

**Files:** `internal/layout/layout.go`, `internal/server/panes.go`, `internal/server/restart.go`, `internal/server/agents.go`, new `internal/server/ended.go`, `internal/cli/*.go` for the verbs, `PROTOCOL.md`, `README.md`, tests beside each.

- [ ] `layout.Pane` gains `EndedAt` (unix seconds). Closing a pane on its own or by restart sets it. An operator `pane.close` removes the record instead.
- [ ] `pane.list` and `agent.list` return live records only. `ended: true` returns only ended records, newest first, with `ended_at` and `exit_code`.
- [ ] `pane.forget {pane}` and `pane.forget {ended: true}`. A live pane is `bad_request`.
- [ ] Records ended more than seven days ago are removed at start and hourly.
- [ ] `pane.resume {pane}` per S1, with `resume_args` in the harness table. The reply carries the new `pane` and `resumed`.
- [ ] `server.status` gains `resumable` count.
- [ ] CLI verbs: `pane forget PANE`, `pane forget --ended`, `pane list --ended`, `pane resume PANE`.
- [ ] Tests: each rule above, including a restart that leaves a pty pane ended, and an operator close that leaves no record.
- [ ] The Python backend and `tests/floor` still pass. Where a Python caller relied on closed rows, it asks with `ended: true`.

### Task 2: What the floor knows about each agent (server and gate hook)

Spec: S2.

**Files:** `internal/proto/state_event.go`, `internal/server/*.go`, new `internal/server/facts.go` (transcript and session-tree tailing), `src/opendaisugi/gate.py` and `src/opendaisugi/hook.py` where they report state to coppice, `PROTOCOL.md`, tests beside each.

- [ ] State reports accept optional `transcript_path`, `verdict {decision, tool, clause}` and `mode`. The server keeps the last verdict and mode per pane.
- [ ] A tailer reads new lines of a reported Claude transcript and a sprig session tree, and keeps `model` and summed `tokens`. It reads only paths the pane itself reported, and only the new bytes since its last read.
- [ ] `stack.router` from the pane's env against the configured gateway address.
- [ ] `pane.list` rows carry `stack`, `gate` and `tokens` when known.
- [ ] `floor.facts` returns the floor-wide header facts from S2.
- [ ] The daisugi gate hook sends `transcript_path`, `verdict` and `mode` in its coppice reports. Python tests cover the payload.
- [ ] Go tests use fixture transcripts with fake content under `testdata/`, never a real one.
- [ ] The harness session id a pty pane's own hook reports (Claude's `session_id`) is stored as the record's `HarnessSessionID`, so `pane.resume` can resume it. The first-run config and the README example for `[harness.claude]` gain `resume_args = ["--resume", "{session}"]`.

### Task 3: One click, a new agent (server and CLI)

Spec: S3, server and CLI parts.

**Files:** `internal/server/panes.go`, new `internal/server/projects.go`, `internal/config/config.go`, `internal/cli/*.go`, `PROTOCOL.md`, `README.md`, tests.

- [ ] `pane.create` with no cwd and no argv: default harness, cwd by the S3 order, label by the S3 rule. `near` names a pane.
- [ ] `projects` in `coppice.toml`; recent directories kept in the data dir; `project.list`.
- [ ] `pane.rename {pane, label}`.
- [ ] CLI: `project add|list|rm`, `coppice new [PROJECT]`.

### Task 4a: The one screen, desktop web: layout, windows, keys, readable text

Spec: S4 layout, start, keys, readable; S6.

**Files:** `internal/web/static/app.js`, `floor.js`, `tiles.js`, `grid.js`, `app.css`, `index.html`, `_tests/*`.

- [ ] Windows fill the area at a readable size; count by size, not a fixed three.
- [ ] Start fill order and start focus per S4.
- [ ] Keys go to the selected window; border and "typing here"; ctrl-space leaves; rail keys after leave.
- [ ] Ended agents never fill a window; a window whose agent ends empties and shows the S1 line; no attach retry loop.
- [ ] Canvas at device pixel ratio; cell size from measured font advance; never below the readable size.
- [ ] node tests for fill order, start focus, key routing and cell sizing.

### Task 4b: The rail: tree, Recent, kill, drag, numbers, New and projects (web)

Spec: S1 web parts, S3 web parts, S4 rail and put-in-a-window.

**Files:** `roster.js`, `floor.js`, `newpane.js`, `app.css`, `index.html`, `_tests/*`.

- [ ] Rail is the task tree with folding; project grouping with no tasks.
- [ ] Row shows its window number; × kill with one confirm; drag row onto window; number keys.
- [ ] Recent fold with Resume, Forget, Resume all, Clear all.
- [ ] New button: one click near the selected agent; project menu; the form behind "more". Rename in place.

### Task 4c: Stack bar, gate mark, header row, overlays, fixes (web)

Spec: S2 display, S4 overlays, S7.

**Files:** new `internal/web/static/stackbar.js`, `floor.js`, `views.js`, `app.js`, `app.css`, `_tests/*`.

- [ ] Stack bar per window: segments loop, daisugi and mode, router, model. Colour is health only; words are the choice; outline marks the open segment; the open segment shows its details and its swap control where S2 allows one.
- [ ] Gate mark with the last verdict and its clause.
- [ ] Header row with the floor-wide facts.
- [ ] Views and the journal open as overlays; Esc closes; the colony can pin as a strip.
- [ ] Every message has an action; the scan test from S7.

### Task 5: The phone (web)

Spec: S4 phone.

**Files:** `dock.js`, `floor.js`, `pane.js`, `app.css`, `_tests/*`.

- [ ] Overview home; agent slides in from the right; bottom bar with prompt, mic and key drawer; swipe or edge tap goes back; no navigation away.

### Task 6: The one screen, TUI

Spec: S1, S3 keys, S4 for the TUI, S2 as one line per window.

**Files:** `internal/tui/*`.

- [ ] Start focus and fill per S4; hardware cursor at the selected agent's cursor.
- [ ] Ended agents leave; the S1 line; returning to the floor when an attached agent ends.
- [ ] `n`, `N`, number keys, ctrl-w kill with confirm, Recent fold.
- [ ] Window count by readable width; one stack line per window header.

### Task 7: Voice with no setup

Spec: S5.

**Files:** `internal/server/voice.go` (new), `internal/config/config.go`, `internal/web/voice.go`, `internal/tui/*`, tests.

### Task 8: The foreman

Spec: S8.

**Files:** `internal/server/*`, `internal/web/static/newpane.js`, `internal/tui/*`, the foreman skill page under `skills/`.

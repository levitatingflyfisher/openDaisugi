# Plan 12: Honest State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live pane never reports `unknown`; a failed spawn leaves no ghost; attach fails loudly or shows a screen; the server resolves harness binaries on PATH; status prints an integer pid.

**Architecture:** The PTY watcher becomes a state source of its own (`process`), so every live pane always has a fact: `working` when bytes arrived in the last five seconds, `idle` with a `quiet_for` age otherwise. Pane creation is made transactional: the tree record is removed when `startPane` fails. Attach on a non-terminal exits 1 with one line. Harness commands resolve through `exec.LookPath` at spawn.

**Tech Stack:** Go 1.26 (`harness/coppice`), Python 3.12 client (`src/opendaisugi/floor`), pytest, `go test`.

**Spec:** `docs/plans/2026-09-12-porcelain/ROADMAP.md` (plan 12), `docs/research/boards/floor/index.html` (roster view), master spec `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.1 as amended in Task 1.

## Global Constraints

- Go 1.26, module `github.com/opendaisugi/coppice`; `go vet ./...` and `go test -p 1 ./...` clean from `harness/coppice`.
- Python: `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean. Never bare `uv run`.
- Layer purity: nothing under the layer imports `opendaisugi.floor`, `opendaisugi.voice`, or `opendaisugi.coppice`.
- Commits atomic, persona OpenDaisugi Contributors, no AI-authorship attribution lines. Never push.
- Never run a real `claude`, `codex`, `sprig`, or `tailscale` in tests. Never own a TTY in tests. Never download a model.
- Copy in STE100: short sentences, active voice, no em dashes, no parentheticals. Errors teach the next command.
- Exit codes: CLI 0 success, 1 user error, 2 gate deny, 3 unreachable.
- `/tmp` is RAM. Scratch on real disk.
- OOM rule: one implementer plus one reviewer at a time. `go test -p 1`.

---

### Task 1: The process source, and no `unknown` for a live pane

**Files:**
- Modify: `harness/coppice/internal/server/panes.go` (`watchExit`, `pumpAdapter`, `handlePaneList`, `LivePane`)
- Modify: `harness/coppice/internal/proto/state_event.go` (document the amendment)
- Modify: `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.1 (one paragraph)
- Test: `harness/coppice/internal/server/state_process_test.go`

**Interfaces:**
- Consumes: `state.Store.Apply(pane, ev, now)`, `LivePane`.
- Produces: `LivePane.LastOutput float64` (seconds), and `pane.list` rows with `state` in `{working, idle, blocked, done}` for every live pane, `source: "process"` when nothing better exists, and `quiet_for` seconds on every row.

- [ ] **Step 1: Write the failing test**

```go
func TestALivePtyPaneNeverListsAsUnknown(t *testing.T) {
	s := newTestServer(t) // helper already used by panes_test.go
	id := createShellPane(t, s, "sh", "-c", "sleep 30")
	rows := listPanes(t, s)
	row := rowFor(t, rows, id)
	if row["state"] == "unknown" || row["state"] == nil {
		t.Fatalf("live pane listed as unknown: %v", row)
	}
	if row["source"] != "process" {
		t.Fatalf("expected process source, got %v", row["source"])
	}
	if _, ok := row["quiet_for"].(float64); !ok {
		t.Fatalf("quiet_for missing: %v", row)
	}
}

func TestOutputFlipsIdleToWorkingAndBack(t *testing.T) {
	s := newTestServer(t)
	id := createShellPane(t, s, "sh", "-c", "sleep 1; echo hi; sleep 30")
	waitState(t, s, id, "working", 5*time.Second)
	waitState(t, s, id, "idle", 15*time.Second) // quiet window is 5 s
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/server -run 'TestALivePtyPaneNeverListsAsUnknown|TestOutputFlipsIdle' -v`
Expected: FAIL, state is `unknown` and `quiet_for` is missing.

- [ ] **Step 3: Implement**

In `LivePane` add `lastOutput atomic.Int64` (unix nanos). In the PTY output pump, after each read, set `lastOutput`. Add a ticker goroutine per live pty pane, every second, that applies a `process` event:

```go
func (s *Server) processTick(paneID string, lp *LivePane) {
	last := time.Unix(0, lp.lastOutput.Load())
	st := proto.StateIdle
	if time.Since(last) < 5*time.Second {
		st = proto.StateWorking
	}
	s.states.Apply(paneID, proto.PaneStateEvent{
		V: 1, TS: nowSeconds(), Harness: lp.Harness, Pane: paneID,
		State: st, Source: "process",
	}, nowSeconds())
}
```

The `state.Merge` rule already lets `gate`, `headless`, and `operator` win over `process` while their hold lasts, so a blocked pane stays blocked. In `handlePaneList`, replace the `"state": proto.StateUnknown, "source": nil` branch with the effective state from the store and add `"quiet_for": now - lastOutput`. Keep `unknown` only for a headless pane whose adapter reported a parse error, as §3.6 requires.

- [ ] **Step 4: Run the tests and the whole package**

Run: `cd harness/coppice && go test -p 1 ./internal/server ./internal/state`
Expected: PASS.

- [ ] **Step 5: Amend the spec, one paragraph in §3.1**

Add after the `unknown` rule: "A live pty pane always has a `process` source: `working` within five seconds of output, else `idle`. `unknown` is reserved for a headless pane whose event stream failed to parse. Every `pane.list` row carries `quiet_for`, seconds since the last byte of output."

- [ ] **Step 6: Commit**

```bash
git add harness/coppice/internal/server/panes.go harness/coppice/internal/server/state_process_test.go harness/coppice/internal/proto/state_event.go docs/plans/2026-09-08-workshop/00-master-spec.md
git commit -m "coppice: a live pane always has a process source, never unknown"
```

---

### Task 2: A failed spawn leaves no ghost

**Files:**
- Modify: `harness/coppice/internal/server/panes.go` (`handlePaneCreate` after the `startPane` error)
- Test: `harness/coppice/internal/server/ghost_test.go`

**Interfaces:**
- Consumes: `layout.Tree.ClosePane(id, exit)`, `s.removeLive(id)`.
- Produces: `pane.create` on failure returns `spawn_failed` and `pane.list` does not contain the id.

- [ ] **Step 1: Write the failing test**

```go
func TestAFailedSpawnRegistersNoPane(t *testing.T) {
	s := newTestServer(t)
	resp := call(t, s, `{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["/nonexistent/binary"],"label":"ghost","kind":"pty"}`)
	if resp.OK || resp.Error.Code != proto.ErrSpawnFailed {
		t.Fatalf("expected spawn_failed, got %+v", resp)
	}
	rows := listPanes(t, s)
	for _, r := range rows {
		if r["label"] == "ghost" {
			t.Fatalf("ghost pane registered: %v", r)
		}
	}
	if _, err := os.Stat(layoutPath(s.dataDir)); err == nil {
		tree, _ := layout.Load(layoutPath(s.dataDir))
		for _, p := range tree.Panes() {
			if p.Label == "ghost" {
				t.Fatal("ghost pane persisted in layout.json")
			}
		}
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/server -run TestAFailedSpawnRegistersNoPane -v`
Expected: FAIL, the label is listed.

- [ ] **Step 3: Implement**

In `handlePaneCreate`, in the `startPane` error branch before returning:

```go
_ = s.tree.ClosePane(rec.ID, nil)
s.removeLive(rec.ID)
s.states.Forget(rec.ID)
s.saveLayout()
```

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/server`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/panes.go harness/coppice/internal/server/ghost_test.go
git commit -m "coppice: a spawn that fails leaves no pane behind"
```

---

### Task 3: Resolve the harness binary on PATH at spawn

**Files:**
- Modify: `harness/coppice/internal/server/panes.go` (`startPane`, pty branch)
- Modify: `src/opendaisugi/floor/coppice_backend.py` (stop expanding `~/.local/bin/<harness>`; pass the bare command)
- Test: `harness/coppice/internal/server/lookpath_test.go`, `tests/floor/test_coppice_backend.py`

**Interfaces:**
- Produces: `pane.create` with `cmd_argv[0]` lacking a slash resolves through `exec.LookPath`; a miss returns `spawn_failed` with message `"<name> is not on PATH. Install it or give a full path."`.

- [ ] **Step 1: Write the failing tests**

Go:

```go
func TestSpawnResolvesTheCommandOnPath(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeharness")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s := newTestServer(t)
	id := createShellPane(t, s, "fakeharness")
	if _, ok := s.Live(id); !ok {
		t.Fatal("pane not live")
	}
}

func TestSpawnNamesPathInTheError(t *testing.T) {
	s := newTestServer(t)
	resp := call(t, s, `{"id":"1","cmd":"pane.create","cwd":"/","cmd_argv":["no-such-harness-xyz"],"label":"x","kind":"pty"}`)
	if !strings.Contains(resp.Error.Message, "not on PATH") {
		t.Fatalf("error does not teach: %q", resp.Error.Message)
	}
}
```

Python (`tests/floor/test_coppice_backend.py`):

```python
def test_spawn_passes_the_bare_command_not_a_home_path(fake_socket):
    backend = CoppiceBackend(socket=fake_socket.path)
    backend.spawn(cwd=Path("/"), cmd=["claude"], env={}, label="x", kind="pty")
    req = fake_socket.last_request()
    assert req["cmd_argv"][0] == "claude"
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/server -run 'TestSpawnResolves|TestSpawnNamesPath' -v` and `uv run --no-sync pytest -q tests/floor/test_coppice_backend.py -k bare_command`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `startPane` before building the PTY: if `!strings.Contains(argv[0], "/")`, call `exec.LookPath(argv[0])`; on error return `fmt.Errorf("%s is not on PATH. Install it or give a full path.", argv[0])` wrapped so `handlePaneCreate` maps it to `spawn_failed`. In the Python backend, delete the branch that rewrites the command to `~/.local/bin/`.

- [ ] **Step 4: Run all tests**

Run: `cd harness/coppice && go test -p 1 ./... && cd ../.. && uv run --no-sync pytest -q tests/floor`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/panes.go harness/coppice/internal/server/lookpath_test.go src/opendaisugi/floor/coppice_backend.py tests/floor/test_coppice_backend.py
git commit -m "coppice: resolve the harness on PATH and say so when it is missing"
```

---

### Task 4: Attach is loud when it cannot show a screen

**Files:**
- Modify: `harness/coppice/internal/cli/cli.go` (the `attach` case near line 121 and the TTY check near line 614)
- Modify: `src/opendaisugi/cli.py` (`daisugi coppice attach` execs the `coppice` binary with the terminal)
- Test: `harness/coppice/internal/cli/attach_cli_test.go`, `tests/floor/test_cli_coppice.py`

**Interfaces:**
- Produces: `coppice attach <pane>` on a non-terminal prints `attach needs a terminal. Run it from a shell, not a pipe.` on stderr and exits 1. With an unreachable server it prints the existing `ErrServerGone` line and exits 3. `daisugi coppice attach` replaces itself with `coppice attach` through `os.execvp`, so the TTY is inherited.

- [ ] **Step 1: Write the failing tests**

Go, driving the CLI with stdin as a pipe:

```go
func TestAttachWithoutATerminalExits1AndSaysWhy(t *testing.T) {
	var stderr bytes.Buffer
	code := Run([]string{"attach", "w1:p1"}, strings.NewReader(""), io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "needs a terminal") {
		t.Fatalf("stderr does not teach: %q", stderr.String())
	}
}
```

Python:

```python
def test_daisugi_coppice_attach_execs_the_binary(monkeypatch):
    calls = []
    monkeypatch.setattr(os, "execvp", lambda f, argv: calls.append((f, argv)))
    monkeypatch.setattr(shutil, "which", lambda n: "/usr/bin/coppice")
    result = runner.invoke(app, ["coppice", "attach", "w1:p1"])
    assert calls == [("/usr/bin/coppice", ["coppice", "attach", "w1:p1"])]
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `cd harness/coppice && go test -p 1 ./internal/cli -run TestAttachWithoutATerminal -v` and `uv run --no-sync pytest -q tests/floor/test_cli_coppice.py -k execs_the_binary`
Expected: FAIL.

- [ ] **Step 3: Implement**

In the CLI attach path, check `term.IsTerminal` on stdin before dialing. Print the teaching line to stderr and return 1. In `cli.py`, the `attach` command resolves `coppice` with `shutil.which`, exits 3 with `coppice is not on PATH. Build it: make -C harness/coppice` when missing, else `os.execvp`.

- [ ] **Step 4: Run all tests**

Run: `cd harness/coppice && go test -p 1 ./... && cd ../.. && uv run --no-sync pytest -q tests/floor`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/cli/cli.go harness/coppice/internal/cli/attach_cli_test.go src/opendaisugi/cli.py tests/floor/test_cli_coppice.py
git commit -m "coppice: attach says why when it cannot show a screen"
```

---

### Task 5: Status prints an integer pid

**Files:**
- Modify: `harness/coppice/internal/cli/status.go:13`
- Test: `harness/coppice/internal/cli/status_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestStatusPrintsAnIntegerPid(t *testing.T) {
	out := renderStatus(map[string]any{"pid": float64(4242), "socket": "/x", "panes": float64(2)})
	if !strings.Contains(out, "pid 4242\n") {
		t.Fatalf("pid rendered wrong: %q", out)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `cd harness/coppice && go test -p 1 ./internal/cli -run TestStatusPrintsAnIntegerPid -v`
Expected: FAIL with `pid 4242` printed as `4.242e+03` or `4242.000000`.

- [ ] **Step 3: Implement**

Extract the formatting into `renderStatus(res map[string]any) string` and format numbers through a helper that prints `float64` values with no fraction as integers.

- [ ] **Step 4: Run the tests**

Run: `cd harness/coppice && go test -p 1 ./internal/cli`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/cli/status.go harness/coppice/internal/cli/status_test.go
git commit -m "coppice: status prints the pid as an integer"
```

---

### Task 6: Prove it by hand, once

**Files:**
- Modify: `docs/plans/2026-09-12-porcelain/RESUME.md` (create if missing; record the run)

- [ ] **Step 1:** Build: `cd harness/coppice && make` (or the documented build command in `harness/coppice/README.md`).
- [ ] **Step 2:** In a real terminal: `coppice server start`, `coppice pane create --cwd . -- sh -c 'sleep 60'`, `coppice pane list`. Expect `idle`, source `process`, a `quiet_for` number.
- [ ] **Step 3:** `coppice pane create --cwd . -- no-such-harness`. Expect one error line naming PATH and no new row in `pane list`.
- [ ] **Step 4:** `coppice attach <id> < /dev/null`. Expect the teaching line and exit 1. Then `coppice attach <id>` in the terminal. Expect the screen.
- [ ] **Step 5:** `coppice server status`. Expect an integer pid.
- [ ] **Step 6:** Record the five results in RESUME.md under "Plan 12 hand check" and commit that file alone.

# Plan 16: First Run Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `coppice` works before it is configured: it finds harnesses on PATH, asks one question at most, writes one readable config file, and Enter opens the default harness in the current directory.

**Architecture:** A config package reads and writes `$XDG_CONFIG_HOME/coppice/coppice.toml` (default `~/.config/coppice/coppice.toml`) with `[harness.<name>]` tables and a `default` key. A discovery function probes PATH for `claude`, `codex`, `pi`, `sprig`, `opencode`. The TUI's first run calls discovery, asks with a numbered list when more than one is found, and prints the three-line vocabulary. App-only harnesses are named honestly.

**Tech Stack:** Go 1.26, `github.com/BurntSushi/toml` (already a dependency).

**Spec:** ROADMAP plan 16; floor board "cold start" and the first-run walkthrough frame; the conversation rule "zero tokens for plumbing".

## Global Constraints

Same as plan 12. Tests set `XDG_CONFIG_HOME` to a temp dir and a fake PATH; never the real home.

---

### Task 1: The config file

**Files:**
- Create: `harness/coppice/internal/config/config.go`
- Test: `harness/coppice/internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
type Harness struct { Command string `toml:"command"`; Args []string `toml:"args"`; State string `toml:"state"` }
type Config struct { Default string `toml:"default"`; Harness map[string]Harness `toml:"harness"`; Plugins []string `toml:"plugins"`; Foreman string `toml:"foreman"` }
func Path() string                      // honours XDG_CONFIG_HOME
func Load() (Config, bool, error)       // ok=false when the file does not exist
func Save(c Config) error               // 0600, creates the directory 0700
```

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadReportsMissingWithoutError(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, ok, err := Load()
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := Config{Default: "claude", Harness: map[string]Harness{"claude": {Command: "claude", State: "hooks"}}}
	if err := Save(c); err != nil { t.Fatal(err) }
	got, ok, err := Load()
	if err != nil || !ok || got.Default != "claude" || got.Harness["claude"].Command != "claude" {
		t.Fatalf("round trip: %+v %v %v", got, ok, err)
	}
	st, _ := os.Stat(Path())
	if st.Mode().Perm() != 0o600 { t.Fatalf("mode %v", st.Mode()) }
}
```

- [ ] **Step 2: Run**: `cd harness/coppice && go test -p 1 ./internal/config -v`. Expected: FAIL.
- [ ] **Step 3: Implement** `config.go`.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/config
git commit -m "coppice/config: one readable file, XDG path, 0600"
```

---

### Task 2: Discovery on PATH

**Files:**
- Create: `harness/coppice/internal/config/discover.go`
- Test: `harness/coppice/internal/config/discover_test.go`

**Interfaces:**
- Produces: `func Discover(lookPath func(string) (string, error)) []Found` where `Found{Name, Path string; State string}`; the probe list is `claude`, `codex`, `pi`, `sprig`, `opencode` in that order; `State` is `hooks` for claude and codex, `rpc` for pi, `sse` for opencode, `hooks` for sprig. `func AppOnlyNote(found []Found) string` returns the honest line when nothing is found: `No harness on PATH. Codex Desktop, Cursor, and Antigravity are apps, and coppice cannot own their panes. Install claude, codex, pi, sprig, or opencode.`

- [ ] **Step 1: Write the failing tests**

```go
func TestDiscoverKeepsProbeOrder(t *testing.T) {
	lp := func(n string) (string, error) {
		if n == "pi" || n == "claude" { return "/bin/" + n, nil }
		return "", errors.New("no")
	}
	f := Discover(lp)
	if len(f) != 2 || f[0].Name != "claude" || f[1].Name != "pi" {
		t.Fatalf("%+v", f)
	}
}

func TestNothingFoundTeaches(t *testing.T) {
	note := AppOnlyNote(nil)
	if !strings.Contains(note, "apps") || !strings.Contains(note, "Install") {
		t.Fatalf("%q", note)
	}
}
```

- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/config/discover.go harness/coppice/internal/config/discover_test.go
git commit -m "coppice/config: find harnesses on PATH and say when none can be owned"
```

---

### Task 3: The first-run screen

**Files:**
- Create: `harness/coppice/internal/tui/firstrun.go`
- Modify: `harness/coppice/internal/tui/run.go` (call `FirstRun` when `config.Load` reports missing)
- Test: `harness/coppice/internal/tui/firstrun_test.go`

**Interfaces:**
- Produces: `func FirstRun(found []config.Found, in io.Reader, out io.Writer) (config.Config, error)`. One found: writes the config with no question and prints `claude is your default. Enter opens it here.` Several: prints the numbered list, reads one digit, writes the config. None: prints `AppOnlyNote` and returns an error that the CLI maps to exit 1. Always ends with the three vocabulary lines: `Enter opens <default> here.`, `space peeks. shift-tab goes to whoever needs you.`, `Type words to talk.`

- [ ] **Step 1: Write the failing tests**

```go
func TestOneHarnessAsksNothing(t *testing.T) {
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude", Path: "/bin/claude", State: "hooks"}}, strings.NewReader(""), &out)
	if err != nil || c.Default != "claude" { t.Fatalf("%+v %v", c, err) }
	if strings.Contains(out.String(), "Which") { t.Fatal("asked a question with one harness") }
	if !strings.Contains(out.String(), "Enter opens claude here.") { t.Fatalf("%s", out.String()) }
}

func TestTwoHarnessesAskOnce(t *testing.T) {
	var out bytes.Buffer
	c, err := FirstRun([]config.Found{{Name: "claude"}, {Name: "pi"}}, strings.NewReader("2\n"), &out)
	if err != nil || c.Default != "pi" { t.Fatalf("%+v %v", c, err) }
	if strings.Count(out.String(), "?") != 1 { t.Fatalf("more than one question: %s", out.String()) }
}

func TestNoHarnessIsHonest(t *testing.T) {
	var out bytes.Buffer
	_, err := FirstRun(nil, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(out.String(), "apps") { t.Fatalf("%v %s", err, out.String()) }
}
```

- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. On a non-terminal `in`, with several found, take the first and print `claude is your default. Edit ~/.config/coppice/coppice.toml to change it.` so scripts never hang.
- [ ] **Step 4: Run**: `cd harness/coppice && go test -p 1 ./internal/tui`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/firstrun.go harness/coppice/internal/tui/firstrun_test.go harness/coppice/internal/tui/run.go
git commit -m "coppice/tui: first run asks one question at most"
```

---

### Task 4: Enter opens the default here, and the shell form

**Files:**
- Modify: `harness/coppice/internal/tui/run.go` (Enter on the empty floor or on no row: `pane.create` with the default harness in the current directory, then attach)
- Modify: `harness/coppice/internal/cli/cli.go` (`coppice <harness> [args...]` creates a pane with that harness and attaches; `coppice open <harness>` is the long form)
- Modify: `harness/coppice/internal/server/panes.go` (`pane.create` accepts `"harness": "<name>"` and resolves command and args from the config when `cmd_argv` is absent)
- Test: `harness/coppice/internal/server/create_by_harness_test.go`, `harness/coppice/internal/cli/open_test.go`

- [ ] **Step 1: Write the failing tests**: `pane.create {"harness":"fakeh","cwd":"/"}` with a config naming `fakeh` as `sh -c 'sleep 30'` produces a live pane; `coppice fakeh` from the CLI with a pipe stdin creates the pane and prints `opened <id>. attach needs a terminal.` and exits 0.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. The server loads the config on each `pane.create` that names a harness, so an edit takes effect without a restart.
- [ ] **Step 4: Run**: `cd harness/coppice && go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui/run.go harness/coppice/internal/cli/cli.go harness/coppice/internal/server/panes.go harness/coppice/internal/server/create_by_harness_test.go harness/coppice/internal/cli/open_test.go harness/coppice/PROTOCOL.md
git commit -m "coppice: Enter opens the default harness here, and coppice <harness> does the same from a shell"
```

---

### Task 5: The Python side reads the same file

**Files:**
- Modify: `src/opendaisugi/floor/registry.py` (harness resolution reads `~/.config/coppice/coppice.toml` when present, before its own defaults)
- Test: `tests/floor/test_registry.py`

- [ ] **Step 1: Write the failing test**: with `XDG_CONFIG_HOME` pointing at a temp dir holding a config with `[harness.pi-llama]`, `prompt_pane(harness="pi-llama")` spawns `["pi", "--model", "llama-3.3-70b", ...]`.
- [ ] **Step 2: Run**: `uv run --no-sync pytest -q tests/floor/test_registry.py -k coppice_toml`. Expected: FAIL.
- [ ] **Step 3: Implement** with `tomllib` from the stdlib.
- [ ] **Step 4: Run**: `uv run --no-sync pytest -q tests/floor`: PASS.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/floor/registry.py tests/floor/test_registry.py
git commit -m "floor: the python client reads the coppice config for harness commands"
```

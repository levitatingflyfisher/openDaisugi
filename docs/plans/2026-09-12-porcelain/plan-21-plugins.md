# Plan 21: Plugins Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A plugin is a directory with a manifest and an id listed in `coppice.toml`. Two kinds: a view, which is a page the web server serves and the TUI can name; and a policy, which is a process the server starts with the socket, that listens to events and holds only the verbs its manifest asks for, never allow or deny. Every shipped view is on by default. Views hot-swap with selection carried across. Three policies and three notifiers ship as examples.

**Architecture:** `internal/plugins` loads manifests from `$XDG_CONFIG_HOME/coppice/plugins/<id>/manifest.json` and from the binary's embedded `plugins/` directory for the shipped set. The web server mounts a view at `/plugins/<id>/`. A policy process is spawned with `COPPICE_SOCKET` and `COPPICE_PLUGIN=<id>` in its environment; its first line must be `hello {role: plugin, id}`, and the server grants only the verbs in `needs`. The `coppice.` id prefix is reserved. A notifier is a policy whose `needs` is empty and which listens for `state` events.

**Tech Stack:** Go 1.26, `embed`, vanilla JS, Python for the example policies.

**Spec:** ROADMAP plan 21; floor board "plugins"; the rule "every mark means a state the socket reports".

## Global Constraints

Same as plan 12. Example policies are Python scripts with PEP 723 headers and no dependencies beyond the stdlib. Tests never start a real ntfy or a real voice engine.

---

### Task 1: The manifest and the loader

**Files:**
- Create: `harness/coppice/internal/plugins/manifest.go`, `loader.go`
- Test: `harness/coppice/internal/plugins/loader_test.go`

**Interfaces:**
- Produces:

```go
type Manifest struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`    // "view" | "policy"
	Page    string   `json:"page"`    // view: relative html file
	Run     string   `json:"run"`     // policy: relative executable
	Listens []string `json:"listens"` // event kinds
	Needs   []string `json:"needs"`   // verbs, never agent.allow or agent.deny
	Title   string   `json:"title"`
}
func Load(dirs []string, enabled []string) ([]Plugin, []Problem)
```

Rules enforced by `Load`: an id must match `^[a-z][a-z0-9-]*$`; `coppice.` prefixed ids are refused unless the directory is the embedded one; `needs` containing `agent.allow` or `agent.deny` is refused with `a plugin can propose. It cannot allow.`; a view must have `page`, a policy must have `run`; problems are returned, never panicked, and the CLI prints them.

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadRefusesTheReservedPrefixFromUserDirs(t *testing.T) {
	dir := writePlugin(t, "coppice.evil", `{"id":"coppice.evil","kind":"view","page":"i.html"}`)
	_, probs := Load([]string{dir}, []string{"coppice.evil"})
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "reserved") { t.Fatalf("%+v", probs) }
}

func TestAPolicyCannotAskForAllow(t *testing.T) {
	dir := writePlugin(t, "merge", `{"id":"merge","kind":"policy","run":"m.py","needs":["pane.read","agent.allow"]}`)
	_, probs := Load([]string{dir}, []string{"merge"})
	if len(probs) != 1 || !strings.Contains(probs[0].Message, "cannot allow") { t.Fatalf("%+v", probs) }
}

func TestOnlyEnabledIdsLoad(t *testing.T) {
	dir := writePlugins(t, "a", "b")
	ps, _ := Load([]string{dir}, []string{"b"})
	if len(ps) != 1 || ps[0].ID != "b" { t.Fatalf("%+v", ps) }
}
```

- [ ] **Step 2: Run**: `cd harness/coppice && go test -p 1 ./internal/plugins -v`. Expected: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/plugins
git commit -m "coppice/plugins: a manifest, an id, and the rules a loader enforces"
```

---

### Task 2: Views served and switchable

**Files:**
- Modify: `harness/coppice/internal/web/static.go` (mount each view at `/plugins/<id>/` from its directory; embedded views from `harness/coppice/plugins/`)
- Modify: `harness/coppice/internal/web/static/app.js` (`#/view/<id>` route loads the view page in the tiles area through an `<iframe>` with the same origin; the rail lists views; selection is passed as `#/view/<id>?sel=a,b`)
- Modify: `harness/coppice/internal/tui/prompt.go` (a view id typed alone prints `views live on the floor page: http://127.0.0.1:<port>/#/view/<id>` because the terminal cannot render a page)
- Create: `harness/coppice/plugins/tree/manifest.json` and `tree/index.html` as the first shipped view (a text tree, the same lines the TUI renders, fetched from `/api/tasks`)
- Modify: `harness/coppice/internal/web/api.go` (`GET /api/tasks`, `GET /api/views`)
- Test: `harness/coppice/internal/web/views_test.go`, `harness/coppice/internal/web/static/_tests/views.test.mjs`

- [ ] **Step 1: Write the failing tests**: `GET /plugins/tree/` returns the page with the bearer guard; `GET /api/views` lists `tree` with its title; switching from the roster to `#/view/tree` with two panes selected passes `sel=` and the tree page marks them.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./internal/web`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web harness/coppice/plugins/tree
git commit -m "coppice/web: views are pages under /plugins, switchable with selection carried"
```

---

### Task 3: Policies as processes with only their verbs

**Files:**
- Create: `harness/coppice/internal/plugins/runner.go` (start each enabled policy with `COPPICE_SOCKET` and `COPPICE_PLUGIN`; restart with backoff up to five times; log every exit)
- Modify: `harness/coppice/internal/server/server.go` (`hello {role: "plugin", id}` looks up the manifest and installs the `needs` set as the connection's allowed verbs; any other verb answers `unauthorized` with `plugin <id> did not ask for <verb> in its manifest`)
- Test: `harness/coppice/internal/server/plugin_verbs_test.go`, `harness/coppice/internal/plugins/runner_test.go`

- [ ] **Step 1: Write the failing tests**: a plugin connection with `needs: ["pane.read"]` can `pane.read` but gets `unauthorized` for `pane.close`; the runner starts a script that exits at once and gives up after five tries with a logged line.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/plugins/runner.go harness/coppice/internal/plugins/runner_test.go harness/coppice/internal/server
git commit -m "coppice: a policy runs as a process holding only the verbs it asked for"
```

---

### Task 4: The policy triad

**Files:**
- Create: `harness/coppice/plugins/merge-on-green/manifest.json`, `merge-on-green.py` (listens `state`; when a pane in a task reports `done` and the task's worktree has an open PR, prompts the pane `checks are green, merge` only after `gh pr checks` reports success; needs `pane.read`, `agent.prompt`, `floor.note`)
- Create: `harness/coppice/plugins/close-quiet/manifest.json`, `close-quiet.py` (a pane `idle` for sixty minutes gets `pane.close` with the worktree kept, and a note saying so)
- Create: `harness/coppice/plugins/turn-budget/manifest.json`, `turn-budget.py` (counts `state` flips per pane; past a budget from its manifest's `config`, sends `send_keys ctrl+c` and a note `paused: past N turns. prompt it to continue.`)
- Test: `tests/floor/test_example_policies.py` (each script is driven with a fake socket that replays event lines from a fixture and records what it sent)

- [ ] **Step 1: Write the failing tests**: one per policy, replaying a fixture of events and asserting the exact requests sent, including that none is `agent.allow`.
- [ ] **Step 2: Run**: `uv run --no-sync pytest -q tests/floor/test_example_policies.py`. Expected: FAIL.
- [ ] **Step 3: Implement** the three scripts with a tiny shared client, `plugins/_lib/floor_client.py`, stdlib only, PEP 723 headers.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins tests/floor/test_example_policies.py
git commit -m "coppice/plugins: three policies that can prompt and wait but never allow"
```

---

### Task 5: The notifier triad and the on-by-default list

**Files:**
- Create: `harness/coppice/plugins/notify-ntfy/` (moves the existing push publisher's message shaping into a plugin that listens to `state` and posts to the configured ntfy; the Go publisher stays as the built-in and this one is the reference for authors)
- Create: `harness/coppice/plugins/notify-lockscreen/` (the same, with ntfy actions per plan 20 Task 5)
- Create: `harness/coppice/plugins/notify-voice/` (listens to `state`; on `blocked` speaks the ask summary through the voice bridge's local speak endpoint from plan 07, if configured; otherwise a note saying voice is not set up)
- Modify: `harness/coppice/internal/config/config.go` (`DefaultPlugins()` returns `tree, minimap, kanban, colony, shift-log, herdr-grid, inbox, merge-on-green, close-quiet, turn-budget, notify-ntfy`; a config without `plugins` gets the default list; `plugins = []` turns everything off)
- Test: `harness/coppice/internal/config/plugins_default_test.go`, `tests/floor/test_example_notifiers.py`

- [ ] **Step 1: Write the failing tests**: a missing `plugins` key yields the default list; an explicit empty list yields none; each notifier, fed one `blocked` event through the fake socket, produces exactly one outbound message with the label and the summary.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: Go and Python suites: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins harness/coppice/internal/config
git commit -m "coppice/plugins: three notifiers, and every shipped plugin on by default"
```

---

### Task 6: Document the plugin kinds

**Files:**
- Create: `harness/coppice/plugins/README.md` (the manifest fields, the two kinds, the reserved prefix, the rule that a policy cannot allow, the rule that a view's every mark means a socket state, how to test one with the fake socket)
- Modify: `harness/coppice/PROTOCOL.md` (`hello`, plugin roles)

- [ ] **Step 1:** Write both. Commit:

```bash
git add harness/coppice/plugins/README.md harness/coppice/PROTOCOL.md
git commit -m "docs: how to write a coppice plugin"
```

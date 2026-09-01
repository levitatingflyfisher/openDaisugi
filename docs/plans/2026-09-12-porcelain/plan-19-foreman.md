# Plan 19: The Foreman Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Any pane can be the foreman. Its hands are the `coppice` command it already has in its shell, taught by one page. There is no allow among its commands, and the socket refuses allow from any connection that names a pane. Every command a pane runs is printed in dim on the floor. Sentences typed at the prompt go to the foreman. Claude Code subagents show as read-only child rows.

**Architecture:** No MCP. A pane runs in a shell with `COPPICE_SOCK` and `COPPICE_PANE` already in its environment. The CLI reads them and opens every connection with `hello {"role": "pane", "pane": "<id>"}`. The server keeps that role on the connection: it refuses `agent.allow`, `agent.deny`, and `pane.report_state` with `source: operator` from it, and it records a `note` for every other verb the connection runs, so the floor shows the foreman's hands without the foreman doing anything. Promotion is one line sent to the pane: read the skill page, you are the foreman. `foreman = "<pane label>"` in `coppice.toml` names where sentences go. An MCP shim for a harness with no shell is a plugin for the day such a harness exists, not part of this plan.

**Tech Stack:** Go 1.26, Python 3.12.

**Spec:** ROADMAP plan 19; research recommendation "Foreman"; floor board "the foreman"; the ruling of 2026-09-13 that the foreman's tools are commands, not a tool schema.

## Global Constraints

Same as plan 12. Tests never run a real harness. The skill page is tested against the verb table so it cannot name a command that does not exist. Layer purity holds: nothing under the layer imports `opendaisugi.floor`.

---

### Task 1: The foreman's page, and a note verb

**Files:**
- Create: `harness/coppice/skills/foreman/SKILL.md`
- Modify: `harness/coppice/internal/cli/verbs.go` (a `floor` group with `note` taking joined trailing text, wire verb `floor.note`)
- Modify: `harness/coppice/internal/cli/cli.go` (route the `floor` group)
- Test: `harness/coppice/internal/cli/skill_test.go`, `harness/coppice/internal/cli/verbs_test.go`

**Interfaces:**
- Produces: `coppice floor note <text>` sends `{"cmd":"floor.note","text":...}`. The skill page names exactly these eight commands and nothing else:

```
coppice pane list
coppice pane read <pane> [--source screen|log]
coppice pane create --label <name> [--cwd <dir>] [--harness <name>] -- <argv>
coppice agent prompt <pane> <text> [--until done|needs_you] [--timeout <ms>]
coppice agent wait <pane> [--until done|needs_you] [--timeout <ms>]
coppice pane close <pane>
coppice pane fork <pane> [--label <name>]
coppice floor note <text>
```

- [ ] **Step 1: Write the failing tests**

```go
func TestTheSkillPageNamesOnlyCommandsThatExist(t *testing.T) {
	page, err := os.ReadFile("../../skills/foreman/SKILL.md")
	if err != nil { t.Fatal(err) }
	re := regexp.MustCompile("(?m)^coppice ([a-z]+) ([a-z-]+)")
	seen := 0
	for _, m := range re.FindAllStringSubmatch(string(page), -1) {
		if _, ok := verbSpecs[m[1]][m[2]]; !ok { t.Fatalf("SKILL.md names %s %s, which the CLI does not have", m[1], m[2]) }
		seen++
	}
	if seen != 8 { t.Fatalf("the page names %d commands, want 8", seen) }
	if strings.Contains(string(page), "allow") { t.Fatal("the page must not teach allow") }
}

func TestFloorNoteJoinsItsText(t *testing.T) {
	params, _, errMsg := parseVerbArgs("floor", "note", []string{"docs", "pane", "spawned"})
	if errMsg != "" || params["text"] != "docs pane spawned" { t.Fatalf("%v %q", params, errMsg) }
	if socketCommand("floor", "note") != "floor.note" { t.Fatal("wire verb") }
}
```

- [ ] **Step 2: Run**: `cd harness/coppice && go test -p 1 ./internal/cli -run 'TestTheSkillPage|TestFloorNote' -v`. Expected: FAIL, no file, no group.
- [ ] **Step 3: Implement**. Write the page in STE100, one screen long: what a foreman is, the eight commands with one line each, the rule that it cannot allow and must say so when a child asks, and the habit of `coppice floor note` before a long wait. `pane fork` arrives in plan 13; if it is not in the verb table yet, add the spec entry now with wire verb `pane.fork` and the flag `label`.
- [ ] **Step 4: Run**: PASS, then `go test -p 1 ./internal/cli`.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/skills/foreman/SKILL.md harness/coppice/internal/cli/verbs.go harness/coppice/internal/cli/cli.go harness/coppice/internal/cli/skill_test.go harness/coppice/internal/cli/verbs_test.go
git commit -m "coppice: one page teaches a pane to be the foreman, and a note verb"
```

---

### Task 2: A connection that names a pane cannot allow, and its hands are printed

**Files:**
- Modify: `harness/coppice/internal/cli/client.go` (when `COPPICE_PANE` is set, the first line on every connection is `{"id":"0","cmd":"hello","role":"pane","pane":"<id>"}`)
- Modify: `harness/coppice/internal/server/server.go` (`hello {role, pane}` sets `Client.Role` and `Client.Pane`; a `pane` role gets `unauthorized` with `a pane can propose. It cannot allow.` for `agent.allow`, `agent.deny`, and `pane.report_state` with `source: operator`; every other verb it runs is recorded as a note `<label> › <verb> <label or pane args>` before the handler runs)
- Modify: `harness/coppice/internal/server/agents.go` (`floor.note {text, pane?}` emits `{"event":"note","text":...,"pane":...,"ts":...}` to subscribers and keeps the last 200 for `floor.notes`; `agent.allow {pane, ask}` and `agent.deny {pane, ask}` as thin verbs that forward to the same answer path `web/ask.go` uses, so porcelain and the pane rule share one door)
- Modify: `harness/coppice/PROTOCOL.md`, `harness/coppice/testdata/protocol/notes.jsonl`
- Test: `harness/coppice/internal/server/notes_test.go`, `harness/coppice/internal/server/pane_role_test.go`, `harness/coppice/internal/cli/hello_test.go`

- [ ] **Step 1: Write the failing tests**: a note round-trips to a subscribed client as a `note` event and `floor.notes` returns it; a connection that sent `hello {role: pane, pane: p1}` and then `agent.allow` gets `unauthorized` with the exact message; the same connection running `pane.create` produces a `note` event whose text starts with the pane's label; the CLI with `COPPICE_PANE=w1:p1` in the environment writes the hello line first on a fake socket, and without it writes no hello.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. The note text for a verb is the verb name and the values of `label`, `pane`, and `text` from its params, joined with two spaces, text cut at 60 runes.
- [ ] **Step 4: Run**: `cd harness/coppice && go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/cli/client.go harness/coppice/internal/cli/hello_test.go harness/coppice/internal/server harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/notes.jsonl
git commit -m "coppice: a pane's connection can never allow, and every command it runs is a note"
```

---

### Task 3: The second door, in the gate

**Files:**
- Modify: `src/opendaisugi/gate.py` (`evaluate_call`: an argv whose first three words are `coppice agent allow` or `coppice agent deny` is denied before any envelope check, reason `a pane can propose. It cannot allow.`)
- Test: `tests/test_gate_coppice_allow.py`

- [ ] **Step 1: Write the failing test**

```python
def test_the_gate_denies_coppice_allow_from_any_agent(envelope_that_allows_everything):
    decision = evaluate_call(
        envelope_that_allows_everything,
        tool="Bash",
        args={"command": "coppice agent allow w1:p1 ask-3"},
    )
    assert decision.allowed is False
    assert "cannot allow" in decision.reason


def test_the_gate_leaves_other_coppice_commands_to_the_envelope(envelope_that_allows_everything):
    decision = evaluate_call(
        envelope_that_allows_everything, tool="Bash", args={"command": "coppice pane list"}
    )
    assert decision.allowed is True
```

- [ ] **Step 2: Run**: `uv run --no-sync pytest -q tests/test_gate_coppice_allow.py`. Expected: FAIL.
- [ ] **Step 3: Implement**. Use the shell decomposition the gate already has so `x && coppice agent allow ...` is caught too.
- [ ] **Step 4: Run**: PASS, then `uv run --no-sync pytest -q tests/test_gate.py tests/test_layer_boundary.py`.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gate.py tests/test_gate_coppice_allow.py
git commit -m "gate: no agent allows through coppice, whatever its envelope says"
```

---

### Task 4: Promotion is one line

**Files:**
- Create: `src/opendaisugi/floor/promote.py` (`promote(backend, pane) -> str` sends one line to the pane with `send_text`: `Read <abs path to harness/coppice/skills/foreman/SKILL.md>. You are the foreman of this floor. Say ready.` and returns the line; the path comes from `importlib.resources` or the installed coppice data dir, and a missing page raises `FileNotFoundError` naming the path)
- Modify: `src/opendaisugi/floor/registry.py` (`prompt_pane(..., foreman=True)` promotes right after spawn)
- Modify: `harness/coppice/internal/tui/prompt.go` (`foreman <harness>` spawns a pane labelled `floor`, sends the same line, and writes `foreman = "floor"` to the config; `foreman` alone prints the current foreman or `no foreman. Type: foreman claude`)
- Test: `tests/floor/test_promote.py`, `harness/coppice/internal/tui/foreman_test.go`

- [ ] **Step 1: Write the failing tests**: promoting sends exactly one `send_text` whose text names the skill page and ends with `Say ready.`; a missing page raises with the path in the message; `foreman pi` in the TUI creates a pane labelled `floor` with harness `pi`, sends the line, and the saved config has `foreman = "floor"`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/floor/promote.py src/opendaisugi/floor/registry.py tests/floor/test_promote.py harness/coppice/internal/tui/prompt.go harness/coppice/internal/tui/foreman_test.go
git commit -m "floor: a pane becomes the foreman by reading one page"
```

---

### Task 5: Sentences go to the foreman, notes render in dim

**Files:**
- Modify: `harness/coppice/internal/tui/run.go` (a `Talk` line with a configured foreman sends `pane.send_text` to it and scrolls the foreman's last lines above the prompt; with none, the one-line answer from plan 14)
- Modify: `harness/coppice/internal/tui/render.go` (notes render as `  floor › pane.create  docs  pi` in dim under the foreman's reply)
- Modify: `harness/coppice/internal/web/static/floor.js` (the same dim lines in the foreman tile)
- Test: `harness/coppice/internal/tui/run_test.go`, `harness/coppice/internal/web/static/_tests/floor.test.mjs`

- [ ] **Step 1: Write the failing tests**: with `foreman = "floor"` and a pane labelled `floor`, typing a sentence and Enter sends `pane.send_text` to that pane; a `note` event appears in the render with the dim attribute; with no foreman the answer line appears and nothing is sent.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui harness/coppice/internal/web/static/floor.js harness/coppice/internal/web/static/_tests/floor.test.mjs
git commit -m "coppice: words go to the foreman, and its hands are printed in dim"
```

---

### Task 6: Subagents as read-only child rows

**Files:**
- Modify: `harness/coppice/internal/adapters/claude/claude.go` (parse `SubagentStart` and `SubagentStop` hook events into `child` events: `{"event":"child","pane":<parent>,"child":<agent id>,"state":"working"|"done","label":...}`)
- Modify: `harness/coppice/internal/tui/model.go` and `render.go` (children render indented under the parent row, no tile, no Enter)
- Modify: `src/opendaisugi/hook.py` (`record_lifecycle_event` forwards subagent lifecycle events to the pane host when `floor_report` is coppice)
- Test: `harness/coppice/internal/adapters/claude/subagent_test.go`, `tests/test_hook.py`

- [ ] **Step 1: Write the failing tests**: feeding a `SubagentStart` payload produces a `child` event with `working`; `SubagentStop` produces `done`; the model renders the child under its parent with two spaces of indent and `Enter` on it is a no-op that prints `a subagent lives inside its parent. Enter on the parent.`
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: Go and Python suites: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/adapters/claude harness/coppice/internal/tui src/opendaisugi/hook.py tests/test_hook.py
git commit -m "coppice: subagents show under their parent, read only"
```

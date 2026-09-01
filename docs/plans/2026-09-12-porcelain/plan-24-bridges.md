# Plan 24: Bridges Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Existing tools become porcelains without changing them: a Neovim plugin, a tmux mirror driven through control mode, a Herdr attach bridge, and ACP-adjacent types documented on the wire.

**Architecture:** Each bridge is a plugin directory under `harness/coppice/plugins/` with a manifest of kind `policy` when it is a process and a README when it is a client the user installs elsewhere. The Neovim plugin is Lua that opens the socket with `vim.fn.sockconnect`, renders the roster in a floating window, and forwards Space and Enter. The tmux mirror runs `tmux -C` control mode, creates one tmux window per pane, pipes the pane's text into it read-only, and keeps the roster in the status line. The Herdr bridge writes a Herdr agent definition whose command is `coppice attach <pane>`, so Herdr runs our pane inside its own grid. ACP-adjacent types are a table in `PROTOCOL.md` mapping our verbs to ACP's session and prompt methods, with the gap named.

**Tech Stack:** Lua for Neovim, Go 1.26 for the tmux mirror, Python for the Herdr bridge, Markdown for the ACP table.

**Spec:** ROADMAP plan 24; loop and floor boards; the tmux control-mode and Neovim remote-UI field notes in the research report.

## Global Constraints

Same as plan 12. Tests never run a real tmux server, Herdr, or Neovim; each bridge has a pure core with fixtures and a thin shell.

---

### Task 1: ACP-adjacent types on the wire

**Files:**
- Modify: `harness/coppice/PROTOCOL.md` (a section "ACP adjacency": `session/new` to `pane.create`, `session/prompt` to `pane.send_text` or `agent.prompt`, `session/cancel` to `pane.send_keys ctrl+c`, `session/request_permission` to the `state` event with an ask and `agent.allow` or `agent.deny` as the reply; the gap: ACP has no fail-closed permission tier and no `frame` stream)
- Create: `harness/coppice/testdata/protocol/acp-adjacent.jsonl` (the JSON-RPC forms of those four verbs)

- [ ] **Step 1:** Write the section and the corpus file; run `cd harness/coppice && go test -p 1 ./internal/proto`. Expected: PASS, the corpus replays.
- [ ] **Step 2:** Commit

```bash
git add harness/coppice/PROTOCOL.md harness/coppice/testdata/protocol/acp-adjacent.jsonl
git commit -m "coppice: name the ACP adjacency and the gap on the wire"
```

---

### Task 2: The Neovim plugin

**Files:**
- Create: `harness/coppice/plugins/nvim/README.md`, `lua/coppice/init.lua` (`require("coppice").setup{socket=...}`; `:Coppice` opens the floating roster; `<Space>` peeks in a split, `<CR>` opens a terminal buffer running `coppice attach <pane>`, `<Esc>` closes), `lua/coppice/roster.lua` (pure: `group(rows)` and `render(rows, width)` returning lines and highlight spans)
- Create: `harness/coppice/plugins/nvim/tests/roster_spec.lua` (run with `nvim --headless -c "luafile tests/roster_spec.lua"`; skipped in CI when `nvim` is absent, listed as a gap)
- Modify: `harness/coppice/internal/web/js_test.go` or a new `plugins_test.go` in `internal/plugins` to run the Lua spec when `nvim` is on PATH

- [ ] **Step 1: Write the failing test**: `group` puts blocked rows first; `render` never returns a line wider than `width`.
- [ ] **Step 2: Run**: FAIL, or SKIP with `nvim not on PATH` printed as a listed gap.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS where `nvim` exists.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/plugins/nvim harness/coppice/internal/plugins/plugins_test.go
git commit -m "coppice/plugins: the floor inside neovim"
```

---

### Task 3: The tmux mirror

**Files:**
- Create: `harness/coppice/internal/tmuxmirror/mirror.go` (`Plan(panes, windows) []Op` pure: which tmux windows to create, rename, or kill so windows match panes; `StatusLine(model) string` for `set -g status-right`)
- Create: `harness/coppice/internal/tmuxmirror/control.go` (drives `tmux -C` over stdin and stdout, parses `%begin`, `%end`, `%output`, `%window-close`)
- Modify: `harness/coppice/internal/cli/cli.go` (`coppice tmux-mirror` runs it against the current tmux server)
- Test: `harness/coppice/internal/tmuxmirror/mirror_test.go`, `control_test.go` (feeds recorded control-mode transcripts from `testdata/tmux/*.txt`)

- [ ] **Step 1: Write the failing tests**: with three panes and one window, `Plan` yields two creates and one rename; a transcript with `%window-close @3` yields an event naming window 3; the status line reads `2 need you · 3 working`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. Each mirrored window runs `coppice attach <pane>` so keys work, and the mirror only manages window lifecycle and the status line.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tmuxmirror harness/coppice/internal/cli/cli.go harness/coppice/testdata/tmux
git commit -m "coppice: a tmux mirror through control mode, one window per pane"
```

---

### Task 4: The Herdr attach bridge

**Files:**
- Create: `src/opendaisugi/floor/herdr_bridge.py` (`agent_definition(pane, socket) -> dict` producing the TOML Herdr expects for an agent whose command is `coppice attach <pane> --socket <socket>` with a manifest that reads coppice's own status line; `install(pane, herdr_config_dir)` writes it)
- Modify: `src/opendaisugi/cli.py` (`daisugi coppice herdr-bridge <pane>`)
- Test: `tests/floor/test_herdr_bridge.py`

- [ ] **Step 1: Write the failing tests**: the definition names `coppice` as the command with `attach` and the pane; the manifest's blocked pattern matches the attach status line `1 need you`; installing writes one file and never touches others.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. The Herdr manifest schema comes from `floor/manifest_schema.py`, which already pins Herdr's format.
- [ ] **Step 4: Run**: `uv run --no-sync pytest -q tests/floor`: PASS.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/floor/herdr_bridge.py src/opendaisugi/cli.py tests/floor/test_herdr_bridge.py
git commit -m "floor: herdr shows a coppice pane by running attach as its agent"
```

---

### Task 5: Document the bridges

**Files:**
- Modify: `harness/coppice/plugins/README.md` (a "Bridges" section: which tool, what it needs installed, one command to start)
- Modify: `docs/research/boards/loop/body.html` (part three gains one line per bridge)

- [ ] **Step 1:** Write, render the boards, commit both.

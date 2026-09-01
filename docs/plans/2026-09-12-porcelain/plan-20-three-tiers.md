# Plan 20: Three Tiers of Allow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every ask carries a tier: `silent` inside the envelope, `undoable` outside it but reversible, `permanent` outside it and irreversible. Deny is always one key. Allow is one key for undoable and the typed pane name for permanent. The same three shapes on terminal, floor page, and phone.

**Architecture:** The gate computes the tier from the action's effect class, using the shell decomposition's `EFFECTS` table (ADR-0014) and the envelope clause that fired. The tier rides in `Ask.tier` on the PaneStateEvent. Each porcelain renders by tier and enforces the permanent rule locally; the server enforces it too, refusing `agent.allow` on a permanent ask unless `confirm` equals the pane's label.

**Tech Stack:** Python 3.12, Go 1.26, vanilla JS.

**Spec:** ROADMAP plan 20; floor board "three tiers of allow"; gate board part three.

## Global Constraints

Same as plan 12. Fail closed: an ask with no tier is treated as permanent by every consumer.

---

### Task 1: The tier on the ask

**Files:**
- Modify: `src/opendaisugi/gate.py` (`GateDecision.tier`; `_tier(plan, decision) -> str`)
- Modify: `src/opendaisugi/floor/events.py` (`Ask.tier: str = "permanent"`)
- Modify: `harness/coppice/internal/proto/state_event.go` (`Ask.Tier string \`json:"tier"\``; empty reads as `permanent` in `Validate`)
- Modify: `docs/plans/2026-09-08-workshop/00-master-spec.md` §3.1
- Test: `tests/test_gate_tier.py`, `harness/coppice/internal/proto/tier_test.go`

**Interfaces:**
- Produces: tier rules. An allowed action never asks, so `silent` appears only in the shadow log and the journal. A denied action whose effect class is in `{read, write_inside_workspace, network_get, test_run}` is `undoable`. Anything in `{delete, force_push, push_shared, credential, prod, unknown}` is `permanent`.

- [ ] **Step 1: Write the failing tests**

```python
def test_a_force_push_is_permanent():
    d = evaluate_call(payload_for("git push --force origin master"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_write_inside_the_workspace_is_undoable():
    d = evaluate_call(payload_for("Write", path="src/x.py"), envelope=deny_all())
    assert d.tier == "undoable"


def test_an_unknown_effect_is_permanent():
    d = evaluate_call(payload_for("some-unknown-binary --flag"), envelope=deny_all())
    assert d.tier == "permanent"
```

Go: `Validate` on an ask without `tier` sets `permanent`.

- [ ] **Step 2: Run**: `uv run --no-sync pytest -q tests/test_gate_tier.py` and `go test -p 1 ./internal/proto`. Expected: FAIL.
- [ ] **Step 3: Implement**. The effect class comes from `shell_decompose.py`'s effect classification for Bash and from the tool name for file tools.
- [ ] **Step 4: Run**: PASS, plus the full gate suite `uv run --no-sync pytest -q tests/test_gate*.py`.
- [ ] **Step 5: Commit**

```bash
git add src/opendaisugi/gate.py src/opendaisugi/floor/events.py harness/coppice/internal/proto tests/test_gate_tier.py docs/plans/2026-09-08-workshop/00-master-spec.md
git commit -m "gate: every ask carries a tier, and no tier means permanent"
```

---

### Task 2: The server enforces the permanent rule

**Files:**
- Modify: `harness/coppice/internal/server/agents.go` (`agent.allow {pane, ask, confirm?}`: when the current ask's tier is `permanent` and `confirm != label`, answer `unauthorized` with `this cannot be undone. Type the pane name to allow: <label>`)
- Modify: `harness/coppice/internal/web/ask.go` (the HTTP answer path calls the same function)
- Test: `harness/coppice/internal/server/tier_enforce_test.go`

- [ ] **Step 1: Write the failing tests**: a permanent ask allowed without `confirm` is refused; with `confirm` equal to the label it passes; an undoable ask allowed without `confirm` passes; deny never needs `confirm`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/agents.go harness/coppice/internal/web/ask.go harness/coppice/internal/server/tier_enforce_test.go harness/coppice/PROTOCOL.md
git commit -m "coppice: a permanent ask needs the pane name typed, on every door"
```

---

### Task 3: Three shapes in the terminal

**Files:**
- Modify: `harness/coppice/internal/tui/render.go` (peek shows the tier line: `Deny is the default.` then for undoable `y allow once  t allow for this task  n deny`, for permanent `n deny   to allow, type <label> and enter`)
- Modify: `harness/coppice/internal/tui/run.go` (`y`, `t`, `n` inside a peek; typing the label then Enter sends allow with `confirm`)
- Test: `harness/coppice/internal/tui/tier_test.go`

- [ ] **Step 1: Write the failing tests**: a permanent peek render contains `type gate-refactor and enter` and pressing `y` does nothing but print `type the pane name to allow`; an undoable peek accepts `y`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**. `t allow for this task` sends `agent.allow` with `scope: "task"`, which the gate records as a proposed envelope edit, never auto-applied (the existing `proposals` path).
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/tui
git commit -m "coppice/tui: three shapes of ask, deny always one key"
```

---

### Task 4: Three shapes on the floor page and the phone

**Files:**
- Modify: `harness/coppice/internal/web/static/floor.js` (the amber bar shows the tier; permanent shows an input `type the pane name`)
- Modify: `harness/coppice/internal/web/static/roster.js` (the phone queue card: Deny big, Look, and Allow only when undoable; permanent shows the name field)
- Modify: `harness/coppice/internal/web/static/app.css`
- Test: `harness/coppice/internal/web/static/_tests/tier.test.mjs`

- [ ] **Step 1: Write the failing tests**: a permanent ask renders no Allow button and a name input; an undoable ask renders Allow; Deny renders in both and is the first button.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./internal/web`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static
git commit -m "coppice/web: three shapes of ask on the floor page and the phone"
```

---

### Task 5: The lock-screen card carries the tier

**Files:**
- Modify: `harness/coppice/internal/web/push.go` (`onState` builds the ntfy message: title `<label> needs you.`, body `Wants <summary>. Gate says no, rule <n>.`; actions `Deny` as an HTTP action to `/api/ask/answer` with the bearer, and `Look` as a view action; no Allow action on permanent)
- Test: `harness/coppice/internal/web/push_test.go`

- [ ] **Step 1: Write the failing test**: a permanent ask publishes a message with exactly two actions, Deny then Look; an undoable ask publishes three with Allow last.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement** using ntfy's `Actions` header.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/push.go harness/coppice/internal/web/push_test.go
git commit -m "coppice/web: the lock-screen card carries the tier, deny first"
```

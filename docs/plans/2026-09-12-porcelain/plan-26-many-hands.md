# Plan 26: Many Hands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** More than one person can work a floor over the tailnet. Every request carries who made it, every allow and deny is recorded with a name, and the roster shows who is looking at what. Built last, designed now.

**Architecture:** Identity is a name bound to a credential: the socket peer uid maps to the local user's name; a web bearer token maps to the name it was minted for. `hello {name}` on the socket and the token record on the web set `who` on the connection. The server stamps `who` on every state event it emits from an operator action and on every `agent.allow` and `agent.deny`, which the gate journals. The roster shows `looking: <name>` on a tile another person has open. Nothing here changes who may allow: the tiers from plan 20 hold for everyone.

**Tech Stack:** Go 1.26, Python 3.12.

**Spec:** ROADMAP plan 26; the conversation record on collaboration; master spec privacy constraint.

## Global Constraints

Same as plan 12. No accounts, no third-party identity. A name is a string in a local file. The first slice is identity on the wire and in the journal; presence is the second; nothing else is in scope.

---

### Task 1: Names on tokens and on the socket

**Files:**
- Modify: `harness/coppice/internal/web/token.go` (a token record gains `name`; `coppice web token --for alice` mints one; `token list` shows names)
- Modify: `harness/coppice/internal/server/server.go` (`Client.Who`; set from `hello {name}` when the peer uid matches the server uid, else refused; the web layer sets it from the token)
- Modify: `harness/coppice/PROTOCOL.md`
- Test: `harness/coppice/internal/web/token_test.go`, `harness/coppice/internal/server/who_test.go`

- [ ] **Step 1: Write the failing tests**: a minted token carries its name and the websocket handshake with it yields a client whose `Who` is the name; a socket `hello {name: "bob"}` from the same uid sets `Who`; a name with spaces or over 32 bytes is refused with `names are one word, up to 32 characters`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./...`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/token.go harness/coppice/internal/web/token_test.go harness/coppice/internal/server harness/coppice/PROTOCOL.md
git commit -m "coppice: a name on every connection"
```

---

### Task 2: Who allowed what

**Files:**
- Modify: `harness/coppice/internal/server/agents.go` (`agent.allow` and `agent.deny` forward `who` to the answer path)
- Modify: `harness/coppice/internal/web/ask.go` (same for the HTTP door)
- Modify: `src/opendaisugi/ask.py` and `src/opendaisugi/gate.py` (the answer record and the journal entry gain `allowed_by` or `denied_by`)
- Modify: `src/opendaisugi/floor/events.py` (`PaneStateEvent.who: str | None`)
- Test: `tests/test_gate_who.py`, `harness/coppice/internal/server/who_test.go`

- [ ] **Step 1: Write the failing tests**: an allow from a connection named `alice` journals `allowed_by: alice`; an answer with no name journals `allowed_by: local`, never empty.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: Go and Python suites: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server/agents.go harness/coppice/internal/web/ask.go src/opendaisugi/ask.py src/opendaisugi/gate.py src/opendaisugi/floor/events.py tests/test_gate_who.py
git commit -m "gate: every allow and deny names who gave it"
```

---

### Task 3: Presence on the roster

**Files:**
- Modify: `harness/coppice/internal/server/attach.go` (an attach or a view-only attach records `{pane, who}` in a presence map; detach clears it; `pane.list` rows carry `looking: [names]`)
- Modify: `harness/coppice/internal/tui/render.go` and `floor.js` (a row or tile another person has open shows `· alice looking`)
- Test: `harness/coppice/internal/server/presence_test.go`, `harness/coppice/internal/web/static/_tests/presence.test.mjs`

- [ ] **Step 1: Write the failing tests**: two clients named `alice` and `bob` attach to the same pane; `pane.list` shows both; `bob` detaching leaves `alice`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/server harness/coppice/internal/tui/render.go harness/coppice/internal/web/static/floor.js harness/coppice/internal/web/static/_tests/presence.test.mjs
git commit -m "coppice: the roster says who is looking"
```

---

### Task 4: Write down what is out of scope

**Files:**
- Modify: `docs/plans/2026-09-12-porcelain/ROADMAP.md` (a "Many hands, not yet" list: shared tiles, chat, per-person envelopes, roles beyond a name)
- Modify: `docs/research/boards/floor/body.html` (one line in the foreman note)

- [ ] **Step 1:** Write, render, commit both.

# Plan 23: Floor Page and Phone Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a laptop the floor page is the rail, as many tiles as fit, the minimap, and the ask in an amber bar. On a phone it is the attention queue, the quiet screen, and a lock-screen card with Deny and Look. The two are one page that adapts.

**Architecture:** Plan 15 built the tiles and plan 21 the views; this plan finishes the phone surface of the same page. Under 700 px the rail becomes the whole screen: asks as cards on top, working rows below, `Tell the floor` at the bottom. A pane opens full screen on tap. The quiet screen is a real screen, shown when nothing is blocked. The ntfy card links straight at the ask through the existing hash route. The PWA from plan 06 and the voice bridge from plan 07 are the base; this plan changes what they show, not how they connect.

**Tech Stack:** Vanilla ES modules, CSS, Go 1.26 for the push shaping.

**Spec:** ROADMAP plan 23; floor board "phone" and "laptop floor"; plan 06 and 07 carries in `docs/plans/2026-09-08-workshop/RESUME.md`.

## Global Constraints

Same as plan 12. The phone never receives a modal. Every push goes through the self-hosted ntfy or not at all.

---

### Task 1: The attention queue on a narrow screen

**Files:**
- Modify: `harness/coppice/internal/web/static/roster.js` (`render(panes)` at narrow width: ask cards first, each with `who`, the summary in code, the gate line, and buttons Deny, Look, Reply by tier from plan 20; working rows compact; done rows last)
- Modify: `harness/coppice/internal/web/static/app.css` (the `phone` breakpoint at 700 px)
- Test: `harness/coppice/internal/web/static/_tests/queue.test.mjs`

- [ ] **Step 1: Write the failing tests**: two blocked panes and three working render two `.ask` cards before any `.row`; the first button in every card is Deny; the title reads `2 need you` and the subtitle `3 working · 1 done`.
- [ ] **Step 2: Run**: `node --test harness/coppice/internal/web/static/_tests/queue.test.mjs`. Expected: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/roster.js harness/coppice/internal/web/static/app.css harness/coppice/internal/web/static/_tests/queue.test.mjs
git commit -m "coppice/web: on a phone the roster is the attention queue"
```

---

### Task 2: The quiet screen

**Files:**
- Modify: `harness/coppice/internal/web/static/roster.js` (when no pane is blocked: a centered `Nothing needs you.`, `N working. I will ping when one asks.`, and `last ask <age> ago` from the events API of plan 22)
- Test: `harness/coppice/internal/web/static/_tests/quiet.test.mjs`

- [ ] **Step 1: Write the failing test**: no blocked panes renders `.quiet` with the working count and no empty list element.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/roster.js harness/coppice/internal/web/static/_tests/quiet.test.mjs
git commit -m "coppice/web: the quiet screen is a real screen"
```

---

### Task 3: Tell the floor, by thumb or voice

**Files:**
- Modify: `harness/coppice/internal/web/static/newpane.js` (the `Tell the floor` bar: text goes to the foreman when configured, else the plumbing forms `claude`, `close docs`; the mic button records through `record.js` and posts to the existing `/api/voice/transcribe`, then fills the bar for review before sending, never sending on its own)
- Modify: `harness/coppice/internal/web/api.go` (`GET /api/foreman` returns the configured foreman pane or null)
- Test: `harness/coppice/internal/web/static/_tests/tell.test.mjs`

- [ ] **Step 1: Write the failing tests**: with a foreman, a sentence sends `pane.send_text` to it; without one, `claude` sends `pane.create` and a sentence shows the one-line answer; a transcription fills the bar and does not send.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/newpane.js harness/coppice/internal/web/api.go harness/coppice/internal/web/static/_tests/tell.test.mjs
git commit -m "coppice/web: tell the floor from the phone, by thumb or by voice, and review before it goes"
```

---

### Task 4: The lock-screen card opens the ask

**Files:**
- Modify: `harness/coppice/internal/web/push.go` (the `click` URL of every ask push is `https://<host>/#/pane/<id>?ask=<askid>`; the `Look` action opens the same; `Deny` posts to `/api/ask/answer` with the bearer in the action header)
- Modify: `harness/coppice/internal/web/static/app.js` (the `ask=` parameter scrolls to and highlights the ask bar)
- Test: `harness/coppice/internal/web/push_test.go`, `harness/coppice/internal/web/static/_tests/app.test.mjs`

- [ ] **Step 1: Write the failing tests**: the push message's click URL names the pane and the ask; routing `#/pane/a?ask=x` yields `{screen: "pane", pane: "a", ask: "x"}`.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/push.go harness/coppice/internal/web/push_test.go harness/coppice/internal/web/static/app.js harness/coppice/internal/web/static/_tests/app.test.mjs
git commit -m "coppice/web: the lock-screen card lands on the ask"
```

---

### Task 5: The laptop floor, finished

**Files:**
- Modify: `harness/coppice/internal/web/static/floor.js` (the rail lists views from `/api/views` under the groups; the minimap sits at the bottom of the rail; `layout` and `lock` controls in the rail footer; the tab title carries the count as `N · coppice`)
- Test: `harness/coppice/internal/web/static/_tests/floor.test.mjs`

- [ ] **Step 1: Write the failing tests**: the document title becomes `2 · coppice` with two blocked panes and `coppice` with none; the rail contains one link per view; toggling lock disables tile drag.
- [ ] **Step 2: Run**: FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run**: `go test -p 1 ./internal/web`: PASS.
- [ ] **Step 5: Commit**

```bash
git add harness/coppice/internal/web/static/floor.js harness/coppice/internal/web/static/_tests/floor.test.mjs
git commit -m "coppice/web: the count in the tab title, views in the rail, the minimap at its foot"
```

---

### Task 6: Hand check on a real phone

- [ ] **Step 1:** Serve over the tailnet as plan 06 documents, open the PWA, trigger a deny from a shell pane with a fake ask through `pane.report_state`, and confirm: the card, Deny, the quiet screen after, the lock-screen push landing on the ask.
- [ ] **Step 2:** Record the result in `docs/plans/2026-09-12-porcelain/RESUME.md` and commit that file alone.

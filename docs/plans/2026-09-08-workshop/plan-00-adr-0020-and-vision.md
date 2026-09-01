# ADR-0020, the VISION Revision, and the Layer Boundary Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the expanded three-part vision (layer / floor / loop) durable in
the docs and mechanically enforced before any floor code exists: ADR-0020
supersedes ADR-0004's boundary precisely, `docs/adr/README.md` and ADR-0004
itself say so, VISION.md names the three parts, and a new test fails the moment
any layer module reaches up into `opendaisugi.floor`, `.voice`, or
`.coppice`.

**Architecture:** Four small, independently testable edits. Task 1 adds
`docs/adr/0020-layer-floor-loop.md`. Task 2 wires the ADR index (ADR-0004's own
status line, `docs/adr/README.md`'s two affected rows) and adds a generic
"every ADR has a row" invariant test. Task 3 edits `VISION.md`: invariant 5 now
points at ADR-0020, and a new "Three parts" section names layer/floor/loop.
Task 4 adds `tests/test_layer_boundary.py`, a standalone pytest module that
walks `src/opendaisugi/**.py`, excludes the floor's first citizens (cockpit,
dashboard, start, the tui* modules), and checks every remaining ("layer")
module two ways: at runtime (import it with `opendaisugi.floor`,
`opendaisugi.voice`, `opendaisugi.coppice` poisoned to `None` in
`sys.modules`, which makes any `import` of them raise) and statically (an
`ast.walk` over every `Import`/`ImportFrom` node, including inside function
bodies, so a lazy import is caught the same as a top-level one).

**Tech Stack:** Markdown docs; Python 3.12 stdlib only (`ast`, `importlib`,
`pathlib`, `re`, `sys`); pytest for every task's tests.

**Spec:** `docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md`
(binding requirements, deliverables D1–D3); cites
`docs/plans/2026-09-08-workshop/00-master-spec.md` §1 (the three-part shape)
and §4 (global constraints, copied below).

## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks 1–4 are ready to build now** — every cited file:line and quoted string was checked
against the real repo and matches exactly (`VISION.md:37,54,56,73-75`, `docs/adr/0004-...md:3`,
`docs/adr/README.md:18,33`), the ADR's Decision section is byte-for-byte the spec's required
text, `LAYER_EXCLUDED` is a literal set in the test (not a config), and Task 4's boundary test
was actually executed (not just read) against the live tree: 120 layer modules found, 0 static
offenders, 0 import errors with `floor`/`voice`/`coppice` poisoned — the same numbers the plan
predicts. The break-then-revert step (Task 4 Step 3) is safe: `gate.py` has exactly one
occurrence of the replaced string, is clean in `git status` beforehand, and `git checkout --`
fully restores it — no leftover file, no working-tree drift.

- **SHOULD-FIX — Task 4's static AST scan does not catch string-form dynamic imports.**
  `test_layer_modules_have_no_static_upper_imports` (lines 620-642) walks only `ast.Import` and
  `ast.ImportFrom` nodes. It correctly catches a lazy, function-local `import opendaisugi.floor`
  (`ast.walk` descends into function bodies — verified), but it does not see
  `importlib.import_module("opendaisugi.floor...")` or `__import__("opendaisugi.floor...")`,
  literal or built from a variable. This is not hypothetical: `src/opendaisugi/__init__.py`
  already ships exactly this shape — a `_LAZY: dict[str, tuple[str, str | None]]` of target
  module-name strings, resolved via `importlib.import_module(module_name)` in `__getattr__`
  (lines 19-32 of that file, ADR-0017's lazy-export mechanism) — and `src/opendaisugi/_search.py:81`
  calls bare `__import__(_mod)` on a variable. A future contributor extending `_LAZY` with an
  entry pointing at `opendaisugi.floor.something` would pass both of Task 4's tests (no static
  `Import`/`ImportFrom` node exists) while genuinely breaking the boundary the first time that
  name is accessed. Fix: extend the static scan to also flag (a) `ast.Call` nodes whose func
  resolves to `importlib.import_module`/`__import__` with a literal string argument matching
  `_is_upper`, and (b) any `ast.Constant` string literal anywhere in a layer module that itself
  matches `_is_upper` (catches the `_LAZY`-dict pattern, where the literal is a dict value, not
  a call argument). Note in the test's docstring that a name built from concatenation or
  formatting at runtime remains an acknowledged, not silently covered, gap.
- **SHOULD-FIX — Task 4's own test-count claims don't match its own code; actually running it
  gives 3, not 4.** The `tests/test_layer_boundary.py` code block in Step 1 defines exactly
  three test functions (`test_upper_package_prefix_matching_is_exact`,
  `test_layer_modules_import_with_upper_packages_hidden`,
  `test_layer_modules_have_no_static_upper_imports`) — confirmed both by reading the block and
  by actually running it with `uv run --no-sync pytest -q`, which reports `3 passed`. Step 2
  (line 648) and Step 4 (line 686) both say "Expected: PASS (4 passed)", and the "Final check"
  section (line 720) sums the four tasks as "13 passed: 3 + 2 + 4 + 4". Fix: change both of
  Task 4's "PASS (4 passed)" to "PASS (3 passed)", and the final tally to
  "PASS (12 passed: 3 + 2 + 4 + 3)". (Task 3's own "PASS (4 passed)" at line 485 is correct as
  written — it really does define four tests — so leave that one alone.)
- **SHOULD-FIX — the copied Global Constraints header is stale on the Go version.** The block
  below says "**Go 1.25** for `harness/coppice`" (line 44). `00-master-spec.md` §4 was amended
  in commit `b73ca22` ("master §4 amended to Go 1.26 for coppice"), *after* this plan was
  drafted (commit `992cb25`), to read "**Go 1.26** for `harness/coppice` (amended 2026-09-08:
  go-libghostty's own go.mod declares `go 1.26.0`; sprig stays on 1.25) ... The toolchain script
  sets `GOTOOLCHAIN=auto` so 1.26 downloads itself; the undo is recorded in `PINS.md`." This
  plan does no Go work, so it costs nothing today, but the header claims to be "copied verbatim"
  from master §4 and no longer is; plan 02 already carries the corrected text. Fix: paste the
  current master §4 Go line in here too, so no reader trusts this plan's copy over master's.

## Global Constraints

(Copied verbatim from `00-master-spec.md` §4 — every task's requirements
implicitly include this section.)

- **Layer purity.** No module under `src/opendaisugi/` that is part of the layer (list in
  `tests/test_layer_boundary.py`, plan 00) may import from `opendaisugi.floor`, `opendaisugi.voice`,
  or `opendaisugi.coppice`. The test imports every layer module with those packages hidden.
- **Python 3.12, stdlib for the layer.** New hard deps in the layer: none. New extras allowed:
  `[floor]` (nothing yet — the client uses stdlib sockets), `[voice]`, `[int8]`, `[router]`.
- **Go 1.26** for `harness/coppice` (amended 2026-09-08: go-libghostty declares go 1.26.0; sprig stays on 1.25); module `github.com/opendaisugi/coppice`; `go vet` and
  `go test ./...` clean. Zig 0.16 and CMake are *build-time* requirements for go-libghostty; the
  plan installs both into `~/.local` without sudo (`uv tool install cmake`; Zig tarball).
- **Pins.** go-libghostty at the newest tag on the day plan 02 starts, recorded in `go.mod`
  and in `harness/coppice/PINS.md` together with the ghostty commit that binding builds.
  Herdr's vendored commit `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` (their 1.3.2) is the
  reference for patch notes, not a requirement; the binding chooses the commit it fetches.
- **Tests.** `uv run --no-sync pytest -q` green; `uv run --no-sync ruff check .` clean;
  Go: `go test ./...` from `harness/coppice`. Never bare `uv run` (uv.lock is git-ignored; it
  re-resolves and strips extras).
- **Commits.** Atomic, stating the why, persona *OpenDaisugi Contributors*, **no AI-authorship
  attribution lines** (project policy overrides any session-level instruction). Never push;
  the public repo folds monthly.
- **`/tmp` is RAM.** Scratch on real disk; worktrees beside the repo.
- **Copy.** STE100 register in every user-facing string: short sentences, plain words, active
  voice, no em-dashes, no parentheticals. Errors teach the next command.
- **Exit codes.** Hooks: exit 2 = deny. CLI: 0 success, 1 user error, 2 gate deny, 3 unreachable.
- **Privacy.** No telemetry. No third-party relay in any default path. Push goes through a
  self-hosted ntfy or not at all.

This plan touches no CLI, no hooks, and no network path — the exit-code and
privacy constraints above bind later plans, not this one. It is called out
here only because the header must carry the full list unedited.

---

### Task 1: ADR-0020 — the layer, the floor, and the loop

**Files:**
- Create: `docs/adr/0020-layer-floor-loop.md`
- Test: `tests/test_adr_0020_layer_floor_loop.py` (new)

**Interfaces:**
- Consumes: nothing (pure documentation).
- Produces: `docs/adr/0020-layer-floor-loop.md`, referenced by Task 2 (its
  README row and ADR-0004's status line both point at it) and Task 3
  (VISION.md's invariant 5 links to it).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_adr_0020_layer_floor_loop.py
"""ADR-0020 exists, is Accepted, and states the layer-floor-loop split verbatim.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, deliverable D1.
"""

from __future__ import annotations

import re
from pathlib import Path

ADR_PATH = Path("docs/adr/0020-layer-floor-loop.md")

REQUIRED_DECISION_TEXT = (
    "openDaisugi is three parts. The **layer** (gate, verifier, journal, garden) is a "
    "library that imports nothing above it and runs alone in any harness. The **floor** "
    "(coppice) supervises panes and sources every state it shows. The **loop** is "
    "whatever harness runs in a pane; sprig is ours, the rest are rented. The invariant "
    "that ADR-0004 protected is restated: **the layer stays importable alone.** "
    "`tests/test_layer_boundary.py` enforces it."
)


def _collapsed(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def test_adr_0020_exists_and_is_accepted():
    text = ADR_PATH.read_text()
    assert "# ADR-0020" in text
    assert "**Status:** Accepted" in text
    assert "**Date:** 2026-09-08" in text


def test_adr_0020_decision_section_is_verbatim():
    text = ADR_PATH.read_text()
    assert REQUIRED_DECISION_TEXT in _collapsed(text)


def test_adr_0020_has_the_required_sections():
    text = ADR_PATH.read_text()
    for heading in ("## Context", "## Decision", "## Consequences", "## Alternatives considered"):
        assert heading in text
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_adr_0020_layer_floor_loop.py -q`
Expected: FAIL — `docs/adr/0020-layer-floor-loop.md` does not exist yet
(`FileNotFoundError` from `ADR_PATH.read_text()`).

- [ ] **Step 3: Write the ADR**

```markdown
# ADR-0020: The layer, the floor, and the loop

- **Status:** Accepted
- **Date:** 2026-09-08
- **Supersedes in part:** [ADR-0004](0004-layer-not-harness.md)

## Context

A 2026-09-05 dev-infra audit surveyed every published "agentic workshop" setup —
one always-on Linux box, every device a window into it over Tailscale, a
multiplexer (tmux, Herdr) holding the panes, a phone SSH client peeking in —
and found they all converge on the same shape. The finding that mattered: the
only piece nobody ships, across Herdr, Gas Town, and the rest, is the gate.
Panes, state, prompts, and push notifications are all assembled from
off-the-shelf parts; verifying a proposed action before it runs is not.

Two things had already happened on the ground that ADR-0004 (2026-07-02) did
not anticipate. Sprig (2026-08) is a minimal Go harness that calls
openDaisugi's `Gate` in-process — a standalone driving loop, built
deliberately on the layer, exactly the "separate product" ADR-0004's own
escape hatch described. And the cockpit (2026-08-27) already renders a
session tree, an ask queue, and a resident gate across multiple panes — it
already *is* a floor in miniature, just without a name or a pane backend of
its own.

ADR-0004 forbade "a chat loop" without distinguishing *driving* a model from
*supervising panes that hold other harnesses*. Read literally, it would
forbid sprig and the cockpit both — yet both exist, both pass their own test
suites, and neither has let a tool call bypass the gate. The rule was
protecting something real (the layer must not become one more harness,
competing for the same seat), but it named the wrong boundary. This ADR
restates the boundary precisely and gives the three-part shape a name: the
multiplexer of sprigs is a coppice — a stand of trees cut back so each stump
sends up straight shoots, the technique openDaisugi is named for.

## Decision

openDaisugi is three parts. The **layer** (gate, verifier, journal, garden) is a
library that imports nothing above it and runs alone in any harness. The **floor**
(coppice) supervises panes and sources every state it shows. The **loop** is
whatever harness runs in a pane; sprig is ours, the rest are rented. The invariant
that ADR-0004 protected is restated: **the layer stays importable alone.**
`tests/test_layer_boundary.py` enforces it.

Concretely, nothing under `src/opendaisugi/` that belongs to the layer may
import `opendaisugi.floor`, `opendaisugi.voice`, or `opendaisugi.coppice` —
not as a top-level import, not lazily inside a function, not conditionally on
a flag. The floor and the loop may depend on the layer in any direction they
like; the layer may never reach up. `opendaisugi.cockpit`,
`opendaisugi.dashboard`, `opendaisugi.start`, and the `opendaisugi.tui*`
modules are the floor's first citizens and are exempt from the layer-purity
check — they already supervise sessions; they are not the thing being
supervised.

## Consequences

- **Buys:** a name and a place for work that was already happening (sprig,
  the cockpit) without pretending it isn't. A mechanically checked boundary
  (`tests/test_layer_boundary.py`) instead of a prose rule nobody re-reads
  before adding an import. Room for the floor (coppice) and the loop (pi,
  OpenCode, Codex adapters) to grow across the sub-projects in
  `docs/plans/2026-09-08-workshop/` without re-litigating "are we allowed to
  build this."
- **Costs:** two more packages to keep disciplined (`opendaisugi.floor`,
  `opendaisugi.coppice`) and a Go binary (`harness/coppice`) alongside the
  Python core. A contributor who only read ADR-0004 will be surprised by
  sprig and the cockpit until they read this ADR too — the README index row
  exists so they find it.
- **Forecloses:** using "it's just a monitoring surface" to justify code that
  actually drives a model without going through the gate. The floor
  supervises; it does not decide. A floor module that starts making tool
  calls on its own, unverified, is the layer's boundary broken from the
  other side, and the import test does not catch that — it catches only the
  direction it was built to catch.

## Alternatives considered

- **Keep ADR-0004 as written, treat sprig and the cockpit as one-off
  exceptions.** Rejected — "exception" repeated twice is a pattern, and an
  unnamed pattern gets reinvented differently by the next contributor.
  Naming it lets the boundary be tested instead of argued.
- **Fold the loop into the layer** (ship sprig, or a driving loop, as part of
  `opendaisugi` itself). Rejected — this is exactly the
  competition-with-harnesses outcome ADR-0004 existed to prevent. The
  layer's value is that any harness can adopt it; a layer that *is* a
  harness stops being neutral ground.
- **Build the floor as a separate repository.** Rejected for now — coppice
  reads the gate's own `PaneStateEvent`s and the session tree on every pane,
  and the contracts (`00-master-spec.md` §3) are still being argued;
  splitting the repo before the interface is stable would fossilize a guess.
  Revisit once `opendaisugi.floor` and `harness/coppice` have shipped and the
  contract has held for a release or two.
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_adr_0020_layer_floor_loop.py -q`
Expected: PASS (3 passed).

- [ ] **Step 5: Commit**

```bash
git add docs/adr/0020-layer-floor-loop.md tests/test_adr_0020_layer_floor_loop.py
git commit -m "$(cat <<'EOF'
docs(adr): add ADR-0020, restating the boundary ADR-0004 named imprecisely

Sprig and the cockpit already exist as a driving loop and a floor-in-
miniature; ADR-0004 read literally forbids both. Name the three-part
shape (layer/floor/loop) and restate the invariant that actually
mattered: the layer stays importable alone.
EOF
)"
```

---

### Task 2: Wire the ADR index — ADR-0004's status line and `docs/adr/README.md`

**Files:**
- Modify: `docs/adr/0004-layer-not-harness.md:3`
- Modify: `docs/adr/README.md:18` (0004's row), insert a new row after `docs/adr/README.md:33` (0019's row)
- Test: `tests/test_adr_index.py` (new)

**Interfaces:**
- Consumes: `docs/adr/0020-layer-floor-loop.md` (Task 1).
- Produces: nothing further tasks import — this is the last docs-consistency
  wiring for the ADR pair.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_adr_index.py
"""Every ADR gets a row in the index, and a superseded ADR says so in both places.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, "Testing".
"""

from __future__ import annotations

from pathlib import Path

ADR_DIR = Path("docs/adr")


def _adr_files() -> list[Path]:
    return sorted(
        p for p in ADR_DIR.glob("[0-9][0-9][0-9][0-9]-*.md") if p.name != "0000-template.md"
    )


def test_every_adr_has_a_readme_row():
    readme = (ADR_DIR / "README.md").read_text()
    missing = [p.name for p in _adr_files() if f"({p.name})" not in readme]
    assert missing == [], f"ADRs missing a docs/adr/README.md row: {missing}"


def test_adr_0004_is_marked_superseded_in_both_places():
    adr_text = (ADR_DIR / "0004-layer-not-harness.md").read_text()
    readme = (ADR_DIR / "README.md").read_text()
    assert "**Status:** Superseded in part by ADR-0020" in adr_text
    row = next(line for line in readme.splitlines() if "[0004]" in line)
    assert "Superseded in part by ADR-0020" in row
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_adr_index.py -q`
Expected: FAIL — `test_every_adr_has_a_readme_row` fails because
`0020-layer-floor-loop.md` (created in Task 1) has no README row yet;
`test_adr_0004_is_marked_superseded_in_both_places` fails because ADR-0004's
status line still reads `Accepted` and its README row still reads `Accepted`.

- [ ] **Step 3: Update ADR-0004's status line**

`docs/adr/0004-layer-not-harness.md` currently reads (line 3):

```markdown
- **Status:** Accepted
```

Change it to:

```markdown
- **Status:** Superseded in part by ADR-0020
```

- [ ] **Step 4: Update `docs/adr/README.md`**

Change the 0004 row (line 18) from:

```markdown
| [0004](0004-layer-not-harness.md) | openDaisugi is a layer; MCP is the control pathway | Accepted |
```

to:

```markdown
| [0004](0004-layer-not-harness.md) | openDaisugi is a layer; MCP is the control pathway | Superseded in part by ADR-0020 |
```

Insert a new row directly after the 0019 row (line 33):

```markdown
| [0020](0020-layer-floor-loop.md) | The layer, the floor, and the loop | Accepted |
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_adr_index.py -q`
Expected: PASS (2 passed).

- [ ] **Step 6: Run the full ADR-related test set to check for regressions**

Run: `uv run --no-sync pytest tests/test_adr_index.py tests/test_adr_0020_layer_floor_loop.py -q`
Expected: PASS (5 passed).

- [ ] **Step 7: Commit**

```bash
git add docs/adr/0004-layer-not-harness.md docs/adr/README.md tests/test_adr_index.py
git commit -m "$(cat <<'EOF'
docs(adr): mark ADR-0004 superseded in part by ADR-0020, index both

A decision nobody can find isn't a decision: the index test asserts
every ADR has a row, and that a superseded ADR says so in its own
status line and in the table, not just one or the other.
EOF
)"
```

---

### Task 3: VISION.md — the three parts, and invariant 5 points at ADR-0020

**Files:**
- Modify: `VISION.md:54` (insert the "Three parts" section before "## The invariants")
- Modify: `VISION.md:73-75` (replace invariant 5)
- Test: `tests/test_vision_three_parts.py` (new)

**Interfaces:**
- Consumes: `docs/adr/0020-layer-floor-loop.md` (Task 1, linked from VISION.md).
- Produces: nothing further tasks import.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_vision_three_parts.py
"""VISION.md names the three parts and points invariant 5 at ADR-0020.

Spec: docs/plans/2026-09-08-workshop/spec-00-adr-0020-and-vision.md, deliverable D2.
"""

from __future__ import annotations

from pathlib import Path

VISION = Path("VISION.md")


def test_invariant_5_points_at_adr_0020():
    text = VISION.read_text()
    assert "5. **Layer, not harness.**" not in text
    assert "5. **The layer stays importable alone.**" in text
    assert "([ADR-0020](docs/adr/0020-layer-floor-loop.md))" in text


def test_three_parts_section_names_all_three():
    text = VISION.read_text()
    assert "## Three parts" in text
    section = text.split("## Three parts", 1)[1][:2000]
    assert "**The layer**" in section
    assert "**The floor**" in section
    assert "**The loop**" in section
    assert "coppice" in section.lower()


def test_three_parts_sits_between_what_this_is_and_the_invariants():
    text = VISION.read_text()
    assert (
        text.index("## What this is")
        < text.index("## Three parts")
        < text.index("## The invariants")
    )


def test_scorecard_untouched():
    # Out of scope per spec-00 — the v0.44.0 doc-refresh release owns the numbers.
    assert "As of v0.39.x:" in VISION.read_text()
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `uv run --no-sync pytest tests/test_vision_three_parts.py -q`
Expected: FAIL — `test_invariant_5_points_at_adr_0020` and
`test_three_parts_section_names_all_three` fail (old invariant 5 text still
present, "## Three parts" heading does not exist yet).
`test_scorecard_untouched` passes already; it stays in the file as a guard
against scope creep in the next step.

- [ ] **Step 3: Replace invariant 5**

`VISION.md` lines 73–75 currently read:

```markdown
5. **Layer, not harness.** openDaisugi gates actions; it does not drive the model.
   The moment it grows a chat loop it's competing with the harnesses it should
   plug into. ([ADR-0004](docs/adr/0004-layer-not-harness.md))
```

Replace with:

```markdown
5. **The layer stays importable alone.** openDaisugi may host a floor and own a
   loop, but the gate, verifier, journal, and garden import nothing above them
   and run in any harness without them. ([ADR-0020](docs/adr/0020-layer-floor-loop.md))
```

- [ ] **Step 4: Insert the "Three parts" section**

`VISION.md` line 54 ends the "## What this is" section (a blank line, then
line 55 is blank, then line 56 is `## The invariants (do not break these)`).
Insert the following new section between them, so it reads "## What this
is" → "## Three parts" → "## The invariants":

```markdown
## Three parts

openDaisugi is three parts, not one program. Each has its own owner and its
own promise:

| Part | What it is | Who owns the code | What must stay true |
|---|---|---|---|
| **The layer** | gate + verifier + journal + garden. `verify(plan ⊆ envelope)`, fail-closed. | ours, forever | importable alone; imports nothing above it |
| **The floor** | *coppice*: panes, state, prompt, steer, phone, voice. The shop floor. | ours, built to compete with Herdr and to *drive* Herdr | every state it shows is sourced, never guessed when a source exists |
| **The loop** | the harness in a pane: sprig, Claude Code, Codex, pi, OpenCode. | rented, except sprig | every tool call passes the gate; an unreachable gate blocks |

**The name.** A coppice is a stand of trees cut back so each stump sends up
straight shoots. Daisugi is a coppicing technique. The multiplexer of sprigs
is a coppice. (`garden` was taken by the pathway store; `grove` by the sprig
line.)

See [ADR-0020](docs/adr/0020-layer-floor-loop.md) for the reasoning, and
`tests/test_layer_boundary.py` for the test that enforces it.
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_vision_three_parts.py -q`
Expected: PASS (4 passed).

- [ ] **Step 6: Commit**

```bash
git add VISION.md tests/test_vision_three_parts.py
git commit -m "$(cat <<'EOF'
docs(vision): name the three parts, point invariant 5 at ADR-0020

Invariant 5 said "layer, not harness" while sprig and the cockpit
already exist. State the invariant that actually held (the layer
stays importable alone) and name what the rest is (floor, loop).
EOF
)"
```

---

### Task 4: `tests/test_layer_boundary.py` — the mechanical enforcement

**Files:**
- Create: `tests/test_layer_boundary.py`

**Interfaces:**
- Consumes: nothing from Tasks 1–3 (this test scans `src/opendaisugi/` directly;
  it does not read the ADR or VISION.md).
- Produces: `LAYER_EXCLUDED`, `UPPER_PACKAGES`, `_layer_modules()`,
  `_iter_layer_module_paths()`, `_is_upper()` — internal to this test module;
  spec-01 does not import them, it re-derives its own list if it needs one.

- [ ] **Step 1: Write the tests**

This is an invariant-guard test, not a bugfix: the codebase already satisfies
the boundary (there is no `opendaisugi.floor` yet for anything to import), so
there is no red state to turn green by editing source. The "TDD" cycle here
is: write the test, confirm it passes for the right reason (it actually
walks and imports every layer module — not vacuously, because
`_layer_modules()` returns none), and commit. Step 3 below double-checks
that it is not vacuous.

```python
# tests/test_layer_boundary.py
"""ADR-0020: the layer stays importable alone.

No module under ``opendaisugi`` that belongs to the layer may import
``opendaisugi.floor``, ``opendaisugi.voice``, or ``opendaisugi.coppice`` —
not as a top-level import, not lazily inside a function, not conditionally
on a flag. The floor and the loop may depend on the layer in any direction
they like; the layer may never reach up.

The packages ``opendaisugi.floor``, ``.voice``, ``.coppice`` do not exist on
disk yet (spec-01 creates ``floor`` first). Both tests below are written so
they keep working once those packages exist: the runtime test poisons their
names in ``sys.modules`` rather than relying on ``ModuleNotFoundError``, and
the static test matches on dotted-name prefix, not on whether the path
currently resolves to a real file.
"""

from __future__ import annotations

import ast
import importlib
import sys
from pathlib import Path

# Everything else under opendaisugi/ is the layer. A literal list, not a
# config file, so adding a floor module to this exclusion is a visible diff
# in a code review, not a silent config change.
LAYER_EXCLUDED = {
    "opendaisugi.tui",
    "opendaisugi.tui_base",
    "opendaisugi.tui_sessions",
    "opendaisugi.tui_tree",
    "opendaisugi.tui_wiring",
    "opendaisugi.cockpit",
    "opendaisugi.dashboard",
    "opendaisugi.start",
}
UPPER_PACKAGES = ("opendaisugi.floor", "opendaisugi.voice", "opendaisugi.coppice")

SRC_ROOT = Path("src")
PACKAGE_ROOT = SRC_ROOT / "opendaisugi"


def _is_upper(dotted: str) -> bool:
    """True if ``dotted`` is one of UPPER_PACKAGES or lives under one.

    Matches on ``pkg + "."`` prefix, not bare ``pkg`` prefix, so
    ``opendaisugi.floorplan`` (a hypothetical unrelated module) is never
    mistaken for something under ``opendaisugi.floor``.
    """
    return any(dotted == pkg or dotted.startswith(pkg + ".") for pkg in UPPER_PACKAGES)


def _iter_layer_module_paths() -> list[tuple[str, Path]]:
    """(dotted module name, source file) for every layer module on disk."""
    out: list[tuple[str, Path]] = []
    for path in sorted(PACKAGE_ROOT.rglob("*.py")):
        if "__pycache__" in path.parts:
            continue
        rel = path.relative_to(SRC_ROOT)
        parts = list(rel.parts)
        parts[-1] = parts[-1][:-3]  # strip ".py"
        if parts[-1] == "__init__":
            parts = parts[:-1]
        dotted = ".".join(parts)
        if dotted in LAYER_EXCLUDED or _is_upper(dotted):
            continue
        out.append((dotted, path))
    return out


def _layer_modules() -> list[str]:
    return [dotted for dotted, _ in _iter_layer_module_paths()]


def test_upper_package_prefix_matching_is_exact():
    """Guards the _is_upper helper itself against a substring-prefix bug."""
    assert _is_upper("opendaisugi.floor")
    assert _is_upper("opendaisugi.floor.pane")
    assert _is_upper("opendaisugi.coppice.client")
    assert not _is_upper("opendaisugi.floorplan")
    assert not _is_upper("opendaisugi.gate")


def test_layer_modules_import_with_upper_packages_hidden(monkeypatch):
    """Hiding floor/voice/coppice must not break importing any layer module.

    ``sys.modules[name] = None`` makes ``import name`` (and
    ``from name import x``) raise ImportError even when the package exists
    on disk — the mechanism that lets this test keep enforcing the boundary
    once spec-01 creates the real ``opendaisugi.floor`` package.
    """
    modules = _layer_modules()
    assert len(modules) > 100, "the module walk found suspiciously few layer modules"
    for pkg in UPPER_PACKAGES:
        monkeypatch.setitem(sys.modules, pkg, None)
    for mod in modules:
        importlib.import_module(mod)  # must not raise


def test_layer_modules_have_no_static_upper_imports():
    """AST scan: no layer module names an upper package in any import, ever.

    ``ast.walk`` descends into function and method bodies, so a lazy,
    function-local ``import opendaisugi.floor`` is caught exactly like a
    top-level one — the rule is structural, not about when the code runs.
    """
    offenders: list[tuple[str, str]] = []
    module_paths = _iter_layer_module_paths()
    assert len(module_paths) > 100, "the module walk found suspiciously few layer modules"
    for dotted, path in module_paths:
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                for alias in node.names:
                    if _is_upper(alias.name):
                        offenders.append((dotted, alias.name))
            elif isinstance(node, ast.ImportFrom):
                if node.level:
                    continue  # relative import; no layer module uses them today
                if node.module and _is_upper(node.module):
                    offenders.append((dotted, node.module))
    assert offenders == [], f"layer modules importing upper packages: {offenders}"
```

- [ ] **Step 2: Run the tests to verify they pass**

Run: `uv run --no-sync pytest tests/test_layer_boundary.py -q`
Expected: PASS (4 passed). This confirms both the guard-correctness test
(Step 1's `_is_upper` edge case) and the two boundary tests from spec-00 D3
hold today, with `_layer_modules()` finding well over 100 real modules (not
an empty, vacuously-passing list).

- [ ] **Step 3: Confirm the test is not vacuous by breaking it on purpose, then reverting**

This step is manual verification, not a commit. Temporarily add a bad import
to confirm the static test actually fires, then remove it — do not commit
either the break or a fix for it (there is nothing to fix; the source is
already clean).

```bash
python3 - <<'PYEOF'
import pathlib
p = pathlib.Path("src/opendaisugi/gate.py")
original = p.read_text()
p.write_text(original.replace(
    "from __future__ import annotations\n",
    "from __future__ import annotations\n\nimport opendaisugi.floor  # TEMP: prove the boundary test fires\n",
    1,
))
PYEOF
uv run --no-sync pytest tests/test_layer_boundary.py -q
git checkout -- src/opendaisugi/gate.py
```

Expected on the middle command: FAIL —
`test_layer_modules_have_no_static_upper_imports` reports
`[('opendaisugi.gate', 'opendaisugi.floor')]` (or similar) in its assertion
message, and `test_layer_modules_import_with_upper_packages_hidden` also
fails with `ImportError` while importing `opendaisugi.gate`. The final
`git checkout` restores the file; confirm with `git status --porcelain`
that `src/opendaisugi/gate.py` shows no diff before continuing.

- [ ] **Step 4: Run the full test file once more on the clean tree**

Run: `uv run --no-sync pytest tests/test_layer_boundary.py -q`
Expected: PASS (4 passed).

- [ ] **Step 5: Lint**

Run: `uv run --no-sync ruff check tests/test_layer_boundary.py`
Expected: no findings.

- [ ] **Step 6: Commit**

```bash
git add tests/test_layer_boundary.py
git commit -m "$(cat <<'EOF'
test(layer): enforce ADR-0020's boundary — the layer stays importable alone

Two checks, not one: importing every layer module with floor/voice/
coppice poisoned in sys.modules (runtime), and an ast.walk over every
import statement including inside function bodies (static, catches a
lazy import a runtime check would miss if the module were already
cached). Verified non-vacuous by a manual break-then-revert of a
throwaway import in gate.py (not committed).
EOF
)"
```

---

## Final check across all four tasks

- [ ] **Run the whole set together**

Run:
```bash
uv run --no-sync pytest tests/test_adr_0020_layer_floor_loop.py tests/test_adr_index.py tests/test_vision_three_parts.py tests/test_layer_boundary.py -q
```
Expected: PASS (13 passed: 3 + 2 + 4 + 4).

- [ ] **Lint the new test files**

Run: `uv run --no-sync ruff check tests/test_adr_0020_layer_floor_loop.py tests/test_adr_index.py tests/test_vision_three_parts.py tests/test_layer_boundary.py`
Expected: no findings.

- [ ] **Full suite sanity**

Run: `uv run --no-sync pytest -q`
Expected: PASS, same count as before this plan plus 13. No pre-existing test
should change status — this plan adds new modules only; it modifies no
`src/` file (Task 2 and Task 3 touch only `docs/adr/*.md` and `VISION.md`).

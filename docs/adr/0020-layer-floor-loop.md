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

## Amendment, 2026-09-24: the layer is called daisugi

"The layer" was unclear to the one person who reads this work every day. The
part made of the gate, the verifier, the journal and the garden is now called
**daisugi**, which is already the name of its command. The whole project stays
**openDaisugi**. "The gate" names only the part that allows or denies a call.
daisugi has three modes, and the screen shows them in words: enforcing (checks,
blocks, journals), watching (checks and journals, never blocks; the config
value is `shadow`), and off. Code names such as `tests/test_layer_boundary.py`
keep their names. The rule they test is unchanged: daisugi stays importable
alone.

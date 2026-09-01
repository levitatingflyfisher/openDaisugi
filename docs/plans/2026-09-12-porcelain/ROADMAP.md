# The porcelain roadmap

Written 2026-09-12 after the multi-agent management research
(`docs/research/multi-agent-management-landscape-2026-09-12.md`) and the field
boards (`docs/research/boards/`). This document is the map. Each plan below gets
its own spec and plan under this directory before it is built.

## The idea in three layers

- **Layer.** The gate and the garden. Importable alone. A verifier in front of
  every action, a store of verified pathways behind it. Fail closed.
- **Floor.** coppice. One Go binary that owns every pane and its state, and one
  socket every screen speaks. Never an unknown state.
- **Loop.** Any harness. A command and its args in one config line.

The conversation sits on top. You type `coppice`, then nearly everything you type
is words to a foreman, which is a normal frontier pane that holds the floor verbs
as tools. The gate stays outside the foreman. The permission layer and the floor
state are the two things the research says a model does not absorb.

## Rules that bind every plan

- Six keys in the roster: ctrl-t, shift-tab, ctrl-w, Space, Enter, Esc. One
  stolen key inside a pane: ctrl-], the escape telnet and virsh console
  already teach, claimed by no harness. Twice sends it to the pane. Set
  `[keys] leave` in coppice.toml to change it. No prefix, no chords.
- One shape everywhere: the roster plus as many live panes as the width allows.
  Tiles never rearrange themselves.
- A pane has at most one tile. Every pane has a row. Closing a tile never closes
  a pane.
- Three tiers of allow: silent inside the envelope, one key when undoable, type
  the pane name when permanent. Deny is always one key.
- Every view is a plugin over the socket, every shipped view is on by default,
  and every mark on a view means a state the socket reports.
- No plugin kind exists in the spec until three examples run against it.
- The foreman is any pane that has read one page. Its tools are commands in
  its shell, never a tool schema, because a schema is context paid for on every
  turn and a command costs nothing until it runs. Not a small local model. A
  team is a node with children and no model.
- The floor is one static Go binary. Python keeps the gate and the daisugi CLI.
  Rust and Lean stay as gate mirrors.
- Every research page archives its screens in the repo with source and fetch
  date.

## Remaining from the 2026-09-08 workshop, paused

- **Plan 04, pi.** coppice drives pi as a pane, and a pi extension puts every pi
  tool call through the gate.
- **Plan 05, OpenCode.** coppice drives OpenCode over its HTTP and SSE server,
  with a fail-closed gate plugin on every tool call.
- **Plan 08, model-host route.** One command probes any self-hosted model box
  on the tailnet and wires it in as a tier.
- **Plan 09, Switchyard.** Switchyard sits upstream of the gateway as the
  chooser while the gateway stays the meter.
- **Plan 10, int8 matcher.** A quantized MiniLM through onnxruntime with no
  torch, blocked first on the dev-extras fix.
- **Plan 11, layer management.** One bench command per layer, five cross-layer
  pairs, hot swap where honest, a verifier dispatch that falls back loudly and
  never to allow, plus the plan 06 and 07 carries.

## New, in build order

- **Plan 12, honest state.** Kill the unknown state, ghost panes, silent
  attach, and the float pid. A row says working, needs you, done, or quiet with
  a duration, and nothing else.
- **Plan 13, the wire and the spec.** Newline JSON-RPC over the socket with the
  nine verbs plus fork, an async notification stream, stable ids, per-pane flow
  control, published as a spec with a conformance suite.
- **Plan 14, the floor binary.** Move the terminal client into the Go binary so
  server, roster, and attach are one static file.
- **Plan 15, one shape.** Roster plus as many live panes as the width allows,
  zero to five, in terminal and browser alike, with the mouse on every row and
  tile, layouts all, focus, and one, swap by drag or two clicks, reset to roster
  order, and a lock.
- **Plan 16, first run.** PATH discovery, one question at most, a default
  harness, Enter as the whole first session, and an honest line for app-only
  harnesses.
- **Plan 17, the six keys and one to leave.** The roster keys, and one stolen
  key inside a pane chosen against Claude Code's and Codex's own bindings.
- **Plan 18, tasks, teams, and trees.** A node with a name, a worktree on real
  disk, a parent, children, and a model or none. State bubbles up. The tree view
  in text and as a graph.
- **Plan 19, the foreman.** The `coppice` command as the foreman's hands,
  taught by one page, no MCP. A pane's connection can never allow, every
  command it runs prints in dim, sentences go to it, subagents show as
  read-only child rows.
- **Plan 20, three tiers of allow.** Identical on terminal, floor page, and
  phone. The tier is a field on the verdict.
- **Plan 21, plugins.** A manifest and an id in one config file, view and
  policy kinds, the `coppice.` namespace reserved, every shipped view on by
  default, hot-swappable with selection carried across. Policy triad: merge on
  green, close a pane quiet for an hour and keep its worktree, pause a pane past
  a turn budget. Notifier triad: ntfy, the lock-screen card, voice.
- **Plan 22, the view suite.** Tree, minimap, kanban, colony with foragers,
  shift log, a Herdr-shaped grid, and a lazybox-shaped inbox, each small, each
  proving the spec is enough.
- **Plan 23, floor page and phone.** As many tiles as fit, the rail with the
  minimap, the ask in an amber bar, the attention queue, the quiet screen, the
  lock-screen card with Deny and Look, on top of plans 06 and 07.
- **Plan 24, bridges.** Neovim and tmux control mode as example plugins, the
  Herdr attach bridge so Herdr shows our panes with no Herdr change, and
  ACP-adjacent types.
- **Plan 25, the stack.** A foreman's children one level down, Enter pushes and
  Esc pops, breadcrumb in the bar, proven at depth two before anything deeper.
- **Plan 26, many hands.** More than one human on a floor over the tailnet, a
  name per person, and the gate recording who allowed what. Designed for now,
  built last.

Plans 12 and 13 come first because everything after reads state and speaks the
wire. Plans 04, 05, and 08 to 11 keep their own lanes.

## The plans

Each plan is written for an implementer with no context, in the writing-plans
shape: files, interfaces, a failing test, the fix, a commit.

- plan-12-honest-state.md
- plan-13-wire-and-spec.md
- plan-14-floor-binary.md
- plan-15-one-shape.md
- plan-16-first-run.md
- plan-17-six-keys.md
- plan-18-tasks-teams-trees.md
- plan-19-foreman.md
- plan-20-three-tiers.md
- plan-21-plugins.md
- plan-22-view-suite.md
- plan-23-floor-page-and-phone.md
- plan-24-bridges.md
- plan-25-the-stack.md
- plan-26-many-hands.md

Order is the order above. Plans 04, 05, and 08 to 11 keep their files under
`docs/plans/2026-09-08-workshop/`.

## Decisions still open

- The DHH source for sixteen parallel sessions, to add beside the OpenAI
  three-to-five anecdote.
- Whether coppice is also an ACP agent server, without losing fail-closed allow
  and deny.

## What is next

1. Plans 12 to 26 are written. Run plan 12 first, then 13.
2. A `field-board` skill in iss-skills so a board is one command from a report.
3. Execution resumes when usage allows, one implementer and one reviewer at a
   time, plan 12 first.

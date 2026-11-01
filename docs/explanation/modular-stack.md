# The modular stack

An agent stack has slots. Each openDaisugi piece fills one. You can put our piece in a slot,
someone else's, or your own. The table of parts is in the [README](../../README.md#what-fills-each-part).
Other tools that fill each slot are in [the landscape](landscape.md). Worked mixes are in
[recipes](../recipes/README.md).

## The idea

**Separate what is allowed from what is decided.** A model decides. An envelope says what is
allowed. A verifier proves each action stays inside the envelope before it runs.

The same idea shapes the stack. The piece that decides (a harness, a model, a router) is not the
piece that allows (the warden). So you can swap, add or remove one piece without changing another.

## The slot rule

- Each piece fills one slot.
- Any slot can be empty. "No warden" and "no router" are normal choices.
- Some slots sit inside another. The routing mode sits inside the router, so a routing mode
  needs a router.
- Nothing is on until you choose it. `daisugi modules --pieces` shows what runs here.
- Pieces share files, not calls. They read the same data directory. If a piece is absent, its
  file is absent, and the reader goes on.
- Where an install command adds more than you asked for, the docs say so. The Python
  `daisugi install` always adds the skill, the MCP server and the capture hook. The Go one
  writes only the warden and router layers.

## The routing mode slot

The routing mode is how the router picks the model for a turn. Five fillers work today. Pick how smart you
want it. `off` and `rules` cost nothing per turn. `knn` is cheap and local. `model` costs one
short model call per turn. The grader sits beside them.

- `off`: the router forwards each turn and only meters it.
- `rules`: the built-in rules. They send easy turns to a cheaper model. It is the default.
- `switchyard`: NVIDIA NeMo Switchyard runs behind the router as a child process.
  See [the Switchyard guide](../how-to/routing-mode-switchyard.md).
- `knn`: the router learns from outcomes. It sends a turn to the model that did well on
  similar past turns. It costs one local embedding.
- `model`: a model you name picks from the allowed names. It costs one short model call.
- The grader: `router_grader: <model>` scores a finished answer, good or bad, after the
  reply. It feeds `knn`. It is not a routing mode.

Details: [pick how smart the routing mode is](../how-to/router.md#pick-how-smart-the-routing-mode-is).

The router sits behind the router, so the router sees which model served each turn. Any router
that speaks the Anthropic Messages wire at a URL works with
`daisugi router serve --routing-mode off --upstream <url>`. The router then cannot book a saving by target.

## Three ways to save tokens

| Saver | The question it answers | What it does |
|---|---|---|
| Router | Which model serves a turn that must reach a model? | Routes easy turns to a cheaper model and meters every turn. |
| Distillation | Does this work need a model at all? | Moves repeated work down a ladder: large model, small model, skill, plain script. |
| History and knowledge | What was said, and what is known? | Keeps context small and fetches what matters. Saves nothing by itself. |

They meet in one place: pathway reuse is distillation's result, served at the router.

## History, knowledge and skills

- **History**: what was said and done. Ours is Pando R2, the steward's conversation history.
- **Knowledge**: facts. We have no store. Bring your own (Mem0, Graphiti and others).
  Others call history and knowledge together agent memory.
- **Skills**: how to do things. Skills and pathways are this kind, and they belong to
  distillation, not to history or knowledge.

## The warden checks meaning, not shape

Constrained output (JSON schema, grammar decoding) makes a model's output parse. It cannot tell
`file_write: ["**"]` from a safe envelope. Harness permission rules and MCP input schemas say
what a tool accepts. The envelope says what this task may do, across tools: "keep src/
unchanged" covers file writes, `cp`, `mv`, `tee` and `sed -i`. Z3 proves each call stays inside
the envelope before it runs. Unknown means deny. We do not hand-build words to help models
write envelopes. Models write well without them. Words come from the journal, when the same long
envelope keeps coming back.

| Tool | Proves | Example |
|---|---|---|
| Constrained output | shape | valid JSON |
| Harness rules | which tool | `Bash(git push:*)` |
| MCP schema | argument types | `repo` is a string |
| `daisugi warden` | meaning for this task | no write lands in `src/` |

## Why not one install

One command that turns everything on is easy to build and hard to trust. A user who wants a
metered proxy would also carry a warden hook. Each extra piece is a place for a failure to hide.
[ADR-0020](../adr/0020-layer-floor-loop.md) records why daisugi, coppice and sprig are separate
programs. This page applies the same rule to the pieces inside daisugi.

# The modular stack

An agent stack has slots. Each openDaisugi piece fills one. You can put our piece in a slot,
someone else's, or your own. The slot table is in the [README](../../README.md#what-fills-each-slot).
Other tools that fill each slot are in [the landscape](landscape.md). Worked mixes are in
[recipes](../recipes/README.md).

## The idea

**Separate what is allowed from what is decided.** A model decides. An envelope says what is
allowed. A verifier proves each action stays inside the envelope before it runs.

The same idea shapes the stack. The piece that decides (a harness, a model, a router) is not the
piece that allows (the gate). So you can swap, add or remove one piece without changing another.

## The slot rule

- Each piece fills one slot.
- Any slot can be empty. "No gate" and "no router" are normal choices.
- Some slots sit inside another. The router sits inside the gateway, so a router needs a
  gateway.
- Nothing is on until you choose it. `daisugi modules --pieces` shows what runs here.
- Pieces share files, not calls. They read the same data directory. If a piece is absent, its
  file is absent, and the reader goes on.
- Where an install command adds more than you asked for, the docs say so. The Python
  `daisugi install` always adds the skill, the MCP server and the capture hook. The Go one
  writes only the gate and gateway layers.

## The router slot

The router picks the model for a gateway turn. Five fillers work today. Pick how smart you
want it. `off` and `rules` cost nothing per turn. `knn` is cheap and local. `model` costs one
short model call per turn. The grader sits beside them.

- `off`: the gateway forwards each turn and only meters it.
- `rules`: the built-in router. It sends easy turns to a cheaper model. It is the default.
- `switchyard`: NVIDIA NeMo Switchyard runs behind the gateway as a child process.
  See [the Switchyard guide](../how-to/router-switchyard.md).
- `knn`: the router learns from outcomes. It sends a turn to the model that did well on
  similar past turns. It costs one local embedding.
- `model`: a model you name picks from the allowed names. It costs one short model call.
- The grader: `gateway_grader: <model>` scores a finished answer, good or bad, after the
  reply. It feeds `knn`. It is not a router.

Details: [pick how smart the router is](../how-to/token-saving-gateway.md#pick-how-smart-the-router-is).

The router sits behind the gateway, so the gateway sees which model served each turn. Any router
that speaks the Anthropic Messages wire at a URL works with
`daisugi gateway --router off --upstream <url>`. The gateway then cannot book a saving by target.

## Three ways to save tokens

| Saver | The question it answers | What it does |
|---|---|---|
| Gateway | Which model serves a turn that must reach a model? | Routes easy turns to a cheaper model and meters every turn. |
| Distillation | Does this work need a model at all? | Moves repeated work down a ladder: large model, small model, skill, plain script. |
| History and knowledge | What was said, and what is known? | Keeps context small and fetches what matters. Saves nothing by itself. |

They meet in one place: pathway reuse is distillation's result, served at the gateway.

## History, knowledge and skills

- **History**: what was said and done. Ours is the foreman's conversation history.
- **Knowledge**: facts. We have no store. Bring your own (Mem0, Graphiti and others).
  Others call history and knowledge together agent memory.
- **Skills**: how to do things. Skills and pathways are this kind, and they belong to
  distillation, not to history or knowledge.

## The gate checks meaning, not shape

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
| `daisugi gate` | meaning for this task | no write lands in `src/` |

## Why not one install

One command that turns everything on is easy to build and hard to trust. A user who wants a
metered proxy would also carry a gate hook. Each extra piece is a place for a failure to hide.
[ADR-0020](../adr/0020-layer-floor-loop.md) records why daisugi, coppice and sprig are separate
programs. This page applies the same rule to the pieces inside daisugi.

# Recipes

A recipe says which tool fills each slot. Each one has a slot table and one install block.
Run `--dry-run` first on any `daisugi install` to see what it writes. The slots are explained
in [the modular stack](../explanation/modular-stack.md). Other tools that fill them are in
[the landscape](../explanation/landscape.md).

| Recipe | What it gives you |
|---|---|
| [Gate only, in Claude Code](gate-only.md) | A proof check on each tool call. Nothing installed. |
| [Gateway and router, in front of any harness](gateway-router.md) | Metered turns and cheaper models for easy turns. No gate. |
| [coppice, Claude Code and the gate](coppice-claude-gate.md) | Many guarded agents in one terminal, browser or phone. |
| [Bring your own: Switchyard, Hermes Agent, Mem0](bring-your-own.md) | Our gateway with other tools in three slots. No gate. |

A recipe file (`recipe.yaml`, applied with `daisugi recipe apply`) is planned. It does not
exist yet. Today you run the blocks by hand.

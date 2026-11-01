# Recipes

A recipe says which tool fills each slot. Each one has a slot table and one install block.
Run `--dry-run` first on any `daisugi install` to see what it writes. The slots are explained
in [the modular stack](../explanation/modular-stack.md). Other tools that fill them are in
[the landscape](../explanation/landscape.md).

| Recipe | What it gives you | Command |
|---|---|---|
| [Warden only, in Claude Code](warden-only.md) | A proof check on each tool call. | `daisugi recipe apply warden-only --set workspace=~/Work` |
| [Router and router, in front of any harness](router-and-routing-mode.md) | Metered turns and cheaper models for easy turns. No warden. | `daisugi recipe apply router-and-routing-mode` |
| [coppice, Claude Code and the warden](coppice-claude-warden.md) | Many guarded agents in one terminal, browser or phone. | `daisugi recipe apply coppice-claude-warden --set workspace=~/Work` |
| [Bring your own: Switchyard, Hermes Agent, Mem0](bring-your-own.md) | Our router with other tools in three slots. No warden. | `daisugi recipe apply bring-your-own --set efficient_model=qwen3-coder:30b` |

Each recipe is a data file inside the binary. `daisugi recipe list` names them; `daisugi recipe
show NAME` prints the steps; `daisugi recipe apply NAME` shows what it would write and writes it with
`--yes`; `daisugi recipe revert NAME` undoes it from the receipt. A step runs one install primitive, never a
shell command. The receipt is `<data>/recipes/NAME.json`. It records only what the recipe added: a hook or
base_url that was there before is not removed by `revert`. If an uninstall is refused, `revert` says why, keeps
the receipt for the steps not undone, and exits 1.

# Recipe: the warden alone, in Claude Code

| Slot | Filled by |
|---|---|
| Agent harness | Claude Code (theirs) |
| Agent multiplexer | none |
| Safety warden | `daisugi warden` (ours) |
| Router and router | none |
| Worker tree, distillation, history, knowledge | none |

To write it with one command: `daisugi recipe apply warden-only --set workspace=~/Work --yes`. Leave out
`--yes` to see what it would write.

By hand (nothing is installed; the hook lives in this one session):

```bash
daisugi warden init --workspace ~/Work    # register an envelope: reads and writes in ~/Work
claude --settings "$(daisugi warden settings)"
```

The warden runs in audit mode: it logs what it would deny and lets the call run. Read
`daisugi warden report`. Then add `--enforce` to `daisugi warden settings` to deny.

To make the hook permanent, run `daisugi install --only warden`. It writes the hook in audit mode and
nothing else. A plain `daisugi install` also writes the skill, the MCP server and the capture hook. Add
`--enforce` to make the hook deny.

More: [warden a live session](../how-to/warden.md).

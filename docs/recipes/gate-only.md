# Recipe: the gate alone, in Claude Code

| Slot | Filled by |
|---|---|
| Agent harness | Claude Code (theirs) |
| Agent multiplexer | none |
| Safety gate | `daisugi gate` (ours) |
| Gateway and router | none |
| Worker tree, distillation, history, knowledge | none |

Nothing is installed. The hook lives in this one session.

```bash
daisugi gate init --workspace ~/Work    # register an envelope: reads and writes in ~/Work
claude --settings "$(daisugi gate settings)"
```

The gate runs in audit mode: it logs what it would deny and lets the call run. Read
`daisugi gate report`. Then add `--enforce` to `daisugi gate settings` to deny.

To make the hook permanent, run `daisugi install --gate`. The Go build writes only the hook.
The Python build also adds the skill, the MCP server and the capture hook.

More: [gate a live session](../how-to/gate.md).

# Recipe: coppice, Claude Code and the gate

| Slot | Filled by |
|---|---|
| Agent harness | Claude Code (theirs) |
| Agent multiplexer | coppice (ours) |
| Views | terminal, web, phone (ours) |
| Safety gate | `daisugi gate` (ours) |
| Gateway, history, knowledge | none |

```bash
daisugi gate init --workspace ~/Work    # a starter envelope, so you can enforce later
daisugi install --gate                  # the gate hook for Claude Code; audit mode
coppice                                 # or: coppice web, for the browser and the phone
```

The Python `daisugi install --gate` also adds the skill, the MCP server and the capture hook.
The Go one writes only the hook. To enforce, run `daisugi install --gate --enforce` after
`gate init`.

coppice shows each agent's state from the gate, not from screen scraping.

More: [coppice](../../harness/coppice/README.md), [the phone](../how-to/phone.md).

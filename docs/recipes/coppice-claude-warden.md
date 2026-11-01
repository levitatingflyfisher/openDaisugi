# Recipe: coppice, Claude Code and the warden

| Slot | Filled by |
|---|---|
| Agent harness | Claude Code (theirs) |
| Agent multiplexer | coppice (ours) |
| Views | terminal, web, phone (ours) |
| Safety warden | `daisugi warden` (ours) |
| Router, history, knowledge | none |

One command for the first two lines below: `daisugi recipe apply coppice-claude-warden --set workspace=~/Work --yes`.

By hand:

```bash
daisugi warden init --workspace ~/Work    # a starter envelope, so you can enforce later
daisugi install --only warden           # the warden hook for Claude Code; audit mode
coppice                                 # or: coppice web, for the browser and the phone
```

`daisugi install` alone also writes the skill, the MCP server, the capture hook and the pathway note.
To enforce, run `daisugi install --only warden --enforce` after `warden init`.

coppice shows each agent's state from the warden, not from screen scraping.

More: [coppice](../../harness/coppice/README.md), [the phone](../how-to/phone.md).

# Recipe: the router and its routing mode, in front of any harness

| Slot | Filled by |
|---|---|
| Agent harness | any harness that lets you set a `base_url` (Claude Code below) |
| Safety warden | none |
| Router | `daisugi router serve` (ours) |
| Routing mode (inside the router) | `rules` by default; `knn` or `model` to be smarter; `off` to only meter |
| Everything else | none |

To write the base URL with one command: `daisugi recipe apply router-and-routing-mode --yes`. Add
`--set routing_mode=knn` to choose the routing mode. Start the proxy after that.

By hand (nothing is installed; start the proxy and point the harness at it):

```bash
daisugi router serve                          # routing mode: rules; add --routing-mode off to meter only
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude
```

To have the router learn, run `daisugi router serve --routing-mode knn`. Correct a turn with
`daisugi route feedback <turn> good|bad`. Turn ids show in `daisugi router status`.

Codex speaks the OpenAI wire. The same proxy answers it on `/chat/completions`.

The router meters each turn and sends easy turns to a cheaper model. A turn that needs the
large model still goes to it. Savings depend on your work.

More: [the token-saving router](../how-to/router.md).

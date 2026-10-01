# Recipe: the gateway and router, in front of any harness

| Slot | Filled by |
|---|---|
| Agent harness | any harness that lets you set a `base_url` (Claude Code below) |
| Safety gate | none |
| Gateway | `daisugi gateway` (ours) |
| Router (inside the gateway) | `rules` by default; `knn` or `model` to be smarter; `off` to only meter |
| Everything else | none |

Nothing is installed. Start the proxy and point the harness at it.

```bash
daisugi gateway                          # router: rules; add --router off to meter only
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude
```

To have the router learn, run `daisugi gateway --router knn`. Correct a turn with
`daisugi route feedback <turn> good|bad`. Turn ids show in `daisugi router status`.

Codex speaks the OpenAI wire. The same proxy answers it on `/chat/completions`.

The gateway meters each turn and sends easy turns to a cheaper model. A turn that needs the
large model still goes to it. Savings depend on your work.

More: [the token-saving gateway](../how-to/token-saving-gateway.md).

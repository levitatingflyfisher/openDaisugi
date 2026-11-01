# Recipe: bring your own

This recipe mixes our router with other people's tools. It has no warden.

| Slot | Filled by | Do we install it? |
|---|---|---|
| Agent harness | [Hermes Agent](https://hermes-agent.nousresearch.com/docs) | No. Install it from its docs. |
| Router | `daisugi router serve` (ours) | Yes. |
| Routing mode (inside the router) | [NVIDIA NeMo Switchyard](https://github.com/NVIDIA-NeMo/Switchyard) | Not by default. You build it with cargo. |
| Knowledge store | [Mem0](https://github.com/mem0ai/mem0) | No. Install it from its docs. |
| Safety warden | none | |

To write the router parts with one command: `daisugi recipe apply bring-your-own --set efficient_model=qwen3-coder:30b --yes`.
It does not build Switchyard and does not touch Hermes or Mem0.

By hand:

```bash
cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server
daisugi install --router --routing-mode switchyard --efficient-model qwen3-coder:30b
daisugi router serve
```

`daisugi install --router` does not point Hermes at the router. It expects an OpenAI wire
that `install` does not write. Set Hermes's OpenAI-compatible base URL to
`http://127.0.0.1:8787` by hand. Check the setting name in the Hermes docs. We have not
tested this pairing.

Mem0 and Hermes join by their own means. Our router does not know about Mem0.

More: [hand model choice to Switchyard](../how-to/routing-mode-switchyard.md).

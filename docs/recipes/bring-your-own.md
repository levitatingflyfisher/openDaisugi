# Recipe: bring your own

This recipe mixes our gateway with other people's tools. It has no gate.

| Slot | Filled by | Do we install it? |
|---|---|---|
| Agent harness | [Hermes Agent](https://hermes-agent.nousresearch.com/docs) | No. Install it from its docs. |
| Gateway | `daisugi gateway` (ours) | Yes. |
| Router (inside the gateway) | [NVIDIA NeMo Switchyard](https://github.com/NVIDIA-NeMo/Switchyard) | Not by default. You build it with cargo. |
| Knowledge store | [Mem0](https://github.com/mem0ai/mem0) | No. Install it from its docs. |
| Safety gate | none | |

```bash
cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server
daisugi install --gateway --router switchyard --efficient-model qwen3-coder:30b
daisugi gateway
```

`daisugi install --gateway` does not point Hermes at the gateway. It expects an OpenAI wire
that `install` does not write. Set Hermes's OpenAI-compatible base URL to
`http://127.0.0.1:8787` by hand. Check the setting name in the Hermes docs. We have not
tested this pairing.

Mem0 and Hermes join by their own means. Our gateway does not know about Mem0.

More: [hand model choice to Switchyard](../how-to/router-switchyard.md).

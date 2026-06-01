# Integrations

openDaisugi is a library, not a framework. It ships narrow adapters
for four external surfaces so agent frameworks, training pipelines,
and simulators can consume it without reimplementation.

## Hermes (Python, direct import)

Hermes is a Python skill framework. The adapter is a plain module:

```python
from opendaisugi.integrations import hermes

envelope = hermes.envelope_from_yaml("./robin.envelope.yaml")
violations = hermes.verify_step(completed_step, envelope)  # Stage 2
result = hermes.verify_plan(proposed_plan, envelope)       # Stage 1
registry = hermes.load_household_aliases("./household_aliases.yaml")
```

Four functions cover the lifecycle — load envelope from disk, Stage 1
structural verify, Stage 2 post-execution verify, household-tier
alias overrides.

**When to use:** any Python agent that runs in the same process as
openDaisugi.

## OpenClaw (Node.js, MCP over stdio)

OpenClaw runs Node. openDaisugi runs Python. They bridge through MCP:

```bash
uv add 'opendaisugi[mcp]'
cd examples/integrations/openclaw && npm install && npm run demo
```

The Node client spawns `daisugi mcp serve` as a subprocess and calls
three tools:

```js
import { OpenDaisugiClient } from "./client.mjs";
const od = new OpenDaisugiClient();
await od.connect();
const env = await od.envelopeFor("send email");
const result = await od.verifyPlan(plan, env);
const violations = await od.verifyCompletedStep(step, env);
```

**When to use:** any non-Python agent, or any Python agent that needs
process-boundary isolation.

The same MCP server works for Claude Code and any other MCP client.

## LoRA (GPU box, training script)

The dataset side ships in `opendaisugi.lora.dataset`:

```python
from opendaisugi.lora import emit_jsonl
stats = emit_jsonl(journal, Path("train.jsonl"), format="alpaca")
```

The trainer is a CLI on the GPU box:

```bash
uv add 'opendaisugi[lora]'
python -m opendaisugi.lora.train \
    --jsonl train.jsonl \
    --base-model Qwen/Qwen2.5-1.5B-Instruct \
    --output adapters/robin \
    --qlora
```

QLoRA + Qwen-1.5B fits in 16 GB VRAM (RTX 4080). Heavy deps are
lazy-imported — the trainer module itself is importable on the
development laptop; only `_train` touches `torch`, `peft`, `trl`,
`bitsandbytes`.

**When to use:** distilling a fine-tuned model of one agent's
envelope-generation behavior, so Tier-1 weights (not Tier-1 prompts)
encode the conventions.

## MuJoCo (physical executor + smoke kit)

MuJoCo is an optional extra:

```bash
uv add 'opendaisugi[robotics]'
python examples/integrations/mujoco/smoke.py
```

The smoke kit closes the envelope/executor loop: declared bounds in
the envelope, real `mj_step` rollout, post-rollout asserts that the
qpos stayed inside the declared bounds.

The full executor (`opendaisugi.executor_mujoco.MuJoCoExecutor`)
handles `JointMoveStep`, `CartesianMoveStep` via IK, `GripperStep`,
and `SimulationResetStep`. Torque violations and contact violations
are flagged as return codes from `run()`.

**When to use:** physical agents where stakes include real-world
actuation; development of robotics envelopes before deployment.

## Cross-integration story

All four surfaces call the same verifier. The Hermes adapter in
Python, the OpenClaw client in Node, and any MCP client all hit the
same `verify()` and `verify_completed_step()` functions. No parallel
implementation, no drift. The LoRA trainer consumes a journal
produced by that same verifier. The MuJoCo executor sits behind the
same `Envelope` model as the rest.

One verifier, four consumers. Keep it that way.

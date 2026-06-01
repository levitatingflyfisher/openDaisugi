# OpenClaw × openDaisugi

OpenClaw runs Node.js. openDaisugi runs Python. They talk through
MCP-over-stdio — OpenClaw spawns an openDaisugi MCP server as a
subprocess and calls its tools as if they were local functions.

## Why MCP rather than a Node port?

The verifier, predicate algebra, Z3 bindings, alias registry, and
Stage 2 machinery already live on the Python side. Reimplementing any
of that in Node would double the maintenance surface and break the
"one verifier" invariant. MCP gives us a narrow process boundary with
well-defined JSON-RPC message shapes, and it's the same protocol
Claude Code and other agent frameworks already speak.

## Setup

Install the MCP extra in the Python environment that runs the server:

```bash
pip install 'opendaisugi[mcp]'
```

Install Node deps for the demo:

```bash
cd examples/integrations/openclaw
npm install
```

## Running the demo

```bash
npm run demo
```

Expected output:

```
[Scenario 1] impersonating body → 1 violation(s)
  - postcondition 'body_no_impersonation' violated on completed step s1
[Scenario 2] clean body → 0 violation(s)
```

## Client surface (`client.mjs`)

```js
import { OpenDaisugiClient } from "./client.mjs";

const od = new OpenDaisugiClient();
await od.connect();

const env = await od.envelopeFor("send email to editor");
const planResult = await od.verifyPlan(plan, env);
const stepResult = await od.verifyCompletedStep(completedStep, env);

await od.close();
```

Three methods, one per MCP tool. Translate your OpenClaw agent's plan
and step shapes to the JSON dicts the server expects (same shape
Pydantic produces via `model_dump(mode="json")`) and you're done.

## Wiring into an OpenClaw agent

Before each agent action executes, call `verifyPlan`. After each
action completes, before its effect commits, call
`verifyCompletedStep`. If violations come back non-empty, roll back
the step and either retry with a different plan or hand off to the
fallback strategy declared in the envelope.

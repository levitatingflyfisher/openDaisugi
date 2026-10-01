# Using openDaisugi via MCP (v0.20+)

When the openDaisugi MCP server is available (`daisugi mcp serve` or
launched as part of an MCP-aware agent runtime), the full Checklist
workflow is callable as MCP tools — no Python imports required.

## The nine tools

| tool | when to call |
|---|---|
| `envelope_for(task, stakes, context)` | Before drafting a plan; lets the server generate an envelope tailored to the task |
| `find_pathway(task)` | Before drafting from scratch; reuses verified pathways for similar past work |
| `verify_plan(plan, envelope)` | Right after authoring; confirms structural + Z3 + DAG checks pass |
| `verify_completed_step(step, envelope)` | Stage-2 check after step execution but before its effect commits externally |
| `run_plan(plan, envelope)` | Execute under supervision; returns receipts and integrity result |
| `receipts_for_run(run_id)` | Audit a past run — every step that actually executed left a receipt |
| `recent_runs(limit)` | Discovery — what's been run recently, what task, what status |
| `list_pathways()` | What reusable pathways are in the store |
| `pathway_stats()` | Aggregate pathway-store stats |

## Typical flow

```
1. find_pathway(task)
   → if matched, fetch pathway envelope + plan_template; skip to step 4
   → else continue
2. envelope_for(task, stakes)
   → server returns a verified envelope dict
3. (agent authors plan in Pydantic; emits as dict)
4. verify_plan(plan, envelope)
   → if violations, agent reads suggested_remediation and revises
5. run_plan(plan, envelope)
   → returns run_id + receipts + integrity_passed
6. (optionally) receipts_for_run(run_id) for audit detail
```

## What the server doesn't ship by default

`run_plan` uses default executors (DryRunExecutor for unrecognized step
types) when called against a vanilla `daisugi mcp serve` — safe but not
real execution. Real-execution deployments construct a `Daisugi` with
custom executors (`DelegatingExecutor` for LLM-backed steps, real shell
executor, etc.) and hand it to `serve()`. That's a deployment-time
choice, not an agent-time one.

## What MCP buys vs direct Python use

- Other agents (OpenClaw, Hermes, anything MCP-aware) consume openDaisugi
  without re-implementing it
- Tool calls are typed end-to-end: schemas are published, agents discover
  the surface
- The agent's runtime can mediate auth/transport — openDaisugi stays
  pure Python with no transport concerns

# Design: sprig, a pack for pi and a small loop of its own

Status: **Proposed**, 2026-09-28. The owner agreed the direction (a pack for pi as well as our own
loop; branching search is worth it, with care for token cost). Details stay open.

Evidence: `docs/research/harnesses-2026-09-28.md` (the harness survey),
`docs/research/grafts-inputs-2026-09-26.md` (what each harness's hooks can do),
`docs/plans/2026-09-24-omarchy/design-delegation-tree.md`, `design-ranking.md`, `design-router.md`.

## What the evidence says

- The sophistication of the loop barely moves pass rate. A loop of about 100 lines with one tool
  scores close to systems a thousand times larger; with the model held fixed, changing the harness
  moved pass rate by 0 to 8 points.
- The harness moves cost a lot: up to 40 times the tokens per solved task, same model.
- The largest independent gains come from outside the plain loop: search over attempts, and
  parallel attempts ranked by a verifier.
- Context management helps a lot for small context windows and barely at all for large ones.
- Tool design (the agent-computer interface) moves results by several points.
- Codex sandboxes only its own shell tool; MCP tools are not guarded.

So sprig does not compete on intelligence. It competes on cost and trust.

## Two forms, one idea

1. **The pack for pi.** pi plus daisugi: the gate extension, pathways at Tier-0, the router, the
   journal, verified "done" scaled to the model, and good defaults. Lowest adoption friction; rides
   pi's growth; no loop of our own needed.
2. **The sprig loop.** A tiny Go loop, agent-first and headless, for what an extension cannot do:
   branching search inside the loop, and being the unit a parent agent starts in a delegation tree.
   It can speak pi's RPC so pi's interfaces can drive it.

Everything that can work for any harness lives in daisugi: the gate over every tool, the garden,
delegation edges, GEPA tuning, recall. sprig owns only what needs the loop.

## The opinions

1. **A tiny core.** Model call, action parsing, tool execution: commodity, kept small.
2. **The gate covers every tool,** whatever its source: shell, file, MCP, a sub-agent.
3. **Help scaled to the model.** The router matches intelligence to the task, and the loop's help
   follows it:
   - frontier model: trust its judgement of "done"; compaction off;
   - cheap or local model: a verified "done" (tests pass, the feature list turns green, the
     verifier agrees); staged compaction with the journal as recall;
   - a replayed pathway: its postconditions are the "done".
4. **Branching search, with a budget.** Branch only where it pays:
   - try the cheapest path first: a pathway, then one attempt;
   - branch only on a signal: a failed verify, failing tests, or the model's own low confidence on a
     hard step, and only when the stakes justify it;
   - branch with cheap models where the verifier can judge the result;
   - prune early: a branch that fails the envelope or a quick check stops at once;
   - a hard token budget per task, journaled, so the cost of every branch is visible;
   - rank survivors with `daisugi rank` (verifier, tests, cost, then preference).
5. **Careful tools.** Tool descriptions and output formats are measured and tuned (GEPA, with the
   verifier as the score), not written once and left.
6. **The garden proves itself.** Pathways must show saved tokens and unchanged task success on
   held-out work, or they are pruned.

## Risks

- **Token cost of branching.** The main risk; the budget, the signals and early pruning are the
  answer, and every branch is journaled so the cost is visible.
- **Verified "done" is not unique to us.** The survey could not show that other harnesses lack it,
  so sprig does not market it as unique.
- **The garden's value is unproven at run time.** The published gains come from training model
  weights, not from skill libraries.

## Open

- How much of the pack can pi's extension API carry (the grafts research says pi can rewrite input
  and replace output; branching inside pi's loop is not possible from an extension).
- The branching signal: which uncertainty signals are reliable enough to spend tokens on.
- Whether the sprig loop speaks pi's RPC, or its own.
- Gate on by default in sprig (owner: yes, once it runs smoothly).

# ADR-0022: weave, a runner for verified plan trees

- **Status:** Accepted in part, 2026-09-28: the owner chose option B (weave is a `daisugi weave` command on the existing supervisor, in Python, Go and Rust). Data flow between steps is typed slots, verified again (WV-1, owner, 2026-09-28). Resume, plan sources and what runs agentic steps stay open. The original text follows.
- **Built 2026-09-30:** `daisugi weave PLAN.json -e ENVELOPE` in Python, Go and Rust, on the supervisor, with a per-step hook that changes nothing when absent. Typed slots as WV-1 rules; the router's model for each task step; resume from receipts and start marks, and agentic steps through `claude -p` in Python, both built as the Proposed rows WV-2 and WV-4 say, and still open with the owner. Plans are `ActionPlan` JSON (WV-3). Go and Rust refuse agentic steps and `--max-parallel` above 1. A filled step is verified again, per step and as part of the whole plan. Rulings WV-R-1 to WV-R-11 in `clients/ADJUDICATIONS.md`; 88 cases, 86 agree in each port and 2 are refused as ruled. Not built: skill expansion with per-step verify, `gate` and `on-fail` words, dialect output, a plan view in coppice. `harness/sprig/weave` is unchanged.
- **Date:** 2026-09-26

## Context

### The intent

The owner, 2026-08-26, on what weave was for (recorded in
`docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md`, "What weave was for"): "the directed
graph of distilled actions, each steps some of which are more deterministic and scripty and some
of which are LLM based small model with constrained token grammar output and some of which are
large very wide open bleeding edge", and "LLMs are esp good at ON THE FLY DSLs".

`docs/harness/harness-designs.md`, Design C, gave weave a small grammar (`step`, `parallel`,
`gate`, `on-fail`), control flow in code at zero model tokens, a model only at the leaves,
resumable and cron-able runs, and a live view of the graph. It called weave "the harness home for
the ADR-0008 idea": parameterized, callable, composable pathways. Design B (grove) was the fleet
view over live sprigs and weaves. grove's daemon half became coppice; its worktree per shoot and
"the gate as a game event" are now coppice tasks and asks (`harness/coppice/PROTOCOL.md`).

### What was built

`harness/sprig/weave/weave.go` (131 lines, header "MVP") and `harness/sprig/cmd/weave/main.go`:

- A step is `{id, task, needs}`. The task is free text for one sprig agent.
- `Parse` checks unique ids, real needs, and no cycle.
- `Run` runs the steps one at a time in topological order and stops at the first failure.
- None of `parallel`, `gate` or `on-fail` exists. There is no resume and no view.
- The command defaults to `sprig.AllowAll{}`. The gate runs only with `--gate`. sprig does the
  same (`harness/sprig/cli.go:89`). **Both fail open by default**, against the fail-closed ruling.

### What was lost

weave never learned daisugi's plan format. It runs free-text tasks, not typed steps, so it
cannot verify a step before it runs. Meanwhile the Python side grew most of what weave's grammar
named:

| weave's word | What exists in Python | File |
|---|---|---|
| `step` | typed steps: `task`, `agentic`, `skill`, `mcp`, `shell`, `file_read`, `file_write`, `network`, robot steps, `vla` | `src/opendaisugi/models.py` |
| the graph | `ActionPlan.steps` with `depends_on`; `check_dag`, `dependency_levels`, `topological_order` | `models.py`, `dag.py` |
| `gate` | plan verify, then `verify_step` and an `ApprovalStrategy` before each step | `supervisor.py` |
| `on-fail` | a `FallbackHandler` for a rejected step (default: halt); a failed step aborts the run | `supervisor.py`, `fallback.py` |
| `parallel` | `max_parallel`, only for independent `parallel_safe` steps in one dependency level | `supervisor.py` |
| model per step | `StepBase.preferred_model`, `model_sizer`, `BudgetAwareDelegatingExecutor` | `models.py`, `orchestrator.py` |
| check after a step | `StepBase.postcondition`, its result in `Receipt.verify_result` | `models.py`, `supervisor.py` |
| plan authoring | `decompose` writes a DAG and verifies it | `decomposer.py` |
| typed reuse | bind a typed pathway's holes, then verify again | `pathway_bind.py` |
| composition | a frozen pathway as a `SkillStep` handler | `compose.py` |

So the Go weave is a thinner copy of the Supervisor, not a missing ancestor of it.

## The proposed shape

weave runs an `ActionPlan` as a verified graph. Each step type has one way to run:

| Step | How weave runs it |
|---|---|
| `shell`, `file_*`, `network`, `mcp` | directly, with the existing executors. Zero model tokens. |
| `task` | through the router (`design-router.md`), which picks a model per step. A small model writes in a dialect (below). |
| `agentic` | as a sprig, or as a coppice pane on a task with a worktree. Built today: `verify.py` checks the requested tools against the caller's envelope, and `AgenticExecutor` wires in the call-time gate. Designed, not built: a proof that a child sprig's envelope is inside the parent's ("a tree of agents" in the strategy notes, **Open** until sprig resumes). |
| `skill` | expands to its pathway. A typed pathway is bound, then verified. Every sub-step is verified like a top-level step. |

For every step: verify before it runs, run the postcondition after, write a receipt. For a
`task` step, "verified" means the delegation check before it and the postcondition after it. No
proof covers what a model's prose says; the strategy notes name output checks as the gap still to
close. The graph
keeps `parallel` (independent steps in one level), `gate` (an ask, shown in coppice) and
`on-fail` (halt, retry, or a fallback step). A run is resumable from its receipts. coppice shows
the run as a plan on the floor, read from the journal.

**Dialects.** A dialect (strategy notes, "Dialects") is a vocabulary of named words that unfold
to the kernel predicate algebra. In weave a small-model step would write its output in a dialect
grammar, not free text. This helps only where the output is action-shaped: a sub-plan, a bind,
or an envelope or policy edit. That output unfolds to kernel terms or typed steps, and `verify`
and Z3 check it. Prose from a `task` step goes to the synthesizer, and a dialect does not make it
checkable. This is the "constrained token grammar" in the owner's quote. No dialect code
exists. The only constrained model output today is `pathway_bind.Bindings`, a Pydantic schema.

### What exists and what is missing

| Part | State | Where |
|---|---|---|
| Typed plan, DAG checks, per-step verify, receipts | exists (Python) | `models.py`, `dag.py`, `supervisor.py` |
| Plan model, verify, DAG in Go and Rust | exists | `clients/go/internal/pmodel`, `clients/go/internal/verify/dag.go`, `clients/rust/src/models.rs`, `verify.rs`, `dag.rs` |
| Supervisor, orchestrator, decomposer in Go and Rust | missing (stage K) | `plan-part2-ports.md` |
| `agentic` steps in a run | missing. `AgenticExecutor` exists but the decomposer cannot emit `agentic` and the orchestrator does not wire it | `agentic_executor.py`, `decomposer.py`, `orchestrator.py` |
| Skill expansion with per-step verify | missing. `pathway_skill_handler` calls raw executors and skips typed pathways; ADR-0008 says it is not auto-wired | `compose.py` |
| Router picking a model per step | designed only | `design-router.md` |
| Resume | missing. Nothing in `supervisor.py` or `run_session.py` | |
| Plan view in coppice | missing. No verb in `PROTOCOL.md` shows a plan | |
| Dialects | missing. First compression test only | strategy notes |
| sprig or coppice pane as a step | parts exist: coppice `task.create` with `worktree`, `pane.create` with `task`, a sprig adapter | `internal/server/tasks.go`, `internal/adapters/sprig` |

## Options

Every option must pass three tests. (1) **Parity:** Python is the oracle, then Go and Rust pass
the same golden cases. (2) **daisugi stays importable alone, and the floor supervises and does
not decide** (ADR-0020, Consequences). (3) **Fail closed.**

**A. weave in Go, in `harness/sprig/weave`.** Grow the existing runner.
- For: code exists; sprig is Go; agentic steps are near.
- Against: a Go-first part has precedent (stage J ports sprig to Rust from Go, with no Python
  sprig). But the task for weave is stricter: it must exist in Python, Go and Rust, so the Python
  work happens anyway. And weave's per-step rules belong to the Supervisor, which has a Python
  oracle and a stage K port. A would copy the Supervisor's verify, receipt and fallback logic a second time in Go,
  outside `clients/go`, where stage K will port the Supervisor too. Two Go supervisors drift.

**B. weave as a daisugi command.** `daisugi weave run plan.json` in Python, on the Supervisor.
Go and Rust get it through stage K, in `clients/go` and `clients/rust`.
- For: meets parity by the normal path. Reuses verify, receipts, fallback and parallel as they
  are. The runner stays in daisugi, where the proofs are. sprig and coppice become executors for
  `agentic` steps, called through a narrow interface.
- Against: stage K is large, so Go and Rust users wait. daisugi must call out to sprig or
  coppice for agentic steps; the call must go through an executor interface so daisugi imports
  nothing above it.

**C. weave inside coppice.** coppice runs the plan and makes each step a pane.
- For: the view comes free; asks already reach the human.
- Against: coppice would decide which step runs next and would run shell and file steps itself.
  That is the floor making tool calls, the exact break ADR-0020 forbids. It also needs a
  verifier inside coppice, and coppice has no Python oracle for plan runs.

## Recommendation

**B**, stated as Proposed. Build the runner in Python on the Supervisor. Add the missing parts
in this order: agentic steps, verified skill expansion, resume, router hook, dialect output. Port
it with stage K. coppice shows the run from the journal and answers its asks, and does not run
it. When the Go port exists, `harness/sprig/weave` becomes a thin front end on it, or goes. Until
then, make `--gate` the default in weave and sprig, so they fail closed.

## Consequences

- **Buys:** one runner, one oracle, the proofs already built. The owner's graph of scripts,
  small models and frontier steps becomes a plan the verifier already understands.
- **Costs:** stage K becomes a dependency of weave in Go and Rust. The Supervisor grows resume
  and an agentic executor, both with real safety work.
- **Forecloses:** a second runner in coppice or sprig that decides steps on its own.

## Open questions for the owner

1. **Data flow between steps.** Today no step output reaches a later step; task outputs go only
   to the synthesizer (`orchestrator._task_step_prompt`). This is the injection boundary: a step
   output never becomes a command. The owner's "graph of distilled actions" suggests data flows.
   Options: (a) outputs to the synthesizer only, as now; (b) typed data slots only, filled like a
   bind and verified again, never a capability; (c) free-text flow into later prompts, never
   into commands. **Recommendation: (b).** (c) lets a poisoned output steer a later model.
2. **Resume.** Raw material: receipts, reversal handles, `_READ_ONLY_KINDS` in `supervisor.py`.
   Options: skip steps with a succeeded receipt for the same plan hash; re-run read-only steps;
   stop and ask a human for an irreversible step with no receipt. **Recommendation:** all three,
   in that order.
3. **Where the plan comes from.** The decomposer, a stored pathway, or a file a model writes.
   **Recommendation:** all three, one format (`ActionPlan` JSON), since the bots write the plan.
4. **Agentic executor.** sprig, a coppice pane, or `claude -p` as `AgenticExecutor` does now.
   **Recommendation:** keep `claude -p` first; add sprig when sprig resumes.
5. **Option A, B or C.** **Recommendation:** B.

## Conflicts found between the notes and the code

- The strategy notes say "a `skill` step holds a whole plan". `SkillStep` holds `skill_id`,
  `skill_input` and `contract_envelope`; the plan is resolved at run time.
- The notes say "tree". `ActionPlan` is a flat DAG over `depends_on`; it nests only through
  skills.
- The notes list a "file" step. The code has `file_read` and `file_write`, plus robot steps and
  `vla`.
- `harness-designs.md` chose Rust for weave. It was built in Go.

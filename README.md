# opendaisugi

[![CI](https://github.com/levitatingflyfisher/openDaisugi/actions/workflows/ci.yml/badge.svg)](https://github.com/levitatingflyfisher/openDaisugi/actions/workflows/ci.yml)

> A restricted predicate algebra authorable by agents, compiling to SMT-LIB2
> that Z3 solves. We verify plans authored by LLMs at runtime.

Runtime assurance for agent actions: generate a safety envelope for a task,
verify proposed action plans against it via Z3, and log replayable traces.
Skills can be treated as contracts — one agent can delegate to another
(including LoRA-tuned smaller models) with a mechanical proof that the
delegation is safe.

## Vision

**Separate what is _allowed_ from what is _decided._** What's decided comes from a
black box (an LLM, a neural policy, a VLA) — capable and fundamentally unverifiable.
What's allowed comes from a space of checkable calculations. The key move: an LLM
closes the verification loop not by becoming verifiable, but by *generating*
verifiable constraints — the envelope — which a deterministic layer then enforces.
It's [Runtime Assurance](https://en.wikipedia.org/wiki/Runtime_assurance) (Simplex,
verified envelopes) pointed somewhere it never has been: LLM agents and robot
foundation models.

→ **[VISION.md](VISION.md)** for the full north star — the invariants that must
stay true, and an honest scorecard of what's built vs. aspirational.
→ **[docs/](docs/README.md)** for the [Diátaxis](https://diataxis.fr/)-organized
docs (tutorials · how-to · reference · explanation).

### Gate an agent you're already running

One command puts a fail-closed gate in front of a live Claude Code session:
every tool call is proven inside an envelope *before it runs*, shadow-mode
first, one flag to enforce.

```bash
daisugi gate quickstart      # → a working shadow-mode gate in minutes
```

It is not a demo. A real delegated sub-agent asked to read a file outside its
envelope is denied by the gate, the value withheld from the model, the reason
proof-backed — captured verbatim in
[`examples/injection-denied/`](examples/injection-denied/):

```
DENIED Read '/.../infra/deploy_region.txt'
  reason: permissions: file_read path '/.../infra/deploy_region.txt'
          not permitted by file_read ['/.../workspace/**']
```

The same 13-attack corpus that gates this project's own merges is one command:
`daisugi gate audit` (denial 1.00; false-positive rate published, not hidden).
Start here: **[Protect an agent you're already running](docs/tutorials/protect-your-existing-session.md)**.

### See it: runtime assurance for a robot swarm

![Property-security patrol — openDaisugi gating a (mock) VLA swarm in MuJoCo](docs/assets/property-patrol.gif)

Three drones patrol a property, one sector each. A black-box policy (a stand-in for a
VLA like π0 / SmolVLA) proposes each drone's next move; openDaisugi verifies it live
against the drone's sector envelope + swarm deconfliction — **green** accepted,
**amber** an out-of-sector proposal refused and pulled back (Simplex fallback),
**red** a proposal held because it closed on a peer. No drone can leave its sector and
no two can collide, *proven every tick before motion*, whatever the policy proposes.
Runnable: [`examples/property-patrol/`](examples/property-patrol/) (the gate, CPU,
zero deps) + `mujoco_render.py` (this recording).

And delegation — *a message that carries authority is a delegation*, verified before anyone acts:

![Comms-loss reassignment — a survivor's authority expands to cover a downed peer, verified](docs/assets/comms-delegation.gif)

`drone_mid` loses comms; the coordinator expands `drone_west`'s authority to cover the
gap — **accepted** only after `verify_swarm_tasking` re-proves it's still contained
*and* deconflicted; the "hand it to both neighbors" alternative flashes the overlap
**red — rejected before any drone moves**. Four such scenarios (hierarchy · hand-off ·
comms-loss · cross-swarm) in [`examples/swarm-comms-delegation/`](examples/swarm-comms-delegation/).

And sixteen kinds of refusal at once — each tile a *real* `verify()` rejection of a
different unsafe action (keep-in · no-fly · deconflict · delegation · formation ·
moving keep-out · geofence · reassignment · cross-swarm · nested delegation · slalom ·
hand-off · leash · restricted airspace · corridor merge · a "must return to base"
*invariant*):

![Sixteen runtime-assurance scenarios, each a real openDaisugi rejection](docs/assets/gallery-grid.gif)

Every gate is proven *before* a single frame is rendered — the renderer asserts each
scenario accepts the safe case and refuses the unsafe one first. See
[`examples/gallery/`](examples/gallery/).

**Status (v0.27.0):** verification-core is sound — strict mode (default-on
at `stakes` high/physical) rejects opaque invariants and postconditions that
can't be discharged, Z3 vacuity detection catches tautological or
contradictory predicates before they reach the solver, and `AliasRegistry`
lets agents author named constraints that propagate through the full
verify → supervise → journal pipeline. The reproduction substrate is
complete: per-step receipts, integrity check, reusable pathways, tiered
model routing. The MCP server exposes the full runtime. Many features are
production-candidate; some surfaces (robotics, LoRA training pipeline,
pathway portability) remain experimental. See
[docs/feature-status.md](docs/feature-status.md) and
[docs/limitations.md](docs/limitations.md) before adopting.

---

## Install

```bash
uv add opendaisugi
```

Python 3.12+. `z3-solver` is a required native dependency (installed
automatically). (`pip install opendaisugi` works too if you're not using uv.)

Optional extras:

```bash
uv add 'opendaisugi[search]'    # REQUIRED for pathway token savings (sentence-transformers)
uv add 'opendaisugi[mcp]'       # MCP server for Claude Code / OpenClaw
uv add 'opendaisugi[robotics]'  # MuJoCo executor (experimental)
uv add 'opendaisugi[lora]'      # LoRA training-data pipeline
```

> **If you want token savings via the pathway store** — which is the core
> value proposition for most users — install `[search]` from the start.
> Without it, `find_pathway()` silently returns `None` and every run pays
> full LLM cost. The `[search]` extra adds `sentence-transformers` (~80 MB)
> which pulls a small CPU-only PyTorch slice. It is optional to keep bare
> `uv add opendaisugi` lightweight for CI and server deployments that
> only use the verifier.

### Wire it into your agent — `daisugi install` (v0.28.0+)

One command detects every agent harness on your machine and wires openDaisugi
in from a single source of truth:

```bash
daisugi install             # detect + configure every harness
daisugi install --dry-run   # preview every change, write nothing
daisugi install --uninstall # reverse every managed change
daisugi install --runtime claude   # target one harness only
```

It installs three layers per harness — all idempotent, backed up, reversible:

| Layer | What | Claude Code | Codex | Hermes | OpenClaw |
|-------|------|-------------|-------|--------|----------|
| **Skill** | `opendaisugi-checklist` (on-demand, 0 token tax) | `~/.agents/skills` → `~/.claude/skills` | `~/.agents/skills` → `~/.codex/skills` | `~/.hermes/skills` | `~/.openclaw/workspace/skills` |
| **Tools** | `daisugi mcp serve` (MCP) | `~/.claude.json` | `config.toml` | `config.yaml` | `openclaw.json` |
| **Capture** | pre-tool-call → distillation | PreToolUse hook | (verify per version) | `pre_tool_call` hook | `before_tool_call` plugin |

The skill is discovered on demand via the cross-vendor `.agents/skills`
standard — no SessionStart injection, so simple sessions pay zero extra tokens.
OpenClaw needs a gateway restart to load the capture plugin; Hermes reads
`~/.hermes/config.yaml` (not `cli-config.yaml`).

### Subscription-credits path (no API key) — v0.12.0+

If you have Claude Code installed, every LLM call in opendaisugi can
route through your existing subscription instead of an API key:

```bash
export OPENDAISUGI_LLM_BACKEND=claude-code    # or pass --llm claude-code per command
daisugi generate-envelope "Delete .tmp files older than 7 days in /var/log"
```

Covers all eight LLM call sites (envelope generation, distillation,
recompute fallback, LLMCheck verification, transcript parsing, Tier-1).
Implemented in `src/opendaisugi/claude_code_llm.py`.

### Day one — turn your existing convos into savings + trust (v0.29.0+)

Already have months of agent conversations? One command discovers them, replays
them into the verified journal, and distills reusable pathways — so from today
matching tasks skip envelope generation (token savings) and every replayed action
is verified (trust):

```bash
uv add 'opendaisugi[search]'        # required for pathway savings

# 0. (optional) wire a hardware-appropriate LOCAL model so distillation is cheap
daisugi setup                       # detect hardware → recommend a llamafile model + next steps
#   ...start the recommended llamafile, then:
daisugi setup --endpoint http://localhost:8080/v1 --model <name> --wire  # qualify + wire it

# 1. turn existing convos into verified pathways
daisugi onboard --dry-run           # preview: what it would discover + distill
daisugi onboard                     # discover ~/.claude/projects, ~/.codex, … → distill
daisugi onboard --llm claude-code   # no API key — use your Claude Code subscription

daisugi status                      # hardware + local model wired? token savings LIVE? journal verified?
daisugi route "refactor the auth module"   # cheapest viable model/tier for a task
```

`daisugi setup` recommends a model *sized to your box* and — crucially — only
wires it as Tier-1 if it passes a **qualification gate** (it must emit valid
envelopes at an acceptable rate on your hardware). The model family is your pick;
none is asserted-best. Once wired, `onboard`/`tend` run bulk envelope generation
on the local model.

`onboard` honors `--limit`, `--harness`, `--threshold`, `--lookback-days`
(default: all history), and `--json`. Point discovery anywhere with
`OPENDAISUGI_TRANSCRIPT_ROOTS=claude-code=/path/to/exported`.

**Routing vs Anthropic's advisor tool.** The advisor tool (beta
`advisor-tool-2026-03-01`) makes a fixed cheap executor smarter via a
mid-generation Opus consult — re-derived every request, unverified. `daisugi
route` is complementary: a *repeat* task that matches a distilled pathway routes
to Tier-0 reuse — ~free **and** re-verified against its envelope — because the
pathway store is the cross-request memory the advisor tool doesn't have. For a
hard *novel* task, `route` points you at the advisor-tool pairing.

Not sure where to start? `daisugi quickstart` prints your hardware, a recommended
local model, the transcripts it found, and the exact command sequence.

### Safe subagents from local models (v0.31.0+)

Run cheap local-model subagents that can't act outside a verified scope:

```python
from opendaisugi import SafeSubagent, Contract, Envelope, Permission
from opendaisugi.subagent import DelegationDenied

# A subagent can only be minted if its contract is subsumed by the parent's
# authority (subsumption, incl. fail-closed robot-capability checks):
sub = SafeSubagent.create(parent_envelope=parent, contract=inspector_contract, tier1=local_model)
await sub.run(plan)          # every plan re-verified against the scope; dry-run by default
```

`create` raises `DelegationDenied` if the subagent asks for more than the parent
grants. `tier1` is the local model (free-ish tokens) the subagent reasons with;
SafeSubagent is the runtime safety gate. See `examples/safe-local-subagent/`.
This is plan-level runtime assurance, not an OS sandbox.

### Run a whole prompt end to end — the Orchestrator (v0.32.0+)

`tend()` looks backward (traces → distilled skills). The **Orchestrator** looks
forward: one prompt → a verified typed-step DAG → each step routed to the cheapest
capable model under a token budget → a synthesized final answer. Repeat prompts
reuse a distilled pathway.

```python
from opendaisugi import Daisugi

dai = Daisugi()
result = await dai.orchestrate(
    "summarize the open PRs and draft a standup note",
    budget_tokens=20_000,          # gates routing DURING the run, not after
)
print(result.final_answer)
for s in result.sizings:           # per-step: difficulty → model
    print(s.step_id, s.difficulty, s.tier, s.model)
print(result.budget.spent, "tokens")
```

Or from the CLI: `daisugi orchestrate "…" --budget 20000`. The decomposed plan is
verified against an envelope before it runs and each step is re-verified at
execution time — the orchestrator adds routing and assembly *on top of* the
assurance guarantees. Pieces are composable too: `decompose()`, `size_plan()`,
`BudgetTracker`, `synthesize()`. New step types `TaskStep` / `SkillStep` /
`MCPStep` each carry a real verify surface (skills prove subsumption; MCP tools
gate against a deny-by-default `mcp_allowlist`). `AgenticStep` (v0.36) is the
tool-using delegation type: unlike TaskStep's pure-reasoning leaf, it runs a
sub-agent with real tools — bounded by the parent envelope via a computed
`--allowedTools` wall *and* the call-time gate wired into the sub-agent's own
hook config (`AgenticExecutor`).

---

## The 30-second demo

Prove that an orchestrator can safely delegate to one skill but not another:

```bash
python examples/delegation_demo.py
```

Output:

```
Scenario 1: orchestrator delegates to narrow echo skill
  allowed:       True
  subsumption:   holds=True  89.3 ms
  reason:        subsumption holds; delegation safe

Scenario 2: orchestrator delegates to wider destroyer skill
  allowed:       False
  subsumption:   holds=False  44.3 ms
  counterexample:
    command:             'rm'
    outer rule violated: shell_allowlist
    inner justification: shell_allowlist
  reason:        subsumption failed: inner allows 'rm' but outer rejects via shell_allowlist
```

The counterexample is a literal Z3 model — the concrete `ShellStep` the
inner envelope admits that the outer forbids. Not demo theater; the solver
produced it.

---

## Tutorial — your first verified plan

Generate a safety envelope for a task, verify a plan against it, log the
trace.

```python
import asyncio
from opendaisugi import ActionPlan, Daisugi, ShellStep


async def main():
    dai = Daisugi()

    # 1. Generate a safety envelope (calls the configured LLM provider;
    #    requires ANTHROPIC_API_KEY or equivalent).
    envelope = await dai.generate_envelope(
        task="Delete .tmp files older than 7 days in /var/log"
    )

    # 2. Your LLM of choice proposes a plan (mocked here for brevity).
    plan = ActionPlan(
        source="vanilla-llm",
        task="Delete .tmp files older than 7 days in /var/log",
        steps=[
            ShellStep(
                id="s1",
                command="find /var/log -name '*.tmp' -mtime +7 -delete",
            ),
        ],
    )

    # 3. Verify the plan against the envelope. Pure, sync, no I/O.
    result = dai.verify(plan, envelope)
    if not result.ok:
        for v in result.violations:
            print(f"[{v.stage}] {v.message}")
        return

    # 4. You run the plan (opendaisugi stays out of execution).
    # subprocess.run(...) or your framework's executor.

    # 5. Log the trace for replay / regression catching.
    dai.journal.log(
        task=envelope.task, envelope=envelope, plan=plan, result=result,
    )


asyncio.run(main())
```

For a hand-written envelope (no LLM call needed), see
[examples/agent-council/](examples/agent-council/).

---

## Saving tokens with pathways

After a few successful runs of the same class of task, opendaisugi
distills them into a **compiled pathway**: a reusable plan template +
pre-verified envelope stored in a local SQLite file. Future runs that
match the task semantically skip the expensive `generate_envelope()` LLM
call entirely and instead adapt the cached template with a cheap Tier-1
call.

**Prerequisites:** install the `[search]` extra (see Install above).

### The loop

```python
import asyncio
from opendaisugi import Daisugi, ActionPlan, ShellStep

async def main():
    # tend_after=5 means: after every 5 successful runs, distill automatically.
    # Omit tend_after and call `await dai.tend()` on your own schedule instead.
    dai = Daisugi(tend_after=5)

    envelope = await dai.generate_envelope("Delete stale .tmp files in /var/log")

    # Use dai.run() instead of Supervisor directly — it tracks successes and
    # auto-tends when the threshold is reached.
    plan = ActionPlan(
        source="llm", task="Delete stale .tmp files in /var/log",
        steps=[ShellStep(id="s1", command="find /var/log -name '*.tmp' -mtime +7 -delete")],
    )
    session = await dai.run(plan, envelope)

asyncio.run(main())
```

### On subsequent runs

```python
async def main():
    dai = Daisugi(tend_after=5)

    # Check if we already have a distilled pathway for this task.
    match = await dai.find_pathway("Delete stale .tmp files in /var/log")
    if match:
        # Adapt the cached template — one cheap LLM call, no envelope generation.
        plan = await dai.adapt_plan(match, task="Delete stale .tmp files in /var/log")
        envelope = match.pathway.envelope
    else:
        # Cold path: generate envelope + plan as normal.
        envelope = await dai.generate_envelope("Delete stale .tmp files in /var/log")
        plan = ...  # your LLM proposes a plan

    session = await dai.run(plan, envelope)
```

### What to know

| | |
|---|---|
| **Cold start** | Pathways require ≥ 3 successful traces of a similar task before `tend()` produces one. First few runs of any new task type pay full cost. |
| **`tend_after` vs manual** | `tend_after=N` auto-tends every N successes via `dai.run()`. For batch pipelines or custom schedules, omit it and call `await dai.tend()` yourself (or `daisugi tend` from the CLI). |
| **`tend()` costs one LLM call per cluster** | It is not free. Use `tend_after` conservatively or run it offline. |
| **Pathway validity** | Adapted plans are re-verified against the stored envelope before being returned. A pathway that drifts out of policy fails verification and falls back to the cold path automatically. |

---

## Architecture

How it all fits together — the verify→supervise→journal→distill spine, the two
loops, the consumption surfaces, and the module map, with diagrams.

→ **[docs/architecture/OVERVIEW.md](docs/architecture/OVERVIEW.md)**

The *why* behind the load-bearing decisions (fail-closed, Z3-over-heuristics,
envelope-as-contract, layer-not-harness, the Python runtime, the `claude -p`
backend) lives in **[docs/adr/](docs/adr/)**.

## Concepts

How opendaisugi actually works — envelopes, the predicate algebra, Z3
compilation, soft nodes, verification stages, subsumption.

→ **[docs/concepts.md](docs/concepts.md)**

---

## How-to guides

Task-oriented recipes.

- **Run a verified plan with per-step receipts and integrity check** —
  `examples/agent-council/run_dogfood.py` is a runnable kit showing the
  full v0.18+ loop (envelope authoring → verify → execute → receipts
  → integrity).
- **Capture tool calls from Claude Code / Hermes / OpenClaw** —
  see [docs/hook-integration.md](docs/hook-integration.md) for the
  one-line wiring recipe per host runtime.
- **Author a problem-specific DSL** — see the
  [opendaisugi-checklist skill](skills/opendaisugi-checklist/SKILL.md)
  and its references for the workflow.
- **Delegate from an orchestrator to a skill** —
  `examples/delegation_demo.py`, uses `verify_delegation`.
- **Integrate with Hermes / OpenClaw** —
  [docs/integrations.md](docs/integrations.md) and
  `examples/integrations/`
- **Verify a robot plan in MuJoCo** —
  [docs/robotics.md](docs/robotics.md) (experimental)
- **Export / import a compiled pathway** —
  [docs/pathway-skill-format.md](docs/pathway-skill-format.md)
- **Serve opendaisugi as an MCP server** —
  `daisugi mcp serve` (requires `[mcp]` extra)

---

## Reference

- **API:** the public surface is defined in
  [`src/opendaisugi/__init__.py`](src/opendaisugi/__init__.py). The
  core primitives are `Envelope`, `Permission`, `Invariant`, `Contract`,
  `verify`, `verify_step`, `verify_delegation`, `Supervisor`, `Journal`,
  `Daisugi`, `Receipt`, `DelegatingExecutor`, `StepBase`, `step_type`,
  `coerce_step`. v0.21+ also exposes `opendaisugi.hook` for passive
  capture and `opendaisugi.mcp_server` for the MCP integration.
- **CLI:** `daisugi --help` for the command tree. Top-level commands:
  `run`, `generate-envelope`, `verify`, `tend`. Subcommand groups:
  `journal`, `pathways`, `tiers`, `gardener`, `lora`, `mcp`, `hook`.
- **Step metadata keys:** [docs/step-vocabulary.md](docs/step-vocabulary.md)
- **Pathway bundle format:** [docs/pathway-skill-format.md](docs/pathway-skill-format.md)
- **YAML envelope schema:** see `tests/fixtures/agent.envelope.yaml`

---

## What opendaisugi does not do

Before adopting, read [docs/limitations.md](docs/limitations.md). Short
version:

- Not an OS-level sandbox. `Supervisor` is a Python-level gate, not a
  container. For runtime cross-process exfiltration prevention, use
  SELinux / AppArmor / seccomp at the OS layer; we sit above that.
- Not a hallucination detector. It verifies plans, not free-form output —
  with the exception of the `llm_check` predicate primitive, which uses
  a cheap LLM to evaluate explicitly-named perceptual claims (and is
  refused under `stakes='physical'` envelopes).
- Not a tool-blocking hook. Claude Code, Hermes, and OpenClaw all ship
  tool-call hooks that can block; v0.21's passive hook deliberately
  doesn't compete with them. It captures runs to feed the reproduction
  substrate; enforcement runs through the Supervisor or MCP `run_plan`.
- Unsupported regex features (lookaround, backrefs, case-insensitive
  flags) fall back to soft nodes — surfaced explicitly, never silently
  approved.

---

## Feature status

Maturity per feature, at a glance:
[docs/feature-status.md](docs/feature-status.md).

- **Production-candidate** (~10 features) — core thesis; audit-ready.
- **Working** (~15 features) — functional, tested, not heavily
  battle-tested.
- **Experimental** (~3 features) — shipped but has sharp edges
  (robotics executor, pathway portability).
- **Planned** — arithmetic-over-paths operator in the algebra.
  (Signature verification shipped v0.15.0; `LengthRange` /
  string-length operator shipped v0.15.0 too; distributed pathway
  registry shipped v0.25.0.)

---

## Case studies

Concrete scenarios where runtime assurance earns its keep:

- **[AI Council — structural gates around perceptual judgment](docs/case-studies/ai-council.md)**:
  envelope-enforced PII redaction across a voting panel of LLMs.


---

## CLI (quick reference)

```bash
# Generate an envelope.
daisugi generate-envelope "Read /data/sales.csv and print the row count"

# Run a whole prompt end to end (decompose → size → execute → synthesize).
daisugi orchestrate "summarize the sales csv and draft a one-line takeaway" --budget 20000

# Recommend the cheapest viable model/tier for a task.
daisugi route "refactor the auth module"

# Verify a plan against an envelope.
daisugi verify plan.yaml --envelope envelope.yaml

# Inspect the journal.
daisugi journal stats
daisugi journal search "csv processing"       # requires [search] extra
daisugi journal replay 2026-04-09-a1b2c3d4    # re-verify; exit 1 on drift

# Parse a transcript into episodes and ingest them.
daisugi journal parse session.jsonl -o episodes.yaml
daisugi journal ingest episodes.yaml

# MCP server.
daisugi mcp serve
```

Full command tree via `daisugi --help` at each level.

---

## Roadmap

Recent releases (last 60 days):

- **v0.15** — ed25519 contract signing + length algebra
- **v0.16** — structured logging + deployment / security-model docs
- **v0.17** — envelope realism (shell allowlist globs, env-prefix head
  extraction, parser compound-shell decomposition)
- **v0.18** — reproduction substrate: per-step receipts, run-end
  integrity check, dynamic step-type registry, two contract-orchestration
  kit (`examples/agent-council/`)
- **v0.19** — cheap-model delegation: `DelegatingExecutor`,
  `_StepBase.preferred_model`, `Receipt.model_id`, physical-stakes guard
- **v0.20** — MCP runtime: `run_plan`, `receipts_for_run`, `recent_runs`
- **v0.21** — passive hook: capture tool calls from Claude Code /
  Hermes / OpenClaw via `daisugi hook record`, convert to journal
  traces via `daisugi hook to-trace`
- **v0.21.1** — architectural-readiness pass: security/robustness
  hardening, registry collision detection, DRY refactors
- **v0.22** — perf (lightweight `verify_step`, sqlite connection reuse),
  README rewrite, deps pinned to `<2`, `StepBase` rename, `run_plan`
  timeout, `CompiledPathway.activation_count`

Future:

- Auto-tend daemon (close the captures → traces → distillation loop)
- More predicate-algebra operators (string-length, arithmetic over step
  metadata, scalar-context `exists_step`)

Full version history: [CHANGELOG.md](CHANGELOG.md).

---

## License

MIT. See [LICENSE](LICENSE).

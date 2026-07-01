# AGENTS.md

Guidance for AI coding agents (and humans) working in this repo. This is the
top-level map; dense subsystems may carry their own nested `AGENTS.md` with more
detail — the closest one to the file you're editing wins.

**Read these three, in order, before non-trivial work:**
1. [VISION.md](VISION.md) — what must stay true and why (the invariants).
2. [docs/architecture/OVERVIEW.md](docs/architecture/OVERVIEW.md) — how it fits together, with diagrams.
3. [docs/architecture/CONVENTIONS.md](docs/architecture/CONVENTIONS.md) — the coding patterns to match.

## Take the code as current-state, not gospel

Every line of source and every comment here was written by an AI assistant. Treat
it as **an accurate record of what currently exists, offered with gratitude and a
grain of salt** — not as a specification and not as guaranteed-correct. A comment
claiming an invariant is a *hypothesis to verify*, not a proof. If a comment and the
tests disagree, the tests win; if the tests and reality disagree, reality wins.
When you rely on a claim, confirm it (read the code, run the test) first.

## What this is

A runtime-assurance **library** (not an agent, not a harness): generate a safety
envelope for a task, prove a proposed plan stays inside it (via Z3), execute it
step-by-step under a supervisor, journal the run, distill repeats into reusable
pathways. ~68 modules, ~20k LOC, ~1600 tests, CI green.

## Non-negotiables (breaking one is a regression, not a feature)

- **Fail closed.** Unprovable ⇒ reject; undeclared ⇒ deny. A fail-*open* in the
  verifier is the worst bug class here. New `match` on step/permission types needs
  a default case that rejects the unknown.
- **Verify before execute.** No effect before its plan is proven inside its
  envelope; the Supervisor re-checks each step at run time.
- **Caller's envelope is the ceiling.** Reused pathways / delegated skills /
  MCP-supplied plans are bounded by the caller's envelope, never their own.
- **TDD, always.** Reproduce → failing test → fix → `uv run pytest` green → commit.
  Every bugfix ships with a regression test.
- **Atomic commits, one concern each.** Commit messages state the *why* and the
  failure mode fixed. **No AI attribution** (`Co-Authored-By` / "Generated with"
  lines) — deliberate project policy.
- **Never commit** `docs/superpowers/` (local plans/specs) or `CLAUDE.md` — they're
  gitignored working artifacts. This repo ships `AGENTS.md`, not `CLAUDE.md`.

## Where things are (progressive disclosure)

Start with the module map in
[OVERVIEW.md § Module map](docs/architecture/OVERVIEW.md#module-map-where-to-look).
The short version, by concern:

| You're touching… | Go to |
|---|---|
| **The safety decision** (allow/deny) | `verify.py` (staged pipeline), `subsumption.py` (delegation), `z3_checks.py` / `predicate_z3.py` / `regex_to_z3.py` (the proofs), `dag.py` |
| **What runs, and how it's supervised** | `supervisor.py`, `executor.py`, `delegating_executor.py`, `approval.py`, `fallback.py`, `run_session.py` |
| **The data model / contract** | `models.py`, `permissions.py`, `predicate.py`, `_invariant_types.py` |
| **Envelope generation** | `envelope.py`, `tier1.py`, `claude_code_llm.py`, `llm.py` |
| **Memory → reuse** (backward loop) | `journal.py`, `distiller.py`, `gardener/`, `pathway*.py`, `signing.py` |
| **Prompt → answer** (forward loop) | `orchestrator.py`, `decomposer.py`, `synthesizer.py`, `model_sizer.py`, `budget.py` |
| **Swarm / robotics** | `swarm.py`, `executor_mujoco.py`, `vla_executor.py` |
| **Surfaces** | `cli.py`, `mcp_server.py`, `install.py` (per-harness adapters) |

Docs are organized [Diátaxis](https://diataxis.fr/)-style — see [docs/README.md](docs/README.md)
for the tutorials / how-to / reference / explanation split.

## How to work here

```bash
uv run pytest -q            # the suite — must be green before you commit
uv run ruff check .         # lint — must pass (config in pyproject [tool.ruff])
uv run ruff check . --fix   # autofix the safe findings
```

- Python 3.12+, managed with `uv`. Dev extras (`uv pip install -e ".[dev]"`) pull
  the test surface; `[robotics]`/`[lora]` stay opt-in and their tests are
  `importorskip`-gated.
- **Executors** implement one protocol: `run(step, *, timeout_s, max_output_bytes)
  -> ExecutorResult` (optionally `configure_from_envelope`). A new effectful step
  type needs a real executor *and* a verification story (a permission surface or a
  Z3 handler) — never a silent pass.
- **Function-local & `TYPE_CHECKING` imports are intentional** (break cycles, keep
  optional deps out of the hard dependency set). Don't "fix" them; the ruff config
  already accounts for them.
- LLM backend is pluggable (`llm.py`): `litellm` (API key) or `claude-code`
  (`claude -p`, no key). Forward flags to the latter with `DAISUGI_CLAUDE_ARGS`.

## When you're unsure

Prefer rejecting to admitting. Prefer a failing test to a plausible fix. Prefer
matching the surrounding code to introducing a new pattern. Prefer asking (or
leaving a `TODO` with the open question) to guessing on a safety-relevant path.
When in doubt about a decision's rationale, grep [docs/adr/](docs/adr/) before
reopening it — you may be re-litigating a settled trade-off.

# Conventions

The patterns this codebase actually follows. They're consistent throughout — this
just writes them down so a change reads like the code around it. For *why* the big
structural choices were made, see [`../adr/`](../adr/); for the shape of the
system, [`OVERVIEW.md`](OVERVIEW.md).

## Correctness

- **Fail closed.** In the verification path, when you can't *prove* safety, deny.
  A `try/except` that swallows an error into "allowed", a Z3 `unknown` treated as
  ok, a missing case that falls through to accept — all bugs. See
  [ADR-0001](../adr/0001-fail-closed-default.md). New `match` on step/permission
  types needs a default case that rejects the unknown.
- **Verify before execute.** Nothing effectful runs before its plan is proven
  inside its envelope. The `Supervisor` re-checks each step at run time; executors
  re-check resolved paths/schemes themselves (defense in depth) rather than
  trusting the plan.
- **The caller's envelope is the ceiling.** Reused pathways, delegated skill
  contracts, and MCP-supplied plans are all bounded by the *caller's* envelope,
  never their own. If you add a new authority-bearing path, verify against the
  caller.

## Testing (TDD, non-negotiable here)

- **Write the failing test first**, watch it fail for the right reason, then fix.
  Every bug fix ships with a regression test that reproduces it. The security
  campaign's discipline was: reproduce live → failing test → fix → full suite green
  → atomic commit.
- Tests are **real code, not mocks of the thing under test.** Mock the boundary
  (subprocess, network, LLM client), never the logic you're checking.
- `uv run pytest -q` must be green before committing. 1598 tests today; keep it so.
- Robotics (`mujoco`) and some heavy paths are `importorskip`-gated — that's why a
  test lives in a non-robotics file when it needs to always run.

## Style & structure

- **Ruff is the linter/formatter of record** (`[tool.ruff]` in `pyproject.toml`).
  `uv run ruff check .` must pass. The ruleset is real-bug + import hygiene
  (`F`, `E9`, `B`, `I`) — deliberately *not* line-length or pyupgrade, which are
  noise on a mature codebase. Tighten via a new ADR if the team wants more.
- **Function-local & `TYPE_CHECKING` imports are intentional**, not sloppiness:
  they break import cycles and keep optional deps (`numpy`, `mujoco`, `instructor`)
  out of the hard dependency set. That's why `E402`/`F821` are tuned in the ruff
  config rather than "fixed." Put a type-only import under `if TYPE_CHECKING:`.
- **One module, one responsibility.** The module map in [`OVERVIEW.md`](OVERVIEW.md)
  is the intended decomposition; new code joins an existing module by concern, or
  earns a new one — it doesn't bolt onto whatever's nearest.
- **Executors implement one protocol:** `run(step, *, timeout_s, max_output_bytes)
  -> ExecutorResult`. Optionally `configure_from_envelope(envelope)` to receive
  envelope-scoped limits. New effectful step types get a real executor *and* a
  verification story (a permission surface or a Z3 handler) — never a silent pass.
- Prefer small, focused files; match the comment density of the surrounding code.
  Comments explain *why* (a non-obvious invariant, a security rationale), not
  *what* the code plainly says.

## Git & releases

- **Atomic commits, one concern each.** A multi-fix pass is several commits, not
  one dump. Commit messages state the *why* and the failure mode fixed.
- **No AI attribution** in commit messages or PRs (no `Co-Authored-By` /
  "Generated with…" lines). This is a deliberate project policy.
- Never commit local working artifacts: `docs/superpowers/` (plans/specs) and
  `CLAUDE.md` stay out of the repo.
- **Releases:** bump `pyproject.toml` + `__init__.__version__`, add a `CHANGELOG.md`
  entry (with an upgrade note if defaults change), tag `vX.Y.Z`, push tag. Security
  fixes are their own release with the failure mode described.

## Local commands

```bash
uv run pytest -q            # the suite (must be green)
uv run ruff check .         # lint (must pass)
uv run ruff check . --fix   # autofix the safe findings
uv run ruff format .        # (optional) formatter — not enforced in CI yet
```

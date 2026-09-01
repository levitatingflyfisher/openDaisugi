# Brief for plan writers (read fully before writing anything)

You are writing ONE implementation plan for ONE sub-project of the openDaisugi workshop design.
Repo: /mnt/tera/working/programming/openDaisugi (git, branch master). Do not run git commands
that change state. Do not modify any file except the plan file you are told to create.

## Read, in this order
1. `/home/inspire/.claude/plugins/cache/claude-plugins-official/superpowers/6.3.0/skills/writing-plans/SKILL.md`
   — the plan FORMAT is mandatory: header block, Global Constraints, tasks with Files/Interfaces,
   bite-sized checkbox steps, real test code and real implementation code in every code step,
   commit step per task, NO placeholders ("TBD", "add error handling", "similar to Task N").
2. `docs/plans/2026-09-08-workshop/00-master-spec.md` — contracts (§3) and Global Constraints (§4).
   Copy §4 verbatim into your plan's Global Constraints section.
3. Your sub-spec `docs/plans/2026-09-08-workshop/spec-NN-*.md` — the binding requirements.
4. The repo files your spec names (read them; cite real line numbers in Modify: paths).
   Useful orientation: `AGENTS.md`, `docs/plans/2026-08-27-cockpit-spec.md` (session tree, ask
   protocol, resident gate), `docs/plans/2026-08-27-plan-3-session-tree.md` (an example of a
   plan in this repo's house style, including the "corrections from adversarial review" block —
   you do not write that block; the reviewer does).
5. For Go work: `harness/sprig/` (module layout, test style, `session_tree.go`).

## House rules that bind every plan
- TDD in every task: failing test → run → minimal code → run → commit. Show the test code.
- Test commands: Python `uv run --no-sync pytest tests/… -q`; Go `cd harness/coppice && go test ./...`.
  Lint: `uv run --no-sync ruff check .`. Never bare `uv run`.
- Commit messages: state the why; persona is set by the repo; NO Co-Authored-By / "Generated
  with" lines — project policy.
- Copy in user-facing strings: STE100 (short sentences, active voice, no em-dashes, no
  parentheticals). Errors teach the next command.
- Fail-closed everywhere. A test that would let a failure path resolve to "allow" is a plan
  defect; name fail-closed tests for the failure they prove.
- Honesty tags (`daisugi modules`, `swap.py`): every new module lands with the true tag.
- `/tmp` is RAM on this machine: scratch under the repo or `~/.cache`, never `/tmp`.
- Discovery tasks: when the spec says "plan task N reads X and pins it" (an upstream API,
  a schema, an env var name), write that as a real task whose deliverable is a recorded fact in a
  named file plus a test that asserts it. Do not guess the fact in later tasks; make later tasks
  read the recorded fact.
- Prerequisite tasks: when the spec names a host/toolchain prerequisite, task 0 is a check that
  stops with a teaching message if absent (and marks the plan's live tests as skip-with-reason).
- Size the plan to the spec's Size line (S ≈ 3–5 tasks, M ≈ 6–9, L ≈ 10–14, XL ≈ 15–22).
- Interfaces: every task's Interfaces block must name exact function/type signatures that
  neighbouring tasks consume. Types used in Task 7 must be defined in a task ≤ 7.

## Output
- Write the plan to `docs/plans/2026-09-08-workshop/plan-NN-<slug>.md` (same slug as the spec).
- Run the SKILL.md self-review (spec coverage, placeholder scan, type consistency) and fix inline.
- Reply with ONLY: the plan path, the task count, one line per spec requirement you could NOT
  map to a task (or "all mapped"), and up to five concerns/rulings (one line each). No summary
  of the plan itself.

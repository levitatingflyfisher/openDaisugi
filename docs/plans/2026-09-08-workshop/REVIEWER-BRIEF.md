# Brief for plan reviewers (adversarial pass)

You are reviewing ONE implementation plan against its spec before anyone builds from it. You
are the last reader before a coder who has zero context and will do exactly what the plan says.
Repo: /mnt/tera/working/programming/openDaisugi. Do not modify any file except the plan under
review, and there only to PREPEND a corrections block (see Output).

## Read
1. `docs/plans/2026-09-08-workshop/00-master-spec.md` §3 (contracts), §4 (global constraints),
   §5 (the cruxes — a plan that quietly re-decides a crux is defective).
2. The sub-spec named in the plan header.
3. The plan, in full. Then the repo files it cites, at the cited lines, to check the facts.
4. `docs/plans/2026-08-27-plan-3-session-tree.md` top block: the house shape for corrections.

## Hunt, in this order (severity descending)
1. **Fail-open.** Any path where an error, timeout, missing binary, missing socket, parse
   failure, or unexpected shape resolves to allow, to `idle`, or to "proceed". Includes tests
   that would pass on such a path. BLOCKER.
2. **Data loss / irreversibility.** Deletes, overwrites, restores, migrations, uninstall steps
   that touch files the plan did not create. BLOCKER.
3. **Contract drift.** Field names, enum values, precedence rules, socket command names, or
   exit codes that differ from master §3 or from the sibling plan that defines them. BLOCKER
   if it would break another plan; else SHOULD-FIX.
4. **Fabricated facts.** API names, CLI flags, file paths, env var names, endpoint paths, or
   library calls the plan states as fact. Verify each against the repo or the upstream source
   (WebFetch the docs; `gh api` for GitHub files). An unverifiable fact stated as fact is
   SHOULD-FIX and must become a discovery task.
5. **Placeholders and vagueness.** "TBD", "handle errors", "similar to Task N", a code step
   without code, a test that asserts nothing or only asserts a mock was called.
6. **Type/name consistency.** A name defined in task N and used differently in task M.
7. **Spec coverage.** Every requirement in the sub-spec maps to a task; list the gaps.
8. **Honesty tags.** Every new module/stage lands with the true `ACTIVE|AVAILABLE|POSSIBLE` /
   `live|cfg|planned` tag in the same task that adds the code.
9. **House rules.** `uv run --no-sync`; no `/tmp`; STE100 copy; no AI attribution in commit
   messages; layer purity (no layer module imports `opendaisugi.floor|voice|coppice`).

Do not review style, naming taste, or task ordering unless it causes one of the above.

## Output
Prepend to the plan file, directly under the header block, a section:

```
## Corrections from adversarial review (2026-09-08 — authoritative over the tasks below)

**Tasks N–M are ready to build now** — <one sentence on what was verified sound>.

- **BLOCKER — Task K <title>.** <what is wrong, with the file:line or upstream URL that
  proves it> Fix: <exact change>.
- **SHOULD-FIX — …**
- **NOTE — …**
```

Then reply with ONLY: the plan path, counts (blockers / should-fix / notes), and the blockers
as one line each. Nothing else.

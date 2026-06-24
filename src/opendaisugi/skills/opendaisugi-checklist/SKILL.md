---
name: opendaisugi-checklist
description: Use when a task's failure mode is silent omission (agent forgets a step) rather than wrong action — invent a problem-specific DSL of Pydantic step types, author a DAG plan in it, verify against a Z3-backed envelope, execute with per-step receipts and a run-end integrity check. Domain-agnostic: applies to shell, email, robotic motion, file ops. Core opendaisugi workflow for multi-step runs whose completeness matters.
---

# opendaisugi-checklist

**When to use this skill:** The task has ≥3 steps; success requires *every* step to actually happen; a silent skip would matter. Common triggers: orchestrating sub-agents, automating a recurring workflow, running an approval-sensitive sequence, composing a robotic motion. If the work is one shell command, skip this.

**What it does:** Teaches you to invent a problem-specific DSL of Pydantic step types, author a plan in it, verify the plan against an envelope (with optional Z3 invariants for contracts between agents), execute each step with a Receipt of evidence, and enforce a run-end integrity check that makes silent step-skipping impossible.

**Rooted in opendaisugi framing #3:** *reproduction substrate for skills.* Successful runs journal cleanly and feed distillation; the Gardener's selection signal only works because receipts + integrity make it trustworthy.

---

## Workflow

1. **Analyze the task.** What *kinds* of things will happen? Name the step-shapes — `DraftEmail`, `ReviewDraft`, `JointMove`, `Vote`. The shapes are your problem's grain.

2. **Invent the DSL.** For each step-kind, subclass `StepBase`, decorate with `@opendaisugi.step_type`, add typed fields that capture what varies per instance. Declare an optional `postcondition` when "did this really happen" has evidence worth checking.

   ```python
   from typing import Literal
   from opendaisugi.models import StepBase, step_type, Postcondition

   @step_type
   class DraftEmail(StepBase):
       type: Literal["draft_email"] = "draft_email"
       recipient: str
       body: str
       signature: str
       postcondition: Postcondition | None = Postcondition(
           type="evidence_present", path="draft_hash",
       )
   ```

3. **Author the envelope.** Permissions (what steps are allowed to touch), plus invariants when the problem has cross-step claims. Envelope invariants compile to SMT-LIB2 and Z3 solves them — use this for *structural* claims like "no step emits email with signature == 'Robin' to a Robin contact" or "count of approve votes ≥ quorum." Leave *perceptual* claims ("is the email well-written") to an LLM reviewer step.

4. **Author the plan.** An `ActionPlan` with steps of your new types, wired into a DAG via `depends_on`. Compound shell (`a && b`) is ALWAYS wrong — emit two ShellSteps with a depends_on edge instead. The verifier will reject compound shell and offer a ready-to-paste decomposition in `violation.suggested_remediation`.

5. **Verify.** `daisugi verify` runs permissions → Z3 → DAG checks. Z3 catches cross-step inconsistencies a per-call allowlist never could — that's where the whole machinery earns its keep. If it fails, read the violations and revise. Do NOT mark verification off until it passes.

6. **Execute under the supervisor.** The supervisor runs each step and writes a `Receipt` (step evidence, content-addressed hash, postcondition verify result) to the journal. If `step.postcondition` exists and fails, the run halts on that step — that's fine, halt is allowed.

7. **Integrity check at run end.** The supervisor compares receipted-step-ids vs expected-step-ids. Missing receipts → `session.integrity_passed = False` regardless of what your executor reported. This is the non-skip guarantee: cheap sub-agents claiming "✓ done" without evidence fail the check.

8. **Over time.** `daisugi tend` promotes successful journaled runs into reusable `CompiledPathway` rows. `daisugi gardener` tracks which pathways succeed vs drift. This is the reproduction loop: the DSL you invented today may power tomorrow's similar problem directly from the pathway store.

---

## Common pitfalls

- **Cramming logic into one step's command string.** If you write `command="ls && cat foo"`, the metachar gate rejects it. Emit two ShellSteps.
- **Missing postconditions on steps that matter.** If "did this happen" has observable evidence, encode it. A step with no postcondition passes integrity trivially on execution-happened; that's fine when execution happening is enough, and weak when it isn't.
- **Using Z3 for perceptual claims.** "Is this good" is LLM-as-judge territory — give it an `AgentReview` step whose postcondition returns the LLM's verdict, not an invariant.
- **Silent executor behavior.** If a sub-agent "skips" a step, it must fail on the integrity check. Don't patch around missing receipts.

---

## References

- [Authoring a DSL](references/authoring-a-dsl.md) — step-type design patterns
- [Postconditions](references/postconditions.md) — per-step verify() shapes, evidence content
- [Contract orchestration](references/contract-orchestration.md) — Z3-backed contracts between sub-agents
- [Cheap-model delegation](references/delegation.md) — `preferred_model`, `llm_check`, model_id selection signal (v0.19+)
- [Using openDaisugi via MCP](references/mcp-usage.md) — calling the same machinery as MCP tools (v0.20+)
- [Passive capture](references/passive-capture.md) — the hook for fueling distillation from external agent runtimes (v0.21+)
- [Git-backed shared registry](references/git-registry.md) — multiple opendaisugi instances share pathways through a git repo (v0.25+)
- [VLA integration](references/vla-integration.md) — VLAStep + envelope-gated rollouts for learned visuomotor policies (v0.26+)
- [Worked example: Agent council](references/worked-example-council.md)

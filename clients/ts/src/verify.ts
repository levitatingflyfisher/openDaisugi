/**
 * Port of verify.py's `_verify` — the pipeline: delegation-safety →
 * permissions → skill delegations → z3 self-consistency → z3
 * plan-vs-envelope → predicate invariants+postconditions (+ robotics z3,
 * SAME stage) → dag. Each stage short-circuits the next.
 */

import {
  decodePlan,
  decodeEnvelope,
  type ActionPlan,
  type Envelope,
  type VerificationResult,
  type Violation,
  violation,
  isSkillStep,
} from "./models.js";
import { checkPermissions, checkDelegationSafety } from "./permissions.js";
import { checkDag } from "./dag.js";
import { checkPlanInvariants, RECOGNIZED_OPAQUE_TYPES } from "./z3checks.js";
import { checkVacuity } from "./vacuity.js";
import { evaluatePredicate } from "./predicateEval.js";
import { verifyDelegation } from "./contracts.js";
import { z3CheckSat, VerificationTimeout } from "./z3client.js";
import { resolveStrict } from "./resolveStrict.js";

const RECOGNIZED_STAGE2_POSTCONDITION_TYPES: ReadonlySet<string> = new Set([
  "exit_code",
  "file_exists",
  "file_size_range",
]);

function robotBackingMissing(typeName: string, perms: Envelope["permissions"]): string | null {
  if (typeName === "end_effector_in_workspace" && perms.workspace_bounds === null) return "workspace_bounds";
  if (typeName === "velocity_bounded" && perms.velocity_limit === null) return "velocity_limit";
  return null;
}

async function checkSkillDelegations(
  plan: ActionPlan,
  envelope: Envelope,
  strict: boolean,
  timeoutMs: number
): Promise<Violation[]> {
  const skillSteps = plan.steps.filter(isSkillStep);
  if (skillSteps.length === 0) return [];
  const violations: Violation[] = [];
  for (const step of skillSteps) {
    if (step.contract_envelope === null) {
      if (strict) {
        violations.push(
          violation("delegation", `Step '${step.id}' invokes opaque skill '${step.skill_id}' with no contract_envelope; strict mode rejects`, {
            step: step.id,
            skill_id: step.skill_id,
            reason: "opaque_skill",
          })
        );
      }
      continue;
    }
    const decision = await verifyDelegation(envelope, step.contract_envelope, strict, timeoutMs, RECOGNIZED_OPAQUE_TYPES);
    if (!decision.allowed) {
      violations.push(
        violation("delegation", `Step '${step.id}' skill '${step.skill_id}' delegation refused`, {
          step: step.id,
          skill_id: step.skill_id,
          reason: "not_subsumed",
        })
      );
    }
  }
  return violations;
}

async function checkEnvelopeSelfConsistency(envelope: Envelope, timeoutMs: number): Promise<Violation[]> {
  const shell = envelope.permissions.shell;
  const canWrite = envelope.permissions.file_write.length > 0;
  const asserts: string[] = [
    "(declare-const shell Bool)",
    "(declare-const can_write Bool)",
    `(assert (= shell ${shell}))`,
    `(assert (= can_write ${canWrite}))`,
  ];
  if (envelope.permissions.shell_allowlist.length > 0) {
    asserts.push("(assert (= shell true))");
  }
  if (envelope.postconditions.some((pc) => pc.type === "file_exists")) {
    asserts.push("(assert (= can_write true))");
  }
  asserts.push(
    "(declare-const max_time Int)",
    `(assert (= max_time ${envelope.permissions.max_execution_time_s}))`,
    "(assert (> max_time 0))",
    "(assert (<= max_time 3600))"
  );
  const result = await z3CheckSat(asserts.join("\n"), timeoutMs);
  if (result === "unknown") {
    throw new VerificationTimeout(`Z3 self-consistency check exceeded ${timeoutMs}ms`);
  }
  if (result === "unsat") {
    return [violation("z3", "Envelope is internally inconsistent", {})];
  }
  return [];
}

async function checkPlanAgainstEnvelope(plan: ActionPlan, envelope: Envelope, timeoutMs: number): Promise<Violation[]> {
  const shellAvailable = envelope.permissions.shell;
  const writeAvailable = envelope.permissions.file_write.length > 0;
  const asserts: string[] = [
    "(declare-const shell_available Bool)",
    "(declare-const write_available Bool)",
    `(assert (= shell_available ${shellAvailable}))`,
    `(assert (= write_available ${writeAvailable}))`,
  ];
  if (plan.steps.some((s) => s.type === "shell")) asserts.push("(assert (= shell_available true))");
  if (plan.steps.some((s) => s.type === "file_write")) asserts.push("(assert (= write_available true))");
  const result = await z3CheckSat(asserts.join("\n"), timeoutMs);
  if (result === "unknown") {
    throw new VerificationTimeout(`Z3 plan-vs-envelope check exceeded ${timeoutMs}ms`);
  }
  if (result === "unsat") {
    return [violation("z3", "Plan requirements contradict envelope permissions", {})];
  }
  return [];
}

async function checkPredicateItem(
  label: "invariant" | "postcondition",
  typeName: string,
  rawExpr: any,
  enforce: boolean,
  plan: ActionPlan,
  envelope: Envelope,
  strict: boolean,
  timeoutMs: number
): Promise<Violation[]> {
  if (!enforce) return [];
  const expr = rawExpr ?? null;

  if (expr === null) {
    const backingReason = label === "invariant" ? robotBackingMissing(typeName, envelope.permissions) : null;
    if (backingReason !== null) {
      return [
        violation("predicate", `invariant '${typeName}' is declared but its backing permission (${backingReason}) is absent`, {
          [label]: typeName,
          reason: "robotics_invariant_unbacked",
        }),
      ];
    }
    const dischargedElsewhere =
      (label === "invariant" && RECOGNIZED_OPAQUE_TYPES.has(typeName)) ||
      (label === "postcondition" && RECOGNIZED_STAGE2_POSTCONDITION_TYPES.has(typeName));
    if (strict && !dischargedElsewhere) {
      return [
        violation("predicate", `${label} '${typeName}' declares a safety property with no verifiable expr; cannot be discharged under strict mode`, {
          [label]: typeName,
          reason: "opaque_unrecognized",
        }),
      ];
    }
    return [];
  }

  // Unresolved alias reference: the conformance wire protocol never carries
  // an AliasRegistry (recording skips registry-bearing calls — they're
  // process state, not self-contained), so `aliases` is always absent here.
  if (expr.op === "alias") {
    return [
      violation("predicate", `${label} '${typeName}' references unresolved alias '${expr.name}'; no AliasRegistry available`, {
        [label]: typeName,
        reason: "unresolved_alias",
        alias: expr.name,
      }),
    ];
  }

  let vacuityVerdict: "tautology" | "contradiction" | "non_trivial" = "non_trivial";
  try {
    // verify.py calls check_vacuity(expr) with NO timeout argument — it
    // always uses the function's own 500ms default, and does NOT thread
    // the case's z3_timeout_ms through. Matched exactly (not `timeoutMs`).
    vacuityVerdict = await checkVacuity(expr, 500);
  } catch {
    vacuityVerdict = "non_trivial";
  }

  if (vacuityVerdict === "contradiction") {
    return [
      violation("predicate", `${label} '${typeName}' can never be satisfied (unsatisfiable)`, {
        [label]: typeName,
        reason: "contradiction",
      }),
    ];
  }
  if (vacuityVerdict === "tautology" && strict) {
    return [
      violation("predicate", `${label} '${typeName}' is a tautology (constrains nothing)`, {
        [label]: typeName,
        reason: "tautology",
      }),
    ];
  }

  const stepDicts = (plan as any)._rawSteps as any[];
  let ok: boolean;
  try {
    ok = evaluatePredicate(expr, stepDicts);
  } catch (e: any) {
    return [violation("predicate", `${label} '${typeName}' evaluation error: ${e?.message ?? e}`, { [label]: typeName })];
  }
  if (!ok) {
    return [violation("predicate", `${label} '${typeName}' violated`, { [label]: typeName })];
  }
  return [];
}

async function checkPredicateInvariants(
  plan: ActionPlan,
  envelope: Envelope,
  strict: boolean,
  timeoutMs: number
): Promise<Violation[]> {
  const violations: Violation[] = [];
  for (const inv of envelope.invariants) {
    violations.push(...(await checkPredicateItem("invariant", inv.type, inv.expr, inv.enforce, plan, envelope, strict, timeoutMs)));
  }
  for (const pc of envelope.postconditions) {
    violations.push(...(await checkPredicateItem("postcondition", pc.type, pc.expr, pc.enforce, plan, envelope, strict, timeoutMs)));
  }
  return violations;
}

async function runVerify(
  plan: ActionPlan,
  envelope: Envelope,
  strictOption: boolean | null,
  z3TimeoutMs: number
): Promise<VerificationResult> {
  const effectiveStrict = resolveStrict(strictOption, envelope.stakes);
  let violations: Violation[] = [];

  violations.push(...checkDelegationSafety(plan, envelope));
  if (violations.length > 0) return { ok: false, violations, warnings: [] };

  violations.push(...(await checkPermissions(plan, envelope, effectiveStrict)));
  if (violations.length > 0) return { ok: false, violations, warnings: [] };

  violations.push(...(await checkSkillDelegations(plan, envelope, effectiveStrict, z3TimeoutMs)));
  if (violations.length > 0) return { ok: false, violations, warnings: [] };

  const warnings: string[] = [];
  try {
    violations.push(...(await checkEnvelopeSelfConsistency(envelope, z3TimeoutMs)));
  } catch (e) {
    if (e instanceof VerificationTimeout) warnings.push(String(e.message));
    else throw e;
  }
  if (violations.length > 0) return { ok: false, violations, warnings };

  try {
    violations.push(...(await checkPlanAgainstEnvelope(plan, envelope, z3TimeoutMs)));
  } catch (e) {
    if (e instanceof VerificationTimeout) warnings.push(String(e.message));
    else throw e;
  }
  if (violations.length > 0) return { ok: false, violations, warnings };

  violations.push(...(await checkPredicateInvariants(plan, envelope, effectiveStrict, z3TimeoutMs)));
  violations.push(...checkPlanInvariants(plan, envelope));
  if (violations.length > 0) return { ok: false, violations, warnings };

  violations.push(...checkDag(plan));
  return { ok: violations.length === 0, violations, warnings };
}

export async function verifyCase(caseBody: any): Promise<VerificationResult> {
  const plan = decodePlan(caseBody.plan);
  // Predicate evaluation needs the RAW step JSON (matches step.model_dump()
  // exactly — every field, dict-shaped) rather than our typed decode.
  (plan as any)._rawSteps = caseBody.plan?.steps ?? [];
  const envelope = decodeEnvelope(caseBody.envelope);
  const options = caseBody.options ?? {};
  const strictOption: boolean | null = options.strict ?? null;
  const z3TimeoutMs: number = options.z3_timeout_ms ?? 500;
  return runVerify(plan, envelope, strictOption, z3TimeoutMs);
}

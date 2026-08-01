/**
 * Port of vacuity.py's check_vacuity: is a compiled predicate a tautology,
 * a contradiction, or neither? Strips a single outer quantifier
 * (forall_steps/exists_step/forall_outputs) and compiles the inner predicate
 * over ONE free symbolic step — same encoding subsumption.ts uses.
 */

import { Scope, newEnv, compileScalar } from "./predicateZ3.js";
import { z3CheckSat } from "./z3client.js";

export type VacuityVerdict = "tautology" | "contradiction" | "non_trivial";

export async function checkVacuity(expr: any, timeoutMs: number): Promise<VacuityVerdict> {
  const inner = expr.op === "forall_steps" || expr.op === "exists_step" || expr.op === "forall_outputs" ? expr.pred : expr;

  const scope = new Scope("vac", null); // fully symbolic
  const env = newEnv();
  const term = compileScalar(inner, scope, env, "vac");

  const declareLines = [...env.declares].join("\n");
  const assumeLines = env.assumptions.join("\n");

  // Contradiction: is `term` (with domain assumptions) UNSAT?
  const satScript = [declareLines, assumeLines, `(assert ${term})`].filter(Boolean).join("\n");
  const satResult = await z3CheckSat(satScript, timeoutMs);
  if (satResult === "unsat") return "contradiction";

  // Tautology: is `Not(term)` UNSAT? Deliberately WITHOUT domain assumptions
  // (see vacuity.py) — an assumption-conditioned always-true isn't a real
  // tautology, it's a genuine constraint.
  const tautScript = [declareLines, `(assert (not ${term}))`].filter(Boolean).join("\n");
  const tautResult = await z3CheckSat(tautScript, timeoutMs);
  if (tautResult === "unsat") return "tautology";

  return "non_trivial";
}

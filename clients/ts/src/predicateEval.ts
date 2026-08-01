/**
 * Port of predicate_z3.py's `evaluate_predicate` / `_eval_scalar` /
 * `_resolve_path` — the GROUND (concrete-plan) evaluation path used inside
 * verify()'s `_check_predicate_item`. This is plain-data evaluation, no Z3.
 *
 * `llm_check` always throws here (fail-closed): it requires a live LLM call
 * in the oracle, which is neither reproducible nor present anywhere in the
 * corpus (verified by scanning every invariant/postcondition expr tree —
 * `llm_check` never appears). A thrown error becomes a predicate violation,
 * matching the oracle's own fail-closed handling of a failed/errored check.
 */

import type { Expression } from "./predicate.js";
import { pyRegexSearch } from "./pyRegex.js";
import { MISSING, resolvePath } from "./resolvePath.js";

function pyEquals(a: any, b: any): boolean {
  // Python's `==` for the JSON-shaped values we deal with (str/num/bool/
  // list/dict/None) coincides with structural equality; JS `===` doesn't
  // handle arrays/objects, so deep-compare those.
  if (a === b) return true;
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((v, i) => pyEquals(v, b[i]));
  }
  if (
    a !== null &&
    b !== null &&
    typeof a === "object" &&
    typeof b === "object" &&
    !Array.isArray(a) &&
    !Array.isArray(b)
  ) {
    const ak = Object.keys(a);
    const bk = Object.keys(b);
    if (ak.length !== bk.length) return false;
    return ak.every((k) => k in b && pyEquals(a[k], b[k]));
  }
  return false;
}

function evalScalar(expr: Expression, scope: any): boolean {
  switch (expr.op) {
    case "equals": {
      const v = resolvePath(scope, expr.path);
      return v !== MISSING && pyEquals(v, expr.value);
    }
    case "not_equals": {
      const v = resolvePath(scope, expr.path);
      return v !== MISSING && !pyEquals(v, expr.value);
    }
    case "in_set": {
      const v = resolvePath(scope, expr.path);
      return v !== MISSING && expr.values.some((x) => pyEquals(x, v));
    }
    case "not_in_set": {
      const v = resolvePath(scope, expr.path);
      return v !== MISSING && !expr.values.some((x) => pyEquals(x, v));
    }
    case "matches": {
      const v = resolvePath(scope, expr.path);
      if (v === MISSING || typeof v !== "string") return false;
      return pyRegexSearch(expr.regex, v) !== null;
    }
    case "not_matches": {
      const v = resolvePath(scope, expr.path);
      if (v === MISSING || typeof v !== "string") return true;
      return pyRegexSearch(expr.regex, v) === null;
    }
    case "numeric_range": {
      const v = resolvePath(scope, expr.path);
      if (v === MISSING || typeof v !== "number") return false;
      return expr.min <= v && v <= expr.max;
    }
    case "length_range": {
      const v = resolvePath(scope, expr.path);
      if (v === MISSING || v === null || typeof v === "number" || typeof v === "boolean") {
        return false;
      }
      const n = typeof v === "string" || Array.isArray(v) ? v.length : Object.keys(v).length;
      if (n < expr.min) return false;
      if (expr.max !== null && n > expr.max) return false;
      return true;
    }
    case "exists":
      return resolvePath(scope, expr.path) !== MISSING;
    case "is_empty": {
      const v = resolvePath(scope, expr.path);
      if (v === MISSING || v === null) return true;
      if (typeof v === "string" || Array.isArray(v)) return v.length === 0;
      if (typeof v === "object") return Object.keys(v).length === 0;
      return false;
    }
    case "and":
      return expr.children.every((c) => evalScalar(c, scope));
    case "or":
      return expr.children.some((c) => evalScalar(c, scope));
    case "not":
      return !evalScalar(expr.child, scope);
    case "implies":
      return !evalScalar(expr.a, scope) || evalScalar(expr.b, scope);
    case "llm_check":
      throw new Error("llm_check must be evaluated via evaluate_llm_check, not _eval_scalar");
    case "alias":
      throw new Error(`unresolved alias reference '${expr.name}'; resolve aliases before evaluation`);
    default:
      throw new Error(`unknown predicate op: ${(expr as any).op}`);
  }
}

export function evaluatePredicate(expr: Expression, stepDicts: any[]): boolean {
  function go(e: Expression): boolean {
    switch (e.op) {
      case "forall_steps":
        return stepDicts.every((s) => evalScalar(e.pred, s));
      case "exists_step":
        return stepDicts.some((s) => evalScalar(e.pred, s));
      case "forall_outputs": {
        const outputs = stepDicts
          .map((s) => s?.metadata?.output)
          .filter((o) => o !== undefined && o !== null)
          .map((o) => ({ output: o }));
        return outputs.every((s) => evalScalar(e.pred, s));
      }
      case "depends_on": {
        for (const s of stepDicts) {
          if (s?.id === e.step_id_a) {
            return (s?.depends_on ?? []).includes(e.step_id_b);
          }
        }
        return false;
      }
      case "before": {
        const ids = stepDicts.map((s) => s?.id);
        const ia = ids.indexOf(e.step_id_a);
        const ib = ids.indexOf(e.step_id_b);
        if (ia === -1 || ib === -1) return false;
        return ia < ib;
      }
      case "llm_check":
        throw new Error("llm_check is not reproducible in the conformance client (no live LLM)");
      case "and":
        return e.children.every((c) => go(c));
      case "or":
        return e.children.some((c) => go(c));
      case "not":
        return !go(e.child);
      case "implies":
        return !go(e.a) || go(e.b);
      default:
        return evalScalar(e, { steps: stepDicts });
    }
  }
  return go(expr);
}

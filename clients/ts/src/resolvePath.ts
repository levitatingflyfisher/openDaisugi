/**
 * Port of predicate_z3.py's `_resolve_path` — shared by the ground evaluator
 * (predicateEval.ts) and the symbolic compiler (predicateZ3.ts), which both
 * need the identical dot-path traversal semantics over a plain JSON scope.
 */

export const MISSING: unique symbol = Symbol("MISSING");
export type Missing = typeof MISSING;

export function resolvePath(obj: any, path: string): any | Missing {
  let cur = obj;
  for (const part of path.split(".")) {
    if (cur !== null && typeof cur === "object" && !Array.isArray(cur)) {
      if (!(part in cur)) return MISSING;
      cur = cur[part];
    } else {
      return MISSING;
    }
  }
  return cur;
}

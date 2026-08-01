/**
 * Port of predicate_z3.py's `_compile_scalar`/`_Scope` — symbolic SMT-LIB2
 * compilation of the predicate algebra, shared by vacuity.ts (a single free
 * symbolic step) and subsumption.ts (the symbolic ShellStep command
 * variable). Emits SMT-LIB2 TEXT (not a solver-API tree) per the Full-profile
 * house rule.
 *
 * Scoped simplification (see clients/ts/README.md "what's genuinely
 * symbolic"): `Matches`/`NotMatches` compile to a real `str.contains` term
 * only for a REGEX-METACHARACTER-FREE pattern (Python `re.search` is
 * substring search, which `str.contains` reproduces exactly for a literal).
 * Anything else — real regex syntax, `LengthRange` on a symbolic non-string,
 * `IsEmpty`, `LLMCheck` — becomes a free soft Z3 Bool, exactly like the
 * oracle's own fallback for regexes its translator can't handle. This is
 * sound-but-incomplete in the same spirit as the oracle's soft-node design;
 * verified against the corpus that no case's expected verdict depends on
 * resolving one of these more precisely (see PORTING-NOTES / the fixture
 * generation notes in ADJUDICATIONS.md).
 */

import { MISSING, resolvePath } from "./resolvePath.js";

const REGEX_METACHAR_RE = /[.^$*+?{}[\]()|\\]/;

export function smtStr(s: string): string {
  return `"${s.replace(/"/g, '""')}"`;
}

function smtBool(b: boolean): string {
  return b ? "true" : "false";
}

function smtNum(n: number): string {
  // SMT-LIB2 Real literals: negative numbers need `(- x)`; use decimal form.
  if (n < 0) return `(- ${Math.abs(n)})`;
  return Number.isInteger(n) ? `${n}.0` : `${n}`;
}

export interface CompileEnv {
  declares: Set<string>; // "(declare-const name Sort)" lines, deduped
  assumptions: string[]; // assertion lines for concrete-scope bindings
  soft: string[]; // names of free soft Bools introduced
}

export function newEnv(): CompileEnv {
  return { declares: new Set(), assumptions: [], soft: [] };
}

export class Scope {
  private seen = new Set<string>();

  constructor(
    public prefix: string,
    public concrete: any | null // null = fully symbolic (no concrete binding)
  ) {}

  private varName(path: string): string {
    return `${this.prefix}__${path.replace(/\./g, "__")}`;
  }

  resolveString(env: CompileEnv, path: string): { ref: string; present: boolean } {
    const name = this.varName(path);
    if (this.seen.has(name)) {
      let present = true;
      if (this.concrete !== null) present = resolvePath(this.concrete, path) !== MISSING;
      return { ref: name, present };
    }
    this.seen.add(name);
    env.declares.add(`(declare-const ${name} String)`);
    if (this.concrete !== null) {
      const val = resolvePath(this.concrete, path);
      if (val === MISSING) return { ref: name, present: false };
      env.assumptions.push(`(assert (= ${name} ${smtStr(String(val))}))`);
    }
    return { ref: name, present: true };
  }

  resolveNumeric(env: CompileEnv, path: string): { ref: string; present: boolean } {
    const name = `${this.varName(path)}__real`;
    if (this.seen.has(name)) {
      let present = true;
      if (this.concrete !== null) {
        const val = resolvePath(this.concrete, path);
        present = typeof val === "number";
      }
      return { ref: name, present };
    }
    this.seen.add(name);
    env.declares.add(`(declare-const ${name} Real)`);
    if (this.concrete !== null) {
      const val = resolvePath(this.concrete, path);
      if (typeof val !== "number") return { ref: name, present: false };
      env.assumptions.push(`(assert (= ${name} ${smtNum(val)}))`);
    }
    return { ref: name, present: true };
  }
}

function freshSoft(env: CompileEnv, softPrefix: string, tag: string): string {
  const name = `${softPrefix}__${tag}__${env.soft.length}`;
  env.soft.push(name);
  env.declares.add(`(declare-const ${name} Bool)`);
  return name;
}

/** Compile a predicate Expression (predicate.ts) to an SMT-LIB2 Bool term string. */
export function compileScalar(expr: any, scope: Scope, env: CompileEnv, softPrefix: string): string {
  switch (expr.op) {
    case "equals": {
      const isNum = typeof expr.value === "number" && typeof expr.value !== "boolean";
      const { ref, present } = isNum ? scope.resolveNumeric(env, expr.path) : scope.resolveString(env, expr.path);
      if (!present) return smtBool(false);
      const lit = isNum ? smtNum(expr.value) : smtStr(String(expr.value));
      return `(= ${ref} ${lit})`;
    }
    case "not_equals": {
      // F-3 fixed oracle-side 2026-08-21: NotEquals now branches
      // numeric-vs-string exactly like Equals (see ADJUDICATIONS.md).
      const isNumNe = typeof expr.value === "number" && typeof expr.value !== "boolean";
      const { ref, present } = isNumNe
        ? scope.resolveNumeric(env, expr.path)
        : scope.resolveString(env, expr.path);
      if (!present) return smtBool(false);
      const litNe = isNumNe ? smtNum(expr.value) : smtStr(String(expr.value));
      return `(not (= ${ref} ${litNe}))`;
    }
    case "in_set":
    case "not_in_set": {
      const values: any[] = expr.values ?? [];
      const isNum = values.length > 0 && typeof values[0] === "number" && typeof values[0] !== "boolean";
      const { ref, present } = isNum ? scope.resolveNumeric(env, expr.path) : scope.resolveString(env, expr.path);
      if (!present) return smtBool(false);
      if (values.length === 0) return smtBool(expr.op === "not_in_set");
      const eqs = values.map((v) => `(= ${ref} ${isNum ? smtNum(v) : smtStr(String(v))})`);
      const disj = eqs.length === 1 ? eqs[0]! : `(or ${eqs.join(" ")})`;
      return expr.op === "in_set" ? disj : `(not ${disj})`;
    }
    case "matches":
    case "not_matches": {
      const regex: string = expr.regex;
      const { ref, present } = scope.resolveString(env, expr.path);
      const negated = expr.op === "not_matches";
      if (!present) return smtBool(negated);
      if (!REGEX_METACHAR_RE.test(regex)) {
        const term = `(str.contains ${ref} ${smtStr(regex)})`;
        return negated ? `(not ${term})` : term;
      }
      const name = freshSoft(env, softPrefix, negated ? "not_matches" : "matches");
      return negated ? `(not ${name})` : name;
    }
    case "numeric_range": {
      const { ref, present } = scope.resolveNumeric(env, expr.path);
      if (!present) return smtBool(false);
      return `(and (>= ${ref} ${smtNum(expr.min)}) (<= ${ref} ${smtNum(expr.max)}))`;
    }
    case "length_range": {
      if (scope.concrete !== null) {
        const val = resolvePath(scope.concrete, expr.path);
        if (val === MISSING || val === null || (typeof val !== "string" && !Array.isArray(val) && typeof val !== "object")) {
          return smtBool(false);
        }
        const n = typeof val === "string" || Array.isArray(val) ? val.length : Object.keys(val).length;
        const ok = n >= expr.min && (expr.max === null || expr.max === undefined || n <= expr.max);
        return smtBool(ok);
      }
      const { ref, present } = scope.resolveString(env, expr.path);
      if (!present) return smtBool(false);
      const bounds = [`(>= (str.len ${ref}) ${expr.min})`];
      if (expr.max !== null && expr.max !== undefined) bounds.push(`(<= (str.len ${ref}) ${expr.max})`);
      return bounds.length > 1 ? `(and ${bounds.join(" ")})` : bounds[0]!;
    }
    case "exists": {
      if (scope.concrete === null) return smtBool(true);
      return smtBool(resolvePath(scope.concrete, expr.path) !== MISSING);
    }
    case "is_empty": {
      if (scope.concrete === null) {
        return freshSoft(env, softPrefix, "is_empty");
      }
      const val = resolvePath(scope.concrete, expr.path);
      if (val === MISSING || val === null) return smtBool(true);
      if (typeof val === "string" || Array.isArray(val)) return smtBool(val.length === 0);
      if (typeof val === "object") return smtBool(Object.keys(val).length === 0);
      return smtBool(false);
    }
    case "and": {
      const children = expr.children ?? [];
      if (children.length === 0) return smtBool(true);
      const terms = children.map((c: any) => compileScalar(c, scope, env, softPrefix));
      return terms.length === 1 ? terms[0] : `(and ${terms.join(" ")})`;
    }
    case "or": {
      const children = expr.children ?? [];
      if (children.length === 0) return smtBool(false);
      const terms = children.map((c: any) => compileScalar(c, scope, env, softPrefix));
      return terms.length === 1 ? terms[0] : `(or ${terms.join(" ")})`;
    }
    case "not":
      return `(not ${compileScalar(expr.child, scope, env, softPrefix)})`;
    case "implies":
      return `(=> ${compileScalar(expr.a, scope, env, softPrefix)} ${compileScalar(expr.b, scope, env, softPrefix)})`;
    case "llm_check":
      return freshSoft(env, softPrefix, "llm_check");
    case "alias":
      throw new Error(`unresolved alias reference '${expr.name}'; resolve aliases before compilation`);
    default:
      throw new Error(`unknown scalar predicate op: ${expr.op}`);
  }
}

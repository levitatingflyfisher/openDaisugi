/**
 * Port of subsumption.py's `envelope_subsumes` — the skill-delegation
 * Z3 primitive. Never calls `(get-model)`: a counterexample's CONTENT
 * (the concrete step Z3 finds) only ever feeds `reason` strings, which are
 * informative (not compared) — only sat/unsat/unknown drives the verdict.
 */

import type { Envelope, Permission } from "./models.js";
import { SHELL_INTERPRETERS } from "./models.js";
import { Scope, newEnv, compileScalar, smtStr } from "./predicateZ3.js";
import { z3CheckSat, VerificationTimeout } from "./z3client.js";

export interface SubsumptionResult {
  holds: boolean;
  unverifiedInvariants: string[];
  reasons: string[];
}

function globToSmt(pathVar: string, glob: string): string {
  if (glob === "**") return "true";
  if (glob.endsWith("/**")) {
    const prefix = glob.slice(0, -3);
    return `(or (str.prefixof ${smtStr(prefix + "/")} ${pathVar}) (= ${pathVar} ${smtStr(prefix)}))`;
  }
  if (!glob.includes("*")) {
    return `(= ${pathVar} ${smtStr(glob)})`;
  }
  if (glob.startsWith("*") && !glob.slice(1).includes("*")) {
    const suffix = glob.slice(1);
    return `(str.suffixof ${smtStr(suffix)} ${pathVar})`;
  }
  if ((glob.match(/\*/g) ?? []).length === 1) {
    const idx = glob.indexOf("*");
    const prefix = glob.slice(0, idx);
    const suffix = glob.slice(idx + 1);
    return `(and (str.prefixof ${smtStr(prefix)} ${pathVar}) (str.suffixof ${smtStr(suffix)} ${pathVar}))`;
  }
  return "true"; // unsupported shape — approximated permissive (matches oracle)
}

function globUnsupported(glob: string): boolean {
  if (glob === "**" || glob.endsWith("/**") || !glob.includes("*")) return false;
  if (glob.startsWith("*") && !glob.slice(1).includes("*")) return false;
  return (glob.match(/\*/g) ?? []).length !== 1;
}

async function patternsSubsume(
  innerPatterns: string[],
  outerPatterns: string[],
  label: string,
  timeoutMs: number
): Promise<string | null> {
  if (innerPatterns.length === 0) return null;
  const badOuter = outerPatterns.filter(globUnsupported);
  if (badOuter.length > 0) {
    return `${label}: outer declares a glob shape that cannot be soundly encoded (${JSON.stringify(
      badOuter
    )}); cannot prove subsumption → denied`;
  }
  const innerOk =
    innerPatterns.length === 1
      ? globToSmt("v", innerPatterns[0]!)
      : `(or ${innerPatterns.map((g) => globToSmt("v", g)).join(" ")})`;
  const outerOk =
    outerPatterns.length === 0
      ? "false"
      : outerPatterns.length === 1
        ? globToSmt("v", outerPatterns[0]!)
        : `(or ${outerPatterns.map((g) => globToSmt("v", g)).join(" ")})`;
  const script = `(declare-const v String)\n(assert ${innerOk})\n(assert (not ${outerOk}))`;
  const result = await z3CheckSat(script, timeoutMs);
  if (result === "sat") {
    return `${label}: inner admits a value which outer forbids`;
  }
  if (result !== "unsat") {
    return `${label}: could not prove subsumption (solver ${result}) → denied`;
  }
  return null;
}

function networkScopeViolation(outer: Permission, inner: Permission): string | null {
  if (!inner.network) return null;
  if (!outer.network) return "network: inner uses network but outer forbids it";
  if (outer.network_hosts.length === 0) return null;
  if (inner.network_hosts.length === 0) {
    return `network: inner admits any host but outer restricts to ${JSON.stringify(outer.network_hosts)}`;
  }
  const outerSet = new Set(outer.network_hosts.map((h) => h.toLowerCase()));
  const extra = inner.network_hosts.filter((h) => !outerSet.has(h.toLowerCase()));
  if (extra.length > 0) {
    return `network: inner hosts ${JSON.stringify(extra)} not in outer allowlist ${JSON.stringify(outer.network_hosts)}`;
  }
  return null;
}

async function permissionScopeViolation(outer: Permission, inner: Permission, timeoutMs: number): Promise<string | null> {
  if (inner.shell_allow_decomposition && !outer.shell_allow_decomposition) {
    return "shell_allow_decomposition: inner admits compound shell but outer does not";
  }
  for (const [label, innerP, outerP] of [
    ["file_read", inner.file_read, outer.file_read],
    ["file_write", inner.file_write, outer.file_write],
    ["mcp_allowlist", inner.mcp_allowlist, outer.mcp_allowlist],
  ] as const) {
    const reason = await patternsSubsume(innerP, outerP, label, timeoutMs);
    if (reason !== null) return reason;
  }
  return networkScopeViolation(outer, inner);
}

export function robotCapabilityViolation(outer: Permission, inner: Permission): string | null {
  if (outer.workspace_bounds !== null) {
    if (inner.workspace_bounds === null) {
      return "inner declares no workspace_bounds but outer constrains the workspace (undeclared = unbounded → denied)";
    }
    const [oMin, oMax] = outer.workspace_bounds;
    const [iMin, iMax] = inner.workspace_bounds;
    for (let k = 0; k < 3; k++) {
      if (iMin[k]! < oMin[k]! || iMax[k]! > oMax[k]!) {
        return `inner workspace_bounds exceed outer`;
      }
    }
  }
  for (const axis of ["velocity_limit", "torque_limit"] as const) {
    const oLim = outer[axis];
    if (oLim !== null) {
      const iLim = inner[axis];
      if (iLim === null) return `inner declares no ${axis} but outer caps it (undeclared → denied)`;
      if (iLim > oLim) return `inner ${axis} ${iLim} exceeds outer ${oLim}`;
    }
  }
  for (const [joint, [oLo, oHi]] of Object.entries(outer.joint_limits)) {
    const iRange = inner.joint_limits[joint];
    if (!iRange) return `inner does not bound joint '${joint}' that outer limits (undeclared → denied)`;
    const [iLo, iHi] = iRange;
    if (iLo < oLo || iHi > oHi) return `inner joint '${joint}' range exceeds outer`;
  }
  const freeze = (boxes: Permission["obstacles"]) => new Set(boxes.map((b) => JSON.stringify(b)));
  const outerSet = freeze(outer.obstacles);
  const innerSet = freeze(inner.obstacles);
  const missing = [...outerSet].filter((o) => !innerSet.has(o));
  if (missing.length > 0) {
    return `inner omits ${missing.length} obstacle region(s) the outer forbids (undeclared forbidden region → denied)`;
  }
  return null;
}

function detectInterpreters(perms: Permission): string[] {
  if (!perms.shell) return [];
  return [...new Set(perms.shell_allowlist.filter((n) => SHELL_INTERPRETERS.has(n)))].sort();
}

function shellHeadInAllowlist(cmdVar: string, allowlist: string[]): string {
  if (allowlist.length === 0) return "false";
  const pieces = allowlist.flatMap((head) => [
    `(= ${cmdVar} ${smtStr(head)})`,
    `(str.prefixof ${smtStr(head + " ")} ${cmdVar})`,
  ]);
  return `(or ${pieces.join(" ")})`;
}

function encodeShellAdmission(perms: Permission, cmdVar: string): string {
  if (!perms.shell) return "false";
  const headOk = shellHeadInAllowlist(cmdVar, perms.shell_allowlist);
  const metachars = [";", "|", "&", "`", "<", ">", "\n", "\r"];
  const substrings = ["$("];
  const noMeta = [
    ...metachars.map((ch) => `(not (str.contains ${cmdVar} ${smtStr(ch)}))`),
    ...substrings.map((s) => `(not (str.contains ${cmdVar} ${smtStr(s)}))`),
  ].join(" ");
  return `(and ${headOk} (and ${noMeta}))`;
}

function compileInvariants(
  invariants: Envelope["invariants"],
  scope: Scope,
  env: ReturnType<typeof newEnv>,
  strict: boolean,
  recognizedOpaqueTypes: ReadonlySet<string>
): { term: string; opaque: string[]; strictBlocking: string[] } {
  const opaque: string[] = [];
  const strictBlocking: string[] = [];
  const terms: string[] = [];
  for (const inv of invariants) {
    if (!inv.enforce) continue;
    if (inv.expr === null || inv.expr === undefined) {
      if (strict && !recognizedOpaqueTypes.has(inv.type)) strictBlocking.push(inv.type);
      else opaque.push(inv.type);
      continue;
    }
    let expr = inv.expr;
    if (expr.op === "forall_steps" || expr.op === "exists_step") expr = expr.pred;
    terms.push(compileScalar(expr, scope, env, scope.prefix));
  }
  if (terms.length === 0) return { term: "true", opaque, strictBlocking };
  if (terms.length === 1) return { term: terms[0]!, opaque, strictBlocking };
  return { term: `(and ${terms.join(" ")})`, opaque, strictBlocking };
}

export async function envelopeSubsumes(
  outer: Envelope,
  inner: Envelope,
  timeoutMs: number,
  strict: boolean,
  recognizedOpaqueTypes: ReadonlySet<string>
): Promise<SubsumptionResult> {
  const robotViolation = robotCapabilityViolation(outer.permissions, inner.permissions);
  if (robotViolation !== null) {
    return {
      holds: false,
      unverifiedInvariants: [],
      reasons: [`robot capability subsumption failed (fail-closed): ${robotViolation}`],
    };
  }

  const scopeViolation = await permissionScopeViolation(outer.permissions, inner.permissions, timeoutMs);
  if (scopeViolation !== null) {
    return {
      holds: false,
      unverifiedInvariants: [],
      reasons: [`permission scope subsumption failed (fail-closed): ${scopeViolation}`],
    };
  }

  const policy = outer.shell_interpreter_policy;
  const innerInterpreters = detectInterpreters(inner.permissions);
  const outerInterpreters = detectInterpreters(outer.permissions);
  if (policy === "strict" && innerInterpreters.length > 0) {
    return {
      holds: false,
      unverifiedInvariants: [
        ...new Set([...innerInterpreters, ...outerInterpreters].map((n) => `shell_interpreter:${n}`)),
      ].sort(),
      reasons: [],
    };
  }

  const cmd = "ctx_command";

  // Inner and outer are compiled with INDEPENDENT CompileEnvs (independent
  // soft-node counters, matching the oracle's separate soft_inner/soft_outer
  // lists — soft names are position-keyed within a scope, so inner and outer
  // can legitimately collide on the SAME name at the same position, which is
  // how "both sides declare the same soft predicate" ends up sharing a Z3
  // Bool). Declarations/assumptions are then merged: since both scopes use
  // the SAME "ctx" prefix, a shared path (e.g. "command") always yields the
  // IDENTICAL variable name from either compile, so a plain Set union over
  // "(declare-const …)" lines is exactly the sharing the oracle gets from
  // pre-seeding both `_Scope.vars["ctx__command"]` with the same Z3 var.
  const envInner = newEnv();
  const scopeInner = new Scope("ctx", null);
  envInner.declares.add(`(declare-const ${cmd} String)`);
  const envOuter = newEnv();
  const scopeOuter = new Scope("ctx", null);
  envOuter.declares.add(`(declare-const ${cmd} String)`);

  const innerShell = encodeShellAdmission(inner.permissions, cmd);
  const outerShell = encodeShellAdmission(outer.permissions, cmd);

  const innerCompiled = compileInvariants(inner.invariants, scopeInner, envInner, strict, recognizedOpaqueTypes);
  const outerCompiled = compileInvariants(outer.invariants, scopeOuter, envOuter, strict, recognizedOpaqueTypes);

  if (strict && innerCompiled.strictBlocking.length > 0) {
    return {
      holds: false,
      unverifiedInvariants: [...new Set([...outerCompiled.opaque, ...outerCompiled.strictBlocking])].sort(),
      reasons: innerCompiled.strictBlocking
        .sort()
        .map((t) => `inner invariant '${t}' is opaque and unrecognized; delegation cannot be proven safe under strict mode`),
    };
  }

  const innerAdmits = `(and ${innerShell} ${innerCompiled.term})`;
  const outerAdmits = `(and ${outerShell} ${outerCompiled.term})`;

  // Soft-node polarity: bind every soft node introduced compiling INNER to
  // true (optimistic — inner is maximally permissive). Any soft node that
  // only came from OUTER (not sharing a name with an inner soft node) fails
  // closed: subsumption can't be proven (oracle: Attack C from the v0.11
  // audit — pinning outer-only soft constraints to False was fail-OPEN under
  // negation).
  const softInnerNames = new Set(envInner.soft);
  const softOuterUniqueNames = envOuter.soft.filter((n) => !softInnerNames.has(n));

  if (softOuterUniqueNames.length > 0) {
    return {
      holds: false,
      unverifiedInvariants: [
        ...new Set([
          ...innerCompiled.opaque,
          ...outerCompiled.opaque,
          ...outerCompiled.strictBlocking,
          ...softOuterUniqueNames.map((n) => `soft:${n}`),
        ]),
      ].sort(),
      reasons: [
        `outer has unverifiable soft constraints not shared by inner (${JSON.stringify(
          softOuterUniqueNames.sort()
        )}); cannot prove subsumption (fail-closed)`,
      ],
    };
  }

  const declareLines = [...new Set([...envInner.declares, ...envOuter.declares])].join("\n");
  const assumeLines = [...envInner.assumptions, ...envOuter.assumptions].join("\n");
  const softBindings = envInner.soft.map((n) => `(assert (= ${n} true))`).join("\n");
  const script = [
    declareLines,
    assumeLines,
    softBindings,
    `(assert ${innerAdmits})`,
    `(assert (not ${outerAdmits}))`,
  ]
    .filter(Boolean)
    .join("\n");

  const result = await z3CheckSat(script, timeoutMs);
  if (result === "unknown") {
    throw new VerificationTimeout(`Z3 subsumption check exceeded ${timeoutMs}ms`);
  }

  const interpreterSurface =
    policy !== "allow" ? [...innerInterpreters, ...outerInterpreters].map((n) => `shell_interpreter:${n}`) : [];
  const unverified = [
    ...new Set([
      ...innerCompiled.opaque,
      ...outerCompiled.opaque,
      ...outerCompiled.strictBlocking,
      ...interpreterSurface,
    ]),
  ].sort();

  if (result === "unsat") {
    return { holds: true, unverifiedInvariants: unverified, reasons: [] };
  }
  return { holds: false, unverifiedInvariants: unverified, reasons: [] };
}

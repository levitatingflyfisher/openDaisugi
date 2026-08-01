/**
 * Port of verify.py's permission stage: check_permissions, _check_shell_command
 * (+ decomposition opt-in), _verify_simple_command, _check_redirect_scopes,
 * _check_agentic_step, _check_delegation_safety.
 */

import type { ActionPlan, Envelope, Permission, Violation } from "./models.js";
import { violation, isKnownStepType, isAgenticStep } from "./models.js";
import { headAllowed } from "./headAllowed.js";
import { pathMatchesAny } from "./pathScopes.js";
import { extractShellHead } from "./shellHead.js";
import { SHELL_METACHAR_RE } from "./shellMetachar.js";
import { parseInterpreter } from "./interpreterParse.js";
import { decomposeCommand, type Decomposition } from "./shellDecompose.js";

const MAX_INTERPRETER_DEPTH = 4;

const SANCTIONED_WRITE_SINKS = new Set(["/dev/null", "/dev/stdout", "/dev/stderr"]);
const SANCTIONED_READ_SOURCES = new Set(["/dev/null", "/dev/stdin"]);

async function checkRedirectScopes(decomp: Decomposition, stepId: string, perms: Permission): Promise<Violation[]> {
  const violations: Violation[] = [];
  for (const path of decomp.writes) {
    if (SANCTIONED_WRITE_SINKS.has(path)) continue;
    if (!pathMatchesAny(path, perms.file_write)) {
      violations.push(
        violation("permissions", `Step '${stepId}' shell redirect writes '${path}' outside file_write scope`, {
          step: stepId,
          redirect_write: path,
        })
      );
    }
  }
  for (const path of decomp.reads) {
    if (SANCTIONED_READ_SOURCES.has(path)) continue;
    if (!pathMatchesAny(path, perms.file_read)) {
      violations.push(
        violation("permissions", `Step '${stepId}' shell redirect reads '${path}' outside file_read scope`, {
          step: stepId,
          redirect_read: path,
        })
      );
    }
  }
  return violations;
}

async function verifySimpleCommand(
  command: string,
  stepId: string,
  perms: Permission,
  policy: string,
  depth: number
): Promise<Violation[]> {
  const violations: Violation[] = [];
  const stripped = command.trim();
  const head = extractShellHead(stripped);
  if (head === null) return violations;
  if (!headAllowed(head, perms.shell_allowlist)) {
    violations.push(
      violation("permissions", `Step '${stepId}' shell command '${head}' not in allowlist`, {
        step: stepId,
        command_head: head,
        depth,
      })
    );
    return violations;
  }
  const payload = parseInterpreter(command);
  if (payload === null) return violations;
  if (payload.opaque) {
    if (policy === "strict") {
      violations.push(
        violation(
          "permissions",
          `Step '${stepId}' invokes opaque interpreter '${payload.head}' whose payload cannot be recursively verified (strict shell_interpreter_policy rejects)`,
          { step: stepId, interpreter: payload.head }
        )
      );
    }
    return violations;
  }
  for (const inner of payload.innerCommands) {
    violations.push(...(await checkShellCommand(inner, stepId, perms, policy, depth + 1)));
  }
  return violations;
}

export async function checkShellCommand(
  command: string,
  stepId: string,
  perms: Permission,
  policy: string,
  depth = 0
): Promise<Violation[]> {
  if (depth > MAX_INTERPRETER_DEPTH) {
    return [
      violation(
        "permissions",
        `Step '${stepId}' interpreter recursion exceeded max depth ${MAX_INTERPRETER_DEPTH}`,
        { step: stepId, command, depth }
      ),
    ];
  }
  const stripped = command.trim();
  if (!stripped) return [];

  if (SHELL_METACHAR_RE.test(command)) {
    if (perms.shell_allow_decomposition && depth <= MAX_INTERPRETER_DEPTH) {
      const decomp = await decomposeCommand(command);
      if (decomp.ok) {
        const violations: Violation[] = [];
        violations.push(...(await checkRedirectScopes(decomp, stepId, perms)));
        for (const simple of decomp.commands) {
          violations.push(...(await verifySimpleCommand(simple, stepId, perms, policy, depth)));
        }
        return violations;
      }
    }
    return [
      violation(
        "permissions",
        `Step '${stepId}' shell command contains dangerous metacharacters (;, |, &, \`, <, >, $(, newline)` +
          (depth ? ` (inside interpreter at depth ${depth})` : ""),
        { step: stepId, command, depth }
      ),
    ];
  }
  return verifySimpleCommand(stripped, stepId, perms, policy, depth);
}

const AGENTIC_TOOL_CAPABILITIES: Record<string, keyof Permission> = {
  Bash: "shell",
  Read: "file_read",
  Glob: "file_read",
  Grep: "file_read",
  Write: "file_write",
  Edit: "file_write",
  MultiEdit: "file_write",
  WebFetch: "network",
  WebSearch: "network",
};

function checkAgenticStep(step: any, perms: Permission): Violation[] {
  const violations: Violation[] = [];
  if (!step.tools || step.tools.length === 0) {
    violations.push(
      violation(
        "permissions",
        `Step '${step.id}' is agentic but requests no tools — a tool-less delegated subtask is a TaskStep (pure reasoning); use that instead`,
        { step: step.id }
      )
    );
    return violations;
  }
  if (!pathMatchesAny(step.workspace, perms.file_read)) {
    violations.push(
      violation(
        "permissions",
        `Step '${step.id}' agentic workspace '${step.workspace}' is not inside the envelope's file_read globs — a sub-agent must be able to read its own working directory`,
        { step: step.id, workspace: step.workspace }
      )
    );
  }
  for (const tool of step.tools as string[]) {
    const cap = AGENTIC_TOOL_CAPABILITIES[tool];
    if (cap === undefined) {
      violations.push(
        violation("permissions", `Step '${step.id}' requests host tool '${tool}' which has no capability mapping — denied by default`, {
          step: step.id,
          tool,
        })
      );
      continue;
    }
    const granted = perms[cap];
    const isGranted = Array.isArray(granted) ? granted.length > 0 : !!granted;
    if (!isGranted) {
      violations.push(
        violation("permissions", `Step '${step.id}' requests host tool '${tool}' but the envelope grants no ${cap} capability`, {
          step: step.id,
          tool,
          capability: cap,
        })
      );
    }
  }
  return violations;
}

export async function checkPermissions(plan: ActionPlan, envelope: Envelope, strict: boolean): Promise<Violation[]> {
  const violations: Violation[] = [];
  const perms = envelope.permissions;

  for (const step of plan.steps) {
    switch (step.type) {
      case "shell": {
        const shellStep = step as import("./models.js").ShellStep;
        if (!perms.shell) {
          violations.push(
            violation("permissions", `Step '${step.id}' requires shell but envelope forbids it`, { step: step.id })
          );
          continue;
        }
        violations.push(...(await checkShellCommand(shellStep.command, step.id, perms, envelope.shell_interpreter_policy)));
        break;
      }
      case "network": {
        const netStep = step as import("./models.js").NetworkStep;
        if (!perms.network) {
          violations.push(
            violation("permissions", `Step '${step.id}' requires network but envelope forbids it`, { step: step.id })
          );
          continue;
        }
        const scheme = pyUrlparseScheme(netStep.url);
        if (scheme !== "http" && scheme !== "https") {
          violations.push(
            violation("permissions", `Step '${step.id}' network URL scheme '${scheme}' not allowed (only http/https)`, {
              step: step.id,
              scheme,
              url: netStep.url,
            })
          );
          continue;
        }
        if (perms.network_hosts.length > 0) {
          const host = pyUrlparseHostname(netStep.url);
          const allowed = new Set(perms.network_hosts.map((h) => h.toLowerCase()));
          if (!allowed.has(host)) {
            violations.push(
              violation("permissions", `Step '${step.id}' network host '${host}' not in network_hosts allowlist`, {
                step: step.id,
                host,
                url: netStep.url,
              })
            );
          }
        }
        break;
      }
      case "file_read": {
        const readStep = step as import("./models.js").FileReadStep;
        if (!pathMatchesAny(readStep.path, perms.file_read)) {
          violations.push(
            violation("permissions", `Step '${step.id}' file_read path '${readStep.path}' not permitted by file_read`, {
              step: step.id,
              path: readStep.path,
            })
          );
        }
        break;
      }
      case "file_write": {
        const writeStep = step as import("./models.js").FileWriteStep;
        if (!pathMatchesAny(writeStep.path, perms.file_write)) {
          violations.push(
            violation("permissions", `Step '${step.id}' file_write path '${writeStep.path}' not permitted by file_write`, {
              step: step.id,
              path: writeStep.path,
            })
          );
        }
        break;
      }
      case "mcp": {
        const mcpStep = step as import("./models.js").MCPStep;
        const key = `${mcpStep.server}/${mcpStep.tool}`;
        if (!headAllowed(key, perms.mcp_allowlist)) {
          violations.push(
            violation("permissions", `Step '${step.id}' MCP tool '${key}' not in mcp_allowlist`, {
              step: step.id,
              mcp_tool: key,
            })
          );
        }
        break;
      }
      case "agentic": {
        if (isAgenticStep(step)) violations.push(...checkAgenticStep(step, perms));
        break;
      }
      default: {
        if (strict && !isKnownStepType(step.type) && !perms.custom_step_allowlist.includes(step.type)) {
          violations.push(
            violation(
              "permissions",
              `Step '${step.id}' has unverifiable step type '${step.type}' (no permission surface or handler); rejected under strict mode`,
              { step: step.id, type: step.type }
            )
          );
        }
      }
    }
  }

  return violations;
}

export function checkDelegationSafety(plan: ActionPlan, envelope: Envelope): Violation[] {
  if (envelope.stakes !== "physical") return [];
  const violations: Violation[] = [];
  for (const step of plan.steps) {
    if (step.type === "agentic") {
      violations.push(
        violation(
          "permissions",
          `Step '${step.id}' is an agentic delegation but envelope stakes='physical'; physical-stakes plans cannot be LLM-delegated`,
          { step: step.id, stakes: "physical" }
        )
      );
      continue;
    }
    if (step.preferred_model) {
      violations.push(
        violation(
          "permissions",
          `Step '${step.id}' requests delegation to '${step.preferred_model}' but envelope stakes='physical'; physical-stakes plans cannot be LLM-delegated`,
          { step: step.id, stakes: "physical", preferred_model: step.preferred_model }
        )
      );
    }
  }
  return violations;
}

// --- urlparse port (scheme/hostname extraction only, matching Python's
// urllib.parse.urlparse: never throws, lowercases scheme+hostname, '' when
// absent — NOT the same as `new URL()`, which throws and normalizes
// differently). ---

const URLPARSE_RE = /^(?:([a-zA-Z][a-zA-Z0-9+.\-]*):)?(?:\/\/([^/?#]*))?/;

function splitAuthority(raw: string): string {
  // Strip userinfo (before last '@') and port (after last ':' following ']'
  // or not containing ']'), matching urlparse's splitnetloc/hostname logic
  // closely enough for the corpus's http(s) URLs.
  let authority = raw;
  const atIdx = authority.lastIndexOf("@");
  if (atIdx !== -1) authority = authority.slice(atIdx + 1);
  if (authority.startsWith("[")) {
    const end = authority.indexOf("]");
    return end === -1 ? authority : authority.slice(1, end);
  }
  const colonIdx = authority.indexOf(":");
  return colonIdx === -1 ? authority : authority.slice(0, colonIdx);
}

export function pyUrlparseScheme(url: string): string {
  const m = URLPARSE_RE.exec(url);
  const scheme = m?.[1] ?? "";
  return scheme.toLowerCase();
}

export function pyUrlparseHostname(url: string): string {
  const m = URLPARSE_RE.exec(url);
  const netloc = m?.[2] ?? "";
  return splitAuthority(netloc).toLowerCase();
}

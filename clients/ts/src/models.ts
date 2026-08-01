/**
 * TypeScript port of opendaisugi/models.py — data shapes only.
 *
 * Cases arrive as full pydantic `model_dump(mode="json")` dumps: every field
 * is present (defaults already filled in by the oracle), snake_case keys
 * matching the Python attribute names verbatim. Decoding here is therefore
 * tolerant/structural, not a from-scratch validator: we trust the JSON shape
 * and fill in sane fallbacks only where a hand-built test envelope might
 * omit an optional field.
 */

export type Stakes = "low" | "medium" | "high" | "physical";
export type ShellInterpreterPolicy = "surface" | "strict" | "allow";

// v0.13.0: names treated as shell interpreters for policy purposes.
export const SHELL_INTERPRETERS: ReadonlySet<string> = new Set([
  "sh",
  "bash",
  "zsh",
  "fish",
  "dash",
  "ksh",
  "csh",
  "tcsh",
  "xargs",
  "find",
  "python",
  "python3",
  "python2",
  "perl",
  "ruby",
  "node",
  "deno",
  "make",
  "awk",
  "gawk",
  "sed",
  "eval",
  "exec",
  "source",
  "env",
  "timeout",
  "nice",
  "nohup",
  "time",
  "stdbuf",
  "command",
  "setsid",
  "ionice",
  "sudo",
  "doas",
  "watch",
]);

export interface Permission {
  file_read: string[];
  file_write: string[];
  network: boolean;
  network_hosts: string[];
  shell: boolean;
  shell_allowlist: string[];
  shell_allow_decomposition: boolean;
  mcp_allowlist: string[];
  custom_step_allowlist: string[];
  max_execution_time_s: number;
  max_output_size_mb: number;
  workspace_bounds: [[number, number, number], [number, number, number]] | null;
  obstacles: [[number, number, number], [number, number, number]][];
  velocity_limit: number | null;
  joint_limits: Record<string, [number, number]>;
  torque_limit: number | null;
}

export function decodePermission(raw: any): Permission {
  raw ??= {};
  return {
    file_read: raw.file_read ?? [],
    file_write: raw.file_write ?? [],
    network: !!raw.network,
    network_hosts: raw.network_hosts ?? [],
    shell: !!raw.shell,
    shell_allowlist: raw.shell_allowlist ?? [],
    shell_allow_decomposition: !!raw.shell_allow_decomposition,
    mcp_allowlist: raw.mcp_allowlist ?? [],
    custom_step_allowlist: raw.custom_step_allowlist ?? [],
    max_execution_time_s: raw.max_execution_time_s ?? 30,
    max_output_size_mb: raw.max_output_size_mb ?? 10,
    workspace_bounds: raw.workspace_bounds ?? null,
    obstacles: raw.obstacles ?? [],
    velocity_limit: raw.velocity_limit ?? null,
    joint_limits: raw.joint_limits ?? {},
    torque_limit: raw.torque_limit ?? null,
  };
}

export interface Invariant {
  type: string;
  target: string | null;
  scope: string | null;
  description: string;
  expr: any | null;
  enforce: boolean;
}

export function decodeInvariant(raw: any): Invariant {
  return {
    type: raw.type,
    target: raw.target ?? null,
    scope: raw.scope ?? null,
    description: raw.description ?? "",
    expr: raw.expr ?? null,
    enforce: raw.enforce ?? true,
  };
}

export interface Postcondition {
  type: string;
  path: string | null;
  expected: number | null;
  min: number | null;
  max: number | null;
  description: string | null;
  expr: any | null;
  enforce: boolean;
}

export function decodePostcondition(raw: any): Postcondition {
  return {
    type: raw.type,
    path: raw.path ?? null,
    expected: raw.expected ?? null,
    min: raw.min ?? null,
    max: raw.max ?? null,
    description: raw.description ?? null,
    expr: raw.expr ?? null,
    enforce: raw.enforce ?? true,
  };
}

export interface Envelope {
  id: string;
  generated_by: string;
  task: string;
  permissions: Permission;
  invariants: Invariant[];
  postconditions: Postcondition[];
  parent_envelope: string | null;
  tightening_only: boolean;
  summary: string | null;
  cache_key: string | null;
  stakes: Stakes;
  shell_interpreter_policy: ShellInterpreterPolicy;
}

export function decodeEnvelope(raw: any): Envelope {
  return {
    id: raw.id ?? "env_case",
    generated_by: raw.generated_by ?? "",
    task: raw.task ?? "",
    permissions: decodePermission(raw.permissions),
    invariants: (raw.invariants ?? []).map(decodeInvariant),
    postconditions: (raw.postconditions ?? []).map(decodePostcondition),
    parent_envelope: raw.parent_envelope ?? null,
    tightening_only: raw.tightening_only ?? true,
    summary: raw.summary ?? null,
    cache_key: raw.cache_key ?? null,
    stakes: raw.stakes ?? "low",
    shell_interpreter_policy: raw.shell_interpreter_policy ?? "surface",
  };
}

// --- Steps ------------------------------------------------------------------

export interface StepBaseFields {
  id: string;
  depends_on: string[];
  metadata: Record<string, any>;
  postcondition: Postcondition | null;
  preferred_model: string | null;
}

export interface ShellStep extends StepBaseFields {
  type: "shell";
  command: string;
}
export interface FileReadStep extends StepBaseFields {
  type: "file_read";
  path: string;
}
export interface FileWriteStep extends StepBaseFields {
  type: "file_write";
  path: string;
  content: string;
}
export interface NetworkStep extends StepBaseFields {
  type: "network";
  url: string;
  method: "GET";
  headers: Record<string, string>;
}
export interface JointMoveStep extends StepBaseFields {
  type: "joint_move";
  joint_targets: Record<string, number>;
  duration_s: number;
  velocity_scale: number;
}
export interface CartesianMoveStep extends StepBaseFields {
  type: "cartesian_move";
  target_position: [number, number, number];
  target_orientation: [number, number, number, number] | null;
  duration_s: number;
  velocity_scale: number;
}
export interface GripperStep extends StepBaseFields {
  type: "gripper";
  action: "open" | "close";
  hold_s: number;
}
export interface SimulationResetStep extends StepBaseFields {
  type: "sim_reset";
  seed: number | null;
}
export interface VLAStep extends StepBaseFields {
  type: "vla";
  task: string;
  target_pose: [number, number, number] | null;
  max_actions: number;
  timeout_s: number;
}
export interface TaskStep extends StepBaseFields {
  type: "task";
  prompt: string;
}
export interface AgenticStep extends StepBaseFields {
  type: "agentic";
  prompt: string;
  workspace: string;
  tools: string[];
  max_turns: number | null;
}
export interface SkillStep extends StepBaseFields {
  type: "skill";
  skill_id: string;
  skill_input: Record<string, any>;
  contract_envelope: Envelope | null;
}
export interface MCPStep extends StepBaseFields {
  type: "mcp";
  server: string;
  tool: string;
  arguments: Record<string, any>;
}
// Unknown/custom @step_type: preserved with its raw type string and all
// extra fields, per the plan's "tolerant decoder" instruction.
export interface UnknownStep extends StepBaseFields {
  type: string;
  raw: Record<string, any>;
}

export type ActionStep =
  | ShellStep
  | FileReadStep
  | FileWriteStep
  | NetworkStep
  | JointMoveStep
  | CartesianMoveStep
  | GripperStep
  | SimulationResetStep
  | VLAStep
  | TaskStep
  | AgenticStep
  | SkillStep
  | MCPStep
  | UnknownStep;

const KNOWN_STEP_TYPES = new Set([
  "shell",
  "file_read",
  "file_write",
  "network",
  "joint_move",
  "cartesian_move",
  "gripper",
  "sim_reset",
  "vla",
  "task",
  "agentic",
  "skill",
  "mcp",
]);

function base(raw: any): StepBaseFields {
  return {
    id: raw.id,
    depends_on: raw.depends_on ?? [],
    metadata: raw.metadata ?? {},
    postcondition: raw.postcondition ? decodePostcondition(raw.postcondition) : null,
    preferred_model: raw.preferred_model ?? null,
  };
}

export function decodeStep(raw: any): ActionStep {
  const b = base(raw);
  switch (raw.type) {
    case "shell":
      return { ...b, type: "shell", command: raw.command };
    case "file_read":
      return { ...b, type: "file_read", path: raw.path };
    case "file_write":
      return { ...b, type: "file_write", path: raw.path, content: raw.content ?? "" };
    case "network":
      return {
        ...b,
        type: "network",
        url: raw.url,
        method: raw.method ?? "GET",
        headers: raw.headers ?? {},
      };
    case "joint_move":
      return {
        ...b,
        type: "joint_move",
        joint_targets: raw.joint_targets ?? {},
        duration_s: raw.duration_s ?? 1.0,
        velocity_scale: raw.velocity_scale ?? 1.0,
      };
    case "cartesian_move":
      return {
        ...b,
        type: "cartesian_move",
        target_position: raw.target_position,
        target_orientation: raw.target_orientation ?? null,
        duration_s: raw.duration_s ?? 1.0,
        velocity_scale: raw.velocity_scale ?? 1.0,
      };
    case "gripper":
      return { ...b, type: "gripper", action: raw.action, hold_s: raw.hold_s ?? 0.2 };
    case "sim_reset":
      return { ...b, type: "sim_reset", seed: raw.seed ?? null };
    case "vla":
      return {
        ...b,
        type: "vla",
        task: raw.task,
        target_pose: raw.target_pose ?? null,
        max_actions: raw.max_actions ?? 50,
        timeout_s: raw.timeout_s ?? 5.0,
      };
    case "task":
      return { ...b, type: "task", prompt: raw.prompt };
    case "agentic":
      return {
        ...b,
        type: "agentic",
        prompt: raw.prompt,
        workspace: raw.workspace,
        tools: raw.tools ?? [],
        max_turns: raw.max_turns ?? null,
      };
    case "skill":
      return {
        ...b,
        type: "skill",
        skill_id: raw.skill_id,
        skill_input: raw.skill_input ?? {},
        contract_envelope: raw.contract_envelope ? decodeEnvelope(raw.contract_envelope) : null,
      };
    case "mcp":
      return {
        ...b,
        type: "mcp",
        server: raw.server,
        tool: raw.tool,
        arguments: raw.arguments ?? {},
      };
    default:
      return { ...b, type: raw.type, raw };
  }
}

export function isKnownStepType(t: string): boolean {
  return KNOWN_STEP_TYPES.has(t);
}

// Explicit type-guard functions rather than relying on `step.type === "…"`
// narrowing: UnknownStep's `type: string` structurally overlaps every known
// literal, which defeats TypeScript's normal discriminated-union narrowing.
// A user-defined type predicate overrides that and narrows correctly.
export function isShellStep(s: ActionStep): s is ShellStep {
  return s.type === "shell";
}
export function isFileReadStep(s: ActionStep): s is FileReadStep {
  return s.type === "file_read";
}
export function isFileWriteStep(s: ActionStep): s is FileWriteStep {
  return s.type === "file_write";
}
export function isNetworkStep(s: ActionStep): s is NetworkStep {
  return s.type === "network";
}
export function isJointMoveStep(s: ActionStep): s is JointMoveStep {
  return s.type === "joint_move";
}
export function isCartesianMoveStep(s: ActionStep): s is CartesianMoveStep {
  return s.type === "cartesian_move";
}
export function isGripperStep(s: ActionStep): s is GripperStep {
  return s.type === "gripper";
}
export function isSimResetStep(s: ActionStep): s is SimulationResetStep {
  return s.type === "sim_reset";
}
export function isVLAStep(s: ActionStep): s is VLAStep {
  return s.type === "vla";
}
export function isTaskStep(s: ActionStep): s is TaskStep {
  return s.type === "task";
}
export function isAgenticStep(s: ActionStep): s is AgenticStep {
  return s.type === "agentic";
}
export function isSkillStep(s: ActionStep): s is SkillStep {
  return s.type === "skill";
}
export function isMCPStep(s: ActionStep): s is MCPStep {
  return s.type === "mcp";
}

export interface ActionPlan {
  id: string;
  source: string;
  task: string;
  steps: ActionStep[];
}

export function decodePlan(raw: any): ActionPlan {
  return {
    id: raw.id ?? "plan_case",
    source: raw.source ?? "",
    task: raw.task ?? "",
    steps: (raw.steps ?? []).map(decodeStep),
  };
}

export interface Violation {
  stage: string;
  message: string;
  detail: Record<string, any>;
  suggested_remediation?: string | null;
}

export function violation(stage: string, message: string, detail: Record<string, any> = {}): Violation {
  return { stage, message, detail };
}

export interface VerificationResult {
  ok: boolean;
  violations: Violation[];
  warnings: string[];
}

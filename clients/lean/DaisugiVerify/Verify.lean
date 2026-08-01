/-
Verify-Core orchestration: delegation-safety -> permissions -> dag, per the
kickoff plan's task 5 scope (predicate/z3/skill-subsumption are OUT of
scope — see clients/lean/README.md; any verify case whose oracle verdict
turns on one of those stages is an expected, honestly-reported mismatch).
-/
import DaisugiVerify.Models
import DaisugiVerify.Dag
import DaisugiVerify.Semantics
import DaisugiVerify.InterpreterParse
import DaisugiVerify.ShellDecompose

namespace DaisugiVerify

def knownStepTypes : List String :=
  ["shell", "network", "file_read", "file_write", "mcp", "task", "skill", "agentic",
   "joint_move", "cartesian_move", "gripper", "sim_reset", "vla"]

def agenticToolCapability : String → Option String
  | "Bash" => some "shell"
  | "Read" => some "file_read"
  | "Glob" => some "file_read"
  | "Grep" => some "file_read"
  | "Write" => some "file_write"
  | "Edit" => some "file_write"
  | "MultiEdit" => some "file_write"
  | "WebFetch" => some "network"
  | "WebSearch" => some "network"
  | _ => none

/-- Port of `_check_delegation_safety`: physical-stakes envelopes refuse
any agentic step, and any step with `preferred_model` set. -/
def checkDelegationSafety (steps : List Step) (stakes : String) : List Violation :=
  if stakes != "physical" then []
  else
    steps.flatMap fun s =>
      if s.type == "agentic" then [({ stage := "permissions", step := some s.id } : Violation)]
      else match s.preferredModel with
        | some _ => [({ stage := "permissions", step := some s.id } : Violation)]
        | none => []

/-- Redirect-target scope check (ADR-0014): a decomposed command's literal
redirect targets face the envelope's file scopes, exactly like a
FileWriteStep/FileReadStep would — except the sanctioned sinks/sources. -/
def sanctionedWriteSinks : List String := ["/dev/null", "/dev/stdout", "/dev/stderr"]
def sanctionedReadSources : List String := ["/dev/null", "/dev/stdin"]

def checkRedirectScopes (stepId : String) (decomp : Decomposition) (perms : Permission) :
    List Violation :=
  let writeViol := decomp.writes.toList.filter (fun p => !sanctionedWriteSinks.contains p) |>.filter
    (fun p => !pathMatchesAny p perms.fileWrite) |>.map fun _ => ({ stage := "permissions", step := some stepId } : Violation)
  let readViol := decomp.reads.toList.filter (fun p => !sanctionedReadSources.contains p) |>.filter
    (fun p => !pathMatchesAny p perms.fileRead) |>.map fun _ => ({ stage := "permissions", step := some stepId } : Violation)
  writeViol ++ readViol

mutual

/-- Port of `_check_shell_command`: metachar gate on the RAW command; if
hit and `shell_allow_decomposition` is on, decompose (our subset parser)
and check redirect scopes + every simple command (via `decomp.commands`,
one entry per `decomp.heads`, in order); if not on (or decomposition
rejects), it's a bare metachar violation. If no metachar, the
single-command path. -/
partial def checkShellCommand (stepId command : String) (perms : Permission) (policy : String)
    (depth : Nat) : List Violation :=
  if depth > 4 then [{ stage := "permissions", step := some stepId }]
  else
    let stripped := pyStrip command
    if stripped.isEmpty then []
    else if hasMetachar command then
      if perms.shellAllowDecomposition && depth <= 4 then
        let decomp := decomposeCommand command
        if decomp.ok then
          checkRedirectScopes stepId decomp perms ++
            decomp.commands.toList.flatMap fun simple =>
              checkSimpleCommand stepId simple perms policy depth
        else [{ stage := "permissions", step := some stepId }]
      else [{ stage := "permissions", step := some stepId }]
    else checkSimpleCommand stepId stripped perms policy depth

partial def checkSimpleCommand (stepId command : String) (perms : Permission) (policy : String)
    (depth : Nat) : List Violation :=
  match extractHead (pyStrip command) with
  | none => []
  | some head =>
    if !headAllowed head perms.shellAllowlist then
      [{ stage := "permissions", step := some stepId }]
    else
      match parseInterpreter command with
      | none => []
      | some payload =>
        if payload.isOpaque then
          if policy == "strict" then [{ stage := "permissions", step := some stepId }] else []
        else
          payload.innerCommands.flatMap fun inner =>
            checkShellCommand stepId inner perms policy (depth + 1)

end

/-- Minimal URL scheme extraction (`urlparse(url).scheme`): the prefix
before the first `:`, if it looks like a scheme token
(`[A-Za-z][A-Za-z0-9+.-]*`); else empty (matches urlparse treating a
malformed/missing scheme as `""`). -/
def urlScheme (url : String) : String :=
  let cs := url.toList
  match cs.findIdx? (· == ':') with
  | none => ""
  | some 0 => ""
  | some i =>
    let pfx := cs.take i
    let isSchemeChar (c : Char) := c.isAlpha || c.isDigit || c == '+' || c == '-' || c == '.'
    if pfx.head!.isAlpha && pfx.all isSchemeChar then
      String.ofList (pfx.map Char.toLower)
    else ""

/-- Minimal URL hostname extraction (`urlparse(url).hostname`): the
authority after `scheme://`, stripped of userinfo (`user:pass@`) and port
(`:port`), lowercased. IPv6 bracket literals are not specially handled
(not exercised by any real envelope in the corpus). -/
def urlHostname (url : String) : String :=
  match url.splitOn "://" with
  | _ :: rest :: _ =>
    let authority := ((rest.splitOn "/").headD "").splitOn "?" |>.headD "" |>.splitOn "#" |>.headD ""
    let afterUser := (authority.splitOn "@").getLast!
    let host := (afterUser.splitOn ":").headD afterUser
    String.ofList (host.toList.map Char.toLower)
  | _ => ""

def checkAgenticStep (s : Step) (perms : Permission) : List Violation :=
  if s.tools.isEmpty then [{ stage := "permissions", step := some s.id }]
  else
    let workspaceViol :=
      match s.workspace with
      | none => [({ stage := "permissions", step := some s.id } : Violation)]
      | some ws => if pathMatchesAny ws perms.fileRead then [] else [{ stage := "permissions", step := some s.id }]
    let toolViol := s.tools.flatMap fun tool =>
      match agenticToolCapability tool with
      | none => [({ stage := "permissions", step := some s.id } : Violation)]
      | some "shell" => if perms.shell then [] else [{ stage := "permissions", step := some s.id }]
      | some "file_read" => if perms.fileRead.isEmpty then [{ stage := "permissions", step := some s.id }] else []
      | some "file_write" => if perms.fileWrite.isEmpty then [{ stage := "permissions", step := some s.id }] else []
      | some "network" => if perms.network then [] else [{ stage := "permissions", step := some s.id }]
      | some _ => [({ stage := "permissions", step := some s.id } : Violation)]
    workspaceViol ++ toolViol

/-- Port of `check_permissions`: per-step dispatch by type. Accumulates —
never dedupes — matching the oracle's discipline (multiple bad heads in
one compound command are multiple identical `(stage, step)` pairs). -/
def checkPermissions (steps : List Step) (perms : Permission) (shellPolicy : String)
    (strict : Bool) : List Violation :=
  steps.flatMap fun s =>
    match s.type with
    | "shell" =>
      if !perms.shell then [{ stage := "permissions", step := some s.id }]
      else
        match s.command with
        | none => []
        | some cmd => checkShellCommand s.id cmd perms shellPolicy 0
    | "network" =>
      if !perms.network then [{ stage := "permissions", step := some s.id }]
      else
        match s.url with
        | none => []
        | some url =>
          let scheme := urlScheme url
          if scheme != "http" && scheme != "https" then
            [({ stage := "permissions", step := some s.id } : Violation)]
          else if !perms.networkHosts.isEmpty then
            let host := urlHostname url
            if perms.networkHosts.map (·.toLower) |>.contains host then []
            else [{ stage := "permissions", step := some s.id }]
          else []
    | "file_read" =>
      match s.path with
      | none => []
      | some p => if pathMatchesAny p perms.fileRead then [] else [{ stage := "permissions", step := some s.id }]
    | "file_write" =>
      match s.path with
      | none => []
      | some p => if pathMatchesAny p perms.fileWrite then [] else [{ stage := "permissions", step := some s.id }]
    | "mcp" =>
      match s.server, s.tool with
      | some server, some tool =>
        let key := server ++ "/" ++ tool
        if headAllowed key perms.mcpAllowlist then [] else [{ stage := "permissions", step := some s.id }]
      | _, _ => []
    | "agentic" => checkAgenticStep s perms
    | ty =>
      if strict && !knownStepTypes.contains ty && !perms.customStepAllowlist.contains ty then
        [{ stage := "permissions", step := some s.id }]
      else []

structure VerifyVerdict where
  ok : Bool
  violations : List Violation

/-- Verify-Core pipeline: delegation-safety -> permissions -> dag, each
short-circuiting the next on any violation. -/
def runVerify (plan : ActionPlan) (envelope : Envelope) (strictOpt : Option Bool) : VerifyVerdict :=
  let strict := resolveStrict strictOpt envelope.stakes
  let v1 := checkDelegationSafety plan.steps envelope.stakes
  if !v1.isEmpty then { ok := false, violations := v1 }
  else
    let v2 := checkPermissions plan.steps envelope.permissions
      envelope.shellInterpreterPolicy strict
    if !v2.isEmpty then { ok := false, violations := v2 }
    else
      let v3 := checkDag plan.steps
      if !v3.isEmpty then { ok := false, violations := v3 } else { ok := true, violations := [] }

end DaisugiVerify

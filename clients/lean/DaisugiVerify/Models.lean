/-
JSON decode of the wire-protocol case shape into lightweight structures.
Core-profile scope (per PORTING-NOTES.md / the kickoff plan): only the
fields `Verify.lean`'s permissions + delegation-safety + dag stages need.
Predicate/z3/skill-subsumption fields (invariants, postconditions,
contract_envelope, robotics permission fields) are intentionally not
decoded — those stages are out of scope and any verify case whose verdict
turns on them is an expected mismatch (see clients/lean/README.md).

Unknown step types keep their raw `type` string and decode with whatever
common fields are present; the permission stage treats anything outside
the known-type set generically (see `Verify.lean`).
-/
import Lean.Data.Json

namespace DaisugiVerify

open Lean (Json)

/-- `j.f` as a string, defaulting to `""` if absent/wrong-shaped. -/
def jGetStr (j : Json) (f : String) (default : String := "") : String :=
  ((j.getObjValD f).getStr?).toOption.getD default

def jGetStrOpt (j : Json) (f : String) : Option String :=
  match j.getObjValD f with
  | Json.null => none
  | v => v.getStr?.toOption

def jGetBool (j : Json) (f : String) (default : Bool := false) : Bool :=
  ((j.getObjValD f).getBool?).toOption.getD default

def jGetBoolOpt (j : Json) (f : String) : Option Bool :=
  match j.getObjValD f with
  | Json.null => none
  | v => v.getBool?.toOption

/-- `j.f` as a string array, defaulting to `[]`. Non-string elements are
skipped rather than failing the whole case. -/
def jGetStrList (j : Json) (f : String) : List String :=
  ((j.getObjValD f).getArr?.toOption.getD #[]).toList.filterMap (·.getStr?.toOption)

def jGetArr (j : Json) (f : String) : Array Json :=
  (j.getObjValD f).getArr?.toOption.getD #[]

structure Step where
  id : String
  type : String
  dependsOn : List String := []
  preferredModel : Option String := none
  -- type-specific (all optional; only the field(s) a given `type` uses
  -- are populated by the oracle, the rest are simply absent):
  command : Option String := none        -- shell
  path : Option String := none           -- file_read / file_write
  url : Option String := none            -- network
  server : Option String := none         -- mcp
  tool : Option String := none           -- mcp
  workspace : Option String := none      -- agentic
  tools : List String := []              -- agentic
  deriving Repr

def decodeStep (j : Json) : Step :=
  { id := jGetStr j "id"
    type := jGetStr j "type"
    dependsOn := jGetStrList j "depends_on"
    preferredModel := jGetStrOpt j "preferred_model"
    command := jGetStrOpt j "command"
    path := jGetStrOpt j "path"
    url := jGetStrOpt j "url"
    server := jGetStrOpt j "server"
    tool := jGetStrOpt j "tool"
    workspace := jGetStrOpt j "workspace"
    tools := jGetStrList j "tools" }

structure ActionPlan where
  id : String
  steps : List Step
  deriving Repr

def decodePlan (j : Json) : ActionPlan :=
  { id := jGetStr j "id", steps := (jGetArr j "steps").toList.map decodeStep }

structure Permission where
  fileRead : List String := []
  fileWrite : List String := []
  network : Bool := false
  networkHosts : List String := []
  shell : Bool := false
  shellAllowlist : List String := []
  shellAllowDecomposition : Bool := false
  mcpAllowlist : List String := []
  customStepAllowlist : List String := []
  deriving Repr

def decodePermission (j : Json) : Permission :=
  { fileRead := jGetStrList j "file_read"
    fileWrite := jGetStrList j "file_write"
    network := jGetBool j "network"
    networkHosts := jGetStrList j "network_hosts"
    shell := jGetBool j "shell"
    shellAllowlist := jGetStrList j "shell_allowlist"
    shellAllowDecomposition := jGetBool j "shell_allow_decomposition"
    mcpAllowlist := jGetStrList j "mcp_allowlist"
    customStepAllowlist := jGetStrList j "custom_step_allowlist" }

structure Envelope where
  id : String
  stakes : String := "low"
  shellInterpreterPolicy : String := "surface"
  permissions : Permission := {}
  deriving Repr

def decodeEnvelope (j : Json) : Envelope :=
  { id := jGetStr j "id"
    stakes := jGetStr j "stakes" "low"
    shellInterpreterPolicy := jGetStr j "shell_interpreter_policy" "surface"
    permissions := decodePermission (j.getObjValD "permissions") }

end DaisugiVerify

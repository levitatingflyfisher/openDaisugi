/-
The conformance wire protocol (docs/spec/conformance.md): one case JSON per
line on stdin, one verdict JSON per line on stdout, flushed per line,
order-independent (matched by `id`). A malformed/unprocessable case yields
an `{"id", "error"}` verdict and the stream continues — this module's
`handleLine` never throws; every path returns a verdict string.
-/
import Lean.Data.Json
import DaisugiVerify.Models
import DaisugiVerify.Verify
import DaisugiVerify.ShellDecompose

namespace DaisugiVerify

open Lean (Json)

/-- Highest wire-format version this client speaks (docs/spec/conformance.md
`CONFORMANCE_VERSION`). A case declaring a higher `v` is rejected via an
`error` verdict, never silently mis-decoded. -/
def wireVersion : Nat := 1

def errorVerdict (id : Option String) (msg : String) : Json :=
  Json.mkObj [("id", match id with | some s => Json.str s | none => Json.null), ("error", Json.str msg)]

def violationJson (v : Violation) : Json :=
  Json.mkObj [("stage", Json.str v.stage),
              ("step", match v.step with | some s => Json.str s | none => Json.null)]

def verifyVerdictJson (id : String) (verdict : VerifyVerdict) : Json :=
  Json.mkObj [("id", Json.str id), ("ok", Json.bool verdict.ok),
              ("violations", Json.arr (verdict.violations.map violationJson).toArray)]

def decomposeVerdictJson (id : String) (d : Decomposition) : Json :=
  if d.ok then
    Json.mkObj [("id", Json.str id), ("ok", Json.bool true),
                ("heads", Json.arr (d.heads.map Json.str)),
                ("commands", Json.arr (d.commands.map Json.str)),
                ("reads", Json.arr (d.reads.qsortOrd.map Json.str)),
                ("writes", Json.arr (d.writes.qsortOrd.map Json.str))]
  else
    Json.mkObj [("id", Json.str id), ("ok", Json.bool false)]

/-- Handles one case line. Never throws — every failure path (bad JSON,
missing `id`, unknown `kind`, higher `v` than we speak) is an `error`
verdict string, matching the spec's "the stream continues" requirement. -/
def handleLine (line : String) : String :=
  match Json.parse line with
  | .error e => (errorVerdict none s!"json parse error: {e}").compress
  | .ok j =>
    let idOpt := (j.getObjValD "id").getStr?.toOption
    match idOpt with
    | none => (errorVerdict none "missing case id").compress
    | some id =>
      let v := ((j.getObjValD "v").getNat?).toOption.getD 1
      if v > wireVersion then (errorVerdict (some id) s!"unsupported v={v}").compress
      else
        match (j.getObjValD "kind").getStr?.toOption with
        | some "verify" =>
          let plan := decodePlan (j.getObjValD "plan")
          let envelope := decodeEnvelope (j.getObjValD "envelope")
          let opts := j.getObjValD "options"
          let strictOpt := jGetBoolOpt opts "strict"
          let verdict := runVerify plan envelope strictOpt
          (verifyVerdictJson id verdict).compress
        | some "decompose" =>
          let command := jGetStr j "command"
          let d := decomposeCommand command
          (decomposeVerdictJson id d).compress
        | some other => (errorVerdict (some id) s!"unknown kind {other}").compress
        | none => (errorVerdict (some id) "missing kind").compress

end DaisugiVerify

/-
Fixture-driven regression test: replays `clients/fixtures/semantics.json`
(134 oracle-generated cases) against the Semantics.lean port. Not part of
the `conform` wire client — a dev-time check (task 3's "#eval-driven test
executable or plain IO test run by lake").

Usage: `lake exe test_semantics [path-to-semantics.json]`
(defaults to `../fixtures/semantics.json`, i.e. `clients/fixtures/...` when
run from `clients/lean/`).
-/
import Lean.Data.Json
import DaisugiVerify.Semantics
import DaisugiVerify.InterpreterParse

open Lean (Json)
open DaisugiVerify

structure Tally where
  pass : Nat := 0
  fail : Nat := 0

def Tally.record (t : Tally) (ok : Bool) (label : String) : IO Tally := do
  if ok then pure { t with pass := t.pass + 1 }
  else
    IO.eprintln s!"FAIL {label}"
    pure { t with fail := t.fail + 1 }

def jstr (j : Json) : String := j.getStr?.toOption.getD "<non-str>"
def jboolD (j : Json) (d : Bool) : Bool := j.getBool?.toOption.getD d
def jarr (j : Json) : Array Json := j.getArr?.toOption.getD #[]
def jfield (j : Json) (k : String) : Json := j.getObjValD k

/-- `null` is `Json.null`; string fields decode to `some`, else `none`. -/
def jstrOpt (j : Json) : Option String :=
  match j with
  | Json.null => none
  | Json.str s => some s
  | _ => none

def strListOf (j : Json) : List String :=
  (jarr j).toList.map jstr

def checkExtractHead (t : Tally) (c : Json) : IO Tally := do
  let line := jstr (jfield c "line")
  let expected := jstrOpt (jfield c "head")
  -- The oracle fixture stores the RAW line but computed `head` from
  -- `_extract_shell_head(l.strip())` — callers always strip first.
  let got := extractHead (pyStrip line)
  t.record (got == expected) s!"extract_head {line.quote}: got {repr got}, want {repr expected}"

def checkHeadAllowed (t : Tally) (c : Json) : IO Tally := do
  let head := jstr (jfield c "head")
  let allowlist := strListOf (jfield c "allowlist")
  let expected := jboolD (jfield c "allowed") false
  let got := headAllowed head allowlist
  t.record (got == expected) s!"head_allowed {head.quote} in {allowlist}: got {got}, want {expected}"

def checkPathMatch (t : Tally) (c : Json) : IO Tally := do
  let path := jstr (jfield c "path")
  let globs := strListOf (jfield c "globs")
  let expected := jboolD (jfield c "matched") false
  let got := pathMatchesAny path globs
  t.record (got == expected) s!"path_match {path.quote} against {globs}: got {got}, want {expected}"

def checkMetachar (t : Tally) (c : Json) : IO Tally := do
  let cmd := jstr (jfield c "command")
  let expected := jboolD (jfield c "hit") false
  let got := hasMetachar cmd
  t.record (got == expected) s!"metachar {cmd.quote}: got {got}, want {expected}"

def checkInterpreter (t : Tally) (c : Json) : IO Tally := do
  let cmd := jstr (jfield c "command")
  let payloadJ := jfield c "payload"
  let expected : Option InterpreterPayload :=
    match payloadJ with
    | Json.null => none
    | _ =>
      some { head := jstr (jfield payloadJ "head")
             innerCommands := strListOf (jfield payloadJ "inner_commands")
             isOpaque := jboolD (jfield payloadJ "opaque") false }
  let got := parseInterpreter cmd
  t.record (got == expected) s!"interpreter {cmd.quote}: got {repr got}, want {repr expected}"

def checkResolveStrict (t : Tally) (c : Json) : IO Tally := do
  let stakes := jstr (jfield c "stakes")
  let strict : Option Bool :=
    match jfield c "strict" with
    | Json.bool b => some b
    | _ => none
  let expected := jboolD (jfield c "effective") false
  let got := resolveStrict strict stakes
  t.record (got == expected) s!"resolve_strict strict={repr strict} stakes={stakes}: got {got}, want {expected}"

def main (args : List String) : IO UInt32 := do
  let path := args.headD "../fixtures/semantics.json"
  let contents ← IO.FS.readFile path
  match Json.parse contents with
  | .error e =>
    IO.eprintln s!"JSON parse error: {e}"
    return 1
  | .ok j =>
    let mut t : Tally := {}
    for c in jarr (jfield j "extract_head") do
      t ← checkExtractHead t c
    for c in jarr (jfield j "head_allowed") do
      t ← checkHeadAllowed t c
    for c in jarr (jfield j "path_match") do
      t ← checkPathMatch t c
    for c in jarr (jfield j "metachar") do
      t ← checkMetachar t c
    for c in jarr (jfield j "interpreter") do
      t ← checkInterpreter t c
    for c in jarr (jfield j "resolve_strict") do
      t ← checkResolveStrict t c
    IO.println s!"semantics fixture: {t.pass} passed, {t.fail} failed"
    return (if t.fail == 0 then 0 else 1)

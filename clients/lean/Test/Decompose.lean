/-
Fixture-driven regression test for the decompose subset parser
(`ShellDecompose.lean`). Replays `clients/fixtures/decompose.json` (synthesized
commands with oracle-truth expectations — no real filesystem paths, so unlike
the conformance corpus it is committed) against `decomposeCommand`.

Two expectation kinds per case:
  * "match"  — expect ok=true and the exact oracle head list (order-sensitive)
               plus the same read/write sets (order-independent).
  * "reject" — a deliberate subset scope cut: `decomposeCommand` MUST return
               ok=false even where the oracle accepts (fail-closed safety).

Usage: `lake exe test_decompose [path-to-decompose.json]`
(defaults to `../fixtures/decompose.json`, i.e. `clients/fixtures/...`).
-/
import Lean.Data.Json
import DaisugiVerify.ShellDecompose

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

def strArrayOf (j : Json) : Array String := (jarr j).map jstr

/-- Order-independent set equality for read/write targets. -/
def setEq (a b : Array String) : Bool :=
  a.size == b.size && a.all (b.contains ·) && b.all (a.contains ·)

def checkCase (t : Tally) (c : Json) : IO Tally := do
  let label := jstr (jfield c "label")
  let command := jstr (jfield c "command")
  let kind := jstr (jfield c "kind")
  let exp := jfield c "expected"
  let expOk := jboolD (jfield exp "ok") false
  let got := decomposeCommand command
  if kind == "reject" then
    return ← t.record (got.ok == false)
      s!"{label}: expected reject (ok=false), got ok={got.ok} heads={got.heads}"
  -- kind == "match"
  if !expOk then
    return ← t.record (got.ok == false) s!"{label}: expected ok=false, got ok={got.ok}"
  let expHeads := strArrayOf (jfield exp "heads")
  let expReads := strArrayOf (jfield exp "reads")
  let expWrites := strArrayOf (jfield exp "writes")
  let ok := got.ok && got.heads == expHeads && setEq got.reads expReads && setEq got.writes expWrites
  t.record ok
    s!"{label}: cmd={repr command}\n  want ok=true heads={expHeads} reads={expReads} writes={expWrites}\n  got  ok={got.ok} heads={got.heads} reads={got.reads} writes={got.writes}"

def main (args : List String) : IO UInt32 := do
  let path := args.headD "../fixtures/decompose.json"
  let contents ← IO.FS.readFile path
  match Json.parse contents with
  | .error e =>
    IO.eprintln s!"JSON parse error: {e}"
    return 1
  | .ok j =>
    let mut t : Tally := {}
    for c in jarr j do
      t ← checkCase t c
    IO.println s!"decompose fixture: {t.pass} passed, {t.fail} failed"
    return (if t.fail == 0 then 0 else 1)

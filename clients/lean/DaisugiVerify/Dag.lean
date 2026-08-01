/-
Port of dag.py's `check_dag`: duplicate step ids -> missing deps -> cycle,
each tier short-circuiting the next (a graph with duplicate ids or a
dangling dependency is structurally meaningless to search for cycles in).
-/
import DaisugiVerify.Models

namespace DaisugiVerify

structure Violation where
  stage : String
  step : Option String := none
  deriving Repr, BEq

/-- Distinct step ids that appear more than once, each reported exactly
once (mirrors the oracle: a triply-duplicated id is still one violation,
appended the moment its SECOND occurrence is seen). -/
def duplicateIds (steps : List Step) : List String :=
  let rec go (ss : List Step) (seen : List String) (dupes : List String) : List String :=
    match ss with
    | [] => dupes
    | s :: rest =>
      if seen.contains s.id then
        go rest seen (if dupes.contains s.id then dupes else dupes ++ [s.id])
      else go rest (seen ++ [s.id]) dupes
  go steps [] []

/-- One violation per (step, missing dependency) pair, in step order —
this is deliberately NOT deduplicated, matching the oracle's
accumulate-don't-dedupe discipline (a step with two dangling deps is two
violations with the same `step`). -/
def missingDepViolations (steps : List Step) : List Violation :=
  let ids := steps.map (·.id)
  steps.flatMap fun s =>
    s.dependsOn.filter (fun d => !ids.contains d) |>.map fun _ =>
      ({ stage := "dag", step := some s.id } : Violation)

-- DFS cycle detection via 3-coloring (unvisited / in-progress / done)
-- over the `dep -> step` edge relation, walked as `step -> its own deps`
-- (a graph and its edge-reversal have a cycle iff the other does). Only
-- WHETHER a cycle exists matters — the oracle's cycle violation carries
-- `step = null`, so the path itself is never compared.
mutual
partial def dagVisit (byId : List (String × Step)) (id : String) (visiting : List String)
    (done : List String) : Bool × List String :=
  if done.contains id then (false, done)
  else if visiting.contains id then (true, done)  -- back-edge: cycle
  else
    match byId.find? (fun (k, _) => k == id) with
    | none => (false, id :: done)  -- dangling dep already excluded upstream
    | some (_, s) =>
      let (cyc, done') := dagWalk byId s.dependsOn (id :: visiting) done
      (cyc, if cyc then done' else id :: done')

partial def dagWalk (byId : List (String × Step)) (deps : List String) (visiting : List String)
    (done : List String) : Bool × List String :=
  match deps with
  | [] => (false, done)
  | d :: rest =>
    let (cyc, done') := dagVisit byId d visiting done
    if cyc then (true, done') else dagWalk byId rest visiting done'
end

def hasCycle (steps : List Step) : Bool :=
  let byId := steps.map (fun s => (s.id, s))
  let rec scan (ids : List String) (done : List String) : Bool :=
    match ids with
    | [] => false
    | id :: rest =>
      let (cyc, done') := dagVisit byId id [] done
      if cyc then true else scan rest done'
  scan (steps.map (·.id)) []

def checkDag (steps : List Step) : List Violation :=
  let dupes := duplicateIds steps
  if !dupes.isEmpty then dupes.map fun d => { stage := "dag", step := some d }
  else
    let missing := missingDepViolations steps
    if !missing.isEmpty then missing
    else if hasCycle steps then [{ stage := "dag", step := none }]
    else []

end DaisugiVerify

# ADR-0008 — Parameterized, callable, composable pathways

**Status:** Accepted (design). Implementation in progress (Phase B).

## Context

Today a distilled `CompiledPathway` is a frozen `plan_template` that the
orchestrator's `_maybe_reuse` serves **verbatim** (a deep copy — it never calls
`adapt_plan`). That has two problems:

1. **Store fragmentation.** Because reuse only fires on a ≥0.55 near-identical
   match and runs the stored plan as-is, "find TODO", "find FIXME", and "find
   XXX" each need their *own* pathway. A family of the same shape becomes N
   near-duplicate rows.
2. **A latent lie.** `distiller._GENERALIZE_SYSTEM` tells the model to emit
   concrete values because "a later `adapt_plan()` step rewrites these" — but the
   orchestrator never runs that step.

We want **one** pathway to serve a whole family, **without** a full-plan LLM
re-derivation, and **without** weakening the guard.

## Decision

A distilled pathway is a **typed, envelope-guarded callable** in one of three
tiers, chosen by **diffing the cluster** of successful traces it came from:

| Tier | When (per cluster) | Reuse cost | Behaviour |
|---|---|---|---|
| **Frozen script** | no leaf field varies across members | **0 tokens** | serve the plan verbatim (today's behaviour) |
| **Typed skill** | same structure, some leaf fields vary | **cheap bind** | a template with typed *parameter holes*; bind args → re-verify → run |
| **Not compiled** | structure diverges across members | — | leave it to a fresh decompose |

**Parameter discovery is a diff, not a guess — and the distiller does it.** The
distiller already loads every cluster member's *concrete* plan in
`_distill_cluster` (the `train_records`); that is where the diff runs. (The
Gardener only holds `source_trace_ids`, not plans, so it cannot diff — it stays
lifecycle: prune / merge.) **Promotion is emergent via re-tend:** each `tend`
re-distills a cluster that gained new traces, re-runs the diff, and updates the
pathway's parameters — so a pathway that was frozen becomes typed the first tend
after a new member makes a field vary. You cannot infer a hole from identical
runs, so we never invent one.

**The same-shape precondition (what makes a varying field a legitimate hole).**
`plan_structure_signature` is step-*types* only — `shell→shell` matches both
`grep TODO` and `git status`. That is too weak to call a differing field a hole.
Before the diff treats a field as a parameter it requires the **fixed portion to
match**: same step types **and** the same *capability head* — the shell argv-head
/ command name, the file path-prefix, the URL host. Only a field that varies
*while the capability head is identical* (`grep TODO` vs `grep FIXME` → the
pattern arg) becomes a typed hole. If the capability head differs across members,
the cluster is **refused for parameterization** (kept frozen, or not compiled) —
never collapsed into a `{command}` hole. This is what makes "data-slots-only"
enforceable rather than aspirational.

**Binding** (typed tier, at reuse): fill each hole from the new task —
**deterministically** when the value is trivially extractable, else with **one
small, schema-constrained LLM call** that returns only the typed arguments
(validated by Pydantic). Then produce the concrete plan and run it through the
**exact same fail-closed verify** reuse already applies (allowlist + the
shell-interpreter-payload re-check + envelope). A bad bind fails verify and falls
through to fresh decompose.

**The safety rule that makes this sound:** parameters are **typed *data* slots
inside a fixed capability** (a grep pattern; a path that is glob-checked), *never*
a free `{command}`/`{tool}` slot — a free capability slot is not a parameter, it
is re-planning, and it is rejected. Because `ShellStep` runs `shell=True`, the
bound concrete command is re-parsed by the existing interpreter-payload check, so
an injected `; rm -rf` in a bound value is caught exactly as it is for a fresh
plan.

**Composition:** a pathway may invoke another as a `SkillStep` (the type already
exists), forming a call graph — "some free steps + some LLM calls composed
together." This stays safe by the **existing `envelope_subsumes`**: a composed
skill's envelope must be ⊆ the caller's, so it can only ever do what the caller
already permits.

### Forks (locked)

- **Materialize internal-first.** Callables are invoked in-process via `SkillStep`;
  export to MCP tool / CLI / `SKILL.md` is a later phase (Phase C), designed for
  but not built now.
- **Bind deterministic-else-tiny-LLM** (above), so the free tier stays free.
- **The distiller computes parameters** (frozen→typed) by diffing the cluster's
  concrete plans at distill time; promotion is emergent via re-tend. The Gardener
  stays lifecycle (prune / merge) — it holds trace IDs, not plans, so it cannot
  diff.

## Consequences

- New data: `CompiledPathway` gains an optional `parameters` schema (list of typed
  holes with a JSON path into the plan). Absent = a frozen pathway (back-compat:
  existing rows load as frozen).
- The reuse path gains a **bind** step before its existing verify; frozen pathways
  skip it (unchanged, still 0-token).
- The distiller's `adapt_plan` promise is **superseded**: the typed bind *is* the
  adaptation, but constrained to typed data and always re-verified — not a
  free-form plan rewrite. The misleading prompt line is removed.
- The **yellow paper** will later formalize two invariants this introduces: (a)
  *bind-then-verify* (no bound plan runs unverified), and (b) *data-slots-only*
  (a hole may never widen a capability). Deferred until B4–B5 exist.

## Invariants preserved (VISION §invariants)

Fail-closed (a bad bind → rejected → fresh decompose); verify-before-execute (the
bound concrete plan is verified before any effect); envelope-as-ceiling (bind is
checked against the *caller's* envelope; composition by subsumption); verify
actions, not understanding (binding changes *data*, never the proof obligation).

## Build order

B1 spec (this) → B3 the cluster diff → parameters on `CompiledPathway`
(same-shape-gated; frozen when no varying data-hole) → B4 typed bind + verify →
B5 composition. B2 (per-harness ingest parser) rides in parallel — more, varied
traces are what make the diff find real holes.

## Post-implementation safety amendments (review outcomes)

An adversarial review after B3–B5 landed two safety changes to the spec above:

1. **Shell is excluded from typed parameterization.** A shell capability head pins
   only `argv[0]`, and `verify` does not glob-check a command's *file operands*
   against the envelope's `file_read`/`file_write`. A search pattern cannot be told
   from a bare filename in a shell string, so a typed `grep <p> /dev/null` could be
   bound to `grep <p> /etc/shadow` and pass verify. Typed binding is therefore
   limited to fields whose head pins **location** — a file path's directory, a URL's
   host. Shell pathways stay **frozen** (still 0-token reuse). The "grep for X"
   example in this ADR is illustrative of the *concept*; the shipped typed tier uses
   `file_read`/`file_write`/`network`.

2. **Composition (B5) is a mechanism, not auto-wired.** `compose.py` provides
   `pathway_skill_handler` / `pathway_skill_handlers_for` / `pathway_contract_envelopes`,
   and they are tested, but the orchestrator does **not** auto-expose pathways as
   skills. Safe auto-exposure requires (a) stamping each pathway-referencing
   `SkillStep`'s `contract_envelope` so the existing subsumption proof fires, and
   (b) routing composed sub-steps through the Supervisor's per-step verify rather
   than raw executors. Until both exist, auto-wiring would be a latent gap (and is
   unreachable anyway — the decomposer isn't told pathway ids). Scoped follow-up.

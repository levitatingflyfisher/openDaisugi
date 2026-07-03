# openDaisugi — Yellow Paper

*A formal specification of the verification semantics: the envelope algebra,
subsumption, and the fail-closed guarantees.*

**Register.** "Yellow paper" is the crypto/protocol convention for a rigorous
formal specification (after Wood's *Ethereum Yellow Paper*). This document plays
that role for openDaisugi's verification core. It is precise about *intent and
current behavior*; it is **not** a machine-checked proof. Where a property is
tested-and-intended rather than mechanically verified, it says so. The code it
describes was authored by an AI assistant — treat it as the current implementation
to be checked against this spec, not as an oracle. A machine-checked proof of the
checker itself (Coq/Lean) is explicitly out of scope and remains aspirational.

For the intuition, read [architecture/OVERVIEW.md](../architecture/OVERVIEW.md)
first; for *why*, the [ADRs](../adr/).

---

## 1. Notation

We write `⊑` for "is subsumed by / is at most as permissive as", `⊨` for "admits /
satisfies", `⊥` for the reject/deny outcome. `𝒮` is the (infinite) space of concrete
*steps* an execution could attempt. Globs and hostnames are treated as sets of the
concrete strings they match. A predicate over steps is a total function
`𝒮 → {⊤, ⊥}`. All relations are decided **fail-closed**: when a decision procedure
returns `unknown`, times out, or encounters an unsupported construct, the result is
`⊥` (deny), never `⊤`.

## 2. Objects

### 2.1 Permission

A `Permission` `P` is a tuple of capability grants over disjoint capability
*dimensions*:

```
P = ⟨ file_read, file_write : set of glob,
      network : bool, network_hosts : set of host,
      shell : bool, shell_allowlist : set of command-head,
      mcp_allowlist : set of tool-pattern,
      workspace_bounds : AABB ∪ {⊥}, velocity/torque : ℝ⁺ ∪ {⊥},
      joint_limits : map ∪ {}, obstacles : set of AABB,
      max_execution_time_s, max_output_size_mb : ℕ ⟩
```

Each dimension `d` induces an **admitted-set** `⟦P⟧_d ⊆ 𝒮`: the concrete steps on
that dimension the permission allows (e.g. `⟦P⟧_shell` = shell steps whose command
head ∈ `shell_allowlist`, if `shell`; else ∅). The permission's admitted set is
`⟦P⟧ = ⋃_d ⟦P⟧_d`, augmented by the robotics dimensions as arithmetic constraints on
trajectory steps (§5.3).

### 2.2 Envelope

An `Envelope` `E = ⟨P, I, Q, stakes, parent⟩` where `P` is a Permission, `I` a set of
*invariants* (predicates that must hold over all steps), `Q` a set of
*postconditions* (predicates over completed-step evidence), `stakes ∈
{low, medium, high, physical}`, and `parent` an optional parent envelope. `strict`
is a derived mode, on by default for `stakes ∈ {high, physical}` (§6).

An envelope's admitted set is the permission's set *intersected* with the invariants:

```
⟦E⟧ = { s ∈ ⟦P⟧ : ∀ φ ∈ I . φ(s) = ⊤ }
```

### 2.3 ActionPlan

An `ActionPlan` `π = ⟨V, D⟩` is a set of steps `V ⊆ 𝒮` with a dependency relation
`D ⊆ V×V`. `π` is well-formed iff `(V, D)` is a DAG with unique step ids and every
`(u,v) ∈ D` has `u,v ∈ V` (§4.5).

## 3. Admissibility

A plan `π` is **admitted** by `E` iff every step is in the envelope's admitted set:

```
E ⊨ π   ⟺   ∀ s ∈ V(π) . s ∈ ⟦E⟧
```

`verify(π, E)` (§4) is the *decision procedure* for `E ⊨ π`. The central soundness
obligation (§6) is one-directional and fail-closed:

> **(Soundness, intended.)** `verify(π, E) = ok ⟹ E ⊨ π`.
> Equivalently: if any step of `π` lies outside `⟦E⟧`, `verify` returns `⊥`.

The converse (completeness — that every admitted plan verifies) is **not** claimed:
`verify` may reject an admitted plan it cannot *prove* admitted (e.g. an unsupported
glob). That asymmetry is deliberate — see §6.

## 4. The verification pipeline

`verify(π, E, strict)` is a short-circuiting conjunction of stage predicates. It
returns `ok` iff **all** stages pass; the first violating stage yields `⊥` with a
diagnostic. Stages are ordered cheap→expensive; SMT runs only after set/string
checks pass.

### 4.1 Stage 0.5 — delegation safety
Reject a plan that delegates a physical-stakes action to a probabilistic/contained
leaf that carries no verification story. (Guards the boundary between deterministic
and stochastic execution.)

### 4.2 Stage 1 — permissions
For each step `s` and its capability dimension `d`, require `s ∈ ⟦P⟧_d`. Set/glob/
scheme membership; no SMT. **Default rule (fail-closed):** a step whose type has no
permission surface and no handler (an unknown custom `@step_type`) is *rejected*
under `strict`, never waved through.

### 4.3 Stage 1b — skill-delegation subsumption
Each `SkillStep` carries a contract envelope `E_c`; require `envelope_subsumes(E,
E_c)` (§5). A skill may run only within authority the caller already holds.

### 4.4 Stage 2 — Z3 (self-consistency + plan-vs-envelope)
Two SMT queries: (i) `E` is internally satisfiable (its invariants aren't mutually
contradictory / vacuous — see §6 vacuity); (ii) no step of `π` can violate `E`'s
predicate constraints. Encoded via §5.

### 4.5 Stage 3 — DAG
`(V,D)` has unique ids, all dependencies resolve, and is acyclic. Duplicate ids are
rejected (they would collapse graph nodes and defeat the receipt-integrity check).

Post-execution, **Stage 2b** re-checks each completed step's evidence against `Q`
(postconditions), and a **run-integrity** predicate requires that the set of
executed steps (in topological order) is exactly covered by receipts — a silently
skipped step falsifies it.

## 5. Subsumption (delegation safety)

`envelope_subsumes(E_out, E_in)` decides `E_in ⊑ E_out`: *inner admits no more than
outer*.

```
E_in ⊑ E_out   ⟺   ⟦E_in⟧ ⊆ ⟦E_out⟧
```

This is the one relation behind delegation, inheritance (a generated child must be a
*tightening*: `E_child ⊑ E_parent`), and pathway reuse (a reused plan is bounded by
the caller's envelope, never its own). It is decided dimension-wise, all fail-closed:

### 5.1 Set dimensions (network hosts, mcp, shell heads)
Exact set-subset: `⟦E_in⟧_d ⊆ ⟦E_out⟧_d`. For hosts this is literal set-subset.

### 5.2 Glob dimensions (file_read, file_write)
`E_in`'s globs must be contained in `E_out`'s glob *language*. Decided by an
existential SMT search for a witness `w` matched by an inner glob but no outer glob;
`unsat` (no witness) ⟹ contained. An inner glob whose form is unsupported by the
glob→SMT translation ⟹ `⊥` (deny). MCP scope uses the same construction.

### 5.3 Robotics dimensions
`workspace_bounds` (inner AABB ⊆ outer AABB), `velocity`/`torque` (inner ≤ outer),
`joint_limits` (inner ⊆ outer), `obstacles` (inner ⊇ outer — inner must forbid at
least what outer forbids). **An undeclared bound where outer constrains it ⟹ deny**
(you cannot delegate into an unbounded region).

### 5.4 Predicate / soft dimensions
Invariants with a supported predicate expression are compiled to SMT and checked by
polarity (§6). An invariant that compiles to a *soft* node (unsupported regex,
free-text `LLMCheck`) present in `E_out` but not `E_in` is treated as an
unverifiable outer constraint the inner doesn't share ⟹ `⊑` fails (fail-closed).

> **(Delegation safety, intended.)** If `envelope_subsumes(E_out, E_in) = holds` and
> `verify(π, E_in) = ok`, then `verify(π, E_out)` would also hold — running `π` under
> a delegation bounded by `E_in` cannot exceed `E_out`.

## 6. Strict mode, vacuity, and the fail-closed law

`strict` (default-on at high/physical stakes) closes the gaps where an *unenforceable*
constraint could masquerade as enforcement:

- **Opaque invariants.** An invariant with no verifiable expression, and no
  registered handler, is rejected under strict rather than assumed satisfied.
- **Vacuity.** A predicate that is tautological (always ⊤) or contradictory is
  caught before the solver: a "safety" invariant that is vacuously true enforces
  nothing. A recognized robotics invariant declared *without its backing bound*
  (e.g. `end_effector_in_workspace` with `workspace_bounds = ⊥`) is rejected as
  vacuous — its handler would no-op.
- **Soft-node polarity.** A soft (unverifiable) node must be handled so that it can
  never *weaken* a constraint under negation. In particular an outer deny-rule that
  degrades to a soft node is failed closed, not silently dropped.

**The fail-closed law (governing all of the above).** For every decision procedure
`δ` in this spec:

```
δ returns unknown / times out / hits an unsupported construct   ⟹   δ ≔ ⊥
```

No stage may return `ok` on the basis of *absence of a found counterexample by an
incomplete method*. "Not disproven" is not "proven".

## 7. What is guaranteed — and what is not

**Guaranteed (by construction + test):**
- Soundness-for-what-it-checks (§3): a plan that `verify`s `ok` has every step inside
  the envelope's admitted set *on the dimensions the spec covers*.
- Fail-closed on every incomplete/unknown result (§6).
- Delegation is transitive containment (§5), so skills-as-contracts, safe
  sub-agents, inheritance, and pathway reuse all reduce to one checked relation.

**Not guaranteed / out of scope:**
- **The checker is not machine-verified.** These properties are the design intent,
  enforced by ~1600 tests, not a Coq/Lean proof. The translation layer
  (glob→SMT, regex→SMT) is itself trusted code and a place bugs can hide; a
  soundness bug there is a *silent* fail-open, which is why it is the most
  safety-critical surface and the target of ongoing adversarial review.
- **Completeness is not claimed** (§3): admissible plans may be rejected.
- **Actions, not understanding.** `⟦E⟧` bounds what a step *does*, never whether the
  model *understood the task*. An envelope proves the arm stayed under 5N; it cannot
  prove the model meant the fork and not the knife. Every guarantee here is
  additionally conditional on the honesty of the evidence a step reports and on the
  envelope's `parent`/provenance being independent of the plan's author (§VISION
  invariant 4).
- **Physical-stakes caveats.** Swarm deconfliction is analytic AABB geometry, not a
  flight-safety certificate: waypoint-in-box ≠ path-in-box, and disjoint boxes ≠
  collision-free unless margins ≥ vehicle radius + position uncertainty.

---

*This specification describes the current implementation as authored by an AI
assistant. Discrepancies between this document and the code are bugs in one or the
other — verify against the tests before relying on any stated property.*

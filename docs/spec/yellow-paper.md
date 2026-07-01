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
- **Call-time gating is safety-only, and narrower than "benign."** The
  call-time gate (§8) soundly enforces that every executed action is in `⟦E⟧`,
  but cannot establish plan-structure or liveness properties (no plan exists to
  range over), cannot enforce information-flow (a hyperproperty), and does not
  make an in-envelope *trajectory* benign — individually-admitted calls compose
  into harm (§8.3). Its guarantee is additionally conditioned on the host
  invoking it and on a working deny path (§8.5).
- **Physical-stakes caveats.** Swarm deconfliction is analytic AABB geometry, not a
  flight-safety certificate: waypoint-in-box ≠ path-in-box, and disjoint boxes ≠
  collision-free unless margins ≥ vehicle radius + position uncertainty.

## 8. Two checkpoints: plan-time verification and call-time gating

Everything above concerns **plan-time** verification: a declared plan `π` is
handed to `verify(π, E)` before any step runs. A second checkpoint operates
where no `π` exists — an agent already running inside a host harness, emitting
tool calls one at a time. The **call-time gate** (ADR-0007) intercepts each
call, synthesizes it into a one-step plan `⟨aᵢ⟩`, and evaluates
`verify(⟨aᵢ⟩, E) ` before the call executes, preventing `aᵢ` when
`aᵢ ∉ ⟦E⟧`. The two checkpoints share the decision procedure of §4; they do
**not** share what they can guarantee, and conflating them would over-claim.

### 8.1 The gate as an execution monitor

Model the running agent as emitting a trace `a₁ a₂ a₃ …`. The gate is an
**execution monitor** in the sense of Schneider [Sch00]: it observes the trace
step by step and prevents any action that would violate the policy. The policy
it enforces is

```
P(a₁ … aₙ)  ≝  ∀ i ≤ n .  aᵢ ∈ ⟦E⟧
```

`P` is **prefix-closed** — if a trace satisfies it, so does every prefix, and a
single out-of-envelope action falsifies it irrecoverably. A prefix-closed trace
property is a **safety property** (Alpern–Schneider [AS85]), and safety
properties are exactly the class an execution monitor can soundly enforce
[Sch00]. So the gate's guarantee is real and not weaker than its mechanism:
*every executed action lies in `⟦E⟧`.* Whether the target **halts** after a
denial or **continues** with that one action suppressed is a host-contract
detail (on the Claude Code path the call is blocked and the agent continues);
the per-action guarantee holds either way.

### 8.2 What plan-time has that call-time structurally cannot (class limit)

Two properties are provable at plan time and **not** at call time — the first
by construction of this codebase, the second by the enforceability class.

- **No cross-step structure at call time.** A call-time evaluation has no `π`:
  the gate builds a *singleton* plan per call. Exactly the checks that quantify
  over multiple steps therefore have nothing to range over and are not run —
  DAG ordering (§4.5), the predicate invariants `exists_step` / `forall_steps`
  (§4.4, §2b), and postconditions spanning more than one step. This is not a
  gap to be closed later; it is what "one call at a time, no plan" means, and
  the implementation says so (`verify_step` deliberately omits the plan-level
  stages). Ordering, completeness, and "the plan as a whole establishes X" are
  **plan-time properties**.
- **Liveness and hyperproperties are outside the monitor class.** An execution
  monitor cannot enforce a **liveness** property — "eventually returns to
  base," "the task is eventually completed" — because no finite prefix
  witnesses its violation [AS85]. Nor can it enforce a **hyperproperty**:
  information-flow / non-interference is 2-safety over *pairs* of traces
  [CS10], not a property of the single trace in front of the monitor, so no
  per-call gate enforces "this run leaked nothing." These are limits of the
  mechanism, independent of `E`.

### 8.3 In-envelope ≠ trajectory-benign (policy-adequacy gap)

Distinct from §8.2, and the easier one to mistake for a bug: `⟦E⟧` is a
predicate on **individual actions**. The gate soundly enforces
`∀ i . aᵢ ∈ ⟦E⟧`. That predicate does **not** entail "the trajectory is
benign." Concretely, let `E` grant `file_read : /work/**` and
`network → api.github.com`. Then

```
a₁ = read  /work/secret           a₁ ∈ ⟦E⟧
a₂ = POST  api.github.com  ⟨secret⟩ a₂ ∈ ⟦E⟧
```

— each call is inside the envelope, the composition is exfiltration. Nothing is
broken: this is **not** an enforcement failure but the difference between
*per-action authorization* and a *trajectory specification*. A gate cannot make
`⟦E⟧` say more than it says; tightening the envelope (drop the network grant,
or bound the host set) is the only remedy, and it is the envelope author's, not
the gate's. Stated plainly for the scorecard: **a call being inside the
envelope does not make the trajectory benign.**

### 8.4 Threat model and boundaries

- **Assumed hostile:** the model's outputs and hence every tool-call argument;
  instructions injected into the model's context (prompt injection); a
  sub-agent that attempts to widen its own authority.
- **Assumed trusted (out of scope):** the verifier and its translation layer
  (§7 — a soundness bug there is a silent fail-open); the envelope's provenance
  being independent of the plan's author (VISION invariant 4 — a self-authored
  envelope authorizes nothing meaningfully); and the host faithfully invoking
  the gate.
- **Mediation boundary.** The gate sees only what the host routes through the
  **hooked tool surface**. An action taken over a channel the host does not
  hook is unmediated — outside the gate's view and therefore outside its
  guarantee. Enforcement is per-harness and per-version (roadmap Stage 5).

### 8.5 The conditioned guarantee, and the fail-open edges

The safety guarantee of §8.1 holds **conditioned on** all of: (i) the host
invokes the gate on *every* tool call; (ii) the host's deny path actually
blocks; (iii) the action traverses a hooked surface (§8.4). Where a condition
can fail outside our control, it is named here, not buried:

- **Host outer hook timeout** — fails open on every harness measured. Mitigated,
  not eliminated: the gate owns an *inner* timeout that denies first (§6's
  fail-closed law applied to the clock).
- **A harness that silently stops firing hooks** — condition (i) fails with no
  signal; only a per-version contract test detects it (Stage 5).
- **Gate-process death / import failure** — would make condition (ii) fail
  (a non-deny exit is non-blocking on the host). Closed at the process
  boundary: the emitted hook command maps every non-deny exit to a deny.

### 8.6 Relation to Simplex / RTA

The two-checkpoint design descends from Simplex runtime assurance [Sha01]:
a trusted safety layer vetoing an untrusted controller. The lineage is
**inspirational, and the call-time guarantee is strictly weaker.** Simplex
guarantees the system remains in a *recoverable safe state over time* — a
trajectory-level, liveness-flavored property delivered by a safety controller
that can *act*. The call-time gate only **prevents** individual actions; it
provides no recoverable-state guarantee over the trajectory (indeed §8.2 says
it cannot). A reader who knows Simplex should not import its temporal guarantee
here.

### References

Standard citations, given for provenance; verify wording and venue against the
sources (this document is AI-authored — the grain-of-salt law applies).

- **[Sch00]** F. B. Schneider. *Enforceable Security Policies.* ACM TISSEC 3(1), 2000. (Execution monitors enforce safety properties.)
- **[AS85]** B. Alpern, F. B. Schneider. *Defining Liveness.* Information Processing Letters 21(4), 1985. (safety / liveness decomposition.)
- **[CS10]** M. R. Clarkson, F. B. Schneider. *Hyperproperties.* Journal of Computer Security 18(6), 2010. (information flow as 2-safety.)
- **[LBW05]** J. Ligatti, L. Bauer, D. Walker. *Edit automata: enforcement mechanisms for run-time security policies.* Int. J. Information Security 4(1–2), 2005. (suppression/insertion beyond truncation.)
- **[Sha01]** L. Sha. *Using Simplicity to Control Complexity.* IEEE Software 18(4), 2001. (the Simplex architecture.)

---

*This specification describes the current implementation as authored by an AI
assistant. Discrepancies between this document and the code are bugs in one or the
other — verify against the tests before relying on any stated property.*

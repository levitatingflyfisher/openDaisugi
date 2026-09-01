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
whole checker (Coq/Lean) remains aspirational. The Lean 4 client
(`clients/lean/DaisugiVerify/Theorems.lean`) proves a few properties of the Core
stages only (permissions, decomposition over a small token model, strict rejects at
least what lenient rejects). Nothing proves the SMT encoders or `U_D` (§2.4).

Sections marked **Proposed** (§2.4, parts of §5.5, and the dialect clauses of §6)
specify behavior the code does not have yet. They fix the law an implementation must
meet.

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

For dialects (§2.4, Proposed): `D` is a dialect; `w@h` is a word `w` with content
hash `h`; `≺_D` is the well-founded "defined earlier than" order on words; `U_D` is
the unfolding of a dialect term to a kernel term; `⟦t⟧_D` and `δ_D` are the meaning of
a dialect term and a decision procedure run on its unfolding. For delegation (§5.5):
`edge_ok(E_parent, E_child)` is the edge rule, and `E_child ⊑ E_parent` reads "the
child admits no more than the parent".

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

### 2.3.1 The write paths of a step (Built)

**Status.** Built 2026-09-30 in Python, Go and Rust (`src/opendaisugi/write_paths.py`,
`forall_writes` in `predicate.py`). Ruled DI-10; rulings DL-1 to DL-3 and DL-11 to
DL-13.

Each step `s` has a derived field `writes(s)`, a finite list of normalized paths, or
`?` (unknown). It is not stored in the step record; only the quantifier below reads it.

```
writes(s) = [ norm(s.path) ]                          if s is a file_write step
          = W(s.command, 0, no)                        if s is a shell step
          = [ ]                                        otherwise

W(c, k, a) = ?                                         if k > 4
           = [ ]                                       if strip(c) = ""
           = P(c, k, a)                                if c has no shell metacharacter
           = ?                                         if the shell grammar refuses c
           = ?                                         if c runs cd, pushd or popd and
                                                       the list below holds a relative path
           = [ norm(t) : t ∈ writes_redirect(c), t ∉ Sinks ] ++ P(c₁, k, a) ++ … ++ P(cₙ, k, a)
                                                       else, c₁ … cₙ its simple commands
P(c, k, a) = W(i₁, k+1, a') ++ … ++ W(iₘ, k+1, a')     i₁ … iₘ the payloads of a
                                                       command-taking wrapper (sh -c,
                                                       xargs, find -exec, env, …);
                                                       a' = a or the wrapper is xargs or find
           = O(c, a)                                   otherwise, an opaque interpreter
                                                       included
O(c, a)    = [ ]                                       if c's head is not a writer
           = ?                                         if a, or c holds a word the shell
                                                       changes, or a flag outside the
                                                       writer's closed set
           = [ norm(t) : t ∈ operands_written(c), t ∉ Sinks ]   otherwise
```

`norm` is `posixpath.normpath`, `writes_redirect` the literal write redirect targets
of the shell decomposition (the targets §4.2 checks against `file_write`), and
`Sinks = {/dev/null, /dev/stdout, /dev/stderr}`. Any part that is `?` makes the whole
`?`. The writers are `write_paths.WRITERS`: `cp`, `mv`, `ln`, `install`, `tee`, `sed`,
`truncate`, `dd`, `rsync`, `touch`, `mkdir`, `rm`, `rmdir`, `unlink` and `shred`, each
with its closed flag sets. `operands_written` follows each command's argument rules:
the destination of `cp`, `ln`, `install` and `rsync` (the last operand or the `-t`
directory) and `DEST/basename(SOURCE)` for each source, unless `-T`; `mv` adds its
sources; every operand of the others (`install -d` too); the files after the script of
`sed -i`, with each backup; the `of=` of `dd`; no remote `rsync` destination, and the
local sources under `--remove-source-files`. A recursive command shows its top path
only. An interpreter's work (`python`, `awk`, `sed` without `-i`, `sudo`), a command
that is not a writer, an MCP tool and a delegated agent write nothing here.

**Placed writes.** `writes_b(s)` resolves each relative path of `writes(s)` against a
base `b`, an absolute normalized directory: `norm(b + "/" + p)`. A path that starts
with `~` makes it `?`. Only a word's unfolding reads `writes_b` (§2.4); a hand-written
`forall_writes` reads `writes`.

**Quantifier.** `forall_writes(φ)` stands inside `forall_steps` or `exists_step`. On a
step `s`:

```
⟦forall_writes(φ)⟧(s) = ⊥                             if writes(s) = ?
                      = ∀ p ∈ writes(s) . ⟦φ⟧({path: p})   otherwise
```

At the plan root, inside `forall_outputs`, or inside another `forall_writes` it is an
evaluation error, a violation, never `⊤`. Z3: a concrete step unrolls into a
conjunction; a symbolic step (vacuity, subsumption) has one free write path `w` and a
free Boolean `n` ("the step writes"), and compiles to `n ⟹ φ(w)`. So the quantifier is
a tautology exactly when `φ` is, and it is never a contradiction.

**What `writes` does not see.** Files a command writes through its operands (`cp`,
`mv`, `tee`, `sed -i`, `touch`), the work of an interpreter (`python`, `sed`, `awk`),
and the writes of MCP tools, skills and delegated agents. A predicate over `writes`
says nothing about them; §4.2's permission checks still govern each of them as today.

### 2.4 Dialects (Proposed; stage 0 Built)

**Status.** Proposed ([design-dialects.md](../plans/2026-09-24-omarchy/design-dialects.md)).
What exists today is the definition store (`src/opendaisugi/aliases.py`): named,
parameterized predicates unfolded by `resolve()` before a check, and the **system
dialect** of stage 0 below. Permission words, population dialects, envelope-level pins,
the register-time order check and the size bound are **not built**. Today the gate
passes no definition registry, so an `alias` reference at the gate is an unresolved
definition: a violation (a deny in enforce mode; logged in audit mode).

**Stage 0 (Built 2026-09-30; rulings DL-1 to DL-17).** The system dialect
(`src/opendaisugi/dialect.py`, tables generated for Go and Rust) holds one predicate
word and its synonyms:

```
keep_unchanged(target : glob = "**")  :=  forall_steps(forall_writes(not_matches(path, R(target))))
```

`R` is a typed hole of sort glob: `R(g)` is a regex that matches exactly the normalized
paths the file-scope matcher of §5.2 matches with `g` (a `/**` suffix, `**` segments,
`*` and `?` inside a segment). A glob with `[` or `]`, a `/**` glob whose prefix is
`.`, or one over the size limits is refused, and a refused target is `⊥`. 23 invariant
type names that models write for the same meaning (`file_unchanged`, `read_only`,
`no_modifications`, `file_immutable`, `file_preservation`, …) are synonyms: each
unfolds to the same term. An **opaque** invariant (no `expr`) whose `type` names a word
is a use of it, with its `target` as the argument. The dialect's identity is the first
16 hex digits of the SHA-256 of the canonical JSON of its definitions, with the
versions of `R` and of the write paths.

At the gate a word is placed from the call's cwd `b` (DL-13): a relative target `g`
becomes `b/g`, and the unfolded `forall_writes` reads `writes_b` (§2.3.1). A target
that starts with `/` or `**` is left as it is, so the default `**` still names every
path. A cwd with a glob character, a target that starts with `~`, and a target with a
`.` or `..` segment that does not end in `/**` are refused. A plan verified outside the
gate has no base, and nothing is placed.

Audit first (DI-1). With no pin, a word use is evaluated and a failure is recorded as a
warning (`dialect audit: …`); the verdict is the one the opaque invariant gets today. A
pin equal to the dialect hash (`config.yaml` `dialect_enforce`) enforces the word: the
word decides the invariant. A pin that names another hash makes every word use `⊥`.
The pin lives in the operator's config, not in the envelope (DL-4), and a hard-deny
rule stops an agent's tool call from writing or naming that config (DL-15).

**Kernel.** Let `K` be the kernel: the `Permission` record (§2.1), the predicate
algebra without definition references, and the envelope and plan objects of §2.2 and §2.3.
`⟦·⟧` is the kernel semantics of §2 to §5.

**Dialect.** A dialect `D` is a finite list of definitions

```
w(x₁ : τ₁, …, xₙ : τₙ) := body_w
```

where:

- `w` is a name. It carries no meaning for any decision procedure.
- each `τᵢ` is a kernel sort: string literal, glob, host, command head, number, or
  regex. A parameter fills one whole field of the body (a *typed hole*). It is never
  spliced into the middle of another string.
- `body_w` is a kernel term that may use only words defined **earlier** in `D`. This
  order `≺_D` is well-founded, and it is checked when a word is registered.
- `w` has one **kind**: a *permission word* (its body is a partial `Permission` with
  set-valued fields only) or a *predicate word* (its body is an algebra expression,
  used in an invariant or a postcondition).

**Identity.** A word's identity is `w@h`, where

```
h = H( canon(signature_w, body_w), { id(v) : v used in body_w } )
```

for a fixed hash `H` over canonical JSON. A new definition is a new identity; a name
never changes meaning in place. An envelope that uses words pins the hash of its
dialect.

**Unfolding.** `U_D` maps a term over `K + D` to a term over `K`:

```
U_D(t)             = t                                    if t contains no word
U_D(w(a₁ … aₙ))    = U_D( body_w[x₁ := a₁, …, xₙ := aₙ] )
```

The substitution is simultaneous, typed and single-pass: each argument is a literal
placed into its hole, and is never read again as a template. For a permission word,
the result is merged into the envelope's `P` by **union** on each set field
(`file_read`, `file_write`, `network_hosts`, `shell_allowlist`, `mcp_allowlist`,
`custom_step_allowlist`); a word that lists hosts sets `network`, and a word that
lists shell heads sets `shell`. Scalar and bound fields (`max_execution_time_s`,
`max_output_size_mb`, `workspace_bounds`, velocity, torque, `joint_limits`,
`obstacles`) may **not** appear in a permission word, because union is not a sound
join for them.

**Bound.** Unfolding can grow exponentially with nesting. A dialect term is admitted
only if its nesting depth is at most `d_max` and `|U_D(t)| ≤ min(k·|t|, n_max)` nodes.
The design proposes `d_max = 4`, `k = 4`, `n_max = 2000`, to be tuned on real
dialects; these values are provisional.

**Semantics.** The meaning of a dialect term is **defined** as the meaning of its
unfolding, and every decision procedure runs on the unfolding:

```
⟦t⟧_D  ≝  ⟦U_D(t)⟧            δ_D(x)  ≝  δ(U_D(x))
```

for every `δ` in §4, §5, §6 and §8 (verify, the gate, subsumption, vacuity,
deconfliction).

**Intended properties** (with the same caveat as §7: tested and intended, not
machine-checked):

1. **Conservativity.** For a kernel term `t`, `U_D(t) = t`, so `⟦t⟧_D = ⟦t⟧`. Adding a
   dialect changes the meaning of no existing envelope. This is an extension by
   definitions, which is always conservative.
2. **Soundness is inherited.** `verify_D(π, E) = verify(π, U_D(E))`, and `U_D(E)` is
   the envelope enforced. So §3's `verify(π, E) = ok ⟹ E ⊨ π` gives
   `verify_D(π, E) = ok ⟹ U_D(E) ⊨ π`. Nothing new is proved about the checker; only
   `U_D` must be correct.
3. **Totality.** `U_D` terminates, because `≺_D` is well-founded and each step
   replaces a word by a body that uses only earlier words.
4. **Decidability is the kernel's, no better and no worse.** Unfolding adds no
   theory. A dialect question is decidable exactly where the same kernel question is:
   set and glob membership over finite allowlists, and linear real arithmetic (ranges,
   boxes). The kernel is **not** decidable as a whole as encoded: `matches` compiles to
   Z3 regular membership and `length_range` to string length. For strings with word
   equations, lengths and regular membership, decidability is known only for
   restricted fragments, and our encoding is not shown to lie in one. Z3 can therefore
   answer `unknown` on legal input, and by §6 that is `⊥`. Nonlinear robot dynamics
   would bring the same outcome.
5. **Cross-dialect relations.** Equivalence, subsumption and disjointness of words or
   envelopes from two dialects are defined on their unfoldings. Two words are equal iff
   `UNSAT(U(a) ≠ U(b))` with parameters matched by position and sort and left free.

**Grants.** A predicate word only narrows: an invariant removes steps from `⟦E⟧`
(§2.2). A permission word grants, but only through an envelope that uses it; importing
a dialect grants nothing.

**Journal.** Each verdict on a dialect envelope records the surface envelope, the
dialect hash and the hash of `U_D(E)`, so a replay checks the same kernel term.

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

### 5.5 Delegation edges (partly built, partly Proposed)

A parent that starts a child agent delegates authority to a second model. The edge
rule ([design-delegation-tree.md](../plans/2026-09-24-omarchy/design-delegation-tree.md)) is:

> **(Edge rule.)** A child with envelope `E_child` may start under a parent with
> envelope `E_parent` only if `edge_ok(E_parent, E_child)`. A failure, a timeout, or a
> check that cannot be made is `⊥`, and the child does not start.

`edge_ok` is subsumption made total over every field that grants authority:

```
edge_ok(E_parent, E_child)  ⟺  E_child declared
                              ∧ envelope_subsumes(E_parent, E_child, strict = ⊤) = holds
                              ∧ I_parent ⊆ I_child ∧ Q_parent ⊆ Q_child
                              ∧ policy_interp(E_child) ⪰ policy_interp(E_parent)
```

where `policy_interp(E)` is `E.shell_interpreter_policy` ordered
`allow ≺ surface ≺ strict`, and `envelope_subsumes` must itself cover the fields
below. Per-step caps and aggregate budgets differ. A per-step cap bounds each step
and is compared directly. An aggregate budget (tokens, turns, a deadline) must be
**split, not copied**: each child reserves its share from the parent's remaining
budget, or N children spend N times the parent's budget.

| Field that grants authority | Required relation | State |
|---|---|---|
| file globs, hosts, shell heads, MCP tools, compiled invariants, robot bounds | §5.1 to §5.4 | Built (Python; Go and Rust) |
| `stakes` | `stakes_child ≥ stakes_parent` in `low < medium < high < physical` (a lower stakes turns strict mode off) | Built in Python (`subsumption._budget_and_stakes_violation`); Go and Rust in progress |
| `custom_step_allowlist` | child ⊆ parent (strict runs a custom step only if listed) | Built in Python; Go and Rust in progress |
| per-step caps `max_execution_time_s`, `max_output_size_mb` | child ≤ parent | Built in Python; Go and Rust in progress |
| aggregate tokens, turns, deadline | child's reservation ≤ parent's remaining budget; child deadline ≤ parent's | Open. Provisional ruling: tokens and turns in a tree ledger outside the envelope, the deadline as an envelope field |
| Z3 `unknown` or timeout in the final query | `⊥` in every mode | Built in Python (`timed_out`); Go and Rust in progress |
| invariants and postconditions, by value | parent's ⊆ child's | Proposed at any depth; today only `verify_inheritance`, depth 1 |
| `shell_interpreter_policy` | child not looser than parent | Proposed; today read from the outer only |
| an opaque skill (no envelope) | `⊥` at every stakes level | Proposed for edges; `check_skill_delegations` still allows it with a warning in lenient mode |
| sequence invariants (`forall_steps`, `exists_step` over a plan) | not compared | Open; subsumption reasons over one symbolic step |

**Induction.** If every edge satisfies `edge_ok`, then by transitivity of `⊑` (§5)
every node satisfies `E_node ⊑ E_root`. There is no depth limit, and strict mode is
on at every edge whatever the stakes.

**Registration** (Proposed). Only the operator registers an envelope with no parent (a
root). Any other registration names its parent and runs `edge_ok`. The starter, not the
child, binds the child's session. A child session with no exact registered envelope is
`⊥`, never the `default` envelope. What is built today: a hook payload's session id
never selects an envelope; an unpinned gate checks every call against `default`, and a
gate pinned to a session with no envelope denies (commit `31871c4d`).

**What an edge does not give.** The proof bounds actions, not the quality of the
child's work. It does not see data flow (§8.3), collusion between siblings whose
envelopes each fit, or the joint motion of sibling robots. The tree never exceeds the
root; it promises no more than the root.

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

**Dialect clauses (Proposed, §2.4).** Before any procedure runs on a dialect term,
unfolding itself is fail-closed. Each of these is `⊥`:

- a word that is not in the pinned dialect (an unknown word);
- an envelope that uses words but pins no dialect, or pins a hash the checker cannot
  load, or a word whose hash does not match the pinned dialect;
- a cycle, or a definition that uses a word not earlier in `≺_D`;
- a missing, extra or ill-typed argument;
- a term over the depth or size bound;
- a word of the wrong kind in a field (a permission word in an invariant, or the
  reverse);
- a permission word whose body sets a scalar or bound field.

Once unfolded, the law above applies unchanged: an unfolding on which Z3 answers
`unknown` is `⊥`, exactly as the same kernel term written by hand. Built today: an
definition with no registry, an unknown definition, a cycle found at resolve time, and a missing
argument each deny; a register-time vacuity error refuses the definition
(`src/opendaisugi/aliases.py`, `src/opendaisugi/verify.py`). For the system dialect
of stage 0 (§2.4), built: a target glob outside the supported set, and an enforcement
pin that names a hash this build does not have, are each `⊥`; `writes(s) = ?` makes
`forall_writes` false (§2.3.1).

## 7. What is guaranteed — and what is not

**Guaranteed (by construction + test):**
- Soundness-for-what-it-checks (§3): a plan that `verify`s `ok` has every step inside
  the envelope's admitted set *on the dimensions the spec covers*.
- Fail-closed on every incomplete/unknown result (§6).
- Delegation is transitive containment (§5), so skills-as-contracts, safe
  sub-agents, inheritance, and pathway reuse all reduce to one checked relation.

**A law for the cost levers (Stages 8–10), stated now:**
- **Admissibility-preservation.** The cost levers — within-instance batch
  compilation, model routing, pathway reuse, and the deed / rationale ledgers
  ([ADR-0011](../adr/0011-verifiable-execution-substrate.md)) — change *which* model or
  script produces an action, never *what is admitted*. Each reused, batched, or routed
  action is re-verified against the **caller's** envelope (§5; VISION invariant 3), so
  no lever can widen `⟦E⟧`. The two ledgers are observational: they record deeds and
  rationale but never gate an action. The one lever that touches authority is
  *constraint promotion* — a mid-task invariant captured into the envelope — and it is
  admitted only through the monotone-narrowing subsumption of §5, so it may only
  tighten. A lever that cannot exhibit this reduction is a design regression, not an
  optimization. (The **deed ledger** is built and observes this law by construction: a
  reversal only ever writes to a path the run itself wrote — restoring a prior state or
  deleting a file the run created — so it cannot widen `⟦E⟧`, and it is valid only
  against the run's own ledger. Batch compilation (v0.42) and the rationale store
  with constraint promotion (v0.43) are built to the mechanism line; a batch proves
  each write inside both the envelope and its declared footprint before it runs. The
  learned router is not built; the law it must satisfy is fixed here so the
  implementation cannot quietly relax it.)

**Not guaranteed / out of scope:**
- **The checker is not machine-verified.** These properties are the design intent,
  enforced by tests and by differential agreement of three implementations (Python,
  Go, Rust) on shared cases, not by a Coq/Lean proof of the checker. The translation
  layer (glob→SMT, regex→SMT, and `U_D` once dialects exist) is itself trusted code and a place bugs can hide; a
  soundness bug there is a *silent* fail-open, which is why it is the most
  safety-critical surface and the target of ongoing adversarial review.
- **Completeness is not claimed** (§3): admissible plans may be rejected.
- **Decidability is not claimed for the whole kernel** (§2.4, property 4). String
  constraints with regular membership and length may give `unknown`, which denies.
- **Names carry no meaning** (§2.4). The checker proves a word's definition, never its
  name. A check of a name against its definition is advice to humans, not part of any
  proof. `keep_unchanged` is the example: its definition covers `file_write` steps,
  shell write redirects and the operand writes of the known writers only (§2.3.1),
  not a file changed by an interpreter, an unknown command or an MCP tool.
- **An edge proves containment, not quality** (§5.5).
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

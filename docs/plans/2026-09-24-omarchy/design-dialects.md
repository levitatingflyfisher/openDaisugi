# Design: Dialects, one kernel, many languages

Status: proposal for the owner, 2026-09-26. The idea is the owner's (strategy notes,
"Dialects"). A small part exists today as predicate definitions (`src/opendaisugi/aliases.py`).
**Stage 0 is Built (2026-09-30):** shell write paths reach the predicate algebra
(`forall_writes`), and the system dialect gives `keep_unchanged(target)` and 23 synonyms
a kernel definition, in audit until the operator pins the dialect (section 11). Everything
else here is **Proposed**. Items the owner decides are marked **Open**.

Sources: `strategy-2026-09-26.md` (sections "Dialects", "What weave was for", "Models writing
languages"), `docs/research/prior-art-2026-09-26.md`, `docs/research/formal-methods-2026-09-23.md`,
`docs/spec/yellow-paper.md`, the code named in each section, and a read-only look at the copied
journal in `~/opendaisugi-scratch/dialects/` (the first test's copy).

## 1. The idea in plain words

An envelope says what an agent may do. Today every envelope is written in one fixed language:
the `Permission` record (`src/opendaisugi/models.py`) and the predicate algebra
(`src/opendaisugi/predicate.py`). The same grammar serves a coding task and a drone.

A dialect is a vocabulary of named words for one domain. Each word has a definition in the
fixed language, the **kernel**. For example:

- `read_only_inspection` = shell heads `cat find grep head tail`, no writes.
- `must_not_modify(target)` = no `file_write` step on a path that matches `target`. (Shell
  writes such as `sed -i` are not visible to a predicate today; see section 3.4.)
- `code_forges` = network on, hosts `github.com gitlab.com f-droid.org`.

A model writes the envelope in dialect words. The checker never trusts a word. It replaces each
word by its definition (it **unfolds** the word) and checks the kernel result with the same
code as today. A human reads the words, because words are short and say what they mean.

This answers the early dream that "models are good at writing a DSL for one situation"
(whitepaper, runtime assurance pattern 4, "compiled policy / live DSL") without a person
learning a DSL:

- **Models write it.** The words come from the owner's own journal. A model proposes names and
  documentation. A model writes envelopes with them. (Owner ruling: the bots write the policy.)
- **Humans only read it.** A deny says "`must_not_modify(src/**)` blocked a write to
  `src/app.py`", not a regex. The kernel form is one click away in coppice.
- **The checker only sees the kernel.** A word adds no new logic. So a dialect cannot weaken a
  proof, however it was learned.

The situation-specific language is the vocabulary. The grammar and the proofs stay fixed.

## 2. Evidence, and what exists today

### 2.1 The first test

The strategy notes record a greedy compression pass over the permission sets of 294 real
envelopes (`~/opendaisugi-scratch/dialects/scan.py`). It cut 2,769 atoms to 2,284, word
definitions included: 17.5% with 15 words. This number is in-sample (words mined and scored on
the same envelopes), over flat sets, by a greedy pass. Section 5.5 defines the held-out test it
still has to pass.

### 2.2 A stronger signal: models already invent words, and nothing enforces them

A read-only count over the same copy (294 envelopes whose task text is longer than 12
characters, the filter `scan.py` uses) found:

| Fact | Count |
|---|---|
| Invariants in those envelopes | 322 |
| Invariants with an `expr` (a checkable predicate) | **0** |
| Most common invariant type: `file_unchanged` | 75 |
| Near-synonyms: `read_only` 7, `no_modifications` 5, `no_file_modification` 5, `read_only_operations` 3 | 20 |
| Envelope stakes: low / medium / high | 267 / 17 / 10 |

So the models already write a dialect. It has synonyms, and it has no definitions. For an
invariant with no `expr`, `_check_predicate_item` in `src/opendaisugi/verify.py` returns no
violation unless `strict` is on, and strict is on by default only at high and physical stakes
(yellow paper §6). At low and medium stakes these invariants are documentation, not policy. The
We found no handler keyed on `file_unchanged` or on `Invariant.target` in `src/` (grep). The
10 high-stakes traces with invariants were all denied at the permissions stage, which runs first,
so the copy shows no case of the strict opaque-invariant check firing. The
envelope prompt teaches this form: its first few-shot example is an opaque
`{"type": "file_unchanged", "target": ...}` (`src/opendaisugi/envelope.py`,
`ENVELOPE_SYSTEM_PROMPT`).

The first payoff of dialects is therefore not compression. It is to give `file_unchanged` and
its synonyms one kernel definition, so the invariant the model already writes is enforced.
The "must not modify <target>" word in the strategy notes came from this count of opaque
invariants, not from the compression pass.

### 2.3 Definitions: the definitional extension that already ships

`src/opendaisugi/aliases.py` (v0.9.0) already implements named, parameterized predicates:

- `Alias(name, params, expr, tier, description)`; `AliasRef(name, args)` in the algebra
  (`predicate.py`).
- Three tiers: `system` (shipped), `household`, `envelope`. Lookup takes the highest-precedence
  tier: envelope, then household, then system.
- `resolve()` unfolds recursively, detects cycles with a `seen` set, and checks that all
  parameters are given.
- `register()` rejects a word with no path reference and, since v0.27.0, a word that Z3 finds
  tautological or contradictory (`vacuity.py`).
- Seven system words ship in `src/opendaisugi/system_aliases.py`: `velocity_scale_bounded`,
  `never_impersonates`, `no_pii_regex`, `no_secrets`, `pytest_passes`, `no_network_writes`,
  `structured_approval`.
- `verify()` takes `aliases=`. An `AliasRef` with no registry is a violation, not a pass
  (`verify.py`, `_check_predicate_item`). The Go and Rust clients parse the `alias` op and fail
  closed on it as unresolved (`clients/go/internal/gate/predicate.go`,
  `clients/rust/src/gate/predicate.rs`).

So dialects grow out of aliases. They are not new machinery. Section 10 lists the gaps in
definitions that must close before a learned word reaches the gate.

## 3. Formal core

### 3.1 Definitions

Let **K** be the kernel: the `Permission` record, the predicate algebra without `AliasRef`, and
the envelope and plan objects of yellow paper §2. `⟦·⟧` is the kernel semantics (yellow paper
§2 to §5).

A **dialect** `D` is a finite list of definitions

```
w(x₁ : τ₁, …, xₙ : τₙ) := body_w
```

where:

- `w` is a name. It carries no meaning for the checker.
- each `τᵢ` is a kernel sort: string literal, glob, host, command head, number, or regex. A
  parameter fills one whole field of the body (a *typed hole*). It is never spliced into the
  middle of another string.
- `body_w` is a kernel term that may use words defined **earlier** in `D`. The "earlier than"
  order is well-founded, so there are no cycles. This is checked when a word is registered, not
  only when it is resolved.
- the word has a **kind**: a permission word (its body is a partial `Permission` with set-valued
  fields only), a predicate word (its body is an algebra expression, used in an invariant or a
  postcondition), or, later, a plan-fragment word (section 4.4).

A word's **identity** is `name@h`, where `h` is a hash of the canonical JSON of its signature
and body, and of the identities of the words it uses.

**Unfolding** `U_D` maps a term over `K + D` to a term over `K`:

- `U_D(t) = t` for a kernel term with no word in it.
- `U_D(w(a₁…aₙ)) = U_D(body_w[x₁ := a₁, …, xₙ := aₙ])`, one simultaneous substitution into
  typed holes. An argument value is a literal. It is never read again as a template.
- For a permission word, the result is merged into the envelope's `Permission` by **union** on
  each set field (`file_read`, `file_write`, `network_hosts`, `shell_allowlist`,
  `mcp_allowlist`, `custom_step_allowlist`). A word that lists hosts sets `network`; a word that
  lists shell heads sets `shell`.

Scalar and bound fields (`max_execution_time_s`, `workspace_bounds`, `velocity_limit`,
`torque_limit`, `joint_limits`, `obstacles`) are **not** allowed in permission words. The
record has one AABB and one velocity limit. Two words cannot both set them by union; the only
sound join would be a meet (intersection), and "using a word narrows the envelope" is a
surprising rule. Robot bounds therefore go in predicate words or stay plain fields (section 3.4).

### 3.2 Why every guarantee is kept

The semantics of a dialect term is **defined** as the semantics of its unfolding:

```
⟦t⟧_D  ≝  ⟦U_D(t)⟧
```

and every decision procedure `δ` (verify, gate, subsumption, vacuity, deconfliction) runs on the
unfolding:

```
δ_D(input)  ≝  δ(U_D(input))
```

Three facts follow at once.

1. **Conservative.** For a kernel term `t`, `U_D(t) = t`, so `⟦t⟧_D = ⟦t⟧`. Adding a dialect
   changes the meaning of no existing envelope.
2. **Soundness is inherited.** Yellow paper §3 states `verify(π, E) = ok ⟹ E ⊨ π`. With a
   dialect, `verify_D(π, E) = verify(π, U_D(E))`, and `U_D(E)` is the envelope that is
   enforced. Nothing new must be proved about the checker. Only `U_D` itself must be correct,
   and it is a small, total, syntactic function.
3. **Unfolding is total.** It terminates because the definition order is well-founded and each
   step replaces a word by a body that uses only earlier words.

This is the textbook notion of a definitional extension: every new symbol is an abbreviation
that can be eliminated. The value is not in the logic. It is in who writes the words (models,
from the journal) and who reads them (humans).

### 3.3 Decidability: what the kernel gives, stated honestly

The strategy notes call the kernel decidable. That is too strong. The correct claim is weaker
and still enough:

- **Unfolding adds no theory.** A question about dialect terms is exactly a question about
  kernel terms. It is no harder in kind.
- **Some kernel fragments are decidable; one is not known to be.** Set and glob membership over
  finite allowlists is decided by plain code (`verify.py`, Stage 1). Linear real arithmetic
  (numeric ranges, AABB checks) is decidable. But `predicate_z3.py` compiles `matches` to Z3
  regex membership (`z3.InRe`) and `length_range` to `z3.Length` over Z3 strings. For strings
  with word equations, length constraints and regular membership, decidability is known only for
  restricted fragments, such as equations in "solved form", and some quantified forms are
  undecidable ([arXiv 1306.6054](https://arxiv.org/abs/1306.6054)). Our encoding is not shown to
  lie in a decidable fragment. Z3 can therefore answer
  `unknown` on legal kernel input.
- **Unknown means deny.** The fail-closed law (yellow paper §6) already covers this. A dialect
  changes nothing here: a word whose unfolding times out denies, like the same kernel term
  written by hand.
- **Size can grow.** If word `a` uses word `b` twice, and `b` uses `c` twice, the unfolding
  doubles per level. A dialect needs a bound on nesting depth and on unfolded size. Over the
  bound, the envelope is rejected (fail closed). Recommendation: depth 4, and 4× the surface
  size or 2,000 nodes, whichever is smaller; to be tuned on real dialects.

### 3.4 What the kernel can and cannot say today

A word can only say what its unfolding can say. So each domain's dialect is limited by the
kernel, and the design must say where.

| A domain wants | Kernel today | Verdict |
|---|---|---|
| shell heads, file globs, hosts, MCP tools | `Permission` set fields; glob containment by Z3 (`subsumption.py`) | expressible |
| "must not modify `target`" | `forall_steps(forall_writes(not_matches(path, glob→regex)))` | expressible for `file_write` steps and shell write redirects (**Built 2026-09-30**, DI-10). `forall_writes` reads each step's derived write paths: a `file_write` path, and the literal write redirects the shell decomposition finds, also inside `sh -c` and other wrappers (yellow paper §2.3.1). Still not seen: files a command writes through its operands (`cp`, `sed -i`, `tee`) and interpreter work |
| content rules (no secrets, no PII) | `not_matches` on `content`, `metadata.body` (`system_aliases.py`) | expressible; regex may give `unknown`, which denies |
| postconditions ("exit 0", "file exists") | opaque Stage 2 handlers (`_invariant_types.py`); `pytest_passes` definition | partly; opaque types are not words today |
| ordering inside one plan | `before`, `depends_on`, `exists_step` | plan time only; the call-time gate cannot check them (yellow paper §8.2) |
| history across a session, counts, quotas | none | not expressible; see ContrAgent and the counter fragment in `formal-methods-2026-09-23.md` findings 5 and 6 |
| information flow ("read then post") | none | not expressible; a hyperproperty (yellow paper §8.2, §8.3) |
| `within_geofence(zone)` on a waypoint | `workspace_bounds` (one AABB per envelope); `_resolve_path` in `predicate_z3.py` cannot index a tuple, so `target_position.0` is not a path | expressible only as the one bound field, not as a predicate word |
| `keep_separation(d)` between robots | `swarm.py`: analytic AABB disjointness between envelopes, outside the algebra, "space, not spacetime" | not a word today; the algebra has no cross-agent predicate |
| `grip_force_below(n)` | `GripperStep` has `action` and `hold_s`, no force field; `torque_limit` is a joint bound | not expressible |
| nonlinear dynamics (stopping distance, Euclidean separation) | none | polynomial real arithmetic is decidable in principle (the NLSAT procedure of Jovanović and de Moura, IJCAR 2012, used in Z3; the paper page did not load, so not verified here) but can be slow |
| rotations, trigonometry, ODE solutions | none | outside Z3's decidable fragments; tools such as dReal give a different guarantee, "unsat or δ-sat" under bounded numerical perturbation ([Gao, Avigad, Clarke, arXiv 1204.3513](https://arxiv.org/abs/1204.3513)) |

So the software-team dialect can start now. The swarm words in the strategy notes need kernel
work first: path indexing into tuples, a cross-agent predicate or a word form for `swarm.py`'s
disjointness, and force fields on robot steps. Each is a kernel change, so each needs its own
yellow-paper section and full parity. A δ-sat verdict must never be read as `ok`; if dReal-style
checks are ever added, they need their own verdict class (**Open**, section 12).

### 3.5 What the yellow paper must add

A new §2.4 "Dialects" and small edits elsewhere:

1. **Objects.** Dialect, definition, kinds, typed holes, word identity `name@h`, the
   well-founded order.
2. **Unfolding.** The definition of `U_D`: simultaneous, typed, single-pass substitution;
   union on permission set fields; bound fields excluded.
3. **Semantics.** `⟦t⟧_D ≝ ⟦U_D(t)⟧` and `δ_D ≝ δ ∘ U_D` for every procedure in §4, §5, §8.
4. **Conservativity and inheritance.** The three facts of section 3.2, stated as intended
   properties with the same "tested, not machine-checked" caveat as §7.
5. **Fail-closed clauses** added to §6. Each is `⊥`: an unknown word; a hash that does not
   match the pinned dialect; a cycle; a missing or ill-typed argument; a nesting or size bound
   exceeded; a word of the wrong kind in a field; a bound field in a permission word.
6. **Names carry no meaning.** A sentence in §7 "not guaranteed": the checker proves the
   definition, never the name. Name checks (section 5.3) are advice to humans.
7. **Journal.** Each verdict records the surface envelope, the dialect hash, and the hash of
   the unfolded envelope, so a replay checks the same kernel term.
8. **Cross-dialect relations.** Equivalence, subsumption and disjointness of words from
   different dialects are defined on unfoldings (section 6.4).

## 4. What a word covers

### 4.1 Permission words

The first test found these. They are sets of atoms, and they compose by union. Examples from the
test: `read_only_inspection` (25 envelopes), `build_and_test` (21), `code_forges` (17),
`config_files` (16), `source_tree_rw` (9), `design_references` (7). Names are illustrative; the
test labelled them `W0..W14`.

A permission word grants. That makes it the riskiest kind (section 9.2, over-grant).

### 4.2 Predicate words with parameters (invariants)

`must_not_modify(target : glob)` is the first one to build, because 75 real envelopes already
ask for it in opaque form. Today it can cover `file_write` steps only (section 3.4). Until the
kernel exposes shell write paths, the word must be named and documented for what it covers
(for example `no_file_write_to(target)`), or the name itself misleads (section 9.1). The glob
argument fills a regex field (`not_matches`), so the glob-to-regex conversion becomes part of
`U_D` and of the trusted base, and must be identical in the three clients. Others the synonyms suggest: `read_only` (no `file_write` step and
only read-class shell heads), `no_force_push`, `no_destructive_operations` (these need the effect
classifier; see section 3.4). Predicate words narrow. They cannot grant anything, because an
invariant only removes steps from `⟦E⟧` (yellow paper §2.2).

### 4.3 Postcondition words

Today postconditions are mostly `exit_code` (146) and `file_exists` (39) (strategy notes). These
are opaque Stage 2 types (`_invariant_types.py`, `stage2.py`). A word such as `tests_pass` can
unfold to a kernel postcondition, as `pytest_passes` does already (`system_aliases.py`; its
docstring warns that it must only be used as a postcondition). Postcondition words are useful
later, when the output checks in the strategy notes ("What the labs say") exist. Low priority.

### 4.4 Plan fragments (Proposed, on hold with weave)

A repeated run of steps with holes, for example `git_status_then_diff(repo)`, is also an
abstraction. But it is an action, not a policy. That is a pathway's job: pathways are learned
actions (`pathway.py`, `distiller.py`, ADR-0008's parameterized pathways); words are learned
policy. The two meet in weave's original design: a graph whose steps are scripts (pathways),
small models writing in a constrained grammar (the dialect), or the frontier (strategy notes,
"What weave was for"). Recommendation: keep plan fragments in the pathway store, and let a
pathway's envelope be written in dialect words. weave and sprig are on hold with the owner, so
this part is **Proposed** only.

### 4.5 Words and pathways, side by side

| | Pathway | Dialect word |
|---|---|---|
| Learned from | successful traces, clustered by task | envelopes and plans, compressed across tasks |
| Is | a plan template plus its envelope | a named definition in kernel terms |
| Saves | a model call (0 tokens at Tier 0) | tokens in envelope generation; human reading time; one enforced definition in place of many opaque ones |
| Checked by | re-verify each reuse against the caller's envelope | unfolding; every check runs on the kernel |
| Muscle memory at | one agent or robot (a skill) | the population (shared culture) |

## 5. The learning loop

Five steps: mine, name, check the name, deduplicate, promote. All of it is offline, on a copy
of the journal, one heavy job at a time. None of it is in the gate's path.

### 5.1 Mine

Two miners, because the data has two shapes.

**Permission sets: itemset mining, not Stitch.** Stitch works on syntax trees and matches
syntactic patterns; its README says nothing about typed or equational matching
([Stitch README](https://github.com/mlb2251/stitch), read 2026-09-26). A set is order-free. If
`{cat find grep head tail}` is encoded as a sorted list, the five atoms are not a subtree
whenever other atoms sort between them (for example `git`). So permission words come from a
frequent-itemset pass: the greedy pass of the first test first, then a standard closed-itemset
miner. The cost function is the one `scan.py` uses: gain `n·(k−1) − k` for a set of `k` atoms
seen in `n` envelopes.

**Predicate expressions and plans: Stitch.** Stitch is MIT, written in Rust, and was pushed
this month (prior-art report, section 2). It takes JSON lists of programs written as
s-expressions with de Bruijn indices, and finds abstractions with holes `#0, #1, …` up to
`--max-arity` ([README](https://github.com/mlb2251/stitch)). It does "corpus-guided top-down
synthesis" ([arXiv 2211.16605](https://arxiv.org/abs/2211.16605)). The encoding:

- An expression node becomes `(op child…)`. Example:
  `(forall_steps (implies (equals type file_write) (not_matches path R17)))`.
- Leaves (paths, values, regexes, globs, hosts) are interned to symbols such as `R17`, because
  Stitch's primitives are tokens. A side table maps symbols back to literals.
- The algebra has no binders (the step quantifiers bind an implicit step), so no `lam` is
  needed. A Stitch hole `#i` becomes a word parameter `xᵢ`, and its sort is read from the field
  it fills.
- A plan becomes `(plan step…)` with `(shell HEAD arg…)`, `(file_write GLOB)`, `(network HOST)`
  after shell decomposition. This is for plan-fragment mining (section 4.4), not for policy.
- Canonical form first: sort `and`/`or` children, flatten nested `and`. Stitch cannot see that
  `and(a, b)` equals `and(b, a)`. babble does compression modulo an equational theory with
  e-graphs ([arXiv 2212.04596](https://arxiv.org/abs/2212.04596)); it is the
  principled fix if canonical forms are not enough.

A problem: the journal has no predicate expressions yet (section 2.2: 0 of 322). Stitch on
invariants has nothing to compress until words make invariants expressible. So the first
predicate words come from a different miner: **cluster the opaque invariant types and
descriptions** (`file_unchanged` with its synonyms), and have a model write one kernel
definition per cluster. Stitch on expressions becomes useful once envelopes carry `expr`.

Stitch runs as its CLI on files in the scratch directory. The PyPI bindings (`stitch_core`) show
no license (prior-art report), so they are not a dependency. Stitch is never a runtime
dependency of daisugi.

### 5.2 Name and document

A model writes a name, a one-line description, and two or three usage examples for each
candidate, from the words' uses in context. This is LILO's AutoDoc: it "infers natural language
names and docstrings based on contextual examples of usage", and the authors report that it
"boosts performance by helping LILO's synthesizer to interpret and deploy learned abstractions"
([arXiv 2310.19791](https://arxiv.org/abs/2310.19791)). We found no step in LILO
that checks a name against its meaning (prior-art report). We add one.

### 5.3 Check that the name means what the definition says

The name is outside the semantics (section 3.5, item 6). A misleading name is still the most
direct way a word can mislead a human. The check:

1. A **second** model sees only the name and description, not the body. It writes **claims**:
   kernel formulas the name implies. For `read_only_inspection`: "grants no `file_write`
   glob". For `must_not_modify(target)`: "for all `target`, a `file_write` step whose path
   matches `target` is not admitted". Some claims a name implies are not kernel formulas, for
   example "every shell head only reads". The effect classifier (`effects.py`) has a `read`
   class, but it is trusted code outside the kernel and is used for ask tiers; such a claim is
   checked by that code, not proved by Z3 (not checked in detail here).
2. Z3 proves `U_D(w) ⟹ claim` for each claim, as `UNSAT(U_D(w) ∧ ¬claim)`. For a parameter,
   the argument is a free symbolic string, so the proof holds for all arguments. This reuses the
   encoding of `subsumption.py`.
3. Any claim that fails blocks the word and returns Z3's counterexample (a concrete step the
   word admits that its name says it should not). The miner renames or drops the word.
4. The usage examples from 5.2 are run concretely: positive examples must pass, negative ones
   must fail.

A blind model is used so the claims cannot be fitted to the body. A weak claim ("grants
something") passes trivially, so the claim writer is asked for the strongest claims it believes
the name implies. This lowers the risk. It does not remove it (section 9.1).

### 5.4 Deduplicate with Z3

Two words are one word when their unfoldings are equivalent:

- permission words: equal atom sets, with glob sets compared by containment in both directions
  (`_patterns_subsume` in `subsumption.py`).
- predicate words: `UNSAT(U(a) ≠ U(b))`, with parameters matched by position and sort and left
  free.

Equal words merge into the one with more uses; the other name becomes a recorded synonym, so
old envelopes still unfold. Today the garden's merger dedupes pathways by embedding similarity
(`gardener/merger.py`). This is the first deduplication in the system by proof. A word that
subsumes another is kept apart and recorded as a relation ("`read_only` ⊑
`read_only_inspection`"), which coppice can show.

### 5.5 Promote only on measured reuse

Three papers show that LLM library learning often does not reuse its library. "Function reuse
is extremely infrequent" in LEGO-Prover and TroVE, and self-correction and self-consistency drive
the gains ([arXiv 2410.20274](https://arxiv.org/abs/2410.20274)). A
LEGO-Prover case study finds "no evidence of the direct reuse of learned lemmas" and gains that
"vanish once computational cost is accounted for" ([arXiv 2504.03048](https://arxiv.org/abs/2504.03048)).
An EACL 2026 paper asks for equal compute budgets and behavioural checks of reuse
([ACL Anthology 2026.eacl-long.163](https://aclanthology.org/2026.eacl-long.163/)).

So a word is promoted only when it passes this **reuse test**:

1. **Time split.** Mine on the older 70% of traces by `created_at`. Test on the newer 30%.
   Words never see the test set.
2. **Held-out coverage.** The word occurs (its atoms are a subset, or its canonical expression
   matches) in at least **k = 3** held-out envelopes from at least **2** distinct tasks.
3. **Held-out compression.** The whole dialect shrinks the held-out envelopes, word definitions
   counted, by a margin above zero. The 17.5% in-sample figure is not this number.
4. **Behavioural reuse.** In an A/B on held-out tasks, the envelope generator is offered the
   dialect. The prompt tokens for listing the words count against the dialect. The word is
   reused only if the generator emits it without being told to, and the envelopes it writes
   still admit the recorded plans (the distiller's `_validate_envelope` check).
5. **No wider grants.** For a permission word, the atoms it grants that the held-out trace's
   plan never used ("over-grant") must not exceed the over-grant of the envelopes written
   without dialects.

A promoted word is demoted when it goes unused for a set window, in the same way the pruner
evicts stale pathways (`gardener/pruner.py`). SkillGen's paired count of repairs against
regressions (prior-art report, section 1) is a stronger form of test 4, for later.

## 6. Where dialects live

### 6.1 Tiers and populations

The definition tiers become dialect scopes:

| Scope | Today (`aliases.py`) | Proposed |
|---|---|---|
| kernel words | `system` tier, 7 words | the same, reviewed and hash-pinned with each release |
| population dialect | `household` tier (YAML via `integrations/hermes.py`) | one dialect per population: the owner's journal first (the "software team" dialect), a robot fleet later |
| envelope words | `envelope` tier | one-off words inside one envelope; the miner's input for later promotion |

A population is a set of agents or robots that share a journal. The owner's own work is the
first and, for now, the only one.

### 6.2 Versioning

A dialect is a file of definitions, content-addressed. A word's identity is `name@h` (section
3.1). An envelope records the identity of every word it uses. A new definition is a new hash;
the old hash stays in the file while any journal entry uses it. A name never changes meaning in
place. Two envelopes that say `read_only` can mean different things only if they pin different
hashes, and the journal shows which.

### 6.3 Sharing and trust

A dialect is data. It is shared as a signed file, with the signing already used for pathway
bundles (`signing.py`, `pathway_bundle.py`), in a git repo, with no service. Trust is simpler
than for pathways, for one reason: **importing a word grants nothing.** Only an envelope that uses
a word grants, and that envelope is checked in kernel form: by the gate, by the ceiling the owner sets (strategy notes; how a
ceiling is enforced today is not checked here) and, for delegation, by subsumption (yellow paper §5). An imported word can still mislead a reader, so an
imported dialect runs the name check of 5.3 again on import, and its words are shown with their
origin.

### 6.4 Checking two dialects against each other

Both dialects unfold to the same kernel, so any pair of words, or any pair of envelopes, can be
compared with the relations that already exist:

- equivalence (5.4): "their `safe_git_op` is our `git_read`".
- subsumption (`subsumption.py`): "their `deploy_check` ⊑ our `read_only_inspection`", with a
  counterexample step when it fails.
- disjointness: for robot volumes, `swarm.py`'s check; for predicates, `UNSAT(U(a) ∧ U(b))`.

The output is a translation table between two dialects, with a proof or a counterexample for
each row. "Two teams prove that their policies agree" (strategy notes) is the subsumption of two
unfolded envelopes, which the code already decides.

## 7. How models and humans use dialects

- **Envelope generation.** The envelope prompt (`envelope.py`) lists the algebra and says
  `alias(name, args)` exists, but it names no definition, so a model cannot use one. Proposed: the
  prompt lists the population dialect's words (name, parameters, one line each), selected by
  relevance to the task when the dialect is large, the way Anthropic Agent Skills show metadata
  first (prior-art report, section 1). The few-shot example for `file_unchanged` becomes
  `must_not_modify(target)`. The raw algebra stays available for anything the dialect lacks.
- **The gate and the verifier unfold first.** The gate calls `verify(plan, envelope,
  strict=None)` with no registry today (`gate.py`, `_dispatch_verify`), so any word at the gate
  is an unresolved-alias violation, a deny. Proposed: the envelope carries its pinned dialect
  hash; the gate loads that dialect, unfolds, and checks the kernel term. A hash it cannot load
  denies. The Python, Go and Rust gates must unfold byte-identically (section 11).
- **Humans read words.** coppice shows the surface form, with the unfolded form on demand. A deny
  names the word and the kernel atom that failed: "`must_not_modify(src/**)` blocked
  `file_write src/app.py`". The journal keeps both forms (section 3.5, item 7).
- **The human writes nothing.** The owner sets a ceiling and reviews. A word the owner dislikes
  is vetoed in coppice, not rewritten by hand.

## 8. Neighbours and what we borrow

| Neighbour | We borrow | What differs |
|---|---|---|
| Stitch | the compressor for expression and plan trees; its cost-based utility | our terms are policy, and Z3 proves words equal; sets need a separate miner |
| LILO, AutoDoc | model-written names and docs from usage examples | a blind claim check of each name against its definition (5.3) |
| babble | compression modulo an equational theory, with e-graphs ([arXiv 2212.04596](https://arxiv.org/abs/2212.04596)) | Z3 equivalence over the kernel, in place of a hand-written theory; babble is a later option for mining |
| Cedar SymCC | the lint set: never-errors, always-allows, always-denies, subsumption, equivalence, disjointness, each with a counterexample ([SymCC README](https://github.com/cedar-policy/cedar/tree/main/cedar-policy-symcc)) | run the same set on each word and on each dialect; our encoder is not Lean-verified, SymCC's is |
| Progent | "monotonic confinement": an update that narrows applies automatically, one that expands needs approval, decided by an SMT solver ([arXiv 2504.11703](https://arxiv.org/abs/2504.11703)) | the rule for dialect changes: a redefinition that narrows every envelope using the word may be automatic; one that widens needs the owner |
| ActGov | an LLM drafts rules from tool specs, benign traces and attack failures; Z3 searches for a record that satisfies the policy and breaks a safety assertion, offline; named rules are catalogued and reused ([arXiv 2609.24446v2](https://arxiv.org/html/2609.24446v2)) | its named rules are hand-organized levels and scopes over finite values; our words are mined, named by a model, deduplicated by proof, and the proof runs per action |
| Library-learning sceptics | the bar: held-out reuse, equal compute, behavioural checks | we must pass it before claiming anything (5.5) |

"Not found" in the prior-art report is not "does not exist". We found no system that learns
named, proved words for the policy that governs agent actions. That is the part that may be new.

## 9. Risks

### 9.1 Misleading names

A word named `read_only` that allows `rm` misleads the human who approves a deny. Mitigation:
the blind claim check (5.3); coppice shows the unfolded form on demand; names are never part of
the proof. Residual risk: a claim writer that asks for too little.

### 9.2 Over-grant through bundling

A permission word is a bundle. A model that needs `grep` may write `read_only_inspection` and
get `find` and `tail` too. Each word pulls envelopes toward wider grants. Mitigation: reuse test
5 (over-grant not worse than baseline); the garden reports granted-but-unused atoms per word;
small words are preferred over large ones in the prompt. **Open**, section 12, item 3.

### 9.3 Vocabulary bloat

Hundreds of words cost prompt tokens and reader attention. Mitigation: promotion needs reuse;
demotion on disuse; a cap on the listed words per prompt; synonyms merged by proof.

### 9.4 Drift

The owner's work changes, and a word stops fitting. Its uses fall, and it is demoted. A word
whose definition changes gets a new hash (6.2), so drift never changes an old envelope's meaning.

### 9.5 A word used outside its domain

`source_tree_rw` means `lib/** src/**` in a Flutter repo, and it may be wrong in another repo.
Mitigation: words carry the population they were mined from; the reuse test counts distinct
projects; relative globs resolve against the task's workspace as today. A word used outside its
population is shown as such in coppice.

### 9.6 Poisoning through the journal

The journal records actions an attacker may have steered (prompt injection). A repeated
malicious grant could be mined as a word. Mitigation: only envelopes from traces that succeeded
and were not denied are mined; a permission word is never promoted without the reuse and
over-grant tests; widening redefinitions need the owner (Progent's rule). **Shadowing** is a
direct poisoning path today: an `envelope`-tier definition named `no_secrets` wins over the system
word (`aliases.py`, `lookup` and `_TIER_ORDER`), so a model-written envelope can redefine a
system word to mean less. The design forbids shadowing (section 10).

### 9.7 Trusted code grows a little

`U_D` joins the trusted base, next to the glob and regex translations (yellow paper §7). It is
small and syntactic, and three clients must agree on it, so the differential harness covers it.

## 10. Gaps in definitions today (found by reading the code)

These were found by reading, not by tests. Each must close before a learned word reaches the gate.

| Gap | Where | Fix |
|---|---|---|
| Arguments are spliced into strings by text replacement, including regex fields (`never_impersonates` puts `$principal` inside a regex). A regex metacharacter in an argument changes what the pattern matches. | `aliases.py`, `_substitute_params` | typed holes that fill a whole field; if a regex needs an argument, escape it unless the parameter's sort is `regex` |
| Substitution runs one name after another. A value that contains `$other` is rewritten by a later pass (longest names go first, so a value for `principal_name` that contains `$principal` is changed by the `principal` pass). | `aliases.py`, `_substitute_params` | one simultaneous pass; values are never read again as templates |
| An envelope-tier word shadows a household or system word of the same name. | `aliases.py`, `lookup` | names unique across tiers; identity by hash; a new definition is a new word |
| Cycles are found only at resolve time. | `aliases.py`, `resolve` | check the definition order at register time |
| Register-time vacuity swallows every exception, so a word Z3 cannot check is registered anyway. The gate path re-checks vacuity after unfolding, so this is a hole in the store, not in the gate. | `aliases.py`, `register` | a word that cannot be checked is not registered |
| When nested resolution fails for a term that is not itself an `AliasRef`, `verify()` falls through and evaluates the term as it is. It fails closed only because the evaluator then raises. | `verify.py`, `_check_predicate_item` | return an `unresolved_alias` violation directly |
| The gate passes no registry, so a word at the gate always denies. | `gate.py`, `_dispatch_verify` | load the dialect pinned by the envelope's hash |
| `verify()` does not record conformance cases when a registry is passed ("a registry is process state"), so the corpus has no definition cases. | `verify.py`, `verify` | a case carries its pinned dialect inline, so it is self-contained |
| No permission words exist. | `models.py`, `Permission` | add permission words (section 3.1) |
| No size or depth bound on unfolding. | `aliases.py`, `resolve` | bounds of section 3.3 |

## 11. Staged plan

Each stage says what it proves and when it stops. Anything that reaches the gate is built in the
Python oracle first, then Go and Rust, with differential cases.

**Stage 0 (done, 2026-09-26).** Greedy itemset pass over permission sets: 17.5% in-sample.
Read-only count: 0 of 322 invariants checkable.

**Stage 0, the kernel and the first words (Built, 2026-09-30).** DI-10 and DI-1, in Python,
Go and Rust, with full parity (rulings DL-1 to DL-10 in `clients/ADJUDICATIONS.md`):

- **The field and the quantifier.** `forall_writes(φ)`, inside `forall_steps` or
  `exists_step`, holds when `φ` holds for every write path of the step
  (`src/opendaisugi/write_paths.py`): a `file_write` path, and each literal write redirect
  of a shell step, normalized. Unknown redirects make it false; outside a step it is an
  error. The paths are derived, never stored. Yellow paper §2.3.1.
- **The system dialect** (`src/opendaisugi/dialect.py`): `keep_unchanged(target)` :=
  `forall_steps(forall_writes(not_matches(path, R(target))))`, where `R` turns the glob
  into a regex that matches exactly what the file-scope matcher matches. 23 synonyms from
  the journal's invariant names (`file_unchanged`, `read_only`, `no_modifications`,
  `file_immutable`, `file_preservation`, …) unfold to the same term, proved equal by Z3.
  The dialect is identified by a hash of its definitions. An opaque invariant whose type
  names a word is a use of it, its `target` the argument (`**` when absent).
- **Audit first.** With no pin, a word use is evaluated and a failure is a warning
  (`dialect audit: …`); the verdict does not change. `dialect_enforce` in config.yaml,
  equal to the hash, enforces the words; any other hash denies each word use. The pin is
  the operator's, not the envelope's (DL-4, provisional against DI-9). The gate logs
  `word_audit`; `gate report` and `gate status` show the would-deny count.
- **Cases.** `clients/fixtures/dialect/` (753 verify cases, 3 decompose cases, and the
  glob fixture), 108 new gate cases (among them `&&` chains near Python's recursion
  limit) and 13 new CLI cases. Go and Rust: 0 disagreements on the dialect cases, the
  1,464 gate cases and the 540 CLI cases.
- **Follow-ups (Built, 2026-09-30; rulings DL-11 to DL-17).** The write paths add the operand
  writes of 15 known writers (`cp`, `mv`, `tee`, `sed -i`, `dd of=`, `rm`, `rsync` and
  others) with each command's argument rules; a writer under `xargs` or `find -exec`, an
  unknown flag or a word the shell changes is unknown; a line that runs `cd` and writes a
  relative path is unknown. At the gate a word is placed from the call's cwd: a relative
  target joins it and relative writes resolve against it, so a model's `src/**` meets the
  gate's absolute paths. The write paths are in the dialect hash (now `108a89a256798dfc`).
  A hard-deny rule covers daisugi's own config.yaml, where the pin lives. `daisugi status`
  counts the plans in the journal and the calls in the gate's audit log that a word would
  deny.
- **Not in stage 0.** Interpreter work and MCP writes, permission words, population
  dialects, envelope-level pins, a word inside an `alias` reference at the gate, and the
  envelope prompt, which still teaches the opaque form. A plan verified outside the gate
  has no cwd, so its relative targets meet only relative writes.

**Stage 1. The Stitch test on the copied journal.** Offline, in
`~/opendaisugi-scratch/dialects/`, one heavy job under the memory cap. Three parts:
(a) the time-split reuse test (5.5, tests 1 to 3) for permission words, with a real itemset
miner; (b) Stitch over plans as trees, to see whether plan fragments repeat (evidence for
pathways and weave, not for policy); (c) cluster the opaque invariants and have a model propose
kernel definitions for the top clusters. Output: a short results file. Stop rule: if held-out
compression is near zero and no invariant cluster gets a definition that verifies the recorded
plans, the idea waits for more data (strategy notes).

**Stage 2. `daisugi dialect mine`, a read-only report.** Proposed CLI:
`daisugi dialect mine [--journal DIR] [--since DATE] [--json]`. It reads the journal and writes
nothing but its report: candidate words, model names and docs, name-check results with
counterexamples, equivalence groups, held-out reuse and over-grant per word. It changes no
envelope and nothing at the gate, so Python only is acceptable at this stage. It needs a model
for naming and claims; with no model it reports unnamed candidates.

**Stage 3. Words in envelope generation.** Prerequisites: every gap in section 10 closed, in all
three clients, with differential cases that carry a dialect inline; the yellow-paper section of
3.5. Then, in order:
(a) ship the seed dialect: `must_not_modify(target)` and the `read_only` family (over `file_write`
steps only, and named for that, until shell write paths reach the kernel; section 3.4), defined from the
Stage 1 clusters; (b) run it in **audit**: generate each envelope with and without the dialect,
check both, log the difference in verdicts and the false-deny rate (the dogfood week measures
this rate already); (c) switch the envelope prompt to the dialect when audit shows no rise in
false denies and no wider grants. This stage turns today's opaque invariants into enforced
ones, which can raise denies. That is why it runs in audit first.

**Stage 4. Sharing.** Signed dialect files; `daisugi dialect diff A B` for the translation table
of 6.4; import with the name check. Parity for anything the gate reads.

Robot words wait for the kernel work named in 3.4, each with its own yellow-paper section.

## 12. Open questions for the owner

1. **Enforce the opaque invariants the models already write?** Mapping `file_unchanged` and its
   synonyms to `must_not_modify(target)` turns documentation into policy at low stakes.
   Options: enforce at once; audit first; leave opaque. **Recommendation:** audit first
   (Stage 3b), then enforce. *Ruled DI-1; built in stage 0 as `keep_unchanged`, in audit
   until `dialect_enforce` pins the dialect.*
2. **Shadowing across tiers.** Options: keep precedence as today; forbid shadowing; allow it only
   for narrowing redefinitions. **Recommendation:** forbid. Identity by hash makes it unneeded.
3. **Permission words at all?** They are what the data shows, and they carry the over-grant risk.
   Options: permission words with the over-grant test; predicate words only; permission words as
   reading aids in coppice but unfolded before the envelope is stored. **Recommendation:**
   permission words with the over-grant test, and a widening redefinition needs the owner.
4. **Who promotes a word to the population dialect?** Options: automatic after the reuse test;
   owner approval for each; automatic for predicate words (they only narrow), owner approval for
   permission words (they grant). **Recommendation:** the last one, with a veto in coppice.
5. **Name checks.** Options: a blind second model writes claims (5.3); a fixed table of claims
   per name token (`read_only`, `no_`); no check. **Recommendation:** the blind model. A fixed
   table is a hand-written policy language by another name.
6. **Stitch as a tool.** Options: build the Stitch CLI from source as an offline tool; the PyPI
   bindings (no license found); a port. **Recommendation:** the CLI, offline only, never a
   runtime dependency. Revisit babble if canonical forms miss too many equal terms.
7. **Kernel growth for robots.** Options: add tuple indexing to paths and a force field now; wait
   for a robot user; add a separate δ-sat verdict class for nonlinear dynamics.
   **Recommendation:** wait. Name the three kernel gaps in the scorecard now, so no one claims
   swarm words that cannot be written.
8. **Plan fragments.** Options: dialect words; pathways; weave. **Recommendation:** pathways
   hold actions and dialects hold policy. weave is on hold, so this stays **Proposed**.
9. **Surface form without a hash.** Should the gate accept an envelope that uses words but pins
   no dialect? **Recommendation:** no. It denies, as an unresolved definition does today.
10. **Expose shell write paths to the algebra?** The flagship word cannot see `sed -i` or `>`
    today (section 3.4). Options: add a field with each shell step's decomposed write paths and a
    quantifier over it, in all three clients; keep shell writes to `Permission.file_write` only;
    deny shell steps under a write-guard word. **Recommendation:** add the field, as part of
    Stage 3's prerequisites. Without it the most-requested word misses every shell write. The
    copied plans hold 490 `file_write` steps and 3,393 shell steps; how many of the shell steps
    write was not counted. *Ruled DI-10; built in stage 0 (`forall_writes`), with the operand
    writes of the known writers added in the stage 0 follow-ups (DL-11).*

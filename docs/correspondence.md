# Concepts across fields

Status: reference, 2026-09-26. This page maps openDaisugi's own terms to the established names in
five fields: functional programming (FP), formal methods and logic (FM), compilers and
programming-language theory (PL), control theory and runtime assurance (RTA), and agents, LLMs and
machine learning (ML). It is a map for readers who know one of those fields, and a checklist of
what the fields know that we do not use yet.

How to read it:

- **Part A** lists our terms. Each cell names the nearest outside term and links to its entry in
  Part B. A blank cell means no honest match exists. A "≈" means the match is partial; the Part B
  entry says where it breaks.
- **Part B** defines each outside term in two sentences, says why it matters here, and cites one
  source that was read for this page.
- **Part C** lists what the correspondence says we are missing.
- **Part D** lists terms we could rename to the standard name. These are recommendations. The
  owner decides.

Status words: **Built** (code exists), **Designed** (a design doc exists, no code), **Proposed**
(on hold with the owner: sprig, weave). Sources marked "(search listing)" were checked from
a search result page, not a fetched page. Books and papers for deeper study are in
[`research/reading-list-2026-09-26.md`](research/reading-list-2026-09-26.md). The companion pages
are [`concepts.md`](concepts.md) and the [yellow paper](spec/yellow-paper.md).

## Part A: our terms and their names elsewhere

| Our term | One-line meaning (where) | FP | FM | PL | RTA | ML |
|---|---|---|---|---|---|---|
| **gate** | Decides allow, deny or ask for each tool call before it runs. Built: `src/opendaisugi/gate.py`. | | [execution monitor](#execution-monitor), [runtime verification](#runtime-verification) | | [reference monitor](#reference-monitor), [envelope protection](#envelope-protection) (hard) | [tool use](#tool-use) hook |
| **envelope** | Permission record, invariants, postconditions, stakes: what an agent may do. Built: `models.py`. | | [specification](#model-checking); a [safety property](#safety-liveness) over steps | ≈ [refinement type](#refinement-types) or [effect](#effect-system) bound on a step | [safety envelope](#envelope-protection), [safe set](#reachability) | ≈ "policy" in the access-control sense ([reference monitor](#reference-monitor)), not the RL sense of [preference learning](#preference-learning); see Part D |
| **verifier** | `verify(plan, envelope)`: staged checks, Z3 last. Built: `verify.py`. | | [decision procedure](#sat-smt), [SMT](#sat-smt) | [type checker](#type-system) | decision module of [Simplex](#simplex) | |
| **predicate** | A formula in the small algebra: leaves, And/Or/Not/Implies, step quantifiers. Built: `predicate.py`, `predicate_z3.py`. | [algebraic data type](#adt) (the AST) | first-order formula over strings and reals ([SMT](#sat-smt)) | refinement predicate ([refinement types](#refinement-types)) | ≈ state constraint ([barrier](#cbf) condition) | |
| **definition** (the `alias` field; was alias) | A named, parameterized predicate, unfolded before checking. Built: `aliases.py`, `system_aliases.py`. | named function ([referential transparency](#pure-function)) | [definition](#conservative-extension) | macro-like unfolding ([term rewriting](#term-rewriting)); [DSL](#dsl) word | | ≈ learned abstraction ([library learning](#library-learning)) |
| **dialect** | A per-domain vocabulary of words defined only in kernel terms. Designed: `design-dialects.md`. | | [conservative extension](#conservative-extension) by definitions | [DSL](#dsl) as a library over one core; [library learning](#library-learning) | | [LLM library learning](#llm-library-learning) |
| **kernel** | The fixed core: `Permission` plus the algebra without `AliasRef`. Designed term: `design-dialects.md` §3.1. | | ≈ trusted kernel of a [proof assistant](#proof-assistant) (a different sense; see Part D) | core language that a [DSL](#dsl) unfolds to ([conservative extension](#conservative-extension)) | | |
| **subsumption** | `E_in ⊑ E_out`: the inner envelope admits no step the outer forbids. Built: `subsumption.py`. | | validity of `admit_in ⇒ admit_out` (UNSAT of `A ∧ ¬B`) by [SMT](#sat-smt) | [subtyping](#subtyping) of refinement types | [attenuation](#ocap) | |
| **effects tiers** | Ask tiers silent, undoable, permanent, from an allowlist classifier. Built: `effects.py`. | ≈ [effects](#algebraic-effects) | | ≈ [effect system](#effect-system) (a classifier, not a type system) | [fail-safe defaults](#fail-safe) (unknown is permanent) | |
| **pathway** | A distilled envelope plus plan template, re-verified on reuse. Built: `pathway.py`, `pathway_store.py`. | [memoized](#pure-function) function | | compiled tier of a [JIT](#jit); residual program of [partial evaluation](#partial-evaluation) | advanced controller in the [Simplex](#simplex) mapping (whitepaper §3) | [skill library](#skill-library) entry |
| **garden** | Keeps pathways honest: A/B test, prune, merge, regression. Built: `gardener/`. | | | profile-guided tier management in a [JIT](#jit) | | skill-library upkeep ([skill library](#skill-library)) |
| **distillation** | Cluster journal traces, intersect permissions, generalize a plan template. Built: `distiller.py`. | | | trace compilation ([JIT](#jit)), [partial evaluation](#partial-evaluation) | | ≈ [program synthesis](#program-synthesis) from examples; NOT [knowledge distillation](#knowledge-distillation) (see Part D) |
| **tend** | One run of the distill pipeline over recent traces. Built: `Distiller.tend` in `distiller.py`. | | | offline recompilation ([JIT](#jit)) | | |
| **router** | Picks the cheapest viable model tier per task. Built: `routing.py`; designed: `design-router.md`. | | | tier choice in [tiered compilation](#jit) | | [LLM routing](#routing), [cascade](#cascade) |
| **gateway** | A `base_url` proxy: routes whole turns, offers reuse. Built: `gateway.py`, ADR-0012, ADR-0015. | | | | | [routing](#routing) of whole turns |
| **graft** | A fourth verdict, rewrite: replace a tool call or its result with a cheaper one, proved at install. Designed: `design-grafts.md`. | | [edit automaton](#edit-automata) (substitution) | [term rewriting](#term-rewriting) rule | correcting [shield](#shield) | tool-call rewriting (per `research/grafts-inputs-2026-09-26.md`) |
| **journal** | Append-only store of every envelope, plan, verdict and result. Built: `journal.py`. | | execution trace for [runtime verification](#runtime-verification) | profile ([JIT](#jit)) | [tamper-evident log](#tamper-evident-log) (in part) | trajectories that [skill library](#skill-library) learners mine |
| **fail closed** | Unknown, timeout or unsupported means deny. Built law: yellow paper §6. | | [soundness](#soundness) over completeness | ≈ [deoptimization](#deoptimization) to the safe path | [fail-safe defaults](#fail-safe) (Saltzer and Schroeder); NOT engineering "fail-safe" | |
| **audit mode** (was shadow mode) | The gate evaluates every call and logs would-deny, but allows. Built: `gate.py` (default mode). | | | | [audit mode](#audit-mode) | |
| **ask** | A denied call goes to a present operator; no operator means deny. Built: `ask.py`, `effects.py`. | | | | [soft protection](#envelope-protection) | human pick as a [preference](#preference-learning) label; the "ask" permission decision in Claude Code hooks (per `research/grafts-inputs-2026-09-26.md`) |
| **delegation tree** (was agent tree) | A parent starts children; each child envelope must be proved inside the parent's. Proposed: `design-delegation-tree.md`. | | | [subtyping](#subtyping) along each edge | capability [attenuation](#ocap), least privilege ([Saltzer and Schroeder](#fail-safe)) | sub-agents ([harness](#harness)) |
| **ranking** | N attempts: eliminate on hard checks, rank by measures, break ties by pairwise votes. Designed: `design-ranking.md`. | | | | | [Bradley-Terry](#bradley-terry), [LLM-as-judge](#llm-judge), [preference learning](#preference-learning) |
| **coppice** | The Go floor daemon: panes, agents, state from the gate. Built: `harness/coppice/`. | | | debugger and console (strategy notes, JIT table) | [supervisory](#supervisory-control) window | multi-agent manager around a [harness](#harness) |
| **sprig** | A minimal Go agent loop with the gate in-process. Built MVP: `harness/sprig/`; further work Proposed (on hold). | | | | | agent loop ([ReAct](#react)), [harness](#harness) |
| **weave** | Runs a JSON graph of steps, one at a time, stop at first failure. Built MVP: `harness/sprig/weave/`; Proposed: ADR-0022. | [free monad](#free-monad) interpreter | | [dataflow](#dataflow) graph runner | | workflow ([harness](#harness)) |
| **plan / ActionPlan** | A DAG of typed steps, verified as a whole before it runs. Built: `models.py`, `dag.py`. | program as data ([free monad](#free-monad)) | a finite trace, checked as in [runtime verification](#runtime-verification) | [dataflow](#dataflow) graph, IR | planned trajectory ([reachability](#reachability)) | a workflow ([harness](#harness)) |
| **step types** | 13 step classes: shell, file read/write, network, MCP, task, agentic, skill, VLA, joint move, Cartesian move, gripper, simulation reset; plus custom `@step_type`. Built: `models.py`. | sum type ([ADT](#adt)) | | cases of the plan IR ([dataflow](#dataflow)) | controllable events ([supervisory control](#supervisory-control)) | [tool](#tool-use) calls |
| **vacuity** | A predicate that is always true or always false is rejected. Built: `vacuity.py`. | | ≈ [vacuous pass](#vacuity-mc); satisfiability checks ([SMT](#sat-smt)) | | | |
| **receipts** | A per-step evidence record; run integrity requires every executed step to have one. Built: `Receipt` in `models.py`, `supervisor.py`. | | evidence for [runtime verification](#runtime-verification) | | [tamper-evident log](#tamper-evident-log) entry | |

Term count, Part A: 29.

## Part B: the outside terms

### Functional programming

- <a id="pure-function"></a>**Pure function, referential transparency, memoization.** An
  expression is referentially transparent when you can replace it by its value without changing
  the result. Only such a function can be memoized, because the cached result stays valid.
  *Us:* a frozen pathway is a memo of a task, and it is safe only because the gate re-verifies
  each reuse against the caller's envelope; the world is not pure.
  [Wikipedia: Memoization](https://en.wikipedia.org/wiki/Memoization),
  [Referential transparency](https://en.wikipedia.org/wiki/Referential_transparency).
- <a id="adt"></a>**Algebraic data type.** A type built from sums (one of several cases) and
  products (several fields together). Pattern matching lets a compiler check that every case is
  handled. *Us:* step types and the predicate AST are sum types; exhaustive handling is what
  "an unknown step type is rejected under strict" means (yellow paper §4.2).
  [Wikipedia](https://en.wikipedia.org/wiki/Algebraic_data_type).
- <a id="free-monad"></a>**Monad, free monad.** A monad sequences steps that carry extra
  information. A free monad records the steps as data and leaves their meaning to a separate
  interpreter. *Us:* an `ActionPlan` is a program held as data, checked before an interpreter
  (the executor, or weave) runs it.
  [Wikipedia](https://en.wikipedia.org/wiki/Monad_(functional_programming)).
- <a id="algebraic-effects"></a>**Algebraic effects and handlers.** Effects such as state, I/O
  and nondeterminism are operations of an algebraic theory. A handler gives them a meaning, so
  one program can run under different handlers. *Us:* the gate is a handler that may refuse an
  effect; a graft is a handler that substitutes one.
  [Plotkin and Pretnar, ESOP 2009](https://www.research.ed.ac.uk/en/publications/handlers-of-algebraic-effects) (search listing).
- <a id="curry-howard"></a>**Propositions as types (Curry-Howard).** A proof is a program, and
  the formula it proves is its type. Proof assistants such as Lean are built on it.
  *Us:* it is the road to a machine-checked checker (Part C, gap 1).
  [Wikipedia](https://en.wikipedia.org/wiki/Curry%E2%80%93Howard_correspondence).

### Formal methods and logic

- <a id="sat-smt"></a>**SAT, SMT, decision procedure.** SMT decides whether a formula over
  theories (integers, reals, strings, arrays) is satisfiable. Solvers such as Z3 and cvc5 answer
  sat (with a model), unsat, or unknown. *Us:* the verifier's core. "Unknown" is a real answer
  on string constraints, and the fail-closed law turns it into deny.
  [Wikipedia](https://en.wikipedia.org/wiki/Satisfiability_modulo_theories).
- <a id="unsat-core"></a>**Counterexample and unsat core.** When a check fails, the solver's
  model is a concrete counterexample; when it succeeds, an unsat core names the constraints that
  made it succeed. *Us:* subsumption already returns a counterexample step
  (`subsumption.Counterexample`); the unsat core as a deny reason is used by a solver-aided
  compliance checker (per `research/prior-art-2026-09-26.md`, §3; source not re-read here).
- <a id="model-checking"></a>**Model checking.** It checks whether a finite-state model meets a
  specification, usually in temporal logic. On failure it gives a counterexample trace.
  *Us:* we check one plan or one call, not a model of all runs; the envelope is the
  specification. [Wikipedia](https://en.wikipedia.org/wiki/Model_checking).
- <a id="proof-assistant"></a>**Proof assistant and trusted kernel.** A tool for writing proofs
  a machine checks. Many rest on a small trusted kernel that can be inspected by hand.
  *Us:* our "kernel" is a core language, not a trusted checker. The Lean client proves a few
  Core properties; the SMT encoders are not machine-verified (yellow paper §7; Part C, gap 1). [Wikipedia](https://en.wikipedia.org/wiki/Proof_assistant).
- <a id="soundness"></a>**Soundness and completeness.** Sound: everything proved is true.
  Complete: everything true is provable. *Us:* we claim soundness for what we check and do not
  claim completeness (yellow paper §3). [Wikipedia](https://en.wikipedia.org/wiki/Soundness).
- <a id="runtime-verification"></a>**Runtime verification.** It extracts events from a running
  system and checks them against a specification with a monitor, and may react to a violation.
  *Us:* the gate plus the journal is runtime verification with enforcement.
  [Wikipedia](https://en.wikipedia.org/wiki/Runtime_verification).
- <a id="execution-monitor"></a>**Execution monitor (security automaton).** Schneider gave the
  exact class of policies a monitor that watches execution can enforce, and automata to write
  them. That class is the safety properties. *Us:* yellow paper §8.1 uses this result to state
  what the call-time gate can promise. [Schneider 2000 (PDF)](https://www.cs.cornell.edu/fbs/publications/EnfSecPols.pdf).
- <a id="edit-automata"></a>**Edit automata.** A monitor that can truncate a run, suppress an
  action, or insert new actions. It enforces more than a monitor that can only stop.
  *Us:* a graft is a suppress-and-insert edit.
  [Ligatti, Bauer, Walker 2005](https://link.springer.com/article/10.1007/s10207-004-0046-8) (search listing).
- <a id="ltl"></a>**Temporal logic (LTL, LTLf).** LTL states properties of runs with operators
  such as always (G), eventually (F), until (U) and next (X). LTLf is its form over finite
  traces. *Us:* the envelope is per step; order, history and quotas need this (Part C, gap 3).
  [Wikipedia](https://en.wikipedia.org/wiki/Linear_temporal_logic).
- <a id="safety-liveness"></a>**Safety and liveness.** Safety: something bad never happens,
  and a violation is final. Liveness: something good eventually happens; no finite prefix
  refutes it. *Us:* the gate enforces safety only (yellow paper §8.2).
  [Wikipedia](https://en.wikipedia.org/wiki/Safety_and_liveness_properties).
- <a id="hyperproperty"></a>**Hyperproperty.** A property of sets of runs, not of one run.
  Information flow (non-interference) is one. *Us:* "read a secret, then post it" is outside
  any per-call gate (yellow paper §8.3). Source: Clarkson and Schneider 2010, as cited in the
  yellow paper (not re-read here).
- <a id="vacuity-mc"></a>**Vacuous pass.** A specification can pass trivially, for example an
  implication whose antecedent never holds. Beer et al. detect such passes and give interesting
  witnesses. *Us:* `vacuity.py` checks a related but simpler thing: a predicate that is a
  tautology or a contradiction. It does not detect an antecedent that the plan never meets.
  [Beer et al., FMSD 2001](https://link.springer.com/article/10.1023/A:1008779610539) (search listing).
- <a id="datalog"></a>**Datalog.** A declarative logic language, a subset of Prolog, evaluated
  bottom-up. It is not Turing-complete, so evaluation always terminates. *Us:* a candidate for
  delegation chains in the delegation tree (Part C, gap 7).
  [Wikipedia](https://en.wikipedia.org/wiki/Datalog).
- <a id="conservative-extension"></a>**Conservative and definitional extension.** A
  conservative extension proves no new theorems in the old language. An extension by
  definitions is always conservative. *Us:* this is the whole safety argument of dialects
  (`design-dialects.md` §3.2). [Wikipedia](https://en.wikipedia.org/wiki/Conservative_extension).

### Compilers and programming-language theory

- <a id="type-system"></a>**Type system, type checker.** A syntactic method that proves the
  absence of some bad behaviours by classifying program parts. The check runs before the program.
  *Us:* the verifier plays the type checker in the JIT analogy (strategy notes).
  [MIT Press: TAPL](https://mitpress.mit.edu/9780262162098/types-and-programming-languages/) (search listing).
- <a id="subtyping"></a>**Subtyping.** S is a subtype of T when a term of S can be used wherever
  a T is expected. For functions, argument types are contravariant. *Us:* subsumption is
  subtyping of envelopes: the inner envelope is the subtype.
  [Wikipedia](https://en.wikipedia.org/wiki/Subtyping).
- <a id="refinement-types"></a>**Refinement types.** A type with a predicate that holds for
  every value of it, such as "integers above zero". Checking often uses an SMT solver.
  *Us:* an envelope is close to a refinement of the step type.
  [Wikipedia](https://en.wikipedia.org/wiki/Refinement_type).
- <a id="effect-system"></a>**Effect system.** A formal system that records what effects a
  program has (reads, writes, I/O), often with the region it touches. It can verify at compile
  time. *Us:* `effects.py` classifies effects of one call; it is an allowlist, not a proved
  system. [Wikipedia](https://en.wikipedia.org/wiki/Effect_system).
- <a id="partial-evaluation"></a>**Partial evaluation.** Specialize a program to its known
  inputs to get a faster residual program. Futamura showed that specializing an interpreter
  gives a compiler. *Us:* a pathway is a plan specialized to a task family, with holes for the
  rest (ADR-0008). [Wikipedia](https://en.wikipedia.org/wiki/Partial_evaluation).
- <a id="jit"></a>**JIT and tiered compilation.** Compile during execution: interpret first,
  profile, then compile hot code. *Us:* the whole strategy analogy: frontier model as
  interpreter, journal as profiler, pathways as compiled code.
  [Wikipedia](https://en.wikipedia.org/wiki/Just-in-time_compilation).
- <a id="deoptimization"></a>**Deoptimization.** When an optimized assumption stops holding,
  the runtime falls back to unoptimized code. Hölzle et al. introduced it for debugging in SELF.
  *Us:* a pathway whose guard fails should drop back to the frontier; today that is re-verification
  plus the garden's pruner. [Hölzle, Chambers, Ungar, PLDI 1992](https://dl.acm.org/doi/10.1145/143103.143114) (search listing).
- <a id="dsl"></a>**Domain-specific language.** A small language for one aspect of a system.
  Fowler separates external DSLs (own parser) from internal ones (an API in a host language),
  and layers a DSL over a library. *Us:* a dialect is an internal DSL over the kernel.
  [Fowler](https://martinfowler.com/books/dsl.html).
- <a id="library-learning"></a>**Library learning.** Compress a corpus of programs into named,
  reusable abstractions. DreamCoder alternates solving and abstracting; Stitch does it by
  top-down search, much faster. *Us:* the method for mining dialect words.
  [DreamCoder](https://arxiv.org/abs/2006.08381), [Stitch](https://arxiv.org/abs/2211.16605).
- <a id="egraph"></a>**E-graph, equality saturation.** An e-graph stores many equivalent terms
  at once; equality saturation applies rewrites until nothing changes, then extracts the best.
  *Us:* babble does library learning modulo equations this way; we propose Z3 equivalence
  instead. [egg, POPL 2021](https://arxiv.org/abs/2004.03082).
- <a id="term-rewriting"></a>**Term rewriting.** Rules replace subterms that match a left side
  with a right side. Termination and confluence are the key properties. *Us:* grafts are rewrite
  rules on tool calls; definition unfolding is a terminating rewrite.
  [Wikipedia](https://en.wikipedia.org/wiki/Rewriting).
- <a id="dataflow"></a>**Dataflow.** A program as a directed graph of data between operations;
  each runs when its inputs are ready. *Us:* plans are DAGs; weave runs them one step at a time
  today. [Wikipedia](https://en.wikipedia.org/wiki/Dataflow_programming).

### Control theory, runtime assurance and security

- <a id="simplex"></a>**Simplex architecture.** A high-assurance controller, a high-performance
  controller, and decision logic that keeps the plant inside the region the safe controller can
  recover from. Sha's point: reliability comes from a simple, reliable core. *Us:* our lineage,
  but the gate only prevents actions and gives no recoverable-state promise (yellow paper §8.6).
  [Sha 2001, reading-group slides](https://engineering.purdue.edu/dcsl/reading/2007/foob-using_simplicity_to_control_complexity.pdf);
  [Black-Box Simplex](https://arxiv.org/abs/2102.12981).
- <a id="rta"></a>**Runtime assurance (RTA).** Bound the behaviour of a complex function that
  cannot be certified by design assurance, with a monitored architecture. ASTM F3269 is the
  aviation standard. *Us:* the whitepaper's frame.
  [ASTM F3269-21](https://store.astm.org/f3269-21.html) (search listing).
- <a id="reference-monitor"></a>**Reference monitor.** Mediates every access by a subject to an
  object. It must be non-bypassable, evaluable, always invoked and tamper-proof. *Us:* the gate;
  "always invoked" holds only while the host fires the hook (yellow paper §8.5).
  [Wikipedia](https://en.wikipedia.org/wiki/Reference_monitor).
- <a id="shield"></a>**Shield.** A synthesized component that enforces a temporal-logic
  specification on a learning agent. It either lists the safe actions first or corrects an
  unsafe chosen action. *Us:* the gate is a blocking shield; a graft is a correcting one.
  [Alshiekh et al. 2017](https://arxiv.org/abs/1708.08611).
- <a id="reachability"></a>**Reachability, reachable set.** Decide whether a state is reachable,
  or compute all states reachable under all admissible inputs. Safety is "no unsafe state is
  reachable". *Us:* swarm deconfliction is a static box check, not a reachable-set computation.
  [Wikipedia](https://en.wikipedia.org/wiki/Reachability_problem);
  [Bansal et al., HJ reachability](https://dl.acm.org/doi/10.1109/CDC.2017.8263977) (search listing).
- <a id="envelope-protection"></a>**Flight envelope protection.** Keeps pilot commands inside
  the aircraft's limits. Hard protection ignores the bad input; soft protection warns and resists.
  *Us:* deny is hard protection; ask is closer to soft.
  [Wikipedia](https://en.wikipedia.org/wiki/Flight_envelope_protection).
- <a id="cbf"></a>**Control barrier function.** A function whose sign marks the safe set, used
  as a constraint in an optimizing controller so the state never leaves that set. *Us:* the
  robot side's future (whitepaper §7: no CBF-QP today).
  [Ames et al. 2019](https://arxiv.org/abs/1903.11199).
- <a id="fail-safe"></a>**Fail-safe defaults, fail-safe, fail-secure.** Saltzer and Schroeder:
  base access on permission, not exclusion. In engineering, "fail-safe" means harmless on failure
  (a door unlocks), and "fail-secure" means the door locks. *Us:* "fail closed" is fail-safe
  defaults and fail-secure, the opposite of the door sense.
  [Saltzer and Schroeder](https://www.cs.virginia.edu/~evans/cs551/saltzer/);
  [Wikipedia: Fail-safe](https://en.wikipedia.org/wiki/Fail-safe).
- <a id="supervisory-control"></a>**Supervisory control.** A supervisor watches a plant's
  events and may disable controllable ones; it cannot force events (Ramadge and Wonham).
  *Us:* the gate is a supervisor over the tool calls, which are all controllable.
  [Wikipedia](https://en.wikipedia.org/wiki/Supervisory_control_theory).
- <a id="ocap"></a>**Object capabilities, attenuation.** Authority is an unforgeable reference;
  you pass on a narrower proxy to delegate less. UCAN states that each delegation must restate
  or attenuate. *Us:* the delegation-tree rule, proved by Z3 over envelopes.
  [Wikipedia](https://en.wikipedia.org/wiki/Object-capability_model);
  [UCAN spec](https://github.com/ucan-wg/spec).
- <a id="audit-mode"></a>**Audit mode.** Kubernetes Pod Security has enforce (reject), audit
  (record the violation, allow) and warn (tell the user, allow). *Us:* audit mode (the owner renamed shadow mode to it).
  [Kubernetes](https://kubernetes.io/docs/concepts/security/pod-security-admission/).
- <a id="tamper-evident-log"></a>**Tamper-evident log.** A log that can prove an event is still
  present and that its current state extends what auditors saw before. *Us:* receipts and run
  integrity detect a skipped step; the journal is append-only by code, not by proof.
  [Crosby and Wallach, USENIX Security 2009](https://www.usenix.org/legacy/event/sec09/tech/full_papers/crosby.pdf).

### Agents, LLMs and machine learning

- <a id="tool-use"></a>**Tool use.** A model decides when to call an external API and with
  what arguments. Toolformer taught itself this. *Us:* every tool call is what the gate checks.
  [Toolformer](https://arxiv.org/abs/2302.04761).
- <a id="react"></a>**ReAct.** The model interleaves reasoning traces with actions. It is the
  common agent loop. *Us:* sprig is such a loop. [ReAct](https://arxiv.org/abs/2210.03629).
- <a id="harness"></a>**Harness, workflow, agent.** Anthropic separates workflows (fixed code
  paths) from agents (the model directs its own tool use), built on an LLM with tools and memory.
  *Us:* daisugi is not a harness (ADR-0004); weave plans are workflows.
  [Anthropic, Building effective agents](https://www.anthropic.com/engineering/building-effective-agents).
  OpenAI's "harness engineering" post was not readable here (HTTP 403), so it is not cited.
- <a id="skill-library"></a>**Skill library.** Voyager keeps a growing library of executable
  skills and retrieves them for new tasks. Anthropic's Agent Skills load a skill by progressive
  disclosure. *Us:* pathways are skills that must fit an envelope before reuse.
  [Voyager](https://arxiv.org/abs/2305.16291);
  [Agent Skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills).
- <a id="routing"></a>**LLM routing.** A router picks a stronger or weaker model per query.
  RouteLLM learns it from preference data. *Us:* the router, and its planned learning from the
  journal. [RouteLLM](https://arxiv.org/abs/2406.18665).
- <a id="cascade"></a>**Cascade.** Try cheap models first and escalate. FrugalGPT learns which
  models to combine per query. *Us:* "cheap first, verifier escalates" (`design-router.md`).
  [FrugalGPT](https://arxiv.org/abs/2305.05176).
- <a id="knowledge-distillation"></a>**Knowledge distillation.** Train a small student model on
  a large teacher's soft outputs. *Us:* only `src/opendaisugi/lora/` (fine-tune a small model on
  journal pairs) is near this. `distiller.py` does not train a model.
  [Hinton, Vinyals, Dean 2015](https://arxiv.org/abs/1503.02531).
- <a id="program-synthesis"></a>**Program synthesis.** Find a program that meets a
  specification, often from examples. *Us:* the distiller generalizes a plan template from a
  cluster of traces and checks it on held-out traces.
  [Gulwani, Polozov, Singh 2017](https://www.microsoft.com/en-us/research/publication/program-synthesis/).
- <a id="llm-library-learning"></a>**LLM library learning.** LILO combines LLM synthesis,
  Stitch compression and model-written documentation. Sceptics find the learned functions are
  rarely reused. *Us:* dialect words must show reuse on held-out envelopes.
  [LILO](https://arxiv.org/abs/2310.19791); [Library Learning Doesn't](https://arxiv.org/abs/2410.20274).
- <a id="llm-judge"></a>**LLM-as-judge.** A strong model grades outputs. It has position,
  verbosity and self-enhancement bias. *Us:* judges only break ties in ranking, with a swap test.
  [Zheng et al. 2023](https://arxiv.org/abs/2306.05685).
- <a id="bradley-terry"></a>**Bradley-Terry model.** The chance that one item beats another is
  a function of their score gap, fitted from pairwise outcomes. *Us:* the tie-break fit in
  ranking. [Wikipedia](https://en.wikipedia.org/wiki/Bradley%E2%80%93Terry_model).
- <a id="preference-learning"></a>**Preference learning (RLHF, DPO).** Learn a reward or a
  policy from human comparisons of outputs. DPO skips the explicit reward model. *Us:* the
  owner's pairwise picks are preference labels for the router and garden.
  [Christiano et al. 2017](https://arxiv.org/abs/1706.03741);
  [InstructGPT](https://arxiv.org/abs/2203.02155); [DPO](https://arxiv.org/abs/2305.18290).
- <a id="camel"></a>**Dual LLM and CaMeL.** Extract control and data flow from the trusted
  query, so untrusted data cannot steer the program; check capabilities at tool calls. *Us:* the
  nearest answer to the information-flow gap. [CaMeL](https://arxiv.org/abs/2503.18813).
- <a id="bitter-lesson"></a>**The bitter lesson.** General methods that use computation, search
  and learning, win in the long run over built-in human knowledge. *Us:* pathways are
  specializations earned from data and dropped when stale, not hand-tuned rules (strategy notes).
  [Sutton 2019 (mirror PDF)](https://www.cs.utexas.edu/~eunsol/courses/data/bitter_lesson.pdf).

Term count, Part B: 57 entries (5 FP, 14 FM, 12 PL, 12 RTA, 14 ML); several entries combine closely
related terms.

## Part C: what the correspondence suggests we are missing

Ordered by one test: a gap where a stated guarantee rests on unproved trusted code ranks above a
gap that only adds a new class of property.

1. **No machine-checked proof of the translation layer or of definition unfolding.** Proof assistants
   rest on a small trusted kernel; Cedar's encoder is verified in Lean (per
   `research/prior-art-2026-09-26.md` §3). Ours: glob to SMT, regex to SMT and `U_D` are trusted
   code (yellow paper §7). The Lean 4 client (`clients/lean/`) re-implements only the Core stages
   (permissions, decompose, DAG, delegation safety, no SMT) and proves a few properties there
   with zero `sorry` (`clients/lean/DaisugiVerify/Theorems.lean`): an empty allowlist admits
   nothing, reported shell heads are real command words (over a small token model), and strict
   mode rejects at least what lenient mode rejects. Nothing proves the predicate encoding, the
   glob and regex translation, or `U_D`. Next: prove `U_D` total and conservative in Lean, then
   the glob encoder. Tied to `design-dialects.md` §3.2 and §3.5; `research/formal-methods-2026-09-23.md`
   open questions.
2. **Effect soundness for shell.** An effect system is proved; ours is an allowlist classifier
   (`effects.py`) behind a decomposer (`shell_decompose.py`). Two gaps: no proof that
   "undoable" calls are undoable, and predicates cannot see a shell step's write targets, so
   `must_not_modify` covers `file_write` steps only (3,393 shell steps against 490 file writes in
   the copied plans). Tied to `design-dialects.md` §3.4; ADR-0014; fail-closed shell subset in
   `formal-methods-2026-09-23.md` finding 11.
3. **Temporal and trajectory properties.** Order, history and quotas need LTLf or a counter
   fragment, checked over the journal (ContrAgent's DFA pattern). The gate is per call by design
   (yellow paper §8.2). Tied to `formal-methods-2026-09-23.md` findings 5 and 6;
   `prior-art-2026-09-26.md` "Where we are behind". New design needed.
4. **Information flow.** "Read a secret, then post it" is a hyperproperty. No per-call gate can
   stop it (yellow paper §8.3). CaMeL-style data-flow tracking is the nearest pattern. New.
5. **Proof certificates.** SMT solvers can emit proofs that an independent checker replays. We
   trust Z3's unsat. Tied to `formal-methods-2026-09-23.md` open questions. New.
6. **Envelope lint beyond vacuity.** Cedar checks always-allows, always-denies, equivalence and
   disjointness of whole policies. We check one predicate at a time (`vacuity.py`) and do not
   detect a vacuous antecedent in the Beer et al. sense. Tied to `prior-art-2026-09-26.md`
   "Where we are behind"; `formal-methods-2026-09-23.md` finding 11 (1).
7. **Datalog for delegation.** Attenuation chains in the delegation tree, and provenance of who
   granted what, are a natural fit for Datalog (terminating by design). Tied to
   `design-delegation-tree.md` (the full edge rule). New as a method.
8. **Deoptimization guards for pathways.** A JIT keeps explicit guards and a deopt path. A
   pathway has re-verification and the pruner, but no stated guard ("the assumption this pathway
   was compiled under") that fails fast. Tied to the strategy notes, "The bitter lesson". New.
9. **Recoverable states.** Simplex promises the plant stays where a safe controller can recover
   it. We only prevent actions. The deed ledger is a start (yellow paper §7). For robots this
   needs reachability and CBFs (whitepaper §7). New.
10. **Names against definitions.** LILO writes names but never checks them. Dialect names are
    chosen by models. Tied to `design-dialects.md` §5.3 and §9.1.

## Part D: rename candidates

Rule from the owner: keep the forest nouns, because they name parts: daisugi, coppice, sprig,
weave, grove, garden, pathway, tend, graft, shoot. For a generic concept, where a standard term
exists and ours is only a private synonym, adopt the standard term. These are recommendations
only.

| Our term | Standard term(s) | Keep or rename | Why |
|---|---|---|---|
| envelope | safety envelope (RTA, aviation); policy (access control); effect bound (PL) | **Keep** | "Envelope" is the RTA term. "Policy" clashes with the ML sense (the model's decision function), which is the thing we constrain. "Effect bound" is too narrow: an `Envelope` (`models.py`) carries invariants, postconditions and stakes beside the permission record, and robot bounds are not effects. Say "safety envelope" on first use. |
| subsumption | subtyping; refinement; policy subsumption; attenuation | **Keep**, and say the direction | Cedar SymCC uses "subsumption" for this query (per prior-art §3). In type terms `E_in ⊑ E_out` means the inner envelope is a subtype, a refinement of the outer (it admits fewer steps). "Refinement" alone is ambiguous, because in stepwise refinement the refined thing is the more concrete one, so do not use it bare. Always write `E_in ⊑ E_out`. Use "attenuation" for the delegation rule. |
| shadow mode | audit mode (Kubernetes); dry run | **Renamed to audit mode** (owner, 2026-09-28; done in code) | "Audit" is the standard name for "evaluate, record, allow". The rename should cover graft and pathway trial runs too, which are the same idea. The owner decides whether `--shadow` stays as a second spelling. |
| distillation, distiller | trace compilation; program synthesis from traces; skill induction | **Rename to compile / compiler** (low confidence) | In ML, distillation means training a student model (Hinton). `distiller.py` trains nothing; `lora/` does. The strategy notes already say "the garden compiles repeated work into pathways". tend keeps its name. |
| kernel | core language, core calculus | **Rename to core** | In proof assistants "kernel" is the trusted checker. Our kernel is a language, and our checker is not a small trusted kernel. The clash invites an over-claim. |
| alias | definition; named predicate; macro | **Renamed to definition** in prose (owner, 2026-09-28); the `alias` field and op stay | In PL, "alias" means another name for the same object. Logic's "extension by definition" is exactly what `aliases.py` does. |
| dialect | DSL; vocabulary; library | **Keep** | It says "one language, local words". MLIR uses "dialect" for extension modules that add new operations with new meaning (https://mlir.llvm.org/docs/LangRef/); ours add none. Note that difference once. |
| effects tiers | approval tiers; ask tiers | **Rename to ask tiers** | `effects.py` itself calls it "the tier of an ask". "Effect" suggests an effect system, which it is not. Keep "effect class" for the classifier output. |
| vacuity | always-allow/always-deny check; triviality | **Keep, with a note** | Related to model checking's vacuous pass but narrower. Document the difference (Part B). |
| fail closed | fail-safe defaults; fail-secure | **Keep** | "Fail-safe" alone means the opposite in engineering. "Fail closed" is plain and standard in security. |
| gate | reference monitor; policy enforcement point | **Keep** | Short, and the docs already say "the gate means only the allow/deny part". Cite reference monitor once. |
| ask | escalation; human-in-the-loop approval | **Keep** | Claude Code's hook API uses "ask" for the same decision (per grafts-inputs). |
| agent tree | delegation tree; attenuation chain | **Renamed to delegation tree** (owner, 2026-09-28) | The rule is about delegation, and "delegation" links to capability literature. |
| invariants (envelope field) | per-step constraint; state invariant | **Open** | In FM an invariant holds in every state of a run; ours hold per step, which is similar for per-step checks. Renaming the field is a schema break; the owner decides. |
| journal, receipts, router, gateway, ranking, verifier, predicate, plan, step types | same names are standard | **Keep** | Already standard terms. |
| daisugi, coppice, sprig, weave, grove, garden, pathway, tend, graft, shoot | n/a | **Keep** | Part names, by the owner's rule. |

Term count, Part D: 16 rows covering 33 terms.

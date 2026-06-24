# Limitations

An honest list of what opendaisugi does not do, cannot do today, or does
imperfectly. Read this before adopting. The goal is to save you an
afternoon of discovery.

## What opendaisugi is not

- **Not a sandbox.** `Supervisor` enforces permissions (shell allowlist,
  file glob bounds, network host bounds) at execution time, but it is a
  Python-level gate, not an OS-level container, seccomp profile, or
  hypervisor. If your threat model includes arbitrary native code
  escaping the Python process, opendaisugi does not help.
- **Not a hallucination detector.** If an LLM writes a plan that reads
  a file and then describes the file contents in natural-language
  output, opendaisugi cannot verify the description is faithful. It
  verifies the *plan*, not the *narration*.
- **Not a safety guarantee over LLM reasoning.** An LLM can still emit
  a plan that technically satisfies the envelope but accomplishes a
  goal you didn't want. The envelope is only as tight as the
  invariants you (or the envelope generator) declared.
- **Not a runtime policy engine for code you don't author.** opendaisugi
  verifies plans authored in its `ActionPlan` format. It will not
  inspect arbitrary Python code or shell scripts. You pass in structured
  steps; it reasons about structured steps.

## Predicate algebra boundaries

The predicate algebra (see [concepts.md](concepts.md)) can express a lot,
but not everything:

- **No recursion or fixpoint.** You cannot express "for all states
  reachable by executing this plan…" — only "for all steps in the
  written plan."
- **No unbounded quantification.** `ForallSteps` unrolls over the
  plan's concrete steps. If your plan has 5 steps, the quantifier
  becomes a conjunction of 5 clauses. Reasoning about "any plan the
  LLM could write" is handled by subsumption, not by unbounded ∀.
- **No arithmetic over symbolic strings.** You can compare strings by
  equality, regex, prefix/suffix (via allowlist semantics). You cannot
  write "length(command) < 1000" in the algebra today.
- **No cross-step dependencies beyond quantification.** "Step 3 only
  runs if step 2 wrote to file X" is not expressible as an invariant.
  Use the Supervisor's runtime hooks if you need this.

## Regex translator limits

`src/opendaisugi/regex_to_z3.py` covers a subset of Python's `re`. What
works:

- Literals, character classes, ranges (`[a-z]`)
- Alternation (`a|b`)
- Quantifiers (`*`, `+`, `?`, `{n,m}`)
- Groups (capturing and non-capturing)
- Category escapes (`\d`, `\w`, `\s`)
- Anchors `^` and `$` (full-match roundtrip)

What raises `UnsupportedRegexError`:

- Lookaround (`(?=...)`, `(?<=...)`, `(?!...)`)
- Backreferences (`\1`, `\k<name>`)
- Inline flags (`(?i)`, `(?m)`, etc.)
- Word boundaries (`\b`, `\B`)

When a regex fails to translate, the caller may choose to treat that
invariant as a soft node (see below) — but static verification cannot
prove anything about that step symbolically.

## Soft nodes

Predicates that can't be compiled (LLMCheck, unsupported regex fallbacks)
become free Z3 `Bool` variables. That has three consequences:

1. **Static proofs lose that clause.** The solver treats the soft node
   as unconstrained. If the plan's safety depends on the soft node
   being true, Stage 1 cannot prove it.
2. **Stage 2 must run.** The soft node is discharged concretely when
   the step actually runs. If your pipeline skips Stage 2, a soft
   constraint is effectively unchecked.
3. **Subsumption is optimistic for soft nodes.** In
   `envelope_subsumes`, soft-node Bools are assumed true — the most
   permissive interpretation. This is the right stance for proving
   "outer ⊨ inner" (we want to find counterexamples that don't depend
   on soft behavior), but it means soft nodes cannot block a
   delegation that would otherwise succeed.

`SubsumptionResult.unverified_invariants` surfaces any invariants
without an `expr` field. `CompiledPredicate.soft_nodes` surfaces
runtime-resolvable ones. Neither is silent.

As of v0.27.0, tautological predicates (always-true, constrains nothing) and
contradictory predicates (always-false, can never pass) are caught by Z3
vacuity detection at alias-registration time and at invariant-evaluation time.
A tautology becomes a Violation under strict mode; a contradiction is a hard
error at all stakes.

## Shell interpreter escape (v0.13.0 surface, v0.14.0 recursion)

The predicate algebra reasons about the command *string*, not about
what an interpreter does with its arguments. v0.13.0 surfaced the
risk; v0.14.0 closes it for tractable interpreters at Stage 1 verify.

**What v0.14.0 added.** ``verify()`` now parses shell steps with
``opendaisugi.interpreter_parse.parse_interpreter`` and recursively
verifies embedded commands against the same allowlist:

- ``sh -c "rm -rf /home"`` → extracts ``rm -rf /home`` → ``rm`` is
  checked against the allowlist (fails if not present).
- ``xargs rm``, ``find -exec rm {} +``, ``env PATH=/bin rm`` —
  equivalent: inner command verified at depth 1.
- Nested: ``bash -c "sh -c 'rm'"`` verified at depth 2.
- Recursion depth capped at 4 to bound pathological nesting.

**What remains under policy.** Opaque interpreters (``python``,
``perl``, ``ruby``, ``node``, ``awk``, ``sed``, ``make``) interpret a
different language than shell — their payloads cannot be parsed as
shell commands. ``Envelope.shell_interpreter_policy`` governs how
verify handles them:

- **`"surface"` (default).** Opaque interpreters pass verify; at
  subsumption time they are flagged in
  ``SubsumptionResult.unverified_invariants`` as
  ``shell_interpreter:<name>``.
- **`"strict"`.** Verify rejects any opaque interpreter invocation
  (we can't prove anything about its payload). Subsumption rejects
  when the inner allowlist admits any interpreter, tractable or not,
  with ``outer_violation == "shell_interpreter_policy"``.
- **`"allow"`.** Opaque interpreters pass both verify and
  subsumption without surfacing — the user has considered them and
  accepts residual risk.

**Subsumption still uses the surface model.** Recursion happens at
verify time against concrete plans. Subsumption (envelope-vs-envelope)
can't recurse because the command strings are symbolic — Z3 can't
parse an unknown string as a shell command. Subsumption continues to
flag interpreter presence in either allowlist per the v0.13 policy.

The recognised interpreter list is ``opendaisugi.models.SHELL_INTERPRETERS``
(sh/bash/zsh/xargs/find/env/python/perl/ruby/node/make/awk/sed/…).
Tractable interpreters (shell ``-c`` / ``xargs`` / ``find -exec`` /
``env``) are parsed. Opaque ones fall through to policy. Unlisted
binaries do not trigger the policy; extend
``src/opendaisugi/interpreter_parse.py`` downstream if you discover
more.

## Z3 practical limits

- **Decidability.** Z3 is complete for linear integer arithmetic and
  most string theories but not all combinations. A particular envelope
  + plan may return `unknown`. We raise `VerificationTimeout` when
  that happens; we do not approve.
- **String alphabet (v0.28.4+).** The regex-to-Z3 translator uses the
  Basic Multilingual Plane (`0x00-0xFFFF`) minus newline as the
  alphabet for `.` and negated character classes. This matches
  Python's `re` over BMP. Supplementary-plane codepoints
  (`0x10000-0x10FFFF` — emoji, ancient scripts, CJK extension B+) are
  outside Z3's string sort and silently disappear from membership
  checks. If a regex authored in the envelope or used at runtime
  contains supplementary-plane characters, results may diverge from
  Python's. Realistic envelope-authoring corpora are BMP-only; the gap
  is documented rather than fixed because Z3 itself rejects the wider
  range.
- **Timeouts.** Default Z3 timeout is 500ms for `verify`, 2000ms for
  `envelope_subsumes`. Configurable. Complex plans with many steps
  and dense invariants can hit the timeout; increase as needed.
- **String-theory performance.** Regex over long symbolic strings is
  the most expensive operation. If you author invariants with many
  disjunctive regex clauses, subsumption can slow noticeably.

## Feature maturity caveats

See [feature-status.md](feature-status.md) for the full matrix. Summary:

- **Core (production-candidate):** Envelope, predicate algebra,
  `verify`, `Contract`, `verify_delegation`, `Supervisor`, `Journal`.
- **Post-adoption depth (functional, lightly used):** Distiller,
  PathwayStore, Gardener (A/B/prune/merge), Tier-1 routing, MCP
  server, LoRA training-data pipeline.
- **Experimental:** MuJoCo-backed robotics executor, pathway
  export/import portability.
- **Planned, not shipped:** arithmetic-over-paths operator in the algebra.

## Platform constraints

- **Python 3.12+.** We use match statements and newer typing features
  throughout.
- **Z3 solver.** Required. `uv add opendaisugi` pulls `z3-solver`
  automatically but the solver is a native dependency.
- **LLM API for envelope generation.** `generate_envelope` calls out
  via `litellm` / `instructor`. Set `ANTHROPIC_API_KEY` (or equivalent),
  use a low-stakes permissive envelope if you don't want network
  calls, or set `OPENDAISUGI_LLM_BACKEND=claude-code` (v0.12.0+) to
  route through an existing Claude Code install instead of an API key.

## ClaudeCode LLM backend (v0.12.0+)

- Each call spawns a fresh `claude -p` subprocess (~0.5-1 s overhead vs
  direct API). Acceptable for envelope generation and distillation;
  noticeable for LLMCheck in tight loops. No batching.
- The subprocess inherits the parent environment, including
  `ANTHROPIC_API_KEY` if set. Unset it beforehand for strict API-key
  isolation.
- Schema-augmented prompts for structured output can be large (the
  `Envelope` schema is ~2 KB). Claude honors "respond with ONLY JSON"
  reliably in practice; pathological outputs surface as
  `EnvelopeGenerationError`.
- **Optional extras** are heavy: `torch`/`peft`/`bitsandbytes` for
  `[lora]`, `mujoco` for `[robotics]`, `sentence-transformers` for
  `[search]`. Install only what you use.

## Planned, not shipped

These are named in specs and tracked in CHANGELOG but do not work today:

- **Arithmetic-over-paths** — e.g. `sum(step.tokens) < budget`. The
  v0.15 `length_range` operator covers the common case (string/list
  length bounds); arbitrary numeric arithmetic across paths is still
  future work.
- **Real-time plan refinement loops beyond basic CEGAR** — v0.2.0
  shipped a one-shot refinement; multi-round is future work.

If any of the above is a hard requirement, opendaisugi is not ready
for you yet.

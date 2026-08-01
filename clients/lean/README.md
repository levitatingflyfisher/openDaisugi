# daisugi-verify (Lean 4) — the executable-spec conformance client

An independent reimplementation of the openDaisugi verifier **Core**
(permissions, decompose, dag, delegation-safety — not the SMT/predicate/z3/
skill-subsumption "Full" stages), built to the differential-testing spec in
`docs/spec/conformance.md`. Where the Go client leans on a mature
production shell parser (`mvdan.cc/sh/v3`) and Rust/TS lean on tree-sitter
itself, this client is deliberately the opposite extreme: a hand-rolled
recursive-descent subset parser with **no grammar dependency at all**, no
solver, and a decompose match rate that is expected to trail the other
clients (see "Scorecard" below — it does, honestly). What this client adds
that the others don't: **machine-checked proofs**. Every soundness claim
below is a Lean theorem, not a passing test.

## Build & run

```sh
export PATH="$HOME/.elan/bin:$PATH"   # Lean 4.33.0 (lean-toolchain)
cd clients/lean
lake build                 # builds `conform` (the wire-protocol executable)
lake build DaisugiVerify.Theorems   # separately type-checks the proofs (task 6)
lake build test_semantics  # the fixture-regression executable
```

No external dependencies — no mathlib, nothing but `Lean.Data.Json` from
core. This was a deliberate house-rule constraint (build weight on a
memory-constrained box); every lemma used in the proofs (`List.mem_flatMap`,
`List.mem_cons`, `List.mem_cons_self`, `List.mem_cons_of_mem`,
`List.any_nil`, `List.Subset`, `List.flatMap_cons`, ...) was confirmed
present in Lean 4 core (`Init.Data.List.*`) by reading the shipped sources
under `~/.elan/toolchains/leanprover--lean4---v4.33.0/src/lean/Init/Data/List/`
before using it — nothing here is a mathlib tactic in disguise (`set` was
tried and rejected for exactly this reason: it's a Mathlib tactic, not
core; see `Theorems.lean`'s use of `generalize`/explicit lemmas instead).

`conform` speaks the wire protocol directly: one case JSON per line on
stdin, one verdict JSON per line on stdout, flushed per line
(`Main.lean`'s `loop`, `DaisugiVerify/Wire.lean`'s `handleLine` — the
latter never throws, so a malformed/unprocessable case yields an
`{"id","error"}` verdict and the stream continues, per spec).

```sh
# from the repo root
CUDA_VISIBLE_DEVICES="" uv run daisugi conformance run \
    .opendaisugi/conformance/corpus.jsonl --client "clients/lean/.lake/build/bin/conform"

CUDA_VISIBLE_DEVICES="" uv run daisugi conformance bench \
    .opendaisugi/conformance/corpus.jsonl --client "clients/lean/.lake/build/bin/conform"
```

`lake build`'s `-j`/parallelism-cap flag was not found for this Lake
version (5.0.0-src; `-j2` errors as an unknown short option) — builds ran
sequentially by construction (one `lake build` invocation at a time, no
backgrounded parallel builds), but no explicit job-count cap was ever
successfully applied. Noted honestly as an unresolved house-rule item
rather than silently dropped.

## Module map

| File | Contents |
|---|---|
| `DaisugiVerify/Basic.lean` | Char classification, `pyStrip` (Python `str.strip()`, since `String.trim` is deprecated here), `fnmatch`-style `globMatch`. |
| `DaisugiVerify/Semantics.lean` | Direct ports of `verify.py`'s pure functions: `headAllowed`, `normpath`, `matchGlobOne`/`matchSegs` (native left-anchored `/`-aware file-scope matcher — ported 2026-08-21 when the oracle replaced the old `PurePosixPath.match`-based approach to fix F-1/F-2 in ADJUDICATIONS.md), `pathMatchesAny`, `hasMetachar`, `extractHead`, `resolveStrict`. |
| `DaisugiVerify/InterpreterParse.lean` | Port of `interpreter_parse.py`: `shlexSplit`/`shlexQuote`, interpreter classification, `parseInterpreter`. |
| `DaisugiVerify/Models.lean` | JSON decode of `Step`/`ActionPlan`/`Permission`/`Envelope` (Core-scoped fields only). |
| `DaisugiVerify/Dag.lean` | Port of `dag.py`: duplicate-id / missing-dep / cycle checks. |
| `DaisugiVerify/ShellDecompose.lean` | The fail-closed subset parser (task 4). |
| `DaisugiVerify/Verify.lean` | Verify-Core orchestration: delegation-safety → permissions → dag, short-circuiting. |
| `DaisugiVerify/Wire.lean` | The wire-protocol dispatcher, `handleLine`. |
| `DaisugiVerify/Theorems.lean` | Task 6 — the proofs. See below. |
| `Test/Semantics.lean` | Fixture-driven regression against `clients/fixtures/semantics.json` (134 cases). |
| `analysis/classify.py`, `analysis/false_accept_detail.py`, `analysis/bucket_fa.py` | Dev-loop scripts (not Lean, not gated) used to iterate the parser against the full corpus faster than the capped `daisugi conformance run` report. `bucket_fa.py` only buckets the "superset_heads" false-accept shape (both sides `ok=true`, different heads); that shape's count is 0 against the current corpus, so it currently prints an empty table — the "40+ distinct buckets" finding it produced is a historical result against the pre-fix corpus, preserved in `clients/ADJUDICATIONS.md`'s original L-1 entry. |

## Scorecard

> **Updated after the parser-coverage campaign.** The forensic sections below
> (the "77.14% / 116 false-accepts" box and its root-cause analysis) are the
> record of an EARLIER state and are kept as history. The decompose subset
> parser was since extended — heredocs, `if`/`while`/`until`/`for`/`select`,
> `${…}`/`$(())`/backtick expansions, assignment-value substitutions, the `!`
> prefix — behind a gate (`clients/gate.py`) that holds genuinely-unsafe cases
> at **zero**. Current standing:
>
> | Kind | Matched | Total | % |
> |---|---:|---:|---:|
> | decompose | **10,770** | 11,089 | **97.1%** |
> | verify (Core slice) | 327 | 327 | 100% |
> | **total** | **11,097** | 11,450 | **96.9%** |
>
> The residual is a long tail: `case`/brace/process-substitution false-rejects
> (safe), plus 136 *verified divergences* (valid bash the oracle's tree-sitter
> can't parse, Lean's heads independently confirmed complete) that can never
> become matches because the oracle refuses them either way. The three theorems
> stayed `sorry`-free throughout — they range over the head model, not the
> production scanner. See [`docs/client-diversity.md`](../../docs/client-diversity.md)
> for the full campaign.

### Historical scorecard (reference box, 2026-08-21, re-aligned to the fail-closed oracle)

Corpus: 11,450 cases (11,089 decompose, 361 verify),
`.opendaisugi/conformance/corpus.manifest.json`. This corpus was
regenerated after an oracle-side fix (see "Findings" below): the oracle
now fails closed on the tree-sitter-bash statement-fusion artifact this
client had already independently confirmed (see the superseded "132
false-accepts" scorecard in git history / the previous revision of this
file) instead of silently accepting the fused, wrong output. The oracle
self-checks 11,450/11,450 against itself.

**Zero crashes (task 2 gate): CONFIRMED.** Every one of the 11,450 cases
gets a verdict line; 0 bad-JSON output lines, 0 missing verdicts, 0
client-side `error` verdicts (`analysis/classify.py`'s header line).

**Whole-corpus decode (task 1 gate): CONFIRMED** — same evidence: nothing
in the corpus fails to decode into a `Step`/`Plan`/`Envelope` and produce
a verdict.

| Kind | Matched | Total | % |
|---|---|---|---|
| decompose | 8,555 | 11,089 | 77.14% |
| verify (non-SMT / Core slice) | 327 | 327 | **100.00%** |
| verify (raw total, incl. out-of-scope Full-stage cases) | 327 | 361 | 90.58% |

### verify — task 5 gate: MET

The non-SMT slice (every verify case whose expected violations are drawn
only from `{permissions, dag}` — i.e. genuinely Core-scoped, no
`predicate`/`z3`/`delegation` stage in the expectation) matches **327/327
(100%)**. The raw 361-case total includes 34 cases whose oracle verdict
turns on a Full-profile stage this client doesn't implement by design
(predicate-algebra, Z3 envelope subsumption — including the corpus's new
F-3 regression tests, which are predicate/z3-stage and so already fall
outside this slice; re-confirmed the stage set found in the new corpus's
verify expectations is unchanged: `{dag, delegation, permissions,
predicate, z3}`). The same scope note as before still holds and was
re-verified against the current oracle source: `stage="delegation"` is
emitted **exclusively** by `check_skill_delegations` (`verify.py` lines
658/683), the Z3-backed skill-subsumption check — never by the actual
Core delegation-safety gate (`_check_delegation_safety`, which emits
`stage="permissions"`, matched exactly by `checkDelegationSafety` in
`Verify.lean`). So every `stage="delegation"` expectation is
out-of-Core-scope by construction, not an ambiguous case to adjudicate.

### decompose — task 4 gate: NOT MET (116 false-accepts, root-caused, new mechanism)

`shell_allow_decomposition` cases: simple commands; `;`/`&&`/`||`/`|`/`|&`
separators; single/double-quoted strings (no expansion — a bare `$VAR`
inside a double-quoted string correctly downgrades the piece to
non-literal); comments; env-assignment prefixes; literal redirects
(oracle's op tables); `$( )` substitution recursion when the inner text is
itself simple; reserved words (`if`/`for`/`while`/...) and headless
keywords (`[`, `[[`, `export`/`declare`/`typeset`/`readonly`/`local`/
`unset`) handled per tree-sitter-bash's actual node classification, not
treated as literal command heads; backslash-newline line continuations
treated as inter-token whitespace. Everything else (backticks, `${}`,
process substitution, heredocs, subshells, arithmetic) → `ok=false`,
fail-closed as required.

| | Count |
|---|---|
| matched | 8,555 |
| false-reject (oracle=true, we say false — safe, expected: subset parser rejects more than it should) | 2,418 |
| **false-accept (must be zero per the kickoff plan)** | **116** |

**Correction to an earlier draft of this section: the oracle fix did NOT
convert this client's previous false-accept bucket into matches — that
claim was written before it was checked and turned out to be false.**
This client's decompose logic never rejected these multi-line-pipe
scripts; it has always parsed each newline as an ordinary statement
separator (correct real-bash/POSIX behavior) and reported the *full*
head list, which is exactly why the old oracle (silently fusing some of
those statements into fewer, wrong heads) mismatched against it as
"superset_heads." That parsing behavior is unmodified in this revision.
Spot-checked three commands recognizable from the old bucket by content
(re-fetched fresh from the regenerated corpus's JSON, not hand-retyped,
to rule out transcription error) — `310f48f3073fdbac` (the same Sundial
flutter-test transcript investigated in the original L-1 entry),
`5be4aea55225f77b`, and `73fbafb2360b80a9` — and all three are **still
mismatches** against the current client build: the client still reports
`ok=true` with the fuller head list, and the new oracle now rejects the
same input outright instead of fusing it. The *symptom* changed shape
(wrong-heads-both-accept → oracle-rejects-client-accepts) but the
underlying disagreement did not resolve for this input shape. The 132→116
numerical change is not evidence of cases being fixed — the corpus was
regenerated with a different sample (11,089 vs. 13,084 decompose cases,
not a strict subset of the old one), so the two counts aren't a
before/after of the same population. Both counts stand as independent,
honestly-reported measurements against their own corpus, not as a
before/after story.

**All 116 remaining false-accepts are a NEW, narrower slice of the SAME
underlying tree-sitter-bash statement-fusion phenomenon — investigated,
majority-characterized, not fully closed.** Every one of the 116 is
rejected by the oracle for the identical reason string, `"ambiguous shell
(bare newline inside command — parser statement fusion)"` (confirmed by
calling `shell_decompose.decompose_command` directly on each of the 116
command texts — read-only oracle probing, no oracle code touched). This
is now a **structural, local** detector in the oracle
(`_command_has_bare_newline` in `shell_decompose.py`: walk every
`command` AST node, reject if its own span contains a raw newline outside
a quote/heredoc/substitution/backslash-continuation) rather than the
purely emergent, whole-input-dependent symptom this client bisected
before — a meaningfully different, more tractable starting point.

Systematic differential probing against the live oracle (20+ synthetic
cases sweeping pipe-stage count, redirect position, and follow-on
statement shape) found a **two-condition sub-rule that explains 87 of the
116** (verified against the real corpus, not just synthetic probes, by
walking each flagged case's real AST): a pipeline of **≥3 stages** whose
**last stage carries no redirect of its own**, immediately followed
(across a raw newline) by a statement that contains a redirect operator
**anywhere** before its own natural end — reject. Mechanistically this
reads as tree-sitter's GLR conflict resolution preferring to keep
extending the current parse into the grammar's more specific
`redirected_statement` production (`pipeline redirect`) when a redirect
eventually appears in that extension, and correctly stopping when no
redirect would ever be found. This sub-rule was validated (stage count ≥
3 AND requires flattening through nested `pipeline`/`redirected_statement`
wrappers, since a fused chain can nest several deep) but **the remaining
29 of 116 falsify it as a complete characterization** — they include cases
with `stage_num` of 1 and 2 with no downstream redirect detected. That
"no downstream redirect detected" reading has a caveat worth stating
precisely rather than glossing: the validation script's redirect-detection
only flattens through `pipeline`/`redirected_statement` nesting, not
through `list` (`;`/`&&`/`||`) nesting, so a redirect sitting past a
top-level separator inside the following statement would be invisible to
it. The 29 are therefore evidence of *either* a second, uncharacterized
triggering mechanism *or* an incomplete redirect-detection pass in the
validation itself — undetermined, not chased further.

**No client-side rule was implemented.** A rule good for only 87/116 would
still leave the gate unmet, and per the same reasoning as the prior
revision's finding, retrofitting a *partial*, only-mostly-validated
heuristic — cross-newline lookahead, nested-wrapper pipe-stage counting,
"does a redirect appear later in the flattened chain" — is real parser
complexity added on top of an admittedly incomplete model, for well under
one point of corpus match rate (87/11,089), on a client whose stated
deliverable is the proofs, not decompose coverage. Reported as an
**honestly unmet gate**: false-accepts = 116, mechanism identified and
partially (not fully) characterized, no defensive fix attempted. See **L-1**
in `clients/ADJUDICATIONS.md` for the full investigation and the specific
probe results.

## Bench

```
cases=11450 repeat=1 p50=0.098ms p95=0.243ms p99=0.373ms throughput=8640 cases/s
```

## Theorems (task 6 — the point of this client)

`DaisugiVerify/Theorems.lean`, 8 declarations, **zero `sorry`**. Confirmed
via `#print axioms` on every theorem: each depends only on Lean's standard
core axioms (`propext`, `Classical.choice`, `Quot.sound` — never
`sorryAx`).

**(a) `headAllowed_nil : ∀ h, headAllowed h [] = false`** — an empty
allowlist admits nothing. Trivial (`List.any_nil`), but it's the base case
every permission-surface check in `Verify.lean` structurally relies on.

**(b) subset-parser soundness — `parseSimple_sound`, narrowed per the
plan's own escape hatch.** `ShellDecompose.lean`'s production parser is
declared `partial def` (necessarily — its mutual recursion across ~15
functions over a shared `Array Char` cursor doesn't structurally decrease
in a way Lean's termination checker accepts). `partial def` generates no
equation lemmas: Lean cannot unfold it in a proof the way it can unfold an
ordinary structurally-recursive `def`, so no property of it — this one or
any other, regardless of how the statement is phrased — can be proved by
induction. That, not merely "no token list," is the real reason the
production scanner isn't the induction target; reshaping it into a
structurally-recursive form this late, just to make it a proof target,
was explicitly out of scope.

So this theorem is proved over a small, self-contained token model built
for exactly this purpose (`Tok := word String | sep`, a structurally-
recursive `parseSimpleAux`/`parseSimple`). This model is a genuine
simplification, not a full match to the production parser: it captures
the shared *head/argument state machine* (a boundary token starts a new
command; the next word is that command's head; further words up to the
next boundary are discarded arguments) but deliberately omits the
production parser's keyword-classification layer — `isReservedWord` and
`isHeadlessKeyword`, the two rules (`for`/`if`/`while`/...; `[`/`[[`/
`export`/`declare`/...) that make the production parser's actual "first
word after a boundary" frequently *not* a head at all. The theorem proves:
every head `parseSimple` returns for a tokenized command line names a
`word` token that literally occurred in that input — never synthesized,
never pulled out of an argument — for that shared core state machine,
proved by structural induction on the token list, generalizing the
in-command flag. The production scanner's actual, fuller coverage
(quoting, redirects, substitutions, reserved words, headless keywords) is
checked *empirically* against the 11,089-case decompose corpus (above),
not proved; this theorem proves the core tokenization invariant both
parsers build on is sound, as a machine-checked fact rather than a test.

**(c) strict-monotonicity — `strict_monotone`, the stretch goal, proved
in full (not narrowed).** `runVerify`'s `strictOpt := some false` run never
rejects anything that `strictOpt := some true` doesn't also reject —
strict mode only ever adds violations. Built from three supporting lemmas:
`checkPermissions_one_subset` (per-step: `checkPermissions`'s match
dispatches on `s.type`; every arm but the final catch-all is syntactically
independent of `strict`, so `split <;> simp_all` closes it after exposing
the one arm that isn't), `flatMap_subset_of_pointwise` (a general-purpose
fact: a pointwise `List.Subset` lifts through `List.flatMap`, via
`List.mem_flatMap`), and `checkPermissions_subset` (composing the two to
the whole-plan level). The final theorem (`runVerify_shape_mono` +
`strict_monotone`) case-splits on which of the three verify stages first
produces a violation under lenient mode and shows the same-or-earlier
stage produces one under strict mode too — `checkDelegationSafety` and
`checkDag` don't read `strict` at all, so only the permissions stage's
behavior needed the subset argument.

## Findings filed against the oracle

See `clients/ADJUDICATIONS.md`, section **L-1** (2026-08-21, updated
2026-08-21 for the oracle's fail-closed fix): originally an independent
confirmation, from a second parser implementation, of the Go client's
**G-4** tree-sitter-bash GLR span-fusion bug. The oracle was subsequently
patched to fail closed on that exact artifact (`_command_has_bare_newline`
in `shell_decompose.py`) and the corpus regenerated. This client's own
decompose logic for the affected input shape (multi-line pipe scripts) is
unmodified and was never rejecting them — spot-checking three commands
recognizable from the old 132-case bucket confirms they are still
mismatches now, just recategorized (the client still says ok=true with
the fuller head list; the oracle now rejects instead of fusing). The
corpus regeneration surfaced a **narrower, structurally different**
residual of the same underlying tree-sitter phenomenon (116 false-accepts
against the new corpus, ~87 fitting a validated two-condition sub-rule,
~29 not); see L-1's update and the
"decompose" scorecard section above for the full investigation. No other
new oracle bugs were found; the verify-side "delegation" stage-name scope
note above is a documentation clarification, not an oracle bug.

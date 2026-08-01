# daisugi-verify (Go) — the independent-parser conformance client

An independent reimplementation of the openDaisugi verifier core, built to
the differential-testing spec in `docs/spec/conformance.md`. Where the
other slate clients are expected to lean on tree-sitter (the oracle's own
parser family), this one deliberately doesn't: shell decomposition is built
on **[`mvdan.cc/sh/v3`](https://pkg.go.dev/mvdan.cc/sh/v3)**, a from-scratch
Go bash/POSIX-shell parser with no code or grammar lineage shared with
tree-sitter-bash. Every place the two parsers disagree is a genuine,
independently-discovered finding about tree-sitter-bash's grammar — logged
in `clients/ADJUDICATIONS.md` (entries G-1 through G-7), not papered over.

## Build & run

```sh
cd clients/go
source .goenv.sh   # GOFLAGS=-p=2, GOPATH/GOCACHE/GOMODCACHE under this dir
go build -o conform ./cmd/conform
```

`conform` speaks the wire protocol directly: one case JSON per line on
stdin, one verdict JSON per line on stdout, flushed per line, logs to
stderr only (see `docs/spec/conformance.md`).

```sh
# from the repo root
CUDA_VISIBLE_DEVICES="" uv run daisugi conformance run \
    .opendaisugi/conformance/corpus.jsonl --client "clients/go/conform"

CUDA_VISIBLE_DEVICES="" uv run daisugi conformance bench \
    .opendaisugi/conformance/corpus.jsonl --client "clients/go/conform"
```

Requires `z3` on `PATH` (`~/.local/bin/z3`, v4.16 on the reference box) for
the Full profile — see "The solver" below. Go 1.25; module deps resolve
through the default proxy.

## Test suite

```sh
go test ./...
```

The corpus (`.opendaisugi/conformance/corpus.jsonl`) is real, uncommitted,
and machine-specific (embeds local paths) — it's the integration gate, not
something `go test` can depend on. Everything committed here is a
regression table hand-derived from probing the live oracle
(`clients/go/probe_gen.py`, `probe_shlex.py`) or from a real corpus
mismatch, so the suite stays meaningful in a fresh checkout or CI:

- `fixtures_test.go` — the shared 134-case `clients/fixtures/semantics.json`
  fixture (head-allowlist glob matching, path-scope glob matching, shell-
  head extraction, the metachar gate, `parse_interpreter`, `resolve_strict`).
- `shlex_test.go` — a 56-row differential fixture against Python's real
  `shlex.split(s, posix=True)` / `shlex.quote`.
- `decompose_test.go` — a hand-curated table covering ordinary decompose
  behavior plus every adjudicated tree-sitter-bash disagreement (G-1
  through G-6) by name.
- `verify_test.go` — permissions stage, DAG checks, delegation safety,
  agentic-step capability checks.
- `full_profile_test.go` — envelope self-consistency, plan-vs-envelope,
  vacuity classification, predicate-algebra invariants (incl. the
  unresolved-alias and strict-opaque-invariant paths), and skill-envelope
  subsumption. Skips itself (`t.Skip`) if `z3` isn't on `PATH`.
- `astdump_test.go` — not a correctness test; dumps the mvdan/sh AST shape
  for tricky constructs (`TestASTDump -v`). Kept because it's how every
  adjudication in this file was actually derived — useful ground truth for
  whoever touches the decompose port next.

## Conformance gate (reference box, 2026-08-21)

Corpus: 11,450 cases (11,089 decompose, 361 verify) — regenerated
2026-08-21 after an oracle fix (see below); same generation the spec
quotes (`.opendaisugi/conformance/corpus.manifest.json`).

| Kind | Matched | Total | % |
|---|---|---|---|
| decompose | 11,033 | 11,089 | 99.50% |
| verify (Core + Full) | 361 | 361 | 100.00% |
| **total** | **11,394** | **11,450** | **99.51%** |

**verify is 100%** — every permissions/DAG/delegation-safety case (Core)
and every predicate-algebra/envelope-self-consistency/plan-vs-envelope/
skill-subsumption case (Full) matches the oracle.

**The oracle changed mid-campaign, and this client changed with it.**
`shell_decompose.py`'s tree-sitter-bash parser has a real bug: certain
multi-line scripts trigger a GLR statement-fusion artifact (`c1\nd1` can
parse as ONE `command` node, head `c1`, argument `d1` — the oracle used to
silently accept this, fusing the two statements and dropping `d1`'s own
head from the check, which is fail-OPEN in a runtime-assurance verifier).
The oracle was fixed 2026-08-21 (`_command_has_bare_newline`) to detect
this at the AST level and fail closed instead — reason `"ambiguous shell
(bare newline inside command — parser statement fusion)"` — and the corpus
was regenerated (13,387 → 11,450 cases). This client's decompose port
originally special-cased the OLD fuse-and-accept behavior (head
suppression); that's now wrong, since the oracle no longer emits a fused
acceptance. It was reframed as a fail-closed REJECT predicting the SAME
trigger shapes, since mvdan parses these scripts correctly and has no
fused node of its own to read the signal off directly.

**decompose's 56 residual mismatches are accounted for, not silently
eaten** — see `clients/ADJUDICATIONS.md`'s G-4/G-4b for the full
derivation (three refinement rounds, each validated against real corpus
deltas) and honest limits:

- Of the corpus's 602 total decompose rejections: 404 are pre-existing
  "non-literal head/redirect target" and 39 are pre-existing "malformed
  shell" — both handled with **zero mismatches**. The remaining 159 are
  the new "ambiguous shell" (G-4b) rejections — the entire residual comes
  from predicting this one bug.
- **32 of 159** true G-4b rejections are missed (this client wrongly
  ACCEPTS) — the dangerous direction for a runtime-assurance verifier,
  since a missed rejection means a head hiding inside a fused span isn't
  checked. 79.9% recall on this category, prioritized over precision
  across three refinement rounds.
- **24** (of ~10,487 accepted cases) are wrongly REJECTED by this client's
  over-eager prediction — a precision/availability cost, not a safety
  cost, under fail-closed design. 99.77% precision.
- Direct oracle probing found the rule's ceiling is a real, provable
  limit, not just insufficient effort: a letter-substituted MINIMAL REPRO
  of one false-positive case's exact structure reliably fires, while the
  real, longer script with the identical abstracted shape does not —
  tree-sitter's GLR disambiguation depends on context no local rule can
  see. The Lean client's independent, differently-derived rule hit the
  same wall from a different angle (explains 87/116 of ITS residual,
  explicitly leaves 29 as "a second, distinct triggering mechanism... not
  characterized" — see `clients/ADJUDICATIONS.md`'s L-1 follow-up).
  Further bisection was judged non-productive and deliberately stopped.
- **1 further, uncharacterized `[`-bracket-test interaction**
  (`[ "$a" \< "$b" ]`, backslash-escaped `<`) — a single occurrence, not
  bisected to a general rule, left as an honest gap (this one predates
  the oracle fix and is unrelated to G-4/G-4b).

Every other mismatch found during porting — six DISTINCT tree-sitter-bash
grammar gaps/bugs (G-1 `<>` unsupported entirely; G-2 the assignment-word
grammar accepts a leading digit that real bash/POSIX reject; G-3 `[` gets a
dedicated non-head-producing node mvdan doesn't have; G-5 `time` has no
grammar production at all; G-6, two distinct heredoc/redirect/list-operator
interaction bugs), one genuine Go-vs-Python regex-dialect gap (G-7,
`\uXXXX` — RE2 doesn't support it; **fixed**, not merely matched, since
it's not a frozen oracle behavior), and one Go-side traversal-order bug
caught by the corpus and fixed during porting (head-before-assignment-
substitution ordering) — is now matched exactly.

Every adjudication above was found by running the full corpus, clustering
mismatches by shape (`clients/go/analyze_mismatches.py`), and bisecting the
largest bucket down to a minimal repro against the live oracle — not
theorized in advance. G-3 (the `[`-bracket-test node) was by far the
highest-leverage single fix, cutting the initial "heads differ" bucket by
more than half in one change; G-6 (heredoc/redirect/pipe interactions)
was the next largest. The traversal-ordering bug and G-7 (the regex-dialect
gap) were each caught by exactly one corpus case.

## Bench (reference box, 2026-08-21)

```
cases=11450 repeat=1 p50=0.037ms p95=0.110ms p99=0.476ms throughput=16751 cases/s
cases=11450 repeat=3 p50=0.044ms p95=0.152ms p99=0.580ms throughput=11641 cases/s
```

Over the pipe (full IPC round trip, `daisugi conformance bench --client`),
matching the spec's own bench protocol. Process startup is ~2ms (`echo -n
"" | ./conform`) — no interpreter/import cost, consistent with
`docs/spec/conformance.md`'s claim that a compiled client's win is startup
and the long tail, not per-case verification cost (already sub-millisecond
in the Python oracle). These numbers are faster than the Python oracle's
own pipe baseline (p50 0.183ms) quoted in the spec, run on the same class
of box — expected, and not the headline: the interesting number is p99
(0.476–0.580ms), which stays low even though a substantial share of the
361 verify cases reach the envelope-self-consistency/plan-vs-envelope
solver stage (each up to two `z3 -in` round trips), a further subset of
those also runs vacuity classification (two more round trips per enforced
predicate item), and some run skill-envelope subsumption — all against the
SAME persistent z3 process, since `--repeat` runs share it across the
whole bench.

## Architecture notes

**Decompose** (`decompose.go`) is a hand-rolled recursive-descent walker
over `mvdan.cc/sh/v3/syntax`'s AST, NOT a transliteration of
`shell_decompose.py`'s tree-sitter node-type dispatch — the two parsers'
node shapes don't correspond closely enough for that to make sense (e.g.
mvdan's `Redirect.Word`/`.Hdoc` split, `CallExpr.Assigns` vs `.Args`, no
node at all for `[` or `time`). It independently re-derives the same
fail-closed contract: a literal head is exactly one `*syntax.Lit` word
part; a literal redirect target is a `Lit`, a `SglQuoted`, or an
all-`Lit` `DblQuoted`; anything else (a lone `ParamExp`/`CmdSubst`/
`ProcSubst`, or ≥2 word parts) is non-literal and rejects; substitutions
(`CmdSubst`, `ProcSubst`, inside `DblQuoted`, inside heredoc/herestring
bodies, inside assignment values) are walked regardless of accept/reject
outcome-in-progress, surfacing nested heads; first reject wins,
pre-order, matching the oracle's short-circuit exactly.

**The solver.** Per `docs/spec/conformance.md` ("emitting SMT-LIB2 text and
invoking a solver binary... never by binding a solver API"), every Full-
profile check goes through `z3client.go`'s `Z3Client` — **one persistent
`z3 -in` subprocess**, lazily started on first use, with every query
isolated by `(push 1)`/`(pop 1)` and resynchronized by a unique `(echo
"...")` marker read after every query (z3 does NOT abort a session on a
malformed command — e.g. a duplicate `declare-const` in one scope prints an
`(error ...)` line and keeps going, which would silently shift the stdout
read cursor for every later query sharing the process; the marker makes
that class of bug loud and self-correcting instead of a silent, delayed
wrong answer several cases later — this is how G-7 was actually caught and
diagnosed, mid-port). Honest accounting of what routes through it and why:

- **Envelope self-consistency / plan-vs-envelope** (`envelope_z3.go`) — go
  through the solver for spec fidelity, even though both are three trivial
  pinned-equality/inequality constraints trivially decidable by direct
  inspection (and the Python oracle's own encoding is exactly that: fixed
  literals asserted as Z3 equalities, not a search over anything). One
  `z3 -in` round trip each.
- **Vacuity (tautology/contradiction) classification** (`vacuity.go`) —
  genuinely needs a solver: it's asking "is this predicate satisfiable /
  is its negation satisfiable" over a fully symbolic step. Two round trips
  per predicate carrying an `expr`. **`Matches`/`NotMatches` always compile
  to a free ("soft") Bool here** — `regex_to_z3.py` (Python `re` →
  Z3 regex-theory AST) is not ported. This is exactly the oracle's own
  fallback for regex shapes its translator can't handle, and is *sound*
  for tautology/contradiction classification whenever the regex isn't the
  sole source of the predicate's determinism — true of every predicate
  invariant/postcondition in the current corpus (verified: none of the
  vacuity-relevant `useless`/`always_false`-style cases involve a regex
  node at all). A predicate that's tautological/contradictory purely
  because of specific regex content would be under-classified as
  `non_trivial` by this client; no such case exists in the corpus.
- **Skill-envelope subsumption** (`subsumption.go`) — the one place a
  solver is load-bearing for the actual *answer*, not just the interface:
  proving `outer ⊨ inner` over shell-command admission (`str.prefixof`/
  `str.contains` in Z3's string theory) and glob-pattern containment.
  Robot-capability and interpreter-policy short-circuits, plus network-host
  scope, are structural (no solver).
- **Ground predicate evaluation** (`evaluate.go`, `predicate.go`) — the
  actual pass/fail check `verify()` runs per plan is the oracle's own
  "fast path" (`predicate_z3.evaluate_predicate`), plain Go over
  JSON-decoded maps. **No Z3 involved at all** — this is what makes the
  four corpus regex predicates (`Matches`/`NotMatches`) evaluate correctly
  even though vacuity never really looks at their regex content.

**What isn't ported.** `regex_to_z3.py` (real Python-`re`-subset → Z3
regex-theory translation) and Z3-based `Counterexample`/model decoding in
subsumption — neither affects the wire verdict (`ok` + the `(stage, step)`
multiset), which is the only thing compared; both are purely diagnostic in
the oracle too.

## Known gaps / honest TODOs

- The 56 residual G-4/G-4b decompose mismatches (32 false-negative, 24
  false-positive) not covered by `detectG4bFusion`'s current rule — see
  above; provably not fully surface-characterizable (see the
  minimal-repro-vs-real-case divergence noted above).
- The one uncharacterized decompose residual (`[ ... \< ... ]`) above.
- `regex_to_z3.py` genuinely not ported (see above) — sound for the
  current corpus, not a general claim.
- No `AliasRegistry` support: every `AliasRef` predicate is treated as
  permanently unresolved, matching the oracle exactly for every case the
  corpus can contain (`docs/spec/conformance.md`: a call carrying an
  `AliasRegistry` is process state and is never recorded).

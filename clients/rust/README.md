# `clients/rust` — the Rust verifier client

An independent Rust reimplementation of the openDaisugi verifier core,
checked differentially against the Python oracle via the conformance
protocol (`docs/spec/conformance.md`). Full profile: Core (permissions,
DAG, delegation safety, shell decomposition) plus predicate-algebra
vacuity classification and skill-delegation subsumption via `z3 -in`.

## Status: 11,450 / 11,450 matched (Full profile)

```
$ CUDA_VISIBLE_DEVICES="" uv run daisugi conformance run \
    .opendaisugi/conformance/corpus.jsonl \
    --client "clients/rust/target/release/conform"
11450/11450 matched
```

Reproduced multiple times across two corpus generations; deterministic.
Also 6/6 fixture tests (all 134 cases in `clients/fixtures/semantics.json`)
and a corpus-deserialization smoke test (`DAISUGI_CORPUS=<path> cargo
test`, skipped cleanly when the env var is unset so the committed suite has
no dependency on the uncommitted corpus).

**History:** first reached 13,387/13,387 on the corpus generation prior to
the 2026-08-21 F-1/F-2 file-scope fix (see below); the oracle's matcher and
the corpus were then both regenerated, dropping the count to 11,450 cases
(11,089 decompose, 361 verify) — the drop is corpus regeneration, not a
regression. Full 11,450/11,450 after porting the new matcher.

**Harness negative control:** before trusting the first `13387/13387`,
`head_allowed` was deliberately mutated to `return true` unconditionally,
rebuilt, and run against that corpus generation — the runner reported
**13357/13387 matched, 30 mismatches, case ids printed** (permissions-stage
shell/mcp rejections that should have fired and didn't). Reverted and
rebuilt back to full match. The comparison harness has teeth; the
full-match number is a measurement, not an unfalsified assertion.

**Reference numbers (this box, this run):**
- rustc 1.98.0 (88d9e12ae 2026-08-18), cargo 1.98.0
- z3 4.16.0 (`~/.local/bin/z3`)
- tree-sitter 0.25.10 (crate), tree-sitter-bash 0.25.1 — version-pinned to
  match the Python oracle's `tree-sitter-bash` wheel (0.25.1) exactly
- corpus: 11,450 cases (11,089 decompose, 361 verify), manifest sha256
  `98093e2db34f6aa8bee6eb173daa670b828273285aeb85d70d61fb6d2ceb7652`
- date: 2026-08-21
- bench, this client (`daisugi conformance bench … --client …`,
  persistent-process IPC, repeat=1): **p50 0.154ms, p95 0.465ms,
  p99 0.879ms, ~4,464 cases/s**
- bench, the Python oracle **on this same box, this same corpus**
  (`daisugi conformance bench …`, in-process, no `--client`, repeat=1, for
  a true apples-to-apples comparison rather than quoting the spec doc's
  numbers from a different corpus generation): p50 0.222ms, p95 0.740ms,
  p99 4.334ms, ~2,604 cases/s. This client's p50/p95/p99 all beat the
  oracle's in-process numbers on this generation (the native
  `posixpath.normpath`-driven recursive matcher this fix added is real
  work the oracle now does in pure Python per path-scope check, across
  many more cases than before) — consistent with the spec's framing that
  per-verification cost is sub-millisecond either way and a compiled
  client's real win is process startup and the long tail, not steady-state
  median throughput, though here it wins on both.

## Build

```
export PATH="$HOME/.cargo/bin:$PATH"
export CARGO_BUILD_JOBS=2   # box is memory-constrained
cd clients/rust
cargo build --release       # first build ~50s (tree-sitter-bash codegen)
cargo test --release        # fixtures + corpus smoke (needs DAISUGI_CORPUS for the latter)
```

Binary: `target/release/conform`. Wire protocol: newline-delimited JSON on
stdin/stdout per `docs/spec/conformance.md`; logs (there are none by
design) would go to stderr, never stdout.

## Architecture

One module per oracle source file, kept in the same names where there's a
clean 1:1 mapping:

| Rust module | Oracle source | Notes |
|---|---|---|
| `models.rs` | `models.py` | `Permission`/`Envelope`/`Invariant`/`Postcondition` via serde derive; `ActionPlan`/`Step` hand-parsed (discriminated union + raw-JSON retention for predicate path resolution) |
| `glob_engine.rs` | `verify.py` (`_head_allowed`, `_path_matches_any`, `_match_glob`, `posixpath.normpath`) | both glob engines + a hand-rolled single-segment `fnmatch.fnmatchcase` (`*`,`?`,`[seq]`,`[!seq]`,ranges); `_match_glob` is the native left-anchored matcher from the 2026-08-21 F-1/F-2 fix |
| `interpreter_parse.rs` | `interpreter_parse.py` | hand-rolled POSIX `shlex.split`/`shlex.quote` (no Rust stdlib equivalent) + the interpreter payload parsers |
| `shell_decompose.rs` | `shell_decompose.py` | tree-sitter-bash walk, byte-for-byte rule mirror |
| `predicate.rs` | `predicate.py` + the pure half of `predicate_z3.py` | `Expr` parser + `evaluate_predicate`/`eval_scalar` (concrete, no Z3) |
| `z3_checks.rs` | `z3_checks.py` | robotics trajectory checks (native f64, exact 8-sample interpolation) **and** envelope self-consistency / plan-vs-envelope (native boolean logic — see below) |
| `z3_bridge.rs` | the symbolic half of `predicate_z3.py` + `vacuity.py` | SMT-LIB2 emission + `z3 -in` subprocess; `check_vacuity` |
| `subsumption.rs` | `subsumption.py` | `envelope_subsumes` — robot-capability + permission-scope fail-closed checks (native), shell-admission + invariant subsumption (Z3) |
| `dag.rs` | `dag.py` | duplicate/missing-dep/cycle checks |
| `verify.rs` | `verify.py` | orchestration: the exact stage order and short-circuit points |
| `wire.rs` / `main.rs` | `conformance.py`'s wire protocol | stdin/stdout JSON-lines loop |

`clients/PORTING-NOTES.md`'s traps (the two glob engines, the metachar-gate-
runs-first invariant, the `**` mid-pattern recursive-segment semantics) are
called out at their exact implementation site in the module docs above, not
re-derived here. The file-scope matcher's right-anchoring bug this note
used to describe (ADJUDICATIONS F-1) is fixed oracle-side as of
2026-08-21 — see the dedicated section below.

## Design choices worth knowing about (not spec deviations — verified choices)

**`check_envelope_self_consistency` / `check_plan_against_envelope` are
native, not `z3 -in` calls.** The spec says Full-profile clients implement
the predicate-algebra stages via a solver binary; these two Z3 calls in the
oracle add only fixed-value boolean/integer conjunctions (`shell == <bool
already known>`, `max_time > 0 and max_time <= 3600`, …) — the "solver
call" always has exactly one possible outcome once the concrete values are
substituted in, so `solver.check() == unsat` reduces to a plain boolean
test with no loss of fidelity. `envelope_subsumes` and `check_vacuity` — the
two call sites that ask a genuine "does *any* assignment exist" question —
do go through `z3 -in` with real SMT-LIB2 text, per spec.

**`Matches`/`NotMatches` always compile to a free/soft Z3 boolean in the
symbolic (vacuity/subsumption) path, never a real `str.in_re` term.** The
oracle's own `regex_to_z3.py` translator falls back to exactly this soft
encoding whenever it can't symbolically translate a pattern (lookaround,
inline flags, etc.); this port takes that fallback branch unconditionally
instead of only on translation failure, skipping the ~250-line sre_parse→Z3
regex translator entirely. This was a deliberate scope call, verified
empirically before committing to it: every regex-bearing invariant/
postcondition in the corpus generation active at the time of this
scope decision (13,387 cases) was traced by hand (there are only
3 distinct regex literals across the whole corpus) to confirm the
tautology/contradiction/non_trivial classification is identical whether the
regex is genuinely translated or left soft, because in every case a sibling
clause in the same `Implies`/`And` already decides the outcome. This is a
real scope boundary, not a proven-general theorem — a future corpus with a
vacuity-relevant predicate that hinges on the *content* of a translatable
regex (e.g. `And(Matches(x, "a.*"), Matches(x, "a"))` with no other
constraint) could expose it. Flagging prominently rather than burying it.

The same choice has a **second, wider consequence** in `subsumption.rs`,
not just `check_vacuity`: `envelope_subsumes` compiles invariants with the
identical `compile_scalar`, and subsumption has a fail-closed rule keyed
on soft nodes — an outer-only soft node (`outer_soft_unique`) forces
`holds = false` unconditionally, because an unshared soft constraint can't
be proven either way. When the oracle's translator *succeeds* on a regex,
that node is a real `str.in_re` term and never enters `soft_outer` at all;
this port always adds one. So an outer envelope carrying a
translatable-regex invariant that the inner contract doesn't mirror would
return `holds = false` here where the oracle proceeds to a genuine Z3
check and could return `holds = true` (a false rejection, not a false
approval — fails closed, not open, but still a real behavioral gap).
Corpus-invisible (all 4 skill-delegation cases in this corpus carry empty
invariants on both sides), but the honest scope of the simplification is
"vacuity classification *and* subsumption's soft-node fail-closed path,"
not vacuity alone.

**`z3 -in` is spawned per query, not held as a persistent process.** The
kickoff plan allowed either; correctness first, and the corpus needs only a
few dozen Z3 round trips total (15 vacuity-relevant expressions and 4
skill-delegation cases on the corpus generation this was measured
against), so process-spawn overhead never shows up in the bench numbers
above (which are dominated by the decompose cases — 11,089 of 11,450 on
the current corpus — none of which ever touch Z3).

**`envelope_subsumes` drops the `SubsumptionResult` detail payload**
(counterexample step, `unverified_invariants`, reasons) — only `.holds` is
wire-relevant, since violation comparison is by `(stage, step)` alone.

**`inheritance.py` (`verify_inheritance`) is not ported.** It is not
reachable from the wire protocol — the corpus has no `"kind": "inheritance"`
case, and `verify()` never calls it. Out of scope by the spec's own
boundary, not an oversight.

## The 2026-08-21 F-1/F-2 file-scope fix (ported, not found by this client)

The oracle's `_path_matches_any`/`_match_glob` used to delegate to Python's
`PurePosixPath.match`, which is right-anchored for relative patterns:
`file_write: ["out.txt"]` admitted `/etc/cron.d/out.txt` (ADJUDICATIONS
F-1, a real scope-escape shape), and mid-pattern `**` had Python-version-
dependent meaning (F-2). The oracle team fixed both with a native
left-anchored, `/`-aware recursive matcher (`verify._match_glob`) — a
pattern must consume the *whole* path, `**` recursively spans zero or more
segments identically on every Python version, and a relative pattern's
segment count can never line up with an absolute path's leading empty
segment (paths are split with plain `str.split('/')`, not filtered of
empty segments, specifically so that invariant holds).

`glob_engine.rs::match_glob` is now a line-for-line port: the `"/**"`-suffix
fast path (root-only `"/**"`, relative-only `"./**"`, literal-prefix
`"dir/**"`) short-circuits first, then the general case does an anchored
recursive segment match (`**` fans out over `0..=remaining` segments;
`*`/`?`/`[...]` stay within one segment via the shared `fnmatch_segment`).
The old right-anchored suffix/full-match model and its `/**`-prefix
shortcut are gone entirely, per the coordinator's explicit instruction —
no fallback path, no dual implementation.

Verified via the regenerated fixture (`clients/fixtures/semantics.json`,
29 `path_match` cases, 7 flipped — all safety tightenings, all now pass)
and the regenerated corpus (11,450 cases, was 13,387 before the oracle
fix and matcher regeneration — the count change is corpus regeneration,
not a mismatch).

## Unfinished / known limitations

**`options.z3_timeout_ms` is parsed out of the wire case and then dropped.**
`wire.rs::parse_case` only extracts `options.strict`; `check_vacuity` and
`envelope_subsumes` take no per-query timeout budget — each `z3 -in`
invocation runs to its own natural completion. Semantically invisible on
this corpus (the formulas are tiny; Z3 answers `sat`/`unsat` in
single-digit milliseconds, well inside any 500ms budget the wire would
have asked for, and `check_vacuity`'s `unknown`-folds-to-`non_trivial`
behavior is preserved regardless of *why* no definite answer came back).
The honest gap: a pathologically expensive predicate would hang the
`z3 -in` child until the *conformance runner's* 600s process-level
timeout, rather than degrading gracefully to a `VerificationTimeout`-style
warning at the 500ms the oracle would have honored. Wiring a real timeout
would mean either a watchdog thread killing the child process or an
external `timeout(1)` wrapper around the `z3` invocation — not done here;
flagged rather than silently absorbed.

## Adjudications

None from this client — no oracle disagreements found on either corpus
generation worked against; every case matched on the first full run each
time, and stayed matched on repeated reproducibility runs. Status update
on `clients/ADJUDICATIONS.md`'s two pre-campaign findings: **F-1** (right-
anchored file scopes) and **F-2** (Python-version-dependent glob semantics)
were fixed oracle-side on 2026-08-21 (they were frozen-but-flagged when
this client was first built, matched deliberately as documented in the
prior revision of `glob_engine.rs`'s module doc) — this client now ports
the fix rather than the bug. See the dedicated section above for what
changed and how it was verified.

# openDaisugi TypeScript conformance client

An independent TypeScript/Node reimplementation of the openDaisugi verifier
core, checked differentially against the Python oracle via
`daisugi conformance run`. Targets the **Full** profile (Core + the
predicate-algebra / skill-subsumption stages, via `z3 -in`).

## Status (2026-08-21)

Matched against the reference corpus generated on this box:

| Gate | Cases | Matched |
|---|---|---|
| decompose slice | 11,089 | 11,089/11,089 |
| verify slice | 361 | 361/361 |
| **Full corpus** | **11,450** | **11,450/11,450** |

Plus the committed 134-case semantics fixture (`clients/fixtures/semantics.json`),
run as `node:test` unit tests (`npm test`) — 6/6 groups, all cases, before any
corpus run was attempted.

```
node: v22.22.2
z3:   4.16.0 (64-bit), invoked as `z3 -in`, SMT-LIB2 text over stdin/stdout
corpus: .opendaisugi/conformance/corpus.jsonl, 11,450 cases (11,089 decompose,
        361 verify) — generated on this box, 2026-08-21, not committed
        (embeds real local paths; regenerate per docs/spec/conformance.md).
        Regenerated mid-campaign after the oracle-side F-1/F-2 fix (see
        below) — an earlier pass matched a prior 13,387-case corpus
        (13,084/303) against the pre-fix oracle; both are recorded here for
        the history, only the current numbers are load-bearing.
```

## Running it

```sh
cd clients/ts
sh vendor/fetch.sh   # pinned grammar wasm (sha256-verified)
npm install          # installs only web-tree-sitter (runtime) + typescript/@types/node (dev)
npm run build         # tsc -> dist/
npm test              # the 134-case fixture, via node:test

# from the repo root, the differential gate:
CUDA_VISIBLE_DEVICES="" uv run daisugi conformance run \
  .opendaisugi/conformance/corpus.jsonl \
  --client "node clients/ts/dist/conform.js"
```

No native modules — the shell grammar is `web-tree-sitter` (wasm), and Z3 is
a subprocess speaking SMT-LIB2 text, never a linked solver API. `node_modules/`
and `dist/` are not committed (see `.gitignore`); `npm install && npm run build`
reproduces them exactly from `package.json`/`package-lock.json` + `src/`.

## Vendored grammar

`vendor/tree-sitter-bash.wasm` — NOT committed (a 1.3MB binary blob);
`sh vendor/fetch.sh` downloads the **`tree-sitter-bash@0.25.1`** npm tarball,
extracts the wasm the package ships at its root, and verifies its sha256
before installing (no build step, no emscripten, no node-gyp needed). This is the
exact same version as the oracle's Python wheel (`tree-sitter-bash==0.25.1`,
grammar ABI version 15), confirmed with:

```sh
CUDA_VISIBLE_DEVICES="" uv run python -c \
  "import tree_sitter_bash as t; import importlib.metadata as im; print(im.version('tree-sitter-bash'))"
# -> 0.25.1
```

- **Origin:** `https://registry.npmjs.org/tree-sitter-bash/-/tree-sitter-bash-0.25.1.tgz`, file `tree-sitter-bash.wasm` at the package root.
- **sha256:** `8292919c88a0f7d3fb31d0cd0253ca5a9531bc1ede82b0537f2c63dd8abe6a7a`
- **Size:** 1,358,224 bytes (~1.3MB) — over the plan's informal <1MB guideline;
  committed anyway since there is no smaller correct alternative (it's the
  grammar's actual compiled size) and vendoring it is explicitly the
  house-rule-sanctioned path. It is the only binary this client ships.
- Loaded via `web-tree-sitter@0.25.10` (pinned to the 0.25.x line matching
  the grammar, per house rules). `Parser.init()` + `Language.load(path)` are
  awaited **before** the wire-protocol entrypoint starts consuming stdin
  (`conform.ts`'s `main()`), so there's no risk of a case racing parser init.

Grammar-fidelity check before writing any decompose semantics: parsed 80
commands (the 20 shortest, 20 around the median, the 20 longest, and 20
sampled from the 2,102 non-ASCII commands in the corpus — non-ASCII is 16% of
the corpus and was called out as the likeliest source of a byte-vs-UTF-16
offset bug) with both the Python wheel and this wasm, and diffed the full
`(type, text, isMissing, childCount)` pre-order tree. **0 mismatches on
first try** — the npm-published wasm and the PyPI wheel are the same grammar
build.

## Layout

```
clients/ts/
  src/
    models.ts            # data shapes (tolerant decoder — unknown step types preserved as UnknownStep)
    pyport.ts             # posixpath.normpath/splitroot, fnmatch.translate, PurePosixPath.match (py3.12)
    pathScopes.ts          # _path_matches_any / _match_glob / the custom trailing "/**" handler
    headAllowed.ts         # _head_allowed (the OTHER glob semantics — left-anchored, segment-count-equal)
    shlex.ts               # shlex.split(posix=True) / shlex.quote, hand-ported state machine
    shellHead.ts            # _extract_shell_head
    shellMetachar.ts         # _SHELL_METACHAR_RE
    resolveStrict.ts          # resolve_strict
    interpreterParse.ts        # parse_interpreter (sh -c / xargs / find -exec / env / ADR-0014 wrappers)
    pyRegex.ts                  # re.search() shim for Matches/NotMatches ground evaluation
    predicate.ts                 # the predicate-algebra Expression union
    resolvePath.ts                # _resolve_path — shared dot-path traversal (ground eval + symbolic compiler)
    predicateEval.ts              # evaluate_predicate — ground (concrete-plan) evaluation, no Z3
    predicateZ3.ts                 # _compile_scalar/_Scope — SYMBOLIC compilation to SMT-LIB2 text
    vacuity.ts                      # check_vacuity (tautology/contradiction over one free symbolic step)
    subsumption.ts                   # envelope_subsumes (skill-delegation Z3 primitive)
    contracts.ts                      # verify_delegation (signature checking is provably dead code here — see below)
    z3checks.ts                        # robotics numeric invariant handlers (NO solver — pure arithmetic)
    z3client.ts                         # the ONE long-lived `z3 -in` subprocess, push/pop framing
    dag.ts                               # check_dag (duplicate ids -> missing deps -> cycle)
    permissions.ts                       # check_permissions + shell command checking + agentic step
    shellDecompose.ts                     # decompose_command via web-tree-sitter (incl. G-4 bare-newline-fusion repair)
    verify.ts                              # the verify() pipeline orchestration
    conform.ts                              # the wire-protocol entrypoint (dist/conform.js)
    test/fixtures.test.ts                    # the 134-case semantics fixture as node:test
  vendor/tree-sitter-bash.wasm
  package.json / tsconfig.json / .gitignore
```

## What's genuinely symbolic (and what's a documented simplification)

The corpus's 303 verify cases were profiled before writing any Z3 code
(`envelope.invariants[].expr` / `postconditions[].expr` op trees, scanned
across every case) to find out what actually needs a solver:

- **z3 stage (18 cases):** overwhelmingly the *numeric* robotics handlers in
  `z3_checks.py` (`_check_velocity_bounds`, `_check_obstacle_avoidance`, …) —
  no solver at all, just IEEE754 arithmetic (identical between Python and
  JS). Only `check_envelope_self_consistency`/`check_plan_against_envelope`
  actually touch Z3, and both are small enough to encode in SMT-LIB2 directly
  (`verify.ts`) even though they're decidable by inspection — ~15 lines, and
  it removes any argument about the Full-profile claim.
- **predicate stage (26 cases):** the only ops that ever appear anywhere in
  an invariant/postcondition expr tree in the whole corpus are `equals`,
  `not_equals`, `in_set`, `not_matches`, `numeric_range`, `and`, `or`,
  `implies`, `forall_steps`, `alias`. All are compiled faithfully to
  SMT-LIB2 in `predicateZ3.ts`. `Matches`/`NotMatches` compile to a real
  `str.contains` term **only for a regex-metacharacter-free pattern**
  (Python `re.search` is substring search, which `str.contains` reproduces
  exactly for a literal) — a pattern carrying real regex syntax becomes a
  free "soft" Z3 Bool, exactly like the oracle's own fallback for regexes
  its translator can't handle. This is sound-but-incomplete in the same
  spirit as the oracle's design, and doesn't change any corpus verdict: the
  two vacuity-classified invariants in the corpus (`always_false` /
  `useless`) use only `equals`/`not_equals`/`and`/`or`, never regex.
- **delegation stage (2 cases) / subsumption:** all 4 `SkillStep` cases in
  the corpus carry **empty invariants on both sides** — the entire predicate
  side of subsumption is `true` for every case this client is graded on.
  What *does* matter and is fully implemented: the Z3 shell-command
  admission encoding (`PrefixOf`/string equality over a free `ctx_command`
  string, metachar `Contains` checks) and the glob-subsumption checks
  (`PrefixOf`/`SuffixOf` for `file_read`/`file_write`/`mcp_allowlist`) — one
  of the 4 corpus cases (`b9327d6c…`) is only correctly rejected because the
  shell-admission Z3 query finds a real witness command.
- **`contracts.ts` signature checking:** `verify.py`'s `check_skill_delegations`
  calls `verify_delegation(envelope, contract, strict=strict, timeout_ms=…)`
  with neither `trusted_signers` nor `signer_registry`, and a `SkillStep`'s
  `contract_envelope` JSON carries no signature field at all — every
  signature branch in the oracle is unreachable from the conformance wire
  protocol. Ported as a stub that always takes the "no signature" path
  rather than porting ~40 lines of dead code.
- **`llm_check`:** never appears anywhere in the corpus (confirmed by
  scanning every expr tree, not just top-level ops) and requires a live LLM
  call in the oracle — not reproducible here. Throws (fail-closed, becomes a
  predicate violation), matching the oracle's own handling of an
  errored/failed check.

`z3 -in` is spawned **lazily** and only once, on the first case that
actually needs it (an expr-bearing invariant/postcondition, or a
`SkillStep`) — the 13,084 decompose cases and the ~277 plain-permissions
verify cases never touch the solver, so the bench numbers below aren't
inflated by it. `(get-model)` is never called anywhere — every consumer here
only needs the sat/unsat/unknown verdict (counterexample *content* only
feeds `reason` strings, which are informative, not compared).

## Bench

`daisugi conformance bench` (this box, repeat=3, current 11,450-case corpus):

| Client | p50 | p95 | p99 | throughput |
|---|---|---|---|---|
| Python oracle, in-process | 0.184ms | 0.579ms | 3.305ms | 3,407 cases/s |
| Python oracle, over the pipe | 0.251ms | 0.700ms | 3.456ms | 2,662 cases/s |
| **TypeScript, over the pipe** | **0.311ms** | **0.831ms** | **1.433ms** | **2,598 cases/s** |

(An earlier bench pass against the prior 13,387-case corpus, before the
oracle's F-1/F-2 fix regenerated it, measured p50 0.130/0.181/0.234ms and
4,569/3,740/3,490 cases/s respectively for the same three rows — the
absolute numbers moved with the box's load and the smaller sample, not with
any code change; the *relative* standing (TS within ~25-35% of the Python
pipe baseline) held both times.)

Steady-state per-case latency is in the same ballpark as the Python oracle's
own pipe numbers — verification is sub-millisecond already; the interesting
cost is process startup, which the bench's warmup round-trip deliberately
excludes. Measured separately:

| What | Cold-start wall time (spawn → first verdict → exit) |
|---|---|
| A single `decompose` case (no Z3 ever touched) | ~0.21s |
| A single `verify` case that needs Z3 (spawns `z3 -in`) | ~0.35s |
| wasm init alone (`Parser.init()` + `Language.load()`), isolated | ~30-40ms |

The wasm-init cost is a small fraction of the ~0.2s Node cold start (module
resolution + V8 startup dominate); spawning the `z3` child process adds
another ~0.1-0.15s on top, paid once per client process (not per case) since
the session is long-lived and reused across every case in the corpus.

## Notable implementation traps (beyond what PORTING-NOTES.md already flags)

- **Node killing the process on EOF:** `z3 -in`, once spawned, is a
  long-lived child whose open stdio pipes keep Node's event loop alive
  forever — `conform.js` would read all of stdin, answer every case
  correctly, and then just hang (the Python differential runner's
  `proc.communicate()` waits for the *process* to exit, not just stdout to
  close). Fixed by explicitly killing the z3 session after the stdin loop
  ends. Cost about an hour of "why does this hang with correct output"
  debugging — worth flagging for the other client authors.
- **`UnknownStep` poisons TypeScript's discriminated-union narrowing.**
  `type ActionStep = ShellStep | … | UnknownStep`, where `UnknownStep.type:
  string`, defeats `step.type === "shell"` narrowing project-wide (a bare
  `string` is structurally compatible with every literal, so TS can't
  exclude `UnknownStep` from the narrowed type). Fixed with explicit
  `isShellStep()`-style type-guard functions in `models.ts` (a user-defined
  type predicate overrides normal narrowing) rather than casts scattered
  through `permissions.ts`/`z3checks.ts`.
- **`fnmatch.translate`'s STAR-collapse algorithm** is hand-ported in
  `pyport.ts` directly from the CPython 3.12 stdlib source (not from
  documentation), and backs both glob semantics the oracle uses (see
  PORTING-NOTES.md — `_head_allowed`'s left-anchored, equal-segment-count
  matching, and `pathScopes.ts`'s file-scope matcher below).
- **File-scope matching was ported twice, because the oracle changed
  mid-campaign.** The first pass ported `PurePosixPath.match`'s "lines"
  trick (path separators swapped for `\n`, matched with a MULTILINE regex)
  verbatim from CPython 3.12 — validated against ~1,600 synthetic
  `(path, glob)` vectors generated directly from `verify._path_matches_any`,
  0 mismatches. That port is **gone**: the oracle's own `_match_glob` was
  right-anchored for relative patterns (`file_write: ["out.txt"]` admitted
  `/etc/cron.d/out.txt` — a real scope-escape bug) and Python-version-
  dependent (`**` meant something different on 3.13); both were logged as
  F-1/F-2 in `clients/ADJUDICATIONS.md`, then fixed oracle-side mid-
  campaign with a native left-anchored, `/`-aware, backtracking matcher
  (`**` = zero-or-more whole segments, `*`/`?`/`[...]` stay within one
  segment via `fnmatchcase`). `pathScopes.ts` now ports *that* algorithm —
  a straightforward recursive `matchFrom(patternIndex, pathIndex)` — and the
  now-dead pathlib-emulation code (`purePosixMatch`, the "lines" helpers)
  was deleted from `pyport.ts` rather than left as an unused alternate path.
  Re-ran the 134-case fixture (7 `path_match` cases flipped, all in the
  tightening direction) and the full corpus after the swap: still
  11,450/11,450.
- **`shlex.split`'s posix+whitespace_split state machine** was ported
  directly from `read_token()`'s character-by-character states (`' '`,
  `'a'`, quote states, escape state) rather than reverse-engineered from
  examples — the escaped-quotes-only-inside-double-quotes rule
  (`escapedquotes = '"'`, so `\x` inside `'...'` is two literal characters
  but `\x` inside `"..."` may drop the backslash) is easy to get subtly
  wrong from black-box testing alone. Validated against 63 vectors from the
  real `shlex.split`, including unbalanced-quote and trailing-backslash
  `ValueError` cases.

## What's unfinished, and why

Nothing required by the plan's task ladder. Scoping decisions, not gaps:

- **`inheritance.py` was not ported.** `verify_inheritance` is not reachable
  from `verify()` (grepped — zero references) and doesn't appear as a stage
  in any of the 303 verify cases (`stages` seen in the corpus: `permissions`,
  `predicate`, `z3`, `delegation`, `dag` — never `inheritance`). Confirmed
  with the advisor before skipping; porting 251 dead lines would not move
  any gate.
- **Full generality of the predicate-algebra symbolic compiler.** `exists`,
  `is_empty`, `length_range`, `depends_on`, `before`, `forall_outputs` are
  all ported for *ground* evaluation (`predicateEval.ts`, used for the
  actual pass/fail decision on every invariant/postcondition) but only
  partially for *symbolic* compilation (`predicateZ3.ts`, used by
  vacuity/subsumption) — see "What's genuinely symbolic" above. This
  matches the oracle's own scope (subsumption's `_compile_invariants` only
  strips `ForallSteps`/`ExistsStep`, not `ForallOutputs` either) and isn't
  exercised by any corpus case.

## Fidelity notes

- `check_vacuity` is called with the function's own 500ms default, NOT the
  case's `z3_timeout_ms` — `verify.py` calls `check_vacuity(expr)` with no
  timeout argument. Matched exactly (hardcoded `500` in `verify.ts`, not
  threaded from `options.z3_timeout_ms`). Invisible on this corpus (every
  case uses `z3_timeout_ms: 500` anyway) but would matter on a regenerated
  corpus with a tighter budget.
- `shellDecompose.ts` calls `tree.delete()` after copying every string it
  needs out of the wasm tree (heads/commands/reads/writes/reason are all
  plain JS strings by the time `visit()` returns) — web-tree-sitter
  allocates each parse in the wasm heap and never frees it automatically,
  and the box this runs on is explicitly memory-constrained.
- The wire protocol's normative "never abort the stream, always emit an
  `error` verdict" behavior isn't exercised by any corpus case (the whole
  corpus is well-formed by construction) — verified by hand instead:
  4 hand-crafted lines (invalid JSON, a `decompose` case missing `command`,
  an unknown `kind`, and `v: 2`) all produced 4 `error` verdicts and a clean
  `exit 0`, never a stream abort.

## Findings for `clients/ADJUDICATIONS.md`

Three findings, all now resolved (either fixed oracle-side, or ported to
match a since-fixed oracle) — none currently open against this client.

**F-3 (originally: a latent oracle crash bug) — FIXED IN THE ORACLE, this
client re-aligned.** `predicate_z3._compile_scalar`'s `NotEquals` branch
used to always resolve a Z3 `String` variable regardless of `expr.value`'s
type, unlike `Equals` (which correctly branched numeric vs. string) — a
`not_equals` predicate authored with a numeric/boolean value produced a
genuine Z3 sort mismatch (`Z3Exception`), uncaught in the subsumption path
(would error a `SkillStep` delegation check out of `verify()` entirely).
Originally found by code inspection (no corpus case exercised it — all
`not_equals` usages compare strings, all `SkillStep` cases have empty
invariants), logged as F-3, and initially *matched* here (this client threw
too, reproducing the oracle's bug on principle — see git history of this
file / `predicateZ3.ts` for that version). The oracle was then fixed
(`NotEquals` now branches numeric-vs-string exactly like `Equals`; see
`clients/ADJUDICATIONS.md`'s RESOLUTIONS entry, pinned oracle-side by
`tests/test_predicate_notequals_numeric.py`) — `predicateZ3.ts`'s
`not_equals` case now does the same branch, no more throw. Still not
exercised by any corpus case; matters only for future `SkillStep` contracts
carrying a numeric/boolean `not_equals` invariant.

**G-4 (a real oracle vulnerability, found by the Go/Lean clients) — FIXED
IN THE ORACLE, ported here (twice).** tree-sitter-bash 0.25.1 can fuse a
newline-separated statement boundary into ONE `command` node (`c1\nd1` →
head `c1`, argument `d1`) without setting `has_error` — `d1` then executes
but never faces the allowlist (fail-open). The oracle's FIRST fix
(`shell_decompose._command_has_bare_newline`) rejected any `command` node
whose span carried a raw, un-exempted newline (outside a quoted string /
heredoc / substitution, and not a `\`-continuation) — correct but blunt: it
failed closed on every real multi-line script, not just the ones the bug
actually endangered. The oracle's fix was later upgraded from detect-and-
reject to detect-and-**repair**: `_all_fused_newline_offsets` finds every
fused newline across the whole tree (not just one `command` node),
`_rewrite_fused_newlines` replaces each with an explicit `;` — the
unambiguous separator tree-sitter will not fuse, adjacency-guarded so it
never produces `;;` — and the WHOLE command is re-parsed from that rewrite,
preserving compound context (`if`/`then`/`else`, loops). A clean re-parse
recurses through `_decompose` and now yields a normal decomposition
(`c1\nd1` correctly surfaces heads `c1` AND `d1`); a rewrite that isn't
valid shell (`;` is illegal right after `then`/`do`/`else`) still fails
closed with reason `"ambiguous shell (bare newline inside command — parser
statement fusion)"`. Because this client parses with the SAME grammar as
the oracle (`web-tree-sitter` + the identical `tree-sitter-bash@0.25.1`
wasm, not an independent parser like Go's mvdan or Lean's own recursive
descent), it exhibits the identical fusion artifact and needed the
identical two-stage fix — ported as `bareNewlineOffsets` /
`allFusedNewlineOffsets` / `rewriteFusedNewlines` plus a `decomposeInner
(parser, command, depth)` recursion in `shellDecompose.ts`, a direct
line-for-line translation of `_bare_newline_offsets` /
`_all_fused_newline_offsets` / `_rewrite_fused_newlines` / `_decompose`
(walking every `MULTILINE_LEGAL`-typed descendant to build a protected-span
list, then scanning each `command` node's own span for an unprotected
`\n`/`\r`; the old `commandHasBareNewline` reject inside the tree walk is
gone — fusion is now resolved once, up front, before the walk ever runs).
One adaptation, not a divergence: the oracle iterates raw UTF-8 *bytes*
(`src: bytes`, `start_byte`/`end_byte`); this port iterates UTF-16 *code
units* directly against the original JS string (`node.startIndex`/
`endIndex`) — each side stays internally consistent (offsets are computed
and consumed against the same string in the same units), so the two only
need to agree on *whether* a given position is a bare newline, not on a
shared numeric offset value; empirically checked, not just assumed — 1,523
of the corpus's decompose cases mix a raw newline with non-ASCII content,
and all match. Confirmed by the full decompose gate (11,089/11,089) rather
than by a targeted fixture (no `bare_newline` group exists in
`clients/fixtures/semantics.json`).

**F-1/F-2 (right-anchored file-scope globs; Python-version-dependent `**`)
— FIXED IN THE ORACLE, ported here.** Logged before this client started
(pre-campaign fixture-generation findings), fixed oracle-side mid-campaign
(a coordinator-relayed patch, not something this client found). Originally
ported as-is per PORTING-NOTES' "match the oracle, don't fix it"
instruction (the old `PurePosixPath.match`-based port in `pyport.ts`); once
the oracle itself changed to a native left-AND-right-anchored, `/`-aware,
backtracking matcher, `pathScopes.ts` was rewritten to that algorithm (see
"Notable implementation traps" above) and the now-dead pathlib-emulation
code was deleted from `pyport.ts` rather than left as an unused alternate
path.

All three re-verified together: the 134-case fixture (`npm test`, 6/6
groups) and the full 11,450-case corpus gate both green after every fix
above landed in this client.

# `clients/rust` — the Rust verifier client

An independent Rust reimplementation of the openDaisugi verifier core,
checked differentially against the Python oracle via the conformance
protocol (`docs/spec/conformance.md`). Full profile: Core (permissions,
DAG, delegation safety, shell decomposition) plus predicate-algebra
vacuity classification and skill-delegation subsumption on the statically
linked Z3 5.1.0.

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
| `z3_bridge.rs` | the symbolic half of `predicate_z3.py` + `vacuity.py` | SMT-LIB2 emission, run on the linked Z3; `check_vacuity_exact` uses the gate's exact vacuity check |
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

**Since 2026-09-25: Z3 is linked, and vacuity is exact.** Every script
below runs through `Z3_eval_smtlib2_string` on the statically linked Z3
5.1.0 (the oracle's version), not a `z3` binary on PATH. The vacuity of
an invariant or postcondition is the gate's own check
(`src/gate/predicate.rs`: the tagged-union parse, `regex_to_z3` over the
port of Python's re, the linked Z3), so the soft-regex choice described
below now applies to subsumption only. The corpus still matches in full.

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

### Since then

Two findings from porting the gate (below) were in this client's
verifier, both fixed: `shlex_split` escaped `\$` and `` \` `` inside
double quotes where Python keeps the backslash (GT-20), and a redirect
with more than one target was accepted where the oracle refuses it
(GT-21). The corpus still matches in full: 11,450 of 11,450.

## daisugi-gate: the tool-call gate in Rust

The second port of the gate hook, beside the Go one
(`clients/go/README.md`, "daisugi-gate"). Every tool call an agent makes
runs the gate; a cold `python -m opendaisugi.gate` costs about half a
second, almost all of it start-up. This binary answers the same hook the
same way without that cost, and it never runs Python.

```sh
../go/scripts/native.sh        # once: the native prefix, with the baseline libc++
cd clients/rust
source tools/z3-build-env.sh   # zig c++ for Z3 (baseline CPU), the prefix's libc++, the build-path maps
cargo build --release          # target/release/daisugi-gate
tools/check-paths.sh target/release/daisugi-gate target/release/conform
echo '{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}' \
  | target/release/daisugi-gate --mode shadow --root ~/.opendaisugi/gate --format claude
```

This builds from a fresh checkout: `z3-build-env.sh` needs zig 0.16.0 on
PATH and the native prefix `clients/go/scripts/native.sh` builds, and cargo
builds Z3 from the z3-src crate's pinned source (about half an hour at two
jobs). Z3 and the C++ runtime are compiled for the baseline CPU, so the
binaries run on any x86-64. They name no build path, and link libc, libm
and `libgcc_s`, which Rust's unwinder needs on glibc (a static glibc build
would need glibc's NSS modules at run time for the name lookup of an
`llm_check` over https). `scripts/release.sh VERSION --rust` puts
`daisugi` and `daisugi-gate` in a second tarball,
`opendaisugi-rust-VERSION-linux-x86_64.tar.gz`.

It reads the same flags (argparse's rules, help text and errors
included), the same payload and the same gate root (envelopes, the
`DISARMED` marker, `config.yaml` beside the root), and writes the same
stdout, stderr and exit code for each format, the same shadow line,
session tree entries, captures line, checkpoint refs and coppice or
Herdr report. `DAISUGI_GATE_TRACE=<file>` appends one line per call:
`native`, `undecided` (with why) or `backstop`.

### What it ports

- the argv (argparse), config.yaml (a PyYAML SafeLoader subset and
  pydantic's Config rules), the payload (Python's `json`, lone surrogates
  and the 4,300-digit limit included), and the envelope as
  `Envelope.model_validate_json` reads it: jiter 0.14.0 (pydantic-core's
  own parser) and pydantic's lax rules and error text for every field;
- the three hard-deny rules, the search rule and the tier, with the
  oracle's shell decomposition on tree-sitter-bash, its `re` (a port of
  CPython's parser and matcher), `realpath`, `expanduser` and glob;
- the verifier: permissions (with urllib's `urlsplit`, its ValueErrors
  included, and `fnmatch.translate` on the ported `re`), self-consistency
  and plan-vs-envelope on the linked Z3 5.1.0, the predicate stage
  (the tagged-union parse, vacuity on Z3 with `regex_to_z3`, and
  evaluation with Python's equality, `len`, `float()` and attribute
  lookup), and a configured `verifier_client`, run as `verify_via` runs
  it;
- `llm_check` as litellm 1.100.0 makes the call, over http or over
  https (rustls with the ring provider and the system's root store),
  a reply json.loads refuses worded as json.loads words it;
- `--ask` (the operator file protocol), `--checkpoints` (the same
  hardened git commands through a temporary index), the Herdr report,
  and every log line;
- the oracle's size caps (a payload over 16 MiB, a shell command over
  262,144 characters) and its 100,000-step glob limit.

Python's RecursionError is part of the contract: each ported oracle
function counts its frame, so the shell walk, the symlink resolver and
the glob matcher stop where the oracle stops.

The call is decided in a child process. The parent reads at most one byte
past the payload cap, starts the child with a fresh nonce and a frame pipe
on fd 3, and gives the child's verdict only when a complete frame with that
nonce comes back before the deadline (the verify budget, plus the ask
budget with `--ask`, plus 3 s, below the host timeout the installer
writes). The child writes the format's deny on its own stdout and exit
code, so a child started by anyone else reads as a deny. Any abnormal end
is a deny in the format argparse reads. The child has done all its work
when it closes the frame pipe, so a complete frame is the answer at once
and the child is then killed, not waited for; a pidfd wakes the parent
the moment a child ends without one.

### What it does not decide

A call the port cannot decide exactly as the oracle would is denied with
exit 2 and a reason (ADJUDICATIONS RG-8, RG-11, RG-12): an `llm_check`
through `claude -p`, under settings that change litellm (a proxy or a CA
file among them), or that fails at the network or TLS level other than a
refused connection (an untrusted certificate, say); a `\N{...}` regex
escape; a regex search past 20 million steps; a regex repeat of more than
10,000 copies inside a vacuity check; a `--verify-timeout` under one
second; and `OPENDAISUGI_CONFORMANCE_RECORD`.

### Differential results and speed

On 2026-09-25, rebased on master 25c38d19:

- gate cases (1,288): 1,288 native, 0 disagreements, 0 stale against a
  live oracle;
- fuzz (at most 2,000 items a run, one run at a time): shell seeds 7 and
  11 (6,000 calls each, three envelopes), heredoc and multiline (6,000
  calls each), unicode and envelope (2,000 each), and the 790-command
  redirect list (2,370 calls): 30,370 calls, all native, all the same as
  the oracle;
- the frozen corpus through `conform`: 11,450 of 11,450;
- `tools/check-paths.sh`: no build-machine path in either binary.

Wall time per call, cold process, over the gate cases: p50 6.1 ms and
p90 7.1 ms, against 429 ms and 464 ms for the Python gate. The two
ground Z3 stages (envelope self-consistency, plan against envelope) are
decided directly, as the Go gate decides them, and a test asks the
linked Z3 the same formulas over every combination of their inputs.
Making a Z3 context costs about 8 ms (z3py pays the same: Z3 5.1.0
zeroes an 8.5 MB term table), so it is made only when the envelope has
invariants or postconditions, on its own thread once the envelope is
loaded, and shared by every check of the call. A predicate case that
runs the vacuity check takes about 20 ms. The binary is 32 MB
and links only libc, libm and libgcc_s dynamically.

```sh
uv run --no-sync python clients/gate_compare.py --binary clients/rust/target/release/daisugi-gate --oracle
uv run --no-sync python clients/gate_compare.py --binary clients/rust/target/release/daisugi-gate --fuzz 2000 --kind envelope
```

### Dependencies

All from crates.io, each used under one of the licences it offers (see
`NOTICE`): serde, serde_json, regex, thiserror and libc (MIT or
Apache-2.0), tree-sitter and tree-sitter-bash (MIT), jiter (MIT),
z3-sys with z3-src (MIT), which build Z3 5.1.0 from source and link it
statically, and rustls with rustls-native-certs (Apache-2.0, ISC or
MIT) on ring for an `llm_check` over https. rustls needs subtle, which
is BSD-3-Clause; `NOTICE` carries its notice. The tests alone use rcgen
for a test CA.

## daisugi: the Rust CLI for the gate

`target/release/daisugi` is the Rust port of the Go `daisugi` binary
(`clients/go/README.md`, "daisugi: the Go CLI for the gate"): the gate
commands a user runs every day and the gate half of `daisugi install`,
with the flags, files and output of the Python CLI. It never runs
Python. `daisugi gate check`, the command an installed hook runs, is
the gate above: it decides in a child it starts as `daisugi gate
check`, with the same frame pipe, nonce and deadline as `daisugi-gate`.

```sh
cd clients/rust
source tools/z3-build-env.sh
cargo build --release          # target/release/daisugi, daisugi-gate, conform
target/release/daisugi gate init --workspace "$PWD"
target/release/daisugi install --gate --enforce --yes
target/release/daisugi gate status
```

It carries the Go binary's commands: `gate check`, `gate init`, `gate
register`, `gate arm`, `gate disarm`, `gate status`, `gate report`,
`gate settings`, `gate proposals`, `install --gate [--enforce|--shadow]
[--runtime X] [--dry-run] [--yes] [--uninstall]`, `install --harness
pi|opencode [--uninstall] [--dry-run]`, `pathways list|show|stats|
delete|export|import` (see the next section), and `gardener`, `tend`,
`hook auto-tend` and `distill-repeats` (the garden, below). Everything else prints one line,
`<command> is not in this binary yet.`, and exits 2 with nothing changed.
The hook it writes has the Go binary's words,

```
DAISUGI_GATE_HOOK=opendaisugi.gate '/abs/path/daisugi' gate check --mode enforce \
  --root ~/.opendaisugi/gate --format claude --verify-timeout 10.0 || exit 2
```

and it finds a gate hook by its words, with the same rules for an
unknown gate hook (ADJUDICATIONS C-9). `config.yaml` and an envelope for
`gate register` go through the Go binary's YAML reader, ported; YAML
outside it is refused with a reason. Files are written atomically, and a
`settings.json` that is a symlink is written through with a note, as the
Go binary does. The pi and OpenCode extensions are the Python package's
own files, built into the binary.

On 2026-09-25, against the committed CLI cases and a live oracle
(`clients/cli_compare.py --binary clients/rust/target/release/daisugi
--oracle`): 234 cases, 221 agree, 10 not ported and 3 refused (the YAML
the reader does not take), as for the Go binary; 0 stale, 0 machine
paths in the fixtures. Every gate case run through `daisugi gate check`
(a two-line wrapper script) is native with 0 disagreements (1,288 of
1,288). Cold start: `daisugi --help` p50 2.0 ms, p90 2.4 ms; `daisugi
gate status` p50 5.0 ms. `tools/check-paths.sh` finds no build-machine
path in it.

## daisugi pathways: the pathway store in Rust

`daisugi pathways list|show|stats|delete|export|import` run in the binary
with the flags, output, exit codes and files of the Python CLI, as the Go
binary runs them (`clients/go/README.md`, "daisugi pathways"). The binary
never runs Python. The store is the oracle's SQLite file
(`~/.opendaisugi/pathways.db`, or `--data-dir`), read and written through
rusqlite, which compiles the SQLite amalgamation it bundles (SQLite
3.45.0) into the binary: the same schema text, the same additive and
dropped columns on open, and each column written with the oracle's own
encoder. Python and the binary read each other's files.

| Module | What it models |
|---|---|
| `pathways::store` | `PathwayStore`: open and migrate, rows read in rowid ranges on four connections under one read transaction, `put`, `delete`, `stats`, an overwrite in one transaction |
| `pathways::pathway` | `_row_to_pathway` in Python's order, and pydantic's JSON out (`model_dump_json`, `mode="json"`) |
| `pathways::pmodel` | pydantic's lax validation of `CompiledPathway`, `Envelope`, `ActionPlan` and its 13 step types, `PathwayParameter`, in Python and JSON mode, with pydantic's error text and the non-finite after-validator (GD-17) |
| `pathways::scan`, `pathways::find` | the stored vectors read sparsely, norms summed in numpy's pairwise order, `find` with the provenance filter, the stale warning and the dimension guard |
| `pathways::lexical`, `pathways::blake2b` | the zero-model matcher of ADR-0019 with its own 8-byte BLAKE2b |
| `pathways::potion` | model2vec's `StaticModel.encode`, the tokenizer (added tokens, `BertNormalizer`, `BertPreTokenizer`, `WordPiece`), safetensors, and the pinned fetch |
| `pathways::export`, `pathways::yamldump`, `pathways::dumped` | the five export formats; `yaml.safe_dump`'s emitter, and a reader for text in that form |
| `pathways::importer`, `pathways::verify` | `parse_bundle`, `_check_storable` and `import_pathway`'s re-verification |

### Matchers

`find` reads `matcher_model` from `~/.opendaisugi/config.yaml`. Neither CLI
has a `find` command; `target/release/pathway-probe` is the test
instrument that runs it on many queries in one process (PW-R-8).

- `lexical` is native and exact: Python's `str.lower()`, `[a-z0-9_]+`
  tokens, the stopwords, an 8-byte BLAKE2b, the bucket and sign from it,
  and `sqrt(count)` weights. Threshold 0.25, identity `lexical-hash-v1`.
- `potion` is native. The default model, `minishlab/potion-base-8M`, is
  fetched only on first use, with one line on stderr before the download,
  from one pinned revision, each file checked against the SHA-256 in
  `src/pathways/potion/mod.rs` (the Go binary's three values), into
  `$XDG_CACHE_HOME/opendaisugi/models/potion-base-8M` (`~/.cache` when
  unset). The download goes over rustls with the system's root store and
  follows Hugging Face's redirects, https only, eight at most, and stops
  past the pinned size. `OPENDAISUGI_POTION_MODEL` naming a local
  directory is read as it is. A model that cannot be had is no match,
  never a fallback. The tokenizer's tables are generated from the
  `tokenizers` library itself (`tools/gen_potion_tables.py`, `--check`
  says whether they are current), and a `tokenizer.json` with any other
  component is refused. Threshold 0.59.
- `all-MiniLM-L6-v2` and `int8` are refused with the Go binary's line,
  naming `lexical` and `potion` (PW-1).

### Import

A bundle is read, validated as `CompiledPathway`, re-verified and checked
storable before the store is opened, so a bundle the binary cannot read or
verify the Python way is refused with nothing written; the other errors
are then reported in Python's order. The verifier is the crate's own
(`verify.rs`, each violation now with the oracle's message) with the
gate's predicate stage, and the two Z3 stages run on the linked Z3 with
`--z3-timeout-ms`: an `unknown` is a timeout and import refuses with
`VERIFICATION_TIMEOUT`; a Z3 error is a violation.

### Proof and speed

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/pathway_compare.py --binary clients/rust/target/release/daisugi \
    --probe clients/rust/target/release/pathway-probe --oracle --fuzz 2000 --seed 7 --speed
cargo test --release --lib pathways   # BLAKE2b, lexical and tiny-model goldens, 577 verify messages
```

With the real potion model in the Hugging Face cache (`HF_HUB_CACHE`), the
compare also checks the real tokenizer's ids and the real-model find
cases, copying the pinned files into a scratch cache; nothing is
downloaded. Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| CLI cases (fixture from the oracle), stderr included | 158 (158 agree; the 4 PW-R-11 refusals are answered since the garden stage) | 0 |
| violation messages (`verify_messages.jsonl`) | 577 plans | 0 |
| find cases, lexical, tiny and real potion | 380 queries | 0 |
| real tokenizer ids | 3,042 texts | 0 |
| fuzz, seed 7: lexical, tiny potion, real potion (150-row stores) | 6,000 queries (4,554 matched) | 0 |
| fuzz, seed 11 | 6,000 queries (4,612 matched, 1 exact tie accepted) | 0 |
| fuzz, seed 23 | 6,000 queries (4,624 matched, 1 exact tie accepted) | 0 |

Cold process, 1,000 lexical pathways (about 22 MB of vector text):

| | p50 | p90 |
|---|---|---|
| `daisugi pathways list` | 21.6 ms | 27.0 ms |
| `daisugi pathways list --json` | 24.1 ms | 25.9 ms |
| find (`pathway-probe`, one query) | 7.0 ms | 7.1 ms |
| Python `pathways list` | 1,085 ms | 1,090 ms |

`list` validates every row as `list_all` does (the envelope, the plan
and all 4,096 floats of each vector), on four threads (PW-R-10). The
same build agrees on the other suites: `gate_compare.py --oracle` over
`daisugi-gate`, 1,310 cases, 0 disagreements, 0 stale; `cli_compare.py
--oracle` over `daisugi`, 261 cases, 248 agree, 10 not ported and 3
refused as before, 0 stale; `conform` on the frozen corpus, 11,450 of
11,450 (the verifier's messages changed nothing else); and
`tools/check-paths.sh` finds no build path in `daisugi`, `daisugi-gate`
or `pathway-probe`.


## daisugi gardener, tend, hook auto-tend, distill-repeats: the garden in Rust

The rest of the pathway life cycle runs in the binary with the flags,
output, exit codes and files of the Python CLI, as the Go binary runs it
(`clients/go/README.md`, "the garden in Go"). The binary never runs
Python. `registry pull-and-tend` and the other `hook` commands say they
are not in this binary yet, as in Go.

| Module | What it models |
|---|---|
| `garden` | `gardener.prune` and `gardener.merge`, `intersect_permissions`, Python's int / int and `==` on dumps |
| `tracejournal` | `Journal`: the YAML trace bodies and the SQLite index, its schema and migrations, a read-only open for the checks, `list_successful_traces`, `load_trace`, refinements, `log`, the converted-session table; networkx's topological order; the envelope cache |
| `distill` | `Distiller.tend`, `pathway_params` and the gateway worklist |
| `llm` | the structured model call, two backends, and its HTTP(S) POST |
| `capture` | `hook.list_sessions`, `infer_envelope` and the plan a capture makes |
| `pathways::literal` | the Python literals a model writes in place of JSON |

### Model calls

`llm` sends the oracle's request bytes (GD-R-1):

- `claude-code`: `claude -p --model=haiku [DAISUGI_CLAUDE_ARGS]
  --output-format json`, the prompt on stdin, in a fresh directory under
  `$TMPDIR`, 120 s; a reply that does not parse or validate is re-asked
  twice with the error.
- `litellm`: the Anthropic Messages API over HTTP or HTTPS (rustls and
  ring, already in the tree for `llm_check`), with `ANTHROPIC_API_KEY` or
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_BASE` or `ANTHROPIC_BASE_URL`,
  and `REQUEST_TIMEOUT`. Only `anthropic/<model>` is carried; any other
  provider is refused before anything is written.

The schema texts and litellm's max_tokens table are generated from the
libraries (`tools/gen_llm_schemas.py`; `--check` says whether
`src/llm/schemas_gen.rs` is current). The key is sent only as a header
and never printed. No crate was added.

### Verification

Every pathway `tend` stores is verified first by `pathways::verify` on
the linked Z3 (500 ms), and the distiller fails closed (GD-R-3): a
violation, a Z3 error, a Z3 check that answered unknown, or a template
this binary cannot verify drops the cluster. NaN and the infinities are
invalid; an int of any size is read as pydantic reads it.

### Proof and speed

```sh
uv run --no-sync python clients/garden_compare.py --binary clients/rust/target/release/daisugi --oracle --speed --verbose
uv run --no-sync python clients/garden_compare.py --binary clients/rust/target/release/daisugi --no-cases --fuzz 200 --seed 3
uv run --no-sync python clients/rust/tools/gen_llm_schemas.py --check
```

Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| garden cases, stderr on every case | 200 (188 agree, 12 refused or not in the binary, the Go binary's same 12) | 0 |
| the same with `--oracle` (fixture current) | 200 | 0 stale |
| guard: Python reads what the binary wrote | 125 trees (stores and journals) | 0 |
| fuzz, seed 3: prune, merge, run, status on random stores | 200 stores | 0 |
| fuzz, seed 11 | 200 stores | 0 |

Cold process, the binary and Python:

| | binary p50 | Python p50 |
|---|---|---|
| `gardener status`, 1,000 pathways | 18.7 ms | 495 ms |
| `gardener prune --dry-run`, 1,000 pathways | 19.2 ms | 493 ms |
| `gardener merge --dry-run`, 1,000 pathways (all pairs) | 65.1 ms | 5,332 ms |
| `tend`, 7 traces, one cluster salvaged and verified | 141 ms | 741 ms |

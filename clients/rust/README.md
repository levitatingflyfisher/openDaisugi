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
native, not Z3 calls.** The spec says Full-profile clients implement
the predicate-algebra stages through SMT-LIB2 text; these two Z3 calls in the
oracle add only fixed-value boolean/integer conjunctions (`shell == <bool
already known>`, `max_time > 0 and max_time <= 3600`, …) — the "solver
call" always has exactly one possible outcome once the concrete values are
substituted in, so `solver.check() == unsat` reduces to a plain boolean
test with no loss of fidelity. `envelope_subsumes` and `check_vacuity` — the
two call sites that ask a genuine "does *any* assignment exist" question —
do go through real SMT-LIB2 text, per spec, run in the linked Z3's command
interpreter (no `z3` on `PATH`, ADJUDICATIONS VZ-1).

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

**Each query runs in the linked Z3, in one process.** Every query is one
script through Z3's SMT-LIB2 command interpreter; no `z3` program is
started. The corpus needs only a few dozen Z3 round trips (15
vacuity-relevant expressions and 4 skill-delegation cases on the corpus
generation this was measured against), so they never show in the bench
numbers above, which the decompose cases dominate (11,089 of 11,450 on the
current corpus, none of which touch Z3).

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
`envelope_subsumes` take no per-query timeout budget — each query runs
to its own natural completion. Semantically invisible on
this corpus (the formulas are tiny; Z3 answers `sat`/`unsat` in
single-digit milliseconds, well inside any 500ms budget the wire would
have asked for, and `check_vacuity`'s `unknown`-folds-to-`non_trivial`
behavior is preserved regardless of *why* no definite answer came back).
The honest gap: a pathologically expensive predicate would run until the
*conformance runner's* 600s process-level timeout, rather than degrading
gracefully to a `VerificationTimeout`-style warning at the 500ms the
oracle would have honored. A `(set-option :timeout N)` in the script would
close it; not done here, flagged rather than silently absorbed.

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
  | target/release/daisugi-gate --mode audit --root ~/.opendaisugi/gate --format claude
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
stdout, stderr and exit code for each format, the same audit line,
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
- `llm_check` as our own model client makes the call, over http or over
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
through `claude -p`, under settings that change httpx (a CA file, or a
proxy setting it does not read as httpx does, PX-3 and PX-5), or that
fails at the network or TLS level other than a refused connection or a
refused CONNECT (an untrusted certificate, say); a `\N{...}` regex
escape; a regex search past 20 million steps; a regex repeat of more than
10,000 copies inside a vacuity check; a `--verify-timeout` under one
second; and `OPENDAISUGI_CONFORMANCE_RECORD`.

### Differential results and speed

On 2026-09-25, rebased on master 25c38d19:

- gate cases (1,288 then; 1,342 since): 1,288 native, 0 disagreements,
  0 stale against a live oracle. Rerun on 2026-09-27: 1,342 of 1,342
  native, 0 disagreements;
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
`gate settings`, `gate proposals`, `gate serve` (the resident gate,
below), `install --gate [--enforce|--audit]
[--runtime X] [--dry-run] [--yes] [--uninstall]`, `install --harness
pi|opencode [--uninstall] [--dry-run]`, `pathways list|show|stats|
delete|export|import` (see the next section), `gardener`, `tend`,
`hook auto-tend` and `distill-repeats` (the garden, below), and
`gateway`, `gateway-report`, `router status|stop`, `route` and `install
--gateway` (the gateway, below), and `status`, `config`, `start` and
`journal stats|replay|search|parse|ingest` (the everyday commands,
below), `generate-envelope`, `run` and `orchestrate`, and `mcp serve`,
`onboard` and `tiers setup` (below), `modules`, `dashboard` and
`metrics` (stage K4, below), `hook record` (below), and `registry`,
`batch prove` and `release` (stage L, below). Everything else
prints one line,
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
(a two-line wrapper script) is native with 0 disagreements (1,342 of
1,342, rerun 2026-09-27). Cold start: `daisugi --help` p50 2.0 ms, p90 2.4 ms; `daisugi
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
  past the pinned size. It goes through the proxy `urllib` would use
  (ADJUDICATIONS PX-2). `OPENDAISUGI_POTION_MODEL` naming a local
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
Python. The command-line `hook report` says it is not in this binary
yet, as in Go (HR-4).

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
- `api`: our own model client, as `llm_client.py` makes the call: the
  Anthropic Messages API for `anthropic/<model>` and `claude*`, or
  OpenAI-compatible chat completions for `openai/<model>` and
  `ollama/<model>`, over HTTP or HTTPS (rustls and ring, already in the
  tree for `llm_check`), with the oracle's request bytes, keys, base URLs,
  `OPENDAISUGI_LLM_TIMEOUT` and the proxies httpx reads (ADJUDICATIONS
  LLM-1 to LLM-14). The old backend name `litellm` is refused with one
  line that names `api` (LLM-13).

The schema texts are generated from the
oracle's models (`tools/gen_llm_schemas.py`; `--check` says whether
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
| garden cases after the proxy cases (PX-1 to PX-8) | 215 (203 agree, 12 not in the binary as before) | 0 |
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

## daisugi gateway, gateway-report, router, route: the gateway in Rust

The token-saving gateway runs in the binary, as in the Go binary
(`clients/go/README.md`, "the gateway in Go"): an HTTP proxy between a
harness and its model that routes each turn, meters it and journals it,
with the flags, output, files and exit codes of the Python CLI. It never
runs Python.

| Command | What it does |
|---|---|
| `gateway` | the proxy: Anthropic Messages and OpenAI Chat Completions, buffered and streamed, the rules router (ADR-0015 sticky routing), `--router off`, and `--router switchyard` with Switchyard as a managed child |
| `gateway-report` | the calibration report: realized routing, the prompt cache, the reuse ceiling, the Switchyard target shares |
| `router status [--json]`, `router stop` | the router choice, the binary and each child from its state file, recent turns; stop what a gateway left running |
| `route` | the cheapest viable tier for a task, a matching pathway first |
| `install --gateway [--base-url U] [--router R ...]` | the base_url layer: Claude Code's `ANTHROPIC_BASE_URL`, Codex's model provider, OpenClaw's provider; `--router` writes config.yaml and the Switchyard TOML |

| Module | What it models |
|---|---|
| `gateway::server` | `gateway_asgi`: the proxy on a small HTTP/1.1 server, a thread for each connection, and the config reloader |
| `gateway::http` | the upstream client (TCP or rustls, a small keep-alive pool) and httpx's gzip and deflate decoders |
| `netproxy` | proxies from the environment as httpx and urllib read them, and the one connector every HTTP client uses: a CONNECT tunnel then TLS, absolute form for http, credentials from the proxy URL (PX-1 to PX-8) |
| `gateway::pipeline`, `route`, `meter`, `sniff`, `answers`, `report` | `gateway_pipeline`, `routing` and `route_turn`, the meter and `record_turn`, the SSE sniffers, the answer store, `gateway_journal` and `gateway_report` |
| `gateway::pyops` | Python's operations on decoded JSON: a body whose shape makes one raise is forwarded untouched |
| `gateway::recall` | `gateway_recall.recall` and `gateway_answers.recall_answer`, the reuse a harness calls through MCP |
| `switchyard` | `router_switchyard`: the TOML it writes, a reader for the TOML a deployment file uses, the child, its state file, health and stop |
| `cli::config` | now also `save_config`: the whole file restated as pydantic dumps it |

How it matches the oracle, beyond what the Go section says:

- The HTTP stack is the standard library's, with no framework (GW-R-2).
  The upstream call has no timeout and is never tied to the client: a
  client that goes away does not stop the turn, which is read to its end
  and journaled. A turn is never sent upstream twice.
- Token counts are exact integers, as Python's; gzip and deflate are
  decoded with miniz_oxide (pure Rust).
- SIGHUP and `POST /_reload` (loopback only) rebuild the Anthropic wire's
  router from config.yaml. SIGINT drains and exits 0; SIGTERM drains,
  stops the Switchyard child, removes its state file, then ends the
  process by the signal (GW-4).
- The upstream calls go through the proxy httpx would use (GW-R-3,
  PX-1 to PX-8). The rulings are GW-R-1 to GW-R-8 in
  `clients/ADJUDICATIONS.md`.

### Proof and speed

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
(cd ../go && go build -o /some/scratch/fake-upstream ./cmd/fake-upstream)
DAISUGI_RECALL_PROBE=target/release/recall-probe uv run --no-sync python ../gateway_compare.py \
    --binary target/release/daisugi --oracle --fuzz 2000 --seed 1 --probe target/release/gateway-probe \
    --speed --upstream-bin /some/scratch/fake-upstream
```

Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| gateway cases, stderr on every case | 169 (167 agree, 2 refused as ruled: GW-8, GW-9) | 0 |
| the same with `--oracle` (fixture rerun live) | 169 | 0 stale |
| gateway cases after the 54 proxy cases (PX-1 to PX-8) | 223 (221 agree, 2 refused as ruled) | 0 |
| the 54 proxy cases with `--oracle` | 54 | 0 stale |
| CLI cases with the `install --gateway` and `--router` cases | 306 (294 agree, 12 not ported or refused as before) | 0 |
| pipeline fuzz, seeds 1 to 6, chunks of at most 500 turns | 12,000 turns | 0 |

The gateway's added time on a buffered turn, 400 turns each way against
Go's `cmd/fake-upstream`, alternating: straight to the upstream p50
0.24 ms, through the gateway p50 0.46 ms, so 0.22 ms added at p50
(target: under 5 ms; the Go binary adds 0.88 ms).

## daisugi gate serve: the resident gate in Rust

`daisugi gate serve [--root R]` is the Rust port of the Go binary's
resident gate (`clients/go/README.md`, "daisugi gate serve"): the server
the pi extension and the OpenCode plugin ask over
`~/.opendaisugi/gate/gate.sock`, with the oracle's wire, socket modes and
lifecycle, and each call decided in a child it starts as `daisugi gate
check`, fed the request on stdin, so a crash is the format's deny. A
connection runs on a thread with a 256 MB stack, since a request may nest
9,992 deep. `daisugi start` starts it, as the Go binary's does (G2-4),
and `install --harness` says so. Rulings G2-1
to G2-8 in `clients/ADJUDICATIONS.md`; G2-3 is the Rust binary's alone
(a `--verify-timeout` that is not finite is not decided, as RG-12).

```sh
target/release/daisugi gate serve --root ~/.opendaisugi/gate
uv run --no-sync python clients/gate_server_compare.py \
    --binary clients/rust/target/release/daisugi --port rust
```

Results on the reference box, 2026-09-27: 198 cases (190 requests, 8
scenarios), 198 agree. The gate, CLI, pathway, garden and gateway cases
rerun on the same binaries: 0 disagreements (1,342 gate calls native;
CLI 295 agree, 175 not ported, 3 refused).

A gate decision over the socket (connect, one request, the reply), 300
calls in a row after 20 to warm up:

| Server | allow p50 | allow p90 | deny p50 | deny p90 |
|---|---|---|---|---|
| Python (`gate_server.py`) | 6.8 ms | 7.2 ms | 4.6 ms | 5.2 ms |
| Go `daisugi gate serve` | 5.9 ms | 6.5 ms | 7.4 ms | 8.3 ms |
| Rust `daisugi gate serve` | 4.8 ms | 5.4 ms | 5.9 ms | 6.3 ms |

The allow is `ls` and the deny `rm -rf /` under the cases' base envelope,
in enforce mode, on a 4-core box under a load of about 5 from other work.
Each call in a port starts a child process, the price of a crash never
reading as an allow; the Python server decides in its own process, warm
after its one import. A deny costs a port about 1.5 ms more than an
allow, where the Python server's deny is the faster of its two; the
cause is not measured here.

## daisugi status, config, start and journal: the everyday commands in Rust

`daisugi status`, `config`, `start` and `journal stats|replay|search|
parse|ingest` run in the binary with the flags, output, exit codes and
files of the Python CLI, as the Go binary runs them
(`clients/go/README.md`), with the Go binary's rulings (C-10 to C-17 in
`clients/ADJUDICATIONS.md`) and three of its own (C-R-1 to C-R-3):

- `status` reads the matcher from `~/.opendaisugi/config.yaml` and says a
  matcher the binary does not carry (MiniLM, int8) is off; it asks
  nvidia-smi for the GPU and never torch.
- `config` lists every setting with its source, as `resolved_config` does.
- `start --no-ui` or `--dry-run` installs the gate hook for this
  directory, registers a starter envelope keyed to it and starts this
  binary's own `gate serve`; there is no view, and `start` without either
  flag is refused.
- `journal` reads and writes the oracle's journal. `replay` and `ingest`
  word each permissions and DAG violation's detail as the oracle does and
  refuse a stored result whose detail they do not word (C-14); `search`
  runs the lexical and potion matchers (C-15); `parse` reads Claude Code
  and Codex transcripts and splits a long episode through claude-code or
  the HTTP backend (C-16).

Results on the reference box, 2026-09-27: 498 CLI cases, 478 agree, 11
not ported and 9 refused, the same cases in each class as the Go
binary's; 0 disagreements. On the same final binaries: the gate cases
(1,348 calls, all native), the pathway cases (158 CLI cases and 245
finds), the garden cases (227; 216 agree, 11 not ported), the gateway
cases (223; 221 agree, 2 refused) and the resident gate cases (200):
0 disagreements. The 577 verify-message cases pass in `cargo test`. A cold run, p50 on a fresh home, inside a
`systemd-run` scope with a 4 GB memory cap (which about doubles every
time here), under a load of about 1.5: `daisugi status`, with the real
nvidia-smi on PATH, Rust 70.8 ms, Go 67.1 ms (60 runs each), Python
3.49 s (9 runs); without nvidia-smi on PATH, Rust 8.5 ms, Go 12.0 ms.
`daisugi config`: Rust 7.2 ms, Go 9.9 ms, Python 472 ms. nvidia-smi
itself takes about 22 ms outside the scope.

## daisugi generate-envelope and envgen: model-written envelopes in Rust

`daisugi generate-envelope TASK` asks a model for a safety envelope, as
the Python command and the Go binary do: the same flags (`--model`,
`--stakes`, `--low-stakes-envelope`, `--thinking-budget`, `--llm`,
`--json`, `--allow-shell-decomposition`), the same system prompt and
request bytes, the same YAML or JSON out, and the same exit codes.

The rest of stage K1 is the library `src/envgen`, a port of the Go
client's `internal/envgen`:

| File | What it models |
|---|---|
| `mod.rs` | `generate_envelope`: task checks, stakes, the Tier-0 pathway lookup (with `increment_hit`), the Tier-1 slot, the envelope cache with its refinement bust, the model ladder with refinement hints and thinking kwargs, inheritance |
| `cache.rs` | `EnvelopeCache` and `make_cache_key`, the oracle's SQLite file |
| `tier1.rs` | `HTTPTier1Provider` (its 30 s deadline covers the whole call) and `load_configured_tier1` |
| `inherit.rs` | `verify_inheritance`, worded as the oracle words it |
| `bind.rs` | `bind_parameters` and `apply_bindings` |
| `compose.rs` | `compose`: frozen pathways as skill handlers and contract envelopes |
| `prompts_gen.rs` | the prompt texts, written from the oracle by `tools/gen_envgen_prompts.py` (`--check` says when they are stale) |

The model client (`src/llm`) now sends the thinking kwargs and takes a
`base_url`, an `api_key` and a deadline for the whole call.
`gateway::recall` binds a typed pathway's holes through `envgen::bind`
before it verifies the plan against the caller's envelope (K1-7). The
self-consistency check runs on the linked Z3, and a check that did not
finish refuses the envelope, as the oracle now does (K1-2). A bound plan
whose verify did not finish gives the template (K1-3). The Rust binary
follows the Go binary's rulings K1-1 to K1-7; K1-R-1 and K1-R-2 in
`clients/ADJUDICATIONS.md` add what is the Rust port's own.

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/k1_compare.py --binary clients/rust/target/release/daisugi \
    --probe clients/rust/target/release/envelope-probe --verbose
cargo test --release --lib envgen
```

Results on the reference box, 2026-09-27: 186 K1 cases, 183 agree, 0
disagreements. The three refused are the Go binary's three: a proxy
setting the binary does not read the httpx way (PX-5), a MiniLM matcher
(PW-1) and a Tier-1 config whose model is not a string (K1-6). Python
read back every pathway store and envelope cache the binary wrote (24
cases). `cargo test` makes the linked Z3 answer `unknown` and sees
`self_consistent` refuse the envelope and `plan_verifies` refuse the bound
plan, which `bind` then drops for its template. On the same
binaries: gate 1,348 calls, all native; CLI 498 cases (478 agree, 11 not
ported, 9 refused); pathway 158 cases and 380 finds; garden 227 cases,
run in parts; gateway 223 (221 agree, 2 refused as ruled); resident gate
200 agree. 0 disagreements in each.

One CLI case, `install gateway idempotent`, disagreed when the binary ran
from a copy named `rs-daisugi`: the hook reader knew the gate only by the
program's file name `daisugi` (INS-1). The CLI compare runs the binary
through a symlink named `daisugi`, so no case depends on where the binary
sits. The reader in all three languages now also knows the gate by the
hook's shape: the installer's marker, `gate check`, and the four flags the
installer writes. A binary installed under another name writes a hook that
status reports, uninstall removes and a second install does not repeat;
cases with `prog` run the binary as `daisugi-copy` to show it.

## daisugi run and daisugi orchestrate: the supervisor and the orchestrator in Rust

`daisugi run PLAN -e ENVELOPE` runs a plan under the supervisor, and
`daisugi orchestrate PROMPT` runs a prompt end to end, as the Python
commands and the Go binary do: the same flags, output, journal rows,
receipts and exit codes (for `run`: 0 succeeded, 1 failed, 2 rejected, 130
aborted). With no `--envelope`, `orchestrate` generates one first through
`src/envgen`. The port follows the Go client's `internal/supervise` and
`internal/orchestrate`.

| Module | What it models |
|---|---|
| `src/supervise` | `Supervisor.run`: the whole-plan verify first, `verify_step` per step, the fallback (halt, or the recompute model call), the approval stack (the allowlist, `DAISUGI_APPROVE`, a terminal prompt, deny), the shell, file_read, file_write (with its reversal handle), network and dry-run executors, stage 2, receipts with `compute_evidence_hash`, the integrity check, `log_run` |
| `src/orchestrate` | `decompose` (the DAG check and the verify), `model_sizer`, `BudgetTracker`, the budget-aware task executor on both backends (the HTTP reply's token count, or Claude Code's own meter), the skill and MCP executors with nothing registered, `synthesize` |
| `src/pathways/verify.rs` | `verify_step`, `stage2.verify_completed_step`, and the refusal of an envelope the binary cannot check the oracle's way |
| `src/tracejournal/runs.rs` | `log_run`, `append_receipt`, `receipts_for_run`, `write_refinement` |
| `src/cli/runcmd.rs`, `src/cli/orchestratecmd.rs` | the two commands |

The model client (`src/llm`) now reads the reply's token count, makes a
completion with a timeout, meters a `claude -p` call by Claude Code's own
accounting, and gives a call with no `max_retries` the client's default
(3 on HTTP, 0 on claude-code). `tools/gen_llm_schemas.py` writes the
decomposer's, the synthesizer's and every step type's schema texts.

The binary follows the Go binary's rulings K2-1 to K2-9. A Z3 check that
does not finish fails closed wherever K2 verifies (K2-2). K2-R-1 in
`clients/ADJUDICATIONS.md` says what is the Rust port's own.

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/k2_compare.py --binary clients/rust/target/release/daisugi \
    --max-refused 7 --verbose
cargo test --release --lib -- supervise orchestrate cli::runcmd
```

Results on the reference box, 2026-09-27: 116 K2 cases, 109 agree, 0
disagreements. The 7 refused, the tree unchanged, are the Go binary's 7 by
name: YAML the binary does not read (`run envelope not yaml`, `run plan
not yaml`, `orch envelope not yaml`; K2-3), `--max-parallel` above 1
(K2-4), a stale-embeddings warning (K2-5), and an `llm_check` invariant and
postcondition (K2-8). (Since then all but K2-8 retired: YM-1, MP-1, PG-1 and,
on 2026-10-08, RF-4 for the warning.) Python read back every journal and pathway store the
binary wrote (99 cases). `cargo test` reaches what no case can: the
per-step rejection and the recompute fallback (K2-1), and a Z3 check that
answers `unknown` in the verify before `run`, in the decomposition, before
`orchestrate`'s run, per step and on a recomputed step (K2-2). On the same
binaries: gate 1,348 calls, all native; CLI 507 cases (487 agree, 11 not
ported, 9 refused); pathway 158 cases and 245 finds (the 2 real-potion
cases skipped: the scratch HF_HOME held no model); garden 227 cases, run
in parts with `--only` and `--skip`; gateway 223 (221 agree, 2 refused as
ruled); resident gate 200 agree; K1 186 (183 agree, 3 refused as ruled). 0
disagreements in each. `cargo test --release` passes.

## daisugi mcp serve, onboard and tiers setup: stage K3 in Rust

`daisugi mcp serve` is the MCP server the oracle's `mcp_server.py`
exposes, with its 11 tools (`envelope_for`, `find_pathway`, `recall`,
`recall_answer`, `verify_plan`, `verify_completed_step`, `list_pathways`,
`pathway_stats`, `run_plan`, `receipts_for_run`, `recent_runs`). As in
Go (DS-6, K3-1) it takes no MCP SDK: `src/mcpwire` is a JSON-RPC 2.0
server over stdio that answers what the oracle's FastMCP answers, byte for
byte. It reads each line with universal newlines and bad UTF-8 replaced,
parses it with jiter (pydantic's own parser, so a line the SDK rejects is
rejected the same way), checks each request as the SDK does, pre-parses
and validates the tool arguments with `src/pathways/pmodel`, and writes
pydantic's JSON text. Each reply is written and flushed at once. The tools
run on the existing libraries: `src/envgen`, the pathway store,
`src/gateway/recall.rs`, the verifier, the supervisor and the journal.
`tools/gen_mcp_tools.py` takes the tools/list result, the capabilities
and the protocol versions from the oracle; `--check` fails when they
drift.

`daisugi onboard` finds Claude Code and Codex transcripts as
`discover_transcripts` does, parses them with the split cache, ingests
each episode's steps as step models, and runs tend. It reads, parses and
verifies everything before its first write (K3-11). Its embedder check
reads the matcher in effect: a key that nothing builds refuses a real run
and adds a note to a dry run; a matcher the oracle builds and this binary
does not (MiniLM) is refused (K3-10). `daisugi tiers setup` probes the box
as the oracle does on Linux without torch (K3-9), recommends a model size,
and qualifies and wires a local `/v1` endpoint. `daisugi setup` names its
replacement.

The binary follows the Go binary's rulings K3-1 to K3-12. A request it
cannot answer the oracle's way gets JSON-RPC error -32000 "... is not in
this binary yet." and the server goes on (K3-3); a tool call that panics
is taken the same way. A Z3 check that does not finish fails closed in the
MCP tools (K3-5). `tiers setup --remote` and `tiers stats` are not in this
binary yet. K3-R-1 in `clients/ADJUDICATIONS.md` says what is the Rust
port's own.

| Module | What it models |
|---|---|
| `src/mcpwire` | the line reader, which lines are requests, notifications or invalid, the SDK's parameter checks, the reply lines, the tools' argument models and pre-parse, pydantic's compact and indented JSON |
| `src/cli/mcpcmd.rs` | `daisugi mcp serve` and the 11 tools |
| `src/cli/onboardcmd.rs` | `daisugi onboard`: discovery, the split cache, the ingest, the report |
| `src/cli/setupcmd.rs` | `daisugi tiers setup` and `daisugi setup` |
| `src/cli/modelscmd.rs` | `daisugi models list`, `search` and `use`: the catalog (`model_catalog.json`, included at build time), the default by hardware, the Hugging Face search and its filters (MC-R-1 to MC-R-7); `models pin` (`src/cli/modelspin.rs`), `--pull` included, as huggingface_hub does it (RF-7) |
| `src/pathways/verify.rs` | `stage2_violations`: `verify_completed_step` with each violation's detail |
| `src/tracejournal/runs.rs` | `receipts`: `receipts_for_run` as the Receipt model reads it |

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/rust/tools/gen_mcp_tools.py --check
uv run --no-sync python clients/k3_compare.py --binary clients/rust/target/release/daisugi \
    --max-refused 3 --verbose
cargo test --release --lib -- mcp onboard setupcmd
```

Results on the reference box, 2026-09-28: 207 K3 cases, 204 agree, 0
disagreements, in one run with `--max-refused 3`. The
3 not ported are the Go binary's 3 by name: `setup remote`, `tiers stats`
(K3-9) and `onboard no matcher config` (a MiniLM matcher, K3-10). Python
read back every journal and pathway store the binary wrote (175 cases).
`cargo test` reaches what no case can: a Z3 unknown in `verify_plan`
(K3-5), a refused MCP method with the server going on, a lone surrogate in
an argument (K3-3), and the CPU-only recommendation for fixed profiles
(K3-9). On the same binaries: gate 1,348 calls, all native; CLI 507 cases
(487 agree, 11 not ported, 9 refused); K1 186 (183 agree, 3 as ruled); K2
116 (109 agree, 7 not ported); pathway 158 cases and 245 finds (the 2
real-potion cases skipped: the scratch HF_HOME held no model); garden 227
cases in four parts (216 agree, 11 not ported); gateway 223 (221 agree, 2
refused as ruled); resident gate 200 agree. 0 disagreements in each.
`cargo test --release` passes, and `tools/check-paths.sh` finds no
build-machine path.

## daisugi modules, dashboard and metrics: stage K4 in Rust

`daisugi modules [--data-dir D] [--json]` draws the module map;
`daisugi dashboard [--data-dir D] [--once] [--json] [--interval S]` adds a
gauge per stage read from the journal, the pathway store and the gateway's
turn journal: one frame when stdout is not a terminal or with `--once`,
JSON with `--json`, and on a terminal a new frame every interval until
Ctrl-C. `daisugi metrics [--data-dir D] [--serve [--host H] [--port P]]`
prints the same numbers as Prometheus text (0.0.4), or serves them at
`/metrics`. `daisugi start` opens the plain live view after its steps
(K4-R-1), and the root `--plain` drops the box drawing.

| Module | What it models |
|---|---|
| `src/cli/modulescmd.rs` | `modules.detect_stages`, `render_wiring`, `wiring_json` |
| `src/cli/dashboardcmd.rs` | `dashboard.read_raw`, `collect_metrics`, `render_dashboard`, `dashboard_json`, `run_live`; `exporter.render_prometheus` and the `/metrics` handler |

The binary follows the Go binary's rulings K4-1 to K4-7: the map answers
as a base install with the binary's extras, the Textual views are refused,
and input the oracle raises on is refused before any write. The map, the
floor and `status` read the journal read-only and make nothing (K4-8).
The `/metrics` server is the binary's own (K4-R-2). An unknown long option
names click's close matches in every command (K4-R-4).

## daisugi hook record: the capture and floor-report hooks

`daisugi hook record [--format F] [--event E] [--captures-root R]` is the
hook `install` writes for Claude Code's capture, Stop, Notification and
subagent events, for Hermes and for the OpenClaw plugin. It reads one
payload on stdin, records the call or reports the session's state to
coppice or herdr with its session-tree entry, and prints the host's
contract. Past the options it always exits 0 (HR-1), so a Stop hook never
blocks the stop. `src/gate/hookrecord.rs` holds the payload half and
`src/cli/hookcmd.rs` the options and the background tend trigger (HR-2).
`hook list` and `hook to-trace` are stage F (below); the command-line
`hook report` stays refused; no hook runs it (HR-4).

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/k4_compare.py --binary clients/rust/target/release/daisugi \
    --max-refused 5 --verbose
cargo test --release
```

Results on the reference box, 2026-09-28: 193 K4 cases (the module maps,
dashboards and metrics runs, 7 cases over a fresh box and an inf hit sum,
and 76 hook cases),
188 agree, 0 disagreements; the 2 not ported (K4-3) and 3 refused (K4-4)
are the Go binary's 5 by name. The integration test drives the terminal
view over a pty (K4-R-3). On the same binaries: gate 1,348 calls, all
native; CLI 508 cases (490 agree, 9 not ported, 9 refused; the 3 `start
view` cases agree); K1 186 (183 agree, 3 as ruled); K2 116 (109 agree, 7
not ported); K3 207 (204 agree, 3 not ported); pathway 158 cases and 245
finds (2 real-potion cases skipped); garden 227 (216 agree, 11 not
ported); gateway 223 (221 agree, 2 refused as ruled); resident gate 200
agree. 0 disagreements in each. `cargo test --release` passes.

## Stage L in Rust: the registry, batch proofs, release signing

The binary carries the Go binary's stage L (`clients/go/README.md`, "Stage
L in Go"): `registry init|pull|publish|status|pull-and-tend`, `batch
prove DECL -e ENV`, and `release keygen|sign|verify`, with the oracle's
flags, output and exit codes.

| Module | What it models |
|---|---|
| `src/signing.rs` | `signing.py`: base64 read as CPython's `b64decode`, `sign_bytes`, `verify_bytes` (ed25519 through ring, L-R-1), the trusted-signer registry, contract signatures |
| `src/bundle.rs` | `pathway_bundle.py`: the canonical payload, `compute_bundle_hash`, `pathway_to_bundle`, `bundle_to_pathway` |
| `src/registry.rs` | `git_pathway_store.py`: open (and materialize), `pull`, `publish`, `status`; git as a child with no inherited `GIT_*` variable and a ceiling above the clone (L-15) |
| `src/batch.rs` | `batch.py`: classify, resolve, `prove_footprint`, `would_be_reversible`, the two-ledger meter, `run_batch`, `rollback_result` |
| `src/deeds.rs` | `deeds.py`: `apply_reversal`, `rollback_run`, `touched_files` |
| `src/strata.rs` | `strata.py`: the strata store, `reconstruct_context`, `promote_constraint` |
| `src/lerr.rs` | how a stage-L operation stops: the exception the oracle raises, pydantic's error, an OS error, or a refusal |
| `src/cli/registrycmd.rs`, `batchcmd.rs`, `releasecmd.rs` | the commands |
| `src/cli/lprobe.rs`, `src/bin/l_probe.rs` | `l-probe`, a test instrument (not shipped): one query of the library parts no command reaches (L-1) |

Keys are the oracle's: the raw 32-byte seed and the raw public key in
base64 (L-4). A bundle is read only in the form `yaml.safe_dump` writes;
any other file is skipped (L-R-2). The rulings are L-1 to L-15 and L-R-1
to L-R-3 in `clients/ADJUDICATIONS.md`.

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/l_compare.py --binary clients/rust/target/release/daisugi \
    --probe clients/rust/target/release/l-probe --max-refused 0
cargo test --release
```

Results on the reference box, 2026-09-28: 239 L cases (144 commands, 95
library probes), 239 agree, 0 refused, 0 disagreements; the hostile git environment and repo-inside-a-repo cases
agree too (L-15). On the same binaries: gate 1,348 calls, all native; CLI
508 cases (490 agree, 9 not ported, 9 refused); K1 186 (183 agree, 2 not
ported, 1 refused); K2 116 (109 agree, 7 not ported); K3 207 (204 agree, 3
not ported); K4 193 (188 agree, 2 not ported, 3 refused); pathway 158
cases and 380 finds; garden 227 (217 agree, 10 not ported: `registry
pull-and-tend no clone` now agrees); gateway 223 (221 agree, 2 refused);
resident gate 200 agree. 0 disagreements in each. `cargo test --release`
passes, and `tools/check-paths.sh` finds no build-machine path in
`daisugi`, `daisugi-gate` or `l-probe`.

## Stage F in Rust: the transcript parsers, ingest and the capture commands

The binary carries the Go binary's stage F (`clients/go/README.md`, "Stage
F in Go"). `daisugi journal parse`, `journal ingest` and `onboard` were in
the binary before (C-14, C-16, K3). Stage F adds Python's answer where the
binary refused a transcript, `daisugi hook list [--captures-root R]
[--json]`, and `daisugi hook to-trace ID [--captures-root R] [--data-dir
D] [--task T] [--allow-shell-decomposition]`.

| Module | What it models |
|---|---|
| `src/gate/pymodel.rs` (`decode_stream`) | `open(path, encoding="utf-8")` read line by line: 8192-byte decode calls and the `UnicodeDecodeError` they raise (F-2, F-R-1) |
| `src/transcript` | `parsers/claude_code.py` and `parsers/codex.py`: every exception the parse raises, worded as Python words it; values kept as the transcript holds them until the step models validate them (F-3, F-5) |
| `src/capture.rs` | `hook.list_sessions` and the records `captures_to_trace` reads; `list.sort(reverse=True)` (F-6, F-R-2) |
| `src/cli/hookcapcmd.rs` | `hook list` and `hook to-trace` (F-7) |
| `src/cli/autotendcmd.rs` (`convert_capture`), `src/cli/journalparse.rs` (`journal_result`) | one capture conversion and one verify result as a trace stores it, shared by ingest, auto-tend and to-trace (F-R-3) |

An exception the parse raises is Python's text: `journal parse` prints
`Parse error: <text>` after the backend note and exits 2, `onboard` warns
and goes on (F-3). A line nested past 900 levels is refused (F-4). The
capture commands name an exception the oracle does not catch and exit 1;
`hook to-trace` makes the journal first, as Python does (F-7). A trace
whose verify fails is journaled with its violations worded; `auto-tend`
does the same (GD-8 retired). The rulings are F-1 to F-9 and F-R-1 to
F-R-3 in `clients/ADJUDICATIONS.md`.

### Proof

```sh
cd clients/rust && source tools/z3-build-env.sh && cargo build --release
uv run --no-sync python clients/f_compare.py --binary clients/rust/target/release/daisugi --max-refused 1
cargo test --release
```

Results on the reference box, 2026-09-28: 199 F cases, 198 agree, 1
refused (F-4, the Go binary's same case), 0 disagreements; the six cases
F-10 found agree. On the same binaries: gate 1,348 calls, all native; CLI
508 cases (490 agree, 9 not ported, 9 refused); K1 186 (183 agree, 2 not
ported, 1 refused); K2 116 (109 agree, 7 not ported); K3 207 (204 agree, 3
not ported); K4 193 (188 agree, 2 not ported, 3 refused); L 239 agree;
pathway 158 cases and 245 finds (2 real-potion cases skipped); garden 226
(219 agree, 7 not ported; the two auto-tend cases GD-8 refused now
agree); gateway 223 (221 agree, 2 refused); resident gate 200 agree. 0
disagreements in each. `cargo test --release` passes, and
`tools/check-paths.sh` finds no build-machine path in `daisugi` or
`daisugi-gate`.

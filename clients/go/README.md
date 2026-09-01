# clients/go: the Go gate and the Go conformance client

Two binaries share this module:

- `cmd/daisugi-gate`, the tool-call gate. It answers each hook call the
  way `python -m opendaisugi.gate` does, with no Python at run time.
- `cmd/conform`, the verifier conformance client. It speaks the wire
  protocol of `docs/spec/conformance.md`.

## Build

Both binaries link C and C++ code (Z3, the tree-sitter runtime and the
tree-sitter-bash grammar) through cgo. Build those once:

```sh
cd clients/go
scripts/native.sh     # fetch the pinned sources, check their SHA-256, build, link .native
scripts/build.sh      # daisugi-gate, daisugi and conform, with -trimpath, then check-paths.sh
go test -p 1 ./...
```

`scripts/native.sh` needs gcc, make, cmake, git, curl, python3 (Z3's own
build runs it) and zig 0.16.0 exactly. Z3 is compiled with `zig c++`, and
zig's libc++, libc++abi and libunwind are linked with it, so the binary
needs no libstdc++. The same script builds libghostty-vt for coppice. It
builds on real disk (never `/tmp`), in
`$XDG_CACHE_HOME/opendaisugi/native-build` (`~/.cache` when
`XDG_CACHE_HOME` is unset), and installs into
`$XDG_CACHE_HOME/opendaisugi/native/<stamp>`. `<stamp>` is a hash of the
pinned versions and digests, the compiler flags, the C compiler, zig and
the CPU architecture, so a change to any of them makes a new prefix and
never reuses an old one. `scripts/native.sh --print-prefix` prints the
path, and `--print-key` a hash of the prefix's manifest. The build scripts
put that key in `CGO_CFLAGS`, which Go hashes into its cache key: Go does
not hash the static libraries a cgo LDFLAGS line names, so without it a
changed prefix would link the old libraries from the cache. The git-ignored symlink `clients/go/.native` points at it, and the
cgo flags look there. `DAISUGI_NATIVE_PREFIX` and
`DAISUGI_NATIVE_BUILD_DIR` override the two paths, and
`DAISUGI_NATIVE_JOBS` the build's parallel jobs (2 by default). Versions
and digests are in `NOTICE`: Z3 5.1.0, tree-sitter 0.26.0 and
tree-sitter-bash 0.25.1, the versions the Python oracle runs.

To build and install coppice, sprig and `daisugi` together, run
`scripts/install.sh` at the repo root. `scripts/release.sh VERSION` there
builds the release tarball.

A published binary names no path of the box that built it. The Go build
uses `-trimpath`; `native.sh` compiles Z3 and tree-sitter with
`-ffile-prefix-map`, `-fmacro-prefix-map` and `-fdebug-prefix-map`, so
`__FILE__` in Z3's assertions reads `/daisugi-build/...`; and
`scripts/check-paths.sh BINARY...` fails when `strings` finds `$HOME`,
the checkout, or a Go cache path in a binary. `native.sh` also compiles
for the baseline CPU, checks `zig version`, and writes a stamp and a
SHA-256 manifest of the prefix. It uses an existing prefix only when both
match, and otherwise stops and prints the command that rebuilds it. zig's C++ runtime has no published digest (zig
compiles it on the box from its own release tarball, which
`harness/coppice/scripts/toolchain.sh` checks), so the zig version pins
it.

The gate binary is about 32 MB and dynamically links only glibc:

```
$ ldd daisugi-gate
	linux-vdso.so.1
	libm.so.6 => /lib64/libm.so.6
	libc.so.6 => /lib64/libc.so.6
	/lib64/ld-linux-x86-64.so.2
```

## daisugi-gate

```sh
echo '{"session_id":"s1","tool_name":"Read","tool_input":{"file_path":"/work/a"},"cwd":"/work"}' \
  | ./daisugi-gate --mode shadow --root ~/.opendaisugi/gate --format claude
```

It reads what the Python gate reads: the same argv (parsed as Python
3.12's argparse parses it, prefixes and errors included), the payload on
stdin, the environment and the gate root (envelopes, the `DISARMED`
marker, `config.yaml` beside the root, asks and answers, the session
tree). It writes what the Python gate writes: stdout, stderr and the exit
code for each format, the shadow log line, the session tree entries, the
captures mirror, the coppice or Herdr state report, checkpoint refs, ask
files, proposals, `last_dispatch.json` and conformance records. Apart from
times, latencies and random ids, every byte is the Python gate's
(`clients/gate_compare.py` checks this).

### How it matches the oracle

The gate is a port of the Python gate, down to the libraries it leans on:

| Package | What it models |
|---|---|
| `internal/tsbash` | tree-sitter-bash through cgo, with LC_CTYPE set as CPython sets it (the scanner reads it) |
| `internal/shell` | `shell_decompose.decompose_command`, node for node, with its frame depth |
| `internal/z3` | Z3's C API, terms built the way z3py builds them |
| `internal/pyre` | Python 3.12's `re` for str patterns: parser, errors, warnings, a backtracking matcher |
| `internal/pystr` | Python `str`: code points, lone surrogates, repr, case mapping, strip and split, the codecs |
| `internal/pyjson` | `json.loads` and `json.dumps`, depth and digit limits included |
| `internal/pmodel` | the pydantic models the gate validates (Envelope, Config, the predicate union), error text included |
| `internal/pyyaml` | `yaml.safe_load` for the plain YAML a config file holds |
| `internal/gate` | the gate itself: rules, records, effects and tiers, the verifier stages, logs, asks, checkpoints, dispatch |

The verifier stages run in the gate: permissions, envelope
self-consistency and plan-against-envelope (ground constraints decided
directly; `z3stages_test.go` asks Z3 the oracle's own formulas over every
combination and checks they agree), the predicate stage (invariants and
postconditions, quantifiers, the vacuity check on Z3, `regex_to_z3`), and
the robotics stage (which finds no target in a one-step gate plan).

Two further ports keep the gate from ever allowing where the oracle
denies:

- **Depth parity.** The oracle denies very deep input through
  RecursionError. The gate counts the oracle's Python frames on every path
  that reaches `decompose_command`, `realpath` and `json.loads`, and raises
  at the same depth. `DAISUGI_GATE_DEPTH_LOG=<file>` writes the depth of
  each such call, for comparison with a probe in the oracle.
- **Crash backstop.** Each call is decided in a child process of the
  binary. A fatal error, a signal, a stack past 256 MB or a child past
  its deadline becomes the format's deny contract (exit 2, or a block
  body for hermes and openclaw), never an allow. The deadline is the
  verify budget, plus the ask budget with `--ask`, plus 3 s, so the child
  ends before the host's hook timeout, which the installer sets at least
  5 s past those budgets. The child sends its verdict to the parent on
  an inherited pipe (fd 3), with a nonce the parent chose; on its own
  stdout and exit code it writes only the format's deny. So a child that
  dies, and a `DAISUGI_GATE_CHILD` variable set by anyone else, both
  deny. The child has done all its work when it closes the pipe, so a
  complete frame is the answer at once and the child is then killed, not
  waited for. The parent reads at most 16 MiB of stdin, as the oracle does.

### Calls it does not decide

A call the port cannot decide is denied with exit 2 and a reason
(`the Go gate cannot decide this call (...), so it denies it`). None of
these occurs in the case suite, the fuzz or the corpus:

| Denied undecided | Why |
|---|---|
| an `llm_check` through the claude-code backend, to a model that is not `anthropic/...`, or under a proxy, a CA bundle or a `LITELLM_*` setting | the port is litellm's one Anthropic call, not litellm |
| an `llm_check` reply or network failure litellm words in a way the port does not model | the failure text is the deny reason, and is not guessed at |
| a `config.yaml` outside the YAML subset `internal/pyyaml` reads | not guessed at |
| no home directory at all | Python cannot import the gate |
| input past what a probe can check (a transcript line nested near the JSON limit, a pid past a C int) | the oracle's behavior there depends on its stack |

`llm_check` is the oracle's own model call: one POST to Anthropic's
messages API with litellm's headers, body and timeout, the verdict read
from the reply, and every failure worded as litellm words it (the tests
use a fake server, never a model). Warning regexes, `--help` at any
width and `--verify-timeout` of any value are decided natively. A
`--verify-timeout` under 1 s denies as a timeout (see GT-30).

`DAISUGI_GATE_TRACE=<file>` appends one JSON line per call: `native`,
`unported` (denied undecided, with the reason) or `crash`. It never
changes stdout, stderr or the exit code.

Known limits are recorded in `clients/ADJUDICATIONS.md` (GT-19 to
GT-37). The file-glob matcher, exponential in `**` segments, stops after
a fixed 100,000 steps in the oracle and in the gate alike
(`verify.GLOB_MATCH_STEP_LIMIT`), so no answer depends on the speed of
the box.

### Proof and speed

`clients/gate_cases.py` writes the case suite from the oracle
(`clients/fixtures/gate/cases.jsonl`, 1,285 calls, the 790-command
redirect list among them); `clients/gate_compare.py` runs the binary over
it and compares every stream, log and file. `--fuzz N --kind K --seed S`
runs seeded generators (`clients/gate_fuzz.py`): compound shell commands
under three envelopes, heredocs, multi-line commands, non-ASCII text, and
envelopes with invariants, postconditions and robotics bounds built from
the oracle's models.

```sh
uv run --no-sync python clients/gate_cases.py                 # rewrite the cases from the oracle
uv run --no-sync python clients/gate_compare.py --oracle      # compare, and check the fixture is current
uv run --no-sync python clients/gate_compare.py --fuzz 10000 --seed 7
uv run --no-sync python clients/gate_compare.py --fuzz 3000 --kind envelope --seed 11
```

Results on the reference box, 2026-09-25:

| Run | Calls | Native | Disagreements |
|---|---|---|---|
| case suite (`--oracle`, fixture regenerated and unchanged) | 1,285 | 1,285 | 0 |
| shell fuzz, seeds 5, 7, 11, 13, 23 and 42 (2,000 commands each), three envelopes | 36,000 | 36,000 | 0 |
| heredoc fuzz, seed 11 | 6,000 | 6,000 | 0 |
| multi-line fuzz, seed 11 | 6,000 | 6,000 | 0 |
| non-ASCII fuzz, seeds 7, 11 and 23 | 6,000 | 6,000 | 0 |
| envelope fuzz (predicates, `llm_check`, warning regexes, robotics), seeds 7, 11 and 23 | 6,000 | 6,000 | 0 |
| frozen local corpus: every unique shell command against two envelopes | 22,178 | 22,178 | 0 |

No run found a call the binary allowed and the oracle denied. The fuzz
compares against the oracle in-process; the case suite and the corpus
compare full streams and logs.

Wall time per call, a cold process each time (the case suite):

| | p50 | p90 |
|---|---|---|
| daisugi-gate, all cases | 8.8 ms | 9.9 ms |
| daisugi-gate, a predicate case that runs the Z3 vacuity check | 22 ms | 25 ms |
| Python gate | 434 ms | 476 ms |

A call starts the binary twice (parent and child), about 2.5 ms each:
the Go runtime, cgo and package init. Package-level regexps compile on
first use (`internal/lazyre`), since compiling all of them cost about
1.1 ms of every start. Starting Z3 costs about 8 ms, nearly all of it
page faults on the 8.5 MB term table Z3 5.1.0 always allocates, and its
first check about 3 ms more; so the ground checks are decided directly
and Z3 runs only where the predicate stage needs it.

## conform

`conform` reads one case JSON per line on stdin and writes one verdict per
line on stdout (`docs/spec/conformance.md`):

```sh
uv run daisugi conformance run .opendaisugi/conformance/corpus.jsonl --client clients/go/conform
```

Its shell decomposition is now the gate's: tree-sitter-bash through
`internal/shell`, the oracle's own grammar. Until 2026-09-25 it used
mvdan.cc/sh/v3, an independent parser, and the disagreements that found
are recorded in `clients/ADJUDICATIONS.md` (G-1 to G-7, GT-1 to GT-18).
The Full profile still talks to a `z3 -in` process in SMT-LIB2 text
(`internal/verify/z3client.go`), as the spec asks of a conformance client,
so it needs `z3` on `PATH`; its tests skip that profile without it.

On the frozen corpus (11,450 cases) `conform` now matches the oracle on
all 11,450 (it was 11,447 with mvdan).

## daisugi: the Go CLI for the gate

`cmd/daisugi` is one static binary for the gate commands a user runs
every day, and the gate half of `daisugi install`. The CLI itself never
runs Python. `daisugi gate check`, the command an installed hook runs,
answers every call in process through `internal/gate`, the native gate
above: no call goes to Python. A call the gate cannot decide is denied
with exit 2 and a reason, as `daisugi-gate` denies it.

```sh
cd clients/go
scripts/build.sh    # after scripts/native.sh
./daisugi gate init --workspace "$PWD"
./daisugi install --gate --enforce --yes
./daisugi gate status
```

It carries these commands, with the flags, files and output of the
Python CLI (`opendaisugi.cli`):

| Command | What it does |
|---|---|
| `gate check` | the hook entry: the contract of `python -m opendaisugi.gate`, answered in process by `internal/gate` |
| `gate init`, `gate register` | write `envelopes/<session or default>.json` as pydantic writes it |
| `gate arm`, `gate disarm` | remove or write the `DISARMED` marker |
| `gate status` | armed state, the effective hook mode (global and project `settings.json`), else `config.yaml`'s `gate_mode`, and the envelopes |
| `gate report` | the shadow-log summary |
| `gate settings` | the hooks-settings JSON for `claude --settings` |
| `gate proposals` | the recorded envelope-edit proposals, read only |
| `install --gate [--enforce\|--shadow] [--runtime X] [--dry-run] [--yes]` | the gate hook in Claude Code's `settings.json` and Codex's `hooks.json`; a runtime that cannot be written is named on stderr and the run exits 1 |
| `install --gate --uninstall` | the gate hook and the floor-report hooks removed |
| `install --harness pi\|opencode [--uninstall] [--dry-run]` | the pi extension and the OpenCode plugin, byte for byte Python's |

Everything else is not in this binary yet. Such a command or flag prints
one line, `<command> is not in this binary yet.`, and exits 2 with nothing
changed. `daisugi --help` lists them under "Not yet in this binary".
`install --gate` writes the gate layer only: the skill, MCP, capture and
instruction layers stay with the Python CLI. `install --ask` is not in the
binary yet (`gate check --ask` itself is answered natively). Files are written atomically
(a temporary file in the same directory, renamed over the old one). A
`settings.json` that is a symlink is written through, as Python does,
and the run prints a note naming the file written. The hook names the
binary by the path it was run by, symlinks kept, so a package upgrade
behind `~/.local/bin/daisugi` does not break it.

### The hook it installs

```
DAISUGI_GATE_HOOK=opendaisugi.gate '/abs/path/daisugi' gate check --mode enforce \
  --root ~/.opendaisugi/gate --format claude --verify-timeout 10.0 || exit 2
```

The flags after `gate check` are the ones Python's hook passes. Both
CLIs find a gate hook by its words, never by a substring
(`config.gate_hook_kind` and `gate_hook_args` in Python,
`config.GateHookKind` and `GateHookArgs` in Go, one rule, one table of
cases in each). The command is split as the shell splits it (quotes,
backslashes, `;` `|` `&` runs as their own words, so `--mode
enforce||exit 2` reads enforce), each simple command is read on its own,
and these are gate hooks:

```
[NAME=val ...] [env [opts] [NAME=val ...]] [uv run [opts]] python*|py [flags] -m opendaisugi.gate[_client] ...
[NAME=val ...] .../daisugi gate check ...
sh|bash|dash|zsh|ksh [opts] -c '<one of the above>'
```

The mode is the last `--mode` word after the entry point, as argparse
reads it. So the Python CLI sees, reports and removes a hook this binary
wrote and does not add a second one, and hand-written hooks (`uv run
python -m ...`, `python3 -I -m ...`) are read too. A foreign hook such as
`opendaisugi.gateway-watch`, `echo -m ...` read as text, or a `--mode`
inside a quoted path, is not a gate hook. A command that holds
`opendaisugi.gate` as a word (or joined to `-m`) in any other form is an
unknown gate hook: `gate status` reports mode `unknown` ("unknown gate
hook") unless a hook it reads enforces, install counts it as installed,
and uninstall leaves that runtime untouched, names the hook and exits
1. `daisugi hook record` hooks are found the same way.

### Reading what Python reads

`config.yaml` is read as PyYAML's SafeLoader reads it (block and flow
collections, YAML 1.1 types, plain scalars over several lines), and
validated field by field with pydantic's lax rules, since one bad field
makes Python's `load_config` fail and `gate status` fall back to shadow.
An envelope file for `gate register` goes through the same YAML reader,
JSON included (PyYAML reads `1e-09` in JSON as a string). Anchors, tags,
block scalars and tabs are outside the reader: the command is refused
with a reason and nothing is written. The same holds for an uninstall
of a settings shape Python's reverse raises on after its backup.

### Conformance

`clients/cli_cases.py` runs the Python CLI on 234 synthetic cases, each
a scratch HOME tree, a command, its stdin and env; it records the exit
code, stdout, stderr and the tree after (`clients/fixtures/cli/`).
`clients/cli_compare.py --binary clients/go/daisugi` runs this binary on
every case in a fresh tree and compares the file set, modes, parsed JSON
and the hook commands by their flags. For `install`, whose Python text
also describes layers this binary does not write, those lines are
dropped on both sides. Each install the binary wrote is also read back
with Python's `gate status`, which must agree with the binary's.

The fixtures are published, so no path of the box that wrote them may be
in one. The generator writes the scratch HOME as `{HOME}`, the
interpreter as `{PYTHON}`, the oracle's source tree as `{SRC}` and the
interpreter's install as `{PY}`, and keeps only the exception line of a
Python traceback, whose frames name files on the box. Both the generator
and the compare then run `clients/fixture_paths.py`, which fails on any
`/mnt/`, `/Users/`, `/root/`, `/home/<name>/` other than the fake
`/home/user/`, virtualenv `lib/` or `site-packages` path under
`clients/fixtures`.

```sh
uv run --no-sync python clients/cli_cases.py            # rewrite the cases from the oracle
uv run --no-sync python clients/cli_compare.py --binary clients/go/daisugi --oracle
uv run --no-sync python clients/fixture_paths.py        # the leak check alone
```

Cold start on the reference box: `daisugi --help` and `daisugi gate
status` answer in about 3 ms at p50; the Python CLI takes about 460 ms.

## daisugi pathways: the pathway store in Go

`daisugi pathways list|show|stats|delete|export|import` run in the binary
with the flags, output, exit codes and files of the Python CLI. The store
is the oracle's SQLite file (`~/.opendaisugi/pathways.db`, or `--data-dir`),
read and written through `github.com/mattn/go-sqlite3`, which compiles the
SQLite amalgamation it bundles into the binary: the same schema text, the
same additive and dropped columns on open, and each column written with
the oracle's own encoder (pydantic's compact JSON for the envelope and the
plan, `json.dumps` for the rest). Python and the binary read each other's
files.

| Package | What it models |
|---|---|
| `internal/pathways` | `PathwayStore`: open and migrate, rows validated as `CompiledPathway`, `find`, export in all five formats, import with re-verification |
| `internal/embed/lexical` | the zero-model matcher of ADR-0019, with its own BLAKE2b |
| `internal/embed/potion` | model2vec's `StaticModel.encode` and the tokenizer it runs (added tokens, `BertNormalizer`, `BertPreTokenizer`, `WordPiece`) |
| `internal/pmodel` | now also `ActionPlan`, its step types, `PathwayParameter` and `CompiledPathway` |
| `internal/pyyaml` | now also `yaml.safe_dump`'s emitter, and a reader for text in that form |

### Matchers

`find` reads `matcher_model` from `~/.opendaisugi/config.yaml`, as the
oracle's does (not from `--data-dir`). It follows the oracle's order: an
empty store answers nothing before any backend is touched; rows stamped
with another model or version are left out, with the one-time warning when
they are 10% of the store or more; the dimension guard drops rows of
another width; and the first row with the highest cosine wins when it
reaches the backend's threshold.

- `lexical` is native and exact: Python's `str.lower()`, `[a-z0-9_]+`
  tokens, the same stopwords, an 8-byte BLAKE2b (written here, since the
  digest size is in its parameter block), the bucket and sign from it, and
  `sqrt(count)` weights. Threshold 0.25, identity `lexical-hash-v1`.
- `potion` is native. The default model, `minishlab/potion-base-8M`, is
  fetched only on first use, with one line on stderr saying so, from one
  pinned revision, each file checked against its SHA-256 in
  `internal/embed/potion/fetch.go`, into
  `$XDG_CACHE_HOME/opendaisugi/models/potion-base-8M` (`~/.cache` when
  unset). `OPENDAISUGI_POTION_MODEL` naming a local directory is read as it
  is. A model that cannot be had is no match, never a fallback to another
  matcher. The tokenizer's tables are generated from the `tokenizers`
  library itself (`gen_tables.py`), and a `tokenizer.json` with any other
  component is refused. Threshold 0.59.
- `all-MiniLM-L6-v2` (the torch backend) and `int8` (onnxruntime) are not
  in the binary. A `find` under either is refused with a line naming
  `lexical` and `potion`.

A store is read in rowid ranges on four connections at once, with
memory-mapped pages, and stored vectors are scanned sparsely: most of a
lexical vector is `0.0`. Row norms are summed in numpy's pairwise order,
so near ties break as numpy breaks them; an exact tie (the same cosine to
the last bits in real numbers) is decided by BLAS rounding, which differs
between CPUs, and any of the tied rows is accepted (PW-4).

### Proof and speed

`clients/pathway_cases.py` writes the cases from the oracle
(`clients/fixtures/pathways/`): 158 CLI cases (a tree, a database given as
rows and laid out by Python's `sqlite3`, one command, and the exit code,
stdout, stderr and tree after, each database dumped as schema and rows),
24 find cases, 577 verify cases with every violation message, unit goldens
(BLAKE2b, lexical vectors, a tiny potion model's token ids and vectors),
and the tiny model itself, so no test downloads one. `clients/pathway_compare.py` runs the binary and
`cmd/pathway-probe` (a test instrument that runs `find` on many queries in
one process; neither CLI has a `find` command) over them:

```sh
go build -o /some/scratch/pathway-probe ./cmd/pathway-probe
uv run --no-sync python clients/pathway_cases.py
uv run --no-sync python clients/pathway_compare.py --binary clients/go/daisugi \
    --probe /some/scratch/pathway-probe --oracle --fuzz 2000 --seed 7 --speed
```

With the real potion model in the Hugging Face cache, the compare also
checks the real tokenizer's ids on 3,042 texts and the real-model find
cases, copying the pinned files into a scratch cache (nothing is
downloaded). Results on the reference box, 2026-09-25:

| Run | Items | Disagreements |
|---|---|---|
| CLI cases (`--oracle`, fixture unchanged), stderr included | 130 | 0 |
| violation messages (`internal/verify`, `verify_messages.jsonl`) | 641 violations | 0 |
| find cases, lexical, tiny and real potion | 380 queries | 0 |
| real tokenizer ids | 3,042 texts | 0 |
| fuzz, seed 7: lexical, tiny potion, real potion (150-row stores) | 6,000 queries (4,554 matched) | 0 |
| fuzz, seed 11 | 900 queries (702 matched) | 0 |
| `yaml.safe_dump` emitter, `testdata/dump_oracle.py` seed 7 | 5,000 values | 0 |

Cold process, 1,000 lexical pathways (about 22 MB of vector text):

| | p50 | p90 |
|---|---|---|
| `daisugi pathways list` | 22.6 ms | 24.7 ms |
| `daisugi pathways list --json` | 25.6 ms | 27.4 ms |
| find (`pathway-probe`, one query) | 15.4 ms | 16.6 ms |
| Python `pathways list` | 965 ms | 970 ms |

`find` meets the 20 ms target; `list` does not, by about 3 ms. `list`
validates every row as pydantic does, as Python's `list_all` does: the
envelope, the plan and all 4,096 floats of each vector. Of its time, about
8 ms is SQLite reading and copying the 22 MB of vector text on four
connections, about 7 ms is validating the envelopes and plans, and about
5 ms is starting and ending the process. Skipping the vector check would
bring it near 10 ms but would print a list where Python raises on a
corrupt row, so it is not done (PW-9).

## daisugi gardener, tend, hook auto-tend, distill-repeats: the garden in Go

The rest of the pathway life cycle runs in the binary with the flags,
output, exit codes and files of the Python CLI. The binary never runs
Python.

| Command | What it does |
|---|---|
| `gardener prune\|merge\|run\|status\|watch` | evict stale or failure-dominated pathways, fold near-duplicates into the winner, both, the store's counts; `watch` is the one-shot run behind a stamp file |
| `tend` | distil successful journal traces into pathways: cluster by the active matcher, intersect, salvage divergent steps or generalize through a model, verify, store |
| `hook auto-tend` | turn captured sessions into journal traces, then tend |
| `distill-repeats` | rank repeated gateway asks into a reuse worklist |

`registry pull-and-tend` and the other `hook` commands say they are not
in this binary yet.

| Package | What it models |
|---|---|
| `internal/garden` | `gardener.prune` and `gardener.merge`, `intersect_permissions`, pydantic's `==` on dumps |
| `internal/tracejournal` | `Journal`: the YAML trace bodies and the SQLite index, its schema and migrations, `list_successful_traces`, `load_trace`, refinements, `log`, the converted-session table; networkx's topological order; the envelope cache the facade makes |
| `internal/distill` | `Distiller.tend` and `pathway_params`; the gateway worklist |
| `internal/llm` | the model call for structured output, two backends |
| `internal/capture` | `hook.list_sessions`, `infer_envelope` and the plan a capture makes |

### Model calls

`internal/llm` makes the call the oracle makes through instructor, with
the same prompt text, schema text, retry count and validation, so the
request bytes match:

- `claude-code`: `claude -p --model=haiku [DAISUGI_CLAUDE_ARGS]
  --output-format json`, the prompt on stdin, in a fresh directory under
  `$TMPDIR`, 120 s. The reply is the envelope's `result`; `is_error` or
  stdout that is not the envelope ends the call, and a reply that does not
  parse or validate is re-asked twice with the error (GD-1).
- `litellm`: the Anthropic Messages API over HTTPS (Go's own TLS), with
  `ANTHROPIC_API_KEY` (or `ANTHROPIC_AUTH_TOKEN`), `ANTHROPIC_API_BASE`
  or `ANTHROPIC_BASE_URL`, and `REQUEST_TIMEOUT`, as litellm reads them.
  The body is litellm's, byte for byte; the schema texts and the
  `max_tokens` table come from the libraries (`gen_schemas.py`). Only
  `anthropic/<model>` is carried; any other provider is refused before
  anything is written (GD-4).

The backend is chosen as the oracle chooses it: `OPENDAISUGI_LLM_BACKEND`,
then `llm_backend` in `~/.opendaisugi/config.yaml`, then a key means
litellm and a `claude` on PATH means claude-code. The key is never
printed.

### Verification

Every pathway `tend` stores is verified first by `internal/verify` with
the linked Z3 (500 ms, the oracle's default), and the distiller fails
closed: a violation, a Z3 error, or a check that answered `unknown`
drops the cluster (GD-3). A test plan that timed out counts as failing.

### Proof and speed

`clients/garden_cases.py` writes the cases from the oracle
(`clients/fixtures/garden/`) and `clients/garden_compare.py` runs the
binary over them:

```sh
uv run --no-sync python clients/garden_cases.py
uv run --no-sync python clients/garden_compare.py --binary clients/go/daisugi --fuzz 200 --seed 3 --speed
uv run --no-sync python clients/go/internal/llm/gen_schemas.py --check
```

Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| garden cases, stderr on every case | 198 (186 agree, 12 refused or not in the binary as ruled) | 0 |
| the same with `--oracle` (fixture current) | 198 | 0 stale |
| guard: Python reads what the binary wrote | 124 trees (stores and journals) | 0 |
| fuzz, seed 3: prune, merge, run, status on random stores | 200 stores | 0 |
| fuzz, seed 11 | 100 stores | 0 |
| `gen_schemas.py --check` | 2 models, 20 max_tokens entries | current |

The refused cases are the rulings' own: a capture that would be
journaled with a violation (GD-8), a trace body not in `safe_dump` form
and a MiniLM run that needs its embedder, over a current or an old
journal or store (GD-7), a model that is not
`anthropic/` (GD-4), and the commands not in the binary (GD-14).

Cold process, the binary and Python:

| | binary p50 | Python p50 |
|---|---|---|
| `gardener status`, 1,000 pathways | 28 ms | 381 ms |
| `gardener prune --dry-run`, 1,000 pathways | 26 ms | 383 ms |
| `gardener merge --dry-run`, 1,000 pathways (all pairs) | 111 ms | 5,310 ms |
| `tend`, 7 traces, one cluster salvaged and verified | 145 ms | 848 ms |

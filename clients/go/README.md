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
  | ./daisugi-gate --mode audit --root ~/.opendaisugi/gate --format claude
```

It reads what the Python gate reads: the same argv (parsed as Python
3.12's argparse parses it, prefixes and errors included), the payload on
stdin, the environment and the gate root (envelopes, the `DISARMED`
marker, `config.yaml` beside the root, asks and answers, the session
tree). It writes what the Python gate writes: stdout, stderr and the exit
code for each format, the audit log line, the session tree entries, the
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
| an `llm_check` through the claude-code backend, or under `SSL_CERT_FILE`, `SSL_CERT_DIR` or a proxy setting it does not read as httpx does (PX-5) | the port is the model client's HTTP call, not `claude -p` or httpx's certificate handling |
| an `llm_check` reply the port does not read as the model client does (a content-encoding, a nesting past 900) | the failure text is the deny reason, and is not guessed at |
| a `config.yaml` outside the YAML subset `internal/pyyaml` reads | not guessed at |
| no home directory at all | Python cannot import the gate |
| input past what a probe can check (a transcript line nested near the JSON limit, a pid past a C int) | the oracle's behavior there depends on its stack |

`llm_check` is the oracle's own model call (`llm_client.py`): one POST to
the Anthropic Messages API or an OpenAI-compatible chat completions
endpoint, with the client's headers, body and timeout, the verdict read
from the reply, and every failure worded as the client words it (LLM-8,
LLM-12; the tests use a fake server, never a model). Warning regexes, `--help` at any
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
(`clients/fixtures/gate/cases.jsonl`, 1,342 calls, the 790-command
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
| case suite (`--oracle`, fixture regenerated and unchanged; 1,342 calls since 2026-09-27) | 1,342 | 1,342 | 0 |
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
The Full profile sends SMT-LIB2 text to the Z3 the binary links, through
Z3's own command interpreter (`internal/verify/z3client.go`), so neither
`conform` nor the verifier tests need a `z3` on `PATH` (ADJUDICATIONS
VZ-1).

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
| `gate report` | the audit-log summary |
| `gate settings` | the hooks-settings JSON for `claude --settings` |
| `gate proposals` | the recorded envelope-edit proposals, read only |
| `gate serve [--root R]` | the resident gate on `<root>/gate.sock`, which pi and OpenCode ask (see below) |
| `install --gate [--enforce\|--audit] [--runtime X] [--dry-run] [--yes]` | the gate hook in Claude Code's `settings.json` and Codex's `hooks.json`; a runtime that cannot be written is named on stderr and the run exits 1 |
| `install --gate --uninstall` | the gate hook and the floor-report hooks removed |
| `install --harness pi\|opencode [--uninstall] [--dry-run]` | the pi extension and the OpenCode plugin, byte for byte Python's |
| `mcp serve`, `onboard`, `tiers setup`, `setup` | stage K3: the MCP server, the day-one onboard and the local-model setup (see below) |
| `modules`, `dashboard`, `metrics` | stage K4: the module map, the live floor and the Prometheus exporter (see below) |
| `hook record [--format F] [--event E] [--captures-root R]` | the capture and floor-report hooks `install` writes (see below) |
| `registry init\|pull\|publish\|status\|pull-and-tend`, `batch prove`, `release keygen\|sign\|verify` | stage L: the git pathway registry, batch proofs and release signing (see below) |

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
makes Python's `load_config` fail and `gate status` fall back to audit.
An envelope file for `gate register` goes through the same YAML reader,
JSON included (PyYAML reads `1e-09` in JSON as a string). Anchors, tags,
block scalars and tabs are outside the reader: the command is refused
with a reason and nothing is written. The same holds for an uninstall
of a settings shape Python's reverse raises on after its backup.

### Conformance

`clients/cli_cases.py` runs the Python CLI on 472 synthetic cases, each
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
  matcher. The download goes through the proxy `urllib` would use
  (ADJUDICATIONS PX-2). The tokenizer's tables are generated from the `tokenizers`
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
- `api`: our own model client, as `llm_client.py` makes the call: the
  Anthropic Messages API for `anthropic/<model>` and `claude*`, or
  OpenAI-compatible chat completions for `openai/<model>` and
  `ollama/<model>`, over HTTP or HTTPS (Go's own TLS), with the oracle's
  request bytes, keys, base URLs, `OPENDAISUGI_LLM_TIMEOUT` and the
  proxies httpx reads (ADJUDICATIONS LLM-1 to LLM-14).

The backend is chosen as the oracle chooses it: `OPENDAISUGI_LLM_BACKEND`,
then `llm_backend` in `~/.opendaisugi/config.yaml`, then a key means
`api` and a `claude` on PATH means claude-code. The old name `litellm` is
refused with one line that names `api` (LLM-13). The key is never
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
| garden cases after the proxy cases (PX-1 to PX-8) | 215 (203 agree, 12 refused or not in the binary as ruled) | 0 |
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

## daisugi gateway, gateway-report, router, route: the gateway in Go

The token-saving gateway runs in the binary: an HTTP proxy between a
harness and its model that routes each turn, meters it, and journals it,
with the flags, output, files and exit codes of the Python CLI. It never
runs Python.

| Command | What it does |
|---|---|
| `gateway` | the proxy: Anthropic Messages and OpenAI Chat Completions, buffered and streamed, the rules router (ADR-0015 sticky routing), `--router off`, and `--router switchyard` with Switchyard as a managed child |
| `gateway-report` | the calibration report: realized routing, the prompt cache, the reuse ceiling, the Switchyard target shares |
| `router status [--json]`, `router stop` | the router choice, the binary and each child from its state file, recent turns; stop what a gateway left running |
| `route` | the cheapest viable tier for a task, a matching pathway first |
| `install --gateway [--base-url U] [--router R ...]` | the base_url layer: Claude Code's `ANTHROPIC_BASE_URL`, Codex's model provider, OpenClaw's provider; `--router` writes config.yaml and the Switchyard TOML |

| Package | What it models |
|---|---|
| `internal/gateway` | `gateway_asgi` (the proxy), `gateway_pipeline`, `gateway` (route_turn, the meter), `gateway_journal`, `gateway_answers`, `gateway_openai`, `gateway_report`, `routing` |
| `internal/switchyard` | `router_switchyard`: the TOML it writes and a reader for the TOML a deployment file uses, the child, its state file, health and stop |
| `internal/recall` | `gateway_recall.recall` and `gateway_answers.recall_answer`, the reuse a harness calls through MCP |
| `internal/config` | now also `save_config`: the whole file restated as pydantic dumps it |

How it matches the oracle:

- A request body is read as Python's `json.loads` reads bytes (UTF-8 with
  or without a BOM, UTF-16, UTF-32, encoded surrogates kept), then routed
  with Python's duck typing: a body whose shape makes an oracle operation
  raise is forwarded untouched and not journaled, as the oracle forwards
  it. A routed body is sent as `json.dumps` writes it.
- The upstream client follows no redirect, adds no compression of its own
  (it decodes gzip and deflate as httpx does and passes anything else as
  it came), has no timeout, and is never tied to the client connection:
  a client that goes away does not stop the turn, which is read to its end
  and journaled.
- The upstream calls, and the Switchyard health probe, go through the
  proxy the oracle's client would use: httpx's rules for a turn,
  urllib's for the probe, with no bypass for loopback
  (`internal/netproxy`, ADJUDICATIONS PX-1 to PX-8). The cases hold a
  fake proxy on 127.0.0.1 that records every CONNECT and request line.
- A stream passes through as it arrives, each read flushed; the usage and
  answer text are sniffed from it on the way.
- Journal and answer lines are written byte for byte as the oracle writes
  them; sums of floats are compensated as CPython 3.12's `sum()` is.
- SIGHUP and `POST /_reload` (loopback only) rebuild the Anthropic
  wire's router from config.yaml, which is also checked for a change at
  most every two seconds; SIGTERM ends the process by the signal after it
  drains, as uvicorn does.
- The Switchyard child binds 127.0.0.1, starts in its own session, and is
  sent SIGTERM by the kernel when the gateway dies (a parent-death signal
  set on a thread held for the life of the process).

The gateway serves no answer from the answer store or from pathways, as
the oracle's does not (GW-1); `internal/recall` carries both reuse paths
for a later MCP server. The rulings are GW-1 to GW-13 in
`clients/ADJUDICATIONS.md`.

### Proof and speed

`clients/gateway_cases.py` writes the cases from the oracle
(`clients/fixtures/gateway/`): each proxy case starts the gateway on a
free loopback port in front of a fake upstream that answers from a table
keyed by the path and the exact body bytes, and records what the client
got, what the upstream got, stdout, stderr and the tree after. The fuzz
drives turn sequences through the pipeline in process on both sides
(`cmd/gateway-probe`), in chunks of at most 500 turns.

```sh
go build -o /some/scratch/gateway-probe ./cmd/gateway-probe
go build -o /some/scratch/recall-probe ./cmd/recall-probe
go build -o /some/scratch/fake-upstream ./cmd/fake-upstream
uv run --no-sync python clients/gateway_cases.py
DAISUGI_RECALL_PROBE=/some/scratch/recall-probe uv run --no-sync python clients/gateway_compare.py \
    --binary clients/go/daisugi --fuzz 2000 --seed 1 --probe /some/scratch/gateway-probe \
    --speed --upstream-bin /some/scratch/fake-upstream
```

Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| gateway cases, stderr on every case | 169 (167 agree, 2 refused as ruled: GW-8, GW-9) | 0 |
| CLI cases with the `install --gateway` and `--router` cases | 306 (294 agree, 12 not ported or refused as before) | 0 |
| pipeline fuzz, seeds 1, 2 and 3 | 6,000 turns (4,973 routed, 1,027 forwarded untouched, 3,769 journaled) | 0 |
| pipeline fuzz weighted to easy, repeated asks, seeds 4, 5 and 6 | 6,000 turns (seed 6: 667 on the cheap rung, 224 local, 184 frontier, 116 off, 149 Switchyard) | 0 |
| gateway cases with `--oracle` (fixture rerun live) | 169 | 0 stale |
| gateway cases after the 54 proxy cases (PX-1 to PX-8) | 223 (221 agree, 2 refused as ruled) | 0 |
| the 15 garden proxy cases with `--oracle` | 15 | 0 stale |

The gateway's added time on a buffered turn, 400 turns each way against
`cmd/fake-upstream`, alternating: straight to the upstream p50 0.86 ms,
through the gateway p50 1.75 ms, so 0.88 ms added at p50 (target: under
5 ms).

## daisugi status, config, start, journal: the everyday commands in Go

The last four everyday commands run in the binary with the flags, output,
exit codes and files of the Python CLI. The binary never runs Python.

| Command | What it does |
|---|---|
| `status [--data-dir D] [--threshold F] [--json]` | day-one readiness: pathway reuse, the journal, compound shell, the local model |
| `config [--json]` | every setting as daisugi uses it, with its source, and the unknown keys |
| `start [--no-ui \| --dry-run] [--enforce [--ask]] [--data-dir D]` | the gate hook scoped to this directory, a starter envelope keyed to it, the resident gate, then the plain live view |
| `journal stats [--data-dir D] [--json]` | the journal index's counts |
| `journal replay ID [--data-dir D] [--json]` | verify a stored trace again and report drift |
| `journal search QUERY [--limit N] [--data-dir D] [--json]` | rank traces by task with the lexical or potion matcher |
| `journal parse TRANSCRIPT -o OUT [--format claude-code\|codex] [--min-tools N] [--max-tools N] [--json] [--llm B]` | a Claude Code or Codex transcript as episodes |
| `journal ingest FILE [--dry-run] [--allow-shell-decomposition] [--data-dir D] [--json]` | infer each episode's envelope, verify, journal |

`config` takes no subcommand: `daisugi config show` is a usage error in
both CLIs.

What each does not do, and why (rulings C-10 to C-17 in
`clients/ADJUDICATIONS.md`):

- `start` opens the plain live view after its steps, as the oracle does
  without the tui extra (K4-5); the Textual view is not in this binary.
  Python's `start` also launches one
  program, `python -m opendaisugi.cli gate serve --root <root>`, detached,
  and waits for its socket; the binary launches its own `gate serve`
  the same way, with Python's step text (G2-4). `start --enforce --ask`
  writes a hook with `--ask`, which `gate check` answers natively.
- `status` reports pathway reuse as off under a matcher the binary does
  not carry (MiniLM, the default, or int8), where Python reports the
  `[search]` extra installed (C-12). It reads the GPU from `nvidia-smi`
  only, never torch (C-13).
- `journal search` carries the lexical and potion matchers; MiniLM and
  int8 are refused before anything is written (C-15).
- `journal parse` asks for a split of a large episode through either
  backend, as the oracle asks (C-16).
- `journal ingest` and `journal replay --json` store or print a
  verification result whole. The binary words the detail of permissions
  and DAG violations; an episode or replay that would carry another
  stage's violation is refused (C-14).

### Proof and speed

The cases are in `clients/cli_cases.py` beside the gate commands', and
run through the garden runner (`clients/garden_cases.py`): journals and
stores laid out by the oracle's own classes, a fake `claude` answering
recorded replies, every database dumped whole. `clients/cli_compare.py`
compares them as the garden compare does (stderr on every case) and
checks that Python reads every journal the binary wrote. A case may hold
`go_expect`, the fields a ruling changes, derived from the oracle's
answer by the generator; a `live` case (one that reads this box's memory
size) is answered by the oracle again on each compare.

```sh
uv run --no-sync python clients/cli_cases.py --keep     # new cases only; the rest kept
uv run --no-sync python clients/cli_compare.py --binary clients/go/daisugi --oracle
```

Results on the reference box, 2026-09-26:

| Run | Items | Disagreements |
|---|---|---|
| CLI cases, all (the 167 new ones: status 35, config 24, start 24, journal 84) | 472 (451 agree, 12 not ported as marked, 9 refused as ruled) | 0 |
| the same with `--oracle` (the fixture rerun live) | 471 | 0 stale |
| guard: Python reads every journal the binary wrote | every rich case that wrote one | 0 |
| gate, pathway, garden and gateway cases, rerun on the new binary | 1,310, 158 + finds, 200, 169 | 0 |

Cold process, 100 runs each, on a box whose bare `daisugi --version`
took 11.6 ms at p50 that hour:

| | p50 | p90 |
|---|---|---|
| `daisugi config` | 12.7 ms | 14.0 ms |
| `daisugi config --json` | 12.2 ms | 13.4 ms |
| `daisugi status --json` | 14.7 ms | 15.6 ms |
| `daisugi status` (no `nvidia-smi` on PATH) | 14.5 ms | 15.5 ms |
| `daisugi status` (a stub `nvidia-smi`) | 21.3 ms | 23.0 ms |
| `daisugi journal stats` | 14.1 ms | 14.9 ms |

The box ran slower that hour than for the earlier stages' numbers (a
bare `--version` took 11.6 ms, where it took about 3 ms before), so the
numbers are not comparable with those. `status --json`, `status` with no
`nvidia-smi` and `config` are under 20 ms at p50. Text `status` runs
`nvidia-smi` as the oracle does and misses 20 ms with it: 21.3 ms with a
stub, about 90 ms with this box's real `nvidia-smi`, whose driver is not
loaded. The Python `status` takes about 5 s here (it imports torch).

## daisugi gate serve: the resident gate in Go

The pi extension and the OpenCode plugin ask the gate over a unix socket,
`~/.opendaisugi/gate/gate.sock` (or `OPENDAISUGI_GATE_SOCK`), and deny
every call when nothing answers. `daisugi gate serve [--root R]` answers
there as `gate_server.py` does, so a Go-only install gates them too.
`daisugi start` starts it; it runs in the foreground until Ctrl-C.

```sh
./daisugi gate serve --root ~/.opendaisugi/gate
```

The wire is one JSON line per connection, `{"v": 1, "argv": [...],
"stdin_b64": "...", "coppice_sock"?, "coppice_pane"?, "herdr_pane"?,
"coppice_data_dir"?}`, and one reply line, `{"v": 1, "stdout": "...",
"stderr": "...", "exit_code": N}`, byte for byte the oracle's. argv is
the gate's own flags (`--format pi --root R --verify-timeout 4` for pi),
or `["hook", "report", ...]` for a state report. The rules (G2-1 to G2-8
in `clients/ADJUDICATIONS.md`):

- Each call is decided in a child of the binary, as `gate check`
  decides one, fed the request on stdin. A crash or a hang of the child
  is the format's deny, never a reply that allows, and never ends the
  server. At most 32 calls are decided at once.
- The payload's session id never selects an envelope; only `--session`
  does (SEC-3). The server drops its own pane variables at start. A
  call's report goes to the caller the request names, only after coppice
  places the connection as that pane (the hello carries the client's
  SO_PEERCRED pid), and the caller's `coppice_data_dir` is guarded beside
  the server's own.
- A request that cannot be read is `DENIED — bad request`, exit 2.
  `--help` in argv gets no reply. A request is UTF-8 (G2-2).
- The root is made 0700, the socket 0600 from the moment it exists. A
  stale socket or file at the path is removed; a second server takes the
  path over. Ctrl-C prints a newline, exits 0 and leaves the socket file,
  as Python does.

### Proof and speed

`clients/gate_server_cases.py` drives the Python server with the pi,
OpenCode and gate_client request shapes, hook reports, malformed and
oversized lines, and scenarios (concurrent, slow and departing clients,
start, stop, stale files, a second server), and records the replies and
every log line, tree entry, coppice line and herdr call
(`clients/fixtures/gate_server/`). `clients/gate_server_compare.py`
runs the binary's server on them.

```sh
uv run --no-sync python clients/gate_server_cases.py      # rewrite the cases from the oracle
uv run --no-sync python clients/gate_server_compare.py --binary clients/go/daisugi --oracle
```

Results on the reference box, 2026-09-27: 198 cases (190 requests, 8
scenarios), 198 agree, 0 stale against the live oracle. The gate, CLI,
pathway, garden and gateway cases rerun on the same binary: 0
disagreements (1,342 gate calls native; CLI 452 agree, 12 not ported, 9
refused).

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

## daisugi generate-envelope and internal/envgen: model-written envelopes in Go

`daisugi generate-envelope TASK` asks a model for a safety envelope, as
the Python command does: the same flags (`--model`, `--stakes`,
`--low-stakes-envelope`, `--thinking-budget`, `--llm`, `--json`,
`--allow-shell-decomposition`), the same system prompt and request bytes,
the same YAML or JSON out, and the same exit codes. The envelope must pass
the self-consistency check; a model that fails is one line, exit 2.

The rest of stage K1 is a library, `internal/envgen`, since the oracle
reaches it only from the orchestrator and the MCP server (stages K2, K3):

| File | What it models |
|---|---|
| `generate.go` | `generate_envelope`: task checks, stakes, the Tier-0 pathway lookup (with `increment_hit`), the Tier-1 slot, the envelope cache with its refinement bust, the model ladder with refinement hints and thinking kwargs, inheritance |
| `cache.go` | `EnvelopeCache` and `make_cache_key`, the oracle's SQLite file |
| `tier1.go` | `HTTPTier1Provider` (its 30 s deadline covers the whole call) and `load_configured_tier1` |
| `inherit.go` | `verify_inheritance`, worded as the oracle words it |
| `bind.go` | `bind_parameters` and `apply_bindings`: holes filled by one model call, a capability head never changed, the result verified |
| `compose.go` | `compose`: frozen pathways as skill handlers and contract envelopes |
| `prompts_gen.go` | the prompt texts, written from the oracle by `gen_prompts.py` (`--check` says when they are stale) |

`internal/recall` now binds a typed pathway's holes through
`envgen.Bind` before it verifies the plan against the caller's envelope
(K1-7). An envelope whose Z3 check did not finish is refused, as the
oracle now refuses it (K1-2, retired); a bound plan whose verify did not
finish gives the template, where the oracle's lenient verify keeps it
(K1-3). The rulings are K1-1 to K1-7
in `clients/ADJUDICATIONS.md`.

### Proof

`clients/k1_cases.py` writes 186 cases from the oracle
(`clients/fixtures/k1/`): `generate-envelope` through the CLI, and the
library through `cmd/envelope-probe`, one JSON query per case, against a
script that calls the oracle's functions. Model calls go to the garden
cases' fake `claude` and fake server, which answer only the exact request
bytes.

```sh
go build -o /some/scratch/envelope-probe ./cmd/envelope-probe
uv run --no-sync python clients/k1_cases.py
uv run --no-sync python clients/k1_compare.py --binary clients/go/daisugi \
    --probe /some/scratch/envelope-probe
```

Results on the reference box, 2026-09-27: 186 cases, 183 agree, 0
disagreements. Three are refused as ruled, the tree unchanged: a proxy
setting the binary does not read the httpx way (PX-5), a MiniLM matcher
(not carried, PW-1) and a Tier-1 config whose model is not a string
(K1-6). Python read back every pathway store and envelope cache the
binary wrote (24 cases). Go tests make the Z3 checks answer `unknown` and
see the envelope refused, the bound plan dropped for its template, and
the plan not served (K1-2, K1-3).
The other compares rerun on the same binary: gate 1,348 calls, all
native, 0 disagreements; CLI 0 disagreements; pathway 158 cases, 380 find
queries; garden 227 cases; gateway 223 (221 agree, 2 refused as ruled);
resident gate 200 agree. `go test -p 1 ./...` passes.

## daisugi run and daisugi orchestrate: the supervisor and the orchestrator in Go

`daisugi run PLAN -e ENVELOPE` runs a plan under the supervisor, as the
Python command does: the same flags (`--data-dir`, `--dry-run`, `--yes`,
`--json`), the same output, journal rows, receipts and exit codes (0
succeeded, 1 failed, 2 rejected, 130 aborted). `daisugi orchestrate PROMPT`
runs a prompt end to end: the model's plan, sized, run under the supervisor,
and its outputs put together (`--envelope`, `--budget`, `--strict-budget`,
`--deterministic-synthesis`, `--model`, `--llm`, `--stakes`, `--cost`,
`--data-dir`, `--json`). With no `--envelope`, one is generated first
through `internal/envgen`.

| Package | What it models |
|---|---|
| `internal/supervise` | `Supervisor.run`: the whole-plan verify first, `verify_step` per step, the fallback (halt, or the recompute model call), the approval stack (the allowlist, `DAISUGI_APPROVE`, a terminal prompt, deny), the shell, file_read, file_write (with its reversal handle), network and dry-run executors, stage 2, receipts with `compute_evidence_hash`, the integrity check, `log_run` |
| `internal/orchestrate` | `decompose` (the DAG check and the verify), `model_sizer`, `BudgetTracker`, the budget-aware task executor on both backends (the HTTP reply's token count, or Claude Code's own meter), the skill and MCP executors with nothing registered, `synthesize` |
| `internal/verify/step.go` | `verify_step` and `stage2.verify_completed_step` |
| `internal/tracejournal/runs.go` | `log_run`, `append_receipt`, `receipts_for_run`, `write_refinement` |

A Z3 check that does not finish fails closed wherever K2 verifies (K2-2).
`--max-parallel` above 1 (K2-4), YAML the binary does not read (K2-3), an
envelope postcondition it cannot decide (K2-8) and a stale-embeddings warning
(K2-5) are refused with exit 2 before anything is written. No CLI path runs an
agentic step (K2-7). The rulings are K2-1 to K2-9 in
`clients/ADJUDICATIONS.md`.

### Proof

`clients/k2_cases.py` writes 116 cases from the oracle
(`clients/fixtures/k2/`). Each runs the command in a scratch HOME; every step
that touches the world touches only that HOME, and model calls go to the garden
cases' fake `claude` and fake server, which answer only the exact request
bytes. A run's id, a step's duration and a receipt's hash are made stable; the
hash is checked against its own row's evidence.

```sh
uv run --no-sync python clients/k2_cases.py
uv run --no-sync python clients/k2_compare.py --binary clients/go/daisugi [--oracle]
```

Results on the reference box, 2026-09-27: 116 cases, 109 agree, 0
disagreements; 7 refused as ruled (K2-3, K2-4, K2-5, K2-8), the tree unchanged. Python read
back every journal and pathway store the binary wrote (99 cases), and `--oracle`
found no stale fixture. Go tests reach what no case can: the per-step
rejection and the recompute fallback (K2-1), and a Z3 check that answers
`unknown` before `run`, in the decomposition, before `orchestrate`'s run, per
step and on a recomputed step (K2-2). The other compares rerun on the same binary: gate 1,348 calls, all
native; CLI 498 cases (478 agree, 11 not ported, 9 refused); pathway 158 cases
and 380 finds; garden 227 cases, run in parts; gateway 223 (221 agree, 2 refused
as ruled); resident gate 200 agree; K1 186 (183 agree, 3 refused as ruled). 0
disagreements in each. `go test -p 1 ./...` passes.

## daisugi mcp serve, onboard and tiers setup: stage K3 in Go

`daisugi mcp serve [--data-dir PATH] [--model TEXT]` is the MCP server the
oracle serves over stdio, with its 11 tools (`envelope_for`, `find_pathway`,
`recall`, `recall_answer`, `verify_plan`, `verify_completed_step`,
`list_pathways`, `pathway_stats`, `run_plan`, `receipts_for_run`,
`recent_runs`). It takes no MCP SDK (DS-6): `internal/mcpwire` is a JSON-RPC
2.0 server of its own that answers as the oracle's FastMCP does, byte for
byte on the wire (K3-1). `daisugi onboard` finds Claude Code and Codex
transcripts, parses them (with the split cache), journals each episode as
verified, and runs tend. `daisugi tiers setup` probes the box, recommends a
model size, and with `--endpoint` qualifies a local model and with `--wire`
records it as Tier-1. `daisugi setup` names its replacement.

| Package or file | What it models |
|---|---|
| `internal/mcpwire` | the stdio transport's line reader, which messages the SDK takes, its -32602, -32601 and error notification, the tools/list and initialize results (`gen_tools.py` writes them from the oracle), each tool's argument model and pre-parse, pydantic's JSON writers |
| `internal/cli/mcpcmd.go`, `mcptools.go` | the server loop and the 11 tools on `internal/envgen`, `internal/pathways`, `internal/recall`, `internal/verify`, `internal/supervise` and the journal |
| `internal/cli/onboardcmd.go` | `discover_transcripts`, the split cache, `ingest_episodes` with its dry-run preview, the report |
| `internal/cli/setupcmd.go` | `detect_hardware`, `recommend_model`, `qualify_local_model`, `write_tier1_config` |

A request the server cannot answer the oracle's way gets JSON-RPC error
-32000 "... is not in this binary yet." and the server goes on (K3-3). A Z3
check that does not finish fails closed in the MCP tools (K3-5). Onboard
reads, parses and verifies everything before its first write (K3-11).
`tiers setup --remote` and `tiers stats` are not in this binary yet. The
rulings are K3-1 to K3-12 in `clients/ADJUDICATIONS.md`.

### Proof

`clients/k3_cases.py` writes 207 cases from the oracle
(`clients/fixtures/k3/`): 136 MCP sessions, 31 setup runs and 40 onboard
runs. An MCP case sends its lines one at a time and waits for each reply
(K3-2). Every case puts a fake `nvidia-smi` first on PATH.

```sh
uv run --no-sync python clients/go/internal/mcpwire/gen_tools.py --check
uv run --no-sync python clients/k3_cases.py
uv run --no-sync python clients/k3_compare.py --binary clients/go/daisugi [--oracle] [--only NAME] [--skip NAME]
```

Results on the reference box, 2026-09-28: 203 cases, 200 agree, 0
disagreements; 3 not ported (K3-9, K3-10). `--oracle` found no stale
fixture. Go tests reach a Z3 unknown in `verify_plan`, a refused MCP method,
and the CPU-only recommendation. The other compares rerun on the same
binary: CLI 507 cases (487 agree, 11 not ported, 9 refused); K2 116 (109
agree, 7 not ported); K1 186 (183 agree, 3 as ruled); pathway 158 cases and
380 finds; garden 227 cases in two parts (216 agree, 11 not ported); gateway
223 (221 agree, 2 refused as ruled); resident gate 200 agree; gate 1,348
calls, all native; conform 11,450 of 11,450. 0 disagreements in each. `go test -p 1 ./...`
passes, and `scripts/check-paths.sh` finds no build-machine path.

The oracle's onboard now checks the matcher in effect, not the
`sentence_transformers` package (K3-10). Four cases hold it: a matcher key
that nothing builds refuses a real run, adds a note to a dry run, and with
`--allow-no-embedder` goes on to tend, which raises. The Go binary answers
them as the oracle does. After the change: 207 cases, 204 agree, the same
3 not ported, 0 disagreements; `go test -p 1 ./internal/cli` passes.
`--oracle` over the 40 onboard cases found no stale fixture.

## daisugi modules, dashboard and metrics: stage K4 in Go

`daisugi modules [--data-dir D] [--json]` draws the module map: for each
stage of the pipeline, which module is active, which is available to swap
in, and which is only a designed slot. `daisugi dashboard [--data-dir D]
[--once] [--json] [--interval S]` adds a gauge per stage read from the
local stores (the journal, the pathway store and the gateway's turn
journal): one frame when stdout is not a terminal or with `--once`, the
map with its gauges as JSON with `--json`, and on a terminal a new frame
every interval until Ctrl-C. `daisugi metrics [--data-dir D] [--serve
[--host H] [--port P]]` prints the same numbers as Prometheus text
(0.0.4), or serves them at `/metrics`. `daisugi start` opens the plain
live view after its steps (K4-5).

| File | What it models |
|---|---|
| `internal/cli/modulescmd.go` | `modules.detect_stages`, `render_wiring`, `wiring_json`: the hooks in `~/.claude/settings.json`, the harness homes, the programs on PATH, the coppice socket, the backend and a recorded model host |
| `internal/cli/dashboardcmd.go` | `dashboard.read_raw`, `collect_metrics`, `render_dashboard`, `dashboard_json`, `run_live` |
| `internal/cli/metricscmd.go` | `exporter.render_prometheus` and the `/metrics` handler |

The map answers as the oracle installed with the extras the binary carries
(potion and the bash grammar), from a wheel, not a checkout (K4-1). Under
MiniLM or int8 it does not fall back to lexical, so the lexical rows are
available, not active (K4-2). `dashboard --tui` and `--serve`, the Textual
views, are not in this binary yet (K4-3). A settings file whose hooks are
in a shape the oracle raises on, and a gateway record of other types, are
refused before any write (K4-4). The endpoint answers HTTP/1.1 with the
oracle's status, Content-Type and body (K4-6). The map, the floor and
`status` read the journal read-only and make nothing on a fresh box, as
the oracle now does (K4-8, retired); `status` keeps the pathway count
when the hit sum is inf (K4-9, retired). The rulings are K4-1 to K4-9 in
`clients/ADJUDICATIONS.md`.

### Proof

`clients/k4_cases.py` writes 110 cases from the oracle
(`clients/fixtures/k4/`): 64 module maps, 30 dashboards and 16 metrics
runs, 3 of them served on a loopback port and scraped. PATH holds only
each case's fakes (K4-7). A Go test drives the terminal view over a pty.

```sh
uv run --no-sync python clients/k4_cases.py
uv run --no-sync python clients/k4_compare.py --binary clients/go/daisugi [--oracle] [--only NAME] [--skip NAME]
```

Results on the reference box, 2026-09-28: 110 cases, 105 agree, 0
disagreements; 2 not ported (K4-3) and 3 refused (K4-4). A fresh rerun of
the oracle changed no fixture but the four the harness fix meant to. The
other compares rerun on the same binary: CLI 508 cases in three parts (490
agree, 9 not ported, 9 refused; the 3 `start view` cases agree); K3 207
(204 agree, 3 not ported); K2 116 (109 agree, 7 not ported); K1 186 (183
agree, 3 as ruled); pathway 158 cases and 380 finds; garden 227 (216 agree,
11 not ported); gateway 223 (221 agree, 2 refused as ruled); resident gate
200 agree; gate 1,348 calls, all native. 0 disagreements in each. `go test
-p 1 ./...` passes, and `scripts/check-paths.sh` finds no build-machine
path.

After the journal read-only fix and `hook record`, 2026-09-28: K4 193
cases (188 agree, 2 not ported, 3 refused, 0 disagreements), CLI 508 (490
agree, 9 not ported, 9 refused), garden 227 (216 agree, 11 not ported), 0
disagreements in each; `go test -p 1 ./...` passes.

## daisugi hook record: the capture and floor-report hooks

`daisugi hook record [--format claude|codex|hermes|openclaw] [--event
pre_tool_use|stop|notification|subagent_start|subagent_stop]
[--captures-root R]` reads one hook payload on stdin and prints the
host's continue contract. The default event records the tool call in
`<R>/<session>.jsonl` and, with the user's consent, starts a background
`hook auto-tend` at most once per 30 minutes. `stop` and `notification`
report idle or blocked to coppice (`COPPICE_SOCK`, `COPPICE_PANE`) or
herdr, and append the state to the session tree beside the captures;
`subagent_start` and `subagent_stop` report a child row to coppice.

| File | What it models |
|---|---|
| `internal/gate/hookrecord.go` | `hook.record_and_contract`, `hook.record_lifecycle_event`, `_state_report.report_child` |
| `internal/cli/hookcmd.go` | the command's options and `hook.maybe_trigger_background_tend` |

Past the options it always prints the contract and exits 0, so a Stop
hook never blocks the stop: a write it cannot finish or an input it does
not model is skipped (HR-1). A config.yaml the binary cannot read counts
as no consent to the background tend (HR-2). `hook list` and `hook
to-trace` are stage F (below); the command-line `hook report` stays
refused; no hook runs it (HR-4).
The 76 hook cases are in the K4 fixtures (HR-3): all agree.

## Stage L in Go: the registry, batch proofs, release signing

`daisugi registry init URL [--clone-to D]` clones a team's registry: a git
repository whose `pathways/` holds signed pathway bundles. `registry pull
[--repo-path D] [--require-signed|--allow-unsigned]` runs `git pull
--ff-only` and puts each bundle signed by a trusted key into the clone's
cache (`<clone>/.cache/pathways.db`); the trusted keys are in
`<clone>/.cache/trusted-signers.json`, never the file the remote sends.
`registry publish ID --private-key K --public-key P [--publisher N]
[--push|--no-push] [--data-dir D]` signs a local pathway as a bundle,
writes `pathways/<hash>.yaml`, commits and pushes. `registry status
[--json]` and `registry pull-and-tend [--data-dir D]` (a pull, then
`tend`) complete the group. `daisugi batch prove DECL -e ENV` proves a
declared batch before any item runs: every write each item resolves to
lies inside the envelope and the declared footprint, and none would be
irreversible. `daisugi release keygen|sign|verify` makes an ed25519
release key, signs a SHA-256 manifest over the artifacts, and verifies a
manifest against the trusted signers in `~/.opendaisugi/trusted_signers.json`.

| Package | What it models |
|---|---|
| `internal/signing` | `signing.py`: base64 read as CPython's `b64decode`, `sign_bytes`, `verify_bytes`, the trusted-signer registry, `canonicalize_contract` and contract signatures |
| `internal/bundle` | `pathway_bundle.py`: the canonical payload, `compute_bundle_hash`, `pathway_to_bundle`, `bundle_to_pathway` |
| `internal/registry` | `git_pathway_store.py`: open (and materialize), `pull`, `publish`, `status`; git as a child |
| `internal/batch` | `batch.py`: classify, resolve, `prove_footprint`, `would_be_reversible`, the two-ledger meter, `run_batch`, `rollback_result` |
| `internal/deeds` | `deeds.py`: `apply_reversal`, `rollback_run`, `touched_files` |
| `internal/strata` | `strata.py`: the strata store, `reconstruct_context`, `promote_constraint` |
| `internal/cli/registrycmd.go`, `batchcmd.go`, `releasecmd.go` | the commands |
| `cmd/l-probe` | a test instrument (not shipped): one query of the library parts no command reaches (L-1) |

Keys are the oracle's: the raw 32-byte seed and the raw public key in
base64 (L-4). An exception the oracle raises ends a command with exit 1
and its type on stderr (L-5). git runs with the caller's environment less
every `GIT_*` variable, and never looks for a repository above the clone
(L-15). `release sign` refuses two artifacts with one file name (L-9).
The rulings are L-1 to L-15 in `clients/ADJUDICATIONS.md`.

### Proof

`clients/l_cases.py` writes 239 cases from the oracle (`clients/fixtures/l/`):
144 commands and 95 library probes (`clients/l_probe_cases.py`, answered by
`clients/l_probe_oracle.py`). Git remotes are bare repositories in the
scratch HOME, run with a scrubbed git environment and fixed dates; keys
come from fixed seeds. Values made from the clock or a random key are
renamed, and the compare checks each with the oracle's own code (L-2).

```sh
uv run --no-sync python clients/l_cases.py
uv run --no-sync python clients/l_compare.py --binary clients/go/daisugi --probe PATH/l-probe [--oracle] [--only NAME]
```

Results on the reference box, 2026-09-28: 232 cases, 232 agree, 0 refused,
0 disagreements. After L-9 and L-15 (239 cases), 239 agree. The other compares on the same binary: CLI 508 (490
agree, 9 not ported, 9 refused), garden 227 (217 agree, 10 not ported),
K1 186 (183 agree, 3 as ruled), K2 116 (109 agree, 7 not ported), K3 207
(204 agree, 3 not ported), K4 193 (188 agree, 2 not ported, 3 refused),
pathway 158 cases and 245 finds, gateway 223 (221 agree, 2 refused),
resident gate 200 agree, gate 1,348 calls, all native: 0 disagreements in
each. `go test -p 1 ./...` passes, and `scripts/check-paths.sh` finds no
build-machine path.

## Stage F in Go: the transcript parsers, ingest and the capture commands

`daisugi journal parse T -o F [--format claude-code|codex]` reads a Claude
Code session or a Codex rollout into episodes, and splits an episode over
`--max-tools` with one model call; `daisugi journal ingest F` journals
each episode against the envelope inferred from its own steps (ADR-0016);
`daisugi onboard` runs both over every transcript it finds. These were in
the binary before (C-14, C-16, K3). Stage F adds `daisugi hook list
[--captures-root R] [--json]`, the captured sessions newest first, and
`daisugi hook to-trace ID [--captures-root R] [--data-dir D] [--task T]
[--allow-shell-decomposition]`, one captured session made a journal trace.

| File | What it models |
|---|---|
| `internal/pystr/stream.go` | `open(path, encoding="utf-8")` read line by line: 8192-byte decode calls and the `UnicodeDecodeError` they raise (F-2) |
| `internal/transcript` | `parsers/claude_code.py` and `parsers/codex.py`: every exception the parse raises, worded as Python words it (F-3) |
| `internal/capture/capture.go`, `sort.go` | `hook.list_sessions` and the records `captures_to_trace` reads; `list.sort(reverse=True)` (F-6) |
| `internal/cli/hookcapcmd.go` | `hook list` and `hook to-trace` (F-7) |
| `internal/cli/journalparse.go` | `journalResult`: a verify result as a trace stores it, shared by ingest, auto-tend and to-trace |

An exception the parse raises is Python's text: `journal parse` prints
`Parse error: <text>` after the backend note and exits 2, `onboard` warns
and goes on (F-3). A line nested past 900 levels is refused (F-4). The
capture commands name an exception the oracle does not catch and exit 1;
`hook to-trace` makes the journal first, as Python does (F-7). A trace
whose verify fails is journaled with its violations worded; `auto-tend`
now does the same (GD-8 retired for Go). The oracle has no parser for pi,
OpenCode, Hermes or OpenClaw transcripts, so neither has the binary (F-1).
The rulings are F-1 to F-10 in `clients/ADJUDICATIONS.md`.

### Proof

`clients/f_cases.py` writes 199 cases from the oracle (`clients/fixtures/f/`):
Claude Code and Codex transcripts in every row shape the parsers read,
malformed, huge and deep lines, bytes that are not UTF-8 at the chunk
edges, the split through the fake model on both backends, the same
parsers under `onboard`, ingest of what they wrote, and the two capture
commands. A file over 32 KiB is compared by its size and SHA-256 (F-9).

```sh
uv run --no-sync python clients/f_cases.py
uv run --no-sync python clients/f_compare.py --binary clients/go/daisugi [--oracle] [--only NAME] [--max-refused 1]
```

Results on the reference box, 2026-09-28: 199 cases, 198 agree, 1 refused
(F-4), 0 disagreements. The other compares on the same binary: CLI 508
(490 agree, 9 not ported, 9 refused), garden 226 (219 agree, 7 not
ported; the two auto-tend cases GD-8 refused now agree), K1 186 (183
agree, 3 as ruled), K2 116 (109 agree, 7 not ported), K3 207 (204 agree, 3
not ported), K4 193 (188 agree, 2 not ported, 3 refused), L 239 agree,
pathway 158 cases and 245 finds, gateway 223 (221 agree, 2 refused),
resident gate 200 agree, gate 1,348 calls, all native: 0 disagreements in
each. `go test -p 1 ./...` passes, and `scripts/check-paths.sh` finds no
build-machine path.

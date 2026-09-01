# The Conformance Protocol — multi-client verification

**Status:** v1, shipped with the conformance module (`opendaisugi.conformance`).
**Audience:** anyone implementing an independent verifier client (Rust, Go,
Lean4, TypeScript, …) and anyone extending the Python oracle.

## Why multiple clients

The verifier is consensus-critical: every harness action passes through it.
A single implementation's test suite only tests what its author thought to
test. Independent reimplementations working from this spec are checked
*differentially*: every disagreement between clients on the same case is a bug
in one implementation or an ambiguity in the spec — both are findings that no
amount of staring at one codebase produces. (The model is Ethereum's
client-diversity discipline applied to the verification core, not to the
periphery: nobody needs five distillers.)

## The corpus

A corpus is a JSONL file: one self-contained **case** per line, sorted by id,
pinned by a sibling `*.manifest.json` (`{"v", "count", "sha256"}` of the exact
file bytes). Cases are content-addressed: `id` = first 16 hex chars of the
SHA-256 of the canonical JSON of the case body *without* the `id` key.
Canonical JSON = sorted keys, `,`/`:` separators, raw (non-escaped) unicode.

Two case kinds:

```json
{"kind": "verify", "v": 1, "id": "…",
 "plan": {…ActionPlan…}, "envelope": {…Envelope…},
 "options": {"strict": null, "z3_timeout_ms": 500},
 "expect": {"ok": false, "violations": [{"stage": "permissions", "step": "s1"}]}}

{"kind": "decompose", "v": 1, "id": "…", "command": "grep -c x f | sort > out",
 "expect": {"ok": true, "heads": ["grep", "sort"], "commands": ["…", "…"],
            "reads": [], "writes": ["out"]}}
```

`plan` and `envelope` are the full pydantic JSON dumps with one normalization:
the top-level `plan.id` and `envelope.id` (random-uuid bookkeeping) are pinned
to `"plan_case"` / `"env_case"` so identical logical cases deduplicate. Step
ids are semantic (violations cite them) and are preserved.

### Normative vs. informative

Clients are compared **structurally, never textually**. Two correct clients
may word a rejection differently; they may not disagree about what was
rejected.

- `verify` cases — normative: `ok`, and the *multiset* of
  `(stage, step)` pairs (`step` may be null for plan-level violations).
  When `expect` holds `word_audit` (the dialect audit warnings, in order;
  DL-7), it is normative too, compared exactly, text included; a case
  without the key does not compare it. Informative: messages,
  `suggested_remediation`, `detail` beyond `step`, the other warnings,
  durations. `options` may carry `dialect_pin`, verify's enforcement pin,
  and `dialect_base`, the directory a word is placed from (DL-13).
- `decompose` cases — normative: `ok`, `heads` **in command order**, `reads`
  and `writes` as sorted sets. Informative: `commands` (source-text
  reconstruction is parser-dependent) and rejection `reason` strings.

## The wire protocol

A client is any executable. It reads one case JSON per line on stdin and
writes one verdict JSON per line on stdout (order-independent; verdicts are
matched by `id`), flushing per line:

```json
{"id": "…", "ok": true, "violations": [], "word_audit": []}    // verify kind
{"id": "…", "ok": true, "heads": […], "commands": […],
 "reads": […], "writes": […]}                                   // decompose kind
{"id": "…", "error": "<what went wrong>"}                       // per-case failure
```

A malformed or unprocessable case must yield an `error` verdict — which the
runner counts as a mismatch — and must never abort the stream. Exit non-zero
only for process-level failure.

## Profiles

- **Core** — everything structural: permissions (shell allowlist + interpreter
  policy + decomposition + file/network scopes), DAG checks, delegation
  safety. No SMT solver required.
- **Full** — Core plus the predicate-algebra stages (envelope
  self-consistency, plan-vs-envelope, skill-delegation subsumption). Clients
  implement these by **emitting SMT-LIB2 text** and handing it to a solver
  that reads it: a solver binary (`z3 -in`, or a compatible one) or the
  SMT-LIB2 command interpreter of a linked Z3 (`Z3_eval_smtlib2_string`).
  The text is the interface, so it stays the same across languages and the
  solver stays swappable. The Go and Rust clients link Z3, so they need no
  solver on `PATH` (`clients/ADJUDICATIONS.md` VZ-1).

Slate clients (Rust, Go, Lean4, TypeScript) target Full. Embedded ports (e.g.
an on-device Dart build) may ship Core and must say so.

## Generating and growing the corpus

Recording is a product feature, not test scaffolding: any process with
`OPENDAISUGI_CONFORMANCE_RECORD=<dir>` set appends its real `verify()` calls
and shell decompositions as raw case lines (`cases-<pid>.jsonl`). Recording is
skipped for non-portable inputs — calls carrying an `AliasRegistry` and plans
containing runtime-registered custom step types are process state, so their
cases would not be self-contained — and recording failures are swallowed: a
broken disk must not break verification.

```sh
OPENDAISUGI_CONFORMANCE_RECORD=raw/ pytest -q      # harvest the suite
daisugi conformance export raw/ --out corpus.jsonl # dedupe + manifest
daisugi conformance run corpus.jsonl --client "python -m opendaisugi.conformance"
daisugi conformance bench corpus.jsonl [--client …] [--repeat N]
```

The corpus is a *generated, environment-specific artifact* (recorded cases
embed real paths), pinned by its manifest per generation — it is not committed.
The reference generation on the development box: the full test suite plus
~12.9k unique real transcript commands → **13,274 cases** (306 verify,
12,968 decompose), oracle self-check 13,274/13,274.

## Baseline numbers (Python oracle, reference box)

- In-process: p50 0.132 ms, p95 0.421 ms, p99 3.3 ms, ~4,500 cases/s.
- Over the pipe (IPC included): p50 0.183 ms, ~3,500 cases/s.

Per-verification cost is sub-millisecond already; the compiled clients' wins
are process startup (the hook's ~0.5 s round trip is Python interpreter + import
cost, not verification), embedding (C ABI / wasm), and the long tail.

## Dialect cases

`clients/fixtures/dialect/`, written by `clients/dialect_cases.py` from the
oracle, holds the kernel's `forall_writes` and the system dialect's words
(DL-1 to DL-17). `cases.jsonl` is ordinary corpus cases: 2,319 of kind
`verify`, each with `expect.word_audit` present (empty included), among
them cases placed from a base (`options.dialect_base`), and 3 of kind
`decompose`, so `clients/compare.py` runs any conform client over it.
`globs.json` is the glob translation: `dialect.glob_regex` for 400 globs,
with the error text of each refused one and the paths each other one
matches among 99 normalized paths. `writes.json` is
`write_paths.step_write_paths` for 313 shell commands (redirects, each
writer's operands with good and bad flags, wrappers, `cd`), with no base
and placed from one. The Go and Rust unit tests check their ports against
both. `cases.manifest.json` pins the three files. Go and Rust `conform`:
2,322 of 2,322.

## Gate cases

A third kind checks a whole gate binary, not a verifier: one hook call in,
the host contract and every log line out. Unlike the corpus, gate cases
are synthetic (fake paths such as `/work` and `/home/user`), so they are
committed: `clients/fixtures/gate/cases.jsonl` with its manifest, written
by `clients/gate_cases.py` from the Python gate.

```json
{"kind": "gate", "v": 1, "id": "…", "name": "bash allowlist miss (enforce)",
 "tags": ["shell"],
 "argv": ["--mode", "enforce", "--root", "{ROOT}/data/gate", "--format", "claude", …],
 "stdin": "{\"session_id\": \"s1\", \"tool_name\": \"Bash\", …}",
 "env": {"CLAUDE_PROJECT_DIR": "/work"}, "coppice": false,
 "state": {"envelopes": {"default": "<envelope file text>"}, "disarmed": false,
           "config": null, "sessions": {}},
 "expect": {"summary": {"exit": 2, "host_verdict": "deny", "allow": false,
                        "would_deny": true, "tier": "permanent", "reason": "…"},
            "exit": 2, "stdout": "", "stderr": "…",
            "audit": [{"file": "s1.jsonl", "record": {…}}],
            "tree": {"s1.jsonl": […]}, "captures": {}, "coppice": []}}
```

- `{ROOT}` is the run's scratch directory, substituted in argv, stdin,
  env and envelope text; the gate root is `{ROOT}/data/gate`, so the
  session tree is `{ROOT}/data/sessions` and `config.yaml` is
  `{ROOT}/data/config.yaml`. `stdin_hex`, when present, replaces `stdin`
  with raw bytes (for invalid UTF-8).
- Every run gets `HOME=/home/user` and no `XDG_*`, `COPPICE_*` or `HERDR_*`
  variables beyond the case's own `env`. With `"coppice": true` the runner
  listens on `{ROOT}/c.sock` as `COPPICE_SOCK` with `COPPICE_PANE=pane-7`
  and records each line it receives.
- Envelope files are stored as text, since their key order is input.
- Volatile values are normalized on both sides before comparison: times
  and latencies (`at`, `ts`, `elapsed_ms`, `latencyMs`, `captured_at`)
  become `"<num>"` (and must have been numbers), `plan_xxxxxxxx` and a
  generated `env_xxxxxxxx` become `"<plan>"` and `"<env>"`, session-tree
  ids become `#1`, `#2`, … in order of first appearance, and `{ROOT}`
  replaces the scratch path.
- Canonical JSON for the file and the id escapes every non-ASCII
  character (`ensure_ascii`), because a case may hold a lone surrogate.
  Otherwise the content addressing is the corpus's.
- A graft case (router part 0, tag `graft`) adds three state keys, only
  when it has them, so no other case's id changed: `files` (real files
  under `{ROOT}`, each `text`, `hex`, `repeat` with `count`, `fifo`, `dir`
  or `symlink`), `grafts` (rule files in `{ROOT}/data/gate/grafts`) and
  `tier1` (the text of `{ROOT}/data/local_tier1.json`). 131 cases cover the
  limit escape, the line count, each rule field, the worker's host and
  grant, every format, an envelope that would deny the delegate call, and
  the delegate call's path (RP-1 to RP-7). The gate cases now number
  1,762; Go and Rust agree on all of them.

Comparison is structural, field by field (`clients/gate_compare.py`), and
each difference is classed by what it does to a host: **fail-open** (the
binary allowed what the oracle denied; a blocker), **stricter** (it denied
what the oracle allowed), or **record** (same verdict, a logged field
differs). All of `expect` is normative for a gate binary: reason text,
tier and log lines are what an operator reads.

`gate_compare.py --fuzz N --seed S` adds a seeded differential fuzz: N
single-line compound shell commands, each run as a Bash call against two
envelopes (decomposition on and off) through the in-process oracle and the
binary. Only the binary's native answers are compared (a hand-off is
Python's own answer); the run fails on any fail-open.

## Pathway cases

A fourth kind checks a pathway store and its matchers, not a verifier:
`clients/fixtures/pathways/`, written by `clients/pathway_cases.py` from
the Python store, matchers and CLI. They are synthetic and committed.

- `cases.jsonl`: one `daisugi pathways ...` command in a scratch HOME
  (`{HOME}`). A `pathways.db` in the tree is given as its rows (and
  `"schema": "legacy"` for the pre-migration table) and laid out by
  Python's `sqlite3`; `expect` holds the exit code, stdout, stderr (a
  traceback reduced to its exception's type) and the tree after, each
  database dumped as its `sqlite_master` SQL and its rows in rowid order
  (floats as `{"float": repr}`, a stored vector as its length and
  nonzero entries). `opendaisugi_version` values read `{VERSION}`.
- `find.jsonl`: rows, a `matcher_model`, the environment, and per query
  the matched id, its score rounded to 6 places, the stale-embedding
  warning, and `tie_ids` when rows tie within 1e-12. A client agrees when
  it picks the same id (or one of the tied ids), within 1e-6 of the
  score for `lexical` and 1e-5 for `potion`. `go_only` cases hold the
  refusal of a matcher a client does not carry; `real_potion` cases need
  `minishlab/potion-base-8M`, which no test downloads.
- `units.json` and `potion-tiny/`: BLAKE2b digests, lexical tokens and
  vectors, and a tiny model in model2vec's file format with its token ids
  and vectors.
- `verify_messages.jsonl`: plans and envelopes, and each violation
  `verify()` found as stage, step and message, the text `pathways import`
  prints in `VERIFICATION_FAILED`. A client checks them in its own tests
  (Go: `internal/verify/messages_test.go`; Rust:
  `pathways::verify::tests`); a refused skill delegation's message is the
  client's own.

`clients/pathway_compare.py` compares a binary on all of them, checks
that Python reads every database the binary wrote as the binary reads it,
and with `--fuzz N --seed S` runs seeded random queries against random
stores through both. It takes the binary (`--binary`) and its find
instrument (`--probe`), for the Go client (`clients/go/daisugi` and
`cmd/pathway-probe`) and the Rust client (`clients/rust/target/release/
daisugi` and `pathway-probe`) alike.

## Garden cases

A fifth kind checks the rest of the pathway life cycle:
`clients/fixtures/garden/`, written by `clients/garden_cases.py` from the
Python CLI. They are synthetic and committed.

- `cases.jsonl`: one `daisugi gardener ...`, `tend`, `hook auto-tend` or
  `distill-repeats` command in a scratch HOME (`{HOME}`). A pathway store
  is given as its rows and a journal as its traces (written through
  `Journal.log` with a fixed verification result), with times relative to
  the moment the tree is laid out. `expect` holds the exit code, stdout,
  stderr, the tree after (every database dumped whole: each table's
  schema and rows, its `user_version` and `sqlite_sequence`) and the model
  requests the run made.
- `model`: the recorded answers, keyed by the SHA-256 of the exact request
  (for `claude`, the argv after the program and the stdin; for the
  Anthropic API, the request body). The run's `claude` is a fake first on
  PATH and its API a fake server on 127.0.0.1; a request with no answer
  fails with exit 97 or HTTP 597. A client agrees only if it sends the
  oracle's request bytes. The fake server checks each credential sent
  against the case's own (HTTP 596 when it is not) and records only its
  name, never its value.
- A `tend api proxy` case sets proxy variables and puts the fake
  proxy of the gateway cases in front of the fake server; `expect` then
  also holds what the proxy got. The model call follows the proxy rules
  httpx reads (`clients/ADJUDICATIONS.md` LLM-11).
- Output is normalized on both sides before it is compared: a time near
  the run as its offset from the run's start, a minted pathway, env, plan
  or trace id by its order of first appearance, and durations hidden.
  Under `potion` a stored vector agrees within 1e-6.

`clients/garden_compare.py` compares a binary on all of them (stderr on
every case), checks that Python reads every store and journal the binary
wrote, and with `--fuzz N --seed S` runs random stores through prune,
merge, run and status on both sides. It takes the binary (`--binary`),
the Go client's `clients/go/daisugi` and the Rust client's
`clients/rust/target/release/daisugi` alike; both answer the same
request table, so both send the oracle's request bytes. A case the
binary refuses (exit 2, one line, the tree unchanged) is counted apart;
`--verbose` lists them, so two binaries' refusals can be compared.

## Envelope generation cases (stage K1)

`clients/fixtures/k1/`, written by `clients/k1_cases.py` from the oracle,
checks model-written envelopes and the reuse paths around them. The cases
are synthetic and committed. They use the garden cases' scratch HOME, fake
`claude`, fake model server and normalization, so a client agrees only if
it sends the oracle's request bytes.

- A `cli` case runs `daisugi generate-envelope`.
- A `probe` case is one query of the library, as JSON in argv: `generate`
  (the arguments of `generate_envelope`, with a pathway store, an envelope
  cache, a journal of refinement records, a Tier-1 provider or its
  `local_tier1.json`, a parent envelope or a model ladder), `inherit`,
  `bind`, `compose` or `recall`. The oracle answers through a script that
  calls its functions; a client answers through its own probe with the
  same argv and one JSON line on stdout. A query the client does not carry
  answers `{"unported": reason}` and must leave the tree unchanged.
- The tree may also hold an envelope cache, laid out with each row's
  `inserted_at` relative to the moment the tree is laid out.

`clients/k1_compare.py --binary B --probe P` compares a client on all of
them, and checks that Python reads every pathway store and envelope cache
the client wrote. A refused skill delegation is compared by its stage
only, as PW-12 rules. The Go and Rust binaries each answer through their
own `envelope-probe` and agree on all 186 cases but the three refused as
ruled (K1-1, K1-R-1).

## Supervised run and orchestration cases (stage K2)

`clients/fixtures/k2/`, written by `clients/k2_cases.py` from the oracle,
checks `daisugi run` and `daisugi orchestrate`. The cases are synthetic and
committed, and use the garden cases' scratch HOME, fake `claude`, fake model
server and normalization. Plan and envelope files are laid out as
`yaml.safe_dump` writes them. On top of the garden normalization, a run's id
(`run_<hex8>`), a step's duration (`duration_ms`, and the "1.2 ms" a run prints)
and a receipt's `evidence_hash` are made stable: each hash is checked against
its own row's evidence and recorded as `{HASH OK}` or `{HASH BAD}`.

`clients/k2_compare.py --binary B` compares a client on all of them and checks
that Python reads every journal and pathway store the client wrote. The Go
and Rust binaries each agree on 109 of 116; the same 7 are refused as ruled
(K2-3, K2-4, K2-5, K2-8, K2-R-1). CI runs it with `--max-refused 7`, so a
binary that refused more would fail.

## weave cases

`clients/fixtures/weave/`, written by `clients/weave_cases.py` from the
oracle, checks `daisugi weave`: typed slots between steps, the plan's slot
checks, the router's model for task steps, the filled step's rejection
(halt and recompute), resume, and the agentic and parallel paths the ports
refuse. The cases use the K2 cases' scratch HOME, fakes and normalization.
A case may run earlier commands with the same binary (`pre`), or lay out a
state file with start marks for its plan file (`weave_marks`); a plan hash
is minted like a run id (WV-R-11).

`clients/weave_compare.py --binary B` compares a client as `k2_compare.py`
does. The Go and Rust binaries each agree on 106 of 108; the same 2 are
refused as ruled (WV-R-10). A ranking id and a choice id are minted like a
run id (RK-R-15).

## rank cases

`clients/fixtures/rank/`, written by `clients/rank_cases.py` from the
oracle, checks `daisugi rank fit`, `choose`, `queue` and `record pick|drop`:
the attempts file's checks, elimination, quality tiers, the votes that
survive the order swap, the fit and its seeded bootstrap (B up to 1000),
owner constraints and cycles, cost, the next judge pair, the comparison
and choice journals, the switch cost from a journal index of receipts, and
decay. Times are laid out relative to the run (a card 6 or 8 days old), and
a `receipts` tree entry is a journal index with receipt rows at fixed run
ids. No case calls a model; the judges' votes are written into the case.
Help texts and click's usage errors are left to the CLI cases (RK-R-11).

`clients/rank_compare.py --binary B` compares a client as `garden_compare.py`
does. The Go and Rust binaries each agree on all 135; CI runs it with
`--max-refused 0`.

## MCP server, onboarding and tiers setup cases (stage K3)

`clients/fixtures/k3/`, written by `clients/k3_cases.py` from the oracle,
checks `daisugi mcp serve`, `daisugi onboard` and `daisugi tiers setup`. The
cases are synthetic and committed, and use the K2 cases' scratch HOME, fakes
and normalization. Every case puts a fake `nvidia-smi` first on PATH.

- An MCP case holds a session: the lines sent to the server's stdin. The
  driver waits for each request's reply, and after a line that gets no
  reply sends a ping and waits for that, so the order of the lines the
  server writes is fixed. `expect` holds every line of stdout, the exit code
  and the tree; stderr, the SDK's log, is not compared (K3-2). The cases
  cover the handshake, the protocol's errors (bad JSON, bad ids, a
  response, line endings, bad UTF-8, a request before initialize, a line cut
  off at the end of input) and each of the 11 tools with good and bad
  input, a model call through the fakes, a verify that fails and NaN.
- An onboard case lays out synthetic Claude Code and Codex transcripts,
  each with its own mtime. A trace id's hash of the transcript's path is
  written as the path it stands for (K3-12).
- A setup case's hardware budget comes from the fake GPU; the CPU count and
  memory are placeholders, so the fixtures replay on any box (K3-9).

`clients/k3_compare.py --binary B` compares a client on all of them and
checks that Python reads every journal and pathway store the client wrote.
An MCP request the client does not answer the oracle's way gets a JSON-RPC
error that says it is not in the binary yet; the session then agrees when
every line before it is the oracle's (K3-3). The Go and Rust binaries each
agree on 246 of 250; the same 4 are refused or not ported (K3-9, K3-13).
CI runs it with `--max-refused 4` for both.

The 36 `delegate` cases (router part 0) lay out a file, a rule and
`local_tier1.json` in the scratch HOME. The local worker is the chat wire
on the fake model server (`OPENAI_API_BASE`); a remote worker names a
`.invalid` host and is reached through the fake proxy, so no real host is
named or reached. Each call's journal row is in the tree, its time as
`{MS}` (RP-8 to RP-10).

## Module map, dashboard and metrics cases (stage K4)

`clients/fixtures/k4/`, written by `clients/k4_cases.py` from the oracle,
checks `daisugi modules`, `daisugi dashboard` and `daisugi metrics`. The
cases are synthetic and committed, and use the garden cases' scratch HOME,
fake `claude` and normalization.

- The oracle runs as a base install with only the extras the binaries
  carry, from a wheel, not a checkout (`clients/base_install.py`, K4-1), so
  the map does not depend on the packages of the box that records it. The
  start cases of the CLI compare run the same way.
- PATH holds only the case's bin directory and its own fakes: the map
  reports which of tmux, herdr, coppice, opencode and switchyard-server are
  on PATH. A coppice socket is made before the command and removed after
  (K4-7).
- A serve case starts `metrics --serve` on a free loopback port, waits until
  it listens, sends each request and writes each file a step names, then
  sends SIGINT. Each reply's status, Content-Type and body, and
  Content-Length on a 200, are recorded (K4-6).
- A case may carry `go_expect`, the fields where the binaries' answer is
  ruled to differ: under MiniLM or int8 the lexical rows are available, not
  active (K4-2).

- `status --json` cases check that the stores are read read-only (K4-8)
  and that an inf hit sum keeps the pathway count (K4-9).
- Hook cases feed `daisugi hook record` its payload on stdin. A case that
  names coppice gets a unix socket that listens, logs each line a client
  sends into the tree and answers `{"ok": true}`; a fake herdr logs its
  argv. Session-tree entry ids are renamed by first appearance (HR-3).

`clients/k4_compare.py --binary B` compares a client on all of them and
checks that Python reads every journal and pathway store the client wrote.
The Go and Rust binaries each agree on 188 of 193; the same 2 are not
ported (K4-3) and 3 refused (K4-4). CI runs it with `--max-refused 5` for
both.

## Registry, batch and signing cases (stage L)

`clients/fixtures/l/`, written by `clients/l_cases.py` from the oracle,
checks `daisugi registry`, `daisugi batch prove` and `daisugi release`,
and, through a probe, the library parts no command reaches: the deed
ledger, the strata store, batch runs, pathway bundles and the signing
primitives (`clients/l_probe_cases.py`; `clients/l_probe_oracle.py`
answers for the oracle, `cmd/l-probe` for Go, `l-probe` for Rust). The cases are synthetic
and committed, and use the garden cases' scratch HOME and normalization.

- A tree entry `gitbare` is a bare repository with the commits the case
  names; `gitclone` is a clone of one, with local commits and files of its
  own. The harness runs git with a scrubbed environment: no system or user
  config but the case's own, a fixed identity and fixed dates, and no
  search above the scratch root. The command under test gets the same
  variables and must drop them (L-15): its git reads the identity and
  default branch from the user config `XDG_CONFIG_HOME` names. Hostile
  cases set `GIT_DIR`, a work tree, an index file, a hooks path and an
  author, and the tree shows the decoy repository untouched. The tree reader summarizes a git directory (refs, log,
  files at HEAD, and a work tree's status) instead of reading its objects.
  Lines git writes to stderr are left out on both sides.
- Keys come from fixed seeds. A case whose output holds values made from
  the clock or a random key names them in `vary`; they are renamed by
  first appearance, and a value the case laid out is kept (L-2).
- `clients/l_compare.py` then checks those values with the oracle's own
  code: a published bundle hashes to its name and verifies under the key
  the case signed with; a signed manifest verifies and matches its files;
  a keygen public key is its private key's. Python must also read every
  pathway store and journal the binary left.

The Go and Rust binaries each agree on all 239, with none refused. CI
runs it with `--probe l-probe --max-refused 0` for both.

## Transcript, ingest and capture cases (stage F)

`clients/fixtures/f/`, written by `clients/f_cases.py` from the oracle,
checks `daisugi journal parse` over Claude Code sessions and Codex
rollouts, the same parsers under `daisugi onboard --dry-run`, `daisugi
journal ingest` of what they wrote, and `daisugi hook list` and `daisugi
hook to-trace`. The cases are synthetic and committed, written from the
parsers' own code, and use the K3 cases' scratch HOME, fakes and
normalization.

- Transcripts hold every row shape the parsers read, malformed lines,
  a line of more than 1 MB, deep nesting, an int past the digit limit and
  bytes that are not UTF-8 at the edges of Python's 8192-byte decode calls
  (F-2). A tree entry may be written as parts repeated a number of times,
  and a file over 32 KiB is compared by its size and SHA-256 (F-9).
- A large episode is split through the fake `claude` or the fake model
  server, and the request bytes are compared.
- A refusal where the oracle exits 2 with its own parse error is counted
  as refused, not as a disagreement (F-9).

`clients/f_compare.py --binary B` compares a client on all of them and
checks that Python reads every journal the client wrote. The Go and
Rust binaries each agree on 198 of 199 and refuse the same 1 (F-4); CI
runs it with `--max-refused 1` for both.

## Gateway cases

A sixth kind checks the token-saving gateway and its router:
`clients/fixtures/gateway/`, written by `clients/gateway_cases.py` from the
Python CLI. They are synthetic and committed.

- A `proxy` case starts `daisugi gateway` as a process on a free loopback
  port, in front of a fake upstream on 127.0.0.1 that answers from the
  case's table, keyed by the SHA-256 of the request path and the exact
  body bytes (HTTP 597 when no answer is recorded). A raw-socket client
  sends the case's requests; steps may send a signal, rewrite config.yaml
  or wait. `expect` holds whether the gateway came up, its exit code,
  stdout and stderr (less the server's own log lines, GW-2), each answer
  the client got (status, the headers that matter, the body, whether a
  stream arrived as it was sent), each request the upstream got (path,
  headers, body), and the tree after (turn journal, answer store,
  Switchyard files). Switchyard cases add a fake `switchyard-server` on
  PATH and record what it was started with and whether it outlived the
  gateway.
  An `env proxy` case also sets proxy variables and runs a fake proxy on
  127.0.0.1 (`clients/fake_proxy.py`); `expect` then holds what the
  proxy got: each CONNECT line with all its headers, each request line
  with `host` and `proxy-authorization`, and the TLS server name a client
  sent into a tunnel. The fake proxy sends a request on only to a loopback
  port or to the fake upstream and never forwards a tunnel, so no host a
  URL names is reached. The proxy rules each client follows are
  `clients/ADJUDICATIONS.md` PX-1 to PX-8.
- A `cli` case runs `gateway-report`, `router status|stop` or `route` once
  in a scratch HOME, as the CLI cases do; live children a state file names
  are started first when the case asks.
- A `recall` case runs queries against a laid-out pathway store or answer
  store: the oracle's `recall` and `recall_answer` in process, the
  client's through a probe named by `DAISUGI_RECALL_PROBE`
  (`cmd/recall-probe` for Go, `recall-probe` from `src/bin/recall_probe.rs`
  for Rust).
- A credential is recorded as a name, never its value; a case fails when
  the value reaches any record. Paths are `{HOME}` and `{WORK}`, ports
  `{GW}`, `{UP}` and `{SY}`, pids and times near the run are hidden, and a
  turn's `elapsed_ms` is `{MS}` (RP-11). The `router status measure` and
  `router status delegate` cases lay out turns, delegation rows and graft
  records on fixed dates (RP-12).

`clients/gateway_compare.py --binary B` compares a binary on all of them;
it takes the Go client's `clients/go/daisugi` and the Rust client's
`clients/rust/target/release/daisugi` alike. With `--fuzz N --seed S
--probe P` it also runs seeded turn sequences through the pipeline in
process on both sides, in chunks of at most 500 turns, and compares the
bytes sent upstream, the stream flag and each journal and answer-store
line (`cmd/gateway-probe` for Go, `gateway-probe` for Rust). `--speed`
times a buffered turn through the binary against the Go client's
`cmd/fake-upstream`. A case a binary refuses (exit 2, one line) counts
apart, and only where a ruling allows it: both binaries refuse the same
two (GW-8, GW-9).

## CLI cases

A seventh kind checks the CLI commands themselves: `clients/fixtures/cli/`,
written by `clients/cli_cases.py` from the Python CLI. They are synthetic
and committed.

- A plain case (the gate commands, `install`) is a file tree, one
  command, its stdin and env, run in a scratch HOME (`{HOME}`); `expect`
  holds the exit code, stdout, stderr and the tree after, JSON files
  parsed and a gate hook command compared by its flags (`{GATE}`).
- A case with `"runner": "rich"` (`status`, `config`, `start`, `journal`)
  runs as a garden case does: journals and stores laid out by the
  oracle's classes, a fake `claude` answering the case's `model` table,
  every database dumped whole, times and minted ids normalized. The path
  of the fake `claude` reads `{BIN}` and the digest `start` takes of the
  working directory `{CWD8}`. `start` cases record the argv of the one
  program the oracle launches (`spawned`); the oracle runs with that
  launch and its views faked, so no case leaves a process.
- `go_expect`, when present, holds the fields a ruling changes for a
  binary (clients/ADJUDICATIONS.md, C-11 to C-13), derived from the
  oracle's answer by the generator; a binary must match `expect` with
  those fields replaced. A `live` case depends on the box (its memory
  size) and is answered by the oracle again on each compare.

`clients/cli_compare.py --binary B` compares a binary on all of them:
the plain cases structurally, the rich ones as the garden compare does
(stderr on every case), and every journal a binary wrote must still
read in Python. A case a binary refuses (exit 2, one line, the tree
unchanged) counts apart.

`start` cases run a binary behind a wrapper script: the binary names
itself by the path it was run by, so its `start` launches the wrapper's
`gate serve`, which logs its argv and makes the socket file, as the
oracle's fake launch does (G2-4). No case leaves a server running.
The Go and Rust binaries answer every CLI case alike: the same cases
agree, are refused and are not ported in each (C-R).

`install --harness` cases compare the line that says how pi and
OpenCode ask the gate: the oracle and both binaries print it word for
word.

## Resident gate cases

An eighth kind checks the resident gate, `daisugi gate serve`, the
server the pi extension, the OpenCode plugin and `gate_client.py` ask
over `<root>/gate.sock`: `clients/fixtures/gate_server/`, written by
`clients/gate_server_cases.py` from the Python server
(`python -m opendaisugi.cli gate serve`). They are synthetic and
committed.

- A request case is the bytes one client sends (a JSON request line in
  the pi, OpenCode or gate_client shape, or raw bytes for a malformed
  one), the gate state it runs against, and the fakes it may reach: a
  coppice socket that places a hello as the pane it names, as another
  pane, or answers every line bare, and a `herdr` on the server's PATH
  that logs its argv. `expect` holds the reply bytes and every line
  written to the audit log, the session tree (with the modes of the
  sessions directory and files), the captures mirror, the fake coppice
  and the fake herdr. The request cases of one server environment share
  one server; each has its own gate root. A second environment gives the
  server its own pane variables and data directory, which no report or
  rule may take for a caller's.
- A scenario runs its own servers: twelve clients at once with a slow
  one holding a connection open, clients that leave mid-line or before
  their reply, `--help` (no reply, the server goes on), start (the root
  0700, the socket 0600, parents made), Ctrl-C (a newline, exit 0, the
  socket removed), SIGTERM (exit 0, the socket removed, also when it was
  ignored at start), a stale socket and a stale file, a second server
  that finds a live one and exits 1 while the first keeps the path, and
  roots that cannot be served.
- A request's own scratch directory is `{ROOT}`, the server's `{SRV}`,
  HOME is `/home/user`, a coppice `peer_pids` that names the client
  process is `<client pid>`, and times are `<num>`.
- `port_expect`, when present, holds the fields a ruling changes for
  both binaries, and `rust_expect` those for the Rust binary alone
  (clients/ADJUDICATIONS.md, G2-2 and G2-3).

`clients/gate_server_compare.py --binary B --port go|rust` starts the
binary's `gate serve` the way the cases start the oracle's and compares
every observation. A reply that allows where the oracle's denies is
fail-open, a blocker; a deny where it allows is stricter. `--oracle`
runs the Python server again to show a stale fixture, and both report
the p50 of a gate decision over the socket.

## Versioning

`v` is the conformance format version (this document). Bump it only for
incompatible case/verdict shape changes; additive informative fields are not
a bump. A client states the highest `v` it speaks and must reject cases with
a higher `v` via an `error` verdict.

## Known v1 limits

- Violation comparison uses `(stage, step)`; a per-violation machine `code`
  (finer than stage) is a v2 candidate once clients exist to consume it.
- `verify_step()` (the per-step hot path) is not recorded — whole-plan
  `verify()` cases subsume its checks.
- Cases embedding absolute paths from the generating machine are portable (a
  case is checked against its own envelope strings) but not *readable* as
  documentation; curate the spec's examples by hand, not from the corpus.

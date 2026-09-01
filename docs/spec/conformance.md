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
  Informative: messages, `suggested_remediation`, `detail` beyond `step`,
  warnings, durations.
- `decompose` cases — normative: `ok`, `heads` **in command order**, `reads`
  and `writes` as sorted sets. Informative: `commands` (source-text
  reconstruction is parser-dependent) and rejection `reason` strings.

## The wire protocol

A client is any executable. It reads one case JSON per line on stdin and
writes one verdict JSON per line on stdout (order-independent; verdicts are
matched by `id`), flushing per line:

```json
{"id": "…", "ok": true, "violations": []}                      // verify kind
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
  implement these by **emitting SMT-LIB2 text and invoking a solver binary**
  (Z3 or compatible) — never by binding a solver API. This keeps the solver
  interface identical across languages and the solver swappable.

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
            "shadow": [{"file": "s1.jsonl", "record": {…}}],
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

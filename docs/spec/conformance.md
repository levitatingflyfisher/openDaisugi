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

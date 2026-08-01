# Porting notes — what every client must get exactly right

Read together with `docs/spec/conformance.md` (the protocol) and the oracle
sources (the semantics). This file pins the traps that cost real debugging
time; it does not replace reading the oracle.

**Oracle sources, in porting order:**
`src/opendaisugi/models.py` (the data model) →
`src/opendaisugi/shell_decompose.py` (265 lines; 13,084 of 13,387 cases) →
`src/opendaisugi/verify.py` (stages; permissions core at lines 39–467) →
`src/opendaisugi/interpreter_parse.py` (wrapper/interpreter recursion) →
`src/opendaisugi/dag.py` →
`src/opendaisugi/z3_checks.py`, `predicate.py`, `predicate_z3.py`,
`contracts.py` + `inheritance.py` (the Full-profile tail).

**The corpus** lives at `.opendaisugi/conformance/corpus.jsonl` (13,387 cases;
NEVER commit it — it embeds real local paths). The synthetic fixture
`clients/fixtures/semantics.json` IS committed: 134 oracle-generated
answers for the matching semantics below. Unit-test your port against the
fixture first; corpus slices are the integration gates.

## Wire protocol (from `opendaisugi.conformance`)

stdin: one case JSON per line. stdout: one verdict JSON per line, flushed
per line, order-independent (matched by `id`). Logs go to stderr, never
stdout. A case you cannot process yields `{"id": …, "error": "…"}` and the
stream continues. Comparison is structural:

- verify: `(ok, sorted multiset of (stage, step-or-""))`. Emit
  `{"id", "ok", "violations": [{"stage": "...", "step": "s1" | null}]}`.
  Messages/remediation/detail are never compared.
- decompose ok=false: compared as bare `(false,)` — the `reason` string is
  informative. Getting the *reject* right matters; the wording does not.
- decompose ok=true: `(true, heads in command order, sorted(reads),
  sorted(writes))`. `commands` is emitted but never compared
  (source-text reconstruction is parser-dependent).

## Stage order (verify pipeline, `_verify` in verify.py)

Short-circuit after each stage that produced violations, in this order:

1. **delegation-safety** (stage `permissions`): stakes == "physical" AND any
   step with `preferred_model` set → reject.
2. **permissions** (`check_permissions`) — per-step by type; see traps below.
3. **skill subsumption** (stage `delegation`): SkillStep contracts, Z3-proved.
4. **z3**: envelope self-consistency, then plan-vs-envelope.
5. **predicate**: invariant/postcondition algebra (unsat/tautology need a
   real solver; backing-permission and alias checks are structural).
6. **z3** (robotics): joint targets/limits, peak velocity, workspace bounds,
   linearly-sampled trajectory vs obstacle boxes — deterministic numerics;
   replicate the sampling loop in z3_checks.py exactly.
7. **dag**: duplicate step ids → missing deps → cycle (each tier
   short-circuits the next; cycle violations have step=null).

`strict` resolution: explicit bool wins; null → true iff stakes ∈
{high, physical}. Under strict, unknown step types (not built-in, not in
`custom_step_allowlist`) reject; opaque skills and opaque safety properties
reject instead of warn.

## The two glob semantics (do not unify them!)

- **Head allowlist + mcp_allowlist** (`_head_allowed`): literal entries need
  exact equality; entries containing `*?[` match segment-wise — split BOTH on
  `/`, segment counts must be EQUAL, each segment via case-sensitive fnmatch.
  Left-anchored by construction.
- **File scopes** (`_path_matches_any`): `posixpath.normpath` the path, then
  (a) custom trailing-`/**` handler: prefix match against the normalized path
  (`glob[:-3]` equal, or prefix + "/"); (b) otherwise Python 3.12
  `PurePosixPath.match` semantics: match segments from the RIGHT for relative
  patterns (`"out.txt"` matches `/any/where/out.txt`), left-anchored only for
  absolute patterns, and a mid-pattern `**` behaves as a single-segment `*`
  (NOT recursive — that is 3.13 behavior the oracle does not have).
  Yes, the right-anchoring is suspicious; it is FROZEN for this campaign
  (see ADJUDICATIONS.md) — match the oracle, do not "fix" it in your client.

## Shell command checking (`_check_shell_command`)

- Metachar gate FIRST on the raw string: regex `[;|&`+"`"+`<>\n\r]|\$\(`.
- If hit and `shell_allow_decomposition` is true → decompose (tree-sitter
  grammar or equivalent). ok → check redirect targets against file scopes
  (sanctioned sinks `/dev/null`, `/dev/stdout`, `/dev/stderr`; sources
  `/dev/null`, `/dev/stdin` bypass), then each simple command through the
  single-command path. NOT ok → metachar violation with the decompose reason
  in detail. The regex is NOT re-run on decomposed parts.
- Single-command path: extract head (skip `NAME=value` env prefixes; `#` or
  empty → nothing executes, no violation), check allowlist, then
  `parse_interpreter`: opaque interpreter + policy "strict" → violation;
  transparent wrappers (`sh -c`, `xargs`, `find -exec`, `env`, `timeout`,
  `nice`, …) recurse into payloads at depth+1, max depth 4.

## Decomposition (shell_decompose.py) — 98% of the corpus

Pre-order walk; first reject reason wins. Pin these:
- `has_error` → reject; any `is_missing` node → reject.
- `file_redirect` nodes: skip `file_descriptor` children; first remaining
  child's TYPE is the operator, next child the destination. `>&-`/`<&-` with
  no destination pass. `number` destination + `>&`/`<&` pass (fd dup);
  `number` + any other operator → reject. Literal destination = plain
  `word` | `raw_string` (quotes stripped) | `string` of pure
  `string_content`. Writes: `>` `>>` `&>` `&>>` `>|` `>&`; reads: `<` `<&`;
  anything else rejects. Never walk beneath a `file_redirect`.
- `command` nodes: the `name` field's children must be exactly `[word]`,
  else reject (non-literal head). Append head + full command text, then KEEP
  walking children (substitutions inside arguments surface inner heads).
- Heredocs/herestrings are not file redirects (stdin data); their bodies are
  walked. Process substitution as redirect DESTINATION is non-literal.
- After the walk: zero heads → reject ("no command heads found").
- Rejected decompositions compare as bare ok=false — your parser's reason
  wording is free, its accept/reject boundary is not.

## web-tree-sitter offset units (TS and other wasm-grammar clients)

`node.startIndex`/`endIndex` from **web-tree-sitter are UTF-16 code-unit
offsets**; native tree-sitter (the Python oracle, the Rust client) reports
**UTF-8 byte offsets**. Any span arithmetic (e.g. the G-4 bare-newline scan)
must index the source in the SAME unit the grammar reports — scan the JS
string with `charCodeAt`, not a UTF-8 byte array. A byte-indexed port passes
every ASCII case and silently corrupts on the first multi-byte character.

## Full profile (the ~46-case tail)

Emit SMT-LIB2 **text** and run `z3 -in` as a subprocess (`z3` is on PATH;
`~/.local/bin/z3`, v4.16). Never link a solver API. The structural parts of
the predicate stage (backing-permission map, unresolved alias, opaque safety
property under strict) need no solver — port them first; only
unsat/tautology classification and expression subsumption need Z3.

## House rules for client work

- Work only inside your `clients/<lang>/` directory; never touch oracle code.
- Build artifacts stay on real disk (`target/`, `node_modules/` under your
  client dir). NOTHING in `/tmp`. Cap build parallelism at 2 jobs.
- When your client disagrees with the oracle and you believe the ORACLE is
  wrong: add an entry to `clients/ADJUDICATIONS.md` (case id, both verdicts,
  your analysis), then match the oracle anyway. Oracle fixes land after the
  tournament and the corpus regenerates.
- No git commits — the reviewing session commits after review.

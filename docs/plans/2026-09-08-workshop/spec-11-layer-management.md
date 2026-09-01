# Spec 11 — Layer management: `daisugi bench`, hot swap, verifier dispatch

**Master:** §5.8, §5.9, §3.5 · **Size:** L · **Depends on:** 03 (loop as a pane), 09 (router
bench), 10 (int8 in the matcher bench)

## Purpose

One front door for comparing the options within a layer, five cross-layer pairs that earn a row,
and hot swap for every stage the architecture lets us reload in process. Every table prints the
command that reproduces it. Every `live` tag is true.

## The cruxes

- **Per layer, not n×n.** Contracts make layers independent by construction; the five pairs are
  where a real coupling exists. Promotion, not speculation, adds a row.
- **Honesty over convenience for `live`.** Gate mode stays `cfg` because the hook file is the
  harness's. Everything we own in-process becomes live *and is tested to be live* (change the
  config, do not restart, observe the new behaviour).
- **Verifier dispatch never out-allows the oracle** (ruling 2026-09-08 after review). Python is
  the oracle and it always runs; its cost is about 0.13 ms. With a compiled client selected, the
  gate acts on `oracle AND client` (both must allow). A client that is missing, crashes, times out,
  or emits garbage leaves the oracle alone with a `WARNING` and `fallback="python"`. The client's
  own verdict is recorded as `client_verdict` for bench attribution. Why: the Lean client
  implements only the Core profile (`clients/lean/DaisugiVerify/Verify.lean` ≈199–209) and would
  allow any invariant- or Z3-protected envelope; a compiled client is a *second opinion*, never a
  replacement for the reference. Cost: one extra sub-millisecond call per verify.

## Files

```
src/opendaisugi/bench/__init__.py
src/opendaisugi/bench/registry.py        # BenchSpec(name, layer, corpus, columns, run(opts) -> Table)
src/opendaisugi/bench/layers/verifier.py # wraps clients/gate.py conformance harness (existing)
src/opendaisugi/bench/layers/matcher.py  # wraps scripts/matcher_fpr.py (moved into the package; script becomes a shim)
src/opendaisugi/bench/layers/router.py   # replays a journal slice through rules vs switchyard fakes; cost + share
src/opendaisugi/bench/layers/loop.py     # the harness battery (docs/harness) as code: 9 cases × loops available
src/opendaisugi/bench/layers/gate_path.py# hook contract tests as a table with the fail-open class column
src/opendaisugi/bench/layers/backend.py  # envelope-generation quality: N tasks → envelopes → verify-rate + tightness
src/opendaisugi/bench/pairs.py           # the five pairs, each a function composing two layer benches
src/opendaisugi/bench/table.py           # human table + --json; "reproduce:" line
bench/corpus/                            # small committed corpora (tasks.jsonl, screens/, journal-slice.jsonl); large frozen corpora referenced by path, skipped when absent
src/opendaisugi/cli.py                   # `daisugi bench <layer>|pairs|all [--json] [--corpus PATH]`
src/opendaisugi/swap.py                  # STAGE_EFFECT: matcher, backend, router, verifier, floor(loop) → LIVE
src/opendaisugi/_search.py               # reload(): drop the cached embedder + warned set; next call re-resolves
src/opendaisugi/pathway_store.py         # lazy re-embed when provenance ≠ active identity (already refuses stale rows; add re-embed on `tend`)
src/opendaisugi/gateway.py               # config reload on SIGHUP and `POST /_reload` from loopback only
src/opendaisugi/verifier_dispatch.py     # verify_via(client_name, plan, envelope) using the conformance clients' CLI protocol
src/opendaisugi/gate.py                  # uses verifier_dispatch when config.verifier_client != "python"
tests/bench/…                            # every bench runs on the committed corpus in < 30 s
tests/test_hot_swap.py                   # per stage: change config → behaviour changes without restart
tests/test_verifier_dispatch.py          # each client if built; fallback path; never-allow-on-failure
```

## `daisugi bench`

```
daisugi bench matcher            # rows: MiniLM, potion, lexical, int8 (present ones); cols: FPR@thr, recall@thr, ms/embed, MB
daisugi bench verifier           # rows: python, rust, go, ts, lean; cols: cases, agree, disagree, fail-open (must be 0), ms/case
daisugi bench router             # rows: rules, switchyard:stage, switchyard:escalation, off; cols: turns, local share, est. cost, pass-rate (if outcomes)
daisugi bench loop               # rows: sprig, claude-code, codex, pi, opencode; cols: 9 battery cases, gate path, fail-open class
daisugi bench gate-path          # rows: claude hook, codex hooks.json, pi ext, opencode plugin, sprig gate, MCP; cols: contract tests, fail-open class, round-trip ms
daisugi bench backend            # rows: configured backends; cols: envelopes generated, verify-rate, mean clauses, tokens
daisugi bench pairs              # the five: verifier×shell, matcher×distiller, router×models, loop×gate-path, backend×envelope
```

Every table ends with `reproduce: <exact command>` and a `corpus: <path> (<sha256[:8]>)` line.
`--json` emits `{layer, corpus, rows, columns, reproduce}`. A row for an option that is not
installed is printed as `absent` with the install command, never omitted silently.

## Hot swap

| stage | mechanism | test |
|---|---|---|
| matcher | `_search.reload()`; `PathwayStore.find` re-resolves identity per call; `tend` re-embeds rows whose provenance ≠ active | set `lexical`, call find, set `potion`, call find: provenance of new rows differs; no restart |
| backend | already per-call; `swap.py` tag corrected to LIVE; `modules` reflects it | change `llm_backend`, generate an envelope, the fake backend that answered is the new one |
| router | gateway watches config mtime every 2 s and on SIGHUP; `POST /_reload` loopback-only | change `router.kind`, next turn journals the new route source |
| verifier | `verifier_dispatch` chosen per verify call from config | set `rust` (if built), verify, verdict carries `client: rust`; set `python`, verdict carries `python` |
| loop (floor) | a pane is a process; "swap" = spawn with another harness | contract suite already covers spawn per harness |
| gate | stays `cfg`; the tag text says "reinstall: the hook file belongs to the harness" | a test asserts the tag text |

`swap.py`'s `EFFECT_TAG` text for `cfg` gains the reason string per stage so the GUI shows *why*.

## Verifier dispatch

`verify_via(client, plan, envelope, *, timeout_s=5.0) -> VerificationResult` runs the compiled
client through the same stdin/stdout JSON protocol `clients/gate.py` uses for conformance. On any
failure (binary missing, non-zero exit, parse error, timeout) it returns the Python oracle's
result with `fallback="python"` and a `WARNING` naming the failure. It never returns allow from
a failed client. `daisugi modules` marks a client `ACTIVE` only if its binary is present and the
last dispatch succeeded.

## Tests

- Each bench on the committed corpus under 30 s; JSON schema check; `reproduce` line re-runs to
  the same rows (determinism).
- The five pairs produce a table each; a pair whose layer bench lacks an installed option prints
  `absent` rows.
- Hot swap tests as in the table, each named `test_<stage>_swap_is_live_without_restart`.
- Verifier dispatch: fake client scripts for success, crash, timeout, garbage; the fallback field;
  `test_dispatch_never_allows_on_client_failure`.
- Honesty: `test_stage_effect_tags_match_reality` walks `STAGE_EFFECT` and, for every `LIVE`
  stage, calls the corresponding swap test; for every `cfg` stage, asserts a reason string exists.

## Out of scope

Distributed benches; publishing numbers anywhere; tuning any option based on a bench in this
pass.

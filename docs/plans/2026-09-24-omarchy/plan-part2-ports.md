# Part 2 plan: Go and Rust, the whole stack

Written 2026-09-24. The map for the ports. Each stage gets its own brief when
it starts. Python stays the oracle until a port has passed the same suite for
a release.

## The method

1. **Conformance at process boundaries.** Each component has golden cases:
   stdin to stdout and exit code, or request to response, or files in to files
   out. Cases are synthetic and live in `clients/fixtures/<component>/`. The
   Python oracle writes the expected results. A port passes when it agrees on
   every case and on a seeded fuzz, structurally, not textually.
2. **Never looser than the oracle.** Where a port cannot yet decide a case, it
   hands the case to Python or refuses it. It never allows what Python denies.
3. **Module by module.** Each implementer gets one module's brief, its cases
   and the Python file. Never the whole codebase.
4. **Both ports share the suite.** Go and Rust run the same cases. A case one
   port fails is a finding in that port or in the oracle, and an adjudication
   is written for it in `clients/ADJUDICATIONS.md`.
5. **daisugi stays importable alone** in each language: the gate core never
   depends on the floor or the loop.

## What is ported

| Stage | Component | Python today | Notes |
|---|---|---|---|
| A | gate hook, verifier, decomposition, effects tiers, hard-deny rules, session tree, state report | gate, hook, verify, effects, floor_config, shell_decompose, session_tree, _state_report | Go done (daisugi-gate); Rust next |
| B | config, envelopes and their store, gate root, journal | config, envelope, envelope_cache, journal, models, contracts | file formats are the cases |
| C | install into harnesses, hooks settings | install, gate settings | writes files; cases are before and after trees |
| D | pathways, matcher, garden | pathway_store, git_pathway_store, pathway_params, _search, gardener, distiller | the potion matcher is a static table: port natively; LLM calls go through one small client |
| E | gateway and router client | gateway, gateway_asgi, gateway_pipeline, gateway_journal, gateway_answers, gateway_recall, router_switchyard, routing | an HTTP proxy; cases are recorded request and response pairs with fake upstreams |
| F | ingest and parsers | ingest, parsers/, decomposer | transcripts in, records out |
| G | the CLI | cli, start, status parts of modules and dashboard | one binary, `daisugi` |
| H | voice client and server | voice/ | the speech model stays external (faster-whisper or whisper.cpp) |
| I | coppice in Rust | harness/coppice (Go) | protocol cases already exist in `testdata/protocol` |
| J | sprig in Rust | harness/sprig (Go) | small |
| K | the parts that use pathways, and envelope generation | envelope (model-generated envelopes), routing, tier1, pathway_bind, compose, gateway_recall, orchestrator, orchestration_executors, supervisor, decomposer, facade, mcp_server, onboarding, modules, dashboard, exporter | Go and Rust; without these, the binaries make pathways that only Python uses |

## Full parity

Owner ruling, 2026-09-26: openDaisugi publishes competing binaries, Python, Go
and Rust, that check each other for mistakes, correctness and speed. So every
part a Go or Rust user would need Python for is ported to both. The list below
holds only the parts that no binary needs.

## What is not ported, and why

These stay Python, or retire. Each can be reopened.

- **Robotics executors** (`vla_executor`, `executor_mujoco`, robotics fields in
  envelopes). Research code with native simulator dependencies. The robotics
  checks in the verifier are ported; only the executors stay Python.
- **LoRA training** (`lora/`). Training belongs in Python's ML stack.
- **The benchmark harness** (`bench/`). A research tool.
- **The Textual TUI** (`tui*.py`, `cockpit`). coppice replaces it.
- **The tmux and Herdr floor backends** (`floor/tmux_backend.py`,
  `floor/herdr_backend.py`). coppice replaces them. The Herdr socket stays
  readable by coppice itself.

## Rulings: standalone binaries

Owner ruling, 2026-09-25: each port is a standalone binary. It never calls
Python at run time, in any path. Python is the oracle the tests compare
against, and nothing more.

- **Z3.** Both ports link real Z3, built from pinned source and linked
  statically: Rust through the `z3` crate, Go through Z3's C API with cgo. They
  run the same checks as the oracle: the envelope and plan checks, the
  predicate-algebra invariants and postconditions, and the robotics bounds.
  Ground checks may still be computed directly, since Z3 decides them the same
  way; the tests prove the two agree.
- **Shell parsing.** Both ports parse shell with tree-sitter-bash, the grammar
  the oracle uses, and port the oracle's decomposition exactly, fusion repair
  included. So a port answers every command the oracle answers, heredocs,
  multi-line commands and non-ASCII text included.
- **No hand-off.** The hand-off to Python is removed from both gates. A case a
  port cannot decide is a bug to fix. Until it is fixed, the port denies it
  with a reason, never allows it.
- **libghostty-vt.** Go coppice keeps it through cgo, linked statically. Rust
  coppice links the same C library through FFI.
- **The potion embedder.** A static embedding table and a tokenizer, ported
  natively, with the model file fetched and pinned by hash.
- **Done means:** on the case suite, the local corpus and the fuzz, each port
  answers 100% of calls itself and agrees with the oracle on every one.

## Order

1. Rust stage A, against the existing gate cases.
2. Go stages B and C, then G's gate and install commands, so `daisugi` is one Go
   binary for the hot path and install. This is the Omarchy single binary.
3. Rust B and C.
4. D in Go, then in Rust (done 2026-09-26).
5. E, then K, then F, each in Go, then in Rust.
6. H, then I and J.

## Speed budgets

- A gate check answers in under 10 ms at p50 on the owner's box when the port
  answers natively.
- `daisugi --help` and `coppice --help` start in under 20 ms.
- A gateway turn adds under 5 ms over the upstream.

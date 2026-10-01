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
| H | voice client and server | voice/ | Built 2026-10-01 in Go and Rust: the speech model stays external, whisper.cpp's `whisper-cli`; 430 voice cases agree in each (VO-0 to VO-8) |
| I | coppice in Rust | harness/coppice (Go) | Built 2026-10-02 in `harness/coppice-rs`, all six slices: the server core (slice 1), then screen detection, `pane.explain`, the ready-prompt rules and `pane.trust`, held asks and the foreman, `floor.talk`, subagents and the facts (slice 2), then ended panes, `pane.forget` and `pane.resume`, the restore of `layout.json` at start, `project.list` and task worktrees (slice 3), then the five headless adapters, `pane.fork` and prompts to a harness (slice 4), then the command line, attach, the tmux mirror, plugins and the voice supervisor (slice 5), then the tiles model, `coppice web` and the phone server with its TLS, local CA, websocket and push (slice 6a), then the floor on a terminal, compared as a screen through each side's libghostty-vt (slice 6b). All 1541 cases of seven suites (the 70 protocol corpus cases, 301 command-line cases, 395 web cases and 230 floor-screen cases among them), all 64 event views and all 19 data-dir tree views agree with Go (`clients/coppice_compare.py`, CP-R-1 to CP-R-45); `e2e/run.sh tui` passes against each binary, and `run.sh web` passed against each at slice 6a. The Rust binary is named `coppice`; `COPPICE_PORT=rust` installs or releases it in place of the Go one, which stays the default. The map is [plan-coppice-rust.md](plan-coppice-rust.md) |
| J | sprig in Rust | harness/sprig (Go) | Built 2026-10-02 in `harness/sprig-rs`: the four binaries the Go module builds (`sprig`, `sprig-hook`, `sprig-mcp`, `weave`), with the loop, the tools, the process gate, the session tree and its resume, the claude and API backends (https through rustls), the command line, the hook and the MCP server, and Go's JSON, flag and error words. All 185 cases of `clients/sprig_compare.py` agree with Go, 12 of them through the real Go and Rust daisugi, none refused (SP-R-1 to SP-R-6). `SPRIG_PORT=rust` installs or releases it in place of the Go one, which stays the default |
| G2 | the resident gate server (`gate serve` socket) | gate_server, the pi and OpenCode clients' socket contract | Go and Rust; without it, pi and OpenCode deny every call on a Go-only or Rust-only install |
| L | deeds, batch compilation, strata, pathway signing, the git pathway registry | deeds ledger, batch, strata, signing, pathway_bundle signing, git_pathway_store | Go and Rust; listed so full parity has no hidden Python-only parts |
| K | the parts that use pathways, and envelope generation | envelope (model-generated envelopes), routing, tier1, pathway_bind, compose, gateway_recall, orchestrator, orchestration_executors, supervisor, decomposer, facade, mcp_server, onboarding, modules, dashboard, exporter | Go and Rust; without these, the binaries make pathways that only Python uses |

## Full parity

Owner ruling, 2026-09-26: openDaisugi publishes competing binaries, Python, Go
and Rust, that check each other for mistakes, correctness and speed. So every
part a Go or Rust user would need Python for is ported to both. The list below
holds only the parts that no binary needs.

With stage J built on 2026-10-02, every stage in the table above has a
built port. Full parity is not reached yet. The inventory below lists what
was still missing on 2026-10-02: every row that `docs/feature-status.md`
marked Open or Part for Go or Rust, and every ruling in
`clients/ADJUDICATIONS.md` still in force that says a port refuses, or does
not port, something a user could need.

### The inventory (2026-10-02)

Verdicts: **port** (this round ports it to both), **open** (needed, not
ported yet), **not ported** (moved to the list below, with the reason),
**not a gap** (no Python part is missing from the ports).

| # | What a Go or Rust user cannot do | Where it is written | Python | Cases that measure it | Verdict |
|---|---|---|---|---|---|
| 1 | Envelopes whose `expr` asks a model (`llm_check`): an invariant on `run` and `orchestrate`, an enforced postcondition at stage 2, the MCP tools, `pathways import`, and the gate under the claude-code backend. The ports refuse them. | feature status rows "Stage 2 output checks" (Open); K2-8, PW-R-5, K3-3; the gate's `llm.go` and `llm.rs` | `llm_check.py`, `predicate_z3.py` (`evaluate_predicate`), `verify.py`, `stage2.py` | k2 `run llm_check *`, `orch llm_check *`; k3 `mcp step llm_check *`, `mcp verify llm_check invariant`; pathway `import llm_check *`; gate `llm claude *` | ported 2026-10-02 (PG-1); a dict expr that does not parse stays refused, where the oracle raises (K2-8) |
| 2 | Named definitions (`alias`). No oracle command builds an alias registry: every command reports an `alias` as unresolved, with the oracle's words. The registry (tiers, system names, one-pass substitution, cycles, deferred vacuity, the shipped system aliases) is a library part that only `integrations/hermes.py` calls. The ports refuse an alias postcondition, and the MCP tools refuse an alias. | feature status row "Definitions" (Part); K2-8, K3-3 | `aliases.py`, `system_aliases.py`, `verify.py`, `stage2.py`, `integrations/hermes.py` | k2 `run alias *`, `orch alias postcondition`; k3 `mcp step alias`, `mcp verify alias invariant`; 23 alias probe cases | ported 2026-10-02 (PG-2, PG-5); the Hermes YAML loaders are not ported (below) |
| 3 | `AgenticStep` and `AgenticExecutor`. The oracle wires it in `weave` only; `run` and `orchestrate` fail an agentic step with "no executor for kind 'agentic'". The ports refuse a weave plan with an agentic step. | feature status row "AgenticStep" (Open); K2-7, WV-R-9, WV-R-10, TR-R-10 | `agentic_executor.py`, `weave.py`, `tree.py` (`edge_ok`), `gate.py` (`gate_settings_json`) | weave `agentic *` (12); k2 `run agentic step has no executor` | ported 2026-10-02 (PG-3, PG-4) |
| 4 | `daisugi verify PLAN --envelope ENV [--json]`. | the ports' list of commands not in the binary | `cli.py` (`verify_cmd`), `verify.py` | cli `verify *` (13) | ported 2026-10-02 (PG-6) |
| 5 | The command-line `hook report` (a pane state event on stdin). | HR-4 | `cli.py` (`hook_report_cmd`), `_state_report.py` | cli `hook report *` (15) | ported 2026-10-02 (PG-6) |
| 6 | `install --ask` (the operator ask in the installed hook). | C-8 | `install.py`, `cli.py` | cli `install gate ask`, `install gate ask audit` | open: those cases run the full oracle install, so a gate-only form of them is needed first (row 7) |
| 7 | The skill, MCP, capture and instruction layers of `install` (the ports' `install --gate` writes the gate layer only, and is compared with an oracle patched to do the same), `install` without `--gate`, `--gateway` or `--harness`, `install --print-skill`, `install --uninstall` of all of them, `install --allow-shell-decomposition`, `install --enforce` without `--gate`, `install --report coppice`. | the ports' `installcmd`; cli `install plain refused`, `install decomposition refused`, `install enforce no gate`, `uninstall all not ported`, `install report refused` | `install.py`, `cli.py` | those cli cases | open |
| 8 | `gate replay` (a captured session through the gate, offline). | the ports' `gate` command | `gate.py` (`replay_captures`), `cli.py` | cli `replay not ported` | open |
| 9 | `orchestrate` and `weave` with `--max-parallel` above 1. | K2-4, WV-R-10 | `orchestrator.py`, `weave.py` | k2 `orch max parallel`, weave `max parallel two` | open |
| 10 | `tiers stats`, `tiers setup --remote`. | K3-9, K3-13 | `accounting.py`, `cli.py`, `local_setup.py` | k3 `tiers stats`, `setup remote` | open |
| 11 | MCP requests the ports answer with -32000: `resources/read`, `resources/subscribe`, `completion/complete`, the `tasks/*` methods, task-augmented calls. Item 1 and 2 close the `llm_check` and `alias` parts of K3-3. | K3-3, K3-4, K3-8 | `mcp_server.py` and the MCP SDK | Go and Rust tests; no case | open |
| 12 | Input the ports' readers do not model, refused before anything is written: YAML outside the `safe_dump` form and the config subset, a skill file's frontmatter in another form, a plan step given as a string, a stale-embeddings warning, proxy settings httpx reads differently, a Switchyard TOML file outside the reader. | K2-3, K2-5, C-7, PW-6, PW-R-5, PX-5, GW-9 | `config.py`, `pathway_store.py`, `gateway.py`, PyYAML, httpx | k2 `run envelope not yaml`, `run plan not yaml`, `orch envelope not yaml`, `orch stale embeddings warn`; garden `tend yaml not safe_dump form`; gateway `switchyard own config not toml` | open |
| 13 | `daisugi viz` (an HTML page of a pathway's plan). | the ports' list of commands not in the binary | `viz.py`, `cli.py` | none | open |
| 14 | `daisugi models` (find and pin a local model on the Hub, `--pull`). | the ports' list of commands not in the binary | `model_registry.py`, `_model_fetch.py`, `cli.py` | none | open |
| 15 | `daisugi help [--all]`. | the ports' list of commands not in the binary | `cli.py` | none | open |
| 16 | The Rust `daisugi` from the one-script install: `scripts/install.sh` builds only the Go one (`release.sh --rust` builds both). The AUR files are not published for either port, so that part is not a gap. | feature status row "One-script install" (Part for Rust) | n/a | none | open |
| 17 | The MiniLM and int8 matchers. | C-12, K3-10, K3-13, K4-1, K4-2, DS-3; garden `tend minilm` and the other MiniLM cases; k3 `onboard minilm config` | `_search.py` with `sentence_transformers` and onnxruntime | those cases | not ported |
| 18 | `gate audit` and `conformance export`, `run`, `bench` and `serve`; journal ingest and onboard with `OPENDAISUGI_CONFORMANCE_RECORD` set. | the ports' list of commands not in the binary; cli `journal ingest conformance record` | `adversarial.py`, `conformance.py` | that cli case | not ported |
| 19 | `install --report herdr`, and the Python `daisugi coppice` command group (panes over tmux or Herdr). | cli `install report refused` (the herdr half) | `floor/`, `install.py` | that cli case | not ported |
| 20 | `dashboard --tui`, `dashboard --serve`. | K4-3 | `tui*.py`, `cockpit.py` | k4 `dashboard tui`, `dashboard serve` | not ported (already listed: the Textual TUI) |
| 21 | `ClaudeCodeTier1Provider`. | K1-1 | `tier1.py` | none | not a gap: no oracle config or command builds it |
| 22 | Router part 4, grafts, dialects, the delegation tree in coppice and sprig, the weave proposals. | feature status (Part, Designed or Proposed in Python too) | n/a | n/a | not a gap: unfinished in every language |
| 23 | The Codex and sprig adapters of coppice, `coppice attach --remote`. | feature status, the coppice table | n/a | n/a | not a gap: these compare Go with Rust, not with Python |
| 24 | The robotics executors: `MuJoCoExecutor` (sim_reset, joint_move, gripper, cartesian_move with its IK, the torque and contact guards), `robotics_executors`, `VLAExecutorBase` and `MockVLAExecutor`. No oracle command builds them; a library caller hands them to a `Supervisor`. A run whose robotics checks reject the plan was refused, since those violations carried no detail. | this section's list "What is not ported" (before 2026-10-02) | `executor_mujoco.py`, `vla_executor.py`, `z3_checks.py` | robotics: 74 cases against the oracle and 5 render cases between the ports | ported 2026-10-02 (RB-R-1 to RB-R-7): our own MuJoCo wrappers (cgo in Go, hand-written FFI in Rust, one shared C layer), behind the `mujoco` build tag and Cargo feature, measured on each port's `robot-probe`; `TransformersVLAExecutor` (SmolVLA inference) followed on 2026-10-03 (row 25) |
| 25 | SmolVLA inference: `lerobot/smolvla_base` run without Python, as `TransformersVLAExecutor` means to run it. | this section's list "What is not ported" (before 2026-10-03) | `vla_executor.py` (and lerobot's policy as the oracle) | vla: 30 tokenizer cases, the processing, a golden chunk, and the closed loop in the pick-place scene | ported 2026-10-03 (VL-R-1 to VL-R-7): exported once to three pinned FP32 ONNX graphs, run through our own ONNX Runtime wrappers (cgo in Go, hand-written FFI in Rust, one shared C layer) with a hand-written tokenizer, behind the `mujoco` build tag and Cargo feature; measured on each port's `vla-probe` |
| 26 | LoRA training and the SmolVLA reference policy, reached from Go and Rust. They stay Python; the binaries install a pack (a pinned CPython, a venv, a hashed lock) and call a worker in it. | owner ruling 2026-10-03 | `lora/train.py`, `pack/vla_oracle.py` (the oracle of VL-R-2, one module), `pack/worker.py` (the protocol) | pack: 24 cases with a fake CPython and a fake wheel index | ported 2026-10-03 (PK-R-1 to PK-R-11): `daisugi pack list, install, remove, status, bundle, run` and `daisugi lora train` in all three; one real install and one real train step on this box (PK-R-8); the vla-ref pack never installed here (PK-R-4) |

## What is not ported, and why

These stay Python, or retire. Each can be reopened.

- **Exporting a VLA policy.** The ports run SmolVLA without Python (row
  25), but making the graphs needs lerobot and torch once
  (`clients/vla_export.py`, in a separate pinned environment).
- **LoRA training** (`lora/`). Training belongs in Python's ML stack.
  The binaries reach it through the train pack (row 26).
- **The benchmark harness** (`bench/`). A research tool.
- **The Textual TUI** (`tui*.py`, `cockpit`). coppice replaces it.
- **The tmux and Herdr floor backends** (`floor/tmux_backend.py`,
  `floor/herdr_backend.py`). coppice replaces them. The Herdr socket stays
  readable by coppice itself. With them go the Python `daisugi coppice`
  command group and `install --report herdr`.
- **The MiniLM and int8 matchers.** They need torch or onnxruntime and a
  model download. The ports carry the lexical and potion matchers, and
  refuse a config that names MiniLM or int8 before anything is written.
- **The Hermes alias-file loaders** (`integrations.hermes.envelope_from_yaml`,
  `load_household_aliases`). Library calls for the Hermes agent, which is
  Python; no binary reads an alias file. The registry they fill is ported
  (PG-5).
- **The oracle's own instruments.** `gate audit` (the adversarial corpus
  run through the Python gate), the `conformance` commands, and the
  `OPENDAISUGI_CONFORMANCE_RECORD` corpus recording. They measure the
  oracle; the compares in `clients/` measure the ports against it.

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

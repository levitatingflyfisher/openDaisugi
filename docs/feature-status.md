# Feature status

One row per feature, with its status in each language. Refreshed 2026-09-26 from the
READMEs ([`clients/go`](../clients/go/README.md), [`clients/rust`](../clients/rust/README.md),
[`harness/coppice`](../harness/coppice/README.md)), the committed golden cases and the build
ledger. For what each problem still needs, see the [roadmap](roadmap.md).

**Status words.**
- **Built**: code exists and is tested. The evidence column says how.
- **Part**: some of it is built; the note says which part.
- **In progress**: work is under way and not yet committed.
- **Designed**: a design document exists; no code.
- **Proposed**: designed, and on hold with the owner (sprig, weave). Nothing is built
  on it.
- **Open**: needed, not built, and no design beyond the port plan.
- **n/a**: not meant to exist in that language.

**How to read the evidence.** Python is the oracle. A Go or Rust row says "Built" only when
the port agrees with the oracle on the committed golden cases
([conformance.md](spec/conformance.md)) with 0 disagreements. The committed cases at HEAD:
1,762 gate, 542 CLI, 158 pathway CLI plus 24 find and 577 verify-message, 215 garden, 243
gateway, 250 MCP (K3). The cases are synthetic. They prove the ports **match** the oracle. They do not prove
a feature is **useful** on real work; that is the dogfood week's job. These counts come from compares run before the Python security fixes of 2026-09-26 (f9e202e7, 92f87edf, 31871c4d, 6a0ad499, 1fad0036). Until the Go and Rust mirrors land, a live `--oracle` compare will show the ports behind on those rules.

## daisugi: checking

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| Envelope, Permission, Invariant models | Built | Built | Built | Ports copy pydantic's validation and messages (`clients/go/internal/pmodel`, `clients/rust/src/pathways/pmodel`). NaN and Infinity are invalid everywhere. |
| Predicate algebra and Z3 compile | Built | Built | Built | Real Z3 5.1.0 linked statically in both ports. Ground checks are decided directly; a test asks the linked Z3 the same formulas. |
| `verify(plan, envelope)`, staged, fail closed | Built | Built | Built | Frozen local corpus (never published): 11,450 of 11,450 for Go `conform` and Rust `conform`. Verify-message cases: 577 plans, 0 disagreements. |
| Shell decomposition (tree-sitter-bash) | Built | Built | Built | The oracle's grammar in all three. Go corpus: 22,178 unique commands, all native, 0 disagreements. |
| Strict mode, vacuity | Built | Built | Built | Vacuity costs about 20 ms in a port (Z3 context start). |
| Shell write paths in the algebra (`forall_writes`) | Built | Built | Built | Built 2026-09-30 (DI-10; rulings DL-1 to DL-3 and DL-11 to DL-13, yellow paper §2.3.1). A quantifier over each step's write paths: a `file_write` path, each literal shell write redirect, and the operand writes of 15 known writers (`cp`, `mv`, `tee`, `sed -i`, `dd of=`, `rm`, …); a line that runs `cd` and writes a relative path is unknown. Dialect cases: 2,322 of 2,322 for Go and Rust `conform`, and `writes.json` (313 commands) in their unit tests. Gate cases scan `&&` chains near the recursion limit: 0 disagreements. |
| Definitions (named predicates, the `alias` field) | Built | Part | Part | Python hardened in 92f87edf (one structural substitution, system words fixed, no swallowed errors). The ports parse the `alias` op and deny it as unresolved: fail closed, no definition registry. |
| Envelope subsumption, skill delegation | Built | Built | Built | Python now checks stakes, custom steps and budgets, and a Z3 timeout is a violation (f9e202e7). Go and Rust: in progress. |
| Effects tiers (silent, undoable, permanent) | Built | Built | Built | Part of the gate cases. |
| Call-time gate (the hook) | Built | Built | Built | Python: the real gate denies a real Read in the real Claude Code CLI (`tests/test_hook_gate_contract.py`). Go `daisugi-gate` and `daisugi gate check`: 1,310 cases, fuzz of 36,000 shell, 6,000 heredoc, 6,000 multi-line, 6,000 non-ASCII and 6,000 envelope calls, 0 disagreements; p50 8.8 ms. Rust: 1,310 cases, 30,370 fuzz calls, 0 disagreements; p50 6.1 ms. Python gate p50 about 430 ms. |
| Crash backstop in the port gates | n/a | Built | Built | Parent and child; a verdict counts only in a nonce-matched frame on fd 3; a crash, a hang or bad output denies. |
| Hard-deny rules (floor config, secrets, `rank record`) | Built | Built | Built | Floor-config and secret-read rules are in both ports. The `rank record` rule is in all three (gate cases `rank rule ...`, 0 disagreements); it covers the owner's verbs `rank record pick` and `rank record drop` and lets `rank queue`, `fit` and `choose` through (RK-R-1). |
| Operator ask and edited input | Built | Part | Part | The ask and `updatedInput` are in all three. Python sends an edited call through the whole gate again (6a0ad499); Go and Rust: in progress. |
| Session binding | Built | Built | Built | A payload's session id never selects an envelope; only the `--session` pin does (31871c4d, SEC-3). The gate cases (`pinned session ignores payload`, `pin with no envelope never reads default`) and the resident `sec3` cases over the socket: 0 disagreements for Go and Rust. |
| Resident gate server (`gate serve`) | Built | Built | Built | What pi and OpenCode ask over `gate.sock`. 200 resident gate cases (requests, hook reports, malformed lines, concurrent and departing clients, start, Ctrl-C and SIGTERM, a stale socket, a second server): 0 disagreements for Go and Rust (G2-1 to G2-9). A socket is a running gate only when it answers; the server removes it when it stops (G2-9). A decision over the socket, p50 allow: Python 6.8 ms, Go 5.9 ms, Rust 4.8 ms (a loaded 4-core box). Go and Rust `start` start it (G2-4). |
| Adversarial corpus and merge gate | Built | n/a | n/a | 13 attacks in 7 categories, 9 benign; a required CI step. Gate denial 1.00, false positives 0.33 (all known). |
| Stage 2 output checks (`verify_completed_step`) | Built | Open | Open | Part of stage K (supervisor). |

## daisugi: the CLI and install

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| `gate init`, `register`, `arm`, `disarm`, `status`, `report`, `settings`, `proposals` | Built | Built | Built | CLI cases, 0 disagreements. |
| `install --gate [--enforce]` | Built | Built | Built | Enforce with no envelope refuses and writes nothing, in all three. A 19-form hook battery finds and removes hand-written hooks the same way in Go and Rust. |
| `install --harness pi\|opencode` | Built | Built | Built | The ports write the Python package's own extension files, which ask the resident gate over its socket; both binaries serve it (`daisugi gate serve`, G2-1). With no server running, the extension denies every call as "unreachable": fail closed. Its message names both ways to start one, `daisugi start` or `daisugi gate serve`; the Rust binary carries only the second, and its `install --harness` line names it. |
| `install --gateway` | Built | Built | Built | Part of the 306 CLI cases both ports carry. |
| `status`, `config`, `start`, `journal` | Built | Built | Built | 508 CLI cases. Go: 490 agree, 9 not ported, 9 refused as ruled (C-10 to C-17, K4-5), 0 disagreements; its `start` opens the plain live view (the 3 `start view` cases agree). Rust: the same 490, 9 and 9 (C-R-1 to C-R-3, K4-R-1); its `start` opens the plain live view too. All three read the journal read-only in `status` and make nothing (K4-8). |
| Journal (store, replay, search) | Built | Built | Built | `journal stats`, `replay` and `search` in both ports (C-14, C-15). |
| Ingest and transcript parsers | Built | Built | Built | Stage F. Go and Rust: `journal parse` and `journal ingest` for Claude Code and Codex, a split through either backend (C-16), and the same parsers under `onboard`. Both answer every parse error Python raises (a file that is not UTF-8, an int past the digit limit, a value of a type the parsers raise on; F-2, F-3) and refuse only a line nested past 900 levels (F-4). The F compare: 199 cases, Go and Rust each agree on 198 and refuse the same 1 (CI `--max-refused 1` for both). The oracle has no parser for pi, OpenCode, Hermes or OpenClaw transcripts (F-1). |
| One-script install, release tarball, AUR files | n/a | Built | Part | `scripts/install.sh`, `scripts/release.sh` (a tarball to the first gate decision in 0.52 s in a scratch HOME; each binary links only glibc; no build paths in any binary). Rust: `release.sh --rust` builds a second tarball with `daisugi` and `daisugi-gate`. No release published; AUR files not published. |

## daisugi: journal, pathways and the garden

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| Pathway store (SQLite) and `pathways` commands | Built | Built | Built | 158 CLI cases, 24 find cases; fuzz 6,000 queries a seed; Python reads what the binary wrote. `pathways list` at 1,000 pathways: Go 22.6 ms, Rust 21.6 ms, Python about 1 s. |
| Matchers: lexical, potion | Built | Built | Built | Potion pinned by sha256; the real tokenizer checked on 3,042 texts. |
| Matchers: MiniLM, int8 | Built | n/a | n/a | Refused in the ports (need torch or onnxruntime). Ruling DS-3: lexical is the default in all three languages; `daisugi tiers setup --matcher potion` opts into potion. |
| Garden: `gardener`, `tend`, `hook auto-tend`, `distill-repeats` | Built | Built | Built | 215 garden cases (203 agree, the same 12 refused in both ports), 0 disagreements. |
| Model client | Built (own client `api`, claude-code) | Built | Built | Ruling DS-2: one own client in all three, the Anthropic Messages API and OpenAI-compatible chat completions (LLM-1 to LLM-15). The backend is named `api`; the old name `litellm` is refused (LLM-13). |
| Distillation-fidelity benchmark | Built | n/a | n/a | Research tool; not ported. No stage 4 numbers meet the bar yet. |
| Deed ledger, batch compilation, strata store | Built | Built | Built | Stage L. Go and Rust: `batch prove`, and the deed ledger, batch runs and the strata store through each port's `l-probe` instrument (L-1). |
| Pathway signing, git registry, release signing | Built | Built | Built | Stage L. Go and Rust: `registry init\|pull\|publish\|status\|pull-and-tend`, `release keygen\|sign\|verify`, bundles and contract signing, keys in the oracle's format (L-4; Rust signs through ring, L-R-1). The registry's git gets no inherited `GIT_*` variable and never climbs above the clone (L-15); `release sign` refuses two artifacts with one name (L-9). The L compare: 239 cases, Go and Rust agree on all, none refused (CI guard `--max-refused 0` for both). |

## daisugi: the forward loop and model-facing parts (stage K)

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| Model-written envelopes, envelope cache | Built | Built | Built | K1. Go: `generate-envelope` and the library `internal/envgen`; Rust: `generate-envelope` and `src/envgen` (Tier-0, Tier-1, the cache and refinement hints, ladders, inheritance). 186 K1 cases for each (183 agree, the same 3 refused as ruled), 0 disagreements. Nothing but a probe reaches the Tier-0, Tier-1 and cache paths until K2 and K3 (K1-1, K1-R-1). |
| Tier-0 reuse, pathway bind and compose | Built | Built | Built | K1. Go and Rust bind a typed pathway's holes with a model in `recall` (K1-7). All three sides refuse an envelope whose self-consistency check did not finish (K1-2); the ports also give the template for a bound plan whose verify did not finish (K1-3). Composition's executors wait for K2. |
| Orchestrator, supervisor, decomposer, model sizer | Built | Built | Built | K2. Go: `run` and `orchestrate` (`internal/supervise`, `internal/orchestrate`); Rust: the same commands (`src/supervise`, `src/orchestrate`, K2-R-1). 116 K2 cases for each (109 agree, the same 7 refused as ruled: YAML the binary does not read, `--max-parallel` above 1, a stale-embeddings warning, an `llm_check` in the envelope), 0 disagreements. A Z3 unknown fails closed (K2-2); the per-step rejection and recompute paths are reached by weave cases (K2-1, retired). |
| `AgenticStep` and `AgenticExecutor` | Built | Open | Open | `daisugi weave` wires it in Python (WV-R-9); `run` and `orchestrate` still fail an agentic step with "no executor" (K2-7). Go and Rust refuse a weave plan with an agentic step (WV-R-10). A live test shows a real sub-agent's out-of-envelope read denied. |
| `daisugi weave`: plan trees with typed slots | Built | Built | Built | [ADR-0022](adr/0022-weave-runner-for-verified-plan-trees.md), rulings WV-R-1 to WV-R-11. A step declares typed outputs; a later step names a slot; the filled step is verified again before it runs; a slot keeps a path's directory and a URL's host and never fills a command or a prompt (WV-1); a model step's slot never fills a file's content (WV-R-2, 2026-09-30); a filled step is verified again per step and as part of the whole plan. Task steps get the router's model; `--resume` skips steps with a succeeded receipt and stops before a started step with no receipt (WV-2, provisional). A task step may run `attempts: N`, ranked by `rank`, with a card and the ask before a permanent step (RK-R-12 to RK-R-14). 108 weave cases for each port: 106 agree, 2 refused as ruled (agentic, `--max-parallel` above 1), 0 disagreements. Before the attempts cases were added, the 93-case weave compare passed 10 runs in a row for each port with no receipt-hash flake (WV-R-11). |
| MCP server, onboarding, `tiers setup` | Built | Built | Built | K3. Go: `mcp serve` (its own JSON-RPC server over stdio, no SDK, DS-6 and K3-1; `internal/mcpwire`), `onboard` and `tiers setup`; Rust: the same commands (`src/mcpwire`, K3-R-1). 214 K3 cases for each: 210 agree, the same 4 refused or not ported (`tiers setup --remote`, `tiers stats`, onboard under a MiniLM config, a config pydantic refuses; K3-13), 0 disagreements. A Z3 unknown fails closed in the MCP tools (K3-5); `recall_answer` refuses a NaN max age on all sides (K3-7); onboard checks the matcher in effect on all sides (K3-10). |
| `modules`, `dashboard`, Prometheus exporter | Built | Built | Built | K4. Go and Rust: `modules`, `dashboard` (one frame, `--once`, `--json`, and on a terminal a frame per interval) and `metrics` (printed, or served at `/metrics`). The K4 compare's 117 non-hook cases: 112 agree in each, 2 not ported (the Textual views, K4-3), 3 refused (K4-4), 0 disagreements. The map answers as a base install with the binary's extras (K4-1). |
| `hook record` (capture and floor-report hooks) | Built | Built | Built | The hook `install` writes for Claude Code's capture, Stop, Notification and subagent events, Hermes and OpenClaw. 76 hook cases in the K4 fixtures, with a listening fake coppice and a fake herdr: all agree in Go and Rust. Past the options it always exits 0, so a Stop hook never blocks the stop (HR-1). Go and Rust also carry `hook list` and `hook to-trace` (stage F, F-6, F-7); both refuse the command-line `hook report` (HR-4). |
| Robotics executors, LoRA training | Built (experimental) | n/a | n/a | Stay in Python by design. The robotics checks in the verifier are ported. |
| Voice bridge (`daisugi voice serve`) | Built (experimental) | Open | Open | Stage H. coppice starts it when the voice extra is installed. Real microphone and phone use not verified. |

## daisugi: token saving

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| Gateway (Anthropic and OpenAI wires, buffered and streamed) | Built | Built | Built | 243 gateway cases (241 agree, 2 refused as ruled), pipeline fuzz of 12,000 turns, 0 disagreements. Each turn records its time, `elapsed_ms` (RP-11, 2026-09-30). Added time at p50: Go 0.88 ms, Rust 0.22 ms. |
| Rules router, cache-aware sticky routing (ADR-0015) | Built | Built | Built | As above. The router is a fixed rule and does not learn. |
| Switchyard as a managed child | Built | Built | Built | SIGTERM stops the child and removes its state file in all three. |
| Proxies for every HTTP client | Built | Built | Built | 54 fake-proxy cases (PX-1 to PX-8). |
| `gateway-report`, `router status\|stop`, `route` | Built | Built | Built | `router status` shows the weekly measure and the delegate in force (RP-12): 33 cases, 0 disagreements. |
| Router part 0: large reads go to the `delegate` MCP tool | Built | Built | Built | 2026-09-30. A `deny_redirect` rule file in the gate root (RP-1 to RP-5), the gate's check of the delegate call (RP-7), the tool, bulk read only, with a local worker by default and quotes checked against the file (RP-6, RP-8, RP-9). 131 gate cases and 36 MCP cases with a fake worker, 0 disagreements. Only the claude format redirects (RP-2). Code-write (RT-1) is not built. |
| Router part 4: measure | Part | Part | Part | 2026-09-30. The delegation journal, each turn's time, and `router status` by week (RP-10 to RP-12). No escalation to count (part 1), no chooser to replace (part 3), and task outcomes unknown until a label source exists. |
| Router parts 1 to 3 (escalate, plan then delegate steps, learn) | Designed | Designed | Designed | [design-router.md](plans/2026-09-24-omarchy/design-router.md). Python first, then the ports. |
| Ranking (`daisugi rank`) and the review queue | Built | Built | Built | [design-ranking.md](plans/2026-09-24-omarchy/design-ranking.md) (RK-10), rulings RK-R-1 to RK-R-15. `rank fit`: elimination, quality tiers, Bradley-Terry with a weak prior over the judges' votes that hold in both orders, a seeded splitmix64 bootstrap, the owner's answers as order constraints, cost last (RK-1). `rank choose` records the choice in `journal/rankings/choices.jsonl`; `rank queue` lists the open cards (switch cost from the ledger, decay at follow-up-only or 7 days, read-only); `rank record pick` and `rank record drop` are the owner's answers. weave runs a task step's attempts through it. 135 rank cases, 0 disagreements in Go and Rust. Not built: `rank judge` (the model judge), `rank show`, the SQLite index, the coppice cards view and its `answer` post-back, the pivot (a pick prints what switching would do), judge trust. |
| Grafts (the rewrite verdict) | Part | Part | Part | [design-grafts.md](plans/2026-09-24-omarchy/design-grafts.md). Shape (c), deny and redirect, is built as router part 0 (GR-1); the rule store is built (GR-5). The installer, `daisugi graft install\|status\|remove`, refuses beside a rival PreToolUse hook (GR-3, GR-R-2). An audit-mode gate only records a graft (RP-4). Not built: the proof at install (no shape needs one yet), the post-call input check, shapes (a) and (b), the learning loop. |

## daisugi: designs not yet built

| Feature | Python | Go | Rust | Evidence and notes |
|---|---|---|---|---|
| Dialects | Part | Part | Part | [design-dialects.md](plans/2026-09-24-omarchy/design-dialects.md). Stage 0 is built in all three (2026-09-30, rulings DL-4 to DL-17): the system dialect's `keep_unchanged(target)` and 23 synonyms (`file_unchanged`, `read_only`, …), audited until `dialect_enforce` pins the dialect hash; at the gate a word is placed from the call's cwd (DL-13); a hard-deny rule covers daisugi's own config.yaml (DL-15); `gate report`, `gate status` and `daisugi status` show the would-deny counts (plans and calls, DL-16). 1,631 gate cases and 542 CLI cases, 0 disagreements in Go and Rust. Permission words, population dialects, envelope pins and mining are not built. First compression test: 17.5% with 15 words, in-sample. |
| Delegation tree, each edge proved | Proposed | Proposed | Proposed | [design-delegation-tree.md](plans/2026-09-24-omarchy/design-delegation-tree.md). Ruling AT-1 lets the daisugi and coppice parts go ahead. |
| weave: skill expansion, `gate` and `on-fail` words, dialect output, a plan view in coppice | Proposed | Proposed | Proposed | [ADR-0022](adr/0022-weave-runner-for-verified-plan-trees.md). The runner itself is built (above). |

## coppice, the floor

coppice is Go. A Rust coppice is port stage I (Open). The Python floor backends (tmux, Herdr)
are not ported; coppice replaces them.

| Feature | Go | Evidence and notes |
|---|---|---|
| Server, client and socket protocol | Built | One binary; the socket is 0600 in a 0700 directory and checks the peer uid. Protocol corpus in `harness/coppice/testdata/protocol`, replayed against a real server. Linux only. |
| State from the gate first, the screen second | Built | The gate hook reports through `COPPICE_SOCK` and `COPPICE_PANE`. With no report, the floor says `unknown`, never a guessed idle. |
| The floor in a terminal (rail, windows, stack bar, gate mark, Recent, projects) | Built | Go tests across 26 packages. Hand checks in [RESUME.md](plans/2026-09-24-omarchy/RESUME.md) not yet all run. |
| The web floor and the phone (`coppice web`) | Built | 452 web tests. A Playwright smoke run drives real Chromium behind `COPPICE_SMOKE=1`; not in CI. Not yet used on a phone for a week. |
| Ready prompts, trust dialog | Built | Found by a live run under Claude 2.1.282; fixed and tested. |
| Voice with no setup | Built | coppice starts `daisugi voice serve`, which needs Python's voice extra. |
| The foreman | Built | A foreman agent that starts, directs and stops agents across projects. |
| Adapters: Claude Code | Built | Live runs on an isolated rig with the Go daisugi hook. |
| Adapters: Codex, sprig | Part | The README says neither has run against a real binary. The ledger records a live sprig pane on the rig under the Go daisugi hook on 2026-09-25. The two sources disagree; the README may be stale. |
| `coppice attach --remote` | Open | Attach dials a local socket only. |

## sprig, the loop (on hold)

The owner put sprig, grove and weave on hold on 2026-09-26, and retired grove on 2026-09-28. Everything new about them is
**Proposed**. A Rust sprig is port stage J (Open).

| Feature | Go | Evidence and notes |
|---|---|---|
| sprig, the minimal loop | Built (MVP) | `harness/sprig`. With `--gate`, it calls `daisugi gate check --mode enforce` and denies when `daisugi` is missing. **Without `--gate` the gate is off** (`AllowAll`, `cli.go`). The owner's answer: gate on by default once it runs smoothly; not done. |
| sprig-hook, sprig-mcp | Built (MVP) | sprig-mcp also defaults to `AllowAll`. |
| grove | Retired | Deleted 2026-09-28. coppice replaced it. |
| weave (sprig's) | Built (MVP) | 131 lines: a JSON graph run one step at a time, stop at the first failure. Defaults to `AllowAll`. The verified-plan runner of ADR-0022 is `daisugi weave`, not this one. |
| Tokens from the Claude backend | Built | Uses `--output-format json` and maps usage. |

## Other verifier clients

| Client | Status | Notes |
|---|---|---|
| TypeScript (`clients/ts`) | Built | A verifier conformance client. |
| Lean 4 (`clients/lean`) | Built (Core profile) | No solver, a hand-written parser; its soundness claims are Lean theorems. It stays the proof of the verifier core; there is no third full stack. |

## Rule of thumb

If what you need is **Designed**, **Proposed** or **Open**, it does not exist yet. If it is
**Part**, read the note. If it is **Built** in a port, the port matches the oracle on every
committed case; it has not been used for long on real work.

# Decisions pending: provisional rulings, 2026-09-26

On 2026-09-26 the owner ruled: adopt the recommended answer to every open question in the
designs as a **provisional ruling**, record it for later review, and build only on the rulings
that are easy to undo. This file collects those rulings.

## What "provisional" means

- A provisional ruling is **in force now**. Work may follow it without asking again.
- It is **reversible**. The owner reviews each one later and confirms it or overturns it.
- Build only on rulings marked **easy** to undo. A ruling marked **hard** (a data format, the
  envelope schema, the predicate algebra, the injection boundary) waits for the owner's
  confirmation before code depends on it.
- A ruling that touches **sprig, weave or grove** stays **Proposed**. It is recorded here, but
  nothing is built on it until the owner resumes that part. Those rows say "Proposed" and
  "Blocks building: yes".
- The recommendation text is a summary. The source document holds the options and the reasons.

Columns: **Reversible?** is how hard it is to undo the ruling after code depends on it.
**Blocks building?** is "yes" when work must wait for the owner (a hard ruling, or a part on
hold); "no" when work may start on the provisional ruling now. **Retires when** (added
2026-09-27) names the event that ends the ruling as a provisional ruling. For most rows that
is the owner's review (confirm or overturn), or the build it waits for, whichever comes
first; the cell says which. A retired row stays in place, marked "Retired" with the date and
the cause, so the history reads true. The pattern comes from the `decisions.md` ledger of
Imp (https://github.com/deepfates/imp/blob/main/decisions.md), whose rulings each carry a
"Status" and a "Retires when".

## ADR-0022, the weave runner (`docs/adr/0022-weave-runner-for-verified-plan-trees.md`)

The whole ADR is about weave, which was on hold on 2026-09-26, so every row was **Proposed**.
On 2026-09-28 the owner confirmed WV-1 and WV-5. WV-2 to WV-4 stay Proposed; ADR-0022 says
they stay open.

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| WV-1 | Data flow between steps | (b) typed data slots only, filled like a bind and verified again, never a capability. Free-text flow is refused because a poisoned output could steer a later model. The K1 bind rule applies: a slot carries data only, never a program, a directory or a host. | hard (moves the injection boundary) | no | Retired 2026-09-28: owner confirmed (typed slots, re-verified). |
| WV-2 | Resume | All three, in order: skip steps with a succeeded receipt for the same plan hash; re-run read-only steps; stop and ask a human for an irreversible step with no receipt. Proposed. | easy | yes (weave on hold) | weave resumes and the owner confirms or overturns it. Built as proposed 2026-09-30 (WV-R-7): a step that started with no receipt stops the run unless `--rerun` names it; a task step runs again. |
| WV-3 | Where the plan comes from | All three sources (decomposer, stored pathway, a file a model writes), one format: `ActionPlan` JSON. Proposed. | easy | yes (weave on hold) | weave resumes and the owner confirms or overturns it. Built 2026-09-30: `daisugi weave` reads a plan file as `ActionPlan` JSON (WV-R-1). |
| WV-4 | Agentic executor | Keep `claude -p` (as `AgenticExecutor` does now) first; add sprig when sprig resumes. Proposed. | easy | yes (weave on hold) | weave resumes and the owner confirms or overturns it. The sprig half waits for sprig to resume too. Built in Python 2026-09-30 (WV-R-9); Go and Rust refuse an agentic step (WV-R-10). |
| WV-5 | Option A, B or C | B: the runner is a `daisugi weave` command on the existing Supervisor, in Python, Go and Rust; coppice shows the run and answers asks, and does not run it. | easy | no | Retired 2026-09-28: owner confirmed option B. ADR-0022 is accepted in part. |

## Router (`design-router.md`)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| RT-1 | How code-write writes | Owner: (b), a draft the frontier reviews and applies with its own gated Write. | easy | no | Retired 2026-10-01: built as a draft (SW-6 to SW-9); the tool writes nothing. |
| RT-2 | The delegate tool's name | `delegate`, a bare name like `recall` and `run_plan` on the `opendaisugi` server. | easy (unreleased) | no | The owner confirms or overturns it, or the `delegate` tool ships in a release (then the name is fixed). |
| RT-3 | How an outside runner reaches the router | `daisugi route --json`, widened to take a step. It exists in Python, Go and Rust. The only outside runner named is weave, so this is Proposed. | easy | yes (weave on hold) | weave resumes and the owner rules on how it reaches the router. Moot for weave 2026-09-30: weave is a daisugi command and calls the route rule in process (WV-R-5). |

## A tree of agents (`design-delegation-tree.md`)

The design is **Proposed** as a whole. Its recommendation says steps 1 to 4 do not need sprig.

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| AT-1 | Is the tree rule wanted before sprig resumes? | Yes for the daisugi and coppice parts: steps 1 to 3 now (`edge_ok`, prove at registration, fix the one-level paths); step 4 (coppice Option 3) after the session-binding adversarial test. The sprig part (step 5) stays Proposed. | easy (additive, fail closed) | no for daisugi and coppice; sprig part: yes | Confirmed by the owner 2026-09-30. Steps 1 to 3 built 2026-09-30 (TR-R-1 to TR-R-12); step 4 (coppice) waits for the session-binding adversarial test; the sprig part waits for sprig to resume. |
| AT-2 | Budget location | B, a tree ledger, for tokens and turns. A, a field in the envelope, for the deadline only, proved per edge. | hard for the envelope deadline field (envelope schema in three clients); the ledger is easy | no | Retired 2026-09-28: owner confirmed (deadline in the envelope, budgets in the ledger). |
| AT-3 | Multi-hop holds | A: nearest foreman, then the operator (today's rule). | easy | no | Confirmed by the owner 2026-09-30. |
| AT-4 | Strict at every edge, whatever the stakes | Yes. Unsupported globs and opaque invariants are refused; the parent model rewrites them. | easy (loosening later is simple) | no | Retired 2026-09-30: owner confirmed; `edge_ok` is built strict (TR-R-2). |
| AT-5 | Refuse LLM-driven children under `stakes='physical'` | Keep the refusal. | easy (status quo) | no | Retired 2026-09-30: owner confirmed; `tree spawn` refuses under a physical parent (TR-R-6). |
| AT-6 | Failed-proof loop | Three narrower proposals from the parent, then the ask goes to a human. | easy | no | Retired 2026-09-30: owner confirmed; built (TR-R-7). |
| AT-7 | sprig fleet's operator override | Fold it into coppice's allow rules when sprig resumes. Proposed. | easy | yes (sprig on hold) | Confirmed by the owner 2026-09-30; built when sprig resumes. |

## Ranking (`design-ranking.md`)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| RK-1 | Where cost goes in the order | (c): quality measures, then preferences, then cost as the last tie-break. `quality_tiers` stays a separate output with no cost in it. | easy | no | The owner confirms or overturns it, or `daisugi rank` is built in Python with this order. |
| RK-2 | Weight of the owner's pick | (b): a Bradley-Terry vote worth k = 10 judge votes, plus a per-pair veto; owner votes held fixed in the bootstrap. | easy (a parameter) | no | Retired 2026-09-28 by the owner's ranking ruling (RK-10): the owner answers cards after the fact, and an answer is a constraint on the pairs it covers, not a weighted vote. |
| RK-3 | Stop threshold and budgets | Fixed: 0.9; 6 judge comparisons; 3 owner questions. Tune from the journal later. | easy | no | Retired 2026-09-28 by the owner's ranking ruling (RK-10): no per-task question limits. 0.9 stays only as the label threshold; judges are bound by one vote per judge per pair. |
| RK-4 | Daily cap on owner questions | Yes, default 10, across all rankings. The rest stay `undecided`. The owner can raise it. | easy | no | Retired 2026-09-28 by the owner's ranking ruling (RK-10): no daily caps. |
| RK-5 | Judge weight from trust | Fixed weight 1 with a drop floor first. A learned weight waits for enough calibration answers. | easy | no | Enough calibration answers exist to learn a weight (then a new ruling), or the owner rules. |
| RK-6 | Where comparisons are stored | (b): JSONL as the source, plus an index table. | easy (plain append-only text; the index rebuilds from it) | no | The owner confirms or overturns it, or the JSONL store and its index are built. |
| RK-7 | Where it lives | A daisugi command beside `verify`, with the coppice arena as a view over it. sprig use stays Proposed. | easy (unreleased) | no for daisugi and coppice; sprig use: yes | The owner confirms or overturns it, or `daisugi rank` is built. The sprig use waits for sprig to resume. |
| RK-8 | Operator override: eliminate an attempt let out of its envelope, or keep it | Keep it, with a warning. | easy | no | The owner confirms or overturns it, or the override is built. |
| RK-9 | Calibration questions | 1 in 10 owner questions. | easy | no | Retired 2026-09-28 by the owner's ranking ruling (RK-10): every answered card is a calibration point. |
| RK-10 | How a choice reaches the owner (owner ruling 2026-09-28, replaces RK-2, RK-3, RK-4, RK-9) | Never block work on a choice unless the next step is irreversible (the gate's `permanent` tier, `unknown` included; that step uses the existing permanent-tier ask). The runner proceeds on the top-ranked option; alternatives are logged. Each choice with two or more survivors opens a card in an asynchronous review queue, readable fully out of context, at home on the phone: one line of what was chosen and what else was on the table; why in plain words; what switching would do; drill-down to diffs, previews, test output and reasoning. Sort by reversibility, impact or complexity, date, or project. Drop = the recommendation was kept. Cards decay: an unreviewed card closes as kept (recorded `ignored`, not an owner label) once switching needs a follow-up task, or after a set time, whichever is first (starting value 7 days, tuned from the journal; see the Open item in the design). A later pick pivots: undo through receipts, checkpoints and worktrees and apply the alternative, or start a follow-up task; the card says honestly when switching is costly. No daily caps, no per-task question limits. No new database: cards are views over the journal's append-only JSONL records; one small choice row (options, chosen, confirmed / overridden / ignored) stays for the router and the garden; bulky alternatives live in worktrees and go with normal cleanup. | the policy is easy; the choice record is a data format (append-only JSONL, so a later field is additive) | no | Not provisional: an owner ruling. Its starting values (decay time) retire when tuned from the journal. |

## Grafts (`design-grafts.md`)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| GR-1 | Which shape first | (c) deny and redirect, the shunt equivalent. This is router part 0. | easy | no | Retired 2026-09-30: built (router part 0, rulings RP-1 to RP-9). |
| GR-2 | The worker | Router-chosen, local by default. A remote worker only with an envelope grant, at the permanent effect tier. | easy | no | Retired 2026-09-28: owner confirmed (local default, remote only by grant). |
| GR-3 | Rival rewriting hooks | Refuse to install grafts beside an unknown PreToolUse hook, with a warning naming it. Add the post-call input check on Claude Code as detection. The gate itself still installs. | easy | no | The owner confirms or overturns it, or the rival-hook refusal and the post-call check are built. Partly built 2026-09-30: the refusal is `daisugi graft install` (GR-R-2); the post-call check is not built. |
| GR-4 | The delegate's form | `daisugi-delegate` executable for shapes (a) and (b); the `delegate` MCP tool for shape (c); both call one library. | easy (unreleased) | no | The owner confirms or overturns it, or the delegate library and its two front ends are built. The library (`delegate.py`) and the MCP tool are built (2026-09-30); `daisugi-delegate` waits for shape (a). |
| GR-5 | Where rules live | The gate root beside envelopes, one file per rule. | easy (rules are data the bots write) | no | Retired 2026-09-30: built, `<gate root>/grafts/*.json` (RP-1). |
| GR-6 | What promotion measures | Billed cost per successful task, with no drop in success; quota tokens shown beside it. | easy | no | Retired 2026-10-01: the promotion meter is built (SW-14, SW-15). |
| GR-7 | Trial split | Per session. | easy | no | Retired 2026-10-01: the per-session split is built (SW-13). |
| GR-8 | Fix the operator-edit gap (edited input skips the second `_decide`) now or with stage 2 | Now, on the security queue. It is a fail-closed fix and does not wait for grafts. | easy (it tightens) | no | Retired 2026-10-01: the fix is in Python, Go (`recheckEdit`) and Rust (`recheck_edit`), with the `operator edit` gate cases (SW-5). |

## Dialects (`design-dialects.md`)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| DI-1 | Enforce the opaque invariants the models already write? | Audit first (Stage 3b), then enforce. | easy | no | Retired 2026-09-28: owner confirmed (audit first, then enforce). |
| DI-2 | Shadowing across tiers | Forbid it. Identity by hash makes it unneeded. | easy (loosening later is simple) | no | The owner confirms or overturns it, or the tier resolver is built without shadowing. |
| DI-3 | Permission words at all? | Yes, with the over-grant test. A widening redefinition needs the owner. | easy | no | The owner confirms or overturns it, or permission words ship with the over-grant test. |
| DI-4 | Who promotes a word to the population dialect | Automatic for predicate words (they only narrow); owner approval for permission words (they grant); a veto in coppice. | easy | no | The owner confirms or overturns it, or promotion is built. |
| DI-5 | Name checks | A blind second model writes claims about each name (section 5.3). | easy | no | The owner confirms or overturns it, or the name check is built. |
| DI-6 | Stitch as a tool | The Stitch CLI built from source, offline only, never a runtime dependency. Revisit babble if canonical forms miss too many equal terms. | easy | no | The owner confirms or overturns it. Revisit when canonical forms miss too many equal terms (then babble). |
| DI-7 | Kernel growth for robots | Wait for a robot user. Name the three kernel gaps in the scorecard now. | easy | no | A robot user appears; then the kernel gaps get their own ruling. |
| DI-8 | Plan fragments | Pathways hold actions, dialects hold policy. The weave part stays Proposed. | easy | no for pathways and dialects; weave part: yes | The owner confirms or overturns it. The weave part waits for weave to resume. |
| DI-9 | Accept an envelope that uses words but pins no dialect? | No. The gate denies it, as it denies an unresolved definition today. | easy (fail closed) | no | The owner confirms or overturns it, or the gate denies an unpinned envelope in all three clients. |
| DI-10 | Expose shell write paths to the algebra? | Yes: add a field with each shell step's decomposed write paths, and a quantifier over it, in all three clients, as a Stage 3 prerequisite. It unblocks dialect stage 0. | hard (changes the predicate algebra, the yellow paper and all three clients) | no | Retired 2026-09-28: owner confirmed (yes). |

## Strategy notes (`strategy-2026-09-26.md`, items marked Open)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| ST-1 | Is "models write the vocabulary" worth a design, after the dogfood week? | Yes. `design-dialects.md` is that design; its rulings are DI-1 to DI-10. | easy | no | Retired 2026-09-26: `design-dialects.md` is written; its rulings live on as DI-1 to DI-10. |
| ST-2 | A tree of agents, each edge proved | As AT-1: the daisugi and coppice parts proceed; the sprig part stays Proposed. | easy | no for daisugi and coppice; sprig part: yes | Retires with AT-1. |
| ST-3 | Write the grafts design after a fresh research pass | Done: `design-grafts.md`, with `docs/research/grafts-inputs-2026-09-26.md`. Rulings GR-1 to GR-8. | easy | no | Retired 2026-09-26: `design-grafts.md` is written; its rulings live on as GR-1 to GR-8. |
| ST-4 | Plans as trees: a runner outside Python | ADR-0022 Option B (WV-5). weave is on hold, so this is Proposed. | easy | no | Retired 2026-09-28 with WV-5 (owner confirmed option B). |
| ST-5 | Pairwise "which is better" screen: a default plugin in coppice or sprig? | The coppice arena view over the daisugi rank command (RK-7). No full diff viewer. sprig use stays Proposed. | easy | no for coppice; sprig use: yes | Retires with RK-7. |
| ST-6 | sprig's own edge over pi plus the extension | No recommendation given. Candidates in the notes: verified plan trees, grafts in its tools, the router in the loop, human ranking of attempts. Stays Open with the owner. | n/a | yes (sprig on hold) | sprig resumes and the owner answers it. |

## Owner answers already given (2026-09-26)

These are the owner's own answers, not provisional rulings.

- **Gate default-on** in sprig, grove and weave, once it runs smoothly. (sprig, grove and weave
  stay on hold; this sets the default for when they resume.) Note 2026-09-28: grove is retired
  (see below), so this applies to sprig and weave.
- **Live Claude runs** are allowed, but token-mindful: at most about 10 short sessions a day,
  with the cheapest model that tests the thing.
- **CI** runs on standard GitHub runners, with a cached Z3 build.
- **Full parity:** the Python oracle first, then Go and Rust.
- **The bots write the policy.** No human learns a DSL.
- **Release tags later.**

## Owner rulings, 2026-09-28

These are the owner's own rulings, not provisional rulings.

- **Renames.** Shadow mode becomes **audit mode**, including the flag: `--mode shadow` becomes
  `--mode audit`, a clean break in all three clients and coppice. Agent tree becomes
  **delegation tree**. Alias becomes **definition** in prose; the field name stays. Kept as they
  are: kernel, the effect tiers, distillation. Done 2026-09-30 in Python, Go, Rust, coppice and
  the live docs (rulings AU-1 to AU-7 in `clients/ADJUDICATIONS.md`).
- **grove is retired.** `cmd/grove` and `sprig/fleet` are deleted (done 2026-09-28); the docs say
  coppice replaced it. Rows above that say "sprig, weave or grove" now mean sprig and weave.
- **Ranking.** RK-10 above: never block on a choice unless the next step is irreversible; an
  asynchronous review queue of cards. It retires RK-2, RK-3, RK-4 and RK-9.
- **Confirmed:** DI-1, DI-10, WV-1, WV-5, AT-2, GR-2. **Changed:** DS-3 (lexical default).
- **Delegation tree (2026-09-30).** The owner adopted every recommendation of the design's open
  questions: AT-1, AT-3, AT-4, AT-5, AT-6 and AT-7 are confirmed. Steps 1 to 3 are built
  (rulings TR-R-1 to TR-R-12 in `clients/ADJUDICATIONS.md`).

## Dependency sweep (2026-09-26)

| ID | Question | Provisional ruling | Reversible? | Blocks building? | Retires when |
|---|---|---|---|---|---|
| DS-1 | Commit `uv.lock` or pin CI with a constraints file | Owner delegated; ruled: commit `uv.lock`, CI runs `uv sync --locked` (the old "never commit uv.lock" rule is retired) | easy | no | Retired 2026-09-26 into a standing rule: `uv.lock` is committed and CI runs `uv sync --locked` (`.github/workflows/ci.yml`, `.github/workflows/clients.yml`). The standing rule does not retire. |
| DS-2 | Replace litellm and instructor with our own model-call module (Python, Go, Rust; add an OpenAI-compatible path for Ollama and llamafile) | Yes, as the one port job after the security queue | hard | no | Retired 2026-09-27: built. `src/opendaisugi/llm_client.py`, `clients/go/internal/llm`, `clients/rust/src/llm`; rulings LLM-1 to LLM-15 in `clients/ADJUDICATIONS.md`. |
| DS-3 | Default matcher | Changed by the owner 2026-09-28: lexical is the default (no model, no download); potion is opt-in through `daisugi setup`. `[search]` stays out of dev and CI. (The earlier provisional ruling was potion with a pinned download, falling back to lexical.) | easy | no | The default matcher is lexical and potion is offered by setup. `[search]` is already out of dev and CI (`pyproject.toml`); Retired 2026-09-30: the default is lexical in Python, Go and Rust, and `daisugi tiers setup --matcher potion` sets potion in one step. |
| DS-4 | Own the embedders in Python (port potion loader and tokenizer back, WordPiece for int8, no huggingface_hub) | Yes, after DS-2 | easy | no | The embedders are our own in Python and huggingface_hub is gone. |
| DS-5 | Declare `click` and the other undeclared imports | Yes, now (a real bug) | easy | no | Every undeclared import is declared. `click` is declared (`pyproject.toml`); the other imports are not checked yet. |
| DS-6 | Drop `textual-serve` and `av`; replace the `mcp` SDK with our own server; replace networkx | Yes, in that order, each as a small job | easy | no | Each of the four replacements lands. |
| DS-7 | `GOTOOLCHAIN=auto` in toolchain.sh | Limit it to the build scripts | easy | no | `GOTOOLCHAIN=auto` is limited to the build scripts. |
| DS-8 | Align tree-sitter and SQLite versions across the ports | Yes | easy | no | The tree-sitter and SQLite versions match across the ports. |

## Owner confirmations, 2026-09-28 (the walk-through)

- **GR-1:** confirmed. Deny-and-redirect for large reads is built first (router part 0).
- **RT-1:** changed by the owner to option (b): code-write returns a draft, and the frontier model
  reviews and applies it. The worker never writes the file itself.
- **GR-3:** confirmed. Grafts refuse to install beside an unknown input-rewriting hook, with a warning
  that names it; the gate itself still installs.
- **RK-10 decay default:** a card closes as "ignored" at the first of: switching needs a follow-up
  task, or 7 days. Provisional; tune from the journal.
- **Bulk:** every other row above that is not retired and not on hold is confirmed as written. The
  rows still waiting are WV-2, WV-3, WV-4 (weave details, to settle when `daisugi weave` is built),
  RT-3 (how a runner reaches the router, now that weave is a daisugi command), AT-7 (sprig's old
  override, moot with grove retired) and ST-6 (answered by `design-sprig.md`, Proposed).

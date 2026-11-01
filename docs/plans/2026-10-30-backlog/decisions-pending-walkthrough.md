# Owner walkthrough: every ruling that still waits for your word (2026-10-30)

> **Answered 2026-10-30.** Every item below (A1 to A22, B1 to B15, C1 to C10, D1 to D5) is ruled. The owner answered in a decision dialog; the controller decided B13, B14, B15, C2 to C6 and C9 as written, because the owner said those were too in-the-weeds. The rulings are recorded in the backlog ledger (`.superpowers/sdd/backlog/progress.md`, section "Owner rulings 2026-10-30"), in the plans of this directory (each changed item names its ruling), and in the "Retires when" cells of `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md`. Changed: A1, A3, A7, A14, A15, B2, B10, C7, C8, D3, D4, D5. Every other item is confirmed as written. The items below are left as they were asked.

How to answer: reply with the item number and one of `confirm` or `change: <what you want instead>`. You can answer a whole group at once: `A: confirm all`, `B: confirm all except B2`. A row you do not answer stays provisional and keeps its current status (work on it continues where the row says it may).

Each item has three lines: what was decided, what it costs if it is wrong, and the reply line. Sources: `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (group A), the backlog ledger and the W3 plans in this directory (groups B, C, D).

## Group A: rows in DECISIONS-PENDING.md that are not retired

You confirmed most of these in bulk on 2026-09-28 ("every other row ... is confirmed as written"). The rows stayed in the table because their "Retires when" event has not happened. This is the last look before each row retires.

**A1. RT-2, the delegate tool's name.** The tool on the `opendaisugi` MCP server is called `delegate`, a bare name like `recall` and `run_plan`.
Cost if wrong: one rename before the first release; after a release the name is fixed in every user's config.
Reply: `A1 confirm` / `A1 change: ...`

**A2. RK-1, where cost sits in the ranking order.** Quality measures first, then your preferences, then cost as the last tie-break; `quality_tiers` carries no cost.
Cost if wrong: a sort key; one line in three ports.
Reply: `A2 confirm` / `A2 change: ...`

**A3. RK-5, judge weight.** Every judge has weight 1 with a drop floor; a learned weight waits until enough calibration answers exist.
Cost if wrong: a parameter; rankings already made do not change.
Reply: `A3 confirm` / `A3 change: ...`

**A4. RK-6, where comparisons are stored.** Append-only JSONL is the source, with an index table rebuilt from it.
Cost if wrong: a data format, but plain text that re-reads; a later field is additive.
Reply: `A4 confirm` / `A4 change: ...`

**A5. RK-7, where ranking lives.** A `daisugi rank` command beside `verify`, with the coppice arena as a view over it; sprig use stays Proposed.
Cost if wrong: the command moves; nothing stored changes.
Reply: `A5 confirm` / `A5 change: ...`

**A6. RK-8, an attempt let out of its envelope.** The operator override keeps it in the ranking, with a warning, instead of eliminating it.
Cost if wrong: one `if` and a warning line.
Reply: `A6 confirm` / `A6 change: ...`

**A7. RK-10 starting value, card decay.** An unreviewed card closes as "kept" at the first of: switching needs a follow-up task, or 7 days.
Cost if wrong: a number; you can tune it from the journal.
Reply: `A7 confirm` / `A7 change: <days>`

**A8. GR-3, second half, the post-call check.** On Claude Code a post-tool hook records when the input that ran differs from the one the warden verified (detection of a rival rewriting hook). The first half (refuse to install grafts beside an unknown hook) is built.
Cost if wrong: a hook that writes one audit line; removable in one task.
Reply: `A8 confirm` / `A8 change: ...`

**A9. GR-4, the delegate's form.** `daisugi-delegate` is its own executable (so an envelope can allow that shell head alone); the `delegate` MCP tool is the other front end; both call one library.
Cost if wrong: one executable name; the library stays.
Reply: `A9 confirm` / `A9 change: ...`

**A10. DI-2, shadowing across dialect tiers.** Forbidden; a word's identity is its hash.
Cost if wrong: loosening later is one task; tightening later would break stored envelopes.
Reply: `A10 confirm` / `A10 change: ...`

**A11. DI-3, permission words at all.** Yes, with the over-grant test; a widening redefinition needs you.
Cost if wrong: a permission word that grants more than intended is a fail-open; the over-grant test is the guard.
Reply: `A11 confirm` / `A11 change: ...`

**A12. DI-4, who promotes a word to the population dialect.** Automatic for predicate words (they only narrow); your approval for permission words (they grant); a veto in coppice.
Cost if wrong: an approval step added or removed.
Reply: `A12 confirm` / `A12 change: ...`

**A13. DI-5, name checks.** A blind second model writes claims about each new word's name.
Cost if wrong: one model call per new word, removable.
Reply: `A13 confirm` / `A13 change: ...`

**A14. DI-6, Stitch as a tool.** The Stitch CLI built from source, offline only, never a runtime dependency.
Cost if wrong: a build-time tool to drop.
Reply: `A14 confirm` / `A14 change: ...`

**A15. DI-7, kernel growth for robots.** Wait for a robot user; name the three kernel gaps in the scorecard now.
Cost if wrong: time; nothing built.
Reply: `A15 confirm` / `A15 change: ...`

**A16. DI-8, plan fragments.** Pathways hold actions, dialects hold policy; the weave part stays Proposed.
Cost if wrong: a boundary moves between two stores; data migrates once.
Reply: `A16 confirm` / `A16 change: ...`

**A17. DI-9, an envelope that uses words but pins no dialect.** The warden denies it.
Cost if wrong: fail closed; a user adds one `dialect` line.
Reply: `A17 confirm` / `A17 change: ...`

**A18. DS-4, own the embedders in Python.** Port the potion loader and a WordPiece tokenizer back from Go; drop `model2vec`, `tokenizers` and `huggingface_hub` from `[potion]` and `[int8]`. Planned as Task 5 of `w3-deps-and-ci.md`.
Cost if wrong: about 600 lines of our own code to maintain instead of three packages.
Reply: `A18 confirm` / `A18 change: ...`

**A19. DS-5, declare every undeclared import.** `click` is declared; the rest is a check that fails on any import under `src/` that no declared package covers. Task 2 of `w3-deps-and-ci.md`.
Cost if wrong: none; it only finds real bugs.
Reply: `A19 confirm` / `A19 change: ...`

**A20. DS-6, four removals.** `av`: done. `networkx`: done. Left: drop `textual-serve` (`daisugi dashboard --serve` goes; coppice web is the browser surface) and replace the `mcp` SDK with our own stdio JSON-RPC server (the 260 recorded sessions are the contract). Tasks 6 and 7 of `w3-deps-and-ci.md`.
Cost if wrong: `--serve` is a feature you may use; say so and it stays. The mcp replacement is the largest task of the wave (about one session); if the sessions re-record unchanged the risk is low.
Reply: `A20 confirm` / `A20 change: keep --serve` / `A20 change: keep the mcp SDK`

**A21. DS-7, `GOTOOLCHAIN=auto`.** Limited to the build scripts; never a global Go setting. The 2026-10-09 change to `toolchain.sh` did this; Task 8 verifies and retires the row.
Cost if wrong: none.
Reply: `A21 confirm`

**A22. DS-8, one tree-sitter and one SQLite across the ports.** Rust and TypeScript move to tree-sitter 0.26; rusqlite moves to the release that bundles the SQLite go-sqlite3 bundles. Task 8.
Cost if wrong: a crate bump to undo; Z3 is not touched.
Reply: `A22 confirm` / `A22 change: ...`

## Group B: new items the backlog ledger marked "owner to rule"

**B1. Pando R2 parity, option B.** Go is the oracle for the Pando package, Rust is the twin checked by Go's golden fixtures, Python is a read-only client (7 to 9 sessions). Full parity with a Python writer is 12 to 16 sessions and is kept as conditional tasks P-A to P-C.
Cost if wrong: the Python oracle rule ("Python first, then Go and Rust") has one exception; adding the Python writer later costs the same 4 to 7 sessions it costs now.
Reply: `B1 confirm` / `B1 change: full parity`

**B2. W3R-1, escalation back to the requested model.** After a downgrade, the router may escalate one time back to the model the harness asked for, never above it; a flag turns it off; default off. This is not an auto model switch because the requested model is the harness's own choice.
Cost if wrong: with the flag off nothing changes; with it on, a turn can cost the frontier price you thought was saved.
Reply: `B2 confirm` / `B2 change: never escalate` / `B2 change: default on`

**B3. The `gguf` dependency.** The train pack adds the `gguf` Python wheel and llama.cpp's `convert_lora_to_gguf.py` so `daisugi lora to-gguf` works inside the pack. With a no, the command prints the converter line for the user to run in their own llama.cpp checkout and exits 2.
Cost if wrong: one more package in an optional pack (MIT); or one manual step for every adapter.
Reply: `B3 confirm` / `B3 change: no, print the command`

**B4. SG-9, enforce enforces the dialect words.** When the warden's mode is `enforce` and no `dialect_enforce` pin is set, the build's dialect hash is the pin, so the words decide. Audit mode and plan-time `verify` keep auditing. (DL-4 said "no pin: audit" in both modes.)
Cost if wrong: an envelope that uses a word your box's dialect lacks is denied in enforce; the fix is one config line. If left as today, enforce passes words it never checks.
Reply: `B4 confirm` / `B4 change: ...`

**B5. SG-3, the base for relative paths.** The warden resolves a relative operand or redirect against the call's cwd; one leading literal `cd` adds a second base, and both must be in scope; any other cwd mover makes relative paths unknown (denied unless the scope is `**`). This is the plan's one allow-direction change (`cat a > out.txt` at cwd `/work` is in `/work/**`; today it is denied).
Cost if wrong: a missed cwd mover is a fail-open; the table of movers is the guard and the review's first item.
Reply: `B5 confirm` / `B5 change: ...`

**B6. SG-6, names are reads.** `ls`, `find`, `grep -r` and `rg` read the names under a path, so they face `file_read` like `cat` does.
Cost if wrong: more denies on listing commands outside scope (false denies you can see in the audit log); no fail-open either way.
Reply: `B6 confirm` / `B6 change: ...`

**B7. SG-11, the sprig family runs with the warden on.** `sprig`, `weave` and `sprig-mcp` check every call by default through `daisugi warden check` with no `--mode`, so the installed mode decides; the only opt-out is `--warden=false`, which prints one line; a missing `daisugi` denies every call.
Cost if wrong: a sprig run on a box with no `daisugi` fails until the flag is passed. You asked for gate default-on on 2026-09-26 once it runs smoothly; sprig has not run on a box since.
Reply: `B7 confirm` / `B7 change: ...`

**B8. R-W3-1, one recorder per harness.** pi and OpenCode record through the resident warden's capture mirror; Hermes through its `pre_tool_call` shell hook; OpenClaw through the `before_tool_call` plugin; Claude Code and Codex as today. No new session-file parser is needed to record.
Cost if wrong: a harness whose hook misses a call type records less than its session file holds; the pi and OpenCode history parsers (R-W3-3) cover the gap later.
Reply: `B8 confirm` / `B8 change: ...`

**B9. R-W3-2, two capture roots.** The resident warden keeps writing under `<warden root>/captures`; the journal feed scans both roots instead of moving one.
Cost if wrong: one more directory to scan; nothing is double-recorded.
Reply: `B9 confirm` / `B9 change: ...`

**B10. R-W3-3, history import last.** `journal parse --format pi|opencode` for sessions recorded before install is built last and the wave is complete without it.
Cost if wrong: your pre-install pi and OpenCode sessions stay out of the journal a little longer.
Reply: `B10 confirm` / `B10 change: build it first`

**B11. R-W3-4, recipes are JSON.** Not YAML as `docs/recipes/README.md` said; the ports share a byte-stable JSON writer.
Cost if wrong: one serializer.
Reply: `B11 confirm` / `B11 change: yaml`

**B12. R-W3-5, a recipe step is one install primitive.** Kinds: `install`, `harness`, `config`, `warden-init`, `note`. No shell in a recipe.
Cost if wrong: a recipe cannot do what install cannot; that is the point.
Reply: `B12 confirm` / `B12 change: ...`

**B13. R-W3-6, `--only` names components.** `skill`, `mcp`, `capture`, `instructions`, `warden`, `router`; repeatable or comma-separated; `--harness` stays its own flag.
Cost if wrong: flag spelling before any release.
Reply: `B13 confirm` / `B13 change: ...`

**B14. R-W3-7, the OpenClaw plugin writes no unverified config key.** Whether `plugins.entries.opendaisugi.enabled` is needed is a box check; until then install prints the directory and the `openclaw plugins install` line.
Cost if wrong: one more manual line for OpenClaw users until the box check runs.
Reply: `B14 confirm` / `B14 change: ...`

**B15. R-W3-8, pi's package name.** The extension keeps importing `@earendil-works/pi-coding-agent` as pinned; the public npm name `@mariozechner/pi-coding-agent` describes the same API and the import is not changed.
Cost if wrong: an import path edit when a pi box check says so.
Reply: `B15 confirm` / `B15 change: ...`

## Group C: other provisional rulings in the W3 plans

**C1. MP-4, path-shaped MCP arguments are reads.** Any string argument that starts with `/` or `~/` and names a path that exists is a read of that path and must match `file_read`; a directory is read as `dir/**`.
Cost if wrong: a false deny on a tool whose argument looks like a path (the fix is a `path_in` rule); existence is the bound, so no leak passes.
Reply: `C1 confirm` / `C1 change: ...`

**C2. MP-5, the MCP schema is an authoring-time check.** The warden never needs a tool schema to decide; `warden schemas import` stores one so `register` can refuse a rule that names an argument the tool does not have.
Cost if wrong: a rule with a misspelled argument is caught at register only when a schema is on file.
Reply: `C2 confirm` / `C2 change: ...`

**C3. MP-8, anchored regexes in MCP rules.** `matches` inside `mcp_rules` must be `^...$`; register refuses otherwise. The kernel's `re.search` stays unchanged elsewhere.
Cost if wrong: one refusal message that teaches the form.
Reply: `C3 confirm` / `C3 change: ...`

**C4. MP-9, a bool is never a number.** `True` is not `1` in `equals` and `in_set`, in every predicate scope and all three ports.
Cost if wrong: a stored envelope that relied on `True == 1` flips; Task 5 of that plan counts the fixture flips and expects zero.
Reply: `C4 confirm` / `C4 change: ...`

**C5. PK-1, a non-absolute warden root.** `start` and `install` refuse with one line and exit 2 before any write; Go and Rust drop the copied traceback.
Cost if wrong: none.
Reply: `C5 confirm`

**C6. PK-8, flakes.** A wait is on a condition with a deadline; a fixed sleep stays only when it models the scenario and a comment says so; the Rust lock race is fixed in the server, not the test. `w3-deps-and-ci.md` Task 10 applies the same rule to the 133 sleeps in coppice's server tests.
Cost if wrong: none.
Reply: `C6 confirm`

**C7. LV-4, promotion is a record, not a switch.** `daisugi lora promote` writes `promoted.json` and prints what to run next; nothing automatic ever calls train, eval or promote.
Cost if wrong: you run two commands by hand after each eval.
Reply: `C7 confirm` / `C7 change: ...`

**C8. LV-5, constrained output proves shape, the warden proves meaning.** A JSON schema goes only to local OpenAI-wire endpoints; the Anthropic wire gets none; every answer still passes the same validation and verifier.
Cost if wrong: none on safety; a frontier call does not get the schema speed-up.
Reply: `C8 confirm` / `C8 change: also send to the Anthropic wire`

**C9. LV-6, no GBNF converter shipped.** daisugi ships the JSON schema and a lint against llama.cpp's unsupported list; a user who wants a grammar file runs llama.cpp's own converter.
Cost if wrong: a later task adds a converter.
Reply: `C9 confirm` / `C9 change: ...`

**C10. LV-8, skins.** A skin pack is a directory the user makes from their own installed game; the repo ships no game asset and no extractor for a named game; the server serves a pack only to the floor page.
Cost if wrong: a licence problem if an extractor for a named game were shipped; this ruling avoids it.
Reply: `C10 confirm` / `C10 change: ...`

## Group D: asks from the dependency and test-speed plan (`w3-deps-and-ci.md`)

**D1. pytest-xdist in `[dev]`.** A new test dependency (MIT, one package) so the Python suite runs on two workers by default. Task 13 first measures the gain with a two-shell split that needs no package; the number will be in the plan's acceptance table before you decide. Until a yes, the suite runs serially.
Cost if wrong: one dev-only package; CI and the box both get the gain or neither does.
Reply: `D1 confirm` / `D1 change: no`

**D2. sccache on this box.** Install `sccache` under `~/.local/bin` for Rust dependency rebuilds, cache under `/mnt/tera/.tt/sccache` (10 GB cap), never in CI. Task 14 measures with and without and keeps it only on a 30 percent gain.
Cost if wrong: 10 GB of disk and one more tool to keep current; removed in one command if it does not pay.
Reply: `D2 confirm` / `D2 change: no`

**D3. CI shape.** `clients.yml` skips docs-only pushes; all three workflows run every Monday 06:00 UTC and by hand from the Actions tab; `ci.yml` runs the dependency register; the Go job runs `-race` on the three concurrent packages.
Cost if wrong: a few runner minutes a week on a public repo (free); a weekly green run is the only proof between folds.
Reply: `D3 confirm` / `D3 change: no timer` / `D3 change: ...`

**D4. The QR modules stay.** `qrterminal` and `rsc.io/qr` (MIT, BSD-3) stay in coppice for the sign-in QR; an own encoder (about 500 lines with Reed-Solomon) is not written. Their "open" mark on the reference page is removed.
Cost if wrong: two permissive modules; replaceable later.
Reply: `D4 confirm` / `D4 change: write our own`

**D5. One Rust target directory for the daisugi binary cache.** `build-at.sh daisugi-rust` builds every worktree's Rust daisugi inside the main checkout's `clients/rust/target` under a lock, keeps only the binaries per commit, and refuses to build when cargo would compile `z3-sys` (the 2026-10-08 incident is the risk this guard covers).
Cost if wrong: a worktree build that trips the guard waits for a human instead of rebuilding Z3 for 29 minutes; that is the intended failure.
Reply: `D5 confirm` / `D5 change: ...`

## Where answers go

Your reply is recorded in `.superpowers/sdd/backlog/progress.md` as a dated ruling line, and each DECISIONS-PENDING row gets its "Retires when" cell filled with the date. Items you change get a new provisional ruling in the plan that owns them, and the builder of that plan follows the changed ruling.

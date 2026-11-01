# Ranking, the delegation tree's follow-ons, and branching: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish the ranking primitive (the judge step, `rank show`, judge trust, the pivot, and the arena: coppice's cards view with the operator-only answer), close the delegation tree's open follow-ons (the session-binding adversarial test in coppice, an envelope on a coppice task proved on `task.create` and pinned on `pane.create`, the tree and its asks on the floor), and build branching: a `tree run` that starts N children on one task in parallel, each in its own worktree, prunes the ones that fail, ranks the survivors with `daisugi rank`, proceeds on the leader and opens a card. Every branch's cost is journaled with the reason it was started.

**Architecture:** The Python package stays the oracle. Go and Rust follow with the same golden cases through `clients/rank_cases.py`, `clients/tree_cases.py` and `clients/sprig_cases.py` and their compare scripts. coppice's parts are built in Go and in coppice-rs; the view JavaScript is one file set that both binaries embed (`harness/coppice/plugins/`, which `harness/coppice-rs/build.rs` reads). coppice shows and relays; daisugi fits, records and decides (ADR-0020). The model-calling judge step uses `llm_client` and the operator's agent profiles (MX-R-1), with the fake model server of `clients/garden_cases.py` in every test and case.

**Tech Stack:** Python 3.12 (stdlib json, sqlite3, hashlib, subprocess; httpx `MockTransport` in unit tests), Go (cgo, Z3 5.1.0), Rust, the fake `sprig` of `clients/agentic_fakes.py`, the fake model server of `clients/garden_cases.py`, `node --test` for the view's JavaScript (run by `harness/coppice/internal/web/js_test.go`).

**Spec:** `docs/plans/2026-09-24-omarchy/design-ranking.md` (the contract, the judge step, the review queue, the pivot, judge trust, the staged plan), `docs/plans/2026-09-24-omarchy/design-delegation-tree.md` (Option 3, "How a child is started", steps 4 and 5), `docs/plans/2026-09-24-omarchy/design-sprig.md` (opinion 4, "Branching search, with a budget"), `docs/roadmap.md` sections 6 and 7. Rulings: `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (Ranking and Tree tables, RK-10, ST-5), `clients/ADJUDICATIONS.md` (RK-R-1 to RK-R-16, TR-R-1 to TR-R-12, MX-R-1 to MX-R-16, SW-11, SEC-3), the owner's words in `.superpowers/sdd/omarchy/progress.md` (2026-09-28: "branching search OK with token care"; 2026-10-07: human ranking of attempts is not part of sprig's edge; 2026-10-24: branching parked at the ship cut).

## What is already built (do not plan it again)

Evidence read on 2026-10-30 from `src/opendaisugi/`, `clients/`, `harness/`, `clients/ADJUDICATIONS.md`, `docs/feature-status.md` and `.superpowers/sdd/omarchy/progress.md`. `docs/roadmap.md` section 6 still says "Designed, not built"; that line is stale and Task 23 fixes it.

| Part | State | Where |
|---|---|---|
| Ranking stages 1 to 4: the comparison and choice records (`<data>/journal/rankings/comparisons.jsonl`, `choices.jsonl`), the pure fit (elimination, quality tiers, Bradley-Terry with the prior, the splitmix64 seeded bootstrap, owner constraints and cycles, cost as the last tie-break, the next judge pair), `rank fit|choose|queue`, `rank record pick|drop`, the switch cost from the journal index, decay at 7 days or `follow_up_only`, the resumed-run rule | Built 2026-09-30 (RK-R-1 to RK-R-16); Python, Go, Rust; 135 rank cases, 0 disagreements | `src/opendaisugi/rank.py` (1,341 lines), `clients/go/internal/rank/{rank,cards}.go`, `clients/rust/src/rank.rs`, `clients/rank_cases.py`, `clients/fixtures/rank/` |
| The warden's hard deny of an agent's `daisugi rank record` | Built (SEC queue) | `src/opendaisugi/rank_rule.py`, `owner_rule.RANK_VERBS`, `tests/test_gate_rank_rule.py` (4 tests; renamed `test_warden_rank_rule.py` by W2) |
| weave's N-attempt form (`attempts: N` on a task step), the never-block rule, the card carried on the permanent-tier ask, resume keeps the question | Built (RK-R-12 to RK-R-15, WV-R-7) | `src/opendaisugi/weave.py` lines 484 to 631, ports under `supervise` |
| The delegation tree steps 1 to 3: `edge_ok`, the deadline field, `warden register --parent`, `tree check|root|spawn|end|answer|status`, the ledger, the fourth refused proposal as an ask, the agentic step's child envelope | Built 2026-09-30 (TR-R-1 to TR-R-12); 134 tree cases | `src/opendaisugi/tree.py`, `clients/go/internal/tree/`, `clients/rust/src/tree.rs`, `clients/tree_cases.py` |
| Mixed-provider trees: agent profiles, keys by pipe, the one starter `tree run`, exact charging, the kill of every descendant, output caps; sprig's `spawn` tool (step 5) | Built 2026-10-10 (MX-R-1 to MX-R-16); tree 164 cases, sprig 277 cases | `src/opendaisugi/tree_run.py`, `agent_profiles.py`, Go `supervise/treerun.go`, Rust `supervise/treerun.rs`, `harness/sprig/spawn.go`, `harness/sprig-rs/src/spawn.rs` |
| The deadline enforced at call time | Built 2026-10-01 (SW-4) | warden |
| The steward's workers run as tree nodes: `spawn` reads `<config dir>/envelopes/<project>.json`, runs `daisugi tree run --parent project-<name>`, reports the answer into the chat; pools; the fence around the worker's sprig | Built (foreman M2/M3, FN-R-25, FM-R-93) | `harness/coppice/internal/server/steward_work.go` (today `foreman_work.go`), `steward_tools.go`, `harness/coppice-rs/src/server/foreman_work.rs` |
| The garden overlay on the floor (the daisugi drawing: trunk, shoots, lanterns for needs-you) | Built 2026-10-24 ("GARDEN SHIPPED") | `harness/coppice/internal/web/static/garden.js`, `overlay.js`, `_tests/garden.test.mjs` |
| The `tree` view plugin (the task tree) and the view post-back whitelist (`open-pane`, `set-sel`, `watch`, `close`) | Built | `harness/coppice/plugins/tree/`, `plugins/_lib/view.js`, `internal/web/static/views.js` |

Not built, and in this plan: `rank show`, `rank judge` (stage 5), judge trust (stage 9), the pivot (stage 8), the coppice cards view with its data kind and `answer` post-back (stage 7, the arena of ST-5), the delegation tree's step 4 in coppice (the adversarial session-binding test, an envelope on a task, the proof on `task.create`, the pin on `pane.create`), the tree and its asks on the floor, parallel children (MX-R-10 says "Parallel children, with ranking, come later"), and branching itself (parked 2026-10-24).

Not in this plan, by ruling (see "Open questions"): the router's labels from `rank` (stage 6: the router plan's grader owns part 3), the garden A/B consumer (stage 10: `gardener/ab_test.py` does not execute plans, so it has no outcomes to rank; only its fail-open judge fallback is fixed here), the SQLite index of RK-6, sprig's own use of human ranking (the owner's 2026-10-07 word), and sequence invariants in `edge_ok` (part 5 of the edge rule already requires the child's invariants to include the parent's by value, TR-R-2, so the collapse inside `envelope_subsumes` cannot widen an edge).

## What branching means here

The sources: `design-sprig.md` opinion 4 ("Branching search, with a budget": branch only on a signal; cheap models where the verifier can judge; prune early; a hard token budget per task, journaled; rank survivors with `daisugi rank`); the owner's word of 2026-09-28 ("branching search OK with token care", ledger line 290); MX-R-10 ("children run one at a time. Parallel children, with ranking, come later"); the ship cut of 2026-10-24, which parked "branching" (ledger line 1046). It is not conversation branching and not a git-flow feature.

So branching is: **one task, N children in parallel, each in its own git worktree under the same proved edge and its own budget reservation; the ones that fail a hard check are eliminated; the survivors are ranked by `daisugi rank` (tests first, then judges, then cost); the parent proceeds on the leader; the choice opens a card; every branch's spend and the reason it was started are rows in the tree ledger.** The one starter is `daisugi tree run` (MX-R-5), so sprig's `spawn` tool and the steward's `spawn` tool get branching by one new field each.

## Global Constraints

- **Build after the W2 rename.** Write every identifier in the new names: gate -> **warden** (`src/opendaisugi/warden.py`, `daisugi warden <verb>`, `<data>/warden/` with `envelopes/` inside it, `clients/go/internal/warden/`, `clients/rust/src/warden/`, `clients/fixtures/warden/`, `clients/warden_cases.py`); gateway -> **router**; the inner policy -> **routing mode** (`routing_mode`); foreman -> **steward** (coppice verbs `task.set_steward`, `floor.steward`, field `steward`; the steward's MCP tool `steward`; the coppice `<data>/foreman/` directory keeps its name on disk behind `steward.DirName`). coppice server files named `foreman_*.go` and `foreman_*.rs` today: use the names W2 gave them; this plan writes `steward_work.go`, `steward_tools.go`, `steward_work.rs`, `steward_tools.rs`. `rank_rule.py`, `tree_rule.py` and `owner_rule.py` keep their names. Where this plan names a `warden register` or `tree spawn` verb, it is the renamed `gate register`.
- Laptop: 4 cores, 15 GB. **One heavy job at a time** (a Z3 or cgo build, a cargo build, a full compare, a fuzz run, the full pytest suite, `go test ./...` in coppice), each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, with `go test -p 1`, `CARGO_BUILD_JOBS=2`, `DAISUGI_NATIVE_JOBS=2`. Never rebuild Z3; never `cargo clean` in `clients/rust`. Rust unit tests run with `--release` (the debug profile rebuilds Z3 and fails on this box, ledger 2026-10-22). coppice-rs gets its own `CARGO_TARGET_DIR` per worktree (its `build.rs` embeds web statics by absolute path; a shared target serves another worktree's files, ledger 2026-10-15).
- `/tmp` is RAM. Scratch is `/mnt/tera/.tt/w3-rank-tree-branch/`. Worktree: `/mnt/tera/working/programming/od-wt/w3-rank-tree-branch`, branch `w3-rank-tree-branch`, made from master after the W2 merge. `DAISUGI_RANK_SCRATCH`, `DAISUGI_TREE_SCRATCH` and the sprig compare's root point into the scratch dir.
- **No network and no real model in any test or case.** Judges answer from the fake model server (`clients/garden_cases.py` `FakeServer`, keyed by the SHA-256 of the request body) or an httpx `MockTransport`; children are the fake `sprig` of `clients/agentic_fakes.py`. Tests never run a real `claude`, `sprig`, `codex`, `pi` or `opencode`. The judge prompt bytes are fixed in Task 3 so the three ports send identical bodies.
- Never `pkill -f`. Stop a process by the pid a test recorded; a branch's children die with the group the ledger holds (MX-R-13).
- No AI attribution in commits. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path. No push: the fold script pushes on the owner's word.
- Prose: ASD-STE100. No em dashes. Never "seam" or "the layer" (write daisugi). No changelog voice in docs. Every number names its source.
- **Every coppice run passes its own `--socket` and `--data-dir`** under scratch, in tests, cases and hand checks. Never touch the owner's live coppice server, `~/.local/state/coppice`, `~/.opendaisugi`, `~/.config/coppice`, `~/.claude/settings.json`. Environment variables do not select the server.
- **Fail closed.** A judge error is no vote. An author with no model name cannot be checked against the other-family rule, so that pair is not judged. A pivot never runs a permanent step; when the undo would be dishonest it prints the follow-up task and changes nothing. A branch whose worktree cannot be made does not start. A `tree run` that cannot reserve every branch's budget starts no branch. A task edge the server cannot prove refuses the task. Reading (`rank show`, `rank queue`, `rank judges`, `/api/cards`, `/api/tree`) creates no file.
- **Only the operator answers a card or a tree ask.** The web routes that record an answer run under `guardAnswer` and `placeAllows`, as `/api/ask/answer` does: a pane process, or a peer the server cannot place, is refused with `PaneRefusal`. A view holds no token and posts the answer to the floor page, which sends it on.
- **Full parity:** Python first, then Go and Rust on the same golden cases, 0 disagreements; coppice parts in Go and coppice-rs with the protocol corpus and the `web` and `chat` case slices; sprig parts in Go and sprig-rs with the sprig cases; every refusal under a ruling in `clients/ADJUDICATIONS.md`. The judge step is model-calling, so its Go and Rust ports follow the Python one inside this plan (the house rule is full parity; the design's "ported last" is met by task order).

## Provisional rulings made by this plan (W3K-n; owner to confirm; each easy to undo)

| ID | Ruling | Cost if wrong |
|---|---|---|
| W3K-1 | A judge entry in `policy.judges` is `PROFILE/MODEL` or `PROFILE` (the profile's default model); the comparison's `judge` field is the entry as written. A model's **family** is the first word of its id (letters before the first digit, `-`, `.`, `/`, `:`), lowercased, with one merge: `gpt`, `o1`, `o3`, `o4`, `chatgpt` are one family, and `gemini`, `gemma` are one family. No other table, no vendor or origin words. The judge's family must differ from every author's family in the pair; an author with no model name makes the pair unjudgeable. | A table swap; votes already recorded keep their `judge` text. |
| W3K-2 | Judge trust (RK-5): agreement is counted over answered cards (`confirmed` or `overridden`) where the judge voted on the pair; a judge whose agreement is under **0.5 over at least 5 such cards** is dropped from the default judge list, and `rank judge` says so. Kendall tau-b and the cyclic magnitude are reported, never used to drop. **Judge weight (owner, 2026-10-30, item A3): a Thompson sample, not a fixed 1.** Each judge has a Beta posterior over its agreement rate, `Beta(1 + agreed, 1 + disagreed)`, counted over answered cards and, where a card has no owner answer, over agreement with the majority of the other judges on that pair (ties and single-judge pairs are skipped). Per ranking, the fit draws one sample per judge and uses `2 * sample` as that judge's vote weight (the factor 2 keeps the prior mean at the old weight of 1). The draw is seeded from the ranking's fit seed and the judge id, so the same journal gives the same weights in all three ports. The drop floor stays and is checked first: a dropped judge has no vote. | Two numbers in `Policy`; the weights are a function of the journal and the seed, so a ranking re-fits to the same bytes. |
| W3K-3 | The SQLite index of RK-6 is not built. `rank queue` reads the JSONL files; a test bounds a 10,000-row `choices.jsonl` read under 2 s on the reference box. The index waits for a real file past that. | Add the index later; the JSONL stays the source (RK-6). |
| W3K-4 | The pivot (`rank pivot`) undoes only receipts whose `reversibility` is `reversible`, through `deeds.rollback_run`, in the runs the card's `facts.run` names, and applies the alternative from its `where.path` by `git diff` and `git apply --3way` when the chosen and the alternative are worktrees of one repo; any other shape, and `follow_up_only`, prints the follow-up task text and changes nothing. A pivot never calls `checkpoints.restore` on its own (that needs a session's checkpoint id the card does not carry). | A later pivot may add the checkpoint path; the plan output format is additive. |
| W3K-5 | coppice's cards and tree routes run `daisugi rank queue --json`, `rank show`, `rank record`, `tree status --json` and `tree answer` as child processes of the server, with `--data-dir` from `COPPICE_DAISUGI_DATA_DIR` when set and daisugi's own default otherwise, and `daisugi` from `COPPICE_DAISUGI_BIN` or PATH (as the fence does). The server never reads or writes the rankings files itself. | One env name. |
| W3K-6 | The diff a card shows comes from `daisugi rank show --choice C --diff`, which reads only `where.path` directories under `<data>/tree/branches/` or under a path the caller passes with `--worktrees-under DIR` (coppice passes its task worktree root); any other path answers `no diff: the alternative is outside the floor's worktrees`. The diff is capped at 512 KiB. | Widen the roots list. |
| W3K-7 | A branch runs in `<data>/tree/branches/<session>/`, made with `git worktree add --detach <dir> HEAD` from the request's `cwd`. A `cwd` that is not inside a git work tree refuses branching: `branching needs a git workspace; <cwd> is not one`. Branch worktrees stay until their card closes; `daisugi tree prune-branches` removes the worktrees of closed cards and the server runs it after an answer. | Add a copy mode for non-git work later. |
| W3K-8 | A `tree run` request with `attempts` 2 to 8 must carry `why`, one of `failed_verify`, `failed_tests`, `low_confidence`, `operator`; it is journaled on every branch's spawn row (`branch_of`, `why`). A missing or other `why` is bad input (exit 2). | Add a word to the list. |
| W3K-9 | Each branch reserves the request's full `tokens` and `turns` from the parent; the parent must have N times that left, else the run is refused with `the parent has T tokens left; N branches of B each need N*B` and no branch starts. Children run with at most `--parallel K` live at once, default `min(N, 4)`. | Two numbers. |
| W3K-10 | A branch's `content_hash` is `sha256:` over `git diff --binary HEAD` in its worktree, then a NUL byte, then the child's answer text as UTF-8. Two branches with the same diff and answer share a hash (RK-R-2 rejects an id used twice, not a hash). | A different digest recipe; the field is text. |
| W3K-11 | The hard checks of a branch (`checks: [{name, argv, required}]`) come only from the starter's side: the `--check NAME=ARGV` flags of `tree run` (which the steward and the executor write from the project's envelope file `checks` key) and never from the spawn tool's input. A check runs in the branch's worktree with the child's scrubbed environment, the envelope's `max_execution_time_s` as its timeout and a 32 KiB output cap; exit 0 is `pass`, anything else `fail`. A branch whose sprig did not end with status `ran` fails the required check `the child ran`. | Add a trust rule for model-written checks later. |
| W3K-12 | On the floor, a pane created into a task that carries an envelope is pinned: the server mints a session, registers the task's envelope under it (`tree root` for a task with no parent, which only the operator may create with an envelope; `tree spawn --parent` for a child task, which proves the edge), and starts the pane's warden hook with `--session <minted>`. A pane connection may `pane.create` or `pane.split` only into its own task or a child of it; another task gets `unauthorized`, `a pane starts panes only under its own task.` | Loosen the placement rule; the pin stays. |
| W3K-13 | Branch ledger rows: all N `spawn` rows are appended, in branch order, under the lock, before any child starts; the `end` rows are appended after every branch has ended, in branch order, each with that branch's real use. The reply's `branches` list is in branch order too. This keeps the golden tree deterministic under parallel children. | None stored depends on it. |
| W3K-14 | Stage 6 (the router's labels from `rank`) belongs to the router plan (`w3-router-grafts.md`), whose grader fills the outcome table. This plan builds no join into `router_status`. | Add one reader of `choices.jsonl` later. |
| W3K-15 | The garden A/B consumer (stage 10) is not built: `gardener/ab_test.py` does not execute plans, so no attempt has outcomes. Its judge fallback is made fail closed here (a judge that raises is `judge_error`, and `passed` is then False). Python only: no port of `ab_test.py` exists (grep `ab_test` under `clients/go` and `clients/rust` finds nothing). | None. |
| W3K-16 | A steward `spawn` with `attempts` reports the leader's answer, then one line `N branches ran; card <choice_id> opened: <headline>`; the steward's `status` tool lists the count of open cards. The steward never answers a card (it is a pane; the route refuses it). | Text only. |
| W3K-17 | The cyclic magnitude of a judge is the share of its voted pairs that lie on a directed cycle of majority votes, not the HodgeRank curl share (a consistent judge reads 0.0; the curl share of a transitive triangle is 1/9). | One function. |
| W3K-18 | The pivot takes the chosen edits out of the chosen worktree first (`git add -A`, `diff --cached`, `apply -R --index`), applies the other attempt's patch, and puts the chosen edits back if that fails. | Swap the loop for a checkpoint restore. |
| W3K-19 | `attempts: 1` is the single child. A bool, 0, 9, a float other than 1.0 and a text are bad input (exit 2). | One condition. |
| W3K-20 | The bare aliases `haiku`, `sonnet`, `opus` and `fable` are the `claude` family, so a `claude-*` judge cannot judge an author recorded under an alias. | The merge table, in three ports. |
| W3K-21 | The leader's edits stay in `<data>/tree/branches/<session>`; nothing is copied by itself. `daisugi tree adopt SESSION` applies the branch's diff against its base commit to the parent repository with `git apply --3way`, and refuses (exit 2, writes nothing) when a touched file has uncommitted changes or the edits conflict. The card names the command (`facts.adopt`). | One verb to remove. |
| W3K-22 | Judge weights apply when the journal supplies the votes; votes held inline in an attempts file keep weight 1. A dropped judge is dropped whatever shape the file has. | One condition. |
| W3K-23 | A budget refusal in `spawn_branches` writes no `refused` row and does not count toward the ask after three; a single `spawn` does both. | One branch of code. |

Tasks that depend on provisional DECISIONS-PENDING rows that are not retired: RK-1 (Tasks 3, 16), RK-5 (Task 4), RK-6 (Task 2, by W3K-3), RK-7 (Tasks 10, 11), RK-8 (Task 16), RK-10's decay default (ruled 14 days on 2026-10-30, Task 4b; Task 10 shows `decays_in_days`), AT-1 step 4 (Tasks 12 to 15), ST-5 (Tasks 10, 11). RK-1, RK-5 to RK-8 were confirmed as written in the owner's 2026-09-28 bulk confirmation and stay in the table as provisional; the commit messages say so. MX-R-10 retires with Task 16 ("Retires when: parallel children are built").

## Review Focus

1. A judge call whose reply is anything but one of `A`, `B` or `tie` on its own line records no vote, and a transport error or a non-2xx status records no vote; neither is ever a win for either side. Test `test_judge_error_is_no_vote` in Task 3.
2. The two swapped calls of one pair share a `pair_id`, and a win counts only when both orders agree (the fit already does this, RK-R-4); the judge step must write both rows or neither. Test `test_judge_writes_both_orders_or_neither` in Task 3.
3. `/api/cards/answer` and `/api/tree/answer` refuse a pane process and a peer the server cannot place, exactly as `/api/ask/answer` does, and a view can post an `answer` only for a `choice_id` the floor page sent it. Tests `TestCardsAnswerRefusesAPane` (Task 10) and `answer post-back for an unknown card is dropped` in `cards.test.mjs` (Task 10).
4. A `tree run` with `attempts` reserves every branch before any child starts, and a refused reservation starts nothing and leaves no worktree. Test `test_branches_reserve_all_or_none` in Task 16.
5. A pane connection cannot start a pane under another task, and a pane started into a task with an envelope carries `--session` in its warden hook and that session resolves to the task's envelope and never to `default`. Tests `TestAPaneStartsPanesOnlyUnderItsOwnTask` and `TestAPinnedPaneNeverReadsDefault` in Task 13, after the adversarial test of Task 12.

---

### Task 1: Worktree, scratch and the baseline

**Files:**
- No source change.

- [ ] **Step 1: Make the worktree and scratch**

```bash
cd /mnt/tera/working/programming/openDaisugi
git fetch --all --prune
git worktree add ../od-wt/w3-rank-tree-branch -b w3-rank-tree-branch master
mkdir -p /mnt/tera/.tt/w3-rank-tree-branch/{rank,tree,sprig,coppice,cargo-coppice}
df -h /mnt/tera | tail -1
cd ../od-wt/w3-rank-tree-branch && uv sync --locked --extra dev --extra shell
```
Expected: the worktree is on real disk; more than 20 GB free.

- [ ] **Step 2: Record the baseline (one heavy job at a time)**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync pytest -q tests/test_rank.py tests/test_tree.py tests/test_tree_run.py tests/test_weave.py tests/test_warden_rank_rule.py tests/test_warden_tree_rule.py
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/rank_compare.py --binary clients/go/daisugi --max-refused 0
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/tree_compare.py --binary clients/go/daisugi --max-refused 0
```
Expected: pytest green (21 + 26 + 22 + 19 + 4 + the tree-rule tests); rank 135 agree, tree 164 agree (the counts `docs/spec/conformance.md` and MX-R-11 name; take the numbers the run prints). Keep the four numbers for the ledger.

- [ ] **Step 3: Write the ledger line**

Append to `.superpowers/sdd/backlog/progress.md`: `W3 rank/tree/branching: worktree od-wt/w3-rank-tree-branch; baseline rank N, tree N, pytest N.` No commit (the ledger is outside the repo's tracked paths).

---

### Task 2: `rank show`: a ranking from the journal

**Files:**
- Modify: `src/opendaisugi/rank.py` (add `show_doc`, `show_text`)
- Modify: `src/opendaisugi/cli.py` (the hidden `rank` group: add `show`)
- Test: `tests/test_rank.py`

**Interfaces:**
- Produces: `rank.show_doc(data_dir: Path, *, ranking_id: str | None, choice_id: str | None, now: float) -> dict`: `{"ranking_id", "comparisons": [rows in file order], "choices": [the choice rows of that ranking, in file order], "card": card_view or None, "owner_answers": [[before, after], ...], "warnings": [...]}`. Exactly one of `ranking_id` and `choice_id` is given; a `choice_id` resolves to its `ranking_id` through `read_cards`. Rows that do not read are counted in `warnings` as `N comparison rows did not read` (the same skip rule as RK-R-8). Reading creates nothing.
- Produces: `rank.show_text(doc) -> str`: one block per section, the house plain style of `queue_text`.
- Produces: `daisugi rank show (--ranking R | --choice C) [--data-dir D] [--json]`: exit 0; exit 1 `No ranking R.` when neither file holds a row for it; exit 2 when both or neither selector is given (`Give --ranking or --choice, not both.`).

- [ ] **Step 1: Write the failing tests**

```python
def test_show_reads_votes_and_choices_and_creates_nothing(tmp_path):
    dd = tmp_path / "dd"
    f = _write(
        tmp_path / "a.json",
        _doc([_att("a"), _att("b")], comps=_cmp("j1", "p1", "a", "b", "a", "a")),
    )
    out = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)
    cid = out["choice_id"]
    res = _run("show", "--choice", cid, "--data-dir", str(dd), "--json")
    assert res.exit_code == 0, res.output
    doc = json.loads(res.output)
    assert doc["ranking_id"] == "r1" and [c["event"] for c in doc["choices"]] == ["opened"]
    assert doc["card"]["choice_id"] == cid
    before = sorted(p.name for p in dd.rglob("*"))
    _run("show", "--ranking", "r1", "--data-dir", str(dd))
    assert sorted(p.name for p in dd.rglob("*")) == before


def test_show_refuses_two_selectors_and_names_a_missing_ranking(tmp_path):
    dd = tmp_path / "dd"
    assert _run("show", "--ranking", "r1", "--choice", "ch_x", "--data-dir", str(dd)).exit_code == 2
    res = _run("show", "--ranking", "nope", "--data-dir", str(dd))
    assert res.exit_code == 1 and "No ranking nope." in res.output
    assert not dd.exists()
```

Run: `uv run --no-sync pytest -q tests/test_rank.py -k show`
Expected: FAIL (`show` is not a command).

- [ ] **Step 2: Implement `show_doc`, `show_text` and the command**

`show_doc` reads `comparisons.jsonl` through `journal_comparisons` (it already returns the rows and the skipped count) and `choices.jsonl` through `_read_rows`; the card is `card_view(card, data_dir, now)` for the card whose `ranking_id` matches, or None; `owner_answers` reuses `owner_answers(data_dir, ranking_id, by_id, warnings)` with `by_id` built from the card's survivors (an empty map when there is no card, so no answer binds). Comparisons that the fit has to hold inline for golden cases (RK-R-11) are not read here: `show` reads the journal only.

- [ ] **Step 3: Run the tests and the suite for the file**

Run: `uv run --no-sync pytest -q tests/test_rank.py`
Expected: 23 passed.

- [ ] **Step 4: Commit**

```bash
git add src/opendaisugi/rank.py src/opendaisugi/cli.py tests/test_rank.py
git commit -m "rank: show a ranking's votes, choices and card from the journal, without writing

RK-R-1 listed rank show as not built. The choice and comparison files were
only readable through queue, which folds them; a reviewer or a port needs
the rows behind a label. Reading creates nothing (K4-8)."
```

---

### Task 3: The judge step, `rank judge`

**Files:**
- Create: `src/opendaisugi/rank_judge.py`
- Modify: `src/opendaisugi/rank.py` (add `family_of`)
- Modify: `src/opendaisugi/cli.py` (`rank judge`)
- Test: `tests/test_rank_judge.py` (new)

**Interfaces:**
- Produces: `rank.family_of(model_id: str) -> str` (W3K-1); `""` for an empty or digit-first id.
- Produces: `rank_judge.JUDGE_SYSTEM: bytes` and `rank_judge.prompt(task: str, first: str, second: str) -> list[dict]`: the messages `[{"role": "system", "content": JUDGE_SYSTEM}, {"role": "user", "content": USER}]` with `USER` exactly `Task:\n{task}\n\nAnswer A:\n{first}\n\nAnswer B:\n{second}\n\nWhich answer does the task better? Reply with one line: A, B or tie.` The bytes are fixed here so the Go and Rust ports send identical request bodies; a test pins their SHA-256.
- Produces: `rank_judge.artifact(attempt: Attempt) -> str`: `where.text` when `where.kind == "output"`; `git diff --binary HEAD` run in `where.path` when `where.kind == "worktree"`, capped at 256 KiB with a last line `[cut at 256 KiB]`; else the empty text with a warning `attempt X has no artifact a judge can read`.
- Produces: `rank_judge.parse_verdict(text: str) -> str | None`: the first non-empty line, stripped of whitespace and trailing `.`, must be exactly `A`, `B` or `tie` (case-insensitive); anything else is None.
- Produces: `rank_judge.judge_once(doc: dict, data_dir: Path, *, profiles: Profiles, env: Mapping[str, str], judge: str | None, now: float, call=llm_client.complete) -> dict`: runs `rank.fit` (with the journal's owner answers as `_rank_fit` does), takes `next` (or the pair of `--judge J` when given, which must be an untried judge for the leader's nearest rival), checks W3K-1 (families), makes two calls with the pair in both orders, records two comparison rows (`pair_id` = `pj_` + first 12 hex of SHA-256 over `[ranking_id, i, j, judge, len(comparisons so far)]`, `shown` `ab` then `ba`, `lengths` of the two artifacts, `ts`) only when both calls returned a parsed verdict; else records nothing. Returns `{"recorded": bool, "pair": [i, j] | None, "judge": J | None, "outcomes": [o1, o2] | None, "why": text, "fit": the fit after recording}`.
- Produces: `daisugi rank judge ATTEMPTS.json [--judge J] [--loop] [--data-dir D] [--json]`: one comparison (two calls), or with `--loop` repeat until the fit's `next` is null or a call records nothing. Exit 0 when at least one comparison was recorded; 3 when nothing was there to judge (`next` null, or no judge of another family, or no artifact); 1 when a call failed (no vote recorded); 2 bad input. The attempts file's `policy.judges` must be non-empty, else exit 3 with `policy.judges names no judge.`
- Consumes: `agent_profiles.Profiles.lookup(name)`, `llm_client.complete(model, messages, base_url=, api_key=, env=)` with the profile's wire fields (as `tree_run` resolves a profile's model and key); the key comes from the environment variable the profile names, read by the operator's process, never written to disk.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_rank_judge.py
import hashlib, json
from pathlib import Path
import pytest
from opendaisugi import rank, rank_judge
from opendaisugi.agent_profiles import Profiles


def _att(aid, author="", text=None):
    return {
        "id": aid,
        "content_hash": f"h-{aid}",
        "author": author,
        "where": {"kind": "output", "text": text or f"answer {aid}"},
    }


def _doc(*atts, judges=("local/judge-9b",)):
    return {
        "ranking_id": "r1",
        "task": "add two numbers",
        "attempts": list(atts),
        "comparisons": [],
        "policy": {"bootstrap": 200, "judges": list(judges)},
    }


def _profiles():
    return Profiles.from_doc(
        {
            "local": {
                "wire": "openai",
                "base_url": "http://127.0.0.1:1/v1",
                "model": "judge-9b",
                "key": "",
            }
        }
    )


def _calls(replies):
    seen = []

    def call(model, messages, **kw):
        seen.append((model, messages, kw))
        r = replies.pop(0)
        if isinstance(r, Exception):
            raise r

        class Reply:
            text = r

        return Reply()

    return call, seen


def test_family_of():
    assert rank.family_of("claude-haiku-4-5") == "claude"
    assert rank.family_of("gpt-5-mini") == rank.family_of("o4-mini") == "gpt"
    assert rank.family_of("gemma3:4b") == rank.family_of("gemini-2.5-flash") == "gemini"
    assert (
        rank.family_of("Qwen3-8B") == "qwen"
        and rank.family_of("") == ""
        and rank.family_of("9b") == ""
    )


def test_prompt_bytes_are_pinned():
    m = rank_judge.prompt("t", "x", "y")
    digest = hashlib.sha256(
        json.dumps(m, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()
    assert digest == "<fill in from the first green run; the ports pin the same value>"


def test_judge_records_both_orders_and_the_fit_moves(tmp_path):
    call, seen = _calls(["A", "B"])  # a wins shown first, and a wins shown second (B is a)
    out = rank_judge.judge_once(
        _doc(_att("a", "claude-x"), _att("b", "claude-y")),
        tmp_path,
        profiles=_profiles(),
        env={},
        judge=None,
        now=1.0,
        call=call,
    )
    assert out["recorded"] is True and out["pair"] == ["a", "b"] and out["outcomes"] == ["a", "a"]
    rows = [
        json.loads(l)
        for l in (tmp_path / "journal/rankings/comparisons.jsonl").read_text().splitlines()
    ]
    assert [r["shown"] for r in rows] == ["ab", "ba"] and len({r["pair_id"] for r in rows}) == 1
    assert all("claude" not in json.dumps(m) for _, m, _ in seen)  # the author is hidden
    assert out["fit"]["order"][0] == "a"


def test_judge_error_is_no_vote(tmp_path):
    call, _ = _calls(["A", RuntimeError("boom")])
    out = rank_judge.judge_once(
        _doc(_att("a", "claude-x"), _att("b", "claude-y")),
        tmp_path,
        profiles=_profiles(),
        env={},
        judge=None,
        now=1.0,
        call=call,
    )
    assert out["recorded"] is False and "no vote" in out["why"]
    assert not (tmp_path / "journal").exists()


def test_judge_writes_both_orders_or_neither(tmp_path):
    call, _ = _calls(["A", "maybe A"])
    out = rank_judge.judge_once(
        _doc(_att("a", "claude-x"), _att("b", "claude-y")),
        tmp_path,
        profiles=_profiles(),
        env={},
        judge=None,
        now=1.0,
        call=call,
    )
    assert out["recorded"] is False and not (tmp_path / "journal").exists()


def test_same_family_judge_is_refused_and_unknown_author_too(tmp_path):
    call, seen = _calls([])
    out = rank_judge.judge_once(
        _doc(_att("a", "judge-9b"), _att("b", "claude-y")),
        tmp_path,
        profiles=_profiles(),
        env={},
        judge=None,
        now=1.0,
        call=call,
    )
    assert out["recorded"] is False and "same family" in out["why"] and seen == []
    out = rank_judge.judge_once(
        _doc(_att("a", ""), _att("b", "claude-y")),
        tmp_path,
        profiles=_profiles(),
        env={},
        judge=None,
        now=1.0,
        call=call,
    )
    assert out["recorded"] is False and "no author" in out["why"] and seen == []


@pytest.mark.parametrize(
    "text,want", [("A", "a"), (" b.\nreason", "b"), ("Tie", "tie"), ("A or B", None), ("", None)]
)
def test_parse_verdict(text, want):
    got = rank_judge.parse_verdict(text)
    assert (got is None) == (want is None)
```

The `prompt` test's digest is filled from the first green run and then copied into the Go and Rust tests (Tasks 7 and 8). `Profiles.from_doc` is the constructor `agent_profiles.resolve` uses; if its name differs, use the one in `agent_profiles.py` and change nothing else.

Run: `uv run --no-sync pytest -q tests/test_rank_judge.py`
Expected: FAIL (no module `rank_judge`).

- [ ] **Step 2: Implement**

`judge_once` maps the parsed verdict of the first call (`A` is the first shown) and the second call (`A` is the second shown) to attempt ids, so `outcomes` are attempt ids or `tie`; the comparison row's `outcome` is `a`, `b` or `tie` in the row's own `a`/`b` frame (as `_cmp` in `tests/test_rank.py` writes them). The profile's key variable is read from `env` at call time; the call's `kw` carries `base_url` and `api_key`; `model` is the entry's model part. Length control is recorded (`lengths`), not applied (design, "Length").

- [ ] **Step 3: Add the CLI command and a CLI test**

```python
def test_cli_judge_loops_until_the_judges_are_exhausted(tmp_path, monkeypatch):
    replies = iter(["A", "B"])
    monkeypatch.setattr(
        "opendaisugi.rank_judge.llm_client.complete",
        lambda model, messages, **kw: type("R", (), {"text": next(replies)})(),
    )
    monkeypatch.setattr("opendaisugi.rank_judge.load_profiles", lambda: _profiles())
    f = tmp_path / "a.json"
    f.write_text(json.dumps(_doc(_att("a", "claude-x"), _att("b", "claude-y"))))
    res = CliRunner().invoke(
        app, ["rank", "judge", str(f), "--loop", "--data-dir", str(tmp_path / "dd"), "--json"]
    )
    assert res.exit_code == 0, res.output
    out = json.loads(res.output)
    assert out["recorded"] == 1 and out["stop"] == "judges_exhausted"
```

Run: `uv run --no-sync pytest -q tests/test_rank_judge.py tests/test_rank.py`
Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add src/opendaisugi/rank_judge.py src/opendaisugi/rank.py src/opendaisugi/cli.py tests/test_rank_judge.py
git commit -m "rank: the judge step, two swapped calls per pair, the author hidden, no vote on error (W3K-1)

Stage 5 of design-ranking.md. A win counts only when both orders agree;
a judge from an author's family, or an author with no model name, is
refused before any call. The prompt bytes are pinned so the ports send
the same body. Depends on provisional RK-1 (confirmed 2026-09-28)."
```

---

### Task 4: Judge trust, `rank judges`

**Files:**
- Modify: `src/opendaisugi/rank.py` (add `kendall_tau_b`, `cyclic_magnitude`, `judge_trust`, `judges_text`, `beta_sample`, `judge_weights`; `Policy.trust_floor = 0.5`, `Policy.trust_min_cards = 5`; `fit(..., weights=None)`)
- Modify: `src/opendaisugi/rank_judge.py` (skip a dropped judge; say so in `why`)
- Modify: `src/opendaisugi/cli.py` (`rank judges`)
- Test: `tests/test_rank.py`

**Interfaces:**
- Produces: `rank.kendall_tau_b(a: list[str], b: list[str]) -> float | None`: tau-b over the items both orders hold; None under 2 shared items. Ties come as nested lists (a quality tier is one tie group).
- Produces: `rank.cyclic_magnitude(votes: list[Vote]) -> float | None`: HodgeRank's share of the vote flow no single order explains, over one judge's votes in one ranking; None when the votes cover fewer than 3 attempts or close no loop ("not measurable", never 0).
- Produces: `rank.judge_trust(data_dir: Path) -> dict`: per judge `{"cards": N answered cards where it voted, "agree": share, "tau": tau-b over whole rankings or None, "cyclic": {ranking_id: magnitude | None}, "dropped": bool, "why": text}`; the sample is named biased toward hard cards (design, "Judge trust").
- Produces: `rank.beta_sample(alpha: int, beta: int, rng: SplitMix64) -> float`: for integer parameters, the `alpha`-th smallest of `alpha + beta - 1` uniforms from `rng` (the order-statistic form of Beta; no logarithm, no gamma function, so the bytes match in Go and Rust). Each uniform is `(rng.next() >> 11) / 2**53`. The result is rounded to 9 digits like every float in the fit.
- Produces: `rank.judge_weights(trust: dict, seed_hex: str) -> dict[str, float]`: for each judge, `agreed` and `disagreed` from `trust` (both default 0), `SplitMix64(int(sha256(seed_hex + judge)[:16], 16))`, weight `round(2 * beta_sample(1 + agreed, 1 + disagreed, rng), 9)`; a judge with `dropped` true gets no entry. `rank.fit(..., weights=None)` multiplies each vote's `w` by `weights.get(judge, 1.0)`, skips a vote whose judge is absent from a non-None `weights`, and puts the weights in the result under `judge_weights`. The weights are part of what `seed_of` hashes, so the bootstrap stays deterministic.
- Produces: `rank.judge_trust` also returns `agreed` and `disagreed` per judge (owner answers first, then the other-judge majority where no owner answered).
- Produces: `daisugi rank judges [--data-dir D] [--json]`: exit 0; reading creates nothing; the text shows each judge's agreed and disagreed counts and its current sampled weight for the next ranking.

- [ ] **Step 1: Write the failing tests**

```python
def test_tau_b_handles_ties():
    assert rank.kendall_tau_b(["a", "b", "c"], ["a", "b", "c"]) == 1.0
    assert rank.kendall_tau_b(["a", "b", "c"], ["c", "b", "a"]) == -1.0
    assert rank.kendall_tau_b([["a", "b"], "c"], ["a", "b", "c"]) == pytest.approx(0.816496581)
    assert rank.kendall_tau_b(["a"], ["a"]) is None


def test_cyclic_magnitude_needs_a_loop():
    v = lambda i, j: rank.Vote(i=i, j=j, w=1.0, judge="j")
    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c")]) is None
    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c"), v("c", "a")]) == pytest.approx(1.0)
    assert rank.cyclic_magnitude([v("a", "b"), v("b", "c"), v("a", "c")]) == pytest.approx(0.0)


def test_a_judge_under_the_floor_is_dropped(tmp_path):
    dd = tmp_path / "dd"
    for n in range(5):
        comps = _cmp("j-bad", f"p{n}", "a", "b", "a", "a")
        f = _write(
            tmp_path / f"{n}.json",
            {**_doc([_att("a"), _att("b")], comps=comps), "ranking_id": f"r{n}"},
        )
        cid = json.loads(_run("choose", str(f), "--data-dir", str(dd), "--json").output)[
            "choice_id"
        ]
        assert _run("record", "pick", cid, "b", "--data-dir", str(dd)).exit_code == 0
    t = json.loads(_run("judges", "--data-dir", str(dd), "--json").output)["judges"]["j-bad"]
    assert t["cards"] == 5 and t["agree"] == 0.0 and t["dropped"] is True


def test_beta_sample_is_deterministic_and_in_range():
    a = rank.beta_sample(3, 2, rank.SplitMix64(7))
    assert a == rank.beta_sample(3, 2, rank.SplitMix64(7)) and 0.0 < a < 1.0
    assert rank.beta_sample(1, 1, rank.SplitMix64(7)) == rank.beta_sample(1, 1, rank.SplitMix64(7))


def test_weights_follow_the_posterior_and_replay_exactly():
    trust = {
        "good": {"agreed": 40, "disagreed": 2, "dropped": False},
        "poor": {"agreed": 6, "disagreed": 6, "dropped": False},
        "cut": {"agreed": 0, "disagreed": 5, "dropped": True},
    }
    w1 = rank.judge_weights(trust, "ab12")
    assert w1 == rank.judge_weights(trust, "ab12") and "cut" not in w1
    assert (
        w1["good"] > 1.4 and 0.0 < w1["poor"] < 2.0
    )  # 2 * Beta(41, 3) has mean 1.86; 2 * Beta(7, 7) has mean 1.0
    assert rank.judge_weights(trust, "cd34") != w1  # another ranking draws again


def test_fit_uses_weights_and_names_them():
    res = rank.fit(_doc_two_judges(), weights={"j1": 1.9, "j2": 0.1})
    assert res["judge_weights"] == {"j1": 1.9, "j2": 0.1}
    assert res["order"][0] == "a"  # j1 prefers a over b; j2 prefers b over a
```

The `Vote` field names are the ones `rank.Vote` has today; match them.

Run: `uv run --no-sync pytest -q tests/test_rank.py -k "tau or cyclic or floor"`
Expected: FAIL.

- [ ] **Step 2: Implement; wire the drop into `rank_judge`**

`rank_judge.judge_once` reads `judge_trust(data_dir)` once, turns it into `judge_weights(trust, seed_hex)` for the ranking and passes them to `rank.fit`; a judge with `dropped` true is skipped for `next` and the `why` says `judge J is dropped: agreement 0.0 over 5 answered cards`. The fit's `warnings` carry the same line.

- [ ] **Step 3: Run and commit**

Run: `uv run --no-sync pytest -q tests/test_rank.py tests/test_rank_judge.py`
Expected: green.

```bash
git add src/opendaisugi/rank.py src/opendaisugi/rank_judge.py src/opendaisugi/cli.py tests/test_rank.py
git commit -m "rank: judge trust from answered cards; judges weigh by a Thompson sample; a judge under the floor is dropped (RK-5, W3K-2)

Stage 9. Kendall tau-b and the cyclic magnitude are reported and never
used to drop; under 3 attempts the magnitude is not measurable, never 0.
Owner ruling A3 (2026-10-30): the vote weight is a seeded Thompson sample
from a Beta posterior per judge, not a fixed 1."
```

---

### Task 4b: Card decay is 14 days

Owner ruling A7 (2026-10-30): an unreviewed card closes as "kept" at the first of: switching needs a follow-up task, or **14 days** (was 7).

**Files:**
- Modify: `src/opendaisugi/rank.py:58` (`DECAY_SECONDS = 14 * 86400`), `clients/go/internal/rank/cards.go:23` (`const DecaySeconds = 14 * 86400`), `clients/rust/src/rank.rs:23` (`pub const DECAY_SECONDS: f64 = 14.0 * 86400.0;`)
- Modify: `clients/rank_cases.py` (the cases that open a card with `age_days=8` to test decay, lines near 571, 653 and 771 to 773, move to `age_days=15`; add `queue not yet decayed at 13 days` with `age_days=13` and `decays_in_days` 1), then regenerate `clients/fixtures/rank`
- Modify: `docs/feature-status.md` row 106 ("decay at follow-up-only or 14 days"), `docs/plans/2026-09-24-omarchy/design-ranking.md` where it says 7 days
- Test: `tests/test_rank.py`

- [ ] **Step 1: Write the failing test**

```python
def test_a_card_decays_after_fourteen_days_not_seven(tmp_path):
    card = _open_card(tmp_path, age_days=10)
    assert rank.decay(card, tmp_path, now=_now()) is None
    old = _open_card(tmp_path, age_days=15)
    assert rank.decay(old, tmp_path, now=_now())["how"] == "decay"
    assert rank.DECAY_SECONDS == 14 * 86400
```

Run: `uv run --no-sync pytest -q tests/test_rank.py -k fourteen`
Expected: FAIL (`DECAY_SECONDS` is 7 days).

- [ ] **Step 2: Change the three constants, the cases and the docs**

Edit the files above. Search for other hard-coded 7: `git grep -n -E '7 \* 86400|7\.0 \* 86400|decays? (in|at).*7' -- src clients docs/feature-status.md`.

- [ ] **Step 3: Regenerate the rank fixtures once and compare both ports**

Use the commands of Task 9 Step 1 and Step 2. Expected: only the decay cases change; 0 disagreements. (Task 9 adds its cases after this change; run this step's regeneration again there.)

- [ ] **Step 4: Run and commit**

Run: `uv run --no-sync pytest -q tests/test_rank.py`
Expected: green.

```bash
git add src/opendaisugi/rank.py clients/go/internal/rank/cards.go clients/rust/src/rank.rs clients/rank_cases.py clients/fixtures/rank docs/feature-status.md docs/plans/2026-09-24-omarchy/design-ranking.md tests/test_rank.py
git commit -m "rank: an unreviewed card decays after 14 days, not 7 (RK-10, owner ruling A7)

A week is short for a reviewer who looks at cards on weekends."
```

---

### Task 5: The pivot, `rank pivot`

**Files:**
- Create: `src/opendaisugi/rank_pivot.py`
- Modify: `src/opendaisugi/cli.py` (`rank pivot`; `rank record pick --apply` calls it after recording)
- Test: `tests/test_rank_pivot.py` (new)

**Interfaces:**
- Produces: `rank_pivot.plan(card: Card, data_dir: Path, to: str) -> dict`: `{"cost": cheap|costly|follow_up_only, "undo": [{"run_id", "step_id", "path"} for each reversible receipt, newest first], "skipped": [irreversible ones], "apply": {"kind": "worktree_patch", "from": alt.where.path, "onto": chosen.where.path} | {"kind": "output", "text": ...} | None, "follow_up": text | None}`. Uses `switch_cost(card, data_dir, to)` for `cost` and `Journal(data_dir).receipts_for_run` for the rows (the same runs RK-R-9 and RK-R-16 name).
- Produces: `rank_pivot.apply(p: dict, data_dir: Path, *, git=subprocess.run) -> dict`: for `cheap` and `costly`: `deeds.rollback_run(journal, run_id)` per run, then for a `worktree_patch`: `git -C FROM diff --binary HEAD` piped to `git -C ONTO apply --3way --index`; a non-zero exit stops with `{"applied": False, "why": ...}` and nothing more is attempted (`deeds.rollback_run` is atomic per file; a half-applied patch is reported, never hidden). For `follow_up_only`: `{"applied": False, "follow_up": text}`. It never runs a permanent step: the patch apply writes inside the chosen worktree only.
- Produces: `daisugi rank pivot --choice C --to X [--apply] [--data-dir D] [--json]`: without `--apply` prints the plan (exit 0); with `--apply` runs it (exit 0 applied, 1 refused or failed, 2 bad input). `rank record pick C X --apply` records the answer, then pivots. Only the operator runs it: it is under `rank`, and `rank pivot` with `--apply` joins `RANK_VERBS` in `owner_rule.py` so the warden denies an agent's shell line (the gate rule test in `tests/test_warden_rank_rule.py` gets one case).

- [ ] **Step 1: Write the failing tests**

```python
def test_plan_lists_the_reversible_receipts_newest_first(tmp_path):
    dd = _card_with_receipts(
        tmp_path, reversibility=["reversible", "none", "reversible"]
    )  # helper over rank_cases' receipts layout
    card = rank.read_cards(dd)[0]
    p = rank_pivot.plan(card, dd, "b")
    assert (
        p["cost"] == "costly"
        and [u["step_id"] for u in p["undo"]] == ["s3", "s1"]
        and p["skipped"] == []
    )


def test_follow_up_only_applies_nothing(tmp_path):
    dd = _card_with_receipts(tmp_path, reversibility=["irreversible"])
    card = rank.read_cards(dd)[0]
    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd)
    assert out["applied"] is False and "follow-up" in out["follow_up"]


def test_apply_stops_at_the_first_failure_and_says_so(tmp_path):
    dd = _two_worktrees_card(tmp_path)  # chosen and alternative are two worktrees of one repo
    card = rank.read_cards(dd)[0]
    calls = []

    def git(argv, **kw):
        calls.append(argv)

        class R:
            returncode = 1 if "apply" in argv else 0
            stdout = b""
            stderr = b"conflict"

        return R()

    out = rank_pivot.apply(rank_pivot.plan(card, dd, "b"), dd, git=git)
    assert out["applied"] is False and "conflict" in out["why"] and any("apply" in c for c in calls)


def test_the_warden_denies_an_agent_pivot():
    from opendaisugi.rank_rule import names_rank_record

    assert names_rank_record("daisugi rank pivot --choice ch_1 --to b --apply")
    assert not names_rank_record("daisugi rank pivot --choice ch_1 --to b")
```

Run: `uv run --no-sync pytest -q tests/test_rank_pivot.py`
Expected: FAIL.

- [ ] **Step 2: Implement; run; commit**

Run: `uv run --no-sync pytest -q tests/test_rank_pivot.py tests/test_rank.py tests/test_warden_rank_rule.py`
Expected: green.

```bash
git add src/opendaisugi/rank_pivot.py src/opendaisugi/cli.py src/opendaisugi/owner_rule.py tests/test_rank_pivot.py tests/test_warden_rank_rule.py
git commit -m "rank: the pivot, undo through the ledger and apply the alternative's patch, or print the follow-up (W3K-4)

Stage 8. A pivot undoes only receipts the ledger marks reversible and
applies a patch only inside the chosen worktree; follow_up_only changes
nothing. The warden denies an agent's rank pivot --apply."
```

---

### Task 6: `gardener/ab_test.py` judge fallback fails closed

**Files:**
- Modify: `src/opendaisugi/gardener/ab_test.py`
- Test: `tests/test_ab_test.py` (existing; add two tests; if the file has another name, use it)

**Interfaces:**
- Produces: `ABResult.judge_error: str | None = None`; `passed` is False when `judge_error` is set, whatever the structural result. The `except` around `judge` sets `judge_error = f"{type(e).__name__}: {e}"` and appends a note; it no longer lets the structural verdict stand.

- [ ] **Step 1: Failing test**

```python
async def test_a_judge_that_raises_fails_the_ab(pathway, task, gen):
    async def judge(a, b):
        raise RuntimeError("judge down")

    r = await ab_test(pathway, task, tier2_generator=gen, judge=judge)
    assert (
        r.judge_error == "RuntimeError: judge down"
        and r.passed is False
        and r.judge_verdict is None
    )
```

Run: `uv run --no-sync pytest -q tests/test_ab_test.py`
Expected: FAIL (`passed` is True today when structural matches).

- [ ] **Step 2: Fix; run; commit**

```bash
git add src/opendaisugi/gardener/ab_test.py tests/test_ab_test.py
git commit -m "gardener: a judge that raises fails the A/B instead of letting the structural verdict stand (W3K-15)

design-ranking.md, Conflicts found: the fallback was fail-open. rank
treats a judge error as no vote; the A/B now treats it as no pass."
```

---

### Task 7: Go: `rank show`, `judge`, `judges`, `pivot`

**Files:**
- Modify: `clients/go/internal/rank/rank.go` (`FamilyOf`, `KendallTauB`, `CyclicMagnitude`, `BetaSample`, `JudgeWeights`, `Fit` with weights), `cards.go` (`ShowDoc`, `JudgeTrust`), new `judge.go` (`Prompt`, `ParseVerdict`, `Artifact`, `JudgeOnce`), new `pivot.go` (`Plan`, `Apply`)
- Modify: `clients/go/internal/cli/rankcmd.go` (the four verbs), `clients/go/internal/warden/` owner-rule verbs (`rank pivot --apply`)
- Consumes: `clients/go/internal/llm` (the complete call the garden and tree code use), `internal/agentprof` (profiles), `internal/deeds` (`RollbackRun`), `internal/tracejournal` (receipts)
- Test: `clients/go/internal/rank/judge_test.go`, `pivot_test.go`, `rank_test.go`

- [ ] **Step 1: Port with the same tests** (the Python tests of Tasks 2 to 5 rewritten as Go table tests; the prompt digest test pins the same hex as Task 3; `BetaSample` uses `uint64` shifts and `float64(x>>11) / (1<<53)`, then the same 9-digit round, and a test pins the Python value of `beta_sample(3, 2, SplitMix64(7))` to the last digit)

- [ ] **Step 2: Build and test under the cap**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./internal/rank/ ./internal/cli/ ./internal/warden/'
```
Expected: ok.

- [ ] **Step 3: Commit**

```bash
git add clients/go/internal/rank clients/go/internal/cli/rankcmd.go clients/go/internal/warden
git commit -m "go: rank show, judge, judges and pivot, the same bytes and verdicts as the oracle"
```

---

### Task 8: Rust: the same four verbs

**Files:**
- Modify: `clients/rust/src/rank.rs` (split into `rank/mod.rs`, `rank/judge.rs`, `rank/pivot.rs` if it passes 2,500 lines), `clients/rust/src/cli/rankcmd.rs`, the warden owner-rule verbs
- Consumes: `clients/rust/src/llm`, `agentprof.rs`, `deeds.rs`, `tracejournal/`
- Test: unit tests in the modules

- [ ] **Step 1: Port (including `beta_sample` and `judge_weights`, pinned to the Python value of `beta_sample(3, 2, SplitMix64(7))`); build and test**

```bash
. clients/rust/tools/z3-build-env.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path clients/rust/Cargo.toml rank::
```
Expected: ok.

- [ ] **Step 2: Commit**

```bash
git add clients/rust/src/rank.rs clients/rust/src/rank clients/rust/src/cli/rankcmd.rs clients/rust/src/warden
git commit -m "rust: rank show, judge, judges and pivot, the same bytes and verdicts as the oracle"
```

---

### Task 9: Rank golden cases for the four verbs; compare

**Files:**
- Modify: `clients/rank_cases.py` (new cases; the fake model server for `judge`; a `worktrees` tree entry: two git repos laid out by `git init` plus `git worktree add`, for `pivot` and the `--diff` of Task 10)
- Modify: `clients/rank_compare.py` (nothing unless a new tree kind needs a normalizer: worktree paths read as `<wt>`)
- Modify: `docs/spec/conformance.md` ("rank cases" paragraph: the new verbs and the count)
- Modify: `clients/ADJUDICATIONS.md` (RK-R-17 to RK-R-21: the commands, the family rule, the trust floor and the Thompson-sampled weight (A3), the pivot, the harness)

- [ ] **Step 1: Add the cases**

| name | command | what it checks |
|---|---|---|
| `show by ranking`, `show by choice`, `show two selectors`, `show missing` | `rank show ...` | Task 2's four outcomes; reading creates nothing |
| `judge records a pair` | `rank judge a.json --data-dir .opendaisugi --json` with the fake server answering `A` then `B` | two rows, one `pair_id`, exit 0, the request bodies' digests logged by the fake |
| `judge second call errors` | the fake answers `A` then status 500 | no rows, exit 1 |
| `judge unparsable` | `A` then `A or B` | no rows, exit 3 |
| `judge same family` | author `judge-9b` | no call, exit 3 |
| `judge no author` | author `""` | no call, exit 3 |
| `judge loop exhausts` | `--loop` with one judge | one comparison, `stop` `judges_exhausted` |
| `judge dropped judge` | five answered cards against `j-bad` laid out, then `judge` | no call, `why` names the drop |
| `judges empty`, `judges five cards` | `rank judges --json` | the trust doc |
| `judges weights replay`, `judges weights differ by ranking`, `fit with weights` | `rank judges --json`; `rank show` of two rankings with the same journal; `rank choose` with a judge set | the sampled weights are equal on a second run and differ across ranking ids; the weights print in the result under `judge_weights`; all three ports agree to the last digit (A3) |
| `pivot plan costly`, `pivot follow up only`, `pivot apply patch`, `pivot apply conflict` | `rank pivot ...` over the `worktrees` entry | the plan and the apply outcomes; the tree after shows the applied file |
| `pick apply` | `rank record pick C b --apply` | the answer row, then the pivot |

The fake server's base URL goes into the case's `env` as `OPENAI_BASE_URL` (the `local` profile is an OpenAI wire); the profile table is laid out in the case HOME's `config.yaml` under `agent_profiles` with an empty key, as `tree_cases.py` lays out profiles.

```bash
DAISUGI_RANK_SCRATCH=/mnt/tera/.tt/w3-rank-tree-branch/rank \
  uv run --no-sync python clients/rank_cases.py --out clients/fixtures/rank
```
Expected: `cases.jsonl` count 135 + 19; the manifest's sha256 changes; `clients/fixture_paths.py` finds no local path.

- [ ] **Step 2: Compare both ports (one at a time)**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/rank_compare.py --binary clients/go/daisugi --oracle --max-refused 0
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/rank_compare.py --binary clients/rust/target/release/daisugi --max-refused 0
```
Expected: 0 disagreements, 0 refused, both. A difference is adjudicated before the next task: oracle first, then the ports.

- [ ] **Step 3: Commit**

```bash
git add clients/rank_cases.py clients/rank_compare.py clients/fixtures/rank docs/spec/conformance.md clients/ADJUDICATIONS.md
git commit -m "rank cases: show, judge over the fake model server, judges, pivot over two worktrees; Go and Rust agree (RK-R-17 to RK-R-21)"
```

---

### Task 10: coppice (Go): the arena, a `cards` view with the operator-only answer

**Files:**
- Create: `harness/coppice/plugins/cards/{manifest.json,index.html,cards.js,cards.css,_tests/cards.test.mjs}`
- Modify: `harness/coppice/plugins/embed.go` (add `cards` to the embed line), `plugins/README.md` (the view table; the post-back list says four plus `answer`)
- Modify: `harness/coppice/plugins/_lib/view.js` (`answer(choice_id, outcome, pick)` post; the `coppice.data` message carries `cards`)
- Modify: `harness/coppice/internal/web/static/views.js` (`viewAction` accepts `{type:'answer', choice_id, outcome, pick}` only for a card id in the last `cards` list the page sent; the floor page fetches `/api/cards` every 30 s while a view is open and posts it), `floor.js` (the `answer` action calls `POST /api/cards/answer`), `garden.js` (a count badge at the trunk: `N to review`, tapping opens the `cards` view), `static/_tests/views.test.mjs`, `garden.test.mjs`
- Modify: `harness/coppice/internal/web/views.go` (`GET /api/cards`, `GET /api/cards/{id}/diff`), new `internal/web/cards.go` (`POST /api/cards/answer` under `guardAnswer`; the `daisugi rank` child runs with W3K-5's data dir and binary; a 10 s timeout; output capped 1 MiB), `internal/web/cards_test.go`, `views_test.go`
- Modify: `harness/coppice/PROTOCOL.md` (a "Cards" paragraph under Plugins: the data kind, the post-back, who may answer), `README.md`

**Interfaces:**
- Produces: `GET /api/cards?sort=reversibility|impact|date|project` -> the JSON of `daisugi rank queue --json --sort S` plus `{"source": "daisugi rank queue", "data_dir": "<as passed or 'default'>"}`; guarded like `/api/tasks`; a child failure answers 502 `{"error": "daisugi rank queue failed: <first line>"}`.
- Produces: `GET /api/cards/{id}/diff` -> `daisugi rank show --choice ID --diff --worktrees-under <task worktree root> --json`; the view shows the unified text.
- Produces: `POST /api/cards/answer` body `{choice_id, outcome: "keep"|"drop"|"switch", pick}`: `keep` and `drop` run `rank record drop ID`; `switch` runs `rank record pick ID PICK`; the reply is the command's JSON; a pane or an unplaced peer gets 403 `PaneRefusal`; an unknown id gets the command's exit 1 text as 404.
- Produces: the view. Level one ("see no code"): the headline, the why, the switch cost line, `decays in N days`, tests as a count, cost, eliminated attempts folded one line each; three buttons Keep, Switch to <id> (one per alternative), Drop. Level two ("see the code"): the diff. One switch between them. Sort keys from the design. Under 600 px the card is the overlay's whole width (the overlay model already slides a view over the floor).

- [ ] **Step 1: Write the failing JS tests**

```js
// static/_tests/views.test.mjs (add)
test('answer post-back is accepted only for a card the page sent', () => {
  const cards = [{ choice_id: 'ch_1' }];
  assert.deepEqual(viewAction({ type: 'answer', choice_id: 'ch_1', outcome: 'drop' }, [], cards),
    { kind: 'answer', choice_id: 'ch_1', outcome: 'drop', pick: '' });
  assert.equal(viewAction({ type: 'answer', choice_id: 'ch_9', outcome: 'drop' }, [], cards), null);
  assert.equal(viewAction({ type: 'answer', choice_id: 'ch_1', outcome: 'burn' }, [], cards), null);
  assert.equal(viewAction({ type: 'answer', choice_id: 'ch_1', outcome: 'switch' }, [], cards), null); // switch needs pick
});
// plugins/cards/_tests/cards.test.mjs
test('a card renders its headline, why, switch cost and buttons, no code at level one', () => { ... });
test('level two shows the diff text the page posted', () => { ... });
test('sort by impact puts the widest diff gap first', () => { ... });
```

Run: `(cd harness/coppice && go test ./internal/web/ -run TestJS)`
Expected: FAIL.

- [ ] **Step 2: Write the failing Go tests**

```go
func TestCardsRouteRunsRankQueueAndNeverWrites(t *testing.T) {
	// a fake daisugi on PATH that prints a fixed queue JSON and records argv;
	// GET /api/cards -> 200, body holds "cards"; argv holds "rank","queue","--json","--data-dir",<dir>;
	// the data dir has no new file after the call
}
func TestCardsAnswerRefusesAPane(t *testing.T) {
	// the same placement fake ask_test.go uses for /api/ask/answer: a hello that places the peer as a pane
	// POST /api/cards/answer -> 403 and the fake daisugi recorded no call
}
func TestCardsAnswerRecordsADropAndASwitch(t *testing.T) {
	// operator client: drop -> argv ends "record","drop","ch_1"; switch with pick b -> "record","pick","ch_1","b"
}
func TestCardsDiffPassesTheWorktreeRoot(t *testing.T) { ... }
```

Run: `(cd harness/coppice && go test ./internal/web/ -run 'TestCards')`
Expected: FAIL.

- [ ] **Step 3: Implement; run the web package and the e2e web script**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd harness/coppice && go vet ./... && go test -race ./internal/web/ ./internal/server/ -run "TestCards|TestJS|TestViews|TestGarden"'
COPPICE_SOCKET=/mnt/tera/.tt/w3-rank-tree-branch/coppice/e2e.sock COPPICE_DATA_DIR=/mnt/tera/.tt/w3-rank-tree-branch/coppice/e2e harness/coppice/e2e/web.sh
```
Expected: ok; e2e 7/7 (plus the one this task adds: open the cards view, see "No open cards.").

- [ ] **Step 4: Commit (two commits: the routes, then the view)**

```bash
git add harness/coppice/internal/web/cards.go harness/coppice/internal/web/views.go harness/coppice/internal/web/cards_test.go harness/coppice/internal/web/views_test.go harness/coppice/PROTOCOL.md
git commit -m "coppice web: the cards queue and diff routes, and the operator-only answer route (W3K-5, W3K-6)

The arena of RK-7 and ST-5 needs two things a view cannot do: data that
is not panes, tasks or events, and a post-back that records an answer. The
answer runs under the same guard and placement as /api/ask/answer, so a
pane cannot answer its own card."
git add harness/coppice/plugins/cards harness/coppice/plugins/embed.go harness/coppice/plugins/_lib/view.js harness/coppice/plugins/README.md harness/coppice/internal/web/static
git commit -m "coppice: the cards view, two drill-down levels, four sorts, the garden's review badge"
```

---

### Task 11: coppice-rs: the same routes; the protocol corpus

**Files:**
- Modify: `harness/coppice-rs/src/web/mod.rs` (routes), new `src/web/cards.rs`, `src/web/statics.rs` (the embed list comes from `build.rs`, which reads `harness/coppice/plugins/embed.go`; nothing to add by hand, check the test `statics` still passes byte for byte)
- Modify: `harness/coppice-rs/cases/web/*.jsonl` (cases for the three routes with the fake daisugi), `harness/coppice/testdata/protocol/` (no new verb: the cards travel over HTTP, not the socket)
- Test: `cargo test --release` web tests; `clients/coppice_compare.py`

- [ ] **Step 1: Port; test; compare**

```bash
CARGO_TARGET_DIR=/mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path harness/coppice-rs/Cargo.toml web::
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust /mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice/release/coppice --only web --max-refused 0
```
Expected: ok; the web slice agrees.

- [ ] **Step 2: Commit**

```bash
git add harness/coppice-rs/src/web harness/coppice-rs/cases/web
git commit -m "coppice-rs: the cards routes, the same guard and placement as Go; web cases agree"
```

---

### Task 12: The adversarial session-binding test in coppice

**Files:**
- Test: `harness/coppice/internal/server/warden_session_test.go` (new), `harness/coppice-rs/src/server/tests.rs` (the twin)
- Modify (only if the test finds a hole): the pane start path that writes the warden hook

**Interfaces:**
- Produces: a test that starts two headless panes A and B under one warden root in scratch, registers an envelope for a session `sA` and none for B, and sends from pane B a warden check whose payload names `session_id: sA` (the shape the harness's hook payload uses). Expected: B's call is decided under `default` (an unpinned root) or denied (a pinned root), never under `sA`'s envelope. The check runs the real `daisugi` binary built in Task 1 against the root (`daisugi warden check --root <scratch root> --format claude`), as the fence's hook line does; no model runs.

- [ ] **Step 1: Write the test, run it**

```bash
(cd harness/coppice && go test -race ./internal/server/ -run TestAPaneCannotNameAnotherPanesSession)
```
Expected: PASS by SEC-3 (the payload never selects an envelope). If it FAILS, stop: this is a blocker on master today, with no tree at all; fix it in the oracle's `warden.load_envelope` path first, mirror it in Go and Rust, add a warden case, and only then go on.

- [ ] **Step 2: Add the Rust twin; commit**

```bash
git add harness/coppice/internal/server/warden_session_test.go harness/coppice-rs/src/server/tests.rs
git commit -m "coppice: the session-binding adversarial test the delegation tree's step 4 waits on (AT-1)

A pane that names another pane's session in its hook payload never gets
that session's envelope. design-delegation-tree.md asked for this test
before any envelope goes on a task."
```

---

### Task 13: coppice (Go): an envelope on a task, proved on `task.create`, pinned on `pane.create`

**Files:**
- Modify: `harness/coppice/internal/layout/layout.go` (`Task.Envelope json.RawMessage` and `Task.Session string`, both `omitempty`)
- Modify: `harness/coppice/internal/server/tasks.go` (`task.create` takes `envelope` (an object) or `project` (a name: the server reads `<config dir>/envelopes/<project>.json` as the steward's spawn does, `steward_work.go` `readProjectEnvelope`); with `parent` whose task has an envelope, the server runs `daisugi tree check` with the parent's envelope and the child's; a refusal is `bad_request` with the reasons joined; with no parent, only the operator may give an envelope (`unauthorized`, `only the operator makes a root task.`))
- Modify: `harness/coppice/internal/server/panes.go` (`pane.create` and `pane.split` into a task with an envelope: mint `t<id>-<16 hex>`, run `daisugi tree root --session S` for a root task or `daisugi tree spawn --parent <parent task's session> --session S` for a child task (the proof again, at registration, TR-R-8), then start the pane with `--session S` in its warden hook; a failure to register refuses the start with the daisugi line; a pane connection may create or split only into its own task or a child of it, W3K-12)
- Modify: `harness/coppice/internal/server/role.go` (`TaskRefusal`), `PROTOCOL.md` (Tasks: the two new params and the rule; Roles: the new refusal)
- Test: `harness/coppice/internal/server/tasks_test.go`, `panes_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestTaskCreateProvesTheEdgeAgainstTheParent(t *testing.T) {
	// operator: task.create label=root envelope={shell_allowlist:[git,ls]} -> ok
	// task.create label=child parent=t1 envelope={shell_allowlist:[git,ls,rm]} -> bad_request, message holds "shell head rm"
	// task.create label=child parent=t1 envelope={shell_allowlist:[git]} -> ok, row carries envelope
}
func TestOnlyTheOperatorMakesARootTaskWithAnEnvelope(t *testing.T) { /* pane hello -> unauthorized */ }
func TestAPaneStartsPanesOnlyUnderItsOwnTask(t *testing.T) {
	// pane P in task t2 (child of t1): pane.create task=t3 (a sibling) -> unauthorized "a pane starts panes only under its own task."
	// pane.create task=t2 -> ok; task=t4 (child of t2) -> ok
}
func TestAPinnedPaneNeverReadsDefault(t *testing.T) {
	// a pane created into t2 carries --session t2-<hex> in its hook argv (read the saved wrapped command);
	// daisugi warden check with that session and a call the task envelope denies -> deny;
	// the same call with no session under the same root -> the default envelope's verdict (differs)
}
```

Run: `(cd harness/coppice && go test -race ./internal/server/ -run 'TestTaskCreate|TestOnlyTheOperator|TestAPane')`
Expected: FAIL.

- [ ] **Step 2: Implement; run the server package under the cap**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd harness/coppice && go test -race -timeout 40m ./internal/server/'
```
Expected: ok (the package takes about 500 s on this box, ledger 2026-10-21).

- [ ] **Step 3: Commit**

```bash
git add harness/coppice/internal/layout/layout.go harness/coppice/internal/server/tasks.go harness/coppice/internal/server/panes.go harness/coppice/internal/server/role.go harness/coppice/internal/server/tasks_test.go harness/coppice/internal/server/panes_test.go harness/coppice/PROTOCOL.md
git commit -m "coppice: an envelope on a task, proved against the parent task on create and registered under a minted session before a pane starts (step 4, W3K-12)

design-delegation-tree.md Option 3. coppice asks daisugi (tree check,
tree root, tree spawn) and decides nothing (ADR-0020). A pane connection
starts panes only under its own task, so its children inherit at most
its own envelope. Depends on AT-1 (confirmed 2026-09-30)."
```

---

### Task 14: coppice-rs: the task envelope and the pin; protocol corpus cases

**Files:**
- Modify: `harness/coppice-rs/src/layout.rs` (`Task.envelope`, `Task.session`), `src/server/tasks.rs`, `src/server/` pane start path, the role refusal
- Modify: `harness/coppice/testdata/protocol/tasks.jsonl` (or the file that holds the task verbs: new frames for `task.create` with `envelope`, the refused edge, the root refusal for a pane, `pane.create` into a task and the refused sibling), with the fake daisugi the protocol corpus already drives for the fence
- Test: `cargo test --release` server tests; `clients/coppice_compare.py --only protocol`

- [ ] **Step 1: Port; test; compare; commit**

```bash
CARGO_TARGET_DIR=/mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path harness/coppice-rs/Cargo.toml server::tasks
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust /mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice/release/coppice --only protocol --max-refused 0
git add harness/coppice-rs/src harness/coppice/testdata/protocol
git commit -m "coppice-rs: the task envelope, the proof on create, the pin on pane start; protocol cases agree"
```

---

### Task 15: The delegation tree and its asks on the floor

**Files:**
- Modify: `harness/coppice/internal/web/views.go` (`GET /api/tree` -> `daisugi tree status --json`), new `internal/web/tree.go` (`POST /api/tree/answer` under `guardAnswer` and `placeAllows` -> `daisugi tree answer ASK allow|deny`), tests
- Modify: `harness/coppice/plugins/tree/tree.js`, `graph.js` (under a worker's task row: its tree nodes with `tokens_used/reserved`, `over by N`, `proved` or `not proved against its parent`, `state`; an ask row with Allow and Deny buttons that post `{type:'tree-answer', ask_id, decision}`), `plugins/_lib/view.js` (the `tree` data kind and the post), `static/views.js` (`viewAction` accepts `tree-answer` only for an ask id in the last tree the page sent), `floor.js`, `_tests`
- Modify: `harness/coppice-rs/src/web/` (the two routes), `cases/web/`
- Modify: `PROTOCOL.md` (Plugins: the `tree` data kind)

**Interfaces:**
- Produces: `GET /api/tree` -> the JSON of `daisugi tree status --json` (W3K-5 data dir), plus `{"source": "daisugi tree status"}`; `POST /api/tree/answer {ask_id, decision: "allow"|"deny", reason}` -> the command's JSON; a pane is refused (403). An allowed child is marked not proved and must still fit its branch's root (TR-R-7); the view says so on the node.

- [ ] **Step 1: Failing tests (Go route tests as Task 10's; JS tests for the rows and the post-back whitelist; Rust twins)**
- [ ] **Step 2: Implement both binaries; run; compare `--only web`; commit**

```bash
git add harness/coppice/internal/web/tree.go harness/coppice/internal/web/views.go harness/coppice/plugins/tree harness/coppice/plugins/_lib/view.js harness/coppice/internal/web/static harness/coppice-rs/src/web harness/coppice-rs/cases/web harness/coppice/PROTOCOL.md
git commit -m "coppice: the delegation tree's nodes, budgets and asks on the tree view; the operator answers a tree ask from the floor

Roadmap section 7, 'coppice shows the tree and the asks that need a
person'. The fourth refused proposal (TR-R-7) reaches the floor as an ask
row; only the operator answers it."
```

---

### Task 16: Branching in the oracle: `tree run` with `attempts`

**Files:**
- Create: `src/opendaisugi/tree_branch.py` (`make_worktree`, `remove_worktree`, `branch_hash`, `run_checks`, `attempts_doc`, `run_branches`, `prune_branches`)
- Modify: `src/opendaisugi/tree_run.py` (`read_request` takes `attempts`, `why`; `run_node` with `attempts > 1` calls `tree_branch.run_branches`), `src/opendaisugi/tree.py` (`spawn` takes `branch_of: str | None`, `why: str | None`, written on the row; `status_doc` groups rows that share `branch_of` under a `branches` key with their total tokens and the `why`; `_fits` accepts the two fields), `src/opendaisugi/cli.py` (`tree run --parallel K --check NAME=ARGV --judge J`; new `tree prune-branches [--data-dir D] [--json]`; `TREE_VERBS` gains `prune-branches`)
- Test: `tests/test_tree_branch.py` (new), `tests/test_tree_run.py`, `tests/test_tree.py`

**Interfaces:**
- Produces: `read_request` accepts `attempts` (int 2 to 8; 1 or absent is the single child) and `why` (W3K-8); refusals: `attempts must be a whole number from 2 to 8.`, `why must be one of failed_verify, failed_tests, low_confidence, operator.`
- Produces: `tree_branch.make_worktree(cwd: str, data_dir: Path, session: str, *, git=subprocess.run) -> Path`: `git -C CWD rev-parse --show-toplevel` (a failure refuses with W3K-7's line), then `git -C TOP worktree add --detach <data>/tree/branches/<session> HEAD`; the dir mode 0700.
- Produces: `tree_branch.branch_hash(worktree: Path, answer: str) -> str` (W3K-10).
- Produces: `tree_branch.run_checks(checks: list[dict], worktree: Path, env: dict, timeout_s: int) -> list[dict]`: `[{"name", "required", "result": "pass"|"fail"|"not_run"}]`, W3K-11; a check that times out is `fail`.
- Produces: `tree_branch.run_branches(data_dir, gate_root, *, parent, envelope, profiles, profile_name, model, tokens, turns, parent_tokens_used, parent_turns_used, task, tools, max_turns, cwd, timeout_s, sprig_binary, environ, attempts, why, parallel, checks, judge, fence, start=tree_run.run_node) -> BranchRun`: in order: (1) `make_worktree` for each branch, sessions `mint_session(parent, n)` in order (a failure removes what was made and refuses); (2) reserve all N under the lock (`tree.spawn` N times with `branch_of = ranking_id`, `why`; the budget check is N times B, W3K-9; a refusal ends nothing started and removes the worktrees); (3) start up to `parallel` children at once with `start(...)`, each with `cwd` its worktree and `session` its minted session; (4) after all end, run the checks per branch, build the attempts doc (`id` = session, `content_hash` = `branch_hash`, `author` = the child's model, `tests` = `[{"name": "the child ran", "required": True, "result": pass|fail}] + checks`, `cost` = `{"tokens": used (measured), "wall_ms": measured, "diff_lines": measured}`, `where` = `{"kind": "worktree", "path": str(worktree)}`, `edge_proof` `ok`, `started_by_parent` True), `ranking_id` = `br_` + first 12 hex of SHA-256 over `[parent, task, [sessions]]`, `project` = the git top, `policy.judges` = `[judge]` when given; (5) `rank.fit`; with `judge`, `rank_judge.judge_once` in a loop while `next` is not null and a vote was recorded; (6) `rank` choose: the `opened` row through `rank.opened_row` + `append_rows` (as weave does), `facts.run` = `{"run_id": ranking_id, "downstream": []}`; (7) `end` rows in branch order (W3K-13). Returns `BranchRun(status, leader_session, answer, choice_id, ranking, branches=[{session, status, worktree, tokens_used, turns_used, tests}], reasons)`. `status` is `ran` when a leader exists, `failed` when `none_survived` (the reply's `reasons` hold each branch's elimination), `refused` on a refused reservation or worktree.
- Produces: `tree run` reply gains `branches`, `leader`, `choice_id`, `ranking` (the fit) when `attempts > 1`; the top-level `answer` and `session` are the leader's. Exit codes as MX-R-5.
- Produces: `daisugi tree prune-branches`: removes `<data>/tree/branches/<session>` (`git worktree remove --force`) for every session whose card is closed (an `opened` row with a close row) or whose ranking has no card; prints the list; `--json`. An open card's worktrees stay.
- Produces: `tree status` text shows a branch group as `  branches of <ranking_id> (<why>): N ran, M eliminated, leader S, T tokens`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_tree_branch.py
import json, subprocess
from pathlib import Path
import pytest
from opendaisugi import rank, tree, tree_branch, tree_run
from tests.test_tree_run import _env, _root  # the helpers that file defines


def _repo(tmp_path):
    r = tmp_path / "repo"
    r.mkdir()
    subprocess.run(["git", "init", "-q", str(r)], check=True)
    (r / "a.txt").write_text("one\n")
    subprocess.run(["git", "-C", str(r), "add", "a.txt"], check=True)
    subprocess.run(
        ["git", "-C", str(r), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
        check=True,
    )
    return r


def test_read_request_takes_attempts_and_why():
    doc = tree_run.read_request(
        json.dumps(
            {"task": "t", "envelope": {}, "tools": [], "attempts": 3, "why": "failed_tests"}
        ).encode()
    )
    assert doc["attempts"] == 3
    with pytest.raises(ValueError, match="2 to 8"):
        tree_run.read_request(
            json.dumps({"task": "t", "envelope": {}, "tools": [], "attempts": 9}).encode()
        )
    with pytest.raises(ValueError, match="why must be one of"):
        tree_run.read_request(
            json.dumps({"task": "t", "envelope": {}, "tools": [], "attempts": 2}).encode()
        )


def test_branching_needs_a_git_workspace(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    out = tree_branch.run_branches(
        d,
        g,
        parent="top",
        envelope=_env(),
        profiles=None,
        profile_name=None,
        model=None,
        tokens=100,
        turns=2,
        parent_tokens_used=0,
        parent_turns_used=0,
        task="t",
        tools=[],
        max_turns=None,
        cwd=str(tmp_path),
        timeout_s=5,
        sprig_binary="sprig",
        environ={},
        attempts=2,
        why="operator",
        parallel=2,
        checks=[],
        judge=None,
        fence=None,
    )
    assert out.status == "refused" and "needs a git workspace" in out.reasons[0]
    assert not (d / "tree" / "branches").exists()


def test_branches_reserve_all_or_none(tmp_path):
    d, g = _root(tmp_path, tokens=150, turns=10)  # two branches of 100 do not fit
    repo = _repo(tmp_path)
    started = []
    out = tree_branch.run_branches(
        d,
        g,
        parent="top",
        envelope=_env(),
        profiles=None,
        profile_name=None,
        model=None,
        tokens=100,
        turns=2,
        parent_tokens_used=0,
        parent_turns_used=0,
        task="t",
        tools=[],
        max_turns=None,
        cwd=str(repo),
        timeout_s=5,
        sprig_binary="sprig",
        environ={},
        attempts=2,
        why="operator",
        parallel=2,
        checks=[],
        judge=None,
        fence=None,
        start=lambda *a, **k: started.append(k) or None,
    )
    assert out.status == "refused" and "2 branches of 100 each need 200" in out.reasons[0]
    assert started == [] and not (d / "tree" / "branches").exists()
    assert [r for r in tree.read_tree(d).nodes if r != "top"] == []


def test_branches_run_in_parallel_rank_and_open_a_card(tmp_path):
    d, g = _root(tmp_path, tokens=1000, turns=10)
    repo = _repo(tmp_path)
    live, peak = [], [0]

    def start(data_dir, gate_root, *, session, cwd, **k):
        live.append(session)
        peak[0] = max(peak[0], len(live))
        (Path(cwd) / "a.txt").write_text(f"{session}\n")  # each branch edits its own worktree
        live.remove(session)
        ok = session.endswith("1") or session.endswith("2")  # the fake: branch 3 fails
        return tree_run.NodeRun(
            "ran" if ok else "failed",
            session,
            answer=f"answer {session}",
            tokens_used=10,
            turns_used=1,
        )

    out = tree_branch.run_branches(
        d,
        g,
        parent="top",
        envelope=_env(),
        profiles=None,
        profile_name=None,
        model="m",
        tokens=100,
        turns=2,
        parent_tokens_used=0,
        parent_turns_used=0,
        task="t",
        tools=[],
        max_turns=None,
        cwd=str(repo),
        timeout_s=5,
        sprig_binary="sprig",
        environ={},
        attempts=3,
        why="failed_tests",
        parallel=3,
        checks=[],
        judge=None,
        fence=None,
        start=start,
    )
    assert out.status == "ran" and out.leader_session in {b["session"] for b in out.branches}
    assert len(out.branches) == 3 and [b["status"] for b in out.branches].count("failed") == 1
    assert out.ranking["eliminated"][0]["reasons"] == ["required test failed: the child ran"]
    cards = rank.read_cards(d)
    assert len(cards) == 1 and cards[0].id == out.choice_id
    t = tree.read_tree(d)
    assert all(t.nodes[b["session"]].state == "ended" for b in out.branches)
    rows = [json.loads(l) for l in (d / "tree" / "ledger.jsonl").read_text().splitlines()]
    assert [r["session"] for r in rows if r["kind"] == "end"] == [
        b["session"] for b in out.branches
    ]  # W3K-13


def test_a_required_check_eliminates(tmp_path):
    # checks=[{"name": "tests", "argv": ["sh", "-c", "grep -q 2 a.txt"], "required": True}] -> only the branch whose a.txt holds 2 survives
    ...


def test_prune_removes_only_closed_cards_worktrees(tmp_path): ...
```

`NodeRun`'s field names are the ones `tree_run.NodeRun` has; match them. The ledger's row `kind` key is whatever `tree.py` writes today (`_fits` names it); match it.

Run: `uv run --no-sync pytest -q tests/test_tree_branch.py`
Expected: FAIL.

- [ ] **Step 2: Implement; run the tree suites and the warden rule test for the new verb**

Run: `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync pytest -q tests/test_tree_branch.py tests/test_tree_run.py tests/test_tree.py tests/test_warden_tree_rule.py tests/test_rank.py`
Expected: green; `names_tree_write("daisugi tree prune-branches")` is True.

- [ ] **Step 3: Commit (two commits)**

```bash
git add src/opendaisugi/tree.py src/opendaisugi/tree_run.py tests/test_tree.py tests/test_tree_run.py
git commit -m "tree: spawn rows carry branch_of and why; the request takes attempts and why (W3K-8)"
git add src/opendaisugi/tree_branch.py src/opendaisugi/cli.py src/opendaisugi/owner_rule.py tests/test_tree_branch.py tests/test_warden_tree_rule.py
git commit -m "tree run: branching, N children in parallel in their own worktrees, ranked by rank, the leader kept, a card opened (W3K-7, W3K-9 to W3K-13)

design-sprig.md opinion 4 and the owner's 2026-09-28 word. Every branch
reserves its budget before any starts; a branch that did not run, or
fails a required check, is eliminated; the parent proceeds on the
leader and the owner sees the choice later (RK-10). Retires MX-R-10.
Depends on provisional RK-1 and RK-8."
```

---

### Task 17: Branch worktree cleanup from the floor, and the steward's `status`

**Files:**
- Modify: `harness/coppice/internal/web/cards.go` (after a recorded answer, run `daisugi tree prune-branches` with the same data dir; its output is appended to the reply as `pruned`), `internal/server/steward_tools.go` (`status` adds `open cards: N` from `daisugi rank queue --json`), the Rust twins
- Test: `cards_test.go` (the prune call follows the answer), the steward tools test, the Rust twins

- [ ] **Step 1: Failing tests; implement; run; commit**

```bash
git add harness/coppice/internal/web/cards.go harness/coppice/internal/web/cards_test.go harness/coppice/internal/server/steward_tools.go harness/coppice-rs/src
git commit -m "coppice: prune branch worktrees after a card closes; the steward's status counts open cards (W3K-7, W3K-16)"
```

---

### Task 18: Go: branching in `supervise/treerun.go`, the ledger fields, `prune-branches`

**Files:**
- Modify: `clients/go/internal/tree/{tree,ledger}.go`, new `internal/supervise/branch.go`, `internal/cli/treecmd.go` (`--parallel`, `--check`, `--judge`, `prune-branches`), the warden's tree-verb list
- Test: `internal/supervise/branch_test.go` (the fake start function as the Python test), `internal/tree/ledger_test.go`

- [ ] **Step 1: Port; build; test**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./internal/tree/ ./internal/supervise/ ./internal/cli/ ./internal/warden/'
```

- [ ] **Step 2: Commit**

```bash
git add clients/go/internal/tree clients/go/internal/supervise clients/go/internal/cli/treecmd.go clients/go/internal/warden
git commit -m "go: branching in the one starter, the ledger's branch rows in branch order, prune-branches"
```

---

### Task 19: Rust: the same

**Files:**
- Modify: `clients/rust/src/tree.rs`, new `src/supervise/branch.rs`, `src/cli/treecmd.rs`, the warden's tree-verb list

```bash
. clients/rust/tools/z3-build-env.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G \
  env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path clients/rust/Cargo.toml tree:: supervise::branch
git add clients/rust/src/tree.rs clients/rust/src/supervise clients/rust/src/cli/treecmd.rs clients/rust/src/warden
git commit -m "rust: branching in the one starter, the ledger's branch rows in branch order, prune-branches"
```

---

### Task 20: Tree golden cases for branching; the fake sprig keyed by session

**Files:**
- Modify: `clients/agentic_fakes.py` (a `sprig_by_session: {session: reply}` case key; the fake sprig reads its `--session` argument and answers from that table; a session not in the table exits 97, as the fake claude does for an unknown key; the fake also writes `a.txt` in its cwd when the reply names `edit`)
- Modify: `clients/tree_cases.py` (cases below; a `repo` tree entry: `git init` + one commit, laid out by the case runner; worktree paths normalize to `<branches>/<session>`)
- Modify: `clients/tree_compare.py` (the normalizer for `<data>/tree/branches/`)
- Modify: `docs/spec/conformance.md`, `clients/ADJUDICATIONS.md` (BR-R-1 to BR-R-6: the request fields, the all-or-none reservation, the row order, the hash, the checks, the harness)

| name | what |
|---|---|
| `tree run attempts bad` x6 | `attempts` 9, a bool, a text, 0; `why` missing, `why` other. `attempts` 1 runs the single child (W3K-19). |
| `tree run branches not git` | `cwd` a plain dir: refused, no worktree |
| `tree run branches budget` | 2 of 100 under 150 left: refused, no spawn row |
| `tree run branches two ran` | both ran, different edits: `ranked` or `provisional` by cost, a card opened, end rows in order |
| `tree run branches one failed` | one child exits 1: eliminated, `single` |
| `tree run branches all failed` | `none_survived`, exit 1, the card not opened |
| `tree run branches check` | `--check tests='sh -c "grep -q two a.txt"'`: one eliminated by the check |
| `tree run branches judge` | `--judge local/judge-9b` with the fake server answering `A`, `B`: one comparison recorded, `ranked` |
| `tree run branches parallel 1` | `--parallel 1`: the same tree after as parallel 2 (the fake records its start order; the compare ignores it) |
| `tree status branches` | the grouped text and `--json` |
| `tree prune-branches` x2 | an open card keeps its worktrees; a closed one loses them |
| `warden tree prune denied` | the warden case for the new verb (in `clients/warden_cases.py`, `tree rule` family) |

```bash
DAISUGI_TREE_SCRATCH=/mnt/tera/.tt/w3-rank-tree-branch/tree uv run --no-sync python clients/tree_cases.py --out clients/fixtures/tree
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/tree_compare.py --binary clients/go/daisugi --oracle --max-refused 0
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/tree_compare.py --binary clients/rust/target/release/daisugi --max-refused 0
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/warden_compare.py --binary clients/go/daisugi --only "tree rule" --max-refused 0
```
Expected: tree 164 + 15 agree in both; the warden tree-rule cases agree.

```bash
git add clients/agentic_fakes.py clients/tree_cases.py clients/tree_compare.py clients/warden_cases.py clients/fixtures/tree clients/fixtures/warden docs/spec/conformance.md clients/ADJUDICATIONS.md
git commit -m "tree cases: branching over the fake sprig keyed by session; Go and Rust agree (BR-R-1 to BR-R-6)"
```

---

### Task 21: sprig's `spawn` tool takes `attempts` and `why` (Go sprig and sprig-rs)

**Files:**
- Modify: `harness/sprig/spawn.go` (input fields `attempts` (2 to 8) and `why`; both go into the stdin request; the tool's reply for a branched run is the leader's answer, then one line `N branches; card <choice_id>`; the branch use counted toward `--max-tokens` is the sum the reply's `branches` carry), `harness/sprig/README.md`, `harness/sprig-rs/src/spawn.rs`
- Test: `spawn_test.go`, `spawn_cap_test.go` (the cap counts every branch), the Rust twin; `clients/sprig_cases.py` (3 cases: `spawn attempts`, `spawn attempts bad`, `spawn attempts cap` with the fake tree command's reply carrying `branches`)

```bash
(cd harness/sprig && go test ./...)
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path harness/sprig-rs/Cargo.toml
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/sprig_compare.py --go harness/sprig/sprig --rust harness/sprig-rs/target/release/sprig --match "spawn attempts" --max-refused 0
git add harness/sprig/spawn.go harness/sprig/spawn_test.go harness/sprig/spawn_cap_test.go harness/sprig/README.md harness/sprig-rs/src/spawn.rs clients/sprig_cases.py
git commit -m "sprig: spawn takes attempts and why, so a parent branches through the one starter; the cap counts every branch"
```

The model decides to branch (design-sprig.md: "branch only on a signal"); the `why` it must give is journaled, and the budget is the hard bound. No human is asked (the owner's 2026-10-07 word on sprig).

---

### Task 22: The steward's `spawn` takes `attempts` and `why` (coppice Go and coppice-rs)

**Files:**
- Modify: `harness/coppice/internal/server/steward_tools.go` (`spawn` args `attempts`, `why`), `steward_work.go` (the request carries them; `checks` come from the project envelope file's `checks` key as `--check` flags, W3K-11; the report line of W3K-16), the steward's tool description text, `PROTOCOL.md` ("The steward's tools")
- Modify: `harness/coppice-rs/src/server/steward_tools.rs`, `steward_work.rs`
- Test: the steward work tests in Go (`steward_work_test.go`, with the fake `daisugi` those tests use) and Rust; `harness/coppice-rs/cases/chat/` (one case: a spawn with attempts and the report)

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd harness/coppice && go test -race -timeout 40m ./internal/server/ -run "Steward|Foreman"'
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust /mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice/release/coppice --only chat --max-refused 0
git add harness/coppice/internal/server/steward_tools.go harness/coppice/internal/server/steward_work.go harness/coppice/internal/server/steward_work_test.go harness/coppice/PROTOCOL.md harness/coppice-rs/src/server harness/coppice-rs/cases/chat
git commit -m "coppice: the steward's spawn branches with attempts and why; the project's checks ride as flags (W3K-11, W3K-16)"
```

---

### Task 23: Docs that say what is built

**Files:**
- Create: `docs/how-to/review-cards.md` (the arena: open the cards view, the two levels, the sorts, what Keep, Switch and Drop record, decay, the pivot and its honest limits)
- Create: `docs/how-to/branching.md` (when a parent branches, what it costs, the `why`, the checks file key, how to read `tree status` and prune)
- Modify: `docs/README.md` (the two how-to lines), `docs/feature-status.md` (rows 106, 107, 116: the judge step, trust, pivot, cards view built; the coppice tree row from Proposed to Built; a branching row), `docs/roadmap.md` (section 6 "Where it stands": built in Python, Go and Rust with the stages named, the arena in coppice; section 7: step 4 built, the tree on the floor, branching; both still "problems, not dated lists"), `docs/plans/2026-09-24-omarchy/design-ranking.md` and `design-delegation-tree.md` (the status paragraph at the top: what this plan built, with the ruling ids), `design-sprig.md` ("Built since" gains branching), `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (RK-5, RK-6, RK-7, RK-8 "Retires when" cells: the build they named happened, with the date; AT-1: step 4 built; a new row block "This plan, W3K-1 to W3K-16" pointing here), `clients/ADJUDICATIONS.md` (MX-R-10 marked retired by Task 16; RK-R-1 "Retires when" met), `harness/coppice/README.md` (the cards view line), `AGENTS.md` (the table row "The data model / what runs": `tree_branch.py`, `rank_judge.py`, `rank_pivot.py`)
- Test: `scripts/names-check.sh` (from W2) passes; a test in `tests/test_docs.py` (or the existing doc test file) asserts `docs/roadmap.md` section 6 no longer says "Designed, not built"

```bash
scripts/names-check.sh
uv run --no-sync pytest -q tests/test_docs.py
git add docs/how-to/review-cards.md docs/how-to/branching.md docs/README.md docs/feature-status.md docs/roadmap.md docs/plans/2026-09-24-omarchy/design-ranking.md docs/plans/2026-09-24-omarchy/design-delegation-tree.md docs/plans/2026-09-24-omarchy/design-sprig.md docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md clients/ADJUDICATIONS.md harness/coppice/README.md AGENTS.md tests/test_docs.py
git commit -m "docs: ranking, the delegation tree on the floor and branching as they stand; the roadmap's stale 'not built' line goes"
```

---

### Task 24: The full run, the ledger, cleanup

- [ ] **Step 1: One heavy job at a time, in this order**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync pytest -q
uv run --no-sync ruff check . && uv run --no-sync ruff format --check .
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd clients/go && go test -p 1 ./...'
. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml
for s in rank tree warden weave k2 sprig; do systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/${s}_compare.py --binary clients/go/daisugi --max-refused 0 || break; done   # sprig_compare takes --go/--rust instead; run it as Task 21 did
for s in rank tree warden weave k2; do systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/${s}_compare.py --binary clients/rust/target/release/daisugi --max-refused 0 || break; done
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G bash -c 'cd harness/coppice && go vet ./... && go test -race -timeout 40m ./...'
CARGO_TARGET_DIR=/mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G env CARGO_BUILD_JOBS=2 cargo test --release --manifest-path harness/coppice-rs/Cargo.toml
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust /mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice/release/coppice --max-refused 0
```
Expected: all green; every compare 0 disagreements; refusals only under a ruling. The weave and k2 suites re-run because `tree_run` changed (their agentic fixtures record the tree command's words; a changed reply shape with `attempts` absent must leave them unchanged; if a fixture moves, the reason goes in the ledger before any re-record).

- [ ] **Step 2: Hand check on scratch (no live state)**

```bash
D=/mnt/tera/.tt/w3-rank-tree-branch/hand; mkdir -p $D
COPPICE_DAISUGI_DATA_DIR=$D/dd harness/coppice/coppice web --socket $D/c.sock --data-dir $D/cd &
# in the browser: open the cards view (expect "No open cards."), the tree view (expect the empty tree), press g for the garden (no badge)
# then lay out one card with clients/rank_cases.py --only "choose opens" into $D/dd and reload: one card, Keep records confirmed, the badge shows 1 then 0
kill %1
```

- [ ] **Step 3: Ledger and cleanup**

Append to `.superpowers/sdd/backlog/progress.md`: the counts (rank, tree, warden, sprig, coppice cases), the rulings W3K-1 to W3K-16 and BR-R-n, RK-R-17 onward, the retired MX-R-10, and the open questions below with their state. Then:

```bash
rm -rf /mnt/tera/.tt/w3-rank-tree-branch/{rank,tree,sprig,coppice,hand}
rm -rf /mnt/tera/.tt/w3-rank-tree-branch/cargo-coppice
```
Keep `clients/rust/target` (it holds the built Z3). The worktree stays until the controller merges; `git worktree remove` after the merge.

## Open questions, with this plan's rulings

1. **Does the judge step port to Go and Rust now, or last?** The design says "ported last, as the other model-calling parts are"; the house rule is full parity. Ruling: in this plan, after the Python one (Tasks 7 to 9), with the prompt bytes pinned so the fake model server proves the bodies equal. Cost if wrong: two tasks that could have waited.
2. **Who may run `rank judge`?** It calls a model and writes comparison rows an agent could shape to favor its own attempt. Ruling: the warden's `RANK_VERBS` deny covers `rank record` and `rank pivot --apply`; `rank judge` is **not** denied, because the fit already binds one vote per judge per pair and the owner's answer outranks every judge (RK-R-8); an agent that runs `rank judge` on its own ranking spends its own budget and cannot add a second opinion from the same judge. A branch's children never see the `daisugi` head anyway (TR-R-9's rule denies the tree's write verbs; the envelope's shell allowlist decides the rest). If the owner wants it denied, it is one entry in `RANK_VERBS` in three ports. Cost if wrong: one line per port.
3. **Where do checks come from?** A model-written check would be a shell step outside the warden. Ruling W3K-11: the starter's side only (the project envelope file, the executor's plan). Cost if wrong: a trust rule later.
4. **How many branches at once on this box?** Ruling W3K-9: `min(N, 4)`; the children are mostly waiting on a model. Cost if wrong: a number.
5. **Does branching wait for a human when the fit is `provisional`?** No, by RK-10: the parent proceeds on the leader; the card is for later. The owner's 2026-10-07 word keeps human ranking out of sprig's edge; the card is the owner's review, not sprig's. Both hold at once.
6. **A pane connection creating a task with an envelope.** Ruling W3K-12: a pane may create a child task under its own task with a narrower envelope (the proof runs); only the operator makes a root. Cost if wrong: one refusal line.
7. **The data dir coppice passes to `daisugi rank` and `daisugi tree`.** Ruling W3K-5: `COPPICE_DAISUGI_DATA_DIR` or daisugi's default. An alternative is a `[daisugi] data_dir` key in `coppice.toml`; it can be added without moving anything. Cost if wrong: one key.
8. **The RK-6 index.** Ruling W3K-3: not built; a bound test instead. Cost if wrong: the index is additive.
9. **Stage 6, the router's labels.** Ruling W3K-14: the router plan owns it. Cost if wrong: one reader.
10. **The pivot and checkpoints.** Ruling W3K-4: the pivot uses the ledger and git only; `checkpoints.restore` needs a session's checkpoint id the card does not carry. A later task can add `facts.run.checkpoint` to the choice row (additive) and the restore path. Cost if wrong: one field.

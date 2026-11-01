# W2: the rename (gate, gateway, foreman, memory) Implementation Plan

> **Controller note (2026-10-30, from the router plan):** `src/opendaisugi/router_report.py` already exists (the `router status` report). Before renaming `gateway_report.py` -> `router_report.py`, first `git mv router_report.py router_status.py` (and its tests, imports, and the Go/Rust twins of the same name) in its own commit. ADR-0025 (record by default, from Wave 1) is a record: add it to `scripts/names-allow.txt`. The rename ADR is 0026.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename four parts across every port and every surface in one branch, as a clean break: gate becomes **warden**, gateway becomes **router** (its inner policy becomes **routing mode**), foreman becomes **steward**, and the conversation memory is **Pando R2** in prose, with `pando` as its code identifier.

**Architecture:** One builder, one branch on real disk, one commit per port or surface, in the order the compare harnesses need: the Python oracle first (it writes the golden cases), then the fixtures, then the Go and Rust `daisugi`, then coppice (Go, then Rust), then sprig, then install and packaging, then docs, site and the record. A mechanical check (`scripts/names-check.sh`) is written first and must fail; it passes only when no old word is left outside an explicit allowlist. On-disk state of the owner's box is moved once, by `daisugi install`, with one printed line per move and nothing deleted.

**Tech Stack:** Python 3.12 (typer, pytest), Go (cgo, Z3), Rust (cargo), shell. No new dependencies.

**Spec:** this file's sections "Owner rulings", "Surface inventory", "Rulings per surface" and "Historical documents". Also `docs/plans/2026-10-30-backlog/INDEX.md` (wave 2), the ledger `.superpowers/sdd/backlog/progress.md` (rulings R-1, R-2), `docs/adr/0023-pando-r2-in-daisugi.md` (Proposed; it names `pando` and the future store path).

## Global Constraints

- Box: 4 cores, 15 GB. **One heavy job at a time**, each under `systemd-run --user --scope -p MemoryMax=5G`, with `-j2` (`cargo -j2`, `GOFLAGS=-p=2`, `go test -p 1` where the README says so). A heavy job is: any Go or Rust build, any full test run, any compare suite, any fixture regeneration.
- `/tmp` is RAM. The worktree is `/mnt/tera/working/programming/openDaisugi-w2`. Scratch is `~/opendaisugi-scratch/w2-rename/` (probe binaries, `DAISUGI_*_SCRATCH`, fake HOMEs). Delete scratch when a task ends.
- No network and no real model in any test. Compare suites use `fake-upstream` and the fakes in `clients/`.
- Never `pkill -f`. Stop a process by the pid in its lock file, or by its own stop verb.
- No AI attribution in commits. Commits state the why. Never commit `CLAUDE.md` or `docs/superpowers/`.
- Prose in STE: short sentences, plain words, active voice, no em dashes. Never the words "seam" or "the layer".
- Every coppice run in a hand check passes its own `--socket` and `--data-dir` under scratch. The live coppice server, `~/.opendaisugi` and `~/.local/state/coppice` are the owner's: **never read, written or moved by a builder or a test.** The one-time move in Task 2 runs only when the owner runs `daisugi install`; tests exercise it on a fake data dir.
- Names that do **not** change: `daisugi`, `coppice`, `sprig`, `journal`, `garden`, `envelope`, `verdict`, `Switchyard` (NVIDIA's backend), `route` (the dry-run verb), `hook` (the capture hook), `fence`, `graft`, `conform`, `ctrl-t`.
- Clean break (R-2): no alias on any surface a person types or edits (command, flag, config key, env var, protocol verb, pane label). The four narrow exceptions (D, G, J and Q) are in "Rulings per surface" and each is one line of code that reads the owner's existing data.
- The branch merges as one series. No fold to the public repo until Task 13 passes.

## Review Focus

1. A hook written by the old install (`DAISUGI_GATE_HOOK=opendaisugi.gate ... gate check`) left in `~/.claude/settings.json` after the new binary lands: Claude Code would run `daisugi gate check`, get exit 2 (`gate is not in this binary yet.`) and **deny every call**. The new install must remove that entry, not add a second one. Test in Task 2 (`test_install_replaces_old_marker_hook`).
2. A pi or OpenCode extension at the old path (`~/.pi/agent/extensions/daisugi-gate`, `.../opencode/plugins/daisugi-gate.ts`) still auto-loads, asks `gate.sock`, finds nothing and denies every call. Install must move it aside, out of the extensions directory. Test in Task 2 (`test_install_moves_old_extensions_aside`).
3. A `config.toml` with `gate_mode = "enforce"` after the key is `warden_mode`: an unknown key must not make the loader fall back to audit in silence, because that turns enforce off. Install rewrites the six keys; `warden status` names a stale key. Tests in Task 2 and Task 3.
4. A live resident gate still listening on `<data>/gate/gate.sock` when install wants to move the directory. Install refuses the move with one line and changes nothing. Test in Task 2 (`test_install_refuses_move_while_socket_live`).
5. The weave and k2 fixtures carry `--gate` in sprig argv and the cli fixtures carry 259 `gate`/`gateway` invocations. A port that passes its old fixtures after the rename has not been renamed. Task 4 regenerates from the oracle and pins new manifests; Tasks 5 to 10 run the compares against the new manifests.

---

## Owner rulings (2026-10-28 and 2026-10-29)

- gate -> **warden**. Described as an observer and a supervisor in the words of control theory: by default it watches and records every action; when enforcing it stops actions outside the envelope. Lore name: bannin.
- gateway -> **router**. The inner routing policy (`off|rules|switchyard|knn|model`) becomes **routing mode** (R-1), so there is no "router router". Switchyard stays the name of NVIDIA's backend.
- foreman -> **steward**. A role: whoever sits in the chair. Lore: bantō.
- the never-ending conversation memory -> **Pando R2** (Reversible Recollections) in prose. Code identifier: `pando` (see ruling P below).
- delegate -> **spawn** (owner, 2026-10-30, item A1): the MCP tool is `spawn`, the executable that W3 builds is `daisugi-spawn`, the Python module is `spawn.py`, the Go package `internal/spawn`, the Rust module `spawn.rs`. The English word "delegate" in prose ("a delegated step", the skill's "delegation" pages) stays. The steward already has its own tool called `spawn` on the coppice MCP server; the two live on different servers and do not clash.
- Clean break, no aliases (R-2). The owner re-runs install once.

## Surface inventory (measured on master d7afcede, whole word, case-insensitive)

Totals: `gate` 34,369 hits in 1,239 files; `gateway` 5,128 in 341; `foreman` 4,414 in 374. 475 tracked files carry a word in their name (fixtures excluded). Already using new names: `docs/start-here.md`, `site/index.html`, `site/start-here.html`, `site/CLAIMS.md` (prose only; commands still old).

| Surface | Python oracle | Go daisugi | Rust daisugi | coppice (Go) | coppice-rs | sprig (+rs) | docs, site, scripts, CI |
|---|---|---|---|---|---|---|---|
| Word hits (gate / gateway / foreman) | src 1169/322/25; tests 1506/259/33 | 1455/408/9 | 1636/310/11 | 2053/352/2318 | 554/153/1189 | 157+137 / 4 / 1 | current docs ~700/180/120; historical 2409/541/263; site 129/20/10; scripts 70; .github 26; packaging 14; examples 53 |
| CLI commands | `gate` group (12 verbs: init check register disarm arm status report replay settings audit serve proposals), `gateway`, `gateway-report`, `router` group (status label stop), `route`, `install` | same dispatch (`cli.go:364-396`) plus `gate` verbs in `gatecmds.go` | same, minus `status config start journal` | `coppice foreman log|view|zoom|date|stats|import|pool`, `coppice skill foreman`, `floor talk` | mirror | none | help text: `cli.py` 23 `help=` strings, `helpall_gen.go` 29, `helpall_gen.rs` 28 (both generated by `clients/gen_help.py`) |
| Flags | `install --gate --gateway --router X --enforce`; `fence --gate-root` | same | same | `--gate-root` | mirror | `--gate`, `--gate-cmd` | |
| Config keys | `gate_mode`, `gate_ask`, `gateway_local_model`, `gateway_router`, `gateway_router_model`, `gateway_grader` | reads the same file | same | `gateway = URL` (top level), `[foreman]` table (its 6 keys carry no word) | mirror | | `pyproject.toml` extra `opendaisugi[gateway]` (+5 lines in `uv.lock`) |
| Env vars (20) | `DAISUGI_GATE_HOOK` (marker, 81 refs), `DAISUGI_GATE_NOW` (40), `_TRACE`, `_CHILD`, `_WORKER`, `_SCRATCH`, `_TEST_CRASH`, `_ASSUME_SHELL_PARSER`, `_DEPTH_TRACE`, `_DEPTH_LOG`, `_TTY_COLUMNS`, `_RESIDENT`, `_SERVER_SCRATCH`; `OPENDAISUGI_GATE_SOCK` (52); `OPENDAISUGI_GATEWAY_BASE_URL` (11); `DAISUGI_GATEWAY_SCRATCH` | shared | shared | `COPPICE_FOREMAN_HELPER` (5), `COPPICE_IS_STRICTER_THAN_HERDR_FOR_NOT_ONLY_GATES` (1, test) | | | CI env in `clients.yml:223-228` |
| Socket, lock | `gate.sock` (46 refs), `gate.lock` (8) | yes | yes | reads `gate.sock` through the fence | | `--gate-cmd` | |
| XDG data and state names | `<data>/gate/` (43 literal refs; holds `envelopes/ audit/ sessions/ report/ graft/ DISARMED gate.sock gate.lock`), `<data>/gateway/` (`turns.jsonl routing.db answers.jsonl`) | `gateroot` pkg | `gateroot` mod | `<data>/foreman/` (17 refs; `main/ tree/ view.json import.json lock`), state `foreman.json` (4) | mirror | | `bench/corpus/gate-paths.jsonl` |
| Hook strings written into harness settings | marker `DAISUGI_GATE_HOOK=opendaisugi.gate` (`config.py:293`), `python -I -m opendaisugi.gate_client`, `daisugi gate check`, `$(daisugi gate settings)`, pi dir `daisugi-gate`, OpenCode `daisugi-gate.ts`, Codex `hooks.json` | `install/` writes the same | `cli/install.rs`, `layers.rs` | `server/fence.go:314` writes the marker | mirror | | |
| MCP tool names | 0 carry a word (`envelope_for find_pathway recall recall_answer verify_plan verify_completed_step list_pathways pathway_stats run_plan receipts_for_run recent_runs delegate`; `delegate` becomes `spawn`, ruling Q) | | | steward tools: `date foreman spawn status zoom` (1 of 5) | mirror | | |
| JSON fields on the wire | `turns.jsonl` key `router` (value = routing mode); journal extra `gated_by_envelope`; adversarial `no_gate` | `json:"gate_root"` | | PROTOCOL: `task.set_foreman`, `floor.foreman`, fields `foreman`, `foreman_label`, `foreman_turn.ran_on`, pane `gate` object, event `source:"gate"`, `gate_root` (web config, fence), text `blocked via gate`, magic pane label `foreman` | mirror | | `tests/floor/testdata/events/gate-blocked*.json`, `harness/coppice/testdata/events/` |
| Packages, modules, types | `gate.py gate_client.py gate_server.py gate_state_rule.py floor/gate_states.py bench/layers/gate_path.py`, `gateway*.py` (9 files), `router_switchyard.py` | pkgs `gate` (61 files), `gateroot` (5), `gateway` (11); 12 types, 251 funcs; `cmd/daisugi-gate`, `cmd/gateway-probe` | mods `gate` (39 files + `py/`), `gateway` (13), `gatestate`, `gateroot`; 161 names; bins `daisugi-gate`, `gateway-probe` | pkgs `foreman` (51 files), `gatefs` (3); `server/foreman_*.go` (35); `config/foreman.go` | `src/foreman/` (17), `server/foreman_*.rs` (12), `server/gatefs.rs` | `gate.go daisugi_gate.go`, `sprig-rs/src/gate.rs` | |
| Test names | 65 files `test_gate*`, 18 `test_gateway*`, 1 `test_gate_foreman_dir`; 340 test functions | 195 `TestXxxGate*` funcs across go+coppice+sprig | `tests/gate_backstop.rs` + 99 fns | golden `testdata/golden/` (regenerated with `-update`) | `cases/chat/foreman-*.jsonl` (21), `cases/facts/foreman*.jsonl` (2), `cases/ended/swap-gateway.jsonl` | | |
| Golden fixtures (`clients/fixtures/`) | dirs `gate` (2310 cases, 9 MB), `gate_server` (239, 25 MB), `gateway` (383); words inside 16 suites: cli 521/725 lines (259 cases invoke renamed commands), f 135/199, fence 94/94, garden 91/233, k1 33, k2 66, k3 149, k4 101, l 1, rank 2, tree 120/216, voice 3, weave 42/169 | compares | compares | | | | CI matrix `clients.yml:196` |
| Binaries and packages | | `daisugi-gate` | `daisugi-gate`, AUR `daisugi-gate-rs` | | | | `install.sh` (4), `release.sh` (13), `smoke-tarball.sh` (14), `packaging/aur/*` (14) |
| Doc files named with a word (current) | `docs/how-to/gate.md`, `token-saving-gateway.md`, `router-switchyard.md`, `foreman-any-model.md`, `foreman-aloud.md`, `bring-your-own-foreman.md`; `docs/recipes/gate-only.md`, `gateway-router.md`, `coppice-claude-gate.md` | | | `skills/foreman/`, `skills/foreman-turn/`, `e2e/fake-foreman.sh`, `testdata/transcripts/chat-foreman.py` | | | 47 current doc files carry a word; 133 historical |

## Rulings per surface

| # | Surface | Ruling | What happens to existing state on the owner's box |
|---|---|---|---|
| A | Commands, verbs, flags, help, log lines | **Rename.** `daisugi warden <verb>`; `daisugi router serve` (was `gateway`), `daisugi router report` (was `gateway-report`), `daisugi router status|label|stop` (kept verbs, now under the router), `daisugi route` (kept); `daisugi install --warden --router --routing-mode X`; `daisugi fence --warden-root`; `coppice steward <verb>`, `coppice skill steward`, `coppice --warden-root`; `sprig --warden --warden-cmd`. | None stored. |
| B | Config keys | **Rename:** `warden_mode`, `warden_ask`, `router_local_model`, `routing_mode`, `routing_model`, `router_grader`; coppice `router = URL`, `[steward]`. The loader does not accept the old keys. | `daisugi install` rewrites the six Python keys in place in the file `load_config` reads, one line printed per key. coppice refuses to start on `[foreman]` or `gateway =` with one line naming the new key (the owner edits `coppice.toml` once). |
| C | Env vars | **Rename** all 20 (`DAISUGI_WARDEN_*`, `OPENDAISUGI_WARDEN_SOCK`, `OPENDAISUGI_ROUTER_BASE_URL`, `DAISUGI_ROUTER_SCRATCH`, `COPPICE_STEWARD_HELPER`, `..._NOT_ONLY_WARDENS`). | Panes started before the rename carry old vars until restarted. Nothing stored. |
| D | Hook marker and hook strings | **Rename:** marker `DAISUGI_WARDEN_HOOK=opendaisugi.warden`, module `opendaisugi.warden_client`, verb `warden check`. **Exception 1 (one substring):** `install` also recognises the old marker and the old `-m opendaisugi.gate` forms as ours, removes that entry and writes the new one, so no settings file ends with two hooks. Precedent: `install.py:564-575` already does this for the `gate` -> `gate_client` move. | `~/.claude/settings.json`, `~/.codex/hooks.json`: old entry removed, new written, one line printed. pi dir `daisugi-gate` and OpenCode `daisugi-gate.ts`: moved to `<data home>/moved-aside/<UTC stamp>/`, never deleted, one line printed; new extension written at `daisugi-warden`, `daisugi-warden.ts`. |
| E | Socket and lock names | **Rename** to `warden.sock`, `warden.lock`. | They live inside the moved directory (F). A stale `gate.sock` left in it is harmless and is not deleted. |
| F | `<data>/gate/`, `<data>/gateway/` | **Rename** the directories to `<data>/warden/`, `<data>/router/`. Inner names (`envelopes/ audit/ sessions/ report/ graft/ DISARMED turns.jsonl routing.db answers.jsonl`) keep their names. | One-time move by `daisugi install` (any flags): `rename(2)` when the new dir is absent and the old one exists; one line printed. If both exist: refuse with one line, change nothing. If `gate.sock` accepts a connection: refuse with one line (a server still runs). `daisugi warden status` prints a hint when it finds `<data>/gate` and no `<data>/warden`. |
| G | `turns.jsonl` record key `router` | **Rename** the key to `routing_mode` for new lines. **Exception 2 (one `.get` chain):** the report reads `routing_mode`, else `router`, for lines written before the rename, so the owner's own metering history is not shown as unknown. | Old lines stay as written, inside the moved `router/` dir. |
| H | Journal extra `gated_by_envelope`, adversarial arm `no_gate` | **Rename** to `checked_by_envelope`, `no_warden`. Readers aggregate nothing by these keys. | Old journal lines keep the old key. Nothing reads it back by name. |
| I | coppice protocol verbs and fields | **Rename:** `task.set_steward`, `floor.steward`, `steward`, `steward_label`, `steward_turn.ran_on`, pane object `warden`, event `source:"warden"`, `warden_root`, text `blocked via warden`, magic pane label `steward`. The protocol is version 1 between coppice and its own clients (TUI, web, CLI, plugins, the daisugi floor backend, coppice-rs); they ship together. | Ended pane records labelled `foreman` lose the magic and become plain records; they resume as ordinary panes. Nothing is moved. |
| J | coppice `<data>/foreman/` directory and state `foreman.json` | **Keep the directory name on disk** behind one constant (`steward.DirName = "foreman"`, comment: "ADR-0023 moves this store to `<data home>/pando/<store>/` with `daisugi pando migrate`; one move, not two"). **Exception 3.** Rename state `foreman.json` to `steward.json`. | The log is irreplaceable and ADR-0023 moves it soon; two moves of it is one too many. A stale `foreman.json` is left in place (never deleted); the floor has no steward named until `floor talk` or `task.set_steward` names one. The re-install step says so. |
| K | Python modules, Go packages, Rust mods, binaries | **Rename.** Python: `warden.py warden_client.py warden_server.py warden_state_rule.py floor/warden_states.py bench/layers/warden_path.py`; `gateway*.py` -> `router*.py`; `router_switchyard.py` -> `routing_switchyard.py` (`routing.py`, the orchestrator's, stays). Go: `internal/warden`, `internal/wardenroot`, `internal/router`; `cli/wardencmds.go`; `cli/gatewaycmd.go` -> `cli/routercmd.go` and the present `routercmd.go`, `routermeasure.go`, `installrouter.go` -> `routingcmd.go`, `routingmeasure.go`, `installrouting.go`; `cmd/daisugi-warden`, `cmd/router-probe`. Rust mirrors Go (`gate` -> `warden`, `gateway` -> `router`, `gatestate` -> `wardenstate`, `gateroot` -> `wardenroot`, `installgw.rs` -> `installrouter.rs`, old `installrouter.rs` -> `installrouting.rs`). coppice: `internal/steward`, `internal/wardenfs`, `server/steward_*.go`, `config/steward.go`. Binaries `daisugi-warden`, AUR `daisugi-warden-rs`, `router-probe`. | Built artifacts under `~/.local/bin` are replaced by the owner's re-install. |
| L | Test names, fixture dirs, case writers, compare scripts | **Rename** (`test_warden_*`, `clients/fixtures/warden`, `warden_server`, `router`; `clients/warden_cases.py` and so on; CI matrix names). Regenerate every suite whose fixture carried a word (16 suites) from the oracle. | None. |
| M | MCP tool names | **Keep** eleven of the twelve `opendaisugi` tools (none carries a word). Rename the twelfth, `delegate`, to `spawn` (ruling Q). Rename the steward's `foreman` tool to `steward`. | None. |
| Q | `delegate` -> `spawn` (owner, 2026-10-30, A1) | **Rename** the MCP tool `delegate` to `spawn` (clean break, no alias, R-2); `src/opendaisugi/delegate.py` -> `spawn.py` (`run_delegate` -> `run_spawn`, `DelegationRecord` -> `SpawnRecord`), `delegating_executor.py` stays; Go `internal/delegate` -> `internal/spawn`; Rust `src/delegate.rs` -> `src/spawn.rs`; the redirect target in graft rules (`redirect.tool`) reads `spawn`; the tool's JSON argument names and journal `kind` value (`delegate` -> `spawn`) follow. The executable named in W3 (`daisugi-delegate`) does not exist yet: W3 creates it as `daisugi-spawn`. | A graft rule file or MCP allowlist entry that names `delegate` stops matching. `daisugi install` re-registers the MCP server, so the tool list refreshes; an envelope that allows `mcp:opendaisugi/delegate` must be edited by the owner once (Task 3b prints the one line). Old journal lines keep `kind: delegate`; the report reads `spawn`, else `delegate` for lines written before today (the one `.get` chain, exception 4). |
| N | Site | **Rename** commands and links in `site/index.html`; regenerate `site/start-here.html` with `site/build.py`; add one claim line to `site/CLAIMS.md`. Prose already says warden and steward. | None. |
| O | Third-party and generic uses | The whole-word check also matches "merge gate", "importorskip-gated", "gates on zig" in our own comments. **Rewrite** those to "merge check", "skipped", "needs". A quoted third-party name ("Warden's post" in `docs/research/stt-2026-10.md`) is historical and allowlisted by path. | None. |
| P | Pando R2 | **Prose:** "the foreman's memory tree", "the foreman's chat log", "memory" (where it means this store) become "Pando R2" on first mention and "Pando" after. **Identifier `pando`:** confirmed, as ADR-0023 already uses it. Reasons: five lowercase letters, zero clashes in code today (its only hits are `site/CLAIMS.md`), a valid identifier in Python, Go and Rust, and the house style of one-word lowercase part names (`daisugi`, `coppice`, `sprig`). `r2` is not in the identifier: `pando_r2` reads as a version, and "R2" is the expansion that lives in prose. **This wave does not create a `pando` package or move the store**; that is ADR-0023's build. This wave uses `pando` in labels only: `coppice steward` usage text, the `coppice backup` usage line, the web and TUI header of the zoom view, the golden README. | None. |

### The `daisugi install` one-time move, exactly

`install.move_old_state(data_dir: Path, home: Path, now: datetime, out: TextIO) -> list[str]` in `src/opendaisugi/install.py` (Go and Rust `install` call the same logic in their own code; the Go `daisugi` is what the owner runs, so Task 5 ports it; Rust follows in Task 7). It runs at the start of every `daisugi install` that is not `--dry-run` (a dry run prints what it would do). In order:

1. `<data>/gate` exists and `<data>/warden` does not: if `<data>/gate/gate.sock` accepts a connection, print `a gate server still listens on <sock>; stop it (the pid is in <data>/gate/gate.lock) and run install again` and exit 2. Else `rename` and print `moved <data>/gate to <data>/warden`.
2. Both exist: print `both <data>/gate and <data>/warden exist; move one aside by hand, then run install again` and exit 2.
3. Same for `<data>/gateway` -> `<data>/router` (no socket check).
4. The config file `load_config` reads: for each of the six old keys present, rewrite the key in place (value untouched) and print `config: gate_mode is now warden_mode`.
5. Harness settings: an entry with the old marker or `-m opendaisugi.gate` / `-mopendaisugi.gate` / `-m opendaisugi.gate_client` is ours: remove it. Then the normal write adds the new one. Print `replaced the old warden hook in <file>`.
6. `~/.pi/agent/extensions/daisugi-gate` and `<opencode config>/plugins/daisugi-gate.ts`, when present: `rename` into `<data home>/moved-aside/<YYYYMMDDTHHMMSSZ>/pi-daisugi-gate` and `.../opencode-daisugi-gate.ts`; print `moved aside <old path> to <new path>`.

Nothing is deleted. Each line is printed once. The returned list holds the lines (tests assert on it).

## Historical documents

Records keep their words and get nothing added: `docs/plans/**` (every dated plan directory, including `DECISIONS-PENDING.md` and `RESUME.md`), `docs/adr/0001` to `0022` (and their README index lines), `docs/research/**` (boards, evidence files), `docs/exploration/**`, `docs/correspondence.md`, `CHANGELOG.md` entries before this one, `clients/ADJUDICATIONS.md` (15,208 lines of rulings with case ids), `clients/PORTING-NOTES.md`, `gateway-spike-log/`, `examples/gateway-spike/`, `opendaisugi-session2summary.md`, `decks/`, `bench/corpus/gate-paths.jsonl` (a recorded corpus). ADR-0023 and ADR-0024 are new and already use the new names in prose; this wave does not edit them. The record of the rename itself is a new ADR-0026 and a new CHANGELOG entry (Task 13). `clients/ADJUDICATIONS.md` gets no edit; the case ids (`GW-8`, `FR-R-1`) stay as cited by the compare scripts.

Why keep: a plan dated 2026-09 that says "the gate" is a true statement about 2026-09. Rewriting it would make the record lie about what the parts were called when the decision was made, and `git log -S` would stop finding the decision by its word.

---

### Task 1: Worktree, scratch, baseline and the failing check

**Files:**
- Create: `scripts/names-check.sh`
- Create: `scripts/names-allow.txt`
- Test: the script itself (it must exit 1 now and 0 at the end of Task 13)

**Interfaces:**
- Produces: `scripts/names-check.sh [--count]` exits 0 when no tracked file outside the allowlist holds a whole word from `gate|gated|gates|gateway|gateways|foreman|foremen` (case-insensitive, text files only, `git grep -I`) and no tracked path outside the allowlist carries one; exits 1 and prints every hit (`path:line:text`, or `path` for a name hit) otherwise. `--count` prints only the number of hits.
- Produces: `scripts/names-allow.txt`, one entry per line: `path <prefix>` (a tracked-path prefix, matched with `git grep -- . ':!<prefix>'` and `git ls-files | grep -v '^<prefix>'`), or `phrase <text>` (an exact, case-insensitive phrase removed from the hits with `grep -v -i -F`). `#` starts a comment. Every `phrase` line ends with `# why`.

- [ ] **Step 1: Make the worktree on real disk and the scratch dir**

```bash
cd /mnt/tera/working/programming/openDaisugi
git worktree add ../openDaisugi-w2 -b w2-rename master
mkdir -p ~/opendaisugi-scratch/w2-rename
df -h /mnt/tera | tail -1
```
Expected: the worktree exists; `/mnt/tera` shows more than 20 GB free.

- [ ] **Step 2: Record the baseline counts into the commit message of this task**

```bash
cd /mnt/tera/working/programming/openDaisugi-w2
for w in gate gateway foreman; do printf "%s " $w; git grep -I -i -w -o -- "$w" | wc -l; done
```
Expected: about `34369`, `5128`, `4414`.

- [ ] **Step 3: Write `scripts/names-allow.txt` with the historical paths**

Path lines, one each: `docs/plans/`, `docs/adr/0001-` through `docs/adr/0022-` (one line per ADR file prefix), `docs/research/`, `docs/exploration/`, `docs/correspondence.md`, `CHANGELOG.md`, `clients/ADJUDICATIONS.md`, `clients/PORTING-NOTES.md`, `gateway-spike-log/`, `examples/gateway-spike/`, `opendaisugi-session2summary.md`, `decks/`, `bench/corpus/gate-paths.jsonl`, `scripts/names-allow.txt`, `scripts/names-check.sh`. No `phrase` lines yet.

- [ ] **Step 4: Write `scripts/names-check.sh`**

Stdlib bash, `set -euo pipefail`, `cd "$(git rev-parse --show-toplevel)"`. Build the pathspec exclusions from `path` lines, run `git grep -I -n -i -w -E 'gate|gated|gates|gateway|gateways|foreman|foremen' -- . "${excl[@]}"`, filter `phrase` lines with `grep -v -i -F -f`, then `git ls-files | grep -i -E 'gate|gateway|foreman'` filtered by the path prefixes. Print hits, exit 1 when any.

- [ ] **Step 5: Run it to see it fail with the baseline**

Run: `scripts/names-check.sh --count`
Expected: a number above 20,000 and exit 1.

- [ ] **Step 6: Commit**

```bash
git add scripts/names-check.sh scripts/names-allow.txt
git commit -m "rename: add the names check that must pass when gate, gateway and foreman are gone

Baseline on master: gate 34369, gateway 5128, foreman 4414 whole-word hits.
Historical records are allowlisted by path and are never edited."
```

---

### Task 2: Python oracle, the warden

**Files:**
- Rename (`git mv`): `src/opendaisugi/gate.py` -> `warden.py`, `gate_client.py` -> `warden_client.py`, `gate_server.py` -> `warden_server.py`, `gate_state_rule.py` -> `warden_state_rule.py`, `floor/gate_states.py` -> `floor/warden_states.py`, `bench/layers/gate_path.py` -> `bench/layers/warden_path.py`, `harness_opencode/plugin/daisugi-gate.ts` -> `daisugi-warden.ts`; `tests/test_gate*.py` (65) -> `tests/test_warden*.py`; `tests/floor/testdata/events/gate-blocked*.json` -> `warden-blocked*.json`; `tests/bench/test_bench_gate_path.py` -> `test_bench_warden_path.py`; `tests/floor/test_gate_states.py` -> `test_warden_states.py`
- Modify: `src/opendaisugi/cli.py:310-330` (the `gate_app`), every `data_home() / "gate"` (lines 698, 912, 1697, 3959, 3989, 4007, 4290, 6873), `src/opendaisugi/config.py:107,118,293`, `src/opendaisugi/install.py` (hook writer, marker, pi and OpenCode paths at 1466-1540), `src/opendaisugi/datahome.py`, `src/opendaisugi/swap.py`, `src/opendaisugi/owner_rule.py:118`, `src/opendaisugi/tui.py:55`, `src/opendaisugi/dashboard.py:174`, `src/opendaisugi/rank.py:214`, `src/opendaisugi/adversarial.py:454-464`, `src/opendaisugi/decomposer.py:235`, `src/opendaisugi/floor/events.py:292`, `src/opendaisugi/floor/manifests.py:778`, `src/opendaisugi/harness_pi/extension/index.ts:46`, `src/opendaisugi/skills/opendaisugi-checklist/SKILL.md`, `src/opendaisugi/install_assets/openclaw_plugin/openclaw.plugin.json`, every `tests/**` import and string
- Create: `tests/test_install_rename_move.py`

**Interfaces:**
- Produces: `daisugi warden {init,check,register,disarm,arm,status,report,replay,settings,audit,serve,proposals}` with the same options as the old verbs; `daisugi install --warden [--enforce] [--ask]`; `daisugi fence --warden-root DIR`.
- Produces: `config.WARDEN_HOOK_MARKER = "DAISUGI_WARDEN_HOOK=opendaisugi.warden"`; config fields `warden_mode: str = "audit"`, `warden_ask: bool = False`; `DEFAULT_SOCK_NAME = "warden.sock"`, lock `warden.lock`; the warden root is `data_home() / "warden"`.
- Produces: `install.move_old_state(data_dir: Path, home: Path, now: datetime, out: TextIO) -> list[str]` as specified above; called from `install_cmd` before any write, skipped under `--dry-run` except for printing.
- Produces: `warden status` prints `found <data>/gate from before the rename: run daisugi install to move it` when that is so, and `config: unknown key gate_mode (now warden_mode)` for each stale key the loader reports.
- Produces: env vars `DAISUGI_WARDEN_HOOK`, `DAISUGI_WARDEN_NOW`, `DAISUGI_WARDEN_TRACE`, `DAISUGI_WARDEN_CHILD`, `DAISUGI_WARDEN_WORKER`, `DAISUGI_WARDEN_SCRATCH`, `DAISUGI_WARDEN_TEST_CRASH`, `DAISUGI_WARDEN_ASSUME_SHELL_PARSER`, `DAISUGI_WARDEN_DEPTH_TRACE`, `DAISUGI_WARDEN_DEPTH_LOG`, `DAISUGI_WARDEN_TTY_COLUMNS`, `DAISUGI_WARDEN_RESIDENT`, `DAISUGI_WARDEN_SERVER_SCRATCH`, `OPENDAISUGI_WARDEN_SOCK`.
- Produces: the help text of `warden_app`: `The warden watches each tool call and records it. With --mode enforce it also stops calls outside the registered envelope.`; `rich_help_panel="Warden"`.
- Produces: floor event `source: "warden"`; journal extra `checked_by_envelope`; adversarial arm `no_warden`.

- [ ] **Step 1: Write the failing tests for the one-time move**

```python
# tests/test_install_rename_move.py
def test_moves_gate_and_gateway_dirs(tmp_path, capsys):
    (tmp_path / "gate" / "envelopes").mkdir(parents=True)
    (tmp_path / "gateway").mkdir()
    lines = install.move_old_state(tmp_path, home=tmp_path / "h", now=NOW, out=sys.stdout)
    assert (tmp_path / "warden" / "envelopes").is_dir() and not (tmp_path / "gate").exists()
    assert (tmp_path / "router").is_dir()
    assert lines == [f"moved {tmp_path}/gate to {tmp_path}/warden", f"moved {tmp_path}/gateway to {tmp_path}/router"]

def test_refuses_when_both_exist(tmp_path):
    (tmp_path / "gate").mkdir(); (tmp_path / "warden").mkdir()
    with pytest.raises(SystemExit) as e: install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert e.value.code == 2 and (tmp_path / "gate").exists()

def test_install_refuses_move_while_socket_live(tmp_path):
    # bind a unix socket at tmp_path/gate/gate.sock and listen; expect exit 2 and no move

def test_rewrites_old_config_keys(tmp_path):
    cfg = tmp_path / "config.toml"; cfg.write_text('gate_mode = "enforce"\ngateway_router = "knn"\n')
    install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert cfg.read_text() == 'warden_mode = "enforce"\nrouting_mode = "knn"\n'

def test_install_replaces_old_marker_hook(tmp_path):
    # settings.json with one PreToolUse entry whose command holds "DAISUGI_GATE_HOOK=opendaisugi.gate"
    # after install_cmd(--warden): exactly one PreToolUse entry, holding "DAISUGI_WARDEN_HOOK=opendaisugi.warden"

def test_install_moves_old_extensions_aside(tmp_path):
    # ~/.pi/agent/extensions/daisugi-gate/index.ts exists -> after install: gone from extensions,
    # present under <data>/moved-aside/20261030T120000Z/pi-daisugi-gate/index.ts
```
Use the socket-live test only on Linux (the suite already is).

- [ ] **Step 2: Run them to see them fail**

Run: `uv run --no-sync pytest tests/test_install_rename_move.py -q`
Expected: FAIL, `AttributeError: module 'opendaisugi.install' has no attribute 'move_old_state'`.

- [ ] **Step 3: `git mv` the files listed above**

One `git mv` per file. Then fix imports with `sed` over `src/ tests/ clients/*.py` for `opendaisugi.gate_client`, `opendaisugi.gate_server`, `opendaisugi.gate_state_rule`, `opendaisugi.gate ` and `from opendaisugi import gate`, `floor.gate_states`, `bench.layers.gate_path`.

- [ ] **Step 4: Rename identifiers, strings, env vars and paths in `src/opendaisugi/`**

Mechanical `sed` first (`gate_mode` -> `warden_mode`, `gate_ask` -> `warden_ask`, `DAISUGI_GATE_` -> `DAISUGI_WARDEN_`, `OPENDAISUGI_GATE_SOCK` -> `OPENDAISUGI_WARDEN_SOCK`, `gate.sock` -> `warden.sock`, `gate.lock` -> `warden.lock`, `"gate"` -> `"warden"`, `daisugi-gate` -> `daisugi-warden`, `opendaisugi.gate` -> `opendaisugi.warden`, `gate check` -> `warden check`, `gate settings` -> `warden settings`, `gate init` -> `warden init`, `gate arm` / `disarm` -> `warden arm` / `disarm`), then read every remaining hit of `git grep -n -i -w gate src/opendaisugi` by hand and rewrite prose and identifiers (`gate_root` -> `warden_root`, `GateState` -> `WardenState`, `_gate_` -> `_warden_`). Keep `delegate`, `aggregate`, `navigate` untouched (the whole-word grep does not list them).

- [ ] **Step 5: Implement `move_old_state` in `install.py` and call it from `install_cmd`**

Signature and the six ordered steps are in "The `daisugi install` one-time move, exactly". The socket probe is `socket.socket(AF_UNIX).connect(path)` with a 0.2 s timeout, inside `try`. Config rewrite is a line-wise regex on `^\s*(gate_mode|gate_ask|gateway_local_model|gateway_router_model|gateway_router|gateway_grader)\s*=` (longest key first).

- [ ] **Step 6: Make the stale-key report in `warden status`**

`load_config` already names unknown keys in its error (check `config.py`; if it swallows them, make it return them in the exception message). `warden status` prints one line per key in the form above and still prints the mode line.

- [ ] **Step 7: Rename the tests' bodies**

`sed` over `tests/` for the same tokens as Step 4, plus `def test_gate` -> `def test_warden`, `def test_gateway` left for Task 3. Then run the renamed warden files only.

Run: `systemd-run --user --scope -p MemoryMax=5G uv run --no-sync pytest -q -x tests/test_warden*.py tests/test_install*.py tests/test_hook_gate_contract.py tests/floor tests/bench tests/test_install_rename_move.py`
Expected: PASS. (`tests/test_hook_gate_contract.py` is renamed to `test_hook_warden_contract.py` in this step.)

- [ ] **Step 8: Commit**

```bash
git add -A src/opendaisugi tests
git commit -m "rename: gate is the warden in the Python oracle

The warden watches and records every tool call; with enforce it stops
the ones outside the envelope. Commands, flags, config keys, env vars,
the hook marker, the socket and the data dir are renamed. daisugi install
moves <data>/gate to <data>/warden once, rewrites the old config keys,
replaces an old hook entry and moves old pi/OpenCode extensions aside.
Nothing is deleted."
```

---

### Task 3: Python oracle, the router and the routing mode

**Files:**
- Rename: `src/opendaisugi/gateway.py` -> `router.py`, `gateway_answers.py` -> `router_answers.py`, `gateway_asgi.py` -> `router_asgi.py`, `gateway_cluster.py` -> `router_cluster.py`, `gateway_distill.py` -> `router_distill.py`, `gateway_journal.py` -> `router_journal.py`, `gateway_openai.py` -> `router_openai.py`, `gateway_pipeline.py` -> `router_pipeline.py`, `gateway_recall.py` -> `router_recall.py`, `gateway_report.py` -> `router_report.py`, `router_switchyard.py` -> `routing_switchyard.py`; `tests/test_gateway*.py` (18) -> `tests/test_router*.py`; `tests/test_gateway_report_switchyard.py` -> `tests/test_router_report_switchyard.py`
- Modify: `src/opendaisugi/cli.py:320-327` (`router_app`), `3117-3230` (`route`, `gateway`), `3595-3770` (`gateway-report`, `router status`), `3847-3898`, `6674-6720` (`install` options), `src/opendaisugi/config.py:60-72`, `src/opendaisugi/install.py:128,1369`, `src/opendaisugi/modules.py`, `src/opendaisugi/dashboard.py`, `src/opendaisugi/routing.py` (only where it names the gateway), `pyproject.toml` extra, `uv.lock`
- Test: `tests/test_cli_router_tree.py` (new)

**Interfaces:**
- Produces: `daisugi router serve [--host --port --upstream ...]` (the old `gateway` options unchanged), `daisugi router report` (old `gateway-report`), `daisugi router status`, `daisugi router label SESSION pass|fail [--note]`, `daisugi router stop`; `daisugi route` unchanged; `daisugi install --router [--routing-mode off|rules|switchyard|knn|model] [--base-url URL]`. The `router_app` is no longer hidden; its help: `The router: a local proxy in front of your model provider. It answers repeated turns from the garden and sends the rest on, by routing mode (rules, switchyard, knn, model, off).`
- Produces: config fields `router_local_model`, `routing_mode: str = "rules"`, `routing_model`, `router_grader`; `OPENDAISUGI_ROUTER_BASE_URL`, `DAISUGI_ROUTER_SCRATCH`; the router root `data_home() / "router"`; `router_journal.TurnRecord.routing_mode: str` (JSON key `routing_mode`); `router_report` reads `rec.get("routing_mode", rec.get("router", ""))`.
- Produces: pyproject extra `opendaisugi[router]`.

- [ ] **Step 1: Write the failing CLI tree test**

```python
# tests/test_cli_router_tree.py
def test_router_tree(runner):
    out = runner.invoke(app, ["router", "--help"]).output
    for verb in ("serve", "report", "status", "label", "stop"):
        assert f" {verb} " in out
    assert runner.invoke(app, ["gateway", "--help"]).exit_code != 0
    assert runner.invoke(app, ["gateway-report", "--help"]).exit_code != 0


def test_install_routing_mode_flag(runner, tmp_home):
    r = runner.invoke(app, ["install", "--router", "--routing-mode", "knn", "--dry-run", "--yes"])
    assert r.exit_code == 0 and "routing_mode" in r.output


def test_report_reads_old_router_key(tmp_path):
    p = tmp_path / "turns.jsonl"
    p.write_text(
        json.dumps({**MIN_TURN, "router": "rules"})
        + "\n"
        + json.dumps({**MIN_TURN, "routing_mode": "knn"})
        + "\n"
    )
    rep = router_report.summarize(RouterJournal(path=p).load())
    assert rep.by_routing_mode == {"rules": 1, "knn": 1}
```

- [ ] **Step 2: Run to see them fail**

Run: `uv run --no-sync pytest tests/test_cli_router_tree.py -q`
Expected: FAIL (`router serve` unknown; `gateway` still answers).

- [ ] **Step 3: `git mv` the files, fix imports**

As Task 2 Step 3, tokens `opendaisugi.gateway` -> `opendaisugi.router`, `router_switchyard` -> `routing_switchyard`.

- [ ] **Step 4: Rebuild the command tree in `cli.py`**

Move `gateway_cmd` under `router_app` as `serve`, `gateway_report_cmd` as `report`; keep `router_status_cmd`, `router_label_cmd`, `router_stop_cmd` where they are; drop the two hidden top-level commands; `add_typer(router_app, name="router", rich_help_panel="Router")`. In `install_cmd`: `--gateway` -> `--router`, the old `--router X` option -> `--routing-mode X`. Replace `data_dir / "gateway"` with `data_dir / "router"` (lines 3211, 3595, 3650, 3745, 3755, 866, 875).

- [ ] **Step 5: Rename config keys, env vars, the JSON key and the extra**

`config.py:60-72`; `TurnRecord.router` -> `routing_mode`; the `.get` chain in `router_report`; `pyproject.toml` `[project.optional-dependencies]` key `gateway` -> `router` and the `[tool.uv]` list; then `uv lock` (network: the lock resolves from cache; if it needs network, run it once outside the no-network rule and say so in the commit).

- [ ] **Step 6: Rename test bodies and run**

Run: `systemd-run --user --scope -p MemoryMax=5G uv run --no-sync pytest -q -x tests/test_router*.py tests/test_cli_router_tree.py tests/test_install*.py tests/test_modules*.py tests/test_dashboard*.py`
Expected: PASS.

- [ ] **Step 7: Full Python suite once**

Run: `systemd-run --user --scope -p MemoryMax=5G uv run --no-sync pytest -q -x -p no:cacheprovider tests/`
Expected: PASS, same count as master minus 0 (new tests add to it).

- [ ] **Step 8: Commit**

```bash
git add -A src/opendaisugi tests pyproject.toml uv.lock
git commit -m "rename: gateway is the router; its inner policy is the routing mode

daisugi router serve|report|status|label|stop replaces gateway,
gateway-report and the hidden router group, so one word names one part
(ruling R-1). Config: routing_mode, routing_model, router_local_model,
router_grader. turns.jsonl writes routing_mode; the report still reads
the old router key on lines written before today."
```

---

### Task 3b: Python oracle, `delegate` becomes `spawn`

**Files:**
- Rename (`git mv`): `src/opendaisugi/delegate.py` -> `spawn.py`, `tests/test_delegate.py` -> `tests/test_spawn.py`
- Modify: `src/opendaisugi/mcp_server.py:407-447` (`def spawn(...)`, the docstring, the `run_spawn` call), `src/opendaisugi/gate.py` or `warden.py` (`_delegate_effects` -> `_spawn_effects`; the tool-name test in the MCP branch), `src/opendaisugi/graft_install.py` and the `Rule` default `redirect.tool`, `src/opendaisugi/cli.py:3749-3840,3942` (`delegate_state` -> `spawn_state`, `delegate_lines` -> `spawn_lines`, `from opendaisugi.spawn import MAX_SEED`), `src/opendaisugi/router_report.py` (its `delegate_state` and `delegate_lines`; the `kind` filter reads `spawn`, else `delegate`), `src/opendaisugi/skills/opendaisugi-checklist/references/when-to-delegate.md` -> `when-to-spawn.md` (and `SKILL.md`, `delegation.md`, `contract-orchestration.md`, `worked-example-council.md` where they name the tool), `clients/go/internal/install/assets/skill/**` copies of the same, `scripts/names-check.sh` (extension below)
- Test: `tests/test_spawn.py`, `tests/test_mcp_server.py`, `tests/test_router_report.py`, `tests/test_warden_graft.py`

**Interfaces:**
- Produces: MCP tool `spawn(path: str, question: str, mode: str = "bulk_read") -> dict` on the `opendaisugi` server; the tool list is `envelope_for find_pathway recall recall_answer verify_plan verify_completed_step list_pathways pathway_stats run_plan receipts_for_run recent_runs spawn`. `spawn.run_spawn(path, question, mode, *, data_dir) -> Result`.
- Produces: journal and delegations records with `kind: "spawn"`; the report's one `.get` chain reads `spawn`, else `delegate` (exception 4, one line, for the owner's existing history).
- Produces: `scripts/names-check.sh` also fails on the code tokens `"delegate"`, `daisugi-delegate`, `opendaisugi.delegate`, `run_delegate`, `delegate.py` outside the allowlist (the English word "delegate" in prose is not matched).
- Produces: no alias. A call to the tool `delegate` gets the host's own "unknown tool" answer.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_mcp_server.py (add)
def test_tool_list_names_spawn_not_delegate():
    names = [t.name for t in asyncio.run(mcp_server.build().list_tools())]
    assert "spawn" in names and "delegate" not in names
    assert len(names) == 12


# tests/test_router_report.py (add)
def test_report_counts_old_delegate_kind_lines(tmp_path):
    write_lines(tmp_path, [{"kind": "delegate", "ok": True}, {"kind": "spawn", "ok": True}])
    assert router_report.spawn_state(tmp_path)["total"] == 2
```

Run: `uv run --no-sync pytest -q tests/test_mcp_server.py tests/test_router_report.py -k "spawn or delegate"`
Expected: FAIL (`spawn` not in the tool list).

- [ ] **Step 2: `git mv` the module and its test, fix imports**

As Task 2 Step 3, tokens `opendaisugi.delegate` -> `opendaisugi.spawn`, `run_delegate` -> `run_spawn`, `from opendaisugi import delegate` -> `from opendaisugi import spawn`. Read every remaining hit of `git grep -n -i -w delegate -- src tests` by hand. Keep `delegating_executor.py`, "delegated step" and "delegation" in prose.

- [ ] **Step 3: Rename the tool, the effects hook, the report functions and the `kind`**

`mcp_server.py` registers `def spawn(...)`. The warden's MCP branch tests for `opendaisugi/spawn`. The report reads `r.get("kind") in ("spawn", "delegate")`. The default graft redirect is `{"tool": "spawn"}`.

- [ ] **Step 4: Print the one envelope line from install**

When `daisugi install` finds `opendaisugi/delegate` in a registered envelope's `mcp_allowlist` under the warden root, print `envelope <id> allows mcp opendaisugi/delegate; add opendaisugi/spawn (the tool was renamed)`. Install edits nothing (W3R-10: no installer edits an envelope). Test: `test_install_names_stale_delegate_allow`. Tasks 5 and 7 port the line.

- [ ] **Step 5: Extend `names-check.sh`, rewrite the skill text, run**

Add the code-token pattern to the script and the skill pages' tool name. Run: `systemd-run --user --scope -p MemoryMax=5G uv run --no-sync pytest -q -x tests/test_spawn.py tests/test_mcp_server.py tests/test_router_report.py tests/test_warden_graft.py tests/test_install*.py`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add src/opendaisugi tests scripts/names-check.sh clients/go/internal/install/assets/skill
git commit -m "rename: the delegate tool is spawn

Owner ruling A1 (2026-10-30): one word, spawn, for the MCP tool, the
module and the executable W3 adds (daisugi-spawn). Clean break, no alias;
the report still reads kind delegate on lines written before today."
```

---

### Task 4: Golden fixtures regenerated from the oracle

**Files:**
- Rename: `clients/fixtures/gate` -> `warden`, `gate_server` -> `warden_server`, `gateway` -> `router`; `clients/gate_cases.py` -> `warden_cases.py`, `gate_compare.py` -> `warden_compare.py`, `gate_fuzz.py` -> `warden_fuzz.py`, `gate.py` -> `warden.py`, `gate_server_cases.py` -> `warden_server_cases.py`, `gate_server_compare.py` -> `warden_server_compare.py`, `gateway_cases.py` -> `router_cases.py`, `gateway_compare.py` -> `router_compare.py`
- Modify: every `clients/*_cases.py` and `*_compare.py` that names a renamed command, env var or path (`cli_cases.py` 333 hits, `sprig_cases.py` 144, `coppice_compare.py` 20, `k4_cases.py` 11, `voice_cases.py` 4, `fence_cases.py`, `coppice_local.py`), `clients/fixture_paths.py`, `clients/fixtures/generate.py`, `.github/workflows/clients.yml:196-260` (matrix names, env, commands), `docs/spec/conformance.md` (the suite names and commands)

- Also (ruling Q): the k3 suite (MCP cases) and any writer that names the tool `delegate` regenerate with `spawn` in the tool list (12 names) and in `redirect.tool`; the Task 3b tests pin the oracle side.
**Interfaces:**
- Consumes: the renamed oracle from Tasks 2 and 3.
- Produces: `clients/fixtures/{warden,warden_server,router,cli,f,fence,garden,k1,k2,k3,k4,l,rank,tree,voice,weave}/cases.jsonl` and their `cases.manifest.json` with new sha256 pins; the other suites (`alias dialect literal models pack pathways robotics toml yaml`) byte-identical to master.

- [ ] **Step 1: Rename the writers and compare scripts, rewrite their tokens**

`git mv` as listed; `sed` the tokens of Tasks 2 and 3 over `clients/*.py`; read the remaining `git grep -n -i -w -E 'gate|gateway' clients/*.py` hits by hand (the sprig writer passes `--gate`; it becomes `--warden` here, and Task 10 makes sprig accept it; the weave suite is regenerated again in Task 10 after that).

- [ ] **Step 2: Regenerate the 16 suites, one at a time, under the cap**

```bash
cd /mnt/tera/working/programming/openDaisugi-w2
export DAISUGI_WARDEN_SCRATCH=~/opendaisugi-scratch/w2-rename/warden DAISUGI_ROUTER_SCRATCH=~/opendaisugi-scratch/w2-rename/router DAISUGI_WARDEN_SERVER_SCRATCH=~/opendaisugi-scratch/w2-rename/server
for s in warden warden_server router cli f fence garden k1 k2 k3 k4 l rank tree voice; do
  systemd-run --user --scope -p MemoryMax=5G --wait uv run --no-sync python clients/${s}_cases.py || { echo "FAILED $s"; break; }
done
```
Expected: each writer prints its count and manifest; `git status` shows changes only under the 16 suites (weave is regenerated in Task 10).

- [ ] **Step 3: Leak check and the untouched suites**

Run: `uv run --no-sync python clients/fixture_paths.py && git status --short clients/fixtures | grep -E 'alias|dialect|literal|models|pack|pathways|robotics|toml|yaml'`
Expected: the leak check exits 0; the grep prints nothing.

- [ ] **Step 4: No old word inside the regenerated suites**

Run: `git grep -I -i -w -c -E 'gate|gateway|foreman' -- clients/fixtures | grep -v -E 'weave'`
Expected: nothing (weave still carries `--gate` until Task 10).

- [ ] **Step 5: Update CI and the conformance spec**

`clients.yml:196` matrix: `warden, cli, pathway, garden, router, warden_server, ...`; lines 223-228 env names; lines 255-260 commands (`warden_compare.py --binary "$B/daisugi-warden"`, `router_compare.py`, `warden_server_compare.py`); lines 88-97, 168-173 binary names (`daisugi-warden`, `router-probe`). `docs/spec/conformance.md` suite names and commands.

- [ ] **Step 6: Commit**

```bash
git add -A clients .github/workflows/clients.yml docs/spec/conformance.md
git commit -m "rename: golden fixtures rewritten from the renamed oracle

16 suites carried a renamed command, env var or path and are regenerated
with new manifests; 9 suites are byte-identical to master. CI matrix and
the conformance spec follow."
```

---

### Task 5: Go daisugi, the warden

**Files:**
- Rename: `clients/go/internal/gate` -> `internal/warden`, `internal/gateroot` -> `internal/wardenroot`, `cmd/daisugi-gate` -> `cmd/daisugi-warden`, `internal/cli/gatecmds.go` -> `wardencmds.go`, `internal/install/assets/opencode-daisugi-gate.ts` -> `opencode-daisugi-warden.ts`, `internal/gate/testdata/*` with the dir
- Modify: `internal/cli/cli.go:364` (`case "warden"`), `internal/cli/installcmd.go`, `internal/install/*.go` (marker, hook strings, pi and OpenCode paths, the port of `move_old_state`), `internal/install/assets/pi-index.ts:46`, `internal/install/assets/skill/**` (11 hits), `internal/datahome`, `internal/delegate/*.go`, `internal/pmodel/surrogate_test.go`, `clients/go/scripts/build.sh`, `clients/go/scripts/check-paths.sh`, `clients/go/README.md`, `clients/go/internal/gate/gen_argparse.py`, `gen_patterns.py` (move with the dir)

- Also (ruling Q): `git mv clients/go/internal/delegate clients/go/internal/spawn` (package `spawn`, `Name = "spawn"`), `internal/mcpwire/args.go` key `"spawn": {Name: "spawnArguments", ...}`, `tools_gen.go` `ToolNames` (regenerate it from the oracle; last name `spawn`), `internal/cli/mcpcmd.go:252` `case "spawn"`, `graftcmd.go:261` `Set("tool", "spawn")`, `routercmd.go:319` and `routermeasure.go` (`spawn_state`, the `kind` read of `spawn` else `delegate`), `internal/gate/gate.go` and `graft.go` (now `warden`) tool-name tests, and `internal/install/assets/skill/**` pages named in Task 3b.
**Interfaces:**
- Consumes: Task 2's surface (commands, flags, keys, env vars, marker, `warden.sock`, `<data>/warden`).
- Produces: package `warden` (`import "daisugi-verify/internal/warden"`), `wardenroot`; binary `clients/go/daisugi-warden`; `install.MoveOldState(dataDir, home string, now time.Time, out io.Writer) ([]string, error)` with the six steps and the same printed lines as Python.

- [ ] **Step 1: Write the failing Go test for the move**

`internal/install/moveold_test.go`: `TestMoveOldStateMovesDirs`, `TestMoveOldStateRefusesBoth`, `TestMoveOldStateRefusesLiveSocket` (listen on a unix socket in `t.TempDir()`), `TestMoveOldStateRewritesConfigKeys`, `TestInstallReplacesOldMarkerHook`, `TestInstallMovesOldExtensionsAside`. Same inputs and expected lines as `tests/test_install_rename_move.py`.

- [ ] **Step 2: Run to see them fail**

Run: `cd clients/go && go test ./internal/install -run 'TestMoveOldState|TestInstallReplaces|TestInstallMoves' 2>&1 | head`
Expected: compile error, `undefined: MoveOldState`.

- [ ] **Step 3: `git mv` the directories and files; `sed` package names and import paths**

`package gate` -> `package warden`, `package gateroot` -> `package wardenroot`, `internal/gate"` -> `internal/warden"`, `gate.` qualifiers -> `warden.` (only where the qualifier is the package; check with `go build ./...`), identifiers `Gate` -> `Warden` in exported names (12 types, the funcs that `go vet` and the compiler point at), env var names, `gate.sock`, `"gate"` dir literal, `daisugi-gate`, the hook strings at `install/*.go:17,22,26,29,35,40,42,71,90,107,112,199,292,393`.

- [ ] **Step 4: Implement `MoveOldState` and call it from `install`**

Same six steps. Rewrite `assets/pi-index.ts` and `assets/opencode-daisugi-warden.ts` to read `OPENDAISUGI_WARDEN_SOCK` and `warden.sock`; keep them byte-identical to the Python copies under `src/opendaisugi/harness_*` (there is a test for that already; find it with `git grep -l harness_opencode clients/go`).

- [ ] **Step 5: Build and test under the cap**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && scripts/build.sh && go test -p 1 ./...'`
Expected: `daisugi-warden`, `daisugi`, `conform` built; all tests PASS.

- [ ] **Step 6: Compare against the new warden fixtures**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait uv run --no-sync python clients/warden_compare.py --binary clients/go/daisugi-warden --oracle` then the same for `clients/warden_server_compare.py --binary clients/go/daisugi --port 18999` and `clients/cli_compare.py --binary clients/go/daisugi --oracle`.
Expected: `0 disagreements` on each; the cli suite may list the same `not ported` and `refused` ids as `clients/go/README.md` records (12 and 9), none new.

- [ ] **Step 7: Commit**

```bash
git add -A clients/go
git commit -m "rename: gate is the warden in the Go daisugi

Package warden, wardenroot and the daisugi-warden binary. install moves
<data>/gate once and replaces an old hook, as the oracle does. Compares
agree with the regenerated warden, warden_server and cli fixtures."
```

---

### Task 6: Go daisugi, the router and the routing mode

**Files:**
- Rename: `clients/go/internal/gateway` -> `internal/router`; `cmd/gateway-probe` -> `cmd/router-probe`; `internal/cli/routercmd.go` -> `routingcmd.go`, `routermeasure.go` -> `routingmeasure.go`, `installrouter.go` -> `installrouting.go`; then `internal/cli/gatewaycmd.go` -> `routercmd.go`; `internal/install/gateway.go` -> `router.go`; `internal/switchyard` stays
- Modify: `internal/cli/cli.go:382-396` (`case "router"` dispatches `serve|report|status|label|stop`; drop `gateway`, `gateway-report`), `internal/cli/installcmd.go` (`--router`, `--routing-mode`), `internal/cli/modulescmd.go`, `piecescmd.go`, `statuscmd.go`, `internal/router/report.go` (the `.get` chain: `routing_mode`, else `router`), `internal/config` (keys), `clients/go/README.md`

**Interfaces:**
- Consumes: Task 3's surface.
- Produces: package `router`; binary `router-probe`; the same command tree as Python; config keys read as `routing_mode`, `routing_model`, `router_local_model`, `router_grader`.

- [ ] **Step 1: Write the failing test**

`internal/cli/routertree_test.go`: `TestRouterTree` runs `daisugi router --help` through the in-process CLI and asserts the five verbs; asserts `daisugi gateway` prints `unknown command` and exits 2. `internal/router/report_test.go`: `TestReportReadsOldRouterKey` with the two-line journal of Task 3.

- [ ] **Step 2: Run to see them fail**

Run: `cd clients/go && go test ./internal/cli -run TestRouterTree ./internal/router -run TestReportReadsOldRouterKey`
Expected: FAIL.

- [ ] **Step 3: Rename, in the order listed under Files (the two-step `routercmd.go` swap avoids a collision)**

Package `gateway` -> `router`; `Gateway` -> `Router` in exported names; the inner `Router` type that holds the policy (`rules|switchyard|knn|model|off`) becomes `RoutingMode`; env vars; `"gateway"` dir literal -> `"router"`; JSON tag `router` on the turn record -> `routing_mode`.

- [ ] **Step 4: Build, test, compare**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && scripts/build.sh && go build -o ~/opendaisugi-scratch/w2-rename/router-probe ./cmd/router-probe && go build -o ~/opendaisugi-scratch/w2-rename/recall-probe ./cmd/recall-probe && go build -o ~/opendaisugi-scratch/w2-rename/fake-upstream ./cmd/fake-upstream && go test -p 1 ./...'`
then: `DAISUGI_RECALL_PROBE=~/opendaisugi-scratch/w2-rename/recall-probe systemd-run --user --scope -p MemoryMax=5G --wait uv run --no-sync python clients/router_compare.py --binary clients/go/daisugi --probe ~/opendaisugi-scratch/w2-rename/router-probe --fake ~/opendaisugi-scratch/w2-rename/fake-upstream` (take the exact flags from the old `gateway_compare.py` docstring), then `clients/cli_compare.py --binary clients/go/daisugi --oracle`, `clients/k4_compare.py --binary clients/go/daisugi`.
Expected: `0 disagreements`; the one refused id `GW-8` stays refused.

- [ ] **Step 5: Commit**

```bash
git add -A clients/go
git commit -m "rename: gateway is the router in the Go daisugi; routing mode is the inner policy"
```

---

### Task 7: Rust daisugi, warden and router

**Files:**
- Rename: `clients/rust/src/gate` -> `src/warden`, `src/gateway` -> `src/router`, `src/bin/daisugi_gate.rs` -> `daisugi_warden.rs`, `src/bin/gateway_probe.rs` -> `router_probe.rs`, `src/cli/gatecmds.rs` -> `wardencmds.rs`, `src/cli/gateroot.rs` -> `wardenroot.rs`, `src/cli/routercmd.rs` -> `routingcmd.rs`, `routermeasure.rs` -> `routingmeasure.rs`, `installrouter.rs` -> `installrouting.rs`, then `src/cli/gatewaycmd.rs` -> `routercmd.rs`, `src/cli/installgw.rs` -> `installrouter.rs`, `tests/gate_backstop.rs` -> `warden_backstop.rs`
- Modify: `Cargo.toml` `[[bin]]` names (`daisugi-warden`, `router-probe`), `src/lib.rs:19-33` (`pub mod warden; pub mod router; pub mod wardenstate; pub mod wardenroot;`), `src/cli/mod.rs:774-785`, `src/cli/install.rs`, `layers.rs`, `layer_assets.rs`, `modulescmd.rs:740-1154`, `servecmd.rs:63`, `src/warden/statefile.rs`, `statewrite.rs` (log lines `daisugi warden: not reading ...`), `src/delegate.rs`, `clients/rust/README.md`, `clients/rust/tools/check-paths.sh`, `.github/workflows/clients.yml:168-173`

- Also (ruling Q): `git mv clients/rust/src/delegate.rs clients/rust/src/spawn.rs`, `src/lib.rs` (`pub mod spawn;`), `src/mcpwire/mod.rs` and `tools_gen.rs` (`spawn`), `src/cli/mcpcmd.rs`, `graftcmd.rs`, `routercmd.rs`, `routermeasure.rs`, `weavecmd.rs`, `layer_assets.rs`, `src/warden/decide.rs` and `graft.rs`, `clients/rust/README.md`. Same `.get` chain for old `kind: delegate` lines.
**Interfaces:**
- Consumes: Tasks 2 and 3.
- Produces: bins `daisugi-warden`, `router-probe`; mods `warden`, `router`, `wardenstate`, `wardenroot`; `install::move_old_state(data_dir: &Path, home: &Path, now: OffsetDateTime, out: &mut dyn Write) -> Result<Vec<String>, Exit>` with the same lines.

- [ ] **Step 1: Write the failing tests**

In `src/cli/install.rs` tests module: `move_old_state_moves_dirs`, `refuses_both`, `refuses_live_socket`, `rewrites_config_keys`, `replaces_old_marker_hook`, `moves_old_extensions_aside`. Same inputs and lines.

- [ ] **Step 2: Run to see them fail**

Run: `cd clients/rust && cargo test -j2 --release --lib install:: 2>&1 | tail -3`
Expected: compile error, `move_old_state` not found.

- [ ] **Step 3: `git mv` and `sed` (`mod gate` -> `mod warden`, `gate::` -> `warden::`, `gateway::` -> `router::`, `Gate` -> `Warden`, `Gateway` -> `Router`, the inner policy type -> `RoutingMode`, env vars, `gate.sock`, dir literals, hook strings)**

Read every remaining `git grep -n -i -w -E 'gate|gateway' clients/rust/src` hit by hand.

- [ ] **Step 4: Implement `move_old_state`, call it from `install`**

- [ ] **Step 5: Build and test under the cap**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait bash -c 'cd clients/rust && cargo build -j2 --release && cargo test -j2 --release'`
Expected: builds `daisugi`, `daisugi-warden`, `conform`, `router-probe` and the other probes; tests PASS. (The Rust README notes a 4 GB cap about doubles build time; expect minutes, not seconds.)

- [ ] **Step 6: Compare**

Run (one at a time, each under the cap): `clients/warden_compare.py --binary clients/rust/target/release/daisugi-warden --oracle`; `clients/cli_compare.py --binary clients/rust/target/release/daisugi --oracle`; `clients/router_compare.py --binary clients/rust/target/release/daisugi --probe clients/rust/target/release/router-probe ...`; `clients/warden_server_compare.py --binary clients/rust/target/release/daisugi --port 18998`.
Expected: `0 disagreements`; `status config start journal` still `not ported`.

- [ ] **Step 7: Commit, then delete the cargo target to free disk**

```bash
git add -A clients/rust .github/workflows/clients.yml
git commit -m "rename: warden and router in the Rust daisugi, mirroring the Go port"
rm -rf clients/rust/target
```

---

### Task 8: coppice (Go), the steward, the warden words and Pando labels

**Files:**
- Rename: `harness/coppice/internal/foreman` -> `internal/steward`, `internal/gatefs` -> `internal/wardenfs`, `internal/config/foreman.go` -> `steward.go` (+ `_test.go`), `internal/server/foreman_*.go` (35 files) -> `steward_*.go`, `internal/server/swap_gateway_test.go` -> `swap_router_test.go`, `internal/tui/foreman_test.go` -> `steward_test.go`, `skills/foreman` -> `skills/steward`, `skills/foreman-turn` -> `skills/steward-turn`, `skills/foreman_turn_test.go` -> `steward_turn_test.go`, `e2e/fake-foreman.sh` -> `fake-steward.sh`, `testdata/transcripts/chat-foreman.py` -> `chat-steward.py`, `testdata/events/gate-blocked*.json` -> `warden-blocked*.json`
- Modify: `internal/cli/cli.go:35-70` (usage), `:182` (`case "steward"`), `:203-207` (`--warden-root`, `warden-refactor`), `:320`; `internal/steward/cli.go:14-16` (usage: `coppice steward: read the steward's Pando R2 log. Only import changes anything.`), `internal/steward/dir.go` (keep the on-disk name behind `const DirName = "foreman"` with the ADR-0023 comment), `internal/steward/tools.go` (tool `foreman` -> `steward`), `internal/steward/prompt_compress.txt`, `prompt_merge.txt`, `internal/config/config.go:53-90` (`Router *string toml:"router"`, `DefaultRouter`, `RouterURL()`; `Steward Steward toml:"steward"`; refuse `gateway` and `[foreman]` by name), `internal/server/fence.go:314` (marker), `internal/server/*.go` protocol verbs and fields, `internal/state` (`steward.json`), `internal/web/config.go:34,243` (`warden_root`), `internal/web` and `internal/tui` labels (`Pando` header on the zoom view), `internal/proto`, `plugins/README.md:121,184-190`, `plugins/_lib/notice.py:8-44` (`Warden says yes.`), `plugins/*` (21 hits), `PROTOCOL.md`, `README.md` (`[steward]`, `router = `, `The steward as a chat`, Pando R2 paragraph at line 85 and the log paragraph after 812), `PINS.md` if it names a word, `testdata/golden/README.md`

**Interfaces:**
- Consumes: Task 2's marker, `warden check`, `--warden-root`; Task 3's router URL default `http://127.0.0.1:8787` (unchanged).
- Produces: protocol v1 verbs `task.set_steward`, `floor.steward`; fields `steward`, `steward_label`, `steward_turn.ran_on`, `warden` (pane object), `source: "warden"`, `warden_root`; magic pane label `steward`; `coppice.toml` keys `router`, `[steward]`; env `COPPICE_STEWARD_HELPER`; `coppice steward log|view|zoom|date|stats|import|pool`; `coppice skill steward`; `coppice --warden-root DIR`; `steward.DirName`; state file `steward.json`.

- [ ] **Step 1: Write the failing tests**

`internal/config/steward_test.go`: `TestOldForemanTableRefused` (a `coppice.toml` with `[foreman]` makes `Load` return an error whose text is `coppice.toml: [foreman] is now [steward]`), `TestOldGatewayKeyRefused` (`gateway = "..."` -> `coppice.toml: gateway is now router`). `internal/server/steward_test.go`: `TestMagicLabelIsSteward` (a pane labelled `steward` is the floor's steward; one labelled `foreman` is not). `internal/steward/dir_test.go`: `TestDirNameStaysForemanUntilADR0023` asserts `DirName == "foreman"`.

- [ ] **Step 2: Run to see them fail**

Run: `cd harness/coppice && go test ./internal/config -run 'TestOld' ./internal/server -run TestMagicLabel ./internal/steward -run TestDirName`
Expected: compile error (`internal/steward` does not exist).

- [ ] **Step 3: `git mv` everything listed; `sed` the package name, import paths, identifiers (`Foreman` -> `Steward`, `foreman` -> `steward`), protocol strings, `gate` -> `warden` in the words that mean our warden (`gate_root`, `gatefs`, `source: "gate"`, `blocked via gate`, the pane `gate` object), `COPPICE_FOREMAN_HELPER`**

Then read every remaining hit of `git grep -n -i -w -E 'gate|gateway|foreman' harness/coppice` by hand. `dir.go` keeps the one `"foreman"` literal as `DirName`.

- [ ] **Step 4: Implement the two config refusals and the state file name**

- [ ] **Step 5: Regenerate the steward golden bytes (the prompts changed)**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait bash -c 'cd harness/coppice && go test ./internal/steward/ -run TestGoldenFixtures -update'`
Expected: files under `internal/steward/testdata/golden/out/` rewritten; `git diff --stat` shows only word changes inside them.

- [ ] **Step 6: Build and test under the cap**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd harness/coppice && go build ./cmd/coppice && go test -p 1 ./...'`
Expected: PASS, including the race-detector job's package if the README runs it locally (`go test -race ./internal/server` under the same cap, separately).

- [ ] **Step 7: Hand check on scratch state only**

```bash
D=~/opendaisugi-scratch/w2-rename/coppice; mkdir -p $D
harness/coppice/coppice --socket $D/sock --data-dir $D/data server start
harness/coppice/coppice --socket $D/sock --data-dir $D/data steward log
harness/coppice/coppice --socket $D/sock --data-dir $D/data server stop
```
Expected: the usage says `steward`; the log dir is `$D/data/foreman` (kept, ruling J); the server stops by its own verb.

- [ ] **Step 8: Commit**

```bash
git add -A harness/coppice
git commit -m "rename: foreman is the steward in coppice; gate words are warden; the chat log is Pando R2

Protocol v1 verbs and fields follow (task.set_steward, floor.steward,
warden, warden_root). coppice.toml takes [steward] and router=, and
refuses the old names by name. The on-disk foreman/ directory keeps its
name behind steward.DirName until ADR-0023 moves the store into daisugi,
so the owner's log moves once, not twice."
```

---

### Task 9: coppice-rs mirror

**Files:**
- Rename: `harness/coppice-rs/src/foreman` -> `src/steward`, `src/server/foreman_*.rs` (12) -> `steward_*.rs`, `src/server/gatefs.rs` -> `wardenfs.rs`, `cases/chat/foreman-*.jsonl` (21) -> `steward-*.jsonl`, `cases/facts/foreman*.jsonl` -> `steward*.jsonl`, `cases/ended/swap-gateway.jsonl` -> `swap-router.jsonl`
- Modify: every string the Go side changed in Task 8 (`sed` the same token list over `src/` and `cases/`), `clients/coppice_compare.py`, `clients/coppice_local.py`, `harness/coppice-rs/PINS.md`

**Interfaces:**
- Consumes: Task 8's protocol and config names.
- Produces: the Rust `coppice` binary agreeing with the Go one on every case under `cases/` and on the suites `clients/coppice_compare.py` builds from the Go testdata.

- [ ] **Step 1: `git mv` and `sed`; read the remaining hits by hand**

- [ ] **Step 2: Build and test under the cap**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait bash -c 'cd harness/coppice-rs && cargo build -j2 --release && cargo test -j2 --release'`
Expected: PASS.

- [ ] **Step 3: Compare Go and Rust servers side by side**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait uv run --no-sync python clients/coppice_compare.py --go harness/coppice/coppice --rust harness/coppice-rs/target/release/coppice --scratch ~/opendaisugi-scratch/w2-rename/cc` (take the exact flags from the script's docstring; it passes its own sockets and data dirs under `--scratch`).
Expected: every case agrees; none refused beyond those the script's README lists today.

- [ ] **Step 4: Commit, delete the target**

```bash
git add -A harness/coppice-rs clients/coppice_compare.py clients/coppice_local.py
git commit -m "rename: the Rust coppice mirrors the steward and warden names; compare cases renamed"
rm -rf harness/coppice-rs/target
```

---

### Task 10: sprig and sprig-rs, then the weave fixtures

**Files:**
- Rename: `harness/sprig/gate.go` -> `warden.go` (+ `_test`), `daisugi_gate.go` -> `daisugi_warden.go` (+ `_test`), `harness/sprig-rs/src/gate.rs` -> `warden.rs`
- Modify: `harness/sprig/cli.go:99-103` (`--warden`, `--warden-cmd`, `DefaultWardenCmd()`), `harness/sprig/README.md`, `harness/sprig/hook`, `harness/sprig/mcp`, `harness/sprig/weave` where they name the flag, `harness/coppice/internal/adapters` (it reads `cli.go` flag names; a test there breaks until this lands), `clients/sprig_cases.py`, `clients/weave_cases.py`, `clients/k2_cases.py` (if it passes the flag), `clients/fixtures/weave`, `clients/fixtures/k2`, `.github/workflows/clients.yml:496-573` comments and commands

**Interfaces:**
- Produces: `sprig --warden --warden-cmd CMD`; default command `daisugi warden check`; `sprig-hook` writes the new marker.

- [ ] **Step 1: Rename flags and files; `sed` the tokens; read remaining hits**

- [ ] **Step 2: Build and test both**

Run: `systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd harness/sprig && go build ./... && go test -p 1 ./...'`, then `systemd-run --user --scope -p MemoryMax=5G --wait bash -c 'cd harness/sprig-rs && cargo build -j2 --release && cargo test -j2 --release'`
Expected: PASS. Then `cd harness/coppice && go test ./internal/adapters` PASS.

- [ ] **Step 3: Regenerate weave and k2, compare**

Run under the cap: `uv run --no-sync python clients/weave_cases.py`, `clients/k2_cases.py`, then `clients/weave_compare.py --binary clients/go/daisugi`, `clients/k2_compare.py --binary clients/go/daisugi`.
Expected: `0 disagreements`; weave keeps its 2 refused (`WV-R-10`).

- [ ] **Step 4: Names check on fixtures**

Run: `git grep -I -i -w -c -E 'gate|gateway|foreman' -- clients/fixtures`
Expected: nothing.

- [ ] **Step 5: Commit; delete the sprig-rs target**

```bash
git add -A harness/sprig harness/sprig-rs harness/coppice/internal/adapters clients
git commit -m "rename: sprig --warden and --warden-cmd; weave and k2 fixtures regenerated"
rm -rf harness/sprig-rs/target
```

---

### Task 11: Install script, release, packaging, CI, examples

**Files:**
- Modify: `scripts/install.sh` (4), `scripts/release.sh` (13: tarball lists `daisugi-warden`), `scripts/smoke-tarball.sh` (14: first warden decision), `packaging/aur/opendaisugi/PKGBUILD:5,38,120`, `.SRCINFO:2`, `packaging/aur/opendaisugi-bin/PKGBUILD:7,19,45`, `.SRCINFO:2`, `packaging/aur/daisugi-py/PKGBUILD:12`, `.SRCINFO:2` (`pkgdesc`: `Agent floor (coppice), loop (sprig) and tool-call warden (daisugi) for AI coding agents`; file `daisugi-warden-rs`), `.github/workflows/ci.yml:34-60` (`daisugi warden audit`; comments "merge check"), `.github/workflows/coppice.yml:63,95` (comments), `examples/**` (53 hits: `jit-metrics`, `dish-wash` READMEs and scripts; `examples/gateway-spike/` is allowlisted and untouched)

- [ ] **Step 1: Edit each file; read the remaining hits by hand**

Run: `git grep -n -i -w -E 'gate|gated|gates|gateway|foreman' -- scripts packaging .github examples ':!examples/gateway-spike'`
Expected: nothing after the edits.

- [ ] **Step 2: Dry-run the installer and the tarball smoke**

Run: `INSTALL_DRY=1 scripts/install.sh` and `systemd-run --user --scope -p MemoryMax=5G --wait scripts/smoke-tarball.sh` (it builds into its own scratch HOME; point it at `~/opendaisugi-scratch/w2-rename/smoke` if it takes a dir; check its header).
Expected: the plan names `daisugi-warden`; the smoke reaches the first warden decision and exits 0.

- [ ] **Step 3: Commit**

```bash
git add -A scripts packaging .github examples
git commit -m "rename: install, release, AUR and CI say warden and router"
```

---

### Task 12: Current docs, site, skill assets

**Files:**
- Rename: `docs/how-to/gate.md` -> `warden.md`, `token-saving-gateway.md` -> `router.md`, `router-switchyard.md` -> `routing-mode-switchyard.md`, `foreman-any-model.md` -> `steward-any-model.md`, `foreman-aloud.md` -> `steward-aloud.md`, `bring-your-own-foreman.md` -> `bring-your-own-steward.md`; `docs/recipes/gate-only.md` -> `warden-only.md`, `gateway-router.md` -> `router-and-routing-mode.md`, `coppice-claude-gate.md` -> `coppice-claude-warden.md`
- Modify: `README.md`, `VISION.md`, `AGENTS.md` (tables, install block, binaries table, "What is proven" suite names), `docs/README.md`, `docs/start-here.md` (commands; add the section of Task 13 Step 3), `docs/feature-status.md`, `docs/roadmap.md`, `docs/whitepaper.md`, `docs/spec/**` (81 hits; `yellow-paper.md` names the gate as a formal object: rename to warden with the same symbols), `docs/reference/**`, `docs/how-to/**`, `docs/recipes/**`, `docs/tutorials/**`, `docs/explanation/**`, `docs/architecture/**`, `docs/harness/**`, `docs/limitations.md`, `docs/hook-integration.md`, `docs/client-diversity.md`, `docs/pathway-skill-format.md`, `docs/adr/README.md` (only the index lines of ADR-0026, added in Task 13; old lines untouched), `site/index.html` (commands at lines 265, 277, 349, 394 and the links to renamed docs), `site/CLAIMS.md` (one line: commands renamed 2026-10-30, this plan), `src/opendaisugi/skills/opendaisugi-checklist/**` and `clients/go/internal/install/assets/skill/**` (same bytes; 11 hits)
- Regenerate: `site/start-here.html`, `site/whitepaper.html`, `site/yellow-paper.html` by `uv run site/build.py`

**Interfaces:**
- Consumes: every command, flag, key and path from Tasks 2 to 10, exactly as they print in `--help`.

- [ ] **Step 1: `git mv` the nine doc files; fix every link to them**

Run: `git grep -n -E 'how-to/(gate|token-saving-gateway|router-switchyard|foreman-[a-z-]+|bring-your-own-foreman)\.md|recipes/(gate-only|gateway-router|coppice-claude-gate)\.md'`
Expected after the fix: nothing outside allowlisted paths.

- [ ] **Step 2: Rewrite the prose**

Rules: STE; the warden is "an observer and a supervisor: it watches and records every action, and with enforce on it stops the ones outside the envelope"; the router is "a local proxy in front of your model provider"; routing mode is "how the router picks a model"; the steward is "the role in the chair, whoever answers you, on any model"; Pando R2 on first mention, Pando after. Generic "gate" in our own prose ("merge gate") becomes "merge check". Do not paraphrase a command; copy it from `--help`.

- [ ] **Step 3: Regenerate the site and check it**

Run: `uv run site/build.py && uv run --no-sync pytest -q site/test_build.py && git grep -n -i -w -E 'gate|gateway|foreman' -- site docs README.md VISION.md AGENTS.md ':!docs/plans' ':!docs/adr' ':!docs/research' ':!docs/exploration' ':!docs/correspondence.md'`
Expected: build and test pass; the grep prints nothing.

- [ ] **Step 4: Commit**

```bash
git add -A docs site README.md VISION.md AGENTS.md src/opendaisugi/skills clients/go/internal/install/assets/skill
git commit -m "docs: warden, router, routing mode, steward and Pando R2 in every current page and on the site

Dated plans, ADRs 1 to 22, research, exploration, correspondence, the
CHANGELOG history and ADJUDICATIONS keep their words: they are records."
```

---

### Task 13: The record, the re-install step, the check passes, the full run

**Files:**
- Create: `docs/adr/0026-names-warden-router-steward-pando.md`
- Modify: `docs/adr/README.md` (one new index line), `CHANGELOG.md` (one new entry at the top), `docs/start-here.md` (the section below), `scripts/names-allow.txt` (final `phrase` lines, each with a reason)

**Interfaces:**
- Produces: `scripts/names-check.sh` exits 0 on the branch head.

- [ ] **Step 1: Write ADR-0026**

Status Accepted, date 2026-10-30. Sections: Names (the four rulings with the one-line descriptions and the lore names), Decision (clean break, R-1 and R-2, the three narrow read-only exceptions D, G and J, the `pando` identifier and why `r2` is not in it), Consequences (the owner's one re-install; friends on old docs: none exist; the allowlist and the check), Records (historical documents keep their words; this ADR is the record). Under 80 lines.

- [ ] **Step 2: Write the CHANGELOG entry**

One entry, `## Unreleased`: the four renames, the command tree (`daisugi warden`, `daisugi router serve|report|status|label|stop`, `daisugi install --warden --router --routing-mode`, `coppice steward`, `sprig --warden`), the six config keys, the 20 env vars, the two directory moves, "no alias; run `daisugi install` once".

- [ ] **Step 3: Write the owner's re-install section in `docs/start-here.md`**

Insert after the "Turn on enforce" bullet, heading `## After the rename (one time)`, this text:

```markdown
## After the rename (one time)

The parts have new names: the gate is the warden, the gateway is the router,
the foreman is the steward, and the chat memory is Pando R2. Commands, config
keys and environment variables changed with them. There are no aliases. Do
this once:

1. Stop coppice (`coppice server stop`, with your usual `--socket` if you set
   one) and close any `daisugi start` window.
2. Build and install the new binaries: `scripts/install.sh --replace-daisugi`.
3. Run `daisugi install --warden` (add `--enforce` if you had it, `--router`
   if you used the gateway). It prints one line for each thing it moves:
   `~/.opendaisugi/gate` becomes `~/.opendaisugi/warden`,
   `~/.opendaisugi/gateway` becomes `~/.opendaisugi/router`, the old keys in
   `config.toml` get their new names, the old hook line in
   `~/.claude/settings.json` is replaced, and an old pi or OpenCode extension
   is moved under `~/.opendaisugi/moved-aside/`. Nothing is deleted. If it
   refuses, it says why on one line; fix that and run it again.
4. Edit `coppice.toml` by hand: `[foreman]` is now `[steward]`, and
   `gateway = ` is now `router = `. coppice refuses to start until you do and
   names the key.
5. Start coppice. The steward's log stays where it was (`foreman/` beside the
   coppice data dir); ADR-0023 moves it into daisugi later, in one step. If
   the floor had a named steward, name it again with `floor talk` or
   `coppice task set-steward`; the old `foreman.json` is left in place.
6. Check: `daisugi warden status` shows your mode and no stale key;
   `daisugi router status` answers if you installed the router.
```

- [ ] **Step 4: Finish the allowlist and run the check**

Add `phrase` lines only for third-party or generic text that survived Step O of the rulings and cannot be rewritten (expected: none, or one or two in quoted material), each with `# why`.

Run: `scripts/names-check.sh`
Expected: exit 0, no output.

- [ ] **Step 5: The full runs, one at a time, each under the cap**

```bash
systemd-run --user --scope -p MemoryMax=5G --wait uv run --no-sync pytest -q -p no:cacheprovider tests/
systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./...'
systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd harness/coppice && go test -p 1 ./...'
systemd-run --user --scope -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd harness/sprig && go test -p 1 ./...'
systemd-run --user --scope -p MemoryMax=5G --wait bash -c 'cd clients/rust && cargo test -j2 --release' ; rm -rf clients/rust/target
uv run ruff check src tests clients
```
Expected: all PASS; ruff clean.

- [ ] **Step 6: Commit**

```bash
git add docs/adr/0026-names-warden-router-steward-pando.md docs/adr/README.md CHANGELOG.md docs/start-here.md scripts/names-allow.txt
git commit -m "rename: ADR-0026 records the names; start-here carries the one-time re-install; names check passes"
```

- [ ] **Step 7: Report**

Report to the owner: the commit list, `scripts/names-check.sh --count` = 0, each compare's agree/refused/disagree line, `df -h /mnt/tera`, and that `~/.opendaisugi`, `~/.local/state/coppice` and the live coppice server were not touched. Do not fold or push; the owner re-installs from the branch and the fold follows the owner's verdict.

---

## Hand checks for the owner (after merge, on the owner's box)

1. `daisugi install --warden --dry-run` first: read the move lines. Then without `--dry-run`.
2. `daisugi warden status`; `ls ~/.opendaisugi` shows `warden/` and `router/`, no `gate/`, no `gateway/`.
3. Open Claude Code in a project; run one tool call; `daisugi warden report` shows it.
4. `coppice` starts; `ctrl-t` talks to the steward; `coppice steward log` prints the old log.
5. If anything refuses, the one line it prints names the fix. Nothing was deleted, so the old state is where the line says.

## Self-review notes

- Spec coverage: rulings A to P each map to a task (A, C, E: 2, 3, 5 to 10; B: 2, 3, 8; D, F: 2, 5, 7; G: 3, 6, 7; H: 2; I, J, M: 8, 9; K: 2 to 9; L: 4, 10; N, O: 12; P: 8, 12, 13). Historical documents: Task 1 allowlist, Task 13 ADR.
- Names used across tasks: `move_old_state` / `MoveOldState` / `move_old_state`; `routing_mode`; `DirName`; `names-check.sh`; `daisugi-warden`; `router-probe`. Checked for consistency.
- Proportion: the plan decides names, order, exceptions and checks. Bodies are left to the builder except the start-here text, which is copy.

# W3: fewer dependencies, self-testing and faster test runs, CI that stays green Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close roadmap sections 8 and 9. (1) Every dependency in Python, Go, Rust, JavaScript, Lean and the native prefix has one row in a machine-readable register with its licence, what uses it, and whether it earns its place; a CI step fails on a dependency with no row, on a lock that grew with no committed name, and on `litellm` or `instructor` anywhere. (2) The dependencies that do not earn their place are removed or replaced (DS-4 to DS-8 and the "Open" list of `docs/reference/dependencies.md`), each with the compare suites green. (3) The verify loop on the laptop runs once per head sha, runs only the touched suites between reviews, runs the I/O-bound compares in parallel inside one memory cap, and the coppice `internal/server` package stops costing 500 s; every change is measured before and after and the numbers are in this plan's acceptance table. (4) The GitHub workflows run what the box runs, skip docs-only pushes, run on push and by hand (no scheduled run; owner, 2026-10-30, D3), and carry the dependency check.

**Architecture:** One register, `docs/reference/dependencies.toml`, is the source; `scripts/deps_register.py` reads every manifest (`pyproject.toml`, `uv.lock`, three `go.mod`, three `Cargo.toml` and `Cargo.lock`, four `package.json`, `clients/lean/lake-manifest.json`, `clients/go/scripts/native.sh`) and checks them against it, writes `docs/reference/dependencies.md` from it, and keeps `docs/reference/lock-names.txt` (every locked name per lock) so a new transitive package is a visible diff. The speed work is three scripts beside the existing `clients/scripts/build-at.sh`: `verify-at.sh` (one verification per head sha, cached under `/mnt/tera/.tt/verify/<sha>/`), `touched-suites.sh` (changed paths to suite names), and a `--jobs` option lifted out of `clients/coppice_compare.py` into `clients/parallel.py` for the other case-replaying compares. The coppice server package loses its fixed sleeps first; a package split happens only if the measurement after that still says so.

**Tech Stack:** Python 3.12 (stdlib `tomllib`, pytest), bash, Go, Rust (cargo), GitHub Actions. New dependencies: none by default. Two are asked of the owner in `decisions-pending-walkthrough.md` (pytest-xdist, sccache) and each has a task that waits for the answer.

**Spec:** this file; `docs/roadmap.md` sections 8 and 9; `docs/reference/dependencies.md` (the register's prose form today, its "Open" list is the removal queue); rulings DS-1 to DS-8 in `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md`; the ledger `.superpowers/sdd/omarchy/progress.md` 2026-10-21 (the owner's speed ideas, `pcompare`, `build-at.sh`, sccache parked); `docs/how-to/testing.md`; the three workflows under `.github/workflows/`.

**Names.** This plan is written in the names W2 lands: warden (was gate), router (was gateway), routing mode (key `routing_mode`), steward (was foreman), Pando R2 (the chair's memory). Builds start after W2 merges. Post-rename paths are used throughout: `clients/warden_compare.py`, `clients/fixtures/warden/`, `clients/go/internal/warden`, `daisugi-warden`, `router-probe`, `harness/coppice/internal/server/steward_*_test.go`, `daisugi warden audit`.

## Global Constraints

- Box: 4 cores, 15 GB. **One heavy job at a time**, each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G`, with `-j2` (`CARGO_BUILD_JOBS=2`, `GOFLAGS=-p=2`, `go test -p 1`, `DAISUGI_NATIVE_JOBS=2`). A heavy job is: any Go or Rust build, the full pytest suite, any compare suite, any fixture regeneration, any fuzz run, any timing run of Task 1. **Parallel workers in this plan (compare `--jobs`, `t.Parallel`, the two-shell pytest split) run inside that one scope and never beside another heavy job.** Fuzz in chunks of at most 2,000 items. Never rebuild Z3, never `cargo clean` in `clients/rust`, never delete the native prefix.
- `/tmp` is RAM. The worktree is `/mnt/tera/working/programming/od-wt/w3-deps-ci` (branch `w3-deps-ci` from master after W2 merges). Scratch is `/mnt/tera/.tt/w3-deps-ci/` (timing logs, fake HOMEs, two-shell pytest split). `TMPDIR` for Go tests is short and on real disk (`/var/tmp/od`), as `scripts/box-gotest.sh` sets. Delete scratch when a task ends; delete `harness/coppice-rs/target` and `harness/sprig-rs/target` at merge.
- No network and no real model in any test. `HF_HUB_OFFLINE=1`. The register check reads files on disk only; it never queries PyPI, crates.io, the Go proxy or npm. Licence strings come from the register, not from a network lookup.
- Never `pkill -f`. Stop a process by the pid in its lock file or its own stop verb. A timing run that hangs is stopped by `systemctl --user stop <scope>`.
- No AI attribution in commits. Commits state the why and the failure they fix. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path.
- Prose in STE: short sentences, plain words, active voice, no em dashes. Never the words "seam" or "the layer". No changelog voice in docs: write the current state.
- Every coppice run in a hand check passes its own `--socket` and `--data-dir` under scratch. The owner's `~/.opendaisugi`, `~/.local/state/coppice` and `~/.local/bin` are never read, written or moved. Tests never run `daisugi install`.
- **A removal never loosens a verdict.** A task that removes or replaces a package reruns every compare suite that touches the replaced code (named in the task) and the Python suite, and merges only at zero disagreements. A port that cannot do a case refuses it (exit 2, one line, nothing written) and that refusal gets a ruling in `clients/ADJUDICATIONS.md`.
- **Ask the owner before adding a dependency** (AGENTS.md). This plan adds none without the walkthrough answer. A task that waits for an answer says so at its top and is skipped, not improvised, while the answer is open.
- **Measure, then change, then measure again.** Every speed task records wall time with `/usr/bin/time -f '%e s %M KB'` under the same cap, on an idle box (no other agent's heavy job; check with `systemctl --user list-units --type=scope | grep run-`), and writes both numbers into the acceptance table in this file and the ledger. A speed change with no number is not done.

## Review Focus

1. **The register check must fail on nothing found.** A parser that finds zero dependencies in a manifest (a moved file, a renamed section) must fail the check, not pass it. Task 2 pins this with a test that points the script at an empty manifest. Reviewers: delete a `require` block in a copy of `go.mod` and confirm the check fails on the missing file, and also fails when `pyproject.toml` has no `[project]` table.
2. **Banned names are checked where they could come back.** `litellm` and `instructor` are checked in `pyproject.toml` dependency strings, in `uv.lock` `name = ` lines, in every `import` and `from ... import` under `src/`, and in `go.mod`, `Cargo.lock` and `package.json` (a port could grow a wrapper). The test in Task 2 plants each name in a temporary copy of each file kind and sees the check fail.
3. **The lock-names file is a gate, not a log.** A new name in any lock fails CI until `lock-names.txt` is regenerated and committed in the same change. A reviewer confirms the check compares sets both ways: a name that left the lock and stayed in the file also fails (a stale row hides a removal).
4. **Removals keep the oracle the oracle.** DS-4 (own potion loader and WordPiece) must give the same vectors the Go and Rust loaders give, to the tolerance the matcher fixtures already use, over the 380 matcher-query cases. A reviewer runs `clients/pathway_compare.py` for both ports before and after Task 5 and sees the same agreement count.
5. **The mcp SDK replacement (Task 7) re-records 260 sessions unchanged.** The bar from `dependencies.md` "Open" item 1: the K3 fixtures re-record with no difference except `serverInfo.version`, and `clients/go/internal/mcpwire/gen_tools.py --check` passes. A reviewer diffs the two recordings and reads every line that differs.
6. **Parallel workers stay inside one cap.** `clients/parallel.py` computes workers as `min(cpus, MemAvail / per_worker)` from `/proc/meminfo` at start, with the per-worker estimate per suite (Task 12 measures it). A reviewer runs a compare with `--jobs auto` under `MemoryMax=5G` and watches `systemd-cgtop` stay under the cap.
7. **A shorter coppice server suite is not a thinner one.** Task 10 replaces fixed sleeps with condition waits. A reviewer takes ten converted tests and confirms the wait's condition is the thing the sleep waited for, not a proxy. A test that waits on "some time passed" is a sleep with a new name.
8. **Sharing one Rust target dir may start a Z3 rebuild, and says so (owner, 2026-10-30, D5; was a refuse-on-z3-sys guard).** Task 11 extends `build-at.sh` to the Rust daisugi by building inside the main checkout's `clients/rust/target` under its flock. The 2026-10-08 incident (a dirty `z3-sys` fingerprint wiped the built Z3) is the risk, and the owner chose to accept it over a refusal. The task keeps the flock, the one-heavy-job rule, `-j2` and the `MemoryMax=5G` scope, and when `cargo build --release -v --dry-run` names `z3-sys` as a unit to compile, `build-at.sh` prints one warning line to stderr (`build-at: this build compiles z3-sys, a Z3 rebuild of about 29 minutes; continuing under MemoryMax=5G, -j2`) and builds. It never exits 3 for that reason. A reviewer reads that the warning exists and that the memory cap is on the path.

---

## What exists and what is open (measured on master e426041f, 2026-10-30)

### Dependencies, counted

| Ecosystem | Declared direct | Locked names | Source |
|---|---|---|---|
| Python core (`pip install opendaisugi`) | 6: pydantic, z3-solver, pyyaml, typer, click, numpy | 17 (`uv export --frozen --no-dev --no-emit-project`) | `pyproject.toml` |
| Python extras (15 extras) | 36 distinct packages across generate, dev, search, potion, int8, voice, mcp, gateway, shell, tui, robotics, vla, hub, lora, sign | 139 with every extra; 85 for `[dev]`; `[mcp]` alone 41, `[tui]` 34, `[voice]` 39, `[potion]` 36, `[int8]` 33 | `uv.lock` (140 entries, the project included) |
| Go `clients/go` | 2: `github.com/mattn/go-sqlite3`, `golang.org/x/text` | 2 | `clients/go/go.mod`, `go.sum` |
| Go `harness/coppice` | 6 direct + 2 indirect (`golang.org/x/sys`, `rsc.io/qr`); 5 direct + 1 indirect after Task 9b | 10 | `harness/coppice/go.mod`, `go.sum` |
| Go `harness/sprig` | 0 | 0 | `harness/sprig/go.mod` |
| Rust `clients/rust` | 15 `[dependencies]` + 1 build (`cc`, optional) + 1 dev (`rcgen`) | 130 (the crate itself included) | `clients/rust/Cargo.toml`, `Cargo.lock` |
| Rust `harness/coppice-rs` | 9, all exact pins | 51 | `harness/coppice-rs/Cargo.lock` |
| Rust `harness/sprig-rs` | 3, all exact pins | 26 | `harness/sprig-rs/Cargo.lock` |
| JavaScript | `clients/ts`: `web-tree-sitter` 0.25.10 (runtime), `typescript`, `@types/node` (dev); `harness/coppice/e2e`: `playwright` 1.61.0; `examples/integrations/openclaw`: `@modelcontextprotocol/sdk` 1.30.1 (no lock); the two OpenClaw plugin `package.json` files: none | 4 and 3 | the `package.json` and `package-lock.json` files |
| Lean 4 | 0 packages; toolchain `leanprover/lean4:v4.33.0` | 0 | `clients/lean/lake-manifest.json`, `lean-toolchain` |
| Native prefix and build tools | Z3 5.1.0, tree-sitter, tree-sitter-bash, zig 0.16.0 (libc++, libc++abi, libunwind), libghostty-vt (simdutf, Highway, Wuffs, uucode), SQLite (bundled by go-sqlite3 and rusqlite), ring's C and assembly; optional: MuJoCo 3.12.0, ONNX Runtime 1.23.2, the ggml tree for voice; tools: cmake 4.4.3 | 12 libraries (roadmap count) | `clients/go/scripts/native.sh`, `harness/coppice/scripts/toolchain.sh`, the four `NOTICE` files |
| System tools the scripts expect | git, gcc, make, pkgconf, curl, python3, binutils, go, uv; cargo and rustc for `--rust`; ffmpeg (voice decode); tmux (the coppice tmux-mirror compare cases); node, npm and a Playwright chromium (e2e); arecord (voice tests, faked) | | `scripts/install.sh`, `scripts/preflight.sh`, `docs/how-to/testing.md`, `.github/workflows/clients.yml` |

`litellm` and `instructor`: 0 hits in `uv.lock` and `pyproject.toml`. The only test that guards this today is `tests/test_llm_client_offline_import.py`, which checks `sys.modules` after importing the model callers; nothing checks the manifests.

### Removal and replacement candidates

Each row says what the dependency costs, and the ruling this plan applies. Rows marked **owner** wait for the walkthrough.

| Dependency | Where | Cost | Ruling in this plan |
|---|---|---|---|
| `mcp` SDK | `[mcp]`, the Python MCP server (the oracle of the Go and Rust servers, K3-1) | 24 packages over core (41 vs 17); `<2` bound; the one extra every memory-over-MCP user installs | **Replace** with an own stdio JSON-RPC server ported from `clients/go/internal/mcpwire` (DS-6). Bar: the 260 K3 sessions re-record unchanged. Task 7. Largest task; may go to its own branch. |
| `textual-serve` | `[tui]`, `daisugi dashboard --serve` (`tui.py`, `cli.py`, `tui_web/app_index.html`) | A second browser surface beside coppice web; its own HTTP server | **Remove** (DS-6, in force, bulk-confirmed 2026-09-28): `--serve` goes; coppice web is the browser surface; `--tui` stays. Task 6. **owner** sees DS-6 in the walkthrough first. |
| `model2vec`, `huggingface_hub` | `[potion]` | 19 packages over core; `snapshot_download` is a network path inside a local-first tool | **Replace** with an own loader (port from `clients/go/internal/embed`) and `_model_fetch.py`'s sha256-pinned fetch (DS-4). Task 5. |
| `tokenizers` | `[int8]` | A Rust wheel for one WordPiece vocabulary read | **Replace** with a small WordPiece of our own (DS-4). Task 5. |
| `sentence-transformers` | `[search]` | torch and the CUDA wheels, about 5.3 GB; never in dev or CI | **Keep as opt-in**; drop when the int8 matcher covers every `[search]` test path. Task 5 lists the remaining paths; removal is a follow-on, not this wave. |
| `pytest-cov` | `[dev]` | 2 packages; no workflow, script or how-to passes `--cov` | **Remove** from `[dev]`; `uvx pytest-cov` when someone wants a coverage run. Task 4. |
| `build` | `[dev]` | 1 package plus deps; no script calls it; `uv build` does the same | **Remove** from `[dev]`; the release how-to says `uv build`. Task 4. |
| `click` | core | 1 package for two `click.Choice` uses in `cli.py` | **Replace** with a `str` `Enum` in the two options (typer renders an Enum as a choice). Task 4. If typer's help output changes a CLI golden case, keep click and record why. |
| `networkx`, `av`, `twine`, `litellm`, `instructor` | | | **Removed already** (2026-10-09 and before). Register rows keep status `removed`; `litellm` and `instructor` are `banned`. |
| `qrterminal`, `rsc.io/qr` | coppice sign-in QR | 2 modules, MIT and BSD-3, for one feature | **Remove (owner, 2026-10-30, D4; was Keep).** Write our own QR encoder with Reed-Solomon in coppice (`internal/qr`), about 500 lines, from ISO/IEC 18004. Task 9b. |
| `thiserror` | `clients/rust` | A second `syn` in the build; 0 source files use it today (`grep -rl thiserror clients/rust/src` is empty) | **Remove** from `Cargo.toml`. Task 9. |
| tree-sitter 0.25 (Rust, TS) vs 0.26 (Python, Go); SQLite 3.45.0 in rusqlite 0.31 vs newer in go-sqlite3 1.14.52 | version skew | Two grammars in flight; two SQLite versions reading one store | **Align** (DS-8): Rust and TS to tree-sitter 0.26 and the matching tree-sitter-bash; rusqlite to the release that bundles the SQLite go-sqlite3 bundles. Task 8. |
| `GOTOOLCHAIN=auto` | `scripts/install.sh`, both `preflight.sh`, `toolchain.sh` | A global Go setting leaking out of a build script | **Verify** DS-7 is done (toolchain.sh sets no global setting since 2026-10-09) and retire the row. Task 8. |
| `playwright`, `typescript`, `@types/node`, `web-tree-sitter`, `@modelcontextprotocol/sdk` (example) | JS | Test and example only; the shipped plugins carry none | **Keep.** The example's sdk is pinned; add a `package-lock.json` there so its lock is checked too. Task 2. |
| MuJoCo, ONNX Runtime | optional native | Never linked into shipped binaries | **Keep optional.** |

### Measured suite times on one small laptop (sources named; blanks are Task 1's job)

| Suite | Command | Measured | Source |
|---|---|---|---|
| Python, full | `uv run --no-sync pytest -q` | 6,801 tests collected today; older logs: 2,673 tests in 386 s to 461 s (0.14 to 0.17 s per test). Extrapolated: 16 to 20 min. **Task 1 measures.** | `.pytest_cache` node ids; `~/opendaisugi-scratch` logs |
| coppice `internal/server` | `go test -p 1 ./internal/server -count=1` | 305 s idle (2026-10-22); 653 s and 753 s for the steward subset alone (2026-10-15, 2026-10-14); 2,092 s under load (2026-10-21); the owner's own figure: 500 s. 863 test functions, 0 `t.Parallel()`, 133 `time.Sleep` calls in 87 test files. | omarchy ledger |
| coppice, all packages | `go test -p 1 ./... -count=1` (26 packages) | **Task 1 measures.** | |
| coppice compare, full | `clients/coppice_compare.py` over 2,082 cases | serial 17 min 26 s; `--jobs auto` 8 min 58 s (2026-10-21) | omarchy ledger |
| `clients/go` tests | `go test -p 1 ./... -count=1` | **Task 1 measures.** | |
| `clients/rust` tests | `cargo test --release --locked` | about 500 tests (count only) | omarchy ledger |
| `coppice-rs`, `sprig-rs` tests | `cargo test --release` | 594 to 624 tests; 63 tests (counts only) | omarchy ledger |
| warden compare | `clients/warden_compare.py --binary ... --oracle` | 1,310 cases; a cold gate call is 8.8 ms Go, 6.1 ms Rust, about 430 ms Python | AGENTS.md |
| Rust release build with Z3 from cold | release tarball | 29 min | AGENTS.md |
| Rust coppice rebuild at a new sha, deps reused | `build-at.sh rust` | 38.9 s | omarchy ledger |
| Full merge verify (today's practice) | build Go + Rust, Go server package, full compare, e2e web | hours per fixer round; each fixer repeated it | omarchy ledger 2026-10-21 |

### CI today versus what it should run

| Workflow | Runs today | Gap |
|---|---|---|
| `ci.yml` (push to master, PRs) | `uv sync --locked --extra dev`; ruff check and format; `pytest -q` with `HF_HUB_OFFLINE=1`; `tests/test_adversarial.py` again; `daisugi warden audit` | No dependency check. No `workflow_dispatch`. Runs the Python suite serially. |
| `coppice.yml` (path-filtered to `harness/coppice/**`, `harness/sprig/**`) | libghostty-vt cache keyed on commit, zig and script sha; vet, gofmt, `go test -count=1`, the same with `-race`, build; sprig: vet, gofmt, race tests, build | The filter misses `clients/coppice_compare.py` and the coppice fixtures (they run in `clients.yml`, so nothing is lost, but a coppice-only change runs there too). Fine as is. |
| `clients.yml` (every push, every PR, no path filter) | `native` (prefix, 120 min cap), `generators`, `go`, `rust` (90 min), `compare` matrix 2 languages x 17 suites, `speed`, `robotics` (installs MuJoCo and ORT each run, 90 min), `coppice-rs`, `sprig-rs`, `e2e` (release tarball, smoke, Playwright) | A docs-only push runs every job. No `workflow_dispatch`. No timer: the public repo gets one push a month (the fold), so a broken runner image or an expired action is found on fold day. `clients/go` has no `-race` run. |

The fact that shapes the CI work: the public history is one commit a month and nothing else is pushed (AGENTS.md "Commits"). CI proves the fold, not the day's work. The day's work is proved on the box. So the box loop must be fast and the CI must be complete and known-green before fold day, not fast.

## Rulings DC-1 to DC-16 (this plan; provisional until the owner reads them)

- **DC-1, one register.** `docs/reference/dependencies.toml` is the source of truth. `docs/reference/dependencies.md` is written from it by `scripts/deps_register.py --write` and CI checks it is current (`--check`). A row is `[[dep]]` with `name`, `ecosystem` (`python`, `go`, `rust`, `js`, `lean`, `native`, `tool`, `model`), `where` (the manifest or extra), `licence` (an SPDX expression), `used_by`, `why`, `status` (`keep`, `optional`, `open`, `build-tool`, `removed`, `banned`), and for `open` a `plan` line. Hand-written prose that the page needs (the "Licences to know" section) lives in `docs/reference/dependencies.toml` under `[prose]` as Markdown strings, so one file holds everything.
- **DC-2, every direct dependency has a row; every row that is not `removed` or `banned` is declared somewhere.** The check reads: `pyproject.toml` `dependencies` and every `optional-dependencies` list; each `go.mod` `require` (direct and `// indirect`); each `Cargo.toml` `[dependencies]`, `[build-dependencies]`, `[dev-dependencies]`; each `package.json` `dependencies` and `devDependencies`; `lake-manifest.json` `packages`; the `*_VERSION=` and `*_SHA256=` pairs in `native.sh` mapped by a small table in the script to library names. A manifest the script cannot find or parse, or one that yields zero names where the table says at least one is expected, fails the check.
- **DC-3, locks are gated by name.** `docs/reference/lock-names.txt` holds, per lock, the sorted set of locked names (`uv.lock`, two `go.sum`, three `Cargo.lock`, two `package-lock.json`). The check fails when any lock's set differs from the file in either direction. `--write` regenerates it. The commit that regenerates it names the row (or the bump) that caused the change.
- **DC-4, banned names.** `litellm` and `instructor` are `status = "banned"` rows. The check fails when either name appears as a dependency string in `pyproject.toml`, a `name = ` line in `uv.lock`, a module in any `import` or `from` statement under `src/opendaisugi/`, a `require` in a `go.mod`, a `name = ` in a `Cargo.lock`, or a key in a `package.json` dependency map. The test plants each and sees the failure.
- **DC-5, licence allowlist.** `[licences] allowed = [...]` in the register lists the SPDX expressions in use today (MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, MPL-2.0, Unicode-3.0, 0BSD, Zlib, CC0-1.0, CC-BY-4.0, "Apache-2.0 WITH LLVM-exception", "Apache-2.0 AND ISC", the OR duals, "public domain", and the two named model licences). A row whose `licence` is not in the list fails the check unless it carries `licence_note` and the commit says the owner saw it. The check never fetches a licence; it checks the string.
- **DC-6, the register check lives in `ci.yml`** as one step after the lint, and in `tests/test_dependency_register.py` so `pytest` on the box runs it too. It needs no native prefix and takes under two seconds.
- **DC-7, removals in `[dev]`.** `pytest-cov` and `build` leave `[dev]`. `uvx pytest-cov` and `uv build` are the one-off commands; `docs/how-to/testing.md` and the release notes in `scripts/release.sh`'s header say so.
- **DC-8, `click`.** The two `click.Choice` options become `Enum` types. If `clients/cli_compare.py` shows a help-text difference against the committed CLI fixtures that cannot be matched by the Go and Rust help renderers, keep `click` and set its row to `keep` with that reason. No fixture is regenerated to hide a help change.
- **DC-9, DS-4 tolerance.** The own potion loader must reproduce the vectors of `model2vec` to the tolerance the Go loader already meets against Python (the one `clients/pathway_cases.py` records in the matcher-query cases). The own WordPiece must tokenize the int8 fixture sentences identically to `tokenizers` (a byte-equal token id list, pinned in a new test over 200 sentences drawn from the existing fixtures).
- **DC-10, the mcp SDK bar.** The own server re-records the 260 K3 sessions with no change except `serverInfo.version`; `gen_tools.py --check` passes; the sessions that the ports refuse (`resources/read`, `completion/complete`, `tasks/*`, `resources/subscribe`) get the same answers the SDK gave. Any other difference is a bug in the own server, never a fixture regeneration.
- **DC-11, one verification per head sha.** `clients/scripts/verify-at.sh WORKTREE [--touched | --full]` runs the named suites and writes `/mnt/tera/.tt/verify/<sha>-<toolkey>/<suite>.{ok,fail,log}`; a second call for a clean tree at the same sha and toolkey prints the cached result and runs nothing. A dirty tree is verified every time and never cached. The toolkey is `build-at.sh`'s. `--touched` runs the suites `touched-suites.sh BASE..HEAD` names; `--full` runs every suite; a merge needs one `--full` result at the merged head.
- **DC-12, the touched map is a table in one file.** `clients/scripts/touched-suites.sh` holds a path-prefix to suite-name table (for example `src/opendaisugi/warden*` to `python warden-compare warden-server-compare cli-compare`, `harness/coppice/internal/server/` to `coppice-server coppice-compare`, `clients/fixtures/<s>/` to `<s>-compare`). A changed path that matches no prefix selects `--full`. The table is tested: a test lists every top-level source directory and asserts each has at least one prefix.
- **DC-13, parallel compares.** `clients/parallel.py` carries `auto_jobs(cpus, avail_kib, per_worker_kib)` (lifted from `coppice_compare.py`, which imports it) and `run_cases(cases, worker, jobs)`. Each worker gets its own scratch directory and port range. The compare scripts whose cases are process-per-case and sleep-bound take `--jobs N|auto`: `warden_server_compare.py`, `k2_compare.py`, `weave_compare.py`, `sprig_compare.py`. The CPU-bound ones (`warden_compare.py`, `cli_compare.py`) are measured in Task 12 and take `--jobs` only if the measurement shows a gain of 30 percent or more. A case that fails under `--jobs > 1` reruns alone once before it counts (the rule `coppice_compare.py` already has).
- **DC-14, the server package, in this order.** (1) Every fixed `time.Sleep` in `harness/coppice/internal/server/*_test.go` becomes a condition wait with a deadline, or keeps the sleep with a comment that says what it models (PK-8's rule). (2) Tests that create their own socket and data dir and read no package-level variable get `t.Parallel()`; `go test -parallel 4` inside one `go test` process stays inside one cap. (3) The package splits into `server` and `server/stewardtest` (the 15 `steward_*_test.go` files) only if, after (1) and (2), the package still takes over 200 s idle and the steward files hold over half of it. The split moves tests to an external test package (`package server_test`) and exports nothing new; a test that needs an unexported symbol stays in `server`.
- **DC-15, the Rust daisugi binary cache shares the checkout's target.** `build-at.sh daisugi-rust WORKTREE` builds with `CARGO_TARGET_DIR=<main checkout>/clients/rust/target` under a flock on that directory, keeps only `daisugi`, `daisugi-warden` and the probes per sha, and, when a dry run names `z3-sys` as a compile unit, prints a warning that a Z3 rebuild started and goes on under the cap (the Z3 warning, Review Focus 8; owner ruling D5, no refusal). `build-at.sh daisugi-go` and `sprig-go` use the content-keyed Go cache and a per-sha copy of the binaries. `build-at.sh sprig-rust` shares `harness/sprig-rs`'s target the same way.
- **DC-16, CI shape.** `clients.yml` gets `paths-ignore: ["docs/**", "**/*.md", "site/**"]` on push and pull_request, plus `workflow_dispatch` (run by hand from the Actions tab). **No `schedule`: the owner ruled on 2026-10-30 (D3) that no workflow runs on a weekly timer.** `ci.yml` and `coppice.yml` get `workflow_dispatch`. `ci.yml` gets the register step. `clients.yml`'s `go` job runs `go test -race` on the packages that have goroutines (`internal/warden`, `internal/router`, `internal/mcpwire`), not the whole tree (the Z3 cgo tests under race are slow). Nothing else in the workflows changes in this plan; the rename's workflow edits are W2's.

## Task ranking by risk

1. Tasks 2 and 3: the register, the banned-name check and the CI step. Cheap, and they stop a regression (litellm, an unlicensed package) that is otherwise found on fold day.
2. Task 1: the baseline numbers. Without them every speed task is a claim.
3. Tasks 10 to 12: the box loop. This is where the owner's time goes every day.
4. Tasks 4, 5, 8, 9: small removals and alignments, each behind the compares.
5. Task 7: the mcp SDK replacement. Largest and most careful; last among the removals, on its own branch if the wave runs long.
6. Tasks 6, 13, 14: owner-gated (DS-6 confirmation, pytest-xdist, sccache). Skipped while the answer is open.
7. Task 15: docs, roadmap, DECISIONS-PENDING retirements, ledger.

---

### Task 1: Worktree, scratch, and the baseline measurements

**Files:**
- Create: `/mnt/tera/.tt/w3-deps-ci/times/` (scratch, not committed)
- Modify: this file (the "Measured suite times" table and the acceptance table), `.superpowers/sdd/backlog/progress.md`

**Interfaces:** none. This task runs the suites as they are and records numbers.

- [ ] **Step 1: Worktree and scratch**

Run:
```bash
git -C /mnt/tera/working/programming/openDaisugi worktree add /mnt/tera/working/programming/od-wt/w3-deps-ci -b w3-deps-ci master
mkdir -p /mnt/tera/.tt/w3-deps-ci/times /var/tmp/od
df -h /mnt/tera | tail -1
```
Expected: the worktree exists; `/mnt/tera` has at least 15 GB free (the ledger rule: `df` before each dispatch).

- [ ] **Step 2: Confirm the box is idle**

Run: `systemctl --user list-units --type=scope | grep -c run-` and `uptime`.
Expected: `0` scopes, load under 1.0. If not, wait; a timing on a loaded box is not a baseline.

- [ ] **Step 3: Python, full, serial, with the slowest 40 tests**

Run (one heavy job):
```bash
cd /mnt/tera/working/programming/od-wt/w3-deps-ci && uv sync --locked --extra dev
HF_HUB_OFFLINE=1 systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait \
  /usr/bin/time -f '%e s %M KB' -o /mnt/tera/.tt/w3-deps-ci/times/pytest.time \
  uv run --no-sync pytest -q --durations=40 -p no:cacheprovider 2>&1 | tee /mnt/tera/.tt/w3-deps-ci/times/pytest.log | tail -60
```
Record: wall seconds, peak KB, the `passed/skipped` line, and the 40 slowest tests with their seconds.

- [ ] **Step 4: Python, two-shell split (the pytest-xdist question, measured with no new dependency)**

Split `tests/` into two halves by file count (`ls tests/*.py | sort | awk 'NR%2'` and `NR%2==0`, plus the subdirectories `bench floor voice harness_pi harness_opencode` all in the second half). Run both halves at once inside one scope:
```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait bash -c '
  HF_HUB_OFFLINE=1 uv run --no-sync pytest -q -p no:cacheprovider $(cat /mnt/tera/.tt/w3-deps-ci/times/half1.txt) > /mnt/tera/.tt/w3-deps-ci/times/half1.log 2>&1 &
  HF_HUB_OFFLINE=1 uv run --no-sync pytest -q -p no:cacheprovider $(cat /mnt/tera/.tt/w3-deps-ci/times/half2.txt) > /mnt/tera/.tt/w3-deps-ci/times/half2.log 2>&1 &
  wait' ; tail -1 /mnt/tera/.tt/w3-deps-ci/times/half*.log
```
Record: wall seconds of the longer half, and any test that fails in the split and passes serially (a shared-state test; list it for Task 13).

- [ ] **Step 5: Go, per package, coppice and clients/go**

Run (each a heavy job, one after the other; `TMPDIR` short and on disk):
```bash
cd /mnt/tera/working/programming/od-wt/w3-deps-ci/harness/coppice
TMPDIR=/var/tmp/od COPPICE_REQUIRE_TOOLCHAIN=1 systemd-run --user --scope -q -p MemoryMax=5G --wait \
  go test -p 1 ./... -count=1 -json 2>/dev/null > /mnt/tera/.tt/w3-deps-ci/times/coppice.json
python3 - <<'EOF'
import json,collections
pk=collections.defaultdict(float); tests=[]
for ln in open('/mnt/tera/.tt/w3-deps-ci/times/coppice.json'):
    e=json.loads(ln)
    if e.get('Action')=='pass' and 'Test' not in e: pk[e['Package']]=e['Elapsed']
    if e.get('Action')=='pass' and 'Test' in e and '/' not in e['Test']: tests.append((e['Elapsed'],e['Package'].split('/')[-1],e['Test']))
for p,s in sorted(pk.items(),key=lambda x:-x[1]): print(f"{s:8.1f}s {p}")
print(); [print(f"{s:7.1f}s {p} {t}") for s,p,t in sorted(tests,reverse=True)[:40]]
EOF
```
Then the same for `clients/go` with `CGO_CFLAGS="-O2 -g -DOPENDAISUGI_NATIVE=$(scripts/native.sh --print-key)"`.
Record: seconds per package (all 26 coppice packages and every `clients/go` package), the 40 slowest tests in `internal/server`, and the sum of `time.Sleep` arguments in `internal/server/*_test.go` (`grep -ho 'time.Sleep([^)]*)' *_test.go | sort | uniq -c`).

- [ ] **Step 6: Rust test suites (counts and wall)**

Run, one at a time, each under the cap with `CARGO_BUILD_JOBS=2`, after `. clients/rust/tools/z3-build-env.sh`: `cargo test --release --locked` in `clients/rust`, `harness/coppice-rs`, `harness/sprig-rs`, each wrapped in `/usr/bin/time -f '%e s %M KB'`. The first run includes a build; run each twice and record the second (tests only) and the difference (the build).

- [ ] **Step 7: Compares, serial, per suite**

Run each compare in `docs/how-to/testing.md` for the Go binary, serially, each under the cap with `/usr/bin/time`, with its own scratch under `/mnt/tera/.tt/w3-deps-ci/runs/`. Include `warden_server_compare.py`, `k2_compare.py`, `weave_compare.py`, `sprig_compare.py` and `coppice_compare.py` (serial and `--jobs auto`). Record seconds per suite and per case (seconds divided by case count).

- [ ] **Step 8: Write the numbers**

Fill the "Measured suite times" table in this file and the "Before" column of the acceptance table at the end. Append one ledger line: `W3 deps-ci baseline: pytest <s> (<n> tests), split-2 <s>, coppice server <s>, coppice all <s>, clients/go <s>, rust <s>/<s>/<s>, compares <list>, coppice_compare serial <s> auto <s>`.

Commit (paths: this file and the ledger are outside `docs/superpowers`, so the plan file commits; the ledger does not, it is git-ignored):
```bash
git add docs/plans/2026-10-30-backlog/w3-deps-and-ci.md
git commit -m "plan: the measured baseline for the dependency and test-speed wave

Every speed task in this plan is judged against these numbers, taken
on an idle box under the memory cap. Without them a faster suite is a
claim, not a result."
```

---

### Task 2: The dependency register, its check, the lock-names gate and the banned-name test

**Files:**
- Create: `docs/reference/dependencies.toml`, `docs/reference/lock-names.txt`, `scripts/deps_register.py`, `tests/test_dependency_register.py`, `examples/integrations/openclaw/package-lock.json`
- Modify: `docs/reference/dependencies.md` (becomes generated; header line says so), `AGENTS.md` (the "Lockfiles and pins" paragraph names the register and the command)

**Interfaces:**
- `scripts/deps_register.py --check [--root DIR]`: exit 0 when every manifest agrees with the register, the lock sets equal `lock-names.txt`, no banned name appears, every licence is allowed, and `dependencies.md` equals what `--write` would produce. Otherwise exit 1 and one line per finding: `pyproject.toml [mcp]: 'mcp' has no row`, `uv.lock: new name 'foo' (not in lock-names.txt)`, `banned: 'litellm' in src/opendaisugi/x.py:12`, `register: row 'twine' status keep but declared nowhere`, `licence: 'GPL-3.0-only' for 'x' is not in [licences].allowed`.
- `scripts/deps_register.py --write [--root DIR]`: rewrites `dependencies.md` and `lock-names.txt`; prints what changed.
- `scripts/deps_register.py --imports [--root DIR]`: lists every top-level module imported under `src/opendaisugi/` that is not stdlib, not `opendaisugi`, and not mapped to a declared package by the `[imports] map` table in the register (DS-5's remaining half); nonzero when any is found.
- Register shape: see DC-1. `[locks]` lists the lock files; `[manifests]` lists manifest paths with `expect_min` (the smallest number of names the parser must find, 0 for `harness/sprig/go.mod` and the Lean manifest); `[banned] names = ["litellm", "instructor"]`; `[licences] allowed = [...]`; `[imports] map = { yaml = "pyyaml", z3 = "z3-solver", ... }`.
- The script uses only the stdlib (`tomllib`, `json`, `re`, `ast`, `pathlib`, `sys`). No PEP 723 block (it lives inside the project; the convention in the workshop map says so). Run with `uv run --no-sync python scripts/deps_register.py`.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_dependency_register.py
import shutil, subprocess, sys
from pathlib import Path
import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "deps_register.py"


def run(*args, root=ROOT):
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args, "--root", str(root)], capture_output=True, text=True
    )


def test_the_checked_in_tree_passes():
    r = run("--check")
    assert r.returncode == 0, r.stdout + r.stderr


def _copy_tree(tmp_path):
    for rel in [
        "pyproject.toml",
        "uv.lock",
        "clients/go/go.mod",
        "clients/go/go.sum",
        "harness/coppice/go.mod",
        "harness/coppice/go.sum",
        "harness/sprig/go.mod",
        "clients/rust/Cargo.toml",
        "clients/rust/Cargo.lock",
        "harness/coppice-rs/Cargo.toml",
        "harness/coppice-rs/Cargo.lock",
        "harness/sprig-rs/Cargo.toml",
        "harness/sprig-rs/Cargo.lock",
        "clients/ts/package.json",
        "clients/ts/package-lock.json",
        "harness/coppice/e2e/package.json",
        "harness/coppice/e2e/package-lock.json",
        "examples/integrations/openclaw/package.json",
        "examples/integrations/openclaw/package-lock.json",
        "src/opendaisugi/install_assets/openclaw_plugin/package.json",
        "clients/go/internal/install/assets/openclaw_plugin/package.json",
        "clients/lean/lake-manifest.json",
        "clients/go/scripts/native.sh",
        "docs/reference/dependencies.toml",
        "docs/reference/dependencies.md",
        "docs/reference/lock-names.txt",
    ]:
        dst = tmp_path / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(ROOT / rel, dst)
    (tmp_path / "src/opendaisugi").mkdir(parents=True, exist_ok=True)
    return tmp_path


@pytest.mark.parametrize(
    "rel,text",
    [
        ("pyproject.toml", '\n[project.optional-dependencies]\nx = ["litellm>=1"]\n'),
        ("uv.lock", '\n[[package]]\nname = "instructor"\nversion = "1.0.0"\n'),
        ("src/opendaisugi/_planted.py", "import litellm\n"),
        ("src/opendaisugi/_planted2.py", "from instructor import patch\n"),
        ("clients/go/go.mod", "\nrequire github.com/x/litellm v0.0.1\n"),
        ("clients/rust/Cargo.lock", '\n[[package]]\nname = "instructor"\nversion = "0.1.0"\n'),
        ("clients/ts/package.json", None),  # the test rewrites dependencies to add "litellm"
    ],
)
def test_a_banned_name_fails_the_check(tmp_path, rel, text):
    root = _copy_tree(tmp_path)
    p = root / rel
    if text is None:
        import json

        d = json.loads(p.read_text())
        d["dependencies"]["litellm"] = "1.0.0"
        p.write_text(json.dumps(d))
    else:
        p.write_text((p.read_text() if p.exists() else "") + text)
    r = run("--check", root=root)
    assert r.returncode == 1 and "banned" in r.stdout, r.stdout + r.stderr


def test_a_new_direct_dependency_without_a_row_fails(tmp_path):
    root = _copy_tree(tmp_path)
    p = root / "pyproject.toml"
    p.write_text(
        p.read_text().replace('"numpy>=1.26",\n]', '"numpy>=1.26",\n    "attrs>=23",\n]', 1)
    )
    r = run("--check", root=root)
    assert r.returncode == 1 and "'attrs' has no row" in r.stdout


def test_a_new_locked_name_fails_until_written(tmp_path):
    root = _copy_tree(tmp_path)
    p = root / "harness/sprig-rs/Cargo.lock"
    p.write_text(p.read_text() + '\n[[package]]\nname = "leftpad"\nversion = "0.1.0"\n')
    assert run("--check", root=root).returncode == 1
    assert run("--write", root=root).returncode == 0
    assert run("--check", root=root).returncode == 0
    assert "leftpad" in (root / "docs/reference/lock-names.txt").read_text()


def test_a_name_that_left_a_lock_fails_too(tmp_path):
    root = _copy_tree(tmp_path)
    p = root / "docs/reference/lock-names.txt"
    p.write_text(p.read_text() + "clients/rust/Cargo.lock ghostcrate\n")
    r = run("--check", root=root)
    assert r.returncode == 1 and "ghostcrate" in r.stdout


def test_an_empty_or_missing_manifest_fails(tmp_path):
    root = _copy_tree(tmp_path)
    (root / "clients/go/go.mod").write_text("module daisugi-verify\n\ngo 1.25.12\n")
    r = run("--check", root=root)
    assert r.returncode == 1 and "clients/go/go.mod" in r.stdout and "expected at least" in r.stdout
    (root / "harness/coppice/go.mod").unlink()
    r = run("--check", root=root)
    assert r.returncode == 1 and "harness/coppice/go.mod" in r.stdout and "missing" in r.stdout


def test_a_row_declared_nowhere_fails(tmp_path):
    root = _copy_tree(tmp_path)
    p = root / "docs/reference/dependencies.toml"
    p.write_text(
        p.read_text()
        + '\n[[dep]]\nname = "ghostpkg"\necosystem = "python"\nwhere = "core"\nlicence = "MIT"\nused_by = "x"\nwhy = "x"\nstatus = "keep"\n'
    )
    r = run("--check", root=root)
    assert r.returncode == 1 and "ghostpkg" in r.stdout and "declared nowhere" in r.stdout


def test_a_licence_outside_the_allowlist_fails(tmp_path):
    root = _copy_tree(tmp_path)
    p = root / "docs/reference/dependencies.toml"
    p.write_text(
        p.read_text()
        .replace('name = "pydantic"\n', 'name = "pydantic"\nlicence = "GPL-3.0-only"\n', 1)
        .replace('licence = "MIT"\nlicence = "GPL-3.0-only"', 'licence = "GPL-3.0-only"', 1)
    )
    r = run("--check", root=root)
    assert r.returncode == 1 and "GPL-3.0-only" in r.stdout


def test_the_markdown_page_is_current():
    r = run("--check")
    assert r.returncode == 0
    before = (ROOT / "docs/reference/dependencies.md").read_text()
    assert run("--write").returncode == 0
    assert (ROOT / "docs/reference/dependencies.md").read_text() == before


def test_no_undeclared_import_in_src():
    r = run("--imports")
    assert r.returncode == 0, r.stdout
```

Run: `uv run --no-sync pytest -q tests/test_dependency_register.py`
Expected: FAIL (no script, no register).

- [ ] **Step 2: Write the register from the current reference page**

Every row of `docs/reference/dependencies.md` becomes a `[[dep]]`. Add rows the page lacks today: `click` is there; add `pytest-asyncio`, `rcgen` (rust dev), `cc` (rust build, optional), `rustls-pki-types`, `num-bigint`, `num-traits`, `miniz_oxide`, `libc`, `jiter`, `z3-src` (through z3-sys), `@types/node`, `typescript`, the `leanprover/lean4` toolchain as `tool`, `uv`, `zig`, `cmake`, `ggml` (voice tree), the models. Statuses as the page says; `pytest-cov` and `build` get `status = "open"` with `plan = "Task 4 removes them from [dev]"` until Task 4 lands, then `removed`. `litellm`, `instructor`: `banned`. `qrterminal`, `rsc.io/qr`: `keep` (DC "qr" ruling, Task 9 edits the reason).

- [ ] **Step 3: Write the script**

Parsers: `pyproject.toml` with `tomllib` (names by PEP 508 split on `[<>=!~; ]`, the `opendaisugi[...]` self-references skipped); `uv.lock` `tomllib` `package[].name`; `go.mod` by a regex over `require` blocks and single-line requires; `go.sum` first field, deduplicated, `/go.mod` suffix stripped; `Cargo.toml` `tomllib` on the three tables, names from keys; `Cargo.lock` `tomllib` `package[].name`; `package.json` and `package-lock.json` with `json` (`packages` keys minus the root, `node_modules/` prefix stripped); `lake-manifest.json` `packages[].name`; `native.sh` by the regex `^([A-Z0-9_]+)_(VERSION|SHA256)=` mapped by a table (`Z3` to `z3`, `TS` to `tree-sitter`, `TSB` to `tree-sitter-bash`, `ZIG` to `zig`, `ORT` to `onnxruntime`, `MUJOCO` to `mujoco`, `GGML_TREE` to `ggml`). Also parse `harness/coppice/scripts/toolchain.sh` for `GHOSTTY_COMMIT` (`libghostty-vt`) and `CMAKE_VERSION` if present.
Markdown writer: the same tables as today's page, grouped by `ecosystem` and `where`, then the `[prose]` sections, with the header `<!-- generated by scripts/deps_register.py --write from dependencies.toml; edit the toml -->`.
Imports: `ast.walk` over every `.py` under `src/opendaisugi/`, top-level names of `Import` and `ImportFrom` (level 0), minus `sys.stdlib_module_names`, minus `opendaisugi`, mapped through `[imports] map`, must be a declared Python package.

Run: `uv run --no-sync python scripts/deps_register.py --write && uv run --no-sync pytest -q tests/test_dependency_register.py`
Expected: PASS. If `--imports` names a module, that module is a real DS-5 finding: add the package to the right extra with a comment, run `uv lock`, and add its row. Record each in the commit message.

- [ ] **Step 4: The openclaw example lock**

Run: `cd examples/integrations/openclaw && npm install --package-lock-only --ignore-scripts` (this one command needs the network; run it once on the box, not in a test). Commit the lock. Add it to `[locks]`.

- [ ] **Step 5: Ruff, format, the whole suite's import check**

Run: `uv run --no-sync ruff check . && uv run --no-sync ruff format --check .`
Expected: clean.

Commit:
```bash
git add scripts/deps_register.py tests/test_dependency_register.py docs/reference/dependencies.toml docs/reference/dependencies.md docs/reference/lock-names.txt examples/integrations/openclaw/package-lock.json AGENTS.md
git commit -m "deps: one register for every dependency, checked against every manifest and lock

A new package with no row, a lock that grew with no committed name,
a licence outside the allowlist, or litellm or instructor anywhere
now fails the check. Before this, dependencies.md was prose nothing
read back, and the only litellm guard was a sys.modules test."
```

---

### Task 3: The register in CI, the workflow triggers, and the docs-only filter

**Files:**
- Modify: `.github/workflows/ci.yml`, `.github/workflows/clients.yml`, `.github/workflows/coppice.yml`, `docs/how-to/testing.md`

**Interfaces:** DC-16.

- [ ] **Step 1: Write the test for the workflow shape**

```python
# tests/test_workflows_shape.py
import yaml
from pathlib import Path

WF = Path(__file__).resolve().parents[1] / ".github" / "workflows"


def _load(name):
    return yaml.safe_load((WF / name).read_text())


def test_every_workflow_can_be_run_by_hand_and_none_has_a_timer():
    for name in ("ci.yml", "clients.yml", "coppice.yml"):
        on = _load(name)[True] if True in _load(name) else _load(name)["on"]
        assert "workflow_dispatch" in on, name
        assert "schedule" not in on, name  # owner ruling D3: runs on push and by hand only


def test_clients_skips_docs_only_pushes():
    on = _load("clients.yml")
    on = on[True] if True in on else on["on"]
    for ev in ("push", "pull_request"):
        assert on[ev]["paths-ignore"] == ["docs/**", "**/*.md", "site/**"], ev


def test_ci_runs_the_dependency_register():
    steps = _load("ci.yml")["jobs"]["lint-and-test"]["steps"]
    cmds = [s.get("run", "") for s in steps]
    assert any("scripts/deps_register.py --check" in c for c in cmds)
    assert any("scripts/deps_register.py --imports" in c for c in cmds)


def test_clients_go_job_runs_the_race_detector_on_the_concurrent_packages():
    steps = _load("clients.yml")["jobs"]["go"]["steps"]
    cmds = " ".join(s.get("run", "") for s in steps)
    assert (
        "go test -race" in cmds
        and "./internal/warden/..." in cmds
        and "./internal/router/..." in cmds
        and "./internal/mcpwire/..." in cmds
    )
```
Run: `uv run --no-sync pytest -q tests/test_workflows_shape.py`. Expected: FAIL.

- [ ] **Step 2: Edit the workflows**

`ci.yml`: after "Format check (ruff)" add
```yaml
      - name: Dependency register
        run: |
          uv run --no-sync python scripts/deps_register.py --check
          uv run --no-sync python scripts/deps_register.py --imports
```
and under `on:` add `workflow_dispatch:` (no `schedule`). `clients.yml`: `paths-ignore` on `push` and `pull_request`, `workflow_dispatch`; in the `go` job after "Test" add a step `Race detector on the concurrent packages` running `go test -race -count=1 ./internal/warden/... ./internal/router/... ./internal/mcpwire/...` with the same `CGO_CFLAGS`. `coppice.yml`: `workflow_dispatch` (its path filter stays).

A hand run (`workflow_dispatch`) on a workflow with `paths-ignore` runs the whole workflow (the filter applies to push and pull_request events only). A broken runner image or an expired action shows on the next push or on a hand run before the monthly fold; the owner runs one by hand when he wants to know sooner.

- [ ] **Step 3: Validate the YAML the way the runner does**

Run: `uv run --no-sync python -c "import yaml,glob; [yaml.safe_load(open(f)) for f in glob.glob('.github/workflows/*.yml')]"` and `uv run --no-sync pytest -q tests/test_workflows_shape.py`.
Expected: PASS. (The real proof is the next fold's push. Note this in the ledger: "only a push or a hand run proves the workflow".)

- [ ] **Step 4: `docs/how-to/testing.md`**

Add a "Dependencies" section with the two commands and what each failure line means, and a line under CI: "The three workflows run on push and by hand from the Actions tab. None runs on a timer. Run one by hand to check the runner image before the monthly push."

Commit:
```bash
git add .github/workflows/ci.yml .github/workflows/clients.yml .github/workflows/coppice.yml tests/test_workflows_shape.py docs/how-to/testing.md
git commit -m "ci: run the dependency register, skip docs-only pushes, add a hand run

The public repo gets one push a month, so a broken runner image or
an expired action is found on fold day; a hand run finds it early.
No timer (owner ruling D3). clients.yml no longer spends two hours of
runners on a docs-only change."
```

---

### Task 4: Small Python removals: `pytest-cov`, `build`, `click`

**Files:**
- Modify: `pyproject.toml`, `uv.lock` (`uv lock`), `src/opendaisugi/cli.py`, `docs/reference/dependencies.toml` (rows to `removed`, `click` to `keep` or `removed`), `docs/how-to/testing.md`, `scripts/release.sh` (header comment: `uv build`), `docs/reference/lock-names.txt`

**Interfaces:** DC-7, DC-8.

- [ ] **Step 1: Failing check**

Edit the register: `pytest-cov`, `build` rows to `status = "removed"`, `removed_on = "2026-11-..."`. Run `uv run --no-sync python scripts/deps_register.py --check`. Expected: FAIL (`row 'pytest-cov' status removed but declared in pyproject.toml [dev]`). The register check needs that rule: a `removed` row that is still declared fails.

- [ ] **Step 2: Remove the two from `[dev]`; `uv lock`; `--write`**

Run: `uv lock && uv sync --locked --extra dev && uv run --no-sync python scripts/deps_register.py --write && uv run --no-sync python scripts/deps_register.py --check`. Expected: PASS; `lock-names.txt` loses `pytest-cov`, `coverage`, `build`, `pyproject_hooks` (and whatever else only they pulled; list them in the commit).

- [ ] **Step 3: `click.Choice` to `Enum`**

Find the two uses: `grep -n 'click\.' src/opendaisugi/cli.py`. For each option, define `class <Name>(str, Enum)` with the same values and type the parameter with it. Remove `import click`. Run the CLI golden compare for both ports: `systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/cli_compare.py --binary clients/go/daisugi --oracle` and the Rust binary. Expected: 0 disagreements and no `record` difference on a help case. If a help case changes (typer prints `[a|b]` for click and `[a|b]` for Enum too, so none is expected), apply DC-8: revert, keep `click`, set the row's `why` to the measured reason.

- [ ] **Step 4: Whole Python suite**

Run: `HF_HUB_OFFLINE=1 systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync pytest -q`. Expected: green.

Commit:
```bash
git add pyproject.toml uv.lock src/opendaisugi/cli.py docs/reference/dependencies.toml docs/reference/dependencies.md docs/reference/lock-names.txt docs/how-to/testing.md scripts/release.sh
git commit -m "deps: drop pytest-cov and build from [dev], and click from core

No script, workflow or how-to passed --cov or ran build; uvx and uv
build do the same on demand. The two click.Choice options are Enums,
which typer renders the same way, so the CLI fixtures are unchanged."
```

---

### Task 5: DS-4, own the embedders in Python

**Files:**
- Create: `src/opendaisugi/_potion_loader.py`, `src/opendaisugi/_wordpiece.py`, `tests/test_potion_loader.py`, `tests/test_wordpiece.py`
- Modify: `src/opendaisugi/_search.py` (the potion and int8 paths), `src/opendaisugi/_model_fetch.py` (the potion files join the sha256-pinned fetch), `pyproject.toml` (`[potion]` becomes `numpy` only; `[int8]` becomes `onnxruntime`, `numpy`), `uv.lock`, the register, `docs/adr/0018-*.md` and `0021-*.md` (one line each: the loader is our own), `tests/conftest.py` (the `sentence_transformers_installed` style stubs for `model2vec` go)

**Interfaces:**
- `_potion_loader.load(dir: Path) -> PotionModel`; `PotionModel.encode(texts: list[str]) -> np.ndarray` (float32, shape `(n, dim)`), reading `model.safetensors` (own minimal safetensors reader: the 8-byte header length, the JSON header, the raw tensors) and `tokenizer.json` (the vocabulary and the unigram or WordPiece pieces potion ships; mirror `clients/go/internal/embed`'s reader, which already does this with no library).
- `_wordpiece.Tokenizer.from_json(path) -> Tokenizer`; `.encode(text, max_len=256) -> list[int]` with `[CLS]`, `[SEP]`, lowercase and accent stripping as `tokenizers`' BertNormalizer does for MiniLM (the Go port has this too; copy its rules and its test vectors).
- Pins: potion-base-8M files move to `_model_fetch.py`'s table with revision and sha256 per file (the Go and Rust loaders already pin sha256; copy theirs).

- [ ] **Step 1: Failing tests from the Go port's vectors**

Copy the Go test vectors (`clients/go/internal/embed/testdata/`) into `tests/fixtures/embed/` if they are not already shared, and write `test_potion_loader.py` (ten sentences, cosine with the Go vectors above 0.9999) and `test_wordpiece.py` (200 sentences from `clients/fixtures/pathways/cases.jsonl`, token ids byte-equal to a pinned list made once with `tokenizers` on the box and committed as JSON). Both tests skip when the model dir is absent (`HF_HUB_OFFLINE=1` in CI; the files are not in the repo), and a third test runs the tokenizer on the pinned list with no model at all (the vocabulary is in the fixture).
Run: `uv run --no-sync pytest -q tests/test_potion_loader.py tests/test_wordpiece.py`. Expected: FAIL.

- [ ] **Step 2: Write the loader and the tokenizer; switch `_search.py`**

Port from Go. `_search.py` imports `_potion_loader` and `_wordpiece` instead of `model2vec` and `tokenizers`; the `ImportError` paths that named `[potion]` now name only a missing model dir.

- [ ] **Step 3: Remove the three packages; register; lock**

`[potion] = ["numpy>=1.26"]`, `[int8] = ["onnxruntime>=1.18", "numpy>=1.26"]`; `huggingface_hub` stays in `[voice]` and `[hub]` only. `uv lock`, `--write`, `--check`.

- [ ] **Step 4: The matcher fixtures, both ports, and the Python suite**

Run (each a heavy job): `uv run --no-sync python clients/pathway_compare.py --binary clients/go/daisugi --probe <scratch>/pathway-probe --oracle`, the same for Rust, and the full Python suite. Expected: the same agreement counts as Task 1 recorded (380 matcher queries, 0 disagreements); Python green. A disagreement is a loader bug, not a fixture to regenerate.

Commit:
```bash
git add src/opendaisugi/_potion_loader.py src/opendaisugi/_wordpiece.py src/opendaisugi/_search.py src/opendaisugi/_model_fetch.py tests/test_potion_loader.py tests/test_wordpiece.py tests/fixtures/embed tests/conftest.py pyproject.toml uv.lock docs/reference/dependencies.toml docs/reference/dependencies.md docs/reference/lock-names.txt docs/adr/0018-*.md docs/adr/0021-*.md
git commit -m "matcher: own potion loader and WordPiece, so [potion] and [int8] need no model2vec, tokenizers or huggingface_hub

The Go and Rust ports load the same files with their own code and
pin them by sha256; Python now does the same, through the fetch
table that already pins the int8 files. The matcher fixtures agree
as before. DS-4."
```

---

### Task 6: DS-6, remove `textual-serve` (waits for the owner's DS-6 answer; skip while open)

**Files:**
- Modify: `src/opendaisugi/tui.py`, `src/opendaisugi/cli.py` (`dashboard --serve` option and its help), `pyproject.toml` (`[tui] = ["textual>=8.0"]`), `uv.lock`, the register, `docs/reference/module-wiring*.md` and `docs/how-to/*dashboard*.md` (the browser surface is coppice web), `tests/test_dashboard*.py`
- Delete: `src/opendaisugi/tui_web/app_index.html` and the local font folder if nothing else reads them

- [ ] **Step 1: Failing test:** `daisugi dashboard --serve` exits 2 with `dashboard: --serve is gone; the browser surface is coppice web (coppice web)`. The CLI golden suite holds the `dashboard` help; regenerate that one suite from the oracle after the change (a real behaviour change, recorded in the commit) and match the Go and Rust help (they print the Python help by the generated table; `clients/gen_help.py --check`).
- [ ] **Step 2:** remove the import, the option, the files; `uv lock`; register `removed`; `--write`.
- [ ] **Step 3:** CLI compare for both ports after `clients/gen_help.py`; Python suite green.

Commit message: `dashboard: the browser surface is coppice web; textual-serve goes (DS-6)`.

---

### Task 7: DS-6, replace the `mcp` SDK with an own stdio JSON-RPC server (largest task; its own branch `w3-mcp-own` if the wave runs long)

**Files:**
- Create: `src/opendaisugi/mcp_wire.py` (framing, JSON-RPC 2.0, the `initialize` handshake, `tools/list`, `tools/call`, `ping`, `notifications/*`, the refusals for `resources/read`, `completion/complete`, `tasks/*`, `resources/subscribe` with the SDK's exact error codes and messages), `tests/test_mcp_wire.py`
- Modify: `src/opendaisugi/mcp_server.py` (the twelve tools register on `mcp_wire` instead of `FastMCP`; tool schemas come from the same source `clients/go/internal/mcpwire/gen_tools.py` reads), `pyproject.toml` (`[mcp]` goes or becomes empty; `daisugi mcp serve` needs nothing beyond core), `uv.lock`, the register, `docs/adr/` (a short ADR: the Python MCP server is own code, the K3 sessions are the contract), `tests/test_mcp_server*.py`

**Interfaces:** `mcp_wire.Server(name, version).tool(name, schema, fn)`, `.serve(stdin, stdout)`; one request per line or Content-Length framing, whichever the SDK used for stdio (read `clients/fixtures/k3/cases.jsonl` to see; the K3 sessions are the spec).

- [ ] **Step 1: Failing test:** replay every K3 session's client lines into `mcp_wire` in process and compare the server lines to the recorded ones, ignoring `serverInfo.version`. Expected: FAIL on the first `initialize`.
- [ ] **Step 2:** write the wire until all 260 sessions replay. Port the refusal shapes from `clients/go/internal/mcpwire/wire.go`.
- [ ] **Step 3:** switch `mcp_server.py`; `gen_tools.py --check` passes (`python clients/go/internal/mcpwire/gen_tools.py --check`); the K3 compare for both ports (`clients/k3_compare.py`) agrees as before; the Python suite is green; `uv lock`; register; `--write`.
- [ ] **Step 4:** one hand check on the box with a scratch `~/.claude.json` copy: `claude --settings <scratch>` lists the twelve tools. Record the result in the ledger; no real model call.

Commit message: `mcp: the Python server speaks the wire itself; the 260 recorded sessions are the contract (DS-6)`.

---

### Task 8: DS-8 version alignment and DS-7 retirement

**Files:**
- Modify: `clients/rust/Cargo.toml` (`tree-sitter = "0.26"`, `tree-sitter-bash` to the matching release, `rusqlite` to the release whose bundled SQLite matches go-sqlite3 1.14.52; read `clients/go/.gomodcache/github.com/mattn/go-sqlite3@v1.14.52/sqlite3-binding.c` for `SQLITE_VERSION`), `clients/rust/Cargo.lock` (`cargo update -p tree-sitter -p tree-sitter-bash -p rusqlite`, nothing else), `clients/rust/NOTICE` (the SQLite line), `clients/ts/package.json` and lock (`web-tree-sitter` 0.26.x), `clients/ts/vendor/fetch.sh` (the grammar wasm's pinned sha256 for the new release), the register, `docs/reference/lock-names.txt`, `DECISIONS-PENDING.md` rows DS-7 and DS-8

- [ ] **Step 1: DS-7 is done or not.** Run `grep -rn GOTOOLCHAIN scripts harness/coppice/scripts clients/go/scripts`. Each hit must be inside a build script and set for that script's own process only (`export` inside the script, or a prefix on the `go` command), never written to `go env -w` or a profile. Expected today: four scripts, all local. Write it in DS-7's "Retires when" cell with the date.
- [ ] **Step 2: The bump, Rust.** `. clients/rust/tools/z3-build-env.sh; cd clients/rust; cargo update -p tree-sitter -p tree-sitter-bash -p rusqlite`. Confirm `git diff Cargo.lock` touches only those crates and their own deps. **Do not run `cargo clean`.** Build under the cap with `CARGO_BUILD_JOBS=2`; Z3 must not rebuild (watch the first lines of `cargo build -v` for `z3-sys`; if it appears, stop and read why before anything else).
- [ ] **Step 3: The bump, TypeScript.** `cd clients/ts && npm install web-tree-sitter@0.26.<x> --save-exact`; update `vendor/fetch.sh`'s URL and sha256; run `npm run build && npm test`.
- [ ] **Step 4: Compares.** `warden_compare.py --oracle` for Rust (the grammar), `pathway_compare.py` for Rust (the SQLite store), `clients/compare.py` for the TS client over the verifier corpus subset it uses in CI. Expected: the same counts as Task 1.
- [ ] **Step 5:** register rows (`tree-sitter` where: both versions now one), `--write`, `--check`; `DECISIONS-PENDING.md` DS-8 row retired with the date.

Commit message: `deps: one tree-sitter and one SQLite across the ports (DS-8); DS-7 verified and retired`.

---

### Task 9: Rust `thiserror` out

**Files:**
- Modify: `clients/rust/Cargo.toml` (remove `thiserror`), `clients/rust/Cargo.lock` (`cargo update -p thiserror` after removal, or the lock drops it on the next build), `clients/rust/NOTICE`, the register (`thiserror` removed; `qrterminal` and `rsc.io/qr` leave in Task 9b), `docs/reference/lock-names.txt`

- [ ] **Step 1:** `grep -rn thiserror clients/rust/src` is empty today; confirm. Remove the line; `cargo build --release` under the cap (`CARGO_BUILD_JOBS=2`); `cargo test --release --locked` under the cap. Expected: builds, tests pass, `Cargo.lock` loses `thiserror` and `thiserror-impl` (and `syn` only if nothing else pulls it; it will stay through `serde_derive`).
- [ ] **Step 2:** `--write`, `--check`.

Commit message: `deps: drop thiserror from the Rust daisugi (nothing used it)`.

---

### Task 9b: Our own QR encoder in coppice; `qrterminal` and `rsc.io/qr` leave

Owner ruling D4 (2026-10-30): write our own encoder with Reed-Solomon and drop both modules.

**Files:**
- Create: `harness/coppice/internal/qr/qr.go` (the encoder), `harness/coppice/internal/qr/gf.go` (GF(256) tables and Reed-Solomon), `harness/coppice/internal/qr/draw.go` (half-block drawing), `harness/coppice/internal/qr/qr_test.go`, `harness/coppice/internal/qr/testdata/golden.json` (the golden matrices)
- Modify: `harness/coppice/internal/web/qr.go` (`WriteQR` calls `qr.Draw`; `MaxQRColumns` stays), `harness/coppice/go.mod`, `go.sum` (drop `github.com/mdp/qrterminal/v3` and `rsc.io/qr`; `golang.org/x/term` stays, `internal/attach`, `cli` and `tui` use it), `harness/coppice/PINS.md` (the "terminal QR" row goes; a line says the encoder is our own, from ISO/IEC 18004), `harness/coppice/internal/web/pins_test.go` (drop the qrterminal check), `docs/reference/dependencies.toml` and `.md`, `docs/reference/lock-names.txt`, `NOTICE` (drop the two notices)
- Modify (Rust, no new dependency): `harness/coppice-rs/src/web/qr.rs` gets the same golden test (`include_str!` of the same `golden.json`, copied by a shared path: the file lives once, in `harness/coppice/internal/qr/testdata/golden.json`, and the Rust test reads it by relative path). Its header comment is corrected: it is a port of rsc.io/qr v0.2.0 (BSD-3-Clause) and `NOTICE` keeps that notice until the file is rewritten from the standard; a rewrite is a follow-on and not in this task.
- Test: `harness/coppice/internal/qr/qr_test.go`

**Interfaces:**
- Produces: `qr.Encode(text string, level Level) (*Code, error)` with `Level` `L|M|Q|H`; `Code.Size() int`, `Code.Black(x, y int) bool`; `qr.Draw(w io.Writer, c *Code, quiet int)` draws half blocks exactly as the current `WriteQR` does (quiet zone 2, `▀`, `▄`, `█`, space; the runes the current output uses are read from a captured sample in Step 1, not guessed).
- Encoding scope: modes numeric, alphanumeric and byte (UTF-8); versions 1 to 40; smallest version that fits; the standard's block and error-correction tables; GF(256) with the polynomial 0x11d; Reed-Solomon over the generator polynomial of the needed degree; format and version information with BCH codes; finder, timing, alignment patterns; **mask 0 always**, as rsc.io/qr v0.2.0 did, so the output equals today's output module for module (any mask is a valid code; one fixed mask keeps the Go and Rust outputs equal). Level `L` is what `WriteQR` uses. Text too long for version 40 returns an error; `WriteQR` prints nothing and the caller prints the URL, as it does today for a failed encode.
- Rule: written from the standard (ISO/IEC 18004), not translated from rsc.io/qr or from `qr.rs`. The reader of the Go file sees the standard's table names.

- [ ] **Step 0: Capture the golden matrices from the current code, before removing it**

On a scratch branch off the task's start, a throwaway test in `internal/web` calls `qrterminal`'s dependency `rsc.io/qr` (`qr.Encode(text, qr.L)`) for these inputs and writes `{"text", "level", "version", "size", "rows": ["0101...", ...]}` into `internal/qr/testdata/golden.json`: the empty string; `0`; `01234567` (numeric, the ISO example); `HELLO WORLD` (alphanumeric); `hello` (byte); a sign-in URL of 40 bytes, 78 bytes (a version boundary) and 120 bytes (`http://100.64.1.2:4664/?t=` plus 64 hex characters); a text with a multi-byte UTF-8 rune; and one input per version boundary at 1, 2, 5, 7 (version information starts), 10, 14 and 20. Also capture `Draw`'s output for three of them as `testdata/draw_*.txt` from the current `WriteQR`. Commit the fixtures alone: `qr: golden matrices from the encoder we are replacing`. The throwaway test is deleted and not committed.

**Independent decoder:** look for an offline decoder on the box once: `which zbarimg`, `python3 -c "import cv2; cv2.QRCodeDetector"`, a vendored `gozxing` in the Go module cache. If one is installed, a test-only helper (`//go:build qrdecode`, never in the default build, never a dependency in `go.mod`) renders each golden matrix to a PNG and checks the decoder reads back the text; record the decoder and its version in the ledger. If none is installed, the fixed matrices of Step 0 are the oracle: they are the output of the encoder that has been scanned by a phone in production, and the ISO `01234567` example is checked against the matrix printed in ISO/IEC 18004 Annex I by hand once (the builder writes the result in the ledger). Do not install a decoder from the network for this.

- [ ] **Step 1: Write the failing tests**

```go
func TestGoldenMatricesMatchModuleForModule(t *testing.T) {
	for _, g := range loadGolden(t) {
		c, err := Encode(g.Text, Level(g.Level))
		if err != nil { t.Fatalf("%q: %v", g.Text, err) }
		if c.Size() != g.Size { t.Fatalf("%q: size %d, want %d", g.Text, c.Size(), g.Size) }
		for y, row := range g.Rows {
			for x, ch := range row {
				if c.Black(x, y) != (ch == '1') { t.Fatalf("%q: module (%d,%d) differs", g.Text, x, y) }
			}
		}
	}
}

func TestReedSolomonKnownVector(t *testing.T) {
	// ISO/IEC 18004 Annex I: version 1-M "01234567": data codewords
	// 16 32 12 86 97 128 236 17 236 17 236 17 236 17 236 17;
	// the error-correction codewords are 165 36 212 193 237 54 199 135 44 85.
	got := rsEncode([]byte{16, 32, 12, 86, 97, 128, 236, 17, 236, 17, 236, 17, 236, 17, 236, 17}, 10)
	want := []byte{165, 36, 212, 193, 237, 54, 199, 135, 44, 85}
	if !bytes.Equal(got, want) { t.Fatalf("got %v want %v", got, want) }
}

func TestDrawMatchesTheCapturedSample(t *testing.T) { /* Draw(...) bytes equal testdata/draw_*.txt */ }

func TestTooLongTextIsAnError(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 3000), L); err == nil { t.Fatal("version 40-L holds 2953 bytes") }
}

func TestWriteQRStaysInsideMaxColumns(t *testing.T) { /* a 120-byte URL draws at most MaxQRColumns columns */ }
```

Run: `cd harness/coppice && systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G go test -p 1 ./internal/qr/ -count=1`
Expected: FAIL (package has no code).

- [ ] **Step 2: Implement `gf.go`, `qr.go`, `draw.go`; make the tests pass**

The Reed-Solomon vector above is the ISO example; if the builder finds the printed values differ from the standard, the standard wins and the ledger says which line was wrong.

- [ ] **Step 3: Switch `WriteQR`, drop the modules**

`WriteQR(w, text)` calls `qr.Encode(text, qr.L)` then `qr.Draw(w, c, 2)`. Run `go mod tidy` under the cap (offline: `GOFLAGS=-mod=mod GOPROXY=off`; the modules are in the cache). Confirm `git diff go.mod go.sum` removes only `qrterminal`, `rsc.io/qr` and what only they needed (`mattn/go-colorable` and `mattn/go-isatty` should not appear). Run `go test -p 1 ./internal/web/... ./internal/qr/...` and the coppice PINS test.

- [ ] **Step 4: Rust golden test**

In `harness/coppice-rs`: a test in `src/web/qr.rs` reads `../coppice/internal/qr/testdata/golden.json` and compares every matrix with `encode`. Run under the cap with `CARGO_BUILD_JOBS=2`. A mismatch is a bug in one port; fix the port that disagrees with the standard, and the ledger names it.

- [ ] **Step 5: Register, lock names, docs; commit**

`uv run --no-sync python scripts/deps_register.py --write && --check`; update `lock-names.txt`.

```bash
git add harness/coppice/internal/qr harness/coppice/internal/web/qr.go harness/coppice/internal/web/pins_test.go harness/coppice/go.mod harness/coppice/go.sum harness/coppice/PINS.md harness/coppice-rs/src/web/qr.rs docs/reference/dependencies.toml docs/reference/dependencies.md docs/reference/lock-names.txt NOTICE
git commit -m "coppice: our own QR encoder with Reed-Solomon; qrterminal and rsc.io/qr leave

Owner ruling D4 (2026-10-30). Mask 0 and the smallest fitting version, so
the sign-in QR is module for module what the old modules drew; golden
matrices captured from the old encoder pin it. One feature no longer
needs two modules."
```

---

### Task 10: The coppice server package: sleeps to waits, `t.Parallel` where isolated, split only by the measurement

**Files:**
- Create: `harness/coppice/internal/server/wait_test.go` (`waitFor(t, 5*time.Second, func() bool, msg)` polling every 10 ms; `waitForLine(t, reader, substr)`), and, only if DC-14 (3) applies, `harness/coppice/internal/server/stewardtest/` with the moved files
- Modify: every `*_test.go` in `harness/coppice/internal/server/` that calls `time.Sleep` (133 calls in 87 files; Task 1's `uniq -c` lists the arguments), `harness/coppice/README.md` (the test section: how to run the package, `-parallel`), `docs/how-to/testing.md`

**Interfaces:** `waitFor` fails the test with `msg` and the elapsed time at the deadline. A kept sleep carries `// models: <what>` on the same line (PK-8); a test pins this: `grep -n 'time.Sleep' *_test.go | grep -v '// models:'` is empty (Step 4 writes that as a Go test that reads its own package's files, as the sprig flag test does).

- [ ] **Step 1: Profile first.** From Task 1's JSON: the 40 slowest tests and the sleep histogram. Classify each sleep: (a) waits for a goroutine to reach a state (becomes `waitFor`), (b) models a timing rule of the server (the 1 s sustain, the 5 s quiet rule, the 30 s dialer end; keeps the sleep, with the comment, or uses the server's injected clock `s.clock()` where the ledger says it exists), (c) a pause with no reason (deleted).
- [ ] **Step 2: Convert in batches of ten files,** running `go test -p 1 ./internal/server -run '<the tests in the batch>' -count=3` under the cap after each batch (three runs: a flake shows). `TMPDIR=/var/tmp/od`.
- [ ] **Step 3: `t.Parallel()`.** For each test function: it qualifies when it calls the package's own `newTestServer`-style helper with a fresh `t.TempDir()` socket and data dir, sets no `os.Setenv`, changes no cwd, and reads no package-level `var` that another test writes. Add `t.Parallel()` as the first line. Run the package with `-parallel 4 -count=2` under the cap; peak memory must stay under 3 GB (`/usr/bin/time`), else lower to `-parallel 2` and write the measured number into the README.
- [ ] **Step 4: Measure.** `go test -p 1 ./internal/server -count=1 -json` under the cap, idle box, twice; record the mean. If over 200 s and the `steward_*` files hold over half (sum their test times from the JSON), do the split (DC-14 (3)): `git mv` the 15 files into `stewardtest/`, change `package server` to `package stewardtest`, import `server` and its test helpers through a small `servertest` helper package if needed, and run both packages. Else, write "no split: <s> after sleeps and parallel" in this plan's acceptance table.
- [ ] **Step 5: The whole coppice suite** (`go test -p 1 ./... -count=1` and once with `-race` on `./internal/server`), the coppice compare `--jobs auto` (2,082 cases, 0 disagreements), and `scripts/check-test-tmp.sh go` clean.

Commit (one per batch, then one for the parallel marks, then the split if any):
```
coppice server tests: wait on the condition, not the clock (files a to j)

Fixed sleeps made the package cost 300 to 750 s idle and over 2,000 s
under load, and a slow box turned them into flakes. Each wait now
names the state it waits for and fails with the elapsed time.
```

---

### Task 11: `verify-at.sh`, `touched-suites.sh`, and `build-at.sh` for the daisugi and sprig binaries

**Files:**
- Create: `clients/scripts/verify-at.sh`, `clients/scripts/touched-suites.sh`, `clients/scripts/suites.tsv` (suite name, command template, scratch env, heavy yes/no), `tests/test_verify_scripts.py` (runs the scripts with `BUILD_AT_BUILDER` and a fake suite table against a scratch git repo)
- Modify: `clients/scripts/build-at.sh` (kinds `daisugi-go`, `daisugi-rust`, `sprig-go`, `sprig-rust`; the Z3 warning), `docs/how-to/testing.md` (the box loop section), `AGENTS.md` "Build and test" (one paragraph: the box loop is `verify-at.sh --touched` between reviews and `--full` once before merge)

**Interfaces:**
- `verify-at.sh WORKTREE --touched BASE | --full [--suites a,b]`: resolves the head sha and dirtiness (`git status --porcelain` empty), builds the binaries it needs through `build-at.sh`, runs each suite from `suites.tsv` under one `systemd-run` scope per suite (never two at once), writes `/mnt/tera/.tt/verify/<sha>-<toolkey>/<suite>.{ok,fail,log}`, prints one line per suite (`ok 212 s warden-compare (cached)`), exit 1 on any fail. A dirty tree runs everything requested and caches nothing; the output says `dirty: not cached`.
- `touched-suites.sh BASE..HEAD`: prints suite names, one per line, from the prefix table (DC-12); prints `full` when a changed path matches nothing.
- `build-at.sh daisugi-go|daisugi-rust|sprig-go|sprig-rust WORKTREE`: prints a directory holding the binaries for that sha and toolkey. `daisugi-rust` and `sprig-rust`: DC-15, the Z3 warning: `cargo build --release --locked -v --dry-run 2>&1 | grep -q 'Compiling z3-sys' && echo "build-at: this build compiles z3-sys, a Z3 rebuild of about 29 minutes; continuing under MemoryMax=5G, -j2" >&2` before the real build, under the flock. The real build always runs under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G` with `CARGO_BUILD_JOBS=2`; the script sets that scope itself when the caller did not.

- [ ] **Step 1: Failing tests.** `test_verify_scripts.py`: a scratch git repo with two commits; `BUILD_AT_BUILDER` set to a script that writes a marker; a `suites.tsv` with two fake suites (`true` and `false`); assert: first run writes `ok` and `fail` files, second run at the same sha prints `(cached)` and runs nothing (the builder marker count does not grow), a dirty tree runs again and writes no cache, `--touched` with a change under a mapped prefix selects only that suite, a change under an unmapped path selects `full`, and, when a fake `cargo` prints `Compiling z3-sys`, the script writes the warning line to stderr, still runs the builder (the marker count grows by one) and exits 0; a second test asserts the real build line carries `MemoryMax=5G` and `CARGO_BUILD_JOBS=2`. Also the DC-12 coverage test over the real table: every top-level dir under `src/`, `clients/`, `harness/` has a prefix.
- [ ] **Step 2: Write the three scripts and the table.** Reuse `build-at.sh`'s toolkey and flock code; factor the shared parts into `clients/scripts/_lib.sh`.
- [ ] **Step 3: Real run, measured.** On an idle box: `verify-at.sh <worktree> --full` once (every suite; record total wall), then again (every line `(cached)`, total under 5 s), then `--touched master` after a one-line edit under `src/opendaisugi/warden.py` (only the warden suites run; record wall).
- [ ] **Step 4: Docs.** The box loop in `testing.md` and the paragraph in `AGENTS.md`.

Commit messages: `verify: one verification per head sha, cached on disk; fixers stop repeating the merge gate`; `build-at: the daisugi and sprig binaries join the per-sha cache; a Z3 rebuild prints a warning and stays under the memory cap (owner ruling D5)`; `verify: the touched-suite map, so a review round runs what the change can break`.

---

### Task 12: Parallel workers for the I/O-bound compares

**Files:**
- Create: `clients/parallel.py`, `tests/test_parallel.py`
- Modify: `clients/coppice_compare.py` (imports `auto_jobs` and `run_cases` from `clients/parallel.py`; behaviour unchanged), `clients/warden_server_compare.py`, `clients/k2_compare.py`, `clients/weave_compare.py`, `clients/sprig_compare.py` (`--jobs N|auto`, per-worker scratch and port), `clients/scripts/suites.tsv` (the `--jobs auto` flags), `docs/spec/conformance.md` (one paragraph: a case that fails under workers reruns alone once)

**Interfaces:** `auto_jobs(cpus: int, avail_kib: int, per_worker_kib: int) -> int` returns `max(1, min(cpus, avail_kib // per_worker_kib))`; `run_cases(cases, worker, jobs, rerun_alone=True) -> list[Result]` with `worker(case, slot) -> Result`, `slot` an int the worker uses for its scratch dir and port offset; order of results equals order of cases.

- [ ] **Step 1: Failing tests.** `auto_jobs` table (4 cpus, 12 GiB free, 2 GiB per worker gives 4; 4 cpus, 3 GiB free gives 1); `run_cases` keeps order, reruns a flaky case alone once and marks it `rerun_alone=True`, passes distinct slots; `coppice_compare.py --jobs auto` still prints the same summary line on the fake cases it has tests for.
- [ ] **Step 2: Write `parallel.py`; move `auto_jobs` out of `coppice_compare.py`.**
- [ ] **Step 3: Add `--jobs` to the four scripts.** Each worker's scratch is `<suite scratch>/w<slot>/`; port is `base + slot * 10`.
- [ ] **Step 4: Measure each, serial and `--jobs auto`, under the cap,** with `systemd-cgtop` sampled every 5 s into a log (peak memory per suite becomes `per_worker_kib` in `suites.tsv`, measured as peak divided by workers, rounded up). Also measure `warden_compare.py` and `cli_compare.py` with a trial `--jobs` branch; keep the option only on a gain of 30 percent or more (DC-13); record both numbers either way.
- [ ] **Step 5: Agreement counts unchanged** for every suite that gained `--jobs`, both ports, three runs each (a flake shows).

Commit message: `compare: parallel workers for the sleep-bound suites, inside one memory cap; the serial result is the contract`.

---

### Task 13: The Python suite (waits for the owner's pytest-xdist answer for Step 3; Steps 1 and 2 need no answer)

**Files:**
- Modify: the tests Task 1's `--durations=40` names (sleeps, real subprocess spawns that a fake would cover, repeated `uv run` children), `tests/conftest.py` (a session-scoped fixture for anything built once per run), `pyproject.toml` (`[dev]` adds `pytest-xdist` only on a yes; `addopts` gains `-n 2` only on a yes), the register

- [ ] **Step 1: The 40 slowest tests.** For each: a sleep becomes a wait; a real `claude`/`sprig` spawn that a test double covers uses the double; a per-test `subprocess.run([sys.executable, ...])` that imports the package becomes one child per module (collect the checks). Keep any test whose time is the thing it tests (`tests/test_speed.py`). Measure: `--durations=40` again; sum of the 40 before and after.
- [ ] **Step 2: Shared-state tests from Task 1 Step 4.** Each test that failed in the two-shell split and passed serially is fixed to own its state (`tmp_path`, `monkeypatch.setenv`, no module-level caches across tests). Rerun the split; it must be green. Record the split wall time: this is the number the owner sees in the walkthrough for pytest-xdist.
- [ ] **Step 3 (on a yes):** `uv add --optional dev pytest-xdist`, a row (`MIT`, `used_by = "[dev]"`), `--write`; `addopts = "-v --strict-markers -n 2"`; measure the full suite under the cap; CI's `pytest -q` picks it up from `addopts` (the runner has 4 cores). Record before and after. **On a no:** write the serial and the split numbers in the acceptance table and stop.

Commit messages: `tests: the forty slowest wait on conditions, not clocks`; `tests: every test owns its state, so two workers give the same answer as one`; and on a yes `tests: two workers by default (pytest-xdist); measured <before> to <after>`.

---

### Task 14: sccache (waits for the owner's answer; skip while open)

**Files (on a yes):**
- Modify: `~/opendaisugi-scratch/buildenv.sh` (not in the repo; `RUSTC_WRAPPER=$HOME/.local/bin/sccache`, `SCCACHE_DIR=/mnt/tera/.tt/sccache`, `SCCACHE_CACHE_SIZE=10G`), `clients/scripts/build-at.sh` (passes the wrapper through when set), `docs/how-to/testing.md` (one paragraph), the register (`sccache`, `tool`, `Apache-2.0`, "installed per box by `cargo install sccache --locked`; never in CI")

- [ ] **Step 1: Measure without.** `build-at.sh rust <wt-a>` and `build-at.sh rust <wt-b>` at two shas that differ in one coppice-rs file, deps reused (the ledger's 38.9 s case), and once after `touch`ing a dep crate's source to force a dep rebuild. Record all three.
- [ ] **Step 2: Install** under `~/.local/bin` (`cargo install sccache --locked`, under the cap, one job), set the env in `buildenv.sh`, `sccache --start-server`.
- [ ] **Step 3: Measure with**, the same three builds, cold cache then warm. **Keep it only if the dep-rebuild case gains 30 percent or more**; the one-file case is bounded by linking and will not change much. Write the numbers in the acceptance table either way; on a no-gain, `cargo uninstall sccache`, remove the env, and record "measured, not kept".

Commit (repo files only): `build: sccache for the Rust dep rebuilds, measured <before> to <after>; per box, never in CI`.

---

### Task 15: Docs, roadmap, DECISIONS-PENDING retirements, the ledger

**Files:**
- Modify: `docs/roadmap.md` sections 8 and 9 ("Where it stands" written as the current state: the register, the removals that landed, the box loop, the CI shape (push and hand runs, no timer); "Solved when" for 8 adds "and the register check is green"; for 9 adds "and the box loop runs once per head sha"), `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md` (DS-4 to DS-8 rows: "Retires when" cells filled with the date and the task; DS-6's three parts named one by one), `docs/reference/dependencies.toml` (every row's status is the truth at merge), `VISION.md` scorecard (the dependency line, if it has one), `clients/ADJUDICATIONS.md` (any refusal or disagreement found in Tasks 4 to 9, under a dated heading `DC-R-n`), the ledger

- [ ] **Step 1:** `uv run --no-sync python scripts/deps_register.py --check` green; `git grep -n -i -E 'litellm|instructor' -- ':!docs/research' ':!clients/ADJUDICATIONS.md' ':!docs/plans'` returns only the register's banned rows and the test.
- [ ] **Step 2:** fill the acceptance table below with every "After" number; copy it to the ledger.
- [ ] **Step 3:** the full merge gate once: `clients/scripts/verify-at.sh <worktree> --full` (this is also the proof of Task 11), zero fails. Then `git merge --ff-only` into master on the owner's word; the fold script pushes; watch the three workflows on GitHub through to green (the only place a workflow is proved).

Commit messages: `roadmap: sections 8 and 9 say what is built`; `decisions: DS-4 to DS-8 retired with dates`.

---

## Acceptance criteria

The wave is done when every row below has an "After" number and meets its target, or a written reason why not.

| Item | Before (Task 1 fills from the box; the sources above where known) | Target | After |
|---|---|---|---|
| Register check | no check | `scripts/deps_register.py --check` green in `pytest` and in `ci.yml`; under 2 s | |
| Banned names | one `sys.modules` test | eight planted cases fail the check (Task 2 test) | |
| Python locked names, `[dev]` | 85 | at most 80 after Task 4 (pytest-cov, coverage, build and their deps gone) | |
| Python locked names, `[potion]` over core | 36 vs 17 | 17 (numpy already in core) after Task 5 | |
| Python locked names, `[int8]` over core | 33 vs 17 | onnxruntime and its deps only (about 24) after Task 5 | |
| Python locked names, `[mcp]` over core | 41 vs 17 | 17 after Task 7 | |
| Python locked names, `[tui]` | 34 | textual and its deps only after Task 6 | |
| Rust `clients/rust` locked crates | 130 | at most 128 (thiserror, thiserror-impl) after Task 9 | |
| tree-sitter versions in use | 0.25 and 0.26 | one | |
| Python full suite, serial, idle, under the cap | (Task 1) | at least 20 percent less after Task 13 Steps 1 and 2 | |
| Python suite, two workers | (Task 1 Step 4) | reported; adopted only on the owner's yes | |
| coppice `internal/server`, idle | 305 s (2026-10-22); 500 s (owner); (Task 1) | under 200 s; no split unless DC-14 (3) applies | |
| coppice `internal/server`, `-race` | (Task 1) | under 400 s | |
| coppice all packages, idle | (Task 1) | reported | |
| coppice compare, full | serial 17 min 26 s; `--jobs auto` 8 min 58 s | unchanged or better; 0 disagreements | |
| `warden_server_compare`, `k2`, `weave`, `sprig` | (Task 1, serial) | each at least 30 percent less with `--jobs auto`, inside `MemoryMax=5G` | |
| `warden_compare`, `cli_compare` | (Task 1) | measured; `--jobs` kept only on a 30 percent gain | |
| Second `verify-at.sh --full` at the same sha | n/a | under 5 s, every suite `(cached)` | |
| `verify-at.sh --touched` after a one-file warden change | n/a | only the warden suites run; wall reported | |
| Rust daisugi binary at a known sha, second call | n/a (each worktree rebuilt) | under 1 s (a cache hit); any Z3 compile across the wave is counted (`grep -c 'Compiling z3-sys'` over every build log) and each one has the warning line in its log | |
| sccache | not installed | measured if the owner says yes; kept only on a 30 percent gain on dep rebuilds | |
| CI | `clients.yml` on every push; no timer; no register | docs-only push runs only `ci.yml`; hand run on all three, no timer; register step; `-race` on three Go packages; all green on the next fold push | |
| Disagreements introduced by removals | 0 | 0, both ports, every compare a task names | |

## Hand checks for the owner (after merge, on the box)

1. `uv run --no-sync python scripts/deps_register.py --check` prints nothing and exits 0; `--imports` the same.
2. Edit `pyproject.toml` to add `"litellm>=1"` to any extra; run `--check`; read the one line; revert.
3. `clients/scripts/verify-at.sh <a clean worktree> --full` twice; the second run prints `(cached)` on every line.
4. `go test -p 1 ./internal/server -count=1` in `harness/coppice` under the cap: read the wall time against the table.
5. After the fold, open the Actions tab: the push run is green, and a hand run of each workflow starts.

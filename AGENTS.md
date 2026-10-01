# AGENTS.md

The map for AI coding agents (and people) who work in this repo. Nested guides win for their own
directory: `harness/coppice/README.md`, `clients/go/README.md`, `clients/rust/README.md`.

**Read these first, in order, before non-trivial work:**
1. [VISION.md](VISION.md): what must stay true, and the honest scorecard.
2. This file: where things are and how to work.
3. [docs/spec/conformance.md](docs/spec/conformance.md): how the ports prove themselves.
4. [docs/architecture/CONVENTIONS.md](docs/architecture/CONVENTIONS.md): the Python coding
   patterns to match.

## Take the code as current state, not gospel

Every line of source and every comment here was written by an AI assistant. Treat it as an
accurate record of what exists now, offered with a grain of salt. A comment that claims an
invariant is a hypothesis to check. If a comment and the tests disagree, the tests win. If the
tests and reality disagree, reality wins.

## The parts and where they live

| Part | Path | Language |
|---|---|---|
| **daisugi**, the oracle: gate, verifier, journal, garden, gateway, orchestrator, MCP server, CLI | `src/opendaisugi/`, tests in `tests/` | Python 3.12+ |
| **daisugi** in Go: `daisugi`, `daisugi-gate`, `conform`, the probes | `clients/go/` (`cmd/`, `internal/`) | Go, cgo |
| **daisugi** in Rust: the same binaries | `clients/rust/` (`src/`, `src/bin/`) | Rust |
| Verifier-core clients for cross-checks | `clients/ts/`, `clients/lean/` | TypeScript, Lean 4 |
| Golden cases the oracle writes | `clients/fixtures/<component>/` | JSONL + manifest |
| Case writers and compare scripts | `clients/*_cases.py`, `clients/*_compare.py`, `clients/gate_fuzz.py`, `clients/fake_proxy.py` | Python |
| Every disagreement and its ruling | `clients/ADJUDICATIONS.md` | |
| **coppice**, the floor | `harness/coppice/` (read its `README.md`, `PROTOCOL.md`, `PINS.md`) | Go, cgo (libghostty-vt) |
| **sprig**, the loop, with `sprig-hook`, `sprig-mcp`, `weave` | `harness/sprig/` | Go |
| Install and release | `scripts/install.sh`, `scripts/preflight.sh`, `scripts/release.sh`, `packaging/aur/` | shell |
| Docs | `docs/` (hub: `docs/README.md`); decisions `docs/adr/`; current plans and designs `docs/plans/2026-09-24-omarchy/` | Markdown |

sprig and weave are **on hold with the owner**. grove is retired: coppice replaced it. Do not build on them, and write any design
that touches them as Proposed. coppice's sprig adapter reads `harness/sprig/cli.go` to check its
flag names, so a change there can break a coppice test.

Inside `src/opendaisugi/`, by concern:

| You are touching | Go to |
|---|---|
| The allow/deny decision | `gate.py`, `verify.py`, `subsumption.py`, `z3_checks.py`, `predicate_z3.py`, `regex_to_z3.py`, `effects.py`, `floor_config.py`, `pane_rule.py`, `rank_rule.py` |
| The data model | `models.py`, `permissions.py`, `predicate.py`, `aliases.py` |
| What runs and how it is supervised | `supervisor.py`, `executor.py`, `delegating_executor.py`, `approval.py`, `fallback.py` |
| Envelope generation | `envelope.py`, `tier1.py`, `llm.py`, `claude_code_llm.py` |
| Memory and reuse | `journal.py`, `distiller.py`, `gardener/`, `pathway*.py`, `signing.py` |
| Prompt to answer | `orchestrator.py`, `decomposer.py`, `model_sizer.py`, `budget.py`, `routing.py` |
| The gateway and router | `gateway*.py`, `router_switchyard.py` |
| Surfaces | `cli.py`, `mcp_server.py`, `install.py` |

## Non-negotiables

- **Fail closed.** Unprovable means reject; undeclared means deny. A new `match` on a step or
  permission type needs a default case that rejects. A Z3 timeout or `unknown` denies at the gate.
- **Verify before execute.** No effect before its plan is proven inside its envelope.
- **The caller's envelope is the ceiling** for reused pathways, delegated skills and plans from
  outside.
- **daisugi imports nothing above it** (`tests/test_layer_boundary.py`).
- **A port is never looser than the oracle.** A case a port cannot decide, it denies with a
  reason. A slow box may deny more; it may never allow more. Where a port and the oracle must
  agree, use step counts and size caps, not wall-clock budgets.
- **TDD.** Reproduce, write a failing test, fix, run the suite green, commit. Every bug fix ships
  with a regression test.

## The conformance method

Python is the oracle. The Go and Rust binaries are standalone (no Python at run time) and must
agree with it.

1. **The oracle writes the golden cases.** Each component has a case writer
   (`clients/gate_cases.py`, `cli_cases.py`, `pathway_cases.py`, `garden_cases.py`,
   `gateway_cases.py`). It runs the Python code and records stdin, argv, env and files in, and
   exit code, stdout, stderr, logs and files out. Cases are synthetic (fake paths such as
   `/work`), so they are committed, pinned by a manifest. `clients/fixture_paths.py` fails a
   generator that leaks a local path.
2. **A compare script runs a binary over the cases.** `clients/<component>_compare.py --binary B`
   compares structurally. `--oracle` reruns the oracle live to catch a stale fixture.
   `--fuzz N --seed S` runs seeded differential fuzz. A difference is **fail-open** (the binary
   allowed what the oracle denied: a blocker), **stricter**, or **record** (same verdict, a field
   differs).
3. **Every disagreement gets an adjudication.** Write a dated entry in
   `clients/ADJUDICATIONS.md`: what disagreed, which side is wrong, the ruling. When the oracle
   is wrong, fix the oracle first, regenerate the cases, then match the ports.
4. **Refusals count apart.** A binary may refuse a case (exit 2, one line, nothing changed) only
   where a ruling allows it. A refusal is never an agreement.

The frozen verifier corpus (11,450 cases, `daisugi conformance run`) holds real local paths. It
stays on the owner's box and is never committed or published.

## Build and test

### Python

```bash
uv sync --locked                       # the venv, from uv.lock
uv run pytest -q                       # must be green before you commit
uv run ruff check . && uv run ruff format --check .
```

### Go and Rust

Each line below is a heavy job. Run it under the cap in the memory rules, one at a time.

```bash
clients/go/scripts/native.sh           # once: the pinned native prefix (Z3, tree-sitter, libghostty-vt)
clients/go/scripts/build.sh            # daisugi-gate, daisugi, conform; then check-paths.sh
(cd clients/go && go test -p 1 ./...)
. clients/rust/tools/z3-build-env.sh   # before any cargo build
CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml
uv run --no-sync python clients/gate_compare.py --binary clients/go/daisugi-gate --oracle
(cd harness/coppice && go vet ./... && go test -race ./...)
```

The native prefix lives in `$XDG_CACHE_HOME/opendaisugi/native/<stamp>` (`~/.cache` when unset).
`clients/go/.native` is a git-ignored link to it.

### Memory rules (this box has 4 cores and 15 GiB; parallel builds have killed sessions)

- **One heavy job at a time.** A heavy job is a Z3 or cgo build, a cargo release build, a fuzz
  run, a full compare, or the full pytest suite. Do not start a second one, and do not run one
  beside another agent's.
- **Run every heavy job under a cap:**
  `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G <command>`.
- **Two jobs inside it at most:** `go test -p 1`, `CARGO_BUILD_JOBS=2`, `DAISUGI_NATIVE_JOBS=2`.
  Fuzz in chunks of at most 2,000 items.
- **Never rebuild Z3.** Do not delete the native prefix, do not change the pinned versions or
  flags in `native.sh`, and do not `cargo clean` in `clients/rust`. The Z3 build is the slow
  part: the Rust release tarball, Z3 included, took 29 minutes.
- **Scratch on real disk,** never `/tmp` (it is RAM here).
- **Never touch the owner's live state.** A trial gets its own `--socket` and `--data-dir` on
  scratch; environment variables do not select the coppice server. Never touch
  `~/.opendaisugi/gate`. Never run `daisugi install` in a trial; use
  `claude --settings "$(daisugi gate settings --root <scratch>)"`. Tests never run a real
  `claude`, `sprig`, `codex`, `pi` or `opencode`, and use no network but loopback.

### Lockfiles and pins

- `uv.lock` for Python. Ruling DS-1 retired the old "never commit uv.lock" rule: commit the
  lock, and CI runs `uv sync --locked --extra dev`. Update it with `uv lock` when `pyproject.toml`
  changes.
- `clients/go/go.sum`, `harness/coppice/go.sum`, `harness/sprig/go.sum`.
- `clients/rust/Cargo.lock`, `clients/ts/package-lock.json`.
- Native libraries: versions and SHA-256 digests in `clients/go/scripts/native.sh` and the
  `NOTICE` files. `scripts/release.sh` fails when a NOTICE misses a static library.

Ask the owner before you add a dependency. `docs/reference/dependencies.md` has the
licence, the reason and the status of each one that exists.

## Style

- **Plain English (ASD-STE100 register).** Short sentences, plain words, active voice. No em
  dashes. No three-part flourishes.
- **Names.** The parts are **daisugi**, **coppice** and **sprig**. Never write "the layer";
  write "daisugi". "The gate" means only the allow/deny part. Never use the word "seam"; write
  gap, boundary, interface or join. "Simplex" always means Sha's 1996 runtime-assurance
  architecture. Renames in `docs/correspondence.md` Part D are candidates, not decisions.
- **Honest docs.** Mark what is built and tested, what is designed, and what is open. Every
  number names its source. Synthetic cases prove correctness, not usefulness.
- **Roadmaps are problems,** not dated feature lists.

## Commits

- Atomic commits, one concern each. The message states the why and the failure it fixes.
- **No attribution lines.** No `Co-Authored-By`, no "Generated with". This is deliberate project
  policy.
- Commit by explicit path. Never commit `CLAUDE.md` or `docs/superpowers/`.
- Push only through the monthly fold script, on the owner's word. The public history is one
  commit a month.
- Never run `git push --all` or `git push --mirror`. The local `backup*` branches hold old
  history with machine paths in it. They stay in this clone and are never pushed.

## When you are unsure

Prefer rejecting to admitting. Prefer a failing test to a plausible fix. Prefer matching the
code around you to a new pattern. On a safety path, ask, or leave the question in a TODO, rather
than guess. Before you reopen a decision, read `docs/adr/` and
`docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md`.

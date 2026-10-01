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

The slots these parts fill, and the other tools that fill them, are in
[docs/explanation/landscape.md](docs/explanation/landscape.md). Recipes are in
[docs/recipes/](docs/recipes/README.md).

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

## Install

Commands that work today. Run `--dry-run` first to see what a command writes.

```bash
daisugi install --gate               # the gate hook for Claude Code and Codex; audit mode
daisugi install --gate --enforce     # needs a registered envelope: run `daisugi gate init --workspace ~/Work` first
daisugi install --harness pi         # or: opencode
daisugi install --gate --gateway     # also point the harness at the gateway
daisugi modules --pieces             # which pieces run here, and the command for each
coppice                              # the multiplexer; or: coppice web, for the browser and the phone
```

The Go `daisugi` installs only the gate and gateway layers. The Python `daisugi install` always
adds the skill, the MCP server, the capture hook and the pathway instructions. A gate-only or
gateway-only Python install flag does not exist yet. Hermes gets no `install --gateway` yet (it
expects an OpenAI wire that `install` does not write).

Memory over MCP alone: register the server by hand. Claude Code reads it from `~/.claude.json`.

```json
{"mcpServers": {"opendaisugi": {"type": "stdio", "command": "daisugi", "args": ["mcp", "serve"]}}}
```

Codex speaks the OpenAI wire. The gateway answers it on `/chat/completions`; see
[the gateway guide](docs/how-to/token-saving-gateway.md).

### Get the binaries

None of these needs Python at run time.

- **From source** (Linux): `scripts/install.sh`. You supply git, a C compiler (gcc), make,
  pkgconf, curl, python3, binutils and go. It names any of those that is missing before it builds
  anything. It fetches zig 0.16.0 and cmake itself (pinned, checksummed, no sudo, under
  `~/.local/share/opendaisugi`), and uses `GOTOOLCHAIN=auto` when your go is older than 1.26. It
  builds the pinned native libraries once, sends compiler output to a log file, and puts
  `daisugi`, `coppice` and `sprig` in `~/.local/bin`. The voice engines build only with
  `--voice`. `INSTALL_DRY=1` prints the plan and builds nothing. A Python `daisugi` script
  already there is kept unless you pass `--replace-daisugi`.
- **A release tarball**: `scripts/release.sh VERSION` builds it; `--rust` builds a second
  tarball with the Rust `daisugi`. Unpack with
  `tar -xzf opendaisugi-VERSION-linux-x86_64.tar.gz --strip-components=1 -C ~/.local/bin`.
- **Omarchy and the AUR**: `omarchy-mise-install github:levitatingflyfisher/openDaisugi coppice`,
  or the packages in `packaging/aur/`: `opendaisugi` (or `opendaisugi-bin`) puts `daisugi` (the
  Go build), `daisugi-rs`, `daisugi-gate-rs`, `coppice` and `sprig` in `/usr/bin`, and
  `daisugi-py` adds the Python one, so the three can check each other. `daisugi install` writes
  hooks that run the Go `daisugi`; with `DAISUGI_PORT=rust` or `DAISUGI_PORT=python` it hands the
  install to `daisugi-rs` or `daisugi-py`, whose hooks then run that port directly. Both routes
  need a published release. **There is no published release yet.**

### The Python package

```bash
uv add opendaisugi          # or: pip install opendaisugi
```

Python 3.12 or later. The Python package is the oracle: every command lives here, and the Go and
Rust binaries are checked against it. Extras: `[generate]` (httpx, for model calls over
HTTP: the Anthropic API, Ollama, llamafile), `[mcp]` (the MCP server), `[search]` (the sentence-transformers matcher), `[robotics]`,
`[lora]`. With no extra, pathway reuse uses the zero-download lexical matcher.


## The binaries

| Binary | Language | What it carries |
|---|---|---|
| `daisugi` (Python) | Python | Everything: the gate, verify, pathways, garden, gateway, orchestrator, supervisor, MCP server, onboarding, dashboard. The oracle. |
| `daisugi` (Go) | Go | Gate commands, `install` (`--gate`, `--harness`, `--gateway`), `pathways`, the garden (`gardener`, `tend`, `hook auto-tend`, `distill-repeats`), `gateway`, `gateway-report`, `router`, `route`, `status`, `config`, `start --no-ui`, `journal`. |
| `daisugi` (Rust) | Rust | The same as the Go binary, except `status`, `config`, `start` and `journal`. |
| `daisugi-gate` | Go and Rust | The hook contract alone. |
| `coppice` | Go | The floor. |
| `sprig` | Go | The loop, plus `sprig-hook`, `sprig-mcp` and `weave`. (grove is retired: coppice replaced it.) |

The Go and Rust binaries link real Z3 5.1.0 statically and parse shell with tree-sitter-bash, the
same grammar the oracle uses. Each links only glibc (the Rust one also `libgcc_s`). A command a
binary does not carry prints `<command> is not in this binary yet.` and exits 2. Only the Python
package has `orchestrate`, `dashboard`, `mcp`, `onboard` and model-written envelopes today.
Porting them is stage K of the [part 2 plan](docs/plans/2026-09-24-omarchy/plan-part2-ports.md).


## What is proven, and how

The Python package is the oracle. It writes golden cases; each port must agree with it. Numbers
below are from the reference box, 2026-09-25 and 2026-09-26, from `clients/go/README.md`,
`clients/rust/README.md` and the build ledger. They prove the ports agree with the oracle
(correctness). They say nothing about usefulness on real work.

They were measured before five oracle fixes committed late on 2026-09-26 (the security queue in
[VISION.md](VISION.md#honest-scorecard-2026-09-26)). The Go and Rust mirrors of those fixes are not
committed yet, so the committed ports lag the committed oracle there.

A refusal (exit 2, one line, nothing changed) is allowed only by a ruling and is never counted as
agreement.

| Suite | Cases | Go | Rust |
|---|---|---|---|
| gate cases (`clients/fixtures/gate/`) | 1,310 | 0 disagreements | 0 disagreements |
| CLI cases (`clients/fixtures/cli/`) | 472 | 451 agree, 12 not ported, 9 refused, 0 disagreements | last full run at 306 cases: 0 disagreements; `status`, `config`, `start`, `journal` not ported |
| pathway commands, matcher queries, verify messages | 158, 380, 577 | 0 disagreements | 0 disagreements |
| garden cases | 215 | 0 disagreements; at 200 cases: 188 agree, 12 refused | the same |
| gateway cases | 223 | 0 disagreements; 1 refused (GW-8) | the same |
| weave cases (`clients/fixtures/weave/`, 2026-09-30) | 88 | 86 agree, 2 refused (WV-R-10), 0 disagreements | the same |

- **Fuzz.** Seeded differential fuzz against the in-process oracle, for example 36,000 shell
  commands plus 24,000 heredoc, multi-line, non-ASCII and envelope calls for the Go gate
  ([clients/go/README.md](clients/go/README.md)). No run found a call a binary allowed and the
  oracle denied.
- **The verifier corpus.** 11,450 cases recorded from real use. It holds local paths, so it is
  frozen on the owner's box and never published. Rust matches 11,450 of 11,450
  ([clients/rust/README.md](clients/rust/README.md)). TypeScript and Lean 4 clients check the
  verifier core too ([docs/client-diversity.md](docs/client-diversity.md)).
- **Adjudications.** Every disagreement, and every oracle bug a port found, has an entry in
  [clients/ADJUDICATIONS.md](clients/ADJUDICATIONS.md). Several were fail-open bugs in the
  oracle itself, now fixed.
- **Speed.** A gate call, cold process, p50: Go 8.8 ms, Rust 6.1 ms, Python about 430 ms. A call
  that needs the Z3 vacuity check takes about 20 ms. From tarball to the first gate decision:
  0.52 s.

The method is in [docs/spec/conformance.md](docs/spec/conformance.md).


## Honest limits

- **Usefulness is not measured yet.** Every number above comes from synthetic cases. The next
  step is a week of daily use: the gate's false-deny rate from its audit log, the gateway on
  real traffic, and a friction log for coppice.
- **Not an OS sandbox.** The gate decides tool calls a harness sends through its hook. For
  process isolation, use SELinux, AppArmor or seccomp under it.
- **Codex hooks fail open.** A Codex hook that crashes, times out or prints bad JSON lets the
  tool run (`src/opendaisugi/install.py`). A deny works; a dead gate does not block.
- **sprig and weave run with the gate off unless you pass `--gate`**
  (`harness/sprig/cli.go`). The owner agreed to turn it on by default once it runs smoothly. That
  is not done.
- **Actions, not understanding.** An envelope can prove an action stayed in bounds. It cannot
  prove the model understood the task. A perceptual check (`llm_check`) is a judgment, not a
  proof.
- **Token savings depend on your work.** A reused deterministic pathway spends zero tokens. The
  average saving depends on how much of your work repeats. A large file read can go to a local
  worker through the `delegate` tool, which returns checked quotes; that saving is an estimate,
  and billed cost can rise while tokens fall. `daisugi router status` shows both per week.
  Measure it with `examples/jit-metrics/`.
- **No published release, no release signing.** Build from a git ref you have read.

More in [docs/limitations.md](docs/limitations.md) and the scorecard in [VISION.md](VISION.md).

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

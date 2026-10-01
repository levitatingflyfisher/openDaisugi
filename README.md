<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.png">
    <img alt="openDaisugi: as simple as the job allows, but no simpler" src="docs/assets/logo.png" width="560">
  </picture>
</p>

<p align="center">
  <a href="https://github.com/levitatingflyfisher/openDaisugi/actions/workflows/ci.yml"><img src="https://github.com/levitatingflyfisher/openDaisugi/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/python-3.12%2B-blue.svg" alt="Python 3.12+">
  <img src="https://img.shields.io/badge/license-MIT-green.svg" alt="MIT">
</p>

**openDaisugi is a JIT compiler for AI agents.** The first time, a frontier model does the
work and a formal proof checks every action before it runs. The tenth time, the work runs as a
verified script for free. One gate, whether the agents write code or fly drones.

The idea under it: **separate what is _allowed_ from what is _decided_.** A model decides. An
envelope says what is allowed. Z3 proves each action stays inside the envelope before the
action runs. In enforce mode, when the proof fails, the action does not run (fail closed). In
audit mode, the default at install, the gate logs what it would deny and lets the call run.

## The three parts

| Part | What it is | Where |
|---|---|---|
| **daisugi** | The gate, the verifier, the journal and the garden. It allows or denies each tool call, records it, and compiles repeated work into pathways. `daisugi weave` runs a verified plan tree, with typed slots between its steps. | `src/opendaisugi` (Python), `clients/go`, `clients/rust` |
| **coppice** | The floor. One Go binary that holds your agents, their panes and their state, in the terminal, the browser and on a phone. State comes from the gate, not from screen scraping. | `harness/coppice` |
| **sprig** | Our own small agent loop, in Go. On hold with the owner. | `harness/sprig` |

Claude Code, Codex, pi and OpenCode run on the floor too. Their tool calls pass the gate once
its hook is in: `daisugi install --gate` for Claude Code and Codex, `daisugi install --harness pi`
or `--harness opencode` for the other two. A Codex hook that crashes or times out lets the call
run; see Honest limits. See [ADR-0020](docs/adr/0020-layer-floor-loop.md) for why the
parts are separate.

## Quick start

With the binaries on PATH, a floor of guarded agents is two commands:

```bash
daisugi install --gate    # the gate hook for Claude Code and Codex; audit mode: it logs what it would deny
coppice                   # the floor in this terminal; or: coppice web, for the browser and the phone
```

To enforce, register a policy first. With no policy, `install --gate --enforce` refuses and
writes nothing, because an enforcing hook with no policy denies every call:

```bash
daisugi gate init --workspace ~/Work   # a starter envelope: reads and writes in ~/Work, a short list of shell commands
daisugi install --gate --enforce
coppice
```

### Get the binaries

None of these needs Python at run time.

- **From source** (Linux): `scripts/install.sh`. It needs Go, zig 0.16.0, cmake, gcc, make,
  pkgconf, git, curl and python. It names every missing tool before it builds anything, builds
  the pinned native libraries once, and puts `daisugi`, `coppice` and `sprig` in `~/.local/bin`.
  A Python `daisugi` script already there is kept unless you pass `--replace-daisugi`.
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

## See it refuse an unsafe action

No API key, no network. The plan an LLM proposed must stay inside the envelope:

```python
from opendaisugi import ActionPlan, Daisugi, Envelope, Permission, ShellStep

dai = Daisugi()
envelope = Envelope(
    generated_by="you",
    task="clean up stale logs",
    permissions=Permission(shell=True, shell_allowlist=["find"]),
)
plan = ActionPlan(
    source="some-llm",
    task="clean up stale logs",
    steps=[ShellStep(id="s1", command="rm -rf /var/log")],  # not `find`
)
result = dai.verify(plan, envelope)  # sync, milliseconds, zero tokens
print("allowed:", result.ok)
for v in result.violations:
    print(f"  [{v.stage}] {v.message}")
```

```
allowed: False
  [permissions] Step 's1' shell command 'rm' not in allowlist ['find']
```

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

## Documentation

- [VISION.md](VISION.md): the one idea, the invariants, the honest scorecard.
- [AGENTS.md](AGENTS.md): the map for anyone (person or agent) who works in this repo.
- [docs/README.md](docs/README.md): the hub, in the Diátaxis split (tutorials, how-to,
  reference, explanation).
- Start: [protect a session you already run](docs/tutorials/protect-your-existing-session.md),
  [gate a session](docs/how-to/gate.md), [the phone](docs/how-to/phone.md),
  [voice](docs/how-to/voice.md), [the token-saving gateway](docs/how-to/token-saving-gateway.md).
- Floor: [harness/coppice/README.md](harness/coppice/README.md).
- Theory: the [white paper](docs/whitepaper.md), the [yellow paper](docs/spec/yellow-paper.md),
  [concepts across fields](docs/correspondence.md), [prior art](docs/research/prior-art-2026-09-26.md).
- Direction: the [strategy notes](docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md), the
  [Omarchy roadmap](docs/plans/2026-09-24-omarchy/ROADMAP.md), the designs in
  [docs/plans/2026-09-24-omarchy/](docs/plans/2026-09-24-omarchy/), and the
  [decisions pending](docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md).
- Decisions: [docs/adr/](docs/adr/). History: [CHANGELOG.md](CHANGELOG.md).

## License

MIT. See [LICENSE](LICENSE). Third-party notices: `clients/go/NOTICE`, `clients/rust/NOTICE`,
`harness/coppice/NOTICE` and `harness/sprig/NOTICE`.

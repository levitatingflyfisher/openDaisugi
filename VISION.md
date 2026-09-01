# Vision

> The north star for openDaisugi. If you (person or agent) are about to change something
> load-bearing, read this first. It says what must stay true and why. For how it is built, see
> [AGENTS.md](AGENTS.md) and [docs/architecture/OVERVIEW.md](docs/architecture/OVERVIEW.md).
> For why each decision was made, see [docs/adr/](docs/adr/).

## The one idea

**Separate what is _allowed_ from what is _decided_.** A black box decides: an LLM, a neural
policy, a VLA. It is capable and useful, and it cannot be verified. What is allowed comes from a
space of checkable calculations. The black box proposes; a checker disposes.

The move that makes this work:

> **An LLM closes the verification loop not by becoming verifiable, but by _generating_
> verifiable constraints. The generated spec is checkable even though the process that
> generated it is not.**

You cannot verify the model. You can verify a specification the model writes, the
**envelope**, and you can prove that each proposed action stays inside it before the action
runs.

The same separation pays a second dividend. An action proven in bounds is safe to make cheaper:
replay it as a script, route it to a smaller model, drop context that no longer needs to hold the
policy. The envelope is not the token saver. It is what makes the savers safe
([ADR-0011](docs/adr/0011-verifiable-execution-substrate.md)).

**Owner ruling: the bots write the policy.** No one learns an envelope language. Models write
envelopes, rules and words. The proofs are mechanical. A human sets a ceiling and reviews.

## A JIT compiler for agents

The analogy fits better now than when it was first used
([strategy notes](docs/plans/2026-09-24-omarchy/strategy-2026-09-26.md)):

| JIT part | openDaisugi part |
|---|---|
| Interpreter (slow, general, always works) | the frontier model, reasoning from scratch |
| Profiler | the journal: every action, verdict and token |
| Tiered compilation | the routing ladder: pathway (0 tokens), local model, warm cache, cheap model, frontier |
| Profile-guided optimizer | the garden, which compiles repeated work into pathways |
| Type checker | Z3 and the envelope, before anything runs |
| Deoptimization | fail closed: a failed proof falls back to the frontier or to a human |
| The runtime's sandbox | the gate |
| Debugger and console | coppice |

The pitch in one breath: *openDaisugi is a JIT compiler for AI agents. The first time, a
frontier model does the work and a formal proof checks every action before it runs. The tenth
time, it runs as a verified script for free. One gate, whether the agents write code or fly
drones.*

The bitter lesson argues against hand-tuned specialization, not against efficiency. A pathway is
a specialization the system earns from its own journal and drops when it stops being true.

The lineage is **runtime assurance** from aerospace control: Simplex (Sha, 1996), verified
envelopes, barrier certificates. openDaisugi points it at LLM agents and robot foundation models.

## Three parts

| Part | What it is | Who owns the code | What must stay true |
|---|---|---|---|
| **daisugi** | gate, verifier, journal, garden. `verify(plan ⊆ envelope)`, fail closed. | ours | importable alone; imports nothing above it |
| **coppice** (the floor) | panes, state, prompt, steer, phone, voice, foreman | ours | every state it shows has a source; `unknown` when none |
| **the loop** | the harness in a pane: Claude Code, Codex, pi, OpenCode, sprig | rented, except sprig | every tool call passes the gate; an unreachable gate blocks |

A coppice is a stand of trees cut back so each stump sends up straight shoots. Daisugi is a
coppicing technique. See [ADR-0020](docs/adr/0020-layer-floor-loop.md), and
`tests/test_layer_boundary.py` for the test that holds daisugi apart from the rest.

## Where it goes

### Dialects: one kernel, many languages

The owner's breakthrough, 2026-09-26. openDaisugi seemed to serve two worlds that did not meet:
safe "muscle memory" for a robot swarm, and a few agents running a software startup. A proof is
universal; only the vocabulary differs. So:

- **One kernel.** The predicate algebra stays small, fixed and decidable. The guarantees live
  there.
- **Many dialects.** Each domain gets named words defined only in kernel terms: `safe_git_op`
  and `no_prod_write` for a software team; `keep_separation(d)` and `within_geofence(zone)` for
  a swarm. A word unfolds to the kernel, so every question stays decidable.
- **Models write and grow the dialect; humans read it; Z3 checks the unfolded kernel.** The
  garden mines the journal for repeated sub-predicates, a model names them, and Z3 proves two
  words equal so the vocabulary does not grow duplicates. This is library learning applied to
  the policy language.
- **Muscle memory at two levels.** A pathway in the dialect is one agent's skill. A word useful
  across the population joins the shared dialect.
- **Dialects meet at the kernel.** Two teams can prove their policies agree.

The first test ran on 2026-09-26 over a copy of the owner's journal (294 distinct envelopes, flat
permission sets only, a greedy pass). Fifteen named words cut the size by 17.5%, and the words
read like a dialect ("read-only inspection", "build and test", "config files"). Small data and a
simple method; the next test runs Stitch over envelopes and plans as trees. The design is
[design-dialects.md](docs/plans/2026-09-24-omarchy/design-dialects.md). It grows from the
definitions that already ship (`src/opendaisugi/aliases.py`).

### Full parity

Owner ruling, 2026-09-26: Python, Go and Rust ship as competing binaries that check each other
for mistakes, correctness and speed. Python is the oracle. It writes golden cases, and each port
must agree on every case and on seeded fuzz. A port never allows what the oracle denies; a case
it cannot decide, it denies with a reason. Anything a Go or Rust user would need Python for is
ported to both. The map is the [part 2 plan](docs/plans/2026-09-24-omarchy/plan-part2-ports.md).

### Problems, not a feature list

- **Does it help on real work?** Every number so far comes from synthetic cases. They prove
  correctness, not usefulness. The answer is a dogfood week: the gate's false-deny rate from its
  audit log, the gateway metered on real traffic, coppice driven across real projects with a
  friction log, and the top three pains fixed before any new design.
- **Does reuse stay correct?** A reused pathway must be as correct as fresh work, across many
  tasks, on a local model ([roadmap Stage 4](docs/roadmap.md)).
- **Can a router save tokens it can prove?** Cheap first, the verifier escalating, a chooser
  learned from the journal ([design-router.md](docs/plans/2026-09-24-omarchy/design-router.md)).
- **Output checks.** openDaisugi checks actions before they run; the labs check outputs after. A
  pass/fail feature list is the missing "done" signal for a task and the missing label for the
  router.
- **Perception-conditioned envelopes.** Every formal guarantee is conditional on perception
  being right. Envelopes that tighten under uncertainty are the principled answer, and nobody has
  built them for foundation models.

See [the North Star](docs/north-star.md) for the far horizon.

## The invariants (do not break these)

Breaking one is a design regression, not a feature.

1. **Fail closed.** Unprovable means rejected. Undeclared means denied. A Z3 timeout or
   `unknown` denies at the gate. In a verification library a fail-open is the worst bug.
   ([ADR-0001](docs/adr/0001-fail-closed-default.md))
2. **Verify before execute.** No effect happens before its plan is proven inside its envelope.
   Each step is checked again at run time.
3. **The envelope is the authorization ceiling.** Reused pathways, delegated skills and plans
   from outside are bounded by the caller's envelope, never their own.
   ([ADR-0003](docs/adr/0003-envelope-as-contract.md))
4. **Independent provenance.** The "allowed" spec must not come from the same untrusted source as
   the "decided" plan. Envelopes carry a more trusted parent and can only tighten.
5. **daisugi stays importable alone.** The gate, verifier, journal and garden import nothing
   above them and run in any harness. ([ADR-0020](docs/adr/0020-layer-floor-loop.md))
6. **A port is never looser than the oracle.** Go and Rust agree with Python on every case, or
   deny. A slow box may deny more; it may never allow more. Where a port and the oracle must
   agree, limits are step counts and size caps, not wall-clock budgets.
7. **Verify actions, not understanding.** An envelope can prove the arm stayed under 5 N. It can
   never prove the model understood you wanted the fork and not the knife. This gap does not
   close. It gets bounded.

## Honest scorecard (2026-09-26)

This code and its comments were written by an AI assistant. Treat them as what exists now, not as
gospel. Verify a claim before you rely on it. Numbers are from the reference box, as committed on
2026-09-26.

### Built and tested

- **The daisugi spine in Python** (package version 0.43.0): envelope, `verify`, subsumption,
  supervisor, journal, distillation into signed pathways, the orchestrator, the token-saving
  gateway and the Switchyard router client, the MCP server. Measured on a benchmark-generated
  journal: the symbolic guard runs a sub-4 ms median with a 0% timeout rate.
- **Standalone gates in Go and Rust.** No Python at run time. Real Z3 5.1.0 linked statically,
  tree-sitter-bash for shell. 1,310 gate cases with 0 disagreements in both, measured before the
  security queue below. Cold call p50: Go 8.8 ms, Rust 6.1 ms, Python about 430 ms.
- **Most of daisugi in Go and Rust.** Install, pathways and matchers (lexical and potion),
  the garden, the gateway and router commands, each with golden cases and fuzz at 0
  disagreements. Go also has `status`, `config`, `start --no-ui` and `journal` (472 CLI cases).
  A case a port does not carry is refused under a ruling. A refusal is not counted as
  agreement. The record is
  [clients/ADJUDICATIONS.md](clients/ADJUDICATIONS.md).
- **One-script install.** `scripts/install.sh` from source; `scripts/release.sh` builds a
  tarball; tarball to the first gate decision took 0.52 s in a scratch home.
- **coppice, the floor.** TUI, web and phone on one screen; state from the gate hook (verdict,
  clause, mode, model, tokens); ended agents in Recent with Resume; one-click new agent and a
  project picker; ready prompts that wait for a loop to be ready and refuse on a question; voice;
  a foreman that starts and directs agents. The part 1 tasks are merged. The hand checks in
  [RESUME.md](docs/plans/2026-09-24-omarchy/RESUME.md) wait for a person.
- **A first security queue, in the Python oracle.** A payload's session id never selects an
  envelope; an operator's edited call passes the whole gate again; subsumption checks stakes,
  custom steps and budgets; no agent can run `daisugi rank record`; definitions substitute
  structurally and system words cannot be redefined. The Go and Rust mirrors of these fixes are in progress, not committed.
- **Our own model client** (`llm_client.py`) in place of litellm and instructor: the Anthropic
  Messages API and OpenAI-compatible chat completions (Ollama, llamafile), with the same request
  bytes in Python, Go and Rust (rulings LLM-1 to LLM-15). The lock lost 15 packages.

### Designed, not built

- The router ([design-router.md](docs/plans/2026-09-24-omarchy/design-router.md)).
- Grafts, a fourth gate verdict, **rewrite**: "do this cheaper call instead", proved at install
  ([design-grafts.md](docs/plans/2026-09-24-omarchy/design-grafts.md)).
- Ranking as a primitive beside verify
  ([design-ranking.md](docs/plans/2026-09-24-omarchy/design-ranking.md)).
- Dialects ([design-dialects.md](docs/plans/2026-09-24-omarchy/design-dialects.md)).
- A delegation tree (a tree of agents) with a proved edge from each parent to each child
  ([design-delegation-tree.md](docs/plans/2026-09-24-omarchy/design-delegation-tree.md)). Proposed.
- weave as the runner for verified plan trees
  ([ADR-0022](docs/adr/0022-weave-runner-for-verified-plan-trees.md)). Proposed.

The owner adopted each design's recommended answers as provisional rulings, to confirm or
overturn later. They are in
[DECISIONS-PENDING.md](docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md). A ruling that is hard
to undo, or that touches sprig or weave, waits for the owner before code depends on it.

### Open

- **Usefulness.** No number yet comes from real work. The dogfood week is next.
- **Parity is not complete.** Stage K (orchestrator, supervisor, decomposer, MCP server,
  onboarding, model-written envelopes, dashboard), ingest, voice, and coppice and sprig in Rust
  are not ported. Rust lacks `status`, `config`, `start` and `journal`. Until the Go binary has
  the dashboard, the Python `daisugi` stays on the owner's PATH.
- **sprig and weave are on hold.** grove is retired: coppice replaced it. They run with the gate off unless `--gate` is passed
  (`harness/sprig/cli.go`). The owner agreed to default-on once the gate runs smoothly; it is not
  done.
- **No published release.** mise and the AUR need a tagged release, and the repo's history
  policy has no tags. The owner decides. Release artifacts are not signed.
- **Reuse fidelity at scale**, robotics on hardware, papers and revenue: still hopes. The
  robotics code is sim-only and plan-level.

The core is real. Anything that needs a publisher, a customer or robot hardware is still a
hope. Keep that line bright.

## Name

**Daisugi** (台杉) is a forestry technique where straight new timber grows from the trunk of an
existing tree, without new seeds. The black box is the rootstock; the verified pathways are the
new growth. The routine work becomes compiled pathways; the model reasons only about what is
new.

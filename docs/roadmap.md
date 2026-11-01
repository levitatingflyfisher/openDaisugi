# Roadmap

> Problems, not promises. For this project, to describe a capability exactly enough to
> schedule it is most of the work of building it. A dated feature list would be wrong
> the day it was written. So each section below is a **problem openDaisugi cannot yet
> solve**, with the **evidence that would prove it solved**. When the evidence exists,
> the problem is solved. Until then it is not, whatever the commit log says.
>
> This page is the near ground. The far horizon (perception-conditioned envelopes,
> robots on real hardware, the three dreams) is in [north-star.md](north-star.md) and
> [VISION.md](../VISION.md).

Last refreshed 2026-09-26, from the build ledger, the port READMEs
([`clients/go/README.md`](../clients/go/README.md),
[`clients/rust/README.md`](../clients/rust/README.md)) and the designs in
[`plans/2026-09-24-omarchy/`](plans/2026-09-24-omarchy/).

## Where the line is today

openDaisugi has three parts ([ADR-0020](adr/0020-layer-floor-loop.md)): **daisugi**
(the warden, the verifier, the journal and the garden), **coppice** (the floor) and
**sprig** (our loop). The owner put sprig and weave on hold on 2026-09-26. grove is retired: coppice replaced it.

- **daisugi** is built in Python, the oracle. Its everyday half is also built twice
  more, as standalone Go and Rust binaries with real Z3 linked statically and no Python
  at run time. The ports agree with the oracle on every committed golden case.
- **coppice** is one Go binary: the floor in a terminal, in a browser and on a phone.
- **Two commands** make a floor of guarded agents: `daisugi install`, then
  `coppice` (or `coppice web`). `scripts/install.sh` builds all three binaries from
  source. No release is published yet; release tags wait on the owner.

Every number below that comes from golden cases or fuzz shows **correctness**: the ports
agree with the oracle. None of it shows **usefulness**. The warden's real false-deny rate,
the router's real saving and the floor's real friction are not measured yet. That is the
first problem.

The problems below are ordered by what the owner asked for first, not by dependency.

---

## 1. The floor people love

**The problem.** Can one person run agents across about ten projects from one screen,
every day, and like it? The Omarchy goal ranked this first. The last days of work went
mostly to the ports, and every test of the floor uses fakes. The owner's worry: we built
too much and too little. The floor may not hold ten projects, the warden may block too
much, and the router may not save tokens.

**Solved when:**
- A **dogfood week** is done: one port job at most in the background; the warden's
  false-deny rate measured from its audit log; the router metered on real traffic;
  coppice driven across the owner's real projects with a friction log; and the top three
  pains fixed before any new design.
- The hand checks in [`plans/2026-09-24-omarchy/RESUME.md`](plans/2026-09-24-omarchy/RESUME.md)
  pass on a real terminal, a real browser, a phone and a microphone.
- The phone floor is used for a week.

**Where it stands.** Part 1 of the Omarchy plan is built and merged: ended agents and
Recent, one-click new agents and the project picker, one screen for rail and windows
(web and TUI), the stack bar and warden mark, a fix button on every error, voice with no
setup, the phone, and the steward. Ready prompts (queue until the harness is ready,
refuse on a trust dialog) were fixed after a live run found three failures. A live run
on an isolated rig showed the Go daisugi hook working under Claude and sprig panes. Not
done: the dogfood week, the hand checks, and a real Codex binary against its adapter
(see [`harness/coppice/README.md`](../harness/coppice/README.md), "What is not verified
yet").

**Design:** [ROADMAP.md, part 1](plans/2026-09-24-omarchy/ROADMAP.md),
[spec-part1-floor.md](plans/2026-09-24-omarchy/spec-part1-floor.md),
[strategy notes, "The worry, and the answer"](plans/2026-09-24-omarchy/strategy-2026-09-26.md).

## 2. Full parity across Python, Go and Rust

**The problem.** The owner ruled on 2026-09-26 that openDaisugi ships competing binaries,
Python, Go and Rust, that check each other for mistakes, correctness and speed. Anything a
Go or Rust user would need Python for must be in both ports. Every port stage is built.
What a Go or Rust user still needs Python for is the inventory in
[plan-part2-ports.md](plans/2026-09-24-omarchy/plan-part2-ports.md) ("Full parity").

**Solved when:** every part a Go or Rust user needs runs in both binaries with no Python;
each part agrees with the oracle on its golden cases and its seeded fuzz with 0
disagreements; and the speed budgets hold (a warden check under 10 ms at p50, `--help`
under 20 ms, a router turn under 5 ms added).

**Where it stands** (stages from
[plan-part2-ports.md](plans/2026-09-24-omarchy/plan-part2-ports.md)):

| Stage | What | Go | Rust |
|---|---|---|---|
| A | the warden hook, verifier, decomposition, effects tiers, hard-deny rules | Done | Done |
| B, C, G (warden half) | config, envelopes, warden root, journal; `install` | Done | Done |
| D | pathways, matchers (lexical, potion), the garden | Done | Done |
| E | the router and the router client | Done | Done |
| G (rest) | `status`, `config`, `start`, `journal` | Done | Done (3c7592f2) |
| F | ingest and transcript parsers | Done | Done |
| K | envelope generation, Tier-0, bind and compose, orchestrator, supervisor, decomposer, MCP server, onboarding, modules, dashboard, exporter | Done | Done |
| G2 | the resident warden server (`warden serve`) | Done | Done |
| L | deeds, batch compilation, strata, pathway signing, the git pathway registry | Done | Done |
| H | voice client and server | Done (2026-10-01) | Done (2026-10-01) |
| I | coppice in Rust | n/a | Done (2026-10-02, `harness/coppice-rs`) |
| J | sprig in Rust | n/a | Done (2026-10-02, `harness/sprig-rs`; 185 compare cases agree with Go) |
| J2 | sprig as an agentic executor (`--agent sprig`) | Done (2026-10-09, SX-R-1 to SX-R-10) | Done (2026-10-09) |

The committed golden cases, and how many each port agrees on, are in
[feature-status.md](feature-status.md) and [conformance.md](spec/conformance.md). Speed at
p50, cold process (measured before stage K): the Go warden 8.8 ms, the Rust warden 6.1 ms, the
Python warden about 430 ms.

**Next:** the inventory's rows marked "port", then its rows marked "open". One heavy job
at a time on the box.

**Design:** [plan-part2-ports.md](plans/2026-09-24-omarchy/plan-part2-ports.md),
[conformance.md](spec/conformance.md), [`clients/ADJUDICATIONS.md`](../clients/ADJUDICATIONS.md).

## 3. Safety gaps found by the designs

**The problem.** Writing the September designs meant reading the warden with fresh eyes.
That found fail-open and privacy gaps. A fail-open in daisugi is the worst bug class this
project has.

**Solved when:** each gap has a failing test first, a fix in the Python oracle, the same
fix in Go and Rust, and regenerated cases with 0 disagreements.

**Where it stands.**

| Gap | Python | Go and Rust |
|---|---|---|
| Subsumption ignored stakes, the custom step list and the time and output budgets; a Z3 unknown could let a delegation through | Fixed (f9e202e7) | In progress |
| A payload's session id could select another session's envelope, or fall back to `default` | Fixed (31871c4d) | In progress |
| An operator's edited call skipped the hard-deny rules | Fixed (6a0ad499) | In progress |
| Definitions: text splicing into regex fields, a lower tier redefining a system word, swallowed vacuity errors | Fixed (92f87edf) | In progress |
| An agent could record a ranking vote as the owner (for the future `rank record`) | Denied by a hard-deny rule (1fad0036) | In progress |
| Importing litellm fetched its cost map from the internet | Retired: litellm is gone (DS-2) | n/a |
| sprig, weave and sprig-mcp ran with the warden off unless `--warden` was given | n/a | Fixed (86ce2b80 Go, 99319c3b Rust): on by default in the installed mode; `--warden=false` is the only opt-out; a missing `daisugi` denies every call (SG-11) |
| Models wrote "opaque" invariants (for example `file_unchanged`) that the warden never enforced at low or medium stakes | Fixed (777f0adb, SG-8 to SG-10) | Fixed (182e7f6f, 2254b6cf) |
| Shell write targets were not visible to the permission stage (`cp`, `tee`, `sed -i`, `tar`) | Fixed (c0e2f986, e1d705b7, SG-1 to SG-6) | Fixed (182e7f6f, 2254b6cf) |
| Read-many commands (`cat`, `grep -r`, `tar -c`, `find`, `xargs`) were not checked against `file_read` | Fixed (c0e2f986, e1d705b7) | Fixed (182e7f6f, 2254b6cf) |

Still open: the warden places the commands in a closed table. Interpreter work, `sed` script
commands (`w`, `e`) and commands outside the table are not placed (SG-12;
[limitations.md](limitations.md)). MCP tools are covered by argument rules (ADR-0027); what
stays open is rules over tool results, live schemas, sprig's `Spawn`, and cross-call limits.

Designed, not built (from the [prior art, section 6](research/prior-art-2026-09-26.md#6-imp-and-gepa-added-2026-09-27)):
- **Tool outcome classes in the journal.** The journal records each tool call's outcome
  class: refused (it did not run, so a retry is safe), not sent, or unknown (it may have
  run). An "unknown" call goes to receipts and rollback (the deed ledger,
  `src/opendaisugi/deeds.py`), never to a retry. The classes come from Imp's `CallFailure`.
- **An Imp warden adapter.** An Imp `authorize` function that asks daisugi's resident warden
  over its socket (`src/opendaisugi/warden_server.py`; `daisugi warden serve` in Go and Rust),
  so Imp agents get proof-backed tool checks. A warden that does not answer means deny.

**Design:** the "Conflicts found" and "Gaps" sections of
[design-grafts.md](plans/2026-09-24-omarchy/design-grafts.md),
[design-dialects.md](plans/2026-09-24-omarchy/design-dialects.md),
[design-delegation-tree.md](plans/2026-09-24-omarchy/design-delegation-tree.md) and
[design-ranking.md](plans/2026-09-24-omarchy/design-ranking.md).

## 4. Token saving that holds up on a bill

**The problem.** The router routes whole turns, keeps the prompt cache warm and meters
tokens, in all three languages. But its router is a fixed rule of thumb that does not
learn and cannot tell if a cheap model's work was correct. And no measurement yet shows a
saving in **billed cost per successful task** on real traffic. Research shows that fewer
tokens can mean a larger bill (see the
[mapping doc, September 2026](research/token-saving-landscape-mapping.md#september-2026-grafts-the-router-and-billed-cost)).

**Solved when:** on the owner's own recent work, held out from training, a new router or
graft lowers billed cost per successful task with no drop in task success, and the meter
shows it with its estimates marked.

**Where it stands.** Built: the router, the rules routing mode with cache-aware sticky routing
([ADR-0015](adr/0015-cache-aware-sticky-routing.md)), Switchyard as a managed child, the
meter and `router report`. Designed, not built:
- **Router parts 0 to 4:** delegate routine reads to a cheap worker (part 0); cheap first,
  the verifier escalates (1); the frontier plans, cheap models do the steps (2); learn from
  the owner's journal (3); measure it honestly (4). Parts 0 and 4 go first.
- **Grafts:** a fourth warden verdict, **rewrite**, that replaces a tool call or its result
  with a cheaper one by a named rule, proved at install and checked on every call. The
  first shape is deny and redirect (ruling GR-1), which is router part 0.
- **GEPA over graft rules:** the garden's optimizer, scored by proofs, tests and warden
  verdicts, proposes better graft rules (see problem 5).

**Design:** [design-router.md](plans/2026-09-24-omarchy/design-router.md),
[design-grafts.md](plans/2026-09-24-omarchy/design-grafts.md),
[grafts research inputs](research/grafts-inputs-2026-09-26.md).

## 5. Dialects: one kernel, many languages

**The problem.** Every envelope is written in one fixed language, the same for a coding
task and a drone. The owner's idea: keep the kernel (the permission record and the
predicate algebra) small and decidable, and let models grow a vocabulary of named words
per domain, each defined only in kernel terms. Z3 checks the unfolded kernel; humans read
the words; no one learns a DSL.

**Solved when:** a real compressor finds shared words in real envelopes and plans; each
word unfolds to the kernel and Z3 keeps duplicates out; a deny names the word, not a
regex; and promotion to a shared dialect happens only on measured reuse.

**Where it stands.** The first test ran on 2026-09-26 over a copy of the owner's journal
(294 distinct envelopes, permission sets only, a greedy pass): 15 words cut the size by
17.5%, and the words read like a dialect (read-only inspection, build and test, code
forges, config files). The number is in-sample, over flat sets, by a greedy pass; the
next test runs Stitch over envelopes and plans as trees, with a held-out check. A second
finding matters more: the same envelopes hold 322 invariants and none has a checkable
expression, so at low and medium stakes they are documentation, not policy. The first
payoff of dialects is to give those words one kernel definition each. Predicate definitions,
the part that already ships, were hardened in 92f87edf.

Designed, not built: **GEPA as the garden's optimizer** (arXiv 2507.19457; see the
[prior art, section 6](research/prior-art-2026-09-26.md#6-imp-and-gepa-added-2026-09-27)).
GEPA rewrites prompts by reflecting on failed runs and keeps a Pareto frontier of
candidates. Its metric here is proofs, tests and warden verdicts: a candidate that scores
higher but adds a deny or breaks a proof loses. Three targets: the envelope-generation
prompts (stage K1 in problem 2), the prompts that name dialect words (this problem), and
graft rules (problem 4). A GEPA run spends model calls, so it is a heavy job under the
owner's rules: about ten short live sessions a day, one heavy job at a time.

**Design:** [design-dialects.md](plans/2026-09-24-omarchy/design-dialects.md),
[prior art](research/prior-art-2026-09-26.md).

## 6. Ranking as a primitive

**The problem.** The router, the garden's A/B test, a parent agent with several children
and the owner reviewing work all need one answer: given N attempts at one task, which is
best, and how sure are we? Today there is no ranking code in daisugi.

**Solved when:** `daisugi rank` sits beside `daisugi verify`, in Python first and then Go
and Rust. It eliminates on hard checks, ranks on measured scores, breaks ties with model
judges and the owner's picks (Bradley-Terry), asks the owner only the most useful pair,
and journals every comparison. coppice shows it as an arena.

**Where it stands.** Built, with two parts still open. In Python, Go and Rust: `rank fit`,
`choose`, `queue`, `record`, `show`, `rank judge` (the model judge, two calls per pair, another model
family than the author), `judges` (judge trust and Thompson-sampled vote weights, owner ruling A3)
and `pivot`, with a 14-day decay (owner ruling A7). The three ports agree on 181 rank cases.
coppice shows the open cards in its cards view, the arena, with two levels (no code, then the
diff), four sorts, a badge in the garden, and Keep, Switch and Drop as the owner's answer only
([how-to](how-to/review-cards.md)). Not built: the SQLite index (a bound test stands in), the
router's labels from `rank` (the router plan owns them), and the garden A/B consumer, which has
no attempts to rank yet. Its rulings (RK-1 to RK-9) are provisional.

**Design:** [design-ranking.md](plans/2026-09-24-omarchy/design-ranking.md),
[ranking research inputs](research/ranking-inputs-2026-09-26.md).

## 7. The delegation tree

**The problem.** An agent that starts agents is a way around the warden, unless each child
is proved to fit inside its parent. Permissions must only shrink down the tree.

**Solved when:** before any child runs, its envelope is proved inside its parent's by the
subsumption check; a failed proof, a timeout or no proof means the child does not start;
and coppice shows the tree and the asks that need a person.

**Where it stands.** **Built.** The daisugi part is built in Python, Go and Rust: `edge_ok`
proves each edge, strict and fail closed; the deadline is an envelope field proved per edge;
`warden register --parent` and `daisugi tree spawn` prove before they register; the tree ledger
splits tokens and turns down the tree; the fourth refused proposal from a parent goes to the
operator; an agentic step's child envelope is proved (rulings TR-R-1 to TR-R-12). Branching is
built in all three: `tree run` with `attempts` and `why` starts N children in their own git
worktrees, ranks the survivors, opens a card and keeps the leader's edits for `tree adopt`
([how-to](how-to/branching.md)); the steward's `spawn` takes the same two fields. coppice's part
is built: an envelope on a task is proved against its parent on `task.create`, registered under
a minted session, and every pane in it starts with that session in its warden hook; a test
shows a pane cannot name another pane's session; the tree view shows each node's budget and
proof and carries the operator's allow or deny for an ask. Open: only a fenced claude pane can
be pinned to a task's envelope, and the cards of the steward's branches sit in the steward's
work directory, which the floor reads only when `COPPICE_DAISUGI_DATA_DIR` names it.

**Design:** [design-delegation-tree.md](plans/2026-09-24-omarchy/design-delegation-tree.md),
[ADR-0022](adr/0022-weave-runner-for-verified-plan-trees.md) (Proposed).

## 8. Fewer dependencies, all of them lawful

**The problem.** Every dependency is a supply-chain risk, a licence question and, for a
local-first tool, a possible network call. The sweep counted 16 packages in the core
Python install and 176 across every extra, 10 Go modules, 64 linked Rust crates and 12
native libraries.

**Solved when:** each ruling DS-1 to DS-8 is carried out, and each dependency left has a
reason.

**Where it stands.** The sweep found nothing we cannot legally use. Done: `uv.lock` is
committed and CI runs `uv sync --locked` (DS-1); `click` is declared (DS-5, the other
imports are not yet checked); `[search]`, and so torch and the CUDA wheels, is out of dev
and CI, and the default matcher is lexical, with potion one step away in `tiers setup --matcher potion` (DS-3); one own model client in Python, Go and
Rust replaces litellm and instructor, with an OpenAI-compatible wire for Ollama and
llamafile (DS-2, rulings LLM-1 to LLM-15; the lock lost 15 packages). Next: own the embedders
(DS-4); drop `textual-serve` and `av`, replace the `mcp` SDK and networkx (DS-6); scope
`GOTOOLCHAIN=auto` (DS-7); align tree-sitter and SQLite across the ports (DS-8).

**Design:** [dependency sweep](research/dependency-sweep-2026-09-26.md),
[DECISIONS-PENDING.md](plans/2026-09-24-omarchy/DECISIONS-PENDING.md).

## 9. Self-testing and CI

**The problem.** Parity is proved today by compares run by hand on the owner's box. A
proof that runs only on one box is a proof nobody else can check. And every check we have
tests actions before they run; nothing yet checks the output after.

**Solved when:** every golden compare (warden, CLI, pathway, garden, router) runs for Go
and Rust on every push, on standard GitHub runners with a cached Z3 build, and is green;
and a pass/fail feature list, as the labs use, gives the router its labels and a coppice
task its "done".

**Where it stands.** Committed: `ci.yml` (ruff, pytest, the adversarial merge check) and
`coppice.yml` (vet, gofmt, tests, the race detector). In progress in the working tree: a
`clients.yml` workflow that builds the native prefix once, caches it, runs the Go and Rust
tests and every compare against the committed fixtures, with no model and no network. Not
in CI: the Playwright phone smoke test (runs behind `COPPICE_SMOKE=1`) and every live
model run (at most about ten short sessions a day, by the owner's rule). Output checks are
not designed.

**Design:** [strategy notes, "What the labs say"](plans/2026-09-24-omarchy/strategy-2026-09-26.md),
[conformance.md](spec/conformance.md).

---

## The earlier stages (1 to 10)

Stages 1 to 7 were the enforcement spine; stages 8 to 10 the cost levers opened by
[ADR-0011](adr/0011-verifiable-execution-substrate.md). The full text of each, with its
"solved when" list, is in this file's git history. They are built in Python. Stages 8, 9
and 10 (`deeds.py`, `batch.py`, `strata.py`) are not in any port stage yet, neither in the
list to port nor in the list to keep in Python.

| Stage | The problem | Status | Evidence, and what is open |
|---|---|---|---|
| 1. The warden | Deny a live tool call by default when its proof fails | Solved (v0.35.0) | `tests/test_hook_warden_contract.py` denies a real Read in the real Claude Code CLI; every failure path denies (`tests/test_warden.py`); `warden disarm`; round trip in [how-to/warden.md](how-to/warden.md). The host-session envelope came with stage 6. |
| 2. Delegation | Sub-agents with real tools, inside the parent's envelope | Mechanism built (v0.36.0) | `AgenticStep`, `AgenticExecutor`; a live test denies a real sub-agent's out-of-envelope read. Open: every stage 3 attack from inside a sub-agent. |
| 3. Evidence | A safety claim someone else can check | Deterministic replay suite done (v0.37.0) | 13 attacks in 7 categories, 9 benign, a required CI check. Attack denial: no warden 0.00, literal glob 0.46, warden 1.00; false positives 0.33, all known. Yellow paper §8. Open: the live bait-taking rate at N, with intervals. |
| 4. Distillation fidelity | Does distillation pay? | Ruler built (v0.39.0); not solved | Pilot (qwen3:4b, 3×3): warm cheaper in direction, 95% intervals span zero, warm success 56% vs cold 67%. claude-code run (4×3): both 100%. Open: 20 tasks × 5 repeats with a local model near 90% run success. |
| 5. Harnesses | Say per harness: hard, soft or observation | Partly | Claude Code contract-tested. Hermes and OpenClaw unverified. The grafts research has since read each harness's hook docs; live contract tests are still open. |
| 6. Onboarding | From "heard of it" to a deny on my machine | Mechanism built | `daisugi start`; the two-command floor; a release tarball to the first warden decision in 0.52 s in a scratch HOME. Open: a published release, and time-to-first measured on a clean machine. |
| 7. Trust | Why a stranger should run this | Four of five | Public CI, content-addressed corpus, supply-chain page, signed pathways; `daisugi release sign` exists. Open: a published release with a signed manifest (release tags wait on the owner). |
| 8. Reversibility | A wrong but allowed action costs a rollback | Solved (v0.41) | `deeds.py`, `tests/test_deed_ledger.py`: rollback from the ledger alone. Out of scope: the warden path, where the harness makes the effect. |
| 9. Within-instance compilation | One proof for a declared batch | Mechanism and meter (v0.42) | `batch.py`, `tests/test_batch.py`. The meter's honest finding: the win is the proved blast radius, not tokens. Open: cross-instance numbers at scale (shares stage 4's need). |
| 10. Rationale durability | Never re-derive what compaction dropped | Mechanism (v0.43) | `strata.py`, `tests/test_strata.py`; "don't touch tenant X" holds under forced compaction. Open: re-derivation numbers with a model. |

---

## What we are deliberately not building

An honest roadmap also names the closed doors, and what would open them.

- **Fine-tuning on distilled pathways.** No corpus is worth training on until stage 4
  produces one. *Opens when:* a measured corpus exists.
- **An HTTP-daemon form of the hook.** It fails open when the connection fails. *Opens
  when:* a design makes connection failure deny.
- **Deep integration with harnesses that cannot block.** The work stops at the contract
  test and the published finding. *Opens when:* the contract test passes.
- **A third full stack.** Lean stays the proof of the verifier core. Bend 2 gets a pilot,
  not a port. *Opens when:* the pilot shows a reason.
- **Ports of research code.** The robotics executors, LoRA training, the benchmark harness,
  the Textual TUI and the tmux and Herdr floor backends stay in Python or retire; coppice
  replaces the last two. *Opens when:* a Go or Rust user needs one.
- **A full diff viewer.** The review screen is a pairwise "which is better" arena, which is
  light (ruling ST-5). *Opens when:* the arena is not enough.

sprig and weave are not on this list: they are not closed, only on hold with the
owner. Every design that touches them is Proposed.

The through-line: every problem ends in something checkable, a test against a real host,
a published rate, a spec section with its limits stated, a number in the scorecard. That
is the [one idea](../VISION.md), applied to the project itself: do not ask anyone to trust
the process; give them something they can verify.

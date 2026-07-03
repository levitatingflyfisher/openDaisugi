# Changelog

## v0.34.2 — 2026-07-02 — claude -p flag passthrough + failed-step reasons

Two fixes for running the orchestrator on the `claude -p` (Claude Code) backend:

- **Forward flags to `claude -p`.** There was no way to pass
  `--dangerously-skip-permissions`, `--allowedTools`, or any other Claude Code
  flag through the backend, so in environments where `claude -p` requires
  interactive permission approval it couldn't act — every affected step failed and
  the run reported `failed`. Set `DAISUGI_CLAUDE_ARGS` (shlex-parsed) to forward
  flags to EVERY `claude -p` call site — e.g.
  `DAISUGI_CLAUDE_ARGS='--dangerously-skip-permissions'` or
  `DAISUGI_CLAUDE_ARGS='--allowedTools "Bash(ls:*) Read"'`. Opt-in; unset = no
  change. Also routed `ClaudeCodeTier1Provider` through the shared injection-safe
  argv builder (fixing its old prompt-after-`-p` / `--model <v>` construction so it
  too honors the env flags).
- **A failed step now says why.** `StepOutcome.error` was left `None` on any
  non-timeout failure, so a `failed` status came with no explanation (the reason
  was buried in the step's captured output). It now carries `exit <rc>: <reason>`,
  and the `run` / `orchestrate` CLIs (text and `--json`) surface per-step failure
  reasons — so "failed" is never reason-less. NOTE: the run-status aggregation was
  verified correct (a genuinely-errored step drives `failed`; a succeeded step is
  never mislabeled) — this was an observability gap, not a status bug.

## v0.34.1 — 2026-07-02 — Security hardening, part 2 (DoS bounds)

Follow-up to v0.34.0 closing the two availability findings that were deferred there:
- **EB-3:** `SubprocessExecutor` buffered the whole of a command's stdout via
  `communicate()` before applying the size cap, so a permitted-but-noisy command
  (`cat /dev/zero`, `yes`) could exhaust memory. Now read in a bounded reader thread
  (stderr is merged, so a single reader can't deadlock) and SIGKILL the process group
  the instant the cap is hit — memory is bounded and the run returns promptly instead
  of spinning to the timeout. Group-kill/grandchild-reaping discipline is preserved.
- **EB-4:** `NetworkExecutor`'s `timeout_s` was only the per-recv socket timeout, so a
  slow-drip server (a byte before each timeout) could hold the executor far past
  `step_timeout_s`. Reads are now bounded against an overall wall-clock deadline.

Still tracked, lower severity: the envelope-improvement breadth gate (M7, needs a
permission-union primitive; partially mitigated by v0.34.0's template-verification),
and a few LOW items. M9 (llm_check on error) was reviewed and confirmed already
fail-closed (`satisfied=False`); its `errored` flag is telemetry only.

## v0.34.0 — 2026-07-02 — Security hardening (SGCM multi-agent review)

**Upgrading from 0.33.x — some fixes are intentionally fail-closed and change
behavior.** If a previously-working flow starts rejecting after upgrade, this is
why (and each is the correct, safer default):
- **Shared git registries** (`GitPathwayStore`): the trust anchor is no longer the
  in-repo `trusted-signers.json` (it was remotely controlled). Configure a LOCAL,
  out-of-band anchor — pass `trusted_signers_path=…` (or drop a JSON file next to
  the cache). Until you do, signed pulls reject with a warning. This is the most
  likely upgrade surprise.
- **Live MCP `run_plan(dry_run=False)`** no longer auto-approves; set
  `DAISUGI_APPROVE=always` (or an allowlist) to opt into live execution. Dry-run is
  unchanged.
- **Signed pathway bundles** (`bundle_to_pathway`) now require a `trusted_pubkey_b64s`
  anchor — a signed bundle with `None` is rejected (it was verified against its own
  embedded key). Unsigned dev round-trips (`require_signed=False`) are unchanged.
- **Delegation contracts**: an unsigned contract is denied when `trusted_signers` is
  supplied.
- **Strict mode / physical stakes** are stricter: unknown custom `@step_type`s and
  robotics invariants declared without their backing bounds now fail closed.

A whole-codebase adversarial review (7 parallel review agents + independent
verification of every finding) closed a set of fail-opens and exploitable holes.
All findings were reproduced before fixing and are covered by regression tests.

**CRITICAL (the verification core — its whole job is to not fail open):**
- `envelope_subsumes` encoded ONLY shell + invariants, so `verify_delegation`
  approved a skill contract permitting `file_write=['/etc/**']`, `network=['evil.com']`,
  or dangerous MCP tools under a caller that allowed none of them. Now file/network/MCP
  scope is subsumed too (Z3 witness search for globs, host set-subset), fail-closed.
- `sh -ec "curl evil"` / `bash -lc` / `-euxc` / `-cSCRIPT` clustered shell flags
  escaped the `-c` interpreter-payload check (matched by exact token equality), so the
  embedded command bypassed the allowlist. Clustered flags are now parsed.

**HIGH:**
- `NetworkStep(url='file:///etc/passwd')` passed verify and read a local file (urllib
  honors file://) — schemes are now restricted to http(s) in verify and the executor.
- Symlink path escape: FileReadExecutor had no symlink guard and FileWriteExecutor's
  realpath check was circular. Both now re-check the *resolved* target against the
  permitted globs.
- Outer soft deny-rules (unsupported-regex/LLMCheck) were dropped under negation
  (pinned False → `Not(False)=True`) — now fail-closed.
- `verify_inheritance` (the sole tightening proof at envelope generation) ignored
  robotics/MCP/stakes — a child could relax velocity 10×, expand its workspace, add
  MCP tools, or downgrade physical→low. Now checked.
- Orchestrator pathway reuse ran under the pathway's own (broader) envelope, ignoring
  the caller's stated boundary — now verified against the caller's envelope.
- `strict_budget` overrun discarded the completed step, dropped its spend, and let
  synthesis keep spending — now counts the spend, keeps the result, stops cleanly.
- `daisugi install` silently reset a malformed `settings.json`/Hermes config to `{}`,
  wiping the user's permission rules — now skip-and-warn.
- The git-registry trust anchor was pulled from the same remote it authenticated
  (circular) — now a local, out-of-band anchor.
- Swarm deconfliction accepted a negative margin (masking overlap) and malformed
  (inverted/non-finite) AABBs — now fail-closed. `claude -p` argv is injection-safe
  (`--model=` + `--` separator).

**MEDIUM:** MCP `run_plan` live execution now requires the operator's approval opt-in
(confused-deputy bypass); an unsigned contract is denied when `trusted_signers` is
supplied; a signed pathway bundle with no trust anchor is rejected (was verified
against its own key); `git registry init` rejects `ext::`/`fd::` RCE URLs; unknown
custom `@step_type` and unbacked robotics invariants fail closed under strict; duplicate
step ids and execution-order integrity are fixed; recomputed fallback steps are
re-verified; the metered token count includes cache tokens and rejects `is_error`
turns; async `claude -p` subprocesses are reaped on timeout; gardener merge only
compares same-embedding-space pathways; a distilled `plan_template` is verified against
its envelope before storing.

KNOWN / deferred (lower severity, need care): subprocess/network output DoS bounds
(EB-3/EB-4), the envelope-improvement pass's breadth gate (M7), llm_check `errored`
telemetry on claude-code (M9), and a few LOW items are tracked but not in this release.

## v0.33.2 — 2026-07-01 — exact cost on the Claude Code subscription; cost is opt-in

The budget's dollar figure is now **exact**, not estimated, on the `claude-code`
backend — and cost display is opt-in.

- **Exact cost with no API key.** `claude -p --output-format json` returns Claude
  Code's own `total_cost_usd` + token `usage`. The delegating executor now uses that
  metered call for prose (TaskStep) execution, so `BudgetTracker` records the *real*
  cost and token count. Confirmed live: a run reported `measured_cost_usd=$0.0238`
  where the old heuristic estimate said `$0.0011` — ~20× off, which is exactly why
  the estimate wasn't trustworthy. `BudgetReport.measured_cost_usd` is the exact
  figure (None when no backend reported one → falls back to `approx_cost_usd`).
- **Cost is opt-in.** `daisugi orchestrate` no longer prints a dollar figure by
  default; pass `--cost` to see it (labeled *exact* on claude-code, *estimated* on
  litellm). The token/routing summary still shows.
- Honest caveat: each `claude -p` subprocess reloads Claude Code's full system prompt
  (cache-creation tokens), so per-call cost is dominated by that overhead (~2¢/call
  even for a one-line answer). The numbers are exact; the *subprocess* backend is just
  expensive per step. The Claude Agent SDK (which can reuse context) is the amortizing
  path — and it also runs on a Claude Code subscription.

## v0.33.1 — 2026-07-01 — orchestrate CLI fixes + approximate cost

Fixes from feedback after v0.33.0 shipped.

- **`daisugi orchestrate` was unusable without an API key.** It lacked the `--llm`
  flag every other LLM command has, so it could only use the litellm backend and
  died without `ANTHROPIC_API_KEY`. Added `--llm litellm|claude-code` (sets
  `OPENDAISUGI_LLM_BACKEND`); the CLI now runs end to end on `claude -p` — verified:
  a real answer with correct per-step routing. Also fixed the `Optional` typing on
  `--budget`/`--envelope` and added a `__main__` guard so `python -m opendaisugi.cli`
  works (was a silent no-op).
- **Budget reframed as approximate.** The token counts are heuristic (and the
  `claude -p` backend reports no usage at all), so the budget is a routing signal +
  a ballpark, not a meter. Added `BudgetTracker.approx_cost_usd()` /
  `BudgetReport.approx_cost_usd` from a blended `$/Mtok` price table (overridable);
  the CLI prints `≈ $X (approximate)`.
- Test isolation: an in-process CLI test with `--llm claude-code` leaked
  `OPENDAISUGI_LLM_BACKEND` into later tests (fired real `claude -p` calls, 7
  failures + 8× slower suite); an autouse conftest fixture now snapshots/restores it.

## v0.33.0 — 2026-07-01 — Verified swarm tasking (airspace deconfliction)

`opendaisugi.swarm`: the deferred multi-robot deconfliction primitive, and the
"tasking envelopes between swarms" story. openDaisugi already proves *containment*
(`envelope_subsumes` — a delegated scope fits inside its parent, fail-closed). Swarms
need the other half — *disjointness*: a proof no two robots were handed overlapping
airspace. That's the one new primitive (an AABB separating-axis test, distinct from
subsumption).

- **`verify_swarm_tasking(total, assignments, margin=…)`** — one call certifies a
  fleet: every drone's envelope is subsumed by the coordinator (delegation) AND every
  pair of `workspace_bounds` is disjoint by at least `margin` (deconfliction). Returns
  a `SwarmVerdict` with the concrete overlap region for any conflict; fail-closed on a
  drone that declares no bounds.
- **`partition_and_assign(total, drone_ids, axis=, margin=)`** — split a coordinator's
  operational volume into disjoint sectors (with a separation gap), one envelope per
  drone, deconflicted by construction and still subsumed.
- **`aabb_disjoint` / `aabb_intersection` / `partition_airspace`** — the analytic
  geometry underneath.
- Composes into **swarm-of-swarms**: delegation is vertical (nested `envelope_subsumes`),
  deconfliction is horizontal (sibling disjointness). Runnable, physics-free demo in
  `examples/swarm-tasking/` — leads with the rejections (over-broad delegation,
  overlapping sectors, out-of-sector waypoints, comms-loss reassignment gated on proof).

HONEST SCOPE (see the example README): this is plan/volume-level, **analytic geometry
(not Z3-backed), 3D space not 4D spacetime, and not a flight-safety certificate** —
waypoint-in-box ≠ path-in-box, disjoint boxes ≠ collision-free (set `margin ≥
vehicle-radius + position-uncertainty`). Complementary to certified geofencing (NASA
PolyCARP), tactical avoidance (DAIDALUS/ORCA), and operational deconfliction (ASTM
F3548-21), not a replacement. Informed by an online research pass; the differentiator
is the *composition* (subsumption + disjointness + plan-gating as one authored artifact
that gates LLM-authored plans), not the geometry.

Also in this release (from live-testing v0.32 on our own setup via the `claude -p`
backend): run TaskSteps in a neutral CWD so project context can't contaminate the LLM
call; honor `json_mode` on the claude-code backend (prose TaskSteps); a generous
per-step timeout for LLM steps; and ground the decomposer in the real skill/MCP
inventory so it can't emit a step for a capability that has no executor.

## v0.32.0 — 2026-07-01 — Orchestrator: run a prompt end to end, safely and budgeted

The forward-looking counterpart to the backward-looking Gardener. `tend()` distills
past traces into pathways; the **Orchestrator** turns one prompt into a verified
plan into a final answer, routing each step to the cheapest capable model under a
live token budget. Reuses the existing runtime-assurance spine — the plan is
verified before it runs and each step is re-verified at execution time.

Pipeline: `prompt → Tier-0 pathway reuse → decompose → size → supervised execute → synthesize`.

- **Decomposer** (`decompose`) — an LLM authors a typed-step DAG; `check_dag` proves
  structural validity always, and when an envelope is supplied `verify()` gates the
  whole plan so an out-of-policy decomposition raises rather than executing.
- **Per-step model sizer** (`model_sizer`) — turns routing's per-*task* difficulty
  seed into a per-*step* heuristic (step-type floor + prompt-text difficulty for
  reasoning steps + dependency fan-in) connected to a configurable cheap→strong
  `ModelLadder`. Picks the cheapest *capable* rung, then downgrades under budget.
- **Live budget** (`BudgetTracker`, `BudgetAwareDelegatingExecutor`) — token
  accounting that gates routing **during** the run: each step re-sizes against the
  tracker's current remaining budget and records actual `usage`, unlike
  `accounting.tier_stats` which only reports spend after the fact.
- **Synthesizer** (`synthesize`) — collects step outputs into the final answer via
  an LLM, always falling back to a deterministic concatenation (budget spent, no
  client, or call failure) so orchestration never errors at the finish line.
- **New step types + executors** — `TaskStep` (pure-reasoning leaf), `SkillStep`
  (delegation proved via `envelope_subsumes` on an optional `contract_envelope`),
  `MCPStep` (gated by a new deny-by-default `Permission.mcp_allowlist`). `verify()`
  gives each a real checkable surface — they cannot verify vacuously. `SkillExecutor`
  and a pluggable-transport `MCPExecutor` execute them.
- **Surface** — `Daisugi.orchestrate(prompt, budget_tokens=…, …)` and the
  `opendaisugi.Orchestrator` composition root. Every LLM stage takes an injectable
  client so the pipeline is fully unit-testable without a live model.

Judgment calls made under autonomy (revisit): TaskStep outputs are consumed only by
the synthesizer — openDaisugi never splices a step's output into a downstream command
string, which removes the prompt-injection→privileged-execution path by construction;
MCP execution ships as step-type + executor + transport protocol with live wiring
deferred; Tier-0 reuse uses the raw distilled template (LLM plan-adaptation deferred);
task steps run their subtask independently (per-step upstream-output threading deferred).

NOTE: the whole pipeline is unit-verified with injected/fake LLM clients and the shell
path runs for real, but a live end-to-end run against a real model was NOT performed in
this build (same environment boundary as the v0.31.1 llamafile download). The real-model
path (TaskStep prose prompting, non-JSON completion, local-rung routing) is
correct-by-construction and unit-tested, not yet exercised against a live API.

## v0.31.1 — 2026-06-24 — Trustable, configurable model resolution

`opendaisugi.model_registry` + `daisugi models`: resolve an open model from the
Hugging Face Hub the trustworthy way instead of a hardcoded, stale model-id table.

- **Trusted-org allowlist** (`DEFAULT_TRUSTED_ORGS`, overridable) — an untrusted
  `org/repo` raises `UntrustedSource` before any fetch.
- **List, never guess** — the filename comes from `list_repo_files`, so an
  automated fetch can't 404 on a hallucinated path (the exact bug `daisugi setup`'s
  first llamafile attempt hit).
- **Commit-pinned** — the resolved `ModelRef` carries an immutable `revision`
  (the repo's current SHA unless supplied), so a download is reproducible.
- **Opt-in download** — `download_pinned` refuses without `allow_download=True`.
- `daisugi models [repo] [--pull] [--json]` discovers/resolves/downloads; `setup`
  guidance now points at the canonical `mozilla-ai` sources (research-corrected).

Built from the local-model research (canonical repo is `mozilla-ai/llamafile`;
model llamafiles live under the HF `mozilla-ai` org). NOTE: the Hub client is
injectable and the resolver is fully unit-tested, but a live end-to-end download
was not run in this build (same environment boundary as v0.30).

## v0.31.0 — 2026-06-24 — Safe local-model subagents + fail-closed robot subsumption

Two deployment-facing capabilities plus a quickstart, built TDD with an advisor
review that caught and fixed the litellm-prefix and labeling risks pre-merge.

- **`SafeSubagent`** (`opendaisugi.subagent`) — a subagent confined to a verified,
  delegated scope. `SafeSubagent.create(parent_envelope=, contract=, tier1=)`
  refuses with `DelegationDenied` unless the subagent's contract is subsumed by the
  parent (you can't mint more authority than the parent holds); `verify`/`run`
  check every plan against the subsumed envelope, dry-run by default. A local
  Tier-1 (cheap/free tokens) is the subagent's configured brain. Runnable example
  in `examples/safe-local-subagent/`. PLAN-LEVEL runtime assurance, not an OS sandbox.
- **Fail-closed robot-capability subsumption** — `envelope_subsumes` /
  `verify_delegation` now compare declared robot capabilities (workspace_bounds,
  velocity/torque limits, joint_limits, obstacles). When the outer declares a bound
  the inner exceeds *or leaves undeclared*, subsumption FAILS — undeclared = denied.
  Closes the v0.28.6-review hole where a 90×-reach robot was "subsumed" into a 0.1m
  envelope because robot fields were ignored.

  > **SCOPE / SAFETY — read this.** This is **plan-level verification** of *declared*
  > capabilities only. It is **NOT a robot safety system, NOT a fleet controller**,
  > does not model executed trajectories or reachability, and execution remains
  > MuJoCo-sim. It must **not** be read as making LLM-driven robots safe to deploy on
  > real hardware. Multi-robot fleet **deconfliction is deliberately NOT included**
  > (deferred) — do not infer cross-robot collision safety from this release.

- **`daisugi quickstart`** — guided one-stop orientation (no token spend): your
  hardware → a recommended local model → discovered-transcript counts → the exact
  command sequence (setup → onboard → status → route).
- **refactor:** shared `executor.dry_run_executor_map(plan)` used by both
  `SafeSubagent.run` and the MCP `run_plan` (was hand-rolled in both).

## v0.30.0 — 2026-06-24 — Hardware-aware local model setup (`daisugi setup`)

Completes the turnkey coworker path: detect the box, recommend a size-appropriate
local model, qualify it on that box, and wire it as a cheap Tier-1 so `onboard`
distills on the local model instead of paying full Tier-2 cost.

- **`opendaisugi.hardware`** — `detect_hardware()` (RAM/VRAM/GPU/arch/Apple-unified,
  best-effort, never raises) + `recommend_model()` mapping a memory budget to a
  size class + `Q4_K_M` + the **llamafile** runtime. The recommendation is
  **provisional**: it names candidate families (Qwen/Gemma/Llama/Phi) as examples,
  not a verified winner (the family pick is unverified per the local-model research).
- **`opendaisugi.local_setup`** — `qualify_local_model()` runs a probe battery
  through the **real** provider path (instructor Mode.JSON against the local
  endpoint) and promotes a model only if its valid-envelope pass rate clears the
  threshold — the per-box honesty gate. `write_tier1_config`/`load_configured_tier1`
  persist the qualified choice.
- **`daisugi setup`** — hardware → recommendation + llamafile commands; with
  `--endpoint`/`--model` runs the gate, and `--wire` persists it only if it passes.
- **`onboard`/`tend`** now defer bulk envelope generation to the wired local
  Tier-1 (`ingest_episodes(tier1=…)`); **`status`** shows the hardware budget and
  whether a local model is wired.

**Validation boundary (honest):** detection is validated against a real 16GB
CPU box, the instructor mode is confirmed `JSON` (works against local `/v1`,
not tool-calling), and the gate logic is tested against a realistically flaky
provider. The end-to-end qualification was **not** run against a live local
model in this build — the build box has no C++ compiler (so `llama-cpp-python`
can't build) and no prebuilt runtime. The gate exercises the real path *by
construction*; it has not yet been exercised against a real GGUF in CI.

## v0.29.1 — 2026-06-23 — Route advice: harness-aware advisor-tool gating

`daisugi route` no longer dangles an Anthropic-only suggestion in front of
cross-harness users. The advisor-tool pairing (Anthropic beta
`advisor-tool-2026-03-01`) is recommended only on Claude harnesses; on Codex,
Ollama/llamafile/local, Hermes, and OpenClaw it stays silent and a hard novel
task just routes to the frontier.

- `RouteAdvisor(advisor_tool_available=True)` gates the Tier-2 advisor-pairing
  suggestion; `advisor_tool_available_for_harness(harness)` returns True only for
  `claude-code` / `claude` / `anthropic` (unknown harnesses fail safe to False).
- `daisugi route --harness <name>` (default `claude-code`) wires it through.

## v0.29.0 — 2026-06-23 — Day-one onboarding: bulk distillation, routing, trust surface

Turns "I just installed this" into "token-saving routing + verified actions
today" for an adopter who already has months of conversations.

- **`daisugi onboard`** — one command that discovers existing harness transcripts
  (Claude Code `~/.claude/projects`, Codex `~/.codex/sessions`, overridable via
  `OPENDAISUGI_TRANSCRIPT_ROOTS`), replays each into the verified journal, and
  distills reusable pathways. Flags: `--dry-run`, `--limit`, `--harness`,
  `--threshold`, `--min-traces`, `--lookback-days` (default: all history),
  `--llm claude-code`, `--json`. Unsupported harnesses are surfaced, never fatal.
- **Pathway threshold calibrated 0.85 → 0.55 and plumbed through the public API.**
  The old default could not cluster or retrieve anything but near-verbatim task
  restatements (on all-MiniLM-L6-v2 paraphrases score ~0.5 mean, different tasks
  ~0.29) — the token-savings "value-killer." `DEFAULT_PATHWAY_THRESHOLD` is now
  configurable via `Daisugi(pathway_threshold=)`, `find_pathway(threshold=)`, and
  `generate_envelope(pathway_threshold=)`.
- **`daisugi route "<task>"`** + `RouteAdvisor` — recommends the cheapest viable
  tier: Tier-0 reuse for a repeat task that matches a verified pathway (~free,
  re-verified), a cheap model for easy novel tasks, the frontier for hard ones —
  and flags where Anthropic's advisor-tool pairing (beta `advisor-tool-2026-03-01`)
  is the better spend. Complementary to the advisor tool, whose differentiator we
  exceed via verified, reusable cross-request memory.
- **`daisugi status`** — day-one readiness surface: whether token savings are LIVE
  (`[search]` installed AND pathways exist) and whether the verified journal is
  populated. Makes the silent `[search]`-missing fallthrough loud.

## v0.28.6 — 2026-06-15 — REVIEW_FINDINGS backlog clear (L1, M7)

Closes the two remaining REVIEW_FINDINGS items beyond the C/H/M sweep:

### L1 — vacuity check now memoized
`check_vacuity` is called per-invariant per-`verify()` (3-5 Z3 SAT calls
per envelope on typical input). Pre-fix it instantiated two Z3 solvers
every time. v0.28.6 wraps it in a bounded LRU keyed on
`(expr.model_dump_json(), timeout_ms)` so structurally-identical
predicates hit a dict lookup instead of a SAT pair. Cap is 512 entries
so a hostile caller cannot blow process memory by churning unique exprs.
Timeout is part of the key — a longer budget can flip `non_trivial`
(Z3 unknown) into a real verdict, so sharing a slot across budgets would
be unsound. Test-only `clear_vacuity_cache()` helper exported.

### M7 — install wizard warns on JSON5 comment loss
OpenClaw's `openclaw.json` is JSON5 but the install wizard writes plain
JSON, so comments disappear on the first `daisugi install` rewrite. The
backup file preserves them, but the docs read as if comments survive
structurally. v0.28.6 surfaces a `UserWarning` at the exact moment of
clobber, pointing at the backup. No comment-preserving round-trip
(would require the `json5` package as a dep); honest visibility instead.

Suite: 1330 pass (1325 + 5 new regression tests), 10 skip, 0 fail.

This closes every documented item in `REVIEW_FINDINGS.md` — C1-C2, H1-H5,
M1-M7, L1. L2 (path-traversal-via-AGENT_DIR) was already covered by
v0.28.1's red-team hardening pass.

## v0.28.5 — 2026-06-15 — Dev-extras: `[search]` + `[mcp]` + `[sign]` in `[dev]`

Tiny dev-experience release. `pip install -e .[dev]` now pulls
`sentence-transformers`, `mcp`, and `cryptography` so the full
regression suite runs out of the box. Pre-fix, seven distiller /
pathway-store tests failed at collection with `ModuleNotFoundError:
sentence_transformers` — they covered real behavior but couldn't run
without the operator separately installing `[search]`.

Heavy extras (`[lora]`, `[robotics]`) stay opt-in — they're hundreds of
MB and most contributors don't touch them.

No code changes; pure dependency reshuffle.

## v0.28.4 — 2026-06-15 — Medium-severity findings + H2 closed

Sixth and final pass through the Fable 5 review. **H2 (`regex_to_z3` ASCII
alphabet) IS unsound**, contra the v0.28.3 deferral note. Adversarial
re-review of PR #4 surfaced a concrete construction (`outer=[ -~]+`,
`inner=.+`) where Z3 returned `unsat` ("subsumed") while reality had `é`
as a counterexample. The pre-fix `Range(0x20, 0x7e)` alphabet shrank
both sides of the subsumption check symmetrically, hiding the non-ASCII
gap. v0.28.4 widens `_any_char()` and `_complement_char()` to the Basic
Multilingual Plane (`0x00-0xFFFF`) minus newline — matching Python's
default `.` semantics. Regression tests in `tests/test_regex_to_z3.py`.

### Pathway / gardener
- **M1 — gardener never pruned failure-only pathways.** `mark_failure`
  now stamps `last_activation_at` (matches `increment_hit`); pruner
  treats `last_activation_at == 0.0` as a fall-through to `distilled_at`
  rather than as "skip stale check." Pre-v0.28.4, a freshly distilled
  pathway that turned out wrong sat in the store indefinitely.
- **M2 — A/B postcondition drift detection was shallow.**
  `_postconditions_equivalent` now compares the full Postcondition
  shape (sorted JSON of `model_dump()`), not just `(type, expected)`.
  Pre-fix, two `file_size_range` postconditions with different
  `min`/`max` bounds were treated as equivalent, defeating the
  gardener's drift-detection pitch. Also: `tier2_tokens` placeholder
  flipped from a fabricated `4500` to `0` with a docstring telling
  downstream not to trust it as telemetry.
- **M3 — `PathwayStore.find` mixed embedding spaces.** Pre-v0.28.4,
  `find()` cosine-sim'd query vectors against every stored row even
  when rows came from a different `(embedding_model,
  embedding_model_version)` than the current distiller — silently
  surfacing semantically wrong matches when the model was upgraded.
  Now filters incompatible rows, warns at ≥ 10% staleness, and admits
  pre-provenance-tracking legacy rows (empty model + version) as
  wildcards so existing stores keep working.

### Defense in depth
- **M4 — `AllowlistBypassStrategy` blind to metachars.** Pre-v0.28.4
  the approval strategy auto-approved any shell step whose first token
  was in the allowlist, regardless of redirects / pipes / command
  substitution. Combined with the v0.28.2 metachar work in verify,
  the approval layer added zero defense-in-depth value. v0.28.4 runs
  `verify._SHELL_METACHAR_RE` first; metachar-laden commands fall
  through to the inner strategy.

### Robustness
- **M5 — `Daisugi.run` propagated `tend()` exceptions.** When
  `tend_after=N` triggered, a `tend()` raise (LLM call, embedder
  unavailable, sqlite locked) made a successful supervised run appear
  to fail at the caller's `await`, despite the run being fully
  journaled. `tend()` failure is now caught, logged at warning, and
  swallowed. The supervised run is the contract; auto-distillation is
  best-effort.

### Documentation
- **M6 — `pytest_passes` system alias was Stage-1-broken.** The
  alias body references `metadata.output`, which is unset at Stage 1
  verify time → predicate evaluates False → loud Violation on every
  plan using it as an `Invariant.expr`. The expr itself is correct
  *as a Postcondition*. Description rewritten to encode the
  authoring contract ("POSTCONDITION-ONLY: …"); same caveat applies
  to any future alias that consults runtime-populated paths.

### H2 — soundness fix in detail
- **Pre-fix construction.** `inner = re.compile(".+")`, `outer =
  re.compile("[ -~]+")`. Reality: `re.search(".+", "é")` matches;
  `re.search("[ -~]+", "é")` doesn't. So `inner ⊄ outer`. Pre-fix Z3
  translation: both sides shrunk to `Range(0x20, 0x7e)+` (the alphabet
  itself), `inner_Z3 == outer_Z3`, Z3 returned `unsat`. Subsumption
  approved a delegation it should have rejected.
- **Why the v0.28.3 deferral was wrong.** I built the wrong empirical
  test then — used `re.fullmatch` (anchored) instead of `re.search`
  (which the translator emulates with `Star(_any_char())` padding).
  The padded comparison was where the unsoundness actually surfaced.
  The PR #4 reviewer's catch was correct.
- **Why BMP, not full Unicode.** Z3's string sort is BMP-bounded —
  `Range(chr(0), chr(0x10FFFF))` silently returns `unsat` for every
  membership check (Z3 treats the supplementary plane as outside its
  string sort). BMP (`0x00-0xFFFF`) covers Latin-extended, CJK, IPA,
  math symbols — every common authoring char a Python regex would
  match on. Supplementary-plane codepoints remain a known soundness
  gap, documented in `docs/limitations.md`.

## v0.28.3 — 2026-06-15 — High-severity findings from the Fable 5 review

Follow-up to v0.28.2. Closes four of the five High-severity findings from
the adversarial review (`REVIEW_FINDINGS.md`); H2 (regex_to_z3 alphabet
vs concrete `re.search` divergence) deferred pending a concrete failing
case for the unsoundness direction.

### Soundness / correctness
- **Subsumption `$(` substring gap (H4-residual).** Pre-v0.28.3,
  `subsumption._encode_shell_admission` was missing the `$(`
  command-substitution substring that `verify._SHELL_METACHAR_RE` has
  always caught. The two gates MUST stay in sync; without this,
  subsumption could prove "outer ⊨ inner" for shell command shapes the
  concrete verifier rejects — unsound delegation. v0.28.3 adds `$(`
  alongside the single-char metachars added in v0.28.2.
- **`_substitute_params` order-dependent (H5).** Alias parameter
  substitution iterated `args.items()` in dict order, so
  `{"principal": "alice", "principal_name": "bob"}` substituted into
  `"$principal_name"` could produce `"alice_name"` (the shorter
  `$principal` prefix wins under the wrong iteration order). v0.28.3
  sorts arg keys by descending length so longer placeholders always
  match first. Regression test in `tests/test_aliases.py`.

### Facade plumbing
- **`strict=` now reachable through `Daisugi.run` / `Daisugi(...)` /
  `Supervisor` (H1).** Pre-v0.28.3, `Supervisor.run` called
  `verify(plan, envelope, z3_timeout_ms=..., aliases=...)` with no
  `strict=` argument and `Daisugi.run` exposed no override either. So
  low/medium-stakes envelopes could not be opted into strict mode
  through the facade, contradicting the README claim that strict is
  "default-on at stakes high/physical" — defaults worked, overrides
  didn't. v0.28.3 adds `strict: bool | None = None` to `Daisugi.__init__`,
  `Daisugi.run`, `Daisugi.verify` (follow-up patch — the first cut of
  v0.28.3 missed `.verify()`, leaving the constructor's `strict` silently
  ignored on the verify path), and `Supervisor.__init__`. Threaded through
  to both `verify()` and `stage2.verify_completed_step` via the supervisor's
  bound `self._strict`. Precedence: method kwarg > constructor kwarg >
  stake-based default.

### Stage 2 handlers
- **Opaque postcondition handlers (H3).** The envelope-generation
  few-shot prompt teaches the LLM to author
  `{"type":"exit_code","expected":0}`,
  `{"type":"file_exists","path":...}`, and
  `{"type":"file_size_range","path":...,"min":...,"max":...}`. Pre-v0.28.3
  **no code evaluated these** — at strict mode they raised "no
  verifiable expr"; at non-strict mode (the default for the README's
  headline data / file / network use cases) they silently passed.
  v0.28.3 adds `stage2._OPAQUE_POSTCONDITION_HANDLERS` for these three
  types. `supervisor` now copies `outcome.rc` into completed step
  metadata so `exit_code` can be evaluated. Stage 1 strict mode
  recognizes these types as discharged-at-stage2 via the new
  `RECOGNIZED_STAGE2_POSTCONDITION_TYPES` registry — Stage 1 must not
  reject them as opaque. Single source of truth: stage2 and
  `_invariant_types` are asserted in sync at import.

### Adversarial self-review follow-ups (same release)
- **Executor-side rc now wins over upstream metadata.** First cut of
  H3 used `setdefault("rc", ...)`, which let any pre-existing
  `metadata.rc` (planted by a parser, pathway, or attacker-controlled
  envelope) survive — an exit_code postcondition could be discharged
  against a forged value. Switched to direct assignment so the
  executor's real rc always wins.
- **`file_size_range` rejects bound-less postconditions.** First cut
  defaulted to `(0, inf)` which silently passed every size — strictly
  looser than the pre-v0.28.3 strict-mode rejection. Now requires at
  least one of `min`/`max`.
- **`Daisugi.verify()` honors constructor `strict=`.** First cut threaded
  strict through `Daisugi.run` but not `Daisugi.verify` — same facade,
  inconsistent precedence. Now both honor `self._strict` and the method
  kwarg wins for both.

### Deferred
- **H2 (regex_to_z3 alphabet).** Review claimed the printable-ASCII
  alphabet (`Range(0x20, 0x7e)`) used for `.` and negated classes makes
  Z3 subsumption diverge from concrete `re.search` on `\n` / `\t` /
  non-ASCII. The asymmetry I could construct from the existing code
  appears to favor false-rejection (incomplete) rather than false-approval
  (unsound) — the unsound direction depends on soft-node fallback
  semantics already documented in `docs/limitations.md`. Punted to a
  separate investigation; tracked under "Known remaining drift" in the
  v0.28.2 CHANGELOG entry.

## v0.28.2 — 2026-06-15 — Metachar gate + MCP default safety (security hotfix)

### Security (critical)
- **`verify._SHELL_METACHAR_RE` now rejects `<`, `>`, `\n`, `\r`.** Prior to
  this release the gate only matched `;`, `|`, `&`, `` ` ``, `$(` — so a
  `cat`-only `shell_allowlist` admitted `cat > /etc/passwd_hacked`,
  `cat < /etc/shadow`, and newline-separated multi-command strings.
  `SubprocessExecutor` runs with `shell=True`, so admitted commands were
  evaluated as written. `subsumption._encode_shell_admission` updated to
  the same list — the two MUST stay in sync. Regression test:
  `tests/test_verify.py::test_redirect_and_newline_metachars_rejected`.
- **MCP `run_plan` defaults to `dry_run=True`.** The v0.20-v0.28.1 docstring
  claimed the MCP tool "uses dry-run executors, which is safe." The code
  actually invoked `default_executors()` (`SubprocessExecutor`,
  `FileWriteExecutor`, `NetworkExecutor`) with approval hardcoded to
  auto-true. v0.28.2 routes every step kind through `DryRunExecutor` when
  `dry_run` is omitted; callers wanting live execution must explicitly
  pass `dry_run=False`. Regression test:
  `tests/test_mcp_server.py::test_run_plan_dry_run_default_does_not_touch_disk`.

### Documentation accuracy
- `docs/feature-status.md`: distributed pathway registry corrected from
  "Planned" to "v0.25.0 / Working" (`GitPathwayStore`, `PathwayBundle`,
  `daisugi registry` CLI); contract signing corrected from "Planned (v0.12)"
  to "v0.15.0 / Working"; LoRA row notes that the trainer (`python -m
  opendaisugi.lora.train`) shipped in v0.10.0.
- `docs/limitations.md`: distributed pathway registry removed from the
  "Planned, not shipped" list.
- `docs/security-model.md`: license corrected from "AGPL-3.0-or-later"
  to "MIT" to match `LICENSE` and `pyproject.toml`.
- `README.md` "Maturity legend" and "Future" sections updated to match.
  v0.28.2 patch: `LengthRange` (shipped v0.15.0) removed from the
  "Planned" list — it was a doc bug. Maturity legend cited only
  arithmetic-over-paths as remaining planned-algebra work.

### Test parity for the `dry_run` flag
Adjacent existing MCP tests (`test_run_plan_executes_and_returns_receipts`,
`test_receipts_for_run_returns_real_receipts_after_run_plan`,
`test_recent_runs_returns_journaled_runs`) explicitly pass
`dry_run=False` so live-execution coverage isn't lost to the new
safe-by-default. A new test
(`test_run_plan_dry_run_false_does_touch_disk`) asserts the reverse
direction — `dry_run=False` actually writes — so a future refactor
that wires DryRun unconditionally would be caught.

### Known remaining drift (deferred)
Several "High" findings from the Fable 5 review still stand and are
tracked separately: `Daisugi.run` / `Supervisor` not threading `strict=`
to `verify`; printable-ASCII alphabet divergence between `regex_to_z3`
and concrete `re.search`; subsumption metachar list still missing the
`$(` substring (verify has it; symmetry preserves soundness but the
divergence ships with v0.28.2 by design — fixed in v0.28.3); unevaluated
`exit_code` / `file_exists` / `file_size_range` postconditions;
`_substitute_params` order-dependence.
See `REVIEW_FINDINGS.md` (committed on `claude/review-openDaisugi-architecture-thXGo`).

## v0.28.1 — 2026-06-08 — Install/hook polish (red-team + dogfooding)

### Performance
- **Capture hook 5.5s → 0.6s per tool call** — `import opendaisugi` eagerly loaded
  litellm/instructor (~2.4s); deferred to their call sites. The PreToolUse hook
  fires on every tool call, so this is the headline fix.

### Security / robustness
- Sanitize host-supplied `session_id` before using it as a capture filename
  (path traversal); enforce `0o700`/`0o600` on the captures dir/files.
- OpenClaw plugin refuses to write through a pre-planted symlink (arbitrary write).
- String-aware JSON5 comment stripping (a `//` inside a URL no longer truncates it);
  `_unpatch_instructions` requires a marker pair (no single-marker truncation).
- Backups gated behind real changes (no `.bak` spray on idempotent re-runs);
  collision-proof `_backup` (nanosecond + counter).

### Distillation (dogfooding our own logs)
- Parser caps episode task length and cleans injected boilerplate (skill bodies →
  `skill: <name>`, continuation banners, system-reminders) so bulk-distilling old
  transcripts produces clean pathway labels instead of garbage.

### Cleanup
- One `_link_skill` primitive (was inlined 3×); unify Claude+OpenClaw MCP writers
  behind `_patch_mcp`; symlink-safe removal; drop dead code; single-pass scans.
- Version-robust stderr assertions in the `--llm` flag tests.

Known deferred: uninstalling one harness removes the shared `~/.agents/skills`
skill from under other still-installed harnesses (ref-count fix later).

## v0.28.0 — 2026-06-07 — Universal cross-harness install

### Added
- **`daisugi install` now wires four harnesses** — Claude Code, Codex, Hermes,
  and OpenClaw — from one bundled source-of-truth skill directory. Three
  layers per harness: skill (symlinked into the cross-vendor `~/.agents/skills`
  path, discovered on demand), MCP server registration, and a pre-tool-call
  capture hook.
- **`daisugi install --uninstall`** reverses every managed change; `--runtime`
  targets a single harness.
- **`daisugi hook record --format <host>`** emits each host's allow contract
  (`{"continue": true}` for Claude, `{}` for Hermes/OpenClaw).
- **OpenClaw `before_tool_call` plugin** shipped and installed — the
  runtime-assurance enforcement seam.

### Changed
- **SessionStart hook removed** and auto-migrated away on upgrade. The skill is
  discovered on demand instead, so simple sessions pay zero extra tokens.
- The bundled skill is now one real package directory
  (`src/opendaisugi/skills/opendaisugi-checklist/`) with its `references/`
  shipped in the wheel.

### Fixed
- **Hermes config target** was `cli-config.yaml`, which Hermes never reads — the
  capture hook silently never fired. Now writes `~/.hermes/config.yaml`.
- Skill `references/*.md` were not bundled in the wheel, so installed skills had
  dangling reference links.
- `daisugi install` CLI no longer crashes rendering the plan (used the removed
  `PlannedAction.file`; now `InstallStep.target`).

## v0.27.1 — 2026-06-06 — Pathway UX: Daisugi.run(), tend_after, louder [search] warning

### Added
- **`Daisugi.run(plan, envelope)`** — convenience wrapper over `Supervisor.run`
  that keeps the facade as the single object callers need. Tracks successes
  for auto-tend.
- **`Daisugi(tend_after=N)`** — after every N successful `run()` calls,
  `tend()` fires automatically so the pathway store stays warm without manual
  scheduling. The counter resets after each trigger; failed/rejected runs do
  not count.

### Fixed
- **`[search]` missing now emits `UserWarning`** instead of a silent
  `logging.warning`. Without a logging handler configured, users had no
  indication that pathway lookup was disabled and token savings were not
  working. The warning fires at most once per process and includes the install
  command.

### Documentation
- README Install section now calls out `[search]` as required for token
  savings with a clear explanation of why it's optional.
- New "Saving tokens with pathways" README section documents the full
  distillation loop: cold-start behaviour, `tend_after` vs manual scheduling,
  LLM cost of `tend()`, and how adapted plans are re-verified.

---

## v0.27.0 — 2026-06-06 — Verification-core soundness: strict mode, vacuity detection, core-API alias authorship

> **BREAKING / Migration required for high-stakes envelopes.**
>
> At `stakes` `"high"` or `"physical"`, invariants that declare a safety
> property with no verifiable `expr` — outside the four recognized robotics
> types (`end_effector_in_workspace`, `joint_limits_respected`,
> `velocity_bounded`, `no_obstacle_penetration`) — are now **loud
> rejections** instead of silent passes. Previously, an opaque invariant
> (one with `expr=None`) at high stakes would silently continue without
> checking anything, creating safety-theater.
>
> **To opt out per invariant:** set `enforce=False` to keep it as
> documentation-only.
> **To opt out per call:** pass `verify(..., strict=False)`.
> **Contradictory invariants** (predicates that are logically unsatisfiable)
> are now hard errors at **all** stakes levels, not just high/physical.

### Added

- **Strict-mode plumbing** (`verify.py`). `verify()` gains a `strict:
  bool | None = None` parameter. A `resolve_strict(strict, envelope)`
  helper resolves effective strictness: `None` defaults to `True` for
  `stakes` in `{"high", "physical"}`, `False` otherwise. Explicit bool
  overrides envelope stakes.

- **Opaque-invariant rejection under strict mode** (`verify.py`). When
  `strict=True`, any enforced invariant with `expr=None` that is not in
  the `RECOGNIZED_OPAQUE_TYPES` set is rejected with a concrete
  `Violation(stage="predicate", ...)` including a `suggested_remediation`.

- **Authoritative `RECOGNIZED_OPAQUE_TYPES` constant** (`_invariant_types.py`,
  new). Single source of truth for the four robotics invariant types that are
  legitimately expr-less (discharged by dedicated symbolic/numerical handlers
  in `z3_checks.py`). Previously each module duplicated this set.

- **Strict subsumption** (`subsumption.py`). `envelope_subsumes()` gains
  `strict: bool = False`. Under strict mode, opaque inner invariants whose
  types are not in `RECOGNIZED_OPAQUE_TYPES` cause `holds=False` with a
  concrete reason. Non-strict path continues to collect them in
  `unverified_invariants`.

- **Z3 vacuity detection** (`vacuity.py`, new). `check_vacuity(expr)` uses
  Z3 to detect whether a compiled predicate is a **tautology** (constrains
  nothing — safety-theater) or a **contradiction** (always false — DoS-class
  bug where the envelope can never pass). Returns `"tautology"`,
  `"contradiction"`, or `"non_trivial"`.

- **Vacuity wired into alias registration and invariant evaluation** (`aliases.py`,
  `verify.py`). `AliasRegistry.register()` now runs `check_vacuity` and raises
  `VacuousAliasError` for tautological or contradictory aliases. Contradictory
  enforced invariants are hard errors at all stakes; tautological enforced
  invariants are violations under strict mode and warnings otherwise.

- **Alias provenance in RefinementLog** (`aliases.py`, `journal.py`).
  `AliasRegistry` accepts an optional `refinement_sink=` journal. On
  successful `register()`, a provenance record `{alias, vacuity, tier}` is
  written to the journal. Journal write failures are caught and logged at
  WARNING — they do not propagate.

- **`AliasRegistry` parameter on core `verify()`** (`verify.py`). `verify()`
  gains `aliases: AliasRegistry | None = None`. Invariant exprs containing
  alias references are resolved through the registry before evaluation. An
  unresolved alias reference without a registry yields a `Violation` (not a
  silent pass — that would reintroduce the fail-open bug).

- **`Daisugi` facade forwards `strict=`/`aliases=`** (`__init__.py`).
  `Daisugi.verify(plan, envelope, *, strict=None, aliases=None)` forwards
  both parameters to core `verify()`, enabling the full
  register→verify→enforce flow through the public API.

- **`llm_check` fails closed** (`llm_check.py`). Any exception from the
  model invocation (network error, timeout, rate-limit) resolves to
  `satisfied=False, errored=True` — never a silent approve. The soft-node
  consumer in `predicate_z3.py` treats errored/None results as unsatisfied.

- **Actionable remediation hints** on every new strict/vacuity/alias
  `Violation`. Each `detail` dict includes `suggested_remediation` with
  a concrete fix instruction:
  - `opaque_unrecognized` → add `expr` or set `enforce=False`
  - `tautology` → tighten the predicate or remove it
  - `contradiction` → fix the unsatisfiable predicate
  - `unresolved_alias` → register the alias and pass `aliases=` to `verify()`

### Changed

- `z3_checks.check_plan_invariants` docstring updated to clarify that
  unrecognized opaque types are handled by `verify.py` (strict reject), not
  silently ignored here.

- `aliases.py` module docstring updated: vacuity check is shipped in v0.27.0,
  not deferred to v1.0.

### Security

Closes four ways the runtime verifier could fail-open or be used for
safety-theater:

1. **Silent-pass on opaque invariants at high stakes** — now a loud rejection
   unless the invariant uses a recognized robotics type or is marked
   `enforce=False`.
2. **Tautological constraints** — an invariant that is always-true (constrains
   nothing) is now detected and flagged.
3. **Contradictory constraints** — an invariant that is always-false (blocks
   everything) is a DoS-class bug; now a hard error at all stakes.
4. **LLM-check fail-open** — a failed probabilistic check previously could
   silently pass; it now fails closed.

### Hardening (adversarial review pass)

A multi-dimension adversarial review of the first implementation pass found the
soundness work had been applied to invariants but not their parallels. Closed:

- **Postcondition parity** (`verify.py`). The opaque-reject + alias-resolution +
  vacuity pipeline was applied to invariants only; an enforced *postcondition*
  declaring a safety property with no `expr` still silently passed at high
  stakes. Invariants and postconditions now share one `_check_predicate_item`
  helper, so the two paths cannot drift apart.
- **`llm_check` production path** (`predicate_z3.py`). `evaluate_predicate` still
  called the deprecated `call_llm_check`, so the fail-closed `run_llm_check` was
  dead code. Migrated the real call site; a failed check now raises and is
  recorded as a violation.
- **Delegation strictness** (`contracts.py`). `verify_delegation` now threads the
  caller's stakes-resolved strict mode into `envelope_subsumes`, so a high-stakes
  delegator refuses a callee's opaque safety invariants instead of merely
  surfacing them.
- **Dict-form alias vacuity** (`aliases.py`). A raw-dict alias expr bypassed the
  vacuity gate (the parse error was swallowed); dicts are now parsed before the
  check.
- **Single-source dispatch** (`z3_checks.py`). Robotics invariant dispatch now
  iterates a handler dict with an import-time assertion that its keys equal
  `RECOGNIZED_OPAQUE_TYPES`, so adding a recognized type without a handler fails
  loudly.
- **Vacuity edge cases** (`vacuity.py`). `ForallOutputs` is now stripped like the
  other quantifiers (was a silent no-op); domain assumptions are kept off the
  tautology solver so a domain-conditional constraint is not mis-reported as a
  tautology.
- **Non-strict tautology warnings** now reach `VerificationResult.warnings`, not
  just the logger.
- **`verify_step` boundary documented**: per-step verification does not run
  plan-level predicate invariants and therefore does not apply strict mode —
  callers must run whole-plan `verify(..., strict=...)` once up front.

A further verification pass over those fixes found two more gaps, also closed:

- **Carve-out is invariant-only** (`verify.py`). `RECOGNIZED_OPAQUE_TYPES` is
  discharged by `check_plan_invariants`, which iterates invariants only — so the
  carve-out had wrongly extended to postconditions, letting an opaque
  recognized-type *postcondition* pass silently at physical stakes. The carve-out
  now applies when `label == "invariant"` only.
- **Runtime stage-2 gate hardened** (`stage2.py`). `verify_completed_step` — the
  last check before an effect commits externally — silently skipped opaque
  enforced postconditions. It now resolves strict from envelope stakes and rejects
  them, and resolves postcondition `AliasRef`s through an optional registry
  (threaded from `Supervisor`).
- **Supervisor alias threading** (`supervisor.py`). `Supervisor(aliases=...)`
  forwards a registry to the whole-plan `verify()` and the stage-2 gate.
- **Subsumption visibility** (`subsumption.py`). Outer opaque invariants now
  surface in `unverified_invariants` under strict mode (both the SAT path and the
  inner-hard-fail early return), so strict visibility is never worse than
  non-strict.

### Tests

- `tests/test_verify_strict.py` — strict-mode resolution + opaque rejection.
- `tests/test_invariant_types.py` — RECOGNIZED_OPAQUE_TYPES single source of
  truth + robotics carve-out.
- `tests/test_subsumption_strict.py` — strict subsumption hard-fail.
- `tests/test_vacuity.py` — Z3 vacuity tautology/contradiction detection.
- `tests/test_vacuity_integration.py` — vacuity wired into alias register +
  invariant evaluation.
- `tests/test_vacuity_provenance.py` — alias provenance lands in journal.
- `tests/test_verify_aliases.py` — AliasRegistry parameter on core `verify()`.
- `tests/test_daisugi_facade_aliases.py` — Daisugi facade round-trip.
- `tests/test_llm_check_failclosed.py` — llm_check fail-closed on errors.
- `tests/test_violation_remediation.py` — actionable remediation hints.
- `tests/test_version_and_docs.py` — version 0.27.0 + docs updated.
- `tests/test_verify_postconditions_strict.py` — postcondition parity + tautology warnings.
- `tests/test_llm_check_production_path.py` — fail-closed through `evaluate_predicate`.
- `tests/test_contracts_strict.py` — strict delegation from caller stakes.
- `tests/test_aliases_dict_vacuity.py` — dict-form alias vacuity at registration.
- `tests/test_z3_dispatch_single_source.py` — z3_checks dispatch ≡ RECOGNIZED_OPAQUE_TYPES.
- `tests/test_vacuity_forall_outputs.py` — ForallOutputs vacuity detection.
- `tests/test_stage2_strict.py` — stage-2 opaque-postcondition rejection under strict.
- `tests/test_stage2_aliases.py` — stage-2 postcondition alias resolution.
- `tests/test_supervisor_aliases.py` — Supervisor forwards aliases to verify.
- `tests/test_subsumption_outer_visibility.py` — outer opaque visibility under strict.
- 1185 tests passing, 9 skipped (was 1158 passing).

### Compatibility

Non-breaking for low/medium stakes envelopes and any envelope using only
predicate-algebra `expr` invariants. Breaking only for the specific case
described in the migration notice at the top of this entry.

---

## v0.26.1 — 2026-05-11 — TransformersVLAExecutor: generic HF path for real VLAs

Generalizes the v0.26 abstract base into a concrete, lazy-loading
executor for any HuggingFace transformers-compatible VLA. Default
``model_id`` targets ``lerobot/smolvla_base`` — a ~450M-param policy
that runs on CPU — but the executor is model-id-pluggable so a host
with GPU can swap to ``lerobot/pi0`` (3.3B params) with a one-line
change.

**Added**

- ``opendaisugi.vla_executor.TransformersVLAExecutor`` —
  ``VLAExecutorBase`` subclass that loads any HF VLA via
  ``AutoProcessor`` + ``AutoModel``. Lazy-loads on first ``run()``:
  ``__init__`` allocates nothing, so a ``Daisugi`` import path that
  never invokes a VLAStep stays cheap. Critical on memory-constrained
  hosts.
- Output-unpack logic handles the three common shapes (dataclass with
  ``.actions``, dict with ``"actions"`` key, raw tensor) and the three
  common dims (``(1, T, action_dim)``, ``(1, action_dim)``,
  ``(action_dim,)``).
- Opt-in smoke test (``OPENDAISUGI_SMOLVLA_SMOKE=1``) that actually
  loads ``lerobot/smolvla_base`` and runs one inference. Skipped by
  default — load profile pushes <8 GB free-RAM laptops into swap.
- Recipe doc section in ``docs/pi-vla-integration.md`` covering
  memory profile, when to subclass, and smoke-test invocation.

**Tests**

- Mocked test exercises the full executor pipeline (model load,
  prediction, action unpack, MuJoCo rollout) using shim torch /
  transformers — validates the integration logic without downloading
  weights or allocating model memory.
- Lazy-load assertion: instantiating ``TransformersVLAExecutor`` does
  not touch torch or transformers.
- 1158 total tests passing (was 1156); +2 new, +1 opt-in.

**Compatibility**

- Additive on the v0.26 substrate. ``VLAExecutorBase`` and
  ``MockVLAExecutor`` unchanged. Tests don't load any real model
  unless ``OPENDAISUGI_SMOLVLA_SMOKE=1`` is set.

## v0.26.0 — 2026-05-09 — Vision-Language-Action (VLA) integration

A learned visuomotor policy (Physical Intelligence's π0/π0.5, an
LeRobot policy, an RT-2-style stack) participates in an
opendaisugi-supervised plan as one ``VLAStep`` per skill. The verifier
treats the VLA as opaque — what's verified is the *envelope around
the rollout* (workspace bounds, max action count, final-pose claims),
not the per-action stream emitted at 30Hz inside the skill. Existing
v0.8 trajectory-invariant Z3 checks now apply to ``VLAStep.target_pose``
the same way they apply to ``CartesianMoveStep.target_position`` —
a learned policy can't be asked to drive into a forbidden region.

The user's framing was "openDaisugi as muscle memory for VLAs."
v0.26 ships the prefrontal-cortex layer that sits above muscle memory:
the envelope decides what skills are allowed, the substrate audits
every rollout, the VLA does the actual motor work. We don't compete
with the VLA for control; we constrain and observe it.

**Added**

- ``VLAStep`` step type — registered via ``@step_type``, present in
  the discriminated union, round-trips through ``ActionPlan``.
  Carries ``task: str`` (natural-language skill description),
  ``target_pose: tuple[float, float, float] | None``,
  ``max_actions: int = 50``, ``timeout_s: float = 5.0``.
- ``opendaisugi.vla_executor.VLAExecutorBase`` — abstract scaffolding
  that handles MuJoCo loading, simulation stepping, evidence packaging.
  Subclasses implement one method (``_predict_actions``) and inherit
  the rest. Doesn't require a GPU; CPU-only base suffices for
  scaffolding tests.
- ``MockVLAExecutor`` — deterministic linear-interpolation policy for
  CI. Generates a fixed action sequence interpolating from the current
  pose toward ``step.target_pose``. Receipts carry real
  ``mjData.qpos`` and ``data.xpos[end_effector]`` — substrate is
  exercised without a learned policy.
- ``z3_checks._check_workspace_containment`` extended to
  ``VLAStep.target_pose``. Same shipped Z3 invariant; new step type.
- ``examples/pi-vla/`` kit — three scenarios (clean three-skill
  sequence; out-of-bounds target rejected pre-execution; physical-
  stakes delegation attempt rejected). Uses ``MockVLAExecutor``
  against the existing 2-DOF ``two_joint_arm.xml`` fixture.
- ``docs/pi-vla-integration.md`` — recipe for swapping in real π0
  inference via LeRobot, GPU requirements, Aloha MJCF as production
  target.
- ``skills/opendaisugi-checklist/references/vla-integration.md`` —
  agent-facing guidance on when to use ``VLAStep`` vs deterministic
  ``JointMoveStep`` / ``CartesianMoveStep``.

**Tests**

- 8 new tests in ``tests/test_vla_executor.py`` covering: step-type
  registration + round-trip, mock executor producing real MuJoCo
  state, abstract-base-refuses-without-subclassing, non-VLA-step
  refusal, out-of-bounds target rejection, in-bounds target
  acceptance, physical-stakes delegation guard, max-actions cap on
  flooding policies.
- 1156 total tests passing (was 1148).

**What's NOT in v0.26**

- No real ``LeRobotPi0Executor`` shipped — the model card name +
  processor signatures shift, and pinning a specific revision in core
  rots. Recipe doc shows the integration; user code subclasses.
- No vision pipeline in ``MockVLAExecutor``. Real subclasses add
  rendering.
- No on-policy fine-tuning of the VLA from receipts — separate
  research direction (v0.28+).
- No Aloha MJCF vendored — too heavy. Documented as production target.

## v0.25.1 — 2026-05-09 — Dish-wash kit driven by real MuJoCo physics

The v0.23 dish-wash kit shipped with a ``MockRoboticExecutor`` that
returned fabricated telemetry. Calling that an "openDaisugi-substrate
demo for robotics" was generous — a hardcoded JSON dict isn't motion.
v0.25.1 wires the kit to actual MuJoCo physics so the receipts carry
real joint positions and end-effector poses.

**Added**

- ``examples/dish-wash/mujoco_executor.py`` — ``DishWashMuJoCoExecutor``
  that translates each domain step type (ApproachDish / LocateRim /
  BeginScrub / RinseWithHose / ReturnToDock) into concrete joint
  targets on a 2-DOF test arm and delegates to the in-tree
  :class:`MuJoCoExecutor`. Receipts carry real ``mjData.qpos`` joint
  positions and ``data.xpos[end_effector]`` Cartesian poses, not
  mocked dicts.
- ``OPENDAISUGI_DISHWASH_MUJOCO=1`` env-var opt-in for the kit's
  ``run_dogfood.py`` so hosts without MuJoCo still get the
  fast-deterministic mock path. Default behavior unchanged for hosts
  that don't opt in.
- ``tests/test_dishwash_mujoco.py`` — end-to-end MuJoCo-driven test
  asserting the kit's 5-step plate-wash and 15-step three-plate
  workflows execute through real physics, the receipts carry distinct
  arm configurations across step types (i.e. motion is real), and the
  v0.18 substrate (envelope verify + per-step receipts + run-end
  integrity) holds against actual contact dynamics.

**Caveat**

The 2-DOF arm in ``tests/fixtures/mjcf/two_joint_arm.xml`` can't
physically wash a dish — it's a planar 2-joint test rig. The kit
exercises real physics, but the per-step joint trajectories are
schematic stand-ins for what a 6+ DOF dish-wash arm would actually do.
For a real deployment, swap in your robot's MJCF/URDF and rewrite
``DishWashMuJoCoExecutor._targets_for`` to your arm's joint set; the
domain step types and envelope stay unchanged.

**Tests**

- 2 new MuJoCo end-to-end tests (1148 total, +2).

## v0.25.0 — 2026-05-09 — Git-backed shared pathway registry

Multiple opendaisugi instances can now share pathways through a shared
git repository. Any team that already has a GitHub / GitLab / internal
git remote gets a registry for free — no server to operate, no postgres,
no S3. Git is the transport; PRs become the moderation queue; ed25519
signatures (v0.15) gate trust.

The first draft of this spec proposed a FastAPI service. Wrong shape
for the team sizes openDaisugi targets — every team that needs pathway
sharing already has a shared git remote, and git already provides
history, blame, branching, provenance, conflict resolution (manual
merge), access control (repo permissions), audit trail (commit log),
and a durable artifact store. The HTTP design is deferred to a future
v0.26+ if scale ever demands it; the v0.25 bundle YAML format is the
forward-compat contract.

**Added**

- ``opendaisugi.pathway_bundle.PathwayBundle`` — content-addressed,
  signed pathway transport unit. ``bundle_hash`` is sha256 of the
  canonical-JSON of (pathway, publisher, published_at). ``signature_b64``
  + ``signer_pubkey_b64`` carry the ed25519 signature so consumers can
  verify against a trusted-signers list. ``bundle_format_version`` +
  Pydantic ``extra='allow'`` give forward-compat for future fields.
- ``pathway_to_bundle(pathway, *, publisher, private_key_b64, public_key_b64)`` —
  serialize + optionally sign.
- ``bundle_to_pathway(bundle, *, trusted_pubkey_b64s, require_signed)`` —
  verify + return pathway. Raises ``UnsignedBundleError`` /
  ``UntrustedSignerError`` / ``InvalidSignatureError`` on failure.
- ``opendaisugi.git_pathway_store.GitPathwayStore`` — subclass of
  ``PathwayStore`` backed by a local clone of a shared git registry.
  ``pull()`` runs ``git pull`` and materializes new bundles into the
  local sqlite cache (verifying signatures against the repo's
  ``trusted-signers.json``); ``publish(pathway)`` signs, writes a
  bundle, ``git add`` + ``git commit`` + ``git push``.
- ``opendaisugi.signing.sign_bytes`` / ``verify_bytes`` — general-purpose
  ed25519 primitives shared by ``sign_contract`` (v0.15) and the
  pathway-bundle path. Operate on arbitrary canonical bytes.
- ``daisugi registry init/pull/publish/status/pull-and-tend`` CLI
  subcommands. ``pull-and-tend`` pairs with v0.22's
  ``daisugi hook auto-tend`` to fully close the captures → traces →
  distillation → publish-eligible-pathways loop on every team instance.
- ``skills/opendaisugi-checklist/references/git-registry.md`` — operator
  setup, daily flow, trust-boundary explanation.

**Tests**

- 9 new tests in ``tests/test_pathway_bundle.py`` covering signed +
  unsigned roundtrips, untrusted-signer rejection, tampered-bundle
  detection, content-addressing stability, forward-compat handshake.
- 5 new tests in ``tests/test_git_pathway_store.py`` covering the full
  publish/pull roundtrip across two cloned repos sharing one bare
  remote, untrusted-signer skip behavior, signing-key requirement at
  publish time, status diagnostics, offline-mode tolerance.
- 1146 total tests passing (was 1132).

**Compatibility**

- Additive. ``PathwayStore`` semantics unchanged. ``GitPathwayStore``
  subclasses it; ``Daisugi(pathway_store=...)`` callers can pass either.
- Bundle format version 1 is forward-compatible: future v0.26+ bundles
  may add fields without breaking existing consumers.

## v0.24.0 — 2026-05-09 — Plan-structure embedding distiller

Closes bet (c) deferred from `examples/REPORT.md`: the Distiller now
clusters by plan structure as well as task text, so two runs of the
same workflow with different task wording group into one pathway.
Prerequisite for v0.25's git-backed shared registry, where teams need
matches to reflect *what the work is*, not *how each teammate phrased
the task*.

**Added**

- ``opendaisugi.distiller.plan_structure_signature(plan)`` — returns a
  canonical ``→``-joined step-type sequence in topological order.
  Deterministic, ignores ids / fields / metadata.
- ``Distiller(structure_weight=0.5, ...)`` — new constructor knob.
  ``0.0`` = pure task-text clustering (v0.23 behavior), ``1.0`` = pure
  structural, ``0.5`` (default) = balanced. Raises ``ValueError``
  outside ``[0, 1]``.
- ``Distiller._embed_plan_structures(signatures)`` — mirror of
  ``_embed_tasks`` for the structural sequence string. Overridable in
  tests.
- ``CompiledPathway.structure_signature: str | None`` — stamped at
  distillation time; ``None`` on v0.23 rows (re-distilled on next
  ``tend``).
- ``DistillableTrace.structure_signature: str | None`` — populated by
  ``Journal.list_successful_traces`` from the new SQLite column.
- Journal SQLite schema migration v4 → v5: traces table gains
  ``structure_signature TEXT`` column + index. ``ALTER TABLE`` is
  idempotent so v0.23 stores upgrade in place.
- ``Journal.log()`` and ``Journal.log_run()`` compute and persist
  ``structure_signature`` at write time. The Distiller reads the
  column directly — no YAML body load per trace.

**Changed**

- ``_EMBEDDING_MODEL_VERSION`` bumped ``2`` → ``3``. The combined
  ``task ⊕ structure`` embedding is incomparable with v0.18-v0.23
  task-only vectors; the Gardener handles re-distillation on version
  drift via existing machinery.
- Distiller clustering now operates on a weighted concatenated vector
  ``(1-w)·task ⊕ w·structure`` (under L2-normalized inputs from
  ``sentence-transformers``, distance over the concat = weighted sum
  of per-component distances). The pathway's stored
  ``task_embedding`` remains task-only at 384-dim so
  ``PathwayStore.find`` continues to embed incoming queries at
  task-text dimensionality.

**Tests**

- 4 new tests in ``tests/test_distiller.py``:
  ``plan_structure_signature`` canonicality + shape-distinguishing,
  cross-wording cluster verification with mocked embedders, validation
  of ``structure_weight`` bounds.
- Updated ``test_journal.py::test_schema_migration_adds_new_columns_to_existing_db``
  for v5.
- 1132 total tests passing (was 1128).

**Compatibility**

- Additive on the SQLite layer (idempotent ALTER TABLE).
- Existing v0.23 stores migrate transparently on next open.
- Distillation latency goes up modestly (one extra
  sentence-transformer encode per trace, parallelizable; default
  embedding model is CPU-fast).

## v0.23.0 — 2026-05-09 — Robotics kit + Ollama provider; verify_step plan-level invariant fix

Closes two queued items from the v0.22 follow-up list: the home-machine
Ollama deployment story and the third worked-example kit (robotics).

**Added**

- ``OllamaTier1Provider`` — convenience subclass of
  ``LiteLLMTier1Provider`` with Ollama-shaped defaults (localhost:11434
  endpoint, no API key, auto-prefixes model name with ``ollama/``).
  Closes the pure-local deployment story:

      from opendaisugi import Daisugi, OllamaTier1Provider
      d = Daisugi(tier1=OllamaTier1Provider(model="llama3.2:3b"))

  No new functionality — pure ergonomics + discoverability over the
  Ollama path that was already supported via raw
  ``LiteLLMTier1Provider``. 6 new tests in
  ``tests/test_tier1_ollama.py``.

- ``examples/dish-wash/`` — robotics worked-example kit. Five domain
  step types (ApproachDish, LocateRim, BeginScrub, RinseWithHose,
  ReturnToDock) compose into a plate-wash 5-step sub-DAG;
  ``build_plan(N)`` chains N plate-washes via depends_on for the
  dish-wash sequence. Envelope is ``stakes="physical"`` with one
  structural invariant (``exists_step ReturnToDock``). Three scenarios:
  clean (3 plates → 15 receipts, integrity passes), missing terminal
  step (invariant rejects pre-execution), motion-step delegation
  attempt (``_check_delegation_safety`` rejects pre-execution).

  Proves the substrate's domain-agnosticism — the same machinery that
  supervised email-drafting and council-voting handles motion control
  with no architectural changes.

**Fixed**

- ``verify_step`` (v0.22 lightweight per-step path) was re-running
  plan-level predicate invariants on singleton plans. Quantified
  invariants like ``exists_step ReturnToDock`` always evaluated false
  on most singletons, producing spurious per-step rejections under the
  v0.22 supervisor flow. Plan-level invariants are now skipped in
  ``verify_step`` — they're plan-level by definition; the whole-plan
  ``verify()`` validated them once at the top of ``Supervisor.run``.

  Practical impact: any envelope using ``forall_steps`` / ``exists_step``
  invariants would have rejected its first step under v0.22, silently
  halting the run. The v0.18 Ada and Council kits happened to use
  scalar predicates so they ran fine; the dish-wash kit surfaced this
  immediately.

**Compatibility**

- All v0.22 tests still pass. 1128 total tests now passing (was 1122).
- The fix is strictly less-strict per-step (no more false-positive
  invariant rejections); whole-plan invariant enforcement unchanged.

## v0.22.0 — 2026-05-07 — Perf, ergonomics, closed reproduction loop

The follow-up to v0.21.1's hardening pass. Eight items on the queued
"deferred but valuable" list, all shipped.

**Performance**

- ``verify_step(step, envelope)`` (new) — lightweight per-step verification
  path that skips ``check_envelope_self_consistency`` and
  ``check_plan_against_envelope`` (both pure functions of the envelope,
  re-proven needlessly per step under v0.21). Supervisor.run uses it for
  per-step gating. Measured: 33× faster on 20-step plans (61ms → 1.86ms).
- Journal sqlite connection reuse — previously each
  ``append_receipt``/``log``/``log_run``/etc. opened a fresh connection
  (~0.4ms). Now a single long-lived connection per Journal instance with
  ``check_same_thread=False`` and autocommit. Measured: ~4× faster
  appends (0.4ms → 0.11ms), ~2× faster reads.

**Ergonomics**

- Renamed ``_StepBase`` → ``StepBase`` and ``_STEP_TYPE_REGISTRY`` →
  ``STEP_TYPE_REGISTRY``. The leading-underscore convention said
  "internal" but six in-tree files plus both shipped kits and external
  agent-authored types imported them across module boundaries. No
  back-compat alias retained — the rename is mechanical
  (s/_StepBase/StepBase/g, s/_STEP_TYPE_REGISTRY/STEP_TYPE_REGISTRY/g).
- ``CompiledPathway.activation_count`` now declared as a Pydantic field.
  Previously the gardener's ``record_run_outcome`` was incrementing a
  transient attribute that never persisted to the pathway store.
  Failure_count was already declared and persisted.
- ``mcp_server.run_plan`` now wraps ``Supervisor.run`` in
  ``asyncio.wait_for`` with a 300s default ceiling, overridable via
  ``OPENDAISUGI_MCP_RUN_TIMEOUT``. On timeout returns a structured
  ``status="timeout"`` response instead of hanging the FastMCP stdio
  transport.

**Reproduction substrate — closed loop**

- ``daisugi hook auto-tend`` (new CLI subcommand). Cron-friendly one-shot
  that closes the captures → traces → distillation loop: iterates
  captured sessions not yet converted, runs ``captures_to_trace`` on
  each, runs ``Daisugi.tend()`` if any new traces landed.
  ``--min-interval`` gate (default 1h) prevents thrashing; ``--force``
  overrides; ``--skip-distill`` leaves tend out. Wire into cron,
  systemd-timer, or Claude ``/loop`` — whichever scheduler your
  environment provides. Without this the v0.21 hook accumulated
  captures the Distiller never saw.
- Journal gains a ``hook_conversions`` table tracking which captured
  sessions are already converted, plus ``Journal.is_session_converted``
  and ``Journal.mark_session_converted`` helpers. ``daisugi hook
  to-trace`` now records the conversion automatically.

**Hygiene**

- README rewritten from "v0.11.1 status" to v0.22.0 reality. Updated
  "What openDaisugi does not do" section, expanded roadmap with the
  v0.15 → v0.22 release history, added Receipt / DelegatingExecutor /
  StepBase / step_type / coerce_step / hook module to the API
  reference.
- Dependency upper bounds pinned (``<2`` on instructor, litellm,
  pydantic; ``<1`` on typer). Both instructor 2.0 and litellm 2.0 are
  in beta with breaking changes; a fresh ``pip install`` could silently
  pick them up and break envelope generation, delegation, and
  ``llm_check`` simultaneously.

**Compatibility**

- The ``_StepBase`` → ``StepBase`` rename is breaking for any external
  code that imported the underscored name. The rename is mechanical;
  agent-authored kits update with one sed.
- ``ActionPlan._dispatch_steps`` and ``RefinementRecord._dispatch_step``
  now share a ``coerce_step`` helper from ``opendaisugi.models``
  (introduced in v0.21.1). v0.22 inherits this with no further change.
- All 1121 v0.21.1 tests pass unchanged.

## v0.21.1 — 2026-05-07 — Architectural readiness pass: security, robustness, registry safety

A primetime-readiness review (full SGCM + simplify + architectural
code-review) surfaced findings across security, robustness, and API
hygiene. None of the findings were release-blockers in isolation, but
in aggregate they were the difference between "experimental" and
"deployable." This patch closes the security and robustness items.

**Security**

- Captures directory now created with mode 0o700. Shell commands and URLs
  may contain secrets; world-readable mode 755 leaked them to other local
  users. The `hook.py` module docstring now flags captures as sensitive.
- `DelegatingExecutor` now propagates the supervisor's `timeout_s` and a
  `max_tokens` cap (derived from `max_output_bytes // 4`, floor 256) into
  both the litellm completion call and the claude-code-llm subprocess.
  Previously these were silently dropped with a "protocol symmetry"
  comment, which let a misbehaving model hold the run loop for the
  litellm default timeout.

**Robustness**

- `Journal.log()` and `Journal.log_run()` now write the YAML body BEFORE
  the SQLite INSERT and roll back the YAML on insert failure. Previously
  a crash between the auto-commit on the SQLite `with` block and the YAML
  write would leave an orphan index row pointing at a missing body,
  permanently breaking `load_trace`/`replay` for that id.
- `Supervisor.run` wraps `approval.decide()` in `try/except`. Custom
  approval strategies that raise (network timeout, callback failure) now
  abort the run cleanly instead of propagating past the step loop.
- `Supervisor._write_step_receipt` wraps the postcondition check in
  `try/except`. A subclass override that raises now produces a
  `verify_result=False` receipt with the exception in `verify_details`
  rather than crashing the run.
- `Supervisor._check_run_integrity` wraps the journal read in
  `try/except` so a sqlite-lock or read failure in the `finally` block
  cannot suppress the original exception.
- ABORTED/HALTED_BY_SIMPLEX integrity expected-set now considers all
  steps the executor actually ran (status in `("succeeded", "failed")`)
  rather than only succeeded ones. A halt-on-stage2-failure no longer
  passes integrity if the failed step's receipt is missing — silent
  skips on failing steps are now caught.

**Registry safety + DRY**

- `step_type` decorator raises `ValueError` on discriminator collision
  unless the caller explicitly opts in via `@step_type(override=True)`.
  Re-registering the same class remains idempotent. Previously an
  adversarial or accidental kit could silently shadow `ShellStep` (or
  any other built-in) by registering `type="shell"`.
- `RefinementRecord` and `ActionPlan` now share a single `coerce_step`
  helper in `opendaisugi.models`; the duplicate dispatcher in
  `refinement.py` is gone.

**Performance**

- `hook.list_sessions` reads first/last lines via stat-and-seek instead
  of slurping every JSONL file. List cost drops from O(total_records)
  to O(num_sessions).

**Tests**

- Six new tests on `step_type` collision/override/idempotence/coerce_step.
- Two new tests on `DelegatingExecutor`'s timeout + max_tokens propagation.
- One new test on supervisor's approval-strategy-raises path.
- 1121 total tests passing (was 1112).

## v0.21.0 — 2026-04-25 — Passive hook: tool-call capture for distillation

**Framing** — Claude Code, Hermes, and OpenClaw all ship tool-blocking
hooks. None ship distillation. v0.21 carves out the wedge they don't
fill: a passive hook that captures tool calls into JSONL files which
then feed `daisugi tend`. Two deployment modes share one journal — the
**active supervisor** enforces envelopes / receipts / integrity; the
**passive hook** observes runs from external runtimes and turns them
into Distiller fuel.

**Added**

- `opendaisugi.hook` module: `record_call` (append a normalized capture
  to a session JSONL), `list_sessions` (summarize captures), `infer_envelope`
  (synthesize a permissive envelope from observed tool calls),
  `captures_to_trace` (convert captured session into a journal trace).
- `daisugi hook record` CLI subcommand — non-blocking JSON stdin/stdout
  designed for Claude Code's `PreToolUse`, Hermes' shell-hooks, OpenClaw's
  middleware chain.
- `daisugi hook list` and `daisugi hook to-trace <session_id>` CLI
  subcommands for inspection and conversion.
- `docs/hook-integration.md` — wiring recipes for Claude Code, Hermes,
  and OpenClaw (plus the current state of OpenClaw's roadmapped
  pre-process events).
- `skills/opendaisugi-checklist/references/passive-capture.md` — the
  two-mode framing (passive hook vs active supervisor).

**Tests**

- 9 new tests in `tests/test_hook.py` covering record, list, infer,
  captures_to_trace round-trip, and the never-block invariant.

**Compatibility**

- Purely additive. No primitives changed. Hook output is JSONL files
  in `~/.opendaisugi/captures/` by default, configurable via
  `--captures-root`. Hook MUST NOT break the host runtime — even
  malformed input returns `{"continue": true}` on stdout.

**Deliberately not in v0.21**

- No verify-and-advise hook mode (option 2 from the SGCM design pass) —
  defer to future when there's a clear use case.
- No verify-and-block — Claude Code / Hermes / OpenClaw all already do
  this well; openDaisugi would be strictly worse on that surface.
- No automatic captures→traces conversion. Explicit `to-trace` keeps
  the user in control of what feeds distillation.

## v0.20.0 — 2026-04-25 — MCP runtime surface: run_plan, receipts_for_run, recent_runs

**Framing** — The MCP server shipped six read-side / pure-verification
tools in v0.6 (`envelope_for`, `find_pathway`, `verify_plan`,
`verify_completed_step`, `list_pathways`, `pathway_stats`). v0.20 closes
the **runtime** surface: external agents can now orchestrate verified
runs and audit receipts through the standard MCP transport, no Python
imports required. No new library primitives — pure exposure of the
v0.18+v0.19 machinery.

**Added**

- `run_plan(plan, envelope)` MCP tool — verifies + executes under
  supervisor + returns `{run_id, status, integrity_passed, receipts}`.
  Receipts include `model_id` from v0.19. Default executors (DryRun) for
  vanilla deployments; real-execution deployments wire custom executors
  via `Daisugi(...)` before passing to `serve()`.
- `receipts_for_run(run_id)` MCP tool — journal receipt query for a
  specific run; lets external auditors confirm every step actually
  produced evidence.
- `recent_runs(limit=20)` MCP tool — discovery surface for past runs in
  the journal index.
- `skills/opendaisugi-checklist/references/mcp-usage.md` — the nine-tool
  surface, typical flow, deployment-time vs agent-time concerns.

**Test coverage**

- `tests/test_mcp_server.py` gains 6 tests covering the three new tools
  + tool-registration check. Total MCP test count: 17.

**Compatibility**

- Additive. Existing MCP clients see three new tools alongside the six
  they already knew. No tool signatures changed. The `daisugi mcp serve`
  CLI subcommand (already present) automatically exposes the new tools.

## v0.19.0 — 2026-04-25 — Cheap-model delegation: tiered execution + per-receipt model attribution

**Framing** — The v0.18 reproduction substrate made selection signal
trustworthy via per-step receipts and the integrity check. The whole point
of trustworthy selection is that runs differ in cost. v0.19 ships the
delegation protocol that makes that real: a `DelegatingExecutor` runs
steps by prompting a configurable LLM, steps declare their preferred model
via a hint, receipts attribute success/failure to the specific model.
Physical-stakes envelopes refuse delegation outright — robotic motion
trajectories cannot be delegated to a model whose arguments static
verification cannot ground.

**Added**

- `Receipt.model_id` field — when an LLM-backed executor produced the
  evidence, this records which model. None for non-LLM executors. (L1)
- `_StepBase.preferred_model` field — agent or human declares per-step
  model preference; honored by `DelegatingExecutor`, ignored by other
  executors. (L2)
- `opendaisugi.delegating_executor.DelegatingExecutor` — a `StepExecutor`
  that runs a step by prompting a configurable LLM with the step's
  serialized fields, retries on response-schema validation failure,
  stamps the model used on `.last.model` so the supervisor can populate
  `Receipt.model_id`. (L3)
- `verify._check_delegation_safety` — runs before permissions stage,
  rejects plans where `envelope.stakes='physical'` AND any step has
  `preferred_model`. The hard refusal that keeps the v0.8 robotics path
  honest. (L4)
- `Supervisor._write_step_receipt` reads `executor.last.model` and stamps
  it onto the Receipt. (L6)
- Ada email kit `scenario_3_haiku_drafting` — same DAG as scenario 1
  but DraftEmail steps route through `DelegatingExecutor`. Receipt
  `model_id="haiku"` round-trips through journal. Run output captured.
- `skills/opendaisugi-checklist/references/delegation.md` — when to set
  `preferred_model`, when not to, the `llm_check` postcondition for
  perceptual claims, the model_id selection signal, drafter/reviewer
  split as a common pattern.
- v0.19 update section in `examples/REPORT.md`.

**Changed**

- Journal sqlite schema migration v3 → v4: receipts table gains
  `model_id TEXT` column. ALTER TABLE is idempotent so existing v0.18
  journals upgrade in place.

**Note**

- The `llm_check` postcondition primitive (LLMCheck predicate + the
  `call_llm_check` helper) shipped in v0.9 already; v0.19 surfaces it
  via the delegation reference doc as the perceptual-judgement
  counterpart to structural Z3 invariants.

**Compatibility**

- Additive. v0.18 journals migrate transparently. Existing kits run
  unchanged; opting into delegation is per-step.

## v0.18.0 — 2026-04-24 — Reproduction substrate: receipts, integrity, contract-orchestration kits

**Framing** — Opendaisugi ships its reproduction substrate for skills
(thesis framing #3, recorded in project memory). The Checklist-style
agent skill is the birth mechanism; per-step receipts produce the
evidence the Gardener's selection signal feeds on; a run-end integrity
check guarantees no silent step-skipping. Two worked-example kits
(Ada email, agent council) demonstrate Z3-backed contract orchestration
between agents as the concrete product claim — the thing Claude's
built-in allow/deny cannot express.

**Added**

- `Receipt` model + `compute_evidence_hash` content-addressing (`models.py`, L1)
- Journal receipt append/read; sqlite `receipts` table (`journal.py`, L2)
- Per-step `postcondition` field on `_StepBase`; supervisor writes a
  Receipt after each executed step with evidence from the ExecutorResult,
  optional postcondition-check result (`supervisor.py`, L3)
- Run-end integrity check; `RunSession.integrity_passed`,
  `RunSession.failed_step_id`; `IntegrityViolation` exception. Halt-on-
  failure remains valid (contiguous-prefix receipts OK); silent
  step-skipping marks integrity-failed (L4)
- Dynamic step-type registration via `@opendaisugi.step_type` decorator;
  `get_step_type_registry()`; built-in step types (Shell, FileRead/Write,
  Network, JointMove, CartesianMove, Gripper, SimulationReset) all
  self-register (`models.py`, L5)
- Parser-side decomposition of compound shell into atomic ShellSteps
  with sequential `depends_on` edges (`parsers/claude_code.py`, L6)
- `Violation.suggested_remediation` carries a ready-to-paste decomposed
  form when a rejected command decomposes cleanly (`models.py`, `verify.py`, L7)
- `record_run_outcome` + `RunOutcome` in `gardener` package; counts
  integrity-failed runs as pathway failures, giving Gardener selection
  pressure trustworthy input (L8)
- `skills/opendaisugi-checklist/` — agent skill + 5 references teaching
  the DSL-invent / verify / execute / integrity-check workflow
- An example email kit — Z3 invariant forbids impersonating
  Robin; dogfood runner proves the reject-before-execution path
- `examples/agent-council/` kit — Z3 invariant forbids PII-flagged
  reviews; dogfood runner proves three scenarios (clean / PII /
  quorum-missed) with full receipt + integrity coverage
- `examples/REPORT.md` — honest yes/no answers for the three v0.18 bets

**Changed**

- `ActionPlan.steps` typed `list[Any]` with field-validator dispatch
  via `_STEP_TYPE_REGISTRY` (preserves subclass identity of dynamically
  registered step types across JSON round-trips). After-validator
  enforces every element is a `_StepBase`
- `RefinementRecord.step` / `recomputed_step` likewise validator-
  dispatched so custom step types round-trip through refinement
  records too

**Compatibility**

- Additive. All 1068 pre-existing tests pass unchanged. The integrity
  check is new but only flags silently-skipped steps — well-behaved
  supervisors pass trivially. The `ActionPlan.steps` type change
  preserves all existing Pydantic discriminated-union behavior for
  built-in step types.

## v0.17.0 — 2026-04-23 — Envelope realism from portfolio dogfood

**Highlights**

Ran the batch-trace pipeline across the entire Claude Code project
directory — 25 projects, 1,100 episodes, 13,104 tool-steps — and used
the violation distribution to drive four surgical envelope / parser
fixes that leave the v0.16 guarantees intact.

- **``shell_allowlist`` accepts glob patterns.** Entries containing
  ``*``, ``?``, or ``[`` match segment-by-segment against the command
  head with equal-segment-count anchoring: ``.venv/bin/*`` matches
  ``.venv/bin/python`` but not ``.venv/bin/subdir/python`` or
  ``/usr/local/.venv/bin/python``. Literal entries still require exact
  equality, so existing allowlists behave identically. ``PurePosixPath.match``
  alone is **not** sufficient here — it is right-anchored and would
  silently accept an unrelated absolute path.
- **Env-prefix and comment-aware shell head extraction.** POSIX
  env-prefixed invocations like ``GIT_DISCOVERY_ACROSS_FILESYSTEM=1 git
  status`` now resolve to ``git`` for allowlist lookup; comment-only
  lines and bare env-assignments are no-ops (nothing to execute) and
  produce no violation.
- **Metachar gate runs first, unconditionally.** Reordered so the raw
  command is cleared of ``;``, ``|``, ``&``, `` ` ``, ``$(`` before any
  head classification. Env-prefix skipping, comment detection, and glob
  allowlists are classifier conveniences that cannot soften the metachar
  invariant. An adversarial injection like ``A=$(rm -rf /) git status``
  still rejects, as the dedicated invariant test confirms.
- **``ClaudeCodeParser`` understands modern transcript layout.** Current
  Claude Code ``.jsonl`` rows wrap real turns under ``message`` with a
  top-level ``type`` discriminator, alongside many metadata row types
  (``system``, ``custom-title``, ``file-history-snapshot``,
  ``agent-name``, ``attachment``). ``_read_messages`` now normalizes both
  the legacy flat shape and the modern wrapped shape into the flat
  ``{role, content}`` dicts the rest of the parser expects. Without this,
  pointing the parser at a live ``~/.claude/projects`` transcript
  silently produced zero episodes.
- **Distiller strips skill-invocation preamble before embedding.** Task
  strings captured from ``/skill-name`` opens carry heavy boilerplate
  (``Base directory for this skill: /path/...``, ``### Skill: name``,
  ``<command-name>…</command-name>`` tags). Embedding those directly made
  every ``/skill``-opened session cluster together by preamble, swamping
  semantic intent. ``_normalize_task_for_embedding`` strips known
  shapes; the original trace text is unchanged in the journal and in
  ``CompiledPathway`` output. If stripping would empty the string we
  fall back to the original so unrelated tasks don't collapse together.

**Added**

- ``opendaisugi.verify._head_allowed`` — anchored glob matcher for
  ``shell_allowlist``.
- ``opendaisugi.verify._extract_shell_head`` — env-prefix- and
  comment-aware classifier.
- ``opendaisugi.distiller._normalize_task_for_embedding`` — skill-preamble
  strip for distillation embeddings.
- 14 new tests in ``tests/test_verify.py`` (glob semantics, env-prefix,
  comment lines, metachar invariant).
- 6 new tests in ``tests/test_distiller.py`` (preamble strip cases +
  fallback).
- 2 new tests in ``tests/test_parsers.py`` + ``sample_transcript_modern.jsonl``
  fixture.

**Changed**

- ``_EMBEDDING_MODEL_VERSION`` bumped ``1`` → ``2`` (existing cached
  vectors were encoded without preamble strip and are not comparable
  under the new preprocessing).

**Compatibility**

- Envelope-side changes are strictly additive: literal ``shell_allowlist``
  entries behave unchanged, the metachar gate is strictly no-less-strict
  (it runs earlier now), and env-prefix / comment handling only reclassifies
  steps the v0.16 verifier would already have rejected for a misread head.
- ``ClaudeCodeParser`` remains back-compatible with flat-shape fixtures.
- Consumers of ``PathwayStore`` rows keyed on embedding version should
  re-embed (see ``_EMBEDDING_MODEL_VERSION``).

## v0.16.0 — 2026-04-21 — Structured logging + enterprise deployment docs

**Highlights**

- **Structured logging on hierarchical loggers.** Every decision
  boundary now emits a named record: ``verify.pass``/``fail``,
  ``delegation.allow``/``deny``, ``signing.verify_ok``/``verify_failed``,
  ``run.start``/``end``/``rejected_by_verify``/``step_halted``/
  ``step_recomputed``/``approval_denied``. Records carry structured
  fields (``run_id``, ``envelope_id``, ``violation_count``,
  ``violation_stages``, ``step_id``, ``approved_by``, ``status``) via
  ``extra=`` — compatible with structlog, ``python-json-logger``, or
  any stdlib handler. A ``NullHandler`` at the top-level
  ``opendaisugi`` logger keeps the library silent by default.
- **Enterprise deployment guide** (``docs/deployment.md``). Install,
  data directory layout, config surfaces, log routing, trusted-signer
  registry management, backup/restore, multi-instance, upgrades,
  diagnostics. Written so an operator can stand the library up without
  a vendor engagement.
- **Security model doc** (``docs/security-model.md``). What the library
  protects against, what it does not, trust boundaries, data handling,
  incident response entry points, compliance posture. No certification
  claims — the document is framed as input for the adopting team's own
  risk review.

**Added**

- ``opendaisugi.verify``, ``opendaisugi.contracts``,
  ``opendaisugi.signing``, ``opendaisugi.supervisor`` loggers with
  named events.
- NullHandler attached at import time on ``opendaisugi`` so importing
  the library is side-effect-free for hosts without logging config.
- ``tests/test_logging.py`` — 7 tests asserting the NullHandler idiom,
  verify pass/fail records, supervisor run lifecycle, supervisor
  verify rejection + approval denial, contract delegation allow.
- ``docs/deployment.md`` and ``docs/security-model.md``.

**Compatibility**

- Purely additive. No existing API changed. Hosts that were not
  configuring opendaisugi loggers see identical behavior (NullHandler
  swallows records). Hosts that were configuring them by the implicit
  convention still work; log messages are now short stable event names
  (``"verify.pass"``) rather than ad-hoc prose.

## v0.15.0 — 2026-04-20 — Real contract signing + length algebra

**Highlights**

- **Ed25519 contract signing ships.** The ``_verify_signature`` stub that
  shipped in v0.11.0 is replaced by real cryptographic verification
  behind an optional ``[sign]`` extra. ``verify_delegation`` now accepts
  ``trusted_signers=["name", ...]`` and checks each contract's
  signature against a persistent :class:`TrustedSignerRegistry`
  (default ``~/.opendaisugi/trusted_signers.json``). Tampered contract
  bodies fail closed.
- **``length_range`` operator closes an algebra gap.** Envelope authors
  can now say "drafted email body must be 10–5000 characters" as a
  first-class predicate. Strings compile to ``z3.Length(var)`` so
  subsumption reasoning stays sound; lists/dicts fall back to concrete
  evaluation.

**Added**

- ``src/opendaisugi/signing.py`` — canonicalization, keypair generation,
  sign/verify primitives, and the ``TrustedSignerRegistry`` JSON store.
  Lazy-imports ``cryptography`` so builds without the ``[sign]`` extra
  still import cleanly (signing functions raise ``SigningUnavailable``
  only when actually called).
- ``LengthRange(path, min, max=None)`` in ``predicate.py`` — discriminated
  union member with ``op: "length_range"``.
- ``tests/test_signing.py`` — 17 tests: keygen distinctness,
  canonicalization excludes signature/signer, sign/verify roundtrip,
  tampered-body rejection, wrong-key rejection, registry persistence,
  multi-signer disjunction, end-to-end delegation accept/reject.
- ``tests/test_length_range.py`` — 11 tests: parse, evaluate (within/
  below/above bounds, open-ended, missing path, lists), Z3 verification
  with counterexamples, JSON roundtrip.
- ``[sign]`` extra: ``cryptography>=42.0``.

**Changed**

- ``verify_delegation`` gains ``trusted_signers`` and ``signer_registry``
  keyword arguments. Unsigned contracts still work as before
  (``signature_valid=None``). Signed contracts without ``trusted_signers``
  are rejected with a clearer reason (``"no trusted_signers supplied
  to verify against"``).
- Top-level package exports ``LengthRange``, ``TrustedSignerRegistry``,
  ``generate_keypair``, ``sign_contract``, ``verify_signature_raw``,
  ``canonicalize_contract``, ``default_registry_path``,
  ``SigningUnavailable``.

**Compatibility**

- No breaking API changes. Existing ``verify_delegation`` callers see
  identical behavior for unsigned contracts. The signed-but-no-registry
  case that previously returned ``signature_valid=False`` continues to
  do so; only the reason string wording shifted.
- ``LengthRange`` is additive — no existing predicate semantics changed.

## v0.14.0 — 2026-04-20 — Semantic recursion into interpreter payloads

**Highlights**

- **Closes the interpreter-escape attack at Stage 1 verify.** v0.13
  surfaced the risk and offered a strict mode for subsumption; v0.14
  parses tractable interpreter payloads at verify time and recursively
  checks the embedded command against the same allowlist. ``sh -c
  "rm -rf /home"`` with ``shell_allowlist=["sh"]`` no longer passes —
  the inner ``rm`` is now visible and gets rejected.
- **Tractable interpreters** (parsed and recursed):
  ``{sh,bash,zsh,dash,ksh,fish,csh,tcsh} -c`` / ``xargs`` /
  ``find -exec CMD {} +`` / ``env [VAR=val] CMD``.
- **Opaque interpreters** (``python``/``perl``/``ruby``/``node``/
  ``awk``/``sed``/``make``) interpret non-shell languages; their
  payloads can't be recursed into. Under ``strict`` policy verify
  rejects them; under ``surface``/``allow`` they pass.

**Added**

- ``src/opendaisugi/interpreter_parse.py`` — ``parse_interpreter()``
  + ``InterpreterPayload`` dataclass. shlex-based, handles ``-c``,
  ``xargs`` flag sets (``-n``/``-I``/``-P``/``-L``/``-d``/``-E``/
  ``-s`` and their long forms), ``find`` with all four exec variants
  (``-exec``/``-execdir``/``-ok``/``-okdir``) and both terminators
  (``;`` and ``+``), ``env`` with ``-i`` and ``VAR=val`` prefix.
- ``tests/test_interpreter_recursion.py`` — 27 tests covering the
  parser (15) and the verify-time recursion (12) including nested
  ``bash -c "sh -c '...'"``, the depth-4 recursion cap, and
  per-policy opaque-interpreter handling.

**Changed**

- ``verify.check_permissions`` delegates shell-step checks to a new
  internal ``_check_shell_command`` helper that recurses. Depth-0
  violations read the same as before; depth>0 violations now include
  ``(inside interpreter at depth N)`` so callers see where in the
  recursion the violation arose.
- ``Envelope.shell_interpreter_policy`` gains a new verify-time
  meaning for opaque interpreters: ``"strict"`` blocks them,
  ``"surface"``/``"allow"`` pass them. Subsumption semantics
  unchanged from v0.13.

**Compatibility**

- No API changes. ``sh script.sh`` (no ``-c``) still passes as before
  — recursion only fires when a ``-c``/``-exec``/``xargs CMD``/
  ``env CMD`` payload is present.
- Existing envelopes that relied on ``sh`` in ``shell_allowlist`` as a
  permissive catch-all will now see failures whenever the plan uses
  ``sh -c`` with commands outside the allowlist. This is the intended
  correction.

## v0.13.0 — 2026-04-20 — Interpreter-escape hardening

**Highlights**

- **Closes a known static-verification bypass.** An envelope with
  ``shell_allowlist=["sh"]`` and ``NotMatches("^rm ")`` admitted
  ``sh -c "rm -rf /home"``: the head literally was "sh" (allowlist
  passes) and the command started with "sh" (regex passes). The
  dangerous action lived inside the interpreter's argument, outside
  the predicate algebra's scope. v0.13 surfaces this risk rather than
  silently approving. Full semantic recursion over interpreter
  payloads (``sh -c`` / ``xargs`` / ``find -exec``) is v0.14+ work.
- **Also fixes Attack C from the same audit.** Outer LLMCheck
  predicates were bound optimistically to ``BoolVal(True)`` in the
  subsumption SAT query, silently approving delegations where the
  outer envelope had an LLMCheck the inner did not. Outer-only soft
  nodes are now pessimistically bound to ``False`` and surfaced in
  ``SubsumptionResult.unverified_invariants`` with a ``soft:`` prefix.

**Added**

- ``Envelope.shell_interpreter_policy: Literal["surface", "strict", "allow"]``
  — governs what subsumption does when an interpreter name appears in
  either envelope's ``shell_allowlist``. Default ``"surface"`` flags
  the interpreter in ``unverified_invariants`` as
  ``shell_interpreter:<name>``. ``"strict"`` causes subsumption to
  fail outright when inner admits an interpreter. ``"allow"``
  suppresses surfacing for users who have accepted the residual risk.
- ``opendaisugi.models.SHELL_INTERPRETERS`` — frozenset enumerating
  recognised interpreter names (sh/bash/zsh/xargs/find/python/perl/
  ruby/node/make/awk/sed/env/…). Extend downstream if you need more.
- ``tests/test_interpreter_escape.py`` — 8 acceptance tests covering
  the default-surface, strict-failure, allow-suppression, and
  non-interpreter paths.
- ``tests/test_subsumption.py::test_outer_llm_check_is_not_silently_approved``
  and ``test_shared_llm_check_in_both_envelopes_still_subsumes`` —
  Attack C regression tests.
- [docs/limitations.md](docs/limitations.md) section
  "Shell interpreter escape" — threat model, three policies, and the
  v0.14+ recursion roadmap.

**Changed**

- ``envelope_subsumes`` now pessimistically binds outer-only soft
  nodes to ``BoolVal(False)`` (previously ``BoolVal(True)``). Inner
  soft nodes remain optimistically ``True`` — the same semantics as
  before for any predicate present in both envelopes.
- ``SubsumptionResult.unverified_invariants`` may now contain entries
  prefixed with ``soft:`` (outer-only soft predicates) and
  ``shell_interpreter:`` (interpreter names). Existing entries
  (invariants without an ``expr``) continue to appear by type name.

**Removed**

- Top-level ``opendaisugi.PredicateCounterexample`` alias. The
  underlying ``Counterexample`` dataclass still lives in
  ``opendaisugi.predicate_z3`` — import it directly
  (``from opendaisugi.predicate_z3 import Counterexample``) if you
  were using the alias. No known external consumers.

**Changed (internal)**

- Promoted ``opendaisugi.llm._resolve_backend`` to public
  ``opendaisugi.llm.resolve_backend``. ``llm_check`` and the
  Claude-Code transcript parser now route through it instead of
  duplicating the ``OPENDAISUGI_LLM_BACKEND`` env-var lookup inline.

**Compatibility**

- No breaking API changes. Existing envelopes without an interpreter
  in their allowlist are unaffected. Envelopes with interpreters now
  surface ``shell_interpreter:<name>`` under the default policy —
  this is new information in ``unverified_invariants`` but does not
  change ``holds``. Set ``shell_interpreter_policy="allow"`` to
  suppress.
- Envelopes relying on the prior optimistic outer-LLMCheck binding
  (Attack C) will begin failing subsumption. This is the intended
  correction.

## v0.12.0 — 2026-04-19 — ClaudeCode-as-LLM universal provider

**Highlights**

- **Subscription-credits path for every LLM call.** Setting
  `OPENDAISUGI_LLM_BACKEND=claude-code` (or passing `--llm claude-code`)
  routes every opendaisugi LLM call through a local `claude -p`
  subprocess. No `ANTHROPIC_API_KEY` required. Covers all eight call
  sites: envelope generation, distillation (generalize / improve /
  adapt), recompute fallback, Tier-1 provider, LLMCheck verification,
  and transcript parsing.
- **`src/opendaisugi/claude_code_llm.py`** — new module exporting
  `call_claude_p_async`, `call_claude_p_sync`, `call_claude_p_json_sync`,
  `call_claude_p_structured`, and `ClaudeCodeInstructorClient` (an
  instructor-compatible shim). The existing `ClaudeCodeTier1Provider`
  subprocess machinery was the blueprint; this generalizes it.
- **`get_instructor_client(model, *, backend=None)`** — the factory
  used by six of the eight call sites now branches on backend. A single
  change there covers envelope generation, distillation ×3, recompute
  fallback, and the Tier-1 instructor-wrapped provider. Default remains
  `litellm` — v0.11.x behavior is preserved byte-for-byte.
- **CLI flag `--llm {litellm|claude-code}`** on `daisugi generate-envelope`
  and `daisugi journal parse`. Sets `OPENDAISUGI_LLM_BACKEND` for the
  duration of the command, following the pattern already used by
  `OPENDAISUGI_LLM_CHECK_MODEL` et al.
- **End-to-end verified.** A 296 KB Claude Code transcript was
  round-tripped through `daisugi journal parse --llm claude-code`, and
  `daisugi generate-envelope --llm claude-code` produced a real
  Envelope in ~14 s with no API key set. Opt-in integration test
  (`tests/test_claude_code_integration.py`) guards the pipeline for
  future releases.

**Added**

- `src/opendaisugi/claude_code_llm.py` — subprocess helpers and
  instructor shim.
- `tests/test_claude_code_llm.py` — 15 unit tests (async + sync + JSON
  + structured + shim, happy path + missing binary + timeout + nonzero
  exit + validation failure).
- `tests/test_llm_backend.py` — factory routing plus migrations at
  `llm_check` and `parsers.claude_code._llm_split`.
- `tests/test_cli_llm_flag.py` — CLI flag tests with env-var isolation
  fixture so the flag doesn't bleed between tests.
- `tests/test_claude_code_integration.py` — opt-in end-to-end
  (`DAISUGI_CLAUDE_CODE_INTEGRATION=1`).

**Changed**

- `llm.get_instructor_client(model)` now accepts an optional `backend`
  keyword. Default behavior (return `instructor.from_litellm(...)`) is
  unchanged.
- `llm_check.call_llm_check` and
  `parsers.claude_code._llm_split` gained a ~10-line branch on
  `OPENDAISUGI_LLM_BACKEND`. Default path stays litellm.

**Notes**

- Each claude-p call spawns a subprocess (~0.5-1 s overhead vs direct
  API). Acceptable for envelope generation / distillation; noticeable
  for LLMCheck in tight loops. No batching.
- The subprocess inherits the parent environment, including
  `ANTHROPIC_API_KEY` if set. Unset it beforehand if you want strict
  API-key isolation.
- Contract cryptographic signing (originally slated for v0.12) moves to
  v0.13 — the subscription-credits path was the higher-leverage
  unlock for real users.

**Breaking**: none. No API changes. Env var and CLI flag are both opt-in.

## v0.11.1 — 2026-04-19 — Audit-prep polish

**Highlights**

- **Diataxis-shaped README.** Cut from 766 to 253 lines. Version-by-
  version feature catalog moved out; README is now the index plus one
  runnable tutorial, with every claim pointing at a file the reader
  can open.
- **`docs/concepts.md`** — explanation quadrant. Envelopes, predicate
  algebra, Z3 compilation, soft nodes, verification stages, and
  skills-as-contracts subsumption, every claim citing a source path.
- **`docs/limitations.md`** — honest "can't do" list so evaluators
  don't burn an afternoon on discovery. Covers what opendaisugi is
  not (sandbox, hallucination detector), predicate algebra
  boundaries, regex translator limits, soft node consequences, Z3
  practical limits, platform constraints, and planned-but-not-shipped
  features.
- **`docs/feature-status.md`** — maturity matrix with four tiers
  (production-candidate, working, experimental, planned). Single
  table, legible at a glance.
- **Audit hygiene: narrow the subsumption diagnosis exception** from
  bare `Exception` to `z3.Z3Exception`. Genuine programming errors
  now surface as tracebacks instead of being swallowed as "unknown"
  outer violations.

**Breaking**: none. No API changes.

## v0.11.0 — 2026-04-19 — Real Z3, skills as contracts

**Highlights**

- **Real Z3 compilation.** `predicate_z3.compile_to_z3` now emits honest Z3
  BoolRef expression trees — `InRe` nodes for `Matches`, symbolic `String`
  and `Real` variables for step fields, real `And` / `Or` / `Not` / `Implies`
  connectives, and concrete-plan quantifier unrolling. Through v0.10.0 the
  function wrapped `evaluate_predicate`'s Python bool in `z3.BoolVal`:
  Python did all the reasoning and Z3 did none. The "restricted predicate
  algebra authorable by agents compiling to SMT-LIB2 that Z3 solves"
  thesis now matches the code. Structural tests in
  `tests/test_predicate_z3_real.py` would fail against the v0.10.0 fake
  and pass against the real implementation.
- **Python `re` → Z3 regex translator** (`opendaisugi.regex_to_z3`). Handles
  literals, character classes `[a-z]` / `[^…]`, alternation, `*` `+` `?`
  `{n,m}`, groups, category escapes `\d` / `\w` / `\s`, and start/end
  anchors. Raises `UnsupportedRegexError` on lookaround, backreferences,
  inline flags `(?i)` / `(?m)`, and word boundaries `\b` / `\B`; callers
  see the fallback rather than silent approval.
- **`envelope_subsumes(outer, inner)`** — the first operation that
  genuinely requires a symbolic solver. Proves, via Z3 entailment, that
  every ActionStep the inner envelope admits is also admitted by the
  outer. When the proof fails, Z3 hands back a concrete ShellStep (the
  specific command the callee could emit that the caller's envelope
  forbids). Opaque invariants (those without an `expr`) are surfaced
  in `unverified_invariants` rather than silently approving.
- **`Contract` + `verify_delegation`** — skills as contracts between
  agents. Delegation is safe iff the caller's envelope subsumes the
  skill's contracted envelope; Z3 provides the proof or the
  counterexample. The signature field is API-stable in v0.11.0;
  cryptographic verification ships in v0.12.0 behind a `[sign]` extra.
- **`examples/delegation_demo.py`** — end-to-end demo: orchestrator
  delegates successfully to a narrow echo skill, then fails to delegate
  to a wider shell-runner skill and sees the counterexample `rm`
  command.
- **README thesis paragraph now matches the code.** The claim is the
  same; the implementation has caught up to it.

**Breaking**: none. `compile_to_z3` now returns a `CompiledPredicate`
dataclass (`.term` is the Z3 `BoolRef`, `.variables` the free-variable
registry, `.soft_nodes` lists LLMCheck / unsupported-regex bailouts).
Existing callers that only used the returned `BoolRef` should access
`.term`; Z3 simplification on ground predicates still produces the same
truth values.

## v0.10.0 — 2026-04-19 — Integration scaffolding

**Highlights**
- **Hermes adapter** — `opendaisugi.integrations.hermes` exposes
  `envelope_from_yaml`, `verify_plan`, `verify_step`, and
  `load_household_aliases` so Python-native agent frameworks (Hermes,
  home-grown skill runners) can call openDaisugi directly without
  re-implementing the YAML loader or alias resolution.
- **MCP `verify_completed_step` tool** — Stage 2 post-execution
  verification is now reachable over MCP, matching the existing
  Stage 1 `verify_plan` coverage. OpenClaw, Claude Code, and any
  MCP-speaking consumer can use runtime-assurance as both a pre-
  execution and post-execution gate.
- **OpenClaw Node client example** — `examples/integrations/openclaw/`
  ships an ESM Node.js client (`OpenDaisugiClient`) that spawns
  `daisugi mcp serve` and calls the three verification tools. A
  Python-side contract test covers the wire protocol without
  requiring Node in CI.
- **LoRA QLoRA trainer** — `python -m opendaisugi.lora.train` consumes
  the JSONL emitted by `opendaisugi.lora.dataset.emit_jsonl` and
  produces a standard PEFT LoRA adapter. Heavy deps (torch, peft,
  trl, bitsandbytes) are lazy-imported; the module imports on
  non-GPU machines.
- **MuJoCo envelope smoke kit** — `examples/integrations/mujoco/smoke.py`
  closes the envelope/executor loop: declared joint limits in the
  envelope, real `mj_step` rollout, post-rollout asserts that the
  actual `qpos` stayed inside the declared bounds.
- **`docs/integrations.md`** — consolidated reference for all four
  entry points with install + usage snippets.

**Schema additions**
- None. v0.10.0 is pure integration plumbing; no predicate algebra,
  envelope, or step-type changes.

**Compatibility**
- Fully compatible with v0.9.0 envelopes and plans. MCP clients that
  didn't depend on a frozen tool list will pick up
  `verify_completed_step` automatically.

## v0.9.0 — 2026-04-18 — Meta-DSL & Stage 2 verification

**Highlights**
- Predicate algebra DSL: agents author invariants and postconditions as
  composable expression trees (equals, in_set, forall_steps, implies, …)
  compiled to Z3. Closes the "silent fail-open on unknown invariant
  types" gap.
- Three-tier named aliases (system / household / envelope) with cycle
  detection and a static plan-path-reference vacuity check.
- Stage 2 output verification in Supervisor: envelope postconditions
  re-run over execution-completed steps before effect-commit — closing
  the perception gap between decided plan and executed effect.
- `llm_check` primitive for fuzzy constraints on non-physical stakes
  (blocked for robotics).
- Open `metadata: dict[str, Any]` bag on all step types — LLM-authored
  semantic fields get carried without schema changes.
- Demo kits: an impersonation-rejection kit and
  `examples/council-kit/` (pre/post-approval structural gates).

**Schema additions**
- `Invariant` and `Postcondition` gain `expr` (predicate tree) and
  `enforce` (bool) fields. `expr=None` means "no predicate check for
  this invariant"; typed-shape invariants (e.g. robotics
  `velocity_bounded`) still dispatch through `z3_checks` for numerical
  trajectory checks that the predicate algebra cannot express.
- All step types now carry `metadata: dict[str, Any]` — any code
  comparing `step.model_dump()` against a literal dict must accommodate
  the extra key.

**Thesis framings**
- opendaisugi is a restricted predicate algebra authorable by agents,
  compiling down to SMT-LIB2 that Z3 solves.
- We verify plans authored by LLMs at runtime.

## [0.8.0] — 2026-04-18

### Added
- **Robotics step types.** Four new members of the `ActionStep`
  discriminated union — `SimulationResetStep`, `JointMoveStep`,
  `CartesianMoveStep`, `GripperStep`. They verify, journal, and
  export through the same pipeline as shell/file/network steps.
- **Permission extensions for kinematic envelopes.** Five optional
  fields — `workspace_bounds` (end-effector AABB), `obstacles` (list
  of AABBs the trajectory must avoid), `velocity_limit` (peak joint
  rad/s), `joint_limits` (per-joint radian ranges), and `torque_limit`
  (peak `|actuator_force|`). Envelopes without any of them behave
  exactly as before.
- **Four new invariant handlers** — `end_effector_in_workspace`,
  `no_obstacle_penetration`, `velocity_bounded`, and
  `joint_limits_respected`. Z3-backed where analytically tractable,
  Python AABB membership where not. Unknown invariant types remain
  documentation (never flagged).
- **`MuJoCoExecutor`** (in `opendaisugi.executor_mujoco`) — physics-
  backed executor for all four robot step kinds. Damped-least-squares
  IK resolves `CartesianMoveStep.target_position` → joint targets
  using `mj_jacBody` on a scratch `MjData`. A shared `MjData` across
  the session means `joint_move` after `sim_reset` sees the reset
  state, and `cartesian_move` after a close-gripper keeps the gripper
  closed.
- **Rollout-time guards.** After every `mj_step`, peak actuator force
  is compared against `torque_limit` (rc=3, `RC_TORQUE_VIOLATION`)
  and the contact count is checked against `forbid_contacts` (rc=4,
  `RC_CONTACT_VIOLATION`). IK non-convergence surfaces as rc=5
  (`RC_IK_FAILED`). Failures land in the run journal with the step
  id so the refinement loop can tighten the envelope.
- **`robotics_executors(mjcf_path, **kw)` factory** — returns a
  `dict[str, StepExecutor]` with all four robot kinds wired to a
  single `MuJoCoExecutor` instance, so session state persists across
  steps.
- **`Supervisor.run()` calls `executor.configure_from_envelope(env)`**
  on any executor that implements it (duck-typed). `MuJoCoExecutor`
  uses this to surface `permissions.torque_limit` → `executor.torque_limit`
  and non-empty `permissions.obstacles` → `executor.forbid_contacts = True`.
  Shared executors are deduplicated by `id()` so configure fires once.
- **New `[robotics]` extra** — `pip install 'opendaisugi[robotics]'`
  installs `mujoco>=3.0,<4.0` and `numpy>=1.24`. The base install does
  not depend on MuJoCo — robotics support degrades gracefully to
  validation only when the extra is absent.
- **MuJoCoExecutor is lazy-imported** via module `__getattr__` so the
  default `import opendaisugi` path never touches `mujoco` or `numpy`.

### Documentation
- `docs/robotics.md` — full guide: install extra, step types,
  permission fields, invariants, `robotics_executors` usage,
  envelope → executor wiring, rollout rc codes, MJCF conventions
  (`a_grip` actuator prefix, `end_effector` body name,
  `<compiler angle="radian"/>`), portability.
- README Mode D section updated from v0.3 target to shipping v0.8
  description with install and doc pointers.

## [0.7.0] — 2026-04-18

### Added
- **Pathway portability pipeline** — compiled pathways can now be
  exported and re-imported as files, so they distribute through any
  skill-sharing mechanism (Claude Code plugins, Hermes skill
  collections, OpenClaw skill directories, plain git repos).
- **`daisugi pathways export <id> <path> --format X`** — five formats:
  - `json` — canonical bundle, lossless round-trip.
  - `skill` — markdown + YAML frontmatter (Claude Code / Hermes /
    OpenClaw compatible). Body is human/LLM docs; frontmatter
    carries the JSON bundle under a `daisugi:` key.
  - `mermaid` — plan DAG as a Mermaid flowchart + permission summary.
  - `md` — human-readable audit report.
  - `smtlib` — SMT-LIB2 proof artifact. Third parties can run
    `z3 pathway.smt2` to independently confirm verification without
    installing openDaisugi.
- **`daisugi pathways import <path>`** — auto-detects JSON vs skill
  markdown. Re-runs Z3 verification against the declared envelope
  before admitting the pathway to the local store. Structured error
  codes: `SCHEMA_INCOMPATIBLE`, `VERIFICATION_FAILED`, `DUPLICATE_ID`.
  `--overwrite` flag for replacing existing IDs.
- **`opendaisugi.portability` module** — public API:
  `export_pathway(pathway, fmt)`, `import_pathway(path, store)`,
  `parse_bundle(text)`, `PathwayImportError`, `BUNDLE_SCHEMA_VERSION`.

### Documentation
- `docs/pathway-skill-format.md` — full spec for the skill-file
  format, consumer integration notes for Claude Code / Hermes /
  OpenClaw / MCP-speaking agents, and versioning rules.
- README gains a v0.7 section with export/import examples.

## [0.6.0] — 2026-04-18

### Added
- **MCP server.** New `daisugi mcp serve` command spins up a
  FastMCP stdio server so Claude Code, OpenClaw, or any MCP
  client can call openDaisugi primitives as tools. Five tools:
  `envelope_for`, `find_pathway`, `verify_plan`, `list_pathways`,
  `pathway_stats`. Uses the official `mcp` SDK's `FastMCP` (no
  framework lock-in, moves with the spec).
- **New `[mcp]` extra** — `pip install 'opendaisugi[mcp]'`.
  Without it, `daisugi mcp serve` exits 1 with a hint pointing
  at the install command.
- `opendaisugi.mcp_server.build_server(daisugi)` — exposed so
  callers embedding openDaisugi in a larger MCP surface can
  register the tools alongside their own.

### Documentation
- README gains a v0.6 section with a Claude Code MCP config
  snippet + note on OpenClaw integration.

## [0.5.0] — 2026-04-17

### Added
- **LoRA training-data pipeline.** New `opendaisugi.lora` package
  scans the journal for successful `(task → envelope)` pairs and
  emits them as JSONL in standard fine-tuning formats.
  - `iter_training_examples(journal, *, since, min_task_chars)` —
    streaming generator over successful traces, filters short or
    stub tasks.
  - `emit_jsonl(journal, output_path, *, format, ...)` — writes
    JSONL and returns a `DatasetStats` summary.
  - `TrainingExample.to_alpaca()` and `to_chat(system_prompt=...)`
    — emit the two most common SFT formats.
- **`daisugi lora export PATH [--format alpaca|chat] [--days N]
  [--min-task-chars N] [--system-prompt STR]`** — CLI wrapper.
  Emits a JSON stats summary to stdout; unknown formats exit 2.

### Documentation
- README gains a v0.5 section with a reference SFTTrainer + PEFT
  recipe for running the fine-tune outside the library.
- Two stale "v0.4+" lines in the v0.3 Limitations section updated
  to reflect that the Gardener now covers iterative improvement.

### Changed (breaking)
- **`CompiledPathway.pitfalls` and `validation_score` removed.**
  Both fields were write-only after distillation — pitfalls got fed
  to the generalization prompt and then persisted dead; validation
  score was display-only, never consulted by runtime routing. The
  SQLite schema drops the columns in place on first open via an
  additive `_DROPPED_COLUMNS` migration, so legacy v0.3/v0.4 stores
  upgrade transparently rather than failing INSERT.
- **`NullTier1Provider` removed.** The declining adapter was dead
  weight — passing `tier1_provider=None` to `generate_envelope`
  already short-circuits the routing, no sentinel needed.

### Changed (internal)
- `cosine_similarity` and `cosine_similarity_batch` extracted to a
  shared `_similarity` module — previously duplicated across
  `_search`, `pathway_store`, and `gardener.merger`.
- `adapt_plan` moved from `Daisugi.adapt_plan` into the distiller
  module as a free function; the method stays as a thin wrapper.
- Defensive underscore-prefixed aliases for `generate_envelope` and
  `verify` in `cli.py` dropped.

### Deferred
- Robotics adapters (v0.6.x+).
- Hardware-in-the-loop verification (v0.6.x+).
- Cross-project pathway sharing (unscheduled).

## [0.4.1] — 2026-04-17

### Added
- **`daisugi gardener watch`** — cron-friendly one-shot scheduler.
  Writes `.gardener-last-run` in the data dir and skips if the last
  run is newer than `--min-interval` (default 1 hour). Designed to
  be invoked from cron every few minutes regardless; the gate
  enforces real interval. Emits a single JSON line per invocation.

### Changed
- **`opendaisugi.permissions.intersect_permissions`** is now the
  canonical home for the Permission-intersection helper. The
  distiller's `_intersect_permissions` still re-exports for
  backward compat — no caller-visible change.

## [0.4.0] — 2026-04-17

### Added
- **Tier-1 pluggable local-model routing.** `generate_envelope` now
  routes between Tier 0 (compiled pathway), Tier 1 (cheap local model),
  and Tier 2 (frontier ladder). The new `Tier1Provider` protocol
  decouples routing from any specific backend.
- **Three Tier-1 adapters ship out of the box:**
  - `NullTier1Provider` — always declines (default, preserves v0.3
    behavior).
  - `LiteLLMTier1Provider(model, base_url=..., api_key=...)` — any
    OpenAI-compat endpoint (Ollama, llamafile, llama.cpp HTTP server,
    Anthropic Haiku, vLLM, etc.). Uses `litellm` + `instructor`.
  - `ClaudeCodeTier1Provider(binary="claude", model_flag="haiku")` —
    shells out to the Claude Code CLI via `asyncio.create_subprocess_exec`.
- **Adapter-failure-returns-None invariant:** any exception, timeout,
  or malformed output in a Tier-1 adapter falls through to the Tier-2
  ladder. `stakes="high"` bypasses Tier-1 entirely.
- **Tier-1 results go through `check_envelope_self_consistency`** before
  being cached — Z3 failure short-circuits to Tier-2.
- **Envelope cache is tier-aware:** `make_cache_key` gained an optional
  `tier1_provider_name` so two providers cache separately. When the
  arg is None, cache keys are byte-identical to v0.3.x.
- **Token accounting:** `opendaisugi.accounting.tier_stats(journal)`
  buckets traces by tier from `envelope.generated_by` (no new journal
  schema). `daisugi tiers stats [--days N] [--json]` exposes it.
- **Gardener lifecycle management** (v0.4.0). `opendaisugi.gardener`
  package exposes:
  - `prune(store, PruneConfig)` — evicts stale / failure-dominated
    pathways with a grace period for newly distilled pathways.
  - `merge(store, MergeConfig)` — collapses near-duplicate pathways by
    cosine similarity + permission compatibility.
  - `ab_test(pathway, task, tier2_generator=...)` — compares compiled
    pathway against a fresh Tier-2 generation; injectable generator
    keeps tests (and cost-conscious production) off the frontier.
  - `regression_check(pathway_id, ab_history)` — splits A/B history
    into historical vs recent, emits a `RegressionAlert` on material
    pass-rate drops.
  - `run_gardener(store, GardenerConfig)` — composes prune + merge
    into one pipeline.
- **`daisugi gardener`** sub-typer: `prune`, `merge`, `run`, `status`.
  All mutating commands support `--dry-run`; listing commands support
  `--json`.
- **Pathway lifecycle fields** — `CompiledPathway.last_activation_at`
  and `failure_count`. Older DBs auto-migrate via the existing
  `_ADDITIVE_COLUMNS` mechanism.
- **`PathwayStore.mark_failure(id)`** — bump a pathway's failure
  counter (used by the A/B harness and regression pass).

### Deferred
- LoRA fine-tunes from journal traces (v0.5.x+).
- Robotics adapters (v0.5.x+).
- Hardware-in-the-loop verification (v0.6.x+).

## [0.3.1] — 2026-04-17

### Fixed
- **`PathwayStore.find()` gracefully degrades** when the `[search]` extra
  is missing — returns `None` with a one-time warning instead of
  propagating `ImportError`. Reconciles README's graceful-degradation
  claim with actual behavior.
- **`PathwayStore.find()` short-circuits on an empty table** before
  importing sentence-transformers — fresh installs with no pathways no
  longer trigger an ~80MB model download on first lookup.
- **`Daisugi.find_pathway()` offloads to `asyncio.to_thread`.** The
  underlying SQLite + numpy + sentence-transformers work is synchronous;
  calling it from async code was blocking the event loop.
- **`daisugi tend --dry-run` uses `PathwayStore(":memory:")`** instead
  of writing a `.tend-dryrun.db` sidecar into the user's data dir.
- **`Distiller.tend()` skips corrupt-YAML traces** via per-trace
  try/except instead of dying on the first parse error.
- **Cluster centroid reused** for `task_embedding` instead of
  re-embedding every cluster member — halves embedding calls in the
  common path.
- **Deferred `sentence_transformers` + `numpy` imports in `_search`**
  so modules that only need `_MODEL_NAME` don't pay the torch cost.

### Added
- **Embedding provenance** — `CompiledPathway.embedding_model` and
  `embedding_model_version` fields stamp every fresh distillation.
  Older DBs auto-migrate via additive `ALTER TABLE` on open.
- **`daisugi pathways stats`** CLI command — count, total hits, avg
  validation score, with optional `--json`.
- **`TendReport.warnings`** now includes a below-`min_traces`
  explanation when zero pathways are created.
- **Structured `_log.info` in `tend()`** with embed/cluster timings
  and cluster sizes.
- **Pitfalls cap** — each pathway's `pitfalls` list is capped at 20
  entries with a truncation marker to bound prompt size.
- README migration note from v0.2 → v0.3 (no breaking changes).

## [0.3.0] — 2026-04-16

### Added
- **Compiled pathways** — the offline tier of the two-tier self-improvement
  architecture. `Distiller` (in `opendaisugi.distiller`) scans successful
  journal traces, clusters them by task-embedding similarity, intersects
  envelope permissions, and produces `CompiledPathway` artifacts stored in
  `PathwayStore`.
- `daisugi tend` CLI — runs the distiller. Flags: `--min-traces`,
  `--lookback-days`, `--model`, `--dry-run`.
- `daisugi pathways list | show <id> | delete <id>` CLI — manage the store.
- `Daisugi.tend()`, `Daisugi.find_pathway(task)`, `Daisugi.adapt_plan(match, task)` —
  facade API for the full distill/consume/adapt loop.
- `generate_envelope()` now checks the pathway store before the cache scan
  (when a `pathway_store` is configured). Hits return a compiled envelope
  tagged `generated_by="compiled-pathway:<id>"` and bypass the LLM call
  entirely.
- `Journal.list_successful_traces(since=...)` + `DistillableTrace` —
  lightweight query for the distiller.
- Public exports: `CompiledPathway`, `PathwayMatch`, `PathwayStore`,
  `Distiller`, `TendReport`.

### Changed
- `Daisugi.__init__` gains `pathway_store: bool | PathwayStore = True`.
  Default auto-constructs at `data_dir/pathways.db` (lazy). Pass
  `pathway_store=False` to opt out.

### Limitations
- **One improvement pass per cluster.** If validation score stays low after
  one LLM retry, the pathway is still stored with that score. Multi-iteration
  train/eval/improve loops (like skill-creator's `run_loop.py`) land post-v0.3.
- **Local pathways only.** No export/import across data directories or
  projects. Cross-project pathway sharing is v0.4+.
- **No agentskills.io export.** `.skill` zip generation is out of scope.
- **Distillation is batch-only.** `generate_envelope()` never distills;
  only `daisugi tend` does.
- **`[search]` extra required.** `sentence-transformers` + `numpy` are soft
  prerequisites for `daisugi tend` and pathway matching.

## [0.2.1] — 2026-04-16

### Added
- **Refinement-aware envelope generation.** `generate_envelope()` now queries
  the journal for past rejections (by envelope cache key) and injects a
  "Prior Rejections" hint block into the user prompt so the LLM can tighten
  the new envelope.
- **Cache-bust on stale entries.** Cached envelopes whose insertion
  timestamp predates the newest matching refinement are invalidated and
  regenerated with hints.
- `Envelope.cache_key` — optional field stamped by `generate_envelope()` at
  creation time. `None` for hand-built envelopes.
- `RefinementRecord.cache_key` — optional field; Supervisor copies
  `envelope.cache_key` into every record it writes.
- `Journal.get_refinements_by_key(cache_key)` — fast indexed lookup. Ignores
  records with `NULL` cache_key.
- `EnvelopeCache.get_inserted_at(cache_key)` and `EnvelopeCache.invalidate(cache_key)`
  — primitives for staleness checks.
- `make_cache_key(...)` promoted from private `_make_cache_key` to the
  public API. Same signature; previous code keeps working via alias.
- `generate_envelope(..., journal=None)` — optional parameter for callers
  that don't go through the `Daisugi` facade.

### Changed
- `ENVELOPE_PROMPT_VERSION` bumped to `2026-04-16` because the embedded
  `Envelope` schema gained `cache_key`. Existing cached entries are evicted
  on next cache construction.
- `Daisugi.generate_envelope()` now threads `self.journal` into the module
  function, which lazy-instantiates the journal on first envelope generation.

### Limitations
- Exact cache-key match only. Semantically similar tasks with different
  exact strings do not share refinements — fuzzy matching lands in v0.3.0
  (compiled pathways with embedding retrieval).
- No programmatic envelope tightening. The LLM decides how to respond to
  hints; no rule-based permission intersection.
- Plan improvements are out of scope — v0.2.1 improves envelopes, not plans.

## [0.2.0] - 2026-04-16

### Added
- **Simplex fallback architecture** (Sha 1996 RTA): when `verify()` rejects a step during supervised execution,
  the `Supervisor` delegates to a pluggable `FallbackHandler` — halt (default) or recompute via LLM.
- `FallbackHandler` protocol + `HaltHandler` (unconditional halt) + `RecomputeHandler` (LLM re-plans the step,
  verifies replacement, halts if replacement also fails).
- `FallbackOutcome` data type — what the handler decided (`"halted"` or `"recomputed"` + replacement).
- `RefinementRecord` + `RefinementLog` data types — structured CEGAR-inspired refinement records capturing
  violations, Z3 counterexamples, and fallback outcomes.
- `Journal.write_refinement()` + `Journal.get_refinements()` — SQLite-backed refinement persistence per session.
- `RunStatus.HALTED_BY_SIMPLEX` — new terminal status for runs halted by the simplex fallback.
- `StepOutcome` gains `"rejected_halted"` and `"rejected_recomputed"` status values.
- `Supervisor` constructor accepts `fallback: FallbackHandler | None` for handler injection. Auto-selects from
  envelope's `FallbackStrategy` when `None`.
- Per-step verification: each step is verified individually before execution (in addition to up-front plan verify).

### Changed
- `Supervisor.run()` is now `async` — callers must `await` it. The CLI handles this transparently.
- `Supervisor` default fallback is `HaltHandler()` — conservative RTA default.

### Limitations
- `RecomputeHandler` gets one shot — if the replacement also fails verification, it halts. No retry loop.
- Z3 counterexample extraction in `RefinementRecord` is always `None` in v0.2.0 (Z3 model extraction is a v0.2.1 enhancement).
- Refinement records are per-session; cross-session aggregation is deferred to v0.3+.
- Refinement does not feed back into envelope improvement — v0.3+ distillation will close this loop.

## [0.1.3] - 2026-04-15

### Added
- `stakes={"low","medium","high"}` kwarg on `generate_envelope` and `Daisugi.generate_envelope`.
  - `low` returns a caller-configured envelope without an LLM call.
  - `medium` preserves current cache-then-LLM behavior (default).
  - `high` bypasses cache read and overwrites on write.
- `DEFAULT_LOW_STAKES_ENVELOPE` constant (in `opendaisugi.defaults`) — permissive sandbox-grade envelope.
- `Daisugi.with_default_low_stakes(**kwargs)` classmethod — opt-in facade preloaded with the default.
- `model: str | list[str]` on `generate_envelope` — list form is an escalation ladder. Escalation fires on
  instructor parse exhaustion or Z3 self-consistency violation. Cache keys on the successful rung.
- `thinking_budget={"light","standard","deep"}` kwarg, mapped per provider
  (Anthropic Claude, OpenAI o-series, Gemini thinking-capable). Unsupported providers log a one-time WARNING.
- New exceptions: `LowStakesNotConfigured`, `ModelLadderExhausted`, `StakesInheritanceWarning`.
- New modules: `opendaisugi.defaults`, `opendaisugi.thinking`.
- CLI: `daisugi generate-envelope` accepts `--stakes`, `--low-stakes-envelope FILE`, `--thinking-budget`.

### Changed
- `EnvelopeCache` key composition now includes `thinking_budget`; budgets produce distinct cache entries.
- `Daisugi(data_dir=...)` accepts `str | os.PathLike | None` (previously `Path | None`).

### Limitations
- Per-tier thinking budgets (e.g., `[(sonnet,light),(opus,deep)]`) are not supported; budget is uniform
  across ladder rungs.
- Stakes are not inferred from task text — callers pass `stakes=` explicitly.
- `stakes="low"` + `parent=` is incoherent; the library warns (`StakesInheritanceWarning`) and ignores parent.

## [0.1.2] — 2026-04-15

### Added
- Envelope cache: `EnvelopeCache` SQLite-backed cache, content-addressable on (task, context, model, parent_envelope_id, summarize). Auto-evicts on `prompt_version` mismatch.
- Parent-envelope inheritance: `generate_envelope(..., parent=...)` stamps `parent_envelope` and verifies child permissions are a tightening of parent. New `verify_inheritance` pure function and `EnvelopeInheritanceError`.
- `summarize=True` flag on `generate_envelope` populates `Envelope.summary` (≤80 chars).
- `Daisugi(cache=True)` (default) auto-constructs an envelope cache at `<data_dir>/envelope_cache.db`. Pass `cache=False` to opt out, or inject a custom `EnvelopeCache` instance.
- `ENVELOPE_PROMPT_VERSION` module constant in `envelope.py` — bump on prompt body edits to invalidate cached envelopes.

### Limitations
- Inheritance enforces strict string-set subset on glob lists (`/tmp/foo.txt` is not recognized as a subset of `/tmp/**`). Documented; semantic subsumption may land in v0.1.3+.
- Inheritance is depth-1 only; envelope chains beyond a single parent are rejected.
- No cache TTL — entries live until prompt version changes or `EnvelopeCache.clear()` is called.
- No CLI for cache management; `Daisugi.cache.stats()` / `.clear()` are the library API.
- Default `Daisugi()` cache lives at `~/.opendaisugi/envelope_cache.db` and is shared across processes/tests. Pass `data_dir=...` (or `cache=False`) for isolation.

### Security
- Cache stores envelope JSON only — no LLM API keys, no command output.

## [0.1.1] — 2026-04-14

### Added
- First-class step kinds: `FileReadStep`, `FileWriteStep`, `NetworkStep` join `ShellStep` as discriminated-union members of `ActionStep`.
- Executor registry: `default_executors()` factory wires the standard set; `Supervisor(executors=...)` accepts `dict[str, StepExecutor]` for per-kind dispatch.
- `FileWriteExecutor`: atomic write (tempfile + fsync + rename), auto-creates parent dirs, `O_NOFOLLOW` on tempfile, post-rename realpath escape check.
- `NetworkExecutor`: stdlib `urllib.request`, GET-only, no automatic redirect following, response-size cap, timeout.
- `Permission.network_hosts: list[str]` — network host allowlist for `NetworkStep`. Empty list = any host (preserves v0.1.0 semantics). Matching is strict and case-insensitive on host.

### Changed
- Step model is now a Pydantic discriminated union keyed on `type`. Construct the kind-specific class directly (`ShellStep(...)`, `FileReadStep(...)`, `FileWriteStep(...)`, `NetworkStep(...)`); the flat `ActionStep(type=..., ...)` constructor is gone.
- `verify` dispatches per-kind against the typed union; field accesses are unambiguous without defensive fallbacks.
- `DryRunExecutor` and `FakeExecutor` are kind-aware: dry-run messages and fake-result keys vary by step kind.

### Removed
- `Supervisor(executor=...)` singleton kwarg — replaced by `executors: dict[str, StepExecutor]`. Pass `Supervisor()` (uses defaults) or `Supervisor(executors=default_executors())`.
- Catch-all "unknown step type" branch in `verify.check_permissions` — unreachable now that the discriminated union rejects unknown kinds at parse time.
- Defensive `or ""` idioms on step field access throughout verify — the typed step guarantees its fields.

### Security
- File-write step is protected against symlink-at-target and symlink-swap-after-verify: tempfile opens with `O_NOFOLLOW`, and after `os.rename` the final path is resolved with `os.path.realpath` and checked against the envelope's allowed `file_write` glob. Escape attempts are deleted and return `rc=2`.
- Network step does not follow redirects: a custom `HTTPRedirectHandler` returns `None`, so 3xx responses surface as `HTTPError` rather than silently chasing to a possibly-disallowed host.

## 0.1.0 — 2026-04-14

### Added
- `Supervisor` class: executes verified ActionPlans step-by-step under live monitoring
- `StepExecutor` protocol + `SubprocessExecutor` (process-group teardown, SIGTERM→SIGKILL), `DryRunExecutor`, `FakeExecutor`
- `ApprovalStrategy` protocol + `AllowlistBypassStrategy`, `TtyPromptStrategy`, `EnvVarStrategy`, `CallbackStrategy`, `DenyStrategy` + `default_strategy()` stack
- `RunSession`, `RunStatus`, `StepOutcome` types
- `Journal.log_run()` + `Journal.load_run()` with schema v2 migration (adds `run_id`, `run_status`, `failed_step_id`, `total_duration_ms` columns)
- `daisugi run` CLI subcommand with `--dry-run`, `--yes`, `--json`
- `Config` fields: `step_timeout_s`, `execution_timeout_s`, `approval_policy`, `max_output_bytes`
- Public API re-exports for all of the above

### Deferred to v0.1.5
- Postcondition verification (filesystem probes)
- Simplex fallback / Tier 2 recompute
- CEGAR-inspired refinement log
- `file_write`, `file_read`, `network` step executors

### Deferred to v0.2
- Envelope caching and similarity retrieval
- Hermes / OpenClaw integration packages
- `RunSession` persistence / resume

## 0.0.4 — 2026-04-12
Security hardening: path traversal normalization, shell metacharacter detection, trace_id validation, type hardening in boundary validation.

## 0.0.3 — 2026-04-11
Clean harvest: bug fixes (SQLite transaction collapse, numpy extras, top-of-file imports).

## 0.0.2 — 2026-04-10
Journal bootstrap: YAML+SQLite trace store, `daisugi journal` subcommands.

## 0.0.1 — 2026-04-09
Initial release: Envelope, ActionPlan, `verify()`, `generate_envelope()`, `Daisugi` facade.

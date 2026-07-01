# Security model

What opendaisugi protects against, what it does not, and where the trust
boundaries run. This document is input for your team's own risk
decisions — the library makes no certification or compliance claims, and
adopting it is not a substitute for a security review of the system it
runs inside.

If you already read `docs/concepts.md` and `docs/limitations.md`, the
new material here is the trust-boundary and data-handling breakdown.

## What the library protects against

**Unsafe agent-authored plans.** The core thesis: an LLM emits an
`ActionPlan`, and before any step executes, the library proves against a
declared `Envelope` that no step can violate the policy. If the proof
fails, the plan is rejected with a concrete counterexample (the step
that would violate the policy, and the specific invariant it breaks).
See `docs/concepts.md` for the predicate algebra and the Z3 pipeline.

**Unsafe delegation between agents.** When one agent delegates to a
skill or sub-agent published as a `Contract`, the library proves that
the contract's envelope is subsumed by the caller's envelope. A
subsumption failure is returned with a concrete step that the sub-agent
could legally produce but the caller would reject. See
`src/opendaisugi/contracts.py`.

**Tampered contracts** (v0.15+). Contracts signed with ed25519 and
verified against a `TrustedSignerRegistry` cannot be modified in transit
without breaking the signature. Verification fails closed: an unknown
signer, a missing signature, or a modified body all evaluate to "not
trusted." See `src/opendaisugi/signing.py`.

**Runtime drift from proved plan to executed plan.** The `Supervisor`
re-verifies every step immediately before executing it, under the same
envelope as the whole-plan proof. If a step was rewritten between proof
and execution (e.g. by a refinement loop), it is re-proved.

**Auditability of agent decisions.** Every run produces a `RunSession`
in the journal with the envelope id, plan id, verification result, and
per-step outcome. Logs emit structured records at every decision
boundary. The journal plus logs give a reconstructible trail of why a
given plan ran or didn't.

## Strict mode and vacuity detection (v0.27.0)

Two classes of subtle but serious bugs previously let verification degrade to
safety-theater without a visible error:

**Opaque invariants at high stakes.** Before v0.27.0, an invariant with
`expr=None` and `enforce=True` at `stakes="high"` was silently skipped. Any
safety property you declared but forgot to give an expression was effectively
unenforced. v0.27.0 closes this with strict mode: at `stakes` `"high"` or
`"physical"`, such invariants are rejected with a concrete `Violation` and a
remediation hint. The four recognized robotics types (`end_effector_in_workspace`,
`joint_limits_respected`, `velocity_bounded`, `no_obstacle_penetration`) are
carved out — they are discharged by dedicated symbolic handlers in
`z3_checks.py`, not the predicate algebra. Opt out per invariant with
`enforce=False`, or per call with `verify(..., strict=False)`.

**Vacuous constraints.** A tautological predicate (always true — constrains
nothing) accepts every plan unconditionally; a contradictory predicate (always
false — satisfiable by nothing) blocks every plan and makes the envelope
unusable. Both are bugs, but they were previously silent. v0.27.0 uses Z3 to
detect them: `check_vacuity(expr)` returns `"tautology"`, `"contradiction"`,
or `"non_trivial"`. Contradictions are hard errors at all stakes levels;
tautologies are violations under strict mode and warnings otherwise. Alias
registration also runs vacuity check — a tautological or contradictory alias
raises `VacuousAliasError` at register time before it can propagate.

**LLM-check fail-open.** Before v0.27.0, a network error or timeout in the
`llm_check` call path could resolve to a soft-node pass. Now it fails closed:
any exception returns `satisfied=False, errored=True`.

## What the library does not protect against

**OS-level escape.** opendaisugi enforces policy at the Python level.
It is not a sandbox, not a seccomp profile, not a hypervisor. Native
code that breaks out of the Python process is outside its scope. Run
untrusted agents under an OS-level sandbox in addition, not instead.

**Natural-language output fidelity.** The library verifies the plan,
not the agent's narration of the plan. If an agent reads a file and
describes it in prose, opendaisugi cannot prove the description
faithfully reflects the file.

**Goals the envelope didn't forbid.** An envelope is only as tight as
the invariants the author wrote. A plan that satisfies a too-loose
envelope is proved safe *under that envelope* — the library does not
invent invariants the author didn't declare.

**Inputs beyond the plan.** The library reasons about structured
`ActionStep` objects. It does not inspect arbitrary Python code, shell
scripts loaded from disk, or the contents of files the plan reads. A
step that reads `/etc/passwd` is a `file_read` with `path="/etc/passwd"`
— the library checks that path against the envelope's allowlist; it
does not read the file itself.

**Supply-chain risk in optional extras.** The `[lora]`, `[robotics]`,
and `[search]` extras pull large third-party packages (torch, mujoco,
sentence-transformers). These are not vendored; their security posture
is whatever the upstream project's is. The core package's dependency
surface is intentionally small (pydantic + z3-solver + litellm) — prefer
running without extras in production-adjacent contexts.

For a broader list of library limits, see `docs/limitations.md`.

## Trust boundaries

Four boundaries matter in practice.

**1. Envelope author → library.** The envelope is trusted input. A
caller who writes a permissive envelope is authorizing permissive
behavior. The library does not second-guess the envelope; it proves
plans against it.

**2. Caller envelope → sub-agent contract.** When delegating, the
caller's envelope is the trust root. The contract's envelope is
unverified input until subsumption proves it fits. The
`unverified_invariants` field on `SubsumptionResult` surfaces any
contract clause the library could not symbolically reason about —
callers who want "no soft clauses" can reject any delegation whose
`unverified_invariants` is non-empty.

**3. Signer registry → contract signature.** The public keys in
`trusted_signers.json` are the cryptographic trust root for contracts.
Whoever controls that file controls which contracts the library
accepts. Protect it accordingly (file permissions, configuration
management, integrity monitoring).

**4. Plan → executor.** The `Supervisor` hands off verified steps to
pluggable executors (`StepExecutor` implementations). The library
verifies the plan; the executor runs it. A misbehaving executor
(e.g. one that ignores the step.command and runs something else) is
outside the library's control — write or vet your own executor if
you're running in a hostile environment.

## Data handling

**Written to disk** (in the configured data directory only):

- Envelope cache (SQLite) — envelopes keyed by a hash of the task
  string; task strings themselves are not persisted.
- Pathway store (SQLite) — compiled pathways with an anonymized task
  fingerprint, a plan template, and an envelope.
- Journal (JSONL) — run history: envelope id, plan id, per-step status,
  duration, return code, stdout (subject to the
  `max_output_bytes` cap), timestamps, trace id.
- Refinement log (JSONL) — one record per plan rejection/recompute
  with the envelope id and counterexample step.
- Trusted-signer registry (JSON) — signer name → public key.

**Written to logs**:

- Envelope IDs, plan IDs, step IDs, run IDs, contract IDs.
- Violation stages and counts.
- Approval strategy identifiers (`"allowlist"`, `"tty"`, `"env"`,
  `"callback"`, `"denied"`).
- Signer names during verification.

**Not written** by the library:

- Raw envelope contents (invariant expressions are not logged, only
  identifiers).
- Task strings or prompts beyond the envelope cache key.
- File contents a plan reads — the library checks paths, not payloads.
- LLM API keys or authentication material.

Hosts that want richer auditing can add a logging handler and record
whatever additional fields they wish; the library's default emission is
deliberately identifier-heavy and payload-light.

## Incident response

When an agent does something surprising, the evidence is in two places.

The journal (`data_dir/journal/*.jsonl`) has one record per run. Each
record pins the envelope that was in force, the plan that was proved,
and the outcome of each step. `Daisugi().journal.replay(run_id)`
re-executes the verification against the current library (not the
action; the proof) so you can see whether the decision would still hold
today.

The logs (`opendaisugi.*` loggers) have the decision boundaries —
`run.rejected_by_verify` with the violation stages, `run.approval_denied`
with the reason, `delegation.deny` with the failing signer or
counterexample. Routing these to a durable sink is the closest thing
the library offers to "audit mode."

## Compliance posture

No certifications. No SOC 2 / ISO 27001 / FedRAMP claims. The library
is a building block; whether the system it runs inside meets a given
compliance regime is a question for the team that deploys it.

Features that typically help compliance reviews:

- Dependency surface is small and declared (see `pyproject.toml`).
- State lives in a single configurable directory.
- Logs are structured, routable, and do not capture PII by default.
- Cryptographic contract signing uses a standard primitive (ed25519)
  via the `cryptography` library's hazmat bindings.
- Verification is deterministic under a fixed Z3 version — the same
  envelope + plan returns the same decision.

Features that typically complicate compliance reviews:

- Optional extras (torch, mujoco) pull large upstream trees; we do not
  audit them. If running in a regulated environment, omit the extras
  you do not use and audit what you do.
- LLM calls through `litellm` or the Claude Code backend leave the
  process. Envelope generation, distillation, and plan adaptation are
  all LLM-gated and require an allowed egress path. Runtime
  verification does not call out.
- The library is MIT-licensed. See `LICENSE` and `pyproject.toml` for
  the authoritative grant.

## Supply-chain & reproducibility — why a stranger should run this (roadmap Stage 7)

A security layer asks for more trust than any other dependency: it sits
between an agent and everything the agent touches. The base answer is that the
code is open and small enough to read. Beyond reading the source, the checkable
trust surface — meant to be auditable in an afternoon:

- **Public CI, green on every push, with the adversarial suite as a required
  step.** `.github/workflows/ci.yml` runs lint + the full suite and an explicit
  *Adversarial merge gate* step (`tests/test_adversarial.py` + `daisugi gate
  audit`). Any attack the gate fails to deny fails the build.
- **Re-runnable, content-addressed evaluation.** The adversarial corpus is
  deterministic and carries a stable content address (`corpus_hash`); `daisugi
  gate audit` reproduces the published attack-denial and false-positive rates
  on anyone's machine, and a rerun is a rerun. Distilled pathway bundles are
  content-addressed too (`PathwayBundle`).
- **Pinned, small dependency surface.** Runtime deps are declared in
  `pyproject.toml`; optional extras (torch, mujoco, sentence-transformers) are
  opt-in and not pulled by the core.
- **Allowlist-based, commit-pinned model resolution.** Local/remote model
  resolution goes through a trusted-org allowlist with list-first lookup and
  commit pinning (`model_registry`) rather than fetching arbitrary refs.
- **No telemetry of any kind.** The library emits nothing over the network on
  its own behalf. The only egress is the LLM calls you configure (envelope
  generation, distillation, delegation); runtime *verification* and the
  call-time *gate* never call out.
- **What is and is not signed — stated honestly.** Distilled pathway bundles
  are cryptographically signed and verified against a trusted-signer registry
  (`opendaisugi.signing`, ed25519). **Release artifacts (the PyPI/sdist
  package) are not yet signed** — that is the open item on this stage; until it
  lands, install from a pinned git ref you have read, not from an unpinned
  index.

The one-line honest summary: everything that decides *allow/deny* is
deterministic, offline, content-addressed, and re-runnable by you; the trust
you must still extend is to the checker's own correctness (it is tested, not
machine-proven — [yellow paper §7](spec/yellow-paper.md)) and to the release
channel until artifact signing lands.

## License

MIT. See `LICENSE`. The license governs the code, not envelopes or
plans authored against it — running opendaisugi against a proprietary
envelope is not a derivative work of the library.

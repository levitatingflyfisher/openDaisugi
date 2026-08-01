# The neutral problem statement

*This is the brief the five blind designers worked from. It deliberately names no
existing system, no envelope, and no solver — it describes only the problem and the
requirements a good answer must meet. The whole experiment depends on this document
carrying no fingerprint of the thing it was testing for.*

## The problem

An LLM agent takes real actions in the world — runs shell commands, edits files,
calls tools, sends messages, spends money. Two things are simultaneously true and in
tension:

- **These agents are expensive.** Every turn is tokens; every call to a strong model
  is dollars. There is enormous pressure to do the same work with fewer tokens,
  fewer turns, and cheaper models.
- **These agents are unsafe to run blind.** A model can propose an action that is
  plausible and wrong, or plausible and catastrophic. The interesting case is the
  *rare, hard task* — done once or twice, with no history of prior failures to have
  already caught the bad case.

Design a mechanism that delivers **both** a cost saving **and** a safety guarantee,
and that works on a task the system has seen zero to two times before.

## The requirements a good answer must satisfy

1. **Dual-currency cost accounting.** Every claimed saving must say *which* currency
   it reduces — input/prefill tokens, output/generation tokens (≈ calls or turns),
   or dollars-per-token (via model choice) — or admit "safety only, no cost effect."
   These move independently; a mechanism can cut one while raising another.
2. **Per-instance measurability.** Gains must be demonstrable on a *single* task
   instance by comparing the mechanism on versus off on that same instance — not only
   as an average over a benchmark distribution. A task done once has no distribution
   to average over.
3. **Cold-start / rare-task yield.** The mechanism must produce a measurable benefit
   the *first, second, or third* time a hard task type is encountered, with no
   pre-existing corpus of similar traces to mine or calibrate against. At worst a
   no-op on a genuinely novel task, never negative ROI.
4. **Authority-independent verification.** The check that approves or blocks an action
   must derive its authority from something *other than* the same generative process
   that proposed the action — a separately specified policy, an external tool or
   solver, deterministic code, or a human. **No self-certification.**
5. **Compaction / context-durability invariance.** The policy a check enforces must
   live and be evaluable somewhere *not* subject to transcript summarization or
   truncation. Old turns — and any policy stated in them — are exactly what
   compaction drops first.
6. **Pre-execution, fail-closed gating.** Verification happens *before* a
   side-effecting action runs, and when the check cannot be evaluated with confidence
   — a novel action shape, an ambiguous risk — the default is refusal or escalation,
   never silent execution.
7. **No correctness regression.** The mechanism must not lower task success relative
   to an unassisted-agent baseline; ideally it *raises* reliability by catching errors
   before they execute. Cost and correctness are both scored.
8. **Overhead-bounded, self-gating cost.** The mechanism's own resource use must be
   smaller than what it saves on the instance it is applied to, and it must cheaply
   skip its heavy machinery when an action is manifestly low-risk.
9. **Composability and small-team buildability.** It must layer onto existing cost
   levers rather than replace them, and be buildable by a small team.

## The hardest requirement, stated exactly

> Deliver both a cost saving and a safety guarantee on a task instance seen zero to
> two times before, where **neither half may come from task-repetition history**. The
> check's authority must derive from something other than the acting model's own
> generation (no self-certification — this rules out constraint-sets an agent induces
> from its own traces, specs it writes for itself, and its own confidence reports as
> sole gates). And the cost saving must be shown on that single instance against a
> mechanism-off counterfactual on the same instance, not amortized over a benchmark.

Together these two clauses **eliminate every purely frequency-amortized family** —
skill/plan caching, cascades tuned to a workload's difficulty mix, corpus-mined
invariants — as a *complete* answer. They force the surviving core mechanism to have
a payoff that is a function of the **action's structure** or the **task's own internal
repetitiveness**, not of how many times that task type has recurred before.

## The design space the frame acknowledged (with each family's tension)

The brief named the known cost-saving families so designers would not merely
re-derive them, and stated why each is incomplete against the hardest requirement:

- **Redundancy caching / context paging** — cuts prefill on boilerplate; yields ~0 on
  the novel reasoning itself, and any eviction policy risks paging out the one
  segment (a safety constraint) that must never be dropped.
- **Retrieval + sub-agent isolation** — input-currency, gated on an index or a
  summarizer already existing and being trustworthy; on a rare task nothing relevant
  is indexed yet, and the summary is a second, un-audited compaction.
- **Plan / skill / workflow reuse** — output-currency, but amortized: it needs the
  same task shape to recur, and injecting the skill catalog into the prompt raises
  input tokens on every call, so net cost can *rise* on a one-off-dominated workload.
- **Model cascades / confidence-gated effort** — dollar-currency, but the usual
  trigger is the model's own confidence, which is self-certification.
- **Speculative / batched decoding, structured decoding** — real, but orthogonal
  plumbing that changes neither safety nor which work is done.

The brief's closing instruction: a complete answer must have a core mechanism that
survives the hardest requirement, and may *compose* the families above as secondary
levers — but may not lean on any of them as the whole story.

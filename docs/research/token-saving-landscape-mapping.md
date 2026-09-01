# The token-saving landscape, mapped onto openDaisugi

*Companion to [token-saving-landscape-2026-08.md](token-saving-landscape-2026-08.md).
That file is the industry survey; this one is where each of its levers lives (or
deliberately doesn't) in this codebase. Written 2026-08-20. The section
[September 2026](#september-2026-grafts-the-router-and-billed-cost) was added on
2026-09-26; it amends one rule below.*

## The one-paragraph verdict

The survey's two central findings are (1) caching beats routing as a savings lever, and
(2) the hidden tax of every savings lever is **silent quality regression** — the cheap
wrong answer that surfaces as customer tickets, not on dashboards. openDaisugi's answer
to (2) is its founding idea: every reused or downgraded path is re-verified against an
envelope, fail-closed. Its answer to (1) is this release: routing became cache-aware and
sticky, because a router that ignores the model-keyed prompt cache optimizes the small
line item while trampling the big one. Nobody in the survey's comparison tables sells
verification of what the cheap path did; that is the missing column, and it is ours.

## The routing ladder

Every turn falls down this ladder to the cheapest rung that can hold it. Safety is held
constant: the envelope verifies the action the same way at every rung, so moving down
the ladder never moves the safety bar.

| Rung | What runs | Marginal token cost | Verification |
|---|---|---|---|
| 0 | **Distilled pathway / deterministic script** | **zero** — no model call at all | re-verified against its stored envelope at reuse |
| 1 | **Local model** (llamafile/Ollama, qualified by `daisugi setup`) | zero *quota*; local compute only | same envelope gate as any model |
| 2 | **Sticky frontier with warm cache** | ~0.1× input on the cached prefix | unchanged |
| 3 | **Routed cheap cloud model** (easy turns) | ~5–15× cheaper than frontier | unchanged |
| 4 | **Frontier** (hard turns, never downgraded) | 1× | unchanged |

Two rungs outrank every row of the survey's caching tables: a pathway hit costs zero
tokens *with a proof attached*, and a local model costs zero quota. That is why the
sticky-routing rule has an exception written into it: stickiness protects the warm
frontier cache from rungs 3–4 thrash, but it never blocks a fall to rung 0 or 1 —
those rungs don't pay cache economics at all.

## Lever by lever

| Survey lever | Where it lives here | Status |
|---|---|---|
| **Provider prompt caching** (~90% off reads) | The gateway prices all three input buckets (`price_turn`: fresh / cache-read 0.1× / cache-write 1.25×) and forwards non-downgraded turns byte-untouched, so it never perturbs a cacheable prefix. Cache-aware *stickiness* (ADR-0015): a conversation with a deep frontier prefix stops being downgraded, because forfeited 0.1× reads plus the re-write on returning exceed a typical easy turn's savings. | Shipped |
| **Model routing** (real-world ~25–40%, not the 85–98% peaks) | `route_turn`: downgrade-only, never upgrades, routes by the governing ask so whole tool loops stay on one model. Our published blended number (~3× on turn-heavy agent traffic) is an honest-meter output, `estimated=True` on every counterfactual. The survey's "treat vendor peaks as proof-of-concept" is our stance verbatim. | Shipped |
| **Semantic caching** (30–73%, stale-hit failure mode) | Deliberately **not** a transparent layer. Reuse is an opt-in MCP tool (recall + freshness-gated answer store, ADR-0012): the agent chooses to reuse, and pathway reuse re-verifies. The survey's "don't deploy semantic caching on multi-turn context without stale-hit guards" is the failure mode we designed around before reading it. | Shipped (as opt-in, by design) |
| **Batch API** (flat 50% off async) | Gap. Distillation/clustering is exactly 24-hour-tolerant work. The claude-code backend has no batch lane; an API-key backend could. Roadmap, not faked. | Gap (roadmap) |
| **Output caps / structured output** | Not at the proxy — rewriting bodies would perturb the cacheable prefix and put an assurance layer in the business of editing requests. Belongs harness-side; the docs say so instead of shipping a footgun. *Amended 2026-09-26:* grafts are that harness-side placement. daisugi still never edits the model request; it may edit a tool call or its result by a proved, logged rule. See [September 2026](#september-2026-grafts-the-router-and-billed-cost). | Rejected at proxy (deliberate); grafts designed |
| **Prompt compression (LLMLingua)** | Rejected at the proxy for the same reason, squared: compression perturbs the prefix (kills provider caching) *and* changes meaning under a layer whose job is verifying meaning. | Rejected (deliberate) |
| **Context editing / memory / sub-agent isolation** (84% on long agents) | Harness-level levers (Claude Code has them). The gateway composes with them; it does not reimplement them. | Composes |
| **Small/local models as the cheap tier** | `daisugi setup`: hardware probe → right-sized model → qualification → local Tier-1 via `base_url`. The gateway's local rung (ADR-0015) routes easy turns there ahead of any cloud downgrade. | Shipped |
| **KV-cache / serving infra (vLLM, LMCache, Dynamo)** | Out of scope: that is the self-host serving layer under the local rung. llamafile/Ollama do their own KV management. | Out of scope |
| **FinOps / token observability** | `gateway-report`: tokens and dollars as separate first-class currencies (subscription quota is the binding constraint, dollars show how cheap the spared tokens were), counterfactuals flagged estimated, and cache-bucket totals so the cache hit rate is visible. | Shipped |
| **Routers' missing column: verification** | The gate, the envelope, Tier-0 re-verification. "Route cheap, but fail closed when the cheap answer leaves the envelope" — the sentence no router vendor in the survey can say. | The product |

## What "killer distillation" means against this landscape

The survey's savings all shrink a model call. Rung 0 removes it. The distiller is
therefore the highest-leverage token-saving component in the codebase, and its design
target is the **do-nothing script inversion** ([Dan Slimmon's gradual
automation](https://blog.danslimmon.com/2019/07/15/do-nothing-scripting-the-key-to-gradual-automation/)):
a do-nothing script encodes a procedure as steps and asks a human to perform the ones
nobody has automated yet. Here the LLM *is* that human. A distilled pathway is a
directed graph in which the steps that ran identically every time are deterministic
(shell/file/network steps replayed with zero tokens), the steps that varied in data
only are typed holes (bound and re-verified, near-zero tokens), and the steps that
genuinely varied in *kind* are `task` leaves — an LLM step, contained by the envelope,
and a standing candidate for promotion once enough runs show it stopped varying.
Gradual automation, with the gate holding the whole graph to policy at every stage of
its hardening.

Style note, settled here: deterministic steps are emitted in whatever stack the traces
actually used (shell traces distill to shell steps, file edits to file steps). The laws
are determinism and idempotence, not a programming ideology — same inputs, same
effects, safe to re-run. Purity where it helps replay; no functional-style dogma.

## What this release changed because of the survey

1. **ADR-0014** — the decomposer stopped treating redirections, substitutions, and
   wrappers as structurally unverifiable. Redirect targets are now checked against the
   envelope's own file scopes, substitution bodies are recursively verified, and
   transparent wrappers are unwrapped. This is not a safety relaxation; it makes the
   check *see* what it previously refused to look at — and it is what unblocks
   distillation over real shell-heavy traces (96.2% of captured shell calls carry a
   metacharacter; redirection alone was ~87% of all decomposition refusals).
2. **ADR-0015** — cache-aware sticky routing with the rung-0/rung-1 exception, and
   cache-bucket totals in the gateway report.
3. **Reject-with-remedy** — a rejection that names the minimal envelope amendment that
   would authorize the action, machine-readable, applied only with explicit consent.
   Safety stays fail-closed; the cost of being fail-closed drops to one deliberate
   command. (The "flaggable / amendable / upgradeable" third way: never silently allow,
   make widening cheap and auditable.)

## September 2026: grafts, the router and billed cost

*Added 2026-09-26. Nothing in this section is built. It summarizes two designs,
[design-grafts.md](../plans/2026-09-24-omarchy/design-grafts.md) and
[design-router.md](../plans/2026-09-24-omarchy/design-router.md), and the research note
[grafts-inputs-2026-09-26.md](grafts-inputs-2026-09-26.md). Outside facts below come from that
note. They were not fetched again for this section.*

### What changed

In September 2026 Spotify published shunt, a Claude Code plugin (`spotify/portal-ai-plugins`,
Apache-2.0, per the research note). A hook stops a large file read, and a cheap worker model
summarizes the file. The author reports 82% to 94% fewer Claude read tokens on selected cases.
Those numbers count tokens Claude would consume. They are not billed cost, they come from the
author, and they give no task success rate.

daisugi sits on every tool call already, as the gate. So it can do what shunt does, with three
things shunt lacks: a proof that the cheaper call does no more than the original call and the
envelope allow; a journal that measures whether the saving is real; and a garden that learns the
rules from the owner's own work.

### Grafts: a rewrite verdict

The gate has three verdicts today: allow, deny and ask. Grafts add a fourth, **rewrite**: "do
this cheaper call instead". A graft is a rule, held as data and written by models. Z3 proves it
at install, the gate checks it again on every call, it runs in shadow first, and it is kept only
when the journal shows it saves billed cost with no loss of task success.

Per the research note, no harness lets a hook swap one tool for another; each rewrite field
changes the arguments of the same tool. So a graft has one of three shapes:

| Shape | What happens | Cost |
|---|---|---|
| (a) shell rewrite | `cat Big.java` becomes a delegate read of the same file; the worker runs inside the delegate command | one turn; cannot reach a built-in read tool |
| (b) output replacement | the tool runs; a post-tool hook swaps its result for the worker's digest | one turn; the output shape must be pinned per tool |
| (c) deny and redirect | the gate denies, and the reason names the delegate tool | one extra model turn, which reads the cached prefix again |

Ruling GR-1 (provisional): shape (c) first. It works on every harness, because every harness
can deny, and it is router part 0.

The safety rules, from the design:
- **No graft turns a deny into an allow.** The gate decides the original call first. Only an
  allowed call can be rewritten, and the rewritten call goes through the whole gate again. If
  that second decision denies, the original call runs unchanged and the failed graft is logged.
- **No graft raises the approval level.** On Claude Code, `allow` with `updatedInput` skips the
  permission prompt, so a rewrite sends `updatedInput` alone unless the original call was
  already silent.
- **A delegate is an effect the envelope must grant.** A bulk read has a file-read effect and,
  with a remote worker, a network effect that sends file text off the machine. The default
  worker is local (GR-2). Grafts are refused under `stakes: physical`.
- **The worker never runs inside the gate.** Host hook timeouts fail open, and a worker takes
  10 to 30 s. The gate only matches, reads a file's size and verifies.
- **Worker output is untrusted text.** It is marked so in the journal and never spliced into a
  command.

### The rule this amends

The August mapping above rejects output caps and prompt compression at the proxy, because
rewriting bodies "would perturb the cacheable prefix and put an assurance layer in the business
of editing requests". Grafts are the harness-side placement that row asked for. The amended
rule:

- **Grafts never touch the model request.** A graft edits a tool call or a tool result on
  the harness side, so the model request and its prompt cache are untouched. (The gateway's
  routing, which sets the request's `model` field for a whole turn, is an older and separate
  lever. A model change forfeits the model-keyed cache, which is why ADR-0015 made routing
  sticky. Grafts add nothing to it.)
- **daisugi may edit a tool call's input, or a tool result, only by a named rule that is proved
  at install, checked again by the gate on every call, and logged with its id.**

The second August objection still holds for shape (b): a digest changes meaning under a system
whose job is to verify meaning. That is why a graft is proved, logged, marked untrusted and
promoted only on measured outcomes.

### The router, parts 0 to 4

The gateway's router is a fixed rule of thumb (`routing.py`, `route_turn` in `gateway.py`). It
does not learn, and it cannot see whether a cheap model's work was correct. The router design
makes the verifier its teacher:

| Part | What it does | Builds on |
|---|---|---|
| 0. Delegate routine reads | A gate rule denies a large read and names a `delegate` tool that runs on a cheap worker, local by default. This is graft shape (c). | the gate, `mcp_server.py`, the local rung |
| 1. Cheap first, the verifier escalates | A cheap model takes the turn; if its calls fail the envelope or its plan fails verify, the turn goes to the frontier. Escalation happens only at a task's start, to keep the cache. | the gate, `verify` |
| 2. The frontier plans, cheap models do the steps | The router becomes the one interface that fills each step's `preferred_model`. | `orchestrator.py`, `model_sizer.py`, pathways |
| 3. Learn from the owner's journal | On a small sample of frontier turns, a cheap model answers in shadow; the verifier and tests label which was good enough; a small local model learns to predict it. | the journal, the lexical and potion embedders |
| 4. Measure it honestly | Every choice, escalation and shadow result in the journal; a new chooser replaces the old one only when it wins on held-out recent turns. | the gateway journal, `router status` |

Order: parts 0 and 4 together first, then 1 behind a flag, then 2 with port stage K, then 3
with a cap on shadow tokens. Each part is built in Python first, then in Go and Rust with the
same golden cases.

### The measurement warning: fewer tokens can cost more

The research note found that a cut in tokens can raise the bill:

- On Claude Code, the most aggressive tool-output compression in one study "reduced delivered
  tool-output tokens by 38.4% but increased billed cost by 6.8%", with a correlation of
  r = 0.15 between the two ([arXiv 2607.12161](https://arxiv.org/abs/2607.12161)). Cache reads
  and writes dominate input cost, and compression added turns.
- An outside test of RTK, a tool that rewrites shell output, measured 5% lower cost on Claude
  Code and 5% higher on OpenCode with DeepSeek, and said "Extra turns can erase the savings"
  ([Quesma](https://quesma.com/blog/does-rtk-make-ai-coding-cheaper/)).
- Sub-agents move tokens to other models; they do not remove them. Anthropic reports that
  multi-agent systems use about 15 times more tokens than chats
  ([Anthropic](https://www.anthropic.com/engineering/multi-agent-research-system)).

So the measure for grafts and the router is **billed cost per successful task**, with task
success held level and quota tokens shown beside it (ruling GR-6). Trials split by session
(GR-7). Every counterfactual is marked as an estimate, as the gateway meter already marks it.
A graft or router that saves tokens and raises the bill is a regression, and the garden drops
it.

### Where this leaves the ladder

The routing ladder above does not change. Grafts make a call cheaper inside a rung; the router
design makes the choice of rung learn from verified results. A pathway (rung 0) still beats
both, because it removes the model call instead of making it smaller.

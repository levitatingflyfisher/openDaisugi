# The token-saving landscape, mapped onto openDaisugi

*Companion to [token-saving-landscape-2026-08.md](token-saving-landscape-2026-08.md).
That file is the industry survey; this one is where each of its levers lives (or
deliberately doesn't) in this codebase. Written 2026-08-20.*

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
| **Output caps / structured output** | Not at the proxy — rewriting bodies would perturb the cacheable prefix and put an assurance layer in the business of editing requests. Belongs harness-side; the docs say so instead of shipping a footgun. | Rejected at proxy (deliberate) |
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

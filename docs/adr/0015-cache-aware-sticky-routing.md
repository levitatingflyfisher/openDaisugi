# ADR-0015 — Cache-aware sticky routing, the local rung, and cache visibility

**Status:** Accepted.

## Context

The August-2026 landscape survey ([research/token-saving-landscape-2026-08.md](../research/token-saving-landscape-2026-08.md),
mapped in [research/token-saving-landscape-mapping.md](../research/token-saving-landscape-mapping.md))
puts provider prompt caching above routing as the biggest real savings lever: cached
reads price at ~0.1× input. The gateway's router ignored that. The provider cache is
keyed to the model, so downgrading one easy turn out of a frontier conversation
forfeits the 0.1× reads on the whole cached prefix, pays a fresh cache write on the
cheap model, and pays a ~1.25× re-write when the conversation returns to the frontier.
With the default price table (frontier 15/75, cheap 1/5 per MTok) the forfeit is
≈ P × $17 per million prefix tokens against a typical easy-turn saving of ≈ $0.035 —
break-even near a 2,000-token prefix. Real agent conversations run to hundreds of
thousands. A router that ignores cache state optimizes the small line item while
trampling the big one.

## Decision

`route_turn` keeps its locked conservative core — never upgrade, never touch a hard
turn, no signal means no downgrade — and an easy turn now falls down the **routing
ladder** to the cheapest rung that can hold it:

1. **The local rung.** A configured, qualified local model (`--local-model` /
   `Config.gateway_local_model`; the model `daisugi setup` qualifies) serves the easy
   turn at zero quota. Cloud cache economics do not apply to it, so **stickiness never
   blocks the fall to local** — this rung strictly dominates the cheap cloud model.
   Local turns price at (0, 0): the dollar multiplier stays conservative, and the
   headline metric (frontier tokens kept off the quota pool) counts the full turn.
2. **Sticky-to-cheap.** A conversation that already lives on the cheap model keeps
   going cheap regardless of prefix depth — its warm cache *is* the cheap one, and
   bouncing it to the frontier for "stickiness" would be the same mistake mirrored.
3. **Sticky-to-frontier.** Past `STICKY_PREFIX_THRESHOLD_TOKENS` (default 4096 ≈ 2×
   the computed break-even; prefix estimated at chars/4 including tool results), an
   easy turn stays on the requested model: the forfeited cached reads plus the
   eventual re-write cost more than the downgrade saves.
4. **The cheap cloud model**, as before, for easy turns in young conversations.

Conversation identity is the first user message's text (hashed); the pipeline holds a
bounded per-conversation memory of the last routed model. A compaction that rewrites
the first message simply starts a fresh sticky history — the safe direction, since
stickiness is an optimization and never policy.

**Cache visibility.** The journal already recorded the provider's cache buckets;
`summarize`/`gateway-report` now surface them: total cache-read and cache-write
tokens, the cache hit rate as a share of all input, and the local-rung turn count —
the measured FinOps view the survey says most teams lack.

## What was deliberately not done

- **No body rewriting** (output caps, compression, injected cache_control): the proxy
  forwards non-downgraded turns byte-untouched. Perturbing the prefix breaks the very
  cache this ADR protects, and an assurance layer must not edit requests.
- **No transparent answer cache.** Reuse remains the opt-in MCP tools (ADR-0012).
- **Rungs above the ladder:** a distilled pathway (zero tokens, re-verified) outranks
  every rung here, but a pathway cannot serve an open-ended Messages turn from inside
  the proxy — that reuse stays where consent and verification live, in the recall
  tools and the hook.

## Consequences

- Easy-turn downgrades stop thrashing warm frontier caches in long conversations; the
  meter can show the trade honestly because both cache buckets are priced and now
  reported.
- With a local model configured, the common case for an easy turn becomes "zero
  quota" rather than "cheaper quota" — and the sticky rules never get in its way.
- The counterfactual stays flagged `estimated=True`; nothing here upgrades an
  estimate into a measurement.

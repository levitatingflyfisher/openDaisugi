# ADR-0016 — Onboarding envelopes are inferred from evidence, not generated from task text

**Status:** Accepted (2026-08-20).

## Context

Bulk onboarding (`daisugi onboard`, `daisugi journal ingest`) verified each
imported episode against an envelope produced by `generate_envelope` — an LLM
writing permissions from the episode's *task text*. The trace-derived
deterministic inference (`infer_envelope`, ADR-0014-aware) existed but was
wired only into the hook capture flow. The result was structural: an
LLM-guessed allowlist almost never names the plumbing heads a real episode
uses (echo, grep, head, cd, sed) — measured at 2 recovered of 1,229 rejected —
so onboarding failed most of the corpus even after shell decomposition
(ADR-0014) fixed the command-level layer, and every episode also paid an LLM
call for the privilege.

## Decision

Ingest infers each episode's envelope from the episode's own observed steps:
every shell head (through decomposition, wrappers, and substitutions), every
redirect target, every file path, every URL, every MCP tool. Deterministic,
zero LLM calls. `generate_envelope`, `tier1`, `model`, and `concurrency` left
the ingest path entirely (tier1 still serves distillation).

This is the correct epistemics, not just a cheaper pipeline: an onboarded
episode already happened. The envelope's job there is to *describe the
evidence precisely* so verification classifies it (and distillation clusters
it), not to guess forward policy from a sentence. Inference stays fail-closed
where it matters — non-literal heads (`$CMD`), non-literal redirect targets
(`> $LOG`), parse errors, and opaque wrappers are uninferable, so those
episodes still land FAIL and are journaled as distillation data.

## Measured

Same 250-transcript corpus as ADR-0014 (1,589 episodes, fresh journal,
decomposition on): **66.0% of episodes now verify end-to-end** (from ~0.2%
with generated envelopes), zero LLM calls, zero errors. Residual failures:
439 episodes on multi-line scripts the decomposer does not yet model
(heredocs, `for` loops, `VAR=x` assignment lines — the next recoverable
chunk), 48 non-literal heads, 44 non-literal redirect targets, 10 with no
extractable heads.

## Consequences

- Onboarding cost drops to parsing + verification; the only LLM spend left in
  `daisugi onboard` is episode splitting (>max-tools) and distillation.
- Onboarded envelopes carry `generated_by=opendaisugi.hook.infer_envelope`,
  `stakes=low`: they are evidence descriptions for the compilation loop, not
  forward policy — exactly as `infer_envelope` has always documented.
- The multi-line-script gap (439 episodes) is now the dominant onboarding
  loss and is queued as decomposer work, not envelope work.

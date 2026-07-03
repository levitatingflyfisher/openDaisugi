# Documentation

Organized on the [Diátaxis](https://diataxis.fr/) model — four kinds of docs for
four different needs. Find what you need by *what you're trying to do*, not by
guessing a filename.

| I want to… | I need | Go to |
|---|---|---|
| **learn by doing** | a Tutorial | [Tutorials](#tutorials) |
| **accomplish a specific task** | a How-to guide | [How-to guides](#how-to-guides) |
| **look up exact details** | Reference | [Reference](#reference) |
| **understand why** | Explanation | [Explanation](#explanation) |

New here? Start with the [README quickstart](../README.md), then the
[Tutorials](#tutorials), then [Explanation § concepts](../docs/concepts.md).

---

## Tutorials
*Learning-oriented — take me by the hand through my first success.*

This is the quadrant we're actively growing. Today the entry points are:
- The **[README quickstart](../README.md)** — install, generate an envelope, verify
  a plan.
- The runnable **[examples/](../examples/)** — end-to-end scripts you can read and
  run (orchestrator, safe sub-agent, swarm tasking, robotics sims).

*Gap (contributions welcome):* a hand-held "verify your first plan and watch it
reject an unsafe one in 10 minutes" tutorial, and a "distill your first pathway"
tutorial. If you write one, put it in `docs/tutorials/`.

## How-to guides
*Task-oriented — how do I accomplish X (assumes you know the basics)?*

- **[Deployment](deployment.md)** — run it in the modes it supports.
- **[Integrations](integrations.md)** — wire it into a harness (per-harness adapters).
- **[Hook integration](hook-integration.md)** — the passive-hook path.
- **[π0 / VLA integration](pi-vla-integration.md)** — the robotics integration path.
- Agent-guidance for working *in* this repo: **[AGENTS.md](../AGENTS.md)**.

## Reference
*Information-oriented — tell me exactly, precisely, completely.*

- **[Step vocabulary](step-vocabulary.md)** — every step type + its metadata keys.
- **[Pathway / skill bundle format](pathway-skill-format.md)** — the on-disk contract.
- **[Feature status](feature-status.md)** — what's shipped, per version.
- **Formal specification (yellow paper)** — the rigorous verification semantics
  (envelope algebra, subsumption soundness, fail-closed guarantees). *(Planned —
  see below.)*
- The public API is the `opendaisugi` package surface (`Daisugi`, `verify`,
  `generate_envelope`, `orchestrate`) — see the docstrings and the
  [architecture module map](architecture/OVERVIEW.md#module-map-where-to-look).

## Explanation
*Understanding-oriented — help me understand the ideas and the why.*

- **[Vision](../VISION.md)** — the one idea, the invariants, the honest scorecard.
- **[Architecture overview](architecture/OVERVIEW.md)** — the spine + diagrams.
- **[Architecture Decision Records](adr/)** — why each load-bearing choice was made.
- **[Concepts](concepts.md)** — envelopes, the predicate algebra, Z3 compilation,
  subsumption, verification stages.
- **[Security model](security-model.md)** — the threat model and fail-closed posture.
- **[Robotics](robotics.md)** — the runtime-assurance-for-VLA thesis (experimental).
- **[Limitations](limitations.md)** — read before adopting. What it does *not* do.
- **[Case study: AI council](case-studies/ai-council.md)** — a worked example.

---

### The white paper & yellow paper

Two long-form documents complement this tree *(both planned; drafts landing next)*:
- **White paper** — the conceptual/strategic case (why this matters, the RTA
  lineage, the layer-not-harness position).
- **Yellow paper / formal spec** — the rigorous specification of the verification
  semantics.

*(A "beige paper" — a plain-language restatement of the yellow paper — would live
here too, if/when the formal spec warrants an accessible companion.)*

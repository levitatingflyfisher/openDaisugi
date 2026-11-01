# How the idea grew

This page tells where openDaisugi came from, which turns the project did not take, and which questions are still open. It is history. For what the project is today, read [VISION](../../VISION.md) and the [README](../../README.md). Where this page and the repo disagree, the repo is right.

## The path

The idea took shape in two long conversations in February and March 2026, one question at a time. Later steps came through the year.

1. **Who writes the domain-specific language?** The first question was about agent languages. A human can write one to constrain an agent. An agent can write code that runs. The third case was less explored: an agent writes the language that another agent obeys.
2. **What about safety?** People say a neural network must not control a system, because its behavior is not defined. The gap is real. Neural systems are capable and unverifiable. Formal systems are verifiable and limited.
3. **The seed.** One model could build a checkable specification that another model runs inside. This bridges the gap by separating what is allowed from what is decided. A space of verifiable calculations says what is allowed. A black box makes the choice.
4. **The prior art.** Aerospace and industrial control already had four ways to combine an unverified system with a verified one. All four belong to one field, runtime assurance. We found no earlier work that mapped them onto LLM agents.
5. **A cheaper agent.** The first product idea was a personal agent runtime that gets cheaper with use. It would write repeated work down as compiled paths and check each path as it ran.
6. **A new name.** Project Bonsai, a Microsoft platform for learned industrial control, used the same metaphor in the same field. The project took the name daisugi.
7. **A compiler for inference.** The picture settled as a supervised just-in-time compiler. Observe what repeats. Compile it. Fall back to the full model on anything new. Watch every compiled path.
8. **One system.** Memory, orchestration, compilation and routing turned out to be parts of one loop. The journal feeds the compiler. Compiled paths make runs cheaper. Runs feed the journal.
9. **The model as compiler.** The largest model does more than get compiled. It writes the compiled paths from journal traces. The language emerges from use. Nobody designs it first.
10. **Failure as a lesson.** A failed compiled path should return a reason, so the path tightens, as in counterexample-guided refinement.
11. **Robots.** The same split applies to robot foundation models. A checker hierarchy over several time scales can sit between a model's proposed motion and the motors. See [Robotics](../robotics.md).
12. **Cost and trust.** A survey of routing and caching showed that caching pays more than routing. It also showed that a router needs hysteresis, because leaving a warm cache costs more than the cheaper model saves. A supply-chain compromise of a popular model client led to a rule: a library that vouches for safety should not depend on code that can steal keys.

Two turns led somewhere the project did not go.

- **An extension first, then a choice of two products.** The early plan built an add-on for one agent, measured how much work repeats, and then chose between a standalone agent and a robot safety product. The project built one verification core with hooks for several harnesses (Claude Code, Codex, pi, OpenCode). The warden is the hook. The core serves agents and, through an optional extra, robots.
- **A library that is not an agent.** A later plan said openDaisugi must stay middleware and never direct agents. The core still runs alone ([ADR-0020](../adr/0020-layer-floor-loop.md)). The project also grew a floor, coppice, and a steward that directs agents through a gated spawn. Both sit on top of the core.

Python was the first language. It is now the reference oracle. The Go and Rust ports are the product's shape ([roadmap](../roadmap.md)). The names also changed: gate became warden, gateway became router, foreman became steward, and conversation memory became Pando R2 ([ADR-0026](../adr/0026-names-warden-router-steward-pando.md)).

## The thesis

Any system that calls an expensive, stochastic model again and again on similar tasks re-derives decisions it has already settled. The settled parts can be compiled into cheaper paths. The same parts can be watched at run time with methods from aerospace control. This holds for software agents and for robot policies alike. In both, a capable but unverifiable component proposes actions, and a simple checker decides which actions may run. A compiled path is also easier to inspect than raw model output.

The insight fits in one sentence:

> **Separate what is allowed from what is decided.**

A model closes the verification loop by generating constraints that can be checked. It does not close the loop by becoming verifiable. The generated envelope is checkable even though the process that wrote it is not. [VISION](../../VISION.md) states the rule in full, and the [white paper](../whitepaper.md) argues the case.

## Four runtime-assurance architectures

Runtime assurance is the practice of deploying an unverified system safely by monitoring and constraining it while it runs. Four patterns exist in the literature.

1. **Simplex.** An advanced controller is fast and unverified. A baseline controller is conservative and verified. A decision module watches the state and switches to the baseline when a safety boundary is near. The canonical paper is Sha, 2001, which builds on early work from 1996.
2. **Neural-tuned classical control.** A classical controller, such as a PID or MPC, gives the structure. A neural network tunes its parameters. The system keeps the classical guarantees inside the parameter bounds.
3. **Verified envelope.** Define the set of actions that are allowed. Reject all others. Verify actions, not perception. This works best where the bounds come from physics or internal state, because every guarantee depends on perception being right.
4. **Compiled policy.** A slow reasoner writes a constraint specification. A simpler system runs inside it. Generated constraints may only tighten fixed hard limits, never loosen them.

openDaisugi is mainly the third architecture, fed by the fourth. A model writes the envelope. Z3 proves that a plan stays inside it. The check fails closed. The garden also compiles repeated, proven work into pathways, which is the fourth pattern applied to cost. Simplex shows up as the fallback: when a proof fails, the work goes back to the frontier model or to a person. The [white paper](../whitepaper.md) maps each pattern in detail.

## Open questions

None of these has been answered. Each names what would answer it.

1. **How much agent work can be compiled?** Nobody has measured what share of real agent interactions repeat closely enough to compile, or how that share changes with time. The journal records the needed data. A count over a long run would answer it. The thesis weakens if the share is low. Earlier notes put the line near 40 percent.
2. **How much tool traffic becomes typed steps?** Typed steps can be proved. Opaque shell cannot, except in part. The warden's audit log holds the answer. A count of typed against opaque calls over real sessions would give it.
3. **Can a repeated robot task skip the large model?** A verified trajectory from a vision-language-action model might compile into a dynamic movement primitive. Then a repeated task would run under a barrier-function filter without the model. A hardware or high-quality simulator test with a success rate against the model baseline would answer it. See [Robotics](../robotics.md).
4. **Can an envelope tighten with perception confidence?** A fixed rule says never exceed 5 N of grip force. An adaptive rule says 5 N with a clear view, 2 N when the object is partly hidden, and stop below a confidence floor. This answers the weakness of verified envelopes, which depend on correct perception. A comparison of fixed and adaptive envelopes on the same tasks, counting violations and stalls, would answer it.
5. **Can one compile function bound cost and quality?** Agent routing, robot policy distillation and neural-tuned control may share one form: a function from traces to a verified policy whose cost is lower and whose quality stays within a stated bound of the traces. A formal statement with a worked case in each domain would answer it.

Parts of the robotics ground are no longer open. Others have put control-barrier filters under vision-language-action models. The [robotics page](../robotics.md) lists that work. What remains open is the envelope written by a model for each task, the compiled primitives, and the confidence-driven tightening.

## The name

Daisugi (台杉) is a Japanese forestry practice from the Kitayama region near Kyoto. A cedar is cut back to a stump, the platform. The stump sends up many new shoots. Each grows into a straight trunk.

The fit is close. The frontier model is the stump. Its work, journaled over many runs, sends up new growth: compiled pathways that are small, straight and cheap to run. The stump stays and keeps supplying growth. A coppice, the name of the floor, is a related technique ([VISION](../../VISION.md)).

For the formal statement, read the [yellow paper](../spec/yellow-paper.md). For what the project does not do, read [Limitations](../limitations.md).

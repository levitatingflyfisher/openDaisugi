# Weights, types and containment: four sources

Checked on: 2026-11-03. Method: WebFetch and WebSearch by one agent. Every claim cites a
link. A claim about openDaisugi cites a file in this repo.

## Question

The owner sent four sources. For each: what it is, its key idea, and what is solid versus
speculative. Then: where it overlaps openDaisugi, what we could borrow, and roadmap ideas.

## Summary

- Schmidhuber's page says that a 1991 network that writes the weights of another network
  is formally a linear transformer without normalization. The owner says the page was
  shared in reply to the weight-space post. The page itself is older (2021, updated
  November 2025) and does not present itself as a reply.
- "Sparks of Weight-Space Programming" (MIT CSAIL, 2026-11-02) reports that one LLM read
  and wrote network weights directly, with no gradient descent. It is a blog post with
  released logs. The results are self-reported and early.
- rust-djinn is not a port of Augustsson's Djinn. It implements Djinnlang, by Simon
  Henniger, Stephen Chong and Nada Amin. An LLM writes Rust code and a Lean 4 proof, and
  Lean checks them. Augustsson's Djinn is a different, older tool. It derives a Haskell
  term from a type by proof search, with no model.
- prevent-contain-prove.org is a strategic guidance report from the NDIST workshop
  (Kestrel Institute). It argues that AI makes attacks cheap. Its answer is to prevent
  defect classes by construction, contain compromise by compartments, and prove critical
  properties with machine-checked evidence.
- The strongest lesson for openDaisugi: trust comes from the checker, not from the model
  that made the artifact. Three of the four sources say this in their own terms.

## 1. Fast weight programmers (Schmidhuber)

**What it is.** A blog page by Jürgen Schmidhuber, "26 March 2021, updated Nov 2025"
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html)).

**Key idea.** In 1991 a "slow" network learned by gradient descent to write the weights
of a "fast" network. One variant writes additive outer products of keys and values. The
page says this is "formally equivalent" to what is now called a linear transformer,
apart from normalization, and calls it an "unnormalised linear Transformer"
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html)).
The page also says that the weights of a network "should be viewed as its program"
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html)).

**Solid.** The equivalence between linearized self-attention and fast weight controllers
is shown in a peer-reviewed line of work. Schlag, Irie and Schmidhuber state "the formal
equivalence of linearised self-attention mechanisms and fast weight controllers from the
early '90s" and add a delta-rule update
([arXiv 2102.11174](https://arxiv.org/abs/2102.11174)). The page lists that paper at
ICML 2021
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html)).

**Speculative or contested.** The page also makes priority claims, for example about
self-supervised pre-training and about who should get credit
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html)).
We did not check those claims. They do not affect anything here.

**Context.** The page does not use the phrase "weight-space programming", and the
weight-space post does not cite Schmidhuber or fast weights
([page](https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html);
[post](https://rikiyat.github.io/blog/weight-space-programming/)). So the link between
the two is a reply by a reader, as the owner reports, not a citation.

## 2. Weight-space programming (Takehi et al.)

**What it is.** "Sparks of Weight-Space Programming: LLMs can natively read & write
neural network weights", by Rikiya Takehi, Jayden Teoh, Alex Zhang and Omar Khattab (MIT
CSAIL), dated 2026-11-02
([post](https://rikiyat.github.io/blog/weight-space-programming/)).

**Key idea.** A general LLM reads raw weights and edits them by reasoning about what each
part computes. No optimizer and no gradient descent. The model in every experiment is
named "GPT-6 Astra". The authors' Python sandbox is a separate harness
([post](https://rikiyat.github.io/blog/weight-space-programming/)).

**What they report** (all from the [post](https://rikiyat.github.io/blog/weight-space-programming/)):

- From weights alone, it named which Atari-style game each of four same-shape PPO
  networks was trained on.
- It fixed a failing control policy by editing 3.5% of its weights in one try: 0% to 100%
  success over 1,024 episodes.
- It wrote MinAtar policies from scratch with no training. Breakout scored 517.4 against
  25.3 for PPO at 500M steps. On Seaquest and Asterix it was below PPO.
- It edited 0.016% of a 0.8B language model's weights to stop looping. MATH-500 went from
  7.9% to 48.1%.
- Limits it found itself: it could not find a sycophantic model from weights alone, and on
  a model that rarely loops its edits did not add skill. The authors write that the edits
  "could not add an ability the model lacked".

**Solid.** The logs of every experiment are public
([logs](https://github.com/RikiyaT/lm_understand-logs), linked from the
[post](https://rikiyat.github.io/blog/weight-space-programming/)). The edited model from
experiment 8 is published. So a reader can check what the model saw and wrote.

**Speculative.** It is a blog post, not a reviewed paper. The numbers are self-reported,
mostly single runs, on small networks. The authors call the results "very early" and say
a benchmark and harness are still being built. The listed uses (audit without
evaluation, recursive self-improvement, model merging) are stated as future
possibilities ([post](https://rikiyat.github.io/blog/weight-space-programming/)).

## 3. rust-djinn and Djinnlang (Henniger, Chong, Amin)

**Who wrote it.** The repo is `shenniger/rust-djinn`
([repo](https://github.com/shenniger/rust-djinn)). The GitHub profile shows no name. It
says "PhD student at Harvard University"
([API](https://api.github.com/users/shenniger)). The README names Simon Henniger, Stephen
Chong and Nada Amin as the authors of the paper behind it, and links henniger.dev
([repo](https://github.com/shenniger/rust-djinn)). That site is Simon Henniger's, a
Harvard CS PhD student, and his address there is shenniger@g.harvard.edu
([henniger.dev](https://henniger.dev)). So the name the owner heard as "Simon Jennifer" is
**Simon Henniger**.

**What it is.** "Djinnlang implementation for Rust"
([repo](https://github.com/shenniger/rust-djinn)). The paper is "Djinnlang: Higher-Level
Programming by Unambiguous Specification with an LLM in the Compiler", arXiv 2609.23954,
submitted 2026-09-21 ([arXiv](https://arxiv.org/abs/2609.23954)). The paper's tool lowers
specs to Dafny. rust-djinn uses Lean 4 over an Aeneas model of the Rust code
([repo](https://github.com/shenniger/rust-djinn)).

**Key idea.** You write only a specification: constraints that relate inputs to outputs.
An LLM writes the implementation and a proof. A checker (Dafny in the paper, Lean in
rust-djinn) verifies both ([arXiv](https://arxiv.org/abs/2609.23954);
[repo](https://github.com/shenniger/rust-djinn)). The new rule is "unambiguity": the LLM
must also prove that any implementation that meets the spec gives the same outputs on the
same inputs. Then the model has no freedom over what the program means, and the code can
be regenerated from the spec at any time, like compiler output
([arXiv](https://arxiv.org/abs/2609.23954)). rust-djinn checks four obligations per
function: `unambiguous`, `exists` (each valid input has an output), `impl_correct` (the
code returns an allowed output, terminates and does not panic) and `ens_0` (each
`#[ensures]` clause follows from the spec)
([repo](https://github.com/shenniger/rust-djinn)). The abstract also says the approach is
a form of AI control: the model is untrusted and its output is tightly verified
([arXiv](https://arxiv.org/abs/2609.23954)).

**Solid.** The trust split is stated plainly. The spec-to-Lean output is trusted. The
LLM's code and proofs are untrusted and checked by Lean
([repo](https://github.com/shenniger/rust-djinn)).

**Speculative or limited.** The README says it is "research code and mostly vibe-coded"
and "not production-ready". The library models of `std`, `core`, `alloc` and `memchr` are
trusted but described as not trustworthy. Unambiguity is undecidable, so a hard spec can
leave the agent with neither a proof nor a counterexample. Performance cannot be
specified ([repo](https://github.com/shenniger/rust-djinn)). The abstract gives no success
rates ([arXiv](https://arxiv.org/abs/2609.23954)). The README does not mention
Augustsson's Djinn ([repo](https://github.com/shenniger/rust-djinn)), so we do not claim a
lineage beyond the name.

### The original Djinn (Augustsson)

**What it is.** A Haskell tool by Lennart Augustsson, announced on 2005-12-11: "a small
program that takes a (Haskell) type and gives you back a function of that type if one
exists" ([announcement](https://mail.haskell.org/pipermail/haskell/2005-December/017055.html)).
It is still on Hackage, BSD-3-Clause, latest release 2025.2.21
([Hackage](https://hackage.haskell.org/package/djinn)). The man page credits Augustsson
and Thomas Johnsson ([man page](https://manpages.org/djinn)).

**Key idea.** Djinn "uses a decision procedure for intuitionistic propositional calculus
due to Roy Dyckhoff", "a variation of Gentzen's LJ system"
([announcement](https://mail.haskell.org/pipermail/haskell/2005-December/017055.html)).
Dyckhoff's 1992 calculus (G4ip) has no contraction rule, so backward proof search always
terminates ([Cambridge](https://www.cambridge.org/core/journals/journal-of-symbolic-logic/article/contractionfree-sequent-calculi-for-intuitionistic-logic/C7ABBFC63E74878E52FF4E624029503B);
[Wikipedia](https://en.wikipedia.org/wiki/Roy_Dyckhoff)). By the Curry-Howard
correspondence, "a proof is a program, and the formula it proves is the type for the
program" ([Wikipedia](https://en.wikipedia.org/wiki/Curry%E2%80%93Howard_correspondence)).
So the proof that Djinn finds is the function. If no proof exists, Djinn halts and says
so: there is no total expression of that type
([announcement](https://mail.haskell.org/pipermail/haskell/2005-December/017055.html);
[man page](https://manpages.org/djinn)).

**Solid.** The search is complete and terminates for propositional types: it finds a
function if one exists and halts if none does
([announcement](https://mail.haskell.org/pipermail/haskell/2005-December/017055.html)).

**Limited.** Propositional types say little about behaviour. Many functions can share one
type, and Djinn has an option to print several solutions
([man page](https://manpages.org/djinn)). It picks a term that fits the
type, not the term you meant.

**The two Djinns side by side.** Augustsson's Djinn has no model: a decision procedure
derives the program. Djinnlang has a model: the LLM writes the program and the proof, and
a checker accepts or rejects. Djinnlang's unambiguity rule closes the gap that the old
Djinn leaves open: it forces the spec to fix one behaviour.

## 4. Prevent, Contain, Prove (NDIST)

**What it is.** "Prevent, Contain, Prove: A Framework for Urgently Securing Critical
Digital Systems", strategic guidance from New Directions in Software Technology (NDIST),
an invitation-only workshop hosted by the Kestrel Institute
([site](https://prevent-contain-prove.org/)). The full report is a 40-page PDF
([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).
The committee co-chairs are Gopal Sarma (RAND), Brad Martin (Galois) and Bryan Loyall
(Charles River Analytics). Members come from Oath Technologies, Collins Aerospace, SRI,
DOLL Labs, MIT CSAIL, the Kestrel Institute and Vanderbilt University
([site](https://prevent-contain-prove.org/)). The report lists participating
organizations but says that taking part "does not represent an endorsement"
([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).

**Key idea.** AI makes finding and chaining exploits cheap, so patching faster is not
enough. Three functions answer it ([site](https://prevent-contain-prove.org/)):

- Prevent: make defect classes impossible by construction, for example memory-safe
  languages and hardware.
- Contain: limit what a compromise can do, with compartments.
- Prove: establish critical properties with machine-checkable analysis.

Adoption is graded by consequence. A system can use light methods (precise interface
specs, executable specs as test oracles, property tests) on most parts and machine-checked
proofs only where failure would be catastrophic
([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).
Claims of assurance should come with inspectable evidence, "not checklists"
([site](https://prevent-contain-prove.org/)).

**Three lines that bear on openDaisugi** (all from the
[report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)):

1. "Because proofs are checked by small independent tools, trust derives from the
   checker, not from the AI that produced the evidence."
2. Prevention and proof do not help against code that does what its author meant, such as
   a supply-chain implant. Containment carries most of the weight there.
3. A retrofitted boundary often needs a new interface to carry weight. If a component
   returns a secret on request, isolating it separates code but does not protect the
   secret.

**Incident it cites.** The report says that in July 2026 autonomous agents broke into
Hugging Face's production systems. It says OpenAI then disclosed that these were its own
models, run with cyber refusals off for an internal evaluation. It says the containment
that failed "was a single control": one package proxy was the only egress, and one
zero-day in it opened the sandbox
([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).
We report this as the report's account. We did not read the primary disclosures.

**Solid.** The frame and the production examples are long-standing practice. The report
cites seL4, CompCert in avionics, and Android's move to memory-safe code, with memory-safety
bugs falling from 76% (2019) to under 20% (2025)
([site](https://prevent-contain-prove.org/);
[report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).

**Speculative.** The report says that its actions are "deliberately limited and initial"
and that detailed criteria belong to a later open process. Many of its AI capability
figures are vendor-reported, which the report itself notes
([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).

## Overlap with openDaisugi

| Source | Overlaps | Does not overlap |
|---|---|---|
| Fast weights | Only as an analogy. The garden (slow, over many runs) writes pathways that run fast later ([README](../../README.md)). | No mechanism in common. daisugi does not write weights at run time. |
| Weight-space programming | `daisugi lora` writes knowledge into weights: it fine-tunes a small model on the journal's (task, envelope) pairs ([lora](../../src/opendaisugi/lora/__init__.py)). | We train with gradients, not edit by reading. The warden checks actions, never weights, so a weight tier made either way passes the same gate. |
| Djinnlang | Same split: an untrusted model writes, a trusted checker decides. Both fail closed when no proof is found. | Djinnlang verifies code against a full spec before the code exists. The warden checks each action against a bound at run time. Djinnlang's unambiguity makes "allowed" equal to "decided". openDaisugi keeps them apart on purpose: the envelope bounds, the agent chooses inside it. |
| Augustsson's Djinn | Proof search over a small logic is close to what Z3 does for an envelope: find a model or show none exists. Subsumption already returns a concrete counterexample step ([subsumption](../../src/opendaisugi/subsumption.py)). | Djinn produces programs. openDaisugi does not search for plans; an LLM or a cached pathway writes them. |
| Prevent, Contain, Prove | Prove maps to the warden's Z3 check. Contain maps to `daisugi fence`, a Landlock file boundary ([README](../../README.md)). Prevent maps to the kernel and dialects: an action the algebra cannot express cannot be allowed. Graded adoption maps to stakes tiers. | The report targets critical infrastructure code, not agent actions. It does not discuss envelopes, journals or distillation. |

## What we could borrow

- **Cite the trust line.** "Trust derives from the checker, not from the AI that produced
  the evidence" is the one idea in another committee's words
  ([report](https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf)).
  Djinnlang states the same split for code
  ([arXiv](https://arxiv.org/abs/2609.23954)).
- **Name the three functions in our docs.** Prevent (kernel and dialects), contain (fence),
  prove (warden). This gives an outside reader a known map, and it makes clear which layer
  carries which risk.
- **Single control is the failure.** The report's incident account says one proxy was the
  only boundary. openDaisugi already runs two layers that fail in different ways: the
  fence at the file level and the warden at the meaning level. We should say so, and not
  sell either alone.
- **The boundary-needs-an-interface caveat.** The fence protects files. A tool the
  envelope allows can still return a secret. That limit belongs on the fence page.
- **Djinnlang's obligation names.** `exists` is our vacuity check (an envelope that admits
  nothing). `impl_correct` is the per-step warden proof. Using the same plain names would
  help readers who know that work.
- **Weights are a cache, not a source of truth.** Djinnlang treats code as output you can
  regenerate from the spec ([repo](https://github.com/shenniger/rust-djinn)). The same
  stance suits our ladder: a pathway, a skill or a LoRA adapter is a compiled cache of
  journal traces, and the envelope plus the warden stay the source of trust.

## Roadmap ideas (ideas, not plans)

Each item respects the current rulings: the fence is the file boundary; the warden proves
meaning-level things; prefer structural fixes over hardening loops.

1. **Two-sided envelope witnesses.** *Idea.* For each new envelope, ask Z3 for one concrete
   allowed step and one concrete denied step near its boundary, and show both to the human
   next to the envelope. Today vacuity checks catch envelopes that allow everything or
   nothing ([limitations](../limitations.md)), and subsumption gives a counterexample when
   one envelope does not cover another. A witness pair shows what a valid envelope means
   by example. *What it takes:* a Z3 model-extraction call over the compiled envelope in
   the Python oracle, then Go and Rust. *Test:* every witness is fed back to the warden in
   all three clients; the allowed one must pass and the denied one must fail, with 0
   disagreements over the fixture corpus. A Z3 "unknown" gives no witness and says so.
2. **Semantic grading for the LoRA envelope writer.** *Idea.* Grade a trained envelope
   writer by meaning, not by text match. On held-out journal tasks, run subsumption both
   ways between the student's envelope and the recorded one. Equal both ways is a pass.
   Otherwise the counterexample step is the error report. *What it takes:* a held-out
   split in the LoRA dataset and a small eval command that reuses `envelope_subsumes`
   ([subsumption](../../src/opendaisugi/subsumption.py)). *Test:* a fixture pair known to
   differ by one path must fail with that path as the counterexample; an identical pair
   must pass. Promotion rule: a new adapter is used only if its equivalence rate does not
   drop. The same gate would apply to any future weight tier, however it was made.
3. **Plans by proof search over dialect words.** *Idea.* Where the step vocabulary is
   finite and typed, search for a plan that meets the task's goal predicate and the
   envelope, Djinn style, with no model call. Fall back to the LLM when the search finds
   nothing within a bound. *What it takes:* goal predicates in the kernel (they do not
   exist today) and a bounded search over dialect words. *Test:* replay journal tasks that
   already have pathways; measure how many plans the search recovers, and require every
   found plan to pass the warden unchanged. This is the most speculative item.
4. **An assurance page generated from evidence.** *Idea.* One page lists each safety claim,
   marks it prevent, contain or prove, and links the test or fixture count that backs it.
   This follows the report's "evidence, not checklists"
   ([site](https://prevent-contain-prove.org/)). *What it takes:* a claims file and a
   small generator. *Test:* CI fails if a claim links a test that does not exist or a count
   that no longer matches the fixtures.
5. **Name the fence's limit.** *Idea.* State on the fence page that it bounds files, not
   what an allowed tool returns, and point to the warden for meaning-level checks. *What it
   takes:* a short doc change. *Test:* none beyond review; it is a doc fix.

## Sources

- https://people.idsia.ch/~juergen/fast-weight-programmer-1991-transformer.html
- https://arxiv.org/abs/2102.11174
- https://rikiyat.github.io/blog/weight-space-programming/
- https://github.com/RikiyaT/lm_understand-logs
- https://github.com/shenniger/rust-djinn
- https://api.github.com/users/shenniger
- https://henniger.dev
- https://arxiv.org/abs/2609.23954
- https://mail.haskell.org/pipermail/haskell/2005-December/017055.html
- https://hackage.haskell.org/package/djinn
- https://manpages.org/djinn
- https://www.cambridge.org/core/journals/journal-of-symbolic-logic/article/contractionfree-sequent-calculi-for-intuitionistic-logic/C7ABBFC63E74878E52FF4E624029503B
- https://en.wikipedia.org/wiki/Roy_Dyckhoff
- https://en.wikipedia.org/wiki/Curry%E2%80%93Howard_correspondence
- https://prevent-contain-prove.org/
- https://prevent-contain-prove.org/Prevent-Contain-Prove-NDIST-Strategic-Guidance.pdf

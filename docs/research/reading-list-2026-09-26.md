# Reading list for lenses

Date: 2026-09-26. Method: WebSearch and WebFetch. Each title, author and year was checked online.
"Fetched" means the page in the last column was read for this list. "Search listing" means the
facts come from a search result page (publisher, library or index entries), not a fetched page.

Purpose: these are the books and essays to compile into lenses (see the `rwtfm` and `lens-*`
skills in `iss-skills`), so that openDaisugi's design work can argue from each field's own
sources. The concept map that uses them is [`../correspondence.md`](../correspondence.md).

**Status rule.** Every row starts as "preliminary lens from research, planned": a lens built from
public summaries, abstracts and research notes. A row whose full text is not legally free online
carries "full lens when the owner provides a copy". A row whose full text is legally free can
become a full lens from that text, so it keeps the first status only. Free here means free from
the author or publisher. Copies on file-sharing sites do not count.

## Functional programming

| Author | Title (year) | What it would teach openDaisugi | Free online | Status | Checked at |
|---|---|---|---|---|---|
| David Thrane Christiansen | *Functional Programming in Lean* (2023, updated to Lean 4.33) | Lean as a programming language, so the Lean client (`clients/lean/`) can grow from a differential implementation toward proofs about the checker. | Yes | preliminary lens from research, planned | [lean-lang.org](https://lean-lang.org/functional_programming_in_lean/) (fetched) |
| Philip Wadler | "Propositions as Types", *Communications of the ACM* (2015) | Why a proof is a program; the base for any machine-checked claim about the verifier. | Yes (author's PDF) | preliminary lens from research, planned | [Wadler's page](https://homepages.inf.ed.ac.uk/wadler/topics/history.html) (fetched) |
| Gordon Plotkin, Matija Pretnar | "Handlers of Algebraic Effects", ESOP (2009) | Effects as operations with handlers: a formal frame for the gate (refuse an effect) and grafts (replace an effect). | Journal version "Handling Algebraic Effects" on arXiv (1312.1399) | preliminary lens from research, planned | [Edinburgh Research Explorer](https://www.research.ed.ac.uk/en/publications/handlers-of-algebraic-effects) (search listing) |
| John Hughes | "Why Functional Programming Matters", *The Computer Journal* 32(2) (1989) | Modularity from higher-order functions and laziness; how plans and pathways could compose. | Yes (author's copy) | preliminary lens from research, planned | [Oxford Academic](https://academic.oup.com/comjnl/article/32/2/98/543535) (search listing) |
| Harold Abelson, Gerald Jay Sussman, Julie Sussman | *Structure and Interpretation of Computer Programs*, 2nd ed. (year not verified here) | Interpreters, and programs as data; the model for a plan runner such as weave. | Yes (CC BY-SA, MIT Press site) | preliminary lens from research, planned | [MIT Press SICP site](https://mitp-content-server.mit.edu/books/content/sectbyfn/books_pres_0/6515/sicp.zip/index.html) (fetched) |

## Formal methods and logic

| Author | Title (year) | What it would teach openDaisugi | Free online | Status | Checked at |
|---|---|---|---|---|---|
| Aaron R. Bradley, Zohar Manna | *The Calculus of Computation: Decision Procedures with Applications to Verification* (Springer, 2007) | First-order theories and decision procedures from the ground up; what the predicate algebra can and cannot decide. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [ACM DL](https://dl.acm.org/doi/10.5555/1324777) (search listing) |
| Daniel Kroening, Ofer Strichman | *Decision Procedures: An Algorithmic Point of View*, 2nd ed. (Springer, 2016) | DPLL(T), string and bit-vector theories, quantifiers; why Z3 answers "unknown" and how to stay in fragments where it does not. | No (slides free) | preliminary lens from research, planned; full lens when the owner provides a copy | [decision-procedures.org](http://www.decision-procedures.org/) (fetched) |
| Daniel Jackson | *Software Abstractions: Logic, Language, and Analysis*, revised ed. (MIT Press, 2012) | Lightweight formal methods with Alloy: small models, bounded checks, counterexamples first. Close to how envelopes are checked. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [Amazon listing](https://www.amazon.com/Software-Abstractions-Logic-Language-Analysis/dp/0262528908) (search listing; the MIT Press page returned 403) |
| Leslie Lamport | *Specifying Systems: The TLA+ Language and Tools for Hardware and Software Engineers* (Addison-Wesley, 2002) | Specifying behaviour over time; the missing temporal properties (correspondence Part C, gap 3). | PDF free for personal use from the author's site | preliminary lens from research, planned | [Lamport's page](https://lamport.azurewebsites.net/tla/book.html) (fetched) |
| Hillel Wayne | *Practical TLA+: Planning Driven Development* (Apress, 2018), plus his essays | A practical path into TLA+, and essays on why teams do or do not use formal methods. | Book: no. Essays: yes | preliminary lens from research, planned; full lens when the owner provides a copy (book) | [Springer listing](https://link.springer.com/book/10.1007/978-1-4842-3829-5) (search listing); [hillelwayne.com](https://www.hillelwayne.com/) (fetched) |
| Christel Baier, Joost-Pieter Katoen | *Principles of Model Checking* (MIT Press, 2008) | LTL, CTL, automata and model checking in one framework; the base for trajectory contracts over the journal. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [Wikipedia listing](https://en.wikipedia.org/wiki/Principles_of_Model_Checking) (search listing) |
| Jeremy Avigad, Leonardo de Moura, Soonho Kong, Sebastian Ullrich | *Theorem Proving in Lean 4* (Lean 4.33 edition) | Writing proofs in Lean: needed to prove alias unfolding and the encoders (correspondence Part C, gap 1). | Yes | preliminary lens from research, planned | [lean-lang.org](https://lean-lang.org/theorem_proving_in_lean4/) (fetched) |
| Benjamin C. Pierce et al. | *Software Foundations* (7 volumes, Rocq) | Semantics and proofs in a proof assistant, with exercises; the method for a verified checker. | Yes | preliminary lens from research, planned | [softwarefoundations.cis.upenn.edu](https://softwarefoundations.cis.upenn.edu/) (fetched; the page does not list authors, so "Pierce et al." is not verified there) |
| Martin Leucker, Christian Schallhart | "A brief account of runtime verification", *J. Logic and Algebraic Programming* 78(5) (2009) | What runtime verification is and is not, compared with model checking and testing. The field the gate belongs to. | Open access | preliminary lens from research, planned | [dblp](https://dblp.uni-trier.de/rec/journals/jlp/LeuckerS09.html) (search listing) |
| Fred B. Schneider | "Enforceable Security Policies", *ACM TISSEC* 3(1) (2000) | Exactly which policies an execution monitor can enforce; the result under yellow paper §8.1. | Yes (author's PDF) | preliminary lens from research, planned | [Cornell PDF](https://www.cs.cornell.edu/fbs/publications/EnfSecPols.pdf) (fetched) |
| Jay Ligatti, Lujo Bauer, David Walker | "Edit automata: enforcement mechanisms for run-time security policies", *Int. J. Information Security* 4(1-2) (2005) | Monitors that suppress and insert actions: the formal home of grafts. | Not verified | preliminary lens from research, planned; full lens when the owner provides a copy | [Springer](https://link.springer.com/article/10.1007/s10207-004-0046-8) (search listing) |

## Compilers and programming-language theory

| Author | Title (year) | What it would teach openDaisugi | Free online | Status | Checked at |
|---|---|---|---|---|---|
| Benjamin C. Pierce | *Types and Programming Languages* (MIT Press, 2002) | Type systems, subtyping and soundness proofs (progress and preservation); the language for subsumption and envelopes as types. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [TAPL page](https://www.cis.upenn.edu/~bcpierce/tapl/main.html) (fetched); year from [MIT Press](https://mitpress.mit.edu/9780262162098/types-and-programming-languages/) (search listing) |
| Robert Harper | *Practical Foundations for Programming Languages*, 2nd ed. (Cambridge, 2016) | Languages defined by statics and dynamics; how to specify the kernel and dialects as a language, with a definitional view of extensions. | Abbreviated online edition only | preliminary lens from research, planned; full lens when the owner provides a copy | [Harper's page](https://www.cs.cmu.edu/~rwh/pfpl/) (fetched) |
| Martin Fowler, with Rebecca Parsons | *Domain-Specific Languages* (Addison-Wesley, 2010) | Internal vs external DSLs, the semantic model under a DSL; how dialects should look to readers. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [martinfowler.com](https://martinfowler.com/books/dsl.html) (fetched) |
| Urs Hölzle, Craig Chambers, David Ungar | "Debugging Optimized Code with Dynamic Deoptimization", PLDI (1992) | Deoptimization: falling back from optimized code when an assumption breaks. The pattern for pathway guards. | Not verified | preliminary lens from research, planned; full lens when the owner provides a copy | [ACM DL](https://dl.acm.org/doi/10.1145/143103.143114) (search listing) |
| Max Willsey et al. | "egg: Fast and Extensible Equality Saturation", POPL (2021) | E-graphs and equality saturation; deduplicating dialect words by equivalence. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2004.03082](https://arxiv.org/abs/2004.03082) (fetched) |
| Kevin Ellis et al. | "DreamCoder: Growing generalizable, interpretable knowledge with wake-sleep Bayesian program learning" (2020) | Library learning by alternating solving and abstraction. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2006.08381](https://arxiv.org/abs/2006.08381) (fetched) |
| Matthew Bowers et al. | "Top-Down Synthesis for Library Learning" (Stitch), POPL (2023) | The compressor to run over envelopes and plans as trees (dialects first test, next step). | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2211.16605](https://arxiv.org/abs/2211.16605) (fetched) |
| Sumit Gulwani, Oleksandr Polozov, Rishabh Singh | *Program Synthesis*, Foundations and Trends in PL 4 (2017) | Synthesis from examples and specifications; the frame for what the distiller does. | Yes (Microsoft Research PDF) | preliminary lens from research, planned | [Microsoft Research](https://www.microsoft.com/en-us/research/publication/program-synthesis/) (fetched) |

## Control theory, runtime assurance and security

| Author | Title (year) | What it would teach openDaisugi | Free online | Status | Checked at |
|---|---|---|---|---|---|
| Lui Sha | "Using Simplicity to Control Complexity", *IEEE Software* 18(4) (2001) | The Simplex idea from its author: a simple, reliable core and a recovery region. Our lineage, and where our guarantee is weaker. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [ACM DL](https://dl.acm.org/doi/abs/10.1109/MS.2001.936213) (search listing); [reading-group slides](https://engineering.purdue.edu/dcsl/reading/2007/foob-using_simplicity_to_control_complexity.pdf) (fetched) |
| James P. Anderson | *Computer Security Technology Planning Study*, ESD-TR-73-51 (US Air Force ESD, October 1972) | The reference monitor concept: mediate every reference, and be small enough to verify. | Yes (NIST archive PDF) | preliminary lens from research, planned | [NIST CSRC PDF](https://csrc.nist.gov/files/pubs/conference/1998/10/08/proceedings-of-the-21st-nissc-1998/final/docs/early-cs-papers/ande72a.pdf) (search listing); [Wikipedia](https://en.wikipedia.org/wiki/Reference_monitor) (fetched) |
| Jerome H. Saltzer, Michael D. Schroeder | "The Protection of Information in Computer Systems", *Proc. IEEE* 63(9) (1975) | Economy of mechanism, fail-safe defaults, complete mediation, least privilege: the design laws the gate follows. | Yes (online copy) | preliminary lens from research, planned | [UVA copy](https://www.cs.virginia.edu/~evans/cs551/saltzer/) (fetched); venue per [Wikipedia listing](https://en.wikipedia.org/wiki/The_Protection_of_Information_in_Computer_Systems) (search listing) |
| Mark S. Miller | *Robust Composition: Towards a Unified Approach to Access Control and Concurrency Control* (PhD thesis, Johns Hopkins, 2006) | Object capabilities and attenuation; the theory under the agent tree. | Yes (author's PDF) | preliminary lens from research, planned | [JScholarship](https://jscholarship.library.jhu.edu/handle/1774.2/873) (search listing) |
| Kerianne L. Hobbs, Mark L. Mote, Matthew C. Abate, Samuel D. Coogan, Eric M. Feron | "Runtime Assurance for Safety-Critical Systems: An Introduction to Safety Filtering Approaches for Complex Control Systems", *IEEE Control Systems Magazine* 43(2) (2023) | RTA architectures and four families of safety filters; the map for the robot side. | Yes (arXiv 2110.03506) | preliminary lens from research, planned | [NSF PAR](https://par.nsf.gov/biblio/10430836-runtime-assurance-safety-critical-systems-introduction-safety-filtering-approaches-complex-control-systems) (search listing) |
| Aaron D. Ames, Samuel Coogan, Magnus Egerstedt, Gennaro Notomista, Koushil Sreenath, Paulo Tabuada | "Control Barrier Functions: Theory and Applications" (2019) | Enforcing a safe set inside an optimizing controller; the CBF-QP the whitepaper says we lack. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 1903.11199](https://arxiv.org/abs/1903.11199) (fetched) |
| Mohammed Alshiekh, Roderick Bloem, Rüdiger Ehlers, Bettina Könighofer, Scott Niekum, Ufuk Topcu | "Safe Reinforcement Learning via Shielding" (2017) | Shields that block or correct a learner's action against a temporal specification. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 1708.08611](https://arxiv.org/abs/1708.08611) (fetched) |
| Somil Bansal, Mo Chen, Sylvia Herbert, Claire J. Tomlin | "Hamilton-Jacobi Reachability: A Brief Overview and Recent Advances", IEEE CDC (2017) | Reachable sets and safety controllers for dynamics; what a swarm envelope would need beyond boxes. | Yes (arXiv 1709.07523) | preliminary lens from research, planned | [ACM DL](https://dl.acm.org/doi/10.1109/CDC.2017.8263977) (search listing) |
| Matthias Althoff, Goran Frehse, Antoine Girard | "Set Propagation Techniques for Reachability Analysis", *Annual Review of Control, Robotics, and Autonomous Systems* 4 (2021) | Set-based reachability for linear, nonlinear and hybrid systems. | Author preprint | preliminary lens from research, planned | [Annual Reviews](https://www.annualreviews.org/content/journals/10.1146/annurev-control-071420-081941) (search listing) |
| ASTM International | F3269-21, *Standard Practice for Methods to Safely Bound Behavior of Aircraft Systems Containing Complex Functions Using Run-Time Assurance* (2021) | How aviation certifies an RTA architecture around an uncertified function. | No | preliminary lens from research, planned; full lens when the owner provides a copy | [ASTM store](https://store.astm.org/f3269-21.html) (search listing) |
| Scott A. Crosby, Dan S. Wallach | "Efficient Data Structures for Tamper-Evident Logging", USENIX Security (2009) | Logs that prove what they hold; the next step for the journal and receipts. | Yes | preliminary lens from research, planned | [USENIX PDF](https://www.usenix.org/legacy/event/sec09/tech/full_papers/crosby.pdf) (fetched) |

## Agents, LLMs and machine learning

| Author | Title (year) | What it would teach openDaisugi | Free online | Status | Checked at |
|---|---|---|---|---|---|
| Rich Sutton | "The Bitter Lesson" (13 March 2019) | General methods that use computation win; the test every hand-built part of openDaisugi must pass. | Yes | preliminary lens from research, planned | [mirror PDF](https://www.cs.utexas.edu/~eunsol/courses/data/bitter_lesson.pdf) (fetched; the author's site gave a certificate error) |
| Paul Christiano, Jan Leike, Tom B. Brown, Miljan Martic, Shane Legg, Dario Amodei | "Deep Reinforcement Learning from Human Preferences" (2017) | Learning from pairwise human picks; the owner's picks in ranking. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 1706.03741](https://arxiv.org/abs/1706.03741) (fetched) |
| Rafael Rafailov et al. | "Direct Preference Optimization: Your Language Model is Secretly a Reward Model" (2023) | Preference learning without an explicit reward model; a cheap path for a learned router. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2305.18290](https://arxiv.org/abs/2305.18290) (fetched) |
| Lianmin Zheng et al. | "Judging LLM-as-a-Judge with MT-Bench and Chatbot Arena" (2023) | Judge biases and their fixes; how model votes enter ranking. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2306.05685](https://arxiv.org/abs/2306.05685) (fetched) |
| Isaac Ong et al. | "RouteLLM: Learning to Route LLMs with Preference Data" (2024) | Routers learned from preference data; the router's learning step. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2406.18665](https://arxiv.org/abs/2406.18665) (fetched) |
| Guanzhi Wang et al. | "Voyager: An Open-Ended Embodied Agent with Large Language Models" (2023) | A skill library of executable code; the nearest neighbour to pathways. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2305.16291](https://arxiv.org/abs/2305.16291) (fetched) |
| Gabriel Grand et al. | "LILO: Learning Interpretable Libraries by Compressing and Documenting Code" (ICLR 2024) | LLM plus Stitch plus model-written names; the dialect learning loop. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2310.19791](https://arxiv.org/abs/2310.19791) (fetched) |
| Ian Berlot-Attwell, Frank Rudzicz, Xujie Si | "Library Learning Doesn't: The Curious Case of the Single-Use 'Library'" (2024) | The sceptic's bar: measure reuse, count compute. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2410.20274](https://arxiv.org/abs/2410.20274) (fetched) |
| Edoardo Debenedetti et al. | "Defeating Prompt Injections by Design" (CaMeL, 2025) | Control and data flow from the trusted query, capabilities at tool calls; the information-flow gap. | Yes (arXiv) | preliminary lens from research, planned | [arXiv 2503.18813](https://arxiv.org/abs/2503.18813) (fetched) |

## Counts

44 entries in five groups: FP 5, FM 11, PL 8, RTA 11, ML 9. Every entry the task named is
present. Items not in the task that were added as core: Hughes, SICP, Baier and Katoen,
Schneider, Ligatti et al., Hölzle et al., egg, DreamCoder, Stitch, Gulwani et al., Hobbs et al.,
Ames et al., Alshiekh et al., Bansal et al., Althoff et al., ASTM F3269, Crosby and Wallach, and
the ML papers.

## Not verified

- SICP's second-edition year: the MIT Press page fetched gave a year that does not match the
  second edition, so no year is stated.
- *Software Foundations* author names: the site does not list them.
- Free status of the Ligatti et al. and Hölzle et al. papers.
- OpenAI's "Harness engineering" post (HTTP 403), so it is not on the list.

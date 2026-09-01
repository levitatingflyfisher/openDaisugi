# Formal methods for openDaisugi

Date: 2026-09-23. Method: a multi-agent web search. Each claim was checked by three separate votes, and a claim stayed only if at least two votes kept it. Every claim below cites its sources.

## Question

Formal methods that could make openDaisugi stronger. openDaisugi is a local-first runtime-assurance layer for AI coding agents: an LLM writes a checkable safety "envelope" (a restricted predicate algebra over shell commands, file paths, network hosts, MCP tool calls), a Z3/SMT verifier proves each proposed action stays inside it before it runs (fail-closed, Simplex-style runtime assurance after Sha 1996), shell lines are decomposed by a parser into effects, a closed per-command flag allowlist classifies undoable vs permanent effects, and five independent client implementations (Python oracle, Rust, Go, TypeScript, Lean 4) are cross-checked by differential testing over ~11k cases. Research 2025-2026 work on: (1) runtime verification and shielding for LLM agents (e.g. formal policy languages for agent tool use, Cedar/AWS Automated Reasoning checks, Invariant Labs, Progent, AgentSpec, ShieldAgent, "guardrails with proofs"), what they prove and how; (2) verified shell parsing / POSIX shell semantics (Smoosh, CoLiS, Morbig, shellcheck-style analyses) and verified path normalization, and how to get soundness for shell effect classification; (3) making the verifier itself trustworthy: proof-producing SMT (cvc5 proofs, Alethe, Z3 proof checking in Lean/Coq), verified-in-Lean policy evaluators (e.g. Cedar's Lean model and differential testing), translation validation; (4) information-flow / capability approaches (CaMeL-style dual-LLM and data-flow policies, taint tracking) for prompt injection; (5) concrete, ranked recommendations for openDaisugi with cost and payoff. Cite source URLs for every claim.

## Summary

The strongest 2025-2026 agent guardrails check each tool call before it runs and fail closed, as openDaisugi does. Progent and AWS AgentCore (Cedar) are examples. None of the guardrails reviewed here proves each action with SMT. Progent uses Z3 only to find overlapping policies. AgentCore uses automated reasoning to find policies that always allow, always deny or can never be satisfied. AgentSpec and ShieldAgent report empirical or probabilistic results, not soundness. LLM-written policies work but miss cases: Progent-LLM cut attack success from 39.9% to 1.0%, and rules written by o1 in AgentSpec had 70.96% recall. So the cheapest gain for openDaisugi is to check the envelope itself with Z3 before it is used. Two further findings apply. First, a 2026 theory preprint shows that a pre-execution gate can enforce only a limited class of safety policies, and that analysis becomes undecidable once the rules have two counters that can be decremented and tested for zero. Second, ContrAgent shows that LTLf contracts compiled to a DFA can add checks across a whole trajectory. On the shell side, the Morbig, CoLiS and Smoosh work shows that POSIX shell cannot be parsed soundly with a plain tokenize-then-parse pipeline. The sound approach is a parser that uses context, a restricted IR, and a fail-closed refusal of anything outside that subset. Smoosh can serve as an executable semantic oracle. No claims about proof-producing SMT, Lean-verified evaluators or CaMeL-style information flow survived verification, so those parts of the question are still open.

## Findings

### 1. Progent is the closest peer to openDaisugi. It has a deterministic JSON-Schema policy DSL with allow and forbid effects, regex and set-membership conditions, and priorities in which forbid wins ties. It checks each call before it runs and blocks by default when no policy matches. It uses Z3 only in a static overlap analyzer that runs when policies are written. At runtime it evaluates JSON Schema and does no per-action SMT proof, so its SMT use is narrower than openDaisugi's.

Confidence: medium. Vote: 3-0, 3-0.

Evidence: Claims 0 and 1, both voted 3-0, from one primary paper. Quote: 'If no policy matches, the tool call is blocked for security'. Also: 'Overlap analyzer uses Z3 to check if conditions from different policies can be satisfied simultaneously'. Runtime checks use the Python jsonschema library. The guarantees hold only if the policies themselves are correct.

Sources:
- https://arxiv.org/html/2504.11703v2

### 2. LLM-written deterministic policies work, but they are incomplete. With gpt-4o writing the policies, Progent-LLM cut AgentDojo attack success from 39.9% to 1.0%. Utility fell from 79.4% to 76.3%. Hand-written policies reached 0%. In AgentSpec, rules written by OpenAI o1 had 95.56% precision but 70.96% recall on embodied agents, and they caught only 87.26% of risky code. This supports openDaisugi's premise that an LLM can write the envelope. It also shows that the envelope needs its own checks.

Confidence: medium. Vote: 3-0, 3-0.

Evidence: Claims 2 and 5, both voted 3-0, from two independent primary papers. AgentDojo tests tool-using assistants, not shell or file effects in coding agents, so it carries over to openDaisugi only by analogy. The AgentSpec figures cover one model on the paper's own benchmarks.

Sources:
- https://arxiv.org/html/2504.11703v2
- https://arxiv.org/abs/2503.18666

### 3. AgentSpec and ShieldAgent give empirical or probabilistic assurance, not sound proofs. AgentSpec is a runtime DSL of triggers, predicates and enforcement actions. Its predicates are Python functions, and it uses no SMT. It blocks unsafe code execution in 'over 90%' of cases. ShieldAgent turns natural-language policy documents into probabilistic rule circuits and checks an agent's action trajectory, not its prompts or text outputs. It accepts an action when a Markov Logic Network scores P(safe) - P(unsafe) at or above a threshold the user sets. openDaisugi's sound SMT check over a closed predicate algebra is a stronger kind of guarantee than either.

Confidence: medium. Vote: 3-0, 3-0, 2-1, 3-0.

Evidence: Claims 3 and 4 (3-0) and claims 6 (2-1) and 7 (3-0). Both are primary 2025 papers (AgentSpec at ICSE 2026, ShieldAgent at ICML 2025). The negative parts of these claims rest on the papers not mentioning SMT, not on a statement that they avoid it. Claim 6 had a split vote, because 'does not filter prompts or outputs' is an inference.

Sources:
- https://arxiv.org/abs/2503.18666
- https://arxiv.org/abs/2503.22738

### 4. AWS Bedrock AgentCore Policy writes agent tool permissions in Cedar and enforces them at the AgentCore Gateway by intercepting each tool call before it runs. Users can write policies in natural language. The system turns the text into candidate Cedar policies, checks them against the tool schema, and uses automated reasoning to flag policies that always allow, always deny, or can never be satisfied. This check analyzes the policy as a whole at authoring time, not single requests. openDaisugi has no equivalent check on its envelopes today, so this is the most direct pattern to copy.

Confidence: high. Vote: 3-0, 3-0, 3-0.

Evidence: Claims 10, 11 and 12, all voted 3-0. The verifiers also checked the AgentCore developer guide (policy-core-concepts), the AWS Security blog post on choosing Cedar, and a third-party write-up. Enforcement at runtime is plain Cedar evaluation, not SMT. The automated reasoning is a narrow check for always-allow and always-deny, not a comparison against what the user meant. The feature was in preview in December 2025 and reached GA in 2026.

Sources:
- https://aws.amazon.com/blogs/aws/amazon-bedrock-agentcore-adds-quality-evaluations-and-policy-controls-for-deploying-trusted-ai-agents/

### 5. Theory result: given fixed oracle predicates, a deterministic pre-execution gate that can allow, block or substitute can enforce, soundly and transparently, exactly the nonempty safety policies whose good-prefix language its register automaton recognizes. This class is strictly smaller than what edit automata can enforce. Blocking is a safe default in the model of external effects. Static analysis of the rule language is undecidable once guards have two counters that can be decremented and tested for zero, by reduction from Minsky machines. It is in PSPACE for a separable, key-local, monotone-counter fragment that covers caps, quotas and revocation, provided no guard compares one counter with another. openDaisugi's envelope algebra should stay inside that fragment if its linting is to stay decidable.

Confidence: medium. Vote: 3-0, 3-0.

Evidence: Claims 8 and 9, both voted 3-0, from one primary source: a single-author arXiv preprint from July 2026 that has not been peer reviewed. It builds on established theory (Schneider 2000, Ligatti et al. 2005/2009), and the undecidability proof is given in full. One caveat: 'blocking is always safe' ignores latency and retries, and a gate that blocks everything is safe but useless.

Sources:
- https://arxiv.org/pdf/2607.22868

### 6. ContrAgent (Xiao and Nuzzo, UC Berkeley) goes beyond per-call checks. It writes trajectory-level assume-guarantee contracts in LTLf over a fixed set of checkable tool-call predicates. Each contract compiles to a DFA, and the same DFA both blocks actions online and grades recorded traces offline, with deterministic verdicts. Numeric bounds such as call counts use arithmetic LTLf. Counters are evaluated pointwise during grounding and kept out of the DFA, so the DFA stays finite. The paper describes per-action checkers such as AgentSpec as scoped to single actions, without reasoning about order, history or counts. openDaisugi's per-action envelope has the same limit.

Confidence: medium. Vote: 3-0, 3-0.

Evidence: Claims 13 and 14, both voted 3-0. This is a v1 preprint posted on 16 Sep 2026, one week before this report, and it is not peer reviewed. The paper claims only parity with its baselines. Its description of AgentSpec could be disputed, because user-written predicates might be able to track history.

Sources:
- https://arxiv.org/pdf/2609.18128

### 7. POSIX shell cannot be parsed soundly by a separate tokenize-then-grammar pipeline. Tokens cannot be recognized with regular expressions, lexing depends on both the parsing context and the evaluation context, and the POSIX grammar is ambiguous. The language mixes syntactic analysis with execution by expansion, so constructs such as eval, alias and dynamic expansion fall outside what a static parser can decide. Morbig gets a sound static parse for a defined, realistic subset by coupling the lexer to parser state and parsing speculatively. For openDaisugi, an ad hoc or regex tokenizer feeding effect classification is a real soundness risk. Anything outside a declared subset must be refused (fail closed), not approximated.

Confidence: high. Vote: 3-0, 2-1.

Evidence: Claim 17 (3-0) and claim 18 (2-1). The source is the Morbig paper (SLE 2018, DOI 10.1145/3276604.3276615), confirmed through the ACM and researchr indexes, and the 2020 COLA journal version repeats the result. The HAL PDF was blocked, so the wording was checked against the abstract only. Claim 18's phrase 'any static parse is an approximation' goes too far: inside Morbig's subset the parse is exact.

Sources:
- https://hal.science/hal-01890044/document

### 8. CoLiS shows a sound design for analyzing real shell. Morbig parses a script statically. The result is translated into a small, restricted IR (the CoLiS language). The tool then runs symbolic execution over feature-tree constraints, which model the file system abstractly, to find policy violations without running the script. Across more than 28,000 Debian maintainer scripts it produced over 150 bug reports. This parser, IR and symbolic-effects pipeline is the model to follow in front of openDaisugi's effect classifier. Scripts outside the subset are left unanalyzed, and openDaisugi would need to refuse them. The claim that CoLiS's interpreter is formally verified in Why3 was refuted and should not be cited.

Confidence: high. Vote: 3-0, 3-0, 3-0.

Evidence: Claims 19, 20 and 21, all voted 3-0. Sources are STTT 2022 and TACAS 2020, and the colis-anr GitHub repositories corroborate them. The Springer page was behind a login, so it was checked through Crossref metadata and the open-access TACAS paper. CoLiS finds bugs; it does not prove scripts correct. The Why3 claim failed verification 1-2.

Sources:
- https://link.springer.com/article/10.1007/s10009-022-00671-1
- https://pmc.ncbi.nlm.nih.gov/articles/PMC7480694/

### 9. Smoosh is a formal, mechanized, executable small-step semantics for POSIX shell. It was tested with three suites (the POSIX suite, Modernish, and the authors' own) against bash, dash, zsh, OSH, mksh, ksh93 and yash, and it was the most POSIX-conformant of them. It could be added to openDaisugi's existing differential harness as a reference oracle for expansion and evaluation. It has limits: it parses with libdash, which is not a verified parser; it models POSIX sh, not bashisms such as [[ ]], arrays or process substitution; and an adapter would be needed to map its traces onto openDaisugi's effect classes.

Confidence: high. Vote: 3-0, 3-0.

Evidence: Claims 15 and 16, both voted 3-0. The source is POPL 2020 (DOI 10.1145/3371111), and the github.com/mgree/smoosh repository confirms it is executable. The conformance result is the authors' own evaluation against 2019 shell versions. Whether the project is still maintained was not checked.

Sources:
- https://arxiv.org/abs/1907.05308

### 10. File-write targets often do not exist yet, so std::fs::canonicalize fails on them. Purely lexical normalization ignores symlinks in the parent directories that do exist. soft-canonicalize (Rust) resolves symlinks and '..' for paths that do not exist yet, the way Python's pathlib.Path.resolve(strict=False) does, with bounded detection of symlink cycles. Its anchored mode clamps every path, including symlink targets, so it cannot go above an anchor root. This is the path semantics openDaisugi needs for write checks, and a Python equivalent exists for the oracle. Canonicalizing a path does not close the TOCTOU race between the check and the write.

Confidence: medium. Vote: 3-0, 2-1.

Evidence: Claim 22 (3-0) and claim 23 (2-1). The source is the library's own README, so its claims are self-reported and not formally verified. Claim 23 mixes up two features: root, share and device clamping applies to the whole library, and anchor clamping applies to the anchored mode. The README states the Windows 8.3 short-name limit itself.

Sources:
- https://github.com/DK26/soft-canonicalize-rs

### 11. Ranked recommendations for openDaisugi. These rankings are an inference from the findings above, and the cost and payoff figures are estimates. (1) Envelope-level Z3 lint, low cost and high payoff. Before an envelope is used, reject it or warn if it is vacuous, always allows, always denies, has overlapping conflicting rules, or is broader than a baseline envelope. This is the AgentCore and Progent pattern, and it answers the recall gap in LLM-written policies. (2) An explicit fail-closed shell subset, medium cost and high payoff. Write down the constructs the parser accepts. Refuse eval, alias, dynamic command names and unresolved expansions. Couple the lexer to parser state as Morbig does, and lower each accepted line to a small IR before effect classification, as CoLiS does. (3) Smoosh as a sixth differential oracle for POSIX expansion, medium cost and medium payoff, while tracking bashisms separately. (4) Symlink-aware soft canonicalization for write targets, plus a no-follow open at write time to narrow TOCTOU, low to medium cost and medium payoff. (5) Trajectory contracts written in LTLf, compiled to a DFA and run over the action journal, both online and in offline grading, with counters kept outside the DFA and the rules kept inside the decidable monotone-counter fragment. This has higher cost and adds a class of properties per-action checks cannot express, such as ordering, history and quotas.

Confidence: low. Vote: synthesis.

Evidence: This finding combines claims 0 to 23. No source measures these ranking judgments directly. The cost estimates assume openDaisugi already has a Z3 verifier, a shell parser, a five-client differential harness and an action journal, as the research question states.

Sources:
- https://aws.amazon.com/blogs/aws/amazon-bedrock-agentcore-adds-quality-evaluations-and-policy-controls-for-deploying-trusted-ai-agents/
- https://arxiv.org/html/2504.11703v2
- https://arxiv.org/abs/2503.18666
- https://hal.science/hal-01890044/document
- https://link.springer.com/article/10.1007/s10009-022-00671-1
- https://arxiv.org/abs/1907.05308
- https://github.com/DK26/soft-canonicalize-rs
- https://arxiv.org/pdf/2609.18128
- https://arxiv.org/pdf/2607.22868

## Caveats

Two parts of the research question have no verified evidence, because no claims about them survived verification. Part (3) is about making the verifier itself trustworthy: cvc5 or Alethe proof certificates, Z3 proof checking in Lean or Coq, Cedar's Lean model and its differential testing, and translation validation. Part (4) is about information-flow and capability defenses against prompt injection: CaMeL dual-LLM data-flow policies and taint tracking. Invariant Labs was also not covered. The recommendations therefore say nothing about those areas. Several key sources are recent preprints that have not been peer reviewed: the gate-enforceability theory (2607.22868, July 2026, single author) and ContrAgent (2609.18128, posted one week before this report). The AgentCore evidence comes from vendor material, and the product was in preview until 2026. The soft-canonicalize evidence is the library's own README. The shell-semantics work (Morbig 2018, Smoosh 2020, CoLiS 2020-2022) is older, but its results are structural facts about POSIX shell. It covers POSIX sh, not bash, and coding agents often write bashisms. The AgentDojo, AgentSpec and ShieldAgent benchmarks measure tool-using assistants, not shell or file effects in coding agents, so their numbers carry over to openDaisugi only by analogy. The claim that CoLiS's symbolic interpreter is verified in Why3 was refuted and is excluded. The ranked recommendations are the synthesizer's judgment, not measured results.

## Openquestions

- Can openDaisugi emit and check an SMT proof certificate (cvc5 Alethe proofs, or a Z3 proof replayed in Lean 4) for each allowed action within its latency budget of about 0.55 s per round trip, or only for sampled or offline audits?
- Does openDaisugi's current predicate algebra, including any quota or counter-like conditions, fall inside the decidable separable monotone-counter fragment (T0) of 2607.22868, and can its Lean 4 client prove this?
- How should CaMeL-style data-flow or taint policies, for example 'no network egress after reading a secret path', be combined with per-action envelopes? Are they better expressed as ContrAgent-style LTLf contracts over the journal?
- Which bash constructs that agents often write (such as [[ ]], arrays, process substitution and brace expansion) can be given a checked semantics, given that Smoosh and CoLiS cover only POSIX sh?

## Refuted

- {"claim": "The symbolic interpreter at the core of CoLiS is formally verified in the Why3 framework, so the analyzer itself carries machine-checked correctness guarantees.", "vote": "1-2", "source": "https://link.springer.com/article/10.1007/s10009-022-00671-1"}

## Unverified


## Sources

- {"url": "https://arxiv.org/html/2504.11703v2", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://arxiv.org/abs/2503.18666", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://arxiv.org/abs/2503.22738", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2607.22868", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://aws.amazon.com/blogs/aws/amazon-bedrock-agentcore-adds-quality-evaluations-and-policy-controls-for-deploying-trusted-ai-agents/", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2609.18128", "quality": "primary", "angle": "Agent shielding and policy languages (state of the art)", "claimCount": 5}
- {"url": "https://arxiv.org/abs/1907.05308", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 5}
- {"url": "https://hal.science/hal-01890044/document", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 5}
- {"url": "https://link.springer.com/article/10.1007/s10009-022-00671-1", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 5}
- {"url": "https://pmc.ncbi.nlm.nih.gov/articles/PMC7480694/", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 4}
- {"url": "https://github.com/DK26/soft-canonicalize-rs", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2608.06508", "quality": "primary", "angle": "Verified shell semantics and path normalization", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2407.01688", "quality": "primary", "angle": "Trustworthy verifier: proof-producing SMT and verified evaluators", "claimCount": 5}
- {"url": "https://www.amazon.science/blog/how-we-built-cedar-with-automated-reasoning-and-differential-testing", "quality": "primary", "angle": "Trustworthy verifier: proof-producing SMT and verified evaluators", "claimCount": 5}
- {"url": "https://link.springer.com/chapter/10.1007/978-3-032-32526-6_9", "quality": "primary", "angle": "Trustworthy verifier: proof-producing SMT and verified evaluators", "claimCount": 5}
- {"url": "https://github.com/ufmg-smite/carcara", "quality": "primary", "angle": "Trustworthy verifier: proof-producing SMT and verified evaluators", "claimCount": 4}
- {"url": "https://arxiv.org/abs/2503.18813", "quality": "primary", "angle": "Information flow and capabilities for prompt injection", "claimCount": 5}
- {"url": "https://arxiv.org/abs/2505.23643", "quality": "primary", "angle": "Information flow and capabilities for prompt injection", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2609.18674", "quality": "primary", "angle": "Information flow and capabilities for prompt injection", "claimCount": 5}
- {"url": "https://github.com/MervinPraison/PraisonAI/security/advisories/GHSA-cv3g-hj65-pcfh", "quality": "primary", "angle": "Limits, bypasses and industry adoption (skeptical/practitioner)", "claimCount": 5}
- {"url": "https://github.com/agno-agi/agno/issues/7103", "quality": "forum", "angle": "Limits, bypasses and industry adoption (skeptical/practitioner)", "claimCount": 5}
- {"url": "https://adversa.ai/blog/opensource-ai-coding-agents-shell-injection-vulnerability/", "quality": "blog", "angle": "Limits, bypasses and industry adoption (skeptical/practitioner)", "claimCount": 5}
- {"url": "https://arxiv.org/pdf/2509.22040", "quality": "primary", "angle": "Limits, bypasses and industry adoption (skeptical/practitioner)", "claimCount": 5}

## Stats

{
 "angles": 5,
 "sourcesFetched": 23,
 "claimsExtracted": 113,
 "claimsVerified": 25,
 "confirmed": 24,
 "killed": 1,
 "unverified": 0,
 "afterSynthesis": 11,
 "urlDupes": 0,
 "budgetDropped": 7,
 "agentCalls": 105
}

# Harness research, 2026-09-28

Deep-research run (110 agents; claims checked by three votes). Question: what might sprig be missing? Raw report kept outside the repo; this file is its verified content.

## Summary

The evidence does not show that sprig lacks some loop feature the leading harnesses have. Production harnesses are hand-rolled async loops. They find code with deterministic tools: ripgrep, tree-sitter, glob and Markdown context files. They do not use agent frameworks or vector search (arXiv 2609.00006, a study of eleven systems). A roughly 100-line loop (Mini-SWE-Agent) reports SWE-bench Verified scores close to systems 1000 times larger. With the model fixed, changing the harness changed tokens per solved task by up to 40 times, but pass rates moved only 0 to 8 points. So a harness pays off in cost, safety, recovery and behaviour under resource pressure, not in the number of tasks solved. The strongest independent gains come from outside the plain loop: search over trajectories, parallel attempts ranked by a verifier, and training on trajectories. These are the places where sprig's parallel search, ranking and verified 'done' can show value. Two confirmed claims set bounds for sprig. Codex sandboxes only its own shell tool and leaves MCP tools unguarded. OpenDev's 'done' is a todo-list check, not a verification. So a gate that covers every tool, and a 'done' check that is more than a heuristic, are real differences. But the survey does not prove that no harness has them, because the claim that verified-done is rare was refuted.

## Findings

### 1. (medium, vote 3-0)

Production coding harnesses are hand-rolled async loops with deterministic retrieval (ripgrep, tree-sitter, glob, auto-discovered Markdown context files). None of the eleven studied imports a general agent framework (LangChain, LangGraph, AutoGen and similar), and none retrieves code with vector embeddings. For sprig: a small Go loop with plain search tools matches field practice, so this is not a gap.

Evidence: Independent source-code study (Wavestone AI Lab, July 2026) covered Claude Code, Codex CLI, Gemini CLI, Mistral Vibe, OpenHands, Aider, Mini-SWE-Agent, Hermes, Pi, OpenCode and OpenClaw, about 4M lines in total. Scope: the claim holds only for these eleven. Cursor, Cline/Roo, Amp, Goose, Devin and Grok Build were not studied, and some of them offer embedding-based code indexing. The claim covers code retrieval only. Memory features can still use vectors. The Claude Code analysis used a March 2026 source snapshot. Single primary source, vote 3-0.

Sources: https://arxiv.org/pdf/2609.00006

### 2. (high, vote 3-0 (both claims))

How sophisticated a loop is does not predict pass rate. Mini-SWE-Agent (about 100 lines, one tool: bash) reports SWE-bench Verified results close to production systems. With the model held fixed, switching among Goose, OpenCode and OpenHands-SDK changed tokens per solved task by up to 40 times, while paired pass-rate differences stayed at 0 to 8 points and were mostly not significant. Production harnesses differ in safety, recovery, cost control and extensibility, not in tasks solved. For sprig, the case to make is on cost and assurance. Pass rate alone will not show sprig's value.

Evidence: 2609.00006 states that the floor differs from production systems 'not [in] task completion but ... safety, recovery, cost management, extensibility'. Its spring-2026 figures are self-reported: OpenHands 77.6%, Mini-SWE-Agent 74%+, Claude Code 72.7%, Codex 69.1%. The authors removed per-system scores from their table because the scores are not comparable. 2607.22585 (Vats and Golev, June 2026): 2 models, 3 open-source harnesses, 50 Terminal-Bench Pro tasks. The 95% paired bootstrap intervals include zero except for the largest gap. Proprietary harnesses were not tested, and n=50 has low power.

Sources: https://arxiv.org/pdf/2609.00006, https://arxiv.org/pdf/2607.22585, https://github.com/swe-agent/mini-swe-agent

### 3. (medium, vote 3-0 (tight-context gain); 2-1 (vanishes at large context))

Context management by the harness helps a lot under tight context and almost nothing when context is ample. In the Yuj harness, with the same model and a 20,480-token window, shortening old tool results and adding stall detection and output safeguards raised mean fail-to-pass fraction from 28% to 49% on 169 SWE-bench Verified tasks. Complete solutions rose from 43 to 72. At 262,144 tokens the gain mostly disappeared on Verified and Pro (difference -0.3 points, CI [-4.5, +3.9]). For sprig: staged compaction is worth building for local and small-window models, and has little value for frontier models with large windows.

Evidence: Single-author preprint (Lewis, Aug 2026). The author is independent of any vendor but built the harness under test. It uses exact McNemar and sign tests and a bootstrap by repository. The effect has the same direction on SWE-bench Pro (15% to 33%) and FeatureBench. FeatureBench kept some gain at 262k. There is one greedy trajectory per task. All models were open-weight models served locally, including Qwen3.6-35B-A3B. Hosted frontier models were not tested, so 'matters for local models' is partly extrapolated. The pressure claim got a 2-1 vote. The claim that stall detection by itself measurably helped was refuted (0-3), so the effect of that single part is not isolated.

Sources: https://arxiv.org/html/2608.26218

### 4. (low, vote 3-0)

One published design for staged compaction (OpenDev) steps up as context fills. It warns at 70%, masks old observations at 80%, prunes fast at 85%, masks aggressively at 90%, and runs full LLM compaction at 99%. It archives the full history to a file the agent can read back. The author reports about 54% lower peak observation context over typical 30-turn sessions, and says tool outputs take 70 to 80% of context in his experience. This is a concrete template sprig could adopt: journal-backed recall of compacted history fits sprig's journal well.

Evidence: Single-author preprint (Bui, March 2026) describing the author's own Rust harness. The 54% figure comes with no method, dataset or baseline. The 70 to 80% figure is framed as the author's experience. Vote 3-0 on what the paper says, which shows only that the design exists, not that it works.

Sources: https://arxiv.org/pdf/2603.05344

### 5. (medium, vote 3-0)

The agent-computer interface (tools, commands, feedback format) is a measurable lever with the model held fixed. SWE-agent ablations with GPT-4 Turbo moved SWE-bench Lite resolve rate from about 11% with a bare shell to about 18% with the full interface. Removing the edit linter, or changing viewer window size or search output format, each moved results by several points. The effect appears to shrink with stronger models: bash-only mini-SWE-agent is competitive in 2026. For sprig: design tool output and edit guardrails with care, but expect smaller gains on frontier models.

Evidence: Peer-reviewed (NeurIPS 2024) with controlled ablations. The verifier cited the ablation figures from memory and did not re-fetch them. The broader claim that agents need purpose-built interfaces rather than a raw shell was refuted (0-3), which fits the later bash-only results.

Sources: https://arxiv.org/abs/2405.15793, https://github.com/swe-agent/mini-swe-agent

### 6. (high, vote 3-0 (all four claims))

Search over trajectories, and parallel attempts ranked by a verifier, give the largest independent gains in the evidence. SWE-Search (MCTS with three roles: an explorer agent, a Value Agent that gives a numeric score plus written feedback, and a Discriminator Agent that picks the final candidate by debate) reports a 23% relative improvement across five models. SWE-Gym verifiers trained on trajectories enabled best-of-N selection that reached 32.0% on Verified and 26.0% on Lite with open weights. R2E-Gym hybrid verifiers (execution-based plus execution-free) lifted a 32B agent from 34.4% pass@1 to 51% on Verified. None of the eleven studied production harnesses was confirmed to ship native tree search or verifier-ranked best-of-N. This is the main opening for sprig's native parallel search with ranking.

Evidence: Three primary academic papers, all voted 3-0. SWE-bench Lite was the SWE-Search test set. Its baseline is the authors' own moatless-tools agent, and compute cost is not normalised. All figures are 2024-25 results on older models. Best-of-N costs about N times the compute. The gains shown come from learned or hybrid verifiers, not formal ones. Two refinements were refuted: that each verifier type saturates at about 42 to 43% (1-2), and the failure-mode account that tests alone cannot rank candidates (0-3). So the evidence does not settle which kind of verifier sprig should use. That there are no native search features in production harnesses is inferred from the architecture survey, not stated as a finding.

Sources: https://arxiv.org/abs/2410.20285, https://arxiv.org/abs/2412.21139, https://arxiv.org/abs/2504.07164

### 7. (medium, vote 3-0)

Trajectory learning measurably improves agents. Training on trajectories from 2,438 executable real-world Python tasks (SWE-Gym) gave up to 19 points absolute gain in resolve rate on SWE-Bench Verified and Lite. This supports sprig's garden, which distills repeated work into reusable pathways. But the published evidence covers training model weights, not skill libraries learned at run time.

Evidence: ICML 2025. The base was a weak open-weight model (Qwen2.5-Coder). 'Up to' is the best case. Absolute scores stay low. No confirmed claim in this run measured skill libraries or harness-level memory, so the garden's approach has no direct support or refutation here.

Sources: https://arxiv.org/abs/2412.21139

### 8. (low, vote 3-0)

Termination in at least one documented harness is a heuristic, not a verification. OpenDev ends on one of four signals: a completion tool with a status, a text-only reply, three consecutive failed recovery attempts, or an iteration cap. If todo items are still open it nudges again before it accepts done. Nothing runs tests or checks the output. A proven 'done' predicate, checked by the gate before a completion is accepted, would be a real difference for sprig.

Evidence: Single-author self-description of one system (OpenDev). Three broader claims were refuted and cannot be used: that only two of eleven harnesses verify done outside the loop, via an OpenHands LLM judge and a Hermes verify-on-stop guard (0-3); that Codex ends a turn only when tool calls stop, with no external check (0-3); and that harnesses have distinct failure-mode fingerprints (0-3). So the idea that verified-done is rare across the field is plausible but NOT established.

Sources: https://arxiv.org/pdf/2603.05344

### 9. (medium, vote 3-0)

Codex applies its sandbox only to its own shell tool. MCP-provided tools are not sandboxed by Codex and must enforce their own guardrails. A gate that checks every tool call, whatever its source (shell, MCP, sub-agent), against one envelope would close a gap that at least one leading harness leaves open by design.

Evidence: Vendor primary source, which is the strongest source for its own design. The verifier could not re-fetch it (HTTP 403). An independent sandbox analysis agrees: MCP servers are child processes of the main Codex process, which is not sandboxed. The post dates from early 2026 and Codex changes quickly. This is a single-vendor fact. Other harnesses were not checked for the same gap.

Sources: https://openai.com/index/unrolling-the-codex-agent-loop/, https://agent-safehouse.dev/docs/agent-investigations/codex

## Refuted claims

- (0-3) Only two of the eleven harnesses check that work is done outside the turn loop (the 'Outer Verification Loop' pattern). OpenHands' /goal endpoint runs an LLM judge. Hermes has a verify-on-stop guard that turns a final text answer back into a continuation when the turn edited code files but produced no fresh verification evidence. So a verified 'done' is rare, and where it exists it rests on an LLM judge or a heuristic, not a formal proof. (https://arxiv.org/pdf/2609.00006)
- (0-3) Codex CLI decides a turn is done only when the model stops asking for tools. The final assistant message is the termination signal. There is no external check that the work is actually finished. (Vendor self-description.) (https://openai.com/index/unrolling-the-codex-agent-loop/)
- (0-3) Codex keeps every request stateless. It resends the full history each time and does not use previous_response_id, because stateless requests support Zero Data Retention. The resent JSON grows quadratically, and prompt caching makes sampling linear only on exact-prefix cache hits. (https://openai.com/index/unrolling-the-codex-agent-loop/)
- (1-2) OpenDev's loop detection catches a stuck agent within 3 repeats. It fingerprints each (tool name, arguments) pair over the last 20 calls. The first repeat triggers an injected warning and skips the tool call. A second repeat triggers a real execution halt with an Allow/Break prompt to the user. The author argues that a real halt is more robust than a warning alone. This is the author's claim, not independent evidence. (https://arxiv.org/pdf/2603.05344)
- (0-3) LM agents should be treated as a new category of end user that needs purpose-built interfaces, not human tools such as a raw shell or an IDE. (https://arxiv.org/abs/2405.15793)
- (1-2) Most LLM software agents run a linear, sequential loop with no backtracking, and the paper names this as a core limitation that tree search fixes. (https://arxiv.org/abs/2410.20285)
- (1-2) For best-of-N selection over SWE-agent trajectories, execution-based (test) verifiers and execution-free (model-judge) verifiers each saturate at about 42-43% on SWE-Bench Verified. Combining them gives much larger gains. This is independent academic evidence, not a vendor claim. (https://arxiv.org/abs/2504.07164)
- (0-3) The two verifier types fail in different ways. Test-based verifiers have low distinguishability: many candidates pass the same tests. Execution-free verifiers are biased toward style. So tests alone cannot rank candidates or certify that work is 'done'. (https://arxiv.org/abs/2504.07164)
- (0-3) Stall detection means watching for the same command failing again and again, or for reading that no longer changes anything. When it fires, the harness tells the model that the same attempt gave the same failure and to take a different action. This hand-coded loop intervention measurably improved results. (https://arxiv.org/html/2608.26218)
- (0-3) Failure-mode fingerprints belong to the harness, not the model, and replicate across both models: Goose stops when stuck (REASON, zero VERIFY), OpenHands-SDK declares done on wrong answers or runs out of turns (VERIFY/MAX TURNS), OpenCode never declares a wrong done but times out or idles (TIME/HANG). This bears directly on termination and verified-done design. (https://arxiv.org/pdf/2607.22585)
- (0-3) Harness choice moves cost far more than a model upgrade does: 40x tokens/solved task for harness vs 1.0-1.3x for model change, with pass-rate effects of comparable magnitude (0-8 pp harness vs 4-10 pp model). (https://arxiv.org/pdf/2607.22585)

## Caveats

Coverage is uneven. Only 14 claims survived, and none covers Claude Code, pi, Cursor, Grok Build, Amp, Goose, Cline/Roo, Devin or Gemini CLI feature by feature. The requested topics of sub-agents and delegation, memory across sessions, skills and plugins, extension APIs, headless/RPC modes, Reflexion, LATS and the bitter-lesson argument got no confirmed claims. So this is not a complete survey of harness features, and a 'missing feature' list for sprig cannot be derived from it. The gap list is therefore inferred from what the evidence does and does not show. In particular, the claims that verified 'done' is rare and that harnesses have distinct termination failure modes were refuted, so sprig cannot claim to be unique there without a direct check. Several sources are single-author 2026 preprints without peer review: OpenDev 2603.05344, Yuj 2608.26218 and Vats/Golev 2607.22585, which has n=50 and 2 models. The search and verifier papers are from 2024-25, and their absolute numbers are well below 2026 frontier scores. Benchmark scores for production harnesses are self-reported and not comparable. SWE-bench-style success rates may be inflated (SWE-ABS, arXiv 2603.00520). The gains from context management were shown only for small open-weight models served locally.

## Open questions

- Do any current harnesses (Claude Code, pi, Amp, Cursor, Devin) ship native best-of-N or tree search with verifier ranking, or a 'done' check that runs outside the loop? The refuted claim leaves this open, and it decides whether sprig's parallel search and verified-done are really unique.
- Can a formal or test-based verifier match the learned and hybrid verifiers that drove the SWE-Gym and R2E-Gym gains, and at what compute cost per solved task, given that harness choice already swings tokens by up to 40 times?
- Do run-time skill libraries or distilled pathways (sprig's garden) improve pass rate or cost measurably without retraining the model? Only weight-level trajectory training was confirmed.
- How do leading harnesses structure sub-agent delegation and permission inheritance? No confirmed evidence covered this, and it bears directly on sprig's proven delegation trees.

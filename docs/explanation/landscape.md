# The landscape

Where openDaisugi sits among other agent tools, as of October 2026. openDaisugi gives the
30,000 ft view of the slots in an agent stack. You can fill a slot with our piece, with
someone else's, or with your own. See [the modular stack](modular-stack.md) for the slot rule.

How to read this page. It came from web research, and most rows rest on search summaries or
third-party posts, not on a full read of the source. "Unverified" marks a gap. Treat a row as a
pointer to follow, not as a fact to quote. Prices, licences and acquisition notes change.

## Safety: permissions and gates

### Harness permission systems

| Name | What it does | How it differs from a Z3-proved envelope | URL |
|---|---|---|---|
| Claude Code sandbox | Seatbelt (macOS) and bubblewrap (Linux/WSL2) confine Bash and children. Read/Edit tools use normal permission rules. By default, if the sandbox cannot start it warns and runs unsandboxed. `sandbox.failIfUnavailable` makes it fail closed. | OS-level confinement of what a process can touch. No per-call proof against a task-specific envelope. Default is fail open. | https://code.claude.com/docs/en/sandboxing |
| Claude sandbox-runtime | Beta package that wraps the whole process, including hooks and MCP servers. Denies write and network by default. | Same: a fixed, coarse boundary, not a per-task checked contract. | https://code.claude.com/docs/en/sandbox-environments.md |
| Codex CLI | Sandbox modes read-only, workspace-write, danger-full-access. Separate approval policy (untrusted, on-request, never). Linux uses Landlock plus seccomp; a bubblewrap path exists behind a flag (sources disagree on default). | Static modes chosen by the human. No generated per-task envelope, no solver. Only third-party mirrors found, official page not fetched. | https://www.mintlify.com/openai/codex/concepts/sandboxing (mirror) |
| Gemini CLI | Policy engine: TOML rules allow, deny or ask before a tool call. Optional sandbox (Docker/Podman/Seatbelt per third parties). | Human-written rules matched by pattern. No LLM-authored envelope, no proof. | https://geminicli.com/docs/reference/policy-engine/ ; https://geminicli.com/docs/cli/sandbox/ |
| OpenCode | `permission` config: allow, ask, deny per tool, glob patterns, last match wins. Third-party guide says it is a policy profile, not a sandbox. | Pattern rules, no semantics of arguments, no proof. | https://opencode.ai/en/docs/permissions |
| Cursor | Terminal sandbox on by default plus allowlist and run modes (Auto-review, Allowlist, Run Everything). Forum reports say allowlist and sandbox interact oddly. | Product setting plus OS sandbox. User can add a command to the allowlist to run it unrestricted. | https://cursor.com/docs/agent/security/run-modes |

### Policy engines and gateways

| Name | What it does | How it differs from a Z3-proved envelope | URL |
|---|---|---|---|
| Amazon Bedrock AgentCore Policy | Cedar policies checked at the AgentCore Gateway on every tool call. Log-only or enforce mode. Policies can be written in natural language and converted to Cedar. Preview announced Dec 2025; GA status not confirmed. | Closest relative. Cedar rules are written ahead of time per deployment, enforced at a gateway. Cedar is decidable and has SMT analysis of the policies themselves. Our envelope is generated per task and each call is proved against it locally. | https://aws.amazon.com/about-aws/whats-new/2025/12/amazon-bedrock-agentcore-policy-evaluations-preview ; https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy.md |
| Cedar analysis (AWS) | Open-source SMT-based analysis of Cedar policies, with Lean models and proofs of the analysis (SymCert, FMCAD 2026). | Proves things about policies, not about a live agent action versus an LLM-written envelope. Shows SMT for authorization is mainstream at AWS. | https://aws.amazon.com/blogs/opensource/introducing-cedar-analysis-open-source-tools-for-verifying-authorization-policies/ ; https://www.amazon.science/publications/symcert-verifying-smt-based-policy-analyses |
| OPA / Rego | General policy engine. Agent use is by third parties (Strata MCP provider, community mcp-opa-authz). I found no official OPA feature for MCP tool calls. | Hand-written Rego, evaluated not proved. | https://docs.strata.io/docs/opa-authorization ; https://openpolicyagent.org/ecosystem/entry/opa-mcp (authoring helper only) |
| Docker MCP Gateway | Open-source gateway for MCP servers. Flags for interceptors, secret blocking, call logging, signature verification, container limits. | Isolation and filtering of MCP servers. No argument-level proof. | https://www.docker.com/blog/docker-mcp-gateway-secure-infrastructure-for-agentic-ai/ ; https://docs.docker.com/reference/cli/docker/mcp/gateway/run.md |

### Guardrail and agent-security companies

| Name | What it does | How it differs from a Z3-proved envelope | URL |
|---|---|---|---|
| Invariant Labs | ETH Zurich spin-off, agent security tooling. Acquired by Snyk, announced 24 Jun 2025, price undisclosed. | Rule/trace analysis of agent behavior. Not a solver proof against a generated envelope. | https://tech.eu/2025/06/24/snyk-acquires-ai-security-pioneer-invariant-labs-for-a-new-era-in-ai-safeguards/ |
| Lakera | Lakera Guard (runtime) and Red (pre-deploy). Check Point announced 16 Sep 2025, closed 22 Oct 2025, price undisclosed. | ML detection of injection and leaks. Probabilistic. | https://cyberscoop.com/check-point-lakera-acquistion-ai-security/ ; https://cxotoday.com/press-release/check-point-software-technologies-completes-acquisition-of-lakera-to-secure-the-worlds-ai-transformation/ |
| Prompt Security | Employee AI-use control, injection and leak blocking, MCP gateway. SentinelOne agreed to acquire it Aug 2025; price reported about $250M by press, not confirmed. | Detection and policy at the edge. Probabilistic. | https://www.darkreading.com/endpoint-security/sentinelone-acquires-ai-startup-prompt-security |
| Protect AI | AI/model security. Palo Alto Networks acquisition (announced Apr 2025) seen only in a directory entry (unverified). | Model supply chain focus. | no primary source found |
| Zenity | Agent security platform. $38M Series B, 29 Oct 2024, over $55M total. | Observe and enforce on agent behavior. No formal proof. | https://zenity.io/company-overview/newsroom/company-news/zenity-raises-38m-series-b-funding-round-to-secure-agentic-ai |
| Noma Security | AI agent security: discovery, posture, attack simulation, defense. $100M Series B mid-2025, $132M total (Calcalist). Valuation rumor $400M unconfirmed. | Posture and runtime defense. No proof. | https://en.globes.co.il/en/article-1001517758 |
| NVIDIA NeMo Guardrails | Open-source Python library. Input, retrieval, dialog, execution, output rails. Colang flows. | Dialog and content rails. Execution rails call checks you write. | https://github.com/NVIDIA-NeMo/Guardrails |
| Meta LlamaFirewall | Open-source. PromptGuard 2 (injection classifier), AlignmentCheck (LLM audit of reasoning, experimental), CodeShield (static analysis of generated code). Authors report AgentDojo attack success cut from 17.6% to 1.75% with combined layers. | Model-based and static checks. Can miss. No proof. | https://arxiv.org/abs/2505.03574 |

### Formal methods near AI-made code or actions

| Name | What it does | How it differs from a Z3-proved envelope | URL |
|---|---|---|---|
| AWS Bedrock Automated Reasoning checks | Part of Bedrock Guardrails. Checks a model answer against a formal policy built from a document. AWS-linked sources claim up to 99% accuracy only for unambiguous translations. | Checks text claims for logical consistency with a policy. Not tool calls. Also depends on NL-to-logic translation. | https://aws.amazon.com/blogs/machine-learning/automated-reasoning-policy-refinement-in-amazon-bedrock |
| Imandra CodeLogician | LLM agents turn source code into formal models (IML); ImandraX reasons about them. | Verifies software logic before shipping. Not a runtime gate on agent actions. | https://arxiv.org/abs/2601.11840 |
| Atlas Computing / ARIA Safeguarded AI | Nonprofit and UK programme (GBP 59M) for specs checked by non-learned tools, with evidence that a solution meets the spec. Atlas 2024 status only; no newer data found. | Same philosophy (spec plus non-learned checker). Targets high-stakes domains, mostly research, no shipping agent gate found. | https://www.aria.org.uk/media/s1sght12/programmethesis-safeguarded-ai-accessible.pdf ; https://www.greaterwrong.com/posts/dKD47KMqvYAB7Zkmv/announcing-atlas-computing |
| Galois | Consultancy. AI plus formal methods (AI-driven verification, memory-safe parser generation). I found no Galois work on agent-action proofs. | Verifies code, not live actions. | https://www.galois.com/ai-fm |
| Heriot-Watt "certificates of validity" paper | Proposes proof-carrying style: consumer fixes a policy, agent supplies action plus certificate, verifier checks. | Same shape as ours; paper-stage. Here the agent supplies the proof; ours checks against an LLM-written envelope. | https://arxiv.org/pdf/2606.23768 |
| Harmonic (Aristotle) | Math model whose output is checked in Lean 4. $120M Series C at $1.45B post-money (Jan 2026). | Proves math, not agent actions. Shows Lean checking is a funded trend. | https://harmonic.fun/news/series-c/ |
| Axiom Math (AxiomProver) | Writes Lean proofs; claims 12/12 on Putnam 2025. $200M raise, about $1.6B valuation per secondary reports. | Same: math proving. | https://www.tamradar.com/funding-rounds/axiom-series-a-200m |
| Bend / HVM2 (Higher Order Company, Victor Taelin) | A high-level parallel functional language on the HVM2 interaction-combinator runtime. It runs on GPUs with no thread code. | Is: a language and runtime for parallel speed. Is not: a verifier, a safety layer or a proof tool. A "Bend 2 with dependent types" claim came from one aggregator and is unconfirmed. | https://github.com/HigherOrderCo/Bend |


### Where our gate differs (a judgment from this search, not a proof)

1. An LLM writes the envelope for each task. A solver checks it. Others use rules a human writes
   once (Cedar, OPA, Gemini TOML) or ML scoring (Lakera, Meta).
2. Z3 proves each tool call against that envelope before it runs. We found no shipping product
   that does this. Bedrock Automated Reasoning proves claims in text, not tool calls.
3. Fail closed is the default. The Claude Code sandbox fails open unless configured, and
   Codex hooks fail open.
4. It runs local and works with any harness. AgentCore Policy needs the AWS gateway.
5. Caveat: AgentCore Policy with Cedar is the nearest relative. Its natural-language-to-Cedar
   step is close to "an LLM writes the policy". The gap is per-task generation and the local
   proof. A search can miss private or new work.

## Agent multiplexers, worker trees and views

### Worker trees and swarms

Shape: tree / flat / graph. Isolation as reported by the source.

| Name | One line | Shape, isolation | URL |
|---|---|---|---|
| Claude Code subagents | Workers in one session, each with own context, tools, permissions. Report back to the parent. | Tree (parent to children). Own context window; no worktree stated. | https://code.claude.com/docs/en/subagents |
| Claude Code agent teams | Separate sessions that message each other through a shared mailbox and claim tasks from a shared list (third-party description). | Flat/graph. Separate sessions. Cost 3-7x (sources disagree). | https://code.claude.com/docs/en/subagents (snippet), https://www.morphllm.com/agent-teams-vs-subagents |
| Claude Code --worktree | Flag gives each agent its own worktree and branch. | Isolation by worktree. | https://www.13labs.au/guides/run-parallel-ai-coding-agents-without-conflicts (third party) |
| Codex subagents | Spawned only on request. Defaults: agents.max_threads=6, agents.max_depth=1. Custom agents in TOML. Experimental spawn_agents_on_csv. | Shallow tree (depth 1). No isolation stated. | https://developers.openai.com/codex/subagents |
| OpenAI Agents SDK handoffs | A handoff is a tool call that passes control and context to another agent. | Graph of peers; control moves, no parent waits. | https://pypi.org/p/openai-agents (snippet; official docs not fetched) |
| Gas Town (Yegge) | Multi-agent orchestrator, many specialized agents on one codebase, persistent "towns". Backend moved to Dolt. Hosted version by Kilo. | Hierarchy of roles. Isolation not confirmed. | https://github.com/steveyegge/gastown ; https://thenewstack.io/steve-yegges-ai-agent-orchestration-project-gas-town-comes-to-the-cloud-and-brings-the-wasteland-with-it/ |
| claude-flow (now Ruflo) | Swarm layer for Claude Code. Mesh, hierarchical, ring, star topologies. Claims 60-100+ agents. Perf claims self-reported. | Selectable topology. Isolation not stated. | https://github.com/ruvnet/ruflo (snippet) ; https://rywalker.com/research/claude-flow |
| tenex | Rust TUI. One worktree and branch per agent. Planning/review swarms; merges child output into parent. Linux only. | Tree. Git worktrees. | https://github.com/Mockapapella/tenex ; https://docs.rs/crate/tenex/1.0.7 |
| graith | Go terminal multiplexer. Daemon holds PTYs; client over Unix socket. Worktree per agent. Optional macOS kernel sandbox. Young (2 stars). | Flat. Worktrees plus optional sandbox. | https://github.com/d0ugal/graith ; https://pkg.go.dev/github.com/d0ugal/graith@v0.56.1 |
| initech | Go TUI. Many Claude agents in terminal panes with message passing between them. | Flat with messaging. Isolation not confirmed. | https://github.com/nmelo/initech ; https://pkg.go.dev/github.com/nmelo/initech@v1.4.2 |
| Claude Squad | Terminal app managing many agents, each in an isolated git workspace. | Flat. Worktrees. | https://vibecodinghub.org/tools/claude-squad (third party) |
| Conductor | Mac app. Each task in its own worktree workspace; Claude Code or Codex. | Flat. Worktrees. | https://www.conductor.build |
| Herdr | Rust agent multiplexer: panes, tabs, workspaces, agent-aware status ("who waits on you"). Licence sources disagree (AGPL vs Apache). | Flat supervisor view. No isolation by itself. | https://github.com/ogulcancelik/herdr ; https://www.bitdoze.com/herdr-agent-multiplexer/ |
| LangGraph supervisor / swarm | Supervisor package nests supervisors. Swarm package does decentralized handoffs. | Tree (supervisor) or graph (swarm). In-process. | https://changelog.langchain.com/announcements/ann_B9y72n1Ackden (snippet) |
| CrewAI | Crews run sequential or hierarchical (manager-led). | Tree or chain. In-process. | https://www.zenml.io/blog/langgraph-vs-crewai (third party) |
| AutoGen / AG2 | Group chat: a manager LLM picks the next speaker. Also a pull-from-queue swarm. AG2 as a fork: unverified. | Graph/room. In-process. | https://www.futureagi.com/blog/best-langgraph-alternatives-2026/ (third party) |
| Vibe Kanban | Kanban board over parallel coding agents. | Flat board. | https://scour.ing/p/https://github.com/BloopAI/vibe-kanban (snippet) |

Note: worktrees isolate files only. Ports, databases, browser profiles stay shared (https://developertoolkit.ai/en/ecosystem/agent-tools/parallel-agents/).

Compared with a delegation tree (long-lived foreman chat spawns gated workers):
- Most tools are flat (many panes, a human at the top). Few have a persistent foreman chat.
- Codex caps depth at 1. Claude subagents are one level. A real tree with gates per spawn is rare.
- Gas Town is closest in spirit (persistent roles) but I did not confirm any per-spawn gate.
- No source here mentions a fail-closed check on each spawn. That looks like open ground.

### Views on top of agent systems

| Name | One line | URL |
|---|---|---|
| Minecraft Agent IDE | Paper plugin plus desktop bridge. Right-click a villager, ask in plain English; a real coding agent works. Progress above the villager's head, Allow/Reject buttons in chat, several helper villagers at once, sessions resume. Closest hit to the villager idea. The villager does not walk up to you; you go to it. | https://github.com/terminalsin/minecraft-agent-ide ; https://www.everydev.ai/tools/minecraft-agent-ide |
| AgentCraft | Fabric mod: a walkable Minecraft studio for a team of coding agents. A lead (Opus) plans and reviews, up to three workers code, each task in its own git worktree, diffs reviewed in game, nothing pushed. Claude Agent SDK or Codex per agent. Its own scripts make every skin and texture. MIT. | https://github.com/blendi-remade/agentcraft |
| ClaudeCraft MCP | Lets Claude control a Minecraft world over MCP. Not a session view. | https://www.modpackindex.com/mod/212233/claudecraft-mcp |
| Villager mods (Smart Villagers AI, Speaking Villagers, VillagerGPT) | Chatty AI villagers, player starts the talk. | https://www.curseforge.com/minecraft/mc-mods/smart-villagers-ai |
| Human-AI Minecraft house study | Agent barred from placing blocks, so it must ask the human. Gate by design. | https://arxiv.org/pdf/2503.19607 (title uncertain, snippet) |
| Voyager | GPT-4 Minecraft agent: auto curriculum, skill library of code, iterative prompting. | https://voyager.minedojo.org/ ; https://arxiv.org/html/2305.16291v2 |
| Mindcraft | LLMs control Mineflayer bots from profile JSON. Code execution off by default. Tasks module for benchmarks. | https://github.com/mindcraft-bots/mindcraft (snippet) |
| Project Sid | Many agents in Minecraft build a society. | https://www.tomsguide.com/ai/a-company-gave-1000-ai-agents-access-to-minecraft-and-they-built-a-society |
| a16z AI Town | MIT starter kit for a town of AI characters. Convex, Ollama default. | https://github.com/a16z-infra/ai-town |
| Generative Agents (Smallville) | 25 agents; memory stream, reflection, planning. | https://arxiv.org/abs/2304.03442 |
| Pixel Agents | VS Code extension: one pixel character per Claude Code terminal; also `npx pixel-agents` in a browser. | https://github.com/pablodelucca/pixel-agents |
| pixel-agents-codex | Fork for Codex. | https://github.com/MichaelMa907/pixel-agents-codex |
| PixelHQ | iOS app plus laptop bridge; sessions show as live office activity. | https://apps.apple.com/ph/app/pixelhq/id6758467298 |
| Claude Code Agent View | Built-in dashboard of sessions by status, answer agents that need input. Only a blog source. | https://www.timesofai.com/industry-insights/agent-view-in-claude-code/ |
| Omnara | Web dashboard (Next.js) plus React Native phone app, human-in-the-loop for agents. | https://docsearch.algolia.com/mcp/docs/repo/omnara-ai/omnara (snippet) |
| claude-code-kanban | Browser kanban for Claude Code teams. | https://nikiforovall.blog/ai/productivity/2026/02/07/claude-code-kanban.html |
| Vibe Kanban | See above. | https://jimmysong.io/en/ai/vibe-kanban |

Not found: an RTS or Factorio-style agent dashboard; the "villagers who come to you" tweet. Happy (phone client) was not covered.


Our delegation tree: a long-lived foreman chat spawns gated workers. Most tools above are flat,
with a human at the top. Codex caps depth at 1. Claude subagents are one level. Gas Town is
closest in spirit. We found no source that checks each spawn and fails closed.

## Learning from repeated work, and memory

### Learning from repeated work

| Name | What improves | How it learns | Model calls to code? | URL |
|---|---|---|---|---|
| Hermes Agent skills | Skill text (SKILL.md) | Agent writes and refines skills from experience; docs call it a built-in learning loop. Exact triggers (5+ tool calls vs every 15 tasks) disagree across secondary sources. Not verified. | No. Text the model reads. | https://hermes-agent.nousresearch.com/docs |
| hermes-agent-self-evolution (reported) | Skill files | DSPy + GEPA over execution traces (secondary source only, cost figures unverified) | No | https://ssojet.com/blog/hermes-agent-self-evolving-skills |
| Anthropic Agent Skills | Skill text and bundled scripts | Hand-authored; progressive disclosure (metadata, then body, then files). No Anthropic eval or A/B source found in this pass. | Only if a skill bundles scripts | https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills |
| Codex skills | Skill text, optional scripts | Hand-authored folders; same three-stage loading. Docs say prefer instructions unless you need deterministic behavior. | Optional, via scripts/ | https://learn.chatgpt.com/docs/build-skills |
| Voyager skill library | Library of executable code | Iterative prompting with environment feedback, execution errors, self-verification; keeps working programs | Yes. Skills are code. | https://arxiv.org/abs/2305.16291 |
| Agent Workflow Memory | Workflows given to the agent as guidance | Extracts recurring routines from past runs, offline or online. +24.6% Mind2Web, +51.1% WebArena relative. | No. Workflows guide the model. | https://arxiv.org/abs/2409.07429 |
| GEPA (DSPy optimizer) | Prompts | Reads trajectories, reflects in language, mutates prompts, Pareto frontier. Beats GRPO by 6% avg (v2) with up to 35x fewer rollouts. | No | https://arxiv.org/abs/2507.19457 |
| ACE (agentic context engineering) | Context playbook | Generate, reflect, curate with incremental updates; works without labels. +10.6% agent tasks. | No | https://arxiv.org/abs/2510.04618 |
| OpenPipe ART | Weights (LoRA) | GRPO on agent rollouts with a reward; RULER makes rewards automatically. Apache-2.0. | Moves behavior into weights, not code | https://github.com/OpenPipe/ART |
| OpenAI RFT | Weights | Programmable grader scores samples; o4-mini only. Platform winding down; new users blocked, jobs until 2027-01-06. | Weights, not code | https://developers.openai.com/api/docs/guides/reinforcement-fine-tuning |
| Distillation (Hinton et al.) | Weights of a small model | Small model trained on a large model's outputs | Cheaper model call, still a call | https://arxiv.org/abs/1503.02531 |
| Do-nothing scripting (Slimmon) | A script, one step at a time | Human writes a script that walks through the manual steps; swap each step for code later | Yes, by design | https://blog.danslimmon.com/2019/07/15/do-nothing-scripting-the-key-to-gradual-automation/ |

### Memory systems

Kinds: E = episodic (what was said or done), S = semantic (facts), P = procedural (how to do things).

| Name | Kinds | What it stores | Retrieval | Licence | URL |
|---|---|---|---|---|---|
| Plain RAG | S, E if you index logs | Chunk embeddings | Vector cosine similarity | n/a | https://arxiv.org/abs/2404.16130 (GraphRAG paper compares against it) |
| GraphRAG | S | LLM-built entity graph, community summaries | Per-community partial answers, merged; for corpus-wide questions | n/a | https://arxiv.org/abs/2404.16130 |
| Mem0 | S (user, session, agent state) | Extracted memories, entity links | Vector + BM25 + entity match, fused | Apache-2.0 | https://github.com/mem0ai/mem0 |
| Letta / MemGPT | E, S (tiered) | Memory blocks in context, plus out-of-context storage; archival has a search API | Model pages memory in and out via tools. Recall-memory detail not verified. | not checked | https://arxiv.org/abs/2310.08560 |
| Zep / Graphiti | E, S | Temporal graph: episodes, entities, facts with validity windows | Embeddings + BM25 + graph traversal | Apache-2.0 | https://github.com/getzep/graphiti |
| Cognee | S, E (session lessons distilled) | Graph + vectors + metadata | Graph/vector/hybrid recall | Apache-2.0 | https://github.com/topoteretes/cognee |
| LangMem | S, E, P | Collections, profiles, episodes; procedural = prompt rules | Store search (semantic, metadata); prompt optimizer rewrites rules | not checked | https://langchain-ai.github.io/langmem/concepts/conceptual_guide/ |
| OptMem (Taelin) | E | Notes up to 280 bytes in an append-only log; 426-token prompt; single Python file `memo` | wake, recall (search), zoom (binary summary tree) | not stated on repo page | https://github.com/VictorTaelin/OptMem |
| OptChat (Taelin spec) | E | Endless chat: verbatim log + binary summary tree | Fixed-budget view, oldest first; zoom into any line to verbatim | Hermes plugin is MIT | https://hermes-agent.nousresearch.com/docs/plugins/optchat |
| Compaction tree | E | Same idea as OptChat tree | Coarse far past, fine recent, zoom | n/a | same as OptChat |
| UniiChat | unknown | Not found in any source | Not found. Likely a spelling variant of OptChat. | unknown | none |

Notes: pi-optchat (https://pi.dev/packages/pi-optchat) is another port of the OptChat recipe. Hermes docs warn that its logs are unencrypted with no retention limit, and the compactor sends text to an auxiliary model.


### Five kinds of saving

1. Routers save tokens at serving time: they send each call to a cheaper model or reuse a cached answer. The task is unchanged.
2. Distillation (and RFT, ART) moves skill into small-model weights: the call stays, but it gets cheaper and faster.
3. Skill and script learning (Voyager, do-nothing scripting) removes the call: a repeated task becomes deterministic code. This saves the most.
4. Prompt and context learning (GEPA, ACE, AWM, skill text) makes calls better, not fewer, and can grow the prompt.
5. Memory (OptChat, Mem0, Graphiti) saves tokens by bounding context and fetching only what is needed. It adds a store to maintain and does not make the model cheaper.

UniiChat appeared in our notes but we found no page for it. It may be a spelling of OptChat.

## What we borrow

- From the Minecraft Agent IDE: a worker that needs approval gets one Allow or Reject action
  where the human is looking.
- From Herdr: status over each worker, so you see who waits on you.
- From graith and Herdr: one daemon owns the sessions and thin clients attach. That fits a
  phone pane and a game skin on the same state.
- From Codex and Claude Code: worktrees isolate files. Ports, databases and browser profiles
  need their own isolation, and we say so.
- From Victor Taelin's OptMem and OptChat: the verbatim log plus a binary summary tree you can
  zoom into. The foreman's memory follows it.
- From Voyager and do-nothing scripting: repeated work becomes plain code, which removes the
  model call.
- From Cedar and AgentCore Policy: SMT-backed authorization is mainstream.

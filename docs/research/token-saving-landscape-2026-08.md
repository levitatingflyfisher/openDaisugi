# Token-Saving LLM Routing, Caching, Serving & Harness Landscape (August 2026)

> Provenance: external research survey dropped into the repo 2026-08-20, reproduced
> near-verbatim. Claims and numbers are the survey's own (several are flagged
> self-reported inside it); openDaisugi's reading of this landscape is in
> [token-saving-landscape-mapping.md](token-saving-landscape-mapping.md).


## TL;DR
- **The single biggest lever is caching, not routing.** Provider prompt caching cuts input-token cost by ~90% (reads at 0.1x on Anthropic; cached-input discounts of 50–90% on OpenAI/Gemini/Bedrock) and pays for itself after 1–2 reuses — a few lines of code that beats most routers on ROI. Routing (Ramp Router, RouteLLM, Bedrock IPR) is the second lever, delivering a real ~25–40% production cut; the vendor benchmark peaks of 85–98% are distribution-dependent and rarely reproduced on your traffic.
- **Order of operations:** (1) prompt caching + output caps, (2) Batch API 50% off for async work, (3) right-size the default model / add a router, (4) semantic caching at the gateway (~10–15% extra), (5) prompt/context compression (LLMLingua up to 20x) and agent context engineering (Anthropic context editing cut token consumption 84% on a 100-turn eval). A team moving from "everything on the frontier model, no caching" to a tuned stack realistically hits 70–85% cost reduction in a quarter.
- **Which lever binds first depends on scale.** Below ~$5K/mo of API spend, just cache + cap + pick a cheaper default. At mid scale, add a gateway/router and a semantic cache. At self-hosted GPU scale the binding constraint becomes GPU utilization: KV-cache reuse (LMCache, NVIDIA Dynamo), prefix caching (vLLM/SGLang), speculative decoding (EAGLE-3, 2–4x), and quantization (FP8/NVFP4) matter more than API-tier tricks.

## Key Findings

- **Ramp Router** (opened publicly July 2026; run internally 3 years across 100+ use cases and ~2.75 trillion tokens/month at 99.99% uptime) cut Ramp's own LLM costs by 30%+ "with no performance losses." A distinct Ramp engineering technique — dynamic failure-aware routing across models and service tiers using Thompson sampling + EWMA latency posteriors — achieved over 25% cost savings while *reducing* error rate by 0.09 percentage points.
- **RouteLLM** (LMSYS/Berkeley, ICLR 2025) is the reference open-source router: cost reductions of over 85% on MT-Bench, 45% on MMLU, and 35% on GSM8K vs GPT-4-only, while still achieving 95% of GPT-4's performance. Its matrix-factorization router (with LLM-judge data augmentation) sent only 14% of queries to the strong model for the same quality (GPT-4-1106 vs Mixtral-8x7B pairing).
- **Vendor peak-savings claims run 30–98%** (FrugalGPT: matches GPT-4 with "up to 98% cost reduction"; Martian claims up to 98%; IBM up to 85%). But 2026 fair-evaluation work (LLMRouterBench, "Towards Fair and Comprehensive Evaluation of Routers") finds most routers collapse to similar performance under unified evaluation and several fail to beat simple baselines. Treat peaks as proof-of-concept, not your number.
- **Anthropic prompt caching:** reads at 0.1x base input, 5-min writes at 1.25x, 1-hour writes at 2x; min 1,024 tokens (Haiku) / 2,048 (Sonnet/Opus). Break-even is ~2 reads in the window; "1 write + 99 reads = 11.15x cost for 100 requests" — an 88.85% saving. **OpenAI** caching is automatic, 50–90% off cached input (25–75% read multiplier by model), min 1024 tokens; GPT-5.6+ charges 1.25x cache writes. **Gemini/Vertex** context caching ~90% off cached reads plus hourly storage (~$1/1M/hr Flash, ~$4.50/1M/hr Pro). **Bedrock** prompt caching up to 90% cost / 85% latency reduction.
- **Bedrock Intelligent Prompt Routing** claims up to 30% (some AWS benchmarks 30–56%) but only routes *within a model family* (Nova Lite↔Nova Pro, Claude Haiku↔Sonnet), pairwise, same region.
- **NVIDIA Nemotron Nano 2** (9B hybrid Mamba-Transformer) delivers up to 6.3x higher inference throughput than Qwen3-8B in reasoning settings (8k in/16k out) at on-par accuracy, runs 128k context on a single A10G, [ADS](https://ui.adsabs.harvard.edu/abs/2025arXiv250814444N/abstract) and its runtime thinking-budget cutoff can lower inference costs by up to 60% without significantly impacting accuracy. **Nemotron 3 Nano** (30B-A3B MoE, Dec 2025) hits up to 3.3x throughput vs GPT-OSS-20B/Qwen3-30B, activates less than half the params per forward pass, supports 1M context and granular reasoning-budget control.
- **LMCache** (KV offload for vLLM) cut TTFT from 11s→1.5s on 128K context, allowed 15x more decode requests/sec, and gave a 69% reduction in prefill cost at an 80% cache-hit rate (3–10x latency reduction vs recompute). **NVIDIA Dynamo** KV-aware routing gave ~3x TTFT improvement and 2x latency reduction on 100K real queries; Baseten reported 2x faster inference with it.
- **LLMLingua** (Microsoft) compresses prompts up to 20x with minimal loss; LLMLingua-2 is 3–6x faster and task-agnostic (latency down up to 2.9x at 2–5x compression); LongLLMLingua adds ~17% performance at 4x compression.
- **Semantic caching** (Redis LangCache, GPTCache): 30–73% cost reduction in high-repetition workloads; one LangCache customer reports a 70% hit rate = 70% LLM spend saved. Failure mode: stale/wrong hits from embedding drift.
- **Speculative decoding** (EAGLE-3): 2–6x faster, lossless (larger models 4–6x). **Batch APIs** (OpenAI/Anthropic/Bedrock): flat 50% off. Stacked with caching, OpenAI cached+batch reaches ~75% off.
- **Anthropic context editing + memory tool:** 84% token reduction on a 100-turn web-search eval; +39% agent performance combined, +29% context editing alone.

## Details

### 1. Where the tokens and dollars actually go
Every LLM bill decomposes as: `input_tokens × input_price + cached_input × cached_price + cache_write × write_price + output_tokens × output_price + tool/storage fees + retry overhead`. Two facts dominate:
1. **Output tokens cost 4–8x input tokens.** Capping output, using structured/JSON output, and shortening reasoning traces attack the most expensive line.
2. **The price spread between cheapest and most-capable usable models is roughly 100x** (e.g., ~$0.10/M for Gemini Flash-Lite input vs $30/M for a frontier "pro" tier). That is why routing works at all — most production traffic never needed a frontier model.

The levers, ranked by mechanism:
- **Cheaper per-token model** (routing, right-sizing the default): pay less per token.
- **Fewer tokens** (compression, context editing, output caps): send/generate less.
- **Cached tokens at ~10% price** (prompt caching): don't reprocess repeated context.
- **Skip the call entirely** (semantic caching): return a prior answer.
- **Higher GPU utilization** (batching, prefix/KV reuse, speculative decoding, quantization): only relevant if you self-host.
- **Discounted tiers** (Batch API 50%, flex/priority tiers): trade latency for price.

### 2. Model routers / cascade routers

**Mechanism:** send each query to the cheapest model that clears a quality bar. Three families — pre-request classifier (cheapest, decides before any model sees the query), at-inference cascade (cheap model first, escalate on low confidence — most accurate), post-response retry (safety net).

| Tool | Type | Mechanism | Claimed savings | Openness / hosting | Notes |
|---|---|---|---|---|---|
| **RouteLLM** (LMSYS/Berkeley) | OSS framework | Preference-trained router (matrix factorization, BERT, SW-ranking, causal LLM) | 85%+ MT-Bench @95% GPT-4; 45% MMLU; 35% GSM8K | Open source, self-host | ICLR 2025; drop-in OpenAI client; peaks are MT-Bench/Mixtral-specific |
| **Ramp Router** | SaaS gateway | Per-request routing to cheapest approved model clearing quality bar; caching, compaction, semantic attribution | 30%+ (Ramp's own bill) | SaaS, one OpenAI-compatible endpoint | Ramp sees your prompts; battle-tested at 2.75T tokens/mo |
| **Martian** | Commercial | "Model mapping" — predicts model behavior from internals before inference | up to 98% (marketing) | SaaS; Accenture-integrated | Reportedly near $1.3B valuation; enterprise compliance (Airlock) features |
| **Not Diamond** | SaaS/OSS | Learns optimal routing from your eval data over any model set | — | SaaS + open components | notdiamond-0001 GPT-3.5↔GPT-4 router |
| **Katanemo Arch-Router** | OSS 1.5B model | Preference-aligned routing via natural-language domain-action taxonomy; add models w/o retraining | 93.17% routing accuracy (beats GPT-4/Claude by ~7pts) | Open (research license), self-host | Powers Arch Gateway; latency driven by short output name |
| **Bedrock Intelligent Prompt Routing** | Cloud managed | Predicts per-model quality, routes within family | up to 30% (AWS; some 30–56%) | AWS only; within-family, pairwise | Zero code — swap model ID for router ARN |
| **Azure AI Foundry Model Router** | Cloud managed | Trained model on single endpoint; Balanced/Cost/Quality modes | 4.5–14.2% in one test run | Azure only | Quality mode saved most (14.2%) by routing simple prompts to cheap models |
| **OpenRouter auto-router / Requesty / Unify / Pulze / Neutrino / Nexus / Eden AI / Kluster** | SaaS aggregators | Single endpoint, cost/latency/quality routing, sticky caching | varies | SaaS | OpenRouter adds sticky routing (session_id) to keep warm caches |
| **GPT-5 / GPT-5.1 built-in router** | Proprietary | Real-time router picks Instant vs Thinking; adaptive reasoning depth | (better answers per token) | ChatGPT/API | Continuously trained on user switches, preference rates, correctness |

**Academic/OSS routing work:** FrugalGPT (Stanford — matches GPT-4 with up to 98% cost reduction, or +4% accuracy at equal cost, via cascades), Hybrid LLM, AutoMix, RouterDC, GraphRouter, Universal Model Routing, MixLLM (97.25% GPT-4 quality at 24.18% cost), R2-Reasoner (84.46% API cost savings), MasRouter, Avengers, EmbedLLM, RoRF, P2L (Prompt-to-Leaderboard). RouterBench and the 2026 LLMRouterBench / fair-evaluation papers are the benchmarks — and they warn routers are highly benchmark-dependent, with realistic savings ~a third off, not 85%.

**Startup consolidation:** Martian pivoting toward enterprise compliance with a high reported valuation; NotDiamond, Unify, Neutrino, Pulze, Nexus competing; Aurelio Labs ships open-source Semantic Router. Accenture invested in Martian (2024) and integrated it into its "switchboard" services.

### 3. Small/efficient models as the cheap tier

- **NVIDIA Nemotron:** Nano 2 (9B hybrid Mamba-Transformer) — up to 6.3x throughput vs Qwen3-8B, 128k context on a single A10G, runtime thinking-budget cutoff (up to 60% cheaper inference at minimal accuracy loss). Nemotron 3 family (Dec 2025) — Nano 30B-A3B MoE up to 3.3x throughput vs GPT-OSS-20B/Qwen3-30B, <half params/forward, 1M context, granular reasoning-budget control; Super/Ultra add LatentMoE + NVFP4 training + MTP layers. Fully open (weights, recipes, most data). Architecture mechanism: replacing most attention layers with Mamba-2 speeds the long "thinking" traces reasoning models generate.
- **Token-saving features across models:** Qwen3 hybrid thinking on/off + thinking budget; Claude reasoning with max_tokens caps; GPT-5.1 adaptive reasoning; Gemini Flash/Flash-Lite as cheap tier ($0.10/$0.40 for Flash-Lite); Phi-4-mini (~$0.07/$0.23, 35–40x cheaper than GPT-4o); Ministral, Gemma, Llama variants, GPT-5-mini/nano, Claude Haiku. The mechanism that saves tokens is **reasoning-effort control / thinking budgets / hybrid on-off toggles** — shorter reasoning traces mean fewer expensive output tokens.

### 4. Caching layers (the biggest real-world saver)

**Provider prompt caching / KV reuse (mechanism: don't reprocess a repeated prefix):**
- Anthropic: read 0.1x, write 1.25x (5-min) / 2x (1-hr); min 1024 (Haiku)/2048 (Sonnet/Opus); [PE Collective](https://pecollective.com/tools/claude-pricing-guide/) break-even ~2 reads; up to ~89% on cached portion. Failure modes: cache misses from whitespace/tool-order changes, TTL expiry, unstable prefix, provider drift.
- OpenAI: automatic, 50–90% off cached input, min 1024 tokens, ~5-min window; GPT-5.6+ charges 1.25x cache writes. [OpenRouter](https://openrouter.ai/docs/guides/best-practices/prompt-caching)
- Gemini/Vertex: implicit + explicit context caching ~90% off cached reads, plus storage.
- DeepSeek: context caching on disk (0.1x reads). Bedrock/Vertex: up to 90% cost / 85% latency (Adobe saw 72% response-time reduction in Bedrock testing).

**Semantic caching (mechanism: skip the call for semantically-similar queries):**
- Redis LangCache: managed, embeddings auto-generated; up to 73% cost reduction; one customer 70% hit rate. GPTCache: OSS, 2–10x speedup on hits, 30–70% cost cut. Portkey semantic cache: ~99% accuracy at ~20% hit rate (RAG 18–60%). Helicone cache, Canonical AI, MeanCache (on-device; ~31% of a user's queries similar to a prior one), vCache. **Failure mode: stale/wrong hits from embedding drift — monitor the cosine-similarity distribution of hits over time; multi-turn context breaks naive semantic caching.**

**KV-cache offload/sharing infra (mechanism: reuse computed KV state across requests/nodes → less prefill compute):**
- LMCache (multi-tier GPU→CPU→disk→Redis/Valkey/Mooncake/InfiniStore/S3): 69% prefill-cost cut at 80% hit; 3–10x latency reduction; engine-independent daemon survives engine crashes. NVIDIA Dynamo KV cache manager/KVBM + Smart Router (radix tree over a GPU fleet, cost = overlap_weight × prefill_blocks + decode_blocks): 3x TTFT, 2x latency on 100K queries; backends vLLM/SGLang/TensorRT-LLM. Mooncake; vLLM prefix caching (PagedAttention cuts KV waste <4%, 2–4x throughput) + KV connector; SGLang RadixAttention/HiCache; AIBrix; llm-d. KV-compression research: H2O, StreamingLLM, SnapKV, PyramidKV, KIVI (quantized KV), DuoAttention, CacheBlend, Cache-Craft.

### 5. Serving harnesses / inference engines

**Mechanism: higher GPU utilization via batching, prefix sharing, speculative decoding, quantization.**
- **vLLM** (V1 engine, prefix caching, disaggregated prefill, PagedAttention), **SGLang** (RadixAttention, structured outputs), **NVIDIA TensorRT-LLM + Dynamo + NIM**, HF TGI, llama.cpp/Ollama/LM Studio, MLC-LLM, DeepSpeed-MII, Friendli, Fireworks FireAttention, Together, Baseten, Modal, RunPod.
- **Speed-vs-cost specialists:** Groq/Cerebras/SambaNova (fast but priced at a premium per token; buy them for latency, not cost).
- **Speculative decoding:** EAGLE-3 2–6x (lossless; 6–12pt higher acceptance than EAGLE-2; beats Medusa 15–25% tok/s at batch 1–4). Medusa, n-gram/lookahead. TensorRT-LLM on H200 >3x, 3.6x with FP8. Benefits shrink at large batch (compute-bound).
- **Quantization:** FP8, FP4/NVFP4, AWQ/GPTQ/INT4 — INT8 gives 1.8–2.4x; W4A16 nearly lossless; W4A4 degrades multi-step reasoning. A one-time gain, not repeatable. EAGLE stacks with quantization for extra speedup (except a conflict with W4A16).

### 6. Gateways / proxies with cost controls

| Gateway | Hosting | Cost-control features | Sees prompts? |
|---|---|---|---|
| **LiteLLM Proxy** | OSS, self-host (CPU-bound + Postgres) | Virtual keys, per-team budgets, fallbacks, load balancing, routing, caching | No (self-host) |
| **Portkey** | SaaS + self-host | Semantic cache, failover, guardrails, budgets, observability | Yes (SaaS) |
| **Cloudflare AI Gateway** | Managed edge | Edge caching, rate limits, analytics, logging | Yes |
| **Kong AI Gateway** | Self-host/enterprise | Semantic routing, token rate limits/budgets, prompt-injection scanning, PII | No (self-host) |
| **Helicone** | Apache-2.0 OSS, SaaS + self-host | Cost/latency/token logging, per-user chargeback, request caching (~20–40% cut) | Optional |
| **Braintrust Gateway** | SaaS | Routing + caching + evals/CI gate on routing changes | Yes |
| **Vercel AI Gateway** | Managed | Routing, observability | Yes |
| **Bifrost (Maxim AI), Arch Gateway (Katanemo), Apache APISIX AI, Envoy AI Gateway, TrueFoundry, AISIX** | mix | Multi-model + semantic routing, token budgets, guardrails, per-model cost | varies (AISIX/Envoy/self-host keep traffic in-VPC) |

Cloud-native: AWS Bedrock IPR, Azure AI Foundry Model Router, Vertex/Gemini "auto," IBM watsonx orchestration. Decision rule: LiteLLM for self-host/data control; Portkey/Cloudflare/Braintrust for managed; Kong if you already run Kong; AISIX/Envoy if traffic must stay in your VPC.

### 7. Context/prompt compression & token reduction

- **LLMLingua / LongLLMLingua / LLMLingua-2** (Microsoft): up to 20x compression, minimal loss; LLMLingua-2 task-agnostic, 3–6x faster, latency down up to 2.9x at 2–5x compression; LongLLMLingua +17.1% at 4x. 500xCompressor (extreme ratios).
- **Prompt optimization:** DSPy, GEPA. **Context pruning**, tool-result truncation (RTK reports 60–90% token savings on dev commands; Headroom 60–95% on real agent workloads). RAG-vs-long-context tradeoff: cache the static corpus once (Opus 1M context + 0.1x reads makes a 500K-token corpus a reasonable line item — ~$2.50 to write once, ~$0.25/request thereafter).
- **Agentic context engineering:** Anthropic context editing (auto-prune stale tool calls) — 84% token cut on a 100-turn eval; memory tool (offload to files, persists across sessions); sub-agent context isolation (subagents consume tens of thousands of tokens but return 1–2K-token distilled summaries); Claude Code compaction. Structured output constraints reduce output tokens.

### 8. Agent-harness-level savings

- **Coding/agent harnesses:** Claude Code (compaction, context editing, memory), Codex, Cursor (auto mode + model picker), Cline, Aider, OpenHands, Windsurf, Amp, GitHub Copilot model picker. Mechanisms: model-tiering (planner uses big model, executor uses small), context management, tool-output filtering. GPT-5 reported 22% fewer tokens and 45% fewer tool calls than its predecessor on SWE-Bench Verified.
- **API economics:** Batch APIs (OpenAI/Anthropic/Bedrock) 50% off for async (24-hr SLA); flex/priority tiers trade latency for price; provisioned throughput vs on-demand for steady high volume (1–3-year commitments save up to ~55–70% on cloud compute).

### 9. Enterprise-specific & FinOps

**Deployments with hard numbers:**
- **Ramp:** 30%+ cut (Router); >25% cut plus a 0.09-percentage-point error-rate *reduction* (Thompson-sampling failure-aware routing across models and service tiers).
- **Klarna:** OpenAI assistant handled 2.3M chats/month (two-thirds of chats = 700 FTEs), resolution 11min→<2min, estimated $40M profit improvement in 2024 (rising to ~$60M annual savings and the work of 853 agents by Q3 2025), cost-per-transaction $0.32→$0.19. **Caveat:** Klarna walked back AI-only claims in May 2025 and re-added humans for complex cases after a quality gap — a cautionary tale on silent quality regression from over-automation.

**FinOps-for-AI / observability tooling:**

| Tool | Type | Cost/token attribution | Openness | Sees prompts? |
|---|---|---|---|---|
| **Langfuse** | Observability | Per-usage-type cost (input/output/cached/reasoning), per-user, per-trace; pricing tiers; MIT | MIT OSS, self-host or cloud | Cloud yes / self-host no |
| **LangSmith** | Observability | Trace/project/dashboard cost; cache-read + reasoning token categories; per-session/user via tags. Plus $39/seat/mo, overage $2.50/1k traces | Proprietary; Enterprise self-host | Yes |
| **Datadog LLM Obs + CCM** | Observability + FinOps | Estimated per-span cost (800+ models) + real OpenAI invoice breakdown to model/token level | Proprietary SaaS | Yes |
| **W&B Weave** | Observability | Auto token+cost capture; custom per-token prices via add_cost (TS SDK lacks cost tracking) | SaaS-primary + self-host | Yes |
| **Arize Phoenix / AX** | Observability | Auto token cost rolled to trace/project; OpenInference/OTel; not retroactive | Phoenix OSS self-host; AX SaaS | Self-host no / AX yes |
| **Helicone** | Gateway + obs | Cost per request/model/custom property, per-user chargeback, cache-hit savings; free tier 10k req/mo | Apache-2.0 OSS + cloud | Optional |
| **Vantage** | FinOps | Token by model/workspace/API key/tier (OpenAI, Anthropic, Bedrock, Vertex, Cursor); unit cost; MCP server | SaaS | No (billing data only) |
| **CloudZero** | FinOps | Direct Anthropic + OpenAI usage/cost APIs incl. caching efficiency; cost per customer/feature/team (CostFormation) | SaaS | No |
| **Finout** | FinOps | "MegaBill" normalizes token+inference across clouds; VTags allocate 100% of spend | SaaS | No |

Distinction: observability tools compute **estimated** cost from token counts × pricing tables and see your prompts (unless self-hosted); FinOps platforms pull **actual invoice/billing-API** data and do not see prompt content. Genuinely open-source/self-hostable: Langfuse (MIT), Phoenix, Helicone (Apache-2.0).

## Recommendations

**Stage 1 — Do this week (any scale):**
1. Turn on **prompt caching** and structure prompts so static content (system prompt, tools, RAG corpus) comes first. Mechanism: ~90% off repeated input. Threshold to act: you send the same prefix ≥2x within the cache window.
2. **Cap output tokens** and use structured/JSON output. Output is 4–8x input; this is free.
3. Move all non-real-time work to the **Batch API** (50% off). Threshold: any workload tolerating a 24-hour SLA.

**Stage 2 — This month (>$5K/mo spend):**
4. Put a **gateway** in front (LiteLLM if you want self-host/data control; Portkey/Cloudflare/Bedrock if you want managed) for budgets, fallbacks, per-token attribution, and semantic caching (~10–15% extra). Add a **FinOps/observability view** (Langfuse/Helicone OSS if cost-sensitive; CloudZero/Vantage/Finout for actual-invoice attribution).
5. **Right-size the default model** behind an eval gate of 50–500 cases before merging. Then add a **router** (Ramp Router, Bedrock IPR within-family, Arch-Router OSS, or RouteLLM). Expect ~25–40% real savings, not the 85% headline. Benchmark that would change the plan: if your cheap-model eval pass rate drops below your quality bar, back off the cheap-model share.

**Stage 3 — At self-hosted GPU scale:**
6. Adopt **vLLM or SGLang** with prefix caching; add **LMCache** or **NVIDIA Dynamo** KV-aware routing for cross-request/-node reuse (~69% prefill savings at high hit rates). Add **speculative decoding** (EAGLE-3, 2–4x) and **FP8/NVFP4 quantization**. Use **Nemotron Nano / Qwen3** with thinking budgets as the cheap tier.
7. For agents, adopt **context editing + memory tool + sub-agent isolation** (up to 84% token cut) and **prompt compression** (LLMLingua) for long static context.

**What to ignore:** Don't chase the 85–98% vendor peak-savings numbers — they're distribution-specific. Don't build a bespoke router before you've done caching + batching + right-sizing (higher ROI, lower effort). Don't deploy semantic caching on multi-turn conversational context without stale-hit guards. Don't over-quantize (W4A4) reasoning models. Don't buy Groq/Cerebras/SambaNova for cost — they're latency plays.

## Caveats
- **Vendor vs measured:** Peak routing savings (RouteLLM 85%, FrugalGPT/Martian 98%, IBM 85%) are author-chosen distributions; fair-evaluation benchmarks (LLMRouterBench, 2026) find most routers barely beat baselines out-of-distribution. Realistic production routing savings are ~a third off, not 85%.
- **Silent quality regression** is the hidden tax of routing/caching — it surfaces as customer tickets days later, not on dashboards [Digital Applied Team](https://www.digitalapplied.com/blog/llm-model-routing-2026-cost-quality-optimization-engineering-guide) (Klarna's walk-back). Always gate with evals.
- **Cache economics require reuse:** a single unreused Anthropic write costs 1.25x [OpenRouter](https://openrouter.ai/blog/tutorials/prompt-caching-sticky-routing/) — caching one-off calls loses money; a warm cache only helps if the next request hits the same provider endpoint.
- **Lock-in / data exit:** SaaS routers and gateways see your prompts; within-family cloud routers (Bedrock IPR) lock you to one provider's models. OSS (LiteLLM, RouteLLM, vLLM, Langfuse, Helicone, Phoenix) keeps data and control in your infra.
- **Numbers flagged unverified/self-reported:** Martian's 98% and ~$1.3B valuation are marketing/press reports; Klarna's savings figures are company self-reported; Helicone OSS "maintenance mode" and some pricing figures come from third-party blogs; Azure Model Router's 4.5–14.2% is a single representative run; several routing survey figures are from secondary aggregators.

---

# (B) CEO Quick Explainer (tweet-thread length)

**1/ Turn on prompt caching today.** ~90% off any context you send repeatedly (system prompts, docs, RAG). Pays off after 2 reuses. A few lines of code. Biggest ROI, lowest effort.

**2/ Cap your output tokens.** Output costs 4–8x input. Tell the model to be concise + use JSON mode. Free money.

**3/ Batch anything that isn't real-time.** OpenAI/Anthropic/Bedrock give a flat 50% off for a 24-hour SLA — nightly jobs, evals, bulk classification.

**4/ Stop defaulting to the frontier model.** ~100x price spread; [Digital Applied Team](https://www.digitalapplied.com/blog/llm-model-routing-2026-cost-quality-optimization-engineering-guide) most traffic doesn't need the big brain. Add a router (Ramp Router, Bedrock routing, or open-source RouteLLM). Expect ~30% real savings — ignore the 85–98% marketing.

**5/ Add a gateway for control** (LiteLLM open-source, or Portkey/Cloudflare managed): budgets, fallbacks, per-team cost attribution, semantic cache. You can't optimize what you can't see.

**6/ If you self-host GPUs, the game changes:** vLLM/SGLang + KV-cache reuse (LMCache / NVIDIA Dynamo) + speculative decoding (EAGLE-3, 2–4x) + FP8 quantization. Use small efficient models (Nemotron Nano, Qwen3) with "thinking budgets" as the cheap tier.

**7/ For agents, context management is the lever:** Anthropic's context editing cut token use 84% on long tasks; use sub-agents that return summaries, not raw dumps.

**8/ Ignore:** bespoke routers before you've cached/batched/right-sized; 90%+ vendor savings claims; semantic caching on multi-turn chat without stale-hit guards; and buying Groq/Cerebras for cost (that's a speed play). Always eval-gate — a cheap wrong answer shows up as churn, not on your dashboard.

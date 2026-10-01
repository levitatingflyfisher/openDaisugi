# Q5: Small base models to replace Qwen2.5-1.5B-Instruct

Date of research: 2026-10-02. Method: web fetches of Hugging Face model cards, license pages, and vendor papers. Every claim has a source in the list at the end, tagged [n]. Where I could not confirm something, it says "could not verify".

## Short answer

- Default base: IBM Granite 4.1 3B (`ibm-granite/granite-4.1-3b`). Apache-2.0, US company, plain dense transformer, official GGUF from IBM, BFCL v3 60.8.
- Smaller option: IBM Granite 4.0 1B (`ibm-granite/granite-4.0-1b`), 1.6B, Apache-2.0. For very weak machines: `ibm-granite/granite-4.0-350m`.
- Avoid: SmolLM3-3B (instruct). Its post-training data came from Qwen3-32B and Qwen3-0.6B. That breaks the no-Qwen rule in spirit. Details below.

## Comparison table

| Model (HF repo id) | Params | License | Developer, country | Qwen-derived? | Tool use evidence | Context | Official or reputable GGUF | LoRA memory evidence |
|---|---|---|---|---|---|---|---|---|
| `ibm-granite/granite-4.1-3b` | 3B dense | Apache-2.0 [1] | IBM, US | Card names no Qwen; synthetic data source "not disclosed" [1] | BFCL v3 60.80, IFEval 82.30 [1] | 131,072 [1] | `ibm-granite/granite-4.1-3b-GGUF`, by IBM, Q2 to BF16, Q4_K_M ~2.2 GB [2] | Generic 3B QLoRA ~3.5 GB, LoRA 16-bit ~8 GB (Unsloth, generic) [3] |
| `ibm-granite/granite-4.0-1b` | 1.6B dense | Apache-2.0 [4] | IBM, US | No mention on card [4] | BFCL v3 54.82, IFEval 80.82 [4] | 128K [4] | `ibm-granite/granite-4.0-1b-GGUF`, by IBM, Q4_K_M ~1 GB [5] | Not stated for this model; below the 3B figure [3] |
| `ibm-granite/granite-4.0-350m` | 350M | Apache-2.0 [6] | IBM, US | No mention [6] | BFCL v3 39.32, IFEval 61.63 [6] | 32K [6] | Nano GGUF is in IBM collection [7]; exact repo for 350m not fetched (could not verify) | Not stated |
| `ibm-granite/granite-4.0-h-micro` | 3B hybrid Mamba2 + attention | Apache-2.0 [8] | IBM, US | No mention | BFCL v3 57.56 [8] | 128K [8] | Not checked | Hybrid arch; LoRA tooling support not verified |
| `mistralai/Ministral-3-3B-Instruct-2512` | 3.4B LM + 0.4B vision encoder | Apache-2.0 [9] | Mistral AI, France | Parent is Mistral Small 3.1, post-training teacher Mistral Medium 3.1 [10]. No Qwen found in abstract. | "native function calling and JSON output"; no tool benchmark on card [9] | 256k [9] | `mistralai/Ministral-3-3B-Instruct-2512-GGUF`, by Mistral, Q4_K_M 2.15 GB [11] | Not stated; Base repo `Ministral-3-3B-Base-2512` is made for custom post-training [12] |
| `google/gemma-4-E2B-it` | 2.3B effective, 5.1B with embeddings | Apache-2.0 [13] | Google, US | No mention [13] | "native support for structured tool use" [13] | 128K [13] | `ggml-org/gemma-4-E4B-it-GGUF` exists for E4B (llama.cpp project) [14]; E2B GGUF not fetched | Unsloth: E2B LoRA/QLoRA 8-10 GB VRAM [15] |
| `google/gemma-4-E4B-it` | 4.5B effective, 8B with embeddings | Apache-2.0 [16] | Google, US | No mention | Function calling supported [16] | 128K [16] | `ggml-org/gemma-4-E4B-it-GGUF`, Q4_0 4.59 GB [14] | Unsloth: E4B QLoRA ~10 GB, LoRA 17 GB [15] |
| `microsoft/Phi-4-mini-instruct` | 3.8B | MIT [17] | Microsoft, US | Not checked on card; could not verify | Function-calling format supported; no tool benchmark on card [17] | 128K [17] | Not checked (could not verify) | Generic 3-4B figure [3] |
| `microsoft/Phi-3.5-mini-instruct` | 3.8B | MIT [18] | Microsoft, US | Not checked | Card does not describe function calling [18] | 128K [18] | Not checked | Same as above |
| `HuggingFaceTB/SmolLM3-3B` | 3B | Apache-2.0 [19] | Hugging Face, US/France | Pretrain: no. Post-training: YES, Qwen3-32B and Qwen3-0.6B used for synthetic and preference data [20] | BFCL 92.3 on card (no-think) [19] | 64k trained, 128k with YaRN [19] | `ggml-org/SmolLM3-3B-GGUF`, Q4_K_M 1.92 GB [21] | Generic 3B figure [3] |
| `HuggingFaceTB/SmolLM2-1.7B-Instruct` | 1.7B | Apache-2.0 [22] | Hugging Face | SFT data: Smol-Magpie-Ultra used Llama-3.1-405B; the other subsets do not name the generator (could not verify) [23] | BFCL 27% [22] | Not on card [22] | Not checked | Small |
| `HuggingFaceTB/SmolLM2-360M-Instruct` | 360M | Apache-2.0 [24] | Hugging Face | As above | Function calling listed, no score [24] | Not on card | Not checked | Tiny |
| `allenai/OLMo-2-0425-1B-Instruct` | 1B | Apache-2.0 [25] | Allen Institute for AI (Ai2), US | Not checked; trained on Tulu 3 data [25] | No native tool use on card [25] | Not on card | Quant links mentioned, repo not verified [25] | Small |
| OLMo 3 | 7B and 32B only | Apache-2.0 [26] | Ai2, US | n/a | n/a | 64k | n/a | No 0.5-4B OLMo 3 found; could not verify any exists |
| `meta-llama/Llama-3.2-3B-Instruct` | 3.21B | Llama 3.2 Community License, custom [27] | Meta, US | No | Agentic uses listed; no tool score on card [27] | 128k [27] | Not checked | Generic 3B figure [3] |
| `google/gemma-3-4b-it` | 4B | "Gemma" terms, custom, gated [28][29] | Google, US | No | Not on card [28] | 128K [28] | Not checked | Heavy vocab, not checked |
| `google/gemma-3n-E4B-it` | 8B raw, 4B effective | Gemma terms, gated [30][29] | Google, US | No | Not on card | 32K [30] | Not checked | Not checked |
| `LiquidAI/LFM2-1.2B` | 1.17B | LFM Open License v1.0, custom [31][32] | Liquid AI, US | No | Agentic tasks recommended [31] | 32,768 [31] | llama.cpp supported [31] | CPU/GPU/NPU; SFT notebooks exist [31] |
| `LiquidAI/LFM2.5-1.2B-Instruct` | 1.17B | "LFM1.0" license [33] | Liquid AI, US | No | Pythonic function calls, JSON option [33] | 32,768 [33] | GGUF listed [33] | Not stated |

Notes on country: I took IBM, Microsoft, Google, Meta, Liquid AI and Ai2 as US companies, Mistral as French, and Hugging Face as US/French, from general knowledge. The cards I fetched did not state countries. Treat that column as unverified from primary sources.

## Findings by question

### Licenses
- Clean Apache-2.0 or MIT: Granite 4.x [1][4][6], Ministral 3 [9], Gemma 4 [13][16], Phi-4-mini and Phi-3.5-mini (MIT) [17][18], SmolLM2 and SmolLM3 [19][22], OLMo 2 and 3 [25][26].
- Big change in 2026: Gemma 4 moved to Apache-2.0 (April 2, 2026), dropping the old Gemma terms [34]. The model cards for E2B and E4B show Apache-2.0 [13][16]. Gemma 3 and 3n still use the Gemma terms [28][30].
- Gemma Terms of Use (Gemma 3, 3n): prohibited-use policy by reference, Google may restrict use remotely, restrictions must flow down to recipients, and derivative models (including models trained on outputs) carry the same terms [29]. A per-user LoRA product would inherit those. Avoid for this project.
- Llama 3.2: custom license. Meta approval needed above 700M monthly users, "Built with Llama" attribution, an acceptable-use list [27]. I did not see the full text, so other clauses (naming rules for derivatives) are could not verify.
- LFM2: LFM Open License v1.0. Commercial use only below $10M annual revenue for legal entities [32]. Not open source in the OSI sense. Fine for a hobbyist, a trap for later growth.

### Qwen lineage
- SmolLM3-3B: the blog says Qwen3-32B in reasoning mode generated synthetic data, and Qwen3-32B outputs were the "chosen" side and Qwen3-0.6B outputs the "rejected" side for preference alignment [20]. The base pretraining is separate [19]. Using the instruct model is therefore Qwen-influenced. SmolLM3-3B-Base would be cleaner, but I did not verify that its mid-training reasoning data is Qwen-free. The blog says mid-training used 140B reasoning tokens [20].
- Ministral 3: derived from Mistral Small 3.1 by cascade distillation; post-training distilled from Mistral Medium 3.1 [10]. These are Mistral's own models. The arXiv abstract does not mention Qwen [35]. I did not read the full paper, so benchmark comparisons against Qwen3 may appear there (the model card says it was compared with Qwen3 models [9]). Comparison is not lineage.
- Granite 4.1: card names no external generators; says "internal synthetic data" with source not disclosed [1]. So "no Qwen" is not proven, only not stated. Same applies to Granite 4.0 [4].
- Gemma 4: no Qwen mention on the card [13]. Not proven absent.
- Phi-4-mini, OLMo 2, Llama 3.2: lineage not checked. Could not verify.

### Tool use
- Highest self-reported scores on cards: SmolLM3 BFCL 92.3 [19] (not comparable with BFCL v3 numbers from IBM; different setups, and I did not check which BFCL variant). Granite 4.1 3B 60.80 BFCL v3 [1]; Granite 4.0 h-micro 57.56 [8]; Granite 4.0 1B 54.82 [4]; Granite 350M 39.32 [6]. SmolLM2-1.7B 27% [22].
- IBM's own numbers are at least on one named benchmark across the whole family, which makes size trade-offs easy to read.
- Ministral 3 and Gemma 4 claim native function calling with no score on the cards [9][13].

### Context
Granite 4.1 3B 131k [1]; Granite 4.0 1B 128K [4]; Ministral 3 256k [9]; Gemma 4 E2B/E4B 128K [13][16]. All are far more than a skill-distillation session needs.

### GGUF
- Official from the vendor: IBM publishes GGUF for 4.1-3B [2] and 4.0-1B [5]. Mistral publishes GGUF for Ministral 3 3B [11].
- Reputable third party: ggml-org (the llama.cpp project) for SmolLM3 [21] and Gemma 4 E4B [14].
- IBM warns that its F16 file fails on many hardware setups and recommends BF16 [5].

### LoRA memory
- Unsloth's published table: 3B QLoRA 3.5 GB, LoRA 16-bit 8 GB; these are "absolute minimum" figures [3]. So QLoRA of a 3B model fits a 6 GB GPU in principle. A 1B model fits with room.
- Gemma 4 is heavier than its size suggests: E2B needs 8-10 GB, E4B QLoRA ~10 GB [15]. It does not fit a 6 GB GPU.
- CPU training: I found no primary source with numbers. Could not verify.
- Unsloth's Granite 4.0 tutorial URL returned 404, so I have no Granite-specific training figure. Could not verify.
- Big open risk, could not verify: whether llama.cpp's LoRA-adapter conversion and runtime loading works for each architecture (Granite 4.1 is a plain dense Llama-like stack, Granite 4.0 h-* are hybrid Mamba, Gemma 4 and Ministral 3 are multimodal). My search did not return a supported-architecture list. Test one adapter end to end before committing.

## Ranked recommendation

1. Default base: `ibm-granite/granite-4.1-3b`.
   - Apache-2.0 [1], IBM, dense transformer (no Mamba kernels to worry about in llama.cpp or Go/Rust bindings) [1], official IBM GGUF [2], best documented tool-use score among the clean-license candidates [1], 131k context [1], QLoRA fits 6 GB by Unsloth's 3B figure [3].
   - Caveats: synthetic data source is undisclosed [1]; BFCL 60.8 is modest in absolute terms; verify LoRA-to-GGUF adapter loading.
   - Runner-up: `mistralai/Ministral-3-3B-Instruct-2512`, if a French vendor is acceptable. Apache-2.0, official GGUF, 256k, Mistral-own lineage [9][10][11]. Drawbacks: carries a 0.4B vision encoder, and the card gives no tool score. Its Base repo is aimed at custom post-training [12], which suits LoRA.
2. Smaller option for weak machines: `ibm-granite/granite-4.0-1b` (1.6B, BFCL 54.82, official GGUF ~1 GB at Q4) [4][5]. Below that, `ibm-granite/granite-4.0-350m` (BFCL 39.32) [6] is probably too weak for skill distillation; I would treat it as a floor test only.
3. Avoid: `HuggingFaceTB/SmolLM3-3B` (instruct). It has the best-looking tool score and the best license, which is why it tempts. But its post-training data was built from Qwen3-32B and Qwen3-0.6B outputs [20], which conflicts with the owner's exclusion. Second tier to skip: Gemma 3 and 3n (Gemma terms with flow-down and remote restriction [29]), Llama 3.2 (custom license [27]), LFM2/2.5 (revenue cap [32]). Gemma 4 is license-clean now [13] but needs 8-10+ GB to train [15].

## Open items (could not verify)
- Countries from primary sources.
- Qwen-free proof for Granite, Gemma 4, Phi-4-mini, OLMo 2 (cards silent).
- llama.cpp LoRA adapter support per architecture.
- CPU-only LoRA cost.
- Granite 4.0 350M GGUF repo id; Gemma 4 E2B GGUF repo id; Phi-4-mini GGUF.
- Full Llama 3.2 license text.

## Sources
1. https://huggingface.co/ibm-granite/granite-4.1-3b
2. https://huggingface.co/ibm-granite/granite-4.1-3b-GGUF
3. https://unsloth.ai/docs/get-started/fine-tuning-for-beginners/unsloth-requirements
4. https://huggingface.co/ibm-granite/granite-4.0-1b
5. https://huggingface.co/ibm-granite/granite-4.0-1b-GGUF
6. https://huggingface.co/ibm-granite/granite-4.0-350m
7. https://huggingface.co/collections/ibm-granite/granite-40-nano-language-models (seen in search results only; also https://itsfoss.com/news/ibm-granite-4-nano/)
8. https://huggingface.co/ibm-granite/granite-4.0-h-micro
9. https://huggingface.co/mistralai/Ministral-3-3B-Instruct-2512
10. https://arxiv.org/pdf/2601.08584 (as summarized in search results; also https://www.deeplearning.ai/the-batch/mistral-uses-cascade-distillation-on-mistral-3-to-build-ministral-family)
11. https://huggingface.co/mistralai/Ministral-3-3B-Instruct-2512-GGUF
12. https://huggingface.co/mistralai/Ministral-3-3B-Base-2512
13. https://huggingface.co/google/gemma-4-E2B-it
14. https://huggingface.co/ggml-org/gemma-4-E4B-it-GGUF
15. https://unsloth.ai/docs/models/gemma-4/train
16. https://huggingface.co/google/gemma-4-E4B-it
17. https://huggingface.co/microsoft/Phi-4-mini-instruct
18. https://huggingface.co/microsoft/Phi-3.5-mini-instruct
19. https://huggingface.co/HuggingFaceTB/SmolLM3-3B
20. https://huggingface.co/blog/smollm3
21. https://huggingface.co/ggml-org/SmolLM3-3B-GGUF
22. https://huggingface.co/HuggingFaceTB/SmolLM2-1.7B-Instruct
23. https://huggingface.co/datasets/HuggingFaceTB/smoltalk
24. https://huggingface.co/HuggingFaceTB/SmolLM2-360M-Instruct
25. https://huggingface.co/allenai/OLMo-2-0425-1B-Instruct
26. OLMo 3 overview from search results only (secondary): https://www.llmreference.com/model-family/olmo
27. https://huggingface.co/meta-llama/Llama-3.2-3B-Instruct
28. https://huggingface.co/google/gemma-3-4b-it
29. https://ai.google.dev/gemma/terms
30. https://huggingface.co/google/gemma-3n-E4B-it
31. https://huggingface.co/LiquidAI/LFM2-1.2B
32. https://huggingface.co/LiquidAI/LFM2-1.2B/blob/main/LICENSE
33. https://huggingface.co/LiquidAI/LFM2.5-1.2B-Instruct
34. https://thenextweb.com/news/google-gemma-4-open-models-apache-2-launch (secondary news; the HF cards [13][16] are the primary confirmation)
35. https://arxiv.org/abs/2601.08584

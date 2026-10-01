# Heritage check for small base-model candidates

Date of research: 2026-10-02. Web only. Every claim has a source number in [brackets]; the source list is at the end.

Method note. Page text was read through a fetch tool that summarizes pages with a small model. Treat quoted phrases as close paraphrase, not verified text. Where the table says "could not verify", I did not find a primary statement. The Ministral 3 paper and Gemma 4 report were downloaded as PDFs and searched as text, so those two are the strongest sources.

## Table

Legend. License class: P = permissive (Apache/MIT/CC-BY), C = custom open, R = restrictive. Qwen-free: YES, NO, UNKNOWN. "Qwen-free" here means: no Qwen init AND no Qwen/DeepSeek named as a data generator or teacher in the primary docs. Your rule may only need the first part. I give both.

| Model | Size | License class | Developer, country | Init base | Distillation or data-generator lineage | Qwen-free | GGUF |
|---|---|---|---|---|---|---|---|
| OpenReasoning-Nemotron-7B (also 1.5B, 14B, 32B) | 7B | P (CC-BY-4.0) [1] | NVIDIA, USA | Qwen/Qwen2.5-7B (HF base_model tag) [1] | Distilled reasoning data; card is Qwen-based | NO | community only; not checked |
| Llama-3.1-Nemotron-Nano-8B-v1 | 8B | C (NVIDIA Open Model License + Llama 3.1 license) [2] | NVIDIA, USA | Meta Llama-3.1-8B-Instruct [2] | Card says "Improved using Qwen". Data set was made by DeepSeek-R1, Qwen2.5-Math/Coder, Llama-3.3-70B, Mixtral-8x22B [3] | NO (data) | community |
| Llama-3.1-Nemotron-Nano-4B-v1.1 | 4B | C (NVIDIA Open + Llama 3.1) [4] | NVIDIA, USA | Llama-3.1-Minitron-4B-Width-Base, pruned and distilled from Llama 3.1 8B [4][5] | Card names no Qwen or DeepSeek [4], but it uses the same Llama-Nemotron post-training dataset family, whose generators include Qwen and DeepSeek [3][5]. Link from this card to that dataset not fully verified | NO or UNKNOWN (data) | yes, community [6] |
| Nemotron-Mini-4B-Instruct | 4B | C (NVIDIA community model license, older) [7] | NVIDIA, USA | Minitron-4B-Base, pruned and distilled from Nemotron-4 15B [7] | Only Nemotron-4 15B named as source [7]. Data list not in card | YES (init); data UNKNOWN | community; not checked |
| Nemotron-H-8B-Reasoning-128K, Nemotron-H-4B-Instruct-128K | 8B, 4B | R (NVIDIA internal scientific research license on the 8B reasoning card) [8] | NVIDIA, USA | Nemotron-H-8B-Base-8K (NVIDIA-native hybrid) [8] | Reasoning traces from DeepSeek R1 [8] | NO (DeepSeek data) | not checked |
| NVIDIA-Nemotron-Nano-9B-v2 | 9B | C (NVIDIA Open Model License) [9][10] | NVIDIA, USA | From scratch. Pruned from the 12B v2 (HF tag finetune of 12B) [9][11] | Synthetic data from DeepSeek-R1/V3, Qwen2.5-72B, Qwen3-30B-A3B, Qwen3-235B-A22B, Mixtral, phi-4, Gemma 3, Nemotron-4 [11][12] | NO (data) | community; official repo GGUF fetch gave 401, not verified |
| NVIDIA-Nemotron-Nano-12B-v2 (-Base) | 12B | C [12] | NVIDIA, USA | From scratch, hybrid Mamba-2 [12] | Same generator list; card says "Improved using Qwen" [12] | NO (data) | n/a (over 9B) |
| NVIDIA-Nemotron-3-Nano-4B | 4B | C (NVIDIA Nemotron Open Model License) [13][14] | NVIDIA, USA | Compressed with Nemotron Elastic from Nano-9B-v2 (HF base_model) [13][14] | Post-training data from DeepSeek R1/R1-0528, Qwen3-235B-A22B, Qwen2.5-32B/14B, Nemotron-4 340B, phi-4, gpt-oss-120b [14] | NO (data) | yes, official nvidia GGUF repo exists [15] |
| NVIDIA-Nemotron-3-Nano-30B-A3B | 30B total, 3B active | C [16] | NVIDIA, USA | "Trained from scratch" per search snippet [16] | Not checked in detail | n/a (too large for your range) | not checked |
| Gemma 3 1B/4B (-it) | 1B, 4B | C (Gemma Terms of Use) [17][18] | Google DeepMind, USA | Gemma 3 pretrained (HF base_model: gemma-3-4b-pt) [17] | "Trained with distillation" [19]. Teacher not named in what I read. Card names no Qwen or DeepSeek [20]. 1B not individually checked | YES (as far as disclosed) | yes; google official QAT GGUF and ggml-org [21] |
| Gemma 3n E2B/E4B | 2B, 4B effective | C (Gemma Terms) [22] | Google DeepMind, USA | gemma-3n-E4B base (HF tag) [22] | Not checked in detail | YES (as far as disclosed) | yes, unsloth/lmstudio [21] |
| Gemma 4 E2B/E4B (also 12B, 26B-A4B, 31B) | 2.3B, 4.5B eff. | P (Apache-2.0 in card field) [23][24] | Google DeepMind, USA | gemma-4-E4B base (HF tag) [23]. Gemma 4 12B "trained from scratch" [24] | Report says post-training is "similar to Gemma 3" [24]. The word "distill" does not appear in the Gemma 4 report text. No Qwen or DeepSeek named as generators in report or card [23][24]. Report compares against Qwen 3.5 and DeepSeek only as benchmarks [24] | YES (as far as disclosed) | yes: ggml-org and unsloth [21] |
| Llama 3.2 1B/3B (-Instruct) | 1B, 3B | C (Llama 3.2 Community License) [25][26] | Meta, USA | Pruned from Llama 3.1 8B; trained up to 9T tokens [27] | Logits from Llama 3.1 8B and 70B used as targets [27]. Post-training synthetic generators not named [27] | YES (as far as disclosed) | community only (bartowski, lmstudio...) [21] |
| Ministral 3 3B and 8B (Base, Instruct, Reasoning; 2512) | 3B, 8B | P (Apache-2.0) [28][29] | Mistral AI, France | Pruned from Mistral Small 3.1 (24B) with "Cascade Distillation" [30] | Pretraining logit distillation from Mistral Small 3.1. SFT logit distillation from Mistral Medium 3. 3B reasoning SFT from Magistral Small 1.2. Then ODPO with an internal reward model, GRPO [30]. Qwen and Gemma appear only as benchmark rivals [30] | YES (as far as disclosed) | 8B-Instruct official mistralai GGUF [31]; 3B community only [31] |
| Granite 4.0 1B / 350M | 1B, 0.35B | P (Apache-2.0) [32][33] | IBM, USA | granite-4.0-1b-base / 350m-base (HF tags) [32][33] | Cards: public permissive data, "internal synthetic data", some human-curated. No generator named [34] | YES (as far as disclosed) | not checked for 1B; 4.1-3B has an official one [35] |
| Granite 4.1 3B | 3B | P (Apache-2.0) [36] | IBM, USA | Granite-4.1-3B-Base per card text. HF API has no base_model field [36][37] | SFT used about 4.1M samples filtered by an LLM judge. IBM blog names no generator or judge [38]. Third-party blog (Kili) says Granite 4.2 CodeAlchemy code used GPT-OSS and judges were GPT-OSS-120B and Gemma 4 [39]. Not confirmed for 4.1 | YES (as far as disclosed) | yes, official ibm-granite GGUF [35] |

Sizes in the 0.5B to 9B band that look cleanest for your rule: Gemma 4 E2B/E4B, Gemma 3 4B, Ministral 3 3B/8B, Granite 4.1 3B, Granite 4.0 1B/350M, Llama 3.2 1B/3B.

## Answers to your specific questions

### 1. NVIDIA
- NVIDIA-native architecture: Nemotron-H (4B, 8B), Nemotron Nano 2 (9B, 12B) and Nemotron 3 Nano 4B (compressed from Nano 9B v2). [8][12][14]
- Llama-derived: Llama-3.1-Nemotron-Nano 8B (from Llama 3.1 8B Instruct) and Nano 4B (pruned from Llama 3.1 8B). [2][4]
- Qwen-derived by init: OpenReasoning-Nemotron. The 7B card has base_model Qwen/Qwen2.5-7B and a qwen2 architecture tag. I only fetched the 7B. The 1.5B, 14B and 32B were not checked. [1]
- "NVIDIA-native" does not mean Qwen-free. Nemotron Nano 2 and Nemotron 3 Nano 4B had Qwen and DeepSeek write part of their synthetic data. Nemotron-H-8B-Reasoning uses DeepSeek R1 traces. [8][11][12][14]
- Nemotron 3 Nano is a 30B-A3B mixture-of-experts. It is too large for your band. The only small Nemotron 3 I found is the 4B, which comes from Nano 9B v2. [13][16] I did not find a Nemotron 3 model under 4B for text chat.
- Nemotron-Mini-4B-Instruct is the one with the best story: pruned and distilled from NVIDIA's own Nemotron-4 15B, and the card names no Qwen. Its older license is "NVIDIA Community Model License". Data sources are not listed, so data heritage is unknown. [7]
- NVIDIA Open Model License in brief [40]: perpetual, worldwide, royalty-free, commercial use allowed. You own your derivative models. Redistribution needs the notice "Licensed by NVIDIA Corporation under the NVIDIA Open Model License". The license ends if you sue NVIDIA over IP, or if you bypass the model's guardrails without a similar one. No user-count or field-of-use limit. The summary came from a fetch tool, so read the text before relying on it. Note: the 4B card now names a "NVIDIA Nemotron Open Model License" with a different link [14]. I did not read that text. Could not verify whether its terms differ.

### 2. Google
- Gemma 3 and 3n: Gemma Terms of Use. Prohibited-use policy applies. Google reserves the right to restrict use remotely. Copies of the terms must pass to recipients. Distillation counts as a "Model Derivative". Outputs are not derivatives. [17][41]
- Gemma 4: the HF API shows license apache-2.0, not gated [23]. The report says Apache 2.0 [24]. Press coverage dates the release to 2026-04-02 and calls it a break from the custom license [42]. I could not fetch Google's own blog post (404). Treat the date as secondary.
- Gemma 4 sizes in the report: E2B, E4B, 12B, 26B-A4B, 31B. [24]
- Gemini: Google DeepMind's page describes Gemini as its proprietary system and Gemma as its open line [43]. A search summary says Gemini has no downloadable weights and is reached by API only [44]. I found no primary Google statement saying "no open weights" in those words. Could not verify with a primary quote. Gemma cards say Gemma is "built from the same research and technology" as Gemini. That is shared research, not a Gemini-derived weight set. Gemma 3's report says its models are trained with distillation, and I did not find the teacher named. [19][20]
- So "Gemini" in the owner's words most likely means Gemma. Gemma is Google, USA.

### 3. Meta Llama 3.2 1B/3B
- License key terms [26]: a license from Meta is needed if the licensee's products exceed 700 million monthly active users. Distributors must show "Built with Llama" and a notice line. Derived AI models must start their name with "Llama". The Acceptable Use Policy applies. Meta can end the license on breach, and then you delete the materials.
- EU limit: it is in the Acceptable Use Policy, for the multimodal Llama 3.2 models only (11B, 90B vision). EU-domiciled individuals and companies are not granted the rights. End users of products that include them are exempt. Text-only 1B/3B are not covered. [45] The license text itself has no EU clause. [26]
- Newer small Llama: I found none. Llama 4 Scout/Maverick are 109B/400B MoE. [46] An HF listing of newest meta-llama models showed nothing newer than April 2025 [47]. Could not verify that Meta released no small model later in 2026.
- Lineage: 1B and 3B were pruned from Llama 3.1 8B and trained with logits from 8B and 70B. [27]

### 4. Ministral 3 (arXiv 2601.08584, read from the PDF) [30]
- Base lineage: all Ministral 3 sizes (3B, 8B, 14B) come from Mistral Small 3.1 Base by iterative prune-then-distill. The vision encoder is copied from Mistral Small 3.1.
- Teachers: Mistral Small 3.1 in pretraining. Mistral Medium 3 in SFT. Magistral Small 1.2 for the 3B reasoning SFT.
- Preference and RL: Online DPO using a pairwise reward model. GRPO with an LLM judge. The judge model is not named.
- Qwen3, Gemma 3 and Mistral Small 3.2 appear only as comparison models in tables.
- The paper does not list pretraining web data sources. Mistral Small 3.1's own data is not described there. So Mistral's lineage is Mistral-only as far as disclosed, but not provable.
- License: Apache-2.0 on the 3B and 8B Instruct cards. [28][29] The HF tag for the Instruct FP8 repos says base_model:quantized:...-Base-2512, which is a loose label (they are post-trained models, not mere quantizations). [28]
- Note the model card lists Chinese among supported languages. That is language coverage, not lineage.

### 5. IBM Granite
- Granite 4.0 1B card: SFT data is "publicly available datasets with permissive license", "internal synthetic data targeting specific capabilities", and a select set of human-curated data. No generator model is named. [34]
- Granite 4.1 3B card repeats the same wording. [36] IBM's HF blog on 4.1 names no generator, no judge and no teacher. [38]
- I could not find a Granite 4 technical report. Could not verify any arXiv report. The HF tags carry the placeholder "arxiv:0000.00000". [32][36]
- A third-party article (Kili) says CodeAlchemy synthetic code used GPT-OSS models, and judges were GPT-OSS-120B and Gemma 4. It is about Granite 4.2 and is not IBM's own text. [39] No Qwen, DeepSeek or Mixtral was named in any Granite source I read. That is absence of disclosure, not proof.
- Sizes: IBM lists 3B, 8B and 30B for 4.1. [38] I checked 4.1-3B. Granite 4.1 has no 1B or 350M in what I read; those are 4.0 models.

## Automatic heritage checking through the HF API

What works.
- `GET https://huggingface.co/api/models/{id}` returns `cardData.base_model` (string or list) and `tags`. Tags include `base_model:{id}` and `base_model:{relation}:{id}`, where relation is finetune, adapter, merge or quantized. [48]
- The Hub infers the relation, and the author can override it with `base_model_relation`. [48]
- `?expand[]=baseModels` returned a structured block in my fetch. Its shape is below. I got it through a summarizing fetch tool, so test it yourself before depending on it.
- The reverse lookup works too: `GET /api/models?filter=base_model:finetune:Qwen/Qwen2.5-7B` lists children. [49]

Example response shape (trimmed). This is the OpenReasoning-Nemotron-7B result [1]:

```json
{
  "id": "nvidia/OpenReasoning-Nemotron-7B",
  "license": "cc-by-4.0",
  "tags": ["transformers","safetensors","qwen2","text-generation",
           "base_model:Qwen/Qwen2.5-7B",
           "base_model:finetune:Qwen/Qwen2.5-7B",
           "license:cc-by-4.0"],
  "cardData": { "base_model": ["Qwen/Qwen2.5-7B"] }
}
```

And the structured form [49]:

```json
{ "baseModels": { "relation": "finetune",
    "models": [ { "id": "Qwen/Qwen2.5-7B-Instruct" } ] } }
```

How reliable it is. Not very, alone. Real cases from this research:
1. Author-declared. The Hub docs say you "can specify" base_model. It is not checked against the weights. [48]
2. Missing. Nemotron-Mini-4B-Instruct has no base_model [7]. Granite 4.1 3B has none [37]. Llama 3.2 3B-Instruct has none [25]. Absence does not mean "no Qwen".
3. Only the direct parent. Llama-3.1-Nemotron-Nano-4B-v1.1 lists nvidia/Llama-3.1-Minitron-4B-Width-Base. The word Llama appears only in the name. A tool must walk the chain to the root. [4] Nano-9B-v2 lists the 12B. Nemotron-3-Nano-4B lists Nano-9B-v2. [9][13]
4. Blind to data and teachers. Nano-9B-v2's metadata shows no Qwen, but its model card says Qwen and DeepSeek generated data. [9][11] Metadata also cannot show distillation from a teacher unless the author sets it.
5. Imprecise relation labels. Ministral 3 Instruct says "quantized" of the Base. [28]
6. Gated or missing repos. Some fetches returned 401. API calls on gated models still return metadata for public tags, but anything private fails.
7. Quantizers and community GGUF repos usually carry `base_model:quantized:...`, which points one hop to the instruct model. Walk to the root.

Suggested tool design (for later, not built):
- Step 1: fetch `/api/models/{id}` and read `cardData.base_model` plus `base_model:*` tags. Recurse up to a depth limit (say 6) with a visited set. Flag any ancestor id matching a deny-list (Qwen/, Alibaba, DeepSeek, and others you list), and any ancestor whose architecture tag is `qwen2` or `qwen3`.
- Step 2: also check `tags` for `qwen*` architecture and `config.json` `model_type`. This catches missing base_model on a Qwen-architecture model. It will miss re-labeled weights.
- Step 3: use `datasets` metadata and a curated allow-list file (model id to human-reviewed heritage note, like the table above). The allow-list is what carries data-generator facts, because the API cannot.
- Step 4: if base_model is missing, return UNKNOWN, never "clean". Fail closed.
- Step 5: pin the commit sha you reviewed. Heritage can change when a card is edited.

## Gaps and could-not-verify list
- Official Google blog post for Gemma 4 (404). Release date and Apache switch rest on press plus the HF card and the Gemma 4 report.
- Gemma 3 1B and Gemma 3n E2B were not individually fetched. Gemma 3 teacher identity not found.
- Gemini "no open weights" has no primary quote. DeepMind page and a search summary only.
- NVIDIA Nemotron Open Model License text for the new "Nemotron" variant: not read.
- Nemotron Nano 4B v1.1 data: card names no Qwen, but I did not confirm which dataset version it used.
- OpenReasoning-Nemotron 1.5B/14B/32B not checked.
- Granite 4 technical report: not found. Granite 4.0 vs 4.1 vs 4.2 generator lists unconfirmed from IBM.
- Mistral Small 3.1 pretraining data: not described.
- Any small Llama released after Llama 3.2: not found, not proven absent.
- Llama 3.2 AUP full text was read only through a summary of the 11B Vision card.

## Sources
[1] https://huggingface.co/api/models/nvidia/OpenReasoning-Nemotron-7B
[2] https://huggingface.co/nvidia/Llama-3.1-Nemotron-Nano-8B-v1
[3] https://huggingface.co/datasets/nvidia/Llama-Nemotron-Post-Training-Dataset
[4] https://huggingface.co/api/models/nvidia/Llama-3.1-Nemotron-Nano-4B-v1.1 and https://huggingface.co/nvidia/Llama-3.1-Nemotron-Nano-4B-v1.1
[5] https://arxiv.org/abs/2408.11796 (cited in the tags of [4]; not opened)
[6] https://huggingface.co/api/models?search=NVIDIA-Nemotron-3-Nano-4B-GGUF&limit=10
[7] https://huggingface.co/nvidia/Nemotron-Mini-4B-Instruct and https://huggingface.co/api/models/nvidia/Nemotron-Mini-4B-Instruct
[8] https://huggingface.co/nvidia/Nemotron-H-8B-Reasoning-128K
[9] https://huggingface.co/api/models/nvidia/NVIDIA-Nemotron-Nano-9B-v2
[10] https://huggingface.co/nvidia/NVIDIA-Nemotron-Nano-9B-v2
[11] same as [10] (generator list)
[12] https://huggingface.co/nvidia/NVIDIA-Nemotron-Nano-12B-v2-Base
[13] https://huggingface.co/api/models/nvidia/NVIDIA-Nemotron-3-Nano-4B-BF16
[14] https://huggingface.co/nvidia/NVIDIA-Nemotron-3-Nano-4B-BF16
[15] https://huggingface.co/api/models?search=NVIDIA-Nemotron-3-Nano-4B-GGUF&limit=10
[16] https://build.nvidia.com/nvidia/nemotron-3-nano-30b-a3b/modelcard (via search result) and https://huggingface.co/api/models?author=nvidia&search=nemotron&sort=lastModified&limit=60
[17] https://huggingface.co/api/models/google/gemma-3-4b-it
[18] https://huggingface.co/google/gemma-3-4b-it
[19] https://arxiv.org/abs/2503.19786
[20] https://huggingface.co/google/gemma-3-4b-it (Gemini research statement)
[21] https://huggingface.co/api/models?search=gemma-4-E4B&limit=15 ; https://huggingface.co/api/models?search=gemma-3-4b-it-gguf&limit=10 ; https://huggingface.co/api/models?search=Llama-3.2-3B-Instruct-GGUF&limit=10
[22] https://huggingface.co/api/models/google/gemma-3n-E4B-it
[23] https://huggingface.co/api/models/google/gemma-4-E4B-it and https://huggingface.co/google/gemma-4-E4B-it
[24] https://arxiv.org/abs/2607.02770 (PDF https://arxiv.org/pdf/2607.02770, read as text)
[25] https://huggingface.co/api/models/meta-llama/Llama-3.2-3B-Instruct
[26] https://huggingface.co/meta-llama/Llama-3.2-3B-Instruct/blob/main/LICENSE.txt
[27] https://huggingface.co/meta-llama/Llama-3.2-3B-Instruct
[28] https://huggingface.co/api/models/mistralai/Ministral-3-8B-Instruct-2512
[29] https://huggingface.co/api/models/mistralai/Ministral-3-3B-Instruct-2512
[30] https://arxiv.org/abs/2601.08584 (PDF https://arxiv.org/pdf/2601.08584, read as text)
[31] https://huggingface.co/api/models?search=Ministral-3-GGUF&limit=15
[32] https://huggingface.co/api/models/ibm-granite/granite-4.0-1b
[33] https://huggingface.co/api/models/ibm-granite/granite-4.0-350m
[34] https://huggingface.co/ibm-granite/granite-4.0-1b and https://huggingface.co/ibm-granite/granite-4.0-1b-base
[35] https://huggingface.co/api/models?search=granite-4.1-3b-GGUF&limit=10
[36] https://huggingface.co/ibm-granite/granite-4.1-3b
[37] https://huggingface.co/api/models/ibm-granite/granite-4.1-3b
[38] https://huggingface.co/blog/ibm-granite/granite-4-1 ; https://www.ibm.com/granite/docs/models/granite4-1 ; https://research.ibm.com/blog/granite-4-1-ai-foundation-models
[39] https://kili-technology.com/blog/data-story-ibm-granite-4-2 (third party)
[40] https://www.nvidia.com/en-us/agreements/enterprise-software/nvidia-open-model-license/
[41] https://ai.google.dev/gemma/terms
[42] https://www.implicator.ai/google-releases-gemma-4-under-apache-2-0-dropping-its-custom-ai-license/ (secondary)
[43] https://deepmind.google/models/gemma/
[44] web search summary on Gemini vs Gemma (secondary): https://www.androidcentral.com/apps-software/google-launches-gemma-open-model
[45] https://huggingface.co/meta-llama/Llama-3.2-11B-Vision-Instruct
[46] https://tech-insider.org/llama-4-vs-qwen-vs-mistral-2026/ (secondary)
[47] https://huggingface.co/api/models?author=meta-llama&sort=createdAt&direction=-1&limit=15
[48] https://huggingface.co/docs/hub/model-cards
[49] https://huggingface.co/api/models/unsloth/Qwen2.5-7B-Instruct?expand[]=baseModels and https://huggingface.co/api/models?filter=base_model:finetune:Qwen/Qwen2.5-7B&limit=3 ; API docs pointer https://huggingface.co/docs/hub/api

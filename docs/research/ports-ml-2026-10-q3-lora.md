# Q3: LoRA training without Python, and adapters from Go and Rust

Research date: 2026-10-02. Web only. Every claim has a source in the list at the end. "Could not verify" means exactly that.
Some fetches were summarised by a small model, so exact quotes come from the README and PR pages as returned. Re-check any load-bearing line before you build on it.

## 1. llama.cpp training today

Short answer: there is a `llama-finetune` example. It does full-parameter fine-tuning. It does NOT train LoRA adapters in master. It is labelled work in progress.

- The `examples/training` directory holds `finetune.cpp`. Its README says finetuning is "technically functional (for FP32 models and limited hardware setups) but the code is very much WIP." [S1]
- The README says LLaMA 3.2 1b and Stories 260K "seems to work with 24 GB of memory." [S1]
- CPU: compile without other backends. GPU: CUDA only, "use the maximum number of GPU layers" (so no partial offload). Flash attention is off during training because `FLASH_ATTN_EXT` has no backward pass. [S1]
- The base PR is #10544 "llama/ggml: add LLM training support", by Johannes Gaessler, merged 2025-05-12. It adds `ggml_opt` based training with `llama_opt_init`, `llama_opt_epoch` and `llama_save_model_to_file`. Single GPU with all layers only. No multi-GPU. [S2]
- On LoRA the PR author said: "There is no implementation for LoRA training." [S2]
- He called it "janky" and not for production. [S2]
- Cost data from that PR: one epoch on Wikitext-2 test for LLaMA 3.2 1b took about 3 minutes on an RTX 4090 and about 15 hours on an Epyc 7742 CPU. [S2]
- The older `finetune` example (2023, before the rewrite) did train LoRA, but it was removed. The PR #27199 author writes of "potentially add back the LoRA training example", which implies it is gone. [S3] I did not fetch the removal PR. Could not verify the removal date.
- Architectures: the README names only Stories 260K and LLaMA 3.2 1b as working. No full architecture list found. Could not verify others.
- Community LoRA/QLoRA attempts, none merged:
  - #20453 "native QLoRA training with reward-weighted SFT and GRPO". Closed by the author on 2026-05-04 to split it up. The maintainer doubted review depth because of an AI-written description. Tested on Qwen3 1.7B Q4_K_M on an RTX 4060 Ti. Adapters were loadable by `llama_adapter_lora_init` and `llama-export-lora`. [S4]
  - #22705 "Feat/qlora training" (MoE QLoRA, CPU/CUDA/Metal/Vulkan ops). Closed by the author 2026-09-24. [S5]
  - #26794 successor. Closed 2026-08-11 without merge. Maintainer 0cc4m: "Way too many changes." 215 commits. [S6]
  - #28269 "webgpu: add backward kernels" is open. [S7]
  - #27199 "finetune: fix no KV cache" merged 2026-09-02. A bug fix only. [S3, S7]
- Verdict: experimental, moving slowly, not LoRA. Do not plan on it for a 6 GB Pascal card. Nothing I found says it runs on Pascal. Could not verify.

## 2. Rust: candle, candle-lora, burn, mistral.rs

- candle (huggingface): the examples directory has `mnist-training` and `reinforcement-learning` as training examples. No LoRA or LLM fine-tune example. [S8] Issues "How to fine-tune Llama?" (#894) and "How to reduce memory usage of backpropagation?" (#1241) show the gap. #1241 is still open. [S9]
- candle-lora (Eric Buehler, MIT): a LoRA layer-swap library for candle models. It lists llama, mistral, falcon, bert, t5, resnet and more, with weight merging. [S10] The last push was 2025-04-18, so it looks stale. 7 open issues. [S11] It has no stated training recipe, no GPU memory numbers, no adapter export to PEFT format that I could find. Could not verify that a full SFT loop on a causal LM works end to end today.
- mistral.rs (MIT): LoRA and X-LoRA are inference only, with per-request adapter selection. No training. [S12]
- burn (tracel-ai): real, recent progress.
  - #5139 "Feat/lora qlora" merged 2026-07-13. It adds `LoraConfig` and `LoraMapper`, and a text-classification fine-tune example with rank 8. Unit tests, 92% patch coverage. [S13]
  - Follow-up fixes in Aug 2026 (#5362, #5364, #5365). [S14]
  - Bug #5351: LoRA gradients explode on an Apple M1 Max Metal backend. Closed as not planned. [S15]
  - Earlier PRs #3993 (burn-peft) and #4321 were closed as stale. [S14, S16]
  - The official models repo has no LoRA example. Llama 2 is a community port. [S17]
  - I found no causal-LM LoRA example, no PEFT adapter export, and no GGUF export. Could not verify. Burn is a viable training framework to build on, not a drop-in.
- Gaps against PyTorch+PEFT: no ready SFT loop for causal LMs, no TRL equivalent, no tested 4-bit path on CUDA for Pascal, no PEFT-format `adapter_model.safetensors` writer that I could confirm. All of those are "could not verify", not "proven absent".

## 3. Go

- I found nothing that is a general LoRA trainer in Go. Searches turned up one thing: `go-inference` / `go-mlx` on forge.lthn.ai, a LoRA trainer through MLX-C bindings, i.e. Apple Silicon only. [S18] Provenance of that forge host: could not verify. It is not useful on a Linux Pascal box.
- Go projects found (yzma, go-llama.cpp) are inference only. [S19, S22]
- Expect "no" for Go training. Shell out to a trainer, or use llama.cpp/Rust.

## 4. Inference: loading LoRA from Go and Rust

llama.cpp side (the reference):
- `llama-server` flags: `--lora FNAME` (comma list), `--lora-scaled FNAME:SCALE`, `--lora-init-without-apply`. `GET/POST /lora-adapters` sets global scales. `/completion` takes a per-request `lora` list of `{id, scale}`. Different adapter sets block request batching. [S20]
- `llama-export-lora` merges adapters into a base GGUF. [S21]
- Convert a PEFT adapter: `python3 convert_lora_to_gguf.py <adapter_dir> --outtype f16 --base <base_model_dir>`. Added in PR #8332, merged 2024-07-15. It uses a hot-swap design: B(A(x)) at runtime, no merge. [S23]
- The script imports torch, transformers, safetensors and the in-tree gguf-py, so conversion still needs Python. [S24]
- Limits: DoRA unsupported. Adapters with new tokens fail. `lm_head` adapters fail with tied embeddings. [S24]
- Licence: llama.cpp is MIT, "The ggml authors." [S25]

Rust:
- llama-cpp-2 (utilityai/llama-cpp-rs): Apache-2.0 per GitHub API (README says dual Apache-2.0/MIT). Last push 2026-10-02. Crate version 0.1.158. [S26, S27]
- API: `LlamaContext::lora_adapter_set(&self, adapter, scale)` and `lora_adapter_remove` (which clears all adapters; upstream now replaces all at once via `llama_set_adapters_lora`). `LlamaLoraAdapter` is `!Send` and `!Sync`. [S28]
- Provenance: GitHub org "utilityai". Contributor country could not verify.

Go:
- yzma (hybridgroup, org "Hybrid Group", Apache-2.0 in LICENSE; GitHub API shows NOASSERTION): uses purego and ffi, so no CGo. Last push 2026-10-02. Release v1.28.0 for llama.cpp "v0.5.0" (naming as returned). Claims "over 96% of llama.cpp functionality". [S19, S29, S30]
- Its `pkg/llama` has `AdapterLoraInit(model, path)`, `SetAdaptersLora(ctx, adapters, scales)`, `AdapterLoraFree`, plus metadata and aLoRA helpers. [S31] LoRA is unsupported in its WebAssembly build. [S30]
- Provenance: Hybrid Group org. Country of maintainers could not verify.
- go-llama.cpp (go-skynet, MIT, CGo): last push 2026-09-25 per API. I found no LoRA mention in its README. Treat as unconfirmed for adapters. [S22, S32]
- Hot-swap from either language: both wrap the same C functions (`llama_set_adapters_lora`), so per-context swap is possible. A simpler route for the three binaries: run `llama-server` and use the REST `/lora-adapters` calls. [S20]

## 5. Cost of a per-user run

Few sources exist. Numbers I could confirm:
- llama.cpp full fine-tune, LLaMA 3.2 1b, one Wikitext-2 test epoch: ~15 hours on a 64-core Epyc 7742, ~3 minutes on an RTX 4090. [S2] This is full FT, not LoRA, so it is a poor guide for LoRA cost.
- PyTorch+PEFT, Qwen2.5-1.5B LoRA/QLoRA on an RTX 4060 8 GB (not a GTX 1060): 500 tok/s at batch 1, seq 512, 1.72M tokens in 3494 s; 628 tok/s with paged 8-bit AdamW; VRAM 6.2 to 8.1 GB. [S33] My arithmetic: a few thousand examples at about 500 tokens is 1 to 2M tokens, so about 30 to 60 minutes per epoch on that card. That figure is derived, not quoted.
- PEFT on CPU only, Qwen2.5-0.5B, 50 examples: about 10 minutes on a Core Ultra 5 NUC with 64 GB RAM. A blog, not rigorous. [S34] Scaling that to thousands of examples is my guess and I do not trust it: could not verify.
- A search snippet claimed Qwen2.5-0.5B memory of 10.18 GB full, 2.88 GB LoRA 16-bit, 1.7 GB QLoRA. I did not find the underlying page. Treat as unverified.
- GTX 1060 (Pascal) specific training numbers: could not verify. I found none.

## Recommendation

1. Keep PEFT + TRL in Python as the trainer for now. It is the only path with confirmed LoRA on causal LMs, adapter export, and a known cost. No Python-free trainer I found is production ready.
2. Make the training step a clean boundary: input JSONL, output a PEFT adapter directory, then `convert_lora_to_gguf.py` makes the GGUF adapter. Go and Rust binaries only do inference, which is where all three languages are fine.
3. Inference from Go: yzma (Apache-2.0, US org by name only, active, no CGo) using `AdapterLoraInit` and `SetAdaptersLora`. Or call `llama-server` over REST for all languages, with hot-swap through `/lora-adapters`.
4. Inference from Rust: llama-cpp-2 (Apache-2.0/MIT, active) with `lora_adapter_set`. Remember the `!Send`/`!Sync` limit.
5. Watch, do not adopt: burn LoRA/QLoRA (merged July 2026, no LLM example yet) is the most credible Rust trainer path. llama.cpp training could add LoRA later (maintainer said it is missing; #27199 hints at a return). Re-check both in a few months.
6. Avoid: candle-lora (stale since April 2025), the Go MLX trainer (Apple only, unknown host), mistral.rs for training (inference only).
7. Owner rule on China-based projects: none of the projects above is known by me to be China-based, but I verified provenance only to the GitHub org name level. Model choice matters more: Qwen is made by Alibaba. If the rule covers model weights, pick a non-China base model. Not researched here.

## Sources

S1 https://raw.githubusercontent.com/ggml-org/llama.cpp/master/examples/training/README.md
S2 https://github.com/ggml-org/llama.cpp/pull/10544
S3 https://github.com/ggml-org/llama.cpp/pull/27199
S4 https://github.com/ggml-org/llama.cpp/pull/20453
S5 https://github.com/ggml-org/llama.cpp/pull/22705
S6 https://github.com/ggml-org/llama.cpp/pull/26794
S7 https://github.com/ggml-org/llama.cpp/pulls?q=is%3Apr+lora+finetune
S8 https://github.com/huggingface/candle/tree/main/candle-examples/examples
S9 https://github.com/huggingface/candle/issues?q=LoRA+training
S10 https://github.com/EricLBuehler/candle-lora
S11 https://api.github.com/repos/EricLBuehler/candle-lora
S12 https://github.com/EricLBuehler/mistral.rs
S13 https://github.com/tracel-ai/burn/pull/5139
S14 https://github.com/tracel-ai/burn/issues?q=LoRA
S15 https://github.com/tracel-ai/burn/issues/5351
S16 https://github.com/tracel-ai/burn/pull/4321
S17 https://github.com/tracel-ai/models
S18 https://forge.lthn.ai/core/go-inference/wiki/Training (search result only, not fetched)
S19 https://github.com/hybridgroup/yzma
S20 https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/server/README.md
S21 https://raw.githubusercontent.com/ggml-org/llama.cpp/master/tools/export-lora/README.md
S22 https://github.com/go-skynet/go-llama.cpp
S23 https://github.com/ggml-org/llama.cpp/pull/8332
S24 https://raw.githubusercontent.com/ggml-org/llama.cpp/master/convert_lora_to_gguf.py
S25 https://github.com/ggml-org/llama.cpp/blob/master/LICENSE
S26 https://github.com/utilityai/llama-cpp-rs
S27 https://api.github.com/repos/utilityai/llama-cpp-rs
S28 https://docs.rs/llama-cpp-2/latest/llama_cpp_2/context/struct.LlamaContext.html and https://docs.rs/llama-cpp-2/latest/llama_cpp_2/model/struct.LlamaLoraAdapter.html
S29 https://api.github.com/repos/hybridgroup/yzma and https://raw.githubusercontent.com/hybridgroup/yzma/main/LICENSE
S30 https://raw.githubusercontent.com/hybridgroup/yzma/main/README.md
S31 https://pkg.go.dev/github.com/hybridgroup/yzma/pkg/llama
S32 https://api.github.com/repos/go-skynet/go-llama.cpp
S33 https://arxiv.org/pdf/2509.12229v1 (Profiling LoRA/QLoRA Fine-Tuning Efficiency on Consumer GPUs: An RTX 4060 Case Study)
S34 https://blog.bjdean.id.au/2025/06/fine-tuning-small-language-models-on-a-basic-desktop-pc/

# Porting the Python-only parts: robotics, VLA, training, packaging (research, 2026-10-03)

A deep-research run. Every source was read on 2026-10-03. Questions 3 to 5 had no claim survive verification; see the follow-up notes.

## Question

For openDaisugi (a local-first runtime-assurance gate for AI agents, shipped as competing Python, Go and Rust binaries that must reach feature parity), research how to bring the remaining Python-only parts to Go and Rust, or call them from Go/Rust safely, as of late 2026. Cite sources for every claim; say 'could not verify' plainly. Exclude China-based developers/runtimes and Qwen-based models (owner rule). Five questions: (1) ROBOTICS SIMULATION: MuJoCo's C API (DeepMind, Apache-2.0) from Rust and Go: maintained bindings (e.g. mujoco-rs, mujoco-rust, Go cgo bindings), their licenses, maintainers, MuJoCo version support (3.x), headless rendering (EGL/OSMesa) for camera images, and how hard a minimal executor is (load MJCF, step, read qpos, set ctrl, render an image). (2) VLA INFERENCE WITHOUT PYTHON: can LeRobot's SmolVLA (lerobot/smolvla_base, ~450M, SmolVLM2 backbone) and Physical Intelligence pi0/pi0.5 (openpi) be exported to ONNX, ExecuTorch, LiteRT or TorchScript and run from C/C++/Rust/Go on CPU or a 6GB GPU? Known exports, official or community, flow-matching action head issues, reported latencies; any other small open VLA (Apache/MIT) that is easier to export. (3) LoRA / small-model FINE-TUNING outside Python: state of LoRA training in Rust (candle, burn, mistral.rs) and Go; llama.cpp's own finetune/LoRA training support (llama-finetune, ggml training) and whether it can train LoRA adapters on CPU/small GPU; maturity and limits versus PyTorch+PEFT. Plus inference of LoRA adapters via llama.cpp/GGUF from Go/Rust. (4) CALLING PYTHON FROM RUST AND GO when it must stay Python: subprocess worker with a framed protocol vs embedding (PyO3 / pyo3 auto-initialize; Go via cgo + libpython, go-python3 status), failure isolation, GIL, memory; and PACKAGING a self-contained Python runtime with a native binary: python-build-standalone (Astral), uv-managed pinned environments with hashes (uv.lock, --require-hashes), PyTorch CPU wheel sizes vs CUDA, offline bundles, how tools like this ship optional ML packs on Arch/AUR (optdepends, split packages) and as release tarballs; examples of projects that ship Rust/Go binaries plus an on-demand Python ML runtime. (5) SMALL BASE MODELS for per-user LoRA fine-tuning of agent 'skills' (the daisugi garden distills user sessions into skills and wants a small fine-tuned model per user): non-Qwen, permissively licensed (Apache/MIT; flag custom licenses like Llama/Gemma terms) models around 0.5B to 4B in 2026, e.g. HuggingFace SmolLM2/SmolLM3, IBM Granite 3.x/4.x small, Microsoft Phi-4-mini, Google Gemma 3 (terms), Meta Llama 3.2 1B/3B (license), AllenAI OLMo 2 1B/OLMo small, Mistral small edge models: license, provenance, size, tool-use/instruction quality, GGUF availability, and LoRA training memory needs on a 6GB GPU or CPU. End with a recommendation per question.

## Summary

Only questions 1 (MuJoCo from Rust/Go) and 2 (VLA inference without Python) have verified evidence. Questions 3 (LoRA training outside Python), 4 (calling and packaging Python) and 5 (small non-Qwen base models) had no claims survive verification, so this report gives no sourced answer for them. Question 1: Rust is ready. mujoco-rs (MIT OR Apache-2.0, one maintainer, crate 6.1.0) binds MuJoCo 3.12.0 and covers load MJCF, step, read qpos and offscreen RGB/depth rendering. MuJoCo's official README lists no Go binding, so Go would need its own cgo wrapper over the Apache-2.0 C API, using the EGL/OSMesa pattern from the official record.cc sample for headless camera images. Question 2: VLA inference without Python works today with caveats. SmolVLA has an official Arm ONNX export validated on ONNX Runtime CPU and an independent ONNX run on a 6 GB RTX 2060. vla.cpp packages SmolVLA, pi0 and pi0.5 as self-contained GGUF files served from C++ with no Python. In every path, tokenization, normalization and state discretization stay outside the graph and must be rebuilt natively, and one preprint shows an ONNX export can quietly lower task success. Recommendations: Q1, adopt mujoco-rs for Rust and write a thin cgo binding for Go. Q2, target SmolVLA through ONNX Runtime (callable from both Rust and Go) or vla.cpp GGUF, with native pre/post-processing and a closed-loop parity test against PyTorch before trusting any export. Q3-Q5, no recommendation can be sourced from this run; they need a new research pass.

## Verified findings

- **high** (3-0 (merged claims 0,1,3,4)): mujoco-rs is the maintained Rust binding for MuJoCo. Crate 6.1.0 (released 2026-09-22) binds MuJoCo 3.12.0 through FFI, needs Rust 1.95 or newer, is dual-licensed MIT OR Apache-2.0, and has one owner (davidhozic). It is pinned to one MuJoCo version and runs a few weeks behind upstream: MuJoCo 3.13.0 and 3.14.0 were already out when 6.1.0 shipped.
  - Evidence: Cargo.toml has version 6.1.0+mj-3.12.0 and license MIT OR Apache-2.0. The README says it uses FFI bindings to MuJoCo 3.12.0 and needs Rust 1.95 or newer. crates.io lists five releases from May to Sept 2026. The google-deepmind/mujoco releases page shows 3.12.0 (2026-08-20), 3.13.0 and 3.14.0 (2026-09-22). One maintainer is a bus-factor risk.
  - Sources: https://github.com/davidhozic/mujoco-rs, https://docs.rs/crate/mujoco-rs/latest, https://crates.io/api/v1/crates/mujoco-rs

- **high** (3-0 (merged claims 2,5,6,7)): A minimal Rust executor (load MJCF, step, read qpos, render a camera image) is directly supported by mujoco-rs: MjModel::from_xml_string, make_data, step, qpos via views, and an offscreen MjRenderer that writes RGB and depth to an array or file. Rendering sits behind the non-default 'renderer' feature. True offscreen rendering is documented only on Linux with driver support; Windows and macOS need 'renderer-winit-fallback' (an invisible window). The docs never name EGL or OSMesa, so display-less headless servers could not be verified.
  - Evidence: README example: from_xml_string -> make_data -> step -> view(&data).qpos[..3]. Renderer docs: 'rendering RGB and depth images to either an array or to a file'. The 'renderer' feature is off by default. Greps for egl, osmesa and headless in the docs found nothing. Cargo.toml suggests the Linux path goes through glutin, which can use EGL, but the docs do not say so. Setting ctrl was not explicitly verified, but it is part of the same MjData API.
  - Sources: https://github.com/davidhozic/mujoco-rs, https://docs.rs/crate/mujoco-rs/latest, https://mujoco-rs.readthedocs.io/en/latest/programming/visualization/renderer.html

- **high** (3-0 (merged claims 8,9)): MuJoCo itself is an Apache-2.0 C library maintained by Google DeepMind, so FFI from Rust or Go has no license conflict. Its official sample record.cc shows headless rendering through a compile-time switch: EGL (pbuffer, no display) or OSMesa (software). The default GLFW path uses an invisible window and still needs a display. This is the pattern a Go cgo binding or a headless Rust build should copy.
  - Evidence: README: 'Source code is licensed under the Apache License, Version 2.0.' record.cc lines 21-30: #if defined(MJ_EGL) / #elif defined(MJ_OSMESA) / #else GLFW. The EGL branch uses eglGetDisplay(EGL_DEFAULT_DISPLAY) with EGL_NO_SURFACE. A verifier saw no GPL or LGPL in the bundled dependencies; Eigen is MPL-2.0, a per-file copyleft.
  - Sources: https://github.com/google-deepmind/mujoco, https://github.com/google-deepmind/mujoco/blob/main/sample/record.cc

- **high** (3-0 (claim 10)): No Go binding for MuJoCo is listed officially. The README's first-party bindings are Python, JavaScript/WASM and C#/Unity. Its third-party list has exactly one Rust binding (MuJoCo-rs) and no Go entry. Go parity therefore means writing an in-house cgo wrapper over the small C API. Community cgo bindings outside the README were not surveyed.
  - Evidence: README 'Third-party bindings' lists MATLAB Simulink, Swift, Java, Julia and Rust (davidhozic/mujoco-rs). A grep for go or golang found nothing. A separate claim that the full loop needs only about 7 specific C calls was refuted 0-3, so the exact surface of a Go wrapper is still unmeasured.
  - Sources: https://github.com/google-deepmind/mujoco

- **high** (3-0 (merged claims 14,16)): SmolVLA can be exported to ONNX and run without PyTorch. Arm's official Learning Path exports the policy (VLM plus action expert) to FP32 ONNX and validates it on ONNX Runtime CPUExecutionProvider on Arm, matching PyTorch within atol and rtol of 1e-3. The flow-matching noise is an explicit graph input of shape [1,50,32], and the graph is fixed-shape. The export itself runs in Python. Only inference is Python-free. The checkpoint used was a LIBERO fine-tune, not smolvla_base. No latency is given.
  - Evidence: Page: 'You've exported SmolVLA to FP32 ONNX and validated its action output with ONNX Runtime on an Arm CPU'. 'PASS: ONNX Runtime matches PyTorch within atol=0.001 and rtol=0.001'. 'The batch includes an explicit flow-matching noise tensor'. Weights sit in external data files next to model.onnx.
  - Sources: https://learn.arm.com/learning-paths/cross-platform/smolvla-onnx-conversion/convert-and-validate-onnx/

- **high** (3-0 (merged claims 15,20)): In every known VLA export, pre- and post-processing stay outside the graph. A Rust or Go host must reimplement them natively: tokenization (the HF tokenizers crate can cover this), state normalization, action un-normalization, and for pi0.5 state discretization, using the stats from the checkpoint. This is the main non-model engineering cost of Python-free VLA inference.
  - Evidence: Arm: 'The exporter writes the policy to an ONNX graph, and the processors remain outside it.' OpenTau: 'tokenization and state discretization are done outside the ONNX graph.' 'Must reimplement' is an inference the sources do not state, but the verifiers judged it sound.
  - Sources: https://learn.arm.com/learning-paths/cross-platform/smolvla-onnx-conversion/convert-and-validate-onnx/, https://opentau.readthedocs.io/en/latest/tutorials/inference.html

- **medium** (3-0 (claims 19,20), single project-doc source): pi0.5 has a community ONNX export path. OpenTau's export_to_onnx.py exports trained PI05 checkpoints, and only PI05, using the same train config as training. The output is float32 model.onnx plus model.onnx.data, because the model exceeds the 2 GiB ONNX limit. Every runtime example is Python (ONNX Runtime or TensorRT), with no C/C++/Rust/Go host shown. This is not an official Physical Intelligence or openpi export. OpenTau's maintainers and license were not checked against the owner's exclusion rule.
  - Evidence: Docs: 'Only PI05 policies are supported for ONNX export.' 'Export uses the same train config as training.' No latency or non-Python host evidence.
  - Sources: https://opentau.readthedocs.io/en/latest/tutorials/inference.html

- **medium** (2-1 (claim 11), 3-0 (claims 12,13)): vla.cpp (a ggml/llama.cpp-style C++ runtime, Apache-2.0) packages each VLA as one self-contained GGUF and serves it from a C++ ZeroMQ server with no Python or PyTorch at inference. Python is used only for convert_*_to_gguf.py, quantize_gguf.py and eval clients. Its roadmap marks SmolVLA, pi0, pi0.5, GR00T N1.5/1.6/1.7, BitVLA, Evo-1, VLA-Adapter, OpenVLA-OFT and VLA-JEPA as supported on CPU and CUDA. Metal is supported only for SmolVLA, pi0 and GR00T N1.7. Self-reported client-side SmolVLA latency, including transport and with bf16 weights: 86 ms on an RTX 3090, 262 ms on an AGX Orin, 567 ms on an 8 GB Orin Nano and 888 ms on an Apple M4. The cited repo is a 0-star fork. The upstream is VinRobotics/vla.cpp (215 stars, pushed 2026-10-02) and should be cited instead. VinRobotics' origin, and that of the Evo-1, VLA-Adapter and VLA-JEPA models, was not checked against the owner's exclusion rule.
  - Evidence: README: 'a single self-contained GGUF that needs no Python or PyTorch at inference time'. Roadmap table columns are CPU/CUDA/Metal/OpenVINO/Hexagon. The benchmark table is labelled 'inference plus transport, measured client-side'. There is no independent reproduction. The README cites arXiv 2606.08094 and LIBERO-finetuned GGUFs at hf.co/vrfai. Because it is a C++ server over ZeroMQ, Rust and Go can call it the same way with no FFI.
  - Sources: https://github.com/AIWintermuteAI/vla.cpp, https://github.com/VinRobotics/vla.cpp

- **medium** (2-1 (claim 17), 3-0 (claim 18)): SmolVLA runs on a 6 GB consumer GPU through ONNX, but faster is not the same as equivalent. One preprint exported HuggingFaceVLA/smolvla_libero with tether 0.12.0 (opset 19, 10 denoise steps) and ran it on ONNX Runtime CUDA EP on an RTX 2060 6 GB: p99 was about 601 ms against 1181 ms for PyTorch+AMP. The 'FP16' and 'INT8' artifacts were really identical FP32 graphs. The default static 16-token language width truncated half the Spatial instructions and cut Spatial success to 41%. Re-exporting at 24 tokens brought it back to 75%, against a 70% PyTorch baseline. The author says width does not explain every difference.
  - Evidence: Single-author, non-peer-reviewed preprint (2026-09-18), one GPU, LIBERO only, seed 42. ONNX p99 came from a separate bench harness, so the 2x is not strictly like-for-like. The lesson for openDaisugi: any non-Python VLA port needs a closed-loop task-success parity test, not only a tensor-tolerance check.
  - Sources: https://arxiv.org/pdf/2609.14146

- **medium** (3-0 (claim 21), single preprint): Distilling the flow-matching head to one step looks like a promising way to make VLA export simpler and faster. SnapFlow self-distillation brings pi0.5 (3B) to one forward pass with no architecture change. It reports 98.75% average LIBERO success against 97.75% for the 10-step teacher, and end-to-end latency down from 274 ms to 83 ms on an A800-80G. That a one-step head removes the ODE loop and simplifies ONNX/TensorRT export is an inference; the paper never mentions export.
  - Evidence: Preprint, April 2026. Datacenter GPU only; nothing on CPU or a 6 GB GPU. Distillation needs about 12 h of extra GPU training in PyTorch/LeRobot. The 1-point gain over the teacher is within noise. A related claim that the action head dominates latency was refuted 0-3.
  - Sources: https://arxiv.org/pdf/2604.05656

## Caveats

The user asked which VLA openDaisugi already uses. This run could not answer: two greps of the repo for SmolVLA, OpenVLA or pi0 timed out on this box. The parent session should check the repo or its memory directly. No claims for questions 3, 4 and 5 survived verification: LoRA training in Rust, Go or llama.cpp; Python embedding vs subprocess workers and python-build-standalone/uv packaging; small non-Qwen base models and their licenses. Nothing here backs any recommendation on those topics, including the user's packaging question and the per-user fine-tuned model question. Do not fill them from memory and present the result as sourced. Most VLA evidence is self-reported (vla.cpp README) or from single, non-peer-reviewed preprints with narrow setups (one GPU, LIBERO simulator only). The vla.cpp source cited was a fork; cite upstream VinRobotics/vla.cpp. Owner exclusion rule: the origins of VinRobotics, OpenTau, and the Evo-1, VLA-Adapter and VLA-JEPA models were not checked. Time sensitivity: mujoco-rs is already two MuJoCo minor versions behind, and both the VLA export tooling and vla.cpp were changing monthly as of Sept-Oct 2026. Three claims were refuted and are left out: an exact 7-call C API surface for the executor; SmolVLA ONNX success-drop figures from a paired run; and the claim that the action head dominates latency.

## Open questions

- Which VLA (if any) does openDaisugi currently use or reference? The repo grep timed out here; check the code and project memory directly.
- Can Go bind MuJoCo headlessly with a small hand-written cgo layer, and is there any maintained community Go binding outside the official README? The exact C call surface was not verified (the related claim was refuted).
- Do mujoco-rs offscreen rendering and vla.cpp GGUF inference actually work display-free on a Linux box with a 6 GB GTX 1060-class GPU or CPU only, and what is the real end-to-end latency there?
- Questions 3-5 are unanswered and need a fresh sourced pass: llama.cpp/candle/burn LoRA training maturity on CPU or 6 GB GPU; subprocess vs PyO3/cgo embedding plus python-build-standalone + uv --require-hashes packaging and AUR optdepends patterns; and license and provenance of non-Qwen 0.5-4B base models (SmolLM3, Granite 4, Phi-4-mini, OLMo, Gemma/Llama terms) for per-user skill LoRAs.

## Refuted claims

- (0-3) The full minimal executor loop uses only a small set of C API calls: load the model (mj_loadXML or mj_loadModel), mj_makeData, mj_forward, mj_step, mjv_updateScene, mjr_render and mjr_readPixels. Any Rust or Go FFI binding would need to wrap just these calls. (https://github.com/google-deepmind/mujoco/blob/main/sample/record.cc)
- (0-3) The faster ONNX export did not keep closed-loop task success. LIBERO Spatial success fell from 70% to 41%, and in a paired 300-episode run from 56.7% to 33.0% (McNemar p=2.2e-9). Object success stayed about the same (88-89%). (https://arxiv.org/pdf/2609.14146)
- (0-3) Flow-matching VLAs (pi0, pi0.5, SmolVLA) usually run about 10 denoising ODE steps. For pi0.5 on an A800 GPU, each step takes about 23 ms and the 10-step chain about 241 ms, out of 274 ms end to end. So the action head, not the VLM prefix, is the main latency cost for any non-Python export. The figures come from a datacenter A800, not a 6GB consumer GPU or a CPU. (https://arxiv.org/pdf/2604.05656)

## Follow-up: the SmolVLA export (2026-10-03)

Checked locally, not by the research run. Both model cards (`lerobot/smolvla_base` at
d9f33c94, its backbone `HuggingFaceTB/SmolVLM2-500M-Video-Instruct` at 7b375e1b) say
Apache-2.0. The checkpoint is 865 MB, with the VLM in bfloat16. The project's virtual
environment has torch and transformers but not lerobot, and lerobot pins other versions of
both, so the export runs in a separate pinned environment (`clients/vla/requirements.txt`).

The recipe is `clients/vla_export.py` (rulings VL-R-2 to VL-R-4). It does not follow Arm's
single graph: under a 4 GB memory cap the policy is three FP32 graphs (vision encoder, prefix
to key and value cache, one denoise step), each exported in its own process. The text width
is the checkpoint's own 48 tokens, not 16. The noise is the caller's, as in Arm's export.
Tokenization, image resize and padding, state and action normalization, the time embedding
and the 10 Euler steps stay outside the graphs (VL-R-3). Against the FP32 PyTorch policy on
one input, the largest difference over a 50x32 action chunk is 1.8e-6. One chunk takes about
2.6 s on this CPU in both runtimes.

The Go and Rust executors run the graphs through our own ONNX Runtime wrappers and a
hand-written tokenizer. The backbone's `tokenizer.json` names a Digits pre-tokenizer, but the
oracle's `GPT2Tokenizer` (transformers 5) does not run it; the ports follow the oracle (VL-R-5).
In the pick-place closed loop the trust test the research asked for holds run by run: each
replayed run is within 3.7e-6 of the oracle, the first free run matches, and Go and Rust agree
bit for bit. The free trajectory against the oracle drifts after run 0, because a qpos gap in
the 7th digit moves a few edge pixels of the next image (VL-R-7). Per chunk on a 4-core i5: Go
2.8 s and Rust 3.0 s; PyTorch 2.6 to 3.2 s on the check and golden inputs, and 6.7 s on the
loop's dark frames, so the numbers are not a speed comparison between runtimes.

## Sources

- https://github.com/davidhozic/mujoco-rs (primary; MuJoCo from Rust/Go (robotics sim executor))
- https://docs.rs/crate/mujoco-rs/latest (primary; MuJoCo from Rust/Go (robotics sim executor))
- https://mujoco-rs.readthedocs.io/en/latest/programming/visualization/renderer.html (primary; MuJoCo from Rust/Go (robotics sim executor))
- https://github.com/google-deepmind/mujoco/blob/main/sample/record.cc (primary; MuJoCo from Rust/Go (robotics sim executor))
- https://docs.pytorch.org/rl/0.9/reference/generated/knowledge_base/MUJOCO_INSTALLATION.html (secondary; MuJoCo from Rust/Go (robotics sim executor))
- https://github.com/google-deepmind/mujoco (primary; MuJoCo from Rust/Go (robotics sim executor))
- https://github.com/AIWintermuteAI/vla.cpp (primary; VLA export without Python (SmolVLA, pi0))
- https://chatpaper.com/de/paper/296727 (secondary; VLA export without Python (SmolVLA, pi0))
- https://learn.arm.com/learning-paths/cross-platform/smolvla-onnx-conversion/convert-and-validate-onnx/ (primary; VLA export without Python (SmolVLA, pi0))
- https://arxiv.org/pdf/2609.14146 (primary; VLA export without Python (SmolVLA, pi0))
- https://opentau.readthedocs.io/en/latest/tutorials/inference.html (primary; VLA export without Python (SmolVLA, pi0))
- https://arxiv.org/pdf/2604.05656 (primary; VLA export without Python (SmolVLA, pi0))
- https://huggingface.co/datasets/echodict/llama.cpp/raw/main/examples/training/README.md (secondary; LoRA training outside PyTorch)
- https://cdn04132025.gitlink.org.cn/replica/llama.cpp/commit/10d2af0eaa0aafd7c6577b279dfa5221ff44a63f (primary; LoRA training outside PyTorch)
- https://gittrend.io/repo/EricLBuehler/candle-lora (secondary; LoRA training outside PyTorch)
- https://github.com/EricLBuehler/mistral.rs/blob/master/README.md (primary; LoRA training outside PyTorch)
- https://docs.astral.sh/python-build-standalone/ (primary; Calling and shipping Python from Rust/Go)
- https://backiee.wasmer.app/https_github_com/davnn/uv-pack (primary; Calling and shipping Python from Rust/Go)
- https://pydevtools.com/handbook/explanation/how-do-i-ship-a-python-application-to-end-users.md (blog; Calling and shipping Python from Rust/Go)
- https://docs.astral.sh/uv/guides/integration/pytorch/ (primary; Calling and shipping Python from Rust/Go)
- https://github.com/Datadog/go-python3 (primary; Calling and shipping Python from Rust/Go)
- https://huggingface.co/HuggingFaceTB/SmolLM3-3B (primary; Small permissive base models for per-user LoRA)
- https://huggingface.co/ibm-granite/granite-4.0-h-1b (primary; Small permissive base models for per-user LoRA)
- https://tinyweights.dev/posts/smollm3-3b-vs-phi-4-mini/ (unreliable; Small permissive base models for per-user LoRA)
- https://unsloth.ai/docs/get-started/fine-tuning-for-beginners/unsloth-requirements (primary; Small permissive base models for per-user LoRA)

"""Export lerobot/smolvla_base to three FP32 ONNX graphs for the Go and Rust VLA executors.

    CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_export.py --part vision --out DIR
    CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_export.py --part prefix --out DIR
    CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_export.py --part denoise --out DIR
    CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_export.py --part check --out DIR
    HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_export.py --part assets --out DIR

VENV is a separate virtual environment made from clients/vla/requirements.txt
(lerobot 0.6.1 and its pinned dependencies, with hashes). The project's own
virtual environment cannot hold lerobot: lerobot pins an older torch,
transformers and numpy. The checkpoint and its backbone's tokenizer files
must already be in HF_HOME at the pinned revisions (ruling VL-R-2); the
script never downloads.

The policy is split in three graphs, each exported in its own process so
that the export stays under a 4 GB memory cap:

- vision.onnx: image [1,3,512,512] (RGB in [0,1]) to img_emb [1,64,960],
  the vision encoder and connector output.
- prefix.onnx: img_emb, lang_tokens [1,48] (int64), lang_mask [1,48]
  (int64, 1 for a token), state [1,32] (the normalized state,
  zero-padded) to kv_keys and kv_values [16,1,5,P,64], the key and value
  cache of the 16 VLM layers over the P = 64 + 48 + 1 prefix tokens.
- denoise.onnx: x_t [1,50,32], time_emb [1,720], kv_keys, kv_values, prefix_mask
  [1,P] (int64) to v_t [1,50,32], one velocity of the flow-matching head.

The caller runs vision, then prefix, then the 10 Euler steps over denoise, as lerobot's
euler_integrate does. Tokenization, the newline step, image scaling to
[0,1], state padding and normalization, and action unnormalization stay
outside the graphs (ruling VL-R-3). So does the sine-cosine time
embedding (time_embedding below): lerobot computes it in float64, and the
CPU provider of ONNX Runtime has no float64 Cos.

--part assets copies the two small files the executors read beside the
graphs: the backbone's tokenizer.json and the checkpoint's normalizer
stats (stats.safetensors, the same file for the pre and post processors).

--part check runs the three graphs with onnxruntime and the Euler loop in numpy
against the PyTorch policy on one fixed input, and prints the largest
difference and the SHA-256 of each file.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time
from pathlib import Path

# huggingface_hub reads this at import: unless set, it fetches its agent
# list from the Hub and names the agent in its User-Agent. A set value is kept.
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

POLICY_REPO = "lerobot/smolvla_base"
POLICY_REV = "d9f33c94a60fb382c90dea2164c96845bd955e28"
BACKBONE_REPO = "HuggingFaceTB/SmolVLM2-500M-Video-Instruct"
BACKBONE_REV = "7b375e1b73b11138ff12fe22c8f2822d8fe03467"
OPSET = 17

# The oracle (the policy loader, the constants, the noise) is one module,
# also carried by the vla-ref pack.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src" / "opendaisugi" / "pack"))
import vla_oracle  # noqa: E402
from vla_oracle import (  # noqa: E402, F401 - the constants, as before
    IMAGE,
    MAX_PERIOD,
    MIN_PERIOD,
    NUM_STEPS,
    TIME_DIM,
    WIDTH,
)


def snapshot(repo: str, rev: str) -> Path:
    home = Path(os.environ.get("HF_HOME", Path.home() / ".cache" / "huggingface"))
    p = home / "hub" / ("models--" + repo.replace("/", "--")) / "snapshots" / rev
    if not p.is_dir():
        sys.exit(f"vla_export: {repo} at {rev} is not in {home}")
    return p


def rss(tag: str) -> None:
    import resource

    print(
        f"{tag}: peak rss {resource.getrusage(resource.RUSAGE_SELF).ru_maxrss // 1024} MB",
        file=sys.stderr,
    )


def load_policy(part: str = "all"):
    """vla_oracle.load_policy on the pinned snapshot (the backbone must be
    there too, for the tokenizer)."""
    path = snapshot(POLICY_REPO, POLICY_REV)
    snapshot(BACKBONE_REPO, BACKBONE_REV)
    return vla_oracle.load_policy(path, part, rss)


def fix_vision_positions(pol) -> None:
    """The vision embeddings compute their position ids with
    torch.bucketize, which the ONNX exporter cannot write. For a full
    512x512 image with no padding mask the ids are a constant: take them
    from the eager code once and use them as a constant."""
    import torch

    vm = pol.model.vlm_with_expert.get_vlm_model().vision_model
    emb = vm.embeddings
    side = IMAGE // emb.patch_size
    captured = {}
    orig = emb.position_embedding.forward

    def grab(ids):
        captured["ids"] = ids.clone()
        return orig(ids)

    emb.position_embedding.forward = grab
    with torch.no_grad():
        emb(torch.zeros(1, 3, IMAGE, IMAGE), torch.ones(1, side, side, dtype=torch.bool))
    emb.position_embedding.forward = orig
    ids = captured["ids"]
    assert torch.equal(ids, torch.arange(side * side).unsqueeze(0)), "unexpected position ids"

    def forward(pixel_values, patch_attention_mask):
        patch = emb.patch_embedding(pixel_values)
        e = patch.flatten(2).transpose(1, 2)
        return e + emb.position_embedding(ids)

    emb.forward = forward

    # With no padding mask every patch attends to every patch, so the
    # encoder needs no mask; the mask helper cannot be traced.
    from transformers.modeling_outputs import BaseModelOutput

    def vision_forward(pixel_values, patch_attention_mask=None, **kwargs):
        h = emb(pixel_values, None)
        h = vm.encoder(inputs_embeds=h, attention_mask=None).last_hidden_state
        return BaseModelOutput(last_hidden_state=vm.post_layernorm(h))

    vm.forward = vision_forward


class Vision:
    """image -> img_emb."""

    def __new__(cls, pol):
        import torch

        m = pol.model

        class _Vision(torch.nn.Module):
            def __init__(self):
                super().__init__()
                self.m = m

            def forward(self, image):
                return self.m.vlm_with_expert.embed_image(image * 2.0 - 1.0)

        return _Vision()


class Prefix:
    """img_emb, lang_tokens, lang_mask, state -> kv_keys, kv_values."""

    def __new__(cls, pol):
        import torch
        from lerobot.policies.common.vla_utils import make_att_2d_masks

        m = pol.model

        class _Prefix(torch.nn.Module):
            def __init__(self):
                super().__init__()
                self.m = m

            def forward(self, img_emb, lang_tokens, lang_mask, state):
                # embed_prefix calls embed_image; hand it the embeddings.
                self.m.vlm_with_expert.embed_image = lambda e: e
                img_mask = torch.ones(1, dtype=torch.bool)
                embs, pad, att = self.m.embed_prefix(
                    [img_emb], [img_mask], lang_tokens, lang_mask.to(torch.bool), state=state
                )
                att2d = make_att_2d_masks(pad, att)
                pos = torch.cumsum(pad, dim=1) - 1
                _, cache = self.m.vlm_with_expert.forward(
                    attention_mask=att2d,
                    position_ids=pos,
                    past_key_values=None,
                    inputs_embeds=[embs, None],
                    use_cache=True,
                )
                keys = torch.stack([layer.keys for layer in cache.layers])
                values = torch.stack([layer.values for layer in cache.layers])
                return keys, values

        return _Prefix()


class Denoise:
    """x_t, time_emb, kv_keys, kv_values, prefix_mask -> v_t."""

    def __new__(cls, pol):
        import torch
        from lerobot.policies.smolvla import modeling_smolvla
        from transformers.cache_utils import DynamicCache

        # The graph takes the time embedding as an input.
        modeling_smolvla.create_sinusoidal_pos_embedding = lambda t, *a, **k: t

        m = pol.model

        class _Denoise(torch.nn.Module):
            def __init__(self):
                super().__init__()
                self.m = m

            def forward(self, x_t, time_emb, kv_keys, kv_values, prefix_mask):
                cache = DynamicCache()
                for i in range(kv_keys.shape[0]):
                    cache.update(kv_keys[i], kv_values[i], i)
                return self.m.denoise_step(
                    prefix_pad_masks=prefix_mask.to(torch.bool),
                    past_key_values=cache,
                    x_t=x_t,
                    timestep=time_emb,
                )

        return _Denoise()


def sample_inputs(seed: int = 0):
    import torch

    g = torch.Generator().manual_seed(seed)
    image = torch.rand(1, 3, IMAGE, IMAGE, generator=g)
    tokens = torch.full((1, WIDTH), 2, dtype=torch.int64)
    tokens[0, :5] = torch.tensor([18188, 614, 260, 3608, 198])
    mask = torch.zeros(1, WIDTH, dtype=torch.int64)
    mask[0, :5] = 1
    state = torch.zeros(1, 32)
    state[0, :3] = torch.tensor([0.1, -0.2, 0.01])
    noise = torch.randn(1, 50, 32, generator=g)
    return image, tokens, mask, state, noise


def time_embedding(t: float):
    """The sine-cosine time embedding [1,720] in float64, cast to float32,
    as lerobot's create_sinusoidal_pos_embedding computes it (dimension
    720, min_period 0.004, max_period 4.0)."""
    import numpy as np

    fraction = np.linspace(0.0, 1.0, TIME_DIM // 2, dtype=np.float64)
    period = MIN_PERIOD * (MAX_PERIOD / MIN_PERIOD) ** fraction
    x = (1.0 / period * 2 * np.pi) * np.float64(np.float32(t))
    return np.concatenate([np.sin(x), np.cos(x)])[None, :].astype(np.float32)


def sha256(p: Path) -> str:
    h = hashlib.sha256()
    with p.open("rb") as f:
        for b in iter(lambda: f.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def export(part: str, out: Path) -> None:
    import torch

    pol = load_policy(part)
    image, tokens, mask, state, _ = sample_inputs()
    if part == "vision":
        fix_vision_positions(pol)
        mod = Vision(pol).eval()
        args = (image,)
        names_in = ["image"]
        names_out = ["img_emb"]
    elif part == "prefix":
        mod = Prefix(pol).eval()
        args = (torch.zeros(1, 64, 960), tokens, mask, state)
        names_in = ["img_emb", "lang_tokens", "lang_mask", "state"]
        names_out = ["kv_keys", "kv_values"]
    else:
        # The shapes are static, so zeros stand in for the prefix's cache.
        cache = (pol.config.num_vlm_layers, 1, 5, 64 + WIDTH + 1, 64)
        keys, values = torch.zeros(cache), torch.zeros(cache)
        prefix_mask = torch.cat(
            [
                torch.ones(1, keys.shape[3] - WIDTH - 1, dtype=torch.int64),
                mask,
                torch.ones(1, 1, dtype=torch.int64),
            ],
            dim=1,
        )
        mod = Denoise(pol).eval()
        args = (
            torch.zeros(1, 50, 32),
            torch.from_numpy(time_embedding(1.0)),
            keys,
            values,
            prefix_mask,
        )
        names_in = ["x_t", "time_emb", "kv_keys", "kv_values", "prefix_mask"]
        names_out = ["v_t"]
    # ONNX CumSum takes no bool input; torch gives int64 for a bool cumsum,
    # so casting first gives the same values.
    cumsum = torch.cumsum
    torch.cumsum = lambda x, dim, **kw: cumsum(
        x.to(torch.int64) if x.dtype == torch.bool else x, dim, **kw
    )
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{part}.onnx"
    rss("export start")
    t = time.time()
    with torch.no_grad():
        torch.onnx.export(
            mod,
            args,
            str(path),
            input_names=names_in,
            output_names=names_out,
            opset_version=OPSET,
            # ONNX Runtime folds constants when it loads the graph; folding
            # here doubles the export's memory.
            do_constant_folding=False,
            dynamo=False,
        )
    print(
        f"{part}: {path} {path.stat().st_size} bytes in {time.time() - t:.0f}s sha256 {sha256(path)}"
    )


def check(out: Path) -> None:
    import numpy as np
    import onnxruntime as ort
    import torch

    image, tokens, mask, state, noise = sample_inputs()
    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 4
    opts.inter_op_num_threads = 1
    opts.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
    opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    # The executor keeps all three sessions open; so does the check.
    vis = ort.InferenceSession(str(out / "vision.onnx"), opts, providers=["CPUExecutionProvider"])
    pre = ort.InferenceSession(str(out / "prefix.onnx"), opts, providers=["CPUExecutionProvider"])
    den = ort.InferenceSession(str(out / "denoise.onnx"), opts, providers=["CPUExecutionProvider"])
    rss("sessions")
    t = time.time()
    (emb,) = vis.run(None, {"image": image.numpy()})
    keys, values = pre.run(
        None,
        {
            "img_emb": emb,
            "lang_tokens": tokens.numpy(),
            "lang_mask": mask.numpy(),
            "state": state.numpy(),
        },
    )
    pmask = np.concatenate(
        [
            np.ones((1, keys.shape[3] - WIDTH - 1), np.int64),
            mask.numpy(),
            np.ones((1, 1), np.int64),
        ],
        axis=1,
    )
    x = noise.numpy()
    dt = np.float32(-1.0 / NUM_STEPS)
    for step in range(NUM_STEPS):
        tm = np.array([1.0 + step * (-1.0 / NUM_STEPS)], dtype=np.float32)
        (v,) = den.run(
            None,
            {
                "x_t": x,
                "time_emb": time_embedding(tm[0]),
                "kv_keys": keys,
                "kv_values": values,
                "prefix_mask": pmask,
            },
        )
        x = x + dt * v
    onnx_ms = (time.time() - t) * 1000
    rss("onnx chunk")
    del vis, pre, den
    pol = load_policy()
    t = time.time()
    with torch.no_grad():
        ref = pol.model.sample_actions(
            [image * 2.0 - 1.0],
            [torch.ones(1, dtype=torch.bool)],
            tokens,
            mask.to(torch.bool),
            state,
            noise=noise,
        ).numpy()
    torch_ms = (time.time() - t) * 1000
    d = np.abs(ref - x)
    print(
        json.dumps(
            {
                "max_abs": float(d.max()),
                "max_rel": float((d / (np.abs(ref) + 1e-6)).max()),
                "allclose_1e-3": bool(np.allclose(x, ref, atol=1e-3, rtol=1e-3)),
                "onnx_ms": round(onnx_ms),
                "torch_ms": round(torch_ms),
                "sha256": {p.name: sha256(p) for p in sorted(out.glob("*.onnx"))},
            },
            indent=1,
        )
    )


def assets(out: Path) -> None:
    import shutil

    out.mkdir(parents=True, exist_ok=True)
    pol = snapshot(POLICY_REPO, POLICY_REV)
    pre = pol / "policy_preprocessor_step_5_normalizer_processor.safetensors"
    post = pol / "policy_postprocessor_step_0_unnormalizer_processor.safetensors"
    if sha256(pre) != sha256(post):
        sys.exit("vla_export: the normalizer and unnormalizer stats differ")
    files = {
        "tokenizer.json": snapshot(BACKBONE_REPO, BACKBONE_REV) / "tokenizer.json",
        "stats.safetensors": pre,
    }
    for name, src in files.items():
        shutil.copyfile(src, out / name)
        print(f"{name}: sha256 {sha256(out / name)}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument(
        "--part", choices=["vision", "prefix", "denoise", "check", "assets"], required=True
    )
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    out = Path(args.out)
    if args.part == "check":
        check(out)
    elif args.part == "assets":
        assets(out)
    else:
        export(args.part, out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

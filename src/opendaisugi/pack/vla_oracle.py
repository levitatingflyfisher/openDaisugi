"""The SmolVLA reference policy, the oracle of ruling VL-R-2: the FP32
PyTorch policy run by lerobot, with lerobot's own steps around it (the
newline and tokenizer processors, resize_with_pad, the normalizer stats
lookup, and sample_actions with the caller's noise).

This file is the one definition. clients/vla_export.py and
clients/vla_cases.py import it in the export environment, and the
vla-ref pack carries a copy for its vla-chunk job. It imports torch,
numpy and lerobot only inside the functions, so it loads anywhere.

The noise is the same in every implementation: noise(seed) draws from
splitmix64 and turns each pair into two normal values by Box-Muller in
float64, then casts them to float32.
"""

from __future__ import annotations

import base64
import math
import zlib
from pathlib import Path

WIDTH = 48  # the checkpoint's tokenizer_max_length
IMAGE = 512  # the checkpoint's resize_imgs_with_padding
NUM_STEPS = 10
TIME_DIM = 720  # the action expert's hidden size
MIN_PERIOD = 0.004
MAX_PERIOD = 4.0
CHUNK, ACTION_DIM = 50, 32
STATS_FILE = "policy_preprocessor_step_5_normalizer_processor.safetensors"


class CaseError(ValueError):
    """An input case the vla-chunk job cannot run."""


def splitmix64(state: int):
    mask = (1 << 64) - 1
    while True:
        state = (state + 0x9E3779B97F4A7C15) & mask
        z = state
        z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & mask
        z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & mask
        yield z ^ (z >> 31)


def noise(seed: int):
    """The noise [1,50,32] for a seed, as float32."""
    import numpy as np

    g = splitmix64(seed)
    out: list[float] = []
    while len(out) < CHUNK * ACTION_DIM:
        u1 = ((next(g) >> 11) + 1) * 2.0**-53  # in (0, 1]
        u2 = (next(g) >> 11) * 2.0**-53  # in [0, 1)
        r = math.sqrt(-2.0 * math.log(u1))
        out += [r * math.cos(2 * math.pi * u2), r * math.sin(2 * math.pi * u2)]
    return np.array(out, dtype=np.float32).reshape(1, CHUNK, ACTION_DIM)


def load_policy(path: Path, part: str = "all", rss=lambda tag: None):
    """Load the policy and cast to FP32 the weights the part uses.

    The checkpoint keeps the VLM in bfloat16. Each part casts only the
    weights its graph traces, so that the export stays under the 4 GB cap:
    prefix keeps the action expert in bfloat16, denoise keeps the VLM in
    bfloat16. A weight the graph does not trace never reaches it. "all"
    (the oracle) casts every weight."""
    import gc

    import torch
    from lerobot.configs.policies import PreTrainedConfig
    from lerobot.policies.smolvla.modeling_smolvla import SmolVLAPolicy

    torch.manual_seed(0)
    cfg = PreTrainedConfig.from_pretrained(str(path))
    # The policy's own weights hold the VLM; do not fetch the backbone's.
    cfg.load_vlm_weights = False
    cfg.device = "cpu"
    pol = SmolVLAPolicy.from_pretrained(str(path), config=cfg).eval()
    rss("loaded")
    assert pol.model.vlm_with_expert.expert_hidden_size == TIME_DIM
    vlm = "model.vlm_with_expert.vlm.model."
    vision = (vlm + "vision_model.", vlm + "connector.")
    if part == "prefix":
        # The prefix graph takes the image embeddings as an input.
        vm = pol.model.vlm_with_expert.get_vlm_model()
        vm.vision_model = torch.nn.Identity()
        vm.connector = torch.nn.Identity()
        gc.collect()

    def wanted(name: str) -> bool:
        if part == "vision":
            return name.startswith(vision)
        if part == "prefix":
            return not name.startswith("model.vlm_with_expert.lm_expert.")
        if part == "denoise":
            return not name.startswith("model.vlm_with_expert.vlm.")
        return True

    with torch.no_grad():
        for name, t in list(pol.named_parameters()) + list(pol.named_buffers()):
            if t.is_floating_point() and t.dtype != torch.float32 and wanted(name):
                t.data = t.data.to(torch.float32)
    gc.collect()
    rss("cast")
    assert cfg.tokenizer_max_length == WIDTH and tuple(cfg.resize_imgs_with_padding) == (
        IMAGE,
        IMAGE,
    )
    assert cfg.num_steps == NUM_STEPS
    assert (cfg.min_period, cfg.max_period) == (MIN_PERIOD, MAX_PERIOD)
    return pol


def tokenizer(backbone: Path):
    """The task to (ids [1,48], mask [1,48] bool), by lerobot's processors."""
    from lerobot.processor.newline_task_processor import NewLineTaskProcessorStep
    from lerobot.processor.tokenizer_processor import TokenizerProcessorStep

    newline = NewLineTaskProcessorStep()
    step = TokenizerProcessorStep(tokenizer_name=str(backbone), max_length=WIDTH)

    def tok(task: str):
        enc = step._tokenize_text([newline.complementary_data({"task": task})["task"]])
        return enc["input_ids"], enc["attention_mask"].bool()

    return tok


def load_stats(policy: Path):
    """The checkpoint's normalizer stats, by lerobot's key."""
    from safetensors.torch import load_file

    return load_file(str(Path(policy) / STATS_FILE))


def chunk(pol, stats, tok, rgb, task: str, state: list[float], seed: int):
    """One action chunk [50,6] of the PyTorch policy."""
    import numpy as np
    import torch
    from lerobot.policies.common.vla_utils import pad_vector, resize_with_pad

    ids, mask = tok(task)
    img = torch.from_numpy(np.ascontiguousarray(rgb)).permute(2, 0, 1).float()[None] / 255.0
    img = resize_with_pad(img, IMAGE, IMAGE, pad_value=0) * 2.0 - 1.0
    st = torch.tensor([state], dtype=torch.float32)
    if "observation.state.mean" in stats and "observation.state.std" in stats:
        st = (st - stats["observation.state.mean"]) / (stats["observation.state.std"] + 1e-8)
    st = pad_vector(st, 32)
    with torch.no_grad():
        out = pol.model.sample_actions(
            [img],
            [torch.ones(1, dtype=torch.bool)],
            ids,
            mask,
            st,
            noise=torch.from_numpy(noise(seed)),
        )
    act = out[0, :, :6]
    if "action.mean" in stats and "action.std" in stats:
        act = act * stats["action.std"] + stats["action.mean"]
    return act.double().numpy()


def decode_image(img: dict):
    """A uint8 RGB image [h,w,3] from {"height", "width", "rgb_zlib"}: the
    rows top first, zlib then base64, as clients/fixtures/vla/loop.json
    stores them."""
    import numpy as np

    try:
        h, w = int(img["height"]), int(img["width"])
        raw = zlib.decompress(base64.b64decode(img["rgb_zlib"]))
    except (KeyError, TypeError, ValueError, zlib.error) as e:
        raise CaseError(f"the image is not readable: {type(e).__name__}") from None
    if len(raw) != h * w * 3:
        raise CaseError(f"the image has {len(raw)} bytes, not {h}x{w}x3")
    return np.frombuffer(raw, dtype=np.uint8).reshape(h, w, 3)


def chunk_from_case(case: dict, emit=lambda text: None) -> dict:
    """The vla-chunk job: {"policy", "backbone", "task", "state", "seed",
    "image"} to {"actions": [[...] x6] x50}. policy and backbone are the
    pinned snapshot directories (ruling VL-R-2)."""
    for k in ("policy", "backbone", "task", "state", "seed", "image"):
        if k not in case:
            raise CaseError(f"the input has no {k}")
    rgb = decode_image(case["image"])
    emit("loading the policy")
    pol = load_policy(Path(case["policy"]))
    emit("running one chunk")
    act = chunk(
        pol,
        load_stats(Path(case["policy"])),
        tokenizer(Path(case["backbone"])),
        rgb,
        case["task"],
        [float(v) for v in case["state"]],
        int(case["seed"]),
    )
    return {"actions": act.tolist()}

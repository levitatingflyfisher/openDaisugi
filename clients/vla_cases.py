"""SmolVLA golden cases: the tokenizer and the closed loop, run through the
PyTorch oracle in the export environment (clients/vla/requirements.txt).

    HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_cases.py --part tiny
    HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_cases.py --part tokens
    HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_cases.py --part process
    CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_cases.py --part chunk
    MUJOCO_GL=egl CUDA_VISIBLE_DEVICES="" HF_HUB_OFFLINE=1 VENV/bin/python clients/vla_cases.py --part loop

Or, with no export environment, the chunk through the vla-ref pack
(`daisugi pack install vla-ref`), which runs the same oracle module:

    HF_HUB_OFFLINE=1 uv run --no-sync python clients/vla_cases.py --part chunk --pack DAISUGI

- tiny: write clients/fixtures/vla/tiny.onnx, a graph of a few bytes the
  ONNX Runtime wrappers test against (y = x * 2 + k, x float32 [2,3], k
  int64 [2,3]).
- tokens: write clients/fixtures/vla/tokens.json, the token ids and mask
  lerobot's processors give for each instruction of INSTRUCTIONS (the
  newline step, then the backbone's tokenizer at width 48).
- process: write clients/fixtures/vla/process.json, the steps outside the
  graphs computed by lerobot and torch: the image resized and padded to
  512x512 (sampled at fixed points), the time embedding of each Euler
  step, and the noise of two seeds.

- chunk: write clients/fixtures/vla/chunk.json, one action chunk [50,6]
  of the FP32 PyTorch policy (the oracle of ruling VL-R-2) for a fixed
  image, task, state and noise seed.

- loop: write clients/fixtures/vla/loop.json, the closed loop in the
  pick-place scene (tests/fixtures/mjcf/two_joint_arm.xml): LOOP["steps"]
  runs of the oracle's VLA executor, each rendering the camera, running one
  chunk and applying its first max_actions rows. Each run records its
  inputs (the state, and the camera image as zlib then base64, so a port
  can replay them), the chunk, the image's SHA-256, the qpos after the
  run and the chunk's time.
  The ports answer the same case with their vla-probe
  (clients/vla_compare.py).

The noise is the same in every implementation: noise(seed) draws from
splitmix64 and turns each pair into two normal values by Box-Muller in
float64, then casts them to float32.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

# huggingface_hub reads this at import: unless set, it fetches its agent
# list from the Hub and names the agent in its User-Agent. A set value is kept.
os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")

REPO = Path(__file__).resolve().parent.parent
FIXTURES = REPO / "clients" / "fixtures" / "vla"
sys.path.insert(0, str(REPO / "clients"))
sys.path.insert(0, str(REPO / "src" / "opendaisugi" / "pack"))

import vla_oracle  # noqa: E402 - the one oracle, also in the vla-ref pack

# Thirty instructions: plain robot tasks, then the edges of the tokenizer
# (digits, contractions, runs of spaces, punctuation, non-ASCII letters and
# numbers, an added token written in the text, a trailing newline, and a
# text longer than 48 tokens, which is truncated).
INSTRUCTIONS = [
    "pick up the block",
    "Pick up the red cube and place it in the bin.",
    "move the arm to the left",
    "grasp the cup",
    "put the block on the plate",
    "open the gripper",
    "close the gripper slowly",
    "rotate joint 2 by 45 degrees",
    "move 3 cm to the right, then 10cm up",
    "stack the blue block on top of the green one",
    "it's the robot's turn; don't drop it",
    "they're here, we've got it, I'm done, you'll see, he'd go",
    "push   the   block    forward",
    "  leading and trailing spaces  ",
    "tabs\tand\nnewlines\r\nhere",
    "already ends with a newline\n",
    "",
    "!!!???...,,,;;;",
    "use the 1st, 2nd and 3rd slots: 123456789",
    "déplace le bloc à gauche",
    "bewege den Würfel über die Kante",
    "把方块放到盘子上",
    "ブロックを持ち上げて",
    "Ⅻ ½ ٣ numbers of other kinds",
    "emoji 🤖 picks 🧊 up",
    "pick up <|im_start|> the block",
    "the <image> token and <end_of_utterance> marker",
    "CamelCaseWordsAndALLCAPS with snake_case_names",
    "e-mail: robot@example.com, url https://example.com/a?b=c",
    "pick up the block " * 12,
]


CHUNK, ACTION_DIM = vla_oracle.CHUNK, vla_oracle.ACTION_DIM
splitmix64 = vla_oracle.splitmix64
noise = vla_oracle.noise


def test_image(h: int, w: int):
    """A uint8 RGB image [h,w,3] with a pattern in every channel."""
    import numpy as np

    y, x, c = np.meshgrid(np.arange(h), np.arange(w), np.arange(3), indexing="ij")
    return ((x * 7 + y * 13 + c * 29 + x * y) % 256).astype(np.uint8)


# Where the resized image is sampled: (channel, row, column).
SAMPLES = [((i * 7) % 3, (i * 97) % 512, (i * 211) % 512) for i in range(300)]


def tiny() -> None:
    import onnx
    from onnx import TensorProto, helper

    x = helper.make_tensor_value_info("x", TensorProto.FLOAT, [2, 3])
    k = helper.make_tensor_value_info("k", TensorProto.INT64, [2, 3])
    y = helper.make_tensor_value_info("y", TensorProto.FLOAT, [2, 3])
    two = helper.make_tensor("two", TensorProto.FLOAT, [], [2.0])
    nodes = [
        helper.make_node("Cast", ["k"], ["kf"], to=TensorProto.FLOAT),
        helper.make_node("Mul", ["x", "two"], ["x2"]),
        helper.make_node("Add", ["x2", "kf"], ["y"]),
    ]
    graph = helper.make_graph(nodes, "tiny", [x, k], [y], [two])
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 17)])
    model.ir_version = 8
    model.producer_name = "vla_cases"
    onnx.checker.check_model(model)
    FIXTURES.mkdir(parents=True, exist_ok=True)
    onnx.save(model, str(FIXTURES / "tiny.onnx"))
    print(f"tiny: {(FIXTURES / 'tiny.onnx').stat().st_size} bytes")


def tokens() -> None:
    from lerobot.processor.newline_task_processor import NewLineTaskProcessorStep
    from lerobot.processor.tokenizer_processor import TokenizerProcessorStep
    from vla_export import BACKBONE_REPO, BACKBONE_REV, WIDTH, snapshot

    path = snapshot(BACKBONE_REPO, BACKBONE_REV)
    newline = NewLineTaskProcessorStep()
    tok = TokenizerProcessorStep(tokenizer_name=str(path), max_length=WIDTH)
    cases = []
    for text in INSTRUCTIONS:
        task = newline.complementary_data({"task": text})["task"]
        enc = tok._tokenize_text([task])
        cases.append(
            {
                "text": text,
                "ids": enc["input_ids"][0].tolist(),
                "mask": enc["attention_mask"][0].tolist(),
            }
        )
    FIXTURES.mkdir(parents=True, exist_ok=True)
    out = FIXTURES / "tokens.json"
    out.write_text(
        json.dumps({"width": WIDTH, "cases": cases}, ensure_ascii=False, indent=1) + "\n"
    )
    print(f"tokens: {len(cases)} cases, longest {max(sum(c['mask']) for c in cases)} tokens")


def process() -> None:
    import numpy as np
    import torch
    from lerobot.policies.common.vla_utils import create_sinusoidal_pos_embedding, resize_with_pad
    from vla_export import IMAGE, MAX_PERIOD, MIN_PERIOD, NUM_STEPS, TIME_DIM

    images = []
    for h, w in [(23, 37), (37, 23), (240, 320), (512, 512)]:
        rgb = torch.from_numpy(test_image(h, w)).permute(2, 0, 1).float()[None] / 255.0
        out = resize_with_pad(rgb, IMAGE, IMAGE, pad_value=0)[0]
        images.append(
            {
                "height": h,
                "width": w,
                "sum": float(out.double().sum()),
                "samples": [float(out[c, y, x]) for c, y, x in SAMPLES],
            }
        )
    times = []
    for step in range(NUM_STEPS):
        t = torch.tensor(1.0 + step * (-1.0 / NUM_STEPS), dtype=torch.float32)
        emb = create_sinusoidal_pos_embedding(
            t[None], TIME_DIM, MIN_PERIOD, MAX_PERIOD, device=torch.device("cpu")
        )
        times.append({"time": float(t), "embedding": [float(v) for v in emb.float()[0]]})
    noises = []
    for seed in [0, 7]:
        n = noise(seed)
        noises.append(
            {
                "seed": seed,
                "sum": float(n.astype(np.float64).sum()),
                "head": [float(v) for v in n.ravel()[:64]],
            }
        )
    out = FIXTURES / "process.json"
    out.write_text(
        json.dumps({"samples": SAMPLES, "images": images, "times": times, "noise": noises}) + "\n"
    )
    print(f"process: {len(images)} images, {len(times)} times, {len(noises)} noises")


def oracle_chunk(pol, stats, rgb, task: str, state: list[float], seed: int):
    """One action chunk [50,6] of the PyTorch policy (vla_oracle.chunk)."""
    return vla_oracle.chunk(pol, stats, oracle_tokenizer(), rgb, task, state, seed)


_TOKENIZER = None


def oracle_tokenizer():
    """The task to (ids [1,48], mask [1,48] bool), by lerobot's processors."""
    global _TOKENIZER
    if _TOKENIZER is None:
        from vla_export import BACKBONE_REPO, BACKBONE_REV, snapshot

        _TOKENIZER = vla_oracle.tokenizer(snapshot(BACKBONE_REPO, BACKBONE_REV))
    return _TOKENIZER


def oracle_stats():
    """The checkpoint's normalizer stats, by lerobot's key."""
    from vla_export import POLICY_REPO, POLICY_REV, snapshot

    return vla_oracle.load_stats(snapshot(POLICY_REPO, POLICY_REV))


CHUNK_CASE = {
    "image": [240, 320],
    "task": "pick up the block",
    "state": [0.1, -0.2, 0.01, 0.0, 0.0, 0.0],
    "seed": 0,
}


def chunk() -> None:
    import time

    from vla_export import load_policy

    pol = load_policy()
    c = CHUNK_CASE
    t = time.time()
    act = oracle_chunk(
        pol, oracle_stats(), test_image(*c["image"]), c["task"], c["state"], c["seed"]
    )
    ms = (time.time() - t) * 1000
    out = FIXTURES / "chunk.json"
    out.write_text(json.dumps({**c, "actions": act.tolist()}) + "\n")
    print(f"chunk: {act.shape} in {ms:.0f} ms, first {act[0].round(4).tolist()}")


def chunk_via_pack(binary: str, out: Path) -> None:
    """--part chunk through the vla-ref pack's vla-chunk job: the same
    case, the image as zlib then base64, the snapshot directories named."""
    import base64
    import subprocess
    import tempfile
    import zlib

    from vla_export import BACKBONE_REPO, BACKBONE_REV, POLICY_REPO, POLICY_REV, snapshot

    c = CHUNK_CASE
    rgb = test_image(*c["image"])
    case = {
        "policy": str(snapshot(POLICY_REPO, POLICY_REV)),
        "backbone": str(snapshot(BACKBONE_REPO, BACKBONE_REV)),
        "task": c["task"],
        "state": c["state"],
        "seed": c["seed"],
        "image": {
            "height": c["image"][0],
            "width": c["image"][1],
            "rgb_zlib": base64.b64encode(zlib.compress(rgb.tobytes(), 9)).decode(),
        },
    }
    with tempfile.TemporaryDirectory() as d:
        inp = Path(d) / "case.json"
        inp.write_text(json.dumps(case))
        r = subprocess.run(
            [binary, "pack", "run", "vla-ref", "vla-chunk", "--input", str(inp)],
            capture_output=True,
            text=True,
        )
    if r.returncode != 0:
        last = (r.stderr.strip().splitlines() or ["no reason given"])[-1]
        sys.exit(f"vla_cases: the vla-ref pack failed: {last}")
    act = json.loads(r.stdout)["actions"]
    out.write_text(json.dumps({**c, "actions": act}) + "\n")
    print(f"chunk (pack): {len(act)}x{len(act[0])}, first {[round(v, 4) for v in act[0]]}")


LOOP = {
    "mjcf": "tests/fixtures/mjcf/two_joint_arm.xml",
    "task": "pick up the block",
    "seed": 11,
    "steps": 5,
    "max_actions": 25,
    "threads": 4,
}


def loop() -> None:
    """The oracle's closed loop. It follows VLAExecutorBase.run and
    TransformersVLAExecutor: the observation is the qpos; the image comes
    from a new Renderer(model, 240, 320) on the default camera, as
    _capture_image makes one per call; the first max_actions rows of the
    chunk go to the MJCF's joints in order, each followed by one mj_step.
    The project's own executor cannot run here (its environment has no
    lerobot, and this one no pydantic), so the loop is written out."""
    import base64
    import hashlib
    import time
    import zlib

    import mujoco
    import torch
    from vla_export import load_policy

    torch.set_num_threads(LOOP["threads"])
    pol = load_policy()
    stats = oracle_stats()
    m = mujoco.MjModel.from_xml_path(str(REPO / LOOP["mjcf"]))
    d = mujoco.MjData(m)
    names = [mujoco.mj_id2name(m, mujoco.mjtObj.mjOBJ_JOINT, j) for j in range(m.njnt)]
    keys = [n for n in names if n][:6]
    actuator = {}
    for name in keys:
        jid = mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_JOINT, name)
        for aid in range(m.nu):
            if m.actuator_trnid[aid, 0] == jid:
                actuator[name] = aid
                break
    records = []
    for k in range(LOOP["steps"]):
        qpos = [float(v) for v in d.qpos]
        r = mujoco.Renderer(m, height=240, width=320)
        r.update_scene(d)
        rgb = r.render().copy()
        r.close()
        t = time.time()
        act = oracle_chunk(pol, stats, rgb, LOOP["task"], qpos, LOOP["seed"] + k)
        ms = (time.time() - t) * 1000
        for row in act[: LOOP["max_actions"]]:
            for i, name in enumerate(keys):
                if name in actuator:
                    d.ctrl[actuator[name]] = float(row[i])
            mujoco.mj_step(m, d)
        records.append(
            {
                "state": qpos,
                "image_zlib": base64.b64encode(zlib.compress(rgb.tobytes(), 9)).decode(),
                "image_sha256": hashlib.sha256(rgb.tobytes()).hexdigest(),
                "image_mean": float(rgb.mean()),
                "chunk": act.tolist(),
                "qpos": [float(v) for v in d.qpos],
                "ms": round(ms),
            }
        )
        print(
            f"loop {k}: {ms:.0f} ms, image mean {rgb.mean():.2f}, qpos {d.qpos.round(4).tolist()}"
        )
    out = FIXTURES / "loop.json"
    out.write_text(json.dumps({**LOOP, "records": records}) + "\n")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--part", choices=["tiny", "tokens", "process", "chunk", "loop"], required=True)
    ap.add_argument("--pack", metavar="DAISUGI", help="run --part chunk in the vla-ref pack")
    ap.add_argument("--out", help="with --pack, where the chunk goes (default: the fixture)")
    args = ap.parse_args()
    os.environ.setdefault("HF_HUB_OFFLINE", "1")
    if args.pack:
        if args.part != "chunk":
            ap.error("--pack runs --part chunk only")
        chunk_via_pack(args.pack, Path(args.out) if args.out else FIXTURES / "chunk.json")
        return 0
    {"tiny": tiny, "tokens": tokens, "process": process, "chunk": chunk, "loop": loop}[args.part]()
    sys.stdout.flush()
    # MuJoCo's EGL context raises in its finalizer at interpreter exit.
    os._exit(0)


if __name__ == "__main__":
    raise SystemExit(main())

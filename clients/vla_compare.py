"""Compare the ports' SmolVLA executors with the oracle on the closed loop.

    python clients/vla_compare.py --probe PATH/vla-probe [--probe PATH2] --model DIR

The oracle's run is recorded in clients/fixtures/vla/loop.json
(clients/vla_cases.py --part loop, in the export environment), with each
run's inputs: the state and the camera image. Each probe reads that file
and the model directory and runs twice in the repository root:

- replay (--replay): each recorded run's own state and image, with the
  same noise seed, fed to the policy. Every value of every chunk must be
  within VL-R-6's tolerance (1e-3) of the oracle's.
- free: the closed loop itself, the same scene, task, seeds, step count
  and max_actions. Run 0 (chunk and qpos after it) must be within the
  tolerance. Later runs are reported, not held to it (ruling VL-R-7): a
  qpos that differs in the 7th digit moves a few edge pixels of the next
  image, and the loop carries that on. The report gives, per run, the
  chunk and qpos differences and the count of image bytes that differ.
  With two probes, the ports are also compared with each other, and must
  agree within the tolerance on every run.

Then it prints the time per chunk of the oracle (as recorded) and of each
probe (the replay runs), and exits 1 when a held value is out of
tolerance. The probes link ONNX Runtime and MuJoCo dynamically:
LD_LIBRARY_PATH must name the lib directories of the prefixes
`clients/go/scripts/native.sh --onnxruntime` and `--mujoco` install.
"""

from __future__ import annotations

import argparse
import base64
import json
import subprocess
import sys
import zlib
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
LOOP = REPO / "clients" / "fixtures" / "vla" / "loop.json"
TOLERANCE = 1e-3  # ruling VL-R-6


def largest(a, b) -> float:
    if isinstance(a, list):
        if not isinstance(b, list) or len(a) != len(b):
            return float("inf")
        return max((largest(x, y) for x, y in zip(a, b, strict=True)), default=0.0)
    return abs(float(a) - float(b))


def image(rec: dict) -> bytes:
    return zlib.decompress(base64.b64decode(rec["image_zlib"]))


def differing(a: bytes, b: bytes) -> int:
    if len(a) != len(b):
        return max(len(a), len(b))
    return sum(1 for x, y in zip(a, b, strict=True) if x != y)


def run_probe(probe: str, model: str, replay: bool) -> list[dict]:
    cmd = [probe, *(["--replay"] if replay else []), str(LOOP), model]
    p = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, timeout=3600)
    if p.returncode != 0:
        raise RuntimeError(f"{probe}: exit {p.returncode}: {p.stderr.strip()[-500:]}")
    return json.loads(p.stdout)["records"]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--probe", action="append", required=True)
    ap.add_argument("--model", required=True)
    args = ap.parse_args()
    oracle = json.loads(LOOP.read_text())["records"]
    ok = True
    times = {"oracle": [r["ms"] for r in oracle]}
    free_runs = {}
    for probe in args.probe:
        name = Path(probe).name
        try:
            replayed = run_probe(probe, args.model, replay=True)
            free = run_probe(probe, args.model, replay=False)
        except (RuntimeError, OSError, subprocess.TimeoutExpired) as e:
            print(f"FAIL {name}: {e}")
            ok = False
            continue
        if len(replayed) != len(oracle) or len(free) != len(oracle):
            print(
                f"FAIL {name}: {len(replayed)} and {len(free)} runs, the oracle has {len(oracle)}"
            )
            ok = False
            continue
        times[name] = [r["ms"] for r in replayed]
        free_runs[name] = free
        for k, (g, o) in enumerate(zip(replayed, oracle, strict=True)):
            dc = largest(g["chunk"], o["chunk"])
            ok = ok and dc <= TOLERANCE
            print(f"{'ok  ' if dc <= TOLERANCE else 'FAIL'} {name} replay {k}: chunk {dc:.2e}")
        for k, (g, o) in enumerate(zip(free, oracle, strict=True)):
            dc, dq = largest(g["chunk"], o["chunk"]), largest(g["qpos"], o["qpos"])
            pixels = differing(image(g), image(o))
            if k == 0:
                good = dc <= TOLERANCE and dq <= TOLERANCE and pixels == 0
                ok = ok and good
                tag = "ok  " if good else "FAIL"
            else:
                tag = "note"
            print(
                f"{tag} {name} free {k}: chunk {dc:.2e}, qpos {dq:.2e}, "
                f"image bytes differing {pixels}"
            )
    names = list(free_runs)
    for i, a in enumerate(names):
        for b in names[i + 1 :]:
            for k, (x, y) in enumerate(zip(free_runs[a], free_runs[b], strict=True)):
                dc, dq = largest(x["chunk"], y["chunk"]), largest(x["qpos"], y["qpos"])
                pixels = differing(image(x), image(y))
                good = dc <= TOLERANCE and dq <= TOLERANCE
                ok = ok and good
                print(
                    f"{'ok  ' if good else 'FAIL'} {a} vs {b} free {k}: chunk {dc:.2e}, "
                    f"qpos {dq:.2e}, image bytes differing {pixels}"
                )
    for name, ms in times.items():
        print(
            f"time per chunk, {name}: mean {sum(ms) / len(ms):.0f} ms ({', '.join(map(str, ms))})"
        )
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())

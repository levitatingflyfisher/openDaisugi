"""Compare the ports' robotics executors with the oracle's on the robotics cases.

    uv run --no-sync python clients/robotics_compare.py --probe PATH/robot-probe [--probe PATH2] [--oracle]

Each probe reads clients/fixtures/robotics/cases.jsonl and the repository
root, and writes one result line per case. The compare drops what differs
on every run (ids, times, durations), checks each receipt's evidence_hash
against the oracle's compute_evidence_hash of that receipt's own evidence
(ruling RB-R-5), names the checkout <REPO>, and then wants each result to
equal the case's recorded expectation as canonical JSON: every float to
the last bit (ruling RB-R-3). The render cases have no expectation (ruling
RB-R-4): every probe must give the same image, and an image that is one
color fails. With --oracle the expectations are made again first (this
needs the mujoco wheel on the path and MUJOCO_GL=disable), and a case
whose expectation changed is reported as stale.

The probes link libmujoco.so dynamically: LD_LIBRARY_PATH must name the
lib directory of the prefix `clients/go/scripts/native.sh --mujoco`
installs, unless --mujoco-lib is given.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import robotics_cases  # noqa: E402 - sibling module, run as a script


def canonical(v: Any) -> str:
    return json.dumps(v, sort_keys=True, ensure_ascii=True)


def normalize(case: dict[str, Any], r: Any) -> Any:
    """A probe's result as the oracle's expectation is recorded."""
    if not isinstance(r, dict):
        return r
    r = dict(r)
    if case["kind"] == "run":
        if "session" in r:
            r["session"] = robotics_cases.scrub(r["session"])
        if "receipts" in r:
            r["receipts"] = [robotics_cases.receipt_view(x) for x in r["receipts"]]
    return robotics_cases.unroot(r)


def run_probe(probe: str, cases_path: Path, env: dict[str, str]) -> dict[str, Any] | None:
    proc = subprocess.run(
        [str(Path(probe).resolve()), str(cases_path), str(robotics_cases.REPO)],
        capture_output=True,
        timeout=600,
        check=False,
        env=env,
    )
    if proc.returncode != 0:
        print(proc.stderr.decode("utf-8", "replace")[-4000:])
        print(f"the probe {probe} failed (exit {proc.returncode})")
        return None
    got = {}
    for ln in proc.stdout.decode("utf-8").splitlines():
        r = json.loads(ln)
        got[r["id"]] = r["result"]
    return got


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--probe", action="append", required=True)
    ap.add_argument("--oracle", action="store_true", help="make the expectations again first")
    ap.add_argument("--mujoco-lib", help="the directory that holds libmujoco.so")
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()
    sys.path.insert(0, str(robotics_cases.REPO / "src"))
    path = robotics_cases.FIXTURE_DIR / "cases.jsonl"
    cases = [json.loads(ln) for ln in path.read_text(encoding="utf-8").splitlines() if ln.strip()]
    stale = 0
    if args.oracle:
        for c in cases:
            if "expect" in c and canonical(
                robotics_cases.unroot(robotics_cases.run_case(c))
            ) != canonical(c["expect"]):
                stale += 1
                print(f"STALE: {c['name']}")
    env = dict(os.environ)
    if args.mujoco_lib:
        env["LD_LIBRARY_PATH"] = args.mujoco_lib + (
            ":" + env["LD_LIBRARY_PATH"] if env.get("LD_LIBRARY_PATH") else ""
        )
    outs = []
    for p in args.probe:
        got = run_probe(p, path, env)
        if got is None:
            return 1
        outs.append((p, got))
    width = None if args.verbose else 1500
    agree = disagree = 0
    for c in cases:
        results = [(p, normalize(c, got.get(c["id"]))) for p, got in outs]
        bad: list[str] = []
        if "expect" in c:
            for p, r in results:
                if canonical(r) != canonical(c["expect"]):
                    bad.append(
                        f"    oracle: {canonical(c['expect'])[:width]}\n    {p}: {canonical(r)[:width]}"
                    )
        else:
            first = canonical(results[0][1])
            for p, r in results[1:]:
                if canonical(r) != first:
                    bad.append(
                        f"    {results[0][0]}: {first[:width]}\n    {p}: {canonical(r)[:width]}"
                    )
            want = "error" if c["name"] in ERROR_CASES else "sha256"
            for p, r in results:
                if not isinstance(r, dict) or want not in r:
                    bad.append(f"    {p}: wanted {want}: {canonical(r)[:width]}")
                elif want == "sha256" and r.get("colors", 0) < 2:
                    bad.append(f"    {p}: the image is one color")
        if bad:
            disagree += 1
            print(f"DISAGREE: {c['name']}")
            print("\n".join(bad))
        else:
            agree += 1
    print(f"\nrobotics: {len(cases)} cases: agree={agree}, disagree={disagree}, stale={stale}")
    return 1 if disagree or stale else 0


# Render cases whose answer is an error, the same from every probe; every
# other render case must draw an image.
ERROR_CASES = {"render too large", "render unknown camera"}


if __name__ == "__main__":
    raise SystemExit(main())

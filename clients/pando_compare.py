"""Replay the `daisugi pando` cases through a binary and compare.

    uv run --no-sync python clients/pando_compare.py --binary PATH/daisugi [--oracle PATH/daisugi-go] [--python PATH/daisugi-go] [--only NAME]

clients/fixtures/pando/cli/cases.jsonl is written by pando_cases.py from the
Go binary. Each case runs in a fresh scratch HOME and data home; the exit
code, stdout, stderr of every step and the tree after must equal the
recorded ones. With --oracle the Go binary is also run live, so a stale
fixture shows up.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import pando_cases  # noqa: E402 - sibling module, run as a script


def diff(want: dict, got: dict) -> list[str]:
    out = []
    for i, (w, g) in enumerate(zip(want["steps"], got["steps"], strict=False)):
        for k in ("code", "out", "err"):
            if w[k] != g[k]:
                out.append(f"  step {i} {k}:\n    want: {w[k]!r:.600}\n    got:  {g[k]!r:.600}")
    if len(want["steps"]) != len(got["steps"]):
        out.append("  the number of steps differs")
    for k in sorted(set(want["tree"]) | set(got["tree"])):
        if want["tree"].get(k) != got["tree"].get(k):
            out.append(
                f"  tree {k}:\n    want: {want['tree'].get(k)!r:.600}\n    got:  {got['tree'].get(k)!r:.600}"
            )
    return out


def python_reads(binary: str):
    """A check for pando_cases.run_case(after=...): the Python reader opens
    every store the case left (and every child store), and its zoom of every
    root and of every message equals what the Go binary prints. A list of
    problems; empty means Python reads it all."""
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
    import subprocess

    from opendaisugi.pando.store import PandoStore, list_stores, store_dir

    def go_zoom(data: Path, store: str, child: str, id_: int, n: int) -> tuple[int, str]:
        argv = [str(Path(binary).resolve()), "pando", "--store", store, "--data-dir", str(data)]
        if child:
            argv += ["--child", child]
        r = subprocess.run(
            [*argv, "zoom", str(id_), str(n)], capture_output=True, timeout=60, check=False
        )
        return r.returncode, r.stdout.decode("utf-8", "replace")

    def check(data: Path) -> list[str]:
        problems = []
        for store in list_stores(data):
            d = store_dir(data, store)
            kids = (
                sorted(p.name for p in (d / "children").iterdir() if (p / "main").is_dir())
                if (d / "children").is_dir()
                else []
            )
            for child in ["", *kids]:
                where = f"{store}/{child}" if child else store
                try:
                    py = PandoStore.open(d / "children" / child if child else d)
                    probes = [(i, 1) for i in range(py.count)] + [
                        (i << lvl, 1 << lvl) for lvl, i in py.roots()
                    ]
                    for i, n in probes:
                        code, want = go_zoom(data, store, child, i, n)
                        got = py.zoom(i, n)
                        if code != 0 or got.rstrip("\n") != want.rstrip("\n"):
                            problems.append(f"{where} zoom {i} {n}: go exit {code}; python differs")
                            break
                except Exception as e:  # noqa: BLE001 - any failure to read is the finding
                    problems.append(f"{where}: python could not read it: {e}")
        return problems

    return check


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", help="the Go daisugi, to make the expectations again")
    ap.add_argument("--only")
    ap.add_argument(
        "--python",
        metavar="GO_DAISUGI",
        help="also check that the Python reader opens every store each case left, with the Go binary as the reference",
    )
    ap.add_argument(
        "--skip-go-only",
        action="store_true",
        help="skip cases marked go_only (for the frozen Rust port)",
    )
    args = ap.parse_args()
    path = pando_cases.FIXTURE_DIR / "cases.jsonl"
    cases = [json.loads(ln) for ln in path.read_text(encoding="utf-8").splitlines() if ln.strip()]
    agree = disagree = stale = unread = 0
    check = python_reads(args.python) if args.python else None
    for c in cases:
        if args.skip_go_only and c.get("go_only"):
            continue
        if args.only and c["name"] != args.only:
            continue
        if args.oracle:
            if diff(c["expect"], pando_cases.run_case(args.oracle, c)):
                stale += 1
                print(f"STALE: {c['name']}")
        got = pando_cases.run_case(args.binary, c, after=check)
        if check and got.get("after"):
            unread += 1
            print(f"PYTHON: {c['name']}")
            print("\n".join("  " + p for p in got["after"]))
        d = diff(c["expect"], got)
        if not d:
            agree += 1
            continue
        disagree += 1
        print(f"DISAGREE: {c['name']}")
        print("\n".join(d))
    note = f", python-unread={unread}" if check else ""
    print(f"\npando: {len(cases)} cases: agree={agree}, disagree={disagree}, stale={stale}{note}")
    return 1 if disagree or stale or unread else 0


if __name__ == "__main__":
    sys.exit(main())

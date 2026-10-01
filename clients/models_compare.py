"""Run the model-catalog cases through a daisugi binary (Go or Rust) and
compare them to the oracle.

    uv run --no-sync python clients/models_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--verbose]

For every case in clients/fixtures/models (clients/models_cases.py writes
them from the oracle) it checks the exit code, stdout, stderr, the tree
after and the targets asked of the fake Hugging Face API, byte for byte,
as clients/garden_compare.py checks its own.

--oracle also reruns the Python side, so a stale fixture shows up.
--max-refused N fails the run when more than N cases are refused or not
ported.
"""

from __future__ import annotations

import argparse
import collections
import json
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from cli_cases import as_daisugi  # noqa: E402
from fixture_paths import leaks  # noqa: E402
from garden_compare import before_tree, compare  # noqa: E402
from models_cases import FIXTURE_DIR, SCRATCH, cmd_for, run_case  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff  # noqa: E402


def python_finds_pull(case: dict, work: Path) -> str:
    """ "" when hf_hub_download(local_files_only=True) finds, in the cache the
    binary wrote, the file it reported; else what went wrong."""
    import subprocess

    out = case["expect"]["stdout"]
    if out.startswith("{"):
        body = json.loads(out)
        repo, name, rev = body["repo_id"], body["filename"], body["revision"]
    else:
        fields = dict(ln.split(":", 1) for ln in out.splitlines() if ":" in ln)
        repo, name = fields["repo"].strip(), fields["file"].strip()
        rev = fields["revision"].split()[0]
    home = work / "home"
    code = (
        "import sys\n"
        "from huggingface_hub import hf_hub_download\n"
        "print(hf_hub_download(sys.argv[1], sys.argv[2], revision=sys.argv[3], local_files_only=True))\n"
    )
    env = {
        "HOME": str(home),
        "HF_HOME": str(home / "hf"),
        "HF_HUB_OFFLINE": "1",
        "PATH": "/usr/bin:/bin",
    }
    p = subprocess.run(
        [sys.executable, "-c", code, repo, name, rev],
        capture_output=True,
        text=True,
        env=env,
        check=False,
    )
    if p.returncode != 0:
        return "Python does not find the pulled file: " + p.stderr.strip().splitlines()[-1]
    path = Path(p.stdout.strip())
    if not path.is_file() or path.read_bytes() != b"GGUFdata\n":
        return f"Python finds {path}, which does not hold the pulled bytes"
    return ""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--max-refused", type=int, default=None)
    args = ap.parse_args()
    SCRATCH.mkdir(parents=True, exist_ok=True)
    binary = as_daisugi(args.binary, SCRATCH)
    cases = [
        json.loads(ln)
        for ln in (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
        if ln.strip()
    ]
    classes: collections.Counter[str] = collections.Counter()
    guarded: collections.Counter[str] = collections.Counter()
    stale = 0
    for i, case in enumerate(cases):
        if args.only and args.only not in case["name"]:
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        before = before_tree(case, SCRATCH / "cmp" / f"{i:04d}b")
        got = run_case(case, cmd_for(case, binary), work)
        cls, problems = compare(case, got, before)
        if cls == "agree" and got["hf"] != case["expect"]["hf"]:
            cls, problems = "disagree", ["hf: " + d for d in diff(case["expect"]["hf"], got["hf"])]
        if cls == "agree" and "--pull" in case["argv"] and case["expect"]["exit"] == 0:
            # The guard: Python finds the file the binary pulled, offline.
            found = python_finds_pull(case, work)
            if found:
                cls, problems = "disagree", [found]
                guarded["no"] += 1
            else:
                guarded["yes"] += 1
        if args.oracle:
            live = run_case(case, cmd_for(case, None), SCRATCH / "cmp" / f"{i:04d}py")
            if live != case["expect"]:
                stale += 1
                print(f"STALE fixture: {case['name']}: {diff(case['expect'], live)[:3]}")
        classes[cls] += 1
        if cls == "disagree":
            print(f"DISAGREE: {case['name']}")
            for p in problems[:12]:
                print(f"    {p}")
        elif cls in ("refused", "not-ported") and args.verbose:
            print(f"{cls}: {case['name']}")
        shutil.rmtree(work, ignore_errors=True)
    shutil.rmtree(SCRATCH / "cmp", ignore_errors=True)
    print(
        f"\nmodels: {sum(classes.values())} cases: "
        + ", ".join(f"{k}={v}" for k, v in sorted(classes.items()))
    )
    print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    if guarded:
        print(
            f"guard: Python found {guarded['yes']} of {sum(guarded.values())} pulled files offline"
        )
    leaked = leaks()
    for hit in leaked[:20]:
        print(f"LEAK {hit}")
    print(f"machine paths in clients/fixtures: {len(leaked)}")
    refused = classes.get("refused", 0) + classes.get("not-ported", 0)
    over = args.max_refused is not None and refused > args.max_refused
    if over:
        print(f"{refused} cases refused or not ported; at most {args.max_refused} are ruled")
    return 1 if classes.get("disagree") or stale or leaked or over else 0


if __name__ == "__main__":
    raise SystemExit(main())

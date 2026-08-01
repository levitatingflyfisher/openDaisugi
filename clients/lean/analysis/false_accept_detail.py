"""Deep dive on decompose false-accepts: categorize each one so we know
which are fixable parser bugs vs. tree-sitter-bash GLR-ambiguity artifacts
(context-dependent merges that can't be replicated by a compositional
recursive-descent parser — see clients/lean/README.md's adjudication).
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
CORPUS = REPO_ROOT / ".opendaisugi/conformance/corpus.jsonl"
CLIENT = REPO_ROOT / "clients/lean/.lake/build/bin/conform"


def main() -> int:
    lines = [l for l in CORPUS.read_text(encoding="utf-8").splitlines() if l.strip()]
    cases = [json.loads(l) for l in lines]
    proc = subprocess.run([str(CLIENT)], input="".join(l + "\n" for l in lines),
                           capture_output=True, text=True, timeout=600)
    verdicts = {}
    for line in proc.stdout.splitlines():
        if line.strip():
            v = json.loads(line)
            verdicts[v.get("id", "")] = v

    reject_miss = []       # expected ok=False, got ok=True
    superset_heads = []    # both ok=True, our heads are exp heads with extras spliced in (order-preserving supersequence)
    other_mismatch = []    # both ok=True, genuinely different (not a supersequence)

    def is_supersequence(big, small):
        it = iter(big)
        return all(x in it for x in small)

    for case in cases:
        if case["kind"] != "decompose":
            continue
        got = verdicts.get(case["id"])
        if got is None or "error" in got:
            continue
        exp = case["expect"]
        exp_ok = bool(exp["ok"])
        got_ok = bool(got.get("ok"))
        if not exp_ok and got_ok:
            reject_miss.append(case)
        elif exp_ok and got_ok:
            exp_t = (tuple(exp.get("heads", [])), tuple(sorted(exp.get("reads", []))), tuple(sorted(exp.get("writes", []))))
            got_t = (tuple(got.get("heads", [])), tuple(sorted(got.get("reads", []))), tuple(sorted(got.get("writes", []))))
            if exp_t != got_t:
                if exp_t[1:] == got_t[1:] and is_supersequence(got_t[0], exp_t[0]):
                    superset_heads.append(case)
                else:
                    other_mismatch.append(case)

    print(f"reject_miss (expected False, we said True): {len(reject_miss)}")
    print(f"superset_heads (both True, ours is a supersequence — likely GLR-ambiguity artifact): {len(superset_heads)}")
    print(f"other_mismatch (both True, genuinely different heads/reads/writes): {len(other_mismatch)}")
    print()
    print("=== reject_miss samples ===")
    for c in reject_miss[:30]:
        print(" ", c["id"], repr(c["command"])[:150])
    print()
    print("=== other_mismatch samples (full) ===")
    for c in other_mismatch:
        got = verdicts[c["id"]]
        print(" ---", c["id"])
        print("   command:", repr(c["command"])[:200])
        print("   expected:", c["expect"])
        print("   got:", got)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

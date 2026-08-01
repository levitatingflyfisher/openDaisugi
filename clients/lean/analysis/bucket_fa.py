"""Bucket decompose false-accepts by the extra head token(s) our parser adds
beyond the oracle's expected heads, to distinguish fixable local bugs from
genuine oracle-side GLR artifacts (see clients/ADJUDICATIONS.md's L-1 and
clients/lean/README.md's "decompose" scorecard section — this script is
what produced the "40+ distinct buckets, no dominant token" finding cited
there).

NOTE: this only buckets the "superset_heads" false-accept shape (both
sides ok=true, but our heads are a supersequence of the oracle's). After
the oracle's 2026-08-21 fail-closed fix for the underlying tree-sitter
statement-fusion bug, that shape's count is 0 against the current corpus
(the oracle now rejects those inputs outright instead of fusing them, so
the mismatch shows up as a "reject_miss" in false_accept_detail.py
instead) — so this script currently prints an empty table. The "40+
distinct buckets" finding it produced is a historical result against the
corpus that existed before that fix; it's preserved verbatim in
ADJUDICATIONS.md's original L-1 entry. Kept uncommented/unmodified rather
than adapted to the new shape, since the historical result is what's
cited.

Usage: CUDA_VISIBLE_DEVICES="" .venv/bin/python3 clients/lean/analysis/bucket_fa.py
"""
from __future__ import annotations

import json
import subprocess
from collections import Counter
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
CORPUS = REPO_ROOT / ".opendaisugi/conformance/corpus.jsonl"
CLIENT = REPO_ROOT / "clients/lean/.lake/build/bin/conform"


def is_supersequence(big: tuple, small: tuple) -> bool:
    it = iter(big)
    return all(x in it for x in small)


def main() -> int:
    lines = [l for l in CORPUS.read_text(encoding="utf-8").splitlines() if l.strip()]
    cases = [json.loads(l) for l in lines]
    proc = subprocess.run([str(CLIENT)], input="".join(l + "\n" for l in lines),
                           capture_output=True, text=True, timeout=600)
    verdicts: dict[str, dict] = {}
    for line in proc.stdout.splitlines():
        if line.strip():
            v = json.loads(line)
            verdicts[v.get("id", "")] = v

    extra_counter: Counter = Counter()
    samples_by_extra: dict[tuple, list[str]] = {}
    fa_cases = []

    for case in cases:
        if case["kind"] != "decompose":
            continue
        got = verdicts.get(case["id"])
        if got is None or "error" in got:
            continue
        exp = case["expect"]
        exp_ok = bool(exp["ok"])
        got_ok = bool(got.get("ok"))
        if exp_ok and got_ok:
            exp_t = (tuple(exp.get("heads", [])), tuple(sorted(exp.get("reads", []))),
                      tuple(sorted(exp.get("writes", []))))
            got_t = (tuple(got.get("heads", [])), tuple(sorted(got.get("reads", []))),
                      tuple(sorted(got.get("writes", []))))
            if exp_t != got_t:
                fa_cases.append((case, got))
                if exp_t[1:] == got_t[1:] and is_supersequence(got_t[0], exp_t[0]):
                    # Extra (our-only) head tokens, found via a greedy
                    # left-to-right alignment against the expected sequence
                    # (valid since we already know got_t[0] is a strict
                    # order-preserving supersequence of exp_t[0]).
                    exp_heads = list(exp_t[0])
                    got_heads = list(got_t[0])
                    i = 0
                    extras = []
                    for h in got_heads:
                        if i < len(exp_heads) and h == exp_heads[i]:
                            i += 1
                        else:
                            extras.append(h)
                    key = tuple(extras) if len(extras) <= 3 else ("MANY", len(extras))
                    extra_counter[key] += 1
                    samples_by_extra.setdefault(key, []).append(case["id"])

    print(f"total false-accepts: {len(fa_cases)}")
    print()
    print("=== extra-token buckets (most common first) ===")
    for key, count in extra_counter.most_common(40):
        ids = samples_by_extra[key][:3]
        print(f"  {count:4d}  extras={key}  e.g. {ids}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Measure each built matcher backend's false-positive rate on real journal
tasks, at a range of thresholds. This is the tool the next threshold
derivation reaches for. See ADR-0018, ADR-0019 and ADR-0021. Not a test: it
prints, it does not assert.

The measurement lives in `opendaisugi.bench.layers.matcher`, so
`daisugi bench matcher` and this script cannot drift. This file stays because
the ADRs cite it by name as the command that produced the shipped thresholds.
It scores every distinct pair of tasks, not a random sample.

Usage: uv run --no-sync python scripts/matcher_fpr.py [--journal PATH]
"""

from __future__ import annotations

import argparse
import sqlite3
from pathlib import Path

from opendaisugi.bench.layers.matcher import (
    MATCHER_BACKENDS,
    TARGET_FPR,
    THRESHOLDS,
    absent_hint,
    build_embedder,
    fpr_table,
)

DEFAULT_JOURNAL = Path.home() / ".opendaisugi" / "journal" / "index.db"
MIN_TASK_LEN = 4  # drop empty and placeholder tasks


def load_distinct_tasks(db_path: Path) -> list[str]:
    con = sqlite3.connect(str(db_path))
    try:
        rows = con.execute("SELECT DISTINCT task FROM traces").fetchall()
    finally:
        con.close()
    return sorted({r[0].strip() for r in rows if r[0] and len(r[0].strip()) >= MIN_TASK_LEN})


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--journal", type=Path, default=DEFAULT_JOURNAL)
    args = parser.parse_args()

    if not args.journal.exists():
        print(f"no journal at {args.journal}; nothing to measure.")
        return
    tasks = load_distinct_tasks(args.journal)
    print(f"{args.journal}: {len(tasks)} distinct usable tasks")
    if len(tasks) < 2:
        print("not enough distinct tasks to sample pairs.")
        return
    n_pairs = len(tasks) * (len(tasks) - 1) // 2
    print(f"{n_pairs} distinct-task pairs, target FPR {TARGET_FPR:.4f}")

    for name in MATCHER_BACKENDS:
        embedder = build_embedder(name)
        if embedder is None:
            print(f"\n=== {name} === skipped: {absent_hint(name)}")
            continue
        print(f"\n=== {name} ===")
        best_thr, best_gap = THRESHOLDS[0], float("inf")
        for thr, fpr in fpr_table(embedder.encode, tasks, THRESHOLDS):
            gap = abs(fpr - TARGET_FPR)
            if gap < best_gap:
                best_gap, best_thr = gap, thr
            print(f"  thr={thr:.2f}  fpr={fpr:.4f}")
        print(f"  closest to target FPR {TARGET_FPR:.4f}: thr={best_thr:.2f}")


if __name__ == "__main__":
    main()

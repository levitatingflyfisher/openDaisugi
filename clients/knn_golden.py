"""The knn router's golden cases: one fixed outcome table and fixed vectors.

    uv run --no-sync python clients/knn_golden.py            # check the file is current
    uv run --no-sync python clients/knn_golden.py --write    # rewrite it

The Python module is the oracle. The Go and Rust tests read the same file and must
give the same decision, reason, neighbours and scores for every case, and the same
label for every label case. Vectors are float32 values written as exact JSON numbers.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from opendaisugi import route_knn as rk  # noqa: E402

PATH = Path(__file__).resolve().parent / "fixtures" / "gateway" / "knn_golden.json"
OPUS, SONNET, HAIKU, LOCAL = "claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5", "local-model"
PRICES = {
    OPUS: [15.0, 75.0],
    SONNET: [3.0, 15.0],
    HAIKU: [1.0, 5.0],
    LOCAL: [0.0, 0.0],
}
DIM = 6


def unit(*xs: float) -> list[float]:
    v = np.array(list(xs) + [0.0] * (DIM - len(xs)), dtype=np.float64)
    return [float(x) for x in (v / np.linalg.norm(v)).astype("<f4")]


QUERY = unit(1, 0.2, 0, 0, 0, 0)
NEAR = unit(1, 0.25, 0, 0, 0, 0)
NEAR2 = unit(1, 0.1, 0.1, 0, 0, 0)
MID = unit(1, 1, 0, 0, 0, 0)
EDGE = unit(0.3, 1, 0.2, 0, 0, 0)
FAR = unit(0, 0, 0, 1, 1, 0)


def row(seq: int, vec: list[float], model: str, outcome: str, embedder: str = "fake") -> dict:
    return {
        "seq": seq,
        "turn_id": f"t{seq}",
        "embedder": embedder,
        "vec": vec,
        "model": model,
        "outcome": outcome,
        "ts": 1000.0 + seq,
    }


def unit64(k: int) -> list[float]:
    v = np.array([((i * (k + 3) + k) % 17) - 8 + 0.37 * k for i in range(64)], dtype=np.float64)
    return [float(x) for x in (v / np.linalg.norm(v)).astype("<f4")]


V64 = [unit64(1), unit64(2), unit64(3)]
G, B, U = rk.GOOD, rk.BAD, rk.UNKNOWN


def decide_cases() -> list[dict]:
    cases = []

    def add(name, rows, *, requested=OPUS, local=None, cheap=HAIKU, threshold=0.25, query=None):
        cases.append(
            {
                "name": name,
                "query": query or QUERY,
                "rows": rows,
                "requested": requested,
                "cheap": cheap,
                "local": local,
                "threshold": threshold,
            }
        )

    add("empty table", [])
    add("two known only", [row(1, NEAR, HAIKU, G), row(2, NEAR, HAIKU, G), row(3, NEAR, HAIKU, U)])
    add(
        "cheap model did well",
        [row(i, NEAR, HAIKU, G) for i in range(1, 5)] + [row(5, NEAR2, OPUS, G)],
    )
    add(
        "cheap model did badly",
        [row(i, NEAR, HAIKU, B) for i in range(1, 5)] + [row(5, NEAR2, OPUS, G)],
    )
    add(
        "cheap mixed",
        [
            row(1, NEAR, HAIKU, G),
            row(2, NEAR, HAIKU, B),
            row(3, NEAR2, HAIKU, G),
            row(4, MID, OPUS, G),
            row(5, MID, OPUS, G),
        ],
    )
    add("no known neighbour for the cheap model", [row(i, NEAR, OPUS, B) for i in range(1, 6)])
    add(
        "cheap model has only unknown neighbours",
        [row(1, NEAR, HAIKU, U), row(2, NEAR, HAIKU, U)]
        + [row(i, NEAR2, OPUS, G) for i in range(3, 6)],
    )
    add(
        "only the eight nearest count",
        [row(i, NEAR, HAIKU, G) for i in range(1, 9)]
        + [row(i, MID, OPUS, G) for i in range(9, 20)],
    )
    add("a similarity tie goes to the newer row", [row(i, NEAR, HAIKU, G) for i in range(1, 12)])
    add("below the threshold", [row(i, FAR, HAIKU, G) for i in range(1, 6)])
    add(
        "a row exactly at the threshold counts",
        [row(i, EDGE, HAIKU, G) for i in range(1, 6)],
        threshold=rk.similarity(QUERY, EDGE),
    )
    add(
        "a row just above the threshold is left out",
        [row(i, EDGE, HAIKU, G) for i in range(1, 6)],
        threshold=rk.similarity(QUERY, EDGE) + 0.000001,
    )
    add(
        "cheap request is never upgraded",
        [row(i, NEAR, OPUS, G) for i in range(1, 6)],
        requested=HAIKU,
    )
    add(
        "sonnet request, haiku did well",
        [row(i, NEAR, HAIKU, G) for i in range(1, 5)] + [row(5, NEAR, SONNET, B)],
        requested=SONNET,
    )
    add(
        "local rung did well",
        [row(i, NEAR, LOCAL, G) for i in range(1, 5)]
        + [row(5, NEAR, HAIKU, G), row(6, NEAR, OPUS, G)],
        local=LOCAL,
    )
    add(
        "local rung did badly",
        [row(i, NEAR, LOCAL, B) for i in range(1, 5)]
        + [row(5, NEAR, HAIKU, G), row(6, NEAR, OPUS, G)],
        local=LOCAL,
    )
    add(
        "another embedder's rows do not count",
        [row(i, NEAR, HAIKU, G, embedder="other") for i in range(1, 6)],
    )
    add("a row of another size never matches", [row(i, [1.0, 0.0], HAIKU, G) for i in range(1, 6)])
    add(
        "an unpriced cheap model is not a candidate",
        [row(i, NEAR, "mystery-model", G) for i in range(1, 6)],
        cheap="mystery-model",
    )
    add(
        "opus rows do not count for a sonnet request",
        [row(1, NEAR, OPUS, G), row(2, NEAR, OPUS, G), row(3, MID, HAIKU, B)],
        requested=SONNET,
    )
    add(
        "a cheaper model must match the requested one's success",
        [row(i, NEAR, SONNET, G) for i in range(1, 4)] + [row(4, MID, HAIKU, G)],
        requested=SONNET,
    )
    add(
        "a local model with one bad row never wins",
        [row(1, NEAR, LOCAL, B)] + [row(i, NEAR, OPUS, B) for i in range(2, 5)],
        local=LOCAL,
    )
    add(
        "sixty-four dimensions",
        [row(i, V64[i % 3], HAIKU, G if i % 4 else B) for i in range(1, 7)]
        + [row(7, V64[0], OPUS, G)],
        query=V64[1],
    )
    return cases


def run_decide(case: dict) -> dict:
    prices = {k: tuple(v) for k, v in PRICES.items()}
    rows = [
        rk.Row(
            r["seq"],
            r["turn_id"],
            "c",
            r["ts"],
            "sha",
            r["embedder"],
            np.array(r["vec"], dtype="<f4"),
            r["model"],
            0.0,
            None,
            r["outcome"],
        )
        for r in case["rows"]
        if r["embedder"] == "fake"
    ]
    cands = rk.candidate_models(
        case["requested"], cheap_model=case["cheap"], local_model=case["local"], prices=prices
    )
    kd = rk.decide(
        np.array(case["query"], dtype="<f4"),
        rows,
        requested=case["requested"],
        candidates=cands,
        prices=prices,
        threshold=case["threshold"],
    )
    return {
        "candidates": cands,
        "model": kd.model,
        "fallback": kd.fallback,
        "reason": kd.reason,
        "known": kd.known,
        "neighbours": [
            {
                "turn_id": n.turn_id,
                "similarity": n.similarity,
                "model": n.model,
                "outcome": n.outcome,
            }
            for n in kd.neighbours
        ],
        "scores": [{"model": m, "score": float(f"{s:.6f}")} for m, s in kd.scores],
    }


def label_cases() -> list[dict]:
    out = []
    for name, outcome, ts, now, vec, emb, thr in [
        ("a retry makes it bad", U, 1000.0, 1100.0, NEAR, "fake", 0.25),
        ("moving on makes it good", U, 1000.0, 1100.0, FAR, "fake", 0.25),
        ("exactly ten minutes still counts", U, 1000.0, 1600.0, FAR, "fake", 0.25),
        ("past ten minutes stays unknown", U, 1000.0, 1600.5, FAR, "fake", 0.25),
        ("a known outcome is kept", B, 1000.0, 1100.0, FAR, "fake", 0.25),
        ("another embedder is left alone", U, 1000.0, 1100.0, NEAR, "other", 0.25),
        ("a retry just under the threshold moves on", U, 1000.0, 1100.0, MID, "fake", 0.99),
        (
            "a retry at the threshold is bad",
            U,
            1000.0,
            1100.0,
            MID,
            "fake",
            rk.similarity(QUERY, MID),
        ),
    ]:
        prev_vec = unit(1, 0.2, 0, 0, 0, 0)
        out.append(
            {
                "name": name,
                "prev": {"outcome": outcome, "ts": ts, "vec": prev_vec, "embedder": emb},
                "now": now,
                "vec": vec,
                "threshold": thr,
            }
        )
    return out


def run_label(case: dict) -> str | None:
    p = case["prev"]
    prev = rk.Row(
        1,
        "t1",
        "c",
        p["ts"],
        "sha",
        p["embedder"],
        np.array(p["vec"], dtype="<f4"),
        HAIKU,
        0.0,
        None,
        p["outcome"],
    )
    return rk.label_previous(
        prev,
        np.array(case["vec"], dtype="<f4"),
        "fake",
        now=case["now"],
        threshold=case["threshold"],
    )


def build() -> dict:
    decides = []
    for c in decide_cases():
        decides.append({**c, "expect": run_decide(c)})
    labels = []
    for c in label_cases():
        labels.append({**c, "expect": run_label(c)})
    return {
        "prices": PRICES,
        "decide": decides,
        "label": labels,
        "round6": [
            [x, rk.round6(x)] for x in (0.0, 0.1234565, 0.9999999, 1.0, -0.4999995, 0.3333333)
        ],
        "pack": {
            "vec": unit(1, 2, 3, 0, 0, 0),
            "hex": rk.pack_vec(np.array(unit(1, 2, 3, 0, 0, 0))).hex(),
        },
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--write", action="store_true")
    args = ap.parse_args()
    text = json.dumps(build(), indent=1) + "\n"
    if args.write:
        PATH.write_text(text)
        print(f"wrote {PATH}")
        return 0
    if not PATH.exists() or PATH.read_text() != text:
        print(f"{PATH} is stale: run with --write", file=sys.stderr)
        return 1
    print("knn golden file is current")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

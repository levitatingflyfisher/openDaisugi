"""Write yaml.safe_dump's output for random values, for the emitter test.

    python dump_oracle.py SEED N out.jsonl

Each line is {"value": typed, "yaml": text}. A typed value keeps what
JSON loses: {"int": text}, {"float": repr}, {"dict": [[key, value], ...]}.
The strings mix ASCII words, spaces in every place, line breaks, quotes,
indicators, YAML words (yes, null, 1e3, 0x1f, 2001-01-01) and non-ASCII
text, at lengths around the 80-column width.
"""

import json
import math
import random
import sys

import yaml

WORDS = ["a", "build", "the", "release", "yes", "No", "null", "~", "true", "1", "0x1f", "1_000", "1.5", "1e3",
         ".5", "1:30", "2001-01-01", "-", "--", "---", "...", "?", ":", "#", "a#b", "a #b", "a: b", "a:b",
         "'", '"', "\\", "[x]", "{y}", "*z", "&w", "!v", "|", ">", "%", "@", "`", ",", "<<", "=", "é", "中文",
         "ü", "\t", "\x00", "\x85", "\u2028", "\ufeff", "\ud800", "\U0001f680", "\xa0", "é" * 3]


def text(rng):
    k = rng.random()
    if k < 0.1:
        return ""
    parts = []
    for _ in range(rng.randint(1, 40 if rng.random() < 0.3 else 6)):
        parts.append(rng.choice(WORDS) if rng.random() < 0.5 else "".join(
            rng.choice("abcdefghij ") for _ in range(rng.randint(1, 12))))
    sep = rng.choice([" ", " ", "", "  ", "\n", " \n", "\n ", "\n\n", "-"])
    s = sep.join(parts)
    if rng.random() < 0.2:
        s = " " + s
    if rng.random() < 0.2:
        s = s + rng.choice([" ", "\n", "  "])
    return s


def value(rng, depth=0):
    k = rng.random()
    if depth < 4 and k < 0.2:
        return [value(rng, depth + 1) for _ in range(rng.randint(0, 4))]
    if depth < 4 and k < 0.4:
        return {text(rng)[:30] if rng.random() < 0.5 else rng.choice(["id", "name", "a b", "x"]) + str(i):
                value(rng, depth + 1) for i in range(rng.randint(0, 4))}
    if k < 0.5:
        return rng.choice([None, True, False])
    if k < 0.6:
        return rng.choice([0, 1, -5, 2**70, 42])
    if k < 0.7:
        return rng.choice([0.0, -0.0, 1.5, 1e-7, 2.5e-05, 1e16, 1e22, 123456789.125, math.inf, -math.inf,
                           math.nan, 5e-324, 0.1, 1e300])
    return text(rng)


def typed(v):
    if isinstance(v, bool) or v is None or isinstance(v, str):
        return v
    if isinstance(v, int):
        return {"int": str(v)}
    if isinstance(v, float):
        return {"float": repr(v)}
    if isinstance(v, list):
        return [typed(x) for x in v]
    return {"dict": [[k, typed(x)] for k, x in v.items()]}


def main():
    seed, n, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
    rng = random.Random(seed)
    with open(out, "w", encoding="utf-8") as f:
        for _ in range(n):
            v = {"root": value(rng)} if rng.random() < 0.8 else value(rng)
            try:
                y = yaml.safe_dump(v, sort_keys=False, default_flow_style=False)
            except Exception as exc:  # noqa: BLE001
                y = {"exc": type(exc).__name__}
            f.write(json.dumps({"value": typed(v), "yaml": y}) + "\n")


if __name__ == "__main__":
    main()

"""Write Python's re answers for pyre's differential test.

    python oracle.py SEED N out.jsonl

Each line: {"p": pattern, "s": subject, "err": "Type: msg"} or
{"p", "s", "span": [start, end] | null, "regs": [[a, b], ...], "warn": n}.
Patterns are built from a fixed seed, so a run can be repeated.
"""

import json
import random
import sys
import warnings

import re

ALPHA = ["a", "b", "c", "A", "B", "_", "0", "1", "9", "é", "É", "ß", "İ", "ı", "i", "I", "K", "K",
         "σ", "ς", "Σ", "٣", " ", " ", "\t", "\n", "-", ".", "/", "x", "\U0001d400", "µ", "μ", "ſ", "s"]
ATOMS = ["a", "b", "c", "A", ".", "\\d", "\\D", "\\w", "\\W", "\\s", "\\S", "\\b", "\\B", "^", "$", "\\A", "\\Z",
         "[abc]", "[^abc]", "[a-c]", "[\\d_]", "[^\\W]", "é", "ß", "İ", "ı", "K", "K", "σ", "\\n", "\\t",
         "\\x41", "\\u00e9", "\\U0001d400", "\\101", "\\0", "[a-]", "[]a]", "[\\]]", "\\.", "\\-", "-", "/", "x",
         "[à-ÿ]", "[α-ω]", "[\\u0100-\\u017f]", "[\\U0001d400-\\U0001d4ff]", "[^\\s]", "[\\S\\d]", "\\\\",
         "(?i)", "(?m)", "(?s)", "(?a)", "(?x)", " ", "#", "\\1", "\\2", "(?P=g)", "{", "}", "{2}", "a{,2}",
         "[", "(", ")", "*", "+", "?", "|", "\\", "\\q", "[z-a]", "(?<", "(?P<1>", "\\N{EM DASH}", "(?L)",
         "[[a]", "[a--b]", "[a&&b]"]
REPEATS = ["*", "+", "?", "{2}", "{1,3}", "{,2}", "{2,}", "*?", "+?", "??", "{1,2}?", "*+", "++", "?+", "{0}", "{1}"]


def pattern(rng: random.Random, depth: int = 0) -> str:
    parts = []
    for _ in range(rng.randint(1, 4)):
        k = rng.random()
        if k < 0.55 or depth > 2:
            parts.append(rng.choice(ATOMS))
        elif k < 0.7:
            inner = pattern(rng, depth + 1)
            opener = rng.choice(["(", "(?:", "(?P<g>", "(?>", "(?=", "(?!", "(?<=", "(?<!", "(?i:", "(?-i:",
                                 "(?s:", "(?m:", "(?a:", "(?x:"])
            parts.append(opener + inner + ")")
        elif k < 0.8:
            parts.append(pattern(rng, depth + 1) + "|" + pattern(rng, depth + 1))
        elif k < 0.85:
            parts.append("(?(1)" + pattern(rng, depth + 1) + "|" + pattern(rng, depth + 1) + ")")
        else:
            parts.append(rng.choice(ATOMS) + rng.choice(REPEATS))
    return "".join(parts)


def subject(rng: random.Random) -> str:
    return "".join(rng.choice(ALPHA) for _ in range(rng.randint(0, 12)))


def main() -> None:
    seed, n = int(sys.argv[1]), int(sys.argv[2])
    rng = random.Random(seed)
    with open(sys.argv[3], "w", encoding="utf-8") as out:
        for _ in range(n):
            p = pattern(rng)
            if rng.random() < 0.3:
                p = "(" + p + ")" + rng.choice(REPEATS)
            for _ in range(3):
                s = subject(rng)
                row = {"p": p, "s": s}
                re.purge()
                with warnings.catch_warnings(record=True) as w:
                    warnings.simplefilter("always")
                    try:
                        m = re.search(p, s)
                    except Exception as exc:  # noqa: BLE001 - the answer being recorded
                        row["err"] = f"{type(exc).__name__}: {exc}"
                        out.write(json.dumps(row) + "\n")
                        break
                row["warn"] = len(w)
                if m is None:
                    row["span"] = None
                else:
                    row["span"] = list(m.span())
                    row["regs"] = [list(r) for r in m.regs]
                out.write(json.dumps(row) + "\n")
            re.purge()


if __name__ == "__main__":
    main()

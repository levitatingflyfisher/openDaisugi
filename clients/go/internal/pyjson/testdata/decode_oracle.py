"""Write json.loads's answers for LoadsPy's differential test.

    python decode_oracle.py SEED N out.jsonl

Each line: {"t": text, "ok": dump} or {"t": text, "err": str(exc)}.
Texts are random fragments of JSON, most of them broken.
"""

import json
import math
import random
import sys

PIECES = ['{', '}', '[', ']', ',', ':', ' ', '\n', '\t', '"', '"a"', '"b"', '1', '-', '0', '01', '1.', '1.5', '1e',
          '1e5', '1E+2', '-0', 'true', 'tru', 'false', 'null', 'nul', 'NaN', 'Infinity', '-Infinity', '"\\', '\\n',
          '\\u00e9', '\\ud83d\\ude00', '\\ud800', '\\u12', '\\x', '\\"', 'é', '😀', '\x01', 'x', '"\\u', '\\ud800\\u0041',
          '"\\ud800\\uzz', '1' * 5, '"k": ', '[1, 2]', '{"a": 1}', '\r', '\x1f', '\\/', '"\\ud83d"']


def dump(v):
    if isinstance(v, bool) or v is None or isinstance(v, str):
        return v
    if isinstance(v, int):
        return {"int": str(v)}
    if isinstance(v, float):
        return {"float": "nan" if math.isnan(v) else repr(v)}
    if isinstance(v, list):
        return [dump(x) for x in v]
    return {"dict": [[k, dump(x)] for k, x in v.items()]}


seed, n, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
rng = random.Random(seed)
with open(out, "w", encoding="utf-8") as f:
    for _ in range(n):
        t = "".join(rng.choice(PIECES) for _ in range(rng.randint(0, 8)))
        row = {"t": t}
        try:
            row["ok"] = dump(json.loads(t))
        except (ValueError, RecursionError) as e:
            row["err"] = str(e)
        f.write(json.dumps(row) + "\n")

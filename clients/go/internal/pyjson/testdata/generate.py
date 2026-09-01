"""Write pyjson.json: what Python's json and repr do on a fixed set of
inputs, for the Go package's differential test.

    uv run --no-sync python clients/go/internal/pyjson/testdata/generate.py
"""

import json
from pathlib import Path

LOADS = [
    '{"a": 1, "b": 2, "a": 3}',
    "[-0, 1E2, 1.5e-7, 12345678901234567890123, 0.1, 1e400]",
    '{"s": "\\u00e9\\u2014\\ud83d\\ude00 \\/ \\b\\f\\n\\r\\t"}',
    "[NaN, Infinity, -Infinity]",
    '  {"nested": {"x": [true, false, null, {}]}}  ',
    '"just a string"',
    "01",
    "1.",
    "[1,]",
    '{"a":1,}',
    "",
    "   ",
    "﻿{}",
    '"tab\there"',
    '{"a" 1}',
    "[1] [2]",
    "tru",
    "-",
    "1e5",
    "-1.25e-3",
    '"\\x41"',
    "{\"é\": \"é—\x7f\"}",
]

FLOATS = [
    0.1, 1e16, 1e15, 123456789012345678.0, 1e-5, 0.0001, -2.5, 3.0,
    1790266313.5970256, 1e22, 5e-324, 1.7976931348623157e308, -0.0, 100.0,
    12345.678, 9999999999999998.0, 0.00012345,
]

ROUNDS = [[0.0005, 3], [1.0005, 3], [2.675, 2], [143.7744, 3], [0.1235, 3], [31.4699999, 3]]

REPRS = [
    "a", "it's", 'say "hi"', "both ' \"", "tab\t\x00\x7f\x85é \U0001f600",
    "back\\slash", "", "/work/**", "​", "\x1b[0m",
]


def main() -> None:
    loads = []
    for text in LOADS:
        try:
            v = json.loads(text)
        except Exception:  # noqa: BLE001 - the error itself is the fixture
            loads.append({"text": text, "ok": False})
            continue
        loads.append(
            {
                "text": text,
                "ok": True,
                "ascii": json.dumps(v),
                "utf8": json.dumps(v, ensure_ascii=False),
            }
        )
    out = {
        "loads": loads,
        "floats": [{"bits": v.hex(), "repr": json.dumps(v)} for v in FLOATS],
        "rounds": [{"bits": x.hex(), "n": n, "repr": json.dumps(round(x, n))} for x, n in ROUNDS],
        "reprs": [{"s": s, "repr": repr(s)} for s in REPRS],
        "repr_list": {"in": REPRS[:4], "repr": repr(REPRS[:4])},
    }
    path = Path(__file__).with_name("pyjson.json")
    path.write_text(json.dumps(out, indent=1, ensure_ascii=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()

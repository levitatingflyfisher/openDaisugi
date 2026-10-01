"""Python's answers for the ports' literal reader: models.decode_dict_text
(json.loads, then ast.literal_eval) on many texts, valid and broken.

    uv run --no-sync python clients/literal_cases.py [--out clients/fixtures/literal]

Each line of cases.jsonl is {"text": ..., "expect": ...}. The expectation
is {"none": true} when decode_dict_text returns None, the dict in a typed
dump ({"dict": [[key, value], ...]}, {"list": [...]}, {"tuple": [...]},
{"int": "5"}, {"float": "1.5"}, {"str": "x"}, true, false, null), or
{"unmodeled": why} for a dict that holds what the ports do not model (a
set, bytes, a complex number, Ellipsis, a key that is not a str, a \\N{...}
escape). "warn" is true when Python's parser printed a SyntaxWarning
while it read the text: the ports refuse such a text, since Python then
writes to stderr.

The texts are a fixed list, then mutations of it (a character dropped,
added or swapped, a line repeated, the text cut short) from a fixed seed,
so the file is the same on every run. The Go and Rust tests read it.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import random
import sys
import warnings
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "src"))

from opendaisugi.models import decode_dict_text  # noqa: E402 - after the path

FIXTURE_DIR = REPO / "clients" / "fixtures" / "literal"
SEED = 20261008
MUTANTS = 3000

BASE = [
    "{}",
    "{'id': 's1', 'type': 'shell', 'command': 'echo hi'}",
    '{"id": "s1", "type": "shell", "command": "echo hi", "depends_on": []}',
    "{'id': 's1', 'type': 'shell', 'command': 'ls', 'depends_on': ('a', 'b'), 'x': (1,)}",
    "{'type': 'file_read', 'path': '/w/a.txt', 'metadata': {'n': 12345678901234567890123, 'f': 1.5, "
    "'t': True, 'u': False, 'v': None, 'w': [1, -2, +3, -0.0]}}",
    "{'a': 'x\\'y\\n', \"b\": \"q\\\"r\", 'c': '\\t\\a\\b\\f\\v\\r\\0\\101\\x41\\u00e9\\U0001F600'}",
    "{'a': '''x\ny''', 'b': \"\"\"p\"q\"\"\", 'c': r'\\d+', 'd': R\"\\w\", 'e': u'z', 'f': U'y'}",
    "{'a': 'x' 'y' \"z\", 'b': 'p' r'\\q'}",
    "{'a': 0x1F, 'b': 0o17, 'c': 0b101, 'd': 1_000, 'e': 0X_f, 'f': 0_0, 'g': 00}",
    "{'a': 1., 'b': .5, 'c': 1e5, 'd': 1E-5, 'e': 1_0.5_0, 'f': 09.5, 'g': 1e400, 'h': -1e400}",
    "{'a': 012}",
    "{'a': 1__0}",
    "{'a': 1if 1 else 2}",
    "{'a': 0x1for}",
    "{'a': '\\d'}",
    "{'a': '\\777'}",
    "{'a': '\\N{BULLET}'}",
    "{'a': '\\é'}",
    "{'a': '\\x4'}",
    "{'a': '\\ud800'}",
    "{'a': b'x'}",
    "{'a': b'x' 'y'}",
    "{'a': rb'\\d', 'b': Br'x'}",
    "{'a': b'\\N'}",
    "{'a': 1+2j, 'b': -1-2j}",
    "{'a': 1+2}",
    "{'a': ...}",
    "{'a': set()}",
    "{'a': {1, 2}}",
    "{1: 2, 'type': 'shell'}",
    "{True: 1, 'type': 'shell'}",
    "{'a': [1,], 'b': (1,), 'c': (), 'd': (1), 'e': [], 'f': {}}",
    "{'a':1,}",
    "{,}",
    "{'a': 1 # c\n}",
    "{'a': \\\n 1}",
    "{'a': -(1)}",
    "{'a': (-1)}",
    "{'a': - 1}",
    "{'a': -(-1)}",
    "{'a': --1}",
    "{'a': -True}",
    "{'a': ~1}",
    "{'a': 1 2}",
    "{'a': f'x'}",
    "{'a': ur'x'}",
    "{'a': [1]}['a']",
    "{'a': 1}, 2",
    "{'a': 1}\n{'b': 2}",
    "{'a': 1} # tail",
    "{**{'a': 1}}",
    "{[1]: 2}",
    "{'a': {[1], 2}}",
    "{'a': {(1, [2])}}",
    "{(1, 2): 'x'}",
    "{'a': 'x\ry'}",
    "{'a': '''x\r\ny'''}",
    "{'a':\u00a01}",
    "{'a': 1\v}",
    "{'type': 'shell', 'type': 'file_read', 'path': 'p'}",
    "{'a': True, 'b': False, 'c': None, 'd': none}",
    "{'a': set( )}",
    "{'a': set(())}",
    "{'a': 1.5_e3}",
    "{'a': 1e_5}",
    "{'a': 1.e5, 'b': 1.j, 'c': 0e5, 'd': 0j, 'e': 1E5J}",
    "{'a': 0b12}",
    "{'a': 0o8}",
    "{'a': 1else}",
    "{'a': 1 if}",
    "{'a': 'é', 'é': 1}",
    "{'a': 'x\\\ny'}",
    "{'a': r'x\\\ny'}",
    "{" + "'a': [" * 3 + "1" + "]" * 3 + "}",
    "{'a':" + "[" * 199 + "]" * 199 + "}",
    "{'a':" + "[" * 200 + "]" * 200 + "}",
    "{'a': (1, 2) + (3,)}",
    "{'a': 1 < 2}",
    "{'a': lambda: 1}",
    "{'a': x}",
    "{'a': 1}\n",
    "{'a': 1} \\\n",
    '{"type": "network", "url": "https://x", "method": "GET", "id": "n"}',
    "{'type': 'shell', 'id': 's', 'command': 'grep -E \"a|b\" f', 'depends_on': ['x']}",
]

ALPHABET = list("{}[](),:'\"\\#\n -+.0123456789abefjnorxyzTNFsu_eE") + ["é", "\t", "j", "\r"]


class Unmodeled(Exception):
    pass


def typed(v: Any) -> Any:
    if v is None or isinstance(v, bool):
        return v
    if isinstance(v, int):
        try:
            return {"int": str(v)}
        except ValueError:
            # Past 4300 decimal digits (read from hex): str() raises.
            raise Unmodeled("an int past 4300 decimal digits") from None
    if isinstance(v, float):
        return {"float": repr(v)}
    if isinstance(v, str):
        return {"str": v}
    if isinstance(v, list):
        return {"list": [typed(x) for x in v]}
    if isinstance(v, tuple):
        return {"tuple": [typed(x) for x in v]}
    if isinstance(v, dict):
        out = []
        for k, x in v.items():
            if not isinstance(k, str):
                raise Unmodeled("a dict key that is not a str")
            out.append([k, typed(x)])
        return {"dict": out}
    raise Unmodeled(type(v).__name__)


def expect(text: str) -> dict[str, Any]:
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        v = decode_dict_text(text)
    warned = any(issubclass(w.category, SyntaxWarning) for w in caught)
    out: dict[str, Any] = {"warn": warned}
    if v is None:
        out["value"] = {"none": True}
        return out
    try:
        out["value"] = typed(v)
    except Unmodeled as e:
        out["value"] = {"unmodeled": str(e)}
        return out
    if "\\N{" in text:
        try:
            json.loads(text.strip())
        except ValueError:
            out["value"] = {"unmodeled": "a \\N{...} escape"}
    return out


def mutate(rng: random.Random, text: str) -> str:
    if not text:
        return rng.choice(ALPHABET)
    op = rng.randrange(6)
    i = rng.randrange(len(text) + 1)
    if op == 0 and i < len(text):
        return text[:i] + text[i + 1 :]
    if op == 1:
        return text[:i] + rng.choice(ALPHABET) + text[i:]
    if op == 2 and i < len(text):
        return text[:i] + rng.choice(ALPHABET) + text[i + 1 :]
    if op == 3:
        j = rng.randrange(len(text) + 1)
        a, b = sorted((i, j))
        return text[:b] + text[a:b] + text[b:]
    if op == 4:
        return text[:i]
    return text[:i] + rng.choice(["\n", " ", "\\\n", "#c\n"]) + text[i:]


def texts() -> list[str]:
    rng = random.Random(SEED)
    out = list(BASE)
    seen = set(out)
    while len(out) < len(BASE) + MUTANTS:
        t = rng.choice(BASE)
        for _ in range(rng.randrange(1, 4)):
            t = mutate(rng, t)
        if t not in seen and len(t) < 2000:
            seen.add(t)
            out.append(t)
    # Python 3.12 reads at most 4300 decimal digits into an int (hex,
    # octal and binary are not limited); these come after the mutants so
    # the seeded list above stays as it was.
    pre = "{'type': 'shell', 'id': 's', 'command': 'c', 'metadata': {'n': "
    out += [
        pre + "1" * 4300 + "}}",
        pre + "1" * 4301 + "}}",
        pre + "-" + "1_" * 4300 + "1}}",
        pre + "0" * 5000 + "}}",
        pre + "0x" + "f" * 5000 + "}}",
        pre + "1" * 5000 + ".5}}",
        pre + "1" * 5000 + "j}}",
        '{"type": "shell", "id": "s", "command": "c", "metadata": {"n": ' + "1" * 4301 + "}}",
    ]
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    lines = [json.dumps({"text": t, **expect(t)}) for t in texts()]
    body = "\n".join(lines) + "\n"
    (args.out / "cases.jsonl").write_text(body, encoding="utf-8")
    manifest = {
        "python": sys.version.split()[0],
        "cases.jsonl": hashlib.sha256(body.encode("utf-8")).hexdigest(),
        "n": len(lines),
    }
    (args.out / "manifest.json").write_text(json.dumps(manifest, indent=1, sort_keys=True) + "\n")
    print(f"wrote {len(lines)} cases to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""tomllib's answers for the ports' TOML reader: tomllib.loads on many
texts, valid and broken.

    uv run --no-sync python clients/toml_cases.py [--out clients/fixtures/toml]

Each line of cases.jsonl is {"text": ..., "expect": ...}. The expectation
is the loaded document in a typed dump, or {"exc": "TOMLDecodeError",
"msg": str(exc)} when tomllib rejects the text ({"exc": "ValueError", ...}
for an int past Python's 4300-digit limit). Values: a table is {"table": [[key, value],
...]} in tomllib's key order, an array {"array": [...]}, a str {"str":
...}, an int {"int": "5"}, a float {"float": repr}, a bool true or false,
and a date or time {"datetime": repr(value)} (its Python repr, so an offset
and fractional seconds compare exactly).

The texts are a fixed list, then mutations of it (a character dropped,
added or swapped, a line repeated, the text cut short) from a fixed seed,
so the file is the same on every run. The Go and Rust tests read it.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import random
import sys
import tomllib
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "toml"
SEED = 20261008
MUTANTS = 3000

BASE = [
    "",
    "# only a comment\n",
    'a = 1\nb = "x"\nc = true\nd = 1.5\n',
    '[routes.r1]\nid = "default"\ncapable_target = "big"\nefficient_target = "small"\n'
    '[targets.big]\nid = "claude-sonnet-5"\nllm_client = "anthropic"\n'
    '[targets.small]\nid = "claude-haiku"\nllm_client = "anthropic"\n'
    "[llm_clients.anthropic]\nforward_auth = true\n",
    'a."b.c".d = 1\n"x y" = 2\n\'lit\' = 3\nbare-key_9 = 4\n',
    "a = \"\"\"\nmulti\nline \\\n   joined\"\"\"\nb = '''\nraw \\n\nlines'''\n",
    'a = "esc \\t \\" \\\\ \\u00e9 \\U0001F600 \\b \\f \\n \\r"\n',
    "a = 'C:\\path\\x'\n",
    "a = 1979-05-27T07:32:00Z\nb = 1979-05-27T00:32:00-07:00\nc = 1979-05-27T00:32:00.999999-07:00\n"
    "d = 1979-05-27T07:32:00\ne = 1979-05-27\nf = 07:32:00\ng = 00:32:00.999999\nh = 1979-05-27 07:32:00Z\n",
    "a = 1979-05-27T07:32:00.123456789Z\nb = 1979-05-27t07:32:00z\n",
    '[[products]]\nname = "Hammer"\nsku = 738594937\n[[products]]\n[[products]]\nname = "Nail"\n',
    '[[fruit]]\nname = "apple"\n[fruit.physical]\ncolor = "red"\n[[fruit.variety]]\nname = "red delicious"\n',
    "a = [\n  1,\n  2, # comment\n  3,\n]\nb = [[1, 2], ['a', \"b\"], [1.5]]\nc = []\n",
    "a = { x = 1, y = { z = 'q' } }\nb = {}\n",
    "a = 0xDEAD_BEEF\nb = 0o755\nc = 0b1101\nd = +17\ne = -0\nf = 1_000_000\n",
    "a = 9223372036854775807\nb = -9223372036854775808\n",
    "a = 9223372036854775808\n",
    "a = inf\nb = -inf\nc = +inf\nd = nan\ne = 6.626e-34\nf = 5e+22\ng = -2E-2\nh = 224_617.445_991\n",
    "a = 01\n",
    "a = 1.\n",
    "a = .1\n",
    "a = 1__0\n",
    "[a]\n[a]\n",
    "a = 1\na = 2\n",
    "a.b = 1\n[a]\nc = 2\n",
    "[a]\nb.c = 1\n[a.b]\nd = 2\n",
    "a = {b = 1}\n[a]\nc = 2\n",
    "[[a]]\n[a]\n",
    "a = [1, 'x']\n",
    "key = # missing\n",
    "= 1\n",
    'a = "\\x41"\n',
    'a = "unterminated\n',
    "a = 1 b = 2\n",
    "[routes\nid = 1\n",
    "a = 'x'\n\n\n[ b ]\n  c=1\n",
    "\ufeffa = 1\n",
    'a = "\u0001"\n',
    "a = 1\r\nb = 2\r\n",
    "a = 1\rb = 2\n",
    "a = true\nb = false\nc = TRUE\n",
    "x = 1987-07-05T17:45:00Z\ny = 1987-07-05 17:45:00\nz = 1987-07-05T17:45Z\n",
    "a = 2000-02-30\n",
    "a = 24:00:00\n",
    "a = 2000-01-01T00:00:00+24:00\n",
    "[a.b.c]\n[a]\nd = 1\n",
    "[x] # comment\ny = \"\"\"\"quoted\" \"\"\"\nz = '''''one quote'''\n",
    'a = """\\\n\n  x"""\n',
    "# é ü ☃\nname = \"☃\"\n'é' = 1\n",
    "a = [ { b = 1 }, { c = 2 } ]\n",
    "a = { b = 1, }\n",
    "a = { b = 1\n}\n",
]

ALPHABET = list("[]{}=.,\"'#\n -+_:0123456789abefinrtxyzTZ") + ["é", "\t", "\\"]


def typed(v: Any) -> Any:
    if isinstance(v, bool):
        return v
    if isinstance(v, int):
        return {"int": str(v)}
    if isinstance(v, float):
        return {"float": repr(v)}
    if isinstance(v, str):
        return {"str": v}
    if isinstance(v, (datetime.datetime, datetime.date, datetime.time)):
        return {"datetime": repr(v)}
    if isinstance(v, list):
        return {"array": [typed(x) for x in v]}
    if isinstance(v, dict):
        return {"table": [[k, typed(x)] for k, x in v.items()]}
    raise TypeError(type(v))


def expect(text: str) -> dict[str, Any]:
    try:
        return typed(tomllib.loads(text))
    except tomllib.TOMLDecodeError as e:
        return {"exc": "TOMLDecodeError", "msg": str(e)}
    except ValueError as e:
        # int() past Python's 4300-digit limit: a ValueError that is not a
        # TOMLDecodeError.
        return {"exc": "ValueError", "msg": str(e)}


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
        lines = text.split("\n")
        j = rng.randrange(len(lines))
        return "\n".join(lines[: j + 1] + [lines[j]] + lines[j + 1 :])
    if op == 4:
        return text[:i]
    return text[:i] + rng.choice([" ", "\n", "#c\n", "\t"]) + text[i:]


def texts() -> list[str]:
    rng = random.Random(SEED)
    out = list(BASE)
    seen = set(out)
    while len(out) < len(BASE) + MUTANTS:
        t = rng.choice(BASE)
        for _ in range(rng.randrange(1, 4)):
            t = mutate(rng, t)
        if t not in seen:
            seen.add(t)
            out.append(t)
    # int() reads at most 4300 decimal digits (hex is not limited); after
    # the mutants, so the seeded list stays as it was.
    out += ["n = " + "1" * 4300 + "\n", "n = " + "1" * 4301 + "\n", "n = -" + "1_" * 4300 + "1\n"]
    out += ["n = 0x" + "f" * 100 + "\n"]
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    lines = [json.dumps({"text": t, "expect": expect(t)}) for t in texts()]
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

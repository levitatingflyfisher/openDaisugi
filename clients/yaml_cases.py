"""PyYAML's answers for the ports' YAML readers: yaml.safe_load on many
texts, valid and broken.

    uv run --no-sync python clients/yaml_cases.py [--out clients/fixtures/yaml]

Each line of cases.jsonl is {"text": ..., "expect": ...}. The expectation
is the loaded value in a typed dump ({"int": "5"}, {"float": "1.5"},
{"list": [...]}, {"dict": [[key, value], ...]}, {"other": "date"}), or the
error safe_load raises: {"exc": type, "msg": str(exc)}, or {"unmodeled":
why} for a value the ports' result model does not hold (bytes, a set, an
ordered map, a float key, a node that holds itself). A key that is not a
str is {"key": "True"}, {"key": "False"}, {"key": "None"} or {"key":
"int:N"}: True and 1 are one key in a dict, and so are False and 0.

The texts are a fixed list, then mutations of it (a character dropped,
added or swapped, a line repeated, the text cut short) from a fixed seed,
so the file is the same on every run. The Go and Rust tests read it.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import math
import random
import sys
from pathlib import Path
from typing import Any

import yaml

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "yaml"
SEED = 20261003
MUTANTS = 2500

BASE = [
    "",
    "a: 1\n",
    "a: yes\nb: No\nc: ~\nd: null\ne:\n",
    "a: 0x1F\nb: 0o17\nc: 017\nd: 0b101\ne: 1_000\nf: 1:30\ng: -0\nh: +12\n",
    "a: 1.5\nb: 1.\nc: .5\nd: 1e5\ne: 1.0e+5\nf: .inf\ng: -.Inf\nh: .NaN\ni: 1:30.5\nj: 1_0.5\n",
    "a: 2024-01-01\nb: 2024-1-1 10:00:00\nc: 2024-02-30\n",
    "a: 2001-12-14t21:59:43.10-05:00\n",
    "- a\n- b\n- - c\n  - d\n",
    "a:\n  - 1\n  - 2\nb:\n- 3\n- 4\n",
    "a: [1, 2, [3, 4]]\nb: {c: d, e: [f]}\n",
    "[1, 2, 3]\n",
    "{a: 1, b: 2}\n",
    '{"id": "env_1", "permissions": {"shell": true, "shell_allowlist": ["echo"]}}\n',
    'a: \'it\'\'s\'\nb: "tab\\there"\nc: "\\u00e9\\x41\\U0001F600"\nd: "\\N\\_\\L\\P"\n',
    'a: "line\n  folded\n\n  para"\n',
    "a: 'single\n  folded'\n",
    "a: |\n  keep\n  lines\n\nb: >\n  folded\n  text\n\n  next\n",
    "a: |-\n  strip\n\n\nb: |+\n  keep\n\n\nc: >2\n    indented\n",
    "a: &x 1\nb: *x\nc: &m {k: v}\nd: *m\n",
    "base: &b\n  x: 1\n  y: 2\nderived:\n  <<: *b\n  y: 3\n",
    "a: &a {x: 1}\nb: &b {y: 2}\nc:\n  <<: [*a, *b]\n  z: 3\n",
    "a: !!str 5\nb: !!int '7'\nc: !!float '1.5'\nd: !!bool 'yes'\ne: !!null ''\n",
    "a: !!seq [1]\nb: !!map {c: d}\nc: ! 5\nd: ! '5'\n",
    "%YAML 1.1\n---\na: 1\n",
    "%TAG !e! tag:example.com,2000:\n---\na: !e!foo 1\n",
    "--- \na: 1\n...\n",
    "---\n- a\n---\n- b\n",
    "? complex\n: value\n? [a, b]\n: seq\n",
    "a: b: c\n",
    "a: [1, 2\n",
    "a: {b: 1\n",
    "a: 'unclosed\n",
    'a: "unclosed\n',
    "  a: 1\n b: 2\n",
    "a:\n  b: 1\n c: 2\n",
    "- a\nb: c\n",
    "a: *nope\n",
    "a: &x 1\nb: &x 2\n",
    "a: !foo 1\n",
    "a: !!binary aGVsbG8=\n",
    "a: !!set {x, y}\n",
    "a: !!omap [{x: 1}]\n",
    "a: \t1\n",
    "\ta: 1\n",
    "a: @x\n",
    "a: `x`\n",
    "a: %x\n",
    'a: "\\q"\n',
    'a: "\\x4"\n',
    "%YAML 2.0\n---\na: 1\n",
    "%YAML 1.1\n%YAML 1.1\n---\na: 1\n",
    "%FOO bar\n---\na: 1\n",
    "a: 1\n--- b\n",
    "a: x\n# comment\nb: y # trailing\n",
    "\ufeffa: 1\n",
    "a: \x01\n",
    "a: \x7f\n",
    "key with spaces: value with spaces\n",
    "'quoted key': 1\n\"dq key\": 2\n",
    "1: one\ntrue: t\n~: n\n0: zero\nfalse: f\n",
    "a: 1\na: 2\n",
    "[a, b]: 1\n",
    "{a: 1}: 2\n",
    "a: =\n",
    "a: <<\n",
    "<<: 1\n",
    "<<: [1]\n",
    "a: -\nb: - x\n",
    "- - - a\n",
    "a:\n- b\n-\n- c\n",
    "a: |\n text\nb: 1\n",
    "a: |0\n  x\n",
    "a: |x\n",
    "a: >\n\n  x\n",
    "a: 1 # c\n# end",
    "a: 'x' y\n",
    "a: [x]y\n",
    "a: {x: 1,}\n",
    "a: [x, ]\n",
    "a: [x,, y]\n",
    "[a: 1, b]\n",
    "{a, b: c}\n",
    "a: 'é中文'\nb: 中文\n",
    "a: b\n  c\n",
    "a:\n  b\n  c\n",
    "a: 12:61\nb: 1:2:3\nc: 0:0.5\n",
    "a: 10000000000000000000000\nb: 1e400\nc: -1e400\n",
    "a: 0b\nb: 0x\nc: 1_\nd: ._5\n",
    "a: &r [*r]\n",
    "*a\n",
    "&a\n",
    "!!str\n",
    "!\n",
    "? a\n? b\n",
    "a:\n  ? b\n  : c\n",
    "- {a: 1}\n- [b]\n- c: d\n  e: f\n",
    'a: "\\\n  b"\n',
    "a: '\n'\n",
    "a: [\n  1,\n  2\n]\n",
    "a: {\n  b: 1\n}\n",
    "a: !<tag:yaml.org,2002:str> 5\n",
    "a: !<!> 5\n",
    "a: !e!x 1\n",
    "a: !%41 1\n",
    "a: !a%zz 1\n",
    "...\n",
    "---\n",
    "--- |\n  x\n",
    "- &a a\n- *a\n- &b [1]\n- *b\n",
    "a:\n  - b\n  -\n    c: d\n",
    'a: "\\u00"\n',
    'a: "\\U0010FFFF"\n',
    "a: '#not comment'\nb: x#y\nc: x #y\n",
    "a: :x\nb: x:\nc: ::\n",
    "a: ?x\nb: -x\n",
    "-1\n",
    "1.5\n",
    "plain text\n",
    "'just a string'\n",
    "a: b\n---\n",
    "a: 2024-01-01 25:00:00\n",
    "a: 0000-01-01\n",
    "a: 2024-01-01T10:00:00+24:00\n",
]

ALPHABET = list(" \n\t:-?,[]{}#&*!|>'\"%@`.~=<\\0123456789abcxyzTF") + ["é", "\u2028", "\r"]


def dump(v: Any, stack: tuple[int, ...] = ()) -> Any:
    if v is None or isinstance(v, (bool, str)):
        return v
    if isinstance(v, int):
        try:
            return {"int": str(v)}
        except ValueError:
            raise Unmodeled("an int past 4300 decimal digits") from None
    if isinstance(v, float):
        return {"float": "nan" if math.isnan(v) else repr(v)}
    if isinstance(v, datetime.datetime):
        return {"other": "datetime"}
    if isinstance(v, datetime.date):
        return {"other": "date"}
    if isinstance(v, (list, dict)):
        if id(v) in stack:
            raise Unmodeled("a node that holds itself")
        stack = stack + (id(v),)
        if isinstance(v, list):
            if any(isinstance(x, tuple) for x in v):
                raise Unmodeled("an ordered map")
            return {"list": [dump(x, stack) for x in v]}
        return {"dict": [[key(k), dump(x, stack)] for k, x in v.items()]}
    raise Unmodeled(type(v).__name__)


def key(k: Any) -> Any:
    if isinstance(k, str):
        return k
    if k is None:
        return {"key": "None"}
    if isinstance(k, bool) or (isinstance(k, int) and k in (0, 1)):
        return {"key": "True" if k else "False"}
    if isinstance(k, int):
        return {"key": f"int:{k}"}
    raise Unmodeled(f"a {type(k).__name__} key")


class Unmodeled(Exception):
    pass


def expect(text: str) -> Any:
    try:
        v = yaml.safe_load(text)
    except RecursionError:
        return {"unmodeled": "recursion"}
    except Exception as e:  # noqa: BLE001 - every error is an answer
        return {"exc": type(e).__name__, "msg": str(e)}
    try:
        return dump(v)
    except Unmodeled as u:
        return {"unmodeled": str(u)}


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
    lines = text.split("\n")
    j = rng.randrange(len(lines))
    lines[j] = " " * rng.randrange(1, 4) + lines[j]
    return "\n".join(lines)


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
    # int() reads at most 4300 decimal digits (hex, octal and binary are
    # not limited; base 60 reads each part); after the mutants, so the
    # seeded list stays as it was.
    out += [
        "n: " + "1" * 4300 + "\n",
        "n: " + "1" * 4301 + "\n",
        "n: -" + "1_" * 4300 + "1\n",
        "n: 0x" + "f" * 100 + "\n",
        "[" + "9" * 5000 + "]\n",
    ]
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    lines = [json.dumps({"text": t, "expect": expect(t)}, ensure_ascii=False) for t in texts()]
    body = "\n".join(lines) + "\n"
    (args.out / "cases.jsonl").write_text(body, encoding="utf-8")
    manifest = {
        "pyyaml": yaml.__version__,
        "cases.jsonl": hashlib.sha256(body.encode("utf-8")).hexdigest(),
        "n": len(lines),
    }
    (args.out / "manifest.json").write_text(json.dumps(manifest, indent=1, sort_keys=True) + "\n")
    print(f"wrote {len(lines)} cases to {args.out}")
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

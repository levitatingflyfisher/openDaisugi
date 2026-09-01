"""Write re's answers to \\N{...} for pyre's name test.

    python names_oracle.py SEED N out.jsonl

Each line: {"name": ..., "cp": code point} or {"name", "err": "Type: msg"},
from re.compile("\\\\N{" + name + "}"). Names mix real names in every
case, aliases, named sequences, Hangul and CJK forms, and junk.
"""

import json
import random
import re
import sys
import unicodedata
from re import _parser

seed, n, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
rng = random.Random(seed)
real = [unicodedata.name(chr(c)) for c in range(0x30000) if unicodedata.name(chr(c), None)]
aliases = []
try:
    for raw in open("/usr/share/unicode/ucd/NameAliases.txt"):
        raw = raw.split("#")[0].strip()
        if raw:
            aliases.append(raw.split(";")[1])
    seqs = [raw.split(";")[0] for raw in open("/usr/share/unicode/ucd/NamedSequences.txt")
            if raw.strip() and not raw.startswith("#")]
except OSError:
    seqs = []


def variant(s):
    k = rng.random()
    if k < 0.3:
        return s.lower()
    if k < 0.4:
        return s.title()
    if k < 0.5:
        return s + rng.choice([" ", "X", "-1"])
    if k < 0.55:
        return s[:-1]
    return s


def name():
    k = rng.random()
    if k < 0.4:
        return variant(rng.choice(real))
    if k < 0.55 and aliases:
        return variant(rng.choice(aliases))
    if k < 0.62 and seqs:
        return variant(rng.choice(seqs))
    if k < 0.75:
        jam = ["G", "GG", "N", "", "A", "AE", "YA", "WA", "GS", "NG", "H", "EU", "YI", "I", "X"]
        return variant("HANGUL SYLLABLE " + "".join(rng.choice(jam) for _ in range(rng.randint(0, 3))))
    if k < 0.88:
        hexd = "".join(rng.choice("0123456789ABCDEFabcdefG") for _ in range(rng.randint(3, 6)))
        return rng.choice(["CJK UNIFIED IDEOGRAPH-", "cjk unified ideograph-"]) + rng.choice(
            [hexd, "4E00", "9FFF", "3400", "2A6DF", "323AF", "323B0", "A000", "4DC0"])
    return rng.choice(["", "EM DASH", "em dash", "x" * 300, "LATIN SMALL LETTER \ud800", "é", "NULL", "BEL",
                       "LATIN SMALL LETTER A\x00", "  EM DASH", "EM  DASH", "BYTE ORDER MARK", "BOM"])


with open(out, "w", encoding="utf-8") as f:
    for _ in range(n):
        nm = name()
        row = {"name": nm}
        try:
            re.compile("\\N{" + nm + "}")
            row["cp"] = _parser.parse("\\N{" + nm + "}")[0][1]
        except Exception as e:  # noqa: BLE001
            row["err"] = f"{type(e).__name__}: {e}"
        f.write(json.dumps(row) + "\n")

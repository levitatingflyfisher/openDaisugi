"""Write PyYAML's and load_config's answers for the pyyaml differential test.

    PYTHONPATH=src python oracle.py SEED N out.jsonl

Each line is {"text": ..., "yaml": dump or {"exc": type}, "config":
{"gate_mode", "verifier_client"} or {"exc": type}}. The texts are random
config files built from fragments, many of them broken on purpose.
"""

import json
import math
import random
import sys
import tempfile
from pathlib import Path

import yaml

from opendaisugi.config import load_config

KEYS = ["gate_mode", "verifier_client", "model", "max_task_chars", "z3_timeout_ms", "data_dir", "auto_tend",
        "gate_ask", "llm_context_window", "floor_report", "voice_cleanup", "unknown_key", "x", "true", "null",
        "yes", "on", "No", "_a", "floor", "shell_allow_decomposition", "llm_backend"]
VALUES = ["enforce", "shadow", "python", "go", "rust", "yes", "no", "on", "off", "True", "FALSE", "y", "n", "~",
          "null", "Null", "", "0", "1", "-1", "+7", "010", "0o10", "0x1F", "0b101", "1_000", "1:30", "08", "1.5",
          "1.", ".5", "1e5", "1.0e+5", "1.0e5", ".inf", "-.Inf", ".NaN", "1_0.5", "2024-01-01",
          "2024-1-1 10:00:00", "2024-02-30", "2023-02-29", "2024-02-29", "0000-01-01", "2024-01-01 25:00:00", "2024-01-01T10:00:00+24:00", "2024-01-01T10:00:00+23:59", "2024-01-01 10:00:60", "2024-1-1t1:00:00.5Z", "'quoted'", "'it''s'", '"dq"', '"a\\tb"', '"\\u00e9"', '"\\x41"', '"\\q"', "'unclosed",
          '"unclosed', "a b c", "http://x.test:80/a", "a: b", "a:", "[1, 2]", "{a: 1}", "- x", "&anchor x", "*ref",
          "!!str 5", "|", ">", "@x", "%x", "`x`", "a #comment", "a#b", "'x' # c", "'x'y", "=", "<<", "é", "中文",
          "?", "-", "-x", ":x", "#only", "1_", "0b", "0x", "._5", "+.inf", "12:61", "1:2:3", "0:0.5", "3.0",
          "10000000000000000000000", "-0", "+0", "0_0", "0.0", "-.5"]


def dump(v):
    if isinstance(v, bool) or v is None or isinstance(v, str):
        return v
    if isinstance(v, int):
        return {"int": str(v)}
    if isinstance(v, float):
        return {"float": "nan" if math.isnan(v) else repr(v)}
    if isinstance(v, dict):
        return {"dict": [[k if isinstance(k, str) else {"key": repr(k)}, dump(x)] for k, x in v.items()]}
    if isinstance(v, list):
        return [dump(x) for x in v]
    return {"other": type(v).__name__}


def line(rng):
    kind = rng.random()
    if kind < 0.1:
        return rng.choice(["", "   ", "# a comment", "  # indented comment", "---", "...", "%YAML 1.1", "\t",
                           "- a", "key", " key: v", "\x7f", "a\tb: c"])
    k = rng.choice(KEYS)
    if k == "floor" and rng.random() < 0.7:
        sub = [f"{rng.choice(['  ', '    ', '  '])}{rng.choice(['backend', 'notify_cmd', 'tmux_socket', 'x'])}: "
               f"{rng.choice(VALUES)}" for _ in range(rng.randint(0, 3))]
        return "\n".join([k + ":"] + sub)
    sep = rng.choice([": ", ": ", ":", ":  ", " : "])
    return k + sep + rng.choice(VALUES)


CLEAN = [v for v in VALUES if v and v[0] not in "[]{},#&*!|>%@`'\"?:-" and ": " not in v and not v.endswith(":")]
OUT_DIR = Path(".")


def clean_line(rng):
    k = rng.choice(KEYS)
    if k == "floor" and rng.random() < 0.7:
        sub = [f"  {rng.choice(['backend', 'notify_cmd', 'tmux_socket', 'x'])}: {rng.choice(CLEAN)}"
               for _ in range(rng.randint(1, 3))]
        return "\n".join([k + ":"] + sub)
    return k + ": " + rng.choice(CLEAN + ["'quoted'", '"dq"', "'it''s'", '"\\u00e9"'])


def saved(rng):
    """A file as save_config writes it."""
    from opendaisugi.config import Config, save_config

    fields = {"gate_mode": rng.choice(["shadow", "enforce", "on", "yes: no", "#x", "a b"]),
              "verifier_client": rng.choice(["python", "go", "rust", "", "- x", "été"]),
              "max_task_chars": rng.choice([1, 4000, 10 ** 20]),
              "auto_tend": rng.choice([None, True, False]),
              "model": rng.choice(["m", "x" * 100 + " " + "y" * 30, "a:b", "1.5", "null"])}
    tmp = Path(tempfile.mkdtemp(dir=OUT_DIR))
    try:
        save_config(Config(**fields), tmp / "c.yaml")
        return (tmp / "c.yaml").read_text()
    finally:
        (tmp / "c.yaml").unlink(missing_ok=True)
        tmp.rmdir()


def text(rng):
    kind = rng.random()
    if kind < 0.1:
        return saved(rng)
    if kind < 0.55:
        return "\n".join(clean_line(rng) for _ in range(rng.randint(1, 6))) + "\n"
    lines = [line(rng) for _ in range(rng.randint(0, 6))]
    t = "\n".join(lines)
    if rng.random() < 0.7:
        t += "\n"
    if rng.random() < 0.03:
        t = "﻿" + t
    if rng.random() < 0.05:
        t = t.replace("\n", "\r\n")
    return t


def main():
    global OUT_DIR
    seed, n, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
    OUT_DIR = Path(out).parent
    rng = random.Random(seed)
    tmp = Path(tempfile.mkdtemp(dir=Path(out).parent))
    cfg = tmp / "config.yaml"
    with open(out, "w", encoding="utf-8") as f:
        for _ in range(n):
            t = text(rng)
            row = {"text": t}
            try:
                # load_config reads with universal newlines.
                row["yaml"] = dump(yaml.safe_load(t.replace("\r\n", "\n").replace("\r", "\n")))
            except Exception as exc:  # noqa: BLE001
                row["yaml"] = {"exc": type(exc).__name__}
            cfg.write_bytes(t.encode("utf-8", "surrogatepass"))
            try:
                c = load_config(cfg)
                row["config"] = {"gate_mode": c.gate_mode, "verifier_client": c.verifier_client}
            except Exception as exc:  # noqa: BLE001
                row["config"] = {"exc": type(exc).__name__}
            f.write(json.dumps(row) + "\n")
    cfg.unlink()
    tmp.rmdir()


main()

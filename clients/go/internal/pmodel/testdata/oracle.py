"""Write pydantic's answers for pmodel's differential test.

    PYTHONPATH=src python oracle.py SEED N out.jsonl

Each line is {"kind": "envelope"|"expr", "input": text-or-value,
"ok": dump} or {"kind", "input", "err": str(exc)}. Envelope inputs are
JSON texts, many of them broken or ill-typed on purpose; expr inputs are
values handed to parse_expression.
"""

import json
import math
import random
import sys

from opendaisugi.models import Envelope
from opendaisugi.predicate import parse_expression

JUNK = [True, False, 0, 1, 2, -1, 1.0, 0.0, 1.5, 30.0, 1e20, "true", "yes", "no", "off", "1", "0", "x", "",
        " 30 ", "30.0", "1_000", "3e1", "nan", None, [], {}, [1], ["a"], {"a": 1}, 10**20, "low", "strict"]


def dump(v):
    """Canonical JSON of a model dump: tuples as lists, floats by repr."""
    if isinstance(v, bool) or v is None or isinstance(v, str):
        return v
    if isinstance(v, int):
        return {"int": str(v)}
    if isinstance(v, float):
        return {"float": repr(v)}
    if isinstance(v, (list, tuple)):
        return [dump(x) for x in v]
    if isinstance(v, dict):
        return {"dict": [[k, dump(x)] for k, x in v.items()]}
    return repr(v)


def expr(rng, depth=0):
    ops = ["equals", "not_equals", "in_set", "not_in_set", "matches", "not_matches", "numeric_range",
           "length_range", "exists", "is_empty", "and", "or", "not", "implies", "forall_steps",
           "exists_step", "forall_outputs", "depends_on", "before", "alias", "llm_check"]
    if rng.random() < 0.05:
        return rng.choice(JUNK)
    op = rng.choice(ops + ["nope"]) if rng.random() < 0.95 else None
    d = {} if op is None else {"op": op}
    fields = {
        "path": lambda: rng.choice(["command", "path", "a.b", 5]),
        "value": lambda: rng.choice(JUNK),
        "values": lambda: rng.choice([[1, 2], ["a"], [], "x", [True]]),
        "regex": lambda: rng.choice(["^a", "x", 5, "[a-"]),
        "min": lambda: rng.choice(JUNK),
        "max": lambda: rng.choice(JUNK),
        "children": lambda: [expr(rng, depth + 1) for _ in range(rng.randint(0, 2))] if depth < 3 else [],
        "child": lambda: expr(rng, depth + 1) if depth < 3 else {"op": "exists", "path": "a"},
        "a": lambda: expr(rng, depth + 1) if depth < 3 else {"op": "exists", "path": "a"},
        "b": lambda: expr(rng, depth + 1) if depth < 3 else {"op": "exists", "path": "a"},
        "pred": lambda: expr(rng, depth + 1) if depth < 3 else {"op": "exists", "path": "a"},
        "step_id_a": lambda: rng.choice(["s0", 1]),
        "step_id_b": lambda: rng.choice(["s1", None]),
        "name": lambda: rng.choice(["n", 3]),
        "args": lambda: rng.choice([{}, {"k": 1}, "x"]),
        "rule": lambda: rng.choice(["r", None]),
    }
    for k in rng.sample(list(fields), rng.randint(0, 4)):
        d[k] = fields[k]()
    return d


def envelope(rng):
    env = {"generated_by": "g", "task": "t", "permissions": {}}
    if rng.random() < 0.1:
        env.pop(rng.choice(["generated_by", "task", "permissions"]))
    perm_fields = ["file_read", "file_write", "network", "network_hosts", "shell", "shell_allowlist",
                   "shell_allow_decomposition", "mcp_allowlist", "custom_step_allowlist", "max_execution_time_s",
                   "max_output_size_mb", "workspace_bounds", "obstacles", "velocity_limit", "joint_limits",
                   "torque_limit"]
    good = {
        "workspace_bounds": [[0, 0, 0], [1, 2.5, 3]], "obstacles": [[[0, 0, 0], [1, 1, 1]]],
        "joint_limits": {"j1": [-1, 1]}, "velocity_limit": 2.5, "torque_limit": 1,
    }
    if isinstance(env.get("permissions"), dict):
        for k in rng.sample(perm_fields, rng.randint(0, 4)):
            env["permissions"][k] = good.get(k, ["/a/**"]) if rng.random() < 0.3 else rng.choice(JUNK + [
                [[0, 0, 0], [1, 1, 1], [2, 2, 2]], [[0, 0], [1, 1, 1]], {"j": [0, 1, 2]}, {"j": "x"}])
    top = ["id", "invariants", "postconditions", "fallback", "parent_envelope", "tightening_only", "summary",
           "cache_key", "stakes", "shell_interpreter_policy"]
    for k in rng.sample(top, rng.randint(0, 3)):
        if k == "invariants":
            env[k] = [rng.choice([{"type": "t", "description": "d", "expr": expr(rng)}, {"type": "t"}, "x",
                                  {"type": "t", "description": "d", "enforce": rng.choice(JUNK)}])]
        elif k == "postconditions":
            env[k] = [rng.choice([{"type": "t", "expected": rng.choice(JUNK)}, {"type": 5}])]
        elif k == "summary":
            env[k] = rng.choice(["s" * 80, "s" * 81, 5, None])
        else:
            env[k] = rng.choice(JUNK)
    text = json.dumps(env)
    r = rng.random()
    if r < 0.05:
        text = text[: rng.randint(0, len(text))]
    elif r < 0.08:
        i = rng.randint(0, len(text))
        text = text[:i] + rng.choice([",", "]", "}", "x", "\x01", "\\", '"', "1e", "NaN", " "]) + text[i:]
    return text


def main():
    seed, n = int(sys.argv[1]), int(sys.argv[2])
    rng = random.Random(seed)
    with open(sys.argv[3], "w", encoding="utf-8") as out:
        for _ in range(n):
            text = envelope(rng)
            try:
                e = Envelope.model_validate_json(text)
                d = e.model_dump()
                d.pop("id") if not ('"id"' in text) else None
                out.write(json.dumps({"kind": "envelope", "input": text, "ok": dump(d)}) + "\n")
            except Exception as exc:  # noqa: BLE001 - recorded
                out.write(json.dumps({"kind": "envelope", "input": text, "err": str(exc)}) + "\n")
            x = expr(rng)
            try:
                p = parse_expression(x)
                out.write(json.dumps({"kind": "expr", "input": x, "ok": dump(p.model_dump())}) + "\n")
            except Exception as exc:  # noqa: BLE001 - recorded
                out.write(json.dumps({"kind": "expr", "input": x, "err": str(exc)}) + "\n")


if __name__ == "__main__":
    main()

"""Synthetic alias cases: the alias registry (`aliases.AliasRegistry`) and the
shipped system aliases (`system_aliases`), run through the Python oracle.

    uv run --no-sync python clients/alias_cases.py [--out clients/fixtures/alias]

No command of the oracle builds an alias registry; only the library calls
`integrations.hermes.envelope_from_yaml` passes one to `verify`. So the
cases drive the registry itself: each case registers aliases in order
(after the system aliases when `system` is set) and resolves expressions,
and records what each step gave: "ok", the resolved expression as
`model_dump(mode="json")` gives it, or the exception's class and text. The
ports answer the same cases with a probe (`alias-probe`) that reads the
cases file and writes one result line per case (clients/alias_compare.py).
Every name and expression is synthetic.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "alias"
CASE_VERSION = 1


def run_case(case: dict[str, Any]) -> dict[str, Any]:
    """The oracle's answer for one case."""
    from opendaisugi.aliases import Alias, AliasRegistry
    from opendaisugi.predicate import parse_expression
    from opendaisugi.system_aliases import load_system_aliases

    def caught(fn) -> dict[str, Any]:
        try:
            return {"ok": fn()}
        except Exception as e:  # noqa: BLE001 - the case records it
            return {"error": type(e).__name__, "message": str(e)}

    reg = AliasRegistry()
    out: dict[str, Any] = {}
    if case.get("system"):
        out["system"] = caught(lambda: load_system_aliases(reg))
    out["register"] = [
        caught(lambda a=a: reg.register(Alias(**a))) for a in case.get("register", [])
    ]
    out["resolve"] = [
        caught(lambda e=e: reg.resolve(parse_expression(e)).model_dump(mode="json"))
        for e in case.get("resolve", [])
    ]
    out["lookup"] = [
        caught(lambda n=n: reg.lookup(n).model_dump(mode="json")) for n in case.get("lookup", [])
    ]
    return out


def ref(name: str, **args: Any) -> dict[str, Any]:
    return {"op": "alias", "name": name, "args": args}


def alias(name: str, expr: Any, *, params=(), tier="household", description="") -> dict[str, Any]:
    return {
        "name": name,
        "params": list(params),
        "expr": expr,
        "tier": tier,
        "description": description,
    }


EQ_TYPE = {"op": "equals", "path": "type", "value": "shell"}


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def add(name: str, **kw: Any) -> None:
        C.append({"name": name, **kw})

    # The shipped system aliases, resolved.
    for n in ("no_secrets", "no_pii_regex", "pytest_passes", "no_network_writes"):
        add(f"system {n}", system=True, resolve=[ref(n)])
    add("system structured approval", system=True, resolve=[ref("structured_approval")])
    add(
        "system never impersonates",
        system=True,
        resolve=[
            ref("never_impersonates", principal="Ann"),
            ref("never_impersonates", principal=".*"),
            ref("never_impersonates", principal="a$principal b"),
            ref("never_impersonates", principal=7),
            ref("never_impersonates"),
        ],
    )
    add(
        "system velocity scale",
        system=True,
        resolve=[
            ref("velocity_scale_bounded", max_scale=0.5),
            ref("velocity_scale_bounded", max_scale=1),
            ref("velocity_scale_bounded", max_scale="0.25"),
            ref("velocity_scale_bounded", max_scale="fast"),
            ref("velocity_scale_bounded", max_scale=None),
        ],
    )
    add("system twice", system=True, register=[alias("no_secrets", EQ_TYPE, tier="system")])
    add(
        "system lookup",
        system=True,
        lookup=["no_secrets", "velocity_scale_bounded", "missing"],
    )
    add("unknown alias", resolve=[ref("nope"), {"op": "not", "child": ref("nope2")}])
    add("no registry entries", resolve=[EQ_TYPE, {"op": "and", "children": [EQ_TYPE]}])

    # Registration checks.
    add(
        "system name taken",
        system=True,
        register=[
            alias("no_secrets", EQ_TYPE),
            alias("no_secrets", EQ_TYPE, tier="envelope"),
        ],
    )
    add(
        "system name after household",
        register=[alias("x", EQ_TYPE), alias("x", EQ_TYPE, tier="system")],
    )
    add(
        "no path reference",
        register=[
            alias("empty_and", {"op": "and", "children": []}),
            alias("dep", {"op": "depends_on", "step_id_a": "a", "step_id_b": "b"}),
            alias("raw", "just text"),
            alias("no_op", {"path": "type"}),
            alias("deep", {"op": "not", "child": {"op": "implies", "a": EQ_TYPE, "b": EQ_TYPE}}),
        ],
    )
    exists = {"op": "exists", "path": "a"}
    add(
        "vacuous bodies",
        register=[
            alias("taut", {"op": "or", "children": [exists, {"op": "not", "child": exists}]}),
            alias("contra", {"op": "and", "children": [exists, {"op": "not", "child": exists}]}),
            alias("fine", exists),
        ],
    )
    add(
        "invalid bodies",
        register=[
            alias("bad", {"op": "numeric_range", "path": "x", "min": "lo", "max": 1}),
            alias("bad_op", {"op": "zz", "path": "x"}),
            alias(
                "typed_placeholder",
                {"op": "numeric_range", "path": "x", "min": 0, "max": "$hi"},
                params=["hi"],
            ),
            alias(
                "placeholder_not_a_param",
                {"op": "numeric_range", "path": "x", "min": 0, "max": "$hi"},
                params=["lo"],
            ),
        ],
        resolve=[ref("typed_placeholder", hi=3), ref("typed_placeholder", hi="many")],
    )

    # Resolution.
    add(
        "nested aliases",
        system=True,
        register=[
            alias(
                "family",
                {"op": "and", "children": [ref("no_secrets"), ref("no_pii_regex")]},
                description="the family's floor",
            )
        ],
        resolve=[
            ref("family"),
            {"op": "implies", "a": EQ_TYPE, "b": ref("family")},
            {"op": "forall_writes", "pred": ref("no_secrets")},
            {"op": "exists_step", "pred": ref("pytest_passes")},
            {"op": "forall_outputs", "pred": ref("no_secrets")},
        ],
    )
    add(
        "cycle",
        register=[alias("a", ref("b")), alias("b", ref("a")), alias("c", ref("c"))],
        resolve=[ref("a"), ref("c"), {"op": "or", "children": [ref("b")]}],
    )
    add(
        "nested unknown",
        register=[alias("outer", {"op": "and", "children": [EQ_TYPE, ref("inner")]})],
        resolve=[ref("outer")],
    )
    add(
        "tier precedence",
        register=[
            alias("pick", {"op": "equals", "path": "type", "value": "household"}),
            alias("pick", {"op": "equals", "path": "type", "value": "envelope"}, tier="envelope"),
            alias("pick", {"op": "equals", "path": "type", "value": "again"}),
        ],
        resolve=[ref("pick")],
        lookup=["pick"],
    )
    two = alias(
        "two",
        {
            "op": "and",
            "children": [
                {"op": "equals", "path": "metadata.signature", "value": "$p_name"},
                {"op": "equals", "path": "metadata.who", "value": "$p and $p_name and $p$p"},
                {"op": "equals", "path": "metadata.whole", "value": "$p"},
                {"op": "matches", "path": "metadata.body", "regex": "^$p_name-$p$"},
                {"op": "in_set", "path": "type", "values": ["$p", "x$p"]},
            ],
        },
        params=["p", "p_name"],
    )
    add(
        "substitution",
        register=[two],
        resolve=[
            ref("two", p="A", p_name="B"),
            ref("two", p="$p_name", p_name="(x|y)+"),
            ref("two", p=["l", 1], p_name={"k": True}),
            ref("two", p=1.5, p_name=False, extra="unused"),
            ref("two", p="a.b"),
        ],
    )
    add(
        "regex escapes",
        register=[
            alias(
                "esc",
                {"op": "matches", "path": "metadata.body", "regex": "$v"},
                params=["v"],
            )
        ],
        resolve=[
            ref("esc", v="()[]{}?*+-|^$\\.&~# \t\n\r\v\f"),
            ref("esc", v="plain_word-1"),
            ref("esc", v="é ü"),
        ],
    )
    add(
        "args in the reference",
        register=[alias("plain", EQ_TYPE)],
        resolve=[ref("plain", unused=1), {"op": "alias", "name": "plain"}],
    )
    return C


def canonical_json(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def case_id(c: dict[str, Any]) -> str:
    body = {k: v for k, v in c.items() if k not in ("id", "expect")}
    return hashlib.sha256(canonical_json(body).encode()).hexdigest()[:16]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    sys.path.insert(0, str(REPO / "src"))
    cases = build_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    for c in cases:
        c["id"] = case_id(c)
        c["expect"] = run_case(c)
    cases.sort(key=lambda c: c["id"])
    args.out.mkdir(parents=True, exist_ok=True)
    data = "".join(canonical_json(c) + "\n" for c in cases).encode("utf-8")
    (args.out / "cases.jsonl").write_bytes(data)
    manifest = {
        "v": CASE_VERSION,
        "cases.jsonl": {"count": len(cases), "sha256": hashlib.sha256(data).hexdigest()},
    }
    (args.out / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"wrote {len(cases)} cases to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

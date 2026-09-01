"""Synthetic stage-K1 cases: envelope generation (Tier-0 pathways, the
Tier-1 slot, the envelope cache, the Tier-2 ladder with refinement hints),
inheritance, pathway binding, composition, and recall's bind path, run
through the Python oracle.

    uv run --no-sync python clients/k1_cases.py [--out clients/fixtures/k1] [--only NAME]

Two kinds of case:

- cli: `daisugi generate-envelope ...`, run as garden_cases runs a
  command: a scratch HOME, the fake `claude` and the fake model server
  answering only exact request bytes, and the exit code, stdout, stderr,
  tree after and model requests recorded.
- probe: one query of the library, the argv the oracle's script below
  (ORACLE) and the binary's instrument (cmd/envelope-probe) both take.
  The rest is as for cli. Nothing but these probes reaches the Tier-0,
  Tier-1 and cache paths yet: the orchestrator and the MCP server that
  call them are later stages.

The fakes, the tree layout, the normalization and the reply recording are
garden_cases'. This file adds one layout, "envcache" (an envelope cache
with rows given relative to the time it is laid out). Every path, key and
task is synthetic.
"""

from __future__ import annotations

import argparse
import copy
import json
import os
import shutil
import sqlite3
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
from garden_cases import (  # noqa: E402
    PY_CLI,
    body_id,
    make_answer,
    run_case,
    write_jsonl,
)
from pathway_cases import REPO, envelope, lexical, pathway, put_row, row_spec  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "k1"
SCRATCH = Path(
    os.environ.get("DAISUGI_K1_SCRATCH") or Path.home() / "opendaisugi-scratch" / "k1" / "runs"
)
CASE_VERSION = 1
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
KEY = {"ANTHROPIC_API_KEY": "sk-test-k1-000000000000"}
API = {**KEY, "OPENDAISUGI_LLM_BACKEND": "api"}
CC = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}
SONNET = "anthropic/claude-sonnet-4-20250514"

ORACLE = r"""
import asyncio, json, sys, warnings
from pathlib import Path
q = json.loads(sys.argv[1])

def err(e):
    n = type(e).__name__
    return {"error": n, "message": "" if n == "ValidationError" else str(e)}

def env_of(d):
    from opendaisugi.models import Envelope
    return Envelope.model_validate(d)

def generate():
    from opendaisugi.envelope import ENVELOPE_PROMPT_VERSION, generate_envelope
    from opendaisugi.envelope_cache import EnvelopeCache
    kw = {"stakes": q.get("stakes") or "medium", "thinking_budget": q.get("thinking") or "standard",
          "summarize": q.get("summarize", False)}
    if q.get("context") is not None:
        kw["context"] = q["context"]
    if q.get("model") is not None:
        kw["model"] = q["model"]
    for k in ("max_retries", "max_task_chars"):
        if q.get(k) is not None:
            kw[k] = q[k]
    if q.get("low_stakes") is not None:
        kw["low_stakes_envelope"] = env_of(q["low_stakes"])
    if q.get("parent") is not None:
        kw["parent"] = env_of(q["parent"])
    if q.get("cache") is not None:
        kw["cache"] = EnvelopeCache(q["cache"], prompt_version=ENVELOPE_PROMPT_VERSION)
    if q.get("pathways") is not None:
        from opendaisugi.pathway_store import PathwayStore
        kw["pathway_store"] = PathwayStore(Path(q["pathways"]))
        if q.get("threshold") is not None:
            kw["pathway_threshold"] = q["threshold"]
    if q.get("journal") is not None:
        from opendaisugi.journal import Journal
        kw["journal"] = Journal(data_dir=Path(q["journal"]))
    t = q.get("tier1")
    if isinstance(t, str):
        from opendaisugi.local_setup import load_configured_tier1
        kw["tier1"] = load_configured_tier1(t)
    elif t is not None:
        from opendaisugi.tier1 import HTTPTier1Provider
        import os
        base = os.environ[t["base_url_env"]] if t.get("base_url_env") else t.get("base_url")
        kw["tier1"] = HTTPTier1Provider(t["model"], base_url=base, api_key=t.get("api_key"),
                                        name=t.get("name") or None)
    env = asyncio.run(generate_envelope(q["task"], **kw))
    return {"envelope": env.model_dump(mode="json")}

def inherit():
    from opendaisugi.inheritance import verify_inheritance
    return {"violations": [v.message for v in verify_inheritance(env_of(q["child"]), env_of(q["parent"]))]}

def bind():
    from opendaisugi.pathway_bind import bind_parameters
    from opendaisugi.pathway_store import PathwayStore
    p = PathwayStore(Path(q["db"])).get(q["id"])
    plan = asyncio.run(bind_parameters(p, q["task"], envelope=env_of(q["envelope"]),
                                       model=q.get("model") or "anthropic/claude-sonnet-4-20250514",
                                       z3_timeout_ms=q["z3_timeout_ms"]))
    return {"plan": plan.model_dump(mode="json")}

class Echo:
    def run(self, s, timeout_s, max_output_bytes):
        f = {"shell": "command", "file_read": "path", "file_write": "path", "network": "url"}.get(s.type)
        class R:
            pass
        r = R()
        r.stdout = f"{s.type}:{getattr(s, f) if f else ''}"
        return r

def compose():
    from opendaisugi.compose import pathway_contract_envelopes, pathway_skill_handlers_for
    from opendaisugi.models import ActionPlan
    from opendaisugi.pathway_store import PathwayStore
    from opendaisugi.verify import verify
    store = PathwayStore(Path(q["db"]))
    contracts = pathway_contract_envelopes(store)
    handlers = pathway_skill_handlers_for(store, set(q["skill_ids"]), executors={t: Echo() for t in q["executors"]})
    runs = {}
    for k in sorted(handlers):
        try:
            runs[k] = handlers[k](None)
        except Exception as e:
            runs[k] = err(e)
    plan = q["plan"]
    for s in plan["steps"]:
        if s.get("type") == "skill" and s["skill_id"] in contracts:
            s["contract_envelope"] = contracts[s["skill_id"]].model_dump(mode="json")
    res = verify(ActionPlan.model_validate(plan), env_of(q["envelope"]), z3_timeout_ms=q["z3_timeout_ms"])
    vs = [[v.stage, None if v.stage == "delegation" else v.message] for v in res.violations]
    return {"contracts": sorted(contracts), "handlers": sorted(handlers), "runs": runs, "ok": res.ok,
            "violations": vs}

def recall():
    from dataclasses import asdict
    from opendaisugi import gateway_recall
    from opendaisugi.pathway_store import PathwayStore
    r = asyncio.run(gateway_recall.recall(q["task"], env_of(q["envelope"]), pathway_store=PathwayStore(Path(q["db"])),
                                          z3_timeout_ms=q["z3_timeout_ms"]))
    return {"hit": r.hit, "reason": r.reason,
            "plan": r.plan.model_dump(mode="json") if r.plan is not None else None,
            "provenance": asdict(r.provenance) if r.provenance is not None else None}

with warnings.catch_warnings(record=True) as ws:
    warnings.simplefilter("always")
    try:
        out = {"generate": generate, "inherit": inherit, "bind": bind, "compose": compose, "recall": recall}[q["kind"]]()
    except Exception as e:
        out = err(e)
if q["kind"] == "generate":
    out["warnings"] = [str(w.message) for w in ws if w.category is UserWarning]
print(json.dumps(out))
"""
ORACLE_CMD = [sys.executable, "-c", ORACLE]


# ---------------------------------------------------------------------------
# The envelope-cache layout
# ---------------------------------------------------------------------------

_garden_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """garden_cases.lay_out, and an "envcache" entry: an envelope cache
    whose rows name their inserted_at relative to t0 ({"now": -3600})."""
    rest = {k: v for k, v in tree.items() if "envcache" not in v}
    _garden_lay_out(rest, home, t0)
    from opendaisugi.envelope_cache import _SCHEMA

    for rel, spec in sorted(tree.items()):
        if "envcache" not in spec:
            continue
        p = home / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        con = sqlite3.connect(p)
        con.executescript(_SCHEMA)
        for r in spec["envcache"]:
            at = r["inserted_at"]
            at = t0 + float(at["now"]) if isinstance(at, dict) else at
            con.execute(
                "INSERT INTO envelope_cache VALUES (?, ?, ?, ?)",
                (r["cache_key"], r["prompt_version"], r["envelope_json"], at),
            )
        con.commit()
        con.close()


# run_case and the compare's before-tree read the module's lay_out.
garden_cases.lay_out = lay_out


def cmd_for(case: dict[str, Any], binary: str | None, probe: str | None) -> list[str]:
    """The command a case runs: the oracle's when binary is None."""
    if case["kind"] == "probe":
        return ORACLE_CMD if probe is None else [probe]
    return PY_CLI if binary is None else [binary]


def record_replies(case: dict[str, Any], work: Path) -> None:
    """garden_cases.record_replies for either kind of case."""
    table: dict[str, Any] = {}
    replies = list(case["replies"])
    cmd = cmd_for(case, None, None)
    while True:
        case["model"] = table
        raw: list[dict[str, Any]] = []
        run_case(case, cmd, work, raw)
        missing = [r for r in raw if r["key"] not in table]
        if not missing or not replies:
            break
        kind, ans = make_answer(replies.pop(0))
        if kind != missing[0]["kind"]:
            raise SystemExit(f"{case['name']}: a {kind} reply for a {missing[0]['kind']} request")
        table[missing[0]["key"]] = ans
    if replies:
        print(f"WARNING {case['name']}: {len(replies)} reply spec(s) never asked for", flush=True)
    case["model"] = table


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def env_dict(i: int = 1, **perm: Any) -> dict[str, Any]:
    return envelope(i, **perm).model_dump(mode="json")


def reply_env(task: str = "t", **perm: Any) -> str:
    """A model's envelope reply: the fields a model writes."""
    p = {
        "file_read": ["/work/**"],
        "file_write": [],
        "network": False,
        "shell": False,
        "shell_allowlist": [],
        "max_execution_time_s": 30,
        "max_output_size_mb": 10,
    }
    p.update(perm)
    return json.dumps(
        {
            "generated_by": "model",
            "task": task,
            "permissions": p,
            "invariants": [],
            "postconditions": [],
        }
    )


def cache_key(task: str, model: str = SONNET, **kw: Any) -> str:
    from opendaisugi.envelope_cache import make_cache_key

    args = {
        "task": task,
        "context": None,
        "model": model,
        "parent_envelope_id": None,
        "summarize": False,
        "thinking_budget": "standard",
    }
    args.update(kw)
    return make_cache_key(**args)


def cache_row(key: str, env: dict[str, Any], *, at: Any = None, version: str | None = None) -> dict:
    from opendaisugi.envelope import ENVELOPE_PROMPT_VERSION
    from opendaisugi.models import Envelope

    return {
        "cache_key": key,
        "prompt_version": version or ENVELOPE_PROMPT_VERSION,
        "envelope_json": Envelope.model_validate(env).model_dump_json(),
        "inserted_at": at if at is not None else {"now": -3600},
    }


def refinement(key: str, ts: float, msgs: list[tuple[str, str]], session: str = "run1") -> dict:
    return {
        "session": session,
        "record": {
            "step": {"id": "s1", "type": "shell", "command": "rm -rf /work"},
            "violations": [{"stage": st, "message": m} for st, m in msgs],
            "z3_counterexample": None,
            "envelope_id": "env_00000001",
            "fallback_action": "halted",
            "timestamp": ts,
            "cache_key": key,
        },
    }


def journal(refs: list[dict]) -> dict:
    return {".opendaisugi": {"journal": {"traces": [], "refinements": refs}}}


def db_of(*pws) -> dict:
    return {".opendaisugi/pathways.db": {"db": {"rows": [row_spec(put_row(p)) for p in pws]}}}


def file_plan(path: str = "/work/a.txt", i: int = 1):
    from opendaisugi.models import ActionPlan, FileReadStep, ShellStep

    return ActionPlan(
        id=f"plan_{i:08x}",
        source="script",
        task=f"task {i}",
        steps=[
            ShellStep(id="s1", command="make test"),
            FileReadStep(id="s2", path=path, depends_on=["s1"]),
        ],
    )


def param(name: str, idx: int, sid: str, field: str, head: str, observed=("/work/out.txt",)):
    from opendaisugi.pathway import PathwayParameter

    return PathwayParameter(
        name=name, step_index=idx, step_id=sid, field=field, head=head, observed=list(observed)
    )


def bind_json(**values: str) -> str:
    return json.dumps({"values": values})


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------


def probe(name: str, query: dict[str, Any], *, before=None, env=None, replies=None, **kw):
    c = {"kind": "probe", "name": name, "argv": [json.dumps(query)], "before": before or {}}
    if env:
        c["env"] = env
    if replies:
        c["replies"] = replies
    c.update(kw)
    return c


def gen(task: str, **kw: Any) -> dict[str, Any]:
    return {"kind": "generate", "task": task, **kw}


def build_generate_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    T = "Read /work/a.txt and count its lines"
    good = reply_env(T)
    # Tier-2, one rung.
    add(probe("gen api one rung", gen(T), env=API, replies=[{"http": good}]))
    add(probe("gen claude-code one rung", gen(T), env=CC, replies=[{"claude": good}]))
    add(
        probe(
            "gen api inconsistent",
            gen(T),
            env=API,
            replies=[{"http": reply_env(T, shell_allowlist=["ls"])}],
        )
    )
    add(
        probe(
            "gen api time limit zero",
            gen(T),
            env=API,
            replies=[{"http": reply_env(T, max_execution_time_s=0)}],
        )
    )
    add(
        probe(
            "gen api file_exists without write",
            gen(T),
            env=API,
            replies=[
                {
                    "http": json.dumps(
                        {
                            "generated_by": "m",
                            "task": T,
                            "permissions": {"file_read": ["/w"]},
                            "postconditions": [{"type": "file_exists", "path": "/w/x"}],
                        }
                    )
                }
            ],
        )
    )
    add(
        probe(
            "gen api reask then good",
            gen(T, max_retries=1),
            env=API,
            replies=[{"http": "not json"}, {"http": good}],
        )
    )
    add(
        probe(
            "gen api reask exhausted",
            gen(T, max_retries=1),
            env=API,
            replies=[{"http": '{"task": 1}'}, {"http": "{}"}],
        )
    )
    add(
        probe(
            "gen api nan reply",
            gen(T, max_retries=0),
            env=API,
            replies=[
                {
                    "http": json.dumps(
                        {
                            "generated_by": "m",
                            "task": T,
                            "permissions": {"max_output_size_mb": 10},
                            "invariants": [],
                        }
                    ).replace("10}", "NaN}")
                }
            ],
        )
    )
    add(
        probe(
            "gen api http 500",
            gen(T),
            env=API,
            replies=[{"http_status": 500, "body": '{"error": "boom"}'}],
        )
    )
    add(probe("gen api cut", gen(T), env=API, replies=[{"http": good, "stop": "max_tokens"}]))
    add(
        probe(
            "gen claude-code is_error", gen(T), env=CC, replies=[{"claude_is_error": "overloaded"}]
        )
    )
    add(
        probe(
            "gen claude-code reask",
            gen(T, max_retries=2),
            env=CC,
            replies=[{"claude": "no json here"}, {"claude": '{"x": 1}'}, {"claude": good}],
        )
    )
    add(
        probe(
            "gen summarize and context",
            gen(T, context="the repo is small", summarize=True),
            env=API,
            replies=[{"http": json.dumps(json.loads(good) | {"summary": "count lines"})}],
        )
    )
    add(
        probe(
            "gen non-ascii task",
            gen("Lire /work/é.txt, compter"),
            env=API,
            replies=[{"http": reply_env("Lire /work/é.txt, compter")}],
        )
    )
    # Thinking kwargs.
    add(
        probe(
            "gen thinking deep anthropic",
            gen(T, thinking="deep"),
            env=API,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen thinking light anthropic",
            gen(T, thinking="light"),
            env=API,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen thinking deep claude prefix",
            gen(T, thinking="deep", model="claude-3-haiku"),
            env=API,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen thinking deep claude3 no dash",
            gen(T, thinking="deep", model="claude3"),
            env=API,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen thinking deep upper model",
            gen(T, thinking="deep", model="Anthropic/Claude-X"),
            env=API,
            replies=[{"http": good}],
        )
    )
    for b in ("light", "standard", "deep"):
        add(
            probe(
                f"gen reasoning openai o3 {b}",
                gen(T, thinking=b, model="openai/o3-mini"),
                env={
                    **API,
                    "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1",
                    "OPENAI_API_KEY": "sk-oa-k1-00000000000",
                },
                replies=[{"chat": good}],
            )
        )
    add(
        probe(
            "gen gemini thinking passes nothing",
            gen(T, thinking="deep", model="openai/gemini-2.5-pro"),
            env={**API, "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1"},
            replies=[{"chat": good}],
        )
    )
    add(
        probe(
            "gen thinking deep claude-code",
            gen(T, thinking="deep"),
            env=CC,
            replies=[{"claude": good}],
        )
    )
    # Ladders.
    ladder = [SONNET, "anthropic/claude-opus-4-8"]
    add(
        probe(
            "gen ladder second rung",
            gen(T, model=ladder),
            env=API,
            replies=[{"http": reply_env(T, shell_allowlist=["ls"])}, {"http": good}],
        )
    )
    add(
        probe(
            "gen ladder exhausted",
            gen(T, model=ladder),
            env=API,
            replies=[{"http_status": 500, "body": "{}"}, {"http_status": 529, "body": "{}"}],
        )
    )
    add(
        probe(
            "gen ladder exhausted inconsistent",
            gen(T, model=ladder),
            env=API,
            replies=[
                {"http_status": 500, "body": "{}"},
                {"http": reply_env(T, shell_allowlist=["ls"])},
            ],
        )
    )
    add(probe("gen ladder empty", gen(T, model=[]), env=API))
    add(
        probe(
            "gen ladder no wire rung",
            gen(T, model=["mystery-model", SONNET]),
            env=API,
            replies=[{"http": good}],
        )
    )
    # Preflight: no key, no claude.
    add(probe("gen api no key", gen(T), env={"OPENDAISUGI_LLM_BACKEND": "api"}))
    add(probe("gen claude-code no binary", gen(T), env=CC, no_claude=True))
    add(
        probe(
            "gen ladder second rung no key",
            gen(T, model=["openai/m", SONNET]),
            env={"OPENDAISUGI_LLM_BACKEND": "api", "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1"},
            replies=[{"http_status": 500, "body": "{}"}],
        )
    )
    add(probe("gen renamed backend", gen(T), env={"OPENDAISUGI_LLM_BACKEND": "litellm"}))
    # Input checks and stakes.
    add(probe("gen empty task", gen("   "), env=API))
    add(probe("gen too long", gen("x" * 30, context="y" * 11, max_task_chars=40), env=API))
    add(
        probe(
            "gen exactly at limit",
            gen("x" * 30, context="y" * 10, max_task_chars=40),
            env=API,
            replies=[{"http": reply_env("x")}],
        )
    )
    add(probe("gen too long non-ascii", gen("é" * 21, max_task_chars=20), env=API))
    add(probe("gen low no envelope", gen(T, stakes="low"), env=API))
    add(probe("gen low envelope", gen(T, stakes="low", low_stakes=env_dict(7)), env=API))
    # The cache.
    cpath = "{HOME}/.opendaisugi/envelope_cache.db"
    cenv = env_dict(3)
    k = cache_key(T)
    add(probe("gen cache miss writes", gen(T, cache=cpath), env=API, replies=[{"http": good}]))
    add(
        probe(
            "gen cache hit",
            gen(T, cache=cpath),
            env=API,
            before={".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k, cenv)]}},
        )
    )
    add(
        probe(
            "gen cache hit keeps its key",
            gen(T, cache=cpath),
            env=API,
            before={
                ".opendaisugi/envelope_cache.db": {
                    "envcache": [cache_row(k, {**cenv, "cache_key": "abc"})]
                }
            },
        )
    )
    add(
        probe(
            "gen cache stale version evicted",
            gen(T, cache=cpath),
            env=API,
            replies=[{"http": good}],
            before={
                ".opendaisugi/envelope_cache.db": {
                    "envcache": [cache_row(k, cenv, version="2020-01-01"), cache_row("other", cenv)]
                }
            },
        )
    )
    add(
        probe(
            "gen cache high stakes skips",
            gen(T, cache=cpath, stakes="high"),
            env=API,
            replies=[{"http": good}],
            before={".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k, cenv)]}},
        )
    )
    add(
        probe(
            "gen cache other thinking misses",
            gen(T, cache=cpath, thinking="deep"),
            env=API,
            replies=[{"http": good}],
            before={".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k, cenv)]}},
        )
    )
    k2 = cache_key(T, model="anthropic/claude-opus-4-8")
    add(
        probe(
            "gen cache second rung hit",
            gen(T, cache=cpath, model=ladder),
            env=API,
            before={".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k2, cenv)]}},
        )
    )
    add(
        probe(
            "gen cache nan row raises",
            gen(T, cache=cpath),
            env=API,
            before={
                ".opendaisugi/envelope_cache.db": {
                    "envcache": [
                        cache_row(k, cenv)
                        | {
                            "envelope_json": cache_row(k, cenv)["envelope_json"].replace(
                                '"max_output_size_mb":10', '"max_output_size_mb":NaN'
                            )
                        }
                    ]
                }
            },
        )
    )
    add(
        probe(
            "gen cache bad json raises",
            gen(T, cache=cpath),
            env=API,
            before={
                ".opendaisugi/envelope_cache.db": {
                    "envcache": [cache_row(k, cenv) | {"envelope_json": "{"}]
                }
            },
        )
    )
    # Refinements: the bust and the hints.
    jd = "{HOME}/.opendaisugi"
    msgs = [
        ("permission", "Step s1 writes outside the allowed paths"),
        ("z3", "time limit exceeded"),
    ]
    add(
        probe(
            "gen cache busted by newer refinement",
            gen(T, cache=cpath, journal=jd),
            env=API,
            replies=[{"http": good}],
            before={
                **journal([refinement(k, 9999999999.0, msgs)]),
                ".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k, cenv)]},
            },
        )
    )
    add(
        probe(
            "gen cache kept by older refinement",
            gen(T, cache=cpath, journal=jd),
            env=API,
            before={
                **journal([refinement(k, 1000.0, msgs)]),
                ".opendaisugi/envelope_cache.db": {"envcache": [cache_row(k, cenv)]},
            },
        )
    )
    many = [
        refinement(
            k,
            5000.0 + i,
            [("permission", f"violation {i % 13}"), ("dag", "cycle")],
            session=f"r{i}",
        )
        for i in range(15)
    ]
    many.append(refinement(k, 4000.0, [("permission", "violation 3")], session="old"))
    many.append(refinement(cache_key("other task"), 9000.0, [("x", "not this key")], session="o"))
    add(
        probe(
            "gen hints from refinements",
            gen(T, journal=jd),
            env=API,
            replies=[{"http": good}],
            before=journal(many),
        )
    )
    add(
        probe(
            "gen hints tie order",
            gen(T, journal=jd),
            env=API,
            replies=[{"http": good}],
            before=journal(
                [
                    refinement(k, 7000.0, [("b", "second"), ("a", "first")]),
                    refinement(k, 7000.0, [("c", "second"), ("a", "third")], session="r2"),
                ]
            ),
        )
    )
    add(
        probe(
            "gen hints per rung",
            gen(T, journal=jd, model=ladder),
            env=API,
            replies=[{"http": reply_env(T, shell_allowlist=["ls"])}, {"http": good}],
            before=journal([refinement(k2, 7000.0, [("perm", "only on the second rung")])]),
        )
    )
    # Tier-0.
    pdb = "{HOME}/.opendaisugi/pathways.db"
    pw = pathway(
        1, "read the work file and count lines", lexical("read the work file and count lines")
    )
    add(
        probe(
            "gen tier0 hit",
            gen("read the work file and count lines", pathways=pdb),
            env=API,
            before={**db_of(pw), **LEX},
        )
    )
    add(
        probe(
            "gen tier0 miss",
            gen("deploy the site to production", pathways=pdb),
            env=API,
            replies=[{"http": good}],
            before={**db_of(pw), **LEX},
        )
    )
    add(
        probe(
            "gen tier0 high stakes skips",
            gen("read the work file and count lines", pathways=pdb, stakes="high"),
            env=API,
            replies=[{"http": good}],
            before={**db_of(pw), **LEX},
        )
    )
    add(
        probe(
            "gen tier0 threshold",
            gen("read the work file", pathways=pdb, threshold=0.99),
            env=API,
            replies=[{"http": good}],
            before={**db_of(pw), **LEX},
        )
    )
    add(
        probe(
            "gen tier0 empty store",
            gen(T, pathways=pdb),
            env=API,
            replies=[{"http": good}],
            before={".opendaisugi/pathways.db": {"db": {"rows": []}}, **LEX},
        )
    )
    add(
        probe(
            "gen tier0 unknown matcher falls through",
            gen(T, pathways=pdb),
            env=API,
            replies=[{"http": good}],
            before={**db_of(pw), CONFIG: {"text": "matcher_model: bogus\n"}},
        )
    )
    add(
        probe(
            "gen tier0 before cache",
            gen("read the work file and count lines", pathways=pdb, cache=cpath),
            env=API,
            before={
                **db_of(pw),
                **LEX,
                ".opendaisugi/envelope_cache.db": {
                    "envcache": [cache_row(cache_key("read the work file and count lines"), cenv)]
                },
            },
        )
    )
    # Tier-1.
    t1 = {"model": "local-7b", "base_url_env": "K1_BASE"}
    t1env = {**API, "K1_BASE": "http://127.0.0.1:{PORT}/v1"}
    add(
        probe(
            "gen tier1 answers", gen(T, tier1=t1, cache=cpath), env=t1env, replies=[{"chat": good}]
        )
    )
    add(
        probe(
            "gen tier1 inconsistent falls through",
            gen(T, tier1=t1),
            env=t1env,
            replies=[{"chat": reply_env(T, shell_allowlist=["ls"])}, {"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 error falls through",
            gen(T, tier1=t1),
            env=t1env,
            replies=[{"http_status": 500, "body": "{}"}, {"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 reask once",
            gen(T, tier1=t1),
            env=t1env,
            replies=[{"chat": "nope"}, {"chat": "{}"}, {"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 named with key",
            gen(T, tier1={**t1, "name": "box", "api_key": "sk-local-k1-0000000"}, cache=cpath),
            env={**t1env, "OPENAI_API_KEY": "sk-local-k1-0000000"},
            replies=[{"chat": good}],
        )
    )
    tk = cache_key(T, tier1_provider_name="http:openai/local-7b")
    add(
        probe(
            "gen tier1 cache hit",
            gen(T, tier1=t1, cache=cpath),
            env=t1env,
            before={".opendaisugi/envelope_cache.db": {"envcache": [cache_row(tk, cenv)]}},
        )
    )
    add(
        probe(
            "gen tier1 high stakes skips",
            gen(T, tier1=t1, stakes="high"),
            env=t1env,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 no key anthropic declines",
            gen(T, tier1={"model": "anthropic/claude-haiku-4-5"}, model="openai/m"),
            env={"OPENDAISUGI_LLM_BACKEND": "api", "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1"},
            replies=[{"chat": good}],
        )
    )
    add(
        probe(
            "gen tier1 claude-code backend",
            gen(T, tier1=t1),
            env={**t1env, **CC},
            replies=[{"claude": good}],
        )
    )
    add(
        probe(
            "gen tier1 from config",
            gen(T, tier1=jd),
            env=API,
            before={
                ".opendaisugi/local_tier1.json": {
                    "text": json.dumps({"model": "anthropic/claude-haiku-4-5", "base_url": None})
                }
            },
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 config not json",
            gen(T, tier1=jd),
            env=API,
            before={".opendaisugi/local_tier1.json": {"text": "{nope"}},
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 config no model",
            gen(T, tier1=jd),
            env=API,
            before={".opendaisugi/local_tier1.json": {"text": '{"model": ""}'}},
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen tier1 config model not a string",
            gen(T, tier1=jd),
            env=API,
            before={
                ".opendaisugi/local_tier1.json": {"text": '{"model": 5, "base_url": "http://x"}'}
            },
        )
    )
    add(
        probe(
            "gen tier1 config model not a string no base",
            gen(T, tier1=jd),
            env=API,
            before={".opendaisugi/local_tier1.json": {"text": '{"model": 5}'}},
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen tier0 minilm not carried",
            gen("read the work file and count lines", pathways=pdb),
            env=API,
            replies=[{"http": good}],
            before={**db_of(pw), CONFIG: {"text": "matcher_model: all-MiniLM-L6-v2\n"}},
        )
    )
    add(
        probe(
            "gen tier1 config a list",
            gen(T, tier1=jd),
            env=API,
            before={".opendaisugi/local_tier1.json": {"text": "[1]"}},
        )
    )
    # Inheritance through generation.
    parent = env_dict(5, file_read=["/work/**"], shell=False, shell_allowlist=[])
    add(
        probe(
            "gen parent tightening",
            gen(T, parent=parent, cache=cpath),
            env=API,
            replies=[{"http": good}],
        )
    )
    add(
        probe(
            "gen parent relaxed",
            gen(T, parent=parent),
            env=API,
            replies=[{"http": reply_env(T, file_read=["/etc/**"])}],
        )
    )
    add(
        probe(
            "gen parent low stakes ignores",
            gen(T, parent=parent, stakes="low", low_stakes=env_dict(7)),
            env=API,
        )
    )
    add(
        probe(
            "gen tier1 parent relaxed",
            gen(T, parent=parent, tier1=t1),
            env=t1env,
            replies=[{"chat": reply_env(T, network=True)}],
        )
    )
    return C


def build_inherit_cases() -> list[dict[str, Any]]:
    base = env_dict(
        1,
        file_read=["/a/**", "/b/**"],
        file_write=["/a/out"],
        shell=True,
        shell_allowlist=["ls", "make"],
        network=True,
        network_hosts=["x.com", "y.com"],
        mcp_allowlist=["fs/read"],
        max_execution_time_s=60,
    )
    pairs: list[tuple[str, dict, dict]] = []

    def child(**perm):
        d = copy.deepcopy(base)
        d["permissions"].update(perm)
        d["id"] = "env_child001"
        return d

    pairs.append(("same", child(), base))
    pairs.append(
        (
            "tighter",
            child(
                file_read=["/a/**"],
                shell=False,
                shell_allowlist=[],
                max_execution_time_s=5,
                network_hosts=["x.com"],
            ),
            base,
        )
    )
    pairs.append(
        ("globs wider", child(file_read=["/c/**", "/a/**", "/c/**", "/0"], file_write=["/z"]), base)
    )
    pairs.append(
        (
            "bools",
            child(shell_allow_decomposition=True),
            base | {"permissions": base["permissions"] | {"shell": False, "network": False}},
        )
    )
    pairs.append(("ints", child(max_execution_time_s=61, max_output_size_mb=11), base))
    pairs.append(("hosts empty child", child(network_hosts=[]), base))
    pairs.append(("hosts extra", child(network_hosts=["z.com", "x.com"]), base))
    pairs.append(
        (
            "hosts any parent",
            child(network_hosts=["q.com"]),
            base | {"permissions": base["permissions"] | {"network_hosts": []}},
        )
    )
    pairs.append(("mcp", child(mcp_allowlist=["fs/write", "fs/read"]), base))
    pairs.append(("stakes down", child() | {"stakes": "low"}, base | {"stakes": "high"}))
    pairs.append(("stakes up", child() | {"stakes": "physical"}, base | {"stakes": "medium"}))
    pairs.append(("depth two", child(), base | {"parent_envelope": "env_gp"}))
    inv = {"type": "file_unchanged", "target": "/a/x", "description": "keep x"}
    pairs.append(("invariant dropped", child(), base | {"invariants": [inv]}))
    pairs.append(
        (
            "invariant kept",
            child() | {"invariants": [inv, inv | {"target": "/b"}]},
            base | {"invariants": [inv]},
        )
    )
    post = {"type": "exit_code", "expected": 0}
    pairs.append(("postcondition dropped", child(), base | {"postconditions": [post, post]}))
    # Two or more members dropped: the oracle sorts them (K1-5).
    pairs.append(
        (
            "invariants dropped sorted",
            child(),
            base
            | {
                "invariants": [
                    inv | {"target": t, "description": f"keep {t}"} for t in ("/z", "/a", "/q/x")
                ]
            },
        )
    )
    pairs.append(
        (
            "postconditions dropped sorted",
            child() | {"postconditions": [post]},
            base
            | {
                "postconditions": [
                    post,
                    {"type": "file_exists", "path": "/b/out"},
                    {"type": "exit_code", "expected": 3},
                ]
            },
        )
    )
    robot = base["permissions"] | {
        "workspace_bounds": [[0, 0, 0], [1, 1, 1]],
        "velocity_limit": 1.5,
        "joint_limits": {"j1": [-1, 1]},
    }
    pairs.append(("robot no bounds", child(), base | {"permissions": robot}))
    pairs.append(
        (
            "robot wider box",
            child(
                workspace_bounds=[[0, 0, -0.5], [1, 1, 1]],
                velocity_limit=1.0,
                joint_limits={"j1": [-1, 1]},
            ),
            base | {"permissions": robot},
        )
    )
    pairs.append(
        (
            "robot faster",
            child(
                workspace_bounds=[[0, 0, 0], [1, 1, 1]],
                velocity_limit=2.0,
                joint_limits={"j1": [-1, 1]},
            ),
            base | {"permissions": robot},
        )
    )
    pairs.append(
        (
            "robot joint",
            child(
                workspace_bounds=[[0, 0, 0], [1, 1, 1]],
                velocity_limit=1.0,
                joint_limits={"j1": [-2, 1]},
            ),
            base | {"permissions": robot},
        )
    )
    pairs.append(
        (
            "robot joint missing",
            child(workspace_bounds=[[0, 0, 0], [1, 1, 1]], velocity_limit=1.0),
            base | {"permissions": robot},
        )
    )
    pairs.append(
        ("robot torque", child(), base | {"permissions": base["permissions"] | {"torque_limit": 3}})
    )
    pairs.append(
        (
            "robot obstacles",
            child(),
            base
            | {
                "permissions": base["permissions"]
                | {"obstacles": [[[0, 0, 0], [1, 1, 1]], [[2, 2, 2], [3, 3, 3]]]}
            },
        )
    )
    pairs.append(
        (
            "many at once",
            child(
                file_read=["/q"],
                network_hosts=[],
                max_output_size_mb=99,
                shell_allow_decomposition=True,
            )
            | {"stakes": "low"},
            base | {"stakes": "medium", "parent_envelope": "env_x"},
        )
    )
    return [
        probe(f"inherit {n}", {"kind": "inherit", "child": c, "parent": p}) for n, c, p in pairs
    ]


def build_bind_cases() -> list[dict[str, Any]]:
    from opendaisugi.models import ActionPlan, NetworkStep, ShellStep

    C: list[dict[str, Any]] = []
    db = "{HOME}/.opendaisugi/pathways.db"
    typed = pathway(
        2,
        "read the log file",
        lexical("read the log file"),
        pl=file_plan(),
        parameters=[param("path", 1, "s2", "path", "/work")],
    )
    frozen = pathway(1, "run the unit tests", lexical("run the unit tests"))
    shell = pathway(
        3,
        "list a dir",
        lexical("list a dir"),
        pl=ActionPlan(
            id="plan_00000003",
            source="script",
            task="t",
            steps=[ShellStep(id="s1", command="ls /work")],
        ),
        env=envelope(3, shell=True, shell_allowlist=["ls"]),
        parameters=[param("cmd", 0, "s1", "command", "ls", observed=["ls /work", "ls /tmp"])],
    )
    net = pathway(
        4,
        "fetch a page",
        lexical("fetch a page"),
        pl=ActionPlan(
            id="plan_00000004",
            source="script",
            task="t",
            steps=[NetworkStep(id="n1", url="https://example.com/a")],
        ),
        env=envelope(4, network=True, network_hosts=["example.com"]),
        parameters=[
            param(
                "url",
                0,
                "n1",
                "url",
                "https://example.com",
                observed=["https://example.com/a", "https://example.com/b"],
            )
        ],
    )
    odd = [
        pathway(
            5,
            "bogus field",
            lexical("bogus field"),
            pl=file_plan(),
            parameters=[param("p", 1, "s2", "bogus", "/work")],
        ),
        pathway(
            6,
            "id field",
            lexical("id field"),
            pl=file_plan(),
            parameters=[param("p", 1, "s2", "id", "/work")],
        ),
        pathway(
            7,
            "negative index",
            lexical("negative index"),
            pl=file_plan(),
            parameters=[param("p", -1, "s2", "path", "/work")],
        ),
        pathway(
            8,
            "index past end",
            lexical("index past end"),
            pl=file_plan(),
            parameters=[param("p", 5, "s2", "path", "/work")],
        ),
        pathway(
            9,
            "two holes",
            lexical("two holes"),
            pl=file_plan(),
            parameters=[
                param("b", 1, "s2", "path", "/work"),
                param("a", 0, "s1", "command", "make", observed=["make test", "make it's"]),
            ],
        ),
    ]
    store = db_of(frozen, typed, shell, net, *odd)
    ok = env_dict(9)
    narrow = env_dict(9, file_read=["/work/a.txt"])
    shell_env = env_dict(9, shell=True, shell_allowlist=["ls"], file_read=[])
    net_env = env_dict(9, shell=False, network=True, network_hosts=["example.com"])

    def b(name, pid, task, env, replies=None, cenv=None, **q):
        C.append(
            probe(
                f"bind {name}",
                {
                    "kind": "bind",
                    "db": db,
                    "id": pid,
                    "task": task,
                    "envelope": env,
                    "z3_timeout_ms": 500,
                    **q,
                },
                before={**store, **LEX},
                env=cenv or CC,
                replies=replies,
            )
        )

    b("frozen no call", "pw_0001", "run the unit tests", ok)
    b("good value", "pw_0002", "read /work/b.txt", ok, [{"claude": bind_json(path="/work/b.txt")}])
    b("head change", "pw_0002", "read /etc/passwd", ok, [{"claude": bind_json(path="/etc/passwd")}])
    b("missing hole", "pw_0002", "read b", ok, [{"claude": bind_json(other="/work/b.txt")}])
    b(
        "not a dict",
        "pw_0002",
        "read b",
        ok,
        [{"claude": '{"values": [1]}'}, {"claude": '{"values": 3}'}, {"claude": "{}"}],
    )
    b(
        "fails verify",
        "pw_0002",
        "read /work/b.txt",
        narrow,
        [{"claude": bind_json(path="/work/b.txt")}],
    )
    b("model fails", "pw_0002", "read b", ok, [{"claude_is_error": "rate limited"}])
    b(
        "api backend",
        "pw_0002",
        "read /work/c.txt",
        ok,
        [{"http": bind_json(path="/work/c.txt")}],
        cenv=API,
    )
    b("api no key", "pw_0002", "read /work/c.txt", ok, cenv={"OPENDAISUGI_LLM_BACKEND": "api"})
    b("shell same head", "pw_0003", "list /tmp", shell_env, [{"claude": bind_json(cmd="ls /tmp")}])
    b(
        "shell other program",
        "pw_0003",
        "remove",
        shell_env,
        [{"claude": bind_json(cmd="rm -rf /work")}],
    )
    b("shell unbalanced quote", "pw_0003", "q", shell_env, [{"claude": bind_json(cmd="ls 'x")}])
    b("shell metachar", "pw_0003", "q", shell_env, [{"claude": bind_json(cmd="ls /tmp; rm -rf /")}])
    b(
        "network same host",
        "pw_0004",
        "fetch c",
        net_env,
        [{"claude": bind_json(url="https://example.com/c")}],
    )
    b(
        "network other host",
        "pw_0004",
        "fetch evil",
        net_env,
        [{"claude": bind_json(url="https://evil.com/c")}],
    )
    b(
        "network userinfo host",
        "pw_0004",
        "fetch",
        net_env,
        [{"claude": bind_json(url="https://example.com@evil.com/c")}],
    )
    b("undeclared field", "pw_0005", "x", ok, [{"claude": bind_json(p="/work/z")}])
    b("id field", "pw_0006", "x", ok, [{"claude": bind_json(p="/work/z")}])
    b("negative index", "pw_0007", "x", ok, [{"claude": bind_json(p="/work/z")}])
    b("index past end", "pw_0008", "x", ok, [{"claude": bind_json(p="/work/z")}])
    b(
        "two holes",
        "pw_0009",
        "x",
        env_dict(9, shell=True, shell_allowlist=["make"]),
        [{"claude": bind_json(a="make build", b="/work/z")}],
    )
    # A hole in a field that does not hold a string: refused, the
    # template given (K1-4).
    kinds = db_of(
        *[
            pathway(
                20 + i,
                f"{f} field",
                lexical(f"{f} field"),
                pl=file_plan(),
                parameters=[param("p", 1, "s2", f, "/work")],
            )
            for i, f in enumerate(("depends_on", "metadata", "type", "postcondition"))
        ]
    )
    for i, f in enumerate(("depends_on", "metadata", "type", "postcondition")):
        C.append(
            probe(
                f"bind {f} field",
                {
                    "kind": "bind",
                    "db": db,
                    "id": f"pw_{20 + i:04d}",
                    "task": "x",
                    "envelope": ok,
                    "z3_timeout_ms": 500,
                },
                before={**kinds, **LEX},
                env=CC,
                replies=[{"claude": bind_json(p="/work/z")}],
            )
        )
    b(
        "custom model",
        "pw_0002",
        "read /work/d.txt",
        ok,
        [{"http": bind_json(path="/work/d.txt")}],
        cenv=API,
        model="anthropic/claude-haiku-4-5",
    )
    return C


def build_compose_cases() -> list[dict[str, Any]]:
    from opendaisugi.models import ActionPlan, FileReadStep

    db = "{HOME}/.opendaisugi/pathways.db"
    a = pathway(
        1,
        "read a",
        lexical("read a"),
        pl=ActionPlan(
            id="plan_00000001",
            source="script",
            task="t",
            steps=[
                FileReadStep(id="r1", path="/work/a"),
                FileReadStep(id="r2", path="/work/b", depends_on=["r1"]),
            ],
        ),
        env=envelope(1, shell=False, shell_allowlist=[], file_read=["/work/**"]),
    )
    b = pathway(
        2,
        "typed",
        lexical("typed"),
        pl=file_plan(),
        parameters=[param("path", 1, "s2", "path", "/work")],
    )
    c = pathway(
        3,
        "shell out",
        lexical("shell out"),
        env=envelope(3, shell=True, shell_allowlist=["make", "pytest"], file_read=["/work/**"]),
    )
    d = pathway(
        4,
        "reads etc",
        lexical("reads etc"),
        pl=ActionPlan(
            id="plan_00000004",
            source="script",
            task="t",
            steps=[FileReadStep(id="e1", path="/etc/hosts")],
        ),
        env=envelope(4, shell=False, shell_allowlist=[], file_read=["/etc/**"]),
    )
    store = {**db_of(a, b, c, d), **LEX}
    caller = env_dict(9, shell=False, shell_allowlist=[], file_read=["/work/**"])

    def plan_with(*skills):
        steps = [{"id": "p0", "type": "file_read", "path": "/work/in"}]
        for i, s in enumerate(skills):
            steps.append({"id": f"k{i}", "type": "skill", "skill_id": s, "depends_on": ["p0"]})
        return {"id": "plan_000000c0", "source": "script", "task": "compose", "steps": steps}

    def q(name, skills, executors, plan, env=caller):
        return probe(
            f"compose {name}",
            {
                "kind": "compose",
                "db": db,
                "skill_ids": skills,
                "executors": executors,
                "plan": plan,
                "envelope": env,
                "z3_timeout_ms": 500,
            },
            before=store,
        )

    return [
        q("subsumed", ["pw_0001"], ["file_read"], plan_with("pw_0001")),
        q("not subsumed", ["pw_0003"], ["shell", "file_read"], plan_with("pw_0003")),
        q("wider reads", ["pw_0004"], ["file_read"], plan_with("pw_0004")),
        q(
            "mixed",
            ["pw_0001", "pw_0002", "pw_0003", "nope"],
            ["file_read"],
            plan_with("pw_0001", "pw_0003", "pw_0004"),
        ),
        q("typed opaque", ["pw_0002"], ["file_read"], plan_with("pw_0002")),
        q(
            "opaque strict",
            [],
            [],
            plan_with("pw_0002"),
            env_dict(9, shell=False, shell_allowlist=[], file_read=["/work/**"])
            | {"stakes": "high"},
        ),
        q(
            "caller wide",
            ["pw_0003", "pw_0004"],
            ["shell", "file_read"],
            plan_with("pw_0003", "pw_0004"),
            env_dict(
                9,
                shell=True,
                shell_allowlist=["make", "pytest", "ls"],
                file_read=["/work/**", "/etc/**"],
            ),
        ),
    ]


def build_recall_cases() -> list[dict[str, Any]]:
    db = "{HOME}/.opendaisugi/pathways.db"
    typed = pathway(
        2,
        "read the log file",
        lexical("read the log file"),
        pl=file_plan(),
        parameters=[param("path", 1, "s2", "path", "/work")],
    )
    frozen = pathway(1, "run the unit tests", lexical("run the unit tests"))
    store = {**db_of(frozen, typed), **LEX}
    ok = env_dict(9)
    narrow = env_dict(9, file_read=["/work/a.txt"])

    def r(name, task, env, replies=None, cenv=None):
        return probe(
            f"recall {name}",
            {"kind": "recall", "db": db, "task": task, "envelope": env, "z3_timeout_ms": 500},
            before=store,
            env=cenv or CC,
            replies=replies,
        )

    return [
        r("typed bound", "read the log file", ok, [{"claude": bind_json(path="/work/log.txt")}]),
        r(
            "typed head change falls back",
            "read the log file",
            ok,
            [{"claude": bind_json(path="/etc/log")}],
        ),
        r(
            "typed bound fails verify, template kept",
            "read the log file",
            narrow,
            [{"claude": bind_json(path="/work/log.txt")}],
        ),
        r(
            "typed template fails too",
            "read the log file",
            env_dict(9, file_read=["/other/**"]),
            [{"claude": bind_json(path="/work/log.txt")}],
        ),
        r("typed model fails", "read the log file", ok, [{"claude_is_error": "down"}]),
        r(
            "typed api",
            "read the log file",
            ok,
            [{"http": bind_json(path="/work/l2.txt")}],
            cenv=API,
        ),
        r("frozen", "run the unit tests", ok),
        r("miss", "summarize the release notes", ok),
    ]


def build_cli_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    T = "Read /work/a.txt and count its lines"
    good = reply_env(T)

    def add(name, argv, *, env=None, replies=None, before=None, **kw):
        c = {
            "kind": "cli",
            "name": name,
            "argv": ["generate-envelope", *argv],
            "before": before or {},
        }
        if env:
            c["env"] = env
        if replies:
            c["replies"] = replies
        c.update(kw)
        C.append(c)

    add("cli yaml", [T], env=API, replies=[{"http": good}])
    add("cli json", [T, "--json"], env=API, replies=[{"http": good}])
    add("cli claude-code", [T, "--json"], env=CC, replies=[{"claude": good}])
    add(
        "cli --llm claude-code",
        [T, "--llm", "claude-code", "--json"],
        env=KEY,
        replies=[{"claude": good}],
    )
    add("cli --llm api", [T, "--llm", "api"], env=KEY, replies=[{"http": good}])
    add("cli --llm bogus", [T, "--llm", "bogus"], env=API)
    add("cli --llm litellm", [T, "--llm", "litellm"], env=API)
    add("cli renamed backend env", [T], env={"OPENDAISUGI_LLM_BACKEND": "litellm"})
    add("cli bad stakes", [T, "--stakes", "extreme"], env=API)
    add("cli bad thinking", [T, "--thinking-budget", "max"], env=API)
    add("cli low default", [T, "--stakes", "low"], env=API)
    add("cli low default json", [T, "--stakes", "low", "--json"], env=API)
    add(
        "cli low file",
        [T, "--stakes", "low", "--low-stakes-envelope", "low.json", "--json"],
        env=API,
        before={"low.json": {"text": json.dumps(env_dict(4))}},
    )
    add(
        "cli low file ignored at medium",
        [T, "--low-stakes-envelope", "nope.json"],
        env=API,
        replies=[{"http": good}],
    )
    add(
        "cli low file missing",
        [T, "--stakes", "low", "--low-stakes-envelope", "nope.json"],
        env=API,
    )
    add(
        "cli low file invalid",
        [T, "--stakes", "low", "--low-stakes-envelope", "low.json"],
        env=API,
        before={"low.json": {"text": '{"task": 1}'}},
    )
    add("cli high", [T, "--stakes", "high", "--json"], env=API, replies=[{"http": good}])
    add(
        "cli thinking deep",
        [T, "--thinking-budget", "deep", "--json"],
        env=API,
        replies=[{"http": good}],
    )
    add(
        "cli openai model",
        [T, "--model", "openai/o4-mini", "--thinking-budget", "light"],
        env={
            **API,
            "OPENAI_API_BASE": "http://127.0.0.1:{PORT}/v1",
            "OPENAI_API_KEY": "sk-oa-k1-00000000000",
        },
        replies=[{"chat": good}],
    )
    add("cli model fails", [T], env=API, replies=[{"http_status": 500, "body": '{"error": "x"}'}])
    add("cli inconsistent", [T], env=API, replies=[{"http": reply_env(T, shell_allowlist=["ls"])}])
    add("cli claude-code fails", [T], env=CC, replies=[{"claude_is_error": "overloaded"}])
    add("cli no key", [T], env={"OPENDAISUGI_LLM_BACKEND": "api"})
    add("cli no claude", [T], env=CC, no_claude=True)
    add("cli empty task", ["  "], env=API)
    add("cli too long", ["x" * 4001], env=API)
    add(
        "cli non-ascii",
        ["Lire /work/é.txt, compter"],
        env=API,
        replies=[{"http": reply_env("Lire /work/é.txt, compter", file_read=["/work/é.txt"])}],
    )
    add(
        "cli non-ascii json",
        ["Lire /work/é.txt, compter", "--json"],
        env=API,
        replies=[{"http": reply_env("Lire /work/é.txt, compter", file_read=["/work/é.txt"])}],
    )
    add(
        "cli decomposition flag",
        [T, "--allow-shell-decomposition", "--json"],
        env=API,
        replies=[{"http": good}],
    )
    add(
        "cli decomposition config",
        [T, "--json"],
        env=API,
        replies=[{"http": good}],
        before={CONFIG: {"text": "shell_allow_decomposition: true\n"}},
    )
    add(
        "cli decomposition config off by flag",
        [T, "--no-allow-shell-decomposition", "--json"],
        env=API,
        replies=[{"http": good}],
        before={CONFIG: {"text": "shell_allow_decomposition: true\n"}},
    )
    add("cli decomposition low", [T, "--stakes", "low", "--allow-shell-decomposition"], env=API)
    add(
        "cli data dir disarmed",
        [T, "--data-dir", "d", "--stakes", "low", "--json"],
        env=API,
        before={"d/gate/DISARMED": {"text": ""}, "d/config.yaml": {"text": "gate_mode: enforce\n"}},
    )
    add(
        "cli data dir enforce",
        [T, "--data-dir", "d", "--stakes", "low", "--json"],
        env=API,
        before={"d/config.yaml": {"text": "gate_mode: enforce\nshell_allow_decomposition: true\n"}},
    )
    add(
        "cli data dir outside home",
        [T, "--data-dir", "/nonexistent-k1", "--stakes", "low", "--json"],
        env=API,
    )
    add("cli quiet", ["-q", "generate-envelope", T, "--stakes", "low", "--json"], env=API)
    C[-1]["argv"] = ["-q", "generate-envelope", T, "--stakes", "low", "--json"]
    add("cli missing task", [], env=API)
    add(
        "cli proxy setting refused",
        [T, "--json"],
        replies=[{"http": good}],
        fake_proxy={"mode": "forward"},
        env={**API, "http_proxy": "http://127.0.0.1:{PX}", "NO_PROXY": "x%y"},
    )
    add(
        "cli summary field",
        [T, "--json"],
        env=API,
        replies=[{"http": json.dumps(json.loads(good) | {"summary": "count"})}],
    )
    add(
        "cli yaml odd strings",
        [T],
        env=API,
        replies=[
            {
                "http": reply_env(
                    "yes: no # 'quote' \"dq\"",
                    file_read=["*", "- x", "null", "1e3", "a: b", " lead"],
                )
            }
        ],
    )
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = (
        build_generate_cases()
        + build_inherit_cases()
        + build_bind_cases()
        + build_compose_cases()
        + build_recall_cases()
        + build_cli_cases()
    )
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out / "cases.jsonl").exists() and not args.fresh:
        for ln in (out / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    # Each case the oracle ran is kept here until the fixture is written,
    # so a run cut short resumes where it stopped.
    cache_path = SCRATCH / "gen-cache.jsonl"
    if cache_path.exists() and not args.fresh:
        for ln in cache_path.read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old.setdefault(c["id"], c)
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only:
            c["expect"] = prev["expect"]
            if "model" in prev:
                c["model"] = prev["model"]
            continue
        work = SCRATCH / "gen" / f"{i:04d}"
        if c.get("replies"):
            record_replies(c, work)
        c["expect"] = run_case(c, cmd_for(c, None, None), work)
        if any(r.get("key_ok") is False for r in c["expect"]["requests"]):
            raise SystemExit(f"{c['name']}: the oracle sent a credential that is not the case's")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:6000]
            )
        else:
            with cache_path.open("a", encoding="utf-8") as fh:
                rec = {"id": body_id(c), "expect": c["expect"]}
                if "model" in c:
                    rec["model"] = c["model"]
                fh.write(json.dumps(rec) + "\n")
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out / "cases.jsonl", cases)
    (out / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    cache_path.unlink(missing_ok=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Run the gateway cases through the Go binary and compare it to the oracle.

    uv run --no-sync python clients/gateway_compare.py --binary clients/go/daisugi \\
        [--oracle] [--only NAME] [--fuzz N --seed S --probe PATH] [--speed]

For every case in clients/fixtures/gateway (clients/gateway_cases.py writes
them from the oracle) it checks: whether the gateway came up, its exit
code, stdout and stderr (less the server's own log lines, GW-1), every
answer the client got (status, the headers that matter, the body, whether
a stream was passed on as it came), every request the upstream got (path,
headers, body bytes), the tree after (journal, answer store, Switchyard
files), and for Switchyard cases what the child was started with and
whether it outlived the gateway. stderr is compared as
clients/pathway_compare.py compares it.

A case the binary refuses (exit 2, one line, nothing changed) is counted
apart, and only where a ruling says it may refuse.

--oracle reruns the Python side too, so a stale fixture shows up.
--fuzz N runs N seeded turn sequences through the pipeline on both sides
(the oracle's Gateway in process, the binary's through --probe, the
gateway-probe test instrument), in chunks of at most 500 turns: the bytes
sent upstream, the stream flag, and each journal and answer-store line.
--speed times a buffered turn through the binary against a fake upstream.
"""

from __future__ import annotations

import argparse
import json
import random
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixture_paths import leaks  # noqa: E402
from gateway_cases import FIXTURE_DIR, PY_CLI, SCRATCH, build_all, run_case  # noqa: E402
from pathway_compare import STDERR_CLASSES, diff, stderr_problems  # noqa: E402

NOT_YET = "is not in this binary yet."
KEYS = (
    "ready",
    "exit",
    "stdout",
    "responses",
    "upstream",
    "tree",
    "switchyard",
    "children_alive_after",
    "proxy",
)


def compare(case: dict[str, Any], got: dict[str, Any]) -> tuple[str, list[str]]:
    expect = case["expect"]
    if case["kind"] == "recall":
        problems = [
            f"{k}: " + d
            for k in ("exit", "results", "tree")
            if expect.get(k) != got.get(k)
            for d in diff(expect.get(k), got.get(k))
        ]
        return ("agree", []) if not problems else ("disagree", problems)
    err = "\n".join(got["stderr"])
    if case.get("go_refuses"):
        said = NOT_YET in err or "Nothing was changed." in err
        ok = got["exit"] == 2 and said and len(got["stderr"]) == 1
        return ("refused", []) if ok else ("disagree", ["not refused cleanly: " + err])
    problems: list[str] = []
    for k in KEYS:
        if expect.get(k) != got.get(k):
            problems += [f"{k}: " + d for d in diff(expect.get(k), got.get(k))]
    problems += stderr_problems(expect, got)
    return ("agree", []) if not problems else ("disagree", problems)


def run_cases(binary: str, only: str | None, oracle: bool) -> int:
    lines = (FIXTURE_DIR / "cases.jsonl").read_text(encoding="utf-8").splitlines()
    cases = [json.loads(ln) for ln in lines if ln.strip()]
    current = {c["name"]: c for c in build_all()}
    counts = {"agree": 0, "refused": 0, "disagree": 0, "stale": 0}
    for i, case in enumerate(cases):
        if only and only not in case["name"]:
            continue
        if case["name"] not in current:
            counts["stale"] += 1
            print(f"STALE (no longer built) {case['name']}")
            continue
        work = SCRATCH / "cmp" / f"{i:04d}"
        got = run_case(case, [binary], work)
        verdict, problems = compare(case, got)
        counts[verdict] += 1
        if verdict == "disagree":
            print(f"DISAGREE {case['name']}")
            for p in problems[:12]:
                print("   ", p[:400])
        if oracle:
            again = run_case(case, PY_CLI, work)
            if again != case["expect"]:
                counts["stale"] += 1
                print(f"STALE {case['name']}")
                for p in diff(case["expect"], again)[:6]:
                    print("   ", p[:300])
        shutil.rmtree(work, ignore_errors=True)
    total = sum(counts[k] for k in ("agree", "refused", "disagree"))
    print(f"cases {total}: " + ", ".join(f"{k}={v}" for k, v in counts.items()))
    print("stderr compared: " + ", ".join(f"{k}={v}" for k, v in sorted(STDERR_CLASSES.items())))
    return 0 if counts["disagree"] == 0 and counts["stale"] == 0 else 1


# ---------------------------------------------------------------------------
# Fuzz: turn sequences through the pipeline, in process on both sides
# ---------------------------------------------------------------------------

WORDS = [
    "say",
    "hi",
    "fix",
    "the",
    "typo",
    "design",
    "a",
    "schema",
    "debug",
    "race",
    "condition",
    "list",
    "files",
    "résumé",
    "İstanbul",
    "ΣΑΣ",
    "é",
    "\t",
    " ",
    "\x1c",
    "  ",
    "readme",
    "architecture",
    "proof",
    "scale",
]
MODELS = [
    "claude-opus-4-8",
    "claude-sonnet-5",
    "claude-haiku-4-5",
    "gpt-5",
    "x",
    "",
    None,
    7,
    ["a"],
    {"m": 1},
    True,
    1.5,
]


EASY = [
    "say",
    "hi",
    "fix",
    "the",
    "typo",
    "list",
    "files",
    "readme",
    "résumé",
    "é",
    "  ",
    "\t",
    "now",
    "please",
]


def rand_text(rng: random.Random, easy: bool = False) -> Any:
    if easy:
        return " ".join(rng.choice(EASY) for _ in range(rng.randint(1, 12)))
    k = rng.random()
    if k < 0.05:
        return rng.choice([5, None, True, [], {}, 0, "", " "])
    n = rng.choice([0, 1, 2, 4, 8, 40, 120])
    text = " ".join(rng.choice(WORDS) for _ in range(n))
    if rng.random() < 0.03:
        text += "\ud800"
    return text


def rand_content(rng: random.Random, easy: bool = False) -> Any:
    k = rng.random()
    if k < 0.6:
        return rand_text(rng, easy)
    if k < 0.95:
        blocks = []
        for _ in range(rng.randint(0, 4)):
            t = rng.random()
            if t < 0.5:
                blocks.append({"type": "text", "text": rand_text(rng, easy)})
            elif t < 0.7:
                blocks.append(
                    {
                        "type": "tool_result",
                        "tool_use_id": "t",
                        "content": rng.choice(
                            [
                                "out " * rng.randint(0, 900),
                                [{"type": "text", "text": "x" * rng.randint(0, 3000)}],
                                5,
                            ]
                        ),
                    }
                )
            elif t < 0.8:
                blocks.append({"type": "tool_use", "id": "t", "name": "ls", "input": {}})
            elif t < 0.9:
                blocks.append(rng.choice(["raw", 5, None, {"type": "image"}, {"text": ["a", "b"]}]))
            else:
                blocks.append({"type": "text", "text": rng.choice([5, None, 0, "", True, ["q"]])})
        return blocks
    return rng.choice([None, 5, {"a": 1}])


def rand_body(rng: random.Random, first: str, easy: bool = False) -> Any:
    msgs: list[Any] = [{"role": "user", "content": first}]
    for _ in range(rng.randint(0, 5)):
        r = rng.random()
        if r < 0.4:
            msgs.append({"role": "assistant", "content": rand_content(rng)})
        elif r < 0.95:
            msgs.append({"role": "user", "content": rand_content(rng, easy)})
        else:
            msgs.append(rng.choice([1, "m", None, [], {"content": "x"}]))
    body: dict[str, Any] = {"model": rng.choice(MODELS), "max_tokens": 16, "messages": msgs}
    if rng.random() < 0.3:
        body["system"] = rng.choice(
            [
                "S" * rng.randint(0, 20000),
                [{"type": "text", "text": "sys " * 50}],
                5,
                None,
                [{"content": [{"text": "n" * 9000}]}],
            ]
        )
    if rng.random() < 0.3:
        body["stream"] = rng.choice([True, False, "true", 1, None])
    if rng.random() < 0.05:
        body["messages"] = rng.choice(["", {}, "abc", {"k": 1}, None, 5])
    if rng.random() < 0.02:
        return rng.choice([[1, 2], "str", 5, None])
    if rng.random() < 0.1:
        body = {k: body[k] for k in rng.sample(list(body), len(body))}
    return body


def rand_usage(rng: random.Random, openai: bool) -> Any:
    if rng.random() < 0.05:
        return rng.choice([[1], "x", 5, None])
    if openai:
        u: dict[str, Any] = {
            "prompt_tokens": rng.choice([rng.randint(0, 10**6), True, "5", None]),
            "completion_tokens": rng.choice([rng.randint(0, 10**5), False, 2.0]),
        }
        if rng.random() < 0.5:
            u["prompt_tokens_details"] = {
                "cached_tokens": rng.choice([rng.randint(0, 10**6), True, "3"])
            }
        return u
    u = {}
    for k in (
        "input_tokens",
        "output_tokens",
        "cache_read_input_tokens",
        "cache_creation_input_tokens",
    ):
        r = rng.random()
        if r < 0.8:
            u[k] = rng.randint(0, 10**6)
        elif r < 0.85:
            u[k] = rng.choice([True, " 12 ", 3.7, "1_000", 10**30])
        elif r < 0.87:
            u[k] = rng.choice(["x", None, [1], 1e400])
    return u


def fuzz_sequences(n: int, seed: int) -> list[dict[str, Any]]:
    """n turns, grouped into gateways: each gateway a config and a run of
    turns that share first asks, so the sticky rungs are exercised."""
    rng = random.Random(seed)
    seqs: list[dict[str, Any]] = []
    left = n
    while left > 0:
        k = min(left, rng.randint(5, 60))
        left -= k
        mode = rng.choice(["rules"] * 6 + ["off", "external"])
        cfg: dict[str, Any] = {
            "mode": mode,
            "cheap": rng.choice(["claude-haiku-4-5", "claude-sonnet-5", "tiny"]),
            "local": rng.choice([None, None, None, "qwen2.5:7b"]),
            "capture": rng.random() < 0.5,
            "openai": rng.random() < 0.15,
        }
        if mode == "external":
            cfg["external"] = {
                "route_id": "daisugi",
                "capable": "claude-sonnet-5",
                "efficient": rng.choice(["claude-haiku-4-5", "llama3"]),
                "prices": rng.choice([{}, {"llama3": [0.0, 0.0]}]),
            }
        # Most runs ask easy things and come back to the same first ask, so
        # the cheap, sticky, local and prefix rungs are all reached.
        easy = rng.random() < 0.7
        firsts = [rand_text(rng, easy) for _ in range(rng.randint(1, 4))]
        firsts = [f if isinstance(f, str) else "hello" for f in firsts]
        turns = []
        for _ in range(k):
            body = rand_body(rng, rng.choice(firsts), easy)
            raw = json.dumps(body)
            if rng.random() < 0.03:
                raw = rng.choice(["{bad", "", "﻿" + raw])
            t = {
                "body": raw,
                "usage": json.dumps(rand_usage(rng, cfg["openai"])),
                "answer": rng.choice(["", "an answer", "é" * 3]),
                "original": rng.random() < 0.1,
                "served": rng.choice(
                    [None, "claude-haiku-4-5", "llama3", "claude-sonnet-5", "daisugi"]
                ),
            }
            turns.append(t)
        seqs.append({"config": cfg, "turns": turns})
    return seqs


ORACLE_FUZZ = r"""
import json, sys, tempfile, os
from dataclasses import replace
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from opendaisugi.gateway_asgi import _prepare, _record, build_default_gateway
from opendaisugi.gateway_pipeline import ExternalRouterConfig
out = []
for ln in sys.stdin:
    seq = json.loads(ln)
    cfg = seq["config"]
    d = Path(tempfile.mkdtemp(dir=os.environ["FUZZ_DIR"]))
    ext = None
    if cfg["mode"] == "external":
        e = cfg["external"]
        ext = ExternalRouterConfig(route_id=e["route_id"], capable_target=e["capable"], efficient_target=e["efficient"],
                                   prices={k: tuple(v) for k, v in e["prices"].items()})
    gw = build_default_gateway(data_dir=d, cheap_model=cfg["cheap"], capture_answers=cfg["capture"],
                               local_model=cfg["local"], router_mode=cfg["mode"], external=ext)
    journal = d / "gateway" / "turns.jsonl"
    answers = d / "gateway" / "answers.jsonl"
    res = []
    for t in seq["turns"]:
        body = t["body"].encode("utf-8", "surrogatepass")
        outbound, prepared, stream = _prepare(gw, body)
        usage = json.loads(t["usage"])
        if cfg["openai"]:
            from opendaisugi.gateway_openai import normalize_openai_usage
            usage = normalize_openai_usage(usage)
        nj = len(journal.read_text().splitlines()) if journal.exists() else 0
        _record(gw, prepared, usage, t["original"] and prepared is not None and prepared.decision.downgraded,
                answer_text=t["answer"], served_target=t["served"])
        lines = journal.read_text().splitlines() if journal.exists() else []
        rec = [json.loads(x) for x in lines[nj:]]
        for r in rec:
            r.pop("created_at")
            # A turn's time differs run to run; only its type is compared.
            if isinstance(r.get("elapsed_ms"), (int, float)):
                r["elapsed_ms"] = "num"
        ans = [json.loads(x) for x in answers.read_text().splitlines()] if answers.exists() else []
        for a in ans:
            a.pop("created_at", None)
        res.append({"outbound": outbound.decode("utf-8", "surrogateescape"), "prepared": prepared is not None,
                    "stream": stream, "records": rec, "answers": len(ans), "last_answer": ans[-1] if ans else None})
    print(json.dumps({"results": res}), flush=True)
"""


def run_fuzz(probe: str, n: int, seed: int) -> int:
    fuzz_dir = SCRATCH / "fuzz"
    shutil.rmtree(fuzz_dir, ignore_errors=True)
    fuzz_dir.mkdir(parents=True)
    seqs = fuzz_sequences(n, seed)
    chunks: list[list[dict[str, Any]]] = [[]]
    count = 0
    for s in seqs:
        if count + len(s["turns"]) > 500 and chunks[-1]:
            chunks.append([])
            count = 0
        chunks[-1].append(s)
        count += len(s["turns"])
    src = str(Path(__file__).resolve().parent.parent / "src")
    env = {
        "FUZZ_DIR": str(fuzz_dir),
        "PATH": "/usr/bin:/bin",
        "HOME": str(fuzz_dir),
        "LANG": "C.UTF-8",
    }
    turns = disagree = 0
    stats = {
        "prepared": 0,
        "fell open": 0,
        "journaled": 0,
        "downgraded": 0,
        "stream": 0,
        "answers kept": 0,
    }
    for chunk in chunks:
        data = "".join(json.dumps(s) + "\n" for s in chunk)
        py = subprocess.run(
            [sys.executable, "-c", ORACLE_FUZZ, src],
            input=data,
            capture_output=True,
            text=True,
            env=env,
            timeout=600,
            check=False,
        )
        go = subprocess.run(
            [probe], input=data, capture_output=True, text=True, env=env, timeout=600, check=False
        )
        if py.returncode != 0 or go.returncode != 0:
            print("fuzz run failed:", py.stderr[-2000:], go.stderr[-2000:])
            return 1
        pys = [json.loads(ln) for ln in py.stdout.splitlines()]
        gos = [json.loads(ln) for ln in go.stdout.splitlines()]
        for s, a, b in zip(chunk, pys, gos, strict=True):
            for i, (x, y) in enumerate(zip(a["results"], b["results"], strict=True)):
                turns += 1
                stats["prepared" if x["prepared"] else "fell open"] += 1
                stats["journaled"] += len(x["records"])
                stats["downgraded"] += sum(1 for r in x["records"] if r.get("downgraded"))
                stats["stream"] += bool(x["stream"])
                stats["answers kept"] += x["answers"]
                for r in x["records"]:
                    key = "tier " + str(r.get("tier"))
                    stats[key] = stats.get(key, 0) + 1
                if x != y:
                    disagree += 1
                    if disagree <= 8:
                        print(
                            "DISAGREE turn",
                            json.dumps(s["config"]),
                            json.dumps(s["turns"][i])[:600],
                        )
                        for d in diff(x, y)[:6]:
                            print("   ", d[:400])
    shutil.rmtree(fuzz_dir, ignore_errors=True)
    print(f"fuzz seed {seed}: {turns} turns in {len(chunks)} chunk(s), {disagree} disagreements")
    print("  " + ", ".join(f"{k} {v}" for k, v in stats.items()))
    return 0 if disagree == 0 else 1


# ---------------------------------------------------------------------------
# Speed: the gateway's added time on a buffered turn
# ---------------------------------------------------------------------------


def run_speed(binary: str, upstream_bin: str, n: int = 400) -> int:
    """A Go fake upstream answers at once; the same raw client times turns
    straight to it and through the gateway, alternating."""
    import socket

    from gateway_cases import wait_ready
    from ports import PortPool

    work = SCRATCH / "speed"
    shutil.rmtree(work, ignore_errors=True)
    (work / "home").mkdir(parents=True)
    with PortPool() as pool:
        up, gw = pool.take(), pool.take()
    upp = subprocess.Popen(
        [upstream_bin, str(up)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    env = {"HOME": str(work / "home"), "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
    gwp = subprocess.Popen(
        [
            binary,
            "gateway",
            "--port",
            str(gw),
            "--upstream",
            f"http://127.0.0.1:{up}",
            "--openai-upstream",
            f"http://127.0.0.1:{up}",
        ],
        env=env,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        if not wait_ready(gw, gwp) or not wait_ready(up, upp):
            print("speed: a server did not come up")
            return 1
        body = json.dumps(
            {
                "model": "claude-opus-4-8",
                "max_tokens": 16,
                "messages": [{"role": "user", "content": "design a schema"}],
            }
        ).encode()

        def one(port: int) -> float:
            req = (
                b"POST /v1/messages HTTP/1.1\r\nHost: x\r\ncontent-type: application/json\r\n"
                b"Content-Length: %d\r\nConnection: close\r\n\r\n" % len(body)
            ) + body
            t0 = time.perf_counter()
            with socket.create_connection(("127.0.0.1", port)) as s:
                s.sendall(req)
                while s.recv(65536):
                    pass
            return time.perf_counter() - t0

        for _ in range(50):
            one(up)
            one(gw)
        direct, via = [], []
        for _ in range(n):
            direct.append(one(up))
            via.append(one(gw))
        d50, v50 = statistics.median(direct) * 1000, statistics.median(via) * 1000
        q = lambda xs, p: sorted(xs)[int(len(xs) * p)] * 1000  # noqa: E731
        print(
            f"buffered turn, {n} each: direct p50 {d50:.2f} ms p90 {q(direct, 0.9):.2f} ms; "
            f"through the gateway p50 {v50:.2f} ms p90 {q(via, 0.9):.2f} ms; added p50 {v50 - d50:.2f} ms"
        )
    finally:
        gwp.send_signal(2)
        gwp.wait(timeout=20)
        upp.kill()
        upp.wait()
        shutil.rmtree(work, ignore_errors=True)
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True)
    ap.add_argument("--oracle", action="store_true")
    ap.add_argument("--only")
    ap.add_argument("--fuzz", type=int, default=0)
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--probe")
    ap.add_argument("--speed", action="store_true")
    ap.add_argument("--upstream-bin", help="the Go fake upstream for --speed")
    ap.add_argument("--skip-cases", action="store_true")
    args = ap.parse_args()
    binary = str(Path(args.binary).resolve())
    rc = 0
    if not args.skip_cases:
        rc |= run_cases(binary, args.only, args.oracle)
    if args.fuzz:
        if not args.probe:
            raise SystemExit("--fuzz needs --probe")
        rc |= run_fuzz(str(Path(args.probe).resolve()), args.fuzz, args.seed)
    if args.speed:
        rc |= run_speed(binary, str(Path(args.upstream_bin).resolve()))
    hits = leaks()
    if hits:
        print("\n".join(hits[:10]))
        rc = 1
    return rc


if __name__ == "__main__":
    raise SystemExit(main())

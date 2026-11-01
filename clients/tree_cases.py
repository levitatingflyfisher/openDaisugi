"""Synthetic delegation-tree cases: `daisugi tree check|root|spawn|run|end|answer|status`
and `daisugi warden register --parent`, run through the Python oracle.

    uv run --no-sync python clients/tree_cases.py [--out clients/fixtures/tree] [--only NAME]

A case runs as a garden case does (clients/garden_cases.py): a scratch
HOME, the tree laid out, one command, and the exit code, stdout, stderr and
the tree after it recorded. A case that needs a tree already grown lays out
its ledger (``.opendaisugi/tree/ledger.jsonl``) and the registered
envelopes (``.opendaisugi/warden/envelopes``) as the commands write them, with
times relative to the run. No case calls a model or a host. Every path, id
and task is synthetic. A `tree run` case starts the fake sprig of
clients/agentic_fakes.py, which answers from the case's recorded replies.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import agentic_fakes  # noqa: E402, F401 - patches garden_cases: the fake sprig
import garden_cases  # noqa: E402, F401 - sibling module, run as a script
import gitfix  # noqa: E402
from garden_cases import PY_CLI, body_id, record_replies, run_case, write_jsonl  # noqa: E402
from pathway_cases import REPO  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "tree"
SCRATCH = Path(
    os.environ.get("DAISUGI_TREE_SCRATCH") or Path.home() / "opendaisugi-scratch" / "tree" / "runs"
)
CASE_VERSION = 1
LEDGER = ".opendaisugi/tree/ledger.jsonl"
ENVS = ".opendaisugi/warden/envelopes"


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# Branching: git work trees, replies by session, a stable request order
# ---------------------------------------------------------------------------

import re  # noqa: E402


def _git(cwd: Path, *args: str) -> None:
    gitfix.run(cwd, *args)


def _lay_out_git(tree: dict[str, Any], home: Path) -> None:
    """Each ``git`` entry: a repository at the entry's path holding ``files``
    in one commit, and a detached worktree for each ``worktrees`` entry
    (a path under the home) with that entry's files written on top."""
    for rel, spec in sorted(tree.items()):
        repo = home / rel
        repo.mkdir(parents=True, exist_ok=True)
        _git(repo, "init", "-q", "-b", "main")
        for name, body in spec["git"]["files"].items():
            (repo / name).parent.mkdir(parents=True, exist_ok=True)
            (repo / name).write_text(body, encoding="utf-8")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-qm", "base")
        for wt, files in spec["git"].get("worktrees", {}).items():
            _git(repo, "worktree", "add", "-q", "--detach", str(home / wt), "HEAD")
            for name, body in files.items():
                (home / wt / name).parent.mkdir(parents=True, exist_ok=True)
                (home / wt / name).write_text(body, encoding="utf-8")


_plain_lay_out = garden_cases.lay_out


def _lay_out_all(tree: dict[str, Any], home: Path, t0: float) -> None:
    git = {k: v for k, v in tree.items() if "git" in v}
    _plain_lay_out({k: v for k, v in tree.items() if k not in git}, home, t0)
    _lay_out_git(git, home)


garden_cases.lay_out = _lay_out_all

_plain_read_tree = garden_cases.read_tree


def _read_tree(home: Path) -> dict[str, Any]:
    """garden_cases.read_tree without git's own files (their bytes carry
    object ids and times that differ on every run)."""
    out = _plain_read_tree(home)
    return {k: v for k, v in out.items() if ".git" not in Path(k).parts}


garden_cases.read_tree = _read_tree

_plain_normalize = garden_cases.normalize
_SEED = re.compile(r'("seed\\?": \\?")[0-9a-f]{16}')
_WALL = re.compile(r'("wall_ms\\?": )[0-9]+')


def _normalize(result: dict[str, Any], home: str, t0: float) -> dict[str, Any]:
    """garden_cases.normalize, the branches' wall time (it differs on every
    run) fixed, and the children's requests in session order (parallel
    children log in the order they happen to start)."""
    reqs = result.get("requests")
    if isinstance(reqs, list):
        result = {**result, "requests": sorted(reqs, key=lambda r: json.dumps(r, sort_keys=True))}
    out = _plain_normalize(result, home, t0)
    led = (out.get("tree") or {}).get(".opendaisugi/tree/ledger.jsonl")
    if isinstance(led, dict) and isinstance(led.get("text"), str) and "branch_of" in led["text"]:
        # Parallel children record their process groups as they start.
        lines = led["text"].split("\n")
        i = 0
        while i < len(lines):
            if lines[i].startswith('{"event": "group"'):
                j = i
                while j < len(lines) and lines[j].startswith('{"event": "group"'):
                    j += 1
                lines[i:j] = sorted(lines[i:j])
                i = j
            else:
                i += 1
        led["text"] = "\n".join(lines)
    text = _WALL.sub(r"\g<1>0", json.dumps(out))
    # The fit's seed hashes the measured wall time.
    text = _SEED.sub(r"\g<1>0000000000000000", text)
    return json.loads(text)


garden_cases.normalize = _normalize


def record_by_session(case: dict[str, Any], work: Path) -> None:
    """Answer the case's requests: a sprig request from ``replies_by_session``
    (by the session in its argv), any other in order from ``replies``. Run
    the oracle, answer every request with no answer yet, and run again until
    none is left."""
    table: dict[str, Any] = {}
    by_session = dict(case["replies_by_session"])
    ordered = list(case.get("replies", []))
    while True:
        case["model"] = table
        raw: list[dict[str, Any]] = []
        run_case(case, PY_CLI, work, raw)
        missing = [r for r in raw if r["key"] not in table]
        if not missing:
            break
        for r in missing:
            if r["kind"] == "sprig":
                argv = r["argv"]
                sid = argv[argv.index("--session") + 1]
                kind, ans = garden_cases.make_answer(by_session.pop(sid))
            else:
                kind, ans = garden_cases.make_answer(ordered.pop(0))
            if kind != r["kind"]:
                raise SystemExit(f"{case['name']}: a {kind} reply for a {r['kind']} request")
            table[r["key"]] = ans
    case["model"] = table


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def env(eid: str = "env_00000001", **kw: Any) -> dict[str, Any]:
    """An envelope document; permission keys go into permissions."""
    top = {
        k: kw.pop(k)
        for k in list(kw)
        if k
        in (
            "stakes",
            "deadline",
            "invariants",
            "postconditions",
            "shell_interpreter_policy",
            "parent_envelope",
        )
    }
    perms = {"file_read": ["/w/**"], "shell": True, "shell_allowlist": ["git", "ls"]}
    perms.update(kw)
    return {"id": eid, "generated_by": "test", "task": "tree case", "permissions": perms, **top}


def text(obj: Any) -> dict[str, str]:
    return {"text": json.dumps(obj, indent=1) + "\n"}


def registered(e: dict[str, Any]) -> dict[str, Any]:
    """An envelope as warden.register_envelope writes it."""
    from opendaisugi.models import Envelope

    return {"text": Envelope(**e).model_dump_json(indent=2), "mode": 0o600}


def ledger(*rows: dict[str, Any] | str) -> dict[str, Any]:
    """Ledger rows as text, each ts an hour or so before the run."""
    out = []
    for k, r in enumerate(rows):
        if isinstance(r, str):
            out.append(r)
            continue
        out.append(json.dumps({**r, "ts": "TS"}).replace('"TS"', "{NOWF:-%d}" % (3600 - k)))
    return {LEDGER: {"text": "".join(x + "\n" for x in out)}}


def root_row(s: str, eid: str = "env_root", tokens=None, turns=None, deadline=None):
    return {
        "event": "root",
        "session": s,
        "envelope_id": eid,
        "tokens": tokens,
        "turns": turns,
        "deadline": deadline,
    }


def spawn_row(p: str, s: str, eid: str = "env_kid", tokens=None, turns=None, proved=True):
    return {
        "event": "spawn",
        "parent": p,
        "session": s,
        "envelope_id": eid,
        "tokens": tokens,
        "turns": turns,
        "deadline": None,
        "proved": proved,
    }


def refused_row(p: str, s: str = "kid"):
    return {"event": "refused", "parent": p, "session": s, "reasons": ["shell_allowlist: x"]}


def end_row(s: str, tokens_used=None, turns_used=None):
    return {"event": "end", "session": s, "tokens_used": tokens_used, "turns_used": turns_used}


def case(name: str, argv: list[str], before: dict[str, Any] | None = None) -> dict[str, Any]:
    return {"kind": "cli", "name": f"tree {name}", "argv": argv, "before": before or {}}


ROOT = env("env_root")
ROOT_ENV = {f"{ENVS}/top.json": registered(ROOT)}
WARDEN_DIRS = {
    ".opendaisugi/warden": {"dir": True, "mode": 0o700},
    ENVS: {"dir": True, "mode": 0o700},
}


def grown(*rows: dict[str, Any] | str, envs: dict[str, dict[str, Any]] | None = None):
    """A tree already grown: the ledger and the registered envelopes."""
    tree = {**WARDEN_DIRS, **ledger(*rows)}
    for s, e in (envs if envs is not None else {"top": ROOT}).items():
        tree[f"{ENVS}/{s}.json"] = registered(e)
    return tree


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------

INV = {
    "type": "no_push",
    "description": "no push",
    "expr": {"op": "not_equals", "path": "command", "value": "git push"},
}
POST = {"type": "file_exists", "path": "/w/out.txt"}
BOX = [[0, 0, 0], [1, 1, 1]]


def check_cases(add: Any) -> None:
    def chk(name: str, parent: dict[str, Any], child: dict[str, Any], *flags: str):
        add(
            case(
                f"check {name}",
                ["tree", "check", "p.json", "c.json", *flags],
                {"p.json": text(parent), "c.json": text(child)},
            )
        )

    base = env()
    chk("same", base, base)
    chk("same json", base, base, "--json")
    chk("narrower", base, env(file_read=["/w/src/**"], shell_allowlist=["git"]))
    chk("narrower head", base, env(shell_allowlist=["git status", "ls -la"]))
    chk("head with a metachar", base, env(shell_allowlist=["git; rm"]))
    chk("deadline inherited", env(deadline=1900000000), base)
    chk("deadline inherited json", env(deadline=1900000000.5), base, "--json")
    chk("deadline earlier", env(deadline=1900000000), env(deadline=1890000000))
    chk("deadline later", env(deadline=1900000000), env(deadline=1900000001), "--json")
    chk("deadline child only", base, env(deadline=1890000000), "--json")
    chk("deadline int and bool", env(deadline=True), env(deadline=1), "--json")
    chk("stakes lower", env(stakes="high"), env(stakes="medium"))
    chk("stakes higher", env(stakes="medium"), env(stakes="physical"))
    chk(
        "custom steps", env(custom_step_allowlist=["a"]), env(custom_step_allowlist=["c", "a", "b"])
    )
    chk("time budget", base, env(max_execution_time_s=31, max_output_size_mb=11))
    chk("policy looser", env(shell_interpreter_policy="strict"), env(), "--json")
    chk("policy tighter", env(), env(shell_interpreter_policy="strict"))
    chk("invariant dropped", env(invariants=[INV]), base)
    chk("invariant kept", env(invariants=[INV]), env(invariants=[INV]))
    chk(
        "invariant added",
        base,
        env(invariants=[{**INV, "expr": {"op": "not_equals", "path": "command", "value": "ls"}}]),
    )
    chk("invariant unenforced", env(invariants=[{**INV, "enforce": False}]), base)
    chk(
        "invariant enforce dropped",
        env(invariants=[INV]),
        env(invariants=[{**INV, "enforce": False}]),
    )
    chk(
        "invariant reordered keys",
        env(invariants=[INV]),
        env(invariants=[dict(reversed(list(INV.items())))]),
    )
    chk("invariant opaque child", base, env(invariants=[{"type": "mystery", "description": "d"}]))
    chk(
        "invariant opaque recognized",
        base,
        env(invariants=[{"type": "velocity_bounded", "description": "d"}]),
    )
    # RF-14: numbers past 2^53 in the proof's Z3 numerals. The child keeps
    # the parent's invariant and adds one that differs only past 2^53.
    p53 = 2**53

    def ninv(t: str, pred: dict[str, Any]) -> dict[str, Any]:
        return {"type": t, "description": "d", "expr": {**pred, "path": "metadata.n"}}

    for name, pp, cp in [
        ("equals", {"op": "equals", "value": p53}, {"op": "equals", "value": p53 + 1}),
        ("in set", {"op": "in_set", "values": [p53]}, {"op": "in_set", "values": [p53 + 1]}),
        (
            "not in set",
            {"op": "not_in_set", "values": [p53 + 1]},
            {"op": "not_in_set", "values": [p53]},
        ),
        (
            "numeric range",
            {"op": "numeric_range", "min": 0, "max": p53},
            {"op": "numeric_range", "min": p53 + 1, "max": p53 + 2},
        ),
    ]:
        parent = env(invariants=[ninv("n_parent", pp)])
        chk(
            f"numbers past 2^53 {name}",
            parent,
            env(invariants=[ninv("n_parent", pp), ninv("n_child", cp)]),
            "--json",
        )
    chk("postcondition dropped", env(postconditions=[POST]), base, "--json")
    chk("robot workspace undeclared", env(workspace_bounds=BOX), base)
    chk(
        "robot workspace wider",
        env(workspace_bounds=BOX),
        env(workspace_bounds=[[0, 0, 0], [1, 2, 1]]),
    )
    chk(
        "robot workspace inside",
        env(workspace_bounds=BOX),
        env(workspace_bounds=[[0, 0, 0], [0.5, 1, 1]]),
    )
    chk("robot velocity", env(velocity_limit=1.0), env(velocity_limit=1.5))
    chk("robot torque", env(torque_limit=2), base)
    chk("robot joint", env(joint_limits={"j1": [0, 1]}), env(joint_limits={"j1": [-1, 1]}))
    chk("robot obstacles", env(obstacles=[BOX, [[2, 2, 2], [3, 3, 3]]]), env(obstacles=[BOX]))
    chk("decomposition", base, env(shell_allow_decomposition=True))
    chk("file_read outside", base, env(file_read=["/w/a.txt", "/etc/**"]))
    chk("file_write outside", env(file_write=["/w/out/**"]), env(file_write=["/w/**"]))
    chk("file_write none in parent", base, env(file_write=["/w/x"]))
    chk("file_read suffix glob", env(file_read=["*.md"]), env(file_read=["/w/a.md", "/w/*.md"]))
    chk("parent exotic glob", env(file_read=["/w/a*b*"]), env(file_read=["/w/ab"]))
    chk("child exotic glob", base, env(file_read=["/w/a*b*"]))
    chk("mcp outside", env(mcp_allowlist=["fs.read"]), env(mcp_allowlist=["fs.*"]))
    chk("network none", base, env(network=True))
    chk("network any host", env(network=True, network_hosts=["a.example"]), env(network=True))
    chk(
        "network host case",
        env(network=True, network_hosts=["A.example"]),
        env(network=True, network_hosts=["a.EXAMPLE", "b.example"]),
    )
    chk("shell none", env(shell=False), base)
    chk(
        "shell heads",
        env(shell_allowlist=["git"]),
        env(shell_allowlist=["ls", "git log", "rm -rf"]),
    )
    chk(
        "interpreter strict",
        env(shell_interpreter_policy="strict", shell_allowlist=["git", "python", "bash"]),
        env(shell_interpreter_policy="strict", shell_allowlist=["python", "bash", "git"]),
    )
    chk(
        "every part",
        env(
            stakes="high", deadline=1900000000, shell_interpreter_policy="strict", invariants=[INV]
        ),
        env(
            stakes="low",
            custom_step_allowlist=["launch"],
            max_execution_time_s=60,
            deadline=1900000001,
            shell_interpreter_policy="allow",
            file_read=["/etc/**"],
            network=True,
            shell_allowlist=["rm", "python"],
            invariants=[{"type": "mystery", "description": "d"}],
        ),
        "--json",
    )
    chk("unicode", env(shell_allowlist=["gît"]), env(shell_allowlist=["gît", "ls ✓"]))
    chk("timeout option", env(invariants=[INV]), env(invariants=[INV]), "--z3-timeout-ms", "5000")
    # Bad input.
    add(case("check no parent file", ["tree", "check", "p.json", "c.json"], {"c.json": text(base)}))
    add(case("check no child file", ["tree", "check", "p.json", "c.json"], {"p.json": text(base)}))
    add(
        case(
            "check not json",
            ["tree", "check", "p.json", "c.json"],
            {"p.json": text(base), "c.json": {"text": "{not json"}},
        )
    )
    add(
        case(
            "check not an object",
            ["tree", "check", "p.json", "c.json"],
            {"p.json": text(base), "c.json": {"text": "[1, 2]\n"}},
        )
    )
    add(
        case(
            "check not valid",
            ["tree", "check", "p.json", "c.json"],
            {"p.json": text(base), "c.json": text({"id": "x", "permissions": {"shell": "maybe"}})},
        )
    )
    add(
        case(
            "check deadline not finite",
            ["tree", "check", "p.json", "c.json"],
            {
                "p.json": text(base),
                "c.json": {"text": json.dumps(env(deadline=1.0)).replace("1.0", "NaN")},
            },
        )
    )
    chk("deadline text", base, env(deadline="soon"))
    chk("deadline numeric text", env(deadline="1900000000"), base, "--json")
    add(
        case(
            "check not utf8",
            ["tree", "check", "p.json", "c.json"],
            {"p.json": text(base), "c.json": {"hex": "7b22ff227d"}},
        )
    )


def root_cases(add: Any) -> None:
    e = {"e.json": text(ROOT)}
    add(case("root", ["tree", "root", "e.json", "--session", "top"], e))
    add(
        case(
            "root budgets",
            ["tree", "root", "e.json", "--session", "top", "--tokens", "1000", "--turns", "20"],
            {"e.json": text(env("env_root", deadline=1900000000))},
        )
    )
    add(case("root default", ["tree", "root", "e.json", "--session", "default"], e))
    add(case("root bad session", ["tree", "root", "e.json", "--session", ".x"], e))
    add(case("root session slash", ["tree", "root", "e.json", "--session", "a/b"], e))
    add(case("root session long", ["tree", "root", "e.json", "--session", "a" * 65], e))
    add(
        case(
            "root negative tokens",
            ["tree", "root", "e.json", "--session", "top", "--tokens", "-1"],
            e,
        )
    )
    add(
        case(
            "root in the ledger",
            ["tree", "root", "e.json", "--session", "top"],
            {**e, **grown(root_row("top"))},
        )
    )
    add(
        case(
            "root envelope registered",
            ["tree", "root", "e.json", "--session", "top"],
            {**e, **WARDEN_DIRS, **ROOT_ENV},
        )
    )
    add(case("root no file", ["tree", "root", "e.json", "--session", "top"]))
    add(
        case(
            "root data dir",
            ["tree", "root", "e.json", "--session", "top", "--data-dir", "dd"],
            e,
        )
    )
    add(
        case(
            "root warden root",
            ["tree", "root", "e.json", "--session", "top", "--root", "g"],
            e,
        )
    )


def spawn_cases(add: Any) -> None:
    kid = env("env_kid", file_read=["/w/src/**"], shell_allowlist=["git"])
    wide = env("env_wide", shell_allowlist=["rm"])
    files = {"k.json": text(kid), "w.json": text(wide)}

    def sp(name: str, before: dict[str, Any], *extra: str, f: str = "k.json", s: str = "kid"):
        argv = ["tree", "spawn", f, "--parent", "top", "--session", s, *extra]
        add(case(f"spawn {name}", argv, {**files, **before}))

    one = grown(root_row("top"))
    sp("ok", one)
    sp("refused", one, f="w.json")
    sp(
        "ok deadline inherited",
        grown(
            root_row("top", deadline=1900000000), envs={"top": env("env_root", deadline=1900000000)}
        ),
    )
    budget = grown(root_row("top", tokens=1000, turns=10))
    sp("budget ok", budget, "--tokens", "1000", "--turns", "10")
    sp("budget missing", budget)
    sp("budget too big", budget, "--tokens", "1001", "--turns", "3")
    used = grown(
        root_row("top", tokens=1000, turns=10),
        spawn_row("top", "a", tokens=600, turns=5),
        spawn_row("top", "b", tokens=300, turns=5),
        end_row("b", tokens_used=100, turns_used=1),
        envs={"top": ROOT, "a": kid},
    )
    sp("budget left after an end", used, "--tokens", "301", "--turns", "1")
    sp("budget fits after an end", used, "--tokens", "200", "--turns", "1")
    sp("unbounded parent", one, "--tokens", "5")
    sp("parent unknown", grown())
    sp("parent ended", grown(root_row("top"), end_row("top")))
    sp("session in the ledger", grown(root_row("top"), spawn_row("top", "kid")), s="kid")
    sp("session registered", {**one, f"{ENVS}/kid.json": registered(kid)})
    sp("session default", one, s="default")
    sp("parent no envelope", grown(root_row("top"), envs={}))
    sp(
        "physical parent",
        grown(root_row("top"), envs={"top": env("env_root", stakes="physical")}),
        f="w.json",
    )
    three = grown(root_row("top"), refused_row("top"), refused_row("top"), refused_row("top"))
    sp("third refusal", grown(root_row("top"), refused_row("top"), refused_row("top")), f="w.json")
    sp("fourth goes to the operator", three, f="w.json")
    sp("fourth that holds", three)
    asked = grown(
        root_row("top"),
        refused_row("top"),
        refused_row("top"),
        refused_row("top"),
        {
            "event": "ask",
            "ask_id": "ask_000000000001",
            "parent": "top",
            "session": "kid",
            "tokens": None,
            "turns": None,
            "envelope": wide,
            "reasons": ['shell_allowlist: the child adds ["rm"]'],
        },
    )
    sp("ask open for the session", asked)
    sp("second ask", asked, f="w.json", s="kid2")
    sp(
        "streak reset by a spawn",
        grown(
            root_row("top"),
            refused_row("top"),
            refused_row("top"),
            refused_row("top"),
            spawn_row("top", "a"),
            envs={"top": ROOT, "a": kid},
        ),
        f="w.json",
    )
    sp(
        "nested",
        grown(root_row("top"), spawn_row("top", "mid", "env_mid"), envs={"top": ROOT, "mid": kid}),
        f="k.json",
        s="leaf",
    )
    add(
        case(
            "spawn nested under mid",
            ["tree", "spawn", "l.json", "--parent", "mid", "--session", "leaf"],
            {
                "l.json": text(
                    env("env_leaf", file_read=["/w/src/app/**"], shell_allowlist=["git log"])
                ),
                **grown(
                    root_row("top"),
                    spawn_row("top", "mid", "env_mid"),
                    envs={"top": ROOT, "mid": kid},
                ),
            },
        )
    )
    sp("no file", one, f="none.json")
    sp("bad session", one, s="../up")


def end_cases(add: Any) -> None:
    kid = env("env_kid", shell_allowlist=["git"])
    tree = grown(
        root_row("top", tokens=1000, turns=10),
        spawn_row("top", "kid", tokens=600, turns=5),
        envs={"top": ROOT, "kid": kid},
    )

    def en(name: str, before: dict[str, Any], *argv: str):
        add(case(f"end {name}", ["tree", "end", *argv], before))

    en("with use", tree, "kid", "--tokens-used", "100", "--turns-used", "2")
    en("over use", tree, "kid", "--tokens-used", "900", "--turns-used", "9")
    en("no use", tree, "kid")
    en("running children", tree, "top")
    en("unknown", tree, "nobody")
    en("twice", grown(root_row("top"), spawn_row("top", "kid"), end_row("kid")), "kid")
    en("root", grown(root_row("top")), "top")
    en(
        "no envelope file",
        grown(root_row("top"), spawn_row("top", "kid"), envs={"top": ROOT}),
        "kid",
    )


def answer_cases(add: Any) -> None:
    # A wide root, a narrow parent under it, and a child that fits between
    # them: the parent's proof fails, the root's holds, so the operator may
    # allow it. An allow never widens the tree past its root (TR-R-7).
    wide_root = env("env_root", shell_allowlist=["git", "ls", "rm"])
    mid = env("env_mid", shell_allowlist=["git"])
    between = env("env_kid", shell_allowlist=["rm"])
    outside = env("env_out", shell_allowlist=["curl"])

    def ask_row(e: dict[str, Any], aid: str = "ask_000000000001", parent: str = "mid"):
        return {
            "event": "ask",
            "ask_id": aid,
            "parent": parent,
            "session": "kid",
            "tokens": 100,
            "turns": None,
            "envelope": e,
            "reasons": ['shell_allowlist: the child adds ["rm"]'],
        }

    head = [
        root_row("top", tokens=1000),
        spawn_row("top", "mid", "env_mid", tokens=500),
        refused_row("mid"),
        refused_row("mid"),
        refused_row("mid"),
    ]
    envs = {"top": wide_root, "mid": mid}
    base = [*head, ask_row(between)]

    def an(name: str, before: dict[str, Any], *argv: str):
        add(case(f"answer {name}", ["tree", "answer", *argv], before))

    an("allow", grown(*base, envs=envs), "ask_000000000001", "allow")
    an("deny", grown(*base, envs=envs), "ask_000000000001", "deny")
    an("other word", grown(*base, envs=envs), "ask_000000000001", "maybe")
    an("unknown", grown(*base, envs=envs), "ask_00000000000f", "allow")
    an(
        "answered",
        grown(
            *base, {"event": "answer", "ask_id": "ask_000000000001", "answer": "deny"}, envs=envs
        ),
        "ask_000000000001",
        "allow",
    )
    an(
        "budget gone",
        grown(
            *base,
            spawn_row("mid", "a", tokens=450),
            envs={**envs, "a": env("env_a", shell_allowlist=["git"])},
        ),
        "ask_000000000001",
        "allow",
    )
    an("parent ended", grown(*base, end_row("mid"), envs=envs), "ask_000000000001", "allow")
    an(
        "outside the root",
        grown(*head, ask_row(outside), envs=envs),
        "ask_000000000001",
        "allow",
    )
    an(
        "root envelope gone",
        grown(*base, envs={"mid": mid}),
        "ask_000000000001",
        "allow",
    )
    an(
        "parent envelope gone",
        grown(*base, envs={"top": wide_root}),
        "ask_000000000001",
        "allow",
    )
    physical = env("env_arm", stakes="physical", shell_allowlist=["git", "ls", "rm"])
    an(
        "under a physical parent",
        grown(
            root_row("arm", tokens=1000),
            refused_row("arm"),
            refused_row("arm"),
            refused_row("arm"),
            ask_row(env("env_kid", stakes="physical", shell_allowlist=["git"]), parent="arm"),
            envs={"arm": physical},
        ),
        "ask_000000000001",
        "allow",
    )
    an(
        "under the root itself",
        grown(
            root_row("top", tokens=1000),
            refused_row("top"),
            refused_row("top"),
            refused_row("top"),
            ask_row(env("env_kid", shell_allowlist=["curl"]), parent="top"),
            envs={"top": wide_root},
        ),
        "ask_000000000001",
        "allow",
    )


def status_cases(add: Any) -> None:
    kid = env("env_kid", shell_allowlist=["git"])

    def st(name: str, before: dict[str, Any], *flags: str):
        add(case(f"status {name}", ["tree", "status", *flags], before))

    st("empty", {})
    st("empty json", {}, "--json")
    rows = [
        root_row("top", tokens=1000, turns=10, deadline=1900000000.0),
        spawn_row("top", "mid", "env_mid", tokens=600, turns=5),
        spawn_row("mid", "leaf", "env_leaf", tokens=200, turns=2),
        spawn_row("top", "b", "env_b", tokens=100, turns=None, proved=False),
        end_row("leaf", tokens_used=150, turns_used=None),
        refused_row("mid"),
        root_row("other"),
        {
            "event": "ask",
            "ask_id": "ask_000000000002",
            "parent": "other",
            "session": "x",
            "tokens": None,
            "turns": None,
            "envelope": kid,
            "reasons": ["stakes: one", "shell: two"],
        },
    ]
    st("grown", grown(*rows))
    st("grown json", grown(*rows), "--json")
    junk = [
        "not json",
        "[1, 2]",
        '{"event": "root", "session": "x", "envelope_id": "e", "ts": true}',
        '{"event": "root", "session": "x", "envelope_id": "e", "tokens": -1, "ts": 1}',
        '{"event": "root", "session": "x", "envelope_id": "e", "tokens": 1.5, "ts": 1}',
        '{"event": "spawn", "parent": "nobody", "session": "y", "envelope_id": "e", "proved": true, "ts": 1}',
        '{"event": "end", "session": "nobody", "ts": 1}',
        '{"event": "answer", "ask_id": "ask_x", "answer": "allow", "ts": 1}',
        '{"event": "wave", "ts": 1}',
        root_row("top"),
        root_row("top"),
        "",
    ]
    st("skipped rows", grown(*junk), "--json")
    st("skipped rows text", grown(*junk))
    st("data dir", {"dd/tree/ledger.jsonl": grown(root_row("top"))[LEDGER]}, "--data-dir", "dd")


def run_cases(add: Any) -> None:
    """`tree run`: the one starter, with the fake sprig as the child."""
    kid = env("env_kid", file_read=["/w/src/**"], shell_allowlist=["git"])
    wide = env("env_wide", shell_allowlist=["rm"])

    def req(e=kid, **kw):
        doc = {"task": "list the files", "envelope": e, "tools": ["Read", "Bash"]}
        doc.update(kw)
        return json.dumps(doc)

    def answer(text="done", **kw):
        body = {
            "answer": text,
            "turns": 4,
            "model_turns": 2,
            "model": "haiku",
            "usage": {"input_tokens": 30, "output_tokens": 5},
        }
        body.update(kw)
        return {"sprig": body}

    def rn(name, before, *argv, stdin=None, replies=None):
        c = case(f"run {name}", ["tree", "run", *argv], before)
        c["stdin"] = req() if stdin is None else stdin
        c["sprig"] = "fake"
        if replies:
            c["replies"] = replies
        add(c)

    budget = grown(root_row("top", tokens=1000, turns=10))
    one = grown(root_row("top"))
    base = ("--parent", "top", "--profile", "default")
    rn(
        "started",
        budget,
        *base,
        "--tokens",
        "200",
        "--turns",
        "4",
        "-",
        stdin=req(max_turns=3),
        replies=[answer()],
    )
    rn("started no budget", one, *base, "-", replies=[answer("ok")])
    rn("session given", one, *base, "--session", "kid", "-", replies=[answer()])
    rn("model", one, *base, "--model", "llama3.3:70b", "-", replies=[answer(model="m")])
    rn("refused", one, *base, "-", stdin=req(wide))
    rn("refused budget", budget, *base, "--tokens", "2000", "--turns", "1", "-")
    rn(
        "parent used lowers left",
        budget,
        *base,
        "--tokens",
        "600",
        "--turns",
        "1",
        "--parent-tokens-used",
        "500",
        "--parent-turns-used",
        "2",
        "-",
    )
    rn(
        "asked",
        grown(root_row("top"), refused_row("top"), refused_row("top"), refused_row("top")),
        *base,
        "-",
        stdin=req(wide),
    )
    rn(
        "minted after rows",
        grown(root_row("top"), refused_row("top")),
        *base,
        "-",
        replies=[answer()],
    )
    rn("profile unknown", one, "--parent", "top", "--profile", "desk", "-")
    rn("no tool backed", one, *base, "-", stdin=req(tools=["WebFetch"]))
    # coppice sends lower-case names; any case maps to the host tool, an
    # unknown name stays out (FN-R-30).
    rn(
        "lowercase tools",
        one,
        *base,
        "-",
        stdin=req(tools=["read", "BASH", "Read", "glob", "Ed\u0130t"]),
        replies=[answer()],
    )
    # A run coppice marks fenced, with no fence named, starts nothing (FN-R-28).
    rn("sprig fence required but unset", one, *base, "--sprig-fence", "-")
    rn(
        "child fails",
        one,
        *base,
        "--tokens",
        "50",
        "-",
        replies=[{"sprig_raw": "", "stderr": "sprig: boom\n", "exit": 1}],
    )
    rn("child not json", one, *base, "-", replies=[{"sprig_raw": "I did it."}])
    # A report with a negative count is no report: the child keeps its
    # whole reservation. Huge counts add up to 2**53.
    rn(
        "negative count is no report",
        budget,
        *base,
        "--tokens",
        "200",
        "--turns",
        "2",
        "-",
        replies=[answer(usage={"input_tokens": 300, "output_tokens": -250})],
    )
    rn(
        "huge counts cap",
        budget,
        *base,
        "--tokens",
        "200",
        "--turns",
        "2",
        "-",
        replies=[
            answer(
                usage={
                    "input_tokens": 2**62,
                    "output_tokens": 2**62,
                    "cache_read_input_tokens": 2**62,
                    "cache_creation_input_tokens": 2**62,
                }
            )
        ],
    )
    rn("parent not running", one, "--parent", "nobody", "--profile", "default", "-")
    rn("bad stdin", one, *base, "-", stdin="[1]")
    rn("bad stdin not json", one, *base, "-", stdin="{nope")
    rn("bad envelope", one, *base, "-", stdin=req({"permissions": {"shell": "maybe"}}))
    rn("bad max turns", one, *base, "-", stdin=req(max_turns=0))
    rn("not dash", one, *base, "x")
    rn("negative tokens", one, *base, "--tokens", "-1", "-")
    rn("bad session", one, *base, "--session", "a/b", "-")
    rn("model control character", one, *base, "--model", "a\x1b[2Jb", "-")
    rn("model c1 control", one, *base, "--model", "x\x9bm", "-")

    def en(name: str, before: dict[str, Any], *argv: str):
        add(case(f"end {name}", ["tree", "end", *argv], before))

    three = grown(
        root_row("top", tokens=1000, turns=20),
        spawn_row("top", "a", tokens=600, turns=10),
        spawn_row("a", "b", tokens=300, turns=5),
        spawn_row("b", "c", tokens=100, turns=2),
        end_row("c", tokens_used=40, turns_used=1),
        end_row("b", tokens_used=50, turns_used=2),
        envs={"top": ROOT, "a": kid},
    )
    en("branch three deep", three, "a", "--tokens-used", "5", "--turns-used", "1")
    en("branch no own report", three, "a")
    st_rows = [
        root_row("top", tokens=100, turns=5),
        spawn_row("top", "a", tokens=60, turns=2),
        end_row("a", tokens_used=150, turns_used=3),
        {
            **spawn_row("top", "b", tokens=10, turns=1),
            "profile": "desk",
            "model": "llama3.3:70b",
            "parent_tokens_used": 7,
            "parent_turns_used": 1,
        },
    ]
    add(case("status overrun", ["tree", "status"], grown(*st_rows)))
    add(case("status overrun json", ["tree", "status", "--json"], grown(*st_rows)))
    odd = [
        root_row("top"),
        {**spawn_row("top", "x"), "profile": 5},
        {**spawn_row("top", "y"), "parent_tokens_used": -1},
    ]
    add(case("status bad spawn extras", ["tree", "status", "--json"], grown(*odd)))
    # The group rows (MX-R-8): the fold keeps a group of a running node and
    # skips any other; status never shows a group.
    grp = lambda s, g: {"event": "group", "session": s, "pgid": g}  # noqa: E731
    groups = [
        root_row("top"),
        spawn_row("top", "a"),
        grp("a", 4242),
        spawn_row("top", "b"),
        end_row("b"),
        grp("b", 4243),
        grp("nobody", 4244),
        grp("a", 1),
        grp("a", 2**31),
        grp("a", "4245"),
        grp("a", True),
        grp("a", 42.0),
        {"event": "group", "session": "a"},
    ]
    add(case("status group rows", ["tree", "status", "--json"], grown(*groups)))
    add(case("status group rows text", ["tree", "status"], grown(*groups)))
    # A node with no report of its own costs the larger of its reservation
    # and its children's charges, so an overrun below it reaches the root.
    hidden = [
        root_row("top", tokens=1000, turns=20),
        spawn_row("top", "c", tokens=100, turns=5),
        spawn_row("c", "g", tokens=80, turns=2),
        end_row("g", tokens_used=150, turns_used=3),
    ]
    hidden_envs = {"top": ROOT, "c": kid}
    add(case("status overrun below a running node", ["tree", "status"], grown(*hidden)))
    add(
        case(
            "status overrun below a node with no report",
            ["tree", "status", "--json"],
            grown(*hidden, end_row("c")),
        )
    )
    en("no report rolls up the children", grown(*hidden, envs=hidden_envs), "c")
    deep = [
        *hidden[:3],
        spawn_row("g", "h", tokens=50, turns=1),
        end_row("h", tokens_used=500, turns_used=1),
        end_row("g"),
    ]
    add(case("status overrun two levels below", ["tree", "status"], grown(*deep)))


def branch_cases(add: Any) -> None:
    """`tree run` with attempts, `tree prune-branches`, `tree adopt` and the
    branch groups of `tree status` (W3K-7 to W3K-13)."""
    from opendaisugi import tree as tree_mod

    kid = env("env_kid", file_read=["/w/src/**"], shell_allowlist=["git"])
    repo = {"repo": {"git": {"files": {"a.txt": "one\n", "b.txt": "base\n"}}}}
    sess = [tree_mod.mint_session("top", n) for n in range(3)]
    base = ("--parent", "top", "--profile", "default")

    def req(**kw: Any) -> str:
        doc = {"task": "list the files", "envelope": kid, "tools": ["Read", "Bash"]}
        doc.update(kw)
        return json.dumps(doc)

    def ran(text_: str, write: dict[str, str] | None = None, tokens: int = 5) -> dict[str, Any]:
        body = {
            "answer": text_,
            "turns": 2,
            "model_turns": 1,
            "model": "haiku",
            "usage": {"input_tokens": 20, "output_tokens": tokens},
        }
        out: dict[str, Any] = {"sprig": body}
        if write:
            out["write"] = write
        return out

    died = {"sprig_raw": "", "stderr": "sprig: boom\n", "exit": 1}

    def br(
        name: str,
        before: dict[str, Any],
        *argv: str,
        stdin: str | None = None,
        by: dict[str, Any] | None = None,
        in_repo: bool = True,
        fence: list[str] | None = None,
    ) -> None:
        c = case(f"run branches {name}", ["tree", "run", *argv], before)
        if fence is not None:
            c["env"] = {"DAISUGI_SPRIG_FENCE": json.dumps(["fence", "--harness", "sprig", *fence])}
        c["stdin"] = req(attempts=2, why="operator") if stdin is None else stdin
        c["sprig"] = "fake"
        if in_repo:
            c["cwd"] = "repo"
        if by is not None:
            c["replies_by_session"] = by
        add(c)

    budget = {**grown(root_row("top", tokens=1000, turns=20)), **repo}
    two = ("--tokens", "100", "--turns", "4", "-")
    # The request fields.
    for label, kw in [
        ("attempts nine", {"attempts": 9, "why": "operator"}),
        ("attempts true", {"attempts": True, "why": "operator"}),
        ("attempts text", {"attempts": "2", "why": "operator"}),
        ("attempts zero", {"attempts": 0, "why": "operator"}),
        ("why missing", {"attempts": 2}),
        ("why other", {"attempts": 2, "why": "because"}),
    ]:
        br(f"bad {label}", budget, *base, *two, stdin=req(**kw))
    br("one is the single child", budget, *base, *two, stdin=req(attempts=1), in_repo=False)
    br("session with attempts", budget, *base, "--session", "kid", *two)
    br("check malformed", budget, *base, "--check", "nope", *two)
    # No git workspace, no budget: nothing starts and no worktree is left.
    br("not git", grown(root_row("top", tokens=1000, turns=20)), *base, *two, in_repo=False)
    # Another grant that covers the repository's git directory: nothing starts.
    fenced = (*base, "--sprig-fence", *two)
    for label, g in [
        ("allow the repo", ["--allow", "{HOME}/repo:rw"]),
        ("allow above the repo", ["--allow", "{HOME}:rw"]),
        ("allow inside the git dir", ["--allow", "{HOME}/repo/.git/hooks:rw"]),
        ("allow equals form", ["--allow={HOME}/repo/.git:rw"]),
        ("workdir inside the git dir", ["--workdir", "{HOME}/repo/.git/hooks"]),
        # read as the fence reads them: ~ is HOME, a relative path lies under
        # the child's worktree ({HOME}/.opendaisugi/tree/branches/<session>)
        ("allow tilde", ["--allow", "~/repo:rw"]),
        ("workdir home", ["--workdir=~"]),
        ("allow relative", ["--allow", "../../../../repo/.git:rw"]),
    ]:
        br(f"fence covers the repo {label}", budget, *fenced, fence=g)
    wide = {**budget, f"{ENVS}/default.json": registered(env(file_write=["{HOME}/repo/**"]))}
    br("fence covers the repo envelope", wide, *fenced, fence=[])
    # Without the socket fence a child can start what the sweep cannot stop.
    br("fence allow weak ipc", budget, *fenced, fence=["--allow-weak-ipc"])
    br("budget", grown(root_row("top", tokens=150, turns=20)) | repo, *base, *two)
    br("turns budget", grown(root_row("top", tokens=1000, turns=5)) | repo, *base, *two)
    br("profile unknown", budget, "--parent", "top", "--profile", "desk", *two)
    br("not a running parent", budget, "--parent", "nobody", "--profile", "default", *two)
    # Two children that edit differently: ranked by cost, a card opens.
    edits = {
        sess[0]: ran("first answer", {"a.txt": "two\n"}, tokens=9),
        sess[1]: ran("second answer", {"a.txt": "three\nmore\n"}, tokens=4),
    }
    br("two ran", budget, *base, *two, by=edits)
    br("two ran parallel one", budget, *base, "--parallel", "1", *two, by=edits)
    br("two ran text", budget, *base, *two, by=edits, stdin=req(attempts=2, why="failed_tests"))
    br(
        "one failed",
        budget,
        *base,
        *two,
        by={sess[0]: ran("good", {"a.txt": "two\n"}), sess[1]: died},
    )
    br("all failed", budget, *base, *two, by={sess[0]: died, sess[1]: died})
    br(
        "same edit",
        budget,
        *base,
        *two,
        by={
            sess[0]: ran("same", {"a.txt": "two\n"}, tokens=5),
            sess[1]: ran("same", {"a.txt": "two\n"}, tokens=6),
        },
    )
    br(
        "three attempts",
        budget,
        *base,
        *two,
        stdin=req(attempts=3, why="low_confidence"),
        by={
            sess[0]: ran("one", {"a.txt": "2\n"}),
            sess[1]: ran("two", {"a.txt": "22\n"}, tokens=7),
            sess[2]: died,
        },
    )
    # A hard check from the starter's side decides.
    check = 'tests=sh -c "grep -q two a.txt"'
    br(
        "check",
        budget,
        *base,
        "--check",
        check,
        *two,
        by={sess[0]: ran("pass", {"a.txt": "two\n"}), sess[1]: ran("fail", {"a.txt": "other\n"})},
    )
    br(
        "check all fail",
        budget,
        *base,
        "--check",
        "never=false",
        *two,
        by={sess[0]: ran("a", {"a.txt": "2\n"}), sess[1]: ran("b", {"a.txt": "3\n"})},
    )

    # The tree and its branches as `tree status` shows them, and the
    # worktrees `tree prune-branches` and `tree adopt` act on.
    one = gitfix.commit_id({"a.txt": "one\n"})

    def brow(s: str, rid: str) -> dict[str, Any]:
        return {
            **spawn_row("top", s, tokens=100, turns=4),
            "branch_of": rid,
            "why": "operator",
            "base": one,
            "repo": "{HOME}/repo/.git",
        }

    rid = "br_000000000001"
    wts = {
        f".opendaisugi/tree/branches/{s}": {"a.txt": f"edit {s}\n"}
        for s in ("n-aaaaaaaaaaaa", "n-bbbbbbbbbbbb")
    }
    git = {"repo": {"git": {"files": {"a.txt": "one\n"}, "worktrees": wts}}}
    rows = (
        root_row("top", tokens=1000, turns=20),
        brow("n-aaaaaaaaaaaa", rid),
        brow("n-bbbbbbbbbbbb", rid),
        end_row("n-aaaaaaaaaaaa", 30, 2),
        end_row("n-bbbbbbbbbbbb", 20, 2),
    )

    def opened(open_: bool) -> dict[str, Any]:
        row = {
            "choice_id": "ch_000000000001",
            "ranking_id": rid,
            "project": "/w/app",
            "task": "t",
            "event": "opened",
            "options": {
                "survivors": [
                    {"id": "n-aaaaaaaaaaaa", "content_hash": "sha256:aaaa"},
                    {"id": "n-bbbbbbbbbbbb", "content_hash": "sha256:bbbb"},
                ],
                "eliminated": [],
            },
            "chosen": "n-aaaaaaaaaaaa",
            "status": "provisional",
            "facts": {
                "order": ["n-aaaaaaaaaaaa", "n-bbbbbbbbbbbb"],
                "decided_by": ["cost"],
                "quality_tiers": [["n-aaaaaaaaaaaa", "n-bbbbbbbbbbbb"]],
                "confidence": 0.5,
                "cost": {},
                "summary": {},
                "where": {},
                "run": None,
                "adopt": "daisugi tree adopt n-aaaaaaaaaaaa",
            },
            "ts": "TS",
        }
        out = [json.dumps(row).replace('"TS"', "{NOWF:-3000}")]
        if not open_:
            out.append(
                json.dumps(
                    {
                        "choice_id": "ch_000000000001",
                        "ranking_id": rid,
                        "event": "confirmed",
                        "how": "drop",
                        "ts": "TS",
                    }
                ).replace('"TS"', "{NOWF:-60}")
            )
        return {
            ".opendaisugi/journal/rankings/choices.jsonl": {"text": "".join(x + "\n" for x in out)}
        }

    grown_branches = {**grown(*rows), **git}
    add(case("status branches", ["tree", "status"], {**grown_branches, **opened(True)}))
    add(
        case(
            "status branches json", ["tree", "status", "--json"], {**grown_branches, **opened(True)}
        )
    )
    add(case("status branches no card", ["tree", "status"], grown_branches))
    add(
        case(
            "prune-branches open card keeps worktrees",
            ["tree", "prune-branches"],
            {**grown_branches, **opened(True)},
        )
    )
    add(
        case(
            "prune-branches closed card removes them",
            ["tree", "prune-branches", "--json"],
            {**grown_branches, **opened(False)},
        )
    )
    # A ranking with no card keeps its worktrees (a lone survivor is copied
    # only by `tree adopt`) until the nodes are older than the card decay.
    add(case("prune-branches no card keeps them", ["tree", "prune-branches"], grown_branches))
    old = 15 * 86400
    aged_rows = [
        json.dumps({**r, "ts": "TS"}).replace('"TS"', "{NOWF:-%d}" % (old + 100 - k))
        for k, r in enumerate(rows)
    ]
    aged = {**grown(*aged_rows), **git}
    add(case("prune-branches no card aged out removes them", ["tree", "prune-branches"], aged))
    add(case("prune-branches aged out json", ["tree", "prune-branches", "--json"], aged))
    add(case("prune-branches nothing", ["tree", "prune-branches"], {}))
    adopt = ["tree", "adopt", "n-aaaaaaaaaaaa", "--cwd", "repo"]
    add(case("adopt clean", adopt, {**grown_branches, **opened(True)}))
    add(case("adopt json", [*adopt, "--json"], {**grown_branches, **opened(True)}))
    add(case("adopt dirty file", adopt, {**grown_branches, "repo/a.txt": {"text": "mine\n"}}))
    add(case("adopt unknown", ["tree", "adopt", "nope"], grown_branches))
    add(case("adopt bad name", ["tree", "adopt", "../x"], grown_branches))
    same = {
        "repo": {
            "git": {
                "files": {"a.txt": "one\n"},
                "worktrees": {".opendaisugi/tree/branches/n-cccccccccccc": {}},
            }
        }
    }
    same_rows = grown(root_row("top", tokens=1000, turns=20), brow("n-cccccccccccc", rid))
    nothing = {**same_rows, **same}
    add(
        case("adopt changed nothing", ["tree", "adopt", "n-cccccccccccc", "--cwd", "repo"], nothing)
    )
    # The ledger names no base for the worktree: nothing is read or applied.
    add(case("adopt not in the ledger", ["tree", "adopt", "n-cccccccccccc", "--cwd", "repo"], same))
    # --cwd names another repository than the one the branch came from.
    other = {"other": {"git": {"files": {"a.txt": "other\n"}}}}
    add(
        case(
            "adopt into another repo",
            ["tree", "adopt", "n-aaaaaaaaaaaa", "--cwd", "other", "--json"],
            {**grown_branches, **other},
        )
    )


def register_cases(add: Any) -> None:
    parent = env("env_p", deadline=1900000000)
    files = {
        "c.json": text(env("env_c", shell_allowlist=["git"])),
        "w.json": text(env("env_w", shell_allowlist=["rm"])),
        **WARDEN_DIRS,
        f"{ENVS}/p.json": registered(parent),
    }

    def rg(name: str, *argv: str, before: dict[str, Any] | None = None):
        add(case(f"register {name}", ["warden", "register", *argv], {**files, **(before or {})}))

    rg("child proved", "c.json", "--session", "c", "--parent", "p")
    rg("child refused", "w.json", "--session", "c", "--parent", "p")
    rg("no session", "c.json", "--parent", "p")
    rg("empty parent", "c.json", "--session", "c", "--parent", "")
    rg("parent missing", "c.json", "--session", "c", "--parent", "q")
    rg("default child", "c.json", "--session", "default", "--parent", "p")
    rg("bad child id", "c.json", "--session", "a b", "--parent", "p")
    rg(
        "child registered",
        "c.json",
        "--session",
        "c",
        "--parent",
        "p",
        before={f"{ENVS}/c.json": registered(env("env_old"))},
    )
    rg(
        "parent default",
        "c.json",
        "--session",
        "c",
        "--parent",
        "default",
        before={f"{ENVS}/default.json": registered(env("env_d"))},
    )
    rg("no parent as before", "w.json", "--session", "c")


def bundle_cases(add) -> None:
    """`agent-profiles bundle --fd N`: the operator's table with its keys.
    The runner reads stdout, so the cases write to descriptor 1; a
    descriptor that is not open is the error case. The keys are fake."""

    def config(text: str) -> dict[str, Any]:
        return {".opendaisugi/config.yaml": {"text": text}}

    def bn(name, argv, before=None, env=None):
        c = {
            "kind": "cli",
            "name": f"agent-profiles bundle {name}",
            "argv": ["agent-profiles", "bundle", *argv],
            "before": before or {},
        }
        if argv[:2] == ["--fd", "3"]:
            c["fd3"] = True
        if env:
            c["env"] = env
        add(c)

    table = (
        "agent_profiles:\n"
        "  desk:\n    wire: openai\n    base_url: http://127.0.0.1:1/v1\n"
        "    model: m1\n    key_env: DESK_API_KEY\n"
        "  cloud:\n    wire: anthropic\n    key_env: CLOUD_API_KEY\n"
        "  local:\n    wire: claude-code\n    model: haiku\n"
    )
    keys = {"DESK_API_KEY": "sk-test-desk", "CLOUD_API_KEY": "sk-test-cloud"}
    bn("no table", ["--fd", "3"])
    bn("empty config", ["--fd", "3"], config("model: x\n"))
    bn("table with keys", ["--fd", "3"], config(table), keys)
    bn("one key not set", ["--fd", "3"], config(table), {"DESK_API_KEY": "sk-test-desk"})
    bn("empty key counts as not set", ["--fd", "3"], config(table), {**keys, "CLOUD_API_KEY": ""})
    bn("no key set", ["--fd", "3"], config(table))
    bn(
        "invalid entries",
        ["--fd", "3"],
        config(
            "agent_profiles:\n  ok:\n    wire: claude-code\n"
            "  bad:\n    wire: smoke\n  default:\n    wire: openai\n  odd: 3\n"
        ),
    )
    bn("table is not a mapping", ["--fd", "3"], config("agent_profiles: [a, b]\n"))
    max_ok = "agent_profiles:\n  big:\n    wire: openai\n    max_tokens: 4096\n  none:\n    wire: anthropic\n    max_tokens:\n"
    max_bad = (
        "agent_profiles:\n  zero:\n    wire: openai\n    max_tokens: 0\n"
        "  text:\n    wire: openai\n    max_tokens: '400'\n"
        "  flag:\n    wire: openai\n    max_tokens: true\n"
        "  half:\n    wire: openai\n    max_tokens: 1.5\n"
        "  huge:\n    wire: openai\n    max_tokens: 1000001\n"
        "  top:\n    wire: openai\n    max_tokens: 1000000\n"
        "  cc:\n    wire: claude-code\n    max_tokens: 400\n"
    )
    bn("max_tokens is read and not carried", ["--fd", "3"], config(max_ok))
    bn("max_tokens invalid", ["--fd", "3"], config(max_bad))
    bn("fd 0 is refused", ["--fd", "0"], config(table), keys)
    bn("fd 1 is refused", ["--fd", "1"], config(table), keys)
    bn("fd 2 is refused", ["--fd", "2"], config(table), keys)
    bn("fd not open", ["--fd", "37"], config(table), keys)
    bn("fd negative", ["--fd", "-1"], config(table), keys)
    bn("fd not a number", ["--fd", "x"], config(table), keys)
    bn("fd missing", [], config(table), keys)
    bn("unknown option", ["--fd", "1", "--to", "2"])
    add(
        {
            "kind": "cli",
            "name": "agent-profiles unknown command",
            "argv": ["agent-profiles", "frob"],
            "before": {},
        }
    )

    def ls(name, argv, before=None, env=None):
        c = {
            "kind": "cli",
            "name": f"agent-profiles list {name}",
            "argv": ["agent-profiles", "list", *argv],
            "before": before or {},
        }
        if env:
            c["env"] = env
        add(c)

    ls("no table", ["--json"])
    ls("table, keys set", ["--json"], config(table), keys)
    ls("table, one key not set", ["--json"], config(table), {"DESK_API_KEY": "sk-test-desk"})
    ls("empty key counts as not set", ["--json"], config(table), {**keys, "CLOUD_API_KEY": ""})
    ls("table, plain lines", [], config(table), keys)
    ls(
        "invalid entries",
        ["--json"],
        config(
            "agent_profiles:\n  ok:\n    wire: claude-code\n"
            "  bad:\n    wire: smoke\n  default:\n    wire: openai\n  odd: 3\n"
        ),
    )
    ls("max_tokens valid", ["--json"], config(max_ok))
    ls("max_tokens invalid", ["--json"], config(max_bad))
    ls("unknown option", ["--to", "2"])


def link_cases(add: Any) -> None:
    """GR r1 MAJOR-1 (GR-R-1, GR-R-7): the tree reads an envelope as the warden
    does. A registered envelope that is a second name of a file, group
    writable, or a link is one the warden refuses to read, so the tree cannot
    read it either: a spawn or an answer that needs it is refused. Each case
    has a twin without the plant (``spawn ok``, ``answer allow``), so the set
    fails when the check is taken out and when it is too strict."""
    kid = env("env_kid", file_read=["/w/src/**"], shell_allowlist=["git"])
    files = {"k.json": text(kid)}
    top = f"{ENVS}/top.json"

    def planted(name: str) -> dict[str, Any]:
        """The envelope of ``top`` as the plant leaves it."""
        base = grown(root_row("top"))
        if name == "hard link":
            return {**base, "second-name.json": {"hardlink": top}}
        if name == "group writable":
            return {**base, top: {**registered(ROOT), "mode": 0o664}}
        if name == "symlink":
            return {**base, top: {"symlink": "real-top.json"}, "real-top.json": registered(ROOT)}
        raise ValueError(name)

    for plant in ("hard link", "group writable", "symlink"):
        add(
            case(
                f"spawn parent envelope {plant}",
                ["tree", "spawn", "k.json", "--parent", "top", "--session", "kid"],
                {**files, **planted(plant)},
            )
        )
    # An answer needs the root's envelope (the allow never widens past it)
    # and the parent's (a physical parent starts nothing).
    wide_root = env("env_root", shell_allowlist=["git", "ls", "rm"])
    mid = env("env_mid", shell_allowlist=["git"])
    between = env("env_kid", shell_allowlist=["rm"])
    ask = {
        "event": "ask",
        "ask_id": "ask_000000000001",
        "parent": "mid",
        "session": "kid",
        "tokens": 100,
        "turns": None,
        "envelope": between,
        "reasons": ['shell_allowlist: the child adds ["rm"]'],
    }
    rows = [
        root_row("top", tokens=1000),
        spawn_row("top", "mid", "env_mid", tokens=500),
        refused_row("mid"),
        refused_row("mid"),
        refused_row("mid"),
        ask,
    ]
    envs = {"top": wide_root, "mid": mid}
    argv = ["tree", "answer", "ask_000000000001", "allow"]
    add(case("answer allow twin", argv, grown(*rows, envs=envs)))
    for plant in ("hard link", "group writable", "symlink"):
        for who in ("top", "mid"):
            tree = grown(*rows, envs=envs)
            f = f"{ENVS}/{who}.json"
            real = tree[f]
            if plant == "hard link":
                tree["second-name.json"] = {"hardlink": f}
            elif plant == "group writable":
                tree[f] = {**real, "mode": 0o664}
            else:
                tree[f] = {"symlink": f"real-{who}.json"}
                tree[f"real-{who}.json"] = real
            label = "root" if who == "top" else "parent"
            add(case(f"answer {label} envelope {plant}", argv, tree))
    # A physical parent whose envelope the warden refuses cannot be shown not
    # to be physical: the allow is refused, not read as "no envelope".
    physical = env("env_mid", stakes="physical", shell_allowlist=["git", "ls", "rm"])
    tree = grown(*rows, envs={"top": wide_root, "mid": physical})
    tree["second-name.json"] = {"hardlink": f"{ENVS}/mid.json"}
    add(case("answer physical parent envelope hard link", argv, tree))
    # N2 (GR-R-17): a running non-root parent always has an envelope. One
    # that is missing or spoiled cannot be shown not to be physical, so the
    # allow is refused, not read as "no envelope".
    tree = grown(*rows, envs=envs)
    tree[f"{ENVS}/mid.json"] = {"text": "{ not json", "mode": 0o600}
    add(case("answer parent envelope corrupt", argv, tree))
    tree = grown(*rows, envs=envs)
    del tree[f"{ENVS}/mid.json"]
    add(case("answer parent envelope missing", argv, tree))
    # end removes through the directory fd: a symlinked envelopes/ is left alone.
    tree = {
        **ledger(root_row("top")),
        ".opendaisugi/warden": {"dir": True, "mode": 0o700},
        "real-envelopes": {"dir": True, "mode": 0o700},
        "real-envelopes/top.json": registered(ROOT),
        ENVS: {"symlink": "real-envelopes"},
    }
    add(case("end envelopes dir symlink", ["tree", "end", "top"], tree))
    # N3: envelopes/ moved aside with no link: nothing found, and that is said.
    tree = {**ledger(root_row("top")), ".opendaisugi/warden": {"dir": True, "mode": 0o700}}
    add(case("end envelope not found", ["tree", "end", "top"], tree))
    # The register parent check reads the parent through the same checks.
    reg = {"k.json": text(kid), **planted("hard link")}
    add(
        case(
            "register parent envelope hard link",
            ["warden", "register", "k.json", "--parent", "top", "--session", "kid"],
            reg,
        )
    )


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    check_cases(C.append)
    root_cases(C.append)
    spawn_cases(C.append)
    end_cases(C.append)
    answer_cases(C.append)
    status_cases(C.append)
    run_cases(C.append)
    branch_cases(C.append)
    register_cases(C.append)
    bundle_cases(C.append)
    link_cases(C.append)
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = build_cases()
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
    out_dir: Path = args.out
    out_dir.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out_dir / "cases.jsonl").exists() and not args.fresh:
        for ln in (out_dir / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
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
        if c.get("replies_by_session"):
            record_by_session(c, work)
        elif c.get("replies"):
            record_replies(c, work)
        c["expect"] = run_case(c, cmd_for(c, None), work)
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} exit={c['expect']['exit']} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION}
    manifest["cases.jsonl"] = write_jsonl(out_dir / "cases.jsonl", cases)
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

"""Synthetic stage-K2 cases: `daisugi run` (the supervisor: verify, per-step
verify, approval, the four executors, stage-2 postconditions, receipts,
integrity, the journal) and `daisugi orchestrate` (decomposition, sizing,
budgets, the task executor, synthesis, Tier-0 reuse), run through the
Python oracle.

    uv run --no-sync python clients/k2_cases.py [--out clients/fixtures/k2] [--only NAME]

Each case runs a command as garden_cases runs one: a scratch HOME, the fake
`claude` and the fake model server answering only exact request bytes, and
the exit code, stdout, stderr, the tree after and the model requests
recorded. A step that touches the world touches only the scratch HOME.

Three values differ run to run and are made stable here, on top of
garden_cases' normalization: a run's id (run_<hex8>), how long a step took
(duration_ms, and the "1.2 ms" a run prints), and a receipt's
evidence_hash, which is the SHA-256 of evidence holding duration_ms. The
hash is checked against its own row's evidence and recorded as
"{HASH OK}" or "{HASH BAD}". Every path, key and task is synthetic.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
from garden_cases import (  # noqa: E402
    PY_CLI,
    body_id,
    record_replies,
    run_case,
    write_jsonl,
)
from pathway_cases import REPO, lexical, pathway, put_row, row_spec  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "k2"
SCRATCH = Path(
    os.environ.get("DAISUGI_K2_SCRATCH") or Path.home() / "opendaisugi-scratch" / "k2" / "runs"
)
CASE_VERSION = 1
CONFIG = ".opendaisugi/config.yaml"
LEX = {CONFIG: {"text": "matcher_model: lexical\n"}}
KEY = {"ANTHROPIC_API_KEY": "sk-test-k2-000000000000"}
API = {**KEY, "OPENDAISUGI_LLM_BACKEND": "api"}
CC = {"OPENDAISUGI_LLM_BACKEND": "claude-code"}

# ---------------------------------------------------------------------------
# Normalization
# ---------------------------------------------------------------------------

garden_cases._MINTED.append((re.compile(r"\brun_[0-9a-f]{8}\b"), "run"))
_JSON_MS = re.compile(r'(\\"duration_ms\\": )-?[0-9][0-9.eE+-]*')
_TEXT_MS = re.compile(r"[0-9]+\.[0-9] ms\)")
_EVIDENCE_MS = re.compile(r'("duration_ms": )-?[0-9][0-9.eE+-]*')
_TOTAL_MS = re.compile(r'("total_duration_ms": \{"float": ")[^"]*(")')
_generic_normalize = garden_cases.normalize


def _receipts(result: dict[str, Any]) -> None:
    from opendaisugi.models import compute_evidence_hash

    for entry in (result.get("tree") or {}).values():
        tables = (entry.get("db") or {}).get("tables") or {}
        for row in tables.get("receipts", []):
            text = row.get("evidence_json")
            if not isinstance(text, str):
                continue
            try:
                good = compute_evidence_hash(json.loads(text)) == row.get("evidence_hash")
            except ValueError:
                good = False
            row["evidence_hash"] = "{HASH OK}" if good else "{HASH BAD}"
            row["evidence_json"] = _EVIDENCE_MS.sub(r'\1"{MS}"', text)


def normalize(result: dict[str, Any], home: str, t0: float) -> dict[str, Any]:
    _receipts(result)
    result["stdout"] = _TEXT_MS.sub("{MS} ms)", result.get("stdout", ""))
    out = _generic_normalize(result, home, t0)
    text = json.dumps(out, ensure_ascii=True)
    text = _JSON_MS.sub(r"\1{MS}", text)
    text = _TOTAL_MS.sub(r"\1{MS}\2", text)
    return json.loads(text)


# run_case reads the module's normalize.
garden_cases.normalize = normalize
_generic_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """garden_cases.lay_out, and a "link" entry: a symlink to a path under
    {HOME}, made after everything else."""
    _generic_lay_out({k: v for k, v in tree.items() if "link" not in v}, home, t0)
    for rel, spec in sorted(tree.items()):
        if "link" in spec:
            p = home / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            os.symlink(spec["link"].replace("{HOME}", str(home)), p)


garden_cases.lay_out = lay_out


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def yml(obj: Any) -> dict[str, str]:
    """A file entry holding obj as YAML, as yaml.safe_dump writes it once
    {HOME} is filled in (a path in its place, so no quoting differs)."""
    import yaml

    stand_in = json.loads(json.dumps(obj).replace("{HOME}", "/HOMEDIR"))
    return {"text": yaml.safe_dump(stand_in, sort_keys=False).replace("/HOMEDIR", "{HOME}")}


def perm(**p: Any) -> dict[str, Any]:
    base: dict[str, Any] = {
        "file_read": [],
        "file_write": [],
        "network": False,
        "network_hosts": [],
        "shell": False,
        "shell_allowlist": [],
    }
    base.update(p)
    return base


def env_doc(i: int = 1, *, post=None, fallback=None, **p: Any) -> dict[str, Any]:
    d: dict[str, Any] = {
        "id": f"env_{i:08x}",
        "generated_by": "test",
        "task": f"task {i}",
        "permissions": perm(**p),
    }
    if post is not None:
        d["postconditions"] = post
    if fallback is not None:
        d["fallback"] = fallback
    return d


def plan_doc(steps: list[dict[str, Any]], i: int = 1) -> dict[str, Any]:
    return {"id": f"plan_{i:08x}", "source": "script", "task": f"task {i}", "steps": steps}


def sh(sid: str, cmd: str, deps=(), **kw) -> dict[str, Any]:
    return {"id": sid, "type": "shell", "command": cmd, "depends_on": list(deps), **kw}


def rd(sid: str, path: str, deps=(), **kw) -> dict[str, Any]:
    return {"id": sid, "type": "file_read", "path": path, "depends_on": list(deps), **kw}


def wr(sid: str, path: str, content: str, deps=(), **kw) -> dict[str, Any]:
    return {
        "id": sid,
        "type": "file_write",
        "path": path,
        "content": content,
        "depends_on": list(deps),
        **kw,
    }


def net(sid: str, url: str, deps=()) -> dict[str, Any]:
    return {"id": sid, "type": "network", "url": url, "depends_on": list(deps)}


def task(sid: str, prompt: str, deps=()) -> dict[str, Any]:
    return {"id": sid, "type": "task", "prompt": prompt, "depends_on": list(deps)}


W = "{HOME}/w"


def run_case_spec(
    name: str,
    steps: list[dict[str, Any]],
    envelope: dict[str, Any],
    *,
    flags=("--yes",),
    before=None,
    env=None,
    **kw,
) -> dict[str, Any]:
    tree: dict[str, Any] = {
        "p.yaml": yml(plan_doc(steps)),
        "e.yaml": yml(envelope),
        "w": {"dir": True},
    }
    tree.update(before or {})
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": ["run", "p.yaml", "-e", "e.yaml", *flags],
        "before": tree,
    }
    if env:
        c["env"] = env
    c.update(kw)
    return c


# ---------------------------------------------------------------------------
# `daisugi run`
# ---------------------------------------------------------------------------


def build_run_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    echo = env_doc(shell=True, shell_allowlist=["echo", "false", "true", "printf"])
    rw = env_doc(file_read=[f"{W}/**"], file_write=[f"{W}/**"])
    # Shell.
    add(run_case_spec("run shell one step", [sh("s1", "echo hi")], echo))
    add(run_case_spec("run shell json", [sh("s1", "echo hi")], echo, flags=("--yes", "--json")))
    add(
        run_case_spec(
            "run shell two steps ordered",
            [sh("b", "echo second", deps=["a"]), sh("a", "echo first")],
            echo,
            flags=("--yes", "--json"),
        )
    )
    add(run_case_spec("run shell allowlist approves", [sh("s1", "echo hi")], echo, flags=()))
    add(
        run_case_spec(
            "run shell metachar not allowlisted", [sh("s1", "echo hi > x")], echo, flags=()
        )
    )
    add(
        run_case_spec(
            "run shell failed step", [sh("s1", "false"), sh("s2", "echo no", ["s1"])], echo
        )
    )
    add(
        run_case_spec(
            "run shell failed json",
            [sh("s1", "printf 'bad\\n'; false")],
            env_doc(
                shell=True, shell_allowlist=["printf", "false"], shell_allow_decomposition=True
            ),
            flags=("--yes", "--json"),
        )
    )
    add(
        run_case_spec(
            "run shell stderr merged",
            [sh("s1", "printf 'x' >&2")],
            env_doc(shell=True, shell_allowlist=["printf"], shell_allow_decomposition=True),
        )
    )
    add(
        run_case_spec(
            "run shell many lines",
            [sh("s1", "printf '1\\n2\\n3\\n4\\n5\\n6\\n7\\n'")],
            env_doc(shell=True, shell_allowlist=["printf"]),
        )
    )
    add(run_case_spec("run shell no output", [sh("s1", "true")], echo))
    add(run_case_spec("run shell non-ascii", [sh("s1", "echo héllo ☃")], echo))
    add(
        run_case_spec(
            "run shell control characters",
            [sh("s1", "printf '\\033[1mX\\033[0m\\tY\\r\\n'")],
            env_doc(shell=True, shell_allowlist=["printf"]),
            flags=("--yes", "--json"),
        )
    )
    add(
        run_case_spec(
            "run shell control characters text",
            [sh("s1", "printf '\\033[1mX\\033[0m\\tY\\r\\n'")],
            env_doc(shell=True, shell_allowlist=["printf"]),
        )
    )
    # Verify before run.
    add(run_case_spec("run rejected not allowlisted", [sh("s1", "rm -rf /work")], echo))
    add(
        run_case_spec(
            "run rejected json", [sh("s1", "rm -rf /work")], echo, flags=("--yes", "--json")
        )
    )
    add(run_case_spec("run rejected shell off", [sh("s1", "echo hi")], env_doc()))
    add(
        run_case_spec(
            "run rejected cycle", [sh("a", "echo a", ["b"]), sh("b", "echo b", ["a"])], echo
        )
    )
    add(run_case_spec("run rejected dangling dep", [sh("a", "echo a", ["zz"])], echo))
    add(
        run_case_spec(
            "run rejected read outside", [rd("s1", "/etc/hostname")], rw, flags=("--yes",)
        )
    )
    add(
        run_case_spec(
            "run rejected envelope inconsistent",
            [sh("s1", "echo hi")],
            env_doc(shell=False, shell_allowlist=["echo"]),
        )
    )
    # Approval.
    add(run_case_spec("run approval denied no tty", [rd("s1", f"{W}/a.txt")], rw, flags=()))
    add(
        run_case_spec(
            "run approval env never",
            [rd("s1", f"{W}/a.txt")],
            rw,
            flags=(),
            env={"DAISUGI_APPROVE": "never"},
        )
    )
    add(
        run_case_spec(
            "run approval env always",
            [rd("s1", f"{W}/a.txt")],
            rw,
            flags=(),
            env={"DAISUGI_APPROVE": " Always "},
            before={"w/a.txt": {"text": "one\n"}},
        )
    )
    add(
        run_case_spec(
            "run approval env interactive",
            [rd("s1", f"{W}/a.txt")],
            rw,
            flags=(),
            env={"DAISUGI_APPROVE": "interactive"},
        )
    )
    add(
        run_case_spec(
            "run approval env bogus",
            [rd("s1", f"{W}/a.txt")],
            rw,
            flags=(),
            env={"DAISUGI_APPROVE": "sometimes"},
        )
    )
    add(
        run_case_spec(
            "run approval env bogus json",
            [rd("s1", f"{W}/a.txt")],
            rw,
            flags=("--json",),
            env={"DAISUGI_APPROVE": "sometimes"},
        )
    )
    add(
        run_case_spec(
            "run yes over env never",
            [rd("s1", f"{W}/a.txt")],
            rw,
            env={"DAISUGI_APPROVE": "never"},
            before={"w/a.txt": {"text": "one\n"}},
        )
    )
    # file_read.
    add(
        run_case_spec(
            "run read ok",
            [rd("s1", f"{W}/a.txt")],
            rw,
            before={"w/a.txt": {"text": "line one\nline two\n"}},
        )
    )
    add(run_case_spec("run read missing", [rd("s1", f"{W}/none.txt")], rw))
    add(
        run_case_spec(
            "run read a directory", [rd("s1", f"{W}/d")], rw, before={"w/d": {"dir": True}}
        )
    )
    add(
        run_case_spec(
            "run read not utf8",
            [rd("s1", f"{W}/b.bin")],
            rw,
            flags=("--yes", "--json"),
            before={"w/b.bin": {"hex": "61ff62c3"}},
        )
    )
    add(
        run_case_spec(
            "run read symlink escape",
            [rd("s1", f"{W}/link")],
            rw,
            before={"w/link": {"link": "{HOME}/secret.txt"}, "secret.txt": {"text": "s\n"}},
        )
    )
    # file_write.
    add(run_case_spec("run write new file", [wr("s1", f"{W}/out/new.txt", "hello\n")], rw))
    add(
        run_case_spec(
            "run write overwrite",
            [wr("s1", f"{W}/a.txt", "new\n")],
            rw,
            before={"w/a.txt": {"text": "old\n"}},
        )
    )
    add(
        run_case_spec(
            "run write prior not utf8",
            [wr("s1", f"{W}/a.bin", "text")],
            rw,
            before={"w/a.bin": {"hex": "ff00ff"}},
        )
    )
    add(
        run_case_spec(
            "run write symlink target",
            [wr("s1", f"{W}/link", "x")],
            rw,
            before={"w/link": {"link": "{HOME}/w/real.txt"}, "w/real.txt": {"text": "r\n"}},
        )
    )
    add(
        run_case_spec(
            "run write then read",
            [wr("s1", f"{W}/a.txt", "abc"), rd("s2", f"{W}/a.txt", ["s1"])],
            rw,
            flags=("--yes", "--json"),
        )
    )
    add(run_case_spec("run write non-ascii content", [wr("s1", f"{W}/u.txt", "é☃\n")], rw))
    # network: a refused connection and a scheme verify refuses.
    add(
        run_case_spec(
            "run network refused",
            [net("s1", "http://127.0.0.1:9/x")],
            env_doc(network=True, network_hosts=["127.0.0.1"]),
        )
    )
    add(
        run_case_spec(
            "run network host not allowed",
            [net("s1", "http://example.invalid/x")],
            env_doc(network=True, network_hosts=["127.0.0.1"]),
        )
    )
    # Steps no executor runs.
    add(
        run_case_spec(
            "run task step has no executor",
            [task("t1", "say hi")],
            env_doc(),
        )
    )
    add(
        run_case_spec(
            "run agentic step has no executor",
            [
                {
                    "id": "g1",
                    "type": "agentic",
                    "prompt": "fix it",
                    "workspace": W,
                    "tools": ["Read"],
                    "depends_on": [],
                }
            ],
            env_doc(file_read=[f"{W}/**"]),
        )
    )
    add(
        run_case_spec(
            "run llm_check invariant",
            [sh("s1", "echo hi")],
            {
                **echo,
                "invariants": [
                    {
                        "type": "judge",
                        "description": "d",
                        "expr": {"op": "llm_check", "rule": "is it kind"},
                    }
                ],
            },
            go_refuses=True,
        )
    )
    # Stage 2 and step postconditions.
    add(
        run_case_spec(
            "run stage2 exit code violated",
            [sh("s1", "echo hi")],
            env_doc(
                shell=True,
                shell_allowlist=["echo"],
                post=[{"type": "exit_code", "expected": 1}],
            ),
        )
    )
    add(
        run_case_spec(
            "run stage2 exit code holds",
            [sh("s1", "echo hi")],
            env_doc(
                shell=True,
                shell_allowlist=["echo"],
                post=[{"type": "exit_code", "expected": 0}],
            ),
        )
    )
    add(
        run_case_spec(
            "run stage2 file exists",
            [wr("s1", f"{W}/made.txt", "m")],
            env_doc(
                file_write=[f"{W}/**"],
                post=[{"type": "file_exists", "path": f"{W}/made.txt"}],
            ),
        )
    )
    add(
        run_case_spec(
            "run stage2 file missing",
            [wr("s1", f"{W}/made.txt", "m")],
            env_doc(
                file_write=[f"{W}/**"],
                post=[{"type": "file_exists", "path": f"{W}/other.txt"}],
            ),
            flags=("--yes", "--json"),
        )
    )
    add(
        run_case_spec(
            "run stage2 size range",
            [wr("s1", f"{W}/made.txt", "12345")],
            env_doc(
                file_write=[f"{W}/**"],
                post=[{"type": "file_size_range", "path": f"{W}/made.txt", "min": 1, "max": 3}],
            ),
        )
    )
    add(
        run_case_spec(
            "run stage2 not enforced",
            [sh("s1", "echo hi")],
            env_doc(
                shell=True,
                shell_allowlist=["echo"],
                post=[{"type": "exit_code", "expected": 3, "enforce": False}],
            ),
        )
    )
    add(
        run_case_spec(
            "run step postcondition path present",
            [sh("s1", "echo hi", postcondition={"type": "evidence", "path": "stdout"})],
            echo,
        )
    )
    add(
        run_case_spec(
            "run step postcondition path absent",
            [sh("s1", "echo hi", postcondition={"type": "evidence", "path": "nope"})],
            echo,
        )
    )
    add(
        run_case_spec(
            "run step postcondition no path",
            [sh("s1", "echo hi", postcondition={"type": "evidence"})],
            echo,
        )
    )
    # Dry run.
    add(
        run_case_spec(
            "run dry run",
            [sh("s1", "echo hi"), rd("s2", f"{W}/a.txt"), wr("s3", f"{W}/b.txt", "héllo")],
            env_doc(
                shell=True,
                shell_allowlist=["echo"],
                file_read=[f"{W}/**"],
                file_write=[f"{W}/**"],
            ),
            flags=("--dry-run", "--yes"),
        )
    )
    add(
        run_case_spec(
            "run dry run network",
            [net("s1", "http://127.0.0.1:9/x")],
            env_doc(network=True, network_hosts=["127.0.0.1"]),
            flags=("--dry-run", "--yes", "--json"),
        )
    )
    add(
        run_case_spec(
            "run dry run task", [task("t1", "hi")], env_doc(), flags=("--dry-run", "--yes")
        )
    )
    # Data dir.
    add(
        run_case_spec(
            "run data dir", [sh("s1", "echo hi")], echo, flags=("--yes", "--data-dir", "dd")
        )
    )
    # Reading the files.
    bad = {
        "p.yaml": yml(plan_doc([sh("s1", "echo hi")])),
        "w": {"dir": True},
    }
    add(
        {
            "kind": "cli",
            "name": "run envelope not yaml",
            "argv": ["run", "p.yaml", "-e", "e.yaml"],
            "go_refuses": True,
            "before": {**bad, "e.yaml": {"text": "a: [\n"}},
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run envelope invalid",
            "argv": ["run", "p.yaml", "-e", "e.yaml"],
            "before": {**bad, "e.yaml": {"text": "generated_by: x\n"}},
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run envelope nan",
            "argv": ["run", "p.yaml", "-e", "e.yaml", "--yes"],
            "before": {
                **bad,
                "e.yaml": {
                    "text": "generated_by: x\ntask: t\npermissions:\n  shell: true\n"
                    "  shell_allowlist:\n  - echo\n  max_output_size_mb: .nan\n"
                },
            },
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run plan not yaml",
            "argv": ["run", "p.yaml", "-e", "e.yaml"],
            "go_refuses": True,
            "before": {"p.yaml": {"text": "steps: {\n"}, "e.yaml": yml(echo)},
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run plan invalid step",
            "argv": ["run", "p.yaml", "-e", "e.yaml"],
            "before": {
                "p.yaml": yml(plan_doc([{"id": "s1", "type": "shell"}])),
                "e.yaml": yml(echo),
            },
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run plan missing",
            "argv": ["run", "nope.yaml", "-e", "e.yaml"],
            "before": {"e.yaml": yml(echo)},
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run envelope missing",
            "argv": ["run", "p.yaml", "-e", "nope.yaml"],
            "before": {"p.yaml": yml(plan_doc([sh("s1", "echo hi")]))},
        }
    )
    add(
        {
            "kind": "cli",
            "name": "run no envelope option",
            "argv": ["run", "p.yaml"],
            "before": {"p.yaml": yml(plan_doc([]))},
        }
    )
    add(run_case_spec("run empty plan", [], echo, flags=("--yes", "--json")))
    return C


# ---------------------------------------------------------------------------
# `daisugi orchestrate`
# ---------------------------------------------------------------------------


def decomposed(*steps: dict[str, Any]) -> str:
    return json.dumps({"steps": list(steps)})


def answer(text: str) -> str:
    return json.dumps({"answer": text})


def orch(
    name: str,
    prompt: str,
    *,
    envelope: dict[str, Any] | None = None,
    flags=(),
    env=None,
    replies=None,
    before=None,
    **kw,
) -> dict[str, Any]:
    tree: dict[str, Any] = dict(LEX)
    argv = ["orchestrate", prompt]
    if envelope is not None:
        tree["e.yaml"] = yml(envelope)
        argv += ["--envelope", "e.yaml"]
    tree.update(before or {})
    c: dict[str, Any] = {
        "kind": "cli",
        "name": name,
        "argv": argv + list(flags),
        "before": tree,
        "env": dict(API if env is None else env),
    }
    if replies:
        c["replies"] = replies
    c.update(kw)
    return c


def build_orchestrate_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    P = "List three risks of the plan"
    none = env_doc()
    two = decomposed(
        {"id": "a", "type": "task", "prompt": "Name one risk"},
        {"id": "b", "type": "task", "prompt": "Name another risk", "depends_on": ["a"]},
    )
    # Decomposition, task steps and synthesis on the HTTP backend.
    add(
        orch(
            "orch api two tasks",
            P,
            envelope=none,
            replies=[
                {"http": two},
                {"http": "risk one"},
                {"http": "risk two"},
                {"http": answer("Risk one; risk two.")},
            ],
        )
    )
    add(
        orch(
            "orch api json",
            P,
            envelope=none,
            flags=("--json",),
            replies=[
                {"http": two},
                {"http": "risk one"},
                {"http": "risk two"},
                {"http": answer("Risk one; risk two.")},
            ],
        )
    )
    add(
        orch(
            "orch api cost",
            P,
            envelope=none,
            flags=("--cost",),
            replies=[
                {"http": two},
                {"http": "risk one"},
                {"http": "risk two"},
                {"http": answer("Both.")},
            ],
        )
    )
    add(
        orch(
            "orch api deterministic synthesis",
            P,
            envelope=none,
            flags=("--deterministic-synthesis",),
            replies=[{"http": two}, {"http": "risk one\n"}, {"http": "  "}],
        )
    )
    add(
        orch(
            "orch api synthesis empty answer",
            P,
            envelope=none,
            replies=[{"http": two}, {"http": "r1"}, {"http": "r2"}, {"http": answer("  ")}],
        )
    )
    add(
        orch(
            "orch api synthesis fails",
            P,
            envelope=none,
            replies=[
                {"http": two},
                {"http": "r1"},
                {"http": "r2"},
                {"http_status": 500, "body": "{}"},
            ],
        )
    )
    add(
        orch(
            "orch api synthesis reask",
            P,
            envelope=none,
            flags=("--json",),
            replies=[
                {"http": two},
                {"http": "r1"},
                {"http": "r2"},
                {"http": "no"},
                {"http": answer("ok")},
            ],
        )
    )
    # A hard subtask sizes to the frontier; a budget downgrades it.
    hard = decomposed(
        {"id": "h", "type": "task", "prompt": "Design the security architecture and prove it"}
    )
    add(
        orch(
            "orch sizing frontier",
            P,
            envelope=none,
            flags=("--json",),
            replies=[{"http": hard}, {"http": "done"}, {"http": answer("A.")}],
        )
    )
    add(
        orch(
            "orch budget downgrades",
            P,
            envelope=none,
            flags=("--budget", "3000"),
            replies=[{"http": hard}, {"http": "done"}, {"http": answer("A.")}],
        )
    )
    add(
        orch(
            "orch budget unaffordable not strict",
            P,
            envelope=none,
            flags=("--budget", "1000", "--json"),
            replies=[{"http": two}, {"http": "r1"}, {"http": "r2"}, {"http": answer("A.")}],
        )
    )
    add(
        orch(
            "orch budget strict stops",
            P,
            envelope=none,
            flags=("--budget", "1000", "--strict-budget"),
            replies=[{"http": two}],
        )
    )
    add(
        orch(
            "orch budget strict crossed",
            P,
            envelope=none,
            flags=("--budget", "2010", "--strict-budget", "--json"),
            replies=[{"http": two}, {"http": "r1"}, {"http": "r2"}],
        )
    )
    add(
        orch(
            "orch budget spent synth deterministic",
            P,
            envelope=none,
            flags=("--budget", "2019"),
            replies=[{"http": two}, {"http": "r1"}],
        )
    )
    add(
        orch(
            "orch fan-in raises difficulty",
            P,
            envelope=none,
            flags=("--json", "--deterministic-synthesis"),
            replies=[
                {
                    "http": decomposed(
                        {"id": "a", "type": "task", "prompt": "a"},
                        {"id": "b", "type": "task", "prompt": "b"},
                        {"id": "c", "type": "task", "prompt": "c"},
                        {"id": "d", "type": "task", "prompt": "d", "depends_on": ["a", "b", "c"]},
                    )
                },
                {"http": "1"},
                {"http": "2"},
                {"http": "3"},
                {"http": "4"},
            ],
        )
    )
    # A failed task step.
    add(
        orch(
            "orch task call fails",
            P,
            envelope=none,
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http_status": 500, "body": '{"error": "down"}'},
                {"http": answer("partial")},
            ],
        )
    )
    add(
        orch(
            "orch task call fails json",
            P,
            envelope=none,
            flags=("--json",),
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http_status": 500, "body": '{"error": "down"}'},
                {"http": "second try"},
                {"http": answer("ok")},
            ],
        )
    )
    # Decomposition failures.
    add(orch("orch no steps", P, envelope=none, replies=[{"http": decomposed()}]))
    add(
        orch(
            "orch no steps json",
            P,
            envelope=none,
            flags=("--json",),
            replies=[{"http": decomposed()}],
        )
    )
    add(
        orch(
            "orch decomposition cycle",
            P,
            envelope=none,
            replies=[
                {
                    "http": decomposed(
                        {"id": "a", "type": "task", "prompt": "a", "depends_on": ["b"]},
                        {"id": "b", "type": "task", "prompt": "b", "depends_on": ["a"]},
                    )
                }
            ],
        )
    )
    add(
        orch(
            "orch decomposition out of policy",
            P,
            envelope=none,
            replies=[{"http": decomposed({"id": "a", "type": "shell", "command": "rm -rf /"})}],
        )
    )
    add(
        orch(
            "orch decomposition missing field",
            P,
            envelope=none,
            replies=[{"http": decomposed({"id": "a", "type": "shell"})}],
        )
    )
    add(
        orch(
            "orch decomposition bad type",
            P,
            envelope=none,
            replies=[
                {"http": decomposed({"id": "a", "type": "email"})},
                {"http": decomposed({"id": "a", "type": "email"})},
                {"http": decomposed({"id": "a", "type": "email"})},
            ],
        )
    )
    add(
        orch(
            "orch decomposition call fails",
            P,
            envelope=none,
            replies=[{"http_status": 500, "body": "{}"}],
        )
    )
    add(
        orch(
            "orch decomposition nan",
            P,
            envelope=none,
            replies=[
                {
                    "http": '{"steps": [{"id": "a", "type": "task", "prompt": "a", "skill_input": {"x": NaN}}]}'
                },
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
                {"http": answer("ok")},
            ],
        )
    )
    # Deterministic steps the model chose, run under the envelope.
    shenv = env_doc(shell=True, shell_allowlist=["echo"])
    add(
        orch(
            "orch shell steps",
            P,
            envelope=shenv,
            flags=("--deterministic-synthesis",),
            replies=[
                {
                    "http": decomposed(
                        {"id": "s", "type": "shell", "command": "echo risk"},
                        {"id": "t", "type": "shell", "command": "echo two", "depends_on": ["s"]},
                    )
                }
            ],
        )
    )
    add(
        orch(
            "orch shell step fails",
            P,
            envelope=env_doc(shell=True, shell_allowlist=["false"]),
            replies=[
                {"http": decomposed({"id": "s", "type": "shell", "command": "false"})},
                {"http": answer("nothing")},
            ],
        )
    )
    add(
        orch(
            "orch skill step no handler",
            P,
            envelope=none,
            replies=[
                {"http": decomposed({"id": "k", "type": "skill", "skill_id": "sum"})},
                {"http": answer("none")},
            ],
        )
    )
    add(
        orch(
            "orch mcp step no transport",
            P,
            envelope=env_doc(mcp_allowlist=["gh/*"]),
            replies=[
                {"http": decomposed({"id": "m", "type": "mcp", "server": "gh", "tool": "list"})},
                {"http": answer("none")},
            ],
        )
    )
    # The claude-code backend.
    add(
        orch(
            "orch claude-code two tasks",
            P,
            envelope=none,
            env=CC,
            flags=("--json", "--cost"),
            replies=[
                {"claude": two},
                {"claude": "risk one"},
                {"claude": "risk two"},
                {"claude": answer("Both risks.")},
            ],
        )
    )
    add(
        orch(
            "orch claude-code text",
            P,
            envelope=none,
            env=CC,
            flags=("--cost",),
            replies=[
                {"claude": two},
                {"claude": "risk one"},
                {"claude": "risk two"},
                {"claude": answer("Both risks.")},
            ],
        )
    )
    add(
        orch(
            "orch claude-code task is_error",
            P,
            envelope=none,
            env=CC,
            replies=[
                {"claude": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"claude_is_error": "overloaded"},
                {"claude": answer("x")},
            ],
        )
    )
    add(
        orch(
            "orch claude-code task not json",
            P,
            envelope=none,
            env=CC,
            flags=("--json",),
            replies=[
                {"claude": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"claude_raw": "plain words"},
                {"claude": answer("x")},
            ],
        )
    )
    add(
        orch(
            "orch claude-code task exits",
            P,
            envelope=none,
            env=CC,
            replies=[
                {"claude": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"claude_raw": "", "stderr": "boom", "exit": 3},
                {"claude": answer("x")},
            ],
        )
    )
    add(
        orch(
            "orch claude args forwarded",
            P,
            envelope=none,
            env={**CC, "DAISUGI_CLAUDE_ARGS": "--max-turns 2"},
            flags=("--deterministic-synthesis",),
            replies=[
                {"claude": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"claude": "r"},
            ],
        )
    )
    # No envelope given: one is generated first.
    add(
        orch(
            "orch generates envelope",
            P,
            flags=("--deterministic-synthesis",),
            replies=[
                {
                    "http": json.dumps(
                        {"generated_by": "m", "task": P, "permissions": perm(), "invariants": []}
                    )
                },
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
        )
    )
    add(
        orch(
            "orch generated envelope fails",
            P,
            replies=[{"http_status": 500, "body": "{}"}],
        )
    )
    add(
        orch(
            "orch stakes low no default",
            P,
            flags=("--stakes", "low"),
        )
    )
    # Tier-0 reuse of a distilled pathway: decomposition skipped.
    T = "Run the tests and read the output"
    from opendaisugi.models import ActionPlan, ShellStep

    pw_plan = ActionPlan(
        id="plan_00000007",
        source="distiller",
        task=T,
        steps=[ShellStep(id="s1", command="echo reused")],
    )
    pw = pathway(7, T, lexical(T), pl=pw_plan)
    store = {".opendaisugi/pathways.db": {"db": {"rows": [row_spec(put_row(pw))]}}}
    add(
        orch(
            "orch tier0 reuse no model",
            T,
            envelope=env_doc(shell=True, shell_allowlist=["echo"]),
            flags=("--deterministic-synthesis", "--json"),
            before=store,
        )
    )
    add(
        orch(
            "orch tier0 reuse refused by envelope",
            T,
            envelope=none,
            flags=("--deterministic-synthesis",),
            before=store,
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
        )
    )
    stale = pathway(8, T, lexical(T), model="other-model", emb_version="1", pl=pw_plan)
    add(
        orch(
            "orch stale embeddings warn",
            T,
            envelope=env_doc(shell=True, shell_allowlist=["echo"]),
            flags=("--deterministic-synthesis",),
            before={
                ".opendaisugi/pathways.db": {
                    "db": {"rows": [row_spec(put_row(pw)), row_spec(put_row(stale))]}
                }
            },
            # The warning's text names the oracle's source file; the case
            # checks only that the binary refuses (K2-5).
            env={**API, "PYTHONWARNINGS": "ignore::UserWarning"},
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
            go_refuses=True,
        )
    )
    add(
        orch(
            "orch llm_check postcondition",
            P,
            envelope=env_doc(
                post=[{"type": "judge", "expr": {"op": "llm_check", "rule": "is it kind"}}]
            ),
            flags=("--deterministic-synthesis",),
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
            go_refuses=True,
        )
    )
    # Refusals and options.
    add(orch("orch bad stakes", P, envelope=none, flags=("--stakes", "huge")))
    add(orch("orch bad llm", P, envelope=none, flags=("--llm", "gpt")))
    add(orch("orch llm renamed", P, envelope=none, flags=("--llm", "litellm")))
    add(orch("orch envelope missing", P, flags=("--envelope", "nope.yaml")))
    add(
        orch(
            "orch envelope not yaml",
            P,
            flags=("--envelope", "bad.yaml"),
            go_refuses=True,
            before={"bad.yaml": {"text": "x: [\n"}},
        )
    )
    add(orch("orch no key", P, envelope=none, env={"OPENDAISUGI_LLM_BACKEND": "api"}))
    add(
        orch(
            "orch no claude",
            P,
            envelope=none,
            env=CC,
            no_claude=True,
        )
    )
    add(
        orch(
            "orch max parallel",
            P,
            envelope=none,
            flags=("--max-parallel", "2", "--deterministic-synthesis"),
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
            go_refuses=True,
        )
    )
    add(
        orch(
            "orch model flag",
            P,
            envelope=none,
            flags=("--model", "anthropic/claude-haiku-4-5", "--deterministic-synthesis"),
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
        )
    )
    add(
        orch(
            "orch data dir",
            P,
            envelope=none,
            flags=("--data-dir", "dd", "--deterministic-synthesis"),
            replies=[
                {"http": decomposed({"id": "a", "type": "task", "prompt": "a"})},
                {"http": "r"},
            ],
        )
    )
    return C


def all_cases() -> list[dict[str, Any]]:
    cases = build_run_cases() + build_orchestrate_cases()
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
        c["expect"] = run_case(c, cmd_for(c, None), work)
        if any(r.get("key_ok") is False for r in c["expect"]["requests"]):
            raise SystemExit(f"{c['name']}: the oracle sent a credential that is not the case's")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:8000]
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

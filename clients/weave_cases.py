"""Synthetic weave cases: `daisugi weave` (a plan tree on the supervisor, typed
slots between steps, the router for task steps, resume), run through the
Python oracle.

    uv run --no-sync python clients/weave_cases.py [--out clients/fixtures/weave] [--only NAME]

A case runs as a stage-K2 case does (clients/k2_cases.py): a scratch HOME,
the fake `claude` and the fake model server answering only exact request
bytes, and the exit code, stdout, stderr, the tree after and the model
requests recorded. Two things are this file's own:

- ``pre``: commands run with the same binary before the case's command,
  so a resume case finds the receipts an earlier run of the same binary
  left;
- a ``weave_marks`` entry in the tree: a state file with start marks for
  the laid-out plan file, named by that file's hash, for a run cut short
  between a step's start and its receipt.

A plan hash (``sha256:<hex>`` or ``sha256-<hex>``), a weave ranking id and
a choice id are minted like a run id.
Every path, key and task is synthetic.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script
import k2_cases  # noqa: E402, F401 - patches garden_cases (normalize, lay_out)
from garden_cases import PY_CLI, body_id, record_replies, run_case, write_jsonl  # noqa: E402
from k2_cases import API, CC, W, env_doc, rd, sh, task, wr, yml  # noqa: E402
from pathway_cases import REPO  # noqa: E402

FIXTURE_DIR = REPO / "clients" / "fixtures" / "weave"
SCRATCH = Path(
    os.environ.get("DAISUGI_WEAVE_SCRATCH")
    or Path.home() / "opendaisugi-scratch" / "weave" / "runs"
)
CASE_VERSION = 1

garden_cases._MINTED.append((re.compile(r"\bsha256[:-][0-9a-f]{64}\b"), "planhash"))
# A ranking id and a choice id hold the plan hash, which holds {HOME} when
# the plan names a path under it.
garden_cases._MINTED.append((re.compile(r"\bweave:[0-9a-f]{16}:"), "wrank"))
garden_cases._MINTED.append((re.compile(r"\bch_[0-9a-f]{12}\b"), "choice"))

# ---------------------------------------------------------------------------
# Earlier runs and start marks
# ---------------------------------------------------------------------------

_generic_invoke = garden_cases.invoke


def invoke(case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, work: Path):
    """garden_cases.invoke, after each of the case's ``pre`` commands."""
    cmd = argv[: len(argv) - len(case["argv"])]
    for pre in case.get("pre") or []:
        _generic_invoke(case, cmd + [a.replace("{HOME}", env["HOME"]) for a in pre], env, cwd, work)
    return _generic_invoke(case, argv, env, cwd, work)


garden_cases.invoke = invoke
_k2_lay_out = garden_cases.lay_out


def lay_out(tree: dict[str, Any], home: Path, t0: float) -> None:
    """k2_cases.lay_out, and each ``weave_marks`` entry: a state file for
    the laid-out plan file, one start mark per (run, step)."""
    marks = {k: v for k, v in tree.items() if "weave_marks" in v}
    _k2_lay_out({k: v for k, v in tree.items() if k not in marks}, home, t0)
    for rel, spec in sorted(marks.items()):
        digest = hashlib.sha256((home / spec["plan"]).read_bytes()).hexdigest()
        p = home / rel / f"sha256-{digest}.jsonl"
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(
            "".join(json.dumps({"run": r, "step": s}) + "\n" for r, s in spec["weave_marks"]),
            encoding="utf-8",
        )


garden_cases.lay_out = lay_out


def cmd_for(case: dict[str, Any], binary: str | None) -> list[str]:
    return PY_CLI if binary is None else [binary]


# ---------------------------------------------------------------------------
# Building blocks
# ---------------------------------------------------------------------------


def plan_json(steps: list[dict[str, Any]], i: int = 1) -> dict[str, str]:
    doc = {"id": f"plan_{i:08x}", "source": "script", "task": f"task {i}", "steps": steps}
    return {"text": json.dumps(doc, indent=1) + "\n"}


def wv(
    name: str,
    steps: list[dict[str, Any]] | None,
    envelope: dict[str, Any],
    *,
    flags=("--yes",),
    before=None,
    env=None,
    pre=None,
    plan=None,
    **kw,
) -> dict[str, Any]:
    tree: dict[str, Any] = {
        "p.json": plan if plan is not None else plan_json(steps or []),
        "e.yaml": yml(envelope),
        "w": {"dir": True},
    }
    tree.update(before or {})
    argv = ["weave", "p.json", "-e", "e.yaml", *flags]
    c: dict[str, Any] = {"kind": "cli", "name": f"weave {name}", "argv": argv, "before": tree}
    if env:
        c["env"] = env
    if pre:
        c["pre"] = [["weave", "p.json", "-e", "e.yaml", *p] for p in pre]
    c.update(kw)
    return c


def out(step: dict[str, Any], **outputs: str) -> dict[str, Any]:
    return {**step, "outputs": outputs}


def fill(step: dict[str, Any], **inputs: str) -> dict[str, Any]:
    return {**step, "inputs": inputs}


def printf_json(obj: Any) -> str:
    """A shell command that prints obj as JSON (no single quote inside)."""
    text = json.dumps(obj)
    assert "'" not in text
    return f"printf '%s' '{text}'"


# ---------------------------------------------------------------------------
# The cases
# ---------------------------------------------------------------------------


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []
    add = C.append
    shell_rw = env_doc(
        shell=True,
        shell_allowlist=["printf", "false", "true", "cat"],
        shell_allow_decomposition=True,
        file_read=[f"{W}/**"],
        file_write=[f"{W}/**"],
    )
    to = f"{W}/out/a.txt"
    place = f"{W}/out/placeholder.txt"

    # Slots between deterministic steps.
    a_path = out(sh("a", printf_json({"to": to, "body": "hello"})), to="path", body="string")
    add(
        wv(
            "slot path and content",
            [a_path, fill(wr("b", place, "", ["a"]), path="a.to", content="a.body")],
            shell_rw,
            before={"w/out": {"dir": True}},
        )
    )
    add(
        wv(
            "slot path and content json",
            [a_path, fill(wr("b", place, "", ["a"]), path="a.to", content="a.body")],
            shell_rw,
            flags=("--yes", "--json"),
            before={"w/out": {"dir": True}},
        )
    )
    add(
        wv(
            "slot read path",
            [
                out(sh("a", printf_json({"f": f"{W}/in/x.txt"})), f="path"),
                fill(rd("b", f"{W}/in/other.txt", ["a"]), path="a.f"),
            ],
            shell_rw,
            before={"w/in/x.txt": {"text": "from x\n"}},
        )
    )
    for label, obj, typ in (
        ("number int", {"v": 3}, "number"),
        ("number float", {"v": 1.5}, "number"),
        ("number big", {"v": 2**53}, "number"),
        ("list of paths", {"v": ["/data/a", "/data/b"]}, "list[path]"),
        ("list of numbers", {"v": [1, 2.25, -3]}, "list[number]"),
        ("list empty", {"v": []}, "list[string]"),
        ("string unicode", {"v": "café ☃"}, "string"),
    ):
        add(
            wv(
                f"slot content {label}",
                [
                    out(sh("a", printf_json(obj)), v=typ),
                    fill(wr("b", to, "", ["a"]), content="a.v"),
                ],
                shell_rw,
                flags=("--yes", "--json"),
            )
        )
    add(
        wv(
            "slot extra keys ignored",
            [
                out(sh("a", printf_json({"v": "x", "other": "y"})), v="string"),
                fill(wr("b", to, "", ["a"]), content="a.v"),
            ],
            shell_rw,
            flags=("--yes", "--json"),
        )
    )
    add(
        wv(
            "slot through a middle step",
            [
                out(sh("a", printf_json({"v": "x"})), v="string"),
                sh("m", "true", ["a"]),
                fill(wr("b", to, "", ["m"]), content="a.v"),
            ],
            shell_rw,
        )
    )
    # Slot values that do not read.
    for label, text, typ in (
        ("not json", "printf 'plain words'", "string"),
        ("json array", printf_json([1]), "string"),
        ("missing slot", printf_json({"w": "x"}), "string"),
        ("path relative", printf_json({"v": "out/a.txt"}), "path"),
        ("path dotdot", printf_json({"v": f"{W}/../x"}), "path"),
        ("path control", printf_json({"v": f"{W}/a\tb"}), "path"),
        ("number bool", printf_json({"v": True}), "number"),
        ("number too big", printf_json({"v": 2**53 + 1}), "number"),
        ("number nan", "printf '{\"v\": NaN}'", "number"),
        ("string nul", printf_json({"v": "a\u0000b"}), "string"),
        ("string long", printf_json({"v": "x" * 4097}), "string"),
        ("list too long", printf_json({"v": ["x"] * 65}), "list[string]"),
        ("list wrong item", printf_json({"v": ["x", 1]}), "list[string]"),
    ):
        add(
            wv(
                f"slot bad {label}",
                [out(sh("a", text), v=typ), fill(wr("b", to, "", ["a"]), content="a.v")],
                shell_rw,
                flags=("--yes", "--json"),
            )
        )
    add(
        wv(
            "slot bad not json text",
            [
                out(sh("a", "printf 'plain words'"), v="string"),
                fill(wr("b", to, "", ["a"]), content="a.v"),
            ],
            shell_rw,
        )
    )
    # A slot keeps the directory; the filled step is verified again.
    moved = out(sh("a", printf_json({"to": f"{W}/elsewhere/a.txt"})), to="path")
    add(
        wv(
            "slot moves the directory",
            [moved, fill(wr("b", place, "", ["a"]), path="a.to")],
            shell_rw,
        )
    )
    add(
        wv(
            "slot moves the directory json",
            [moved, fill(wr("b", place, "", ["a"]), path="a.to")],
            shell_rw,
            flags=("--yes", "--json"),
        )
    )
    narrow = env_doc(
        shell=True,
        shell_allowlist=["printf"],
        shell_allow_decomposition=True,
        file_write=[place],
        fallback={"strategy": "halt"},
    )
    other = out(sh("a", printf_json({"to": f"{W}/out/other.txt"})), to="path")
    add(
        wv(
            "slot verified again rejects",
            [other, fill(wr("b", place, "", ["a"]), path="a.to")],
            narrow,
        )
    )
    add(
        wv(
            "slot verified again rejects json",
            [other, fill(wr("b", place, "", ["a"]), path="a.to")],
            narrow,
            flags=("--yes", "--json"),
        )
    )
    # The envelope's default fallback asks a model for a replacement step.
    # The paths are outside the scratch HOME, so the request is the same on
    # every run; no step there runs.
    far = "/nonexistent/out"
    recompute = env_doc(
        shell=True,
        shell_allowlist=["printf"],
        shell_allow_decomposition=True,
        file_write=[f"{far}/placeholder.txt"],
    )
    far_other = out(sh("a", printf_json({"to": f"{far}/other.txt"})), to="path")
    for label, reply in (
        ("not json", {"claude_raw": "no"}),
        (
            "a step outside",
            {"claude": json.dumps({**wr("b", f"{far}/third.txt", ""), "depends_on": []})},
        ),
    ):
        add(
            wv(
                f"slot verified again recompute {label}",
                [far_other, fill(wr("b", f"{far}/placeholder.txt", "", ["a"]), path="a.to")],
                recompute,
                flags=("--yes", "--json"),
                replies=[reply],
            )
        )
    # The per-step verify skips plan-level invariants, so the filled plan is
    # verified again whole.
    add(
        wv(
            "slot filled plan breaks an invariant",
            [
                out(sh("a", printf_json({"v": "a secret"})), v="string"),
                fill(wr("b", to, "", ["a"]), content="a.v"),
            ],
            {
                **shell_rw,
                "invariants": [
                    {
                        "type": "no_secret",
                        "description": "no secret in a write",
                        "expr": {
                            "op": "forall_steps",
                            "pred": {"op": "not_matches", "path": "content", "regex": "secret"},
                        },
                    }
                ],
            },
            flags=("--yes", "--json"),
        )
    )
    # A URL keeps its scheme and host; the run stops before any request.
    net_env = env_doc(
        shell=True,
        shell_allowlist=["printf"],
        shell_allow_decomposition=True,
        network=True,
        network_hosts=["127.0.0.1"],
    )
    for label, value in (
        ("moves the host", "http://evil.invalid/x"),
        ("userinfo", "http://127.0.0.1:9@evil.invalid/x"),
        ("scheme", "https://127.0.0.1:9/x"),
    ):
        add(
            wv(
                f"slot url {label}",
                [
                    out(sh("a", printf_json({"u": value})), u="string"),
                    fill(
                        {
                            "id": "b",
                            "type": "network",
                            "url": "http://127.0.0.1:9/x",
                            "depends_on": ["a"],
                        },
                        url="a.u",
                    ),
                ],
                net_env,
            )
        )
    # The plan's slots are checked before anything runs.
    s_a = sh("a", "true")
    for label, steps in (
        ("outputs on a write", [out(wr("a", to, ""), v="string")]),
        ("outputs not an object", [{**s_a, "outputs": ["v"]}]),
        ("outputs bad name", [out(s_a, V="string")]),
        ("outputs bad type", [out(s_a, v="object")]),
        ("inputs not an object", [out(s_a, v="string"), {**sh("b", "true", ["a"]), "inputs": 1}]),
        (
            "input into a command",
            [out(s_a, v="string"), fill(sh("b", "true", ["a"]), command="a.v")],
        ),
        ("input into a prompt", [out(s_a, v="string"), fill(task("b", "x", ["a"]), prompt="a.v")]),
        ("input not a dependency", [out(s_a, v="path"), fill(rd("b", to), path="a.v")]),
        ("input no step", [fill(rd("b", to), path="zz.v")]),
        ("input no dot", [out(s_a, v="path"), fill(rd("b", to, ["a"]), path="av")]),
        ("input not a string", [out(s_a, v="path"), {**rd("b", to, ["a"]), "inputs": {"path": 1}}]),
        ("input no slot", [out(s_a, v="path"), fill(rd("b", to, ["a"]), path="a.w")]),
        ("input wrong type", [out(s_a, v="number"), fill(rd("b", to, ["a"]), path="a.v")]),
        (
            "input path no directory",
            [out(s_a, v="path"), fill(rd("b", "x.txt", ["a"]), path="a.v")],
        ),
        (
            "input url no host",
            [
                out(s_a, v="string"),
                fill(
                    {"id": "b", "type": "network", "url": "file:///x", "depends_on": ["a"]},
                    url="a.v",
                ),
            ],
        ),
    ):
        add(wv(f"plan {label}", steps, shell_rw))
    add(wv("plan not json", None, shell_rw, plan={"text": "{"}))
    add(wv("plan json list", None, shell_rw, plan={"text": "[]"}))
    add(wv("plan no steps", None, shell_rw, plan={"text": '{"source": "s", "task": "t"}'}))
    add(wv("plan verify rejects", [sh("a", "rm -rf /")], shell_rw))
    add(wv("plan verify rejects json", [sh("a", "rm -rf /")], shell_rw, flags=("--yes", "--json")))
    add(wv("max parallel zero", [s_a], shell_rw, flags=("--yes", "--max-parallel", "0")))
    add(wv("no yes approval denied", [rd("a", f"{W}/x")], shell_rw, flags=()))

    # Task steps: the router's model, the slot prompt, the reply. A task
    # step's output is model text, so its slot never fills a file's content
    # (WV-R-2); it fills a path under the directory rule. /dev/null is the
    # one path whose read gives the same bytes on every machine.
    t_out = out(task("t", "name the file"), f="path")
    dev = env_doc(
        shell=True,
        shell_allowlist=["printf", "false", "true", "cat"],
        shell_allow_decomposition=True,
        file_read=["/dev/*"],
        file_write=[f"{W}/**"],
    )
    t_read = fill(rd("b", "/dev/placeholder", ["t"]), path="t.f")
    null = '{"f": "/dev/null"}'
    for label, typ in (("number", "number"), ("string", "string"), ("path", "path")):
        add(
            wv(
                f"task slot {label} into content refused",
                [
                    out(task("t", "count the files"), n=typ),
                    fill(wr("b", to, "", ["t"]), content="t.n"),
                ],
                shell_rw,
                env=API,
            )
        )
    add(
        wv(
            "task slot into content refused json",
            [
                out(task("t", "count the files"), n="number"),
                fill(wr("b", to, "", ["t"]), content="t.n"),
            ],
            shell_rw,
            flags=("--yes", "--json"),
            env=API,
        )
    )
    add(wv("task slot", [t_out, t_read], dev, env=API, replies=[{"http": null}]))
    add(
        wv(
            "task slot json",
            [t_out, t_read],
            dev,
            flags=("--yes", "--json"),
            env=API,
            replies=[{"http": null}],
        )
    )
    add(
        wv(
            "task slot fenced",
            [t_out, t_read],
            dev,
            env=API,
            replies=[{"http": f"```json\n{null}\n```"}],
        )
    )
    add(
        wv(
            "task slot prose",
            [t_out, t_read],
            dev,
            env=API,
            replies=[{"http": "The null device."}],
        )
    )
    add(
        wv(
            "task slot moves the directory",
            [t_out, t_read],
            dev,
            env=API,
            replies=[{"http": '{"f": "/etc/passwd"}'}],
        )
    )
    add(
        wv(
            "task plain",
            [task("t", "name one risk")],
            shell_rw,
            flags=("--yes", "--json"),
            env=API,
            replies=[{"http": "A risk."}],
        )
    )
    hard = (
        "design the architecture for a distributed consensus protocol and analyze "
        "its security and concurrency failure modes"
    )
    add(
        wv(
            "task routed frontier",
            [task("t", hard)],
            shell_rw,
            flags=("--yes", "--json"),
            env=API,
            replies=[{"http": "A design."}],
        )
    )
    add(
        wv(
            "task preferred model kept",
            [{**task("t", "name one risk"), "preferred_model": "claude-sonnet-4-5"}],
            shell_rw,
            flags=("--yes", "--json"),
            env=API,
            replies=[{"http": "A risk."}],
        )
    )
    add(
        wv(
            "task claude-code slot",
            [t_out, t_read],
            dev,
            env=CC,
            replies=[{"claude": null}],
        )
    )
    add(
        wv(
            "task model error",
            [t_out],
            shell_rw,
            env=API,
            replies=[{"http_status": 500, "body": "{}"}],
        )
    )

    # Agentic steps (WV-4) and parallel levels (K2-4): the ports refuse both.
    agentic = {
        "id": "g",
        "type": "agentic",
        "prompt": "fix it",
        "workspace": f"{W}/missing",
        "tools": ["Read"],
        "depends_on": [],
    }
    add(wv("agentic no workspace", [agentic], shell_rw, flags=("--yes", "--json")))
    add(
        wv(
            "max parallel two",
            [rd("r1", f"{W}/x.txt"), rd("r2", f"{W}/y.txt"), sh("c", "true", ["r1", "r2"])],
            shell_rw,
            flags=("--yes", "--json", "--max-parallel", "2"),
            before={"w/x.txt": {"text": "x\n"}, "w/y.txt": {"text": "y\n"}},
        )
    )

    # Resume (WV-2).
    two = [sh("a", "printf a"), sh("b", "false", ["a"])]
    add(wv("resume after a failure", two, shell_rw, pre=[["--yes"]], flags=("--yes", "--resume")))
    add(
        wv(
            "resume after a failure json",
            two,
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume", "--json"),
        )
    )
    ok2 = [sh("a", "printf a"), sh("b", "printf b", ["a"])]
    add(wv("resume all done", ok2, shell_rw, pre=[["--yes"]], flags=("--yes", "--resume")))
    add(wv("resume no state", ok2, shell_rw, flags=("--yes", "--resume")))
    add(wv("no resume runs all again", ok2, shell_rw, pre=[["--yes"]], flags=("--yes",)))
    reads = [sh("a", "printf a"), rd("r", f"{W}/x.txt", ["a"]), sh("b", "false", ["r"])]
    add(
        wv(
            "resume reruns a read",
            reads,
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume", "--json"),
            before={"w/x.txt": {"text": "x\n"}},
        )
    )
    slot_steps = [
        out(sh("a", printf_json({"f": f"{W}/in/x.txt"})), f="path"),
        fill(rd("b", f"{W}/in/other.txt", ["a"]), path="a.f"),
        sh("c", "false", ["b"]),
    ]
    add(
        wv(
            "resume slot from a receipt",
            slot_steps,
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume"),
            before={"w/in/x.txt": {"text": "from x\n"}},
        )
    )
    add(
        wv(
            "resume slot from a receipt json",
            slot_steps,
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume", "--json"),
            before={"w/in/x.txt": {"text": "from x\n"}},
        )
    )
    mark = {".opendaisugi/weave": {"weave_marks": [["run_0badc0de", "b"]], "plan": "p.json"}}
    add(wv("resume started no receipt", ok2, shell_rw, flags=("--yes", "--resume"), before=mark))
    add(
        wv(
            "resume started no receipt json",
            ok2,
            shell_rw,
            flags=("--yes", "--resume", "--json"),
            before=mark,
        )
    )
    add(
        wv(
            "resume started rerun named",
            ok2,
            shell_rw,
            flags=("--yes", "--resume", "--rerun", "b"),
            before=mark,
        )
    )
    add(
        wv(
            "resume started other rerun named",
            ok2,
            shell_rw,
            flags=("--yes", "--resume", "--rerun", "a"),
            before=mark,
        )
    )
    add(wv("started no resume", ok2, shell_rw, flags=("--yes",), before=mark))
    crash = {".opendaisugi/weave": {"weave_marks": [["run_0badc0de", "b"]], "plan": "p.json"}}
    add(
        wv(
            "resume crash in a later run",
            two,
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume"),
            before=crash,
        )
    )
    read_mark = {".opendaisugi/weave": {"weave_marks": [["run_0badc0de", "r"]], "plan": "p.json"}}
    add(
        wv(
            "resume started read runs",
            [rd("r", f"{W}/x.txt")],
            shell_rw,
            flags=("--yes", "--resume"),
            before={**read_mark, "w/x.txt": {"text": "x\n"}},
        )
    )
    task_mark = {".opendaisugi/weave": {"weave_marks": [["run_0badc0de", "t"]], "plan": "p.json"}}
    add(
        wv(
            "resume started task runs",
            [task("t", "name one risk")],
            shell_rw,
            flags=("--yes", "--resume"),
            env=API,
            replies=[{"http": "A risk."}],
            before=task_mark,
        )
    )
    add(
        wv(
            "resume bad state lines",
            ok2,
            shell_rw,
            flags=("--yes", "--resume"),
            before={
                ".opendaisugi/weave": {
                    "weave_marks": [["run_0badc0de", "zz"]],
                    "plan": "p.json",
                },
            },
        )
    )
    add(
        wv(
            "resume task skipped",
            [t_out, t_read, sh("c", "false", ["b"])],
            dev,
            pre=[["--yes"]],
            flags=("--yes", "--resume", "--json"),
            env=API,
            replies=[{"http": null}],
        )
    )
    # Attempts: the runner asks N times, ranks the answers, goes on with the
    # leader and opens a card; a step that cannot be undone below an open
    # choice goes to the terminal ask, even under --yes.
    t3 = {**out(task("t", "name the file"), f="path"), "attempts": 3}
    t3_read = fill(rd("b", "/dev/placeholder", ["t"]), path="t.f")
    three = [{"http": null}, {"http": null}, {"http": '{"f": "/dev/zero"}'}]
    add(wv("attempts three", [t3, t3_read], dev, env=API, replies=three))
    add(
        wv(
            "attempts three json",
            [t3, t3_read],
            dev,
            flags=("--yes", "--json"),
            env=API,
            replies=three,
        )
    )
    add(
        wv(
            "attempts first out",
            [t3, t3_read],
            dev,
            env=API,
            replies=[{"http": "no idea"}, {"http": null}, {"http": null}],
        )
    )
    add(
        wv(
            "attempts none survive",
            [t3, t3_read],
            dev,
            flags=("--yes", "--json"),
            env=API,
            replies=[{"http": "no"}, {"http_status": 500, "body": "{}"}, {"http": "[1]"}],
        )
    )
    add(
        wv(
            "attempts one survives no card",
            [t3, t3_read],
            dev,
            env=API,
            replies=[{"http": "no"}, {"http": "no"}, {"http": null}],
        )
    )
    two_plain = {**task("t", "name one risk"), "attempts": 2}
    risks = [{"http": "A risk."}, {"http": "Another risk."}]
    add(
        wv(
            "attempts then a permanent step",
            [two_plain, sh("s", "true", ["t"])],
            shell_rw,
            env=API,
            replies=risks,
        )
    )
    add(
        wv(
            "attempts then a permanent step json",
            [two_plain, sh("s", "true", ["t"])],
            shell_rw,
            flags=("--yes", "--json"),
            env=API,
            replies=risks,
        )
    )
    add(
        wv(
            "attempts then an undoable write",
            [two_plain, wr("w", to, "done", ["t"])],
            shell_rw,
            env=API,
            replies=risks,
        )
    )
    add(
        wv(
            "attempts then a write outside",
            [two_plain, wr("w", "/nonexistent/out/a.txt", "done", ["t"])],
            env_doc(file_write=["/nonexistent/out/**"]),
            env=API,
            replies=risks,
        )
    )
    add(
        wv(
            "attempts resume keeps the card",
            [two_plain, sh("c", "false", ["t"])],
            shell_rw,
            pre=[["--yes"]],
            flags=("--yes", "--resume", "--json"),
            env=API,
            replies=risks,
        )
    )
    for label, step in (
        ("on a shell step", {**sh("a", "true"), "attempts": 2}),
        ("one", {**task("t", "x"), "attempts": 1}),
        ("nine", {**task("t", "x"), "attempts": 9}),
        ("a bool", {**task("t", "x"), "attempts": True}),
        ("text", {**task("t", "x"), "attempts": "2"}),
    ):
        add(wv(f"plan attempts {label}", [step], shell_rw))
    add(
        wv(
            "data dir",
            ok2,
            shell_rw,
            flags=("--yes", "--data-dir", "dd"),
        )
    )
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
    manifest["cases.jsonl"] = write_jsonl(out_dir / "cases.jsonl", cases)
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    cache_path.unlink(missing_ok=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

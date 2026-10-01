"""Robotics golden cases: the MuJoCo executors (`executor_mujoco`) and the
VLA scaffolding (`vla_executor`), run through the Python oracle.

    MUJOCO_GL=disable uv run --no-sync python clients/robotics_cases.py [--out clients/fixtures/robotics]

No command of the oracle builds a robot executor: `run`, `orchestrate` and
`weave` fail a robot step with "no executor for kind ...". Only the library
`Supervisor` takes them. So the cases drive the library, as
clients/alias_cases.py does for the alias registry, and the ports answer
the same cases with a probe (`robot-probe`) that reads the cases file and
writes one result line per case (clients/robotics_compare.py).

A case is one of four kinds:

- ``steps``: a MuJoCoExecutor built with the case's keyword arguments, the
  envelope (if any) applied through configure_from_envelope, then each step
  run in order: its rc and stdout, or the exception's class and text.
- ``run``: a Supervisor with robotics_executors (and a MockVLAExecutor for
  ``vla`` steps when the case names one), an always-approve strategy and a
  journal: the session, the receipts and the guard settings the envelope
  left on the executor.
- ``vla``: a MockVLAExecutor, or the bare VLAExecutorBase, and its steps.
- ``render``: a camera rendered offscreen. The oracle cannot render here
  (its EGL path needs PyOpenGL, which the robotics extra does not bring),
  so these cases carry no expectation; the compare checks the ports
  against each other (ruling RB-R-4).

Every case ends with the simulation state: qpos, qvel, ctrl, the contact
count, and the positions of the named bodies and sites. Floats are kept
as floats, so the compare sees every bit (ruling RB-R-3). duration_ms and
the run's ids and times are left out: they differ on every run.
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import sys
import tempfile
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "robotics"
CASE_VERSION = 1

ARM = "tests/fixtures/mjcf/two_joint_arm.xml"
CONTACT = "tests/fixtures/mjcf/contact_at_rest.xml"
SITES = "clients/fixtures/robotics/arm_sites.xml"
CTRLRANGE = "clients/fixtures/robotics/grip_ctrlrange.xml"
UNBOUNDED = "clients/fixtures/robotics/grip_unbounded.xml"

# Keys that change on every run.
VOLATILE = {"id", "run_id", "started_at", "ended_at", "duration_ms", "timestamp", "trace_id"}


def caught(fn) -> dict[str, Any]:
    try:
        return {"ok": fn()}
    except Exception as e:  # noqa: BLE001 - the case records it
        return {"error": type(e).__name__, "message": str(e)}


def state(model, data, bodies: list[str], sites: list[str]) -> dict[str, Any]:
    """The simulation state a case ends with."""
    import mujoco

    def named(kind, names, arr) -> dict[str, Any]:
        out: dict[str, Any] = {}
        for n in names:
            i = mujoco.mj_name2id(model, kind, n)
            out[n] = None if i < 0 else [float(v) for v in arr[i]]
        return out

    return {
        "qpos": [float(v) for v in data.qpos],
        "qvel": [float(v) for v in data.qvel],
        "ctrl": [float(v) for v in data.ctrl],
        "ncon": int(data.ncon),
        "bodies": named(mujoco.mjtObj.mjOBJ_BODY, bodies, data.xpos),
        "sites": named(mujoco.mjtObj.mjOBJ_SITE, sites, data.site_xpos),
    }


def scrub(v: Any) -> Any:
    """Drop the keys that change on every run, at any depth."""
    if isinstance(v, dict):
        return {k: scrub(x) for k, x in v.items() if k not in VOLATILE}
    if isinstance(v, list):
        return [scrub(x) for x in v]
    return v


def receipt_view(r: dict[str, Any]) -> dict[str, Any]:
    """A receipt as the compare sees it. The hash covers duration_ms, which
    differs on every run, so the hash is checked against the receipt's own
    evidence and then dropped (ruling RB-R-5)."""
    from opendaisugi.models import compute_evidence_hash

    out = dict(r)
    ev = dict(out.get("evidence") or {})
    out["evidence_hash_ok"] = out.pop("evidence_hash", None) == compute_evidence_hash(ev)
    out["evidence"] = ev
    return scrub(out)


def _steps_of(steps: list[dict[str, Any]]):
    from opendaisugi.models import ActionPlan

    return ActionPlan(source="robotics-case", task="robotics case", steps=steps).steps


def run_steps(case: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.executor_mujoco import MuJoCoExecutor
    from opendaisugi.models import Envelope

    try:
        ex = MuJoCoExecutor(str(REPO / case["mjcf"]), **case.get("executor", {}))
    except Exception as e:  # noqa: BLE001 - the case records it
        return {"made": {"error": type(e).__name__, "message": str(e)}}
    out: dict[str, Any] = {}
    if case.get("envelope") is not None:
        ex.configure_from_envelope(Envelope(**case["envelope"]))
    out["guards"] = {"torque_limit": ex.torque_limit, "forbid_contacts": ex.forbid_contacts}
    results = []
    for step in _steps_of(case["steps"]):
        r = caught(lambda s=step: ex.run(s, timeout_s=30, max_output_bytes=10 * 1024 * 1024))
        if "ok" in r:
            res = r["ok"]
            r = {"ok": {"rc": res.rc, "stdout": res.stdout, "timed_out": res.timed_out}}
        results.append(r)
    out["results"] = results
    out["state"] = state(ex.model, ex.data, case.get("bodies", []), case.get("sites", []))
    return out


def run_vla(case: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.vla_executor import MockVLAExecutor, VLAExecutorBase

    v = case["vla"]
    mjcf = str(REPO / case["mjcf"]) if case.get("mjcf") else v.get("mjcf_path", "")
    if v.get("base"):
        ex = VLAExecutorBase(mjcf_path=mjcf or None, **v.get("kwargs", {}))
    else:
        ex = MockVLAExecutor(mjcf_path=mjcf, **v.get("kwargs", {}))
    results = []
    for step in _steps_of(case["steps"]):
        res = ex.run(step, timeout_s=30, max_output_bytes=10 * 1024 * 1024)
        results.append({"rc": res.rc, "stdout": res.stdout, "timed_out": res.timed_out})
    out: dict[str, Any] = {"joint_names": list(ex._joint_names), "results": results}
    if ex._data is not None:
        out["state"] = state(ex._model, ex._data, case.get("bodies", []), case.get("sites", []))
    return out


def run_supervised(case: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.approval import CallbackStrategy
    from opendaisugi.cli import _serialize_session
    from opendaisugi.executor_mujoco import robotics_executors
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.supervisor import Supervisor
    from opendaisugi.vla_executor import MockVLAExecutor

    executors: dict[str, Any] = robotics_executors(
        str(REPO / case["mjcf"]), **case.get("executor", {})
    )
    vla = None
    if case.get("vla") is not None:
        vla = MockVLAExecutor(mjcf_path=str(REPO / case["mjcf"]), **case["vla"].get("kwargs", {}))
        executors["vla"] = vla
    plan = ActionPlan(**case["plan"])
    env = Envelope(**case["envelope"])
    tmp = os.environ.get("TMPDIR") or None
    with tempfile.TemporaryDirectory(dir=tmp) as d:
        j = Journal(data_dir=Path(d))
        sup = Supervisor(
            executors=executors,
            approval=CallbackStrategy(lambda step, env: True),
            journal=j,
        )
        session = asyncio.run(sup.run(plan, env))
        receipts = [r.model_dump(mode="json") for r in j.receipts_for_run(session.id)]
        j.close()
    ex = executors["joint_move"]
    out: dict[str, Any] = {
        "session": scrub(json.loads(json.dumps(_serialize_session(session), default=str))),
        "receipts": [receipt_view(r) for r in receipts],
        "guards": {"torque_limit": ex.torque_limit, "forbid_contacts": ex.forbid_contacts},
        "state": state(ex.model, ex.data, case.get("bodies", []), case.get("sites", [])),
    }
    if vla is not None:
        out["vla_state"] = state(
            vla._model, vla._data, case.get("bodies", []), case.get("sites", [])
        )
    return out


def run_case(case: dict[str, Any]) -> dict[str, Any]:
    """The oracle's answer for one case."""
    kind = case["kind"]
    if kind == "steps":
        return run_steps(case)
    if kind == "vla":
        return run_vla(case)
    if kind == "run":
        return run_supervised(case)
    raise ValueError(f"no oracle for case kind {kind!r}")


# ---- the cases ----------------------------------------------------------


def reset(sid: str, seed: int | None = None, deps=()) -> dict[str, Any]:
    s: dict[str, Any] = {"type": "sim_reset", "id": sid, "depends_on": list(deps)}
    if seed is not None:
        s["seed"] = seed
    return s


def joint(sid: str, targets: dict[str, Any], deps=(), **kw: Any) -> dict[str, Any]:
    return {
        "type": "joint_move",
        "id": sid,
        "joint_targets": targets,
        "depends_on": list(deps),
        **kw,
    }


def cart(sid: str, pos, deps=(), **kw: Any) -> dict[str, Any]:
    return {
        "type": "cartesian_move",
        "id": sid,
        "target_position": list(pos),
        "depends_on": list(deps),
        **kw,
    }


def grip(sid: str, action: str, deps=(), **kw: Any) -> dict[str, Any]:
    return {"type": "gripper", "id": sid, "action": action, "depends_on": list(deps), **kw}


def vla(sid: str, task: str, deps=(), **kw: Any) -> dict[str, Any]:
    return {"type": "vla", "id": sid, "task": task, "depends_on": list(deps), **kw}


def chain(steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Each step depends on the one before it."""
    out = []
    for i, s in enumerate(steps):
        s = dict(s)
        if i and not s.get("depends_on"):
            s["depends_on"] = [steps[i - 1]["id"]]
        out.append(s)
    return out


ARM_BODIES = ["link1", "link2", "end_effector", "finger", "block"]
SITES_LIST = ["ee_site", "block_site"]


def pick_place_steps() -> list[dict[str, Any]]:
    """The sequence of tests/test_mujoco_pickplace.py."""
    return chain(
        [
            reset("reset"),
            joint("home", {"j1": 0.0, "j2": 0.0}, duration_s=0.5),
            cart("approach", (0.35, 0.20, 0.0)),
            grip("grasp", "close", hold_s=0.3),
            cart("lift", (0.35, 0.15, 0.0)),
            cart("transport", (0.20, 0.35, 0.0)),
            grip("release", "open", hold_s=0.3),
            cart("retreat", (0.30, 0.30, 0.0)),
        ]
    )


def dish_steps(num: int) -> list[dict[str, Any]]:
    """The dish-wash kit (examples/dish-wash) as plain joint moves.

    The kit's own step types are user code the ports cannot read, so each
    becomes the joint_move its DishWashMuJoCoExecutor runs, with the same
    targets (the scrub target's sine computed here, as the kit does) and
    the kit's settle_steps=200 (ruling RB-R-6)."""
    import math

    steps = []
    for di in range(num):
        b = len(steps)
        steps += [
            joint(f"s{b}", {"j1": 0.6 - di * 0.15, "j2": 0.4}),
            joint(f"s{b + 1}", {"j1": 0.6 - di * 0.15, "j2": 0.55}),
            joint(f"s{b + 2}", {"j1": 0.65 - di * 0.15, "j2": 0.55 + 0.05 * math.sin(di)}),
            joint(f"s{b + 3}", {"j1": 0.55 - di * 0.15, "j2": 0.45}),
            joint(f"s{b + 4}", {"j1": 0.0, "j2": 0.0}),
        ]
    return chain(steps)


def robot_env(**perm: Any) -> dict[str, Any]:
    p = {
        "joint_limits": {"j1": [-3.14, 3.14], "j2": [-3.14, 3.14], "j_grip": [-0.05, 0.05]},
        "velocity_limit": 5.0,
        "workspace_bounds": [[-1.0, -1.0, -1.0], [1.0, 1.0, 1.0]],
    }
    p.update(perm)
    return {
        "id": "env_robotics",
        "generated_by": "robotics-case",
        "task": "move the arm",
        "permissions": p,
        "invariants": [
            {"type": "joint_limits_respected", "description": "joints stay in range"},
            {"type": "velocity_bounded", "description": "velocity stays bounded"},
        ],
    }


def plan_of(steps: list[dict[str, Any]], task: str = "move the arm") -> dict[str, Any]:
    return {"id": "plan_robotics", "source": "robotics-case", "task": task, "steps": steps}


def build_cases() -> list[dict[str, Any]]:
    C: list[dict[str, Any]] = []

    def add(name: str, kind: str, **kw: Any) -> None:
        C.append({"name": name, "kind": kind, **kw})

    def steps(name: str, mjcf: str, st: list[dict[str, Any]], **kw: Any) -> None:
        kw.setdefault("bodies", ARM_BODIES if mjcf in (ARM, SITES) else ["end_effector"])
        if mjcf == SITES:
            kw.setdefault("sites", SITES_LIST)
        add(name, "steps", mjcf=mjcf, steps=st, **kw)

    # -- pick and place, every step type --------------------------------
    steps("pick place sequence", ARM, pick_place_steps())
    steps("pick place on the site arm", SITES, pick_place_steps())
    steps("pick place short settle", SITES, pick_place_steps(), executor={"settle_steps": 300})

    # -- sim_reset -------------------------------------------------------
    steps("reset no seed", ARM, [reset("r")])
    steps("reset with seed", ARM, [reset("r", 7)])
    steps("reset after a move", ARM, [joint("m", {"j1": 0.4}), reset("r")])

    # -- joint_move ------------------------------------------------------
    steps("joint move one joint", ARM, [joint("m", {"j1": 0.5})])
    steps("joint move two joints", SITES, [joint("m", {"j2": -0.3, "j1": 0.7})])
    steps("joint move int target", ARM, [joint("m", {"j1": 0, "j2": 1})])
    steps("joint move gripper slide", ARM, [joint("m", {"j_grip": 0.02})])
    steps("joint move unknown joint", ARM, [joint("m", {"nope": 0.1})])
    steps("joint move joint without actuator", CTRLRANGE, [joint("m", {"j_free": 0.1})])
    steps("joint move settle 1", ARM, [joint("m", {"j1": 0.5})], executor={"settle_steps": 1})
    steps("joint move settle 0", ARM, [joint("m", {"j1": 0.5})], executor={"settle_steps": 0})
    steps(
        "joint move persists state",
        ARM,
        [joint("a", {"j1": 0.3}), joint("b", {"j2": 0.6}), joint("c", {"j1": -0.2})],
    )

    # -- gripper ---------------------------------------------------------
    steps("gripper close", ARM, [grip("g", "close")])
    steps("gripper open hold", ARM, [grip("g", "open", hold_s=0.3)])
    steps("gripper hold zero", ARM, [grip("g", "close", hold_s=0.0)])
    steps("gripper hold tiny", ARM, [grip("g", "open", hold_s=0.0035)])
    steps("gripper no actuator", CONTACT, [grip("g", "close")])
    steps("gripper ctrlrange", CTRLRANGE, [grip("g", "open"), grip("h", "close")])
    steps("gripper unbounded", UNBOUNDED, [grip("g", "open")])

    # -- cartesian_move and the IK ---------------------------------------
    steps("cartesian reachable", ARM, [cart("c", (0.35, 0.20, 0.0))])
    steps(
        "cartesian from home",
        SITES,
        [joint("h", {"j1": 0.0, "j2": 0.0}), cart("c", (0.4, -0.1, 0.0))],
    )
    steps("cartesian unreachable", ARM, [cart("c", (2.0, 2.0, 0.0))])
    steps("cartesian out of plane", ARM, [cart("c", (0.3, 0.3, 0.5))])
    steps("cartesian ee missing", ARM, [cart("c", (0.3, 0.3, 0.0))], executor={"ee_body": "nope"})
    steps("cartesian other ee", ARM, [cart("c", (0.2, 0.1, 0.0))], executor={"ee_body": "link2"})
    steps(
        "cartesian few iterations",
        ARM,
        [cart("c", (0.1, 0.4, 0.0))],
        executor={"ik_max_iter": 3},
    )
    steps(
        "cartesian loose tolerance",
        ARM,
        [cart("c", (0.25, 0.25, 0.0))],
        executor={"ik_tol": 0.05, "ik_damping": 0.3},
    )
    steps(
        "cartesian with orientation",
        ARM,
        [cart("c", (0.3, 0.2, 0.0), target_orientation=[1, 0, 0, 0])],
    )
    steps("cartesian keeps gripper", ARM, [grip("g", "close"), cart("c", (0.3, -0.2, 0.0))])
    steps("cartesian one hinge", CTRLRANGE, [cart("c", (0.0, 0.1, 0.0))])

    # -- guards ----------------------------------------------------------
    steps("torque violation", ARM, [joint("m", {"j1": 1.0})], executor={"torque_limit": 0.1})
    steps("torque within limit", ARM, [joint("m", {"j1": 0.1})], executor={"torque_limit": 1000.0})
    steps("torque during gripper", ARM, [grip("g", "close")], executor={"torque_limit": 0.01})
    steps(
        "torque during cartesian",
        ARM,
        [cart("c", (0.35, 0.2, 0.0))],
        executor={"torque_limit": 0.5},
    )
    steps(
        "contact violation", CONTACT, [joint("m", {"j1": 0.1})], executor={"forbid_contacts": True}
    )
    steps(
        "contact unnamed geoms",
        CTRLRANGE,
        [joint("m", {"j1": 0.1})],
        executor={"forbid_contacts": True},
    )
    steps("contact allowed", CONTACT, [joint("m", {"j1": 0.1})])
    steps(
        "contact on the site arm",
        SITES,
        [cart("c", (0.35, 0.20, 0.0))],
        executor={"forbid_contacts": True},
    )

    # -- configure_from_envelope ----------------------------------------
    steps(
        "envelope sets torque",
        ARM,
        [joint("m", {"j1": 0.8})],
        envelope=robot_env(torque_limit=0.2),
    )
    steps(
        "envelope obstacles forbid contacts",
        CONTACT,
        [joint("m", {"j1": 0.1})],
        envelope=robot_env(obstacles=[[[5, 5, 5], [6, 6, 6]]]),
    )
    steps(
        "envelope leaves kwargs",
        ARM,
        [joint("m", {"j1": 0.2})],
        executor={"torque_limit": 900.0},
        envelope=robot_env(),
    )

    # -- steps the executor does not run --------------------------------
    steps("shell step refused", ARM, [{"type": "shell", "id": "s", "command": "echo hi"}])
    steps("vla step refused", ARM, [vla("v", "wave")])

    # -- the executor cannot be made ------------------------------------
    steps("mjcf missing", "clients/fixtures/robotics/absent.xml", [reset("r")])

    # -- VLA --------------------------------------------------------------
    def vcase(name: str, mjcf: str | None, st, **kw: Any) -> None:
        kw.setdefault("bodies", ["end_effector"])
        add(name, "vla", mjcf=mjcf, steps=st, **kw)

    vcase("mock vla target", ARM, [vla("v", "reach", target_pose=[0.3, -0.2, 0.0])], vla={})
    vcase("mock vla no target", ARM, [vla("v", "rest")], vla={})
    vcase(
        "mock vla twice",
        SITES,
        [vla("v", "reach", target_pose=[0.5, 0.4, 0.0]), vla("w", "back", target_pose=[0, 0, 0])],
        vla={},
        sites=SITES_LIST,
    )
    vcase(
        "mock vla capped by step",
        ARM,
        [vla("v", "reach", target_pose=[0.3, 0.2, 0.1], max_actions=4)],
        vla={},
    )
    vcase(
        "mock vla many actions",
        ARM,
        [vla("v", "reach", target_pose=[1.0, -1.0, 0.0])],
        vla={"kwargs": {"num_actions": 60}},
    )
    vcase(
        "mock vla zero actions",
        ARM,
        [vla("v", "reach", target_pose=[1.0, -1.0, 0.0])],
        vla={"kwargs": {"num_actions": 0}},
    )
    vcase("mock vla one joint", CONTACT, [vla("v", "turn", target_pose=[0.4, 0.9, 0.0])], vla={})
    vcase(
        "mock vla gripper only",
        UNBOUNDED,
        [vla("v", "squeeze", target_pose=[0.1, 0.1, 0.0])],
        vla={},
    )
    vcase(
        "mock vla ctrlrange arm", CTRLRANGE, [vla("v", "turn", target_pose=[0.2, 0.3, 0.0])], vla={}
    )
    vcase("mock vla not a vla step", ARM, [joint("m", {"j1": 0.1})], vla={})
    vcase("mock vla no mjcf", None, [vla("v", "reach")], vla={"mjcf_path": ""})
    vcase("base vla not implemented", ARM, [vla("v", "reach")], vla={"base": True})
    vcase("base vla no mjcf", None, [vla("v", "reach")], vla={"base": True})
    vcase(
        "base vla global cap",
        ARM,
        [vla("v", "reach")],
        vla={"base": True, "kwargs": {"max_actions_global": 3}},
    )

    # -- through the supervisor ------------------------------------------
    def rcase(name: str, mjcf: str, st, env=None, **kw: Any) -> None:
        kw.setdefault("bodies", ARM_BODIES if mjcf in (ARM, SITES) else ["end_effector"])
        if mjcf == SITES:
            kw.setdefault("sites", SITES_LIST)
        add(name, "run", mjcf=mjcf, plan=plan_of(st), envelope=env or robot_env(), **kw)

    rcase("run pick place", SITES, pick_place_steps())
    rcase("run dish wash one plate", ARM, dish_steps(1), executor={"settle_steps": 200})
    rcase("run dish wash two plates", ARM, dish_steps(2), executor={"settle_steps": 200})
    rcase(
        "run torque from envelope",
        ARM,
        chain([reset("r"), joint("m", {"j1": 0.5})]),
        env=robot_env(torque_limit=0.1),
    )
    rcase("run no torque limit", ARM, chain([reset("r"), joint("m", {"j1": 0.5})]))
    rcase(
        "run obstacles enable contact guard",
        SITES,
        chain([reset("r"), cart("c", (0.35, 0.20, 0.0))]),
        env=robot_env(obstacles=[[[10, 10, 10], [11, 11, 11]]]),
    )
    rcase(
        "run ik failure",
        ARM,
        chain([reset("r"), cart("c", (2.0, 2.0, 0.0)), joint("m", {"j1": 0.1})]),
    )
    rcase(
        "run unknown joint",
        ARM,
        chain([joint("m", {"nope": 0.1})]),
        env=robot_env(joint_limits={"nope": [-1, 1]}),
    )
    rcase("run gripper unbounded", UNBOUNDED, chain([grip("g", "open")]))
    rcase(
        "run vla mock",
        SITES,
        chain(
            [
                reset("r"),
                vla("v", "reach the block", target_pose=[0.3, 0.2, 0.0]),
                joint("m", {"j1": 0.0}),
            ]
        ),
        vla={},
    )
    rcase("run vla no executor", ARM, chain([vla("v", "reach", target_pose=[0.3, 0.2, 0.0])]))
    rcase(
        "run rejected joint limit",
        ARM,
        chain([joint("m", {"j1": 3.5})]),
    )
    rcase(
        "run outside workspace",
        ARM,
        chain([cart("c", (1.5, 0.0, 0.0))]),
    )

    # -- rendering (the ports against each other) ------------------------
    add("render overhead", "render", mjcf=SITES, steps=[], camera="overhead", width=64, height=48)
    add("render free camera", "render", mjcf=SITES, steps=[], camera=None, width=64, height=48)
    add(
        "render after moves",
        "render",
        mjcf=SITES,
        steps=chain([joint("m", {"j1": 0.6, "j2": -0.4}), grip("g", "close")]),
        camera="overhead",
        width=320,
        height=240,
    )
    add(
        "render too large", "render", mjcf=SITES, steps=[], camera="overhead", width=640, height=480
    )
    add("render unknown camera", "render", mjcf=SITES, steps=[], camera="nope", width=64, height=48)
    return C


def unroot(v: Any) -> Any:
    """Name the checkout as <REPO> in every string, so the expectations do
    not depend on where the repository lies."""
    return json.loads(json.dumps(v).replace(json.dumps(str(REPO))[1:-1], "<REPO>"))


def case_id(case: dict[str, Any]) -> str:
    body = {k: v for k, v in case.items() if k not in ("id", "expect")}
    return hashlib.sha256(json.dumps(body, sort_keys=True).encode()).hexdigest()[:16]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", default=str(FIXTURE_DIR))
    args = ap.parse_args()
    sys.path.insert(0, str(REPO / "src"))
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    lines = []
    for c in build_cases():
        c = {"id": case_id(c), "version": CASE_VERSION, **c}
        if c["kind"] != "render":
            c["expect"] = unroot(run_case(c))
        # Key order is kept: a joint_move runs its joint_targets in order.
        lines.append(json.dumps(c, ensure_ascii=True))
    (out / "cases.jsonl").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"robotics: {len(lines)} cases written to {out / 'cases.jsonl'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

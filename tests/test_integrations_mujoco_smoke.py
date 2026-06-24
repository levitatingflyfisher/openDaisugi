"""Test for the MuJoCo smoke kit (v0.10.0).

Skipped when mujoco isn't installed (as on the 12 GB dev box); run on
the 4080 box or any machine with 'pip install opendaisugi[robotics]'.
"""

from __future__ import annotations

import runpy
import sys
from pathlib import Path

import pytest

pytest.importorskip("mujoco")


SMOKE = Path(__file__).parent.parent / "examples" / "integrations" / "mujoco" / "smoke.py"


def test_mujoco_smoke_envelope_builds_without_mujoco():
    """The envelope + plan builders should be MuJoCo-independent.

    If this ever breaks, the smoke script has accidentally tangled
    planning code with rollout code.
    """
    sys.path.insert(0, str(SMOKE.parent))
    try:
        smoke = runpy.run_path(str(SMOKE))
        envelope = smoke["build_envelope"]()
        plan = smoke["build_plan"]()
        assert envelope.permissions.joint_limits["j1"] == (-1.5, 1.5)
        assert len(plan.steps) == 3
    finally:
        sys.path.pop(0)


def test_mujoco_smoke_main_runs():
    """Full smoke: Stage 1 verify → MuJoCo rollout → bounds assertion."""
    sys.path.insert(0, str(SMOKE.parent))
    try:
        smoke = runpy.run_path(str(SMOKE))
        assert smoke["main"]() == 0
    finally:
        sys.path.pop(0)

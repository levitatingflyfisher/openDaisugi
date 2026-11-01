"""The 3D gallery's tests, collected by the default suite.

The scenes live in ``examples/gallery3d`` with their tests beside them, and
the default suite collects ``tests/`` only. This file loads each of those
test files and hands its tests to pytest, so a change to the verifier that
breaks a scene, or a picture that would say more than was checked, fails
here and not only when someone remembers to run the gallery.

The checks (``test_delivery``, ``test_patrol``, ``test_many``) need nothing
beyond the package. The pictures need MuJoCo, NumPy and Pillow
(``uv sync --extra robotics``, and Pillow through ``uv run --with pillow``).
Where one of them is absent, each picture file gives one skipped test that
names what is missing. Where they are present and MuJoCo cannot make a
renderer, the tests that draw skip by their own marks.

A test is named ``<its own name>__<its file>``. The files are loaded by
path under a private module name, so pytest does not rewrite their asserts:
for the full detail of a failure, run ``pytest examples/gallery3d``.
"""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

import pytest

GALLERY = Path(__file__).resolve().parents[1] / "examples" / "gallery3d"
CHECKS = ("test_delivery", "test_patrol", "test_many", "test_thumbs")
PICTURES = ("test_render_delivery", "test_render_patrol", "test_render_many")
PICTURES_NEED = ("mujoco", "numpy", "PIL")


def missing() -> list[str]:
    """The modules the pictures need that this environment does not have."""
    return [name for name in PICTURES_NEED if importlib.util.find_spec(name) is None]


def _load(name: str):
    spec = importlib.util.spec_from_file_location(f"_gallery3d_{name}", GALLERY / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _skipped(name: str, absent: list[str]):
    @pytest.mark.skip(reason=f"examples/gallery3d/{name}.py needs {', '.join(absent)}")
    def test():
        raise AssertionError("skipped")

    return test


def _collect() -> dict[str, object]:
    # The scene modules import each other by bare name (patrol, many, render_delivery).
    if str(GALLERY) not in sys.path:
        sys.path.insert(0, str(GALLERY))
    absent = missing()
    found: dict[str, object] = {}
    for name in (*CHECKS, *PICTURES):
        short = name.removeprefix("test_")
        if name in PICTURES and absent:
            found[f"test_pictures_are_skipped__{short}"] = _skipped(name, absent)
            continue
        for attr, value in vars(_load(name)).items():
            if attr.startswith("test_") and callable(value):
                found[f"{attr}__{short}"] = value
    return found


globals().update(_collect())


def test_every_gallery_test_file_is_listed_here():
    """A new test file in the gallery must be added to CHECKS or PICTURES, or the default
    suite would pass without running it."""
    on_disk = {p.stem for p in GALLERY.glob("test_*.py")}
    assert on_disk == set(CHECKS) | set(PICTURES)

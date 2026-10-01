"""No test resolves a path in the operator's own home: conftest moves HOME
into a scratch directory before anything imports opendaisugi."""

from __future__ import annotations

import os
from pathlib import Path

REAL_HOME = os.environ.get("DAISUGI_TEST_REAL_HOME", "")
TEST_HOME = os.environ["HOME"]


def test_home_is_the_scratch_home():
    assert REAL_HOME, "conftest did not move HOME"
    assert TEST_HOME != REAL_HOME
    assert Path(TEST_HOME).name.startswith("daisugi-test-home-")
    assert Path.home() == Path(TEST_HOME)
    for var in ("XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "OPENDAISUGI_HOME"):
        assert var not in os.environ, var


def test_the_default_dirs_are_in_the_scratch_home():
    from opendaisugi.floor_config import coppice_data_dirs
    from opendaisugi.gate import DEFAULT_GATE_ROOT

    for p in [DEFAULT_GATE_ROOT, *coppice_data_dirs()]:
        assert Path(p).is_relative_to(TEST_HOME), p

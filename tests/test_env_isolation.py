"""Whole-branch review, minor 4: tests must not run against whatever floor
pane env vars the developer's real shell happens to have set.

``opendaisugi._state_report.report_state`` defaults its ``env`` parameter
to ``os.environ`` (spec-01, §3.1's delivery side) — if the very session
running the tests is itself inside a coppice pane or a Herdr pane,
COPPICE_SOCK/COPPICE_PANE/HERDR_PANE_ID/HERDR_PANE would already be set in
the real environment, and any test that exercises the default-env path
(rather than passing its own ``env=``) would silently reach a real socket
or binary instead of the test's own fixtures. ``tests/conftest.py`` clears
all four before every test via an autouse fixture; this pins that.
"""

from __future__ import annotations

import os

import pytest

_DIRTY_ENV = {
    "COPPICE_SOCK": "/should/not/leak.sock",
    "COPPICE_PANE": "w1:p1",
    "HERDR_PANE_ID": "w1:p1",
    "HERDR_PANE": "w1:p1",
}


@pytest.fixture(autouse=True, scope="module")
def _pollute_the_real_environment_once():
    """Real ``os.environ`` mutation, not monkeypatch — simulates a
    developer's actual shell having these set. Module-scoped so it runs
    (and is torn down) around the whole module, but the key property is
    that pytest instantiates higher-scoped fixtures BEFORE lower-scoped
    ones for the same test request: this setup runs before
    tests/conftest.py's function-scoped env-clearing autouse fixture, so
    the test below only sees a clean environment if that fixture actually
    did its job.
    """
    prior = {k: os.environ.get(k) for k in _DIRTY_ENV}
    os.environ.update(_DIRTY_ENV)
    yield
    for k, v in prior.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v


def test_pane_env_vars_never_leak_into_a_test():
    for var in _DIRTY_ENV:
        assert var not in os.environ, f"{var} leaked into a test from the real environment"

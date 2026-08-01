"""`import opendaisugi` must not load the verifier, the executors, or networkx."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import opendaisugi

HEAVY = ("z3", "networkx", "opendaisugi.agentic_executor", "opendaisugi.verify", "opendaisugi.gate")


def _modules_after(stmt: str) -> set[str]:
    code = f"import sys; {stmt}; print('\\n'.join(sorted(sys.modules)))"
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    return set(out.stdout.split())


def test_import_package_is_light():
    loaded = _modules_after("import opendaisugi")
    assert not (loaded & set(HEAVY)), loaded & set(HEAVY)


def test_import_cli_is_light():
    loaded = _modules_after("import opendaisugi.cli")
    assert not (loaded & set(HEAVY)), loaded & set(HEAVY)


def test_every_public_name_still_resolves():
    missing = []
    for name in opendaisugi.__all__:
        try:
            getattr(opendaisugi, name)
        except ImportError:  # an optional extra (signing) is allowed to be absent
            continue
        except AttributeError:
            missing.append(name)
    assert not missing, missing


def test_facade_and_constants_are_reachable():
    from opendaisugi import DEFAULT_DATA_DIR, Daisugi, verify  # noqa: F401

    assert callable(verify)
    assert Daisugi.__module__ == "opendaisugi.facade"
    # DEFAULT_DATA_DIR's literal value isn't asserted here: tests/conftest.py's
    # autouse `_isolate_default_data_dir` fixture redirects it to a per-test
    # tmp dir for every test in the suite (so a bare Daisugi() never touches
    # the real ~/.opendaisugi). Reachability + type is what this test owns.
    assert isinstance(DEFAULT_DATA_DIR, Path)


def test_dir_lists_lazy_names():
    assert "Daisugi" in dir(opendaisugi) and "verify" in dir(opendaisugi)


def test_unknown_attribute_raises_attribute_error():
    import pytest

    with pytest.raises(AttributeError):
        opendaisugi.no_such_thing  # noqa: B018


def test_integrations_is_reachable_and_not_self_referential():
    # `from opendaisugi import integrations` used to be a top-level eager import;
    # lazily, "integrations" must resolve to the submodule, not recurse into
    # opendaisugi.__getattr__("integrations") -> import opendaisugi -> itself.
    mod = opendaisugi.integrations
    assert mod.__name__ == "opendaisugi.integrations"


def test_gate_import_does_not_load_networkx():
    loaded = _modules_after("import opendaisugi.gate")
    assert "networkx" not in loaded
    assert "z3" in loaded  # the verifier itself is expected here

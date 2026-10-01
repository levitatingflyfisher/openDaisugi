"""The data directory rule, the same in Python, Go and Rust:

OPENDAISUGI_HOME when set; else $XDG_DATA_HOME/opendaisugi when
XDG_DATA_HOME is set and ~/.opendaisugi does not exist; else ~/.opendaisugi.
An existing install keeps its directory.
"""

from __future__ import annotations

from pathlib import Path

from opendaisugi.datahome import data_home, guarded_data_dirs


def test_neither_set_is_the_dot_directory(tmp_path):
    assert data_home({}, tmp_path) == tmp_path / ".opendaisugi"


def test_opendaisugi_home_wins(tmp_path):
    (tmp_path / ".opendaisugi").mkdir()
    env = {"OPENDAISUGI_HOME": str(tmp_path / "od"), "XDG_DATA_HOME": str(tmp_path / "x")}
    assert data_home(env, tmp_path) == tmp_path / "od"


def test_xdg_when_the_dot_directory_is_absent(tmp_path):
    env = {"XDG_DATA_HOME": str(tmp_path / "x")}
    assert data_home(env, tmp_path) == tmp_path / "x" / "opendaisugi"


def test_an_existing_dot_directory_keeps_working(tmp_path):
    (tmp_path / ".opendaisugi").mkdir()
    env = {"XDG_DATA_HOME": str(tmp_path / "x")}
    assert data_home(env, tmp_path) == tmp_path / ".opendaisugi"


def test_empty_values_count_as_unset(tmp_path):
    env = {"OPENDAISUGI_HOME": "", "XDG_DATA_HOME": ""}
    assert data_home(env, tmp_path) == tmp_path / ".opendaisugi"


def test_the_process_environment_is_the_default(monkeypatch, tmp_path):
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("OPENDAISUGI_HOME", str(tmp_path / "od"))
    assert data_home() == tmp_path / "od"


def test_the_guarded_set_holds_every_place_the_data_can_be(tmp_path):
    """The gate guards each directory the rule can pick, with no exists
    check, so a move of the data never leaves the gate's own state open."""
    env = {"OPENDAISUGI_HOME": str(tmp_path / "od"), "XDG_DATA_HOME": str(tmp_path / "x")}
    assert guarded_data_dirs(env, tmp_path) == [
        tmp_path / ".opendaisugi",
        tmp_path / "od",
        tmp_path / "x" / "opendaisugi",
    ]
    assert guarded_data_dirs({}, tmp_path) == [tmp_path / ".opendaisugi"]
    same = {"OPENDAISUGI_HOME": str(tmp_path / ".opendaisugi")}
    assert guarded_data_dirs(same, tmp_path) == [tmp_path / ".opendaisugi"]


def test_the_module_is_stdlib_only():
    """gate_client imports it on the hook's hot path."""
    src = (Path(__file__).resolve().parent.parent / "src/opendaisugi/datahome.py").read_text()
    imports = [ln for ln in src.splitlines() if ln.startswith(("import ", "from "))]
    assert all(
        ln.split()[1].split(".")[0] in {"__future__", "os", "pathlib", "collections"}
        for ln in imports
    ), imports


def test_a_leading_tilde_is_expanded(tmp_path):
    assert data_home({"OPENDAISUGI_HOME": "~/od"}, tmp_path) == tmp_path / "od"
    assert data_home({"OPENDAISUGI_HOME": "~"}, tmp_path) == tmp_path
    xdg = {"XDG_DATA_HOME": "~/x"}
    assert data_home(xdg, tmp_path) == tmp_path / "x" / "opendaisugi"


def test_a_value_that_is_not_absolute_is_ignored(tmp_path):
    """A relative value would move with the cwd, so the gate's state could
    land in an agent's workspace. It is ignored, as the XDG spec says."""
    for bad in ("od", "./od", "~user/od", "~od", ""):
        assert data_home({"OPENDAISUGI_HOME": bad}, tmp_path) == tmp_path / ".opendaisugi", bad
        assert data_home({"XDG_DATA_HOME": bad}, tmp_path) == tmp_path / ".opendaisugi", bad
    both = {"OPENDAISUGI_HOME": "od", "XDG_DATA_HOME": str(tmp_path / "x")}
    assert data_home(both, tmp_path) == tmp_path / "x" / "opendaisugi"


def test_the_guarded_set_holds_the_expanded_value_and_drops_relative_ones(tmp_path):
    env = {"OPENDAISUGI_HOME": "~/od", "XDG_DATA_HOME": "rel"}
    assert guarded_data_dirs(env, tmp_path) == [tmp_path / ".opendaisugi", tmp_path / "od"]

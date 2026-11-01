import json

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.recipes import RECIPE_DIR, load, load_all, names, render_show

runner = CliRunner()


def test_four_recipes_ship_and_validate():
    assert names() == [
        "bring-your-own",
        "coppice-claude-warden",
        "router-and-routing-mode",
        "warden-only",
    ]
    assert len(load_all()) == 4
    assert RECIPE_DIR.is_dir()


def test_every_install_step_names_known_components_only():
    from opendaisugi.install import COMPONENTS

    known = {c[0] for c in COMPONENTS}
    for r in load_all():
        for s in r["steps"]:
            if s["kind"] == "install":
                assert set(s["only"]) <= known, (r["name"], s["only"])


def _write(tmp_path, monkeypatch, steps, params=None):
    bad = tmp_path / "x.json"
    bad.write_text(
        json.dumps(
            {
                "name": "x",
                "title": "t",
                "doc": "d",
                "slots": {},
                "params": params or {},
                "steps": steps,
            }
        )
    )
    monkeypatch.setattr("opendaisugi.recipes.RECIPE_DIR", tmp_path)


def test_loader_rejects_unknown_kind(tmp_path, monkeypatch):
    _write(tmp_path, monkeypatch, [{"kind": "shell", "command": "rm -rf /"}])
    with pytest.raises(ValueError, match="x.json: steps\\[0\\].kind 'shell'"):
        load("x")


def test_loader_rejects_undeclared_param(tmp_path, monkeypatch):
    _write(tmp_path, monkeypatch, [{"kind": "warden-init", "workspace": "{workspace}"}])
    with pytest.raises(ValueError, match="param 'workspace' is not declared"):
        load("x")


def test_loader_rejects_unknown_config_key(tmp_path, monkeypatch):
    _write(tmp_path, monkeypatch, [{"kind": "config", "set": {"auto_tend": "true"}}])
    with pytest.raises(ValueError, match="x.json: steps\\[0\\].set.auto_tend"):
        load("x")


def test_loader_rejects_unknown_component(tmp_path, monkeypatch):
    _write(tmp_path, monkeypatch, [{"kind": "install", "only": ["journal"]}])
    with pytest.raises(ValueError, match="unknown component 'journal'"):
        load("x")


def test_loader_rejects_unknown_harness(tmp_path, monkeypatch):
    _write(tmp_path, monkeypatch, [{"kind": "harness", "name": "emacs"}])
    with pytest.raises(ValueError, match="steps\\[0\\].name"):
        load("x")


def test_recipe_list_text_and_json():
    res = runner.invoke(app, ["recipe", "list"])
    assert res.exit_code == 0 and res.output.startswith("bring-your-own")
    js = json.loads(runner.invoke(app, ["recipe", "list", "--json"]).output)
    assert [r["name"] for r in js] == names() and set(js[0]) == {"name", "title", "doc"}


def test_recipe_show_numbers_the_steps():
    res = runner.invoke(app, ["recipe", "show", "warden-only"])
    assert res.exit_code == 0
    assert (
        "1. warden-init" in res.output
        and "2. install  --only warden --runtime claude" in res.output
    )
    assert "(only with --set workspace=DIR)" in res.output


def test_render_show_names_params():
    text = render_show(load("bring-your-own"))
    assert "efficient_model (required):" in text
    assert "routing_mode (default rules):" in render_show(load("router-and-routing-mode"))


def test_recipe_show_json_is_the_file():
    res = runner.invoke(app, ["recipe", "show", "warden-only", "--json"])
    assert json.loads(res.output) == json.loads((RECIPE_DIR / "warden-only.json").read_text())


def test_recipe_show_unknown_exits_2():
    res = runner.invoke(app, ["recipe", "show", "nope"])
    assert res.exit_code == 2 and "unknown recipe 'nope'. Recipes: bring-your-own," in res.output

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def snapshot(root: Path) -> dict[str, bytes]:
    return {str(p.relative_to(root)): p.read_bytes() for p in root.rglob("*") if p.is_file()}


def home(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude").mkdir()
    return tmp_path


def test_apply_without_yes_writes_nothing(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}"])
    assert res.exit_code == 0, res.output
    assert "Dry run. Add --yes to apply." in res.output and "would write" in res.output
    assert snapshot(h) == before


def test_apply_yes_runs_steps_and_writes_a_receipt(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"])
    assert res.exit_code == 0, res.output
    assert (h / ".opendaisugi" / "warden" / "envelopes" / "default.json").exists()
    assert "warden" in (h / ".claude" / "settings.json").read_text()
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert receipt["recipe"] == "warden-only" and receipt["completed"] == 3
    assert list(receipt) == ["recipe", "applied_at", "params", "completed", "steps"]
    assert receipt["steps"][0]["undo"] == {"remove_envelope": "default"}
    assert receipt["steps"][1]["undo"] == {"uninstall": {"claude": ["warden"]}}


def test_apply_twice_keeps_everything(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    runner.invoke(app, argv)
    after_one = snapshot(h)
    first = json.loads(after_one[".opendaisugi/recipes/warden-only.json"])
    res = runner.invoke(app, argv)
    assert res.exit_code == 0 and res.output.count("kept") >= 2
    now = snapshot(h)
    second = json.loads(now.pop(".opendaisugi/recipes/warden-only.json"))
    after_one.pop(".opendaisugi/recipes/warden-only.json")
    assert now == after_one
    # The undo of the run that did the work survives the second run.
    assert second["steps"] == first["steps"] and second["completed"] == first["completed"]


def test_skipped_when_step_without_its_param(tmp_path, monkeypatch):
    home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "apply", "warden-only"])
    assert "skipped (no --set workspace)" in res.output


def test_missing_required_param_refuses_before_any_write(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "apply", "bring-your-own", "--yes"])
    assert res.exit_code == 2 and "needs --set efficient_model=VALUE" in res.output
    assert snapshot(h) == before


def test_unknown_set_key_refuses(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "apply", "warden-only", "--set", "zzz=1", "--yes"])
    assert res.exit_code == 2 and "recipe 'warden-only' has no param 'zzz'" in res.output
    assert snapshot(h) == before


def test_bad_routing_mode_refuses_before_any_write(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    before = snapshot(h)
    res = runner.invoke(
        app, ["recipe", "apply", "router-and-routing-mode", "--set", "routing_mode=zzz", "--yes"]
    )
    assert res.exit_code == 2 and "unknown routing_mode 'zzz'" in res.output
    assert snapshot(h) == before


def test_revert_restores_the_earlier_config_value(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    cfg = h / ".opendaisugi" / "config.yaml"
    cfg.parent.mkdir(parents=True)
    cfg.write_text("routing_mode: knn\n")
    assert (
        runner.invoke(app, ["recipe", "apply", "router-and-routing-mode", "--yes"]).exit_code == 0
    )
    assert "routing_mode: rules" in cfg.read_text()
    res = runner.invoke(app, ["recipe", "revert", "router-and-routing-mode", "--yes"])
    assert res.exit_code == 0, res.output
    assert "routing_mode: knn" in cfg.read_text()
    assert not (h / ".opendaisugi" / "recipes" / "router-and-routing-mode.json").exists()
    assert "ANTHROPIC_BASE_URL" not in (h / ".claude" / "settings.json").read_text()


def test_revert_dry_run_writes_nothing(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"])
    before = snapshot(h)
    res = runner.invoke(app, ["recipe", "revert", "warden-only"])
    assert res.exit_code == 0 and "Dry run. Add --yes to revert." in res.output
    assert snapshot(h) == before


def test_revert_removes_the_envelope_and_the_hook(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    runner.invoke(app, ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"])
    res = runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"])
    assert res.exit_code == 0, res.output
    assert not (h / ".opendaisugi" / "warden" / "envelopes" / "default.json").exists()
    assert "warden" not in (h / ".claude" / "settings.json").read_text()


def test_revert_without_receipt_is_exit_1(tmp_path, monkeypatch):
    home(tmp_path, monkeypatch)
    res = runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"])
    assert res.exit_code == 1 and "nothing to revert" in res.output


# --- Fix round 1: a revert undoes only what the recipe added ---------------


def _old_enforce_hook(h):
    """An enforce hook in the older module form, as an earlier install wrote it."""
    from opendaisugi.warden import warden_settings_json

    entry = json.loads(
        warden_settings_json(mode="enforce", fmt="claude", root=h / ".opendaisugi" / "warden")
    )["hooks"]["PreToolUse"][0]
    cmd = entry["hooks"][0]["command"]
    old = cmd.replace(" -I -m opendaisugi.warden_client", " -m opendaisugi.warden")
    assert "warden_client" not in old
    entry["hooks"][0]["command"] = old
    settings = h / ".claude" / "settings.json"
    settings.write_text(json.dumps({"hooks": {"PreToolUse": [entry]}}))
    return settings


def test_revert_keeps_an_enforce_hook_the_recipe_did_not_add(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    settings = _old_enforce_hook(h)
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, argv).exit_code == 0
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert receipt["steps"][1]["undo"] is None
    res = runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"])
    assert res.exit_code == 0, res.output
    assert "--mode enforce" in settings.read_text()


def test_a_router_recipe_leaves_a_preset_base_url_alone(tmp_path, monkeypatch):
    from opendaisugi.install import DEFAULT_ROUTER_BASE_URL

    h = home(tmp_path, monkeypatch)
    (h / ".codex").mkdir()
    settings = h / ".claude" / "settings.json"
    settings.write_text(json.dumps({"env": {"ANTHROPIC_BASE_URL": DEFAULT_ROUTER_BASE_URL}}))
    argv = ["recipe", "apply", "router-and-routing-mode", "--yes"]
    assert runner.invoke(app, argv).exit_code == 0
    receipt = json.loads(
        (h / ".opendaisugi" / "recipes" / "router-and-routing-mode.json").read_text()
    )
    assert receipt["steps"][0]["undo"] == {"uninstall": {"codex": ["router"]}}
    assert (
        runner.invoke(app, ["recipe", "revert", "router-and-routing-mode", "--yes"]).exit_code == 0
    )
    assert "ANTHROPIC_BASE_URL" in settings.read_text()
    assert "opendaisugi" not in (h / ".codex" / "config.toml").read_text()


def test_the_receipt_names_the_runtimes_this_run_added(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, argv).exit_code == 0
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert receipt["steps"][1]["undo"] == {"uninstall": {"claude": ["warden"]}}
    res = runner.invoke(app, ["recipe", "revert", "warden-only"])
    assert "uninstall warden from claude (added by this recipe)" in res.output


def test_a_refused_uninstall_is_shown_and_the_receipt_is_kept(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, argv).exit_code == 0
    settings = h / ".claude" / "settings.json"
    data = json.loads(settings.read_text())
    data["hooks"]["PreToolUse"].append(
        {"hooks": [{"type": "command", "command": 'python -m opendaisugi.warden "unterminated'}]}
    )
    settings.write_text(json.dumps(data))
    res = runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"])
    assert res.exit_code == 1, res.output
    assert "not undone" in res.output and "unterminated" in res.output
    assert "Reverted" not in res.output
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert [s["index"] for s in receipt["steps"]] == [2]
    # The envelope step was undone and is not kept in the receipt.
    assert not (h / ".opendaisugi" / "warden" / "envelopes" / "default.json").exists()


def test_a_skipped_step_carries_its_earlier_undo(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    first = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, first).exit_code == 0
    assert runner.invoke(app, ["recipe", "apply", "warden-only", "--yes"]).exit_code == 0
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert [s["index"] for s in receipt["steps"]] == [1, 2, 3]
    assert receipt["steps"][0]["undo"] == {"remove_envelope": "default"}
    assert runner.invoke(app, ["recipe", "revert", "warden-only", "--yes"]).exit_code == 0
    assert not (h / ".opendaisugi" / "warden" / "envelopes" / "default.json").exists()


def test_a_failing_rerun_does_not_truncate_the_receipt(tmp_path, monkeypatch):
    from opendaisugi import recipes

    h = home(tmp_path, monkeypatch)
    first = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, first).exit_code == 0
    real = recipes._run_step

    def boom(raw, step, params, *, home, index):
        if index == 2:
            raise RuntimeError("boom")
        return real(raw, step, params, home=home, index=index)

    monkeypatch.setattr(recipes, "_run_step", boom)
    res = runner.invoke(app, first)
    assert res.exit_code == 1
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert [s["index"] for s in receipt["steps"]] == [1, 2, 3]
    assert receipt["steps"][1]["undo"] == {"uninstall": {"claude": ["warden"]}}
    assert receipt["completed"] == 1


def test_apply_moves_old_state_like_install(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    old = h / ".opendaisugi" / "gate"  # names-ok
    old.mkdir(parents=True)
    (old / "marker").write_text("x")
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    assert runner.invoke(app, argv).exit_code == 0
    assert (h / ".opendaisugi" / "warden" / "marker").exists()


# --- Fix round 2 ----------------------------------------------------------


def test_recipe_apply_after_a_full_install_keeps_the_hook(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    assert runner.invoke(app, ["install", "--yes"]).exit_code == 0
    argv = ["recipe", "apply", "warden-only", "--set", f"workspace={h}", "--yes"]
    res = runner.invoke(app, argv)
    assert res.exit_code == 0, res.output
    receipt = json.loads((h / ".opendaisugi" / "recipes" / "warden-only.json").read_text())
    assert receipt["steps"][1]["undo"] is None


def test_layer_present_survives_a_skill_symlink(tmp_path, monkeypatch):
    from opendaisugi.install import ClaudeCodeRuntime, Layer, install, layer_present

    h = home(tmp_path, monkeypatch)
    install(home=h, yes=True, layers={Layer.SKILL})
    assert layer_present(ClaudeCodeRuntime(), h, Layer.WARDEN) is False
    assert layer_present(ClaudeCodeRuntime(), h, Layer.SKILL) is True


def test_an_old_format_receipt_is_refused_not_read(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    path = h / ".opendaisugi" / "recipes" / "warden-only.json"
    path.parent.mkdir(parents=True)
    old = {
        "recipe": "warden-only",
        "applied_at": "2026-10-31T00:00:00Z",
        "params": {},
        "completed": 3,
        "steps": [
            {"index": 2, "kind": "install", "undo": {"uninstall_only": ["warden"], "runtime": []}}
        ],
    }
    path.write_text(json.dumps(old))
    for argv in (["recipe", "revert", "warden-only"], ["recipe", "revert", "warden-only", "--yes"]):
        res = runner.invoke(app, argv)
        assert res.exit_code == 2, res.output
        assert "does not read" in res.output
    assert json.loads(path.read_text()) == old


def test_a_rerun_config_step_keeps_the_first_restore(tmp_path, monkeypatch):
    h = home(tmp_path, monkeypatch)
    cfg = h / ".opendaisugi" / "config.yaml"
    cfg.parent.mkdir(parents=True)
    cfg.write_text("routing_mode: rules\n")
    for mode in ("knn", "off"):
        argv = [
            "recipe",
            "apply",
            "router-and-routing-mode",
            "--set",
            f"routing_mode={mode}",
            "--yes",
        ]
        assert runner.invoke(app, argv).exit_code == 0
    assert (
        runner.invoke(app, ["recipe", "revert", "router-and-routing-mode", "--yes"]).exit_code == 0
    )
    assert "routing_mode: rules" in cfg.read_text()

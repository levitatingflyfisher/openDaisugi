"""The pieces view: `daisugi modules --pieces` says which of the six pieces run here."""

from __future__ import annotations

import json

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import Config, save_config
from opendaisugi.modules import detect_pieces

runner = CliRunner()


def test_pieces_say_on_or_off_and_the_command_for_each(tmp_path) -> None:
    home = tmp_path / "home"
    home.mkdir()
    pieces = {p.key: p for p in detect_pieces(tmp_path, home=home, which=lambda _n: None, env={})}
    assert list(pieces) == ["gate", "gateway", "router", "memory", "floor", "loop"]
    assert {p.state for p in pieces.values() if p.key != "router"} == {"off"}
    assert pieces["router"].state == "off"  # no gateway, so no router
    assert pieces["gate"].turn_on == "daisugi install --gate"
    assert pieces["router"].turn_on.endswith("--router rules|switchyard|knn|model")


def test_pieces_see_a_gate_hook_and_an_mcp_entry(tmp_path) -> None:
    home = tmp_path / "home"
    (home / ".claude").mkdir(parents=True)
    (home / ".claude" / "settings.json").write_text(
        json.dumps(
            {"hooks": {"PreToolUse": [{"hooks": [{"command": "daisugi hook --mode audit"}]}]}}
        )
    )
    (home / ".claude.json").write_text(json.dumps({"mcpServers": {"opendaisugi": {}}}))
    save_config(Config(matcher_model="lexical", gateway_router="off"), tmp_path / "config.yaml")
    pieces = {p.key: p for p in detect_pieces(tmp_path, home=home, which=lambda _n: "/x", env={})}
    assert pieces["gate"].state == "on" and "claude-code" in pieces["gate"].detail
    assert pieces["memory"].state == "on"
    assert pieces["router"].state == "off"
    assert pieces["loop"].state == "on"


def test_modules_pieces_is_read_only_and_prints_six_blocks(tmp_path) -> None:
    res = runner.invoke(app, ["modules", "--pieces", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert res.output.count("  on:   ") == 6 and res.output.count("  off:  ") == 6
    res = runner.invoke(app, ["modules", "--pieces", "--json", "--data-dir", str(tmp_path)])
    assert [p["key"] for p in json.loads(res.output)] == [
        "gate",
        "gateway",
        "router",
        "memory",
        "floor",
        "loop",
    ]

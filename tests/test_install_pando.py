"""The pando layer of `daisugi install`: the Python twin of Go's pandolayer_test.go.

Pando R2 is there in every harness with no setup step: its MCP server, hooks
that log a Claude Code chat and give a new chat the view, a Hermes plugin that
gives the view in every turn, and the follow service.
"""

from __future__ import annotations

import json

import pytest
import yaml

from opendaisugi import install as inst
from opendaisugi.install import (
    PANDO_UNIT_NAME,
    ClaudeCodeRuntime,
    CodexRuntime,
    HermesRuntime,
    Layer,
    install,
    pando_unit_path,
    uninstall,
)

ONLY = {Layer.PANDO}


@pytest.fixture(autouse=True)
def _no_hook_and_no_hermes_home(monkeypatch):
    monkeypatch.setattr(inst, "UNIT_HOOK", None)
    monkeypatch.delenv("HERMES_HOME", raising=False)
    monkeypatch.setattr(inst, "daisugi_exe", lambda: "/opt/bin/daisugi")


@pytest.fixture
def home(tmp_path):
    for d in (".claude", ".hermes", ".hermes/profiles/household"):
        (tmp_path / d).mkdir(parents=True)
    (tmp_path / ".hermes/profiles/household/config.yaml").write_text("model: x\n")
    (tmp_path / ".claude/settings.json").write_text('{"permissions":{"allow":["Bash"]}}')
    return tmp_path


def _hook(settings: str, ev: str) -> dict:
    for e in json.loads(settings)["hooks"][ev]:
        for h in e["hooks"]:
            if h["command"].endswith(" pando hook"):
                return h
    raise AssertionError(f"no pando hook for {ev}")


def test_claude_gets_the_mcp_server_four_hooks_and_keeps_the_rest(home):
    install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    mcp = (home / ".claude.json").read_text()
    assert '"pando"' in mcp and '"serve"' in mcp and "opendaisugi" not in mcp
    settings = (home / ".claude/settings.json").read_text()
    for ev in ("SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"):
        assert ev in settings
    assert settings.count("/opt/bin/daisugi pando hook") == 4
    assert '"permissions"' in settings
    assert pando_unit_path(home).exists()
    # Idempotent: a second run changes nothing.
    assert install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY).modified_files == []
    # Reverse takes out the pando entries and nothing else.
    uninstall(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    settings = (home / ".claude/settings.json").read_text()
    assert "pando" not in settings and '"permissions"' in settings
    assert "pando" not in (home / ".claude.json").read_text()
    assert not pando_unit_path(home).exists()


def test_logging_hooks_are_async_and_the_view_hook_is_not(home):
    install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    settings = (home / ".claude/settings.json").read_text()
    assert "async" not in _hook(settings, "SessionStart")
    for ev in ("UserPromptSubmit", "Stop"):
        assert _hook(settings, ev)["async"] is True
    # The end of the chat is waited for, so the last reply is not lost at exit.
    assert "async" not in _hook(settings, "SessionEnd")
    assert _hook(settings, "SessionEnd")["timeout"] == 10


def test_session_end_of_the_first_build_is_brought_to_the_foreground(home):
    old = {
        "hooks": {
            "SessionEnd": [
                {
                    "hooks": [
                        {
                            "type": "command",
                            "command": "/opt/bin/daisugi pando hook",
                            "timeout": 10,
                            "async": True,
                        }
                    ]
                }
            ]
        }
    }
    (home / ".claude/settings.json").write_text(json.dumps(old))
    install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    settings = (home / ".claude/settings.json").read_text()
    assert "async" not in _hook(settings, "SessionEnd")
    assert settings.count(" pando hook") == 4


def test_hooks_of_the_first_build_are_brought_up(home):
    old = {
        "hooks": {
            "Stop": [
                {
                    "hooks": [
                        {"type": "command", "command": "/opt/bin/daisugi pando hook", "timeout": 10}
                    ]
                }
            ],
            "SessionStart": [
                {
                    "hooks": [
                        {
                            "type": "command",
                            "command": "/opt/bin/daisugi pando hook",
                            "timeout": 10,
                            "async": True,
                        }
                    ]
                }
            ],
        }
    }
    (home / ".claude/settings.json").write_text(json.dumps(old))
    install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    settings = (home / ".claude/settings.json").read_text()
    assert _hook(settings, "Stop")["async"] is True
    assert "async" not in _hook(settings, "SessionStart")
    assert settings.count(" pando hook") == 4


def test_hermes_covers_every_profile_asks_every_turn_and_is_idempotent(home):
    install(home=home, runtimes=[HermesRuntime()], layers=ONLY)
    for d in (".hermes", ".hermes/profiles/household"):
        cfg = yaml.safe_load((home / d / "config.yaml").read_text())
        assert "pando" in cfg["mcp_servers"] and cfg["plugins"]["enabled"] == ["pando"]
        py = (home / d / "plugins/pando/__init__.py").read_text()
        assert '"/opt/bin/daisugi"' in py and "pre_llm_call" in py
        assert "if not is_first_turn" not in py, "the plugin skips later turns"
        assert (home / d / "plugins/pando/plugin.yaml").is_file()
    assert "model: x" in (home / ".hermes/profiles/household/config.yaml").read_text()
    assert install(home=home, runtimes=[HermesRuntime()], layers=ONLY).modified_files == []
    uninstall(home=home, runtimes=[HermesRuntime()], layers=ONLY)
    assert not (home / ".hermes/plugins/pando").exists()
    assert "pando" not in (home / ".hermes/profiles/household/config.yaml").read_text()


def test_the_codex_block_is_found_by_line_and_removed_with_its_sub_tables(tmp_path):
    codex = tmp_path / ".codex"
    codex.mkdir()
    p = codex / "config.toml"
    # A comment that names the table is not the table.
    p.write_text('# add [mcp_servers.pando] later\nmodel = "x"\n')
    install(home=tmp_path, runtimes=[CodexRuntime()], layers=ONLY)
    assert p.read_text().count("\n[mcp_servers.pando]\n") == 1
    p.write_text(
        'model = "x"\n\n[mcp_servers.pando]\ncommand = "d"\n\n'
        '[mcp_servers.pando.env]\nA = "1"\n\n[[profiles]]\nname = "p"\n'
    )
    uninstall(home=tmp_path, runtimes=[CodexRuntime()], layers=ONLY)
    text = p.read_text()
    assert "pando" not in text and "[[profiles]]" in text and 'model = "x"' in text


def test_the_hermes_dir_follows_hermes_home(tmp_path, monkeypatch):
    elsewhere = tmp_path / "hh"
    elsewhere.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(elsewhere))
    assert inst._hermes_homes(tmp_path / "nohome")[0] == elsewhere


def test_the_owners_own_unit_is_backed_up_and_never_removed(home):
    p = pando_unit_path(home)
    p.parent.mkdir(parents=True)
    p.write_text("[Service]\nExecStart=/my/own\n")
    install(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    baks = list(p.parent.glob(PANDO_UNIT_NAME + ".bak*"))
    assert len(baks) == 1 and "/my/own" in baks[0].read_text()
    p.write_text("[Service]\nExecStart=/my/own\n")
    uninstall(home=home, runtimes=[ClaudeCodeRuntime()], layers=ONLY)
    assert p.exists(), "uninstall removed a unit that is not ours"


def test_the_unit_hook_enables_on_install_and_disables_on_uninstall(home, monkeypatch):
    calls: list[str] = []
    monkeypatch.setattr(inst, "UNIT_HOOK", lambda a, u: calls.append(f"{a} {u}"))
    rts = [ClaudeCodeRuntime(), HermesRuntime()]
    install(home=home, runtimes=rts, layers=ONLY)
    # Claude Code and Hermes share one unit: the service starts once.
    assert calls == [f"enable {PANDO_UNIT_NAME}"]
    calls.clear()
    uninstall(home=home, runtimes=rts, layers=ONLY)
    assert calls == [f"disable {PANDO_UNIT_NAME}", f"reload {PANDO_UNIT_NAME}"]


def test_the_dry_run_says_it_enables_and_starts_the_service_once(home):
    result = install(
        home=home, dry_run=True, runtimes=[ClaudeCodeRuntime(), HermesRuntime()], layers=ONLY
    )
    units = [s for s in result.planned if str(s.target).endswith(PANDO_UNIT_NAME)]
    assert units and all("enable and start it" in s.description for s in units)
    assert not pando_unit_path(home).exists()


def test_pando_is_a_component_so_only_can_name_it():
    assert inst.component_layers(["pando"]) == {Layer.PANDO}
    assert "pando" in inst.list_components()
    assert Layer.PANDO in inst.DEFAULT_LAYERS
    assert inst.component_names({Layer.PANDO}) == ["pando"]

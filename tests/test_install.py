"""daisugi install — plug-and-play runtime configuration.

Tests cover:
- Runtime detection (Claude Code, Hermes, Codex)
- Dry-run mode (no writes)
- Claude Code settings.json patching (idempotent)
- Claude Code CLAUDE.md appending (idempotent)
- Hermes cli-config.yaml patching (idempotent)
- --yes skips confirmation prompt
- Backup of existing configs before modification
"""
from __future__ import annotations

import json
import shutil
from pathlib import Path
from unittest import mock

import pytest
import yaml

from opendaisugi.install import (
    ClaudeCodeRuntime,
    HermesRuntime,
    detect_runtimes,
    install,
    InstallResult,
)


# ---------------------------------------------------------------------------
# Detection
# ---------------------------------------------------------------------------

def test_detect_claude_code_by_dir(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    runtimes = detect_runtimes(home=tmp_path)
    assert any(isinstance(r, ClaudeCodeRuntime) for r in runtimes)


def test_detect_claude_code_not_present(tmp_path):
    runtimes = detect_runtimes(home=tmp_path)
    assert not any(isinstance(r, ClaudeCodeRuntime) for r in runtimes)


def test_detect_hermes_by_dir(tmp_path):
    hermes_dir = tmp_path / ".hermes"
    hermes_dir.mkdir()
    runtimes = detect_runtimes(home=tmp_path)
    assert any(isinstance(r, HermesRuntime) for r in runtimes)


def test_detect_returns_empty_when_nothing_present(tmp_path):
    assert detect_runtimes(home=tmp_path) == []


# ---------------------------------------------------------------------------
# InstallPlan / dry-run
# ---------------------------------------------------------------------------

def test_dry_run_writes_nothing(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    result = install(home=tmp_path, dry_run=True, yes=True)
    assert result.dry_run is True
    # No files written
    assert not (claude_dir / "settings.json").exists()
    assert not (tmp_path / ".claude" / "CLAUDE.md").exists()


def test_dry_run_still_reports_planned_changes(tmp_path):
    (tmp_path / ".claude").mkdir()
    result = install(home=tmp_path, dry_run=True, yes=True)
    assert result.planned  # non-empty list of planned actions


# ---------------------------------------------------------------------------
# Claude Code — settings.json
# ---------------------------------------------------------------------------

def test_install_creates_settings_json(tmp_path):
    # MCP lands in ~/.claude.json (the file Claude Code actually reads), not settings.json.
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert "opendaisugi" in cj.get("mcpServers", {})


def test_install_adds_pretooluse_hook(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    hooks = settings.get("hooks", {})
    pre = hooks.get("PreToolUse", [])
    commands = [
        h["command"]
        for entry in pre
        for h in entry.get("hooks", [])
        if h.get("type") == "command"
    ]
    assert any("daisugi" in c and "hook" in c for c in commands)


def test_install_no_longer_adds_session_start_hook(tmp_path):
    # v0.28.0: the skill is discovered on demand; SessionStart is not added.
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    assert "SessionStart" not in settings.get("hooks", {})


def test_install_idempotent_settings(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    install(home=tmp_path, yes=True)
    # MCP server should appear exactly once in ~/.claude.json
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert list(cj["mcpServers"].keys()).count("opendaisugi") == 1
    # PreToolUse hooks (in settings.json) should not be duplicated
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    pre = settings["hooks"]["PreToolUse"]
    daisugi_hooks = [
        h
        for entry in pre
        for h in entry.get("hooks", [])
        if "daisugi" in h.get("command", "")
    ]
    assert len(daisugi_hooks) == len(set(h["command"] for h in daisugi_hooks))


def test_install_merges_into_existing_settings(tmp_path):
    # Existing mcpServers in ~/.claude.json must be preserved alongside ours.
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    (tmp_path / ".claude.json").write_text(json.dumps(
        {"mcpServers": {"other-tool": {"command": "other"}}, "someKey": True}
    ))
    install(home=tmp_path, yes=True)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert cj["someKey"] is True                # unrelated user data preserved
    assert "other-tool" in cj["mcpServers"]     # existing preserved
    assert "opendaisugi" in cj["mcpServers"]    # new added


def test_install_backs_up_existing_settings(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    (claude_dir / "settings.json").write_text('{"existing": true}')
    install(home=tmp_path, yes=True)
    backups = list(claude_dir.glob("settings.json.bak*"))
    assert backups, "should create a backup before modifying"


# ---------------------------------------------------------------------------
# Claude Code — CLAUDE.md
# ---------------------------------------------------------------------------

def test_install_appends_to_claude_md(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    md = (tmp_path / ".claude" / "CLAUDE.md").read_text()
    assert "find_pathway" in md
    assert "opendaisugi" in md.lower()


def test_install_does_not_duplicate_claude_md_block(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    install(home=tmp_path, yes=True)
    md = (tmp_path / ".claude" / "CLAUDE.md").read_text()
    assert md.count("find_pathway") == 1


def test_install_preserves_existing_claude_md(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    (claude_dir / "CLAUDE.md").write_text("# My existing instructions\n\nDo stuff.\n")
    install(home=tmp_path, yes=True)
    md = (claude_dir / "CLAUDE.md").read_text()
    assert "My existing instructions" in md
    assert "find_pathway" in md


# ---------------------------------------------------------------------------
# Hermes
# ---------------------------------------------------------------------------

def test_install_hermes_adds_hook(tmp_path):
    # v0.28.0: Hermes reads config.yaml (cli-config.yaml was a no-op bug).
    (tmp_path / ".hermes").mkdir()
    (tmp_path / ".hermes" / "config.yaml").write_text("hooks:\n  pre_tool_call: []\n")
    install(home=tmp_path, yes=True)
    cfg = yaml.safe_load((tmp_path / ".hermes" / "config.yaml").read_text())
    commands = [h.get("command", "") for h in cfg["hooks"]["pre_tool_call"]]
    assert any("daisugi" in c for c in commands)


def test_install_hermes_idempotent(tmp_path):
    (tmp_path / ".hermes").mkdir()
    (tmp_path / ".hermes" / "config.yaml").write_text("hooks:\n  pre_tool_call: []\n")
    install(home=tmp_path, yes=True)
    install(home=tmp_path, yes=True)
    cfg = yaml.safe_load((tmp_path / ".hermes" / "config.yaml").read_text())
    daisugi_hooks = [h for h in cfg["hooks"]["pre_tool_call"] if "daisugi" in h.get("command", "")]
    assert len(daisugi_hooks) == 1


# ---------------------------------------------------------------------------
# InstallResult
# ---------------------------------------------------------------------------

def test_install_result_lists_modified_files(tmp_path):
    (tmp_path / ".claude").mkdir()
    result = install(home=tmp_path, yes=True)
    assert result.modified_files
    assert any("settings.json" in str(f) for f in result.modified_files)


def test_install_result_no_runtimes_detected(tmp_path):
    result = install(home=tmp_path, yes=True)
    assert result.modified_files == []
    assert "no supported" in result.summary.lower()


# --- v0.28.0 universal install ------------------------------------------------

from opendaisugi.install import InstallStep, Layer


def test_install_step_carries_layer_and_target(tmp_path):
    step = InstallStep(
        layer=Layer.SKILL,
        description="Symlink opendaisugi-checklist skill",
        target=tmp_path / "skills",
    )
    assert step.layer is Layer.SKILL
    assert "skill" in step.description.lower()
    assert step.target == tmp_path / "skills"


# --- Task 5: ClaudeCodeRuntime revised ---------------------------------------

from opendaisugi.install import ClaudeCodeRuntime


def test_claude_install_symlinks_skill_to_agents_dir(tmp_path):
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path)
    skill = tmp_path / ".agents" / "skills" / "opendaisugi-checklist"
    assert skill.exists()
    assert (skill / "SKILL.md").is_file()


def test_claude_install_does_not_add_session_start(tmp_path):
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path)
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    assert "SessionStart" not in settings.get("hooks", {})


def test_claude_install_migrates_out_old_session_start(tmp_path):
    claude = tmp_path / ".claude"
    claude.mkdir()
    (claude / "settings.json").write_text(json.dumps({
        "hooks": {"SessionStart": [
            {"hooks": [{"type": "command", "command": "daisugi install --print-skill"}]}
        ]}
    }))
    ClaudeCodeRuntime().apply(tmp_path)
    settings = json.loads((claude / "settings.json").read_text())
    ss = settings.get("hooks", {}).get("SessionStart", [])
    cmds = [h["command"] for e in ss for h in e.get("hooks", [])]
    assert "daisugi install --print-skill" not in cmds


def test_claude_install_keeps_mcp_and_pretooluse(tmp_path):
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert "opendaisugi" in cj["mcpServers"]   # MCP in the file Claude reads
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    pre = settings["hooks"]["PreToolUse"]       # hook stays in settings.json
    cmds = [h["command"] for e in pre for h in e.get("hooks", [])]
    assert any("daisugi hook record" in c for c in cmds)


# --- Task 9: shared instruction-file writer ----------------------------------

from opendaisugi.install import _patch_instructions, _CLAUDE_MD_MARKER


def test_patch_instructions_appends_once(tmp_path):
    p = tmp_path / "AGENTS.md"
    p.write_text("# My rules\n")
    _patch_instructions(p)
    _patch_instructions(p)  # idempotent
    text = p.read_text()
    assert text.count(_CLAUDE_MD_MARKER) == 2  # one open, one close marker, single block
    assert "# My rules" in text  # preserved
    assert "find_pathway" in text


def test_patch_instructions_creates_file(tmp_path):
    p = tmp_path / "AGENTS.md"
    assert _patch_instructions(p) == [p]
    assert "opendaisugi" in p.read_text().lower()


# --- Task 6: CodexRuntime -----------------------------------------------------

from opendaisugi.install import CodexRuntime


def test_codex_install_symlinks_skill(tmp_path):
    (tmp_path / ".codex").mkdir()
    CodexRuntime().apply(tmp_path)
    skill = tmp_path / ".agents" / "skills" / "opendaisugi-checklist"
    assert (skill / "SKILL.md").is_file()


def test_codex_install_registers_mcp_in_toml(tmp_path):
    (tmp_path / ".codex").mkdir()
    CodexRuntime().apply(tmp_path)
    toml_text = (tmp_path / ".codex" / "config.toml").read_text()
    assert "mcp_servers.opendaisugi" in toml_text
    assert 'command = "daisugi"' in toml_text


def test_codex_install_is_idempotent(tmp_path):
    (tmp_path / ".codex").mkdir()
    CodexRuntime().apply(tmp_path)
    before = (tmp_path / ".codex" / "config.toml").read_text()
    CodexRuntime().apply(tmp_path)
    after = (tmp_path / ".codex" / "config.toml").read_text()
    assert before == after


# --- Task 7: HermesRuntime (config.yaml, skill+mcp+hook) ----------------------

def _hermes_cfg(home):
    return yaml.safe_load((home / ".hermes" / "config.yaml").read_text())


def test_hermes_writes_config_yaml_not_cli_config(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    assert (tmp_path / ".hermes" / "config.yaml").exists()
    assert not (tmp_path / ".hermes" / "cli-config.yaml").exists()


def test_hermes_registers_mcp_and_hook(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    cfg = _hermes_cfg(tmp_path)
    assert "opendaisugi" in cfg["mcp_servers"]
    pre = cfg["hooks"]["pre_tool_call"]
    assert any("daisugi hook record --format hermes" in h["command"] for h in pre)


def test_hermes_symlinks_skill(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    skill = tmp_path / ".hermes" / "skills" / "opendaisugi" / "opendaisugi-checklist"
    assert (skill / "SKILL.md").is_file()


def test_hermes_idempotent_v028(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    first = _hermes_cfg(tmp_path)
    HermesRuntime().apply(tmp_path)
    assert _hermes_cfg(tmp_path) == first


# --- Task 8: OpenClawRuntime --------------------------------------------------

from opendaisugi.install import OpenClawRuntime


def test_openclaw_symlinks_skill_into_workspace(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path)
    skill = tmp_path / ".openclaw" / "workspace" / "skills" / "opendaisugi-checklist"
    assert (skill / "SKILL.md").is_file()


def test_openclaw_registers_mcp_in_json(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path)
    cfg = json.loads((tmp_path / ".openclaw" / "openclaw.json").read_text())
    assert cfg["mcp"]["servers"]["opendaisugi"]["command"] == "daisugi"


def test_openclaw_preserves_existing_keys(tmp_path):
    oc = tmp_path / ".openclaw"
    (oc / "workspace").mkdir(parents=True)
    (oc / "openclaw.json").write_text(json.dumps({"agents": {"defaults": {"workspace": "~/x"}}}))
    OpenClawRuntime().apply(tmp_path)
    cfg = json.loads((oc / "openclaw.json").read_text())
    assert cfg["agents"]["defaults"]["workspace"] == "~/x"  # untouched
    assert "opendaisugi" in cfg["mcp"]["servers"]


def test_openclaw_detect_requires_dir(tmp_path):
    assert OpenClawRuntime().detect(tmp_path) is False
    (tmp_path / ".openclaw").mkdir()
    assert OpenClawRuntime().detect(tmp_path) is True


# --- Task 10: OpenClaw before_tool_call plugin -------------------------------

def test_openclaw_plugin_materialized(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path)
    plugin = tmp_path / ".openclaw" / "extensions" / "opendaisugi"
    assert (plugin / "index.mjs").is_file()
    manifest = json.loads((plugin / "openclaw.plugin.json").read_text())
    assert manifest["id"] == "opendaisugi"
    body = (plugin / "index.mjs").read_text()
    assert "before_tool_call" in body
    assert "daisugi" in body and "hook" in body


# --- Task 11: layer-aware orchestration / dry-run -----------------------------

def test_dry_run_reports_all_layers_no_writes(tmp_path):
    (tmp_path / ".claude").mkdir()
    res = install(home=tmp_path, dry_run=True, runtimes=[ClaudeCodeRuntime()])
    assert res.dry_run is True
    assert res.modified_files == []
    layers = {s.layer for s in res.planned}
    assert Layer.SKILL in layers and Layer.MCP in layers
    assert not (tmp_path / ".agents").exists()  # nothing written


# --- Task 12: uninstall -------------------------------------------------------

from opendaisugi.install import uninstall


def test_uninstall_removes_skill_and_mcp(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()])
    assert (tmp_path / ".agents" / "skills" / "opendaisugi-checklist").exists()

    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    assert not (tmp_path / ".agents" / "skills" / "opendaisugi-checklist").exists()
    # MCP removed from ~/.claude.json (the file Claude actually reads)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert "opendaisugi" not in cj.get("mcpServers", {})


def test_uninstall_preserves_unmanaged_content(tmp_path):
    claude = tmp_path / ".claude"
    claude.mkdir()
    (claude / "CLAUDE.md").write_text("# My own rules\n")
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()])
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    assert "# My own rules" in (claude / "CLAUDE.md").read_text()
    assert _CLAUDE_MD_MARKER not in (claude / "CLAUDE.md").read_text()


# --- Task 16: combined four-harness install -----------------------------------

def test_combined_install_all_harnesses(tmp_path):
    for d in (".claude", ".codex", ".hermes", ".openclaw/workspace"):
        (tmp_path / d).mkdir(parents=True)
    rts = detect_runtimes(home=tmp_path)
    names = {r.name for r in rts}
    assert {"Claude Code", "Codex", "Hermes", "OpenClaw"} <= names
    res = install(home=tmp_path, yes=True, runtimes=rts)
    assert res.modified_files
    # every harness got a skill. Claude + Codex both target the cross-vendor
    # path so it's shared here — assert it directly (the old `or` short-circuited
    # and never actually checked the second arm).
    assert (tmp_path / ".agents" / "skills" / "opendaisugi-checklist" / "SKILL.md").is_file()
    assert (tmp_path / ".hermes" / "skills" / "opendaisugi" / "opendaisugi-checklist" / "SKILL.md").is_file()
    assert (tmp_path / ".openclaw" / "workspace" / "skills" / "opendaisugi-checklist" / "SKILL.md").is_file()


# --- SGCM fix: --runtime exact-key selection (was substring over-select) -------

from opendaisugi.install import _select_runtimes
import pytest as _pytest


def test_select_runtimes_code_resolves_to_codex_only():
    sel = _select_runtimes(["code"])
    assert {r.name for r in sel} == {"Codex"}  # was {Claude Code, Codex}


def test_select_runtimes_exact_keys():
    assert {r.name for r in _select_runtimes(["claude"])} == {"Claude Code"}
    assert {r.name for r in _select_runtimes(["openclaw"])} == {"OpenClaw"}


def test_select_runtimes_ambiguous_or_unknown_raises():
    with _pytest.raises(ValueError):
        _select_runtimes(["c"])       # ambiguous: claude + codex
    with _pytest.raises(ValueError):
        _select_runtimes(["bogus"])   # matches nothing


# --- SGCM fix: Claude MCP lands in ~/.claude.json (settings.json is dead) ------

def test_claude_mcp_goes_to_claude_json_not_settings(tmp_path):
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert cj["mcpServers"]["opendaisugi"]["command"] == "daisugi"
    assert cj["mcpServers"]["opendaisugi"]["type"] == "stdio"
    settings = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    assert "mcpServers" not in settings              # the dead file must stay clean
    assert "PreToolUse" in settings.get("hooks", {})  # hook stays in settings.json


def test_claude_mcp_preserves_existing_claude_json(tmp_path):
    (tmp_path / ".claude").mkdir()
    (tmp_path / ".claude.json").write_text(json.dumps({
        "mcpServers": {"other": {"command": "x"}},
        "numStartups": 42,
    }))
    ClaudeCodeRuntime().apply(tmp_path)
    cj = json.loads((tmp_path / ".claude.json").read_text())
    assert cj["numStartups"] == 42               # unrelated user data preserved
    assert "other" in cj["mcpServers"]           # existing server preserved
    assert "opendaisugi" in cj["mcpServers"]


# --- SGCM fix: reverse paths are backed-up, change-guarded, no empty scaffolding

def test_reverse_hermes_no_op_when_not_installed(tmp_path):
    (tmp_path / ".hermes").mkdir()
    original = "mcp_servers:\n  other:\n    command: x\n"
    (tmp_path / ".hermes" / "config.yaml").write_text(original)
    HermesRuntime().reverse(tmp_path)
    # untouched byte-for-byte; no empty hooks block injected
    assert (tmp_path / ".hermes" / "config.yaml").read_text() == original


def test_reverse_hermes_backs_up_and_keeps_user_content(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    # add user content alongside ours
    cfg = yaml.safe_load((tmp_path / ".hermes" / "config.yaml").read_text())
    cfg["mcp_servers"]["mine"] = {"command": "z"}
    (tmp_path / ".hermes" / "config.yaml").write_text(yaml.dump(cfg))
    HermesRuntime().reverse(tmp_path)
    out = yaml.safe_load((tmp_path / ".hermes" / "config.yaml").read_text())
    assert "opendaisugi" not in out.get("mcp_servers", {})
    assert "mine" in out["mcp_servers"]  # user server preserved
    assert list((tmp_path / ".hermes").glob("config.yaml.bak*")), "must back up before reverse"


def test_reverse_openclaw_no_op_when_not_installed(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    original = json.dumps({"mcp": {"servers": {"other": {"command": "x"}}}}, indent=2)
    (tmp_path / ".openclaw" / "openclaw.json").write_text(original)
    OpenClawRuntime().reverse(tmp_path)
    out = json.loads((tmp_path / ".openclaw" / "openclaw.json").read_text())
    assert out == {"mcp": {"servers": {"other": {"command": "x"}}}}  # untouched


# --- SGCM fix: uninstall isolates per-runtime failures ------------------------

def test_uninstall_isolates_per_runtime_failures(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()])

    class Boom:
        name = "Boom"
        def reverse(self, home):
            raise RuntimeError("kaboom")

    res = uninstall(home=tmp_path, runtimes=[Boom(), ClaudeCodeRuntime()])
    # Claude still reversed despite Boom failing first in the loop
    assert not (tmp_path / ".agents" / "skills" / "opendaisugi-checklist").exists()
    assert "Boom" in res.summary  # failure surfaced, not swallowed silently


# --- SGCM fix #4: skill removal is path-stable (no orphan on uninstall) --------

from opendaisugi.install import _remove_skill_both


def test_remove_skill_both_clears_vendor_path(tmp_path):
    vendor = tmp_path / ".claude" / "skills" / "opendaisugi-checklist"
    vendor.mkdir(parents=True)
    removed = _remove_skill_both(tmp_path, ".claude/skills")
    assert vendor in removed
    assert not vendor.exists()


def test_uninstall_removes_skill_even_after_agents_dir_appears(tmp_path):
    # Force the per-vendor branch: .claude/skills exists, .agents does not.
    (tmp_path / ".claude" / "skills").mkdir(parents=True)
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()])
    vendor_skill = tmp_path / ".claude" / "skills" / "opendaisugi-checklist"
    assert vendor_skill.exists(), "skill should have landed in the per-vendor path"
    # Now ~/.agents/skills appears (e.g. a later Codex install), flipping the heuristic.
    (tmp_path / ".agents" / "skills").mkdir(parents=True)
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    assert not vendor_skill.exists()  # not orphaned despite the flipped target


# --- SGCM fix: apply-path idempotency (no rewrite/backup on no-op re-run) ------

from opendaisugi.install import _patch_claude_settings


def test_patch_claude_settings_noop_rerun_no_backup(tmp_path):
    sp = tmp_path / "settings.json"
    _patch_claude_settings(sp)          # first: creates
    before = sp.read_text()
    result = _patch_claude_settings(sp)  # second: nothing to change
    assert result == []                  # reports no modification
    assert sp.read_text() == before      # byte-identical
    assert not list(tmp_path.glob("settings.json.bak*"))  # no backup on no-op


def test_patch_claude_settings_preserves_user_session_start(tmp_path):
    sp = tmp_path / "settings.json"
    sp.write_text(json.dumps({"hooks": {"SessionStart": [
        {"hooks": [{"type": "command", "command": "my-own-thing"}]}
    ]}}))
    _patch_claude_settings(sp)
    after1 = sp.read_text()
    _patch_claude_settings(sp)            # idempotent
    assert sp.read_text() == after1       # user's SessionStart not churned
    s = json.loads(sp.read_text())
    cmds = [h["command"] for e in s["hooks"]["SessionStart"] for h in e.get("hooks", [])]
    assert "my-own-thing" in cmds         # user hook preserved


# --- SGCM fix: OpenClaw plugin must time out a stalled daisugi -----------------

def test_openclaw_plugin_has_spawn_timeout(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path)
    body = (tmp_path / ".openclaw" / "extensions" / "opendaisugi" / "index.mjs").read_text()
    assert "setTimeout" in body, "plugin must bound the daisugi subprocess with a timeout"
    assert "kill" in body, "plugin must kill a stalled subprocess and fail open"


# --- SGCM convergence: install isolation + claude.json clobber-safety ----------

from opendaisugi.install import _patch_claude_mcp


def test_install_isolates_per_runtime_apply_failures(tmp_path):
    (tmp_path / ".claude").mkdir()

    class Boom:
        name = "Boom"
        def detect(self, h): return True
        def plan(self, h): return []
        def apply(self, h): raise RuntimeError("boom")

    res = install(home=tmp_path, yes=True, runtimes=[Boom(), ClaudeCodeRuntime()])
    assert (tmp_path / ".claude.json").exists()  # Claude installed despite Boom
    assert "Boom" in res.summary                 # failure surfaced


def test_patch_claude_mcp_does_not_clobber_unparseable_file(tmp_path):
    cj = tmp_path / ".claude.json"
    cj.write_text("{ this is : not valid json,,,")
    result = _patch_claude_mcp(cj)
    assert result == []                          # skipped, not overwritten
    assert "this is" in cj.read_text()           # user's file preserved verbatim
    assert "opendaisugi" not in cj.read_text()


# --- SGCM polish: backup discipline (no .bak spray; collision-proof) -----------

from opendaisugi.install import _backup


def test_hermes_apply_no_bak_on_idempotent_rerun(tmp_path):
    (tmp_path / ".hermes").mkdir()
    HermesRuntime().apply(tmp_path)
    HermesRuntime().apply(tmp_path)  # idempotent
    assert list((tmp_path / ".hermes").glob("config.yaml.bak*")) == []


def test_openclaw_apply_no_bak_on_idempotent_rerun(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path)
    OpenClawRuntime().apply(tmp_path)  # idempotent
    assert list((tmp_path / ".openclaw").glob("openclaw.json.bak*")) == []


def test_backup_is_collision_proof(tmp_path):
    f = tmp_path / "c.json"
    f.write_text("v1")
    _backup(f)
    f.write_text("v2")
    _backup(f)
    baks = sorted(tmp_path.glob("c.json.bak*"))
    assert len(baks) == 2, "two backups in the same second must not overwrite each other"
    assert {b.read_text() for b in baks} == {"v1", "v2"}


# --- SGCM polish: parsing robustness (json5 strings, marker-pair) -------------

from opendaisugi.install import _strip_json5_comments, _unpatch_instructions, _CLAUDE_MD_MARKER as _MARK


def test_strip_json5_comments_preserves_double_slash_in_strings(tmp_path):
    src = '{ "url": "https://example.com/a//b", "x": 1 /* note */ }'
    out = _strip_json5_comments(src)
    d = json.loads(out)
    assert d["url"] == "https://example.com/a//b"  # // inside a string is NOT a comment
    assert d["x"] == 1                              # block comment still stripped


def test_unpatch_instructions_requires_marker_pair(tmp_path):
    p = tmp_path / "AGENTS.md"
    p.write_text(f"# Notes\nI reference {_MARK} once, in prose.\nKeep this line.\n")
    result = _unpatch_instructions(p)
    assert result == []                       # single stray marker → no-op
    assert "Keep this line." in p.read_text()  # file not truncated to EOF


# --- SGCM polish: _remove_dir symlink-safety ---------------------------------

from opendaisugi.install import _remove_dir


def test_remove_dir_is_symlink_safe(tmp_path):
    victim = tmp_path / "victim"
    victim.mkdir()
    (victim / "keep.txt").write_text("precious")
    link = tmp_path / "link"
    link.symlink_to(victim, target_is_directory=True)
    _remove_dir(link)
    assert not link.exists()                       # the link is gone
    assert victim.exists() and (victim / "keep.txt").read_text() == "precious"  # target untouched


# --- v0.28.1: OpenClaw plugin must not write through a pre-planted symlink ----

from opendaisugi.install import _install_openclaw_plugin


def test_openclaw_plugin_does_not_follow_symlink(tmp_path):
    dest = tmp_path / ".openclaw" / "extensions" / "opendaisugi"
    dest.mkdir(parents=True)
    victim = tmp_path / "victim.txt"
    victim.write_text("precious")
    (dest / "index.mjs").symlink_to(victim)  # attacker pre-plants a symlink
    _install_openclaw_plugin(tmp_path)
    assert victim.read_text() == "precious"            # victim NOT overwritten
    assert not (dest / "index.mjs").is_symlink()       # link replaced by real file
    assert "before_tool_call" in (dest / "index.mjs").read_text()


# v0.28.6 — M7: surface JSON5 comment loss when the install wizard
# rewrites a config file. Comments DO disappear (the writer is plain
# JSON); the backup preserves them. The warning makes that visible at
# the moment of loss rather than hiding behind "idempotent, reversible".


def test_patch_mcp_warns_when_clobbering_json5_comments(tmp_path):
    import warnings as _warnings

    from opendaisugi.install import _patch_mcp, _JSON5

    cfg = tmp_path / "openclaw.json"
    cfg.write_text(
        "// top-level note: this file is JSON5\n"
        '{ "mcp": { "servers": {} /* will be patched */ } }\n'
    )

    with _warnings.catch_warnings(record=True) as caught:
        _warnings.simplefilter("always")
        written = _patch_mcp(cfg, _JSON5, ("mcp", "servers"),
                             {"command": "daisugi", "args": ["mcp", "serve"]})

    assert written == [cfg]
    comment_warnings = [w for w in caught if "JSON5 comments" in str(w.message)]
    assert len(comment_warnings) == 1, (
        f"expected one JSON5-comment-loss warning, got {[str(w.message) for w in caught]}"
    )
    # And the comments are in fact gone from the rewritten file (we are
    # honest about it, not silently magic).
    assert "//" not in cfg.read_text()
    assert "/*" not in cfg.read_text()


def test_patch_mcp_no_warning_when_no_comments_to_lose(tmp_path):
    import warnings as _warnings

    from opendaisugi.install import _patch_mcp, _JSON5

    cfg = tmp_path / "openclaw.json"
    cfg.write_text('{ "mcp": { "servers": {} } }\n')  # plain JSON, no comments

    with _warnings.catch_warnings(record=True) as caught:
        _warnings.simplefilter("always")
        _patch_mcp(cfg, _JSON5, ("mcp", "servers"),
                   {"command": "daisugi", "args": ["mcp", "serve"]})

    comment_warnings = [w for w in caught if "JSON5 comments" in str(w.message)]
    assert comment_warnings == [], (
        "no comments to lose → no warning"
    )


def test_patch_claude_settings_does_not_clobber_unparseable_file(tmp_path):
    # H4: a malformed settings.json (common hand-edit: trailing comma) must be
    # LEFT INTACT with a warning — never reset to {} and rewritten (which would
    # destroy the user's permission deny-rules and env).
    import warnings as _w
    from opendaisugi.install import _patch_claude_settings
    sp = tmp_path / "settings.json"
    original = '{"permissions": {"deny": ["Read(./secrets/**)"]}, "env": {"K": "v"},}'  # trailing comma
    sp.write_text(original)
    with _w.catch_warnings(record=True) as caught:
        _w.simplefilter("always")
        result = _patch_claude_settings(sp)
    assert result == []                       # skipped (no files modified)
    assert sp.read_text() == original         # file untouched
    assert any("not valid JSON" in str(x.message) for x in caught)


def test_patch_hermes_config_does_not_clobber_unparseable_file(tmp_path):
    import warnings as _w
    from opendaisugi.install import _patch_hermes_config
    cp = tmp_path / "config.yaml"
    original = "mcp_servers: [\n  - name: x\n   bad_indent: y\n"  # malformed YAML
    cp.write_text(original)
    with _w.catch_warnings(record=True) as caught:
        _w.simplefilter("always")
        result = _patch_hermes_config(cp)
    assert result == []
    assert cp.read_text() == original
    assert any("not valid YAML" in str(x.message) for x in caught)

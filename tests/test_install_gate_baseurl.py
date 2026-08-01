"""Phase 4 (ADR-0013) — the GATE and BASE_URL install layers.

`daisugi install` already wires skill + MCP + capture + instructions per
harness (tests/test_install.py). This file covers the two new opt-in
layers added on top of that:

  - GATE: installs the ADR-0007 fail-closed verify hook (Claude Code only,
    shadow-by-default, `--enforce` an explicit opt-in).
  - BASE_URL: points the harness at the local token-saving gateway
    (Claude Code via env; OpenClaw via a registered provider block).

Both are opt-in (`layers=` must include them explicitly) so the existing
four-layer default install stays byte-identical. Every other (runtime,
layer) combination that ADR-0013 does NOT wire is asserted to surface an
honest reason rather than silently doing nothing.
"""

from __future__ import annotations

import json
import warnings
from pathlib import Path

from opendaisugi.install import (
    DEFAULT_LAYERS,
    ClaudeCodeRuntime,
    CodexRuntime,
    HermesRuntime,
    InstallStep,
    Layer,
    OpenClawRuntime,
    _format_summary,
    install,
    uninstall,
)

GATE_LAYERS = DEFAULT_LAYERS | {Layer.GATE}
BASE_URL_LAYERS = DEFAULT_LAYERS | {Layer.BASE_URL}
BOTH_LAYERS = DEFAULT_LAYERS | {Layer.GATE, Layer.BASE_URL}
GATEWAY_URL = "http://127.0.0.1:8787"


def _settings(home):
    return json.loads((home / ".claude" / "settings.json").read_text())


def _pre_commands(settings: dict) -> list[str]:
    pre = settings.get("hooks", {}).get("PreToolUse", [])
    return [h["command"] for entry in pre for h in entry.get("hooks", [])]


# ---------------------------------------------------------------------------
# Default install is unchanged (back-compat)
# ---------------------------------------------------------------------------


def test_default_install_writes_no_gate_and_no_base_url(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True)
    settings = _settings(tmp_path)
    commands = _pre_commands(settings)
    assert not any("opendaisugi.gate" in c for c in commands)
    assert "ANTHROPIC_BASE_URL" not in settings.get("env", {})


def test_install_layers_none_defaults_to_current_four(tmp_path):
    (tmp_path / ".claude").mkdir()
    res = install(home=tmp_path, dry_run=True, runtimes=[ClaudeCodeRuntime()])
    layers = {s.layer for s in res.planned}
    assert layers == DEFAULT_LAYERS


# ---------------------------------------------------------------------------
# Claude Code — GATE
# ---------------------------------------------------------------------------


def test_claude_gate_shadow_is_the_default(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    commands = _pre_commands(_settings(tmp_path))
    gate_cmds = [c for c in commands if "opendaisugi.gate" in c]
    assert len(gate_cmds) == 1
    assert "--mode shadow" in gate_cmds[0]
    assert "|| exit 2" not in gate_cmds[0]


def test_claude_gate_enforce_requires_explicit_flag(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=GATE_LAYERS,
        enforce=True,
    )
    commands = _pre_commands(_settings(tmp_path))
    gate_cmds = [c for c in commands if "opendaisugi.gate" in c]
    assert len(gate_cmds) == 1
    assert "--mode enforce" in gate_cmds[0]
    assert gate_cmds[0].rstrip().endswith("|| exit 2")


def test_claude_gate_coexists_with_capture_hook(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    commands = _pre_commands(_settings(tmp_path))
    assert any("daisugi hook record" in c for c in commands)
    assert any("opendaisugi.gate" in c for c in commands)


def test_claude_gate_idempotent_on_rerun(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    before = (tmp_path / ".claude" / "settings.json").read_text()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    after = (tmp_path / ".claude" / "settings.json").read_text()
    assert before == after
    commands = _pre_commands(_settings(tmp_path))
    assert sum("opendaisugi.gate" in c for c in commands) == 1


def test_claude_gate_mode_mismatch_warns_instead_of_silently_lying(tmp_path):
    # Install shadow, then ask for enforce: idempotency means it does NOT
    # silently rewrite (that could also silently escalate a user into
    # enforce). But it must not pretend nothing is wrong either.
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        install(
            home=tmp_path,
            yes=True,
            runtimes=[ClaudeCodeRuntime()],
            layers=GATE_LAYERS,
            enforce=True,
        )
    commands = _pre_commands(_settings(tmp_path))
    gate_cmds = [c for c in commands if "opendaisugi.gate" in c]
    assert len(gate_cmds) == 1
    assert "--mode shadow" in gate_cmds[0]  # unchanged — still shadow
    assert any("already installed" in str(w.message) for w in caught)


def test_claude_gate_unparseable_settings_skip_and_warn(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    original = '{"permissions": {"deny": ["Read(./secrets/**)"]},}'  # trailing comma
    (claude_dir / "settings.json").write_text(original)
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        from opendaisugi.install import _patch_claude_gate

        result = _patch_claude_gate(claude_dir / "settings.json", enforce=False)
    assert result == []
    assert (claude_dir / "settings.json").read_text() == original
    assert any("not valid JSON" in str(w.message) for w in caught)


# ---------------------------------------------------------------------------
# Claude Code — BASE_URL
# ---------------------------------------------------------------------------


def test_claude_base_url_sets_env(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BASE_URL_LAYERS,
        base_url=GATEWAY_URL,
    )
    settings = _settings(tmp_path)
    assert settings["env"]["ANTHROPIC_BASE_URL"] == GATEWAY_URL


def test_claude_base_url_idempotent(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BASE_URL_LAYERS,
        base_url=GATEWAY_URL,
    )
    before = (tmp_path / ".claude" / "settings.json").read_text()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BASE_URL_LAYERS,
        base_url=GATEWAY_URL,
    )
    assert (tmp_path / ".claude" / "settings.json").read_text() == before


def test_claude_base_url_unparseable_settings_skip_and_warn(tmp_path):
    claude_dir = tmp_path / ".claude"
    claude_dir.mkdir()
    original = '{"env": {"X": "y"},}'  # trailing comma
    (claude_dir / "settings.json").write_text(original)
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        from opendaisugi.install import _patch_claude_base_url

        result = _patch_claude_base_url(claude_dir / "settings.json", GATEWAY_URL)
    assert result == []
    assert (claude_dir / "settings.json").read_text() == original
    assert any("not valid JSON" in str(w.message) for w in caught)


# ---------------------------------------------------------------------------
# No .bak spray across the three settings.json writers on idempotent rerun
# ---------------------------------------------------------------------------


def test_claude_gate_and_base_url_no_bak_spray_on_rerun(tmp_path):
    # GATE, BASE_URL, and CAPTURE are three independent writers of the same
    # settings.json; on the FIRST call each one backs up whatever the previous
    # writer in that same call just wrote (matches the house pattern — see
    # `_pop_json_hook` on reverse). What must NOT happen is the idempotent
    # rerun adding MORE backups — every writer's second call is a true no-op.
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BOTH_LAYERS,
        base_url=GATEWAY_URL,
    )
    before = set((tmp_path / ".claude").glob("settings.json.bak*"))
    install(  # second run: everything already present, must be a true no-op
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BOTH_LAYERS,
        base_url=GATEWAY_URL,
    )
    after = set((tmp_path / ".claude").glob("settings.json.bak*"))
    assert after == before  # no NEW backups on the idempotent rerun


# ---------------------------------------------------------------------------
# reverse / uninstall
# ---------------------------------------------------------------------------


def test_reverse_removes_gate_and_base_url_keeps_unrelated(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=BOTH_LAYERS,
        base_url=GATEWAY_URL,
    )
    # Add an unrelated user-owned hook and env key alongside ours.
    sp = tmp_path / ".claude" / "settings.json"
    settings = json.loads(sp.read_text())
    settings["hooks"]["PreToolUse"].append(
        {"matcher": "Bash", "hooks": [{"type": "command", "command": "my-own-hook"}]}
    )
    settings["env"]["MY_OWN_VAR"] = "keep-me"
    sp.write_text(json.dumps(settings))

    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])

    after = json.loads(sp.read_text())
    commands = _pre_commands(after)
    assert not any("opendaisugi.gate" in c for c in commands)
    assert not any("daisugi hook record" in c for c in commands)
    assert "my-own-hook" in commands  # unrelated hook preserved
    assert "ANTHROPIC_BASE_URL" not in after.get("env", {})
    assert after["env"]["MY_OWN_VAR"] == "keep-me"  # unrelated env preserved


# ---------------------------------------------------------------------------
# Honest gaps — Codex, Hermes, OpenClaw
# ---------------------------------------------------------------------------


def test_codex_gate_installs_pretooluse_hook_in_hooks_json(tmp_path):
    # Codex v0.114+ hooks: same PreToolUse stdin shape and exit-2 deny as
    # Claude Code — the same gate entry point serves both. One honest
    # difference the step must state: Codex hooks fail OPEN on hook
    # crash/timeout, so a dead gate does not block.
    (tmp_path / ".codex").mkdir()
    steps = CodexRuntime().plan(tmp_path, GATE_LAYERS)
    gate_steps = [s for s in steps if s.layer is Layer.GATE]
    assert len(gate_steps) == 1
    assert gate_steps[0].supported is True
    assert "fail OPEN" in gate_steps[0].description

    CodexRuntime().apply(tmp_path, GATE_LAYERS)
    hooks = json.loads((tmp_path / ".codex" / "hooks.json").read_text())
    entries = hooks["hooks"]["PreToolUse"]
    commands = [h["command"] for e in entries for h in e.get("hooks", [])]
    assert any("opendaisugi.gate" in c for c in commands)
    # Codex regex-matches matchers; Claude's '*' glob is not a valid regex.
    assert all(
        e["matcher"] == ".*"
        for e in entries
        if any("opendaisugi.gate" in h["command"] for h in e.get("hooks", []))
    )


def test_codex_gate_idempotent_and_reversible(tmp_path):
    (tmp_path / ".codex").mkdir()
    CodexRuntime().apply(tmp_path, GATE_LAYERS)
    CodexRuntime().apply(tmp_path, GATE_LAYERS)
    hooks = json.loads((tmp_path / ".codex" / "hooks.json").read_text())
    commands = [h["command"] for e in hooks["hooks"]["PreToolUse"] for h in e.get("hooks", [])]
    assert sum("opendaisugi.gate" in c for c in commands) == 1

    CodexRuntime().reverse(tmp_path)
    hooks = json.loads((tmp_path / ".codex" / "hooks.json").read_text())
    commands = [
        h["command"]
        for e in hooks.get("hooks", {}).get("PreToolUse", [])
        for h in e.get("hooks", [])
    ]
    assert not any("opendaisugi.gate" in c for c in commands)


def test_codex_base_url_writes_model_provider_toml(tmp_path):
    (tmp_path / ".codex").mkdir()
    steps = CodexRuntime().plan(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    bu_steps = [s for s in steps if s.layer is Layer.BASE_URL]
    assert len(bu_steps) == 1 and bu_steps[0].supported is True

    CodexRuntime().apply(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    text = (tmp_path / ".codex" / "config.toml").read_text()
    assert "[model_providers.opendaisugi]" in text
    assert f'base_url = "{GATEWAY_URL}/v1"' in text
    assert 'wire_api = "chat"' in text
    # The top-level selector must precede any table header or TOML nests it.
    assert text.index('model_provider = "opendaisugi"') < text.index("[model_providers")


def test_codex_base_url_idempotent_and_reversible(tmp_path):
    (tmp_path / ".codex").mkdir()
    (tmp_path / ".codex" / "config.toml").write_text('model = "gpt-5.3-codex"\n')
    CodexRuntime().apply(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    CodexRuntime().apply(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    text = (tmp_path / ".codex" / "config.toml").read_text()
    assert text.count("[model_providers.opendaisugi]") == 1
    assert text.count('model_provider = "opendaisugi"') == 1

    CodexRuntime().reverse(tmp_path)
    text = (tmp_path / ".codex" / "config.toml").read_text()
    assert "[model_providers.opendaisugi]" not in text
    assert 'model_provider = "opendaisugi"' not in text
    assert 'model = "gpt-5.3-codex"' in text  # user content untouched


def test_hermes_base_url_unsupported_with_reason(tmp_path):
    (tmp_path / ".hermes").mkdir()
    steps = HermesRuntime().plan(tmp_path, BASE_URL_LAYERS)
    bu_steps = [s for s in steps if s.layer is Layer.BASE_URL]
    assert len(bu_steps) == 1
    assert bu_steps[0].supported is False
    assert "OpenAI" in bu_steps[0].description


def test_hermes_gate_unsupported_with_reason(tmp_path):
    (tmp_path / ".hermes").mkdir()
    steps = HermesRuntime().plan(tmp_path, GATE_LAYERS)
    gate_steps = [s for s in steps if s.layer is Layer.GATE]
    assert len(gate_steps) == 1
    assert gate_steps[0].supported is False
    assert "not yet wired" in gate_steps[0].description


def test_openclaw_gate_unsupported_with_reason(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    steps = OpenClawRuntime().plan(tmp_path, GATE_LAYERS)
    gate_steps = [s for s in steps if s.layer is Layer.GATE]
    assert len(gate_steps) == 1
    assert gate_steps[0].supported is False
    assert "not yet wired" in gate_steps[0].description


def test_unsupported_layers_matrix():
    assert ClaudeCodeRuntime().unsupported_layers() == {}
    assert CodexRuntime().unsupported_layers() == {}  # gate + base_url both wired
    hermes_gaps = HermesRuntime().unsupported_layers()
    assert set(hermes_gaps) == {Layer.GATE, Layer.BASE_URL}
    openclaw_gaps = OpenClawRuntime().unsupported_layers()
    assert set(openclaw_gaps) == {Layer.GATE}  # BASE_URL IS wired for OpenClaw


# ---------------------------------------------------------------------------
# OpenClaw — BASE_URL IS wired (anthropic-messages)
# ---------------------------------------------------------------------------


def test_openclaw_base_url_registers_provider(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    install(
        home=tmp_path,
        yes=True,
        runtimes=[OpenClawRuntime()],
        layers=BASE_URL_LAYERS,
        base_url=GATEWAY_URL,
    )
    cfg = json.loads((tmp_path / ".openclaw" / "openclaw.json").read_text())
    provider = cfg["models"]["providers"]["opendaisugi"]
    assert provider["api"] == "anthropic-messages"
    assert provider["baseUrl"] == GATEWAY_URL


def test_openclaw_base_url_idempotent(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    OpenClawRuntime().apply(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    before = (tmp_path / ".openclaw" / "openclaw.json").read_text()
    OpenClawRuntime().apply(tmp_path, BASE_URL_LAYERS, base_url=GATEWAY_URL)
    assert (tmp_path / ".openclaw" / "openclaw.json").read_text() == before


def test_openclaw_reverse_removes_provider_keeps_mcp_and_other_providers(tmp_path):
    (tmp_path / ".openclaw" / "workspace").mkdir(parents=True)
    install(
        home=tmp_path,
        yes=True,
        runtimes=[OpenClawRuntime()],
        layers=BOTH_LAYERS,
        base_url=GATEWAY_URL,
    )
    cfg_path = tmp_path / ".openclaw" / "openclaw.json"
    cfg = json.loads(cfg_path.read_text())
    cfg.setdefault("models", {}).setdefault("providers", {})["other"] = {"baseUrl": "x"}
    cfg_path.write_text(json.dumps(cfg))

    uninstall(home=tmp_path, runtimes=[OpenClawRuntime()])

    after = json.loads(cfg_path.read_text())
    assert "opendaisugi" not in after.get("mcp", {}).get("servers", {})
    assert "opendaisugi" not in after.get("models", {}).get("providers", {})
    assert after["models"]["providers"]["other"] == {"baseUrl": "x"}  # unrelated preserved


# ---------------------------------------------------------------------------
# _format_summary must not count unsupported "gap" steps as real changes
# ---------------------------------------------------------------------------


def test_format_summary_excludes_unsupported_steps_from_change_counts():
    planned = [
        InstallStep(Layer.SKILL, "Symlink the skill", None, supported=True),
        InstallStep(Layer.GATE, "Not wired: no external tool gate", None, supported=False),
    ]
    summary = _format_summary([ClaudeCodeRuntime()], planned, modified=[])
    lines = [line.strip() for line in summary.splitlines()]
    assert any(line == "[skill] 1 change(s)" for line in lines)
    assert not any(line.startswith("[gate]") for line in lines)


# ---------------------------------------------------------------------------
# CLI — `daisugi install --gate --gateway`
# ---------------------------------------------------------------------------

from typer.testing import CliRunner  # noqa: E402

from opendaisugi.cli import app  # noqa: E402

runner = CliRunner()


def _cli_output(result) -> str:
    combined = result.output.lower()
    try:
        combined += (result.stderr or "").lower()
    except ValueError:
        pass  # older click already mixed stderr into output above
    return combined


def test_cli_install_dry_run_gate_gateway_lists_both_steps(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--dry-run", "--gate", "--gateway", "--runtime", "claude"],
    )
    assert result.exit_code == 0
    out = _cli_output(result)
    assert "gate" in out
    assert "base_url" in out
    assert "anthropic_base_url" in out or "gateway" in out


def test_cli_install_gate_yes_writes_hook(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--gate", "--yes", "--runtime", "claude"],
    )
    assert result.exit_code == 0
    settings = _settings(tmp_path)
    commands = _pre_commands(settings)
    assert any("opendaisugi.gate" in c and "--mode shadow" in c for c in commands)


def test_cli_install_codex_gate_plans_the_hook_with_the_fail_open_caveat(tmp_path, monkeypatch):
    (tmp_path / ".codex").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--dry-run", "--gate", "--runtime", "codex"],
    )
    assert result.exit_code == 0
    out = _cli_output(result)
    assert "hooks.json" in out
    assert "fail open" in out  # the honest Codex caveat, stated up front


def test_cli_install_enforce_without_gate_is_inert_and_noted(tmp_path, monkeypatch):
    # --enforce only means something alongside --gate; asking for it alone
    # must not silently do nothing without saying so (same "never a silent
    # skip" principle as the honest-gap reporting).
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(
        app,
        ["install", "--dry-run", "--enforce", "--runtime", "claude"],
    )
    assert result.exit_code == 0
    out = _cli_output(result)
    assert "only applies with --gate" in out
    assert "[gate]" not in out  # no gate step actually planned

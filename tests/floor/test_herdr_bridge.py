"""The Herdr bridge: Herdr runs `coppice attach` in one of its panes.

No test runs Herdr or coppice. The manifest is checked against the schema
constants in `opendaisugi.floor.manifest_schema`, and its rules against
status lines in the shape `harness/coppice/internal/attach/render.go`
draws.
"""

from __future__ import annotations

import json
import re
import tomllib
from pathlib import Path

import pytest

from opendaisugi.floor import herdr_bridge, manifest_schema

# Real attach status lines. harness/coppice/internal/attach checks that
# StatusLine draws each one, so a change there shows up here.
STATUS_LINES = (
    Path(__file__).resolve().parents[2]
    / "harness"
    / "coppice"
    / "testdata"
    / "attach"
    / "status-lines.json"
)


def _status(pane: str, label: str, harness: str, state: str, source: str, extra: str = "") -> str:
    """One attach status line, in the shape StatusLine in render.go builds."""
    parts = [pane]
    if label:
        parts.append(label)
    if harness:
        parts.append(harness)
    parts.append(f"{state} via {source}")
    return "  ".join(parts) + extra + "   ctrl-space leave"


def _state_of(manifest: dict, line: str) -> str:
    """The state the highest-priority matching rule asserts for one line,
    with the matcher rules manifest_schema records: every line_regex must
    match some line, and every regex must match the region."""
    best = None
    for rule in manifest["rules"]:
        ok = all(re.search(p, line) for p in rule.get("line_regex", []))
        ok = ok and all(re.search(p, line) for p in rule.get("regex", []))
        ok = ok and all(n.lower() in line.lower() for n in rule.get("contains", []))
        if ok and (best is None or rule.get("priority", 0) > best.get("priority", 0)):
            best = rule
    return "unknown" if best is None else best.get("state", "unknown")


def test_the_definition_runs_coppice_attach_on_the_pane():
    d = herdr_bridge.agent_definition("w1:p2", "/run/user/1000/coppice/server.sock")
    assert d["command"] == [
        "coppice",
        "--socket",
        "/run/user/1000/coppice/server.sock",
        "attach",
        "w1:p2",
    ]
    assert d["agent"] == "coppice"
    assert d["verified"] is False
    assert d["manifest_file"] == "agent-detection/coppice.toml"


def test_the_herdr_run_line_quotes_the_command():
    d = herdr_bridge.agent_definition("w1:p2", "/srv/a b/s.sock", coppice="/opt/bin/coppice")
    assert d["herdr_run"] == [
        "herdr",
        "pane",
        "run",
        "<herdr-pane>",
        "/opt/bin/coppice --socket '/srv/a b/s.sock' attach w1:p2",
    ]


def test_the_manifest_is_valid_toml_with_only_known_keys_and_regions():
    text = herdr_bridge.manifest_toml()
    m = tomllib.loads(text)
    assert m == herdr_bridge.manifest()
    assert set(m) <= manifest_schema.MANIFEST_KEYS
    assert m["id"] == "coppice"
    assert m["rules"]
    for rule in m["rules"]:
        assert set(rule) <= manifest_schema.RULE_KEYS
        assert rule["state"] in manifest_schema.RULE_STATES
        region = rule["region"]
        name, _, arg = region.partition("(")
        assert region in manifest_schema.FIXED_REGIONS or (
            name in manifest_schema.PARAMETERIZED_REGIONS and arg.rstrip(")").isdigit()
        )
        assert set(rule) & manifest_schema.POSITIVE_MATCHER_KEYS
        for p in rule.get("line_regex", []) + rule.get("regex", []):
            assert len(p) <= manifest_schema.MAX_MATCHER_CHARS
            re.compile(p)
            # RE2 has no lookaround and no backreference.
            assert "(?=" not in p and "(?!" not in p and "(?<" not in p
            assert not re.search(r"\\[1-9]", p)


def test_the_blocked_rule_matches_the_attach_status_line():
    m = herdr_bridge.manifest()
    blocked = _status("w1:p2", "build", "claude-code", "blocked", "gate", " Bash: rm -rf build/")
    assert _state_of(m, blocked) == "blocked"
    assert _state_of(m, _status("w1:p2", "", "", "blocked", "manifest")) == "blocked"


def test_the_other_rules_match_their_states_and_never_blocked():
    m = herdr_bridge.manifest()
    assert _state_of(m, _status("w1:p2", "build", "claude-code", "working", "gate")) == "working"
    assert _state_of(m, _status("w1:p2", "build", "claude-code", "idle", "process")) == "idle"
    assert _state_of(m, _status("w1:p2", "build", "", "done", "process")) == "unknown"
    assert _state_of(m, _status("w1:p2", "build", "", "unknown", "none")) == "unknown"
    # An ask summary that says "blocked via gate" cannot turn a working
    # line blocked: the state word sits right after the fixed fields.
    tricky = _status("w1:p2", "build", "pi", "working", "gate", " Bash: echo blocked via gate")
    assert _state_of(m, tricky) == "working"


def test_the_rules_read_the_status_lines_attach_draws():
    rows = json.loads(STATUS_LINES.read_text(encoding="utf-8"))
    assert rows
    m = herdr_bridge.manifest()
    for row in rows:
        assert _state_of(m, row["line"]) == row["herdr_state"], row["line"]


def test_the_committed_manifest_is_what_the_bridge_writes():
    # internal/detect runs this file through the Go port of Herdr's engine
    # against every row of the status line fixture.
    committed = STATUS_LINES.with_name("herdr-coppice.toml").read_text(encoding="utf-8")
    assert committed == herdr_bridge.manifest_toml()


def test_the_manifest_reads_only_the_last_line():
    for rule in herdr_bridge.manifest()["rules"]:
        assert rule["region"] == "bottom_non_empty_lines(1)"


def test_install_writes_one_file_and_touches_no_other(tmp_path):
    cfg = tmp_path / "herdr"
    (cfg / "agent-detection").mkdir(parents=True)
    other = cfg / "agent-detection" / "claude.toml"
    other.write_text("id = 'claude'\n")
    top = cfg / "config.toml"
    top.write_text("theme = 'dark'\n")
    before = {p: p.read_bytes() for p in cfg.rglob("*") if p.is_file()}

    path = herdr_bridge.install(cfg)

    assert path == cfg / "agent-detection" / "coppice.toml"
    assert path.read_text() == herdr_bridge.manifest_toml()
    after = {p: p.read_bytes() for p in cfg.rglob("*") if p.is_file()}
    assert set(after) - set(before) == {path}
    for p, b in before.items():
        assert after[p] == b


def test_install_twice_is_the_same_file(tmp_path):
    first = herdr_bridge.install(tmp_path)
    second = herdr_bridge.install(tmp_path)
    assert first == second
    assert first.read_text() == herdr_bridge.manifest_toml()


def test_install_refuses_to_replace_a_different_file(tmp_path):
    path = tmp_path / "agent-detection" / "coppice.toml"
    path.parent.mkdir(parents=True)
    path.write_text("id = 'coppice'\n# mine\n")
    with pytest.raises(FileExistsError, match="coppice.toml"):
        herdr_bridge.install(tmp_path)
    assert path.read_text() == "id = 'coppice'\n# mine\n"


def test_install_replaces_an_older_manifest_of_its_own(tmp_path):
    path = tmp_path / "agent-detection" / "coppice.toml"
    path.parent.mkdir(parents=True)
    first = herdr_bridge.manifest_toml().splitlines()[0]
    path.write_text(first + '\nid = "coppice"\n# an older version\n')
    assert herdr_bridge.install(tmp_path) == path
    assert path.read_text() == herdr_bridge.manifest_toml()
    assert sorted(p.name for p in path.parent.iterdir()) == ["coppice.toml"]


def test_install_refuses_a_link_even_to_its_own_manifest(tmp_path):
    real = tmp_path / "real.toml"
    real.write_text(herdr_bridge.manifest_toml())
    path = tmp_path / "agent-detection" / "coppice.toml"
    path.parent.mkdir(parents=True)
    path.symlink_to(real)
    with pytest.raises(FileExistsError, match="coppice.toml"):
        herdr_bridge.install(tmp_path)


def test_the_cli_writes_an_absolute_socket_into_the_run_line(tmp_path, monkeypatch):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    monkeypatch.chdir(tmp_path)
    result = CliRunner().invoke(
        app,
        [
            "coppice",
            "herdr-bridge",
            "w1:p2",
            "--socket",
            "rel/s.sock",
            "--herdr-config",
            str(tmp_path / "herdr"),
            "--data-dir",
            str(tmp_path / "data"),
            "--json",
        ],
    )
    assert result.exit_code == 0, result.output
    d = json.loads(result.stdout)
    assert d["command"][2] == str(tmp_path / "rel" / "s.sock")


def test_the_cli_installs_and_prints_the_run_line(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    result = CliRunner().invoke(
        app,
        [
            "coppice",
            "herdr-bridge",
            "w1:p2",
            "--socket",
            str(tmp_path / "s.sock"),
            "--herdr-config",
            str(tmp_path / "herdr"),
            "--data-dir",
            str(tmp_path / "data"),
        ],
    )
    assert result.exit_code == 0, result.output
    assert (tmp_path / "herdr" / "agent-detection" / "coppice.toml").is_file()
    assert "herdr pane run" in result.output
    assert "attach w1:p2" in result.output
    assert "not verified" in result.output


def test_the_cli_refuses_when_a_different_manifest_is_there(tmp_path):
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    path = tmp_path / "agent-detection" / "coppice.toml"
    path.parent.mkdir(parents=True)
    path.write_text("# mine\n")
    result = CliRunner().invoke(
        app,
        [
            "coppice",
            "herdr-bridge",
            "w1:p2",
            "--herdr-config",
            str(tmp_path),
            "--data-dir",
            str(tmp_path / "data"),
        ],
    )
    assert result.exit_code == 1
    assert "coppice.toml" in result.output
    assert path.read_text() == "# mine\n"

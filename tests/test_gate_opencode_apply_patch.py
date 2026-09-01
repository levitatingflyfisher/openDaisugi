"""OpenCode's apply_patch, as the gate sees it.

The plugin sends apply_patch as mcp__opencode__apply_patch. The gate never
treats it as an MCP call. It reads each Add, Update, Delete and Move-to
path out of the patch and checks each one as a file write, against the
envelope, the tiers and both hard-deny rules. A patch it cannot read is
denied.
"""

from __future__ import annotations

import pytest

from opendaisugi.effects import PERMANENT
from opendaisugi.floor_config import OPENCODE_PLUGIN_REFUSAL
from opendaisugi.floor_config import REFUSAL as FLOOR_REFUSAL
from opendaisugi.gate import evaluate_call
from opendaisugi.hook import _payload_to_record, parse_apply_patch
from opendaisugi.models import Envelope, Permission

TOOL = "mcp__opencode__apply_patch"


@pytest.fixture
def homes(tmp_path, monkeypatch):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path / "cfg"))
    monkeypatch.setenv("XDG_DATA_HOME", str(tmp_path / "share"))
    return tmp_path


def _env(**kw) -> Envelope:
    perms = {
        "file_read": ["/**"],
        "file_write": ["/work/**"],
        "mcp_allowlist": ["opencode/apply_patch", "opencode/*", "*/*"],
        **kw,
    }
    return Envelope(generated_by="test", task="patch", permissions=Permission(**perms))


def _patch(*body: str) -> str:
    return "\n".join(["*** Begin Patch", *body, "*** End Patch"])


def _call(patch, cwd: str = "/work") -> dict:
    return {"tool_name": TOOL, "tool_input": {"patchText": patch}, "session_id": "s1", "cwd": cwd}


# --- the parser --------------------------------------------------------------------------


def test_the_parser_reads_every_header_path():
    patch = _patch(
        "*** Add File: a.txt",
        "+hi",
        "*** Update File: src/b.py",
        "*** Move to: src/c.py",
        "@@",
        "-x",
        "+y",
        "*** Delete File: old.txt",
    )
    assert parse_apply_patch(patch) == ["a.txt", "src/b.py", "src/c.py", "old.txt"]


def test_the_parser_reads_a_heredoc_wrapped_patch():
    patch = "cat <<'EOF'\n" + _patch("*** Add File: a.txt", "+x") + "\nEOF"
    assert parse_apply_patch(patch) == ["a.txt"]


@pytest.mark.parametrize(
    "text",
    [
        "",
        "not a patch",
        "*** End Patch\n*** Begin Patch\n*** Add File: a",
        "*** Begin Patch\n*** Add File: a.txt\n+x",
        _patch(),
        _patch("just text"),
        None,
        42,
    ],
)
def test_the_parser_refuses_what_it_cannot_read(text):
    assert parse_apply_patch(text) is None


# --- never an MCP admission --------------------------------------------------------------


def test_apply_patch_is_never_an_mcp_record():
    for fmt in ("opencode", "claude", "pi"):
        record = _payload_to_record({"tool_name": TOOL, "tool_input": {"patchText": "x"}}, fmt=fmt)
        assert record is None, fmt


@pytest.mark.parametrize("fmt", ["opencode", "claude"])
def test_an_add_file_outside_the_envelope_is_denied_though_mcp_names_it(homes, fmt):
    d = evaluate_call(
        _call(_patch("*** Add File: /etc/evil", "+x")), _env(), mode="enforce", fmt=fmt
    )
    assert d.allow is False
    assert "/etc/evil" in d.reason or "file_write" in d.reason
    assert d.tier == PERMANENT


def test_a_patch_inside_the_envelope_is_allowed(homes):
    patch = _patch(
        "*** Update File: /work/a.py", "@@", "-x", "+y", "*** Add File: /work/b.py", "+z"
    )
    d = evaluate_call(_call(patch), _env(), mode="enforce", fmt="opencode")
    assert d.allow is True, d.reason


def test_one_path_outside_the_envelope_denies_the_whole_patch(homes):
    patch = _patch(
        "*** Add File: /work/ok.py", "+x", "*** Update File: /work/a.py", "*** Move to: /etc/b.py"
    )
    d = evaluate_call(_call(patch), _env(), mode="enforce", fmt="opencode")
    assert d.allow is False


def test_an_unparseable_patch_is_denied(homes):
    d = evaluate_call(_call("rm -rf /"), _env(), mode="enforce", fmt="opencode")
    assert d.allow is False
    assert "patch" in d.reason
    d = evaluate_call(
        {"tool_name": TOOL, "tool_input": {"patchText": 7}, "cwd": "/work"}, _env(), mode="enforce"
    )
    assert d.allow is False


# --- the hard-deny rules see every path --------------------------------------------------


def test_a_dotdot_path_to_the_plugin_is_denied(homes):
    proj = homes / "proj"
    proj.mkdir()
    patch = _patch("*** Delete File: ../cfg/opencode/plugins/daisugi-gate.ts")
    d = evaluate_call(_call(patch, str(proj)), _env(), mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL
    assert d.pane_rule is True


def test_a_symlinked_path_to_the_plugin_is_denied(homes):
    plugins = homes / "cfg" / "opencode" / "plugins"
    plugins.mkdir(parents=True)
    proj = homes / "proj"
    proj.mkdir()
    (proj / "lnk").symlink_to(plugins)
    patch = _patch("*** Delete File: lnk/daisugi-gate.ts")
    d = evaluate_call(_call(patch, str(proj)), _env(), mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_a_move_into_a_project_plugin_directory_is_denied(homes):
    patch = _patch("*** Update File: /work/a.ts", "*** Move to: /work/.opencode/plugins/a.ts")
    d = evaluate_call(_call(patch), _env(), mode="enforce", fmt="opencode")
    assert d.reason == OPENCODE_PLUGIN_REFUSAL


def test_a_patch_into_the_floor_config_is_denied(homes):
    patch = _patch(f"*** Add File: {homes}/cfg/coppice/coppice.toml", "+x")
    d = evaluate_call(_call(patch), _env(), mode="enforce", fmt="opencode")
    assert d.reason == FLOOR_REFUSAL


def test_distill_never_writes_opencode_apply_patch_into_an_envelope():
    from opendaisugi.hook import infer_envelope

    records = [
        {"step_type": "mcp", "mcp_server": "opencode", "mcp_tool": "apply_patch"},
        {"step_type": "mcp", "mcp_server": "github", "mcp_tool": "list"},
    ]
    env = infer_envelope(records, task="t")
    assert env.permissions.mcp_allowlist == ["github/list"]


def test_a_relative_patch_path_is_placed_from_the_call_cwd(homes):
    proj = homes / "proj"
    proj.mkdir()
    env = _env(file_write=[f"{proj}/**"])
    d = evaluate_call(_call(_patch("*** Add File: src/a.py", "+x"), str(proj)), env, mode="enforce")
    assert d.allow is True, d.reason


def test_a_relative_patch_path_from_another_cwd_is_not_the_envelopes(homes):
    env = _env(file_write=["src/**"])
    d = evaluate_call(_call(_patch("*** Add File: src/a.py", "+x"), "/etc"), env, mode="enforce")
    assert d.allow is False


def test_a_patch_with_no_cwd_is_denied(homes):
    patch = _patch("*** Add File: /work/a.py", "+x")
    d = evaluate_call(
        {"tool_name": TOOL, "tool_input": {"patchText": patch}}, _env(), mode="enforce"
    )
    assert d.allow is False
    assert "working directory" in d.reason

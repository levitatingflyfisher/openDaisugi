"""The fence's rule list: properties of the oracle and its golden file."""

from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path

import pytest

from opendaisugi import fence
from opendaisugi.fence import SECRETS, covers, glob_prefix, plan

CLIENTS = Path(__file__).resolve().parent.parent / "clients"
sys.path.insert(0, str(CLIENTS))

import fence_cases  # noqa: E402

HOME = "/home/user"


def inp(**kw):
    return fence_cases.base(**kw)


def modes(out):
    return {r["path"]: r["mode"] for r in out["rules"]}


def reach(out, path, write=False):
    """Would the rule list let the agent read (or write) path?"""
    for r in out["rules"]:
        if not covers(r["path"], path):
            continue
        if write and r["mode"] in ("rw", "rw+unix"):
            return True
        if not write and r["mode"] != "unix":
            return True
    return False


def connect(out, path):
    """Would the rule list let the agent connect to the socket at path?"""
    return any(
        covers(r["path"], path) and r["mode"] in ("unix", "sock", "rw+unix") for r in out["rules"]
    )


def test_default_plan_shuts_every_secret_and_the_rest_of_home():
    out = plan(inp())
    assert out["refusals"] == []
    for s in SECRETS:
        assert not reach(out, f"{HOME}/{s}/key"), s
    assert not reach(out, HOME + "/Documents/a.txt")
    assert not reach(out, HOME + "/.bashrc")
    assert reach(out, HOME + "/proj/a.txt", write=True)
    assert reach(out, "/usr/bin/env")
    assert not reach(out, "/usr/bin/env", write=True)


def test_gate_state_is_never_writable_and_its_gate_dir_is_readable():
    out = plan(inp())
    assert reach(out, HOME + "/.opendaisugi/gate/envelopes/default.json")
    assert not reach(out, HOME + "/.opendaisugi/gate/DISARMED", write=True)
    assert not reach(out, HOME + "/.opendaisugi/journal")


def test_fn_v4_nothing_under_the_gate_state_is_writable_and_gate_sock_connects():
    out = plan(inp(gate_root="/srv/gate"))
    g = HOME + "/.opendaisugi"
    for sub in (
        "gate/audit/s.jsonl",
        "gate/asks/a.json",
        "gate/checkpoint-state/x",
        "gate/answers/a.json",
        "sessions/s.jsonl",
        "gate/DISARMED",
    ):
        assert not reach(out, f"{g}/{sub}", write=True), sub
    assert not reach(out, "/srv/gate/asks/a.json", write=True)
    assert connect(out, g + "/gate/gate.sock")
    assert connect(out, "/srv/gate/gate.sock")
    assert not connect(out, HOME + "/proj/agent.sock")


def test_a_workspace_above_a_secret_is_refused_with_the_way_out():
    out = plan(inp(workspace=HOME))
    assert len(out["refusals"]) == 1
    assert "--no-fence" in out["refusals"][0]


def test_fn_v3_a_secret_as_workspace_temp_dir_or_harness_path_is_refused():
    for kw in (
        {"workspace": HOME + "/.ssh"},
        {"workspace": HOME + "/.ssh/sub"},
        {"workspace": HOME + "/.local/share/keyrings"},
        {"tmpdir": HOME + "/.ssh"},
        {"workdirs": [HOME + "/.aws"]},
        {"real": {HOME + "/.config/claude": HOME + "/.ssh"}},
        {"workspace": HOME + "/.config/coppice"},
    ):
        out = plan(inp(**kw))
        assert out["refusals"], kw
        assert not reach(out, HOME + "/.ssh/id_ed25519"), kw


def test_fn_v3_an_envelope_prefix_inside_a_secret_is_dropped():
    out = plan(inp(envelope={"file_read": ["~/.ssh/**"], "file_write": ["~/.aws/x/*"]}))
    assert not reach(out, HOME + "/.ssh/id_ed25519")
    assert not reach(out, HOME + "/.aws/x/c")
    assert len(out["notes"]) == 2


def test_fn_v8_a_harness_path_or_prefix_that_links_to_a_secret_is_caught():
    out = plan(inp(real={HOME + "/.cache/claude": HOME + "/.gnupg"}))
    assert out["refusals"]
    out = plan(
        inp(
            envelope={"file_read": ["/mnt/out/**"], "file_write": []},
            real={"/mnt/out": HOME + "/.ssh"},
        )
    )
    assert not reach(out, HOME + "/.ssh/k")


def test_fn_v11_browser_and_tool_token_stores_stay_shut():
    out = plan(inp(path_dirs=[HOME + "/.mozilla/bin", HOME + "/.ollama"]))
    for s in (
        ".mozilla",
        ".thunderbird",
        ".config/google-chrome",
        ".config/Code",
        ".config/git/credentials",
        ".pki",
        ".ollama",
        ".cache/huggingface/token",
        ".config/tailscale",
        ".age",
    ):
        assert s in SECRETS, s
        assert not reach(out, f"{HOME}/{s}/k"), s
    assert plan(inp(workspace=HOME + "/.config"))["refusals"]


def test_fn_v1_the_password_home_keeps_its_secrets_shut_when_home_moves():
    out = plan(inp(home="/elsewhere", workspace=HOME, other_homes=[HOME]))
    assert out["refusals"]
    out = plan(
        inp(
            home="/elsewhere",
            workspace="/elsewhere/w",
            other_homes=[HOME],
            allow=[HOME + "/.config"],
        )
    )
    assert out["refusals"]


def test_fn_v6_harness_config_stays_read_only():
    out = plan(inp(harness="claude"))
    for p in (
        ".claude/settings.json",
        ".claude/hooks/h.sh",
        ".claude/skills/s/SKILL.md",
        ".claude/agents/a.md",
        ".claude/plugins/p",
        ".claude/CLAUDE.md",
    ):
        assert reach(out, f"{HOME}/{p}"), p
        assert not reach(out, f"{HOME}/{p}", write=True), p
    for p in (
        ".claude/projects/" + fence.claude_project_name(fence_cases.WS) + "/x.jsonl",
        ".claude/todos/t",
        ".claude/.credentials.json",
    ):
        assert reach(out, f"{HOME}/{p}", write=True), p
    # N-2: ~/.claude.json holds mcpServers and per-project settings that a
    # later unfenced claude runs: read only, unless --allow opens it.
    assert reach(out, HOME + "/.claude.json")
    assert not reach(out, HOME + "/.claude.json", write=True)
    out = plan(inp(harness="claude", allow=["~/.claude.json:rw"]))
    assert out["refusals"] == []
    assert reach(out, HOME + "/.claude.json", write=True)
    out = plan(inp(harness="codex"))
    assert not reach(out, HOME + "/.codex/config.toml", write=True)
    assert not reach(out, HOME + "/.codex/hooks.json", write=True)
    assert reach(out, HOME + "/.codex/sessions/s", write=True)
    out = plan(inp(harness="pi"))
    assert not reach(out, HOME + "/.pi/agent/extensions/e.ts", write=True)
    assert reach(out, HOME + "/.pi/agent/sessions/s", write=True)
    out = plan(inp(harness="sprig"))
    assert not reach(out, HOME + "/.local/share/sprig/config", write=True)
    # N-2: OpenCode's config (plugins, MCP servers, the gate plugin), its
    # downloaded binaries and its plugin packages run later outside any
    # fence: read only. Its sessions, logs and state stay writable.
    out = plan(inp(harness="opencode"))
    for p in (
        ".config/opencode/opencode.json",
        ".config/opencode/plugins/daisugi-gate.ts",
        ".local/share/opencode/bin/lsp",
        ".cache/opencode/node_modules/x/index.js",
    ):
        assert reach(out, f"{HOME}/{p}"), p
        assert not reach(out, f"{HOME}/{p}", write=True), p
    for p in (
        ".local/share/opencode/storage/s.json",
        ".local/share/opencode/log/l",
        ".local/share/opencode/project/x",
        ".local/share/opencode/auth.json",
        ".local/state/opencode/kv.json",
    ):
        assert reach(out, f"{HOME}/{p}", write=True), p


def test_fn_v9_tmp_is_not_granted_and_the_private_temp_dir_is():
    out = plan(inp(tmpdir=""))
    assert not reach(out, "/tmp/claude-1000/x")
    own = HOME + "/.local/share/opendaisugi/fence-tmp/4242"
    out = plan(inp(tmpdir=own))
    assert out["refusals"] == []
    assert reach(out, own + "/f", write=True)
    assert not reach(out, "/tmp/x")
    pane = HOME + "/.local/share/coppice/fence-tmp/w1:p1"
    out = plan(inp(tmpdir=pane))
    assert out["refusals"] == [] and reach(out, pane + "/f", write=True)
    assert not reach(out, HOME + "/.local/share/coppice/token")
    deep = HOME + "/.local/share/coppice/fence-tmp/a/b"
    assert plan(inp(tmpdir=deep))["refusals"]
    assert reach(plan(inp(tmpdir="", allow=["/tmp:rw"])), "/tmp/x", write=True)


def test_fn_v2_only_the_coppice_socket_dir_and_gate_root_connect():
    out = plan(inp(sockets=["/mnt/c/run/server.sock"]))
    assert connect(out, "/mnt/c/run/server.sock")
    assert not reach(out, "/mnt/c/run/token")
    assert not connect(out, "/run/user/1000/bus")
    assert not connect(out, "/tmp/tmux-1000/default")
    for sock in ("/run/user/1000/bus", "/tmp/x.sock", HOME + "/s.sock", HOME + "/.ssh/cm-host"):
        out = plan(inp(sockets=[sock], env={"XDG_RUNTIME_DIR": "/run/user/1000"}))
        assert not connect(out, sock), sock
        assert out["notes"], sock
    out = plan(
        inp(
            sockets=["/run/user/1000/coppice/server.sock"],
            env={"XDG_RUNTIME_DIR": "/run/user/1000"},
        )
    )
    assert connect(out, "/run/user/1000/coppice/server.sock")
    assert not connect(out, "/run/user/1000/bus")


def test_a_secret_named_exactly_is_granted():
    out = plan(inp(allow=["~/.ssh"]))
    assert out["refusals"] == []
    assert modes(out)[HOME + "/.ssh"] == "ro"


def test_a_writable_grant_on_gate_state_is_refused_even_when_named():
    for spec in ("~/.opendaisugi/gate:rw", "~/.opendaisugi:rw", "~/.opendaisugi/gate/x:rw"):
        assert plan(inp(allow=[spec]))["refusals"], spec


def test_a_path_entry_above_or_inside_a_secret_is_dropped_not_refused():
    out = plan(inp(path_dirs=[HOME, HOME + "/.ssh/bin"]))
    assert out["refusals"] == []
    assert HOME not in modes(out)
    assert HOME + "/.ssh/bin" not in modes(out)


def test_envelope_globs_are_cut_to_their_literal_directory():
    assert glob_prefix("/mnt/ref/**", HOME, "/w") == "/mnt/ref"
    assert glob_prefix("out/*.txt", HOME, "/w") == "/w/out"
    assert glob_prefix("/etc/hosts", HOME, "/w") == "/etc/hosts"
    assert glob_prefix("**", HOME, "/w") == "/w"
    assert glob_prefix("/**", HOME, "/w") == "/"
    assert glob_prefix("../x/[ab]", HOME, "/w/p") == "/w/x"


def test_envelope_cannot_widen_past_home_or_into_gate_state():
    out = plan(
        inp(
            envelope={
                "file_read": ["/**", "~/**", "/home/user/.config/**"],
                "file_write": ["~/.opendaisugi/**"],
            }
        )
    )
    assert len(out["notes"]) == 4
    assert not reach(out, HOME + "/Documents")
    assert not reach(out, HOME + "/.opendaisugi/gate/DISARMED", write=True)


def test_plan_is_pure_and_deterministic():
    a = inp(allow=["/b:rw", "/a"], path_dirs=["/usr/bin", "/usr/bin"])
    assert plan(a) == plan(json.loads(json.dumps(a)))


def test_golden_file_matches_the_oracle():
    body, n = fence_cases.render()
    on_disk = (CLIENTS / "fixtures" / "fence" / "cases.jsonl").read_text()
    assert on_disk == body
    manifest = json.loads((CLIENTS / "fixtures" / "fence" / "manifest.json").read_text())
    assert manifest == {"cases.jsonl": hashlib.sha256(body.encode()).hexdigest(), "n": n}


@pytest.mark.parametrize("name,case", fence_cases.CASES)
def test_every_case_is_stable(name, case):
    assert plan(case) == plan(json.loads(json.dumps(case)))


def test_r2_3_harness_data_that_steers_a_later_run_is_read_only():
    # OpenCode runs git on its snapshot repos later, outside any fence.
    out = plan(inp(harness="opencode"))
    assert reach(out, HOME + "/.local/share/opencode/snapshot/p/config")
    assert not reach(out, HOME + "/.local/share/opencode/snapshot/p/config", write=True)
    out = plan(inp(harness="opencode", allow=["~/.local/share/opencode/snapshot:rw"]))
    assert reach(out, HOME + "/.local/share/opencode/snapshot/p/config", write=True)
    # Claude's memory of another project is loaded by a later session there:
    # only this workspace's project dir is writable. Landlock cannot cut
    # memory/ out of that one dir, so it stays writable, like the
    # workspace's own CLAUDE.md (FN-R-31).
    own = HOME + "/.claude/projects/" + fence.claude_project_name(fence_cases.WS)
    out = plan(inp(harness="claude"))
    assert reach(out, own + "/s.jsonl", write=True)
    assert reach(out, own + "/s/subagents/a.jsonl", write=True)
    for p in ("-elsewhere/memory/MEMORY.md", "-elsewhere/s.jsonl"):
        assert reach(out, HOME + "/.claude/projects/" + p), p
        assert not reach(out, HOME + "/.claude/projects/" + p, write=True), p
    out = plan(inp(harness="claude", allow=["~/.claude/projects/-elsewhere/memory:rw"]))
    assert out["refusals"] == []
    assert reach(out, HOME + "/.claude/projects/-elsewhere/memory/MEMORY.md", write=True)


def test_r2_3_claude_project_name_is_claudes_own():
    assert fence.claude_project_name("/mnt/tera/a_b/c.d") == "-mnt-tera-a-b-c-d"
    # A name Claude would shorten, or a path with other than ASCII, cannot
    # be named: the whole projects dir stays writable, and a note says so.
    for ws in ("/w/" + "x" * 200, "/w/caf\u00e9"):
        out = plan(inp(harness="claude", workspace=ws))
        assert reach(out, HOME + "/.claude/projects/-elsewhere/memory/M.md", write=True), ws
        assert any("cannot name" in n for n in out["notes"]), ws

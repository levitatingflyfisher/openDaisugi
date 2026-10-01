"""Agent profiles: the operator's table, per-step choice, keys by pipe.

No real agent runs: a spy stands in for the sprig process call and for
the claude call, and a tiny fake sprig stands in where the descriptors
themselves are the point. Every key here is a fake sentinel.
"""

from __future__ import annotations

import json
import os
import stat
import sys
from pathlib import Path

import pytest

from opendaisugi import agent_profiles, agentic_executor
from opendaisugi.agentic_executor import AgenticExecutor
from opendaisugi.models import AgenticStep, Envelope, Permission
from opendaisugi.verify import _check_agentic_step

KEY = "sk-SENTINEL-profiles"
TABLE = {
    "oai": {"wire": "openai", "base_url": "http://127.0.0.1:9/v1", "key_env": "OAI_KEY"},
    "local": {"wire": "anthropic", "base_url": "http://127.0.0.1:8", "model": "llama3.3:70b"},
    "plain": {"wire": "anthropic"},
    "cc": {"wire": "claude-code", "model": "opus"},
    "nokey": {"wire": "openai", "key_env": "NOT_SET_HERE"},
    "bad": {"wire": "grpc"},
}
ENV = {"OAI_KEY": KEY, "HOME": "/h"}


def _envelope(**perm) -> Envelope:
    perms = {"file_read": ["/tmp/**", "/work/**"], "shell": True, **perm}
    return Envelope(generated_by="test", task="profiles", permissions=Permission(**perms))


def _step(workspace, **kw) -> AgenticStep:
    d = dict(id="a1", prompt="do it", workspace=str(workspace), tools=["Read"])
    d.update(kw)
    return AgenticStep(**d)


def _run(ex, step):
    return ex.run(step, timeout_s=30, max_output_bytes=100_000)


@pytest.fixture
def spy(monkeypatch):
    seen: dict = {}

    def _fake(argv, *, stdin, cwd, timeout_s, **kw):
        seen.update(argv=list(argv), kw=kw)
        body = {"answer": "ok", "turns": 2, "model_turns": 1, "usage": {"input_tokens": 3}}
        return 0, json.dumps(body).encode(), b"", False

    monkeypatch.setattr(agentic_executor, "_run_sprig", _fake)
    return seen


def _flag(argv, name):
    return argv[argv.index(name) + 1] if name in argv else None


# --- the table -------------------------------------------------------------


def test_resolve_reads_valid_entries_and_says_why_others_are_not():
    p = agent_profiles.resolve({**TABLE, "default": {"wire": "openai"}, 5: {}}, ENV)
    assert sorted(p.valid) == ["cc", "local", "nokey", "oai", "plain"]
    assert p.invalid == {
        "bad": "wire must be claude-code, anthropic or openai",
        "default": "default is built in",
    }
    assert p.valid["oai"].key == KEY and p.valid["nokey"].key is None
    assert p.key_names() == ["NOT_SET_HERE", "OAI_KEY"]


@pytest.mark.parametrize(
    "doc,why",
    [
        ({"wire": "claude-code", "key_env": "X"}, "the claude-code wire takes no key_env"),
        ({"wire": "claude-code", "base_url": "http://x"}, "the claude-code wire takes no base_url"),
        ({"wire": "openai", "model": 3}, "model must be text"),
        ({"wire": "openai", "key_env": "1X"}, "key_env must be a variable name"),
        ({"wire": "openai", "url": "x"}, "it has an unknown field url"),
        ("openai", "it is not a mapping"),
    ],
)
def test_invalid_entries(doc, why):
    assert agent_profiles.resolve({"p": doc}, {}).invalid == {"p": why}


def test_lookup_texts_name_no_origin():
    p = agent_profiles.resolve(TABLE, ENV)
    assert p.lookup("zzz")[1] == (
        "no agent profile named 'zzz'; the profiles are cc, default, local, nokey, oai, plain"
    )
    assert p.lookup("nokey")[1] == "agent profile 'nokey' names NOT_SET_HERE, which is not set"
    assert p.lookup("bad")[1] == (
        "agent profile 'bad' is not valid: wire must be claude-code, anthropic or openai"
    )
    assert p.lookup(None)[0] is agent_profiles.BUILTIN


def test_bundle_round_trips_with_keys():
    p = agent_profiles.resolve(TABLE, ENV)
    back = agent_profiles.from_bundle(p.bundle())
    assert back.valid["oai"].key == KEY
    assert json.loads(p.bundle())["default"]["wire"] == "claude-code"


def test_config_text_shows_names_only():
    assert agent_profiles.config_text(TABLE) == "bad, cc, local, nokey, oai, plain"
    assert agent_profiles.config_text(None) == "none"
    assert agent_profiles.config_text([1]) == "not a mapping"


def test_profile_is_hidden_and_left_out_of_dumps():
    step = AgenticStep(id="a", prompt="p", workspace="/w", tools=["Read"])
    assert "profile" not in step.model_dump()
    assert "profile" not in AgenticStep.model_json_schema()["properties"]
    assert AgenticStep(id="a", prompt="p", workspace="/w", profile="oai").profile == "oai"


# --- the executor ----------------------------------------------------------


def test_no_profile_keeps_todays_argv(tmp_path, spy):
    ex = AgenticExecutor(envelope=_envelope(), runtime="sprig", profiles=TABLE, environ=ENV)
    assert _run(ex, _step(tmp_path)).rc == 0
    for word in ("--wire", "--base-url", "--key-fd", "--scrub-env", "--tree-cmd"):
        assert word not in spy["argv"]
    assert "fds" not in spy["kw"]


def test_openai_profile_runs_sprig_whatever_agent_says(tmp_path, spy):
    ex = AgenticExecutor(envelope=_envelope(), runtime="claude", profiles=TABLE, environ=ENV)
    res = _run(ex, _step(tmp_path, profile="oai", preferred_model="qwen3-coder"))
    assert res.rc == 0, res.stdout
    a = spy["argv"]
    assert _flag(a, "--wire") == "openai"
    assert _flag(a, "--base-url") == "http://127.0.0.1:9/v1"
    assert _flag(a, "--model") == "qwen3-coder"
    assert _flag(a, "--key-fd") == "3"
    assert _flag(a, "--scrub-env") == "NOT_SET_HERE,OAI_KEY"
    assert spy["kw"]["fds"] == {3: KEY.encode()}
    env = spy["kw"]["env"]
    assert "OAI_KEY" not in env and "HOME" in env
    assert KEY not in json.dumps(a)


def test_model_comes_from_step_then_profile_then_default(tmp_path, spy):
    ex = AgenticExecutor(envelope=_envelope(), runtime="sprig", profiles=TABLE, environ=ENV)
    _run(ex, _step(tmp_path, profile="local"))
    assert _flag(spy["argv"], "--model") == "llama3.3:70b"
    _run(ex, _step(tmp_path, profile="plain"))
    assert _flag(spy["argv"], "--model") == "haiku"
    assert _flag(spy["argv"], "--base-url") == "https://api.anthropic.com"
    assert "--key-fd" not in spy["argv"]


def test_profiled_child_gets_no_ambient_key_or_redirect(tmp_path, spy):
    environ = {
        **ENV,
        "ANTHROPIC_API_KEY": "sk-ambient",
        "ANTHROPIC_BASE_URL": "http://evil",
        "SPRIG_BACKEND": "api",
        "SPRIG_MODEL": "x",
    }
    ex = AgenticExecutor(envelope=_envelope(), runtime="sprig", profiles=TABLE, environ=environ)
    _run(ex, _step(tmp_path, profile="plain"))
    for name in agent_profiles.AMBIENT:
        assert name not in spy["kw"]["env"]


@pytest.mark.parametrize(
    "profile,text",
    [
        ("zzz", "no agent profile named 'zzz'"),
        ("nokey", "agent profile 'nokey' names NOT_SET_HERE, which is not set"),
    ],
)
def test_profile_failures_fail_the_step(tmp_path, spy, profile, text):
    ex = AgenticExecutor(envelope=_envelope(), runtime="sprig", profiles=TABLE, environ=ENV)
    res = _run(ex, _step(tmp_path, profile=profile))
    assert res.rc == 1 and text in res.stdout
    assert "argv" not in spy


def test_spawn_in_the_wall_sends_the_bundle_on_fd_4(tmp_path, spy):
    env = _envelope(mcp_allowlist=["sprig/spawn"])
    ex = AgenticExecutor(envelope=env, runtime="sprig", profiles=TABLE, environ=ENV)
    res = _run(ex, _step(tmp_path, tools=["Read", "Spawn"]))
    assert res.rc == 0, res.stdout
    a = spy["argv"]
    assert _flag(a, "--tools") == "read,spawn"
    assert _flag(a, "--spawn-keys-fd") == "4"
    words = _flag(a, "--tree-cmd").split()
    assert words[1:6] == ["-I", "-m", "opendaisugi.cli", "tree", "run"]
    assert words[6::2] == ["--data-dir", "--root"]
    bundle = json.loads(spy["kw"]["fds"][4])
    assert bundle["oai"]["key"] == KEY and "bad" not in bundle


def test_spawn_needs_the_allowlist_to_name_it():
    bare = _envelope(mcp_allowlist=["github/*"]).permissions
    step = AgenticStep(id="s", prompt="p", workspace="/work/x", tools=["Spawn"])
    assert _check_agentic_step(step, bare)
    assert not _check_agentic_step(step, _envelope(mcp_allowlist=["sprig/*"]).permissions)


def test_claude_runtime_drops_spawn_and_key_names(tmp_path, monkeypatch):
    seen = {}

    def _fake(prompt, *, timeout_s, model, binary, cwd, extra_args, env=None):
        seen.update(model=model, extra_args=list(extra_args), env=env)
        return json.dumps({"result": "ok", "is_error": False, "usage": {}})

    monkeypatch.setattr(agentic_executor, "call_claude_p_sync", _fake)
    env = _envelope(mcp_allowlist=["sprig/spawn"])
    ex = AgenticExecutor(envelope=env, profiles=TABLE, environ=ENV)
    assert _run(ex, _step(tmp_path, tools=["Read", "Spawn"], profile="cc")).rc == 0
    assert seen["model"] == "opus"
    assert _flag(seen["extra_args"], "--allowedTools") == "Read"
    assert "OAI_KEY" not in seen["env"] and "NOT_SET_HERE" not in seen["env"]


def test_claude_runtime_with_no_key_names_still_scrubs_keys(tmp_path, monkeypatch):
    seen = {}

    def _fake(prompt, *, timeout_s, model, binary, cwd, extra_args, env):
        seen["env"] = env
        return json.dumps({"result": "ok", "is_error": False, "usage": {}})

    monkeypatch.setattr(agentic_executor, "call_claude_p_sync", _fake)
    environ = {"PATH": "/bin", "ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://g"}
    ex = AgenticExecutor(
        envelope=_envelope(), profiles={"cc": {"wire": "claude-code"}}, environ=environ
    )
    assert _run(ex, _step(tmp_path)).rc == 0
    assert seen["env"] == {"PATH": "/bin", "ANTHROPIC_BASE_URL": "http://g"}


# --- the descriptors, through a real process -------------------------------

FAKE = r"""#!{PY}
import json, os, sys
out = {"argv": sys.argv[1:], "env": sorted(os.environ)}
fds = {}
for n in range(3, 12):
    try:
        os.fstat(n)
    except OSError:
        continue
    fds[n] = len(os.read(n, 65536))
out["fds"] = fds
sys.stdin.read()
with open(os.environ["SPY_OUT"], "w") as f:
    json.dump(out, f)
print(json.dumps({"answer": "ok", "turns": 2, "usage": {}}))
"""


def test_key_reaches_fd_3_and_nothing_else_leaks(tmp_path):
    fake = tmp_path / "sprig"
    fake.write_text(FAKE.replace("{PY}", sys.executable))
    fake.chmod(fake.stat().st_mode | stat.S_IEXEC)
    work = tmp_path / "w"
    work.mkdir()
    environ = {**ENV, "PATH": os.environ["PATH"], "SPY_OUT": str(tmp_path / "out.json")}
    ex = AgenticExecutor(
        envelope=_envelope(mcp_allowlist=["sprig/spawn"]),
        runtime="sprig",
        sprig_binary=str(fake),
        profiles=TABLE,
        environ=environ,
    )
    res = _run(ex, _step(work, profile="oai", tools=["Read", "Spawn"]))
    assert res.rc == 0, res.stdout
    out = json.loads(Path(tmp_path / "out.json").read_text())
    bundle = agent_profiles.resolve(TABLE, ENV).bundle()
    assert out["fds"] == {"3": len(KEY), "4": len(bundle)}
    assert "OAI_KEY" not in out["env"]
    assert KEY not in json.dumps(out["argv"])

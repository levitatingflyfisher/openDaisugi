"""`daisugi agent-profiles bundle --fd N`: the operator's table, with its
keys, written once to a descriptor. It is for an operator-side root starter,
so the gate denies it to an agent."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from opendaisugi.agent_profiles import operator_bundle, resolve
from opendaisugi.tree_rule import names_tree_write

SENTINEL = "sk-SENTINEL-bundle"
SRC = str(Path(__file__).resolve().parent.parent / "src")

TABLE = """agent_profiles:
  desk:
    wire: openai
    base_url: http://127.0.0.1:1/v1
    key_env: DESK_API_KEY
  cloud:
    wire: anthropic
    key_env: CLOUD_API_KEY
"""


def _run(tmp_path: Path, config: str | None, env: dict[str, str], fd_arg: str | None = None):
    home = tmp_path / "home"
    (home / ".opendaisugi").mkdir(parents=True)
    if config is not None:
        (home / ".opendaisugi" / "config.yaml").write_text(config, encoding="utf-8")
    r, w = os.pipe()
    full_env = {
        "HOME": str(home),
        "PATH": "/usr/bin:/bin",
        "PYTHONPATH": SRC,
        "CUDA_VISIBLE_DEVICES": "",
        **env,
    }
    try:
        proc = subprocess.run(
            [
                sys.executable,
                "-m",
                "opendaisugi.cli",
                "agent-profiles",
                "bundle",
                "--fd",
                fd_arg or str(w),
            ],
            env=full_env,
            capture_output=True,
            pass_fds=(w,),
            check=False,
        )
    finally:
        os.close(w)
    data = b""
    while chunk := os.read(r, 65536):
        data += chunk
    os.close(r)
    return proc, data


def test_no_table_gives_the_default_profile_only(tmp_path):
    proc, data = _run(tmp_path, None, {})
    assert proc.returncode == 0
    assert proc.stdout == b"" and proc.stderr == b""
    assert list(json.loads(data)) == ["default"]


def test_the_key_goes_to_the_descriptor_and_nowhere_else(tmp_path):
    proc, data = _run(tmp_path, TABLE, {"DESK_API_KEY": SENTINEL, "CLOUD_API_KEY": SENTINEL + "2"})
    assert proc.returncode == 0
    assert SENTINEL.encode() in data
    assert SENTINEL.encode() not in proc.stdout + proc.stderr
    assert proc.stdout == b""


def test_a_key_env_that_is_not_set_is_left_out_with_one_stderr_line(tmp_path):
    proc, data = _run(tmp_path, TABLE, {"DESK_API_KEY": SENTINEL})
    assert proc.returncode == 0
    assert sorted(json.loads(data)) == ["default", "desk"]
    lines = proc.stderr.decode().splitlines()
    assert lines == ["agent profile 'cloud' is left out: CLOUD_API_KEY is not set"]
    assert SENTINEL not in proc.stderr.decode()


def test_a_descriptor_that_is_not_open_is_an_error_that_leaks_no_key(tmp_path):
    proc, data = _run(
        tmp_path, TABLE, {"DESK_API_KEY": SENTINEL, "CLOUD_API_KEY": SENTINEL}, fd_arg="-1"
    )
    assert proc.returncode == 2
    assert data == b""
    assert SENTINEL.encode() not in proc.stdout + proc.stderr


def test_operator_bundle_lists_each_left_out_profile_once():
    profiles = resolve(
        {
            "a": {"wire": "openai", "key_env": "A_API_KEY"},
            "b": {"wire": "nope"},
            "c": {"wire": "claude-code"},
        },
        {},
    )
    data, notes = operator_bundle(profiles)
    assert sorted(json.loads(data)) == ["c", "default"]
    assert notes == [
        "agent profile 'a' is left out: A_API_KEY is not set",
        "agent profile 'b' is left out: wire must be claude-code, anthropic or openai",
    ]


@pytest.mark.parametrize(
    "command",
    [
        "daisugi agent-profiles bundle --fd 3",
        "/usr/bin/python3 -m opendaisugi.cli agent-profiles bundle --fd 4",
        "uv run --no-sync python -m opendaisugi agent-profiles bundle --fd 1",
        "sh -c 'daisugi agent-profiles bundle --fd 3'",
        "daisugi --data-dir /d agent-profiles bundle --fd 3",
        "daisugi agent-profiles list --json",
        "daisugi agent-profiles list",
    ],
)
def test_the_gate_denies_the_verb_to_an_agent(command):
    assert names_tree_write(command)


@pytest.mark.parametrize(
    "command",
    ["daisugi agent-profiles", "echo agent-profiles bundle", "daisugi agent-profile bundle"],
)
def test_other_lines_are_left_alone(command):
    assert not names_tree_write(command)


@pytest.mark.parametrize("fd", ["0", "1", "2"])
def test_the_standard_descriptors_are_refused_and_no_key_is_printed(tmp_path, fd):
    proc, data = _run(tmp_path, TABLE, {"DESK_API_KEY": SENTINEL}, fd_arg=fd)
    assert proc.returncode == 2
    assert data == b""
    assert SENTINEL.encode() not in proc.stdout + proc.stderr
    assert b"standard input, output or error" in proc.stderr

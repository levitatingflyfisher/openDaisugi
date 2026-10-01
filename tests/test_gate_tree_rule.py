"""No agent changes the delegation tree: registering an envelope, and the
tree verbs that write, are hard denies. Reading the tree passes."""

from __future__ import annotations

import json

import pytest

from opendaisugi.gate import gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission
from opendaisugi.tree_rule import REFUSAL, names_tree_write

HITS = [
    "daisugi gate register env.json",
    "daisugi gate register env.json --session kid --parent top",
    "/usr/local/bin/daisugi gate register e.yaml",
    "uv run --no-sync python -m opendaisugi gate register e.yaml",
    "daisugi --root /g gate --x register e.json",
    "daisugi tree root e.json --session top",
    "daisugi tree spawn e.json --parent top --session kid",
    "daisugi tree end kid --tokens-used 3",
    "daisugi tree answer ask_0123456789ab allow",
    "sh -c 'daisugi tree spawn e.json'",
    "dai\\sugi tr'ee' spa\"wn\"",
    "daisugi tree status; daisugi gate register e.json",
    "echo daisugi tree end",
    "daisugi gate init --force",
    "daisugi gate init --session kid --workspace /w",
    "daisugi start claude",
    "uv run daisugi --data-dir /d start",
    "daisugi-py start",
    "npm test && daisugi start",
]

MISSES = [
    "daisugi tree check p.json c.json",
    "daisugi tree status --json",
    "daisugi gate check --mode enforce",
    "daisugi gate status",
    "daisugi register gate",
    "tree spawn daisugi",
    "daisugi-helper tree spawn",
    "daisugi trees spawn",
    "daisugi tree check end.json root.json",
    "daisugi registry init /r",
    "npm start",
    # start is read per simple command: npm's start is not daisugi's.
    "daisugi status && npm start",
    "daisugi status; yarn start",
    "daisugi init",
    "ls",
]


@pytest.mark.parametrize("line", HITS)
def test_a_line_that_writes_the_tree_is_a_hit(line):
    assert names_tree_write(line)


@pytest.mark.parametrize("line", MISSES)
def test_a_line_that_reads_the_tree_is_not_a_hit(line):
    assert not names_tree_write(line)


@pytest.fixture
def root(tmp_path):
    r = tmp_path / "gate"
    wide = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["daisugi"]),
    )
    register_envelope(wide, root=r)
    return r


def _call(cmd: str) -> bytes:
    return json.dumps(
        {"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": cmd}, "cwd": "/w"}
    ).encode()


@pytest.mark.parametrize("mode", ["enforce", "audit"])
def test_the_gate_denies_it_even_when_the_envelope_allows_daisugi(root, mode):
    out = gate_and_contract(
        _call("daisugi gate register e.json"), root=root, fmt="claude", mode=mode
    )
    d = out.decision
    assert d.would_deny and d.pane_rule and d.reason == REFUSAL
    assert not d.allow and out.exit_code == 2


def test_the_gate_lets_a_read_of_the_tree_through(root):
    out = gate_and_contract(_call("daisugi tree status"), root=root, fmt="claude", mode="enforce")
    assert out.exit_code == 0

"""No agent records a ranking vote: ``daisugi rank record`` is a hard deny.

A vote recorded as the owner outweighs the model judges, so an agent that
recorded one for its own attempt would decide its own ranking. The rule
runs before any envelope, and no operator ask turns it.
"""

from __future__ import annotations

import json

import pytest

from opendaisugi.gate import gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission
from opendaisugi.rank_rule import REFUSAL, names_rank_record

HITS = [
    "daisugi rank record --ranking R --a X --b Y --outcome a --shown ab --judge owner",
    "daisugi rank record",
    "/usr/local/bin/daisugi rank record --judge owner",
    "~/.local/bin/opendaisugi rank record",
    "env FOO=1 daisugi rank record",
    "uv run daisugi rank record --judge owner",
    "uv run --no-sync python -m opendaisugi rank record",
    "python -m opendaisugi.cli rank record",
    "sh -c 'daisugi rank record --judge owner'",
    'bash -c "cd /w && daisugi rank record"',
    "daisugi --data-dir /d rank --x record",
    "'daisugi' 'rank' 'record'",
    "dai\\sugi rank rec\\ord",
    "'dai'sugi \"ra\"nk record",
    "ls; daisugi rank record",
    "true && daisugi\trank\nrecord",
    "daisugi rank record",
    "echo daisugi rank record",
]

MISSES = [
    "daisugi rank list",
    "daisugi rank show --ranking R",
    "daisugi rank judge --ranking R",
    "daisugi record rank",
    "rank record daisugi",
    "daisugi-helper rank record",
    "mydaisugi rank record",
    "daisugi ranked record",
    "daisugi rank recorder",
    "grep 'rank record' daisugi.log",
    "ls",
]


@pytest.mark.parametrize("line", HITS)
def test_a_line_that_runs_rank_record_is_a_hit(line):
    assert names_rank_record(line)


@pytest.mark.parametrize("line", MISSES)
def test_a_near_miss_is_not_a_hit(line):
    assert not names_rank_record(line)


@pytest.fixture
def root(tmp_path):
    r = tmp_path / "gate"
    wide = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["daisugi", "uv", "sh"]),
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
        _call("daisugi rank record --judge owner"), root=root, fmt="claude", mode=mode
    )
    d = out.decision
    assert d.would_deny and d.pane_rule and d.reason == REFUSAL
    assert not d.allow  # a hard-deny rule denies in every mode
    assert out.exit_code == 2


def test_the_gate_still_allows_a_near_miss(root):
    out = gate_and_contract(_call("daisugi rank list"), root=root, fmt="claude", mode="enforce")
    assert out.exit_code == 0

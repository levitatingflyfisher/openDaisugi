"""No agent changes how the gate enforces.

``daisugi gate disarm`` turns the gate off, ``gate arm`` turns it back on,
``gate serve`` starts the process the hooks trust for a verdict, and
``install`` and ``graft install|remove`` rewrite the hooks and the graft
rules. These verbs are the operator's. The gate denies an agent's shell
line that runs one, before any envelope check, in every mode, and no
operator ask turns that deny.

The rule reads the parsed command: each simple command of the line, its
words as the shell splits them, and the verbs as arguments of a
``daisugi`` command. A verb named by another command is not a hit.
"""

from __future__ import annotations

import json

import pytest

from opendaisugi import owner_rule
from opendaisugi.gate import gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission
from opendaisugi.owner_rule import REFUSAL, names_gate_change, runs_verb

HITS = [
    "daisugi gate disarm",
    "daisugi gate arm",
    "daisugi gate disarm --root /g",
    "daisugi gate serve",
    "daisugi install --gate --enforce",
    "daisugi install --uninstall",
    "daisugi graft install --state active",
    "daisugi graft remove --id big-read",
    "/usr/local/bin/daisugi gate disarm",
    "./daisugi gate disarm",
    "daisugi-py gate disarm",
    "~/.local/bin/daisugi-py gate arm",
    "uv run daisugi gate disarm",
    "uv run --no-sync python -m opendaisugi.cli gate disarm",
    "python3 -m opendaisugi gate disarm",
    "env A=1 daisugi gate disarm",
    "nohup daisugi gate serve &",
    "daisugi --plain gate disarm",
    "daisugi -q gate disarm",
    "daisugi gate --x disarm",
    "ls && daisugi gate disarm",
    "true; daisugi gate disarm",
    "daisugi status || daisugi gate disarm",
    "x=$(daisugi gate disarm)",
    "echo $(daisugi gate disarm)",
    "sh -c 'daisugi gate disarm'",
    'bash -lc "cd /w && daisugi gate disarm"',
    "'daisugi' 'gate' 'disarm'",
    "dai\\sugi gate dis\\arm",
    "'dai'sugi gate disarm",
    "daisugi gate disarm; (",
    "echo daisugi gate disarm",
    # Forms shlex splits differently from the shell: read by word order.
    "daisugi gate dis\\\narm",
    "daisugi \\\ngate disarm",
    "daisugi gate $'disarm'",
    'daisugi gate $"disarm"',
]

MISSES = [
    "daisugi gate status",
    "daisugi gate check --mode enforce",
    "daisugi gate settings --enforce",
    "daisugi gate report",
    "daisugi graft status",
    "daisugi status && npm install",
    "npm install && daisugi status",
    "pip install daisugi",
    "daisugi status; gate disarm",
    "gate disarm daisugi",
    "daisugi-helper gate disarm",
    "mydaisugi gate disarm",
    "daisugi gates disarm",
    "daisugi pathways show disarm",
    "daisugi help gate",
    "git commit -m 'gate: disarm in tests'",
    "grep 'gate disarm' daisugi.log",
    "ls",
]


@pytest.mark.parametrize("line", HITS)
def test_a_line_that_changes_how_the_gate_enforces_is_a_hit(line):
    assert names_gate_change(line)


@pytest.mark.parametrize("line", MISSES)
def test_a_line_that_does_not_is_not_a_hit(line):
    assert not names_gate_change(line)


def test_start_after_another_command_is_not_daisugi_start():
    assert not runs_verb("daisugi status && npm start", owner_rule.TREE_VERBS)
    assert not runs_verb("npm start", owner_rule.TREE_VERBS)
    assert runs_verb("npm test && daisugi start", owner_rule.TREE_VERBS)
    assert runs_verb("daisugi --plain start --enforce", owner_rule.TREE_VERBS)


def test_a_line_that_does_not_parse_falls_back_to_the_word_order():
    # A parse error: the simple commands are not known, so the old rule
    # reads the whole line: a daisugi word, then the verbs in order.
    assert runs_verb("daisugi status && npm start (", owner_rule.TREE_VERBS)
    assert not runs_verb("npm start (", owner_rule.TREE_VERBS)


def test_a_quoted_command_string_is_read_by_word_order():
    # The payload of sh -c is one word; its words are read in order.
    assert names_gate_change("sh -c 'daisugi gate disarm'")
    # So a message that names the verbs is a hit too: run the two apart.
    assert names_gate_change("git commit -m 'run daisugi gate disarm first'")


def test_unknown_words_before_the_verb_do_not_hide_it():
    # The root takes no option with a value, so a word there is not one of
    # its commands; the first known command decides.
    assert names_gate_change("daisugi --data-dir /d gate disarm")
    assert not names_gate_change("daisugi status gate disarm")


def test_the_command_table_matches_the_cli():
    from typer.main import get_command

    from opendaisugi.cli import app

    cli = get_command(app)
    assert set(cli.commands) == owner_rule.COMMANDS
    for group, subs in owner_rule.SUBCOMMANDS.items():
        assert set(cli.commands[group].commands) == subs, group
    for verbs in (
        owner_rule.RANK_VERBS,
        owner_rule.TREE_VERBS,
        owner_rule.GATE_VERBS,
        owner_rule.LABEL_VERBS,
        owner_rule.PACK_VERBS,
    ):
        for v in verbs:
            assert v[0] in owner_rule.COMMANDS
            if len(v) > 1:
                assert v[1] in owner_rule.SUBCOMMANDS[v[0]]


@pytest.fixture
def root(tmp_path):
    r = tmp_path / "gate"
    wide = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["daisugi", "uv", "sh", "npm"]),
    )
    register_envelope(wide, root=r)
    return r


def _call(cmd: str) -> bytes:
    return json.dumps(
        {"session_id": "s1", "tool_name": "Bash", "tool_input": {"command": cmd}, "cwd": "/w"}
    ).encode()


@pytest.mark.parametrize("mode", ["enforce", "audit"])
@pytest.mark.parametrize("line", ["daisugi gate disarm", "daisugi gate arm", "daisugi install"])
def test_the_gate_denies_it_even_when_the_envelope_allows_daisugi(root, mode, line):
    out = gate_and_contract(_call(line), root=root, fmt="claude", mode=mode)
    d = out.decision
    assert d.would_deny and d.pane_rule and d.reason == REFUSAL
    assert not d.allow
    assert out.exit_code == 2


def test_the_gate_lets_a_read_of_the_gate_through(root):
    out = gate_and_contract(_call("daisugi gate status"), root=root, fmt="claude", mode="enforce")
    assert out.exit_code == 0


def test_npm_start_after_a_daisugi_read_passes(root):
    out = gate_and_contract(
        _call("daisugi status && npm start"), root=root, fmt="claude", mode="enforce"
    )
    assert not out.decision.pane_rule, out.decision.reason


@pytest.mark.parametrize(
    "line,verbs",
    [
        ("daisugi \\\nrank record", owner_rule.RANK_VERBS),
        ("daisugi $'rank' record", owner_rule.RANK_VERBS),
        ('daisugi $"rank" record', owner_rule.RANK_VERBS),
        ("dai\\\nsugi start", owner_rule.TREE_VERBS),
        ("daisugi router $'label' s1 pass", owner_rule.LABEL_VERBS),
    ],
)
def test_a_line_continuation_or_dollar_quote_does_not_hide_a_verb(line, verbs):
    assert runs_verb(line, verbs)


def test_a_dollar_quote_elsewhere_does_not_make_npm_start_a_hit():
    assert not runs_verb("echo $'x' && npm start", owner_rule.TREE_VERBS)


# A pack install, remove or bundle fetches code and changes what the box
# runs; it is the operator's (PK-R-12). Listing, status, a job in an
# installed pack and LoRA training stay with the envelope.
@pytest.mark.parametrize(
    "line",
    [
        "daisugi pack install train",
        "daisugi pack install train --offline --bundle /b.tar",
        "daisugi pack remove train",
        "daisugi pack bundle train --out /x",
        "cd /w && daisugi pack install train",
    ],
)
def test_the_gate_denies_a_pack_change(root, line):
    out = gate_and_contract(_call(line), root=root, fmt="claude", mode="enforce")
    d = out.decision
    assert d.would_deny and d.pane_rule and d.reason == owner_rule.PACK_REFUSAL
    assert out.exit_code == 2


@pytest.mark.parametrize(
    "line",
    [
        "daisugi pack list",
        "daisugi pack status train",
        "daisugi pack run train selftest",
        "daisugi lora train --jsonl a --output b",
        "daisugi pack status gate disarm",
    ],
)
def test_the_gate_leaves_the_other_pack_verbs_to_the_envelope(root, line):
    out = gate_and_contract(_call(line), root=root, fmt="claude", mode="enforce")
    assert not out.decision.pane_rule, out.decision.reason

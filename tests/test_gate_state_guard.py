"""The gate's rule that no agent writes the gate's own state.

The gate root holds the disarm marker, the envelopes, the graft rules and
the audit logs; the data dir holds the files the meters and the review
queue trust (router labels and delegations, the journal, the tree ledger,
the gateway's turns and answers, weave's run files, the envelope cache).
An envelope that grants writes everywhere must not grant these. The gate
denies every write it can place under them, before any envelope check, in
every mode, and no ask turns it. Reads stay allowed.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from opendaisugi.gate import evaluate_call
from opendaisugi.gate_state_rule import data_dirs, protected_dirs, refusal
from opendaisugi.models import Envelope, Permission

SHELL = [
    "cat",
    "cd",
    "chmod",
    "cp",
    "curl",
    "dd",
    "echo",
    "find",
    "git",
    "grep",
    "head",
    "install",
    "jq",
    "ln",
    "ls",
    "mkdir",
    "mv",
    "python",
    "rm",
    "rsync",
    "sed",
    "sh",
    "tail",
    "tar",
    "tee",
    "touch",
    "truncate",
    "wc",
]


@pytest.fixture
def allow_all() -> Envelope:
    return Envelope(
        generated_by="test",
        task="allow everything",
        permissions=Permission(
            file_read=["/**"],
            file_write=["/**"],
            shell=True,
            shell_allowlist=SHELL,
            shell_allow_decomposition=True,
            mcp_allowlist=["fs/write_file", "fs/read_file"],
        ),
    )


@pytest.fixture
def root(tmp_path, monkeypatch) -> Path:
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    (tmp_path / "home").mkdir()
    (tmp_path / "data" / "gate" / "envelopes").mkdir(parents=True)
    (tmp_path / "data" / "router").mkdir()
    (tmp_path / "data2").mkdir()
    (tmp_path / "work").mkdir()
    return tmp_path / "data" / "gate"


def _call(tool: str, tool_input: dict, cwd: str | None = "/work") -> dict:
    out = {"tool_name": tool, "tool_input": tool_input, "session_id": "s1"}
    if cwd is not None:
        out["cwd"] = cwd
    return out


def _bash(line: str, root: Path, allow_all: Envelope, *, cwd: str | None = "/work", mode="enforce"):
    return evaluate_call(_call("Bash", {"command": line}, cwd=cwd), allow_all, mode=mode, root=root)


def test_the_refusal_names_the_directory():
    assert (
        refusal("/x/gate")
        == "the gate's own state is under /x/gate; only the operator writes there."
    )


def test_the_protected_dirs(root):
    data = root.parent
    home = Path.home() / ".opendaisugi"
    got = protected_dirs(root)
    for d in (
        root,
        data / "gate",
        data / "router",
        data / "journal",
        data / "tree",
        data / "gateway",
        data / "weave",
        data / "envelope_cache.db",
        home / "gate",
        home / "router",
        home / "journal",
    ):
        assert str(d) in got, d
    assert str(data / "pathways.db") not in got
    assert str(data / "config.yaml") not in got


def test_a_root_not_named_gate_protects_only_itself(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    root = tmp_path / "daisugi-agentic-gate-x1"
    root.mkdir()
    assert data_dirs(root) == [str(tmp_path / "home" / ".opendaisugi")]
    got = protected_dirs(root)
    assert str(root) in got
    assert str(tmp_path / "router") not in got


# -- the tool writes ---------------------------------------------------------


@pytest.mark.parametrize("mode", ["enforce", "audit"])
@pytest.mark.parametrize(
    "rel",
    ["gate/DISARMED", "gate/envelopes/default.json", "gate/grafts/g.json", "router/labels.jsonl"],
)
def test_a_write_tool_call_into_the_state_is_denied(allow_all, root, mode, rel):
    path = str(root.parent / rel)
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode=mode, root=root)
    assert d.allow is False
    assert d.pane_rule is True
    assert d.reason.startswith("the gate's own state is under "), d.reason


def test_the_reason_names_the_protected_dir(allow_all, root):
    d = evaluate_call(
        _call("Write", {"file_path": str(root / "DISARMED")}), allow_all, mode="enforce", root=root
    )
    assert d.reason == refusal(str(root))
    d = evaluate_call(
        _call("Write", {"file_path": str(root.parent / "router" / "labels.jsonl")}),
        allow_all,
        mode="enforce",
        root=root,
    )
    assert d.reason == refusal(str(root.parent / "router"))


@pytest.mark.parametrize("tool", ["Edit", "MultiEdit"])
def test_the_edit_tools_are_denied(allow_all, root, tool):
    path = str(root / "envelopes" / "default.json")
    d = evaluate_call(_call(tool, {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(root))


def test_the_default_data_dir_is_guarded_too(allow_all, root):
    path = str(Path.home() / ".opendaisugi" / "tree" / "ledger.jsonl")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(Path.home() / ".opendaisugi" / "tree"))


@pytest.mark.parametrize("var", ["OPENDAISUGI_HOME", "XDG_DATA_HOME"])
def test_a_data_home_moved_by_the_environment_is_guarded(
    allow_all, root, tmp_path, monkeypatch, var
):
    """OPENDAISUGI_HOME and XDG_DATA_HOME can move the data home
    (opendaisugi.datahome). Each place it can move to is guarded, whether
    or not it exists yet."""
    moved = tmp_path / "moved"
    monkeypatch.setenv(var, str(moved))
    data = moved if var == "OPENDAISUGI_HOME" else moved / "opendaisugi"
    assert str(data) in data_dirs(root)
    path = str(data / "gate" / "envelopes" / "default.json")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(data / "gate"))
    # The legacy directory stays guarded too.
    assert str(Path.home() / ".opendaisugi") in data_dirs(root)


def test_a_symlinked_parent_is_followed(allow_all, root, tmp_path):
    os.symlink(root, tmp_path / "work" / "g")
    path = str(tmp_path / "work" / "g" / "DISARMED")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(root))


def test_dotdot_is_followed(allow_all, root):
    path = str(root.parent / "data2" / ".." / "gate" / "DISARMED")
    d = evaluate_call(_call("Write", {"file_path": path}), allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(root))


def test_an_operator_ask_cannot_turn_it(allow_all, root):
    # pane_rule True: the gate's ask path never offers this deny to the operator.
    d = evaluate_call(
        _call("Write", {"file_path": str(root / "DISARMED")}), allow_all, mode="enforce", root=root
    )
    assert d.pane_rule is True


# -- reads and near misses stay allowed --------------------------------------


def test_a_read_tool_call_is_allowed(allow_all, root):
    d = evaluate_call(
        _call("Read", {"file_path": str(root / "envelopes" / "default.json")}),
        allow_all,
        mode="enforce",
        root=root,
    )
    assert d.allow is True


@pytest.mark.parametrize(
    "line",
    [
        "cat {root}/envelopes/default.json",
        "ls -la {root}",
        "tail -n 5 {data}/router/labels.jsonl | jq .outcome",
        "wc -l {data}/router/delegations.jsonl",
        "grep -c pass {data}/router/labels.jsonl",
        "head {root}/audit/s1.jsonl > /work/out.txt",
        "find {root} -name '*.json'",
        "cp {root}/envelopes/default.json /work/env.json",
    ],
)
def test_a_read_in_the_shell_is_allowed(allow_all, root, line):
    d = _bash(line.format(root=root, data=root.parent), root, allow_all)
    assert d.allow is True, (line, d.reason)


@pytest.mark.parametrize(
    "line",
    [
        "touch {data}2/DISARMED",
        "echo x > {data}2/gate/DISARMED",
        "python x.py {data}2/",
        "git -C {data}2 status",
    ],
)
def test_a_sibling_with_the_same_prefix_is_allowed(allow_all, root, line):
    d = _bash(line.format(data=root.parent), root, allow_all)
    assert d.allow is True, (line, d.reason)


def test_a_write_beside_the_state_in_the_data_dir_is_allowed(allow_all, root):
    d = _bash(f"touch {root.parent}/notes.txt", root, allow_all)
    assert d.allow is True, d.reason


# -- the shell writes ----------------------------------------------------------


@pytest.mark.parametrize(
    "line",
    [
        "touch {root}/DISARMED",
        "echo x > {root}/DISARMED",
        "echo '{{}}' >> {data}/router/labels.jsonl",
        "mkdir -p {root}/grafts",
        "rm -f {root}/envelopes/default.json",
        "mv /work/e.json {root}/envelopes/default.json",
        "cp /work/e.json {root}/envelopes/default.json",
        "cp /work/e.json {root}/envelopes/",
        "ln -s /work/e.json {root}/envelopes/default.json",
        "chmod 777 {root}/envelopes/default.json",
        "truncate -s 0 {data}/router/delegations.jsonl",
        "tee {root}/DISARMED < /dev/null",
        "dd if=/dev/zero of={root}/DISARMED count=0",
        "install -m 600 /work/e.json {root}/envelopes/default.json",
        "rsync -a /work/grafts/ {root}/grafts/",
        "sed -i 's/enforce/audit/' {root}/envelopes/default.json",
        "git -C {root} checkout .",
        "tar -xf /work/x.tar -C {root}",
        "python -c \"open('{root}/DISARMED','w')\"",
        "sh -c 'touch {root}/DISARMED'",
        "curl -o {root}/grafts/g.json http://example.com/g",
        "find {root} -name '*.json' -delete",
        "touch ~/.opendaisugi/gate/DISARMED",
        "touch $HOME/.opendaisugi/gate/DISARMED",
        "cd {root} && touch DISARMED",
        "cd {data} && rm -r gate",
        "touch {data}/journal/traces/x.yaml",
        "echo x > {data}/gateway/turns.jsonl",
        "echo x > {data}/weave/run.jsonl",
        "echo x > {data}/envelope_cache.db",
        "echo x > {data}/tree/ledger.jsonl",
    ],
)
@pytest.mark.parametrize("mode", ["enforce", "audit"])
def test_a_shell_write_into_the_state_is_denied(allow_all, root, line, mode):
    d = _bash(line.format(root=root, data=root.parent), root, allow_all, mode=mode)
    assert d.allow is False, line
    assert d.reason.startswith("the gate's own state is under "), (line, d.reason)


@pytest.mark.parametrize(
    "line",
    [
        "rm -rf {data}",
        "mv {data} /work/old",
        "mv {root} /work/old",
        "chmod -R 777 {data}",
    ],
)
def test_a_write_to_a_directory_above_the_state_is_denied(allow_all, root, line):
    d = _bash(line.format(root=root, data=root.parent), root, allow_all)
    assert d.allow is False, line
    assert d.reason.startswith("the gate's own state is under "), (line, d.reason)


def test_a_relative_write_from_inside_the_gate_root_is_denied(allow_all, root):
    d = _bash("touch DISARMED", root, allow_all, cwd=str(root))
    assert d.reason == refusal(str(root))


def test_a_read_from_inside_the_gate_root_is_allowed(allow_all, root):
    d = _bash("ls envelopes", root, allow_all, cwd=str(root))
    assert d.allow is True, d.reason


def test_a_hard_link_of_a_state_file_is_denied(allow_all, root):
    d = _bash(f"ln {root}/envelopes/default.json /work/x", root, allow_all)
    assert d.allow is False
    assert d.reason == refusal(str(root))


def test_a_cp_link_of_a_state_file_is_denied(allow_all, root):
    d = _bash(f"cp -l {root}/envelopes/default.json /work/x", root, allow_all)
    assert d.reason == refusal(str(root))


def test_a_symlink_then_a_write_in_one_line_is_denied(allow_all, root):
    d = _bash(f"ln -s {root}/DISARMED /work/x && echo x > /work/x", root, allow_all)
    assert d.reason == refusal(str(root))


def test_an_unplaced_write_on_a_line_that_names_the_data_dir_is_denied(allow_all, root):
    d = _bash(f'cp /work/x "{root.parent}/$NAME"', root, allow_all)
    assert d.allow is False
    assert d.reason == refusal(str(root.parent))


def test_an_unplaced_write_on_a_line_that_names_no_data_dir_is_left_alone(allow_all, root):
    d = _bash('cp /work/x "/work/$NAME"', root, allow_all)
    assert not d.reason.startswith("the gate's own state"), d.reason


def test_an_unknown_command_that_names_no_data_dir_is_left_alone(allow_all, root):
    d = _bash("python x.py /work/out", root, allow_all)
    assert d.allow is True, d.reason


# -- MCP -----------------------------------------------------------------------


def test_an_mcp_argument_under_the_state_is_denied(allow_all, root):
    call = _call("mcp__fs__write_file", {"path": str(root / "DISARMED"), "content": "x"})
    d = evaluate_call(call, allow_all, mode="enforce", root=root)
    assert d.reason == refusal(str(root))


def test_an_mcp_argument_elsewhere_is_left_alone(allow_all, root):
    call = _call("mcp__fs__write_file", {"path": "/work/x", "content": "x"})
    d = evaluate_call(call, allow_all, mode="enforce", root=root)
    assert d.allow is True, d.reason

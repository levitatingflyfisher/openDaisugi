"""The gate refuses a recursive search that reaches coppice's secrets.

A search rooted at a data directory's ancestor reads every secret under
it. These cases cover the ways a root can be spelled so the gate must
place it, and the ways it cannot, where the gate refuses."""

from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope, Permission

SEARCH_REFUSAL = "this search reaches coppice's secrets. Search a narrower directory."
PANE_REFUSAL = "a pane can propose. It cannot allow."
HOME = str(Path.home())


@pytest.fixture
def allow_all() -> Envelope:
    return Envelope(
        generated_by="test",
        task="allow everything",
        permissions=Permission(
            file_read=["/**"],
            file_write=["/**"],
            shell=True,
            shell_allowlist=["*", "/*/*", "./*"],
            shell_allow_decomposition=True,
        ),
    )


def _bash(command: str, cwd: str | None = "/work") -> dict:
    p = {"tool_name": "Bash", "tool_input": {"command": command}, "session_id": "s1"}
    if cwd is not None:
        p["cwd"] = cwd
    return p


def _reason(payload: dict, env: Envelope) -> str:
    return evaluate_call(payload, env, mode="enforce").reason


# Each is a search that reaches the default data directory's secrets.
_DENIED = [
    # F1: a value word does not stand in for a root.
    ("grep -r -m 1 token", HOME),
    ("rg -t py token", HOME),
    ("rg -g '*' token", HOME),
    ("grep -r -A 2 token", HOME),
    ("grep -r --include '*' token", HOME),
    ("rg -e token", HOME),
    # F2: a line shlex cannot split is refused.
    ("echo $'it\\'s' ; rg token ~", "/work"),
    # F3: every way grep recurses, and the programs that recurse unasked.
    ("grep -d recurse token ~", "/work"),
    ("grep -drecurse token ~", "/work"),
    ("grep --directories=recurse token ~", "/work"),
    ("grep --directories recurse token ~", "/work"),
    ("grep --recur token ~", "/work"),
    ("grep --deref token ~", "/work"),
    ("zgrep -r token ~", "/work"),
    ("ugrep token ~", "/work"),
    ("ug token", HOME),
    ("ugrep -3 token", HOME),
    ("git grep --no-index token", HOME),
    ("git -C ~ grep --no-index token", "/work"),
    # F4: a glob that can match an ancestor, the data dir, or a path in it.
    ("rg token /*", "/work"),
    ("rg --hidden token ~/.*", "/work"),
    ("rg --hidden token ~/.o*", "/work"),
    ("rg token /hom?/" + Path.home().name, "/work"),
    ("rg token /[h]ome/" + Path.home().name, "/work"),
    ("rg token {" + HOME + ",/x}", "/work"),
    ("find /* -type f -exec cat {} +", "/work"),
    # F5: find's own options before its roots.
    ("find -- ~ -type f -exec cat {} +", "/work"),
    ("find -O3 ~ -type f -exec cat {} +", "/work"),
    ("find -D stat ~ -type f -exec cat {} +", "/work"),
    # F6: a cwd the gate cannot follow.
    ("cd - && rg token", "/work"),
    ("cd $HOME/x/.. && rg token", "/work"),
    ("pushd ~ && rg token", "/work"),
    ("cd - && rg token src", "/work"),
    # F7: a root the gate cannot resolve.
    ("rg token $PWD", "/work"),
    ("H=~; rg token $H", "/work"),
    ("rg token ~+", "/work"),
    ("rg token ~user", "/work"),
    ("rg token $(echo ~)", "/work"),
    ("for d in ~; do rg token $d; done", "/work"),
    # Prefixes and wrappers do not hide the search.
    ("env rg token ~", "/work"),
    ("timeout 5 rg token ~", "/work"),
    ("nice rg token ~", "/work"),
    ("\\rg token ~", "/work"),
    ("/usr/bin/rg token ~", "/work"),
    ("echo ~ | xargs rg token", "/work"),
    ("bash -c 'rg token ~'", "/work"),
    ("rg token -- ~", "/work"),
    ("rg --files ~", "/work"),
]


@pytest.mark.parametrize("command,cwd", _DENIED)
def test_a_search_that_reaches_the_secrets_is_refused(allow_all, command, cwd):
    assert _reason(_bash(command, cwd), allow_all) == SEARCH_REFUSAL, command


def test_a_pathless_search_with_no_cwd_is_refused(allow_all):
    assert _reason(_bash("rg token", cwd=None), allow_all) == SEARCH_REFUSAL


_LEFT = [
    ("rg token src", "/work"),
    ("rg -t py token src", "/work"),
    ("grep -r token /work/src", HOME),
    ("grep -rn token src lib", HOME),
    ("rg -e token /work/src", HOME),
    ("grep token ~/notes.txt", "/work"),
    ("find ~ -name '*.md'", "/work"),
    ("rg 'foo$' src", "/work"),
    ("rg token /work/*", "/work"),
    ("rg token src/*.py", "/work"),
    ("ls ~/.opendaisugi/*", "/work"),
    ("cd /work && rg token", "/"),
]


@pytest.mark.parametrize("command,cwd", _LEFT)
def test_a_narrow_search_is_left_to_the_envelope(allow_all, command, cwd):
    assert _reason(_bash(command, cwd), allow_all) != SEARCH_REFUSAL, command


def test_a_glob_that_names_a_secret_file_is_refused_by_the_pane_rule(allow_all):
    for command in (
        "cat ~/.opendaisugi/*/*/*",
        "cat ~/.opendaisugi/coppice/web/*",
        "cat ~/.opendaisugi/coppice/web/ca/*.key",
        "cp ~/.opendaisugi/coppice/voice/tok?n /work/x",
    ):
        assert _reason(_bash(command), allow_all) == PANE_REFUSAL, command


def test_a_grep_with_both_file_path_and_path_checks_both(allow_all):
    payload = {
        "tool_name": "Grep",
        "tool_input": {"pattern": ".", "file_path": "/work", "path": HOME},
        "session_id": "s1",
        "cwd": "/work",
    }
    assert _reason(payload, allow_all) == SEARCH_REFUSAL
    payload["tool_input"]["path"] = HOME + "/.opendaisugi/coppice/web"
    assert _reason(payload, allow_all) == PANE_REFUSAL


@pytest.mark.parametrize("command", ["grep -2 token ~", "grep --depth=3 token ~"])
def test_ugreps_depth_options_recurse(allow_all, command):
    assert _reason(_bash(command), allow_all) == SEARCH_REFUSAL, command


def test_a_pathless_search_from_a_cwd_the_gate_cannot_place_is_refused(allow_all):
    assert _reason(_bash("grep -r token .", cwd="user"), allow_all) == SEARCH_REFUSAL
    for cwd in (None, "user"):
        payload = {"tool_name": "Grep", "tool_input": {"pattern": "."}, "session_id": "s1"}
        if cwd is not None:
            payload["cwd"] = cwd
        assert _reason(payload, allow_all) == SEARCH_REFUSAL, cwd
    payload = {
        "tool_name": "Grep",
        "tool_input": {"pattern": ".", "path": "/work"},
        "session_id": "s1",
    }
    assert _reason(payload, allow_all) != SEARCH_REFUSAL


_MANY = "{" + ",".join(f"/a{i}" for i in range(1, 65)) + "," + HOME + "}"


@pytest.mark.parametrize(
    "command",
    [
        "rg token " + HOME[:-1] + "{" + HOME[-1] + ".." + HOME[-1] + "}",
        "rg token /{a..z}ome/" + Path.home().name,
        "rg token " + HOME[:-1] + "{01..03}",
        "rg token /home/{a..1}",
        "rg token " + _MANY,
        'sh -c "sh -c \'sh -c \\"rg token ~\\"\'"',
        'bash -c "bash -c \'bash -c \\"sh -c \\\\\\"rg token ~\\\\\\"\\"\'"',
    ],
)
def test_round_two_roots_are_refused_by_the_search_rule(allow_all, command):
    assert _reason(_bash(command), allow_all) == SEARCH_REFUSAL, command


def test_a_sequence_brace_that_names_a_secret_file_is_refused(allow_all):
    cmd = "cat ~/.opendaisugi/coppice/web/toke{m..o}"
    assert _reason(_bash(cmd), allow_all) == PANE_REFUSAL


def test_a_narrow_sequence_brace_is_left_to_the_envelope(allow_all):
    for cmd in (
        "rg token /work/src{1..3}",
        "rg token /home/{0..9..2}",
        "echo {1..5}",
        "cat /work/f{a..c}",
    ):
        assert _reason(_bash(cmd), allow_all) not in (SEARCH_REFUSAL, PANE_REFUSAL), cmd


def test_a_long_brace_that_cannot_reach_a_secret_is_left_to_the_envelope(allow_all):
    for cmd in ("echo {1..100}", "rg token /work/f{1..100}", "cat /work/f{1..100}"):
        assert _reason(_bash(cmd), allow_all) not in (SEARCH_REFUSAL, PANE_REFUSAL), cmd
    assert _reason(_bash("rg token " + HOME[:-2] + "{1..100}"), allow_all) == SEARCH_REFUSAL
    assert _reason(_bash("rg token {1..100}", cwd=HOME), allow_all) == SEARCH_REFUSAL


@pytest.mark.parametrize(
    "word,want",
    [
        ("a{1..3}", "a1 a2 a3"),
        ("{c..a}", "c b a"),
        ("{1..9..4}", "1 5 9"),
        ("{5..1..-2}", "5 3 1"),
        ("x{a,b}{1..2}", "xa1 xa2 xb1 xb2"),
        ("{}", "{}"),
    ],
)
def test_sequence_braces_expand_like_bash(word, want):
    from opendaisugi.search_rule import _brace_expand

    out: list[str] = []
    assert _brace_expand(word, out) and " ".join(out) == want


def test_a_zero_padded_sequence_is_not_expanded():
    from opendaisugi.search_rule import _brace_expand

    assert not _brace_expand("{01..03}", [])

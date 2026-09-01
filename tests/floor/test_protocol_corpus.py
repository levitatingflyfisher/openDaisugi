"""The Python client replays the coppice protocol corpus.

`harness/coppice/testdata/protocol/*.jsonl` is the wire contract as request
and expected-reply pairs. The Go test
`harness/coppice/internal/proto/conformance_test.go` replays it against a
real server. This module replays the same files, with the same rules, over
the same kind of socket, from Python. A mismatch here is a real difference
between the two clients. It is fixed in the Python side or in the Go server,
never by editing the corpus.

Each file is replayed once, on one connection, and every case in it becomes
one pytest case. A `#skip` line is reported as a pytest skip for that case
and never as a pass. When the coppice binary cannot be built here, every
case skips with the sandbox fixture's own reason.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

import pytest

from tests.floor.coppice_sandbox import (  # noqa: F401
    REPO_ROOT,
    CoppiceServer,
    CorpusConnection,
    coppice_binary,
    coppice_server,
)
from tests.floor.protocol_match import (
    SKIP_MARKER,
    Matcher,
    Mismatch,
    request_id,
)

CORPUS = REPO_ROOT / "harness" / "coppice" / "testdata" / "protocol"


@dataclass(frozen=True)
class Case:
    no: int
    request: str
    expected: str


@dataclass
class Outcome:
    """What one case did. Exactly one of the two is set for a case that did
    not pass. Both None means the reply matched."""

    skipped: str | None = None
    failure: str | None = None


def load_cases(path: Path) -> list[Case]:
    """The cases of one corpus file, with the line rules the README states:
    blank lines and `#` comments are dropped, except a `#skip` line in the
    expected position, and the rest alternate request then expected reply.
    An odd number of lines is a broken file."""
    lines = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.startswith("#") and not line.startswith(SKIP_MARKER):
            continue
        lines.append(line)
    if len(lines) % 2 != 0:
        raise ValueError(f"{path} has {len(lines)} lines. Every request needs one expected line.")
    return [
        Case(no=i // 2 + 1, request=lines[i], expected=lines[i + 1])
        for i in range(0, len(lines), 2)
    ]


def replay(server: CoppiceServer, path: Path) -> list[Outcome]:
    """Replay one file on one connection and return one Outcome per case,
    in order. A case that could not be sent, or whose reply never came,
    fails, and every case after it fails as not reached, because the
    connection state the later cases depend on is unknown from then on."""
    cases = load_cases(path)
    outcomes = [Outcome() for _ in cases]
    matcher = Matcher()
    with CorpusConnection(server.socket_path) as conn:
        for i, case in enumerate(cases):
            if case.expected.startswith(SKIP_MARKER):
                reason = case.expected[len(SKIP_MARKER) :].strip()
                outcomes[i].skipped = f"{path.name} case {case.no}: {reason}"
                continue
            try:
                req = matcher.substitute(json.loads(case.request))
                sent = json.dumps(req)
                conn.send_line(sent)
                actual = conn.read_reply()
            except (ValueError, OSError) as exc:
                outcomes[i].failure = f"{path.name} case {case.no}: {exc}"
                for later in outcomes[i + 1 :]:
                    later.failure = f"{path.name}: not reached, case {case.no} broke the replay"
                return outcomes
            expected = json.loads(case.expected)
            try:
                matcher.match(expected, actual, request_id(req))
            except Mismatch as exc:
                outcomes[i].failure = (
                    f"{path.name} case {case.no}: {exc}\n"
                    f"request:  {sent}\n"
                    f"expected: {case.expected}\n"
                    f"actual:   {json.dumps(actual)}"
                )
    return outcomes


FILES = sorted(CORPUS.glob("*.jsonl"))
PARAMS = [
    pytest.param(path, case.no, id=f"{path.name}:{case.no}")
    for path in FILES
    for case in load_cases(path)
]

_OUTCOMES: dict[Path, list[Outcome]] = {}


@pytest.fixture
def outcomes(coppice_server: CoppiceServer):
    """The outcomes of one file, replayed at most once per test run."""

    def get(path: Path) -> list[Outcome]:
        if path not in _OUTCOMES:
            _OUTCOMES[path] = replay(coppice_server, path)
        return _OUTCOMES[path]

    return get


def test_the_corpus_has_files():
    """A corpus with no files would prove nothing, so it fails, as the Go
    replay does."""
    assert FILES, f"no corpus files under {CORPUS}"


@pytest.mark.parametrize("path,case_no", PARAMS)
def test_corpus_case_replays_through_the_python_client(path: Path, case_no: int, outcomes):
    outcome = outcomes(path)[case_no - 1]
    if outcome.skipped is not None:
        pytest.skip(f"a listed gap, never a pass: {outcome.skipped}")
    if outcome.failure is not None:
        pytest.fail(outcome.failure)


def test_a_skip_line_is_a_gap_and_its_request_is_never_sent(tmp_path, coppice_server):
    """The replay does not send a skipped request. If it did, the reply
    would arrive as the next case's reply and its id would not match."""
    corpus = tmp_path / "gap.jsonl"
    corpus.write_text(
        "\n".join(
            [
                '{"id":"1","cmd":"server.status"}',
                '{"id":"$id","ok":true,"result":{"protocol":1}}',
                '{"id":"x","cmd":"server.status"}',
                "#skip a verb the server does not have yet",
                '{"id":"3","cmd":"server.status"}',
                '{"id":"$id","ok":true,"result":{"pid":"*"}}',
                "",
            ]
        ),
        encoding="utf-8",
    )
    got = replay(coppice_server, corpus)
    assert got[0] == Outcome()
    assert got[1].skipped is not None and "does not have yet" in got[1].skipped
    assert got[2] == Outcome()


def test_a_mismatch_names_the_case_and_shows_both_lines(tmp_path, coppice_server):
    corpus = tmp_path / "bad.jsonl"
    corpus.write_text(
        '{"id":"1","cmd":"server.status"}\n{"id":"$id","ok":true,"result":{"protocol":99}}\n',
        encoding="utf-8",
    )
    got = replay(coppice_server, corpus)
    assert got[0].failure is not None
    assert "bad.jsonl case 1" in got[0].failure
    assert "result.protocol" in got[0].failure
    assert "expected:" in got[0].failure and "actual:" in got[0].failure


def test_an_odd_line_count_is_a_broken_file(tmp_path):
    corpus = tmp_path / "odd.jsonl"
    corpus.write_text('{"id":"1","cmd":"server.status"}\n', encoding="utf-8")
    with pytest.raises(ValueError, match="Every request needs one expected line"):
        load_cases(corpus)


# --- the matcher, rule by rule, as conformance_match_test.go checks it -----


def test_wildcard_matches_any_present_value_but_not_an_absent_key():
    m = Matcher()
    m.match({"a": "*"}, {"a": 0}, None)
    m.match({"a": "*"}, {"a": None}, None)
    with pytest.raises(Mismatch, match="key 'a' is missing"):
        m.match({"a": "*"}, {}, None)


def test_id_placeholder_matches_the_request_id_with_its_type():
    m = Matcher()
    m.match({"id": "$id"}, {"id": 7}, 7)
    m.match({"id": "$id"}, {"id": "7"}, "7")
    with pytest.raises(Mismatch, match="want the request id"):
        m.match({"id": "$id"}, {"id": "7"}, 7)


def test_a_name_binds_and_a_later_binding_replaces():
    m = Matcher()
    m.match({"pane": "$p"}, {"pane": "w1:p1"}, None)
    assert m.bindings == {"p": "w1:p1"}
    m.match({"pane": "$p"}, {"pane": "w1:p2"}, None)
    assert m.bindings == {"p": "w1:p2"}


def test_extra_keys_are_allowed_and_values_must_equal():
    m = Matcher()
    m.match({"ok": True}, {"ok": True, "result": {}}, None)
    with pytest.raises(Mismatch, match="ok: want true, got false"):
        m.match({"ok": True}, {"ok": False}, None)
    with pytest.raises(Mismatch, match="want null, got 0"):
        m.match({"a": None}, {"a": 0}, None)
    with pytest.raises(Mismatch, match='want "7", got 7'):
        m.match({"a": "7"}, {"a": 7}, None)
    with pytest.raises(Mismatch, match="want 1, got true"):
        m.match({"a": 1}, {"a": True}, None)


def test_arrays_match_by_length_and_element():
    m = Matcher()
    m.match([1, "*", "$x"], [1, {"k": 2}, "bound"], None)
    assert m.bindings == {"x": "bound"}
    with pytest.raises(Mismatch, match="want 2 elements, got 3"):
        m.match([1, 2], [1, 2, 3], None)
    with pytest.raises(Mismatch, match="1: want 2, got 3"):
        m.match([1, 2], [1, 3], None)


def test_substitute_replaces_bound_names_at_any_depth_and_refuses_the_rest():
    m = Matcher()
    m.bindings["p"] = "w1:p1"
    assert m.substitute({"pane": "$p", "list": ["$p", "*"], "keep": "x"}) == {
        "pane": "w1:p1",
        "list": ["w1:p1", "*"],
        "keep": "x",
    }
    with pytest.raises(ValueError, match="is not bound"):
        m.substitute({"pane": "$q"})
    with pytest.raises(ValueError, match="is never bound"):
        m.substitute({"id": "$id"})

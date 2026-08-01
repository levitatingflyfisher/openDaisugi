"""The capture path under ADR-0010's compound-shell opt-in.

``infer_envelope`` is the one place in the system that already holds the real
commands, so it is the only place where the opt-in can actually flip a trace
from rejected to verified: 90% of decomposable commands in a real transcript
corpus carry two or more distinct heads, so the flag without every head — or
every head without the flag — recovers nothing. Together they recover 48 of the
1,229 episodes rejected across 1,582 measured episodes.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.hook import captures_to_trace, infer_envelope
from opendaisugi.journal import Journal
from opendaisugi.shell_decompose import parser_available

_needs_parser = pytest.mark.skipif(
    not parser_available(), reason="needs the opendaisugi shell extra (tree-sitter-bash)"
)


def _write_capture(tmp_path: Path, *commands: str) -> Path:
    path = tmp_path / "sess.jsonl"
    with path.open("w", encoding="utf-8") as f:
        for c in commands:
            f.write(json.dumps({"step_type": "shell", "command": c}) + "\n")
    return path


def test_default_still_takes_only_the_first_head_and_leaves_the_opt_in_off():
    """Fail-closed regression: nothing changes unless the operator asks."""
    env = infer_envelope([{"step_type": "shell", "command": "cd /repo && pytest -q"}], task="t")
    assert env.permissions.shell_allowlist == ["cd"]
    assert env.permissions.shell_allow_decomposition is False


@_needs_parser
def test_asking_collects_every_head_of_a_compound_command():
    env = infer_envelope(
        [{"step_type": "shell", "command": "cd /repo && pytest -q | tail -5"}],
        task="t",
        allow_shell_decomposition=True,
    )
    assert env.permissions.shell_allowlist == ["cd", "pytest", "tail"]
    assert env.permissions.shell_allow_decomposition is True


@_needs_parser
def test_the_opt_in_is_set_even_when_nothing_compound_was_captured():
    """Unconditional when asked, not "only when a compound appeared".

    ``distiller`` intersects permissions across a cluster and then re-verifies
    the plan template against the result, so a cluster mixing one compound
    session with one simple session would AND the opt-in away and die.
    """
    env = infer_envelope(
        [{"step_type": "shell", "command": "pytest -q"}],
        task="t",
        allow_shell_decomposition=True,
    )
    assert env.permissions.shell_allow_decomposition is True


@_needs_parser
def test_a_redirect_contributes_its_head_and_its_write_scope():
    """ADR-0014: a redirect decomposes, and the observed write target becomes
    file_write evidence — without it the inferred envelope rejects the very
    command it was inferred from."""
    env = infer_envelope(
        [{"step_type": "shell", "command": "pytest -q > /tmp/out.txt"}],
        task="t",
        allow_shell_decomposition=True,
    )
    assert env.permissions.shell_allowlist == ["pytest"]
    assert "/tmp/**" in env.permissions.file_write


@_needs_parser
def test_a_command_that_still_refuses_contributes_its_raw_head():
    """A non-literal redirect target still refuses decomposition; the raw head
    remains evidence."""
    env = infer_envelope(
        [{"step_type": "shell", "command": "pytest -q > $OUT"}],
        task="t",
        allow_shell_decomposition=True,
    )
    assert env.permissions.shell_allowlist == ["pytest"]


@_needs_parser
def test_wrapper_and_substitution_inner_heads_are_collected():
    """The verifier checks wrapped payload heads and substitution inner heads,
    so inference must collect them or the envelope under-admits."""
    env = infer_envelope(
        [
            {"step_type": "shell", "command": "timeout 30 git fetch && make"},
            {"step_type": "shell", "command": "echo $(date -u)"},
            {"step_type": "shell", "command": "sh -c 'pytest -q'"},
        ],
        task="t",
        allow_shell_decomposition=True,
    )
    allow = env.permissions.shell_allowlist
    for head in ("timeout", "git", "make", "echo", "date", "sh", "pytest"):
        assert head in allow, (head, allow)


def test_an_env_prefixed_command_contributes_the_real_head():
    """Head extraction is the verifier's own classifier, not a local guess.

    ``FOO=1 pytest`` used to drop out entirely (the "=" check rejected the whole
    token), so the inferred envelope failed to admit a command that really ran.
    """
    env = infer_envelope(
        [{"step_type": "shell", "command": "CUDA_VISIBLE_DEVICES= pytest -q"}], task="t"
    )
    assert env.permissions.shell_allowlist == ["pytest"]


@_needs_parser
def test_captures_to_trace_verifies_a_compound_capture_when_asked(tmp_path: Path):
    """The money test: the same capture is rejected today and verified when asked."""
    cap = _write_capture(tmp_path, "cd /repo && pytest -q")
    journal = Journal(data_dir=tmp_path / "d")

    tid_default = captures_to_trace(cap, journal, task="t")
    assert journal.load_trace(tid_default).result.ok is False

    tid_opted = captures_to_trace(cap, journal, task="t", allow_shell_decomposition=True)
    assert journal.load_trace(tid_opted).result.ok is True


@_needs_parser
def test_observed_redirect_write_round_trips_through_inference(tmp_path: Path):
    """ADR-0014: inference is evidence collection — the observed write lands in
    file_write scope and the trace verifies. Policy enforcement lives in
    hand-written envelopes (see test_verify_shell_decomposition: the same
    command against an envelope WITHOUT the scope is rejected)."""
    cap = _write_capture(tmp_path, "pytest -q > /etc/passwd")
    journal = Journal(data_dir=tmp_path / "d")
    tid = captures_to_trace(cap, journal, task="t", allow_shell_decomposition=True)
    assert journal.load_trace(tid).result.ok is True


@_needs_parser
def test_redirect_capture_round_trip_verifies_when_asked(tmp_path: Path):
    """The bulk-onboarding money test for ADR-0014: the dominant real-world
    rejected shape (a pipeline with a redirect) now round-trips."""
    cap = _write_capture(tmp_path, "grep -r TODO src | sort > /tmp/hits.txt")
    journal = Journal(data_dir=tmp_path / "d")
    tid = captures_to_trace(cap, journal, task="t", allow_shell_decomposition=True)
    assert journal.load_trace(tid).result.ok is True


def test_asking_on_a_box_without_the_grammar_widens_nothing(monkeypatch, tmp_path: Path):
    """Fail-closed on a missing capability, all the way down.

    With no bash grammar the opt-in cannot collect heads and cannot admit a
    compound command — and must not quietly do either. The envelope still
    carries the field, because it may travel to a box that has the extra.
    """
    monkeypatch.setattr("opendaisugi.shell_decompose._load_parser", lambda: None)
    env = infer_envelope(
        [{"step_type": "shell", "command": "cd /repo && pytest -q"}],
        task="t",
        allow_shell_decomposition=True,
    )
    assert env.permissions.shell_allowlist == ["cd"]
    assert env.permissions.shell_allow_decomposition is True

    cap = _write_capture(tmp_path, "cd /repo && pytest -q")
    journal = Journal(data_dir=tmp_path / "d")
    tid = captures_to_trace(cap, journal, task="t", allow_shell_decomposition=True)
    assert journal.load_trace(tid).result.ok is False

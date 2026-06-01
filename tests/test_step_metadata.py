"""Tests for the per-step metadata bag (v0.9.0)."""

from __future__ import annotations

from opendaisugi.models import FileWriteStep, ShellStep


def test_step_has_empty_metadata_by_default():
    s = ShellStep(id="s1", command="ls")
    assert s.metadata == {}


def test_step_accepts_arbitrary_metadata():
    s = ShellStep(
        id="s1",
        command="send_email",
        metadata={
            "to": "x@y.z",
            "signature": "Robin",
            "body": "Hi!",
        },
    )
    assert s.metadata["signature"] == "Robin"
    assert s.metadata["body"] == "Hi!"


def test_file_write_step_also_has_metadata():
    s = FileWriteStep(id="s2", path="/tmp/x", content="hi", metadata={"author": "sam"})
    assert s.metadata["author"] == "sam"


def test_metadata_round_trips_through_model_dump():
    s = ShellStep(id="s1", command="ls", metadata={"k": "v"})
    dumped = s.model_dump()
    assert dumped["metadata"] == {"k": "v"}

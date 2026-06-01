"""Tests for opendaisugi.lora.dataset (v0.5.0)."""

from __future__ import annotations

import json
import sqlite3

import pytest

from opendaisugi.journal import Journal
from opendaisugi.lora.dataset import (
    DatasetStats,
    TrainingExample,
    emit_jsonl,
    iter_training_examples,
)
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
)


def _seed(journal: Journal, *, task: str = "Summarize a PDF", trace_id: str = "t1") -> None:
    env = Envelope(
        id=f"env_{trace_id}",
        generated_by="test",
        task=task,
        permissions=Permission(shell=True),
    )
    plan = ActionPlan(
        id=f"plan_{trace_id}", task=task, source="test",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    result = VerificationResult(
        plan_id=plan.id, envelope_id=env.id,
        ok=True, violations=[], warnings=[], duration_ms=1.0,
    )
    journal.log(
        trace_id=trace_id, task=task,
        envelope=env, plan=plan, result=result,
    )
    # list_successful_traces filters by run_status — set it explicitly.
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_status = 'succeeded' WHERE id = ?",
            (trace_id,),
        )


def test_training_example_alpaca_shape() -> None:
    ex = TrainingExample(task="Do X", envelope_json="{}", trace_id="t1")
    d = ex.to_alpaca()
    assert d["instruction"] == "Do X"
    assert d["input"] == ""
    assert d["output"] == "{}"


def test_training_example_chat_shape() -> None:
    ex = TrainingExample(task="Do X", envelope_json="{}", trace_id="t1")
    d = ex.to_chat()
    assert d["messages"][0]["role"] == "user"
    assert d["messages"][0]["content"] == "Do X"
    assert d["messages"][1]["role"] == "assistant"


def test_training_example_chat_with_system_prompt() -> None:
    ex = TrainingExample(task="Do X", envelope_json="{}", trace_id="t1")
    d = ex.to_chat(system_prompt="You produce safety envelopes.")
    assert d["messages"][0]["role"] == "system"
    assert len(d["messages"]) == 3


def test_iter_training_examples_from_journal(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")
    _seed(journal, task="Deploy the app", trace_id="t2")

    examples = list(iter_training_examples(journal))
    assert len(examples) == 2
    tasks = {e.task for e in examples}
    assert tasks == {"Build a widget", "Deploy the app"}


def test_iter_skips_short_tasks(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="ok", trace_id="t1")          # 2 chars — skipped
    _seed(journal, task="Build a widget", trace_id="t2")

    examples = list(iter_training_examples(journal, min_task_chars=10))
    assert len(examples) == 1
    assert examples[0].trace_id == "t2"


def test_emit_jsonl_alpaca(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")
    _seed(journal, task="Deploy the app", trace_id="t2")

    out = tmp_path / "train.jsonl"
    stats = emit_jsonl(journal, out, format="alpaca")

    assert isinstance(stats, DatasetStats)
    assert stats.written == 2
    assert stats.output_path == str(out)

    lines = out.read_text().strip().splitlines()
    assert len(lines) == 2
    first = json.loads(lines[0])
    assert "instruction" in first
    assert "output" in first
    # Envelope JSON round-trips.
    Envelope.model_validate_json(first["output"])


def test_emit_jsonl_chat(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")

    out = tmp_path / "chat.jsonl"
    stats = emit_jsonl(journal, out, format="chat",
                       system_prompt="Produce safety envelopes.")
    assert stats.written == 1

    payload = json.loads(out.read_text().strip())
    assert payload["messages"][0]["role"] == "system"
    assert payload["messages"][1]["content"] == "Build a widget"


def test_emit_jsonl_counts_skips(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")
    _seed(journal, task="x", trace_id="t2")  # too short
    _seed(journal, task="Deploy the app", trace_id="t3")

    out = tmp_path / "train.jsonl"
    stats = emit_jsonl(journal, out, min_task_chars=10)
    assert stats.total == 3
    assert stats.written == 2
    assert stats.skipped_empty_task == 1


def test_emit_jsonl_empty_journal(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    out = tmp_path / "empty.jsonl"
    stats = emit_jsonl(journal, out)
    assert stats.total == 0
    assert stats.written == 0
    assert out.exists()
    assert out.read_text() == ""


def test_emit_jsonl_rejects_unknown_format(tmp_path) -> None:
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")
    with pytest.raises(ValueError, match="unknown format"):
        emit_jsonl(journal, tmp_path / "x.jsonl", format="markdown")  # type: ignore[arg-type]

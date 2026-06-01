"""Emit LoRA training examples from the journal (v0.5.0).

The journal records every successful (task, envelope, plan, result)
tuple. For LoRA we only need the (task → envelope JSON) mapping —
the envelope is what the base model needs to learn to produce, and
the rest (plan, result) is downstream verification.

Two output formats are supported:

- **alpaca**: ``{"instruction": task, "input": "", "output": envelope_json}``
  — widely compatible with HuggingFace SFTTrainer and Axolotl.
- **chat**: ``{"messages": [{"role": "user", ...}, {"role": "assistant", ...}]}``
  — native format for instruction-tuned chat models.

The dataset is emitted as JSONL for streaming-friendly reading.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator, Literal

from opendaisugi.journal import Journal

_log = logging.getLogger("opendaisugi.lora.dataset")

Format = Literal["alpaca", "chat"]


@dataclass
class TrainingExample:
    """One (task → envelope) training pair."""

    task: str
    envelope_json: str
    trace_id: str

    def to_alpaca(self) -> dict:
        return {
            "instruction": self.task,
            "input": "",
            "output": self.envelope_json,
        }

    def to_chat(self, system_prompt: str | None = None) -> dict:
        messages = []
        if system_prompt:
            messages.append({"role": "system", "content": system_prompt})
        messages.append({"role": "user", "content": self.task})
        messages.append({"role": "assistant", "content": self.envelope_json})
        return {"messages": messages}


@dataclass
class DatasetStats:
    """Summary of an emit_jsonl run."""

    total: int
    written: int
    skipped_empty_task: int = 0
    skipped_load_error: int = 0
    output_path: str = ""


def iter_training_examples(
    journal: Journal,
    *,
    since: float | None = None,
    min_task_chars: int = 10,
) -> Iterator[TrainingExample]:
    """Yield training examples from successful journal traces.

    ``min_task_chars`` filters out stub tasks that would poison a fine-tune
    with noise (e.g. smoke-test traces with single-char tasks). ``since``
    is a unix timestamp; pass None to scan everything.
    """
    for row in journal.list_successful_traces(since=since):
        trace_id = row.trace_id
        try:
            record = journal.load_trace(trace_id)
        except Exception as e:
            _log.warning("skipping trace %s: load error %s", trace_id, e)
            continue

        task = (record.task or "").strip()
        if len(task) < min_task_chars:
            continue

        yield TrainingExample(
            task=task,
            envelope_json=record.envelope.model_dump_json(),
            trace_id=trace_id,
        )


def emit_jsonl(
    journal: Journal,
    output_path: Path,
    *,
    format: Format = "alpaca",
    since: float | None = None,
    min_task_chars: int = 10,
    system_prompt: str | None = None,
) -> DatasetStats:
    """Stream training examples from ``journal`` into a JSONL file.

    Returns a :class:`DatasetStats` summary. The output file is written
    in append-friendly JSONL so large dumps don't need to fit in memory.
    """
    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)

    total = 0
    written = 0
    skipped_empty = 0
    skipped_load = 0

    # Pre-scan to populate skip counts — the generator already skips
    # silently, so we tally here for observability.
    rows = journal.list_successful_traces(since=since)
    total = len(rows)

    with output_path.open("w", encoding="utf-8") as fp:
        for row in rows:
            trace_id = row.trace_id
            try:
                record = journal.load_trace(trace_id)
            except Exception:
                skipped_load += 1
                continue
            task = (record.task or "").strip()
            if len(task) < min_task_chars:
                skipped_empty += 1
                continue

            example = TrainingExample(
                task=task,
                envelope_json=record.envelope.model_dump_json(),
                trace_id=trace_id,
            )
            if format == "alpaca":
                payload = example.to_alpaca()
            elif format == "chat":
                payload = example.to_chat(system_prompt=system_prompt)
            else:
                raise ValueError(f"unknown format: {format!r}")

            fp.write(json.dumps(payload) + "\n")
            written += 1

    return DatasetStats(
        total=total,
        written=written,
        skipped_empty_task=skipped_empty,
        skipped_load_error=skipped_load,
        output_path=str(output_path),
    )

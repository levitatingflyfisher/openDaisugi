"""Envelope-generation smoke test via `claude -p`.

WHAT THIS CHECKS
    End-to-end exercise of ENVELOPE_SYSTEM_PROMPT against a small corpus
    using `claude -p --output-format json` as a no-API-key transport.
    Confirms the prompt produces parseable, schema-valid envelopes that
    satisfy basic shape assertions across three permission shapes
    (read-only, read+write, shell+allowlist).

WHAT THIS DOES NOT CHECK
    The production pipeline (litellm + instructor's tool-use enforcement
    and Pydantic re-ask loop). For that, run test_calibration.py with
    ANTHROPIC_API_KEY set.

GATING
    Opt-in via DAISUGI_SMOKE=1. Each call pays full system-prompt cache
    cost (~$0.13), so a run is ~$0.40 — too expensive for default CI.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest
import yaml

from opendaisugi.envelope import ENVELOPE_SYSTEM_PROMPT, _check_assert
from opendaisugi.models import Envelope

CORPUS_PATH = Path(__file__).parent / "fixtures" / "calibration_tasks.yaml"
SMOKE_IDS = {"fs_read_csv", "dt_json_to_yaml", "fs_delete_old_logs"}

pytestmark = [
    pytest.mark.smoke,
    pytest.mark.skipif(
        not os.getenv("DAISUGI_SMOKE"),
        reason="DAISUGI_SMOKE not set; opt-in test (~$0.40 per run)",
    ),
    pytest.mark.skipif(
        shutil.which("claude") is None,
        reason="claude CLI not on PATH",
    ),
]


def _strip_fences(text: str) -> str:
    """Strip ```json ... ``` fences if claude added them despite instruction."""
    text = text.strip()
    if not text.startswith("```"):
        return text
    body = text.split("\n", 1)[1] if "\n" in text else text[3:]
    body = body.rsplit("```", 1)[0]
    if body.startswith("json\n"):
        body = body[5:]
    return body.strip()


def _generate_via_claude_p(task: str, *, timeout_s: int = 90) -> Envelope:
    prompt = (
        f"{ENVELOPE_SYSTEM_PROMPT}\n\n"
        f"Task: {task}\n\n"
        "Return ONLY the JSON object — no prose, no code fences, no commentary."
    )
    proc = subprocess.run(
        ["claude", "-p", prompt, "--output-format", "json"],
        capture_output=True,
        text=True,
        timeout=timeout_s,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"claude -p rc={proc.returncode}: {proc.stderr[:500]}")
    wrapper = json.loads(proc.stdout)
    if wrapper.get("is_error"):
        raise RuntimeError(f"claude -p error: {wrapper.get('result', '')[:500]}")
    return Envelope.model_validate_json(_strip_fences(wrapper["result"]))


def test_smoke_envelope_generation_via_claude_p():
    """Three corpus entries must produce schema-valid, shape-passing envelopes."""
    corpus = yaml.safe_load(CORPUS_PATH.read_text())
    smoke_entries = [e for e in corpus if e["id"] in SMOKE_IDS]
    assert len(smoke_entries) == len(SMOKE_IDS), (
        f"Smoke fixture drift: expected {SMOKE_IDS}, "
        f"found {sorted(e['id'] for e in smoke_entries)}"
    )

    failures: list[tuple[str, str]] = []
    for entry in smoke_entries:
        try:
            env = _generate_via_claude_p(entry["task"])
        except Exception as e:
            failures.append((entry["id"], f"generation: {type(e).__name__}: {e}"))
            continue
        for a in entry.get("asserts", []):
            if not _check_assert(env, a):
                failures.append((entry["id"], f"assert failed: {a}"))

    assert failures == [], f"Smoke failures: {failures}"

"""End-to-end: journal parse of a real transcript via ``claude -p``.

Opt-in only. Skipped unless ``DAISUGI_CLAUDE_CODE_INTEGRATION=1`` is set, the
``claude`` binary is present, and ``DAISUGI_CLAUDE_CODE_TRANSCRIPT`` points at a
real Claude Code ``.jsonl`` transcript. This exists so the claude-code backend's
full pipeline can be exercised against a real conversation without hitting the API.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

_TRANSCRIPT = os.environ.get("DAISUGI_CLAUDE_CODE_TRANSCRIPT", "")


@pytest.mark.skipif(
    shutil.which("claude") is None
    or os.environ.get("DAISUGI_CLAUDE_CODE_INTEGRATION") != "1"
    or not _TRANSCRIPT
    or not Path(_TRANSCRIPT).exists(),
    reason=(
        "requires the claude binary, DAISUGI_CLAUDE_CODE_INTEGRATION=1, and "
        "DAISUGI_CLAUDE_CODE_TRANSCRIPT pointing at a real .jsonl transcript"
    ),
)
def test_parse_real_transcript_via_claude_code(tmp_path):
    out = tmp_path / "episodes.yaml"
    proc = subprocess.run(
        [
            "daisugi", "journal", "parse", _TRANSCRIPT,
            "-o", str(out), "--llm", "claude-code",
        ],
        capture_output=True,
        text=True,
        timeout=900,  # large transcript + subprocess overhead
    )
    assert proc.returncode == 0, f"stderr: {proc.stderr!r}"
    assert out.exists()
    assert out.stat().st_size > 0

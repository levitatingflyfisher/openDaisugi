"""claude -p runs in a neutral CWD so project context can't contaminate calls."""

from __future__ import annotations

import subprocess
from unittest.mock import patch

from opendaisugi.claude_code_llm import _neutral_cwd, call_claude_p_sync


def test_sync_uses_neutral_cwd_by_default():
    captured = {}

    def fake_run(args, **kwargs):
        captured.update(kwargs)
        return subprocess.CompletedProcess(args, 0, stdout="ok", stderr="")

    with patch("subprocess.run", fake_run):
        call_claude_p_sync("hi", timeout_s=5, model="haiku")
    assert captured["cwd"] == _neutral_cwd()
    # The neutral dir is not the project cwd and holds no CLAUDE.md.
    import os
    assert not os.path.exists(os.path.join(captured["cwd"], "CLAUDE.md"))


def test_explicit_cwd_overrides():
    captured = {}

    def fake_run(args, **kwargs):
        captured.update(kwargs)
        return subprocess.CompletedProcess(args, 0, stdout="ok", stderr="")

    with patch("subprocess.run", fake_run):
        call_claude_p_sync("hi", timeout_s=5, model="haiku", cwd="/some/dir")
    assert captured["cwd"] == "/some/dir"


def test_neutral_cwd_is_stable():
    assert _neutral_cwd() == _neutral_cwd()

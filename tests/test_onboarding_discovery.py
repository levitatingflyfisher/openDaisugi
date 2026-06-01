"""Day-one onboarding: discover existing harness transcripts to bulk-distill.

A coworker with months of work has thousands of conversation transcripts already
on disk (Claude Code under ~/.claude/projects, Codex under ~/.codex, ...).
``discover_transcripts`` finds them so ``daisugi onboard`` can turn them into
pathways without the user hand-listing files.
"""

import os

import pytest

from opendaisugi.onboarding import (
    DiscoveredTranscript,
    default_transcript_roots,
    discover_transcripts,
)


def test_default_roots_derive_from_home(tmp_path):
    roots = default_transcript_roots(home=tmp_path)
    assert roots["claude-code"] == tmp_path / ".claude" / "projects"
    assert roots["codex"] == tmp_path / ".codex" / "sessions"


def test_env_override_adds_roots(tmp_path, monkeypatch):
    custom = tmp_path / "exported"
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"myharness={custom}")
    roots = default_transcript_roots(home=tmp_path)
    assert roots["myharness"] == custom


def test_discovers_planted_claude_code_transcript(tmp_path):
    proj = tmp_path / ".claude" / "projects" / "some-project"
    proj.mkdir(parents=True)
    t = proj / "session-abc.jsonl"
    t.write_text('{"type":"user"}\n')

    found = discover_transcripts(roots={"claude-code": tmp_path / ".claude" / "projects"})
    assert len(found) == 1
    assert isinstance(found[0], DiscoveredTranscript)
    assert found[0].path == t
    assert found[0].harness == "claude-code"
    assert found[0].size > 0


def test_ignores_non_jsonl_and_empty_files(tmp_path):
    root = tmp_path / "t"
    root.mkdir()
    (root / "real.jsonl").write_text('{"type":"user"}\n')
    (root / "notes.md").write_text("hello")          # wrong extension
    (root / "empty.jsonl").write_text("")            # empty

    found = discover_transcripts(roots={"h": root})
    names = {f.path.name for f in found}
    assert names == {"real.jsonl"}


def test_missing_root_returns_empty(tmp_path):
    found = discover_transcripts(roots={"h": tmp_path / "does-not-exist"})
    assert found == []


def test_results_are_newest_first(tmp_path):
    root = tmp_path / "t"
    root.mkdir()
    old = root / "old.jsonl"
    new = root / "new.jsonl"
    old.write_text('{"x":1}\n')
    new.write_text('{"x":1}\n')
    os.utime(old, (1000, 1000))
    os.utime(new, (2000, 2000))

    found = discover_transcripts(roots={"h": root})
    assert [f.path.name for f in found] == ["new.jsonl", "old.jsonl"]

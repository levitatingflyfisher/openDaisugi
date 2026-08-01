"""The bulk-ingest half of ADR-0010's opt-in, under ADR-0016 inference.

Envelopes are inferred from each episode's observed steps — deterministic,
zero LLM. The opt-in default stays fail-closed (no decomposition unless
asked), and with the opt-in the inferred allowlist names every observed head,
so a real compound episode round-trips. The old guard rail about stamping the
opt-in onto a generated pathway envelope is structurally obsolete: ingest no
longer calls ``generate_envelope`` at all, so there is no pathway envelope to
stamp.
"""

from __future__ import annotations

import pytest

from opendaisugi.ingest import ingest_episodes
from opendaisugi.journal import Journal
from opendaisugi.models import ShellStep
from opendaisugi.parsers import Episode, ParseResult


def _parse_result() -> ParseResult:
    return ParseResult(
        source="claude-code",
        source_file="/tmp/t.jsonl",
        parsed_at="2026-08-19T00:00:00Z",
        episodes=[
            Episode(
                id="ep_00",
                task="run the tests",
                steps=[ShellStep(id="s0", command="cd /repo && pytest -q")],
                source_range={"first_message": 0, "last_message": 1},
            )
        ],
    )


@pytest.fixture
def journal(tmp_path):
    return Journal(data_dir=tmp_path)


async def test_ingest_leaves_the_opt_in_off_by_default(journal):
    await ingest_episodes(_parse_result(), journal)
    record = journal.load_trace(journal.list_recent(limit=1)[0].id)
    assert record.envelope.permissions.shell_allow_decomposition is False
    assert record.result.ok is False  # metachar gate holds without the opt-in


async def test_ingest_opt_in_round_trips_a_real_compound_episode(journal):
    # THE onboarding fix: the inferred allowlist names cd AND pytest, the
    # opt-in decomposes the compound — the episode verifies with zero LLM.
    await ingest_episodes(_parse_result(), journal, allow_shell_decomposition=True)
    record = journal.load_trace(journal.list_recent(limit=1)[0].id)
    assert record.envelope.permissions.shell_allow_decomposition is True
    assert record.result.ok is True
    assert "pytest" in record.envelope.permissions.shell_allowlist

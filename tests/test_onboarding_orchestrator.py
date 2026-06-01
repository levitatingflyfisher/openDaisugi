"""Day-one onboarding orchestrator: discover -> parse -> ingest -> tend.

``onboard`` chains the existing pipeline over every discovered transcript so a
new adopter runs ONE command and gets pathways from their existing convos.
Tested over injected seams (no real LLM / filesystem parsing) — the orchestration
logic is what matters here; parse/ingest/tend are covered by their own suites.
"""

import asyncio
from pathlib import Path

from opendaisugi.onboarding import DiscoveredTranscript, OnboardReport, onboard


def _t(name, harness="claude-code", mtime=1000.0):
    return DiscoveredTranscript(path=Path(f"/x/{name}"), harness=harness, size=10, mtime=mtime)


class _StubParseResult:
    def __init__(self, n_episodes):
        self.episodes = list(range(n_episodes))


class _StubIngest:
    def __init__(self, passed=0, failed=0, skipped=0, errored=0):
        self.total = passed + failed + skipped + errored
        self.passed, self.failed, self.skipped, self.errored = passed, failed, skipped, errored
        self.episodes = []


class _StubTend:
    def __init__(self, created=0, updated=0):
        self.created, self.updated = created, updated
        self.warnings = []


def _run(**kw):
    calls = {"parse": [], "ingest": [], "tend": 0}

    def parse_one(t):
        calls["parse"].append(t)
        return _StubParseResult(2)

    async def ingest_one(pr):
        calls["ingest"].append(pr)
        return _StubIngest(passed=2)

    async def run_tend():
        calls["tend"] += 1
        return _StubTend(created=1)

    defaults = dict(parse_one=parse_one, ingest_one=ingest_one, run_tend=run_tend)
    defaults.update(kw)
    report = asyncio.run(onboard(**defaults))
    return report, calls


def test_onboard_chains_every_transcript_then_tends_once():
    report, calls = _run(discover=lambda roots=None: [_t("a"), _t("b")])
    assert isinstance(report, OnboardReport)
    assert report.transcripts_found == 2
    assert report.transcripts_processed == 2
    assert len(calls["parse"]) == 2
    assert len(calls["ingest"]) == 2
    assert calls["tend"] == 1            # tend runs once, after all ingests
    assert report.traces_passed == 4     # 2 episodes * 2 transcripts
    assert report.pathways_created == 1


def test_onboard_limit_caps_transcripts():
    report, calls = _run(
        discover=lambda roots=None: [_t("a"), _t("b"), _t("c")], limit=2
    )
    assert report.transcripts_found == 3
    assert report.transcripts_processed == 2
    assert len(calls["parse"]) == 2


def test_onboard_harness_filter():
    report, calls = _run(
        discover=lambda roots=None: [_t("a", "claude-code"), _t("b", "codex")],
        harnesses=["codex"],
    )
    assert report.transcripts_processed == 1
    assert report.by_harness == {"codex": 1}


def test_onboard_dry_run_skips_tend():
    report, calls = _run(
        discover=lambda roots=None: [_t("a")], dry_run=True
    )
    assert report.dry_run is True
    assert calls["tend"] == 0            # no distillation / no writes in dry-run
    assert report.pathways_created == 0


def test_onboard_no_transcripts_warns_and_skips_tend():
    report, calls = _run(discover=lambda roots=None: [])
    assert report.transcripts_found == 0
    assert calls["tend"] == 0
    assert any("no transcripts" in w.lower() for w in report.warnings)


def test_onboard_skips_harness_with_no_parser():
    # parse_one returns None for an unsupported harness — counted, warned, not fatal.
    def parse_one(t):
        return None if t.harness == "codex" else _StubParseResult(2)

    async def ingest_one(pr):
        return _StubIngest(passed=2)

    async def run_tend():
        return _StubTend(created=1)

    report = asyncio.run(
        onboard(
            parse_one=parse_one,
            ingest_one=ingest_one,
            run_tend=run_tend,
            discover=lambda roots=None: [_t("a", "claude-code"), _t("b", "codex")],
        )
    )
    assert report.transcripts_processed == 1
    assert report.by_harness == {"claude-code": 1}
    assert any("no parser" in w.lower() and "codex" in w.lower() for w in report.warnings)

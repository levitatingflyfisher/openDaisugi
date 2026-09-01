"""Host facts: the one place a floor test says which host it needs.

A skip that does not name the missing host is a lie of omission. A fact that
claims a binary is present when it is not would turn every skip into a false
failure. Both directions are tested here.
"""

from __future__ import annotations

import shutil

from tests.floor import hostfacts


def test_every_fact_carries_a_reason_and_a_fix():
    for name, fact in hostfacts.host_facts().items():
        assert fact.name == name
        assert fact.why_not.strip(), f"{name} has no why_not"
        assert fact.fix.strip(), f"{name} has no fix"
        assert len(fact.fix) <= 90, f"{name}'s fix is too long to read in a skip line"
        assert fact.fix[0].islower(), f"{name}'s fix should start with a verb, not a capital"


def test_tmux_presence_matches_the_path():
    assert hostfacts.host_facts()["tmux"].present is (shutil.which("tmux") is not None)


def test_herdr_presence_matches_the_path():
    assert hostfacts.host_facts()["herdr"].present is (shutil.which("herdr") is not None)


def test_skip_reason_is_none_when_present_and_teaches_when_absent():
    facts = hostfacts.host_facts()
    for name, fact in facts.items():
        reason = hostfacts.skip_reason(name)
        if fact.present:
            assert reason is None
        else:
            assert reason is not None
            assert fact.fix in reason
            assert "—" not in reason, "STE100: no em-dash in a string an operator reads"


def test_vendored_manifest_fact_points_at_the_plan_02_directory():
    fact = hostfacts.host_facts()["vendored_manifests"]
    assert "harness/coppice/internal/detect/manifests" in fact.why_not
    assert fact.present is hostfacts.MANIFEST_DIR.is_dir()


def test_unknown_fact_name_teaches_the_known_ones():
    reason = hostfacts.skip_reason("nope")
    assert reason is not None and "tmux" in reason
    assert "—" not in reason, "STE100: no em-dash in a string an operator reads"

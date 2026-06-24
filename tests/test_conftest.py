"""Sanity tests that exercise the shared pytest fixtures."""

from opendaisugi.models import ActionPlan, Envelope


def test_sample_envelope_fixture(sample_envelope: Envelope):
    assert sample_envelope.task == "Delete .tmp files in /var/log"
    assert sample_envelope.permissions.shell is True
    assert "find" in sample_envelope.permissions.shell_allowlist
    assert sample_envelope.permissions.file_read == ["/var/log/**"]
    assert len(sample_envelope.postconditions) == 1


def test_sample_plan_fixture(sample_plan: ActionPlan):
    assert sample_plan.source == "vanilla-llm"
    assert len(sample_plan.steps) == 1
    assert sample_plan.steps[0].type == "shell"

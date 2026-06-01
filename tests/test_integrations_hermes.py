"""Tests for the Hermes integration adapter (v0.10.0)."""

from __future__ import annotations

from pathlib import Path

from opendaisugi.integrations import hermes
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    Postcondition,
    ShellStep,
)


FIXTURES = Path(__file__).parent / "fixtures"


def test_hermes_envelope_from_yaml_loads_agent():
    envelope = hermes.envelope_from_yaml(FIXTURES / "agent.envelope.yaml")
    assert isinstance(envelope, Envelope)
    assert envelope.generated_by == "agent-kit-robin"
    assert envelope.stakes == "medium"
    assert any(inv.type == "never_impersonates" for inv in envelope.invariants)
    assert any(pc.type == "body_no_impersonation" for pc in envelope.postconditions)


def test_hermes_verify_step_rejects_impersonation():
    envelope = hermes.envelope_from_yaml(FIXTURES / "agent.envelope.yaml")
    bad_step = ShellStep(
        id="s1",
        command="send_email",
        metadata={
            "type": "email_send",
            "to": "editor@blog.com",
            "to_folder": "drafts",
            "body": "Hi,\n\nDraft attached.\n\n— Ada",
        },
    )
    violations = hermes.verify_step(bad_step, envelope)
    assert any("body_no_impersonation" in v.message for v in violations)


def test_hermes_verify_step_accepts_clean_body():
    envelope = hermes.envelope_from_yaml(FIXTURES / "agent.envelope.yaml")
    ok_step = ShellStep(
        id="s2",
        command="send_email",
        metadata={
            "type": "email_send",
            "to": "editor@blog.com",
            "to_folder": "drafts",
            "body": "Hi,\n\nDraft attached.\n\n— Robin",
        },
    )
    violations = hermes.verify_step(ok_step, envelope)
    assert violations == []


def test_hermes_verify_plan_structural_check():
    envelope = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    result = hermes.verify_plan(plan, envelope)
    assert result.ok, result.violations


def test_hermes_load_household_aliases():
    registry = hermes.load_household_aliases(FIXTURES / "household_aliases.yaml")
    assert "only_send_to_approved_contacts" in registry
    assert "curriculum_uses_only_owned_items" in registry


def test_hermes_envelope_from_yaml_with_household_registry():
    registry = hermes.load_household_aliases(FIXTURES / "household_aliases.yaml")
    envelope = hermes.envelope_from_yaml(
        FIXTURES / "agent.envelope.yaml",
        extra_registry=registry,
    )
    assert envelope.generated_by == "agent-kit-robin"


def test_hermes_envelope_from_yaml_with_bare_extra_registry():
    """A bare AliasRegistry() as extra_registry must still resolve system aliases.

    Regression: earlier code only loaded system aliases when extra_registry
    was None, so passing an empty registry caused UnknownAliasError on
    invariants that referenced system-tier names (never_impersonates).
    """
    from opendaisugi.aliases import AliasRegistry

    envelope = hermes.envelope_from_yaml(
        FIXTURES / "agent.envelope.yaml",
        extra_registry=AliasRegistry(),
    )
    assert any(inv.type == "never_impersonates" for inv in envelope.invariants)

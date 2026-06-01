"""Ensure v0.9.0 public symbols are exported from the package root."""

from __future__ import annotations


def test_expression_types_exported():
    import opendaisugi
    assert hasattr(opendaisugi, "Expression")
    assert hasattr(opendaisugi, "parse_expression")


def test_alias_registry_exported():
    import opendaisugi
    assert hasattr(opendaisugi, "Alias")
    assert hasattr(opendaisugi, "AliasRegistry")
    assert hasattr(opendaisugi, "load_system_aliases")


def test_stage2_verifier_exported():
    import opendaisugi
    assert hasattr(opendaisugi, "verify_completed_step")


def test_version_bumped():
    import opendaisugi
    # v0.9.0 symbols remain exported beyond v0.9 — only assert the
    # package is at or past that version.
    v = opendaisugi.__version__
    major, minor, *_ = v.split(".")
    assert (int(major), int(minor)) >= (0, 9), f"version regressed: {v}"


def test_integrations_module_exported():
    import opendaisugi
    assert hasattr(opendaisugi, "integrations")
    from opendaisugi.integrations import hermes
    assert callable(hermes.envelope_from_yaml)
    assert callable(hermes.verify_plan)
    assert callable(hermes.verify_step)

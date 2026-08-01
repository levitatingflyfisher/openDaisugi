"""ADR-0010's opt-in reaches the envelope the gate actually checks against.

The starter envelope is what an operator gets from ``daisugi gate init`` and
what every gated session falls back to. Until now it hard-coded
``shell_allow_decomposition`` to the model default by omission, so `a && b`
was unconditionally denied with no way to opt in short of editing the JSON.
"""

from __future__ import annotations

from opendaisugi.gate import starter_envelope


def test_starter_envelope_still_defaults_to_no_decomposition(tmp_path):
    """The default is unchanged — decomposition stays opt-in (ADR-0010)."""
    env = starter_envelope(tmp_path)
    assert env.permissions.shell_allow_decomposition is False


def test_starter_envelope_can_opt_into_decomposition(tmp_path):
    env = starter_envelope(tmp_path, allow_shell_decomposition=True)
    assert env.permissions.shell_allow_decomposition is True


def test_opting_in_changes_nothing_else(tmp_path):
    """One variable — the opt-in must not quietly widen any other permission."""
    plain = starter_envelope(tmp_path).permissions.model_dump()
    opted = starter_envelope(tmp_path, allow_shell_decomposition=True).permissions.model_dump()
    plain.pop("shell_allow_decomposition")
    opted.pop("shell_allow_decomposition")
    assert plain == opted

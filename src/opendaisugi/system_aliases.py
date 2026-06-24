"""System-shipped aliases (v0.9.0).

Loaded into an AliasRegistry at startup. These aliases are the baseline
vocabulary all envelopes can reference without further registration.
Household and envelope tiers can override by re-registering under the
same name at a higher precedence tier.

Alias bodies are stored as raw dicts (not parsed Expression models) so
that typed-field placeholders like ``$max_scale`` can survive until
parameter substitution. ``AliasRegistry.resolve`` calls
``parse_expression`` after substitution.
"""

from __future__ import annotations

from opendaisugi.aliases import Alias, AliasRegistry


def _velocity_scale_bounded() -> Alias:
    return Alias(
        name="velocity_scale_bounded",
        params=["max_scale"],
        tier="system",
        description="forall joint_move / cartesian_move steps: velocity_scale <= max_scale",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "implies",
                "a": {"op": "in_set", "path": "type", "values": ["joint_move", "cartesian_move"]},
                "b": {"op": "numeric_range", "path": "velocity_scale", "min": 0.0, "max": "$max_scale"},
            },
        },
    )


def _never_impersonates() -> Alias:
    return Alias(
        name="never_impersonates",
        params=["principal"],
        tier="system",
        description="outbound communication steps must not sign as the principal",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "implies",
                "a": {"op": "in_set", "path": "type", "values": ["email_send", "imessage_send", "shell"]},
                "b": {
                    "op": "and",
                    "children": [
                        {"op": "not_equals", "path": "metadata.signature", "value": "$principal"},
                        {"op": "not_matches", "path": "metadata.body", "regex": r"(?i)(\u2014|-)\s*$principal"},
                    ],
                },
            },
        },
    )


def _no_pii_regex() -> Alias:
    return Alias(
        name="no_pii_regex",
        params=[],
        tier="system",
        description="forbid PII patterns (SSN, credit card) in content or metadata.body",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "and",
                "children": [
                    {"op": "not_matches", "path": "content", "regex": r"\b\d{3}-\d{2}-\d{4}\b"},
                    {"op": "not_matches", "path": "content", "regex": r"\b(?:\d[ -]*?){13,16}\b"},
                    {"op": "not_matches", "path": "metadata.body", "regex": r"\b\d{3}-\d{2}-\d{4}\b"},
                    {"op": "not_matches", "path": "metadata.body", "regex": r"\b(?:\d[ -]*?){13,16}\b"},
                ],
            },
        },
    )


def _no_secrets() -> Alias:
    return Alias(
        name="no_secrets",
        params=[],
        tier="system",
        description="forbid common secret patterns (AWS keys, JWT, ssh private keys)",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "and",
                "children": [
                    {"op": "not_matches", "path": "content", "regex": r"AKIA[0-9A-Z]{16}"},
                    {"op": "not_matches", "path": "content", "regex": r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}"},
                    {"op": "not_matches", "path": "content", "regex": r"-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"},
                    {"op": "not_matches", "path": "metadata.body", "regex": r"AKIA[0-9A-Z]{16}"},
                    {"op": "not_matches", "path": "metadata.body", "regex": r"-----BEGIN (RSA |OPENSSH |EC )?PRIVATE KEY-----"},
                ],
            },
        },
    )


def _pytest_passes() -> Alias:
    # v0.28.4: this alias references ``metadata.output`` which is unset at
    # Stage 1 verify time — it ONLY discharges at Stage 2
    # (verify_completed_step). Used as an ``Invariant.expr`` it produces a
    # loud violation on every plan, because the predicate evaluates False
    # against the unpopulated metadata. Authoring guidance: only attach to
    # a ``Postcondition`` (Stage 2 gate) — never to an ``Invariant``
    # (Stage 1 gate). Same caveat applies to any alias whose path begins
    # with ``metadata.output`` / ``metadata.body`` / other
    # runtime-populated fields. The alias name kept for compatibility;
    # docstring is the authoring contract.
    return Alias(
        name="pytest_passes",
        params=[],
        tier="system",
        description="POSTCONDITION-ONLY: at least one completed step's output "
                    "matches /passed/. Do NOT use as an Invariant — "
                    "metadata.output is unset at Stage 1 verify.",
        expr={
            "op": "exists_step",
            "pred": {"op": "matches", "path": "metadata.output", "regex": r"passed"},
        },
    )


def _no_network_writes() -> Alias:
    return Alias(
        name="no_network_writes",
        params=[],
        tier="system",
        description="reject any network step that is not a GET",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "implies",
                "a": {"op": "equals", "path": "type", "value": "network"},
                "b": {"op": "equals", "path": "method", "value": "GET"},
            },
        },
    )


def _structured_approval() -> Alias:
    return Alias(
        name="structured_approval",
        params=[],
        tier="system",
        description="council-member outputs must be structured approval JSON",
        expr={
            "op": "forall_steps",
            "pred": {
                "op": "matches",
                "path": "metadata.output",
                "regex": r'^\s*\{[^}]*"approve"\s*:\s*(true|false)',
            },
        },
    )


def load_system_aliases(registry: AliasRegistry) -> None:
    """Register all shipped system aliases on the given registry."""
    for fn in (
        _velocity_scale_bounded,
        _never_impersonates,
        _no_pii_regex,
        _no_secrets,
        _pytest_passes,
        _no_network_writes,
        _structured_approval,
    ):
        registry.register(fn())


__all__ = ["load_system_aliases"]

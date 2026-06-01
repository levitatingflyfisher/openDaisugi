"""Hermes skill adapter (v0.10.0).

Hermes is a Python skill framework. This module gives Hermes agents a
narrow, stable surface to call openDaisugi without reimplementing the
YAML-envelope loader, alias resolution, or Stage 1/Stage 2 dispatch.

Usage inside a Hermes skill::

    from opendaisugi.integrations import hermes

    envelope = hermes.envelope_from_yaml("./robin.envelope.yaml")
    violations = hermes.verify_step(completed_step, envelope)
    if violations:
        raise RuntimeError(f"runtime-assurance rejected step: {violations}")

The adapter's public surface is exactly four functions:

- ``envelope_from_yaml`` — YAML file → resolved Envelope
- ``load_household_aliases`` — YAML file → populated AliasRegistry
- ``verify_plan`` — Stage 1 structural verification
- ``verify_step`` — Stage 2 post-execution verification

All other helpers live in the core library; Hermes callers should not
need to import them directly.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml

from opendaisugi.aliases import Alias, AliasRegistry
from opendaisugi.models import (
    ActionPlan,
    ActionStep,
    Envelope,
    Invariant,
    Permission,
    Postcondition,
    VerificationResult,
    Violation,
)
from opendaisugi.predicate import parse_expression
from opendaisugi.stage2 import verify_completed_step
from opendaisugi.system_aliases import load_system_aliases
from opendaisugi.verify import verify


__all__ = [
    "envelope_from_yaml",
    "load_household_aliases",
    "verify_plan",
    "verify_step",
]


def envelope_from_yaml(
    path: str | Path,
    *,
    extra_registry: AliasRegistry | None = None,
) -> Envelope:
    """Load a YAML envelope file, resolve aliases, return a ready Envelope.

    ``extra_registry`` layers household-tier aliases on top of the system
    defaults. It may be a bare ``AliasRegistry()`` or one populated by
    ``load_household_aliases`` — system aliases are loaded into the
    resulting registry unconditionally so callers can't accidentally
    skip them.
    """
    data = yaml.safe_load(Path(path).read_text())

    registry = extra_registry if extra_registry is not None else AliasRegistry()
    # Load system aliases unconditionally. load_household_aliases already
    # loads them, so guard against double-registration by checking one
    # sentinel name — registering the same name twice accumulates entries,
    # not a hard error, but we prefer a single canonical copy.
    if "never_impersonates" not in registry:
        load_system_aliases(registry)

    invariants: list[Invariant] = []
    for inv in data.get("invariants", []):
        expr = (
            registry.resolve(parse_expression(inv["expr"]))
            if "expr" in inv
            else None
        )
        invariants.append(
            Invariant(
                type=inv["type"],
                description=inv.get("description", ""),
                target=inv.get("target"),
                scope=inv.get("scope"),
                expr=expr,
                enforce=inv.get("enforce", True),
            )
        )

    postconditions: list[Postcondition] = []
    for pc in data.get("postconditions", []):
        expr = (
            registry.resolve(parse_expression(pc["expr"]))
            if "expr" in pc
            else None
        )
        postconditions.append(
            Postcondition(
                type=pc["type"],
                description=pc.get("description"),
                path=pc.get("path"),
                expected=pc.get("expected"),
                min=pc.get("min"),
                max=pc.get("max"),
                expr=expr,
                enforce=pc.get("enforce", True),
            )
        )

    return Envelope(
        generated_by=data["generated_by"],
        task=data["task"],
        stakes=data.get("stakes", "low"),
        permissions=Permission(**data.get("permissions", {})),
        invariants=invariants,
        postconditions=postconditions,
    )


def load_household_aliases(path: str | Path) -> AliasRegistry:
    """Load a household-tier alias YAML file on top of the system tier.

    The file shape mirrors ``tests/fixtures/household_aliases.yaml``::

        aliases:
          - name: family_principals
            params: []
            expr:
              op: any_of
              exprs: [...]

    Returns a registry ready to pass to ``envelope_from_yaml``.
    """
    registry = AliasRegistry()
    load_system_aliases(registry)
    data = yaml.safe_load(Path(path).read_text())
    for alias_data in data.get("aliases", []):
        registry.register(
            Alias(
                name=alias_data["name"],
                params=alias_data.get("params", []),
                expr=alias_data["expr"],
                tier=alias_data.get("tier", "household"),
                description=alias_data.get("description", ""),
            )
        )
    return registry


def verify_plan(plan: ActionPlan, envelope: Envelope) -> VerificationResult:
    """Stage 1 verification — structural check before execution."""
    return verify(plan, envelope)


def verify_step(step: ActionStep, envelope: Envelope) -> list[Violation]:
    """Stage 2 verification — postconditions on a completed step.

    Run immediately after the step executes and before its effect commits
    externally (SMTP send, HTTP write, etc.). An empty list means the step
    is safe to commit; any violations mean roll back.
    """
    return verify_completed_step(step, envelope)

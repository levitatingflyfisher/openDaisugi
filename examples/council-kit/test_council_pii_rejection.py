"""examples/council-kit/test_council_pii_rejection.py

Runnable demo: a contribution containing an AWS secret is rejected by
the pre-council gate before any council LLM sees it. Deterministic
structural check on the way in means the council burns no bandwidth on
obvious rejects.
"""

from __future__ import annotations

from pathlib import Path

import yaml

from opendaisugi.aliases import AliasRegistry
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
)
from opendaisugi.predicate import parse_expression
from opendaisugi.system_aliases import load_system_aliases
from opendaisugi.verify import verify


def _load_pre_council_envelope() -> Envelope:
    path = Path(__file__).parent / "pre_council.envelope.yaml"
    data = yaml.safe_load(path.read_text())
    registry = AliasRegistry()
    load_system_aliases(registry)
    invariants = [
        Invariant(
            type=i["type"],
            description=i.get("description", ""),
            expr=registry.resolve(parse_expression(i["expr"])),
        )
        for i in data["invariants"]
    ]
    return Envelope(
        generated_by=data["generated_by"],
        task=data["task"],
        stakes=data.get("stakes", "low"),
        permissions=Permission(**data.get("permissions", {})),
        invariants=invariants,
    )


def test_pre_council_rejects_aws_secret():
    envelope = _load_pre_council_envelope()
    plan = ActionPlan(
        source="council-inbox",
        task="proposed KB contribution",
        steps=[
            FileWriteStep(
                id="c1",
                path="~/council/inbox/contribution.md",
                content=(
                    "Here's how to configure the deployment:\n"
                    "export AWS_ACCESS_KEY=AKIAEXAMPLEAAAAAAAAA\n"
                    "That's all folks."
                ),
                metadata={"content_size_bytes": 100},
            )
        ],
    )
    result = verify(plan, envelope)
    assert not result.ok
    assert any("no_secrets" in v.message for v in result.violations)


def test_pre_council_rejects_pii():
    envelope = _load_pre_council_envelope()
    plan = ActionPlan(
        source="council-inbox",
        task="proposed KB contribution",
        steps=[
            FileWriteStep(
                id="c2",
                path="~/council/inbox/contribution.md",
                content="Please reach out to me at 123-45-6789 for questions.",
                metadata={"content_size_bytes": 60},
            )
        ],
    )
    result = verify(plan, envelope)
    assert not result.ok
    assert any("no_pii" in v.message for v in result.violations)


def test_pre_council_accepts_clean_contribution():
    envelope = _load_pre_council_envelope()
    plan = ActionPlan(
        source="council-inbox",
        task="proposed KB contribution",
        steps=[
            FileWriteStep(
                id="c3",
                path="~/council/inbox/contribution.md",
                content="Here is a clean snippet about logging patterns.",
                metadata={"content_size_bytes": 50},
            )
        ],
    )
    result = verify(plan, envelope)
    assert result.ok, result.violations

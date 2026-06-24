"""v0.27.0 — the programmatic Daisugi facade supports register->verify->enforce."""
from __future__ import annotations
from opendaisugi import Daisugi
from opendaisugi.aliases import Alias, AliasRegistry, AliasRef
from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep
from opendaisugi.predicate import parse_expression


def test_facade_round_trip_register_verify_enforce():
    reg = AliasRegistry()
    reg.register(Alias(name="only_ls", tier="household",
        expr=parse_expression({"op": "forall_steps",
            "pred": {"op": "equals", "path": "command", "value": "ls"}})))
    env = Envelope(generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls", "rm"]),
        stakes="high",
        invariants=[Invariant(type="only_ls", description="via alias",
                              expr=AliasRef(name="only_ls"))])
    dai = Daisugi()  # must not touch disk/network on construction
    bad = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="rm")])
    result = dai.verify(bad, env, aliases=reg)
    assert not result.ok
    # ...and the conforming direction passes (guards against a facade that always rejects).
    good = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    assert dai.verify(good, env, aliases=reg).ok


# v0.28.3 — strict mode is reachable through Daisugi.run / Daisugi(strict=...).
# Pre-v0.28.3 the facade never threaded `strict` to verify(); low/medium-stakes
# envelopes could not be opted into strict mode through the facade path.


def test_daisugi_run_strict_kwarg_overrides_low_stakes_default():
    """At low stakes, verify() defaults strict=False. An explicit
    strict=True on Daisugi.run must flip it through to the Supervisor."""
    import asyncio
    from opendaisugi.models import Postcondition
    from opendaisugi.run_session import RunStatus

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes="low",
        postconditions=[Postcondition(type="custom_unknown", description="opaque", enforce=True)],
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    dai = Daisugi(pathway_store=False, cache=False)

    # Default (non-strict at low stakes) — verify passes because opaque
    # postcondition is tolerated.
    session = asyncio.run(dai.run(plan, env))
    assert session.verification.ok

    # strict=True via run kwarg — verify rejects the opaque postcondition.
    session = asyncio.run(dai.run(plan, env, strict=True))
    assert not session.verification.ok
    assert session.status == RunStatus.REJECTED


def test_daisugi_constructor_strict_kwarg_persists():
    """Daisugi(strict=True) sets the default for every subsequent run."""
    import asyncio
    from opendaisugi.models import Postcondition

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes="low",
        postconditions=[Postcondition(type="custom_unknown", description="opaque", enforce=True)],
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    dai = Daisugi(pathway_store=False, cache=False, strict=True)
    session = asyncio.run(dai.run(plan, env))
    assert not session.verification.ok


def test_daisugi_run_strict_kwarg_overrides_constructor_strict():
    """v0.28.3 follow-up — the missing cell of the 3-state truth table:
    Daisugi(strict=True).run(strict=False) MUST downgrade to non-strict.
    The base v0.28.3 PR proved each kwarg works in isolation; this proves
    the precedence chain — method kwarg wins over constructor."""
    import asyncio
    from opendaisugi.models import Postcondition

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes="low",
        postconditions=[Postcondition(type="custom_unknown", description="opaque", enforce=True)],
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    dai = Daisugi(pathway_store=False, cache=False, strict=True)
    # Construct: strict-on. Run: strict-off (False, not None) — must win.
    session = asyncio.run(dai.run(plan, env, strict=False))
    assert session.verification.ok, (
        "Daisugi.run(strict=False) must override Daisugi(strict=True)"
    )


def test_daisugi_verify_honors_constructor_strict():
    """v0.28.3 follow-up — Daisugi.verify() (not .run()) previously
    ignored the constructor strict, contradicting precedence claims."""
    from opendaisugi.models import Postcondition

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes="low",
        postconditions=[Postcondition(type="custom_unknown", description="opaque", enforce=True)],
    )
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    # No constructor strict → stake-based default (low → non-strict) → ok
    assert Daisugi(pathway_store=False, cache=False).verify(plan, env).ok
    # Constructor strict=True → opaque postcondition rejected
    assert not Daisugi(pathway_store=False, cache=False, strict=True).verify(plan, env).ok
    # Method strict=False overrides constructor strict=True
    assert Daisugi(pathway_store=False, cache=False, strict=True).verify(plan, env, strict=False).ok

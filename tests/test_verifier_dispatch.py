"""Dispatching to a compiled verifier client must never turn a failure into an
allow, must never let a client widen what the oracle denies, and must say
loudly when it fell back to the Python oracle."""

import logging
import sys
from pathlib import Path

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.verifier_dispatch import last_dispatch, verify_via

ENV_STRICT = Envelope(
    generated_by="test",
    task="run tests",
    permissions=Permission(shell=True, shell_allowlist=["pytest"]),
)


def _plan(command: str) -> ActionPlan:
    return ActionPlan(source="test", task="run tests", steps=[ShellStep(id="s1", command=command)])


def _fake_client(tmp_path: Path, body: str, name: str) -> Path:
    script = tmp_path / f"fake_client_{name}.py"
    script.write_text(body)
    return script


@pytest.fixture()
def register_fake(monkeypatch, tmp_path):
    def _register(body: str, name: str = "fake", profile: str = "full"):
        from opendaisugi.bench import options

        script = _fake_client(tmp_path, body, name)
        spec = options.ClientSpec(
            name=name,
            argv=(sys.executable, str(script)),
            probe=None,
            build_steps=(),
            build_cwd=".",
            readme="docs/spec/conformance.md",
            profile=profile,
        )
        monkeypatch.setitem(options.VERIFIER_CLIENTS, name, spec)
        return name

    return _register


AGREEING = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": case["expect"]["ok"],
                      "violations": case["expect"]["violations"]}), flush=True)
"""

CRASHING = "import sys; sys.exit(3)\n"
GARBAGE = "import sys; sys.stdin.read(); print('not json', flush=True)\n"
HANGING = "import sys, time; sys.stdin.read(); time.sleep(30)\n"
# A Core-profile client answers ok for anything. That is what a client that
# implements no Full-profile stage does on an envelope whose protection is a
# predicate. Named for the profile, not for "permissive", because the profile
# is the real reason the verdict cannot be trusted alone.
CORE_PROFILE = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": True, "violations": []}), flush=True)
"""

REFUSING = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": False,
                      "violations": [{"stage": "permissions", "step": "s1"}]}), flush=True)
"""


def test_a_working_client_answers_and_is_named(register_fake, tmp_path):
    name = register_fake(AGREEING)
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is True
    assert result.client == name
    assert result.fallback is None
    assert result.client_verdict is True


def test_a_working_client_reproduces_a_denial(register_fake, tmp_path):
    name = register_fake(AGREEING)
    result = verify_via(name, _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.violations
    assert result.fallback is None
    assert result.client_verdict is False


def _invariant_envelope() -> Envelope:
    return Envelope(
        generated_by="test",
        task="write a build artifact",
        permissions=Permission(
            shell=True, shell_allowlist=["pytest"], file_write=["build/**"], file_read=["**"]
        ),
        invariants=[
            Invariant(
                type="shell_only",
                description="every step is a shell step",
                expr={
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                },
            )
        ],
    )


def _mixed_plan() -> ActionPlan:
    return ActionPlan(
        source="test",
        task="write a build artifact",
        steps=[
            ShellStep(id="s1", command="pytest -q"),
            FileWriteStep(id="s2", path="build/out.txt", content="ok"),
        ],
    )


def test_dispatch_never_allows_what_the_oracle_denies(register_fake, tmp_path):
    """A Core-profile client runs delegation, permissions and DAG and then says
    ok. That is what clients/lean/DaisugiVerify/Verify.lean does. On an
    envelope whose only protection is an invariant, its allow must not stand,
    or selecting `lean` would silently switch off the predicate algebra for
    every envelope."""
    from opendaisugi.verify import verify

    envelope = _invariant_envelope()
    plan = _mixed_plan()
    assert verify(plan, envelope, strict=None).ok is False

    name = register_fake(CORE_PROFILE, name="core", profile="core")
    result = verify_via(name, plan, envelope, root=tmp_path / "gate")
    assert result.ok is False, "a core-profile allow overrode the oracle"
    assert result.client_verdict is True, "the client's own allow is still recorded"
    assert result.client == name
    assert result.fallback is None
    assert any(v.stage == "predicate" for v in result.violations)


def test_a_disagreement_is_logged_as_a_warning(register_fake, tmp_path, caplog):
    name = register_fake(CORE_PROFILE, name="core-warn", profile="core")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        verify_via(name, _mixed_plan(), _invariant_envelope(), root=tmp_path / "gate")
    assert any("core-warn" in r.message and "disagree" in r.message for r in caplog.records)


def test_a_client_may_tighten_an_allow_into_a_deny(register_fake, tmp_path):
    """The other direction is allowed. Diversity exists to catch what the
    oracle missed, so a client that refuses what the oracle accepts wins."""
    name = register_fake(REFUSING, name="strict")
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.client_verdict is False
    assert any("strict" in v.message for v in result.violations)
    assert any("pytest -q" in v.message for v in result.violations)


def test_a_dispatched_denial_keeps_the_oracle_s_own_violations(register_fake, tmp_path):
    """The ask prompt shows the oracle's clause and counterexample. A dispatch
    must carry them through, not replace them with the client's bare pair."""
    from opendaisugi.verify import verify

    oracle = verify(_plan("rm -rf /"), ENV_STRICT, strict=None)
    name = register_fake(AGREEING, name="agree")
    result = verify_via(name, _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    oracle_messages = [v.message for v in oracle.violations]
    assert oracle_messages
    assert [v.message for v in result.violations][: len(oracle_messages)] == oracle_messages


def test_dispatch_never_allows_on_client_failure(register_fake, tmp_path):
    """A crash, a timeout, garbage, or a missing binary must all resolve to
    the oracle's verdict, which for this plan is a denial."""
    for body in (CRASHING, GARBAGE, HANGING):
        name = register_fake(body, name=f"fake-{len(body)}")
        result = verify_via(
            name, _plan("rm -rf /"), ENV_STRICT, timeout_s=1.0, root=tmp_path / "gate"
        )
        assert result.ok is False, body[:20]
        assert result.fallback == "python"
        assert result.client_verdict is None


def test_an_unknown_client_name_falls_back_and_never_allows(tmp_path):
    result = verify_via("no-such-client", _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.fallback == "python"


def test_a_failure_still_returns_the_oracle_s_allow_when_the_plan_is_fine(tmp_path):
    result = verify_via("no-such-client", _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is True
    assert result.fallback == "python"
    assert result.client == "no-such-client"


def test_an_unbuilt_client_falls_back_with_its_build_command(monkeypatch, tmp_path, caplog):
    from opendaisugi.bench import options

    monkeypatch.setattr(options, "client_is_built", lambda spec: spec.probe is None)
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        result = verify_via("rust", _plan("rm -rf /"), ENV_STRICT, root=tmp_path / "gate")
    assert result.ok is False
    assert result.fallback == "python"
    assert any("cargo build" in r.message for r in caplog.records)


def test_the_fallback_warns_and_names_the_failure(register_fake, tmp_path, caplog):
    name = register_fake(CRASHING, name="crashy")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert any("crashy" in r.message for r in caplog.records)
    assert any("python" in r.message for r in caplog.records)


def test_the_last_dispatch_is_recorded_for_the_module_map(register_fake, tmp_path):
    root = tmp_path / "gate"
    name = register_fake(AGREEING, name="recorded")
    verify_via(name, _plan("pytest -q"), ENV_STRICT, root=root)
    record = last_dispatch(root)
    assert record["client"] == "recorded"
    assert record["ok"] is True
    verify_via("no-such-client", _plan("pytest -q"), ENV_STRICT, root=root)
    assert last_dispatch(root)["ok"] is False


def test_last_dispatch_on_a_fresh_root_is_empty(tmp_path):
    assert last_dispatch(tmp_path / "nothing") == {}


def test_the_verify_case_body_shape_did_not_change():
    """The additive result fields are safe only because make_verify_case
    projects through _normative_violations. If a result is ever dumped
    straight into a case, every content-addressed id in every corpus shifts
    silently."""
    from opendaisugi.conformance import make_verify_case
    from opendaisugi.verify import verify

    plan = _plan("pytest -q")
    result = verify(plan, ENV_STRICT, strict=None)
    body = make_verify_case(plan, ENV_STRICT, {"strict": None, "z3_timeout_ms": 500}, result)
    assert set(body) == {"kind", "v", "plan", "envelope", "options", "expect", "id"}
    assert set(body["expect"]) == {"ok", "violations"}
    for v in body["expect"]["violations"]:
        assert set(v) == {"stage", "step"}


INCONSISTENT = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": True,
                      "violations": [{"stage": "permissions", "step": "s1"}]}), flush=True)
"""

TWO_VERDICTS = """
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    case = json.loads(line)
    print(json.dumps({"id": case["id"], "ok": True, "violations": []}), flush=True)
    print(json.dumps({"id": case["id"], "ok": False,
                      "violations": [{"stage": "permissions", "step": "s1"}]}), flush=True)
"""


def test_an_allow_that_lists_violations_is_a_failed_client_not_an_allow(
    register_fake, tmp_path, caplog
):
    """A client whose final ok disagrees with its own list has a bug. Its
    finding must not be lost behind the allow, so the verdict is refused."""
    name = register_fake(INCONSISTENT, name="inconsistent")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.fallback == "python"
    assert result.client_verdict is None
    assert any("inconsistent" in r.message for r in caplog.records)


def test_two_verdicts_for_one_case_are_a_failed_client(register_fake, tmp_path, caplog):
    """A provisional allow followed by a final deny must not be read as the
    allow. More than one verdict for one id is refused as a whole."""
    name = register_fake(TWO_VERDICTS, name="twice")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.verifier_dispatch"):
        result = verify_via(name, _plan("pytest -q"), ENV_STRICT, root=tmp_path / "gate")
    assert result.fallback == "python"
    assert result.client_verdict is None
    assert any("2 verdicts" in r.message for r in caplog.records)


def test_the_client_only_gets_the_time_the_oracle_left(register_fake, tmp_path, monkeypatch):
    """`timeout_s` bounds the whole call. A slow oracle leaves the client
    less, and a client with no time left is a failed client, so the gate's
    join never fires on the client's account."""
    import importlib
    import time

    verify_mod = importlib.import_module("opendaisugi.verify")
    real = verify_mod.verify

    def _slow(plan, envelope, **kwargs):
        time.sleep(0.3)
        return real(plan, envelope, **kwargs)

    monkeypatch.setattr(verify_mod, "verify", _slow)
    name = register_fake(HANGING, name="hang")
    t0 = time.monotonic()
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, timeout_s=0.2, root=tmp_path / "gate")
    assert time.monotonic() - t0 < 1.0
    assert result.ok is True
    assert result.fallback == "python"
    assert "no time left" in last_dispatch(tmp_path / "gate")["error"]


def test_a_fast_oracle_leaves_the_client_the_rest_of_the_budget(register_fake, tmp_path):
    import time

    name = register_fake(HANGING, name="hang")
    t0 = time.monotonic()
    result = verify_via(name, _plan("pytest -q"), ENV_STRICT, timeout_s=0.5, root=tmp_path / "gate")
    elapsed = time.monotonic() - t0
    assert 0.4 < elapsed < 1.5, elapsed
    assert result.fallback == "python"

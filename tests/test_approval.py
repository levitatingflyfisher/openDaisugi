"""Unit tests for ApprovalStrategy implementations."""

import pytest

from opendaisugi.approval import (
    AllowlistBypassStrategy,
    ApprovalDecision,
    ApprovalStrategy,
    CallbackStrategy,
    DenyStrategy,
    EnvVarStrategy,
    TtyPromptStrategy,
    default_strategy,
)
from opendaisugi.exceptions import NotTerminalError
from opendaisugi.models import FileWriteStep, ShellStep, Envelope, Permission


def _env(allowlist: list[str] | None = None) -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=allowlist or []),
    )


def _shell_step(cmd: str) -> ShellStep:
    return ShellStep(id="s1", command=cmd)


def test_approval_decision_is_frozen():
    from dataclasses import FrozenInstanceError
    d = ApprovalDecision(approved=True, approved_by="allowlist", reason="ok")
    with pytest.raises(FrozenInstanceError):
        d.approved = False


def test_deny_strategy_always_denies():
    s = DenyStrategy()
    decision = s.decide(_shell_step("echo"), _env())
    assert decision.approved is False
    assert decision.approved_by == "denied"


def test_allowlist_bypass_approves_matching_first_token():
    s = AllowlistBypassStrategy(inner=DenyStrategy())
    decision = s.decide(_shell_step("echo hi"), _env(["echo"]))
    assert decision.approved is True
    assert decision.approved_by == "allowlist"
    assert "echo" in decision.reason


def test_allowlist_bypass_delegates_when_no_match():
    """Non-allowlisted commands fall through to the inner strategy."""
    s = AllowlistBypassStrategy(inner=DenyStrategy())
    decision = s.decide(_shell_step("rm -rf /"), _env(["echo", "ls"]))
    assert decision.approved is False
    assert decision.approved_by == "denied"


def test_allowlist_bypass_empty_allowlist_delegates():
    s = AllowlistBypassStrategy(inner=DenyStrategy())
    decision = s.decide(_shell_step("echo hi"), _env([]))
    assert decision.approved is False


def test_allowlist_bypass_skips_non_shell_steps():
    """file_write / file_read / network steps pass to inner strategy."""
    s = AllowlistBypassStrategy(inner=DenyStrategy())
    step = FileWriteStep(id="s1", path="/tmp/x", content="hi")
    decision = s.decide(step, _env(["echo"]))
    assert decision.approved is False


# v0.28.4 — defense-in-depth. AllowlistBypassStrategy was previously a
# first-token allowlist check. ``cat > /etc/passwd`` auto-approved at the
# human-in-the-loop layer despite verify rejecting it. The strategy now
# runs the same metachar gate verify does, so metachar-laden commands
# fall through to the inner strategy regardless of head match.


@pytest.mark.parametrize("cmd", [
    "cat > /etc/passwd",
    "cat < /etc/shadow",
    "cat >> /etc/passwd",
    "echo $(rm -rf /)",
    "echo hi; rm -rf /",
    "echo hi\nrm -rf /",
])
def test_allowlist_bypass_rejects_metachar_laden_commands(cmd):
    """v0.28.4 — head-token allowlist check is gated by metachar inspection."""
    s = AllowlistBypassStrategy(inner=DenyStrategy())
    head = cmd.split()[0]
    decision = s.decide(_shell_step(cmd), _env([head, "echo"]))
    assert decision.approved is False, (
        f"AllowlistBypass should fall through to inner on metachar-laden {cmd!r}"
    )
    assert decision.approved_by == "denied"


def test_protocol_conformance():
    assert isinstance(DenyStrategy(), ApprovalStrategy)
    assert isinstance(AllowlistBypassStrategy(DenyStrategy()), ApprovalStrategy)


def test_callback_strategy_approves_when_callback_returns_true():
    def always_yes(step, envelope):
        return True
    s = CallbackStrategy(always_yes)
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is True
    assert decision.approved_by == "callback"


def test_callback_strategy_denies_when_callback_returns_false():
    def always_no(step, envelope):
        return False
    s = CallbackStrategy(always_no)
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is False
    assert decision.approved_by == "callback"


def test_envvar_always_approves(monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    s = EnvVarStrategy(fallback=DenyStrategy())
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is True
    assert decision.approved_by == "env"


def test_envvar_never_denies(monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "never")
    s = EnvVarStrategy(fallback=DenyStrategy())
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is False
    assert decision.approved_by == "env"


def test_envvar_interactive_falls_through(monkeypatch):
    """'interactive' means 'defer to fallback'."""
    monkeypatch.setenv("DAISUGI_APPROVE", "interactive")
    s = EnvVarStrategy(fallback=DenyStrategy())
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is False
    assert decision.approved_by == "denied"


def test_envvar_unset_falls_through(monkeypatch):
    monkeypatch.delenv("DAISUGI_APPROVE", raising=False)
    s = EnvVarStrategy(fallback=DenyStrategy())
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved_by == "denied"


def test_envvar_invalid_value_raises(monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "maybe")
    s = EnvVarStrategy(fallback=DenyStrategy())
    with pytest.raises(ValueError, match="DAISUGI_APPROVE"):
        s.decide(_shell_step("anything"), _env())


def test_tty_prompt_raises_when_no_tty(monkeypatch):
    monkeypatch.setattr("sys.stdin.isatty", lambda: False)
    monkeypatch.setattr("sys.stdout.isatty", lambda: True)
    s = TtyPromptStrategy()
    with pytest.raises(NotTerminalError):
        s.decide(_shell_step("anything"), _env())


def test_tty_prompt_approves_on_y(monkeypatch):
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    monkeypatch.setattr("sys.stdout.isatty", lambda: True)
    monkeypatch.setattr("builtins.input", lambda _prompt: "y")
    s = TtyPromptStrategy()
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is True
    assert decision.approved_by == "tty"


def test_tty_prompt_denies_on_n(monkeypatch):
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    monkeypatch.setattr("sys.stdout.isatty", lambda: True)
    monkeypatch.setattr("builtins.input", lambda _prompt: "n")
    s = TtyPromptStrategy()
    decision = s.decide(_shell_step("anything"), _env())
    assert decision.approved is False
    assert decision.approved_by == "tty"


def test_default_strategy_short_circuits_on_allowlist(monkeypatch):
    """default_strategy() must auto-approve allowlisted commands without prompting."""
    monkeypatch.delenv("DAISUGI_APPROVE", raising=False)
    s = default_strategy()
    decision = s.decide(_shell_step("echo hi"), _env(["echo"]))
    assert decision.approved is True
    assert decision.approved_by == "allowlist"


def test_default_strategy_respects_env_override(monkeypatch):
    monkeypatch.setenv("DAISUGI_APPROVE", "always")
    s = default_strategy()
    decision = s.decide(_shell_step("rm -rf /"), _env([]))
    assert decision.approved is True
    assert decision.approved_by == "env"

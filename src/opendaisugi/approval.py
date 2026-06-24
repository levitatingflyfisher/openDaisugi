"""Pluggable step-approval strategies.

Strategies compose left-to-right: the supervisor's default stack routes every
step through ``AllowlistBypassStrategy`` first so the 95% common case of
allowlisted commands never prompts. Only commands outside the allowlist reach
the inner strategy (TTY prompt, env var, callback, or deny).

This file exposes the protocol and two simplest strategies; the
interactive/env/callback strategies are added in a sibling task.
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass
from typing import Callable, Literal, Protocol, runtime_checkable

from opendaisugi.exceptions import NotTerminalError
from opendaisugi.models import ActionStep, Envelope

_ApprovedBy = Literal["allowlist", "tty", "env", "callback", "denied"]


@dataclass(frozen=True)
class ApprovalDecision:
    approved: bool
    approved_by: _ApprovedBy
    reason: str


@runtime_checkable
class ApprovalStrategy(Protocol):
    def decide(
        self,
        step: ActionStep,
        envelope: Envelope,
    ) -> ApprovalDecision: ...


class DenyStrategy:
    """Approves nothing. Useful as a terminal in a stack."""

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        return ApprovalDecision(
            approved=False,
            approved_by="denied",
            reason="DenyStrategy rejects all steps",
        )


class AllowlistBypassStrategy:
    """Auto-approves shell steps whose first token is in the envelope allowlist.

    Falls through to ``inner`` for non-shell steps or non-matching commands.
    """

    def __init__(self, inner: ApprovalStrategy) -> None:
        self._inner = inner

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        if step.type != "shell":
            return self._inner.decide(step, envelope)
        cmd = (step.command or "").strip()
        if not cmd:
            return self._inner.decide(step, envelope)
        # v0.28.4: reuse verify's metachar gate before short-circuiting on
        # allowlist match. Pre-v0.28.4 the approval strategy auto-approved on
        # ``cmd.split()[0]`` alone, so ``cat > /etc/passwd`` (head ``cat``
        # in allowlist) was auto-approved at the human-in-the-loop layer
        # even though verify would reject it. Defense-in-depth: every layer
        # that can auto-approve MUST run the same metachar check.
        from opendaisugi.verify import _SHELL_METACHAR_RE

        if _SHELL_METACHAR_RE.search(cmd):
            return self._inner.decide(step, envelope)
        first_token = cmd.split()[0]
        allowlist = envelope.permissions.shell_allowlist or []
        if first_token in allowlist:
            return ApprovalDecision(
                approved=True,
                approved_by="allowlist",
                reason=f"'{first_token}' is in shell_allowlist",
            )
        return self._inner.decide(step, envelope)


class CallbackStrategy:
    """Delegates approval to a caller-supplied function.

    Enables programmatic approval — this is the hook Hermes/OpenClaw will use
    when embedding the supervisor as a library (they bring their own UX).
    """

    def __init__(
        self,
        callback: Callable[[ActionStep, Envelope], bool],
    ) -> None:
        self._callback = callback

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        ok = bool(self._callback(step, envelope))
        return ApprovalDecision(
            approved=ok,
            approved_by="callback",
            reason="user-supplied callback" + (" approved" if ok else " denied"),
        )


class EnvVarStrategy:
    """Honors ``DAISUGI_APPROVE`` environment variable.

    ``always`` → auto-approve, ``never`` → auto-deny, ``interactive`` or
    ``auto`` or unset → defer to ``fallback``. Any other value raises
    ``ValueError`` so typos surface early rather than silently denying.
    """

    _VALID = {"always", "never", "interactive", "auto"}

    def __init__(self, fallback: ApprovalStrategy) -> None:
        self._fallback = fallback

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        value = os.environ.get("DAISUGI_APPROVE", "").strip().lower()
        if value == "":
            return self._fallback.decide(step, envelope)
        if value not in self._VALID:
            raise ValueError(
                f"DAISUGI_APPROVE={value!r} is not a valid value; "
                f"expected one of {sorted(self._VALID)}"
            )
        if value == "always":
            return ApprovalDecision(
                approved=True, approved_by="env",
                reason="DAISUGI_APPROVE=always",
            )
        if value == "never":
            return ApprovalDecision(
                approved=False, approved_by="env",
                reason="DAISUGI_APPROVE=never",
            )
        return self._fallback.decide(step, envelope)


class TtyPromptStrategy:
    """Interactive y/N prompt when stdin and stdout are both TTYs."""

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        if not (sys.stdin.isatty() and sys.stdout.isatty()):
            raise NotTerminalError(
                "TtyPromptStrategy requires an interactive terminal"
            )
        cmd = getattr(step, "command", None) or getattr(step, "path", None) or getattr(step, "url", None) or step.id
        prompt = f"Approve step {step.id!r} ({cmd})? [y/N] "
        answer = input(prompt).strip().lower()
        ok = answer in ("y", "yes")
        return ApprovalDecision(
            approved=ok,
            approved_by="tty",
            reason=f"user answered {answer!r}",
        )


def default_strategy() -> ApprovalStrategy:
    """Assemble the default strategy stack.

    Order: allowlist → env var → TTY prompt → deny. Allowlist short-circuits
    almost all calls; env var lets CI/integrations override; TTY prompt is the
    humane default; deny is the backstop.
    """
    return AllowlistBypassStrategy(
        inner=EnvVarStrategy(
            fallback=_TtyOrDeny(),
        ),
    )


class _TtyOrDeny:
    """Try TTY prompt; if no terminal is attached, fall back to deny.

    Keeps ``default_strategy()`` usable in both interactive shells and CI
    pipelines without special-casing at the call site.
    """

    def decide(self, step: ActionStep, envelope: Envelope) -> ApprovalDecision:
        try:
            return TtyPromptStrategy().decide(step, envelope)
        except NotTerminalError:
            return ApprovalDecision(
                approved=False,
                approved_by="denied",
                reason="no TTY available; set DAISUGI_APPROVE=always for non-interactive approval",
            )

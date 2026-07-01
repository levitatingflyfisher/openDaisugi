"""Call-time tool gate — the enforce-mode counterpart of the passive hook.

Where :mod:`opendaisugi.hook` observes a host harness's tool calls and fails
open (correct for capture, wrong for protection), this module takes each
intercepted call, synthesizes it into a one-step plan, and proves it inside
the session's registered envelope *before it runs* (ADR-0007). The two share
a seam, not a failure policy:

- **enforce** mode is fail-closed: unknown tool, unparseable input, internal
  exception, or a slow verifier all DENY. The gate owns an inner timeout that
  itself denies, because every known host's *outer* hook timeout fails open.
- **shadow** mode (the default) observes: every call is evaluated and the
  would-have-denied verdict recorded, but the host always gets its allow
  contract. Shadow mode is observation, not protection.

The full :func:`opendaisugi.verify.verify` pipeline runs per call — not the
``verify_step`` hot path — so plan-level strict-mode checks are never skipped
at the boundary, and strictness is resolved from the envelope's stakes
(``strict=None``), never relaxed at the gate.
"""

from __future__ import annotations

import json
import os
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from opendaisugi.hook import (
    _payload_to_record,
    _records_to_steps,
    _safe_session_id,
    stdout_for_format,
)
from opendaisugi.models import ActionPlan, Envelope, Permission
from opendaisugi.verify import verify

DEFAULT_GATE_ROOT = Path.home() / ".opendaisugi" / "gate"
_DISARM_FILENAME = "DISARMED"
_DEFAULT_VERIFY_TIMEOUT_S = 10.0


@dataclass
class GateDecision:
    """The gate's verdict on one intercepted tool call.

    ``allow`` is what the host is told in the current mode; ``would_deny``
    is what enforce mode would have done — in shadow mode the pair diverges
    by design, and the shadow report is built from ``would_deny``.
    """

    allow: bool
    would_deny: bool
    reason: str
    mode: str
    tool_name: str | None = None
    step_type: str | None = None
    detail: str = ""
    elapsed_ms: float = 0.0


def _deny(mode: str, reason: str, *, tool_name: str | None = None,
          step_type: str | None = None, detail: str = "",
          t0: float) -> GateDecision:
    return GateDecision(
        allow=(mode == "shadow"),
        would_deny=True,
        reason=reason,
        mode=mode,
        tool_name=tool_name,
        step_type=step_type,
        detail=detail,
        elapsed_ms=(time.monotonic() - t0) * 1000,
    )


def _verify_with_timeout(plan: ActionPlan, envelope: Envelope,
                         timeout_s: float):
    """Run verify() in a worker thread with an inner deny-on-timeout.

    Returns the VerificationResult, or raises TimeoutError when the verifier
    outlives the budget. The worker is a daemon thread — a hung Z3 query
    cannot pin the gate process open past its own deadline.
    """
    box: list[Any] = []
    err: list[BaseException] = []

    def _run() -> None:
        try:
            box.append(verify(plan, envelope, strict=None))
        except BaseException as exc:  # noqa: BLE001 — re-raised on the caller side
            err.append(exc)

    worker = threading.Thread(target=_run, daemon=True)
    worker.start()
    worker.join(timeout_s)
    if worker.is_alive():
        raise TimeoutError(
            f"verifier exceeded the gate's inner time budget ({timeout_s}s)"
        )
    if err:
        raise err[0]
    return box[0]


def evaluate_record(record: dict[str, Any], envelope: Envelope, *,
                    mode: str = "shadow",
                    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                    ) -> GateDecision:
    """Decide one already-normalized capture record against an envelope.

    Deny-by-default: every failure path inside this function resolves to a
    deny decision, never an exception to the caller.
    """
    t0 = time.monotonic()
    tool_name = record.get("tool_name")
    step_type = record.get("step_type")
    detail = str(
        record.get("command") or record.get("path") or record.get("url") or ""
    )
    try:
        steps = _records_to_steps([record])
        if not steps:
            return _deny(mode, f"could not synthesize a step for tool {tool_name!r}",
                         tool_name=tool_name, step_type=step_type, detail=detail, t0=t0)
        plan = ActionPlan(source="call-time-gate", task=envelope.task, steps=steps)
        result = _verify_with_timeout(plan, envelope, verify_timeout_s)
        if result.ok:
            return GateDecision(
                allow=True, would_deny=False, reason="verified in envelope",
                mode=mode, tool_name=tool_name, step_type=step_type,
                detail=detail, elapsed_ms=(time.monotonic() - t0) * 1000,
            )
        summary = "; ".join(
            f"{v.stage}: {v.message}" for v in result.violations
        ) or "verification failed"
        return _deny(mode, summary, tool_name=tool_name, step_type=step_type,
                     detail=detail, t0=t0)
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}",
                     tool_name=tool_name, step_type=step_type, detail=detail, t0=t0)


def evaluate_call(payload: Any, envelope: Envelope, *,
                  mode: str = "shadow",
                  verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                  ) -> GateDecision:
    """Decide one raw hook payload against an envelope. Deny-by-default.

    Never raises: malformed payloads, unknown tools, verifier errors, and
    verifier timeouts all come back as deny decisions (allowed-but-flagged
    in shadow mode).
    """
    t0 = time.monotonic()
    try:
        if not isinstance(payload, dict):
            return _deny(mode, "hook payload is not a JSON object", t0=t0)
        tool_name = (
            payload.get("tool_name") or payload.get("tool") or payload.get("name")
        )
        if not tool_name:
            return _deny(mode, "no tool name in hook payload", t0=t0)
        record = _payload_to_record(payload)
        if record is None:
            return _deny(
                mode,
                f"unrecognized tool {tool_name!r} — not in the gate's "
                "classification map, denied by default",
                tool_name=str(tool_name), t0=t0,
            )
        return evaluate_record(
            record, envelope, mode=mode, verify_timeout_s=verify_timeout_s,
        )
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}", t0=t0)


# ---------------------------------------------------------------------------
# Envelope registration channel + disarm switch + host contract (I/O layer)
# ---------------------------------------------------------------------------

def _envelopes_dir(root: Path) -> Path:
    return root / "envelopes"


def _shadow_dir(root: Path) -> Path:
    return root / "shadow"


def _mkdir_private(d: Path) -> None:
    d.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        os.chmod(d, 0o700)
    except OSError:
        pass


# A conservative starting shell allowlist for a generated envelope: read-
# oriented inspection plus version control and test runners. It is
# deliberately NOT a blanket ``*`` and omits the interpreters (``bash``,
# ``sh``) and anything that trivially shells out, because the operator is
# meant to *review and tighten or widen* this before trusting enforce mode.
# Shell allowlisting matches only the command head, and shadow mode is where
# you discover what your real session actually needs.
_STARTER_SHELL_ALLOWLIST: tuple[str, ...] = (
    "cat", "cd", "echo", "find", "git", "grep", "head", "ls", "npm", "cargo",
    "printf", "pwd", "pytest", "python", "python3", "rg", "sort", "tail",
    "uniq", "wc", "which",
)


def starter_envelope(workspace: Path, *, stakes: str = "medium") -> Envelope:
    """Generate a reviewable starter envelope for an existing session.

    This is the drafted-then-reviewed answer to "where does the envelope come
    from?" for an operator's *own* running session (roadmap Stage 1's open
    sub-problem; the onboarding funnel of Stage 6). It grants read/write
    within one workspace, a conservative shell head allowlist, and NO network
    — a sane, tight default that the operator edits before enforcing. It is
    not a security guarantee on its own; it is a starting point that shadow
    mode and `daisugi gate report` help tune.
    """
    ws = str(Path(workspace).resolve())
    return Envelope(
        generated_by="opendaisugi.gate.starter_envelope",
        task=f"session in {ws}",
        permissions=Permission(
            file_read=[f"{ws}/**"],
            file_write=[f"{ws}/**"],
            shell=True,
            shell_allowlist=sorted(_STARTER_SHELL_ALLOWLIST),
            network=False,
            max_execution_time_s=60,
            max_output_size_mb=20,
        ),
        stakes=stakes,
    )


def register_envelope(envelope: Envelope, *, session_id: str | None = None,
                      root: Path = DEFAULT_GATE_ROOT) -> Path:
    """Register an envelope for the gate to check calls against.

    With a ``session_id`` the envelope binds to that session; without one it
    becomes the ``default`` envelope every unmatched session falls back to.
    Files are private (0700 dir / 0600 file) — envelopes reveal what a
    session is allowed to touch.
    """
    d = _envelopes_dir(root)
    _mkdir_private(d)
    name = _safe_session_id(session_id) if session_id else "default"
    path = d / f"{name}.json"
    path.write_text(envelope.model_dump_json(indent=2), encoding="utf-8")
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass
    return path


def load_envelope(session_id: str | None, *,
                  root: Path = DEFAULT_GATE_ROOT) -> Envelope | None:
    """Load the envelope for a session: exact match first, then ``default``."""
    candidates = []
    if session_id:
        candidates.append(_safe_session_id(session_id))
    candidates.append("default")
    for name in candidates:
        path = _envelopes_dir(root) / f"{name}.json"
        if path.exists():
            return Envelope.model_validate_json(path.read_text(encoding="utf-8"))
    return None


def disarm(root: Path = DEFAULT_GATE_ROOT) -> Path:
    """One-command kill switch: an armed gate allows everything while the
    marker exists. Deliberately requires no allowed tool call — the operator
    runs it from any shell, outside the gated agent."""
    _mkdir_private(root)
    marker = root / _DISARM_FILENAME
    marker.write_text("disarmed by operator\n", encoding="utf-8")
    return marker


def arm(root: Path = DEFAULT_GATE_ROOT) -> None:
    """Remove the disarm marker; the gate resumes evaluating calls."""
    marker = root / _DISARM_FILENAME
    if marker.exists():
        marker.unlink()


def is_disarmed(root: Path = DEFAULT_GATE_ROOT) -> bool:
    return (root / _DISARM_FILENAME).exists()


@dataclass
class GateOutcome:
    """What the gate process should emit to the host: stdout, stderr, exit
    code — plus the decision for logging/inspection."""

    stdout: str
    stderr: str
    exit_code: int
    decision: GateDecision


def _outcome(decision: GateDecision, fmt: str) -> GateOutcome:
    deny_now = decision.mode == "enforce" and decision.would_deny
    if fmt == "claude":
        if deny_now:
            return GateOutcome(
                stdout="",
                stderr=f"openDaisugi gate: DENIED — {decision.reason}",
                exit_code=2,
                decision=decision,
            )
        return GateOutcome(
            stdout=stdout_for_format("claude", block=False),
            stderr="", exit_code=0, decision=decision,
        )
    return GateOutcome(
        stdout=stdout_for_format(fmt, block=deny_now, reason=decision.reason),
        stderr="", exit_code=0, decision=decision,
    )


def _log_shadow(root: Path, session_id: str | None,
                decision: GateDecision,
                payload_session_id: str | None = None) -> None:
    """Best-effort JSONL decision log — the raw material of the shadow
    report. Never raises; a logging failure must not change a verdict."""
    try:
        d = _shadow_dir(root)
        _mkdir_private(d)
        path = d / f"{_safe_session_id(session_id)}.jsonl"
        newly_created = not path.exists()
        rec = {
            "at": time.time(),
            "session_id": _safe_session_id(session_id),
            # What the payload claimed, kept even when the envelope was pinned
            # to something else — so a report can show a mismatch.
            "payload_session_id": payload_session_id,
            "tool_name": decision.tool_name,
            "step_type": decision.step_type,
            "detail": decision.detail,
            "mode": decision.mode,
            "allow": decision.allow,
            "would_deny": decision.would_deny,
            "reason": decision.reason,
            "elapsed_ms": round(decision.elapsed_ms, 3),
        }
        with path.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec) + "\n")
        if newly_created:
            try:
                os.chmod(path, 0o600)
            except OSError:
                pass
    except Exception:  # noqa: BLE001 — logging is best-effort by contract
        pass


def gate_and_contract(raw: bytes, *, root: Path = DEFAULT_GATE_ROOT,
                      fmt: str = "claude", mode: str = "shadow",
                      verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                      captures_root: Path | None = None,
                      pin_session: str | None = None,
                      ) -> GateOutcome:
    """Full gate entry: raw hook stdin → decision → host contract.

    Failure policy is mode-selected (ADR-0007): enforce fails CLOSED (any
    error here denies with exit 2), shadow fails OPEN (observation must
    never break the host). Every decision is appended to the shadow log,
    in both modes — enforce sessions produce the same report material.

    With ``captures_root``, calls the gate *allows* are also mirrored into
    passive-capture format (best-effort) so a gated session feeds the same
    captures → to-trace → journal pipeline distillation already reads.
    Denied calls are never mirrored — they didn't happen.

    ``pin_session`` fixes which registered envelope is used, ignoring the
    session id in the payload. Authorization must not key on input the
    caller can influence: unpinned, a payload claiming another session's id
    is checked against *that* session's envelope, which may be more
    permissive. The hook command supplies the pin from outside anything the
    agent can rewrite — the same principle as the sub-agent gate root living
    outside its workspace.
    """
    t0 = time.monotonic()
    try:
        if is_disarmed(root):
            decision = GateDecision(
                allow=True, would_deny=False,
                reason="gate disarmed by operator (marker file present)",
                mode=mode, elapsed_ms=(time.monotonic() - t0) * 1000,
            )
            _log_shadow(root, None, decision)
            return _outcome(decision, fmt)
        try:
            text = raw.decode("utf-8", "replace")
            payload = json.loads(text) if text.strip() else None
        except Exception:  # noqa: BLE001 — malformed stdin is a deny, not a crash
            payload = None
        payload_session = (
            payload.get("session_id") if isinstance(payload, dict) else None
        )
        # Pinned wins: the payload's claim is recorded but never authorizes.
        session_id = pin_session or payload_session
        envelope = load_envelope(session_id, root=root)
        if envelope is None:
            decision = _deny(
                mode,
                "no envelope registered for this session — run "
                "`daisugi gate register <envelope.json>` to authorize it, or "
                "`daisugi gate disarm` to switch the gate off",
                t0=t0,
            )
        elif payload is None:
            decision = _deny(mode, "hook payload was not parseable JSON", t0=t0)
        else:
            decision = evaluate_call(
                payload, envelope, mode=mode, verify_timeout_s=verify_timeout_s,
            )
        _log_shadow(root, session_id, decision, payload_session_id=payload_session)
        if captures_root is not None and decision.allow and isinstance(payload, dict):
            try:
                from opendaisugi.hook import record_call
                record_call(payload, root=captures_root)
            except Exception:  # noqa: BLE001 — mirroring is best-effort
                pass
        return _outcome(decision, fmt)
    except Exception as exc:  # noqa: BLE001 — mode-selected failure policy
        decision = _deny(mode, f"gate I/O error (denied fail-closed): {exc}", t0=t0)
        if mode != "enforce":
            decision = GateDecision(
                allow=True, would_deny=True,
                reason=f"gate I/O error (shadow mode allows): {exc}",
                mode=mode, elapsed_ms=(time.monotonic() - t0) * 1000,
            )
        return _outcome(decision, fmt)


# ---------------------------------------------------------------------------
# Shadow report + capture replay
# ---------------------------------------------------------------------------

def _is_false_positive_candidate(reason: str) -> bool:
    """Classify a would-deny as a likely false positive worth operator review.

    Two known classes (the product's false-positive economics, per the
    roadmap): compound-command metachar denials (the command may be benign;
    the gate can't prove it and offers a decomposition instead) and host
    tools the classification map doesn't know (TodoWrite, Task, …) which
    deny-by-default sweeps up wholesale.
    """
    return "metacharacters" in reason or reason.startswith("unrecognized tool")


def _build_report(records: list[dict[str, Any]]) -> dict[str, Any]:
    denied = [r for r in records if r.get("would_deny")]
    reasons: dict[str, int] = {}
    for r in denied:
        key = (r.get("reason") or "")[:120]
        reasons[key] = reasons.get(key, 0) + 1
    return {
        "calls": len(records),
        "allowed": sum(1 for r in records if not r.get("would_deny")),
        "would_deny": len(denied),
        "reasons": reasons,
        "denied": denied,
        "false_positive_candidates": [
            r for r in denied
            if _is_false_positive_candidate(r.get("reason") or "")
        ],
    }


def shadow_report(*, root: Path = DEFAULT_GATE_ROOT,
                  session_id: str | None = None) -> dict[str, Any]:
    """Summarize the shadow log: what an enforcing gate would have denied.

    Denied records are included verbatim so the operator can adjudicate each
    one; the ``false_positive_candidates`` subset flags the two known
    over-denial classes (compound-command metachars, unrecognized host
    tools). One session, or all sessions when ``session_id`` is None.
    """
    d = _shadow_dir(root)
    files = (
        [d / f"{_safe_session_id(session_id)}.jsonl"] if session_id
        else sorted(d.glob("*.jsonl")) if d.exists() else []
    )
    records: list[dict[str, Any]] = []
    for f in files:
        if not f.exists():
            continue
        for line in f.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    return _build_report(records)


def replay_captures(captures_jsonl: Path, envelope: Envelope, *,
                    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                    ) -> dict[str, Any]:
    """Run a passively captured session back through the gate, offline.

    This is how an operator tunes an envelope against a real session before
    trusting enforce mode: every captured call is decided in shadow terms
    against ``envelope`` and summarized like :func:`shadow_report` — false
    positive candidates included. Nothing is executed and nothing is denied;
    the captures are historical.
    """
    records: list[dict[str, Any]] = []
    for line in captures_jsonl.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        try:
            cap = json.loads(line)
        except json.JSONDecodeError:
            continue
        decision = evaluate_record(
            cap, envelope, mode="shadow", verify_timeout_s=verify_timeout_s,
        )
        records.append({
            "at": cap.get("captured_at"),
            "session_id": cap.get("session_id"),
            "tool_name": decision.tool_name,
            "step_type": decision.step_type,
            "detail": decision.detail,
            "mode": "shadow",
            "allow": decision.allow,
            "would_deny": decision.would_deny,
            "reason": decision.reason,
            "elapsed_ms": round(decision.elapsed_ms, 3),
        })
    return _build_report(records)


# ---------------------------------------------------------------------------
# Host wiring: settings emitter + lean hook entry
# ---------------------------------------------------------------------------

def gate_settings_json(*, mode: str = "shadow",
                       root: Path = DEFAULT_GATE_ROOT,
                       fmt: str = "claude",
                       hook_timeout_s: int = 30,
                       python: str | None = None,
                       verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                       captures_root: Path | None = None,
                       session: str | None = None,
                       ) -> str:
    """Return the Claude Code hooks-settings JSON that wires in the gate.

    Usable inline (``claude --settings "$(daisugi gate settings ...)"``) or
    merged into a settings file. The matcher is ``*`` — total by design;
    tool classification happens *inside* the gate so an unmatched tool can
    never be silently allowed. The command uses ``python -m opendaisugi.gate``
    (argparse only), not the full typer CLI — the lean entry ADR-0007 names
    as the latency seam. The one-flag flip to protection is ``mode="enforce"``.

    The host-side ``timeout`` is a backstop only: on every known host an
    outer hook timeout fails OPEN, which is why the gate owns an inner
    ``verify_timeout_s`` that denies first.
    """
    import shlex
    import sys as _sys

    py = python or _sys.executable
    inner = min(verify_timeout_s, max(1.0, hook_timeout_s - 5.0))
    command = (
        f"{shlex.quote(py)} -m opendaisugi.gate"
        f" --mode {shlex.quote(mode)}"
        f" --root {shlex.quote(str(root))}"
        f" --format {shlex.quote(fmt)}"
        f" --verify-timeout {inner}"
    )
    if captures_root is not None:
        command += f" --captures-root {shlex.quote(str(captures_root))}"
    # Pin the envelope from OUTSIDE the agent: with --session set, a payload
    # claiming another session's id cannot select that session's (possibly
    # more permissive) envelope. The agent cannot rewrite this command.
    if session is not None:
        command += f" --session {shlex.quote(session)}"
    # Default-deny at the PROCESS boundary. The host runs `command` through a
    # shell (verified live), and on Claude Code any hook exit that is not 2 is
    # non-blocking — so a crashed gate (exit 1), or a package that fails to
    # import (where main() never runs to trap anything), would silently ALLOW.
    # `... || exit 2` maps every nonzero exit — including import failure, since
    # the whole invocation is the left operand — to a deny, while leaving exit 0
    # an allow and exit 2 a deny. `python -m …` is an external command, so its
    # nonzero exit triggers `||` (unlike a shell `exit` builtin). Live-verified:
    # without this, an exit-1 hook lets the read through; with it, it blocks.
    # ENFORCE only, and only for the claude contract (exit-code deny). In
    # SHADOW mode a crash must stay non-blocking — shadow never breaks the
    # host, so mapping a gate error to exit 2 (deny) would be the exact
    # regression shadow exists to avoid. hermes/openclaw deny via stdout JSON,
    # where this idiom is meaningless anyway (their crash behavior is part of
    # the documented 'unverified' enforcement class).
    if fmt == "claude" and mode == "enforce":
        command = f"{command} || exit 2"
    return json.dumps({
        "hooks": {
            "PreToolUse": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": command,
                    "timeout": hook_timeout_s,
                }],
            }],
        },
    })


def main(argv: list[str] | None = None) -> int:
    """Lean hook entry: ``python -m opendaisugi.gate`` (no typer import).

    Reads one hook payload from stdin, emits the host contract, and returns
    the process exit code (2 = deny on the Claude Code path). Kept argparse-
    only because hook round-trip latency is import-dominated; the full
    ``daisugi gate check`` command delegates here.
    """
    import argparse
    import sys as _sys

    parser = argparse.ArgumentParser(prog="opendaisugi.gate", add_help=True)
    parser.add_argument("--mode", choices=("shadow", "enforce"), default="shadow")
    parser.add_argument("--root", type=Path, default=DEFAULT_GATE_ROOT)
    parser.add_argument("--format", dest="fmt", default="claude")
    parser.add_argument("--verify-timeout", type=float,
                        default=_DEFAULT_VERIFY_TIMEOUT_S)
    parser.add_argument("--captures-root", type=Path, default=None)
    parser.add_argument(
        "--session", default=None,
        help="Pin the envelope to this registered session, ignoring the "
             "session id in the payload (authorization must not key on "
             "caller-influenceable input).",
    )
    args = parser.parse_args(argv)

    # Fail-closed wrapper: ANY escape from here — a BaseException re-raised out
    # of the verify thread (which gate_and_contract's `except Exception` won't
    # catch), an error in print(), a broken stdout — must still deny in enforce
    # mode. Shadow mode has nothing to protect, so a crash there is allowed to
    # surface. The `|| exit 2` in the emitted command is the outer belt for the
    # case this can't reach (the package failing to import before main() runs).
    try:
        try:
            raw = _sys.stdin.buffer.read()
        except Exception:  # noqa: BLE001 — a broken stdin still gets a verdict
            raw = b""
        out = gate_and_contract(
            raw, root=args.root, fmt=args.fmt, mode=args.mode,
            verify_timeout_s=args.verify_timeout,
            captures_root=args.captures_root,
            pin_session=args.session,
        )
        if out.stdout:
            print(out.stdout)
        if out.stderr:
            print(out.stderr, file=_sys.stderr)
        return out.exit_code
    except BaseException as exc:  # noqa: BLE001 — deny-by-default on any escape
        if args.mode == "enforce":
            try:
                print(f"openDaisugi gate: DENIED (fail-closed on error): {exc}",
                      file=_sys.stderr)
            except Exception:  # noqa: BLE001 — even a broken stderr must not un-deny
                pass
            return 2
        return 0


if __name__ == "__main__":  # pragma: no cover — exercised via main() tests
    raise SystemExit(main())

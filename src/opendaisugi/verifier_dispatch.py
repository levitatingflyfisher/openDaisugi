"""Run a compiled conformance client beside the Python oracle at runtime.

The five clients already speak one wire protocol for conformance scoring, in
`docs/spec/conformance.md`: a case as JSON on stdin, a verdict as JSON on
stdout. This module reuses that protocol for a live verification, so the
runtime choice and the differential harness cannot drift apart.

The dispatch rule: a client may only ever tighten. The Python oracle always
runs, and the returned verdict is ``oracle.ok and client_ok``. A client that
runs cleanly can turn an allow into a deny. It can never turn a deny into an
allow, because a Core-profile client implements none of the predicate or Z3
stages, and its allow means "I did not look", not "this is safe". The
client's own answer is recorded as ``client_verdict`` so ``daisugi bench
verifier`` can attribute a disagreement, and a disagreement logs a WARNING.

The fallback rule is the other half of the safety argument. A client that is
missing, exits non-zero, writes garbage, or runs long is a FAILED client, and a
failed client's answer is never used. The oracle's verdict stands alone, the
result says ``fallback="python"``, and a WARNING names what broke. There is
no path here that turns a failure into an allow.

``daisugi modules`` marks a client ACTIVE only after a dispatch through it
succeeded, which is what ``last_dispatch`` records.
"""

from __future__ import annotations

import json
import logging
import subprocess
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from opendaisugi.models import ActionPlan, Envelope, VerificationResult, Violation

_log = logging.getLogger("opendaisugi.verifier_dispatch")

DEFAULT_GATE_ROOT = Path.home() / ".opendaisugi" / "gate"
LAST_DISPATCH_NAME = "last_dispatch.json"


class ClientFailure(RuntimeError):
    """The dispatched client did not produce a usable verdict."""


def _case_for(plan: ActionPlan, envelope: Envelope, oracle: VerificationResult) -> dict[str, Any]:
    """One conformance verify case, carrying the oracle's own expectation.

    The oracle has already run, so the case is the same body the conformance
    corpus records, built by the same function. The client ignores `expect`;
    a client that echoes it is agreeing with the oracle, which is the shape
    the bench scores.
    """
    from opendaisugi.conformance import make_verify_case

    return make_verify_case(plan, envelope, {"strict": None, "z3_timeout_ms": 500}, oracle)


def _well_formed(verdict: object) -> bool:
    """True when a verdict has the shape the wire protocol promises."""
    if not isinstance(verdict, dict) or "id" not in verdict:
        return False
    if "ok" in verdict and not isinstance(verdict["ok"], bool):
        return False
    violations = verdict.get("violations", [])
    return isinstance(violations, list) and all(
        isinstance(v, dict) and isinstance(v.get("stage"), str) for v in violations
    )


def _run_client(argv: list[str], case: dict, timeout_s: float) -> dict:
    """Feed one case to the client and return its verdict for that case.

    Raises ``ClientFailure`` for everything that is not a well-formed verdict
    carrying an ``ok`` field: a binary that cannot start, a non-zero exit, a
    timeout, a line that is not JSON, a verdict of the wrong shape, an error
    verdict, or no verdict at all.
    """
    from opendaisugi.conformance import canonical_json

    try:
        proc = subprocess.run(  # noqa: S603 - the operator's own client binary
            argv,
            input=canonical_json(case) + "\n",
            capture_output=True,
            text=True,
            errors="replace",
            timeout=timeout_s,
            check=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise ClientFailure(f"timed out after {timeout_s:g} s") from exc
    except OSError as exc:
        raise ClientFailure(f"could not start: {exc}") from exc
    if proc.returncode != 0:
        raise ClientFailure(f"exited {proc.returncode}: {proc.stderr.strip()[-200:]}")
    matches: list[dict] = []
    for line in proc.stdout.splitlines():
        if not line.strip():
            continue
        try:
            verdict = json.loads(line)
        except ValueError as exc:
            raise ClientFailure(f"wrote a non-JSON verdict: {line[:120]!r}") from exc
        if not _well_formed(verdict):
            raise ClientFailure(f"wrote a malformed verdict: {line[:120]!r}")
        if verdict.get("id") == case["id"]:
            matches.append(verdict)
    if not matches:
        raise ClientFailure("produced no verdict for the case")
    if len(matches) > 1:
        # A provisional allow followed by a final deny must not be read as
        # the allow. One case gets one verdict; anything else is refused whole.
        raise ClientFailure(f"wrote {len(matches)} verdicts for one case")
    verdict = matches[0]
    if "ok" not in verdict:
        raise ClientFailure(f"error verdict: {verdict.get('error', 'no ok field')}")
    if verdict["ok"] is True and verdict.get("violations"):
        # The client's final ok disagrees with its own list. Its finding must
        # not be lost behind the allow, so the verdict is not used at all.
        raise ClientFailure(
            f"inconsistent verdict: ok with {len(verdict['violations'])} violations"
        )
    return verdict


def _step_detail(plan: ActionPlan, step_id: object) -> str:
    """The command or path of the step a client cited, for the human reason.

    A dispatched verdict carries only the stage and the step id. The operator
    reading an ask prompt needs to know WHAT was refused, so the text comes
    back from the plan.
    """
    for step in plan.steps:
        if step.id == step_id:
            for attr in ("command", "path", "url", "skill_id", "tool"):
                value = getattr(step, attr, None)
                if value:
                    return f"{attr}={value}"
            return f"step type {getattr(step, 'type', 'unknown')}"
    return "no matching step in the plan"


def _client_violations(verdict: dict, plan: ActionPlan, client: str) -> list[Violation]:
    """The client's own violations, rebuilt with a reason a human can act on.

    Only `ok` and the stage and step pairs are normative, so the message is
    reconstructed. It names the client AND the step's command or path, because
    `evaluate_record` builds the deny summary and the ask prompt from these
    strings, and "the rust client rejected this step" tells an operator nothing.
    """
    return [
        Violation(
            stage=str(v.get("stage", "unknown")),
            message=(
                f"the {client} verifier client refused this step, "
                f"{_step_detail(plan, v.get('step'))}"
            ),
            detail={"step": v.get("step"), "client": client},
        )
        for v in verdict.get("violations", [])
    ]


def _record_dispatch(root: Path, client: str, ok: bool, error: str) -> None:
    """Remember the last dispatch so `daisugi modules` can be honest.

    Best effort: a read-only disk must never change a verdict.
    """
    try:
        target = root / "verifier"
        target.mkdir(parents=True, exist_ok=True, mode=0o700)
        (target / LAST_DISPATCH_NAME).write_text(
            json.dumps({"client": client, "ok": ok, "error": error, "at": time.time()}),
            encoding="utf-8",
        )
    except OSError:
        pass


def last_dispatch(root: Path | None = None) -> dict:
    """The last recorded dispatch, or {} when there is none."""
    base = root or DEFAULT_GATE_ROOT
    try:
        record = json.loads((base / "verifier" / LAST_DISPATCH_NAME).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return record if isinstance(record, dict) else {}


def verify_via(
    client: str,
    plan: ActionPlan,
    envelope: Envelope,
    *,
    timeout_s: float = 5.0,
    root: Path | None = None,
    locate: "Callable[[Any], list[str]] | None" = None,
) -> VerificationResult:
    """Verify with the oracle AND a compiled client. The client may only tighten.

    The returned ``ok`` is ``oracle.ok and client_ok``. A client that runs
    cleanly can turn an allow into a deny. It can never turn a deny into an
    allow, because a Core-profile client implements none of the predicate or
    Z3 stages and its allow means "I did not look", not "this is safe".

    ``timeout_s`` bounds the whole call. The oracle runs first and the client
    gets whatever time is left, so a slow oracle plus a hung client cannot
    outlive the budget the caller planned for. A client with no time left is
    a failed client.

    Any client failure leaves the oracle's verdict standing alone with
    ``fallback="python"``, ``client_verdict=None``, and a WARNING naming what
    broke.
    """
    from opendaisugi.bench import options
    from opendaisugi.verify import verify

    base = root or DEFAULT_GATE_ROOT
    t0 = time.monotonic()
    oracle = verify(plan, envelope, strict=None)
    oracle_s = time.monotonic() - t0
    left = timeout_s - oracle_s

    spec = options.VERIFIER_CLIENTS.get(client)
    failure = ""
    verdict: dict | None = None
    # ``locate`` is how a client is found: the bench's checkout rule by
    # default, options.gate_client_argv on the gate.
    argv = (locate(spec) if locate else options.client_argv(spec)) if spec is not None else []
    built = bool(argv) if locate else spec is not None and options.client_is_built(spec)
    if spec is None:
        failure = f"no client named {client!r}"
    elif not built:
        failure = f"not built; {options.build_hint(spec)}"
    elif left <= 0:
        failure = f"no time left after the oracle, which took {oracle_s:.2f} s of {timeout_s:g} s"
    else:
        argv = argv or list(spec.argv)
        try:
            verdict = _run_client(argv, _case_for(plan, envelope, oracle), left)
        except ClientFailure as exc:
            failure = str(exc)

    if verdict is None:
        _log.warning(
            "verifier client %r failed (%s); the python oracle decided alone. "
            "Run `daisugi bench verifier` to see which clients are built.",
            client,
            failure,
        )
        _record_dispatch(base, client, False, failure)
        return oracle.model_copy(
            update={"client": client, "fallback": "python", "client_verdict": None}
        )

    _record_dispatch(base, client, True, "")
    client_ok = bool(verdict["ok"])
    if client_ok != oracle.ok:
        _log.warning(
            "verifier client %r disagreed with the python oracle: client ok=%s, "
            "oracle ok=%s. The stricter verdict stands. This is a finding: "
            "run `daisugi bench verifier` on the corpus.",
            client,
            client_ok,
            oracle.ok,
        )
    return oracle.model_copy(
        update={
            "ok": oracle.ok and client_ok,
            # The oracle's violations carry the clause and counterexample an
            # operator reads. The client's follow, so a tightening is
            # attributable rather than anonymous.
            "violations": list(oracle.violations) + _client_violations(verdict, plan, client),
            "client": client,
            "fallback": None,
            "client_verdict": client_ok,
        }
    )

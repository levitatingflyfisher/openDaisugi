"""Language-neutral conformance corpus + differential harness (multi-client).

The verifier is being reimplemented in other languages (Rust, Go, Lean4,
TypeScript). Independent clients are only worth having if disagreement is
detectable, so this module gives every client the same three things:

- a **corpus**: self-contained JSONL cases, each carrying everything needed to
  reproduce one verification (``kind: verify`` — plan + envelope + options +
  expected verdict) or one shell decomposition (``kind: decompose`` — command
  + expected heads/effects), harvested from real ``verify()`` calls;
- a **wire protocol**: a client reads one case JSON per line on stdin and
  writes one verdict JSON per line on stdout — no linking, any language;
- a **differential runner**: feed the corpus to a client, compare verdicts,
  report disagreements. Every mismatch is a bug in one implementation or an
  ambiguity in the spec — both are findings.

Comparison is structural, never textual: ``ok`` plus the multiset of
(stage, step) pairs for verify cases; ``ok``, ordered heads, and sorted
read/write effects for decompose cases. Two correct clients may word a
rejection differently; they may not disagree about what was rejected.

Recording is a product feature: set ``OPENDAISUGI_CONFORMANCE_RECORD=<dir>``
in any process (a test run, live usage) and its verifications are appended as
raw case lines, deduplicated later by ``export_corpus``. Recording never
raises — a broken disk must not break verification. Spec: docs/spec/conformance.md.
"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any, Iterable, Iterator

if TYPE_CHECKING:  # pragma: no cover
    from opendaisugi.models import ActionPlan, Envelope, VerificationResult
    from opendaisugi.shell_decompose import Decomposition

RECORD_ENV = "OPENDAISUGI_CONFORMANCE_RECORD"
CONFORMANCE_VERSION = 1

# True while serve_lines is re-running cases: the oracle answering the corpus
# must not re-record it (harmless duplication, but pointless growth).
_serving = False


# --- canonical form --------------------------------------------------------------


def canonical_json(obj: Any) -> str:
    """Deterministic JSON: sorted keys, no whitespace, raw unicode."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def case_id(body: dict) -> str:
    """Content address of a case body (id field excluded by convention)."""
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()[:16]


# --- case construction -----------------------------------------------------------


def _normative_violations(result: "VerificationResult") -> list[dict]:
    # (stage, step) is the normative pair; messages/remediation are informative.
    return [{"stage": v.stage, "step": v.detail.get("step")} for v in result.violations]


def make_verify_case(
    plan: "ActionPlan",
    envelope: "Envelope",
    options: dict,
    result: "VerificationResult",
) -> dict:
    plan_body = plan.model_dump(mode="json")
    envelope_body = envelope.model_dump(mode="json")
    # Top-level plan/envelope ids are random-uuid bookkeeping, not semantics
    # (step ids, which violations cite, are preserved). Pin them so identical
    # logical cases content-address identically and dedupe is real.
    plan_body["id"] = "plan_case"
    envelope_body["id"] = "env_case"
    body = {
        "kind": "verify",
        "v": CONFORMANCE_VERSION,
        "plan": plan_body,
        "envelope": envelope_body,
        "options": options,
        "expect": {"ok": result.ok, "violations": _normative_violations(result)},
    }
    body["id"] = case_id(body)
    return body


def make_decompose_case(command: str, decomp: "Decomposition") -> dict:
    expect: dict[str, Any] = {"ok": decomp.ok}
    if decomp.ok:
        expect.update(
            heads=list(decomp.heads),
            commands=list(decomp.commands),
            reads=sorted(decomp.reads),
            writes=sorted(decomp.writes),
        )
    body = {"kind": "decompose", "v": CONFORMANCE_VERSION, "command": command, "expect": expect}
    body["id"] = case_id(body)
    return body


# --- live recording --------------------------------------------------------------


def _record(body: dict) -> None:
    try:
        raw = os.environ.get(RECORD_ENV)
        if not raw or _serving:
            return
        rec_dir = Path(raw)
        rec_dir.mkdir(parents=True, exist_ok=True)
        with (rec_dir / f"cases-{os.getpid()}.jsonl").open("a", encoding="utf-8") as f:
            f.write(canonical_json(body) + "\n")
    except Exception:  # noqa: BLE001 — recording must never break verification
        pass


def _plan_is_portable(plan: "ActionPlan") -> bool:
    """True when a fresh process can validate the plan from its JSON alone.

    Custom step types registered at runtime (``@opendaisugi.step_type``) are
    process state — a case carrying one would fail model validation in any
    other client or process, so it cannot join the corpus.
    """
    from opendaisugi.models import STEP_TYPE_REGISTRY

    for step in plan.steps:
        cls = STEP_TYPE_REGISTRY.get(step.type)
        if cls is None or cls.__module__ != "opendaisugi.models":
            return False
    return True


def record_verify(
    plan: "ActionPlan",
    envelope: "Envelope",
    options: dict,
    result: "VerificationResult",
) -> None:
    if not os.environ.get(RECORD_ENV) or _serving:
        return
    try:
        if _plan_is_portable(plan):
            _record(make_verify_case(plan, envelope, options, result))
    except Exception:  # noqa: BLE001
        pass


def record_decompose(command: str, decomp: "Decomposition") -> None:
    if not os.environ.get(RECORD_ENV) or _serving:
        return
    try:
        _record(make_decompose_case(command, decomp))
    except Exception:  # noqa: BLE001
        pass


# --- export ----------------------------------------------------------------------


def export_corpus(raw_dir: Path, out: Path) -> dict:
    """Dedupe recorded case lines into a sorted corpus + pinning manifest."""
    seen: dict[str, str] = {}
    for f in sorted(Path(raw_dir).glob("*.jsonl")):
        for line in f.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            case = json.loads(line)
            seen[case["id"]] = canonical_json(case)
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    payload = "".join(seen[cid] + "\n" for cid in sorted(seen))
    out.write_text(payload, encoding="utf-8")
    manifest = {
        "v": CONFORMANCE_VERSION,
        "count": len(seen),
        "sha256": hashlib.sha256(payload.encode("utf-8")).hexdigest(),
    }
    out.with_suffix(".manifest.json").write_text(canonical_json(manifest) + "\n", encoding="utf-8")
    return manifest


# --- verdict comparison ----------------------------------------------------------


@dataclass
class Mismatch:
    case_id: str
    kind: str
    expected: dict
    got: dict


def _normal_verify(v: dict) -> tuple:
    pairs = sorted((x["stage"], x.get("step") or "") for x in v.get("violations", []))
    return (bool(v["ok"]), tuple(pairs))


def _normal_decompose(v: dict) -> tuple:
    if not v.get("ok"):
        return (False,)
    return (
        True,
        tuple(v.get("heads", [])),
        tuple(sorted(v.get("reads", []))),
        tuple(sorted(v.get("writes", []))),
    )


def compare_verdict(case: dict, verdict: dict) -> Mismatch | None:
    """Structural comparison of a client verdict against a case expectation."""
    if "ok" not in verdict:  # error verdicts and garbage are mismatches, not crashes
        return Mismatch(case["id"], case["kind"], expected=case["expect"], got=verdict)
    normal = _normal_verify if case["kind"] == "verify" else _normal_decompose
    if normal(case["expect"]) == normal(verdict):
        return None
    return Mismatch(case["id"], case["kind"], expected=case["expect"], got=verdict)


# --- the oracle client -----------------------------------------------------------


def serve_lines(lines: Iterable[str]) -> Iterator[str]:
    """Answer corpus cases with the Python oracle, speaking the wire protocol.

    One malformed or unreconstructable case yields an error verdict (which the
    runner counts as a mismatch) — it must never abort the stream, because the
    differential runner still needs every other verdict.
    """
    global _serving
    _serving = True
    try:
        for line in lines:
            if not line.strip():
                continue
            try:
                yield _serve_one(json.loads(line))
            except Exception as e:  # noqa: BLE001 — contain per-case failures
                try:
                    cid = json.loads(line).get("id")
                except Exception:  # noqa: BLE001
                    cid = None
                yield canonical_json({"id": cid, "error": repr(e)[:300]})
    finally:
        _serving = False


def _serve_one(case: dict) -> str:
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.verify import verify

    if case["kind"] == "verify":
        opts = case.get("options", {})
        result = verify(
            ActionPlan.model_validate(case["plan"]),
            Envelope.model_validate(case["envelope"]),
            z3_timeout_ms=opts.get("z3_timeout_ms", 500),
            strict=opts.get("strict"),
        )
        return canonical_json(
            {"id": case["id"], "ok": result.ok, "violations": _normative_violations(result)}
        )
    if case["kind"] == "decompose":
        from opendaisugi.shell_decompose import decompose_command

        d = decompose_command(case["command"])
        verdict: dict[str, Any] = {"id": case["id"], "ok": d.ok}
        if d.ok:
            verdict.update(
                heads=list(d.heads),
                commands=list(d.commands),
                reads=sorted(d.reads),
                writes=sorted(d.writes),
            )
        return canonical_json(verdict)
    return canonical_json({"id": case.get("id"), "error": f"unknown kind {case['kind']!r}"})


# --- the differential runner -----------------------------------------------------


@dataclass
class RunReport:
    total: int
    matched: int
    mismatches: list[Mismatch] = field(default_factory=list)


class ClientError(RuntimeError):
    """The client process died, timed out, or spoke garbage."""


def run_corpus(corpus: Path, client_cmd: list[str], *, timeout_s: float = 600.0) -> RunReport:
    """Feed every corpus case to a client binary and compare its verdicts."""
    cases = [
        json.loads(x) for x in Path(corpus).read_text(encoding="utf-8").splitlines() if x.strip()
    ]
    proc = subprocess.Popen(  # noqa: S603 — the client command is the caller's own binary
        client_cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        out, err = proc.communicate(
            input="".join(canonical_json(c) + "\n" for c in cases), timeout=timeout_s
        )
    except subprocess.TimeoutExpired as e:
        proc.kill()
        raise ClientError(f"client timed out after {timeout_s}s") from e
    if proc.returncode != 0:
        raise ClientError(f"client exited {proc.returncode}: {err.strip()[-500:]}")
    verdicts: dict[str, dict] = {}
    for line in out.splitlines():
        if not line.strip():
            continue
        try:
            v = json.loads(line)
        except json.JSONDecodeError as e:
            raise ClientError(f"client emitted non-JSON verdict: {line[:200]!r}") from e
        verdicts[v.get("id", "")] = v
    report = RunReport(total=len(cases), matched=0)
    for case in cases:
        got = verdicts.get(case["id"])
        if got is None:
            report.mismatches.append(
                Mismatch(case["id"], case["kind"], case["expect"], {"error": "no verdict"})
            )
            continue
        m = compare_verdict(case, got)
        if m is None:
            report.matched += 1
        else:
            report.mismatches.append(m)
    return report


# --- benchmarks ------------------------------------------------------------------


@dataclass
class BenchStats:
    n_cases: int
    repeat: int
    p50_ms: float
    p95_ms: float
    p99_ms: float
    cases_per_s: float


def _percentile(sorted_ms: list[float], q: float) -> float:
    idx = min(len(sorted_ms) - 1, max(0, int(q * len(sorted_ms) + 0.999999) - 1))
    return sorted_ms[idx]


def bench_corpus(
    corpus: Path, *, repeat: int = 1, client_cmd: list[str] | None = None
) -> BenchStats:
    """Per-case latency over the corpus: in-process oracle, or a client pipe.

    Client mode times full round trips (IPC included) via one-line-in,
    one-line-out exchanges, so compare clients to each other, not to the
    in-process oracle numbers.
    """
    lines = [x for x in Path(corpus).read_text(encoding="utf-8").splitlines() if x.strip()]
    durations_ms: list[float] = []
    if client_cmd is None:
        list(serve_lines(lines))  # warmup: parser + Z3 + import costs paid once
        for _ in range(repeat):
            for line in lines:
                t0 = time.perf_counter()
                next(iter(serve_lines([line])))
                durations_ms.append((time.perf_counter() - t0) * 1000)
    else:
        proc = subprocess.Popen(  # noqa: S603
            client_cmd,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
        )
        assert proc.stdin is not None and proc.stdout is not None
        try:
            proc.stdin.write(lines[0] + "\n")  # warmup round trip
            proc.stdin.flush()
            proc.stdout.readline()
            for _ in range(repeat):
                for line in lines:
                    t0 = time.perf_counter()
                    proc.stdin.write(line + "\n")
                    proc.stdin.flush()
                    if not proc.stdout.readline():
                        raise ClientError("client closed its stdout mid-benchmark")
                    durations_ms.append((time.perf_counter() - t0) * 1000)
        finally:
            proc.stdin.close()
            proc.wait(timeout=30)
    total_s = sum(durations_ms) / 1000
    ordered = sorted(durations_ms)
    return BenchStats(
        n_cases=len(lines),
        repeat=repeat,
        p50_ms=_percentile(ordered, 0.50),
        p95_ms=_percentile(ordered, 0.95),
        p99_ms=_percentile(ordered, 0.99),
        cases_per_s=(len(durations_ms) / total_s) if total_s > 0 else 0.0,
    )


def main() -> None:  # pragma: no cover — exercised via subprocess in tests
    import sys

    for verdict in serve_lines(sys.stdin):
        print(verdict, flush=True)


if __name__ == "__main__":  # pragma: no cover
    main()

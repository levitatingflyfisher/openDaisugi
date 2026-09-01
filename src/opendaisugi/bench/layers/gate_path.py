"""`daisugi bench gate-path`: the hook contracts as a table, with the class.

Six ways a tool call reaches the gate, four contract cases each: an
in-envelope call allows, an out-of-envelope call denies, a malformed payload
denies, and the path's alternate input key still parses. Every case runs in
process through `gate.evaluate_call` in enforce mode, because that function
is what every path funnels into.

The fail-open class column is the pinned fact from bench.options, not a
measurement, and the evidence column says whether it was measured or
designed. Codex reads `soft` and will until Codex changes: their hooks fail
open, so the gate guarantee breaks there.
"""

from __future__ import annotations

import subprocess
import sys
import tempfile
import time

from opendaisugi.bench.corpus import (
    CorpusRef,
    corpus_ref,
    load_json,
    load_jsonl,
    resolve_corpus,
)
from opendaisugi.bench.options import GATE_PATHS
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope

CORPUS = "gate-paths.jsonl"
ENVELOPE = "battery-envelope.json"

# One cold gate process. A cold start costs well under a second, so a start
# that runs past this is hung, and the cell names that instead of waiting.
ROUNDTRIP_TIMEOUT_S = 20.0

# What the gate exits with when it denies on the Claude hook path. The empty
# payload the round trip sends must deny, so any other code is not a gate run.
DENY_EXIT_CODE = 2

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("contract", "contract tests"),
    Column("fail_open", "fail-open class"),
    Column("evidence", "evidence"),
    Column("roundtrip_ms", "round-trip ms", align="right", volatile=True),
    Column("source", "source"),
)


def battery_envelope_ref() -> CorpusRef:
    """The committed envelope, pinned, so a table can print its digest."""
    return corpus_ref(resolve_corpus(ENVELOPE, None).path)


def battery_envelope() -> Envelope:
    """The committed envelope every contract case is checked against."""
    return Envelope.model_validate(load_json(battery_envelope_ref()))


def run_contract(path: str, cases: list[dict], envelope: Envelope) -> tuple[int, int]:
    """(passed, total) for one path's contract cases, in enforce mode.

    A case passes when the gate's verdict matches the case's expectation. The
    gate is the judge and the corpus is the claim, so a mislabelled case
    counts against the path rather than for it. Each payload is judged under
    the path's own gate format, so pi's lowercase names classify the way the
    extension delivers them.
    """
    spec = GATE_PATHS.get(path)
    fmt = spec.fmt if spec is not None else "claude"
    mine = [c for c in cases if c["path"] == path]
    passed = 0
    for case in mine:
        decision = evaluate_call(case["payload"], envelope, mode="enforce", fmt=fmt)
        if decision.allow is (case["expect"] == "allow"):
            passed += 1
    return passed, len(mine)


def measure_roundtrip_ms() -> float | str:
    """One cold `python -m opendaisugi.gate` round trip on an empty payload.

    Every subprocess gate path pays the same interpreter and import cost, so
    this is measured once and shared. The gate root is an empty directory that
    exists for the call only, so nothing the operator owns is read or written.

    The empty payload must deny, and on the Claude path a deny is exit 2. Any
    other exit means the gate did not run as a gate: an import error exits 1
    in a few milliseconds and would otherwise read as a fast round trip. A
    start that cannot run, exits with another code, or runs past the timeout
    returns a string naming that failure instead of a number.
    """
    t0 = time.monotonic()
    with tempfile.TemporaryDirectory(prefix="daisugi-bench-gate-") as root:
        try:
            proc = subprocess.run(
                [sys.executable, "-m", "opendaisugi.gate", "--mode", "enforce", "--root", root],
                input=b"{}",
                capture_output=True,
                timeout=ROUNDTRIP_TIMEOUT_S,
                check=False,
            )
        except subprocess.TimeoutExpired:
            return f"timed out after {ROUNDTRIP_TIMEOUT_S:g} s"
        except OSError as exc:
            return f"cannot execute: {exc.strerror or exc}"
    elapsed_ms = (time.monotonic() - t0) * 1000
    if proc.returncode != DENY_EXIT_CODE:
        return f"exit {proc.returncode}: {_last_line(proc.stderr)}"
    return elapsed_ms


def _last_line(stderr: bytes | str) -> str:
    """The last non-blank stderr line, which for a traceback is the error itself."""
    text = stderr.decode("utf-8", "replace") if isinstance(stderr, bytes) else str(stderr)
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    return lines[-1] if lines else "no stderr"


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    envelope_ref = battery_envelope_ref()
    envelope = Envelope.model_validate(load_json(envelope_ref))
    roundtrip = measure_roundtrip_ms()
    rows: list[Row] = []
    for name, spec in GATE_PATHS.items():
        passed, total = run_contract(name, cases, envelope)
        rows.append(
            Row(
                name=name,
                cells={
                    "option": spec.title,
                    "contract": f"{passed}/{total}",
                    "fail_open": spec.fail_open_class,
                    "evidence": spec.evidence,
                    "roundtrip_ms": roundtrip,
                    "source": spec.source,
                },
            )
        )
    return Table(
        layer="gate-path",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        companions=(envelope_ref,),
        reproduce=f"uv run --no-sync daisugi bench gate-path --corpus {ref.rel}",
        notes=(
            "contract cases run in process through gate.evaluate_call in enforce mode, "
            "each under its path's own --format",
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            "round-trip is one cold subprocess start, shared by every path that spawns one",
            "codex hooks fail open, so its gate guarantee is soft; the class is not a rating",
        ),
    )


register(BenchSpec(name="gate-path", layer="gate-path", corpus=CORPUS, columns=COLUMNS, run=run))

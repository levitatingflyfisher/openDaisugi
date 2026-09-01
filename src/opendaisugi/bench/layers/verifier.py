"""`daisugi bench verifier`: the five clients over the committed corpus.

This is the conformance harness in clients/gate.py and clients/compare.py
reduced to one table. The one column that matters for safety is `fail-open`:
cases where the oracle rejects and the client accepts. It must be zero.
Everything else is information.

Python is the oracle, so its row is a self-check. It proves the corpus and the
comparison are wired correctly before any client is read.
"""

from __future__ import annotations

import json
import subprocess
import time

from opendaisugi.bench.corpus import load_jsonl, resolve_corpus
from opendaisugi.bench.options import (
    VERIFIER_CLIENTS,
    build_hint,
    client_argv,
    client_is_built,
)
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.conformance import _normal_decompose, _normal_verify, canonical_json

CORPUS = "verifier.jsonl"

# One client gets this long for the whole corpus. The compiled clients answer
# 47 cases in well under a second, and the bench as a whole has a 30 s budget,
# so a client that needs more than this is hung, not slow.
CLIENT_TIMEOUT_S = 15.0

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("cases", "cases", align="right"),
    Column("agree", "agree", align="right"),
    Column("disagree", "disagree", align="right"),
    Column("fail_open", "fail-open", align="right"),
    Column("ms", "ms/case", align="right", volatile=True),
    Column("failure", "failure"),
)


def _well_formed(v: object) -> bool:
    """True when a verdict has the shape the wire protocol promises.

    A verdict with the wrong shape is dropped before scoring, so a client that
    writes garbage cannot crash the normaliser or score a single case.
    """
    if not isinstance(v, dict) or "id" not in v:
        return False
    if "ok" in v and not isinstance(v["ok"], bool):
        return False
    violations = v.get("violations", [])
    if not isinstance(violations, list) or not all(
        isinstance(x, dict) and isinstance(x.get("stage"), str) for x in violations
    ):
        return False
    for key in ("heads", "reads", "writes", "commands"):
        items = v.get(key, [])
        if not isinstance(items, list) or not all(isinstance(x, str) for x in items):
            return False
    return True


def client_verdicts(
    argv: list[str], cases: list[dict], *, timeout_s: float = CLIENT_TIMEOUT_S
) -> tuple[dict[str, dict], float, str | None]:
    """Feed every case on one stdin stream.

    Returns the well-formed verdicts by id, the elapsed ms, and a failure
    string or None. A client that cannot start, exits non-zero, runs past the
    timeout, or writes lines that are not JSON or not the protocol's shape is
    named in that string. Its unanswered cases count as disagreements, never
    as passes. A silent client must not score well, and a broken one must not
    read as a client that merely disagrees.
    """
    stream = "".join(canonical_json(c) + "\n" for c in cases)
    t0 = time.monotonic()
    failure: str | None = None
    stdout = ""
    try:
        proc = subprocess.run(
            argv,
            input=stream,
            capture_output=True,
            text=True,
            errors="replace",
            timeout=timeout_s,
            check=False,
        )
        stdout = proc.stdout
        if proc.returncode != 0:
            failure = f"exit {proc.returncode}"
    except subprocess.TimeoutExpired:
        failure = f"timed out after {timeout_s:g} s"
    except OSError as exc:
        failure = f"cannot execute: {exc.strerror or exc}"
    elapsed_ms = (time.monotonic() - t0) * 1000
    out: dict[str, dict] = {}
    not_json = malformed = 0
    for line in stdout.splitlines():
        if not line.strip():
            continue
        try:
            v = json.loads(line)
        except ValueError:
            not_json += 1
            continue
        if not _well_formed(v):
            malformed += 1
            continue
        out[str(v["id"])] = v
    problems = [failure] if failure else []
    if not_json:
        problems.append(f"{not_json} lines not JSON")
    if malformed:
        problems.append(f"{malformed} malformed verdicts")
    return out, elapsed_ms, "; ".join(problems) or None


def _is_fail_open(case: dict, got: dict | None) -> bool:
    """True when the oracle's expectation rejects and the client accepts."""
    if got is None or "ok" not in got:
        return False
    return bool(got["ok"]) and not bool(case["expect"]["ok"])


def fail_open_ids(cases: list[dict], verdicts: dict[str, dict]) -> list[str]:
    """The ids of every case the client accepted and the oracle rejects."""
    return [c["id"] for c in cases if _is_fail_open(c, verdicts.get(c["id"]))]


def score(cases: list[dict], verdicts: dict[str, dict]) -> dict[str, int]:
    """Agreement with the oracle's own expectation, plus the fail-open count.

    fail-open means the oracle's expectation rejects and the client accepts.
    That is the direction that lets a command through unchecked, so it gets its
    own number rather than hiding inside `disagree`.
    """
    agree = disagree = fail_open = 0
    for case in cases:
        normal = _normal_verify if case["kind"] == "verify" else _normal_decompose
        expected = normal(case["expect"])
        got = verdicts.get(case["id"])
        if got is None or "ok" not in got:
            disagree += 1
            continue
        try:
            same = normal(got) == expected
        except (TypeError, KeyError, AttributeError, ValueError):
            same = False
        if same:
            agree += 1
            continue
        disagree += 1
        if _is_fail_open(case, got):
            fail_open += 1
    return {"agree": agree, "disagree": disagree, "fail_open": fail_open}


UNKNOWN = "n/a"


def counts_or_unknown(
    cases: list[dict], verdicts: dict[str, dict], failure: str | None
) -> dict[str, object]:
    """The score, or `n/a` in every scored cell when the client failed.

    A client that hung, crashed, or wrote garbage measured nothing. Printing
    `agree 0` and `fail-open 0` beside its failure would read as a client
    that answered every case and never let one through. Unknown is not zero.
    """
    if failure:
        return {"agree": UNKNOWN, "disagree": UNKNOWN, "fail_open": UNKNOWN}
    return dict(score(cases, verdicts))


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    rows: list[Row] = []
    for name, spec in VERIFIER_CLIENTS.items():
        if not client_is_built(spec):
            rows.append(Row(name=name, absent=build_hint(spec)))
            continue
        verdicts, elapsed_ms, failure = client_verdicts(
            client_argv(spec), cases, timeout_s=CLIENT_TIMEOUT_S
        )
        rows.append(
            Row(
                name=name,
                cells={
                    "option": name,
                    "cases": len(cases),
                    **counts_or_unknown(cases, verdicts, failure),
                    "ms": elapsed_ms / max(1, len(cases)),
                    "failure": failure or "",
                },
            )
        )
    return Table(
        layer="verifier",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        reproduce=f"uv run --no-sync daisugi bench verifier --corpus {ref.rel}",
        notes=(
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            f"a client gets {CLIENT_TIMEOUT_S:g} s for the whole corpus; a hung, crashed or "
            "garbage-writing client is named in the failure column and its scored cells "
            "read n/a, because unknown is not zero",
            "python is the oracle; agree means agrees with the reference, not provably correct",
            "fail-open counts cases the oracle rejects and the client accepts; it must be 0 "
            "for every Full-profile client",
            "lean implements the Core profile only, so it accepts the corpus's predicate-stage "
            "denial; that row shows the profile gap instead of hiding it",
            "a built client may predate the current clients/ source; rebuild before you trust a row",
            "go, rust and ts reach Full profile by spawning a z3 binary; with z3 off PATH they "
            "deny every predicate envelope, which is fail-closed and shows here as disagreement",
        ),
    )


register(BenchSpec(name="verifier", layer="verifier", corpus=CORPUS, columns=COLUMNS, run=run))

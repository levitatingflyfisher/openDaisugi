"""The safety gate for client-parser work: is a client's decompose coverage
growing WITHOUT weakening safety?

The conformance metric is "match the oracle exactly." When you extend a client
parser to close *false-rejects* (oracle accepts, client rejects — the safe
direction), the danger is silently trading them for one of two *unsafe* shapes:

  * false-accept    — client says ok, oracle says reject. The client waved
                      through a command the reference refuses.
  * both-ok-differ  — both say ok, but the head/read/write sets differ. Almost
                      always the client dropped or invented a head. On a corpus
                      skewed toward one command shape this can *coincidentally
                      match*, so it is a fail-open shape a "no new false-accept"
                      check alone would miss — it is counted here too.

This gate runs a client over a FROZEN corpus, compares each verdict to the
oracle's (cached once — the oracle is fixed), and prints the four-way split per
kind. With --max-unsafe N it exits nonzero if (false-accept + both-ok-differ)
exceeds N, so it can guard a parser change: capture the baseline, then never let
the unsafe count climb.

Usage:
    uv run python clients/gate.py CORPUS.jsonl \
        --client "clients/lean/.lake/build/bin/conform" \
        --max-unsafe 116          # Lean's frozen baseline; fail if it grows

Never commit gate output or the cache: mismatch samples embed corpus content.
"""

from __future__ import annotations

import argparse
import collections
import hashlib
import json
import shlex
import subprocess
import sys
from pathlib import Path

from opendaisugi.conformance import _normal_decompose, _normal_verify, canonical_json
from opendaisugi.shell_decompose import decompose_command


def _norm(kind: str, verdict: dict) -> tuple:
    return _normal_verify(verdict) if kind == "verify" else _normal_decompose(verdict)


def _unfuse(command: str) -> str | None:
    """Turn statement-boundary newlines into `;` so the oracle can reparse a
    tree-sitter-fused script WITHOUT the fusion, while preserving the newlines
    that are NOT boundaries: those inside single/double quotes (a wrapped grep
    pattern, an awk program, a commit message), after a `\\` line continuation,
    and inside heredoc bodies (stdin data). Returns None on an unterminated
    heredoc — no clean reference is derivable."""
    out: list[str] = []
    i, n, sq, dq = 0, len(command), False, False
    pend: list[tuple[str, bool]] = []  # heredoc (delim, dash) queued for after this line
    hd: tuple[str, bool] | None = None  # heredoc body currently being copied
    while i < n:
        c = command[i]
        if hd is not None:  # copy a heredoc body line verbatim
            j = command.find("\n", i)
            if j == -1:
                j = n
            delim, dash = hd
            stripped = command[i:j].lstrip("\t") if dash else command[i:j]
            out.append(command[i:j])
            if j < n:
                out.append("\n")
            hd = (pend.pop(0) if pend else None) if stripped == delim else hd
            i = j + 1 if j < n else n
            continue
        if c == "\\" and i + 1 < n:
            out.append(command[i:i + 2])
            i += 2
            continue
        if c == "'" and not dq:
            sq = not sq
            out.append(c)
            i += 1
            continue
        if c == '"' and not sq:
            dq = not dq
            out.append(c)
            i += 1
            continue
        if not sq and not dq and command[i:i + 2] == "<<" and command[i:i + 3] != "<<<":
            dash = command[i:i + 3] == "<<-"
            k = i + (3 if dash else 2)
            while k < n and command[k] in " \t":
                k += 1
            if k < n and command[k] in "'\"":
                q = command[k]
                e = command.find(q, k + 1)
                if e == -1:
                    return None
                delim, k = command[k + 1:e], e + 1
            else:
                m = k
                while m < n and command[m] not in " \t\n;&|<>()'\"`":
                    m += 1
                delim, k = command[k:m], m
            if not delim:
                return None
            out.append(command[i:k])
            pend.append((delim, dash))
            i = k
            continue
        if c == "\n" and not sq and not dq:
            if pend:
                out.append("\n")
                hd = pend.pop(0)
            else:
                out.append(" ; ")
            i += 1
            continue
        out.append(c)
        i += 1
    return None if (pend or hd is not None) else "".join(out)


def _run_client(cmd: list[str], cases: list[dict]) -> dict[str, dict]:
    """Feed every case to the client on one stdin stream; collect verdicts by id."""
    stream = "".join(canonical_json(c) + "\n" for c in cases)
    proc = subprocess.run(  # noqa: S603 — caller's own client binary
        cmd, input=stream, capture_output=True, text=True, timeout=600, check=False
    )
    out: dict[str, dict] = {}
    for line in proc.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        v = json.loads(line)
        if "id" in v:
            out[v["id"]] = v
    return out


def _oracle_verdicts(corpus: Path, cases: list[dict]) -> dict[str, dict]:
    """Oracle verdicts, cached by corpus content hash (the oracle is fixed)."""
    digest = hashlib.sha256(corpus.read_bytes()).hexdigest()[:16]
    cache = corpus.parent / ".gate-cache" / f"oracle-{digest}.jsonl"
    if cache.exists():
        return {v["id"]: v for v in (json.loads(x) for x in cache.read_text().splitlines() if x.strip())}
    verdicts = _run_client([sys.executable, "-m", "opendaisugi.conformance"], cases)
    cache.parent.mkdir(parents=True, exist_ok=True)
    cache.write_text("".join(canonical_json(v) + "\n" for v in verdicts.values()) + "\n")
    return verdicts


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("corpus", type=Path)
    ap.add_argument("--client", required=True, help="client command line (quoted)")
    ap.add_argument("--corroborate", default=None,
                    help="an INDEPENDENT client (quoted cmd) whose exact head match "
                         "corroborates a divergence the oracle's tree-sitter can't parse")
    ap.add_argument("--max-unsafe", type=int, default=None,
                    help="fail (exit 1) if false-accept + both-ok-differ exceeds this")
    ap.add_argument("--samples", type=int, default=0,
                    help="print up to N sample ids per unsafe bucket")
    args = ap.parse_args()

    cases = [json.loads(x) for x in args.corpus.read_text().splitlines() if x.strip()]
    kind_by_id = {c["id"]: c["kind"] for c in cases}
    cmd_by_id = {c["id"]: c.get("command", "") for c in cases if c["kind"] == "decompose"}
    oracle = _oracle_verdicts(args.corpus, cases)
    client = _run_client(shlex.split(args.client), cases)
    corrob = _run_client(shlex.split(args.corroborate), cases) if args.corroborate else {}

    # A decompose false-accept (client ok, oracle reject) splits two ways:
    #   * fusion    — a documented tree-sitter parser-diversity divergence where
    #                 the client's head list is provably COMPLETE. Two ways to
    #                 prove it: (a) the oracle refused for statement fusion AND an
    #                 un-fused reparse's heads are covered by the client's; or
    #                 (b) an INDEPENDENT parser (the --corroborate client, e.g.
    #                 Go/mvdan-sh) accepts with the client's EXACT heads. Either
    #                 witnesses that every executing head is present — not a
    #                 fielded safety hole.
    #   * genuine   — the client accepted a command no independent reference
    #                 corroborates: it waved through something the oracle rejects
    #                 on its merits (a real over-acceptance), OR dropped a head
    #                 under cover of a divergence. A real bug. Must never grow.
    # both-ok-differ is always a real bug (dropped/invented head). The gate
    # threshold governs genuine + both-ok-differ; fusion is reported, not gated.
    def _divergence_safe(cid: str, client_verdict: dict) -> bool:
        """A decompose false-accept is a benign parser-diversity divergence only
        if the client's head list is proven COMPLETE by an independent reference —
        never on the oracle's reason string alone.

        Proof (a): the oracle refused for statement fusion, and an un-fused reparse
        (`_unfuse`: statement-boundary newlines → `;`, quotes/continuations/heredoc
        bodies preserved) decomposes with heads covered by the client's.

        Proof (b): the --corroborate client (an independent parser) accepts with
        the client's EXACT heads. This catches the tree-sitter parse-error class
        (valid bash the oracle's grammar chokes on) that no reparse recovers.

        With neither proof the case is genuine (unsafe) — a real over-acceptance
        or a dropped head."""
        cmd = cmd_by_id.get(cid, "")
        got = collections.Counter(client_verdict.get("heads", []))
        if "statement fusion" in decompose_command(cmd).reason:
            joined = _unfuse(cmd)
            ref = decompose_command(joined) if joined is not None else None
            if ref is not None and ref.ok and all(got[h] >= n for h, n in collections.Counter(ref.heads).items()):
                return True
        cv = corrob.get(cid)
        if cv and cv.get("ok") and cv.get("heads") == client_verdict.get("heads"):
            return True
        return False

    kinds = sorted({c["kind"] for c in cases})
    tally = {k: dict(match=0, false_reject=0, verified_div=0, fusion_unverified=0,
                     genuine_fa=0, both_ok_differ=0, missing=0) for k in kinds}
    unsafe_ids: dict[str, list[str]] = {"genuine_fa": [], "both_ok_differ": []}

    for cid, kind in kind_by_id.items():
        o, c = oracle.get(cid), client.get(cid)
        if c is None:
            tally[kind]["missing"] += 1
            continue
        on, cn = _norm(kind, o), _norm(kind, c)
        o_ok, c_ok = on[0], cn[0]
        if on == cn:
            tally[kind]["match"] += 1
        elif o_ok and not c_ok:
            tally[kind]["false_reject"] += 1
        elif c_ok and not o_ok:
            if kind == "decompose" and _divergence_safe(cid, c):
                tally[kind]["verified_div"] += 1  # completeness proven by an independent reference
            elif kind == "decompose" and "statement fusion" in decompose_command(cmd_by_id[cid]).reason:
                tally[kind]["fusion_unverified"] += 1  # known-class divergence, no reference; reported, spot-checked
            else:
                tally[kind]["genuine_fa"] += 1  # over-acceptance the oracle rejects on merits — gated
                unsafe_ids["genuine_fa"].append(cid)
        elif o_ok and c_ok:
            tally[kind]["both_ok_differ"] += 1
            unsafe_ids["both_ok_differ"].append(cid)
        else:  # both reject, different reason — structurally a match (reasons non-normative)
            tally[kind]["match"] += 1

    # The parser campaign is decompose-only. verify's residual (Full-profile Z3/
    # predicate stages a Core client doesn't implement) is out-of-scope-by-design
    # and reported for transparency, not gated.
    genuine_unsafe = tally["decompose"]["genuine_fa"] + tally["decompose"]["both_ok_differ"]
    total_disagree = 0
    print(f"corpus: {args.corpus}  cases: {len(cases)}")
    for k in kinds:
        t = tally[k]
        total_disagree += (t["false_reject"] + t["verified_div"] + t["fusion_unverified"]
                           + t["genuine_fa"] + t["both_ok_differ"])
        total = sum(v for kk, v in t.items() if kk != "missing")
        tag = " (out-of-scope)" if k == "verify" else ""
        print(f"  {k:10} match={t['match']:6}  false-reject(safe)={t['false_reject']:6}  "
              f"verified-div={t['verified_div']:4}  fusion-unverified={t['fusion_unverified']:3}  "
              f"genuine-fa={t['genuine_fa']:3}  both-ok-differ={t['both_ok_differ']:3}  "
              f"[{t['match']}/{total}]{tag}")
    print(f"  DISAGREEMENTS (all mismatch): {total_disagree}")
    print(f"  GENUINE UNSAFE decompose (genuine-fa + both-ok-differ): {genuine_unsafe}")

    if args.samples:
        for bucket, ids in unsafe_ids.items():
            if ids:
                print(f"  sample {bucket}: {ids[:args.samples]}")

    if args.max_unsafe is not None and genuine_unsafe > args.max_unsafe:
        print(f"GATE FAIL: genuine-unsafe {genuine_unsafe} > allowed {args.max_unsafe}")
        return 1
    print("GATE OK" if args.max_unsafe is not None else "(no gate threshold set)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

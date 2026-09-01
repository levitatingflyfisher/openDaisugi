"""The five cross-layer pairs.

The layer boundaries are contracts, so most cross-layer cells are theatre:
the two sides are independent by construction. These five are where a real
coupling exists: a shared input shape, a shared threshold, or a shared wire
format. A pair that surprises us gets promoted to a permanent row. Nothing
gets a row on speculation, and there is no sixth pair without an argument
for it in WHY.
"""

from __future__ import annotations

import itertools

from opendaisugi.bench.corpus import load_json, load_jsonl, resolve_corpus
from opendaisugi.bench.layers import backend as backend_bench
from opendaisugi.bench.layers import gate_path as gate_path_bench
from opendaisugi.bench.layers import loop as loop_bench
from opendaisugi.bench.layers import matcher as matcher_bench
from opendaisugi.bench.layers import router as router_bench
from opendaisugi.bench.layers import verifier as verifier_bench
from opendaisugi.bench.options import (
    GATE_PATHS,
    LOOPS,
    MATCHER_BACKENDS,
    VERIFIER_CLIENTS,
    build_hint,
    client_argv,
    client_is_built,
)
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table

PAIRS: tuple[str, ...] = (
    "verifier-x-shell",
    "matcher-x-distiller",
    "router-x-models",
    "loop-x-gate-path",
    "backend-x-envelope",
)

_REPRODUCE = "uv run --no-sync daisugi bench pairs"

WHY: dict[str, tuple[str, str]] = {
    "verifier-x-shell": (
        "verifier x shell",
        "both sides read the same command text; a decomposition change moves every client",
    ),
    "matcher-x-distiller": (
        "matcher x distiller",
        "one embedding space serves reuse and clustering, at one threshold",
    ),
    "router-x-models": (
        "router x model set",
        "a router's saving is meaningless without the price of the models it picks",
    ),
    "loop-x-gate-path": (
        "loop x gate path",
        "a loop is only as gated as its path, and the class differs per path",
    ),
    "backend-x-envelope": (
        "backend x envelope source",
        "the envelope source decides whether the backend is called at all",
    ),
}


def _decomposition_cases(cases: list[dict], setting: bool) -> list[dict]:
    """Re-derive the verify cases with `shell_allow_decomposition` set one way.

    `shell_allow_decomposition` is an envelope field, so the axis only exists
    on verify cases. A decompose case carries no envelope. Flipping it changes
    the envelope, so the expectation is recomputed by the oracle and the case
    re-addressed. Scoring a client against an expectation that belongs to the
    other setting would be a label, not a measurement.
    """
    from opendaisugi.conformance import case_id
    from opendaisugi.models import ActionPlan, Envelope
    from opendaisugi.verify import verify

    out: list[dict] = []
    for case in cases:
        if case["kind"] != "verify":
            continue
        envelope = Envelope.model_validate(case["envelope"])
        envelope = envelope.model_copy(
            update={
                "permissions": envelope.permissions.model_copy(
                    update={"shell_allow_decomposition": setting}
                )
            }
        )
        plan = ActionPlan.model_validate(case["plan"])
        options = case["options"]
        result = verify(
            plan,
            envelope,
            strict=options.get("strict"),
            z3_timeout_ms=int(options.get("z3_timeout_ms", 500)),
        )
        body = {
            "kind": "verify",
            "v": case["v"],
            "plan": case["plan"],
            "envelope": envelope.model_dump(mode="json"),
            "options": options,
            "expect": {
                "ok": result.ok,
                "violations": [
                    {"stage": v.stage, "step": v.detail.get("step")} for v in result.violations
                ],
            },
        }
        body["id"] = case_id(body)
        out.append(body)
    return out


def verifier_x_shell(opts: BenchOpts) -> Table:
    """Each client's verify agreement with shell decomposition on and off.

    The axis is a real envelope field, set to both values, with the oracle's
    expectation recomputed for each. A row labelled `off` was produced with
    the field off. A client that breaks is named in its own cell and its
    unanswered cases count as disagreements, the same as the layer bench.
    """
    ref = resolve_corpus(verifier_bench.CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    columns = (
        Column("option", "client"),
        Column("decomposition", "decomposition"),
        Column("cases", "cases", align="right"),
        Column("allows", "oracle allows", align="right"),
        Column("agree", "agree", align="right"),
        Column("fail_open", "fail-open", align="right"),
        Column("failure", "failure"),
    )
    variants = {"off": _decomposition_cases(cases, False), "on": _decomposition_cases(cases, True)}
    allows = {k: sum(1 for c in v if c["expect"]["ok"]) for k, v in variants.items()}
    separated = any(
        a["expect"]["ok"] != b["expect"]["ok"]
        for a, b in zip(variants["off"], variants["on"], strict=True)
    )
    rows: list[Row] = []
    for name, spec in VERIFIER_CLIENTS.items():
        if not client_is_built(spec):
            rows.append(Row(name=name, absent=build_hint(spec)))
            continue
        # Both settings go down one stdin stream, so a client is started once
        # and one hung client costs one timeout, not one per setting. The two
        # variants are different envelopes with different content addresses,
        # so their verdicts split cleanly by id.
        verdicts, _, failure = verifier_bench.client_verdicts(
            client_argv(spec),
            variants["off"] + variants["on"],
            timeout_s=verifier_bench.CLIENT_TIMEOUT_S,
        )
        for setting in ("off", "on"):
            subset = variants[setting]
            counts = verifier_bench.counts_or_unknown(subset, verdicts, failure)
            rows.append(
                Row(
                    name=f"{name}/{setting}",
                    cells={
                        "option": name,
                        "decomposition": setting,
                        "cases": len(subset),
                        "allows": allows[setting],
                        "agree": counts["agree"],
                        "fail_open": counts["fail_open"],
                        "failure": failure or "",
                    },
                )
            )
    notes = [
        WHY["verifier-x-shell"][1],
        "the axis is Permission.shell_allow_decomposition, set both ways, with the "
        "oracle's expectation recomputed for each; oracle allows counts the cases the "
        "oracle accepts under that setting",
        "a hung, crashed or garbage-writing client is named in the failure column and "
        "its scored cells read n/a, because unknown is not zero",
    ]
    if not separated:
        notes.append(
            "this corpus does not separate the two settings: no verify case changes its "
            "oracle verdict with decomposition on, so the on and off rows measure the same thing"
        )
    return Table(
        "verifier-x-shell",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=tuple(notes),
    )


def matcher_x_distiller(opts: BenchOpts) -> Table:
    """Cluster the corpus with each embedder at its own threshold; report purity."""
    import numpy as np

    from opendaisugi.distiller import _cluster_with_centroids

    ref = resolve_corpus(matcher_bench.CORPUS, opts.corpus)
    task_rows = load_jsonl(ref)
    tasks = [r["task"] for r in task_rows]
    task_by_id = {r["id"]: r["task"] for r in task_rows}
    para_ref, paras = matcher_bench._paraphrases_beside(ref, task_by_id)
    texts = tasks + [p["task"] for p in paras]
    truth = list(range(len(tasks))) + [tasks.index(task_by_id[p["of"]]) for p in paras]

    columns = (
        Column("option", "embedder"),
        Column("threshold", "threshold", align="right"),
        Column("clusters", "clusters", align="right"),
        Column("purity", "paraphrase purity", align="right"),
        Column("singletons", "singletons", align="right"),
    )
    rows: list[Row] = []
    for name in MATCHER_BACKENDS:
        embedder = matcher_bench.build_embedder(name)
        if embedder is None:
            rows.append(Row(name=name, absent=matcher_bench.absent_hint(name)))
            continue
        thr = matcher_bench._threshold_for(name)
        vecs = np.asarray(embedder.encode(texts))
        clusters = _cluster_with_centroids(list(range(len(texts))), vecs, threshold=thr)
        together = 0
        for members, _ in clusters:
            labels = [truth[i] for i in members]
            together += sum(1 for a, b in itertools.combinations(labels, 2) if a == b)
        rows.append(
            Row(
                name=f"{name}@{thr}",
                cells={
                    "option": name,
                    "threshold": thr,
                    "clusters": len(clusters),
                    "purity": round(together / max(1, len(paras)), 3),
                    "singletons": sum(1 for m, _ in clusters if len(m) == 1),
                },
            )
        )
    return Table(
        "matcher-x-distiller",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(
            WHY["matcher-x-distiller"][1],
            "purity is the share of paraphrase pairs that land in one cluster at the "
            "embedder's own threshold",
        ),
        companions=(para_ref,),
    )


def router_x_models(opts: BenchOpts) -> Table:
    """Each router crossed with each cheap-model choice in the committed price table."""
    ref = resolve_corpus(router_bench.CORPUS, opts.corpus)
    turns = load_jsonl(ref)
    prices_ref = router_bench._beside(ref, router_bench.PRICES)
    targets_ref = router_bench._beside(ref, router_bench.TARGETS)
    targets = load_json(targets_ref)
    prices = {k: (float(v[0]), float(v[1])) for k, v in load_json(prices_ref).items()}

    columns = (
        Column("option", "router"),
        Column("cheap_model", "cheap model"),
        Column("turns", "turns", align="right"),
        Column("local_share", "local share", align="right"),
        Column("cost", "est. cost $", align="right"),
    )
    local = targets["local"]
    candidates = [targets["cheap"], local]
    rows: list[Row] = []
    for option in router_bench.ROUTERS:
        for cheap in candidates:
            these = dict(targets, cheap=cheap)
            chosen = [router_bench.choose(option, r, these) for r in turns]
            cost = sum(
                router_bench._cost(m, r["usage"], prices, prices_rel=prices_ref.rel)
                for m, r in zip(chosen, turns, strict=True)
            )
            rows.append(
                Row(
                    name=f"{option}/{cheap}",
                    cells={
                        "option": option,
                        "cheap_model": cheap,
                        "turns": len(turns),
                        "local_share": round(
                            sum(m == local for m in chosen) / max(1, len(turns)), 4
                        ),
                        "cost": round(cost, 4),
                    },
                )
            )
    return Table(
        "router-x-models",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(WHY["router-x-models"][1],),
        companions=(prices_ref, targets_ref),
    )


def loop_x_gate_path(opts: BenchOpts) -> Table:
    """Every loop against its gate path: vocabulary cases, contract, class."""
    ref = resolve_corpus(loop_bench.CORPUS, opts.corpus)
    cases = load_jsonl(ref)
    vocab_ref = loop_bench._vocabulary_beside(ref)
    vocab = load_json(vocab_ref)
    envelope_ref = gate_path_bench.battery_envelope_ref()
    envelope = gate_path_bench.Envelope.model_validate(load_json(envelope_ref))
    gate_ref = resolve_corpus(gate_path_bench.CORPUS, None)
    gate_cases = load_jsonl(gate_ref)
    columns = (
        Column("option", "loop"),
        Column("gate_path", "gate path"),
        Column("gate_cases", "gate cases"),
        Column("contract", "path contract"),
        Column("fail_open", "fail-open class"),
    )
    rows: list[Row] = []
    for name, spec in LOOPS.items():
        entry = vocab.get(name) or {}
        if not entry.get("names"):
            rows.append(Row(name=name, absent=f"{entry.get('note', 'not pinned')}: {spec.install}"))
            continue
        passed, applicable, na = loop_bench.run_vocabulary_cases(name, cases, vocab, envelope)
        cpassed, ctotal = gate_path_bench.run_contract(spec.gate_path, gate_cases, envelope)
        rows.append(
            Row(
                name=f"{name}/{spec.gate_path}",
                cells={
                    "option": name,
                    "gate_path": spec.gate_path,
                    "gate_cases": f"{passed}/{applicable}" + (f", {na} n/a" if na else ""),
                    "contract": f"{cpassed}/{ctotal}",
                    "fail_open": GATE_PATHS[spec.gate_path].fail_open_class,
                },
            )
        )
    return Table(
        "loop-x-gate-path",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(WHY["loop-x-gate-path"][1],),
        companions=(vocab_ref, envelope_ref, gate_ref),
    )


def backend_x_envelope(opts: BenchOpts) -> Table:
    """Each backend crossed with each envelope source.

    evidence-inferred spends nothing on any backend: the envelope is built
    from observed steps with no model call, so `tokens: 0` is a fact about
    that source. Everything else about an evidence-inferred envelope is not
    known here. It is a different envelope with a different clause count, and
    none are recorded, so those cells print `n/a`. Reusing the LLM row's
    numbers under this label would be a fabricated cell.
    """
    ref = resolve_corpus(backend_bench.CORPUS, opts.corpus)
    task_ids = {t["id"] for t in load_jsonl(ref)}
    columns = (
        Column("option", "backend"),
        Column("envelope_source", "envelope source"),
        Column("envelopes", "envelopes", align="right"),
        Column("mean_clauses", "mean clauses", align="right"),
        Column("tokens", "tokens", align="right"),
    )
    from opendaisugi.models import Envelope
    from opendaisugi.swap import SWAP_KNOBS

    rows: list[Row] = []
    companions = []
    # The auto option names no backend of its own, so it has no row here.
    for option in SWAP_KNOBS["backend"].options:
        if option.value is None:
            continue
        name = str(option.value)
        found = backend_bench._recorded(ref, name, task_ids)
        if found is None:
            rows.append(Row(name=name, absent=backend_bench.absent_hint(ref, name)))
            continue
        rec_ref, recorded = found
        companions.append(rec_ref)
        envelopes = [Envelope.model_validate(r["envelope"]) for r in recorded]
        clauses = [backend_bench.clause_count(e) for e in envelopes]
        rows.append(
            Row(
                name=f"{name}/llm-generated",
                cells={
                    "option": name,
                    "envelope_source": "llm-generated",
                    "envelopes": len(envelopes),
                    "mean_clauses": round(sum(clauses) / max(1, len(clauses)), 1),
                    "tokens": sum(int(r["tokens_in"]) + int(r["tokens_out"]) for r in recorded),
                },
            )
        )
        rows.append(
            Row(
                name=f"{name}/evidence-inferred",
                cells={
                    "option": name,
                    "envelope_source": "evidence-inferred",
                    # Nothing evidence-inferred was recorded, so nothing is
                    # claimed for it beyond the one fact the source
                    # guarantees: no model call, therefore no tokens.
                    "envelopes": "n/a",
                    "mean_clauses": "n/a",
                    "tokens": 0,
                },
            )
        )
    return Table(
        "backend-x-envelope",
        columns,
        tuple(rows),
        ref,
        _REPRODUCE,
        notes=(
            WHY["backend-x-envelope"][1],
            "evidence-inferred rows show n/a for what was not recorded; only tokens=0 "
            "is claimed, and that follows from that source making no model call",
        ),
        companions=tuple(companions),
    )


_RUNNERS = {
    "verifier-x-shell": verifier_x_shell,
    "matcher-x-distiller": matcher_x_distiller,
    "router-x-models": router_x_models,
    "loop-x-gate-path": loop_x_gate_path,
    "backend-x-envelope": backend_x_envelope,
}


def run_all(opts: BenchOpts) -> list[Table]:
    """One table per pair, in PAIRS order. Each pair runs exactly once."""
    return [_RUNNERS[name](opts) for name in PAIRS]


def index_of(tables: list[Table]) -> Table:
    """The index table, derived from tables that already ran.

    Deriving rather than re-running is the point: the index needs a row
    count, and re-running five benches to count their rows would double the
    cost of `daisugi bench pairs` for no new information.
    """
    columns = (
        Column("option", "pair"),
        Column("layers", "layers"),
        Column("rows", "rows", align="right"),
        Column("why", "why it earns a row"),
    )
    rows = []
    for name, table in zip(PAIRS, tables, strict=True):
        title, why = WHY[name]
        rows.append(
            Row(
                name=name,
                cells={"option": name, "layers": title, "rows": len(table.rows), "why": why},
            )
        )
    return Table(
        "pairs",
        columns,
        tuple(rows),
        None,
        _REPRODUCE,
        notes=("five pairs, by argument; a sixth needs a reason recorded in pairs.py",),
    )


def run(opts: BenchOpts) -> Table:
    """The index: which pairs exist, which layers they cross, and why."""
    return index_of(run_all(opts))


register(BenchSpec(name="pairs", layer="pairs", corpus="tasks.jsonl", columns=(), run=run))

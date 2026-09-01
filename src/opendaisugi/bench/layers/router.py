"""`daisugi bench router`: one committed journal slice, four routing options.

The gateway is the meter, so the comparison is honest only if every option
sees the same turns and the same price table. Prices come from `prices.json`
and the three model targets from `switchyard-targets.json`, both read beside
the journal slice and both pinned in the table, so an operator can edit them
for their own plan and re-run the reproduce line.

The two Switchyard rows are fakes: pure functions in this module that mimic
the stage and escalation strategies Switchyard documents. Their source cell
says `fake`. Nothing here imports or runs the real router. A fake row is a
shape, not a measurement of NVIDIA's router, and the note line says so on
every table.
"""

from __future__ import annotations

from pathlib import Path

from opendaisugi.bench.corpus import (
    CorpusMissing,
    CorpusRef,
    corpus_ref,
    load_json,
    load_jsonl,
    resolve_corpus,
)
from opendaisugi.bench.registry import BenchOpts, BenchSpec, register
from opendaisugi.bench.table import Column, Row, Table
from opendaisugi.gateway import _latest_user_text, price_turn, route_turn
from opendaisugi.routing import estimate_difficulty

CORPUS = "journal-slice.jsonl"
PRICES = "prices.json"
TARGETS = "switchyard-targets.json"
ROUTERS: tuple[str, ...] = ("rules", "switchyard:stage", "switchyard:escalation", "off")

COLUMNS: tuple[Column, ...] = (
    Column("option", "option"),
    Column("turns", "turns", align="right"),
    Column("local_share", "local share", align="right"),
    Column("cost", "est. cost $", align="right"),
    Column("pass_rate", "pass-rate", align="right"),
    Column("source", "source"),
)


def stage_router(difficulty: float, targets: dict[str, str]) -> str:
    """Fake stage router: three bands, cheapest first. Mimics Switchyard's shape."""
    if difficulty < 0.25:
        return targets["local"]
    if difficulty < 0.5:
        return targets["cheap"]
    return targets["capable"]


def escalation_router(difficulty: float, targets: dict[str, str]) -> str:
    """Fake escalation router: try local, escalate only when the turn is hard."""
    return targets["capable"] if difficulty >= 0.5 else targets["local"]


def choose(option: str, row: dict, targets: dict[str, str]) -> str:
    """The model this option sends the turn to. An unknown option raises KeyError."""
    body = row["body"]
    if option == "off":
        return str(body.get("model", ""))
    if option == "rules":
        # The built-in router gets the same three targets as the Switchyard
        # rows. Its local rung only exists when a local model is offered, so
        # without one its local share would be 0.0 by construction.
        return route_turn(
            body,
            cheap_model=targets["cheap"],
            local_model=targets["local"],
            prefix_tokens=row.get("prefix_tokens"),
        ).model
    difficulty = estimate_difficulty(_latest_user_text(body))
    if option == "switchyard:stage":
        return stage_router(difficulty, targets)
    if option == "switchyard:escalation":
        return escalation_router(difficulty, targets)
    raise KeyError(option)


def _source(option: str) -> str:
    """`built-in` for the gateway's own ladder and for off; `fake` for the Switchyard shapes."""
    return "built-in" if option in ("rules", "off") else "fake"


def _beside(ref: CorpusRef, name: str) -> CorpusRef:
    """A companion file in the directory of the journal slice that was read."""
    path = Path(ref.path).with_name(name)
    if not path.is_file():
        raise CorpusMissing(
            f"No {name} at {path}.\n"
            f"The router bench reads {PRICES} and {TARGETS} from the directory of the "
            f"journal slice.\n"
            f"Put both beside {ref.path.name}, or run without --corpus for the committed set."
        )
    return corpus_ref(path)


def _cost(
    model: str, usage: dict, prices: dict[str, tuple[float, float]], *, prices_rel: str
) -> float:
    """The turn's cost on `model` from the committed table only.

    `price_turn` would fall back to a constant for a model the table lacks,
    and the note says prices come from the file, so an unlisted model is a
    corpus error naming the model and the file, never a silent guess.
    """
    if model not in prices:
        raise CorpusMissing(
            f"{prices_rel} has no price for {model!r}.\n"
            f"Add a [per_input_mtok, per_output_mtok] entry for it, or route the "
            f"journal slice to models the table lists."
        )
    return price_turn(
        model,
        int(usage.get("input_tokens", 0)),
        int(usage.get("output_tokens", 0)),
        cache_read_tokens=int(usage.get("cache_read_input_tokens", 0)),
        cache_creation_tokens=int(usage.get("cache_creation_input_tokens", 0)),
        prices=prices,
    ).dollars


def run(opts: BenchOpts) -> Table:
    ref = resolve_corpus(CORPUS, opts.corpus)
    turns = load_jsonl(ref)
    prices_ref = _beside(ref, PRICES)
    targets_ref = _beside(ref, TARGETS)
    targets = load_json(targets_ref)
    prices = {k: (float(v[0]), float(v[1])) for k, v in load_json(prices_ref).items()}
    local = targets["local"]

    rows: list[Row] = []
    for option in ROUTERS:
        chosen = [choose(option, row, targets) for row in turns]
        cost = sum(
            _cost(m, row["usage"], prices, prices_rel=prices_ref.rel)
            for m, row in zip(chosen, turns, strict=True)
        )
        rows.append(
            Row(
                name=option,
                cells={
                    "option": option,
                    "turns": len(turns),
                    "local_share": round(sum(m == local for m in chosen) / max(1, len(turns)), 4),
                    "cost": round(cost, 4),
                    "pass_rate": "n/a, no outcomes",
                    "source": _source(option),
                },
            )
        )
    return Table(
        layer="router",
        columns=COLUMNS,
        rows=tuple(rows),
        corpus=ref,
        companions=(prices_ref, targets_ref),
        reproduce=f"uv run --no-sync daisugi bench router --corpus {ref.rel}",
        notes=(
            f"prices come from {PRICES} beside the journal slice; edit it for your plan and re-run",
            "the corpus path is repo-relative, so run the reproduce line from the checkout root",
            "every option is offered the same three targets, so local share compares like for like",
            "a switchyard row marked fake mimics the documented strategy; it is not NVIDIA's router",
            "pass-rate needs recorded outcomes; this corpus has none, so the column says so",
        ),
    )


register(BenchSpec(name="router", layer="router", corpus=CORPUS, columns=COLUMNS, run=run))

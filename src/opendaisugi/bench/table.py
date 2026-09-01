"""One table shape for every layer bench, human and JSON.

Two rules live here. A row for an option that is not installed prints `absent`
with the command that installs it, never nothing. A silently missing row reads
as "we compared everything". A column that measures time is marked volatile,
so the determinism check can compare two runs without a stopwatch defeating it.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from opendaisugi.bench.corpus import CorpusRef


@dataclass(frozen=True)
class Column:
    key: str
    title: str
    align: str = "left"
    volatile: bool = False


@dataclass(frozen=True)
class Row:
    name: str
    cells: dict[str, object] = field(default_factory=dict)
    absent: str | None = None


@dataclass(frozen=True)
class Table:
    layer: str
    columns: tuple[Column, ...]
    rows: tuple[Row, ...]
    corpus: CorpusRef | None
    reproduce: str
    notes: tuple[str, ...] = ()
    companions: tuple[CorpusRef, ...] = ()


def _cell_text(value: object) -> str:
    """Floats keep up to four decimals and always show one, so 0.0152 is not 0.02."""
    if isinstance(value, float):
        text = f"{value:.4f}".rstrip("0")
        return text + "0" if text.endswith(".") else text
    return "" if value is None else str(value)


def render(table: Table) -> str:
    """The human table: header, rows, notes, corpus, reproduce."""
    first = table.columns[0]
    widths = {c.key: len(c.title) for c in table.columns}
    for row in table.rows:
        if row.absent is not None:
            widths[first.key] = max(widths[first.key], len(row.name))
            continue
        for c in table.columns:
            widths[c.key] = max(widths[c.key], len(_cell_text(row.cells.get(c.key))))

    def _pad(c: Column, text: str) -> str:
        return text.rjust(widths[c.key]) if c.align == "right" else text.ljust(widths[c.key])

    out: list[str] = [f"{table.layer}: {len(table.rows)} options", ""]
    out.append("  " + "  ".join(_pad(c, c.title) for c in table.columns))
    for row in table.rows:
        if row.absent is not None:
            out.append(f"  {_pad(first, row.name)}  absent: {row.absent}")
            continue
        out.append(
            "  " + "  ".join(_pad(c, _cell_text(row.cells.get(c.key))) for c in table.columns)
        )
    if table.notes:
        out.append("")
        out.extend(f"note: {n}" for n in table.notes)
    out.append("")
    if table.corpus is None:
        out.append("corpus: none")
    else:
        out.append(f"corpus: {table.corpus.rel} ({table.corpus.short})")
    out.extend(f"corpus: {c.rel} ({c.short})" for c in table.companions)
    out.append(f"reproduce: {table.reproduce}")
    return "\n".join(out)


def _row_json(row: Row) -> dict:
    if row.absent is not None:
        return {"name": row.name, "absent": row.absent}
    return {"name": row.name, **row.cells}


def _corpus_json(ref: CorpusRef) -> dict:
    return {"path": ref.rel, "sha256": ref.sha256}


def to_dict(table: Table) -> dict:
    """The machine form: layer, corpus, companions, columns, rows, notes, reproduce.

    `companions` are the other files the bench read. A table that reads two
    files pins both, or an edit to the second changes its numbers under an
    unchanged corpus line.
    """
    return {
        "layer": table.layer,
        "corpus": None if table.corpus is None else _corpus_json(table.corpus),
        "companions": [_corpus_json(c) for c in table.companions],
        "columns": [
            {"key": c.key, "title": c.title, "align": c.align, "volatile": c.volatile}
            for c in table.columns
        ],
        "rows": [_row_json(r) for r in table.rows],
        "notes": list(table.notes),
        "reproduce": table.reproduce,
    }


def to_json(table: Table) -> str:
    """`to_dict` as one indented JSON document."""
    return json.dumps(to_dict(table), indent=2)


def stable_rows(table: Table) -> list[dict]:
    """Rows with every volatile column removed. A determinism check compares these."""
    volatile = {c.key for c in table.columns if c.volatile}
    return [{k: v for k, v in _row_json(r).items() if k not in volatile} for r in table.rows]

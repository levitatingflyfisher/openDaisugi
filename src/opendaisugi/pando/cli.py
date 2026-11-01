"""``daisugi pando`` in Python: the read verbs over a store (PD-R-10)."""

from __future__ import annotations

import json
from pathlib import Path

import typer

from opendaisugi.pando.store import (
    DEFAULT_STORE,
    READ_ONLY,
    PandoStore,
    list_stores,
    store_dir,
)

pando_app = typer.Typer(
    name="pando",
    help="Pando R2, the conversation memory: log, summary tree and view.",
    no_args_is_help=True,
)

_STORE = typer.Option(DEFAULT_STORE, "--store", help="The store: work, home or another name.")
_DATA = typer.Option(None, "--data-dir", help="The data home that holds pando/<store>.")
_JSON = typer.Option(False, "--json", help="Print JSON.")


def _data(data_dir: Path | None) -> Path:
    if data_dir is not None:
        return Path(data_dir)
    from opendaisugi.datahome import data_home

    return Path(data_home())


# A process that is a worker (DAISUGI_PANDO_SKIP) or works in a branch of
# the memory (DAISUGI_PANDO_BRANCH, round 8b) reads only its own part of the
# tree. The Python reader does not scope to a branch, so it reads nothing for
# such a process: the Go or Rust daisugi serves branches.
MARKED = (
    "This process works in a branch of the memory or is a worker. The Python reader "
    "does not serve branches; it reads nothing here. Use the Go or Rust daisugi."
)


def marked() -> bool:
    """Whether this process carries a Pando marker (a worker or a branch)."""
    import os

    skip = os.environ.get("DAISUGI_PANDO_SKIP", "").strip()
    return (skip not in ("", "0")) or bool(os.environ.get("DAISUGI_PANDO_BRANCH", "").strip())


def _open(store: str, data_dir: Path | None) -> PandoStore:
    if marked():
        typer.echo(MARKED, err=True)
        raise typer.Exit(1)
    try:
        d = store_dir(_data(data_dir), store)
    except ValueError as e:
        typer.echo(str(e), err=True)
        raise typer.Exit(2) from e
    if not d.is_dir():
        typer.echo(f"no store {store} at {d}", err=True)
        raise typer.Exit(1)
    return PandoStore.open(d)


def _fail(e: Exception) -> typer.Exit:
    typer.echo(str(e), err=True)
    return typer.Exit(1)


@pando_app.command("stores")
def stores(data_dir: Path | None = _DATA, as_json: bool = _JSON) -> None:
    """List the stores."""
    names = list_stores(_data(data_dir))
    typer.echo(json.dumps(names) if as_json else "\n".join(names), nl=bool(names or as_json))


@pando_app.command("status")
def status(store: str = _STORE, data_dir: Path | None = _DATA, as_json: bool = _JSON) -> None:
    """Show a store: messages and where it is."""
    s = _open(store, data_dir)
    if as_json:
        typer.echo(
            json.dumps(
                {
                    "store": store,
                    "path": str(s.path),
                    "messages": s.count,
                    "not_in_view": s.not_in_view,
                }
            )
        )
    else:
        typer.echo(
            f"store: {store}\npath: {s.path}\nmessages: {s.count}\nnot in the view yet: {s.not_in_view}"
        )


@pando_app.command("log")
def log(
    store: str = _STORE,
    data_dir: Path | None = _DATA,
    day: str = typer.Option("", "--day", help="One day, YYYY-MM-DD."),
    as_json: bool = _JSON,
) -> None:
    """Print the messages, one per line."""
    s = _open(store, data_dir)
    try:
        msgs = s.day(day) if day else [s.message(i) for i in range(s.count)]
    except ValueError as e:
        raise _fail(e) from e
    for m in msgs:
        if as_json:
            typer.echo(json.dumps(m.__dict__ | {"act": m.act}, ensure_ascii=False))
        else:
            typer.echo(f"{m.i}\t{m.kind}\t{m.date}\t{' '.join(m.text.splitlines())}")


@pando_app.command("zoom")
def zoom(
    id: int,
    n: int,
    store: str = _STORE,
    data_dir: Path | None = _DATA,
    offset: int = typer.Option(0, "--offset"),
    size: int = typer.Option(0, "--size"),
) -> None:
    """Open a node: ID N. N=1 is the message word for word."""
    s = _open(store, data_dir)
    try:
        text = s.zoom(id, n, offset, size)
    except ValueError as e:
        raise _fail(e) from e
    typer.echo(text, nl=not text.endswith("\n"))


@pando_app.command("date")
def date(id: int, store: str = _STORE, data_dir: Path | None = _DATA) -> None:
    """Print when message ID was logged, in UTC."""
    s = _open(store, data_dir)
    try:
        typer.echo(s.date(id))
    except ValueError as e:
        raise _fail(e) from e


@pando_app.command("view")
def view(store: str = _STORE, data_dir: Path | None = _DATA) -> None:
    """Print the view: the summary lines a turn would see."""
    for name, text in _open(store, data_dir).view_lines():
        typer.echo(f"{name}\t{text}")


@pando_app.command("recall")
def recall(
    query: str,
    store: str = _STORE,
    data_dir: Path | None = _DATA,
    k: int = typer.Option(8, "-k"),
) -> None:
    """Find where a word was said: QUERY [-k N]. It gives ids, then zoom."""
    for h in _open(store, data_dir).recall(query, k):
        typer.echo(h.format())


@pando_app.command("serve")
def serve(store: str = _STORE, data_dir: Path | None = _DATA) -> None:
    """Run a read-only MCP server named pando on stdin and stdout."""
    from opendaisugi.pando.mcp import serve as run
    from opendaisugi.pando.mcp import serve_closed

    if marked():
        serve_closed()
        return
    run(_open(store, data_dir))


def _refuse() -> None:
    typer.echo(READ_ONLY, err=True)
    raise typer.Exit(2)


@pando_app.command("append")
def append(text: str) -> None:
    """Add a message. The Python package does not write stores."""
    _refuse()


@pando_app.command("tend")
def tend() -> None:
    """Build summary nodes. The Python package does not write stores."""
    _refuse()


@pando_app.command("import")
def import_() -> None:
    """Bring old chats in. The Python package does not write stores."""
    _refuse()


@pando_app.command("migrate")
def migrate() -> None:
    """Move the old coppice log into pando/work. Go and Rust do this."""
    _refuse()

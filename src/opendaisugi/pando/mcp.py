"""A read-only MCP server named ``pando`` over one store: zoom, date, recall."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from opendaisugi.pando.store import PandoStore

ZOOM_DESCRIPTION = (
    "Open a node of the chat's memory: give id and n. id is the first message, n the count: "
    "a power of two. n = 1 gives the message word for word; a larger node gives its two halves "
    "as lines. A long node comes in pages: give offset (a byte) and size to read the next page. "
    "A bad id answers with the root ids. A worker's report names its child store and the root "
    "ids of its transcript: give child, the worker's label, with an id and an n from those roots "
    "to read inside that transcript. Without child the id is in the chat."
)


def build_pando_server(store: PandoStore):
    """Build the FastMCP server. It has no write tool."""
    from mcp.server.fastmcp import FastMCP

    mcp = FastMCP(name="pando")

    def now() -> PandoStore:
        """The store as it is now: reads take no lock, and a server that runs
        for hours must see what was logged since it started. A store that
        does not read keeps the last snapshot."""
        from opendaisugi.pando.store import PandoStore

        try:
            return PandoStore.open(store.path)
        except (OSError, ValueError):
            return store

    @mcp.tool(description=ZOOM_DESCRIPTION)
    def zoom(id: int, n: int, offset: int = 0, size: int = 0, child: str = "") -> str:
        cur = now()
        if child:
            return cur.zoom_child(child, id, n, offset, size)
        return cur.zoom(id, n, offset, size)

    @mcp.tool(description="When message id was logged, in UTC.")
    def date(id: int) -> str:
        return now().date(id)

    @mcp.tool(
        description=(
            "Find where a word or a topic was said: give a query. You get node ids with a score "
            "and one line each, best first. Then zoom into an id to read it. k is how many "
            "(default 8, at most 32)."
        )
    )
    def recall(query: str, k: int = 8) -> str:
        hits = now().recall(query, k)
        return "\n".join(h.format() for h in hits) if hits else "no match"

    return mcp


def serve(store: PandoStore) -> None:
    build_pando_server(store).run()


def serve_closed() -> None:
    """A server with no tool, for a worker or a process in a branch: the
    Python reader never falls back to the whole store for such a process."""
    from mcp.server.fastmcp import FastMCP

    FastMCP(name="pando").run()

"""Pando R2, the conversation memory, as a read-only Python client.

A store is a dir, ``<data home>/pando/<store>``: a verbatim log (``main/``),
a summary tree (``tree/``) and a saved view (``view.json``). The Go and Rust
ports write it; this package reads it and never writes (ADR-0023, PD-R-10).
"""

from __future__ import annotations

from opendaisugi.pando.store import (
    DEFAULT_STORE,
    ZOOM_PAGE_BYTES,
    Message,
    PandoStore,
    list_stores,
    store_dir,
    valid_store,
)

__all__ = [
    "DEFAULT_STORE",
    "ZOOM_PAGE_BYTES",
    "Message",
    "PandoStore",
    "list_stores",
    "store_dir",
    "valid_store",
]

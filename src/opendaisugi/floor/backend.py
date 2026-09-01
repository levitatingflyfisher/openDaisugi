"""The PaneBackend contract (master spec §3.2) — protocol + value types.

No implementations here — coppice, Herdr, and tmux backends are spec-02/03.
This module exists so spec-02/03/04/05 can type against one contract before
any backend exists.
"""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Literal, Protocol

from opendaisugi.floor.events import PaneStateEvent


@dataclass(frozen=True)
class PaneRef:
    backend: str
    id: str


@dataclass(frozen=True)
class PaneInfo:
    ref: PaneRef
    label: str
    cwd: str
    cmd: list[str]
    kind: str
    state: PaneStateEvent | None = None


@dataclass(frozen=True)
class Frame:
    pane: str
    seq: int
    cols: int
    rows: int
    cursor: tuple[int, int]
    rows_changed: dict[int, list[list]]


class PaneBackend(Protocol):
    name: str
    yields_frames: bool  # only coppice is True; the floor's attach depends on knowing

    def available(self) -> bool: ...
    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"],
        harness: str | None = None,
    ) -> PaneRef: ...
    # ``harness`` names the adapter (e.g. "claude-code", "codex", "pi",
    # "opencode", "sprig" — master §3.4's table) so coppice-server can pick
    # the right headless adapter and PaneStateEvents can carry a real
    # ``harness`` value from spawn onward. Required when
    # ``kind == "headless"``; a PTY pane infers its harness later, from
    # the manifest fallback or its own hook, so it may omit it. Not
    # enforced here — ``PaneBackend`` is a contract for implementers
    # (spec-02/03/04/05), not a runtime check.
    def list(self) -> list[PaneInfo]: ...
    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None: ...
    def send_keys(self, pane: PaneRef, keys: list[str]) -> None: ...
    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str: ...
    def resize(self, pane: PaneRef, cols: int, rows: int) -> None: ...
    def close(self, pane: PaneRef) -> None: ...
    def subscribe(self) -> Iterator[PaneStateEvent | Frame]: ...
    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None: ...

    # The five members below are declared here so a type checker can name
    # them, but nothing checks a caller actually implements them: this
    # project runs no mypy or pyright, and a Protocol carries no built-in
    # "optional member" marker. A caller reaches them through
    # ``getattr(backend, name, None)``, the pattern ``registry.prompt_pane``
    # already uses for ``prompt``, which is not declared here at all. Only
    # coppice yields frames, so only coppice implements attach, detach and
    # last_error. close_connection releases a long-lived connection a
    # backend may hold open across calls; a backend with nothing of the
    # sort has nothing to release and may leave it out entirely. list_proven
    # tells a failed call apart from a genuinely empty answer, which list()
    # alone cannot: a caller that needs that distinction, such as
    # wait_for_state, reads it when a backend has one and otherwise treats
    # list() as fully proven.
    def attach(self, pane: PaneRef, cols: int, rows: int) -> str: ...
    def detach(self, pane: PaneRef) -> str: ...
    def last_error(self, request_id: str) -> Exception | None: ...
    def close_connection(self) -> None: ...
    def list_proven(self) -> tuple[bool, list[PaneInfo]]: ...

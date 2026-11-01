"""Rank view lines and raw messages by the garden's lexical matcher."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from opendaisugi.pando.store import PandoStore

RECALL_LINE_BYTES = 160
RECALL_MAX = 32


@dataclass(frozen=True)
class RecallHit:
    id: str
    score: float
    line: str

    def format(self) -> str:
        return f"{self.id}\t{self.score:.3f}\t{self.line}"


def _clip(s: str, n: int) -> str:
    raw = s.encode("utf-8")
    if len(raw) <= n:
        return s
    while n > 0 and (raw[n] & 0xC0) == 0x80:
        n -= 1
    return raw[:n].decode("utf-8")


def recall(store: PandoStore, query: str, k: int = 8) -> list[RecallHit]:
    """The best ``k`` hits, best first, ties by id. A score of 0 is left out.

    A level-0 view line is the message itself, so it counts once, as the
    message (PD-R-5).
    """
    from opendaisugi._search import _lexical_vector

    k = min(max(k, 1), RECALL_MAX)
    q = _lexical_vector(query)
    nq = math.sqrt(float((q * q).sum()))
    cands: list[tuple[float, int, int, RecallHit]] = []

    def add(first: int, count: int, name: str, text: str) -> None:
        v = _lexical_vector(text)
        nv = math.sqrt(float((v * v).sum()))
        if nq == 0 or nv == 0:
            return
        s = float((q * v).sum()) / (nq * nv)
        if s <= 0:
            return
        line = " ".join(text.split("\n"))
        cands.append((s, first, count, RecallHit(name, s, _clip(line, RECALL_LINE_BYTES))))

    for name, text in store.view_lines():
        first, count = (int(x) for x in name.split("+"))
        if count > 1:
            add(first, count, name, text)
    for i in range(store.count):
        add(i, 1, f"{i}+1", store.message(i).text)
    cands.sort(key=lambda c: (-c[0], c[1], c[2]))
    return [c[3] for c in cands[:k]]

"""Read a Pando R2 store: the log, the tree and the saved view.

This follows the Go package ``clients/go/pando`` (the oracle for this part).
It opens a store read only, takes no lock and repairs nothing. A last line a
writer has half written is skipped.
"""

from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path

DEFAULT_STORE = "work"
ZOOM_PAGE_BYTES = 32 * 1024
UNBUILT = "(not summarized yet: zoom it)"
ALL_REFUSED = "one store at a time: pando stores are separate on purpose."
READ_ONLY = "the Python package reads pando stores; coppice or daisugi (Go or Rust) writes them."
_STORE_RE = re.compile(r"^[a-z][a-z0-9-]{0,31}$")
_CHILD_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
CHILDREN_DIR = "children"
_MAX_LEVEL = 62


def valid_store(name: str) -> bool:
    """A store name: a letter, then letters, digits and dashes, 32 at most."""
    return bool(_STORE_RE.match(name))


def store_dir(data_dir: Path | str, store: str) -> Path:
    """``<data_dir>/pando/<store>``. ``all`` and bad names raise ValueError."""
    if store == "all":
        raise ValueError(ALL_REFUSED)
    if not valid_store(store):
        raise ValueError(
            f"bad store name {store!r}: use a-z, 0-9 and -, 1 to 32 letters, starting with a letter"
        )
    _no_link(Path(data_dir), "pando", store)
    return Path(data_dir) / "pando" / store


def _no_link(base: Path, *parts: str) -> None:
    """Check each part below base in turn and refuse a symbolic link: a link
    in a store or child path could reach another store. base itself may be
    a link. A part that does not exist ends the check."""
    p = base
    for part in parts:
        p = p / part
        if not os.path.lexists(p):
            return
        if p.is_symlink():
            raise ValueError(f"pando: {p} is a symbolic link; a store path may hold none")


def _store_links(path: Path) -> None:
    for part in ("main", "tree", "view.json", "import.json", CHILDREN_DIR, "lock"):
        _no_link(path, part)


def child_dir(store_path: Path | str, label: str) -> Path:
    """``<store>/children/<label>``: a worker's child store. A bad label or
    a link raises ValueError."""
    if not _CHILD_RE.match(label) or label == "..":
        raise ValueError(
            f"bad worker label {label!r}: use letters, digits, dot, dash and underscore, "
            "1 to 64, starting with a letter or digit"
        )
    _no_link(Path(store_path), CHILDREN_DIR, label)
    return Path(store_path) / CHILDREN_DIR / label


def list_stores(data_dir: Path | str) -> list[str]:
    """The stores under a data home: dirs of ``pando/`` that hold ``main/``."""
    root = Path(data_dir) / "pando"
    if not root.is_dir():
        return []
    return sorted(
        p.name
        for p in root.iterdir()
        if not p.is_symlink() and p.is_dir() and valid_store(p.name) and (p / "main").is_dir()
    )


@dataclass(frozen=True)
class Message:
    i: int
    kind: str
    text: str
    size: int
    date: str
    act: str | None = None


def _flat(s: str) -> str:
    return s.replace("\r\n", " ").replace("\n", " ").replace("\r", " ")


def _lines(path: Path):
    """The JSON objects of one file. A torn last line (no newline) is skipped."""
    data = path.read_bytes()
    if not data:
        return
    parts = data.split(b"\n")
    if parts and parts[-1] == b"":
        parts.pop()
    else:
        parts.pop()  # no final newline: a writer is part way through it
    for raw in parts:
        if not raw.strip():
            continue
        try:
            yield json.loads(raw)
        except ValueError:
            continue


class PandoStore:
    """A store opened read only. It has no ``append``."""

    def __init__(self, path: Path) -> None:
        self.path = path
        self._msgs: list[Message] = []
        self._nodes: dict[tuple[int, int], str] = {}
        self._view: list[tuple[int, int]] = []
        self._load()

    @classmethod
    def open(cls, path: Path | str) -> PandoStore:
        p = Path(path)
        if not p.is_dir():
            raise FileNotFoundError(f"no store {p.name} at {p}")
        _store_links(p)
        return cls(p)

    def _view_count(self) -> int | None:
        """The message count the saved view covers, or None when there is no
        usable view. The messages past it are not in the view yet; a reader
        still reads them, as Go's read only open does."""
        view = self.path / "view.json"
        if not view.is_file():
            return None
        try:
            return int(json.loads(view.read_text(encoding="utf-8"))["count"])
        except (ValueError, TypeError, KeyError, OSError):
            return None

    def _load(self) -> None:
        self._view_n = self._view_count() or 0
        by_i: dict[int, Message] = {}
        main = self.path / "main"
        for f in sorted(main.glob("*.jsonl")) if main.is_dir() else []:
            if f.is_symlink():  # a link could be another store's log
                continue
            for o in _lines(f):
                m = Message(
                    i=int(o["i"]),
                    kind=o.get("kind", ""),
                    text=o.get("text", ""),
                    size=int(o.get("size", 0)),
                    date=o.get("date", ""),
                    act=o.get("act"),
                )
                by_i[m.i] = m
        n = 0
        while n in by_i:
            n += 1
        self._msgs = [by_i[k] for k in range(n)]
        tree = self.path / "tree"
        for f in sorted(tree.glob("*.jsonl")) if tree.is_dir() else []:
            if f.is_symlink():
                continue
            for o in _lines(f):
                self._nodes[(int(o["l"]), int(o["i"]))] = o.get("text", "")
        view = self.path / "view.json"
        if view.is_file():
            try:
                doc = json.loads(view.read_text(encoding="utf-8"))
                self._view = [(int(a), int(b)) for a, b in doc.get("lines", [])]
            except (ValueError, TypeError):
                self._view = []

    @property
    def count(self) -> int:
        return len(self._msgs)

    @property
    def not_in_view(self) -> int:
        """The messages past the saved view: logged, not in the view yet."""
        return max(len(self._msgs) - self._view_n, 0)

    def zoom_child(self, label: str, id: int, n: int, offset: int = 0, size: int = 0) -> str:
        """zoom inside a worker's child store of this store. The label goes
        through child_dir, so it cannot leave the children dir."""
        d = child_dir(self.path, label)
        if not d.is_dir():
            raise FileNotFoundError(f"no worker transcript is named {label!r}")
        return PandoStore.open(d).zoom(id, n, offset, size)

    def message(self, i: int) -> Message:
        if i < 0 or i >= len(self._msgs):
            raise ValueError(f"pando: no message {i}")
        return self._msgs[i]

    def date(self, i: int) -> str:
        return self.message(i).date

    def day(self, day: str) -> list[Message]:
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", day):
            raise ValueError(f"pando: {day!r} is not a day like 2026-10-08")
        return [m for m in self._msgs if m.date.startswith(day)]

    def _line(self, level: int, idx: int) -> str:
        text = self._nodes.get((level, idx), UNBUILT)
        return f"{idx << level}+{1 << level}|{_flat(text)}\n"

    def roots(self) -> list[tuple[int, int]]:
        out, first = [], 0
        for lvl in range(_MAX_LEVEL, -1, -1):
            if (len(self._msgs) >> lvl) & 1:
                out.append((lvl, first >> lvl))
                first += 1 << lvl
        return out

    def _root_hint(self) -> str:
        roots = self.roots()
        if not roots:
            return " The chat is empty."
        return " The root nodes are " + ", ".join(f"{i << lv}+{1 << lv}" for lv, i in roots) + "."

    def _zoom_text(self, id_: int, n: int) -> str:
        if id_ < 0 or n <= 0 or n & (n - 1):
            raise ValueError(
                f"node {id_}+{n}: n must be a power of two and id at least 0" + self._root_hint()
            )
        level = n.bit_length() - 1
        if level > _MAX_LEVEL or id_ % n:
            raise ValueError(f"node {id_}+{n}: id must be a multiple of n" + self._root_hint())
        if id_ + n - 1 >= len(self._msgs):
            raise ValueError(
                f"pando: node {id_}+{n}: no such node; the chat has {len(self._msgs)} messages"
                + self._root_hint()
            )
        if level == 0:
            return self.message(id_).text
        idx = id_ // n
        return self._line(level - 1, 2 * idx) + self._line(level - 1, 2 * idx + 1)

    def zoom(self, id: int, n: int, offset: int = 0, size: int = 0) -> str:
        """Open a node (``n`` = 1 is the message word for word), by pages."""
        text = self._zoom_text(id, n)
        if offset < 0 or size < 0:
            raise ValueError("pando: offset and size must not be negative")
        if size == 0 or size > ZOOM_PAGE_BYTES:
            size = ZOOM_PAGE_BYTES
        raw = text.encode("utf-8")
        if offset == 0 and len(raw) <= size:
            return text
        offset = min(offset, len(raw))
        start = offset
        while start < len(raw) and (raw[start] & 0xC0) == 0x80:
            start += 1
        if offset != 0 and start >= len(raw):
            raise ValueError(f"pando: the offset is past the end: the node has {len(raw)} bytes")
        end = start + size
        if end >= len(raw):
            end = len(raw)
        else:
            while end > start and (raw[end] & 0xC0) == 0x80:
                end -= 1
            if end == start:
                end = start + 1
                while end < len(raw) and (raw[end] & 0xC0) == 0x80:
                    end += 1
        page = raw[start:end].decode("utf-8", errors="replace")
        tail = "End." if end >= len(raw) else f"More: offset {end}"
        return f"{page}\n[bytes {start}-{end} of {len(raw)}. {tail}]"

    def view_lines(self) -> list[tuple[str, str]]:
        """The saved view as ``(node name, one-line text)``, oldest first."""
        return [
            (f"{i << lv}+{1 << lv}", _flat(self._nodes.get((lv, i), UNBUILT)))
            for lv, i in self._view
        ]

    def recall(self, query: str, k: int = 8):
        from opendaisugi.pando.recall import recall

        return recall(self, query, k)

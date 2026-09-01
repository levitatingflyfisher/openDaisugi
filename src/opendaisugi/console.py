"""The one output layer: results on stdout, everything else on stderr.

Rules (clig.dev, applied): color and box drawing only on a real terminal, off
under --plain, NO_COLOR, or TERM=dumb; -q silences progress notes but never
results; a task longer than a blink says so on a TTY and stays silent when piped.
"""

from __future__ import annotations

import os
import sys
import time
from collections.abc import Iterator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass
from typing import TextIO

_COLORS = {
    "red": "31",
    "green": "32",
    "yellow": "33",
    "blue": "34",
    "magenta": "35",
    "cyan": "36",
    "dim": "2",
    "bold": "1",
}


@dataclass(frozen=True)
class OutputMode:
    color: bool
    plain: bool
    quiet: bool
    verbose: bool
    json: bool = False


BOX = {
    "h": "─",
    "v": "│",
    "tl": "┌",
    "tr": "┐",
    "bl": "└",
    "br": "┘",
    "t": "┬",
    "b": "┴",
    "l": "├",
    "r": "┤",
    "x": "┼",
    "on": "●",
    "avail": "○",
    "off": "·",
    "ok": "✓",
    "no": "✗",
    "flow": "▸",
    "down": "▼",
}
ASCII_BOX = {
    "h": "-",
    "v": "|",
    "tl": "+",
    "tr": "+",
    "bl": "+",
    "br": "+",
    "t": "+",
    "b": "+",
    "l": "+",
    "r": "+",
    "x": "+",
    "on": "*",
    "avail": "o",
    "off": ".",
    "ok": "ok",
    "no": "X",
    "flow": ">",
    "down": "v",
}


def resolve_output(
    *,
    plain: bool = False,
    quiet: bool = False,
    verbose: bool = False,
    no_color: bool = False,
    json: bool = False,
    stream: TextIO | None = None,
    env: Mapping[str, str] | None = None,
) -> OutputMode:
    stream = sys.stdout if stream is None else stream
    env = os.environ if env is None else env
    try:
        tty = bool(stream.isatty())
    except (AttributeError, ValueError):
        tty = False
    color = tty and not plain and not no_color and not json
    if env.get("NO_COLOR") or env.get("TERM") == "dumb":
        color = False
    return OutputMode(color=color, plain=plain or not tty, quiet=quiet, verbose=verbose, json=json)


_MODE = resolve_output()
_ERR: TextIO | None = None


def set_mode(mode: OutputMode, *, err_stream: TextIO | None = None) -> None:
    global _MODE, _ERR
    _MODE = mode
    _ERR = err_stream


def current() -> OutputMode:
    return _MODE


def glyphs(mode: OutputMode | None = None) -> dict[str, str]:
    mode = _MODE if mode is None else mode
    return ASCII_BOX if mode.plain else BOX


def _err() -> TextIO:
    return _ERR if _ERR is not None else sys.stderr


def say(text: str) -> None:
    """A result. Always printed, always stdout."""
    sys.stdout.write(text + "\n")
    sys.stdout.flush()


def note(text: str) -> None:
    """Progress or context. stderr; silent under -q."""
    if _MODE.quiet:
        return
    e = _err()
    e.write(text + "\n")
    e.flush()


def warn(text: str) -> None:
    e = _err()
    e.write(style(text, "yellow") + "\n")
    e.flush()


def style(text: str, color: str) -> str:
    if not _MODE.color:
        return text
    code = _COLORS.get(color)
    return f"\x1b[{code}m{text}\x1b[0m" if code else text


@contextmanager
def step(label: str, *, min_s: float = 0.1) -> Iterator[None]:
    """`label…` on a TTY, then ` done (1.2 s)`. Nothing when piped or quiet.

    The label prints immediately so the user is never looking at a silent
    prompt; the tail prints only when the step took longer than ``min_s``.
    """
    e = _err()
    live = not _MODE.quiet and not _MODE.plain
    t0 = time.monotonic()
    if live:
        e.write(f"{label}…")
        e.flush()
    try:
        yield
    finally:
        if live:
            dt = time.monotonic() - t0
            e.write(f" done ({dt:.1f} s)\n" if dt >= min_s else "\n")
            e.flush()

"""Where openDaisugi keeps its data when no flag names a directory.

The rule, the same in Python, Go (clients/go/internal/datahome), Rust
(clients/rust datahome.rs) and the pi and OpenCode extensions:

1. ``$OPENDAISUGI_HOME`` when it is usable;
2. else ``$XDG_DATA_HOME/opendaisugi`` when XDG_DATA_HOME is usable and
   ``~/.opendaisugi`` does not exist;
3. else ``~/.opendaisugi``.

A value is usable when, after a leading ``~`` or ``~/`` is replaced by the
home directory, it is an absolute path. Any other value (empty, relative,
``~user``) is ignored, as the XDG spec says of a relative XDG path: a
relative data home would move with the cwd and could put the gate's own
state inside an agent's workspace.

An existing install keeps its directory: once ``~/.opendaisugi`` exists,
XDG_DATA_HOME does not move it. The gate guards every directory the rule
can pick (``guarded_data_dirs``), with no exists check, so a move of the
data never leaves the gate's own state unguarded.

coppice keeps its own rule (harness/coppice/internal/datahome and
harness/coppice-rs datahome.rs). It never asks whether ``~/.opendaisugi``
exists, because ``daisugi install --gate`` creates that directory and the
same box would then use two places, depending on the order of two commands.
``coppice_data_home`` is that rule, for the code here that must find
coppice's socket. The gap behind it: ``~/.local/share/opendaisugi`` is in no
guard set when XDG_DATA_HOME is unset, so XDG is chosen only when the
variable is set.

Stdlib only: gate_client imports it on the hook's hot path.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from pathlib import Path

LEGACY_NAME = ".opendaisugi"


def _home(home: Path | str | None) -> Path:
    return Path.home() if home is None else Path(home)


def usable(value: str | None, home: Path) -> Path | None:
    """``value`` with a leading ``~`` or ``~/`` expanded to ``home``, when
    the result is an absolute path; else None."""
    if not value:
        return None
    h = str(home)
    if value == "~":
        value = h
    elif value.startswith("~/"):
        value = h + value[1:]
    return Path(value) if value.startswith("/") else None


def data_home(env: Mapping[str, str] | None = None, home: Path | str | None = None) -> Path:
    """The data directory for ``env`` (default: this process's) and
    ``home`` (default: Path.home())."""
    env = os.environ if env is None else env
    h = _home(home)
    d = usable(env.get("OPENDAISUGI_HOME"), h)
    if d is not None:
        return d
    dot = h / LEGACY_NAME
    x = usable(env.get("XDG_DATA_HOME"), h)
    if x is not None and not dot.exists():
        return x / "opendaisugi"
    return dot


def guarded_data_dirs(
    env: Mapping[str, str] | None = None, home: Path | str | None = None
) -> list[Path]:
    """Every directory ``data_home`` can pick under ``env``, legacy first,
    without duplicates and with no exists check. The gate guards them all."""
    env = os.environ if env is None else env
    h = _home(home)
    out = [h / LEGACY_NAME]
    d = usable(env.get("OPENDAISUGI_HOME"), h)
    if d is not None:
        out.append(d)
    x = usable(env.get("XDG_DATA_HOME"), h)
    if x is not None:
        out.append(x / "opendaisugi")
    seen: list[Path] = []
    for p in out:
        if p not in seen:
            seen.append(p)
    return seen


def _has(p: Path) -> bool:
    """A file, or a directory with at least one entry. Links are followed."""
    try:
        if not p.is_dir():
            return p.exists()
        return any(p.iterdir())
    except OSError:
        return False


def coppice_data_home(env: Mapping[str, str] | None = None, home: Path | str | None = None) -> Path:
    """The rule coppice uses for its data directory (F11).

    ``$OPENDAISUGI_HOME``, else ``$XDG_DATA_HOME/opendaisugi`` when that
    variable is usable, else ``~/.opendaisugi``. One exception, as in
    coppice: when XDG was chosen, an old ``~/.opendaisugi/coppice`` that holds
    data is kept while the XDG coppice directory holds none.
    """
    env = os.environ if env is None else env
    h = _home(home)
    d = usable(env.get("OPENDAISUGI_HOME"), h)
    if d is not None:
        return d
    dot = h / LEGACY_NAME
    x = usable(env.get("XDG_DATA_HOME"), h)
    if x is None:
        return dot
    xdg = x / "opendaisugi"
    if not _has(xdg / "coppice") and _has(dot / "coppice"):
        return dot
    return xdg

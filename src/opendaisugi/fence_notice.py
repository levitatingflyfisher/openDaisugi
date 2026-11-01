"""The one-line notice for agent work that runs without a fence.

The warden checks commands and tool calls. Only a fence stops a file
access (ADR-0028). When the warden is on and coppice's config turns the
fence off, `daisugi warden status`, `daisugi status` and `daisugi start`
say so in one plain line.

The check reads no kernel state, so the same files give the same answer on
every box. A kernel without Landlock under ``fence = "auto"`` is coppice's
to report on its floor: it is the one that knows what the kernel answers.
The Go and Rust ports hold the same rule (``fencenotice.go``,
``fencenotice.rs``).
"""

from __future__ import annotations

import os
import re
from pathlib import Path

LINE = (
    "Files are not confined: the warden checks commands, but only a fence stops "
    'a file access. Turn the fence on with fence = "auto" in coppice.toml.'
)

_FENCE_OFF = re.compile(r"""^\s*fence\s*=\s*(?:"off"|'off')\s*(?:#.*)?$""")
_TABLE = re.compile(r"^\s*\[")


def coppice_config_path(env: dict[str, str] | None = None, home: Path | None = None) -> Path:
    """Where coppice reads its config: $XDG_CONFIG_HOME/coppice/coppice.toml,
    else ~/.config/coppice/coppice.toml."""
    e = os.environ if env is None else env
    base = e.get("XDG_CONFIG_HOME", "")
    if not base:
        base = str((home if home is not None else Path.home()) / ".config")
    return Path(base) / "coppice" / "coppice.toml"


def config_turns_fence_off(path: Path) -> bool:
    """True when the top level of coppice.toml says ``fence = "off"``. A
    missing or unreadable file, any other value, or a key under a table does
    not count."""
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return False
    for line in text.splitlines():
        if _TABLE.match(line):
            return False
        if _FENCE_OFF.match(line):
            return True
    return False


def warden_is_on(root: Path) -> bool:
    """True when the root holds envelopes and no DISARMED marker (coppice
    reads the warden the same way)."""
    if not (root / "envelopes").exists():
        return False
    return not os.path.lexists(root / "DISARMED")


def unconfined(root: Path, env: dict[str, str] | None = None, home: Path | None = None) -> bool:
    """True when the warden at root is on and coppice's config turns the
    fence off."""
    return warden_is_on(root) and config_turns_fence_off(coppice_config_path(env, home))

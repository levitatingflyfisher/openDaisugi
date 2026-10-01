"""DAISUGI_PORT (ruling PK-R-15): which port's daisugi writes the hooks.

A package installs the three ports beside each other as ``daisugi`` (Go),
``daisugi-rs`` and ``daisugi-py``. ``install`` in any of them hands the
whole command to the one the variable names, once, at install time. The
hook it writes then runs that port's program directly, so the gate's hot
path has no extra exec.
"""

from __future__ import annotations

import os
import shutil
import sys
from collections.abc import Mapping

PORT_ENV = "DAISUGI_PORT"
# Set on the one hand-over, so a program that turns out to be another
# port stops instead of handing over again.
PORT_HOP_ENV = "DAISUGI_PORT_HOP"
PORT_BINARIES = {"go": "daisugi", "rust": "daisugi-rs", "python": "daisugi-py"}
OWN_PORT = "python"
_ROOT_FLAGS = ("--plain", "-q", "--quiet", "-v", "--verbose", "--no-color")


class PortError(Exception):
    """One line, said with exit 2; nothing was changed."""


def self_path(arg0: str, path: str) -> str:
    """The path this program was run by, made absolute with its symlinks
    kept, as the Go and Rust ports find theirs."""
    if "/" in arg0:
        return os.path.abspath(arg0)
    found = shutil.which(arg0, path=path)
    return os.path.abspath(found) if found else os.path.abspath(sys.executable)


def port_hop(own: str, self: str, env: Mapping[str, str]) -> str | None:
    """None for this program, or the path of the sibling DAISUGI_PORT names."""
    want = env.get(PORT_ENV, "")
    if not want or want == own:
        return None
    name = PORT_BINARIES.get(want)
    if name is None:
        raise PortError(f"{PORT_ENV} must be go, rust or python, not {want}. Nothing was changed.")
    if env.get(PORT_HOP_ENV, ""):
        raise PortError(
            f"{PORT_ENV} is {want}, but the daisugi it ran is the {own} port. Nothing was changed."
        )
    target = os.path.join(os.path.dirname(self), name)
    if not (os.path.isfile(target) and os.access(target, os.X_OK)):
        raise PortError(
            f"{PORT_ENV} is {want}, but there is no {name} beside {self}. "
            f"Install it, or unset {PORT_ENV}. Nothing was changed."
        )
    return target


def install_argv(argv: list[str]) -> list[str] | None:
    """The root flags and the words after ``install`` in a command line
    (argv[0] dropped), or None when the command is not install."""
    i = 1
    flags: list[str] = []
    while i < len(argv) and argv[i] in _ROOT_FLAGS:
        if argv[i] in ("-q", "--quiet"):
            flags.append("-q")
        elif argv[i] == "--plain":
            flags.append("--plain")
        i += 1
    if i >= len(argv) or argv[i] != "install":
        return None
    return [*flags, "install", *argv[i + 1 :]]


def hand_over(argv: list[str], env: Mapping[str, str], execve=os.execve) -> None:
    """Run the port DAISUGI_PORT names in place of this process, when it
    is not this one. Returns when there is no hand-over."""
    words = install_argv(argv)
    if words is None:
        return
    me = self_path(argv[0], env.get("PATH", ""))
    try:
        target = port_hop(OWN_PORT, me, env)
    except PortError as exc:
        print(f"daisugi: {exc}", file=sys.stderr)
        raise SystemExit(2) from None
    if target is None:
        return
    sys.stdout.flush()
    sys.stderr.flush()
    try:
        execve(target, [target, *words], {**env, PORT_HOP_ENV: "1"})
    except OSError as exc:
        print(f"daisugi: cannot run {target}: {exc}", file=sys.stderr)
        raise SystemExit(1) from None

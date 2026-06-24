"""Shell-interpreter payload extraction for v0.14 semantic recursion.

When a command head is a tractable interpreter (shell ``-c``, ``xargs``,
``find -exec``, ``env``), the "real" command lives inside the
interpreter's arguments. We parse those arguments with ``shlex`` to
extract the embedded shell command(s) so higher layers can recurse.

Opaque interpreters (``python -c``, ``perl -e``, ``ruby -e``, ``node -e``,
``awk``, ``sed``, ``make``) interpret their own language, not shell. We
identify them but don't try to parse the payload — the caller decides
whether to warn, fail, or let them pass based on
``Envelope.shell_interpreter_policy``.
"""

from __future__ import annotations

import shlex
from dataclasses import dataclass, field
from typing import Final

from opendaisugi.models import SHELL_INTERPRETERS

_SHELL_C_INTERPRETERS: Final[frozenset[str]] = frozenset({
    "sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh",
})

_OPAQUE_INTERPRETERS: Final[frozenset[str]] = frozenset({
    "python", "python3", "python2",
    "perl", "ruby", "node", "deno",
    "awk", "gawk", "sed",
    "make",
    "eval", "exec", "source",
})

_XARGS_VALUE_FLAGS: Final[frozenset[str]] = frozenset({
    "-n", "-I", "-P", "-L", "-d", "-E", "-s",
    "--max-args", "--replace", "--max-procs", "--max-lines",
    "--delimiter", "--eof", "--max-chars",
})

_FIND_EXEC_FLAGS: Final[frozenset[str]] = frozenset({
    "-exec", "-execdir", "-ok", "-okdir",
})


@dataclass(frozen=True)
class InterpreterPayload:
    """Result of parsing an interpreter command.

    - ``head``: the interpreter name (e.g. ``"sh"``, ``"xargs"``).
    - ``inner_commands``: embedded shell commands recoverable from the
      invocation. Empty for opaque interpreters or benign invocations
      (``sh script.sh`` with no ``-c``, ``env`` with only VAR=val); one
      entry for ``sh -c "..."`` / ``xargs CMD`` / ``env CMD``; multiple
      for ``find ... -exec CMD ; ... -exec CMD2 ;``.
    - ``opaque``: the interpreter runs non-shell code — recursion
      impossible. Callers apply ``shell_interpreter_policy``.
    """

    head: str
    inner_commands: list[str] = field(default_factory=list)
    opaque: bool = False


def parse_interpreter(command: str) -> InterpreterPayload | None:
    """Parse a shell command and return its interpreter payload if any.

    Returns ``None`` when the head is not an interpreter (or when
    ``shlex`` fails to tokenize — e.g. unbalanced quotes). The caller
    should treat ``None`` as "not an interpreter, apply the normal
    allowlist check only".
    """
    stripped = command.strip()
    if not stripped:
        return None
    try:
        tokens = shlex.split(stripped, posix=True)
    except ValueError:
        return None
    if not tokens:
        return None
    head = tokens[0]
    if head not in SHELL_INTERPRETERS:
        return None
    if head in _OPAQUE_INTERPRETERS:
        return InterpreterPayload(head=head, opaque=True)
    if head in _SHELL_C_INTERPRETERS:
        return _parse_shell_c(head, tokens)
    if head == "xargs":
        return _parse_xargs(head, tokens)
    if head == "find":
        return _parse_find(head, tokens)
    if head == "env":
        return _parse_env(head, tokens)
    return InterpreterPayload(head=head, opaque=True)


def _parse_shell_c(head: str, tokens: list[str]) -> InterpreterPayload:
    """``sh -c "SCRIPT"`` — SCRIPT is another shell command.

    ``sh script.sh`` (no ``-c``) executes a script file; not a bypass
    vector because verify reasons about the command string we see, and
    the file's contents aren't in the plan. Return empty inners.
    """
    for i in range(1, len(tokens) - 1):
        if tokens[i] == "-c":
            return InterpreterPayload(head=head, inner_commands=[tokens[i + 1]])
    return InterpreterPayload(head=head)


def _parse_xargs(head: str, tokens: list[str]) -> InterpreterPayload:
    """``xargs [FLAGS] CMD [ARGS]`` — first non-flag token is the command."""
    i = 1
    while i < len(tokens):
        t = tokens[i]
        if t == "--":
            i += 1
            break
        if t.startswith("-"):
            if t in _XARGS_VALUE_FLAGS and i + 1 < len(tokens):
                i += 2
                continue
            i += 1
            continue
        break
    if i < len(tokens):
        inner = " ".join(shlex.quote(t) for t in tokens[i:])
        return InterpreterPayload(head=head, inner_commands=[inner])
    return InterpreterPayload(head=head)


def _parse_find(head: str, tokens: list[str]) -> InterpreterPayload:
    """``find ... -exec CMD [ARGS] ;`` or ``... -exec CMD [ARGS] +``.

    ``find`` can invoke multiple commands via repeated ``-exec`` clauses.
    We extract all of them.
    """
    inners: list[str] = []
    i = 0
    while i < len(tokens):
        if tokens[i] in _FIND_EXEC_FLAGS:
            start = i + 1
            j = start
            while j < len(tokens) and tokens[j] not in {";", "+"}:
                j += 1
            if j > start:
                inner = " ".join(shlex.quote(t) for t in tokens[start:j])
                inners.append(inner)
            i = j + 1
        else:
            i += 1
    return InterpreterPayload(head=head, inner_commands=inners)


def _parse_env(head: str, tokens: list[str]) -> InterpreterPayload:
    """``env [-i] [NAME=VALUE ...] [CMD [ARGS]]`` — CMD is another
    command to run with the adjusted environment."""
    i = 1
    while i < len(tokens):
        t = tokens[i]
        if t.startswith("-"):
            i += 1
            continue
        if "=" in t and not t.startswith("="):
            i += 1
            continue
        break
    if i < len(tokens):
        inner = " ".join(shlex.quote(x) for x in tokens[i:])
        return InterpreterPayload(head=head, inner_commands=[inner])
    return InterpreterPayload(head=head)

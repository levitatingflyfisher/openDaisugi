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

_SHELL_C_INTERPRETERS: Final[frozenset[str]] = frozenset(
    {
        "sh",
        "bash",
        "zsh",
        "dash",
        "ksh",
        "fish",
        "csh",
        "tcsh",
    }
)

_OPAQUE_INTERPRETERS: Final[frozenset[str]] = frozenset(
    {
        "python",
        "python3",
        "python2",
        "perl",
        "ruby",
        "node",
        "deno",
        "awk",
        "gawk",
        "sed",
        "make",
        "eval",
        "exec",
        "source",
        # ADR-0014: privilege/re-execution wrappers stay opaque — sudo's -i/-s
        # open shells and watch re-runs its payload; a transparent parse that
        # got their flag grammar subtly wrong would under-verify. Strict policy
        # rejects them; that is the fail-closed direction.
        "sudo",
        "doas",
        "watch",
        # Wrappers whose inner command nothing here unwraps. Strict policy
        # rejects them and the placer cannot read the command they run.
        "chroot",
        "flock",
        "busybox",
        "script",
        "parallel",
        "coproc",
        "trap",
        # Heads that run the command they are given and nothing unwraps.
        "systemd-run",
        "screen",
        "unbuffer",
        "expect",
        "runuser",
        "taskset",
        "chrt",
        "prlimit",
        "unshare",
        "nsenter",
        "entr",
        "batch",
    }
)

# Long options, as GNU getopt_long reads them: an exact name wins, otherwise
# a unique prefix selects the option, and an ambiguous or unknown name is not
# an option the parsers can follow. Kinds: "n" takes no value, "v" takes a
# value (``--name=V`` or the next word), "o" takes a value only as ``--name=V``.
_ENV_LONG: Final[dict[str, str]] = {
    "--ignore-environment": "n",
    "--null": "n",
    "--unset": "v",
    "--chdir": "v",
    "--split-string": "v",
    "--argv0": "v",
    "--block-signal": "o",
    "--default-signal": "o",
    "--ignore-signal": "o",
    "--list-signal-handling": "n",
    "--debug": "n",
    "--help": "n",
    "--version": "n",
}

_XARGS_LONG: Final[dict[str, str]] = {
    "--null": "n",
    "--arg-file": "v",
    "--delimiter": "v",
    "--eof": "o",
    "--replace": "o",
    "--max-lines": "o",
    "--max-args": "v",
    "--open-tty": "n",
    "--max-procs": "v",
    "--interactive": "n",
    "--process-slot-var": "v",
    "--no-run-if-empty": "n",
    "--max-chars": "v",
    "--show-limits": "n",
    "--verbose": "n",
    "--exit": "n",
    "--help": "n",
    "--version": "n",
}

_HELP_VERSION: Final[dict[str, str]] = {"--help": "n", "--version": "n"}

# ADR-0014 transparent wrappers: ``head [FLAGS] [POSITIONALS] CMD [ARGS]``
# runs CMD with an adjusted execution context (a timeout, a niceness, a
# detached tty, …). Each entry is (short letters that take a value, short
# letters that take a value only attached, long options, count of non-flag
# positionals before CMD — timeout's DURATION). The options are those of the
# tool's man page (GNU coreutils 9.7, util-linux ionice and setsid, GNU time).
# Before these were recognized, allowlisting ``timeout`` ran
# ``timeout 30 <anything>`` with <anything> completely unverified.
_TRANSPARENT_WRAPPERS: Final[dict[str, tuple[str, str, dict[str, str], int]]] = {
    "timeout": (
        "ks",
        "",
        {
            "--foreground": "n",
            "--kill-after": "v",
            "--preserve-status": "n",
            "--signal": "v",
            "--verbose": "n",
            **_HELP_VERSION,
        },
        1,
    ),
    "nice": ("n", "", {"--adjustment": "v", **_HELP_VERSION}, 0),
    "nohup": ("", "", dict(_HELP_VERSION), 0),
    # GNU time: -o FILE writes FILE, -f FORMAT takes a format.
    "time": (
        "fo",
        "",
        {
            "--append": "n",
            "--format": "v",
            "--output": "v",
            "--portability": "n",
            "--quiet": "n",
            "--verbose": "n",
            **_HELP_VERSION,
        },
        0,
    ),
    "stdbuf": ("ioe", "", {"--input": "v", "--output": "v", "--error": "v", **_HELP_VERSION}, 0),
    "command": ("", "", {}, 0),
    "setsid": (
        "",
        "",
        {"--ctty": "n", "--fork": "n", "--wait": "n", **_HELP_VERSION},
        0,
    ),
    # util-linux ionice: -c, -n, -p, -P and -u take values; -t takes none.
    "ionice": (
        "cnpPu",
        "",
        {
            "--class": "v",
            "--classdata": "v",
            "--pid": "v",
            "--pgid": "v",
            "--ignore": "n",
            "--uid": "v",
            **_HELP_VERSION,
        },
        0,
    ),
}

# xargs short options: n I P L d E s a take a value; e i l take one only
# attached (``-eEOF``, ``-iR``, ``-l2``).
_XARGS_SHORT_VALUE: Final[str] = "nIPLdEsa"
_XARGS_SHORT_OPTIONAL: Final[str] = "eil"

# Heads whose payload is opaque when an option cannot be followed: the
# placer cannot read the command they run.
OPTION_HEADS: Final[frozenset[str]] = frozenset({"env", "xargs", *_TRANSPARENT_WRAPPERS})


def resolve_long(arg: str, table: dict[str, str]) -> tuple[str, str | None, str] | None:
    """The long option ``arg`` (``--name`` or ``--name=VALUE``) selects, as
    GNU getopt_long reads it: ``(full name, inline value or None, kind)``.

    An exact name wins; otherwise a unique prefix selects the option. An
    ambiguous name, an unknown name, or a value given to an option that takes
    none returns None."""
    name, eq, value = arg.partition("=")
    if name in table:
        full = name
    else:
        found = [k for k in table if k.startswith(name)]
        if len(name) <= 2 or len(found) != 1:
            return None
        full = found[0]
    kind = table[full]
    if eq and kind == "n":
        return None
    return full, (value if eq else None), kind


def scan_options(
    tokens: list[str],
    start: int,
    short_value: str,
    short_optional: str,
    long: dict[str, str],
) -> tuple[int, list[tuple[str, str | None]]] | None:
    """Read the leading options of ``tokens[start:]``.

    Returns ``(index of the first word that is not an option, the options as
    (full name, value))``, or None when a long option is ambiguous or unknown.
    ``--`` ends the options and is consumed. A short bundle is read letter by
    letter: a letter that takes a value takes the rest of the word, or the
    next word."""
    n = len(tokens)
    i = start
    opts: list[tuple[str, str | None]] = []
    while i < n:
        t = tokens[i]
        if t == "--":
            i += 1
            break
        if t.startswith("--"):
            got = resolve_long(t, long)
            if got is None:
                return None
            name, value, kind = got
            i += 1
            if kind == "v" and value is None and i < n:
                value = tokens[i]
                i += 1
            opts.append((name, value))
            continue
        if t.startswith("-") and t != "-":
            i += 1
            for j in range(1, len(t)):
                c = t[j]
                rest = t[j + 1 :]
                if c in short_value:
                    if rest:
                        opts.append(("-" + c, rest))
                    elif i < n:
                        opts.append(("-" + c, tokens[i]))
                        i += 1
                    else:
                        opts.append(("-" + c, None))
                    break
                if c in short_optional:
                    opts.append(("-" + c, rest or None))
                    break
                opts.append(("-" + c, None))
            continue
        break
    return i, opts


def xargs_options(tokens: list[str], start: int) -> tuple[int, list[tuple[str, str | None]]] | None:
    return scan_options(tokens, start, _XARGS_SHORT_VALUE, _XARGS_SHORT_OPTIONAL, _XARGS_LONG)


def wrapper_options(
    head: str, tokens: list[str], start: int
) -> tuple[int, list[tuple[str, str | None]]] | None:
    short_value, short_optional, long, _skip = _TRANSPARENT_WRAPPERS[head]
    return scan_options(tokens, start, short_value, short_optional, long)


_FIND_EXEC_FLAGS: Final[frozenset[str]] = frozenset(
    {
        "-exec",
        "-execdir",
        "-ok",
        "-okdir",
    }
)


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
    inner_kinds: tuple[str, ...] = ()


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
    if head.startswith("/"):
        # An absolute path names the program: /usr/bin/env is env. A relative
        # path (./timeout, bin/env) is the project's own file and is not.
        head = head.rsplit("/", 1)[-1]
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
    if head in _TRANSPARENT_WRAPPERS:
        return _parse_wrapper(head, tokens)
    return InterpreterPayload(head=head, opaque=True)


def _parse_shell_c(head: str, tokens: list[str]) -> InterpreterPayload:
    """``sh -c "SCRIPT"`` — SCRIPT is another shell command.

    Also handles CLUSTERED short flags (``sh -ec``, ``bash -lc``, ``-euxc``) and
    the attached-argument form (``sh -cSCRIPT``). POSIX shells accept these, so an
    exact ``== "-c"`` match let ``sh -ec "curl evil"`` slip past verification (the
    embedded command was never extracted or re-checked). Over-detection here is
    the safe direction: a spurious inner command just gets re-verified.

    ``sh script.sh`` (no ``-c``) executes a script file; not a bypass vector
    because verify reasons about the command string we see, and the file's
    contents aren't in the plan. Return empty inners.
    """
    for i in range(1, len(tokens)):
        tok = tokens[i]
        # A single-dash short-flag cluster (not a long ``--`` option) containing 'c'.
        if len(tok) < 2 or tok[0] != "-" or tok[1] == "-" or "c" not in tok:
            continue
        cluster = tok[1:]
        cpos = cluster.find("c")
        # Chars before 'c' must look like clustered short flags (letters).
        if cluster[:cpos] and not cluster[:cpos].isalpha():
            continue
        attached = cluster[cpos + 1 :]
        if attached:  # ``-cSCRIPT`` / ``-ecSCRIPT`` — command attached to the token
            return InterpreterPayload(head=head, inner_commands=[attached])
        if i + 1 < len(tokens):  # ``-c SCRIPT`` / ``-ec SCRIPT`` — next token
            return InterpreterPayload(head=head, inner_commands=[tokens[i + 1]])
        return InterpreterPayload(head=head)
    return InterpreterPayload(head=head)


def _parse_wrapper(head: str, tokens: list[str]) -> InterpreterPayload:
    """``timeout [FLAGS] DURATION CMD…`` / ``nice [FLAGS] CMD…`` and friends.

    Skips the wrapper's own options (see ``scan_options``) and its leading
    positionals, then treats everything left as the wrapped command. No
    remainder — a bare ``timeout 30`` — yields no inner command, which the
    caller treats as a benign invocation (allowlist check on the head only).
    An option that cannot be matched makes the payload opaque.
    """
    scanned = wrapper_options(head, tokens, 1)
    if scanned is None:
        return InterpreterPayload(head=head, opaque=True)
    i = scanned[0]
    positional_skip = _TRANSPARENT_WRAPPERS[head][3]
    i += min(positional_skip, max(0, len(tokens) - i))
    if i < len(tokens):
        inner = " ".join(shlex.quote(t) for t in tokens[i:])
        return InterpreterPayload(head=head, inner_commands=[inner])
    return InterpreterPayload(head=head)


def _parse_xargs(head: str, tokens: list[str]) -> InterpreterPayload:
    """``xargs [FLAGS] CMD [ARGS]`` — first non-flag token is the command."""
    scanned = xargs_options(tokens, 1)
    if scanned is None:
        return InterpreterPayload(head=head, opaque=True)
    i = scanned[0]
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
    kinds: list[str] = []
    i = 0
    while i < len(tokens):
        if tokens[i] in _FIND_EXEC_FLAGS:
            flag = tokens[i]
            start = i + 1
            j = start
            while j < len(tokens) and tokens[j] not in {";", "+"}:
                j += 1
            if j > start:
                inner = " ".join(shlex.quote(t) for t in tokens[start:j])
                inners.append(inner)
                kinds.append("execdir" if flag in ("-execdir", "-okdir") else "exec")
            i = j + 1
        else:
            i += 1
    return InterpreterPayload(head=head, inner_commands=inners, inner_kinds=tuple(kinds))


_ENV_VALUE_SHORT = "uCSa"
# What env -S expands or unescapes itself. A string with one of these is not
# split here: it stays one word, the command, and fails the allowlist.
_ENV_S_HARD = ("\\", "$", "#")


def _parse_env(head: str, tokens: list[str]) -> InterpreterPayload:
    """``env [-i] [-u NAME] [-C DIR] [NAME=VALUE ...] [CMD [ARGS]]`` — CMD is
    another command to run with the adjusted environment.

    The options that take a value consume it (``-u NAME`` is not the
    command). ``-S STRING`` is split into words that take its place, so the
    command in it is checked like any command line."""
    toks = list(tokens)
    i = 1
    while i < len(toks):
        t = toks[i]
        if t.startswith("--") and t != "--":
            got = resolve_long(t, _ENV_LONG)
            if got is None:
                return InterpreterPayload(head=head, opaque=True)
            name, value, kind = got
            if name == "--split-string":
                if value is not None:
                    toks[i : i + 1] = _env_split(value)
                elif i + 1 < len(toks):
                    toks[i : i + 2] = _env_split(toks[i + 1])
                else:
                    i += 1
                continue
            i += 2 if kind == "v" and value is None else 1
            continue
        if t.startswith("-") and t != "-":
            skip = 1
            for j, c in enumerate(t[1:], 1):
                if c not in _ENV_VALUE_SHORT:
                    continue
                rest = t[j + 1 :]
                if c == "S":
                    if rest:
                        toks[i : i + 1] = _env_split(rest)
                    elif i + 1 < len(toks):
                        toks[i : i + 2] = _env_split(toks[i + 1])
                    else:
                        i += 1
                    skip = 0
                else:
                    skip = 1 if rest else 2
                break
            i += skip
            continue
        if "=" in t and not t.startswith("="):
            i += 1
            continue
        break
    if i < len(toks):
        inner = " ".join(shlex.quote(x) for x in toks[i:])
        return InterpreterPayload(head=head, inner_commands=[inner])
    return InterpreterPayload(head=head)


def _env_split(value: str) -> list[str]:
    """The words of an ``env -S`` string, or the string as one word when they
    cannot be read exactly."""
    if any(c in value for c in _ENV_S_HARD):
        return [value]
    try:
        return shlex.split(value)
    except ValueError:
        return [value]

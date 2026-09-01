"""The write paths of one plan step: the field ``forall_writes`` reads.

A predicate reaches a step's write paths only through the ``forall_writes``
quantifier (``predicate.py``). They are not part of the step's own record,
so a plan, a journal entry and a conformance case look the same as before.

The write paths of a step:

- a ``file_write`` step: its ``path``;
- a ``shell`` step: every literal write redirect that the shell
  decomposition (``shell_decompose.py``) finds, the same targets the
  permission stage checks against ``file_write``, and every file a known
  writing command writes through its operands (``WRITERS``: ``cp``, ``mv``,
  ``ln``, ``install``, ``tee``, ``sed -i``, ``truncate``, ``dd of=``,
  ``rsync``, ``touch``, ``mkdir``, ``rm``, ``rmdir``, ``unlink``, ``shred``),
  read with that command's own argument rules. The walk goes into the
  payloads of command-taking wrappers (``sh -c``, ``xargs``, ``find -exec``,
  ``env``, ``timeout``), as the permission stage does. The null device and
  the standard streams are not write paths;
- any other step type: none.

The operand rules. For ``cp``, ``ln``, ``install`` and ``rsync`` the
destination is written: the last operand, or the ``-t`` directory. A plan
cannot ask the filesystem whether the destination is a directory, so each
source also gives ``DEST/basename(SOURCE)``, unless ``-T`` says the
destination is not a directory. ``mv`` also changes its sources. For the
other commands every operand is written (``install -d`` too). ``sed``
writes only with ``-i``, and then every file after the script, and the
backup with its suffix. ``dd`` writes its ``of=`` operand. ``rsync``
writes no remote destination, and with ``--remove-source-files`` its local
sources too. A recursive command shows only its top path, so a target such
as ``src/*.py`` does not see ``rm -r src``, while ``src/**`` does.

Each path is normalized with ``posixpath.normpath``, so ``./src/a.py`` and
``lib/../src/a.py`` read as ``src/a.py``.

Unknown write paths: ``step_write_paths`` returns None, and
``forall_writes`` is then false. A shell step has unknown write paths when
its redirects cannot be read (the grammar refuses it, the shell extra is
missing, or wrappers nest too deep), when a writing command carries a flag
outside its closed set, a word the shell would change (``$``, a backtick, a
glob, a brace, ``~user``), or a line ``shlex`` cannot split, when a writing
command runs under ``xargs`` or ``find -exec`` (they add operands the line
does not show), and when a line that runs ``cd``, ``pushd`` or ``popd``
also writes a relative path.

With a ``base`` (an absolute, normalized directory), each relative write
path is resolved against it; a write path that starts with ``~`` is then
unknown.

Not write paths: the work of an interpreter such as ``python``, ``awk``,
``sed`` without ``-i`` or ``sudo``, a command not in ``WRITERS``, and the
writes of MCP tools, skills and delegated agents.
"""

from __future__ import annotations

import posixpath
import shlex
from typing import Any

# The commands whose operands name files they write, and the closed set of
# flags each may carry. ``short`` and ``short_value`` are single letters
# that take no value and that take one; ``short_optional`` takes a value
# only in the same word. ``long``, ``long_value`` and ``long_optional`` are
# the same split for long flags (an optional value only after "="). Any
# other flag makes the write paths unknown. ``rule`` says which operands
# are written (see the module docstring).
WRITERS: dict[str, dict[str, Any]] = {
    "cp": {
        "rule": "dest",
        "short": "adfiHlLnPpRrsTuvx",
        "short_value": "t",
        "long": [
            "--archive",
            "--dereference",
            "--force",
            "--interactive",
            "--link",
            "--no-clobber",
            "--no-dereference",
            "--no-target-directory",
            "--one-file-system",
            "--recursive",
            "--remove-destination",
            "--strip-trailing-slashes",
            "--symbolic-link",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
        "long_optional": ["--preserve", "--reflink", "--update"],
    },
    "mv": {
        "rule": "move",
        "short": "finTuv",
        "short_value": "t",
        "long": [
            "--force",
            "--interactive",
            "--no-clobber",
            "--no-target-directory",
            "--strip-trailing-slashes",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
        "long_optional": ["--update"],
    },
    "ln": {
        "rule": "dest",
        "short": "finLPrsTv",
        "short_value": "t",
        "long": [
            "--force",
            "--interactive",
            "--logical",
            "--no-dereference",
            "--no-target-directory",
            "--physical",
            "--relative",
            "--symbolic",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
    },
    "install": {
        "rule": "install",
        "short": "cCdDpsTv",
        "short_value": "gmot",
        "long": [
            "--compare",
            "--directory",
            "--no-target-directory",
            "--preserve-timestamps",
            "--strip",
            "--verbose",
        ],
        "long_value": ["--group", "--mode", "--owner", "--target-directory"],
    },
    "tee": {
        "rule": "all",
        "short": "aip",
        "long": ["--append", "--ignore-interrupts"],
        "long_optional": ["--output-error"],
    },
    "sed": {
        "rule": "sed",
        "short": "Enrsuz",
        "short_value": "efl",
        "short_optional": "i",
        "long": [
            "--debug",
            "--follow-symlinks",
            "--null-data",
            "--posix",
            "--quiet",
            "--regexp-extended",
            "--sandbox",
            "--separate",
            "--silent",
            "--unbuffered",
        ],
        "long_value": ["--expression", "--file", "--line-length"],
        "long_optional": ["--in-place"],
    },
    "truncate": {
        "rule": "all",
        "short": "co",
        "short_value": "rs",
        "long": ["--io-blocks", "--no-create"],
        "long_value": ["--reference", "--size"],
    },
    "dd": {"rule": "dd"},
    "rsync": {
        "rule": "rsync",
        "short": "0346aAbcCdDEgHhiIkKlLmnoOpPqrRsStuUvWxXyz",
        "short_value": "Bef",
        "long": [
            "--acls",
            "--append",
            "--archive",
            "--backup",
            "--checksum",
            "--compress",
            "--copy-links",
            "--cvs-exclude",
            "--delete",
            "--delete-after",
            "--delete-before",
            "--delete-delay",
            "--delete-during",
            "--delete-excluded",
            "--devices",
            "--dirs",
            "--dry-run",
            "--executability",
            "--existing",
            "--force",
            "--fuzzy",
            "--group",
            "--hard-links",
            "--human-readable",
            "--ignore-existing",
            "--ignore-times",
            "--inplace",
            "--itemize-changes",
            "--links",
            "--list-only",
            "--mkpath",
            "--no-group",
            "--no-owner",
            "--no-perms",
            "--no-relative",
            "--no-times",
            "--no-whole-file",
            "--numeric-ids",
            "--omit-dir-times",
            "--one-file-system",
            "--owner",
            "--partial",
            "--perms",
            "--progress",
            "--protect-args",
            "--prune-empty-dirs",
            "--quiet",
            "--recursive",
            "--relative",
            "--remove-source-files",
            "--safe-links",
            "--size-only",
            "--sparse",
            "--specials",
            "--stats",
            "--times",
            "--update",
            "--verbose",
            "--whole-file",
            "--xattrs",
        ],
        "long_value": [
            "--block-size",
            "--bwlimit",
            "--chmod",
            "--chown",
            "--compress-level",
            "--contimeout",
            "--debug",
            "--exclude",
            "--exclude-from",
            "--files-from",
            "--filter",
            "--include",
            "--include-from",
            "--info",
            "--max-delete",
            "--max-size",
            "--min-size",
            "--modify-window",
            "--out-format",
            "--port",
            "--rsh",
            "--suffix",
            "--timeout",
        ],
    },
    "touch": {
        "rule": "all",
        "short": "acm",
        "short_value": "dtr",
        "long_value": ["--date", "--reference"],
    },
    "mkdir": {
        "rule": "all",
        "short": "pv",
        "short_value": "m",
        "long": ["--parents", "--verbose"],
        "long_value": ["--mode"],
    },
    "rm": {
        "rule": "all",
        "short": "dfiIrRv",
        "long": [
            "--dir",
            "--force",
            "--no-preserve-root",
            "--one-file-system",
            "--recursive",
            "--verbose",
        ],
        "long_optional": ["--interactive", "--preserve-root"],
    },
    "rmdir": {
        "rule": "all",
        "short": "v",
        "long": ["--ignore-fail-on-non-empty", "--verbose"],
    },
    "unlink": {"rule": "all"},
    "shred": {
        "rule": "all",
        "short": "fuvxz",
        "short_value": "ns",
        "long": ["--exact", "--force", "--verbose", "--zero"],
        "long_value": ["--iterations", "--size"],
        "long_optional": ["--remove"],
    },
}

# dd's operands, each KEY=VALUE. Any other word makes the paths unknown.
DD_KEYS = frozenset(
    {
        "bs",
        "cbs",
        "conv",
        "count",
        "ibs",
        "if",
        "iflag",
        "iseek",
        "obs",
        "of",
        "oflag",
        "oseek",
        "seek",
        "skip",
        "status",
    }
)

# The heads after which the line's cwd is not the call's.
CWD_HEADS = frozenset({"cd", "pushd", "popd"})

# The wrappers that add operands the line does not show.
_ADDS_OPERANDS = frozenset({"xargs", "find"})


def step_write_paths(step: dict[str, Any], base: str | None = None) -> list[str] | None:
    """The normalized write paths of one step record, or None when unknown.

    With ``base``, an absolute normalized directory, each relative path is
    resolved against it."""
    kind = step.get("type")
    if kind == "file_write":
        path = step.get("path")
        if not isinstance(path, str):
            return None
        paths: list[str] | None = [posixpath.normpath(path)]
    elif kind == "shell":
        command = step.get("command")
        if not isinstance(command, str):
            return None
        paths = _shell_writes(command, 0, False)
    else:
        return []
    if paths is None or base is None:
        return paths
    return resolve_writes(paths, base)


def resolve_writes(paths: list[str], base: str) -> list[str] | None:
    """Each relative path resolved against ``base``; None when a path starts
    with ``~``, which the shell expands to a home the plan does not name."""
    out: list[str] = []
    for p in paths:
        if p.startswith("/"):
            out.append(p)
        elif p.startswith("~"):
            return None
        else:
            out.append(posixpath.normpath(base + "/" + p))
    return out


def _shell_writes(command: str, depth: int, adds: bool) -> list[str] | None:
    from opendaisugi.verify import (
        _MAX_INTERPRETER_DEPTH,
        _SANCTIONED_WRITE_SINKS,
        _SHELL_METACHAR_RE,
    )

    if depth > _MAX_INTERPRETER_DEPTH:
        return None
    stripped = command.strip()
    if not stripped:
        return []
    if not _SHELL_METACHAR_RE.search(command):
        return _payload_writes(stripped, depth, adds)
    from opendaisugi.shell_decompose import decompose_command

    decomp = decompose_command(command)
    if not decomp.ok:
        return None
    out = [posixpath.normpath(p) for p in decomp.writes if p not in _SANCTIONED_WRITE_SINKS]
    for simple in decomp.commands:
        inner = _payload_writes(simple, depth, adds)
        if inner is None:
            return None
        out.extend(inner)
    if _moves_cwd(decomp.heads) and _has_relative(out):
        return None
    return out


def _moves_cwd(heads: tuple[str, ...]) -> bool:
    for h in heads:
        if h in CWD_HEADS:
            return True
    return False


def _has_relative(paths: list[str]) -> bool:
    for p in paths:
        if not p.startswith("/"):
            return True
    return False


def _payload_writes(command: str, depth: int, adds: bool) -> list[str] | None:
    from opendaisugi.interpreter_parse import parse_interpreter

    payload = parse_interpreter(command)
    if payload is None or payload.opaque:
        return operand_writes(command, adds)
    inner_adds = adds or payload.head in _ADDS_OPERANDS
    out: list[str] = []
    for inner in payload.inner_commands:
        writes = _shell_writes(inner, depth + 1, inner_adds)
        if writes is None:
            return None
        out.extend(writes)
    return out


def operand_writes(command: str, adds: bool) -> list[str] | None:
    """The normalized files one simple command writes through its operands.

    ``adds`` is True under ``xargs`` or ``find -exec``, which add operands
    the line does not show. A command not in ``WRITERS`` writes none."""
    from opendaisugi.effects import _unsafe_words
    from opendaisugi.verify import _ENV_ASSIGN_RE, _SANCTIONED_WRITE_SINKS

    try:
        tokens = shlex.split(command)
    except ValueError:
        return None
    i = 0
    while i < len(tokens) and _ENV_ASSIGN_RE.match(tokens[i]):
        i += 1
    if i == len(tokens):
        return []
    head = tokens[i].rsplit("/", 1)[-1]
    spec = WRITERS.get(head)
    if spec is None:
        return []
    if adds or _unsafe_words(command):
        return None
    raw = _writer_paths(head, spec, tokens[i + 1 :])
    if raw is None:
        return None
    return [posixpath.normpath(p) for p in raw if p not in _SANCTIONED_WRITE_SINKS]


def _parse_flags(
    args: list[str], spec: dict[str, Any]
) -> tuple[list[tuple[str, str | None]], list[str]] | None:
    """The flags (name and value) and the operands of args, or None when a
    flag is outside spec or lacks its value."""
    short = spec.get("short", "")
    short_value = spec.get("short_value", "")
    short_optional = spec.get("short_optional", "")
    long = spec.get("long", [])
    long_value = spec.get("long_value", [])
    long_optional = spec.get("long_optional", [])
    opts: list[tuple[str, str | None]] = []
    ops: list[str] = []
    i = 0
    ended = False
    while i < len(args):
        a = args[i]
        i += 1
        if ended or a == "-" or not a.startswith("-"):
            ops.append(a)
            continue
        if a == "--":
            ended = True
            continue
        if a.startswith("--"):
            name, eq, value = a.partition("=")
            if eq:
                if name in long_value or name in long_optional:
                    opts.append((name, value))
                    continue
                return None
            if name in long or name in long_optional:
                opts.append((name, None))
                continue
            if name in long_value:
                if i >= len(args):
                    return None
                opts.append((name, args[i]))
                i += 1
                continue
            return None
        body = a[1:]
        for j, c in enumerate(body):
            if c in short:
                opts.append(("-" + c, None))
                continue
            if c in short_optional:
                opts.append(("-" + c, body[j + 1 :]))
                break
            if c in short_value:
                rest = body[j + 1 :]
                if rest:
                    opts.append(("-" + c, rest))
                elif i >= len(args):
                    return None
                else:
                    opts.append(("-" + c, args[i]))
                    i += 1
                break
            return None
    return opts, ops


def _basename(path: str) -> str:
    return posixpath.basename(path.rstrip("/"))


def _under(dest: str, sources: list[str]) -> list[str]:
    """dest, then dest/basename(source) for each source that has one."""
    out = [dest]
    for s in sources:
        b = _basename(s)
        if b not in ("", ".", ".."):
            out.append(dest + "/" + b)
    return out


def _is_remote(word: str) -> bool:
    """True for an rsync operand on another host: HOST:PATH or rsync://."""
    if word.startswith("rsync://"):
        return True
    colon = word.find(":")
    return colon > 0 and "/" not in word[:colon]


def _writer_paths(head: str, spec: dict[str, Any], args: list[str]) -> list[str] | None:
    """The raw paths one writing command writes, or None when unknown."""
    rule = spec["rule"]
    if rule == "dd":
        out: list[str] = []
        for a in args:
            key, eq, value = a.partition("=")
            if a.startswith("-") or not eq or key not in DD_KEYS:
                return None
            if key == "of":
                out.append(value)
        return out
    parsed = _parse_flags(args, spec)
    if parsed is None:
        return None
    opts, ops = parsed
    names = [n for n, _ in opts]
    if rule == "all":
        return ops
    if rule == "sed":
        inplace = [v for n, v in opts if n in ("-i", "--in-place")]
        if not inplace:
            return []
        scripted = any(n in ("-e", "--expression", "-f", "--file") for n in names)
        files = ops if scripted else ops[1:]
        suffix = inplace[-1] or ""
        if not suffix:
            return files
        if "*" in suffix or "/" in suffix:
            return None
        return files + [f + suffix for f in files]
    if rule == "install" and ("-d" in names or "--directory" in names):
        return ops
    target = [v or "" for n, v in opts if n in ("-t", "--target-directory")]
    no_target = "-T" in names or "--no-target-directory" in names
    if target and no_target:
        return None
    if target:
        dest: str | None = target[-1]
        sources = ops
    elif head == "ln" and len(ops) == 1:
        return [_basename(ops[0])]
    elif len(ops) < 2:
        return []
    else:
        dest, sources = ops[-1], ops[:-1]
    if rule == "rsync":
        local = [s for s in sources if not _is_remote(s)]
        out = local if "--remove-source-files" in names else []
        if not _is_remote(dest):
            out = out + _under(dest, sources)
        return out
    written = [dest] if no_target else _under(dest, sources)
    if rule == "move":
        return sources + written
    return written


__all__ = [
    "CWD_HEADS",
    "DD_KEYS",
    "WRITERS",
    "operand_writes",
    "resolve_writes",
    "step_write_paths",
]

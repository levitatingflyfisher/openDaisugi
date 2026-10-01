"""The gate's rule that no agent writes the gate's own state.

The gate root holds the disarm marker, the envelopes, the graft rules, the
asks and the audit logs. The data dir holds the files the meters and the
review queue trust. An envelope that grants writes everywhere must not
grant these, or an agent could disarm its own gate, rewrite its envelope,
or label its own task a pass. So the gate denies every write it can place
under them, before any envelope check, in every mode, and no operator ask
turns the deny. Reads stay allowed.

The protected paths (``protected_dirs``): the gate root in force, and under
each data dir its ``gate``, ``router`` (labels, delegations), ``journal``
(traces and their receipts, the index, rank's choices), ``tree`` (the
ledger), ``gateway`` (turns and answers), ``weave`` (run files a resume
reads) and ``envelope_cache.db`` (the envelopes a run reuses). The data
dirs are the default one (~/.opendaisugi) and the gate root's parent when
the gate root is named ``gate``. A gate root with another name, such as the
temporary one an agentic step gets, protects only itself, so its parent
(the temporary directory) is not a data dir. config.yaml has its own rule
(floor_config), which also denies reads.

A write is placed as the config rule places a path: ~, $HOME and the XDG
homes expanded, a relative path joined to the cwd, ``..`` and symlinks
followed, and both the typed and the resolved spelling checked. A write
hits when it lands under a protected path, or when a protected path lies
under it (``rm -r``, ``mv`` or ``chmod -R`` of the data dir or of a
directory above it; so ``cp x ~`` hits too, since cp may replace its
destination).

The writes of a tool call:

- a file_write record (Write, Edit, MultiEdit, NotebookEdit, each
  apply_patch path): its path;
- a shell line: the write paths ``write_paths`` reads (redirects and the
  operands of its 15 writing commands), and the sources of ``ln`` and of
  ``cp`` with a link flag, since a hard link or a symlink made now is a
  door to a later write. The shell is checked only when the line names a
  data dir (or a gate root not named ``gate``): a word placed under it, the
  path as text with a path boundary after it, or a cwd under it. On such a
  line, a write the gate cannot place denies: write paths ``write_paths``
  cannot read (a variable, a glob, a substitution, a cd before a relative
  write), a relative write with no cwd, and any simple command that is not
  one of the writing commands and not a known read-only command
  (``READ_ONLY``), such as ``python``, ``git``, ``tar``, ``chmod`` or
  ``sh -c``. A ``find`` with an action that writes or runs a command is
  not read-only;
- an MCP call: each string argument placed as a path from the cwd, and the
  cwd itself. An MCP tool's reads and writes look the same, so a read of
  the state through MCP is denied too. A directory above the state is not
  a hit here.

Any error in the check is a hit, so a broken check denies.
"""

from __future__ import annotations

import os
import shlex
from pathlib import Path
from typing import Any

from opendaisugi.datahome import guarded_data_dirs

# The paths under a data dir that hold the gate's state.
STATE_PARTS = ("gate", "router", "journal", "tree", "gateway", "weave", "envelope_cache.db")

# Commands that write nothing. A command outside this set and outside
# write_paths.WRITERS may write where the gate cannot see.
READ_ONLY = frozenset(
    {
        ":",
        "[",
        "b2sum",
        "basename",
        "cat",
        "cd",
        "cmp",
        "comm",
        "cut",
        "df",
        "diff",
        "dirname",
        "du",
        "echo",
        "egrep",
        "false",
        "fgrep",
        "find",
        "grep",
        "head",
        "hexdump",
        "jq",
        "ls",
        "md5sum",
        "nl",
        "od",
        "paste",
        "popd",
        "printf",
        "pushd",
        "pwd",
        "readlink",
        "realpath",
        "sha1sum",
        "sha256sum",
        "sha512sum",
        "stat",
        "strings",
        "tail",
        "test",
        "tr",
        "true",
        "wc",
    }
)

# find's words that write a file, delete one, or run a command.
FIND_ACTIONS = frozenset(
    {
        "-delete",
        "-exec",
        "-execdir",
        "-fls",
        "-fprint",
        "-fprint0",
        "-fprintf",
        "-ok",
        "-okdir",
    }
)

# The characters that end a path written as text.
_BOUNDARY = frozenset("/ \t\n\r'\"`;&|()<>:,")


def refusal(directory: str) -> str:
    return f"the gate's own state is under {directory}; only the operator writes there."


def data_dirs(root: Path | None) -> list[str]:
    """The data dirs, as written: the gate root's parent when the gate root
    is named gate, then the default one."""
    from opendaisugi.gate import _gate_root

    gate = _gate_root(root)
    out: list[str] = []
    for d in ([gate.parent] if gate.name == "gate" else []) + guarded_data_dirs():
        s = os.path.normpath(os.path.expanduser(str(d)))
        if s not in out:
            out.append(s)
    return out


def protected_dirs(root: Path | None) -> list[str]:
    """The protected paths, as written: the gate root in force, then each
    data dir's state paths."""
    from opendaisugi.gate import _gate_root

    out = [os.path.normpath(os.path.expanduser(str(_gate_root(root))))]
    for d in data_dirs(root):
        for part in STATE_PARTS:
            p = d + "/" + part
            if p not in out:
                out.append(p)
    return out


def _named_roots(root: Path | None) -> list[str]:
    """The paths a shell line must name for the shell check to run: the
    data dirs, and a gate root that is not named gate."""
    from opendaisugi.gate import _gate_root

    out = data_dirs(root)
    gate = os.path.normpath(os.path.expanduser(str(_gate_root(root))))
    if os.path.basename(gate) != "gate" and gate not in out:
        out.append(gate)
    return out


def _spell(path: str) -> list[str]:
    """A path as written and as the OS resolves it, once each."""
    out = [path]
    real = os.path.realpath(path)
    if real not in out:
        out.append(real)
    return out


def _under(path: str, top: str) -> bool:
    return path == top or path.startswith(top.rstrip("/") + "/")


def _placed(raw: str, cwd: str) -> list[str]:
    """A path's spellings as the config rule places it: as typed when
    absolute, as resolved from cwd, and as joined to cwd."""
    from opendaisugi.effects import _resolve
    from opendaisugi.floor_config import _expand_home

    word = _expand_home(raw)
    out: list[str] = []
    typed = os.path.normpath(os.path.expanduser(word))
    if os.path.isabs(typed):
        out.append(typed)
    base = cwd or os.getcwd()
    resolved = _resolve(word, base)
    if resolved is not None and resolved not in out:
        out.append(resolved)
    joined = os.path.normpath(os.path.join(base, os.path.expanduser(word)))
    if joined not in out:
        out.append(joined)
    return out


def _hit_of(spellings: list[str], entries: list[tuple[str, list[str]]], above: bool) -> str | None:
    """The first protected path a write's spellings land under, or, with
    above, lie above."""
    for name, forms in entries:
        for f in forms:
            for s in spellings:
                if _under(s, f) or (above and _under(f, s)):
                    return name
    return None


def _names_text(text: str, top: str) -> bool:
    """True when text holds top followed by a path boundary or the end."""
    start = 0
    while True:
        i = text.find(top, start)
        if i < 0:
            return False
        j = i + len(top)
        if j == len(text) or text[j] in _BOUNDARY:
            return True
        start = i + 1


def _names(command: str, cwd: str, forms: list[str]) -> bool:
    """True when a shell line names a path by one of its spellings: as
    text, as a placed word or redirect, or by running from under it."""
    from opendaisugi.floor_config import Guard, _expand_home, _shell_hit, path_in_floor

    guard = Guard(lambda: [Path(f) for f in forms])
    expanded = _expand_home(command)
    for f in forms:
        if _names_text(expanded, f):
            return True
    if cwd and path_in_floor(".", cwd, guard):
        return True
    return _shell_hit(command, cwd, guard)[0]


def _link_sources(words: list[str]) -> list[str]:
    """The operands of ln, or of cp with a link flag: each may be the
    other end of a link the line makes."""
    head = words[0].rsplit("/", 1)[-1]
    args = words[1:]
    if head == "cp":
        linked = False
        for w in args:
            if w == "--":
                break
            if w in ("--link", "--symbolic-link"):
                linked = True
            elif w.startswith("-") and not w.startswith("--") and ("l" in w or "s" in w):
                linked = True
        if not linked:
            return []
    elif head != "ln":
        return []
    out: list[str] = []
    flags = True
    for w in args:
        if flags and w == "--":
            flags = False
        elif flags and w.startswith("-") and w != "-":
            continue
        else:
            out.append(w)
    return out


def _shell_scan(command: str) -> tuple[bool, list[str]]:
    """Whether every simple command of the line is a known writer or a
    read-only command, and the link sources it names."""
    from opendaisugi.shell_decompose import decompose_command
    from opendaisugi.verify import _ENV_ASSIGN_RE
    from opendaisugi.write_paths import WRITERS

    dec = decompose_command(command)
    if not dec.ok:
        return False, []
    links: list[str] = []
    for text in dec.commands:
        try:
            words = shlex.split(text)
        except ValueError:
            return False, []
        i = 0
        while i < len(words) and _ENV_ASSIGN_RE.match(words[i]):
            i += 1
        if i == len(words):
            continue
        head = words[i].rsplit("/", 1)[-1]
        if head in WRITERS:
            links.extend(_link_sources(words[i:]))
            continue
        if head not in READ_ONLY:
            return False, []
        if head == "find":
            for w in words[i + 1 :]:
                if w in FIND_ACTIONS:
                    return False, []
    return True, links


def _shell(
    command: str, cwd: str, entries: list[tuple[str, list[str]]], named: list[tuple[str, list[str]]]
) -> str | None:
    from opendaisugi.write_paths import step_write_paths

    hit_root = None
    for name, forms in named:
        if _names(command, cwd, forms):
            hit_root = name
            break
    if hit_root is None:
        return None
    writes = step_write_paths({"type": "shell", "command": command})
    if writes is None:
        return hit_root
    known, links = _shell_scan(command)
    if not known:
        return hit_root
    for w in [*writes, *links]:
        expanded = os.path.expanduser(w)
        if not os.path.isabs(expanded) and not cwd:
            return hit_root
        got = _hit_of(_placed(w, cwd), entries, True)
        if got is not None:
            return got
    return None


def _entries(paths: list[str]) -> list[tuple[str, list[str]]]:
    return [(p, _spell(p)) for p in paths]


def gate_state_hit(
    payload: dict[str, Any], record: dict[str, Any] | None, root: Path | None = None
) -> str | None:
    """The protected path a tool call writes, as written, or None. Any
    error counts as a hit on the gate root, so a broken check denies."""
    from opendaisugi.floor_config import _strings

    try:
        if record is None:
            return None
        cwd = str(payload.get("cwd") or "")
        step = record.get("step_type")
        if step == "file_write":
            entries = _entries(protected_dirs(root))
            return _hit_of(_placed(str(record.get("path") or ""), cwd), entries, True)
        if step == "shell":
            entries = _entries(protected_dirs(root))
            named = _entries(_named_roots(root))
            return _shell(str(record.get("command") or ""), cwd, entries, named)
        if step == "mcp":
            entries = _entries(protected_dirs(root))
            if cwd:
                got = _hit_of(_placed(".", cwd), entries, False)
                if got is not None:
                    return got
            for v in _strings(record.get("arguments")):
                got = _hit_of(_placed(v, cwd or "/"), entries, False)
                if got is not None:
                    return got
        return None
    except Exception:  # noqa: BLE001 - fail closed
        return _first_protected(root)


def _first_protected(root: Path | None) -> str:
    """The gate root as written, for the reason of a check that failed."""
    try:
        return protected_dirs(root)[0]
    except Exception:  # noqa: BLE001
        return str(root)

"""The gate's rule that no search reaches coppice's secrets.

A recursive search reads every file under its root. A root that is an
ancestor of a coppice data directory, such as the home directory or /,
reads every secret the data directory holds. So the gate refuses such a
search with ``SEARCH_REFUSAL``, which names the fix: search a narrower
directory. The pane rule already refuses a search of the data directory
itself and of each directory in it that holds a secret.

A file-read tool call other than Glob and Read is checked on each path
its input names (file_path, path and filePath), and with none on its
cwd. A relative path, or no path, with no absolute cwd is refused. Glob
prints names, never contents, and Read opens one file.

A shell line is checked on each simple command that searches a tree:
rg, ag, ack, rgrep, ugrep, ug and git grep always, grep, egrep, fgrep and
zgrep when any word asks it to recurse (-r, -R, a -d or --directories
value of recurse, a prefix of --recursive or --dereference-recursive, or
ugrep's -NUM or --depth, since ugrep is often installed as grep),
and find with -exec, -execdir, -ok or -okdir. The program counts wherever
it stands in the command, so env, timeout, nice or a path before it does
not hide it, and a word that holds a whole shell line, as for sh -c, is
checked as a line of its own, two levels deep; past that, a line that
still names a search program is refused. Each cd is followed.

The roots of a search are its words that are not options. A word right
after an option that may take a value is placed as a root too, but it
does not count as one: a search that names no root it surely has
searches its cwd, so the cwd is checked as well. The first sure word is
the pattern, unless -e, -f, --regexp, --file or --files gives it. The
gate refuses the search when any root, placed from the cwd the line has
reached:

- is an ancestor of a data directory, typed, normalized or resolved;
- holds a glob (``*``, ``?``, ``[...]``, a ``{a,b}`` brace, or a bash
  sequence brace such as ``{a..e}`` or ``{1..9..2}``) that can match an
  ancestor of a data directory, the data directory, or a path in it that
  leads to a secret. A brace past 64 words, or a zero-padded or mixed
  sequence, is not expanded: it counts as a match unless the plain
  absolute path before it cannot lead to one;
- holds a ``$`` or a backtick, or starts with ``~`` after the gate
  expands the home, so the gate cannot tell where it points;
- is relative, or is the cwd, and the cwd is unknown: the call names no
  absolute cwd, or a cd the gate cannot follow (cd -, a .., a symlink)
  came before it.

A line the shell parser or the word splitter cannot read is refused when
it names one of these programs. Left to the envelope: any other program
that reads a tree, such as cp -r, tar, fd -x, or find piped to xargs; a
search whose root the program reads from its input; an MCP tool's
search; and a root that a symlink made by an earlier call points above
a data directory.
"""

from __future__ import annotations

import os
import re
import shlex
from pathlib import Path
from typing import Any

SEARCH_REFUSAL = "this search reaches coppice's secrets. Search a narrower directory."

_ALWAYS = frozenset({"rg", "ag", "ack", "rgrep", "ugrep", "ug"})
_GREPS = frozenset({"grep", "egrep", "fgrep", "zgrep"})
_FIND_EXEC = frozenset({"-exec", "-execdir", "-ok", "-okdir"})
_ASSIGN = re.compile(r"[A-Za-z_][A-Za-z0-9_]*=")
# Any of the programs above as a word, for a line the gate cannot split.
_PROGRAM_TEXT = re.compile(
    r"(^|[^A-Za-z0-9_.-])(rg|ag|ack|rgrep|ugrep|ug|grep|egrep|fgrep|zgrep|find)($|[^A-Za-z0-9_.-])"
)
_GLOB = re.compile(r"[*?\[{]")
# Short options that take no value, per program. Any other letter may
# take one, so the word after it does not count as a root.
_GREP_FLAGS = frozenset("rRnilLcvwxsqHhoEFGPIazZUbTy")
_RG_FLAGS = frozenset("iIlLcvwxsqHhoFPazUbuSNnp0.")
_OTHER_FLAGS = frozenset("ilLcvwxHhonRr")
# Long options that take no value.
_LONG_FLAGS = frozenset(
    {
        "--recursive",
        "--dereference-recursive",
        "--hidden",
        "--no-ignore",
        "--no-ignore-vcs",
        "--line-number",
        "--ignore-case",
        "--files-with-matches",
        "--files-without-match",
        "--count",
        "--invert-match",
        "--word-regexp",
        "--line-regexp",
        "--fixed-strings",
        "--follow",
        "--no-heading",
        "--with-filename",
        "--no-filename",
        "--files",
        "--unrestricted",
        "--null",
        "--text",
        "--smart-case",
        "--case-sensitive",
        "--no-messages",
        "--quiet",
        "--only-matching",
        "--multiline",
        "--json",
        "--vimgrep",
        "--search-zip",
        "--no-index",
    }
)
# Options that give the pattern, so no sure word is the pattern.
_PATTERN_OPTS = ("-e", "-f", "--regexp", "--file", "--files")
_MAX_DEPTH = 2
_MAX_BRACES = 64


def _flags_for(head: str) -> frozenset[str]:
    if head in _GREPS:
        return _GREP_FLAGS
    if head == "rg":
        return _RG_FLAGS
    return _OTHER_FLAGS


def _takes_value(word: str, flags: frozenset[str]) -> bool:
    """True when the word after this option may be its value."""
    if word.startswith("--"):
        return "=" not in word and word not in _LONG_FLAGS
    for i, c in enumerate(word[1:], start=1):
        if c not in flags:
            return i == len(word) - 1
    return False


def _grep_recurses(words: list[str]) -> bool:
    for w in words:
        if "recurse" in w:
            return True
        if w.startswith("--"):
            name = w.split("=", 1)[0]
            if len(name) > 2 and (
                "--recursive".startswith(name) or "--dereference-recursive".startswith(name)
            ):
                return True
        elif w.startswith("-") and ("r" in w[1:] or "R" in w[1:]):
            return True
        # ugrep, often installed as grep, recurses to a depth given as -NUM
        # or --depth.
        if re.fullmatch(r"-[0-9]+", w) or w.startswith("--depth"):
            return True
    return False


def _gives_pattern(words: list[str]) -> bool:
    for w in words:
        if w == "--":
            return False
        for opt in _PATTERN_OPTS:
            if w == opt or w.startswith(opt + "="):
                return True
        if w.startswith("-") and not w.startswith("--") and ("e" in w[1:] or "f" in w[1:]):
            return True
    return False


def _tool_roots(head: str, args: list[str]) -> tuple[list[str], list[str]]:
    """The words a search tool may search (all of them), and those it
    surely searches, not counting its pattern."""
    flags = _flags_for(head)
    every: list[str] = []
    sure: list[str] = []
    ended = False
    value_next = False
    for w in args:
        if ended:
            every.append(w)
            sure.append(w)
            continue
        if w == "--":
            ended = True
            value_next = False
            continue
        if w.startswith("-") and len(w) > 1:
            value_next = _takes_value(w, flags)
            continue
        every.append(w)
        if not value_next:
            sure.append(w)
        value_next = False
    if sure and not _gives_pattern(args):
        sure = sure[1:]
    if not sure:
        every.append(".")
    return every, sure


def _find_roots(args: list[str]) -> list[str]:
    roots: list[str] = []
    i = 0
    while i < len(args):
        w = args[i]
        if w in ("-H", "-L", "-P", "--") or re.fullmatch(r"-O[0-9]*", w):
            i += 1
            continue
        if w == "-D":
            i += 2
            continue
        if w.startswith(("-", "(", "!")):
            break
        roots.append(w)
        i += 1
    return roots or ["."]


# Programs that run the command after them, so a search may stand there.
_PREFIXES = frozenset(
    {
        "env",
        "sudo",
        "doas",
        "timeout",
        "nice",
        "ionice",
        "stdbuf",
        "time",
        "command",
        "exec",
        "nohup",
        "xargs",
        "setsid",
        "chrt",
        "taskset",
        "strace",
        "busybox",
    }
)


def _search_roots(tokens: list[str]) -> list[str] | None:
    """Every root a simple command searches, or None when it is no search
    this rule knows. The program counts as the first word, or anywhere
    after a program that runs the command after it, such as env or sudo.
    A search fed by xargs gets the root "$(xargs)", which no gate can place."""
    words = [t[1:] if t.startswith("\\") else t for t in tokens]
    start = 0
    while start < len(words) and _ASSIGN.match(words[start]):
        start += 1
    prefixed = False
    for i, w in enumerate(words):
        if i < start:
            continue
        head = os.path.basename(w)
        if i > start and not prefixed:
            prefixed = os.path.basename(words[i - 1]) in _PREFIXES
            if not prefixed:
                continue
        rest = words[i + 1 :]
        if head == "git":
            if "grep" not in rest:
                continue
            g = rest.index("grep")
            pre = rest[:g]
            every, _ = _tool_roots("rg", rest[g + 1 :])
            return [p for p in pre if not p.startswith("-")] + every
        if head in _ALWAYS or (head in _GREPS and _grep_recurses(rest)):
            if "xargs" in [os.path.basename(x) for x in words[:i]]:
                return ["$(xargs)"]
            every, _ = _tool_roots(head, rest)
            return every
        if head == "find" and any(x in _FIND_EXEC for x in rest):
            return _find_roots(rest)
    return None


_BRACE = re.compile(r"\{([^{}]*)\}")
_SEQ = re.compile(r"(-?[0-9]+|[A-Za-z])\.\.(-?[0-9]+|[A-Za-z])(\.\.(-?[0-9]+))?")


def _sequence(body: str) -> list[str] | None:
    """The words of a bash sequence brace such as a..e or 1..9..2, or None
    when the body is none. A zero-padded or mixed sequence gives ["{"],
    which no path matches, and the caller refuses it."""
    m = _SEQ.fullmatch(body)
    if m is None:
        return None
    a, b, step = m.group(1), m.group(2), m.group(4)
    ints = a.lstrip("-").isdigit() and b.lstrip("-").isdigit()
    if not ints and (a.lstrip("-").isdigit() or b.lstrip("-").isdigit()):
        return ["{"]
    if ints and any(len(x.lstrip("-")) > 1 and x.lstrip("-").startswith("0") for x in (a, b)):
        return ["{"]
    if any(x is not None and len(x.lstrip("-")) > 9 for x in (a, b, step)):
        return ["{"]
    lo, hi = (int(a), int(b)) if ints else (ord(a), ord(b))
    n = abs(int(step)) if step else 1
    n = n or 1
    if abs(hi - lo) // n + 1 > _MAX_BRACES:
        return ["{"]
    d = n if hi >= lo else -n
    vals = list(range(lo, hi + (1 if d > 0 else -1), d))
    return [str(v) for v in vals] if ints else [chr(v) for v in vals]


def _brace_expand(word: str, out: list[str]) -> bool:
    """Expand the first {a,b} or bash sequence brace in word, again and
    again. False when the word holds more than the gate will expand, or a
    sequence it will not expand, such as a zero-padded one."""
    for m in _BRACE.finditer(word):
        body = m.group(1)
        if "," in body:
            alts = body.split(",")
        else:
            seq = _sequence(body)
            if seq is None:
                continue
            if seq == ["{"]:
                return False
            alts = seq
        for part in alts:
            if not _brace_expand(word[: m.start()] + part + word[m.end() :], out):
                return False
            if len(out) > _MAX_BRACES:
                return False
        return True
    out.append(word)
    return True


def _match_part(pat: str, name: str) -> bool:
    """One path part against one glob part: *, ?, and [...] with ! or ^
    and ranges. A [ with no closing ] is a plain character."""
    if not pat:
        return not name
    c = pat[0]
    if c == "*":
        return any(_match_part(pat[1:], name[i:]) for i in range(len(name) + 1))
    if not name:
        return False
    if c == "?":
        return _match_part(pat[1:], name[1:])
    if c == "[":
        end = pat.find("]", 2 if pat[1:2] in ("!", "^") else 1)
        if pat[1:2] in ("!", "^") and end == 2:
            end = pat.find("]", 3)
        if end == -1:
            return name[0] == "[" and _match_part(pat[1:], name[1:])
        body = pat[1:end]
        neg = body[:1] in ("!", "^")
        if neg:
            body = body[1:]
        hit = False
        j = 0
        while j < len(body):
            if j + 2 < len(body) and body[j + 1] == "-":
                if body[j] <= name[0] <= body[j + 2]:
                    hit = True
                j += 3
            else:
                if body[j] == name[0]:
                    hit = True
                j += 1
        return hit != neg and _match_part(pat[end + 1 :], name[1:])
    return c == name[0] and _match_part(pat[1:], name[1:])


def _parts(path: str) -> list[str]:
    return [p for p in path.split("/") if p]


def _overflow_reaches(word: str, base: str, targets: list[str]) -> bool:
    """For a word the gate will not expand: True unless the text before
    its first brace shows it cannot reach a target. A word with no / is
    placed in base, so it can reach one only when base leads to a target.
    A word whose text before the brace is not a plain absolute path is
    taken to reach one."""
    if "/" not in word:
        return any(t == base or t.startswith(base.rstrip("/") + "/") for t in targets)
    pre = _home_text(word[: word.index("{")]) if "{" in word else word
    pre = os.path.expanduser(pre)
    if not pre.startswith("/") or _GLOB.search(pre):
        return True
    if os.path.normpath(pre).rstrip("/") != pre.rstrip("/") and pre != "/":
        return True
    return any(t.startswith(pre) for t in targets)


def glob_matches(word: str, base: str, targets: list[str]) -> bool:
    """True when a glob word, brace-expanded and placed from base, can
    match any target path, part by part."""
    alts: list[str] = []
    if not _brace_expand(word, alts):
        return _overflow_reaches(word, base, targets)
    for alt in alts:
        pp = _parts(os.path.normpath(_place(alt, base)))
        for t in targets:
            tp = _parts(t)
            if len(tp) == len(pp) and all(_match_part(a, b) for a, b in zip(pp, tp, strict=True)):
                return True
    return False


def _data_dirs() -> list[str]:
    from opendaisugi.floor_config import coppice_data_dirs

    return [str(d) for d in coppice_data_dirs()]


def secret_files() -> list[str]:
    """Every coppice secret file under every data directory the gate knows."""
    from opendaisugi.pane_rule import _SECRET_NAMES

    return [d + "/" + s for d in _data_dirs() for s in _SECRET_NAMES]


def _reach_targets() -> list[str]:
    """Every path a search root must not match: each prefix of each secret
    file, from / down to the file."""
    out: list[str] = ["/"]
    for f in secret_files():
        parts = _parts(f)
        for i in range(1, len(parts) + 1):
            p = "/" + "/".join(parts[:i])
            if p not in out:
                out.append(p)
    return out


def _home_text(word: str) -> str:
    from opendaisugi.pane_rule import _home_spellings

    return _home_spellings(word)


def _place(word: str, cwd: str) -> str:
    from opendaisugi.pane_rule import _home_spellings

    p = os.path.expanduser(_home_spellings(word))
    if not os.path.isabs(p):
        p = os.path.join(cwd or os.getcwd(), p)
    return p


def path_above_secret(raw: str, cwd: str) -> bool:
    """True when a path, as typed, normalized and resolved from cwd, is an
    ancestor of a coppice data directory."""
    if not raw:
        return False
    p = _place(raw, cwd)
    cands = {os.path.normpath(p), str(Path(p).resolve(strict=False))}
    return any(d.startswith(c.rstrip("/") + "/") for d in _data_dirs() for c in cands)


# A variable, a command substitution, or a ~ form the gate does not
# expand. A $ at the end of a word, or before ) or |, is a regex anchor.
_UNRESOLVED = re.compile(r"\$[A-Za-z_{(0-9@*#?!$-]|`")


def _unresolvable(word: str) -> bool:
    return bool(_UNRESOLVED.search(word)) or word.startswith("~")


def _root_reaches(word: str, here: str | None) -> bool:
    """True when one search root, from cwd here (None when unknown),
    reaches a secret or cannot be placed."""
    from opendaisugi.floor_config import _expand_home

    w = _expand_home(word)
    if _unresolvable(w):
        return True
    if not os.path.isabs(w) and here is None:
        return True
    base = here or "/"
    if _GLOB.search(w):
        return glob_matches(w, base, _reach_targets())
    return path_above_secret(w, base)


def _line_reaches(command: str, cwd: str | None, depth: int) -> bool:
    from opendaisugi.effects import _next_cwd
    from opendaisugi.floor_config import _expand_home
    from opendaisugi.shell_decompose import decompose_command

    dec = decompose_command(command)
    if not dec.ok or not dec.commands:
        return bool(_PROGRAM_TEXT.search(command))
    here = cwd
    for text in dec.commands:
        try:
            raw = shlex.split(text)
        except ValueError:
            return bool(_PROGRAM_TEXT.search(command))
        for w in raw:
            if " " in w and _PROGRAM_TEXT.search(w):
                # Past the depth the gate follows, a line that still names
                # a search program is refused.
                if depth >= _MAX_DEPTH or _line_reaches(w, here, depth + 1):
                    return True
        roots = _search_roots(raw)
        if roots is not None and any(_root_reaches(r, here) for r in roots):
            return True
        here = _next_cwd([_expand_home(t) for t in raw], here)
    return False


def shell_search_reaches(command: str, cwd: str) -> bool:
    """True when a shell line searches a tree that reaches a secret."""
    return _line_reaches(command, cwd if os.path.isabs(cwd) else None, 0)


_PATH_KEYS = ("file_path", "path", "filePath")


def search_paths(payload: dict[str, Any]) -> list[str] | None:
    """The paths a file-read tool's input names, or None when it names a
    path that is not a string."""
    inp = payload.get("tool_input") or payload.get("args") or payload.get("input") or {}
    if not isinstance(inp, dict):
        return []
    out: list[str] = []
    for k in _PATH_KEYS:
        v = inp.get(k)
        if not v:
            continue
        if not isinstance(v, str):
            return None
        out.append(v)
    return out


def search_above_secret_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a call searches a tree that reaches a secret. Any error
    counts as a hit, so a broken check denies."""
    try:
        if record is None:
            return False
        cwd = str(payload.get("cwd") or "")
        step = record.get("step_type")
        if step == "shell":
            return shell_search_reaches(str(record.get("command") or ""), cwd)
        if step != "file_read":
            return False
        from opendaisugi.pane_rule import _NAMES_ONLY, _ONE_FILE

        tool = str(record.get("tool_name") or "")
        if tool in _NAMES_ONLY or tool in _ONE_FILE:
            return False
        paths = search_paths(payload)
        if paths is None:
            return True
        placed = all(os.path.isabs(os.path.expanduser(_home_text(p))) for p in paths)
        if not os.path.isabs(cwd) and (not paths or not placed):
            # A relative path, or no path, from a cwd the gate cannot place.
            return True
        if paths:
            return any(path_above_secret(p, cwd) for p in paths)
        return path_above_secret(cwd, cwd)
    except Exception:  # noqa: BLE001 - fail closed
        return True

"""The gate's rule that no agent edits the floor's own config.

coppice.toml names the plugins the coppice server runs, and the plugin
directories hold their code. A policy the server starts runs outside every
pane's gate. So a pane that could write there could run any program it
likes. The gate denies every write it can place under the coppice config
directory and the coppice data directories, before any envelope check,
in every mode. No operator ask can turn that deny. A path the gate
cannot place, such as a glob, a brace, or a variable the line sets, falls
to its tier, which is permanent.

A shell line or an MCP tool's arguments that name one of those
directories are denied too. Each word goes through the gate's path
normalizer, with ~, $HOME and the XDG homes expanded and each cd
followed, so ./, // and ../ spellings are caught. The check cannot tell
a read from a write there, and it denies both. After a cd the gate
cannot follow, a relative word that names a floor directory by its last
part, such as coppice, is denied too, and any other relative word is left
to its tier, which is permanent.

A file-read tool call, such as Read or Grep, is not denied by this rule.
Reading the floor's config is harmless: coppice.toml names harnesses,
plugins, projects and the gateway, and web.json holds paths and the name
of an environment variable, never a secret. An operator can put any value
in a [plugin.<id>] table, and such a value is left to the envelope. The
secrets the coppice data directory holds, and every directory that holds
one, are the pane rule's, since each is a door to allow.

A coppice server started with its own --data-dir tells each pane where
that is through COPPICE_DATA_DIR, and that directory is a floor directory
too. On a resident call the caller sends the value on its request. The
variable only adds a directory. The cost is the same as for the default
one: a word that holds the directory's path, or after a cd the gate cannot
follow a relative word that holds its last part, is denied. So a short or
common last part, such as data, denies more than the default does.

The same rule, with its own paths and its own refusal, guards OpenCode's
config. OpenCode loads code at start from its global config directory,
from each project's .opencode plugin and tool directories and from the
plugin list in any opencode.json. A pane that could write there could delete its own
gate, or load code beside it that undoes it. The rule covers the whole
OpenCode config directory under the XDG config home and under ~/.config,
every path with a .opencode component followed by plugin, plugins, tool
or tools, a .opencode directory itself, and every file named
opencode.json or opencode.jsonc, for every host. The text rule meets the
whole shell line, so a read that names these files, such as a grep, is
denied too, and fails closed. Only a git -m or --message value and the
free text of an MCP argument are left out, since they are messages for
people, not paths.
"""

from __future__ import annotations

import os
import re
import shlex
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any

from opendaisugi.effects import _next_cwd, _resolve

REFUSAL = "this is the floor's own config. Edit it yourself."
OPENCODE_PLUGIN_REFUSAL = "this is OpenCode's config or gate plugin. Edit it yourself."


def _never(_: str) -> bool:
    return False


@dataclass(frozen=True)
class Guard:
    """What one hard-deny rule covers: directories by prefix, a rule on a
    placed absolute path, and a rule on raw text such as a shell line."""

    roots: Callable[[], list[Path]]
    names: Callable[[str], bool] = _never
    text: Callable[[str], bool] = _never


DATA_DIR_ENV = "COPPICE_DATA_DIR"


def custom_data_dirs() -> list[Path]:
    """The coppice data directories the caller's pane names: the
    COPPICE_DATA_DIR this process sees, and on a resident call the one
    the caller sent on its request. coppice sets the variable on every
    pane it starts, over any value the server itself inherited. Only an
    absolute path counts. coppice always sends one, and a relative one
    says nothing about where the data is."""
    from opendaisugi.gate import _resident

    out: list[Path] = []
    caller = getattr(_resident, "caller", None) or {}
    for v in (os.environ.get(DATA_DIR_ENV), caller.get("data_dir")):
        if isinstance(v, str) and v and os.path.isabs(v):
            p = Path(os.path.normpath(v))
            if p not in out:
                out.append(p)
    return out


def coppice_data_dirs() -> list[Path]:
    """Every coppice data directory the gate knows: the default one and
    any the caller's pane names, each as written and as resolved."""
    return _spellings([Path.home() / ".opendaisugi" / "coppice", *custom_data_dirs()])


def _roots() -> list[Path]:
    """The floor's directories: the config directory, the coppice data
    directory, and the data home the runner writes policies to. Each comes
    in its XDG spelling and its default spelling. A data directory the
    caller's pane names through COPPICE_DATA_DIR is one of them too."""
    home = Path.home()
    out: list[Path] = []
    for base in (os.environ.get("XDG_CONFIG_HOME"), str(home / ".config")):
        if base:
            out.append(Path(base) / "coppice")
    for base in (os.environ.get("XDG_DATA_HOME"), str(home / ".local" / "share")):
        if base:
            out.append(Path(base) / "coppice")
    out.append(home / ".opendaisugi" / "coppice")
    out.extend(custom_data_dirs())
    return _spellings(out)


def _spellings(out: list[Path]) -> list[Path]:
    """Each root as written and as the OS resolves it, once each."""
    seen: list[Path] = []
    for r in out:
        for p in (r.expanduser(), r.expanduser().resolve(strict=False)):
            if p not in seen:
                seen.append(p)
    return seen


def opencode_config_roots() -> list[Path]:
    """OpenCode's global config directory, $XDG_CONFIG_HOME/opencode or
    ~/.config/opencode. It holds the global plugin directories and the
    global opencode.json."""
    home = Path.home()
    out: list[Path] = []
    for base in (os.environ.get("XDG_CONFIG_HOME"), str(home / ".config")):
        if base:
            out.append(Path(base) / "opencode")
    return _spellings(out)


_OPENCODE_FILES = frozenset({"opencode.json", "opencode.jsonc"})


def _opencode_path(path: str) -> bool:
    """True for a placed path that OpenCode loads code from: a .opencode
    directory, anything under its plugin, plugins, tool or tools directory,
    and any file named opencode.json or opencode.jsonc."""
    parts = PurePosixPath(path).parts
    if not parts:
        return False
    if parts[-1] in _OPENCODE_FILES or parts[-1] == ".opencode":
        return True
    return any(
        a == ".opencode" and b in ("plugin", "plugins", "tool", "tools")
        for a, b in zip(parts, parts[1:], strict=False)
    )


_OPENCODE_TEXT = re.compile(
    r"(^|[^\w.-])\.opencode(/+(plugins?|tools?)(\b|/)|/*(?=$|[\s'\"`;&|)]))|\bopencode\.jsonc?\b"
)


def _opencode_text(text: str) -> bool:
    return _OPENCODE_TEXT.search(text) is not None


_VARS = re.compile(
    r"\$\{(HOME|XDG_CONFIG_HOME|XDG_DATA_HOME)\}|\$(HOME|XDG_CONFIG_HOME|XDG_DATA_HOME)\b"
)


def _expand_home(text: str) -> str:
    """Expand ~, $HOME and the two XDG homes the way the shell will. An
    unset XDG home expands to nothing, as in the shell."""
    home = str(Path.home())

    def var(m: re.Match) -> str:
        name = m.group(1) or m.group(2)
        return home if name == "HOME" else os.environ.get(name, "")

    text = _VARS.sub(var, text)
    out = []
    for i, ch in enumerate(text):
        if (
            ch == "~"
            and (i == 0 or text[i - 1] in " \t'\"=:(")
            and text[i + 1 : i + 2] in ("/", "")
        ):
            out.append(home)
        else:
            out.append(ch)
    return "".join(out)


FLOOR = Guard(_roots)


def _fixed(guard: Guard) -> Guard:
    """The guard with its roots worked out once, for one check. Each root
    resolves through the filesystem, and one long line can place thousands
    of words. The roots are not kept past the check, since a symlink can
    change between checks."""
    roots = list(guard.roots())
    return Guard(lambda: roots, guard.names, guard.text)
OPENCODE = Guard(opencode_config_roots, _opencode_path, _opencode_text)


def _in_roots(path: str, guard: Guard = FLOOR) -> bool:
    for r in guard.roots():
        rs = str(r)
        if path == rs or path.startswith(rs.rstrip("/") + "/"):
            return True
    return guard.names(path)


def path_in_floor(raw: str, cwd: str, guard: Guard = FLOOR) -> bool:
    """True when a file path, as typed and as the OS resolves it from cwd,
    is inside a path the guard covers, the floor by default.
    Both spellings go through the gate's one path normalizer."""
    if not raw:
        return False
    word = _expand_home(raw)
    typed = os.path.normpath(os.path.expanduser(word))
    if os.path.isabs(typed) and _in_roots(typed, guard):
        return True
    base = cwd or os.getcwd()
    resolved = _resolve(word, base)
    if resolved is not None and _in_roots(resolved, guard):
        return True
    joined = os.path.normpath(os.path.join(base, os.path.expanduser(word)))
    return _in_roots(joined, guard)


# git's global options that take the next word as their value.
_GIT_GLOBAL_VALUE = frozenset(
    {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env"}
)
# git verbs whose -m takes a message. On any other verb, -m is a flag with
# no value, so the next word is a path and is checked.
_GIT_MESSAGE_VERBS = frozenset({"commit", "tag", "merge", "notes"})
_GIT_STASH_MESSAGE = frozenset({"push", "save"})


def _git_message_values(tokens: list[str]) -> list[str]:
    """The values of the -m and --message flags of git commit, tag, merge,
    notes, and stash push or save. They are a message for people, not a
    path. The verb is found after git's global options."""
    if not tokens or tokens[0] != "git":
        return []
    i = 1
    while i < len(tokens) and tokens[i].startswith("-"):
        i += 2 if tokens[i] in _GIT_GLOBAL_VALUE else 1
    if i >= len(tokens):
        return []
    verb = tokens[i]
    i += 1
    if verb == "stash":
        if i >= len(tokens) or tokens[i] not in _GIT_STASH_MESSAGE:
            return []
        i += 1
    elif verb not in _GIT_MESSAGE_VERBS:
        return []
    out: list[str] = []
    while i < len(tokens):
        t = tokens[i]
        if t == "--":
            break
        if t in ("-m", "--message") or (
            t.startswith("-") and not t.startswith("--") and t.endswith("m") and len(t) > 2
        ):
            if i + 1 < len(tokens):
                out.append(tokens[i + 1])
            i += 2
            continue
        if t.startswith("--message="):
            out.append(t.split("=", 1)[1])
        elif t.startswith("-m") and len(t) > 2:
            out.append(t[2:])
        i += 1
    return out


def _without_messages(text: str, messages: list[str]) -> str:
    """text with one occurrence of each message value taken out. One only,
    so a message cannot hide the same words where a command uses them."""
    for m in messages:
        if m:
            text = text.replace(m, " ", 1)
    return text


# The most working directories one shell line is followed through.
_MAX_CWDS = 32


def _names_root_part(word: str, guard: Guard) -> bool:
    """True when a relative word names a guarded directory by its last
    part, such as coppice. With the cwd unknown, such a word could land in
    the guarded directory, so it counts as a hit."""
    parts = Path(os.path.normpath(word)).parts
    return any(r.name in parts for r in guard.roots())


def _shell_hit(command: str, cwd: str, guard: Guard = FLOOR) -> tuple[bool, list[str]]:
    """Whether a shell line names a guarded path in any word or redirect,
    from any cwd the line passes through, and the git message values the
    line holds. The cwd follows each cd the way the gate's effects check
    follows it. After a cd the gate cannot follow, a relative word that
    names a guarded directory by its last part is a hit, and any other
    relative word is left to its tier, which is permanent. A git message
    value is not a path and is not placed."""
    try:
        from opendaisugi.shell_decompose import decompose_command

        dec = decompose_command(command)
    except ImportError:  # no parser leaves the text match
        return False, []
    except Exception:  # noqa: BLE001 - a line the rule cannot split is a hit
        return True, []
    if not dec.ok or not dec.commands:
        return False, []
    here: str | None = cwd or None
    cwds: list[str | None] = [here]
    messages: list[str] = []
    for text in dec.commands:
        try:
            raw = shlex.split(text)
        except ValueError:
            # The words cannot be placed, so the cwd after them is unknown.
            here = None
            if here not in cwds:
                cwds.append(here)
            continue
        values = _git_message_values(raw)
        messages.extend(values)
        tokens = [_expand_home(t) for t in raw]
        skip = list(values)
        for word, orig in zip(tokens, raw, strict=True):
            if orig in skip:
                skip.remove(orig)
                continue
            if word.startswith("-") and "=" in word:
                word = word.split("=", 1)[1]
                if orig.startswith("--message="):
                    continue
            if os.path.isabs(os.path.expanduser(word)) or here:
                if path_in_floor(word, here or "/", guard):
                    return True, messages
            elif _names_root_part(word, guard):
                return True, messages
        here = _next_cwd(tokens, here)
        if here not in cwds:
            if len(cwds) >= _MAX_CWDS:
                # Past this many directories the gate stops following cd,
                # as it does for a cd it cannot follow, so the check stays
                # well inside a hook timeout.
                here = None
            if here not in cwds:
                cwds.append(here)
    for p in (*dec.writes, *dec.reads):
        p = _expand_home(p)
        for c in cwds:
            if (os.path.isabs(os.path.expanduser(p)) or c) and path_in_floor(p, c or "/", guard):
                return True, messages
            if not c and not os.path.isabs(os.path.expanduser(p)) and _names_root_part(p, guard):
                return True, messages
    return False, messages


def text_names_floor(text: str, cwd: str, guard: Guard = FLOOR, free_text: bool = False) -> bool:
    """True when a shell line or a tool argument names a path the guard
    covers, as typed or after the gate places each of its words, or runs
    from inside one. The guard's text rule meets the whole line, less one
    copy of each git message value. free_text marks an MCP argument, which
    is data: its words are still placed, but the guard's text rule skips
    it."""
    guard = _fixed(guard)
    expanded = _expand_home(text)
    for r in guard.roots():
        if str(r) in expanded:
            return True
    if cwd and path_in_floor(".", cwd, guard):
        return True
    hit, messages = _shell_hit(text, cwd, guard)
    if hit:
        return True
    if free_text:
        return False
    return guard.text(_without_messages(expanded, [_expand_home(m) for m in messages]))


def _strings(value: Any, depth: int = 0) -> list[str]:
    if depth > 8:
        return []
    if isinstance(value, str):
        return [value]
    if isinstance(value, dict):
        return [s for v in value.values() for s in _strings(v, depth + 1)]
    if isinstance(value, (list, tuple)):
        return [s for v in value for s in _strings(v, depth + 1)]
    return []


def _hit(payload: dict[str, Any], record: dict[str, Any] | None, guard: Guard) -> bool:
    """True when a tool call writes to, or names, a path the guard covers.
    An MCP call that runs from inside one counts too. A file read is not a
    hit here: the pane rule guards the secrets. Any error counts as a hit,
    so a broken check denies."""
    try:
        if record is None:
            return False
        cwd = str(payload.get("cwd") or "")
        step = record.get("step_type")
        if step == "file_write":
            return path_in_floor(str(record.get("path") or ""), cwd, guard)
        if step == "shell":
            return text_names_floor(str(record.get("command") or ""), cwd, guard)
        if step == "mcp":
            if cwd and path_in_floor(".", cwd, guard):
                return True
            return any(
                path_in_floor(v, cwd or "/", guard)
                or text_names_floor(v, "", guard, free_text=True)
                for v in _strings(record.get("arguments"))
            )
        return False
    except Exception:  # noqa: BLE001 - fail closed
        return True


def floor_config_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a tool call writes to, or names, the floor's own config.
    Any error counts as a hit, so a broken check denies."""
    return _hit(payload, record, FLOOR)


def opencode_plugin_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a tool call writes to, or names, a path OpenCode loads code
    from, the gate plugin's directory among them. Any error counts as a
    hit."""
    return _hit(payload, record, OPENCODE)

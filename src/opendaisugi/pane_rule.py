"""The gate's rule that no agent answers an ask.

An agent can propose. Only the operator can allow. A pane's agent could try
to allow in three ways: run ``coppice agent allow``, write the answer file
the gate reads under ``asks/`` or ``answers/`` in the gate directory, or
take the phone's bearer token and call the phone's answer route. This
module finds all three in a tool call. The gate denies such a call before
any envelope check, in every mode.

``coppice agent deny`` is not an allow and passes here. A task foreman
denies the asks it holds that way, and coppice-server lets a pane deny
only an ask it holds now. The server knows a pane only by the caller's
process tree and session, so a deny that leaves both reads as the
operator's and could refuse any ask. So a line that sends a deny, in the
CLI or the wire form, is refused when it also names another server or
socket (``--remote``, ``--socket``, ``XDG_RUNTIME_DIR``, or a ``HOME=``
assignment), or a program that runs a command outside the pane's tree or
session: ssh, setsid, systemd-run, nohup, at, batch, tmux, screen, script,
sudo and the like. A wrapper word counts at the head of a command, after a
quote, or after a prefix such as env; the most telling ones, such as ssh
and setsid, count anywhere in the line. A deny that a script file or a
relay set up by an earlier call sends is not seen here.

The same door covers every other secret the coppice data directory holds,
since each is a way to reach the same answer route or a worse one: the
local CA's own private key would let a pane mint a certificate the phone
already trusts, a TLS leaf or tailscale private key would let it stand in
for the phone server, and the voice server's bearer token opens that
server the same way the web token opens this one. A public certificate or
a config file that holds no secret is left to the envelope.

The check reads the call's text. It finds the command through the shell
decomposition when that parses, and through plain text matches always, so
``x && coppice agent allow``, ``sh -c '...'``, a raw ``agent.allow`` wire
line, and a path to the ask files or any of these secret files, in any
common spelling, are all caught. A file read's path and an MCP tool's
arguments also go through the gate's path normalizer, so a doubled
slash, a bare ``.`` segment, or a bogus segment a ``..`` undoes cannot
spell a secret's path past the plain text match. A program that builds
the path at run time is not caught here, nor is a secret an operator
moved outside its default place with ``--ca-dir`` or
``--voice-token-file``. The coppice server's own pane check is the first
door for both.

A tool that searches a directory reads every file under it. So a
file-read call is denied too when its path is a directory that holds a
secret: a coppice data directory itself, or its web, web/ca, web/tls or
voice directory. The data directories are the default one and any the
caller's pane names through COPPICE_DATA_DIR, which coppice sets on each
pane it starts. The path counts as typed, normalized and resolved, so a
./, // or ../ spelling or a symlink does not hide it. A search tool with
no path searches its cwd, and the record then holds only its pattern, so
for a search tool the cwd is checked too. Read opens only the one file
its path names, so its cwd is not checked. Glob prints file names and
never their contents, and a secret's name is no secret, so Glob gets no
directory check.

A search rooted above a data directory is the search rule's (see
search_rule.py). A shell word that holds a glob, such as
``~/.opendaisugi/*/*/*``, is placed from the cwd the line has reached
and refused when it can match a secret file.
"""

from __future__ import annotations

import os
import re
import shlex
from pathlib import Path
from typing import Any

REFUSAL = "a pane can propose. It cannot allow."

_ASK_DIRS = ("asks", "answers")

# coppice, then any words that do not end a command, then agent allow.
_ALLOW_TEXT = re.compile(r"\bcoppice\b[^\n;&|]*?\bagent\b[\s'\"]+(allow)\b")
# The wire verb as a request names it, in JSON or in a program's dict, or
# the CLI words as a list in a program. A bare mention, as in a grep for
# the verb name, is left to the envelope.
_WIRE_TEXT = re.compile(
    r"['\"]?(cmd|method)\\?['\"]?\s*[:=]\s*\\?['\"]agent\.(allow)\\?['\"]"
    r"|['\"]agent['\"]\s*,\s*['\"](allow)['\"]"
)
# A deny in the CLI form or the wire form.
_DENY_TEXT = re.compile(
    r"\bcoppice\b[^\n;&|]*?\bagent\b[\s'\"]+deny\b"
    r"|['\"]?(cmd|method)\\?['\"]?\s*[:=]\s*\\?['\"]agent\.deny\\?['\"]"
    r"|['\"]agent['\"]\s*,\s*['\"]deny['\"]"
)
# Another server or another socket than the pane's own.
_OTHER_SERVER = re.compile(r"(^|[\s'\"])--(remote|socket)(=|[\s'\"]|$)|\bXDG_RUNTIME_DIR\b|\bHOME=")
# Programs that run a command outside the caller's process tree or session.
_WRAPPERS = (
    "ssh|mosh|setsid|systemd-run|nohup|at|batch|tmux|screen|dtach|abduco|zellij|byobu"
    "|script|unbuffer|expect|daemonize|start-stop-daemon|sudo|doas|pkexec|su|runuser"
    "|machinectl|crontab|flatpak-spawn|distrobox-host-exec"
)
# A wrapper at the head of a command, after a quote, or after env
# assignments or a prefix such as env or exec.
_WRAPPER_HEAD = re.compile(
    r"(?:^|[\n;&|(){}`'\"!])\s*"
    r"(?:\w+=\S*\s+)*"
    r"(?:(?:\S*/)?(?:env|exec|command|builtin|nice|time|stdbuf|ionice|chrt|taskset|timeout)"
    r"\b[^\n;&|]*?\s)?"
    r"(?:\S*/)?(?:" + _WRAPPERS + r")(?:$|[\s;&|)'\"`])"
)
# Wrapper names, and a program's own detach, too telling to need the head.
_WRAPPER_ANYWHERE = re.compile(
    r"\b(ssh|setsid|systemd-run|nohup|tmux|daemonize|start-stop-daemon|start_new_session)\b"
)


def shell_denies_from_outside(command: str) -> bool:
    """True when a shell line sends a deny through another server or
    socket, or under a program that runs it outside the pane's process
    tree and session, where the server would read it as the operator's."""
    if not _DENY_TEXT.search(command):
        return False
    return bool(
        _OTHER_SERVER.search(command)
        or _WRAPPER_HEAD.search(command)
        or _WRAPPER_ANYWHERE.search(command)
    )


# The web door: the command that prints the phone's bearer token, and
# every secret file a coppice data directory holds, named by the same
# directory and file names wherever that data directory itself lives, so
# a plain substring match works the same way it does for the token below:
# the web token, the local CA's private key and the TLS leaf key it
# signs, the private key of a tailscale certificate coppice writes, and
# the voice server's bearer token. A public cert (``*.crt``) or a config
# file such as ``web.json``, which holds a path to a token and not the
# token itself, is not a secret and is not in this set.
_WEB_TOKEN_CMD = re.compile(r"\bcoppice\b[^\n;&|]*?\bweb\b[\s'\"]+token\b")
# Each secret file, relative to its coppice data directory.
_SECRET_NAMES = (
    "web/token",
    "web/ca/ca.key",
    "web/ca/leaf.key",
    "web/tls/tailscale.key",
    "voice/token",
)
_SECRET_FILE = re.compile(
    r"(^|[^A-Za-z0-9_])"
    r"(web/token|web/ca/ca\.key|web/ca/leaf\.key|web/tls/tailscale\.key|voice/token)"
    r"\b"
)
_WEB_ANSWER_ROUTE = "/api/ask/answer"

# A gate directory's ask files in a relative or home spelling.
_ASK_PATH_TEXT = re.compile(r"(^|[\s'\"=:(/])gate/(asks|answers)\b")

# Global options of the coppice CLI that take a value.
_VALUE_OPTIONS = frozenset({"--socket", "--remote", "--data-dir"})


def _home_spellings(text: str) -> str:
    """Expand ``~``, ``$HOME`` and ``${HOME}`` so a path reads the same in
    every spelling."""
    home = str(Path.home())
    text = text.replace("${HOME}", home).replace("$HOME", home)
    return re.sub(r"(^|[\s'\"=:(])~(?=/)", lambda m: m.group(1) + home, text)


def _is_allow_words(words: list[str]) -> bool:
    """True when a word list runs ``coppice [options] agent allow``."""
    for i, w in enumerate(words):
        if os.path.basename(w) != "coppice":
            continue
        j = i + 1
        while j < len(words) and words[j].startswith("--"):
            j += 2 if words[j] in _VALUE_OPTIONS else 1
        if j + 1 < len(words) and words[j] == "agent" and words[j + 1] == "allow":
            return True
    return False


def _commands(command: str) -> list[str]:
    """The simple commands in a shell line, or the whole line when it does
    not parse."""
    try:
        from opendaisugi.shell_decompose import decompose_command

        d = decompose_command(command)
        if d.ok and d.commands:
            return list(d.commands)
    except Exception:  # noqa: BLE001 - a parser failure falls back to the whole line
        pass
    return [command]


def web_door(text: str) -> bool:
    """True when text asks for the phone's token or its answer route, or
    names any other secret file the coppice data directory holds: the
    local CA's key, a TLS leaf or tailscale key, or the voice token."""
    return bool(
        _WEB_TOKEN_CMD.search(text) or _SECRET_FILE.search(text) or _WEB_ANSWER_ROUTE in text
    )


def shell_allows(command: str) -> bool:
    """True when a shell line runs an allow through coppice or on the wire,
    or opens the web door."""
    if web_door(command):
        return True
    for cmd in _commands(command):
        try:
            words = shlex.split(cmd)
        except ValueError:
            words = cmd.split()
        if _is_allow_words(words):
            return True
    return bool(_ALLOW_TEXT.search(command) or _WIRE_TEXT.search(command))


def _roots(root: Path | None) -> list[Path]:
    """The gate roots to guard: the one in force and the default one."""
    from opendaisugi.gate import DEFAULT_GATE_ROOT

    out: list[Path] = []
    for r in (root, DEFAULT_GATE_ROOT):
        if r is None:
            continue
        for p in (Path(r).expanduser(), Path(r).expanduser().resolve(strict=False)):
            if p not in out:
                out.append(p)
    return out


def _under_ask_dirs(path: Path, roots: list[Path]) -> bool:
    for r in roots:
        for sub in _ASK_DIRS:
            d = r / sub
            if path == d or d in path.parents:
                return True
    return False


def path_touches_asks(raw: str, cwd: str, root: Path | None) -> bool:
    """True when a tool's file path is inside a gate's asks or answers."""
    if not raw:
        return False
    p = Path(_home_spellings(raw)).expanduser()
    if not p.is_absolute():
        p = Path(cwd or os.getcwd()) / p
    roots = _roots(root)
    candidates = {Path(os.path.normpath(p)), p.resolve(strict=False)}
    return any(_under_ask_dirs(c, roots) for c in candidates)


def path_names_secret(raw: str, cwd: str) -> bool:
    """True when a file path, as typed and as the OS resolves it from
    cwd, names one of the coppice secrets ``_SECRET_FILE`` matches. Both
    spellings go through the gate's one path normalizer, the same as
    ``path_touches_asks`` does for the gate's own ask files, so a doubled
    slash, a bare ``.`` segment, or a bogus segment a ``..`` undoes
    cannot spell past the plain substring check ``web_door`` runs on the
    text as typed."""
    if not raw:
        return False
    if _SECRET_FILE.search(raw):
        return True
    p = Path(_home_spellings(raw)).expanduser()
    if not p.is_absolute():
        p = Path(cwd or os.getcwd()) / p
    normalized = os.path.normpath(str(p))
    resolved = str(p.resolve(strict=False))
    return bool(_SECRET_FILE.search(normalized) or _SECRET_FILE.search(resolved))


# Each directory, under a coppice data directory, that holds a secret
# file: the data directory itself and each directory on the way down.
_SECRET_DIRS = ("", "web", "web/ca", "web/tls", "voice")
# Tools that print file names only, never contents.
_NAMES_ONLY = frozenset({"Glob"})
# Tools that open only the one file their path names.
_ONE_FILE = frozenset({"Read", "read"})


def _secret_dirs() -> list[str]:
    """Every directory that holds a coppice secret, as written and as the
    OS resolves it."""
    from opendaisugi.floor_config import coppice_data_dirs

    out: list[str] = []
    for d in coppice_data_dirs():
        for sub in _SECRET_DIRS:
            p = d / sub if sub else d
            for s in (str(p), str(p.resolve(strict=False))):
                if s not in out:
                    out.append(s)
    return out


def path_holds_secret(raw: str, cwd: str) -> bool:
    """True when a path, as typed, normalized and resolved from cwd, is a
    directory that holds a coppice secret."""
    if not raw:
        return False
    p = Path(_home_spellings(raw)).expanduser()
    if not p.is_absolute():
        p = Path(cwd or os.getcwd()) / p
    dirs = _secret_dirs()
    return os.path.normpath(str(p)) in dirs or str(p.resolve(strict=False)) in dirs


def _search_refuses(command: str, cwd: str) -> bool:
    """True when the search rule refuses this line anyway, so its own
    reason, which names the fix, is the one the agent sees."""
    from opendaisugi.search_rule import shell_search_reaches

    return shell_search_reaches(command, cwd)


def shell_globs_secret(command: str, cwd: str) -> bool:
    """True when a word in a shell line holds a glob that, placed from the
    cwd the line has reached, can match a coppice secret file."""
    from opendaisugi.effects import _next_cwd
    from opendaisugi.floor_config import _expand_home
    from opendaisugi.search_rule import _GLOB, glob_matches, secret_files
    from opendaisugi.shell_decompose import decompose_command

    dec = decompose_command(command)
    if not dec.ok or not dec.commands:
        return False
    files = secret_files()
    here: str | None = cwd or None
    for text in dec.commands:
        try:
            raw = shlex.split(text)
        except ValueError:
            return False
        tokens = [_expand_home(t) for t in raw]
        for w in tokens:
            if w.startswith("-") and "=" in w:
                w = w.split("=", 1)[1]
            if not _GLOB.search(w) or not (os.path.isabs(w) or here):
                continue
            if glob_matches(w, here or "/", files):
                return True
        here = _next_cwd(tokens, here)
    return False


def shell_touches_asks(command: str, cwd: str, root: Path | None) -> bool:
    """True when a shell line names a gate's asks or answers: by a full path,
    by the gate directory and one of the two names, by a ``gate/asks`` or
    ``gate/answers`` fragment, or by a relative name from a working
    directory inside the gate directory."""
    text = _home_spellings(command)
    if _ASK_PATH_TEXT.search(text):
        return True
    roots = _roots(root)
    names = re.search(r"\b(asks|answers)\b", text) is not None
    for r in roots:
        rs = str(r)
        for sub in _ASK_DIRS:
            if rs + "/" + sub in text:
                return True
        if names and rs in text:
            return True
    if names and cwd:
        here = Path(os.path.normpath(Path(cwd).expanduser()))
        for r in roots:
            if here == r or r in here.parents:
                return True
    return False


def _strings(value: Any, depth: int = 0) -> list[str]:
    """Every string inside an MCP tool's arguments, to a fixed depth. A
    tool that writes files names its path somewhere in them."""
    if depth > 8:
        return []
    if isinstance(value, str):
        return [value]
    if isinstance(value, dict):
        return [s for v in value.values() for s in _strings(v, depth + 1)]
    if isinstance(value, (list, tuple)):
        return [s for v in value for s in _strings(v, depth + 1)]
    return []


def pane_rule_hit(
    payload: dict[str, Any], record: dict[str, Any] | None, root: Path | None
) -> bool:
    """True when a tool call tries to answer an ask. Any error counts as a
    hit, so a broken check denies."""
    try:
        cwd = str(payload.get("cwd") or "")
        if record is None:
            return False
        step = record.get("step_type")
        if step == "shell":
            command = str(record.get("command") or "")
            return (
                shell_allows(command)
                or shell_denies_from_outside(command)
                or (shell_globs_secret(command, cwd) and not _search_refuses(command, cwd))
                or shell_touches_asks(command, cwd, root)
            )
        if step == "file_write":
            return path_touches_asks(str(record.get("path") or ""), cwd, root)
        if step == "file_read":
            path = str(record.get("path") or "")
            if web_door(path) or path_names_secret(path, cwd):
                return True
            tool = str(record.get("tool_name") or "")
            if tool in _NAMES_ONLY:
                return False
            if path_holds_secret(path, cwd):
                return True
            if tool not in _ONE_FILE:
                from opendaisugi.search_rule import search_paths

                paths = search_paths(payload)
                if paths is None or any(path_holds_secret(p, cwd) for p in paths):
                    return True
            return tool not in _ONE_FILE and path_holds_secret(cwd, cwd)
        if step == "mcp":
            return any(
                web_door(v)
                or path_names_secret(v, cwd)
                or path_touches_asks(v, cwd, root)
                or shell_touches_asks(v, cwd, root)
                for v in _strings(record.get("arguments"))
            )
        return False
    except Exception:  # noqa: BLE001 - fail closed
        return True

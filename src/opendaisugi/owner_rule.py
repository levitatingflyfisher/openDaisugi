"""The gate's rules that keep the owner's verbs for the owner.

Some ``daisugi`` verbs change what the gate checks or how it enforces. An
agent that could run one could widen its own authority:

- ``daisugi rank record`` writes a ranking vote as the owner
  (``rank_rule``);
- ``daisugi gate register`` and ``gate init``, ``daisugi start``, and
  ``daisugi tree root``, ``spawn``, ``end`` and ``answer`` register an
  envelope or write the delegation tree (``tree_rule``);
- ``daisugi gate disarm`` turns the gate off; ``gate arm`` turns it on
  again; ``gate serve`` starts the resident gate, the process the hooks
  trust for a verdict; ``daisugi install`` writes, changes the mode of, or
  removes the gate hook; ``daisugi graft install`` and ``graft remove``
  write and remove the graft rules (this module, ``REFUSAL``);
- ``daisugi router label`` records a task's outcome, which decides a graft
  trial (this module, ``LABEL_REFUSAL``);
- ``daisugi pack install``, ``remove`` and ``bundle`` fetch code or change
  the Python the binaries run as a worker (this module, ``PACK_REFUSAL``).

The gate denies an agent's shell line that runs one of them, before any
envelope check, in every mode, and no operator ask can turn that deny.

The line is read as the shell runs it. ``shell_decompose`` splits it into
its simple commands, the ones inside a substitution included. Each simple
command is split into words with ``shlex``. A word whose last path part is
``daisugi``, ``daisugi-py`` or ``opendaisugi``, or starts with
``opendaisugi.``, is the head of a ``daisugi`` command, wherever it is in
the words: so a path, an ``env``, ``nohup`` or ``uv run`` wrapper and
``python -m opendaisugi`` are all seen. The words after the head that do
not start with ``-`` are its arguments. The first argument that names a
top-level command (``COMMANDS``) is the command; for a group in
``SUBCOMMANDS``, the first later argument that names one of its
subcommands is the subcommand. The root takes no option with a value, so
any other word there cannot be one; it is skipped, which can only find
more. The line is a hit when the command, or the command and its
subcommand, is one of the verbs.

So ``daisugi status && npm start`` is not a hit: ``start`` is npm's. And
``echo daisugi gate disarm`` is a hit: the rule does not know what echo
does with its words.

Four fallbacks read words in order, as the rules did before they parsed:
a line that does not decompose, a simple command ``shlex`` cannot split,
a simple command that holds a form ``shlex`` splits differently from the
shell (a line continuation, ``$'...'`` or ``$"..."``), and a word with a
character outside ``_WORD`` (the payload of ``sh -c``, ``bash -lc`` or
``eval``, or a quoted message). Line continuations are joined, backslashes
and quotes are dropped, a word is a run of ``_WORD`` characters, and the
text is a hit when a head word comes before the command word, which comes
before the subcommand word. So ``sh -c 'daisugi gate disarm'`` is a hit, and so is a
commit message that names the three words in that order.

Words the shell builds at run time (a variable, a command substitution
used as a word, ``eval`` of a variable) are not seen. These rules are a
second line: an agent's envelope should not allow the ``daisugi`` shell
head at all.
"""

from __future__ import annotations

import shlex
from typing import Any

REFUSAL = "only the operator changes how the gate enforces. Run it yourself."

# Every top-level command of the daisugi CLI, hidden ones included.
COMMANDS = frozenset(
    {
        "batch",
        "bench",
        "config",
        "conformance",
        "coppice",
        "dashboard",
        "distill-repeats",
        "gardener",
        "gate",
        "gateway",
        "gateway-report",
        "generate-envelope",
        "graft",
        "help",
        "hook",
        "install",
        "journal",
        "lora",
        "mcp",
        "metrics",
        "models",
        "modules",
        "onboard",
        "orchestrate",
        "pack",
        "pathways",
        "rank",
        "registry",
        "release",
        "route",
        "router",
        "run",
        "setup",
        "start",
        "status",
        "tend",
        "tiers",
        "tree",
        "verify",
        "viz",
        "voice",
        "weave",
    }
)

# The subcommands of each group that holds an owner's verb.
SUBCOMMANDS: dict[str, frozenset[str]] = {
    "gate": frozenset(
        {
            "arm",
            "audit",
            "check",
            "disarm",
            "init",
            "proposals",
            "register",
            "replay",
            "report",
            "serve",
            "settings",
            "status",
        }
    ),
    "graft": frozenset({"install", "remove", "status"}),
    "pack": frozenset({"bundle", "install", "list", "remove", "run", "status"}),
    "router": frozenset({"label", "status", "stop"}),
    "rank": frozenset({"choose", "fit", "queue", "record"}),
    "tree": frozenset({"answer", "check", "end", "root", "spawn", "status"}),
}

RANK_VERBS = frozenset({("rank", "record")})
TREE_VERBS = frozenset(
    {
        ("gate", "register"),
        ("gate", "init"),
        ("start",),
        ("tree", "root"),
        ("tree", "spawn"),
        ("tree", "end"),
        ("tree", "answer"),
    }
)
GATE_VERBS = frozenset(
    {
        ("gate", "disarm"),
        ("gate", "arm"),
        ("gate", "serve"),
        ("install",),
        ("graft", "install"),
        ("graft", "remove"),
    }
)

LABEL_VERBS = frozenset({("router", "label")})
LABEL_REFUSAL = "only the operator labels a task's outcome. Label it yourself."

# A pack install, remove or bundle fetches code from the network, or
# changes the Python the binaries run as a worker (PK-R-12).
PACK_VERBS = frozenset({("pack", "install"), ("pack", "remove"), ("pack", "bundle")})
PACK_REFUSAL = "only the operator installs, removes or bundles a pack. Run it yourself."

# The shell forms shlex splits differently from the shell.
_SHLEX_GAPS = ("\\\n", "$'", '$"')

_WORD = frozenset("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./:=+@%,-")


def words(text: str) -> list[str]:
    """Backslashes and quotes dropped, then the runs of ``_WORD``
    characters. Any other character, any non-ASCII one included, ends a
    word."""
    # A backslash-newline is a line continuation: the shell joins the two
    # lines, so the word scan does too.
    text = text.replace("\\\n", "")
    for ch in ("\\", "'", '"'):
        text = text.replace(ch, "")
    out: list[str] = []
    cur: list[str] = []
    for ch in text:
        if ch in _WORD:
            cur.append(ch)
        elif cur:
            out.append("".join(cur))
            cur = []
    if cur:
        out.append("".join(cur))
    return out


def is_head(word: str) -> bool:
    """True when ``word`` names the daisugi CLI: its last path part is
    ``daisugi``, ``daisugi-py`` or ``opendaisugi``, or starts with
    ``opendaisugi.``."""
    last = word.rsplit("/", 1)[-1]
    return last in ("daisugi", "daisugi-py", "opendaisugi") or last.startswith("opendaisugi.")


def scan_words(text: str, verbs: frozenset[tuple[str, ...]]) -> bool:
    """The word-order reading: a head word, then each word of a verb, in
    that order, anywhere in ``text``."""
    ws = words(text)
    for verb in verbs:
        want = 0
        for w in ws:
            if want == 0:
                if is_head(w):
                    want = 1
            elif w == verb[want - 1]:
                want += 1
                if want > len(verb):
                    return True
    return False


def command_path(args: list[str]) -> tuple[str, ...]:
    """The command and subcommand the arguments of one daisugi head name:
    () when none, (command,) or (command, subcommand)."""
    pos = [a for a in args if not a.startswith("-")]
    for i, w in enumerate(pos):
        if w in COMMANDS:
            subs = SUBCOMMANDS.get(w)
            if subs is not None:
                for x in pos[i + 1 :]:
                    if x in subs:
                        return (w, x)
            return (w,)
    return ()


def _simple_commands(command: str) -> list[str] | None:
    """The simple commands of a shell line, or None when it does not
    decompose (or the parser raises)."""
    try:
        from opendaisugi.shell_decompose import decompose_command

        d = decompose_command(command)
        if d.ok and d.commands:
            return list(d.commands)
    except Exception:  # noqa: BLE001 - a parser failure reads the words in order
        pass
    return None


def _simple_hit(simple: str, verbs: frozenset[tuple[str, ...]]) -> bool:
    """One simple command: a daisugi head whose arguments name a verb, or a
    word that holds a command string naming one."""
    try:
        tokens = shlex.split(simple)
    except ValueError:
        return scan_words(simple, verbs)
    for i, tok in enumerate(tokens):
        if is_head(tok):
            if command_path(tokens[i + 1 :]) in verbs:
                return True
        elif any(ch not in _WORD for ch in tok) and scan_words(tok, verbs):
            return True
    # shlex does not read a line continuation or $'...' and $"..." quoting
    # as the shell does; a command that holds one is also read by word
    # order.
    if any(form in simple for form in _SHLEX_GAPS):
        return scan_words(simple, verbs)
    return False


def runs_verb(command: str, verbs: frozenset[tuple[str, ...]]) -> bool:
    """True when the shell line ``command`` runs one of ``verbs``."""
    simples = _simple_commands(command)
    if simples is None:
        return scan_words(command, verbs)
    for simple in simples:
        if _simple_hit(simple, verbs):
            return True
    return False


def names_gate_change(text: str) -> bool:
    """True when ``text`` runs a verb that changes how the gate enforces."""
    return runs_verb(text, GATE_VERBS)


def shell_hit(record: dict[str, Any] | None, verbs: frozenset[tuple[str, ...]]) -> bool:
    """True when a shell call runs one of ``verbs``. Any error counts as a
    hit, so a broken check denies."""
    try:
        if record is None or record.get("step_type") != "shell":
            return False
        command = record.get("command")
        return isinstance(command, str) and runs_verb(command, verbs)
    except Exception:  # noqa: BLE001 - fail closed
        return True


def gate_change_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs a verb that changes how the gate
    enforces."""
    return shell_hit(record, GATE_VERBS)


def label_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs ``daisugi router label``."""
    return shell_hit(record, LABEL_VERBS)


def pack_hit(payload: dict[str, Any], record: dict[str, Any] | None) -> bool:
    """True when a shell call runs ``daisugi pack install``, ``remove`` or
    ``bundle``."""
    return shell_hit(record, PACK_VERBS)

"""Fail-closed compound-shell decomposition for the verifier.

The verifier's default shell gate rejects any command containing a shell
metacharacter (``;`` ``|`` ``&`` `` ` `` ``<`` ``>`` ``$(`` newline), because a
single allowlisted head can smuggle more work past a head-classifier
(``git status; rm -rf /``). That is correct but blunt: it also rejects every
realistic multi-command line an agent actually runs.

This module lets an envelope opt into a *sound* alternative: parse the command
with a real bash grammar (tree-sitter-bash), and if — and only if — it is a
composition of plain simple-commands joined by safe operators (pipes, ``&&`` /
``||`` / ``;``, newlines), return each command's literal head so the caller can
check EVERY head against the allowlist. Anything the grammar can't prove safe is
rejected:

  * a parse error or missing token (``root.has_error`` / ``is_missing``);
  * command substitution ``$(...)`` / backticks, or process substitution
    ``<(...)`` — either can run an unbounded second command;
  * any redirection — ``echo x > /etc/cron.d/pwn`` has a harmless head but
    writes a file the ``file_write`` scope never authorized;
  * a non-literal head (``$CMD``, ``${x:-rm}``, a quoted/concatenated word) — the
    real command isn't known until runtime;
  * a known command-taking wrapper (``eval``, ``sh -c``, ``xargs`` …) — its head
    is allowlisted but it runs an argument as a command.

The non-literal-head and redirection rejections are the load-bearing ones; the
wrapper set is a denylist and therefore the weakest link (kept short + explicit).

tree-sitter-bash is an optional dependency (``opendaisugi[shell]``). When it is
absent, :func:`decompose_command` returns ``ok=False`` with a clear reason, and
the verifier treats an opted-in-but-unparseable command as a hard reject
(fail-closed on a missing capability) rather than silently accepting it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from functools import lru_cache

# Command-taking wrappers: their head is a normal allowlistable word, but they
# execute an *argument* as a command, so verifying the wrapper head alone is a
# bypass. Denylist — the weakest link here; non-literal-head + redirection
# rejection below is what actually bounds the head set.
_WRAPPER_DENYLIST = frozenset(
    {
        "eval",
        "exec",
        "env",
        "sh",
        "bash",
        "zsh",
        "dash",
        "command",
        "xargs",
        "sudo",
        "nice",
        "timeout",
        "nohup",
        "watch",
        "time",
    }
)

# Grammar node types that can run an unbounded second command or escape a
# declared scope — never decomposable, always reject.
_SUBSTITUTION_NODES = frozenset({"command_substitution", "process_substitution"})
_REDIRECTION_NODES = frozenset({"redirected_statement", "file_redirect", "heredoc_redirect"})


@dataclass(frozen=True)
class Decomposition:
    """Result of decomposing one shell command.

    ``ok`` is True only when the command is a safe composition of plain
    simple-commands. ``heads`` is then every literal command head in order (a
    human-readable summary), and ``commands`` is the full source text of each
    simple command, so the caller can re-verify each one through the ordinary
    single-command path (head allowlist, interpreter policy, …). When ``ok`` is
    False, ``reason`` says why it was rejected (surfaced in the violation).
    """

    ok: bool
    heads: tuple[str, ...] = ()
    commands: tuple[str, ...] = ()
    reason: str = field(default="")


@lru_cache(maxsize=1)
def _load_parser():
    """Return a tree-sitter bash Parser, or None if the extra isn't installed."""
    try:
        import tree_sitter_bash as tsb
        from tree_sitter import Language, Parser
    except ImportError:
        return None
    return Parser(Language(tsb.language()))


def decompose_command(command: str) -> Decomposition:
    """Decompose ``command`` into literal simple-command heads, or reject.

    See the module docstring for the exact fail-closed rules.
    """
    parser = _load_parser()
    if parser is None:
        return Decomposition(
            False, reason="shell decomposition parser not installed (opendaisugi[shell])"
        )

    root = parser.parse(command.encode("utf-8")).root_node
    if root.has_error:
        return Decomposition(False, reason="malformed shell (parse error)")

    heads: list[str] = []
    commands: list[str] = []
    reason: str | None = None

    def visit(node) -> None:
        nonlocal reason
        if reason is not None:
            return
        if node.is_missing:
            reason = "malformed shell (missing token)"
            return
        if node.type in _SUBSTITUTION_NODES:
            reason = "command/process substitution not decomposable"
            return
        if node.type in _REDIRECTION_NODES:
            reason = "shell redirection not decomposable (would escape file scope)"
            return
        if node.type == "command":
            name = node.child_by_field_name("name")
            if name is None:
                reason = "command with no resolvable head"
                return
            # A literal head is a single ``word`` child; anything else
            # (simple_expansion $CMD, expansion ${..}, string, concatenation)
            # is not known until runtime.
            if [c.type for c in name.children] != ["word"]:
                reason = f"non-literal command head ({name.text.decode('utf-8', 'replace')!r})"
                return
            head = name.text.decode("utf-8", "replace")
            if head in _WRAPPER_DENYLIST:
                reason = f"command-taking wrapper {head!r} not decomposable"
                return
            heads.append(head)
            commands.append(node.text.decode("utf-8", "replace"))
        for child in node.children:
            visit(child)
            if reason is not None:
                return

    visit(root)

    if reason is not None:
        return Decomposition(False, reason=reason)
    if not heads:
        return Decomposition(False, reason="no command heads found")
    return Decomposition(True, heads=tuple(heads), commands=tuple(commands))

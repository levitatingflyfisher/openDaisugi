"""Fail-closed compound-shell decomposition for the verifier.

The verifier's default shell gate rejects any command containing a shell
metacharacter (``;`` ``|`` ``&`` `` ` `` ``<`` ``>`` ``$(`` newline), because a
single allowlisted head can smuggle more work past a head-classifier
(``git status; rm -rf /``). That is correct but blunt: it also rejects every
realistic multi-command line an agent actually runs.

This module lets an envelope opt into a *sound* alternative: parse the command
with a real bash grammar (tree-sitter-bash) and return everything the shell
itself will do, so the caller can check each piece against the envelope —

  * every simple command's literal head (``heads``) and full text
    (``commands``), for the allowlist + interpreter-policy checks;
  * every literal redirect target, split into ``reads`` and ``writes``, for the
    envelope's ``file_read`` / ``file_write`` scope checks (ADR-0014 — a
    redirect is a file access spelled in shell, so it faces the same scope the
    ``file_write`` step type does);
  * substitution bodies (``$(...)``, backticks, ``<(...)``) are *recursively*
    decomposed — their inner commands appear in ``heads``/``commands`` like any
    other, so "everything that executes has a checked head" holds through
    nesting (ADR-0014; previously a blanket rejection).

Anything the grammar cannot prove literal still fails closed:

  * a parse error or missing token (``root.has_error`` / ``is_missing``);
  * a non-literal redirect target (``> $OUT``, ``> out$N.txt``, ``> $(mktemp)``)
    — the touched path isn't known until runtime;
  * a non-literal head (``$CMD``, ``${x:-rm}``, a quoted/concatenated word).

Command-taking wrappers (``sh -c``, ``xargs``, ``timeout``, ``eval`` …) are NOT
rejected here (they were pre-ADR-0014): each decomposed simple command flows
through the verifier's single-command path, where the interpreter layer
extracts and recursively verifies wrapped payloads and ``shell_interpreter_policy``
governs the opaque ones — exactly the treatment a standalone wrapper gets.

Heredocs (``<<``) and herestrings (``<<<``) feed *stdin data*, not files, so
they are safe as redirections; their bodies are still walked, so a substitution
inside an unquoted heredoc surfaces its inner head like any other. FD
duplications (``2>&1``) and closes (``2>&-``) touch no path and pass.

tree-sitter-bash 0.25.1's GLR parser has a fail-OPEN bug (G-4): it can fuse a
newline statement boundary into a single ``command`` node (``c1\nd1`` parses as
command ``c1`` with argument ``d1``), so a head after the newline would execute
unchecked. A correct POSIX parser (mvdan/sh) never fuses. Rather than fail
closed on every such case, this module *repairs* the parse: it rewrites each
fused newline to an explicit ``;`` (the separator tree-sitter will not fuse,
adjacency-guarded so it never forms ``;;``) and re-parses the whole command,
preserving compound context (``if``/``then``/``else``, loops). A clean re-parse
decomposes correctly; a rewrite that is not valid shell (``;`` is illegal right
after ``then``/``do``/``else``) still fails closed. The repair only ever turns a
former reject into a correct decomposition — never a fail-open (see the G-4
tests and ``_rewrite_fused_newlines``).

tree-sitter-bash is an optional dependency (``opendaisugi[shell]``). When it is
absent, :func:`decompose_command` returns ``ok=False`` with a clear reason, and
the verifier treats an opted-in-but-unparseable command as a hard reject
(fail-closed on a missing capability) rather than silently accepting it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from functools import lru_cache

from opendaisugi import conformance

# Redirect operators by shell-level effect. Membership decides whether the
# literal destination lands in ``writes`` or ``reads``; any operator in
# neither set (bash keeps growing) fails closed.
_WRITE_REDIRECT_OPS = frozenset({">", ">>", "&>", "&>>", ">|", ">&"})
_READ_REDIRECT_OPS = frozenset({"<", "<&"})
# FD closes touch no path at all.
_FD_CLOSE_OPS = frozenset({">&-", "<&-"})


@dataclass(frozen=True)
class Decomposition:
    """Result of decomposing one shell command.

    ``ok`` is True only when every executing head is literal and every redirect
    target is a literal path (or a pathless fd operation). ``heads`` is then
    every literal command head in order — including heads from inside
    substitutions — and ``commands`` the full source text of each simple
    command, so the caller can re-verify each one through the ordinary
    single-command path (head allowlist, interpreter policy, …). ``reads`` and
    ``writes`` are the literal redirect targets, for the caller's file-scope
    checks. When ``ok`` is False, ``reason`` says why (surfaced in the
    violation).
    """

    ok: bool
    heads: tuple[str, ...] = ()
    commands: tuple[str, ...] = ()
    reads: tuple[str, ...] = ()
    writes: tuple[str, ...] = ()
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


def _literal_text(node) -> str | None:
    """The literal string a destination node denotes, or None if not literal.

    Literal means the path is fully known before execution: a plain ``word``, a
    double-quoted ``string`` whose only content is ``string_content`` (no
    embedded expansion/substitution), or a ``raw_string``. Everything else —
    ``simple_expansion``, ``expansion``, ``concatenation``, a string with
    embedded parts — is runtime-dependent and returns None.
    """
    if node.type == "word":
        return node.text.decode("utf-8", "replace")
    if node.type == "raw_string":
        return node.text.decode("utf-8", "replace")[1:-1]
    if node.type == "string":
        parts = [c for c in node.children if c.type not in ('"',)]
        if all(c.type == "string_content" for c in parts):
            return "".join(c.text.decode("utf-8", "replace") for c in parts)
    return None


def _classify_file_redirect(node) -> tuple[str | None, str | None, str | None]:
    """Classify one ``file_redirect`` node.

    Returns ``(read_path, write_path, reject_reason)`` — exactly one of the
    three is non-None, except the pathless-safe case (all None): an fd
    duplication (``2>&1`` — destination is a ``number``) or an fd close
    (``2>&-``).
    """
    operator: str | None = None
    destination = None
    for child in node.children:
        if child.type == "file_descriptor":
            continue
        if operator is None:
            operator = child.type
            continue
        destination = child
        break

    if operator in _FD_CLOSE_OPS and destination is None:
        return None, None, None
    if operator is None or destination is None:
        return (
            None,
            None,
            f"unrecognized shell redirection ({node.text.decode('utf-8', 'replace')!r})",
        )
    if destination.type == "number":
        # ``2>&1`` / ``>&2`` / ``<&0`` — duplicating an fd touches no path.
        if operator in (">&", "<&"):
            return None, None, None
        return (
            None,
            None,
            f"unrecognized shell redirection ({node.text.decode('utf-8', 'replace')!r})",
        )

    path = _literal_text(destination)
    if path is None:
        return (
            None,
            None,
            f"non-literal redirect target ({destination.text.decode('utf-8', 'replace')!r})",
        )
    if operator in _WRITE_REDIRECT_OPS:
        return None, path, None
    if operator in _READ_REDIRECT_OPS:
        return path, None, None
    return None, None, f"unrecognized shell redirection operator ({operator!r})"


# Node types whose spans may legitimately contain a raw newline (quoted text,
# heredoc bodies, and substitutions). A newline anywhere else inside a single
# ``command`` node is not part of that command in bash — it is a statement
# terminator the GLR parser fused into one node (see ``_command_has_bare_newline``).
_MULTILINE_LEGAL_NODES = frozenset(
    {
        "string",
        "raw_string",
        "ansi_c_string",
        "translated_string",
        "command_substitution",
        "process_substitution",
        "arithmetic_expansion",
        "heredoc_body",
        "heredoc_redirect",
    }
)


def _bare_newline_offsets(node, src: bytes) -> list[int]:
    """Byte offsets of statement-terminator newlines fused into a ``command``.

    tree-sitter-bash 0.25.1 can parse ``c1\nd1`` as ONE command (name ``c1``,
    argument ``d1``) without flagging ``has_error`` — the newline, which bash
    treats as a command terminator, is swallowed into the command span. Left
    unchecked, ``d1`` executes but never faces the allowlist (fail-OPEN). This
    returns every raw ``\n``/``\r`` offset inside the command's own span that is
    not within a multiline-legal child (quote/heredoc/substitution) and not a
    backslash line continuation — i.e. the exact points where the fused
    statements must be cut apart. Empty list means no fusion.
    """
    protected: list[tuple[int, int]] = []

    def collect(m) -> None:
        if m.type in _MULTILINE_LEGAL_NODES:
            protected.append((m.start_byte, m.end_byte))
            return
        for child in m.children:
            collect(child)

    collect(node)
    offsets: list[int] = []
    for i in range(node.start_byte, node.end_byte):
        if src[i : i + 1] not in (b"\n", b"\r"):
            continue
        if any(a <= i < b for a, b in protected):
            continue
        if i > 0 and src[i - 1 : i] == b"\\":  # line continuation
            continue
        offsets.append(i)
    return offsets


def _command_has_bare_newline(node, src: bytes) -> bool:
    """True when a ``command`` node carries a fused statement-boundary newline.

    Thin predicate over :func:`_bare_newline_offsets`; kept for the probe/test
    call sites that only ask *whether* fusion occurred, not where.
    """
    return bool(_bare_newline_offsets(node, src))


def _all_fused_newline_offsets(root, src: bytes) -> list[int]:
    """Union of fused-newline offsets across every ``command`` node in a tree."""
    offsets: set[int] = set()

    def walk(node) -> None:
        if node.type == "command":
            offsets.update(_bare_newline_offsets(node, src))
        for child in node.children:
            walk(child)

    walk(root)
    return sorted(offsets)


def _comment_end_offsets(root) -> set[int]:
    """End offsets of ``comment`` nodes — the newline that terminates each ``#``.

    A fused newline at one of these positions must NOT be rewritten to ``;``: a
    ``#`` comment runs to the newline, so replacing that newline would pull the
    following statement into the comment and hide its head — a fail-OPEN that no
    ``has_error`` check catches (``head;# note;sed`` is valid shell, just with
    ``sed`` commented out). Keep such newlines as newlines instead. Found by the
    Go and Lean clients both decomposing more heads than the oracle on a
    comment-bearing multi-block script.
    """
    ends: set[int] = set()

    def walk(node) -> None:
        if node.type == "comment":
            ends.add(node.end_byte)
        for child in node.children:
            walk(child)

    walk(root)
    return ends


def _rewrite_fused_newlines(
    src: bytes, offsets: list[int], comment_ends: frozenset[int] = frozenset()
) -> bytes:
    """Replace each fused newline with ``;`` — the separator tree-sitter won't
    fuse — but with a space where a ``;`` would abut an existing separator, and
    left as a newline where it terminates a ``#`` comment.

    A newline after a trailing ``;`` (or a blank line: two fused newlines in a
    row) would otherwise yield ``;;``, which is a ``case``-only token and a
    syntax error anywhere else — tree-sitter accepts it leniently, real bash
    does not. Emitting a space in that position keeps the statements separated
    by the separator already there, so the rewrite stays valid shell. A newline
    ending a comment (``comment_ends``) is kept verbatim so the comment's own
    terminator survives and the next statement is not swallowed into it.
    """
    cut = set(offsets)
    out = bytearray()
    for i in range(len(src)):
        if i in cut and i not in comment_ends:
            prev = next((c for c in reversed(out) if c not in (0x20, 0x09)), None)
            out.append(0x20 if prev in (ord(";"), ord("&"), ord("|")) else ord(";"))
        else:
            out.append(src[i])
    return bytes(out)


# Splitting at every bare newline strictly reduces the newline count, so a
# fragment can never re-fuse — this bound only trips on a logic bug, loudly.
_MAX_FUSION_SPLIT_DEPTH = 64


def decompose_command(command: str) -> Decomposition:
    """Decompose ``command`` into literal heads and redirect effects, or reject.

    See the module docstring for the exact fail-closed rules. With
    ``OPENDAISUGI_CONFORMANCE_RECORD`` set, every decomposition of a real
    command is appended to the conformance corpus — except parser-unavailable
    results, which describe this environment, not the command.
    """
    result = _decompose(command)
    if result.reason != _PARSER_MISSING_REASON:
        conformance.record_decompose(command, result)
    return result


_PARSER_MISSING_REASON = "shell decomposition parser not installed (opendaisugi[shell])"


def _decompose(command: str, _depth: int = 0) -> Decomposition:
    parser = _load_parser()
    if parser is None:
        return Decomposition(False, reason=_PARSER_MISSING_REASON)

    src = command.encode("utf-8")
    root = parser.parse(src).root_node
    if root.has_error:
        return Decomposition(False, reason="malformed shell (parse error)")

    # G-4 repair: tree-sitter-bash 0.25.1 can fuse a newline statement boundary
    # into a single ``command`` node, so a later head would execute unchecked
    # (fail-OPEN). A correct POSIX parser (mvdan/sh) never fuses. Recover the
    # true decomposition by rewriting each fused bare newline to an explicit
    # ``;`` — the unambiguous separator tree-sitter will not fuse — and
    # re-parsing the WHOLE command, so compound context (if/then/else, loops)
    # is preserved. If the rewrite does not parse cleanly (``;`` is invalid
    # right after ``then``/``do``/``else``), the fusion was genuinely ambiguous
    # to resolve locally: fail closed, exactly as before.
    fused = _all_fused_newline_offsets(root, src)
    if fused:
        if _depth >= _MAX_FUSION_SPLIT_DEPTH:
            raise AssertionError(  # provably unreachable — loud if not
                "fusion-repair recursion exceeded; a rewrite re-fused, which "
                "should be impossible (each pass replaces newlines with ';')"
            )
        rewritten = _rewrite_fused_newlines(
            src, fused, frozenset(_comment_end_offsets(root))
        )
        if rewritten == src:
            # Every fused newline terminates a comment; none can be rewritten to
            # ``;`` without burying the following statement in the comment. The
            # fusion can't be resolved locally — fail closed, don't hide a head.
            return Decomposition(
                False,
                reason="ambiguous shell (bare newline inside command — parser statement fusion)",
            )
        if parser.parse(rewritten).root_node.has_error:
            return Decomposition(
                False,
                reason="ambiguous shell (bare newline inside command — parser statement fusion)",
            )
        return _decompose(rewritten.decode("utf-8", "replace"), _depth + 1)

    heads: list[str] = []
    commands: list[str] = []
    reads: list[str] = []
    writes: list[str] = []
    reason: str | None = None

    def visit(node) -> None:
        nonlocal reason
        if reason is not None:
            return
        if node.is_missing:
            reason = "malformed shell (missing token)"
            return
        if node.type == "file_redirect":
            read_path, write_path, reject = _classify_file_redirect(node)
            if reject is not None:
                reason = reject
            elif read_path is not None:
                reads.append(read_path)
            elif write_path is not None:
                writes.append(write_path)
            # Destination fully classified — nothing below needs walking, and a
            # rejected destination must not ALSO contribute heads.
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
            heads.append(head)
            commands.append(node.text.decode("utf-8", "replace"))
            # Fall through: arguments may hold substitutions whose inner
            # commands must surface too.
        for child in node.children:
            visit(child)
            if reason is not None:
                return

    visit(root)

    if reason is not None:
        return Decomposition(False, reason=reason)
    if not heads:
        return Decomposition(False, reason="no command heads found")
    return Decomposition(
        True,
        heads=tuple(heads),
        commands=tuple(commands),
        reads=tuple(reads),
        writes=tuple(writes),
    )


def parser_available() -> bool:
    """Is the bash grammar actually usable on this box?

    Answers the question the CLI needs before writing an envelope that opts
    into decomposition: with the field ``True`` and the parser absent, every
    compound command is REJECTED (fail-closed on a missing capability), which
    looks to an operator like the opt-in made things worse. Probes by running
    a real decomposition rather than by importing, so a grammar that installs
    but fails to load is reported honestly.
    """
    return decompose_command("a && b").ok

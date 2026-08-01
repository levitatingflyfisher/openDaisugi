"""G-4 (found by the Go + Lean client ports): tree-sitter statement fusion.

tree-sitter-bash 0.25.1's GLR parser sometimes fuses a newline-separated
statement boundary into a single ``command`` node — e.g. ``c1\nd1`` parses
as command ``c1`` with argument ``d1`` — and does NOT set ``has_error``. Left
unrepaired, ``d1`` would execute without ever facing the allowlist: fail-OPEN.

A correct POSIX parser (mvdan/sh, the Go client) never fuses, so the earlier
fail-closed rejection was refusing perfectly valid scripts. This repair instead
SPLITS the fused command node at each bare (unquoted, unescaped, non-heredoc)
newline and re-decomposes each fragment, recovering every hidden head. The
reference is now correct, not merely safe. Legitimate multiline commands
(quoted newline, line continuation, heredoc body, command substitution) carry
no bare newline and are unaffected.
"""

from opendaisugi.shell_decompose import decompose_command


def test_fusion_across_newline_is_split_into_all_heads():
    # tree-sitter fuses 'c1\nd1' into one node. Before the split repair the
    # oracle failed closed ("bare newline…"); before *that* it dropped d1
    # silently (fail-open). It must now recover every head, in order.
    d = decompose_command("a1 | b1 | c1\nd1 | e1 > f.txt")
    assert d.ok, d.reason
    assert d.heads == ("a1", "b1", "c1", "d1", "e1")
    assert d.writes == ("f.txt",)


def test_multiline_script_recovers_every_head_not_just_the_survivors():
    d = decompose_command("echo one && grep p f | grep q | head\necho two | ls")
    assert d.ok, d.reason
    # The whole point: 'echo two' and 'ls' after the fused boundary survive.
    for h in ("echo", "grep", "head", "ls"):
        assert h in d.heads


def test_quoted_newline_argument_is_not_a_fusion():
    d = decompose_command('echo "line one\nline two"')
    assert d.ok
    assert d.heads == ("echo",)


def test_line_continuation_is_not_a_fusion():
    d = decompose_command("echo foo \\\nbar")
    assert d.ok
    assert d.heads == ("echo",)


def test_heredoc_body_newlines_are_not_a_fusion():
    d = decompose_command("cat <<EOF\nhello\nworld\nEOF")
    assert d.ok
    assert d.heads == ("cat",)


def test_command_substitution_spanning_lines_surfaces_inner_heads():
    d = decompose_command("x=$(echo a\necho b)")
    assert d.ok
    assert "echo" in d.heads


def test_fused_fragment_with_nonliteral_head_still_fails_closed():
    # A fragment that genuinely can't be authorized (a $VAR head) must still
    # fail closed after the split — the repair recovers heads, it never
    # rubber-stamps a non-literal one.
    d = decompose_command("ok1 | ok2 | ok3\n$MYSTERY arg")
    assert not d.ok
    assert "non-literal" in d.reason


def test_rewrite_does_not_create_double_semicolon():
    # A fused newline after a trailing ';' (or a blank line) must not become
    # ';;' — that is a case-only token and a syntax error elsewhere, which
    # tree-sitter accepts leniently but real bash rejects. Found by bash -n
    # over the corpus's accepted rewrites.
    from opendaisugi.shell_decompose import _rewrite_fused_newlines

    # 'a;\nb'  — offset 2 is the fused newline, preceded by ';'
    assert _rewrite_fused_newlines(b"a;\nb", [2]) == b"a; b"
    # 'a\n\nb' — blank line: two fused newlines in a row must not double up
    assert _rewrite_fused_newlines(b"a\n\nb", [1, 2]) == b"a; b"
    # ordinary case still gets a real separator
    assert _rewrite_fused_newlines(b"a\nb", [1]) == b"a;b"


def test_compound_spanning_fusion_fails_closed_not_wrong():
    # When tree-sitter fuses across an if/then/else boundary, rewriting the
    # bare newlines to ';' yields invalid shell ('then;'/'else;'), so we fail
    # CLOSED rather than emit a bogus 'else' head. Safe, not silently wrong.
    d = decompose_command(
        'if [ -d x ]; then\ngrep a b | c | d\ne > f\nelse\ng\nfi'
    )
    if not d.ok:
        assert "fusion" in d.reason or "malformed" in d.reason
    else:
        assert "else" not in d.heads and "fi" not in d.heads


def test_fusion_across_a_comment_does_not_hide_the_post_comment_head():
    # tree-sitter fused this whole script into one span. Rewriting the newline
    # that TERMINATES the '# restore' comment to ';' would bury 'sed | tail' and
    # 'cmd' inside the comment — a fail-OPEN. The comment-terminating newline
    # must stay a newline so every post-comment head survives for checking.
    # Found by the Go AND Lean clients both decomposing more heads than the
    # oracle on a comment-bearing multi-block script.
    d = decompose_command("grep a b | grep c | head\n# restore\nsed y | tail\ncmd z > out")
    assert d.ok, d.reason
    for h in ("grep", "head", "sed", "tail", "cmd"):
        assert h in d.heads, f"{h} hidden by the comment — fail-open"
    assert "out" in d.writes

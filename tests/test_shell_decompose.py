"""Tests for the fail-closed compound-shell decomposer (opendaisugi[shell]).

ADR-0010 established decomposition; ADR-0014 extended it from *structure only*
(heads) to *shell-level effects*: literal redirect targets are returned as
``reads``/``writes`` for the verifier to check against the envelope's file
scopes, and substitution bodies are recursively decomposed instead of
rejected. Everything the grammar cannot prove literal still fails closed.
"""

from __future__ import annotations

import pytest

pytest.importorskip("tree_sitter_bash")

from opendaisugi.shell_decompose import decompose_command

# --- safe compound commands decompose into their simple-command heads ----------


def test_pipe_yields_each_head():
    d = decompose_command("grep -r TODO src | head -20")
    assert d.ok
    assert d.heads == ("grep", "head")
    assert d.reads == () and d.writes == ()


def test_and_chain_yields_each_head():
    d = decompose_command("cd /mnt/x && sed -n '1,40p' f")
    assert d.ok
    assert d.heads == ("cd", "sed")


def test_newline_separated_yields_each_head():
    d = decompose_command("cd x\necho hi\nsed -n 1,5p f")
    assert d.ok
    assert d.heads == ("cd", "echo", "sed")


def test_semicolon_exposes_every_head_for_allowlist_check():
    # The point of decomposition: the caller sees BOTH heads, so 'rm' can be
    # rejected against the allowlist instead of the whole thing sneaking in.
    d = decompose_command("git status; rm -rf /")
    assert d.ok
    assert d.heads == ("git", "rm")


# --- redirections: literal targets become checkable reads/writes (ADR-0014) ----


def test_output_redirect_reports_write_target():
    d = decompose_command("sort x > out.txt")
    assert d.ok
    assert d.heads == ("sort",)
    assert d.writes == ("out.txt",)
    assert d.reads == ()


def test_append_and_stderr_redirects_report_all_write_targets():
    d = decompose_command("make >> build.log 2> err.log")
    assert d.ok
    assert d.writes == ("build.log", "err.log")


def test_input_redirect_reports_read_target():
    d = decompose_command("wc -l < in.txt")
    assert d.ok
    assert d.reads == ("in.txt",)
    assert d.writes == ()


def test_cron_dropper_decomposes_with_its_write_exposed():
    # The canonical ADR-0010 example: harmless head, dangerous write. It now
    # decomposes — and the write target is exposed so verify() can reject it
    # against the envelope's file_write scope instead of rejecting the shape.
    d = decompose_command("echo x > /etc/cron.d/pwn")
    assert d.ok
    assert d.heads == ("echo",)
    assert d.writes == ("/etc/cron.d/pwn",)


def test_fd_dup_and_close_are_safe_and_report_no_paths():
    for cmd in ("prog 2>&1", "prog >&2", "prog 2>&-"):
        d = decompose_command(cmd)
        assert d.ok, cmd
        assert d.reads == () and d.writes == (), cmd


def test_ampersand_and_clobber_and_fd_word_redirects_are_writes():
    d = decompose_command("prog &> all.log")
    assert d.ok and d.writes == ("all.log",)
    d = decompose_command("prog >| clobber.log")
    assert d.ok and d.writes == ("clobber.log",)
    d = decompose_command("prog >& both.log")
    assert d.ok and d.writes == ("both.log",)


def test_quoted_redirect_targets_are_literal():
    d = decompose_command('echo hi > "quoted path.txt"')
    assert d.ok
    assert d.writes == ("quoted path.txt",)
    d = decompose_command("echo hi > 'single.txt'")
    assert d.ok
    assert d.writes == ("single.txt",)


def test_variable_redirect_target_rejected():
    d = decompose_command("echo hi > $OUT")
    assert not d.ok
    assert "redirect" in d.reason


def test_concatenated_redirect_target_rejected():
    d = decompose_command("echo hi > out$N.txt")
    assert not d.ok
    assert "redirect" in d.reason


def test_substitution_redirect_target_rejected():
    d = decompose_command("echo hi > $(mktemp)")
    assert not d.ok
    assert "redirect" in d.reason


def test_redirect_inside_compound_chain():
    d = decompose_command("grep foo src/*.py | sort > hits.txt && wc -l hits.txt")
    assert d.ok
    assert d.heads == ("grep", "sort", "wc")
    assert d.writes == ("hits.txt",)


# --- substitutions: bodies are recursively decomposed (ADR-0014) ---------------


def test_command_substitution_yields_inner_head():
    d = decompose_command("echo $(git rev-parse HEAD)")
    assert d.ok
    assert d.heads == ("echo", "git")
    # Both the outer and the inner command are returned for re-verification.
    assert "git rev-parse HEAD" in d.commands


def test_command_substitution_dangerous_inner_head_is_exposed():
    # The old blanket rejection is replaced by exposure: 'rm' is now visible
    # to the allowlist check instead of the whole command being unshapeable.
    d = decompose_command("echo $(rm -rf /) ok")
    assert d.ok
    assert d.heads == ("echo", "rm")


def test_backtick_substitution_yields_inner_head():
    d = decompose_command("echo `date`")
    assert d.ok
    assert d.heads == ("echo", "date")


def test_nested_substitution_yields_all_heads():
    d = decompose_command("echo $(echo $(date))")
    assert d.ok
    assert d.heads == ("echo", "echo", "date")


def test_process_substitution_yields_inner_heads():
    d = decompose_command("diff <(sort a) <(sort b)")
    assert d.ok
    assert d.heads == ("diff", "sort", "sort")


def test_assignment_substitution_yields_inner_head():
    d = decompose_command("X=$(date) prog --flag")
    assert d.ok
    assert set(d.heads) == {"prog", "date"}


def test_substitution_as_head_still_rejected():
    d = decompose_command("$(which python) script.py")
    assert not d.ok
    assert "non-literal" in d.reason


# --- heredocs / herestrings: input data, walked for substitutions --------------


def test_unquoted_heredoc_exposes_embedded_substitution_head():
    d = decompose_command("cat <<EOF\nhello $(date)\nEOF")
    assert d.ok
    assert d.heads == ("cat", "date")
    assert d.reads == () and d.writes == ()


def test_quoted_heredoc_body_is_literal_data():
    d = decompose_command("cat <<'EOF'\nliteral $(date)\nEOF")
    assert d.ok
    assert d.heads == ("cat",)


def test_herestring_exposes_embedded_substitution_head():
    d = decompose_command('grep foo <<< "some $(date) text"')
    assert d.ok
    assert d.heads == ("grep", "date")


# --- still fail-closed ---------------------------------------------------------


def test_malformed_rejected():
    d = decompose_command("cat f |")
    assert not d.ok
    assert "malformed" in d.reason


def test_non_literal_head_rejected():
    d = decompose_command("$CMD foo")
    assert not d.ok
    assert "non-literal" in d.reason


def test_parameter_expansion_head_rejected():
    d = decompose_command("${x:-rm} -rf /")
    assert not d.ok
    assert "non-literal" in d.reason


def test_wrappers_decompose_for_verify_layer_recursion():
    # ADR-0014: wrappers are no longer blanket-rejected here. The verifier's
    # interpreter layer extracts and recursively checks their payloads, so the
    # decomposer's job is only to surface them as ordinary simple commands.
    d = decompose_command("sh -c 'rm -rf /' && ls")
    assert d.ok
    assert d.heads == ("sh", "ls")
    d = decompose_command('eval "$s"')
    assert d.ok
    assert d.heads == ("eval",)

"""Tests for the fail-closed compound-shell decomposer (opendaisugi[shell])."""

from __future__ import annotations

import pytest

pytest.importorskip("tree_sitter_bash")

from opendaisugi.shell_decompose import decompose_command

# --- safe compound commands decompose into their simple-command heads ----------


def test_pipe_yields_each_head():
    d = decompose_command("grep -r TODO src | head -20")
    assert d.ok
    assert d.heads == ("grep", "head")


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


# --- unsafe constructs are rejected (fail-closed) ------------------------------


def test_command_substitution_rejected():
    d = decompose_command("echo $(rm -rf /) ok")
    assert not d.ok
    assert "substitution" in d.reason


def test_process_substitution_rejected():
    d = decompose_command("diff <(sort a) <(sort b)")
    assert not d.ok
    assert "substitution" in d.reason


def test_redirection_rejected():
    # head 'echo' is harmless but the redirect writes a file the file_write
    # scope never authorized — must reject, not trust the head.
    d = decompose_command("echo x > /etc/cron.d/pwn")
    assert not d.ok
    assert "redirect" in d.reason


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


def test_command_taking_wrapper_rejected():
    d = decompose_command('eval "$s"')
    assert not d.ok
    assert "wrapper" in d.reason


def test_sh_dash_c_wrapper_rejected():
    d = decompose_command("sh -c 'rm -rf /'")
    assert not d.ok
    assert "wrapper" in d.reason

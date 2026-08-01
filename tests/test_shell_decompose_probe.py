"""The capability probe behind the honest `--allow-shell-decomposition` warning.

Deliberately NOT behind ``importorskip``: the probe's whole job is to answer
"is the parser here?", so it must be exercised on a box where the extra is
absent as well as one where it is present.
"""

from __future__ import annotations

from opendaisugi.shell_decompose import parser_available


def test_probe_returns_a_bool_either_way():
    assert isinstance(parser_available(), bool)


def test_probe_is_false_when_the_grammar_cannot_load(monkeypatch):
    monkeypatch.setattr("opendaisugi.shell_decompose._load_parser", lambda: None)
    assert parser_available() is False


def test_probe_agrees_with_a_real_decomposition():
    from opendaisugi.shell_decompose import decompose_command

    assert parser_available() == decompose_command("echo a && echo b").ok

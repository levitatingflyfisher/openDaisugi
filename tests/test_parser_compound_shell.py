"""Parser decomposes compound shell into atomic ShellSteps (v0.18 L6)."""
from opendaisugi.parsers.claude_code import _split_compound_shell, _extract_step_maybe_multiple


def test_and_compound_splits_into_two():
    parts = _split_compound_shell("ls dir && cat dir/file")
    assert parts == ["ls dir", "cat dir/file"]


def test_semicolon_compound_splits():
    parts = _split_compound_shell("echo a; echo b; echo c")
    assert parts == ["echo a", "echo b", "echo c"]


def test_or_compound_splits():
    parts = _split_compound_shell("test -f foo || touch foo")
    assert parts == ["test -f foo", "touch foo"]


def test_mixed_operators_split():
    parts = _split_compound_shell("a && b; c || d")
    assert parts == ["a", "b", "c", "d"]


def test_operators_inside_single_quotes_not_split():
    parts = _split_compound_shell("echo 'a && b'")
    assert parts == ["echo 'a && b'"]


def test_operators_inside_double_quotes_not_split():
    parts = _split_compound_shell('echo "a; b"')
    assert parts == ['echo "a; b"']


def test_simple_command_single_part():
    parts = _split_compound_shell("ls -la")
    assert parts == ["ls -la"]


def test_empty_input_returns_empty_list():
    assert _split_compound_shell("") == []


def test_dollar_paren_left_intact_as_single_part():
    parts = _split_compound_shell("cat $(find . -name foo)")
    assert parts == ["cat $(find . -name foo)"]


def test_backtick_left_intact_as_single_part():
    parts = _split_compound_shell("echo `date`")
    assert len(parts) == 1


def test_extract_step_maybe_multiple_with_compound_bash():
    tu = {"name": "Bash", "id": "t1", "input": {"command": "ls && cat foo"}}
    steps = _extract_step_maybe_multiple(tu)
    assert len(steps) == 2
    assert all(s["type"] == "shell" for s in steps)
    assert steps[0]["command"] == "ls"
    assert steps[1]["command"] == "cat foo"


def test_extract_step_maybe_multiple_with_simple_bash():
    tu = {"name": "Bash", "id": "t1", "input": {"command": "ls"}}
    steps = _extract_step_maybe_multiple(tu)
    assert len(steps) == 1


def test_extract_step_maybe_multiple_with_non_bash():
    tu = {"name": "Read", "id": "t1", "input": {"file_path": "/tmp/foo"}}
    steps = _extract_step_maybe_multiple(tu)
    assert len(steps) == 1
    assert steps[0]["type"] == "file_read"


def test_extract_step_maybe_multiple_with_unknown_tool_returns_empty():
    tu = {"name": "SomeUnknownTool", "id": "t1", "input": {}}
    assert _extract_step_maybe_multiple(tu) == []

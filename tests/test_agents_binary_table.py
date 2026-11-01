"""AGENTS.md says which commands each binary carries. The dispatch lists in
the Go and Rust sources are the truth; this test fails when the table drifts."""

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
OLD_VERB = "gate"  # names-ok: the old verb that refuses
RETIRED = {"help", OLD_VERB}  # the help verb, and the old name


def go_commands() -> set[str]:
    src = (REPO / "clients/go/internal/cli/cli.go").read_text()
    start = src.index("\tswitch args[0] {")
    end = src.index("\n\t}\n", start)
    found = re.findall(r'^\s*case "([a-z][a-z-]*)":', src[start:end], re.M)
    return set(found) - RETIRED


def rust_commands() -> set[str]:
    src = (REPO / "clients/rust/src/cli/mod.rs").read_text()
    return set(re.findall(r'^\s*"([a-z][a-z-]*)" => return self\.', src, re.M)) - RETIRED


def row(prefix: str) -> set[str]:
    text = (REPO / "AGENTS.md").read_text()
    line = next(ln for ln in text.splitlines() if ln.startswith(prefix))
    return set(re.findall(r"`([a-z][a-z-]*)`", line.split("|")[3])) - {"daisugi", "warden audit"}


def test_the_go_row_matches_the_dispatch_list():
    row_cmds = row("| `daisugi` (Go) |")
    # The row also names verbs inside commands (`dashboard`, `pathways`); those are
    # top-level commands too, so equality holds when no other word is quoted.
    assert go_commands() <= row_cmds
    assert row_cmds - go_commands() <= {"audit", "replay", "import"}, row_cmds - go_commands()


def test_the_rust_row_matches_the_dispatch_list():
    row_cmds = row("| `daisugi` (Rust) |")
    assert rust_commands() <= row_cmds
    assert row_cmds - rust_commands() <= {"audit"}, row_cmds - rust_commands()


def test_both_binaries_dispatch_the_same_commands():
    assert go_commands() == rust_commands()


def test_the_table_no_longer_says_only_python_has_them():
    assert "Only the Python package has" not in (REPO / "AGENTS.md").read_text()

from __future__ import annotations

from pathlib import Path

PINS = (
    Path(__file__).parents[2] / "src" / "opendaisugi" / "harness_pi" / "extension" / "PINS.md"
).read_text(encoding="utf-8")


def test_pins_cite_both_source_urls():
    assert (
        "raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/extensions.md"
        in PINS
    )
    assert (
        "raw.githubusercontent.com/earendil-works/pi/main/packages/coding-agent/docs/rpc.md" in PINS
    )


def test_pins_record_the_tool_call_return_shape():
    assert "block: true" in PINS
    assert "reason?" in PINS
    assert "terminate?" in PINS
    # pi's own fail-safe: a thrown error inside tool_call also blocks. This
    # is a second line of defense behind the extension's explicit checks.
    assert "tool_call errors block the tool (fail-safe)" in PINS


def test_pins_record_lowercase_builtin_tool_names():
    for name in ("bash", "read", "write", "edit"):
        assert f'"{name}"' in PINS


def test_pins_record_the_rpc_framing_rule():
    assert "LF" in PINS and "only" in PINS  # LF-delimited JSONL, not a generic line reader


def test_pins_record_switch_session_takes_a_file_path_not_a_bare_id():
    assert "sessionPath" in PINS
    assert "sessionFile" in PINS


def test_pins_record_the_dialog_method_set():
    for method in ("select", "confirm", "input", "editor"):
        assert f'"{method}"' in PINS
    # confirm carries title AND message; select/input/editor carry title only.
    # No field is literally named "prompt".
    assert "no field is literally named" in PINS

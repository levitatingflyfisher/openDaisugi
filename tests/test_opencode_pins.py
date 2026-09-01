"""The OpenCode API pins are a static fact file. This test reads the
committed file only. It never needs opencode or the network."""

from __future__ import annotations

from pathlib import Path

PINS = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "adapters"
    / "opencode"
    / "PINS.md"
)


def test_pins_file_exists():
    assert PINS.exists(), "write PINS.md, then check it with scripts/opencode_discover.py"


def test_pins_names_the_version_and_the_date():
    text = PINS.read_text(encoding="utf-8")
    assert "`1.18.32`" in text
    assert "2026-09-23" in text


def test_pins_records_the_server_surface():
    text = PINS.read_text(encoding="utf-8")
    for fact in (
        "OPENCODE_SERVER_PASSWORD",
        "OPENCODE_SERVER_USERNAME",
        "GET /global/health",
        "POST /session",
        "GET /session/{id}",
        "POST /session/{id}/prompt_async",
        "POST /permission/{requestID}/reply",
        "GET /event",
        '{"type": "text", "text": "..."}',
        "opencode server listening on http://127.0.0.1:<port>",
    ):
        assert fact in text, f"missing pinned fact: {fact!r}"


def test_pins_records_the_tool_ids_and_their_argument_keys():
    text = PINS.read_text(encoding="utf-8")
    for tool_id in (
        "bash",
        "read",
        "write",
        "edit",
        "glob",
        "grep",
        "webfetch",
        "websearch",
        "apply_patch",
    ):
        assert f"`{tool_id}`" in text, f"missing pinned tool id: {tool_id!r}"
    for key in ("filePath", "workdir", "patchText"):
        assert key in text


def test_pins_records_the_permission_ask_caveat():
    text = PINS.read_text(encoding="utf-8")
    assert "permission.asked" in text
    assert "9229" in text or "7006" in text


def test_pins_records_the_plugin_hook_keys():
    text = PINS.read_text(encoding="utf-8")
    for hook_key in ("tool.execute.before", "permission.ask", "event", "tool.execute.after"):
        assert f"`{hook_key}`" in text, f"missing pinned plugin hook key: {hook_key!r}"


def test_pins_records_how_a_plugin_file_is_loaded():
    """The gate plugin's one-export rule rests on this fact."""
    text = PINS.read_text(encoding="utf-8")
    assert "calls every distinct exported value as a plugin function" in text
    assert "{plugin,plugins}/*.{ts,js}" in text

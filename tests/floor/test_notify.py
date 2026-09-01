"""notify_cmd: the operator's own command, run when a pane's merged state
becomes blocked.

Never a service, never a relay we chose (master spec section 5.6). The trigger
is the merged state: an event whose effective state reads working never
notifies, even one that started as a blocked ask whose deadline has passed.
Debounced per pane so a flapping agent cannot turn one block into forty phone
buzzes. Wrapped so a broken command never takes the floor down.
"""

from __future__ import annotations

import json
import logging
import sys
import time
from pathlib import Path

from opendaisugi.floor import Ask, PaneStateEvent, effective_state
from opendaisugi.floor.notify import EXAMPLE_CMD, Notifier


def _ev(pane="w1:p1", state="blocked", ts=1.0, source="process", ask=None) -> PaneStateEvent:
    return PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state=state,
        source=source,
        ts=ts,
        pane=pane,
        ask=ask,
    )


def _capture_script(tmp_path: Path, out: Path) -> str:
    """Write a script that copies its stdin to out. Returns the command line."""
    stub = tmp_path / "capture.py"
    stub.write_text(
        "import pathlib\nimport sys\npathlib.Path(sys.argv[1]).write_text(sys.stdin.read())\n"
    )
    return f"{sys.executable} {stub} {out}"


def _append_script(tmp_path: Path, target: Path) -> str:
    """Write a script that appends one 'x' to target each run. Returns the command line."""
    stub = tmp_path / "append.py"
    stub.write_text(
        "import pathlib\nimport sys\n"
        "p = pathlib.Path(sys.argv[1])\n"
        "p.write_text(p.read_text() + 'x')\n"
    )
    return f"{sys.executable} {stub} {target}"


def test_no_command_means_no_run():
    assert Notifier(None).notify(_ev()) is False


def test_the_command_receives_a_small_json_payload_on_stdin(tmp_path):
    out = tmp_path / "seen.json"
    out.write_text("")
    cmd = _capture_script(tmp_path, out)
    assert Notifier(cmd).notify(_ev()) is True
    payload = json.loads(out.read_text())
    assert payload["state"] == "blocked"
    assert payload["pane"] == "w1:p1"


def test_the_payload_never_carries_the_asks_contents(tmp_path):
    out = tmp_path / "seen.json"
    out.write_text("")
    cmd = _capture_script(tmp_path, out)
    ask = Ask(id="a1", tool="Bash", summary="rm -rf build/ and other secrets", deadline=100.0)
    ev = _ev(source="gate", ask=ask)
    assert Notifier(cmd).notify(ev) is True
    payload = json.loads(out.read_text())
    assert set(payload) == {"pane", "harness", "state", "detail"}


def test_a_second_event_for_the_same_pane_is_debounced(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = _append_script(tmp_path, counter)
    clock = iter([0.0, 1.0, 6.1])
    notifier = Notifier(cmd, debounce_s=5.0, clock=lambda: next(clock))
    assert notifier.notify(_ev()) is True
    assert notifier.notify(_ev()) is False
    assert notifier.notify(_ev()) is True
    assert counter.read_text() == "xx"


def test_debounce_is_per_pane_not_global(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = _append_script(tmp_path, counter)
    clock = iter([0.0, 0.1])
    notifier = Notifier(cmd, debounce_s=5.0, clock=lambda: next(clock))
    assert notifier.notify(_ev(pane="w1:p1")) is True
    assert notifier.notify(_ev(pane="w1:p2")) is True
    assert counter.read_text() == "xx"


def test_a_non_blocked_event_never_notifies(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = _append_script(tmp_path, counter)
    assert Notifier(cmd).notify(_ev(state="working")) is False
    assert counter.read_text() == ""


def test_an_expired_ask_reads_working_and_never_notifies(tmp_path):
    counter = tmp_path / "count"
    counter.write_text("")
    cmd = _append_script(tmp_path, counter)
    ask = Ask(id="a1", tool="Bash", summary="rm -rf build/", deadline=100.0)
    blocked = _ev(source="gate", ask=ask, ts=1.0)
    lapsed = effective_state(blocked, now=101.0)
    assert lapsed.state == "working"
    assert Notifier(cmd).notify(lapsed) is False
    assert counter.read_text() == ""


def test_a_failing_command_never_raises_and_logs_once(caplog):
    with caplog.at_level(logging.WARNING, logger="opendaisugi.floor.notify"):
        assert Notifier("definitely-not-a-real-binary --flag").notify(_ev()) is False
    seen = [r for r in caplog.records if r.name == "opendaisugi.floor.notify"]
    assert len(seen) == 1


def test_an_unparsable_command_string_never_raises_and_logs_once(caplog):
    with caplog.at_level(logging.WARNING, logger="opendaisugi.floor.notify"):
        assert Notifier('sh -c "unbalanced').notify(_ev()) is False
    seen = [r for r in caplog.records if r.name == "opendaisugi.floor.notify"]
    assert len(seen) == 1


def test_a_hanging_command_is_killed_inside_the_budget_and_logs_once(caplog):
    started = time.monotonic()
    cmd = f'{sys.executable} -c "import time;time.sleep(30)"'
    with caplog.at_level(logging.WARNING, logger="opendaisugi.floor.notify"):
        assert Notifier(cmd, timeout_s=0.5).notify(_ev()) is False
    assert time.monotonic() - started < 5.0
    seen = [r for r in caplog.records if r.name == "opendaisugi.floor.notify"]
    assert len(seen) == 1


def test_a_nonzero_exit_still_counts_as_ran_but_logs_once(caplog):
    cmd = f'{sys.executable} -c "raise SystemExit(3)"'
    with caplog.at_level(logging.WARNING, logger="opendaisugi.floor.notify"):
        assert Notifier(cmd).notify(_ev()) is True
    seen = [r for r in caplog.records if r.name == "opendaisugi.floor.notify"]
    assert len(seen) == 1


def test_the_example_command_is_ntfy_and_reads_stdin():
    assert EXAMPLE_CMD.startswith("ntfy publish")
    assert "http" in EXAMPLE_CMD

"""PaneStateEvent (master spec §3.1): every rule as its own test."""

from __future__ import annotations

import json
import re
from pathlib import Path

import pytest

from opendaisugi.floor.events import ERROR_CODES, Ask, PaneStateEvent, effective_state, merge
from tests.floor import hostfacts

FIXTURE_DIR = Path(__file__).resolve().parent / "testdata" / "events"
PROTO_GO = hostfacts.REPO_ROOT / "harness" / "coppice" / "internal" / "proto" / "proto.go"


def _row(**over):
    base = {
        "v": 1,
        "ts": 1000.0,
        "session_id": "s1",
        "harness": "claude-code",
        "state": "idle",
        "source": "manifest",
        "detail": "",
    }
    base.update(over)
    return base


def _row_missing(key):
    row = _row()
    del row[key]
    return row


def test_round_trip_preserves_every_field():
    ev = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        harness_session_id="h1",
        pane="w1:p1",
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1090.0),
        detail="awaiting operator",
    )
    back = PaneStateEvent.from_json(ev.to_json())
    assert back == ev


def test_unknown_state_is_rejected():
    with pytest.raises(ValueError, match="unknown state"):
        PaneStateEvent.from_json(json.dumps(_row(state="napping")))


def test_unknown_source_is_rejected():
    with pytest.raises(ValueError, match="unknown source"):
        PaneStateEvent.from_json(json.dumps(_row(source="ouija")))


def test_manifest_cannot_say_done():
    with pytest.raises(ValueError, match="done"):
        PaneStateEvent.from_json(json.dumps(_row(source="manifest", state="done")))


def test_process_can_say_done():
    ev = PaneStateEvent.from_json(json.dumps(_row(source="process", state="done")))
    assert ev.state == "done"


def test_only_process_and_headless_may_say_done():
    """Master §3.1: 'done comes only from process or headless'. A gate- or
    operator-sourced done would show a live agent as finished — not just
    manifest is barred, every other source is too."""
    for bad_source in ("gate", "operator", "manifest"):
        with pytest.raises(ValueError, match="done"):
            PaneStateEvent.from_json(json.dumps(_row(source=bad_source, state="done")))
    for good_source in ("process", "headless"):
        ev = PaneStateEvent.from_json(json.dumps(_row(source=good_source, state="done")))
        assert ev.state == "done"


def test_detail_over_200_chars_is_rejected():
    with pytest.raises(ValueError, match="200"):
        PaneStateEvent.from_json(json.dumps(_row(detail="x" * 201)))


def test_ask_only_valid_when_blocked():
    row = _row(
        state="working",
        source="gate",
        ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
    )
    with pytest.raises(ValueError, match="blocked"):
        PaneStateEvent.from_json(json.dumps(row))


def test_ask_is_required_when_present_but_incomplete():
    row = _row(state="blocked", source="gate", ask={"id": "t1", "tool": "Bash"})
    with pytest.raises(ValueError, match="ask missing"):
        PaneStateEvent.from_json(json.dumps(row))


def test_from_json_coerces_a_whole_number_ask_deadline_to_a_float():
    """Go's JSON encoder drops the decimal point on a whole-number float, so
    a deadline of 100.0 arrives over the wire as the JSON integer 100. The
    parsed ask must still carry a float, so later code sees one type
    whatever second the deadline happened to land on."""
    row = _row(
        state="blocked",
        source="gate",
        ask={"id": "t1", "tool": "Bash", "summary": "rm -rf", "deadline": 100},
    )
    ev = PaneStateEvent.from_json(json.dumps(row))
    assert isinstance(ev.ask.deadline, float)
    assert ev.ask.deadline == 100.0


def test_missing_required_field_is_rejected():
    row = _row()
    del row["harness"]
    with pytest.raises(ValueError, match="harness"):
        PaneStateEvent.from_json(json.dumps(row))


def test_gate_blocked_holds_until_deadline():
    """A gate 'blocked' hold survives a lower-precedence event; once the
    ask's deadline passes, the hold releases and the NEXT event (gate or
    otherwise) gets through — merge() does not itself synthesize
    'working'. The gate sends that event itself the moment _maybe_ask's
    wait resolves (gate.py, task 3)."""
    current = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1010.0),
    )
    manifest_idle = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1005.0
    )
    assert merge(current, manifest_idle, now=1005.0) is current
    assert merge(current, manifest_idle, now=1011.0) is manifest_idle
    gate_working = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1005.0
    )
    assert merge(current, gate_working, now=1005.0) is gate_working


def test_source_precedence_within_two_seconds():
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=1000.0
    )
    lower = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.5
    )
    assert merge(current, lower, now=1000.5) is current


def test_source_precedence_expires_after_two_seconds():
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=1000.0
    )
    lower = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1002.5
    )
    assert merge(current, lower, now=1002.5) is lower


def test_the_hold_window_uses_current_ts_not_a_receive_clock():
    """Same inputs as Go's TestTheHoldWindowIgnoresASkewedEventTimestamp:
    current is working/gate with ts nine billion seconds in the future,
    incoming is idle/manifest at ts=103, now=103. Go measures the window
    on its own receive clock, so a future-dated ts cannot extend a hold
    there, and its outcome is idle/manifest. This function measures the
    window against current.ts instead, so the same inputs still hold here.
    This is the one test that actually distinguishes the two clocks: every
    other precedence test sets current.ts to a value a receive clock would
    also hold, so the two clocks agree on every other row in this file."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=9_999_999_999.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=103.0
    )
    assert merge(current, incoming, now=103.0) is current


def test_a_headless_stop_is_not_swallowed_by_a_fresh_gate_working():
    """Whole-branch review, Important 2 (ruled): the 2 s precedence window
    must hold back ONLY an incoming `manifest` event. A real fact from
    `headless` arriving a heartbeat after a gate 'working' must apply at
    once — the old precedence comparison (any lower-precedence incoming
    source held back) would have swallowed this idle and kept showing a
    stale 'working' pane."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=100.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="headless", ts=101.2
    )
    assert merge(current, incoming, now=101.2) is incoming


def test_a_manifest_is_still_held_back_within_two_seconds():
    """Same pair as above, but the incoming event is from `manifest` — the
    one source the 2 s window still holds back, since it carries no real
    signal of its own."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=100.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=101.2
    )
    assert merge(current, incoming, now=101.2) is current


def test_gate_blocked_still_holds_against_a_headless_idle():
    """The gate-blocked hold is untouched by the Important 2 fix: it still
    holds against a lower-precedence fact until the ask's deadline passes
    (or a gate event clears it), exactly as before."""
    current = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=100.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=200.0),
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="headless", ts=101.2
    )
    assert merge(current, incoming, now=101.2) is current


def test_an_expired_headless_block_reads_as_working():
    """effective_state's expiry is no longer restricted to source == 'gate'
    (Important 2, ruled): a headless-sourced 'blocked' with a passed ask
    deadline reads as 'working' too."""
    blocked = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="headless",
        ts=100.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=190.0),
    )
    resolved = effective_state(blocked, now=100000.0)
    assert resolved.state == "working" and resolved.ask is None
    assert resolved.detail == "ask deadline passed"


def test_no_current_event_always_takes_the_incoming_one():
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.0
    )
    assert merge(None, incoming, now=1000.0) is incoming


def test_expired_gate_block_reads_as_working_with_no_fresh_event():
    """merge()'s expiry only RELEASES a hold when a fresh event arrives —
    two paths never send one at all (the gate's is_disarmed early return
    emits no report; a gate-process crash between post_ask and its own
    resolving 'working' report leaves nothing to arrive). effective_state
    is the read-time counterpart: a cached blocked event past its
    deadline reads as working even with nothing fresh to merge against."""
    blocked = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=1010.0),
    )
    assert effective_state(blocked, now=1005.0) is blocked
    resolved = effective_state(blocked, now=1011.0)
    assert resolved.state == "working" and resolved.ask is None
    assert resolved.detail == "ask deadline passed"
    assert effective_state(None, now=1000.0) is None
    idle = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1000.0
    )
    assert effective_state(idle, now=99999.0) is idle


def test_process_exit_ends_a_gate_working_hold_immediately():
    """Fix round 1, Finding 1 (Critical): a terminal 'done' must win at
    once, before the 2 s source-precedence window. Nothing ever re-sends
    'done', so a precedence loss here would show a finished pane as
    'working' forever."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1000.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="process", ts=1000.5
    )
    assert merge(current, incoming, now=1000.5) is incoming


def test_headless_end_ends_a_gate_blocked_hold_immediately():
    """Fix round 1, Finding 1 (Critical): a terminal 'done' wins even
    against an unexpired gate 'blocked' hold, before merge()'s
    hold-versus-expiry logic runs at all."""
    current = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1000.0,
        ask=Ask(id="t1", tool="Bash", summary="rm -rf build/", deadline=99999.0),
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="headless", ts=1000.5
    )
    assert merge(current, incoming, now=1000.5) is incoming


def test_gate_blocked_without_an_ask_is_rejected():
    """Fix round 1, Finding 3 (Important): a gate 'blocked' event with no
    ask is an unrecoverable hold — merge() and effective_state() only
    release a gate hold via the ask's deadline, so a gate 'blocked' with
    ask=None never expires. A gate 'blocked' must carry an ask."""
    with pytest.raises(ValueError, match="ask"):
        PaneStateEvent(
            session_id="s1", harness="claude-code", state="blocked", source="gate", ts=1000.0
        )


@pytest.mark.parametrize(
    "row,should_raise",
    [
        (_row(), False),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
            ),
            False,
        ),
        (_row(state="napping"), True),
        (_row(source="ouija"), True),
        (_row(source="manifest", state="done"), True),
        (_row(source="gate", state="done"), True),
        (_row(source="operator", state="done"), True),
        (_row(source="process", state="done"), False),
        (_row(source="headless", state="done"), False),
        (_row(detail="x" * 201), True),
        (
            _row(
                state="working",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1090.0},
            ),
            True,
        ),
        (_row(v=2), True),
        # Fix round 1, Finding 2 (Important): the layer validator used to
        # accept these malformed types (or crash with an uncaught
        # TypeError, in the ask=5 case) instead of rejecting them like the
        # dataclass already did.
        (_row(ts="abc"), True),
        (_row(ts=None), True),
        (_row(state="blocked", source="gate", ask=5), True),
        (_row_missing("harness"), True),
        (_row(session_id=5), True),
        # A present, non-string pane or harness_session_id must be rejected
        # by both validators, the same as a present, non-string session_id
        # or harness above. Null or absent stays valid (it means "not
        # reported"), so this pins only the wrong-type case.
        (_row(pane=5), True),
        (_row(harness_session_id=5), True),
        (_row(pane=None), False),
        (_row(harness_session_id=None), False),
        # Fix round 1, Finding 3 (Important): a gate 'blocked' event with
        # no ask is an unrecoverable hold.
        (_row(state="blocked", source="gate"), True),
        # Whole-branch review, Important 1: the layer validator used to
        # check only that the four ask keys were present, while
        # PaneStateEvent.from_json coerced them with str()/float() — so a
        # non-numeric or missing deadline, or a non-string id, was accepted
        # by one side (or silently stringified) and rejected by the other.
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": "soon"},
            ),
            True,
        ),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": None},
            ),
            True,
        ),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": 5, "tool": "Bash", "summary": "ls", "deadline": 1090.0},
            ),
            True,
        ),
        # The tier is optional. A present tier that is not a string is
        # refused by both. An unknown string reads as permanent, so it is
        # accepted by both.
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0, "tier": 5},
            ),
            True,
        ),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0, "tier": "x"},
            ),
            False,
        ),
        (
            _row(
                state="blocked",
                source="gate",
                ask={"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0, "tier": None},
            ),
            False,
        ),
    ],
)
def test_layer_validator_and_dataclass_agree_on_every_rule(row, should_raise):
    """Two implementations of §3.1 exist by construction (layer purity:
    gate_server.py, a layer module, cannot import opendaisugi.floor). This
    pins them to the same accept/reject verdict so they can't silently
    drift apart."""
    from opendaisugi._state_report import _validate_hook_report_row

    def _raises(fn):
        try:
            fn()
        except ValueError:
            return True
        return False

    layer_raises = _raises(lambda: _validate_hook_report_row(dict(row)))
    dataclass_raises = _raises(lambda: PaneStateEvent.from_json(json.dumps(row)))
    assert layer_raises == dataclass_raises == should_raise


def test_states_and_sources_are_exported_from_the_floor_package():
    """Whole-branch review, minor 6: STATES/SOURCES were only reachable via
    opendaisugi.floor.events, not the package's own public surface.
    ERROR_CODES joins them here for the same reason."""
    from opendaisugi.floor import ERROR_CODES as PKG_ERROR_CODES
    from opendaisugi.floor import SOURCES, STATES

    assert STATES == ("idle", "working", "blocked", "done", "unknown")
    assert SOURCES == ("operator", "gate", "headless", "process", "manifest")
    assert PKG_ERROR_CODES == ERROR_CODES


def test_state_and_source_enums_match_the_layer():
    from opendaisugi._state_report import SOURCES as LAYER_SOURCES
    from opendaisugi._state_report import STATES as LAYER_STATES
    from opendaisugi.floor.events import SOURCES, STATES

    assert set(STATES) == set(LAYER_STATES)
    assert set(SOURCES) == set(LAYER_SOURCES)


def test_done_is_absorbing_whatever_the_incoming_event_says():
    """Master §3.1: done is terminal. Go's state.go carries an explicit
    guard for this in its own TestDoneIsTerminal. merge() needs the same
    guard, or a later event at any delay overrides a pane that has already
    ended. Same inputs as TestDoneIsTerminal: current is done/process at
    ts=100, incoming is working/operator at ts=200, now=200."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="process", ts=100.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=200.0
    )
    assert merge(current, incoming, now=200.0) is current


def test_done_absorbs_a_later_headless_done_too():
    """The absorbing guard does not special-case which later event arrives.
    A second done, from a source allowed to say done, still loses to the
    one already recorded."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="process", ts=100.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="headless", ts=200.0
    )
    assert merge(current, incoming, now=200.0) is current


def test_done_absorbs_regardless_of_which_source_recorded_it():
    """Go's rule 2 has no condition on current's source: `if cur.State ==
    proto.StateDone`. The two tests above both use a done/process current;
    this pins done/headless too, so a guard narrowed to source ==
    "process" cannot pass silently."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="done", source="headless", ts=100.0
    )
    incoming = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="operator", ts=200.0
    )
    assert merge(current, incoming, now=200.0) is current


def test_from_json_rejects_a_non_string_pane():
    """Go's ParseStateEvent rejects a present, non-string pane the same
    way it rejects a non-string session_id or harness. from_json must
    match: a present pane of the wrong JSON type is an error, not a
    silently-accepted int or list."""
    with pytest.raises(ValueError, match="pane must be a string"):
        PaneStateEvent.from_json(json.dumps(_row(pane=5)))


def test_from_json_still_accepts_a_null_or_absent_pane():
    ev = PaneStateEvent.from_json(json.dumps(_row(pane=None)))
    assert ev.pane is None
    ev = PaneStateEvent.from_json(json.dumps(_row()))
    assert ev.pane is None


def test_from_json_rejects_a_non_string_harness_session_id():
    with pytest.raises(ValueError, match="harness_session_id must be a string"):
        PaneStateEvent.from_json(json.dumps(_row(harness_session_id=5)))


def test_from_json_still_accepts_a_null_or_absent_harness_session_id():
    ev = PaneStateEvent.from_json(json.dumps(_row(harness_session_id=None)))
    assert ev.harness_session_id is None
    ev = PaneStateEvent.from_json(json.dumps(_row()))
    assert ev.harness_session_id is None


# The Go expectations each fixture must parse to, recorded here beside the
# fixtures themselves. The fixtures live in tests/floor/testdata/events,
# copied byte for byte from harness/coppice/testdata/events. Every field
# below matches what harness/coppice/internal/proto.ParseStateEvent decodes
# from the same bytes, so a Go/Python divergence on any one of these
# fixtures fails here as loudly as it fails
# harness/coppice/internal/proto/fixtures_test.go.
_FIXTURE_EXPECTATIONS = {
    "gate-blocked.json": PaneStateEvent(
        session_id="d41c",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1757300000.123,
        harness_session_id="11111111-2222-3333-4444-555555555555",
        pane="w1:p3",
        ask=Ask(id="toolu_01", tool="Bash", summary="rm -rf build/", deadline=1757300090.0),
        detail="verdict=deny clause=shell.deny[2]",
    ),
    "gate-blocked-undoable.json": PaneStateEvent(
        session_id="d41c",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1757300000.123,
        harness_session_id="11111111-2222-3333-4444-555555555555",
        pane="w1:p3",
        ask=Ask(
            id="toolu_02",
            tool="Write",
            summary="src/main.py",
            deadline=1757300090.0,
            tier="undoable",
        ),
        detail="verdict=deny clause=shell.deny[2]",
    ),
    "headless-blocked.json": PaneStateEvent(
        session_id="d41c",
        harness="pi",
        state="blocked",
        source="headless",
        ts=1757300075.0,
        harness_session_id="pi-77",
        pane="w1:p6",
        ask=Ask(id="ui_1", tool="Write", summary="src/main.go", deadline=1757300165.0),
        detail="extension_ui_request",
    ),
    "manifest-working.json": PaneStateEvent(
        session_id="d41c",
        harness="codex",
        state="working",
        source="manifest",
        ts=1757300050.0,
        harness_session_id=None,
        pane="w1:p5",
        detail="rule=live_turn_working",
    ),
    "operator-who.json": PaneStateEvent(
        session_id="d41c",
        harness="claude-code",
        state="idle",
        source="operator",
        ts=1757300120.0,
        harness_session_id=None,
        pane="w1:p3",
        detail="",
        who="alice",
    ),
    "process-done.json": PaneStateEvent(
        session_id="d41c",
        harness="shell",
        state="done",
        source="process",
        ts=1757300100.5,
        harness_session_id=None,
        pane="w1:p4",
        detail="exit=0",
    ),
}


def test_event_fixtures_exist_and_cover_the_recorded_set():
    assert FIXTURE_DIR.is_dir(), f"{FIXTURE_DIR} does not exist yet"
    names = {p.name for p in FIXTURE_DIR.iterdir()}
    assert names == set(_FIXTURE_EXPECTATIONS)


@pytest.mark.parametrize("name", sorted(_FIXTURE_EXPECTATIONS))
def test_event_fixture_parses_to_the_go_expectation(name):
    text = (FIXTURE_DIR / name).read_text()
    assert PaneStateEvent.from_json(text) == _FIXTURE_EXPECTATIONS[name]


def test_error_codes_include_server_closed():
    """Master spec §3.3: the wire error codes are a closed enum, listed in
    full in spec-02. server_closed is what coppice-server's pane.create
    answers once Close has started shutting the server down. The floor's
    Python error enum carries the same closed set the coppice client will
    switch on. That client lands in a later plan task."""
    assert ERROR_CODES == (
        "bad_request",
        "no_such_pane",
        "no_such_workspace",
        "no_such_tab",
        "pane_closed",
        "server_closed",
        "not_attached",
        "adapter_error",
        "spawn_failed",
        "timeout",
        "unauthorized",
        "internal",
    )


def test_error_codes_match_gos_validcodes_enum():
    """Binds ERROR_CODES to Go by measurement, not by comment. Reads the
    ErrCode const block straight out of
    harness/coppice/internal/proto/proto.go and compares the set of string
    values against ERROR_CODES, so a later Go-side addition or removal
    fails this test instead of drifting unnoticed."""
    go_text = PROTO_GO.read_text()
    match = re.search(r"const \(\n(.*?)\n\)\n", go_text, re.S)
    assert match, "ErrCode const block not found in proto.go"
    go_codes = set(re.findall(r'ErrCode = "([a-z_]+)"', match.group(1)))
    assert go_codes, "no ErrCode values found in proto.go's const block"
    assert set(ERROR_CODES) == go_codes


def test_a_screen_blocked_outranks_a_process_idle():
    """A question that waits for a key draws nothing more, so the process
    goes quiet. That idle must not clear the screen's blocked read. Go's
    TestAScreenBlockedOutranksAProcessIdle pins the same rule."""
    current = PaneStateEvent(
        session_id="s1", harness="claude-code", state="blocked", source="manifest", ts=1000.0
    )
    quiet = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="process", ts=1010.0
    )
    assert merge(current, quiet, now=1010.0) is current
    output = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="process", ts=1010.0
    )
    assert merge(current, output, now=1010.0) is output
    reread = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1010.0
    )
    assert merge(current, reread, now=1010.0) is reread


def _blocked_with(ask: dict) -> str:
    return json.dumps(_row(state="blocked", source="gate", ask=ask))


def test_an_ask_with_no_tier_reads_as_permanent():
    ev = PaneStateEvent.from_json(
        _blocked_with({"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0})
    )
    assert ev.ask is not None and ev.ask.tier == "permanent"


def test_an_undoable_tier_survives_the_round_trip():
    ev = PaneStateEvent.from_json(
        _blocked_with(
            {"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0, "tier": "undoable"}
        )
    )
    assert ev.ask is not None and ev.ask.tier == "undoable"
    assert json.loads(ev.to_json())["ask"]["tier"] == "undoable"


def test_an_unknown_tier_reads_as_permanent():
    ev = PaneStateEvent.from_json(
        _blocked_with(
            {"id": "t1", "tool": "Bash", "summary": "ls", "deadline": 1.0, "tier": "silent"}
        )
    )
    assert ev.ask is not None and ev.ask.tier == "permanent"


def test_who_round_trips_on_an_operator_event():
    ev = PaneStateEvent(
        session_id="s1", harness="claude", state="idle", source="operator", ts=1.0, who="alice"
    )
    assert json.loads(ev.to_json())["who"] == "alice"
    assert PaneStateEvent.from_json(ev.to_json()) == ev


def test_an_event_with_no_who_leaves_the_key_out():
    ev = PaneStateEvent(session_id="s1", harness="claude", state="idle", source="operator", ts=1.0)
    assert "who" not in json.loads(ev.to_json())


def test_who_is_only_valid_on_an_operator_event():
    with pytest.raises(ValueError, match="who is only valid"):
        PaneStateEvent(
            session_id="s1", harness="claude", state="idle", source="gate", ts=1.0, who="alice"
        )
    row = {**_row(), "who": 7}
    with pytest.raises(ValueError, match="who must be a string"):
        PaneStateEvent.from_json(json.dumps(row))

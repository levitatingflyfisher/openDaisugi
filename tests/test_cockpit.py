"""The roster: grouped by what the operator must do; the header: honest about mode and cache."""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi import ask
from opendaisugi.cockpit import (
    GROUPS,
    Roster,
    SessionRow,
    alerts_for,
    build_roster,
    header_state,
    is_destructive_action,
    load_alert_policy,
)
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord
from opendaisugi.session_tree import SessionTree
from tests.test_claude_transcript import ROWS

NOW = 1_000_000.0


def _claude_session(
    data_dir: Path, sid: str, *, last_ts: float, tool_use_id: str, decision: str
) -> None:
    t = data_dir / f"{sid}.transcript.jsonl"
    t.write_text("\n".join(json.dumps(r) for r in ROWS) + "\n")
    tree = SessionTree.create(
        data_dir / "sessions",
        session_id=sid,
        harness="claude-code",
        cwd="/w",
        harness_session_id=sid,
        transcript_path=str(t),
        clock=lambda: last_ts - 2,
    )
    c = tree.append(
        "tool_call",
        {"toolUseId": tool_use_id, "name": "Bash", "detail": "rm -rf build/"},
        clock=lambda: last_ts - 1,
    )
    tree.append(
        "verdict",
        {
            "toolUseId": tool_use_id,
            "decision": decision,
            "mode": "enforce",
            "clause": "shell: no delete outside tmp",
            "reason": "x",
        },
        parent_id=c.id,
        clock=lambda: last_ts,
    )


def _sprig_session(data_dir: Path, sid: str, *, last_ts: float) -> None:
    tree = SessionTree.create(
        data_dir / "sessions", session_id=sid, harness="sprig", cwd="/e", clock=lambda: last_ts - 3
    )
    tree.append("prompt", {"text": "hi"}, clock=lambda: last_ts - 2)
    tree.append(
        "assistant",
        {
            "model": "claude-sonnet-4",
            "text": "ok",
            "usage": {"fresh": 100, "cacheRead": 900, "cacheWrite": 0, "out": 10},
        },
        clock=lambda: last_ts,
    )


def _gateway_record(**overrides) -> GatewayTurnRecord:
    # ``estimated=False`` on purpose: GatewayTurnRecord.estimated is a *routing-saving*
    # flag (whether the frontier-cost counterfactual was measured or guessed), not the
    # cockpit's honesty flag. cockpit.HeaderState.cache_is_estimate must come from WHICH
    # STORE the cache number was read from (a live transcript vs. this blended journal),
    # never from this unrelated field — setting it False here and still expecting
    # cache_is_estimate True is what proves the source, not the field, drives it.
    base = dict(
        created_at="2026-08-27T00:00:00Z",
        signature="",
        task="x",
        tier="tier1-cloud",
        requested_model="claude-sonnet-4",
        model="claude-sonnet-4",
        difficulty=0.1,
        downgraded=False,
        estimated=False,
        input_tokens=100,
        output_tokens=10,
        frontier_tokens_saved=0,
        actual_dollars=0.01,
        counterfactual_dollars=0.02,
        cache_read_tokens=900,
        cache_creation_tokens=0,
    )
    base.update(overrides)
    return GatewayTurnRecord(**base)


def _row(
    session_id: str,
    *,
    verdict: str = "",
    action: str = "",
    pending_ask=None,
    group: str = "WORKING",
    harness: str = "claude-code",
) -> SessionRow:
    return SessionRow(
        session_id=session_id,
        group=group,
        agent="a",
        action=action,
        verdict=verdict,
        clause="",
        steps=0,
        fresh=0,
        cache_read=0,
        cache_write=0,
        age_s=0.0,
        pending_ask=pending_ask,
        harness=harness,
        cwd="/w",
        transcript_path=None,
    )


def test_groups_and_order(tmp_path: Path):
    _claude_session(tmp_path, "needs", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "needs", "toolName": "Bash"},
        deadline=NOW + 60,
    )
    _claude_session(tmp_path, "working", last_ts=NOW - 10, tool_use_id="t2", decision="allow")
    _sprig_session(tmp_path, "parked", last_ts=NOW - 600)
    _sprig_session(tmp_path, "done", last_ts=NOW - 7200)
    roster = build_roster(tmp_path, now=NOW)
    assert [r.session_id for r in roster.rows] == ["needs", "working", "parked", "done"]
    assert [r.group for r in roster.rows] == list(GROUPS)
    assert roster.counts == {"NEEDS YOU": 1, "WORKING": 1, "PARKED": 1, "DONE": 1}


def test_row_fields_for_a_claude_session(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.harness == "claude-code" and row.agent == "claude-sonnet-4"
    assert row.action == "Bash rm -rf build/"
    assert row.verdict == "DENY" and row.clause == "shell: no delete outside tmp"
    assert row.steps == 1
    assert (row.fresh, row.cache_read, row.cache_write) == (17, 2100, 50)  # from the transcript
    assert row.age_s == 5 and row.pending_ask is None


def test_row_fields_for_a_sprig_session_use_tree_usage(tmp_path: Path):
    _sprig_session(tmp_path, "e1", last_ts=NOW - 1)
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.harness == "sprig" and row.agent == "claude-sonnet-4"
    assert (row.fresh, row.cache_read) == (100, 900)
    assert row.verdict == "" and row.action == ""


def test_pending_ask_is_attached_to_its_row(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    ask.post_ask(
        tmp_path / "gate",
        tool_use_id="t1",
        question={"sessionId": "s1", "toolName": "Bash"},
        deadline=NOW + 60,
    )
    row = build_roster(tmp_path, now=NOW).rows[0]
    assert row.group == "NEEDS YOU" and row.pending_ask["toolUseId"] == "t1"


def test_an_anonymous_ask_is_not_cross_attached_to_every_id_less_session(tmp_path: Path):
    # Bug: an ask whose question has no sessionId was keyed under "", and EVERY sprig
    # session has harness_session_id=None, so asks_by_session.get(harness_session_id or
    # "") attached that one anonymous ask to every sprig session — pushing all of them
    # into NEEDS YOU with a pending_ask that isn't theirs.
    _sprig_session(tmp_path, "e1", last_ts=NOW - 1)
    _sprig_session(tmp_path, "e2", last_ts=NOW - 2)
    ask.post_ask(
        tmp_path / "gate", tool_use_id="tX", question={"toolName": "Bash"}, deadline=NOW + 60
    )
    roster = build_roster(tmp_path, now=NOW)
    assert {r.session_id for r in roster.rows} == {"e1", "e2"}
    assert all(r.pending_ask is None for r in roster.rows)
    assert all(r.group != "NEEDS YOU" for r in roster.rows)


def test_row_marks_controls_not_live_on_the_claude_path(tmp_path: Path):
    # S6/roster-grouping correction: on path D (Claude) some controls the peek pane
    # offers (steer) are not live. The row must carry enough to say so honestly —
    # `harness` is the signal the screen renders "not on this path" from.
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    _sprig_session(tmp_path, "e1", last_ts=NOW - 1)
    rows = {r.harness: r for r in build_roster(tmp_path, now=NOW).rows}
    assert rows["claude-code"].harness == "claude-code"
    assert rows["sprig"].harness == "sprig"


def _write_hook(settings_path: Path, mode: str) -> None:
    settings_path.parent.mkdir(parents=True, exist_ok=True)
    settings_path.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": f"py -m opendaisugi.gate_client --mode {mode}",
                                }
                            ]
                        }
                    ]
                }
            }
        )
    )


def test_header_mode_sources(tmp_path: Path):
    home = tmp_path / "home"
    proj = tmp_path / "proj"
    (home / ".claude").mkdir(parents=True)
    h = header_state(tmp_path, home=home, cwd=proj)
    assert h.gate_mode == "SHADOW" and h.gate_mode_source == "config"
    _write_hook(home / ".claude" / "settings.json", "enforce")
    h = header_state(tmp_path, home=home, cwd=proj)
    # A machine-global-only hook reads as "global", not a bare "hook" — the
    # header must be able to say WHICH hook is governing, not just that one is.
    assert h.gate_mode == "ENFORCE" and h.gate_mode_source == "global"
    (tmp_path / "gate").mkdir(exist_ok=True)
    (tmp_path / "gate" / "DISARMED").write_text("")
    disarmed = header_state(tmp_path, home=home, cwd=proj)
    assert disarmed.gate_mode == "DISARMED"
    # The DISARMED marker overrides hook/config, so the source must say so too —
    # otherwise the header would render "DISARMED (hook)", which is a lie about why.
    assert disarmed.gate_mode_source == "marker"


def test_header_shows_a_cwd_enforce_hook_with_no_global_hook(tmp_path: Path):
    """A `daisugi start --enforce` hook over the TUI's own launch directory,
    with no machine-global hook installed, must show ENFORCE — not the safer
    SHADOW/config default a home-only read would have produced."""
    home = tmp_path / "home"
    proj = tmp_path / "proj"
    _write_hook(proj / ".claude" / "settings.json", "enforce")
    h = header_state(tmp_path, home=home, cwd=proj)
    assert h.gate_mode == "ENFORCE"
    assert h.gate_mode_source == "project"


def test_header_reports_project_plus_global_and_the_stricter_mode(tmp_path: Path):
    home = tmp_path / "home"
    proj = tmp_path / "proj"
    _write_hook(home / ".claude" / "settings.json", "shadow")
    _write_hook(proj / ".claude" / "settings.json", "enforce")
    h = header_state(tmp_path, home=home, cwd=proj)
    assert h.gate_mode == "ENFORCE"
    assert h.gate_mode_source == "project+global"


def test_header_survives_a_deleted_cwd(tmp_path: Path, monkeypatch):
    """Path.cwd() raises OSError if the launch directory was deleted out from
    under a still-running TUI — the header must never crash a refresh over it."""

    def _raise():
        raise FileNotFoundError("cwd deleted")

    monkeypatch.setattr("pathlib.Path.cwd", _raise)
    h = header_state(tmp_path, home=tmp_path)
    assert h.gate_mode == "SHADOW" and h.gate_mode_source == "config"


def test_header_cache_from_live_transcripts_else_gateway(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="allow")
    h = header_state(tmp_path, home=tmp_path, roster=build_roster(tmp_path, now=NOW))
    assert h.cache_source == "transcripts" and round(h.cache_hit_rate, 2) == round(
        2100 / (17 + 2100 + 50), 2
    )
    assert h.session_count == 1
    assert h.cache_is_estimate is False  # live transcript numbers, not an estimate
    empty = header_state(tmp_path / "none", home=tmp_path)
    assert empty.cache_hit_rate is None and empty.cache_source == "none"
    assert empty.cache_is_estimate is False  # nothing to estimate


def test_header_marks_a_gateway_derived_cache_rate_as_an_estimate(tmp_path: Path):
    # S5 (honesty correction): estimated cache numbers must carry a `?` flag in
    # HeaderState — the header must distinguish a live cache-hit-rate (measured
    # from this run's own transcripts) from a gateway-derived estimate (blended
    # across whatever the journal happened to record).
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    journal.append(_gateway_record())
    h = header_state(tmp_path, home=tmp_path)
    assert h.cache_source == "gateway"
    assert h.cache_hit_rate is not None
    assert h.cache_is_estimate is True


def test_alert_policy_defaults_and_file(tmp_path: Path):
    pol = load_alert_policy(tmp_path)
    assert pol["deny"] == "log" and pol["ask"] == "pause" and pol["budget"] == "pause"
    (tmp_path / "alerts.yaml").write_text("deny: pause\n")
    assert load_alert_policy(tmp_path)["deny"] == "pause"


def test_alerts_count_by_class(tmp_path: Path):
    _claude_session(tmp_path, "s1", last_ts=NOW - 5, tool_use_id="t1", decision="deny")
    _claude_session(tmp_path, "s2", last_ts=NOW - 6, tool_use_id="t2", decision="deny")
    ask.post_ask(
        tmp_path / "gate", tool_use_id="t2", question={"sessionId": "s2"}, deadline=NOW + 60
    )
    alerts = alerts_for(build_roster(tmp_path, now=NOW))
    by_class = {a.klass: a for a in alerts}
    assert by_class["deny"].count == 2 and set(by_class["deny"].sessions) == {"s1", "s2"}
    assert by_class["ask"].count == 1 and by_class["ask"].policy == "pause"


def test_alerts_for_merges_a_partial_custom_policy(tmp_path: Path):
    # `policy or dict(_DEFAULT_ALERTS)` used an `or`: a non-empty but PARTIAL custom
    # policy (only "deny" set) was used as-is, so policy["deny_destructive"] KeyError'd
    # the moment a destructive denial was in the roster. Must merge onto the defaults.
    roster = Roster(rows=[_row("s1", verdict="DENY", action="Bash rm -rf build/")], counts={})
    alerts = alerts_for(roster, policy={"deny": "modal"})
    by_class = {a.klass: a for a in alerts}
    assert by_class["deny"].policy == "modal"  # the override took
    assert by_class["deny_destructive"].policy == "pause"  # unset key fell back, no KeyError


def test_alert_policy_falls_back_on_a_non_mapping_yaml_document(tmp_path: Path):
    (tmp_path / "alerts.yaml").write_text("just a scalar string\n")  # parses, but not a dict
    pol = load_alert_policy(tmp_path)
    assert pol["deny"] == "log" and pol["ask"] == "pause"  # defaults, no AttributeError


def test_alert_policy_falls_back_when_the_file_is_unreadable(tmp_path: Path):
    (tmp_path / "alerts.yaml").mkdir()  # exists() is True; read_text() raises IsADirectoryError
    pol = load_alert_policy(tmp_path)
    assert pol["deny"] == "log"  # defaults, no OSError escapes


def test_destructive_alert_matching_is_word_bounded_and_case_insensitive(tmp_path: Path):
    rows = [
        _row("false_pos", verdict="DENY", action="Bash confirm -y then perform a transform"),
        _row("false_neg", verdict="DENY", action="Bash DROP TABLE users"),
        _row("harmless_redirect", verdict="DENY", action="Bash echo hi > /dev/null"),
        _row("true_pos", verdict="DENY", action="Bash rm -rf build/"),
    ]
    roster = Roster(rows=rows, counts={})
    alerts = alerts_for(roster)
    by_class = {a.klass: a for a in alerts}
    destructive = set(by_class["deny_destructive"].sessions)
    assert destructive == {"false_neg", "true_pos"}
    assert (
        "false_pos" not in destructive
    )  # "rm " no longer matches inside con-firm/per-form/trans-form
    assert "harmless_redirect" not in destructive  # "> /dev/null" is not "> /dev/<something>"


def test_is_destructive_action_is_public_and_classifies_by_word_boundary():
    # Public so a screen can classify the guard it shows for an allow (S2).
    assert is_destructive_action("Bash rm -rf build/")
    assert is_destructive_action("git reset --hard HEAD~1")
    assert is_destructive_action("psql DROP TABLE users")
    assert not is_destructive_action("Bash confirm the transform performs")
    assert not is_destructive_action("Bash echo hi > /dev/null")
    assert not is_destructive_action("Read x.py")


def test_is_destructive_action_covers_the_wider_destructive_class():
    # Fix round 1: honest beyond `rm` — dd/mkfs/chmod -R/chown -R/git clean/
    # find -delete/power-state/truncating redirect over a sensitive file.
    for cmd in (
        "Bash dd if=/dev/zero of=/dev/sda",
        "Bash mkfs.ext4 /dev/sdb1",
        "Bash chmod -R 777 /",
        "Bash chown -R root:root /srv",
        "Bash git clean -fdx",
        "Bash find / -name '*.log' -delete",
        "Bash shutdown -h now",
        "Bash reboot",
        "Bash echo x > ~/.ssh/authorized_keys",
        "Bash cat evil > /etc/passwd",
    ):
        assert is_destructive_action(cmd), cmd
    # still no false positives on the benign lookalikes
    for cmd in (
        "Bash echo done >> build.log",  # append, not truncate
        "Bash echo hi > out.txt",  # ordinary file
        "Bash echo hi > /dev/null",  # the harmless sink
        "Bash chmod 644 file",  # not recursive
        "Read authorized_keys",  # no redirect
    ):
        assert not is_destructive_action(cmd), cmd

"""Herdr as a pane backend, driven through a fake `herdr` on PATH that records argv.

Master spec 5.5 keeps Herdr first-class, not a competitor we pretend not to
see. The spawn chain and the --json ladder are PINNED facts in
herdr_verbs.json, not guesses, because the sub-spec's `herdr pane create`
does not exist in Herdr's own CLI reference.
"""

from __future__ import annotations

import json
import os
import queue
import stat
import threading
import time

import pytest

from opendaisugi.floor import Ask, PaneRef
from opendaisugi.floor.herdr_backend import (
    STATE_OUT,
    HerdrBackend,
    load_verbs,
)
from tests.floor import hostfacts


@pytest.fixture
def fake_herdr(tmp_path, monkeypatch):
    """A `herdr` on PATH that records argv and replays canned stdout per verb."""
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    log = tmp_path / "argv.log"
    # /bin/sh, not python3: `available()` probes with a 500 ms budget and a cold
    # interpreter start can miss it on a loaded box.
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        f"printf '%s\\n' \"$*\" >> {log}\n"
        f'key="$1 $2"\n'
        f'out="{tmp_path}/replies/$(printf %s "$key" | tr " /" "__").out"\n'
        f'rc="{tmp_path}/replies/$(printf %s "$key" | tr " /" "__").rc"\n'
        '[ -f "$out" ] && cat "$out"\n'
        '[ -f "$rc" ] && exit "$(cat "$rc")"\n'
        "exit 0\n",
        encoding="utf-8",
    )
    (tmp_path / "replies").mkdir()
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    class Fake:
        def argv(self):
            if not log.exists():
                return []
            return [line.split(" ") for line in log.read_text().splitlines() if line]

        def reply(self, key, out="", rc=0):
            stem = tmp_path / "replies" / key.replace(" ", "_").replace("/", "_")
            stem.with_suffix(".out").write_text(out, encoding="utf-8")
            stem.with_suffix(".rc").write_text(str(rc), encoding="utf-8")

    return Fake()


def test_the_pin_records_that_pane_create_does_not_exist():
    verbs = load_verbs()
    assert "pane create" not in verbs.spawn_chain
    assert verbs.spawn_chain[0] in ("tab create", "pane split")
    assert verbs.source.startswith("https://herdr.dev/docs/cli-reference/")


def test_the_pin_records_which_commands_take_json():
    verbs = load_verbs()
    assert "session list" in verbs.json_commands
    assert "pane list" not in verbs.json_commands, (
        "pane list --json is undocumented; the backend must fall back to text"
    )


def test_the_keys_pin_is_a_subset_of_the_coppice_key_vocabulary():
    """The floor cannot bind a key coppice itself does not know.

    keys.json is the one cross-language list; a Go test writes it from
    coppice's own namedKeys. The herdr pin is a documented subset of it, not
    the whole list: herdr's own CLI reference names none of coppice's f5
    through f12 or its extra ctrl combinations, and a fact the pin cannot
    read off Herdr's own documentation does not belong in a file marked
    verified false against that documentation.
    """
    vocab = json.loads(
        (hostfacts.REPO_ROOT / "harness" / "coppice" / "testdata" / "keys.json").read_text(
            encoding="utf-8"
        )
    )
    verbs = load_verbs()
    assert set(verbs.keys) <= set(vocab)


def test_state_map_never_sends_done_to_herdr_and_never_calls_it_idle():
    """Herdr takes idle, working, blocked or unknown. Idle means "ready for work"."""
    assert set(STATE_OUT.values()) <= {"idle", "working", "blocked", "unknown"}
    assert STATE_OUT["done"] == "unknown", (
        "a pane whose process exited is not ready for work; unknown is the honest cell"
    )


def test_a_done_report_carries_the_word_done_in_the_message(fake_herdr):
    from opendaisugi.floor import PaneStateEvent

    ev = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="done",
        source="process",
        ts=1.0,
        pane="p1",
        detail="exit 0",
    )
    HerdrBackend().report_state(PaneRef("herdr", "p1"), ev)
    argv = fake_herdr.argv()[-1]
    assert "--state" in argv and "unknown" in argv
    assert "--message" in argv and any("done" in a for a in argv)


def test_available_is_true_when_session_list_json_exits_zero(fake_herdr):
    fake_herdr.reply("session list", out="[]\n", rc=0)
    assert HerdrBackend().available() is True
    assert ["session", "list", "--json"] in fake_herdr.argv()


def test_available_is_false_when_herdr_exits_nonzero(fake_herdr):
    fake_herdr.reply("session list", out="", rc=1)
    assert HerdrBackend().available() is False


def test_available_is_false_with_no_herdr_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert HerdrBackend().available() is False


def test_list_read_and_close_never_raise_when_herdr_is_not_on_path(monkeypatch):
    """Master §3.2: only spawn, send_text and send_keys may raise. A herdr
    binary that has vanished from PATH is the same shape as a host that is
    gone: list(), read() and close() must degrade, not crash the cockpit."""
    monkeypatch.setattr("shutil.which", lambda name: None)
    backend = HerdrBackend()
    assert backend.list() == []
    assert backend.read(PaneRef("herdr", "p1")) == ""
    backend.close(PaneRef("herdr", "p1"))  # must not raise


def test_report_state_never_raises_when_herdr_is_not_on_path(monkeypatch):
    """Master §3.2's contract row already says report_state must never
    raise. A herdr binary that vanished from PATH is the same shape as
    every other verb's dead-host case: close() already degrades it, and
    report_state must too, since nothing here waits on this call's result."""
    from opendaisugi.floor import PaneStateEvent

    monkeypatch.setattr("shutil.which", lambda name: None)
    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    HerdrBackend().report_state(PaneRef("herdr", "p1"), ev)  # must not raise


def test_list_proven_is_true_with_rows_when_pane_list_answers(fake_herdr):
    fake_herdr.reply("pane list", out="p1\n", rc=0)
    proven, rows = HerdrBackend().list_proven()
    assert proven is True
    assert [i.ref.id for i in rows] == ["p1"]


def test_list_proven_is_false_when_pane_ids_itself_fails(fake_herdr):
    fake_herdr.reply("pane list", out="", rc=1)
    assert HerdrBackend().list_proven() == (False, [])


def test_list_proven_treats_a_second_failing_call_as_unproven_not_empty(tmp_path, monkeypatch):
    """_pane_ids() and list() are two separate round trips to `pane list`.
    A first call that proves a pane exists, followed by a second that
    comes back empty, is the second call failing, not every pane having
    exited in the instant between the two.
    """
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    count_file = tmp_path / "pane_list_count"
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        'if [ "$1 $2" = "pane list" ]; then\n'
        "  count=0\n"
        f"  [ -f {count_file} ] && count=$(cat {count_file})\n"
        "  count=$((count + 1))\n"
        f'  echo "$count" > {count_file}\n'
        '  [ "$count" -eq 1 ] && echo p1\n'
        "  exit 0\n"
        "fi\n"
        "exit 0\n",
        encoding="utf-8",
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    assert HerdrBackend().list_proven() == (False, [])


def test_pane_ids_falls_back_to_the_plain_verb_the_same_way_list_json_does(tmp_path, monkeypatch):
    """_list_json retries the plain verb when a --json call itself fails.
    _pane_ids must not differ: today's pin never marks pane list as a
    json command, so this is latent, not reachable, but a future pin
    that does must not turn subscribe() into a backend that never sees
    a proven poll again. The fake here answers the --json form with a
    failure and the plain form with a real row, so a caller that never
    retries the plain verb sees None instead of {"p1"}.
    """
    import dataclasses

    from opendaisugi.floor.herdr_backend import load_verbs

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        'if [ "$1 $2" = "pane list" ]; then\n'
        '  if [ "$3" = "--json" ]; then\n'
        "    exit 1\n"
        "  fi\n"
        "  echo p1\n"
        "  exit 0\n"
        "fi\n"
        "exit 0\n",
        encoding="utf-8",
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    verbs = dataclasses.replace(load_verbs(), json_commands=frozenset({"pane list"}))
    backend = HerdrBackend(verbs=verbs)
    assert backend._pane_ids() == {"p1"}


def test_spawn_and_send_text_still_raise_when_herdr_is_not_on_path(monkeypatch, tmp_path):
    monkeypatch.setattr("shutil.which", lambda name: None)
    backend = HerdrBackend()
    with pytest.raises(OSError):
        backend.spawn(cwd=tmp_path, cmd=["true"], env={}, label="x", kind="pty")
    with pytest.raises(OSError):
        backend.send_text(PaneRef("herdr", "p1"), "hi")
    with pytest.raises(OSError):
        backend.send_keys(PaneRef("herdr", "p1"), ["enter"])


def test_subscribe_reports_done_with_source_process_when_a_pane_leaves_the_list(
    tmp_path, monkeypatch
):
    """F30: herdr has no done state of its own. subscribe() now compares
    list()'s own pane ids across polls, the same process fact tmux emits
    for a pane that simply vanishes from list-panes.

    This fake herdr counts its own `pane list` calls and answers p1 only
    on the first one, empty after. Driving that from the subprocess's own
    call count, instead of a second thread watching argv() and rewriting
    the canned reply mid-flight, removes a real race: a watcher thread can
    time out its own wait before the first poll ever runs under load, then
    overwrite the reply before subscribe() has read p1 even once, and the
    pane is never reported gone at all. That raced twice under box
    contention with the watcher design; this one does not race by
    construction.
    """
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    count_file = tmp_path / "pane_list_count"
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        'if [ "$1 $2" = "pane list" ]; then\n'
        "  count=0\n"
        f"  [ -f {count_file} ] && count=$(cat {count_file})\n"
        "  count=$((count + 1))\n"
        f'  echo "$count" > {count_file}\n'
        '  [ "$count" -eq 1 ] && echo p1\n'
        "  exit 0\n"
        "fi\n"
        "exit 0\n",
        encoding="utf-8",
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    backend = HerdrBackend()
    events: queue.Queue = queue.Queue()

    def pump():
        for ev in backend.subscribe():
            events.put(ev)

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    ev = events.get(timeout=8.0)
    assert ev.pane == "p1"
    assert ev.state == "done"
    assert ev.source == "process"
    backend.close_connection()
    thread.join(timeout=3.0)


def test_subscribe_never_fabricates_done_when_a_later_poll_fails_ambiguously(tmp_path, monkeypatch):
    """A `pane list` failure on a later poll, after the pane was already
    seen alive, must not read as the pane having exited: an unproven
    failure is not proof of anything, the same fact tmux's own close()
    treats a failed list-panes call as. The fake answers p1 on the first
    call and fails every call after, deterministically by its own call
    count.
    """
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    count_file = tmp_path / "pane_list_count"
    script = bin_dir / "herdr"
    script.write_text(
        "#!/bin/sh\n"
        'if [ "$1 $2" = "pane list" ]; then\n'
        "  count=0\n"
        f"  [ -f {count_file} ] && count=$(cat {count_file})\n"
        "  count=$((count + 1))\n"
        f'  echo "$count" > {count_file}\n'
        '  if [ "$count" -eq 1 ]; then\n'
        "    echo p1\n"
        "    exit 0\n"
        "  fi\n"
        "  exit 1\n"
        "fi\n"
        "exit 0\n",
        encoding="utf-8",
    )
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")

    backend = HerdrBackend()
    events: queue.Queue = queue.Queue()

    def pump():
        for ev in backend.subscribe():
            events.put(ev)

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    time.sleep(3.5)  # several poll cycles at _POLL_S = 1.0
    backend.close_connection()
    thread.join(timeout=3.0)
    collected = []
    while not events.empty():
        collected.append(events.get_nowait())
    assert not any(e.pane == "p1" and e.state == "done" for e in collected), (
        "an unproven later poll failure must never fabricate done for a pane already seen alive"
    )
    # Positive control: without this, the row would pass just as well if
    # p1 were never seen alive at all, for instance if a pinned verb
    # changed and the fake's own match stopped firing. At least 2 proves
    # the first poll actually answered and a later one actually failed.
    assert int(count_file.read_text()) >= 2, "the fake herdr was never actually called twice"


def test_subscribe_clears_a_leftover_ready_event_before_polling(fake_herdr):
    """A caller that keeps one threading.Event across two subscribe()
    calls must not read a leftover set flag as a fresh baseline: a
    re-subscribe clears it at the top, the same way _stopped already is.
    """
    fake_herdr.reply("pane list", out="", rc=1)  # _pane_ids() never proven
    backend = HerdrBackend()
    ready = threading.Event()
    ready.set()  # simulates a leftover flag from a subscription already stopped

    def pump():
        for _ in backend.subscribe(ready=ready):
            pass

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    time.sleep(0.1)
    assert not ready.is_set(), "a re-subscribe must clear a leftover ready event"
    backend.close_connection()
    thread.join(timeout=3.0)


def test_close_connection_stops_the_poll_thread_within_two_intervals(fake_herdr):
    """subscribe() polls forever by design; close_connection() is the only
    way to make it stop. A caller that starts a pump thread and later
    tears it down must see that thread actually end, not keep polling a
    host nobody is listening to any more.

    The real bound is one poll interval plus whatever herdr call is
    already in flight, and one cycle can make several, each up to
    _CALL_S; against this fake, a shell script that answers at once, the
    join below comfortably covers that.
    """
    from opendaisugi.floor.herdr_backend import _POLL_S

    fake_herdr.reply("pane list", out="", rc=0)
    fake_herdr.reply("agent list", out="", rc=0)
    backend = HerdrBackend()
    stopped = threading.Event()

    def pump():
        for _ in backend.subscribe():
            pass
        stopped.set()

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    time.sleep(0.05)  # let the loop start polling
    backend.close_connection()
    thread.join(timeout=2 * _POLL_S + 1.0)
    assert stopped.is_set(), "the pump thread never stopped"


def test_spawn_uses_the_pinned_chain_not_pane_create(fake_herdr, tmp_path):
    fake_herdr.reply("tab create", out="pane_id: p7\n", rc=0)
    ref = HerdrBackend().spawn(
        cwd=tmp_path, cmd=["claude"], env={"COPPICE_PANE": "p7"}, label="auth fix", kind="pty"
    )
    verbs = [" ".join(a[:2]) for a in fake_herdr.argv()]
    assert "pane create" not in verbs
    assert verbs[0] == "tab create"
    assert "pane run" in verbs
    assert ref == PaneRef("herdr", "p7")
    create = next(a for a in fake_herdr.argv() if a[:2] == ["tab", "create"])
    assert "--cwd" in create and str(tmp_path) in create
    assert "--env" in create


def test_read_passes_the_pinned_source_names(fake_herdr):
    fake_herdr.reply("pane read", out="READY\n", rc=0)
    backend = HerdrBackend()
    assert backend.read(PaneRef("herdr", "p1"), source="detection") == "READY\n"
    argv = fake_herdr.argv()[-1]
    assert argv[:2] == ["pane", "read"] and "--source" in argv and "detection" in argv


def test_report_state_uses_report_agent_with_the_daisugi_source(fake_herdr):
    from opendaisugi.floor import PaneStateEvent

    # A gate-sourced blocked event always carries an Ask; PaneStateEvent
    # refuses one that does not.
    ev = PaneStateEvent(
        session_id="s1",
        harness="claude-code",
        state="blocked",
        source="gate",
        ts=1.0,
        pane="p1",
        ask=Ask(id="a1", tool="Bash", summary="rm -rf build/", deadline=61.0),
    )
    HerdrBackend().report_state(PaneRef("herdr", "p1"), ev)
    argv = fake_herdr.argv()[-1]
    assert argv[:2] == ["pane", "report-agent"]
    assert "--source" in argv and "daisugi" in argv
    assert "--state" in argv and "blocked" in argv
    assert "--agent" in argv and "claude-code" in argv


def _explain(fake_herdr, payload):
    fake_herdr.reply("pane list", out="p1\n", rc=0)
    fake_herdr.reply("agent explain", out=json.dumps(payload), rc=0)
    fake_herdr.reply("agent list", out="p1 claude-code blocked\n", rc=0)
    infos = HerdrBackend().list()
    return [i.state for i in infos if i.state is not None]


def test_the_pin_records_the_authority_path_not_a_substring_rule():
    verbs = load_verbs()
    assert verbs.authority_field == ("authority", "source")
    assert verbs.authority_value == "daisugi"


def test_a_blocked_row_sources_as_manifest_with_a_gate_blocked_detail_when_the_field_says_daisugi(
    fake_herdr,
):
    """agent list gives no Ask, so a blocked row can never be a gate source.

    PaneStateEvent refuses source gate on state blocked without an Ask. A
    block Herdr attributes to daisugi is reported as manifest instead, and
    the detail says where it really came from.
    """
    states = _explain(
        fake_herdr, {"agent": "claude-code", "state": "blocked", "authority": {"source": "daisugi"}}
    )
    assert states and states[0].source == "manifest"
    assert states[0].detail == "reported by herdr as gate-blocked"


def test_state_source_is_manifest_when_the_field_names_someone_else(fake_herdr):
    states = _explain(
        fake_herdr,
        {"agent": "claude-code", "state": "blocked", "authority": {"source": "screen-manifest"}},
    )
    assert states and states[0].source == "manifest"
    assert states[0].detail != "reported by herdr as gate-blocked"


def test_daisugi_anywhere_else_in_the_document_never_forges_a_gate_source(fake_herdr):
    """The operator's own repo lives at .../openDaisugi. A cwd is not an authority."""
    states = _explain(
        fake_herdr,
        {
            "agent": "claude-code",
            "state": "blocked",
            "cwd": "/home/user/openDaisugi",
            "command": "claude --resume daisugi-1",
            "label": "daisugi floor",
            "authority": {"source": "screen-manifest"},
        },
    )
    assert states and states[0].source == "manifest"
    assert states[0].detail != "reported by herdr as gate-blocked"


def test_a_missing_authority_field_is_manifest_not_gate(fake_herdr):
    states = _explain(fake_herdr, {"agent": "claude-code", "state": "blocked"})
    assert states and states[0].source == "manifest"
    assert states[0].detail != "reported by herdr as gate-blocked"


def test_an_authority_field_of_the_wrong_shape_is_manifest_not_gate(fake_herdr):
    for payload in (
        {"authority": "daisugi"},  # not a dict at the first hop
        {"authority": {"source": {"name": "daisugi"}}},  # a dict where a name belongs
        {"authority": {"source": ["daisugi"]}},  # a list where a name belongs
        {"authority": {"other": "daisugi"}},  # the wrong leaf
    ):
        payload.update({"agent": "claude-code", "state": "blocked"})
        states = _explain(fake_herdr, payload)
        assert states and states[0].source == "manifest", payload
        assert states[0].detail != "reported by herdr as gate-blocked", payload


def test_explain_that_fails_is_manifest_not_gate(fake_herdr):
    fake_herdr.reply("pane list", out="p1\n", rc=0)
    fake_herdr.reply("agent explain", out="", rc=1)
    fake_herdr.reply("agent list", out="p1 claude-code blocked\n", rc=0)
    infos = HerdrBackend().list()
    states = [i.state for i in infos if i.state is not None]
    assert states and states[0].source == "manifest"
    assert states[0].detail != "reported by herdr as gate-blocked"


def test_state_source_is_gate_for_a_non_blocked_state_when_the_field_says_daisugi(fake_herdr):
    """Only a blocked state needs an Ask. Idle, working and unknown do not."""
    fake_herdr.reply("pane list", out="p1\n", rc=0)
    fake_herdr.reply(
        "agent explain",
        out=json.dumps(
            {"agent": "claude-code", "state": "working", "authority": {"source": "daisugi"}}
        ),
        rc=0,
    )
    fake_herdr.reply("agent list", out="p1 claude-code working\n", rc=0)
    infos = HerdrBackend().list()
    states = [i.state for i in infos if i.state is not None]
    assert states and states[0].state == "working" and states[0].source == "gate"


def test_the_pin_records_every_verb_the_backend_runs():
    verbs = load_verbs()
    for name in (
        "read",
        "send_text",
        "send_keys",
        "close",
        "list_panes",
        "list_agents",
        "prompt",
        "explain",
        "report_agent",
    ):
        assert verbs.verb(name), f"{name} is not pinned"
    assert verbs.keys["ctrl+c"] == "ctrl+c" and verbs.keys["esc"] == "esc"


def test_a_key_herdr_rejects_raises_instead_of_being_dropped(fake_herdr):
    """A silent no-op would let the contract's send-keys test pass on nothing."""
    fake_herdr.reply("pane send-keys", out="", rc=1)
    with pytest.raises(RuntimeError) as e:
        HerdrBackend().send_keys(PaneRef("herdr", "p1"), ["ctrl+c"])
    assert "rejected" in str(e.value)


def test_an_unknown_key_name_is_refused_before_the_subprocess(fake_herdr):
    with pytest.raises(ValueError) as e:
        HerdrBackend().send_keys(PaneRef("herdr", "p1"), ["hyperspace"])
    assert "ctrl+c" in str(e.value)
    assert not any(a[:2] == ["pane", "send-keys"] for a in fake_herdr.argv())


def test_spawn_accepts_the_protocols_harness_keyword_and_ignores_it(fake_herdr, tmp_path):
    fake_herdr.reply("tab create", out="pane_id: p7\n", rc=0)
    ref = HerdrBackend().spawn(
        cwd=tmp_path, cmd=["claude"], env={}, label="l", kind="pty", harness="claude-code"
    )
    assert ref.id == "p7"


def test_a_herdr_that_times_out_is_unavailable_not_an_exception(fake_herdr, monkeypatch):
    import subprocess

    def boom(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd="herdr", timeout=0.5)

    monkeypatch.setattr("subprocess.run", boom)
    assert HerdrBackend().available() is False


def test_against_real_herdr_when_it_is_installed():
    reason = hostfacts.skip_reason("herdr")
    if reason:
        pytest.skip(reason)
    backend = HerdrBackend()
    assert backend.available() is True
    verbs = load_verbs()
    if not verbs.verified:
        pytest.skip(
            "herdr is installed but herdr_verbs.json is still unverified. "
            "Confirm the spawn chain and the --json ladder, then set verified true."
        )

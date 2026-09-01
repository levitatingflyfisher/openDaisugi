from __future__ import annotations

import pytest

from opendaisugi.voice.deliver import DeliverResult, _armed_path, arm, deliver, disarm, is_armed

from .conftest import FakeBackend, FakePaneRef, RecordingSend


def test_deliver_preview_mode_never_calls_send(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    result = deliver(pane, "hello", lambda: backend, mode="preview", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="preview")
    assert send.calls == []


def test_deliver_preview_mode_never_calls_send_even_when_the_pane_is_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    result = deliver(pane, "hello", lambda: backend, mode="preview", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="preview")
    assert send.calls == []


def test_deliver_refuses_an_unknown_mode_even_when_the_pane_is_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    result = deliver(
        pane, "hello", lambda: backend, mode="something-else", armed_dir=tmp_path, send=send
    )
    assert result.delivered == "refused"
    assert "preview" in result.reason
    assert "send" in result.reason
    assert send.calls == []


def test_deliver_send_is_refused_when_the_pane_is_not_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    result = deliver(pane, "hello", lambda: backend, mode="send", armed_dir=tmp_path, send=send)
    assert result.delivered == "refused"
    assert "daisugi voice arm w1:p1" in result.reason
    assert send.calls == []


def test_deliver_send_succeeds_once_the_pane_is_armed(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    result = deliver(pane, "hello", lambda: backend, mode="send", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="sent")
    assert send.calls == [(backend, pane, "hello")]


def test_arm_grant_expires(tmp_path):
    entry = arm("w1:p1", minutes=1, armed_dir=tmp_path, now=1_000.0)
    assert entry.expires_at == pytest.approx(1_060.0)
    assert is_armed("w1:p1", armed_dir=tmp_path, now=1_030.0)
    assert not is_armed("w1:p1", armed_dir=tmp_path, now=1_100.0)


def test_deliver_send_succeeds_within_an_armed_window_using_an_injected_clock(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=1, armed_dir=tmp_path, now=1_000.0)
    result = deliver(
        pane, "hello", lambda: backend, mode="send", armed_dir=tmp_path, send=send, now=1_030.0
    )
    assert result == DeliverResult(delivered="sent")
    assert send.calls == [(backend, pane, "hello")]


def test_deliver_send_is_refused_once_the_grant_has_expired(tmp_path):
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=1, armed_dir=tmp_path, now=1_000.0)
    result = deliver(
        pane, "hello", lambda: backend, mode="send", armed_dir=tmp_path, send=send, now=1_100.0
    )
    assert result.delivered == "refused"
    assert send.calls == []


def test_deliver_preview_never_calls_the_backend_factory(tmp_path):
    calls: list[int] = []
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()

    def _factory() -> FakeBackend:
        calls.append(1)
        return FakeBackend()

    result = deliver(pane, "hello", _factory, mode="preview", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="preview")
    assert calls == []


def test_deliver_send_on_an_unarmed_pane_never_calls_the_backend_factory(tmp_path):
    calls: list[int] = []
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()

    def _factory() -> FakeBackend:
        calls.append(1)
        return FakeBackend()

    result = deliver(pane, "hello", _factory, mode="send", armed_dir=tmp_path, send=send)
    assert result.delivered == "refused"
    assert calls == []
    assert send.calls == []


def test_deliver_send_on_an_armed_pane_calls_the_backend_factory_exactly_once(tmp_path):
    calls: list[int] = []
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=30, armed_dir=tmp_path)

    def _factory() -> FakeBackend:
        calls.append(1)
        return backend

    result = deliver(pane, "hello", _factory, mode="send", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="sent")
    assert len(calls) == 1
    assert send.calls == [(backend, pane, "hello")]


def test_disarm_revokes_the_grant(tmp_path):
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    assert is_armed("w1:p1", armed_dir=tmp_path)
    assert disarm("w1:p1", armed_dir=tmp_path) is True
    assert not is_armed("w1:p1", armed_dir=tmp_path)


def test_disarm_on_a_pane_that_was_never_armed_returns_false_and_leaves_no_file(tmp_path):
    armed_dir = tmp_path / "armed"
    assert disarm("never:armed", armed_dir=armed_dir) is False
    assert not armed_dir.exists()


@pytest.mark.parametrize(
    "content",
    [
        "not json",
        "[1, 2]",
        "null",
        '{"expires_at": "not a number"}',
        '{"expires_at": null}',
        "{}",
    ],
)
def test_is_armed_treats_every_corrupt_shape_as_unarmed(tmp_path, content):
    _armed_path(tmp_path, "w1:p1").write_text(content)
    assert not is_armed("w1:p1", armed_dir=tmp_path)


def test_colliding_looking_keys_get_separate_grants(tmp_path):
    arm("a:b", minutes=30, armed_dir=tmp_path)
    assert is_armed("a:b", armed_dir=tmp_path)
    assert not is_armed("a/b", armed_dir=tmp_path)


def test_disarming_one_colliding_key_does_not_disarm_the_other(tmp_path):
    arm("a:b", minutes=30, armed_dir=tmp_path)
    arm("a/b", minutes=30, armed_dir=tmp_path)
    assert disarm("a/b", armed_dir=tmp_path) is True
    assert is_armed("a:b", armed_dir=tmp_path)
    assert not is_armed("a/b", armed_dir=tmp_path)


def test_arm_sanitizes_a_pane_key_containing_a_slash(tmp_path):
    arm("grp/w1:p1", minutes=30, armed_dir=tmp_path)
    assert is_armed("grp/w1:p1", armed_dir=tmp_path)
    entry_files = [p for p in tmp_path.iterdir() if p.is_file()]
    assert len(entry_files) == 1


@pytest.mark.parametrize("pane_key", ["../../x", "../x", "a/../../etc/passwd"])
def test_arm_keeps_a_traversal_shaped_key_inside_the_armed_dir(tmp_path, pane_key):
    armed_dir = tmp_path / "armed"
    arm(pane_key, minutes=30, armed_dir=armed_dir)
    assert is_armed(pane_key, armed_dir=armed_dir)
    assert list(tmp_path.iterdir()) == [armed_dir]
    children = list(armed_dir.iterdir())
    assert len(children) == 1
    assert children[0].is_file()


def test_arm_creates_the_armed_dir_at_0700_and_the_entry_at_0600(tmp_path):
    armed_dir = tmp_path / "armed"
    arm("w1:p1", minutes=30, armed_dir=armed_dir)
    assert oct(armed_dir.stat().st_mode)[-3:] == "700"
    entry_files = list(armed_dir.glob("*.json"))
    assert len(entry_files) == 1
    assert oct(entry_files[0].stat().st_mode)[-3:] == "600"


def test_is_armed_reports_unarmed_for_a_pane_key_too_long_for_a_file_name(tmp_path):
    # armed_dir must already exist for this to reproduce: Path.exists() on a
    # long name under a parent that is not there yet just answers False at
    # the missing parent, never reaching the OS's own file-name-length
    # check. Arming a different, short key first creates the directory.
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    long_key = "p" * 300
    assert not is_armed(long_key, armed_dir=tmp_path)


def test_deliver_send_calls_is_armed_exactly_once(tmp_path, monkeypatch):
    import opendaisugi.voice.deliver as deliver_module

    real_is_armed = deliver_module.is_armed
    calls: list[int] = []

    def _counting_is_armed(*args, **kwargs):
        calls.append(1)
        return real_is_armed(*args, **kwargs)

    monkeypatch.setattr(deliver_module, "is_armed", _counting_is_armed)
    backend = FakeBackend()
    pane = FakePaneRef(backend="coppice", id="w1:p1")
    send = RecordingSend()
    arm("w1:p1", minutes=30, armed_dir=tmp_path)
    result = deliver(pane, "hello", lambda: backend, mode="send", armed_dir=tmp_path, send=send)
    assert result == DeliverResult(delivered="sent")
    assert len(calls) == 1

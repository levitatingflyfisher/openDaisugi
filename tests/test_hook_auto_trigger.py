"""The capture hook's opportunistic 'no cron' background-tend trigger."""

from __future__ import annotations

from opendaisugi.config import Config, save_config
from opendaisugi.hook import maybe_trigger_background_tend


def test_no_trigger_without_consent(tmp_path):
    calls: list = []
    fired = maybe_trigger_background_tend(
        tmp_path, now=10_000.0, spawn=calls.append, min_interval_s=1800
    )
    assert fired is False
    assert calls == []


def test_no_trigger_within_interval(tmp_path):
    save_config(Config(auto_tend=True), tmp_path / "config.yaml")
    (tmp_path / ".hook-record-tend-trigger").write_text("9500.0")
    calls: list = []
    fired = maybe_trigger_background_tend(
        tmp_path, now=10_000.0, spawn=calls.append, min_interval_s=1800
    )
    assert fired is False
    assert calls == []


def test_triggers_when_consented_and_due(tmp_path):
    save_config(Config(auto_tend=True), tmp_path / "config.yaml")
    calls: list = []
    fired = maybe_trigger_background_tend(
        tmp_path, now=10_000.0, spawn=calls.append, min_interval_s=1800
    )
    assert fired is True
    assert calls == [tmp_path]
    # Stamp was written → an immediate re-check is not due.
    again = maybe_trigger_background_tend(
        tmp_path, now=10_100.0, spawn=calls.append, min_interval_s=1800
    )
    assert again is False


def test_never_raises_on_spawn_error(tmp_path):
    save_config(Config(auto_tend=True), tmp_path / "config.yaml")

    def boom(_):
        raise RuntimeError("spawn failed")

    # A failing spawn must never disrupt the host hook — it swallows and returns False.
    assert (
        maybe_trigger_background_tend(tmp_path, now=10_000.0, spawn=boom, min_interval_s=1800)
        is False
    )


def test_resident_mirror_triggers_auto_tend_from_the_data_home(tmp_path, monkeypatch):
    import json

    from opendaisugi.models import Envelope, Permission
    from opendaisugi.warden import register_envelope, warden_and_contract

    spawned = []
    monkeypatch.setattr("opendaisugi.hook._spawn_detached_auto_tend", lambda d: spawned.append(d))
    data = tmp_path / "d"
    data.mkdir()
    save_config(Config(auto_tend=True), data / "config.yaml")
    env = Envelope(generated_by="test", task="t", permissions=Permission(shell_allowlist=["ls"]))
    register_envelope(env, root=data / "warden")
    payload = json.dumps(
        {"tool_name": "bash", "tool_input": {"command": "ls"}, "session_id": "s1"}
    ).encode()
    warden_and_contract(
        payload, root=data / "warden", fmt="pi", captures_root=data / "warden" / "captures"
    )
    assert spawned == [data]

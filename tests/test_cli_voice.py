"""Tests for `daisugi voice serve|ptt|arm|disarm`.

No test here starts a real server, opens an audio device, loads a speech
model, or reaches the network. `serve` tests patch
`opendaisugi.voice.server.serve` and assert only the exit code and the
message; `ptt` tests patch `opendaisugi.voice.ptt.main_loop`. Every `serve`
test passes `--data-dir tmp_path` so none of them reads the operator's real
config or real token path.
"""

from __future__ import annotations

import json
import time

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.voice.deliver import is_armed

runner = CliRunner()


def _serve_raises(exc: BaseException):
    """A fake `serve` that fails before it would ever call on_bound.

    Matches the real serve(): pick_engine and build_server both run before
    on_bound is reached, so a failure from either of them never announces a
    listening line that was never true.
    """

    def _fake(*, on_bound=None, **kwargs):
        raise exc

    return _fake


def _serve_announces_then_stops(*, keyboard_interrupt: bool = False):
    """A fake `serve` that calls on_bound, the way a real bind success does.

    With keyboard_interrupt, it then raises KeyboardInterrupt, the way a
    real Ctrl-C during serve_forever does. Otherwise it just returns.
    """

    def _fake(*, on_bound=None, **kwargs):
        if on_bound is not None:
            on_bound()
        if keyboard_interrupt:
            raise KeyboardInterrupt

    return _fake


def test_voice_help_lists_the_four_subcommands():
    result = runner.invoke(app, ["voice", "--help"])
    assert result.exit_code == 0, result.output
    for name in ("serve", "ptt", "arm", "disarm"):
        assert name in result.output


def test_voice_arm_then_disarm(tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "30m", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")

    result = runner.invoke(app, ["voice", "disarm", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")
    assert "w1:p1 disarmed" in result.output


def test_voice_arm_accepts_hours(tmp_path):
    before = time.time()
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "2h", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed", now=before + 3600)
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed", now=before + 3 * 3600)


def test_voice_arm_exits_1_on_a_bad_for_value(tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "not-a-duration", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1, result.output
    assert "--for must be" in result.output
    assert not (tmp_path / "voice" / "armed").exists()


@pytest.mark.parametrize("value", ["inf", "1e400", "nan"])
def test_voice_arm_exits_1_on_a_non_finite_for_value(value, tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", value, "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1, result.output
    assert "--for must be a positive number of minutes" in result.output
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")


@pytest.mark.parametrize("value", ["-5m", "0m"])
def test_voice_arm_exits_1_on_a_non_positive_for_value(value, tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", value, "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1, result.output
    assert "--for must be a positive number of minutes" in result.output
    assert not is_armed("w1:p1", armed_dir=tmp_path / "voice" / "armed")


def test_voice_arm_json_output_reports_the_pane_and_the_epoch_expiry(tmp_path):
    result = runner.invoke(
        app, ["voice", "arm", "w1:p1", "--for", "30m", "--json", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["pane"] == "w1:p1"
    assert isinstance(payload["expires_at"], (int, float))


def test_voice_disarm_on_a_never_armed_pane_still_exits_0(tmp_path):
    result = runner.invoke(app, ["voice", "disarm", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert "w1:p1 had no grant" in result.output


def test_voice_arm_exits_1_with_a_sentence_on_a_read_only_data_dir(tmp_path):
    # arm() creates <data-dir>/voice/armed with mkdir. A read-only voice/
    # directory makes that mkdir raise, and this must become one sentence
    # naming the directory and --data-dir, not a traceback.
    voice_dir = tmp_path / "voice"
    voice_dir.mkdir()
    voice_dir.chmod(0o500)
    try:
        result = runner.invoke(
            app, ["voice", "arm", "w1:p1", "--for", "30m", "--data-dir", str(tmp_path)]
        )
        assert result.exit_code == 1, result.output
        assert "Traceback" not in result.output
        assert str(voice_dir / "armed") in result.output
        assert "--data-dir" in result.output
    finally:
        voice_dir.chmod(0o700)


def test_voice_disarm_exits_1_with_a_sentence_on_a_read_only_data_dir(tmp_path):
    voice_dir = tmp_path / "voice"
    armed_dir = voice_dir / "armed"
    armed_dir.mkdir(parents=True)
    (armed_dir / "w1%3Ap1.json").write_text('{"pane_key": "w1:p1", "expires_at": 0}')
    armed_dir.chmod(0o500)
    try:
        result = runner.invoke(app, ["voice", "disarm", "w1:p1", "--data-dir", str(tmp_path)])
        assert result.exit_code == 1, result.output
        assert "Traceback" not in result.output
        assert str(armed_dir) in result.output
        assert "--data-dir" in result.output
    finally:
        armed_dir.chmod(0o700)


def test_voice_disarm_json_output_reports_whether_a_grant_was_removed(tmp_path):
    runner.invoke(app, ["voice", "arm", "w1:p1", "--for", "30m", "--data-dir", str(tmp_path)])
    removed = runner.invoke(
        app, ["voice", "disarm", "w1:p1", "--json", "--data-dir", str(tmp_path)]
    )
    assert removed.exit_code == 0, removed.output
    assert json.loads(removed.output) == {"pane": "w1:p1", "removed": True}

    untouched = runner.invoke(
        app, ["voice", "disarm", "w1:p2", "--json", "--data-dir", str(tmp_path)]
    )
    assert untouched.exit_code == 0, untouched.output
    assert json.loads(untouched.output) == {"pane": "w1:p2", "removed": False}


def test_voice_serve_reaches_pick_engine_for_parakeet_when_faster_whisper_is_missing(
    monkeypatch, tmp_path
):
    # B5: the eager faster-whisper check is gone. A box configured for
    # Parakeet, with faster-whisper hidden to prove nothing still guards on
    # it, reaches the real pick_engine and is told about sherpa-onnx, the
    # package Parakeet actually needs.
    import sys

    from opendaisugi.config import Config, save_config

    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    save_config(
        Config(voice_engine="parakeet", voice_model=str(tmp_path / "model")),
        tmp_path / "config.yaml",
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3, result.output
    assert "sherpa-onnx" in result.output


def test_voice_serve_exits_3_when_the_engine_is_unavailable(monkeypatch, tmp_path):
    from opendaisugi.voice.engines import EngineUnavailable

    monkeypatch.setattr(
        "opendaisugi.voice.server.serve",
        _serve_raises(
            EngineUnavailable(
                "faster-whisper is not installed. Install it with: pip install 'opendaisugi[voice]'"
            )
        ),
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert "opendaisugi[voice]" in result.output


def test_voice_serve_exits_1_when_the_engine_name_is_unknown(monkeypatch, tmp_path):
    from opendaisugi.voice.engines import UnknownEngine

    monkeypatch.setattr(
        "opendaisugi.voice.server.serve",
        _serve_raises(
            UnknownEngine("Unknown voice_engine 'espeak'. Valid names: faster-whisper, parakeet.")
        ),
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Valid names: faster-whisper, parakeet" in result.output


def test_voice_serve_exits_3_when_no_floor_backend_is_available(monkeypatch, tmp_path):
    from opendaisugi.exceptions import FloorNotAvailable

    monkeypatch.setattr(
        "opendaisugi.voice.server.serve",
        _serve_raises(
            FloorNotAvailable(
                "no pane backend is available. Start one: `coppice server start`, or install tmux."
            )
        ),
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert "coppice server start" in result.output


def test_voice_serve_exits_1_when_the_port_is_already_in_use(monkeypatch, tmp_path):
    monkeypatch.setattr(
        "opendaisugi.voice.server.serve",
        _serve_raises(OSError("[Errno 98] Address already in use")),
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1, result.output
    assert "Address already in use" in result.output


def test_voice_serve_exits_1_on_a_half_given_tls_pair(monkeypatch, tmp_path):
    monkeypatch.setattr(
        "opendaisugi.voice.server.serve",
        _serve_raises(ValueError("pass both --tls-cert and --tls-key, or pass neither")),
    )
    result = runner.invoke(
        app, ["voice", "serve", "--tls-key", "key.pem", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1, result.output
    assert "pass both --tls-cert and --tls-key" in result.output


def test_voice_serve_exits_0_on_keyboard_interrupt(monkeypatch, tmp_path):
    monkeypatch.setattr(
        "opendaisugi.voice.server.serve", _serve_announces_then_stops(keyboard_interrupt=True)
    )
    result = runner.invoke(app, ["voice", "serve", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output


def test_voice_serve_exits_1_on_a_malformed_listen_value(monkeypatch, tmp_path):
    def _raise(**kwargs):
        raise AssertionError("serve must not be called when --listen fails to parse")

    monkeypatch.setattr("opendaisugi.voice.server.serve", _raise)
    result = runner.invoke(
        app, ["voice", "serve", "--listen", "no-port-here", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1, result.output
    assert "--listen must be host:port" in result.output


def test_voice_serve_banner_names_https_when_tls_cert_is_given(monkeypatch, tmp_path):
    monkeypatch.setattr("opendaisugi.voice.server.serve", _serve_announces_then_stops())
    result = runner.invoke(
        app,
        [
            "voice",
            "serve",
            "--tls-cert",
            str(tmp_path / "cert.pem"),
            "--tls-key",
            str(tmp_path / "key.pem"),
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 0, result.output
    assert "https://127.0.0.1:7477" in result.output


def test_voice_ptt_invokes_main_loop_with_the_pane_and_the_configured_server_url(monkeypatch):
    from opendaisugi.config import Config

    monkeypatch.setattr(
        "opendaisugi.config.load_config",
        lambda *a, **k: Config(voice_server_url="http://127.0.0.1:9999"),
    )
    calls = []
    monkeypatch.setattr(
        "opendaisugi.voice.ptt.main_loop",
        lambda pane, *, server_url, **kwargs: calls.append((pane, server_url)),
    )
    result = runner.invoke(app, ["voice", "ptt", "w1:p1"])
    assert result.exit_code == 0, result.output
    assert calls == [("w1:p1", "http://127.0.0.1:9999")]


def test_voice_ptt_server_flag_overrides_the_config_default(monkeypatch):
    from opendaisugi.config import Config

    monkeypatch.setattr(
        "opendaisugi.config.load_config",
        lambda *a, **k: Config(voice_server_url="http://127.0.0.1:9999"),
    )
    calls = []
    monkeypatch.setattr(
        "opendaisugi.voice.ptt.main_loop",
        lambda pane, *, server_url, **kwargs: calls.append((pane, server_url)),
    )
    result = runner.invoke(app, ["voice", "ptt", "w1:p1", "--server", "http://10.0.0.5:7477"])
    assert result.exit_code == 0, result.output
    assert calls == [("w1:p1", "http://10.0.0.5:7477")]


def test_voice_ptt_passes_the_data_dir_resolved_config_through_to_main_loop(monkeypatch, tmp_path):
    calls = []
    monkeypatch.setattr(
        "opendaisugi.voice.ptt.main_loop",
        lambda pane, *, server_url, config=None, **kwargs: calls.append(
            config.data_dir if config is not None else None
        ),
    )
    result = runner.invoke(app, ["voice", "ptt", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert calls == [tmp_path]


def _ptt_main_loop_must_not_be_called(*args, **kwargs):
    raise AssertionError("main_loop must not be called on an invalid --server")


def test_voice_ptt_exits_1_on_an_invalid_ipv6_server_url(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.ptt.main_loop", _ptt_main_loop_must_not_be_called)
    result = runner.invoke(app, ["voice", "ptt", "w1:p1", "--server", "http://[::1"])
    assert result.exit_code == 1, result.output
    assert "--server must be" in result.output


def test_voice_ptt_exits_1_on_a_non_url_server_value(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.ptt.main_loop", _ptt_main_loop_must_not_be_called)
    result = runner.invoke(app, ["voice", "ptt", "w1:p1", "--server", "not-a-url"])
    assert result.exit_code == 1, result.output
    assert "--server must be" in result.output

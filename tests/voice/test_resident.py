"""The resident engine protocol and its lifecycle (voice/resident.py), with
the fake engine of clients/fake_resident.py. Every path is passed in; no
test changes PATH or the environment."""

from __future__ import annotations

import importlib.util
import json
import os
import signal
import threading
import time
from pathlib import Path

import pytest

from opendaisugi.voice import engines, resident

from .conftest import make_wav

_spec = importlib.util.spec_from_file_location(
    "fake_resident", Path(__file__).resolve().parents[2] / "clients" / "fake_resident.py"
)
assert _spec is not None and _spec.loader is not None
_fake_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_fake_mod)
write_fake = _fake_mod.write_fake


class Unavailable(Exception):
    pass


class Loading(Unavailable):
    pass


CLIP = make_wav(sr=16000, n_samples=8000)


def _fake(tmp_path: Path, spec: dict, name: str = "engine") -> tuple[Path, Path]:
    (tmp_path / "bin").mkdir(exist_ok=True)
    spec_file = tmp_path / "spec.json"
    spec_file.write_text(json.dumps(spec))
    log = tmp_path / "log.jsonl"
    exe = tmp_path / "bin" / name
    write_fake(exe, spec_file, log)
    return exe, log


def _log(log: Path) -> list[dict]:
    if not log.exists():
        return []
    return [json.loads(ln) for ln in log.read_text().splitlines()]


def _engine(exe: Path, **kw) -> resident.ResidentEngine:
    return resident.ResidentEngine(
        "fake",
        [str(exe), "-m", "MODEL"],
        binary="fake-cli",
        unavailable=Unavailable,
        loading=Loading,
        **kw,
    )


@pytest.fixture
def started():
    made: list[resident.ResidentEngine] = []

    def start(exe: Path, **kw) -> resident.ResidentEngine:
        e = _engine(exe, **kw)
        made.append(e)
        e.start()
        return e

    yield start
    for e in made:
        e.stop()


def test_a_frame_is_the_big_endian_length_then_the_clip():
    assert resident.frame(b"abc") == b"\x00\x00\x00\x03abc"


def test_reply_lines_are_text_or_error_objects():
    assert resident.reply_kind(resident.parse_line(b'{"text": "hi", "decode_ms": 3}\n')) == "text"
    assert resident.reply_kind(resident.parse_line(b'{"error": "bad"}\n')) == "error"
    assert resident.reply_kind(resident.parse_line(b'{"text": 3}\n')) is None
    assert resident.reply_kind(resident.parse_line(b"[1]\n")) is None
    assert resident.reply_kind(resident.parse_line(b"\xff\n")) is None
    assert resident.is_ready(resident.parse_line(b'{"ready": "daisugi-voice-1"}\n'))
    assert not resident.is_ready(resident.parse_line(b'{"ready": "daisugi-voice-2"}\n'))


def test_exit_reasons_name_the_code_or_the_signal_and_the_last_stderr_line():
    assert resident.exit_reason(4, ["a", "boom "]) == "exited 4: boom"
    assert resident.exit_reason(-9, []) == "killed by signal 9"


def test_one_load_answers_many_clips(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"text": "one"}, {"text": " two \n three "}]})
    e = started(exe)
    assert e.transcribe_text(CLIP) == "one"
    assert e.transcribe_text(CLIP) == " two \n three "
    assert e.transcribe_text(CLIP) == " two \n three "
    entries = _log(log)
    assert entries[0]["start"] == ["-m", "MODEL"]
    assert [x for x in entries if "start" in x] == [entries[0]]
    assert len([x for x in entries if "clip" in x]) == 3


def test_a_load_error_line_stops_the_start(tmp_path):
    exe, _ = _fake(tmp_path, {"starts": [{"error": "the model /m did not load"}]})
    with pytest.raises(Unavailable, match="^fake-cli did not start: the model /m did not load$"):
        _engine(exe).start()


def test_a_child_that_exits_before_ready_names_its_last_stderr_line(tmp_path):
    exe, _ = _fake(tmp_path, {"starts": [{"exit": 4, "stderr": "one\nboom\n"}]})
    with pytest.raises(Unavailable, match="^fake-cli did not start: exited 4: boom$"):
        _engine(exe).start()


def test_a_first_line_that_is_not_the_ready_line_stops_the_start(tmp_path):
    exe, _ = _fake(tmp_path, {"starts": [{"line": "hello"}]})
    with pytest.raises(Unavailable, match="was not the daisugi-voice-1 ready line"):
        _engine(exe).start()


def test_a_slow_load_past_the_timeout_stops_the_start(tmp_path):
    exe, _ = _fake(tmp_path, {"starts": [{"delay": 5}]})
    t0 = time.monotonic()
    with pytest.raises(Unavailable, match="did not load its model in 0.5 seconds"):
        _engine(exe, load_timeout_s=0.5).start()
    assert time.monotonic() - t0 < 4


def test_a_program_that_is_not_there_does_not_start(tmp_path):
    with pytest.raises(Unavailable, match="^fake-cli did not start: "):
        _engine(tmp_path / "absent").start()


def test_an_error_reply_refuses_that_clip_and_keeps_the_child(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"error": "the clip is not a WAV"}, {"text": "ok"}]})
    e = started(exe)
    with pytest.raises(RuntimeError, match="^fake-cli: the clip is not a WAV$"):
        e.transcribe_text(CLIP)
    assert e.transcribe_text(CLIP) == "ok"
    assert len([x for x in _log(log) if "start" in x]) == 1


def test_a_crash_inside_a_clip_restarts_the_child(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"crash": 7, "stderr": "died\n"}, {"text": "back"}]})
    e = started(exe)
    with pytest.raises(Unavailable, match=r"stopped during a clip \(exited 7: died\)\. It is"):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(CLIP) == "back"
    assert len([x for x in _log(log) if "start" in x]) == 2


def test_a_malformed_reply_restarts_the_child(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"line": "not json"}, {"text": "fine"}]})
    e = started(exe)
    with pytest.raises(Unavailable, match="gave a reply that is not one JSON object with text"):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(CLIP) == "fine"
    assert len([x for x in _log(log) if "start" in x]) == 2


def test_a_clip_with_no_reply_in_time_restarts_the_child(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"delay": 5, "text": "late"}, {"text": "fine"}]})
    e = started(exe, clip_timeout_s=0.5)
    with pytest.raises(Unavailable, match="did not answer in 0.5 seconds"):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(CLIP) == "fine"


def test_a_child_that_stops_reading_cannot_hold_a_clip_past_the_timeout(tmp_path, started):
    # Five seconds of audio is bigger than a pipe's buffer, so the write
    # itself blocks while the child does not read.
    big = make_wav(sr=16000, n_samples=80000)
    exe, _ = _fake(tmp_path, {"starts": [{"stall": 30}, {}], "replies": [{"text": "x"}]})
    e = started(exe, clip_timeout_s=0.5)
    t0 = time.monotonic()
    with pytest.raises(Unavailable, match="did not answer in 0.5 seconds"):
        e.transcribe_text(big)
    assert time.monotonic() - t0 < 5
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(big) == "x"


def test_clips_are_refused_while_a_child_loads(tmp_path, started):
    exe, _ = _fake(
        tmp_path,
        {"starts": [{}, {"delay": 1.5}], "replies": [{"crash": 1}, {"text": "after"}]},
    )
    e = started(exe)
    with pytest.raises(Unavailable):
        e.transcribe_text(CLIP)
    with pytest.raises(Loading, match="^The speech engine is loading its model"):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(CLIP) == "after"


def test_a_failed_restart_waits_for_the_next_clip(tmp_path, started):
    exe, log = _fake(
        tmp_path,
        {
            "starts": [{}, {"error": "no model"}, {}],
            "replies": [{"crash": 1}, {"text": "third start"}],
        },
    )
    e = started(exe)
    with pytest.raises(Unavailable):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "failed"
    time.sleep(0.3)
    assert len([x for x in _log(log) if "start" in x]) == 2  # no loop
    with pytest.raises(Unavailable, match="did not start again: no model. The next clip tries"):
        e.transcribe_text(CLIP)
    assert e.wait_ready(10) == "ready"
    assert e.transcribe_text(CLIP) == "third start"


def test_a_child_that_dies_while_idle_is_started_again_at_once(tmp_path, started):
    exe, log = _fake(tmp_path, {})
    e = started(exe)
    first = e.pid
    assert first is not None
    os.kill(first, signal.SIGKILL)
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline and (e.pid == first or e.wait_ready(0) != "ready"):
        time.sleep(0.05)
    assert e.pid not in (None, first)
    assert e.transcribe_text(CLIP) == "hello world"
    assert len([x for x in _log(log) if "start" in x]) == 2


def test_clips_that_arrive_together_are_answered_one_at_a_time(tmp_path, started):
    exe, log = _fake(tmp_path, {"replies": [{"delay": 0.2, "text": "a"}]})
    e = started(exe)
    got: list[str] = []
    threads = [
        threading.Thread(target=lambda: got.append(e.transcribe_text(CLIP))) for _ in range(3)
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert got == ["a", "a", "a"]
    assert len([x for x in _log(log) if "clip" in x]) == 3


def test_stop_ends_the_child_through_its_stdin(tmp_path):
    exe, log = _fake(tmp_path, {})
    e = _engine(exe)
    e.start()
    pid = e.pid
    e.stop()
    assert _log(log)[-1] == {"eof": True}
    with pytest.raises(ProcessLookupError):
        os.kill(pid, 0)
    with pytest.raises(Unavailable, match="has stopped"):
        e.transcribe_text(CLIP)


# --- the engines that run resident -----------------------------------------


def test_moonshine_runs_its_program_resident_with_the_model_and_arch(tmp_path):
    exe, log = _fake(tmp_path, {"replies": [{"text": " Hello there. \n"}]}, "moonshine-cli")
    model = tmp_path / "moon"
    model.mkdir()
    e = engines.MoonshineEngine(str(model), search_path=str(tmp_path / "bin"))
    e.start()
    try:
        t = e.transcribe(CLIP, language="de")
    finally:
        e.stop()
    assert t.text == "Hello there."
    assert t.duration_s == pytest.approx(0.5)
    assert _log(log)[0]["start"] == ["-m", str(model), "-a", "small", "--resident"]


def test_parakeet_runs_its_program_resident_with_the_model_file(tmp_path):
    exe, log = _fake(tmp_path, {"replies": [{"text": "hi"}]}, "parakeet-cli")
    model = tmp_path / "p.gguf"
    model.write_text("x")
    e = engines.ParakeetEngine(str(model), search_path=str(tmp_path / "bin"))
    e.start()
    try:
        assert e.transcribe(CLIP).text == "hi"
    finally:
        e.stop()
    assert _log(log)[0]["start"] == ["-m", str(model), "--resident"]


def test_parakeet_needs_its_program_and_a_model_file(tmp_path):
    (tmp_path / "bin").mkdir()
    with pytest.raises(engines.EngineUnavailable, match="^parakeet-cli is not on PATH. Build it"):
        engines.ParakeetEngine(str(tmp_path / "p.gguf"), search_path=str(tmp_path / "bin"))
    _fake(tmp_path, {}, "parakeet-cli")
    with pytest.raises(engines.EngineUnavailable, match="^Parakeet model file missing: "):
        engines.ParakeetEngine(str(tmp_path / "p.gguf"), search_path=str(tmp_path / "bin"))

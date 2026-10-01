from __future__ import annotations

from pathlib import Path

import pytest

from opendaisugi.config import Config
from opendaisugi.voice import engines, pins

from .conftest import make_wav, requires_faster_whisper
from .fixtures import generate_fixtures, has_speech_synthesis


def _word_error_rate(ref: str, hyp: str) -> float:
    def norm(s: str) -> list[str]:
        return "".join(c.lower() if c.isalnum() or c.isspace() else " " for c in s).split()

    r, h = norm(ref), norm(hyp)
    d = [[0] * (len(h) + 1) for _ in range(len(r) + 1)]
    for i in range(len(r) + 1):
        d[i][0] = i
    for j in range(len(h) + 1):
        d[0][j] = j
    for i in range(1, len(r) + 1):
        for j in range(1, len(h) + 1):
            cost = 0 if r[i - 1] == h[j - 1] else 1
            d[i][j] = min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + cost)
    return d[len(r)][len(h)] / max(1, len(r))


@requires_faster_whisper
def test_pick_engine_defaults_to_faster_whisper_on_cpu(monkeypatch):
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            captured["model"] = model
            captured["device"] = device
            captured["compute_type"] = compute_type

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engine = engines.pick_engine(Config())
    assert isinstance(engine, engines.FasterWhisperEngine)
    assert captured["device"] == "cpu"
    assert captured["model"] == pins.FASTER_WHISPER_TEST_MODEL
    assert captured["compute_type"] == "int8"


@requires_faster_whisper
def test_pick_engine_forwards_a_non_default_compute_type(monkeypatch):
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            captured["compute_type"] = compute_type

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.pick_engine(Config(voice_compute_type="float16"))
    assert captured["compute_type"] == "float16"


def test_pick_engine_raises_unknown_engine_for_a_bad_name():
    with pytest.raises(engines.UnknownEngine) as exc_info:
        engines.pick_engine(Config(voice_engine="parkeet-typo"))
    message = str(exc_info.value)
    assert "faster-whisper" in message
    assert "sherpa-onnx" not in message


def test_parakeet_is_an_engine_again():
    assert engines.ParakeetEngine.name == "parakeet"


@requires_faster_whisper
def test_faster_whisper_engine_falls_back_to_cpu_when_cuda_unavailable(monkeypatch):
    monkeypatch.setattr(engines, "_cuda_available", lambda: False)
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            captured["device"] = device

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en", device="cuda", compute_type="int8")
    assert captured["device"] == "cpu"


@requires_faster_whisper
def test_faster_whisper_engine_keeps_cuda_when_available(monkeypatch):
    monkeypatch.setattr(engines, "_cuda_available", lambda: True)
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            captured["device"] = device

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en", device="cuda", compute_type="int8")
    assert captured["device"] == "cuda"


@requires_faster_whisper
def test_faster_whisper_transcribe_maps_segments_text_and_duration(monkeypatch):
    class FakeSegment:
        def __init__(self, start, end, text):
            self.start = start
            self.end = end
            self.text = text

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            pass

        def transcribe(self, audio, language=None):
            segments = [
                FakeSegment(0.0, 0.5, " hello"),
                FakeSegment(0.5, 1.0, " world "),
            ]
            return iter(segments), None

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engine = engines.FasterWhisperEngine("tiny.en", device="cpu", compute_type="int8")
    wav_bytes = make_wav(sr=16000, n_samples=16000)

    result = engine.transcribe(wav_bytes)

    assert result.text == "hello world"
    assert result.segments == [
        {"start": 0.0, "end": 0.5, "text": "hello"},
        {"start": 0.5, "end": 1.0, "text": "world"},
    ]
    assert result.duration_s == pytest.approx(1.0)
    assert result.rtf >= 0.0


@requires_faster_whisper
@pytest.mark.voice_live
def test_faster_whisper_transcribes_fixtures_within_the_recorded_wer_ceiling(tmp_path):
    if not has_speech_synthesis():
        pytest.skip("espeak-ng and/or ffmpeg not on PATH: no real speech fixture to score")
    transcripts = generate_fixtures(tmp_path)
    engine = engines.FasterWhisperEngine(
        pins.FASTER_WHISPER_TEST_MODEL, device="cpu", compute_type="int8"
    )
    total_wer = 0.0
    for name, expected in transcripts.items():
        wav_bytes = (tmp_path / name).read_bytes()
        result = engine.transcribe(wav_bytes)
        wer = _word_error_rate(expected, result.text)
        assert wer <= pins.FIXTURE_MAX_WER[name], f"{name}: wer {wer:.3f}, got {result.text!r}"
        total_wer += wer
    assert total_wer / len(transcripts) <= pins.FIXTURE_MEAN_MAX_WER


def _fake_whisper_cli(bin_dir: Path, body: str) -> Path:
    """Write a fake whisper-cli into bin_dir. It runs body as /bin/sh."""
    bin_dir.mkdir(parents=True, exist_ok=True)
    script = bin_dir / "whisper-cli"
    script.write_text("#!/bin/sh\n" + body)
    script.chmod(0o755)
    return script


def test_whisper_cpp_args_name_the_model_the_file_and_no_extra_output():
    args = engines.whisper_cpp_args("/bin/whisper-cli", Path("/m/ggml.bin"), "/t/clip.wav", None)
    assert args == [
        "/bin/whisper-cli",
        "-m",
        "/m/ggml.bin",
        "-f",
        "/t/clip.wav",
        "-l",
        "auto",
        "-nt",
        "-np",
    ]


def test_whisper_cpp_args_pass_a_named_language():
    args = engines.whisper_cpp_args("w", Path("m"), "f", "de")
    assert args[args.index("-l") + 1] == "de"


@pytest.mark.parametrize(
    ("stdout", "text"),
    [
        ("\n hello world\n", "hello world"),
        ("\n one\n two \n\n", "one two"),
        ("", ""),
        ("   \n\t\n", ""),
        ("a\r\nb\r\n", "a b"),
    ],
)
def test_whisper_cpp_output_joins_the_stripped_lines(stdout, text):
    assert engines.whisper_cpp_text(stdout) == text


def test_whisper_cpp_engine_is_unavailable_with_no_binary_on_path(monkeypatch, tmp_path):
    model = tmp_path / "ggml.bin"
    model.write_bytes(b"x")
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    with pytest.raises(engines.EngineUnavailable, match="whisper-cli is not on PATH"):
        engines.WhisperCppEngine(model)


def test_whisper_cpp_engine_is_unavailable_with_no_model_file(monkeypatch, tmp_path):
    _fake_whisper_cli(tmp_path / "bin", "exit 0\n")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    with pytest.raises(engines.EngineUnavailable, match="model file missing"):
        engines.WhisperCppEngine(tmp_path / "absent.bin")


def test_whisper_cpp_engine_runs_the_binary_and_reads_its_text(monkeypatch, tmp_path):
    log = tmp_path / "argv.txt"
    _fake_whisper_cli(
        tmp_path / "bin",
        f'printf "%s\\n" "$@" > {log}\nprintf "\\n hello there\\n"\n',
    )
    model = tmp_path / "ggml.bin"
    model.write_bytes(b"x")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    engine = engines.WhisperCppEngine(model)
    assert engine.name == "whisper.cpp"
    result = engine.transcribe(make_wav(sr=16000, n_samples=8000))
    assert result.text == "hello there"
    assert result.segments == [{"start": 0.0, "end": 0.5, "text": "hello there"}]
    assert result.duration_s == pytest.approx(0.5)
    argv = log.read_text().splitlines()
    assert argv[:2] == ["-m", str(model)]
    assert argv[2] == "-f" and argv[3].endswith(".wav")
    assert argv[4:] == ["-l", "auto", "-nt", "-np"]
    assert not Path(argv[3]).exists(), "the temp clip must be removed"


def test_whisper_cpp_engine_raises_when_the_binary_fails(monkeypatch, tmp_path):
    _fake_whisper_cli(tmp_path / "bin", 'echo "error: bad model" >&2\nexit 3\n')
    model = tmp_path / "ggml.bin"
    model.write_bytes(b"x")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    engine = engines.WhisperCppEngine(model)
    with pytest.raises(RuntimeError, match="whisper-cli exited 3: error: bad model"):
        engine.transcribe(make_wav(sr=16000, n_samples=1600))


def test_pick_engine_routes_to_whisper_cpp(monkeypatch, tmp_path):
    _fake_whisper_cli(tmp_path / "bin", "exit 0\n")
    model = tmp_path / "ggml.bin"
    model.write_bytes(b"x")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    engine = engines.pick_engine(Config(voice_engine="whisper.cpp", voice_model=str(model)))
    assert isinstance(engine, engines.WhisperCppEngine)


def test_unknown_engine_names_the_valid_names():
    with pytest.raises(
        engines.UnknownEngine,
        match="Valid names: faster-whisper, moonshine, parakeet, whisper.cpp[.]",
    ):
        engines.pick_engine(Config(voice_engine="nope"))

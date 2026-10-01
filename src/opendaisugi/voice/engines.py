"""Speech-to-text engines: a Protocol plus the adapters.

FasterWhisperEngine wraps CTranslate2 through the faster-whisper package. It
keeps its model loaded in this process. It runs on CPU with int8 unless Config.voice_device
is "cuda" and ctranslate2 actually reports a CUDA device. CUDA is opt-in and
verified, never auto-detected. This workshop's own box has a GPU that has
crashed other torch-based inference under CUDA before. See ADR-0019. This
engine's CUDA path is untested on it.

WhisperCppEngine runs the whisper.cpp command-line binary, whisper-cli, as
an external process. It needs no Python package, so the Go and Rust ports
run the same engine the same way. The model stays outside this repo: a
ggml model file that voice_model names by path. See pins.WHISPER_CPP_BINARY.

MoonshineEngine and ParakeetEngine run as one resident child each
(resident.py): moonshine-cli and parakeet-cli, this repo's own small
programs on Moonshine's C API and on CrispASR's Parakeet code
(clients/native), load their model once and answer clip after clip. The
model is a curated one, fetched once into the user cache and checked
against a pinned sha256, or one the config names by path. See
pins.MOONSHINE_MODELS, pins.PARAKEET_MODELS and rulings VO-13 and VO-16.
whisper.cpp stays one process per clip (ruling VO-18).

A config that leaves voice_engine and voice_model at their defaults (or
unset) gets the engine the hardware picks (choose_engine, ruling VO-17),
with one line that says which and why. That is what lets the Go and Rust
daisugi, which cannot load faster-whisper, serve voice with no setup.
"""

from __future__ import annotations

import importlib
import io
import os
import shutil
import subprocess
import sys
import tempfile
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Protocol

from opendaisugi import hardware as _hardware
from opendaisugi._model_fetch import FetchVerificationError
from opendaisugi.voice import models, pins, resident
from opendaisugi.voice.audio import wav_duration_s

if TYPE_CHECKING:
    from opendaisugi.config import Config


@dataclass(frozen=True)
class Transcript:
    text: str
    segments: list[dict]
    duration_s: float
    rtf: float  # wall time divided by duration_s. Above 1 means slower than real time.


class Engine(Protocol):
    name: str

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript: ...

    def start(self) -> None: ...

    def stop(self) -> None: ...


class _InProcess:
    """An engine with no child to start: start and stop do nothing."""

    def start(self) -> None:
        return None

    def stop(self) -> None:
        return None


class EngineUnavailable(RuntimeError):
    """The configured engine's package, or its model files, are not available on this host."""


class UnknownEngine(ValueError):
    """config.voice_engine names an engine this module does not build."""


VALID_ENGINE_NAMES = "faster-whisper, moonshine, parakeet, whisper.cpp"


def _say(line: str) -> None:
    """One line for the person who started the server, on stderr."""
    print(line, file=sys.stderr, flush=True)


def _fetch_failed(what: str) -> str:
    return f"The {what} model could not be fetched. Check the network, then run daisugi voice serve again."


def _cuda_available() -> bool:
    """Best-effort probe for a visible CUDA device. Never raises."""
    try:
        import ctranslate2

        return ctranslate2.get_cuda_device_count() > 0
    except Exception:
        return False


class FasterWhisperEngine(_InProcess):
    name = "faster-whisper"

    def __init__(
        self,
        model: str = pins.FASTER_WHISPER_TEST_MODEL,
        *,
        device: str = "cpu",
        compute_type: str = "int8",
    ) -> None:
        try:
            from faster_whisper import WhisperModel
        except ImportError as exc:
            raise EngineUnavailable(
                "faster-whisper is not installed. Install it with: pip install 'opendaisugi[voice]'"
            ) from exc
        resolved_device = device
        if device == "cuda" and not _cuda_available():
            resolved_device = "cpu"
        revision = pins.FASTER_WHISPER_REVISIONS.get(model)

        def load(local_only: bool):
            return WhisperModel(
                model,
                device=resolved_device,
                compute_type=compute_type,
                local_files_only=local_only,
                revision=revision,
            )

        from huggingface_hub.errors import LocalEntryNotFoundError

        # The cache first, with no network. Only a model that is not in the
        # cache is fetched, after one line; any other load error is that
        # error.
        try:
            self._model = load(True)
        except LocalEntryNotFoundError:
            size = pins.FASTER_WHISPER_SIZES.get(model)
            size_note = f" ({(size + 500_000) // 1_000_000} MB)" if size else ""
            _say(f"Fetching the faster-whisper {model} model{size_note}. This happens once.")
            try:
                self._model = load(False)
            except ValueError:
                raise
            except Exception as exc:
                raise EngineUnavailable(_fetch_failed(f"faster-whisper {model}")) from exc
        self.model_name = model
        self.device = resolved_device

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        duration_s = wav_duration_s(wav_16k_mono)
        start = time.monotonic()
        segments_iter, _info = self._model.transcribe(io.BytesIO(wav_16k_mono), language=language)
        segments = [{"start": s.start, "end": s.end, "text": s.text.strip()} for s in segments_iter]
        elapsed = time.monotonic() - start
        text = " ".join(s["text"] for s in segments).strip()
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


def whisper_cpp_args(binary: str, model: Path, wav_path: str, language: str | None) -> list[str]:
    """The whisper-cli command line for one clip.

    language None asks whisper.cpp to detect the language, the same as
    faster-whisper's language=None. whisper.cpp's own default is English.
    """
    return [
        binary,
        "-m",
        str(model),
        "-f",
        wav_path,
        "-l",
        language or pins.WHISPER_CPP_AUTO_LANGUAGE,
        "-nt",
        "-np",
    ]


def whisper_cpp_text(stdout: str) -> str:
    """The transcript in whisper-cli's stdout: each non-blank line stripped, joined by a space."""
    return " ".join(line.strip() for line in stdout.splitlines() if line.strip())


class WhisperCppEngine(_InProcess):
    name = "whisper.cpp"

    def __init__(self, model: Path) -> None:
        binary = shutil.which(pins.WHISPER_CPP_BINARY)
        if binary is None:
            raise EngineUnavailable(
                f"{pins.WHISPER_CPP_BINARY} is not on PATH. Build whisper.cpp and put "
                f"{pins.WHISPER_CPP_BINARY} on PATH, then try again."
            )
        if not model.is_file():
            raise EngineUnavailable(
                f"whisper.cpp model file missing: {model}. Download a ggml model, "
                "then set voice_model to its path."
            )
        self.binary = binary
        self.model = model

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        duration_s = wav_duration_s(wav_16k_mono)
        fd, wav_path = tempfile.mkstemp(prefix="daisugi-voice-", suffix=".wav")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(wav_16k_mono)
            start = time.monotonic()
            proc = subprocess.run(
                whisper_cpp_args(self.binary, self.model, wav_path, language),
                stdin=subprocess.DEVNULL,
                capture_output=True,
                timeout=pins.WHISPER_CPP_TIMEOUT_S,
            )
            elapsed = time.monotonic() - start
        finally:
            os.unlink(wav_path)
        if proc.returncode != 0:
            lines = proc.stderr.decode("utf-8", errors="replace").strip().splitlines()
            last = lines[-1].strip()[:200] if lines else ""
            raise RuntimeError(f"{pins.WHISPER_CPP_BINARY} exited {proc.returncode}: {last}")
        text = whisper_cpp_text(proc.stdout.decode("utf-8", errors="replace"))
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        segments = [{"start": 0.0, "end": duration_s, "text": text}]
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


def moonshine_args(binary: str, model: Path, arch: str) -> list[str]:
    """The moonshine-cli command line of the resident child."""
    return [binary, "-m", str(model), "-a", arch, "--resident"]


def parakeet_args(binary: str, model: Path) -> list[str]:
    """The parakeet-cli command line of the resident child."""
    return [binary, "-m", str(model), "--resident"]


class EngineLoading(EngineUnavailable):
    """The resident engine is loading its model, so a clip is refused (503)."""


class _Resident:
    """A speech engine run as one resident child (see resident.py)."""

    name = ""
    binary_name = ""

    def _resident_setup(
        self, argv: list[str], load_timeout_s: float, clip_timeout_s: float
    ) -> None:
        self.argv = argv
        self._resident = resident.ResidentEngine(
            self.name,
            argv,
            binary=self.binary_name,
            unavailable=EngineUnavailable,
            loading=EngineLoading,
            load_timeout_s=load_timeout_s,
            clip_timeout_s=clip_timeout_s,
        )

    def start(self) -> None:
        """Start the child and wait for its model. Raises EngineUnavailable."""
        self._resident.start()

    def stop(self) -> None:
        self._resident.stop()

    def wait_ready(self, timeout: float) -> str:
        return self._resident.wait_ready(timeout)

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        """The clip's text. The models are English, so language is not passed on."""
        raw, duration_s, elapsed = self._resident.transcribe(wav_16k_mono)
        text = whisper_cpp_text(raw)
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        segments = [{"start": 0.0, "end": duration_s, "text": text}]
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


class MoonshineEngine(_Resident):
    name = "moonshine"
    binary_name = pins.MOONSHINE_BINARY

    def __init__(
        self,
        model: str,
        *,
        search_path: str | None = None,
        load_timeout_s: float = resident.LOAD_TIMEOUT_S,
        clip_timeout_s: float = resident.CLIP_TIMEOUT_S,
    ) -> None:
        binary = shutil.which(pins.MOONSHINE_BINARY, path=search_path)
        if binary is None:
            raise EngineUnavailable(
                f"{pins.MOONSHINE_BINARY} is not on PATH. Build it with "
                "clients/go/scripts/native.sh --moonshine (scripts/install.sh does this "
                "and puts it on PATH), then try again."
            )
        if model in pins.MOONSHINE_MODELS:
            try:
                model_dir = models.ensure_moonshine(model, _say)
            except FetchVerificationError as exc:
                raise EngineUnavailable(
                    f"A file of the Moonshine {model} model did not match its pinned sha256 "
                    "and was deleted. Run daisugi voice serve again to fetch it again."
                ) from exc
            except OSError as exc:
                raise EngineUnavailable(_fetch_failed(f"Moonshine {model}")) from exc
            arch = model
        else:
            model_dir = Path(model)
            if not model_dir.is_dir():
                raise EngineUnavailable(
                    f"Moonshine model directory missing: {model_dir}. Set voice_model to "
                    "tiny, small or medium, or to a Moonshine streaming model directory."
                )
            arch = pins.MOONSHINE_DIR_ARCH
        self.binary = binary
        self.model = model_dir
        self.arch = arch
        self._resident_setup(
            moonshine_args(binary, model_dir, arch), load_timeout_s, clip_timeout_s
        )


class ParakeetEngine(_Resident):
    name = "parakeet"
    binary_name = pins.PARAKEET_BINARY

    def __init__(
        self,
        model: str,
        *,
        search_path: str | None = None,
        load_timeout_s: float = resident.LOAD_TIMEOUT_S,
        clip_timeout_s: float = resident.CLIP_TIMEOUT_S,
    ) -> None:
        binary = shutil.which(pins.PARAKEET_BINARY, path=search_path)
        if binary is None:
            raise EngineUnavailable(
                f"{pins.PARAKEET_BINARY} is not on PATH. Build it with "
                "clients/go/scripts/native.sh --parakeet (scripts/install.sh does this "
                "and puts it on PATH), then try again."
            )
        if model in pins.PARAKEET_MODELS:
            quantizer = Path(os.path.realpath(binary)).parent / pins.PARAKEET_QUANTIZE_BINARY
            try:
                path = models.ensure_parakeet(model, quantizer, _say)
            except FetchVerificationError as exc:
                raise EngineUnavailable(
                    f"A file of the Parakeet {model} model did not match its pinned sha256 "
                    "and was deleted. Run daisugi voice serve again to fetch it again."
                ) from exc
            except models.QuantizeError as exc:
                raise EngineUnavailable(str(exc)) from exc
            except OSError as exc:
                raise EngineUnavailable(_fetch_failed(f"Parakeet {model}")) from exc
        else:
            path = Path(model)
            if not path.is_file():
                raise EngineUnavailable(
                    f"Parakeet model file missing: {path}. Set voice_model to "
                    f"{pins.PARAKEET_DEFAULT_MODEL}, or to a Parakeet-TDT GGUF file."
                )
        self.binary = binary
        self.model = path
        self._resident_setup(parakeet_args(binary, path), load_timeout_s, clip_timeout_s)


def _faster_whisper_imports() -> bool:
    try:
        importlib.import_module("faster_whisper")
    except Exception:
        return False
    return True


# The Config defaults. save_config writes every field, so a box that ever
# saved its config names these values without choosing them: they count as
# no choice (ruling VO-13).
DEFAULT_ENGINE = "faster-whisper"
DEFAULT_MODEL = "tiny.en"


# The engine a box with no voice choice gets lives in the layer's hardware
# module, so the module map can ask it without importing this package
# (ADR-0020). The names, again here for the voice code:
HARDWARE_ENV = _hardware.HARDWARE_ENV
PARAKEET_MIN_RAM_GB = _hardware.PARAKEET_MIN_RAM_GB
PARAKEET_MIN_CPUS = _hardware.PARAKEET_MIN_CPUS
MOONSHINE_MIN_RAM_GB = _hardware.MOONSHINE_MIN_RAM_GB
VoiceHardware = _hardware.VoiceHardware
choose_engine = _hardware.choose_engine
detect_voice_hardware = _hardware.detect_voice_hardware
hardware_order = _hardware.hardware_order


def installed_engines(search_path: str | None = None) -> tuple[set[str], bool]:
    """The engines this box can run now, and whether faster-whisper imports."""
    fw = _faster_whisper_imports()
    have = {"faster-whisper"} if fw else set()
    if shutil.which(pins.PARAKEET_BINARY, path=search_path):
        have.add("parakeet")
    if shutil.which(pins.MOONSHINE_BINARY, path=search_path):
        have.add("moonshine")
    return have, fw


def resolve_engine(
    config: "Config",
    *,
    say: Callable[[str], None] | None = None,
    hardware: VoiceHardware | None = None,
) -> tuple[str, str]:
    """The engine and model a config means.

    A config whose voice_engine and voice_model are both absent or at their
    defaults means the engine the hardware picks (choose_engine), after one
    line that says which and why. ``hardware`` replaces the probe (tests). Moonshine or Parakeet with voice_model
    absent or at its default means their default model.
    """
    model_default = config.voice_model == DEFAULT_MODEL
    if config.voice_engine == DEFAULT_ENGINE and model_default:
        installed, fw = installed_engines()
        hw = hardware if hardware is not None else detect_voice_hardware()
        engine, model, line = choose_engine(hw, installed, fw)
        (say or _say)(line)
        return engine, model
    if config.voice_engine == "moonshine" and model_default:
        return "moonshine", pins.MOONSHINE_DEFAULT_MODEL
    if config.voice_engine == "parakeet" and model_default:
        return "parakeet", pins.PARAKEET_DEFAULT_MODEL
    return config.voice_engine, config.voice_model


def pick_engine(config: "Config", *, hardware: VoiceHardware | None = None) -> Engine:
    """Return the engine the config means (see resolve_engine), not started.

    faster-whisper runs on CPU by default. It runs on CUDA only when
    config.voice_device is "cuda" and a CUDA device is actually visible.
    whisper.cpp checks that whisper-cli is on PATH and that voice_model
    names a model file. moonshine and parakeet check that their program is
    on PATH, then fetch a curated model or check the one named. Any other
    voice_engine value raises UnknownEngine naming the valid names.
    """
    engine, model = resolve_engine(config, hardware=hardware)
    if engine == "faster-whisper":
        return FasterWhisperEngine(
            model, device=config.voice_device, compute_type=config.voice_compute_type
        )
    if engine == "whisper.cpp":
        return WhisperCppEngine(Path(model))
    if engine == "moonshine":
        return MoonshineEngine(model)
    if engine == "parakeet":
        return ParakeetEngine(model)
    raise UnknownEngine(f"Unknown voice_engine {engine!r}. Valid names: {VALID_ENGINE_NAMES}.")

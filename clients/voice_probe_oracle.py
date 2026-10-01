"""The oracle side of the voice probe: one query on stdin, one JSON answer on stdout.

    uv run --no-sync python clients/voice_probe_oracle.py < query.json

clients/voice_cases.py runs it for every probe case, and runs the port's
voice-probe binary on the same query, in the same scratch world (the same
PATH, HOME and TMPDIR). A query names an ``op``; each op calls one pure part
of src/opendaisugi/voice the way a caller does and reports what came back.
Every failure the voice code raises is reported as its class name and its
message, never as a traceback.

Bytes travel as base64. Each answer is a JSON object.

faster_whisper is hidden, as the binaries are built (ruling VO-13): no probe
loads a real model, and a config with no voice settings picks what a port
picks.
"""

from __future__ import annotations

import base64
import json
import os
import sys
import urllib.parse
from pathlib import Path
from typing import Any

sys.modules["faster_whisper"] = None  # type: ignore[assignment]


def _b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


def _unb64(s: str) -> bytes:
    return base64.b64decode(s)


def _err(exc: BaseException) -> dict[str, Any]:
    return {"error": type(exc).__name__, "message": str(exc)}


def _tree(root: Path) -> dict[str, Any]:
    """Every file under root: its text (or base64 when it is not UTF-8) and mode."""
    out: dict[str, Any] = {}
    if not root.exists():
        return out
    for p in sorted(root.rglob("*")):
        rel = str(p.relative_to(root))
        mode = p.stat().st_mode & 0o777
        if p.is_dir():
            out[rel + "/"] = {"mode": mode}
            continue
        raw = p.read_bytes()
        try:
            out[rel] = {"mode": mode, "text": raw.decode("utf-8")}
        except UnicodeDecodeError:
            out[rel] = {"mode": mode, "b64": _b64(raw)}
    return out


def _lay(root: Path, files: dict[str, Any]) -> None:
    for rel, spec in files.items():
        p = root / rel
        if rel.endswith("/"):
            p.mkdir(parents=True, exist_ok=True)
            p.chmod(spec.get("mode", 0o700))
            continue
        p.parent.mkdir(parents=True, exist_ok=True)
        if "b64" in spec:
            p.write_bytes(_unb64(spec["b64"]))
        else:
            p.write_text(spec.get("text", ""), encoding="utf-8")
        p.chmod(spec.get("mode", 0o600))


def op_pins(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice import engines, models, pins, resident

    return {
        "faster_whisper_model_names": sorted(pins.FASTER_WHISPER_MODEL_NAMES),
        "faster_whisper_test_model": pins.FASTER_WHISPER_TEST_MODEL,
        "fixture_transcripts": pins.FIXTURE_TRANSCRIPTS,
        "fixture_observed_wer": pins.FIXTURE_OBSERVED_WER,
        "fixture_max_wer": pins.FIXTURE_MAX_WER,
        "fixture_mean_max_wer": pins.FIXTURE_MEAN_MAX_WER,
        "whisper_cpp_binary": pins.WHISPER_CPP_BINARY,
        "whisper_cpp_auto_language": pins.WHISPER_CPP_AUTO_LANGUAGE,
        "whisper_cpp_timeout_s": pins.WHISPER_CPP_TIMEOUT_S,
        "faster_whisper_revisions": pins.FASTER_WHISPER_REVISIONS,
        "faster_whisper_sizes": pins.FASTER_WHISPER_SIZES,
        "moonshine_binary": pins.MOONSHINE_BINARY,
        "moonshine_timeout_s": pins.MOONSHINE_TIMEOUT_S,
        "moonshine_default_model": pins.MOONSHINE_DEFAULT_MODEL,
        "moonshine_dir_arch": pins.MOONSHINE_DIR_ARCH,
        "moonshine_base_url": pins.MOONSHINE_BASE_URL,
        "moonshine_revision": pins.MOONSHINE_REVISION,
        "moonshine_models": {
            name: [[f.name, f.sha256, f.size] for f in files]
            for name, files in pins.MOONSHINE_MODELS.items()
        },
        "moonshine_size_mb": {
            name: models.moonshine_size_mb(name) for name in pins.MOONSHINE_MODELS
        },
        "parakeet_binary": pins.PARAKEET_BINARY,
        "parakeet_quantize_binary": pins.PARAKEET_QUANTIZE_BINARY,
        "parakeet_default_model": pins.PARAKEET_DEFAULT_MODEL,
        "parakeet_base_url": pins.PARAKEET_BASE_URL,
        "parakeet_quant": pins.PARAKEET_QUANT,
        "parakeet_models": {
            name: {
                "dir": m.dir,
                "source": list(m.source),
                "quantized": list(m.quantized),
            }
            for name, m in pins.PARAKEET_MODELS.items()
        },
        "valid_engine_names": engines.VALID_ENGINE_NAMES,
        "hardware": {
            "parakeet_min_ram_gb": engines.PARAKEET_MIN_RAM_GB,
            "parakeet_min_cpus": engines.PARAKEET_MIN_CPUS,
            "moonshine_min_ram_gb": engines.MOONSHINE_MIN_RAM_GB,
        },
        "resident": {
            "protocol": resident.PROTOCOL,
            "max_frame_bytes": resident.MAX_FRAME_BYTES,
            "load_timeout_s": resident.LOAD_TIMEOUT_S,
            "clip_timeout_s": resident.CLIP_TIMEOUT_S,
            "stop_grace_s": resident.STOP_GRACE_S,
        },
        "cleanup_prompt": _cleanup_prompt(),
    }


def _cleanup_prompt() -> str:
    from opendaisugi.voice.cleanup import CLEANUP_PROMPT_V1

    return CLEANUP_PROMPT_V1


def op_prereq(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.prereq import check_voice_prereqs

    r = check_voice_prereqs()
    return {
        "faster_whisper_available": r.faster_whisper_available,
        "ffmpeg_available": r.ffmpeg_available,
        "espeak_available": r.espeak_available,
        "whisper_cpp_available": r.whisper_cpp_available,
        "moonshine_available": r.moonshine_available,
        "parakeet_available": r.parakeet_available,
        "ok": r.ok,
    }


def op_engine_args(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.engines import whisper_cpp_args

    return {
        "args": whisper_cpp_args(q["binary"], Path(q["model"]), q["wav_path"], q.get("language"))
    }


def op_engine_text(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.engines import whisper_cpp_text

    return {"text": whisper_cpp_text(q["stdout"])}


def op_pick_engine(q: dict[str, Any]) -> dict[str, Any]:
    """pick_engine on a config with only the keys the query names set,
    without transcribing. faster_whisper is hidden, so its engine never
    loads. ``moonshine_base_url`` and ``xdg_cache_home`` set those variables
    first. The lines the engine says go in ``said``."""
    import contextlib
    import io

    from opendaisugi.config import Config
    from opendaisugi.voice import pins
    from opendaisugi.voice.engines import pick_engine

    if "moonshine_base_url" in q:
        os.environ[pins.MOONSHINE_BASE_URL_ENV] = q["moonshine_base_url"]
    if "xdg_cache_home" in q:
        os.environ["XDG_CACHE_HOME"] = q["xdg_cache_home"]
    if "parakeet_base_url" in q:
        os.environ[pins.PARAKEET_BASE_URL_ENV] = q["parakeet_base_url"]
    if "hardware" in q:
        from opendaisugi.voice.engines import HARDWARE_ENV

        os.environ[HARDWARE_ENV] = q["hardware"]
    fields = {k: q[k] for k in ("voice_engine", "voice_model") if k in q}
    said = io.StringIO()
    try:
        with contextlib.redirect_stderr(said):
            engine = pick_engine(Config(**fields))
    except Exception as exc:  # noqa: BLE001 - every raise is the answer
        answer = _err(exc)
    else:
        answer = {"engine": engine.name}
        if engine.name == "moonshine":
            answer["model"] = str(engine.model)
            answer["arch"] = engine.arch
        if engine.name == "parakeet":
            answer["model"] = str(engine.model)
        if engine.name in ("moonshine", "parakeet"):
            answer["argv"] = engine.argv
    if said.getvalue():
        answer["said"] = said.getvalue().splitlines()
    return answer


def op_moonshine_args(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.engines import moonshine_args

    return {"args": moonshine_args(q["binary"], Path(q["model"]), q["arch"])}


def op_parakeet_args(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.engines import parakeet_args

    return {"args": parakeet_args(q["binary"], Path(q["model"]))}


def op_resident(q: dict[str, Any]) -> dict[str, Any]:
    """A resident engine (moonshine or parakeet) on a model the query names,
    with whatever program PATH holds: built, started, then each step (a
    ``clip`` to transcribe, or a ``wait`` of up to N seconds while a child
    loads, answered with the state then), then stopped."""
    from opendaisugi.voice import engines

    cls = engines.MoonshineEngine if q["engine"] == "moonshine" else engines.ParakeetEngine
    kw = {k: q[k] for k in ("load_timeout_s", "clip_timeout_s") if k in q}
    try:
        engine = cls(q["model"], **kw)
        engine.start()
    except Exception as exc:  # noqa: BLE001
        return {"start": _err(exc)}
    steps: list[dict[str, Any]] = []
    try:
        for step in q["steps"]:
            if "wait" in step:
                steps.append({"state": engine.wait_ready(step["wait"])})
                continue
            try:
                t = engine.transcribe(_unb64(step["clip"]), language=step.get("language"))
            except Exception as exc:  # noqa: BLE001
                steps.append(_err(exc))
            else:
                steps.append({"text": t.text, "duration_s": t.duration_s})
    finally:
        engine.stop()
    return {"start": "ok", "steps": steps}


def op_choose_engine(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.engines import VoiceHardware, choose_engine, hardware_order

    hw = VoiceHardware(q["ram_gb"], q["cpus"], q["vram_gb"])
    engine, model, line = choose_engine(hw, set(q["installed"]), q["faster_whisper"])
    return {"order": hardware_order(hw), "engine": engine, "model": model, "line": line}


def op_resident_line(q: dict[str, Any]) -> dict[str, Any]:
    """How the server reads one line from the child: ready, a reply kind, or neither."""
    from opendaisugi.voice import resident

    obj = resident.parse_line(_unb64(q["line_b64"]))
    return {"ready": resident.is_ready(obj), "kind": resident.reply_kind(obj)}


def op_fetch_file(q: dict[str, Any]) -> dict[str, Any]:
    """_model_fetch.fetch with the caller's digest: ``ok``, or the kind of
    failure (``verify`` for a digest that does not match, ``download`` for
    any other), and the files then in the destination's directory with
    their digests (a partial download, kept for a resume, is left out)."""
    from opendaisugi._model_fetch import FetchVerificationError, fetch, sha256_file

    dest = Path(q["dest"])
    try:
        fetch(q["url"], q["sha256"], dest, size=q.get("size"), notice=False)
        answer: dict[str, Any] = {"ok": True}
    except FetchVerificationError:
        answer = {"error": "verify"}
    except OSError:
        answer = {"error": "download"}
    files = {}
    if dest.parent.is_dir():
        for p in sorted(dest.parent.iterdir()):
            if ".part" not in p.name:
                files[p.name] = sha256_file(p)
    answer["files"] = files
    return answer


def op_transcribe(q: dict[str, Any]) -> dict[str, Any]:
    """The whisper.cpp engine on one clip, with whatever whisper-cli PATH holds."""
    from opendaisugi.voice.engines import WhisperCppEngine

    try:
        engine = WhisperCppEngine(Path(q["model"]))
        t = engine.transcribe(_unb64(q["wav_b64"]), language=q.get("language"))
    except Exception as exc:  # noqa: BLE001
        return _err(exc)
    return {"text": t.text, "segments": t.segments, "duration_s": t.duration_s}


def op_wav_duration(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.audio import wav_duration_s

    try:
        return {"seconds": wav_duration_s(_unb64(q["wav_b64"]))}
    except Exception:  # noqa: BLE001
        return {"error": "unreadable"}


def op_to_wav(q: dict[str, Any]) -> dict[str, Any]:
    """to_wav_16k_mono. A failure the server answers 400 bad_audio is
    ``bad_audio``; any other is ``internal``, which the server answers 500."""
    import wave

    from opendaisugi.voice.audio import AudioFormatUnsupported, to_wav_16k_mono

    try:
        out = to_wav_16k_mono(_unb64(q["raw_b64"]), q.get("content_type", ""))
    except (AudioFormatUnsupported, ValueError, wave.Error, RuntimeError):
        return {"error": "bad_audio"}
    except Exception:  # noqa: BLE001
        return {"error": "internal"}
    return {"wav_b64": _b64(out)}


def op_multipart(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.server import _extract_multipart_audio

    try:
        payload, part_type = _extract_multipart_audio(_unb64(q["body_b64"]), q["content_type"])
    except Exception as exc:  # noqa: BLE001
        return _err(exc)
    return {"payload_b64": _b64(payload), "part_type": part_type}


def op_content_length(q: dict[str, Any]) -> dict[str, Any]:
    from email.message import Message

    from opendaisugi.voice.server import _content_length

    headers = Message()
    if q.get("value") is not None:
        headers["Content-Length"] = q["value"]
    return {"length": _content_length(headers)}


def op_is_loopback(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.server import _is_loopback

    return {"loopback": _is_loopback(q["address"])}


def op_armed_name(q: dict[str, Any]) -> dict[str, Any]:
    return {"name": urllib.parse.quote(q["pane_key"], safe="") + ".json"}


def _armed_dir(q: dict[str, Any]) -> Path:
    root = Path(os.environ["VOICE_PROBE_DIR"])
    return root / "armed"


def op_arm(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.deliver import arm

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    try:
        e = arm(q["pane_key"], minutes=q["minutes"], armed_dir=_armed_dir(q), now=q["now"])
        res: dict[str, Any] = {
            "entry": {"pane_key": e.pane_key, "armed_at": e.armed_at, "expires_at": e.expires_at}
        }
    except Exception as exc:  # noqa: BLE001
        res = {"error": type(exc).__name__}
    res["tree"] = _tree(root)
    return res


def op_disarm(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.deliver import disarm

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    try:
        res: dict[str, Any] = {"removed": disarm(q["pane_key"], armed_dir=_armed_dir(q))}
    except Exception as exc:  # noqa: BLE001
        res = {"error": type(exc).__name__}
    res["tree"] = _tree(root)
    return res


def op_is_armed(q: dict[str, Any]) -> dict[str, Any]:
    from opendaisugi.voice.deliver import is_armed

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    return {"armed": is_armed(q["pane_key"], armed_dir=_armed_dir(q), now=q["now"])}


def op_deliver(q: dict[str, Any]) -> dict[str, Any]:
    """deliver() with a counting factory and a recording send."""
    from opendaisugi.voice.deliver import deliver

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    calls: list[str] = []

    class Pane:
        id = q["pane"]
        backend = "fake"

    def factory() -> object:
        calls.append("factory")
        return object()

    def send(backend: object, pane: object, text: str) -> str:
        calls.append(f"send {getattr(pane, 'id', '?')} {text}")
        return "typed"

    r = deliver(
        Pane(),
        q["text"],
        factory,
        mode=q["mode"],
        armed_dir=_armed_dir(q),
        pane_key=q.get("pane_key"),
        now=q["now"],
        send=send,
    )
    return {"delivered": r.delivered, "reason": r.reason, "calls": calls}


def op_cleanup(q: dict[str, Any]) -> dict[str, Any]:
    """clean_transcript with a scripted transport, journaled to the probe dir."""
    from opendaisugi.config import Config
    from opendaisugi.voice.cleanup import clean_transcript

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    calls: list[dict[str, str]] = []

    class Transport:
        def complete(self, *, model: str, system: str, user: str):
            calls.append({"model": model, "system": system, "user": user})
            t = q["transport"]
            if t.get("raise"):
                raise RuntimeError("the transport failed")
            return t["text"], t.get("usage", {"input_tokens": 0, "output_tokens": 0})

    config = Config(data_dir=root / "data", **q.get("config", {}))
    r = clean_transcript(q["text"], config=config, transport=Transport())
    journal = root / "data" / "gateway" / "turns.jsonl"
    lines = []
    if journal.is_file():
        for ln in journal.read_text(encoding="utf-8").splitlines():
            rec = json.loads(ln)
            rec["created_at"] = "<created_at>" if rec.get("created_at") else rec.get("created_at")
            lines.append(rec)
    return {
        "text": r.text,
        "cleaned": r.cleaned,
        "reason": r.reason,
        "calls": calls,
        "journal": lines,
    }


def op_ptt(q: dict[str, Any]) -> dict[str, Any]:
    """run_ptt with scripted keys, streams and a scripted client."""
    from opendaisugi.voice.ptt import VoiceServerError, run_ptt

    events: list[Any] = []
    printed: list[str] = []
    streams = [list(s) for s in q.get("streams", [])]
    transcribes = list(q.get("transcribe", []))
    delivers = list(q.get("deliver", []))

    class Stream:
        def __init__(self, chunks: list[list[Any]]) -> None:
            self.chunks = chunks

        def start(self) -> None:
            events.append("start")

        def stop(self) -> None:
            events.append("stop")

        def read(self, frames: int) -> tuple[bytes, bool]:
            events.append(f"read {frames}")
            if not self.chunks:
                return b"", False
            chunk, more = self.chunks.pop(0)
            return _unb64(chunk), bool(more)

    def open_stream() -> Stream:
        events.append("open")
        return Stream(streams.pop(0) if streams else [])

    class Client:
        def transcribe(self, wav: bytes) -> dict:
            events.append({"transcribe": _b64(wav)})
            a = transcribes.pop(0)
            if "error" in a:
                raise VoiceServerError(a["error"])
            return a["reply"]

        def deliver(self, pane: str, text: str, *, mode: str) -> dict:
            events.append({"deliver": [pane, text, mode]})
            a = delivers.pop(0)
            if "error" in a:
                raise VoiceServerError(a["error"])
            return a["reply"]

    results = run_ptt(
        q["pane"],
        client=Client(),
        keys=q["keys"],
        open_stream=open_stream,
        chunk_frames=q.get("chunk_frames", 1600),
        print_fn=printed.append,
    )
    return {"results": [list(r) for r in results], "printed": printed, "events": events}


def op_client(q: dict[str, Any]) -> dict[str, Any]:
    """HttpVoiceClient against a server the case harness runs, or none."""
    from opendaisugi.config import Config
    from opendaisugi.voice.ptt import HttpVoiceClient, VoiceServerError

    root = Path(os.environ["VOICE_PROBE_DIR"])
    _lay(root, q.get("files", {}))
    config = Config(data_dir=root / "data")
    try:
        client = HttpVoiceClient(q["url"], config=config, timeout_s=5.0)
        if q["call"] == "transcribe":
            reply = client.transcribe(_unb64(q["wav_b64"]))
        else:
            reply = client.deliver(q["pane"], q["text"], mode=q["mode"])
    except VoiceServerError as exc:
        return _err(exc)
    return {"reply": reply}


OPS = {
    "pins": op_pins,
    "prereq": op_prereq,
    "engine_args": op_engine_args,
    "engine_text": op_engine_text,
    "pick_engine": op_pick_engine,
    "transcribe": op_transcribe,
    "moonshine_args": op_moonshine_args,
    "parakeet_args": op_parakeet_args,
    "resident": op_resident,
    "choose_engine": op_choose_engine,
    "resident_line": op_resident_line,
    "fetch_file": op_fetch_file,
    "wav_duration": op_wav_duration,
    "to_wav": op_to_wav,
    "multipart": op_multipart,
    "content_length": op_content_length,
    "is_loopback": op_is_loopback,
    "armed_name": op_armed_name,
    "arm": op_arm,
    "disarm": op_disarm,
    "is_armed": op_is_armed,
    "deliver": op_deliver,
    "cleanup": op_cleanup,
    "ptt": op_ptt,
    "client": op_client,
}


def main() -> int:
    q = json.loads(sys.stdin.buffer.read())
    answer = OPS[q["op"]](q)
    sys.stdout.write(json.dumps(answer, sort_keys=True) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

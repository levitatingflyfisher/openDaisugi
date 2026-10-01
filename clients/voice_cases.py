"""Voice bridge cases (stage H): the oracle's answers, for the Go and Rust ports.

    uv run --no-sync python clients/voice_cases.py [--out clients/fixtures/voice] [--only TEXT]

Three kinds of case:

- ``probe``: one query of a pure part of src/opendaisugi/voice (pins,
  prereq, the whisper.cpp command line and its output rule, WAV reading and
  resampling, multipart, the arm grants, deliver's decision, cleanup, the
  push-to-talk state machine, the HTTP client). The oracle answers with
  clients/voice_probe_oracle.py; a port answers with its voice-probe binary.
- ``cli``: one ``daisugi voice ...`` command in a scratch HOME: exit code,
  stdout, stderr and the tree after.
- ``server``: ``daisugi voice serve`` started in a scratch world, then a list
  of raw HTTP requests. The speech engine is a FAKE whisper-cli (or
  moonshine-cli) script that records its argv and the clip it was given and
  prints a fixed transcript.
  The pane backend is a FAKE coppice socket in the scratch directory that
  records every line it is sent. The cleanup model, when a case uses one, is
  a fake OpenAI-compatible upstream on loopback. No case uses a real model, a
  microphone or a real coppice server.

Every run has HOME, TMPDIR and XDG_RUNTIME_DIR in the case's scratch
directory and PATH set to the case's own fake-tool directory only, so ffmpeg,
whisper-cli, moonshine-cli, parakeet-cli, tmux and herdr exist exactly when a
case says they do. The Moonshine and Parakeet download hosts are a closed
loopback port unless a case serves one, the hardware the engine choice reads
is a fixed desktop (16 GB, 8 cores, no GPU) unless a case names another, and
the oracle runs with faster_whisper hidden, as the binaries are built: no case
loads or fetches a real model or reads the real box.

Paths are normalized to ``{W}`` (the case's scratch directory), ports to
``{PORT}``, times to placeholders. The file is content-addressed as the
other suites are: each case's ``id`` is the first 16 hex digits of the
SHA-256 of its canonical JSON without ``id`` and ``expect``... (see body_id),
lines are sorted by id, and a manifest pins the bytes.

A case may carry ``port_expect``: fields where a port's answer differs from
the oracle's by a ruling in clients/ADJUDICATIONS.md (VO-n). The compare puts
those fields over the oracle's answer before it compares.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import io
import json
import os
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import threading
import time
import wave
from pathlib import Path
from typing import Any

from fake_resident import write_fake

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "voice"
SCRATCH = Path(
    os.environ.get("DAISUGI_VOICE_SCRATCH")
    or Path.home() / "opendaisugi-scratch" / "voice" / "runs"
)
CASE_VERSION = 1
PROBE_ORACLE = Path(__file__).resolve().parent / "voice_probe_oracle.py"
TOKEN = "tok-voice-0123456789abcdef"
# Port 1 on loopback: nothing listens there, so a fetch fails at once.
CLOSED_MOONSHINE = "http://127.0.0.1:1/model"
CLOSED_PARAKEET = "http://127.0.0.1:1/parakeet"
# The hardware every case sees unless it names other hardware (RAM_GB,CPUS,VRAM_GB).
CASE_HARDWARE = "16,8,0"
RESIDENT = ("moonshine-cli", "parakeet-cli")
# VO-1: the ports' answer where a config names faster-whisper.
PORT_FASTER_WHISPER = (
    "faster-whisper needs the Python build of daisugi. "
    "Set voice_engine: moonshine to use Moonshine instead."
)


def canonical_json(obj: Any) -> str:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True)


def body_id(case: dict[str, Any]) -> str:
    body = {k: v for k, v in case.items() if k not in ("id", "expect")}
    return hashlib.sha256(canonical_json(body).encode("utf-8")).hexdigest()[:16]


def b64(b: bytes) -> str:
    return base64.b64encode(b).decode()


# ---------------------------------------------------------------------------
# Clips
# ---------------------------------------------------------------------------


def tone(n: int, *, period: int = 37, amp: int = 9000, phase: int = 0) -> list[int]:
    """A deterministic integer waveform: a triangle wave, no floating point."""
    out = []
    for i in range(n):
        k = (i + phase) % period
        half = period // 2
        v = k if k <= half else period - k
        out.append((v * 2 * amp) // max(1, half) - amp)
    return out


def wav(
    *,
    sr: int,
    n: int,
    channels: int = 1,
    width: int = 2,
    samples: list[int] | None = None,
) -> bytes:
    """A PCM WAV written by the wave module."""
    s = samples if samples is not None else tone(n * channels)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(width)
        w.setframerate(sr)
        if width == 2:
            w.writeframes(struct.pack(f"<{len(s)}h", *s))
        elif width == 1:
            w.writeframes(bytes((x >> 8) + 128 & 0xFF for x in s))
        else:
            w.writeframes(b"".join((x << 8).to_bytes(3, "little", signed=True) for x in s))
    return buf.getvalue()


def riff(chunks: list[tuple[bytes, bytes]], *, riff_size: int | None = None) -> bytes:
    """A RIFF/WAVE file from raw chunks, each padded to an even length."""
    body = b"WAVE"
    for cid, data in chunks:
        body += cid + struct.pack("<I", len(data)) + data
        if len(data) % 2:
            body += b"\x00"
    size = len(body) if riff_size is None else riff_size
    return b"RIFF" + struct.pack("<I", size) + body


def fmt_chunk(
    *, fmt: int = 1, channels: int = 1, sr: int = 16000, width: int = 2, extra: bytes = b""
) -> bytes:
    block = channels * width
    return struct.pack("<HHIIHH", fmt, channels, sr, sr * block, block, width * 8) + extra


def pcm(samples: list[int]) -> bytes:
    return struct.pack(f"<{len(samples)}h", *samples)


def streamed(clip: bytes) -> bytes:
    """A WAV as ffmpeg writes one to a pipe: placeholder RIFF and data sizes."""
    out = bytearray(clip)
    out[4:8] = b"\xff\xff\xff\xff"
    idx = out.find(b"data")
    out[idx + 4 : idx + 8] = b"\xff\xff\xff\xff"
    return bytes(out)


def multipart(
    parts: list[tuple[str, bytes]], boundary: str = "XyZ", *, trailing: bool = True
) -> bytes:
    out = b""
    for headers, payload in parts:
        out += f"--{boundary}\r\n{headers}\r\n\r\n".encode() + payload + b"\r\n"
    if trailing:
        out += f"--{boundary}--\r\n".encode()
    return out


# ---------------------------------------------------------------------------
# The world one case runs in
# ---------------------------------------------------------------------------


def _script(path: Path, body: str) -> None:
    path.write_text("#!/bin/sh\n" + body, encoding="utf-8")
    path.chmod(0o755)


def lay_bin(work: Path, spec: dict[str, Any]) -> None:
    """The case's fake tools. Every one is a /bin/sh script that names its
    helpers by absolute path, since PATH holds only this directory."""
    bin_dir = work / "bin"
    bin_dir.mkdir(parents=True, exist_ok=True)
    log = work / "log"
    log.mkdir(exist_ok=True)
    for name, tool in spec.items():
        if name in RESIDENT:
            spec_file = log / f"{name}.spec.json"
            spec_file.write_text(json.dumps(tool), encoding="utf-8")
            write_fake(bin_dir / name, spec_file, log / "resident.jsonl")
        elif name == "parakeet-quantize":
            _script(
                bin_dir / name,
                f"""{{ echo "--call"; for a in "$@"; do printf '%s\\n' "$a"; done; }} >> {log}/quantize.log
exit {int(tool.get("exit", 0))}
""",
            )
        elif name == "whisper-cli":
            (log / "engine.out").write_text(tool.get("stdout", ""), encoding="utf-8")
            (log / "engine.err").write_text(tool.get("stderr", ""), encoding="utf-8")
            _script(
                bin_dir / name,
                f"""LOG={log}
prev=""; f=""
{{ echo "--call"; for a in "$@"; do printf '%s\\n' "$a"; [ "$prev" = "-f" ] && f="$a"; prev="$a"; done; }} >> "$LOG/engine.log"
if [ -n "$f" ] && [ -f "$f" ]; then /usr/bin/sha256sum "$f" | /usr/bin/cut -d' ' -f1 >> "$LOG/engine.sha"; else echo none >> "$LOG/engine.sha"; fi
/bin/cat "$LOG/engine.out"
/bin/cat "$LOG/engine.err" >&2
exit {int(tool.get("exit", 0))}
""",
            )
        elif name == "ffmpeg":
            (log / "ffmpeg.out").write_bytes(base64.b64decode(tool.get("out_b64", "")))
            (log / "ffmpeg.err").write_text(tool.get("stderr", ""), encoding="utf-8")
            _script(
                bin_dir / name,
                f"""LOG={log}
{{ echo "--call"; for a in "$@"; do printf '%s\\n' "$a"; done; }} >> "$LOG/ffmpeg.log"
/usr/bin/sha256sum | /usr/bin/cut -d' ' -f1 >> "$LOG/ffmpeg.sha"
/bin/cat "$LOG/ffmpeg.out"
/bin/cat "$LOG/ffmpeg.err" >&2
exit {int(tool.get("exit", 0))}
""",
            )
        else:
            _script(bin_dir / name, "exit 0\n")


def world_env(work: Path) -> dict[str, str]:
    return {
        "HOME": str(work / "home"),
        "PATH": str(work / "bin"),
        "TMPDIR": str(work / "tmp"),
        "XDG_RUNTIME_DIR": str(work / "run"),
        "VOICE_PROBE_DIR": str(work / "probe"),
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "TZ": "UTC",
        "NO_COLOR": "1",
        "TERM": "dumb",
        "COLUMNS": "80",
        "COPPICE_NO_AUTOSTART": "1",
        "HF_HUB_OFFLINE": "1",
        "OPENDAISUGI_MOONSHINE_BASE_URL": CLOSED_MOONSHINE,
        "OPENDAISUGI_PARAKEET_BASE_URL": CLOSED_PARAKEET,
        "OPENDAISUGI_VOICE_HARDWARE": CASE_HARDWARE,
        "TRANSFORMERS_OFFLINE": "1",
        "PYTHONDONTWRITEBYTECODE": "1",
        "PYTHONPATH": str(REPO / "src"),
    }


def fresh_world(work: Path, spec: dict[str, Any]) -> None:
    shutil.rmtree(work, ignore_errors=True)
    for d in ("home", "tmp", "run", "probe", "log"):
        (work / d).mkdir(parents=True)
    (work / "run").chmod(0o700)
    lay_bin(work, spec.get("bin", {}))


def norm_text(text: str, work: Path) -> str:
    text = text.replace(str(work / "home"), "{HOME}")
    text = text.replace(str(work), "{W}")
    text = re.sub(r"daisugi-voice-[^/\s\"']*\.wav", "daisugi-voice-{R}.wav", text)
    return text


def norm(obj: Any, work: Path) -> Any:
    if isinstance(obj, str):
        return norm_text(obj, work)
    if isinstance(obj, list):
        return [norm(x, work) for x in obj]
    if isinstance(obj, dict):
        return {norm_text(k, work): norm(v, work) for k, v in obj.items()}
    return obj


def _calls(path: Path) -> list[list[str]]:
    if not path.exists():
        return []
    calls: list[list[str]] = []
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if line == "--call":
            calls.append([])
        elif calls:
            calls[-1].append(line)
    return calls


def _alive(pids: list[int]) -> int:
    """How many of the fake engine's runs are still alive, after a short wait."""
    deadline = time.monotonic() + 3.0
    while True:
        alive = 0
        for pid in pids:
            try:
                with open(f"/proc/{pid}/stat", encoding="ascii") as f:
                    state = f.read().rsplit(")", 1)[1].split()[0]
            except OSError:
                continue
            if state != "Z":
                alive += 1
        if alive == 0 or time.monotonic() > deadline:
            return alive
        time.sleep(0.05)


def tool_logs(work: Path) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for tool in ("engine", "ffmpeg"):
        calls = _calls(work / "log" / f"{tool}.log")
        if not calls:
            continue
        shas = (work / "log" / f"{tool}.sha").read_text().split()
        out[tool] = [
            {"argv": norm(c, work), "input_sha256": s} for c, s in zip(calls, shas, strict=False)
        ]
    res = work / "log" / "resident.jsonl"
    if res.exists():
        entries = [json.loads(ln) for ln in res.read_text(encoding="utf-8").splitlines()]
        pids = [e.pop("pid") for e in entries if "pid" in e]
        out["resident"] = norm(entries, work)
        out["children_left"] = _alive(pids)
    q = _calls(work / "log" / "quantize.log")
    if q:
        out["quantize"] = norm(q, work)
    left = sorted(p.name for p in (work / "tmp").iterdir()) if (work / "tmp").exists() else []
    if left:
        out["tmp_left"] = norm(left, work)
    return out


# ---------------------------------------------------------------------------
# Fakes the harness runs: an HTTP answerer, a coppice socket, an upstream
# ---------------------------------------------------------------------------


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def read_http(conn: socket.socket) -> tuple[str, dict[str, str], bytes] | None:
    """One request off conn: its request line, its headers (lower-cased
    names) and its body, read by Content-Length."""
    buf = b""
    while b"\r\n\r\n" not in buf:
        chunk = conn.recv(65536)
        if not chunk:
            return None
        buf += chunk
    head, rest = buf.split(b"\r\n\r\n", 1)
    lines = head.decode("latin-1").split("\r\n")
    headers: dict[str, str] = {}
    for ln in lines[1:]:
        k, _, v = ln.partition(":")
        headers[k.strip().lower()] = v.strip()
    n = int(headers.get("content-length", "0") or 0)
    while len(rest) < n:
        chunk = conn.recv(65536)
        if not chunk:
            break
        rest += chunk
    return lines[0], headers, rest[:n]


class Answerer:
    """A loopback HTTP server that answers each request with the next canned
    reply and records what it was sent."""

    def __init__(self, replies: list[dict[str, Any]]) -> None:
        self.replies = list(replies)
        self.seen: list[dict[str, Any]] = []
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(8)
        self.sock.settimeout(0.05)
        self.port = self.sock.getsockname()[1]
        self._stop = False
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except (TimeoutError, OSError):
                continue
            with conn:
                conn.settimeout(5.0)
                try:
                    req = read_http(conn)
                except OSError:
                    continue
                if req is None:
                    continue
                line, headers, body = req
                rec: dict[str, Any] = {
                    "line": line,
                    "content_type": headers.get("content-type"),
                    "authorization": headers.get("authorization"),
                }
                if "range" in headers:
                    rec["range"] = headers["range"]
                try:
                    rec["json"] = json.loads(body)
                except ValueError:
                    rec["body_sha256"] = hashlib.sha256(body).hexdigest()
                self.seen.append(rec)
                r = self.replies.pop(0) if self.replies else {"status": 500, "body": "{}"}
                if "body_b64" in r:
                    payload = base64.b64decode(r["body_b64"])
                else:
                    payload = r.get("body", "").encode()
                # declared_length > the payload is a transfer cut short.
                length = r.get("declared_length", len(payload))
                head = (
                    f"HTTP/1.1 {r['status']} X\r\nContent-Type: "
                    f"{r.get('content_type', 'application/json')}\r\n"
                    f"Content-Length: {length}\r\nConnection: close\r\n\r\n"
                )
                try:
                    conn.sendall(head.encode() + payload)
                except OSError:
                    pass

    def close(self) -> None:
        self._stop = True
        self.thread.join(timeout=2)
        self.sock.close()


class FakeCoppice:
    """A coppice socket in the case's scratch directory. It answers
    server.status, pane.list (with the case's panes), pane.send_text and
    agent.prompt, and records every request line as JSON.

    ``fail`` names a command that answers ``ok: false``."""

    def __init__(self, path: Path, panes: list[dict[str, Any]], fail: str | None = None) -> None:
        self.path = path
        self.panes = panes
        self.fail = fail
        self.lines: list[Any] = []
        self._lock = threading.Lock()
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(str(path))
        self.sock.listen(16)
        self.sock.settimeout(0.05)
        self._stop = False
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _answer(self, msg: Any) -> dict[str, Any]:
        mid = msg.get("id") if isinstance(msg, dict) else None
        cmd = msg.get("cmd") if isinstance(msg, dict) else None
        if cmd == self.fail:
            return {"id": mid, "ok": False, "error": {"code": "not_found", "message": "no pane"}}
        if cmd == "server.status":
            return {"id": mid, "ok": True, "result": {"version": "fake"}}
        if cmd == "pane.list":
            return {"id": mid, "ok": True, "result": {"panes": self.panes}}
        return {"id": mid, "ok": True, "result": {}}

    def _conn(self, conn: socket.socket) -> None:
        with conn:
            conn.settimeout(3.0)
            buf = b""
            try:
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    buf += chunk
                    while b"\n" in buf:
                        line, buf = buf.split(b"\n", 1)
                        try:
                            msg = json.loads(line)
                        except ValueError:
                            msg = {"unparsed": line.decode("utf-8", "replace")}
                        with self._lock:
                            self.lines.append(msg)
                        conn.sendall(json.dumps(self._answer(msg)).encode() + b"\n")
            except OSError:
                pass

    def _serve(self) -> None:
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except (TimeoutError, OSError):
                continue
            threading.Thread(target=self._conn, args=(conn,), daemon=True).start()

    def close(self) -> None:
        self._stop = True
        self.thread.join(timeout=2)
        self.sock.close()


# ---------------------------------------------------------------------------
# Probe cases
# ---------------------------------------------------------------------------


def run_probe(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    fresh_world(work, case)
    for rel, text in case.get("files", {}).items():
        (work / rel).parent.mkdir(parents=True, exist_ok=True)
        (work / rel).write_text(text, encoding="utf-8")
    env = world_env(work)
    query = json.loads(json.dumps(case["query"]))
    answerer = None
    closed = None
    if "http" in case:
        answerer = Answerer(case["http"])
    if answerer is not None:
        query = json.loads(json.dumps(query).replace("{HTTP}", f"http://127.0.0.1:{answerer.port}"))
    if "url" in query:
        if "{CLOSED}" in query["url"]:
            closed = free_port()
            query["url"] = query["url"].replace("{CLOSED}", f"http://127.0.0.1:{closed}")
    text = json.dumps(query).replace("{W}", str(work))
    try:
        proc = subprocess.run(
            cmd,
            input=text.encode(),
            capture_output=True,
            env=env,
            cwd=work,
            timeout=60,
        )
    finally:
        if answerer is not None:
            answerer.close()
    out: dict[str, Any] = {"exit": proc.returncode}
    try:
        answer = json.loads(proc.stdout)
    except ValueError:
        answer = None
        out["stdout"] = proc.stdout.decode("utf-8", "replace")[-2000:]
        out["stderr"] = proc.stderr.decode("utf-8", "replace")[-2000:]
    if answer is not None and closed is not None:
        answer = json.loads(json.dumps(answer).replace(str(closed), "{CLOSEDPORT}"))
    out["answer"] = norm(answer, work)
    if answerer is not None:
        seen = json.loads(json.dumps(answerer.seen).replace(str(answerer.port), "{HTTPPORT}"))
        out["http_seen"] = seen
    out.update(tool_logs(work))
    return out


def probe(name: str, query: dict[str, Any], **extra: Any) -> dict[str, Any]:
    return {"kind": "probe", "name": name, "query": query, **extra}


def probe_cases() -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = [probe("pins", {"op": "pins"})]
    # prereq: PATH is exactly the fake-tool directory.
    # The oracle hides faster_whisper (VO-13), as a port cannot import it
    # (VO-1), so only whisper-cli, moonshine-cli or parakeet-cli makes it ok.
    for tools in (
        [],
        ["ffmpeg"],
        ["espeak"],
        ["espeak-ng", "whisper-cli"],
        ["whisper-cli"],
        ["moonshine-cli"],
        ["parakeet-cli"],
        ["ffmpeg", "moonshine-cli", "whisper-cli"],
        ["moonshine-cli", "parakeet-cli", "whisper-cli"],
    ):
        cases.append(
            probe(
                f"prereq {'+'.join(tools) or 'none'}",
                {"op": "prereq"},
                bin={t: {} for t in tools},
            )
        )
    # The whisper.cpp command line and its stdout rule.
    for lang in (None, "en", "de", "", "auto"):
        cases.append(
            probe(
                f"engine args lang={lang!r}",
                {
                    "op": "engine_args",
                    "binary": "/opt/w/whisper-cli",
                    "model": "/m/ggml base.bin",
                    "wav_path": "/t/a.wav",
                    "language": lang,
                },
            )
        )
    for i, out in enumerate(
        [
            "",
            "\n",
            "\n hello world\n",
            " [BLANK_AUDIO]\n",
            "a\nb\n",
            "a\r\nb\r\n",
            "  lead\ttab\t\n\n trail  ",
            "x\x0by\x0cz\x1cw\x1dv\x1eu\x85t\u2028s\u2029r",
            "\u00a0nbsp\u00a0\n\u3000ideo\u3000",
            "caf\u00e9 \u4e2d\u6587\n\U0001f600",
        ]
    ):
        cases.append(probe(f"engine text {i}", {"op": "engine_text", "stdout": out}))
    # pick_engine without transcribing.
    model = "{W}/home/ggml.bin"
    cases += [
        probe(
            "pick whisper.cpp ok",
            {"op": "pick_engine", "voice_engine": "whisper.cpp", "voice_model": model},
            bin={"whisper-cli": {}},
            files={"home/ggml.bin": "x"},
        ),
        probe(
            "pick whisper.cpp no binary",
            {"op": "pick_engine", "voice_engine": "whisper.cpp", "voice_model": model},
        ),
        probe(
            "pick whisper.cpp no model",
            {"op": "pick_engine", "voice_engine": "whisper.cpp", "voice_model": model},
            bin={"whisper-cli": {}},
        ),
        probe(
            "pick whisper.cpp model is a dir",
            {"op": "pick_engine", "voice_engine": "whisper.cpp", "voice_model": "{W}/home"},
            bin={"whisper-cli": {}},
        ),
        probe(
            "pick unknown engine",
            {"op": "pick_engine", "voice_engine": "Whisper.CPP", "voice_model": "x"},
        ),
        probe(
            "pick empty engine",
            {"op": "pick_engine", "voice_engine": "", "voice_model": "x"},
        ),
    ]
    cases += moonshine_cases()
    cases += parakeet_cases()
    cases += resident_cases()
    cases += choose_cases()
    # transcribe through the fake whisper-cli.
    clip = wav(sr=16000, n=8000)
    for name, tool in [
        ("plain", {"stdout": "\n hello there\n"}),
        ("two lines", {"stdout": "\n one\n two\n"}),
        ("empty", {"stdout": ""}),
        ("fails", {"stdout": "", "stderr": "load\nerror: bad model\n", "exit": 3}),
        ("fails no stderr", {"exit": 1}),
        ("fails long stderr", {"stderr": "e" * 400 + "\n", "exit": 2}),
        ("fails blank stderr lines", {"stderr": "first\n\n  \n", "exit": 4}),
        ("unicode", {"stdout": "caf\u00e9 ok\n"}),
    ]:
        t = dict(tool)
        cases.append(
            probe(
                f"transcribe {name}",
                {"op": "transcribe", "model": model, "wav_b64": b64(clip)},
                bin={"whisper-cli": t},
                files={"home/ggml.bin": "x"},
            )
        )
    cases.append(
        probe(
            "transcribe a language",
            {"op": "transcribe", "model": model, "wav_b64": b64(clip), "language": "fr"},
            bin={"whisper-cli": {"stdout": "bonjour"}},
            files={"home/ggml.bin": "x"},
        )
    )
    cases.append(
        probe(
            "transcribe zero frames",
            {"op": "transcribe", "model": model, "wav_b64": b64(wav(sr=16000, n=0))},
            bin={"whisper-cli": {"stdout": "x"}},
            files={"home/ggml.bin": "x"},
        )
    )
    cases += audio_cases()
    cases += multipart_cases()
    for v in [
        None,
        "0",
        "12",
        "-1",
        "-0",
        "+5",
        " 12 ",
        "\t7\t",
        "1_0",
        "1__0",
        "_1",
        "abc",
        "",
        "12a",
        "0x10",
        "1e3",
        "1.0",
        "\uff11\uff12",
        "\u0663",
        "99999999999999999999999",
        "\u00a012",
    ]:
        cases.append(probe(f"content length {v!r}", {"op": "content_length", "value": v}))
    for a in [
        "127.0.0.1",
        "127.255.255.254",
        "127.1",
        "::1",
        "0:0:0:0:0:0:0:1",
        "localhost",
        "LOCALHOST",
        "10.0.0.1",
        "0.0.0.0",
        "::",
        "::ffff:127.0.0.1",
        "",
        "fe80::1%eth0",
        "127.0.0.01",
        " 127.0.0.1",
    ]:
        cases.append(probe(f"loopback {a!r}", {"op": "is_loopback", "address": a}))
    for k in ["w1:p1", "a/b", "%41", "~x", "a b", "", "..", "caf\u00e9", "\U0001f600", "a+b=c&d"]:
        cases.append(probe(f"armed name {k!r}", {"op": "armed_name", "pane_key": k}))
    cases += arm_cases()
    cases += cleanup_cases()
    cases += ptt_cases()
    cases += client_cases()
    return cases


def moonshine_cases() -> list[dict[str, Any]]:
    """The Moonshine engine: its command line, picking it, its pinned
    models (fetched only from a fake host or a closed port) and a fake
    moonshine-cli."""
    moon = {"moonshine-cli": {}}
    mdir = {"moon/tokenizer.bin": "x"}
    fw = {"answer": {"error": "EngineUnavailable", "message": PORT_FASTER_WHISPER}}
    cases = [
        probe(
            f"moonshine args {arch}",
            {
                "op": "moonshine_args",
                "binary": "/opt/m/moonshine-cli",
                "model": "/m/small model",
                "arch": arch,
            },
        )
        for arch in ("tiny", "small", "medium")
    ]

    def pick(name: str, query: dict[str, Any], **extra: Any) -> dict[str, Any]:
        return probe(f"pick {name}", {"op": "pick_engine", **query}, **extra)

    cases += [
        pick(
            "moonshine dir",
            {"voice_engine": "moonshine", "voice_model": "{W}/moon"},
            bin=moon,
            files=mdir,
        ),
        pick(
            "moonshine dir trailing slash",
            {"voice_engine": "moonshine", "voice_model": "{W}/moon/"},
            bin=moon,
            files=mdir,
        ),
        pick(
            "moonshine no binary",
            {"voice_engine": "moonshine", "voice_model": "{W}/moon"},
            files=mdir,
        ),
        pick(
            "moonshine dir missing",
            {"voice_engine": "moonshine", "voice_model": "{W}/none"},
            bin=moon,
        ),
        pick(
            "moonshine dir is a file",
            {"voice_engine": "moonshine", "voice_model": "{W}/moon/tokenizer.bin"},
            bin=moon,
            files=mdir,
        ),
        pick(
            "moonshine relative dir",
            {"voice_engine": "moonshine", "voice_model": "moon"},
            bin=moon,
            files={"moon/x": "x"},
        ),
        pick(
            "moonshine Small is a dir name",
            {"voice_engine": "moonshine", "voice_model": "Small"},
            bin=moon,
        ),
        pick(
            "moonshine small closed host",
            {"voice_engine": "moonshine", "voice_model": "small"},
            bin=moon,
        ),
        pick(
            "moonshine tiny closed host",
            {"voice_engine": "moonshine", "voice_model": "tiny"},
            bin=moon,
        ),
        pick(
            "moonshine medium host 404",
            {
                "voice_engine": "moonshine",
                "voice_model": "medium",
                "moonshine_base_url": "{HTTP}/m",
            },
            bin=moon,
            http=[{"status": 404, "body": "no"}],
        ),
        pick(
            "moonshine medium host cut short",
            {
                "voice_engine": "moonshine",
                "voice_model": "medium",
                "moonshine_base_url": "{HTTP}/m",
            },
            bin=moon,
            http=[
                {
                    "status": 200,
                    "body": "abc",
                    "content_type": "application/octet-stream",
                    "declared_length": 3651296,
                }
            ],
        ),
        pick("moonshine no model set", {"voice_engine": "moonshine"}, bin=moon),
        pick(
            "moonshine no model set xdg cache",
            {"voice_engine": "moonshine", "xdg_cache_home": "{W}/xdg"},
            bin=moon,
        ),
        pick(
            "moonshine no model set relative xdg cache",
            {"voice_engine": "moonshine", "xdg_cache_home": "xdg"},
            bin=moon,
        ),
        pick("no config", {}, bin=moon),
        pick("no config no binary", {}),
        pick("model only", {"voice_model": "base.en"}, bin=moon, port_expect=fw),
        # The default values, as save_config writes them, are no choice.
        pick("faster-whisper named", {"voice_engine": "faster-whisper"}, bin=moon),
        pick(
            "faster-whisper named with model",
            {"voice_engine": "faster-whisper", "voice_model": "tiny.en"},
        ),
        pick(
            "faster-whisper base.en",
            {"voice_engine": "faster-whisper", "voice_model": "base.en"},
            bin=moon,
            port_expect=fw,
        ),
        pick(
            "moonshine default model value",
            {"voice_engine": "moonshine", "voice_model": "tiny.en"},
            bin=moon,
        ),
    ]
    # The generic pinned download, with a caller's digest.
    data = b"moonshine model bytes\n"
    good = hashlib.sha256(data).hexdigest()
    octet = "application/octet-stream"

    def fetch(name: str, http: list[dict[str, Any]] | None, **extra: Any) -> dict[str, Any]:
        q = {
            "op": "fetch_file",
            "url": "{HTTP}/m/f.bin" if http is not None else "{CLOSED}/m/f.bin",
            "sha256": extra.pop("sha256", good),
            "dest": "{W}/cache/f.bin",
            "size": extra.pop("size", len(data)),
        }
        kw: dict[str, Any] = {"http": http} if http is not None else {}
        return probe(f"fetch {name}", q, **kw, **extra)

    cases += [
        fetch("ok", [{"status": 200, "body_b64": b64(data), "content_type": octet}]),
        fetch(
            "ok no size",
            [{"status": 200, "body_b64": b64(data), "content_type": octet}],
            size=None,
        ),
        fetch(
            "cached",
            [],
            files={"cache/f.bin": data.decode()},
        ),
        fetch(
            "stale file replaced",
            [{"status": 200, "body_b64": b64(data), "content_type": octet}],
            files={"cache/f.bin": "old bytes"},
        ),
        fetch(
            "digest mismatch",
            [{"status": 200, "body_b64": b64(data), "content_type": octet}],
            sha256="0" * 64,
        ),
        fetch(
            "cut short",
            [{"status": 200, "body": "moon", "content_type": octet, "declared_length": len(data)}],
        ),
        fetch("404", [{"status": 404, "body": "missing"}]),
        fetch("500", [{"status": 500, "body": "{}"}]),
        fetch("closed", None),
    ]
    # No voice settings, other hardware: the line says what was chosen and why.
    for hw, tools in [
        ("16,8,0", {}),
        ("16,8,0", {"parakeet-cli": {}}),
        ("15.6,4,5.9", moon),
        ("3.8,2,0", moon),
        ("1,1,0", moon),
        (",4,0", moon),
    ]:
        cases.append(
            pick(
                f"no config hardware {hw} {'+'.join(tools) or 'none'}", {"hardware": hw}, bin=tools
            )
        )
    return cases


def parakeet_cases() -> list[dict[str, Any]]:
    """The Parakeet engine: its command line and picking it. Its model is
    fetched only from a closed port or a fake host that answers 404."""
    para = {"parakeet-cli": {}}
    cases = [
        probe(
            "parakeet args",
            {"op": "parakeet_args", "binary": "/opt/p/parakeet-cli", "model": "/m/p v2.gguf"},
        )
    ]

    def pick(name: str, query: dict[str, Any], **extra: Any) -> dict[str, Any]:
        return probe(f"pick {name}", {"op": "pick_engine", **query}, **extra)

    cases += [
        pick(
            "parakeet file",
            {"voice_engine": "parakeet", "voice_model": "{W}/p.gguf"},
            bin=para,
            files={"p.gguf": "x"},
        ),
        pick(
            "parakeet no binary",
            {"voice_engine": "parakeet", "voice_model": "{W}/p.gguf"},
            files={"p.gguf": "x"},
        ),
        pick(
            "parakeet file missing",
            {"voice_engine": "parakeet", "voice_model": "{W}/p.gguf"},
            bin=para,
        ),
        pick(
            "parakeet file is a dir",
            {"voice_engine": "parakeet", "voice_model": "{W}/pdir"},
            bin=para,
            files={"pdir/x": "x"},
        ),
        pick("parakeet no model set closed host", {"voice_engine": "parakeet"}, bin=para),
        pick(
            "parakeet default model value closed host",
            {"voice_engine": "parakeet", "voice_model": "tiny.en"},
            bin=para,
        ),
        pick(
            "parakeet v2 closed host xdg cache",
            {"voice_engine": "parakeet", "voice_model": "v2", "xdg_cache_home": "{W}/xdg"},
            bin=para,
        ),
        pick(
            "parakeet v2 host 404",
            {
                "voice_engine": "parakeet",
                "voice_model": "v2",
                "parakeet_base_url": "{HTTP}/p",
            },
            bin=para,
            http=[{"status": 404, "body": "no"}],
        ),
        pick(
            "parakeet V2 is a file name",
            {"voice_engine": "parakeet", "voice_model": "V2"},
            bin=para,
        ),
        pick("no config desktop with parakeet", {}, bin=para),
    ]
    return cases


def resident_cases() -> list[dict[str, Any]]:
    """The resident child, through a fake engine: one load and many clips, a
    crash and the restart, a malformed reply, a slow load, a failed load,
    and clips refused while a child loads."""
    clip = b64(wav(sr=16000, n=8000))
    clip2 = b64(wav(sr=16000, n=4000, samples=tone(4000, period=11)))
    cases: list[dict[str, Any]] = []

    def run(name: str, engine: str, spec: dict[str, Any], steps: list[dict[str, Any]], **q: Any):
        tool = "moonshine-cli" if engine == "moonshine" else "parakeet-cli"
        model = "{W}/moon" if engine == "moonshine" else "{W}/p.gguf"
        files = {"moon/tokenizer.bin": "x"} if engine == "moonshine" else {"p.gguf": "x"}
        cases.append(
            probe(
                f"resident {engine} {name}",
                {"op": "resident", "engine": engine, "model": model, "steps": steps, **q},
                bin={tool: spec},
                files=files,
            )
        )

    c, c2, wait = {"clip": clip}, {"clip": clip2}, {"wait": 10}
    for engine in ("moonshine", "parakeet"):
        run(
            "many clips",
            engine,
            {"replies": [{"text": "one"}, {"text": " Two.\n  three \n"}]},
            [c, c2, c],
        )
        run(
            "crash and restart",
            engine,
            {
                "replies": [
                    {"text": "a"},
                    {"crash": 7, "stderr": "load\nsegfault here\n"},
                    {"text": "b"},
                ]
            },
            [c, c, wait, c2],
        )
    run(
        "malformed reply",
        "parakeet",
        {"replies": [{"line": "not json"}, {"line": '{"text": 5}'}, {"text": "ok"}]},
        [c, wait, c, wait, c],
    )
    run(
        "error reply keeps the child",
        "parakeet",
        {"replies": [{"error": "the clip is bad"}, {"text": "ok"}]},
        [c, c],
    )
    run(
        "slow load within the timeout",
        "parakeet",
        {"starts": [{"delay": 0.5}]},
        [c],
        load_timeout_s=10.0,
    )
    run(
        "slow load past the timeout",
        "parakeet",
        {"starts": [{"delay": 5}]},
        [c],
        load_timeout_s=0.5,
    )
    run(
        "load error line",
        "parakeet",
        {"starts": [{"error": "the model /m/p.gguf did not load"}]},
        [c],
    )
    run(
        "exits before ready", "moonshine", {"starts": [{"exit": 4, "stderr": "x\nno model\n"}]}, [c]
    )
    run("first line not ready", "moonshine", {"starts": [{"line": "hello"}]}, [c])
    run(
        "old protocol ready line",
        "moonshine",
        {"starts": [{"line": '{"ready": "daisugi-voice-0"}'}]},
        [c],
    )
    run(
        "clips refused while it loads",
        "parakeet",
        {"starts": [{}, {"delay": 1.5}], "replies": [{"crash": 1}, {"text": "after"}]},
        [c, c, c, wait, c],
    )
    run(
        "a failed restart waits for the next clip",
        "parakeet",
        {"starts": [{}, {"error": "no model"}, {}], "replies": [{"crash": 1}, {"text": "third"}]},
        [c, wait, c, wait, c],
    )
    run(
        "clip timeout",
        "parakeet",
        {"replies": [{"delay": 5, "text": "late"}, {"text": "ok"}]},
        [c, wait, c],
        clip_timeout_s=0.5,
    )
    big = {"clip": b64(wav(sr=16000, n=80000))}
    run(
        "stops reading its stdin",
        "parakeet",
        {"starts": [{"stall": 30}, {}], "replies": [{"text": "after"}]},
        [big, wait, big],
        clip_timeout_s=0.5,
    )
    run(
        "unicode and control text",
        "moonshine",
        {"replies": [{"text": "caf\u00e9\t\u4e2d \u2028x"}]},
        [c],
    )
    for i, line in enumerate(
        [
            b'{"ready": "daisugi-voice-1", "load_ms": 3}',
            b'{"ready": "daisugi-voice-1"}\n',
            b'{"ready": "daisugi-voice-2"}',
            b'{"text": "hi", "decode_ms": 3}',
            b'{"error": "bad"}',
            b'{"text": "a", "error": "b"}',
            b'{"text": 3}',
            b'{"error": null}',
            b"[1, 2]",
            b'"text"',
            b"",
            b"\xff\xfe",
            b'{"text": "\\ud800"}',
            b'  {"text": "x"}  ',
            b'{"text": "x"} trailing',
            b"NaN",
        ]
    ):
        cases.append(probe(f"resident line {i}", {"op": "resident_line", "line_b64": b64(line)}))
    return cases


def choose_cases() -> list[dict[str, Any]]:
    """The engine a box with no voice choice gets: hardware and what is installed in."""
    cases = []
    every = ["faster-whisper", "moonshine", "parakeet"]
    for ram, cpus, vram, installed, fw in [
        (15.6, 4, 0.0, every, True),
        (15.6, 4, 0.0, ["moonshine", "parakeet"], False),
        (15.6, 4, 0.0, ["moonshine"], False),
        (15.6, 4, 0.0, [], False),
        (8.0, 4, 0.0, ["parakeet"], False),
        (7.9, 16, 0.0, every, True),
        (64.0, 2, 24.0, every, False),
        (16.0, 8, 5.9, ["parakeet"], False),
        (2.0, 1, 0.0, ["moonshine"], False),
        (1.9, 8, 0.0, every, True),
        (1.9, 8, 0.0, ["moonshine"], False),
        (None, 4, 0.0, ["moonshine"], False),
        (0.5, 1, 0.0, [], True),
        (31.3, 12, 0.0, ["faster-whisper", "moonshine"], True),
    ]:
        q = {"op": "choose_engine", "ram_gb": ram, "cpus": cpus, "vram_gb": vram}
        q.update({"installed": installed, "faster_whisper": fw})
        cases.append(
            probe(f"choose {ram} {cpus} {vram} {'+'.join(installed) or 'none'} fw={fw}", q)
        )
    return cases


def audio_cases() -> list[dict[str, Any]]:
    cases: list[dict[str, Any]] = []

    def to_wav(name: str, raw: bytes, ctype: str = "audio/wav", **extra: Any) -> None:
        cases.append(
            probe(
                f"to_wav {name}",
                {"op": "to_wav", "raw_b64": b64(raw), "content_type": ctype},
                **extra,
            )
        )

    to_wav("16k mono", wav(sr=16000, n=1600))
    to_wav("16k stereo", wav(sr=16000, n=800, channels=2))
    to_wav(
        "16k stereo odd sums", wav(sr=16000, n=4, channels=2, samples=[1, 2, -1, -2, 3, 0, -3, 0])
    )
    to_wav("16k three channels", wav(sr=16000, n=500, channels=3))
    to_wav("8k mono linear", wav(sr=8000, n=800))
    to_wav("44100 stereo linear", wav(sr=44100, n=4410, channels=2))
    to_wav("22050 odd length", wav(sr=22050, n=2207))
    to_wav("48k mono", wav(sr=48000, n=4801))
    to_wav("11025 one frame", wav(sr=11025, n=1))
    to_wav("8k zero frames", wav(sr=8000, n=0))
    to_wav("16k zero frames", wav(sr=16000, n=0))
    to_wav("extremes 8k", wav(sr=8000, n=4, samples=[32767, -32768, 32767, -32768]))
    to_wav("8-bit", wav(sr=16000, n=100, width=1))
    to_wav("24-bit", wav(sr=16000, n=100, width=3))
    to_wav("not wav no ffmpeg", b"OggS" + b"\x00" * 60, "audio/ogg")
    to_wav("empty no ffmpeg", b"", "audio/webm")
    to_wav("riff but not wave", b"RIFF\x10\x00\x00\x00AVI LIST\x00\x00\x00\x00")
    to_wav("short riff", b"RIFF\x04\x00\x00\x00WAVE")
    data = pcm(tone(320))
    to_wav("no fmt chunk", riff([(b"data", data)]))
    to_wav("no data chunk", riff([(b"fmt ", fmt_chunk())]))
    to_wav("float format", riff([(b"fmt ", fmt_chunk(fmt=3, width=4)), (b"data", data)]))
    ext = fmt_chunk(fmt=0xFFFE, extra=struct.pack("<HHI", 22, 16, 4))
    pcm_guid = b"\x01\x00\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71"
    to_wav("extensible pcm", riff([(b"fmt ", ext + pcm_guid), (b"data", data)]))
    to_wav("extensible float", riff([(b"fmt ", ext + b"\x03" + pcm_guid[1:]), (b"data", data)]))
    to_wav(
        "list chunk odd size", riff([(b"fmt ", fmt_chunk()), (b"LIST", b"abc"), (b"data", data)])
    )
    to_wav("data before fmt", riff([(b"data", data), (b"fmt ", fmt_chunk())]))
    to_wav("zero channels", riff([(b"fmt ", fmt_chunk(channels=0)), (b"data", data)]))
    to_wav("zero rate", riff([(b"fmt ", fmt_chunk(sr=0)), (b"data", data)]))
    to_wav("8k zero rate", riff([(b"fmt ", fmt_chunk(sr=0)), (b"data", b"")]))
    to_wav("odd data length", riff([(b"fmt ", fmt_chunk(sr=8000)), (b"data", data + b"\x01")]))
    trunc = wav(sr=8000, n=400)
    to_wav("truncated data", trunc[: len(trunc) - 301])
    to_wav("truncated in header", trunc[:30])
    to_wav("streamed sizes 8k", streamed(wav(sr=8000, n=300)))
    to_wav("streamed sizes 16k", streamed(wav(sr=16000, n=300)))
    to_wav("big riff size", riff([(b"fmt ", fmt_chunk()), (b"data", data)], riff_size=10**6))
    to_wav("fmt too short", riff([(b"fmt ", b"\x01\x00\x01\x00"), (b"data", data)]))
    to_wav(
        "bad chunk id bytes",
        riff([(b"fmt ", fmt_chunk()), (b"\xff\xfe\x00x", b"zz"), (b"data", data)]),
    )
    # ffmpeg present: a fake that answers a fixed streamed WAV.
    out8 = streamed(wav(sr=16000, n=200))
    to_wav("ffmpeg resamples 8k", wav(sr=8000, n=100), bin={"ffmpeg": {"out_b64": b64(out8)}})
    to_wav(
        "ffmpeg decodes ogg",
        b"OggS" + b"\x01" * 40,
        "audio/ogg",
        bin={"ffmpeg": {"out_b64": b64(out8)}},
    )
    to_wav("ffmpeg skipped at 16k", wav(sr=16000, n=100), bin={"ffmpeg": {"out_b64": b64(out8)}})
    to_wav(
        "ffmpeg fails",
        wav(sr=8000, n=100),
        bin={"ffmpeg": {"stderr": "Invalid data\n", "exit": 1}},
    )
    to_wav(
        "ffmpeg answers 8-bit",
        b"\x00webm",
        "audio/webm",
        bin={"ffmpeg": {"out_b64": b64(wav(sr=16000, n=50, width=1))}},
    )
    to_wav(
        "ffmpeg answers stereo 22050",
        b"\x00webm",
        "audio/webm",
        bin={"ffmpeg": {"out_b64": b64(streamed(wav(sr=22050, n=60, channels=2)))}},
    )
    to_wav(
        "ffmpeg answers nothing",
        b"\x00webm",
        "audio/webm",
        bin={"ffmpeg": {"out_b64": ""}},
    )
    for name, clip in [
        ("16k", wav(sr=16000, n=16000)),
        ("8k", wav(sr=8000, n=4000)),
        ("stereo", wav(sr=16000, n=8000, channels=2)),
        ("streamed", streamed(wav(sr=16000, n=1234))),
        ("zero", wav(sr=16000, n=0)),
        ("odd data", riff([(b"fmt ", fmt_chunk()), (b"data", b"\x01\x02\x03")])),
        ("garbage", b"nope"),
    ]:
        cases.append(probe(f"wav duration {name}", {"op": "wav_duration", "wav_b64": b64(clip)}))
    return cases


def multipart_cases() -> list[dict[str, Any]]:
    clip = wav(sr=16000, n=10)
    disp = 'Content-Disposition: form-data; name="audio"; filename="a.webm"'
    rows = [
        (
            "plain",
            multipart([(disp + "\r\nContent-Type: audio/webm", clip)]),
            "multipart/form-data; boundary=XyZ",
        ),
        ("quoted boundary", multipart([(disp, clip)]), 'multipart/form-data; boundary="XyZ"'),
        ("no part type", multipart([(disp, clip)]), "multipart/form-data; boundary=XyZ"),
        (
            "lower header",
            multipart([(disp + "\r\ncontent-type:  audio/ogg ", clip)]),
            "multipart/form-data; boundary=XyZ",
        ),
        ("no boundary", multipart([(disp, clip)]), "multipart/form-data"),
        ("wrong boundary", multipart([(disp, clip)]), "multipart/form-data; boundary=Other"),
        (
            "no disposition",
            multipart([("Content-Type: audio/webm", clip)]),
            "multipart/form-data; boundary=XyZ",
        ),
        (
            "no blank line",
            b"--XyZ\r\nContent-Disposition: x\r\nbody--XyZ--",
            "multipart/form-data; boundary=XyZ",
        ),
        (
            "two parts",
            multipart([("X-A: 1", b"skip"), (disp, b"first"), (disp, b"second")]),
            "multipart/form-data; boundary=XyZ",
        ),
        (
            "no trailing crlf",
            b"--XyZ\r\n" + disp.encode() + b"\r\n\r\npayload",
            "multipart/form-data; boundary=XyZ",
        ),
        (
            "boundary then params",
            multipart([(disp, clip)]),
            "multipart/form-data; boundary=XyZ; charset=utf-8",
        ),
        ("empty body", b"", "multipart/form-data; boundary=XyZ"),
        (
            "empty boundary",
            multipart([(disp, clip)], boundary=""),
            "multipart/form-data; boundary=",
        ),
        (
            "disposition in payload",
            b"--XyZ\r\nX: y\r\n\r\nContent-Disposition\r\n--XyZ--",
            "multipart/form-data; boundary=XyZ",
        ),
        (
            "latin-1 header",
            multipart([(disp + "\r\nContent-Type: audio/\xe9", b"z")]).replace(
                b"\xc3\xa9", b"\xe9"
            ),
            "multipart/form-data; boundary=XyZ",
        ),
        ("crlf only payload", multipart([(disp, b"\r\n")]), "multipart/form-data; boundary=XyZ"),
        (
            "content-type colon value",
            multipart([(disp + "\r\nContent-Type: a:b", b"z")]),
            "multipart/form-data; boundary=XyZ",
        ),
        ("BOUNDARY= upper", multipart([(disp, clip)]), "multipart/form-data; BOUNDARY=XyZ"),
    ]
    out = []
    for name, body, ctype in rows:
        out.append(
            probe(
                f"multipart {name}",
                {"op": "multipart", "body_b64": b64(body), "content_type": ctype},
            )
        )
    return out


def grant(key: str, armed: Any, expires: Any) -> dict[str, Any]:
    import urllib.parse

    return {
        f"armed/{urllib.parse.quote(key, safe='')}.json": {
            "text": json.dumps({"pane_key": key, "armed_at": armed, "expires_at": expires})
        }
    }


def arm_cases() -> list[dict[str, Any]]:
    now = 1_700_000_000.25
    cases = [
        probe("arm 30m", {"op": "arm", "pane_key": "w1:p1", "minutes": 30.0, "now": now}),
        probe("arm fraction", {"op": "arm", "pane_key": "w1:p1", "minutes": 2.5, "now": now}),
        probe("arm slash key", {"op": "arm", "pane_key": "a/b", "minutes": 1.0, "now": now}),
        probe(
            "arm unicode key", {"op": "arm", "pane_key": "caf\u00e9", "minutes": 1.0, "now": now}
        ),
        probe(
            "arm replaces",
            {
                "op": "arm",
                "pane_key": "p",
                "minutes": 5.0,
                "now": now,
                "files": grant("p", 1.0, 2.0),
            },
        ),
        probe(
            "arm keeps existing dir mode",
            {
                "op": "arm",
                "pane_key": "p",
                "minutes": 5.0,
                "now": now,
                "files": {"armed/": {"mode": 0o755}},
            },
        ),
        probe(
            "arm loosens no file mode",
            {
                "op": "arm",
                "pane_key": "p",
                "minutes": 5.0,
                "now": now,
                "files": {"armed/p.json": {"text": "old", "mode": 0o644}},
            },
        ),
        probe("arm long key", {"op": "arm", "pane_key": "k" * 300, "minutes": 1.0, "now": now}),
        probe(
            "arm dir is a file",
            {
                "op": "arm",
                "pane_key": "p",
                "minutes": 1.0,
                "now": now,
                "files": {"armed": {"text": "x"}},
            },
        ),
        probe("arm int now", {"op": "arm", "pane_key": "p", "minutes": 1.0, "now": 100}),
        probe("disarm none", {"op": "disarm", "pane_key": "w1:p1"}),
        probe(
            "disarm one", {"op": "disarm", "pane_key": "w1:p1", "files": grant("w1:p1", 1.0, 2.0)}
        ),
        probe(
            "disarm leaves others",
            {"op": "disarm", "pane_key": "a", "files": {**grant("a", 1, 2), **grant("b", 1, 2)}},
        ),
        probe("disarm long key", {"op": "disarm", "pane_key": "k" * 300}),
        probe(
            "disarm a directory",
            {"op": "disarm", "pane_key": "d", "files": {"armed/d.json/": {"mode": 0o700}}},
        ),
    ]
    cases.append(
        probe(
            "disarm through a file",
            {"op": "disarm", "pane_key": "p", "files": {"armed": {"text": "x"}}},
        )
    )
    expiries: list[tuple[str, Any]] = [
        ("future", now + 60),
        ("past", now - 60),
        ("equal", now),
        ("string inf", "inf"),
        ("string number", "1e20"),
        ("padded string", " 5e12 "),
        ("underscore string", "2_000_000_000"),
        ("string nan", "nan"),
        ("bool", True),
        ("list", [now + 60]),
        ("null", None),
        ("dict", {"a": 1}),
        ("huge int", "BIGINT"),
        ("zero", 0),
        ("negative", -1),
        ("unicode digits", "\uff12\uff10\uff10\uff10\uff10\uff10\uff10\uff10\uff10\uff10"),
        ("empty string", ""),
    ]
    for name, exp in expiries:
        files = grant("w1:p1", now, exp)
        if exp == "BIGINT":
            files = {"armed/w1%3Ap1.json": {"text": '{"expires_at": ' + "9" * 400 + "}"}}
        cases.append(
            probe(
                f"is_armed {name}",
                {"op": "is_armed", "pane_key": "w1:p1", "now": now, "files": files},
            )
        )
    for name, text in [
        ("Infinity literal", '{"expires_at": Infinity}'),
        ("NaN literal", '{"expires_at": NaN}'),
        ("1e999", '{"expires_at": 1e999}'),
        ("-Infinity", '{"expires_at": -Infinity}'),
        ("list root", "[1, 2]"),
        ("no key", '{"armed_at": 1}'),
        ("not json", "not json"),
        ("empty", ""),
        ("duplicate key", '{"expires_at": 1, "expires_at": 9e12}'),
        ("bom", '\ufeff{"expires_at": 9e12}'),
        ("trailing garbage", '{"expires_at": 9e12} x'),
        ("nested", '{"expires_at": {"expires_at": 9e12}}'),
        ("latin-1 bytes", "LATIN1"),
    ]:
        f: dict[str, Any] = {"armed/w1%3Ap1.json": {"text": text}}
        if text == "LATIN1":
            f = {"armed/w1%3Ap1.json": {"b64": b64(b'{"expires_at": 9e12, "x": "\xe9"}')}}
        cases.append(
            probe(
                f"is_armed {name}",
                {"op": "is_armed", "pane_key": "w1:p1", "now": now, "files": f},
            )
        )
    cases += [
        probe("is_armed no file", {"op": "is_armed", "pane_key": "w1:p1", "now": now}),
        probe("is_armed long key", {"op": "is_armed", "pane_key": "k" * 300, "now": now}),
        probe(
            "is_armed dir",
            {
                "op": "is_armed",
                "pane_key": "d",
                "now": now,
                "files": {"armed/d.json/": {"mode": 0o700}},
            },
        ),
        probe(
            "is_armed unreadable",
            {
                "op": "is_armed",
                "pane_key": "u",
                "now": now,
                "files": {"armed/u.json": {"text": '{"expires_at": 9e12}', "mode": 0o000}},
            },
        ),
        probe(
            "is_armed other key's file",
            {
                "op": "is_armed",
                "pane_key": "w1:p2",
                "now": now,
                "files": grant("w1:p1", now, now + 60),
            },
        ),
    ]
    future = grant("w1:p1", now, now + 60)
    for name, q in [
        ("preview", {"mode": "preview", "pane": "w1:p1"}),
        ("preview armed", {"mode": "preview", "pane": "w1:p1", "files": future}),
        ("send unarmed", {"mode": "send", "pane": "w1:p1"}),
        ("send armed", {"mode": "send", "pane": "w1:p1", "files": future}),
        ("send expired", {"mode": "send", "pane": "w1:p1", "files": grant("w1:p1", 1, now - 1)}),
        ("send other key", {"mode": "send", "pane": "w1:p1", "pane_key": "w1:p2", "files": future}),
        ("send by key", {"mode": "send", "pane": "zz", "pane_key": "w1:p1", "files": future}),
        ("send empty key", {"mode": "send", "pane": "w1:p1", "pane_key": ""}),
        ("unknown mode", {"mode": "Send", "pane": "w1:p1", "files": future}),
        ("empty mode", {"mode": "", "pane": "w1:p1"}),
        ("unicode mode", {"mode": "pr\u00e9view", "pane": "w1:p1"}),
        ("send quote in key", {"mode": "send", "pane": 'it\'s "x"'}),
    ]:
        cases.append(probe(f"deliver {name}", {"op": "deliver", "text": "hi", "now": now, **q}))
    return cases


def cleanup_cases() -> list[dict[str, Any]]:
    on = {"voice_cleanup": True, "voice_cleanup_model": "local-m"}
    ok = {"text": "Hello, world.", "usage": {"input_tokens": 12, "output_tokens": 4}}
    rows: list[tuple[str, dict[str, Any], str, dict[str, Any], dict[str, Any]]] = [
        ("off", {}, "hello world", ok, {}),
        ("on no model", {"voice_cleanup": True}, "hello world", ok, {}),
        ("model but off", {"voice_cleanup_model": "m"}, "hello world", ok, {}),
        ("empty model", {"voice_cleanup": True, "voice_cleanup_model": ""}, "hi", ok, {}),
        ("cleaned", on, "hello world", ok, {}),
        ("raises", on, "hello world", {"raise": True}, {}),
        ("empty reply", on, "hello world", {"text": ""}, {}),
        ("blank reply", on, "hello world", {"text": " \n\t "}, {}),
        ("reply stripped", on, "x", {"text": "  Done.  \n"}, {}),
        ("zero usage", on, "a b c", {"text": "A b c."}, {}),
        (
            "big usage",
            on,
            "ask",
            {"text": "Ask.", "usage": {"input_tokens": 10**12, "output_tokens": 7}},
            {},
        ),
        ("unicode", on, "caf\u00e9 \u4e2d\u6587 please", {"text": "Caf\u00e9."}, {}),
        ("long text", on, "word " * 400, {"text": "Words."}, {}),
        ("blank transcript", on, "   ", {"text": "x"}, {}),
        ("journal unwritable", on, "hello", ok, {"data/gateway": {"text": "a file"}}),
        (
            "journal appends",
            on,
            "hello again",
            ok,
            {"data/gateway/turns.jsonl": {"text": '{"old": 1}\n'}},
        ),
        (
            "model with slash",
            {"voice_cleanup": True, "voice_cleanup_model": "ollama/qwen"},
            "hi",
            ok,
            {},
        ),
        ("question", on, "what is the weather like today?", ok, {}),
    ]
    return [
        probe(
            f"cleanup {name}",
            {"op": "cleanup", "config": cfg, "text": text, "transport": tr, "files": files},
        )
        for name, cfg, text, tr, files in rows
    ]


def ptt_cases() -> list[dict[str, Any]]:
    c1, c2 = b64(pcm(tone(40))), b64(pcm(tone(30, phase=5)))
    hello = {"reply": {"text": "hello world"}}
    preview = {"reply": {"delivered": "preview", "reason": None}}
    rows: list[tuple[str, dict[str, Any]]] = [
        ("no keys", {"keys": ""}),
        ("quit first", {"keys": "q"}),
        ("other keys ignored", {"keys": "abc\r\n"}),
        (
            "one cycle",
            {
                "keys": "  ",
                "streams": [[[c1, True], [c2, False]]],
                "transcribe": [hello],
                "deliver": [preview],
            },
        ),
        (
            "cycle then quit",
            {
                "keys": "  q  ",
                "streams": [[[c1, False]]],
                "transcribe": [hello],
                "deliver": [preview],
            },
        ),
        ("quit while recording", {"keys": " q", "streams": [[[c1, False]]]}),
        ("unterminated recording", {"keys": " ", "streams": [[[c1, False]]]}),
        ("no audio", {"keys": "  ", "streams": [[]]}),
        (
            "empty chunks skipped",
            {
                "keys": "  ",
                "streams": [[["", True], [c1, False]]],
                "transcribe": [hello],
                "deliver": [preview],
            },
        ),
        (
            "no speech",
            {"keys": "  ", "streams": [[[c1, False]]], "transcribe": [{"reply": {"text": ""}}]},
        ),
        ("no text key", {"keys": "  ", "streams": [[[c1, False]]], "transcribe": [{"reply": {}}]}),
        (
            "cleanup changed",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [{"reply": {"text": "Hello.", "raw_text": "hello"}}],
                "deliver": [preview],
            },
        ),
        (
            "raw same",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [{"reply": {"text": "a", "raw_text": "a"}}],
                "deliver": [preview],
            },
        ),
        (
            "refused with reason",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [hello],
                "deliver": [{"reply": {"delivered": "refused", "reason": "pane not armed"}}],
            },
        ),
        (
            "deliver no word",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [hello],
                "deliver": [{"reply": {}}],
            },
        ),
        (
            "deliver odd word",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [hello],
                "deliver": [{"reply": {"delivered": 3, "reason": ""}}],
            },
        ),
        (
            "transcribe error then ok",
            {
                "keys": "    ",
                "streams": [[[c1, False]], [[c2, False]]],
                "transcribe": [{"error": "Could not reach the voice server."}, hello],
                "deliver": [preview],
            },
        ),
        (
            "deliver error",
            {
                "keys": "  ",
                "streams": [[[c1, False]]],
                "transcribe": [hello],
                "deliver": [{"error": "The voice server answered 500."}],
            },
        ),
        (
            "two cycles",
            {
                "keys": "    ",
                "streams": [[[c1, False]], [[c2, False]]],
                "transcribe": [hello, {"reply": {"text": "second"}}],
                "deliver": [preview, preview],
            },
        ),
        (
            "chunk frames",
            {
                "keys": "  ",
                "chunk_frames": 7,
                "streams": [[[c1, True], [c2, True]]],
                "transcribe": [hello],
                "deliver": [preview],
            },
        ),
        (
            "odd byte chunk",
            {
                "keys": "  ",
                "streams": [[[b64(b"\x01\x02\x03"), False]]],
                "transcribe": [hello],
                "deliver": [preview],
            },
        ),
        ("Q is not quit", {"keys": "Q", "streams": []}),
    ]
    return [probe(f"ptt {name}", {"op": "ptt", "pane": "w1:p1", **q}) for name, q in rows]


def client_cases() -> list[dict[str, Any]]:
    tok = {"data/coppice/web/token": {"text": "  secret-token \n"}}
    clip = b64(wav(sr=16000, n=10))
    tr = {"op": "client", "call": "transcribe", "wav_b64": clip, "url": "{HTTP}/", "files": tok}
    dl = {
        "op": "client",
        "call": "deliver",
        "pane": "w1:p1",
        "text": "hi \u00e9",
        "mode": "preview",
        "url": "{HTTP}",
        "files": tok,
    }
    j = "application/json"
    return [
        probe("client no token file", {**tr, "files": {}}),
        probe(
            "client blank token file", {**tr, "files": {"data/coppice/web/token": {"text": " \n"}}}
        ),
        probe("client transcribe ok", tr, http=[{"status": 200, "body": '{"text": "hi"}'}]),
        probe(
            "client transcribe 401",
            tr,
            http=[
                {"status": 401, "body": '{"error": "unauthorized", "message": "Needs a token."}'}
            ],
        ),
        probe(
            "client transcribe error only",
            tr,
            http=[{"status": 400, "body": '{"error": "bad_audio"}'}],
        ),
        probe(
            "client transcribe empty message",
            tr,
            http=[{"status": 400, "body": '{"error": "e", "message": ""}'}],
        ),
        probe(
            "client transcribe non-json error",
            tr,
            http=[{"status": 502, "body": "<html>bad gateway</html>", "content_type": "text/html"}],
        ),
        probe("client transcribe list error", tr, http=[{"status": 500, "body": "[1]"}]),
        probe(
            "client transcribe message number", tr, http=[{"status": 500, "body": '{"message": 5}'}]
        ),
        probe("client unreachable", {**tr, "url": "{CLOSED}"}),
        probe(
            "client deliver ok",
            dl,
            http=[{"status": 200, "body": '{"delivered": "preview", "reason": null}'}],
        ),
        probe(
            "client deliver refused 403",
            dl,
            http=[{"status": 403, "body": '{"delivered": "refused", "reason": "pane not armed"}'}],
        ),
        probe(
            "client deliver 403 no delivered",
            dl,
            http=[{"status": 403, "body": '{"error": "cross_origin_refused", "message": "No."}'}],
        ),
        probe(
            "client deliver 503",
            dl,
            http=[
                {
                    "status": 503,
                    "body": '{"error": "no_pane_backend", "message": "coppice is not available."}',
                }
            ],
        ),
        probe(
            "client deliver send mode",
            {**dl, "mode": "send"},
            http=[
                {"status": 200, "body": '{"delivered": "sent", "reason": null}', "content_type": j}
            ],
        ),
        probe("client deliver unreachable", {**dl, "url": "{CLOSED}"}),
    ]


# ---------------------------------------------------------------------------
# CLI cases
# ---------------------------------------------------------------------------

# The oracle CLI with faster_whisper hidden, as the binaries are built (VO-13).
HIDE_FASTER_WHISPER = "import sys; sys.modules['faster_whisper'] = None\n"
ORACLE_CLI = [
    sys.executable,
    "-c",
    HIDE_FASTER_WHISPER
    + "import runpy; runpy.run_module('opendaisugi.cli', run_name='__main__', alter_sys=True)",
]


def cli(name: str, argv: list[str], **extra: Any) -> dict[str, Any]:
    return {"kind": "cli", "name": "cli " + name, "argv": argv, **extra}


def norm_cli_text(text: str, work: Path) -> str:
    text = norm_text(text, work)
    text = re.sub(r"until \d\d:\d\d:\d\d\.", "until {HMS}.", text)
    text = re.sub(r'"expires_at": [0-9.e+]+', '"expires_at": "{T}"', text)
    return text


def home_tree(work: Path) -> dict[str, Any]:
    root = work / "home"
    out: dict[str, Any] = {}
    for p in sorted(root.rglob("*")):
        rel = str(p.relative_to(root))
        mode = p.lstat().st_mode & 0o777
        if p.is_dir():
            out[rel + "/"] = {"mode": mode}
            continue
        raw = p.read_bytes()
        text = raw.decode("utf-8", "replace")
        if "/armed/" in "/" + rel and rel.endswith(".json"):
            try:
                g = json.loads(text)
                g["window_s"] = round(g["expires_at"] - g["armed_at"], 3)
                g["armed_at"] = g["expires_at"] = "{T}"
                text = json.dumps(g, sort_keys=True)
            except (ValueError, KeyError, TypeError):
                pass
        out[rel] = {"mode": mode, "text": norm_text(text, work)}
    return out


def lay_home(work: Path, before: dict[str, Any]) -> None:
    root = work / "home"
    for rel, spec in before.items():
        p = root / rel
        if rel.endswith("/"):
            p.mkdir(parents=True, exist_ok=True)
            p.chmod(spec.get("mode", 0o700))
            continue
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(spec.get("text", "").replace("{W}", str(work)), encoding="utf-8")
        p.chmod(spec.get("mode", 0o644))


def run_cli(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    fresh_world(work, case)
    lay_home(work, case.get("before", {}))
    env = world_env(work)
    argv = [a.replace("{W}", str(work)) for a in case["argv"]]
    proc = subprocess.run(
        cmd + argv,
        input=case.get("stdin", "").encode(),
        capture_output=True,
        env=env,
        cwd=work / "home",
        timeout=60,
    )
    out = {
        "exit": proc.returncode,
        "stdout": norm_cli_text(proc.stdout.decode("utf-8", "replace"), work),
        "stderr": norm_cli_text(proc.stderr.decode("utf-8", "replace"), work),
        "tree": home_tree(work),
    }
    if case.get("help"):
        out["stdout"] = "{HELP}" if proc.stdout else ""
        out["stderr"] = "{HELP}" if proc.stderr else ""
    if case.get("usage"):
        out["stderr"] = "{USAGE}" if proc.stderr else ""
    return out


def cli_cases() -> list[dict[str, Any]]:
    tok = {".opendaisugi/coppice/web/token": {"text": TOKEN + "\n"}}
    cases = [
        cli("voice help", ["voice", "--help"], help=True),
        cli("voice serve help", ["voice", "serve", "--help"], help=True),
        cli("voice ptt help", ["voice", "ptt", "--help"], help=True),
        cli("voice arm help", ["voice", "arm", "--help"], help=True),
        cli("voice disarm help", ["voice", "disarm", "--help"], help=True),
        cli("voice arm no pane", ["voice", "arm"], usage=True),
        cli("voice disarm no pane", ["voice", "disarm"], usage=True),
        cli("voice no such verb", ["voice", "talk"], usage=True),
        cli("voice arm extra arg", ["voice", "arm", "a", "b"], usage=True),
        cli("voice arm unknown flag", ["voice", "arm", "a", "--bogus"], usage=True),
        cli("voice arm --for missing value", ["voice", "arm", "a", "--for"], usage=True),
    ]
    for spec in [
        None,
        "30m",
        "2h",
        "90",
        "2.5m",
        "3.5m",
        "0.5m",
        "1_0m",
        " 30M ",
        "1.5H",
        "\uff11\uff10m",
        "1e1",
        "10080",
        "168h",
        "10081",
        "0",
        "-5m",
        "abc",
        "",
        "m",
        "h",
        "inf",
        "nan",
        "-inf",
        "1e400",
        "30s",
        "30 m",
        "0.0001",
    ]:
        argv = ["voice", "arm", "w1:p1"] + ([] if spec is None else ["--for", spec])
        cases.append(cli(f"arm --for {spec!r}", argv))
        cases.append(cli(f"arm --for {spec!r} json", argv + ["--json"]))
    cases += [
        cli("arm --for= form", ["voice", "arm", "p", "--for=2h"]),
        cli("arm slash pane", ["voice", "arm", "a/b"]),
        cli("arm unicode pane", ["voice", "arm", "caf\u00e9", "--json"]),
        cli("arm long pane", ["voice", "arm", "k" * 300]),
        cli("arm data dir", ["voice", "arm", "p", "--data-dir", "{W}/home/d"]),
        cli("arm data dir relative", ["voice", "arm", "p", "--data-dir", "rel"]),
        cli(
            "arm armed is a file",
            ["voice", "arm", "p"],
            before={".opendaisugi/voice/armed": {"text": "x"}},
        ),
        cli(
            "arm replaces",
            ["voice", "arm", "p", "--for", "1m"],
            before={".opendaisugi/voice/armed/p.json": {"text": "old", "mode": 0o600}},
        ),
        cli("arm dash pane", ["voice", "arm", "--", "-x"]),
        cli("arm pane after flags", ["voice", "arm", "--for", "5m", "p"]),
        cli("arm json before pane", ["voice", "arm", "--json", "p"]),
        cli("arm empty pane", ["voice", "arm", ""]),
        cli(
            "disarm present",
            ["voice", "disarm", "w1:p1"],
            before={".opendaisugi/voice/armed/w1%3Ap1.json": {"text": "{}"}},
        ),
        cli("disarm absent", ["voice", "disarm", "w1:p1"]),
        cli(
            "disarm present json",
            ["voice", "disarm", "w1:p1", "--json"],
            before={".opendaisugi/voice/armed/w1%3Ap1.json": {"text": "{}"}},
        ),
        cli("disarm absent json", ["voice", "disarm", "w1:p1", "--json"]),
        cli("disarm long pane", ["voice", "disarm", "k" * 300]),
        cli(
            "disarm a directory",
            ["voice", "disarm", "d"],
            before={".opendaisugi/voice/armed/d.json/": {"mode": 0o700}},
        ),
        cli(
            "disarm through a file",
            ["voice", "disarm", "p"],
            before={".opendaisugi/voice/armed": {"text": "x"}},
        ),
        cli("disarm data dir", ["voice", "disarm", "p", "--data-dir", "{W}/home/d", "--json"]),
        # ptt: only paths that open no microphone. Empty stdin ends the key
        # stream before any space is read.
        cli("ptt bad scheme", ["voice", "ptt", "p", "--server", "ftp://x"]),
        cli("ptt no host", ["voice", "ptt", "p", "--server", "http://"]),
        cli("ptt not a url", ["voice", "ptt", "p", "--server", "localhost:7477"]),
        cli("ptt bad port", ["voice", "ptt", "p", "--server", "http://[::1"]),
        cli("ptt empty server", ["voice", "ptt", "p", "--server", ""]),
        cli("ptt no token", ["voice", "ptt", "p"]),
        cli("ptt token empty stdin", ["voice", "ptt", "p"], before=tok),
        cli("ptt token quit", ["voice", "ptt", "p"], before=tok, stdin="q"),
        cli("ptt token other keys", ["voice", "ptt", "p"], before=tok, stdin="abcq"),
        cli(
            "ptt server from config",
            ["voice", "ptt", "p"],
            before={
                **tok,
                ".opendaisugi/config.yaml": {"text": "voice_server_url: https://box.tail:9/\n"},
            },
        ),
        cli(
            "ptt bad server in config",
            ["voice", "ptt", "p"],
            before={".opendaisugi/config.yaml": {"text": "voice_server_url: nope\n"}},
        ),
        cli("ptt https server", ["voice", "ptt", "p", "--server", "https://h:1"], before=tok),
        cli("ptt data dir", ["voice", "ptt", "p", "--data-dir", "{W}/home/d"]),
        cli("ptt no pane", ["voice", "ptt"], usage=True),
    ]
    # serve: only the paths that stop before or at the bind.
    wcfg = "voice_engine: whisper.cpp\nvoice_model: {W}/home/ggml.bin\n"
    model = {"ggml.bin": {"text": "x"}}
    cases += [
        cli(
            "serve unknown engine",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: espeak\n"}, **tok},
        ),
        cli(
            "serve parakeet no binary",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: parakeet\n"}, **tok},
        ),
        cli(
            "serve parakeet no model set closed host",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: parakeet\n"}, **tok},
            bin={"parakeet-cli": {}},
        ),
        cli(
            "serve parakeet model file missing",
            ["voice", "serve"],
            before={
                ".opendaisugi/config.yaml": {
                    "text": "voice_engine: parakeet\nvoice_model: {W}/home/p.gguf\n"
                },
                **tok,
            },
            bin={"parakeet-cli": {}},
        ),
        cli(
            "serve parakeet load fails",
            ["voice", "serve"],
            before={
                ".opendaisugi/config.yaml": {
                    "text": "voice_engine: parakeet\nvoice_model: {W}/home/p.gguf\n"
                },
                "p.gguf": {"text": "x"},
                **tok,
            },
            bin={"parakeet-cli": {"starts": [{"error": "the model {W}/home/p.gguf did not load"}]}},
        ),
        cli(
            "serve no config desktop with parakeet",
            ["voice", "serve"],
            before=tok,
            bin={"parakeet-cli": {}},
        ),
        cli(
            "serve whisper.cpp no binary",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": wcfg}, **tok, **model},
        ),
        cli(
            "serve whisper.cpp no model",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": wcfg}, **tok},
            bin={"whisper-cli": {}},
        ),
        cli(
            "serve no token",
            ["voice", "serve", "--port", "1"],
            before={".opendaisugi/config.yaml": {"text": wcfg}, **model},
            bin={"whisper-cli": {}},
        ),
        cli(
            "serve no token listen",
            ["voice", "serve", "--listen", "0.0.0.0:1"],
            before={".opendaisugi/config.yaml": {"text": wcfg}, **model},
            bin={"whisper-cli": {}},
        ),
        cli(
            "serve blank token",
            ["voice", "serve", "--port", "1"],
            before={
                ".opendaisugi/config.yaml": {"text": wcfg},
                ".opendaisugi/coppice/web/token": {"text": "  \n"},
                **model,
            },
            bin={"whisper-cli": {}},
        ),
        cli(
            "serve missing token file flag",
            ["voice", "serve", "--port", "1", "--token-file", "{W}/home/nope"],
            before={".opendaisugi/config.yaml": {"text": wcfg}, **tok, **model},
            bin={"whisper-cli": {}},
        ),
        cli(
            "serve engine before token",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: nope\n"}},
        ),
    ]
    mcfg = "voice_engine: moonshine\nvoice_model: {W}/home/moon\n"
    mdir = {"moon/tokenizer.bin": {"text": "x"}}
    moon = {"moonshine-cli": {}}
    cases += [
        cli(
            "serve moonshine no binary",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": mcfg}, **tok, **mdir},
        ),
        cli(
            "serve moonshine no model dir",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": mcfg}, **tok},
            bin=moon,
        ),
        cli(
            "serve moonshine no model set closed host",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: moonshine\n"}, **tok},
            bin=moon,
        ),
        cli(
            "serve moonshine tiny closed host",
            ["voice", "serve"],
            before={
                ".opendaisugi/config.yaml": {
                    "text": "voice_engine: moonshine\nvoice_model: tiny\n"
                },
                **tok,
            },
            bin=moon,
        ),
        # No voice settings at all: the fresh box (VO-13).
        cli("serve no config closed host", ["voice", "serve"], before=tok, bin=moon),
        cli("serve no config no binary", ["voice", "serve"], before=tok),
        cli(
            "serve empty config closed host",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_device: cpu\n"}, **tok},
            bin=moon,
        ),
        cli(
            "serve faster-whisper named",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_engine: faster-whisper\n"}, **tok},
            bin=moon,
        ),
        cli(
            "serve saved default config",
            ["voice", "serve"],
            before={
                ".opendaisugi/config.yaml": {
                    "text": "matcher_model: lexical\nvoice_engine: faster-whisper\n"
                    "voice_model: tiny.en\nvoice_device: cpu\n"
                },
                **tok,
            },
            bin=moon,
        ),
        cli(
            "serve faster-whisper base.en",
            ["voice", "serve"],
            before={
                ".opendaisugi/config.yaml": {
                    "text": "voice_engine: faster-whisper\nvoice_model: base.en\n"
                },
                **tok,
            },
            bin=moon,
            port_expect={"stderr": PORT_FASTER_WHISPER + "\n"},
        ),
        cli(
            "serve model only",
            ["voice", "serve"],
            before={".opendaisugi/config.yaml": {"text": "voice_model: base.en\n"}, **tok},
            bin=moon,
            port_expect={"stderr": PORT_FASTER_WHISPER + "\n"},
        ),
    ]
    for listen in ["7477", "h:", ":", "h:x", "h:-1", "h:1:2", "[::1]:7477x", "h:\uff17"]:
        cases.append(
            cli(
                f"serve listen {listen!r}",
                ["voice", "serve", "--listen", listen],
                before={".opendaisugi/config.yaml": {"text": "voice_engine: nope\n"}},
            )
        )
    cases.append(cli("serve port not a number", ["voice", "serve", "--port", "x"], usage=True))
    return cases


# ---------------------------------------------------------------------------
# Server cases
# ---------------------------------------------------------------------------


def build_request(step: dict[str, Any], port: int, token: str) -> bytes:
    if "raw" in step:
        return step["raw"].replace("{PORT}", str(port)).replace("{TOKEN}", token).encode("latin-1")
    method = step.get("method", "POST")
    path = step.get("path", "/transcribe")
    if "json" in step:
        body = json.dumps(step["json"]).encode()
    elif "body_b64" in step:
        body = base64.b64decode(step["body_b64"])
    elif "wav" in step:
        sr, n, ch = step["wav"]
        body = wav(sr=sr, n=n, channels=ch)
    else:
        body = step.get("body", "").encode("utf-8")
    headers: list[tuple[str, str]] = [("Host", f"127.0.0.1:{port}")]
    auth = step.get("auth", "good")
    if auth == "good":
        headers.append(("Authorization", f"Bearer {token}"))
    elif auth is not None:
        headers.append(("Authorization", auth))
    for k, v in step.get("headers", []):
        headers.append((k, v))
    if not step.get("no_cl") and not any(k.lower() == "content-length" for k, _ in headers):
        if method != "GET" or body:
            headers.append(("Content-Length", str(len(body))))
    head = f"{method} {path} HTTP/1.1\r\n" + "".join(f"{k}: {v}\r\n" for k, v in headers) + "\r\n"
    return head.encode("latin-1") + body


def send_raw(port: int, data: bytes, timeout: float = 15.0) -> dict[str, Any]:
    with socket.create_connection(("127.0.0.1", port), timeout=timeout) as s:
        s.sendall(data)
        try:
            s.shutdown(socket.SHUT_WR)
        except OSError:
            pass
        buf = b""
        try:
            while True:
                chunk = s.recv(65536)
                if not chunk:
                    break
                buf += chunk
        except (TimeoutError, OSError):
            pass
    if not buf:
        return {"dropped": True}
    head, _, body = buf.partition(b"\r\n\r\n")
    lines = head.decode("latin-1").split("\r\n")
    parts = lines[0].split(" ", 2)
    out: dict[str, Any] = {"status": int(parts[1]) if len(parts) > 1 else None}
    for ln in lines[1:]:
        k, _, v = ln.partition(":")
        if k.strip().lower() == "content-type":
            out["content_type"] = v.strip()
    try:
        payload = json.loads(body)
        if isinstance(payload, dict) and isinstance(payload.get("rtf"), (int, float)):
            payload["rtf"] = "{RTF}" if payload["rtf"] >= 0 else payload["rtf"]
        out["json"] = payload
    except ValueError:
        out["text"] = body.decode("utf-8", "replace")
    return out


def wait_health(port: int, proc: subprocess.Popen, deadline: float) -> bool:
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            return False
        try:
            r = send_raw(port, b"GET /health HTTP/1.1\r\nHost: x\r\n\r\n", timeout=2.0)
            if r.get("status") == 200:
                return True
        except OSError:
            pass
        time.sleep(0.1)
    return False


def serve_config(case: dict[str, Any], work: Path, up_port: int | None) -> str:
    if case.get("engine") == "moonshine":
        engine = f"voice_engine: moonshine\nvoice_model: {work}/data/moon\n"
    elif case.get("engine") == "parakeet":
        engine = f"voice_engine: parakeet\nvoice_model: {work}/data/p.gguf\n"
    else:
        engine = f"voice_engine: whisper.cpp\nvoice_model: {work}/data/ggml.bin\n"
    cfg = (
        engine + "floor:\n"
        f"  backend: {case.get('backend', 'coppice')}\n"
        f"  coppice_socket: {work}/run/cop.sock\n"
    )
    cfg += case.get("config", "")
    if up_port is not None:
        cfg = cfg.replace("{UP}", str(up_port))
    if "{CLOSED}" in cfg:
        cfg = cfg.replace("{CLOSED}", str(free_port()))
    return cfg


def run_server(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    fresh_world(work, {"bin": case.get("bin", {"whisper-cli": {"stdout": "\n hello world\n"}})})
    data = work / "data"
    (data / "coppice" / "web").mkdir(parents=True)
    (data / "ggml.bin").write_text("x")
    (data / "moon").mkdir()
    (data / "moon" / "tokenizer.bin").write_text("x")
    (data / "p.gguf").write_text("x")
    token = case.get("token", TOKEN)
    if token is not None:
        (data / "coppice" / "web" / "token").write_text(token + "\n", encoding="utf-8")
    upstream = Answerer(case["upstream"]) if "upstream" in case else None
    (data / "config.yaml").write_text(
        serve_config(case, work, upstream.port if upstream else None), encoding="utf-8"
    )
    for key, secs in case.get("grants", {}).items():
        _write_grant(data, key, secs)
    coppice = None
    if "coppice" in case:
        cop = case["coppice"]
        coppice = FakeCoppice(work / "run" / "cop.sock", cop.get("panes", []), cop.get("fail"))
    port = free_port()
    argv = ["voice", "serve", "--port", str(port), "--data-dir", str(data)]
    busy = None
    if any("{BUSY}" in a for a in case.get("flags", [])):
        busy = socket.socket()
        busy.bind(("127.0.0.1", 0))
        busy.listen(1)
    flags = [a.replace("{W}", str(work)) for a in case.get("flags", [])]
    if busy is not None:
        flags = [a.replace("{BUSY}", str(busy.getsockname()[1])) for a in flags]
    argv += flags
    out_f = (work / "log" / "serve.out").open("wb")
    err_f = (work / "log" / "serve.err").open("wb")
    proc = subprocess.Popen(
        cmd + argv, stdout=out_f, stderr=err_f, env=world_env(work), cwd=work / "home"
    )
    replies: list[Any] = []
    try:
        up = wait_health(port, proc, time.monotonic() + 30)
        if up:
            for step in case["steps"]:
                op = step.get("op")
                if op == "token":
                    tf = data / "coppice" / "web" / "token"
                    if step.get("b64") is not None:
                        tf.write_bytes(base64.b64decode(step["b64"]))
                    elif step["text"] is None:
                        tf.unlink(missing_ok=True)
                    else:
                        tf.write_text(step["text"], encoding="utf-8")
                    continue
                if op == "grant":
                    _write_grant(data, step["pane"], step["seconds"])
                    continue
                reply = send_raw(port, build_request(step, port, token or ""))
                # A step that waits out a loading engine: sent again while
                # the answer is engine_loading; only the last answer counts.
                tries = 0
                while (
                    step.get("until_ready")
                    and tries < 200
                    and (reply.get("json") or {}).get("error") == "engine_loading"
                ):
                    time.sleep(0.05)
                    tries += 1
                    reply = send_raw(port, build_request(step, port, token or ""))
                replies.append(reply)
    finally:
        if proc.poll() is None:
            proc.send_signal(signal.SIGINT)
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        out_f.close()
        err_f.close()
        if coppice is not None:
            coppice.close()
        if upstream is not None:
            upstream.close()
        busy_port = busy.getsockname()[1] if busy is not None else None
        if busy is not None:
            busy.close()
    stdout = (work / "log" / "serve.out").read_text("utf-8", "replace")
    stderr = (work / "log" / "serve.err").read_text("utf-8", "replace")
    if busy_port is not None:
        stdout = stdout.replace(str(busy_port), "{BUSY}")
        stderr = stderr.replace(str(busy_port), "{BUSY}")
    res: dict[str, Any] = {
        "exit": proc.returncode,
        "stdout": norm_text(stdout.replace(str(port), "{PORT}"), work),
        "replies": norm(replies, work),
    }
    if not stdout or proc.returncode != 0:
        res["stderr"] = norm_text(stderr.replace(str(port), "{PORT}"), work)
    if coppice is not None:
        res["coppice"] = norm(coppice.lines, work)
    if upstream is not None:
        res["upstream"] = json.loads(
            json.dumps(norm(upstream.seen, work)).replace(str(upstream.port), "{UP}")
        )
    journal = data / "gateway" / "turns.jsonl"
    if journal.exists():
        recs = []
        for ln in journal.read_text("utf-8").splitlines():
            r = json.loads(ln)
            r["created_at"] = "{T}"
            recs.append(r)
        res["journal"] = recs
    res.update(tool_logs(work))
    return res


def _write_grant(data: Path, key: str, secs: float) -> None:
    import urllib.parse

    d = data / "voice" / "armed"
    d.mkdir(parents=True, exist_ok=True)
    now = time.time()
    (d / f"{urllib.parse.quote(key, safe='')}.json").write_text(
        json.dumps({"pane_key": key, "armed_at": now, "expires_at": now + secs})
    )


def server(name: str, steps: list[dict[str, Any]], **extra: Any) -> dict[str, Any]:
    return {"kind": "server", "name": "serve " + name, "steps": steps, **extra}


def server_cases() -> list[dict[str, Any]]:
    clip = wav(sr=16000, n=8000)
    tr = {
        "path": "/transcribe",
        "wav": [16000, 8000, 1],
        "headers": [["Content-Type", "audio/wav"]],
    }
    j = [["Content-Type", "application/json"]]

    def dl(payload: Any, **kw: Any) -> dict[str, Any]:
        return {"path": "/deliver", "json": payload, "headers": j, **kw}

    pty = {"panes": [{"id": "w1:p1", "kind": "pty", "label": "a"}]}
    headless = {"panes": [{"id": "w1:p1", "kind": "headless"}]}
    cases = [
        server(
            "health and 404s",
            [
                {"method": "GET", "path": "/health", "auth": None},
                {"method": "GET", "path": "/health?x=1", "auth": None},
                {"method": "GET", "path": "/", "auth": None},
                {"method": "GET", "path": "/transcribe"},
                {"method": "POST", "path": "/nope"},
                {"method": "POST", "path": "/nope", "auth": None},
                {"method": "POST", "path": "/health"},
                {"method": "POST", "path": "/transcribe/"},
                {"method": "POST", "path": "/TRANSCRIBE"},
            ],
        ),
        server(
            "methods http.server does not answer",
            [
                {"method": "PUT", "path": "/transcribe"},
                {"method": "HEAD", "path": "/health", "auth": None},
                {"method": "DELETE", "path": "/deliver"},
            ],
        ),
        server(
            "transcribe auth",
            [
                {**tr, "auth": None},
                {**tr, "auth": "Bearer wrong"},
                {**tr, "auth": f"bearer {TOKEN}"},
                {**tr, "auth": f"Bearer  {TOKEN}  "},
                {**tr, "auth": f"Basic {TOKEN}"},
                {**tr, "auth": "good"},
            ],
        ),
        server(
            "ban after three bad tokens",
            [
                {**tr, "auth": "Bearer a"},
                {**tr, "auth": "Bearer b"},
                {**tr, "auth": None},
                tr,
                {"method": "GET", "path": "/health", "auth": None},
                {"method": "POST", "path": "/nope"},
                dl({"pane": "p", "text": "t"}),
            ],
        ),
        server(
            "two bad tokens do not ban",
            [{**tr, "auth": "Bearer a"}, {**tr, "auth": "Bearer b"}, tr, tr],
        ),
        server(
            "transcribe a 16k clip",
            [
                tr,
                {**tr, "path": "/transcribe?cleanup=0"},
                {**tr, "path": "/transcribe?x=1&cleanup=2"},
            ],
        ),
        server("transcribe an 8k clip resamples", [{**tr, "wav": [8000, 4000, 1]}]),
        server(
            "transcribe a stereo clip",
            [{**tr, "wav": [44100, 4410, 2]}],
        ),
        server(
            "transcribe through ffmpeg",
            [
                {**tr, "wav": [8000, 4000, 1]},
                {
                    **tr,
                    "body_b64": b64(b"OggS" + b"\x02" * 50),
                    "headers": [["Content-Type", "audio/ogg"]],
                },
            ],
            bin={
                "whisper-cli": {"stdout": "via ffmpeg"},
                "ffmpeg": {"out_b64": b64(streamed(wav(sr=16000, n=3200)))},
            },
        ),
        server(
            "transcribe multipart",
            [
                {
                    "path": "/transcribe",
                    "body_b64": b64(
                        multipart(
                            [
                                (
                                    'Content-Disposition: form-data; name="audio"; filename="c.wav"\r\n'
                                    "Content-Type: audio/wav",
                                    clip,
                                )
                            ]
                        )
                    ),
                    "headers": [["Content-Type", "multipart/form-data; boundary=XyZ"]],
                },
                {
                    "path": "/transcribe",
                    "body_b64": b64(b"--XyZ--\r\n"),
                    "headers": [["Content-Type", "multipart/form-data; boundary=XyZ"]],
                },
                {
                    "path": "/transcribe",
                    "body_b64": b64(clip),
                    "headers": [["Content-Type", "multipart/form-data"]],
                },
            ],
        ),
        server(
            "transcribe bad audio",
            [
                {**tr, "body_b64": b64(b"not audio at all")},
                {**tr, "body_b64": b64(wav(sr=16000, n=10, width=1))},
                {**tr, "body_b64": b64(clip[: len(clip) // 2])},
                {**tr, "body_b64": b64(clip[:20])},
                {**tr, "body_b64": ""},
                {
                    **tr,
                    "body_b64": b64(
                        riff([(b"fmt ", fmt_chunk(channels=0)), (b"data", b"\x00" * 8)])
                    ),
                },
                {**tr, "body_b64": b64(wav(sr=8000, n=0))},
            ],
        ),
        server(
            "transcribe content length",
            [
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Content-Length", "abc"]]},
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Content-Length", "-1"]]},
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Content-Length", "30000001"]]},
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Content-Length", "1_0"]]},
                {
                    **tr,
                    "headers": [["Content-Type", "audio/wav"], ["Transfer-Encoding", "chunked"]],
                    "no_cl": True,
                },
                {**tr, "no_cl": True},
            ],
        ),
        server(
            "transcribe too long",
            [
                {**tr, "wav": [16000, 16000 * 61, 1]},
                {**tr, "wav": [16000, 16000 * 60, 1]},
            ],
        ),
        server(
            "transcribe cross origin",
            [
                {
                    **tr,
                    "headers": [["Content-Type", "audio/wav"], ["Origin", "https://evil.example"]],
                },
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Origin", ""]]},
                {
                    **tr,
                    "headers": [["Content-Type", "audio/wav"], ["Sec-Fetch-Site", "cross-site"]],
                },
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Sec-Fetch-Site", "same-site"]]},
                {
                    **tr,
                    "headers": [["Content-Type", "audio/wav"], ["Sec-Fetch-Site", "same-origin"]],
                },
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Sec-Fetch-Site", "none"]]},
                {**tr, "headers": [["Content-Type", "audio/wav"], ["Sec-Fetch-Site", "None"]]},
                {**tr, "headers": [["Content-Type", "audio/wav"], ["origin", "x"]]},
            ],
        ),
        server(
            "engine fails is 500",
            [tr],
            bin={"whisper-cli": {"stderr": "boom\n", "exit": 1}},
        ),
        server(
            "engine prints nothing",
            [tr],
            bin={"whisper-cli": {"stdout": ""}},
        ),
        server(
            "moonshine transcribes",
            [tr, {**tr, "wav": [8000, 4000, 1]}],
            engine="moonshine",
            bin={"moonshine-cli": {"replies": [{"text": "Hello there.\n"}]}},
        ),
        server(
            "moonshine fails is 500",
            [tr],
            engine="moonshine",
            bin={"moonshine-cli": {"replies": [{"error": "transcription failed"}]}},
        ),
        server(
            "parakeet transcribes many clips on one load",
            [tr, {**tr, "wav": [8000, 4000, 1]}, tr],
            engine="parakeet",
            bin={
                "parakeet-cli": {"replies": [{"text": "one"}, {"text": "two"}, {"text": "three"}]}
            },
        ),
        server(
            "parakeet crash then restart",
            [tr, tr, {**tr, "until_ready": True}],
            engine="parakeet",
            bin={
                "parakeet-cli": {
                    "replies": [{"text": "a"}, {"crash": 9, "stderr": "abort\n"}, {"text": "b"}]
                }
            },
        ),
        server(
            "parakeet refuses clips while it loads",
            [tr, tr, {**tr, "until_ready": True}],
            engine="parakeet",
            bin={
                "parakeet-cli": {
                    "starts": [{}, {"delay": 2}],
                    "replies": [{"crash": 1}, {"text": "after"}],
                }
            },
        ),
        server(
            "parakeet malformed reply",
            [tr, {**tr, "until_ready": True}],
            engine="parakeet",
            bin={"parakeet-cli": {"replies": [{"line": "garbage"}, {"text": "fine"}]}},
        ),
        server(
            "parakeet slow load",
            [tr],
            engine="parakeet",
            bin={"parakeet-cli": {"starts": [{"delay": 1}], "replies": [{"text": "late riser"}]}},
        ),
        server(
            "parakeet load fails",
            [tr],
            engine="parakeet",
            bin={"parakeet-cli": {"starts": [{"exit": 3, "stderr": "cannot map the model\n"}]}},
        ),
        server(
            "parakeet failed restart",
            [tr, {**tr, "until_ready": True}, {**tr, "until_ready": True}],
            engine="parakeet",
            bin={
                "parakeet-cli": {
                    "starts": [{}, {"error": "no model"}, {}],
                    "replies": [{"crash": 1}, {"text": "third start"}],
                }
            },
        ),
        server(
            "token rotates mid run",
            [tr, {"op": "token", "text": "new-token\n"}, tr, {"op": "token", "text": None}, tr],
        ),
        server(
            "cleanup query forms",
            [
                {**tr, "path": f"/transcribe?{q}"}
                for q in [
                    "cleanup=%31",
                    "cleanup=1&cleanup=0",
                    "cleanup=0&cleanup=1",
                    "cleanup",
                    "cleanup=",
                    "cleanup=+1",
                    "a=1;cleanup=1",
                    "a&cleanup=1",
                    "Cleanup=1",
                    "cleanup=1#frag",
                    "cleanup=%",
                    "cl%65anup=1",
                    "=1&cleanup=1",
                ]
            ],
            config="voice_cleanup: true\nvoice_cleanup_model: m\n"
            "voice_cleanup_base_url: http://127.0.0.1:{CLOSED}/v1\n",
        ),
        server(
            "a bearer token that is not ascii drops the connection",
            [
                {**tr, "auth": "Bearer t\u00f6k"},
                {**tr, "auth": "Bearer t\u00f6k"},
                {**tr, "auth": "Bearer t\u00f6k"},
                tr,
            ],
        ),
        server(
            "a token file that is not utf-8 drops the connection",
            [
                {"op": "token", "text": "", "b64": b64(b"\xfftok\n")},
                tr,
                {**tr, "auth": None},
                {"op": "token", "text": TOKEN},
                tr,
            ],
        ),
        server(
            "a token file with a non-ascii token",
            [{"op": "token", "text": "t\u00f6k\n"}, tr, {**tr, "auth": None}],
        ),
        server(
            "deliver checks",
            [
                dl({"pane": "w1:p1", "text": "hi"}),
                dl({"pane": "w1:p1", "text": "hi", "mode": "preview"}),
                dl({"pane": "w1:p1", "text": "hi", "mode": "send"}),
                dl({"pane": "w1:p1", "text": "hi", "mode": "Send"}),
                dl({"pane": "", "text": "hi"}),
                dl({"pane": 5, "text": "hi"}),
                dl({"text": "hi"}),
                dl({"pane": "p", "text": ""}),
                dl({"pane": "p", "text": None}),
                dl({"pane": "p"}),
                dl({"pane": "p", "text": "t", "backend": 3}),
                dl({"pane": "p", "text": "t", "backend": None}),
                dl({"pane": "p", "text": "t", "mode": None}),
                dl({"pane": "p", "text": "x" * 20000}),
                dl({"pane": "p", "text": "x" * 20001}),
                dl({"pane": "p", "text": "\u00e9" * 20000}),
                dl({"pane": "p", "text": "\U0001f600" * 15000}),
                {"path": "/deliver", "body": "not json", "headers": j},
                {"path": "/deliver", "body": "", "headers": j},
                {"path": "/deliver", "body": "[]", "headers": j},
                {"path": "/deliver", "body": "null", "headers": j},
                {"path": "/deliver", "body": '"s"', "headers": j},
                {"path": "/deliver", "body_b64": b64(b'{"pane": "\xff"}'), "headers": j},
                {
                    "path": "/deliver",
                    "body": '{"pane": "p", "text": "t", "pane": ""}',
                    "headers": j,
                },
                {"path": "/deliver", "body": '{"pane": "p", "text": NaN}', "headers": j},
                dl({"pane": "p", "text": "t"}, headers=[["Content-Type", "text/plain"]]),
                dl(
                    {"pane": "p", "text": "t"},
                    headers=[["Content-Type", "application/json; charset=utf-8"]],
                ),
                dl({"pane": "p", "text": "t"}, headers=[["Content-Type", "application/jsonx"]]),
                dl({"pane": "p", "text": "t"}, headers=[["Content-Type", "Application/JSON"]]),
                dl({"pane": "p", "text": "t"}, headers=[]),
                dl(
                    {"pane": "p", "text": "t"},
                    headers=[["Content-Type", "application/json"], ["Content-Length", "65537"]],
                ),
                dl(
                    {"pane": "p", "text": "t"},
                    headers=[["Content-Type", "application/json"], ["Content-Length", "x"]],
                ),
                dl(
                    {"pane": "p", "text": "t"},
                    headers=[["Content-Type", "application/json"], ["Origin", "null"]],
                ),
                dl(
                    {"pane": "p", "text": "t"},
                    headers=[
                        ["Content-Type", "application/json"],
                        ["Sec-Fetch-Site", "cross-site"],
                    ],
                ),
                {"path": "/deliver", "body": "{}", "headers": j, "no_cl": True},
            ],
            coppice=pty,
        ),
        server(
            "deliver send unarmed is refused",
            [
                dl({"pane": "w1:p1", "text": "hi", "mode": "send"}),
                dl({"pane": 'it\'s "x"', "text": "hi", "mode": "send"}),
                dl({"pane": "k" * 300, "text": "hi", "mode": "send"}),
            ],
            coppice=pty,
        ),
        server(
            "deliver send armed types into a pty pane",
            [
                dl({"pane": "w1:p1", "text": "hello pane", "mode": "send", "backend": "coppice"}),
                dl({"pane": "w1:p1", "text": "second", "mode": "send"}),
            ],
            coppice=pty,
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send armed prompts a headless pane",
            [dl({"pane": "w1:p1", "text": "do it", "mode": "send"})],
            coppice=headless,
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send to a pane coppice does not list",
            [dl({"pane": "w1:p9", "text": "hi", "mode": "send"})],
            coppice=pty,
            grants={"w1:p9": 600},
        ),
        server(
            "deliver send when coppice fails the send",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            coppice={**pty, "fail": "pane.send_text"},
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send when pane.list fails",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            coppice={**headless, "fail": "pane.list"},
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send with no coppice socket",
            [
                dl({"pane": "w1:p1", "text": "hi", "mode": "send"}),
                dl({"pane": "w1:p1", "text": "hi"}),
            ],
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send expired grant",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            coppice=pty,
            grants={"w1:p1": -5},
        ),
        server(
            "deliver send grant lands mid run",
            [
                dl({"pane": "w1:p1", "text": "a", "mode": "send"}),
                {"op": "grant", "pane": "w1:p1", "seconds": 600},
                dl({"pane": "w1:p1", "text": "b", "mode": "send"}),
            ],
            coppice=pty,
        ),
        server(
            "deliver send under backend auto with no coppice",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            backend="auto",
            grants={"w1:p1": 600},
            # VO-3: the ports deliver through coppice only.
            port_expect={
                "replies": [
                    {
                        "status": 503,
                        "content_type": "application/json",
                        "json": {
                            "error": "no_pane_backend",
                            "message": "no pane backend is available. coppice: coppice did not "
                            "answer. This daisugi delivers through coppice only. Start one: "
                            "`coppice server start`.",
                        },
                    }
                ]
            },
        ),
        server(
            "deliver send under backend auto with coppice",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            backend="auto",
            coppice=pty,
            grants={"w1:p1": 600},
        ),
        server(
            "deliver send under backend tmux",
            [
                dl({"pane": "w1:p1", "text": "hi", "mode": "send"}),
                dl({"pane": "w1:p1", "text": "hi"}),
            ],
            backend="tmux",
            coppice=pty,
            grants={"w1:p1": 600},
            port_expect={
                "replies": [
                    {
                        "status": 503,
                        "content_type": "application/json",
                        "json": {
                            "error": "no_pane_backend",
                            "message": "tmux is not available: this daisugi delivers through "
                            "coppice only. Set floor.backend to coppice or auto.",
                        },
                    },
                    {
                        "status": 200,
                        "content_type": "application/json",
                        "json": {"delivered": "preview", "reason": None},
                    },
                ]
            },
        ),
        server(
            "deliver send under an unknown backend",
            [dl({"pane": "w1:p1", "text": "hi", "mode": "send"})],
            backend="screen",
            coppice=pty,
            grants={"w1:p1": 600},
        ),
        server(
            "cleanup off reports raw",
            [{**tr, "path": "/transcribe?cleanup=1"}],
        ),
        server(
            "cleanup through a local model",
            [{**tr, "path": "/transcribe?cleanup=1"}, tr],
            config="voice_cleanup: true\nvoice_cleanup_model: tiny-local\n"
            "voice_cleanup_base_url: http://127.0.0.1:{UP}/v1\n",
            upstream=[
                {
                    "status": 200,
                    "body": json.dumps(
                        {
                            "id": "x",
                            "object": "chat.completion",
                            "model": "tiny-local",
                            "choices": [
                                {
                                    "index": 0,
                                    "message": {"role": "assistant", "content": "Hello, world."},
                                    "finish_reason": "stop",
                                }
                            ],
                            "usage": {
                                "prompt_tokens": 30,
                                "completion_tokens": 5,
                                "total_tokens": 35,
                            },
                        }
                    ),
                }
            ],
        ),
        server(
            "cleanup model fails",
            [{**tr, "path": "/transcribe?cleanup=1"}],
            config="voice_cleanup: true\nvoice_cleanup_model: tiny-local\n"
            "voice_cleanup_base_url: http://127.0.0.1:{UP}/v1\n",
            upstream=[{"status": 500, "body": '{"error": {"message": "down"}}'}],
        ),
        server(
            "cleanup model answers blank",
            [{**tr, "path": "/transcribe?cleanup=1"}],
            config="voice_cleanup: true\nvoice_cleanup_model: tiny-local\n"
            "voice_cleanup_base_url: http://127.0.0.1:{UP}/v1\n",
            upstream=[
                {
                    "status": 200,
                    "body": json.dumps(
                        {"choices": [{"message": {"role": "assistant", "content": "   "}}]}
                    ),
                }
            ],
        ),
        server(
            "half a tls pair",
            [],
            flags=["--tls-cert", "{W}/data/ggml.bin"],
        ),
        server(
            "listen on a port in use",
            [{"method": "GET", "path": "/health", "auth": None}],
            flags=["--listen", "127.0.0.1:{BUSY}"],
        ),
    ]
    return cases


# ---------------------------------------------------------------------------
# All cases, running them, writing them
# ---------------------------------------------------------------------------


def build_cases() -> list[dict[str, Any]]:
    return probe_cases() + cli_cases() + server_cases()


def all_cases() -> list[dict[str, Any]]:
    cases = build_cases()
    names = [c["name"] for c in cases]
    dup = {n for n in names if names.count(n) > 1}
    if dup:
        raise SystemExit(f"duplicate case names: {sorted(dup)}")
    return cases


def run_case(
    case: dict[str, Any], binary: str | None, probe_bin: str | None, work: Path
) -> dict[str, Any]:
    """Run one case on the oracle (binary None) or on a port."""
    kind = case["kind"]
    if kind == "probe":
        cmd = [sys.executable, str(PROBE_ORACLE)] if probe_bin is None else [probe_bin]
        return run_probe(case, cmd, work)
    cmd = ORACLE_CLI if binary is None else [binary]
    if kind == "cli":
        return run_cli(case, cmd, work)
    return run_server(case, cmd, work)


def write_jsonl(path: Path, items: list[dict[str, Any]]) -> dict[str, Any]:
    for it in items:
        it.pop("id", None)
        it["id"] = body_id(it)
    items.sort(key=lambda c: c["id"])
    data = "".join(canonical_json(c) + "\n" for c in items).encode("utf-8")
    path.write_bytes(data)
    return {"count": len(items), "sha256": hashlib.sha256(data).hexdigest()}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    ap.add_argument(
        "--only", help="run only cases whose name holds this text; print, write nothing"
    )
    ap.add_argument("--fresh", action="store_true", help="rerun every case")
    ap.add_argument(
        "--fresh-kind",
        choices=("probe", "cli", "server"),
        help="rerun every case of this kind, keep the others' answers",
    )
    args = ap.parse_args()
    out_dir: Path = args.out
    out_dir.mkdir(parents=True, exist_ok=True)
    SCRATCH.mkdir(parents=True, exist_ok=True)
    cases = all_cases()
    old: dict[str, dict[str, Any]] = {}
    if (out_dir / "cases.jsonl").exists() and not args.fresh:
        for ln in (out_dir / "cases.jsonl").read_text(encoding="utf-8").splitlines():
            if ln.strip():
                c = json.loads(ln)
                old[c["id"]] = c
    for i, c in enumerate(cases):
        if args.only and args.only not in c["name"]:
            continue
        prev = old.get(body_id(c))
        if prev is not None and not args.only and c["kind"] != args.fresh_kind:
            c["expect"] = prev["expect"]
            continue
        c["expect"] = run_case(c, None, None, SCRATCH / "gen" / f"{i:04d}")
        if args.only:
            print(
                json.dumps({"name": c["name"], **c["expect"]}, indent=1, ensure_ascii=False)[:6000]
            )
        else:
            print(f"{i + 1:4d}/{len(cases)} {c['kind']:6s} {c['name']}", flush=True)
    if args.only:
        return 0
    manifest = {"v": CASE_VERSION, "cases.jsonl": write_jsonl(out_dir / "cases.jsonl", cases)}
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=1, sort_keys=True) + "\n", encoding="utf-8"
    )
    shutil.rmtree(SCRATCH / "gen", ignore_errors=True)
    from fixture_paths import leaks

    leaked = leaks()
    if leaked:
        raise SystemExit("a machine path reached the fixtures:\n" + "\n".join(leaked[:10]))
    print(f"wrote {len(cases)} cases to {out_dir}")
    return 0


if __name__ == "__main__":
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    raise SystemExit(main())

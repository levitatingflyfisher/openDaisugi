"""A fake resident speech engine for tests and cases (src/opendaisugi/voice/resident.py).

``write_fake(path, spec, log)`` writes an executable at ``path`` that runs
``main(spec, log)`` under this Python, by absolute path, so it works with
PATH holding only the fake's own directory. It speaks the daisugi-voice-1
protocol and does what the spec file says. It never loads a model.

The spec is a JSON object:

- ``starts``: a list, one entry per start of the program (the last one
  repeats). Each entry may hold ``delay`` (seconds before the ready line),
  ``error`` (write {"error": ...} and exit 3), ``exit`` with ``stderr``
  (exit with that code and no line), ``line`` (write this line in place
  of the ready line) or ``stall`` (seconds to wait after the ready line
  before it reads stdin at all).
- ``replies``: a list, one entry per clip over all starts (the last one
  repeats; the default is {"text": "hello world"}). Each entry is
  ``{"text": T}``, ``{"error": E}``, ``{"crash": CODE, "stderr": S}``,
  ``{"line": RAW}`` (write RAW as the reply line), and may add
  ``delay`` (seconds before the reply).

The log is JSON lines: {"start": argv, "pid": PID} for each start, {"clip": sha256}
for each clip, and {"eof": true} for a clean end of stdin.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import time
from pathlib import Path


def write_fake(path: Path, spec: Path, log: Path, python: str | None = None) -> None:
    here = str(Path(__file__).resolve().parent)
    py = python or sys.executable
    path.write_text(
        f"#!{py} -S\n"
        "import sys\n"
        f"sys.path.insert(0, {here!r})\n"
        "import fake_resident\n"
        f"fake_resident.main({str(spec)!r}, {str(log)!r})\n",
        encoding="utf-8",
    )
    path.chmod(0o755)


def _log(log: str, obj: dict) -> None:
    with open(log, "a", encoding="utf-8") as f:
        f.write(json.dumps(obj) + "\n")


def _count(log: str, key: str) -> int:
    try:
        lines = Path(log).read_text(encoding="utf-8").splitlines()
    except OSError:
        return 0
    return sum(1 for ln in lines if key in json.loads(ln))


def _pick(items: list, i: int, default: dict) -> dict:
    if not items:
        return default
    return items[min(i, len(items) - 1)]


def _read(n: int) -> bytes | None:
    buf = b""
    while len(buf) < n:
        chunk = sys.stdin.buffer.read(n - len(buf))
        if not chunk:
            return None if not buf else buf
        buf += chunk
    return buf


def _say(line: str) -> None:
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def main(spec_path: str, log: str) -> None:
    spec = json.loads(Path(spec_path).read_text(encoding="utf-8"))
    start = _pick(spec.get("starts", []), _count(log, "start"), {})
    _log(log, {"start": sys.argv[1:], "pid": os.getpid()})
    time.sleep(start.get("delay", 0))
    if "error" in start:
        _say(json.dumps({"error": start["error"]}))
        sys.exit(3)
    if "exit" in start:
        sys.stderr.write(start.get("stderr", ""))
        sys.exit(start["exit"])
    _say(start.get("line", json.dumps({"ready": "daisugi-voice-1", "load_ms": 0})))
    time.sleep(start.get("stall", 0))
    while True:
        head = _read(4)
        if head is None:
            _log(log, {"eof": True})
            sys.exit(0)
        if len(head) < 4:
            sys.exit(2)
        n = int.from_bytes(head, "big")
        body = _read(n) if n else b""
        if body is None or len(body) < n or n == 0:
            sys.exit(2)
        k = _count(log, "clip")
        _log(log, {"clip": hashlib.sha256(body).hexdigest()})
        reply = _pick(spec.get("replies", []), k, {"text": "hello world"})
        time.sleep(reply.get("delay", 0))
        if "crash" in reply:
            sys.stderr.write(reply.get("stderr", ""))
            sys.stderr.flush()
            sys.exit(reply["crash"])
        if "line" in reply:
            _say(reply["line"])
        elif "error" in reply:
            _say(json.dumps({"error": reply["error"]}))
        else:
            _say(json.dumps({"text": reply.get("text", "")}))

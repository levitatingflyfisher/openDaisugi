"""The pack worker: a child process in a pack's own Python that runs one
job at a time for the daisugi binary that started it. This module is the
one definition of the protocol between the caller and the worker. The
Python caller is opendaisugi.pack.client; the Go and Rust callers follow
this text.

The worker uses only the standard library at import time, so it runs in
any pack. A pack carries a copy of it, made by `daisugi pack install`.

The protocol (version daisugi-pack-1):

- The caller starts `PACK/venv/bin/python -I PACK/worker/daisugi_pack_worker.py
  --pack NAME`. The worker writes one line on stdout, the ready line: a
  JSON object whose "ready" is "daisugi-pack-1", with "pack" and
  "python" (the version of the pack's Python). A caller that reads
  another "ready" refuses the worker and asks for a new install.
- Each request is a frame on stdin, framed as daisugi-voice-1 frames a
  clip: four bytes, the length N as an unsigned big-endian integer, then
  N bytes. Here the N bytes are a UTF-8 JSON object
  {"job": JOB, "args": [ARG, ...]}, every ARG a string. N is from 1 to
  MAX_FRAME_BYTES.
- For each request the worker writes zero or more progress lines
  {"progress": "TEXT"}, then one end line: {"result": OBJECT} when the
  job is done, or {"error": "WHY"} when it failed. Then it waits for the
  next frame. A job that fails never ends the worker.
- End of file on stdin at a frame boundary means exit 0. A length of 0
  or over MAX_FRAME_BYTES, or end of file inside a frame, means the
  framing is lost: the worker exits 2.
- stdout carries the ready line and the replies, nothing else. On start
  the worker keeps its own copy of stdout and points file descriptor 1
  at stderr, so text a library prints goes to stderr.
- stderr is free text. A caller keeps its last lines and names the last
  one when the worker dies.

What the caller does (all three do the same): it waits for the ready
line, sends one request, prints each progress text on its stderr, and
on the end line closes stdin and waits for the worker to exit. A result
is printed as JSON on stdout (exit 0). An error is one line on stderr,
"pack NAME: JOB: WHY" (exit 1). A worker that dies, or writes a line
that is not a reply, is one line on stderr (exit 1), never a crash.

The jobs: "selftest" (import the pack's modules), "train" (run the LoRA
trainer, lora_train.py beside this file) and "vla-chunk" (one action
chunk from the SmolVLA reference policy, vla_oracle.py beside this
file). "echo" and "die" exist only for the protocol's own tests, when
DAISUGI_PACK_TEST_JOBS=1.
"""

from __future__ import annotations

import json
import os
import platform
import re
import struct
import subprocess
import sys
from pathlib import Path

PROTOCOL = "daisugi-pack-1"
MAX_FRAME_BYTES = 64 * 1024 * 1024
HERE = Path(__file__).resolve().parent
BAD_REQUEST = 'a request is a JSON object with a string "job" and a list "args"'


class JobError(Exception):
    """A job that failed, with the reason the caller shows."""


def ready_line(pack: str) -> dict:
    return {"ready": PROTOCOL, "pack": pack, "python": platform.python_version()}


def request(job: str, args: list[str]) -> bytes:
    """One request frame: the length, big-endian, then the JSON body."""
    body = json.dumps({"job": job, "args": list(args)}).encode("utf-8")
    return struct.pack(">I", len(body)) + body


def reply_kind(obj: object) -> str | None:
    """'ready', 'progress', 'result' or 'error' for a line the caller
    reads, else None."""
    if not isinstance(obj, dict):
        return None
    if isinstance(obj.get("ready"), str):
        return "ready"
    if isinstance(obj.get("progress"), str):
        return "progress"
    if isinstance(obj.get("result"), dict):
        return "result"
    if isinstance(obj.get("error"), str):
        return "error"
    return None


def flag_value(args: list[str], flag: str) -> str | None:
    """The value of `--flag V` or `--flag=V` in args, the last one."""
    got = None
    for i, a in enumerate(args):
        if a == flag and i + 1 < len(args):
            got = args[i + 1]
        elif a.startswith(flag + "="):
            got = a[len(flag) + 1 :]
    return got


# ---------------------------------------------------------------------------
# Jobs
# ---------------------------------------------------------------------------


def _pack_info() -> dict:
    try:
        return json.loads((HERE / "pack.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def job_selftest(args, emit):
    """Import each module the pack carries (or the args), and give their
    versions."""
    import importlib

    mods = list(args) or list(_pack_info().get("check") or [])
    found = {}
    for m in mods:
        try:
            mod = importlib.import_module(m)
        except Exception as e:  # noqa: BLE001 - any import failure is the answer
            raise JobError(f"cannot import {m}: {type(e).__name__}: {e}") from None
        found[m] = str(getattr(mod, "__version__", ""))
    return {"python": platform.python_version(), "imports": found}


def _lines(stream):
    """The text lines of a byte stream, split at \\n or \\r (progress bars
    redraw with \\r)."""
    buf = b""
    while True:
        chunk = stream.read1(65536) if hasattr(stream, "read1") else stream.read(65536)
        if not chunk:
            break
        buf += chunk
        while True:
            cut = [i for i in (buf.find(b"\n"), buf.find(b"\r")) if i >= 0]
            if not cut:
                break
            i = min(cut)
            yield buf[:i].decode("utf-8", errors="replace")
            buf = buf[i + 1 :]
    if buf:
        yield buf.decode("utf-8", errors="replace")


# The last line of a Python traceback: "SomeError: why" (a progress bar
# can close after it, so the last line is not always the reason).
_EXCEPTION = re.compile(r"^[A-Za-z_][\w.]*(Error|Exception|Interrupt|Exit)\b")


def job_train(args, emit):
    """Run the LoRA trainer in a child of this Python, each of its output
    lines a progress line. The caller resolves the base model."""
    if flag_value(args, "--base-model") is None:
        raise JobError("the train job needs --base-model; daisugi lora train fills it in")
    out = flag_value(args, "--output")
    env = dict(os.environ, PYTHONUNBUFFERED="1")
    proc = subprocess.Popen(
        [sys.executable, "-I", str(HERE / "lora_train.py"), *args],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=env,
    )
    last = ""
    raised = ""
    for line in _lines(proc.stdout):
        if line.strip():
            emit(line)
            last = line.strip()
            if _EXCEPTION.match(last):
                raised = last
    rc = proc.wait()
    if rc != 0:
        why = raised or last
        raise JobError(f"the trainer exited {rc}: {why}" if why else f"the trainer exited {rc}")
    return {"adapter": str(Path(out).resolve()) if out else None}


def job_vla_chunk(args, emit):
    """One action chunk from the SmolVLA reference policy for the input
    file (vla_oracle.chunk_from_file)."""
    path = flag_value(args, "--input")
    if path is None:
        raise JobError("the vla-chunk job needs --input FILE")
    sys.path.insert(0, str(HERE))
    import vla_oracle

    try:
        case = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        raise JobError(f"cannot read {path}: {type(e).__name__}") from None
    try:
        return vla_oracle.chunk_from_case(case, emit)
    except vla_oracle.CaseError as e:
        raise JobError(str(e)) from None


def job_echo(args, emit):
    for a in args:
        print(f"echo: {a}", flush=True)
        emit(a)
    return {"args": list(args)}


def job_die(args, emit):
    sys.stderr.write("die: asked to exit\n")
    sys.stderr.flush()
    os._exit(int(args[0]) if args else 3)


JOBS = {"selftest": job_selftest, "train": job_train, "vla-chunk": job_vla_chunk}
TEST_JOBS = {"echo": job_echo, "die": job_die}


# ---------------------------------------------------------------------------
# The loop
# ---------------------------------------------------------------------------


def _read_exact(stream, n: int) -> bytes:
    buf = b""
    while len(buf) < n:
        chunk = stream.read(n - len(buf))
        if not chunk:
            break
        buf += chunk
    return buf


def serve(pack: str, stdin, out) -> int:
    def send(obj: dict) -> None:
        out.write((json.dumps(obj) + "\n").encode("utf-8"))
        out.flush()

    jobs = dict(JOBS)
    if os.environ.get("DAISUGI_PACK_TEST_JOBS") == "1":
        jobs.update(TEST_JOBS)
    send(ready_line(pack))
    while True:
        head = _read_exact(stdin, 4)
        if not head:
            return 0
        if len(head) < 4:
            return 2
        (n,) = struct.unpack(">I", head)
        if n == 0 or n > MAX_FRAME_BYTES:
            return 2
        body = _read_exact(stdin, n)
        if len(body) < n:
            return 2
        try:
            req = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, ValueError):
            req = None
        if (
            not isinstance(req, dict)
            or not isinstance(req.get("job"), str)
            or not isinstance(req.get("args"), list)
            or not all(isinstance(a, str) for a in req["args"])
        ):
            send({"error": BAD_REQUEST})
            continue
        fn = jobs.get(req["job"])
        if fn is None:
            send({"error": f"no job named {req['job']} in this worker"})
            continue
        try:
            result = fn(req["args"], lambda text: send({"progress": text}))
        except JobError as e:
            send({"error": str(e)})
        except Exception as e:  # noqa: BLE001 - a job never ends the worker
            send({"error": f"{type(e).__name__}: {e}"})
        else:
            send({"result": result})


def main(argv: list[str]) -> int:
    pack = flag_value(argv, "--pack") or ""
    # Keep stdout for the protocol; point fd 1 (and so print) at stderr.
    out = os.fdopen(os.dup(1), "wb", buffering=0)
    os.dup2(2, 1)
    return serve(pack, sys.stdin.buffer, out)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

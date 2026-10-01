"""The caller side of the pack worker protocol (defined in
opendaisugi.pack.worker): start the worker in the pack's Python, send one
job, stream its progress, and turn every way it can end into an
Outcome. Nothing the worker does raises here."""

from __future__ import annotations

import queue
import subprocess
import threading
from collections import deque
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.childline import exit_reason, parse_line
from opendaisugi.pack.worker import PROTOCOL, reply_kind, request

LOAD_TIMEOUT_S = 300.0
STOP_GRACE_S = 2.0
STDERR_LINES = 20
WORKER_FILE = "daisugi_pack_worker.py"


@dataclass(frozen=True)
class Outcome:
    code: int
    result: dict | None
    error: str | None


def worker_argv(pack_dir: Path, name: str) -> list[str]:
    return [
        str(pack_dir / "venv" / "bin" / "python"),
        "-I",
        str(pack_dir / "worker" / WORKER_FILE),
        "--pack",
        name,
    ]


def run_job(
    pack_dir: Path,
    name: str,
    job: str,
    args: list[str],
    on_progress: Callable[[str], None],
    env: dict[str, str] | None = None,
) -> Outcome:
    """Run one job in the pack's worker."""
    argv = worker_argv(Path(pack_dir), name)
    try:
        proc = subprocess.Popen(
            argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
    except OSError:
        return Outcome(1, None, f"pack {name}: cannot start the worker ({argv[0]})")
    tail: deque[str] = deque(maxlen=STDERR_LINES)
    lines: queue.Queue[bytes | None] = queue.Queue()

    def read_err() -> None:
        for raw in proc.stderr:
            text = raw.decode("utf-8", errors="replace").rstrip("\n")
            if text.strip():
                tail.append(text)

    def read_out() -> None:
        for raw in proc.stdout:
            lines.put(raw)
        lines.put(None)

    t_err = threading.Thread(target=read_err, daemon=True)
    t_out = threading.Thread(target=read_out, daemon=True)
    t_err.start()
    t_out.start()

    def dead() -> Outcome:
        rc = proc.wait()
        t_err.join(5)
        return Outcome(1, None, f"pack {name}: the worker {exit_reason(rc, list(tail))}")

    def stop() -> None:
        try:
            proc.stdin.close()
        except OSError:
            pass
        try:
            proc.wait(STOP_GRACE_S)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()

    def kill() -> None:
        proc.kill()
        proc.wait()

    def not_a_reply(raw: bytes) -> Outcome:
        kill()
        text = raw.decode("utf-8", errors="replace").strip()[:80]
        return Outcome(1, None, f"pack {name}: the worker wrote a line that is not a reply: {text}")

    try:
        first = lines.get(timeout=LOAD_TIMEOUT_S)
    except queue.Empty:
        kill()
        return Outcome(1, None, f"pack {name}: the worker was not ready in {LOAD_TIMEOUT_S:g} s")
    if first is None:
        return dead()
    obj = parse_line(first)
    if reply_kind(obj) != "ready":
        return not_a_reply(first)
    if obj["ready"] != PROTOCOL:
        kill()
        return Outcome(
            1,
            None,
            f"pack {name}: the worker speaks {obj['ready']}, not {PROTOCOL}. "
            f"Install it again: daisugi pack install {name} --force",
        )
    try:
        proc.stdin.write(request(job, args))
        proc.stdin.flush()
    except OSError:
        pass
    while True:
        raw = lines.get()
        if raw is None:
            return dead()
        obj = parse_line(raw)
        kind = reply_kind(obj)
        if kind == "progress":
            on_progress(obj["progress"])
        elif kind == "result":
            stop()
            return Outcome(0, obj["result"], None)
        elif kind == "error":
            stop()
            return Outcome(1, None, f"pack {name}: {job}: {obj['error']}")
        else:
            return not_a_reply(raw)

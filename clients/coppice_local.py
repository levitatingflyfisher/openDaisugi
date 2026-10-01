"""The local request kinds of the coppice compare driver.

A case line in the cases suite is a protocol request, sent on the side's
connection, unless it carries one of the keys below. Then the driver runs it
itself, on the side's own machine state, and builds a reply object that the
matcher and the normalization read like any other reply:
{"id": ID, "ok": true, "result": {...}}.

- `cli`: run the side's coppice binary. The driver puts `--socket S
  --data-dir D` first and refuses an argv that names either flag. The run has
  the server's environment, with COPPICE_NO_AUTOSTART=1, the request's
  `env`, `cwd` (default {START}) and `stdin` (default empty). `"alt": true`
  names a second socket and data dir, {WORK}/alt.sock and {WORK}/alt-data,
  for the cases that start a detached server of their own; `"autostart":
  true` leaves COPPICE_NO_AUTOSTART unset. `"peer": true` runs the other
  side's binary on this side's socket and data dir, so one data dir is read
  by both ports. With `"bg": NAME` the run goes on
  in the background, and a later `{"stop": NAME}` ends it. The result is
  {"code", "stdout", "stderr"}.
- `stop`: end a background run: SIGTERM by default, or `"signal": "INT"`,
  or `"signal": "none"` to wait for its own exit, at most `wait_ms`.
- `tmux`: run tmux on the side's scratch tmux server, `tmux -S
  {WORK}/tmux.sock ARGS`, with TMUX_TMPDIR in the scratch dir and no TMUX or
  TMUX_PANE. The result is {"code", "stdout", "stderr"}.
- `write`: write `text` to a file under the side's work dir, making its
  parent directories. The result is {"written": true}.
- `sleep`: wait that many milliseconds.
- `file`: read one file. The result is {"text", "mode"} or {"missing": true},
  or with `count`, {"count": N}, how many times that text is in the file.
- `ls`: list a directory tree: each path, its type and its mode.
- `log`: the lines of the server log that hold the given text (that start
  with it, with `"prefix": true`), at most `first` of them, with each slog
  time masked.
- `pty`: run the binary on a pseudo terminal of `cols` x `rows`, feed it the
  `steps` (`{"send": TEXT}`, `{"until": TEXT, "ms": N}`, `{"sleep": MS}`),
  then wait up to `end_ms` for it to exit. The result is the exit code (null
  when it had to be killed), the alternate screen as it was last drawn, and
  the main screen, each a list of rows with the trailing blanks cut. The
  screens are read by a small terminal model that knows what attach writes.
- `screen`: the screen of a pane (`"screen": PANE`, on the alt server's
  socket with `"alt": true`) as the side's own server draws it through its
  libghostty-vt: a view-only attach on a connection with no hello, and the
  first full frame. With `until` it waits for that text on a row, then for
  two frames in a row, `settle_ms` (default 400) apart, that are the same,
  for at most `ms` (default 15000). The result is {"rows", "cursor"}: the
  rows with the trailing blanks cut, and the cursor's column and row. A
  row's age, an ended row's "ago" and a held row's wait are masked as
  {AGE}, and the tail of such a row that the rail cuts short as {CUT}
  (CP-R-41). A screen that never shows its text, or never settles, is a
  reply with ok false and the screen as its message.
- `http`: one HTTP/1.1 request, the method named, to `url` (http or https;
  for https, `cafile` is the CA the server must chain to, and `sni` the name
  to check, default the URL's host). It sends `headers` and `body`, and
  `Connection: close`, and reads the whole response. With `wait_ms` a
  refused connection is tried again for that long, while the server starts.
  The result is {"status", "headers", "body"}: every header but Date, by the
  name the server sent, and the body as text. A chunked body is joined and
  `"chunked": true` added. `proto` sends another version (HTTP/1.0); a
  100 Continue before the reply is skipped and noted as `"interim"`. With `same_as` a body equal to that file's bytes
  is written as {SAME_AS_FILE}. With `refused_ok` a refused connection is
  the result {"refused": true}, not a failure. A Last-Modified value is
  masked: a file on disk carries the mtime of its copy, and so is each
  header `mask_headers` names (a length that follows a masked body).
- `ws`: one websocket client on `url` (ws or wss): the handshake offers
  `protocols`, with `headers` (an Origin, say). Then it sends each text line
  of `send` (and, with `send_big` N, one text frame of N bytes), reads frames
  for at most `ms`, until each line sent has had a reply (a frame that is a
  JSON object with an id), or the server closes. With `"wait_close": true`
  it waits for the server's close; else it sends a close 1000 itself. The
  result is {"status", "headers", "protocol", "frames", "close"}: the
  handshake, the reply frames as JSON (event frames, with no id, are left
  out and counted as "events": true when there were any), and the close
  code and reason the server sent. With `"order_free": true` the reply
  frames are sorted by id, for lines the server may answer in any order. A handshake the server refuses gives
  {"status", "headers", "body"} as `http` does.
"""

from __future__ import annotations

import base64
import codecs
import fcntl
import hashlib
import json
import os
import re
import select
import signal
import socket
import ssl
import struct
import subprocess
import termios
import threading
import time
import unicodedata
from pathlib import Path
from typing import Any

LOCAL_KINDS = (
    "cli",
    "stop",
    "tmux",
    "write",
    "file",
    "ls",
    "log",
    "pty",
    "sleep",
    "http",
    "ws",
    "screen",
)


def is_local(req: Any) -> bool:
    return isinstance(req, dict) and any(k in req for k in LOCAL_KINDS)


def _ok(req: dict[str, Any], result: dict[str, Any]) -> dict[str, Any]:
    return {"id": req.get("id"), "ok": True, "result": result}


class Local:
    """The local requests of one side. bg holds its background runs."""

    def __init__(self, side: Any) -> None:
        self.side = side
        self.bg: dict[str, tuple[subprocess.Popen, Path, Path]] = {}

    # The flags the driver owns.
    OWNED = ("--socket", "--data-dir")

    def argv(self, req: dict[str, Any], args: list[str]) -> list[str]:
        # The words after "--" are the command a new pane runs, which may
        # name its own socket; the driver owns only the run's own flags.
        own = args[: args.index("--")] if "--" in args else args
        for a in own:
            if a in self.OWNED or any(a.startswith(f + "=") for f in self.OWNED):
                raise ValueError(f"a cli case may not pass {a}; the driver names the socket")
        work = Path(self.side.paths["{WORK}"])
        if req.get("alt"):
            sock, data = work / "alt.sock", work / "alt-data"
        else:
            sock, data = Path(self.side.paths["{SOCK}"]), Path(self.side.paths["{DATA}"])
        binary = self.side.peer if req.get("peer") else self.side.binary
        return [binary, "--socket", str(sock), "--data-dir", str(data), *args]

    def env(self, req: dict[str, Any]) -> dict[str, str]:
        env = self.side.server_env()
        # tmux finds its default server through TMUX_TMPDIR, so a run that
        # names no tmux socket still never reaches the owner's tmux.
        tmp = Path(self.side.paths["{WORK}"]) / "tmuxtmp"
        tmp.mkdir(exist_ok=True)
        env["TMUX_TMPDIR"] = str(tmp)
        env.pop("TMUX", None)
        env.pop("TMUX_PANE", None)
        if not req.get("autostart"):
            env["COPPICE_NO_AUTOSTART"] = "1"
        for k, v in (req.get("env") or {}).items():
            if v is None:
                env.pop(k, None)
            else:
                env[k] = v
        return env

    def run(self, req: dict[str, Any]) -> dict[str, Any]:
        if "pty" in req:
            return _ok(req, self.pty(req))
        if "cli" in req and "bg" in req:
            return _ok(req, self.start_bg(req))
        if "cli" in req:
            argv = self.argv(req, req["cli"])
            cwd = req.get("cwd") or self.side.paths["{START}"]
            stdin = req.get("stdin", "")
            p = subprocess.run(
                argv,
                cwd=cwd,
                env=self.env(req),
                input=stdin.encode(),
                capture_output=True,
                timeout=req.get("timeout_ms", 60000) / 1000,
                start_new_session=True,
            )
            return _ok(req, self.outcome(p.returncode, p.stdout, p.stderr))
        if "stop" in req:
            return _ok(req, self.stop_bg(req))
        if "sleep" in req:
            time.sleep(req["sleep"] / 1000)
            return _ok(req, {"slept": True})
        if "tmux" in req:
            return _ok(req, self.tmux(req["tmux"]))
        if "write" in req:
            path = Path(req["write"]).resolve()
            work = Path(self.side.paths["{WORK}"]).resolve()
            if work not in path.parents:
                raise ValueError(f"a write must stay under the work dir: {path}")
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(req["text"], encoding="utf-8")
            return _ok(req, {"written": True})
        if "file" in req:
            path = Path(req["file"])
            if "count" in req:
                try:
                    text = path.read_text(encoding="utf-8", errors="replace")
                except FileNotFoundError:
                    text = ""
                return _ok(req, {"count": text.count(req["count"])})
            try:
                return _ok(
                    req,
                    {
                        "text": path.read_text(encoding="utf-8", errors="replace"),
                        "mode": oct(path.stat().st_mode & 0o777),
                    },
                )
            except FileNotFoundError:
                return _ok(req, {"missing": True})
        if "ls" in req:
            return _ok(req, {"tree": list_tree(Path(req["ls"]))})
        if "http" in req:
            return _ok(req, http_request(req))
        if "ws" in req:
            return _ok(req, ws_session(req))
        if "log" in req:
            return _ok(req, {"lines": self.log_lines(req)})
        if "screen" in req:
            res = self.screen(req)
            if res.get("unsettled"):
                # A checkpoint whose text never came fails on its side, so
                # two sides that both miss it can never agree.
                return {
                    "id": req.get("id"),
                    "ok": False,
                    "error": {"code": "unsettled", "message": json.dumps(res)},
                }
            return _ok(req, res)
        raise ValueError(f"no local request kind in {req}")

    def outcome(self, code: int | None, out: bytes, err: bytes) -> dict[str, Any]:
        return {
            "code": code,
            "stdout": out.decode("utf-8", errors="replace"),
            "stderr": err.decode("utf-8", errors="replace"),
        }

    def start_bg(self, req: dict[str, Any]) -> dict[str, Any]:
        name = req["bg"]
        if name in self.bg:
            raise ValueError(f"a background run named {name} already runs")
        work = Path(self.side.paths["{WORK}"])
        out, err = work / f"bg-{name}.out", work / f"bg-{name}.err"
        with open(out, "wb") as fo, open(err, "wb") as fe:
            p = subprocess.Popen(
                self.argv(req, req["cli"]),
                cwd=req.get("cwd") or self.side.paths["{START}"],
                env=self.env(req),
                stdin=subprocess.DEVNULL,
                stdout=fo,
                stderr=fe,
                start_new_session=True,
            )
        self.bg[name] = (p, out, err)
        return {"started": True}

    def stop_bg(self, req: dict[str, Any]) -> dict[str, Any]:
        p, out, err = self.bg.pop(req["stop"])
        sig = req.get("signal", "TERM")
        if sig != "none" and p.poll() is None:
            p.send_signal(signal.SIGINT if sig == "INT" else signal.SIGTERM)
        try:
            p.wait(timeout=req.get("wait_ms", 10000) / 1000)
        except subprocess.TimeoutExpired:
            os.killpg(p.pid, signal.SIGKILL)
            p.wait(timeout=5)
            return {**self.outcome(None, out.read_bytes(), err.read_bytes()), "killed": True}
        return self.outcome(p.returncode, out.read_bytes(), err.read_bytes())

    def stop_all(self) -> None:
        """Ends every background run and the scratch tmux server."""
        for p, _, _ in self.bg.values():
            if p.poll() is None:
                try:
                    os.killpg(p.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            p.wait(timeout=5)
        self.bg.clear()
        sock = Path(self.side.paths.get("{WORK}", "/nonexistent")) / "tmux.sock"
        if sock.exists():
            self.tmux(["kill-server"])

    def tmux_env(self) -> dict[str, str]:
        env = self.side.server_env()
        tmp = Path(self.side.paths["{WORK}"]) / "tmuxtmp"
        tmp.mkdir(exist_ok=True)
        env["TMUX_TMPDIR"] = str(tmp)
        env.pop("TMUX", None)
        env.pop("TMUX_PANE", None)
        return env

    def tmux(self, args: list[str]) -> dict[str, Any]:
        sock = Path(self.side.paths["{WORK}"]) / "tmux.sock"
        p = subprocess.run(
            ["tmux", "-S", str(sock), *args],
            env=self.tmux_env(),
            stdin=subprocess.DEVNULL,
            capture_output=True,
            timeout=20,
        )
        return self.outcome(p.returncode, p.stdout, p.stderr)

    def log_lines(self, req: dict[str, Any]) -> list[str]:
        text = req["log"]
        out = []
        for line in self.side.log().splitlines():
            hit = line.startswith(text) if req.get("prefix") else text in line
            if hit:
                out.append(re.sub(r"^time=\S+ ", "time={MASKED} ", line))
        return out[: req["first"]] if "first" in req else out

    def screen(self, req: dict[str, Any]) -> dict[str, Any]:
        """The screen of a pane as the side's own server draws it, through
        its libghostty-vt: a view-only attach on a connection with no hello,
        so no name is counted as looking, and the first full frame. It
        waits for `until` when given, then for two frames in a row,
        `settle_ms` apart, that are the same. The result is the rows with
        the trailing blanks cut, the cursor, and the clocks masked
        (CP-R-41)."""
        work = Path(self.side.paths["{WORK}"])
        sock = work / "alt.sock" if req.get("alt") else Path(self.side.paths["{SOCK}"])
        deadline = time.monotonic() + req.get("ms", 15000) / 1000
        settle = req.get("settle_ms", 400) / 1000
        until = req.get("until")
        last: dict[str, Any] | None = None
        while True:
            snap = frame_screen(sock, req["screen"])
            seen = until is None or any(until in r for r in snap["rows"])
            if seen and snap == last:
                return mask_clocks(snap)
            if time.monotonic() >= deadline:
                out = mask_clocks(snap)
                out["unsettled"] = True
                return out
            last = snap if seen else None
            time.sleep(settle if seen else 0.1)

    def pty(self, req: dict[str, Any]) -> dict[str, Any]:
        cols, rows = req.get("cols", 80), req.get("rows", 24)
        master, slave = os.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
        p = subprocess.Popen(
            self.argv(req, req["pty"]),
            cwd=req.get("cwd") or self.side.paths["{START}"],
            env={**self.env(req), "TERM": "xterm-256color"},
            stdin=slave,
            stdout=slave,
            stderr=slave,
            start_new_session=True,
        )
        os.close(slave)
        term = Term(cols, rows)
        lock = threading.Lock()

        def pump() -> None:
            dec = codecs.getincrementaldecoder("utf-8")(errors="replace")
            while True:
                try:
                    r, _, _ = select.select([master], [], [], 0.2)
                    if not r:
                        if p.poll() is not None:
                            return
                        continue
                    b = os.read(master, 65536)
                except OSError:
                    return
                if not b:
                    return
                with lock:
                    term.feed(dec.decode(b))

        reader = threading.Thread(target=pump, daemon=True)
        reader.start()
        timed_out = []
        for step in req.get("steps", []):
            if "send" in step:
                os.write(master, step["send"].encode())
            elif "sleep" in step:
                time.sleep(step["sleep"] / 1000)
            elif "until" in step:
                deadline = time.monotonic() + step.get("ms", 5000) / 1000
                while time.monotonic() < deadline:
                    with lock:
                        if step["until"] in "\n".join(term.text(term.alt_seen or term.main)):
                            break
                    time.sleep(0.05)
                else:
                    timed_out.append(step["until"])
        code: int | None
        try:
            code = p.wait(timeout=req.get("end_ms", 5000) / 1000)
        except subprocess.TimeoutExpired:
            os.killpg(p.pid, signal.SIGKILL)
            p.wait(timeout=5)
            code = None
        reader.join(timeout=3)
        os.close(master)
        with lock:
            out = {
                "code": code,
                "alt": term.text(term.alt_seen) if term.alt_seen else [],
                "main": term.text(term.main),
            }
        if timed_out:
            out["timed_out"] = timed_out
        return out


def frame_screen(sock: Path, pane: str) -> dict[str, Any]:
    """The first full frame of a view-only attach, as plain rows and the
    cursor. The spacer cell after a wide glyph prints nothing."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(5)
        s.connect(str(sock))
        f = s.makefile("rwb")
        f.write(
            json.dumps(
                {"id": "scr", "cmd": "pane.attach", "pane": pane, "view_only": True}
            ).encode()
            + b"\n"
        )
        f.flush()
        for raw in f:
            obj = json.loads(raw)
            if obj.get("id") == "scr" and not obj.get("ok"):
                return {"rows": [], "cursor": None, "error": obj.get("error")}
            if obj.get("event") == "frame" and obj.get("full") and obj.get("pane") == pane:
                break
        else:
            return {"rows": [], "cursor": None, "error": "no frame"}
    rows: list[str] = []
    changed = obj.get("rows_changed") or {}
    for y in range(int(obj.get("rows") or 0)):
        line = ""
        wide = False
        for cell in changed.get(str(y)) or []:
            text = cell[0]
            if wide and text == "":
                wide = False
                continue
            wide = bool(text) and _cell_width(text[0]) == 2
            line += text or " "
        rows.append(line.rstrip())
    return {"rows": rows, "cursor": obj.get("cursor")}


# CP-R-41: the clocks a floor screen shows. A row's age is a field of its
# own between two spaces (now, 3s, 4m, 2h), an ended row says it ago, and a
# held row says how long it waited.
_AGE = re.compile(
    r"(?<=  )(?:now|\d+[smh])(?=  |\s*(?:×|$)| ago)|(?<=foreman · )\d+[smh]|(?<=foreman · )now"
)


def mask_clocks(snap: dict[str, Any]) -> dict[str, Any]:
    """An age takes as many cells as its digits, so the padding after it
    on a row that ends with the close mark is cut to one space."""
    rows = []
    for r in snap["rows"]:
        masked = _AGE.sub("{AGE}", r)
        if masked != r:
            masked = re.sub(r" +×(?=$|[│┃])", " ×", masked)
            # A row cut short by a narrow rail is cut where the age's width
            # puts the cut, so the text cut there is masked too.
            masked = re.sub(r"(\{AGE\}  )[^×│┃]*… ×(?=$|[│┃])", r"\1{CUT} ×", masked)
        rows.append(masked)
    return {**snap, "rows": rows}


def list_tree(root: Path) -> list[list[str]]:
    out = []
    if not root.exists():
        return out
    for p in sorted(root.rglob("*")):
        rel = p.relative_to(root).as_posix()
        kind = "link" if p.is_symlink() else "dir" if p.is_dir() else "file"
        out.append([rel, kind, oct(p.lstat().st_mode & 0o777)])
    return out


def _cell_width(ch: str) -> int:
    return 2 if unicodedata.east_asian_width(ch) in ("W", "F") else 1


class Term:
    """A small terminal model: enough of a VT for what coppice attach
    writes. It keeps a main and an alternate screen, the cursor, a saved
    cursor, and reads cursor moves, erases, the alternate screen switch and
    text. SGR and every other sequence change no cell. alt_seen keeps the
    alternate screen as it was when the program left it."""

    def __init__(self, cols: int, rows: int) -> None:
        self.cols, self.rows = cols, rows
        self.main = self._blank()
        self.alt: list[list[str]] | None = None
        self.alt_seen: list[list[str]] | None = None
        self.x = self.y = 0
        self.saved = (0, 0)
        self.wrap = False
        self.state = ""
        self.buf = ""

    def _blank(self) -> list[list[str]]:
        return [[" "] * self.cols for _ in range(self.rows)]

    @property
    def grid(self) -> list[list[str]]:
        return self.alt if self.alt is not None else self.main

    def text(self, grid: list[list[str]]) -> list[str]:
        return ["".join(c for c in row if c != "").rstrip() for row in grid]

    def feed(self, s: str) -> None:
        for ch in s:
            self._ch(ch)
        if self.alt is not None:
            self.alt_seen = [row[:] for row in self.alt]

    def _ch(self, ch: str) -> None:
        if self.state == "esc":
            self.state = ""
            if ch == "[":
                self.state, self.buf = "csi", ""
            elif ch == "]":
                self.state = "osc"
            elif ch == "7":
                self.saved = (self.x, self.y)
            elif ch == "8":
                self.x, self.y = self.saved
                self.wrap = False
            return
        if self.state == "osc":
            if ch == "\x07":
                self.state = ""
            elif ch == "\x1b":
                self.state = "osc-esc"
            return
        if self.state == "osc-esc":
            self.state = ""
            return
        if self.state == "csi":
            if "\x40" <= ch <= "\x7e":
                self.state = ""
                self._csi(self.buf, ch)
            else:
                self.buf += ch
            return
        if ch == "\x1b":
            self.state = "esc"
        elif ch == "\r":
            self.x, self.wrap = 0, False
        elif ch == "\n":
            self._down()
        elif ch == "\b":
            self.x, self.wrap = max(0, self.x - 1), False
        elif ch < " " or ch == "\x7f":
            pass
        else:
            self._put(ch)

    def _down(self) -> None:
        self.wrap = False
        if self.y == self.rows - 1:
            g = self.grid
            del g[0]
            g.append([" "] * self.cols)
        else:
            self.y += 1

    def _put(self, ch: str) -> None:
        w = _cell_width(ch)
        if self.wrap:
            self.x = 0
            self._down()
        if self.x + w > self.cols:
            self.x = 0
            self._down()
        row = self.grid[self.y]
        row[self.x] = ch
        if w == 2 and self.x + 1 < self.cols:
            row[self.x + 1] = ""
        self.x += w
        if self.x >= self.cols:
            self.x, self.wrap = self.cols - 1, True

    def _csi(self, params: str, final: str) -> None:
        private = params.startswith("?")
        nums = (
            [int(n) if n.isdigit() else 0 for n in params.lstrip("?").split(";")] if params else []
        )

        def n(i: int, default: int) -> int:
            return nums[i] if len(nums) > i and nums[i] else default

        self.wrap = False if final not in "m" else self.wrap
        if private and final in "hl" and 1049 in nums:
            if final == "h" and self.alt is None:
                self.saved = (self.x, self.y)
                self.alt = self._blank()
                self.alt_seen = [row[:] for row in self.alt]
            elif final == "l" and self.alt is not None:
                self.alt = None
                self.x, self.y = self.saved
            return
        if private:
            return
        g = self.grid
        if final in "Hf":
            self.y = min(max(n(0, 1), 1), self.rows) - 1
            self.x = min(max(n(1, 1), 1), self.cols) - 1
        elif final == "A":
            self.y = max(0, self.y - n(0, 1))
        elif final == "B":
            self.y = min(self.rows - 1, self.y + n(0, 1))
        elif final == "C":
            self.x = min(self.cols - 1, self.x + n(0, 1))
        elif final == "D":
            self.x = max(0, self.x - n(0, 1))
        elif final == "G":
            self.x = min(max(n(0, 1), 1), self.cols) - 1
        elif final == "d":
            self.y = min(max(n(0, 1), 1), self.rows) - 1
        elif final == "K":
            mode = n(0, 0)
            row = g[self.y]
            lo, hi = {0: (self.x, self.cols), 1: (0, self.x + 1), 2: (0, self.cols)}.get(
                mode, (0, 0)
            )
            for i in range(lo, min(hi, self.cols)):
                row[i] = " "
        elif final == "J":
            mode = n(0, 0)
            if mode == 2 or mode == 3:
                for r in g:
                    r[:] = [" "] * self.cols
            elif mode == 0:
                for i in range(self.x, self.cols):
                    g[self.y][i] = " "
                for r in g[self.y + 1 :]:
                    r[:] = [" "] * self.cols
            elif mode == 1:
                for r in g[: self.y]:
                    r[:] = [" "] * self.cols
                for i in range(0, self.x + 1):
                    g[self.y][i] = " "


def _connect(url: str, req: dict[str, Any]) -> tuple[socket.socket, str, str]:
    """A connection to url's host, wrapped in TLS for https and wss, and the
    host and the path of the request. A refused connection is tried again
    for wait_ms."""
    scheme, _, rest = url.partition("://")
    host, slash, path = rest.partition("/")
    path = slash + path if slash else "/"
    name, _, port = host.rpartition(":")
    deadline = time.monotonic() + req.get("wait_ms", 0) / 1000
    while True:
        try:
            raw = socket.create_connection((name, int(port)), timeout=10)
            break
        except ConnectionRefusedError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.05)
    raw.settimeout(req.get("ms", 10000) / 1000)
    if scheme in ("https", "wss"):
        ctx = ssl.create_default_context(cafile=req["cafile"])
        raw = ctx.wrap_socket(raw, server_hostname=req.get("sni", name))
    return raw, host, path


def _read_head(f: Any) -> tuple[int, dict[str, Any]]:
    """The status and the headers of a response. A header sent twice keeps
    both values in a list. Date is left out: it is the clock's. The
    version of the status line is kept under the key ":version"."""
    status_line = f.readline().decode("latin-1")
    status = int(status_line.split(" ", 2)[1])
    headers: dict[str, Any] = {":version": status_line.split(" ", 1)[0]}
    while True:
        line = f.readline().decode("latin-1").rstrip("\r\n")
        if not line:
            break
        k, _, v = line.partition(":")
        v = v.strip()
        if k == "Date":
            continue
        if k == "Last-Modified":
            # A file on disk carries its own mtime, which differs by copy.
            v = "{MASKED}"
        if k in headers:
            prev = headers[k]
            headers[k] = [*prev, v] if isinstance(prev, list) else [prev, v]
        else:
            headers[k] = v
    return status, headers


def _read_body(f: Any, headers: dict[str, Any]) -> tuple[bytes, bool]:
    if headers.get("Transfer-Encoding") == "chunked":
        out = b""
        while True:
            size = int(f.readline().split(b";")[0].strip(), 16)
            if size == 0:
                f.readline()
                return out, True
            out += f.read(size)
            f.readline()
    if "Content-Length" in headers:
        return f.read(int(headers["Content-Length"])), False
    return f.read(), False


def http_request(req: dict[str, Any]) -> dict[str, Any]:
    try:
        sock, host, path = _connect(req["url"], req)
    except ConnectionRefusedError:
        if req.get("refused_ok"):
            return {"refused": True}
        raise
    body = req.get("body", "").encode()
    proto = req.get("proto", "HTTP/1.1")
    lines = [f"{req['http']} {path} {proto}", f"Host: {req.get('host', host)}"]
    for k, v in (req.get("headers") or {}).items():
        lines.append(f"{k}: {v}")
    if body or req["http"] in ("POST", "PUT"):
        lines.append(f"Content-Length: {len(body)}")
    lines.append("Connection: close")
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode() + body)
    f = sock.makefile("rb")
    try:
        status, headers = _read_head(f)
        interim = []
        while status == 100:
            interim.append(status)
            status, headers = _read_head(f)
        data, chunked = (b"", False) if req["http"] == "HEAD" else _read_body(f, headers)
    finally:
        f.close()
        sock.close()
    for k in req.get("mask_headers", []):
        if k in headers:
            headers[k] = "{MASKED}"
    out: dict[str, Any] = {"status": status, "headers": headers}
    if interim:
        out["interim"] = interim
    if "same_as" in req and Path(req["same_as"]).read_bytes() == data:
        out["body"] = "{SAME_AS_FILE}"
    else:
        out["body"] = data.decode("utf-8", errors="replace")
    if chunked:
        out["chunked"] = True
    return out


def _ws_frame(opcode: int, payload: bytes) -> bytes:
    """One masked client frame."""
    head = bytes([0x80 | opcode])
    n = len(payload)
    if n < 126:
        head += bytes([0x80 | n])
    elif n < 1 << 16:
        head += bytes([0x80 | 126]) + struct.pack(">H", n)
    else:
        head += bytes([0x80 | 127]) + struct.pack(">Q", n)
    key = os.urandom(4)
    return head + key + bytes(b ^ key[i % 4] for i, b in enumerate(payload))


def _ws_read(f: Any) -> tuple[int, bytes]:
    """One frame from the server, or (-1, b"") at the end of the stream."""
    b = f.read(2)
    if len(b) < 2:
        return -1, b""
    opcode, n = b[0] & 0x0F, b[1] & 0x7F
    if n == 126:
        n = struct.unpack(">H", f.read(2))[0]
    elif n == 127:
        n = struct.unpack(">Q", f.read(8))[0]
    if b[1] & 0x80:
        key = f.read(4)
        data = bytes(x ^ key[i % 4] for i, x in enumerate(f.read(n)))
    else:
        data = f.read(n)
    return opcode, data


def ws_session(req: dict[str, Any]) -> dict[str, Any]:
    sock, host, path = _connect(req["ws"], req)
    key = base64.b64encode(os.urandom(16)).decode()
    head = {
        "Host": host,
        "Upgrade": "websocket",
        "Connection": "Upgrade",
        "Sec-WebSocket-Key": key,
        "Sec-WebSocket-Version": "13",
    }
    if req.get("protocols"):
        head["Sec-WebSocket-Protocol"] = ", ".join(req["protocols"])
    # A header the case names replaces the one the client would send.
    head.update(req.get("headers") or {})
    lines = [f"GET {path} HTTP/1.1"] + [f"{k}: {v}" for k, v in head.items()]
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
    f = sock.makefile("rb")
    try:
        status, headers = _read_head(f)
        if status != 101:
            data, _ = _read_body(f, headers)
            return {"status": status, "headers": headers, "body": data.decode("utf-8", "replace")}
        accept = base64.b64encode(
            hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
        ).decode()
        out: dict[str, Any] = {
            "status": status,
            "headers": {k: v for k, v in headers.items() if k.lower() != "sec-websocket-accept"},
            "accept_ok": any(
                k.lower() == "sec-websocket-accept" and v == accept for k, v in headers.items()
            ),
            "frames": [],
            "close": None,
        }
        sent = req.get("send", [])
        for line in sent:
            sock.sendall(_ws_frame(1, line.encode()))
        if "send_big" in req:
            sock.sendall(_ws_frame(1, b"a" * req["send_big"]))
        sock.settimeout(req.get("ms", 5000) / 1000)
        want = len(sent)
        try:
            while True:
                if out["close"] is None and not req.get("wait_close") and want <= 0:
                    sock.sendall(_ws_frame(8, struct.pack(">H", 1000)))
                    want = -(1 << 30)
                opcode, data = _ws_read(f)
                if opcode == -1:
                    break
                if opcode == 8:
                    code = struct.unpack(">H", data[:2])[0] if len(data) >= 2 else None
                    out["close"] = {"code": code, "reason": data[2:].decode("utf-8", "replace")}
                    if want > -(1 << 30):
                        sock.sendall(_ws_frame(8, data[:2]))
                    break
                if opcode == 9:
                    sock.sendall(_ws_frame(10, data))
                    continue
                if opcode != 1:
                    continue
                try:
                    obj = json.loads(data)
                except ValueError:
                    out["frames"].append(data.decode("utf-8", "replace"))
                    want -= 1
                    continue
                if isinstance(obj, dict) and "id" in obj:
                    out["frames"].append(obj)
                    want -= 1
                else:
                    out["events"] = True
        except (TimeoutError, OSError):
            out["timed_out"] = True
        if req.get("order_free"):
            # The server may answer the lines of one connection in any
            # order, so a case that sends several may ask for the replies
            # by id.
            out["frames"].sort(key=lambda x: str(x.get("id")) if isinstance(x, dict) else "")
        return out
    finally:
        f.close()
        sock.close()

"""Check harness/coppice/internal/adapters/opencode/PINS.md against a real
`opencode serve`. Not a test: it prints, it does not assert.

The script starts `opencode serve` once on a loopback port. HOME and every
XDG home point at directories under the scratch directory you name, so the
server never reads or writes your real OpenCode config, data or cache.
Autoupdate is off. The script sends no prompt, so no model runs. It reads
the version from /global/health and the tool ids from
/experimental/tool/ids. At start the server installs @opencode-ai/plugin
into its own config directory. The script reads the Hooks interface keys
from that copy. Then it stops the server and prints each fact that PINS.md
does not name.

Usage: uv run --no-sync python scripts/opencode_discover.py --scratch DIR

DIR must be on real disk. Delete it when you are done: the server writes
about 150 MB of node_modules there.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

PINS_PATH = (
    Path(__file__).resolve().parents[1]
    / "harness"
    / "coppice"
    / "internal"
    / "adapters"
    / "opencode"
    / "PINS.md"
)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _get(url: str, password: str) -> tuple[int, bytes]:
    req = urllib.request.Request(url)
    token = base64.b64encode(f"opencode:{password}".encode()).decode()
    req.add_header("Authorization", f"Basic {token}")
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, b""
    except OSError:
        return 0, b""


def _isolated_env(scratch: Path, password: str) -> dict[str, str]:
    dirs = {name: scratch / name for name in ("home", "config", "data", "cache", "state")}
    for d in dirs.values():
        d.mkdir(parents=True, exist_ok=True)
    return {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "HOME": str(dirs["home"]),
        "XDG_CONFIG_HOME": str(dirs["config"]),
        "XDG_DATA_HOME": str(dirs["data"]),
        "XDG_CACHE_HOME": str(dirs["cache"]),
        "XDG_STATE_HOME": str(dirs["state"]),
        "OPENCODE_DISABLE_AUTOUPDATE": "1",
        "OPENCODE_SERVER_PASSWORD": password,
    }


def discover(scratch: Path) -> dict | None:
    exe = shutil.which("opencode")
    if exe is None:
        print("opencode is not on PATH. Install it, then run this script again.")
        return None
    password = secrets.token_hex(8)
    env = _isolated_env(scratch, password)
    work = scratch / "work"
    work.mkdir(parents=True, exist_ok=True)
    port = _free_port()
    base = f"http://127.0.0.1:{port}"
    log = (scratch / "serve.log").open("wb")
    proc = subprocess.Popen(
        [exe, "serve", "--hostname", "127.0.0.1", "--port", str(port)],
        cwd=work,
        env=env,
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    try:
        deadline = time.monotonic() + 20
        status, body = 0, b""
        while time.monotonic() < deadline:
            status, body = _get(f"{base}/global/health", password)
            if status == 200:
                break
            time.sleep(0.5)
        if status != 200:
            print(f"opencode serve did not answer /global/health (last status {status}).")
            return None
        version = json.loads(body).get("version", "unknown")
        _, raw_ids = _get(f"{base}/experimental/tool/ids", password)
        tool_ids = json.loads(raw_ids) if raw_ids else []
        dts = Path(env["XDG_CONFIG_HOME"]) / "opencode" / "node_modules"
        dts = dts / "@opencode-ai" / "plugin" / "dist" / "index.d.ts"
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline and not dts.exists():
            time.sleep(1)
        hook_keys: list[str] = []
        if dts.exists():
            text = dts.read_text(encoding="utf-8")
            start = text.find("export interface Hooks {")
            body_text = text[start:] if start >= 0 else ""
            depth, end = 0, len(body_text)
            for i, ch in enumerate(body_text):
                if ch == "{":
                    depth += 1
                elif ch == "}":
                    depth -= 1
                    if depth == 0:
                        end = i
                        break
            top = []
            depth = 0
            for line in body_text[:end].splitlines()[1:]:
                if depth == 0:
                    m = re.match(r'^\s*(?:"([\w.]+)"|(\w+))\?:', line)
                    if m:
                        top.append(m.group(1) or m.group(2))
                depth += line.count("{") + line.count("(") - line.count("}") - line.count(")")
            hook_keys = top
        else:
            print("the server did not install @opencode-ai/plugin in 60 s. No hook keys read.")
        return {"version": version, "tool_ids": tool_ids, "hook_keys": hook_keys}
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--scratch", type=Path, required=True, help="An empty directory on real disk.")
    args = ap.parse_args()
    scratch = args.scratch.resolve()
    if str(scratch).startswith("/tmp"):
        print("choose a scratch directory on real disk, not /tmp.")
        return 1
    facts = discover(scratch)
    if facts is None:
        return 1
    pins = PINS_PATH.read_text(encoding="utf-8") if PINS_PATH.exists() else ""
    print(f"opencode version: {facts['version']}")
    print(f"tool ids: {', '.join(facts['tool_ids'])}")
    print(f"plugin hook keys: {', '.join(facts['hook_keys'])}")
    missing = [
        f
        for f in [facts["version"], *facts["tool_ids"], *facts["hook_keys"]]
        if f"`{f}`" not in pins
    ]
    if missing:
        print("PINS.md does not name: " + ", ".join(missing))
    else:
        print("PINS.md names every fact above.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

"""Golden cases for the fence's rule list, from the Python oracle.

    uv run --no-sync python clients/fence_cases.py [--out clients/fixtures/fence]

Each case is the input of ``opendaisugi.fence.plan`` and what it answers.
The Go and Rust ``daisugi fence`` must print the same list for the same
input (``clients/fence_compare.py``; each port also reads this file in its
own tests). Every path is fake. The file is content-addressed like the
other case files: each ``id`` is the first 16 hex digits of the SHA-256 of
the case's canonical JSON without the id, lines are sorted by id, and a
manifest pins the file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "src"))

from opendaisugi.fence import plan  # noqa: E402

FIXTURE_DIR = HERE / "fixtures" / "fence"

HOME = "/home/user"
WS = "/home/user/proj"
TMP = "/home/user/.local/share/opendaisugi/fence-tmp/4242"


def base(**kw: Any) -> dict[str, Any]:
    inp: dict[str, Any] = {
        "harness": "claude",
        "workspace": WS,
        "home": HOME,
        "other_homes": [],
        "tmpdir": TMP,
        "env": {},
        "path_dirs": ["/usr/bin", "/home/user/.cargo/bin", "/home/user/.local/bin"],
        "exe_dirs": [],
        "gate_root": "",
        "envelope": {"file_read": [], "file_write": []},
        "allow": [],
        "workdirs": [],
        "sockets": [],
        "real": {},
        "never_extra": [],
        "state_extra": [],
    }
    inp.update(kw)
    return inp


CASES: list[tuple[str, dict[str, Any]]] = [
    ("claude default", base()),
    ("codex", base(harness="codex")),
    ("pi", base(harness="pi")),
    ("opencode", base(harness="opencode")),
    ("sprig", base(harness="sprig")),
    ("unknown harness", base(harness="")),
    ("tmpdir elsewhere", base(tmpdir="/srv/m/tera/.tt/tmp")),
    ("workspace is home", base(workspace=HOME)),
    ("workspace is ssh", base(workspace=HOME + "/.ssh")),
    ("workspace above home", base(workspace="/home")),
    ("workspace in gate state", base(workspace=HOME + "/.opendaisugi/gate/x")),
    ("workspace is root", base(workspace="/")),
    ("path dir is home", base(path_dirs=["/usr/bin", HOME])),
    ("path dir in ssh", base(path_dirs=[HOME + "/.ssh/bin", "/usr/bin"])),
    ("path dir relative ignored", base(path_dirs=["bin", "/usr/bin"])),
    ("exe dir", base(exe_dirs=[HOME + "/.local/share/claude/versions"])),
    ("claude json opened by allow", base(allow=["~/.claude.json:rw"])),
    ("opencode config opened by allow", base(harness="opencode", allow=["~/.config/opencode:rw"])),
    ("allow ro", base(allow=["/srv/m/data/ref"])),
    ("allow rw", base(allow=["/srv/m/data/out:rw"])),
    ("allow explicit ro", base(allow=["/srv/m/data/ref:ro"])),
    ("allow tilde", base(allow=["~/notes:rw"])),
    ("allow relative", base(allow=["../sibling"])),
    ("allow names ssh", base(allow=["~/.ssh"])),
    ("allow inside ssh", base(allow=["~/.ssh/config"])),
    ("allow home", base(allow=["~"])),
    ("allow gate state rw", base(allow=["~/.opendaisugi/gate:rw"])),
    ("allow gate state ro", base(allow=["~/.opendaisugi/gate"])),
    ("allow data dir rw", base(allow=["~/.opendaisugi:rw"])),
    ("allow coppice dir", base(allow=["~/.config/coppice"])),
    ("env data dirs", base(env={"OPENDAISUGI_HOME": "/srv/od", "XDG_DATA_HOME": "/home/user/xdg"})),
    ("env data dir tilde", base(env={"OPENDAISUGI_HOME": "~/od"})),
    ("env data dir relative", base(env={"OPENDAISUGI_HOME": "od"})),
    ("coppice data dir", base(env={"COPPICE_DATA_DIR": "/srv/m/c"}, allow=["/srv/m/c:rw"])),
    ("gate root", base(gate_root="/srv/gate")),
    ("workspace in gate root", base(gate_root="/srv/gate", workspace="/srv/gate/w")),
    (
        "envelope literal",
        base(envelope={"file_read": ["/srv/m/ref/**"], "file_write": ["out/**", "/srv/m/out/*.txt"]}),
    ),
    (
        "envelope inside workspace",
        base(envelope={"file_read": ["src/**/*.py"], "file_write": ["./build/**", "**"]}),
    ),
    (
        "envelope too wide",
        base(envelope={"file_read": ["/**", "~/**", "/home/**", "~"], "file_write": ["/*"]}),
    ),
    (
        "envelope secret",
        base(envelope={"file_read": ["/home/user/.ssh/**", "~/.aws"], "file_write": ["~/.gnupg/*"]}),
    ),
    (
        "envelope above secret",
        base(envelope={"file_read": ["/home/user/.config/**"], "file_write": []}),
    ),
    (
        "envelope gate state",
        base(
            envelope={"file_read": [], "file_write": ["~/.opendaisugi/**", "~/.opendaisugi/gate/x"]}
        ),
    ),
    (
        "envelope plain file and brace",
        base(envelope={"file_read": ["/etc/hosts", "/srv/m/{a,b}/x"], "file_write": ["/srv/m/w?/f"]}),
    ),
    ("state extra", base(state_extra=["/srv/m/x/od"], workspace="/srv/m/x/od/w")),
    ("never extra", base(never_extra=["/srv/m/real/ssh"], allow=["/srv/m/real"])),
    ("home with slashes", base(home="/home/user/", workspace="/home/user//proj/./x/..")),
    ("tmp under home", base(tmpdir=HOME + "/tmp")),
    ("tmp is root", base(tmpdir="/")),
    ("claude dir as allow", base(allow=["~/.claude:rw"])),
    # FN-V-3: only --allow opens a secret.
    ("workspace inside ssh", base(workspace=HOME + "/.ssh/sub")),
    ("workspace is keyrings", base(workspace=HOME + "/.local/share/keyrings")),
    ("workspace is coppice data", base(workspace=HOME + "/.local/share/coppice")),
    ("tmpdir is ssh", base(tmpdir=HOME + "/.ssh")),
    ("tmpdir is tmp", base(tmpdir="/tmp")),
    ("tmpdir empty", base(tmpdir="")),
    ("tmpdir allow tmp rw", base(tmpdir="", allow=["/tmp:rw"])),
    ("tmpdir coppice pane", base(tmpdir=HOME + "/.local/share/coppice/fence-tmp/w1:p1")),
    (
        "tmpdir coppice data dir env",
        base(tmpdir="/srv/m/c/fence-tmp/p2", env={"COPPICE_DATA_DIR": "/srv/m/c"}),
    ),
    ("tmpdir fence-tmp too deep", base(tmpdir=HOME + "/.local/share/coppice/fence-tmp/a/b")),
    ("tmpdir fence-tmp itself", base(tmpdir=HOME + "/.opendaisugi/fence-tmp")),
    ("tmpdir xdg data", base(tmpdir="/x/d/opendaisugi/fence-tmp/9", env={"XDG_DATA_HOME": "/x/d"})),
    ("workspace symlink to ssh", base(workspace=HOME + "/.ssh", real={WS: HOME + "/.ssh"})),
    ("harness path links to ssh", base(real={HOME + "/.config/claude": HOME + "/.ssh"})),
    ("harness path links outside", base(real={HOME + "/.cache/claude": "/srv/m/cache/claude"})),
    (
        "envelope prefix links to ssh",
        base(
            envelope={"file_read": ["/srv/m/out/**"], "file_write": []},
            real={"/srv/m/out": HOME + "/.ssh"},
        ),
    ),
    (
        "envelope inside secret",
        base(envelope={"file_read": ["~/.ssh/**"], "file_write": ["~/.aws/x/*"]}),
    ),
    ("workdir ok", base(workdirs=["/srv/m/sess"])),
    ("workdir secret", base(workdirs=["~/.aws"])),
    ("workdir gate state", base(workdirs=["~/.opendaisugi/sessions"])),
    (
        "workspace inside allowed coppice dir",
        base(
            workspace=HOME + "/.local/share/coppice/foreman/f1",
            allow=[HOME + "/.local/share/coppice/foreman/f1:rw"],
        ),
    ),
    (
        "workspace inside ro allow stays refused",
        base(workspace=HOME + "/.local/share/coppice/f", allow=[HOME + "/.local/share/coppice/f"]),
    ),
    ("workdir inside allowed secret", base(workdirs=[HOME + "/.ssh/w"], allow=["~/.ssh:rw"])),
    ("workspace under refused allow", base(workspace=HOME + "/x", allow=[HOME + ":rw"])),
    # FN-V-11
    ("new secrets", base(path_dirs=[HOME + "/.mozilla/bin", HOME + "/.ollama", "/usr/bin"])),
    ("workspace above chrome", base(workspace=HOME + "/.config")),
    # FN-V-1: the password entry's home keeps its secrets.
    ("other home", base(home="/elsewhere", workspace="/elsewhere/w", other_homes=[HOME])),
    ("other home workspace", base(home="/elsewhere", workspace=HOME, other_homes=[HOME])),
    ("other home same", base(other_homes=[HOME, "rel"])),
    # FN-V-2: sockets.
    ("coppice socket", base(sockets=["/srv/m/c/run/server.sock"])),
    (
        "coppice socket runtime",
        base(
            sockets=["/run/user/1000/coppice/server.sock"],
            env={"XDG_RUNTIME_DIR": "/run/user/1000"},
        ),
    ),
    (
        "socket runtime dir",
        base(sockets=["/run/user/1000/bus"], env={"XDG_RUNTIME_DIR": "/run/user/1000"}),
    ),
    ("socket tmp", base(sockets=["/tmp/x.sock"])),
    ("socket home", base(sockets=[HOME + "/s.sock"])),
    ("socket ssh", base(sockets=[HOME + "/.ssh/cm-host"])),
    ("socket in coppice data", base(sockets=[HOME + "/.local/share/coppice/server.sock"])),
    ("socket in workspace", base(sockets=[WS + "/server.sock"])),
    ("socket relative", base(sockets=["server.sock"])),
    # FN-V-6: harness config read only.
    ("claude config read only", base(harness="claude", allow=["~/.claude/projects:rw"])),
    # FN-R-31: only this workspace's Claude project dir is writable; a name
    # the fence cannot know leaves the whole projects dir writable.
    (
        "claude memory allow",
        base(harness="claude", allow=["~/.claude/projects/-elsewhere/memory:rw"]),
    ),
    ("claude project name odd chars", base(harness="claude", workspace=HOME + "/a_b.c d")),
    ("claude project name too long", base(harness="claude", workspace="/w/" + "x" * 200)),
    ("claude project name not ascii", base(harness="claude", workspace="/w/caf\u00e9")),
    (
        "claude project dir resolved",
        base(harness="claude", real={HOME + "/.claude/projects": "/data/cp"}),
    ),
    (
        "opencode snapshot allow",
        base(harness="opencode", allow=["~/.local/share/opencode/snapshot:rw"]),
    ),
]


def render() -> tuple[str, int]:
    rows = []
    for name, inp in CASES:
        body = {"name": name, "input": inp, "expect": plan(inp)}
        canon = json.dumps(body, sort_keys=True, separators=(",", ":"))
        rows.append({"id": hashlib.sha256(canon.encode()).hexdigest()[:16], **body})
    rows.sort(key=lambda r: r["id"])
    return "".join(json.dumps(r, sort_keys=True) + "\n" for r in rows), len(rows)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=FIXTURE_DIR)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    body, n = render()
    (args.out / "cases.jsonl").write_text(body, encoding="utf-8")
    manifest = {"cases.jsonl": hashlib.sha256(body.encode()).hexdigest(), "n": n}
    (args.out / "manifest.json").write_text(json.dumps(manifest, indent=1, sort_keys=True) + "\n")
    print(f"wrote {n} cases to {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

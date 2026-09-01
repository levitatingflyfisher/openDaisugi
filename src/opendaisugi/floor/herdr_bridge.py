"""The Herdr bridge: Herdr shows a coppice pane by running `coppice attach`.

A Herdr user runs `coppice --socket <socket> attach <pane>` in one of
Herdr's panes, through the pinned `pane run` verb in `herdr_verbs.json`.
Keys reach the coppice pane, and the last screen row is the attach status
line, which names the pane's state and its source.

The bridge writes one detection manifest, `agent-detection/coppice.toml`
under the Herdr config directory. Its rules read the attach status line.
The format is the Herdr manifest format that `manifest_schema` records.

Two facts are not verified, since no box that built this has Herdr on it:

* Herdr picks a manifest by the foreground process of a pane, and its
  documentation says a new agent needs a Herdr binary update for process
  detection. A local override only patches an agent Herdr already knows.
  So Herdr may never apply the `coppice` manifest. The attach status line
  still shows the state to a person who looks at the pane.
* `herdr pane run <pane> <command>` is the pinned verb, marked
  `verified: false` in `herdr_verbs.json`.

The rules read screen text, so they are a guess. attach makes each run of
spaces in a label one space, so a label cannot push the real state word out
of reach. A label that reads like a state, such as `blocked via gate`, can
still fake that state in Herdr's read. An agent can set a label.
"""

from __future__ import annotations

import os
import shlex
from pathlib import Path
from typing import Any

AGENT_ID = "coppice"
MANIFEST_FILE = Path("agent-detection") / f"{AGENT_ID}.toml"

# The attach status line is the pane id, then up to two fields, the label
# and the harness, then "<state> via <source>". Fields are joined by two
# spaces, and a field holds single spaces only. The state word must come
# right after those fields, so an ask summary later on the line that says
# "blocked via gate" never matches.
_FIELDS = r"^\S+  (?:[^ ]+(?: [^ ]+)*  ){0,2}"
_REGION = "bottom_non_empty_lines(1)"

# The first line of every manifest the bridge writes. install replaces a
# file that starts with it, and refuses any other.
_HEADER = "# Herdr detection rules for a pane that runs coppice attach."

_NOTE = (
    "not verified: Herdr picks a manifest by the pane's foreground process, "
    "and a new agent id may need a Herdr update before this manifest applies."
)


def _rule(state: str, priority: int) -> dict[str, Any]:
    return {
        "id": f"coppice-attach-{state}",
        "state": state,
        "priority": priority,
        "region": _REGION,
        "line_regex": [_FIELDS + state + r" via [a-z]+"],
    }


def manifest() -> dict[str, Any]:
    """The detection manifest as a dict. done and unknown have no rule, so
    Herdr reads them as unknown. Herdr has no done state."""
    return {
        "id": AGENT_ID,
        "rules": [
            _rule("blocked", 30),
            _rule("working", 20),
            _rule("idle", 10),
        ],
    }


def _toml_string(s: str) -> str:
    out = ['"']
    for ch in s:
        if ch == "\\":
            out.append("\\\\")
        elif ch == '"':
            out.append('\\"')
        elif ord(ch) < 0x20 or ord(ch) == 0x7F:
            out.append(f"\\u{ord(ch):04x}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _toml_value(v: Any) -> str:
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, int):
        return str(v)
    if isinstance(v, str):
        return _toml_string(v)
    if isinstance(v, list):
        return "[" + ", ".join(_toml_value(x) for x in v) + "]"
    raise TypeError(f"no TOML form for {type(v).__name__}")


def manifest_toml() -> str:
    """The manifest as TOML text, in a fixed order."""
    m = manifest()
    lines = [
        _HEADER,
        "# They read the attach status line on the last row.",
        f"# {_NOTE}",
        f"id = {_toml_value(m['id'])}",
    ]
    for rule in m["rules"]:
        lines.append("")
        lines.append("[[rules]]")
        for key, value in rule.items():
            lines.append(f"{key} = {_toml_value(value)}")
    return "\n".join(lines) + "\n"


def attach_argv(pane: str, socket: str | os.PathLike[str], coppice: str = "coppice") -> list[str]:
    """The command Herdr runs. The global --socket flag comes before attach,
    since attach takes no flag of its own."""
    return [coppice, "--socket", str(socket), "attach", pane]


def agent_definition(
    pane: str, socket: str | os.PathLike[str], coppice: str = "coppice"
) -> dict[str, Any]:
    """What a Herdr user needs to show one coppice pane: the command, the
    pinned Herdr line that runs it, and the manifest with its path under
    the Herdr config directory."""
    argv = attach_argv(pane, socket, coppice)
    return {
        "agent": AGENT_ID,
        "pane": pane,
        "command": argv,
        "herdr_run": ["herdr", "pane", "run", "<herdr-pane>", shlex.join(argv)],
        "manifest": manifest(),
        "manifest_file": MANIFEST_FILE.as_posix(),
        "verified": False,
        "note": _NOTE,
    }


def default_config_dir(env: dict[str, str] | None = None) -> Path:
    """`$XDG_CONFIG_HOME/herdr`, else `~/.config/herdr`."""
    env = dict(os.environ) if env is None else env
    base = env.get("XDG_CONFIG_HOME")
    return (Path(base) if base else Path.home() / ".config") / "herdr"


def install(herdr_config_dir: str | os.PathLike[str]) -> Path:
    """Write the manifest to `agent-detection/coppice.toml` and return its
    path. It writes that one file and no other. The same file already there
    is left as it is. An older manifest the bridge wrote, which starts with
    its header line, is replaced. Any other file, or a link, is refused."""
    path = Path(herdr_config_dir) / MANIFEST_FILE
    text = manifest_toml()
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise FileExistsError(
            f"{path} is a link or not a file. Move it away, then run the bridge again."
        )
    if path.exists():
        old = path.read_text(encoding="utf-8", errors="replace")
        if old == text:
            return path
        if old.split("\n", 1)[0] != _HEADER:
            raise FileExistsError(
                f"{path} is there and the coppice bridge did not write it. "
                "Move it away, then run the bridge again."
            )
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.{os.getpid()}.new")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
        os.replace(tmp, path)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    return path

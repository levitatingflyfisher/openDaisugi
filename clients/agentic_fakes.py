"""The fakes of an agentic step's sub-agent, for the weave and K2 suites.

Importing this module patches garden_cases, as k2_cases and weave_cases
patch it: the fake `claude` becomes AGENTIC_CLAUDE, a reply spec may be
``agentic``, ``sprig`` or ``sprig_raw``, and a case's ``sprig`` entry
puts a sprig on PATH:

- ``"sprig": "fake"``: FAKE_SPRIG as ``sprig`` on the case's PATH;
- ``"sprig": "real"``: the real sprig binary given with ``--sprig BIN``
  (set_real_sprig), named to daisugi by DAISUGI_SPRIG. It talks to the
  fake `claude`, so no model runs.

Both suites import it, so a K2 case and a weave case run the same fakes.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import garden_cases  # noqa: E402 - sibling module, run as a script

# The fake `claude` of the agentic suites. A call without --settings is the
# garden suite's fake. A call with --settings is an agentic step's
# sub-agent: its key leaves out the settings value (it names a fresh
# gate root and the gate's own program, which differ run to run and side
# to side), its log entry holds the settings with the program and the root
# written as placeholders, the answer's tool calls are run through the
# settings' PreToolUse command as the host runs a hook, and the log entry
# holds each verdict and the envelopes registered in the gate root.
# The key reads the prompt with the case's HOME and any agentic gate root
# masked: a real sprig puts tool results that can name them in its prompt.
AGENTIC_CLAUDE = r"""#!{PYTHON}
import hashlib, json, os, re, shlex, subprocess, sys, time
argv = sys.argv[1:]
data = sys.stdin.buffer.read()
entry = {"kind": "claude"}
key_argv = argv
if "--settings" in argv:
    i = argv.index("--settings")
    settings = json.loads(argv[i + 1])
    key_argv = argv[: i + 1] + ["<SETTINGS>"] + argv[i + 2 :]
    hook = settings["hooks"]["PreToolUse"][0]["hooks"][0]
    command = hook["command"]
    words = shlex.split(command[command.index(" --mode ") :])
    root = words[words.index("--root") + 1]
    norm = lambda s: s.replace(root, "{GATEROOT}")
    shown = json.loads(json.dumps(settings))
    h = shown["hooks"]["PreToolUse"][0]["hooks"][0]
    h["command"] = "<GATE>" + norm(command[command.index(" --mode ") :])
    entry["settings"] = shown
    entry["root_mode"] = oct(os.stat(root).st_mode & 0o777)
    entry["cwd"] = os.getcwd()
kdata = data
if os.environ.get("HOME"):
    kdata = kdata.replace(os.environ["HOME"].encode(), b"{HOME}")
kdata = re.sub(rb"daisugi-agentic-gate-[A-Za-z0-9_]+", b"daisugi-agentic-gate-{X}", kdata)
key = hashlib.sha256(json.dumps(key_argv).encode() + b"\0" + kdata).hexdigest()
entry.update({"argv": key_argv, "stdin": data.decode("utf-8", "surrogateescape"), "key": key})
table = json.load(open(os.environ["FAKE_MODEL_TABLE"], encoding="utf-8"))
ans = table.get(key)
if ans is not None and "--settings" in argv:
    verdicts = []
    for call in ans.get("calls", []):
        payload = {
            "session_id": "sub-agent-1",
            "transcript_path": "",
            "cwd": os.getcwd(),
            "hook_event_name": "PreToolUse",
            "tool_name": call["tool_name"],
            "tool_input": call["tool_input"],
        }
        p = subprocess.run(["/bin/sh", "-c", command], input=json.dumps(payload).encode(),
                           capture_output=True, timeout=120)
        verdicts.append({"exit": p.returncode,
                         "stdout": norm(p.stdout.decode("utf-8", "replace")),
                         "stderr": norm(p.stderr.decode("utf-8", "replace"))})
    entry["verdicts"] = verdicts
    registered = {}
    d = os.path.join(root, "envelopes")
    if os.path.isdir(d):
        entry["envelopes_mode"] = oct(os.stat(d).st_mode & 0o777)
        for name in sorted(os.listdir(d)):
            f = os.path.join(d, name)
            registered[name] = {"mode": oct(os.stat(f).st_mode & 0o777),
                                "text": open(f, encoding="utf-8").read()}
    entry["registered"] = registered
with open(os.environ["FAKE_MODEL_LOG"], "a", encoding="utf-8") as f:
    f.write(json.dumps(entry) + "\n")
if ans is None:
    sys.stderr.write("fake claude: no recorded answer for " + key + "\n")
    sys.exit(97)
if ans.get("sleep"):
    time.sleep(ans["sleep"])
sys.stdout.write(ans.get("stdout", ""))
sys.stderr.write(ans.get("stderr", ""))
sys.exit(ans.get("exit", 0))
"""

# The fake `sprig` (kind A). Its log entry holds the argv with the gate
# program written <GATE> and the gate root {GATEROOT}, the cwd, the sprig
# variables of its environment, the prompt on stdin, and the envelopes
# registered in the gate root. Each scripted tool call is sent to the
# --gate-cmd words as sprig sends it (sprig's tool name mapped to the
# host's, the input, the cwd, and the --session id unless the call names
# another), and the log entry holds each verdict. Then it prints the
# scripted --json reply and exits with the scripted code.
FAKE_SPRIG = r"""#!{PYTHON}
import hashlib, json, os, subprocess, sys, time
argv = sys.argv[1:]
data = sys.stdin.buffer.read()
entry = {"kind": "sprig"}
words, root = [], None
shown = list(argv)
if "--gate-cmd" in argv:
    i = argv.index("--gate-cmd")
    words = argv[i + 1].split()
    if "--root" in words:
        root = words[words.index("--root") + 1]
    m = words.index("--mode") if "--mode" in words else len(words)
    shown[i + 1] = " ".join(["<GATE>"] + words[m:])
norm = (lambda s: s.replace(root, "{GATEROOT}")) if root else (lambda s: s)
shown = [norm(a) for a in shown]
entry["argv"] = shown
entry["cwd"] = os.getcwd()
entry["env"] = {k: os.environ[k] for k in ("SPRIG_BACKEND", "SPRIG_MODEL") if k in os.environ}
if root:
    entry["root_mode"] = oct(os.stat(root).st_mode & 0o777)
key = hashlib.sha256(json.dumps(shown).encode() + b"\0" + data).hexdigest()
entry.update({"stdin": data.decode("utf-8", "surrogateescape"), "key": key})
table = json.load(open(os.environ["FAKE_MODEL_TABLE"], encoding="utf-8"))
ans = table.get(key)
if ans is not None and words:
    session = argv[argv.index("--session") + 1] if "--session" in argv else None
    names = {"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash"}
    verdicts = []
    for call in ans.get("calls", []):
        payload = {"tool_name": names.get(call["tool"], call["tool"]),
                   "tool_input": call["input"], "cwd": os.getcwd()}
        sid = call.get("session_id", session)
        if sid is not None:
            payload["session_id"] = sid
        p = subprocess.run(words, input=json.dumps(payload, sort_keys=True).encode(),
                           capture_output=True, timeout=120)
        verdicts.append({"exit": p.returncode,
                         "stdout": norm(p.stdout.decode("utf-8", "replace")),
                         "stderr": norm(p.stderr.decode("utf-8", "replace"))})
    entry["verdicts"] = verdicts
if root:
    registered = {}
    d = os.path.join(root, "envelopes")
    if os.path.isdir(d):
        entry["envelopes_mode"] = oct(os.stat(d).st_mode & 0o777)
        for name in sorted(os.listdir(d)):
            f = os.path.join(d, name)
            registered[name] = {"mode": oct(os.stat(f).st_mode & 0o777),
                                "text": open(f, encoding="utf-8").read()}
    entry["registered"] = registered
with open(os.environ["FAKE_MODEL_LOG"], "a", encoding="utf-8") as f:
    f.write(json.dumps(entry) + "\n")
if ans is None:
    sys.stderr.write("fake sprig: no recorded answer for " + key + "\n")
    sys.exit(97)
if ans.get("sleep"):
    time.sleep(ans["sleep"])
sys.stdout.write(ans.get("stdout", ""))
sys.stderr.write(ans.get("stderr", ""))
sys.exit(ans.get("exit", 0))
"""

garden_cases.FAKE_CLAUDE = AGENTIC_CLAUDE
_generic_make_answer = garden_cases.make_answer


def make_answer(spec: dict[str, Any]) -> tuple[str, dict[str, Any]]:
    """garden_cases.make_answer, and three more replies: ``agentic`` (the
    claude sub-agent's JSON result, with the tool calls it makes first),
    ``sprig`` (the --json reply of the fake sprig, a dict, with its tool
    calls) and ``sprig_raw`` (the fake sprig's stdout as it is)."""
    if "agentic" in spec:
        ans = {
            "stdout": garden_cases.claude_envelope(
                spec["agentic"], is_error=spec.get("is_error", False)
            )
        }
        ans["calls"] = spec.get("calls", [])
        return "claude", ans
    if "sprig" in spec or "sprig_raw" in spec:
        import json

        out = spec["sprig_raw"] if "sprig_raw" in spec else json.dumps(spec["sprig"])
        ans = {"stdout": out, "calls": spec.get("calls", [])}
        ans.update({k: spec[k] for k in ("stderr", "exit", "sleep") if k in spec})
        return "sprig", ans
    return _generic_make_answer(spec)


garden_cases.make_answer = make_answer

_REAL_SPRIG: str | None = None


def set_real_sprig(path: str | None) -> None:
    """The real sprig binary the ``"sprig": "real"`` cases run."""
    global _REAL_SPRIG
    _REAL_SPRIG = str(Path(path).resolve()) if path else None


def has_real_sprig() -> bool:
    return _REAL_SPRIG is not None


_generic_invoke = garden_cases.invoke


def invoke(case: dict[str, Any], argv: list[str], env: dict[str, str], cwd: Path, work: Path):
    """garden_cases.invoke, with the case's sprig on PATH first. A case's
    ``delay_s`` waits that long first: a case that runs into the 30 s step
    timeout would otherwise end on the edge of the minute its times are
    rounded to."""
    if case.get("delay_s"):
        import time

        time.sleep(case["delay_s"])
    mode = case.get("sprig")
    if mode == "fake":
        fake = work / "bin" / "sprig"
        fake.write_text(FAKE_SPRIG.replace("{PYTHON}", sys.executable), encoding="utf-8")
        fake.chmod(0o755)
    elif mode == "real":
        if _REAL_SPRIG is None:
            raise SystemExit(f"{case['name']}: a real-sprig case needs --sprig BIN")
        env = {**env, "DAISUGI_SPRIG": _REAL_SPRIG}
    return _generic_invoke(case, argv, env, cwd, work)


garden_cases.invoke = invoke

"""Regenerate semantics.json from the Python oracle.

The fixture freezes the oracle's exact matching semantics on synthetic
inputs so client implementations can unit-test their ports without the
(never-committed) corpus. Regenerate after any oracle semantics change:

    uv run python clients/fixtures/generate.py
"""

import json
from pathlib import Path

from opendaisugi.interpreter_parse import parse_interpreter
from opendaisugi.verify import (
    _SHELL_METACHAR_RE,
    _extract_shell_head,
    _head_allowed,
    _path_matches_any,
    resolve_strict,
)
from opendaisugi.models import Envelope, Permission

HEADS = [
    ("git", ["git"]), ("git", ["gi?"]), ("git", ["g*"]), ("git", []),
    ("gitx", ["git"]), ("rm", ["git", "rm"]), ("a*b", ["a*b"]),
    (".venv/bin/python", [".venv/bin/*"]), (".venv/bin/sub/python", [".venv/bin/*"]),
    ("/abs/.venv/bin/python", [".venv/bin/*"]), ("/usr/bin/python", ["/usr/bin/*"]),
    ("usr/bin/python", ["/usr/bin/*"]), ("/usr/bin/python", ["/usr/*/python"]),
    ("python3", ["python[23]"]), ("python4", ["python[23]"]),
    ("a/b", ["*"]), ("ab", ["*"]), ("a/b", ["*/*"]), ("a/b/c", ["*/*"]),
    ("x", ["**"]), ("x/y", ["**"]), ("npm", ["npm", "npx"]), ("npx", ["npm", "npx"]),
    ("uv", ["uv*"]), ("uvicorn", ["uv*"]), ("", ["git"]), ("git", ["GIT"]),
]
PATHS = [
    ("out.txt", ["out.txt"]), ("./out.txt", ["out.txt"]), ("a/../out.txt", ["out.txt"]),
    ("/etc/passwd", ["passwd"]), ("/etc/cron.d/job", ["job"]),
    ("sub/dir/x.py", ["*.py"]), ("x.py", ["*.py"]), ("/abs/x.py", ["*.py"]),
    ("out/x.txt", ["out/*"]), ("out/sub/x.txt", ["out/*"]),
    ("out/sub/x.txt", ["out/**"]), ("out", ["out/**"]), ("outer/x", ["out/**"]),
    ("a/b/c/x.py", ["a/**/x.py"]), ("a/x.py", ["a/*/x.py"]), ("a/b/x.py", ["a/*/x.py"]),
    ("./**", ["./**"]), ("x", ["./**"]), ("./x", ["./**"]), ("/x", ["./**"]),
    ("../x", ["./**"]), ("./a/b", ["./**"]), ("~/x", ["~/x"]), ("x", []),
    ("/tmp/x", ["/tmp/**"]), ("/tmp", ["/tmp/**"]), ("/tmpx/y", ["/tmp/**"]),
    ("/tmp/../etc/x", ["/tmp/**"]), ("/dev/null", ["/dev/*"]),
]
LINES = [
    "git status", "  git status  ", "", "   ", "# comment", "  # c", "FOO=1", "FOO=1 BAR=2",
    "FOO=1 git status", "FOO=1 BAR=2 git commit", "1FOO=1 git", "FOO=x=y git", "=x git",
    "env FOO=1 git", "./run.sh", "python3 x.py",
]
METACHAR = [
    "git status", "a; b", "a | b", "a & b", "a && b", "a > f", "a < f", "a`b`", "a $(b)",
    "a $x", "a\nb", "a\rb", "a ${x}", "echo 'a;b'", "echo \"$(x)\"", "a >> f", "a 2>&1",
]
INTERP = [
    "git status", "sh -c 'git status'", "bash -c \"ls\"", "bash -lc 'ls'", "sh -c",
    "xargs rm", "xargs -0 rm", "xargs -I {} mv {} /d", "find . -exec rm {} ;",
    "find . -name x", "env FOO=1 git status", "env -i git s", "timeout 5 git s",
    "timeout -k 2 5 git s", "nice -n 10 make", "nohup make", "stdbuf -oL make",
    "python -c 'print(1)'", "python3 script.py", "perl -e 'x'", "ruby x.rb", "node x.js",
    "awk '{print}'", "sed s/a/b/", "make test", "eval 'ls'", "command git s",
    "exec git s", "time git s", "sudo git s", "doas git s", "xargs", "env",
]

def interp_case(cmd):
    p = parse_interpreter(cmd)
    if p is None:
        return {"command": cmd, "payload": None}
    return {"command": cmd, "payload": {
        "head": p.head, "opaque": p.opaque, "inner_commands": list(p.inner_commands)}}

def env(stakes):
    return Envelope(generated_by="fixture", task="t", stakes=stakes,
                    permissions=Permission())

fixture = {
    "v": 1,
    "head_allowed": [
        {"head": h, "allowlist": a, "allowed": _head_allowed(h, a)} for h, a in HEADS],
    "path_match": [
        {"path": p, "globs": g, "matched": _path_matches_any(p, g)} for p, g in PATHS],
    "extract_head": [
        {"line": l, "head": _extract_shell_head(l.strip())} for l in LINES],
    "metachar": [
        {"command": c, "hit": bool(_SHELL_METACHAR_RE.search(c))} for c in METACHAR],
    "interpreter": [interp_case(c) for c in INTERP],
    "resolve_strict": [
        {"strict": s, "stakes": st, "effective": resolve_strict(s, env(st))}
        for s in (None, True, False) for st in ("low", "medium", "high", "physical")],
}

out = Path(__file__).parent / "semantics.json"
out.write_text(json.dumps(fixture, indent=1, sort_keys=True) + "\n")
counts = {k: len(v) for k, v in fixture.items() if isinstance(v, list)}
print(f"wrote {out} — {counts}")

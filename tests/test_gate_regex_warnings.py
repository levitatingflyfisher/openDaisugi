"""A hook's stderr carries the verdict and nothing else.

Python's re warns (FutureWarning) on a character class that may be read
differently in a future version, such as a nested set ``[[``. The warning
printed the oracle's own install path and source line into the gate's
stderr, which is the host's deny channel, so the text depended on where
the oracle was installed. The gate now ignores re's set warnings; the
pattern itself is read exactly as before.
"""

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]


def _envelope(regex: str, where: str) -> dict:
    expr = {"op": "not_matches", "path": "tool", "regex": regex}
    env = {"generated_by": "t", "task": "t", "permissions": {"file_read": ["/work/**"]}}
    if where == "postcondition":
        env["postconditions"] = [{"type": "t", "expr": {"op": "forall_steps", "pred": expr}}]
    else:
        env["invariants"] = [{"type": "t", "description": "d", "expr": expr}]
    return env


@pytest.mark.parametrize("regex", ["[[:alpha:]]", "(a([[b]))", "[a--b]", "[a&&b]", "[a||b]", "[a~~b]"])
@pytest.mark.parametrize("where", ["invariant", "postcondition"])
def test_a_set_warning_never_reaches_the_hook_stderr(tmp_path, regex, where):
    root = tmp_path / "gate"
    (root / "envelopes").mkdir(parents=True)
    (root / "envelopes" / "default.json").write_text(json.dumps(_envelope(regex, where)))
    payload = {"session_id": "s", "tool_name": "Read", "cwd": "/work", "tool_input": {"file_path": "/work/a"}}
    env = {**os.environ, "HOME": str(tmp_path), "PYTHONPATH": str(ROOT / "src")}
    env.pop("PYTHONWARNINGS", None)
    p = subprocess.run(
        [sys.executable, "-m", "opendaisugi.gate", "--mode", "enforce", "--root", str(root)],
        input=json.dumps(payload).encode(),
        capture_output=True,
        env=env,
        timeout=120,
        check=False,
    )
    err = p.stderr.decode()
    assert "Warning" not in err, err
    assert err == "" or err.startswith("openDaisugi gate: DENIED")

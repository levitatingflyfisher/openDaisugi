"""Importing the model client and every module that calls a model opens no
socket: the child process runs with a guard that records each attempt and
refuses it."""

import json
import subprocess
import sys

_GUARD = r"""
import json, socket, sys
attempts = []
def _refuse(name):
    def f(*a, **k):
        attempts.append([name, repr(a[:2])])
        raise OSError("network is off in this test")
    return f
socket.getaddrinfo = _refuse("getaddrinfo")
socket.create_connection = _refuse("create_connection")
socket.socket.connect = _refuse("connect")
socket.socket.connect_ex = _refuse("connect_ex")
import opendaisugi
from opendaisugi import (
    llm, llm_client, llm_check, claude_code_llm, distiller, envelope, decomposer,
    delegating_executor, tier1, synthesizer, pathway_bind, fallback,
)
from opendaisugi.voice import cleanup
from opendaisugi.parsers import claude_code
import httpx
llm_client.ModelClient()
sys.stdout.write("ATTEMPTS=" + json.dumps(attempts) + "\n")
sys.stdout.write("MODULES=" + json.dumps(sorted(m for m in ("litellm", "instructor") if m in sys.modules)) + "\n")
"""


def test_importing_the_model_callers_opens_no_socket():
    out = subprocess.run(
        [sys.executable, "-c", _GUARD],
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    lines = dict(
        ln.split("=", 1)
        for ln in out.stdout.splitlines()
        if ln.startswith(("ATTEMPTS=", "MODULES="))
    )
    assert "ATTEMPTS" in lines, f"the child did not finish: {out.stderr[-2000:]}"
    assert json.loads(lines["ATTEMPTS"]) == []
    assert json.loads(lines["MODULES"]) == []

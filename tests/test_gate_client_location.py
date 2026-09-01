"""The gate finds a compiled verifier client the same way wherever it runs.

The bench finds clients in the source checkout. The gate used the same
rule, so an installed gate, or a gate binary kept outside the checkout,
decided differently from one run inside it. The gate now finds a client
only through OPENDAISUGI_<NAME>_CLIENT (a path) or as daisugi-conform-<name>
on PATH. Python needs no lookup.
"""

import os
import stat
import sys

from opendaisugi.bench import options
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verifier_dispatch import verify_via

ENV = Envelope(generated_by="t", task="t", permissions=Permission(shell=True, shell_allowlist=["ls"]))
PLAN = ActionPlan(source="t", task="t", steps=[ShellStep(id="s0", command="ls")])
REFUSER = f"""#!{sys.executable}
import json, sys
for line in sys.stdin:
    case = json.loads(line)
    print(json.dumps({{"id": case["id"], "ok": False, "violations": [{{"stage": "z3", "step": None}}]}}))
"""


def _client(tmp_path, name):
    p = tmp_path / name
    p.write_text(REFUSER)
    p.chmod(p.stat().st_mode | stat.S_IXUSR)
    return p


def test_no_variable_and_nothing_on_path_is_not_built(tmp_path):
    env = {"PATH": str(tmp_path)}
    assert options.gate_client_argv(options.VERIFIER_CLIENTS["go"], env) == []
    assert options.gate_client_argv(options.VERIFIER_CLIENTS["python"], env) != []


def test_the_variable_names_the_client(tmp_path):
    p = _client(tmp_path, "my-go-client")
    env = {"PATH": "/usr/bin:/bin", "OPENDAISUGI_GO_CLIENT": str(p)}
    assert options.gate_client_argv(options.VERIFIER_CLIENTS["go"], env) == [str(p)]


def test_path_names_the_client(tmp_path):
    p = _client(tmp_path, "daisugi-conform-rust")
    env = {"PATH": f"/nonexistent:{tmp_path}"}
    assert options.gate_client_argv(options.VERIFIER_CLIENTS["rust"], env) == [str(p)]


def test_verify_via_on_the_gate_uses_the_gate_lookup(tmp_path, monkeypatch):
    p = _client(tmp_path, "daisugi-conform-go")
    monkeypatch.setenv("PATH", f"{tmp_path}{os.pathsep}{os.environ.get('PATH', '')}")
    monkeypatch.delenv("OPENDAISUGI_GO_CLIENT", raising=False)
    result = verify_via("go", PLAN, ENV, root=tmp_path / "gate", locate=options.gate_client_argv)
    assert result.ok is False and result.client_verdict is False
    assert p.exists()

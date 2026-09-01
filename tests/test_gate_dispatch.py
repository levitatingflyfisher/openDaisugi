"""The gate reads verifier_client per call, and every dispatch failure still
denies. Fail-closed is not negotiable at the boundary."""

import sys
import time
from pathlib import Path

from opendaisugi.gate import evaluate_record
from opendaisugi.models import Envelope, Permission

ENV = Envelope(
    generated_by="test",
    task="run tests",
    permissions=Permission(shell=True, shell_allowlist=["pytest"]),
)


def _record(command: str) -> dict:
    return {"tool_name": "Bash", "step_type": "shell", "command": command}


def _config(data_dir: Path, client: str) -> None:
    """Write the choice the way the swap menu writes it, not raw YAML."""
    from opendaisugi.config import load_config, save_config

    data_dir.mkdir(parents=True, exist_ok=True)
    path = data_dir / "config.yaml"
    save_config(load_config(path).model_copy(update={"verifier_client": client}), path)


def _register_script(monkeypatch, tmp_path: Path, name: str, body: str) -> None:
    from opendaisugi.bench import options

    script = tmp_path / f"{name}.py"
    script.write_text(body)
    monkeypatch.setitem(
        options.VERIFIER_CLIENTS,
        name,
        options.ClientSpec(
            name=name,
            argv=(sys.executable, str(script)),
            probe=None,
            build_steps=(),
            build_cwd=".",
            readme="docs/spec/conformance.md",
        ),
    )


def test_python_is_the_default_and_still_decides(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    assert evaluate_record(_record("pytest -q"), ENV, mode="enforce").allow is True
    assert evaluate_record(_record("rm -rf /"), ENV, mode="enforce").allow is False


def test_an_unbuilt_client_falls_back_and_still_denies(tmp_path, monkeypatch):
    import opendaisugi
    from opendaisugi.bench import options

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    monkeypatch.setattr(options, "client_is_built", lambda spec: spec.probe is None)
    _config(tmp_path, "lean")
    decision = evaluate_record(_record("rm -rf /"), ENV, mode="enforce")
    assert decision.allow is False


def test_a_core_profile_client_cannot_widen_the_gate(tmp_path, monkeypatch):
    """The Lean client implements the Core profile only. Selecting it must
    tighten or leave the verdict alone. Here a Core-profile fake allows an
    invariant-protected envelope the oracle denies, and the gate denies."""
    import opendaisugi
    from opendaisugi.models import Invariant

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _register_script(
        monkeypatch,
        tmp_path,
        "core",
        "import json, sys\n"
        "for line in sys.stdin:\n"
        "    line = line.strip()\n"
        "    if not line:\n"
        "        continue\n"
        "    case = json.loads(line)\n"
        "    print(json.dumps({'id': case['id'], 'ok': True, 'violations': []}), flush=True)\n",
    )
    _config(tmp_path, "core")
    envelope = Envelope(
        generated_by="test",
        task="write a build artifact",
        permissions=Permission(shell=True, shell_allowlist=["pytest"], file_write=["build/**"]),
        invariants=[
            Invariant(
                type="shell_only",
                description="every step is a shell step",
                expr={
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                },
            )
        ],
    )
    record = {"tool_name": "Write", "step_type": "file_write", "path": "build/out.txt"}
    decision = evaluate_record(record, envelope, mode="enforce")
    assert decision.allow is False, "a core-profile allow widened the gate"
    assert any(v["stage"] == "predicate" for v in decision.violations)


def test_a_dispatch_that_raises_denies_fail_closed_and_names_the_client(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "rust")

    def _boom(*a, **k):
        raise RuntimeError("dispatch exploded")

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _boom)
    decision = evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert decision.allow is False
    assert "denied fail-closed" in decision.reason
    assert "verifier_client=rust" in decision.reason
    assert "verifier_client: python" in decision.reason


def test_a_hung_client_leaves_room_for_the_oracle_inside_the_gate_budget(tmp_path, monkeypatch):
    """A dispatch gets a fraction of the inner budget. Without that split a
    hung client eats the whole budget, the join fires, and every call denies
    for good. Here the client really sleeps, and the oracle's verdict still
    arrives inside the join."""
    import opendaisugi
    from opendaisugi.gate import _DISPATCH_BUDGET_FRACTION
    from opendaisugi.verifier_dispatch import last_dispatch

    assert 0.0 < _DISPATCH_BUDGET_FRACTION < 1.0
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _register_script(
        monkeypatch, tmp_path, "hang", "import sys, time; sys.stdin.read(); time.sleep(30)\n"
    )
    _config(tmp_path, "hang")
    budget = 2.0
    t0 = time.monotonic()
    decision = evaluate_record(_record("pytest -q"), ENV, mode="enforce", verify_timeout_s=budget)
    elapsed = time.monotonic() - t0
    assert elapsed < budget, "the hung client consumed the whole gate budget"
    assert decision.reason == "verified in envelope", decision.reason
    assert decision.allow is True
    record = last_dispatch(tmp_path / "gate")
    assert record["client"] == "hang"
    assert record["ok"] is False
    assert "timed out" in record["error"]


def test_a_hung_client_never_lets_a_denial_through(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _register_script(
        monkeypatch, tmp_path, "hang", "import sys, time; sys.stdin.read(); time.sleep(30)\n"
    )
    _config(tmp_path, "hang")
    decision = evaluate_record(_record("rm -rf /"), ENV, mode="enforce", verify_timeout_s=2.0)
    assert decision.allow is False
    assert "denied fail-closed" not in decision.reason


def test_the_client_choice_is_read_per_call_not_at_import(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    seen: list[str] = []

    def _spy(client, plan, envelope, **kwargs):
        from opendaisugi.verify import verify

        seen.append(client)
        return verify(plan, envelope, strict=None)

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _spy)
    _config(tmp_path, "rust")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    _config(tmp_path, "go")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert seen == ["rust", "go"], "the gate cached the client choice"


def test_the_gate_root_follows_the_data_dir_so_a_dispatch_never_writes_to_home(
    tmp_path, monkeypatch
):
    """`_dispatch_verify` passes `root=DEFAULT_DATA_DIR / "gate"`. Without that,
    every test dispatch would stamp the operator's real ~/.opendaisugi/gate."""
    import opendaisugi
    from opendaisugi.bench import options
    from opendaisugi.verifier_dispatch import last_dispatch

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    monkeypatch.setattr(options, "client_is_built", lambda spec: spec.probe is None)
    _config(tmp_path, "lean")
    evaluate_record(_record("pytest -q"), ENV, mode="enforce")
    assert last_dispatch(tmp_path / "gate")["client"] == "lean"


def test_a_bad_client_name_in_config_denies_nothing_extra_but_still_verifies(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    _config(tmp_path, "not-a-client")
    assert evaluate_record(_record("rm -rf /"), ENV, mode="enforce").allow is False
    assert evaluate_record(_record("pytest -q"), ENV, mode="enforce").allow is True


def test_a_broken_config_file_still_verifies_with_the_oracle(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").mkdir()
    assert evaluate_record(_record("rm -rf /"), ENV, mode="enforce").allow is False
    assert evaluate_record(_record("pytest -q"), ENV, mode="enforce").allow is True


def test_the_gate_reads_the_client_from_the_root_s_own_config_not_home(tmp_path, monkeypatch):
    """`resolve_gate_mode` reads `root.parent / config.yaml`. The verifier
    choice must come from the same file, and the dispatch record must land
    under the same root, or `--root /srv/x/gate` dispatches by the home
    config and `daisugi modules --data-dir /srv/x` never sees a dispatch."""
    import opendaisugi
    from opendaisugi.verifier_dispatch import last_dispatch

    home = tmp_path / "home"
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", home)
    _config(home, "python")
    _register_script(
        monkeypatch,
        tmp_path,
        "agree",
        "import json, sys\n"
        "for line in sys.stdin:\n"
        "    line = line.strip()\n"
        "    if not line:\n"
        "        continue\n"
        "    case = json.loads(line)\n"
        "    print(json.dumps({'id': case['id'], 'ok': True, 'violations': []}), flush=True)\n",
    )
    served = tmp_path / "srv"
    root = served / "gate"
    _config(served, "agree")
    decision = evaluate_record(_record("pytest -q"), ENV, mode="enforce", root=root)
    assert decision.allow is True
    assert last_dispatch(root)["client"] == "agree", "the gate read the home config"
    assert last_dispatch(home / "gate") == {}, "the dispatch record landed under home"


def test_a_fail_closed_deny_names_the_client_from_the_root_s_config(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path / "home")
    served = tmp_path / "srv"
    _config(served, "rust")

    def _boom(*a, **k):
        raise RuntimeError("dispatch exploded")

    monkeypatch.setattr("opendaisugi.verifier_dispatch.verify_via", _boom)
    decision = evaluate_record(_record("pytest -q"), ENV, mode="enforce", root=served / "gate")
    assert decision.allow is False
    assert "verifier_client=rust" in decision.reason


def test_the_hook_entry_threads_its_root_to_the_dispatch(tmp_path, monkeypatch):
    """`gate_and_contract` is what the installed hook runs. Its --root must
    reach the verifier choice and the dispatch record."""
    import json

    import opendaisugi
    from opendaisugi.gate import gate_and_contract, register_envelope
    from opendaisugi.verifier_dispatch import last_dispatch

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path / "home")
    _register_script(
        monkeypatch,
        tmp_path,
        "agree",
        "import json, sys\n"
        "for line in sys.stdin:\n"
        "    line = line.strip()\n"
        "    if not line:\n"
        "        continue\n"
        "    case = json.loads(line)\n"
        "    print(json.dumps({'id': case['id'], 'ok': True, 'violations': []}), flush=True)\n",
    )
    served = tmp_path / "srv"
    root = served / "gate"
    _config(served, "agree")
    register_envelope(ENV, root=root)
    payload = json.dumps(
        {"tool_name": "Bash", "tool_input": {"command": "pytest -q"}, "session_id": "s1"}
    ).encode()
    gate_and_contract(payload, root=root, mode="enforce", captures_root=tmp_path / "captures")
    assert last_dispatch(root)["client"] == "agree"

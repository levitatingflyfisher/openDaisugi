"""Every stage tagged live must be proven live: change the config, do not
restart, observe the new behaviour. Each proof is a plain function so the
honesty test over STAGE_EFFECT can call it directly.

A stage tagged live with no proof here is a lie the suite must catch.
"""

from __future__ import annotations

from collections.abc import Callable
from pathlib import Path


def _write_config(data_dir: Path, **fields) -> Path:
    """Write config.yaml the way the product writes it.

    Through `load_config` and `save_config`, never raw YAML. A proof that a
    stage is live has to change the config the way an operator does, or it
    proves the test helper works and nothing else.
    """
    from opendaisugi.config import load_config, save_config

    data_dir.mkdir(parents=True, exist_ok=True)
    path = data_dir / "config.yaml"
    save_config(load_config(path).model_copy(update=fields), path)
    return path


def prove_backend_swap(tmp: Path) -> None:
    """Rewrite llm_backend through save_config, ask for a client, and the
    backend that answers is the new one. No restart.

    The observation is `get_instructor_client`, the factory every envelope
    generation goes through, with both real backends replaced by fakes so no
    subprocess and no network is touched. Watching `resolve_backend()` alone
    would stay green if a later change memoised the client per process, and
    the tag would go stale with the proof still passing.
    """
    import os
    import shutil

    import pytest

    instructor = pytest.importorskip("instructor")  # v0.47: [generate] extra, not base

    import opendaisugi
    from opendaisugi import claude_code_llm, llm

    class FakeClaude:
        backend = "claude-code"

    class FakeLitellm:
        backend = "litellm"

    data_dir = tmp / "backend"
    original_dir = opendaisugi.DEFAULT_DATA_DIR
    original_claude = claude_code_llm.ClaudeCodeInstructorClient
    original_from_litellm = instructor.from_litellm
    original_which = shutil.which
    saved_env = {k: os.environ.get(k) for k in ("OPENDAISUGI_LLM_BACKEND", "ANTHROPIC_API_KEY")}
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        claude_code_llm.ClaudeCodeInstructorClient = FakeClaude
        instructor.from_litellm = lambda *a, **k: FakeLitellm()
        # preflight must pass for both backends: a claude binary on PATH and a key.
        shutil.which = lambda name, *a, **k: "/usr/bin/claude" if name == "claude" else None
        os.environ.pop("OPENDAISUGI_LLM_BACKEND", None)
        os.environ["ANTHROPIC_API_KEY"] = "sk-ant-fake"

        _write_config(data_dir, llm_backend="claude-code")
        assert llm.get_instructor_client("m").backend == "claude-code"
        _write_config(data_dir, llm_backend="litellm")
        assert llm.get_instructor_client("m").backend == "litellm", (
            "the client did not follow the config; the backend choice is cached"
        )
        _write_config(data_dir, llm_backend="claude-code")
        assert llm.get_instructor_client("m").backend == "claude-code"
        # An explicit argument and the env var both still outrank the file.
        assert llm.get_instructor_client("m", backend="litellm").backend == "litellm"
        os.environ["OPENDAISUGI_LLM_BACKEND"] = "litellm"
        assert llm.get_instructor_client("m").backend == "litellm"
    finally:
        for k, v in saved_env.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        shutil.which = original_which
        instructor.from_litellm = original_from_litellm
        claude_code_llm.ClaudeCodeInstructorClient = original_claude
        opendaisugi.DEFAULT_DATA_DIR = original_dir


def test_the_backend_proof_catches_a_client_cache(tmp_path, monkeypatch):
    """The proof exists to catch a memoised client. Prove it has teeth."""
    import functools

    import pytest

    from opendaisugi import llm

    monkeypatch.setattr(llm, "get_instructor_client", functools.cache(llm.get_instructor_client))
    with pytest.raises(AssertionError, match="cached"):
        prove_backend_swap(tmp_path)


def test_backend_swap_is_live_without_restart(tmp_path):
    prove_backend_swap(tmp_path)


def test_the_backend_stage_is_tagged_live():
    """The tag and the code have to move in the same commit, or the map lies."""
    from opendaisugi.swap import LIVE, STAGE_EFFECT

    assert STAGE_EFFECT["backend"] == LIVE


def test_a_default_config_does_not_pin_the_backend(tmp_path):
    """`llm_backend` defaults to None, which means auto-detect. A file that does
    not set it must not shadow auto-detection, or every box would force one."""
    from opendaisugi.config import configured_backend

    (tmp_path / "config.yaml").write_text("gate_mode: shadow\n")
    assert configured_backend(tmp_path / "config.yaml") is None
    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    assert configured_backend(tmp_path / "config.yaml") == "ollama"


def test_a_product_write_that_touches_another_field_does_not_pin_the_backend(tmp_path, monkeypatch):
    """save_config writes every field. The default it writes for llm_backend
    must still mean auto, or the first consent prompt or swap click would pin
    a backend nobody chose."""
    import opendaisugi
    from opendaisugi.config import configured_backend

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: None)
    data_dir = tmp_path / "product"
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", data_dir)
    path = _write_config(data_dir, gate_mode="shadow")
    assert "llm_backend" in path.read_text()
    assert configured_backend(path) is None
    from opendaisugi.llm import resolve_backend

    assert resolve_backend() == "litellm"
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    assert resolve_backend() == "litellm"
    monkeypatch.delenv("ANTHROPIC_API_KEY")
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/claude")
    assert resolve_backend() == "claude-code"


def test_a_file_that_names_the_backend_is_honoured_as_written(tmp_path, monkeypatch):
    """A config the product wrote before the default changed carries
    `llm_backend: claude-code`. That is a choice on disk and it stays one."""
    import opendaisugi
    from opendaisugi.config import configured_backend, load_config
    from opendaisugi.llm import resolve_backend

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("llm_backend: claude-code\n")
    assert configured_backend(tmp_path / "config.yaml") == "claude-code"
    assert load_config(tmp_path / "config.yaml").llm_backend == "claude-code"
    assert resolve_backend() == "claude-code"


def test_the_backend_knob_shows_auto_for_null_and_round_trips(tmp_path):
    from opendaisugi.config import load_config
    from opendaisugi.swap import SWAP_KNOBS, apply_swap, selected_label

    data_dir = tmp_path / "knob"
    path = _write_config(data_dir)
    assert selected_label(load_config(path), "backend") == "auto"
    for opt in SWAP_KNOBS["backend"].options:
        cfg = apply_swap("backend", opt.label, config_path=path)
        assert cfg.llm_backend == opt.value
        assert load_config(path).llm_backend == opt.value
        assert selected_label(load_config(path), "backend") == opt.label
    cfg = apply_swap("backend", "auto", config_path=path)
    assert cfg.llm_backend is None


def test_config_truth_surface_shows_auto_for_a_null_backend(tmp_path, monkeypatch):
    from opendaisugi.config import resolved_config

    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: None)
    path = _write_config(tmp_path / "surface", gate_mode="shadow")
    fields = {f.key: f for f in resolved_config(path, home=tmp_path, cwd=tmp_path, env={})}
    assert fields["llm_backend"].value == "auto"
    assert fields["llm_backend (resolved)"].source == "auto"
    assert fields["llm_backend (resolved)"].value == "litellm"


def test_config_truth_surface_reads_only_the_path_and_env_it_was_given(tmp_path, monkeypatch):
    """The fallback must not read the process env or the default data dir,
    or the value would come from a source the row does not name."""
    import opendaisugi
    from opendaisugi.config import resolved_config

    other = tmp_path / "other"
    other.mkdir()
    (other / "config.yaml").write_text("llm_backend: ollama\n")
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", other)
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "anthropic")
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    monkeypatch.setattr("shutil.which", lambda name: None)
    (tmp_path / "config.yaml").write_text("gate_mode: shadow\n")
    fields = {
        f.key: f
        for f in resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={})
    }
    row = fields["llm_backend (resolved)"]
    assert (row.value, row.source) == ("litellm", "auto")


def test_configured_backend_defaults_to_the_package_data_dir(tmp_path, monkeypatch):
    import opendaisugi
    from opendaisugi.config import configured_backend

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    assert configured_backend() == "ollama"


def test_a_blank_or_broken_file_does_not_pin_the_backend(tmp_path):
    from opendaisugi.config import configured_backend

    assert configured_backend(tmp_path / "missing.yaml") is None
    (tmp_path / "config.yaml").write_text("llm_backend: '   '\n")
    assert configured_backend(tmp_path / "config.yaml") is None
    (tmp_path / "config.yaml").write_text("llm_backend: [not, a, name]\n")
    assert configured_backend(tmp_path / "config.yaml") is None
    (tmp_path / "config.yaml").write_text(": : not yaml\n")
    assert configured_backend(tmp_path / "config.yaml") is None


def test_an_unreadable_config_falls_back_to_auto_detection(tmp_path, monkeypatch):
    """A broken config must not break backend choice."""
    import opendaisugi
    from opendaisugi.llm import resolve_backend

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setattr("shutil.which", lambda name: None)
    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").mkdir()
    assert resolve_backend() == "litellm"


def test_config_truth_surface_says_the_source_is_the_file(tmp_path, monkeypatch):
    from opendaisugi.config import resolved_config

    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    fields = {
        f.key: f
        for f in resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={})
    }
    row = fields["llm_backend (resolved)"]
    assert row.value == "ollama"
    assert row.source == "file"


def test_config_truth_surface_ranks_env_above_the_file(tmp_path):
    from opendaisugi.config import resolved_config

    (tmp_path / "config.yaml").write_text("llm_backend: ollama\n")
    fields = {
        f.key: f
        for f in resolved_config(
            tmp_path / "config.yaml",
            home=tmp_path,
            cwd=tmp_path,
            env={"OPENDAISUGI_LLM_BACKEND": "litellm"},
        )
    }
    row = fields["llm_backend (resolved)"]
    assert (row.value, row.source) == ("litellm", "env")


def prove_verifier_swap(tmp: Path) -> None:
    """Rewrite verifier_client in config.yaml, call the gate, and the gate
    dispatches to the new client. No restart.

    The observation runs through `gate.evaluate_record`, the function every
    hook payload lands in, and reads back the dispatch that actually happened
    from `verifier_dispatch.last_dispatch`, which the gate writes under the
    data dir. Nothing here inspects a private attribute or re-reads the YAML.

    The clients are fake scripts, never compiled binaries, so the proof holds
    on a box where nothing is built. A proof that skips would let the live tag
    pass vacuously, which is exactly what the honesty test exists to catch.
    """
    import sys

    import opendaisugi
    from opendaisugi.bench import options
    from opendaisugi.gate import evaluate_record
    from opendaisugi.models import Envelope, Permission
    from opendaisugi.verifier_dispatch import last_dispatch

    data_dir = tmp / "verifier"
    data_dir.mkdir(parents=True, exist_ok=True)
    script = data_dir / "agree.py"
    script.write_text(
        "import json, sys\n"
        "for line in sys.stdin:\n"
        "    line = line.strip()\n"
        "    if not line:\n"
        "        continue\n"
        "    case = json.loads(line)\n"
        "    print(json.dumps({'id': case['id'], 'ok': True, 'violations': []}), flush=True)\n"
    )
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["pytest"]),
    )
    record = {"tool_name": "Bash", "step_type": "shell", "command": "pytest -q"}
    original = opendaisugi.DEFAULT_DATA_DIR
    saved = dict(options.VERIFIER_CLIENTS)
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        for name in ("alpha", "beta"):
            options.VERIFIER_CLIENTS[name] = options.ClientSpec(
                name=name,
                argv=(sys.executable, str(script)),
                probe=None,
                build_steps=(),
                build_cwd=".",
                readme="docs/spec/conformance.md",
            )

        _write_config(data_dir, verifier_client="alpha")
        assert evaluate_record(record, envelope, mode="enforce").allow is True
        assert last_dispatch(data_dir / "gate")["client"] == "alpha"

        _write_config(data_dir, verifier_client="beta")
        assert evaluate_record(record, envelope, mode="enforce").allow is True
        assert last_dispatch(data_dir / "gate")["client"] == "beta", (
            "the gate cached the client choice"
        )
    finally:
        options.VERIFIER_CLIENTS.clear()
        options.VERIFIER_CLIENTS.update(saved)
        opendaisugi.DEFAULT_DATA_DIR = original


def test_verifier_swap_is_live_without_restart(tmp_path):
    prove_verifier_swap(tmp_path)


def prove_matcher_swap(tmp: Path) -> None:
    """Rewrite matcher_model through save_config and watch matching change.

    Both halves are public: the config is written the way the swap menu writes
    it, and the behaviour is read off `PathwayStore.find`, which is what
    actually reuses a pathway. With `lexical` the stored row matches. With a
    matcher key that is not built, `active_model_name` raises
    `MatcherNotAvailable`, `find` degrades to no reuse, and the operator gets
    zero pathways. That is a real, config-driven behaviour change with no
    second model package and nothing private touched.

    No `_search.reload()` inside the proof: `find` re-resolves the identity
    per call and the embedder cache is keyed by identity, so the stage is
    live without it. The one call in `finally` is hygiene for the tests that
    follow, not the mechanism.
    """
    import opendaisugi
    from opendaisugi import _search
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION
    from opendaisugi.exceptions import MatcherNotAvailable
    from opendaisugi.models import ActionPlan, Envelope, Permission
    from opendaisugi.pathway import CompiledPathway
    from opendaisugi.pathway_store import PathwayStore

    data_dir = tmp / "matcher"
    _write_config(data_dir, matcher_model="lexical")
    original = opendaisugi.DEFAULT_DATA_DIR
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        assert _search.active_model_name() == "lexical-hash-v1"

        store = PathwayStore(data_dir / "pathways.db")
        vec = [float(x) for x in _search._get_model().encode(["add a test"])[0]]
        store.put(
            CompiledPathway(
                id="a",
                task_description="add a test",
                task_embedding=vec,
                embedding_model="lexical-hash-v1",
                embedding_model_version=_EMBEDDING_MODEL_VERSION,
                envelope=Envelope(generated_by="t", task="add a test", permissions=Permission()),
                plan_template=ActionPlan(source="t", task="add a test", steps=[]),
                source_trace_ids=["t1"],
                distilled_at=0.0,
            )
        )
        assert store.find("add a test") is not None

        # A matcher key with no shipped embedder. `not-a-matcher` rather than
        # `int8`, because int8 would try to fetch its model files.
        _write_config(data_dir, matcher_model="not-a-matcher")
        try:
            _search.active_model_name()
        except MatcherNotAvailable:
            pass
        else:
            raise AssertionError("the matcher choice is cached, not read per call")
        assert store.find("add a test") is None, "an unbuilt matcher must not still match"

        # And back again, in the same process.
        _write_config(data_dir, matcher_model="lexical")
        assert _search.active_model_name() == "lexical-hash-v1"
        assert store.find("add a test") is not None
    finally:
        _search.reload()
        opendaisugi.DEFAULT_DATA_DIR = original


def test_matcher_swap_is_live_without_restart(tmp_path):
    prove_matcher_swap(tmp_path)


def test_two_real_backends_swap_live_when_both_are_installed(tmp_path):
    """The same proof across two genuinely different embedders. Skips, with the
    reason, when only one is installed; the hermetic proof above is the one the
    honesty test calls. Nothing here loads a model: `active_model_name` is an
    identity lookup, so no download can happen."""
    import importlib.util

    import pytest

    if importlib.util.find_spec("model2vec") is None:
        pytest.skip("model2vec is not installed; potion cannot be compared to lexical")

    import opendaisugi
    from opendaisugi import _search

    data_dir = tmp_path / "pair"
    _write_config(data_dir, matcher_model="lexical")
    original = opendaisugi.DEFAULT_DATA_DIR
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        _search.reload()
        assert _search.active_model_name() == "lexical-hash-v1"
        _write_config(data_dir, matcher_model="potion")
        _search.reload()
        assert _search.active_model_name() != "lexical-hash-v1"
    finally:
        _search.reload()
        opendaisugi.DEFAULT_DATA_DIR = original


def prove_shell_swap(tmp: Path) -> None:
    """Flip shell_allow_decomposition in config.yaml and the gate's verdict on a
    compound command changes. No restart.

    The whole path is public: `daisugi gate init` reads config.yaml and
    registers a starter envelope, `gate.load_envelope` reads it back, and
    `gate.evaluate_record` rules on `echo a && echo b`. With decomposition off
    the compound is refused outright; with it on, every head is checked
    against the starter allowlist and `echo` is in it.
    """
    from typer.testing import CliRunner

    import opendaisugi
    from opendaisugi.cli import app
    from opendaisugi.gate import evaluate_record, load_envelope
    from opendaisugi.shell_decompose import parser_available

    assert parser_available(), (
        "this proof needs the bash grammar from the shell extra. Without it the "
        "config value cannot change a verdict, and the shell stage would not be live."
    )

    data_dir = tmp / "shell"
    root = data_dir / "gate"
    runner = CliRunner()
    record = {"tool_name": "Bash", "step_type": "shell", "command": "echo a && echo b"}
    original = opendaisugi.DEFAULT_DATA_DIR
    try:
        opendaisugi.DEFAULT_DATA_DIR = data_dir
        _write_config(data_dir, shell_allow_decomposition=False)
        init = ["gate", "init", "--root", str(root), "--workspace", str(tmp), "--force"]
        assert runner.invoke(app, init).exit_code == 0
        off = evaluate_record(record, load_envelope(None, root=root), mode="enforce")
        assert off.allow is False, "a compound command must be refused with decomposition off"

        _write_config(data_dir, shell_allow_decomposition=True)
        assert runner.invoke(app, init).exit_code == 0
        on = evaluate_record(record, load_envelope(None, root=root), mode="enforce")
        assert on.allow is True, "the config change did not reach the verdict"
    finally:
        opendaisugi.DEFAULT_DATA_DIR = original


def prove_distill_swap(tmp: Path) -> None:
    """Flip auto_tend and the code that consults it behaves differently.

    The observation is `daisugi hook auto-tend`, the command cron and the
    detached spawn actually run. It prints "skipped" when consent is off and
    proceeds when it is on. That is the behaviour the flag exists to control.
    The captures root is under the proof's own directory, so nothing in the
    operator's home is read.
    """
    from typer.testing import CliRunner

    from opendaisugi.cli import app

    data_dir = tmp / "distill"
    captures = data_dir / "captures"
    runner = CliRunner()
    argv = ["hook", "auto-tend", "--data-dir", str(data_dir), "--captures-root", str(captures)]

    _write_config(data_dir, auto_tend=False)
    off = runner.invoke(app, argv)
    assert off.exit_code == 0
    assert "skipped: background distillation is off" in off.output

    _write_config(data_dir, auto_tend=True)
    on = runner.invoke(app, argv)
    assert on.exit_code == 0
    assert "background distillation is off" not in on.output, (
        "the consent gate did not follow the config"
    )
    assert "converted 0 sessions" in on.output


def prove_router_swap(tmp: Path) -> None:
    """Rewrite the gateway's config, and the SAME running app routes the next
    turn to the new model. No restart, no new app object.

    The behavioural half uses `gateway_local_model`. The structural half,
    `test_reloader_hands_the_rebuild_a_whole_fresh_config` in
    tests/test_gateway_reload.py, covers any other field the rebuild reads:
    the reloader hands the rebuild a whole fresh Config, not a list of named
    fields. The choice of chooser, gateway_router, is not live. `daisugi
    gateway` reads it at launch, because a Switchyard child cannot follow a
    config change.
    """
    import asyncio
    import json

    import httpx

    from opendaisugi.gateway_asgi import ConfigReloader, build_default_gateway, make_gateway_app

    data_dir = tmp / "router"
    path = _write_config(data_dir, gateway_local_model=None)
    reloader = ConfigReloader(
        path,
        lambda config: build_default_gateway(
            data_dir=data_dir, journalling=False, local_model=config.gateway_local_model
        ),
        min_interval_s=0.0,
    )
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=data_dir, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "list the files in src"}],
    }

    async def _post() -> None:
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
            await c.post(
                "/v1/messages",
                content=json.dumps(body).encode(),
                headers={"content-type": "application/json"},
            )

    asyncio.run(_post())
    assert seen[-1] == "claude-haiku-4-5"

    _write_config(data_dir, gateway_local_model="qwen2.5-coder-7b")
    asyncio.run(_post())
    assert seen[-1] == "qwen2.5-coder-7b", "the running app cached its gateway"


def prove_floor_backend_swap(tmp: Path) -> None:
    """Rewrite floor.backend and the next pane pick asks for the new backend.

    `pick_backend` is the one function every floor client calls, and it reads
    `config.floor.backend` from the Config it is handed, which the cockpit
    reloads from disk on every screen resume. The probe is stubbed so the
    proof never touches a real coppice socket, herdr, or tmux on this box: it
    records which backend was asked for and hands back a stand-in.
    """
    from opendaisugi.config import load_config
    from opendaisugi.floor import registry

    data_dir = tmp / "floor"
    path = _write_config(data_dir)
    asked: list[str] = []

    class _StandIn:
        def __init__(self, name: str) -> None:
            self.name = name

    def _probe(name, config, *, autostart=False):
        asked.append(name)
        return _StandIn(name), registry.BackendStatus(name, True, "", "")

    original = registry._probe
    registry._probe = _probe
    try:
        _write_config(
            data_dir, floor=load_config(path).floor.model_copy(update={"backend": "tmux"})
        )
        assert registry.pick_backend(load_config(path)).name == "tmux"
        _write_config(
            data_dir, floor=load_config(path).floor.model_copy(update={"backend": "coppice"})
        )
        assert registry.pick_backend(load_config(path)).name == "coppice", (
            "the pane pick cached its backend"
        )
        assert asked == ["tmux", "coppice"]
    finally:
        registry._probe = original


def test_router_swap_is_live_without_restart(tmp_path):
    prove_router_swap(tmp_path)


def test_shell_swap_is_live_without_restart(tmp_path):
    prove_shell_swap(tmp_path)


def test_distill_swap_is_live_without_restart(tmp_path):
    prove_distill_swap(tmp_path)


def test_floor_backend_swap_is_live_without_restart(tmp_path):
    prove_floor_backend_swap(tmp_path)


LIVE_PROOFS: dict[str, Callable[[Path], None]] = {
    "shell": prove_shell_swap,
    "distill": prove_distill_swap,
    "backend": prove_backend_swap,
    "verifier": prove_verifier_swap,
    "matcher": prove_matcher_swap,
    "router": prove_router_swap,
    "floor_backend": prove_floor_backend_swap,
}


def test_stage_effect_tags_match_reality(tmp_path):
    """Every live tag is backed by a proof that runs here, and every cfg tag
    says why. A tag nobody checks is a dishonest control."""
    from opendaisugi.swap import CFG_REASON, LIVE, PLANNED, STAGE_EFFECT

    for stage, effect in STAGE_EFFECT.items():
        if effect == LIVE:
            proof = LIVE_PROOFS.get(stage)
            assert proof is not None, (
                f"stage {stage!r} is tagged live with no proof. "
                f"Add one to LIVE_PROOFS in tests/test_hot_swap.py, or change the tag."
            )
            proof(tmp_path / f"proof-{stage}")
        elif effect == PLANNED:
            continue
        else:
            reason = CFG_REASON.get(stage, "")
            assert reason.strip(), f"cfg stage {stage!r} must say why it is not live"


def test_every_proof_names_a_stage_that_is_tagged_live():
    """A proof for a stage that is not live is dead code the honesty test
    never runs. Keep the two in step."""
    from opendaisugi.swap import LIVE, STAGE_EFFECT

    for stage in LIVE_PROOFS:
        assert STAGE_EFFECT.get(stage) == LIVE, f"{stage!r} has a proof but is not tagged live"


def test_the_gate_stage_stays_cfg_and_says_the_hook_file_is_not_ours():
    from opendaisugi.swap import CFG_REASON, STAGE_EFFECT, tag_for

    assert STAGE_EFFECT["gate"] == "cfg"
    assert "harness" in CFG_REASON["gate"]
    assert "reinstall" in tag_for("gate")


def test_tag_for_a_live_stage_carries_no_reason():
    from opendaisugi.swap import tag_for

    assert tag_for("shell").startswith("live")
    assert "reinstall" not in tag_for("shell")
    assert tag_for("no-such-stage") == ""


def test_a_cfg_tag_line_does_not_repeat_itself():
    from opendaisugi.swap import CFG_REASON, tag_for

    for stage in CFG_REASON:
        line = tag_for(stage)
        assert line.count("reinstall") <= 1, line
        assert line.count("restart") <= 1, line

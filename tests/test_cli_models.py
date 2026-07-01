"""CLI: `daisugi models` — discover/resolve a trustworthy, commit-pinned local model."""


from typer.testing import CliRunner

import opendaisugi.model_registry as mr
from opendaisugi.cli import app
from opendaisugi.model_registry import ModelRef, UntrustedSource

runner = CliRunner()


def test_models_help():
    res = runner.invoke(app, ["models", "--help"])
    assert res.exit_code == 0
    assert "models" in res.output.lower()


def test_models_resolve_prints_pinned_ref(monkeypatch):
    monkeypatch.setattr(
        mr, "resolve_pinned",
        lambda repo, **kw: ModelRef(repo_id=repo, filename="m.Q4_K_M.llamafile", revision="abc123"),
    )
    res = runner.invoke(app, ["models", "mozilla-ai/x"])
    assert res.exit_code == 0, res.output
    assert "m.Q4_K_M.llamafile" in res.output
    assert "abc123" in res.output           # the pinned revision is shown


def test_models_untrusted_exits_nonzero(monkeypatch):
    def boom(repo, **kw):
        raise UntrustedSource("nope")

    monkeypatch.setattr(mr, "resolve_pinned", boom)
    res = runner.invoke(app, ["models", "evil/x"])
    assert res.exit_code != 0


def test_models_discover_lists_trusted(monkeypatch):
    monkeypatch.setattr(mr, "discover_llamafiles", lambda **kw: ["mozilla-ai/a", "Qwen/b"])
    res = runner.invoke(app, ["models"])
    assert res.exit_code == 0, res.output
    assert "mozilla-ai/a" in res.output

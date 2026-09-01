"""SGCM review M3: `daisugi registry init` must reject git ext::/fd:: URLs."""

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def test_registry_init_rejects_ext_transport(tmp_path):
    marker = tmp_path / "pwned"
    res = runner.invoke(
        app,
        ["registry", "init", f'ext::sh -c "touch {marker}"', "--clone-to", str(tmp_path / "clone")],
    )
    assert res.exit_code == 2
    assert "ext::" in res.output
    assert not marker.exists()  # no command executed


def test_registry_init_rejects_fd_transport(tmp_path):
    res = runner.invoke(app, ["registry", "init", "fd::7", "--clone-to", str(tmp_path / "c")])
    assert res.exit_code == 2


def test_registry_init_ignores_an_inherited_git_environment(tmp_path, monkeypatch):
    import subprocess

    bare = tmp_path / "remote.git"
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(bare)], check=True)
    seed = tmp_path / "seed"
    subprocess.run(["git", "init", "-q", str(seed)], check=True)
    subprocess.run(
        ["git", "-C", str(seed), "-c", "user.email=s@test", "-c", "user.name=s"]
        + ["commit", "-q", "--allow-empty", "-m", "seed"],
        check=True,
    )
    subprocess.run(["git", "-C", str(seed), "push", "-q", str(bare), "HEAD:main"], check=True)
    hooks = tmp_path / "hooks"
    hooks.mkdir()
    marker = tmp_path / "hook-ran"
    (hooks / "post-checkout").write_text(f"#!/bin/sh\ntouch {marker}\n")
    (hooks / "post-checkout").chmod(0o755)
    monkeypatch.setenv("GIT_CONFIG_COUNT", "1")
    monkeypatch.setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
    monkeypatch.setenv("GIT_CONFIG_VALUE_0", str(hooks))
    clone = tmp_path / "clone"
    res = runner.invoke(app, ["registry", "init", str(bare), "--clone-to", str(clone)])
    assert res.exit_code == 0, res.output
    assert (clone / ".git").is_dir()
    assert not marker.exists()

"""`daisugi coppice …`: the floor for scripts and agents.

Exit codes are the master's: 0 success, 1 user error, 3 unreachable. `backends`
is the diagnostic, so it exits 0 even when nothing is installed — a table of
three `no` rows with three fixes is the answer, not a failure.
"""

from __future__ import annotations

import json
import os
import shutil

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def _no_backends(monkeypatch):
    from opendaisugi.floor import registry

    class Dead:
        name = "dead"
        yields_frames = False

        def available(self):
            return False

    monkeypatch.setattr(registry, "build_backend", lambda name, config, *, autostart=False: Dead())


def test_backends_with_nothing_installed_prints_three_no_rows_and_exits_zero(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(app, ["coppice", "backends", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    for name in ("coppice", "herdr", "tmux"):
        assert name in result.stdout
    assert result.stdout.count("no") >= 3
    assert "coppice server start" in result.stdout


def test_backends_json_is_a_list_of_rows(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(app, ["coppice", "backends", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    rows = json.loads(result.stdout)
    assert [r["name"] for r in rows] == ["coppice", "herdr", "tmux"]
    assert all(r["available"] is False and r["fix"] for r in rows)


def test_spawn_with_nothing_installed_exits_three_and_names_the_fix(monkeypatch, tmp_path):
    _no_backends(monkeypatch)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "coppice server start" in result.output


def test_an_unknown_backend_name_exits_three_and_lists_the_real_ones(tmp_path):
    result = runner.invoke(
        app, ["coppice", "list", "--backend", "zellij", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 3
    assert "coppice, herdr, tmux" in result.output


def test_every_coppice_command_accepts_data_dir(tmp_path, fake_floor_backend):
    """No group may be pinned to the developer's real ~/.opendaisugi."""
    for argv in (
        ["coppice", "backends"],
        ["coppice", "list"],
        ["coppice", "read", "p1"],
        ["coppice", "close", "p1"],
        ["coppice", "send-keys", "p1", "enter"],
        ["coppice", "prompt", "p1", "hi"],
        ["coppice", "task", "list"],
        ["coppice", "task", "create", "--label", "x"],
        ["coppice", "task", "close", "t1"],
    ):
        result = runner.invoke(app, [*argv, "--data-dir", str(tmp_path)])
        assert result.exit_code == 0, f"{argv}: {result.output}"


def test_the_config_read_is_the_one_under_data_dir(tmp_path, monkeypatch):
    """`--data-dir` must reach `load_config`, not just be accepted and dropped."""
    seen: list = []
    from opendaisugi.floor import registry

    class Fake:
        name = "fake"
        yields_frames = False

        def available(self):
            return True

        def list(self):
            return []

    def picked(config, *, name=None, autostart=False):
        seen.append(config)
        return Fake()

    monkeypatch.setattr(registry, "pick_backend", picked)
    (tmp_path / "config.yaml").write_text("floor:\n  backend: tmux\n", encoding="utf-8")
    result = runner.invoke(app, ["coppice", "list", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert seen and seen[0].floor.backend == "tmux"


def test_an_explicit_coppice_backend_asks_for_autostart(tmp_path, monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    class Fake:
        name = "coppice"
        yields_frames = True

        def available(self):
            return True

        def list(self):
            return []

    def picked(config, *, name=None, autostart=False):
        seen.append(autostart)
        return Fake()

    monkeypatch.setattr(registry, "pick_backend", picked)
    runner.invoke(app, ["coppice", "list", "--backend", "coppice", "--data-dir", str(tmp_path)])
    runner.invoke(app, ["coppice", "list", "--data-dir", str(tmp_path)])
    assert seen == [True, False]


def test_an_unknown_read_source_exits_one_and_names_the_real_ones(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "read", "p1", "--source", "scrollback", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "detection" in result.output
    assert fake_floor_backend.reads == [], "a bad source must be refused before the backend"


def test_an_unknown_spawn_kind_exits_one_and_names_the_real_ones(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--kind",
            "vt",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    assert result.exit_code == 1
    assert "pty, headless" in result.output
    assert fake_floor_backend.spawned == [], "a bad kind must be refused before the backend"


def test_spawn_prints_the_pane_id(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--label",
            "fix",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    assert result.exit_code == 0
    assert "p1" in result.stdout
    assert fake_floor_backend.spawned[0]["label"] == "fix"


def test_spawn_passes_harness_straight_through_with_no_typeerror_retry(
    tmp_path, fake_floor_backend
):
    """A retry-on-TypeError would swallow a real TypeError raised inside spawn."""
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--kind",
            "headless",
            "--harness",
            "claude-code",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    assert result.exit_code == 0
    assert fake_floor_backend.spawned[0]["harness"] == "claude-code"


def test_a_typeerror_inside_spawn_is_not_retried(tmp_path, monkeypatch, fake_floor_backend):
    calls: list[int] = []

    def boom(**kwargs):
        calls.append(1)
        raise TypeError("a real bug inside spawn")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code != 0
    assert calls == [1], "spawn was retried, hiding the bug"


def test_spawn_json_reports_the_backend_it_used(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--json",
            "--data-dir",
            str(tmp_path),
            "--",
            "claude",
        ],
    )
    body = json.loads(result.stdout)
    assert body["pane"] == "p1" and body["backend"] == "fake"


def test_list_json_carries_state_and_source(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "list", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    rows = json.loads(result.stdout)
    assert rows[0]["pane"] == "p1"
    assert rows[0]["state"] == "working" and rows[0]["source"] == "gate"


def test_read_passes_the_source_through(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "read", "p1", "--source", "detection", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert ("p1", "detection") in fake_floor_backend.reads


def test_read_json_carries_the_pane_source_and_text(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        ["coppice", "read", "p1", "--source", "detection", "--json", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0
    body = json.loads(result.stdout)
    assert body == {"pane": "p1", "source": "detection", "text": "[detection]\n"}


def test_prompt_says_whether_it_prompted_or_typed(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "prompt", "p1", "fix the test", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert "typed" in result.stdout
    assert fake_floor_backend.sent == [("p1", "fix the test")]


def test_prompt_json_carries_the_pane_and_how(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "prompt", "p1", "fix the test", "--json", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert json.loads(result.stdout) == {"pane": "p1", "how": "typed"}


def test_send_keys_forwards_every_key(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "send-keys", "p1", "ctrl+c", "enter", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert fake_floor_backend.keys == [("p1", ["ctrl+c", "enter"])]


def test_close_reports_the_pane(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "close", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert fake_floor_backend.closed == ["p1"]


def test_wait_that_times_out_exits_one_and_says_the_state_it_saw(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "wait",
            "p1",
            "--until",
            "blocked",
            "--timeout",
            "0.2",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 1
    assert "timed out" in result.output and "working" in result.output


def test_attach_on_a_non_coppice_backend_teaches_instead_of_pretending(
    tmp_path, fake_floor_backend
):
    result = runner.invoke(app, ["coppice", "attach", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "fake" in result.output and "coppice" in result.output


def test_attach_on_the_coppice_backend_execs_with_its_own_socket_and_data_dir(
    tmp_path, monkeypatch
):
    """The exec path is tested by replacing the exec call, never by running the
    real binary inside the test process. F15: a non-default socket or data dir
    must reach the binary too, not just the default."""
    from opendaisugi.floor import registry

    picked_sock_path = tmp_path / "coppice" / "server.sock"
    picked_data_dir = tmp_path / "coppice"

    class Coppice:
        name = "coppice"
        yields_frames = True
        sock_path = picked_sock_path
        data_dir = picked_data_dir

        def available(self):
            return True

    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: Coppice()
    )
    calls: list[list[str]] = []
    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr(os, "execvp", lambda file, args: calls.append(list(args)))
    result = runner.invoke(app, ["coppice", "attach", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert calls == [
        [
            "coppice",
            "--socket",
            str(picked_sock_path),
            "--data-dir",
            str(picked_data_dir),
            "attach",
            "p1",
        ]
    ]


def _coppice_backend_stub(monkeypatch, tmp_path):
    """Pick a coppice backend with a known socket and data dir, never a
    real server."""
    from opendaisugi.floor import registry

    class Coppice:
        name = "coppice"
        yields_frames = True
        sock_path = tmp_path / "server.sock"
        data_dir = tmp_path

        def available(self):
            return True

    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: Coppice()
    )


def test_daisugi_coppice_attach_execs_the_binary(tmp_path, monkeypatch):
    """attach replaces this process with the resolved coppice binary, so the
    terminal is inherited. The file handed to exec is the path which found,
    and argv[0] stays the bare name the binary expects."""
    _coppice_backend_stub(monkeypatch, tmp_path)
    calls: list[tuple[str, list[str]]] = []
    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr(os, "execvp", lambda file, args: calls.append((file, list(args))))
    result = runner.invoke(app, ["coppice", "attach", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert calls == [
        (
            "/usr/bin/coppice",
            [
                "coppice",
                "--socket",
                str(tmp_path / "server.sock"),
                "--data-dir",
                str(tmp_path),
                "attach",
                "w1:p1",
            ],
        )
    ]


def test_daisugi_coppice_attach_says_when_coppice_is_not_on_path(tmp_path, monkeypatch):
    """No binary on PATH is unreachable, exit 3, with the build command. exec
    is never attempted."""
    _coppice_backend_stub(monkeypatch, tmp_path)
    monkeypatch.setattr(shutil, "which", lambda name: None)

    def never(file, args):
        raise AssertionError("execvp must not run when coppice is not on PATH")

    monkeypatch.setattr(os, "execvp", never)
    result = runner.invoke(app, ["coppice", "attach", "w1:p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert "coppice is not on PATH" in result.output
    assert "go build" in result.output


def test_daisugi_coppice_floor_execs_the_binary_with_no_verb(tmp_path, monkeypatch):
    """floor replaces this process with the coppice binary and no verb: the
    binary with no arguments is the floor. The socket and data dir are the
    backend's own, so the floor lands on the same server every other verb
    reaches."""
    _coppice_backend_stub(monkeypatch, tmp_path)
    calls: list[tuple[str, list[str]]] = []
    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr(os, "execvp", lambda file, args: calls.append((file, list(args))))
    result = runner.invoke(app, ["coppice", "floor", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert calls == [
        (
            "/usr/bin/coppice",
            [
                "coppice",
                "--socket",
                str(tmp_path / "server.sock"),
                "--data-dir",
                str(tmp_path),
            ],
        )
    ]


def test_daisugi_coppice_floor_says_when_coppice_is_not_on_path(tmp_path, monkeypatch):
    """No binary on PATH is unreachable, exit 3, with the build command. exec
    is never attempted."""
    _coppice_backend_stub(monkeypatch, tmp_path)
    monkeypatch.setattr(shutil, "which", lambda name: None)

    def never(file, args):
        raise AssertionError("execvp must not run when coppice is not on PATH")

    monkeypatch.setattr(os, "execvp", never)
    result = runner.invoke(app, ["coppice", "floor", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert "coppice is not on PATH" in result.output
    assert "go build" in result.output


def test_daisugi_coppice_floor_on_tmux_names_the_coppice_binary(tmp_path, monkeypatch):
    """The floor is the coppice binary and nothing else. On another backend
    the message must not name a command that does not exist, such as
    `tmux floor`."""
    from opendaisugi.floor import registry

    class Tmux:
        name = "tmux"
        yields_frames = False

        def available(self):
            return True

    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: Tmux()
    )
    result = runner.invoke(app, ["coppice", "floor", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "The floor is the coppice binary." in result.output
    assert "coppice server start" in result.output
    assert "--backend coppice" in result.output
    assert "tmux floor" not in result.output


def test_attach_exits_three_when_the_coppice_binary_is_not_on_path(tmp_path, monkeypatch):
    """A backend that answered on its socket does not mean the CLI binary itself
    is on PATH. exec's own OSError must not surface as a raw traceback."""
    from opendaisugi.floor import registry

    class Coppice:
        name = "coppice"
        yields_frames = True
        sock_path = tmp_path / "server.sock"
        data_dir = tmp_path

        def available(self):
            return True

    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: Coppice()
    )

    def missing(file, args):
        raise FileNotFoundError(file)

    # which finds it, so the guard passes and exec's own failure is the path
    # under test here.
    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/coppice")
    monkeypatch.setattr(os, "execvp", missing)
    result = runner.invoke(app, ["coppice", "attach", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert "coppice" in result.output


def test_every_coppice_command_is_reachable_from_help_all():
    result = runner.invoke(app, ["help", "--all"])
    for verb in (
        "spawn",
        "list",
        "prompt",
        "wait",
        "read",
        "send-keys",
        "close",
        "attach",
        "backends",
    ):
        assert f"coppice {verb}" in result.stdout


# --- fix round 1: no verb may leak a raw traceback -------------------------


def test_spawn_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom(**kwargs):
        raise RuntimeError("tmux pane creation failed: no such session")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_prompt_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom(*args, **kwargs):
        raise RuntimeError("send-keys failed: no such pane")

    monkeypatch.setattr(fake_floor_backend, "send_text", boom)
    result = runner.invoke(app, ["coppice", "prompt", "p1", "hi", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_send_keys_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom(*args, **kwargs):
        raise RuntimeError("unknown key")

    monkeypatch.setattr(fake_floor_backend, "send_keys", boom)
    result = runner.invoke(app, ["coppice", "send-keys", "p1", "zzz", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_close_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom(*args, **kwargs):
        raise RuntimeError("no such pane")

    monkeypatch.setattr(fake_floor_backend, "close", boom)
    result = runner.invoke(app, ["coppice", "close", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_list_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom():
        raise RuntimeError("tmux list-panes failed")

    monkeypatch.setattr(fake_floor_backend, "list", boom)
    result = runner.invoke(app, ["coppice", "list", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_json_with_a_backend_raised_error_leaves_stdout_empty(
    tmp_path, monkeypatch, fake_floor_backend
):
    """The error message belongs on stderr. A --json caller reads stdout
    and expects either valid JSON or nothing, never a mix of an error
    line and no JSON."""

    def boom():
        raise RuntimeError("tmux list-panes failed")

    monkeypatch.setattr(fake_floor_backend, "list", boom)
    result = runner.invoke(app, ["coppice", "list", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert result.stdout == ""


def test_read_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom(*args, **kwargs):
        raise RuntimeError("capture-pane failed")

    monkeypatch.setattr(fake_floor_backend, "read", boom)
    result = runner.invoke(app, ["coppice", "read", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_wait_raising_runtime_error_exits_one_with_no_traceback(
    tmp_path, monkeypatch, fake_floor_backend
):
    def boom():
        raise RuntimeError("tmux list-panes failed")

    monkeypatch.setattr(fake_floor_backend, "list", boom)
    result = runner.invoke(
        app, ["coppice", "wait", "p1", "--until", "working", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "Traceback" not in result.output
    assert "daisugi coppice list" in result.output


def test_an_os_error_from_the_backend_exits_three(tmp_path, monkeypatch, fake_floor_backend):
    def boom(**kwargs):
        raise ConnectionError("tmux server not running")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "Traceback" not in result.output
    assert "The backend is unreachable." in result.output


def test_a_dropped_coppice_connection_exits_three(tmp_path, monkeypatch, fake_floor_backend):
    """A CoppiceError for a connection the server dropped mid-call is an
    unreachable host, not a user error, and must exit 3 like any other
    transport failure."""
    from opendaisugi.floor.coppice_backend import CoppiceError

    def boom(**kwargs):
        raise CoppiceError("internal", "coppice closed the connection on pane.spawn")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "Traceback" not in result.output


def test_a_coppice_server_closed_reply_exits_three(tmp_path, monkeypatch, fake_floor_backend):
    """The server can answer `ok:false` with code server_closed while it is
    mid-shutdown, without the socket itself dropping first. That is a
    dropped connection just the same, and must exit 3 like any other
    transport failure, not 1 like a request the server understood and
    refused."""
    from opendaisugi.floor.coppice_backend import CoppiceError

    def boom(**kwargs):
        raise CoppiceError("server_closed", "coppice-server is shutting down")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "Traceback" not in result.output


def test_a_coppice_unparseable_reply_exits_three(tmp_path, monkeypatch, fake_floor_backend):
    """`_call` raises code internal with 'is not JSON' when the server writes
    a partial line and dies mid-write. That is a dropped socket too, same as
    the connection-closed message, and must exit 3, not 1."""
    from opendaisugi.floor.coppice_backend import CoppiceError

    def boom(**kwargs):
        raise CoppiceError("internal", "coppice sent a reply that is not JSON: '{\"pa'")

    monkeypatch.setattr(fake_floor_backend, "spawn", boom)
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 3
    assert "Traceback" not in result.output


def test_a_coppice_bad_request_error_still_exits_one(tmp_path, monkeypatch, fake_floor_backend):
    """A CoppiceError that is not a dropped connection is still a user
    error: the request itself was bad, not the transport."""
    from opendaisugi.floor.coppice_backend import CoppiceError

    def boom(pane, *, source="visible"):
        raise CoppiceError("bad_request", "no such source")

    monkeypatch.setattr(fake_floor_backend, "read", boom)
    result = runner.invoke(app, ["coppice", "read", "p1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "Traceback" not in result.output


# --- fix round 1: the three --json branches the first round left untested --


def test_send_keys_json_carries_the_pane_and_keys(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "send-keys", "p1", "enter", "--json", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    assert json.loads(result.stdout) == {"pane": "p1", "keys": ["enter"]}


def test_close_json_reports_the_pane_closed(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "close", "p1", "--json", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert json.loads(result.stdout) == {"pane": "p1", "closed": True}


def test_wait_json_carries_the_pane_state_and_source(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "wait", "p1", "--until", "working", "--json", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0
    body = json.loads(result.stdout)
    assert body == {"pane": "p1", "state": "working", "source": "gate"}


# --- fix round 1: an unknown --until is refused before polling -------------


def test_an_unknown_until_state_exits_one_and_names_the_real_ones(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "wait", "p1", "--until", "blocekd", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "idle, working, blocked, done, any" in result.output


# --- fix round 1: --socket overrides floor.coppice_socket for one call -----


def test_socket_overrides_coppice_socket_on_the_config_pick_backend_receives(tmp_path, monkeypatch):
    from opendaisugi.floor import registry

    seen: list = []

    class Fake:
        name = "fake"
        yields_frames = False

        def available(self):
            return True

        def list(self):
            return []

    def picked(config, *, name=None, autostart=False):
        seen.append(config)
        return Fake()

    monkeypatch.setattr(registry, "pick_backend", picked)
    sock = tmp_path / "custom.sock"
    result = runner.invoke(
        app, ["coppice", "list", "--data-dir", str(tmp_path), "--socket", str(sock)]
    )
    assert result.exit_code == 0
    assert seen and seen[0].floor.coppice_socket == str(sock)


def _write_coppice_toml(tmp_path, text: str) -> None:
    d = tmp_path / "coppice"
    d.mkdir(parents=True, exist_ok=True)
    (d / "coppice.toml").write_text(text, encoding="utf-8")


def test_spawn_with_harness_and_no_argv_reads_the_coppice_config(
    tmp_path, monkeypatch, fake_floor_backend
):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.fakeh]\ncommand = "sh"\nargs = ["-c", "sleep 30"]\n')
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--harness",
            "fakeh",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 0, result.output
    spawned = fake_floor_backend.spawned[0]
    assert spawned["cmd"] == ["sh", "-c", "sleep 30"]
    assert spawned["label"] == "fakeh"
    assert spawned["harness"] == "fakeh"


def test_spawn_with_harness_and_no_argv_and_no_config_teaches_the_file(
    tmp_path, monkeypatch, fake_floor_backend
):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--harness",
            "nope",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 1
    assert "no harness named 'nope' in" in result.output
    assert "coppice.toml" in result.output
    assert "[harness.nope]" in result.output
    assert fake_floor_backend.spawned == [], "nothing may spawn without a command"


def test_spawn_with_no_argv_and_no_harness_teaches_both_forms(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "spawn", "--cwd", str(tmp_path), "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "spawn needs a command or --harness NAME." in result.output
    assert fake_floor_backend.spawned == []


def test_spawn_argv_still_wins_over_the_config(tmp_path, monkeypatch, fake_floor_backend):
    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.fakeh]\ncommand = "sh"\n')
    result = runner.invoke(
        app,
        [
            "coppice",
            "spawn",
            "--cwd",
            str(tmp_path),
            "--harness",
            "fakeh",
            "--data-dir",
            str(tmp_path),
            "--",
            "echo",
            "hi",
        ],
    )
    assert result.exit_code == 0, result.output
    assert fake_floor_backend.spawned[0]["cmd"] == ["echo", "hi"]


def test_task_create_passes_every_field_and_prints_the_id(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        [
            "coppice",
            "task",
            "create",
            "--label",
            "gate-refactor",
            "--parent",
            "t1",
            "--cwd",
            str(tmp_path),
            "--worktree",
            "--model",
            "opus",
            "--data-dir",
            str(tmp_path),
        ],
    )
    assert result.exit_code == 0, result.output
    assert "t2" in result.stdout
    assert fake_floor_backend.tasks_created == [
        {
            "label": "gate-refactor",
            "parent": "t1",
            "cwd": str(tmp_path),
            "worktree": True,
            "model": "opus",
        }
    ]


def test_task_create_json_is_the_task_info(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        ["coppice", "task", "create", "--label", "x", "--json", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    row = json.loads(result.stdout)
    assert row["task"] == "t2"
    assert row["label"] == "x"
    assert row["worktree"] == ""
    assert fake_floor_backend.tasks_created[0]["worktree"] is False
    assert fake_floor_backend.tasks_created[0]["cwd"] is None


def test_task_list_prints_every_task_and_its_state(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "task", "list", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert "t1" in result.stdout
    assert "review-team" in result.stdout
    assert "blocked" in result.stdout
    result = runner.invoke(app, ["coppice", "task", "list", "--json", "--data-dir", str(tmp_path)])
    rows = json.loads(result.stdout)
    assert rows[0]["task"] == "t1"
    assert rows[0]["panes"] == ["p1"]


def test_task_close_passes_keep_worktree(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "task", "close", "t1", "--keep-worktree", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 0, result.output
    assert "closed t1" in result.stdout
    assert fake_floor_backend.tasks_closed == [("t1", True)]
    result = runner.invoke(
        app, ["coppice", "task", "close", "t1", "--json", "--data-dir", str(tmp_path)]
    )
    assert json.loads(result.stdout) == {"task": "t1", "closed": True}
    assert fake_floor_backend.tasks_closed[-1] == ("t1", False)


def test_spawn_with_a_task_needs_no_cwd_and_passes_the_task(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app,
        ["coppice", "spawn", "--task", "t1", "--data-dir", str(tmp_path), "--", "claude"],
    )
    assert result.exit_code == 0, result.output
    assert fake_floor_backend.spawned[0]["task"] == "t1"
    assert fake_floor_backend.spawned[0]["cwd"] is None


def test_spawn_without_cwd_or_task_exits_one_and_teaches(tmp_path, fake_floor_backend):
    result = runner.invoke(app, ["coppice", "spawn", "--data-dir", str(tmp_path), "--", "claude"])
    assert result.exit_code == 1
    assert "--cwd" in result.output and "--task" in result.output
    assert fake_floor_backend.spawned == []


def test_task_create_resolves_cwd_before_sending(tmp_path, monkeypatch, fake_floor_backend):
    monkeypatch.chdir(tmp_path)
    result = runner.invoke(
        app,
        ["coppice", "task", "create", "--label", "x", "--cwd", ".", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    assert fake_floor_backend.tasks_created[0]["cwd"] == str(tmp_path.resolve())


def test_spawn_hints_never_print_cwd_none(tmp_path, fake_floor_backend):
    result = runner.invoke(
        app, ["coppice", "spawn", "--task", "t1", "--kind", "bogus", "--data-dir", str(tmp_path)]
    )
    assert result.exit_code == 1
    assert "None" not in result.output
    assert "Try:" in result.output
    result = runner.invoke(app, ["coppice", "spawn", "--task", "t1", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "None" not in result.output

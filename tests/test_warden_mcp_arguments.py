# tests/test_warden_mcp_arguments.py
import pytest

from opendaisugi import warden, warden_client
from opendaisugi.models import Envelope, McpRule, Permission


def env(**perm):
    base = {"file_read": ["/work/**"], "mcp_allowlist": [], "mcp_rules": []}
    base.update(perm)
    return Envelope(
        generated_by="test",
        task="t",
        stakes="medium",
        permissions=Permission(**base),
        invariants=[],
        postconditions=[],
    )


def decide(envelope, payload, fmt="claude"):
    rec = warden._payload_to_record(payload, fmt=fmt)
    return warden.evaluate_record(
        rec,
        envelope,
        mode="enforce",
        verify_timeout_s=5.0,
        root=None,
        cwd="/work",
        call_cwd=payload.get("cwd"),
    )


RULES = [
    McpRule(
        tool="github/get_issue", args=[{"op": "in_set", "path": "repo", "values": ["acme/app"]}]
    )
]


def test_rule_admits_and_denies_at_the_warden():
    e = env(mcp_rules=RULES)
    ok = decide(
        e,
        {
            "tool_name": "mcp__github__get_issue",
            "tool_input": {"repo": "acme/app"},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert ok.allow and not ok.would_deny
    bad = decide(
        e,
        {
            "tool_name": "mcp__github__get_issue",
            "tool_input": {"repo": "acme/app", "body": "x"},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert bad.would_deny and "argument 'body' is not named" in bad.reason


def test_rule_admits_without_allowlist_entry():
    e = env(mcp_rules=RULES, mcp_allowlist=[])
    assert decide(
        e,
        {
            "tool_name": "mcp__github__get_issue",
            "tool_input": {"repo": "acme/app"},
            "session_id": "s",
            "cwd": "/work",
        },
    ).allow


def test_allowlist_only_tool_keeps_name_alone_semantics():
    e = env(mcp_allowlist=["slack/post"])
    assert decide(
        e,
        {
            "tool_name": "mcp__slack__post",
            "tool_input": {"text": "hi", "channel": "c"},
            "session_id": "s",
            "cwd": "/work",
        },
    ).allow


def test_existing_path_shaped_argument_is_a_read(tmp_path):
    secret = tmp_path / "secret"
    secret.write_text("x")
    e = env(mcp_allowlist=["fs/*"], file_read=["/work/**"])
    d = decide(
        e,
        {
            "tool_name": "mcp__fs__list_dir",
            "tool_input": {"path": str(secret)},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert d.would_deny and f"reads {secret} outside file_read" in d.reason
    e2 = env(mcp_allowlist=["fs/*"], file_read=[str(tmp_path) + "/**"])
    assert decide(
        e2,
        {
            "tool_name": "mcp__fs__list_dir",
            "tool_input": {"path": str(secret)},
            "session_id": "s",
            "cwd": "/work",
        },
    ).allow


def test_nonexistent_absolute_string_is_not_a_read():
    e = env(mcp_allowlist=["fs/*"])
    assert decide(
        e,
        {
            "tool_name": "mcp__fs__list_dir",
            "tool_input": {"path": "/nonexistent-w3/x"},
            "session_id": "s",
            "cwd": "/work",
        },
    ).allow


def test_directory_argument_reads_the_tree(tmp_path):
    e = env(mcp_allowlist=["fs/*"], file_read=[str(tmp_path) + "/*"])  # one level only
    d = decide(
        e,
        {
            "tool_name": "mcp__fs__list_dir",
            "tool_input": {"path": str(tmp_path)},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert d.would_deny  # a directory is dir/** and /* does not cover it


def test_nested_and_list_strings_are_scanned(tmp_path):
    f = tmp_path / "a"
    f.write_text("x")
    e = env(mcp_allowlist=["fs/*"])
    d = decide(
        e,
        {
            "tool_name": "mcp__fs__read_many",
            "tool_input": {"opts": {"paths": ["/work/ok", str(f)]}},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert d.would_deny and "argument 'opts.paths[1]'" in d.reason


def test_cap_denies_before_rules():
    e = env(mcp_rules=RULES)
    d = decide(
        e,
        {
            "tool_name": "mcp__github__get_issue",
            "tool_input": {"repo": "x" * (256 * 1024 + 1)},
            "session_id": "s",
            "cwd": "/work",
        },
    )
    assert d.would_deny and "longer than 262144 characters" in d.reason


@pytest.mark.parametrize(
    "fmt,payload",
    [
        (
            "claude",
            {
                "tool_name": "mcp__github__get_issue",
                "tool_input": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
            },
        ),
        (
            "claude",
            {
                "tool_name": "mcp__github__get_issue",
                "tool_input": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
                "hook_event_name": "PreToolUse",
                "model": "gpt-5-codex",
                "turn_id": "turn_1",
                "permission_mode": "default",
                "tool_use_id": "call_1",
            },
        ),  # Codex shape
        (
            "pi",
            {
                "tool_name": "mcp__github__get_issue",
                "tool_input": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
            },
        ),
        (
            "opencode",
            {
                "tool_name": "mcp__opencode__github_get_issue",
                "tool_input": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
            },
        ),
        (
            "hermes",
            {
                "event": "pre_tool",
                "tool": "mcp__github__get_issue",
                "args": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
            },
        ),
        (
            "openclaw",
            {
                "tool": "mcp__github__get_issue",
                "args": {"repo": "evil/app"},
                "session_id": "s",
                "cwd": "/work",
            },
        ),
    ],
)
def test_every_format_delivers_arguments_to_the_rule(fmt, payload):
    rules = RULES + [
        McpRule(
            tool="opencode/github_get_issue",
            args=[{"op": "in_set", "path": "repo", "values": ["acme/app"]}],
        )
    ]
    d = decide(env(mcp_rules=rules), payload, fmt=fmt)
    assert d.would_deny and "in_set" in d.reason


# -- review round 1 ------------------------------------------------------


def call(tool, inp, key="tool_input", **extra):
    return {"tool_name": tool, key: inp, "session_id": "s", "cwd": "/work", **extra}


def rule(tool, *args, known=None):
    return McpRule(tool=tool, args=list(args), known=known)


@pytest.mark.parametrize(
    "regex, value, allowed",
    [
        ("^acme/app$", "acme/app", True),
        ("^acme/app$", "acme/app\n", False),
        ("acme/.*", "acme/app", True),
        ("acme/.*", "evil/acme/app", False),
        ("acme/.*", "acme/app\n", False),
        ("^(?:a|b)$", "b", True),
    ],
)
def test_a_rule_regex_must_match_the_whole_string(regex, value, allowed):
    e = env(mcp_rules=[rule("github/get_issue", {"op": "matches", "path": "repo", "regex": regex})])
    d = decide(e, call("mcp__github__get_issue", {"repo": value}))
    assert d.allow is allowed


def test_not_matches_in_a_rule_also_tests_the_whole_string():
    e = env(
        mcp_rules=[rule("github/get_issue", {"op": "not_matches", "path": "repo", "regex": "evil"})]
    )
    assert decide(e, call("mcp__github__get_issue", {"repo": "evil"})).would_deny
    assert decide(e, call("mcp__github__get_issue", {"repo": "an evil one"})).allow


def test_an_invariant_regex_still_searches():
    from opendaisugi.predicate import Matches
    from opendaisugi.predicate_z3 import _eval_scalar

    assert _eval_scalar(Matches(path="x", regex="b"), {"x": "abc"})


def test_the_lint_refuses_a_top_level_alternation():
    from opendaisugi.mcp_rules import rule_lint

    r = rule("a/b", {"op": "matches", "path": "r", "regex": "^a|.*$"})
    refusals, _ = rule_lint([r], {})
    assert refusals and "not anchored" in refusals[0]
    ok = rule("a/b", {"op": "matches", "path": "r", "regex": "^(?:a|b)$"})
    assert rule_lint([ok], {})[0] == []


@pytest.mark.parametrize("bad", [["--all"], '{"all": 1}', 7, None, [], "", 0, False])
@pytest.mark.parametrize("key", ["tool_input", "args", "input"])
def test_arguments_that_are_no_object_reach_the_rule(bad, key):
    e = env(mcp_rules=[rule("github/list", known=[])])
    d = decide(e, call("mcp__github__list", bad, key=key))
    assert d.would_deny
    assert "arguments are not an object" in d.reason


def test_no_arguments_key_is_a_call_with_none():
    e = env(mcp_rules=[rule("github/list", known=[])])
    payload = {"tool_name": "mcp__github__list", "session_id": "s", "cwd": "/work"}
    assert decide(e, payload).allow
    assert decide(e, call("mcp__github__list", {})).allow


def test_non_object_arguments_to_an_allowlisted_tool_are_still_allowed():
    e = env(mcp_allowlist=["github/list"])
    assert decide(e, call("mcp__github__list", ["x"])).allow


def test_non_object_arguments_deny_under_hermes_and_openclaw():
    e = env(mcp_rules=[rule("github/list", known=[])])
    for fmt in ("hermes", "openclaw"):
        d = decide(e, {"tool": "mcp__github__list", "args": ["x"], "session_id": "s"}, fmt=fmt)
        assert d.would_deny and "arguments are not an object" in d.reason


@pytest.mark.parametrize(
    "path",
    [
        "file:///etc/passwd",
        "C:/Windows/x",
        "c:x",
        "..\\..\\etc\\passwd",
        "a\\b",
        " /etc/passwd",
        "/work/a\n",
        "src/a.py ",
        "\t/work/a",
        "src/a\x00.py",
        "../x",
        "src/../../x",
        "a/..",
    ],
)
def test_path_in_refuses_a_value_a_server_could_read_another_way(path):
    e = env(mcp_rules=[rule("fs/read", {"op": "path_in", "path": "path", "scope": "file_read"})])
    assert decide(e, call("mcp__fs__read", {"path": path})).would_deny


@pytest.mark.parametrize("path", ["/work/src/a.py", "/work/../work/a"])
def test_path_in_still_admits_its_neighbours(path):
    e = env(mcp_rules=[rule("fs/read", {"op": "path_in", "path": "path", "scope": "file_read"})])
    assert decide(e, call("mcp__fs__read", {"path": path})).allow


@pytest.mark.parametrize("path", ["src/a.py", "b/c", "./a", "~/x"])
def test_path_in_refuses_a_relative_value_with_one_message(path):
    """Owner, 2026-11-03: MCP rules take absolute paths only."""
    e = env(mcp_rules=[rule("fs/read", {"op": "path_in", "path": "path", "scope": "file_read"})])
    d = decide(e, call("mcp__fs__read", {"path": path}))
    assert d.would_deny
    assert "path_in takes absolute paths only" in d.reason


def test_a_relative_value_under_not_is_still_refused():
    e = env(
        mcp_rules=[
            rule(
                "fs/read",
                {"op": "not", "child": {"op": "path_in", "path": "path", "scope": "file_read"}},
            )
        ]
    )
    d = decide(e, call("mcp__fs__read", {"path": "src/a.py"}))
    assert d.would_deny and "path_in takes absolute paths only" in d.reason


def test_lint_refuses_path_in_over_a_relative_glob():
    from opendaisugi.mcp_rules import rule_lint

    e = env(
        file_read=["src/**", "/work/**"],
        mcp_rules=[rule("fs/read", {"op": "path_in", "path": "path", "scope": "file_read"})],
    )
    refusals, _ = rule_lint(e.permissions.mcp_rules, {}, e.permissions)
    assert refusals == [
        "mcp_rules: fs/read: path_in on 'path' reads file_read, which holds the relative glob "
        "'src/**'. path_in takes absolute paths only: write the glob from /"
    ]
    assert rule_lint(e.permissions.mcp_rules, {})[0] == []


def test_a_wildcard_allowlist_entry_does_not_admit_a_ruled_tool():
    e = env(
        mcp_allowlist=["opencode/*", "pi/*"],
        mcp_rules=[rule("github/get_issue", {"op": "in_set", "path": "repo", "values": ["a/b"]})],
    )
    d = decide(e, call("mcp__opencode__github_get_issue", {"repo": "evil/x"}), fmt="opencode")
    assert d.would_deny and "argument rule under 'github/get_issue'" in d.reason
    d = decide(e, call("github_get_issue", {"repo": "evil/x"}), fmt="pi")
    assert d.would_deny and "argument rule under 'github/get_issue'" in d.reason
    other = decide(e, call("mcp__opencode__slack_post", {"t": 1}), fmt="opencode")
    assert other.allow
    named = env(
        mcp_allowlist=["opencode/github_get_issue"],
        mcp_rules=[rule("github/get_issue", {"op": "in_set", "path": "repo", "values": ["a/b"]})],
    )
    assert decide(
        named, call("mcp__opencode__github_get_issue", {"repo": "x"}), fmt="opencode"
    ).allow


@pytest.fixture(autouse=False)
def default_sigint():
    """A host that starts a background job may hand the process SIGINT as
    ignored. Put Python's own handler back, so a test signal is delivered."""
    import signal

    old = signal.signal(signal.SIGINT, signal.default_int_handler)
    yield
    signal.signal(signal.SIGINT, old)


def _held_check(monkeypatch, sender):
    """A check that never ends on its own: it asks ``sender`` to signal the
    process, then waits. The signal is sent from inside the check, so there is
    no timing race."""
    import time as _time

    def slow(*a, **k):
        sender()
        _time.sleep(10)

    monkeypatch.setattr(warden, "warden_and_contract", slow)


def _stdin(monkeypatch, data=b"{}"):
    import io

    monkeypatch.setattr("sys.stdin", io.TextIOWrapper(io.BytesIO(data)))


def test_sigint_during_a_check_denies_with_exit_2(monkeypatch, default_sigint):
    import os
    import signal

    _held_check(monkeypatch, lambda: os.kill(os.getpid(), signal.SIGINT))
    out = warden.run_argv(["--mode", "enforce"], b"{}")
    assert out.exit_code == 2 and "DENIED" in out.stderr


def test_sigterm_during_a_check_denies_with_exit_2(monkeypatch, capsys):
    import os
    import signal

    _held_check(monkeypatch, lambda: os.kill(os.getpid(), signal.SIGTERM))
    _stdin(monkeypatch)
    before = signal.getsignal(signal.SIGTERM)
    code = warden.main(["--mode", "enforce"])
    assert code == 2
    assert "DENIED" in capsys.readouterr().err
    assert signal.getsignal(signal.SIGTERM) is before


@pytest.mark.parametrize("sig", ["SIGTERM", "SIGINT"])
def test_a_signal_during_a_check_in_audit_mode_allows_with_exit_0(
    monkeypatch, capsys, default_sigint, sig
):
    import os
    import signal

    num = getattr(signal, sig)
    _held_check(monkeypatch, lambda: os.kill(os.getpid(), num))
    _stdin(monkeypatch)
    assert warden.main(["--mode", "audit"]) == 0
    assert "DENIED" not in capsys.readouterr().err


def test_a_signal_while_the_mode_is_read_keeps_the_argv_mode(monkeypatch, capsys, default_sigint):
    import os
    import signal

    def interrupted(argv, fallback):
        os.kill(os.getpid(), signal.SIGINT)
        raise AssertionError("not reached")

    monkeypatch.setattr(warden, "_resolved_mode", interrupted)
    _stdin(monkeypatch)
    assert warden.main(["--mode", "audit"]) == 0
    _stdin(monkeypatch)
    assert warden.main(["--mode", "enforce"]) == 2
    _stdin(monkeypatch)
    assert warden.main([]) == 2


def test_a_keyboard_interrupt_out_of_parse_args_denies(monkeypatch, capsys):
    def boom(argv):
        raise KeyboardInterrupt

    monkeypatch.setattr(warden, "_resolved_mode", lambda a, f: f)
    monkeypatch.setattr(
        warden, "_build_parser", lambda: type("P", (), {"parse_args": staticmethod(boom)})()
    )
    _stdin(monkeypatch)
    assert warden.main(["--mode", "enforce"]) == 2
    assert "DENIED" in capsys.readouterr().err


def test_the_installed_client_entry_gives_a_verdict_on_a_signal(tmp_path, default_sigint):
    """The hook command runs ``warden_client``. A resident socket that takes
    the request and never answers holds it; a signal then ends in the mode's
    verdict. Python resets a handler it installed to SIG_DFL across exec,
    while SIG_IGN is inherited, so the ``default_sigint`` fixture (a Python
    handler in this process) leaves the child with SIGINT at its default,
    whatever the job control of the box left. No ``preexec_fn``: it is unsafe
    beside the ``hold`` thread. The signal goes out once the socket has the
    request, so nothing waits on a clock."""
    import os
    import signal
    import socket
    import subprocess
    import sys
    import threading

    root = tmp_path / "root"
    root.mkdir(mode=0o700)
    sock_path = root / "warden.sock"
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock_path))
    os.chmod(sock_path, 0o600)
    srv.listen(1)
    got = threading.Event()
    held: list[socket.socket] = []

    def hold():
        while True:
            try:
                conn, _ = srv.accept()
            except OSError:
                return
            held.append(conn)
            conn.recv(65536)
            got.set()

    threading.Thread(target=hold, daemon=True).start()

    def run(mode, sig):
        got.clear()
        proc = subprocess.Popen(
            [
                sys.executable,
                "-I",
                "-m",
                "opendaisugi.warden_client",
                "--mode",
                mode,
                "--root",
                str(root),
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env={**os.environ, "HOME": str(tmp_path)},
        )
        assert proc.stdin is not None
        proc.stdin.write(b"{}")
        proc.stdin.close()
        proc.stdin = None  # communicate() must not flush a closed pipe
        assert got.wait(30)
        proc.send_signal(sig)
        out, err = proc.communicate(timeout=30)
        return proc.returncode, out, err

    try:
        for sig in (signal.SIGTERM, signal.SIGINT):
            code, _, err = run("enforce", sig)
            assert code == 2 and b"DENIED" in err, (sig, code, err)
            code, _, err = run("audit", sig)
            assert code == 0 and b"DENIED" not in err, (sig, code, err)
    finally:
        for c in held:
            c.close()
        srv.close()


def _interrupting_main(monkeypatch):
    def boom(argv):
        raise KeyboardInterrupt

    monkeypatch.setattr(warden_client, "_main", boom)


def test_the_client_escape_reads_the_full_mode_not_only_argv(tmp_path, monkeypatch, capsys):
    """A hook command with no ``--mode`` takes its mode from the config. A
    signal during the check gives that mode's verdict, as the Go and Rust
    parents do (F4)."""
    root = tmp_path / "data" / "warden"
    root.mkdir(parents=True)
    _interrupting_main(monkeypatch)
    (tmp_path / "data" / "config.yaml").write_text("warden_mode: audit\n")
    assert warden_client.main(["--root", str(root)]) == 0
    (tmp_path / "data" / "config.yaml").write_text("warden_mode: enforce\n")
    assert warden_client.main(["--root", str(root)]) == 2
    # An explicit --mode still wins over the config.
    assert warden_client.main(["--mode", "enforce", "--root", str(root)]) == 2
    err = capsys.readouterr().err
    assert "DENIED" in err


def test_the_client_escape_falls_back_to_argv_when_the_mode_cannot_be_read(monkeypatch, capsys):
    _interrupting_main(monkeypatch)

    def broken(argv, fallback):
        raise RuntimeError("no mode")

    monkeypatch.setattr(warden, "_resolved_mode", broken)
    assert warden_client.main(["--mode", "audit"]) == 0
    assert warden_client.main(["--mode", "enforce"]) == 2
    assert warden_client.main([]) == 2


def test_the_deny_after_a_signal_carries_a_reason(monkeypatch, capsys):
    _interrupting_main(monkeypatch)
    assert warden_client.main(["--mode", "enforce"]) == 2
    err = capsys.readouterr().err
    reason = err.split("DENIED (fail-closed on error):", 1)[1].strip()
    assert reason, err
    assert "signal" in reason


def test_a_deny_after_an_exception_with_no_text_carries_the_type():
    out = warden._escape_outcome("enforce", ValueError(), fmt="claude")
    assert out.stderr.rstrip().endswith("ValueError")
    assert "ValueError" in out.decision.reason

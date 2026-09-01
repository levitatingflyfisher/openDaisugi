"""The gate's own door: no agent answers an ask, whatever its envelope says.

A pane can run ``coppice agent allow``, or write the answer file the gate
reads. The coppice server refuses the first from a pane connection. This
file proves the gate refuses both before any envelope check, in every mode.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi import gate
from opendaisugi.gate import DEFAULT_GATE_ROOT, evaluate_call, gate_and_contract, register_envelope
from opendaisugi.models import Envelope, Permission

REFUSAL = "a pane can propose. It cannot allow."


@pytest.fixture
def envelope_that_allows_everything() -> Envelope:
    return Envelope(
        generated_by="test",
        task="allow everything",
        permissions=Permission(
            file_read=["/**"],
            file_write=["/**"],
            shell=True,
            shell_allowlist=["coppice", "echo", "cd", "ls", "python", "x", "cat", "sh"],
            shell_allow_decomposition=True,
        ),
    )


def _bash(command: str, cwd: str = "/work") -> dict:
    return {
        "tool_name": "Bash",
        "tool_input": {"command": command},
        "session_id": "s1",
        "cwd": cwd,
        "hook_event_name": "PreToolUse",
    }


def _write(path: str, tool: str = "Write", cwd: str = "/work") -> dict:
    inp = {"file_path": path, "content": "{}"}
    if tool == "Edit":
        inp = {"file_path": path, "old_string": "a", "new_string": "b"}
    return {"tool_name": tool, "tool_input": inp, "session_id": "s1", "cwd": cwd}


def test_the_gate_denies_coppice_allow_from_any_agent(envelope_that_allows_everything):
    decision = evaluate_call(
        _bash("coppice agent allow w1:p1 ask-3"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False
    assert "cannot allow" in decision.reason


def test_the_gate_leaves_other_coppice_commands_to_the_envelope(envelope_that_allows_everything):
    decision = evaluate_call(
        _bash("coppice pane list"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True


@pytest.mark.parametrize(
    "command",
    [
        "x && coppice agent allow w1:p1 ask-3",
        "/usr/local/bin/coppice --socket /run/s.sock agent allow w1:p1 ask-3",
        "coppice --remote ssh://box agent allow w1:p1 ask-3",
        "sh -c 'coppice agent allow w1:p1 ask-3'",
        'echo \'{"cmd":"agent.allow","pane":"w1:p1","ask":"a"}\' | x',
    ],
)
def test_every_spelling_of_an_allow_is_denied(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    assert decision.allow is False, command
    assert decision.reason == REFUSAL


# A deny is not an allow. A task foreman denies the asks it holds, and
# coppice-server itself lets a pane deny only an ask it holds now, so the
# gate leaves a deny the pane runs itself to the envelope and the server.
@pytest.mark.parametrize(
    "command",
    [
        "coppice agent deny w1:p1 ask-3",
        "x && coppice agent deny w1:p1 h1 --reason 'not this one'",
        'python -c "import socket; s.sendall(b\'{\\"cmd\\":\\"agent.deny\\"}\')"',
    ],
)
def test_a_deny_is_left_to_the_server(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    assert decision.reason != REFUSAL, command
    assert decision.allow is True, command


# The server lets a pane deny only an ask it holds, but it knows a pane only
# by the caller's process tree and session. A deny sent through another
# server, another socket, or a wrapper that starts it outside the pane's
# tree and session reads as the operator's, so the gate refuses it.
@pytest.mark.parametrize(
    "command",
    [
        "coppice --remote ssh://box agent deny w1:p1 ask-3",
        "coppice --remote=ssh://box agent deny w1:p1 ask-3",
        "x && coppice --socket /run/s.sock agent deny w1:p1 h1",
        "XDG_RUNTIME_DIR=/x coppice agent deny w1:p1 h1",
        "ssh box coppice agent deny w1:p1 h1",
        "ssh box 'coppice agent deny w1:p1 h1'",
        "setsid -f coppice agent deny w1:p1 h1",
        "/usr/bin/setsid coppice agent deny w1:p1 h1",
        "systemd-run --user coppice agent deny w1:p1 h1",
        "nohup coppice agent deny w1:p1 h1 &",
        "echo 'coppice agent deny w1:p1 h1' | at now",
        "echo 'coppice agent deny w1:p1 h1' | batch",
        "tmux send-keys -t other 'coppice agent deny w1:p1 h1' Enter",
        "tmux new-session -d 'coppice agent deny w1:p1 h1'",
        "screen -dm coppice agent deny w1:p1 h1",
        "script -qc 'coppice agent deny w1:p1 h1' /dev/null",
        "sudo -u me coppice agent deny w1:p1 h1",
        "daemonize /usr/bin/coppice agent deny w1:p1 h1",
        'echo \'{"cmd": "agent.deny"}\' | ssh box nc -U s',
        'setsid python -c "s.sendall(b\'{\\"cmd\\":\\"agent.deny\\"}\')"',
    ],
)
def test_a_deny_from_outside_the_pane_is_denied(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    assert decision.allow is False, command
    assert decision.reason == REFUSAL, command


@pytest.mark.parametrize(
    "command",
    [
        "ssh box ls",
        "nohup make build &",
        "tmux send-keys -t other 'ls' Enter",
        "coppice --remote ssh://box agent list",
    ],
)
def test_a_wrapper_with_no_deny_is_left_to_the_envelope(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    assert decision.reason != REFUSAL, command


@pytest.mark.parametrize(
    "command",
    [
        "grep -rn agent.allow harness/coppice",
        'python -c "import socket; s.sendall(b\'{\\"cmd\\":\\"agent.allow\\"}\')"',
    ],
)
def test_the_wire_verb_is_caught_only_in_a_request(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    if command.startswith("grep"):
        assert decision.reason != REFUSAL, command
    else:
        assert decision.allow is False, command
        assert decision.reason == REFUSAL


def test_shadow_mode_denies_it_too(envelope_that_allows_everything):
    decision = evaluate_call(
        _bash("coppice agent allow w1:p1 ask-3"), envelope_that_allows_everything, mode="shadow"
    )
    assert decision.allow is False
    assert decision.would_deny is True


@pytest.mark.parametrize("sub", ["asks", "answers"])
@pytest.mark.parametrize("tool", ["Write", "Edit"])
def test_a_write_to_the_ask_files_is_denied(envelope_that_allows_everything, tmp_path, sub, tool):
    root = tmp_path / "gate"
    decision = evaluate_call(
        _write(str(root / sub / "a.json"), tool),
        envelope_that_allows_everything,
        mode="enforce",
        root=root,
    )
    assert decision.allow is False
    assert decision.reason == REFUSAL


def test_a_relative_write_from_inside_the_gate_dir_is_denied(
    envelope_that_allows_everything, tmp_path
):
    root = tmp_path / "gate"
    decision = evaluate_call(
        _write("answers/a.json", cwd=str(root)),
        envelope_that_allows_everything,
        mode="enforce",
        root=root,
    )
    assert decision.allow is False
    assert decision.reason == REFUSAL


def test_a_write_under_the_default_gate_root_is_denied(envelope_that_allows_everything, tmp_path):
    decision = evaluate_call(
        _write(str(DEFAULT_GATE_ROOT / "answers" / "a.json")),
        envelope_that_allows_everything,
        mode="enforce",
        root=tmp_path / "gate",
    )
    assert decision.allow is False


def test_a_write_elsewhere_is_left_to_the_envelope(envelope_that_allows_everything, tmp_path):
    root = tmp_path / "gate"
    decision = evaluate_call(
        _write(str(tmp_path / "notes" / "answers.json")),
        envelope_that_allows_everything,
        mode="enforce",
        root=root,
    )
    assert decision.allow is True


@pytest.mark.parametrize(
    "command",
    [
        "echo x > {root}/answers/a.json",
        "cd {root} && echo x > answers/a.json",
        "echo x > $HOME/.opendaisugi/gate/answers/a.json",
        "echo x > ~/.opendaisugi/gate/asks/a.json",
        "python -c \"open('{root}/answers/x','w')\"",
        "cat x > gate/answers/a.json",
    ],
)
def test_a_shell_write_to_the_ask_files_is_denied(
    envelope_that_allows_everything, tmp_path, command
):
    root = tmp_path / "gate"
    decision = evaluate_call(
        _bash(command.format(root=root)),
        envelope_that_allows_everything,
        mode="enforce",
        root=root,
    )
    assert decision.allow is False, command
    assert decision.reason == REFUSAL


def test_the_deny_is_never_handed_to_the_operator(
    envelope_that_allows_everything, tmp_path, monkeypatch
):
    root = tmp_path / "gate"
    register_envelope(envelope_that_allows_everything, session_id="s1", root=root)
    asked: list[object] = []
    monkeypatch.setattr(gate, "_maybe_ask", lambda *a, **k: asked.append(a) or a[2])
    raw = json.dumps({**_bash("coppice agent allow w1:p1 ask-3"), "tool_use_id": "t1"}).encode()
    out = gate_and_contract(raw, root=root, mode="enforce", ask=True, pin_session="s1")
    assert asked == []
    assert out.exit_code == 2


def test_the_default_root_is_home(tmp_path):
    assert Path.home() / ".opendaisugi" / "gate" == DEFAULT_GATE_ROOT


@pytest.mark.parametrize(
    "command",
    [
        "coppice web token --qr=false",
        "/usr/bin/coppice --data-dir /d web token --rotate",
        "cat ~/.opendaisugi/coppice/web/token",
        "t=$(cat $HOME/.opendaisugi/coppice/web/token)",
        "curl -X POST https://127.0.0.1:8443/api/ask/answer -d '{}'",
    ],
)
def test_the_web_door_is_denied(envelope_that_allows_everything, command):
    decision = evaluate_call(_bash(command), envelope_that_allows_everything, mode="enforce")
    assert decision.allow is False, command
    assert decision.reason == REFUSAL


def test_a_read_of_the_web_token_file_is_denied(envelope_that_allows_everything):
    payload = {
        "tool_name": "Read",
        "tool_input": {"file_path": str(Path.home() / ".opendaisugi/coppice/web/token")},
        "session_id": "s1",
        "cwd": "/work",
    }
    decision = evaluate_call(payload, envelope_that_allows_everything, mode="enforce")
    assert decision.allow is False
    assert decision.reason == REFUSAL


def test_coppice_web_serve_is_left_to_the_envelope(envelope_that_allows_everything):
    decision = evaluate_call(
        _bash("coppice web cert show"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.reason != REFUSAL


@pytest.mark.parametrize(
    "arguments",
    [
        {"path": "{root}/answers/a.json", "content": "{}"},
        {"edits": [{"file": "{root}/asks/a.json"}]},
        {"url": "https://127.0.0.1:8443/api/ask/answer"},
    ],
)
def test_an_mcp_tool_that_names_the_ask_files_is_denied(
    envelope_that_allows_everything, tmp_path, arguments
):
    root = tmp_path / "gate"
    args = json.loads(json.dumps(arguments).replace("{root}", str(root)))
    payload = {
        "tool_name": "mcp__filesystem__write_file",
        "tool_input": args,
        "session_id": "s1",
        "cwd": "/work",
    }
    decision = evaluate_call(payload, envelope_that_allows_everything, mode="enforce", root=root)
    assert decision.allow is False
    assert decision.reason == REFUSAL


def test_an_mcp_tool_elsewhere_is_left_to_the_envelope(envelope_that_allows_everything, tmp_path):
    payload = {
        "tool_name": "mcp__filesystem__write_file",
        "tool_input": {"path": str(tmp_path / "notes.txt")},
        "session_id": "s1",
    }
    decision = evaluate_call(
        payload, envelope_that_allows_everything, mode="enforce", root=tmp_path / "gate"
    )
    assert decision.reason != REFUSAL


# Every secret a coppice data directory holds, as a path relative to that
# data directory: the web token, the local CA's own key and the leaf key
# it signs, the private key of a tailscale certificate coppice writes, and
# the voice server's bearer token.
_COPPICE_SECRETS = [
    "web/token",
    "web/ca/ca.key",
    "web/ca/leaf.key",
    "web/tls/tailscale.key",
    "voice/token",
]

# The public or config counterpart beside each secret above: a certificate
# nobody needs to keep private, and web.json, which holds a path to a
# token file, not a token.
_COPPICE_PUBLIC_FILES = [
    "web/ca/ca.crt",
    "web/ca/leaf.crt",
    "web/ca/meta.json",
    "web/tls/tailscale.crt",
    "web/web.json",
]

_DATA_DIR = Path.home() / ".opendaisugi" / "coppice"


def _read(path: str, cwd: str = "/work") -> dict:
    return {
        "tool_name": "Read",
        "tool_input": {"file_path": path},
        "session_id": "s1",
        "cwd": cwd,
    }


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_read_of_every_coppice_secret_is_denied(envelope_that_allows_everything, secret):
    decision = evaluate_call(
        _read(str(_DATA_DIR / secret)), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_shell_cat_of_every_coppice_secret_is_denied(envelope_that_allows_everything, secret):
    decision = evaluate_call(
        _bash(f"cat {_DATA_DIR / secret}"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_shell_redirect_read_of_every_coppice_secret_is_denied(
    envelope_that_allows_everything, secret
):
    decision = evaluate_call(
        _bash(f"python steal.py < {_DATA_DIR / secret}"),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_cp_of_every_coppice_secret_as_its_source_is_denied(
    envelope_that_allows_everything, secret, tmp_path
):
    decision = evaluate_call(
        _bash(f"cp {_DATA_DIR / secret} {tmp_path / 'out'}"),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_every_coppice_secret_is_denied_from_inside_the_data_dir(
    envelope_that_allows_everything, secret
):
    # The absolute spelling, read with cwd set to the data directory itself.
    decision = evaluate_call(
        _read(str(_DATA_DIR / secret), cwd=str(_DATA_DIR)),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL
    # The relative spelling, typed from cwd set to the data directory itself.
    decision = evaluate_call(
        _bash(f"cat {secret}", cwd=str(_DATA_DIR)),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_an_mcp_tool_that_names_a_coppice_secret_is_denied(envelope_that_allows_everything, secret):
    payload = {
        "tool_name": "mcp__filesystem__read_file",
        "tool_input": {"path": str(_DATA_DIR / secret)},
        "session_id": "s1",
        "cwd": "/work",
    }
    decision = evaluate_call(payload, envelope_that_allows_everything, mode="enforce")
    assert decision.allow is False, secret
    assert decision.reason == REFUSAL


def _respellings(secret: str) -> list[str]:
    """A few spellings of one secret's relative path that name the same
    file: a doubled slash, a bare ``.`` segment, and a bogus segment a
    ``..`` undoes, each right before the file name."""
    head, _, name = secret.rpartition("/")
    return [f"{head}//{name}", f"{head}/./{name}", f"{head}/bogus/../{name}"]


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_respelled_read_of_every_coppice_secret_is_still_denied(
    envelope_that_allows_everything, secret
):
    for spelling in _respellings(secret):
        path = str(_DATA_DIR) + "/" + spelling
        decision = evaluate_call(_read(path), envelope_that_allows_everything, mode="enforce")
        assert decision.allow is False, (secret, spelling)
        assert decision.reason == REFUSAL, (secret, spelling)


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_respelled_mcp_argument_naming_a_coppice_secret_is_denied(
    envelope_that_allows_everything, secret
):
    for spelling in _respellings(secret):
        path = str(_DATA_DIR) + "/" + spelling
        payload = {
            "tool_name": "mcp__filesystem__read_file",
            "tool_input": {"path": path},
            "session_id": "s1",
            "cwd": "/work",
        }
        decision = evaluate_call(payload, envelope_that_allows_everything, mode="enforce")
        assert decision.allow is False, (secret, spelling)
        assert decision.reason == REFUSAL, (secret, spelling)


def test_a_symlinked_path_to_a_secret_is_still_denied(envelope_that_allows_everything, tmp_path):
    # The typed path names no secret by its own text (it goes through
    # "link", not "web/ca"). Only the resolved spelling does, so this is
    # the one case that needs path_names_secret's resolve() candidate,
    # not just its normpath() one.
    real_dir = tmp_path / "data" / "web" / "ca"
    real_dir.mkdir(parents=True)
    (real_dir / "ca.key").write_text("shh")
    link = tmp_path / "link"
    link.symlink_to(real_dir)
    decision = evaluate_call(
        _read(str(link / "ca.key")), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False
    assert decision.reason == REFUSAL


@pytest.mark.parametrize("public", _COPPICE_PUBLIC_FILES)
def test_a_public_cert_or_config_file_is_left_to_the_envelope(
    envelope_that_allows_everything, public
):
    # The Read tool: nothing else guards this path, so an actual allow
    # proves the pane rule left it alone rather than merely not naming it.
    decision = evaluate_call(
        _read(str(_DATA_DIR / public)), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True, public
    # A shell cat of anything under the coppice data directory is still
    # denied, by the floor rule (this directory is the floor's own), not
    # by the pane rule. Check the reason names the right rule.
    decision = evaluate_call(
        _bash(f"cat {_DATA_DIR / public}"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.reason != REFUSAL, public


def _search(tool: str, path: str | None, cwd: str = "/work", pattern: str = ".") -> dict:
    inp: dict = {"pattern": pattern}
    if path is not None:
        inp["path"] = path
    return {"tool_name": tool, "tool_input": inp, "session_id": "s1", "cwd": cwd}


# Each directory that holds a coppice secret: the data directory itself and
# every directory between it and a secret file.
_SECRET_DIRS = ["", "web", "web/ca", "web/tls", "voice"]


@pytest.mark.parametrize("sub", _SECRET_DIRS)
def test_a_grep_of_a_directory_that_holds_a_secret_is_denied(envelope_that_allows_everything, sub):
    target = str(_DATA_DIR / sub) if sub else str(_DATA_DIR)
    decision = evaluate_call(
        _search("Grep", target), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, sub
    assert decision.reason == REFUSAL, sub


@pytest.mark.parametrize("sub", _SECRET_DIRS)
def test_a_grep_with_no_path_from_inside_a_secret_directory_is_denied(
    envelope_that_allows_everything, sub
):
    here = str(_DATA_DIR / sub) if sub else str(_DATA_DIR)
    decision = evaluate_call(
        _search("Grep", None, cwd=here), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, sub
    assert decision.reason == REFUSAL, sub


def test_a_respelled_grep_of_the_web_dir_is_denied(envelope_that_allows_everything):
    for spelling in ("web/", "web//", "web/./", "web/bogus/.."):
        decision = evaluate_call(
            _search("Grep", str(_DATA_DIR) + "/" + spelling),
            envelope_that_allows_everything,
            mode="enforce",
        )
        assert decision.allow is False, spelling
        assert decision.reason == REFUSAL, spelling


def test_a_relative_grep_of_the_web_dir_from_the_data_dir_is_denied(
    envelope_that_allows_everything,
):
    decision = evaluate_call(
        _search("Grep", "web", cwd=str(_DATA_DIR)), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False
    assert decision.reason == REFUSAL


def test_a_grep_of_an_unrelated_dir_is_left_to_the_envelope(envelope_that_allows_everything):
    decision = evaluate_call(
        _search("Grep", "/work/src/web"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True
    decision = evaluate_call(
        _search("Grep", None, cwd="/work/web"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True


@pytest.mark.parametrize("sub", _SECRET_DIRS)
def test_a_glob_of_a_secret_directory_lists_names_and_is_left_to_the_envelope(
    envelope_that_allows_everything, sub
):
    # Glob prints file names, never their contents, so it reads no secret.
    target = str(_DATA_DIR / sub) if sub else str(_DATA_DIR)
    decision = evaluate_call(
        _search("Glob", target, pattern="*"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.reason != REFUSAL, sub
    decision = evaluate_call(
        _search("Glob", None, cwd=target, pattern="*"),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.reason != REFUSAL, sub


def test_a_read_of_a_public_file_from_inside_the_web_dir_is_left_to_the_envelope(
    envelope_that_allows_everything,
):
    # Read opens the one file its path names, so the cwd does not matter.
    decision = evaluate_call(
        _read(str(_DATA_DIR / "web" / "web.json"), cwd=str(_DATA_DIR / "web")),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is True


_CUSTOM_DATA_DIR = "/srv/elsewhere/cdata"


@pytest.mark.parametrize("secret", _COPPICE_SECRETS)
def test_a_shell_respelling_of_a_custom_data_dirs_secret_is_denied(
    envelope_that_allows_everything, secret, monkeypatch
):
    # The respelling hides the secret's name from the text match, so only
    # knowing the data directory catches it.
    monkeypatch.setenv("COPPICE_DATA_DIR", _CUSTOM_DATA_DIR)
    head, _, name = secret.rpartition("/")
    decision = evaluate_call(
        _bash(f"cd {_CUSTOM_DATA_DIR}/{head} && cat ./{name}"),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is False, secret


def test_a_custom_data_dir_is_unguarded_without_the_variable(
    envelope_that_allows_everything, monkeypatch
):
    monkeypatch.delenv("COPPICE_DATA_DIR", raising=False)
    decision = evaluate_call(
        _bash(f"cd {_CUSTOM_DATA_DIR}/web && cat ./web.json"),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is True


@pytest.mark.parametrize("sub", _SECRET_DIRS)
def test_a_grep_of_a_custom_data_dirs_secret_directory_is_denied(
    envelope_that_allows_everything, sub, monkeypatch
):
    monkeypatch.setenv("COPPICE_DATA_DIR", _CUSTOM_DATA_DIR)
    target = f"{_CUSTOM_DATA_DIR}/{sub}" if sub else _CUSTOM_DATA_DIR
    decision = evaluate_call(
        _search("Grep", target), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, sub
    assert decision.reason == REFUSAL, sub


def test_a_relative_custom_data_dir_is_ignored(envelope_that_allows_everything, monkeypatch):
    # coppice always names an absolute path. A relative one says nothing
    # about where the data is, so it adds no directory.
    monkeypatch.setenv("COPPICE_DATA_DIR", "cdata")
    decision = evaluate_call(
        _search("Grep", "/work/cdata/web"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True


def test_a_resident_call_guards_the_callers_own_data_dir(
    envelope_that_allows_everything, monkeypatch
):
    # The resident gate's own environment is not the pane's. The caller
    # names its data directory on the request, and the gate guards it.
    monkeypatch.delenv("COPPICE_DATA_DIR", raising=False)
    with gate.resident_call(data_dir=_CUSTOM_DATA_DIR):
        decision = evaluate_call(
            _search("Grep", f"{_CUSTOM_DATA_DIR}/web"),
            envelope_that_allows_everything,
            mode="enforce",
        )
    assert decision.allow is False
    assert decision.reason == REFUSAL


SEARCH_REFUSAL = "this search reaches coppice's secrets. Search a narrower directory."

# Directories above the default data directory: a recursive search there
# reads every secret under it.
_ANCESTORS = [str(Path.home()), "/", str(Path.home() / ".opendaisugi")]


@pytest.mark.parametrize("above", _ANCESTORS)
def test_a_grep_rooted_above_the_data_dir_is_denied(envelope_that_allows_everything, above):
    decision = evaluate_call(
        _search("Grep", above), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, above
    assert decision.reason == SEARCH_REFUSAL, above
    decision = evaluate_call(
        _search("Grep", None, cwd=above), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, above
    assert decision.reason == SEARCH_REFUSAL, above


def test_a_respelled_grep_above_the_data_dir_is_denied(envelope_that_allows_everything):
    for spelling in ("~", "~/", "/home/../" + str(Path.home()).lstrip("/"), "//"):
        decision = evaluate_call(
            _search("Grep", spelling), envelope_that_allows_everything, mode="enforce"
        )
        assert decision.reason == SEARCH_REFUSAL, spelling


def test_a_grep_with_a_narrow_path_from_home_is_left_to_the_envelope(
    envelope_that_allows_everything,
):
    # The cwd counts only when the search names no path.
    decision = evaluate_call(
        _search("Grep", "/work/src", cwd=str(Path.home())),
        envelope_that_allows_everything,
        mode="enforce",
    )
    assert decision.allow is True


def test_a_glob_or_read_above_the_data_dir_is_left_to_the_envelope(
    envelope_that_allows_everything,
):
    for p in (
        _search("Glob", str(Path.home()), pattern="*"),
        _search("Glob", None, cwd="/", pattern="**/*"),
        _read(str(Path.home() / "notes.txt"), cwd=str(Path.home())),
    ):
        decision = evaluate_call(p, envelope_that_allows_everything, mode="enforce")
        assert decision.reason != SEARCH_REFUSAL, p


def test_a_grep_above_a_custom_data_dir_is_denied(envelope_that_allows_everything, monkeypatch):
    monkeypatch.setenv("COPPICE_DATA_DIR", _CUSTOM_DATA_DIR)
    decision = evaluate_call(
        _search("Grep", "/srv/elsewhere"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.reason == SEARCH_REFUSAL
    monkeypatch.delenv("COPPICE_DATA_DIR")
    decision = evaluate_call(
        _search("Grep", "/srv/elsewhere"), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is True


@pytest.mark.parametrize(
    "command,cwd",
    [
        ("rg token ~", "/work"),
        ("rg token", str(Path.home())),
        ("rg -uu token /", "/work"),
        ("ag token ~/", "/work"),
        ("grep -r token ~", "/work"),
        ("grep -rn token .", str(Path.home())),
        ("grep -R token", str(Path.home())),
        ("cd ~ && rg token", "/work"),
        ("find ~ -type f -exec cat {} +", "/work"),
        ("find -L / -name token -exec cat {} ;", "/work"),
        ("FOO=1 rg token ~/.opendaisugi", "/work"),
    ],
)
def test_a_shell_search_above_the_data_dir_is_denied(envelope_that_allows_everything, command, cwd):
    decision = evaluate_call(
        _bash(command, cwd=cwd), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.allow is False, command
    assert decision.reason == SEARCH_REFUSAL, command


@pytest.mark.parametrize(
    "command,cwd",
    [
        ("rg token src", "/work"),
        ("grep token ~/notes.txt", "/work"),
        ("grep -r token /work/src", str(Path.home())),
        ("find ~ -name '*.md'", "/work"),
        ("cat ~/notes.txt", "/work"),
    ],
)
def test_a_narrow_or_names_only_shell_search_is_left_to_the_envelope(
    envelope_that_allows_everything, command, cwd
):
    decision = evaluate_call(
        _bash(command, cwd=cwd), envelope_that_allows_everything, mode="enforce"
    )
    assert decision.reason != SEARCH_REFUSAL, command

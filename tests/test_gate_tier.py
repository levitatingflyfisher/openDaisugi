"""Every gate decision carries a tier: silent, undoable, or permanent.

An allowed call is silent. A denied call is undoable when its effect class
is read, write inside the workspace, a network get, or a test run. Every
other denied call is permanent, and so is any call the gate cannot place.
"""

from __future__ import annotations

import json

import pytest

from opendaisugi import effects
from opendaisugi.gate import evaluate_call
from opendaisugi.models import Envelope, Permission

WORKSPACE = "/work/repo"


def deny_all() -> Envelope:
    return Envelope(generated_by="test", task="tier test", permissions=Permission())


def allow_reads() -> Envelope:
    return Envelope(
        generated_by="test",
        task="tier test",
        permissions=Permission(file_read=[WORKSPACE + "/**"]),
    )


def payload_for(tool_or_command: str, *, path: str | None = None, cwd: str = WORKSPACE) -> dict:
    if path is not None:
        return {
            "tool_name": tool_or_command,
            "tool_input": {"file_path": path, "content": "x"},
            "session_id": "s1",
            "cwd": cwd,
        }
    return {
        "tool_name": "Bash",
        "tool_input": {"command": tool_or_command},
        "session_id": "s1",
        "cwd": cwd,
    }


@pytest.fixture(autouse=True)
def _project_dir(monkeypatch):
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", WORKSPACE)
    # WORKSPACE is not on disk. It stands for a repo root that holds .git;
    # the temp-dir tests below check the real rule.
    real = effects._holds_git_dir
    monkeypatch.setattr(effects, "_holds_git_dir", lambda base: base == WORKSPACE or real(base))


needs_parser = pytest.mark.skipif(
    not effects.shell_parser_available(), reason="the shell extra is not installed"
)


def test_a_force_push_is_permanent():
    d = evaluate_call(payload_for("git push --force origin master"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_write_inside_the_workspace_is_undoable():
    d = evaluate_call(payload_for("Write", path="src/x.py"), envelope=deny_all())
    assert d.tier == "undoable"


def test_an_unknown_effect_is_permanent():
    d = evaluate_call(payload_for("some-unknown-binary --flag"), envelope=deny_all())
    assert d.tier == "permanent"


def test_an_allowed_call_is_silent():
    d = evaluate_call(payload_for("Read", path=WORKSPACE + "/a.txt"), envelope=allow_reads())
    assert d.allow is True
    assert d.tier == "silent"


def test_a_shadow_would_deny_still_gets_its_tier():
    d = evaluate_call(payload_for("git push -f"), envelope=deny_all(), mode="shadow")
    assert d.allow is True
    assert d.would_deny is True
    assert d.tier == "permanent"


def test_a_write_outside_the_workspace_is_permanent():
    d = evaluate_call(payload_for("Write", path="/etc/hosts"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_write_that_climbs_out_of_the_workspace_is_permanent():
    d = evaluate_call(payload_for("Write", path="../other/x.py"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_write_with_no_workspace_is_permanent():
    p = payload_for("Write", path="src/x.py")
    del p["cwd"]
    d = evaluate_call(p, envelope=deny_all())
    assert d.tier == "permanent"


def test_a_read_of_a_credential_is_permanent():
    d = evaluate_call(payload_for("Read", path="/home/u/.ssh/id_ed25519"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_denied_read_inside_the_workspace_is_undoable():
    d = evaluate_call(payload_for("Read", path=WORKSPACE + "/src/x.py"), envelope=deny_all())
    assert d.tier == "undoable"


def test_a_denied_read_outside_the_workspace_is_permanent():
    d = evaluate_call(payload_for("Read", path="/etc/hosts"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_web_fetch_is_undoable():
    p = {
        "tool_name": "WebFetch",
        "tool_input": {"url": "https://example.org/"},
        "session_id": "s1",
        "cwd": WORKSPACE,
    }
    d = evaluate_call(p, envelope=deny_all())
    assert d.tier == "undoable"


def test_an_mcp_call_is_permanent():
    p = {"tool_name": "mcp__mail__send", "tool_input": {}, "session_id": "s1", "cwd": WORKSPACE}
    d = evaluate_call(p, envelope=deny_all())
    assert d.tier == "permanent"


def test_an_unrecognized_tool_is_permanent():
    p = {"tool_name": "TotallyNovelTool", "tool_input": {}, "session_id": "s1"}
    d = evaluate_call(p, envelope=deny_all())
    assert d.tier == "permanent"


def test_a_payload_that_is_not_an_object_is_permanent():
    d = evaluate_call("not a payload", envelope=deny_all())
    assert d.tier == "permanent"


@needs_parser
@pytest.mark.parametrize(
    "command",
    [
        "git status",
        "ls -la src",
        "uv run --no-sync pytest -q tests",
        "go test ./...",
        "git add src/x.py && git commit -m wip",
        "mkdir -p build/out",
        "echo hi > notes.txt",
        "curl -sS https://example.org/",
        "git branch topic",
        "git checkout -b topic",
        "sort src/x.py",
        "git diff HEAD~1",
    ],
)
def test_undoable_shell_commands(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "undoable", command


@pytest.mark.parametrize(
    "command",
    [
        "rm -rf build",
        "git push origin master",
        "git push origin +master",
        "git push --force-with-lease",
        "git reset --hard HEAD~1",
        "git checkout -- .",
        "git branch -D topic",
        "git clean -fdx",
        "git restore src/x.py",
        "find . -name '*.pyc' -delete",
        "find . -exec rm {} ;",
        "sed -i s/a/b/ src/x.py",
        "cp a.txt b.txt",
        "mv a.txt b.txt",
        "curl -o out.bin https://example.org/",
        "curl -sSo out.bin https://example.org/",
        "curl -X POST https://example.org/",
        "sudo ls",
        "env FOO=1 ls",
        "FOO=1 ls",
        "xargs rm",
        "sh -c 'ls'",
        "bash -c ls",
        "python -c 'print(1)'",
        "cat ~/.ssh/id_rsa",
        "cat .env",
        "kubectl get pods",
        "terraform apply",
        "npm publish",
        "echo hi > /etc/motd",
        "cd /tmp && echo hi > x.txt",
        "ls $(rm -rf x)",
        "git -C /elsewhere status",
        "unterminated 'quote",
        "sed -n 1p src/x.py",
        "sed s/a/b/e src/x.py",
        "sort -o out.txt in.txt",
        "sort -ro out.txt in.txt",
        "sort --compress-program=sh in.txt",
        "uniq in.txt out.txt",
        "tree -o out.txt",
        "rg --pre sh pattern",
        "git diff --output=/tmp/x",
        "git log --output=x",
        "git show --ext-diff",
        "git grep -Ovim pattern",
        "git grep --open-files-in-pager=sh x",
        "git checkout src/x.py",
        "git checkout main",
        "date -s 2020-01-01",
    ],
)
def test_permanent_shell_commands(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


def test_without_the_shell_extra_every_bash_ask_is_permanent(monkeypatch):
    monkeypatch.setattr(effects, "shell_parser_available", lambda: False)
    d = evaluate_call(payload_for("git status"), envelope=deny_all())
    assert d.tier == "permanent"


def test_the_ask_report_carries_the_tier(tmp_path, monkeypatch):
    from opendaisugi import gate

    seen: list[dict] = []

    def fake_report(root, **kw):
        seen.append(kw)

    monkeypatch.setattr(gate, "_report_and_append_state", fake_report)
    d = evaluate_call(payload_for("Write", path="src/x.py"), envelope=deny_all())
    gate._report_blocked(
        tmp_path,
        payload_for("Write", path="src/x.py"),
        d,
        session_id="s1",
        fmt="claude",
        tool_use_id="toolu_1",
        deadline=10.0,
    )
    assert seen[0]["ask"]["tier"] == "undoable"


def test_the_ask_file_carries_the_tier(tmp_path, monkeypatch):
    from opendaisugi import ask, gate

    monkeypatch.setattr(ask, "operator_present", lambda root: True)
    monkeypatch.setattr(gate, "_report_blocked", lambda *a, **k: None)
    written: list[dict] = []
    real_post = ask.post_ask

    def spy_post(root, **kw):
        written.append(kw["question"])
        return real_post(root, **kw)

    monkeypatch.setattr(ask, "post_ask", spy_post)
    p = payload_for("git push -f")
    p["tool_use_id"] = "toolu_2"
    d = evaluate_call(p, envelope=deny_all(), mode="enforce")
    gate._maybe_ask(tmp_path, p, d, timeout_s=0.0, sleep=lambda s: None)
    assert written[0]["tier"] == "permanent"


def test_the_shadow_log_carries_the_tier(tmp_path):
    from opendaisugi import gate

    d = evaluate_call(payload_for("Write", path="src/x.py"), envelope=deny_all())
    gate._log_shadow(tmp_path, "s1", d)
    rows = [
        json.loads(line) for line in (tmp_path / "shadow" / "s1.jsonl").read_text().splitlines()
    ]
    assert rows[0]["tier"] == "undoable"


def _answered(monkeypatch, reply):
    from opendaisugi import ask, gate

    monkeypatch.setattr(ask, "operator_present", lambda root: True)
    monkeypatch.setattr(gate, "_report_blocked", lambda *a, **k: None)
    monkeypatch.setattr(ask, "wait_answer", lambda root, **kw: reply)


def test_allow_for_the_task_records_a_proposal(tmp_path, monkeypatch):
    from opendaisugi import gate

    _answered(monkeypatch, {"decision": "allow", "scope": "task", "reason": "fine"})
    p = payload_for("Write", path="src/x.py")
    p["tool_use_id"] = "toolu_3"
    d = evaluate_call(p, envelope=deny_all(), mode="enforce")
    out = gate._maybe_ask(tmp_path, p, d, timeout_s=1.0, sleep=lambda s: None)
    assert out.allow is True
    rows = [json.loads(f.read_text()) for f in (tmp_path / "proposals").glob("*.json")]
    assert len(rows) == 1
    assert rows[0]["scope"] == "task"
    assert rows[0]["kind"] == "allow-pattern"
    assert rows[0]["toolUseId"] == "toolu_3"
    assert rows[0]["tier"] == "undoable"


def test_allow_once_records_no_proposal(tmp_path, monkeypatch):
    from opendaisugi import gate

    _answered(monkeypatch, {"decision": "allow", "scope": "once", "reason": "fine"})
    p = payload_for("Write", path="src/x.py")
    p["tool_use_id"] = "toolu_4"
    d = evaluate_call(p, envelope=deny_all(), mode="enforce")
    gate._maybe_ask(tmp_path, p, d, timeout_s=1.0, sleep=lambda s: None)
    assert not list((tmp_path / "proposals").glob("*.json"))


def test_a_permanent_ask_never_becomes_a_task_proposal(tmp_path, monkeypatch):
    from opendaisugi import gate

    _answered(monkeypatch, {"decision": "allow", "scope": "task", "reason": "fine"})
    p = payload_for("git push -f")
    p["tool_use_id"] = "toolu_5"
    d = evaluate_call(p, envelope=deny_all(), mode="enforce")
    gate._maybe_ask(tmp_path, p, d, timeout_s=1.0, sleep=lambda s: None)
    assert not list((tmp_path / "proposals").glob("*.json"))


def test_a_write_with_no_fixed_project_dir_is_permanent(monkeypatch):
    monkeypatch.delenv("CLAUDE_PROJECT_DIR")
    d = evaluate_call(payload_for("Write", path="src/x.py"), envelope=deny_all())
    assert d.tier == "permanent"


def test_a_cd_out_of_the_project_does_not_move_the_workspace():
    # The agent ran cd / in an earlier call, so the hook reports cwd /.
    d = evaluate_call(payload_for("Write", path="etc/hosts", cwd="/"), envelope=deny_all())
    assert d.tier == "permanent"
    d = evaluate_call(
        payload_for("Write", path="/work/repo/src/x.py", cwd="/"), envelope=deny_all()
    )
    assert d.tier == "permanent"


def test_a_cwd_inside_the_project_resolves_relative_paths():
    sub = WORKSPACE + "/pkg"
    d = evaluate_call(payload_for("Write", path="x.py", cwd=sub), envelope=deny_all())
    assert d.tier == "undoable"
    d = evaluate_call(payload_for("Write", path="../x.py", cwd=sub), envelope=deny_all())
    assert d.tier == "undoable"
    d = evaluate_call(payload_for("Write", path="../../x.py", cwd=sub), envelope=deny_all())
    assert d.tier == "permanent"


def test_workspace_root_needs_the_cwd_inside_the_fixed_root():
    assert effects.workspace_root("/a/b", "/a") == effects.Workspace("/a/b", "/a")
    assert effects.workspace_root("/a", "/a/b") is None
    assert effects.workspace_root("/a", "/c") is None
    assert effects.workspace_root("/a", None) is None
    assert effects.workspace_root("rel", "/a") is None


# Each of these runs a program the agent names, writes or truncates a file
# the agent names, deletes, or rewrites a ref, so none may be one key.
PERMANENT_PROBES = [
    "git rebase --exec 'rm -rf ~' HEAD~1",
    "git rebase -x 'rm -rf ~' HEAD~1",
    "git fetch --upload-pack='rm -rf /home/user/x' /some/repo",
    "go test -exec 'rm -rf /home/user/x' ./...",
    "go test -o /tmp/bin ./...",
    "go vet -vettool=/bin/prog ./...",
    "uv run --with=evil pytest",
    "uv run --with evil pytest",
    "curl -D /home/user/.bashrc http://example.com",
    "curl --dump-header out http://example.com",
    "curl -c jar http://example.com",
    "curl --cookie-jar jar http://example.com",
    "curl --trace out http://example.com",
    "curl --trace-ascii out http://example.com",
    "curl --stderr out http://example.com",
    "curl --libcurl out http://example.com",
    "curl --etag-save out http://example.com",
    "curl --hsts out http://example.com",
    "curl --alt-svc out http://example.com",
    "curl -w '%output{out}' http://example.com",
    "find . -fprint0 /home/user/.bashrc",
    "find . -fprint out",
    "find . -delete",
    "find . -exec rm {} +",
    "pytest --junitxml=out.xml",
    "pytest --basetemp=/home/user",
    "python -m pytest --basetemp=x",
    "uv run pytest --basetemp=x",
    "ruff check --output-file out.txt",
    "mypy --junit-xml out.xml",
    "cargo test --target-dir /tmp/t",
    "cargo test -- --logfile out",
    "git branch -df topic",
    "git branch -Dr origin/x",
    "git tag -df v1",
    "git fetch origin +main:main",
    "git fetch --prune",
    "git merge --abort",
    "grep -r token ~",
    "grep -r token /",
    "echo x > .git/HEAD",
    "echo x > .git/objects/pack/pack-X.pack",
    "echo x > .GIT/config",
    "echo x > src/../.git/packed-refs",
    "git switch evil && echo x > link/.bashrc",
    "git checkout -b topic && mkdir -p link/x",
    "git stash pop && echo x > notes.txt",
]


@pytest.mark.parametrize("command", PERMANENT_PROBES)
def test_every_probe_is_permanent(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


@pytest.mark.parametrize("path", [".git/config", ".git/hooks/pre-commit", "sub/.git/HEAD"])
def test_a_write_into_git_is_permanent(path):
    d = evaluate_call(payload_for("Write", path=path), envelope=deny_all())
    assert d.tier == "permanent", path


def _listed_commands():
    out = []
    for head in effects._READ_FLAGS:
        out.append(head)
    for head in effects._MAKE_FLAGS:
        out.append(head + " x")
    for sub in effects._GIT_READ:
        out.append("git " + sub)
    for sub in effects._GIT_WRITE:
        out.append("git " + sub)
    out += [
        "git fetch",
        "git branch",
        "git tag",
        "git stash",
        "git stash list",
        "git stash pop",
        "curl http://example.com",
        "find .",
        "pytest",
        "python -m pytest",
        "uv run pytest",
        "ruff check",
        "ruff format --check",
        "mypy",
        "go test",
        "go vet",
        "cargo test",
    ]
    return out


@needs_parser
@pytest.mark.parametrize("command", _listed_commands())
@pytest.mark.parametrize("flag", ["--zz-unknown", "--zz-unknown=v", "-Z"])
def test_an_unknown_flag_on_a_listed_command_is_permanent(command, flag):
    # go spells long flags with one dash, so -zz-unknown is its unknown flag.
    if command.startswith("go ") and flag == "-Z":
        flag = "-zz-unknown"
    d = evaluate_call(payload_for(f"{command} {flag}"), envelope=deny_all())
    assert d.tier == "permanent", f"{command} {flag}"


@needs_parser
@pytest.mark.parametrize(
    "command",
    [
        "git status -sb",
        "git log --oneline -5",
        "git diff --stat HEAD~1",
        "pytest -q -x -k tier tests",
        "go test -count=1 -run TestX ./...",
        "cargo test -- --nocapture",
        "grep -n tier src/x.py",
        "find src -name '*.py' -type f",
        "curl -sSL -H 'Accept: text/plain' https://example.org/",
        "git stash",
        "git stash list",
    ],
)
def test_listed_commands_with_listed_flags_stay_undoable(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "undoable", command


def test_a_redirect_through_a_symlink_out_of_the_workspace_is_permanent(tmp_path, monkeypatch):
    ws = tmp_path / "ws"
    out = tmp_path / "out"
    (ws / "sub").mkdir(parents=True)
    out.mkdir()
    (ws / "link").symlink_to(out)
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", str(ws))
    for command in ["echo x > link/a", "echo x > sub/../link/a", "echo x > ../out/a"]:
        d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
        assert d.tier == "permanent", command
    d = evaluate_call(payload_for("echo x > sub/a", cwd=str(ws)), envelope=deny_all())
    assert d.tier == "undoable"


# A flag whose value is optional takes it only when attached. The next word
# is never its value, so an output flag after it is still seen.
OPTIONAL_VALUE_PROBES = [
    "git diff -U --output=/home/user/.bashrc",
    "git diff --unified --output=x",
    "git diff --color --output=x",
    "git diff --stat --output=x",
    "git log --pretty --output=x",
    "git show --pretty --output=x",
    "git log --format --output=x",
    "git log --decorate --output=x",
    "git status -u --output=x",
    "git branch --contains --output=x",
    "git describe --abbrev --output=x",
    "git rev-parse --short --output=x",
    "grep --color --output=x pattern",
    "ls --color --output=x",
    "diff --color --output=x a b",
    "echo --output=x",
    "cat --out x",
    "cat -ofile",
    "ls -oout",
]


@pytest.mark.parametrize("command", OPTIONAL_VALUE_PROBES)
def test_an_optional_value_never_swallows_an_output_flag(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
@pytest.mark.parametrize(
    "command",
    ["git diff -U3 HEAD", "git diff --color=never", "git log --pretty=oneline", "git status -uno"],
)
def test_an_attached_optional_value_stays_undoable(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "undoable", command


@pytest.mark.parametrize(
    "command",
    [
        "curl gopher://127.0.0.1:6379/_FLUSHALL",
        "curl telnet://host:23",
        "curl dict://host/x",
        "curl file:///etc/passwd",
        "curl ftp://host/x",
        "curl example.com",
        "curl -sS gophers://host/x",
    ],
)
def test_curl_takes_only_http_and_https(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
def test_curl_http_and_https_stay_undoable():
    for command in ["curl http://example.org/", "curl -s HTTPS://example.org/"]:
        assert evaluate_call(payload_for(command), envelope=deny_all()).tier == "undoable", command


@pytest.mark.parametrize(
    "command",
    [
        "pytest && echo x > link/a",
        "npm test && echo x > notes.txt",
        "go test ./... ; mkdir -p out",
        "uv run --no-sync pytest -q && touch done.txt",
    ],
)
def test_a_test_run_takes_the_workspace_from_later_paths(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


# One case per credential path: each is permanent and names credential.
CREDENTIAL_PROBES = [
    "env",
    "printenv",
    "printenv GH_TOKEN",
    "jq -n env",
    "jq -n '$ENV'",
    "cat /proc/self/environ",
    "cat /proc/1234/environ",
    "cat ~/.config/gh/hosts.yml",
    "cat ~/.ssh/config",
    "cat ~/.aws/credentials",
    "cat ~/.netrc",
    "cat ~/.git-credentials",
    "cat server.pem",
    "cat tls/server.key",
    "cat .env",
    "cat .env.local",
    "curl -H @headers.txt http://host/",
    "curl -H@headers.txt http://host/",
    "curl --header=@headers.txt http://host/",
    "curl -d @body.json http://host/",
    "curl -d@body.json http://host/",
    "curl --data=@body.json http://host/",
]


@pytest.mark.parametrize("command", CREDENTIAL_PROBES)
def test_every_credential_path_is_permanent(command):
    from opendaisugi.gate import evaluate_call as ev

    d = ev(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command
    if effects.shell_parser_available():
        rec = {"step_type": "shell", "command": command}
        assert effects.effect_class(rec, WORKSPACE) == "credential", command


@pytest.mark.parametrize(
    "path",
    [
        "/proc/self/environ",
        "~/.config/gh/hosts.yml",
        "/home/u/.config/x",
        "certs/site.pem",
        "certs/site.key",
    ],
)
def test_a_read_of_a_credential_file_is_permanent(path, monkeypatch):
    monkeypatch.setenv("HOME", "/home/u")
    d = evaluate_call(payload_for("Read", path=path), envelope=deny_all())
    assert d.tier == "permanent", path


# Every path check resolves the path the way the OS will: against the
# call's cwd, with ~ expanded, and ../ and symlinks followed.
CLIMB = "../../home/u"
RESOLVED_CREDENTIAL_PROBES = [
    f"cat {CLIMB}/.config/gh/hosts.yml",
    f"grep -r token {CLIMB}/.config",
    "cat ../../proc/self/environ",
    "cat /proc/self/task/1/environ",
    f"cat {CLIMB}/.ssh/id_ed25519",
    "jq -n 'env|keys'",
    "jq -n '[env]'",
    "jq -n '{a: env}'",
    "jq -n 'env[\"HOME\"]'",
    "jq -n '$ENV.PATH'",
    "git show HEAD:.env",
    "git show HEAD:config/.env",
    "git cat-file -p HEAD:deploy/server.key",
]


@pytest.mark.parametrize("command", RESOLVED_CREDENTIAL_PROBES)
def test_a_resolved_credential_path_is_permanent(command, monkeypatch):
    monkeypatch.setenv("HOME", "/home/u")
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command
    if effects.shell_parser_available():
        rec = {"step_type": "shell", "command": command}
        assert effects.effect_class(rec, WORKSPACE) == "credential", command


@needs_parser
def test_jq_without_env_stays_a_read():
    for command in ["jq -n '.environment'", "jq .envelope data.json", "jq -r .name x.json"]:
        assert evaluate_call(payload_for(command), envelope=deny_all()).tier == "undoable", command


@pytest.mark.parametrize(
    "path", [f"{CLIMB}/.config/gh/hosts.yml", "../../proc/self/environ", "/proc/1/task/1/environ"]
)
def test_a_read_tool_call_on_a_resolved_credential_is_permanent(path, monkeypatch):
    monkeypatch.setenv("HOME", "/home/u")
    d = evaluate_call(payload_for("Read", path=path), envelope=deny_all())
    assert d.tier == "permanent", path


def _linked_workspace(tmp_path, monkeypatch):
    """A workspace with a link to a fake home, and a file that links to a
    fake secret in it."""
    home = tmp_path / "home"
    (home / ".config" / "gh").mkdir(parents=True)
    (home / ".config" / "gh" / "hosts.yml").write_text("token: fake\n")
    (home / ".ssh").mkdir()
    (home / ".ssh" / "id_ed25519").write_text("fake key\n")
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "homelink").symlink_to(home)
    (ws / "notes.txt").symlink_to(home / ".ssh" / "id_ed25519")
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", str(ws))
    return ws


@pytest.mark.parametrize(
    "command",
    [
        "cat homelink/.config/gh/hosts.yml",
        "grep -r token homelink",
        "cat notes.txt",
        "head -1 notes.txt",
    ],
)
def test_a_symlink_to_a_secret_is_permanent(command, tmp_path, monkeypatch):
    ws = _linked_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent", command


@pytest.mark.parametrize("path", ["notes.txt", "homelink/.config/gh/hosts.yml"])
def test_a_read_tool_call_through_a_symlink_to_a_secret_is_permanent(path, tmp_path, monkeypatch):
    ws = _linked_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for("Read", path=path, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent", path


def test_a_cd_is_followed_when_it_names_a_directory(monkeypatch):
    monkeypatch.setenv("HOME", "/home/u")
    d = evaluate_call(payload_for("cd /home/u && cat .config/gh/hosts.yml"), envelope=deny_all())
    assert d.tier == "permanent"
    d = evaluate_call(payload_for("cd / && cat proc/self/environ"), envelope=deny_all())
    assert d.tier == "permanent"
    d = evaluate_call(payload_for("cd /home/u && cat < .config/gh/hosts.yml"), envelope=deny_all())
    assert d.tier == "permanent"


# After a cd or popd the gate cannot follow, a relative path could name
# anything, so it is permanent, a read included.
@pytest.mark.parametrize(
    "command",
    [
        "popd && cat notes.txt",
        "cd - && cat notes.txt",
        "cd -P /x && cat notes.txt",
        "popd; ls src",
        "popd && echo x > notes.txt",
        "popd && cat < notes.txt",
    ],
)
def test_a_relative_path_after_a_lost_cd_is_permanent(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
def test_an_absolute_path_after_a_lost_cd_stays_a_read():
    d = evaluate_call(payload_for("popd && cat /work/repo/notes.txt"), envelope=deny_all())
    assert d.tier == "undoable"


@pytest.mark.parametrize(
    "command",
    [
        "git show HEAD:../secret.txt",
        "git show HEAD:sub/../../x",
        "git cat-file -p main:..",
    ],
)
def test_a_rev_path_with_a_climb_is_permanent(command):
    d = evaluate_call(payload_for(command), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
def test_a_rev_path_without_a_climb_stays_a_read():
    d = evaluate_call(payload_for("git show HEAD:src/x.py"), envelope=deny_all())
    assert d.tier == "undoable"


def _plain_workspace(tmp_path, monkeypatch):
    ws = tmp_path / "ws"
    (ws / "src").mkdir(parents=True)
    (ws / "src" / "x.py").write_text("x = 1\n")
    (tmp_path / "outside.txt").write_text("outside\n")
    home = tmp_path / "home"
    home.mkdir()
    (home / "notes.txt").write_text("notes\n")
    (ws / "homelink").symlink_to(home)
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", str(ws))
    return ws


# Reads by place: only a path inside the workspace, outside its .git and
# not under /proc, is a one-key read.
@pytest.mark.parametrize(
    ("how", "path", "tier"),
    [
        ("shell", "/etc/hostname", "permanent"),
        ("shell", "~/notes.txt", "permanent"),
        ("shell", "~/anything", "permanent"),
        ("shell", "../outside.txt", "permanent"),
        ("shell", ".git/config", "permanent"),
        ("shell", "/proc/cpuinfo", "permanent"),
        ("shell", "src/x.py", "undoable"),
        ("read", "/etc/hostname", "permanent"),
        ("read", "~/anything", "permanent"),
        ("read", "../outside.txt", "permanent"),
        ("read", ".git/config", "permanent"),
        ("read", "/proc/cpuinfo", "permanent"),
        ("read", "src/x.py", "undoable"),
    ],
)
def test_a_read_is_undoable_only_inside_the_workspace(how, path, tier, tmp_path, monkeypatch):
    if how == "shell" and not effects.shell_parser_available():
        pytest.skip("the shell extra is not installed")
    ws = _plain_workspace(tmp_path, monkeypatch)
    payload = (
        payload_for(f"cat {path}", cwd=str(ws))
        if how == "shell"
        else payload_for("Read", path=path, cwd=str(ws))
    )
    d = evaluate_call(payload, envelope=deny_all())
    assert d.tier == tier, (how, path)


@pytest.mark.parametrize(
    "command",
    [
        # bash cd works on the logical path, so .. after a symlink comes back.
        "cd homelink && cd .. && cat homelink/.config/gh/hosts.yml",
        "cd homelink/.. && cat homelink/.config/gh/hosts.yml",
        "cd homelink && cat notes.txt",
        # Globs, braces, and ~+, ~-, ~user change the word before cat sees it.
        "cat /home/u/.conf*/gh/hosts.yml",
        "cat ~/.ss?/id_rsa",
        "cat ~/.[s]sh/id_rsa",
        "cat /home/u/.{config,x}/gh/hosts.yml",
        "cat ~+/homelink/.config/gh/hosts.yml",
        "cat ~-/.config/gh/hosts.yml",
        "cat ~root/.bashrc",
        "cat src/*.py",
        "echo x > out*.txt",
        "cat < ~+/x",
        # /proc/self resolves in the gate, not in the agent's shell.
        "cat /proc/self/cwd/homelink/.config/gh/hosts.yml",
        "cat /proc/thread-self/root/etc/hostname",
        "cat /proc/1/root/etc/hostname",
        "ls /proc",
    ],
)
def test_every_normalizer_probe_is_permanent(command, tmp_path, monkeypatch):
    ws = _plain_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
@pytest.mark.parametrize(
    "command",
    ["cat 'src/*.py'", "find src -name '*.py'", "grep -n 'a{2}' src/x.py", "cd src && cat x.py"],
)
def test_quoted_globs_and_a_plain_cd_stay_undoable(command, tmp_path, monkeypatch):
    ws = _plain_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "undoable", command


def test_a_path_the_gate_cannot_resolve_is_unknown_not_an_error(monkeypatch):
    def boom(path):
        raise PermissionError(path)

    monkeypatch.setattr(effects.os.path, "realpath", boom)
    rec = {"step_type": "file_read", "path": "src/x.py"}
    assert effects.effect_class(rec, WORKSPACE) == "unknown"


def _outside_workspace(tmp_path, monkeypatch, git=True):
    """A temp workspace that is a repo root, with a link sub/out to a temp
    directory outside it. Never the real home."""
    ws = tmp_path / "ws"
    (ws / "sub").mkdir(parents=True)
    (ws / "src").mkdir()
    (ws / "src" / "x.py").write_text("x = 1\n")
    if git:
        (ws / ".git").mkdir()
    out = tmp_path / "outside"
    out.mkdir()
    (out / "secret.txt").write_text("secret-marker\n")
    (out / "t.js").write_text("\n")
    (ws / "sub" / "out").symlink_to(out)
    (ws / "sub2").mkdir()
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", str(ws))
    return ws, out


ROUND_6_PROBES = [
    # A test run reads, and often runs, every path it is given.
    "pytest ../outside",
    "pytest {out}",
    "python -m pytest ../outside",
    "uv run --no-sync pytest ../outside",
    "ruff check ../outside",
    "ruff format --check ../outside",
    "mypy /etc",
    "go test ../outside/...",
    "node --test ../outside/t.js",
    "cargo test -- ../outside",
    # Recursive and link-following flags walk through sub/out.
    "grep -R secret sub",
    "grep -r secret sub",
    "grep -rn secret .",
    "grep --recursive secret sub",
    "grep -d recurse secret sub",
    "diff -r sub sub2",
    "diff --recursive sub sub2",
    "find -L sub",
    "find sub -follow",
    "ls -R sub",
    "ls --recursive sub",
    "cp sub/out/secret.txt x",
    "rsync -a sub/ x/",
    # A git flag that names a file.
    "git commit -F ../outside/secret.txt",
    "git commit -F/etc/hostname",
    "git commit --file=../outside/secret.txt",
    "git commit --file ../outside/secret.txt",
    "git commit -aF ../outside/secret.txt",
    "git commit -t ../outside/secret.txt",
    "git commit --template=../outside/secret.txt",
    "git add --pathspec-from-file=../outside/secret.txt",
    "git -C ../outside status",
]


@pytest.mark.parametrize("command", ROUND_6_PROBES)
def test_every_round_6_probe_is_permanent(command, tmp_path, monkeypatch):
    ws, out = _outside_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for(command.format(out=out), cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
@pytest.mark.parametrize(
    "command",
    [
        "pytest src",
        "ruff check src",
        "go test ./...",
        "git commit -F src/x.py",
        "git commit -mFix",
        "git commit --file=src/x.py",
        "git status",
        "grep -n x src/x.py",
        "find src -name '*.py'",
    ],
)
def test_the_same_commands_inside_the_workspace_stay_undoable(command, tmp_path, monkeypatch):
    ws, _ = _outside_workspace(tmp_path, monkeypatch)
    d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "undoable", command


# A git verb is undoable only when the workspace root holds the repo's
# .git, so the repo is the workspace.
@pytest.mark.parametrize(
    "command",
    [
        "git status",
        "git log -p -- ../other",
        "git diff",
        "git show HEAD",
        "git add src/x.py",
        "git commit -m wip",
    ],
)
def test_a_workspace_inside_a_larger_repo_makes_git_permanent(command, tmp_path, monkeypatch):
    repo = tmp_path / "repo"
    ws = repo / "pkg"
    (ws / "src").mkdir(parents=True)
    (repo / ".git").mkdir()
    monkeypatch.setenv("CLAUDE_PROJECT_DIR", str(ws))
    d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent", command


@needs_parser
def test_a_workspace_that_is_the_repo_root_keeps_git_undoable(tmp_path, monkeypatch):
    ws, _ = _outside_workspace(tmp_path, monkeypatch)
    for command in ["git status", "git add src/x.py", "git commit -m wip"]:
        d = evaluate_call(payload_for(command, cwd=str(ws)), envelope=deny_all())
        assert d.tier == "undoable", command


def test_a_workspace_with_no_git_makes_git_permanent(tmp_path, monkeypatch):
    ws, _ = _outside_workspace(tmp_path, monkeypatch, git=False)
    d = evaluate_call(payload_for("git status", cwd=str(ws)), envelope=deny_all())
    assert d.tier == "permanent"

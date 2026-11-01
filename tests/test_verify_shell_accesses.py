"""Operand reads and writes face file_read and file_write at the permission
stage (SG-1), trees need a covering glob (SG-2), the base places relative
paths (SG-3), unknown sets are violations unless the scope is everything."""

from __future__ import annotations

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission
from opendaisugi.verify import verify

ALLOW = [
    "cp",
    "mv",
    "tee",
    "sed",
    "cat",
    "find",
    "xargs",
    "tar",
    "grep",
    "ls",
    "echo",
    "sh",
    "env",
    "cd",
    "rm",
    "head",
]


def env(**over):
    p = dict(
        file_read=["/work/**"],
        file_write=["/work/**"],
        shell=True,
        shell_allowlist=ALLOW,
        shell_allow_decomposition=True,
    )
    p.update(over)
    return Envelope(generated_by="t", task="t", permissions=Permission(**p))


def run(cmd, base="/work", **over):
    plan = ActionPlan(source="t", task="t", steps=[{"id": "s1", "type": "shell", "command": cmd}])
    return verify(plan, env(**over), dialect_base=base)


@pytest.mark.parametrize(
    "cmd",
    [
        "cp /work/a /home/user/.ssh/authorized_keys",
        "tee /etc/cron.d/x",
        "sed -i s/a/b/ /etc/hosts",
        "mv /work/a /etc/x",
        "cat /etc/passwd",
        "tar -xzf /work/evil.tgz -C /",
        "tar -czf /work/out.tgz /home/user/.aws",
        "cp -r /home/user/.ssh /work/out",
        "grep -r BEGIN /home/user",
        "find / -name '*.pem'",
        "find /home/user -name 'id_*' -exec cat {} +",
        "sh -c 'cp /work/a /etc/x'",
    ],
)
def test_outside_scope_is_denied(cmd):
    r = run(cmd)
    assert not r.ok, cmd
    assert all(v.stage == "permissions" for v in r.violations)


@pytest.mark.parametrize(
    "cmd",
    [
        "cp /work/a /work/b",
        "cp a b",
        "cd /work/src && cp a b",
        "find /work -name '*.py' -exec cat {} +",
        "grep -rn foo /work/src",
        "tar -czf /work/out.tgz /work/src",
        "tar -xzf /work/in.tgz -C /work/out",
        "cp -r /work/src /work/out",
        "cat /work/a /work/b",
        "ls /work",
        "echo x > /dev/null",
        "cat a > out.txt",
        "ls /work | xargs echo",
    ],
)
def test_inside_scope_is_allowed(cmd):
    r = run(cmd)
    assert r.ok, (cmd, [v.message for v in r.violations])


def test_tree_write_needs_covering_glob():
    r = run("tar -xzf /work/in.tgz -C /work/out", file_write=["/work/out/*"])
    assert not r.ok
    assert r.violations[0].message == (
        "Step 's1' shell writes under '/work/out' (recursive) outside file_write scope ['/work/out/*']"
    )
    assert r.violations[0].detail == {"step": "s1", "shell_write_tree": "/work/out"}


def test_unknown_reads_deny_unless_everything():
    r = run("ls | xargs cat")
    assert not r.ok
    assert r.violations[0].detail == {
        "step": "s1",
        "shell_reads_unknown": "cat runs under xargs, which adds operands the line does not show",
    }
    assert run("ls | xargs cat", file_read=["/**"]).ok
    assert not run("ls | xargs cat", file_read=["/**/*"]).ok


def test_cd_then_relative_outside_is_denied():
    r = run("cd /etc && tee hosts")
    assert not r.ok
    assert "shell writes '/etc/hosts'" in r.violations[0].message


def test_env_chdir_is_unknown():
    r = run("env --chdir=/etc tee hosts")
    assert not r.ok and "shell_writes_unknown" in r.violations[0].detail


def test_no_base_keeps_literal_matching():
    assert not run("cat a > out.txt", base=None).ok
    assert run("cat a > out.txt", base=None, file_read=["a"], file_write=["out.txt"]).ok


def test_blanket_reject_still_names_the_files():
    r = run("cat /etc/passwd; ls", shell_allow_decomposition=False)
    msgs = [v.message for v in r.violations]
    assert any("dangerous metacharacters" in m for m in msgs)
    assert any("shell reads '/etc/passwd'" in m for m in msgs)


def test_writer_with_unknown_flag_denies():
    r = run("cp --weird a b")
    assert (
        not r.ok
        and r.violations[0].detail["shell_writes_unknown"] == "a flag outside the closed set of cp"
    )

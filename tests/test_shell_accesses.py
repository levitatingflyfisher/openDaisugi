"""One access analysis for a shell step: the paths it reads and writes
through operands and redirects, each a path or a tree, placed from a base,
or a named reason why they are unknown (rulings SG-1 to SG-6)."""

from __future__ import annotations

import pytest

from opendaisugi.interpreter_parse import parse_interpreter
from opendaisugi.write_paths import (
    CWD_MOVERS,
    FIND_ARGS,
    READERS,
    TAR,
    UNKNOWN_REASONS,
    WRITERS,
    Access,
    shell_accesses,
    step_accesses,
    step_write_paths,
)


def R(path, tree=False, via="operand"):
    return Access(path, tree, via)


def W(path, tree=False, via="operand"):
    return Access(path, tree, via)


def acc(cmd, base="/work"):
    return shell_accesses(cmd, base)


@pytest.mark.parametrize(
    ("cmd", "reads", "writes"),
    [
        ("cp /work/a /etc/x", (R("/work/a"),), (W("/etc/x"), W("/etc/x/a"))),
        ("cp a b", (R("/work/a"),), (W("/work/b"), W("/work/b/a"))),
        ("cp -r src out", (R("/work/src", True),), (W("/work/out"), W("/work/out/src", True))),
        (
            "mv a /etc/x",
            (R("/work/a", True),),
            (W("/work/a", True), W("/etc/x", True), W("/etc/x/a", True)),
        ),
        ("tee /etc/cron.d/x", (), (W("/etc/cron.d/x"),)),
        ("sed -i s/a/b/ /etc/hosts", (R("/etc/hosts"),), (W("/etc/hosts"),)),
        ("sed -n 1p a", (R("/work/a"),), ()),
        ("cat /etc/passwd", (R("/etc/passwd"),), ()),
        ("cat a b", (R("/work/a"), R("/work/b")), ()),
        ("cat", (), ()),
        ("head -n 5 a", (R("/work/a"),), ()),
        ("grep -rn foo src", (R("/work/src", True),), ()),
        ("grep -r foo", (R("/work", True),), ()),
        ("grep -e foo -f pats a", (R("/work/pats"), R("/work/a")), ()),
        ("rg foo", (R("/work", True),), ()),
        ("ls", (R("/work"),), ()),
        ("ls -R /home/user", (R("/home/user", True),), ()),
        ("find / -name '*.pem'", (R("/", True),), ()),
        ("find . -name '*.py' -exec cat {} +", (R("/work", True),), ()),
        ("find /work -type f -delete", (R("/work", True),), (W("/work", True),)),
        ("find src -name x -fprint /etc/out", (R("/work/src", True),), (W("/etc/out"),)),
        ("tar -czf out.tgz src", (R("/work/src", True),), (W("/work/out.tgz"),)),
        ("tar czf out.tgz src", (R("/work/src", True),), (W("/work/out.tgz"),)),
        ("tar -xzf in.tgz -C out", (R("/work/in.tgz"),), (W("/work/out", True),)),
        ("tar -xf /work/evil.tgz", (R("/work/evil.tgz"),), (W("/work", True),)),
        ("tar -tf in.tgz", (R("/work/in.tgz"),), ()),
        ("tar -cf - src", (R("/work/src", True),), ()),
        ("dd if=a of=/dev/sda", (R("/work/a"),), (W("/dev/sda"),)),
        ("cat a > out.txt", (R("/work/a"),), (W("/work/out.txt", via="redirect"),)),
        ("echo x > /dev/null", (), ()),
        (
            "cd /work/src && cp a b",
            (R("/work/a"), R("/work/src/a")),
            (W("/work/b"), W("/work/b/a"), W("/work/src/b"), W("/work/src/b/a")),
        ),
        ("sh -c 'cp /work/a /etc/x'", (R("/work/a"),), (W("/etc/x"), W("/etc/x/a"))),
        ("ls /work | xargs echo", (R("/work"),), ()),
    ],
)
def test_known_accesses(cmd, reads, writes):
    a = acc(cmd)
    assert (a.reads_unknown, a.writes_unknown) == (None, None)
    assert sorted(a.reads) == sorted(reads)
    assert sorted(a.writes) == sorted(writes)


@pytest.mark.parametrize(
    ("cmd", "reads_unknown", "writes_unknown"),
    [
        (
            "ls | xargs cat",
            "cat runs under xargs, which adds operands the line does not show",
            None,
        ),
        ("ls | xargs rm", None, "rm runs under xargs, which adds operands the line does not show"),
        (
            "find / -name '*.pem' | xargs cat",
            "cat runs under xargs, which adds operands the line does not show",
            None,
        ),
        (
            "find src -exec tee x {} +",
            None,
            "a writer runs under find -exec, which adds operands the line does not show",
        ),
        ("find src -execdir cat ../../x {} ;", "a relative path with .. under find -execdir", None),
        (
            "find src -newerXY x",
            "a find test or action outside the closed set",
            "a find test or action outside the closed set",
        ),
        (
            "cd /etc; cd /work && tee hosts",
            "the line moves its cwd and names a relative path",
            "the line moves its cwd and names a relative path",
        ),
        (
            "pushd /etc && tee hosts",
            "the line moves its cwd and names a relative path",
            "the line moves its cwd and names a relative path",
        ),
        (
            "env --chdir=/etc tee hosts",
            "the line moves its cwd and names a relative path",
            "the line moves its cwd and names a relative path",
        ),
        (
            "env -C /etc tee hosts",
            "the line moves its cwd and names a relative path",
            "the line moves its cwd and names a relative path",
        ),
        (
            "builtin cd /etc && tee hosts",
            UNKNOWN_REASONS[13],
            UNKNOWN_REASONS[13],
        ),
        (
            "tar -xPf evil.tgz",
            "a flag outside the closed set of tar",
            "a flag outside the closed set of tar",
        ),
        (
            "tar -xf evil.tgz -C a -C b",
            "a flag outside the closed set of tar",
            "a flag outside the closed set of tar",
        ),
        (
            "tar -x",
            "tar without one of c, x, t and one -f",
            "tar without one of c, x, t and one -f",
        ),
        (
            "cp a.key /home/user/*",
            "a word the shell would change ($, a backtick, a glob, a brace, ~user)",
            "a word the shell would change ($, a backtick, a glob, a brace, ~user)",
        ),
        (
            "sort -o out a",
            "a flag outside the closed set of sort",
            "a flag outside the closed set of sort",
        ),
        ("cat ~/x", "a path under ~", None),
        (
            "cat ~alice/x",
            "a word the shell would change ($, a backtick, a glob, a brace, ~user)",
            None,
        ),
    ],
)
def test_unknown_accesses(cmd, reads_unknown, writes_unknown):
    a = acc(cmd)
    assert (a.reads_unknown, a.writes_unknown) == (reads_unknown, writes_unknown)


def test_every_reason_is_from_the_closed_list():
    heads = ("cp", "cat", "rm", "tar", "sort")
    instances = {u.format(head=h) for u in UNKNOWN_REASONS for h in heads}
    for cmd in (
        "ls | xargs cat",
        "ls | xargs rm",
        "tar -xPf e.tgz",
        "sort -o out a",
        "cat ~/x",
        "env -C /etc tee hosts",
        "find src -newerXY x",
        "cp a.key /home/user/*",
        "tar -x",
    ):
        a = acc(cmd)
        for r in (a.reads_unknown, a.writes_unknown):
            assert r is None or r in instances, (cmd, r)


def test_execdir_dotdot_is_unknown():
    assert (
        acc("find /work -execdir cat ../../x {} ;").reads_unknown
        == "a relative path with .. under find -execdir"
    )
    a = acc("find /work -execdir cat x {} ;")
    assert a.reads_unknown is None and a.reads == (R("/work", True),)


def test_no_base_keeps_relative_paths():
    a = shell_accesses("cd src && cp a b", None)
    assert sorted(x.path for x in a.writes) == ["b", "b/a", "src/b", "src/b/a"]


def test_every_cwd_mover_is_a_mover():
    # A leading literal cd is the one mover the base rule handles (two bases).
    for head in sorted(CWD_MOVERS - {"cd"}):
        a = acc(f"{head} /etc && tee hosts")
        runs = head in ("exec", "eval", "builtin", "source", ".")
        want = UNKNOWN_REASONS[13] if runs else "the line moves its cwd and names a relative path"
        assert a.writes_unknown == want, head


def test_dialect_field_is_the_write_half():
    step = {"id": "s", "type": "shell", "command": "cd src && tee a"}
    assert step_write_paths(step, "/work") == ["/work/a", "/work/src/a"]
    assert (
        step_write_paths({"id": "s", "type": "shell", "command": "ls | xargs rm"}, "/work") is None
    )


def test_tables_are_closed_and_disjoint_where_they_must_be():
    both = set(READERS) & set(WRITERS)
    assert both == {"cp", "mv", "install", "rsync", "tar", "dd", "sed", "uniq", "xxd"}
    assert "tar" in WRITERS and "tar" in READERS
    assert FIND_ARGS["-exec"] == "*" and FIND_ARGS["-name"] == 1 and TAR["modes"] == "cxt"


def test_file_steps_are_one_access_each():
    w = step_accesses({"id": "s", "type": "file_write", "path": "out/a", "content": "x"}, "/work")
    assert w.writes == (W("/work/out/a"),) and w.reads == ()
    r = step_accesses({"id": "s", "type": "file_read", "path": "/etc/hosts"}, "/work")
    assert r.reads == (R("/etc/hosts"),) and r.writes == ()
    assert step_accesses(
        {"id": "s", "type": "network", "url": "https://x"}, "/work"
    ) == step_accesses({"type": "other"}, None)


# ---- review round 1 -------------------------------------------------------

MOVES = "the line moves its cwd and names a relative path"
RUNS = UNKNOWN_REASONS[13]
EMBED = "a find -exec operand that wraps the found file in a longer word"
REMOTE = "a tar archive on another host (HOST:PATH)"


@pytest.mark.parametrize(
    "cmd",
    [
        "for i in 1 2 3; do cd ..; done; echo x > etc/cron.d/x",
        "f(){ cd ..; }; f; f; f; echo x > etc/shadow",
        "(cd ..; tee x)",
        "{ cd ..; }; tee x",
        "if cd ..; then tee x; fi",
        "while cd ..; do tee x; done",
        "until cd ..; do tee x; done",
        "cd ..; cd ..; cd ..; echo x > etc/shadow",
    ],
)
def test_a_cd_that_is_not_the_line_start_is_a_mover(cmd):
    a = shell_accesses(cmd, "/work/a/b")
    assert a.writes_unknown == MOVES, cmd


def test_a_leading_cd_still_doubles_the_base():
    a = shell_accesses("cd .. && tee etc/hosts", "/work/a")
    assert a.writes_unknown is None
    assert sorted(x.path for x in a.writes) == ["/work/a/etc/hosts", "/work/etc/hosts"]
    a = shell_accesses("cd /work/src\ntee a", "/work")
    assert a.writes_unknown is None


@pytest.mark.parametrize(
    "cmd",
    [
        "exec cat /etc/passwd",
        "eval cat /etc/passwd",
        "builtin cat /etc/passwd",
        "source /etc/passwd",
        ". /etc/passwd",
        "exec -a x cat /etc/passwd",
        "env exec cat /etc/passwd",
        "sh -c 'eval cat /etc/passwd'",
    ],
)
def test_exec_eval_builtin_source_make_the_line_unknown(cmd):
    a = acc(cmd)
    assert a.reads_unknown == RUNS and a.writes_unknown == RUNS, cmd


def test_exec_with_only_redirects_runs_nothing():
    a = acc("exec 3>/work/log")
    assert a.reads_unknown is None and a.writes_unknown is None
    assert W("/work/log", via="redirect") in a.writes


@pytest.mark.parametrize(
    "cmd",
    [
        "find . -name shadow -exec cat /etc/{} \;",
        "find /work -exec cat {}/../../etc/passwd \;",
        "find /work -exec grep --file={} x \;",
    ],
)
def test_an_operand_that_embeds_braces_under_find_exec_is_unknown(cmd):
    assert acc(cmd).reads_unknown == EMBED, cmd


def test_a_bare_brace_operand_is_the_found_file():
    a = acc("find /work -exec grep -e {} /etc/passwd \;")
    assert a.reads_unknown is None
    assert R("/etc/passwd") in a.reads
    a = acc("find /work -exec cat {} \;")
    assert a.reads == (R("/work", True),) and a.reads_unknown is None


def test_uniq_and_xxd_write_their_second_operand():
    for cmd, rd, wr in (
        ("uniq /etc/passwd /etc/evil", "/etc/passwd", "/etc/evil"),
        ("xxd /work/a /etc/evil", "/work/a", "/etc/evil"),
        ("uniq -c a b", "/work/a", "/work/b"),
        ("uniq - /etc/evil", None, "/etc/evil"),
    ):
        a = acc(cmd)
        assert a.writes == (W(wr),), cmd
        assert a.reads == ((R(rd),) if rd else ()), cmd
    a = acc("uniq a")
    assert a.writes == () and a.reads == (R("/work/a"),)
    a = acc("uniq a b c")
    assert a.writes_unknown is not None


def test_env_option_values_are_not_the_command():
    # -u NAME consumes NAME; -C DIR is a mover (the line is unknown).
    a = acc("env -u grep -C /etc tee hosts")
    assert a.writes_unknown == MOVES
    a = acc("env -u cat tee /etc/hosts")
    assert a.writes == (W("/etc/hosts"),)
    a = acc("env -uX tee /etc/hosts")
    assert a.writes == (W("/etc/hosts"),)
    a = acc("env --unset X tee /etc/hosts")
    assert a.writes == (W("/etc/hosts"),)


def test_a_remote_tar_archive_is_unknown():
    for cmd in ("tar -cf evil:/x /work/a", "tar -xf host:/in.tgz", "tar cf h:/x a"):
        a = acc(cmd)
        assert a.writes_unknown == REMOTE and a.reads_unknown == REMOTE, cmd
    a = acc("tar -cf /work/a:b /work/src")
    assert a.writes_unknown is None


def test_grep_directories_recurse_is_recursive():
    for cmd in (
        "grep -d recurse x /work",
        "grep --directories=recurse x /work",
        "grep -drecurse x /work",
    ):
        assert acc(cmd).reads == (R("/work", True),), cmd
    assert acc("grep -d skip x /work").reads == (R("/work"),)


def test_a_root_base_has_one_leading_slash():
    a = shell_accesses("cd .. && tee etc/hosts", "/work")
    assert W("/etc/hosts") in a.writes
    assert all(not x.path.startswith("//") for x in a.writes)
    assert acc("cat //etc/hosts").reads == (R("/etc/hosts"),)


def test_process_substitution_is_not_an_operand():
    a = acc("cat <(cat /etc/passwd)")
    assert sorted(a.reads) == [R("/etc/passwd")]
    a = acc("diff <(sort a) <(sort b) c")
    assert sorted(x.path for x in a.reads) == ["/work/a", "/work/b", "/work/c"]
    a = acc("cat '<(x)' /work/y")
    assert sorted(x.path for x in a.reads) == ["/work/<(x)", "/work/y"]


# --- review round 2 -------------------------------------------------------

OPTION = UNKNOWN_REASONS[16]
SED = UNKNOWN_REASONS[17]
FLAG_HEADS = ("sort", "less", "uniq", "grep")


@pytest.mark.parametrize(
    "cmd",
    [
        "env --i cat tee /etc/x",  # ambiguous: --ignore-environment or --ignore-signal
        "env --d cat tee /etc/x",  # ambiguous: --debug or --default-signal
        "env --nope cat tee /etc/x",
        "xargs --max cat tee /etc/x",  # ambiguous
        "xargs --bogus tee /etc/x",
        "nice --adj 5 --nope tee /etc/x",
        "timeout --kill 5 --nope 30 tee /etc/x",
        "ionice --nope tee /etc/x",
    ],
)
def test_an_unmatched_long_option_makes_the_command_unknown(cmd):
    a = acc(cmd)
    assert a.reads_unknown == OPTION and a.writes_unknown == OPTION, cmd


def test_a_unique_prefix_of_a_long_option_is_followed():
    # --u is --unset: its value is cat, the command is tee
    assert acc("env --u cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --uns cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --ar cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --nu tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --unset cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --unset=cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("env --block-signal tee /etc/x").writes == (W("/etc/x"),)
    assert acc("xargs --no-run tee /etc/x").writes_unknown is not None
    assert acc("nice --adjustment 5 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("nice --adj 5 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("timeout --kill-after 5 30 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("timeout --signal=KILL 30 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("timeout --foreground 30 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("stdbuf --output=L tee /etc/x").writes == (W("/etc/x"),)


def test_an_env_prefix_chdir_is_a_mover():
    for cmd in ("env --ch /etc tee hosts", "env --chd=/etc tee hosts", "env --sp 'tee hosts'"):
        assert acc(cmd).writes_unknown == MOVES, cmd


def test_xargs_arg_file_prefix_is_a_read():
    # --arg-f is --arg-file: cat is the FILE, tee is the command under xargs
    a = acc("xargs --arg-f cat tee /etc/x")
    assert R("/work/cat") in a.reads and a.writes_unknown is not None
    a = acc("xargs --arg-fi in tee /etc/x")
    assert R("/work/in") in a.reads
    a = acc("xargs -a in echo")
    assert a.reads == (R("/work/in"),)


def test_xargs_bundled_value_options_take_their_value():
    # -rI takes the next word as the replace string: the command is tee
    a = acc("xargs -rI cat tee /etc/x")
    assert a.writes_unknown is not None
    # --replace and --max-lines take a value only as --name=VALUE
    assert parse_interpreter("xargs --replace cat tee x").inner_commands == ["cat tee x"]
    assert parse_interpreter("xargs --replace=R cat x").inner_commands == ["cat x"]
    assert parse_interpreter("xargs -n 1 -I{} cat {}").inner_commands == ["cat '{}'"]


@pytest.mark.parametrize(
    "cmd",
    [
        "sort -o /etc/x in",
        "sort --output=/etc/x in",
        "less -o /etc/x",
        "file -C -m x",
        "cat --bogus a",
    ],
)
def test_a_flag_outside_a_readers_set_makes_reads_and_writes_unknown(cmd):
    a = acc(cmd)
    head = cmd.split()[0]
    reason = _flag_reason(head)
    assert a.reads_unknown == reason and a.writes_unknown == reason, cmd


def _flag_reason(head):
    return UNKNOWN_REASONS[5].format(head=head)


def test_a_reader_with_known_flags_writes_nothing():
    a = acc("sort -r in")
    assert a.writes == () and a.writes_unknown is None
    assert a.reads == (R("/work/in"),)
    assert acc("less -N f").writes_unknown is None


def test_ionice_t_takes_no_value():
    # -t is --ignore: tee is the command, cat and /etc/passwd are its operands
    p = parse_interpreter("ionice -t tee cat /etc/passwd")
    assert p.inner_commands == ["tee cat /etc/passwd"]
    assert acc("ionice -t tee /etc/x").writes == (W("/etc/x"),)
    assert acc("ionice -c 2 -n 5 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("ionice -c2 -n5 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("ionice --ignore tee /etc/x").writes == (W("/etc/x"),)
    assert parse_interpreter("ionice -p 12 -c 1").inner_commands == []


def test_gnu_time_output_is_a_write_and_format_takes_a_value():
    assert acc("time -o /etc/x tee y").writes == (W("/etc/x"), W("/work/y"))
    assert acc("time --output=/etc/x ls").writes == (W("/etc/x"),)
    assert acc("time --output /etc/x ls").writes == (W("/etc/x"),)
    assert acc("time -o/etc/x ls").writes == (W("/etc/x"),)
    assert acc("time -a -o /etc/x ls").writes == (W("/etc/x"),)
    p = parse_interpreter("time -o cat tee /etc/x")
    assert p.inner_commands == ["tee /etc/x"]
    assert parse_interpreter("time -f %e cat x").inner_commands == ["cat x"]
    assert parse_interpreter("time -p cat x").inner_commands == ["cat x"]
    assert acc("time -p cat x").writes_unknown is None and acc("time -p cat x").writes == ()


@pytest.mark.parametrize(
    "script",
    [
        "s/a/b/w /etc/x",
        "w /etc/x",
        "1w /etc/x",
        "/a/W /etc/x",
        "e echo hi",
        "s/a/b/e",
        "s/a/b/gw /etc/x",
        "$!{p;w /etc/x}",
        "r /etc/shadow",
        "R /etc/shadow",
    ],
)
def test_a_sed_script_that_writes_or_runs_a_file_is_unknown(script):
    for cmd in (
        f"sed -n '{script}' in",
        f"sed -e '{script}' in",
        f"sed --expression='{script}' in",
    ):
        a = acc(cmd)
        assert a.writes_unknown == SED and a.reads_unknown == SED, cmd


def test_a_sed_script_file_is_unknown_and_benign_scripts_are_placed():
    assert acc("sed -f prog.sed in").writes_unknown == SED
    assert acc("sed --file=prog.sed in").writes_unknown == SED
    for script in (
        "s/a/b/",
        "s/world/x/g",
        "1,5p",
        "$!d",
        "/x/,/y/{p;d}",
        "y/abc/xyz/",
        "a text; w x",
    ):
        a = acc(f"sed -n '{script}' in")
        assert a.writes_unknown is None and a.writes == (), script
        assert a.reads == (R("/work/in"),), script
    a = acc("sed -i 's/a/b/' in")
    assert a.writes == (W("/work/in"),)
    assert acc("sed -i 's/a/b/w x' in").writes_unknown == SED


@pytest.mark.parametrize(
    "cmd",
    [
        "chroot /jail tee /etc/x",
        "flock /tmp/l tee /etc/x",
        "flock -c 'tee /etc/x' /tmp/l",
        "busybox tee /etc/x",
        "script -c 'tee /etc/x' /dev/null",
        "parallel tee ::: /etc/x",
        "coproc tee /etc/x",
        "trap 'tee /etc/x' EXIT",
    ],
)
def test_wrappers_the_placer_cannot_unwrap_make_the_line_unknown(cmd):
    a = acc(cmd)
    assert a.reads_unknown == RUNS and a.writes_unknown == RUNS, cmd


def test_wrappers_in_the_opaque_set():
    from opendaisugi.interpreter_parse import parse_interpreter as pi

    for head in ("chroot", "flock", "busybox", "script", "parallel", "coproc", "trap"):
        p = pi(f"{head} x")
        assert p is not None and p.opaque, head


# --- review round 3 -------------------------------------------------------

FLAG = "a flag outside the closed set of {head}"


def test_od_w_takes_a_value_only_when_attached():
    # od -w /etc/shadow: -w has no value here, the file is an operand and is read
    assert acc("od -w /etc/shadow").reads == (R("/etc/shadow"),)
    assert acc("od -w16 /etc/shadow").reads == (R("/etc/shadow"),)
    assert acc("od -w").reads == ()
    assert acc("od -A x -t x1 -w8 a").reads == (R("/work/a"),)
    assert acc("od -S 4 a").reads == (R("/work/a"),)


def test_hexdump_f_is_a_flag_outside_the_closed_set():
    a = acc("hexdump -f /etc/shadow a")
    assert a.reads_unknown == FLAG.format(head="hexdump")
    assert a.writes_unknown == FLAG.format(head="hexdump")
    assert acc("hexdump -C a").reads == (R("/work/a"),)
    assert acc("hexdump -n 16 -s 4 -e '1/1 \"%02x\"' a").reads == (R("/work/a"),)


def test_rg_color_takes_a_value():
    assert acc("rg --color never secret").reads == (R("/work", True),)
    assert acc("rg --color=never secret").reads == (R("/work", True),)
    assert acc("rg --color never secret src").reads == (R("/work/src", True),)
    assert acc("rg --colour never secret").reads_unknown == FLAG.format(head="rg")


@pytest.mark.parametrize("head", ["md5sum", "sha1sum", "sha256sum", "sha512sum"])
def test_a_checksum_check_reads_the_files_it_lists(head):
    # -c reads the files named inside the operand: nothing the placer can see
    for flag in ("-c", "--check"):
        a = acc(f"{head} {flag} sums.txt")
        assert a.reads_unknown == FLAG.format(head=head), (head, flag)
    assert acc(f"{head} a b").reads == (R("/work/a"), R("/work/b"))


@pytest.mark.parametrize(
    "cmd",
    [
        "sudo tee /etc/x",
        "sudo -u root tee /etc/x",
        "doas tee /etc/x",
        "watch tee /etc/x",
        "watch -n 1 tee /etc/x",
        "systemd-run --user tee /etc/x",
        "systemd-run --user --scope tee /etc/x",
        "screen -dm tee /etc/x",
        "unbuffer tee /etc/x",
        "expect -c 'spawn tee /etc/x'",
        "runuser -u root -- tee /etc/x",
        "su -c 'tee /etc/x' root",
        "su root -c 'tee /etc/x'",
        "su -lc 'tee /etc/x'",
        "su --command='tee /etc/x'",
        "taskset -c 0 tee /etc/x",
        "chrt -f 10 tee /etc/x",
        "prlimit --nofile=1024 tee /etc/x",
        "unshare -r tee /etc/x",
        "nsenter -t 1 -m tee /etc/x",
        "batch",
        "at now",
        "at -f job.sh now",
        "crontab -",
        "crontab cron.txt",
        "entr tee /etc/x",
        "ssh host tee /etc/x",
        "ssh -p 22 host tee /etc/x",
        "ssh -o ProxyCommand='tee /etc/x' host",
        "ssh -oLocalCommand=x host",
        "ssh -F cfg host",
        "tmux new-session -d 'tee /etc/x'",
        "tmux new -d 'tee /etc/x'",
        "tmux new-window 'tee /etc/x'",
        "tmux split-window 'tee /etc/x'",
        "tmux send-keys 'tee /etc/x' Enter",
        "tmux run-shell 'tee /etc/x'",
        "tmux run 'tee /etc/x'",
        "tmux respawn-pane -k 'tee /etc/x'",
        "tmux -f cfg ls",
        "tmux -L sock send-keys x",
        "tmux display-popup 'tee /etc/x'",
        "tmux if-shell true 'run-shell x'",
        "vim -c 'w /etc/x' a",
        "nvim -c 'w /etc/x' a",
        "vi +'w /etc/x' a",
        "vim +q a",
        "vim -S s.vim a",
        "vim --cmd 'w /etc/x' a",
        "vim -es a",
        "git -c alias.x='!tee /etc/x' x",
        "git -c core.pager='tee /etc/x' log",
        "git -c core.sshCommand=x fetch",
        "git -C /work -c core.pager=x log",
        "git --config-env=core.pager=P log",
        "git --config-env core.pager=P log",
        "git --exec-path=/work/bin status",
        "git -c x=y",
    ],
)
def test_heads_that_run_a_command_make_the_line_unknown(cmd):
    a = acc(cmd)
    assert a.reads_unknown == RUNS and a.writes_unknown == RUNS, cmd


@pytest.mark.parametrize(
    "cmd",
    [
        "git status",
        "git diff HEAD~1",
        "git log --oneline -n 5",
        "git -C /work status",
        "git -C /work log",
        "git --no-pager log",
        "git --git-dir=/work/.git status",
        "git commit -m 'c flag -c is fine here'",
        "git log -c",
        "git config user.name x",
        "tmux ls",
        "tmux list-sessions",
        "tmux capture-pane -p",
        "tmux display-message hi",
        "tmux kill-session -t x",
        "vim a",
        "vim",
        "nvim file.txt",
        "ssh host",
        "su",
        "su root",
        "crontab -l",
        "at -l",
        "sudo",
        "screen",
        "ssh -V",
    ],
)
def test_the_benign_neighbours_of_a_runner_head_are_not_unknown(cmd):
    a = acc(cmd)
    assert a.reads_unknown is None and a.writes_unknown is None, cmd


def test_runner_heads_by_a_path_are_runners():
    for cmd in ("/usr/bin/sudo tee /etc/x", "/usr/bin/watch tee /etc/x", "/usr/bin/su -c x root"):
        a = acc(cmd)
        assert a.writes_unknown == RUNS, cmd


def test_an_absolute_path_head_is_the_interpreter_it_names():
    p = parse_interpreter("/usr/bin/env tee /etc/x")
    assert p is not None and p.head == "env" and p.inner_commands == ["tee /etc/x"]
    p = parse_interpreter("/usr/bin/timeout 5 tee /etc/x")
    assert p is not None and p.inner_commands == ["tee /etc/x"]
    p = parse_interpreter("/bin/sh -c 'tee /etc/x'")
    assert p is not None and p.inner_commands == ["tee /etc/x"]
    p = parse_interpreter("/usr/bin/python3 x.py")
    assert p is not None and p.opaque
    assert acc("/usr/bin/env --u cat tee /etc/x").writes == (W("/etc/x"),)
    assert acc("/usr/bin/timeout 5 tee /etc/x").writes == (W("/etc/x"),)
    assert acc("/usr/bin/xargs tee /etc/x").writes_unknown is not None
    # a relative path stays what it was: not an interpreter
    assert parse_interpreter("./timeout 5 tee /etc/x") is None
    assert parse_interpreter("bin/env tee /etc/x") is None
    assert acc("./timeout 5 tee /etc/x").writes == ()


@pytest.mark.parametrize(
    "cmd",
    [
        "diff --color a b",
        "diff --color=always a b",
        "diff --color=never -u a b",
        "diff -c a b",
        "diff -n a b",
        "diff --ignore-space-change --ignore-blank-lines a b",
        "diff --side-by-side --suppress-common-lines a b",
        "diff --strip-trailing-cr a b",
        "diff --label x --label y a b",
        "ls -la --group-directories-first",
        "ls --group-directories-first src",
        "ls --full-time --si -l",
        "ls --file-type --hide-control-chars",
        "ls --block-size=K -l",
        "ls --hide='*.pyc'",
        "ls --inode --size --numeric-uid-gid",
        "ls --quoting-style=shell",
        "grep -5 foo a",
        "grep --line-buffered --byte-offset foo a",
        "grep --label=x --group-separator=-- foo a",
        "grep --no-ignore-case --initial-tab foo a",
        "cat -u a",
        "sort -c a",
        "sort -m a b",
        "sort -S 1G a",
        "sort --version-sort --human-numeric-sort a",
        "sort --merge a b",
        "sort --sort=month a",
        "uniq -D a",
        "uniq --zero-terminated a",
        "tail --max-unchanged-stats=5 -f a",
        "wc --total=never a",
        "strings -n 8 a",
        "strings -a -f a",
        "nl -p -d '::' a",
        "rg --no-heading --json foo",
    ],
)
def test_common_long_options_of_the_readers_are_in_the_closed_set(cmd):
    a = acc(cmd)
    assert a.reads_unknown is None and a.writes_unknown is None, cmd
    assert a.writes == (), cmd

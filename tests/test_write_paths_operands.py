"""The write paths a command names through its operands, a line that moves
its cwd, and the resolution of relative write paths against a base.

``write_paths.step_write_paths`` adds, for each simple command of a shell
step, the files a known writing command writes through its operands
(``cp``, ``mv``, ``tee``, ``sed -i``, ``dd of=``, ``rm`` and others), with
each command's own argument rules. Anything it cannot read is unknown
(None), so ``forall_writes`` is false.
"""

from __future__ import annotations

import pytest

from opendaisugi.write_paths import WRITERS, operand_writes, step_write_paths


def _shell(command: str) -> dict:
    return {"id": "s", "type": "shell", "command": command}


@pytest.mark.parametrize(
    ("command", "expected"),
    [
        # cp and ln: the destination, and the destination/basename of each
        # source, since the destination may be a directory.
        ("cp a.py src/a.py", ["src/a.py", "src/a.py/a.py"]),
        ("cp -r lib src", ["src", "src/lib"]),
        ("cp -a x y z/", ["z", "z/x", "z/y"]),
        ("cp -t out a b/c", ["out", "out/a", "out/c"]),
        ("cp --target-directory=out a", ["out", "out/a"]),
        ("cp -T a b", ["b"]),
        ("cp a", []),
        ("cp -Z a b", None),
        ("cp -t out -T a", None),
        ("/bin/cp a b", ["b", "b/a"]),
        ("A=1 cp a b", ["b", "b/a"]),
        ("cp -- -x y", ["y", "y/-x"]),
        ("cp a b -v", ["b", "b/a"]),
        ("cp . out", ["out"]),
        ("ln -s ../lib/x.so lib.so", ["lib.so", "lib.so/x.so"]),
        ("ln -s ../lib/x.so", ["x.so"]),
        ("ln -sf a -t dir", ["dir", "dir/a"]),
        # mv changes its sources too.
        ("mv a.py src/", ["a.py", "src", "src/a.py"]),
        ("mv -t out a b", ["a", "b", "out", "out/a", "out/b"]),
        # install: -d makes every operand a directory.
        ("install -m 755 bin/x /usr/local/bin/x", ["/usr/local/bin/x", "/usr/local/bin/x/x"]),
        ("install -d a b/c", ["a", "b/c"]),
        # Every operand is written.
        ("tee out.log", ["out.log"]),
        ("tee -a a b", ["a", "b"]),
        ("touch -d 2020-01-01 a b", ["a", "b"]),
        ("mkdir -p src/x -m 700", ["src/x"]),
        ("truncate -s 0 log.txt", ["log.txt"]),
        ("truncate -r ref.txt a", ["a"]),
        ("rm -rf build ./dist", ["build", "dist"]),
        ("rmdir a", ["a"]),
        ("rmdir -p a/b", None),
        ("unlink a", ["a"]),
        ("shred -u -n 3 secret", ["secret"]),
        # sed writes only with -i; the first operand is the script unless
        # -e or -f names it.
        ("sed 's/a/b/' src/a.py", []),
        ("sed -i 's/a/b/' src/a.py src/b.py", ["src/a.py", "src/b.py"]),
        ("sed -i -e 's/a/b/' src/a.py", ["src/a.py"]),
        ("sed -i.bak 's/a/b/' a.py", ["a.py", "a.py.bak"]),
        ("sed -ie 's/a/b/' a.py", ["a.py", "a.pye"]),
        ("sed --in-place=.orig -f fix.sed a.py", ["a.py", "a.py.orig"]),
        ("sed -i'*.bak' 's/a/b/' a.py", None),
        ("sed -ni 's/a/b/p' a.py", ["a.py"]),
        # dd writes its of= operand.
        ("dd if=/dev/zero of=disk.img bs=1M count=1", ["disk.img"]),
        ("dd if=a of=/dev/null", []),
        ("dd if=a", []),
        ("dd if=a xyz=1", None),
        ("dd --of=a", None),
        # rsync: a local destination only; with --remove-source-files the
        # local sources too.
        ("rsync -av src/ backup/", ["backup", "backup/src"]),
        ("rsync -a src host:/srv/", []),
        ("rsync -a --remove-source-files a b host:/srv/", ["a", "b"]),
        ("rsync -a --exclude '*.o' src dst", ["dst", "dst/src"]),
        ("rsync -a --backup-dir=old a b", None),
        # The sanctioned sinks are left out.
        ("tee /dev/null", []),
        ("cp a /dev/stdout", ["/dev/stdout/a"]),
        # A word the shell changes is unknown.
        ("cp a $DEST", None),
        ("cp *.py out", None),
        ("rm -f ~/x", ["~/x"]),
        ("rm -f '~/x'", ["~/x"]),
        ("touch a{1,2}", None),
        # Unknown flags are unknown; a command that is not a writer is not.
        ("rm --frobnicate a", None),
        ("ls -la src", []),
        ("git commit -m x", []),
        ("echo 'a > b'", []),
    ],
)
def test_operand_writes(command, expected):
    assert step_write_paths(_shell(command)) == expected


@pytest.mark.parametrize(
    ("command", "expected"),
    [
        # Inside wrappers the operand rules still hold.
        ("sh -c 'cp a src/b'", ["src/b", "src/b/a"]),
        ("timeout 5 rm -f src/a", ["src/a"]),
        ("env A=1 touch src/a", ["src/a"]),
        ("nohup tee log < in", ["log"]),
        ("ls && rm x > y", ["y", "x"]),
        # xargs and find -exec add operands the line does not show.
        ("ls | xargs rm", None),
        ("ls | xargs -I{} cp {} out", None),
        ("find . -name '*.o' -exec rm {} ';'", None),
        ("ls | xargs sh -c 'echo > a'", ["a"]),
        ("ls | xargs sh -c 'rm a'", None),
        # Interpreters stay out of scope.
        ("python -c 'import os'", []),
        ("sudo rm -rf /", []),
    ],
)
def test_operand_writes_in_wrappers(command, expected):
    assert step_write_paths(_shell(command)) == expected


@pytest.mark.parametrize(
    ("command", "expected"),
    [
        ("cd src && echo x > a.py", None),
        ("cd src && touch a.py", None),
        ("cd /repo && echo x > /repo/a.py", ["/repo/a.py"]),
        ("pushd src; ls", []),
        ("sh -c 'cd src' && touch a", ["a"]),
        ("cd src && sh -c 'touch a'", None),
    ],
)
def test_a_line_that_moves_its_cwd(command, expected):
    assert step_write_paths(_shell(command)) == expected


@pytest.mark.parametrize(
    ("step", "base", "expected"),
    [
        (_shell("echo x > src/a.py"), "/repo", ["/repo/src/a.py"]),
        (_shell("echo x > ../up.txt"), "/repo/sub", ["/repo/up.txt"]),
        (_shell("echo x > /abs/a"), "/repo", ["/abs/a"]),
        (_shell("cp a b"), "/repo", ["/repo/b", "/repo/b/a"]),
        (_shell("echo x > '~/a'"), "/repo", None),
        (_shell("echo x > $OUT"), "/repo", None),
        (
            {"id": "w", "type": "file_write", "path": "src/a.py", "content": ""},
            "/r",
            ["/r/src/a.py"],
        ),
        ({"id": "w", "type": "file_write", "path": "/x/a.py", "content": ""}, "/r", ["/x/a.py"]),
        (_shell("ls"), "/repo", []),
        (_shell("echo x > src/a.py"), None, ["src/a.py"]),
    ],
)
def test_resolved_against_a_base(step, base, expected):
    assert step_write_paths(step, base) == expected


def test_every_writer_has_a_rule_and_closed_flag_sets():
    rules = {"all", "dest", "move", "install", "sed", "dd", "rsync"}
    for name, spec in WRITERS.items():
        assert spec["rule"] in rules, name
        for key in ("short", "short_value", "short_optional"):
            assert isinstance(spec.get(key, ""), str), (name, key)
        for key in ("long", "long_value", "long_optional"):
            assert all(f.startswith("--") for f in spec.get(key, [])), (name, key)


def test_operand_writes_of_a_line_that_does_not_split():
    assert operand_writes("cp 'a b", False) is None
    assert operand_writes("echo 'a b", False) is None

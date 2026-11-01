"""Dialect cases: the kernel's forall_writes and the system dialect's words.

    uv run --no-sync python clients/dialect_cases.py [--out clients/fixtures/dialect]

Three files, all from the Python oracle:

- ``cases.jsonl``: conformance cases (docs/spec/conformance.md) of kind
  ``verify``, and a few of kind ``decompose``. Every verify case carries
  ``expect.word_audit``, the dialect audit warnings in order, empty
  included, so a client that audits where the oracle does not is caught.
  ``clients/compare.py`` runs a client over them.
- ``globs.json``: the glob translation (``dialect.glob_regex``) for a
  fixed set of globs, and the file-scope matcher's answer for each glob
  against a fixed set of normalized paths. The clients test their ports
  of ``glob_regex`` against it.
- ``accesses.json``: the reads and writes (``write_paths.shell_accesses``)
  of the same commands, the readers' own samples and a fixed list of lines
  for the tar, find, xargs and cwd rules, with no base and with one: each
  access as ``[path, tree, via]`` and the reason each half is unknown.
- ``writes.json``: the write paths (``write_paths.step_write_paths``) of
  a fixed set of shell commands, with no base and with one: redirects,
  the operand writes of each command in ``write_paths.WRITERS`` with good
  and bad flags, wrappers, and lines that move their cwd. The clients
  test their ports against it.

Every path is fake. The files hold no content from any real session, are
content-addressed like the conformance corpus, and may be committed. A
manifest pins each file's bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import posixpath
import random
import shlex
from pathlib import Path

from opendaisugi.conformance import canonical_json, case_id, make_decompose_case, make_verify_case
from opendaisugi.dialect import DIALECT_HASH, UnsupportedGlob, glob_regex
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.shell_decompose import decompose_command
from opendaisugi.verify import _match_glob, verify
from opendaisugi.write_paths import READERS, WRITERS, shell_accesses, step_write_paths

REPO = Path(__file__).resolve().parent.parent
OUT = REPO / "clients" / "fixtures" / "dialect"

HEADS = [
    "echo",
    "sh",
    "bash",
    "cat",
    "ls",
    "cp",
    "xargs",
    "python",
    "env",
    "find",
    "timeout",
    "mv",
    "sed",
    "rm",
    "tee",
    "dd",
    "touch",
    "cd",
]

# The base a warden call is placed from (its cwd), for the placed cases.
BASE = "/abs/proj"


def _perms(**over) -> dict:
    perms = {
        "shell": True,
        "shell_allowlist": HEADS,
        "file_write": ["**"],
        "file_read": ["**"],
        "shell_allow_decomposition": True,
        "network": True,
    }
    perms.update(over)
    return perms


def _envelope(invariants=(), postconditions=(), stakes="medium", **perm_over) -> Envelope:
    return Envelope(
        task="dialect case",
        generated_by="clients/dialect_cases.py",
        permissions=_perms(**perm_over),
        invariants=list(invariants),
        postconditions=list(postconditions),
        stakes=stakes,
    )


def _inv(type_name, target=None, **kw) -> dict:
    d = {"type": type_name, "description": "d", **kw}
    if target is not None:
        d["target"] = target
    return d


def _shell(sid, command, **kw) -> dict:
    return {"id": sid, "type": "shell", "command": command, **kw}


def _write(sid, path) -> dict:
    return {"id": sid, "type": "file_write", "path": path, "content": "x"}


PLANS: dict[str, list[dict]] = {
    "ls": [_shell("s1", "ls")],
    "redirect_src": [_shell("s1", "echo x > src/a.py")],
    "redirect_dot_src": [_shell("s1", "echo x >> ./src/a.py 2>/dev/null")],
    "redirect_out": [_shell("s1", "echo x > out/a.txt")],
    "redirect_null": [_shell("s1", "echo x > /dev/null && ls 2>&1")],
    "redirect_abs": [_shell("s1", "echo x > /abs/proj/src/a.py")],
    "redirect_unknown": [_shell("s1", "echo x > $OUT")],
    "redirect_subst": [_shell("s1", "echo x > $(mktemp)")],
    "sh_c": [_shell("s1", "sh -c 'echo x > src/a.py'")],
    "bash_c_nested": [_shell("s1", "bash -c \"sh -c 'echo x > src/deep.py'\"")],
    "xargs": [_shell("s1", "ls | xargs sh -c 'cat > lib/b.py'")],
    "find_exec": [_shell("s1", "find . -name x -exec sh -c 'echo > src/f.py' \\;")],
    "env_wrap": [_shell("s1", "env A=1 sh -c 'echo > src/e.py'")],
    "cp_operand": [_shell("s1", "cp a.py src/a.py")],
    "cp_into_out": [_shell("s1", "cp src/a.py out/")],
    "mv_out_of_src": [_shell("s1", "mv src/a.py out/")],
    "sed_in_place": [_shell("s1", "sed -i 's/a/b/' src/a.py")],
    "sed_no_i": [_shell("s1", "sed 's/a/b/' src/a.py")],
    "rm_rf_src": [_shell("s1", "rm -rf src")],
    "tee_src": [_shell("s1", "echo x | tee -a src/t.py")],
    "dd_of": [_shell("s1", "dd if=/dev/zero of=src/z.bin bs=1 count=1")],
    "touch_bad_flag": [_shell("s1", "touch --frobnicate out/a")],
    "xargs_rm": [_shell("s1", "ls | xargs rm")],
    "cd_then_write": [_shell("s1", "cd out && touch a.py")],
    "cd_abs_write": [_shell("s1", "cd out && touch /abs/proj/out/a.py")],
    "home_write": [_shell("s1", "touch ~/a.py")],
    "python_opaque": [_shell("s1", 'python -c \'open("src/a.py","w")\'')],
    "newline_target": [_shell("s1", "echo x > 'src/a\nb.py'")],
    "write_src": [_write("s1", "lib/../src/b.py")],
    "write_out": [_write("s1", "out/b.txt")],
    "write_abs": [_write("s1", "/abs/proj/src/c.py")],
    "two_steps": [
        _shell("s1", "echo x > out/x"),
        _write("s2", "src/z.py"),
        _shell("s3", "echo > src/y"),
    ],
    "network": [{"id": "s1", "type": "network", "url": "https://example.org/x", "method": "GET"}],
    "empty": [],
    "mixed_ok": [_shell("s1", "ls > out/list.txt"), _write("s2", "docs/readme.md")],
}

# Targets that only a base makes different.
PLACED_INVARIANTS = [
    _inv("keep_unchanged", "../proj/src/**"),
    _inv("keep_unchanged", "./src/*.py"),
    _inv("keep_unchanged", "~/x/**"),
    _inv("keep_unchanged", "src/../x.py"),
    _inv("keep_unchanged", "out/*.py"),
    _inv("keep_unchanged", "/abs/proj/src/**"),
]

WORD_INVARIANTS = [
    _inv("file_unchanged", "src/**"),
    _inv("keep_unchanged", "src/**"),
    _inv("read_only"),
    _inv("no_modifications", "*.py"),
    _inv("file_immutable", "**/*.py"),
    _inv("file_preservation", "/abs/proj/**"),
    _inv("no_file_writes", "/**"),
    _inv("read_only_operations", "src/a.py"),
    _inv("file_unchanged", "[x]"),
    _inv("file_unchanged", "./**"),
    _inv("file_unchanged", ""),
]

KERNEL_EXPRS = {
    "forall_no_src": {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {"op": "not_matches", "path": "path", "regex": "^src/"},
        },
    },
    "exists_writes_nothing": {
        "op": "exists_step",
        "pred": {"op": "forall_writes", "pred": {"op": "exists", "path": "nope"}},
    },
    "nested_in_and": {
        "op": "forall_steps",
        "pred": {
            "op": "and",
            "children": [
                {"op": "exists", "path": "id"},
                {
                    "op": "implies",
                    "a": {"op": "equals", "path": "type", "value": "shell"},
                    "b": {
                        "op": "forall_writes",
                        "pred": {"op": "matches", "path": "path", "regex": "^out/"},
                    },
                },
            ],
        },
    },
    "at_root": {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
    "in_outputs": {
        "op": "forall_outputs",
        "pred": {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
    },
    "nested_twice": {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {"op": "forall_writes", "pred": {"op": "exists", "path": "path"}},
        },
    },
    "tautology": {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {
                "op": "or",
                "children": [
                    {"op": "matches", "path": "path", "regex": "a"},
                    {"op": "not_matches", "path": "path", "regex": "a"},
                ],
            },
        },
    },
    "contradiction_body": {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {
                "op": "and",
                "children": [
                    {"op": "equals", "path": "path", "value": "a"},
                    {"op": "equals", "path": "path", "value": "b"},
                ],
            },
        },
    },
    "len_of_path": {
        "op": "forall_steps",
        "pred": {
            "op": "forall_writes",
            "pred": {"op": "length_range", "path": "path", "min": 0, "max": 12},
        },
    },
}


def _verify_case(plan_steps, envelope, options) -> dict:
    plan = ActionPlan(task="dialect case", source="clients/dialect_cases.py", steps=plan_steps)
    kwargs = {"strict": options.get("strict"), "z3_timeout_ms": options.get("z3_timeout_ms", 500)}
    result = verify(
        plan,
        envelope,
        dialect_pin=options.get("dialect_pin"),
        dialect_base=options.get("dialect_base"),
        **kwargs,
    )
    case = make_verify_case(plan, envelope, options, result)
    case["expect"]["word_audit"] = [w for w in result.warnings if w.startswith("dialect audit: ")]
    body = {k: v for k, v in case.items() if k != "id"}
    case["id"] = case_id(body)
    return case


def verify_cases() -> list[dict]:
    cases: list[dict] = []
    pins = [{}, {"dialect_pin": DIALECT_HASH}, {"dialect_pin": "0000000000000000"}]
    for inv in WORD_INVARIANTS:
        for name, steps in PLANS.items():
            for pin in pins:
                if pin.get("dialect_pin") == "0000000000000000" and name not in (
                    "ls",
                    "redirect_src",
                ):
                    continue
                cases.append(_verify_case(steps, _envelope([inv]), dict(pin)))
    # Placed from a base, as the warden places a call from its cwd.
    for inv in [*WORD_INVARIANTS, *PLACED_INVARIANTS]:
        for steps in PLANS.values():
            for pin in pins[:2]:
                opts = {**pin, "dialect_base": BASE}
                cases.append(_verify_case(steps, _envelope([inv]), opts))
    for base in ("/abs/pr*j", "/", "relative/dir", "/abs//proj/./x/.."):
        cases.append(
            _verify_case(
                PLANS["redirect_src"],
                _envelope([_inv("file_unchanged", "src/**")]),
                {"dialect_base": base},
            )
        )
    # Audit changes no verdict: strict stakes still reject the opaque type.
    for name in ("ls", "redirect_src", "write_out"):
        for pin in pins[:2]:
            cases.append(
                _verify_case(PLANS[name], _envelope([_inv("read_only")], stakes="high"), dict(pin))
            )
    # What is not a word use.
    others = [
        _inv("file_unchanged", "src/**", enforce=False),
        _inv("file_unchanged", "src/**", expr={"op": "exists", "path": "id"}),
        _inv("no_force_push"),
    ]
    for inv in others:
        for pin in pins[:2]:
            cases.append(_verify_case(PLANS["redirect_src"], _envelope([inv]), dict(pin)))
    post = {"type": "file_unchanged", "path": "src/a.py"}
    cases.append(_verify_case(PLANS["redirect_src"], _envelope(postconditions=[post]), {}))
    # Two words in one envelope, both audited, in order.
    two = [_inv("read_only"), _inv("file_unchanged", "src/**")]
    cases.append(_verify_case(PLANS["two_steps"], _envelope(two), {}))
    # A permission deny comes first: no audit.
    cases.append(
        _verify_case(
            PLANS["redirect_src"],
            _envelope([_inv("read_only")], shell_allow_decomposition=False),
            {},
        )
    )
    cases.append(
        _verify_case(
            PLANS["redirect_src"], _envelope([_inv("read_only")], file_write=["out/**"]), {}
        )
    )
    # The kernel quantifier written by hand.
    for expr_name, expr in KERNEL_EXPRS.items():
        for name, steps in PLANS.items():
            if expr_name == "in_outputs":
                steps = [dict(s, metadata={"output": "o"}) for s in steps]
            env = _envelope([_inv(f"k_{expr_name}", expr=expr)])
            cases.append(_verify_case(steps, env, {}))
            if expr_name == "forall_no_src" and name in ("redirect_src", "ls"):
                cases.append(_verify_case(steps, env, {"strict": True}))
    # Numbers compare as Python compares them (RF-13): ints exactly, an
    # int with a float exactly. Ints to 64 bits, which every client's JSON
    # reader holds.
    p53, p63 = 2**53, 2**63
    for n, op, value in [
        (p53 + 1, "equals", p53),
        (-(p53 + 1), "equals", -p53),
        (p53 + 1, "in_set", [p53]),
        (p53 + 1, "not_equals", p53),
        (p53 + 1, "not_in_set", [p53]),
        (p53 + 1, "equals", float(p53)),
        (p53, "equals", float(p53)),
        (p53 + 1, "not_equals", float(p53)),
        (p63 - 1, "equals", p63 - 2),
        (p63 - 1, "equals", float(p63)),
        (2**64 - 1, "equals", 2**64 - 2),
        (True, "equals", 1),
        (3.5, "equals", 3),
    ]:
        key = "values" if op.endswith("in_set") else "value"
        expr = {"op": "forall_steps", "pred": {"op": op, "path": "metadata.n", key: value}}
        steps = [_shell("s1", "ls", metadata={"n": n})]
        cases.append(_verify_case(steps, _envelope([_inv("k_number", expr=expr)]), {}))

    # Skill delegation: the contract is proved inside the caller's envelope
    # by Z3, whose numerals Python builds exactly (RF-14).
    def n_rule(pred):
        return _inv(
            "n_rule",
            expr={
                "op": "forall_steps",
                "pred": {
                    "op": "implies",
                    "a": {"op": "equals", "path": "type", "value": "shell"},
                    "b": {**pred, "path": "metadata.n"},
                },
            },
        )

    p60 = 2**60
    for outer, inner in [
        ({"op": "equals", "value": p53}, {"op": "equals", "value": p53 + 1}),
        ({"op": "in_set", "values": [p53]}, {"op": "in_set", "values": [p53 + 1]}),
        ({"op": "not_in_set", "values": [p53 + 1]}, {"op": "not_in_set", "values": [p53]}),
        (
            {"op": "numeric_range", "min": 0, "max": p53},
            {"op": "numeric_range", "min": 0, "max": p53 + 2},
        ),
        ({"op": "in_set", "values": [1152921504606847000]}, {"op": "equals", "value": p60}),
        ({"op": "numeric_range", "min": 0, "max": 1e19}, {"op": "equals", "value": 2**64 - 1}),
        ({"op": "equals", "value": p53 + 1}, {"op": "equals", "value": p53 + 1}),
    ]:
        contract = _envelope([n_rule(inner)]).model_dump(mode="json")
        steps = [{"id": "s1", "type": "skill", "skill_id": "k", "contract_envelope": contract}]
        cases.append(_verify_case(steps, _envelope([n_rule(outer)]), {}))
    return cases


def decompose_cases() -> list[dict]:
    return [
        make_decompose_case(c, decompose_command(c))
        for c in ("echo x > src/a.py", "ls | xargs sh -c 'cat > lib/b.py'", "echo x > $OUT")
    ]


GLOB_SEGS = [
    "a",
    "b",
    "ab",
    ".",
    "..",
    "",
    "*",
    "?",
    "**",
    "a*",
    "*b",
    "a?b",
    ".x",
    "*.py",
    "\n",
    "é",
    "+",
    "(",
    "$",
    "{",
    "|",
    "\\",
]
PATH_SEGS = [
    "a",
    "b",
    "ab",
    ".",
    "..",
    "x.py",
    "ab.py",
    ".x",
    "c",
    "\n",
    "é",
    "+",
    "(",
    "*",
    "$",
    "\\",
]


def globs_fixture() -> dict:
    rng = random.Random(20260930)
    globs = [
        "**",
        "*",
        "/**",
        "//**",
        "src/**",
        "src/*/**",
        "**/*.py",
        "*.py",
        "/abs/proj/**",
        "a/**/b",
        "**/**",
        "**/src/**",
        "src/a.py",
        "/",
        "",
        "[x]",
        "a]",
        "./**",
        "a/../**",
        "**/**/**/**/**",
        "**/**/**/**/**/x",
        "*" * 16,
        "*" * 17,
        "a" * 1024,
        "a" * 1025,
        "é" * 1024,
        "é" * 1025,
        "a\\b",
        "a.b+c(d)|e{1}$^",
    ]
    while len(globs) < 400:
        g = "/".join(rng.choice(GLOB_SEGS) for _ in range(rng.randint(1, 5)))
        if rng.random() < 0.2:
            g = "/" + g
        globs.append(g)
    paths = [
        "a",
        "/a",
        "src",
        "src/a.py",
        "/abs/proj/src/a.py",
        ".",
        "..",
        "../a",
        "/",
        "//a",
        "a\nb.py",
        "x.py\n",
    ]
    while len(paths) < 120:
        p = "/".join(rng.choice(PATH_SEGS) for _ in range(rng.randint(1, 5)))
        if rng.random() < 0.3:
            p = "/" + p
        paths.append(posixpath.normpath(p))
    paths = sorted(set(paths))
    entries = []
    for g in globs:
        try:
            regex, error = glob_regex(g), None
        except UnsupportedGlob as e:
            regex, error = None, str(e)
        matches = None
        if regex is not None:
            matches = [p for p in paths if _match_glob(p, g)]
        entries.append({"glob": g, "regex": regex, "error": error, "matches": matches})
    return {"paths": paths, "globs": entries}


WRITE_COMMANDS = [
    "ls",
    "echo x > src/a.py",
    "echo x >> ./src/a.py 2>/dev/null",
    "echo x > $OUT",
    "echo x > ~/a",
    "echo 'a > b'",
    "cp a.py src/a.py",
    "cp -r lib src",
    "cp -a x y z/",
    "cp -t out a b/c",
    "cp --target-directory=out a",
    "cp -T a b",
    "cp a",
    "cp -Z a b",
    "cp -t out -T a",
    "/bin/cp a b",
    "A=1 cp a b",
    "A=$x cp a b",
    "cp -- -x y",
    "cp a b -v",
    "cp . out",
    "cp a/ b/.. c",
    "cp -tout a",
    "cp --preserve=mode a b",
    "cp --preserve a b",
    "cp --target-directory out a",
    "cp --target-directory",
    "cp -t",
    "cp --recur a b",
    "ln -s ../lib/x.so lib.so",
    "ln -s ../lib/x.so",
    "ln -s ../lib/",
    "ln -sf a -t dir",
    "mv a.py src/",
    "mv -t out a b",
    "mv -T a b",
    "mv --update=older a b",
    "install -m 755 bin/x /usr/local/bin/x",
    "install -d a b/c",
    "install --directory a",
    "install -Dm644 a /etc/x",
    "tee out.log",
    "tee -a a b",
    "tee --output-error=warn a",
    "tee /dev/null",
    "touch -d 2020-01-01 a b",
    "touch -t 202001010000 a",
    "mkdir -p src/x -m 700",
    "mkdir -pm 700 a",
    "truncate -s 0 log.txt",
    "truncate -r ref.txt a",
    "rm -rf build ./dist",
    "rm -rf /",
    "rm --interactive=never a",
    "rm --frobnicate a",
    "rmdir a",
    "rmdir -p a/b",
    "unlink a",
    "unlink -- -a",
    "shred -u -n 3 secret",
    "shred --remove=wipe a",
    "sed 's/a/b/' src/a.py",
    "sed -i 's/a/b/' src/a.py src/b.py",
    "sed -i -e 's/a/b/' src/a.py",
    "sed -i.bak 's/a/b/' a.py",
    "sed -ie 's/a/b/' a.py",
    "sed --in-place=.orig -f fix.sed a.py",
    "sed --in-place -e s/a/b/ a.py",
    "sed -i'*.bak' 's/a/b/' a.py",
    "sed -i'd/x' 's/a/b/' a.py",
    "sed -ni 's/a/b/p' a.py",
    "sed -E -i -s -e 1d -e 2d a b",
    "sed -i",
    "sed -i -e x",
    "sed -l 80 -i x a",
    "dd if=/dev/zero of=disk.img bs=1M count=1",
    "dd if=a of=/dev/null",
    "dd if=a",
    "dd if=a xyz=1",
    "dd --of=a",
    "dd of=a of=b",
    "dd of==x",
    "rsync -av src/ backup/",
    "rsync -a src host:/srv/",
    "rsync -a --remove-source-files a b host:/srv/",
    "rsync -a --remove-source-files host:a b",
    "rsync -a --exclude '*.o' src dst",
    "rsync -a --backup-dir=old a b",
    "rsync -e ssh a rsync://h/m",
    "rsync -a ./a:b c",
    "rsync -a --info=progress2 a b",
    "rsync src",
    "rsync -T tmp a b",
    "cp a $DEST",
    "cp *.py out",
    "rm -f ~/x",
    "rm -f '~/x'",
    "rm -f ~user/x",
    "touch a{1,2}",
    "touch 'a{1,2}'",
    'touch "$HOME/a"',
    "touch `pwd`/a",
    "ls -la src",
    "git commit -m x",
    "cp 'a b",
    "echo 'a b",
    "sh -c 'cp a src/b'",
    "timeout 5 rm -f src/a",
    "env A=1 touch src/a",
    "nohup tee log < in",
    "ls && rm x > y",
    "ls | xargs rm",
    "ls | xargs -I{} cp {} out",
    "find . -name '*.o' -exec rm {} ';'",
    "find . -exec touch x ';'",
    "ls | xargs sh -c 'echo > a'",
    "ls | xargs sh -c 'rm a'",
    "ls | xargs env touch a",
    "python -c 'import os'",
    "sudo rm -rf /",
    "command rm a",
    "nice -n 5 cp a b",
    "cd src && echo x > a.py",
    "cd src && touch a.py",
    "cd /repo && echo x > /repo/a.py",
    "pushd src; ls",
    "popd; rm a",
    "sh -c 'cd src' && touch a",
    "cd src && sh -c 'touch a'",
    "sh -c 'cd src && touch a'",
    "x=$(cd a && pwd); touch b",
    "bash -c \"sh -c 'cp a src/deep.py'\"",
    "echo x > ../up.txt",
    "echo x > /abs/a",
    "touch ../../a ./b //c",
]


def _write_samples(rng: random.Random) -> list[str]:
    """Commands built from each writer's own flags, good and bad."""
    words = ["a", "src/a.py", "out/", "../x", "/abs/y", "-", "--", "b c", "", "."]
    out: list[str] = []
    for name, spec in sorted(WRITERS.items()):
        flags = [f"-{c}" for c in spec.get("short", "")]
        flags += [f"-{c} v" for c in spec.get("short_value", "")]
        flags += [f"-{c}v" for c in spec.get("short_value", "")]
        flags += [f"-{c}v" for c in spec.get("short_optional", "")]
        flags += list(spec.get("long", []))
        flags += [f"{f}=v" for f in spec.get("long_value", []) + spec.get("long_optional", [])]
        flags += [f"{f} v" for f in spec.get("long_value", [])]
        flags += ["-Q", "--nope"]
        if name == "dd":
            flags = ["if=a", "of=b", "bs=1", "of=/dev/null", "x=1", "-v", "of="]
        if name == "tar":
            flags = [
                "-c",
                "-x",
                "-t",
                "-f a.tgz",
                "-C out",
                "-xzf in.tgz",
                "-czf out.tgz",
                "-P",
                "--create",
            ]
        for _ in range(12):
            parts = [name]
            for _ in range(rng.randint(0, 3)):
                parts.append(rng.choice(flags))
            for _ in range(rng.randint(0, 3)):
                w = rng.choice(words)
                parts.append(shlex.quote(w) if w else "''")
            out.append(" ".join(parts))
    return out


ACCESS_COMMANDS = [
    "cp /work/a /etc/x",
    "cp a b",
    "cp -r src out",
    "cp -a src out/",
    "cp -t out a b",
    "mv a /etc/x",
    "tee /etc/cron.d/x",
    "tee -a log -",
    "sed -i s/a/b/ /etc/hosts",
    "sed -i.bak s/a/b/ f",
    "sed -n 1p a",
    "sed -e s/a/b/ -f script a",
    "cat /etc/passwd",
    "cat a b",
    "cat",
    "cat -n a",
    "head -n 5 a",
    "head -5 a",
    "tail -f log",
    "grep -rn foo src",
    "grep -r foo",
    "grep foo a b",
    "grep -e foo -f pats a",
    "grep -R x .",
    "rg foo",
    "rg -e x src",
    "ls",
    "ls -R /home/user",
    "ls -la a b",
    "find / -name '*.pem'",
    "find . -name '*.py' -exec cat {} +",
    "find /work -type f -delete",
    "find src -name x -fprint /etc/out",
    "find src -fprintf out %p",
    "find src -newerXY x",
    "find -L /x -name y",
    "find src -exec tee x {} +",
    "find src -execdir cat ../../x {} ;",
    "find /work -execdir cat x {} ;",
    "find /work -exec cat /etc/hosts {} ;",
    "find src ( -name a -o -name b ) -print",
    "tar -czf out.tgz src",
    "tar czf out.tgz src",
    "tar -xzf in.tgz -C out",
    "tar -xf /work/evil.tgz",
    "tar -tf in.tgz",
    "tar -cf - src",
    "tar -xPf evil.tgz",
    "tar -xf evil.tgz -C a -C b",
    "tar -cf o.tgz x -C src y",
    "tar -x",
    "tar --create --file=o.tgz --directory=d src",
    "tar -xf in.tgz --strip-components=1",
    "dd if=a of=/dev/sda",
    "cat a > out.txt",
    "cat < in > out",
    "echo x > /dev/null",
    "cd /work/src && cp a b",
    "cd /etc && tee hosts",
    "cd /etc; cd /work && tee hosts",
    "pushd /etc && tee hosts",
    "builtin cd /etc && tee hosts",
    "env --chdir=/etc tee hosts",
    "env -C /etc tee hosts",
    "env A=1 cat a",
    "sh -c 'cp /work/a /etc/x'",
    "sh -c 'cd src && cat a'",
    "cd a && sh -c 'cd b && cat c'",
    "ls /work | xargs echo",
    "ls | xargs cat",
    "ls | xargs rm",
    "find / -name '*.pem' | xargs cat",
    "xargs -a /etc/hosts echo",
    "cp a.key /home/user/*",
    "sort -o out a",
    "sort a b",
    "cat ~/x",
    "cat ~alice/x",
    "cat $HOME/x",
    "rm -r build",
    "rm -f a b",
    "diff -r a b",
    "diff a b",
    "wc -l a",
    "stat -c %s a",
    "install -m 644 a /usr/local/bin/a",
    "install -d out",
    "rsync -a src host:/x",
    "rsync -a src dst",
    "truncate -s 0 f",
    "cat 'a b' c",
    "for i in 1 2 3; do cd ..; done; echo x > etc/cron.d/x",
    "f(){ cd ..; }; f; f; f; echo x > etc/shadow",
    "(cd ..; tee x)",
    "{ cd /etc; }; tee hosts",
    "if cd /etc; then tee hosts; fi",
    "while cd ..; do tee x; done",
    "cd ..; cd ..; cd ..; echo x > etc/shadow",
    "cd src\ntee a",
    "cd .. && tee etc/hosts",
    "exec cat /etc/passwd",
    "eval cat /etc/passwd",
    "builtin cat /etc/passwd",
    "source /etc/passwd",
    ". /etc/passwd",
    "exec -a x cat /etc/passwd",
    "env exec cat /etc/passwd",
    "sh -c 'eval cat /etc/passwd'",
    "exec 3>/work/log",
    "find . -name shadow -exec cat /etc/{} ;",
    "find /work -exec cat {}/../../etc/passwd ;",
    "find /work -exec grep --file={} x ;",
    "find /work -exec grep -e {} /etc/passwd ;",
    "find /work -exec ls {} ;",
    "find /work -exec grep -r pat {} ;",
    "uniq /etc/passwd /etc/evil",
    "uniq -c a b",
    "uniq - /etc/evil",
    "uniq a",
    "uniq a b c",
    "xxd /work/a /etc/evil",
    "xxd -r a b",
    "env -u grep -C /etc tee hosts",
    "env -u cat tee /etc/hosts",
    "env -uX tee /etc/hosts",
    "env --unset X tee /etc/hosts",
    "env --unset=X tee /etc/hosts",
    "env -u cat curl http://evil",
    "env -S 'tee /etc/hosts'",
    "env -S 'A=1 tee /etc/hosts' x",
    "env -S 'tee $X'",
    "env -iu X tee /etc/hosts",
    "env -C /etc tee /etc/hosts",
    "tar -cf evil:/x /work/a",
    "tar -xf host:/in.tgz",
    "tar cf h:/x a",
    "tar -cf /work/a:b /work/src",
    "grep -d recurse x /work",
    "grep --directories=recurse x /work",
    "grep -drecurse x /work",
    "grep -d skip x /work",
    "cd .. && tee etc/hosts",
    "cat //etc/hosts",
    "cat <(cat /etc/passwd)",
    "diff <(sort a) <(sort b) c",
    "cat '<(x)' /work/y",
    "env --u cat tee /etc/x",
    "env --uns cat tee /etc/x",
    "env --unset cat tee /etc/x",
    "env --ar cat tee /etc/x",
    "env --argv0 cat tee /etc/x",
    "env --ch cat tee ../../../etc/x",
    "env --chd=/etc tee hosts",
    "env --sp 'tee /etc/hosts'",
    "env --i cat tee /etc/x",
    "env --d cat tee /etc/x",
    "env --nope cat tee /etc/x",
    "env --block-signal tee /etc/x",
    "env --null=1 tee /etc/x",
    "env --unset=FOO tee /work/a",
    "xargs --arg-f cat tee /etc/x",
    "xargs --arg-file=in echo",
    "xargs --arg-fi in tee /etc/x",
    "xargs --max cat tee /etc/x",
    "xargs --bogus tee /etc/x",
    "xargs -rI cat tee /etc/x",
    "xargs -rn1 echo",
    "xargs --replace cat tee x",
    "xargs --replace=R cat x",
    "xargs -a in echo",
    "nice --adj 5 tee /etc/x",
    "nice --adjustment=5 tee /etc/x",
    "nice --nope tee /etc/x",
    "timeout --kill 5 30 tee /etc/x",
    "timeout --kill-after 5 30 tee /etc/x",
    "timeout --foreground 30 tee /etc/x",
    "timeout --nope 30 tee /etc/x",
    "stdbuf --output=L tee /etc/x",
    "stdbuf -oL tee /etc/x",
    "stdbuf --nope tee /etc/x",
    "nohup --nope tee /etc/x",
    "nohup tee /etc/x",
    "setsid --fork tee /etc/x",
    "setsid --nope tee /etc/x",
    "ionice -t tee cat /etc/passwd",
    "ionice -t tee /etc/x",
    "ionice -c 2 -n 5 tee /etc/x",
    "ionice -c2 -n5 tee /etc/x",
    "ionice --ignore tee /etc/x",
    "ionice --nope tee /etc/x",
    "time -o /etc/x ls",
    "time -o cat tee /etc/x",
    "time --output=/etc/x ls",
    "time --output /etc/x ls",
    "time -a -o /etc/x ls",
    "time -f %e cat x",
    "time -p cat x",
    "time --nope cat x",
    "sort -o /etc/x in",
    "sort --output=/etc/x in",
    "sort -r in",
    "less -o /etc/x",
    "less -N f",
    "file -C -m x",
    "cat --bogus a",
    "sed -n 's/a/b/w /etc/x' in",
    "sed -n 'w /etc/x' in",
    "sed -n '1W /etc/x' in",
    "sed 's/a/b/e' in",
    "sed '1e echo' in",
    "sed 'r /etc/shadow' in",
    "sed 'R /etc/shadow' in",
    "sed -e 's/a/b/' -e 'w /etc/x' in",
    "sed --expression='w /etc/x' in",
    "sed -f prog.sed in",
    "sed --file=prog.sed in",
    "sed -i 's/a/b/w x' in",
    "sed -n 's/world/x/g' in",
    "sed -n '1,5p' in",
    "sed -n '$!d' in",
    "sed -n '/x/,/y/{p;d}' in",
    "sed -n 'y/abc/xyz/' in",
    "sed -n 'a text; w x' in",
    "sed -n 's/[/]/x/w f' in",
    "sed -n 's/a/b/gw f' in",
    "sed -i 's/a/b/' in",
    "sed ':a;N;$!ba;s/\\n/ /g' in",
    "sed -n 'p;p' in",
    "chroot /jail tee /etc/x",
    "chroot",
    "flock /tmp/l tee /etc/x",
    "flock -c 'tee /etc/x' /tmp/l",
    "busybox tee /etc/x",
    "script -c 'tee /etc/x' /dev/null",
    "parallel tee ::: /etc/x",
    "coproc tee /etc/x",
    "trap 'tee /etc/x' EXIT",
    "trap",
    "cmd",
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
    "od -w /etc/shadow",
    "od -w16 /etc/shadow",
    "od -S 4 a",
    "hexdump -f /etc/shadow a",
    "hexdump -C a",
    "rg --color never secret",
    "rg --color=never secret src",
    "md5sum -c sums",
    "sha256sum --check sums",
    "sha256sum a b",
    "expect",
    "crontab",
    "at",
    "ls --color=auto -la",
    "diff --color=auto a b",
    "diff -u --color=always a b",
    "/usr/bin/env tee /etc/x",
    "/usr/bin/nohup tee /etc/x",
    "/usr/bin/sh -c 'tee /etc/x'",
    "./timeout 5 tee /etc/x",
    "tmux ; send-keys x",
    "tmux ls ';' run x",
    "tmux ls \\; run x",
]


def _access_samples(rng: random.Random) -> list[str]:
    """Commands built from each reader's and writer's own flags, good and bad."""
    words = ["a", "src/a.py", "out/", "../x", "/abs/y", "-", "--", "b c", ".", "/etc/hosts"]
    out: list[str] = []
    for name in sorted(set(READERS) | set(WRITERS)):
        spec = READERS.get(name, {})
        if name in WRITERS and name not in ("sed",):
            spec = WRITERS[name]
        elif name == "sed":
            spec = WRITERS["sed"]
        flags = [f"-{c}" for c in spec.get("short", "")][:12]
        flags += [f"-{c} v" for c in spec.get("short_value", "")][:6]
        flags += list(spec.get("long", []))[:6]
        flags += [f"{f}=v" for f in spec.get("long_value", []) + spec.get("long_optional", [])][:6]
        flags += ["-Q", "--nope", "-r", "-R", "-e x", "-f pats"]
        if name == "dd":
            flags = ["if=a", "of=b", "bs=1", "of=/dev/null", "if=/etc/hosts", "-v"]
        if name == "find":
            flags = ["-name x", "-delete", "-type f", "-fprint o", "-exec cat {} ;", "-bogus"]
        if name == "tar":
            flags = ["-c", "-x", "-t", "-f a.tgz", "-C out", "-xzf in.tgz", "-czf o.tgz", "-P"]
        for _ in range(6):
            parts = [name]
            for _ in range(rng.randint(0, 3)):
                parts.append(rng.choice(flags))
            for _ in range(rng.randint(0, 3)):
                w = rng.choice(words)
                parts.append(shlex.quote(w) if w else "''")
            out.append(" ".join(parts))
    return out


def _access_pairs(accesses) -> list[list]:
    return [[a.path, a.tree, a.via] for a in accesses]


def accesses_fixture() -> dict:
    rng = random.Random(20261030)
    commands = list(dict.fromkeys(ACCESS_COMMANDS + WRITE_COMMANDS + _access_samples(rng)))
    cases = []
    for command in commands:
        for base in (None, BASE):
            acc = shell_accesses(command, base)
            cases.append(
                {
                    "command": command,
                    "base": base,
                    "reads": _access_pairs(acc.reads),
                    "writes": _access_pairs(acc.writes),
                    "reads_unknown": acc.reads_unknown,
                    "writes_unknown": acc.writes_unknown,
                }
            )
    return {"v": 1, "base": BASE, "cases": cases}


def writes_fixture() -> dict:
    rng = random.Random(20260930)
    commands = list(dict.fromkeys(WRITE_COMMANDS + _write_samples(rng)))
    entries = []
    for command in commands:
        step = {"id": "s1", "type": "shell", "command": command}
        entries.append(
            {
                "command": command,
                "writes": step_write_paths(step),
                "placed": step_write_paths(step, BASE),
            }
        )
    return {"base": BASE, "commands": entries}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", type=Path, default=OUT)
    args = ap.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    cases = {c["id"]: c for c in verify_cases() + decompose_cases()}
    lines = "".join(canonical_json(cases[k]) + "\n" for k in sorted(cases))
    (args.out / "cases.jsonl").write_text(lines, encoding="utf-8")
    globs = json.dumps(globs_fixture(), indent=1, ensure_ascii=True) + "\n"
    (args.out / "globs.json").write_text(globs, encoding="utf-8")
    writes = json.dumps(writes_fixture(), indent=1, ensure_ascii=True) + "\n"
    (args.out / "writes.json").write_text(writes, encoding="utf-8")
    accesses = json.dumps(accesses_fixture(), indent=1, ensure_ascii=True) + "\n"
    (args.out / "accesses.json").write_text(accesses, encoding="utf-8")
    manifest = {
        "v": 1,
        "count": len(cases),
        "accesses_sha256": hashlib.sha256(accesses.encode("utf-8")).hexdigest(),
        "cases_sha256": hashlib.sha256(lines.encode("utf-8")).hexdigest(),
        "globs_sha256": hashlib.sha256(globs.encode("utf-8")).hexdigest(),
        "writes_sha256": hashlib.sha256(writes.encode("utf-8")).hexdigest(),
    }
    (args.out / "cases.manifest.json").write_text(json.dumps(manifest, indent=1) + "\n")
    kinds: dict[str, int] = {}
    for c in cases.values():
        kinds[c["kind"]] = kinds.get(c["kind"], 0) + 1
    print(f"{len(cases)} cases {kinds}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

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
from opendaisugi.write_paths import WRITERS, step_write_paths

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

# The base a gate call is placed from (its cwd), for the placed cases.
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
    # Placed from a base, as the gate places a call from its cwd.
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
        for _ in range(12):
            parts = [name]
            for _ in range(rng.randint(0, 3)):
                parts.append(rng.choice(flags))
            for _ in range(rng.randint(0, 3)):
                w = rng.choice(words)
                parts.append(shlex.quote(w) if w else "''")
            out.append(" ".join(parts))
    return out


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
    manifest = {
        "v": 1,
        "count": len(cases),
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

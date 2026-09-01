"""The effect class of one tool call, and the tier of an ask about it.

The gate asks the operator about a call the envelope denied. The tier says
how careful that answer must be:

- ``silent``: the envelope allowed the call. Nobody is asked.
- ``undoable``: the call reads, writes inside the workspace, does a network
  get, or runs tests. One key allows it.
- ``permanent``: every other call. The operator types the pane name to
  allow it.

The classifier is an allowlist. A call it cannot place is ``unknown``, and
``unknown`` is permanent. A shell command is placed only through the shell
decomposition, so without the ``opendaisugi[shell]`` extra every shell call
is ``unknown``.
"""

from __future__ import annotations

import os
import re
import shlex
from dataclasses import dataclass
from typing import Any, NamedTuple

SILENT = "silent"
UNDOABLE = "undoable"
PERMANENT = "permanent"

UNDOABLE_CLASSES = frozenset({"read", "write_inside_workspace", "network_get", "test_run"})

# Worst first. When one call has more than one effect, the worst one names
# the call.
_SEVERITY = (
    "credential",
    "prod",
    "force_push",
    "delete",
    "push_shared",
    "unknown",
    "write_git_dir",
    "read_git_dir",
    "write_outside_workspace",
    "read_outside_workspace",
    "write_inside_workspace",
    "network_get",
    "test_run",
    "read",
)

_CREDENTIAL_DIRS = frozenset(
    {".ssh", ".gnupg", ".aws", ".kube", ".docker", ".password-store", ".azure", ".config/gcloud"}
)
_CREDENTIAL_FILES = frozenset(
    {
        ".netrc",
        ".git-credentials",
        ".pypirc",
        ".npmrc",
        "credentials",
        "credentials.json",
        ".env",
    }
)

_CWD_HEADS = frozenset({"cd", "pushd", "popd"})
_DELETE_HEADS = frozenset({"rm", "rmdir", "shred", "unlink", "truncate", "dd"})
_PROD_HEADS = frozenset(
    {
        "kubectl",
        "helm",
        "terraform",
        "pulumi",
        "ansible",
        "ansible-playbook",
        "fly",
        "flyctl",
        "heroku",
        "vercel",
        "netlify",
        "gcloud",
        "aws",
        "az",
        "doctl",
        "ssh",
        "scp",
        "rsync",
    }
)
_CREDENTIAL_HEADS = frozenset({"gpg", "pass", "ssh-keygen", "ssh-add", "security", "op"})
_PUBLISH = {
    "npm": "publish",
    "pnpm": "publish",
    "yarn": "publish",
    "cargo": "publish",
    "uv": "publish",
    "twine": "upload",
    "docker": "push",
    "podman": "push",
}


def shell_parser_available() -> bool:
    """True when the shell decomposition can parse a command here."""
    try:
        from opendaisugi.shell_decompose import parser_available
    except Exception:  # noqa: BLE001 - no decomposition means no parser
        return False
    return parser_available()


def tier_for(effect: str) -> str:
    """The tier of a denied call with this effect class."""
    return UNDOABLE if effect in UNDOABLE_CLASSES else PERMANENT


def _worst(classes: list[str]) -> str:
    if not classes:
        return "unknown"
    rank = {name: i for i, name in enumerate(_SEVERITY)}
    return min(classes, key=lambda c: rank.get(c, -1))


def _resolve(path: str, cwd: str | None) -> str | None:
    """The one path normalizer every path check uses: ~ expanded, joined
    with the call's cwd, and ../ and symlinks followed with realpath, the
    way the OS will open it. None for a relative path with no known cwd."""
    expanded = os.path.expanduser(path)
    if not os.path.isabs(expanded):
        if not cwd or not os.path.isabs(cwd):
            return None
        expanded = os.path.join(cwd, expanded)
    return os.path.realpath(expanded)


def _credential_text(norm: str) -> bool:
    """True when a normalized path names a credential."""
    parts = [p for p in norm.split("/") if p]
    for i, part in enumerate(parts):
        if part in _CREDENTIAL_DIRS:
            return True
        if i + 1 < len(parts) and f"{part}/{parts[i + 1]}" in _CREDENTIAL_DIRS:
            return True
    if not parts:
        return False
    # Any process's environment holds its secrets, at any depth under /proc.
    if norm.startswith("/") and parts[0] == "proc" and "environ" in parts[1:]:
        return True
    # Tools keep their tokens under the home config directory.
    if norm.startswith("/"):
        home = os.path.expanduser("~")
        for h in {os.path.normpath(home), os.path.realpath(home)}:
            if _under(norm, os.path.join(h, ".config")):
                return True
    name = parts[-1]
    if name.casefold().endswith((".pem", ".key")):
        return True
    return name in _CREDENTIAL_FILES or name.startswith(".env.")


def _is_credential_path(path: str, cwd: str | None = None) -> bool:
    """True when path, as typed or as the OS resolves it from cwd, names a
    credential. Either match is enough."""
    typed = os.path.normpath(os.path.expanduser(path)).replace("\\", "/")
    if _credential_text(typed):
        return True
    resolved = _resolve(path, cwd)
    return resolved is not None and _credential_text(resolved)


def _under(path: str, base: str) -> bool:
    try:
        return os.path.commonpath([path, base]) == base
    except ValueError:
        return False


class Workspace(NamedTuple):
    """Where a call runs and where its writes must land.

    ``cwd`` is the directory a relative path resolves against. ``base`` is
    the root a path must land inside to count as inside the workspace.
    """

    cwd: str
    base: str


def _as_workspace(root: Workspace | str | None) -> Workspace | None:
    if isinstance(root, Workspace):
        return root
    if isinstance(root, str) and root:
        return Workspace(root, root)
    return None


def _inside(path: str, root: Workspace | None) -> bool:
    """True when path lands inside the workspace. No workspace means not
    inside."""
    if root is None or not os.path.isabs(root.cwd) or not os.path.isabs(root.base) or not path:
        return False
    expanded = os.path.expanduser(path)
    if "~" in expanded or "$" in expanded:
        return False
    target = os.path.realpath(os.path.join(root.cwd, expanded))
    base = os.path.realpath(root.base)
    try:
        return os.path.commonpath([target, base]) == base
    except ValueError:
        return False


def _write_class(path: str, root: Workspace | None, cwd: str | None = None) -> str:
    if _under_proc(os.path.normpath(os.path.expanduser(path))):
        return "unknown"
    resolved = _resolve(path, root.cwd if root else cwd)
    if resolved is not None and _under_proc(resolved):
        return "unknown"
    if _is_credential_path(path, root.cwd if root else cwd):
        return "credential"
    if not _inside(path, root):
        return "write_outside_workspace"
    # The repo's own .git holds the history and the checkpoints that make
    # every other write here undoable, so a write into it is not.
    target = os.path.realpath(os.path.join(root.cwd, os.path.expanduser(path)))
    rel = os.path.relpath(target, os.path.realpath(root.base))
    if any(part.casefold() == ".git" for part in rel.split(os.sep)):
        return "write_git_dir"
    return "write_inside_workspace"


def _under_proc(path: str) -> bool:
    return _under(path, "/proc")


def _read_place(path: str, base: str | None, cwd: str | None) -> str:
    """The class of a read of path, by where it lands. A read is a plain
    read only when the path resolves inside the workspace, outside its .git,
    and not under /proc. The credential list is a second net inside the
    workspace."""
    typed = os.path.normpath(os.path.expanduser(path))
    if _under_proc(typed):
        return "unknown"
    if _credential_text(typed):
        return "credential"
    resolved = _resolve(path, cwd)
    if resolved is None:
        return "read_outside_workspace"
    if _under_proc(resolved):
        return "unknown"
    if _credential_text(resolved):
        return "credential"
    if base is None:
        return "read_outside_workspace"
    real_base = os.path.realpath(base)
    if not _under(resolved, real_base):
        return "read_outside_workspace"
    rel = os.path.relpath(resolved, real_base)
    if any(part.casefold() == ".git" for part in rel.split(os.sep)):
        return "read_git_dir"
    return "read"


def _read_class(path: str, cwd: str | None = None, base: str | None = None) -> str:
    return _read_place(path, base, cwd)


@dataclass(frozen=True)
class Flags:
    """The closed set of flags one command may carry and keep its class.

    ``short`` and ``short_value`` are single letters: ones that take no
    value and ones that take a value. ``long`` and ``long_value`` are
    whole flags with their dashes, the same split. ``long_optional`` and
    ``short_optional`` take a value only when it is attached, with "=" or in
    the same word, and never the next word. A go-style command
    spells long flags with one dash, and ``single_dash`` says so. ``numeric``
    lets ``-5`` stand for a count. ``positional`` is False when the command
    takes no operands. Any flag outside these sets makes the command
    unknown.
    """

    short: str = ""
    short_value: str = ""
    long: frozenset[str] = frozenset()
    long_value: frozenset[str] = frozenset()
    long_optional: frozenset[str] = frozenset()
    short_optional: str = ""
    single_dash: bool = False
    numeric: bool = False
    positional: bool = True


def _flags(
    short: str = "",
    short_value: str = "",
    long: tuple[str, ...] = (),
    long_value: tuple[str, ...] = (),
    long_optional: tuple[str, ...] = (),
    short_optional: str = "",
    **kw: bool,
) -> Flags:
    return Flags(
        short,
        short_value,
        frozenset(long),
        frozenset(long_value),
        frozenset(long_optional),
        short_optional,
        **kw,
    )


def _operands(args: list[str], spec: Flags) -> list[str] | None:
    """The operands of args, or None when any flag is outside spec."""
    out: list[str] = []
    i = 0
    ended = False
    while i < len(args):
        a = args[i]
        i += 1
        if ended or a == "-" or not a.startswith("-"):
            if not spec.positional:
                return None
            out.append(a)
            continue
        if a == "--":
            ended = True
            continue
        if spec.numeric and a[1:].isdigit():
            continue
        if a.startswith("--") or spec.single_dash:
            name, eq, _ = a.partition("=")
            if spec.single_dash:
                name = "-" + name.lstrip("-")
            if eq:
                if name not in spec.long_value and name not in spec.long_optional:
                    return None
                continue
            # A flag whose value is optional takes it only after "=". The
            # next word is never its value.
            if name in spec.long or name in spec.long_optional:
                continue
            if name in spec.long_value:
                if i >= len(args):
                    return None
                i += 1
                continue
            return None
        body = a[1:]
        for j, c in enumerate(body):
            if c in spec.short:
                continue
            if c in spec.short_optional:
                break
            if c in spec.short_value:
                if j + 1 == len(body):
                    if i >= len(args):
                        return None
                    i += 1
                break
            return None
    return out


# Read-only commands, each with the flags that keep it read-only. No flag
# here runs a program, names an output file, or removes anything.
_READ_FLAGS: dict[str, Flags] = {
    "ls": _flags(
        short="aAlhtrSd1FisGnogcuUvXx",
        long=(
            "--all",
            "--almost-all",
            "--human-readable",
            "--directory",
            "--classify",
            "--group-directories-first",
        ),
        long_value=("--sort", "--time-style"),
        long_optional=("--color",),
    ),
    "cat": _flags(short="nbsAETv", long=("--number", "--show-all")),
    "head": _flags(
        short="qvz",
        short_value="nc",
        long=("--quiet", "--verbose"),
        long_value=("--lines", "--bytes"),
        numeric=True,
    ),
    "tail": _flags(
        short="qvzfF",
        short_value="nc",
        long=("--quiet", "--verbose", "--follow"),
        long_value=("--lines", "--bytes"),
        numeric=True,
    ),
    "grep": _flags(
        short="ivnlLcwxEFPosqHhIzab",
        short_value="eABCm",
        long=(
            "--ignore-case",
            "--line-number",
            "--count",
            "--files-with-matches",
            "--files-without-match",
            "--invert-match",
            "--word-regexp",
            "--fixed-strings",
            "--extended-regexp",
            "--perl-regexp",
            "--only-matching",
            "--quiet",
            "--no-filename",
            "--with-filename",
        ),
        long_value=(
            "--include",
            "--exclude",
            "--exclude-dir",
            "--max-count",
            "--regexp",
            "--context",
            "--after-context",
            "--before-context",
        ),
        long_optional=("--color", "--colour"),
    ),
    "wc": _flags(short="lwcmL"),
    "pwd": _flags(short="LP", positional=False),
    "echo": _flags(short="neE"),
    "printf": _flags(),
    "stat": _flags(short="Ltf", short_value="c", long_value=("--format", "--printf")),
    "file": _flags(short="bLiz", long=("--mime", "--brief")),
    "sort": _flags(
        short="bdfgiMhnRrVucsz",
        short_value="kt",
        long=(
            "--numeric-sort",
            "--reverse",
            "--unique",
            "--human-numeric-sort",
            "--version-sort",
            "--ignore-case",
            "--stable",
        ),
        long_value=("--key", "--field-separator"),
    ),
    "cut": _flags(
        short="sz",
        short_value="dfbc",
        long=("--complement", "--only-delimited"),
        long_value=("--delimiter", "--fields", "--bytes", "--characters"),
    ),
    "diff": _flags(
        short="uqNwbBiyas",
        short_value="U",
        long=(
            "--brief",
            "--new-file",
            "--ignore-all-space",
            "--side-by-side",
            "--text",
        ),
        long_value=("--unified",),
        long_optional=("--color",),
    ),
    "which": _flags(short="a"),
    "du": _flags(
        short="shackmxb",
        short_value="d",
        long=("--summarize", "--human-readable", "--all", "--total", "--apparent-size"),
        long_value=("--max-depth",),
    ),
    "df": _flags(short="hTkail", long=("--human-readable", "--print-type")),
    "whoami": _flags(positional=False),
    "true": _flags(),
    "false": _flags(),
    "basename": _flags(short="az", short_value="s"),
    "dirname": _flags(short="z"),
    "realpath": _flags(short="emsqz"),
    "readlink": _flags(short="femnqsz"),
    "jq": _flags(
        short="rcnesSCMaj",
        long=(
            "--raw-output",
            "--compact-output",
            "--null-input",
            "--exit-status",
            "--slurp",
            "--sort-keys",
            "--color-output",
            "--monochrome-output",
            "--ascii-output",
            "--join-output",
        ),
    ),
    "cd": _flags(),
    "pushd": _flags(),
    "popd": _flags(positional=False),
}

# Commands whose operands are paths they create.
_MAKE_FLAGS: dict[str, Flags] = {
    "mkdir": _flags(
        short="pv", short_value="m", long=("--parents", "--verbose"), long_value=("--mode",)
    ),
    "touch": _flags(short="acm", short_value="dtr", long_value=("--date", "--reference")),
}

# Reading these recursively from home, or from above it, reads the
# credentials that live there.
_RECURSIVE_READERS = frozenset({"grep", "find", "du", "ls"})

# find: the tests that take a value, and the ones that take none. Every
# action that runs a program, writes a file, or deletes is left out.
_FIND_VALUE = frozenset(
    {
        "-name",
        "-iname",
        "-path",
        "-ipath",
        "-wholename",
        "-iwholename",
        "-type",
        "-xtype",
        "-maxdepth",
        "-mindepth",
        "-newer",
        "-mtime",
        "-mmin",
        "-atime",
        "-amin",
        "-ctime",
        "-cmin",
        "-size",
        "-regex",
        "-iregex",
        "-user",
        "-group",
        "-perm",
    }
)
_FIND_PLAIN = frozenset(
    {
        "-print",
        "-print0",
        "-empty",
        "-not",
        "-o",
        "-a",
        "-and",
        "-or",
        "-prune",
        "-true",
        "-false",
        "-readable",
        "-writable",
        "-executable",
        "-depth",
        "-xdev",
        "-mount",
        "-P",
        "(",
        ")",
        "!",
        ",",
    }
)

_PYTEST = _flags(
    short="qvxsl",
    short_value="kmrWn",
    long=(
        "--lf",
        "--last-failed",
        "--ff",
        "--failed-first",
        "--nf",
        "--sw",
        "--stepwise",
        "--no-header",
        "--co",
        "--collect-only",
        "--quiet",
        "--verbose",
        "--exitfirst",
        "--showlocals",
    ),
    long_value=(
        "--maxfail",
        "--tb",
        "--durations",
        "--deselect",
        "--ignore",
        "--ignore-glob",
        "--color",
        "--capture",
        "--import-mode",
        "--timeout",
    ),
)
_RUFF_CHECK = _flags(
    short="q",
    long=("--quiet", "--no-cache", "--statistics", "--preview", "--diff"),
    long_value=(
        "--select",
        "--ignore",
        "--extend-select",
        "--extend-ignore",
        "--output-format",
        "--target-version",
        "--line-length",
    ),
)
_RUFF_FORMAT = _flags(
    short="q",
    long=("--check", "--diff", "--quiet", "--preview", "--no-cache"),
    long_value=("--line-length", "--target-version"),
)
_MYPY = _flags(
    long=(
        "--strict",
        "--ignore-missing-imports",
        "--show-error-codes",
        "--pretty",
        "--no-error-summary",
        "--check-untyped-defs",
    ),
    long_value=("--python-version",),
)
_GO_TEST = _flags(
    single_dash=True,
    long=("-v", "-race", "-short", "-cover", "-failfast", "-json", "-benchmem"),
    long_value=(
        "-run",
        "-count",
        "-timeout",
        "-p",
        "-bench",
        "-cpu",
        "-tags",
        "-skip",
        "-parallel",
    ),
)
_GO_VET = _flags(single_dash=True, long=("-v", "-json"), long_value=("-tags",))
_CARGO_TEST = _flags(
    short="q",
    short_value="pj",
    long=(
        "--release",
        "--quiet",
        "--all",
        "--workspace",
        "--lib",
        "--bins",
        "--no-fail-fast",
        "--doc",
        "--all-features",
        "--no-default-features",
        "--locked",
        "--frozen",
        "--offline",
    ),
    long_value=("--package", "--test", "--features", "--jobs"),
)
_LIBTEST = _flags(
    short="q",
    long=("--nocapture", "--ignored", "--include-ignored", "--exact", "--show-output", "--quiet"),
    long_value=("--test-threads", "--skip"),
)
_UV_RUN = _flags(short="q", long=("--no-sync", "--frozen", "--locked", "--offline", "--quiet"))

# curl flags that keep it a plain get to stdout. Every flag that writes a
# file, sends data, changes the method, or reads a config is left out.
_CURL = _flags(
    short="sSLfIikv",
    short_value="HAem",
    long=(
        "--silent",
        "--show-error",
        "--location",
        "--fail",
        "--head",
        "--include",
        "--insecure",
        "--verbose",
        "--compressed",
        "--http1.1",
        "--http2",
    ),
    long_value=(
        "--header",
        "--user-agent",
        "--referer",
        "--max-time",
        "--connect-timeout",
        "--retry",
    ),
)


def _is_test_run(head: str, args: list[str]) -> bool:
    """True when the command runs tests or checks with only flags that write
    nothing the agent names and run nothing the agent names."""
    if head == "pytest":
        return _operands(args, _PYTEST) is not None
    if head == "ruff":
        if args[:1] == ["check"]:
            return _operands(args[1:], _RUFF_CHECK) is not None
        if args[:1] == ["format"] and ("--check" in args or "--diff" in args):
            return _operands(args[1:], _RUFF_FORMAT) is not None
        return False
    if head == "mypy":
        return _operands(args, _MYPY) is not None
    if head == "go":
        if args[:1] == ["test"]:
            return _operands(args[1:], _GO_TEST) is not None
        if args[:1] == ["vet"]:
            return _operands(args[1:], _GO_VET) is not None
        return False
    if head == "cargo":
        if args[:1] != ["test"]:
            return False
        rest = args[1:]
        cut = rest.index("--") if "--" in rest else len(rest)
        if _operands(rest[:cut], _CARGO_TEST) is None:
            return False
        return _operands(rest[cut + 1 :], _LIBTEST) is not None
    if head in ("npm", "pnpm", "yarn"):
        return args == ["test"] or args == ["run", "test"]
    if head == "node":
        return args[:1] == ["--test"] and all(not a.startswith("-") for a in args[1:])
    if head == "make":
        return args in (["test"], ["check"])
    if head in ("python", "python3"):
        return args[:2] == ["-m", "pytest"] and _operands(args[2:], _PYTEST) is not None
    if head == "uv":
        if args[:1] != ["run"]:
            return False
        rest = args[1:]
        tool = next((i for i, a in enumerate(rest) if not a.startswith("-")), None)
        if tool is None or _operands(rest[:tool], _UV_RUN) is None:
            return False
        if rest[tool] not in ("pytest", "ruff", "mypy"):
            return False
        return _is_test_run(rest[tool], rest[tool + 1 :])
    return False


def _test_operands(head: str, args: list[str]) -> list[str] | None:
    """The operands of a test run, each a path it reads and often runs.
    None when the flags do not parse."""
    if head == "pytest":
        return _operands(args, _PYTEST)
    if head == "ruff":
        return _operands(args[1:], _RUFF_CHECK if args[:1] == ["check"] else _RUFF_FORMAT)
    if head == "mypy":
        return _operands(args, _MYPY)
    if head == "go":
        return _operands(args[1:], _GO_TEST if args[:1] == ["test"] else _GO_VET)
    if head == "cargo":
        rest = args[1:]
        cut = rest.index("--") if "--" in rest else len(rest)
        a = _operands(rest[:cut], _CARGO_TEST)
        b = _operands(rest[cut + 1 :], _LIBTEST)
        return None if a is None or b is None else a + b
    if head == "node":
        return args[1:]
    if head in ("python", "python3"):
        return _operands(args[2:], _PYTEST)
    if head == "uv":
        rest = args[1:]
        tool = next((i for i, a in enumerate(rest) if not a.startswith("-")), None)
        if tool is None:
            return None
        return _test_operands(rest[tool], rest[tool + 1 :])
    return []


def _find_paths(args: list[str]) -> list[str]:
    """The start paths of a find: the words before its first test."""
    out: list[str] = []
    for a in args:
        if a == "-P":
            continue
        if a.startswith("-") or a in ("(", ")", "!", ","):
            break
        out.append(a)
    return out or ["."]


def _find_class(args: list[str]) -> str:
    i = 0
    while i < len(args):
        a = args[i]
        i += 1
        if a in _FIND_VALUE:
            if i >= len(args):
                return "unknown"
            i += 1
        elif a in _FIND_PLAIN:
            continue
        elif a.startswith("-"):
            return "unknown"
    return "read"


# git verbs that only read, each with its closed flag set.
_GIT_READ: dict[str, Flags] = {
    "status": _flags(
        short="sb",
        short_optional="u",
        long=("--short", "--branch", "--ignored"),
        long_optional=("--porcelain", "--untracked-files"),
    ),
    "log": _flags(
        short="p",
        short_value="nSG",
        long=(
            "--oneline",
            "--stat",
            "--graph",
            "--decorate",
            "--all",
            "--patch",
            "--no-merges",
            "--reverse",
            "--name-only",
            "--name-status",
            "--shortstat",
            "--first-parent",
            "--no-color",
            "--abbrev-commit",
            "--follow",
            "--no-ext-diff",
        ),
        long_value=(
            "--max-count",
            "--since",
            "--until",
            "--author",
            "--grep",
            "--format",
            "--pretty",
            "--date",
            "--decorate",
        ),
        numeric=True,
    ),
    "diff": _flags(
        short="w",
        short_value="U",
        long=(
            "--stat",
            "--cached",
            "--staged",
            "--name-only",
            "--name-status",
            "--shortstat",
            "--no-color",
            "--numstat",
            "--check",
            "--no-ext-diff",
        ),
        long_value=("--unified", "--diff-filter", "--color", "--stat"),
    ),
    "show": _flags(
        short="s",
        long=("--stat", "--name-only", "--name-status", "--oneline", "--no-patch", "--no-ext-diff"),
        long_value=("--format", "--pretty"),
    ),
    "blame": _flags(short="wse", short_value="L"),
    "rev-parse": _flags(
        long=(
            "--abbrev-ref",
            "--show-toplevel",
            "--short",
            "--verify",
            "--git-dir",
            "--is-inside-work-tree",
            "--symbolic-full-name",
        ),
        long_value=("--short",),
    ),
    "ls-files": _flags(
        short="modscz",
        long=("--others", "--modified", "--deleted", "--cached", "--exclude-standard", "--stage"),
    ),
    "grep": _flags(
        short="nilwvEFcI",
        short_value="e",
        long=("--cached", "--line-number", "--ignore-case", "--count", "--files-with-matches"),
    ),
    "describe": _flags(
        long=("--tags", "--always", "--dirty", "--long"), long_value=("--abbrev", "--match")
    ),
    "shortlog": _flags(short="sne", long=("--summary", "--numbered", "--email")),
    "cat-file": _flags(short="tpse"),
    "remote": _flags(short="v", positional=False),
}

# git verbs that change the repo in a way the reflog can undo, each with
# its closed flag set.
_GIT_WRITE: dict[str, Flags] = {
    "add": _flags(
        short="uAvnN", long=("--all", "--update", "--verbose", "--dry-run", "--intent-to-add")
    ),
    "commit": _flags(
        short="aqvs",
        short_value="mF",
        long=(
            "--all",
            "--quiet",
            "--verbose",
            "--signoff",
            "--allow-empty",
            "--no-edit",
            "--amend",
        ),
        long_value=("--message", "--author", "--file"),
    ),
    "merge": _flags(
        short_value="m",
        long=("--no-ff", "--ff-only", "--ff", "--no-edit", "--squash", "--no-commit"),
        long_value=("--message",),
    ),
    "rebase": _flags(long=("--continue",), long_value=("--onto",)),
    "cherry-pick": _flags(short="n", long=("--no-commit", "--continue")),
    "revert": _flags(short="n", long=("--no-edit", "--no-commit", "--continue")),
    "reset": _flags(short="q", long=("--soft", "--mixed", "--quiet")),
    "switch": _flags(short_value="c", long=("--detach",), long_value=("--create",)),
}

# git verbs that move HEAD or the index. One of them can put a symlink in
# the tree, so no later path counts as inside the workspace.
_GIT_TREE_MOVERS = frozenset(
    {
        "add",
        "commit",
        "merge",
        "rebase",
        "cherry-pick",
        "revert",
        "reset",
        "switch",
        "checkout",
        "stash",
    }
)

_GIT_BRANCH_LIST = _flags(
    short="arv",
    long=("--all", "--remotes", "--verbose", "--list", "--show-current"),
    long_value=("--contains", "--merged", "--no-merged", "--sort"),
)
_GIT_TAG = _flags(short="la", short_value="m", long=("--list",), long_value=("--message",))
_GIT_FETCH = _flags(short="qv", long=("--all", "--tags", "--no-tags", "--quiet", "--verbose"))
_GIT_STASH_SAVE = _flags(
    short="uq", short_value="m", long=("--include-untracked", "--quiet"), long_value=("--message",)
)
_GIT_DELETE = frozenset({"clean", "restore", "rm", "gc", "prune", "filter-branch", "update-ref"})


def _optional(spec: Flags) -> Flags:
    """spec with every value flag made optional: its value counts only when
    attached. Git takes several values only when attached, and reading a
    detached word as a value could hide an output flag behind it."""
    return Flags(
        short=spec.short,
        short_value="",
        long=spec.long,
        long_value=frozenset(),
        long_optional=spec.long_value | spec.long_optional,
        short_optional=spec.short_value + spec.short_optional,
        single_dash=spec.single_dash,
        numeric=spec.numeric,
        positional=spec.positional,
    )


_GIT_READ = {k: _optional(v) for k, v in _GIT_READ.items()}
_GIT_WRITE = {k: _optional(v) for k, v in _GIT_WRITE.items()}
_GIT_BRANCH_LIST = _optional(_GIT_BRANCH_LIST)
_GIT_TAG = _optional(_GIT_TAG)
_GIT_FETCH = _optional(_GIT_FETCH)
_GIT_STASH_SAVE = _optional(_GIT_STASH_SAVE)


def _moves_tree(tokens: list[str]) -> bool:
    """True for a git verb that moves HEAD or the index, and for a test run,
    which runs code that can change the tree."""
    if len(tokens) > 1 and tokens[0] == "git" and tokens[1] in _GIT_TREE_MOVERS:
        return True
    return bool(tokens) and _is_test_run(tokens[0], tokens[1:])


def _is_output_word(word: str) -> bool:
    """True for --out..., --output..., or -o with a value joined to it."""
    if word.startswith("--out"):
        return True
    return word.startswith("-o") and not word.startswith("--") and len(word) > 2


def _is_http_url(word: str) -> bool:
    low = word.casefold()
    return low.startswith("http://") or low.startswith("https://")


def _sends_a_file(word: str) -> bool:
    """True for a curl header or data value read from a file with @."""
    if word.startswith("@"):
        return True
    if word.startswith("--"):
        name, eq, value = word.partition("=")
        return bool(eq) and value.startswith("@") and name.startswith(("--header", "--data"))
    return word.startswith("-") and ("H@" in word or "d@" in word)


def _git_class(args: list[str]) -> str:
    if not args or args[0].startswith("-"):
        return "unknown"
    sub, rest = args[0], args[1:]
    if sub == "push":
        for a in rest:
            if (
                a in ("-f", "--force", "--mirror", "--delete", "-d", "--prune")
                or a.startswith("--force")
                or a.startswith("+")
                or a.startswith(":")
            ):
                return "force_push"
        return "push_shared"
    if sub in _GIT_DELETE:
        return "delete"
    if sub in _GIT_READ:
        return "read" if _operands(rest, _GIT_READ[sub]) is not None else "unknown"
    if sub in _GIT_WRITE:
        return (
            "write_inside_workspace" if _operands(rest, _GIT_WRITE[sub]) is not None else "unknown"
        )
    if sub == "fetch":
        ops = _operands(rest, _GIT_FETCH)
        # A refspec can overwrite or delete a local ref.
        if ops is None or any(":" in o or o.startswith("+") for o in ops):
            return "unknown"
        return "network_get"
    if sub == "branch":
        ops = _operands(rest, _GIT_BRANCH_LIST)
        if ops is None or len(ops) > 2:
            return "unknown"
        return "write_inside_workspace" if ops else "read"
    if sub == "tag":
        ops = _operands(rest, _GIT_TAG)
        if ops is None:
            return "unknown"
        return (
            "write_inside_workspace"
            if ops and "-l" not in rest and "--list" not in rest
            else "read"
        )
    if sub == "stash":
        verb = rest[0] if rest and not rest[0].startswith("-") else "push"
        tail = rest[1:] if rest and not rest[0].startswith("-") else rest
        if verb in ("drop", "clear"):
            return "delete"
        if verb in ("list", "show"):
            ops = _operands(tail, _flags(short="p", long=("--stat",)))
            return "read" if ops is not None else "unknown"
        if verb in ("pop", "apply"):
            ops = _operands(tail, _flags(long=("--index",)))
            return "write_inside_workspace" if ops is not None else "unknown"
        if verb in ("push", "save"):
            ops = _operands(tail, _GIT_STASH_SAVE)
            return "write_inside_workspace" if ops is not None else "unknown"
        return "unknown"
    if sub == "checkout":
        # Only "checkout -b NAME [START]" is a plain new branch. Any other
        # form can overwrite work in the tree.
        if (
            len(rest) in (2, 3)
            and rest[0] in ("-b", "-B")
            and not any(r.startswith("-") for r in rest[1:])
        ):
            return "write_inside_workspace"
        return "delete"
    return "unknown"


def _covers_home(arg: str, cwd: str | None) -> bool:
    """True when arg names the home directory or a directory above it."""
    target = _resolve(arg, cwd)
    if target is None:
        return False
    home = os.path.realpath(os.path.expanduser("~"))
    try:
        return os.path.commonpath([target, home]) == target
    except ValueError:
        return False


# jq names the environment as env or $ENV.
_JQ_ENV = re.compile(r"(?<![\w$])env(?!\w)|\$ENV(?!\w)")


# Read commands whose operands are text, not paths they open.
_NO_PATH_HEADS = frozenset(
    {"echo", "printf", "true", "false", "which", "whoami", "pwd", "basename", "dirname"}
)
# Read commands that read the cwd when they name no path.
_CWD_READERS = frozenset({"ls", "du", "grep"})


def _command_class(
    tokens: list[str],
    root: Workspace | None,
    cwd: str | None = None,
    base: str | None = None,
    cwd_base: str | None = None,
) -> str:
    """The class of one simple command. root is the workspace for writes.
    base is the workspace root for reads, or None when no path counts as
    inside. cwd_base is the root the cwd must sit inside for a git read or
    a test run."""
    if not tokens:
        return "unknown"
    head, args = tokens[0], tokens[1:]
    if "=" in head or "/" in head:
        return "unknown"
    # The environment holds tokens, and these print it.
    if head in ("env", "printenv"):
        return "credential"
    if head == "jq" and any(_JQ_ENV.search(a) for a in args):
        return "credential"
    if head == "curl" and any(_sends_a_file(a) for a in args):
        return "credential"
    # An output flag anywhere, whatever came before it, may write a file.
    if any(_is_output_word(a) for a in args):
        return "unknown"
    # A word the shell expands at run time is not known now.
    if any("$" in a or "`" in a for a in args):
        return "unknown"
    for a in args:
        # git names a file in a commit as REV:path.
        paths = [a]
        if head == "git" and ":" in a and "://" not in a:
            rev_path = a.split(":", 1)[1]
            # A climb in REV:path leaves the tree the gate reads it from.
            if ".." in rev_path.replace("\\", "/").split("/"):
                return "unknown"
            paths.append(rev_path)
        if any(p and _is_credential_path(p, cwd) for p in paths):
            return "credential"
    if head in _CREDENTIAL_HEADS:
        return "credential"
    if head == "gh":
        return "credential" if args[:1] == ["auth"] else "push_shared"
    if head in _PROD_HEADS:
        return "prod"
    if head in _DELETE_HEADS:
        return "delete"
    if head in _PUBLISH and _PUBLISH[head] in args[:1]:
        return "push_shared"
    if head in _RECURSIVE_READERS and any(
        _covers_home(a, cwd) for a in args if not a.startswith("-")
    ):
        return "credential"
    if head == "git":
        cls = _git_class(args)
        if tier_for(cls) != UNDOABLE:
            return cls
        # git is undoable only in a repo that is the workspace, run from
        # inside it.
        if not _cwd_inside(cwd, cwd_base) or not _holds_git_dir(cwd_base):
            return "unknown"
        files = _git_file_values(args)
        if files is None:
            return "unknown"
        places = [_read_place(p, base, cwd) for p in files]
        if any(p != "read" for p in places):
            return _worst(["unknown", *places])
        return cls
    if head == "curl":
        ops = _operands(args, _CURL)
        # Only http and https are gets. gopher, telnet, dict and the rest
        # send bytes the agent picks to any host and port.
        if ops is None or not ops or not all(_is_http_url(o) for o in ops):
            return "unknown"
        return "network_get"
    if _is_test_run(head, args):
        if not _cwd_inside(cwd, cwd_base):
            return "unknown"
        ops = _test_operands(head, args)
        if ops is None:
            return "unknown"
        # A test run reads, and often runs, what it is given.
        places = [_read_place(p, base, cwd) for p in ops]
        return "test_run" if all(p == "read" for p in places) else _worst(["unknown", *places])
    if head == "find":
        cls = _find_class(args)
        if cls != "read":
            return cls
        return _worst([_read_place(p, base, cwd) for p in _find_paths(args)])
    if head in _MAKE_FLAGS:
        paths = _operands(args, _MAKE_FLAGS[head])
        if not paths:
            return "unknown"
        return _worst([_write_class(p, root, cwd) for p in paths])
    if head in _READ_FLAGS:
        ops = _operands(args, _READ_FLAGS[head])
        if ops is None:
            return "unknown"
        if head in _NO_PATH_HEADS or head in _CWD_HEADS:
            return "read"
        if not ops and head in _CWD_READERS:
            ops = ["."]
        return _worst(["read", *(_read_place(p, base, cwd) for p in ops)])
    return "unknown"


def _holds_git_dir(base: str | None) -> bool:
    """True when the workspace root holds the repo's .git, so the repo is
    the workspace."""
    return bool(base) and os.path.lexists(os.path.join(base, ".git"))


def _git_file_values(args: list[str]) -> list[str] | None:
    """The paths git commit reads a message from with -F or --file. None
    when a -F or --file names no path."""
    if args[:1] != ["commit"]:
        return []
    out: list[str] = []
    rest = args[1:]
    i = 0
    while i < len(rest):
        a = rest[i]
        i += 1
        if a == "--":
            break
        value = None
        if a == "--file":
            value = rest[i] if i < len(rest) else None
            i += 1
        elif a.startswith("--file="):
            value = a.split("=", 1)[1]
        elif a.startswith("-") and not a.startswith("--") and "F" in a.split("m", 1)[0]:
            # In a cluster such as -aF, F takes the rest of the word or the
            # next word. A letter m before it takes the rest as a message.
            tail = a[a.index("F") + 1 :]
            if tail:
                value = tail
            else:
                value = rest[i] if i < len(rest) else None
                i += 1
        else:
            continue
        if not value:
            return None
        out.append(value)
    return out


def _cwd_inside(cwd: str | None, base: str | None) -> bool:
    return bool(cwd) and bool(base) and _under(os.path.realpath(cwd), os.path.realpath(base))


def _unsafe_words(text: str) -> bool:
    """True when the shell would change a word before the command sees it:
    an unquoted glob or brace, a ~+, ~- or ~user at the start of a word, or
    a $ or backtick anywhere."""
    i = 0
    n = len(text)
    start = True
    quote = ""
    while i < n:
        c = text[i]
        if quote == "'":
            if c == "'":
                quote = ""
            i += 1
            continue
        if quote == '"':
            if c == "\\":
                i += 2
                continue
            if c == '"':
                quote = ""
            elif c in "$`":
                return True
            i += 1
            continue
        if c == "\\":
            i += 2
            start = False
            continue
        if c in "'\"":
            quote = c
            start = False
            i += 1
            continue
        if c.isspace():
            start = True
            i += 1
            continue
        if c in "$`*?[{}":
            return True
        if c == "~" and start:
            nxt = text[i + 1] if i + 1 < n else ""
            if nxt and nxt != "/" and not nxt.isspace():
                return True
        start = False
        i += 1
    return False


def _unsafe_redirect(path: str) -> bool:
    return any(c in path for c in "$`*?[{}") or (
        path.startswith("~") and len(path) > 1 and path[1] != "/"
    )


# The most working directories one shell line is followed through.
_MAX_CWDS = 32


def _shell_class(command: str, root: Workspace | None, cwd: str | None = None) -> str:
    if not command.strip() or not shell_parser_available():
        return "unknown"
    try:
        from opendaisugi.shell_decompose import decompose_command

        dec = decompose_command(command)
    except Exception:  # noqa: BLE001 - a parse failure is unknown
        return "unknown"
    if not dec.ok or not dec.commands:
        return "unknown"
    token_lists: list[list[str]] = []
    for cmd in dec.commands:
        try:
            token_lists.append(shlex.split(cmd))
        except ValueError:
            return "unknown"
    # A cd moves the shell. After one, no path counts as inside the
    # workspace for a write. Reads follow a cd to a literal place.
    base = root.base if root else None
    moved_cwd = any(t and t[0] in _CWD_HEADS for t in token_lists)
    if moved_cwd:
        root = None
    classes: list[str] = []
    here = root
    read_base = base
    cwds = [cwd]
    lost = False
    for tokens, text in zip(token_lists, dec.commands, strict=True):
        if _unsafe_words(text):
            classes.append("unknown")
        # After a cd or popd the gate cannot follow, a relative word could
        # name anything.
        if lost and any(_is_relative_word(a) for a in tokens[1:]):
            classes.append("unknown")
        classes.append(_command_class(tokens, here, cwd, read_base, base))
        # A git verb that moves HEAD or the index, or a test run, can put a
        # symlink in the tree, so no later path counts as inside.
        if _moves_tree(tokens):
            here = None
            read_base = None
        cwd = _next_cwd(tokens, cwd)
        if cwd not in cwds and len(cwds) >= _MAX_CWDS:
            # Past this many directories the line counts as moved to a
            # place the gate cannot follow, so the check stays well inside
            # a hook timeout and relative words stay unknown.
            cwd = None
            lost = True
        if cwd not in cwds:
            cwds.append(cwd)
        if tokens and tokens[0] in _CWD_HEADS and cwd is None:
            lost = True
    # Redirects are not ordered against the commands, so any tree change
    # anywhere in the line takes the workspace from every redirect, and a
    # redirect is checked from every cwd the line passes through.
    redirect_root = root if here is root else None
    redirect_cwd = cwds[0] if not moved_cwd else None
    for p in (*dec.writes, *dec.reads):
        if _unsafe_redirect(p):
            classes.append("unknown")
        if any(_is_credential_path(p, c) for c in cwds):
            classes.append("credential")
        if lost and _is_relative_word(p):
            classes.append("unknown")
    classes.extend(_write_class(p, redirect_root, redirect_cwd) for p in dec.writes)
    for p in dec.reads:
        classes.extend(_read_place(p, read_base, c) for c in cwds)
    return _worst(classes)


def _is_relative_word(word: str) -> bool:
    """True for an operand that is a relative path, not a flag."""
    if not word or word.startswith("-"):
        return False
    return not os.path.isabs(os.path.expanduser(word))


def _next_cwd(tokens: list[str], cwd: str | None) -> str | None:
    """The cwd after one command. bash's cd works on the logical path, so a
    cd is followed only to a literal path with no .. whose resolved form is
    its logical form, with no symlink on the way. Any other cd, pushd or
    popd leaves the cwd unknown."""
    if not tokens or tokens[0] not in _CWD_HEADS:
        return cwd
    args = tokens[1:]
    if tokens[0] == "popd":
        return None
    if tokens[0] == "cd" and not args:
        target = os.path.expanduser("~")
    elif len(args) == 1 and not args[0].startswith("-"):
        target = args[0]
    else:
        return None
    if ".." in target.replace("\\", "/").split("/"):
        return None
    expanded = os.path.expanduser(target)
    if not os.path.isabs(expanded):
        if not cwd:
            return None
        expanded = os.path.join(cwd, expanded)
    logical = os.path.normpath(expanded)
    if os.path.realpath(logical) != logical:
        return None
    return logical


def workspace_root(cwd: object, fixed: object) -> Workspace | None:
    """The workspace of one call.

    ``cwd`` is the call's working directory, which the agent can move with
    cd. ``fixed`` is a root the agent cannot move, such as the project
    directory the harness names. Relative paths resolve against ``cwd``. A
    path counts as inside only when it lands inside ``fixed``, and only
    while ``cwd`` itself is inside ``fixed``. Without both, there is no
    workspace.
    """
    if not isinstance(cwd, str) or not isinstance(fixed, str) or not cwd or not fixed:
        return None
    if not os.path.isabs(cwd) or not os.path.isabs(fixed):
        return None
    a, b = os.path.realpath(cwd), os.path.realpath(fixed)
    try:
        if os.path.commonpath([a, b]) != b:
            return None
    except ValueError:
        return None
    return Workspace(a, b)


def effect_class(
    record: dict[str, Any] | None,
    cwd: Workspace | str | None,
    call_cwd: str | None = None,
) -> str:
    """The effect class of one normalized capture record.

    ``cwd`` is the workspace, or a directory that is both the working
    directory and the root. Without one, no write counts as inside the
    workspace. ``call_cwd`` is the call's own working directory, which
    the credential checks resolve paths against when there is no
    workspace.
    """
    try:
        return _effect_class(record, cwd, call_cwd)
    except Exception:  # noqa: BLE001 - a path the gate cannot resolve is unknown
        return "unknown"


def _effect_class(
    record: dict[str, Any] | None,
    cwd: Workspace | str | None,
    call_cwd: str | None,
) -> str:
    if not isinstance(record, dict):
        return "unknown"
    step = record.get("step_type")
    root = _as_workspace(cwd)
    here = root.cwd if root else (call_cwd if isinstance(call_cwd, str) else None)
    if step == "file_read":
        path = str(record.get("path") or "")
        return _read_place(path, root.base if root else None, here) if path else "unknown"
    if step == "file_write":
        path = str(record.get("path") or "")
        return _write_class(path, root, here) if path else "unknown"
    if step == "network":
        return "network_get" if record.get("tool_name") in ("WebFetch", "WebSearch") else "unknown"
    if step == "shell":
        return _shell_class(str(record.get("command") or ""), root, here)
    return "unknown"

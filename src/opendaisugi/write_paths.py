"""The write paths of one plan step: the field ``forall_writes`` reads.

A predicate reaches a step's write paths only through the ``forall_writes``
quantifier (``predicate.py``). They are not part of the step's own record,
so a plan, a journal entry and a conformance case look the same as before.

The write paths of a step:

- a ``file_write`` step: its ``path``;
- a ``shell`` step: every literal write redirect that the shell
  decomposition (``shell_decompose.py``) finds, the same targets the
  permission stage checks against ``file_write``, and every file a known
  writing command writes through its operands (``WRITERS``: ``cp``, ``mv``,
  ``ln``, ``install``, ``tee``, ``sed -i``, ``truncate``, ``dd of=``,
  ``rsync``, ``touch``, ``mkdir``, ``rm``, ``rmdir``, ``unlink``, ``shred``),
  read with that command's own argument rules. The walk goes into the
  payloads of command-taking wrappers (``sh -c``, ``xargs``, ``find -exec``,
  ``env``, ``timeout``), as the permission stage does. The null device and
  the standard streams are not write paths;
- any other step type: none.

The operand rules. For ``cp``, ``ln``, ``install`` and ``rsync`` the
destination is written: the last operand, or the ``-t`` directory. A plan
cannot ask the filesystem whether the destination is a directory, so each
source also gives ``DEST/basename(SOURCE)``, unless ``-T`` says the
destination is not a directory. ``mv`` also changes its sources. For the
other commands every operand is written (``install -d`` too). ``sed``
writes only with ``-i``, and then every file after the script, and the
backup with its suffix. ``dd`` writes its ``of=`` operand. ``rsync``
writes no remote destination, and with ``--remove-source-files`` its local
sources too. A recursive command shows only its top path, so a target such
as ``src/*.py`` does not see ``rm -r src``, while ``src/**`` does.

Each path is normalized with ``posixpath.normpath``, so ``./src/a.py`` and
``lib/../src/a.py`` read as ``src/a.py``.

Unknown write paths: ``step_write_paths`` returns None, and
``forall_writes`` is then false. A shell step has unknown write paths when
its redirects cannot be read (the grammar refuses it, the shell extra is
missing, or wrappers nest too deep), when a writing command carries a flag
outside its closed set, a word the shell would change (``$``, a backtick, a
glob, a brace, ``~user``), or a line ``shlex`` cannot split, when a writing
command runs under ``xargs`` or ``find -exec`` (they add operands the line
does not show), and when a line that runs ``cd``, ``pushd`` or ``popd``
also writes a relative path.

With a ``base`` (an absolute, normalized directory), each relative write
path is resolved against it; a write path that starts with ``~`` is then
unknown.

Not write paths: the work of an interpreter such as ``python``, ``awk``,
``sed`` without ``-i`` or ``sudo``, a command not in ``WRITERS``, and the
writes of MCP tools, skills and delegated agents.
"""

from __future__ import annotations

import posixpath
import re
import shlex
from dataclasses import dataclass
from typing import Any

# The commands whose operands name files they write, and the closed set of
# flags each may carry. ``short`` and ``short_value`` are single letters
# that take no value and that take one; ``short_optional`` takes a value
# only in the same word. ``long``, ``long_value`` and ``long_optional`` are
# the same split for long flags (an optional value only after "="). Any
# other flag makes the write paths unknown. ``rule`` says which operands
# are written (see the module docstring).
WRITERS: dict[str, dict[str, Any]] = {
    "cp": {
        "rule": "dest",
        "short": "adfiHlLnPpRrsTuvx",
        "short_value": "t",
        "long": [
            "--archive",
            "--dereference",
            "--force",
            "--interactive",
            "--link",
            "--no-clobber",
            "--no-dereference",
            "--no-target-directory",
            "--one-file-system",
            "--recursive",
            "--remove-destination",
            "--strip-trailing-slashes",
            "--symbolic-link",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
        "long_optional": ["--preserve", "--reflink", "--update"],
    },
    "mv": {
        "rule": "move",
        "short": "finTuv",
        "short_value": "t",
        "long": [
            "--force",
            "--interactive",
            "--no-clobber",
            "--no-target-directory",
            "--strip-trailing-slashes",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
        "long_optional": ["--update"],
    },
    "ln": {
        "rule": "dest",
        "short": "finLPrsTv",
        "short_value": "t",
        "long": [
            "--force",
            "--interactive",
            "--logical",
            "--no-dereference",
            "--no-target-directory",
            "--physical",
            "--relative",
            "--symbolic",
            "--verbose",
        ],
        "long_value": ["--target-directory"],
    },
    "install": {
        "rule": "install",
        "short": "cCdDpsTv",
        "short_value": "gmot",
        "long": [
            "--compare",
            "--directory",
            "--no-target-directory",
            "--preserve-timestamps",
            "--strip",
            "--verbose",
        ],
        "long_value": ["--group", "--mode", "--owner", "--target-directory"],
    },
    "tee": {
        "rule": "all",
        "short": "aip",
        "long": ["--append", "--ignore-interrupts"],
        "long_optional": ["--output-error"],
    },
    "sed": {
        "rule": "sed",
        "short": "Enrsuz",
        "short_value": "efl",
        "short_optional": "i",
        "long": [
            "--debug",
            "--follow-symlinks",
            "--null-data",
            "--posix",
            "--quiet",
            "--regexp-extended",
            "--sandbox",
            "--separate",
            "--silent",
            "--unbuffered",
        ],
        "long_value": ["--expression", "--file", "--line-length"],
        "long_optional": ["--in-place"],
    },
    "truncate": {
        "rule": "all",
        "short": "co",
        "short_value": "rs",
        "long": ["--io-blocks", "--no-create"],
        "long_value": ["--reference", "--size"],
    },
    "dd": {"rule": "dd"},
    # tar has its own flag grammar (TAR below); the rule name is all this table needs.
    "tar": {"rule": "tar"},
    "rsync": {
        "rule": "rsync",
        "short": "0346aAbcCdDEgHhiIkKlLmnoOpPqrRsStuUvWxXyz",
        "short_value": "Bef",
        "long": [
            "--acls",
            "--append",
            "--archive",
            "--backup",
            "--checksum",
            "--compress",
            "--copy-links",
            "--cvs-exclude",
            "--delete",
            "--delete-after",
            "--delete-before",
            "--delete-delay",
            "--delete-during",
            "--delete-excluded",
            "--devices",
            "--dirs",
            "--dry-run",
            "--executability",
            "--existing",
            "--force",
            "--fuzzy",
            "--group",
            "--hard-links",
            "--human-readable",
            "--ignore-existing",
            "--ignore-times",
            "--inplace",
            "--itemize-changes",
            "--links",
            "--list-only",
            "--mkpath",
            "--no-group",
            "--no-owner",
            "--no-perms",
            "--no-relative",
            "--no-times",
            "--no-whole-file",
            "--numeric-ids",
            "--omit-dir-times",
            "--one-file-system",
            "--owner",
            "--partial",
            "--perms",
            "--progress",
            "--protect-args",
            "--prune-empty-dirs",
            "--quiet",
            "--recursive",
            "--relative",
            "--remove-source-files",
            "--safe-links",
            "--size-only",
            "--sparse",
            "--specials",
            "--stats",
            "--times",
            "--update",
            "--verbose",
            "--whole-file",
            "--xattrs",
        ],
        "long_value": [
            "--block-size",
            "--bwlimit",
            "--chmod",
            "--chown",
            "--compress-level",
            "--contimeout",
            "--debug",
            "--exclude",
            "--exclude-from",
            "--files-from",
            "--filter",
            "--include",
            "--include-from",
            "--info",
            "--max-delete",
            "--max-size",
            "--min-size",
            "--modify-window",
            "--out-format",
            "--port",
            "--rsh",
            "--suffix",
            "--timeout",
        ],
    },
    "touch": {
        "rule": "all",
        "short": "acm",
        "short_value": "dtr",
        "long_value": ["--date", "--reference"],
    },
    "mkdir": {
        "rule": "all",
        "short": "pv",
        "short_value": "m",
        "long": ["--parents", "--verbose"],
        "long_value": ["--mode"],
    },
    "rm": {
        "rule": "all",
        "short": "dfiIrRv",
        "long": [
            "--dir",
            "--force",
            "--no-preserve-root",
            "--one-file-system",
            "--recursive",
            "--verbose",
        ],
        "long_optional": ["--interactive", "--preserve-root"],
    },
    "rmdir": {
        "rule": "all",
        "short": "v",
        "long": ["--ignore-fail-on-non-empty", "--verbose"],
    },
    "unlink": {"rule": "all"},
    "shred": {
        "rule": "all",
        "short": "fuvxz",
        "short_value": "ns",
        "long": ["--exact", "--force", "--verbose", "--zero"],
        "long_value": ["--iterations", "--size"],
        "long_optional": ["--remove"],
    },
}

# dd's operands, each KEY=VALUE. Any other word makes the paths unknown.
DD_KEYS = frozenset(
    {
        "bs",
        "cbs",
        "conv",
        "count",
        "ibs",
        "if",
        "iflag",
        "iseek",
        "obs",
        "of",
        "oflag",
        "oseek",
        "seek",
        "skip",
        "status",
    }
)


def _reader(
    rule: str,
    short: str = "",
    short_value: str = "",
    long: str = "",
    long_value: str = "",
    long_optional: str = "",
    short_optional: str = "",
) -> dict[str, Any]:
    """One READERS entry. A letter that takes a value is never also a plain flag.
    ``short_optional`` letters take a value only when it is attached (``-w16``)."""
    spec: dict[str, Any] = {
        "rule": rule,
        "short": "".join(c for c in short if c not in short_value + short_optional),
        "short_value": short_value,
        "short_optional": short_optional,
        "long": long.split(),
        "long_value": long_value.split(),
        "long_optional": long_optional.split(),
    }
    return {k: v for k, v in spec.items() if v}


# No -c or --check: they read the files named inside the operand, which the
# placer cannot see.
_SUM_LONG = "--binary --text --tag --zero"
_GREP_SHORT = "abcEFGhHiIlLnoPqrRsTUvwxyzZ0123456789"
_GREP_SHORT_VALUE = "ABCdDefm"
_GREP_LONG = (
    "--count --extended-regexp --fixed-strings --ignore-case --files-with-matches "
    "--files-without-match --line-number --only-matching --perl-regexp --quiet --silent "
    "--recursive --dereference-recursive --no-messages --invert-match --word-regexp "
    "--line-regexp --null --text --binary --with-filename --no-filename --null-data "
    "--byte-offset --initial-tab --line-buffered --no-ignore-case --no-group-separator"
)
_GREP_LONG_VALUE = (
    "--regexp --file --max-count --after-context --before-context --context --include "
    "--exclude --exclude-dir --binary-files --directories --devices --label --group-separator"
)

# The commands whose operands name files they read, and the closed flag
# sets each may carry (same shape as WRITERS). The commands in both tables
# (cp, mv, install, rsync, tar, dd, sed) take their flags from WRITERS; the
# entry here only names the rule. ``find`` has its own grammar (FIND_ARGS).
READERS: dict[str, dict[str, Any]] = {
    "cat": _reader(
        "files",
        "AbeEnstTuv",
        long="--show-all --number-nonblank --show-ends --number --squeeze-blank "
        "--show-tabs --show-nonprinting",
    ),
    "tac": _reader("files"),
    "rev": _reader("files"),
    "cksum": _reader("files"),
    "nl": _reader("files", "p", "bdfhilnsvw"),
    "strings": _reader("files", "adfwoh", "ntTesU"),
    "md5sum": _reader("files", "btwz", long=_SUM_LONG),
    "sha1sum": _reader("files", "btwz", long=_SUM_LONG),
    "sha256sum": _reader("files", "btwz", long=_SUM_LONG),
    "sha512sum": _reader("files", "btwz", long=_SUM_LONG),
    "file": _reader("files", "bhiLNnprsz"),
    "cmp": _reader("files", "bls", "in"),
    "paste": _reader("files", "sz", "d"),
    "head": _reader(
        "files",
        "qvz0123456789",
        "nc",
        "--quiet --silent --verbose --zero-terminated",
        "--lines --bytes",
    ),
    "tail": _reader(
        "files",
        "qvzfF0123456789",
        "ncs",
        "--quiet --silent --verbose --zero-terminated --retry",
        "--lines --bytes --sleep-interval --pid --max-unchanged-stats",
        "--follow",
    ),
    "less": _reader("files", "NSRFXieqsEM"),
    "more": _reader("files", "dlfpcsu"),
    "wc": _reader(
        "files",
        "cmlwL",
        long="--bytes --chars --lines --words --max-line-length",
        long_value="--total",
    ),
    "cut": _reader(
        "files",
        "nsz",
        "bcfd",
        "--complement --only-delimited --zero-terminated",
        "--bytes --characters --fields --delimiter --output-delimiter",
    ),
    "sort": _reader(
        "files",
        "bcCdfgiMhmnRrsuVz",
        "ktS",
        "--stable --unique --reverse --numeric-sort --ignore-case --zero-terminated "
        "--version-sort --human-numeric-sort --general-numeric-sort --month-sort "
        "--random-sort --dictionary-order --ignore-leading-blanks --ignore-nonprinting "
        "--merge",
        "--key --field-separator --sort --buffer-size",
    ),
    # uniq and xxd write their second operand: the table entry (WRITERS, below
    # READERS) carries the flags and the rule "inout".
    "uniq": {"rule": "inout"},
    "od": _reader("files", "abcdfiloxv", "AjNtS", short_optional="w"),
    "xxd": {"rule": "inout"},
    "hexdump": _reader("files", "bcCdovx", "ens"),
    "base64": _reader("files", "di", "w", "--decode --ignore-garbage", "--wrap"),
    "stat": _reader(
        "files",
        "fLt",
        "c",
        "--dereference --file-system --terse",
        "--format --printf",
    ),
    "diff": _reader(
        "diff",
        "abBcdeEilnNpqrstTuwyZ",
        "UCWxFID",
        "--brief --recursive --ignore-all-space --ignore-case --new-file --text "
        "--ignore-space-change --ignore-blank-lines --ignore-trailing-space "
        "--ignore-tab-expansion --expand-tabs --initial-tab --minimal --side-by-side "
        "--suppress-common-lines --left-column --report-identical-files --show-c-function "
        "--strip-trailing-cr --no-dereference --normal --ed --rcs --paginate "
        "--speed-large-files --suppress-blank-empty --unidirectional-new-file "
        "--ignore-file-name-case --no-ignore-file-name-case",
        "--exclude --width --label --tabsize --horizon-lines --show-function-line "
        "--ifdef --ignore-matching-lines --palette",
        "--unified --context --color",
    ),
    "grep": _reader(
        "grep",
        _GREP_SHORT,
        _GREP_SHORT_VALUE,
        _GREP_LONG,
        _GREP_LONG_VALUE,
        "--color --colour",
    ),
    "egrep": _reader(
        "grep",
        _GREP_SHORT,
        _GREP_SHORT_VALUE,
        _GREP_LONG,
        _GREP_LONG_VALUE,
        "--color --colour",
    ),
    "fgrep": _reader(
        "grep",
        _GREP_SHORT,
        _GREP_SHORT_VALUE,
        _GREP_LONG,
        _GREP_LONG_VALUE,
        "--color --colour",
    ),
    "rg": _reader(
        "rg",
        "aAcFhHiIlLnNopPqsSuUvwxz",
        "ABCdegjmMrtT",
        "--count --fixed-strings --files-with-matches --line-number --no-filename "
        "--with-filename --only-matching --quiet --smart-case --case-sensitive "
        "--ignore-case --word-regexp --line-regexp --invert-match --hidden --no-ignore "
        "--files --follow --heading --no-heading --column --trim --stats --json "
        "--vimgrep --no-messages --no-line-number",
        "--regexp --file --type --type-not --glob --iglob --max-depth --max-count "
        "--context --after-context --before-context --replace --threads --encoding "
        "--color --sort --sortr --max-filesize",
    ),
    "ls": _reader(
        "ls",
        "aAbBcCdDfFgGhHiklLmnNopqQrRsStTuUvwxXZ1",
        "IwT",
        "--all --almost-all --recursive --directory --long --human-readable --reverse "
        "--dereference --classify --group-directories-first --full-time --si --file-type "
        "--author --inode --size --numeric-uid-gid --escape --literal --quote-name "
        "--hide-control-chars --show-control-chars --no-group --zero --dired --context "
        "--kibibytes --ignore-backups",
        "--ignore --width --tabsize --sort --time --time-style --format --hide "
        "--block-size --quoting-style --indicator-style",
        "--color --hyperlink",
    ),
    "find": {"rule": "find"},
    "cp": {"rule": "dest"},
    "mv": {"rule": "move"},
    "install": {"rule": "install"},
    "rsync": {"rule": "rsync"},
    "tar": {"rule": "tar"},
    "dd": {"rule": "dd"},
    "sed": {"rule": "sed"},
}

# The two commands that read INPUT and write OUTPUT (the second operand).
WRITERS["uniq"] = _reader(
    "inout",
    "cdDiuz",
    "fsw",
    "--count --repeated --unique --ignore-case --zero-terminated",
    "--skip-fields --skip-chars --check-chars",
)
WRITERS["xxd"] = _reader("inout", "abCEeipruv", "cglnos")

# The tests and actions of find with their argument counts. "*" is an
# -exec style clause, consumed through ";" or "+".
FIND_ARGS: dict[str, int | str] = {
    **{
        t: 1
        for t in (
            "-name -iname -path -ipath -wholename -iwholename -regex -iregex -type -xtype "
            "-newer -anewer -cnewer -newermt -mtime -mmin -atime -amin -ctime -cmin -size "
            "-perm -user -group -uid -gid -links -inum -maxdepth -mindepth -printf "
            "-fprint -fprint0 -fls"
        ).split()
    },
    **{
        t: 0
        for t in (
            "-empty -nouser -nogroup -readable -writable -executable -true -false -prune "
            "-print -print0 -ls -quit -depth -xdev -mount -noleaf -not ! -a -and -o -or "
            "( ) -L -H -P -follow -delete"
        ).split()
    },
    "-fprintf": 2,
    "-exec": "*",
    "-execdir": "*",
    "-ok": "*",
    "-okdir": "*",
}

# tar (SG-5). One mode, one -f, at most one -C, a closed flag set.
TAR: dict[str, Any] = {
    "modes": "cxt",
    "flags": "vzjJa",
    "value": "fC",
    "long": [
        "--create",
        "--extract",
        "--list",
        "--gzip",
        "--bzip2",
        "--xz",
        "--auto-compress",
        "--verbose",
    ],
    "long_value": ["--file", "--directory", "--strip-components"],
}

UNKNOWN_REASONS: tuple[str, ...] = (
    "the shell grammar refused the line",
    "the shell extra is missing",
    "wrappers nest past depth 4",
    "a word the shell cannot split",
    "a word the shell would change ($, a backtick, a glob, a brace, ~user)",
    "a flag outside the closed set of {head}",
    "{head} runs under xargs, which adds operands the line does not show",
    "a writer runs under find -exec, which adds operands the line does not show",
    "a relative path with .. under find -execdir",
    "the line moves its cwd and names a relative path",
    "a path under ~",
    "a find test or action outside the closed set",
    "tar without one of c, x, t and one -f",
    "a head that runs a command the placer cannot read (exec, eval, source, sudo, watch, "
    "ssh, tmux, git -c, and the others in RUNNERS)",
    "a find -exec operand that wraps the found file in a longer word",
    "a tar archive on another host (HOST:PATH)",
    "a wrapper option the placer cannot match (an unknown or ambiguous long option)",
    "a sed script with a command that reads, writes or runs a file the placer cannot place",
)
(
    _R_GRAMMAR,
    _R_EXTRA,
    _R_DEPTH,
    _R_SPLIT,
    _R_UNSAFE,
    _R_FLAG,
    _R_XARGS,
    _R_FIND_EXEC,
    _R_EXECDIR,
    _R_MOVES,
    _R_TILDE,
    _R_FIND_ARG,
    _R_TAR,
    _R_RUNS,
    _R_EMBED,
    _R_REMOTE_TAR,
    _R_OPTION,
    _R_SED,
) = UNKNOWN_REASONS

# The heads that run the command they are given (or a file as commands),
# which the placer cannot read: with an argument the whole line is unknown.
# Allowlisting one of these heads allows any command it runs.
RUNNERS = frozenset(
    {
        "exec",
        "eval",
        "builtin",
        "source",
        ".",
        "chroot",
        "flock",
        "busybox",
        "script",
        "parallel",
        "coproc",
        "trap",
        "sudo",
        "doas",
        "watch",
        "systemd-run",
        "screen",
        "unbuffer",
        "runuser",
        "taskset",
        "chrt",
        "prlimit",
        "unshare",
        "nsenter",
        "entr",
    }
)

# Heads that read their commands from standard input (a here-document or a
# pipe): unknown even with no argument.
STDIN_RUNNERS = frozenset({"batch", "expect"})

# The tmux commands that run a command or a file of commands, by full name
# and alias. tmux takes any unique prefix of a command name.
_TMUX_RUNS = (
    "new-session new new-window neww split-window splitw send-keys send run-shell run "
    "respawn-pane respawnp respawn-window respawnw pipe-pane pipep if-shell if "
    "display-popup popup display-menu menu source-file source set-hook bind-key bind "
    "command-prompt confirm-before confirm choose-tree choose-client choose-buffer"
).split()

_VIM_HEADS = frozenset({"vim", "nvim", "vi", "view", "vimdiff", "ex", "rvim", "rview", "gvim"})
_SSH_VALUE = "BbcDEeFIiJLlmOopQRSWw"
_SSH_RUNS_RE = re.compile(r"command|match|include|exec", re.IGNORECASE)
_GIT_VALUE_LONG = ("--git-dir", "--work-tree", "--namespace", "--super-prefix", "--attr-source")


def _git_runs(args: list[str]) -> bool:
    """``git -c``, ``--config-env`` and ``--exec-path`` before the subcommand:
    a configured alias, pager or ssh command is a command git runs."""
    i = 0
    while i < len(args):
        a = args[i]
        if not a.startswith("-"):
            return False
        if a == "-C" or a in _GIT_VALUE_LONG:
            i += 2
            continue
        if a.startswith(("-c", "--config-env", "--exec-path")):
            return True
        i += 1
    return False


def _vim_runs(args: list[str]) -> bool:
    """Any option or ``+cmd``: -c, +cmd, --cmd, -S, -s, -u and -es run commands."""
    return any(a.startswith("+") or (a.startswith("-") and a != "-") for a in args)


def _ssh_runs(args: list[str]) -> bool:
    """A word after the host is the remote command; -o ProxyCommand and kin,
    -F and -E run or write something the placer cannot see."""
    i = 0
    host = False
    ended = False
    while i < len(args):
        a = args[i]
        i += 1
        if not ended and a == "--":
            ended = True
            continue
        if not ended and a.startswith("-") and a != "-":
            body = a[1:]
            for j, c in enumerate(body):
                if c not in _SSH_VALUE:
                    continue
                rest = body[j + 1 :]
                value = rest
                if not rest and i < len(args):
                    value = args[i]
                if not rest:
                    i += 1
                if c in "FE" or (c == "o" and _SSH_RUNS_RE.search(value)):
                    return True
                break
            continue
        if host:
            return True
        host = True
    return False


def _su_runs(args: list[str]) -> bool:
    for a in args:
        if a.startswith(("--command", "--session-command")):
            return True
        if a.startswith("-") and not a.startswith("--") and "c" in a:
            return True
    return False


def _tmux_runs(args: list[str]) -> bool:
    """A tmux command that runs a command, found at the start of the command
    or after a ``;``, by unique prefix; or -c / -f before the command."""
    i = 0
    n = len(args)
    start = True
    first = True
    while i < n:
        a = args[i]
        i += 1
        if first and a.startswith("-") and a != "-":
            for j, c in enumerate(a[1:], 1):
                if c in "cf":
                    return True
                if c in "LST":
                    if j == len(a) - 1:
                        i += 1
                    break
            continue
        first = False
        if a == ";":
            start = True
            continue
        if start:
            start = False
            if any(name.startswith(a) for name in _TMUX_RUNS):
                return True
    return False


def runs_a_command(head: str, raw_head: str, args: list[str]) -> bool:
    """True when this head with these arguments runs a command the placer
    cannot read."""
    if head in STDIN_RUNNERS:
        return True
    if head in RUNNERS or raw_head in RUNNERS:
        return bool(args)
    if head == "git":
        return _git_runs(args)
    if head in _VIM_HEADS:
        return _vim_runs(args)
    if head == "ssh":
        return _ssh_runs(args)
    if head == "su":
        return _su_runs(args)
    if head == "tmux":
        return _tmux_runs(args)
    if head in ("crontab", "at"):
        return args != ["-l"]
    return False


# The heads that can change the cwd of what follows. ``env`` with -C, -S or
# --chdir does too (see ``_is_mover``).
CWD_MOVERS = frozenset({"cd", "pushd", "popd", "builtin", "command", "exec", "eval", "source", "."})

_PARSER_MISSING = "shell decomposition parser not installed (opendaisugi[shell])"
_NULL_WRITES = frozenset({"/dev/null", "/dev/stdout", "/dev/stderr"})
_NULL_READS = frozenset({"/dev/null", "/dev/stdin"})


@dataclass(frozen=True, order=True)
class Access:
    path: str  # posixpath.normpath'd; absolute when a base resolved it
    tree: bool  # the path and everything under it
    via: str  # "operand" or "redirect"


@dataclass(frozen=True)
class Accesses:
    reads: tuple[Access, ...] = ()
    writes: tuple[Access, ...] = ()
    reads_unknown: str | None = None
    writes_unknown: str | None = None


class _Ctx:
    def __init__(self) -> None:
        self.reads: list[tuple[str, bool, str, tuple[str, ...]]] = []
        self.writes: list[tuple[str, bool, str, tuple[str, ...]]] = []
        self.ru: str | None = None
        self.wu: str | None = None
        self.movers = 0

    def unknown_reads(self, reason: str) -> None:
        if self.ru is None:
            self.ru = reason

    def unknown_writes(self, reason: str) -> None:
        if self.wu is None:
            self.wu = reason

    def unknown(self, reason: str, reads: bool = True, writes: bool = True) -> None:
        if reads:
            self.unknown_reads(reason)
        if writes:
            self.unknown_writes(reason)


def _norm(path: str) -> str:
    # normpath keeps a leading "//"; on Linux "//etc" is "/etc".
    out = posixpath.normpath(path)
    return "/" + out.lstrip("/") if out.startswith("//") else out


def _join(base: str, path: str) -> str:
    if path.startswith("/") or not base:
        return _norm(path)
    return _norm(base + "/" + path)


def _finalize(ctx: _Ctx, has_base: bool) -> Accesses:
    rel = any(not p.startswith("/") for p, _t, _v, _b in ctx.reads + ctx.writes)
    if ctx.movers and rel:
        ctx.unknown(_R_MOVES)

    def place(
        raw: list[tuple[str, bool, str, tuple[str, ...]]], sinks: frozenset[str], reads: bool
    ) -> tuple[Access, ...]:
        out: list[Access] = []
        for path, tree, via, bases in raw:
            for base in bases:
                if path.startswith("~") and has_base:
                    ctx.unknown(_R_TILDE, reads=reads, writes=not reads)
                    continue
                full = _join(base, path)
                if full in sinks:
                    continue
                a = Access(full, tree, via)
                if a not in out:
                    out.append(a)
        return tuple(out)

    reads = place(ctx.reads, _NULL_READS, True)
    writes = place(ctx.writes, _NULL_WRITES, False)
    return Accesses(reads, writes, ctx.ru, ctx.wu)


def shell_accesses(command: str, base: str | None) -> Accesses:
    """The reads and writes of one shell line, placed from ``base``
    (an absolute normalized directory), or why they are unknown."""
    ctx = _Ctx()
    _level(command, 0, (base or "",), None, ctx)
    return _finalize(ctx, bool(base))


def operand_accesses(command: str, under: str | None) -> Accesses:
    """The accesses of one simple command. ``under`` is None, "xargs",
    "find-exec" or "find-execdir"."""
    ctx = _Ctx()
    _simple(command, 0, ("",), under, ctx, False)
    return _finalize(ctx, False)


def step_accesses(step: dict[str, Any], base: str | None = None) -> Accesses:
    kind = step.get("type")
    if kind in ("file_write", "file_read"):
        path = step.get("path")
        if not isinstance(path, str):
            reason = _R_SPLIT
            return Accesses(reads_unknown=reason, writes_unknown=reason)
        ctx = _Ctx()
        entry = [(path, False, "operand", (base or "",))]
        if kind == "file_write":
            ctx.writes = entry
        else:
            ctx.reads = entry
        return _finalize(ctx, bool(base))
    if kind == "shell":
        command = step.get("command")
        if not isinstance(command, str):
            return Accesses(reads_unknown=_R_SPLIT, writes_unknown=_R_SPLIT)
        return shell_accesses(command, base)
    return Accesses()


def step_write_paths(step: dict[str, Any], base: str | None = None) -> list[str] | None:
    """The normalized write paths of one step record, or None when unknown.

    With ``base``, an absolute normalized directory, each relative path is
    resolved against it."""
    kind = step.get("type")
    if kind == "file_write" and not isinstance(step.get("path"), str):
        return None
    if kind == "shell" and not isinstance(step.get("command"), str):
        return None
    acc = step_accesses(step, base)
    if acc.writes_unknown is not None:
        return None
    return [a.path for a in acc.writes]


def operand_writes(command: str, adds: bool) -> list[str] | None:
    """The normalized files one simple command writes through its operands.

    ``adds`` is True under ``xargs`` or ``find -exec``, which add operands
    the line does not show. A command not in ``WRITERS`` writes none."""
    acc = operand_accesses(command, "xargs" if adds else None)
    if acc.writes_unknown is not None:
        return None
    return [a.path for a in acc.writes]


def _head_of(tokens: list[str]) -> tuple[int, str, str]:
    from opendaisugi.verify import _ENV_ASSIGN_RE

    i = 0
    while i < len(tokens) and _ENV_ASSIGN_RE.match(tokens[i]):
        i += 1
    if i == len(tokens):
        return i, "", ""
    return i, tokens[i], tokens[i].rsplit("/", 1)[-1]


def _level(command: str, depth: int, bases: tuple[str, ...], under: str | None, ctx: _Ctx) -> None:
    from opendaisugi.verify import _MAX_INTERPRETER_DEPTH, _SHELL_METACHAR_RE

    if depth > _MAX_INTERPRETER_DEPTH:
        ctx.unknown(_R_DEPTH)
        return
    stripped = command.strip()
    if not stripped:
        return
    cmds: tuple[str, ...] = (stripped,)
    if _SHELL_METACHAR_RE.search(command):
        from opendaisugi.shell_decompose import decompose_command

        decomp = decompose_command(command)
        if not decomp.ok:
            ctx.unknown(_R_EXTRA if decomp.reason == _PARSER_MISSING else _R_GRAMMAR)
            return
        cmds = decomp.commands
        lead = _leading_cd(cmds, stripped)
        if lead is not None and len(bases) == 1:
            bases = bases + (_join(bases[0], lead),)
        else:
            lead = None
        for p in decomp.writes:
            ctx.writes.append((p, False, "redirect", bases))
        for p in decomp.reads:
            ctx.reads.append((p, False, "redirect", bases))
        for n, simple in enumerate(cmds):
            _simple(simple, depth, bases, under, ctx, n == 0 and lead is not None)
        return
    lead = _leading_cd(cmds, stripped)
    if lead is not None and len(bases) == 1:
        bases = bases + (_join(bases[0], lead),)
    else:
        lead = None
    _simple(stripped, depth, bases, under, ctx, lead is not None)


def _leading_cd(cmds: tuple[str, ...], line: str) -> str | None:
    """The literal operand of a first command that is ``cd LIT``, else None.

    The first decomposed command is the leading one only when the line starts
    with it. A line that starts with ``for``, ``while``, ``(``, ``{`` or a
    function definition has its ``cd`` inside a body that can run any number
    of times, so that ``cd`` is a mover like any other."""
    from opendaisugi.effects import _unsafe_words

    if not cmds or not line.startswith(cmds[0]):
        return None
    try:
        tokens = shlex.split(cmds[0])
    except ValueError:
        return None
    if len(tokens) != 2 or tokens[0] != "cd" or _unsafe_words(cmds[0]):
        return None
    lit = tokens[1]
    if not lit or lit.startswith(("-", "~")):
        return None
    return lit


# env options whose value is the next word (or the rest of the bundle).
ENV_VALUE_SHORT = "uCSa"


def _is_mover(head: str, raw_head: str, args: list[str]) -> bool:
    if head in CWD_MOVERS or raw_head in CWD_MOVERS:
        return True
    if head != "env":
        return False
    from opendaisugi.interpreter_parse import _ENV_LONG, resolve_long

    i = 0
    while i < len(args):
        a = args[i]
        i += 1
        if a.startswith("--"):
            got = resolve_long(a, _ENV_LONG)
            if got is None:
                return False  # the payload is opaque: the line is unknown anyway
            name, value, kind = got
            if name in ("--chdir", "--split-string"):
                return True
            if kind == "v" and value is None:
                i += 1
        elif a.startswith("-") and a != "-":
            for j, c in enumerate(a[1:], 1):
                if c in "CS":
                    return True
                if c in ENV_VALUE_SHORT:
                    if j == len(a) - 1:
                        i += 1
                    break
        elif "=" not in a:
            break
    return False


def _strip_process_substitution(text: str) -> str | None:
    """``text`` without its ``<(...)`` and ``>(...)`` words, which are inner
    commands the decomposition checks on their own; None when a span does
    not close."""
    out: list[str] = []
    i = 0
    n = len(text)
    quote = ""
    while i < n:
        c = text[i]
        if quote:
            out.append(c)
            if c == "\\" and quote == '"' and i + 1 < n:
                out.append(text[i + 1])
                i += 1
            elif c == quote:
                quote = ""
        elif c == "\\" and i + 1 < n:
            out.append(c)
            out.append(text[i + 1])
            i += 1
        elif c in "'\"":
            quote = c
            out.append(c)
        elif c in "<>" and text.startswith("(", i + 1):
            depth = 0
            j = i + 1
            q = ""
            while j < n:
                d = text[j]
                if q:
                    if d == "\\" and q == '"' and j + 1 < n:
                        j += 1
                    elif d == q:
                        q = ""
                elif d == "\\" and j + 1 < n:
                    j += 1
                elif d in "'\"":
                    q = d
                elif d == "(":
                    depth += 1
                elif d == ")":
                    depth -= 1
                    if depth == 0:
                        break
                j += 1
            if j >= n:
                return None
            out.append(" ")
            i = j
        else:
            out.append(c)
        i += 1
    return "".join(out)


def _simple(
    text: str,
    depth: int,
    bases: tuple[str, ...],
    under: str | None,
    ctx: _Ctx,
    skip_mover: bool,
) -> None:
    from opendaisugi.interpreter_parse import OPTION_HEADS, parse_interpreter

    stripped = _strip_process_substitution(text)
    if stripped is None:
        ctx.unknown(_R_SPLIT)
        return
    try:
        tokens = shlex.split(stripped)
    except ValueError:
        ctx.unknown(_R_SPLIT)
        return
    i, raw_head, head = _head_of(tokens)
    if not head:
        return
    args = tokens[i + 1 :]
    if not skip_mover and _is_mover(head, raw_head, args):
        ctx.movers += 1
        if head == "env":
            # env -C moves the cwd of the command that follows and -S splits
            # a string into it: nothing about the line is placed.
            ctx.unknown(_R_MOVES)
            return
    if runs_a_command(head, raw_head, args):
        ctx.unknown(_R_RUNS)
        return
    wrapped = shlex.join(tokens[i:]) if i else text
    payload = parse_interpreter(wrapped)
    if payload is None or payload.opaque:
        if payload is not None and payload.head in OPTION_HEADS:
            ctx.unknown(_R_OPTION)
            return
        _operands(head, args, text, bases, under, ctx)
        return
    if head == "find":
        _find(args, text, bases, under, ctx)
    elif head == "xargs":
        _xargs_files(args, bases, ctx)
    elif head == "time":
        _time_files(args, bases, ctx)
    for n, inner in enumerate(payload.inner_commands):
        kinds = payload.inner_kinds
        kind = kinds[n] if n < len(kinds) else ""
        if under == "xargs" or head == "xargs":
            inner_under: str | None = "xargs"
        elif head == "find":
            inner_under = "find-execdir" if kind == "execdir" else "find-exec"
        else:
            inner_under = under
        _level(inner, depth + 1, bases, inner_under, ctx)


def _xargs_files(args: list[str], bases: tuple[str, ...], ctx: _Ctx) -> None:
    from opendaisugi.interpreter_parse import xargs_options

    scanned = xargs_options(args, 0)
    if scanned is None:
        return
    for name, value in scanned[1]:
        if name in ("-a", "--arg-file") and value is not None and value != "-":
            ctx.reads.append((value, False, "operand", bases))


def _time_files(args: list[str], bases: tuple[str, ...], ctx: _Ctx) -> None:
    """GNU ``time -o FILE`` (and ``--output``) writes FILE."""
    from opendaisugi.interpreter_parse import wrapper_options

    scanned = wrapper_options("time", args, 0)
    if scanned is None:
        return
    for name, value in scanned[1]:
        if name in ("-o", "--output") and value is not None:
            ctx.writes.append((value, False, "operand", bases))


def _unsafe(text: str) -> bool:
    from opendaisugi.effects import _unsafe_words

    return _unsafe_words(text.replace("{}", "_"))


def _find(args: list[str], text: str, bases: tuple[str, ...], under: str | None, ctx: _Ctx) -> None:
    if under == "xargs":
        ctx.unknown(_R_XARGS.format(head="find"))
        return
    if _unsafe(text):
        ctx.unknown(_R_UNSAFE)
        return
    i = 0
    while i < len(args) and args[i] in ("-H", "-L", "-P"):
        i += 1
    starts: list[str] = []
    while i < len(args) and args[i] not in FIND_ARGS and not args[i].startswith("-"):
        starts.append(args[i])
        i += 1
    if not starts:
        starts = ["."]
    reads: list[tuple[str, bool]] = [(s, True) for s in starts]
    writes: list[tuple[str, bool]] = []
    n = len(args)
    while i < n:
        tok = args[i]
        count = FIND_ARGS.get(tok)
        if count is None:
            ctx.unknown(_R_FIND_ARG)
            return
        if count == "*":
            j = i + 1
            while j < n and args[j] not in (";", "+"):
                j += 1
            i = j + 1  # an unterminated clause makes find fail; nothing runs
            continue
        assert isinstance(count, int)
        if i + count > n - 1:
            ctx.unknown(_R_FIND_ARG)
            return
        if tok == "-delete":
            writes.extend((s, True) for s in starts)
        elif tok in ("-fprint", "-fprint0", "-fls", "-fprintf"):
            writes.append((args[i + 1], False))
        i += 1 + count
    for p, t in reads:
        ctx.reads.append((p, t, "operand", bases))
    for p, t in writes:
        ctx.writes.append((p, t, "operand", bases))


def _operands(
    head: str,
    args: list[str],
    text: str,
    bases: tuple[str, ...],
    under: str | None,
    ctx: _Ctx,
) -> None:
    is_r = head in READERS
    is_w = head in WRITERS
    if not (is_r or is_w):
        return
    if under == "xargs":
        reason = _R_XARGS.format(head=head)
        ctx.unknown(reason, reads=is_r, writes=is_w)
        return
    if _unsafe(text):
        ctx.unknown(_R_UNSAFE, reads=is_r, writes=is_w)
        return
    execing = under in ("find-exec", "find-execdir")
    if execing:
        if any("{}" in a and a != "{}" for a in args):
            ctx.unknown(_R_EMBED, reads=is_r, writes=is_w)
            return
        if is_w:
            ctx.unknown_writes(_R_FIND_EXEC)
    got = _head_accesses(head, args)
    if isinstance(got, str):
        ctx.unknown(got.format(head=head), reads=is_r, writes=is_w)
        return
    if got is None:
        # A flag outside the closed set can read or write a file the table
        # does not know (``sort -o FILE``, ``less -o FILE``): both are unknown.
        ctx.unknown(_R_FLAG.format(head=head), reads=True, writes=True)
        return
    reads, writes = got
    if execing:
        # a bare {} is the file find found: find's own start tree covers it
        reads = [(p, t) for p, t in reads if p != "{}"]
    if not is_r:
        reads = []
    if not execing:
        for p, t in writes:
            ctx.writes.append((p, t, "operand", bases))
    for p, t in reads:
        if under == "find-execdir" and not p.startswith("/"):
            if ".." in p.split("/"):
                ctx.unknown_reads(_R_EXECDIR)
            continue
        ctx.reads.append((p, t, "operand", bases))


Pairs = list[tuple[str, bool]]


def _head_accesses(head: str, args: list[str]) -> tuple[Pairs, Pairs] | str | None:
    """(reads, writes) of one table command, a reason, or None for a flag
    outside the closed set."""
    rule = (WRITERS.get(head) or READERS[head])["rule"]
    if rule == "tar":
        return _tar(args)
    if rule == "dd":
        reads: Pairs = []
        writes: Pairs = []
        for a in args:
            key, eq, value = a.partition("=")
            if a.startswith("-") or not eq or key not in DD_KEYS:
                return None
            if key == "if":
                reads.append((value, False))
            elif key == "of":
                writes.append((value, False))
        return reads, writes
    if rule == "find":
        return [], []
    spec = WRITERS.get(head) or READERS[head]
    parsed = _parse_flags(args, spec)
    if parsed is None:
        return None
    opts, ops = parsed
    names = [n for n, _ in opts]
    if rule == "inout":
        if len(ops) > 2:
            return None
        return (
            [(o, False) for o in ops[:1] if o != "-"],
            [(o, False) for o in ops[1:] if o != "-"],
        )
    ops = [o for o in ops if o != "-"] if rule not in ("grep", "rg", "sed") else ops
    if rule == "files":
        return [(o, False) for o in ops], []
    if rule == "diff":
        rec = "-r" in names or "--recursive" in names
        return [(o, rec) for o in ops], []
    if rule in ("grep", "rg"):
        return _grep(rule, opts, names, ops), []
    if rule == "ls":
        rec = "-R" in names or "--recursive" in names
        return [(o, rec) for o in (ops or ["."])], []
    if rule == "all":
        rec = head == "rm" and any(n in ("-r", "-R", "--recursive") for n in names)
        return [], [(o, rec) for o in ops]
    if rule == "sed":
        scripts = [v or "" for n, v in opts if n in ("-e", "--expression")]
        has_file = any(n in ("-f", "--file") for n in names)
        if has_file or sed_script_risky("\n".join(scripts) if scripts else (ops[0] if ops else "")):
            return _R_SED
        inplace = [v for n, v in opts if n in ("-i", "--in-place")]
        scripted = any(n in ("-e", "--expression", "-f", "--file") for n in names)
        files = [f for f in (ops if scripted else ops[1:]) if f != "-"]
        sreads: Pairs = [(v or "", False) for n, v in opts if n in ("-f", "--file")]
        sreads += [(f, False) for f in files]
        if not inplace:
            return sreads, []
        suffix = inplace[-1] or ""
        if not suffix:
            return sreads, [(f, False) for f in files]
        if "*" in suffix or "/" in suffix:
            return None
        return sreads, [(f, False) for f in files + [f + suffix for f in files]]
    if rule == "install" and ("-d" in names or "--directory" in names):
        return [], [(o, False) for o in ops]
    target = [v or "" for n, v in opts if n in ("-t", "--target-directory")]
    no_target = "-T" in names or "--no-target-directory" in names
    if target and no_target:
        return None
    if target:
        dest: str = target[-1]
        sources = ops
    elif head == "ln" and len(ops) == 1:
        return [], [(_basename(ops[0]), False)]
    elif len(ops) < 2:
        return [], []
    else:
        dest, sources = ops[-1], ops[:-1]
    if rule == "rsync":
        local = [s for s in sources if not _is_remote(s)]
        rw: Pairs = [(s, True) for s in local] if "--remove-source-files" in names else []
        if not _is_remote(dest):
            rw += [(w, True) for w in _under(dest, sources)]
        return [(s, True) for s in local], rw
    if rule == "move":
        written = [dest] if no_target else _under(dest, sources)
        return [(s, True) for s in sources], [(s, True) for s in sources + written]
    rec = head == "cp" and any(n in ("-r", "-R", "-a", "--recursive", "--archive") for n in names)
    if no_target:
        wr: Pairs = [(dest, rec)]
    else:
        wr = [(dest, False)] + [(w, rec) for w in _under(dest, sources)[1:]]
    rd: Pairs = []
    if head == "cp":
        rd = [(s, rec) for s in sources]
    elif rule == "install":
        rd = [(s, False) for s in sources]
    return rd, wr


def _grep(
    rule: str,
    opts: list[tuple[str, str | None]],
    names: list[str],
    ops: list[str],
) -> Pairs:
    has_pattern = any(n in ("-e", "--regexp", "-f", "--file") for n in names)
    if rule == "rg" and "--files" in names:
        has_pattern = True
    files = ops if has_pattern else ops[1:]
    out: Pairs = [(v or "", False) for n, v in opts if n in ("-f", "--file") and v != "-"]
    if rule == "rg":
        recursive = True
    else:
        recursive = any(
            n in ("-r", "-R", "--recursive", "--dereference-recursive") for n in names
        ) or any(n in ("-d", "--directories") and v == "recurse" for n, v in opts)
    files = [f for f in files if f != "-"]
    if not files and recursive:
        files = ["."]
    return out + [(f, recursive) for f in files]


_SED_PLAIN = frozenset("=dDgGhHnNpPxzF")
_SED_NUMBERED = frozenset("lqQL")
_DIGITS = "0123456789"
_SED_END = frozenset(" \t\n;}#")


def _sed_skip_regex(s: str, j: int, delim: str) -> int:
    """The index after the closing ``delim`` of a regex that starts at ``j``,
    or -1 when it does not close. A backslash escapes the next character and
    a bracket expression hides the delimiter, as in GNU sed."""
    n = len(s)
    while j < n:
        c = s[j]
        if c == "\\":
            j += 2
        elif c == "[":
            j += 1
            if j < n and s[j] == "^":
                j += 1
            if j < n and s[j] == "]":
                j += 1
            while j < n and s[j] != "]":
                if s[j] == "[" and j + 1 < n and s[j + 1] in ":.=":
                    end = s.find(s[j + 1] + "]", j + 2)
                    if end < 0:
                        return -1
                    j = end + 2
                else:
                    j += 1
            if j >= n:
                return -1
            j += 1
        elif c == delim:
            return j + 1
        elif c == "\n":
            return -1
        else:
            j += 1
    return -1


def _sed_skip_replacement(s: str, j: int, delim: str) -> int:
    n = len(s)
    while j < n:
        c = s[j]
        if c == "\\":
            j += 2
        elif c == delim:
            return j + 1
        else:
            j += 1
    return -1


def _sed_skip_line(s: str, j: int, continued: bool) -> int:
    """The index of the newline that ends the text from ``j`` (or the end).
    With ``continued`` a backslash before a newline continues the text."""
    n = len(s)
    while j < n and s[j] != "\n":
        j += 2 if continued and s[j] == "\\" else 1
    return min(j, n)


def sed_script_risky(script: str) -> bool:
    """True when a sed script may read, write or run a file the placer cannot
    place: an ``r``, ``R``, ``w``, ``W`` or ``e`` command, an ``s`` flag ``w``
    or ``e``, or anything the reader below does not understand."""
    s = script
    n = len(s)
    i = 0
    while i < n:
        while i < n and s[i] in " \t\n;":
            i += 1
        if i >= n:
            break
        if s[i] == "#":
            i = _sed_skip_line(s, i, False)
            continue
        for k in range(2):
            i, ok = _sed_address(s, i, k == 1)
            if not ok:
                return True
            while i < n and s[i] in " \t":
                i += 1
            if k == 0:
                if i < n and s[i] == ",":
                    i += 1
                    while i < n and s[i] in " \t":
                        i += 1
                else:
                    break
        while i < n and s[i] in " \t!":
            i += 1
        if i >= n:
            return True
        cmd = s[i]
        i += 1
        if cmd == "{":
            continue
        if cmd == "}" or cmd in _SED_PLAIN:
            pass
        elif cmd in _SED_NUMBERED:
            while i < n and s[i] in _DIGITS:
                i += 1
        elif cmd in "aic":
            i = _sed_skip_line(s, i, True)
            continue
        elif cmd in "btT:":
            while i < n and s[i] in " \t":
                i += 1
            while i < n and s[i] not in " \t\n;":
                i += 1
            continue
        elif cmd == "y":
            if i >= n or s[i] in "\n\\":
                return True
            d = s[i]
            i = _sed_skip_replacement(s, i + 1, d)
            if i < 0:
                return True
            i = _sed_skip_replacement(s, i, d)
            if i < 0:
                return True
        elif cmd == "s":
            if i >= n or s[i] in "\n\\":
                return True
            d = s[i]
            i = _sed_skip_regex(s, i + 1, d)
            if i < 0:
                return True
            i = _sed_skip_replacement(s, i, d)
            if i < 0:
                return True
            while i < n and (s[i] in _DIGITS or s[i] in "gpiImM"):
                i += 1
        else:
            return True
        if i < n and s[i] not in _SED_END:
            return True
    return False


def _sed_address(s: str, i: int, second: bool) -> tuple[int, bool]:
    """Skip one address at ``i``. Returns (new index, ok); ok is False when
    an address does not close. No address leaves ``i`` where it was."""
    n = len(s)
    if i >= n:
        return i, True
    c = s[i]
    if c in _DIGITS:
        while i < n and s[i] in _DIGITS:
            i += 1
        if i < n and s[i] == "~":
            i += 1
            while i < n and s[i] in _DIGITS:
                i += 1
        return i, True
    if c == "$":
        return i + 1, True
    if second and c in "+~":
        i += 1
        while i < n and s[i] in _DIGITS:
            i += 1
        return i, True
    if c == "/" or c == "\\":
        d = "/"
        if c == "\\":
            i += 1
            if i >= n or s[i] in "\n\\":
                return i, False
            d = s[i]
        i = _sed_skip_regex(s, i + 1, d)
        if i < 0:
            return i, False
        while i < n and s[i] in "IM":
            i += 1
        return i, True
    return i, True


def _tar(args: list[str]) -> tuple[Pairs, Pairs] | str | None:
    modes: list[str] = []
    archive: list[str] = []
    dirs: list[str] = []
    ops: list[str] = []
    long_modes = {"--create": "c", "--extract": "x", "--list": "t"}
    i = 0
    n = len(args)
    first = True
    while i < n:
        a = args[i]
        i += 1
        if a == "--":
            ops.extend(args[i:])
            break
        if a.startswith("--"):
            name, eq, value = a.partition("=")
            if name in long_modes and not eq:
                modes.append(long_modes[name])
            elif name in TAR["long"] and not eq:
                pass
            elif name in TAR["long_value"]:
                if not eq:
                    if i >= n:
                        return None
                    value = args[i]
                    i += 1
                if name == "--file":
                    archive.append(value)
                elif name == "--directory":
                    if ops:
                        return None
                    dirs.append(value)
            else:
                return None
        elif a.startswith("-") and a != "-":
            body = a[1:]
            j = 0
            while j < len(body):
                c = body[j]
                j += 1
                if c in TAR["modes"]:
                    modes.append(c)
                elif c in TAR["flags"]:
                    continue
                elif c in TAR["value"]:
                    rest = body[j:]
                    if rest:
                        value = rest
                    elif i < n:
                        value = args[i]
                        i += 1
                    else:
                        return None
                    if c == "f":
                        archive.append(value)
                    else:
                        if ops:
                            return None
                        dirs.append(value)
                    break
                else:
                    return None
        elif first and a != "-":
            # Old style: letters with no dash; values come from the next words in order.
            for c in a:
                if c in TAR["modes"]:
                    modes.append(c)
                elif c in TAR["flags"]:
                    continue
                elif c in TAR["value"]:
                    if i >= n:
                        return None
                    value = args[i]
                    i += 1
                    if c == "f":
                        archive.append(value)
                    else:
                        dirs.append(value)
                else:
                    return None
        else:
            ops.append(a)
        first = False
    if len(archive) > 1 or len(dirs) > 1:
        return None
    if len(modes) != 1 or not archive:
        return _R_TAR
    mode = modes[0]
    where = archive[0]
    if _is_remote(where):
        return _R_REMOTE_TAR
    stream = where == "-"
    directory = dirs[0] if dirs else None
    reads: Pairs = []
    writes: Pairs = []
    if mode == "c":
        if not stream:
            writes.append((where, False))
        for o in ops:
            if directory is not None and not o.startswith("/"):
                o = posixpath.normpath(directory + "/" + o)
            reads.append((o, True))
    else:
        if not stream:
            reads.append((where, False))
        if mode == "x":
            writes.append((directory if directory is not None else ".", True))
    return reads, writes


def _parse_flags(
    args: list[str], spec: dict[str, Any]
) -> tuple[list[tuple[str, str | None]], list[str]] | None:
    """The flags (name and value) and the operands of args, or None when a
    flag is outside spec or lacks its value."""
    short = spec.get("short", "")
    short_value = spec.get("short_value", "")
    short_optional = spec.get("short_optional", "")
    long = spec.get("long", [])
    long_value = spec.get("long_value", [])
    long_optional = spec.get("long_optional", [])
    opts: list[tuple[str, str | None]] = []
    ops: list[str] = []
    i = 0
    ended = False
    while i < len(args):
        a = args[i]
        i += 1
        if ended or a == "-" or not a.startswith("-"):
            ops.append(a)
            continue
        if a == "--":
            ended = True
            continue
        if a.startswith("--"):
            name, eq, value = a.partition("=")
            if eq:
                if name in long_value or name in long_optional:
                    opts.append((name, value))
                    continue
                return None
            if name in long or name in long_optional:
                opts.append((name, None))
                continue
            if name in long_value:
                if i >= len(args):
                    return None
                opts.append((name, args[i]))
                i += 1
                continue
            return None
        body = a[1:]
        for j, c in enumerate(body):
            if c in short:
                opts.append(("-" + c, None))
                continue
            if c in short_optional:
                opts.append(("-" + c, body[j + 1 :]))
                break
            if c in short_value:
                rest = body[j + 1 :]
                if rest:
                    opts.append(("-" + c, rest))
                elif i >= len(args):
                    return None
                else:
                    opts.append(("-" + c, args[i]))
                    i += 1
                break
            return None
    return opts, ops


def _basename(path: str) -> str:
    return posixpath.basename(path.rstrip("/"))


def _under(dest: str, sources: list[str]) -> list[str]:
    """dest, then dest/basename(source) for each source that has one."""
    out = [dest]
    for s in sources:
        b = _basename(s)
        if b not in ("", ".", ".."):
            out.append(dest + "/" + b)
    return out


def _is_remote(word: str) -> bool:
    """True for an rsync operand on another host: HOST:PATH or rsync://."""
    if word.startswith("rsync://"):
        return True
    colon = word.find(":")
    return colon > 0 and "/" not in word[:colon]


__all__ = [
    "CWD_MOVERS",
    "DD_KEYS",
    "FIND_ARGS",
    "READERS",
    "RUNNERS",
    "STDIN_RUNNERS",
    "runs_a_command",
    "TAR",
    "UNKNOWN_REASONS",
    "WRITERS",
    "Access",
    "Accesses",
    "operand_accesses",
    "operand_writes",
    "shell_accesses",
    "step_accesses",
    "step_write_paths",
]

"""Differential fixture generator for the Go shlex port.

Runs Python's REAL shlex.split(s, posix=True) (exactly as
interpreter_parse.py calls it) over a mix of hand-picked edge cases and
strings pulled from the real corpus, and dumps NDJSON {command, tokens|error}
for the Go test to assert against byte-for-byte.

    CUDA_VISIBLE_DEVICES="" uv run python clients/go/probe_shlex.py \
        > clients/go/internal/verify/testdata/shlex_fixture.jsonl
"""

import json
import shlex

HAND = [
    "git status",
    "  git status  ",
    "",
    "   ",
    "a'b'c",
    'a"b"c',
    "a\\ b",
    "a\\\\b",
    'a\\"b',
    "a\\'b",
    "'it'\\''s'",
    '"a\\"b"',
    '"a\\\\b"',
    '"a\\nb"',
    "'a\\nb'",
    "''",
    '""',
    "a''b",
    'a""b',
    "'unterminated",
    '"unterminated',
    "trailing\\",
    "a\\",
    "sh -c 'git status'",
    'bash -c "ls -la"',
    "xargs -I {} mv {} /d",
    "find . -exec rm {} ;",
    "python -c 'print(\"hi\")'",
    "a\tb\nc\rd",
    "café",
    "emoji 🎉 arg",
    "a=b c=d cmd",
    "--flag=value",
    "-c",
    "-ec",
    "-cSCRIPT",
    "a#b",
    "# comment",
    "cmd # not a comment",
    "a\\#b",
]

CORPUS_SAMPLE_CMDS = [
    "cd /repo && pytest -q",
    "$CMD --do-things && ls",
    "find . -name '*.tmp' | xargs rm",
    "sh -c 'rm -rf /' && ls",
    "grep -r TODO src | sort > /tmp/hits.txt",
    "echo x > /etc/cron.d/pwn",
    "git status && ls -la",
    "pytest -q > /etc/passwd",
    "git status && rm -rf /tmp/x",
    "timeout 30 git fetch && make install",
    "grep -E \"a|b\" f && ls",
    "prog --version > /dev/null 2>&1",
    "wc -l < /etc/shadow",
    "grep x f | sed -n 1,5p f",
    "echo $(rm -rf /) ok",
    "grep -n foo src/*.py | head -5 > out/hits.txt",
]

CASES = HAND + CORPUS_SAMPLE_CMDS


def main() -> None:
    for cmd in CASES:
        row = {"command": cmd}
        try:
            row["tokens"] = shlex.split(cmd, posix=True)
        except ValueError as e:
            row["error"] = str(e)
        print(json.dumps(row))


if __name__ == "__main__":
    main()

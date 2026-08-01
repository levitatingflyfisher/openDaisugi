"""Generate clients/fixtures/decompose.json — oracle-truth decompose expectations
over SYNTHESIZED commands (no real filesystem paths, safe to commit, unlike the
conformance corpus).

Two expectation kinds:
  * "match"  — the oracle accepts and we expect a client to reproduce its exact
               heads/reads/writes. These drive the parser-coverage work.
  * "reject" — the command is a deliberate client-side scope cut (e.g. process
               substitution, or a heredoc body carrying a substitution). The
               oracle may accept it; a subset client MUST fail closed. We record
               the client expectation (ok=false), not the oracle's.

Run from the repo root:  uv run python clients/fixtures/generate_decompose.py
"""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.shell_decompose import decompose_command

# (label, command, kind) where kind is "match" (expect oracle output) or
# "reject" (deliberate subset scope cut — client must reject regardless).
CASES: list[tuple[str, str, str]] = [
    # --- already supported (regression anchors) ---
    ("simple", "echo hello", "match"),
    ("pipe", "cat foo.txt | grep bar", "match"),
    ("andor", "make build && make test", "match"),
    ("seq", "cd src; ls -la", "match"),
    ("redir-write", "echo hi > out.txt", "match"),
    ("redir-read", "sort < in.txt", "match"),
    ("cmdsubst", "echo $(date)", "match"),
    ("dquote-subst", 'echo "today is $(date)"', "match"),
    # --- arithmetic $((...)) : an expansion, never a head ---
    ("arith-arg", "echo $((1 + 2))", "match"),
    ("arith-mid", "printf %d $((a * b)) done", "match"),
    # --- ${...} parameter expansion in argument / redirect-free positions ---
    ("paramexp-arg", "echo ${HOME}", "match"),
    ("paramexp-default", 'echo "${name:-nobody}"', "match"),
    ("paramexp-two", "cp ${src} ${dst}", "match"),
    # --- backticks : command substitution, recurse like $() ---
    ("backtick", "echo `whoami`", "match"),
    ("backtick-arg", "tar czf out.tgz `cat filelist`", "match"),
    # --- subshell / brace group ---
    ("subshell", "( cd build && cmake .. )", "match"),
    ("brace-group", "{ echo a; echo b; }", "match"),
    # --- compound keywords ---
    ("for-loop", "for f in a b c; do echo $f; done", "match"),
    ("if-block", "if test -f x; then echo yes; fi", "match"),
    ("while-loop", "while read line; do echo $line; done", "match"),
    ("case-block", "case $x in a) echo A;; *) echo other;; esac", "match"),
    ("for-with-cmd", "for f in $(ls); do rm $f; done", "match"),
    # --- heredocs : body is stdin data, not commands ---
    ("heredoc", "cat <<EOF\nhello world\nEOF", "match"),
    ("heredoc-quoted", "cat <<'EOF'\nliteral $stuff\nEOF", "match"),
    ("heredoc-dash", "cat <<-END\n\tindented\n\tEND", "match"),
    ("heredoc-sql", "sqlite3 db <<SQL\nSELECT 1;\nSQL", "match"),
    # --- assignment values with command substitutions (head emitted first) ---
    ("assign-subst", "RID=$(gh run list) gh view $RID", "match"),
    ("assign-subst-only", "gz=$(gzip -c f | wc -c)", "match"),
    ("assign-two", "a=$(one) b=$(two) run arg", "match"),
    ("assign-then-arg-subst", "FOO=$(echo x) realcmd $(echo y)", "match"),
    # --- multi-line statement lists ---
    ("multiline", "cd repo\ngit add .\ngit status", "match"),
    # --- deliberate scope cuts: client MUST reject even though oracle may accept ---
    ("procsubst", "diff <(sort a.txt) <(sort b.txt)", "reject"),
    ("heredoc-with-subst", "cat <<EOF\nresult: $(date)\nEOF", "reject"),
    ("nested-backtick", "echo `echo \\`date\\``", "reject"),
]


def expected(command: str) -> dict:
    d = decompose_command(command)
    if not d.ok:
        return {"ok": False}
    return {
        "ok": True,
        "heads": list(d.heads),
        "reads": sorted(d.reads),
        "writes": sorted(d.writes),
    }


def main() -> None:
    out = []
    for label, command, kind in CASES:
        exp = expected(command) if kind == "match" else {"ok": False}
        oracle = expected(command)
        out.append({
            "label": label,
            "command": command,
            "kind": kind,
            "expected": exp,
            "oracle": oracle,  # informative: what the oracle actually returns
        })
    path = Path(__file__).parent / "decompose.json"
    path.write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
    match = sum(1 for c in out if c["kind"] == "match")
    reject = sum(1 for c in out if c["kind"] == "reject")
    print(f"wrote {path} — {len(out)} cases ({match} match, {reject} reject)")


if __name__ == "__main__":
    main()

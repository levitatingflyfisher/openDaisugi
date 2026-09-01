"""Generate bench/corpus/verifier.jsonl from a hand-written case list.

The ids are content addresses. See docs/spec/conformance.md. They cannot be
typed by hand, so this script builds them with the same helpers the oracle
uses and writes the manifest that pins the bytes. Output is deterministic.
Run it twice and you get the same file.

Nothing here is exported from a recorded corpus. Recorded cases embed real
local paths, which is why the frozen corpora are never committed.

    uv run --no-sync python bench/corpus/make_verifier_corpus.py --out bench/corpus/verifier.jsonl
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

from opendaisugi.conformance import (
    CONFORMANCE_VERSION,
    canonical_json,
    make_decompose_case,
    make_verify_case,
)
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    FileWriteStep,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.shell_decompose import decompose_command
from opendaisugi.verify import verify

# Forty decomposition commands. Two thirds are ordinary agent lines. The rest
# are the shapes the client campaign found interesting: pipes, and-or lists,
# heredocs, subshells, redirections, compounds, and a few that must fail closed.
COMMANDS: tuple[str, ...] = (
    "echo hello",
    "ls -la",
    "git status",
    "git commit -m 'fix the parser'",
    "cat foo.txt | grep bar",
    "grep -c x f | sort > out",
    "make build && make test",
    "pytest -q || echo failed",
    "cd src; ls",
    "FOO=1 git status",
    "FOO=1 BAR=2 make",
    "find . -name '*.py' -print",
    "python -m opendaisugi.conformance",
    "uv run --no-sync pytest tests -q",
    "rm -rf build",
    "mkdir -p build && cp a.txt build/",
    "tar czf out.tgz src",
    "sed -n '1,20p' README.md",
    "awk '{print $1}' data.txt",
    "curl https://example.com -o page.html",
    "echo $(date)",
    "echo `hostname`",
    "echo ${HOME}",
    "echo $((1 + 2))",
    "( cd src && ls )",
    "cat <<EOF\nhello\nEOF",
    "cat <<'EOF'\n$(date)\nEOF",
    "if [ -f a ]; then cat a; else echo none; fi",
    "for f in *.py; do echo $f; done",
    "while read line; do echo $line; done < in.txt",
    "until false; do break; done",
    "! grep -q x f",
    "ls > out.txt 2>&1",
    "diff <(sort a) <(sort b)",
    "case $x in a) echo a;; esac",
    "{ echo a; echo b; }",
    "git log --oneline | head -20 > log.txt",
    "npm install && npm run build",
    "go build -o conform ./cmd/conform",
    "lake build",
)

_ENV_STRICT = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="run the project's tests",
    stakes="medium",
    permissions=Permission(shell=True, shell_allowlist=["pytest", "echo"]),
)
_ENV_OPEN = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="run the project's tests",
    stakes="medium",
    permissions=Permission(shell=True, shell_allowlist=["pytest", "echo", "git", "ls"]),
)

# The predicate case. Its denial turns on a Full-profile stage: permissions,
# DAG and delegation all pass, and only the invariant refuses the file_write
# step. A Core-profile client runs delegation, permissions and dag and then
# returns ok, so it answers `ok: true` here. This one case is what lets the
# fail-open column of `daisugi bench verifier` read non-zero instead of
# certifying a hole. Do not remove it to make a row green.
_ENV_PREDICATE = Envelope(
    id="env_case",
    generated_by="hand-written for bench/corpus",
    task="write a build artifact",
    stakes="medium",
    permissions=Permission(
        shell=True, shell_allowlist=["pytest"], file_write=["build/**"], file_read=["**"]
    ),
    invariants=[
        Invariant(
            type="shell_only",
            description="every step is a shell step",
            expr={
                "op": "forall_steps",
                "pred": {"op": "equals", "path": "type", "value": "shell"},
            },
        )
    ],
)

# Seven shell verification cases: three that pass, four that fail on
# different stages. The eighth is the predicate case above. It is built
# separately because it needs a non-shell step.
#
# `deny-compound-allowlisted` is the one case that turns on the envelope's
# shell_allow_decomposition: every head is in the allowlist, so it is refused
# only because the envelope refuses compounds outright. The shell pair in
# `daisugi bench pairs` flips that field and expects this verdict to flip
# with it. Do not remove it to make the two settings read alike.
VERIFY_CASES: tuple[tuple[str, Envelope, tuple[tuple[str, str], ...]], ...] = (
    ("pass-echo", _ENV_OPEN, (("s1", "echo ok"),)),
    ("pass-two", _ENV_OPEN, (("s1", "git status"), ("s2", "ls"))),
    ("pass-pytest", _ENV_STRICT, (("s1", "pytest -q"),)),
    ("deny-head", _ENV_STRICT, (("s1", "curl https://example.com"),)),
    ("deny-second", _ENV_STRICT, (("s1", "echo ok"), ("s2", "rm -rf build"))),
    ("deny-compound", _ENV_STRICT, (("s1", "echo ok && rm -rf /"),)),
    ("deny-compound-allowlisted", _ENV_STRICT, (("s1", "echo ok && echo done"),)),
)


def _predicate_plan() -> ActionPlan:
    """A plan every structural stage accepts and only the invariant refuses."""
    return ActionPlan(
        id="plan_case",
        source="bench/deny-predicate",
        task=_ENV_PREDICATE.task,
        steps=[
            ShellStep(id="s1", command="pytest -q"),
            FileWriteStep(id="s2", path="build/out.txt", content="ok"),
        ],
    )


def build_cases() -> list[dict]:
    cases: list[dict] = []
    for command in COMMANDS:
        cases.append(make_decompose_case(command, decompose_command(command)))
    options = {"strict": None, "z3_timeout_ms": 500}
    plans = [
        (
            envelope,
            ActionPlan(
                id="plan_case",
                source=f"bench/{name}",
                task=envelope.task,
                steps=[ShellStep(id=sid, command=cmd) for sid, cmd in steps],
            ),
        )
        for name, envelope, steps in VERIFY_CASES
    ]
    plans.append((_ENV_PREDICATE, _predicate_plan()))
    for envelope, plan in plans:
        result = verify(plan, envelope, strict=None, z3_timeout_ms=500)
        cases.append(make_verify_case(plan, envelope, options, result))
    return sorted(cases, key=lambda c: c["id"])


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()

    cases = build_cases()
    body = "".join(canonical_json(c) + "\n" for c in cases)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(body, encoding="utf-8")
    manifest = {
        "v": CONFORMANCE_VERSION,
        "count": len(cases),
        "sha256": hashlib.sha256(body.encode("utf-8")).hexdigest(),
    }
    args.out.with_suffix(args.out.suffix + ".manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"{args.out}: {len(cases)} cases, sha256 {manifest['sha256'][:8]}")


if __name__ == "__main__":
    main()

"""Cases for `daisugi pando`, written from the Go binary.

    uv run --no-sync python clients/pando_cases.py --binary PATH/daisugi-go [--out DIR]

The Go `daisugi pando` is the oracle of the Pando R2 move (ADR-0023). Each
case lays out a scratch HOME and data home, runs one or more commands, and
records the exit code, stdout and stderr of every step and the tree of files
the store tree holds after. Paths read as {SCRATCH}; the day of the run reads
as {TODAY} (a message without a date of its own is dated now). The invented
content is all here or in clients/fixtures/pando. `pando_compare.py` replays
the file through any other binary.
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
FIXTURES = HERE / "fixtures" / "pando"
FIXTURE_DIR = FIXTURES / "cli"
SCRATCH_ROOT = Path.home() / "opendaisugi-scratch" / "pando" / "cases"

LONG = "word " * 300
SESSION_CODEX = [
    '{"timestamp":"2026-03-05T08:00:00.000Z","type":"session_meta","payload":{"id":"c1","cwd":"/work/other"}}',
    '{"timestamp":"2026-03-05T08:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"write a test"}]}}',
    '{"timestamp":"2026-03-05T08:00:05.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done, the test passes"}]}}',
]
SESSION_CLAUDE = [
    '{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"2026-03-01T10:00:00.000Z","cwd":"/old/proj/app","message":{"role":"user","content":"add a cache"}}',
    '{"type":"assistant","uuid":"a1","sessionId":"s1","timestamp":"2026-03-01T10:00:03.000Z","cwd":"/old/proj/app","message":{"role":"assistant","content":[{"type":"text","text":"cache added"}]}}',
]
CHATS_DIR = [
    {
        "file": "{SCRATCH}/chats/-other-proj/z1.jsonl",
        "text": SESSION_CLAUDE[1].replace('"s1"', '"z1"').replace('"a1"', '"za1"') + "\n",
    }
]
BIG_SESSION = (
    "\n".join(
        '{"type":"user","uuid":"b%d","sessionId":"big","timestamp":"2026-03-01T10:00:%02d.000Z","cwd":"/old/proj/app","message":{"role":"user","content":"%s"}}'
        % (i, i, "word " * 300)
        for i in range(40)
    )
    + "\n"
)
FAKE_CLAUDE = "#!/bin/sh\ncat >/dev/null\necho 'a short summary line'\n"
# A model that gives no text for a source that holds the word clipme, as a
# thinking model out of tokens gives none, and a line for any other.
FAKE_CLAUDE_GAP = (
    "#!/bin/sh\nin=$(cat)\ncase \"$in\" in *clipme*) exit 0;; esac\necho 'a short summary line'\n"
)
CLIPME = "clipme " * 200
# A model for the merge question: it answers "MARKER LEAK" when the left part
# holds a clip marker, "merged line" for any other merge, nothing for a source
# that holds clipme, and a line for the rest.
FAKE_CLAUDE_MERGE = (
    "#!/bin/sh\nin=$(cat)\n"
    "case \"$in\" in *'<left>'*'[clipped]'*) echo 'MARKER LEAK'; exit 0;; esac\n"
    "case \"$in\" in *'<left>'*) echo 'merged line'; exit 0;; esac\n"
    'case "$in" in *clipme*) exit 0;; esac\n'
    "echo 'a short summary line'\n"
)
# A model that numbers its answers (the count sits in a file next to it): a
# long line for a message and a short one for a merge, so every level has a
# line of its own that a later run can tell from an earlier one.
FAKE_CLAUDE_LAYERS = (
    "#!/bin/sh\nin=$(cat)\n"
    'n=$(cat "$0.n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$0.n"\n'
    'case "$in" in *\'<left>\'*) echo "merge line $n";; '
    "*) printf 'leaf line %s ' \"$n\"; head -c 300 /dev/zero | tr '\\0' x; echo;; esac\n"
)
# The same model, and its tenth call fails: with four messages the tend takes
# six calls, so the fourth call of a redo is the one that fails.
FAKE_CLAUDE_FLAKY = FAKE_CLAUDE_LAYERS.replace(
    "case \"$in\" in *'<left>'*)", '[ "$n" = 10 ] && exit 1\ncase "$in" in *\'<left>\'*)', 1
)
# The same model, and its seventh call fails: the first call of a redo after a tend of four messages.
FAKE_CLAUDE_FLAKY_FIRST = FAKE_CLAUDE_LAYERS.replace(
    "case \"$in\" in *'<left>'*)", '[ "$n" = 7 ] && exit 1\ncase "$in" in *\'<left>\'*)', 1
)
MID = "word " * 130


def golden(store: str = "work") -> dict[str, Any]:
    return {"golden": store}


def step(*argv: str, stdin: str = "", env: dict[str, str] | None = None) -> dict[str, Any]:
    out: dict[str, Any] = {"argv": list(argv), "stdin": stdin}
    if env:
        out["env"] = env
    return out


# Round 8b: branches. A lead's transcript, a nested lead's, and the hook
# events that log them. Paths read as {SCRATCH}.
def lead_line(sid: str, k: int, role: str, text: str) -> str:
    return json.dumps(
        {
            "type": role,
            "uuid": f"{sid}-{k}",
            "sessionId": sid,
            "timestamp": f"2026-03-02T10:00:0{k}.000Z",
            "cwd": "/work/trellis",
            "message": {"role": role, "content": [{"type": "text", "text": text}]},
        }
    )


LEAD = (
    "\n".join(
        [
            lead_line("lead1", 0, "user", "lead brief: build the reader with the epub parser"),
            lead_line("lead1", 1, "assistant", "the reader plan is ready"),
        ]
    )
    + "\n"
)
UI = "\n".join([lead_line("ui1", 0, "user", "ui brief: draw the lattice icon")]) + "\n"
LEAD_MORE = LEAD + lead_line("lead1", 2, "user", "resumed from a plain terminal") + "\n"
TRELLIS = {"DAISUGI_PANDO_BRANCH": "trellis"}
UI_ENV = {"DAISUGI_PANDO_BRANCH": "trellis/ui"}


def hook_ev(name: str, sid: str) -> str:
    return json.dumps(
        {
            "hook_event_name": name,
            "session_id": sid,
            "transcript_path": "{SCRATCH}/proj/" + sid + ".jsonl",
        }
    )


BRANCH_SETUP = [
    {"file": "{SCRATCH}/proj/lead1.jsonl", "text": LEAD},
    {"file": "{SCRATCH}/proj/ui1.jsonl", "text": UI},
]
BRANCH_STEPS = [
    step("pando", "append", "the household budget sheet lives in the attic"),
    step("pando", "hook", stdin=hook_ev("SessionStart", "lead1"), env=TRELLIS),
    step("pando", "hook", stdin=hook_ev("Stop", "lead1"), env=TRELLIS),
    step("pando", "hook", stdin=hook_ev("Stop", "ui1"), env=UI_ENV),
]


def mcp_call(name: str, args: dict[str, Any]) -> str:
    return json.dumps(
        {
            "jsonrpc": "2.0",
            "id": 9,
            "method": "tools/call",
            "params": {"name": name, "arguments": args},
        }
    )


def case(
    name: str,
    steps: list[dict[str, Any]],
    setup: list[dict[str, Any]] | None = None,
    go_only: bool = False,
) -> dict[str, Any]:
    # go_only: the Rust port is frozen behind the Go build (owner ruling,
    # Pando round 8b fix 3); a Rust runner skips the case.
    out: dict[str, Any] = {"name": name, "setup": setup or [], "steps": steps}
    if go_only:
        out["go_only"] = True
    return out


def build_cases() -> list[dict[str, Any]]:
    g = [golden()]
    web = "{WEB}"
    # Thirty messages: more than the 25 paid calls tend allows without --profile or --yes.
    many = [step("pando", "append", "-", stdin=LONG)] * 30
    cases: list[dict[str, Any]] = [
        case("no-args", [step("pando")]),
        case("help", [step("pando", "--help")]),
        case("unknown-verb", [step("pando", "nope")]),
        case("unknown-option", [step("pando", "--nope", "view")]),
        case("option-without-value", [step("pando", "view", "--store")]),
        case("arity", [step("pando", "zoom", "0")]),
        case("stores-none", [step("pando", "stores"), step("pando", "stores", "--json")]),
        case(
            "stores-two",
            [step("pando", "stores"), step("pando", "stores", "--json")],
            [golden("work"), golden("home")],
        ),
        case("status", [step("pando", "status")], g),
        case("status-json", [step("pando", "status", "--json")], g),
        case("status-missing-store", [step("pando", "status", "--store", "home")], g),
        case("store-all", [step("pando", "view", "--store", "all")], g),
        case("store-bad-name", [step("pando", "view", "--store", "Bad Name")], g),
        case("log", [step("pando", "log")], g),
        case("log-json", [step("pando", "log", "--json")], g),
        case("log-day", [step("pando", "log", "--day", "2026-10-07")], g),
        case("log-bad-day", [step("pando", "log", "--day", "nope")], g),
        case("view", [step("pando", "view")], g),
        case("view-json", [step("pando", "view", "--json")], g),
        case("context", [step("pando", "context")], g),
        case("zoom-leaf", [step("pando", "zoom", "0", "1")], g),
        case("zoom-node", [step("pando", "zoom", "0", "4")], g),
        case("zoom-root", [step("pando", "zoom", "0", "64")], g),
        case("zoom-json", [step("pando", "zoom", "3", "1", "--json")], g),
        case("zoom-page", [step("pando", "zoom", "0", "1", "--offset", "40", "--size", "60")], g),
        case("zoom-past-end", [step("pando", "zoom", "0", "1", "--offset", "99999")], g),
        case("zoom-bad-id", [step("pando", "zoom", "7", "2")], g),
        case("zoom-not-numbers", [step("pando", "zoom", "a", "b")], g),
        case("date", [step("pando", "date", "3"), step("pando", "date", "3", "--json")], g),
        case("date-missing", [step("pando", "date", "99999")], g),
        case(
            "recall",
            [
                step("pando", "recall", "warden status"),
                step("pando", "recall", "worker ledger", "-k", "3"),
            ],
            g,
        ),
        case("recall-json", [step("pando", "recall", "spawn pane", "--json", "-k", "2")], g),
        case("recall-no-match", [step("pando", "recall", "zzzzqqqq")], g),
        # Stopwords, case and punctuation: the garden's lexical matcher drops the
        # first and folds the rest, so a Rust steward must rank the same.
        case(
            "recall-lexical",
            [
                step("pando", "recall", "What is the Worker doing with the ledger?", "-k", "4"),
                step("pando", "recall", "WARDEN, status!", "--json", "-k", "3"),
            ],
            g,
        ),
        case("child-missing", [step("pando", "zoom", "0", "1", "--child", "w-1")], g),
        case("child-bad-label", [step("pando", "zoom", "0", "1", "--child", "../x")], g),
        case(
            "serve",
            [
                step(
                    "pando",
                    "serve",
                    stdin="\n".join(
                        [
                            '{"jsonrpc":"2.0","id":1,"method":"initialize"}',
                            '{"jsonrpc":"2.0","method":"notifications/initialized"}',
                            '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
                            '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"zoom","arguments":{"id":3,"n":1}}}',
                            '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"date","arguments":{"id":3}}}',
                            '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"recall","arguments":{"query":"spawn pane","k":2}}}',
                            '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"zoom","arguments":{"id":"x"}}}',
                            '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"append","arguments":{}}}',
                            '{"jsonrpc":"2.0","id":8,"method":"nope"}',
                            "not json",
                        ]
                    )
                    + "\n",
                )
            ],
            g,
        ),
        case(
            "append",
            [step("pando", "append", "we chose sqlite for the index"), step("pando", "log")],
        ),
        case(
            "append-json",
            [step("pando", "append", "a <b> & c", "--json"), step("pando", "log", "--json")],
        ),
        case(
            "append-note-and-store",
            [
                step("pando", "append", "--kind", "note", "a note", "--store", "home"),
                step("pando", "stores"),
            ],
        ),
        case("append-bad-kind", [step("pando", "append", "--kind", "tool", "x")]),
        case("append-empty", [step("pando", "append", "   ")]),
        case(
            "append-stdin",
            [
                step("pando", "append", "-", stdin="from standard input\nsecond line\n"),
                step("pando", "zoom", "0", "1"),
            ],
        ),
        case(
            "append-long-splits",
            [step("pando", "append", "-", stdin="x" * 70000), step("pando", "status", "--json")],
        ),
        case(
            "append-golden",
            [step("pando", "append", "after the golden run"), step("pando", "status")],
            g,
        ),
        case(
            "tend-dry-run",
            [step("pando", "append", "-", stdin=LONG), step("pando", "tend", "--dry-run")],
        ),
        case("tend-no-claude", [step("pando", "append", "-", stdin=LONG), step("pando", "tend")]),
        case(
            "tend-fake-claude",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend"),
                step("pando", "view"),
            ],
            [{"fake_claude": True}],
        ),
        case(
            "tend-max",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--max", "1"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude": True}],
        ),
        case("tend-bad-profile", [step("pando", "tend", "--profile", "nobody")], g),
        case(
            "tend-refuse-over-limit",
            [*many, step("pando", "tend"), step("pando", "status", "--json")],
        ),
        case(
            "tend-yes-over-limit",
            [*many, step("pando", "tend", "--yes"), step("pando", "status", "--json")],
            [{"fake_claude": True}],
        ),
        case(
            "tend-max-over-limit",
            [*many, step("pando", "tend", "--max", "3"), step("pando", "status", "--json")],
            [{"fake_claude": True}],
        ),
        case("tend-profile-over-limit", [*many, step("pando", "tend", "--profile", "nobody")]),
        case("tend-dry-run-over-limit", [*many, step("pando", "tend", "--dry-run")]),
        # -- a model that gives no text: tend counts it, stops, and --redo-clipped fixes it --
        case(
            "tend-clipped-why",
            [
                step("pando", "append", "-", stdin=CLIPME),
                step("pando", "append", "-", stdin=CLIPME),
                step("pando", "tend"),
                step("pando", "view"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-stops-after-five-clipped",
            [
                *[step("pando", "append", "-", stdin=CLIPME)] * 8,
                step("pando", "tend"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-redo-clipped",
            [
                *[step("pando", "append", "-", stdin=CLIPME)] * 2,
                step("pando", "tend"),
                step("pando", "view"),
                step("pando", "tend", "--redo-clipped", "--dry-run"),
                {"write": "{SCRATCH}/bin/claude", "text": FAKE_CLAUDE},
                step("pando", "tend", "--redo-clipped"),
                step("pando", "view"),
                step("pando", "tend", "--redo-clipped"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-redo-clipped-max",
            [
                *[step("pando", "append", "-", stdin=CLIPME)] * 2,
                step("pando", "tend"),
                {"write": "{SCRATCH}/bin/claude", "text": FAKE_CLAUDE},
                step("pando", "tend", "--redo-clipped", "--max", "1"),
                step("pando", "view"),
                step("pando", "tend", "--redo-clipped"),
                step("pando", "view"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-redo-clipped-nothing",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend"),
                step("pando", "tend", "--redo-clipped"),
                step("pando", "tend", "--redo-clipped", "--dry-run"),
            ],
            [{"fake_claude": True}],
        ),
        case(
            "tend-redo-clipped-still-broken",
            [
                step("pando", "append", "-", stdin=LONG),
                *[step("pando", "append", "-", stdin=CLIPME)] * 7,
                step("pando", "tend"),
                step("pando", "tend", "--redo-clipped"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-redo-clipped-refuse-over-limit",
            [
                step("pando", "append", "-", stdin=LONG),
                *[step("pando", "append", "-", stdin=CLIPME)] * 29,
                step("pando", "tend", "--yes"),
                {"write": "{SCRATCH}/bin/claude", "text": FAKE_CLAUDE},
                step("pando", "tend", "--redo-clipped"),
                step("pando", "tend", "--redo-clipped", "--max", "3"),
                step("pando", "tend", "--redo-clipped", "--yes"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-merge-never-sees-the-clip-marker",
            [
                step("pando", "append", "-", stdin=CLIPME),
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend"),
                step("pando", "view"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_MERGE}],
        ),
        case(
            "tend-redo-level",
            [
                *[step("pando", "append", "-", stdin=MID)] * 4,
                step("pando", "tend"),
                step("pando", "view"),
                step("pando", "tend", "--redo-level", "1", "--dry-run"),
                step("pando", "tend", "--redo-level", "1"),
                step("pando", "view"),
                step("pando", "tend", "--redo-level", "0", "--max", "2"),
                step("pando", "view"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_LAYERS}],
        ),
        # A redo that stops on a failed call goes on where it stopped, and one cut by --max does too.
        case(
            "tend-redo-level-resumes",
            [
                *[step("pando", "append", "-", stdin=MID)] * 4,
                step("pando", "tend"),
                step("pando", "tend", "--redo-level", "0"),
                step("pando", "view"),
                step("pando", "tend", "--redo-level", "0", "--dry-run"),
                step("pando", "tend", "--redo-level", "0"),
                step("pando", "view"),
                step("pando", "tend", "--redo-level", "0", "--dry-run"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_FLAKY}],
        ),
        case(
            "tend-redo-level-max-then-the-rest",
            [
                *[step("pando", "append", "-", stdin=MID)] * 4,
                step("pando", "tend"),
                step("pando", "tend", "--redo-level", "0", "--max", "2"),
                step("pando", "tend", "--redo-level", "0", "--dry-run"),
                step("pando", "tend", "--redo-level", "0"),
                step("pando", "tend", "--redo-level", "0", "--dry-run"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_LAYERS}],
        ),
        case(
            "tend-redo-level-other-set-keeps-the-marker",
            [
                *[step("pando", "append", "-", stdin=MID)] * 4,
                step("pando", "tend"),
                step("pando", "tend", "--redo-level", "0"),
                step("pando", "tend", "--redo-clipped"),
                step("pando", "tend", "--redo-level", "0", "--dry-run"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_FLAKY_FIRST}],
        ),
        case(
            "tend-redo-level-and-clipped",
            [
                *[step("pando", "append", "-", stdin=CLIPME)] * 2,
                step("pando", "append", "-", stdin=MID),
                step("pando", "append", "-", stdin=MID),
                step("pando", "tend"),
                step("pando", "tend", "--redo-level", "1", "--dry-run"),
                step("pando", "tend", "--redo-clipped", "--dry-run"),
                step("pando", "tend", "--redo-clipped", "--redo-level", "1", "--dry-run"),
            ],
            [{"fake_claude_gap": True}],
        ),
        case(
            "tend-redo-level-nothing",
            [
                step("pando", "append", "-", stdin=MID),
                step("pando", "tend"),
                step("pando", "tend", "--redo-level", "5"),
                step("pando", "tend", "--redo-level", "5", "--dry-run"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_LAYERS}],
        ),
        case(
            "tend-redo-level-bad-number",
            [
                step("pando", "tend", "--redo-level", "x"),
                step("pando", "tend", "--redo-level", "-1"),
                step("pando", "tend", "--redo-level"),
            ],
            g,
        ),
        case("tend-redo-level-wrong-verb", [step("pando", "view", "--redo-level", "1")], g),
        case("tend-redo-level-no-store", [step("pando", "tend", "--redo-level", "1")]),
        case(
            "tend-redo-level-refuse-over-limit",
            [
                *[step("pando", "append", "-", stdin=MID)] * 30,
                step("pando", "tend", "--yes"),
                step("pando", "tend", "--redo-level", "0"),
                step("pando", "tend", "--redo-level", "0", "--max", "3"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude_text": FAKE_CLAUDE_LAYERS}],
        ),
        case(
            "tend-redo-level-counts-calls-not-nodes",
            [
                *[step("pando", "append", "w")] * 30,
                step("pando", "tend", "--yes"),
                step("pando", "tend", "--redo-level", "1", "--dry-run"),
                step("pando", "tend", "--redo-level", "1"),
                step("pando", "status", "--json"),
            ],
            [{"fake_claude": True}],
        ),
        case("tend-redo-clipped-no-store", [step("pando", "tend", "--redo-clipped")]),
        case("tend-redo-clipped-wrong-verb", [step("pando", "view", "--redo-clipped")], g),
        case(
            "tend-profile-max-tokens",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "view"),
            ],
            [{"fake_openai": {"mode": "empty-length", "max_tokens": 3000}}],
        ),
        case(
            "tend-profile-default-max-tokens",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "view"),
            ],
            [{"fake_openai": {"mode": "empty-length"}}],
        ),
        case(
            "tend-profile-timeout-s",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "view"),
            ],
            [{"fake_openai": {"mode": "ok", "timeout_s": 300}}],
        ),
        # The environment's own limit is 1 s and the model takes 2 s: the call fails,
        # unless the profile names timeout_s, which the HTTP client then waits for.
        case(
            "tend-profile-slow-model-hits-the-env-limit",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "status", "--json"),
            ],
            [
                {"env": {"OPENDAISUGI_LLM_TIMEOUT": "1"}},
                {"fake_openai": {"mode": "ok", "delay": 2}},
            ],
        ),
        case(
            "tend-profile-timeout-s-outwaits-the-env-limit",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "status", "--json"),
            ],
            [
                {"env": {"OPENDAISUGI_LLM_TIMEOUT": "1"}},
                {"fake_openai": {"mode": "ok", "delay": 2, "timeout_s": 5}},
            ],
        ),
        # thinking on the openai wire is reasoning_effort; off is "none".
        case(
            "tend-profile-thinking-off",
            [step("pando", "append", "-", stdin=LONG), step("pando", "tend", "--profile", "local")],
            [{"fake_openai": {"mode": "ok", "thinking": "off"}}],
        ),
        case(
            "tend-profile-thinking-high",
            [step("pando", "append", "-", stdin=LONG), step("pando", "tend", "--profile", "local")],
            [{"fake_openai": {"mode": "ok", "thinking": "high"}}],
        ),
        case(
            "tend-profile-bad-thinking",
            [step("pando", "append", "-", stdin=LONG), step("pando", "tend", "--profile", "local")],
            [
                {
                    "file": "{DATA}/config.yaml",
                    "text": "agent_profiles:\n  local: {wire: openai, thinking: loud}\n",
                }
            ],
        ),
        case(
            "tend-profile-bad-timeout-s",
            [step("pando", "append", "-", stdin=LONG), step("pando", "tend", "--profile", "local")],
            [
                {
                    "file": "{DATA}/config.yaml",
                    "text": "agent_profiles:\n  local: {wire: openai, timeout_s: 0}\n",
                }
            ],
        ),
        case(
            "tend-profile-with-text",
            [
                step("pando", "append", "-", stdin=LONG),
                step("pando", "tend", "--profile", "local"),
                step("pando", "view"),
            ],
            [{"fake_openai": {"mode": "ok", "max_tokens": 2048}}],
        ),
        case(
            "import-tend-clipped",
            [step("pando", "import", "--from", "codex", "--tend"), step("pando", "status")],
            [
                {"codex": [SESSION_CODEX[0], SESSION_CODEX[1].replace("write a test", CLIPME)]},
                {"fake_claude_gap": True},
            ],
        ),
        case(
            "migrate",
            [step("pando", "migrate"), step("pando", "migrate"), step("pando", "status")],
            [{"golden_to": "foreman"}, {"file": "{DATA}/foreman/turns.jsonl", "text": "{}\n"}],
        ),
        case("migrate-json", [step("pando", "migrate", "--json")], [{"golden_to": "foreman"}]),
        case("migrate-nothing", [step("pando", "migrate")]),
        case(
            "migrate-target-exists",
            [step("pando", "migrate")],
            [{"golden_to": "foreman"}, golden()],
        ),
        case(
            "migrate-from",
            [step("pando", "migrate", "--from", "{DATA}/old-steward", "--store", "home")],
            [{"golden_to": "old-steward"}],
        ),
        case(
            "import-dry-run",
            [step("pando", "import", "--dry-run")],
            [{"codex": SESSION_CODEX}, {"claude": SESSION_CLAUDE}],
        ),
        case(
            "import-local",
            [
                step("pando", "import", "--from", "codex"),
                step("pando", "log"),
                step("pando", "import", "--from", "codex"),
            ],
            [{"codex": SESSION_CODEX}, {"claude": SESSION_CLAUDE}],
        ),
        case(
            "import-tend",
            [step("pando", "import", "--from", "codex", "--tend"), step("pando", "status")],
            [{"codex": SESSION_CODEX}, {"fake_claude": True}],
        ),
        case(
            "import-tend-dry-run",
            [step("pando", "import", "--from", "codex", "--dry-run", "--tend")],
            [{"codex": SESSION_CODEX}],
        ),
        case(
            "import-tend-bad-profile",
            [
                step("pando", "import", "--from", "codex", "--tend", "--profile", "nobody"),
                step("pando", "status"),
            ],
            [{"codex": SESSION_CODEX}] + g,
        ),
        case(
            "import-dir-dry-run",
            [step("pando", "import", "--dir", "{SCRATCH}/chats", "--dry-run")],
            CHATS_DIR + [{"claude": SESSION_CLAUDE}],
        ),
        case(
            "import-dir",
            [
                step("pando", "import", "--dir", "{SCRATCH}/chats"),
                step("pando", "log"),
                step("pando", "import", "--dir", "{SCRATCH}/chats"),
            ],
            CHATS_DIR + [{"claude": SESSION_CLAUDE}],
        ),
        case(
            "import-dir-from-claude",
            [step("pando", "import", "--from", "claude", "--dir", "{SCRATCH}/chats", "--dry-run")],
            CHATS_DIR,
        ),
        case(
            "import-dir-flat",
            [step("pando", "import", "--dir", "{SCRATCH}/flat")],
            [
                {
                    "file": "{SCRATCH}/flat/s9.jsonl",
                    "text": SESSION_CLAUDE[0].replace('"s1"', '"s9"').replace('"u1"', '"u9"')
                    + "\n",
                }
            ],
        ),
        case("import-dir-missing", [step("pando", "import", "--dir", "{SCRATCH}/nowhere")]),
        case(
            "import-dir-wrong-from",
            [step("pando", "import", "--dir", "{SCRATCH}/chats", "--from", "codex")],
            CHATS_DIR,
        ),
        case(
            "import-dir-with-web",
            [step("pando", "import", "--dir", "{SCRATCH}/chats", "--from", "web", "--web", web)],
            CHATS_DIR,
        ),
        case(
            "import-dir-tend-refuse",
            [
                step("pando", "import", "--dir", "{SCRATCH}/big", "--tend"),
                step("pando", "status", "--json"),
            ],
            [{"file": "{SCRATCH}/big/-p/s1.jsonl", "text": BIG_SESSION}],
        ),
        case(
            "import-dir-tend-yes",
            [
                step("pando", "import", "--dir", "{SCRATCH}/big", "--tend", "--yes"),
                step("pando", "status", "--json"),
            ],
            [{"file": "{SCRATCH}/big/-p/s1.jsonl", "text": BIG_SESSION}, {"fake_claude": True}],
        ),
        case(
            "import-map",
            [
                step("pando", "import", "--from", "claude", "--map", "/old/proj=/new/proj"),
                step("pando", "log"),
            ],
            [{"claude": SESSION_CLAUDE}],
        ),
        case(
            "import-since",
            [step("pando", "import", "--dry-run", "--since", "2026-03-03")],
            [{"codex": SESSION_CODEX}, {"claude": SESSION_CLAUDE}],
        ),
        case("import-bad-from", [step("pando", "import", "--from", "nope")]),
        case("import-bad-map", [step("pando", "import", "--from", "claude", "--map", "nope")]),
        case("import-bad-since", [step("pando", "import", "--since", "soon")]),
        case("import-web-needs-path", [step("pando", "import", "--from", "web")]),
        case("import-web-count", [step("pando", "import", "--from", "web", "--web", web)]),
        case(
            "import-web-propose",
            [
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--propose",
                    "{SCRATCH}/prop.tsv",
                )
            ],
        ),
        case(
            "import-web-accept-work",
            [
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--propose",
                    "{SCRATCH}/prop.tsv",
                ),
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--accept",
                    "{SCRATCH}/prop.tsv",
                ),
                step("pando", "log"),
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--propose",
                    "{SCRATCH}/prop2.tsv",
                ),
            ],
        ),
        case(
            "import-web-accept-home",
            [
                {
                    "argv": [
                        "pando",
                        "import",
                        "--from",
                        "web",
                        "--web",
                        web,
                        "--propose",
                        "{SCRATCH}/prop.tsv",
                    ],
                    "stdin": "",
                },
                {"edit": "{SCRATCH}/prop.tsv", "sub": ["\twork\t", "\thome\t"]},
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--accept",
                    "{SCRATCH}/prop.tsv",
                ),
                step("pando", "stores"),
                step("pando", "log", "--store", "home"),
            ],
        ),
        case(
            "import-web-accept-all-refused",
            [
                {"write": "{SCRATCH}/prop.tsv", "text": "web:c1\tall\tr\tt\n"},
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--accept",
                    "{SCRATCH}/prop.tsv",
                ),
            ],
        ),
        case(
            "import-web-propose-and-accept-both",
            [
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--propose",
                    "a",
                    "--accept",
                    "b",
                )
            ],
        ),
        case(
            "import-web-accept-unknown-chat",
            [
                {"write": "{SCRATCH}/prop.tsv", "text": "web:nope\twork\tr\tt\n"},
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--accept",
                    "{SCRATCH}/prop.tsv",
                ),
            ],
        ),
        case(
            "import-web-accept-skip",
            [
                {"write": "{SCRATCH}/prop.tsv", "text": "# a comment\nweb:nope\tskip\tr\tt\n"},
                step(
                    "pando",
                    "import",
                    "--from",
                    "web",
                    "--web",
                    web,
                    "--accept",
                    "{SCRATCH}/prop.tsv",
                ),
            ],
        ),
        case(
            "import-web-missing-file",
            [step("pando", "import", "--from", "web", "--web", "{SCRATCH}/none.json")],
        ),
        case(
            "data-dir-flag",
            [step("pando", "status", "--data-dir", "{SCRATCH}/other")],
            [{"golden_at": "{SCRATCH}/other/pando/work"}],
        ),
        # -- round 8b: branches --
        case(
            "branch-hooks-log-into-the-branch",
            [
                *BRANCH_STEPS,
                step("pando", "log"),
                step("pando", "log", "--branch", "trellis"),
                step("pando", "log", "--branch", "trellis/ui"),
                step("pando", "branches"),
                step("pando", "branches", "--json"),
                step("pando", "status"),
                step("pando", "hook", stdin=hook_ev("SessionStart", "lead1"), env=TRELLIS),
                step("pando", "context"),
                step("pando", "context", env=TRELLIS),
                step("pando", "context", env=UI_ENV),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-recall-from-above-and-below",
            [
                *BRANCH_STEPS,
                step("pando", "recall", "budget epub lattice reader"),
                step("pando", "recall", "budget epub lattice reader", "--json"),
                step("pando", "recall", "budget epub lattice reader", env=TRELLIS),
                step("pando", "recall", "budget epub lattice reader", env=UI_ENV),
                step("pando", "zoom", "0", "1", "--branch", "trellis/ui"),
                step("pando", "zoom", "0", "1", "--branch", "ui", env=TRELLIS),
                step("pando", "date", "0", "--branch", "trellis"),
                step("pando", "view", "--branch", "trellis"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-serve-scope",
            [
                *BRANCH_STEPS,
                step(
                    "pando",
                    "serve",
                    env=TRELLIS,
                    stdin="\n".join(
                        [
                            '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
                            mcp_call("recall", {"query": "household budget attic"}),
                            mcp_call("recall", {"query": "epub lattice"}),
                            mcp_call("zoom", {"id": 0, "n": 1, "branch": "ui"}),
                            mcp_call("zoom", {"id": 0, "n": 1, "branch": ".."}),
                            mcp_call("zoom", {"id": 0, "n": 1, "branch": "../trellis"}),
                            mcp_call("zoom", {"id": 0, "n": 1, "branch": "/etc"}),
                            mcp_call("zoom", {"id": 0, "n": 1, "child": "../../x"}),
                            mcp_call("date", {"id": 0, "branch": "ui"}),
                            mcp_call("view", {}),
                        ]
                    )
                    + "\n",
                ),
                step(
                    "pando",
                    "serve",
                    env={**TRELLIS, "DAISUGI_PANDO_SKIP": "1"},
                    stdin="\n".join(
                        [
                            '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
                            mcp_call("recall", {"query": "epub"}),
                            mcp_call("view", {}),
                        ]
                    )
                    + "\n",
                ),
                step(
                    "pando",
                    "serve",
                    stdin="\n".join(
                        [
                            '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
                            mcp_call("recall", {"query": "epub lattice budget"}),
                            mcp_call("zoom", {"id": 0, "n": 1, "branch": "trellis"}),
                        ]
                    )
                    + "\n",
                ),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-bad-markers-are-closed",
            [
                *BRANCH_STEPS,
                *[
                    s
                    for bad in ["..", "/tmp", "nope", "trellis/../trellis", "trellis//ui"]
                    for s in [
                        step(
                            "pando",
                            "serve",
                            env={"DAISUGI_PANDO_BRANCH": bad},
                            stdin='{"jsonrpc":"2.0","id":2,"method":"tools/list"}\n'
                            + mcp_call("recall", {"query": "budget"})
                            + "\n",
                        ),
                        step("pando", "context", env={"DAISUGI_PANDO_BRANCH": bad}),
                        step("pando", "recall", "budget", env={"DAISUGI_PANDO_BRANCH": bad}),
                    ]
                ],
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-process-writes-nothing-by-hand",
            [
                *BRANCH_STEPS,
                step("pando", "append", "x", env=TRELLIS),
                step("pando", "stores", env=TRELLIS),
                step("pando", "tend", env=TRELLIS),
                step("pando", "close", "trellis", env=TRELLIS),
                step("pando", "import", env=TRELLIS),
                step("pando", "stores", env={"DAISUGI_PANDO_SKIP": "1"}),
                step("pando", "view", env={"DAISUGI_PANDO_SKIP": "1"}),
                step("pando", "context", env={**TRELLIS, "DAISUGI_PANDO_SKIP": "1"}),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-close-and-reopen",
            [
                *BRANCH_STEPS,
                step("pando", "close", "trellis/ui"),
                step("pando", "close", "trellis/ui"),
                step("pando", "close", "nope"),
                step("pando", "close", "../x"),
                step("pando", "branches"),
                step("pando", "context", env=TRELLIS),
                step("pando", "recall", "lattice"),
                step("pando", "hook", stdin=hook_ev("Stop", "ui1"), env=UI_ENV),
                step("pando", "branches"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-owner-hook-routes-a-mapped-session",
            [
                *BRANCH_STEPS,
                {"write": "{SCRATCH}/proj/lead1.jsonl", "text": LEAD_MORE},
                step("pando", "hook", stdin=hook_ev("Stop", "lead1")),
                step("pando", "log"),
                step("pando", "log", "--branch", "trellis"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-context-maps-a-hermes-session",
            [
                *BRANCH_STEPS,
                step("pando", "context", "--session", "h-77", env=TRELLIS),
                step(
                    "pando",
                    "context",
                    "--session",
                    "h-78",
                    env={**TRELLIS, "DAISUGI_PANDO_SKIP": "1"},
                ),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-import-skips-a-mapped-session",
            [
                *BRANCH_STEPS,
                step("pando", "import", "--dir", "{SCRATCH}/proj"),
                step("pando", "log"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-tend-dry-run",
            [
                step("pando", "append", "-", stdin=LONG),
                *BRANCH_STEPS[1:],
                step("pando", "tend", "--dry-run"),
            ],
            [
                {
                    "file": "{SCRATCH}/proj/lead1.jsonl",
                    "text": "\n".join([lead_line("lead1", 0, "user", LONG.strip())]) + "\n",
                },
                {"file": "{SCRATCH}/proj/ui1.jsonl", "text": UI},
            ],
        ),
        case(
            "branch-bad-flag",
            [
                step("pando", "zoom", "0", "1", "--branch", "../x"),
                step("pando", "zoom", "0", "1", "--branch", "nope"),
            ],
            g,
        ),
        # -- review fix 1 --
        case(
            "branch-forged-start-cannot-claim-the-owner",
            [
                {
                    "write": "{SCRATCH}/proj/owner1.jsonl",
                    "text": "\n".join(
                        [lead_line("owner1", 0, "user", "owner first words at the root")]
                    )
                    + "\n",
                },
                step("pando", "hook", stdin=hook_ev("Stop", "owner1")),
                *BRANCH_STEPS[1:],
                step("pando", "hook", stdin=hook_ev("SessionStart", "owner1"), env=TRELLIS),
                step("pando", "context", "--session", "x1", env=TRELLIS),
                step("pando", "routes"),
                step("pando", "routes", "--json"),
                {"write": "{SCRATCH}/data/pando/work/branch.sessions", "text": "owner1\ttrellis\n"},
                {
                    "write": "{SCRATCH}/proj/owner1.jsonl",
                    "text": "\n".join(
                        [
                            lead_line("owner1", 0, "user", "owner first words at the root"),
                            lead_line("owner1", 1, "user", "owner second words"),
                        ]
                    )
                    + "\n",
                },
                step("pando", "hook", stdin=hook_ev("Stop", "owner1")),
                step("pando", "log"),
                step("pando", "log", "--branch", "trellis"),
                step("pando", "unroute", "owner1", env=TRELLIS),
                step("pando", "unroute", "owner1"),
                step("pando", "unroute", "owner1"),
                step("pando", "routes"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-worker-refusal-and-view-lines",
            [
                *BRANCH_STEPS,
                step("pando", "stores", env={"DAISUGI_PANDO_SKIP": "1"}),
                step("pando", "routes", env={"DAISUGI_PANDO_SKIP": "1"}),
                step("pando", "view"),
                step("pando", "view", "--branch", "trellis"),
                step("pando", "close", "trellis"),
                step("pando", "hook", stdin=hook_ev("Stop", "lead1"), env=TRELLIS),
                step("pando", "branches"),
                step("pando", "view"),
            ],
            BRANCH_SETUP,
        ),
        case(
            "branch-owner-start-claims-its-session",
            [
                {
                    "write": "{SCRATCH}/proj/owner1.jsonl",
                    "text": lead_line("owner1", 0, "user", "a new owner chat") + "\n",
                },
                step("pando", "hook", stdin=hook_ev("SessionStart", "owner1")),
                step("pando", "hook", stdin=hook_ev("SessionStart", "owner1"), env=TRELLIS),
                step("pando", "routes"),
                step("pando", "hook", stdin=hook_ev("Stop", "owner1")),
                step("pando", "log"),
                step("pando", "branches"),
            ],
            BRANCH_SETUP,
        ),
        # H2: a branch line for an owner chat the store never logged loses to
        # the owner's unmarked SessionStart.
        case(
            "branch-owner-claim-wins-over-an-earlier-branch-line",
            [
                {
                    "write": "{SCRATCH}/proj/old1.jsonl",
                    "text": lead_line("old1", 0, "user", "OWNER OLD CHAT") + "\n",
                },
                *BRANCH_STEPS[1:],
                step("pando", "hook", stdin=hook_ev("SessionStart", "old1"), env=TRELLIS),
                step("pando", "routes"),
                step("pando", "hook", stdin=hook_ev("SessionStart", "old1")),
                step("pando", "routes"),
                step("pando", "hook", stdin=hook_ev("Stop", "old1")),
                step("pando", "log"),
                step("pando", "log", "--branch", "trellis"),
            ],
            BRANCH_SETUP,
            go_only=True,
        ),
        case(
            "routes-none",
            [
                step("pando", "routes"),
                step("pando", "routes", "--json"),
                step("pando", "unroute", "x"),
            ],
            g,
        ),
    ]
    return cases


DATE_RE = re.compile(r"(\d{4}-\d{2}-\d{2})(T\d{2}:\d{2}:\d{2}Z)?")
STAMP_RE = re.compile(r"(\d{8})T(\d{6})Z")


def norm(text: str, scratch: Path) -> str:
    text = text.replace(str(scratch), "{SCRATCH}")
    today = time.strftime("%Y-%m-%d", time.gmtime())
    today_c = today.replace("-", "")

    def day(m: re.Match[str]) -> str:
        if m.group(1) != today:
            return m.group(0)
        return "{TODAY}" + ("T{TIME}Z" if m.group(2) else "")

    text = DATE_RE.sub(day, text)
    text = STAMP_RE.sub(lambda m: "{STAMP}" if m.group(1) == today_c else m.group(0), text)
    # Names the run picks: the temp dirs of the fake claude and the OS.
    text = re.sub(r"/tmp/pando-tend-[A-Za-z0-9]+", "{TMP}", text)
    text = re.sub(r"127\.0\.0\.1:\d+", "127.0.0.1:{PORT}", text)
    # The seconds a node took, in tend's progress line.
    text = re.sub(r"(built level \d+ index \d+ in )[0-9.]+s", r"\1{SEC}s", text)
    return text


def tree_of(root: Path, scratch: Path) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for p in sorted(root.rglob("*")):
        rel = norm(str(p.relative_to(root)), scratch)
        if p.name in {"lock", "import.lock"} or "/bin" in rel or rel.startswith("bin"):
            continue
        if p.is_dir():
            out[rel + "/"] = oct(p.stat().st_mode & 0o777)
        else:
            try:
                body = norm(p.read_text(encoding="utf-8"), scratch)
            except UnicodeDecodeError:
                body = "<binary>"
            out[rel] = {"mode": oct(p.stat().st_mode & 0o777), "text": body}
    return out


def fake_openai(data: Path, mode: str, servers: list[Any], delay: float = 0.0) -> int:
    """An openai-wire server on a free local port. Each request adds a line
    to <data>/requests.jsonl with the max_tokens it asked for. Mode
    "empty-length" answers with no text, a reasoning field and a finish
    reason of length, as a thinking model out of tokens does; mode "ok"
    answers with a line. A delay holds the reply back that many seconds. Returns the port."""

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802 - http.server's name
            body = json.loads(
                self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}"
            )
            with (data / "requests.jsonl").open("a", encoding="utf-8") as f:
                seen = {"max_tokens": body.get("max_tokens")}
                for k in ("reasoning_effort", "thinking"):
                    if k in body:
                        seen[k] = body[k]
                f.write(json.dumps(seen, sort_keys=True) + "\n")
            if mode == "empty-length":
                msg = {"content": "", "reasoning_content": "SECRET REASONING"}
                reply = {"choices": [{"message": msg, "finish_reason": "length"}]}
            else:
                reply = {
                    "choices": [
                        {"message": {"content": "a short summary line"}, "finish_reason": "stop"}
                    ]
                }
            out = json.dumps(reply).encode()
            if delay:
                time.sleep(delay)
            try:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(out)))
                self.end_headers()
                self.wfile.write(out)
            except OSError:
                pass  # the client gave up first

        def log_message(self, *a: Any) -> None:
            pass

    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    servers.append(srv)
    return srv.server_address[1]


def lay_out(
    c: dict[str, Any], scratch: Path, web: Path, servers: list[Any] | None = None
) -> dict[str, str]:
    data = scratch / "data"
    home = scratch / "home"
    binp = scratch / "bin"
    for d in (data, home, binp):
        d.mkdir(parents=True, exist_ok=True)
    sub = {"{DATA}": str(data), "{SCRATCH}": str(scratch), "{WEB}": str(web), "{HOME}": str(home)}

    def fill(s: str) -> str:
        for k, v in sub.items():
            s = s.replace(k, v)
        return s

    path = "/usr/bin:/bin"
    extra_env: dict[str, str] = {}
    for op in c["setup"]:
        if "golden" in op:
            shutil.copytree(
                FIXTURES / "golden" / "out",
                data / "pando" / op["golden"],
                ignore=shutil.ignore_patterns("probes.json", "views.jsonl", "view.txt"),
            )
        elif "golden_to" in op:
            shutil.copytree(
                FIXTURES / "golden" / "out",
                data / op["golden_to"],
                ignore=shutil.ignore_patterns("probes.json", "views.jsonl", "view.txt"),
            )
        elif "golden_at" in op:
            shutil.copytree(
                FIXTURES / "golden" / "out",
                Path(fill(op["golden_at"])),
                ignore=shutil.ignore_patterns("probes.json", "views.jsonl", "view.txt"),
            )
        elif "file" in op:
            p = Path(fill(op["file"]))
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(fill(op["text"]), encoding="utf-8")
        elif "fake_claude" in op:
            p = binp / "claude"
            p.write_text(FAKE_CLAUDE, encoding="utf-8")
            p.chmod(0o755)
            path = f"{binp}:/usr/bin:/bin"
        elif "fake_claude_gap" in op:
            p = binp / "claude"
            p.write_text(FAKE_CLAUDE_GAP, encoding="utf-8")
            p.chmod(0o755)
            path = f"{binp}:/usr/bin:/bin"
        elif "env" in op:
            extra_env.update(op["env"])
        elif "fake_claude_text" in op:
            p = binp / "claude"
            p.write_text(op["fake_claude_text"], encoding="utf-8")
            p.chmod(0o755)
            path = f"{binp}:/usr/bin:/bin"
        elif "fake_openai" in op:
            spec = op["fake_openai"]
            port = fake_openai(
                data, spec["mode"], servers if servers is not None else [], spec.get("delay", 0.0)
            )
            tokens = f", max_tokens: {spec['max_tokens']}" if spec.get("max_tokens") else ""
            tokens += f", timeout_s: {spec['timeout_s']}" if spec.get("timeout_s") else ""
            tokens += f", thinking: {spec['thinking']}" if spec.get("thinking") else ""
            (data / "config.yaml").write_text(
                f"agent_profiles:\n  local: {{wire: openai, base_url: 'http://127.0.0.1:{port}/v1', model: openai/m{tokens}}}\n",
                encoding="utf-8",
            )
        elif "codex" in op:
            p = home / ".codex/sessions/2026/03/05/rollout-2026-03-05T08-00-00-c1.jsonl"
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("\n".join(op["codex"]) + "\n", encoding="utf-8")
        elif "claude" in op:
            p = home / ".claude/projects/-old-proj-app/s1.jsonl"
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("\n".join(op["claude"]) + "\n", encoding="utf-8")
    return {
        "HOME": str(home),
        "OPENDAISUGI_HOME": str(data),
        "PATH": path,
        "XDG_DATA_HOME": str(scratch / "xdg"),
        **extra_env,
    }, fill


def run_case(
    binary: str, c: dict[str, Any], keep: bool = False, after: Any = None
) -> dict[str, Any]:
    SCRATCH_ROOT.mkdir(parents=True, exist_ok=True)
    scratch = Path(tempfile.mkdtemp(prefix="c-", dir=SCRATCH_ROOT))
    web = FIXTURES / "transcripts" / "web" / "conversations.json"
    servers: list[Any] = []
    try:
        env, fill = lay_out(c, scratch, web, servers)
        steps = []
        for s in c["steps"]:
            if "edit" in s:
                p = Path(fill(s["edit"]))
                if not p.exists():
                    continue
                p.write_text(p.read_text(encoding="utf-8").replace(*s["sub"]), encoding="utf-8")
                continue
            if "write" in s:
                Path(fill(s["write"])).write_text(fill(s["text"]), encoding="utf-8")
                continue
            step_env = {**env, **s.get("env", {})}
            r = subprocess.run(
                [str(Path(binary).resolve()), *[fill(a) for a in s["argv"]]],
                input=fill(s["stdin"]).encode(),
                capture_output=True,
                env=step_env,
                timeout=120,
                check=False,
                cwd=scratch,
            )
            steps.append(
                {
                    "code": r.returncode,
                    "out": norm(r.stdout.decode("utf-8", "replace"), scratch),
                    "err": norm(r.stderr.decode("utf-8", "replace"), scratch),
                }
            )
        out = {
            "steps": steps,
            "tree": {k: v for k, v in tree_of(scratch / "data", scratch).items()},
        }
        if after is not None:
            # A check on the live scratch tree (the Python reader): its result rides along.
            out["after"] = after(scratch / "data")
        return out
    finally:
        for srv in servers:
            srv.shutdown()
            srv.server_close()
        if not keep:
            shutil.rmtree(scratch, ignore_errors=True)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--binary", required=True, help="the Go daisugi")
    ap.add_argument("--out", default=str(FIXTURE_DIR))
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    lines = []
    for c in build_cases():
        c = dict(c)
        c["expect"] = run_case(args.binary, c)
        lines.append(json.dumps(c, sort_keys=True, ensure_ascii=True))
    (out / "cases.jsonl").write_text("\n".join(lines) + "\n", encoding="utf-8")
    print(f"wrote {len(lines)} cases to {out / 'cases.jsonl'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

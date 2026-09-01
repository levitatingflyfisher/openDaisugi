"""Write argparse's answers for the gate parser's differential test.

    PYTHONPATH=src python argv_oracle.py SEED N out.jsonl

Each line is {"argv": [...], "columns": n, "ns": {...}} for a parse, or
{"argv", "columns", "exit": code, "stdout", "stderr"} for an exit. The argv
lists are random mixes of the gate's flags, prefixes of them, values and
junk.
"""

import contextlib
import io
import json
import os
import random
import sys

os.environ["HOME"] = "/home/user"  # the default --root the test expects
from opendaisugi.gate import _build_parser  # noqa: E402

WORDS = ["--mode", "--mo", "--m", "--mode=enforce", "--mo=shadow", "--root", "--r", "--format", "--f", "--for=x",
         "--verify-timeout", "--v", "--verify", "--captures-root", "--c", "--ca", "--ch", "--session", "--s",
         "--ask", "--as", "--ask-", "--ask-timeout", "--ask=1", "--checkpoints", "--check", "--checkpoints=x",
         "-h", "--help", "--he", "-hx", "-hh", "-h=1", "-x", "-", "--", "---", "--x", "--nope", "enforce",
         "shadow", "x", "", " ", "a b", "-1", "-1.5", "-.5", "1e3", "inf", "nan", "-inf", " 2 ", "1_0", "1__0",
         "0x10", "10.", ".5", "_1", "claude", "/r", "--root=/r", "--session=", "--format=", "é", "-é",
         "--mode=bogus", "bogus", "--verify-timeout=x", "--ask-timeout=-1", "-m", "-hm", "-h-"]


def main():
    seed, n, out = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
    rng = random.Random(seed)
    with open(out, "w", encoding="utf-8") as f:
        for _ in range(n):
            argv = [rng.choice(WORDS) for _ in range(rng.randint(0, 6))]
            if rng.random() < 0.3:
                argv.insert(rng.randint(0, len(argv)), rng.choice(["--help", "-h", "--he"]))
            cols = str(rng.choice([80, 80, 80, rng.randint(1, 200), rng.randint(1, 200), 0, -5, "x", " 90 ", "1_20"]))
            os.environ["COLUMNS"] = cols
            row = {"argv": argv, "columns": cols}
            so, se = io.StringIO(), io.StringIO()
            try:
                with contextlib.redirect_stdout(so), contextlib.redirect_stderr(se):
                    ns = _build_parser().parse_args(argv)
                row["ns"] = {"mode": ns.mode, "root": str(ns.root), "fmt": ns.fmt,
                             "verify_timeout": repr(ns.verify_timeout),
                             "captures_root": None if ns.captures_root is None else str(ns.captures_root),
                             "session": ns.session, "ask": ns.ask, "ask_timeout": repr(ns.ask_timeout),
                             "checkpoints": ns.checkpoints}
            except SystemExit as exc:
                row.update(exit=exc.code, stdout=so.getvalue(), stderr=se.getvalue())
            f.write(json.dumps(row) + "\n")


main()

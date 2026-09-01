"""Write Python's answers for the resident server's request helpers.

    PYTHONPATH=src python resident_oracle.py > resident_vectors.json

b64decode: [text, bytes as hex, ok] for base64.b64decode(text), the
non-strict decode gate_server.py runs on stdin_b64. hook_report_argv:
[argv, pane, root, ok] for the `daisugi hook report` parser
(_state_report._build_hook_report_parser, with the default root left out).
"""

import argparse
import base64
import binascii
import contextlib
import io
import json
import random

random.seed(7)
texts = [
    "",
    "YQ",
    "YQ=",
    "YQ==",
    "YQ===",
    "Y=Q==",
    "e30=",
    "e30=!!",
    "e3 0=",
    "=e30=",
    "e30===",
    "e=30",
    "ab=c=d",
    "ab==cd==",
    "abc",
    "abcde",
    "a",
    "====",
    "Zm9vYmFy",
    "Zm9v\nYmFy",
    "Zm9vYg",
    "Zm9vYg=",
    "Zm9vYg==x",
    "Zm9vYmE=",
    "Zm9vYmE=Zg==",
]
for _ in range(300):
    texts.append("".join(random.choice("ABCDab01+/==-_ \n!") for _ in range(random.randint(0, 12))))
b64 = []
for t in texts:
    try:
        b64.append([t, base64.b64decode(t).hex(), True])
    except (binascii.Error, ValueError):
        b64.append([t, "", False])

p = argparse.ArgumentParser(prog="x", add_help=False)
p.add_argument("--pane", default=None)
p.add_argument("--root", default=None)
argvs = [
    [],
    ["--pane", "w1:p2"],
    ["--pane=w1"],
    ["--pa", "x"],
    ["--p", "x"],
    ["--ro", "r"],
    ["--r=r"],
    ["--", "--pane"],
    ["--pane", "--", "x"],
    ["--pane", "x", "--"],
    ["--pane", "-1"],
    ["--pane", "-x"],
    ["--pane", "-a b"],
    ["--pane"],
    ["-h"],
    ["x"],
    ["--pane", "a", "--pane", "b"],
    ["--=x"],
    ["--x"],
    ["-"],
    ["--pane", "-"],
    [""],
    ["--pane", ""],
    ["--pane="],
    ["--pane", "--root", "r"],
    ["--root", "a", "--pane", "b"],
    ["--pane", "x", "y"],
    ["-p", "x"],
    ["--pane", "-.5"],
    ["--pane", "-1.5e3"],
    ["--pane", "--", "--"],
    ["--pan", "x", "--roo=y"],
    ["--"],
    ["--pane=--"],
    ["--pane", "--x"],
    ["--pane", "a b"],
    ["--pane", "--a b"],
    ["--pane", "-a=b"],
    ["--pane", "-1x"],
    ["--pane", "--pa"],
    ["--pane=a=b"],
    ["--p=", "--r="],
]
argp = []
for a in argvs:
    try:
        with contextlib.redirect_stderr(io.StringIO()):
            ns = p.parse_args(a)
        argp.append([a, ns.pane, ns.root, True])
    except SystemExit:
        argp.append([a, None, None, False])
print(json.dumps({"b64decode": b64, "hook_report_argv": argp}, indent=0))

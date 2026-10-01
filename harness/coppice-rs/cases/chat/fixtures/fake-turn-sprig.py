#!/usr/bin/env python3
"""A stand-in for sprig in a chat-mode foreman turn on an operator profile.
It never calls a model.

coppice starts sprig with an allowlist environment, so this fake finds its
record dir by HOME: it writes $HOME/sprig/turn-N.json with the argv (the
system file as SYSTEM_FILE), the names of the variables it got, whether a
key came on descriptor 3 and how long it was (never the key), and the stdin
with each date written as DATE. A message "status" calls the status tool
through the MCP relay that --mcp-config names. The answer is
{"answer": "sprig heard: MESSAGE", "usage": {...}}.
"""

import json
import os
import re
import subprocess
import sys

DATE = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ")
# The owner-block nonce of a turn is 24 random hex digits (FM-R-109).
NONCE = re.compile(r"\b[0-9a-f]{24}\b")


def main():
    args = sys.argv[1:]
    stdin = sys.stdin.read()
    key = ""
    if "--key-fd" in args:
        with os.fdopen(int(args[args.index("--key-fd") + 1]), encoding="utf-8") as f:
            key = f.read()
    sys_file = args[args.index("--system-file") + 1]
    rec_dir = os.path.join(os.environ["HOME"], "sprig")
    os.makedirs(rec_dir, exist_ok=True)
    n = len(os.listdir(rec_dir))
    rec = {
        "argv": ["SYSTEM_FILE" if a == sys_file else a for a in args],
        "env": sorted(os.environ),
        "key_bytes": len(key),
        "key_in_argv": bool(key) and any(key in a for a in args),
        "key_in_env": bool(key) and any(key in v for v in os.environ.values()),
        "stdin": NONCE.sub("NONCE", DATE.sub("DATE", stdin)),
    }
    with open(os.path.join(rec_dir, f"turn-{n}.json"), "w", encoding="utf-8") as f:
        json.dump(rec, f, indent=1)
    # A key that starts sk-out is an account out of quota (HTTP 429); one that
    # starts sk-401 is a refused key. Both fail as sprig does: one line on
    # stderr and exit 1 (foreman failover cases).
    for prefix, line in (
        ("sk-out", "anthropic api 429: slow down"),
        ("sk-401", "anthropic api 401: invalid x-api-key"),
    ):
        if key.startswith(prefix):
            sys.stderr.write(line + "\n")
            sys.exit(1)
    # The compactor runs sprig with no tools. A text with REFUSE-ME gets an
    # empty answer and the stop reason refusal; a text with BALK-ME gets a
    # non-zero exit with a complaint that is no outage (foreman clip cases).
    tools_none = "--tools" in args and args[args.index("--tools") + 1] == "none"
    if tools_none and "REFUSE-ME" in stdin:
        print(json.dumps({"answer": "", "stop": "refusal", "usage": {"input_tokens": 3, "output_tokens": 0}}))
        return
    if tools_none and "BALK-ME" in stdin:
        sys.stderr.write("I can't summarize this.\n")
        sys.exit(1)
    m = re.search(r"<message>\n(.*)\n</message>\n", stdin, re.S)
    msg = m.group(1) if m else ""
    answer = "sprig heard: " + msg
    if msg == "status":
        mcp = json.loads(args[args.index("--mcp-config") + 1])["mcpServers"]["foreman"]
        lines = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "status", "arguments": {}},
            },
        ]
        p = subprocess.run(
            [mcp["command"], *mcp["args"]],
            input="".join(json.dumps(x) + "\n" for x in lines).encode(),
            capture_output=True,
            timeout=60,
        )
        out = [json.loads(x) for x in p.stdout.decode().splitlines() if x.strip()]
        answer = "sprig status: " + out[1]["result"]["content"][0]["text"]
    usage = {
        "input_tokens": 7,
        "output_tokens": 2,
        "cache_read_input_tokens": 0,
        "cache_creation_input_tokens": 0,
    }
    print(json.dumps({"answer": answer, "usage": usage}))


main()

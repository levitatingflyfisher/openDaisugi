#!/usr/bin/env python3
"""A stand-in for `claude -p --output-format json`: one turn of the chat-mode
foreman. It never calls a model.

It records what the turn gave it in $FAKE_TURN_DIR/turn-N.json: the argv
(the system prompt file as SYSTEM_FILE), the system prompt and the stdin,
with each date written as DATE, so the Go and the Rust coppice give the same
bytes. Then it acts on the owner's message, the text in <message>:

- "zoom ID N": calls the zoom tool through the MCP relay that --mcp-config
  names, and answers with the result;
- "date ID", "status": the same for those tools;
- "spawn PROJECT BRIEF": calls spawn;
- "label LABEL": calls zoom with that label;
- "hook TOOL": runs the hook that --settings names for TOOL and says its exit
  code and its message;
  a tool's result has each date written as DATE;
- "fail": writes two lines on stderr and exits 1;
- "error": answers a result with is_error true;
- anything else: answers "heard: " and the message.

The answer carries fixed token counts.
"""

import json
import os
import re
import shlex
import subprocess
import sys

DATE = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ")


def opt(args, name):
    return args[args.index(name) + 1]


def relay(cmd, name, arguments):
    lines = [
        {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
        {"jsonrpc": "2.0", "method": "notifications/initialized"},
        {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
        {
            "jsonrpc": "2.0",
            "id": 3,
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        },
    ]
    text = "".join(json.dumps(x) + "\n" for x in lines)
    p = subprocess.run(cmd, input=text.encode(), capture_output=True, timeout=60)
    out = [json.loads(x) for x in p.stdout.decode().splitlines() if x.strip()]
    names = [t["name"] for t in out[1]["result"]["tools"]]
    res = out[2]["result"]
    return names, res["content"][0]["text"], res["isError"]


def main():
    args = sys.argv[1:]
    stdin = sys.stdin.read()
    sys_file = opt(args, "--system-prompt-file")
    with open(sys_file, encoding="utf-8") as f:
        system = f.read()
    rec_dir = os.environ["FAKE_TURN_DIR"]
    os.makedirs(rec_dir, exist_ok=True)
    n = len([x for x in os.listdir(rec_dir) if x.startswith("turn-")])
    shown = ["SYSTEM_FILE" if a == sys_file else a for a in args]
    with open(os.path.join(rec_dir, f"turn-{n}.json"), "w", encoding="utf-8") as f:
        json.dump(
            {"argv": shown, "system": DATE.sub("DATE", system), "stdin": DATE.sub("DATE", stdin)},
            f,
            indent=1,
        )
    m = re.search(r"<message>\n(.*)\n</message>\n", stdin, re.S)
    msg = m.group(1) if m else ""
    words = msg.split()
    mcp = json.loads(opt(args, "--mcp-config"))["mcpServers"]["foreman"]
    cmd = [mcp["command"], *mcp["args"]]
    reply = "heard: " + msg
    if words[:1] == ["fail"]:
        print("first line\nthe turn failed here", file=sys.stderr)
        sys.exit(1)
    if words[:1] == ["error"]:
        print(json.dumps({"type": "result", "is_error": True, "result": "the model is down"}))
        return
    call = None
    if words[:1] == ["zoom"] and len(words) == 3:
        call = ("zoom", {"id": int(words[1]), "n": int(words[2])})
    elif words[:1] == ["label"] and len(words) == 2:
        call = ("zoom", {"label": words[1]})
    elif words[:1] == ["date"] and len(words) == 2:
        call = ("date", {"id": int(words[1])})
    elif words[:1] == ["status"]:
        call = ("status", {})
    elif words[:1] == ["spawn"] and len(words) >= 3:
        call = ("spawn", {"project": words[1], "brief": " ".join(words[2:])})
    if call is not None:
        names, text, bad = relay(cmd, *call)
        reply = f"tools {','.join(names)}; {call[0]} gave error={bad}: {DATE.sub('DATE', text)}"
    if words[:1] == ["hook"] and len(words) == 2:
        hook = json.loads(opt(args, "--settings"))["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
        p = subprocess.run(
            shlex.split(hook),
            input=json.dumps({"tool_name": words[1], "tool_input": {}}).encode(),
            capture_output=True,
        )
        reply = f"hook {words[1]}: exit {p.returncode} {p.stderr.decode().strip()}"
    usage = {
        "input_tokens": 10,
        "output_tokens": 3,
        "cache_read_input_tokens": 90,
        "cache_creation_input_tokens": 0,
    }
    print(json.dumps({"type": "result", "is_error": False, "result": reply, "usage": usage}))


main()

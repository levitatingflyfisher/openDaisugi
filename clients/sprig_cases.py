"""The sprig cases: what clients/sprig_compare.py runs on the Go and the Rust
sprig binaries.

Each case is a dict:

- name: unique.
- bin: "sprig", "sprig-hook", "sprig-mcp" or "weave".
- args: the arguments. Placeholders: {ROOT} the side's scratch root, {WORK}
  its work dir (the cwd), {SESS} its session dir, {API} the fake API's
  base URL, {DEAD} a base URL on a port nothing listens on.
- stdin: text (str, sent as UTF-8), bytes (sent as they are), or None for
  an empty stdin.
- env: more environment, with the same placeholders.
- files: files to make under {WORK} first, {path: text}. A str is written
  as UTF-8, bytes as they are.
- sessions: files to make under {SESS} first, {name: text}.
- claude: the fake claude's replies, in order (see reply()).
- api: the fake API's replies, in order (see api_reply()).
- gate: the fake gate's rules (see gate_rule()).
- fakes: which fake binaries are on PATH: a subset of "claude", "fakegate",
  "daisugi" (the fake gate under the name daisugi). Default: claude and
  fakegate.
- pwd_link: run with PWD set to a symlink to {WORK}.
- real_gate: run only with --daisugi, once per daisugi binary given, with
  that binary on PATH as daisugi, after `daisugi gate init --workspace
  {WORK}` in the side's HOME.
- tls: serve the fake API over https: {APIS} is its base URL and {CERT}
  the CA file that signed its certificate.
- timeout: seconds before the driver kills the run (default 40).

An argument may be bytes, passed as it is.
"""

from __future__ import annotations

import json

TOOL_FENCE = "```sprig-tool"


def envelope(text: str, usage: dict | None = None, is_error: bool = False, **extra) -> str:
    """The JSON `claude -p --output-format json` prints."""
    body = {
        "type": "result",
        "is_error": is_error,
        "result": text,
        "usage": usage
        or {
            "input_tokens": 11,
            "cache_read_input_tokens": 22,
            "cache_creation_input_tokens": 33,
            "output_tokens": 44,
        },
    }
    body.update(extra)
    return json.dumps(body)


def reply(text: str | None = None, raw: str | bytes | None = None, exit: int = 0, sleep: float = 0):
    """One fake claude reply: an envelope around text, or raw stdout (a str
    is sent as UTF-8, bytes as they are)."""
    out = raw if raw is not None else envelope(text or "")
    return {"raw": out, "exit": exit, "sleep": sleep}


def call(tool: str, fence: str = TOOL_FENCE, **inp) -> str:
    """A model reply that calls one tool in the text protocol."""
    return f"{fence}\n{json.dumps({'tool': tool, 'input': inp})}\n```"


def api_reply(body, status: int = 200, chunked: bool = False):
    """One fake API reply. body is a dict (sent as JSON) or raw text."""
    text = body if isinstance(body, str) else json.dumps(body)
    return {"status": status, "body": text, "chunked": chunked}


def api_text(text: str, model: str = "claude-fake-1"):
    return api_reply(
        {
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "model": model,
            "content": [{"type": "text", "text": text}],
            "usage": {
                "input_tokens": 5,
                "cache_read_input_tokens": 6,
                "cache_creation_input_tokens": 7,
                "output_tokens": 8,
            },
        }
    )


def api_tools(*uses, text: str = ""):
    content = [{"type": "text", "text": text}] if text else []
    for uid, name, inp in uses:
        content.append({"type": "tool_use", "id": uid, "name": name, "input": inp})
    return api_reply({"model": "claude-fake-1", "content": content, "usage": {"input_tokens": 1}})


def gate_rule(tool="*", match="", exit=0, stdout="", stderr="", sleep=0.0):
    """A fake gate rule: the first rule whose tool and match fit the payload
    decides. tool is the payload's tool_name or "*"; match is a substring of
    the raw payload."""
    return {
        "tool": tool,
        "match": match,
        "exit": exit,
        "stdout": stdout,
        "stderr": stderr,
        "sleep": sleep,
    }


ALLOW_ALL = [gate_rule()]
DENY_BASH = [
    gate_rule(
        tool="Bash",
        exit=2,
        stderr="RuntimeWarning: noise\nopenDaisugi gate: DENIED \u2014 bash is not allowed\ntrailing\n",
    ),
    gate_rule(),
]

API_ENV = {
    "SPRIG_BACKEND": "api",
    "ANTHROPIC_API_KEY": "fake-key-not-real",
    "ANTHROPIC_BASE_URL": "{API}",
}


def sprig(name, *args, **kw):
    return {"name": name, "bin": "sprig", "args": list(args), **kw}


def rpc(id_, method, params=None):
    msg = {"jsonrpc": "2.0", "method": method}
    if id_ is not None:
        msg["id"] = id_
    if params is not None:
        msg["params"] = params
    return json.dumps(msg)


CASES: list[dict] = []


def add(*cases):
    for c in cases:
        CASES.append(c)


# --- the command line -------------------------------------------------------

add(
    sprig("cli-version", "--version"),
    sprig("cli-version-one-dash", "-version"),
    sprig("cli-help", "--help"),
    sprig("cli-h", "-h"),
    sprig("cli-undefined-flag", "--nope", "task"),
    sprig("cli-missing-int", "--max-turns"),
    sprig("cli-bad-int", "--max-turns", "x", "task"),
    sprig("cli-int-out-of-range", "--max-turns=99999999999999999999", "task"),
    sprig("cli-int-hex-no-task", "--max-turns", "0x10"),
    sprig("cli-int-underscore-no-task", "--max-turns", "1_0"),
    sprig("cli-bad-bool", "--json=maybe", "task"),
    sprig("cli-bool-false-version", "--json=false", "--version"),
    sprig("cli-bool-T", "--version=T"),
    sprig("cli-no-task"),
    sprig("cli-task-bytes", b"t\xffask", b"\xe2\x82", claude=[reply("bytes")]),
    sprig(
        "cli-session-bytes",
        "--session-dir",
        "{SESS}",
        "--session",
        b"s\xff",
        "hi",
        claude=[reply("x")],
    ),
    sprig("cli-dash-blank-stdin", "-", stdin="  \n\t "),
    sprig("cli-resume-needs-dir", "--resume", "abc", "task"),
    sprig("cli-triple-dash", "---x"),
    sprig("cli-dash-equals", "-=x"),
    sprig("cli-missing-string", "--gate-cmd"),
    sprig("cli-api-no-key", "task", env={"SPRIG_BACKEND": "api"}),
    sprig("cli-no-claude", "hello", fakes=[]),
    sprig("cli-flag-after-task", "do", "it", "--json", claude=[reply("ok")]),
    sprig("cli-double-dash", "--", "--version", claude=[reply("v")]),
    sprig("cli-single-dash-flag-value", "-max-turns=3", "-json", "hi", claude=[reply("x")]),
)

# --- the tool wall, the model, usage in --json ------------------------------

add(
    sprig(
        "cli-tools-read-only",
        "--tools",
        "read",
        "read it",
        files={"notes.txt": "one\n"},
        claude=[reply(call("read", path="notes.txt")), reply("one line")],
    ),
    sprig(
        "cli-tools-call-outside-wall",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "--tools",
        "bash,read",
        "go",
        gate=ALLOW_ALL,
        claude=[reply(call("write", path="x.txt", content="no")), reply("refused")],
    ),
    sprig("cli-tools-empty", "--tools", "", "go", claude=[reply("never")]),
    sprig("cli-tools-unknown", "--tools=read,grep", "go", claude=[reply("never")]),
    sprig(
        "cli-model-flag-claude-argv",
        "--model",
        "sonnet",
        "--json",
        "say hi",
        claude=[reply("hi")],
    ),
    sprig(
        "cli-model-flag-api",
        "--model",
        "flag-model",
        "--tools",
        "edit,read",
        "say hi",
        env={**API_ENV, "SPRIG_MODEL": "env-model"},
        api=[api_text("hi")],
    ),
    sprig(
        "cli-json-usage",
        "--json",
        "read it",
        files={"notes.txt": "one\n"},
        claude=[reply(call("read", path="notes.txt")), reply("one line")],
    ),
    sprig(
        "cli-json-usage-none",
        "--json",
        "say hi",
        claude=[
            reply(
                envelope(
                    "hi",
                    usage={
                        "input_tokens": 0,
                        "cache_read_input_tokens": 0,
                        "cache_creation_input_tokens": 0,
                        "output_tokens": 0,
                    },
                )
            )
        ],
    ),
)

# --- the loop on the claude backend -----------------------------------------

add(
    sprig("loop-answer", "say hi", claude=[reply("hi there")]),
    sprig("loop-answer-json", "--json", "say hi", claude=[reply("hi <&>   there")]),
    sprig("loop-task-on-stdin", "-", stdin="  from stdin  \n", claude=[reply("ok")]),
    sprig("loop-task-stdin-no-arg", stdin="read me", claude=[reply("ok")]),
    sprig(
        "loop-read",
        "read it",
        files={"notes.txt": "line one\nline two\n"},
        claude=[reply(call("read", path="notes.txt")), reply("two lines")],
    ),
    sprig(
        "loop-read-missing",
        "read it",
        claude=[reply(call("read", path="nope/missing.txt")), reply("gone")],
    ),
    sprig(
        "loop-read-dir",
        "read it",
        files={"d/x": "x"},
        claude=[reply(call("read", path="d")), reply("a dir")],
    ),
    sprig(
        "loop-read-empty-path",
        "read it",
        claude=[reply(call("read")), reply("no path")],
    ),
    sprig(
        "loop-write-nested",
        "write it",
        claude=[reply(call("write", path="a/b/c.txt", content="hello\n")), reply("done")],
    ),
    sprig(
        "loop-write-under-file",
        "write it",
        files={"f": "x"},
        claude=[reply(call("write", path="f/g/h.txt", content="x")), reply("no")],
    ),
    sprig(
        "loop-write-to-dir",
        "write it",
        files={"d/x": "x"},
        claude=[reply(call("write", path="d", content="x")), reply("no")],
    ),
    sprig(
        "loop-edit-unique",
        "edit it",
        files={"e.txt": "alpha beta gamma\n"},
        claude=[reply(call("edit", path="e.txt", old="beta", new="BETA")), reply("edited")],
    ),
    sprig(
        "loop-edit-not-found",
        "edit it",
        files={"e.txt": "alpha\n"},
        claude=[reply(call("edit", path="e.txt", old='q"é\t\x01 ', new="x")), reply("n")],
    ),
    sprig(
        "loop-edit-not-unique",
        "edit it",
        files={"e.txt": "ab ab ab\n"},
        claude=[reply(call("edit", path="e.txt", old="ab", new="x")), reply("n")],
    ),
    sprig(
        "loop-edit-empty-old",
        "edit it",
        files={"e.txt": "hé\n"},
        claude=[reply(call("edit", path="e.txt", old="", new="x")), reply("n")],
    ),
    sprig(
        "loop-edit-empty-old-empty-file",
        "edit it",
        files={"e.txt": ""},
        claude=[reply(call("edit", path="e.txt", old="", new="new")), reply("n")],
    ),
    sprig(
        "loop-edit-missing-file",
        "edit it",
        claude=[reply(call("edit", path="nope.txt", old="a", new="b")), reply("n")],
    ),
    sprig(
        "loop-bash-combined",
        "run it",
        claude=[
            reply(call("bash", cmd="echo out; echo err >&2; echo out2; exit 3")),
            reply("ran"),
        ],
    ),
    sprig(
        "loop-bash-stdin-is-null",
        "run it",
        stdin="LEAKED STDIN\n",
        claude=[reply(call("bash", cmd="cat; echo end")), reply("ran")],
    ),
    sprig(
        "loop-bash-signal",
        "run it",
        claude=[reply(call("bash", cmd="echo before; kill -TERM $$")), reply("ran")],
    ),
    sprig(
        "loop-bash-binary-output",
        "run it",
        claude=[reply(call("bash", cmd="printf 'a\\377b\\342\\202c'")), reply("ran")],
    ),
    sprig(
        "loop-unknown-tool",
        "do it",
        claude=[reply(call("nope", fence="```tool", x=1)), reply("ok")],
    ),
    sprig(
        "loop-json-fence-unknown-is-answer",
        "do it",
        claude=[reply('Here:\n```json\n{"tool":"nope","input":{}}\n```')],
    ),
    sprig(
        "loop-json-fence-known",
        "do it",
        files={"k.txt": "K"},
        claude=[reply('```json\n{"tool":"read","input":{"path":"k.txt"}}\n```'), reply("k")],
    ),
    sprig(
        "loop-bare-call",
        "do it",
        files={"k.txt": "K"},
        claude=[reply('  {"tool":"read","input":{"path":"k.txt"}}  '), reply("k")],
    ),
    sprig(
        "loop-bare-unknown-is-answer",
        "do it",
        claude=[reply('{"tool":"zap","input":{}}')],
    ),
    sprig(
        "loop-case-folded-keys",
        "do it",
        files={"k.txt": "K"},
        claude=[
            reply('```tool\n{"TOOL":"read","ſkip":1,"Input":{"path":"k.txt"}}\n```'),
            reply("k"),
        ],
    ),
    sprig(
        "loop-null-input",
        "do it",
        claude=[reply('```sprig-tool\n{"tool":"read","input":null}\n```'), reply("k")],
    ),
    sprig(
        "loop-bad-input-type-is-answer",
        "do it",
        claude=[reply('{"tool":"read","input":[1]}'), reply("k")],
    ),
    sprig(
        "loop-clarify-then-answer",
        "do it",
        claude=[reply("```sprig-tool\n{not json\n```"), reply("final")],
    ),
    sprig(
        "loop-clarify-twice-accepts",
        "do it",
        claude=[reply("   "), reply("```tool broken")],
    ),
    sprig(
        "loop-clarify-reset-after-call",
        "do it",
        files={"k.txt": "K"},
        claude=[reply(""), reply(call("read", path="k.txt")), reply(""), reply("end")],
    ),
    sprig(
        "loop-max-turns",
        "--max-turns",
        "2",
        "do it",
        files={"k.txt": "K"},
        claude=[reply(call("read", path="k.txt")), reply(call("read", path="k.txt"))],
    ),
    sprig("loop-max-turns-zero", "--max-turns", "0", "do it"),
    sprig("loop-max-turns-negative", "--max-turns=-1", "do it"),
    sprig(
        "loop-envelope-is-error",
        "do it",
        claude=[reply(raw=envelope("x" * 250, is_error=True))],
    ),
    sprig(
        "loop-envelope-plain-text",
        "do it",
        claude=[reply(raw="é" * 150 + "tail")],
    ),
    sprig(
        "loop-envelope-wrong-type",
        "do it",
        claude=[reply(raw='{"type":"assistant","result":"x"}')],
    ),
    sprig(
        "loop-envelope-empty-object",
        "do it",
        claude=[reply(raw="{}")],
    ),
    sprig(
        "loop-envelope-bad-usage-type",
        "do it",
        claude=[reply(raw='{"type":"result","result":"x","usage":{"input_tokens":1.5}}')],
    ),
    sprig(
        "loop-envelope-case-keys",
        "do it",
        claude=[reply(raw='{"TYPE":"result","Result":"cased","IS_ERROR":false}')],
    ),
    sprig(
        "loop-envelope-surrogates-and-bytes",
        "--json",
        "do it",
        claude=[
            reply(raw=b'{"type":"result","result":"a\\ud800b\\udc00\\ud83d\\ude00 \xff\xe2\x82 c"}')
        ],
    ),
    sprig(
        "loop-envelope-trailing-garbage",
        "do it",
        claude=[reply(raw='{"type":"result","result":"x"} extra')],
    ),
    sprig("loop-claude-exit-1", "do it", claude=[reply("x", exit=1)]),
    sprig(
        "loop-claude-stdout-whitespace",
        "do it",
        claude=[reply(raw="\n\n  " + envelope("  spaced  ") + "  \n")],
    ),
    sprig(
        "loop-read-binary-file",
        "--session-dir",
        "{SESS}",
        "--session",
        "s1",
        "read it",
        files={"bin.dat": b"ok\xff\xfe\xe2\x82 end"},
        claude=[reply(call("read", path="bin.dat")), reply("binary")],
    ),
    sprig(
        "loop-read-long-multibyte",
        "--session-dir",
        "{SESS}",
        "--session",
        "s1",
        "read it",
        files={"long.txt": "é中" * 300},
        claude=[reply(call("read", path="long.txt")), reply("long")],
    ),
    sprig(
        "loop-read-long-bytes-short-runes",
        "--session-dir",
        "{SESS}",
        "--session",
        "s1",
        "read it",
        files={"mid.txt": ("中" * 200).encode("utf-8") + b"\xff"},
        claude=[reply(call("read", path="mid.txt")), reply("mid")],
    ),
    sprig(
        "loop-input-numbers",
        "--gate",
        "--gate-cmd",
        "fakegate check",
        "--session-dir",
        "{SESS}",
        "--session",
        "nums",
        "do it",
        files={"f": "F"},
        gate=ALLOW_ALL,
        claude=[
            reply(
                "```sprig-tool\n"
                '{"tool":"read","input":{"path":"f","n":1e2,"big":12345678901234567890,'
                '"neg":-0,"tiny":0.0000001,"small":0.000001,"huge":1e21,"frac":1.50,'
                '"s":"<&>\\u2028","b":true,"z":null,"arr":[1,{"k":"v"}],"dup":1,"dup":2}}\n'
                "```"
            ),
            reply("nums"),
        ],
    ),
    sprig(
        "loop-input-number-overflow",
        "do it",
        claude=[reply('```sprig-tool\n{"tool":"read","input":{"path":"f","n":1e400}}\n```')],
    ),
)

# --- the gate ---------------------------------------------------------------

add(
    sprig(
        "gate-allow-and-deny",
        "--gate",
        "--gate-cmd",
        "fakegate check --mode enforce",
        "do it",
        files={"k.txt": "K"},
        gate=DENY_BASH,
        claude=[
            reply(call("read", path="k.txt")),
            reply(call("bash", cmd="echo hi")),
            reply("done"),
        ],
    ),
    sprig(
        "gate-deny-stdout-only",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        gate=[gate_rule(exit=1, stdout="  just stdout  \n")],
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-deny-silent",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        gate=[gate_rule(exit=7)],
        claude=[reply(call("write", path="w", content="c")), reply("done")],
    ),
    sprig(
        "gate-deny-stderr-last-line",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        gate=[gate_rule(exit=1, stderr="first\n  second line  \n\n\n")],
        claude=[reply(call("edit", path="e", old="a", new="b")), reply("done")],
    ),
    sprig(
        "gate-default-cmd-no-daisugi",
        "--gate",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-default-cmd-fake-daisugi",
        "--gate",
        "do it",
        fakes=["claude", "daisugi"],
        gate=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply(call("read", path="x")), reply("d")],
    ),
    sprig(
        "gate-cmd-not-found",
        "--gate",
        "--gate-cmd",
        "nogate here",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-cmd-bad-path",
        "--gate",
        "--gate-cmd",
        "./no/such/gate",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-cmd-not-executable",
        "--gate",
        "--gate-cmd",
        "./plain.txt",
        "do it",
        files={"plain.txt": "not a program"},
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-cmd-empty",
        "--gate",
        "--gate-cmd",
        "   ",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-unknown-tool-skips-gate",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        gate=ALLOW_ALL,
        claude=[reply(call("zap", fence="```tool")), reply("done")],
    ),
    sprig(
        "gate-pwd-symlink",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        pwd_link=True,
        gate=ALLOW_ALL,
        claude=[reply(call("read", path="k")), reply("done")],
        files={"k": "K"},
    ),
    sprig(
        "gate-session-id",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "--session-dir",
        "{SESS}",
        "--session",
        "gs",
        "do it",
        gate=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "gate-timeout",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "do it",
        gate=[gate_rule(sleep=12)],
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
        timeout=60,
    ),
)

# --- the session tree -------------------------------------------------------

RESUME_FILE = "\n".join(
    [
        '{"type":"session","v":1,"id":"r1","harness":"sprig","cwd":"/x","ts":1.5}',
        '{"type":"prompt","id":"aaaa0001","parentId":null,"ts":2,"text":"first task"}',
        '{"type":"assistant","id":"aaaa0002","parentId":"aaaa0001","ts":3,"model":"haiku",'
        '"text":"","toolUses":[{"id":"t1","name":"read"}],"usage":{}}',
        '{"type":"tool_call","id":"aaaa0003","parentId":"aaaa0002","ts":4,"toolUseId":"t1",'
        '"name":"read","input":{"path":"k.txt"}}',
        '{"type":"verdict","id":"aaaa0004","parentId":"aaaa0003","ts":5,"toolUseId":"t1",'
        '"decision":"allow"}',
        "",
        '{"type":"tool_result","id":"aaaa0005","parentId":"aaaa0004","ts":6,"toolUseId":"t1",'
        '"ok":true,"summary":"K contents"}\r',
        '{"type":"assistant","id":"aaaa0006","parentId":"aaaa0005","ts":7,"model":"haiku",'
        '"text":"the answer"}',
        '{"type":"prompt","id":"bbbb0001","parentId":"aaaa0002","ts":8,"text":"branch"}',
        '{"type":"label","id":"cccc0001","parentId":"aaaa0006","ts":9,"name":"x"}',
        '{"torn',
        '{"type":"head","id":"dddd0001","ts":10,"leafId":"aaaa0006"}',
        "[1,2]",
        '{"type":7,"id":"eeee0001"}',
        '{"type":"note","n":1e400,"id":"ffff0001"}',
    ]
)

add(
    sprig(
        "sess-fixed-id",
        "--session-dir",
        "{SESS}",
        "--session",
        "fixed",
        "read it",
        files={"k.txt": "K"},
        claude=[
            reply(call("read", path="k.txt")),
            reply(call("zap", fence="```tool")),
            reply("done"),
        ],
    ),
    sprig(
        "sess-default-id",
        "--session-dir",
        "{SESS}",
        "say hi",
        claude=[reply("hi")],
    ),
    sprig(
        "sess-exists",
        "--session-dir",
        "{SESS}",
        "--session",
        "taken",
        "say hi",
        sessions={"taken.jsonl": "{}\n"},
        claude=[reply("hi")],
    ),
    sprig(
        "sess-resume",
        "--session-dir",
        "{SESS}",
        "--resume",
        "r1",
        "next task",
        sessions={"r1.jsonl": RESUME_FILE},
        claude=[reply("resumed answer")],
    ),
    sprig(
        "sess-resume-api",
        "--session-dir",
        "{SESS}",
        "--resume",
        "r1",
        "next task",
        sessions={"r1.jsonl": RESUME_FILE},
        env=API_ENV,
        api=[api_text("resumed via api")],
    ),
    sprig(
        "sess-resume-head-missing-leaf",
        "--session-dir",
        "{SESS}",
        "--resume",
        "r2",
        "go on",
        sessions={
            "r2.jsonl": '{"type":"prompt","id":"p1","parentId":null,"text":"one"}\n'
            '{"type":"head","id":"h1"}\n'
        },
        claude=[reply("fresh")],
    ),
    sprig(
        "sess-resume-missing",
        "--session-dir",
        "{SESS}",
        "--resume",
        "nope",
        "go on",
        claude=[reply("x")],
    ),
    sprig(
        "sess-dir-is-file",
        "--session-dir",
        "{WORK}/afile",
        "say hi",
        files={"afile": "x"},
        claude=[reply("hi")],
    ),
    sprig(
        "sess-id-with-slash",
        "--session-dir",
        "{SESS}",
        "--session",
        "a/b",
        "say hi",
        claude=[reply("hi")],
    ),
    sprig(
        "sess-id-dotdot",
        "--session-dir",
        "{SESS}/x/",
        "--session",
        "../y",
        "say hi",
        claude=[reply("hi")],
    ),
    sprig(
        "sess-api-tool-ids",
        "--session-dir",
        "{SESS}",
        "--session",
        "apis",
        "do it",
        files={"k.txt": "K"},
        env=API_ENV,
        api=[
            api_tools(
                ("toolu_real1", "read", {"path": "k.txt"}),
                ("toolu_real2", "bash", {"cmd": "echo two"}),
                text="thinking",
            ),
            api_text("done"),
        ],
    ),
)

# --- the API backend --------------------------------------------------------

add(
    sprig("api-text", "say hi", env=API_ENV, api=[api_text("hello from api")]),
    sprig(
        "api-https",
        "do it",
        files={"k.txt": "K"},
        tls=True,
        env={**API_ENV, "ANTHROPIC_BASE_URL": "{APIS}", "SSL_CERT_FILE": "{CERT}"},
        api=[api_tools(("u1", "read", {"path": "k.txt"})), api_text("over tls")],
    ),
    sprig(
        "api-input-numbers",
        "do it",
        env=API_ENV,
        api=[
            api_reply(
                '{"content":[{"type":"tool_use","id":"u1","name":"read",'
                '"input":{"path":"nope","n":1e2,"big":12345678901234567890,"neg":-0,"s":"<&>"}}]}'
            ),
            api_text("nums"),
        ],
    ),
    sprig(
        "api-tools-and-results",
        "--json",
        "do it",
        files={"k.txt": "K"},
        env=API_ENV,
        api=[
            api_tools(("u1", "read", {"path": "k.txt"}), ("u2", "nope", None)),
            api_tools(("u3", "write", {"path": "o.txt", "content": "O"})),
            api_text("  all done  "),
        ],
    ),
    sprig(
        "api-model-and-slash",
        "say hi",
        env={**API_ENV, "ANTHROPIC_BASE_URL": "{API}/", "SPRIG_MODEL": "other-model"},
        api=[api_text("hi")],
    ),
    sprig(
        "api-400-no-reason",
        "say hi",
        env=API_ENV,
        api=[api_reply({"error": {"message": "  "}}, status=400)],
    ),
    sprig(
        "api-400-reason",
        "say hi",
        env=API_ENV,
        api=[api_reply({"error": {"message": "bad <thing>"}}, status=400)],
    ),
    sprig("api-500", "say hi", env=API_ENV, api=[api_reply("  oops\n", status=500)]),
    sprig("api-bad-json", "say hi", env=API_ENV, api=[api_reply("{nope")]),
    sprig("api-empty-body", "say hi", env=API_ENV, api=[api_reply("")]),
    sprig(
        "api-type-error",
        "say hi",
        env=API_ENV,
        api=[api_reply('{"model":"m","content":[{"type":5}]}')],
    ),
    sprig(
        "api-top-level-array",
        "say hi",
        env=API_ENV,
        api=[api_reply("[]")],
    ),
    sprig(
        "api-usage-float",
        "say hi",
        env=API_ENV,
        api=[api_reply('{"content":[],"usage":{"output_tokens":2.5}}')],
    ),
    sprig(
        "api-chunked",
        "say hi",
        env=API_ENV,
        api=[{**api_text("chunked hello"), "chunked": True}],
    ),
    sprig("api-refused", "say hi", env={**API_ENV, "ANTHROPIC_BASE_URL": "{DEAD}"}),
    sprig(
        "api-empty-reply-nudge",
        "say hi",
        env=API_ENV,
        api=[api_reply({"content": []}), api_text("second")],
    ),
    sprig(
        "api-refusal-then-prompt-join",
        "--max-turns",
        "3",
        "say hi",
        env=API_ENV,
        api=[api_reply({"content": [{"type": "text", "text": "```tool x"}]}), api_text("ok")],
    ),
)

# --- sprig-hook -------------------------------------------------------------


def hook(name, payload, *args, **kw):
    return {"name": name, "bin": "sprig-hook", "args": list(args), "stdin": payload, **kw}


BASH_PAYLOAD = json.dumps({"tool_name": "Bash", "tool_input": {"command": "rm -rf /"}})
READ_PAYLOAD = json.dumps({"tool_name": "Read", "tool_input": {"file_path": "/etc/hosts"}})

add(
    hook("hook-allow", READ_PAYLOAD, "--gate-cmd", "fakegate check", gate=DENY_BASH),
    hook("hook-deny", BASH_PAYLOAD, "--gate-cmd", "fakegate check", gate=DENY_BASH),
    hook("hook-deny-silent", BASH_PAYLOAD, "--gate-cmd", "fakegate", gate=[gate_rule(exit=1)]),
    hook("hook-unreadable", "not json", "--gate-cmd", "fakegate", gate=ALLOW_ALL),
    hook("hook-empty-stdin", "", "--gate-cmd", "fakegate", gate=ALLOW_ALL),
    hook("hook-no-tool-name", '{"tool_input":{}}', "--gate-cmd", "fakegate", gate=ALLOW_ALL),
    hook(
        "hook-tool-name-wrong-type",
        '{"tool_name":5,"tool_input":{}}',
        "--gate-cmd",
        "fakegate",
        gate=ALLOW_ALL,
    ),
    hook(
        "hook-folded-dup-keys",
        '{"TOOL_NAME":"Read","tool_name":"Bash","Tool_Input":{"command":"x"},"tool_input":{"b":1}}',
        "--gate-cmd",
        "fakegate",
        gate=DENY_BASH,
    ),
    hook("hook-no-input", '{"tool_name":"write"}', "--gate-cmd", "fakegate", gate=ALLOW_ALL),
    hook(
        "hook-input-numbers",
        '{"tool_name":"Bash","tool_input":{"command":"ls","n":1e2,"big":12345678901234567890,'
        '"neg":-0,"s":"<&>\\u2028","dup":1,"dup":{"x":[]}}}',
        "--gate-cmd",
        "fakegate",
        gate=ALLOW_ALL,
    ),
    hook(
        "hook-gate-cmd-nbsp",
        READ_PAYLOAD,
        "--gate-cmd",
        "fakegate\u00a0check\u3000x",
        gate=ALLOW_ALL,
    ),
    hook("hook-default-cmd-no-daisugi", READ_PAYLOAD),
    hook(
        "hook-default-cmd-fake-daisugi",
        BASH_PAYLOAD,
        fakes=["daisugi"],
        gate=DENY_BASH,
    ),
    hook(
        "hook-timeout",
        READ_PAYLOAD,
        "--gate-cmd",
        "fakegate",
        "--gate-timeout",
        "1",
        gate=[gate_rule(sleep=3)],
    ),
    hook(
        "hook-bad-timeout-ignored",
        READ_PAYLOAD,
        "--gate-timeout",
        "zero",
        "--gate-cmd",
        "fakegate",
        "--gate-timeout",
        "-4",
        "--unknown",
        gate=ALLOW_ALL,
    ),
    hook("hook-trailing-gate-cmd", READ_PAYLOAD, "--gate-cmd"),
)

# --- sprig-mcp --------------------------------------------------------------


def mcp(name, lines, *args, **kw):
    stdin = "".join(line + "\n" for line in lines) if isinstance(lines, list) else lines
    return {"name": name, "bin": "sprig-mcp", "args": list(args), "stdin": stdin, **kw}


def tcall(id_, name, arguments):
    return rpc(id_, "tools/call", {"name": name, "arguments": arguments})


add(
    mcp(
        "mcp-handshake",
        [
            rpc(1, "initialize", {"protocolVersion": "2024-11-05"}),
            rpc(None, "notifications/initialized"),
            rpc(2, "tools/list"),
        ],
    ),
    mcp(
        "mcp-tools",
        [
            tcall(1, "write", {"path": "w/x.txt", "content": "W <&>"}),
            tcall(2, "read", {"path": "w/x.txt"}),
            tcall(3, "edit", {"path": "w/x.txt", "old": "W", "new": "V"}),
            tcall(4, "edit", {"path": "w/x.txt", "old": "Q", "new": "V"}),
            tcall(5, "bash", {"cmd": "cat; echo $((1+2)); exit 4"}),
            tcall(6, "bash", {"cmd": "exit 5"}),
            tcall(7, "read", {"path": "bin.dat"}),
            tcall(8, "zap", {}),
        ],
        files={"bin.dat": b"a\xffb"},
    ),
    mcp(
        "mcp-odd-requests",
        [
            "",
            "   ",
            "{bad json",
            '{"jsonrpc":"2.0","id":9,"method":"no/such"}',
            '{"jsonrpc":"2.0","method":"no/such/notification"}',
            '{"jsonrpc":"2.0","id":null,"method":"tools/list"}',
            '{"jsonrpc":"2.0","id":"s<1>","method":"initialize"}',
            '{"jsonrpc":"2.0","id": { "a" : [1, 2] , "b":"<x>"} ,"method":"initialize"}',
            '{"jsonrpc":5,"id":1,"method":"initialize"}',
            '{"JSONRPC":"2.0","ID":3,"METHOD":"tools/list"}',
            '{"jsonrpc":"2.0","id":4,"method":"tools/call"}',
            '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":5,"arguments":{"path":"x"}}}',
            '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read","arguments":"x"}}',
            '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read",'
            '"arguments":{"path":"a"},"arguments":{"x":1e400}}}',
            '{"jsonrpc":"2.0","id":8,"method":"notifications/initialized"}',
            "\t" + rpc(10, "tools/list") + "\r",
            "[1,2]",
            "null",
            '{"id":11}',
        ],
    ),
    mcp("mcp-no-trailing-newline", rpc(1, "tools/list")),
    mcp(
        "mcp-large-write",
        [
            tcall(1, "write", {"path": "big.txt", "content": "x\u00e9" * 100000}),
            tcall(2, "bash", {"cmd": "wc -c big.txt"}),
        ],
    ),
    mcp(
        "mcp-binary-stdin",
        b'\xff\xfe{"jsonrpc":"2.0","id":1}\n' + rpc(2, "tools/list").encode() + b"\n",
    ),
    mcp("mcp-empty", ""),
    mcp("mcp-help", [], "--help"),
    mcp(
        "mcp-gate",
        [tcall(1, "bash", {"cmd": "echo hi"}), tcall(2, "read", {"path": "r"})],
        "--gate",
        "--gate-cmd",
        "fakegate",
        files={"r": "R"},
        gate=DENY_BASH,
    ),
    mcp(
        "mcp-gate-cmd-without-gate",
        [tcall(1, "bash", {"cmd": "echo free"})],
        "--gate-cmd",
        "fakegate",
        gate=[gate_rule(exit=1)],
    ),
    mcp(
        "mcp-gate-timeout",
        [tcall(1, "read", {"path": "r"})],
        "--gate",
        "--gate-cmd",
        "fakegate",
        "--gate-timeout",
        "1",
        gate=[gate_rule(sleep=3)],
    ),
    mcp("mcp-gate-default-no-daisugi", [tcall(1, "read", {"path": "r"})], "--gate"),
)

# --- weave ------------------------------------------------------------------


def weave(name, *args, **kw):
    return {"name": name, "bin": "weave", "args": list(args), **kw}


def wf(steps, name="flow"):
    return json.dumps({"name": name, "steps": steps})


THREE_STEPS = wf(
    [
        {"id": "c", "task": "third", "needs": ["a", "b"]},
        {"id": "a", "task": "first"},
        {"id": "b", "task": "second", "needs": ["a"]},
    ],
    name='three "steps" é',
)

add(
    weave("weave-no-args"),
    weave("weave-help", "--help"),
    weave("weave-h-after-file", "x.json", "-h"),
    weave("weave-missing-file", "run", "nope.json"),
    weave("weave-dir", "run", "d", files={"d/x": "x"}),
    weave("weave-bad-json", "w.json", files={"w.json": "{"}),
    weave("weave-bad-json-char", "w.json", files={"w.json": '{"name": x}'}),
    weave("weave-bad-json-byte", "w.json", files={"w.json": b"\xc3\xa9"}),
    weave("weave-type-error", "w.json", files={"w.json": '{"steps":[{"id":5}]}'}),
    weave("weave-type-error-needs", "w.json", files={"w.json": '{"steps":[{"needs":[1]}]}'}),
    weave("weave-type-error-top", "w.json", files={"w.json": "[]"}),
    weave("weave-type-error-steps", "w.json", files={"w.json": '{"steps":{}}'}),
    weave("weave-no-id", "w.json", files={"w.json": wf([{"task": "t"}])}),
    weave(
        "weave-dup-id",
        "w.json",
        files={"w.json": wf([{"id": "a", "task": "t"}, {"id": "a", "task": "u"}])},
    ),
    weave(
        "weave-missing-need",
        "w.json",
        files={"w.json": wf([{"id": "a", "task": "t", "needs": ["zzé"]}])},
    ),
    weave(
        "weave-cycle",
        "w.json",
        files={
            "w.json": wf(
                [
                    {"id": "a", "task": "t", "needs": ["b"]},
                    {"id": "b", "task": "u", "needs": ["a"]},
                ]
            )
        },
    ),
    weave(
        "weave-self-need",
        "w.json",
        files={"w.json": wf([{"id": "a", "task": "t", "needs": ["a"]}])},
    ),
    weave(
        "weave-run-text",
        "run",
        "w.json",
        files={"w.json": THREE_STEPS},
        claude=[reply("A out"), reply("B out"), reply("C out")],
    ),
    weave(
        "weave-run-json",
        "w.json",
        "--json",
        files={"w.json": THREE_STEPS},
        claude=[reply("A <out>"), reply("B"), reply("C")],
    ),
    weave(
        "weave-step-fails",
        "w.json",
        "--json",
        files={"w.json": THREE_STEPS},
        claude=[reply("A"), reply("x", exit=1)],
    ),
    weave(
        "weave-step-fails-text",
        "w.json",
        files={"w.json": THREE_STEPS},
        claude=[reply("A"), reply("x", exit=1)],
    ),
    weave("weave-empty-json", "--json", "w.json", files={"w.json": wf([])}),
    weave("weave-empty-text", "w.json", files={"w.json": '{"name":"e"}'}),
    weave(
        "weave-folded-keys",
        "w.json",
        files={
            "w.json": '{"NAME":"N","name":"n2","STEPS":[{"ID":"a","Task":"t","NEEDS":null}],'
            '"ſteps":[{"id":"k","task":"kt"}]}'
        },
        claude=[reply("K")],
    ),
    weave(
        "weave-dup-steps-reuse",
        "--json",
        "w.json",
        files={
            "w.json": '{"steps":[{"id":"a","task":"ta"},{"id":"b","task":"tb"}],'
            '"steps":[{"id":"c"}]}'
        },
        claude=[reply("C")],
    ),
    weave(
        "weave-gate",
        "--gate",
        "--gate-cmd",
        "fakegate",
        "w.json",
        files={"w.json": wf([{"id": "a", "task": "t"}])},
        gate=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply("blocked")],
    ),
    weave(
        "weave-last-file-wins",
        "a.json",
        "--nope",
        files={"--nope": wf([{"id": "a", "task": "t"}])},
        claude=[reply("from --nope")],
    ),
)

# --- the real daisugi gate --------------------------------------------------

add(
    sprig(
        "real-gate-loop",
        "--gate",
        "--session-dir",
        "{SESS}",
        "--session",
        "real",
        "do it",
        real_gate=True,
        files={"in.txt": "inside"},
        claude=[
            reply(call("read", path="in.txt")),
            reply(call("write", path="out.txt", content="made")),
            reply(call("bash", cmd="rm -rf ~")),
            reply(call("read", path="/etc/hosts")),
            reply(call("bash", cmd="ls")),
            reply("done"),
        ],
    ),
    hook("real-gate-hook-deny", BASH_PAYLOAD, real_gate=True),
    hook(
        "real-gate-hook-allow",
        json.dumps({"tool_name": "Bash", "tool_input": {"command": "ls"}}),
        real_gate=True,
    ),
    hook(
        "real-gate-hook-sprig-names",
        json.dumps({"tool_name": "read", "tool_input": {"path": "/etc/hosts"}}),
        real_gate=True,
    ),
    mcp(
        "real-gate-mcp",
        [
            tcall(1, "write", {"path": "m.txt", "content": "M"}),
            tcall(2, "bash", {"cmd": "rm -rf /"}),
            tcall(3, "read", {"path": "/etc/passwd"}),
        ],
        "--gate",
        real_gate=True,
    ),
    weave(
        "real-gate-weave",
        "--gate",
        "w.json",
        real_gate=True,
        files={"w.json": wf([{"id": "a", "task": "t"}])},
        claude=[reply(call("bash", cmd="rm -rf ~")), reply("stopped")],
    ),
)


def by_name() -> dict[str, dict]:
    out: dict[str, dict] = {}
    for c in CASES:
        if c["name"] in out:
            raise ValueError(f"duplicate case {c['name']}")
        out[c["name"]] = c
    return out

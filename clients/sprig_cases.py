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
- warden: the fake warden's rules (see warden_rule()).
- fakes: which fake binaries are on PATH: a subset of "claude", "fakegate",
  "daisugi" (the fake warden under the name daisugi). Default: claude and
  fakegate.
- pwd_link: run with PWD set to a symlink to {WORK}.
- real_warden: run only with --daisugi, once per daisugi binary given, with
  that binary on PATH as daisugi, after `daisugi warden init --workspace
  {WORK}` in the side's HOME.
- tree: the fake tree command's replies, in order ({"stdout", "stderr",
  "exit"}); it is on PATH as faketree and records each call.
- mcp: the fake MCP server's script ({"tools": [...], "calls": {name: act},
  "child": bool}); it is on PATH as fakemcp and records each message.
- tls: serve the fake API over https: {APIS} is its base URL and {CERT}
  the CA file that signed its certificate.
- timeout: seconds before the driver kills the run (default 40).
- fds: descriptors open in sprig at start, {"N": text}: each a pipe that
  holds the text, its write end closed.
- sentinel: the case's key. The fake API logs only whether an auth header
  holds it, and the case fails as a leak when it is anywhere else.

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


def api_reply(body, status: int = 200, chunked: bool = False, location: str = ""):
    """One fake API reply. body is a dict (sent as JSON) or raw text.
    location sets a Location header ({OTHER} and {OTHERHOST} name the
    second server, which must see no request)."""
    text = body if isinstance(body, str) else json.dumps(body)
    r = {"status": status, "body": text, "chunked": chunked}
    if location:
        r["location"] = location
    return r


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


def api_tools(*uses, text: str = "", usage: dict | None = None):
    content = [{"type": "text", "text": text}] if text else []
    for uid, name, inp in uses:
        content.append({"type": "tool_use", "id": uid, "name": name, "input": inp})
    usage = usage if usage is not None else {"input_tokens": 1}
    return api_reply({"model": "claude-fake-1", "content": content, "usage": usage})


def api_text_usage(text: str, usage: dict):
    return api_reply(
        {"model": "claude-fake-1", "content": [{"type": "text", "text": text}], "usage": usage}
    )


def warden_rule(tool="*", match="", exit=0, stdout="", stderr="", sleep=0.0):
    """A fake warden rule: the first rule whose tool and match fit the payload
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


ALLOW_ALL = [warden_rule()]
DENY_BASH = [
    warden_rule(
        tool="Bash",
        exit=2,
        stderr="RuntimeWarning: noise\nopenDaisugi warden: DENIED \u2014 bash is not allowed\ntrailing\n",
    ),
    warden_rule(),
]

API_ENV = {
    "SPRIG_BACKEND": "api",
    "ANTHROPIC_API_KEY": "fake-key-not-real",
    "ANTHROPIC_BASE_URL": "{API}",
}


def _warden_off(args, default_warden=False):
    """Prepend --warden=false to a case that means "warden off": the warden is
    on by default, so a case that does not name a --warden flag and does not
    ask for the default gets the opt-out."""
    named = any(
        isinstance(a, str) and a.startswith("-") and a.lstrip("-").startswith("warden")
        for a in args
    )
    if default_warden or named:
        return list(args)
    return ["--warden=false", *args]


def sprig(name, *args, default_warden=False, **kw):
    return {"name": name, "bin": "sprig", "args": _warden_off(args, default_warden), **kw}


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
    sprig("cli-help", "--help", go_only=True),
    sprig("cli-h", "-h", go_only=True),
    sprig("cli-undefined-flag", "--nope", "task", go_only=True),
    sprig("cli-missing-int", "--max-turns", go_only=True),
    sprig("cli-bad-int", "--max-turns", "x", "task", go_only=True),
    sprig("cli-int-out-of-range", "--max-turns=99999999999999999999", "task", go_only=True),
    sprig("cli-int-hex-no-task", "--max-turns", "0x10"),
    sprig("cli-int-underscore-no-task", "--max-turns", "1_0"),
    sprig("cli-bad-bool", "--json=maybe", "task", go_only=True),
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
    sprig("cli-triple-dash", "---x", go_only=True),
    sprig("cli-dash-equals", "-=x", go_only=True),
    sprig("cli-missing-string", "--warden-cmd", go_only=True),
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
        "--warden",
        "--warden-cmd",
        "fakegate",
        "--tools",
        "bash,read",
        "go",
        warden=ALLOW_ALL,
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
        "--warden",
        "--warden-cmd",
        "fakegate check",
        "--session-dir",
        "{SESS}",
        "--session",
        "nums",
        "do it",
        files={"f": "F"},
        warden=ALLOW_ALL,
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

# --- the warden ---------------------------------------------------------------

add(
    sprig(
        "warden-allow-and-deny",
        "--warden",
        "--warden-cmd",
        "fakegate check --mode enforce",
        "do it",
        files={"k.txt": "K"},
        warden=DENY_BASH,
        claude=[
            reply(call("read", path="k.txt")),
            reply(call("bash", cmd="echo hi")),
            reply("done"),
        ],
    ),
    sprig(
        "warden-deny-stdout-only",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        warden=[warden_rule(exit=1, stdout="  just stdout  \n")],
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-deny-silent",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        warden=[warden_rule(exit=7)],
        claude=[reply(call("write", path="w", content="c")), reply("done")],
    ),
    sprig(
        "warden-deny-stderr-last-line",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        warden=[warden_rule(exit=1, stderr="first\n  second line  \n\n\n")],
        claude=[reply(call("edit", path="e", old="a", new="b")), reply("done")],
    ),
    sprig(
        "warden-default-cmd-no-daisugi",
        "--warden",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-default-cmd-fake-daisugi",
        "--warden",
        "do it",
        fakes=["claude", "daisugi"],
        warden=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply(call("read", path="x")), reply("d")],
    ),
    sprig(
        "default-warden-missing-daisugi",
        "do it",
        default_warden=True,
        fakes=["claude"],
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "default-warden-fake-daisugi",
        "do it",
        default_warden=True,
        fakes=["claude", "daisugi"],
        warden=ALLOW_ALL,
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-off-flag",
        "--warden=false",
        "do it",
        fakes=["claude", "daisugi"],
        warden=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-cmd-not-found",
        "--warden",
        "--warden-cmd",
        "nogate here",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-cmd-bad-path",
        "--warden",
        "--warden-cmd",
        "./no/such/warden",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-cmd-not-executable",
        "--warden",
        "--warden-cmd",
        "./plain.txt",
        "do it",
        files={"plain.txt": "not a program"},
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-cmd-empty",
        "--warden",
        "--warden-cmd",
        "   ",
        "do it",
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-unknown-tool-skips-warden",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        warden=ALLOW_ALL,
        claude=[reply(call("zap", fence="```tool")), reply("done")],
    ),
    sprig(
        "warden-pwd-symlink",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        pwd_link=True,
        warden=ALLOW_ALL,
        claude=[reply(call("read", path="k")), reply("done")],
        files={"k": "K"},
    ),
    sprig(
        "warden-session-id",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "--session-dir",
        "{SESS}",
        "--session",
        "gs",
        "do it",
        warden=DENY_BASH,
        claude=[reply(call("bash", cmd="echo hi")), reply("done")],
    ),
    sprig(
        "warden-timeout",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "do it",
        warden=[warden_rule(sleep=12)],
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

# --- keys: the key fd, the scrubbed child environment, redaction ------------
#
# A key case carries a sentinel key, sk-SENTINEL-<case>. The driver fails
# the case when the sentinel is in any output, call, request body or file;
# the fake API logs only whether an auth header held it.


def sentinel(name: str) -> str:
    return "sk-SENTINEL-" + name


def key_env(name: str) -> dict:
    """Every key variable set to the case's sentinel, and two more."""
    k = sentinel(name)
    return {
        "ANTHROPIC_API_KEY": k,
        "ANTHROPIC_AUTH_TOKEN": k,
        "OPENAI_API_KEY": k,
        "SPRIG_API_KEY": k,
        "SECRET_EXTRA": k,
        "KEEP_ME": "kept",
    }


SHOW_ENV = "env | grep -E '^(ANTHROPIC|OPENAI|SPRIG_API|SECRET_|KEEP_)' | sort; echo end"
SHOW_FDS = "for n in 3 4 5 6 7 8; do [ -e /proc/$$/fd/$n ] && echo open $n; done; echo checked"
FD_API_ENV = {"SPRIG_BACKEND": "api", "ANTHROPIC_BASE_URL": "{API}"}

add(
    sprig(
        "cli-key-fd",
        "--key-fd",
        "3",
        "say hi",
        fds={"3": sentinel("cli-key-fd") + "\n"},
        sentinel=sentinel("cli-key-fd"),
        env={**FD_API_ENV, "ANTHROPIC_API_KEY": "fake-key-not-real"},
        api=[api_text("hi")],
    ),
    sprig(
        "cli-key-fd-two-newlines",
        "--key-fd=4",
        "say hi",
        fds={"4": "fake-key-not-real\n\n"},
        env=FD_API_ENV,
        api=[api_text("hi")],
    ),
    sprig("cli-key-fd-empty", "--key-fd", "3", "say hi", fds={"3": ""}, env=FD_API_ENV),
    sprig("cli-key-fd-newline-only", "--key-fd", "3", "say hi", fds={"3": "\n"}, env=FD_API_ENV),
    sprig("cli-key-fd-not-open", "--key-fd", "9", "say hi", env=FD_API_ENV),
    sprig("cli-key-fd-stdio", "--key-fd", "1", "say hi", env=FD_API_ENV),
    sprig("cli-key-fd-bad-int", "--key-fd", "x", "say hi", go_only=True),
    sprig("cli-key-fd-huge", "--key-fd", "99999999999", "say hi", env=FD_API_ENV),
    sprig(
        "cli-key-fd-version-first",
        "--key-fd",
        "9",
        "--version",
    ),
    sprig(
        "cli-env-scrubbed-bash",
        "--scrub-env",
        "SECRET_EXTRA,,",
        "show env",
        sentinel=sentinel("cli-env-scrubbed-bash"),
        env=key_env("cli-env-scrubbed-bash"),
        claude=[reply(call("bash", cmd=SHOW_ENV)), reply("shown")],
    ),
    sprig(
        "cli-env-scrubbed-bash-api",
        "--key-fd",
        "3",
        "--scrub-env",
        "SECRET_EXTRA",
        "show env",
        fds={"3": sentinel("cli-env-scrubbed-bash-api")},
        sentinel=sentinel("cli-env-scrubbed-bash-api"),
        env={**key_env("cli-env-scrubbed-bash-api"), **FD_API_ENV},
        api=[api_tools(("u1", "bash", {"cmd": SHOW_ENV})), api_text("shown")],
    ),
    sprig(
        "cli-env-unscrubbed-extra",
        "show env",
        env={"SECRET_EXTRA": "plain", "KEEP_ME": "kept"},
        claude=[reply(call("bash", cmd=SHOW_ENV)), reply("shown")],
    ),
    sprig(
        "cli-env-scrubbed-warden",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "--scrub-env",
        "SECRET_EXTRA",
        "read it",
        files={"a.txt": "A"},
        warden=ALLOW_ALL,
        sentinel=sentinel("cli-env-scrubbed-warden"),
        env=key_env("cli-env-scrubbed-warden"),
        claude=[reply(call("read", path="a.txt")), reply("read")],
    ),
    sprig(
        "cli-key-redacted-error",
        "--key-fd",
        "3",
        "say hi",
        fds={"3": sentinel("cli-key-redacted-error") + "\n"},
        sentinel=sentinel("cli-key-redacted-error"),
        env=FD_API_ENV,
        api=[
            api_reply(
                {"error": {"message": "bad key " + sentinel("cli-key-redacted-error") + " here"}},
                status=401,
            )
        ],
    ),
    sprig(
        "cli-key-redacted-400",
        "--key-fd",
        "3",
        "say hi",
        fds={"3": sentinel("cli-key-redacted-400")},
        sentinel=sentinel("cli-key-redacted-400"),
        env=FD_API_ENV,
        api=[api_reply({"error": {"message": sentinel("cli-key-redacted-400")}}, status=400)],
    ),
    sprig(
        "cli-env-key-redacted-error",
        "say hi",
        sentinel=sentinel("cli-env-key-redacted-error"),
        env={
            "SPRIG_BACKEND": "api",
            "ANTHROPIC_BASE_URL": "{API}",
            "ANTHROPIC_API_KEY": sentinel("cli-env-key-redacted-error"),
        },
        api=[api_reply("echo " + sentinel("cli-env-key-redacted-error"), status=401)],
    ),
    sprig(
        "openai-env-key-redacted-error",
        "--wire",
        "openai",
        "--base-url",
        "{API}",
        "--model",
        "m",
        "say hi",
        sentinel=sentinel("openai-env-key-redacted-error"),
        env={"OPENAI_API_KEY": sentinel("openai-env-key-redacted-error")},
        api=[api_reply("echo " + sentinel("openai-env-key-redacted-error"), status=401)],
    ),
    sprig(
        "cli-no-fd-leak",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "check fds",
        fds={"5": "five", "7": ""},
        warden=ALLOW_ALL,
        claude=[reply(call("bash", cmd=SHOW_FDS)), reply("checked")],
    ),
    sprig(
        "cli-no-fd-leak-key",
        "--key-fd",
        "3",
        "check fds",
        fds={"3": "fake-key-not-real", "4": "four"},
        env=FD_API_ENV,
        api=[api_tools(("u1", "bash", {"cmd": SHOW_FDS})), api_text("checked")],
    ),
)

# --- the wires: --wire, --base-url, the OpenAI chat wire --------------------


def oai_reply(content=None, calls=(), usage=None, model="oai-fake-1"):
    """One fake OpenAI chat-completions reply. calls are (id, name,
    arguments); arguments is a dict (sent as a JSON string) or sent as it
    is."""
    msg: dict = {"role": "assistant", "content": content}
    if calls:
        msg["tool_calls"] = [
            {
                "id": cid,
                "type": "function",
                "function": {
                    "name": name,
                    "arguments": json.dumps(args) if isinstance(args, dict) else args,
                },
            }
            for cid, name, args in calls
        ]
    body = {"id": "chatcmpl-1", "model": model, "choices": [{"index": 0, "message": msg}]}
    body["usage"] = usage if usage is not None else {"prompt_tokens": 9, "completion_tokens": 4}
    return api_reply(body)


OAI = ("--wire", "openai", "--base-url", "{API}/v1")

# A response past 32 MiB fails the call with one plain line.
BIG = "x" * (33 << 20)
add(
    sprig(
        "anthropic-response-too-big",
        "--wire",
        "anthropic",
        "--base-url",
        "{API}",
        "say hi",
        api=[api_reply(BIG)],
    ),
    sprig(
        "anthropic-response-too-big-chunked",
        "--wire",
        "anthropic",
        "--base-url",
        "{API}",
        "say hi",
        api=[{**api_reply(BIG, chunked=True), "chunk": 1 << 16}],
    ),
    sprig("openai-response-too-big", *OAI, "--model", "m1", "say hi", api=[api_reply(BIG)]),
)

# A negative usage count fails the turn; huge counts add with no wrap.
add(
    sprig(
        "anthropic-usage-negative",
        "--wire",
        "anthropic",
        "--base-url",
        "{API}",
        "--json",
        "say hi",
        api=[api_text_usage("hi", {"input_tokens": 5, "output_tokens": -1000000})],
    ),
    sprig(
        "openai-usage-negative-cached",
        *OAI,
        "--model",
        "m1",
        "--json",
        "say hi",
        api=[
            api_reply(
                {
                    "choices": [{"message": {"content": "hi"}}],
                    "usage": {"prompt_tokens": 3, "prompt_tokens_details": {"cached_tokens": -9}},
                }
            )
        ],
    ),
    sprig(
        "openai-usage-negative-prompt",
        *OAI,
        "--model",
        "m1",
        "say hi",
        api=[
            api_reply({"choices": [{"message": {"content": "hi"}}], "usage": {"prompt_tokens": -3}})
        ],
    ),
    sprig(
        "anthropic-usage-huge-capped",
        "--wire",
        "anthropic",
        "--base-url",
        "{API}",
        "--max-tokens",
        "100",
        "--json",
        "read it",
        files={"a.txt": "A"},
        api=[
            api_tools(
                ("u1", "read", {"path": "a.txt"}),
                usage={
                    "input_tokens": 4611686018427387903,
                    "output_tokens": 4611686018427387903,
                    "cache_read_input_tokens": 4611686018427387903,
                    "cache_creation_input_tokens": 4611686018427387903,
                },
            ),
            api_text("never"),
        ],
    ),
)

# A server's redirect is an error: sprig follows none, so neither the key
# nor the request reaches the second server.
for status in (302, 307, 308):
    for where, target in (("host", "{OTHERHOST}"), ("port", "{OTHER}")):
        add(
            sprig(
                f"anthropic-redirect-{status}-{where}",
                "--wire",
                "anthropic",
                "--base-url",
                "{API}",
                "--key-fd",
                "3",
                "say hi",
                fds={"3": sentinel(f"anthropic-redirect-{status}-{where}")},
                sentinel=sentinel(f"anthropic-redirect-{status}-{where}"),
                api=[api_reply("moved", status=status, location=target + "/v1/messages")],
            ),
            sprig(
                f"openai-redirect-{status}-{where}",
                *OAI,
                "--model",
                "m1",
                "--key-fd",
                "3",
                "say hi",
                fds={"3": sentinel(f"openai-redirect-{status}-{where}")},
                sentinel=sentinel(f"openai-redirect-{status}-{where}"),
                api=[api_reply("moved", status=status, location=target + "/v1/chat/completions")],
            ),
        )
ANT = ("--wire", "anthropic", "--base-url", "{API}")

add(
    sprig(
        "openai-text",
        *OAI,
        "--key-fd",
        "3",
        "--model",
        "m-1",
        "--json",
        "say hi",
        fds={"3": sentinel("openai-text")},
        sentinel=sentinel("openai-text"),
        api=[oai_reply("  hello  ")],
    ),
    sprig(
        "openai-tool-call",
        *OAI,
        "--key-fd",
        "3",
        "--model",
        "m-1",
        "read it",
        files={"k.txt": "K<&>"},
        fds={"3": sentinel("openai-tool-call")},
        sentinel=sentinel("openai-tool-call"),
        api=[oai_reply(calls=[("c1", "read", {"path": "k.txt"})]), oai_reply("read K")],
    ),
    sprig(
        "openai-two-calls",
        *OAI,
        "--model",
        "m-1",
        "--tools",
        "read,write",
        "do it",
        files={"k.txt": "K"},
        api=[
            oai_reply(
                "thinking",
                calls=[
                    ("c1", "read", {"path": "k.txt"}),
                    ("c2", "write", {"path": "o.txt", "content": "O", "n": 1e2}),
                ],
            ),
            oai_reply(calls=[("c3", "bash", {"cmd": "true"})]),
            oai_reply("done"),
        ],
    ),
    sprig(
        "openai-bad-arguments",
        *OAI,
        "--model",
        "m-1",
        "do it",
        api=[oai_reply(calls=[("c1", "read", "{not json")])],
    ),
    sprig(
        "openai-arguments-array",
        *OAI,
        "--model",
        "m-1",
        "do it",
        api=[oai_reply(calls=[("c1", "read", "[1]")])],
    ),
    sprig(
        "openai-arguments-empty-and-null",
        *OAI,
        "--model",
        "m-1",
        "do it",
        api=[
            oai_reply(calls=[("c1", "bash", ""), ("c2", "bash", "null")]),
            oai_reply("ok"),
        ],
    ),
    sprig(
        "openai-arguments-object",
        *OAI,
        "--model",
        "m-1",
        "do it",
        files={"k.txt": "K"},
        api=[
            api_reply(
                '{"choices":[{"message":{"tool_calls":[{"id":"c1","function":'
                '{"name":"read","arguments":{"path":"k.txt","n":12345678901234567890}}}]}}]}'
            ),
            oai_reply("ok"),
        ],
    ),
    sprig(
        "openai-usage-cached",
        *OAI,
        "--model",
        "m-1",
        "--json",
        "say hi",
        api=[
            oai_reply(
                calls=[("c1", "bash", {"cmd": "true"})],
                usage={
                    "prompt_tokens": 100,
                    "completion_tokens": 7,
                    "prompt_tokens_details": {"cached_tokens": 40},
                },
            ),
            oai_reply(
                "hi",
                usage={
                    "prompt_tokens": 5,
                    "completion_tokens": 2.5,
                    "prompt_tokens_details": {"cached_tokens": 9},
                },
            ),
        ],
    ),
    sprig(
        "openai-usage-odd",
        *OAI,
        "--model",
        "m-1",
        "--json",
        "say hi",
        api=[oai_reply("hi", usage={"prompt_tokens": 1e2, "completion_tokens": -3})],
    ),
    sprig(
        "openai-http-error",
        *OAI,
        "--key-fd",
        "3",
        "--model",
        "m-1",
        "say hi",
        fds={"3": sentinel("openai-http-error")},
        sentinel=sentinel("openai-http-error"),
        api=[api_reply("  no such key " + sentinel("openai-http-error") + "\n", status=401)],
    ),
    sprig("openai-bad-json", *OAI, "--model", "m-1", "say hi", api=[api_reply("{nope")]),
    sprig(
        "openai-huge-number",
        *OAI,
        "--model",
        "m-1",
        "say hi",
        api=[api_reply('{"choices":[],"x":1e400}')],
    ),
    sprig(
        "openai-no-choices",
        *OAI,
        "--model",
        "m-1",
        "say hi",
        api=[api_reply('{"choices":[]}'), oai_reply("second")],
    ),
    sprig(
        "openai-no-key-local",
        *OAI,
        "--model",
        "m-1",
        "say hi",
        env={"OPENAI_API_KEY": ""},
        api=[oai_reply("local")],
    ),
    sprig(
        "openai-env-key",
        *OAI,
        "--model",
        "m-1",
        "say hi",
        env={"OPENAI_API_KEY": "fake-key-not-real"},
        api=[oai_reply("from env")],
    ),
    sprig("openai-default-no-key", "--wire", "openai", "--model", "m-1", "say hi"),
    sprig("openai-no-model", *OAI, "say hi"),
    sprig(
        "openai-model-from-env",
        *OAI,
        "say hi",
        env={"SPRIG_MODEL": "env-model"},
        api=[oai_reply("hi")],
    ),
    sprig("openai-refused", "--wire", "openai", "--base-url", "{DEAD}", "--model", "m", "x"),
    sprig(
        "openai-session-tree",
        *OAI,
        "--model",
        "m-1",
        "--session-dir",
        "{SESS}",
        "--session",
        "s1",
        "read it",
        files={"k.txt": "K"},
        api=[oai_reply(calls=[("c1", "read", {"path": "k.txt"})]), oai_reply("read")],
    ),
    sprig(
        "anthropic-base-url-flag",
        *ANT,
        "--key-fd",
        "3",
        "say hi",
        fds={"3": sentinel("anthropic-base-url-flag")},
        sentinel=sentinel("anthropic-base-url-flag"),
        env={"ANTHROPIC_BASE_URL": "{DEAD}"},
        api=[api_text("hi")],
    ),
    sprig(
        "anthropic-no-key-local",
        *ANT,
        "say hi",
        env={"ANTHROPIC_API_KEY": ""},
        api=[api_text("local")],
    ),
    sprig(
        "anthropic-env-base-no-key",
        "say hi",
        env={"SPRIG_BACKEND": "api", "ANTHROPIC_BASE_URL": "{API}/"},
        api=[api_text("local")],
    ),
    sprig("anthropic-default-no-key", "--wire", "anthropic", "say hi"),
    sprig(
        "anthropic-wire-overrides-backend",
        "--wire",
        "claude-code",
        "say hi",
        env=API_ENV,
        claude=[reply("from claude")],
    ),
    sprig("wire-unknown", "--wire", "grpc", "say hi"),
    sprig("wire-empty-is-default", "--wire=", "say hi", claude=[reply("default")]),
    sprig("wire-base-url-claude-code", "--base-url", "{API}", "say hi"),
)

# Any model id goes through every wire the same way: the id is free text.
for _model in ("qwen3-coder", "llama3.3:70b", "grok-4", "gemini-2.5-pro"):
    _safe = _model.replace(":", "-").replace(".", "-")
    add(
        sprig(
            f"ancestry-{_safe}-claude-code",
            "--model",
            _model,
            "--json",
            "say hi",
            claude=[reply("hi")],
        ),
        sprig(
            f"ancestry-{_safe}-anthropic",
            *ANT,
            "--model",
            _model,
            "--json",
            "say hi",
            api=[api_text("hi", model=_model)],
        ),
        sprig(
            f"ancestry-{_safe}-openai",
            *OAI,
            "--model",
            _model,
            "--json",
            "say hi",
            api=[oai_reply("hi", model=_model)],
        ),
    )

# --- the token cap and model turns ------------------------------------------

add(
    sprig(
        "cli-max-tokens-stop",
        "--max-tokens",
        "150",
        "read it",
        files={"a.txt": "A"},
        claude=[
            reply(call("read", path="a.txt")),
            reply(call("read", path="a.txt")),
            reply("never asked"),
        ],
    ),
    sprig(
        "cli-max-tokens-stop-json-session",
        "--max-tokens=110",
        "--json",
        "--session-dir",
        "{SESS}",
        "--session",
        "cap",
        "read it",
        files={"a.txt": "A"},
        claude=[reply(call("read", path="a.txt")), reply("never asked")],
    ),
    sprig("cli-max-tokens-zero", "--max-tokens", "0", "say hi", claude=[reply("never")]),
    sprig(
        "cli-max-tokens-room",
        "--max-tokens",
        "1000",
        "--json",
        "say hi",
        claude=[reply("plenty")],
    ),
    sprig("cli-max-tokens-bad", "--max-tokens", "lots", "say hi", go_only=True),
    sprig(
        "cli-max-tokens-negative-off",
        "--max-tokens",
        "-7",
        "say hi",
        claude=[reply("no cap")],
    ),
    sprig(
        "cli-max-tokens-api",
        "--max-tokens",
        "26",
        "read it",
        files={"a.txt": "A"},
        env=API_ENV,
        api=[
            api_reply(
                {
                    "model": "m",
                    "content": [
                        {"type": "tool_use", "id": "u1", "name": "read", "input": {"path": "a.txt"}}
                    ],
                    "usage": {
                        "input_tokens": 5,
                        "cache_read_input_tokens": 6,
                        "cache_creation_input_tokens": 7,
                        "output_tokens": 8,
                    },
                }
            ),
            api_text("never asked"),
        ],
    ),
    sprig(
        "cli-json-model-turns",
        "--json",
        "do it",
        files={"a.txt": "A"},
        claude=[reply(call("read", path="a.txt")), reply(""), reply("done")],
    ),
)

# --- a one-shot call: --tools none and --system-file -------------------------
# No tool schema goes out, a tool call from the model fails the run, and the
# file's bytes are the system prompt on every wire. The compactor reads tool
# results word for word, so on the claude-code wire claude starts with every
# built-in off and an empty strict MCP config.

SYS = {"sys.txt": "You make one short line.\nKeep every name."}
ONESHOT = ("--tools", "none", "--system-file", "sys.txt", "--json")

add(
    sprig(
        "oneshot-anthropic",
        *ONESHOT,
        "-",
        stdin="a long message",
        files=SYS,
        env=API_ENV,
        api=[api_text("short line")],
    ),
    sprig(
        "oneshot-openai",
        *ONESHOT,
        *OAI,
        "--model",
        "m1",
        "-",
        stdin="a long message",
        files=SYS,
        api=[oai_reply("short line")],
    ),
    sprig(
        "oneshot-claude-code",
        *ONESHOT,
        "--model",
        "haiku",
        "-",
        stdin="a long message",
        files=SYS,
        claude=[reply("short line")],
    ),
    sprig("oneshot-claude-code-no-file", "--tools", "none", "say hi", claude=[reply("hi")]),
    sprig(
        "oneshot-anthropic-no-file",
        "--tools",
        "none",
        "say hi",
        env=API_ENV,
        api=[api_text("hi")],
    ),
    sprig(
        "oneshot-tool-call-refused",
        *ONESHOT,
        "-",
        stdin="a long message",
        files=SYS,
        env=API_ENV,
        api=[api_tools(("u1", "bash", {"cmd": "ls"})), api_text("never asked")],
    ),
    sprig(
        "oneshot-tool-call-refused-claude-code",
        "--tools",
        "none",
        "go",
        claude=[reply(call("bash", cmd="ls")), reply("never asked")],
    ),
    sprig(
        "system-file-with-tools-claude-code",
        "--system-file",
        "sys.txt",
        "read it",
        files={**SYS, "notes.txt": "one\n"},
        claude=[reply(call("read", path="notes.txt")), reply("one line")],
    ),
    sprig(
        "system-file-with-tools-anthropic",
        "--system-file",
        "sys.txt",
        "say hi",
        files=SYS,
        env=API_ENV,
        api=[api_text("hi")],
    ),
    sprig("system-file-missing", "--system-file", "nope.txt", "go", claude=[reply("never")]),
    sprig("system-file-is-a-dir", "--system-file", ".", "go", claude=[reply("never")]),
    sprig("tools-none-with-others", "--tools", "none,read", "go", claude=[reply("never")]),
)

# --- the spawn tool ----------------------------------------------------------


def tree_reply(status="ran", exit=None, **kw):
    """One reply of the fake tree command, as daisugi tree run prints it."""
    body = {
        "status": status,
        "session": kw.pop("session", "n-0123456789ab"),
        "answer": kw.pop("answer", None),
        "tokens_used": kw.pop("tokens_used", None),
        "turns_used": kw.pop("turns_used", None),
        "reasons": kw.pop("reasons", []),
        "ask_id": kw.pop("ask_id", None),
    }
    code = {"ran": 0, "asked": 3}.get(status, 1) if exit is None else exit
    return {"stdout": json.dumps(body) + "\n", "exit": code, **kw}


def branch_reply(answer="kept", tokens=(10, 12), choice_id="ch_000000000001", **kw):
    """One reply of the fake tree command for a branched run (attempts)."""
    branches = [
        {
            "session": f"n-00000000000{k}",
            "status": "ran",
            "worktree": f"/w/b{k}",
            "tokens_used": t,
            "turns_used": 1,
            "tests": [],
        }
        for k, t in enumerate(tokens)
    ]
    body = {
        "status": "ran",
        "session": "n-000000000000",
        "answer": answer,
        "reasons": [],
        "branches": branches,
        "leader": "n-000000000000",
        "choice_id": choice_id,
        "ranking": {},
        "ranking_id": "br_000000000001",
        "ask_id": None,
    }
    return {"stdout": json.dumps(body) + "\n", "exit": 0, **kw}


SPAWN = ("--tools", "read,spawn", "--tree-cmd", "faketree --data-dir D", "--session", "par-1")
CHILD_ENV = {"permissions": {"file_read": ["{WORK}/**"], "max_execution_time_s": 30}}


def spawn_call(**inp):
    inp.setdefault("task", "list the files")
    inp.setdefault("envelope", CHILD_ENV)
    return call("spawn", **inp)


add(
    # Every key of the bundle reads [key] in the spawn result, whether it
    # comes back in the reasons or on the tree command's stderr.
    sprig(
        "spawn-redacts-bundle-keys",
        *SPAWN,
        "--spawn-keys-fd",
        "4",
        "go",
        fds={
            "4": json.dumps(
                {
                    "desk": {"wire": "openai", "key": sentinel("spawn-redacts-bundle-keys")},
                    "z": {"wire": "anthropic", "key": "sk-other-SENTINEL-2"},
                },
                sort_keys=True,
                separators=(",", ":"),
            )
        },
        sentinel=sentinel("spawn-redacts-bundle-keys"),
        tree=[
            tree_reply(
                "refused",
                reasons=[
                    "bad key " + sentinel("spawn-redacts-bundle-keys") + " and sk-other-SENTINEL-2"
                ],
            ),
            {
                "stdout": "",
                "stderr": "boom " + sentinel("spawn-redacts-bundle-keys") + "\n",
                "exit": 1,
            },
        ],
        claude=[reply(spawn_call()), reply(spawn_call()), reply("after")],
    ),
)

add(
    # Tree command output past 16 MiB fails the call with one line.
    sprig(
        "spawn-tree-output-too-big",
        *SPAWN,
        "go",
        tree=[tree_reply(big=17 << 20)],
        claude=[reply(spawn_call()), reply("after")],
    ),
)

add(
    sprig("spawn-not-offered", "--tools", "read,spawn", "x"),
    sprig("spawn-needs-session", "--tools", "spawn", "--tree-cmd", "faketree", "x"),
    sprig("spawn-tree-cmd-blank", "--tools", "spawn", "--tree-cmd", "  ", "--session", "s", "x"),
    sprig(
        "spawn-prompt-five-tools",
        "--tools",
        "read,write,edit,bash,spawn",
        "--tree-cmd",
        "faketree",
        "--session",
        "s",
        "say hi",
        claude=[reply("hi")],
    ),
    sprig(
        "spawn-warden-deny",
        "--warden",
        "--warden-cmd",
        "fakegate",
        *SPAWN,
        "spawn",
        warden=[
            warden_rule(
                tool="mcp__sprig__spawn", exit=2, stderr="openDaisugi warden: DENIED spawn\n"
            ),
            warden_rule(),
        ],
        tree=[],
        claude=[reply(spawn_call()), reply("it was refused")],
    ),
    sprig(
        "spawn-runs",
        "--warden",
        "--warden-cmd",
        "fakegate",
        *SPAWN,
        "--json",
        "spawn",
        warden=ALLOW_ALL,
        tree=[tree_reply(answer="three files", tokens_used=40, turns_used=2)],
        claude=[
            reply(spawn_call(profile="desk", model="llama3.3:70b", tools=["read", "bash"])),
            reply("the child said three files"),
        ],
    ),
    sprig(
        "spawn-refused",
        *SPAWN,
        "spawn",
        tree=[
            tree_reply(
                "refused", reasons=["tokens: the child asks for 9; the parent has 1 left", "x"]
            )
        ],
        claude=[reply(spawn_call(tokens=9)), reply("refused")],
    ),
    sprig(
        "spawn-asked",
        *SPAWN,
        "spawn",
        tree=[tree_reply("asked", ask_id="ask_0123456789ab", reasons=["r"])],
        claude=[reply(spawn_call()), reply("asked")],
    ),
    sprig(
        "spawn-failed-child",
        *SPAWN,
        "--json",
        "spawn",
        tree=[tree_reply("failed", reasons=["agentic sub-agent failed: sprig exited with code 1"])],
        claude=[reply(spawn_call(tokens=30)), reply("it failed")],
    ),
    sprig(
        "spawn-parent-pinned",
        *SPAWN,
        "spawn",
        tree=[tree_reply(answer="ok")],
        claude=[
            reply(spawn_call(parent="evil-root", session="evil", tokens=5, turns=2, max_turns=3)),
            reply("done"),
        ],
    ),
    sprig(
        "spawn-keys-forwarded",
        *SPAWN,
        "--spawn-keys-fd",
        "4",
        "spawn",
        fds={"4": json.dumps({"desk": {"key": sentinel("spawn-keys-forwarded")}})},
        sentinel=sentinel("spawn-keys-forwarded"),
        env=key_env("spawn-keys-forwarded"),
        tree=[tree_reply(answer="ok")],
        claude=[reply(spawn_call(profile="desk")), reply("done")],
    ),
    sprig(
        "spawn-branch-counts",
        *SPAWN,
        "--max-tokens",
        "200",
        "spawn",
        tree=[tree_reply(answer="a", tokens_used=80), tree_reply(answer="b", tokens_used=80)],
        claude=[reply(spawn_call()), reply(spawn_call()), reply("never asked")],
    ),
    sprig(
        "spawn attempts",
        *SPAWN,
        "--json",
        "spawn",
        tree=[branch_reply(answer="the kept answer", tokens=(30, 40), choice_id="ch_000000000001")],
        claude=[
            reply(spawn_call(attempts=2, why="failed_tests", tokens=60)),
            reply("kept"),
        ],
    ),
    sprig(
        "spawn attempts no card",
        *SPAWN,
        "spawn",
        tree=[branch_reply(answer="only one ran", tokens=(30, None), choice_id=None)],
        claude=[reply(spawn_call(attempts=3, why="operator", tokens=50)), reply("kept")],
    ),
    sprig(
        "spawn attempts bad",
        *SPAWN,
        "spawn",
        tree=[],
        claude=[
            reply(spawn_call(attempts=9, why="operator")),
            reply(spawn_call(attempts=2)),
            reply(spawn_call(attempts=2, why="because")),
            reply(spawn_call(attempts=1.5, why="operator")),
            reply(spawn_call(attempts="2", why="operator")),
            reply("gave up"),
        ],
    ),
    sprig(
        "spawn attempts cap",
        *SPAWN,
        "--max-tokens",
        "100",
        "spawn",
        tree=[
            branch_reply(answer="a", tokens=(40, 40), choice_id="ch_000000000001"),
            tree_reply(answer="b", tokens_used=30),
        ],
        claude=[
            reply(spawn_call(attempts=2, why="operator", tokens=60)),
            reply(spawn_call()),
            reply("never asked"),
        ],
    ),
    sprig(
        "spawn-bad-input",
        *SPAWN,
        "spawn",
        tree=[],
        claude=[
            reply(spawn_call(tokens=1.5)),
            reply(call("spawn", task="t")),
            reply(spawn_call(tools="read")),
            reply(spawn_call(profile=5)),
            reply("gave up"),
        ],
    ),
    sprig(
        "spawn-tree-fails",
        *SPAWN,
        "spawn",
        tree=[{"stdout": "not json", "stderr": "Traceback\nboom: it broke\n", "exit": 1}],
        claude=[reply(spawn_call()), reply("broken")],
    ),
    sprig(
        "spawn-tree-missing",
        "--tools",
        "read,spawn",
        "--tree-cmd",
        "no-such-tree-cmd",
        "--session",
        "s",
        "spawn",
        claude=[reply(spawn_call()), reply("missing")],
    ),
    sprig(
        "spawn-api-wire",
        *SPAWN,
        "--wire",
        "anthropic",
        "--base-url",
        "{API}",
        "spawn",
        tree=[tree_reply(answer="child ok", tokens_used=3)],
        api=[
            api_tools(("u1", "spawn", {"task": "t", "envelope": {"permissions": {}}, "tokens": 7})),
            api_text("done"),
        ],
    ),
    sprig(
        "spawn-openai-wire",
        *SPAWN,
        "--wire",
        "openai",
        "--base-url",
        "{API}",
        "--model",
        "qwen3-coder",
        "spawn",
        tree=[tree_reply(answer="child ok")],
        api=[
            oai_reply(calls=[("c1", "spawn", {"task": "t", "envelope": {"permissions": {}}})]),
            oai_reply("done"),
        ],
    ),
)

# --- sprig-hook -------------------------------------------------------------


def hook(name, payload, *args, **kw):
    return {"name": name, "bin": "sprig-hook", "args": list(args), "stdin": payload, **kw}


BASH_PAYLOAD = json.dumps({"tool_name": "Bash", "tool_input": {"command": "rm -rf /"}})
READ_PAYLOAD = json.dumps({"tool_name": "Read", "tool_input": {"file_path": "/etc/hosts"}})

add(
    hook("hook-allow", READ_PAYLOAD, "--warden-cmd", "fakegate check", warden=DENY_BASH),
    hook("hook-deny", BASH_PAYLOAD, "--warden-cmd", "fakegate check", warden=DENY_BASH),
    hook(
        "hook-deny-silent", BASH_PAYLOAD, "--warden-cmd", "fakegate", warden=[warden_rule(exit=1)]
    ),
    hook("hook-unreadable", "not json", "--warden-cmd", "fakegate", warden=ALLOW_ALL),
    hook("hook-empty-stdin", "", "--warden-cmd", "fakegate", warden=ALLOW_ALL),
    hook("hook-no-tool-name", '{"tool_input":{}}', "--warden-cmd", "fakegate", warden=ALLOW_ALL),
    hook(
        "hook-tool-name-wrong-type",
        '{"tool_name":5,"tool_input":{}}',
        "--warden-cmd",
        "fakegate",
        warden=ALLOW_ALL,
    ),
    hook(
        "hook-folded-dup-keys",
        '{"TOOL_NAME":"Read","tool_name":"Bash","Tool_Input":{"command":"x"},"tool_input":{"b":1}}',
        "--warden-cmd",
        "fakegate",
        warden=DENY_BASH,
    ),
    hook("hook-no-input", '{"tool_name":"write"}', "--warden-cmd", "fakegate", warden=ALLOW_ALL),
    hook(
        "hook-input-numbers",
        '{"tool_name":"Bash","tool_input":{"command":"ls","n":1e2,"big":12345678901234567890,'
        '"neg":-0,"s":"<&>\\u2028","dup":1,"dup":{"x":[]}}}',
        "--warden-cmd",
        "fakegate",
        warden=ALLOW_ALL,
    ),
    hook(
        "hook-warden-cmd-nbsp",
        READ_PAYLOAD,
        "--warden-cmd",
        "fakegate\u00a0check\u3000x",
        warden=ALLOW_ALL,
    ),
    hook("hook-default-cmd-no-daisugi", READ_PAYLOAD),
    hook(
        "hook-default-cmd-fake-daisugi",
        BASH_PAYLOAD,
        fakes=["daisugi"],
        warden=DENY_BASH,
    ),
    hook(
        "hook-timeout",
        READ_PAYLOAD,
        "--warden-cmd",
        "fakegate",
        "--warden-timeout",
        "1",
        warden=[warden_rule(sleep=3)],
    ),
    hook(
        "hook-bad-timeout-ignored",
        READ_PAYLOAD,
        "--warden-timeout",
        "zero",
        "--warden-cmd",
        "fakegate",
        "--warden-timeout",
        "-4",
        "--unknown",
        warden=ALLOW_ALL,
    ),
    hook("hook-trailing-warden-cmd", READ_PAYLOAD, "--warden-cmd"),
)

# --- sprig-mcp --------------------------------------------------------------


def mcp(name, lines, *args, default_warden=False, **kw):
    stdin = "".join(line + "\n" for line in lines) if isinstance(lines, list) else lines
    return {
        "name": name,
        "bin": "sprig-mcp",
        "args": _warden_off(args, default_warden),
        "stdin": stdin,
        **kw,
    }


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
        "mcp-warden",
        [tcall(1, "bash", {"cmd": "echo hi"}), tcall(2, "read", {"path": "r"})],
        "--warden",
        "--warden-cmd",
        "fakegate",
        files={"r": "R"},
        warden=DENY_BASH,
    ),
    mcp(
        "mcp-warden-cmd-without-warden",
        [tcall(1, "bash", {"cmd": "echo free"})],
        "--warden-cmd",
        "fakegate",
        warden=[warden_rule(exit=1)],
    ),
    mcp(
        "mcp-warden-timeout",
        [tcall(1, "read", {"path": "r"})],
        "--warden",
        "--warden-cmd",
        "fakegate",
        "--warden-timeout",
        "1",
        warden=[warden_rule(sleep=3)],
    ),
    mcp("mcp-warden-default-no-daisugi", [tcall(1, "read", {"path": "r"})], "--warden"),
)

# --- weave ------------------------------------------------------------------


def weave(name, *args, default_warden=False, **kw):
    return {"name": name, "bin": "weave", "args": _warden_off(args, default_warden), **kw}


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
        "weave-warden",
        "--warden",
        "--warden-cmd",
        "fakegate",
        "w.json",
        files={"w.json": wf([{"id": "a", "task": "t"}])},
        warden=DENY_BASH,
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

# --- the real daisugi warden --------------------------------------------------

add(
    sprig(
        "real-warden-loop",
        "--warden",
        "--session-dir",
        "{SESS}",
        "--session",
        "real",
        "do it",
        real_warden=True,
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
    hook("real-warden-hook-deny", BASH_PAYLOAD, real_warden=True),
    hook(
        "real-warden-hook-allow",
        json.dumps({"tool_name": "Bash", "tool_input": {"command": "ls"}}),
        real_warden=True,
    ),
    hook(
        "real-warden-hook-sprig-names",
        json.dumps({"tool_name": "read", "tool_input": {"path": "/etc/hosts"}}),
        real_warden=True,
    ),
    mcp(
        "real-warden-mcp",
        [
            tcall(1, "write", {"path": "m.txt", "content": "M"}),
            tcall(2, "bash", {"cmd": "rm -rf /"}),
            tcall(3, "read", {"path": "/etc/passwd"}),
        ],
        "--warden",
        real_warden=True,
    ),
    weave(
        "real-warden-weave",
        "--warden",
        "w.json",
        real_warden=True,
        files={"w.json": wf([{"id": "a", "task": "t"}])},
        claude=[reply(call("bash", cmd="rm -rf ~")), reply("stopped")],
    ),
)


# --- --mcp-config: sprig as an MCP client -------------------------------------
# sprig starts each stdio server in a group of its own, offers its tools as
# mcp__<server>__<tool>, relays the calls, and kills the group at the end. A
# tool error or a dead server is a tool result. On the claude-code wire claude
# runs the servers, from the same JSON. The fake server is on PATH as fakemcp.

MCP_TOOLS = [
    {
        "name": "echo",
        "description": "Echo the arguments.",
        "inputSchema": {
            "type": "object",
            "properties": {"text": {"type": "string"}, "n": {"type": "integer"}},
            "required": ["text"],
        },
    },
    {"name": "fail", "description": "Always fails.", "inputSchema": {"type": "object"}},
    {"name": "die", "description": "Ends the server.", "inputSchema": {"type": "object"}},
    {"name": "bare"},
]
MCP_CALLS = {
    "echo": {"echo": True},
    "fail": {"text": "it broke", "error": True},
    "die": {"die": True},
}
MCP_SCRIPT = {"tools": MCP_TOOLS, "calls": MCP_CALLS}


def mcp_cfg(**server):
    server = {"command": "fakemcp", "args": ["--flag"], **server}
    return json.dumps({"mcpServers": {"fk": server}})


MCP_CFG = mcp_cfg(env={"KEEP_ME": "1", "ANTHROPIC_API_KEY": "leak"})
ANTH = ("--wire", "anthropic", "--base-url", "{API}")
ECHO = ("u1", "mcp__fk__echo", {"text": "hi", "n": 2})

add(
    sprig(
        "mcp-anthropic-list-and-call",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[api_tools(ECHO), api_text("done")],
    ),
    sprig(
        "mcp-openai-list-and-call",
        *OAI,
        "--model",
        "m1",
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        mcp=MCP_SCRIPT,
        api=[oai_reply(calls=[("c1", "mcp__fk__echo", {"text": "hi", "n": 2})]), oai_reply("done")],
    ),
    sprig(
        "mcp-with-builtin-tools",
        *ANTH,
        "--tools",
        "read",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        files={"a.txt": "A"},
        api=[
            api_tools(("u1", "read", {"path": "a.txt"}), ECHO),
            api_text("done"),
        ],
    ),
    sprig(
        "mcp-config-file",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        "cfg.json",
        "echo hi",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        files={"cfg.json": MCP_CFG},
        api=[api_tools(ECHO), api_text("done")],
    ),
    sprig(
        "mcp-error-result",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[api_tools(("u1", "mcp__fk__fail", {})), api_text("it failed")],
    ),
    sprig(
        "mcp-rpc-error-result",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp={**MCP_SCRIPT, "calls": {"fail": {"rpc_error": "no such thing"}}},
        api=[api_tools(("u1", "mcp__fk__fail", {})), api_text("it failed")],
    ),
    sprig(
        "mcp-server-dies-mid-run",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[
            api_tools(("u1", "mcp__fk__die", {})),
            api_tools(ECHO),
            api_text("the server is gone"),
        ],
    ),
    sprig(
        "mcp-non-stdio-refused",
        "--mcp-config",
        json.dumps({"mcpServers": {"web": {"type": "http", "url": "http://127.0.0.1:1/mcp"}}}),
        "go",
        claude=[reply("never")],
    ),
    sprig(
        "mcp-no-command-refused",
        "--mcp-config",
        json.dumps({"mcpServers": {"web": {"args": ["x"]}}}),
        "go",
        claude=[reply("never")],
    ),
    sprig("mcp-bad-json", "--mcp-config", "{nope", "go", claude=[reply("never")]),
    sprig("mcp-missing-file", "--mcp-config", "nope.json", "go", claude=[reply("never")]),
    sprig("mcp-no-servers-key", "--mcp-config", '{"servers": {}}', "go", claude=[reply("never")]),
    sprig(
        "mcp-start-failure",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        mcp_cfg(command="no-such-server"),
        "go",
        env=API_ENV,
        api=[api_text("never")],
    ),
    sprig(
        "mcp-group-killed-at-end",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp={**MCP_SCRIPT, "child": True},
        api=[api_tools(ECHO), api_text("done")],
    ),
    sprig(
        "mcp-group-killed-on-failure",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp={**MCP_SCRIPT, "child": True},
        api=[api_reply("no", status=500)],
    ),
    sprig(
        "mcp-unlisted-tool-fails-run",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[api_tools(("u1", "bash", {"cmd": "ls"})), api_text("never")],
    ),
    sprig(
        "mcp-claude-code-wire",
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        mcp=MCP_SCRIPT,
        claude=[reply("done")],
    ),
    sprig(
        "mcp-claude-code-wire-file",
        "--mcp-config",
        "cfg.json",
        "go",
        files={"cfg.json": MCP_CFG},
        claude=[reply("done")],
    ),
    sprig(
        "mcp-key-stays-out",
        *OAI,
        "--model",
        "m1",
        "--key-fd",
        "4",
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        mcp=MCP_SCRIPT,
        fds={"4": "sk-SENTINEL-mcp\n"},
        sentinel="sk-SENTINEL-mcp",
        api=[oai_reply(calls=[("c1", "mcp__fk__echo", {"text": "hi"})]), oai_reply("done")],
    ),
)

# --- the MCP client against a misbehaving server ------------------------------
# Noise on stdout, answers to other ids, requests from the server, a silent
# server (30 s), a message over 32 MiB, and a server that sends requests and
# never reads the replies. Each is a tool result or a refusal, never a crash,
# and nothing outlives the run.


def mcp_run(name, script, calls, **kw):
    """One run on the anthropic wire that calls the given MCP tools."""
    return sprig(
        name,
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp={**MCP_SCRIPT, **script},
        api=[api_tools(*[(f"u{i}", n, a) for i, (n, a) in enumerate(calls)]), api_text("done")],
        **kw,
    )


add(
    mcp_run("mcp-noise-on-stdout", {"noise": True}, [("mcp__fk__echo", {"text": "hi"})]),
    mcp_run("mcp-answers-to-other-ids", {"stray": True}, [("mcp__fk__echo", {"text": "hi"})]),
    mcp_run(
        "mcp-server-request-is-answered",
        {"srv_request": True},
        [("mcp__fk__echo", {"text": "hi"})],
    ),
    mcp_run(
        "mcp-silent-server-times-out",
        {"calls": {**MCP_CALLS, "fail": {"hang": True}}, "child": True},
        [("mcp__fk__fail", {})],
        timeout=100,
    ),
    mcp_run(
        "mcp-message-over-32-mib",
        {"calls": {**MCP_CALLS, "fail": {"big": True}}, "child": True},
        [("mcp__fk__fail", {})],
        timeout=100,
    ),
    mcp_run(
        "mcp-server-that-never-reads-replies",
        {"calls": {**MCP_CALLS, "fail": {"flood": 3000}}, "child": True},
        [("mcp__fk__fail", {})],
        timeout=100,
    ),
    sprig(
        "mcp-duplicate-and-empty-tool-names",
        *ANTH,
        "--tools",
        "none",
        "--mcp-config",
        MCP_CFG,
        "go",
        env=API_ENV,
        mcp={
            **MCP_SCRIPT,
            "tools": [
                *MCP_TOOLS,
                {"name": "echo", "description": "again"},
                {"name": "", "description": "nameless"},
            ],
        },
        api=[api_text("done")],
    ),
)


# --- the cache marks of a system file -------------------------------------------
# On the anthropic wire a --system-file is one system block and carries a
# cache mark. When the run can call a tool (the wall's, or an MCP server's),
# the last block of the request carries a mark too, and it moves with each
# tool round, so the next call of the loop reads everything before it. A
# one-shot call (--tools none, no MCP) marks the system block only; a run
# with no system file marks nothing. The other wires send no mark.

SYS = {"sys.txt": "You are the steward."}

add(
    sprig(
        "sysmark-one-shot",
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        "--json",
        "-",
        stdin="what now?",
        files=SYS,
        env=API_ENV,
        api=[api_text("wait")],
    ),
    sprig(
        "sysmark-mcp",
        *ANTH,
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        files=SYS,
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[api_tools(ECHO), api_text("done")],
    ),
    sprig(
        "sysmark-tool-loop",
        "--tools",
        "read",
        "--system-file",
        "sys.txt",
        "-",
        stdin="what now?",
        files={**SYS, "a.txt": "A"},
        env=API_ENV,
        api=[
            api_tools(("u1", "read", {"path": "a.txt"})),
            api_tools(("u2", "read", {"path": "a.txt"})),
            api_text("wait"),
        ],
    ),
    sprig(
        "sysmark-no-system-file",
        "--tools",
        "read",
        "-",
        stdin="what now?",
        files={"a.txt": "A"},
        env=API_ENV,
        api=[api_tools(("u1", "read", {"path": "a.txt"})), api_text("wait")],
    ),
    sprig(
        "sysmark-empty-system-file",
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        "--mcp-config",
        MCP_CFG,
        "echo hi",
        files={"sys.txt": ""},
        env=API_ENV,
        mcp=MCP_SCRIPT,
        api=[api_tools(ECHO), api_text("done")],
    ),
    sprig(
        "sysmark-empty-task",
        "--tools",
        "read",
        "--system-file",
        "sys.txt",
        "-",
        stdin="",
        files=SYS,
        env=API_ENV,
        api=[api_text("wait")],
    ),
    sprig(
        "sysmark-cache-usage",
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        "--json",
        "-",
        stdin="what now?",
        files=SYS,
        env=API_ENV,
        api=[
            api_text_usage(
                "wait",
                {
                    "input_tokens": 3,
                    "output_tokens": 4,
                    "cache_read_input_tokens": 900,
                    "cache_creation_input_tokens": 0,
                },
            )
        ],
    ),
    sprig(
        "sysmark-openai",
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        *OAI,
        "--model",
        "m1",
        "--json",
        "-",
        stdin="what now?",
        files=SYS,
        api=[oai_reply("wait")],
    ),
    sprig(
        "sysmark-claude-code",
        "--tools",
        "none",
        "--system-file",
        "sys.txt",
        "--model",
        "haiku",
        "--json",
        "-",
        stdin="what now?",
        files=SYS,
        claude=[reply("wait")],
    ),
)


# --- the help tool ---------------------------------------------------------

HELP_FILE = "\n".join(
    [
        '{"type":"session","v":1,"id":"h1","harness":"sprig","cwd":"/x","ts":1.5}',
        '{"type":"prompt","id":"aaaa0001","parentId":null,"ts":2,"text":"first task"}',
        '{"type":"assistant","id":"aaaa0002","parentId":"aaaa0001","ts":3,"model":"haiku",'
        '"text":"","toolUses":[{"id":"t1","name":"help"}],"usage":{}}',
        '{"type":"tool_call","id":"aaaa0003","parentId":"aaaa0002","ts":4,"toolUseId":"t1",'
        '"name":"help","input":{"kind":"need_info","text":"which db?"}}',
        '{"type":"verdict","id":"aaaa0004","parentId":"aaaa0003","ts":5,"toolUseId":"t1",'
        '"decision":"allow"}',
        '{"type":"tool_result","id":"aaaa0005","parentId":"aaaa0004","ts":6,"toolUseId":"t1",'
        '"ok":true,"summary":"Your question was sent. The run pauses here. The answer comes as the next message."}',
    ]
)
HELP_SESS = ("--session-dir", "{SESS}", "--session", "h1")


def helpcall(kind, text="which db?"):
    return call("help", kind=kind, text=text)


add(
    *[
        sprig(
            f"help-claude-{kind}",
            "--json",
            "--tools",
            "read,help",
            *HELP_SESS,
            "task",
            claude=[reply(helpcall(kind)), reply("never asked")],
        )
        for kind in ("need_info", "escalate", "give_up")
    ],
    sprig(
        "help-claude-plain-output",
        "--tools",
        "help",
        "task",
        claude=[reply(helpcall("need_info", "plain?"))],
    ),
    sprig(
        "help-api-escalate",
        "--json",
        "--tools",
        "read,help",
        "task",
        env=API_ENV,
        api=[api_tools(("u1", "help", {"kind": "escalate", "text": "owner call"}))],
    ),
    sprig(
        "help-openai-give-up",
        *OAI,
        "--model",
        "m1",
        "--json",
        "--tools",
        "help",
        "task",
        api=[oai_reply(calls=[("c1", "help", {"kind": "give_up", "text": "cannot do it"})])],
    ),
    sprig(
        "help-bad-kind",
        "--json",
        "--tools",
        "help",
        "task",
        claude=[reply(helpcall("shout")), reply("ok after error")],
    ),
    sprig(
        "help-empty-text",
        "--json",
        "--tools",
        "help",
        "task",
        claude=[reply(helpcall("need_info", "  ")), reply("ok after error")],
    ),
    sprig(
        "help-twice",
        "--json",
        "--tools",
        "help",
        "task",
        claude=[
            reply(
                f"{TOOL_FENCE}\n"
                + json.dumps({"tool": "help", "input": {"kind": "need_info", "text": "a"}})
                + "\n```"
            ),
            reply("never"),
        ],
    ),
    sprig(
        "help-not-in-wall",
        "--tools",
        "read",
        "task",
        claude=[reply(helpcall("need_info")), reply("ok")],
    ),
    sprig(
        "help-with-warden-down",
        "--json",
        "--warden",
        "--tools",
        "help",
        "task",
        claude=[reply(helpcall("need_info"))],
        fakes=["claude", "fakegate", "daisugi"],
        warden=[warden_rule(exit=2, stderr="no")],
    ),
    sprig(
        "help-resume-new-session",
        "--json",
        "--tools",
        "help",
        "--session-dir",
        "{SESS}",
        "--resume",
        "h1",
        "--session",
        "h2",
        "sqlite",
        sessions={"h1.jsonl": HELP_FILE},
        claude=[reply("used sqlite")],
    ),
    sprig(
        "help-resume-api",
        "--json",
        "--tools",
        "help",
        "--session-dir",
        "{SESS}",
        "--resume",
        "h1",
        "--session",
        "h2",
        "sqlite",
        sessions={"h1.jsonl": HELP_FILE},
        env=API_ENV,
        api=[api_text("used sqlite via api")],
    ),
    sprig("help-tools-unknown", "--tools", "help,grep", "task", claude=[reply("never")]),
)


def by_name() -> dict[str, dict]:
    out: dict[str, dict] = {}
    for c in CASES:
        if c["name"] in out:
            raise ValueError(f"duplicate case {c['name']}")
        out[c["name"]] = c
    return out

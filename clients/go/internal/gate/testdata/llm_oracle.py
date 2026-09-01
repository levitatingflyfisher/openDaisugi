"""Run the oracle's llm_check against a fake Anthropic server.

    PYTHONPATH=src python llm_oracle.py OUT.jsonl

Each row: {"env": the variables set for the call ("{BASE}" stands for the
fake's URL), "rule", "payload", "status" and "body" the fake answers with,
"result": {satisfied, reason, errored}, "requests": what the fake got}.
The Go test (llm_test.go) replays every row against its own fake and
checks it sends the same requests and returns the same result. Never a
real model: every URL is the fake's.
"""
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

os.environ["HOME"] = "/home/user"
# No claude on PATH: auto-detection must never pick the claude-code backend,
# which would start a real agent.
os.environ["PATH"] = "/usr/bin:/bin"
CLEAR = ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_BASE", "ANTHROPIC_BASE_URL",
         "OPENDAISUGI_LLM_CHECK_MODEL", "OPENDAISUGI_LLM_BACKEND")
for k in CLEAR:
    os.environ.pop(k, None)

state = {"status": 200, "body": "", "log": []}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        body = self.rfile.read(n).decode()
        skip = ("host", "content-length", "connection", "accept-encoding")
        hdr = {k.lower(): v for k, v in self.headers.items() if k.lower() not in skip}
        state["log"].append({"path": self.path, "headers": hdr, "body": body})
        raw = state["body"].encode()
        self.send_response(state["status"])
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
BASE = f"http://127.0.0.1:{srv.server_address[1]}"

from opendaisugi.llm_check import run_llm_check  # noqa: E402


def msg(*texts, extra=None, **over):
    blocks = [{"type": "text", "text": t} for t in texts]
    if extra:
        blocks.append(extra)
    body = {"id": "msg_1", "type": "message", "role": "assistant", "model": "m", "content": blocks,
            "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 1}}
    body.update(over)
    return json.dumps(body)


ERR = '{"type":"error","error":{"type":"x","message":"boom"}}'
KEY = {"ANTHROPIC_API_KEY": "sk-test", "ANTHROPIC_API_BASE": "{BASE}", "OPENDAISUGI_LLM_BACKEND": "litellm"}
PAYLOAD = {"task": "t", "steps": [{"id": "s0", "type": "shell", "command": "ls é ✓ 😀"}]}
rows = []


def add(status, body, env=None, rule="be nice ✓", payload=None):
    rows.append((env or KEY, rule, payload or PAYLOAD, status, body))


for st in (400, 401, 402, 403, 404, 405, 408, 409, 410, 413, 418, 422, 429, 451, 500, 501, 502, 503, 504, 505, 520,
           529, 599, 300):
    add(st, ERR)
for text in ("overloaded_error here", "Overloaded", "prompt is too long: 300000 tokens", "prompt: length",
             "Invalid API Key", "content filtering policy", "Client error '400 Bad Request'", "", "plain text",
             "sk-abcdefghijklmnopqrstuvwxyz0123", '{"error": "api_key=supersecretvalue123"}', "Request timed out",
             "Bearer abcdefghijklmnop", "exceed context limit", "PASSWORD: hunter22 and x_secret=1",
             "EXCEED CONTEXT LIMIT", "é ✓ café", "The read operation timed out"):
    add(400, text)
    add(500, text)
    add(403, text)
for v in ('{"satisfied": true, "rationale": "fine"}', '{"satisfied": false, "rationale": "no"}',
          '{"satisfied": "yes"}', '{"satisfied": 0, "rationale": 5}', '{"satisfied": null, "rationale": null}',
          '{"rationale": [1, "a", {"b": 2.5}]}', '{"satisfied": 1, "rationale": {"k": true, "n": null}}',
          '[]', '[true]', '"str"', '3', '1.5', 'null', 'true', '', ' ', 'I think so', '{"satisfied": tru}', '{"a" 1}',
          '{"a": 1,}', '{"a": 1', '{"satisfied": true} extra', '{\n  "satisfied": true,\n  "rationale": "x"\n}',
          '{"satisfied": true, "rationale": "\\u00e9 é 😀"}', '{"satisfied": NaN}', '{"a": "\\x"}',
          '{"a": "b\nc"}', "{'satisfied': true}", '{"satisfied": true, "satisfied": false}',
          '```json\n{"satisfied": true}\n```', '{"satisfied": [], "rationale": 1e400}',
          '{"satisfied": {}, "rationale": -0.0}', '{"satisfied": "", "rationale": "\\ud800"}'):
    add(200, msg(v))
add(200, msg('{"satisfied": ', 'true}'))
add(200, msg('{"satisfied": true}', extra={"type": "thinking", "thinking": "hm", "signature": "s"}))
add(200, msg())
add(200, "not json")
add(200, "")
add(200, "not json sk-abcdefghijklmnopqrstuvwxyz0123")
full = json.loads(msg('{"satisfied": true, "rationale": "r"}'))
for drop in ("id", "type", "role", "stop_sequence"):
    add(200, json.dumps({k: v for k, v in full.items() if k != drop}))
for k, v in (("usage", {}), ("stop_reason", None), ("model", 5), ("id", None), ("stop_reason", "max_tokens")):
    add(200, json.dumps({**full, k: v}))
add(200, json.dumps({**full, "usage": {"input_tokens": 3, "output_tokens": 2, "cache_creation_input_tokens": 0,
                                       "cache_read_input_tokens": 0, "service_tier": "standard",
                                       "cache_creation": {"ephemeral_5m_input_tokens": 0}}}))
# Credentials and URLs.
ok = msg('{"satisfied": true, "rationale": "ok"}')
add(200, ok, env={"ANTHROPIC_AUTH_TOKEN": "tok-1", "ANTHROPIC_API_BASE": "{BASE}", "OPENDAISUGI_LLM_BACKEND": "litellm"})
add(200, ok, env={"ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "tok-2", "ANTHROPIC_API_BASE": "{BASE}"})
add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_AUTH_TOKEN": "t", "ANTHROPIC_BASE_URL": "{BASE}"})
add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_API_BASE": "{BASE}/v1/messages"})
add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_API_BASE": "{BASE}/"})
add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_API_BASE": "", "ANTHROPIC_BASE_URL": "{BASE}/x"})
add(200, ok, env={**KEY, "OPENDAISUGI_LLM_CHECK_MODEL": "anthropic/claude-sonnet-4-5"})
add(200, ok, env={**KEY, "OPENDAISUGI_LLM_BACKEND": "something-else"})
add(200, ok, env={"ANTHROPIC_API_BASE": "{BASE}"})
add(200, ok, env={"ANTHROPIC_API_KEY": "", "ANTHROPIC_API_BASE": "{BASE}", "OPENDAISUGI_LLM_BACKEND": "litellm"})
add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_API_BASE": "http://127.0.0.1:1"})
add(200, ok, env={"ANTHROPIC_AUTH_TOKEN": "", "ANTHROPIC_API_BASE": "{BASE}", "OPENDAISUGI_LLM_BACKEND": "litellm"})
for base in ("{BASE}//", "{BASE}/x/", "{BASE}/v1/messages/", "{BASE}/V1/MESSAGES", "{BASE}/v1"):
    add(200, ok, env={"ANTHROPIC_API_KEY": "k", "ANTHROPIC_API_BASE": base})
# Payloads and rules.
add(200, ok, rule="", payload={"task": "t", "steps": []})
add(200, ok, rule="line\nbreak \"q\"", payload={"task": "x" * 5000, "steps": [{"n": 1.5, "b": True, "z": None}]})
add(200, ok, payload={"task": "é" * 3999 + "😀", "steps": [{"f": float("nan"), "i": float("inf")}]})

with open(sys.argv[1], "w", encoding="utf-8") as f:
    for env, rule, payload, status, body in rows:
        for k in CLEAR:
            os.environ.pop(k, None)
        for k, v in env.items():
            os.environ[k] = v.replace("{BASE}", BASE)
        state.update(status=status, body=body, log=[])
        r = run_llm_check(rule, payload)
        reqs = [dict(q, body=q["body"].replace(BASE, "{BASE}")) for q in state["log"]]
        f.write(json.dumps({"env": env, "rule": rule, "payload": payload, "status": status, "body": body,
                            "result": {"satisfied": r.satisfied, "reason": r.reason.replace(BASE, "{BASE}"),
                                       "errored": r.errored},
                            "requests": reqs}) + "\n")
print(len(rows), "rows")

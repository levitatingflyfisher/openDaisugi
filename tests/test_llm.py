"""Tests for opendaisugi.llm and opendaisugi.llm_client: the model-client
factory, error translation, and the HTTP client's request bytes, re-ask
loop and error texts. Every server here is a fake on 127.0.0.1."""

import asyncio
import http.server
import json
import socket
import threading
import time

import pytest
from pydantic import BaseModel

from opendaisugi import llm_client
from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.llm import _redact_keys, get_model_client, translate_llm_error
from opendaisugi.llm_client import (
    CUT_TEXT,
    ModelCallError,
    ModelClient,
    complete,
    json_mode_system,
    request_body,
    resolve_timeout,
    resolve_wire,
)

# ---------------------------------------------------------------------------
# A fake model server
# ---------------------------------------------------------------------------


class _Fake:
    """Answers each POST with the next (status, body) and records requests."""

    def __init__(self, answers):
        self.answers = list(answers)
        self.requests = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self):  # noqa: N802
                n = int(self.headers.get("content-length", 0))
                body = self.rfile.read(n)
                outer.requests.append(
                    {
                        "path": self.path,
                        "headers": {k.lower(): v for k, v in self.headers.items()},
                        "body": body,
                    }
                )
                status, out, sleep = outer.answers.pop(0)
                if sleep:
                    time.sleep(sleep)
                raw = out.encode()
                try:
                    self.send_response(status)
                    self.send_header("content-type", "application/json")
                    self.send_header("content-length", str(len(raw)))
                    self.end_headers()
                    self.wfile.write(raw)
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *a):
                pass

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


@pytest.fixture
def fake():
    made = []

    def make(*answers):
        f = _Fake([a if len(a) == 3 else (*a, 0) for a in answers])
        made.append(f)
        return f

    yield make
    for f in made:
        f.close()


def _msg(text, stop="end_turn"):
    return json.dumps(
        {
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "content": [{"type": "text", "text": text}],
            "stop_reason": stop,
            "usage": {"input_tokens": 3, "output_tokens": 4},
        }
    )


def _chat(text, finish="stop"):
    return json.dumps(
        {
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": text},
                    "finish_reason": finish,
                }
            ],
            "usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
        }
    )


@pytest.fixture(autouse=True)
def _no_env_keys(monkeypatch):
    for k in (
        "ANTHROPIC_API_KEY",
        "ANTHROPIC_AUTH_TOKEN",
        "ANTHROPIC_API_BASE",
        "ANTHROPIC_BASE_URL",
        "OPENAI_API_KEY",
        "OPENAI_API_BASE",
        "OPENAI_BASE_URL",
        "OLLAMA_API_BASE",
        "OPENDAISUGI_LLM_TIMEOUT",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "http_proxy",
        "https_proxy",
        "all_proxy",
    ):
        monkeypatch.delenv(k, raising=False)


# ---------------------------------------------------------------------------
# The factory and error translation
# ---------------------------------------------------------------------------


def test_get_model_client_returns_the_http_client(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
    client = get_model_client(model="anthropic/claude-sonnet-4-20250514")
    assert isinstance(client, ModelClient)
    assert hasattr(client.chat.completions, "create")


def test_translate_any_exception_becomes_envelope_error():
    translated = translate_llm_error(RuntimeError("upstream failure"))
    assert isinstance(translated, EnvelopeGenerationError)
    assert "upstream failure" in str(translated)


def test_translate_preserves_envelope_error_unchanged():
    original = EnvelopeGenerationError("already translated")
    assert translate_llm_error(original) is original


def test_translate_usage_in_try_except():
    with pytest.raises(EnvelopeGenerationError) as exc:
        try:
            raise RuntimeError("simulated failure")
        except Exception as e:
            raise translate_llm_error(e) from e
    assert isinstance(exc.value.__cause__, RuntimeError)


def test_redact_keys_scrubs_anthropic_key():
    msg = "Invalid API key: sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234 was rejected"
    red = _redact_keys(msg)
    assert "abcdefghijklmnopqrstuvwxyz" not in red
    assert "sk-ant-" in red


def test_redact_keys_leaves_non_key_strings_alone():
    assert _redact_keys("rate limit exceeded") == "rate limit exceeded"


def test_translate_llm_error_redacts_key_in_message():
    exc = RuntimeError("auth failed for key sk-ant-api03-abcdefghijklmnopqrstuvwxyz1234")
    assert "abcdefghijklmnopqrstuvwxyz" not in str(translate_llm_error(exc))


# ---------------------------------------------------------------------------
# Wires, URLs, credentials, bodies
# ---------------------------------------------------------------------------


def test_the_wire_follows_the_model_name():
    env = {"ANTHROPIC_API_KEY": "k"}
    assert resolve_wire("anthropic/claude-x", env=env).kind == "messages"
    assert resolve_wire("anthropic/claude-x", env=env).model == "claude-x"
    assert resolve_wire("claude-3-haiku", env=env).model == "claude-3-haiku"
    assert resolve_wire("openai/gpt-4o", env={}).kind == "chat"
    assert resolve_wire("ollama/llama3.2:3b", env={}).model == "llama3.2:3b"
    assert resolve_wire("ollama_chat/qwen", env={}).model == "qwen"
    with pytest.raises(ModelCallError) as exc:
        resolve_wire("gpt-4o", env={})
    assert str(exc.value) == (
        "no model wire for 'gpt-4o': name it anthropic/<model>, openai/<model> or ollama/<model>"
    )


@pytest.mark.parametrize(
    "model,base,url",
    [
        ("anthropic/m", None, "https://api.anthropic.com/v1/messages"),
        ("anthropic/m", "http://h:1/", "http://h:1/v1/messages"),
        ("anthropic/m", "http://h:1/v1/messages/", "http://h:1/v1/messages"),
        ("anthropic/m", "http://h:1/V1/MESSAGES", "http://h:1/V1/MESSAGES/v1/messages"),
        ("openai/m", None, "https://api.openai.com/v1/chat/completions"),
        ("openai/m", "http://h:8080/v1", "http://h:8080/v1/chat/completions"),
        ("openai/m", "http://h:8080/v1/chat/completions", "http://h:8080/v1/chat/completions"),
        ("ollama/m", None, "http://localhost:11434/v1/chat/completions"),
        ("ollama/m", "http://h:11434/", "http://h:11434/v1/chat/completions"),
        ("ollama/m", "http://h:11434/v1", "http://h:11434/v1/chat/completions"),
    ],
)
def test_urls(model, base, url):
    assert resolve_wire(model, base_url=base, env={"ANTHROPIC_API_KEY": "k"}).url == url


def test_credentials():
    h = dict(resolve_wire("anthropic/m", env={"ANTHROPIC_API_KEY": "k"}).headers)
    assert h["x-api-key"] == "k" and "authorization" not in h
    h = dict(
        resolve_wire(
            "anthropic/m", env={"ANTHROPIC_API_KEY": "", "ANTHROPIC_AUTH_TOKEN": "t"}
        ).headers
    )
    assert h["authorization"] == "Bearer t" and "x-api-key" not in h
    with pytest.raises(
        ModelCallError, match="^no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set$"
    ):
        resolve_wire("anthropic/m", env={})
    assert "authorization" not in dict(
        resolve_wire("ollama/m", env={"OPENAI_API_KEY": "o"}).headers
    )
    h = dict(resolve_wire("openai/m", env={"OPENAI_API_KEY": "o"}).headers)
    assert h["authorization"] == "Bearer o"
    h = dict(resolve_wire("ollama/m", api_key="x", env={}).headers)
    assert h["authorization"] == "Bearer x"


def test_request_bodies_are_compact_ascii_json():
    w = resolve_wire("anthropic/m", env={"ANTHROPIC_API_KEY": "k"})
    msgs = [{"role": "system", "content": "S é"}, {"role": "user", "content": "U 😀"}]
    assert request_body(w, msgs, temperature=0, max_tokens=200) == (
        b'{"model":"m","max_tokens":200,"system":"S \\u00e9","messages":'
        b'[{"role":"user","content":"U \\ud83d\\ude00"}],"temperature":0}'
    )
    assert request_body(w, msgs[1:]) == (
        b'{"model":"m","max_tokens":8192,"messages":[{"role":"user","content":"U \\ud83d\\ude00"}]}'
    )
    thinking = {"type": "enabled", "budget_tokens": 16000}
    assert json.loads(request_body(w, msgs, thinking=thinking))["max_tokens"] == 24192
    c = resolve_wire("ollama/m", env={})
    assert request_body(c, msgs, json_object=True) == (
        b'{"model":"m","messages":[{"role":"system","content":"S \\u00e9"},'
        b'{"role":"user","content":"U \\ud83d\\ude00"}],"response_format":{"type":"json_object"}}'
    )


def test_timeouts():
    assert resolve_timeout(None, {}) == 600.0
    assert resolve_timeout(None, {"OPENDAISUGI_LLM_TIMEOUT": " 1.5 "}) == 1.5
    for bad in ("0", "-1", "nan", "inf", "x", ""):
        assert resolve_timeout(None, {"OPENDAISUGI_LLM_TIMEOUT": bad}) == 600.0
    assert resolve_timeout(3, {"OPENDAISUGI_LLM_TIMEOUT": "1"}) == 3.0


class _Tiny(BaseModel):
    """A tiny reply."""

    n: int


def test_json_mode_system_is_instructors_text():
    assert json_mode_system(_Tiny) == (
        "\n            As a genius expert, your task is to understand the content and provide\n"
        "            the parsed objects in json that match the following json_schema:\n\n\n"
        '            {\n  "description": "A tiny reply.",\n  "properties": {\n    "n": {\n'
        '      "title": "N",\n      "type": "integer"\n    }\n  },\n  "required": [\n'
        '    "n"\n  ],\n  "title": "_Tiny",\n  "type": "object"\n}\n\n'
        "            Make sure to return an instance of the JSON, not the schema itself\n"
    )


# ---------------------------------------------------------------------------
# Calls against the fake
# ---------------------------------------------------------------------------


def _structured(url, response_model, max_retries, model="anthropic/claude-x", **kw):
    client = ModelClient(env={"ANTHROPIC_API_KEY": "sk-test", "OPENDAISUGI_LLM_TIMEOUT": "5"})
    return asyncio.run(
        client.chat.completions.create(
            model=model,
            response_model=response_model,
            messages=[{"role": "system", "content": "S"}, {"role": "user", "content": "go"}],
            max_retries=max_retries,
            base_url=url,
            **kw,
        )
    )


def test_a_structured_call_sends_the_exact_request(fake):
    f = fake((200, _msg('{"n": 1}')))
    assert _structured(f.url, _Tiny, 0) == _Tiny(n=1)
    (req,) = f.requests
    assert req["path"] == "/v1/messages"
    for k, v in {
        "accept": "application/json",
        "accept-encoding": "identity",
        "content-type": "application/json",
        "user-agent": "opendaisugi",
        "anthropic-version": "2023-06-01",
        "x-api-key": "sk-test",
    }.items():
        assert req["headers"][k] == v
    body = json.loads(req["body"])
    assert body["system"] == "S\n\n" + json_mode_system(_Tiny)
    assert body["messages"] == [{"role": "user", "content": "go"}]


_NAN_REPLY = '{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}}'
_NAN_ERROR = (
    "1 validation error for Envelope\n"
    "permissions.velocity_limit\n"
    "  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n"
    "    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"
)


def test_a_reply_with_nan_is_reasked_then_refused(fake):
    # GD-16: NaN, Infinity or -Infinity in a reply is schema-invalid.
    from opendaisugi.models import Envelope

    f = fake((200, _msg(_NAN_REPLY)), (200, _msg(_NAN_REPLY)))
    with pytest.raises(ModelCallError) as exc:
        _structured(f.url, Envelope, 1)
    assert str(exc.value) == "ValidationError: 1 validation error for Envelope (2 attempts)"
    second = json.loads(f.requests[1]["body"])["messages"]
    assert second[1] == {"role": "assistant", "content": _NAN_REPLY}
    assert second[2]["content"] == (
        "Correct your JSON ONLY RESPONSE, based on the following errors:\n" + _NAN_ERROR
    )


def test_a_reply_after_a_reask_is_the_callers_own_model(fake):
    from opendaisugi.models import Envelope

    good = '{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": 1e3}}'
    f = fake((200, _msg(_NAN_REPLY.replace("NaN", "-Infinity"))), (200, _msg(good)))
    env = _structured(f.url, Envelope, 1)
    assert type(env) is Envelope
    assert env.permissions.velocity_limit == 1000.0


def test_default_retries_make_four_attempts(fake):
    f = fake(*[(200, _msg("no"))] * 4)
    with pytest.raises(ModelCallError, match=r"\(4 attempts\)$"):
        _structured(f.url, _Tiny, 3)
    assert len(f.requests) == 4


def test_a_cut_reply_fails_at_once(fake):
    f = fake((200, _msg('{"n":', stop="max_tokens")))
    with pytest.raises(ModelCallError, match=f"^{CUT_TEXT}$"):
        _structured(f.url, _Tiny, 3)
    assert len(f.requests) == 1


def test_an_http_error_after_a_bad_reply_is_its_own_text(fake):
    f = fake((200, _msg("{}")), (500, '{"type":"error"}'))
    with pytest.raises(ModelCallError) as exc:
        _structured(f.url, _Tiny, 3)
    assert str(exc.value) == 'the model server answered HTTP 500: {"type":"error"}'


def test_an_error_body_is_cut_and_keys_are_shortened(fake):
    f = fake((401, "bad key sk-abcdefghijklmnopqrstuvwxyz0123 " + "x" * 600))
    with pytest.raises(ModelCallError) as exc:
        _structured(f.url, _Tiny, 0)
    text = str(exc.value)
    assert "sk-abcd...0123" in text and "efghijkl" not in text
    assert text.startswith("the model server answered HTTP 401: bad key")


def test_an_unreadable_reply(fake):
    f = fake((200, "not json"), (200, '{"content": 5}'))
    for _ in range(2):
        with pytest.raises(ModelCallError) as exc:
            _structured(f.url, _Tiny, 0)
        assert str(exc.value).startswith("the model reply could not be read: ")


def test_the_chat_wire(fake):
    f = fake((200, _chat('{"n": 2}')))
    assert _structured(f.url + "/v1", _Tiny, 0, model="ollama/qwen") == _Tiny(n=2)
    (req,) = f.requests
    assert req["path"] == "/v1/chat/completions"
    body = json.loads(req["body"])
    assert body["model"] == "qwen"
    assert body["response_format"] == {"type": "json_object"}
    assert body["messages"][0]["content"] == "S\n\n" + json_mode_system(_Tiny)
    assert "authorization" not in req["headers"]


def test_a_plain_call(fake):
    f = fake((200, _chat("hello", finish="length")))
    r = complete("openai/m", [{"role": "user", "content": "hi"}], base_url=f.url, max_tokens=7)
    assert (r.text, r.cut, r.tokens, r.input_tokens, r.output_tokens) == ("hello", True, 11, 5, 6)
    assert json.loads(f.requests[0]["body"]) == {
        "model": "m",
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 7,
    }


def test_a_refused_connection_and_a_timeout(fake):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    url = f"http://u:secret@127.0.0.1:{port}"
    with pytest.raises(ModelCallError) as exc:
        complete("ollama/m", [{"role": "user", "content": "hi"}], base_url=url)
    assert str(exc.value) == (
        f"could not reach the model server at http://127.0.0.1:{port}/v1/chat/completions"
    )
    f = fake((200, _chat("late"), 2))
    with pytest.raises(ModelCallError) as exc:
        complete("ollama/m", [{"role": "user", "content": "hi"}], base_url=f.url, timeout=0.3)
    assert str(exc.value) == f"the model call to {f.url}/v1/chat/completions timed out"


def test_the_client_prints_nothing(fake, capfd):
    f = fake((500, "boom"))
    with pytest.raises(ModelCallError):
        _structured(f.url, _Tiny, 0)
    out, err = capfd.readouterr()
    assert (out, err) == ("", "")


def test_llm_client_module_uses_no_litellm_or_instructor():
    import inspect

    src = inspect.getsource(llm_client)
    assert "import litellm" not in src and "import instructor" not in src

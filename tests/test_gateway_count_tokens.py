"""The count-tokens shim.

Claude Code calls POST /v1/messages/count_tokens before every turn. Ollama
and most self-hosted Anthropic-compatible servers do not implement it. The
Ollama docs list it as unsupported, and it has been observed to wedge the
server afterwards. When the configured upstream is not the real Anthropic
API, the gateway answers this one route itself. It uses the same chars/4
estimate the router already trusts for cache-prefix sizing,
gateway.estimate_prefix_tokens.
"""

from __future__ import annotations

import httpx
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.gateway import estimate_prefix_tokens
from opendaisugi.gateway_asgi import make_gateway_app
from opendaisugi.gateway_pipeline import Gateway

runner = CliRunner()

_BODY = {
    "model": "claude-opus-4-8",
    "max_tokens": 1024,
    "system": "you are a careful coding agent. " * 20,
    "messages": [{"role": "user", "content": "summarize the last tool result: " + ("x" * 380)}],
}


def _mock_upstream(handler):
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


async def _count_tokens(app, body=_BODY, content=None):
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        kwargs = {"content": content} if content is not None else {"json": body}
        return await client.post(
            "/v1/messages/count_tokens",
            params={"beta": "true"},
            headers={"authorization": "Bearer sk-oauth-XYZ", "content-type": "application/json"},
            **kwargs,
        )


async def test_count_tokens_is_answered_locally_when_upstream_is_not_anthropic():
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("the shim must never reach the upstream")

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="ollama",
        client=_mock_upstream(handler),
    )
    resp = await _count_tokens(app_)
    assert resp.status_code == 200
    # 1052 chars, the system prompt times 20 plus the user message, divided by 4.
    assert resp.json() == {"input_tokens": 263}
    assert resp.json()["input_tokens"] == estimate_prefix_tokens(_BODY)


async def test_count_tokens_shim_refuses_a_body_that_is_not_a_json_object():
    def handler(request: httpx.Request) -> httpx.Response:
        raise AssertionError("the shim must never reach the upstream")

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="ollama",
        client=_mock_upstream(handler),
    )
    resp = await _count_tokens(app_, content=b"not json")
    assert resp.status_code == 400
    assert resp.json()["type"] == "error"
    resp = await _count_tokens(app_, content=b"[1, 2]")
    assert resp.status_code == 400


async def test_count_tokens_passes_through_when_upstream_is_real_anthropic():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["path"] = request.url.path
        return httpx.Response(200, json={"input_tokens": 245})

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="anthropic",
        client=_mock_upstream(handler),
    )
    resp = await _count_tokens(app_)
    assert resp.status_code == 200
    # The request reached the upstream fake.
    assert seen["path"] == "/v1/messages/count_tokens"
    upstream_answer = resp.json()["input_tokens"]
    # The upstream's own answer, untouched.
    assert upstream_answer == 245
    estimate = estimate_prefix_tokens(_BODY)
    assert abs(estimate - upstream_answer) / upstream_answer <= 0.10


async def test_make_gateway_app_defaults_upstream_kind_to_anthropic():
    # The library default. The CLI derives upstream_kind by a join against
    # the recorded host, which the CLI tests below cover.
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"input_tokens": 42})

    app_ = make_gateway_app(
        Gateway(), upstream_base_url="http://up", client=_mock_upstream(handler)
    )
    resp = await _count_tokens(app_)
    # Untouched: the upstream's own answer.
    assert resp.json() == {"input_tokens": 42}


async def test_shim_never_touches_the_openai_wire():
    # Chat Completions has no count_tokens route. A request to it must
    # route through the normal pipeline, not the shim's early return.
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"id": "x", "choices": [], "usage": {}})

    app_ = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        upstream_kind="ollama",
        openai_gateway=Gateway(),
        client=_mock_upstream(handler),
    )
    transport = httpx.ASGITransport(app=app_)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
        resp = await client.post(
            "/v1/chat/completions",
            json={"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "hi"}]},
        )
    assert resp.status_code == 200
    # The request reached the mock upstream, not the shim.
    assert resp.json()["id"] == "x"


def test_gateway_help_mentions_upstream_kind():
    res = runner.invoke(app, ["gateway", "--help"])
    assert res.exit_code == 0
    assert "--upstream-kind" in res.output


def test_gateway_rejects_an_upstream_kind_outside_the_four_values(tmp_path, monkeypatch):
    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    called = []
    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", lambda **kw: called.append(kw))

    res = runner.invoke(app, ["gateway", "--upstream-kind", "anthropc"])
    assert res.exit_code == 1
    assert "anthropc" in res.output
    assert "anthropic-compatible" in res.output
    assert called == []


def _fake_serve_gateway_capturing(captured):
    def fake(**kwargs):
        captured.update(kwargs)

    return fake


def test_gateway_upstream_kind_stays_anthropic_when_upstream_is_not_the_recorded_host(
    tmp_path, monkeypatch
):
    """A recorded ollama host must never leak its kind onto an unrelated
    --upstream, and that includes the default, the real Anthropic API. A
    wrong join here would make the gateway shim count_tokens on a real
    account."""
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:11434", llm_host_kind="ollama"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    # The default --upstream is the real Anthropic API.
    res = runner.invoke(app, ["gateway"])
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "anthropic"


def test_gateway_upstream_kind_resolves_to_the_recorded_kind_when_upstream_matches(
    tmp_path, monkeypatch
):
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:11434", llm_host_kind="ollama"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    res = runner.invoke(app, ["gateway", "--upstream", "http://box:11434"])
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "ollama"


def test_gateway_upstream_kind_for_an_anthropic_compatible_host_keeps_the_shim_on(
    tmp_path, monkeypatch
):
    """A self-hosted Anthropic-compatible server is not the real API. Its
    recorded kind "anthropic" must not turn the shim off."""
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:4000", llm_host_kind="anthropic"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    res = runner.invoke(app, ["gateway", "--upstream", "http://box:4000/"])
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "anthropic-compatible"


def test_gateway_upstream_kind_flag_beats_the_recorded_host(tmp_path, monkeypatch):
    from opendaisugi.config import Config, save_config

    monkeypatch.setattr("pathlib.Path.home", lambda: tmp_path)
    (tmp_path / ".opendaisugi").mkdir()
    save_config(
        Config(llm_base_url="http://box:11434", llm_host_kind="ollama"),
        tmp_path / ".opendaisugi" / "config.yaml",
    )
    captured: dict = {}
    monkeypatch.setattr(
        "opendaisugi.gateway_asgi.serve_gateway", _fake_serve_gateway_capturing(captured)
    )

    res = runner.invoke(
        app, ["gateway", "--upstream", "http://box:11434", "--upstream-kind", "anthropic"]
    )
    assert res.exit_code == 0, res.output
    assert captured["upstream_kind"] == "anthropic"

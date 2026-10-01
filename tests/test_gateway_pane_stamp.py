"""A coppice pane's base URL carries its pane: <gateway>/pane/<floor>/<pane>.

The gateway takes the prefix off before it routes, so the upstream path is the
plain one, and it stamps the turn record with the pane and the floor. A path
with no well-formed prefix is routed as it is and the record has no pane. A
record with no pane has no pane or floor key in its journal line, so every
older journal line and fixture stays byte for byte the same.
"""

from __future__ import annotations

import json

import httpx
import pytest

from opendaisugi.gateway_asgi import make_gateway_app, split_pane_path
from opendaisugi.gateway_journal import GatewayJournal
from opendaisugi.gateway_pipeline import Gateway

FLOOR = "0123456789abcdef"

_JSON_REPLY = {
    "id": "msg_1",
    "type": "message",
    "role": "assistant",
    "model": "claude-haiku-4-5",
    "content": [{"type": "text", "text": "hi"}],
    "usage": {"input_tokens": 12, "output_tokens": 5},
}


@pytest.mark.parametrize(
    ("path", "want"),
    [
        (f"/pane/{FLOOR}/w1:p2/v1/messages", (FLOOR, "w1:p2", "/v1/messages")),
        (
            f"/pane/{FLOOR}/w12:p345/v1/chat/completions",
            (FLOOR, "w12:p345", "/v1/chat/completions"),
        ),
        (f"/pane/{FLOOR}/w1:p2", (FLOOR, "w1:p2", "/")),
        ("/v1/messages", ("", "", "/v1/messages")),
        (
            f"/pane/{FLOOR.upper()}/w1:p2/v1/messages",
            ("", "", f"/pane/{FLOOR.upper()}/w1:p2/v1/messages"),
        ),
        (
            f"/pane/{FLOOR[:15]}/w1:p2/v1/messages",
            ("", "", f"/pane/{FLOOR[:15]}/w1:p2/v1/messages"),
        ),
        (f"/pane/{FLOOR}/w1:p2x/v1/messages", ("", "", f"/pane/{FLOOR}/w1:p2x/v1/messages")),
        (f"/pane/{FLOOR}/w1:p/v1/messages", ("", "", f"/pane/{FLOOR}/w1:p/v1/messages")),
        (
            f"/pane/{FLOOR}/w1234567890:p1/v1/messages",
            ("", "", f"/pane/{FLOOR}/w1234567890:p1/v1/messages"),
        ),
        (f"/x/pane/{FLOOR}/w1:p2/v1/messages", ("", "", f"/x/pane/{FLOOR}/w1:p2/v1/messages")),
    ],
)
def test_split_pane_path(path: str, want: tuple[str, str, str]) -> None:
    assert split_pane_path(path) == want


async def _post(app, path: str) -> httpx.Response:
    body = {
        "model": "claude-opus-4-8",
        "max_tokens": 64,
        "messages": [{"role": "user", "content": "say hi"}],
    }
    async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app), base_url="http://gw") as c:
        return await c.post(
            path, content=json.dumps(body).encode(), headers={"authorization": "Bearer x"}
        )


async def test_the_prefix_is_stripped_and_the_record_names_the_pane(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.path)
        return httpx.Response(200, json=_JSON_REPLY)

    app = make_gateway_app(
        Gateway(journal=journal),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    resp = await _post(app, f"/pane/{FLOOR}/w1:p2/v1/messages")
    assert resp.status_code == 200
    assert seen == ["/v1/messages"]
    line = json.loads((tmp_path / "turns.jsonl").read_text().splitlines()[0])
    assert line["pane"] == "w1:p2" and line["floor"] == FLOOR
    rec = journal.load()[0]
    assert (rec.pane, rec.floor) == ("w1:p2", FLOOR)


async def test_a_path_with_no_prefix_leaves_no_pane_or_floor_key(tmp_path) -> None:
    journal = GatewayJournal(path=tmp_path / "turns.jsonl")

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json=_JSON_REPLY)

    app = make_gateway_app(
        Gateway(journal=journal),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    await _post(app, "/v1/messages")
    line = json.loads((tmp_path / "turns.jsonl").read_text().splitlines()[0])
    assert "pane" not in line and "floor" not in line
    assert journal.load()[0].pane is None


async def test_a_malformed_prefix_goes_upstream_as_it_is(tmp_path) -> None:
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.path)
        return httpx.Response(200, json=_JSON_REPLY)

    app = make_gateway_app(
        Gateway(),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
    )
    await _post(app, f"/pane/{FLOOR.upper()}/w1:p2/v1/messages")
    assert seen == [f"/pane/{FLOOR.upper()}/w1:p2/v1/messages"]


def _caps_call(path: str, method: str, client):
    import asyncio

    sent: list[dict] = []

    async def receive():
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message):
        sent.append(message)

    scope = {"type": "http", "path": path, "method": method, "headers": [], "client": client}
    asyncio.run(make_gateway_app(object())(scope, receive, send))
    return sent


def test_caps_says_the_gateway_understands_the_pane_prefix() -> None:
    """coppice stamps a pane's base URL only when the gateway says it can
    strip the stamp. The answer is local: loopback only, GET only, and it is
    never sent upstream, so an old gateway's silence is a clean 'no'."""
    sent = _caps_call("/_caps", "GET", ("127.0.0.1", 5))
    assert sent[0]["status"] == 200
    assert json.loads(sent[1]["body"]) == {"pane_prefix": True}
    # Behind a pane stamp it is the same route.
    sent = _caps_call(f"/pane/{FLOOR}/w1:p2/_caps", "GET", ("::1", 5))
    assert sent[0]["status"] == 200
    assert _caps_call("/_caps", "POST", ("127.0.0.1", 5))[0]["status"] == 405
    for client in (("10.0.0.4", 5), None, ()):
        assert _caps_call("/_caps", "GET", client)[0]["status"] == 403, client

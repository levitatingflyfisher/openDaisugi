# Spec 08 — The model-host route: this box first, any self-hosted box next

**Master:** §5.12, §5.7 (the count-tokens shim lives in the gateway) · **Size:** S · **Depends on:** nothing

## Purpose

`daisugi setup` already right-sizes a *local* model on this machine. Add the remote: a model
server on any box the operator owns, reached by `host:port` over Tailscale or the LAN, probed
honestly, and recorded so the backend, the gateway, and Switchyard all use the same fact.

## The crux

Local-first means the zero-config path is the machine in front of you, so the default stays the
laptop. A remote host is a swap, not a requirement, and it must be *probed* rather than trusted:
the endpoint kind, the model list, and the context window are facts we can check in one round
trip each, and a route that claims a 32K context on a 4K model wastes an afternoon.

## Files

```
src/opendaisugi/model_host.py       # probe(host, port) → HostInfo; context floor check; record()
src/opendaisugi/cli.py              # `daisugi setup --remote HOST[:PORT] [--kind auto|ollama|openai|anthropic] [--model M]`
src/opendaisugi/config.py           # llm_base_url: str|None; llm_host_kind: str|None; llm_context_window: int|None
src/opendaisugi/gateway_asgi.py     # /v1/messages/count_tokens answered locally (see below)
src/opendaisugi/modules.py          # backend stage shows `<host> (ollama, qwen3-coder, 32k)` when set
tests/test_model_host.py            # httpx-style fakes for each endpoint kind
tests/test_gateway_count_tokens.py
```

## Probe

```python
@dataclass(frozen=True)
class HostInfo:
    base_url: str; kind: str                  # ollama | openai | anthropic
    models: list[str]; chosen: str | None
    context_window: int | None; latency_ms: float; warnings: list[str]

def probe(host: str, port: int | None, *, kind: str = "auto", timeout_s: float = 3.0) -> HostInfo
```

Order for `auto`: `GET /api/tags` (Ollama) → `GET /v1/models` (OpenAI-compatible: llamafile,
llama.cpp server, vLLM, LM Studio) → `POST /v1/messages` with `max_tokens: 1` (Anthropic-compatible;
Ollama ≥ 0.14 answers this too). Context window: Ollama `POST /api/show {"name": m}` →
`model_info.*.context_length` and the `num_ctx` parameter if set; OpenAI-compatible → `/v1/models`
metadata when present; else `None` with a warning "context window unknown; set it with
`--context 32768` if you know it". Floor: if known and `< 32768`, warn "below the 32K floor
agentic coding needs; expect truncation".

`record()` writes `llm_base_url`, `llm_host_kind`, `llm_backend` (`ollama` | `llamafile` |
`openai-compatible` | `anthropic-compatible`), model, and context window to config; prints one
line per fact and the exact env a harness needs (`ANTHROPIC_BASE_URL=…` plus
`ANTHROPIC_AUTH_TOKEN=ollama` for the Ollama case).

## The count-tokens shim

Claude Code calls `POST /v1/messages/count_tokens?beta=true`. Ollama does not implement it and
its server has been observed to wedge afterwards. When the gateway proxies to a host whose
`kind != anthropic`, it answers that route itself with `{"input_tokens": estimate}` using
`gateway.estimate_prefix_tokens` on the body, and logs one DEBUG line. This is not a fake:
the response is an estimate and Claude Code uses it only for display and budgeting.

## Tests

- Probe: fakes for each endpoint kind in each order position; a host answering nothing → `HostInfo`
  with `kind="unknown"` and a teaching warning; the Ollama `/api/show` context parse; the floor
  warning; latency recorded.
- Record: config round-trip; `daisugi modules` shows the host line.
- Shim: a proxied count_tokens request never reaches the upstream fake; the estimate is within
  10% of the upstream's answer on a fixture body when the upstream *is* Anthropic-compatible
  (then the shim is bypassed).
- Live (skip without `OPENDAISUGI_TEST_MODEL_HOST`): probe the named host, assert ≥ 1 model.

## Out of scope

Model downloads on the remote; managing the remote's server process; TLS to the model host
(the tailnet is the transport security; a LAN host is documented as plaintext).

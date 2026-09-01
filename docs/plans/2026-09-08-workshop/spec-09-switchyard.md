# Spec 09 — NeMo Switchyard upstream of the gateway

**Master:** §5.7 · **Size:** M · **Depends on:** 08

## Purpose

Let the gateway hand *model choice* to NVIDIA's open-source router while keeping the *meter*
ours. Claude Code → daisugi gateway → Switchyard → providers (the model host from spec-08,
Anthropic, others). The gateway's own rules router stays the no-dependency default.

## The crux

A chooser we do not own sits where we can measure it. If Switchyard were in front of the gateway,
the gateway would see a request whose model had already been chosen and could not attribute a
saving to the choice. Behind the gateway, every turn is journaled with the target Switchyard
picked, and the report can say "the escalation router sent 71% of turns to the local model and
the pass rate held." A claim we cannot measure is not a claim we make.

## Files

As built. The first plan named gateway.py, install.py and pyproject.toml. None
of them changed.

```
src/opendaisugi/router_switchyard.py    # pinned facts; find the binary; render TOML; read the tier pair; start, stop, health
src/opendaisugi/gateway_pipeline.py     # router_mode rules | external | off; ExternalRouterConfig
src/opendaisugi/gateway_asgi.py         # read the served target; book the turn; keep the launch mode on reload
src/opendaisugi/gateway_report.py       # per-target share table
src/opendaisugi/cli.py                  # `daisugi gateway --router`; `daisugi router status|stop`; `daisugi install --router`
src/opendaisugi/config.py               # gateway_router, switchyard_route_id, switchyard_*_model, switchyard_api_key_env
src/opendaisugi/modules.py              # router stage: the NeMo Switchyard row with its real state
tests/fixtures/switchyard/              # routes.example.toml, and a fake switchyard-server for tests
tests/test_router_switchyard.py
tests/test_config_switchyard.py
tests/test_gateway_upstream.py
tests/test_gateway_report_switchyard.py
tests/test_cli_router_switchyard.py
docs/how-to/router-switchyard.md
```

The server is a Rust binary, installed with `cargo install --locked --git
https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server`. No
Python dependency was added.

## Config rendering

`render_switchyard_toml(config) -> str` produces:

```toml
schema_version = 1
[llm_clients.local]
format = "openai_chat"            # or "anthropic_messages" per spec-08's host kind
base_url = "<llm_base_url>"
[llm_clients.anthropic]
format = "anthropic_messages"
base_url = "https://api.anthropic.com"
api_key_env = "ANTHROPIC_API_KEY"
[targets.cheap]
id = "<chosen local model>"
llm_client = "local"
[targets.capable]
id = "<capable model id, default claude-sonnet-5>"
llm_client = "anthropic"
[routes.daisugi]
type = "stage_router"             # or "escalation" per --strategy
```

Exact field names are read from the Switchyard README pinned in plan task 1 and asserted by a
test against a checked-in copy of their example. The file lands at
`~/.opendaisugi/switchyard.toml`, 0600, and is never committed.

The Anthropic client is included only when `ANTHROPIC_API_KEY` is set; otherwise the capable
target is the subscription path *through the gateway's existing base_url* (Claude Max), which
Switchyard cannot route to. In that case the how-to says so plainly: "with no API key, Switchyard
can only route between local hosts; frontier turns still go through your Claude login."

## Process management

`daisugi gateway --router switchyard` starts `switchyard-server --config …` as a managed child
(same pattern as the resident gate's `_detach`), waits for its health endpoint, then sets the
gateway's `upstream_url` to it. `daisugi router status` prints binary path, version, config path,
health, and the last 10 route decisions from the journal. Stop tears both down.

## The gateway change

As built. In external mode the gateway sends the route id as `model` and
changes nothing else in the body. Switchyard 0.3.0 names the served target in
the `x-model-router-selected-model` response header and in the body `model`.
No `x-switchyard-target` header exists.

There is no separate `route_target` field. The served target goes into the
existing `GatewayTurnRecord.model`, and the tier is `tier-switchyard`. The
gateway books a saving only when the header and the body agree, they name the
configured efficient target, both it and the requested model have a price,
and the served price is lower on input and on output. The counterfactual is
the model the harness asked for. A missing, unclear or route-id answer is
journaled as `unknown` and books no saving. The meter can undersell. It never
books a saving that the price table cannot show, which is what `route_target`
was for.

`gateway_report` adds a table: target, turns, share, input tokens, output
tokens, and frontier tokens saved. It has no pass-rate column, because no
turn outcome is recorded.

Corrections to the text above, from the v0.3.0 schema:

- There is no `escalation` route type. Escalation is `type = "llm_classifier"`
  with `mode = "escalation"`. The renderer writes `stage_router` only.
- Every cloud client uses `forward_auth = true` and sends the caller's own
  login, whatever key the environment holds. The frontier tier works on a
  subscription login. A key is used only when `switchyard_api_key_env` names
  its variable.

## Tests

- TOML rendering against the pinned example; with and without an API key.
- Managed child: a fake `switchyard-server` script that serves `/health` and echoes a chosen
  target header; start/stop; health timeout → clear error naming the binary.
- Gateway upstream: fake upstream records the forwarded body byte-for-byte; `route_target` lands
  in the journal; the count-tokens shim from spec-08 still applies.
- Wire-format conformance: an Anthropic-format request through gateway → fake Switchyard
  translating to OpenAI-format upstream → back; assert tool_use blocks and streaming deltas
  survive both hops (fixtures from `gateway_openai.py`'s existing tests).
- Report: share table sums to 100%; pass-rate column absent when no outcomes exist (never a
  fake 100%).

## Out of scope

Training Switchyard's prefill routers; Switchyard as a NeMo Relay plugin; anything paid.

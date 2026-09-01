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

```
src/opendaisugi/router_switchyard.py    # locate binary; render TOML; start/stop as managed child; health
src/opendaisugi/install.py              # `--router switchyard`: write config, set gateway upstream, ledger
src/opendaisugi/gateway.py              # upstream_url + upstream_kind; per-turn `route_target` field
src/opendaisugi/gateway_report.py       # per-model share table
src/opendaisugi/cli.py                  # `daisugi gateway --router switchyard|rules|off`; `daisugi router status`
src/opendaisugi/config.py               # router: {kind, switchyard_config, upstream_url}
src/opendaisugi/modules.py              # router stage: NeMo Switchyard AVAILABLE/ACTIVE with the real reason
pyproject.toml                          # [router] = nemo-switchyard (if it provides the server; else docs say cargo)
tests/test_router_switchyard.py
tests/test_gateway_upstream.py
docs/how-to/router-switchyard.md
```

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

`upstream_url` (default: the provider's real URL as today). When set, requests forward there
unchanged; the response's `model` field (and a `x-switchyard-target` header if present — plan
task 1 checks the name) is journaled as `route_target`. `gateway_report` adds a table: target,
turns, share, input tokens, output tokens, and the pass-rate column when a pass signal exists
(the rationale ledger's outcome field).

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

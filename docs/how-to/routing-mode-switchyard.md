# Hand model choice to NeMo Switchyard

This guide runs [NVIDIA NeMo Switchyard](https://github.com/NVIDIA-NeMo/Switchyard)
behind the token-saving router. Switchyard picks the model for each turn. The
router stays the meter. It journals every turn with the target that served it.

The facts here come from Switchyard tag v0.3.0, commit 336196f, read on
2026-09-23.

## What this is

Switchyard is a chooser. The router is the meter. The chooser sits behind the
router, so the router sees which model served each turn. If Switchyard sat in
front, the router would only see a model that was already chosen, and it could
not tie a saving to that choice.

This is opt-in. A plain `daisugi router serve` keeps the built-in rules routing mode.

The Switchyard proxy is a Rust binary. `pip install nemo-switchyard` installs a
different thing: the Python library. Install the server with cargo. You need a
Rust toolchain first.

```bash
cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard --tag v0.3.0 switchyard-server
```

This builds the server from NVIDIA's repository at the pinned tag.

## Turn it on

```bash
daisugi install --router --routing-mode switchyard --efficient-model qwen3-coder:30b
daisugi router serve
```

The first command does three things:

- It sets `routing_mode: switchyard` in `~/.opendaisugi/config.yaml`.
- It saves your two tiers there. The capable tier is `claude-sonnet-5` unless
  you pass `--capable-model`.
- It writes `~/.opendaisugi/switchyard.toml` with mode 0600. The file has one
  `stage_router` route with the id `daisugi`.

The second command reads `routing_mode` and does four things:

- It renders the config from config.yaml into a copy for this child only,
  `~/.opendaisugi/router/switchyard-PORT.toml`. A later install cannot
  change what a running child loaded.
- It starts `switchyard-server` on 127.0.0.1, port 4000. Use
  `--switchyard-port` for another port.
- It waits for `GET /health`.
- It sends every Anthropic-wire turn to Switchyard with the route id as
  `model`.

When the router exits, it sends SIGTERM to the child it started. Switchyard
lets open requests finish for up to 30 seconds. On Linux the kernel also sends
the child SIGTERM when the router is killed, so a router killed with
`kill -9` does not leave its child serving.

The router reads the routing mode once, at start. After you change
`routing_mode`, restart the router.

## Pick the efficient tier

- A local model id, such as `qwen3-coder:30b`, runs on the host that
  `daisugi tiers setup --remote` recorded. With no recorded host, it runs on
  Ollama at `http://127.0.0.1:11434`. The local client gets no credential, so
  your login never goes to the local host. The meter prices a local model at
  zero dollars.
- A `claude-*` id, such as `claude-haiku-4-5`, runs on the Anthropic API.

## Who pays for each call

Every cloud tier forwards your own login by default. Switchyard sends the
credential that Claude Code sends, as the router does without Switchyard. A
Claude subscription login works this way. A key that is exported in your shell
changes nothing.

To pay with an API key instead, name its variable:

```bash
daisugi install --router --routing-mode switchyard --api-key-env MY_ANTHROPIC_KEY
```

This sets `switchyard_api_key_env` in config.yaml. Every cloud tier then uses
that key, and Anthropic bills each token to it. The variable must be set in the
shell that starts the router. If it is not set, the router refuses to start.
It never falls back to another way of paying. To go back to your login, run
the same command with `--api-key-env ""`.

The router prints how each tier authenticates when it starts. `daisugi router
status` shows the record the router made when the child started.

With `forward_auth`, Switchyard keeps only the `oauth-*` values of the
`anthropic-beta` header. It drops every other beta value. A feature that needs
another beta value does not reach the model.

The subscription path is what the Switchyard schema documents. It was not run
end to end against a live Anthropic account on this machine.

## How the router books a turn

Switchyard names the target that served each turn in two places: the
`x-model-router-selected-model` response header and the `model` field of the
response body. The router reads both.

- **Both name the efficient target, and it costs less than the model the
  harness asked for.** The turn is a saving. Both models must have a price in
  the router's price table, and the served price must be lower on input and
  on output. The counterfactual is the model the harness asked for, as in rules
  mode.
- **Both name the efficient target, but it does not cost less, or either model
  has no price.** The turn is journaled with the served model and books no
  saving.
- **Both name the capable target, or another model.** The turn is journaled
  with that model and books no saving.
- **One is missing, they differ, or they name the route id.** The turn is
  journaled with the model `unknown` and books no saving.

The meter can undersell. It never books a saving that the price table cannot
show for a served target.

Switchyard picks a model for every Anthropic-wire call, background calls
included. Claude Code sends some small calls as `claude-haiku-4-5`. If
Switchyard serves one of these on a tier that costs more, the turn costs more
than the harness asked for. The meter books that turn as no saving. It does not
book it as a loss.

A `count_tokens` preflight also goes to the route, because Switchyard answers it
there. The router does not journal it as a turn.

## See what it did

```bash
daisugi router status
daisugi router report
```

`daisugi router status` shows:

- the routing mode in config.yaml, the binary, and its version;
- each running child: its pid, port, route, config file, and whether it
  answers `/health`;
- how each tier of that child authenticates, as recorded when it started;
- the share of turns per target, and the last 10 turns.

Each child has its own state file, `~/.opendaisugi/router/switchyard-PORT.json`,
so two routers on two ports do not touch each other's child. When the pid in
a state file is gone, or is no longer switchyard-server, status shows a stale
state file. `daisugi router stop` clears it.

`daisugi router report` adds the share table at the top. Each row shows a
target, its turns, its share, its input and output tokens, and the frontier
tokens it saved. There is no pass-rate column. No turn outcome is recorded, and
the report shows no number it cannot measure.

Each child also writes its own routing log to
`~/.opendaisugi/router/switchyard-routing-PORT.jsonl`. Its own output goes to
`~/.opendaisugi/router/switchyard-PORT.log`.

## Use your own Switchyard config

```bash
daisugi router serve --routing-mode switchyard --switchyard-config ~/my-routes.toml
```

The router does not change your file. It copies it to the per-port path and
starts the child on the copy. It finds the route whose `id` is
`switchyard_route_id`, `daisugi` by default. That route must name two tiers:
`capable_target` and `efficient_target`, or `strong_target` and `weak_target`.
The router meters each turn by the ids of those two targets. If it cannot find
them, it refuses to start. With your own file, no target gets a zero price, so
the dollar saving reads low.

Check a config with:

```bash
switchyard-server --config ~/my-routes.toml --dry-run
```

## When it will not start

- **`switchyard-server is not on PATH`**: install it with the cargo command
  above.
- **`something already answers on http://127.0.0.1:4000/health`**: another
  program holds the port, or a child is left from a router that could not stop
  it. `daisugi router stop` stops every child that has a state file. Else pick
  another port with `--switchyard-port`.
- **`switchyard_api_key_env names ..., which is not set`**: export that
  variable in the shell that starts the router, or clear the name with
  `--api-key-env ""`.
- **`exited before it answered`**: the message shows the end of the child's
  log. Run the `--dry-run` command that the message names.

## Dollars need prices

The token counts are exact. They come from the provider's own usage report. A
saving needs a price for both models, so a model with no price never books
one. A local efficient model that the router configured is priced at zero.

A model with no price still gets a dollar cost on its own turns. That cost
uses the fallback rate, which is the `claude-sonnet-5` rate: 3 dollars per
million input tokens and 15 per million output tokens. It can be too high or
too low for the real model.

The counterfactual of a saving is an estimate too. It prices the served
turn's own token counts at the requested model's rate. The requested model
might have used more tokens or fewer, so the estimate can err in both
directions. Tokens stay the headline number for this reason.

## Turn it off

```bash
daisugi install --router --routing-mode rules
daisugi install --router --routing-mode off
```

`rules` goes back to the built-in router. `off` forwards every turn unchanged
and only meters it. Restart the router after either one.

## Not in this version

- Codex and every other OpenAI-wire harness keep the rules routing mode.
- Training Switchyard's own routers is out of scope.
- Running Switchyard as a NeMo Relay plugin is out of scope.

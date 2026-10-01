# Run agentic steps on other providers (agent profiles)

An agentic step can run its sub-agent on any model you list: Claude through
`claude -p`, an Anthropic or OpenAI key, a local Ollama or llama.cpp server,
Switchyard, or a model on another machine of your tailnet. You list the
places once, in `config.yaml`. A plan step, or a parent agent, names a place
by its profile name. It never names a URL or a key.

## 1. List the profiles

In `~/.opendaisugi/config.yaml` (or the data home you use):

```yaml
agent_profiles:
  cloud:                       # Anthropic, with a key from your shell
    wire: anthropic
    key_env: ANTHROPIC_KEY_FOR_AGENTS
  oai:                         # an OpenAI-compatible service
    wire: openai
    base_url: https://api.openai.com/v1
    model: gpt-4.1-mini
    key_env: OPENAI_KEY_FOR_AGENTS
  desk:                        # Ollama on the desktop, over the tailnet
    wire: openai
    base_url: http://desk.tail1234.ts.net:11434/v1
    model: llama3.3:70b
  switchyard:                  # the local Switchyard router
    wire: anthropic
    base_url: http://127.0.0.1:4000
    model: daisugi
  opus:                        # claude -p with another model
    wire: claude-code
    model: opus
```

The fields:

- `wire`: `claude-code` (`claude -p`), `anthropic` (the Messages API) or
  `openai` (chat completions). Ollama speaks both HTTP wires.
- `base_url`: the server. Leave it out for the provider's own server.
- `model`: the default model id. Any id is allowed; it is free text.
- `key_env`: the name of the variable that holds the key. Leave it out for
  a server that needs no key. A claude-code profile takes no `base_url`
  and no `key_env`.

`default` is built in: the run's own runtime (`--agent claude|sprig`), with
no key and no URL. `daisugi config` shows the profile names, never a key
name or a URL.

## 2. Name a profile in a step

```json
{"id": "count", "type": "agentic", "prompt": "count the TODOs",
 "workspace": "/home/me/src/app", "tools": ["Read", "Bash"],
 "profile": "desk", "preferred_model": "qwen3-coder"}
```

The model is the step's `preferred_model`, else the profile's `model`, else
haiku. A profile on the `anthropic` or `openai` wire runs on sprig, whatever
`--agent` says.

## 3. Let a sprig agent start children

Add `Spawn` to the step's tools, and let the envelope admit it:

```yaml
permissions:
  mcp_allowlist: ["sprig/spawn"]
```

A glob counts too: `sprig/*` or `*/*` in `mcp_allowlist` also grants
spawn. The sprig agent can then call `spawn` with a task, a narrower
envelope, a profile and a model. Each child is proved inside its parent, reserved from
its parent's budget and charged its real use (`daisugi tree status` shows a
branch that overran as "over by N"). Give the run a budget with `--tokens`
and `--turns` on `weave`, `run` or `orchestrate`.

## How the keys move

`daisugi` reads each `key_env` once, when the run starts. The child gets its
key on an inherited pipe (descriptor 3), and a child that may start children
gets the resolved table on descriptor 4. No key goes in an argv, a variable
of a child, a log, the ledger, a session file or a model's prompt. A key in
a provider's error text, or in a child's answer to a spawn, reads `[key]`.

Every child, with a profile or without, gets an environment with no
variable whose name ends in `_API_KEY` or `_TOKEN` (so no `GITHUB_TOKEN`
either) and none of the table's `key_env` names. A `claude -p` child uses
Claude Code's own login: a login by `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN` or `CLAUDE_CODE_OAUTH_TOKEN` does not reach it. It
keeps `ANTHROPIC_BASE_URL`. sprig itself scrubs four key variables and its
`--scrub-env` names from what its tools see; other credentials in sprig's
own environment (for example `GEMINI_API_KEY` when sprig runs on its own)
reach its tools.

## Keep provider keys out of your exported shell

Any process of your user can read the environment its ancestors started
with, through `/proc/<pid>/environ`, and daisugi cannot clear an
ancestor's environment. So a key exported in your shell is readable by
every agent you start from that shell, even one whose environment daisugi
scrubbed. Keep provider keys out of the exported environment: put them in
a key file that a small launcher reads and sets only for `daisugi`, for
example:

```sh
# ~/bin/daisugi-run: the keys exist only in this process and its children
ANTHROPIC_KEY_FOR_AGENTS=$(cat ~/.config/keys/anthropic) exec daisugi "$@"
```

Not covered: a sub-agent runs as your user. A process of your user can
also read a sprig's memory when `kernel.yama.ptrace_scope` is 0 (the
Fedora default), and a node that may spawn holds every profile's key. This
is runtime assurance, not an OS sandbox. Give an agent that may hold a key
an envelope with no interpreter, and do not put credentials in a
`base_url`: the base URL is in sprig's argv.

## Budgets and the kill

`--max-tokens` counts all four usage counts of every model turn. A server
that reports no usage never stops sprig on tokens, only on turns, and the
ledger then charges the node its whole reservation. A `claude -p` child has
no token cap; its use is charged after it ends. When a node ends, times
out or writes more than 16 MiB, its starter kills its process group and
the groups of the running nodes below it, deepest first, and sprig kills
the groups of its own bash and gate processes when it ends.

## Check it

```sh
daisugi config                 # the agent_profiles row lists the names
daisugi weave plan.json -e envelope.yaml --yes --tokens 200000
```

Each agentic step runs as a tree in a private directory under `$TMPDIR`
(`daisugi-agentic-*`, kept after the run): `gate/` holds the envelopes and
the audit log, `tree/` the ledger. `daisugi tree status --data-dir <that
directory>` shows each node, its profile, its model and its use.

## The operator's bundle

A starter that is not `daisugi` itself, such as the coppice server when it
starts a foreman's worker, reads no key variable. It runs `daisugi
agent-profiles bundle --fd 3` with a pipe on descriptor 3, and hands the
bytes on to `daisugi tree run --keys-fd 3`. The verb reads each `key_env`
once and writes the resolved table, keys included, to the descriptor; it
prints nothing on stdout. A profile that is not valid, or whose variable is
not set, is left out with one line on stderr that names the profile and the
variable, never the key. An agent cannot run the verb: the gate denies it,
as it denies `tree run`.

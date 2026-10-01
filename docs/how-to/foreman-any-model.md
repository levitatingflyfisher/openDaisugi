# Run the foreman on any model

The foreman answers each thing you say with a fresh model call. That call
does not have to be Claude. You pick the place with one key in
`coppice.toml`, and the place is a profile you already list for agent
steps. This page shows three setups: the Anthropic API, an OpenAI-style
server (on this machine or another), and Claude Code.

The foreman has four tools and no other: `zoom`, `date`, `spawn` and
`status`. They are the same on every model. So are the log and the turn
records. Only the model changes.

## 1. List the place once

Profiles live in `config.yaml` (see [agent profiles](agent-profiles.md)). A
profile is a wire, an optional base URL, an optional model and, if the
server needs a key, the name of the variable that holds it:

```yaml
agent_profiles:
  cloud:                       # the Anthropic API
    wire: anthropic
    model: your-model-id
    key_env: ANTHROPIC_KEY_FOR_AGENTS
  desk:                        # an OpenAI-style server on a machine of your tailnet
    wire: openai
    base_url: http://desk.tail1234.ts.net:11434/v1
    model: llama3.3:70b
```

The key stays in your shell. coppice asks daisugi for the resolved table
when a turn starts, hands the key to sprig on a pipe, and drops it when the
turn ends. It is never in an argument, a file, the log or a note. sprig
starts with a short allowlist of coppice's environment (PATH, HOME, USER,
LOGNAME, SHELL, LANG, LC_*, TZ, TERM, TMPDIR, XDG_*) and nothing else, so a
key or a proxy variable in your shell reaches neither it nor the foreman's
tool relay. The profile's `base_url` is the only address, and it is always
sent (the provider's own server when the profile names none). The same holds
for the compactor's calls.

## 2. Point the foreman at it

```toml
[foreman]
mode = "chat"
turn_profile = "desk"       # the profile's name; empty is Claude Code
turn_model = ""             # empty is the profile's own model
```

`turn_model` overrides the profile's model when you set it. Restart
coppice, then talk to the foreman as before.

### The Anthropic API

Use `turn_profile = "cloud"`. The system page and the chat's view are marked
for the cache, so each turn pays full price only for the short tail and your
message. `coppice foreman stats` shows the cache reads.

### An OpenAI-style server

Use `turn_profile = "desk"`. The base URL can be a server on this machine
(Ollama, llama.cpp, vLLM) or on another one; a profile with no key sends no
auth header. The model must support function calls, because that is how it
reaches the four tools. If it does not, the turn fails and the note says
what the server answered. The server caches a shared prefix on its own, and
the view is the prefix.

### Claude Code

Leave `turn_profile` empty (or `default`) and the turn runs on `claude -p`,
as it always has. A profile with `wire: claude-code` is refused for a turn
with a note that says so: Claude Code has its own runner, with its own
guards.

## 3. What you see when it goes wrong

- A profile name that is not in your table: one note in the chat that names
  the profile. Nothing runs.
- A server that is down, or a key it refuses: one note with the server's
  last line. Say it again to retry; coppice never retries by itself.
- A turn that runs past five minutes is stopped and noted.

## 4. Mixed machines

`spawn` starts a worker with its own profile, so one foreman can send work
to a model on this machine and another on the tailnet. The foreman's own
turn needs only that its profile's server answers. Nothing in coppice
assumes a host: the base URL is the only address.

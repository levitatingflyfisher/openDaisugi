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

## 5. Pools: workers go where there is room

A pool is several machines that serve the same model. You write it in
`pools.toml`, in the same folder as `coppice.toml`:

```toml
idle_after = "2m"          # how long a machine must have had no foreground work
                           # before a background job may start on it

[pool.desk]                # the name of a profile from your agent profiles
model = "llama3.3:70b"     # every entry serves this one model

[[pool.desk.entries]]
name = "gpu"
base_url = "http://gpu.tail1234.ts.net:11434/v1"
slots = 2                  # workers at once on this machine; the default is 1

[[pool.desk.entries]]
name = "laptop"
base_url = "http://laptop.tail1234.ts.net:11434/v1"
```

`spawn(project, brief, profile: "desk")` then picks the first entry that has
a free slot and is not cooling down. If none has room, the answer is
`queued proj-3: ...` and the job waits. `coppice foreman queue` shows the
waiting jobs and each entry (free, busy, or cooling until a time, with the
reason). The Machines panel lists the same entries.

The rules do not bend:

- A pool has one model. A spawn on it that asks for another model is refused.
  A worker never moves to another model on its own.
- An entry that does not answer cools down for 30 seconds, then 1 minute, and
  so on up to 10 minutes. The job goes to another entry of the same pool, and
  the foreman says so in one line. The entry is tried again when its time ends.
- When a quota is out on every entry of the pool, the job is paused and put
  back in the queue, and the foreman says so. Other pools keep going: a local
  model works while a cloud quota is out.
- The Claude Code profile (`default`) is not a pool. Pools run for the chat
  foreman.
- After a restart, jobs that were waiting in the foreground are dropped (one
  line each). Background jobs stay in the queue.
- A file that does not read is never deleted. If `pools.toml` has a mistake,
  coppice moves it to `pools.toml.broken-<time>`, says so in one line, and goes
  on with no pools: a profile that names no pool is not touched by it. Fix the
  file and save it again under its name. A bad `queue.json` or a bad
  `coppice foreman queue add` request is moved aside the same way.
- Every entry gets the key of the pool's profile, so write only `base_url`
  addresses you trust with it.

## 6. Idle machines pull background work

A background job is work that can wait for a quiet machine. Nothing is in the
background queue until you add something:

```
coppice foreman queue add --project proj --brief "tidy the tests" --profile desk
```

The foreman can do the same with `spawn(..., background: true)` on a pool
profile. A background job starts only on an entry that has had no foreground
job for `idle_after` (2 minutes unless you set it). When a foreground job needs
the room, the background worker is paused at once (every process
group of the worker, the model process included, is stopped, not killed, so no
file is cut in the middle of a write), and it goes
on when the entry has been idle again. The foreman's chat gets one line for each
change: started on X, paused, resumed, done.

One limit to know: a pause stops the worker where it is, not at a safe point.
If it stopped in the middle of a git write, it holds that git lock (an
`index.lock` or a ref lock) until it goes on, and a foreground job in the same
project can fail on the lock meanwhile. Run background jobs on a project that no
foreground job writes to, or use a worktree for each.


## 7. A message while the foreman works

You can talk while a turn runs. When the foreman next uses a tool, the server
adds your new message to the tool's result, and the model answers it in the
same turn. The chat shows your line as `delivered mid-turn`. If the turn ends
first, your message is the next turn, and the chat shows `queued` until then.
A message is never lost and never answered twice.

Two details keep this honest. Your words come in a block marked with a code
that is new for each turn, and the model is told to trust only a block with
that code. A worker's log or a file that holds a look-alike block is shown
to the model with the tag broken, so it cannot pass for you. And once your
message has reached the model, it stays delivered even if the turn then
fails: the foreman does not run it again as a new turn, because that could
repeat what the model did on it. One line says "Your message reached the turn
before it failed." Say it again if you want it run. If the message could not
be written to the model, it was not delivered and it is the next turn. A long
message that the log split in parts is delivered in all its parts, in order.

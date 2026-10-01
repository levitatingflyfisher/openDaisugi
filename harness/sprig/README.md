# sprig

sprig is openDaisugi's own small agent loop, in Go. It is on hold with the owner.

| Binary | What it is |
|---|---|
| `sprig` (`cmd/sprig`) | The loop: one model, a small tool set, each tool call checked by the gate. |
| `sprig-hook` (`cmd/sprig-hook`) | The loop run inside a host's hook path. |
| `sprig-mcp` (`cmd/sprig-mcp`) | The loop's tools served over MCP. |
| `weave` (`cmd/weave`) | A JSON graph of steps, run one step at a time. |

With `--gate`, each entry point calls `daisugi gate check --mode enforce`. Without `--gate` the
gate is off (`AllowAll`, `cli.go`). The owner agreed to turn the gate on by default once it runs
smoothly.

Three flags shape one run of `sprig`:

- `--tools LIST` is the tool wall: a comma list of `read`, `write`, `edit` and `bash` (all four by
  default). A tool not in the list is not offered to the model, and a call to it is refused as an
  unknown tool. An empty list or another name exits 2 with one line.
  `--tools none` offers no tool at all, for a one-shot call: no tool schema is sent (on `claude -p`,
  every built-in is off and the MCP config is empty), and a tool call from the model fails the run.
- `--system-file F` uses the bytes of F as the system prompt, in place of sprig's own, on every wire.
- `--mcp-config F` is the MCP servers whose tools the model may call: a file, or inline JSON
  `{"mcpServers": {name: {command, args, env}}}`. Only stdio servers run; another kind exits 2.
  sprig starts each in a process group of its own, offers its tools as `mcp__<server>__<tool>`,
  and relays the calls. A tool error or a dead server is a tool result, not a crash. A call waits
  30 s, and a message may hold 32 MiB. The groups die when the run ends. With `--tools none` the
  model sees only these tools. On the claude-code wire claude runs the servers from the same JSON.
- On the anthropic wire a `--system-file` is one system block with a `cache_control: ephemeral`
  mark (never on an empty block). When the run can call a tool (the wall's, or an MCP
  server's), the last block of the request that has text carries a mark too, and it moves with
  each tool round, so the next call of the loop reads everything before it. A one-shot call
  (`--tools none` with no MCP) marks the system block only. The other wires send no mark.
- `--model M` is the model to ask. The `claude -p` backend passes `--model=M` (default `haiku`); the
  API backend uses it over `SPRIG_MODEL`.
- `--max-tokens N` stops the run before a model call once its turns have used N tokens or more
  (all four usage counts added up); it exits 1 with `sprig: used T tokens of the N allowed`. One
  turn may go past N. The default, -1, is no cap.
- `--json` prints `answer`, `turns` (every message), `model_turns` (the model's replies), `model`
  (the model asked for) and `usage`: the token counts of all turns added up, as `input_tokens`, `output_tokens`, `cache_read_input_tokens` and
  `cache_creation_input_tokens`. A count that adds up to zero is left out.

The model wire and the key:

- `--wire claude-code|anthropic|openai` picks the wire. `claude-code` runs `claude -p` (the
  default). `anthropic` speaks the Messages API (`SPRIG_BACKEND=api` picks it too). `openai`
  speaks chat completions with native function calls: OpenAI, Ollama, llama.cpp, vLLM, or a
  gateway such as Switchyard. The model id is free text on every wire.
- `--base-url U` is the server for the `anthropic` or `openai` wire. The request goes to
  `U/v1/messages` or `U/chat/completions`. A key is required only for the default server
  (`https://api.anthropic.com`, `https://api.openai.com/v1`); a local server without a key gets
  no auth header.
- `--key-fd N` reads the key from descriptor N (3 or more) and closes it. With it, sprig reads no
  key from the environment. A key with a control character is refused.
- Every process sprig starts (a bash call, the `claude -p` backend, the gate) gets sprig's
  environment less `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `OPENAI_API_KEY`, `SPRIG_API_KEY`
  and the names in `--scrub-env A,B`. None inherits a descriptor above 2. The key in a provider's
  error text reads `[key]`.

The spawn tool:

- `--tools ...,spawn --tree-cmd CMD --session S` offers `spawn`: start a child agent on a task in
  a narrower envelope, wait, and return its answer. CMD is the one starter, `daisugi tree run`,
  split at whitespace as `--gate-cmd` is. The gate sees the call as `mcp__sprig__spawn`, so only
  an envelope whose `mcp_allowlist` names `sprig/spawn` lets it run.
- The parent of every child is sprig's own `--session`, never a value from the call. sprig's own
  use so far goes as `--parent-tokens-used` and `--parent-turns-used`; the call's profile, model,
  tokens and turns go as flags; the task, envelope, tools and max_turns go on stdin.
- `--spawn-keys-fd N` reads the resolved agent profiles, with their keys, and hands them to the
  tree command on a fresh pipe as its descriptor 3. They are never in an argv, a log or a prompt.
- The result is the child's answer, `refused: ` and the reasons, or `sent to the operator as ask
  ID`. A child's branch use counts toward `--max-tokens`. Children run one at a time.

This is not an OS sandbox: a tool the gate allows still runs as the same user.

daisugi's agentic steps can run on sprig (`daisugi weave|run|orchestrate --agent sprig`): daisugi
starts `sprig --json --gate --gate-cmd ... --tools ... --model ...` in the step's workspace, with
the gate pinned to a private gate root and a session daisugi picks.

grove, the fleet view over many sprigs, is retired. coppice (`../coppice`) replaced it: coppice
holds every session in one server with thin TUI, web and phone clients, for any harness.

Build and test:

```sh
go build ./...
go test -p 1 ./...
```

The designs are in `docs/harness/harness-designs.md`.

## The Rust sprig

`harness/sprig-rs` is a second sprig, written in Rust: the same four binaries, the same command
line, prompt, request bodies, session tree, hook and MCP wire, and Go's own error words.
`clients/sprig_compare.py` runs each case on the Go and the Rust binaries side by side, with a fake
`claude`, a fake gate and a fake API, and some cases through the real Go and Rust daisugi
(`clients/ADJUDICATIONS.md`, SP-R-1 to SP-R-6). Go stays the default. To build and install the
Rust one in its place (you need cargo as well):

```sh
SPRIG_PORT=rust ../../scripts/install.sh
```

`SPRIG_PORT=rust scripts/release.sh VERSION` puts it in the release tarball in place of the Go one,
with its own `NOTICE`. It links `libgcc_s` as well as libc. `harness/sprig-rs/PINS.md` lists its
crates and why each is there.

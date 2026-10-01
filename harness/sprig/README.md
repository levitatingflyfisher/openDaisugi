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
- `--model M` is the model to ask. The `claude -p` backend passes `--model=M` (default `haiku`); the
  API backend uses it over `SPRIG_MODEL`.
- `--json` prints `answer`, `turns`, `model` (the model asked for) and `usage`: the token counts of
  all turns added up, as `input_tokens`, `output_tokens`, `cache_read_input_tokens` and
  `cache_creation_input_tokens`. A count that adds up to zero is left out.

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

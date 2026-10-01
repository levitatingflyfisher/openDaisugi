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

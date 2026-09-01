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

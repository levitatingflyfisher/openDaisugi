# coppice pins

Everything coppice pins, why, and the command that re-reads it. Verify before you rely.

## Terminal engine

| what | value |
|---|---|
| module | `go.mitchellh.com/libghostty` |
| canonical source | `https://tangled.org/mitchellh.com/go-libghostty` (GitHub is a mirror) |
| version | `v0.0.0-20260908040635-9f448dfe8052` |
| upstream tags | none exist. The author has published no tag and no release. |
| ghostty commit it builds | `b0c421fcd2e290629d4285c181b52fe2f2095f06` |
| licence | MIT |

Re-read with:

```
curl -s https://proxy.golang.org/go.mitchellh.com/libghostty/@latest
gh api repos/mitchellh/go-libghostty/contents/CMakeLists.txt -q .content | base64 -d | grep GIT_TAG
```

The author states "I'm not promising any API stability yet." That is why every call into this
module lives in `internal/vt` and nowhere else, and why `internal/boundary/boundary_test.go`
fails the build if a second package imports it. That confinement test lives in its own cgo-free
package, not inside `internal/vt` itself: a test file under `internal/vt`, even in package
`vt_test`, still builds package `vt` to link the test binary, which means it builds cgo. On a
machine without the toolchain from `scripts/toolchain.sh`, that turns `go test ./...` into a hard
compile failure instead of the graceful `toolchain.SkipReason()` skip, and the confinement check
itself could never run to report a clean result. `internal/boundary` has no non-test file and
passes under `CGO_ENABLED=0 go test ./internal/boundary/...`.

## Build toolchain

| what | value |
|---|---|
| Zig | 0.16.0, `https://ziglang.org/download/0.16.0/zig-x86_64-linux-0.16.0.tar.xz` |
| Zig sha256 (x86_64-linux) | `70e49664a74374b48b51e6f3fdfbf437f6395d42509050588bd49abe52ba3d00` (downloaded and run on this box) |
| Zig sha256 (aarch64-linux) | `ea4b09bfb22ec6f6c6ceac57ab63efb6b46e17ab08d21f69f3a48b38e1534f17` (from ziglang.org's index, not independently re-verified here) |
| Zig sha256 (x86_64-macos) | `0387557ed1877bc6a2e1802c8391953baddba76081876301c522f52977b52ba7` (from ziglang.org's index, not independently re-verified here) |
| Zig sha256 (aarch64-macos) | `b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489` (from ziglang.org's index, not independently re-verified here) |
| Zig sha256 source | `https://ziglang.org/download/index.json`, version `0.16.0` |
| CMake | whatever `uv tool install cmake` resolves |
| install prefix | `$HOME/.local/ghostty-vt`, override with `COPPICE_GHOSTTY_PREFIX` |

## Go toolchain deviation

Master spec §4 asked for Go 1.25. `go.mitchellh.com/libghostty` declares `go 1.26.0`, so Go
refuses to build against it from a 1.25 main module. The dev machine ships go1.25.12; some
distributions ship Go with `GOTOOLCHAIN=local` compiled in, so it will not switch on its own.

- `go.mod` declares `go 1.26.0` and `toolchain go1.26.8`.
- `scripts/toolchain.sh` runs `go env -w GOTOOLCHAIN=auto`, which downloads go1.26.8 into the
  module cache on first build.
- Undo both global Go settings with `go env -u GOTOOLCHAIN PKG_CONFIG`.
- §4 already reads Go 1.26 (amended 2026-09-08); this task edited nothing there.

## Global Go environment

`scripts/toolchain.sh` wrote two values with `go env -w`, so they persist across shells on this
machine, not just inside `harness/coppice`:

| key | value |
|---|---|
| `GOTOOLCHAIN` | `auto` |
| `PKG_CONFIG` | `/home/user/.local/bin/coppice-pkg-config` |

Undo both with:

```
go env -u GOTOOLCHAIN PKG_CONFIG
```

`coppice-pkg-config` is a thin wrapper that **appends** `~/.local/ghostty-vt/share/pkgconfig` to
`PKG_CONFIG_PATH` and execs the real `pkg-config`; it never replaces `PKG_CONFIG_PATH`. Verified
transparent for unrelated packages:

```
$ diff <(pkg-config --cflags --libs zlib) <(coppice-pkg-config --cflags --libs zlib)
$ echo $?
0
```

(no output, exit 0: the wrapper's output for a package outside `~/.local/ghostty-vt` is byte-for-
byte identical to the system `pkg-config`.)

Task 0's check that `harness/sprig` still builds after the installer runs cannot, by itself,
catch a `PKG_CONFIG` regression: sprig has no cgo in its dependency graph, so it never invokes
`pkg-config` at all and would build the same whether the wrapper was broken or absent. The
`diff` above against a real cgo consumer (`pkg-config`'s own `zlib.pc`) is what actually exercises
the wrapper.

`COPPICE_GHOSTTY_PREFIX` and `PKG_CONFIG_PATH` name the same install location two different
ways: `internal/toolchain.SkipReason` and `scripts/toolchain.sh` both read the prefix from
`COPPICE_GHOSTTY_PREFIX`, but the linker itself resolves `libghostty-vt` through
`PKG_CONFIG_PATH` (or the `coppice-pkg-config` wrapper above, on a machine that ran the
installer). Set both or neither. Pointing only one of them somewhere else does not move the
library; it only moves what one half of the build thinks the library's address is, and every
cgo test then skips with a reason that no longer matches what actually failed to link.

## Platform

Linux only, in practice and in the build itself. `internal/server/lock_other.go` and
`internal/server/peercred_other.go` exist so the module compiles on other unixes, but neither
has a real implementation there: the start lock always reports held, and every connection's
peer-uid check fails closed, refusing everything. The client's own autostart path
(`internal/cli/client.go`) sets `syscall.SysProcAttr{Setsid: true}`, which has no Windows
equivalent as written, so the build does not target Windows at all.

`COPPICE_NO_AUTOSTART`, read at `internal/cli/client.go`, disables the autostart every socket
command otherwise falls back to when nothing is listening. It is not stripped from a spawned
pane's environment the way `COPPICE_SOCK` and `COPPICE_PANE` are, so it inherits into every
process coppice starts, gate hooks included.

## Other Go dependencies

| module | version | why |
|---|---|---|
| `github.com/creack/pty` | v1.1.24 | PTY spawn and winsize. MIT. |
| `github.com/BurntSushi/toml` | v1.5.0 | manifest parsing. Its `MetaData.Undecoded()` is how we reject unknown keys the way Herdr's `deny_unknown_fields` does. MIT. |
| `golang.org/x/term` | v0.45.0 | raw mode for `coppice attach`. The same module `harness/sprig` already requires, so no new module family. BSD-3. |

`creack/pty` is now a direct `require` in `go.mod`: Task 6's `internal/pane/pty.go` imports it for
real. `toml` is now a direct `require` too: Task 11's `internal/detect/manifest.go` imports it for
real, so the pre-declaration comment that used to sit ahead of it in `go.mod` is gone. `go.sum`
already carried the checksum lines from the pre-declaration, so no `go mod download` was needed to
land this task; `go build ./...` and `go test -race ./...` both pass unchanged. No dependency
remains pre-declared-but-unimported in this module as of Task 11.

Earlier history, kept for the scratch-`go mod tidy` trap it documents: mid-Task-1, a scratch
`go mod tidy` run once treated the still-unimported `toml` require as unused and silently dropped
its `require` line and `go.sum` checksum lines; `go.mod` was restored from a backup but `go.sum`
was not, and `go list -m all` caught the mismatch ("missing go.sum entry"). That specific trap no
longer applies to `toml` now that it is imported, but the same thing can happen to any other
pre-declared-ahead-of-its-task dependency added later: never run a bare `go mod tidy` in this
module before the task that actually imports the new dependency has landed.

## Herdr reference

| what | value |
|---|---|
| repository | `https://github.com/herdrdev/herdr` |
| manifests vendored from | `src/detect/manifests/*.toml` |
| default branch | `master`, not `main`. `gh api repos/herdrdev/herdr/commits/main` returns HTTP 422. |
| vendored at commit | `b9ce96869e89937278d673d70ae4c135dd318469`, also recorded in `internal/detect/manifests/PROVENANCE` |
| licence | Apache-2.0. See `harness/coppice/NOTICE`. |

Master §4 originally named `c5a21edfcbc2d5b46540ad91b7980aca31f5f1f3` as "their 1.3.2". That
commit does not exist (`gh api repos/herdrdev/herdr/commits/c5a21edf…` returns HTTP 422 "No commit
found") and the newest tag is `v0.9.0`, so there is no 1.3.2. Task 12 Step 1 removed the SHA and
the version claim from §4 and records only the commit actually vendored, above.

## Build-time version

`cmd/coppice`'s `--version` reads `internal/cli.version`, a package-level `var`, not
`main.version`. CI must set it at build time with:

```
go build -ldflags "-X github.com/opendaisugi/coppice/internal/cli.version=X.Y.Z" ./cmd/coppice
```

`-X main.version=...` is a silent no-op: `main` has no `version` symbol of its own to overwrite,
and the linker does not fail when the named symbol does not exist. Verified:

```
$ go build -ldflags "-X github.com/opendaisugi/coppice/internal/cli.version=9.9.9" -o v ./cmd/coppice && ./v --version
coppice 9.9.9
```

## libghostty symbols coppice uses

Filled in by Task 1 Step 3 from `go doc`. If a future bump changes any signature here, the
compile breaks in `internal/vt` and nowhere else.

<!-- GODOC-BEGIN -->
Read on 2026-09-09 with `go doc go.mitchellh.com/libghostty <Symbol>` against the pinned
pseudo-version above (after `go get`). Every signature below matches the brief's table
exactly; no signature correction was needed.

```
NewTerminal(opts ...TerminalOption) (*Terminal, error)
WithSize(cols, rows uint16) TerminalOption
WithMaxScrollbackLines(lines uint) TerminalOption
WithProgressReport(fn ProgressReportFn) TerminalOption
type ProgressReportFn func(t *Terminal, report TerminalProgressReport)   // TWO arguments
type TerminalProgressReport struct { State TerminalProgressState; Progress int8 }
(*Terminal).VTWrite(data []byte)                     // returns NOTHING
(*Terminal).Resize(cols, rows uint16, cellWidthPx, cellHeightPx uint32) error
(*Terminal).Cols() (uint16, error)                   // returns an error
(*Terminal).Rows() (uint16, error)                   // returns an error
(*Terminal).CursorX() (uint16, error)
(*Terminal).CursorY() (uint16, error)
(*Terminal).CursorVisible() (bool, error)
(*Terminal).Title() (string, error)                  // returns an error
(*Terminal).SelectAll() (*Selection, error)          // RETURNS the selection; does not install it
(*Terminal).SetSelection(sel *Selection) error
(*Terminal).Close()                                  // returns NOTHING
NewFormatter(t *Terminal, opts ...FormatterOption) (*Formatter, error)
WithFormatterFormat(f FormatterFormat) / WithFormatterTrim(bool) / WithFormatterUnwrap(bool)
WithFormatterSelection(sel *Selection) FormatterOption
FormatterFormatPlain
(*Formatter).FormatString() (string, error)
(*Formatter).Close()
NewRenderState() (*RenderState, error)
(*RenderState).Update(t *Terminal) error
(*RenderState).RowIterator(ri *RenderStateRowIterator) error    // fills a PRE-ALLOCATED handle
NewRenderStateRowIterator() (*RenderStateRowIterator, error)
NewRenderStateRowCells() (*RenderStateRowCells, error)
(*RenderStateRowIterator).Next() bool
(*RenderStateRowIterator).Cells(rc *RenderStateRowCells) error
(*RenderStateRowCells).Next() bool
(*RenderStateRowCells).AppendGraphemes(dst []byte) ([]byte, error)
(*RenderStateRowCells).Style() (*Style, error)
(*RenderStateRowCells).FgColor() (*ColorRGB, error) / BgColor() (*ColorRGB, error)
(ColorRGB).Components() (r, g, b uint8)
(Style).Bold/Faint/Italic/Blink/Inverse/Strikethrough() bool
(Style).Underline() SGRUnderline
```

**Behavioural finding beyond the signature table** (found by the Step 4 tests failing, not by
`go doc`): a `Formatter` built with a **nil** selection formats the terminal's *entire* active
buffer, scrollback included - empirically identical to the output of a `Formatter` given the
selection from `SelectAll()`. There is no "just the visible screen, nil selection" shape. To get
the screen only (what `Term.PlainScreen` and `Term.PlainScreenUnwrapped` promise), `internal/vt`
builds an explicit `Selection` bounded to `PointTagActive`, the cursor-movable area, which
excludes scrollback, from two `Terminal.GridRef` calls at `(0,0)` and `(cols-1, rows-1)`:

```go
start, _ := t.term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: 0, Y: 0})
end, _ := t.term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: cols - 1, Y: uint32(rows - 1)})
sel := &libghostty.Selection{Start: *start, End: *end}
```

`PointTag` also has `PointTagViewport` (the visible area, which moves when the user scrolls into
history) and `PointTagHistory` (scrollback only); coppice does not implement scrollback browsing
in Task 1, so only `PointTagActive` is used. This is a real API surface (`Point`, `PointTag`,
`Terminal.GridRef`) that the brief's table did not list because it was not needed under the
brief's (incorrect) assumption that a nil selection already meant "screen only". Confirmed with a
standalone `go run` reproduction against the pinned commit before changing `internal/vt/vt.go`;
deleted after use, per the scratch-cleanup rule.

One implementation choice, not a signature difference: `internal/vt` compares
`Style.Underline()` against the named constant `libghostty.UnderlineNone` rather than the literal
`0` the brief's draft used. Both compile to the same value; the named constant is what `go doc`
actually exports for that comparison.

**Second behavioural finding, found in fix round 1**: `PlainScreenUnwrapped` is bounded to
`PointTagActive` (same as `PlainScreen`, above), so when a soft-wrapped logical line has part of
itself pushed into scrollback by later output, `PlainScreenUnwrapped` does **not** reconstruct the
full line. It joins whatever physical rows remain in the active area and silently drops the
scrolled-off prefix. There is no truncation marker. Reproduced with a 10-column, 3-row terminal:
feeding a 25-character line fills the screen exactly; one more row of output scrolls the line's
first physical row ("abcdefghij") into history. `PlainScreenUnwrapped()` afterward returns only
`"klmnopqrstuvwxy\nZZZZZ"`. The full line is still recoverable, just not joined: `PlainAll()`
(which formats with `unwrap=false`) still contains the scrolled-off `"abcdefghij"`, in its original
wrapped-row form. A caller that needs the complete, joined, historical line has no single call
that produces it as of Task 1. That would need `unwrap=true` over `SelectAll()`'s selection,
which `internal/vt` does not currently expose. Covered by
`TestPlainScreenUnwrappedDoesNotReachIntoScrollback` in `internal/vt/vt_test.go`.
<!-- GODOC-END -->

## Web / phone (plan 06)

| what | value | how it was fixed |
|---|---|---|
| websocket library | `github.com/coder/websocket` v1.8.15 | `go list -m -versions github.com/coder/websocket` on this box, latest version listed. Its own `go.mod` declares no dependencies. Licence is ISC, read from the module's `LICENSE.txt`, not MIT. `Accept` refuses a cross-origin request unless the caller sets `OriginPatterns` or `InsecureSkipVerify`, which is the wall a bearer token alone would not give. |
| terminal QR | `github.com/mdp/qrterminal/v3` v3.2.1 | `go list -m -versions github.com/mdp/qrterminal/v3` on this box, latest version listed. `GenerateWithConfig` with `HalfBlocks: true` is the call this code uses. The file that defines it imports only `golang.org/x/term` and `rsc.io/qr`, both already resolvable in this module, so drawing the code needs no new transitive dependency. The module's `cmd/` demo also imports `github.com/mattn/go-colorable` and `github.com/mattn/go-isatty`, which stay out of `go.sum` because nothing in this module imports that demo. |
| `tailscale cert` flags | `--cert-file value` and `--key-file value` | `tailscale cert --help` on this box. Verified: yes. `tailscale` is present on PATH. |
| Let's Encrypt validity | 90 days, renewal is the operator's | https://tailscale.com/kb/1153/enabling-https. Not exercised on this box. Recorded from the source page, unverified locally. |
| frame cell `attrs` | bold 1, faint 2, italic 4, underline 8, blink 16, inverse 32, strike 64 | Read from `internal/vt/vt.go`: `AttrBold uint16 = 1 << iota` through `AttrStrike`, seven names. `internal/pane/grid.go` copies the value unchanged into `proto.Cell.Attrs`, and `Cell.MarshalJSON` sends it as the fourth element of `[text, fg, bg, attrs]`. |
| `pane.attach` size | `cols` and `rows` are optional; absent means attach, do not resize | `internal/server/attach.go:37`, `if cols, okc := r.Int("cols")`, resizes only when both are present and positive. Already true in the tree; no edit landed here. |
| `pane.list` row shape | **flat**: `id, label, cwd, cmd, kind, harness, workspace, tab, closed, cols, rows, state, source, detail`, plus `ts` and `session_id` once a current event exists, plus `exit_code` once the process exited, plus `ask` once the gate holds one | Read from `handlePaneList` in `internal/server/panes.go`. The state is a string on the row, not a nested `PaneStateEvent`. Master 3.2's `state: PaneStateEvent|None` describes the Python `PaneBackend`, a different reader. `ts` and `session_id` were already on the row before this task; `TestPaneListCarriesTsAndSessionIDFromTheStoredEvent` in `internal/server/panes_test.go` already covers both, so no server edit was needed. |
| `state` event shape | `{"event":"state", ...PaneStateEvent...}` flattened: `v, ts, session_id, harness_session_id, harness, pane, state, source, ask, detail` | Read from `stateEvent` in `internal/server/agents.go` and `PaneStateEvent` in `internal/proto/state_event.go`. Same field names as a `pane.list` row, so one reader serves both. |
| refusal shape | `{"id":…,"ok":false,"error":{"code":…,"message":…}}` | `proto.ErrResp` and `proto.Response` in `internal/proto/proto.go`. `code` is the closed enum: `bad_request, no_such_pane, no_such_workspace, no_such_tab, pane_closed, server_closed, not_attached, adapter_error, spawn_failed, timeout, unauthorized, internal`. |
| Go toolchain this module resolves to | `go1.26.8` | `go version` run inside `harness/coppice`, with the box's global `GOTOOLCHAIN=auto` already set by `scripts/toolchain.sh`. The system Go outside the module is `go1.25.12`. Same fact as the "Go toolchain deviation" section above; repeated here because the host check this plan runs depends on it. |
| where the web verbs live | `internal/cli/web.go`, a case beside `server`, `attach`, and `workspace|tab|pane|agent|session` in the verb switch inside `internal/cli/cli.go`'s `Run` | Read from `internal/cli/cli.go`. The global `--socket` and `--data-dir` flags parse into `c.Socket` and `c.DataDir` before that switch runs, so `web serve`, `web token`, and `web cert` reach them without any extra wiring. |
| websocket close reasons | `"coppice-server is not reachable"` for a dial that never connected, `"coppice-server closed the connection"` for an upstream that ended after connecting, both closed with the same `StatusInternalError` code | Read from `internal/web/ws.go`. The code is identical for both, so `internal/web/static/app.js` tells them apart by the reason text alone, copied in by hand. A test asserts both literals against `ws.go` and against `app.js`, since nothing but a shared string keeps the two files agreeing. |

Both new modules were fetched with `go get` over the network; neither was in the module cache
before this section was written. `go.mod` carries them as `// indirect` because nothing imports
them yet, the same pre-declared-ahead-of-its-task shape `toml` and `creack/pty` carried earlier in
this file's "Other Go dependencies" section, and the same trap applies: a bare `go mod tidy` run
before the step that imports `coder/websocket` and `qrterminal/v3` will drop both `require` lines
and their `go.sum` entries. No such tidiness check runs in `.github/workflows/coppice.yml`, so an
unimported require does not fail CI, but the module must still build clean, and it does:
`go build ./...` and `go test -race -count=1 ./internal/server/...` both pass unchanged with the
two new requires present.

The host check for this plan found every tool it looks for: Go resolves to 1.26.8 as above, Node
is v22.22.2, `tailscale` and `magick` are both on PATH. No SKIP-REASON line was printed.

# VT fixtures

Two kinds of file live here.

**Generated.** `*.bin` written by `go run ./testdata/vt/gen` from
`testdata/vt/gen/main.go`. These cover the VT behaviour the grid must get right:
SGR, wrap, scroll, erase, OSC title and OSC progress, and a prompt box. They are
deterministic, so regenerating them must not change a golden. If it does, the
generator changed and the goldens need a fresh look.

**Recorded.** `recorded-*.bin` captured from a real harness with
`scripts/record-vt.sh`. These are opt-in. Nothing in CI records anything, and
the tests that read them skip with a reason when the file is absent. Record one
when a real harness renders something the generated set does not cover.

`*.golden.txt` is the plain-text screen the grid produces for the matching
stream. Regenerate every golden with `COPPICE_UPDATE_GOLDEN=1 go test ./internal/pane/`.

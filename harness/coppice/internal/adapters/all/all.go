// Package all registers every coppice adapter with the shared registry. It
// exists so one import does what five blank imports would otherwise do
// wherever a live registry is needed: the built binary and the CLI's own
// tests both import this package instead of each adapter package by hand.
package all

import (
	_ "github.com/opendaisugi/coppice/internal/adapters/claude"
	_ "github.com/opendaisugi/coppice/internal/adapters/codex"
	_ "github.com/opendaisugi/coppice/internal/adapters/opencode"
	_ "github.com/opendaisugi/coppice/internal/adapters/pi"
	_ "github.com/opendaisugi/coppice/internal/adapters/sprig"
)

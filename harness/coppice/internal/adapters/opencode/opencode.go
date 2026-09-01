// Package opencode is the OpenCode headless adapter slot. spec-05 implements it
// against `opencode serve` plus the plugin whose before-execute hook is
// deny-only by design.
package opencode

import (
	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/pane"
)

func New() pane.Adapter {
	return adapters.NotBuilt{AdapterName: "opencode", Owner: "spec-05"}
}

func init() { adapters.Register(New()) }

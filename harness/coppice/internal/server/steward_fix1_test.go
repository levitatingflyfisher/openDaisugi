package server

import (
	"os"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/steward"
)

// The zoom tool has no label any more: a worker's report names its child
// store, and zoom takes child, an id and an n.
func TestZoomToolRefusesALabel(t *testing.T) {
	_, _, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	for _, arg := range []string{"offset", "size"} {
		r := toolCall(t, ts.Path(), steward.ToolRequest{Tool: "zoom", Args: map[string]any{"label": "x", arg: 1}})
		if r.OK || !strings.Contains(r.Error, "no label any more") {
			t.Fatalf("%s with a label: %+v", arg, r)
		}
	}
}

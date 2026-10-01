package server

import (
	"os"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// M-7: the zoom tool refuses offset and size next to a label, as it refuses
// id and n.
func TestZoomToolRefusesOffsetWithALabel(t *testing.T) {
	_, _, ts := toolsFixture(t)
	ts.Started(os.Getpid())
	for _, arg := range []string{"offset", "size"} {
		r := toolCall(t, ts.Path(), foreman.ToolRequest{Tool: "zoom", Args: map[string]any{"label": "x", arg: 1}})
		if r.OK || !strings.Contains(r.Error, "not a label") {
			t.Fatalf("%s with a label: %+v", arg, r)
		}
	}
}

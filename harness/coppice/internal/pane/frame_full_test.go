package pane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

// The first frame is full and says so. A delta frame leaves the key out, so
// its bytes are what they were before the key existed. ForceFull makes the
// next frame full even when nothing on the grid changed.
func TestFrameFullFlagAndForceFull(t *testing.T) {
	toolchain.RequireOrSkip(t)
	g, err := NewGrid(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, err := g.Write([]byte("aa\r\nbb")); err != nil {
		t.Fatal(err)
	}

	fs := NewFrameState()
	first, changed, err := fs.Next("w1:p1", g)
	if err != nil || !changed {
		t.Fatalf("first frame: changed=%v err=%v", changed, err)
	}
	if !first.Full {
		t.Fatal("the first frame is not marked full")
	}
	b, _ := json.Marshal(first)
	if !strings.Contains(string(b), `"full":true`) {
		t.Fatalf("the first frame's JSON lacks full: %s", b)
	}

	if _, err := g.Write([]byte("\rcc")); err != nil {
		t.Fatal(err)
	}
	delta, changed, err := fs.Next("w1:p1", g)
	if err != nil || !changed {
		t.Fatalf("delta frame: changed=%v err=%v", changed, err)
	}
	if delta.Full {
		t.Fatal("a one-row change is marked full")
	}
	b, _ = json.Marshal(delta)
	if strings.Contains(string(b), "full") {
		t.Fatalf("a delta frame carries the full key: %s", b)
	}

	if _, changed, err := fs.Next("w1:p1", g); err != nil || changed {
		t.Fatalf("an unchanged grid produced a frame: changed=%v err=%v", changed, err)
	}
	fs.ForceFull()
	forced, changed, err := fs.Next("w1:p1", g)
	if err != nil || !changed {
		t.Fatalf("forced frame: changed=%v err=%v", changed, err)
	}
	if !forced.Full || len(forced.RowsChanged) != 2 {
		t.Fatalf("ForceFull did not build a full frame: full=%v rows=%d", forced.Full, len(forced.RowsChanged))
	}
	if _, changed, err := fs.Next("w1:p1", g); err != nil || changed {
		t.Fatalf("ForceFull stuck: changed=%v err=%v", changed, err)
	}
}

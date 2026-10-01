package foreman

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// refViewStep is the batch merge as it was before the planner: a MostDue
// scan per merge. viewStep must give the same lines, and must make them
// through the planner (plannerMerges).
func refViewStep(c *Chat, v viewState, t int64) viewState {
	v.Lines = append(append([]NodeID(nil), v.Lines...), NodeID{L: 0, I: t})
	v.Count = t + 1
	size := 0
	for _, id := range v.Lines {
		size += len(c.line(id))
	}
	if size > c.cfg.ViewMax {
		v.Batch = true
	}
	built := func(id NodeID) bool { _, ok := c.nodes[id]; return ok }
	for v.Batch {
		if size <= c.cfg.ViewMin {
			v.Batch = false
			break
		}
		k := MostDue(v.Lines, v.Count, built)
		if k < 0 {
			break
		}
		size -= len(c.line(v.Lines[k])) + len(c.line(v.Lines[k+1]))
		v.Lines = mergeAt(v.Lines, k)
		size += len(c.line(v.Lines[k]))
	}
	return v
}

func TestViewStepUsesThePlannerAndGivesTheOldLines(t *testing.T) {
	cfg := Config{ViewMax: 3000, ViewMin: 1500}
	cfg.fill()
	c := &Chat{cfg: cfg, nodes: map[NodeID]Node{}}
	const n = 400
	for i := int64(0); i < n; i++ {
		for _, id := range chain(i) {
			text := fmt.Sprintf("node %s %s", id.Name(), strings.Repeat("w", int(7*id.I%40)))
			c.nodes[id] = Node{L: id.L, I: id.I, Text: text, Size: len(text)}
		}
	}
	before := plannerMerges.Load()
	ref := viewState{}
	for i := int64(0); i < n; i++ {
		ref = refViewStep(c, ref, i)
		c.viewStep(i)
		if !reflect.DeepEqual(c.view, ref) {
			t.Fatalf("message %d: view differs from the MostDue loop\n got %v\nwant %v", i, c.view.Lines, ref.Lines)
		}
	}
	if plannerMerges.Load() == before {
		t.Fatal("viewStep made no merge through the planner")
	}
}

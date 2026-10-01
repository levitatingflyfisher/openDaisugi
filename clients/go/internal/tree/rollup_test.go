package tree

import (
	"math/big"
	"testing"
)

// A node with no report of its own costs the larger of its reservation
// and its children's charges, so an overrun below it reaches the root.
func TestANodeWithNoReportRollsUpItsChildrensOverrun(t *testing.T) {
	d, g := growRoot(t, 1000)
	spawnKid(t, d, g, "top", "c", 100, SpawnOpts{})
	spawnKid(t, d, g, "c", "g", 80, SpawnOpts{})
	if _, err := End(d, g, "g", big.NewInt(150), nil); err != nil {
		t.Fatal(err)
	}
	left := func() int64 {
		tr := ReadTree(d)
		return tr.Left(tr.Nodes["top"], "tokens").Int64()
	}
	if l := left(); l != 850 {
		t.Fatalf("running: left %d", l)
	}
	if _, err := End(d, g, "c", nil, nil); err != nil {
		t.Fatal(err)
	}
	if l := left(); l != 850 {
		t.Fatalf("ended: left %d", l)
	}
	spawnKid(t, d, g, "top", "x", 10, SpawnOpts{})
	spawnKid(t, d, g, "x", "y", 5, SpawnOpts{})
	spawnKid(t, d, g, "y", "z", 1, SpawnOpts{})
	if _, err := End(d, g, "z", big.NewInt(400), nil); err != nil {
		t.Fatal(err)
	}
	EndSubtree(d, g, "x", true)
	if l := left(); l != 450 {
		t.Fatalf("subtree: left %d", l)
	}
}

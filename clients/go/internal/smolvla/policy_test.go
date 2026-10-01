//go:build mujoco

package smolvla

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func TestChunkMatchesTheOracle(t *testing.T) {
	dir := modelDir(t)
	b, err := os.ReadFile("../../../fixtures/vla/chunk.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Image   [2]int      `json:"image"`
		Task    string      `json:"task"`
		State   []float64   `json:"state"`
		Seed    uint64      `json:"seed"`
		Actions [][]float64 `json:"actions"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	p, err := Open(dir, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got, err := p.Chunk(testImage(c.Image[0], c.Image[1]), c.Image[0], c.Image[1], c.Task, c.State, Noise(c.Seed), 6)
	if err != nil {
		t.Fatal(err)
	}
	worst := 0.0
	for r := range c.Actions {
		for i, want := range c.Actions[r] {
			worst = math.Max(worst, math.Abs(got[r][i]-want))
		}
	}
	t.Logf("largest difference from the oracle: %.3g", worst)
	if worst > ChunkTolerance {
		t.Fatalf("largest difference %.3g is over %g", worst, ChunkTolerance)
	}
}

func TestOpenChecksThePins(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, 1); err == nil {
		t.Fatal("an empty directory opened")
	}
	for name := range Pins {
		if err := os.WriteFile(dir+"/"+name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir, 1); err == nil {
		t.Fatal("files that do not match the pins opened")
	}
}

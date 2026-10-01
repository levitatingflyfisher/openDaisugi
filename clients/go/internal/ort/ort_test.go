//go:build mujoco

package ort

import (
	"strings"
	"testing"
)

const tiny = "../../../fixtures/vla/tiny.onnx"

func TestVersionIsThePin(t *testing.T) {
	if v := Version(); v != "1.23.2" {
		t.Fatalf("ONNX Runtime %s, want 1.23.2", v)
	}
}

func TestRunTiny(t *testing.T) {
	s, err := Open(tiny, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	y := make([]float32, 6)
	err = s.Run(
		[]Input{
			{Name: "x", F32: []float32{0, 1, 2, 3, 4, 5}, Shape: []int64{2, 3}},
			{Name: "k", I64: []int64{10, 20, 30, 40, 50, 60}, Shape: []int64{2, 3}},
		},
		[]Output{{Name: "y", Data: y}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{10, 22, 34, 46, 58, 70}
	for i := range want {
		if y[i] != want[i] {
			t.Fatalf("y = %v, want %v", y, want)
		}
	}
}

func TestErrorsAreErrors(t *testing.T) {
	if _, err := Open("no/such/model.onnx", 1); err == nil || !strings.Contains(err.Error(), "could not load") {
		t.Errorf("missing model: %v", err)
	}
	s, err := Open(tiny, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	x := Input{Name: "x", F32: make([]float32, 6), Shape: []int64{2, 3}}
	k := Input{Name: "k", I64: make([]int64, 6), Shape: []int64{2, 3}}
	for name, c := range map[string]struct {
		in  []Input
		out []Output
	}{
		"short output":   {[]Input{x, k}, []Output{{Name: "y", Data: make([]float32, 5)}}},
		"missing input":  {[]Input{x}, []Output{{Name: "y", Data: make([]float32, 6)}}},
		"unknown output": {[]Input{x, k}, []Output{{Name: "z", Data: make([]float32, 6)}}},
		"wrong shape":    {[]Input{{Name: "x", F32: make([]float32, 6), Shape: []int64{3, 2}}, k}, []Output{{Name: "y", Data: make([]float32, 6)}}},
		"data too short": {[]Input{{Name: "x", F32: make([]float32, 5), Shape: []int64{2, 3}}, k}, []Output{{Name: "y", Data: make([]float32, 6)}}},
		"two kinds":      {[]Input{{Name: "x", F32: make([]float32, 6), I64: make([]int64, 6), Shape: []int64{2, 3}}, k}, []Output{{Name: "y", Data: make([]float32, 6)}}},
	} {
		if err := s.Run(c.in, c.out); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

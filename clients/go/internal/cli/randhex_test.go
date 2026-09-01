package cli

import (
	"errors"
	"regexp"
	"testing"
)

func TestRandHex8(t *testing.T) {
	a, err := randHex8()
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(a) {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := randHex8()
	if a == b {
		t.Fatalf("two draws gave %s", a)
	}
	old := randRead
	defer func() { randRead = old }()
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	if s, err := randHex8(); err == nil || s != "" {
		t.Fatalf("a failed read gave %q, %v", s, err)
	}
}

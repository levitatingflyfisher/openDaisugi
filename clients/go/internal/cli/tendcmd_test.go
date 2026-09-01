package cli

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/tracejournal"
)

// GD-R-4: once a tend run has started writing, input this binary cannot
// read stops it (exit 2) without the refusal's "Nothing was changed.".
// The checks before the first write still say "Nothing was changed.",
// through refuse/tendErr; only afterWrites, used from the point the run
// writes on, says the run stopped after its first write.
func TestAfterWritesReportsAStoppedRunNotNothingChanged(t *testing.T) {
	for _, sentinel := range []error{tracejournal.ErrUnreadable, pathways.ErrUnreadable} {
		var errb bytes.Buffer
		e := &Env{Stderr: &errb}
		err := e.afterWrites("tend", fmt.Errorf("%w: the trace t01 is not a mapping", sentinel))
		var x *exitError
		if !errors.As(err, &x) || x.code != 2 {
			t.Fatalf("%v: exit %v", sentinel, err)
		}
		want := fmt.Sprintf("daisugi tend: %v: the trace t01 is not a mapping. The run stopped after its first write.\n", sentinel)
		if errb.String() != want {
			t.Fatalf("%v: got %q want %q", sentinel, errb.String(), want)
		}
		if bytes.Contains(errb.Bytes(), []byte("Nothing was changed")) {
			t.Fatalf("%v: still claims nothing changed: %q", sentinel, errb.String())
		}
	}
	// A non-unreadable error falls through to the normal failure path
	// (exit 1, Python's exception text), unchanged by afterWrites.
	var errb bytes.Buffer
	e := &Env{Stderr: &errb}
	err := e.afterWrites("tend", errors.New("boom"))
	var x *exitError
	if !errors.As(err, &x) || x.code != 1 {
		t.Fatalf("exit %v", err)
	}
	if want := "daisugi tend: boom\n"; errb.String() != want {
		t.Fatalf("got %q want %q", errb.String(), want)
	}
}

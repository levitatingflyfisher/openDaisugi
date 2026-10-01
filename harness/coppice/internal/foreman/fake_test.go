package foreman

import (
	"context"
	"errors"
	"fmt"
)

// FakeSummarizer is the deterministic summarizer of the tests and the
// golden fixtures. testdata/golden/README.md states it, so a port can make
// the same lines:
//
//	Compress: "c " + the first 60 bytes of the text (rune-safe clip)
//	Merge:    "m " + the first 40 bytes of left + " | " + the first 40 bytes of right
type fakeSummarizer struct {
	compress, merge int
	fail            bool
}

var errFake = errors.New("fake summarizer is down")

func (f *fakeSummarizer) Compress(_ context.Context, m Message, limit int) (string, error) {
	if f.fail {
		return "", errFake
	}
	f.compress++
	return "c " + clip(m.Text, 60), nil
}

func (f *fakeSummarizer) Merge(_ context.Context, id NodeID, left, right string, limit int) (string, error) {
	if f.fail {
		return "", errFake
	}
	f.merge++
	return "m " + clip(left, 40) + " | " + clip(right, 40), nil
}

// longSummarizer returns more than the limit, to test the clip.
type longSummarizer struct{}

func (longSummarizer) Compress(_ context.Context, m Message, limit int) (string, error) {
	return fmt.Sprintf("%0*d", limit*2, 7), nil
}

func (longSummarizer) Merge(_ context.Context, id NodeID, l, r string, limit int) (string, error) {
	return fmt.Sprintf("%0*d", limit*2, 8), nil
}

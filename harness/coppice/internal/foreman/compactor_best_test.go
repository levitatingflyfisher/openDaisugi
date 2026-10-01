package foreman

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTheShortestTryWinsNotTheLastOne(t *testing.T) {
	// Tries of 120, 90, 150, 140, 130 bytes: the shortest is the second.
	answers := []string{strings.Repeat("a", 120), strings.Repeat("b", 90), strings.Repeat("c", 150), strings.Repeat("d", 140), strings.Repeat("e", 130)}
	sc := &scriptedCall{answers: answers}
	m := &ModelSummarizer{Call: sc.call}
	got, err := m.Compress(ctx, Message{Text: strings.Repeat("m", 400)}, 64)
	if err != nil || sc.n != 5 {
		t.Fatalf("Compress = %q, %v after %d calls", got, err, sc.n)
	}
	if got != strings.Repeat("b", 64) {
		t.Fatalf("got %q", got)
	}
}

func TestALaterTryThatFailsKeepsTheShortestEarlierTry(t *testing.T) {
	n := 0
	m := &ModelSummarizer{Call: func(_ context.Context, _, _ string) (string, CallUsage, error) {
		n++
		switch n {
		case 1:
			return strings.Repeat("a", 100), CallUsage{}, nil
		case 2:
			return strings.Repeat("b", 80), CallUsage{}, nil
		}
		return "", CallUsage{}, errors.New("the model is down")
	}}
	got, err := m.Compress(ctx, Message{Text: strings.Repeat("m", 400)}, 64)
	if err != nil || got != strings.Repeat("b", 64) || n != 3 {
		t.Fatalf("Compress = %q, %v after %d calls", got, err, n)
	}
}

func TestAFirstTryThatFailsIsAnError(t *testing.T) {
	m := &ModelSummarizer{Call: func(_ context.Context, _, _ string) (string, CallUsage, error) {
		return "", CallUsage{}, errors.New("the model is down")
	}}
	if _, err := m.Compress(ctx, Message{Text: "x"}, 64); err == nil {
		t.Fatal("no error")
	}
}

func TestAnAnswerUpToEightBytesOverIsClippedAndEndsTheTries(t *testing.T) {
	sc := &scriptedCall{answers: []string{strings.Repeat("a", 72)}}
	m := &ModelSummarizer{Call: sc.call}
	got, err := m.Compress(ctx, Message{Text: strings.Repeat("m", 400)}, 64)
	if err != nil || sc.n != 1 || got != strings.Repeat("a", 64) {
		t.Fatalf("Compress = %q, %v after %d calls", got, err, sc.n)
	}
	// Nine bytes over asks again.
	sc = &scriptedCall{answers: []string{strings.Repeat("a", 73), "fits"}}
	m = &ModelSummarizer{Call: sc.call}
	got, err = m.Compress(ctx, Message{Text: strings.Repeat("m", 400)}, 64)
	if err != nil || sc.n != 2 || got != "fits" {
		t.Fatalf("Compress = %q, %v after %d calls", got, err, sc.n)
	}
}

func TestThePromptsKeepTheOwnersWordsAndClaimNoProgress(t *testing.T) {
	for name, p := range map[string]string{"compress": promptCompress, "merge": promptMerge} {
		for _, rule := range []string{"own words come first", "Never say more was done than", "Keep names, numbers and paths exactly as written"} {
			if !strings.Contains(p, rule) {
				t.Errorf("the %s prompt lacks %q", name, rule)
			}
		}
	}
}

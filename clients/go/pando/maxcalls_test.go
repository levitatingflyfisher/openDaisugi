package pando

import (
	"context"
	"strings"
	"testing"
)

// runTend runs `tend` with a model that always answers a line far over the
// limit, so each node asks CompactTries times. It returns the model calls,
// stdout and stderr.
func runTend(t *testing.T, data string, args ...string) (calls int, out, errs string) {
	t.Helper()
	var o, e strings.Builder
	env := CLIEnv{Out: &o, Err: &e, Now: newClock().now,
		NewSummarizer: func(string) (Summarizer, error) {
			return &ModelSummarizer{Call: func(context.Context, string, string) (string, CallUsage, error) {
				calls++
				return strings.Repeat("x", 4000), CallUsage{}, nil
			}}, nil
		}}
	code := Run(append([]string{"tend", "--yes", "--data-dir", data}, args...), env)
	if code != 0 {
		t.Fatalf("tend exit %d: %s", code, e.String())
	}
	return calls, o.String(), e.String()
}

func seedLong(t *testing.T, n int) string {
	t.Helper()
	data := t.TempDir()
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, strings.Repeat("word ", 300))
	}
	seedStore(t, data, "work", lines...)
	return data
}

// --max caps every model call, a re-ask for a shorter line included. It
// used to cap nodes, and each node could ask CompactTries times.
func TestMaxCapsEveryModelCall(t *testing.T) {
	data := seedLong(t, 12)
	calls, out, errs := runTend(t, data, "--max", "7")
	if calls != 7 {
		t.Fatalf("model calls = %d, want 7\n%s", calls, errs)
	}
	if !strings.Contains(out, "built:") {
		t.Fatalf("out: %q", out)
	}
	// Only completed nodes print a line; the last node, cut short by the
	// budget, still counts as built.
	if n := strings.Count(errs, "built level "); n < 1 || n > 7 {
		t.Fatalf("progress lines = %d\n%s", n, errs)
	}
}

func TestEachNodePrintsOneProgressLineAndQuietHidesThem(t *testing.T) {
	data := seedLong(t, 3)
	_, _, errs := runTend(t, data, "--max", "100")
	lines := 0
	for _, l := range strings.Split(errs, "\n") {
		if strings.HasPrefix(l, "built level ") {
			lines++
			if !strings.Contains(l, " index ") || !strings.Contains(l, "s (") || !strings.Contains(l, "model calls so far)") {
				t.Errorf("progress line: %q", l)
			}
		}
	}
	if lines < 3 {
		t.Fatalf("progress lines = %d\n%s", lines, errs)
	}
	data = seedLong(t, 3)
	_, _, errs = runTend(t, data, "--max", "100", "-q")
	if strings.Contains(errs, "built level") {
		t.Fatalf("-q still prints: %q", errs)
	}
}

func TestStatusCountsClippedNodes(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", strings.Repeat("word ", 300))
	env := CLIEnv{Out: &strings.Builder{}, Err: &strings.Builder{}, Now: newClock().now,
		NewSummarizer: func(string) (Summarizer, error) {
			return &ModelSummarizer{Call: func(context.Context, string, string) (string, CallUsage, error) {
				return "", CallUsage{}, nil // no text: the node is clipped
			}}, nil
		}}
	if code := Run([]string{"tend", "--yes", "-q", "--data-dir", data}, env); code != 0 {
		t.Fatalf("tend: %d", code)
	}
	_, out, _ := cli(t, "", "status", "--data-dir", data)
	if !strings.Contains(out, "clipped nodes: 1\n") {
		t.Fatalf("status: %s", out)
	}
	_, out, _ = cli(t, "", "status", "--json", "--data-dir", data)
	if !strings.Contains(out, `"clipped":1`) && !strings.Contains(out, `"clipped": 1`) {
		t.Fatalf("status json: %s", out)
	}
}

func TestMaxCapsEveryModelCallOfARedo(t *testing.T) {
	data := seedLong(t, 8)
	runTend(t, data, "-q")
	calls, _, errs := runTend(t, data, "--redo-level", "0", "--max", "9")
	if calls != 9 {
		t.Fatalf("redo model calls = %d, want 9\n%s", calls, errs)
	}
}

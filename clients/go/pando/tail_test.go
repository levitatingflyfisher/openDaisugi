package pando

import (
	"fmt"
	"strings"
	"testing"
)

// Five messages appended with no tend: a reader sees all five, though the
// saved view covers only the first. status and tend --dry-run say how many
// are not in the view yet, and the pending count is right.
func TestReadersSeeEveryAppendedMessage(t *testing.T) {
	data := t.TempDir()
	for i := 0; i < 5; i++ {
		text := fmt.Sprintf("message %d %s", i, strings.Repeat("w", 320))
		if code, _, errs := cli(t, "", "append", text, "--data-dir", data); code != 0 {
			t.Fatalf("append %d: %s", i, errs)
		}
	}
	dir, _ := StoreDir(data, "work")
	r, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Count() != 5 {
		t.Fatalf("a reader sees %d messages, want 5", r.Count())
	}
	if r.NotInView() < 1 || r.Pending() < 1 {
		t.Fatalf("not in view %d, pending %d: the test needs an untended tail", r.NotInView(), r.Pending())
	}
	code, out, _ := cli(t, "", "status", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "messages: 5\n") ||
		!strings.Contains(out, fmt.Sprintf("pending nodes: %d\n", r.Pending())) ||
		!strings.Contains(out, fmt.Sprintf("not in the view yet: %d\n", r.NotInView())) {
		t.Fatalf("status: %q", out)
	}
	code, out, errs := cli(t, "", "zoom", "4", "1", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "message 4 ") {
		t.Fatalf("zoom 4 1: %d %q %q", code, out, errs)
	}
	code, out, _ = cli(t, "", "log", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "message 4 ") {
		t.Fatalf("log: %q", out)
	}
	code, out, _ = cli(t, "", "recall", "message 4", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "4+1") {
		t.Fatalf("recall: %q", out)
	}
	code, out, _ = cli(t, "", "tend", "--dry-run", "--data-dir", data)
	if code != 0 || !strings.Contains(out, fmt.Sprintf("pending: %d\n", r.Pending())) ||
		!strings.Contains(out, fmt.Sprintf("not in the view yet: %d\n", r.NotInView())) {
		t.Fatalf("tend --dry-run: %q", out)
	}
}

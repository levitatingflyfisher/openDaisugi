package foreman

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fix round 2 of the import review (N-2, N-3, N-5).

// failImport fails every call for an imported message while bad is set.
type failImport struct {
	*recSummarizer
	bad bool
}

func (f *failImport) Compress(ctx context.Context, m Message, limit int) (string, error) {
	if f.bad && m.Kind == KindPast {
		return "", errors.New("quota")
	}
	return f.recSummarizer.Compress(ctx, m, limit)
}

// N-2: a live node that moves ahead of a failed import node and commits
// must not clear the import node's retry count.
func TestALiveCommitKeepsTheRetryStateOfAFailedImportNode(t *testing.T) {
	home := t.TempDir()
	long := strings.Repeat("x", 700)
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		claudeLine("user", "u0", "2026-03-01T10:00:00.000Z", `"`+long+`"`, ""))
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	if fed, err := r.im.Feed(ctx, r.c, &r.mu, nil); err != nil || !fed {
		t.Fatal(fed, err)
	}
	f := &failImport{recSummarizer: r.sum, bad: true}
	if err := r.c.Drain(ctx, f, &r.mu); err == nil {
		t.Fatal("the import call should have failed")
	}
	if _, err := r.c.Append(ctx, KindUser, "LIVE "+long); err != nil {
		t.Fatal(err)
	}
	// The live node builds first; the append also asked the import node
	// again at once, and it fails a second time: the count goes on.
	if err := r.c.Drain(ctx, f, &r.mu); err == nil {
		t.Fatal("the import call should have failed again")
	}
	if r.c.wait == nil || r.c.wait.fails != 2 {
		t.Fatalf("wait = %+v: the live commit erased the import node's failure count", r.c.wait)
	}
	if d, ok := r.c.RetryIn(); !ok || d != 2*time.Minute {
		t.Fatalf("RetryIn = %v %v, want the second step, 2 min", d, ok)
	}
}

// N-3: a skip dir reached through a symlink still skips the session whose
// cwd is the resolved path (Claude Code records the resolved cwd).
func TestImportSkipsADirReachedThroughASymlink(t *testing.T) {
	home := t.TempDir()
	base := t.TempDir()
	real := filepath.Join(base, "real", "coppice")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		`{"type":"user","uuid":"k1","sessionId":"s1","timestamp":"2026-03-03T10:00:00.000Z","cwd":"`+filepath.Join(real, "turns")+`","message":{"role":"user","content":"hello"}}`)
	for _, skip := range []string{filepath.Join(link, "coppice"), real} {
		o := ImportOpts{Skip: []string{skip}}
		if all, _ := Totals(Scan(home, o, ImportState{}, nil)); all.Messages != 0 {
			t.Fatalf("skip %s: scan = %+v, want the session skipped", skip, all)
		}
	}
	// And the other way: the cwd is the link form, the skip dir the real one.
	writeLines(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		`{"type":"user","uuid":"k1","sessionId":"s1","timestamp":"2026-03-03T10:00:00.000Z","cwd":"`+filepath.Join(link, "coppice", "turns")+`","message":{"role":"user","content":"hello"}}`)
	if all, _ := Totals(Scan(home, ImportOpts{Skip: []string{real}}, ImportState{}, nil)); all.Messages != 0 {
		t.Fatalf("link cwd: scan = %+v", all)
	}
	// A neighbour is still imported.
	if all, _ := Totals(Scan(home, ImportOpts{Skip: []string{filepath.Join(base, "real", "coppic")}}, ImportState{}, nil)); all.Messages != 1 {
		t.Fatalf("neighbour: scan = %+v", all)
	}
}

// N-5: a copied Codex rollout (same session id, other file name) brings no
// line twice.
func TestACopiedCodexRolloutImportsNoLineTwice(t *testing.T) {
	home := fakeHome(t)
	src := filepath.Join(home, ".codex", "sessions", "2026", "03", "05", "rollout-2026-03-05T08-00-00-c1.jsonl")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(home, ".codex", "sessions", "2026", "03", "06", "rollout-2026-03-06T09-00-00-copy.jsonl")
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := newImportRig(t, home)
	r.accept(ImportOpts{})
	r.pump(nil)
	n := 0
	for i := int64(0); i < r.c.Count(); i++ {
		m, _ := r.c.Message(i)
		if strings.Contains(m.Text, "write a test") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%q: the copied line was logged %d times, want once", allText(r.c), n)
	}
}

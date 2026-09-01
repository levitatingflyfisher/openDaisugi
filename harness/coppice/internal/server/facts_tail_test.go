//go:build unix

package server

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// copyFixture copies a fake transcript from testdata/facts into a temp dir
// and returns its path. Tests never read a real transcript.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "facts", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendFixture(t *testing.T, path, name string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "facts", name))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestAClaudeTranscriptGivesTheNewestModelAndTokensCountedOncePerMessage(t *testing.T) {
	tl := newTailer(copyFixture(t, "claude-transcript.jsonl"), kindClaude)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	want := Tokens{Fresh: 13, CacheRead: 300, CacheWrite: 20, Out: 12}
	if tl.sum != want {
		t.Fatalf("tokens %+v, want %+v (a message split over two lines counts once)", tl.sum, want)
	}
	if tl.model != "claude-fake-2" {
		t.Fatalf("model %q, want claude-fake-2 (the synthetic entry is not a model)", tl.model)
	}
}

func TestATailerReadsOnlyTheNewBytes(t *testing.T) {
	p := copyFixture(t, "claude-transcript.jsonl")
	tl := newTailer(p, kindClaude)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if tl.off != fi.Size() {
		t.Fatalf("offset %d after one read, want the file size %d", tl.off, fi.Size())
	}
	appendFixture(t, p, "claude-transcript-more.jsonl")
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	want := Tokens{Fresh: 14, CacheRead: 1300, CacheWrite: 22, Out: 15}
	if tl.sum != want || tl.model != "claude-fake-3" {
		t.Fatalf("after append: tokens %+v model %q, want %+v claude-fake-3", tl.sum, tl.model, want)
	}
	// A poll with nothing new changes nothing.
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.sum != want {
		t.Fatalf("a quiet poll moved tokens to %+v", tl.sum)
	}
}

func TestAPartLineIsReadOnceItEnds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"id":"m1","model":"claude-fake-1","usage":{"input_tokens":2,"output_tokens":1}}}`
	if err := os.WriteFile(p, []byte(line[:40]), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := newTailer(p, kindClaude)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.off != 0 || tl.model != "" {
		t.Fatalf("a part line moved the offset to %d", tl.off)
	}
	if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.model != "claude-fake-1" || tl.sum.Fresh != 2 || tl.sum.Out != 1 {
		t.Fatalf("model %q tokens %+v after the line ended", tl.model, tl.sum)
	}
}

func TestALineLongerThanTheCapIsSkippedNotStuckOn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	long := `{"type":"assistant","message":{"id":"big","model":"too-big","text":"` +
		strings.Repeat("x", 300) + `"}}` + "\n"
	good := `{"type":"assistant","message":{"id":"m2","model":"claude-fake-2","usage":{"output_tokens":4}}}` + "\n"
	if err := os.WriteFile(p, []byte(long+good), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := newTailer(p, kindClaude)
	tl.cap = 128
	for i := 0; i < 10; i++ {
		if err := tl.poll(); err != nil {
			t.Fatal(err)
		}
	}
	if tl.model != "claude-fake-2" || tl.sum.Out != 4 {
		t.Fatalf("model %q tokens %+v, want the line after the long one read", tl.model, tl.sum)
	}
}

func TestATruncatedTranscriptIsReadAgainFromTheStart(t *testing.T) {
	p := copyFixture(t, "claude-transcript.jsonl")
	tl := newTailer(p, kindClaude)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join("..", "..", "testdata", "facts", "claude-transcript-more.jsonl"))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	want := Tokens{Fresh: 1, CacheRead: 1000, CacheWrite: 2, Out: 3}
	if tl.sum != want {
		t.Fatalf("after truncation tokens %+v, want %+v", tl.sum, want)
	}
}

func TestATailerRefusesASymlinkADirectoryAndAFIFO(t *testing.T) {
	dir := t.TempDir()
	real := copyFixture(t, "claude-transcript.jsonl")
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, dir, fifo} {
		tl := newTailer(p, kindClaude)
		if err := tl.poll(); err == nil {
			t.Errorf("%s: read, want a refusal", p)
		}
		if !tl.refused {
			t.Errorf("%s: not marked refused", p)
		}
		if tl.sum != (Tokens{}) || tl.model != "" {
			t.Errorf("%s: facts %+v %q from a refused path", p, tl.sum, tl.model)
		}
	}
}

func TestAMissingTranscriptIsTriedAgainLater(t *testing.T) {
	p := filepath.Join(t.TempDir(), "later.jsonl")
	tl := newTailer(p, kindClaude)
	if err := tl.poll(); err == nil {
		t.Fatal("a missing file read without error")
	}
	if tl.refused {
		t.Fatal("a missing file was refused for good")
	}
	if err := os.WriteFile(p, []byte(`{"type":"assistant","message":{"model":"claude-fake-1"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.model != "claude-fake-1" {
		t.Fatalf("model %q", tl.model)
	}
}

func TestASprigTreeGivesModelTokensAndItsLastVerdict(t *testing.T) {
	tl := newTailer(copyFixture(t, "sprig-tree.jsonl"), kindSprig)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	want := Tokens{Fresh: 5, CacheRead: 90, CacheWrite: 8, Out: 5}
	if tl.sum != want || tl.model != "sprig-fake-2" {
		t.Fatalf("tokens %+v model %q, want %+v sprig-fake-2", tl.sum, tl.model, want)
	}
	v := tl.verdict
	if v == nil || v.Decision != "deny" || v.Tool != "bash" || v.Clause != "fake clause" || v.At != 4.0 {
		t.Fatalf("verdict %+v", v)
	}
	if tl.mode != "enforcing" {
		t.Fatalf("mode %q, want enforcing", tl.mode)
	}
}

func TestASprigTreeInTheWriterOwnByteShapeGivesNonZeroTokens(t *testing.T) {
	// sprig-tree-writer-shape.jsonl is not hand-typed: it is the actual bytes
	// harness/sprig's SessionWriter (session_tree.go) produced for a session
	// header plus one prompt plus one assistant entry with usage 11/22/33/44,
	// including its real key order (alphabetical, from json.Marshal on a
	// map[string]any) and its real header fields (v, harness, cacheKey, and
	// so on). Only the ids and timestamps were made readable afterward. This
	// is the case where sprig's own claude-code backend used to write zero
	// usage: this fixture proves coppice sums a REAL non-zero usage entry
	// correctly, not just the hand-built sprig-tree.jsonl fixture above.
	tl := newTailer(copyFixture(t, "sprig-tree-writer-shape.jsonl"), kindSprig)
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	want := Tokens{Fresh: 11, CacheRead: 22, CacheWrite: 33, Out: 44}
	if tl.sum != want || tl.model != "haiku" {
		t.Fatalf("tokens %+v model %q, want %+v haiku", tl.sum, tl.model, want)
	}
}

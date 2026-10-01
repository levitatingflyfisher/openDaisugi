package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"daisugi-verify/internal/pyjson"
)

func TestCountLines(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"", 0}, {"a", 1}, {"a\n", 1}, {"a\nb", 2}, {"a\rb\x0bc\u0085d\n", 1}} {
		if got := CountLines([]byte(c.in)); got != c.want {
			t.Errorf("CountLines(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:9/v1/chat/completions": "127.0.0.1",
		"http://u:p@Worker.Invalid:8/x":          "worker.invalid",
		"http://[::1]:11434/v1":                  "::1",
		"http://[::1/x":                          "",
		"http:///x":                              "",
		"no-scheme":                              "",
		"http://h?q=a@b":                         "h",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "::1": true, "127.0.0.1": true, "127.255.9.1": true,
		"127.0.0.256": false, "127.01.0.1": false, "128.0.0.1": false, "localhost.": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v", host, got)
		}
	}
}

func TestParseRule(t *testing.T) {
	v, _ := pyjson.Loads(`{"id": "big\n", "version": 1, "shape": "deny_redirect", "state": "active", "match": {"tool": "Read"}}`)
	if r, why := ParseRule(v, "a"); r != nil || why == "" {
		t.Fatalf("an id with a newline must not parse")
	}
	v, _ = pyjson.Loads(`{"id": "big", "version": 1, "shape": "deny_redirect", "state": "active", "match": {"tool": "Read"}}`)
	r, why := ParseRule(v, "a")
	if r == nil || r.MinLines.Text != "350" || r.AllowRemote {
		t.Fatalf("parse: %v %q", r, why)
	}
}

func TestMeasureFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, why := MeasureFile(p); m == nil || m.Lines != 2 || m.Size != 8 {
		t.Fatalf("measure: %v %q", m, why)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, why := MeasureFile(fifo); why != "not a regular file" {
		t.Fatalf("fifo: %q", why)
	}
	if _, why := MeasureFile("/tmp/a\x00b"); why != "the file cannot be opened" {
		t.Fatalf("nul: %q", why)
	}
}

func TestApplyDiffReadsLinesNotNumbers(t *testing.T) {
	text := "def a():\n    return 1\n\n\ndef b():\n    return 2\n"
	got := ApplyDiff("--- a/x.py\n+++ b/x.py\n@@ -90,2 +90,2 @@\n def b():\n-    return 2\n+    return 3\n", text)
	if !got.Applies || got.Text != strings.Replace(text, "return 2", "return 3", 1) {
		t.Fatalf("got %+v", got)
	}
	for diff, why := range map[string]string{
		"":                       "the diff has no hunk",
		"hello\n@@\n def a():\n": "line 1 before the first hunk is not a diff header",
		"@@\n def c():\n":        "hunk 1 does not match the file",
		"@@\n+new\n":             "hunk 1 has no context or removed lines, so it has no place in the file",
		"@@\n-\n":                "hunk 1 matches 3 places in the file",
		"@@\n def a():\n*bad\n":  "line 3 is not a context, removed or added line",
	} {
		if got := ApplyDiff(diff, text); got.Applies || got.Why != why {
			t.Errorf("%q: got %+v", diff, got)
		}
	}
}

func TestFenceOutrunsTheBackticks(t *testing.T) {
	for _, c := range [][3]string{
		{"x\n", "", "```\nx\n```"},
		{"x", "diff", "```diff\nx\n```"},
		{"a ``` b\n", "", "````\na ``` b\n````"},
	} {
		if got := Fence(c[0], c[1]); got != c[2] {
			t.Errorf("%q: got %q", c[0], got)
		}
	}
}

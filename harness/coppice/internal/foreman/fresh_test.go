package foreman

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const brokenName = "foreman.broken-20261008T235800Z"

// makeHistory writes a five-message chat in parent/foreman and returns
// the dir and its main day file.
func makeHistory(t *testing.T, parent string) (string, string) {
	t.Helper()
	dir := filepath.Join(parent, "foreman")
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	for i := 0; i < 5; i++ {
		mustAppend(t, c, KindUser, "old words")
	}
	c.Close()
	return dir, filepath.Join(dir, "main", "2026-10-08.jsonl")
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

type corruption struct {
	reason string
	apply  func(t *testing.T, dir, day string)
}

// Only parse and consistency faults count as a broken history.
var corruptions = map[string]corruption{
	"bad log line": {"a bad line in main/2026-10-08.jsonl", func(t *testing.T, dir, day string) { appendTo(t, day, "not json\n") }},
	"duplicate id": {"message 0 is in the log twice", func(t *testing.T, dir, day string) {
		data, _ := os.ReadFile(day)
		first := strings.SplitAfter(string(data), "\n")[0]
		appendTo(t, day, first)
	}},
	"id gap": {"message 2 is missing", func(t *testing.T, dir, day string) {
		data, _ := os.ReadFile(day)
		lines := strings.SplitAfter(string(data), "\n")
		os.WriteFile(day, []byte(strings.Join(append(lines[:2:2], lines[3:]...), "")), fileMode)
	}},
	"bad tree line": {"a bad line in tree/2026-10-08.jsonl", func(t *testing.T, dir, day string) {
		appendTo(t, filepath.Join(dir, "tree", "2026-10-08.jsonl"), "{\n")
	}},
	"node outside the log": {"a bad line in tree/2026-10-08.jsonl", func(t *testing.T, dir, day string) {
		appendTo(t, filepath.Join(dir, "tree", "2026-10-08.jsonl"), `{"l":3,"i":0,"text":"x","size":1}`+"\n")
	}},
	"view outside the log": {"view.json does not match the log", func(t *testing.T, dir, day string) {
		os.WriteFile(filepath.Join(dir, "view.json"), []byte(`{"count":99,"batch":false,"lines":[[0,0]]}`+"\n"), fileMode)
	}},
	"bad view": {"view.json is not valid JSON", func(t *testing.T, dir, day string) {
		os.WriteFile(filepath.Join(dir, "view.json"), []byte("[["), fileMode)
	}},
}

// coppice must always start: a history it cannot read is moved aside,
// whole, and a new chat starts.
func TestBrokenHistoryStartsFresh(t *testing.T) {
	for name, k := range corruptions {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir, day := makeHistory(t, parent)
			k.apply(t, dir, day)
			want := readAll(t, dir)

			c, err := Open(ctx, Config{Dir: dir, Summarizer: &fakeSummarizer{}, Now: newClock().now})
			if err != nil {
				t.Fatalf("Open on a broken history: %v", err)
			}
			defer c.Close()
			broken := filepath.Join(parent, brokenName)
			want1 := "Foreman history could not be read (" + k.reason + "); it was saved to " + broken + " and a new chat started."
			if c.Notice() != want1 {
				t.Fatalf("Notice = %q, want %q", c.Notice(), want1)
			}
			if c.Count() != 0 || c.Render() != "<chat>\n</chat>\n" {
				t.Fatalf("the new chat is not empty: %d messages", c.Count())
			}
			info, err := os.Stat(broken)
			if err != nil || info.Mode().Perm() != dirMode {
				t.Fatalf("broken dir: %v, %v", info, err)
			}
			if got := readAll(t, broken); got != want {
				t.Fatalf("the old history changed when it was moved:\n%s\nwant\n%s", got, want)
			}
			mustAppend(t, c, KindUser, "a new start")
			if c.Count() != 1 {
				t.Fatalf("Count = %d", c.Count())
			}
		})
	}
}

func TestBrokenHistoryNeverOverwritesAnEarlierOne(t *testing.T) {
	parent := t.TempDir()
	dir, day := makeHistory(t, parent)
	earlier := filepath.Join(parent, brokenName)
	if err := os.MkdirAll(earlier, dirMode); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(earlier, "keep"), []byte("earlier"), fileMode)
	appendTo(t, day, "not json\n")
	c, err := Open(ctx, Config{Dir: dir, Summarizer: &fakeSummarizer{}, Now: newClock().now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.Contains(c.Notice(), brokenName+"-2 ") {
		t.Fatalf("Notice = %q", c.Notice())
	}
	if data, _ := os.ReadFile(filepath.Join(earlier, "keep")); string(data) != "earlier" {
		t.Fatal("the earlier broken history was touched")
	}
}

func TestHealthyOpenHasNoNotice(t *testing.T) {
	parent := t.TempDir()
	dir, _ := makeHistory(t, parent)
	c := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
	if c.Notice() != "" || c.Count() != 5 {
		t.Fatalf("Notice %q, Count %d", c.Notice(), c.Count())
	}
}

// A reader never moves anything: it reports the error and the path.
func TestReaderReportsABrokenHistory(t *testing.T) {
	parent := t.TempDir()
	dir, day := makeHistory(t, parent)
	appendTo(t, day, "not json\n")
	before := readAll(t, dir)
	_, err := OpenReadOnly(dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("OpenReadOnly = %v, want an error naming %s", err, dir)
	}
	if readAll(t, dir) != before {
		t.Fatal("the reader changed the history")
	}
	ents, _ := os.ReadDir(parent)
	if len(ents) != 1 {
		t.Fatalf("the reader moved something: %v", ents)
	}
	code, _, errOut := runCLI(t, "", "--data-dir", dir, "view")
	if code != 1 || !strings.Contains(errOut, dir) {
		t.Fatalf("CLI = %d %q", code, errOut)
	}
}

// An I/O error is not a broken history: Open returns it and moves nothing.
func TestIOErrorLeavesTheHistoryInPlace(t *testing.T) {
	for _, name := range []string{"injected EIO", "unreadable file"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir, day := makeHistory(t, parent)
			if name == "unreadable file" {
				os.Chmod(day, 0)
				defer os.Chmod(day, 0o600)
			} else {
				real := readFile
				readFile = func(p string) ([]byte, error) {
					if p == day {
						return nil, &os.PathError{Op: "read", Path: p, Err: syscall.EIO}
					}
					return real(p)
				}
				defer func() { readFile = real }()
			}
			c, err := Open(ctx, Config{Dir: dir, Summarizer: &fakeSummarizer{}, Now: newClock().now})
			if err == nil {
				c.Close()
				t.Fatal("Open hid an I/O error")
			}
			var herr *historyError
			if errors.As(err, &herr) {
				t.Fatalf("an I/O error counted as a broken history: %v", err)
			}
			ents, _ := os.ReadDir(parent)
			if len(ents) != 1 || ents[0].Name() != "foreman" {
				t.Fatalf("the dir moved: %v", ents)
			}
		})
	}
}

// If the rename works but a later step fails, the error names where the
// history now is.
func TestMoveAsideErrorNamesTheBrokenPath(t *testing.T) {
	parent := t.TempDir()
	dir, day := makeHistory(t, parent)
	appendTo(t, day, "not json\n")
	real := chmod
	chmod = func(p string, m os.FileMode) error {
		if strings.Contains(p, ".broken-") {
			return errInjected
		}
		return real(p, m)
	}
	defer func() { chmod = real }()
	_, err := Open(ctx, Config{Dir: dir, Summarizer: &fakeSummarizer{}, Now: newClock().now})
	broken := filepath.Join(parent, brokenName)
	if err == nil || !strings.Contains(err.Error(), broken) || !errors.Is(err, errInjected) {
		t.Fatalf("Open = %v, want an error naming %s", err, broken)
	}
	if _, serr := os.Stat(broken); serr != nil {
		t.Fatalf("the history is not at the named path: %v", serr)
	}
}

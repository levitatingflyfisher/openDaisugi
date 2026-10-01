package foreman

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/golden from the Go reference")

const goldenDir = "testdata/golden"

// goldenScript is the fixed input the golden files come from. A port reads
// script.json and must write the same out/ files byte for byte.
type goldenScript struct {
	Config struct {
		NodeBytes  int `json:"node_bytes"`
		ViewMax    int `json:"view_max"`
		ViewMin    int `json:"view_min"`
		SplitBytes int `json:"split_bytes"`
	} `json:"config"`
	Messages []goldenMsg   `json:"messages"`
	Probes   []goldenProbe `json:"probes"`
}

type goldenMsg struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Date string `json:"date"`
}

type goldenProbe struct {
	Op  string `json:"op"`
	ID  int64  `json:"id"`
	N   int64  `json:"n,omitempty"`
	Out string `json:"out,omitempty"`
	Err bool   `json:"err,omitempty"`
}

const multiLine = "first line\nsecond line\r\nthird line\rfourth line"

func makeScript() goldenScript {
	var s goldenScript
	s.Config.NodeBytes = 512
	s.Config.ViewMax = 4096
	s.Config.ViewMin = 2048
	s.Config.SplitBytes = 1024
	kinds := []string{KindUser, KindForeman, KindWork, KindPast, KindNote}
	words := []string{"gate", "pane", "worker", "brief", "envelope", "ledger", "sprig", "coppice", "spawn", "report"}
	start := time.Date(2026, 10, 7, 23, 30, 0, 0, time.UTC)
	for k := 0; k < 160; k++ {
		var text string
		switch {
		case k == 40:
			text = strings.Repeat("split me across messages. ", 100) // 2600 bytes
		case k == 41:
			text = "naïve café, 日本語のテキスト, a tree 🌲, and ünïcode at a cut: " + strings.Repeat("é", 520)
		case k == 42:
			text = "word for word: {\"out\":\"a\\nb\"} and 100% kept"
		case k == 43:
			text = strings.Repeat("a", 1000) + multiLine + " after"
		case k == 44:
			text = "<b>html</b> & \"quotes\" \\ back\tslash"
		case k == 45:
			text = ""
		case k%9 == 0:
			text = fmt.Sprintf("long entry %d: ", k) + strings.Repeat(words[k%len(words)]+" status and plan; ", 30)
		default:
			n := 1 + (k*7)%12
			parts := make([]string, n)
			for j := range parts {
				parts[j] = words[(k+j*3)%len(words)]
			}
			text = fmt.Sprintf("entry %d: %s", k, strings.Join(parts, " "))
		}
		s.Messages = append(s.Messages, goldenMsg{
			Kind: kinds[k%len(kinds)],
			Text: text,
			Date: start.Add(time.Duration(k) * time.Minute).Format(DateLayout),
		})
	}
	for _, p := range [][2]int64{{0, 1}, {40, 1}, {45, 1}, {0, 64}, {64, 32}, {128, 16}, {160, 2}, {3, 2}, {0, 512}} {
		s.Probes = append(s.Probes, goldenProbe{Op: "zoom", ID: p[0], N: p[1]})
	}
	for _, id := range []int64{0, 29, 30, 163, 99999} {
		s.Probes = append(s.Probes, goldenProbe{Op: "date", ID: id})
	}
	return s
}

func encodeIndent(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// runGolden plays the script into dir and returns the extra outputs.
func runGolden(t *testing.T, s goldenScript, dir string) map[string][]byte {
	t.Helper()
	clk := &testClock{}
	c, err := Open(ctx, Config{
		Dir: dir, Summarizer: &fakeSummarizer{}, Now: clk.now,
		NodeBytes: s.Config.NodeBytes, ViewMax: s.Config.ViewMax, ViewMin: s.Config.ViewMin,
		SplitBytes: s.Config.SplitBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var views bytes.Buffer
	for k, m := range s.Messages {
		d, err := time.Parse(DateLayout, m.Date)
		if err != nil {
			t.Fatal(err)
		}
		clk.t = d
		if _, err := c.Append(ctx, m.Kind, m.Text); err != nil {
			t.Fatalf("script message %d: %v", k, err)
		}
		vj, _ := c.ViewJSON()
		fmt.Fprintf(&views, "{\"step\":%d,\"size\":%d,\"view\":%s}\n", k, c.ViewSize(), bytes.TrimSuffix(vj, []byte("\n")))
	}
	probes := make([]goldenProbe, len(s.Probes))
	for k, p := range s.Probes {
		var out string
		var err error
		switch p.Op {
		case "zoom":
			out, err = c.Zoom(p.ID, p.N)
		case "date":
			out, err = c.Date(p.ID)
		}
		p.Out, p.Err = out, err != nil
		probes[k] = p
	}
	return map[string][]byte{
		"views.jsonl": views.Bytes(),
		"view.txt":    []byte(c.Render()),
		"probes.json": encodeIndent(probes),
	}
}

func filesUnder(t *testing.T, root string) map[string][]byte {
	t.Helper()
	got := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == "lock" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		got[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The Go reference's outputs for the fixed script, kept for the Rust port
// to compare against byte for byte. Run with -update to rewrite them.
func TestGoldenFixtures(t *testing.T) {
	s := makeScript()
	dir := t.TempDir()
	extra := runGolden(t, s, dir)
	got := filesUnder(t, dir)
	for k, v := range extra {
		got[k] = v
	}
	scriptJSON := encodeIndent(s)
	outDir := filepath.Join(goldenDir, "out")
	if *update {
		if err := os.RemoveAll(outDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, "script.json"), scriptJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		for name, data := range got {
			p := filepath.Join(outDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantScript, err := os.ReadFile(filepath.Join(goldenDir, "script.json"))
	if err != nil {
		t.Fatalf("no golden script; run go test -run TestGoldenFixtures -update: %v", err)
	}
	if !bytes.Equal(wantScript, scriptJSON) {
		t.Fatal("script.json differs from makeScript; run with -update if the change is meant")
	}
	want := filesUnder(t, outDir)
	var names []string
	for n := range want {
		names = append(names, n)
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if !bytes.Equal(got[n], want[n]) {
			t.Errorf("golden %s differs (have %d bytes, want %d)", n, len(got[n]), len(want[n]))
		}
	}
}

// The golden script must reach every path the port has to copy: a split,
// a summary, a merge, a batch and a day change.
func TestGoldenScriptCoversThePaths(t *testing.T) {
	s := makeScript()
	dir := t.TempDir()
	runGolden(t, s, dir)
	files := filesUnder(t, dir)
	all := ""
	mains, trees := 0, 0
	for n, d := range files {
		all += string(d)
		if strings.HasPrefix(n, "main/") {
			mains++
		}
		if strings.HasPrefix(n, "tree/") {
			trees++
		}
	}
	// The log keeps the text word for word, escapes and all.
	for _, want := range []string{`"text":"c `, `"text":"m `, `word for word: {\"out\":\"a\\nb\"} and 100% kept`} {
		if !strings.Contains(all, want) {
			t.Errorf("golden output has no %q", want)
		}
	}
	if mains < 2 || trees < 2 {
		t.Errorf("the script does not cross a day: %d main and %d tree files", mains, trees)
	}
	var vf viewFile
	if err := json.Unmarshal(files["view.json"], &vf); err != nil {
		t.Fatal(err)
	}
	if vf.Count <= int64(len(s.Messages)) {
		t.Errorf("no split: %d messages from %d script lines", vf.Count, len(s.Messages))
	}
	merged := false
	for _, l := range vf.Lines {
		merged = merged || l[0] > 0
	}
	if !merged {
		t.Error("the view never merged")
	}
}

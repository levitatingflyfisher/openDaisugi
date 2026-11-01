package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pandoRun runs daisugi with its own HOME and data home, never the real ones.
func pandoRun(t *testing.T, data string, stdin string, extraEnv []string, args ...string) (int, string, string) {
	t.Helper()
	var out, errs strings.Builder
	home := t.TempDir()
	env := append([]string{"HOME=" + home, "OPENDAISUGI_HOME=" + data, "PATH=/usr/bin:/bin"}, extraEnv...)
	code := Main(&Env{Args: args, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errs, Environ: env})
	return code, out.String(), errs.String()
}

func TestPandoAppendLogZoomDateAndTheTwoStores(t *testing.T) {
	data := t.TempDir()
	code, out, errs := pandoRun(t, data, "", nil, "pando", "append", "we chose sqlite for the index")
	if code != 0 || out != "0\n" {
		t.Fatalf("append: %d %q %q", code, out, errs)
	}
	code, out, _ = pandoRun(t, data, "", nil, "pando", "zoom", "0", "1")
	if code != 0 || out != "we chose sqlite for the index\n" {
		t.Fatalf("zoom: %d %q", code, out)
	}
	code, out, _ = pandoRun(t, data, "", nil, "pando", "date", "0")
	if code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "Z") {
		t.Fatalf("date: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(data, "pando", "work", "main")); err != nil {
		t.Fatalf("the default store is work under the data home: %v", err)
	}
	code, _, errs = pandoRun(t, data, "", nil, "pando", "status", "--store", "home")
	if code != 1 || !strings.Contains(errs, "no store home at") {
		t.Fatalf("home must be a separate, missing store: %d %q", code, errs)
	}
	code, _, errs = pandoRun(t, data, "", nil, "pando", "recall", "sqlite", "--store", "all")
	if code != 2 || !strings.Contains(errs, "one store at a time") {
		t.Fatalf("all: %d %q", code, errs)
	}
}

func TestPandoTendWithAFakeClaude(t *testing.T) {
	data := t.TempDir()
	long := strings.Repeat("word ", 300)
	if code, _, errs := pandoRun(t, data, long, nil, "pando", "append", "-"); code != 0 {
		t.Fatal(errs)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\necho 'a short summary line'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errs := pandoRun(t, data, "", nil, "pando", "tend", "--dry-run")
	if code != 0 || !strings.Contains(out, "pending: 1") {
		t.Fatalf("dry run: %d %q %q", code, out, errs)
	}
	code, out, errs = pandoRun(t, data, "", []string{"PATH=" + bin + ":/usr/bin:/bin"}, "pando", "tend")
	if code != 0 || !strings.Contains(out, "built: 1") || !strings.Contains(out, "pending: 0") {
		t.Fatalf("tend: %d %q %q", code, out, errs)
	}
	code, out, _ = pandoRun(t, data, "", nil, "pando", "view")
	if code != 0 || !strings.Contains(out, "a short summary line") {
		t.Fatalf("view: %d %q", code, out)
	}
}

// The summarizer's own claude call is not the owner's chat: it carries the
// skip marker, so the user's Claude Code hooks give it no view and log
// nothing (round 7 fix).
func TestPandoTendClaudeCallCarriesTheSkipMarker(t *testing.T) {
	data := t.TempDir()
	if code, _, errs := pandoRun(t, data, strings.Repeat("word ", 300), nil, "pando", "append", "-"); code != 0 {
		t.Fatal(errs)
	}
	bin := t.TempDir()
	seen := filepath.Join(bin, "seen")
	script := "#!/bin/sh\ncat >/dev/null\necho \"skip=$DAISUGI_PANDO_SKIP\" > " + seen + "\necho 'a short summary line'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errs := pandoRun(t, data, "", []string{"PATH=" + bin + ":/usr/bin:/bin"}, "pando", "tend")
	if code != 0 {
		t.Fatalf("tend: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(seen); strings.TrimSpace(string(b)) != "skip=1" {
		t.Fatalf("the model call has no skip marker: %q", b)
	}
}

// A profile on the openai wire reaches a fake server: the body names the
// profile's max_tokens (1024 when it names none), and a reply with no text
// that stopped for length is a fallback, never the reasoning field.
func TestPandoSummaryCallUsesTheProfileMaxTokensAndNeverTheReasoning(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"","reasoning_content":"SECRET REASONING"},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()
	for _, tc := range []struct{ profile, want string }{
		{"{wire: openai, base_url: '" + srv.URL + "/v1', model: openai/m, max_tokens: 3000}", `"max_tokens":3000`},
		{"{wire: openai, base_url: '" + srv.URL + "/v1', model: openai/m}", `"max_tokens":1024`},
	} {
		data := t.TempDir()
		if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte("agent_profiles:\n  local: "+tc.profile+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, errs := pandoRun(t, data, strings.Repeat("word ", 300), nil, "pando", "append", "-"); code != 0 {
			t.Fatal(errs)
		}
		mu.Lock()
		bodies = nil
		mu.Unlock()
		code, out, errs := pandoRun(t, data, "", nil, "pando", "tend", "--profile", "local")
		if code != 0 || !strings.Contains(out, "built: 1, fell back to clipped: 1\n") || !strings.Contains(out, "--redo-clipped") {
			t.Fatalf("%s: tend %d %q %q", tc.want, code, out, errs)
		}
		mu.Lock()
		if len(bodies) != 1 || !strings.Contains(bodies[0], tc.want) {
			t.Fatalf("want %s in the request, got %v", tc.want, bodies)
		}
		mu.Unlock()
		code, out, _ = pandoRun(t, data, "", nil, "pando", "view")
		if code != 0 || strings.Contains(out, "SECRET REASONING") || !strings.Contains(out, "[clipped] word word") {
			t.Fatalf("the view must hold the clipped source, not the reasoning: %d %q", code, out)
		}
	}
}

func TestPandoProfileMaxTokensIsCheckedBeforeATendRuns(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte("agent_profiles:\n  local: {wire: openai, max_tokens: 0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := pandoRun(t, data, strings.Repeat("word ", 300), nil, "pando", "append", "-"); code != 0 {
		t.Fatal(errs)
	}
	code, _, errs := pandoRun(t, data, "", nil, "pando", "tend", "--profile", "local")
	if code != 1 || !strings.Contains(errs, "max_tokens must be a whole number from 1 to 1000000") {
		t.Fatalf("%d %q", code, errs)
	}
}

// A profile's timeout_s stretches the HTTP client: a reply that takes 2 s is
// cut by the environment's own 1 s limit, and read when the profile names
// timeout_s 5.
func TestPandoProfileTimeoutStretchesTheHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"a short summary line"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		ts   string
		code int
		want string
	}{
		{"", 1, "tend stopped"},
		{", timeout_s: 5", 0, "built: 1, fell back to clipped: 0"},
	} {
		data := t.TempDir()
		cfg := "agent_profiles:\n  local: {wire: openai, base_url: '" + srv.URL + "/v1', model: openai/m" + tc.ts + "}\n"
		if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, errs := pandoRun(t, data, strings.Repeat("word ", 300), nil, "pando", "append", "-"); code != 0 {
			t.Fatal(errs)
		}
		code, out, errs := pandoRun(t, data, "", []string{"OPENDAISUGI_LLM_TIMEOUT=1"}, "pando", "tend", "--profile", "local")
		if code != tc.code || !strings.Contains(out+errs, tc.want) {
			t.Fatalf("timeout %q: %d %q %q", tc.ts, code, out, errs)
		}
	}
}

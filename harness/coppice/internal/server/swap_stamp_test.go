//go:build unix

package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/layout"
)

// A pane whose own env sets a base URL to the gateway starts with the pane
// prefix on it, so the gateway can name the pane on its turn record (F12a).

// capsGateway is a loopback server that answers GET /_caps with status and
// body, and counts the asks. It answers its base URL.
func capsGateway(t *testing.T, status int, body string) (string, *atomic.Int32) {
	t.Helper()
	var asks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/_caps" {
			w.WriteHeader(404)
			return
		}
		asks.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &asks
}

const capsYes = `{"pane_prefix": true}`

func TestStampURLPutsThePrefixAheadOfThePathAndReplacesOldStamps(t *testing.T) {
	const floor = "0123456789abcdef"
	for _, c := range []struct{ in, want string }{
		{"http://127.0.0.1:8787", "http://127.0.0.1:8787/pane/" + floor + "/w1:p2"},
		{"http://127.0.0.1:8787/", "http://127.0.0.1:8787/pane/" + floor + "/w1:p2"},
		{"http://127.0.0.1:8787/v1", "http://127.0.0.1:8787/pane/" + floor + "/w1:p2/v1"},
		{"http://127.0.0.1:8787/pane/fedcba9876543210/w9:p9", "http://127.0.0.1:8787/pane/" + floor + "/w1:p2"},
		{"http://127.0.0.1:8787/pane/fedcba9876543210/w9:p9/v1", "http://127.0.0.1:8787/pane/" + floor + "/w1:p2/v1"},
		{"not a url", "not a url"},
	} {
		if got := stampURL(c.in, floor, "w1:p2"); got != c.want {
			t.Errorf("stampURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAPaneStartsWithTheStampOnItsOwnGatewayURLOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw, _ := capsGateway(t, 200, capsYes)
	if err := config.Save(config.Config{Gateway: &gw}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_BASE_URL", gw) // inherited from the server: left alone
	s := newPaneServer(t)
	rec := layout.Pane{ID: "w1:p2", Env: map[string]string{
		"ANTHROPIC_BASE_URL": gw,
		"OTHER":              "x",
	}}
	got := s.stampedEnv(rec)
	want := gw + "/pane/" + s.floorKey() + "/w1:p2"
	if got["ANTHROPIC_BASE_URL"] != want || got["OTHER"] != "x" {
		t.Fatalf("env = %v, want ANTHROPIC_BASE_URL %s", got, want)
	}
	if _, ok := got["OPENAI_BASE_URL"]; ok {
		t.Fatalf("an inherited base URL was stamped: %v", got)
	}
	if rec.Env["ANTHROPIC_BASE_URL"] != gw {
		t.Fatalf("the record's env changed: %v", rec.Env)
	}
	// Direct, or another address, is left as it is.
	rec.Env["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
	if got := s.stampedEnv(rec); got["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("a direct URL was stamped: %v", got)
	}
}

func TestAProcessGetsTheStampedURLInItsEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw, _ := capsGateway(t, 200, capsYes)
	if err := config.Save(config.Config{Gateway: &gw}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	got := roundTrip(t, s,
		`{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","kind":"pty","cmd_argv":["sh","-c","echo URL=$ANTHROPIC_BASE_URL; sleep 30"],`+
			`"env":{"ANTHROPIC_BASE_URL":"`+gw+`"}}`)
	id, _ := result(t, got[0])["pane"].(string)
	want := "URL=" + gw + "/pane/" + s.floorKey() + "/" + id
	got = roundTrip(t, s, `{"id":"2","cmd":"pane.wait_output","pane":"`+id+`","contains":"`+want+`","timeout_ms":5000}`)
	if !got[0].OK {
		t.Fatalf("the process never printed %s: %+v", want, got[0].Error)
	}
	rec, _ := s.tree.Pane(id)
	if rec.Env["ANTHROPIC_BASE_URL"] != gw {
		t.Fatalf("the record keeps the plain URL, got %q", rec.Env["ANTHROPIC_BASE_URL"])
	}
}

// A record that names a floor counts only on that floor: pane ids start at
// w1:p1 on every floor, and floors share a gateway journal.
func TestFloorFactsCountsARecordOfAnotherFloorForNoPane(t *testing.T) {
	s := newFactsServer(t)
	s.cfg.GateRoot = filepath.Join(t.TempDir(), "gate")
	now := s.clock().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	dir := filepath.Join(filepath.Dir(s.cfg.GateRoot), "gateway")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(saved int, floor string) string {
		f := ""
		if floor != "" {
			f = fmt.Sprintf(`,"floor":%q`, floor)
		}
		return fmt.Sprintf(`{"created_at":%q,"frontier_tokens_saved":%d,"model":"m","pane":"w1:p1"%s}`, now, saved, f)
	}
	text := strings.Join([]string{line(100, s.floorKey()), line(7000, "0000000000000000"), line(20, "")}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "turns.jsonl"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	v, ok := savedToday(t, s)
	m, _ := v.(map[string]any)
	if !ok || m["tokens"] != float64(120) {
		t.Fatalf("saved_today %v, want tokens 120 (this floor's 100 and the floorless 20)", v)
	}
}

// coppice stamps only when the gateway says it takes the prefix. An older
// gateway forwards /_caps and answers an error, or nothing: the pane then
// keeps its plain URL and works as before, with no per-pane figure.
func TestAPaneIsNotStampedUnlessTheGatewaySaysItTakesThePrefix(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"an old gateway that forwards it", 404, `{"error":"not found"}`},
		{"a gateway that says no", 200, `{"pane_prefix": false}`},
		{"a gateway that says nothing", 200, `{}`},
		{"an answer that is not JSON", 200, `ok`},
		{"a refusal", 403, `{"error":"caps is loopback only"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			gw, asks := capsGateway(t, c.status, c.body)
			if err := config.Save(config.Config{Gateway: &gw}); err != nil {
				t.Fatal(err)
			}
			s := newPaneServer(t)
			rec := layout.Pane{ID: "w1:p2", Env: map[string]string{"ANTHROPIC_BASE_URL": gw}}
			if got := s.stampedEnv(rec); got["ANTHROPIC_BASE_URL"] != gw {
				t.Fatalf("env = %v, want the plain URL %s", got, gw)
			}
			if asks.Load() != 1 {
				t.Fatalf("asked %d times, want 1", asks.Load())
			}
			// The no is kept for a while, so the next start does not ask again.
			s.stampedEnv(rec)
			if asks.Load() != 1 {
				t.Fatalf("asked %d times after a second start, want 1", asks.Load())
			}
		})
	}
	t.Run("a gateway that does not answer", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		gw := "http://127.0.0.1:1"
		if err := config.Save(config.Config{Gateway: &gw}); err != nil {
			t.Fatal(err)
		}
		s := newPaneServer(t)
		rec := layout.Pane{ID: "w1:p2", Env: map[string]string{"ANTHROPIC_BASE_URL": gw}}
		if got := s.stampedEnv(rec); got["ANTHROPIC_BASE_URL"] != gw {
			t.Fatalf("env = %v, want the plain URL", got)
		}
	})
}

// A yes is asked once per gateway address, however many panes start.
func TestTheGatewayIsAskedOncePerAddressWhenItSaysYes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw, asks := capsGateway(t, 200, capsYes)
	if err := config.Save(config.Config{Gateway: &gw}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	for _, id := range []string{"w1:p1", "w1:p2", "w1:p3"} {
		rec := layout.Pane{ID: id, Env: map[string]string{"ANTHROPIC_BASE_URL": gw}}
		if got := s.stampedEnv(rec); !strings.Contains(got["ANTHROPIC_BASE_URL"], "/pane/") {
			t.Fatalf("%s: env = %v, want a stamp", id, got)
		}
	}
	if asks.Load() != 1 {
		t.Fatalf("asked %d times, want 1", asks.Load())
	}
}

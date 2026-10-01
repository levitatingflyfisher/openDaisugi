package llmcheck

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/llm"
)

// fakeClaude writes a `claude` that reads its stdin into seen and prints
// out, exiting with code; the client finds it by name only.
func fakeClaude(t *testing.T, out string, code int) (*llm.Client, string) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	script := "#!/bin/sh\ncat > " + seen + "\nprintf '%s' '" + out + "'\necho boom >&2\nexit " +
		string(rune('0'+code)) + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	c := llm.New(llm.Env{Getenv: getenv, Home: dir, LookPath: func(name string) (string, error) {
		return bin, nil
	}})
	c.TempDir = dir
	return c, seen
}

func ccEnv(c *llm.Client) Env {
	return Env{Getenv: func(string) (string, bool) { return "", false }, Backend: "claude-code", Claude: c}
}

func TestClaudeCodeVerdict(t *testing.T) {
	c, seen := fakeClaude(t, `{"satisfied": true, "rationale": "kind"}`, 0)
	o := Invoke(ccEnv(c), "is it kind", `{"task": "t"}`)
	if o.Raised || o.Unported != "" || !o.Satisfied || o.Reason != "kind" {
		t.Fatalf("got %+v", o)
	}
	got, _ := os.ReadFile(seen)
	want := "[system]\n" + System + "\n\n[user]\nRule:\nis it kind\n\nPlan payload (JSON):\n" +
		`{"task": "t"}` + "\n\nDoes the plan payload satisfy the rule?"
	if string(got) != want {
		t.Fatalf("prompt %q", got)
	}
}

func TestClaudeCodeDictLiteral(t *testing.T) {
	c, _ := fakeClaude(t, `{"satisfied": True, "rationale": None}`, 0)
	o := Invoke(ccEnv(c), "r", "{}")
	if !o.Satisfied || o.Reason != "None" {
		t.Fatalf("got %+v", o)
	}
}

// A failed claude run is a verdict of not satisfied, not an error: the
// oracle catches EnvelopeGenerationError in _invoke_model.
func TestClaudeCodeFailureIsNotSatisfied(t *testing.T) {
	c, _ := fakeClaude(t, "", 3)
	res, why := Run(ccEnv(c), "r", "{}")
	if why != "" || res.Errored || res.Satisfied || res.Reason != "llm-check failed: claude -p exited 3: 'boom\\n'" {
		t.Fatalf("got %+v %q", res, why)
	}
	c, _ = fakeClaude(t, "I think so.", 0)
	res, _ = Run(ccEnv(c), "r", "{}")
	if res.Errored || res.Satisfied || !strings.HasPrefix(res.Reason, "llm-check failed: no JSON object in claude -p stdout") {
		t.Fatalf("got %+v", res)
	}
}

func TestRenamedBackendFailsClosed(t *testing.T) {
	res, why := Run(Env{Getenv: func(string) (string, bool) { return "", false }, Backend: " litellm "}, "r", "{}")
	if why != "" || !res.Errored || res.Reason != "error: llm_check call failed: "+llm.RenamedText {
		t.Fatalf("got %+v", res)
	}
}

func TestHTTPReplyThatIsNoDict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"type": "message", "role": "assistant", "content": [{"type": "text", "text": "[1, 2]"}],`+
			` "stop_reason": "end_turn", "usage": {"input_tokens": 1, "output_tokens": 1}}`)
	}))
	defer srv.Close()
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-test", "ANTHROPIC_API_BASE": srv.URL}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	res, why := Run(Env{Getenv: getenv, Vars: env, Backend: "api"}, "r", "{}")
	if why != "" || !res.Errored || res.Reason != "error: llm_check call failed: 'list' object has no attribute 'get'" {
		t.Fatalf("got %+v %q", res, why)
	}
}

// A setting httpx reads in a way the port does not model leaves the
// check undecided.
func TestHTTPUnportedSetting(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-test", "SSL_CERT_FILE": "/x"}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	_, why := Run(Env{Getenv: getenv, Vars: env, Backend: "api"}, "r", "{}")
	if !strings.Contains(why, "SSL_CERT_FILE") {
		t.Fatalf("got %q", why)
	}
}

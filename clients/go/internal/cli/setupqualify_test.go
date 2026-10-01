package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// tiers setup --endpoint: the backend is pinned, and a failure says why.

const validEnvelopeReply = `{"generated_by":"model","task":"t","permissions":{"file_read":["/work/**"],` +
	`"file_write":[],"network":false,"shell":false,"shell_allowlist":[],"max_execution_time_s":30,` +
	`"max_output_size_mb":10},"invariants":[],"postconditions":[]}`

// chatServer answers /v1/chat/completions with handler and counts the hits.
func chatServer(t *testing.T, hits *atomic.Int32, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			hits.Add(1)
			handler(w)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatReply(w http.ResponseWriter, message map[string]any, finish string) {
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// setupEnv runs tiers setup with a stub `claude` first on PATH, the box
// where the bug was found, and any extra environment entries.
func setupEnv(t *testing.T, extraEnv []string, args ...string) (int, string, string) {
	t.Helper()
	return setupEnvRoot(t, nil, extraEnv, args...)
}

// setupEnvRoot is setupEnv with root flags (such as -q) before the command.
func setupEnvRoot(t *testing.T, root, extraEnv []string, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Main(&Env{
		Args:    append(append(append([]string{}, root...), "tiers", "setup", "--data-dir", filepath.Join(dir, "d")), args...),
		Stdin:   strings.NewReader(""),
		Stdout:  &out,
		Stderr:  &errb,
		Environ: append([]string{"HOME=" + dir, "PATH=" + bin}, extraEnv...),
	})
	return code, out.String(), errb.String()
}

// The box had a `claude` on PATH, so auto-detection chose claude-code and
// the probes never reached the endpoint (0/3 in 2 s).
func TestTiersSetupEndpointIgnoresClaudeOnPath(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
	})
	code, out, errs := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m", "--repeats", "1")
	if code != 0 || hits.Load() != 3 || !strings.Contains(out, "PASSED") || !strings.Contains(out, "3/3") {
		t.Fatalf("code %d, hits %d\n%s\n%s", code, hits.Load(), out, errs)
	}
}

// Forcing claude-code with --endpoint is a conflict, named in one line.
func TestTiersSetupEndpointRefusesForcedClaudeCode(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {})
	code, out, errs := setupEnv(t, []string{"OPENDAISUGI_LLM_BACKEND=claude-code"},
		"--endpoint", srv.URL+"/v1", "--model", "m")
	if code == 0 || hits.Load() != 0 || !strings.Contains(errs, "--endpoint cannot be used with the claude-code backend") {
		t.Fatalf("code %d, hits %d\n%s\n%s", code, hits.Load(), out, errs)
	}
}

// A failed probe says why, in the text and in --json.
func TestQualifyReportsReason(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	})
	code, out, errs := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m")
	if code != 0 || !strings.Contains(out, "FAILED") || !strings.Contains(out, "boom") ||
		!strings.Contains(out, "ALL attempts errored") {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
	_, js, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m", "--json")
	var payload struct {
		Qualification struct {
			Outcomes []struct{ Task, Outcome, Reason string }
		}
	}
	if err := json.Unmarshal([]byte(js), &payload); err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	o := payload.Qualification.Outcomes
	if len(o) != 3 || o[0].Task == "" || o[0].Outcome != "error" || !strings.Contains(o[0].Reason, "boom") {
		t.Fatalf("outcomes %+v", o)
	}
}

// A long run is not silent: one line before the probes and one per probe go
// to stderr, and --json stdout stays one clean document.
func TestQualifyProgressGoesToStderr(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
	})
	ep := srv.URL + "/v1"
	_, out, errs := setupEnv(t, nil, "--endpoint", ep, "--model", "m", "--json")
	want := "qualifying m at " + ep + ": 3 probes, up to 300 s each\n"
	if !strings.Contains(errs, want) || !strings.Contains(errs, "probe 1/3: valid") || !strings.Contains(errs, "probe 3/3: valid") {
		t.Fatalf("stderr:\n%s", errs)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	// A failed probe says why on its own line.
	bad := chatServer(t, &hits, func(w http.ResponseWriter) { w.WriteHeader(500); w.Write([]byte("boom")) })
	_, _, errs = setupEnv(t, nil, "--endpoint", bad.URL+"/v1", "--model", "m")
	if !strings.Contains(errs, "probe 1/3: error: ") || !strings.Contains(errs, "boom") {
		t.Fatalf("stderr:\n%s", errs)
	}
	// -q silences the notes.
	_, _, errs = setupEnvRoot(t, []string{"-q"}, nil, "--endpoint", ep, "--model", "m")
	if strings.Contains(errs, "qualifying") || strings.Contains(errs, "probe 1/3") {
		t.Fatalf("-q still printed notes:\n%s", errs)
	}
}

// A reply that is not an envelope is declined, not errored; a thinking
// model cut off with no content says where its tokens went.
func TestQualifyDeclinedAndCutReasons(t *testing.T) {
	var hits atomic.Int32
	srv := chatServer(t, &hits, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": "nope"}, "stop")
	})
	_, out, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m", "--json")
	if !strings.Contains(out, `"outcome": "declined"`) || strings.Contains(out, `"outcome": "error"`) {
		t.Fatalf("%s", out)
	}
	srv2 := chatServer(t, &hits, func(w http.ResponseWriter) {
		chatReply(w, map[string]any{"role": "assistant", "content": "", "reasoning_content": "x"}, "length")
	})
	_, out, _ = setupEnv(t, nil, "--endpoint", srv2.URL+"/v1", "--model", "m")
	if !strings.Contains(out, "token budget") {
		t.Fatalf("%s", out)
	}
}

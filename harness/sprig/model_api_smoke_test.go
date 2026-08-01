package sprig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// This is Design E's live-wiring smoke test. It stands up a local server that
// speaks the Anthropic Messages API shape AND validates every incoming request
// against Anthropic's documented contract, then drives the WHOLE agent loop —
// real net/http over a real socket, the native tool_use/tool_result exchange, and
// the gate — to a disk side-effect. A green run means: E emits a contract-conformant
// request, completes the tool loop through the gate, and parses a real HTTP reply.
//
// What it deliberately does NOT prove (only a real key can): that api.anthropic.com
// itself accepts the exact schema, and E's real minimal-prompt token number. The
// mock validates against the documented contract; the server's own semantics are
// the residual the key settles.

// anthropicMock is a scripted, contract-validating stand-in for /v1/messages.
type anthropicMock struct {
	mu         sync.Mutex
	requests   int
	violations []string
	sawPairing bool // a tool_result whose id matched a prior tool_use — the pairing invariant
	writePath  string
	writeBody  string
}

func (a *anthropicMock) fail(format string, args ...any) {
	a.violations = append(a.violations, fmt.Sprintf(format, args...))
}

func (a *anthropicMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++

	if r.Method != http.MethodPost {
		a.fail("method %s, want POST", r.Method)
	}
	if r.URL.Path != "/v1/messages" {
		a.fail("path %q, want /v1/messages", r.URL.Path)
	}
	if r.Header.Get("x-api-key") == "" {
		a.fail("missing x-api-key header")
	}
	if r.Header.Get("anthropic-version") == "" {
		a.fail("missing anthropic-version header")
	}

	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Model     string           `json:"model"`
		MaxTokens int              `json:"max_tokens"`
		System    string           `json:"system"`
		Tools     []map[string]any `json:"tools"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		a.fail("body is not JSON: %v", err)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		a.fail("missing model")
	}
	if req.MaxTokens <= 0 {
		a.fail("max_tokens must be > 0, got %d", req.MaxTokens)
	}
	if len(req.Tools) == 0 {
		a.fail("no tools array — the whole point of E")
	}
	for _, tl := range req.Tools {
		if tl["name"] == nil || tl["name"] == "" {
			a.fail("a tool has no name: %v", tl)
		}
		sch, ok := tl["input_schema"].(map[string]any)
		if !ok || sch["type"] != "object" {
			a.fail("tool %v has no object input_schema", tl["name"])
		}
	}
	if len(req.Messages) == 0 {
		a.fail("no messages")
	}

	// Collect tool_use ids seen so far, and verify any tool_result references one —
	// the pairing invariant the API enforces (an orphan tool_result is a 400).
	useIDs := map[string]bool{}
	sawResult := false
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			a.fail("message role %q not user/assistant", m.Role)
		}
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if b.ID == "" {
					a.fail("tool_use block has no id")
				}
				useIDs[b.ID] = true
			case "tool_result":
				sawResult = true
				if !useIDs[b.ToolUseID] {
					a.fail("tool_result %q references no prior tool_use id", b.ToolUseID)
				} else {
					a.sawPairing = true
				}
			case "text":
			default:
				a.fail("unknown content block type %q", b.Type)
			}
		}
	}

	// Script the exchange off the history: no tool_result yet → ask for the write;
	// once the result comes back → finish. This exercises the full round-trip.
	w.Header().Set("content-type", "application/json")
	if !sawResult {
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_smoke","name":"write","input":{"path":%q,"content":%q}}]}`,
			req.Model, a.writePath, a.writeBody)
		return
	}
	fmt.Fprint(w, `{"id":"msg_2","type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"done: wrote the file"}]}`)
}

func TestAPIModelDrivesTheFullLoopOverARealSocket(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "smoke.txt")
	mock := &anthropicMock{writePath: outPath, writeBody: "hello from E"}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	// Real net/http to the test server (httpDo nil), pointed via BaseURL — exactly
	// what ANTHROPIC_BASE_URL does for the real binary.
	model := &APIModel{APIKey: "sk-test", Model: "claude-haiku-4-5", BaseURL: srv.URL, MaxTokens: 1024, Timeout: 10 * time.Second}
	agent := &Agent{
		Model:    model,
		Exec:     NewExecutor(DefaultTools(), AllowAll{}),
		MaxTurns: 5,
	}

	final, err := agent.Run("create the smoke file")
	if err != nil {
		t.Fatalf("full E loop failed over a real socket: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.violations) > 0 {
		t.Fatalf("E sent a request that violates the Anthropic contract:\n  - %v", mock.violations)
	}
	if mock.requests != 2 {
		t.Fatalf("want 2 round-trips (tool_use turn + final turn), got %d", mock.requests)
	}
	if !mock.sawPairing {
		t.Fatal("the second request never carried a tool_result paired to the tool_use id")
	}
	if final != "done: wrote the file" {
		t.Fatalf("final answer not returned from the real reply, got %q", final)
	}
	// The objective proof: the tool actually ran through the gate and hit disk.
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("the write tool never reached disk: %v", err)
	}
	if string(got) != "hello from E" {
		t.Fatalf("file content %q, want %q", got, "hello from E")
	}
}

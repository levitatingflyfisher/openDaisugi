package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientCreateSessionSendsBasicAuthAndReturnsID(t *testing.T) {
	var gotUser, gotPass string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		if r.URL.Path != "/session" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ses_abc123"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "secret-token")
	id, err := c.createSession(context.Background(), "coppice pane")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if id != "ses_abc123" {
		t.Fatalf("got id %q, want ses_abc123", id)
	}
	if gotUser != "opencode" || gotPass != "secret-token" {
		t.Fatalf("basic auth not sent: user=%q pass=%q", gotUser, gotPass)
	}
	if gotBody["title"] != "coppice pane" {
		t.Fatalf("title not sent: %v", gotBody)
	}
}

func TestClientCreateSessionRefusesAnErrorStatusOrAnEmptyID(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"empty id": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"id":""}`))
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`nope`))
		},
	} {
		srv := httptest.NewServer(h)
		c := newClient(srv.URL, "opencode", "x")
		if _, err := c.createSession(context.Background(), "t"); err == nil {
			t.Errorf("%s: want an error", name)
		}
		srv.Close()
	}
}

func TestClientSessionExistsIsTrueOnlyOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session/ses_live" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	if !c.sessionExists(context.Background(), "ses_live") {
		t.Fatal("want true for an existing session")
	}
	if c.sessionExists(context.Background(), "ses_gone") {
		t.Fatal("want false for a missing session")
	}
	if c.sessionExists(context.Background(), "../doc") {
		t.Fatal("an id with a slash must not reach another path")
	}
}

func TestClientPromptAsyncSendsOneTextPart(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	if err := c.promptAsync(context.Background(), "ses_1", "say hi"); err != nil {
		t.Fatalf("promptAsync: %v", err)
	}
	if gotPath != "/session/ses_1/prompt_async" {
		t.Fatalf("got path %q", gotPath)
	}
	parts, _ := gotBody["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("want one part, got %v", gotBody)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "say hi" {
		t.Fatalf("got part %v", part)
	}
}

func TestClientPromptAsyncRefusesAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := newClient(srv.URL, "opencode", "x")
	if err := c.promptAsync(context.Background(), "ses_1", "hi"); err == nil {
		t.Fatal("want an error on 404")
	}
}

func TestClientReplyPermissionAndRejectQuestion(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte("true"))
	}))
	defer srv.Close()
	c := newClient(srv.URL, "opencode", "x")
	if err := c.replyPermission(context.Background(), "per_1", "reject", "not now"); err != nil {
		t.Fatalf("replyPermission: %v", err)
	}
	if err := c.rejectQuestion(context.Background(), "que_1"); err != nil {
		t.Fatalf("rejectQuestion: %v", err)
	}
	if paths[0] != "POST /permission/per_1/reply" || paths[1] != "POST /question/que_1/reject" {
		t.Fatalf("paths = %v", paths)
	}
	if bodies[0]["reply"] != "reject" || bodies[0]["message"] != "not now" {
		t.Fatalf("reply body = %v", bodies[0])
	}
}

func TestClientWaitReadySucceedsOnHealth200(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"healthy":true,"version":"1.18.32"}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.waitReady(ctx); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if path != "/global/health" {
		t.Fatalf("readiness polled %q, want /global/health", path)
	}
}

func TestClientWaitReadyTimesOutWhenNeverReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "opencode", "x")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.waitReady(ctx); err == nil {
		t.Fatal("want a timeout error")
	}
}

// The event stream stays open for as long as the server lives, so the client
// that reads it must have no whole-request timeout.
func TestClientOpenEventsOutlivesTheRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n"))
	}))
	defer srv.Close()
	defer close(release)

	c := newClient(srv.URL, "opencode", "x")
	c.http.Timeout = 50 * time.Millisecond
	resp, err := c.openEvents(context.Background())
	if err != nil {
		t.Fatalf("openEvents: %v", err)
	}
	defer resp.Body.Close()
	time.Sleep(150 * time.Millisecond)
	release <- struct{}{}
	buf := make([]byte, 64)
	if n, err := resp.Body.Read(buf); n == 0 {
		t.Fatalf("stream was cut: %v", err)
	}
}

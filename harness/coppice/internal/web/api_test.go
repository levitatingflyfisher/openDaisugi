package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The failure this names: an /api path that answers without a token is an
// open door to every pane on the box. The list names every route this
// package registers, so a route added and left off the list is the failure
// this test exists to catch.
func TestEveryAPIPathRefusesAMissingToken(t *testing.T) {
	routes := []struct{ Method, Path string }{
		{"GET", "/api/token/check"},
		{"GET", "/api/panes"},
		{"GET", "/api/tasks"},
		{"GET", "/api/views"},
		{"POST", "/api/ask/answer"},
		{"POST", "/api/push/test"},
		{"POST", "/api/voice/transcribe"},
	}
	// Each route gets its own server. Three bad tokens from one address bans
	// it, and this test is about the guard on each route, not the ban.
	for _, r := range routes {
		_, d := newFakeServer(t, echoOK)
		ts, _, _ := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
		req, err := http.NewRequest(r.Method, ts.URL+r.Path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token returned %d, want 401", r.Method, r.Path, resp.StatusCode)
		}
	}
}

func TestAPIPanesProxiesPaneList(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		if req["cmd"] != "pane.list" {
			return []map[string]any{{"id": req["id"], "ok": false}}
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{
			"panes": []any{map[string]any{"id": "w1:p1", "label": "auth fix"}},
		}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
	req, _ := http.NewRequest("GET", ts.URL+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	panes, _ := body["panes"].([]any)
	if len(panes) != 1 {
		t.Fatalf("body was %v", body)
	}
}

// The failure this names: a 200 with an empty list would make the PWA print
// "No panes yet. Tap New to start one." on top of a real server error, and
// the operator would go looking for a bug that is not there.
func TestAPIPanesSurfacesARefusalInsteadOfAnEmptyRoster(t *testing.T) {
	_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
		return []map[string]any{{"id": req["id"], "ok": false, "error": map[string]any{
			"code": "internal", "message": "the layout store is unreadable",
		}}}
	})
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
	req, _ := http.NewRequest("GET", ts.URL+"/api/panes", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != "internal" {
		t.Errorf("the server's code was dropped: %v", body["code"])
	}
	msg, _ := body["error"].(string)
	if !strings.HasPrefix(msg, "server refused: ") || !strings.Contains(msg, "layout store") {
		t.Errorf("the client would show %q", msg)
	}
	if _, hasPanes := body["panes"]; hasPanes {
		t.Error("a refusal came back carrying a pane list")
	}
}

// The failure this names: coppice-server answering ok:true with a result
// that is not an object is not "no panes yet", it is a reply the phone
// cannot read. Painting an empty roster over it would send an operator
// hunting a bug that is not theirs, the same failure a refusal already
// guards against.
func TestAPIPanesTreatsANonObjectResultAsARefusal(t *testing.T) {
	cases := []struct {
		name   string
		result any
	}{
		{"a string result", "oops"},
		{"a missing result", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, d := newFakeServer(t, func(req map[string]any) []map[string]any {
				msg := map[string]any{"id": req["id"], "ok": true}
				if c.result != nil {
					msg["result"] = c.result
				}
				return []map[string]any{msg}
			})
			ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
			req, _ := http.NewRequest("GET", ts.URL+"/api/panes", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status %d, want 502", resp.StatusCode)
			}
			var body map[string]any
			json.NewDecoder(resp.Body).Decode(&body)
			if _, hasPanes := body["panes"]; hasPanes {
				t.Error("a malformed reply came back carrying a pane list")
			}
			msg, _ := body["error"].(string)
			if msg == "" {
				t.Error("the malformed reply carried no error sentence")
			}
		})
	}
}

// agentsWithAsk answers agent.list with one blocked pane holding ask id at
// tier, labelled "auth fix", and every other request with echoOK.
func agentsWithAsk(id, tier string) func(map[string]any) []map[string]any {
	return func(req map[string]any) []map[string]any {
		if req["cmd"] != "agent.list" {
			return echoOK(req)
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"agents": []any{
			map[string]any{"pane": "w1:p1", "label": "auth fix", "state": "blocked",
				"ask": map[string]any{"id": id, "tool": "Bash", "summary": "ls", "deadline": 9e9, "tier": tier}},
		}}}}
	}
}

func TestAPIAskAnswerAllowsAPendingAsk(t *testing.T) {
	_, d := newFakeServer(t, agentsWithAsk("toolu_01ABC", "undoable"))
	gate := t.TempDir()
	writeTieredAsk(t, gate, "toolu_01ABC", "undoable")
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/ask/answer",
		bytes.NewReader([]byte(`{"tool_use_id":"toolu_01ABC","decision":"allow","reason":"fine"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	raw, err := os.ReadFile(filepath.Join(gate, "answers", "toolu_01ABC.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answerBody map[string]any
	if err := json.Unmarshal(raw, &answerBody); err != nil {
		t.Fatal(err)
	}
	if answerBody["nonce"] != "deadbeef" {
		t.Fatalf("nonce is %v, want the ask's deadbeef", answerBody["nonce"])
	}
	if answerBody["decision"] != "allow" {
		t.Fatalf("decision is %v, want allow", answerBody["decision"])
	}
}

// The error has to teach: the operator is looking at a phone and needs to
// know why the button did nothing.
func TestAPIAskAnswerReturns409AndSaysWhyWhenTheAskIsGone(t *testing.T) {
	_, d := newFakeServer(t, agentsWithAsk("toolu_other", "undoable"))
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
	req, _ := http.NewRequest("POST", ts.URL+"/api/ask/answer",
		bytes.NewReader([]byte(`{"tool_use_id":"toolu_gone","decision":"allow"}`)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatal("409 carried no message")
	}
}

// The failure this names: the settings screen's Test button is the one
// diagnostic this feature gives an operator, and it must say why it did
// nothing when push is off, and must not turn a real ntfy failure into a
// green light.
func TestPushTestAnswersMatchWhetherPushIsConfiguredAndWhetherNtfyAccepts(t *testing.T) {
	cases := []struct {
		name       string
		configure  bool
		ntfyStatus int
		want       int
	}{
		{name: "push not configured", configure: false, want: http.StatusConflict},
		{name: "ntfy refuses", configure: true, ntfyStatus: 500, want: http.StatusBadGateway},
		{name: "ntfy accepts", configure: true, ntfyStatus: 200, want: http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, d := newFakeServer(t, echoOK)
			opts := Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}}
			if c.configure {
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(c.ntfyStatus)
				}))
				defer ts.Close()
				now := time.Unix(1_757_300_000, 0)
				push, err := NewPublisher(PushConfig{BaseURL: ts.URL, Topic: "coppice"},
					http.DefaultClient, func() time.Time { return now }, nil)
				if err != nil {
					t.Fatal(err)
				}
				opts.Push = push
			}
			ts, _, tok := newTestServer(t, opts)
			req, _ := http.NewRequest("POST", ts.URL+"/api/push/test", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.want)
			}
			if !c.configure {
				var body map[string]any
				json.NewDecoder(resp.Body).Decode(&body)
				if msg, _ := body["error"].(string); !strings.Contains(msg, "--ntfy") {
					t.Fatalf("the message does not teach the flag: %q", body["error"])
				}
			}
		})
	}
}

func postAnswer(t *testing.T, url, tok, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/api/ask/answer", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	msg, _ := out["error"].(string)
	return resp.StatusCode, msg
}

// The phone route holds the same permanent rule as the socket: the pane
// name, or no allow.
func TestAPIAskAnswerNeedsThePaneNameOnAPermanentAsk(t *testing.T) {
	_, d := newFakeServer(t, agentsWithAsk("toolu_1", "permanent"))
	gate := t.TempDir()
	writeTieredAsk(t, gate, "toolu_1", "permanent")
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}})
	code, msg := postAnswer(t, ts.URL, tok, `{"tool_use_id":"toolu_1","decision":"allow"}`)
	if code != http.StatusForbidden || msg != "this cannot be undone. Type the pane name to allow: auth fix" {
		t.Fatalf("status %d %q, want 403 and the name refusal", code, msg)
	}
	if _, err := os.Stat(filepath.Join(gate, "answers", "toolu_1.json")); err == nil {
		t.Fatal("a refused allow wrote an answer")
	}
	code, msg = postAnswer(t, ts.URL, tok, `{"tool_use_id":"toolu_1","decision":"allow","confirm":"auth fix"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d %q with the name, want 200", code, msg)
	}
}

// A deny asks the coppice server nothing, so it works even when the
// server cannot say which pane holds the ask.
func TestAPIAskAnswerDeniesWithoutALookup(t *testing.T) {
	f, d := newFakeServer(t, echoOK)
	gate := t.TempDir()
	writeTieredAsk(t, gate, "toolu_1", "permanent")
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}})
	if code, msg := postAnswer(t, ts.URL, tok, `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusOK {
		t.Fatalf("status %d %q, want 200", code, msg)
	}
	for _, line := range f.Received() {
		if strings.Contains(string(line), "agent.list") {
			t.Fatal("a deny looked the ask up")
		}
	}
}

// An allow whose tier the server cannot give is refused, never written.
func TestAPIAskAnswerRefusesAnAllowWhenTheServerCannotSay(t *testing.T) {
	_, d := newFakeServer(t, echoOK)
	gate := t.TempDir()
	writeTieredAsk(t, gate, "toolu_1", "undoable")
	ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}})
	if code, _ := postAnswer(t, ts.URL, tok, `{"tool_use_id":"toolu_1","decision":"allow"}`); code == http.StatusOK {
		t.Fatal("an allow with no tier from the server passed")
	}
	if _, err := os.Stat(filepath.Join(gate, "answers", "toolu_1.json")); err == nil {
		t.Fatal("an allow with no tier from the server wrote an answer")
	}
}

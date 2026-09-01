package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A harness holds some asks itself, such as OpenCode's permission prompts.
// The gate has no file for them, so the phone cannot answer them. It says
// where to answer, offers no Allow, and never calls the ask gone.

func heldEvent(pane string) StateEvent {
	ev := tieredEvent(pane, "permanent")
	ev.Ask.ID = "per_1"
	ev.Ask.Holder = "harness"
	return ev
}

func TestAHarnessHeldAskCardOffersOnlyLookAndSaysWhereToAnswer(t *testing.T) {
	cap, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	p := newPublisher(t, base, &now, "")
	p.OnState(heldEvent("w1:p1"), "oc pane")
	acts := actionsOf(cap.reqs[0].Actions)
	if len(acts) != 1 || acts[0][1] != "Look" {
		t.Fatalf("actions %q, want Look alone", cap.reqs[0].Actions)
	}
	if strings.Contains(cap.reqs[0].Actions, "Allow") || strings.Contains(cap.reqs[0].Actions, "Deny") {
		t.Fatalf("actions %q offer an answer", cap.reqs[0].Actions)
	}
	if !strings.Contains(cap.reqs[0].Body, "Open the floor to answer it") || strings.Contains(cap.reqs[0].Body, "coppice agent") {
		t.Fatalf("body %q does not say where to answer", cap.reqs[0].Body)
	}
}

func agentsWithHeldAsk(id string) func(map[string]any) []map[string]any {
	return func(req map[string]any) []map[string]any {
		if req["cmd"] != "agent.list" {
			return echoOK(req)
		}
		return []map[string]any{{"id": req["id"], "ok": true, "result": map[string]any{"agents": []any{
			map[string]any{"pane": "w1:p1", "label": "oc pane", "state": "blocked",
				"ask": map[string]any{"id": id, "tool": "bash", "summary": "ls", "deadline": 9e9,
					"tier": "permanent", "holder": "harness"}},
		}}}}
	}
}

func TestAnAnswerToAHarnessHeldAskSaysWhereToAnswerNotGone(t *testing.T) {
	for _, decision := range []string{"allow", "deny"} {
		_, d := newFakeServer(t, agentsWithHeldAsk("per_1"))
		ts, _, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: t.TempDir()}})
		req, _ := http.NewRequest("POST", ts.URL+"/api/ask/answer",
			bytes.NewReader([]byte(`{"tool_use_id":"per_1","decision":"`+decision+`","confirm":"oc pane"}`)))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		msg, _ := body["error"].(string)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s: status %d, want 409", decision, resp.StatusCode)
		}
		if strings.Contains(msg, "gone") || !strings.Contains(msg, "Open w1:p1 on the floor") ||
			strings.Contains(msg, "coppice agent") {
			t.Fatalf("%s: message %q", decision, msg)
		}
	}
}

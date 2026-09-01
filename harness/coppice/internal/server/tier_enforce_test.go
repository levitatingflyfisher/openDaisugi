package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const permanentRefusal = "this cannot be undone. Type the pane name to allow: auth fix"

// plantTieredAsk writes one pending ask the way the gate does, with the
// tier the gate put in the ask file. An empty tier writes none.
func plantTieredAsk(t *testing.T, root, id, tier string) {
	t.Helper()
	dir := filepath.Join(root, "asks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"toolUseId": id, "nonce": "n1", "postedAt": 1, "deadline": 9999999999}
	if tier != "" {
		body["tier"] = tier
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tieredBlockedLine reports a gate block on pane with ask id and tier. An
// empty tier sends none.
func tieredBlockedLine(pane, id, tier string) string {
	line := strings.Replace(gateBlockedLine(pane), "toolu_1", id, 1)
	if tier == "" {
		return line
	}
	return strings.Replace(line, `"deadline":9999999999.0}`, `"deadline":9999999999.0,"tier":"`+tier+`"}`, 1)
}

func answerFile(t *testing.T, root, id string) (map[string]any, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "answers", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, true
}

func tierRun(t *testing.T, fileTier, reportTier, verb string) (resp struct {
	ok      bool
	code    string
	message string
}, s *Server) {
	t.Helper()
	s = newAgentServer(t)
	plantTieredAsk(t, s.cfg.GateRoot, "ask-3", fileTier)
	got := roundTrip(t, s,
		strings.Replace(paneCreate, "%s", t.TempDir(), 1),
		tieredBlockedLine("w1:p1", "ask-3", reportTier),
		verb)
	r := got[2]
	resp.ok = r.OK
	if r.Error != nil {
		resp.code, resp.message = string(r.Error.Code), r.Error.Message
	}
	return resp, s
}

func TestAPermanentAskAllowedWithoutTheNameIsRefused(t *testing.T) {
	r, s := tierRun(t, "permanent", "permanent",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	if r.ok || r.code != "unauthorized" || r.message != permanentRefusal {
		t.Fatalf("got %+v, want unauthorized %q", r, permanentRefusal)
	}
	if _, ok := answerFile(t, s.cfg.GateRoot, "ask-3"); ok {
		t.Fatal("a refused allow wrote an answer")
	}
}

func TestAnAskWithNoTierIsPermanent(t *testing.T) {
	r, _ := tierRun(t, "", "",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	if r.ok || r.code != "unauthorized" || r.message != permanentRefusal {
		t.Fatalf("got %+v, want unauthorized %q", r, permanentRefusal)
	}
}

func TestAPermanentAskAllowedWithTheNamePasses(t *testing.T) {
	r, s := tierRun(t, "permanent", "permanent",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","confirm":"auth fix"}`)
	if !r.ok {
		t.Fatalf("got %+v, want ok", r)
	}
	ans, ok := answerFile(t, s.cfg.GateRoot, "ask-3")
	if !ok || ans["decision"] != "allow" {
		t.Fatalf("answer %v", ans)
	}
}

func TestAPermanentAskAllowedWithTheWrongNameIsRefused(t *testing.T) {
	for _, confirm := range []string{"auth", "Auth Fix", "w1:p1", ""} {
		r, _ := tierRun(t, "permanent", "permanent",
			`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","confirm":"`+confirm+`"}`)
		if r.ok || r.code != "unauthorized" {
			t.Fatalf("confirm %q: got %+v, want unauthorized", confirm, r)
		}
	}
}

func TestAnUndoableAskAllowedWithoutTheNamePasses(t *testing.T) {
	r, s := tierRun(t, "undoable", "undoable",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	if !r.ok {
		t.Fatalf("got %+v, want ok", r)
	}
	if ans, ok := answerFile(t, s.cfg.GateRoot, "ask-3"); !ok || ans["decision"] != "allow" {
		t.Fatalf("answer %v", ans)
	}
}

// A pane may report its own ask as undoable. The gate's own ask file says
// permanent, and permanent wins.
func TestTheAskFileTierWinsOverAnUndoableReport(t *testing.T) {
	r, _ := tierRun(t, "permanent", "undoable",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	if r.ok || r.code != "unauthorized" {
		t.Fatalf("got %+v, want unauthorized", r)
	}
	r, _ = tierRun(t, "", "undoable",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3"}`)
	if r.ok || r.code != "unauthorized" {
		t.Fatalf("ask file with no tier: got %+v, want unauthorized", r)
	}
}

func TestDenyNeverNeedsTheName(t *testing.T) {
	r, s := tierRun(t, "permanent", "permanent",
		`{"id":"2","cmd":"agent.deny","pane":"w1:p1","ask":"ask-3"}`)
	if !r.ok {
		t.Fatalf("got %+v, want ok", r)
	}
	if ans, ok := answerFile(t, s.cfg.GateRoot, "ask-3"); !ok || ans["decision"] != "deny" {
		t.Fatalf("answer %v", ans)
	}
}

// The ask must exist before the tier is read. An ask the pane does not
// hold is bad_request, not the permanent refusal.
func TestAnAskThePaneDoesNotHoldIsRefusedBeforeTheTier(t *testing.T) {
	r, _ := tierRun(t, "permanent", "permanent",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-9"}`)
	if r.ok || r.code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", r)
	}
}

func TestAllowForTheTaskRidesInTheAnswerOnAnUndoableAsk(t *testing.T) {
	r, s := tierRun(t, "undoable", "undoable",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","scope":"task"}`)
	if !r.ok {
		t.Fatalf("got %+v, want ok", r)
	}
	ans, _ := answerFile(t, s.cfg.GateRoot, "ask-3")
	if ans["scope"] != "task" {
		t.Fatalf("answer %v, want scope task", ans)
	}
}

func TestAPermanentAskCannotBeAllowedForTheTask(t *testing.T) {
	r, _ := tierRun(t, "permanent", "permanent",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","scope":"task","confirm":"auth fix"}`)
	if r.ok || r.code != "unauthorized" {
		t.Fatalf("got %+v, want unauthorized", r)
	}
}

func TestAnUnknownScopeIsABadRequest(t *testing.T) {
	r, _ := tierRun(t, "undoable", "undoable",
		`{"id":"2","cmd":"agent.allow","pane":"w1:p1","ask":"ask-3","scope":"forever"}`)
	if r.ok || r.code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", r)
	}
}

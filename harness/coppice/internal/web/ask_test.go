package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAsk builds an ask file the way src/opendaisugi/ask.py's post_ask does.
func writeAsk(t *testing.T, root, toolUseID, nonce string) {
	t.Helper()
	dir := filepath.Join(root, "asks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"toolUseId": toolUseID,
		"nonce":     nonce,
		"postedAt":  float64(time.Now().Unix()),
		"deadline":  float64(time.Now().Add(90 * time.Second).Unix()),
		"toolName":  "Bash",
		"detail":    "rm -rf build/",
	})
	if err := os.WriteFile(filepath.Join(dir, SafeID(toolUseID)+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSafeIDMatchesEveryPythonFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/safe_id_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct{ In, Out string } `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("the fixture file has no cases")
	}
	for _, c := range doc.Cases {
		if got := SafeID(c.In); got != c.Out {
			t.Errorf("SafeID(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestAnswerWritesTheFileThePythonGateWillHonor(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "toolu_01ABC", "deadbeef")
	a := AskChannel{Root: root}
	if err := a.Answer(Reply{ToolUseID: "toolu_01ABC", Decision: "allow", Reason: "allowed from the phone", Name: "auth fix", Confirm: "auth fix"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("answers holds %d entries, want exactly one with no temp file left behind", len(entries))
	}
	path := filepath.Join(root, "answers", "toolu_01ABC.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("answer mode is %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(root, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("answers directory mode is %o, want 700", di.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["nonce"] != "deadbeef" {
		t.Fatalf("nonce is %v, want the ask's deadbeef", body["nonce"])
	}
	if body["decision"] != "allow" || body["toolUseId"] != "toolu_01ABC" {
		t.Fatalf("answer body is %v", body)
	}
	if _, ok := body["updatedInput"]; !ok {
		t.Fatal("answer body has no updatedInput key")
	}
}

// The failure this names: with no ask on disk there is no nonce to echo, and
// ask.py retires both files the moment it sees any answer. Writing one the
// gate is going to reject would burn the ask cycle for nothing, so write
// nothing at all.
func TestAnswerWritesNothingWhenThereIsNoAsk(t *testing.T) {
	root := t.TempDir()
	a := AskChannel{Root: root}
	if err := a.Answer(Reply{ToolUseID: "toolu_gone", Decision: "allow", Reason: "too late"}); err != ErrNoAsk {
		t.Fatalf("Answer returned %v, want ErrNoAsk", err)
	}
	if _, err := os.Stat(filepath.Join(root, "answers", "toolu_gone.json")); !os.IsNotExist(err) {
		t.Fatal("Answer wrote a file with no ask to answer")
	}
}

// The failure this names: only allow and deny are decisions. Anything else
// must not reach the gate's directory.
func TestAnswerRefusesADecisionThatIsNeitherAllowNorDeny(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "toolu_01ABC", "deadbeef")
	a := AskChannel{Root: root}
	for _, bad := range []string{"", "yes", "ALLOW", "maybe"} {
		if err := a.Answer(Reply{ToolUseID: "toolu_01ABC", Decision: bad}); err != ErrBadDecision {
			t.Errorf("Answer(%q) returned %v, want ErrBadDecision", bad, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "answers", "toolu_01ABC.json")); !os.IsNotExist(err) {
		t.Fatal("a bad decision still wrote an answer file")
	}
}

func TestAnswerRefusesAnAskWithNoNonce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "asks")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "toolu_x.json"), []byte(`{"toolUseId":"toolu_x"}`), 0o600)
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_x", Decision: "deny"}); err != ErrNoAsk {
		t.Fatalf("Answer returned %v, want ErrNoAsk", err)
	}
}

// The failure this names: a server built with no gate root must not read
// "That ask is gone" over a config mistake. It gets its own distinct error
// so the operator sees the real fault.
func TestAnswerFailsWithNoGateRoot(t *testing.T) {
	a := AskChannel{}
	err := a.Answer(Reply{ToolUseID: "toolu_01ABC", Decision: "allow"})
	if err == nil {
		t.Fatal("Answer with no gate root returned nil")
	}
	if errors.Is(err, ErrNoAsk) || errors.Is(err, ErrBadDecision) {
		t.Fatalf("Answer with no gate root returned %v, want a distinct error", err)
	}
}

// The failure this names: a tool_use id is attacker-influenced text. It must
// never steer a write out of the answers directory.
func TestAnswerNeverWritesOutsideTheAnswersDirectory(t *testing.T) {
	root := t.TempDir()
	writeAsk(t, root, "../../etc/passwd", "deadbeef")
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "../../etc/passwd", Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "answers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != SafeID("../../etc/passwd")+".json" {
		t.Fatalf("answers holds %v", entries)
	}
}

// writeTieredAsk is writeAsk with the tier the gate writes in the ask file.
func writeTieredAsk(t *testing.T, root, toolUseID, tier string) {
	t.Helper()
	writeAsk(t, root, toolUseID, "deadbeef")
	path := filepath.Join(root, "asks", SafeID(toolUseID)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["tier"] = tier
	b, _ := json.Marshal(body)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerAllowsAnUndoableAskOnlyWhenBothSidesSayUndoable(t *testing.T) {
	cases := []struct {
		file, reply string
		ok          bool
	}{
		{"undoable", "undoable", true},
		{"undoable", "", false},
		{"permanent", "undoable", false},
		{"bogus", "undoable", false},
	}
	for _, c := range cases {
		root := t.TempDir()
		writeTieredAsk(t, root, "toolu_1", c.file)
		err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Tier: c.reply, Name: "auth fix"})
		var needsName *NeedsNameError
		if c.ok && err != nil {
			t.Errorf("file %q reply %q: %v, want nil", c.file, c.reply, err)
		}
		if !c.ok && !errors.As(err, &needsName) {
			t.Errorf("file %q reply %q: %v, want the name refusal", c.file, c.reply, err)
		}
	}
}

func TestAnswerNeedsTheExactNameOnAPermanentAsk(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "toolu_1", "permanent")
	a := AskChannel{Root: root}
	for _, confirm := range []string{"", "auth", "AUTH FIX"} {
		err := a.Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Tier: "permanent", Name: "auth fix", Confirm: confirm})
		var needsName *NeedsNameError
		if !errors.As(err, &needsName) || err.Error() != "this cannot be undone. Type the pane name to allow: auth fix" {
			t.Fatalf("confirm %q: %v", confirm, err)
		}
	}
	// An empty name never matches, not even an empty confirm.
	if err := a.Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Tier: "permanent"}); err == nil {
		t.Fatal("an empty name matched an empty confirm")
	}
	if err := a.Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Tier: "permanent", Name: "auth fix", Confirm: " auth fix "}); err != nil {
		t.Fatalf("the typed name did not pass: %v", err)
	}
}

func TestAnswerDenyNeedsNoName(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "toolu_1", "permanent")
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerRefusesAPermanentAskForTheTask(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "toolu_1", "permanent")
	err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Scope: "task",
		Tier: "permanent", Name: "auth fix", Confirm: "auth fix"})
	if !errors.Is(err, ErrPermanentTask) {
		t.Fatalf("%v, want ErrPermanentTask", err)
	}
}

func TestAnswerWritesTheScope(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "toolu_1", "undoable")
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Scope: "task", Tier: "undoable"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "answers", "toolu_1.json"))
	var body map[string]any
	json.Unmarshal(raw, &body)
	if body["scope"] != "task" {
		t.Fatalf("answer %v, want scope task", body)
	}
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "allow", Scope: "always"}); !errors.Is(err, ErrBadScope) {
		t.Fatalf("%v, want ErrBadScope", err)
	}
}

// Deny passes every check but the one that the ask exists. A stray scope
// on a deny is ignored, and the answer records scope once.
func TestAnswerDenyIgnoresScope(t *testing.T) {
	root := t.TempDir()
	writeTieredAsk(t, root, "toolu_1", "permanent")
	if err := (AskChannel{Root: root}).Answer(Reply{ToolUseID: "toolu_1", Decision: "deny", Scope: "forever"}); err != nil {
		t.Fatalf("a deny with a stray scope was refused: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "answers", "toolu_1.json"))
	var body map[string]any
	json.Unmarshal(raw, &body)
	if body["decision"] != "deny" || body["scope"] != "once" {
		t.Fatalf("answer %v", body)
	}
}

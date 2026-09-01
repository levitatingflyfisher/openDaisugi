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
	if err := a.Answer("toolu_01ABC", "allow", "allowed from the phone"); err != nil {
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
	if err := a.Answer("toolu_gone", "allow", "too late"); err != ErrNoAsk {
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
		if err := a.Answer("toolu_01ABC", bad, ""); err != ErrBadDecision {
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
	if err := (AskChannel{Root: root}).Answer("toolu_x", "deny", ""); err != ErrNoAsk {
		t.Fatalf("Answer returned %v, want ErrNoAsk", err)
	}
}

// The failure this names: a server built with no gate root must not read
// "That ask is gone" over a config mistake. It gets its own distinct error
// so the operator sees the real fault.
func TestAnswerFailsWithNoGateRoot(t *testing.T) {
	a := AskChannel{}
	err := a.Answer("toolu_01ABC", "allow", "")
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
	if err := (AskChannel{Root: root}).Answer("../../etc/passwd", "deny", ""); err != nil {
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

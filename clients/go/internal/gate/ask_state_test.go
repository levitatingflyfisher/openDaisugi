package gate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/statefile"
)

// GR-R-10: the ask channel is read and written through no link. A planted
// link at operator.json, asks/ or answers/ used to let a concurrent
// workspace write supply an allow. Each test plants one and needs the
// refusal. The Python twin is tests/test_ask_channel_state.py.

func askRunner(t *testing.T) (*runner, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "gate")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &runner{root: root}, root
}

func presence(t *testing.T, path string) {
	t.Helper()
	body := fmt.Sprintf(`{"pid": %d, "at": 0}`, os.Getpid())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func postOne(t *testing.T, r *runner, root string) string {
	t.Helper()
	if refused := r.postAsk("t1", pyjson.NewObject().Set("tier", "permanent"), float64(time.Now().Unix()+60)); refused {
		t.Fatal("the ask was refused")
	}
	raw, err := os.ReadFile(filepath.Join(root, "asks", "t1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var b struct{ Nonce string }
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b.Nonce
}

func forged(nonce string) string {
	return fmt.Sprintf(`{"toolUseId": "t1", "decision": "allow", "nonce": %q}`, nonce)
}

func TestAPlainPresenceFileCounts(t *testing.T) {
	r, root := askRunner(t)
	presence(t, filepath.Join(root, "operator.json"))
	if !r.operatorPresent() {
		t.Fatal("a plain operator.json did not count")
	}
}

func TestAnOperatorFileThatIsLinkedOrSharedIsNoOperator(t *testing.T) {
	r, root := askRunner(t)
	op := filepath.Join(root, "operator.json")
	real := filepath.Join(t.TempDir(), "elsewhere.json")
	presence(t, real)
	if err := os.Symlink(real, op); err != nil {
		t.Fatal(err)
	}
	if r.operatorPresent() {
		t.Fatal("a symlinked operator.json counted")
	}
	os.Remove(op)
	presence(t, op)
	if err := os.Chmod(op, 0o666); err != nil {
		t.Fatal(err)
	}
	if r.operatorPresent() {
		t.Fatal("a world-writable operator.json counted")
	}
	os.Chmod(op, 0o600)
	if err := os.Link(op, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	if r.operatorPresent() {
		t.Fatal("a hard-linked operator.json counted")
	}
}

func TestASymlinkedAsksDirTakesNoAsk(t *testing.T) {
	r, root := askRunner(t)
	real := t.TempDir()
	if err := os.Symlink(real, filepath.Join(root, "asks")); err != nil {
		t.Fatal(err)
	}
	if r.channelOK() {
		t.Fatal("a symlinked asks/ was ok")
	}
	if refused := r.postAsk("t1", pyjson.NewObject(), 0); !refused {
		t.Fatal("an ask was posted through the link")
	}
	if ents, _ := os.ReadDir(real); len(ents) != 0 {
		t.Fatalf("written through the link: %v", ents)
	}
}

func TestASymlinkedAnswersDirIsNotHonoredAndTheWaitEndsAtOnce(t *testing.T) {
	r, root := askRunner(t)
	nonce := postOne(t, r, root)
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "t1.json"), []byte(forged(nonce)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "answers")); err != nil {
		t.Fatal(err)
	}
	if r.channelOK() {
		t.Fatal("a symlinked answers/ was ok")
	}
	t0 := time.Now()
	if got := r.waitAnswer("t1", 30); got != nil {
		t.Fatalf("a forged answer was honored: %v", got)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("the wait did not end at once")
	}
	if _, err := os.Stat(filepath.Join(real, "t1.json")); err != nil {
		t.Fatal("a file was removed through the link")
	}
}

func TestAnAnswerThatIsLinkedSharedOrHardLinkedIsNotHonored(t *testing.T) {
	r, root := askRunner(t)
	plants := map[string]func(p, body string){
		"symlink": func(p, body string) {
			real := filepath.Join(t.TempDir(), "real.json")
			os.WriteFile(real, []byte(body), 0o600)
			os.Symlink(real, p)
		},
		"world writable": func(p, body string) {
			os.WriteFile(p, []byte(body), 0o600)
			os.Chmod(p, 0o666)
		},
		"hard link": func(p, body string) {
			os.WriteFile(p, []byte(body), 0o600)
			os.Link(p, filepath.Join(t.TempDir(), "second"))
		},
	}
	for name, plant := range plants {
		os.RemoveAll(filepath.Join(root, "asks"))
		os.RemoveAll(filepath.Join(root, "answers"))
		nonce := postOne(t, r, root)
		if err := os.Mkdir(filepath.Join(root, "answers"), 0o700); err != nil {
			t.Fatal(err)
		}
		plant(filepath.Join(root, "answers", "t1.json"), forged(nonce))
		if got := r.waitAnswer("t1", 2); got != nil {
			t.Fatalf("%s: a forged answer was honored", name)
		}
	}
}

func TestARealAnswerIsStillHonored(t *testing.T) {
	r, root := askRunner(t)
	nonce := postOne(t, r, root)
	if _, err := statefile.WriteFile(root, []string{"answers"}, "t1.json", forged(nonce), true); err != nil {
		t.Fatal(err)
	}
	got := r.waitAnswer("t1", 2)
	if got == nil || got.Value("decision") != "allow" {
		t.Fatalf("a real answer was not honored: %v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "asks", "t1.json")); err == nil {
		t.Fatal("the ask was left")
	}
}

func TestTheChannelIsPrivateWhateverTheUmask(t *testing.T) {
	r, root := askRunner(t)
	os.RemoveAll(root)
	postOne(t, r, root)
	for p, want := range map[string]os.FileMode{root: 0o700, root + "/asks": 0o700, root + "/asks/t1.json": 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
}

func TestTheSweepRemovesAPlantedAskLinkAndReadsNoTarget(t *testing.T) {
	r, root := askRunner(t)
	real := filepath.Join(t.TempDir(), "real-ask.json")
	os.WriteFile(real, []byte(fmt.Sprintf(`{"deadline": %d}`, time.Now().Unix()+600)), 0o600)
	os.Mkdir(filepath.Join(root, "asks"), 0o700)
	os.Symlink(real, filepath.Join(root, "asks", "x.json"))
	r.sweepExpired(float64(time.Now().Unix()))
	if _, err := os.Lstat(filepath.Join(root, "asks", "x.json")); err == nil {
		t.Fatal("the link was left")
	}
	if _, err := os.Stat(real); err != nil {
		t.Fatal("the target was removed")
	}
}

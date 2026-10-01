package tree

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func growRoot(t *testing.T, tokens int64) (string, string) {
	t.Helper()
	d := t.TempDir()
	g := filepath.Join(d, "gate")
	root := envOf(t, `{"id": "env_root", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["git"]}}`)
	var tk *big.Int
	if tokens >= 0 {
		tk = big.NewInt(tokens)
	}
	if _, err := MakeRoot(d, g, root, "top", tk, nil); err != nil {
		t.Fatal(err)
	}
	return d, g
}

func spawnKid(t *testing.T, d, g, parent, session string, tokens int64, o SpawnOpts) *Spawn {
	t.Helper()
	kid := envOf(t, `{"id": "env_kid", "generated_by": "t", "task": "t", "permissions": {"shell": true, "shell_allowlist": ["git"]}}`)
	var sp *string
	if session != "" {
		sp = &session
	}
	res, err := DoSpawnOpts(d, g, kid, parent, sp, big.NewInt(tokens), nil, DefaultTimeoutMs, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBranchTotalsRollUpThreeDeep(t *testing.T) {
	d, g := growRoot(t, 1000)
	spawnKid(t, d, g, "top", "a", 600, SpawnOpts{})
	spawnKid(t, d, g, "a", "b", 300, SpawnOpts{})
	spawnKid(t, d, g, "b", "c", 100, SpawnOpts{})
	for _, e := range []struct {
		s string
		n int64
	}{{"c", 40}, {"b", 10}} {
		if _, err := End(d, g, e.s, big.NewInt(e.n), big.NewInt(1)); err != nil {
			t.Fatal(err)
		}
	}
	node, err := End(d, g, "a", big.NewInt(5), big.NewInt(1))
	if err != nil || node.TokensUsed.Int64() != 55 || node.TurnsUsed.Int64() != 3 {
		t.Fatalf("node %+v %v", node, err)
	}
	tr := ReadTree(d)
	if l := tr.Left(tr.Nodes["top"], "tokens"); l.Int64() != 945 {
		t.Fatalf("left %v", l)
	}
}

func TestAnOverrunIsChargedAndShown(t *testing.T) {
	d, g := growRoot(t, 100)
	spawnKid(t, d, g, "top", "a", 60, SpawnOpts{})
	if _, err := End(d, g, "a", big.NewInt(150), nil); err != nil {
		t.Fatal(err)
	}
	text := StatusText(StatusDoc(d))
	for _, want := range []string{"top (running) tokens 100 (over by 50)", "a (ended) tokens 60 (used 150, over by 90)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
}

func TestNoReportKeepsTheReservationAndParentUseLowersLeft(t *testing.T) {
	d, g := growRoot(t, 100)
	spawnKid(t, d, g, "top", "a", 60, SpawnOpts{})
	spawnKid(t, d, g, "a", "b", 30, SpawnOpts{})
	if got := EndSubtree(d, g, "a", true); strings.Join(got, ",") != "b,a" {
		t.Fatalf("ended %v", got)
	}
	tr := ReadTree(d)
	if tr.Nodes["a"].TokensUsed != nil || tr.Left(tr.Nodes["top"], "tokens").Int64() != 40 {
		t.Fatalf("a %+v left %v", tr.Nodes["a"], tr.Left(tr.Nodes["top"], "tokens"))
	}
	res := spawnKid(t, d, g, "top", "x", 30, SpawnOpts{ParentTokensUsed: big.NewInt(20)})
	if res.Status != "refused" || res.Reasons[0] != "tokens: the child asks for 30; the parent has 20 left" {
		t.Fatalf("got %+v", res)
	}
}

func TestMintedSessionsAndSpawnExtras(t *testing.T) {
	d, g := growRoot(t, -1)
	p, m := "desk", "llama3.3:70b"
	res := spawnKid(t, d, g, "top", "", 1, SpawnOpts{Profile: &p, Model: &m})
	// The oracle mints n-798c5d0e353b for the first child of top.
	if res.Session != "n-798c5d0e353b" || res.Session != MintSession("top", 0) || !strings.HasPrefix(res.Session, "n-") || len(res.Session) != 14 {
		t.Fatalf("session %q", res.Session)
	}
	if _, err := os.Stat(filepath.Join(g, "envelopes", res.Session+".json")); err != nil {
		t.Fatal("not registered")
	}
	text := StatusText(StatusDoc(d))
	if !strings.Contains(text, ", profile desk, model llama3.3:70b") {
		t.Fatalf("status %s", text)
	}
	if res2 := spawnKid(t, d, g, "top", "", 1, SpawnOpts{}); res2.Session != MintSession("top", 1) {
		t.Fatalf("second %q", res2.Session)
	}
}

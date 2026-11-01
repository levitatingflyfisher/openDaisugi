package supervise

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/tree"
)

func helpEnvelope(t *testing.T, id string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(`{"id": "` + id + `", "generated_by": "t", "task": "t", "permissions": {"file_read": ["/**"], "max_execution_time_s": 30}}`)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// runHelpNode runs one node on a fake sprig that prints body, and returns the
// node, the argv sprig got and its stdin.
func runHelpNode(t *testing.T, body string, tools []string, resume string) (*NodeRun, string, string) {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "d")
	wr := filepath.Join(data, "warden")
	if _, err := tree.MakeRoot(data, wr, helpEnvelope(t, "env_root"), "top", big.NewInt(1000), big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	bin := fakeSprig(t, root, body)
	s := &Starter{Self: "/opt/daisugi", TempDir: "/t", Sprig: bin, Environ: []string{"PATH=/usr/bin:/bin"}}
	n, err := s.RunNode(data, wr, NodeOpts{Parent: "top", Envelope: helpEnvelope(t, "env_kid"), Profiles: agentprof.Profiles{},
		Tools: tools, Task: "the answer", Cwd: root, TimeoutS: 10, DefaultModel: "haiku", Tokens: big.NewInt(100),
		Turns: big.NewInt(4), Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	stdin, _ := os.ReadFile(filepath.Join(root, "stdin"))
	return n, string(args), string(stdin)
}

const helpJSON = `echo '{"answer":"which db?","status":"help","help_kind":"need_info","help_text":"which db?","model_turns":1,"usage":{"input_tokens":5,"output_tokens":3}}'`

func TestHelpIsAlwaysOnTheWallAndEndsTheNodeWithItsKind(t *testing.T) {
	n, args, _ := runHelpNode(t, helpJSON, []string{"help"}, "")
	if !strings.Contains(args, "--tools\nhelp\n") {
		t.Fatalf("help needs no envelope grant, argv: %q", args)
	}
	if strings.Contains(args, "--resume") {
		t.Fatalf("a first segment does not resume: %q", args)
	}
	doc := pyjson.Dumps(n.Doc(), false)
	for _, want := range []string{`"status": "help"`, `"help_kind": "need_info"`, `"help_text": "which db?"`, `"answer": "which db?"`, `"tokens_used": 8`} {
		if !strings.Contains(doc, want) {
			t.Fatalf("the doc lacks %s: %s", want, doc)
		}
	}
}

func TestAResumedNodeGetsTheOldSessionAndTheAnswer(t *testing.T) {
	n, args, stdin := runHelpNode(t, `echo '{"answer":"done","model_turns":1}'`, []string{"help"}, "old-1")
	if !strings.Contains(args, "--resume\nold-1\n--session\n") {
		t.Fatalf("sprig must resume the old session under the new node: %q", args)
	}
	if strings.TrimSpace(stdin) != "the answer" {
		t.Fatalf("the answer is the new task: %q", stdin)
	}
	if strings.Contains(pyjson.Dumps(n.Doc(), false), "help_kind") {
		t.Fatal("a plain answer carries no help fields")
	}
}

func TestWithoutHelpDropsItForABranch(t *testing.T) {
	got := WithoutHelp([]string{"read", "HELP", "bash", "Help"})
	if strings.Join(got, ",") != "read,bash" {
		t.Fatalf("%v", got)
	}
}

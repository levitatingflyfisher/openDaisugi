package batch

import (
	"fmt"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/supervise"
)

func decl(t *testing.T, text string) Decl {
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	d, verr := Validate(v)
	if verr != nil {
		t.Fatal(verr)
	}
	return d
}

const three = `{"program": {"id": "plan_b0000001", "source": "script", "task": "batch",
	"steps": [{"id": "w", "type": "file_write", "path": "/out/x.txt", "content": "hi"}]},
	"parameters": [{"name": "p", "step_index": 0, "step_id": "w", "field": "path", "head": "/out"}],
	"items": [{"p": "/out/a.txt"}, {"p": "/out/b.txt"}, {"q": "x"}],
	"footprint": ["/out/*"]}`

// A proof names the items that did not bind and the writes that escape.
func TestAProofNamesWhatDidNotBindAndWhatEscapes(t *testing.T) {
	d := decl(t, three)
	p, err := Prove(d, []string{"/out/**"})
	if err != nil {
		t.Fatal(err)
	}
	if p.OK || strings.Join(p.Writes, ",") != "/out/a.txt,/out/b.txt" || len(p.BadBindings) != 1 || p.BadBindings[0] != 2 {
		t.Fatalf("%+v", p)
	}
	if p.Reason != "1 item(s) failed to bind (unbound hole or head change): [2]" {
		t.Fatal(p.Reason)
	}
	p, err = Prove(d, []string{"/elsewhere/**"})
	if err != nil || !strings.Contains(p.Reason, "2 write(s) outside the envelope: ['/out/a.txt', '/out/b.txt']") {
		t.Fatalf("%v %q", err, p.Reason)
	}
}

// A shell or mcp step is not batchable, and the batch is refused before
// any item runs.
func TestAShellStepIsNotBatchable(t *testing.T) {
	d := decl(t, `{"program": {"id": "plan_b0000001", "source": "script", "task": "batch",
		"steps": [{"id": "s", "type": "shell", "command": "make"}, {"id": "m", "type": "mcp", "server": "x", "tool": "t"}]},
		"items": [{}]}`)
	c := Classify(d)
	if c.Batchable || c.NonBatchableKinds() != "mcp, shell" {
		t.Fatalf("%+v %q", c, c.NonBatchableKinds())
	}
	never := func(*pyjson.Object) (*supervise.Session, error) {
		t.Fatal("an item ran")
		return nil, nil
	}
	r, err := Run(d, nil, never, nil)
	if err != nil || r.Status != "rejected" {
		t.Fatalf("%+v %v", r, err)
	}
}

// The ledger counts the declaration's JSON when no spec size is given.
func TestTheLedgerCountsTheDeclaration(t *testing.T) {
	d := decl(t, three)
	l := WithinInstance(d, 10, 1, nil, nil)
	want := int64(len([]rune(d.DumpJSON())) / 4)
	if l.SpecInjected != want || l.PerCall != DefaultTokensPerCall {
		t.Fatalf("%+v", l)
	}
	if got := l.Dump().Value("net").(pyjson.Int).Text; got != fmt.Sprint(10+2000-want) {
		t.Fatal(got)
	}
}

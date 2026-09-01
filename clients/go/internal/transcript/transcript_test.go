package transcript

import (
	"reflect"
	"testing"

	"daisugi-verify/internal/verify"
)

func TestQuoteIsURLLibQuote(t *testing.T) {
	for in, want := range map[string]string{
		"python async é & more": "python%20async%20%C3%A9%20%26%20more",
		"a/b_c.d-e~f":           "a/b_c.d-e~f",
		"?=#":                   "%3F%3D%23",
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCompoundShell(t *testing.T) {
	got := verify.SplitCompoundShell("cd /work && make test; echo 'a;b' || true")
	want := []string{"cd /work", "make test", "echo 'a;b'", "true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := verify.SplitCompoundShell("ls | wc -l"); len(got) != 1 {
		t.Fatalf("a pipe is not split: %q", got)
	}
}

func TestLinesAreUniversalNewlines(t *testing.T) {
	got := Lines("a\r\nb\rc\nd")
	want := []string{"a\r\n", "b\r", "c\n", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergeKeepsPrevIndex(t *testing.T) {
	msgs, err := ClaudeMessages(`{"role":"user","content":"a"}
{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/x"}}]}
{"role":"user","content":"b"}
{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"a && b"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	eps, err := Identify(msgs)
	if err != nil || len(eps) != 2 {
		t.Fatalf("%v %d", err, len(eps))
	}
	merged := MergeSmall(eps, 3)
	if len(merged) != 1 || merged[0].Steps() != 3 || merged[0].LastMessage != 3 {
		t.Fatalf("merged %+v", merged[0])
	}
}

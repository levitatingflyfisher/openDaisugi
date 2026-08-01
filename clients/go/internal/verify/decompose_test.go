package verify

import (
	"reflect"
	"testing"
)

// TestDecomposeCommand is a committed regression table for the decompose
// port. The real gate is the (never-committed) corpus — see
// docs/spec/conformance.md — but the corpus isn't always available (a
// fresh checkout, CI without .opendaisugi/), so this locks in every shape
// that was hand-derived from the live oracle while porting, including
// every case named in clients/ADJUDICATIONS.md.
func TestDecomposeCommand(t *testing.T) {
	type want struct {
		ok     bool
		heads  []string
		reads  []string
		writes []string
	}
	cases := []struct {
		name    string
		command string
		want    want
	}{
		{"simple", "git status", want{true, []string{"git"}, nil, nil}},
		{"empty", "", want{false, nil, nil, nil}},
		{"whitespace only", "   ", want{false, nil, nil, nil}},
		{"comment only", "# comment", want{false, nil, nil, nil}},
		{"bare assignment", "FOO=1", want{false, nil, nil, nil}},
		{"assignment then command", "FOO=1 git status", want{true, []string{"git"}, nil, nil}},
		{"assignment order (heads before nested subst)", "X=$(date) prog --flag",
			want{true, []string{"prog", "date"}, nil, nil}},
		{"pipeline", "a | b | c", want{true, []string{"a", "b", "c"}, nil, nil}},
		{"and chain", "a && b && c", want{true, []string{"a", "b", "c"}, nil, nil}},
		{"substitution surfaces inner head", "git $(echo status)",
			want{true, []string{"git", "echo"}, nil, nil}},
		{"write redirect", "a > out.txt", want{true, []string{"a"}, nil, []string{"out.txt"}}},
		{"read redirect", "a < in.txt", want{true, []string{"a"}, []string{"in.txt"}, nil}},
		{"append redirect", "a >> out.txt", want{true, []string{"a"}, nil, []string{"out.txt"}}},
		{"fd dup passes pathless", "a 2>&1", want{true, []string{"a"}, nil, nil}},
		{"fd close passes pathless", "a >&-", want{true, []string{"a"}, nil, nil}},
		{"non-literal redirect target rejects", "a > $OUT", want{false, nil, nil, nil}},
		{"non-literal head rejects", "$CMD arg", want{false, nil, nil, nil}},
		{"quoted head rejects", `"git" status`, want{false, nil, nil, nil}},
		{"sanctioned sink still reported", "a > /dev/null", want{true, []string{"a"}, nil, []string{"/dev/null"}}},

		// --- G-1: "<>" is a whole-parse-error, not a redirect classification.
		{"G-1 RdrInOut rejects", "exec 3<>f", want{false, nil, nil, nil}},

		// --- G-2: lenient (digit-leading) assignment-word recognition.
		{"G-2 digit-leading assignment", "1FOO=1 git", want{true, []string{"git"}, nil, nil}},
		{"G-2 digit-only assignment", "9=1 git", want{true, []string{"git"}, nil, nil}},
		{"G-2 hyphen breaks it (not an assignment)", "FOO-BAR=1 git",
			want{true, []string{"FOO-BAR=1"}, nil, nil}},
		{"G-2 no name at all is not an assignment", "=x git",
			want{true, []string{"=x"}, nil, nil}},

		// --- G-3: "[" is the bracket test, never a head.
		{"G-3 bracket test alone has no head", "[ -f x ]", want{false, nil, nil, nil}},
		{"G-3 bracket test still walks args for substitutions",
			`[ -f "$(mktemp)" ]`, want{true, []string{"mktemp"}, nil, nil}},
		{"G-3 test word form is an ordinary head", "test -f x",
			want{true, []string{"test"}, nil, nil}},

		// --- G-5: "time" has no grammar production at all.
		{"G-5 time is a literal head, unsplit argv", "time a b c",
			want{true, []string{"time"}, nil, nil}},
		{"G-5 time only wraps the first pipe stage", "time a | b",
			want{true, []string{"time", "b"}, nil, nil}},
		{"G-5 time before a subshell: empty argv, subshell walked separately",
			"time (echo hi)", want{true, []string{"time", "echo"}, nil, nil}},

		// --- G-6: heredoc + another redirect + pipe; heredoc chains via &&/||.
		{"G-6.1 heredoc alone piped is fine", "cat <<'EOF' | tail -1\nhello\nEOF",
			want{true, []string{"cat", "tail"}, nil, nil}},
		{"G-6.1 heredoc + extra redirect, no pipe, is fine", "cat <<'EOF' 2>&1\nhello\nEOF",
			want{true, []string{"cat"}, nil, nil}},
		{"G-6.1 heredoc + extra redirect + pipe rejects",
			"cat <<'EOF' 2>&1 | tail -1\nhello\nEOF", want{false, nil, nil, nil}},
		{"G-6.1 extra redirect BEFORE heredoc is fine even piped",
			"cat 2>&1 <<'EOF' | tail -1\nhello\nEOF", want{true, []string{"cat", "tail"}, nil, nil}},
		{"G-6.2 one heredoc then && is fine", "a <<'A' && echo b\nx\nA",
			want{true, []string{"a", "echo"}, nil, nil}},
		{"G-6.2 two heredocs chained by && rejects",
			"a <<'A' && b <<'B'\nx\nA\ny\nB", want{false, nil, nil, nil}},
		{"G-6.2 two heredocs on their own lines (no &&/||) is fine",
			"git commit -q -F - <<'EOF'\nbody1\nEOF\ngit commit -q -F - <<'EOF2'\nbody2\nEOF2",
			want{true, []string{"git", "git"}, nil, nil}},

		// --- G-4 / G-4b: tree-sitter-bash's statement-fusion bug (the
		// oracle's parser gluing two top-level statements' source spans
		// into one "command" node across a bare newline). The oracle used
		// to fail closed on this; this client carried detectG4bFusion, a
		// leaky PREDICTION of the same trigger, purely to match that
		// fail-closed behavior. The oracle has since been fixed to repair
		// the fused parse and decompose correctly instead, and mvdan never
		// fuses these shapes in the first place (no bare-newline artifact
		// exists in its parse tree) — so detectG4bFusion was retired and
		// these shapes now decompose normally, surfacing every head from
		// every statement. See ADJUDICATIONS.md G-4/G-4b for the history.
		{"G-4b shape: 3-stage pipe then a redirect-bearing final pipe decomposes normally",
			"a | b | c\nd 2>/dev/null | e",
			want{true, []string{"a", "b", "c", "d", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: intervening single-command statement decomposes normally",
			"a | b | c\necho next\nd 2>/dev/null | e",
			want{true, []string{"a", "b", "c", "echo", "d", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: intervening multi-stage pipe decomposes normally",
			"a | b | c\nx | y\nd 2>/dev/null | e",
			want{true, []string{"a", "b", "c", "x", "y", "d", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: 2-stage earlier pipe decomposes normally",
			"a | b\necho next\nc 2>/dev/null | d",
			want{true, []string{"a", "b", "echo", "c", "d"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: final pipe with no redirect decomposes normally",
			"a | b | c\nx | y\nd | e",
			want{true, []string{"a", "b", "c", "x", "y", "d", "e"}, nil, nil}},
		{"G-4b shape: redirect statement not the file's last decomposes normally",
			"a | b | c\nd 2>/dev/null | e\nf",
			want{true, []string{"a", "b", "c", "d", "e", "f"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: ';' between trigger and target decomposes normally",
			"a | b | c; d 2>/dev/null | e",
			want{true, []string{"a", "b", "c", "d", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: bare non-pipe redirect statement decomposes normally",
			"a 2>&1 | b | c\nd 2>/dev/null\ne",
			want{true, []string{"a", "b", "c", "d", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: bare redirect statement with more following decomposes normally",
			"a 2>&1 | b | c\nd 2>/dev/null\ne\nf\ng",
			want{true, []string{"a", "b", "c", "d", "e", "f", "g"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: later pipe-shaped redirect statement (not last) decomposes normally",
			"a 2>&1 | b | c\nd 2>/dev/null | x\ne",
			want{true, []string{"a", "b", "c", "d", "x", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: redirect hiding inside an && branch decomposes normally",
			"a 2>&1 | b | c\nd 2>/dev/null && echo x\ne",
			want{true, []string{"a", "b", "c", "d", "echo", "e"}, nil, []string{"/dev/null"}}},
		{"G-4b shape: ';' between statements decomposes normally",
			"a 2>&1 | b | c\nz; d 2>/dev/null\ne",
			want{true, []string{"a", "b", "c", "z", "d", "e"}, nil, []string{"/dev/null"}}},
	}
	sameStringSet := func(a, b []string) bool {
		return reflect.DeepEqual(SortedCopy(a), SortedCopy(b))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecomposeCommand(c.command)
			if d.OK != c.want.ok {
				t.Fatalf("ok = %v, want %v (reason=%q)", d.OK, c.want.ok, d.Reason)
			}
			if !d.OK {
				return
			}
			if !reflect.DeepEqual(d.Heads, c.want.heads) {
				t.Errorf("heads = %v, want %v", d.Heads, c.want.heads)
			}
			if !sameStringSet(d.Reads, c.want.reads) {
				t.Errorf("reads = %v, want %v", d.Reads, c.want.reads)
			}
			if !sameStringSet(d.Writes, c.want.writes) {
				t.Errorf("writes = %v, want %v", d.Writes, c.want.writes)
			}
		})
	}
}

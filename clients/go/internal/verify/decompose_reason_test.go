package verify

import "testing"

// The gate copies a refused decomposition's reason into a violation's
// detail, so the reason text must be the oracle's, Python repr included.
func TestDecomposeReasonUsesPythonRepr(t *testing.T) {
	cases := map[string]string{
		`git status && $CMD`:         `non-literal command head ('$CMD')`,
		`"$AAPT" dump badging x.apk`: `non-literal command head ('"$AAPT"')`,
		`echo hi > $(mktemp)`:        `non-literal redirect target ('$(mktemp)')`,
		`tr a b < "$F" | wc -l`:      `non-literal redirect target ('"$F"')`,
	}
	for cmd, want := range cases {
		if got := DecomposeCommand(cmd).Reason; got != want {
			t.Errorf("DecomposeCommand(%q).Reason = %q, want %q", cmd, got, want)
		}
	}
}

// tree-sitter-bash files the words after a redirect as more targets of
// it. Since 0423900 the oracle refuses such a redirect, so a head or an
// argument after it is never left unchecked. The shapes below were
// probed against that oracle.
func TestRedirectFollowedByWordsIsRefusedAsTheOracleRefusesIt(t *testing.T) {
	refused := map[string]string{
		`ls | FOO=1 </work/i rm x`: `</work/i rm x`,
		`rm </dev/null -rf /work`:  `</dev/null -rf /work`,
		`cat <a b`:                 `<a b`,
		`echo hi >out more`:        `>out more`,
		`ls 2>&1 x`:                `2>&1 x`,
		`ls >&2 x`:                 `>&2 x`,
		`<x cat >y z`:              `>y z`,
		`ls <x >y z`:               `>y z`,
		`ls <a 2>&1 b`:             `2>&1 b`,
		`ls x 2>&1 | grep >a b`:    `>a b`,
		`ls | FOO=1 </x rm`:        `</x rm`,
		`ls | FOO=1 rm </x y`:      `</x y`,
	}
	for cmd, text := range refused {
		want := "ambiguous shell (a redirect with more than one target '" + text + "')"
		if d := DecomposeCommand(cmd); d.OK || d.Reason != want {
			t.Errorf("DecomposeCommand(%q) = ok %v reason %q, want %q", cmd, d.OK, d.Reason, want)
		}
	}
	for _, cmd := range []string{
		`<x cat y`, `FOO=1 </x rm y z`, `ls && FOO=1 </x rm y`, `FOO=1 </x rm y | ls`, `ls | </work/i rm x`,
		`ls | 2>/dev/null rm x`, `ls >a 2>&1`, `ls x >a`, `ls >a <b`, `cat <<< c d`,
	} {
		if d := DecomposeCommand(cmd); !d.OK {
			t.Errorf("DecomposeCommand(%q) refused (%s); the oracle accepts it", cmd, d.Reason)
		}
	}
}

func TestHerestringAfterAFileRedirectIsAParseError(t *testing.T) {
	for _, cmd := range []string{`echo a >b <<< c`, `cat <a <<< c`, `cat 2>/dev/null <<< c`, `cat 2>&1 <<< c`} {
		if d := DecomposeCommand(cmd); d.OK || d.Reason != "malformed shell (parse error)" {
			t.Errorf("DecomposeCommand(%q) = ok %v reason %q", cmd, d.OK, d.Reason)
		}
	}
	for _, cmd := range []string{`cat <<< c >b`, `cat <<< c 2>&1`, "cat >b <<EOF\nx\nEOF"} {
		if d := DecomposeCommand(cmd); !d.OK {
			t.Errorf("DecomposeCommand(%q) refused (%s); the oracle accepts it", cmd, d.Reason)
		}
	}
}

// tree-sitter-bash has no extglob production, so the oracle refuses every
// extglob as a parse error. mvdan parses @(...) as a leaf whose pattern
// text is never walked, which hid the head inside it.
func TestExtglobIsRefusedAsTheOracleRefusesIt(t *testing.T) {
	for _, cmd := range []string{
		`echo @(a|$(rm x)) ; ls`, `echo !(a|$(rm x)) ; ls`, `echo +($(rm x)) ; ls`,
		"echo *(`rm x`) ; ls", `echo ?(<(rm x)) ; ls`, `ls @($(rm x)) | ls`, `echo @(a|b) ; ls`,
	} {
		d := DecomposeCommand(cmd)
		if d.OK || d.Reason != "malformed shell (parse error)" {
			t.Errorf("DecomposeCommand(%q) = ok %v reason %q, want a parse-error refusal", cmd, d.OK, d.Reason)
		}
	}
}

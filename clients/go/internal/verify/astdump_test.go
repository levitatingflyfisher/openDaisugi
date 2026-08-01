package verify

import (
	"fmt"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// TestASTDump is throwaway exploration tooling — NOT a correctness test. It
// prints the mvdan/sh AST shape for tricky command forms so the decompose
// port can be derived from ground truth instead of guesses. Run with
// `go test -run TestASTDump -v`.
func TestASTDump(t *testing.T) {
	cases := []string{
		`a > f`,
		`a > "f"`,
		`a > 'f'`,
		`a > out\ file.txt`,
		`2>&1`,
		`a >&-`,
		`a <&-`,
		`a >&2`,
		`exec 3<>f`,
		`cat <<EOF
heredoc
EOF`,
		`cat <<< "hello"`,
		`cat <<< "$(x)"`,
		`a <(cmd)`,
		`a > <(cmd)`,
		`1FOO=1 git`,
		`=x git`,
		`FOO=x=y git`,
		`git "status"`,
		`echo "a"b`,
		`git$(echo status)`,
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			p := syntax.NewParser(syntax.Variant(syntax.LangBash))
			f, err := p.Parse(strings.NewReader(src), "")
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("SRC=%q ERR=%v\n", src, err))
			if f != nil {
				syntax.Walk(f, func(n syntax.Node) bool {
					if n == nil {
						return true
					}
					sb.WriteString(fmt.Sprintf("  %T pos=%v end=%v -> %#v\n", n, n.Pos(), n.End(), n))
					return true
				})
			}
			t.Log(sb.String())
		})
	}
}

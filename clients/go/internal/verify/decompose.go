package verify

import (
	"fmt"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Decomposition mirrors shell_decompose.Decomposition. See
// clients/PORTING-NOTES.md "Decomposition" section and
// clients/ADJUDICATIONS.md for every place mvdan.cc/sh/v3's grammar
// disagrees with tree-sitter-bash (the oracle's parser) and how this port
// resolves it to match the oracle anyway.
type Decomposition struct {
	OK       bool
	Heads    []string
	Commands []string
	Reads    []string
	Writes   []string
	Reason   string
}

// DecomposeCommand ports shell_decompose.decompose_command /
// shell_decompose._decompose.
func DecomposeCommand(command string) Decomposition {
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := p.Parse(strings.NewReader(command), "")
	if err != nil {
		return Decomposition{Reason: "malformed shell (parse error)"}
	}
	// NOTE: this client used to carry detectG4bFusion, a leaky PREDICTION
	// of tree-sitter-bash's ADJUDICATIONS.md G-4 statement-fusion bug, so
	// as to reject the same shapes the (formerly fail-closed) oracle
	// rejected. The oracle has since been fixed to repair the fused parse
	// and decompose correctly instead of failing closed, and mvdan never
	// fuses in the first place — so the prediction hack has been retired.
	// Let the normal walk stand; see ADJUDICATIONS.md G-4/G-4b for history.
	d := &decomposer{src: command}
	d.walkStmts(f.Stmts)
	if d.reason != "" {
		return Decomposition{Reason: d.reason}
	}
	if len(d.heads) == 0 {
		return Decomposition{Reason: "no command heads found"}
	}
	return Decomposition{
		OK:       true,
		Heads:    d.heads,
		Commands: d.commands,
		Reads:    d.reads,
		Writes:   d.writes,
	}
}

type decomposer struct {
	src      string
	heads    []string
	commands []string
	reads    []string
	writes   []string
	reason   string
}

func (d *decomposer) fail(reason string) {
	if d.reason == "" {
		d.reason = reason
	}
}

func (d *decomposer) failed() bool { return d.reason != "" }

// --- statement / command tree -------------------------------------------------

func (d *decomposer) walkStmts(stmts []*syntax.Stmt) {
	for _, s := range stmts {
		if d.failed() {
			return
		}
		d.walkStmt(s)
	}
}

func (d *decomposer) walkStmt(s *syntax.Stmt) {
	if s == nil || d.failed() {
		return
	}
	if s.Cmd != nil {
		d.walkCommand(s.Cmd)
		if d.failed() {
			return
		}
	}
	for _, r := range s.Redirs {
		d.walkRedirect(r)
		if d.failed() {
			return
		}
	}
}

func (d *decomposer) walkCommand(cmd syntax.Command) {
	if cmd == nil || d.failed() {
		return
	}
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		d.walkCallExpr(c)
	case *syntax.BinaryCmd:
		// ADJUDICATIONS.md G-6: tree-sitter-bash cannot parse a pipeline
		// stage that carries a heredoc/herestring redirect FOLLOWED by any
		// further redirect on the same statement ("cmd <<'EOF' 2>&1 |
		// tail" — whole-file parse error). Confirmed minimal repro: the
		// heredoc alone piped is fine, the extra redirect alone (no pipe)
		// is fine, the extra redirect BEFORE the heredoc is fine — only
		// heredoc-then-another-redirect-then-pipe fails. mvdan (and real
		// bash) parse it correctly; match the oracle's parse error anyway.
		if (c.Op == syntax.Pipe || c.Op == syntax.PipeAll) && c.X != nil &&
			hasHeredocFollowedByAnotherRedirect(c.X.Redirs) {
			d.fail("malformed shell (parse error)")
			return
		}
		// ADJUDICATIONS.md G-6 (continued): two or more heredoc-bearing
		// statements chained by &&/|| — anywhere in the chain, not
		// necessarily adjacent — ALSO produce a whole-parse error in
		// tree-sitter-bash, even though each one individually (one
		// heredoc + && + a plain command) parses fine. mvdan (and real
		// bash) handle this correctly; match the oracle's reject anyway.
		if c.Op == syntax.AndStmt || c.Op == syntax.OrStmt {
			if countAndOrHeredocStmts(c.X)+countAndOrHeredocStmts(c.Y) >= 2 {
				d.fail("malformed shell (parse error)")
				return
			}
		}
		d.walkStmt(c.X)
		if d.failed() {
			return
		}
		d.walkStmt(c.Y)
	case *syntax.Block:
		d.walkStmts(c.Stmts)
	case *syntax.Subshell:
		d.walkStmts(c.Stmts)
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			d.walkStmts(ic.Cond)
			if d.failed() {
				return
			}
			d.walkStmts(ic.Then)
			if d.failed() {
				return
			}
		}
	case *syntax.WhileClause:
		d.walkStmts(c.Cond)
		if d.failed() {
			return
		}
		d.walkStmts(c.Do)
	case *syntax.ForClause:
		d.walkLoop(c.Loop)
		if d.failed() {
			return
		}
		d.walkStmts(c.Do)
	case *syntax.CaseClause:
		d.walkWord(c.Word)
		if d.failed() {
			return
		}
		for _, item := range c.Items {
			for _, pat := range item.Patterns {
				d.walkWord(pat)
				if d.failed() {
					return
				}
			}
			d.walkStmts(item.Stmts)
			if d.failed() {
				return
			}
		}
	case *syntax.FuncDecl:
		d.walkStmt(c.Body)
	case *syntax.ArithmCmd:
		d.walkArithmExpr(c.X)
	case *syntax.TestClause:
		d.walkTestExpr(c.X)
	case *syntax.DeclClause:
		for _, a := range c.Args {
			d.walkAssign(a)
			if d.failed() {
				return
			}
		}
	case *syntax.LetClause:
		for _, e := range c.Exprs {
			d.walkArithmExpr(e)
			if d.failed() {
				return
			}
		}
	case *syntax.TimeClause:
		d.walkTimeWrapped(c.Time, c.Stmt)
	case *syntax.CoprocClause:
		d.walkStmt(c.Stmt)
	default:
		// An unrecognized command construct might hide an executable head
		// this port doesn't know how to see — fail closed rather than
		// silently skip it.
		d.fail(fmt.Sprintf("unsupported shell construct (%T)", cmd))
	}
}

// walkTimeWrapped ports the oracle's ADJUDICATIONS.md G-5 behavior:
// tree-sitter-bash has NO grammar production for bash's "time" reserved
// word — it parses as an ordinary command whose literal head is "time" and
// whose argv is everything up to the next real pipeline/list operator or
// structural token it can't fold into a bare word. Empirically (see the
// adjudication): "time a" / "time a b c" -> one command, head "time",
// unsplit text; "time a | b" -> pipeline of TWO commands ["time a", "b"];
// "time a && b" -> list of two ["time a", "b"]; "time (sub)" -> "time"
// alone (empty argv — "(" isn't a word char) THEN the subshell is
// recognized as ordinary structure; "time { a; b; }" swallows "{" as a
// literal word (not reproduced here — see the adjudication, not worth the
// engineering for a construct absent from the corpus).
func (d *decomposer) walkTimeWrapped(timePos syntax.Pos, s *syntax.Stmt) {
	if s == nil || d.failed() {
		return
	}
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		d.heads = append(d.heads, "time")
		d.commands = append(d.commands, sliceSrc(d.src, timePos, cmd.End()))
		for _, a := range cmd.Assigns {
			d.walkAssign(a)
			if d.failed() {
				return
			}
		}
		for _, w := range cmd.Args {
			d.walkWord(w)
			if d.failed() {
				return
			}
		}
	case *syntax.BinaryCmd:
		// "time" only wraps as far as the first operand under tree-sitter's
		// bare-word theory; the second operand is an ordinary sibling.
		d.walkTimeWrapped(timePos, cmd.X)
		if d.failed() {
			return
		}
		d.walkStmt(cmd.Y)
	case *syntax.Subshell:
		// "(" isn't a valid bare-word character, so the "time" command
		// node ends with an empty argv right there; the subshell that
		// follows is ordinary structure.
		d.heads = append(d.heads, "time")
		d.commands = append(d.commands, "time")
		d.walkStmts(cmd.Stmts)
	default:
		d.heads = append(d.heads, "time")
		d.commands = append(d.commands, sliceSrc(d.src, timePos, s.End()))
	}
	for _, r := range s.Redirs {
		d.walkRedirect(r)
		if d.failed() {
			return
		}
	}
}

// countAndOrHeredocStmts counts heredoc/herestring-bearing statements
// reachable through a chain of &&/|| operators (see ADJUDICATIONS.md G-6) —
// scoped to the and/or chain itself, not into unrelated nested structures.
func countAndOrHeredocStmts(s *syntax.Stmt) int {
	if s == nil {
		return 0
	}
	n := 0
	if hasHeredocRedirect(s.Redirs) {
		n++
	}
	if bc, ok := s.Cmd.(*syntax.BinaryCmd); ok && (bc.Op == syntax.AndStmt || bc.Op == syntax.OrStmt) {
		n += countAndOrHeredocStmts(bc.X) + countAndOrHeredocStmts(bc.Y)
	}
	return n
}

func hasHeredocRedirect(redirs []*syntax.Redirect) bool {
	for _, r := range redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			return true
		}
	}
	return false
}

// hasHeredocFollowedByAnotherRedirect reports whether redirs contains a
// heredoc/herestring redirect that is followed (later in source order) by
// any other redirect on the same statement — see ADJUDICATIONS.md G-6.
func hasHeredocFollowedByAnotherRedirect(redirs []*syntax.Redirect) bool {
	sawHeredoc := false
	for _, r := range redirs {
		if sawHeredoc {
			return true
		}
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
			sawHeredoc = true
		}
	}
	return false
}

func (d *decomposer) walkLoop(l syntax.Loop) {
	if l == nil || d.failed() {
		return
	}
	switch lp := l.(type) {
	case *syntax.WordIter:
		for _, w := range lp.Items {
			d.walkWord(w)
			if d.failed() {
				return
			}
		}
	case *syntax.CStyleLoop:
		d.walkArithmExpr(lp.Init)
		if d.failed() {
			return
		}
		d.walkArithmExpr(lp.Cond)
		if d.failed() {
			return
		}
		d.walkArithmExpr(lp.Post)
	}
}

// --- CallExpr (the "command" equivalent) --------------------------------------

func (d *decomposer) walkCallExpr(c *syntax.CallExpr) {
	// ADJUDICATIONS.md G-2: tree-sitter-bash's grammar recognizes a leading
	// assignment word ("1FOO=1 git") even when the name starts with a
	// digit; mvdan requires a POSIX-valid name for its own Assigns split
	// and leaves such a word in Args. Re-peel it here so the head we pick
	// matches the oracle's.
	args := c.Args
	i := 0
	for i < len(args) {
		lit, ok := singleLit(args[i])
		if !ok || !looksLikeLenientAssignWord(lit) {
			break
		}
		i++
	}

	if i < len(args) {
		headWord := args[i]
		lit, ok := singleLit(headWord)
		if !ok {
			d.fail(fmt.Sprintf("non-literal command head (%q)", wordSource(d.src, headWord)))
			return
		}
		// The oracle appends the head to `heads`/`commands` THE MOMENT it
		// visits the "command" node — i.e. before walking any of that
		// node's children, including LEADING assignments. `X=$(date)
		// prog` therefore yields heads ["prog", "date"], not ["date",
		// "prog"] (confirmed via corpus mismatch f06b6ffac9daa60f). Match
		// that ordering: append first, walk Assigns/Args after.
		//
		// ADJUDICATIONS.md G-3: "[ ... ]" (the bracket test) is real
		// bash's ordinary "[" utility — mvdan parses it as any other
		// CallExpr with a literal head "[". tree-sitter-bash's grammar
		// gives it a DEDICATED test_command node distinct from "command",
		// so the oracle never surfaces "[" itself as a head (though it
		// still walks the bracket's arguments for nested substitutions —
		// confirmed empirically: `[ -f "$(mktemp)" ]` surfaces "mktemp").
		// Match the oracle: skip the head, keep walking.
		if lit.Value != "[" {
			d.heads = append(d.heads, lit.Value)
			d.commands = append(d.commands, sliceSrc(d.src, c.Pos(), c.End()))
		}
	}

	for _, a := range c.Assigns {
		d.walkAssign(a)
		if d.failed() {
			return
		}
	}
	for _, w := range args {
		d.walkWord(w)
		if d.failed() {
			return
		}
	}
}

func (d *decomposer) walkAssign(a *syntax.Assign) {
	if a == nil || d.failed() {
		return
	}
	if a.Value != nil {
		d.walkWord(a.Value)
		if d.failed() {
			return
		}
	}
	if a.Array != nil {
		for _, elem := range a.Array.Elems {
			if elem.Value != nil {
				d.walkWord(elem.Value)
				if d.failed() {
					return
				}
			}
			if elem.Index != nil {
				d.walkArithmExpr(elem.Index)
				if d.failed() {
					return
				}
			}
		}
	}
	if a.Index != nil {
		d.walkArithmExpr(a.Index)
	}
}

// --- words / expansions --------------------------------------------------------

func (d *decomposer) walkWord(w *syntax.Word) {
	if w == nil || d.failed() {
		return
	}
	for _, part := range w.Parts {
		d.walkWordPart(part)
		if d.failed() {
			return
		}
	}
}

func (d *decomposer) walkWordPart(p syntax.WordPart) {
	if p == nil || d.failed() {
		return
	}
	switch wp := p.(type) {
	case *syntax.Lit, *syntax.SglQuoted, *syntax.ExtGlob:
		// Leaves: no possible nested substitution.
	case *syntax.DblQuoted:
		for _, inner := range wp.Parts {
			d.walkWordPart(inner)
			if d.failed() {
				return
			}
		}
	case *syntax.CmdSubst:
		d.walkStmts(wp.Stmts)
	case *syntax.ProcSubst:
		d.walkStmts(wp.Stmts)
	case *syntax.ParamExp:
		d.walkParamExp(wp)
	case *syntax.ArithmExp:
		d.walkArithmExpr(wp.X)
	default:
		d.fail(fmt.Sprintf("unsupported word construct (%T)", p))
	}
}

func (d *decomposer) walkParamExp(p *syntax.ParamExp) {
	if p == nil || d.failed() {
		return
	}
	if p.Index != nil {
		d.walkArithmExpr(p.Index)
		if d.failed() {
			return
		}
	}
	if p.Slice != nil {
		d.walkArithmExpr(p.Slice.Offset)
		if d.failed() {
			return
		}
		d.walkArithmExpr(p.Slice.Length)
		if d.failed() {
			return
		}
	}
	if p.Repl != nil {
		d.walkWord(p.Repl.Orig)
		if d.failed() {
			return
		}
		d.walkWord(p.Repl.With)
		if d.failed() {
			return
		}
	}
	if p.Exp != nil {
		d.walkWord(p.Exp.Word)
		if d.failed() {
			return
		}
	}
	if p.NestedParam != nil {
		d.walkWordPart(p.NestedParam)
	}
}

func (d *decomposer) walkArithmExpr(e syntax.ArithmExpr) {
	if e == nil || d.failed() {
		return
	}
	switch v := e.(type) {
	case *syntax.Word:
		d.walkWord(v)
	case *syntax.BinaryArithm:
		d.walkArithmExpr(v.X)
		if d.failed() {
			return
		}
		d.walkArithmExpr(v.Y)
	case *syntax.UnaryArithm:
		d.walkArithmExpr(v.X)
	case *syntax.ParenArithm:
		d.walkArithmExpr(v.X)
	}
}

func (d *decomposer) walkTestExpr(e syntax.TestExpr) {
	if e == nil || d.failed() {
		return
	}
	switch v := e.(type) {
	case *syntax.Word:
		d.walkWord(v)
	case *syntax.BinaryTest:
		d.walkTestExpr(v.X)
		if d.failed() {
			return
		}
		d.walkTestExpr(v.Y)
	case *syntax.UnaryTest:
		d.walkTestExpr(v.X)
	case *syntax.ParenTest:
		d.walkTestExpr(v.X)
	}
}

// --- redirects -----------------------------------------------------------------

func (d *decomposer) walkRedirect(r *syntax.Redirect) {
	if r == nil || d.failed() {
		return
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		// Stdin data, not a file access (PORTING-NOTES.md) -- walk the body
		// for nested substitutions, never classify it as a read/write path.
		d.walkWord(r.Hdoc)
		return
	case syntax.WordHdoc:
		// Herestring: content lives in .Word, not .Hdoc (verified via
		// astdump_test.go).
		d.walkWord(r.Word)
		return
	case syntax.RdrInOut:
		// ADJUDICATIONS.md G-1: mvdan parses "<>" (RdrInOut); tree-sitter-
		// bash's grammar cannot, producing a whole-parse error. Match the
		// oracle: reject.
		d.fail("malformed shell (parse error)")
		return
	}

	lit, isLit := singleLit(r.Word)
	isNumeric := isLit && isAllDigits(lit.Value)

	switch r.Op {
	case syntax.DplOut, syntax.DplIn:
		if r.Word == nil || isNumeric || (isLit && lit.Value == "-") {
			return // fd dup ("2>&1") or fd close ("<&-") -- pathless.
		}
	default:
		if isNumeric {
			// A purely-numeric destination on a non-dup/close operator
			// (e.g. "a > 2") is not a filename to the oracle's grammar —
			// it is an unrecognized shape. Reject rather than silently
			// writing to a file literally named "2".
			d.fail(fmt.Sprintf("unrecognized shell redirection (%q)", redirectSource(d.src, r)))
			return
		}
	}

	path, literal := literalTargetText(r.Word)
	if !literal {
		d.fail(fmt.Sprintf("non-literal redirect target (%q)", wordSource(d.src, r.Word)))
		return
	}
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.DplOut:
		d.writes = append(d.writes, path)
	case syntax.RdrIn, syntax.DplIn:
		d.reads = append(d.reads, path)
	default:
		d.fail(fmt.Sprintf("unrecognized shell redirection operator (%v)", r.Op))
	}
}

// --- literal-text helpers -------------------------------------------------------

// singleLit is the strict "literal head" test: EXACTLY one Lit word part —
// mirrors shell_decompose.py's `[c.type for c in name.children] != ["word"]`
// check. A single-quoted or double-quoted head ('git', "git") is NOT literal
// by this rule, even though it would be for a redirect target.
func singleLit(w *syntax.Word) (*syntax.Lit, bool) {
	if w == nil || len(w.Parts) != 1 {
		return nil, false
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	return lit, ok
}

// literalTargetText is the looser "literal redirect target" test: a plain
// Lit, a single-quoted string (content only), or a double-quoted string
// whose parts are ALL plain Lits (no embedded expansion/substitution) —
// mirrors shell_decompose.py's _literal_text.
func literalTargetText(w *syntax.Word) (string, bool) {
	if w == nil || len(w.Parts) != 1 {
		return "", false
	}
	switch p := w.Parts[0].(type) {
	case *syntax.Lit:
		return p.Value, true
	case *syntax.SglQuoted:
		return p.Value, true
	case *syntax.DblQuoted:
		var sb strings.Builder
		for _, inner := range p.Parts {
			lit, ok := inner.(*syntax.Lit)
			if !ok {
				return "", false
			}
			sb.WriteString(lit.Value)
		}
		return sb.String(), true
	default:
		return "", false
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// looksLikeLenientAssignWord mirrors tree-sitter-bash's (more permissive
// than POSIX/mvdan) variable_assignment name grammar, empirically derived
// via clients/go/probe_gen.py-style probing of the real oracle: any run of
// [A-Za-z0-9_] (digits allowed leading — unlike POSIX), optionally followed
// by "+", then "=". See ADJUDICATIONS.md G-2.
func looksLikeLenientAssignWord(lit *syntax.Lit) bool {
	s := lit.Value
	i := 0
	for i < len(s) && isWordByte(s[i]) {
		i++
	}
	if i == 0 {
		return false
	}
	if i < len(s) && s[i] == '+' {
		i++
	}
	return i < len(s) && s[i] == '='
}

func isWordByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}

func sliceSrc(src string, pos, end syntax.Pos) string {
	a, b := int(pos.Offset()), int(end.Offset())
	if a < 0 || b > len(src) || a > b {
		return ""
	}
	return src[a:b]
}

func wordSource(src string, w *syntax.Word) string {
	if w == nil {
		return ""
	}
	return sliceSrc(src, w.Pos(), w.End())
}

func redirectSource(src string, r *syntax.Redirect) string {
	return sliceSrc(src, r.Pos(), r.End())
}

// SortedCopy returns a sorted copy of ss (verify/decompose wire verdicts
// compare reads/writes as sorted sets).
func SortedCopy(ss []string) []string {
	out := make([]string, len(ss))
	copy(out, ss)
	sort.Strings(out)
	return out
}

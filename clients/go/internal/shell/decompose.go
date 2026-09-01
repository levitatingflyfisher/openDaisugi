// Package shell is shell_decompose.py on tree-sitter-bash, the oracle's own
// grammar: the same parse, the same fusion repair and the same refusals.
//
// Python's recursion limit is part of the oracle's behavior. Its walks
// recurse once per tree level, so a deep enough command (about a thousand
// && links) raises RecursionError, which the gate turns into a deny. This
// port counts Python frames the same way and raises at the same depth:
// every function below takes the frame depth its Python twin runs at.
package shell

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tsbash"
)

// Decomposition is shell_decompose.Decomposition.
type Decomposition struct {
	OK       bool
	Heads    []string
	Commands []string
	Reads    []string
	Writes   []string
	Reason   string
}

// RecursionLimit is sys.getrecursionlimit() in the oracle.
const RecursionLimit = 1000

// maxFusionSplitDepth is _MAX_FUSION_SPLIT_DEPTH.
const maxFusionSplitDepth = 64

var (
	writeRedirectOps = map[string]bool{">": true, ">>": true, "&>": true, "&>>": true, ">|": true, ">&": true}
	readRedirectOps  = map[string]bool{"<": true, "<&": true}
	fdCloseOps       = map[string]bool{">&-": true, "<&-": true}
	multilineLegal   = map[string]bool{
		"string": true, "raw_string": true, "ansi_c_string": true, "translated_string": true,
		"command_substitution": true, "process_substitution": true, "arithmetic_expansion": true,
		"heredoc_body": true, "heredoc_redirect": true,
	}
)

// walker counts the deepest Python frame a decomposition reaches,
// relative to decompose_command's own frame (0).
type walker struct{ max int }

// errTooDeep ends a walk no caller could survive: every caller sits at
// frame 1 or deeper, so a relative depth of RecursionLimit raises.
var errTooDeep = pystr.RecursionError()

func (w *walker) enter(rel int) {
	if rel > w.max {
		w.max = rel
	}
	if rel >= RecursionLimit {
		panic(errTooDeep)
	}
}

// answer is one decomposition: the result or the exception Python raises
// whatever the depth, and the deepest relative frame the walk reached.
type answer struct {
	d   Decomposition
	exc *pystr.Exception
	max int
}

func compute(command string) (a answer) {
	w := &walker{}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*pystr.Exception)
			if !ok {
				panic(r)
			}
			a = answer{exc: e, max: w.max}
		}
	}()
	d := w.decompose(command, 0, 1)
	return answer{d: d, max: w.max}
}

// resolve turns an answer into what decompose_command raises or returns
// when its frame is at depth. RecursionError is raised exactly when the
// walk went deeper than the limit, since the walk does not depend on the
// depth it starts at.
func (a answer) resolve(depth int) (Decomposition, *pystr.Exception) {
	if depth > RecursionLimit || a.exc == errTooDeep || depth+a.max > RecursionLimit {
		return Decomposition{}, pystr.RecursionError()
	}
	if a.exc != nil {
		return Decomposition{}, a.exc
	}
	return a.d, nil
}

// Decompose is decompose_command(command), called at Python frame depth
// depth (the depth of decompose_command's own frame). A Python exception
// the oracle would raise comes back as err.
func Decompose(command string, depth int) (Decomposition, *pystr.Exception) {
	return compute(command).resolve(depth)
}

// Cache keeps one call's decompositions: the gate decomposes the same
// line in several rules, and parsing it once is enough.
type Cache struct {
	mu sync.Mutex
	m  map[string]answer
}

// Decompose is the package Decompose, from the cache when it can be.
func (c *Cache) Decompose(command string, depth int) (Decomposition, *pystr.Exception) {
	c.mu.Lock()
	a, ok := c.m[command]
	c.mu.Unlock()
	if !ok {
		a = compute(command)
		c.mu.Lock()
		if c.m == nil {
			c.m = map[string]answer{}
		}
		c.m[command] = a
		c.mu.Unlock()
	}
	return a.resolve(depth)
}

// DecomposeCommand is Decompose at a shallow depth, for callers that model
// no recursion (the conformance client). A Python exception becomes a
// refusal naming it.
func DecomposeCommand(command string) Decomposition {
	d, err := Decompose(command, 1)
	if err != nil {
		return Decomposition{Reason: err.String()}
	}
	return d
}

type tree struct {
	*tsbash.Tree
}

func (t tree) typ(i int) string { return t.Nodes[i].Type }

func (t tree) text(i int) string { return pystr.DecodeReplace(t.Text(i)) }

func parse(src []byte) tree {
	t, err := tsbash.Parse(src)
	if err != nil {
		// tree-sitter fails only on allocation failure, where Python
		// raises MemoryError.
		panic(pystr.NewException("MemoryError", ""))
	}
	return tree{t}
}

// decompose is _decompose(command, _depth=rdepth), its frame at depth fd.
func (w *walker) decompose(command string, rdepth, fd int) Decomposition {
	w.enter(fd)
	src, encErr := pystr.EncodeUTF8(command)
	if encErr != nil {
		panic(encErr)
	}
	t := parse(src)
	if t.HasError {
		return Decomposition{Reason: "malformed shell (parse error)"}
	}
	fused := w.allFusedNewlineOffsets(t, src, fd+1)
	if len(fused) > 0 {
		if rdepth >= maxFusionSplitDepth {
			panic(pystr.NewException("AssertionError",
				"fusion-repair recursion exceeded; a rewrite re-fused, which should be impossible (each pass replaces newlines with ';')"))
		}
		ends := w.commentEndOffsets(t, fd+1)
		rewritten := w.rewriteFusedNewlines(src, fused, ends, fd+1)
		if string(rewritten) == string(src) {
			return Decomposition{Reason: "ambiguous shell (bare newline inside command — parser statement fusion)"}
		}
		if parse(rewritten).HasError {
			return Decomposition{Reason: "ambiguous shell (bare newline inside command — parser statement fusion)"}
		}
		return w.decompose(pystr.DecodeReplace(rewritten), rdepth+1, fd+1)
	}

	var heads, commands, reads, writes []string
	reason := ""
	var visit func(i, depth int)
	visit = func(i, depth int) {
		w.enter(depth)
		if reason != "" {
			return
		}
		n := t.Nodes[i]
		if n.Missing {
			reason = "malformed shell (missing token)"
			return
		}
		if n.Type == "file_redirect" {
			readPath, writePath, reject := w.classifyFileRedirect(t, i, depth+1)
			switch {
			case reject != nil:
				reason = *reject
			case readPath != nil:
				reads = append(reads, *readPath)
			case writePath != nil:
				writes = append(writes, *writePath)
			}
			return
		}
		if n.Type == "command" {
			if n.Name < 0 {
				reason = "command with no resolvable head"
				return
			}
			nm := t.Nodes[n.Name]
			if len(nm.Children) != 1 || t.typ(nm.Children[0]) != "word" {
				reason = fmt.Sprintf("non-literal command head (%s)", pystr.Repr(t.text(n.Name)))
				return
			}
			heads = append(heads, t.text(n.Name))
			commands = append(commands, t.text(i))
		}
		for _, c := range n.Children {
			visit(c, depth+1)
			if reason != "" {
				return
			}
		}
	}
	visit(0, fd+1)
	if reason != "" {
		return Decomposition{Reason: reason}
	}
	if len(heads) == 0 {
		return Decomposition{Reason: "no command heads found"}
	}
	return Decomposition{OK: true, Heads: heads, Commands: commands, Reads: reads, Writes: writes}
}

// literalText is _literal_text, its frame at depth fd.
func (w *walker) literalText(t tree, i, fd int) *string {
	w.enter(fd)
	switch t.typ(i) {
	case "word":
		s := t.text(i)
		return &s
	case "raw_string":
		s := pystr.Slice(t.text(i), 1, -1)
		return &s
	case "string":
		var parts []int
		for _, c := range t.Nodes[i].Children {
			if t.typ(c) != `"` {
				parts = append(parts, c)
			}
		}
		// all(c.type == "string_content" for c in parts): a generator
		// frame, entered even when parts is empty.
		w.enter(fd + 1)
		for _, c := range parts {
			if t.typ(c) != "string_content" {
				return nil
			}
		}
		// "".join(... for c in parts): another generator frame.
		w.enter(fd + 1)
		var b strings.Builder
		for _, c := range parts {
			b.WriteString(t.text(c))
		}
		s := b.String()
		return &s
	}
	return nil
}

// classifyFileRedirect is _classify_file_redirect, its frame at depth fd.
func (w *walker) classifyFileRedirect(t tree, i, fd int) (read, write, reject *string) {
	w.enter(fd)
	str := func(s string) *string { return &s }
	operator := ""
	haveOp := false
	destination := -1
	for _, c := range t.Nodes[i].Children {
		if t.typ(c) == "file_descriptor" {
			continue
		}
		if !haveOp {
			operator, haveOp = t.typ(c), true
			continue
		}
		if destination >= 0 {
			return nil, nil, str("ambiguous shell (a redirect with more than one target " + pystr.Repr(t.text(i)) + ")")
		}
		destination = c
	}
	if fdCloseOps[operator] && destination < 0 {
		return nil, nil, nil
	}
	if !haveOp || destination < 0 {
		return nil, nil, str("unrecognized shell redirection (" + pystr.Repr(t.text(i)) + ")")
	}
	if t.typ(destination) == "number" {
		if operator == ">&" || operator == "<&" {
			return nil, nil, nil
		}
		return nil, nil, str("unrecognized shell redirection (" + pystr.Repr(t.text(i)) + ")")
	}
	path := w.literalText(t, destination, fd+1)
	if path == nil {
		return nil, nil, str("non-literal redirect target (" + pystr.Repr(t.text(destination)) + ")")
	}
	if writeRedirectOps[operator] {
		return nil, path, nil
	}
	if readRedirectOps[operator] {
		return path, nil, nil
	}
	return nil, nil, str("unrecognized shell redirection operator (" + pystr.Repr(operator) + ")")
}

// bareNewlineOffsets is _bare_newline_offsets, its frame at depth fd.
func (w *walker) bareNewlineOffsets(t tree, i int, src []byte, fd int) []int {
	w.enter(fd)
	type span struct{ a, b int }
	var protected []span
	var collect func(m, depth int)
	collect = func(m, depth int) {
		w.enter(depth)
		if multilineLegal[t.typ(m)] {
			protected = append(protected, span{t.Nodes[m].Start, t.Nodes[m].End})
			return
		}
		for _, c := range t.Nodes[m].Children {
			collect(c, depth+1)
		}
	}
	collect(i, fd+1)
	var offsets []int
	n := t.Nodes[i]
	for k := n.Start; k < n.End; k++ {
		if src[k] != '\n' && src[k] != '\r' {
			continue
		}
		// any(a <= i < b for a, b in protected): a generator frame.
		w.enter(fd + 1)
		inside := false
		for _, p := range protected {
			if p.a <= k && k < p.b {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		if k > 0 && src[k-1] == '\\' {
			continue
		}
		offsets = append(offsets, k)
	}
	return offsets
}

// allFusedNewlineOffsets is _all_fused_newline_offsets, its frame at fd.
func (w *walker) allFusedNewlineOffsets(t tree, src []byte, fd int) []int {
	w.enter(fd)
	set := map[int]bool{}
	var walk func(i, depth int)
	walk = func(i, depth int) {
		w.enter(depth)
		if t.typ(i) == "command" {
			for _, o := range w.bareNewlineOffsets(t, i, src, depth+1) {
				set[o] = true
			}
		}
		for _, c := range t.Nodes[i].Children {
			walk(c, depth+1)
		}
	}
	walk(0, fd+1)
	out := make([]int, 0, len(set))
	for o := range set {
		out = append(out, o)
	}
	sort.Ints(out)
	return out
}

// commentEndOffsets is _comment_end_offsets, its frame at depth fd.
func (w *walker) commentEndOffsets(t tree, fd int) map[int]bool {
	w.enter(fd)
	ends := map[int]bool{}
	var walk func(i, depth int)
	walk = func(i, depth int) {
		w.enter(depth)
		if t.typ(i) == "comment" {
			ends[t.Nodes[i].End] = true
		}
		for _, c := range t.Nodes[i].Children {
			walk(c, depth+1)
		}
	}
	walk(0, fd+1)
	return ends
}

// rewriteFusedNewlines is _rewrite_fused_newlines, its frame at depth fd.
func (w *walker) rewriteFusedNewlines(src []byte, offsets []int, commentEnds map[int]bool, fd int) []byte {
	w.enter(fd)
	cut := map[int]bool{}
	for _, o := range offsets {
		cut[o] = true
	}
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		if cut[i] && !commentEnds[i] {
			// next((c for c in reversed(out) if ...), None): a generator frame.
			w.enter(fd + 1)
			prev := -1
			for j := len(out) - 1; j >= 0; j-- {
				if out[j] != ' ' && out[j] != '\t' {
					prev = int(out[j])
					break
				}
			}
			if prev == ';' || prev == '&' || prev == '|' {
				out = append(out, ' ')
			} else {
				out = append(out, ';')
			}
		} else {
			out = append(out, src[i])
		}
	}
	return out
}

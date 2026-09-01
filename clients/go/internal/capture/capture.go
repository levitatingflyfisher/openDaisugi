// Package capture is the oracle's capture conversion (opendaisugi/hook.py):
// the captured sessions under a captures root listed, and one session
// turned into a journal trace with an envelope inferred from what it did.
package capture

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// ErrUnreadable is a capture this binary does not convert the way Python
// does. The command refuses before it writes anything.
var ErrUnreadable = errors.New("cannot read")

// InvalidPlan is a session whose plan ActionPlan refuses in a way this
// binary words exactly: a NaN or an infinity in a step's data
// (models.non_finite_error). Python's ValidationError is a ValueError, so
// auto-tend skips the session with its text.
type InvalidPlan struct{ Text string }

func (e *InvalidPlan) Error() string { return e.Text }

// Session is one row of list_sessions.
type Session struct {
	ID     string
	Calls  int
	LastAt any
	Path   string
}

func lines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: %s is not UTF-8", ErrUnreadable, filepath.Base(path))
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n")
	parts := strings.SplitAfter(text, "\n")
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func number(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case pyjson.Int:
		n, ok := new(big.Int).SetString(x.Text, 10)
		if !ok {
			return nil, false
		}
		return new(big.Rat).SetInt(n), true
	case pyjson.Float:
		r := new(big.Rat)
		if r.SetFloat64(float64(x)) == nil {
			return nil, false
		}
		return r, true
	case bool:
		if x {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	}
	return nil, false
}

// ListSessions is list_sessions(root): every *.jsonl with a readable first
// and last line, newest last_at first.
func ListSessions(root string) ([]Session, error) {
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names, err := filepath.Glob(filepath.Join(globEscape(root), "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var rows []Session
	for _, f := range names {
		if fi, err := os.Stat(f); err != nil || fi.IsDir() {
			continue
		}
		ls, err := lines(f)
		if err != nil {
			return nil, err
		}
		var first, last string
		calls := 0
		for _, l := range ls {
			if pystr.Strip(l) == "" {
				continue
			}
			if calls == 0 {
				first = l
			}
			last = l
			calls++
		}
		if calls == 0 {
			continue
		}
		fv, err1 := pyjson.LoadsPy(first, 900)
		lv, err2 := pyjson.LoadsPy(last, 900)
		if err1 != nil || err2 != nil {
			continue
		}
		fo, ok1 := fv.(*pyjson.Object)
		lo, ok2 := lv.(*pyjson.Object)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%w: a line of %s is not a JSON object", ErrUnreadable, filepath.Base(f))
		}
		_ = fo
		lastAt, _ := lo.Get("captured_at")
		if lastAt != nil {
			if _, ok := number(lastAt); !ok {
				return nil, fmt.Errorf("%w: captured_at in %s is not a number", ErrUnreadable, filepath.Base(f))
			}
		}
		rows = append(rows, Session{ID: strings.TrimSuffix(filepath.Base(f), ".jsonl"), Calls: calls, LastAt: lastAt, Path: f})
	}
	key := func(v any) *big.Rat {
		if n, ok := number(v); ok && n.Sign() != 0 {
			return n
		}
		return new(big.Rat)
	}
	sort.SliceStable(rows, func(i, j int) bool { return key(rows[i].LastAt).Cmp(key(rows[j].LastAt)) > 0 })
	return rows, nil
}

func globEscape(s string) string {
	r := strings.NewReplacer("*", "[*]", "?", "[?]", "[", "[[]")
	return r.Replace(s)
}

// Records reads a session's records: each non-blank line's JSON, lines
// that do not parse skipped. A record whose shape the conversion would
// raise on is ErrUnreadable.
func Records(path string) ([]*pyjson.Object, error) {
	ls, err := lines(path)
	if err != nil {
		return nil, err
	}
	var out []*pyjson.Object
	for _, l := range ls {
		l = pystr.Strip(l)
		if l == "" {
			continue
		}
		v, derr := pyjson.LoadsPy(l, 900)
		if derr != nil {
			continue
		}
		o, ok := v.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("%w: a record of %s is not a JSON object", ErrUnreadable, filepath.Base(path))
		}
		st, has := o.Get("step_type")
		if _, isStr := st.(string); !has || !isStr {
			return nil, fmt.Errorf("%w: a record of %s has no step_type", ErrUnreadable, filepath.Base(path))
		}
		for _, k := range []string{"command", "path", "url", "mcp_server", "mcp_tool"} {
			if v, ok := o.Get(k); ok && pyjson.Truthy(v) {
				if _, isStr := v.(string); !isStr {
					return nil, fmt.Errorf("%w: %s in a record of %s is not text", ErrUnreadable, k, filepath.Base(path))
				}
			}
		}
		if v, ok := o.Get("arguments"); ok && pyjson.Truthy(v) {
			if _, isObj := v.(*pyjson.Object); !isObj {
				return nil, fmt.Errorf("%w: arguments in a record of %s is not an object", ErrUnreadable, filepath.Base(path))
			}
		}
		out = append(out, o)
	}
	return out, nil
}

func strOr(o *pyjson.Object, k string) string {
	if v, ok := o.Get(k); ok {
		if s, isStr := v.(string); isStr {
			return s
		}
	}
	return ""
}

// posixPath is pathlib.PurePosixPath's root and parts.
func posixPath(s string) (root string, parts []string) {
	switch {
	case strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "///"):
		root = "//"
	case strings.HasPrefix(s, "/"):
		root = "/"
	}
	for _, p := range strings.Split(s, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	return root, parts
}

func pathStr(root string, parts []string) string {
	s := root + strings.Join(parts, "/")
	if s == "" {
		return "."
	}
	return s
}

// globForPath is hook._glob_for_path.
func globForPath(path string) string {
	if path == "" {
		return "**"
	}
	root, parts := posixPath(path)
	parent := parts
	if len(parts) > 0 {
		parent = parts[:len(parts)-1]
	}
	if root != "" {
		p := "/"
		if len(parts) > 0 {
			p = pathStr(root, parent)
		}
		return strings.TrimRight(p, "/") + "/**"
	}
	p := pathStr(root, parent)
	if p == "" || p == "." {
		return "./**"
	}
	return "./" + strings.TrimRight(p, "/") + "/**"
}

const maxObserveDepth = 4

// observedEffects is hook._observed_effects.
func observedEffects(command string, decompose bool) (heads, reads, writes []string) {
	var collect func(cmd string, depth int)
	collect = func(cmd string, depth int) {
		if depth > maxObserveDepth {
			return
		}
		var simple []string
		ok := false
		if decompose {
			d := verify.DecomposeCommand(cmd)
			if d.OK {
				reads = append(reads, d.Reads...)
				writes = append(writes, d.Writes...)
				simple, ok = d.Commands, true
			}
		}
		if !ok {
			simple = []string{cmd}
		}
		for _, s := range simple {
			if h, found := verify.ExtractShellHead(pystr.Strip(s)); found {
				heads = append(heads, h)
			}
			if p, found := verify.ParseInterpreter(s); found && !p.Opaque {
				for _, inner := range p.InnerCommands {
					collect(inner, depth+1)
				}
			}
		}
	}
	collect(command, 0)
	return heads, reads, writes
}

var urlHost = regexp.MustCompile(`^https?://([^/]+)`)

var sanctionedWrite = map[string]bool{"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true}
var sanctionedRead = map[string]bool{"/dev/null": true, "/dev/stdin": true}

func sortedAny(set map[string]bool) []any {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// InferEnvelope is hook.infer_envelope: an envelope admitting every
// captured call, stakes low, with a fresh id.
func InferEnvelope(records []*pyjson.Object, task string, decompose bool) (*pyjson.Object, error) {
	heads, reads, writes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	hosts, mcp := map[string]bool{}, map[string]bool{}
	network := false
	for _, r := range records {
		switch strOr(r, "step_type") {
		case "shell":
			cmd := pystr.Strip(strOr(r, "command"))
			if cmd == "" {
				continue
			}
			h, rs, ws := observedEffects(cmd, decompose)
			for _, x := range h {
				heads[x] = true
			}
			for _, p := range rs {
				if !sanctionedRead[p] {
					reads[globForPath(p)] = true
				}
			}
			for _, p := range ws {
				if !sanctionedWrite[p] {
					writes[globForPath(p)] = true
				}
			}
		case "file_read":
			reads[globForPath(strOr(r, "path"))] = true
		case "file_write":
			writes[globForPath(strOr(r, "path"))] = true
		case "network":
			network = true
			if m := urlHost.FindStringSubmatch(strOr(r, "url")); m != nil {
				hosts[pystr.Lower(m[1])] = true
			}
		case "mcp":
			server, tool := strOr(r, "mcp_server"), strOr(r, "mcp_tool")
			if server != "" && tool != "" && !(server == "opencode" && tool == "apply_patch") {
				mcp[server+"/"+tool] = true
			}
		}
	}
	perm := pyjson.NewObject().Set("shell", len(heads) > 0).Set("shell_allowlist", sortedAny(heads)).
		Set("shell_allow_decomposition", decompose).Set("mcp_allowlist", sortedAny(mcp)).
		Set("file_read", sortedAny(reads)).Set("file_write", sortedAny(writes)).Set("network", network).
		Set("network_hosts", sortedAny(hosts)).Set("max_execution_time_s", pyjson.Int{Text: "60"}).
		Set("max_output_size_mb", pyjson.Int{Text: "20"})
	env := pyjson.NewObject().Set("generated_by", "opendaisugi.hook.infer_envelope").Set("task", task).
		Set("permissions", perm).Set("stakes", "low")
	out, verr := pmodel.Envelope.ValidateObject(env, pmodel.Python)
	if verr != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, verr)
	}
	return out, nil
}

// Plan is ActionPlan(source="hook-capture", task, _records_to_steps(...)).
func Plan(records []*pyjson.Object, task string) (*pyjson.Object, error) {
	steps := []any{}
	prev := ""
	for i, r := range records {
		sid := fmt.Sprintf("s%d", i)
		deps := []any{}
		if prev != "" {
			deps = append(deps, prev)
		}
		var st *pyjson.Object
		switch strOr(r, "step_type") {
		case "shell":
			st = pyjson.NewObject().Set("id", sid).Set("depends_on", deps).Set("type", "shell").Set("command", strOr(r, "command"))
		case "file_read":
			st = pyjson.NewObject().Set("id", sid).Set("depends_on", deps).Set("type", "file_read").Set("path", strOr(r, "path"))
		case "file_write":
			st = pyjson.NewObject().Set("id", sid).Set("depends_on", deps).Set("type", "file_write").
				Set("path", strOr(r, "path")).Set("content", "")
		case "network":
			st = pyjson.NewObject().Set("id", sid).Set("depends_on", deps).Set("type", "network").Set("url", strOr(r, "url"))
		case "mcp":
			args := pyjson.NewObject()
			if v, ok := r.Get("arguments"); ok && pyjson.Truthy(v) {
				args = v.(*pyjson.Object)
			}
			st = pyjson.NewObject().Set("id", sid).Set("depends_on", deps).Set("type", "mcp").
				Set("server", strOr(r, "mcp_server")).Set("tool", strOr(r, "mcp_tool")).Set("arguments", args)
		}
		if st != nil {
			steps = append(steps, st)
		}
		prev = sid
	}
	plan := pyjson.NewObject().Set("source", "hook-capture").Set("task", task).Set("steps", steps)
	out, verr := pmodel.ActionPlan.ValidateObject(plan, pmodel.Python)
	if verr != nil {
		finite := true
		for _, e := range verr.Errs {
			finite = finite && e.Type == "finite_number"
		}
		if finite {
			return nil, &InvalidPlan{verr.String()}
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreadable, verr)
	}
	return out, nil
}

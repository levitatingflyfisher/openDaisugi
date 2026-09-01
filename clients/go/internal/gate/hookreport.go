package gate

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// HookReport is _state_report.hook_report_argv as the resident server
// runs it: argv after "hook report", the event on raw, and the caller's
// pane identity. environ is the server's own environment; the report
// never reads it for pane identity. It returns the reply's stdout, stderr
// and exit code, and never panics out.
func HookReport(argv []string, raw []byte, caller Caller, environ []string) (stdout, stderr string, exit int) {
	defer func() {
		if p := recover(); p != nil {
			stdout, stderr, exit = "", fmt.Sprintf("daisugi hook report: %v", p), 1
		}
	}()
	env := map[string]string{}
	for _, kv := range PaneFreeEnviron(environ) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	r := &runner{env: env, t0: time.Now(), resident: true, caller: caller}
	r.home = r.expanduser("~")
	pane, root, ok := parseHookReportArgv(argv)
	if !ok {
		return "", "daisugi hook report: bad arguments", 1
	}
	if root == nil {
		d := pathJoin(pathStr(r.home), ".opendaisugi/gate")
		root = &d
	}
	text, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return "", "daisugi hook report: not valid JSON: " + exc.Msg, 1
	}
	v, derr := pyjson.LoadsPy(text, requestDepth)
	if derr != nil {
		return "", "daisugi hook report: not valid JSON: " + derr.Error(), 1
	}
	row, isObj := v.(*pyjson.Object)
	if !isObj {
		return "", "daisugi hook report: event must be a JSON object", 1
	}
	if s := row.Value("source"); s == "gate" || s == "operator" {
		row.Set("source", "headless")
	}
	if pane != nil {
		row.Set("pane", *pane)
	}
	if msg := validateHookReportRow(row); msg != "" {
		return "", "daisugi hook report: " + msg, 1
	}
	func() {
		defer func() { _ = recover() }()
		appendReportTree(*root, row)
	}()
	func() {
		defer func() { _ = recover() }()
		r.reportState(row)
	}()
	return "", "", 0
}

// parseHookReportArgv is _build_hook_report_parser().parse_args(argv):
// --pane and --root, each taking one value, abbreviations allowed, no
// --help and no positionals. ok is false where argparse would exit.
func parseHookReportArgv(argv []string) (pane, root *string, ok bool) {
	opts := []string{"--pane", "--root"}
	negNum := regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)
	// pattern: 'O' an option (with its action, or "" for an unknown
	// one), 'A' an argument, '-' the first "--".
	type word struct {
		kind     byte
		action   string
		explicit *string
		text     string
	}
	var words []word
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			words = append(words, word{kind: '-', text: a})
			for _, rest := range argv[i+1:] {
				words = append(words, word{kind: 'A', text: rest})
			}
			break
		}
		w, isOpt, bad := classifyOptional(a, opts, negNum)
		if bad {
			return nil, nil, false
		}
		if !isOpt {
			words = append(words, word{kind: 'A', text: a})
			continue
		}
		words = append(words, word{kind: 'O', action: w.action, explicit: w.explicit, text: a})
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		// No positionals: an argument no option takes is unrecognized, and
		// so is "--" (Python 3.12.12 keeps it as an argument).
		if w.kind != 'O' || w.action == "" {
			return nil, nil, false
		}
		var val string
		if w.explicit != nil {
			val = *w.explicit
		} else {
			// nargs=None: the next word, when it is an argument.
			if i+1 >= len(words) || words[i+1].kind != 'A' {
				return nil, nil, false
			}
			i++
			val = words[i].text
		}
		v := val
		if w.action == "--pane" {
			pane = &v
		} else {
			p := pathStr(v)
			root = &p
		}
	}
	return pane, root, true
}

type optMatch struct {
	action   string
	explicit *string
}

// classifyOptional is ArgumentParser._parse_optional for a parser whose
// only options are opts: (match, true) for an option word (an unknown one
// has action ""), false for an argument, bad for argparse's own error (an
// ambiguous abbreviation).
func classifyOptional(a string, opts []string, negNum *regexp.Regexp) (m optMatch, isOpt, bad bool) {
	if a == "" || a[0] != '-' {
		return m, false, false
	}
	for _, o := range opts {
		if a == o {
			return optMatch{action: o}, true, false
		}
	}
	if len(pystr.Runes(a)) == 1 {
		return m, false, false
	}
	if k, v, has := strings.Cut(a, "="); has {
		for _, o := range opts {
			if k == o {
				val := v
				return optMatch{action: o, explicit: &val}, true, false
			}
		}
	}
	// Abbreviations: only a word that starts with "--" can match these
	// options (a single-dash word's prefixes never start "--").
	if strings.HasPrefix(a, "--") {
		prefix, explicit, hasEq := strings.Cut(a, "=")
		var found []optMatch
		for _, o := range opts {
			if strings.HasPrefix(o, prefix) {
				m := optMatch{action: o}
				if hasEq {
					e := explicit
					m.explicit = &e
				}
				found = append(found, m)
			}
		}
		if len(found) > 1 {
			return m, false, true
		}
		if len(found) == 1 {
			return found[0], true, false
		}
	}
	if negNum.MatchString(a) {
		return m, false, false
	}
	if strings.Contains(a, " ") {
		return m, false, false
	}
	return optMatch{}, true, false
}

// validateHookReportRow is _state_report._validate_hook_report_row: ""
// when the row passes, else the ValueError's text.
func validateHookReportRow(row *pyjson.Object) string {
	for _, k := range []string{"session_id", "harness", "state", "source", "ts"} {
		if _, has := row.Get(k); !has {
			return fmt.Sprintf("event missing required field %s", pystr.Repr(k))
		}
	}
	for _, k := range []string{"session_id", "harness", "state", "source"} {
		if _, isStr := row.Value(k).(string); !isStr {
			return k + " must be a string"
		}
	}
	if !isPyNumber(row.Value("ts")) {
		return "ts must be a number"
	}
	state, source := row.Value("state").(string), row.Value("source").(string)
	switch state {
	case "idle", "working", "blocked", "done", "unknown":
	default:
		return "unknown state " + pystr.Repr(state)
	}
	switch source {
	case "operator", "gate", "headless", "process", "manifest":
	default:
		return "unknown source " + pystr.Repr(source)
	}
	if state == "done" && source != "process" && source != "headless" {
		return "state 'done' may only come from source 'process' or 'headless'"
	}
	detail := any("")
	if d, has := row.Get("detail"); has {
		detail = d
	}
	ds, isStr := detail.(string)
	if !isStr {
		return "detail must be a string"
	}
	if pystr.Len(ds) > 200 {
		return "detail exceeds 200 characters"
	}
	if h := row.Value("harness_session_id"); h != nil {
		if _, isStr := h.(string); !isStr {
			return "harness_session_id must be a string"
		}
	}
	if p := row.Value("pane"); p != nil {
		if _, isStr := p.(string); !isStr {
			return "pane must be a string"
		}
	}
	askV := row.Value("ask")
	if askV != nil {
		ask, isObj := askV.(*pyjson.Object)
		if !isObj {
			return "ask must be an object"
		}
		if state != "blocked" {
			return "ask is only valid when state == 'blocked'"
		}
		for _, k := range []string{"id", "tool", "summary", "deadline"} {
			if _, has := ask.Get(k); !has {
				return fmt.Sprintf("ask missing required field %s", pystr.Repr(k))
			}
		}
		for _, k := range []string{"id", "tool", "summary"} {
			if _, isStr := ask.Value(k).(string); !isStr {
				return "ask " + k + " must be a string"
			}
		}
		if !isPyNumber(ask.Value("deadline")) {
			return "ask deadline must be a number"
		}
		if t := ask.Value("tier"); t != nil {
			if _, isStr := t.(string); !isStr {
				return "ask tier must be a string"
			}
		}
	}
	if state == "blocked" && source == "gate" && askV == nil {
		return "a gate blocked event needs an ask"
	}
	v := any(pyjson.Int{Text: "1"})
	if x, has := row.Get("v"); has {
		v = x
	}
	n, isInt := v.(pyjson.Int)
	if !isInt {
		return "invalid schema version " + pyValueRepr(v)
	}
	if n.Text != "1" {
		return "unsupported event schema version " + n.Text
	}
	return ""
}

// isPyNumber is isinstance(v, (int, float)) and not a bool.
func isPyNumber(v any) bool {
	switch v.(type) {
	case pyjson.Int, pyjson.Float:
		return true
	}
	return false
}

// appendReportTree is hook_report_argv's tree step:
// SessionTree.open_or_create under root's parent, then append("state",
// row). A step that raises in the oracle stops the rest here too.
func appendReportTree(root string, row *pyjson.Object) {
	dir := pathJoin(pathParent(root), "sessions")
	sid := safeSessionID(row.Value("session_id").(string))
	file := pathJoin(dir, sid+".jsonl")
	var head any
	if !exists(file) {
		// SessionTree.create: the directory 0700, the header, then 0600.
		if mkdirPrivate(dir) != nil {
			return
		}
		if exists(file) {
			head = readHead(file)
		} else {
			hsid := row.Value("harness_session_id")
			header := pyjson.NewObject().
				Set("type", "session").
				Set("id", sid).
				Set("ts", pyjson.Float(pyTime(time.Now()))).
				Set("v", 1).
				Set("harness", row.Value("harness")).
				Set("cwd", "").
				Set("harnessSessionId", hsid).
				Set("transcriptPath", nil).
				Set("parentSession", nil).
				Set("parentEntry", nil).
				Set("cacheKey", nil)
			f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
			if err != nil {
				return
			}
			line := dumpsLine(header, false)
			if pystr.HasSurrogate(line) {
				// The encoder raises before anything is written, and
				// before create() makes the file private.
				f.Close()
				return
			}
			_, werr := f.WriteString(line)
			f.Close()
			if werr != nil {
				return
			}
			_ = os.Chmod(file, 0o600)
		}
	} else {
		head = readHead(file)
	}
	entry := pyjson.NewObject().
		Set("type", "state").
		Set("id", randomHex(4)).
		Set("parentId", head).
		Set("ts", pyjson.Float(pyTime(time.Now())))
	for _, k := range row.Keys() {
		switch k {
		case "type", "id", "parentId", "ts":
			continue
		}
		entry.Set(k, row.Value(k))
	}
	_ = appendText(file, dumpsLine(entry, false))
}

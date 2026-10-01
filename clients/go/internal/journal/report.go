// Package journal reads the gate's verdict journal: the audit log each
// gate call appends one JSON line to, under <root>/audit/<session>.jsonl.
// Report is gate.audit_report; Text is the lines `daisugi gate report`
// prints.
package journal

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pyjson"
)

// ErrUnsupported marks a log line holding a value this package does not
// print the way Python does (a float, a list or an object where a string
// is expected). The command is refused; nothing is written anyway.
var ErrUnsupported = errors.New("the audit log holds a value this binary does not print yet")

// CrashError is a log Python's own report fails on: it raises instead of
// reporting. The Go report stops with the same exit code and says why.
type CrashError struct{ Why string }

func (e *CrashError) Error() string { return e.Why }

// Report is the dict audit_report returns, keys in order.
type Report struct {
	Records []*pyjson.Object
	Denied  []*pyjson.Object
	FP      []*pyjson.Object
	Reasons *pyjson.Object
	// Word are the records with a dialect audit line (gate._word_lines).
	Word []*pyjson.Object
}

// wordLines is gate._word_lines: a record's word_audit list, else none.
func wordLines(r *pyjson.Object) []any {
	l, _ := r.Value("word_audit").([]any)
	return l
}

// Files is the list audit_report reads: from the legacy directory (the
// log written while audit mode was called shadow mode), then the audit
// directory, one session's file or every *.jsonl in each.
func Files(root, session string, hasSession bool) ([]string, error) {
	var out []string
	for _, d := range []string{gateroot.LegacyAuditDir(root), gateroot.AuditDir(root)} {
		got, err := dirFiles(d, session, hasSession)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

func dirFiles(d, session string, hasSession bool) ([]string, error) {
	if hasSession {
		return []string{gateroot.Join(d, gateroot.SafeSessionID(session)+".jsonl")}, nil
	}
	if _, err := os.Stat(d); err != nil {
		return nil, nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, &CrashError{Why: err.Error()}
	}
	var out []string
	for _, e := range entries {
		if !utf8.ValidString(e.Name()) {
			return nil, ErrUnsupported
		}
		if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, gateroot.Join(d, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Build is audit_report over the given files.
func Build(files []string) (*Report, error) {
	rep := &Report{Reasons: pyjson.NewObject()}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, &CrashError{Why: fmt.Sprintf("cannot read %s: %v", f, err)}
		}
		if !utf8.Valid(raw) {
			return nil, &CrashError{Why: f + " is not valid UTF-8"}
		}
		for _, line := range splitlines(string(raw)) {
			if pyStrip(line) == "" {
				continue
			}
			v, err := pyjson.Loads(line)
			if err != nil {
				if errors.Is(err, pyjson.ErrUnsupported) {
					return nil, ErrUnsupported
				}
				continue
			}
			o, isObj := v.(*pyjson.Object)
			if !isObj {
				continue // JSON but not a record: skipped like a line that is not JSON
			}
			rep.Records = append(rep.Records, o)
		}
	}
	return rep.summarize()
}

// FromRecords is gate._build_report over records already in hand (the
// records gate replay decides).
func FromRecords(records []*pyjson.Object) (*Report, error) {
	rep := &Report{Reasons: pyjson.NewObject(), Records: records}
	return rep.summarize()
}

// summarize fills the counts, the denied list and the reasons from the
// records, as _build_report does.
func (rep *Report) summarize() (*Report, error) {
	for _, r := range rep.Records {
		if len(wordLines(r)) > 0 {
			rep.Word = append(rep.Word, r)
		}
	}
	for _, r := range rep.Records {
		if !pyjson.Truthy(r.Value("would_deny")) {
			continue
		}
		rep.Denied = append(rep.Denied, r)
		reason := r.Value("reason")
		text := ""
		if pyjson.Truthy(reason) {
			s, isStr := reason.(string)
			if !isStr {
				return nil, &CrashError{Why: "a denied record's reason is not a string"}
			}
			text = s
		}
		key := headRunes(text, 120)
		n := 0
		if v, present := rep.Reasons.Get(key); present {
			n = v.(int)
		}
		rep.Reasons.Set(key, n+1)
		if strings.Contains(text, "metacharacters") || strings.HasPrefix(text, "unrecognized tool") {
			rep.FP = append(rep.FP, r)
		}
	}
	return rep, nil
}

// JSON is json.dumps(rep, indent=2).
func (rep *Report) JSON() string {
	o := pyjson.NewObject().
		Set("calls", len(rep.Records)).
		Set("allowed", len(rep.Records)-len(rep.Denied)).
		Set("would_deny", len(rep.Denied)).
		Set("word_would_deny", len(rep.Word)).
		Set("word_denied", objs(rep.Word)).
		Set("reasons", rep.Reasons).
		Set("denied", objs(rep.Denied)).
		Set("false_positive_candidates", objs(rep.FP))
	return pyjson.DumpsIndent(o, 2, true)
}

func objs(l []*pyjson.Object) []any {
	out := make([]any, len(l))
	for i, o := range l {
		out[i] = o
	}
	return out
}

// Text is the plain report: a counts line, then one line per denied call.
func (rep *Report) Text() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "calls=%d allowed=%d would_deny=%d false_positive_candidates=%d word_would_deny=%d\n",
		len(rep.Records), len(rep.Records)-len(rep.Denied), len(rep.Denied), len(rep.FP), len(rep.Word))
	// `r in candidates` compares by value, but two equal records carry the
	// same reason and so the same class: membership by identity agrees.
	isFP := map[*pyjson.Object]bool{}
	for _, c := range rep.FP {
		isFP[c] = true
	}
	for _, r := range rep.Denied {
		fp := ""
		if isFP[r] {
			fp = " [FP-candidate]"
		}
		tool, err := pyStr(r.Value("tool_name"))
		if err != nil {
			return "", err
		}
		detail := any("")
		if v, present := r.Get("detail"); present {
			detail = v
		}
		dr, err := pyRepr(detail)
		if err != nil {
			return "", err
		}
		reason, err := pyStr(r.Value("reason"))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  DENY%s %s %s: %s\n", fp, tool, dr, reason)
	}
	for _, r := range rep.Word {
		tool, err := pyStr(r.Value("tool_name"))
		if err != nil {
			return "", err
		}
		detail := any("")
		if v, present := r.Get("detail"); present {
			detail = v
		}
		dr, err := pyRepr(detail)
		if err != nil {
			return "", err
		}
		for _, line := range wordLines(r) {
			text, err := pyStr(line)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "  WORD %s %s: %s\n", tool, dr, text)
		}
	}
	return b.String(), nil
}

// WordWouldDeny is the report's word_would_deny: the calls a word of the
// system dialect would deny.
func (rep *Report) WordWouldDeny() int { return len(rep.Word) }

// pyStr is str(v) for the JSON values whose str this package models.
func pyStr(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "None", nil
	case bool:
		if x {
			return "True", nil
		}
		return "False", nil
	case pyjson.Int:
		return x.Text, nil
	case pyjson.Float:
		f := float64(x)
		switch {
		case math.IsNaN(f):
			return "nan", nil
		case math.IsInf(f, 1):
			return "inf", nil
		case math.IsInf(f, -1):
			return "-inf", nil
		}
		return pyjson.FloatRepr(f), nil
	case string:
		return x, nil
	}
	return "", ErrUnsupported
}

// pyRepr is repr(v) for the same values.
func pyRepr(v any) (string, error) {
	if s, isStr := v.(string); isStr {
		return reprStr(s), nil
	}
	return pyStr(v)
}

// isPrintable is str.isprintable for one character: false for the Other
// and Separator categories (unassigned included), except the space. Go
// and Python 3.12 both carry Unicode 15.0.
func isPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs, unicode.Zl, unicode.Zp, unicode.Zs) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

// reprStr is repr(str).
func reprStr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case isPrintable(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func headRunes(s string, n int) string {
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

// splitlines is str.splitlines().
func splitlines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		brk := 0
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			brk = size
		case '\r':
			brk = size
			if i+1 < len(s) && s[i+1] == '\n' {
				brk = 2
			}
		}
		if brk > 0 {
			out = append(out, s[start:i])
			i += brk
			start = i
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// pyStrip is str.strip() with no argument: Unicode whitespace.
func pyStrip(s string) string {
	return strings.TrimFunc(s, isPySpace)
}

func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// WordWouldDeny is gate.word_would_deny: the records of the audit log
// under root with a non-empty word_audit list. A file it cannot read, or
// that is not UTF-8, is skipped, and so is a line that is not a JSON
// object.
func WordWouldDeny(root string) (int, error) {
	count := 0
	for _, d := range []string{gateroot.LegacyAuditDir(root), gateroot.AuditDir(root)} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		files, err := dirFiles(d, "", false)
		if err != nil {
			var crash *CrashError
			if errors.As(err, &crash) {
				continue
			}
			return 0, err
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil || !utf8.Valid(raw) {
				continue
			}
			for _, line := range splitlines(string(raw)) {
				if pyStrip(line) == "" {
					continue
				}
				v, err := pyjson.Loads(line)
				if err != nil {
					if errors.Is(err, pyjson.ErrUnsupported) {
						return 0, ErrUnsupported
					}
					continue
				}
				if o, isObj := v.(*pyjson.Object); isObj && len(wordLines(o)) > 0 {
					count++
				}
			}
		}
	}
	return count, nil
}

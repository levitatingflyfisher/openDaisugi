// Package transcript reads agent transcripts into episodes as the
// oracle's parsers do (opendaisugi/parsers): ClaudeCodeParser for Claude
// Code .jsonl sessions and CodexParser for Codex rollouts. An episode is
// the work between two real user turns, with each tool call as a typed
// step; small episodes merge into the one before, and large ones are
// split by a model (the caller supplies the split).
//
// The oracle reads each value with Python's duck typing. This package
// reads the value types a real transcript holds; a value of a type where
// the oracle would raise, or would print a Python repr, is ErrUnreadable,
// for the caller to refuse.
package transcript

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"daisugi-verify/internal/lazyre"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// ErrUnreadable is input this package does not read the way the oracle
// does.
var ErrUnreadable = errors.New("a transcript value this binary does not read yet")

func unreadable(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnreadable, fmt.Sprintf(format, a...))
}

// Message is one flat {role, content} message.
type Message struct {
	Role    any
	Content any
	// HasContent is whether the row had a content key (a flat row may not).
	HasContent bool
}

// step is a step dict before _finalize: its type and the fields that
// type reads.
type step struct {
	typ           string
	path, command any // the transcript's values, of any type
	url, query    any
	prompt        any
	skillID       any
	skillInput    *pyjson.Object
	server, tool  string
	arguments     *pyjson.Object
	prevHint      bool
	prevIndex     int // -1: none
}

// RawEpisode is parsers.claude_code._RawEpisode.
type RawEpisode struct {
	// Task is a str, or for a split episode the model's value as it is.
	Task         any
	FirstMessage int
	LastMessage  int
	steps        []step
	// StepStart and StepEnd are the split's start_index and end_index,
	// nil for an episode that was not split.
	StepStart, StepEnd any
}

// Steps is the number of steps.
func (r *RawEpisode) Steps() int { return len(r.steps) }

// get is dict.get(k) on a JSON object: the value, or nil.
func get(o *pyjson.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

// truthy is bool(v).
func truthy(v any) bool { return pyjson.Truthy(v) }

// or2 is `a or b`.
func or2(a, b any) any {
	if truthy(a) {
		return a
	}
	return b
}

// Lines is Python's text-mode iteration of a file: universal newlines.
func Lines(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			out = append(out, text[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				out = append(out, text[start:i+2])
				i++
			} else {
				out = append(out, text[start:i+1])
			}
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// loadLine is json.loads of a stripped line: nil and false when the line
// is not JSON. The ValueError of an int past the digit limit ends the
// parse; a line nested past the decoder's stack is unreadable (where
// Python's RecursionError comes depends on its C stack).
func loadLine(line string) (any, bool, error) {
	v, derr := pyjson.LoadsPy(line, 900)
	if derr != nil {
		if derr.TooDeep {
			return nil, false, unreadable("a transcript line nested deeper than this binary decodes")
		}
		if derr.NotJSON {
			return nil, false, &ParseError{Msg: derr.RawError}
		}
		return nil, false, nil
	}
	return v, true, nil
}

// Read is parse() up to the merge: the transcript's bytes read as
// open(path, encoding="utf-8") reads them, the messages, and the raw
// episodes. An exception the parse raises is a *ParseError; input this
// package does not read the oracle's way is ErrUnreadable.
func Read(raw []byte, format string) ([]*RawEpisode, error) {
	text, dec := pystr.DecodeStream(raw)
	var msgs []Message
	var err error
	if format == "codex" {
		msgs, err = CodexMessages(text)
	} else {
		msgs, err = ClaudeMessages(text)
	}
	// The lines read before a byte that is not UTF-8 are parsed first,
	// so an error in them comes first.
	if err != nil {
		return nil, err
	}
	if dec != nil {
		return nil, &ParseError{Msg: dec.Msg}
	}
	return Identify(msgs)
}

// ClaudeMessages is ClaudeCodeParser._read_messages.
func ClaudeMessages(text string) ([]Message, error) {
	var out []Message
	for _, raw := range Lines(text) {
		line := pystr.Strip(raw)
		if line == "" {
			continue
		}
		v, ok, err := loadLine(line)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		d, isObj := v.(*pyjson.Object)
		if !isObj {
			continue
		}
		rowType, hasType := d.Get("type")
		rt, rtStr := rowType.(string)
		if hasType && rtStr && (rt == "user" || rt == "assistant") {
			if msg, isObj := get(d, "message").(*pyjson.Object); isObj {
				content, has := msg.Get("content")
				if !has {
					content = ""
				}
				out = append(out, Message{Role: or2(get(msg, "role"), rowType), Content: content, HasContent: true})
				continue
			}
		}
		if truthy(rowType) {
			// `row_type not in ("user", "assistant")` on an unhashable
			// value compares by ==: never a member.
			if !(rtStr && (rt == "user" || rt == "assistant")) {
				continue
			}
		}
		if _, has := d.Get("role"); has {
			content, hasC := d.Get("content")
			out = append(out, Message{Role: get(d, "role"), Content: content, HasContent: hasC})
		}
	}
	return out, nil
}

// maxTaskChars is _MAX_TASK_CHARS.
const maxTaskChars = 2000

var (
	reReminder = lazyre.New(`(?s)<system-reminder>.*?</system-reminder>`)
	reLocalCmd = lazyre.New(`(?s)<local-command-[^>]*>.*?</local-command-[^>]*>`)
	reCmdMsg   = lazyre.New(`(?s)<command-message>.*?</command-message>`)
	reCmdTag   = lazyre.New(`</?command-(name|args)>`)
)

// cleanTask is _clean_task.
func cleanTask(text string) string {
	t := pystr.Strip(text)
	if strings.HasPrefix(t, "Base directory for this skill:") {
		first, _, _ := strings.Cut(t, "\n")
		pathPart := ""
		if _, after, found := strings.Cut(first, ":"); found {
			pathPart = pystr.Strip(after)
		}
		name := strings.TrimRight(pathPart, "/")
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if name == "" {
			name = "unknown"
		}
		return "skill: " + name
	}
	if strings.HasPrefix(t, "This session is being continued") {
		return "session continuation"
	}
	t = reReminder().ReplaceAllString(t, " ")
	t = reLocalCmd().ReplaceAllString(t, " ")
	t = reCmdMsg().ReplaceAllString(t, " ")
	t = reCmdTag().ReplaceAllString(t, " ")
	return strings.Join(pystr.Split(t), " ")
}

// userText is _user_text: the task label of a real user turn, or false.
func userText(m Message) (string, bool, error) {
	role, isStr := m.Role.(string)
	if !isStr || (role != "user" && role != "human") {
		return "", false, nil
	}
	var text string
	switch c := m.Content.(type) {
	case string:
		text = c
	case []any:
		if len(c) != 1 {
			return "", false, nil
		}
		b, isObj := c[0].(*pyjson.Object)
		if !isObj || get(b, "type") != "text" {
			return "", false, nil
		}
		inner, isStr := get(b, "text").(string)
		if !isStr {
			return "", false, nil
		}
		text = inner
	default:
		return "", false, nil
	}
	cleaned := cleanTask(text)
	if cleaned == "" {
		cleaned = pystr.Slice(pystr.Strip(text), 0, 80)
	}
	return pystr.Slice(cleaned, 0, maxTaskChars), true, nil
}

// toolUses is _extract_tool_uses: the tool_use blocks of an assistant
// message.
func toolUses(m Message) ([]*pyjson.Object, error) {
	content := m.Content
	if !m.HasContent {
		content = []any{}
	}
	switch c := content.(type) {
	case string:
		return nil, nil
	case []any:
		var out []*pyjson.Object
		for _, b := range c {
			if o, isObj := b.(*pyjson.Object); isObj && get(o, "type") == "tool_use" {
				out = append(out, o)
			}
		}
		return out, nil
	case *pyjson.Object:
		return nil, nil // iterating a dict yields its keys: never a dict
	}
	return nil, &ParseError{Msg: fmt.Sprintf("'%s' object is not iterable", pyTypeName(content))}
}

func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case pyjson.Int:
		return "int"
	case float64, pyjson.Float:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	}
	return "dict"
}

var toolTypes = map[string]string{
	"Edit": "file_write", "Write": "file_write", "Read": "file_read", "Bash": "shell",
	"Glob": "file_read", "Grep": "file_read", "WebFetch": "network", "WebSearch": "network",
}

// pyStr is str(v) for a value json.loads makes.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pmodel.Repr(v)
}

// hashable reports the TypeError a set or dict lookup of v raises: a list
// or a dict is unhashable.
func unhashable(v any) error {
	switch v.(type) {
	case []any, *pyjson.Object:
		return &ParseError{Msg: fmt.Sprintf("unhashable type: '%s'", pyTypeName(v))}
	}
	return nil
}

// noAttr is the AttributeError of v.<attr> on a value that has none.
func noAttr(v any, attr string) error {
	return &ParseError{Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyTypeName(v), attr)}
}

// extractStep is _extract_step: nil for a tool that is not work. Values
// are kept as the transcript holds them; _finalize validates them.
func extractStep(tu *pyjson.Object) (*step, error) {
	nameV, has := tu.Get("name")
	if !has {
		nameV = ""
	}
	inpV := or2(get(tu, "input"), pyjson.NewObject())
	inp, inpObj := inpV.(*pyjson.Object)
	if err := unhashable(nameV); err != nil {
		return nil, err
	}
	name, isStr := nameV.(string)
	t := ""
	if isStr {
		t = toolTypes[name]
	}
	if t == "network" {
		if !inpObj {
			return nil, noAttr(inpV, "get")
		}
		return &step{typ: "network", url: get(inp, "url"), query: get(inp, "query"), prevIndex: -1}, nil
	}
	if t != "" {
		if !inpObj {
			return nil, noAttr(inpV, "get")
		}
		path := or2(or2(get(inp, "file_path"), get(inp, "path")), get(inp, "pattern"))
		cmd := or2(get(inp, "command"), get(inp, "query"))
		return &step{typ: t, path: path, command: cmd, prevIndex: -1}, nil
	}
	switch {
	case isStr && (name == "Agent" || name == "Task"):
		if !inpObj {
			return nil, noAttr(inpV, "get")
		}
		return &step{typ: "task", prompt: or2(get(inp, "prompt"), ""), prevIndex: -1}, nil
	case isStr && name == "Skill":
		if !inpObj {
			return nil, noAttr(inpV, "get")
		}
		args := get(inp, "args")
		var si *pyjson.Object
		switch a := args.(type) {
		case *pyjson.Object:
			si = a
		default:
			si = pyjson.NewObject()
			if truthy(args) {
				si.Set("args", args)
			}
		}
		id := or2(or2(get(inp, "skill"), get(inp, "command")), "")
		return &step{typ: "skill", skillID: id, skillInput: si, prevIndex: -1}, nil
	case !isStr:
		return nil, noAttr(nameV, "startswith")
	case strings.HasPrefix(name, "mcp__"):
		server, tool, _ := strings.Cut(name[len("mcp__"):], "__")
		args := pyjson.NewObject()
		if inpObj {
			args = inp
		}
		return &step{typ: "mcp", server: server, tool: tool, arguments: args, prevIndex: -1}, nil
	}
	return nil, nil
}

// splitCompound is _split_compound_shell on a command of any JSON type:
// a str is split as the oracle splits it; a falsy value gives no parts;
// a list is walked item by item, as Python walks it.
func splitCompound(v any) ([]string, error) {
	if !truthy(v) {
		return nil, nil
	}
	switch c := v.(type) {
	case string:
		return verify.SplitCompoundShell(c), nil
	case []any:
		return splitItems(c)
	case *pyjson.Object:
		// command[0] on a dict: KeyError(0).
		return nil, &ParseError{Msg: "0"}
	}
	return nil, &ParseError{Msg: fmt.Sprintf("object of type '%s' has no len()", pyTypeName(v))}
}

// splitItems is _split_compound_shell over a list: each item stands where
// a character would, and "".join raises on an item that is not a str.
func splitItems(items []any) ([]string, error) {
	join := func(cur []any) (string, error) {
		var b strings.Builder
		for k, c := range cur {
			s, ok := c.(string)
			if !ok {
				return "", &ParseError{Msg: fmt.Sprintf("sequence item %d: expected str instance, %s found", k, pyTypeName(c))}
			}
			b.WriteString(s)
		}
		return b.String(), nil
	}
	var parts []string
	var cur []any
	inS, inD := false, false
	for i := 0; i < len(items); {
		c := items[i]
		if c == "'" && !inD {
			inS = !inS
			cur = append(cur, c)
			i++
			continue
		}
		if c == `"` && !inS {
			inD = !inD
			cur = append(cur, c)
			i++
			continue
		}
		if !inS && !inD {
			step := 0
			switch {
			case c == ";":
				step = 1
			case c == "&" && i+1 < len(items) && items[i+1] == "&":
				step = 2
			case c == "|" && i+1 < len(items) && items[i+1] == "|":
				step = 2
			}
			if step > 0 {
				s, err := join(cur)
				if err != nil {
					return nil, err
				}
				parts = append(parts, pystr.Strip(s))
				cur = nil
				i += step
				continue
			}
		}
		cur = append(cur, c)
		i++
	}
	tail, err := join(cur)
	if err != nil {
		return nil, err
	}
	if t := pystr.Strip(tail); t != "" {
		parts = append(parts, t)
	}
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// extractSteps is _extract_step_maybe_multiple.
func extractSteps(tu *pyjson.Object) ([]step, error) {
	if name, _ := get(tu, "name").(string); name == "Bash" {
		inpV, has := tu.Get("input")
		if !has {
			inpV = pyjson.NewObject()
		}
		inp, isObj := inpV.(*pyjson.Object)
		if !isObj {
			return nil, noAttr(inpV, "get")
		}
		cmdV, has := inp.Get("command")
		if !has {
			cmdV = ""
		}
		parts, err := splitCompound(cmdV)
		if err != nil {
			return nil, err
		}
		if len(parts) > 1 {
			out := make([]step, len(parts))
			for i, p := range parts {
				out[i] = step{typ: "shell", command: p, prevHint: true, prevIndex: -1}
			}
			return out, nil
		}
	}
	s, err := extractStep(tu)
	if err != nil || s == nil {
		return nil, err
	}
	return []step{*s}, nil
}

// Identify is _identify_episodes: episodes cut at real user turns.
func Identify(messages []Message) ([]*RawEpisode, error) {
	var eps []*RawEpisode
	var cur *RawEpisode
	for idx, m := range messages {
		task, isUser, err := userText(m)
		if err != nil {
			return nil, err
		}
		if isUser {
			if cur != nil {
				cur.LastMessage = idx - 1
			}
			cur = &RawEpisode{Task: task, FirstMessage: idx, LastMessage: idx}
			eps = append(eps, cur)
			continue
		}
		if cur == nil {
			continue
		}
		cur.LastMessage = idx
		if m.Role != "assistant" {
			continue
		}
		tus, err := toolUses(m)
		if err != nil {
			return nil, err
		}
		for _, tu := range tus {
			subs, err := extractSteps(tu)
			if err != nil {
				return nil, err
			}
			prev := -1
			for _, s := range subs {
				if s.prevHint && prev >= 0 {
					s.prevIndex = prev
				}
				s.prevHint = false
				cur.steps = append(cur.steps, s)
				prev = len(cur.steps) - 1
			}
		}
	}
	return eps, nil
}

// MergeSmall is _merge_small.
func MergeSmall(eps []*RawEpisode, minTools int64) []*RawEpisode {
	if len(eps) == 0 {
		return nil
	}
	out := []*RawEpisode{eps[0]}
	for _, ep := range eps[1:] {
		if int64(len(ep.steps)) < minTools {
			last := out[len(out)-1]
			last.steps = append(last.steps, ep.steps...)
			last.LastMessage = ep.LastMessage
		} else {
			out = append(out, ep)
		}
	}
	return out
}

// SplitContent is the user content _llm_split sends for one episode.
func SplitContent(ep *RawEpisode) string {
	lines := make([]string, len(ep.steps))
	for i, s := range ep.steps {
		label := ""
		if v := or2(s.path, s.command); truthy(v) {
			label = pyStr(v)
		}
		lines[i] = fmt.Sprintf("%d: %s %s", i, s.typ, label)
	}
	return fmt.Sprintf("User message: %s\n\nTool calls (%d total):\n%s", pyStr(ep.Task), len(ep.steps), strings.Join(lines, "\n"))
}

// Split is _split_large: each episode over maxTools is cut where split
// says. split returns the boundaries (the "subtasks" value), or nil on a
// failed call.
func Split(eps []*RawEpisode, maxTools int64, split func(content string) (any, error)) ([]*RawEpisode, error) {
	var out []*RawEpisode
	for _, ep := range eps {
		if int64(len(ep.steps)) <= maxTools {
			out = append(out, ep)
			continue
		}
		b, err := split(SplitContent(ep))
		if err != nil {
			return nil, err
		}
		subs, ok := validBoundaries(b, len(ep.steps))
		if !ok {
			out = append(out, ep)
			continue
		}
		for _, sub := range subs {
			task, has := sub.Get("task")
			if !has {
				return nil, &ParseError{Msg: "'task'"}
			}
			start, _ := intOf(get(sub, "start_index"))
			end, _ := intOf(get(sub, "end_index"))
			lo, hi := pySliceBounds(len(ep.steps), start, end+1)
			out = append(out, &RawEpisode{Task: task, FirstMessage: ep.FirstMessage, LastMessage: ep.LastMessage,
				steps: append([]step{}, ep.steps[lo:hi]...), StepStart: get(sub, "start_index"),
				StepEnd: get(sub, "end_index")})
		}
	}
	return out, nil
}

// ParseError is an exception the parse raises: the CLI prints
// "Parse error: <Msg>".
type ParseError struct{ Msg string }

func (e *ParseError) Error() string { return e.Msg }

// intOf is an int value, bool included (bool is an int in Python).
func intOf(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case pyjson.Int:
		var n int
		if _, err := fmt.Sscan(x.Text, &n); err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// pySliceBounds is the [lo:hi) that seq[a:b] takes of n items.
func pySliceBounds(n, a, b int) (int, int) {
	norm := func(i int) int {
		if i < 0 {
			i += n
			if i < 0 {
				i = 0
			}
		}
		if i > n {
			i = n
		}
		return i
	}
	lo, hi := norm(a), norm(b)
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// validBoundaries is _validate_boundaries: the boundaries, in their own
// order, when sorted by start they cover [0, n) with no gap or overlap.
func validBoundaries(b any, n int) ([]*pyjson.Object, bool) {
	if !truthy(b) {
		return nil, false
	}
	list, isList := b.([]any)
	if !isList {
		// A dict iterates its keys and a str its characters: indexing a
		// str by "start_index" raises TypeError, which is caught.
		return nil, false
	}
	subs := make([]*pyjson.Object, len(list))
	starts := make([]int, len(list))
	for i, x := range list {
		o, isObj := x.(*pyjson.Object)
		if !isObj {
			return nil, false
		}
		sv, has := o.Get("start_index")
		if !has {
			return nil, false
		}
		s, ok := intOf(sv)
		if !ok {
			// Sorting on keys of mixed types raises TypeError, and a
			// single non-int start fails the isinstance check.
			return nil, false
		}
		subs[i], starts[i] = o, s
	}
	order := make([]int, len(subs))
	for i := range order {
		order[i] = i
	}
	// sorted() is stable.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && starts[order[j]] < starts[order[j-1]]; j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	first := starts[order[0]]
	last, ok := intOf(get(subs[order[len(order)-1]], "end_index"))
	if !ok || first != 0 || last != n-1 {
		return nil, false
	}
	for k := 1; k < len(order); k++ {
		prevEnd, ok := intOf(get(subs[order[k-1]], "end_index"))
		if !ok || starts[order[k]] != prevEnd+1 {
			return nil, false
		}
	}
	// Each sub is sliced by its end_index too, which must be an int.
	for _, s := range subs {
		if _, ok := intOf(get(s, "end_index")); !ok {
			return nil, false
		}
	}
	return subs, true
}

// quote is urllib.parse.quote(s) with the default safe "/".
func quote(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// networkURL is _network_url.
func networkURL(s step) string {
	if truthy(s.url) {
		return pyStr(s.url)
	}
	if truthy(s.query) {
		return "https://web-search.invalid/?q=" + quote(pyStr(s.query))
	}
	return ""
}

// jsonMode is a value as model_dump(mode="json") writes it: a float that
// is nan or an infinity becomes None.
func jsonMode(v any) any {
	switch x := v.(type) {
	case pyjson.Float:
		if f := float64(x); math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, jsonMode(x.Value(k)))
		}
		return out
	}
	return v
}

// build is one step model constructed from keyword values, as _finalize
// constructs it: pydantic's error when a value does not validate.
func build(tag string, kw *pyjson.Object) error {
	if _, verr := pmodel.StepTypes[tag].ValidateObject(kw, pmodel.Python); verr != nil {
		return &ParseError{Msg: verr.String()}
	}
	return nil
}

// Finalize is _finalize: the episodes as Episode dumps (mode="json",
// exclude_none), with sequential step ids across the whole transcript. A
// value a step or an episode does not take is pydantic's error.
func Finalize(eps []*RawEpisode) ([]any, error) {
	out := make([]any, 0, len(eps))
	counter := 0
	for i, ep := range eps {
		idOf := map[int]string{}
		steps := make([]any, 0, len(ep.steps))
		for li, s := range ep.steps {
			id := fmt.Sprintf("s%d", counter)
			idOf[li] = id
			deps := []any{}
			if s.prevIndex >= 0 {
				if d, ok := idOf[s.prevIndex]; ok {
					deps = []any{d}
				}
			}
			kw := pyjson.NewObject().Set("id", id)
			tag := s.typ
			switch s.typ {
			case "shell":
				kw.Set("command", or2(s.command, ""))
			case "file_read":
				kw.Set("path", or2(s.path, ""))
			case "file_write":
				kw.Set("path", or2(s.path, "")).Set("content", "")
			case "network":
				kw.Set("url", networkURL(s))
			case "task":
				kw.Set("prompt", or2(s.prompt, ""))
			case "skill":
				kw.Set("skill_id", or2(s.skillID, "")).Set("skill_input", or2(s.skillInput, pyjson.NewObject()))
			case "mcp":
				kw.Set("server", s.server).Set("tool", s.tool).Set("arguments", or2(s.arguments, pyjson.NewObject()))
			default:
				tag = "shell"
				kw.Set("command", "")
			}
			kw.Set("depends_on", deps)
			if err := build(tag, kw); err != nil {
				return nil, err
			}
			o := pyjson.NewObject().Set("id", id).Set("depends_on", deps).Set("metadata", pyjson.NewObject()).Set("type", tag)
			for _, k := range kw.Keys() {
				if k != "id" && k != "depends_on" {
					o.Set(k, kw.Value(k))
				}
			}
			if tag == "network" {
				o.Set("method", "GET").Set("headers", pyjson.NewObject())
			}
			steps = append(steps, jsonMode(o))
			counter++
		}
		rng := pyjson.NewObject().Set("first_message", pyjson.Int{Text: fmt.Sprint(ep.FirstMessage)}).
			Set("last_message", pyjson.Int{Text: fmt.Sprint(ep.LastMessage)})
		if ep.StepStart != nil {
			rng.Set("step_start", ep.StepStart).Set("step_end", ep.StepEnd)
		}
		epID := fmt.Sprintf("ep_%02d", i)
		kw := pyjson.NewObject().Set("id", epID).Set("task", ep.Task).Set("steps", []any{}).Set("source_range", rng)
		if _, verr := Episode.ValidateObject(kw, pmodel.Python); verr != nil {
			return nil, &ParseError{Msg: verr.String()}
		}
		out = append(out, pyjson.NewObject().Set("id", epID).Set("task", ep.Task).
			Set("steps", steps).Set("source_range", rng))
	}
	return out, nil
}

package transcript

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// openCodeToClaude is parsers.opencode._OPENCODE_TO_CLAUDE.
var openCodeToClaude = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit", "glob": "Glob",
	"grep": "Grep", "webfetch": "WebFetch", "websearch": "WebSearch",
}

// jsonNames is _json_names: the sorted names of the .json files in a
// directory, or none when it is not a directory.
func jsonNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !st.Mode().IsRegular() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// loadObject is _load_object: the file's JSON object, or nil for a file
// that cannot be read, is not UTF-8, is not JSON or is not an object.
func loadObject(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	text, dec := pystr.DecodeStream(raw)
	if dec != nil {
		return nil, nil
	}
	v, ok, err := loadLine(text)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			return nil, nil // a ValueError the oracle catches
		}
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	o, _ := v.(*pyjson.Object)
	return o, nil
}

// intText reads a JSON integer's text as an int64.
func intText(text string) (int64, bool) {
	n, err := strconv.ParseInt(text, 10, 64)
	return n, err == nil
}

// created is _created: time.created as a sort key, 0 when unusable.
func created(msg *pyjson.Object) float64 {
	t, isObj := get(msg, "time").(*pyjson.Object)
	if !isObj {
		return 0
	}
	switch c := get(t, "created").(type) {
	case pyjson.Int:
		n, ok := intText(c.Text)
		if ok && math.Abs(float64(n)) <= 1<<53 {
			return float64(n)
		}
	case pyjson.Float:
		f := float64(c)
		if !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
	return 0
}

// OpenCodeMessages is OpenCodeParser._read_messages over the session file
// at path.
func OpenCodeMessages(path string) ([]Message, error) {
	storage := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	sid := filepath.Base(path)
	sid = strings.TrimSuffix(sid, ".json")
	msgDir := filepath.Join(storage, "message", sid)
	names := jsonNames(msgDir)
	if len(names) == 0 {
		return nil, &ParseError{Msg: "no OpenCode messages for session " + sid}
	}
	type loaded struct {
		at   float64
		name string
		msg  *pyjson.Object
	}
	var all []loaded
	for _, name := range names {
		d, err := loadObject(filepath.Join(msgDir, name))
		if err != nil {
			return nil, err
		}
		if d != nil {
			all = append(all, loaded{created(d), name, d})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].at != all[j].at {
			return all[i].at < all[j].at
		}
		return all[i].name < all[j].name
	})
	var msgs []Message
	for _, l := range all {
		// The layout names the message file by its id, so the file stem is the
		// id. The id inside the file is third-party data and never a path.
		mid := strings.TrimSuffix(l.name, ".json")
		partDir := filepath.Join(storage, "part", mid)
		var parts []*pyjson.Object
		partNames := jsonNames(partDir)
		if mid == "" || mid == "." || mid == ".." {
			partNames = nil
		}
		for _, name := range partNames {
			d, err := loadObject(filepath.Join(partDir, name))
			if err != nil {
				return nil, err
			}
			if d != nil {
				parts = append(parts, d)
			}
		}
		switch get(l.msg, "role") {
		case "user":
			var texts []string
			for _, p := range parts {
				if get(p, "type") == "text" {
					if t, isStr := get(p, "text").(string); isStr {
						texts = append(texts, t)
					}
				}
			}
			if text := pystr.Strip(strings.Join(texts, "\n")); text != "" {
				msgs = append(msgs, Message{Role: "user", Content: text, HasContent: true})
			}
		case "assistant":
			var blocks []any
			for _, p := range parts {
				if b := openCodeToolBlock(p); b != nil {
					blocks = append(blocks, b)
				}
			}
			if len(blocks) > 0 {
				msgs = append(msgs, Message{Role: "assistant", Content: blocks, HasContent: true})
			}
		}
	}
	return msgs, nil
}

// openCodeToolBlock is OpenCodeParser._tool_block.
func openCodeToolBlock(part *pyjson.Object) *pyjson.Object {
	if get(part, "type") != "tool" {
		return nil
	}
	tool, isStr := get(part, "tool").(string)
	if !isStr {
		return nil
	}
	state, isObj := get(part, "state").(*pyjson.Object)
	if !isObj {
		return nil
	}
	if s := get(state, "status"); s == "pending" || s == "error" {
		return nil
	}
	input := pyjson.NewObject()
	if raw, isObj := get(state, "input").(*pyjson.Object); isObj {
		for _, k := range raw.Keys() {
			input.Set(k, raw.Value(k))
		}
	}
	if fp, isStr := get(input, "filePath").(string); isStr {
		if _, has := input.Get("file_path"); !has {
			input.Set("file_path", fp)
		}
	}
	mapped, known := openCodeToClaude[tool]
	if !known {
		mapped = "mcp__opencode__" + tool
	}
	return toolUse(mapped, input)
}

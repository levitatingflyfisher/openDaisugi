package config

// Found is one harness Discover found on PATH.
type Found struct {
	Name, Path, State string
}

// probe is one harness coppice knows how to own, in the order Discover
// looks for it. State names how coppice learns the harness state.
type probe struct {
	name, state string
}

// resumeArgs are the resume_args a first-run table gets, for each harness
// whose resume flag is known. claude --resume takes a session id.
var resumeArgs = map[string][]string{
	"claude": {"--resume", "{session}"},
}

// probes is the probe order. The names match adapters.Names, so one name
// works for a pty pane and a headless one.
var probes = []probe{
	{"claude", "hooks"},
	{"codex", "hooks"},
	{"pi", "rpc"},
	{"sprig", "hooks"},
	{"opencode", "sse"},
}

// appOnlyNote is what Discover teaches when PATH holds no harness.
const appOnlyNote = "No harness on PATH. Codex Desktop, Cursor, and Antigravity are apps, " +
	"and coppice cannot own their panes. Install claude, codex, pi, sprig, or opencode."

// Discover looks up each known harness with lookPath and returns the ones
// it finds, in probe order.
func Discover(lookPath func(string) (string, error)) []Found {
	var found []Found
	for _, p := range probes {
		path, err := lookPath(p.name)
		if err != nil || path == "" {
			continue
		}
		found = append(found, Found{Name: p.name, Path: path, State: p.state})
	}
	return found
}

// AppOnlyNote is the line to print when found is empty. It is empty when
// found holds at least one harness.
func AppOnlyNote(found []Found) string {
	if len(found) > 0 {
		return ""
	}
	return appOnlyNote
}

// FromFound builds a Config with one harness table per found harness and
// def as the default.
func FromFound(found []Found, def string) Config {
	c := Config{Default: def, Harness: map[string]Harness{}}
	for _, f := range found {
		h := Harness{Command: f.Name, State: f.State}
		if ra := resumeArgs[f.Name]; ra != nil {
			h.ResumeArgs = append([]string{}, ra...)
		}
		c.Harness[f.Name] = h
	}
	return c
}

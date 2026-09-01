// Package plugins loads coppice plugins. A plugin is a directory with a
// manifest.json. A view is a page the web server serves in a sandboxed
// frame. It holds nothing and is given data by the floor page. A policy is
// a process the server starts, which listens to events and holds only the
// verbs its manifest names. No plugin may hold agent.allow or agent.deny.
package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// Refusal is the message a manifest gets when it asks for an allow verb.
const Refusal = "a plugin can propose. It cannot allow."

// ReservedPrefix starts ids that only the shipped plugins may use.
const ReservedPrefix = "coppice."

// Kinds of plugin.
const (
	KindView   = "view"
	KindPolicy = "policy"
)

// Manifest is one manifest.json.
type Manifest struct {
	ID string `json:"id"`
	// Kind is view or policy.
	Kind string `json:"kind"`
	// Page is the view's html file, relative to the plugin directory.
	Page string `json:"page"`
	// Run is the policy's program, relative to the plugin directory.
	Run string `json:"run"`
	// Listens are the event kinds a policy may subscribe to.
	Listens []string `json:"listens"`
	// Needs are the verbs a policy may run. Never agent.allow or agent.deny.
	Needs []string `json:"needs"`
	Title string   `json:"title"`
	// About says in one line what the plugin does.
	About string `json:"about"`
	// Ring asks the floor page to post the view the web server's ring of
	// recent state events. Only a view may ask. A view that draws no
	// history leaves it off, so the floor does not post it the ring.
	Ring bool `json:"ring"`
	// Config is the plugin's own settings. The runner hands them to a
	// policy, with the operator's table from coppice.toml laid over them.
	Config map[string]any `json:"config"`
}

// Problem is one plugin that did not load, and why.
type Problem struct {
	ID      string
	Dir     string
	Message string
}

func (p Problem) String() string {
	if p.Dir != "" {
		return fmt.Sprintf("plugin %s in %s: %s", p.ID, p.Dir, p.Message)
	}
	return fmt.Sprintf("plugin %s: %s", p.ID, p.Message)
}

var (
	idRe   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	verbRe = regexp.MustCompile(`^[a-z][a-z_]*(\.[a-z][a-z_]*)+$`)
)

// allowVerbs are the verbs no plugin may hold.
var allowVerbs = map[string]bool{"agent.allow": true, "agent.deny": true}

// Listenable are the event kinds a policy may subscribe to. Frames are
// left out: a screen is read through pane.read, which a manifest must ask
// for by name.
var Listenable = map[string]bool{"state": true, "layout": true, "note": true, "child": true}

// parseManifest reads and checks raw as the manifest of the directory
// named dirName. shipped says whether the directory is one the binary
// carries. files is the plugin directory, to check that page and run
// exist.
func parseManifest(raw []byte, dirName string, shipped bool, files fs.FS) (Manifest, string) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, "manifest.json does not parse: " + err.Error()
	}
	if strings.HasPrefix(m.ID, ReservedPrefix) || strings.HasPrefix(dirName, ReservedPrefix) {
		if !shipped {
			return m, "the id prefix " + ReservedPrefix + " is reserved for the plugins coppice ships."
		}
	}
	if !idRe.MatchString(strings.TrimPrefix(m.ID, ReservedPrefix)) {
		return m, "id must be lower case letters, digits and hyphens, and start with a letter."
	}
	if m.ID != dirName {
		return m, fmt.Sprintf("id %s does not match its directory %s.", m.ID, dirName)
	}
	for _, v := range m.Needs {
		if allowVerbs[v] {
			return m, Refusal
		}
	}
	for _, v := range m.Needs {
		if v == "pane.report_state" || v == "pane.report_child" {
			return m, "a plugin reports no state. Only a pane reports its own."
		}
		if !verbRe.MatchString(v) {
			return m, fmt.Sprintf("needs has %q, which is not a verb name.", v)
		}
	}
	for _, k := range m.Listens {
		if !Listenable[k] {
			return m, fmt.Sprintf("listens has %q. A policy may listen to state, layout, note or child.", k)
		}
	}
	switch m.Kind {
	case KindView:
		if m.Page == "" {
			return m, "a view needs page, the html file it serves."
		}
		if m.Run != "" || len(m.Needs) > 0 || len(m.Listens) > 0 {
			return m, "a view holds no verbs and runs nothing. It reads through the floor page's API."
		}
		return m, checkFile(files, m.Page)
	case KindPolicy:
		if m.Ring {
			return m, "only a view reads the ring. A policy listens to events."
		}
		if m.Run == "" {
			return m, "a policy needs run, the program the server starts."
		}
		if m.Page != "" {
			return m, "a policy has no page. Make a view for that."
		}
		return m, checkFile(files, m.Run)
	}
	return m, "kind must be view or policy."
}

// checkFile refuses a name that leaves the plugin directory or names no
// file in it.
func checkFile(files fs.FS, name string) string {
	if path.IsAbs(name) || !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return fmt.Sprintf("%s must name a file inside the plugin directory.", name)
	}
	st, err := fs.Stat(files, name)
	if err != nil || st.IsDir() {
		return fmt.Sprintf("there is no file %s in the plugin directory.", name)
	}
	return ""
}

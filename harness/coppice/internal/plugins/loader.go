package plugins

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Plugin is one loaded plugin.
type Plugin struct {
	Manifest
	// Dir is the plugin directory on disk. It is empty for a plugin the
	// binary carries.
	Dir string
	// FS holds the plugin's files. For a directory on disk it cannot
	// follow a link out of the directory.
	FS fs.FS
	// Shipped is true for a plugin the binary carries.
	Shipped bool
}

// Source is one place plugins live: a root directory on disk whose
// subdirectories are plugins, or a file system that the binary carries.
type Source struct {
	Dir     string
	FS      fs.FS
	Shipped bool
}

// Load reads the enabled plugins from the root directories dirs, each of
// which holds one subdirectory per plugin. It never panics. Every plugin
// that does not load is one Problem, and the rest load.
func Load(dirs []string, enabled []string) ([]Plugin, []Problem) {
	srcs := make([]Source, 0, len(dirs))
	for _, d := range dirs {
		srcs = append(srcs, Source{Dir: d})
	}
	return LoadFrom(srcs, enabled)
}

// LoadFrom reads the enabled plugins from srcs. The first source that has
// an id wins. A directory of the operator's that takes a shipped id is a
// problem, and the shipped plugin loads. Plugins come back in the enabled
// order, each id once.
func LoadFrom(srcs []Source, enabled []string) ([]Plugin, []Problem) {
	var out []Plugin
	var probs []Problem
	// A directory on disk is opened once as a root, so no file a plugin
	// names can reach past it through a link.
	opened := make([]Source, 0, len(srcs))
	for _, src := range srcs {
		if src.Dir != "" {
			r, err := os.OpenRoot(src.Dir)
			if err != nil {
				continue
			}
			src.FS = r.FS()
		}
		opened = append(opened, src)
	}
	srcs = opened
	seen := map[string]bool{}
	for _, id := range enabled {
		if seen[id] {
			continue
		}
		seen[id] = true
		var found *Plugin
		var prob *Problem
		for _, src := range srcs {
			p, pr := loadOne(src, id)
			if p == nil && pr == nil {
				continue
			}
			if found != nil && found.Shipped && !src.Shipped {
				dir := ""
				if src.Dir != "" {
					dir = filepath.Join(src.Dir, id)
				}
				probs = append(probs, Problem{ID: id, Dir: dir, Message: "it takes the id of the shipped plugin " + id +
					". A directory of yours may not replace it. Give it another id."})
				continue
			}
			if found == nil && prob == nil {
				found, prob = p, pr
			}
		}
		switch {
		case prob != nil:
			probs = append(probs, *prob)
		case found != nil:
			out = append(out, *found)
		default:
			probs = append(probs, Problem{ID: id, Message: "it is enabled, but no plugin directory has that id."})
		}
	}
	return out, probs
}

// loadOne reads plugin id from one source. Both results are nil when the
// source has no directory for id.
func loadOne(src Source, id string) (*Plugin, *Problem) {
	if !fs.ValidPath(id) || id == "." || filepath.Base(id) != id {
		return nil, &Problem{ID: id, Message: "id must be lower case letters, digits and hyphens, and start with a letter."}
	}
	root := src.FS
	dir := ""
	if src.Dir != "" {
		dir = filepath.Join(src.Dir, id)
	}
	if root == nil {
		return nil, nil
	}
	raw, err := fs.ReadFile(root, id+"/manifest.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Problem{ID: id, Dir: dir, Message: "cannot read manifest.json: " + err.Error()}
	}
	files, err := fs.Sub(root, id)
	if err != nil {
		return nil, &Problem{ID: id, Dir: dir, Message: err.Error()}
	}
	m, msg := parseManifest(raw, id, src.Shipped, files)
	if msg != "" {
		return nil, &Problem{ID: id, Dir: dir, Message: msg}
	}
	return &Plugin{Manifest: m, Dir: dir, FS: files, Shipped: src.Shipped}, nil
}

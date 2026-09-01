package detect

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed manifests/*.toml
var bundled embed.FS

// Set is every manifest coppice knows, bundled plus local overrides. There is
// no network path: Herdr's remote update is not ported, so a manifest can only
// change when coppice is updated or the operator edits an override file.
type Set struct {
	mu       sync.RWMutex
	byID     map[string]*Compiled
	aliases  map[string]string
	sources  map[string]string
	warnings []string
}

// OverrideDir is where an operator's own manifests live. A file there
// replaces the bundled manifest with the same id -- the id read from the
// override file's own `id` field, not the override's filename. A file named
// anything at all still overrides the right bundled entry as long as its
// `id` matches; the filename only matters for the .toml suffix check below.
func OverrideDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "coppice", "agent-detection")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "coppice", "agent-detection")
}

// LoadSet reads the bundled manifests then the overrides. A manifest that fails
// to load is skipped with a warning rather than taking the whole set down: one
// bad override must not blind the floor to twenty working agents.
func LoadSet(overrideDir string) (*Set, error) {
	s := &Set{
		byID: map[string]*Compiled{}, aliases: map[string]string{},
		sources: map[string]string{},
	}
	entries, err := bundled.ReadDir("manifests")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		data, err := bundled.ReadFile("manifests/" + e.Name())
		if err != nil {
			return nil, err
		}
		s.add(e.Name(), "bundled", data)
	}
	if overrideDir != "" {
		files, err := os.ReadDir(overrideDir)
		switch {
		case err == nil:
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
					continue
				}
				p := filepath.Join(overrideDir, f.Name())
				data, err := os.ReadFile(p)
				if err != nil {
					s.warn("cannot read %s: %v", p, err)
					continue
				}
				s.add(f.Name(), p, data)
			}
		case os.IsNotExist(err):
			// No override directory configured (the common case): nothing
			// to warn about, this is not a problem.
		default:
			// The directory exists but couldn't be listed -- permission
			// denied is the usual shape. Fail closed with a named warning
			// rather than silently running with only the bundled set and no
			// explanation why an operator's overrides never took effect.
			s.warn("cannot read override directory %s: %v", overrideDir, err)
		}
	}
	return s, nil
}

// warn appends a warning under the write lock, so every mutation to a *Set
// (this and add, below) goes through the same synchronization as the reads
// in Agents/For/Source/Warnings, rather than relying on the fact that today
// nothing calls either concurrently with LoadSet.
func (s *Set) warn(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warnings = append(s.warnings, fmt.Sprintf(format, args...))
}

func (s *Set) add(name, source string, data []byte) {
	m, c, err := Parse(name, data)
	if err != nil {
		s.warn("%s did not load: %v. Fix it or delete it.", source, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[m.ID] = c
	s.sources[m.ID] = source
	for _, a := range m.Aliases {
		s.aliases[a] = m.ID
	}
}

func (s *Set) Agents() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.byID))
	for id := range s.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// For resolves an agent name or alias to its compiled manifest.
func (s *Set) For(agent string) (*Compiled, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.byID[agent]; ok {
		return c, true
	}
	if id, ok := s.aliases[agent]; ok {
		c, ok := s.byID[id]
		return c, ok
	}
	return nil, false
}

// Source names where an agent's compiled manifest came from: "bundled", or the
// override file path that replaced it.
func (s *Set) Source(agent string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[agent]
}

// Warnings is every non-fatal problem LoadSet met along the way: a bundled or
// override manifest that failed to parse, or an override file that could not
// be read. The Set still loads everything that did parse; server.status
// is what surfaces these to an operator.
func (s *Set) Warnings() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.warnings...)
}

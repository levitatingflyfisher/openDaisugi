// Package pack is opendaisugi.pack: the ML packs (a pinned CPython, a
// venv, the wheels of a hashed lock) under DATA/packs/NAME, and the
// caller side of the worker protocol daisugi-pack-1, which
// src/opendaisugi/pack/worker.py defines.
package pack

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The catalog, the locks and the worker files, copied from packs/ and
// src/opendaisugi (a test checks the copies).
//
//go:embed assets
var assets embed.FS

// CatalogEnv names another catalog.json; the command line reads it.
const CatalogEnv = "OPENDAISUGI_PACK_CATALOG"

// Protocol is worker.PROTOCOL.
const Protocol = "daisugi-pack-1"

// WorkerFile is the worker's name in a pack.
const WorkerFile = "daisugi_pack_worker.py"

// WorkerPy is worker.py.
var WorkerPy = mustAsset("worker.py")

func mustAsset(name string) string {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Python is the catalog's pinned CPython.
type Python struct {
	Version string `json:"version"`
	File    string `json:"file"`
	URL     string `json:"url"`
	Sha256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// Pack is one catalog entry.
type Pack struct {
	Name           string   `json:"name"`
	GPU            bool     `json:"gpu"`
	Summary        string   `json:"summary"`
	Lock           string   `json:"lock"`
	IndexURL       string   `json:"index_url"`
	ExtraIndexURLs []string `json:"extra_index_urls"`
	Check          []string `json:"check"`
}

// Catalog is packs/catalog.json. Dir is where its locks are; empty for
// the embedded one.
type Catalog struct {
	Protocol string `json:"protocol"`
	Platform string `json:"platform"`
	Python   Python `json:"python"`
	Packs    []Pack `json:"packs"`
	Dir      string `json:"-"`
}

// Load reads the catalog at path, or the embedded one when path is "".
func Load(path string) (*Catalog, error) {
	var raw []byte
	var err error
	if path == "" {
		raw, err = assets.ReadFile("assets/catalog.json")
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	c := &Catalog{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	if path != "" {
		c.Dir = filepath.Dir(path)
	}
	return c, nil
}

// Find is catalog.find.
func (c *Catalog) Find(name string) *Pack {
	for i := range c.Packs {
		if c.Packs[i].Name == name {
			return &c.Packs[i]
		}
	}
	return nil
}

// LockText is the pack's lock file.
func (c *Catalog) LockText(p Pack) (string, error) {
	var b []byte
	var err error
	if c.Dir == "" {
		b, err = assets.ReadFile("assets/" + p.Lock)
	} else {
		b, err = os.ReadFile(filepath.Join(c.Dir, p.Lock))
	}
	return string(b), err
}

var sepRun = regexp.MustCompile(`[-_.]+`)

// Normalize is the PEP 503 form of a project name.
func Normalize(name string) string {
	return strings.ToLower(sepRun.ReplaceAllString(name, "-"))
}

// Req is one pinned requirement.
type Req struct {
	Name, Version string
	Hashes        []string
}

func (r Req) pins(h string) bool {
	for _, x := range r.Hashes {
		if x == h {
			return true
		}
	}
	return false
}

var hashArg = regexp.MustCompile(`^--hash=sha256:([0-9a-f]{64})$`)

// ParseLock is catalog.parse_lock.
func ParseLock(text string) ([]Req, error) {
	var logical []string
	cur := ""
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " \t")
		if strings.HasSuffix(line, "\\") {
			cur += line[:len(line)-1] + " "
			continue
		}
		cur += line
		if strings.TrimSpace(cur) != "" {
			logical = append(logical, strings.TrimSpace(cur))
		}
		cur = ""
	}
	if strings.TrimSpace(cur) != "" {
		logical = append(logical, strings.TrimSpace(cur))
	}
	var out []Req
	for _, ln := range logical {
		words := strings.Fields(ln)
		head := words[0]
		if !strings.Contains(head, "==") || strings.Contains(ln, ";") {
			return nil, fmt.Errorf("not one pinned version: %s", clip(ln, 80))
		}
		name, version, _ := strings.Cut(head, "==")
		var hashes []string
		for _, w := range words[1:] {
			m := hashArg.FindStringSubmatch(w)
			if m == nil {
				return nil, fmt.Errorf("not a sha256 hash: %s", clip(w, 80))
			}
			hashes = append(hashes, m[1])
		}
		if len(hashes) == 0 || name == "" || version == "" {
			return nil, fmt.Errorf("no sha256 hash: %s", clip(ln, 80))
		}
		out = append(out, Req{Normalize(name), version, hashes})
	}
	return out, nil
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// WheelKey is catalog.wheel_key.
func WheelKey(filename string) (name, version string, ok bool) {
	if !strings.HasSuffix(filename, ".whl") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
	if len(parts) != 5 && len(parts) != 6 {
		return "", "", false
	}
	return Normalize(parts[0]), parts[1], true
}

// Package datahome is where coppice keeps its data when no flag names a
// directory. One rule, so a friend can predict it (F11, 2026-10-24). It
// never asks whether ~/.opendaisugi exists:
//
//  1. $OPENDAISUGI_HOME when it is usable (see Usable);
//  2. else $XDG_DATA_HOME/opendaisugi when XDG_DATA_HOME is usable;
//  3. else ~/.opendaisugi;
//  4. except that, when step 2 chose, an existing ~/.opendaisugi/coppice
//     that holds data is kept while the XDG coppice directory holds none.
//     The user keeps the old state, and Legacy says so in one line.
//
// Before this rule, the answer depended on whether ~/.opendaisugi
// existed, and `daisugi install --gate` creates it: the same box used two
// places, depending on the order of two commands. Both places are in the
// set the gate and the fence guard (datahome.guarded_data_dirs), so no
// ruling here leaves the foreman's log open. Step 2 needs the variable
// to be set: when XDG_DATA_HOME is unset, ~/.local/share/opendaisugi is in
// no guard set, so the default stays ~/.opendaisugi. coppice's data directory, its
// fallback socket and the foreman's directory sit under it.
package datahome

import (
	"os"
	"path/filepath"
	"strings"
)

// Usable is v with a leading "~" or "~/" replaced by home, when the result
// is an absolute path; else "". Empty, relative and "~user" values are
// ignored, as the XDG spec says of a relative XDG path.
func Usable(v, home string) string {
	switch {
	case v == "":
		return ""
	case (v == "~" || strings.HasPrefix(v, "~/")) && !filepath.IsAbs(home):
		return ""
	case v == "~":
		v = home
	case strings.HasPrefix(v, "~/"):
		v = home + v[1:]
	}
	if !filepath.IsAbs(v) {
		return ""
	}
	return filepath.Clean(v)
}

// xdgDir is $XDG_DATA_HOME/opendaisugi, or "" when XDG_DATA_HOME is unset
// or not usable.
func xdgDir(getenv func(string) string, home string) string {
	if x := Usable(getenv("XDG_DATA_HOME"), home); x != "" {
		return filepath.Join(x, "opendaisugi")
	}
	return ""
}

// Resolve applies the rule. getenv reads the environment; has says whether
// a path exists and holds data (a file, or a directory with an entry).
// legacy is true when the old ~/.opendaisugi was kept.
func Resolve(getenv func(string) string, home string, has func(string) bool) (dir string, legacy bool) {
	if d := Usable(getenv("OPENDAISUGI_HOME"), home); d != "" {
		return d, false
	}
	dot := filepath.Join(home, ".opendaisugi")
	xdg := xdgDir(getenv, home)
	if xdg == "" {
		return dot, false // XDG_DATA_HOME not set: ~/.opendaisugi, whether or not it exists
	}
	if !has(filepath.Join(xdg, "coppice")) && has(filepath.Join(dot, "coppice")) {
		return dot, true
	}
	return xdg, false
}

// Dir is Resolve without the legacy flag.
func Dir(getenv func(string) string, home string, has func(string) bool) string {
	d, _ := Resolve(getenv, home, has)
	return d
}

func processHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// Default is Dir for this process: its environment and its home
// directory.
func Default() string {
	return Dir(os.Getenv, processHome(), Has)
}

// Legacy is the one-line note for a process that keeps the old
// ~/.opendaisugi/coppice, or "" when it does not.
func Legacy() string {
	home := processHome()
	dir, legacy := Resolve(os.Getenv, home, Has)
	if !legacy {
		return ""
	}
	return "coppice: keeping the data in " + filepath.Join(dir, "coppice") +
		". New installs keep it in " + filepath.Join(xdgDir(os.Getenv, home), "coppice") + "."
}

// Has reports whether p exists and holds data: a file, or a directory
// with at least one entry. Links are followed.
func Has(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	if !st.IsDir() {
		return true
	}
	es, err := os.ReadDir(p)
	return err == nil && len(es) > 0
}

// Exists reports whether p exists, following links.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

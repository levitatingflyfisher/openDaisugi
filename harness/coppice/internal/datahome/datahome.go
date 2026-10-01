// Package datahome is where openDaisugi keeps its data when no flag names a
// directory, the rule of opendaisugi.datahome (and clients/go's datahome):
//
//  1. $OPENDAISUGI_HOME when it is usable (see Usable);
//  2. else $XDG_DATA_HOME/opendaisugi when XDG_DATA_HOME is usable and
//     ~/.opendaisugi does not exist;
//  3. else ~/.opendaisugi.
//
// An existing install keeps its directory. coppice's own data directory,
// its fallback socket and the foreman's directory sit under it.
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

// Dir applies the rule. getenv reads the environment; exists says whether
// a path exists.
func Dir(getenv func(string) string, home string, exists func(string) bool) string {
	if d := Usable(getenv("OPENDAISUGI_HOME"), home); d != "" {
		return d
	}
	dot := filepath.Join(home, ".opendaisugi")
	if x := Usable(getenv("XDG_DATA_HOME"), home); x != "" && !exists(dot) {
		return filepath.Join(x, "opendaisugi")
	}
	return dot
}

// Default is Dir for this process: its environment and its home
// directory. With no home directory, the legacy name is relative, as
// coppice's paths were before.
func Default() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return Dir(os.Getenv, home, Exists)
}

// Exists reports whether p exists, following links.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

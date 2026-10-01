// Package datahome is opendaisugi.datahome: where openDaisugi keeps its
// data when no flag names a directory.
//
//  1. $OPENDAISUGI_HOME when it is usable (see Usable);
//  2. else $XDG_DATA_HOME/opendaisugi when XDG_DATA_HOME is usable and
//     ~/.opendaisugi does not exist;
//  3. else ~/.opendaisugi.
//
// An existing install keeps its directory. The gate guards every directory
// the rule can pick (Guarded), with no exists check. Paths print as
// pathlib prints them. The package imports nothing outside the standard
// library, so every other package can use it.
package datahome

import (
	"os"
	"strings"
)

// LegacyName is the data directory's name under HOME.
const LegacyName = ".opendaisugi"

// Usable is datahome.usable: v with a leading "~" or "~/" replaced by
// home, when the result is an absolute path; else "" (empty, relative and
// "~user" values are ignored, as the XDG spec says of a relative XDG path).
func Usable(v, home string) string {
	switch {
	case v == "":
		return ""
	case v == "~":
		v = home
	case strings.HasPrefix(v, "~/"):
		v = home + v[1:]
	}
	if !strings.HasPrefix(v, "/") {
		return ""
	}
	return clean(v)
}

// Dir is data_home(env, home). getenv reads the environment, home is
// Path.home() as pathlib prints it, and exists is Path.exists.
func Dir(getenv func(string) string, home string, exists func(string) bool) string {
	h := clean(home)
	if d := Usable(getenv("OPENDAISUGI_HOME"), h); d != "" {
		return d
	}
	dot := join(h, LegacyName)
	if x := Usable(getenv("XDG_DATA_HOME"), h); x != "" && !exists(dot) {
		return join(x, "opendaisugi")
	}
	return dot
}

// Guarded is guarded_data_dirs(env, home): every directory Dir can pick,
// legacy first, without duplicates and with no exists check.
func Guarded(getenv func(string) string, home string) []string {
	h := clean(home)
	all := []string{join(h, LegacyName)}
	if d := Usable(getenv("OPENDAISUGI_HOME"), h); d != "" {
		all = append(all, d)
	}
	if x := Usable(getenv("XDG_DATA_HOME"), h); x != "" {
		all = append(all, join(x, "opendaisugi"))
	}
	var out []string
	for _, p := range all {
		dup := false
		for _, q := range out {
			dup = dup || q == p
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// Exists is Path.exists: os.Stat follows links, and any error is false.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// clean is str(PurePosixPath(p)): repeated slashes collapsed (two leading
// slashes kept), "." parts dropped, no trailing slash.
func clean(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	if out := lead + strings.Join(parts, "/"); out != "" {
		return out
	}
	return "."
}

// join is pathlib's a / b for a relative b.
func join(a, b string) string {
	switch a {
	case ".", "":
		return b
	case "/", "//":
		return a + b
	}
	return a + "/" + b
}

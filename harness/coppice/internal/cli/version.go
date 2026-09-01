package cli

import "runtime/debug"

// version is set at link time:
//
//	go build -ldflags "-X github.com/opendaisugi/coppice/internal/cli.version=0.44.0"
//
// scripts/release.sh sets the release version, and scripts/install.sh the
// git describe of the checkout. A build that sets none reports the commit
// Go stamps into it.
var version = ""

// Version returns the build's version: the one set at link time, else the
// VCS revision go build records (short, with -dirty for a tree with
// changes), else "unknown".
func Version() string {
	if version != "" {
		return version
	}
	return buildRevision()
}

func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

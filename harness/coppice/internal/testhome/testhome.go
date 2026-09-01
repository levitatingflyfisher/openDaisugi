// Package testhome keeps coppice's tests out of the operator's home. A test
// package calls Isolate from its TestMain, before any test runs: HOME and
// every XDG directory then point into one scratch directory, so a test
// server never reads, writes or dials anything of the operator's (their
// config, their gate hooks, their running server's socket).
package testhome

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/opendaisugi/coppice/internal/toolchain"
)

var realHome string

// RealHome is the home the process had before Isolate, or "" before it.
// Only a check that a test stays out of it should read it.
func RealHome() string { return realHome }

// Isolate points HOME, XDG_CONFIG_HOME, XDG_DATA_HOME, XDG_STATE_HOME,
// XDG_CACHE_HOME, XDG_BIN_HOME and XDG_RUNTIME_DIR into a new scratch
// directory, and returns it. The libghostty-vt prefix the cgo tests look
// for is found first, so moving HOME does not make them skip.
func Isolate(prefix string) (string, error) {
	realHome, _ = os.UserHomeDir()
	if os.Getenv("COPPICE_GHOSTTY_PREFIX") == "" {
		_ = os.Setenv("COPPICE_GHOSTTY_PREFIX", toolchain.GhosttyPrefix())
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	for k, sub := range map[string]string{
		"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data",
		"XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache", "XDG_BIN_HOME": "bin",
		"XDG_RUNTIME_DIR": "run",
	} {
		p := filepath.Join(dir, sub)
		if err := os.MkdirAll(p, 0o700); err != nil {
			return "", err
		}
		if err := os.Setenv(k, p); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// UnderRealHome reports whether p lies in the home the process had before
// Isolate. A path under the working directory does not count: the
// checkout itself may live in a home.
func UnderRealHome(p string) bool {
	if realHome == "" || p == "" {
		return false
	}
	p = filepath.Clean(p)
	if wd, err := os.Getwd(); err == nil {
		if top := checkoutTop(wd); top != "" && (p == top || strings.HasPrefix(p, top+string(filepath.Separator))) {
			return false
		}
	}
	h := filepath.Clean(realHome)
	return p == h || strings.HasPrefix(p, h+string(filepath.Separator))
}

// checkoutTop is the directory holding go.mod above wd, the module a test
// runs in.
func checkoutTop(wd string) string {
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

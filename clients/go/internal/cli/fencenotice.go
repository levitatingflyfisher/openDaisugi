package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// unconfinedLine is the one line shown when the warden is on and coppice's
// config turns the fence off. Only a fence stops a file access (ADR-0028).
// The Python oracle holds the same text and rule (fence_notice.py).
const unconfinedLine = "Files are not confined: the warden checks commands, but only a fence stops " +
	"a file access. Turn the fence on with fence = \"auto\" in coppice.toml."

var (
	fenceOffLine = regexp.MustCompile(`^\s*fence\s*=\s*(?:"off"|'off')\s*(?:#.*)?$`)
	tableLine    = regexp.MustCompile(`^\s*\[`)
)

// coppiceConfigPath is where coppice reads its config:
// $XDG_CONFIG_HOME/coppice/coppice.toml, else ~/.config/coppice/coppice.toml.
func (e *Env) coppiceConfigPath() string {
	base := e.env["XDG_CONFIG_HOME"]
	if base == "" {
		base = filepath.Join(e.home, ".config")
	}
	return filepath.Join(base, "coppice", "coppice.toml")
}

// configTurnsFenceOff is true when the top level of the file says
// fence = "off". A missing file, another value, or a key under a table does
// not count.
func configTurnsFenceOff(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if tableLine.MatchString(line) {
			return false
		}
		if fenceOffLine.MatchString(line) {
			return true
		}
	}
	return false
}

// wardenIsOn is true when the root holds envelopes and no DISARMED marker.
func wardenIsOn(root string) bool {
	if _, err := os.Stat(filepath.Join(root, "envelopes")); err != nil {
		return false
	}
	_, err := os.Lstat(filepath.Join(root, "DISARMED"))
	return err != nil
}

// unconfined is true when the warden at root is on and coppice's config
// turns the fence off.
func (e *Env) unconfined(root string) bool {
	return wardenIsOn(root) && configTurnsFenceOff(e.coppiceConfigPath())
}

// Package config reads and writes the one coppice config file. The file
// names the harness Enter opens and the command each harness runs. It is
// TOML, so a person can edit it by hand. The package imports nothing from
// the rest of coppice, so every other package can read it.
package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Harness is one [harness.<name>] table: the command to run, its fixed
// arguments, how coppice learns its state, and how a pty pane of this
// harness resumes.
type Harness struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	State   string   `toml:"state"`
	// ResumeArgs is appended after Args when pane.resume rebuilds a pty
	// pane's command line: command, then Args, then ResumeArgs. "{session}"
	// in any entry is replaced with the ended record's harness session id.
	// A pty pane resumes only when this is set and the record has a session
	// id; otherwise pane.resume starts it fresh. Example:
	// resume_args = ["--resume", "{session}"].
	ResumeArgs []string `toml:"resume_args"`
}

// Config is the whole file.
type Config struct {
	Default string             `toml:"default"`
	Harness map[string]Harness `toml:"harness"`
	// Plugins lists the plugin ids that run. A file with no plugins key
	// runs DefaultPlugins. plugins = [] runs none. It is a pointer so the
	// two stay apart through a Load and a Save.
	Plugins *[]string `toml:"plugins,omitempty"`
	Keys    Keys      `toml:"keys,omitempty"`
	// Plugin holds the operator's settings for each plugin, one
	// [plugin.<id>] table each. They lie over the manifest's config.
	Plugin map[string]map[string]any `toml:"plugin,omitempty"`
	// Projects is the operator's own list of pinned working directories,
	// each an absolute path. `coppice project add` and `project rm` are
	// what edit it. project.list shows these first, then the directories
	// panes have most recently started in.
	Projects []string `toml:"projects,omitempty"`
	// Gateway is the base URL of the daisugi token-saving gateway. The
	// floor compares a pane's ANTHROPIC_BASE_URL and OPENAI_BASE_URL with
	// it, and checks that it answers. With no gateway key the floor uses
	// DefaultGateway. gateway = "" says there is no gateway, and the floor
	// then shows no router at all. It is a pointer so the two stay apart.
	Gateway *string `toml:"gateway,omitempty"`
	// Voice is the [voice] table. See Voice.
	Voice Voice `toml:"voice,omitempty"`
}

// DefaultGateway is where `daisugi gateway` listens when started with no
// --host or --port, and the base URL `daisugi install --gateway` points
// a harness at.
const DefaultGateway = "http://127.0.0.1:8787"

// GatewayURL is the gateway base URL the floor compares with: the gateway
// key when the file has one, else DefaultGateway. It is empty when the
// file says gateway = "".
func (c Config) GatewayURL() string {
	if c.Gateway == nil {
		return DefaultGateway
	}
	return *c.Gateway
}

// DefaultPlugins is the plugin list for a file with no plugins key: every
// shipped view, and notify-ntfy, which does nothing until its table names
// a topic. The other policies ship off, since each one acts on panes, and
// an operator turns one on by listing it.
func DefaultPlugins() []string {
	return []string{"tree", "minimap", "kanban", "colony", "shift-log", "herdr-grid", "inbox", "notify-ntfy"}
}

// EnabledPlugins is the plugin ids that run: the plugins list when the
// file has one, else DefaultPlugins.
func (c Config) EnabledPlugins() []string {
	if c.Plugins == nil {
		return DefaultPlugins()
	}
	return append([]string{}, *c.Plugins...)
}

// header is the first line of every file Save writes.
const header = "# coppice config. Edit it and save. The next pane reads it.\n"

// Path is $XDG_CONFIG_HOME/coppice/coppice.toml, or
// ~/.config/coppice/coppice.toml when XDG_CONFIG_HOME is empty.
func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "coppice", "coppice.toml")
}

// Load reads the file at Path. ok is false and err is nil when the file
// does not exist. A file that does not parse is an error with ok false.
func Load() (Config, bool, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, false, nil
		}
		return Config{}, false, err
	}
	if err := toml.Unmarshal(b, &c); err != nil {
		return Config{}, false, err
	}
	return c, true, nil
}

// Save writes c to Path. The directory is 0700 and the file 0600. It
// writes a temp file in the same directory and renames it into place, so
// a reader never sees a half-written file.
func Save(c Config) error {
	path := Path()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var body bytes.Buffer
	body.WriteString(header)
	if err := toml.NewEncoder(&body).Encode(c); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "coppice.toml.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(body.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

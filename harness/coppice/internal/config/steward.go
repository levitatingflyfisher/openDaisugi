package config

import (
	"daisugi-verify/pando"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Steward is the [steward] table. It sets how the floor's steward runs.
//
// mode = "pane" (the default) runs the steward as a harness pane in its own
// directory, as always. mode = "chat" keeps the steward as one chat that
// never ends: each sentence the operator says is logged, and a fresh model
// call answers it. compactor_profile and compactor_model say where the
// chat's summaries are made. turn_profile is where each turn runs: empty
// (the default) or "default" is Claude Code, any other value names an
// operator agent profile (an Anthropic API key, or any OpenAI-style
// server). turn_profiles is the same as an ordered list of accounts that all
// serve ONE model (two subscriptions, or a subscription and an API key): a
// turn tries the first account that is not cooling down, and moves to the
// next when one is out of quota or cannot be reached. It never moves to
// another model (see server/steward_failover.go). With more than one entry,
// turn_model must name that model.
// turn_profile = "a" is turn_profiles = ["a"]; setting both is an error. An
// empty name in the list is Claude Code. turn_model is the model of
// every entry (empty means Claude Code's own default, or the profile's own
// model). A turn may
// take StewardTurnTimeout; that is not a setting.
type Steward struct {
	Mode             string   `toml:"mode,omitempty"`
	CompactorProfile string   `toml:"compactor_profile,omitempty"`
	CompactorModel   string   `toml:"compactor_model,omitempty"`
	TurnProfile      string   `toml:"turn_profile,omitempty"`
	TurnProfiles     []string `toml:"turn_profiles,omitempty"`
	TurnModel        string   `toml:"turn_model,omitempty"`
	// Store names the Pando R2 store the steward keeps its chat in:
	// <data home>/pando/<store>. work is the default; home is the other
	// named store. all is refused: no command reads two stores.
	Store string `toml:"store,omitempty"`
	// Dir is the steward's working dir. Empty means the default
	// ($XDG_STATE_HOME/coppice-steward). It must be an absolute path, or
	// start with ~/, and sit outside the coppice config, state and data
	// dirs, which the fence treats as secrets.
	Dir string `toml:"dir,omitempty"`
	// TrustedRoots are dirs whose projects get the default permission
	// (clients/go/envdefault) the first time the steward spawns into them,
	// with no card. Empty means DefaultTrustedRoots. The pinned projects
	// count as trusted roots too. Each must be an absolute path or start
	// with ~/, and may not be / or the home dir itself.
	TrustedRoots []string `toml:"trusted_roots,omitempty"`
	// WorkerMaxRunS caps the whole run of a worker the steward spawns, in
	// seconds (tree run --max-run-s). Zero means DefaultWorkerMaxRunS. A
	// lead has no cap: long work is a lead. A pointer, since the TOML
	// encoder counts an int field as set and would write an empty table.
	WorkerMaxRunS *int `toml:"worker_max_run_s,omitempty"`
}

// DefaultWorkerMaxRunS is [steward] worker_max_run_s when the file sets none.
const DefaultWorkerMaxRunS = 1800

// WorkerMaxRunSOrDefault is [steward] worker_max_run_s, or its default.
func (c Config) WorkerMaxRunSOrDefault() int {
	if c.Steward.WorkerMaxRunS != nil && *c.Steward.WorkerMaxRunS > 0 {
		return *c.Steward.WorkerMaxRunS
	}
	return DefaultWorkerMaxRunS
}

// DefaultTrustedRoots is [steward] trusted_roots when the file sets none.
var DefaultTrustedRoots = []string{"~/code"}

// TrustedRoots are the dirs whose projects the steward grants the default
// permission without a card: [steward] trusted_roots (or
// DefaultTrustedRoots) with ~ expanded, then the pinned projects. A value
// that is not allowed is an error that names the key and the file.
func (c Config) TrustedRoots() ([]string, error) {
	home, herr := os.UserHomeDir()
	want := c.Steward.TrustedRoots
	if len(want) == 0 {
		want = DefaultTrustedRoots
	}
	var out []string
	for _, r := range want {
		d := r
		if d == "~" || strings.HasPrefix(d, "~/") {
			if herr != nil {
				return nil, fmt.Errorf("[steward] trusted_roots holds %q in %s, and the home dir is unknown: %w", r, Path(), herr)
			}
			d = filepath.Join(home, d[1:])
		}
		if !filepath.IsAbs(d) {
			return nil, fmt.Errorf("[steward] trusted_roots holds %q in %s. Use an absolute path, or one that starts with ~/", r, Path())
		}
		d = filepath.Clean(d)
		if d == "/" || (herr == nil && d == filepath.Clean(home)) {
			return nil, fmt.Errorf("[steward] trusted_roots holds %q in %s. A trusted root may not be / or the home dir: name the dir your projects sit in", r, Path())
		}
		out = append(out, d)
	}
	for _, p := range c.Projects {
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	return out, nil
}

// Steward modes.
const (
	StewardPane = "pane"
	StewardChat = "chat"
)

// StewardSettings is the [steward] table with its defaults filled in.
type StewardSettings struct {
	Mode             string
	CompactorProfile string
	// CompactorModel is as the file has it. Empty means haiku on the
	// default profile, and the profile's own model on any other.
	CompactorModel string
	// TurnProfiles is the chain of turn profiles, in order, never empty.
	// An empty name runs the turn on Claude Code; any other value is the
	// name of an operator agent profile. turn_profile = "a" reads as ["a"],
	// and neither key reads as [""].
	TurnProfiles []string
	TurnModel    string
	// Store is the Pando R2 store name; work when the file names none.
	Store string
	// Dir is the steward's working dir, ~ expanded; empty means the default.
	Dir string
}

// Steward defaults.
const (
	DefaultCompactorProfile = "default"
	DefaultCompactorModel   = "haiku"
)

// StewardTurnTimeout is how long one steward turn may take.
const StewardTurnTimeout = 300 * time.Second

// StewardSettings reads the [steward] table. A value that is not allowed is
// an error that names the key and the file.
func (c Config) StewardSettings() (StewardSettings, error) {
	f := c.Steward
	s := StewardSettings{
		Mode: StewardPane, CompactorProfile: DefaultCompactorProfile, CompactorModel: f.CompactorModel,
		TurnModel: f.TurnModel, Store: pando.DefaultStore,
	}
	if f.Dir != "" {
		d := f.Dir
		if d == "~" || strings.HasPrefix(d, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return StewardSettings{}, fmt.Errorf("[steward] dir is %q in %s, and the home dir is unknown: %w", f.Dir, Path(), err)
			}
			d = filepath.Join(home, d[1:])
		}
		if !filepath.IsAbs(d) {
			return StewardSettings{}, fmt.Errorf("[steward] dir is %q in %s. Use an absolute path, or one that starts with ~/", f.Dir, Path())
		}
		s.Dir = filepath.Clean(d)
	}
	if f.Store != "" {
		if _, err := pando.StoreDir("", f.Store); err != nil {
			return StewardSettings{}, fmt.Errorf("[steward] store is %q in %s: %w", f.Store, Path(), err)
		}
		s.Store = f.Store
	}
	switch {
	case f.TurnProfile != "" && len(f.TurnProfiles) > 0:
		return StewardSettings{}, fmt.Errorf("[steward] sets both turn_profile and turn_profiles in %s. Keep turn_profiles and delete turn_profile", Path())
	case len(f.TurnProfiles) > 1 && f.TurnModel == "":
		return StewardSettings{}, fmt.Errorf("[steward] turn_profiles lists %d accounts in %s, so turn_model must name the one model they all serve", len(f.TurnProfiles), Path())
	case len(f.TurnProfiles) > 0:
		seen := map[string]bool{}
		for _, p := range f.TurnProfiles {
			name := p
			if name == "default" {
				name = ""
			}
			if seen[name] {
				shown := p
				if p == "" {
					shown = `"" (Claude Code)`
				}
				return StewardSettings{}, fmt.Errorf("[steward] turn_profiles lists the account %s twice in %s. Each account may appear once in the turn chain (\"\" and \"default\" are the same login)", shown, Path())
			}
			seen[name] = true
		}
		s.TurnProfiles = append([]string{}, f.TurnProfiles...)
	default:
		s.TurnProfiles = []string{f.TurnProfile}
	}
	if f.Mode != "" {
		if f.Mode != StewardPane && f.Mode != StewardChat {
			return StewardSettings{}, fmt.Errorf("[steward] mode is %q in %s. Use \"pane\" or \"chat\"", f.Mode, Path())
		}
		s.Mode = f.Mode
	}
	if f.CompactorProfile != "" {
		s.CompactorProfile = f.CompactorProfile
	}
	return s, nil
}

// CompactorModelFor is the model the compactor asks for, given the model
// the compactor's profile names ("" for none).
func (s StewardSettings) CompactorModelFor(profileModel string) string {
	switch {
	case s.CompactorModel != "":
		return s.CompactorModel
	case s.CompactorProfile != DefaultCompactorProfile && profileModel != "":
		return profileModel
	}
	return DefaultCompactorModel
}

package config

import (
	"fmt"
	"time"
)

// Foreman is the [foreman] table. It sets how the floor's foreman runs.
//
// mode = "pane" (the default) runs the foreman as a harness pane in its own
// directory, as always. mode = "chat" keeps the foreman as one chat that
// never ends: each sentence the operator says is logged, and a fresh model
// call answers it. compactor_profile and compactor_model say where the
// chat's summaries are made. turn_profile is where each turn runs: empty
// (the default) or "default" is Claude Code, any other value names an
// operator agent profile (an Anthropic API key, or any OpenAI-style
// server). turn_profiles is the same as an ordered list of accounts that all
// serve ONE model (two subscriptions, or a subscription and an API key): a
// turn tries the first account that is not cooling down, and moves to the
// next when one is out of quota or cannot be reached. It never moves to
// another model (see server/foreman_failover.go). With more than one entry,
// turn_model must name that model.
// turn_profile = "a" is turn_profiles = ["a"]; setting both is an error. An
// empty name in the list is Claude Code. turn_model is the model of
// every entry (empty means Claude Code's own default, or the profile's own
// model). A turn may
// take ForemanTurnTimeout; that is not a setting.
type Foreman struct {
	Mode             string   `toml:"mode,omitempty"`
	CompactorProfile string   `toml:"compactor_profile,omitempty"`
	CompactorModel   string   `toml:"compactor_model,omitempty"`
	TurnProfile      string   `toml:"turn_profile,omitempty"`
	TurnProfiles     []string `toml:"turn_profiles,omitempty"`
	TurnModel        string   `toml:"turn_model,omitempty"`
}

// Foreman modes.
const (
	ForemanPane = "pane"
	ForemanChat = "chat"
)

// ForemanSettings is the [foreman] table with its defaults filled in.
type ForemanSettings struct {
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
}

// Foreman defaults.
const (
	DefaultCompactorProfile = "default"
	DefaultCompactorModel   = "haiku"
)

// ForemanTurnTimeout is how long one foreman turn may take.
const ForemanTurnTimeout = 300 * time.Second

// ForemanSettings reads the [foreman] table. A value that is not allowed is
// an error that names the key and the file.
func (c Config) ForemanSettings() (ForemanSettings, error) {
	f := c.Foreman
	s := ForemanSettings{
		Mode: ForemanPane, CompactorProfile: DefaultCompactorProfile, CompactorModel: f.CompactorModel,
		TurnModel: f.TurnModel,
	}
	switch {
	case f.TurnProfile != "" && len(f.TurnProfiles) > 0:
		return ForemanSettings{}, fmt.Errorf("[foreman] sets both turn_profile and turn_profiles in %s. Keep turn_profiles and delete turn_profile", Path())
	case len(f.TurnProfiles) > 1 && f.TurnModel == "":
		return ForemanSettings{}, fmt.Errorf("[foreman] turn_profiles lists %d accounts in %s, so turn_model must name the one model they all serve", len(f.TurnProfiles), Path())
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
				return ForemanSettings{}, fmt.Errorf("[foreman] turn_profiles lists the account %s twice in %s. Each account may appear once in the turn chain (\"\" and \"default\" are the same login)", shown, Path())
			}
			seen[name] = true
		}
		s.TurnProfiles = append([]string{}, f.TurnProfiles...)
	default:
		s.TurnProfiles = []string{f.TurnProfile}
	}
	if f.Mode != "" {
		if f.Mode != ForemanPane && f.Mode != ForemanChat {
			return ForemanSettings{}, fmt.Errorf("[foreman] mode is %q in %s. Use \"pane\" or \"chat\"", f.Mode, Path())
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
func (s ForemanSettings) CompactorModelFor(profileModel string) string {
	switch {
	case s.CompactorModel != "":
		return s.CompactorModel
	case s.CompactorProfile != DefaultCompactorProfile && profileModel != "":
		return profileModel
	}
	return DefaultCompactorModel
}

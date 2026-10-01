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
// server). turn_model is the model of each turn (empty
// means Claude Code's own default, or the profile's own model). A turn may
// take ForemanTurnTimeout; that is not a setting.
type Foreman struct {
	Mode             string `toml:"mode,omitempty"`
	CompactorProfile string `toml:"compactor_profile,omitempty"`
	CompactorModel   string `toml:"compactor_model,omitempty"`
	TurnProfile      string `toml:"turn_profile,omitempty"`
	TurnModel        string `toml:"turn_model,omitempty"`
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
	// TurnProfile is as the file has it. Empty runs each turn on Claude
	// Code; any other value is the name of an operator agent profile.
	TurnProfile string
	TurnModel   string
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
		TurnProfile: f.TurnProfile, TurnModel: f.TurnModel,
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

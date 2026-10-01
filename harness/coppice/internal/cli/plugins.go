package cli

import (
	"fmt"
	"log/slog"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/plugins"
	"github.com/opendaisugi/coppice/internal/server"
)

// loadPlugins loads the plugins coppice.toml enables and prints each one
// that did not load. A config file that does not parse runs no plugin.
func (c *CLI) loadPlugins() []plugins.Plugin {
	ps, _ := c.loadPluginsAndConfig()
	return ps
}

// loadPluginsAndConfig is loadPlugins with the operator's plugin tables.
func (c *CLI) loadPluginsAndConfig() ([]plugins.Plugin, map[string]map[string]any) {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintf(c.Err, "plugins: cannot read %s: %v. No plugin runs.\n", config.Path(), err)
		return nil, nil
	}
	ps, probs := plugins.LoadEnabled(cfg.EnabledPlugins())
	for _, p := range probs {
		fmt.Fprintln(c.Err, "plugins: "+p.String())
	}
	return ps, cfg.Plugin
}

// policySpecs is the verbs and events each policy in ps holds.
func policySpecs(ps []plugins.Plugin) map[string]server.PluginSpec {
	out := map[string]server.PluginSpec{}
	for _, p := range ps {
		if p.Kind == plugins.KindPolicy {
			out[p.ID] = server.PluginSpec{Needs: p.Needs, Listens: p.Listens}
		}
	}
	return out
}

// startPolicies tells s which policies run and starts them. The caller
// stops the returned runner before it closes s.
func (c *CLI) startPolicies(s *server.Server, ps []plugins.Plugin, settings map[string]map[string]any) *plugins.Runner {
	s.SetPlugins(policySpecs(ps))
	r := &plugins.Runner{
		Socket: c.Socket, WorkDir: plugins.WorkDir(), Lib: plugins.SharedLib(),
		Launch: s.LaunchPlugin, Exited: s.PluginExited, Config: settings,
		Log: slog.New(slog.NewTextHandler(c.Err, nil)), Output: c.Err,
	}
	r.Start(ps)
	return r
}

// viewIDs is the ids of the views among ps.
func viewIDs(ps []plugins.Plugin) []string {
	var out []string
	for _, v := range plugins.ViewsOf(ps) {
		out = append(out, v.ID)
	}
	return out
}

// voiceConfig is what coppice.toml's [voice] table asks of the server. A
// file that does not read might have said enabled = false, so voice stays
// off, and voice.status says why.
// foremanConfig is what coppice.toml's [foreman] table asks of the server.
// A file that does not read, or a value that is not allowed, leaves the
// foreman in pane mode and says why in a note: the floor always starts.
func foremanConfig() server.ForemanConfig {
	cfg, _, err := config.Load()
	if err != nil {
		return server.ForemanConfig{Problem: fmt.Sprintf(
			"The foreman stays in pane mode: %s does not read (%v). Fix the file, then start the coppice server again.", config.Path(), err)}
	}
	s, err := cfg.ForemanSettings()
	if err != nil {
		return server.ForemanConfig{Problem: fmt.Sprintf("The foreman stays in pane mode: %v.", err)}
	}
	return server.ForemanConfigOf(s)
}

func voiceConfig() server.VoiceConfig {
	cfg, _, err := config.Load()
	if err != nil {
		return server.VoiceConfig{Off: true,
			OffReason: fmt.Sprintf("Voice is off: %s does not read (%v).", config.Path(), err),
			OffFix:    "Fix the file, then start the coppice server again"}
	}
	return server.VoiceConfig{
		Off: !cfg.VoiceOn(), URL: cfg.Voice.URL, TokenFile: cfg.Voice.TokenFile,
		Args:     append([]string{}, cfg.Voice.Args...),
		SpeakURL: cfg.Voice.SpeakURL, SpeakModel: cfg.Voice.SpeakModel,
		SpeakVoice: cfg.Voice.SpeakVoice, SpeakProfile: cfg.Voice.SpeakProfile,
	}
}

package install

import (
	"fmt"
	"strings"
)

// Component is one name `daisugi install --only` takes: a layer under its
// user-facing name (install.COMPONENTS).
type Component struct {
	Name, Layer, What, Where string
}

// Components is install.COMPONENTS, in order.
var Components = []Component{
	{"skill", LayerSkill, "the opendaisugi-checklist skill directory", "claude codex hermes openclaw"},
	{"mcp", LayerMCP, "the opendaisugi MCP server entry", "claude codex hermes openclaw"},
	{"capture", LayerCapture, "the passive capture hook (daisugi hook record)", "claude hermes openclaw"},
	{"instructions", LayerInstructions, "the pathway note in the harness instructions file", "claude codex hermes openclaw"},
	{"warden", LayerWarden, "the warden hook, audit mode; --enforce makes it deny", "claude codex"},
	{"pando", LayerPando, "the Pando memory: MCP server, chat hooks, Hermes plugin, follow service", "claude codex hermes"},
	{"router", LayerBaseURL, "the harness base_url pointed at the router", "claude codex openclaw"},
}

// DefaultComponents is install.DEFAULT_COMPONENTS.
var DefaultComponents = []string{"skill", "mcp", "capture", "instructions", "warden", "pando"}

func componentNames() string {
	names := make([]string, len(Components))
	for i, c := range Components {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// ComponentLayers is install.component_layers: the layers the names select.
// Each name may hold several, split on commas. An unknown name is an error
// with the oracle's text.
func ComponentLayers(names []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, raw := range names {
		for _, tok := range strings.Split(raw, ",") {
			name := pyStripASCII(tok)
			found := false
			for _, c := range Components {
				if c.Name == name {
					out[c.Layer] = true
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("unknown component '%s'. Components: %s. Run: daisugi install --list",
					name, componentNames())
			}
		}
	}
	return out, nil
}

// ComponentNames is install.component_names: the names of a set of layers,
// in Components order.
func ComponentNames(layers map[string]bool) []string {
	var out []string
	for _, c := range Components {
		if layers[c.Layer] {
			out = append(out, c.Name)
		}
	}
	return out
}

// ListComponents is install.list_components, the table `install --list`
// prints (no trailing newline).
func ListComponents() string {
	var lines []string
	for _, c := range Components {
		lines = append(lines, fmt.Sprintf("%-13s%s (%s)", c.Name, c.What, c.Where))
	}
	lines = append(lines, "",
		"Default: "+strings.Join(DefaultComponents, ", ")+". Add router with --router or --only router.",
		"The pi and OpenCode extensions are separate: daisugi install --harness pi|opencode.")
	return strings.Join(lines, "\n")
}

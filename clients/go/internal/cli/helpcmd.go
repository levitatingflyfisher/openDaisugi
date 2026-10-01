package cli

import "strings"

// helpNotPorted is clients/cli_cases.HELP_NOT_PORTED: the commands of the
// oracle's help --all this binary does not carry, a top-level command or
// "group sub". Their lines are left out (ruling HP-1).
var helpNotPorted = map[string]bool{
	"bench": true, "conformance": true, "coppice": true, "gate audit": true,
}

// helpCmd is `daisugi help [--all]`: the start-here text, or every
// command grouped, as the oracle prints them.
func (e *Env) helpCmd(args []string) error {
	opts := []opt{{names: []string{"--all"}, help: "List every command, grouped."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage("help", err)
	}
	if p.help {
		return e.cmdHelp("help", "", "Show the short start-here text, or every command with --all.", opts)
	}
	if !p.flag("--all") {
		e.out("%s", startHere)
		return nil
	}
	e.out("%s", helpAllForPort(helpAll))
	return nil
}

// helpAllForPort is the oracle's help --all text with the lines of the
// commands this binary does not carry left out.
func helpAllForPort(text string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		words := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "    ") && len(words) >= 2:
			if helpNotPorted[words[0]] || helpNotPorted[words[0]+" "+words[1]] {
				continue
			}
		case strings.HasPrefix(line, "  ") && len(words) > 0:
			if helpNotPorted[words[0]] {
				continue
			}
		}
		b.WriteString(line)
	}
	return b.String()
}

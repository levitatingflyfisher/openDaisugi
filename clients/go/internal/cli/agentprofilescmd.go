package cli

import (
	"math/big"
	"os"
	"sort"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

var agentProfilesHelp = `Usage: daisugi agent-profiles [OPTIONS] COMMAND [ARGS]...

  The operator's agent profiles.

Options:
  --help  Show this message and exit.

Commands:
  bundle  Write the resolved agent profiles to a descriptor.
  list    List the agent profiles: name, wire, base URL, model, and whether the key is set.
`

// agentProfilesCmd is `daisugi agent-profiles`: the operator's table.
func (e *Env) agentProfilesCmd(args []string) error {
	if len(args) == 0 {
		e.out("%s", agentProfilesHelp)
		return exit(2)
	}
	switch args[0] {
	case "--help":
		e.out("%s", agentProfilesHelp)
		return nil
	case "bundle":
		return e.agentProfilesBundle(args[1:])
	case "list":
		return e.agentProfilesList(args[1:])
	}
	e.errf("Usage: daisugi agent-profiles [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi agent-profiles --help' for help.\n\nError: No such command '%s'.\n", args[0])
	return exit(2)
}

// agentProfilesBundle is cli.agent_profiles_bundle_cmd: resolve the table
// (each key_env is read once), print one line to stderr for each profile
// left out, and write the bundle to the descriptor. It prints nothing on
// stdout.
func (e *Env) agentProfilesBundle(args []string) error {
	const cmd = "agent-profiles bundle"
	opts := []opt{{names: []string{"--fd"}, value: true, metavar: "INTEGER", help: "The descriptor to write the bundle to."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "Write the resolved agent profiles to a descriptor.", opts)
	}
	if !p.has("--fd") {
		return e.usage(cmd, &usageError{"Missing option '--fd'."})
	}
	fdText := p.str("--fd", "")
	n, ok := pyInt(fdText)
	if !ok {
		return e.usage(cmd, &usageError{"Invalid value for '--fd': " + pystr.Repr(fdText) + " is not a valid int."})
	}
	fd := big.NewInt(n)
	if fd.Sign() >= 0 && fd.Cmp(big.NewInt(3)) < 0 {
		return e.fail3("Tried to write the agent profiles.", "Descriptor "+fd.String()+" is standard input, output or error.",
			"Pass a descriptor of 3 or more.", 2)
	}
	profiles := agentprof.Resolve(agentprof.ConfiguredTable(e.dataHome()), e.lookup)
	data, notes := agentprof.OperatorBundle(profiles)
	for _, line := range notes {
		e.errf("%s\n", line)
	}
	f := os.NewFile(uintptr(fd.Int64()), "bundle")
	var werr error
	if f == nil || fd.Sign() < 0 {
		werr = os.ErrInvalid
	} else {
		_, werr = f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
	}
	if werr != nil {
		return e.fail3("Tried to write the agent profiles.", "Descriptor "+fd.String()+" cannot be written.",
			"Pass the descriptor the profiles go to.", 2)
	}
	return nil
}

// agentProfilesList is cli.agent_profiles_list_cmd: the valid profiles with
// no key, one line to stderr for each entry left out. It reads whether each
// key variable is set and nothing else about it.
func (e *Env) agentProfilesList(args []string) error {
	const cmd = "agent-profiles list"
	opts := []opt{{names: []string{"--json"}, help: "Machine-readable JSON output."}}
	p, err := parseArgs(args, opts, 0)
	if err != nil {
		return e.usage(cmd, err)
	}
	if p.help {
		return e.cmdHelp(cmd, "", "List the agent profiles: name, wire, base URL, model, and whether the key is set.", opts)
	}
	profiles := agentprof.Resolve(agentprof.ConfiguredTable(e.dataHome()), e.lookup)
	rows := agentprof.ListRows(profiles)
	invalid := make([]string, 0, len(profiles.Invalid))
	for n := range profiles.Invalid {
		invalid = append(invalid, n)
	}
	sort.Strings(invalid)
	for _, n := range invalid {
		e.errf("agent profile '%s' is left out: %s\n", n, profiles.Invalid[n])
	}
	if p.flag("--json") {
		e.out("%s\n", pyjson.CanonicalASCII(rows))
		return nil
	}
	for _, r := range rows {
		o := r.(*pyjson.Object)
		str := func(k string) string {
			if s, ok := o.Value(k).(string); ok && s != "" {
				return s
			}
			return "-"
		}
		key := "key not set"
		if o.Value("key_set") == true {
			key = "key set"
		}
		e.out("%s\t%s\t%s\t%s\t%s\n", str("name"), str("wire"), str("base_url"), str("model"), key)
	}
	return nil
}

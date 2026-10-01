package cli

import (
	"math/big"
	"strings"

	"daisugi-verify/internal/agentprof"
	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// agentOpt is the --agent option of weave, run and orchestrate: the
// runtime of agentic steps.
var agentOpt = opt{names: []string{"--agent"}, value: true, metavar: "TEXT",
	help: "The runtime of agentic steps: claude (claude -p) or sprig."}

// tokensOpt and turnsOpt are the budget of each agentic step's tree.
var tokensOpt = opt{names: []string{"--tokens"}, value: true, metavar: "INTEGER",
	help: "Token budget of each agentic step's delegation tree (default: none)."}
var turnsOpt = opt{names: []string{"--turns"}, value: true, metavar: "INTEGER",
	help: "Turn budget of each agentic step's delegation tree (default: none)."}

// agentRun is --agent with --tokens and --turns.
type agentRun struct {
	Name          string
	Tokens, Turns *big.Int
}

// checkAgent is cli._check_agent then cli._check_budget: an --agent value
// that names no runtime, or a budget that is not a whole number from 0
// to 2**53, is refused in one line, exit 2.
func (e *Env) checkAgent(p *parsed, cmd, args string) (agentRun, error) {
	var run agentRun
	for _, o := range []struct {
		name string
		dst  **big.Int
	}{{"--tokens", &run.Tokens}, {"--turns", &run.Turns}} {
		if !p.has(o.name) {
			continue
		}
		n, ok := new(big.Int).SetString(strings.ReplaceAll(strings.TrimSpace(p.str(o.name, "")), "_", ""), 10)
		if !ok {
			return run, e.usageArgs(cmd, args, &usageError{"Invalid value for '" + o.name + "': " +
				pystr.Repr(p.str(o.name, "")) + " is not a valid integer."})
		}
		*o.dst = n
	}
	agent := p.str("--agent", "claude")
	if agent != "claude" && agent != "sprig" {
		e.errf("Invalid --agent %s; choose from ['claude', 'sprig'].\n", pystr.Repr(agent))
		return run, exit(2)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 53)
	for _, o := range []struct {
		name string
		v    *big.Int
	}{{"--tokens", run.Tokens}, {"--turns", run.Turns}} {
		if o.v != nil && (o.v.Sign() < 0 || o.v.Cmp(limit) > 0) {
			e.errf("Error: %s must be a whole number from 0 to 2**53.\n", o.name)
			return run, exit(2)
		}
	}
	run.Name = agent
	return run, nil
}

// agentic is AgenticExecutor(envelope=env, runtime=agent): the executor
// of agentic steps for this run. sprig is DAISUGI_SPRIG, else sprig on
// PATH.
func (e *Env) agentic(env *pyjson.Object, run agentRun) (*supervise.Agentic, error) {
	agent := run.Name
	self, err := install.Self()
	if err != nil {
		return nil, err
	}
	sprig := "sprig"
	if v, ok := e.lookup("DAISUGI_SPRIG"); ok {
		sprig = v
	}
	// The root starter reads each profile's key variable here, once.
	profiles := agentprof.Resolve(agentprof.ConfiguredTable(e.dataHome()), e.lookup)
	return &supervise.Agentic{Envelope: env, Model: "haiku", Claude: e.llmClient(), Self: self,
		TempDir: e.gettempdir(), Runtime: agent, Sprig: sprig, Environ: e.Environ,
		Profiles: profiles, Tokens: run.Tokens, Turns: run.Turns,
		LookPath: func(name string) (string, error) {
			if strings.Contains(name, "/") {
				return name, nil
			}
			return lookPath(name, e.env["PATH"])
		}}, nil
}

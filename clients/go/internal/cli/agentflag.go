package cli

import (
	"strings"

	"daisugi-verify/internal/install"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// agentOpt is the --agent option of weave, run and orchestrate: the
// runtime of agentic steps.
var agentOpt = opt{names: []string{"--agent"}, value: true, metavar: "TEXT",
	help: "The runtime of agentic steps: claude (claude -p) or sprig."}

// checkAgent is cli._check_agent: an --agent value that names no runtime
// is refused in one line, exit 2.
func (e *Env) checkAgent(p *parsed) (string, error) {
	agent := p.str("--agent", "claude")
	if agent != "claude" && agent != "sprig" {
		e.errf("Invalid --agent %s; choose from ['claude', 'sprig'].\n", pystr.Repr(agent))
		return "", exit(2)
	}
	return agent, nil
}

// agentic is AgenticExecutor(envelope=env, runtime=agent): the executor
// of agentic steps for this run. sprig is DAISUGI_SPRIG, else sprig on
// PATH.
func (e *Env) agentic(env *pyjson.Object, agent string) (*supervise.Agentic, error) {
	self, err := install.Self()
	if err != nil {
		return nil, err
	}
	sprig := "sprig"
	if v, ok := e.lookup("DAISUGI_SPRIG"); ok {
		sprig = v
	}
	return &supervise.Agentic{Envelope: env, Model: "haiku", Claude: e.llmClient(), Self: self,
		TempDir: e.gettempdir(), Runtime: agent, Sprig: sprig, Environ: e.Environ,
		LookPath: func(name string) (string, error) {
			if strings.Contains(name, "/") {
				return name, nil
			}
			return lookPath(name, e.env["PATH"])
		}}, nil
}

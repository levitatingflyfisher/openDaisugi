package warden

import (
	"strings"
	"time"

	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/statefile"
)

// ModeFor is the mode the child will check in, worked out with the child's
// own functions (parseArgv, resolveMode, effectiveMode). The hook parent
// reads it before it starts the child, so a signal that ends the check can
// give the mode's verdict: the allow body in audit, a deny in enforce.
// Anything it cannot read stands as the text of --mode in argv (audit only
// when that says audit), else enforce: an unknown mode must deny.
func ModeFor(argv []string, environ []string) (mode string) {
	mode = modeFromArgv(argv)
	// Reading the mode can note a refused state file. The child notes it
	// again when it decides the call, so the parent keeps these to itself.
	statefile.CollectNotes()
	defer func() {
		statefile.TakeNotes()
		if recover() != nil {
			mode = modeFromArgv(argv)
		}
	}()
	env := map[string]string{}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = pystr.FSDecode([]byte(a))
	}
	r := &runner{env: env, t0: time.Now()}
	o := parseArgv(args, 80)
	r.defaultRoot = pathJoin(r.dataHome(), "warden")
	r.root = r.defaultRoot
	if o.root != nil {
		r.root = pathStr(*o.root)
	}
	return r.effectiveMode(r.resolveMode(o.mode))
}

// modeFromArgv is warden_client.mode_from_argv: the --mode text of argv,
// else enforce. Anything but audit reads as enforce.
func modeFromArgv(argv []string) string {
	mode := "enforce"
	for i, a := range argv {
		if a == "--mode" && i+1 < len(argv) {
			mode = argv[i+1]
		} else if v, ok := strings.CutPrefix(a, "--mode="); ok {
			mode = v
		}
	}
	if mode == "audit" {
		return "audit"
	}
	return "enforce"
}

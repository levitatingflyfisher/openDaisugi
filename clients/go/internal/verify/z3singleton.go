package verify

import "sync"

// InProcessZ3, when a binary sets it before the first check, runs every
// Full-profile check in its own linked Z3 (the daisugi binary sets
// z3.EvalSMTLIB2). conform leaves it nil and talks to `z3 -in`, as the
// conformance spec asks of a client.
var InProcessZ3 func(cmds string) (string, error)

var (
	z3Once   sync.Once
	z3Shared *Z3Client
	z3Err    error
)

// sharedZ3 lazily starts the ONE persistent z3 subprocess this process
// uses for every Full-profile check (envelope self-consistency, plan-vs-
// envelope, vacuity, subsumption). Returns the same client on every call.
func sharedZ3() (*Z3Client, error) {
	z3Once.Do(func() {
		if InProcessZ3 != nil {
			// Read at each call, so a test can put a stand-in in place.
			z3Shared = &Z3Client{eval: func(cmds string) (string, error) { return InProcessZ3(cmds) }}
			return
		}
		z3Shared, z3Err = NewZ3Client()
	})
	return z3Shared, z3Err
}

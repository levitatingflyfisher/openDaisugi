package verify

import (
	"errors"
	"sync"

	"daisugi-verify/internal/z3"
)

// InProcessZ3 runs SMT-LIB2 commands in the Z3 this binary links and
// returns what they print. Every binary that verifies uses it, conform
// included, so none needs a z3 program on PATH. A test may put a stand-in
// in its place; it is read at each query. nil means no solver: every
// Full-profile check then fails closed.
var InProcessZ3 = z3.EvalSMTLIB2

var (
	z3Once   sync.Once
	z3Shared *Z3Client
	z3Err    error
)

// errNoZ3 is a query made while InProcessZ3 is nil.
var errNoZ3 = errors.New("no Z3 is linked")

// sharedZ3 returns the ONE client this process uses for every Full-profile
// check (envelope self-consistency, plan-vs-envelope, vacuity,
// subsumption).
func sharedZ3() (*Z3Client, error) {
	z3Once.Do(func() {
		z3Shared = &Z3Client{eval: func(cmds string) (string, error) {
			f := InProcessZ3
			if f == nil {
				return "", errNoZ3
			}
			return f(cmds)
		}}
	})
	return z3Shared, z3Err
}

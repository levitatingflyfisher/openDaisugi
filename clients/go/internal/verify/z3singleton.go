package verify

import "sync"

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
		z3Shared, z3Err = NewZ3Client()
	})
	return z3Shared, z3Err
}

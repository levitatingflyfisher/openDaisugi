//go:build !linux

package supervise

// sweep is a no-op where the fence does not run.
type sweep struct{}

func newSweep() *sweep { return &sweep{} }

func (s *sweep) clear() bool { return true }

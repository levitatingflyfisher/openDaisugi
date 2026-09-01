//go:build !unix

package server

// There is no portable single-instance lock outside unix. Refusing to start is
// the fail-closed direction, and master spec section 8 puts other platforms out
// of scope.
type startLock struct{}

func lockFile(string) string { return "" }

// StartLockHeld has no portable answer outside unix; report the lock as held
// so a caller fails closed rather than assuming a stop finished.
func StartLockHeld(string) (bool, error) { return true, nil }

func acquireStartLock(string) (*startLock, int, error) { return nil, 0, ErrAlreadyRunning }

func (l *startLock) release() error { return nil }

func readLockPID(string) int { return 0 }

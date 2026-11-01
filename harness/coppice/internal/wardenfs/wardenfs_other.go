//go:build !linux

// Package wardenfs has no openat to read the warden's files as the warden does
// off Linux: every read says nothing is there, and a write is refused.
package wardenfs

import "errors"

// Refused is an error that carries a refusal.
type Refused struct{ Text string }

func (e *Refused) Error() string { return e.Text }

// Read reads nothing.
func Read(root string, dirs []string, name string, maxBytes int64) ([]byte, bool, string) {
	return nil, false, ""
}

// Write writes nothing.
func Write(root string, dirs []string, name string, data []byte) error {
	return errors.New("wardenfs: the warden's files are written only on Linux")
}

// OpenDirs opens nothing.
func OpenDirs(root string, dirs ...string) ([]int, string) { return nil, "" }

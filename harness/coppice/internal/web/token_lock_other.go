//go:build !unix

package web

import "sync"

// namesMu orders changes to the named tokens inside this process. A
// system with no flock has no lock across processes.
var namesMu sync.Mutex

// lockNames takes the process lock on the named tokens. The returned func
// releases it.
func (s TokenStore) lockNames() (func(), error) {
	namesMu.Lock()
	return namesMu.Unlock, nil
}

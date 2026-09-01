// Package adapters is the registry of headless harness adapters. Every slot in
// spec-02 is registered, built or not: an adapter that is only a design gets a
// name here and refuses to start, so `coppice agent list` tells the truth about
// what exists.
package adapters

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/opendaisugi/coppice/internal/pane"
)

// ErrNotBuilt is what a registered but unimplemented adapter returns.
var ErrNotBuilt = errors.New("this adapter is a slot, not an implementation yet")

var (
	mu  sync.RWMutex
	reg = map[string]pane.Adapter{}
)

func Register(a pane.Adapter) {
	mu.Lock()
	defer mu.Unlock()
	reg[a.Name()] = a
}

func Get(name string) (pane.Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := reg[name]
	return a, ok
}

func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NotBuilt is the adapter shape for a slot that is designed and not written.
type NotBuilt struct {
	AdapterName string
	Owner       string // which sub-spec fills it
}

func (n NotBuilt) Name() string { return n.AdapterName }

func (n NotBuilt) Start(_ context.Context, _ pane.StartOpts, _ *pane.Grid) (pane.Proc, error) {
	return nil, fmt.Errorf("%s: %w. %s fills this slot.", n.AdapterName, ErrNotBuilt, n.Owner)
}

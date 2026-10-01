package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/adapters"
	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

// dirSnapshot lists every path under dir with its size and modification
// time, so two snapshots differ when anything under dir was written.
func dirSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s %v %d %s", p, fi.Mode(), fi.Size(), fi.ModTime().Format(time.RFC3339Nano)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// A headless pane's pump saves the layout when its stream ends. When Close
// runs while that save is still in flight, Close must wait for it, and a
// server that has closed must write nothing more into its data dir. Before
// the fix, Close returned with the save still running, so the test's
// TempDir cleanup could find a temp layout file appear under it
// ("directory not empty").
func TestCloseWaitsForADataWriteInFlightAndAllowsNoneAfter(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	var armed atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// Set before the pane exists, so no goroutine reads the field while it
	// changes.
	s.beforeDataWrite = func() {
		if armed.Load() {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	a := &fakeForker{name: "fake-late-writer"}
	adapters.Register(a)
	id := createHeadlessPane(t, s, "fake-late-writer", "late")

	armed.Store(true)
	a.proc(0).closeIn()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the pump never began its terminal save")
	}

	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while a save into the data dir was still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned after the save finished")
	}

	// Nothing writes into the data dir after Close returned: not the pump,
	// and not a late save from any other path.
	before := dirSnapshot(t, s.cfg.DataDir)
	time.Sleep(200 * time.Millisecond)
	if after := dirSnapshot(t, s.cfg.DataDir); len(after) != len(before) {
		t.Fatalf("the data dir changed after Close:\nbefore %v\nafter  %v", before, after)
	} else {
		for i := range after {
			if after[i] != before[i] {
				t.Fatalf("the data dir changed after Close: %q became %q", before[i], after[i])
			}
		}
	}
	if err := os.Remove(layoutPath(s.cfg.DataDir)); err != nil {
		t.Fatal(err)
	}
	s.saveLayout()
	_ = s.updatePane(id, func(*layout.Pane) {})
	if _, err := os.Stat(layoutPath(s.cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatalf("a closed server wrote layout.json again (stat err %v)", err)
	}
}

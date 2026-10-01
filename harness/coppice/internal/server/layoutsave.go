package server

import (
	"sync"
	"time"
)

// layoutSaver writes layout.json once per burst of changes. Every site that
// changes the tree calls markLayoutDirty. The write happens settle after the
// LAST change (each change restarts the timer), and at once on Close. With
// no change there is no write. A settle of zero or less writes at once on
// every change, which the tests use.
type layoutSaver struct {
	mu    sync.Mutex
	dirty bool
	timer *time.Timer
	// closing is set by the first flush (Close). From then on a change is
	// written at once, since a timer would fire after the data dir shuts.
	closing bool
}

// markLayoutDirty says the tree changed and must reach disk.
func (s *Server) markLayoutDirty() {
	settle := s.cfg.LayoutSaveSettle
	ls := &s.layoutSave
	ls.mu.Lock()
	if settle <= 0 || ls.closing {
		ls.mu.Unlock()
		s.saveLayoutNow()
		return
	}
	ls.dirty = true
	if ls.timer == nil {
		ls.timer = time.AfterFunc(settle, s.writePendingLayout)
	} else {
		ls.timer.Reset(settle)
	}
	ls.mu.Unlock()
}

// writePendingLayout writes the layout if a change is waiting. The settle
// timer calls it. It does not change the mode: later changes still wait.
// The lock is not held across the write.
func (s *Server) writePendingLayout() {
	ls := &s.layoutSave
	ls.mu.Lock()
	was := ls.dirty
	ls.dirty = false
	ls.mu.Unlock()
	if was {
		s.saveLayoutNow()
	}
}

// flushLayout writes a waiting change now, and makes every later change
// write at once. Only Close calls it.
func (s *Server) flushLayout() {
	ls := &s.layoutSave
	ls.mu.Lock()
	ls.closing = true
	if ls.timer != nil {
		ls.timer.Stop()
	}
	ls.mu.Unlock()
	s.writePendingLayout()
}

// saveLayoutNow writes the layout. Best effort: a layout write failure must
// never fail a command that already succeeded in the world.
func (s *Server) saveLayoutNow() {
	s.writeData(func() { _ = s.tree.Save(layoutPath(s.cfg.DataDir)) })
}

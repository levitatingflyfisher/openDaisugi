package server

import (
	"testing"
	"time"
)

// server.stop exists only once a caller supplies a stop function: the server
// package itself has no notion of process exit. This test stands in for the
// CLI, which is the real caller.
func TestRegisterStopCommandRepliesStoppingAndCallsStop(t *testing.T) {
	s := newTestServer(t)
	called := make(chan struct{})
	if err := s.RegisterStopCommand(func() { close(called) }); err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, s, `{"id":"1","cmd":"server.stop"}`)
	m := result(t, got[0])
	if m["stopping"] != true {
		t.Fatalf("server.stop result = %v, want stopping true", m)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("server.stop replied but never called the stop function")
	}
}

// Handle refuses a registration once Serve has started, and RegisterStopCommand
// must surface that refusal rather than swallow it - the same rule every other
// verb's registration already follows.
func TestRegisterStopCommandSurfacesAHandleError(t *testing.T) {
	s := newTestServer(t)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	// Serve sets s.serving at the very start of its own goroutine, so the
	// instant after go func(){ s.Serve() }() is not guaranteed to see it yet.
	// Poll until it does, the same way TestRestoreRefusesAfterServeHasStarted
	// in restart_test.go already does for the same reason.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := s.RegisterStopCommand(func() {}); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("RegisterStopCommand kept succeeding after Serve started, want it to eventually refuse")
		}
		time.Sleep(time.Millisecond)
	}
}

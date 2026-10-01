package server

import (
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
)

// A swap to the gateway is refused before anything stops when nothing
// answers at the gateway's address: the agent would come back with a
// base URL that fails every call.
func TestSetRouterToAGatewayThatDoesNotAnswerIsRefusedBeforeAnythingStops(t *testing.T) {
	s, id, _ := swapRouterServer(t)
	dead := "http://127.0.0.1:1"
	if err := config.Save(config.Config{Gateway: &dead, Harness: map[string]config.Harness{
		"claude": {Command: "sh", Args: []string{"-c"}, ResumeArgs: []string{"echo RESUMED-{session}; sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	s.outsideMu.Lock()
	s.outside = outsideCache{}
	s.outsideMu.Unlock()
	got := roundTrip(t, s, `{"id":"2","cmd":"agent.set_router","pane":"`+id+`","router":"gateway"}`)
	want := "the gateway at " + dead + " does not answer. Start it, then swap."
	if got[0].OK || got[0].Error.Code != "bad_request" || got[0].Error.Message != want {
		t.Fatalf("reply = %+v, want bad_request %q", got[0], want)
	}
	if _, ok := s.Live(id); !ok {
		t.Fatal("a refused swap stopped the agent")
	}
	// Direct does not need the gateway.
	got = roundTrip(t, s, `{"id":"3","cmd":"agent.set_router","pane":"`+id+`","router":"direct"}`)
	if !got[0].OK {
		t.Fatalf("direct: %+v", got[0].Error)
	}
}

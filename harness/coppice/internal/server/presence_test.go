package server

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/proto"
)

// named opens one connection that says name in its hello.
func named(t *testing.T, s *Server, name string) (send func(string), next func() map[string]any, stop func()) {
	t.Helper()
	send, next, stop = stream(t, s)
	send(`{"id":"h","cmd":"hello","name":"` + name + `"}`)
	okReply(t, next())
	return
}

// replyTo reads lines until the reply with id, and skips the frames and
// events on the way.
func replyTo(next func() map[string]any, id string) map[string]any {
	for {
		if r := next(); r["id"] == id {
			return r
		}
	}
}

// lookingOf is the looking list of pane id in a fresh pane.list, or nil.
func lookingOf(t *testing.T, s *Server, id string) []string {
	t.Helper()
	got := roundTrip(t, s, `{"id":"l","cmd":"pane.list"}`)
	panes, _ := result(t, got[0])["panes"].([]any)
	for _, raw := range panes {
		p, _ := raw.(map[string]any)
		if p["id"] != id {
			continue
		}
		raw, _ := p["looking"].([]any)
		var out []string
		for _, n := range raw {
			out = append(out, n.(string))
		}
		return out
	}
	t.Fatalf("no pane %s in pane.list", id)
	return nil
}

// waitLooking waits until pane id shows want.
func waitLooking(t *testing.T, s *Server, id string, want []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = lookingOf(t, s, id); reflect.DeepEqual(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("looking = %v, want %v", got, want)
}

func TestPaneListSaysWhoIsLooking(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	aSend, aNext, aStop := named(t, s, "alice")
	defer aStop()
	bSend, bNext, bStop := named(t, s, "bob")
	defer bStop()
	aSend(`{"id":"a","cmd":"pane.attach","pane":"w1:p1"}`)
	okReply(t, replyTo(aNext, "a"))
	bSend(`{"id":"b","cmd":"pane.attach","pane":"w1:p1","view_only":true}`)
	okReply(t, replyTo(bNext, "b"))
	waitLooking(t, s, "w1:p1", []string{"alice", "bob"})

	bSend(`{"id":"d","cmd":"pane.detach","pane":"w1:p1"}`)
	okReply(t, replyTo(bNext, "d"))
	waitLooking(t, s, "w1:p1", []string{"alice"})

	aStop()
	waitLooking(t, s, "w1:p1", nil)
}

// A connection with no name, and a pane, are not people. Neither shows.
func TestOnlyNamedOperatorsShowAsLooking(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	send, next, stop := stream(t, s)
	defer stop()
	send(`{"id":"a","cmd":"pane.attach","pane":"w1:p1"}`)
	okReply(t, replyTo(next, "a"))
	pSend, pNext, pStop := streamFacts(t, s, &peerFacts{checked: true, pane: true, paneID: "w1:p9"})
	defer pStop()
	pSend(`{"id":"h","cmd":"hello","name":"carol"}`)
	okReply(t, pNext())
	pSend(`{"id":"a","cmd":"pane.attach","pane":"w1:p1","view_only":true}`)
	okReply(t, replyTo(pNext, "a"))
	if got := lookingOf(t, s, "w1:p1"); got != nil {
		t.Fatalf("looking = %v, want nobody", got)
	}
}

// A client that subscribed to presence hears who looks at a pane when it
// changes.
func TestPresenceIsAnEvent(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	wSend, wNext, wStop := stream(t, s)
	defer wStop()
	wSend(`{"id":"s","cmd":"events.subscribe","kinds":["presence"],"panes":"*"}`)
	okReply(t, replyTo(wNext, "s"))
	aSend, aNext, aStop := named(t, s, "alice")
	defer aStop()
	aSend(`{"id":"a","cmd":"pane.attach","pane":"w1:p1"}`)
	okReply(t, replyTo(aNext, "a"))
	for {
		ev := wNext()
		if ev["event"] != "presence" {
			continue
		}
		if ev["pane"] != "w1:p1" || fmt.Sprint(ev["looking"]) != "[alice]" {
			t.Fatalf("presence event = %v", ev)
		}
		break
	}
	aSend(`{"id":"d","cmd":"pane.detach","pane":"w1:p1"}`)
	okReply(t, replyTo(aNext, "d"))
	for {
		ev := wNext()
		if ev["event"] != "presence" {
			continue
		}
		if ev["pane"] != "w1:p1" || fmt.Sprint(ev["looking"]) != "[]" {
			t.Fatalf("presence event = %v, want nobody looking", ev)
		}
		break
	}
}

// Attaches and detaches on many connections at once end with a last
// presence event that matches pane.list. Each event is worked out and sent
// in one step, so a stale list can never arrive after a fresh one.
func TestTheLastPresenceEventMatchesPaneList(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { s.ServeConn(inR, outW); _ = outW.Close() }()
	defer inW.Close()
	events := make(chan map[string]any, 4096)
	go func() {
		defer close(events)
		d := proto.NewDecoder(outR)
		for {
			line, err := d.Next()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				events <- m
			}
		}
	}()
	if _, err := io.WriteString(inW, `{"id":"s","cmd":"events.subscribe","kinds":["presence"],"panes":"*"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if r := <-events; r["ok"] != true {
		t.Fatalf("subscribe: %v", r)
	}
	const people, rounds = 6, 15
	var wg sync.WaitGroup
	stops := make([]func(), people)
	for i := 0; i < people; i++ {
		send, next, stop := named(t, s, fmt.Sprintf("p%d", i))
		stops[i] = stop
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				send(fmt.Sprintf(`{"id":"a%d","cmd":"pane.attach","pane":"w1:p1","view_only":true}`, r))
				replyTo(next, fmt.Sprintf("a%d", r))
				if r < rounds-1 || i%3 == 0 {
					send(fmt.Sprintf(`{"id":"d%d","cmd":"pane.detach","pane":"w1:p1"}`, r))
					replyTo(next, fmt.Sprintf("d%d", r))
				}
			}
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	want := "[p1 p2 p4 p5]"
	if got := fmt.Sprint(lookingOf(t, s, "w1:p1")); got != want {
		t.Fatalf("pane.list says %s, want %s", got, want)
	}
	last := ""
	for {
		select {
		case ev := <-events:
			if ev["event"] == "presence" {
				last = fmt.Sprint(ev["looking"])
			}
			continue
		case <-time.After(500 * time.Millisecond):
		}
		break
	}
	if last != want {
		t.Fatalf("the last presence event says %s, pane.list says %s", last, want)
	}
}

// A presence event held between working out the list and sending it
// cannot arrive after a later event with a fresher list.
func TestAHeldPresenceEventDoesNotArriveLate(t *testing.T) {
	s := newAgentServer(t)
	roundTrip(t, s, strings.Replace(paneCreate, "%s", t.TempDir(), 1))
	wSend, wNext, wStop := stream(t, s)
	defer wStop()
	wSend(`{"id":"s","cmd":"events.subscribe","kinds":["presence"],"panes":"*"}`)
	okReply(t, replyTo(wNext, "s"))
	aSend, aNext, aStop := named(t, s, "alice")
	defer aStop()
	aSend(`{"id":"a","cmd":"pane.attach","pane":"w1:p1"}`)
	okReply(t, replyTo(aNext, "a"))
	if ev := replyToEvent(wNext); fmt.Sprint(ev["looking"]) != "[alice]" {
		t.Fatalf("first presence event = %v", ev)
	}

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.presenceGap = func(string) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(reached)
			<-release
		}
	}
	aSend(`{"id":"d","cmd":"pane.detach","pane":"w1:p1"}`)
	<-reached
	bSend, bNext, bStop := named(t, s, "bob")
	defer bStop()
	bSend(`{"id":"b","cmd":"pane.attach","pane":"w1:p1","view_only":true}`)
	time.Sleep(200 * time.Millisecond)
	close(release)
	okReply(t, replyTo(aNext, "d"))
	okReply(t, replyTo(bNext, "b"))
	first, second := replyToEvent(wNext), replyToEvent(wNext)
	if fmt.Sprint(first["looking"]) != "[]" || fmt.Sprint(second["looking"]) != "[bob]" {
		t.Fatalf("presence events came as %v then %v, want [] then [bob]", first["looking"], second["looking"])
	}
}

// replyToEvent reads lines until a presence event.
func replyToEvent(next func() map[string]any) map[string]any {
	for {
		if r := next(); r["event"] == "presence" {
			return r
		}
	}
}

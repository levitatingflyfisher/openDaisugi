package web

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// denyFixture is a web server with a publisher that has put one card, and
// so one deny token, on the lock screen for ask toolu_1.
func denyFixture(t *testing.T) (url, bearer, deny, gate string, s *Server) {
	t.Helper()
	capd, base := newNtfy(t)
	now := time.Unix(1_757_300_000, 0)
	pub := newPublisher(t, base, &now, "")
	pub.OnState(tieredEvent("w1:p1", "permanent"), "auth fix")
	deny = denyTokenIn(t, capd.reqs[0].Actions)
	gate = t.TempDir()
	writeTieredAsk(t, gate, "toolu_1", "permanent")
	writeTieredAsk(t, gate, "toolu_2", "undoable")
	_, d := newFakeServer(t, agentsWithAsk("toolu_2", "undoable"))
	ts, srv, tok := newTestServer(t, Options{Dial: d, Gate: AskChannel{Root: gate}, Push: pub})
	return ts.URL, tok, deny, gate, srv
}

func TestADenyTokenDeniesItsOwnAsk(t *testing.T) {
	url, _, deny, gate, _ := denyFixture(t)
	code, msg := postAnswer(t, url, deny, `{"tool_use_id":"toolu_1","decision":"deny"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d %q", code, msg)
	}
	if _, err := os.Stat(filepath.Join(gate, "answers", "toolu_1.json")); err != nil {
		t.Fatal("no answer written")
	}
	// Used once, its ask is gone, and a second tap says so.
	if code, _ := postAnswer(t, url, deny, `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusConflict {
		t.Fatalf("a used token answered %d, want 409", code)
	}
}

func TestADenyTokenCannotAllow(t *testing.T) {
	url, _, deny, gate, _ := denyFixture(t)
	code, _ := postAnswer(t, url, deny, `{"tool_use_id":"toolu_1","decision":"allow","confirm":"auth fix"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", code)
	}
	if _, err := os.Stat(filepath.Join(gate, "answers", "toolu_1.json")); err == nil {
		t.Fatal("a deny token wrote an allow")
	}
}

func TestADenyTokenCannotDenyAnotherAsk(t *testing.T) {
	url, _, deny, gate, _ := denyFixture(t)
	if code, _ := postAnswer(t, url, deny, `{"tool_use_id":"toolu_2","decision":"deny"}`); code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", code)
	}
	if _, err := os.Stat(filepath.Join(gate, "answers", "toolu_2.json")); err == nil {
		t.Fatal("a deny token answered another ask")
	}
}

func TestADenyTokenOpensNoOtherRoute(t *testing.T) {
	url, _, deny, _, _ := denyFixture(t)
	for _, path := range []string{"/api/panes", "/api/token/check"} {
		req, _ := http.NewRequest("GET", url+path, nil)
		req.Header.Set("Authorization", "Bearer "+deny)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with a deny token answered %d", path, resp.StatusCode)
		}
	}
}

// The bearer still does everything, and a full answer retires the card's
// token for that ask.
func TestTheBearerStillAnswersAndRetiresTheCard(t *testing.T) {
	url, bearer, deny, _, _ := denyFixture(t)
	if code, msg := postAnswer(t, url, bearer, `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusOK {
		t.Fatalf("status %d %q", code, msg)
	}
	if code, _ := postAnswer(t, url, deny, `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusConflict {
		t.Fatalf("the card's token survived the answer: %d", code)
	}
}

// The operator answers on the floor, then taps Deny on the lock screen.
// That is normal use: no strike, so the phone is never banned for it.
func TestStaleCardTapsNeverBanThePhone(t *testing.T) {
	url, bearer, deny, _, _ := denyFixture(t)
	postAnswer(t, url, bearer, `{"tool_use_id":"toolu_1","decision":"deny"}`)
	for i := 0; i < 5; i++ {
		if code, _ := postAnswer(t, url, deny, `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusConflict {
			t.Fatalf("tap %d answered %d, want 409", i, code)
		}
	}
	req, _ := http.NewRequest("GET", url+"/api/token/check", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the phone was banned after stale taps: %d", resp.StatusCode)
	}
}

// A token nobody minted is still a strike.
func TestAMadeUpTokenIsStillAStrike(t *testing.T) {
	url, _, _, _, _ := denyFixture(t)
	if code, _ := postAnswer(t, url, "made-up", `{"tool_use_id":"toolu_1","decision":"deny"}`); code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", code)
	}
}

package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

const blockedLine = `{"v":1,"ts":1,"session_id":"s","harness":"claude-code","pane":"w1:p1",` +
	`"state":"blocked","source":"gate","ask":{"id":"t1","tool":"Bash","summary":"ls","deadline":9%s}}`

func withTier(tier string) []byte {
	return []byte(strings.Replace(blockedLine, "9%s", "9"+tier, 1))
}

func TestValidateSetsAnAskWithNoTierToPermanent(t *testing.T) {
	ev := PaneStateEvent{V: 1, State: StateBlocked, Source: SrcGate,
		Ask: &Ask{ID: "t1", Tool: "Bash", Summary: "ls", Deadline: 9}}
	if err := ev.Validate(); err != nil {
		t.Fatal(err)
	}
	if ev.Ask.Tier != TierPermanent {
		t.Fatalf("tier %q, want permanent", ev.Ask.Tier)
	}
}

func TestParseReadsAMissingTierAsPermanent(t *testing.T) {
	ev, err := ParseStateEvent(withTier(""))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Ask.Tier != TierPermanent {
		t.Fatalf("tier %q, want permanent", ev.Ask.Tier)
	}
}

func TestParseKeepsAnUndoableTier(t *testing.T) {
	ev, err := ParseStateEvent(withTier(`,"tier":"undoable"`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Ask.Tier != TierUndoable {
		t.Fatalf("tier %q, want undoable", ev.Ask.Tier)
	}
}

func TestParseReadsAnUnknownTierAsPermanent(t *testing.T) {
	for _, tier := range []string{`"silent"`, `"bogus"`, `""`, `null`} {
		ev, err := ParseStateEvent(withTier(`,"tier":` + tier))
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if ev.Ask.Tier != TierPermanent {
			t.Fatalf("%s: tier %q, want permanent", tier, ev.Ask.Tier)
		}
	}
}

func TestParseRefusesATierThatIsNotAString(t *testing.T) {
	if _, err := ParseStateEvent(withTier(`,"tier":5`)); err == nil {
		t.Fatal("a numeric tier parsed")
	}
}

func TestTheTierRidesOnTheWire(t *testing.T) {
	ev, err := ParseStateEvent(withTier(`,"tier":"undoable"`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tier":"undoable"`) {
		t.Fatalf("encoded %s, want the tier", b)
	}
}

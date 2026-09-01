package web

import (
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestBanlistBansAfterThreeBadTokensInOneMinute(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	b.Fail("10.0.0.7")
	now = now.Add(10 * time.Second)
	b.Fail("10.0.0.7")
	if b.Banned("10.0.0.7") {
		t.Fatal("two failures banned an address")
	}
	now = now.Add(10 * time.Second)
	b.Fail("10.0.0.7")
	if !b.Banned("10.0.0.7") {
		t.Fatal("three failures inside a minute did not ban the address")
	}
	if b.Banned("10.0.0.8") {
		t.Fatal("the ban leaked to another address")
	}
}

func TestBanlistForgetsFailuresOlderThanAMinute(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	b.Fail("10.0.0.7")
	b.Fail("10.0.0.7")
	now = now.Add(61 * time.Second)
	b.Fail("10.0.0.7")
	if b.Banned("10.0.0.7") {
		t.Fatal("failures a minute apart accumulated into a ban")
	}
}

// The failure this names: an address that failed once and never came back
// stays in fails forever, because nothing but a later Fail for that same
// address ever shrinks the map. On a tailnet the address set is small, so
// this is bounded in practice, but Banned should still sweep what Fail
// leaves behind rather than lean on that.
func TestBannedSweepsFailuresOlderThanTheWindowForEveryAddress(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	b.Fail("10.0.0.7") // one failure, never repeated
	now = now.Add(BanWindow + time.Second)
	b.Banned("10.0.0.8") // a query for an unrelated address
	if n := len(b.fails); n != 0 {
		t.Fatalf("fails still holds %d addresses after the window passed", n)
	}
}

func TestBanLiftsAfterSixtySeconds(t *testing.T) {
	now := time.Unix(1_757_300_000, 0)
	b := NewBanlist(fixedClock(&now))
	for i := 0; i < BanFailures; i++ {
		b.Fail("10.0.0.7")
	}
	now = now.Add(59 * time.Second)
	if !b.Banned("10.0.0.7") {
		t.Fatal("the ban lifted early")
	}
	now = now.Add(2 * time.Second)
	if b.Banned("10.0.0.7") {
		t.Fatal("the ban did not lift after a minute")
	}
}

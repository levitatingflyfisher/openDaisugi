package web

import (
	"sync"
	"time"
)

const (
	// BanFailures is how many bad tokens from one address inside BanWindow
	// earn that address BanDuration of silence.
	BanFailures = 3
	BanWindow   = time.Minute
	BanDuration = time.Minute
)

// Banlist counts bad tokens per client address. A ban holds even if a good
// token arrives during it. An address that just guessed three times is not
// trusted for a minute, and the operator's own phone can wait that long.
type Banlist struct {
	mu     sync.Mutex
	now    func() time.Time
	fails  map[string][]time.Time
	banned map[string]time.Time
}

// NewBanlist creates a banlist that reads the time from now. Pass time.Now
// in production. A test passes a fake clock, so a ban can be proven without
// waiting a real minute.
func NewBanlist(now func() time.Time) *Banlist {
	if now == nil {
		now = time.Now
	}
	return &Banlist{now: now, fails: map[string][]time.Time{}, banned: map[string]time.Time{}}
}

// Banned reports whether addr is serving a ban right now. A ban whose time
// has passed is cleared as a side effect of asking. It also sweeps fails:
// an address that failed once and never came back would otherwise sit in
// that map forever, since only a ban or a later Fail for the same address
// ever shrinks it.
func (b *Banlist) Banned(addr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepFails()
	until, ok := b.banned[addr]
	if !ok {
		return false
	}
	if !b.now().Before(until) {
		delete(b.banned, addr)
		return false
	}
	return true
}

// sweepFails drops every address whose most recent failure is older than
// BanWindow. Callers must hold b.mu.
func (b *Banlist) sweepFails() {
	now := b.now()
	for addr, times := range b.fails {
		newest := times[len(times)-1]
		if now.Sub(newest) >= BanWindow {
			delete(b.fails, addr)
		}
	}
}

// Fail records one bad token from addr. Failures older than BanWindow do not
// count. Once BanFailures land inside BanWindow, addr is banned for
// BanDuration and its failure history is cleared.
func (b *Banlist) Fail(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	recent := b.fails[addr][:0]
	for _, t := range b.fails[addr] {
		if now.Sub(t) < BanWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	b.fails[addr] = recent
	if len(recent) >= BanFailures {
		b.banned[addr] = now.Add(BanDuration)
		delete(b.fails, addr)
	}
}

package foreman

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The foreman's failover (FM-R-45 to FM-R-54): a turn tries an ordered chain
// of profiles. A profile that is out of quota, overloaded or not reachable
// cools down, and the turn moves to the next one; when the cool-down ends the
// next turn tries the first profile again. This file holds the two pure
// parts, with no clock of its own and no I/O but one small state file: the
// classifier that reads a failed turn, and the chain with its cool-downs.
// The server (server/foreman_failover.go) runs the turns over it.

// Outcome says what a failed turn means for the chain.
type Outcome int

const (
	// NotOut: the failure is not an outage. The turn fails as it always
	// did and the chain does not move.
	NotOut Outcome = iota
	// Out: the profile is out of quota, overloaded or not reachable.
	// The profile cools down and the turn moves on.
	Out
	// KeyProblem: HTTP 401 or 403. A key is wrong, which a move would
	// hide, so the turn fails and the chain does not move.
	KeyProblem
)

// Verdict is what Classify makes of a failed turn.
type Verdict struct {
	Outcome Outcome
	// Reason is a short plain phrase: "HTTP 429", "usage limit reached".
	Reason string
	// Reset is when the reply says the profile is usable again; zero when
	// it names no time.
	Reset time.Time
}

// OutTexts are the known texts of a quota or a usage limit, lower case,
// matched anywhere in the failure in any case. Anthropic, OpenAI and Claude
// Code each have their own. A new provider text is one line here and one
// case in the test, which walks this list.
var OutTexts = []string{
	// Anthropic API.
	"rate_limit_error",
	"overloaded_error",
	"credit balance is too low",
	"reached your specified api usage limits",
	// OpenAI API and its compatible servers.
	"insufficient_quota",
	"you exceeded your current quota",
	"rate_limit_exceeded",
	"rate limit reached for",
	// Others that speak the same wire.
	"resource_exhausted",
	// Claude Code (claude -p).
	"usage limit reached",
	"claude ai usage limit",
	"5-hour limit reached",
	"weekly limit reached",
	"you've hit your limit",
	"limit reached ∙ resets",
}

// connTexts are the texts of a profile that cannot be reached, lower case.
var connTexts = []string{
	"connection refused",
	"no such host",
	"no route to host",
	"network is unreachable",
	"tcp connect error",
	"dns error",
	"failed to lookup address",
	"temporary failure in name resolution",
	"connect: connection timed out",
}

var (
	// statusRe finds an HTTP status the way sprig and claude word it:
	// "anthropic api 429: ...", "API Error: 529", "HTTP 503".
	statusRe   = regexp.MustCompile(`(?i)\b(?:api|http|status|status code|error)[: ]+(\d{3})\b`)
	dialTimeRe = regexp.MustCompile(`(?i)dial tcp[^\n]*i/o timeout`)
	retryAfter = regexp.MustCompile(`(?im)^\s*retry-after:\s*(\S[^\r\n]*)$`)
	anthReset  = regexp.MustCompile(`(?im)\banthropic-ratelimit-[a-z-]*-reset:\s*(\S+)`)
	oaiReset   = regexp.MustCompile(`(?im)\bx-ratelimit-reset-[a-z]+:\s*(\S+)`)
	epochReset = regexp.MustCompile(`(?i)limit reached\|(\d{9,11})\b`)
)

// ClassifyBytes is how much of a failure's text the classifier reads: the
// last 4 KiB, where a runner puts the code and the headers.
const ClassifyBytes = 4096

// Tail is the last n bytes of s, cut at a character start.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

var keyReasonRe = regexp.MustCompile(`^HTTP \d{3}: the key was refused\. That is a key problem, not a quota$`)

// SafeReason is the only form of a failure that is saved or shown. It is
// information only, so it is never raw error text: a fixed phrase of the
// classifier (a quota text, "cannot connect", the key-refused line), else
// "HTTP nnn" when the text names a status, else "the turn failed". A key
// cannot survive it, whatever its shape (FM-R-60).
func SafeReason(s string) string {
	s = strings.TrimSpace(s)
	if keyReasonRe.MatchString(s) || s == "cannot connect" {
		return s
	}
	for _, t := range OutTexts {
		if s == t {
			return s
		}
	}
	if m := statusRe.FindStringSubmatch(s); m != nil {
		return "HTTP " + m[1]
	}
	return "the turn failed"
}

// MaxNamedReset caps a reset time that a reply names.
const MaxNamedReset = 7 * 24 * time.Hour

// Classify reads the text of a failed turn. A key problem (401, 403) comes
// first so that a bad key is never taken for a quota. Then come the codes of
// an outage (429, 529, 503), the known quota texts, and a profile that cannot
// be connected to. Everything else (400, a model error, a tool error, a bad
// answer) is NotOut. now is the clock of the caller, for a reset time given
// as a delay.
func Classify(text string, now time.Time) Verdict {
	now = now.Truncate(time.Second)
	text = Tail(text, ClassifyBytes)
	low := strings.ToLower(text)
	var codes []string
	for _, m := range statusRe.FindAllStringSubmatch(text, -1) {
		codes = append(codes, m[1])
	}
	for _, c := range codes {
		if c == "401" || c == "403" {
			return Verdict{Outcome: KeyProblem, Reason: "HTTP " + c + ": the key was refused. That is a key problem, not a quota"}
		}
	}
	for _, c := range codes {
		if c == "429" || c == "529" || c == "503" {
			return out("HTTP "+c, text, now)
		}
	}
	for _, t := range OutTexts {
		if strings.Contains(low, t) {
			return out(t, text, now)
		}
	}
	for _, t := range connTexts {
		if strings.Contains(low, t) {
			return out("cannot connect", text, now)
		}
	}
	if dialTimeRe.MatchString(text) {
		return out("cannot connect", text, now)
	}
	return Verdict{Outcome: NotOut}
}

func out(reason, text string, now time.Time) Verdict {
	return Verdict{Outcome: Out, Reason: reason, Reset: resetOf(text, now)}
}

// resetOf finds the time a reply names for the end of the limit. Retry-After
// wins; else the earliest of the rate-limit reset headers; else a Claude Code
// epoch after the limit text. A time that is not after now is ignored, and
// one past MaxNamedReset is cut to it.
func resetOf(text string, now time.Time) time.Time {
	var cands []time.Time
	if m := retryAfter.FindStringSubmatch(text); m != nil {
		if t, ok := parseDelayOrDate(strings.TrimSpace(m[1]), now); ok {
			return clampReset(t, now)
		}
	}
	for _, m := range anthReset.FindAllStringSubmatch(text, -1) {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			cands = append(cands, t)
		}
	}
	for _, m := range oaiReset.FindAllStringSubmatch(text, -1) {
		if t, ok := parseDelayOrDate(m[1], now); ok {
			cands = append(cands, t)
		}
	}
	if m := epochReset.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			cands = append(cands, time.Unix(n, 0))
		}
	}
	var best time.Time
	for _, t := range cands {
		if !t.After(now) {
			continue
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return clampReset(best, now)
}

func clampReset(t, now time.Time) time.Time {
	t = t.Truncate(time.Second)
	if t.IsZero() || !t.After(now) {
		return time.Time{}
	}
	if max := now.Add(MaxNamedReset); t.After(max) {
		return max
	}
	return t.UTC()
}

// parseDelayOrDate reads a delay in seconds ("90", "1.5"), a Go-style
// duration ("6m0s"), or an HTTP date.
func parseDelayOrDate(s string, now time.Time) (time.Time, bool) {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return time.Time{}, false
		}
		if f > 1e9 { // an epoch, not a delay
			return time.Unix(int64(f), 0), true
		}
		return now.Add(time.Duration(f * float64(time.Second))), true
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d), true
	}
	if t, err := time.Parse(http.TimeFormat, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// Cool-down bounds. A cool-down with no named reset starts at CoolStart and
// doubles on each repeat to CoolMax; a success starts it over.
const (
	CoolStart = 15 * time.Minute
	CoolMax   = 2 * time.Hour
)

// FailoverFile is the file in the log dir that holds the chain's state. The
// server writes it; `coppice foreman status` reads it.
const FailoverFile = "failover.json"

// ProfileName is how a profile reads to the operator: Claude Code for the
// empty name and "default".
func ProfileName(p string) string {
	if p == "" || p == "default" {
		return "Claude"
	}
	return p
}

type slot struct {
	profile string
	until   time.Time
	repeats int
	reason  string
	failing string
}

// Chain is the ordered turn profiles with the state of each. It is safe for
// concurrent use.
type Chain struct {
	mu       sync.Mutex
	slots    []slot
	lastRan  int
	loadNote string
}

// LoadNote is the one note about a state file that held a bad end time, or
// "". The server logs it once.
func (c *Chain) LoadNote() string { c.mu.Lock(); defer c.mu.Unlock(); return c.loadNote }

// NewChain makes a chain of profiles, all ready, none run yet.
func NewChain(profiles []string) *Chain {
	c := &Chain{lastRan: -1}
	for _, p := range profiles {
		c.slots = append(c.slots, slot{profile: p})
	}
	return c
}

// Len is the number of profiles.
func (c *Chain) Len() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.slots) }

// Profile is the name of profile i, as the config has it.
func (c *Chain) Profile(i int) string { c.mu.Lock(); defer c.mu.Unlock(); return c.slots[i].profile }

// First is the first profile that is not cooling down at now, or -1.
func (c *Chain) First(now time.Time) int { return c.Next(-1, now) }

// Next is the first profile after i that is not cooling down at now, or -1.
func (c *Chain) Next(i int, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for j := i + 1; j < len(c.slots); j++ {
		if !c.slots[j].until.After(now) {
			return j
		}
	}
	return -1
}

// Earliest is the soonest end of a cool-down, for when every profile is out.
func (c *Chain) Earliest(now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best time.Time
	for _, s := range c.slots {
		if s.until.After(now) && (best.IsZero() || s.until.Before(best)) {
			best = s.until
		}
	}
	return best
}

// MarkOut cools profile i down and returns when the cool-down ends: at the
// verdict's reset when it has one, else CoolStart doubled by each repeat up
// to CoolMax.
func (c *Chain) MarkOut(i int, v Verdict, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.slots[i]
	d := CoolStart
	for n := 0; n < s.repeats && d < CoolMax; n++ {
		d *= 2
	}
	if d > CoolMax {
		d = CoolMax
	}
	s.repeats++
	s.until = now.Add(d)
	if !v.Reset.IsZero() {
		s.until = v.Reset
	}
	s.until = s.until.UTC().Truncate(time.Second)
	s.reason, s.failing = v.Reason, ""
	return s.until
}

// MarkOK clears everything about profile i: a success starts the doubling
// over.
func (c *Chain) MarkOK(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slots[i] = slot{profile: c.slots[i].profile}
}

// MarkFailing records that the last turn on profile i failed for a reason
// that is not an outage. The chain does not move for it.
func (c *Chain) MarkFailing(i int, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slots[i].failing = SafeReason(reason)
}

// MarkRan records that the last turn ran on profile i.
func (c *Chain) MarkRan(i int) { c.mu.Lock(); defer c.mu.Unlock(); c.lastRan = i }

// LastRan is the profile the last turn ran on, or -1.
func (c *Chain) LastRan() int { c.mu.Lock(); defer c.mu.Unlock(); return c.lastRan }

// StatusRow is one profile of `coppice foreman status --json`.
type StatusRow struct {
	Profile string `json:"profile"`
	Name    string `json:"name"`
	// State is "ready", "cooling" (until Until) or "failing" (Reason).
	State  string `json:"state"`
	Until  string `json:"until,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Status lists the chain with each profile's state at now.
func (c *Chain) Status(now time.Time) []StatusRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := make([]StatusRow, len(c.slots))
	for i, s := range c.slots {
		r := StatusRow{Profile: s.profile, Name: ProfileName(s.profile), State: "ready"}
		switch {
		case s.until.After(now):
			r.State, r.Until, r.Reason = "cooling", s.until.UTC().Format(DateLayout), SafeReason(s.reason)
		case s.failing != "":
			r.State, r.Reason = "failing", SafeReason(s.failing)
		}
		rows[i] = r
	}
	return rows
}

type statusDoc struct {
	Chain   []StatusRow `json:"chain"`
	LastRan string      `json:"last_ran,omitempty"`
}

// StatusJSON is the status as one JSON line.
func (c *Chain) StatusJSON(now time.Time) []byte {
	d := statusDoc{Chain: c.Status(now)}
	if i := c.LastRan(); i >= 0 {
		d.LastRan = ProfileName(c.Profile(i))
	}
	b, _ := json.Marshal(d)
	return append(b, '\n')
}

// fileRow is a StatusRow with the doubling count, which only the file keeps.
type fileRow struct {
	StatusRow
	Repeats int `json:"repeats,omitempty"`
}

type fileDoc struct {
	Chain   []fileRow `json:"chain"`
	LastRan string    `json:"last_ran,omitempty"`
}

// Save writes the chain's state to FailoverFile in dir, 0600, by a rename.
func (c *Chain) Save(dir string, now time.Time) error {
	rows := c.Status(now)
	c.mu.Lock()
	d := fileDoc{}
	for i, r := range rows {
		// The end time stays after the cool-down ends, so a reload sees it end.
		r.Until, r.Reason = "", ""
		sl := c.slots[i]
		if !sl.until.IsZero() {
			r.Until, r.Reason = sl.until.UTC().Format(DateLayout), sl.reason
		}
		if sl.failing != "" {
			r.Reason = sl.failing
		}
		d.Chain = append(d.Chain, fileRow{r, sl.repeats})
	}
	if c.lastRan >= 0 {
		d.LastRan = ProfileName(c.slots[c.lastRan].profile)
	}
	c.mu.Unlock()
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".failover-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FailoverFile))
}

// LoadChain reads the state file in dir for profiles. A missing file, a file
// that does not read, or one for a different turn chain gives a fresh chain:
// the state is a convenience, never a reason to refuse to start. An end time
// that does not parse, or lies more than MaxNamedReset after now, is dropped
// (the profile counts as ready) and LoadNote says so; a past one is ready.
func LoadChain(dir string, profiles []string, now time.Time) (*Chain, error) {
	c := NewChain(profiles)
	data, err := os.ReadFile(filepath.Join(dir, FailoverFile))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	var d fileDoc
	if json.Unmarshal(data, &d) != nil || len(d.Chain) != len(profiles) {
		return c, nil
	}
	for i, r := range d.Chain {
		if r.Profile != profiles[i] {
			return NewChain(profiles), nil
		}
	}
	var bad []string
	defer func() {
		if len(bad) > 0 {
			c.loadNote = "The foreman's saved end time for " + strings.Join(bad, " and ") +
				" was not a time within 7 days, so it was dropped and the account counts as ready."
		}
	}()
	for i, r := range d.Chain {
		s := &c.slots[i]
		s.repeats, s.reason = r.Repeats, r.Reason
		if r.Until != "" {
			t, err := time.Parse(DateLayout, r.Until)
			if err != nil || t.After(now.Add(MaxNamedReset)) {
				s.repeats, s.reason = 0, ""
				bad = append(bad, "account "+ProfileName(profiles[i]))
			} else {
				s.until = t
			}
		}
		if r.State == "failing" {
			s.failing = r.Reason
			s.reason = ""
		}
	}
	for i, p := range profiles {
		if d.LastRan != "" && ProfileName(p) == d.LastRan {
			c.lastRan = i
			break
		}
	}
	return c, nil
}

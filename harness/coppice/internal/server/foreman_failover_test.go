package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opendaisugi/coppice/internal/foreman"
)

// stepRunner stands in for the profiles of a chain. Each profile has a list
// of steps; each turn on that profile takes the next step (the last repeats).
type stepRunner struct {
	mu    sync.Mutex
	steps map[string][]func(in TurnInput) (TurnResult, error)
	seen  []string // the profile of each run, in order
	n     map[string]int
}

func okStep(text string) func(TurnInput) (TurnResult, error) {
	return func(TurnInput) (TurnResult, error) { return TurnResult{Text: text}, nil }
}

func errStep(msg string) func(TurnInput) (TurnResult, error) {
	return func(TurnInput) (TurnResult, error) { return TurnResult{}, errors.New(msg) }
}

// toolThenErrStep calls one tool of the turn's socket, as a model would,
// then fails.
func toolThenErrStep(msg string) func(TurnInput) (TurnResult, error) {
	return func(in TurnInput) (TurnResult, error) {
		in.Started(os.Getpid())
		conn, err := net.Dial("unix", in.Socket)
		if err != nil {
			return TurnResult{}, err
		}
		defer conn.Close()
		b, _ := json.Marshal(foreman.ToolRequest{Tool: "status"})
		_, _ = conn.Write(append(b, '\n'))
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		return TurnResult{}, errors.New(msg)
	}
}

func (r *stepRunner) runnerFor(profile string) TurnRunner { return profileRunner{r, profile} }

type profileRunner struct {
	r       *stepRunner
	profile string
}

func (p profileRunner) Run(ctx context.Context, in TurnInput) (TurnResult, error) {
	p.r.mu.Lock()
	steps := p.r.steps[p.profile]
	if p.r.n == nil {
		p.r.n = map[string]int{}
	}
	k := p.r.n[p.profile]
	p.r.n[p.profile]++
	p.r.seen = append(p.r.seen, p.profile)
	p.r.mu.Unlock()
	if len(steps) == 0 {
		return TurnResult{}, errors.New("no step for " + p.profile)
	}
	if k >= len(steps) {
		k = len(steps) - 1
	}
	return steps[k](in)
}

func (r *stepRunner) ran() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.seen, ",")
}

// failoverServer is a chat-mode server with a chain and scripted profiles.
// Its sleep moves the server's clock instead of waiting.
type failoverServer struct {
	*turnServer
	r      *stepRunner
	sleeps []time.Duration
}

func newFailoverServer(t *testing.T, chain []string, steps map[string][]func(TurnInput) (TurnResult, error)) *failoverServer {
	t.Helper()
	ts := newTurnServer(t, func(f *ForemanConfig) { f.TurnProfiles = chain; f.TurnModel = "desk-model" })
	fs := &failoverServer{turnServer: ts, r: &stepRunner{steps: steps}}
	ts.turns.runnerFor = fs.r.runnerFor
	ts.turns.sleep = func(ctx context.Context, d time.Duration) error {
		fs.sleeps = append(fs.sleeps, d)
		ts.clockSkew.Add(int64(d))
		return nil
	}
	return fs
}

// turn says text and waits for that turn to end.
func (fs *failoverServer) turn(t *testing.T, n int, text string) {
	t.Helper()
	fs.say(t, text)
	waitUntil(t, "the turn", 30*time.Second, fs.turnsDone(n))
}

// texts are the kind and text of each log message after the first n.
func (fs *failoverServer) tail(t *testing.T, from int) []string {
	t.Helper()
	var out []string
	for _, m := range fs.logMessages(t)[from:] {
		out = append(out, m.Kind+": "+m.Text)
	}
	return out
}

func clockOf(fs *failoverServer, d time.Duration) string { return clockText(fs.clock().Add(d)) }

func TestAnOutProfileMovesTheTurnOnAndTheChatSaysSo(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("sprig ended with an error: anthropic api 429: slow down")},
		"desk": {okStep("hello from the desk")},
	})
	at := fs.clock()
	fs.turn(t, 1, "hi")
	if got := fs.r.ran(); got != ",desk" {
		t.Fatalf("ran on %q", got)
	}
	want := []string{
		"user: hi",
		"foreman: hello from the desk",
		"note: account Claude is out until " + clockText(at.Add(15*time.Minute).Truncate(time.Second)) + "; this turn ran on account desk.",
	}
	got := fs.tail(t, 0)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || !strings.HasPrefix(got[2], "note: account Claude is out until ") ||
		!strings.HasSuffix(got[2], "; this turn ran on account desk.") {
		t.Fatalf("log = %q, want like %q", got, want)
	}
}

func TestTheNextTurnStaysOnTheFallbackWithoutANoteAndThenGoesBack(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x"), okStep("claude is back")},
		"desk": {okStep("desk one"), okStep("desk two")},
	})
	fs.turn(t, 1, "one")
	n := len(fs.logMessages(t))
	fs.turn(t, 2, "two") // Claude still cools: not tried, no note
	if got := fs.r.ran(); got != ",desk,desk" {
		t.Fatalf("ran on %q", got)
	}
	if got := fs.tail(t, n); len(got) != 2 {
		t.Fatalf("a second turn on the fallback logged %q", got)
	}
	fs.clockSkew.Add(int64(16 * time.Minute)) // the cool-down ends
	n = len(fs.logMessages(t))
	fs.turn(t, 3, "three")
	if got := fs.r.ran(); got != ",desk,desk," {
		t.Fatalf("ran on %q", got)
	}
	got := fs.tail(t, n)
	if len(got) != 3 || got[1] != "foreman: claude is back" || got[2] != "note: account Claude is back; this turn ran on account Claude." {
		t.Fatalf("log = %q", got)
	}
}

func TestAKeyProblemFailsTheTurnAndDoesNotMove(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("sprig ended with an error: anthropic api 401: invalid x-api-key")},
		"desk": {okStep("never")},
	})
	fs.turn(t, 1, "hi")
	if got := fs.r.ran(); got != "" {
		t.Fatalf("ran on %q", got)
	}
	got := fs.tail(t, 0)
	if len(got) != 2 || !strings.Contains(got[1], "401") || !strings.Contains(got[1], "key problem") ||
		!strings.Contains(got[1], "did not move") || strings.Contains(got[1], "ended with an error") {
		t.Fatalf("log = %q", got)
	}
	st := string(fs.turns.chain.StatusJSON(fs.clock()))
	if !strings.Contains(st, `"state":"failing"`) || !strings.Contains(st, "401") {
		t.Fatalf("status = %s", st)
	}
}

func TestANotOutFailureDoesNotMove(t *testing.T) {
	for _, msg := range []string{"sprig ended with an error: openai api 400: bad request", "claude reported an error: the model refused", "a tool failed"} {
		fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
			"": {errStep(msg)}, "desk": {okStep("never")},
		})
		fs.turn(t, 1, "hi")
		if got := fs.r.ran(); got != "" {
			t.Fatalf("%q: ran on %q", msg, got)
		}
		if got := fs.tail(t, 0); len(got) != 2 || !strings.HasPrefix(got[1], "note: The foreman's turn failed: ") {
			t.Fatalf("%q: log = %q", msg, got)
		}
	}
}

func TestATurnThatRanToolsBeforeItWentOutIsNotReplayed(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {toolThenErrStep("anthropic api 429: x")},
		"desk": {okStep("never")},
	})
	fs.turn(t, 1, "hi")
	if got := fs.r.ran(); got != "" {
		t.Fatalf("ran on %q", got)
	}
	var note string
	for _, l := range fs.tail(t, 0) {
		if strings.HasPrefix(l, "note: The foreman's turn failed: ") {
			note = l
		}
	}
	if !strings.Contains(note, "after it had used tools") || !strings.Contains(note, "not run again") {
		t.Fatalf("log = %q", fs.tail(t, 0))
	}
	// The profile still cools, so the next turn moves on.
	fs.r.steps[""] = []func(TurnInput) (TurnResult, error){okStep("never")}
	fs.turn(t, 2, "again")
	if got := fs.r.ran(); got != ",desk" {
		t.Fatalf("second turn ran on %q", got)
	}
}

func TestWhenEveryProfileIsOutTheTurnWaitsForTheEarliestEnd(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x\nretry-after: 1200"), okStep("claude answers")},
		"desk": {errStep("dial tcp 100.64.0.2:8080: connect: connection refused"), okStep("late desk")},
	})
	fs.turn(t, 1, "hi")
	if len(fs.sleeps) != 1 || fs.sleeps[0] < 14*time.Minute || fs.sleeps[0] > 15*time.Minute+time.Second {
		t.Fatalf("sleeps = %v", fs.sleeps)
	}
	// desk (15 min) ends before Claude (20 min): the turn runs on desk.
	if got := fs.r.ran(); got != ",desk,desk" {
		t.Fatalf("ran on %q", got)
	}
	log := strings.Join(fs.tail(t, 0), "\n")
	if strings.Count(log, "The foreman is paused: every account is out") != 1 || !strings.Contains(log, "foreman: late desk") {
		t.Fatalf("log = %s", log)
	}
}

func TestTheStateFileListsTheChainForTheStatusCommand(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("anthropic api 529: overloaded")}, "desk": {okStep("ok")},
	})
	fs.turn(t, 1, "hi")
	var out strings.Builder
	var errw strings.Builder
	if code := foreman.Main([]string{"--data-dir", fs.logDir, "status", "--json"}, "", &out, &errw); code != 0 {
		t.Fatalf("status: %d %s", code, errw.String())
	}
	s := out.String()
	if !strings.Contains(s, `"name":"Claude","state":"cooling"`) || !strings.Contains(s, `"reason":"HTTP 529"`) ||
		!strings.Contains(s, `"state":"ready"`) || !strings.HasSuffix(s, `"last_ran":"desk"}`+"\n") {
		t.Fatalf("status = %s", s)
	}
	if _, err := os.Stat(filepath.Join(fs.logDir, foreman.FailoverFile)); err != nil {
		t.Fatal(err)
	}
}

func TestTheStackBarFactsNameTheProfileTheLastTurnRanOn(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("anthropic api 429: x")}, "desk": {okStep("ok")},
	})
	if _, ok := fs.lastTurnProfile(); ok {
		t.Fatal("a profile before any turn")
	}
	fs.turn(t, 1, "hi")
	if name, ok := fs.lastTurnProfile(); !ok || name != "desk" {
		t.Fatalf("last turn profile = %q %v", name, ok)
	}
	r := result(t, roundTrip(t, fs.Server, `{"id":"1","cmd":"floor.facts"}`)[0])
	ft, _ := r["foreman_turn"].(map[string]any)
	if ft["ran_on"] != "desk" {
		t.Fatalf("facts = %v", r)
	}
}

func TestAOneAccountTurnChainPausesWhenItsAccountIsOut(t *testing.T) {
	fs := newFailoverServer(t, []string{""}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("anthropic api 429: x"), okStep("fine")},
	})
	fs.sleeps = nil
	fs.turn(t, 1, "hi")
	if len(fs.sleeps) != 1 || fs.r.ran() != "," {
		t.Fatalf("sleeps %v ran %q", fs.sleeps, fs.r.ran())
	}
	log := strings.Join(fs.tail(t, 0), "\n")
	if !strings.Contains(log, "account Claude until") || !strings.Contains(log, "foreman: fine") {
		t.Fatalf("log = %s", log)
	}
}

func TestAChainOfTwoModelsIsRefusedAndNamesBoth(t *testing.T) {
	a, b, bad := modelMismatch([]string{"", "work", "api"}, "opus", map[string]string{"work": "opus", "api": "sonnet"})
	if !bad || a != "opus" || b != "sonnet" {
		t.Fatalf("mismatch = %q %q %v", a, b, bad)
	}
	if _, _, bad := modelMismatch([]string{"", "work"}, "opus", map[string]string{"work": "opus"}); bad {
		t.Fatal("one model was refused")
	}
	// A profile that names no model takes the turn model.
	if _, _, bad := modelMismatch([]string{"work"}, "opus", map[string]string{}); bad {
		t.Fatal("a profile with no model was refused")
	}
}

func TestASecondAllOutPassWritesANoteAndNamesTheAccounts(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x"), errStep("anthropic api 429: x"), okStep("back")},
		"desk": {errStep("openai api 429: x"), errStep("openai api 429: x"), okStep("never")},
	})
	fs.turn(t, 1, "hi")
	log := strings.Join(fs.tail(t, 0), "\n")
	if strings.Count(log, "The foreman is paused") != 2 || !strings.Contains(log, "paused again (wait 2") ||
		!strings.Contains(log, "account Claude until") || !strings.Contains(log, "account desk until") ||
		!strings.Contains(log, "no longer paused") || !strings.Contains(log, "foreman: back") {
		t.Fatalf("log = %s", log)
	}
	if len(fs.sleeps) != 2 || fs.sleeps[1] <= fs.sleeps[0] {
		t.Fatalf("sleeps = %v", fs.sleeps)
	}
}

func TestAnOwnerMessageEndsThePauseAndTheTurnSaysTheAccountsAreStillOut(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x"), okStep("late")},
		"desk": {errStep("openai api 429: x")},
	})
	waiting := make(chan struct{}, 4)
	var calls int
	var mu sync.Mutex
	fs.turns.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			waiting <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		fs.clockSkew.Add(int64(d))
		return nil
	}
	fs.say(t, "first")
	select {
	case <-waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never paused")
	}
	fs.say(t, "second")
	waitUntil(t, "both turns", 30*time.Second, fs.turnsDone(2))
	log := strings.Join(fs.tail(t, 0), "\n")
	if !strings.Contains(log, "every account is still out (account Claude until") || !strings.Contains(log, "foreman: late") {
		t.Fatalf("log = %s", log)
	}
}

func TestThePauseNoteGivesTheDateWhenTheResetIsNotToday(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("claude ended with an error: Claude AI usage limit reached|" + strconv.FormatInt(time.Now().Add(3*24*time.Hour).Unix(), 10)), okStep("ok")},
		"desk": {errStep("openai api 429: x"), okStep("ok")},
	})
	fs.clockSkew.Store(0)
	fs.turn(t, 1, "hi")
	log := strings.Join(fs.tail(t, 0), "\n")
	if !strings.Contains(log, "account desk until") || !strings.Contains(log, "account Claude until "+time.Now().Add(3*24*time.Hour).UTC().Format("Jan 2")) {
		t.Fatalf("log = %s", log)
	}
}

func TestWhenTextAddsADateOnlyOffToday(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if got := whenText(now.Add(time.Hour), now); got != "13:00 UTC" {
		t.Fatal(got)
	}
	if got := whenText(now.Add(60*time.Hour), now); got != "Oct 24 00:00 UTC" {
		t.Fatal(got)
	}
}

func TestARunnersWholeErrorTextReachesTheClassifier(t *testing.T) {
	// The recorded shape of a claude usage limit on stdout with exit 1 and
	// nothing on stderr, and a pretty-printed OpenAI body.
	e1 := &runnerError{msg: "claude ended with an error: exit status 1", detail: failDetail("", `{"type":"result","is_error":true,"result":"Claude AI usage limit reached|1800003600"}`)}
	e2 := &runnerError{msg: "sprig ended with an error: }", detail: failDetail("openai api 429: {\n  \"error\": {\"code\": \"insufficient_quota\"}\n}", "")}
	for _, e := range []error{e1, e2} {
		if v := foreman.Classify(failText(e), time.Unix(1800000000, 0)); v.Outcome != foreman.Out {
			t.Fatalf("%v: %+v", e, v)
		}
	}
	if v := foreman.Classify(failText(e1), time.Unix(1800000000, 0)); v.Reset.Unix() != 1800003600 {
		t.Fatalf("reset = %v", v.Reset)
	}
	if failText(errors.New("plain")) != "plain" {
		t.Fatal("plain error text changed")
	}
}

func TestAFailingReasonInTheStatusIsMasked(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("openai api 400: bad key sk-fakefakefake1234567890")}, "desk": {okStep("never")},
	})
	fs.turn(t, 1, "hi")
	data, _ := os.ReadFile(filepath.Join(fs.logDir, foreman.FailoverFile))
	if strings.Contains(string(data), "sk-fake") || !strings.Contains(string(data), "HTTP 400") {
		t.Fatalf("file = %s", data)
	}
	var out, errw strings.Builder
	foreman.Main([]string{"--data-dir", fs.logDir, "status"}, "", &out, &errw)
	if strings.Contains(out.String(), "sk-fake") {
		t.Fatalf("status = %s", out.String())
	}
}

func TestAToolCallInFlightCountsBeforeItsRequestIsRead(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"": {func(in TurnInput) (TurnResult, error) {
			in.Started(os.Getpid())
			conn, err := net.Dial("unix", in.Socket)
			if err != nil {
				return TurnResult{}, err
			}
			defer conn.Close()
			time.Sleep(300 * time.Millisecond) // accepted, nothing sent yet
			return TurnResult{}, errors.New("anthropic api 429: x")
		}},
		"desk": {okStep("never")},
	})
	fs.turn(t, 1, "hi")
	if got := fs.r.ran(); got != "" {
		t.Fatalf("replayed: ran on %q", got)
	}
}

func TestTheSavedChainNamesTheLastAccountForAFreshLoad(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"": {errStep("anthropic api 429: x")}, "desk": {okStep("ok")},
	})
	fs.turn(t, 1, "hi")
	// A fresh server on the same log dir names the account at once.
	c, _ := foreman.LoadChain(fs.logDir, []string{"", "desk"}, fs.clock())
	if c.LastRan() != 1 {
		t.Fatalf("last ran = %d", c.LastRan())
	}
}

func TestAMixedModelTurnChainIsNotedWhenTheChatOpens(t *testing.T) {
	ts := newTurnServer(t, func(f *ForemanConfig) { f.TurnProfiles = []string{"", "desk"}; f.TurnModel = "sonnet" })
	var joined string
	waitUntil(t, "the open-time note", 30*time.Second, func() bool {
		var log []string
		for _, m := range ts.logMessages(t) {
			log = append(log, m.Kind+": "+m.Text)
		}
		joined = strings.Join(log, "\n")
		return strings.Contains(joined, "turn chain serves two models")
	})
	if strings.Count(joined, "turn chain serves two models") != 1 || !strings.Contains(joined, `"sonnet"`) || !strings.Contains(joined, `"desk-model"`) {
		t.Fatalf("log = %s", joined)
	}
}

func TestASlowDaisugiDoesNotStallTheChatOpen(t *testing.T) {
	// newFakeEnv resets FAKE_BUNDLE_HANG, so the hang is set after it runs,
	// by the tweak that newTurnServer calls once the env exists.
	start := time.Now()
	ts := newTurnServer(t, func(f *ForemanConfig) {
		f.TurnProfiles = []string{"", "desk"}
		f.TurnModel = "sonnet"
		t.Setenv("FAKE_BUNDLE_HANG", "1")
	})
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the chat open took %v with a hung daisugi", d)
	}
	// floor.talk answers at once too.
	begin := time.Now()
	ts.say(t, "hello")
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("floor.talk took %v", d)
	}
}

func TestAnOwnerMessageBetweenTheWakeHookAndThePauseNoteStillEndsThePause(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x"), okStep("late")},
		"desk": {errStep("openai api 429: x")},
	})
	var calls int
	var mu sync.Mutex
	var lost atomic.Bool
	fs.turns.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Second):
				lost.Store(true)
				return errors.New("the wake was lost: the pause ran its full wait")
			}
		}
		fs.clockSkew.Add(int64(d))
		return nil
	}
	var once sync.Once
	fs.turns.pauseSet = func() { once.Do(func() { fs.say(t, "second") }) }
	fs.say(t, "first")
	waitUntil(t, "both turns", 30*time.Second, fs.turnsDone(2))
	log := strings.Join(fs.tail(t, 0), "\n")
	if !strings.Contains(log, "every account is still out") || strings.Contains(log, "the wake was lost") || lost.Load() {
		t.Fatalf("log = %s", log)
	}
}

// runnerFails is a step that fails the way a real runner does: the one line
// for the chat holds the fake key, and so does the detail.
func runnerFails(lead, key, extra string) func(TurnInput) (TurnResult, error) {
	return func(TurnInput) (TurnResult, error) {
		return TurnResult{}, &runnerError{
			lead:   lead,
			msg:    lead + ": " + extra + " bad key " + key,
			detail: failDetail(extra+" bad key "+key+"\n"+key, "{\"key\":\""+key+"\"}"),
		}
	}
}

func TestNoRawRunnerTextReachesAnySavedOrShownPlace(t *testing.T) {
	const key = "sk-FAKEKEY-0123456789abcdef"
	cases := map[string]func(TurnInput) (TurnResult, error){
		"not out":     runnerFails("sprig ended with an error", key, "openai api 400:"),
		"key problem": runnerFails("sprig ended with an error", key, "anthropic api 401:"),
		"claude":      runnerFails("claude reported an error", key, "the model said"),
	}
	for name, step := range cases {
		var logBuf strings.Builder
		log.SetOutput(&logBuf)
		fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
			"": {step}, "desk": {okStep("never")},
		})
		fs.turn(t, 1, "hi")
		log.SetOutput(os.Stderr)
		var seen []string
		_ = filepath.Walk(fs.logDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				seen = append(seen, string(b))
			}
			return nil
		})
		fs.notesMu.Lock()
		for _, n := range fs.notes {
			seen = append(seen, n.Text)
		}
		fs.notesMu.Unlock()
		seen = append(seen, logBuf.String(), strings.Join(fs.tail(t, 0), "\n"))
		all := strings.Join(seen, "\n")
		if strings.Contains(all, "FAKEKEY") {
			t.Fatalf("%s: the key reached a saved or shown place:\n%s", name, all)
		}
		if !strings.Contains(all, "The foreman's turn failed: ") {
			t.Fatalf("%s: no failure note:\n%s", name, all)
		}
	}
}

func TestAnOwnerMessageQueuedBeforeTheWakeHookIsSetStillEndsThePause(t *testing.T) {
	fs := newFailoverServer(t, []string{"", "desk"}, map[string][]func(TurnInput) (TurnResult, error){
		"":     {errStep("anthropic api 429: x"), okStep("late")},
		"desk": {errStep("openai api 429: x")},
	})
	var calls int
	var mu sync.Mutex
	var lost atomic.Bool
	fs.turns.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Second):
				lost.Store(true)
				return errors.New("the wake was lost")
			}
		}
		fs.clockSkew.Add(int64(d))
		return nil
	}
	var once sync.Once
	fs.turns.pauseGap = func() { once.Do(func() { fs.say(t, "second") }) }
	fs.say(t, "first")
	waitUntil(t, "both turns", 30*time.Second, fs.turnsDone(2))
	if lost.Load() {
		t.Fatal("a message queued before the hook was set did not end the pause")
	}
}

func TestAnEmptyClaudeErrorReadsAsTheFixedWords(t *testing.T) {
	e := &runnerError{lead: "claude reported an error", msg: "claude reported an error: ", detail: ""}
	if got := safeTurnError(e); got != "claude reported an error: the turn failed" {
		t.Fatalf("got %q", got)
	}
}

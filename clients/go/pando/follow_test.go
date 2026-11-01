package pando

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeSource struct{ msgs []Live }

func (f *fakeSource) Name() string { return "fake" }
func (f *fakeSource) Poll(ctx context.Context, c *Chat, since time.Time) (int, error) {
	n := 0
	for _, m := range f.msgs {
		fresh, err := c.AppendLive(ctx, m)
		if err != nil {
			return n, err
		}
		if fresh {
			n++
		}
	}
	return n, nil
}

func followFixture(t *testing.T) (dir, proj string) {
	root := t.TempDir()
	proj = filepath.Join(root, "projects", "-work-proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "pando", "work"), proj
}

func bigText(n int) string { return strings.Repeat("word ", n) }

func TestFollowLogsBothSourcesOnceAndBuildsNodesWithAProfile(t *testing.T) {
	dir, proj := followFixture(t)
	now := time.Now().UTC()
	ts := now.Format(DateLayout)
	writeTranscript(t, filepath.Join(proj, "sess1.jsonl"),
		liveLine("user", "u1", ts, bigText(300)), // over a node: needs the model
		liveLine("assistant", "a1", ts, "ok"))
	var log bytes.Buffer
	fs := &fakeSummarizer{}
	o := &FollowOpts{Dir: dir, Log: &log, Summarizer: fs, Sources: []Source{
		&ClaudeSource{Root: filepath.Dir(proj)},
		&fakeSource{msgs: []Live{{Kind: KindUser, Text: "hermes user alice in mattermost: hi", Act: "hermes:p:s:1", At: now,
			Meta: Meta{Src: "hermes:p", Chat: "s", Who: "alice"}}}},
	}}
	ctx := context.Background()
	added, _, _, err := FollowOnce(ctx, o)
	if err != nil || added != 3 {
		t.Fatalf("added %d %v", added, err)
	}
	// Another cycle logs nothing new and builds the waiting nodes.
	for i := 0; i < 5; i++ {
		added, _, _, err = FollowOnce(ctx, o)
		if err != nil || added != 0 {
			t.Fatalf("cycle %d: added %d %v", i, added, err)
		}
	}
	c, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Count() != 3 || c.Pending() != 0 || fs.compress == 0 {
		t.Fatalf("count %d pending %d compress %d log %q", c.Count(), c.Pending(), fs.compress, log.String())
	}
	m, _ := c.Message(2)
	if m.Who != "alice" || m.Src != "hermes:p" {
		t.Fatalf("hermes message kept no source: %+v", m)
	}
}

func TestFollowWithNoProfileOnlyLogsAndSaysOnce(t *testing.T) {
	dir, proj := followFixture(t)
	ts := time.Now().UTC().Format(DateLayout)
	writeTranscript(t, filepath.Join(proj, "s.jsonl"), liveLine("user", "u1", ts, bigText(300)))
	var log bytes.Buffer
	o := &FollowOpts{Dir: dir, Log: &log, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	for i := 0; i < 3; i++ {
		if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(log.String(), "wait for a profile") != 1 {
		t.Fatalf("log: %q", log.String())
	}
	c, _ := OpenReadOnly(dir)
	if c.Count() != 1 || c.Pending() == 0 {
		t.Fatalf("count %d pending %d", c.Count(), c.Pending())
	}
}

func TestFollowOutageDelaysNodesAndNeverClips(t *testing.T) {
	dir, proj := followFixture(t)
	ts := time.Now().UTC().Format(DateLayout)
	writeTranscript(t, filepath.Join(proj, "s.jsonl"), liveLine("user", "u1", ts, bigText(300)))
	clock := time.Now()
	fs := &fakeSummarizer{fail: true}
	var log bytes.Buffer
	o := &FollowOpts{Dir: dir, Log: &log, Summarizer: fs, Now: func() time.Time { return clock },
		Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	if _, _, more, err := FollowOnce(context.Background(), o); err != nil || !more {
		t.Fatalf("more %v err %v", more, err)
	}
	if !strings.Contains(log.String(), "model failed") {
		t.Fatalf("log: %q", log.String())
	}
	// Inside the back-off no call is made.
	before := fs.compress
	fs.fail = false
	FollowOnce(context.Background(), o)
	if fs.compress != before {
		t.Fatal("called the model inside the back-off")
	}
	clock = clock.Add(6 * time.Minute)
	for i := 0; i < 4; i++ {
		FollowOnce(context.Background(), o)
	}
	c, _ := OpenReadOnly(dir)
	if c.Pending() != 0 || c.ClippedCount() != 0 {
		t.Fatalf("pending %d clipped %d", c.Pending(), c.ClippedCount())
	}
}

func TestTwoAppendersShareTheStore(t *testing.T) {
	dir, _ := followFixture(t)
	c, err := Open(context.Background(), Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(300 * time.Millisecond); c.Close() }()
	c2, err := OpenWait(context.Background(), Config{Dir: dir}, 5*time.Second)
	if err != nil {
		t.Fatalf("the second writer did not get in: %v", err)
	}
	c2.Close()
}

func TestProfileVerbSetsShowsAndClears(t *testing.T) {
	data := t.TempDir()
	if _, out, _ := cli(t, "", "profile", "--data-dir", data); !strings.Contains(out, "none") {
		t.Fatalf("%q", out)
	}
	if code, _, _ := cli(t, "", "profile", "pando-g12", "--data-dir", data); code != 0 {
		t.Fatal(code)
	}
	if _, out, _ := cli(t, "", "profile", "--data-dir", data); strings.TrimSpace(out) != "pando-g12" {
		t.Fatalf("%q", out)
	}
	if got := ReadProfile(data, func(string) string { return "from-env" }); got != "from-env" {
		t.Fatalf("env must win: %q", got)
	}
	cli(t, "", "profile", "none", "--data-dir", data)
	if ReadProfile(data, nil) != "" {
		t.Fatal("not cleared")
	}
}

func TestFollowVerbOnceLogsAClaudeChat(t *testing.T) {
	data := t.TempDir()
	cdir := filepath.Join(t.TempDir(), "projects", "p")
	os.MkdirAll(cdir, 0o700)
	writeTranscript(t, filepath.Join(cdir, "s.jsonl"), liveLine("user", "u1", time.Now().UTC().Format(DateLayout), "hello store"))
	code, out, errs := cli(t, "", "follow", "--once", "--claude-dir", filepath.Dir(cdir), "--data-dir", data)
	if code != 0 || !strings.Contains(out, "logged: 1") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if code, out, _ := cli(t, "", "log", "--data-dir", data); code != 0 || !strings.Contains(out, "hello store") {
		t.Fatalf("log: %q", out)
	}
}

// Round 7 fix: each source keeps its own time, so a source that fails does
// not stall the others and is not read from the start for ever.

type failingSource struct{ calls int }

func (f *failingSource) Name() string { return "bad" }
func (f *failingSource) Poll(ctx context.Context, c *Chat, since time.Time) (int, error) {
	f.calls++
	return 0, os.ErrPermission
}

func TestAFailingSourceKeepsItsOwnTimeAndDoesNotStallTheOthers(t *testing.T) {
	dir, _ := followFixture(t)
	now := time.Now().UTC()
	clock := now
	var log bytes.Buffer
	good := &fakeSource{msgs: []Live{{Kind: KindUser, Text: "ok", Act: "g:1", At: now, Meta: Meta{Src: "g"}}}}
	bad := &failingSource{}
	o := &FollowOpts{Dir: dir, Log: &log, Now: func() time.Time { return clock }, Sources: []Source{good, bad}}
	if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Minute)
	if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	st := readFollowState(dir)
	gs, bs := st.Sources["fake"], st.Sources["bad"]
	gt, _ := time.Parse(DateLayout, gs.Since)
	bt, _ := time.Parse(DateLayout, bs.Since)
	if !gt.After(bt) || bt.After(now.Add(time.Second)) {
		t.Fatalf("the good source moves (%s), the bad one stays at the first start (%s)", gs.Since, bs.Since)
	}
	if !strings.Contains(log.String(), "follow: bad:") {
		t.Fatalf("log: %q", log.String())
	}
}

func TestAnErrorLineIsOneShortLineAndNeverHoldsTheStoreText(t *testing.T) {
	long := strings.Repeat("secret store text ", 100)
	got := clipErr(&CallError{Msg: "the model said:\n" + long, Detail: long})
	if strings.Contains(got, "\n") || len([]rune(got)) > 200 {
		t.Fatalf("not clipped: %d %q", len(got), got)
	}
}

func TestAllGoesOnlyWithFollowOrHermesImport(t *testing.T) {
	data := t.TempDir()
	seedStore(t, data, "work", "x")
	if code, _, errs := cli(t, "", "status", "--all", "--data-dir", data); code != 2 || !strings.Contains(errs, "--all goes with") {
		t.Fatalf("%d %q", code, errs)
	}
	if code, _, errs := cli(t, "", "follow", "--once", "--all", "--hermes-profile", "x", "--data-dir", data); code != 2 || !strings.Contains(errs, "exclude each other") {
		t.Fatalf("%d %q", code, errs)
	}
}

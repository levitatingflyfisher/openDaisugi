package pando

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The layout of the first real lead on the box (2026-11-07): the steward's
// own session claimed for the store, a dir route for the lead's clone, the
// lead's minted id and transcript path, and the store above already holding
// the lead's first five lines, logged by a follow service that still ran
// the binary from before branches. The rest of the lead's chat must go to
// its branch; before the fix every later line went to the store above.
func TestALeadsChatLoggedEarlyByTheRootStillGoesToItsBranch(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home", "user")
	data := filepath.Join(home, ".opendaisugi")
	store := filepath.Join(data, "pando", "work")
	clone := filepath.Join(home, "code", "PrimingTrellis-worktrees", "lead-trellis")
	projects := filepath.Join(home, ".claude", "projects")
	leadProj := filepath.Join(projects, "-home-user-code-PrimingTrellis-worktrees-lead-trellis")
	stewardProj := filepath.Join(projects, "-home-user--local-state-coppice-steward")
	for _, d := range []string{clone, leadProj, stewardProj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sid := "03f0346d-09b0-444f-b49c-4d8c257f59cc"
	steward := "b2284ee4-b877-4559-8694-b649cd88b33f"
	tr := filepath.Join(leadProj, sid+".jsonl")
	line := func(typ, uuid, ts, text string) string {
		b, _ := json.Marshal(map[string]any{"type": typ, "uuid": uuid, "sessionId": sid, "cwd": clone, "timestamp": ts,
			"message": map[string]any{"role": typ, "content": []map[string]string{{"type": "text", "text": text}}}})
		return string(b)
	}
	early := []string{
		line("user", "u1", "2026-11-07T05:16:30Z", "You are the Trellis 1.5.9 controller (a long-running lead)."),
		line("assistant", "a1", "2026-11-07T05:16:32Z", "I'll start by orienting."),
		line("assistant", "a2", "2026-11-07T05:16:53Z", "Kicking off the approved emulator downloads."),
	}
	os.WriteFile(tr, []byte(strings.Join(early, "\n")+"\n"), 0o600)

	// The old follow service: no routing, the lead's first lines in the root.
	c, err := Open(ctx, Config{Dir: store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.IngestClaude(ctx, tr, nil); err != nil {
		t.Fatal(err)
	}
	c.Close()
	// The map as coppice and the hooks wrote it.
	os.MkdirAll(filepath.Join(store, BranchesDir, "trellis"), 0o700)
	if _, err := EnsureBranch(store, "trellis", nil); err != nil {
		t.Fatal(err)
	}
	m := steward + "\t-\n" + filepath.Join(stewardProj, steward+".jsonl") + "\t-\n" +
		DirRoutePrefix + clone + "\ttrellis\n" + sid + "\ttrellis\n" + tr + "\ttrellis\n"
	os.WriteFile(filepath.Join(store, RouteFile), []byte(m), 0o600)
	if !RootHolds(store, sid) {
		t.Fatal("the layout is wrong: the root does not hold the lead's first lines")
	}
	rootBefore := messagesOf(t, store)

	// The lead goes on; the new follow service runs.
	later := append(early, line("assistant", "a3", "2026-11-07T05:17:34Z", "The app expects sibling repos two levels up."),
		line("assistant", "a4", "2026-11-07T05:20:00Z", "Emulator image is ready."))
	os.WriteFile(tr, []byte(strings.Join(later, "\n")+"\n"), 0o600)
	o := &FollowOpts{Dir: store, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: projects}},
		Now: func() time.Time { return time.Now() }}
	if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(t, store); got != rootBefore {
		t.Fatalf("the store above took more of the lead's chat:\n%s", strings.TrimPrefix(got, rootBefore))
	}
	got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis"))
	if !strings.Contains(got, "sibling repos two levels up") || !strings.Contains(got, "Emulator image is ready") {
		t.Fatalf("branch: %q", got)
	}
}

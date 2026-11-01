package pando

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F2: a forged SessionStart from a branch cannot claim a chat the root
// already holds, and the owner can list and take off a route.
func TestAForgedSessionStartCannotClaimTheOwnersChat(t *testing.T) {
	data, store := branchFixture(t)
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	tr := claudeTranscript(t, proj, "owner1", [2]string{"user", "owner first words at the root"})
	ev := func(name string) string {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "owner1", "transcript_path": tr})
		return string(b)
	}
	cliEnv(t, ev("Stop"), nil, "hook", "--data-dir", data) // the owner's chat is in the root
	if !RootHolds(store, "owner1") {
		t.Fatal("RootHolds is false after the owner's Stop")
	}
	cliEnv(t, ev("SessionStart"), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	if b := LoadRoutes(store).For("owner1", tr); b != "" {
		t.Fatalf("a branch claimed the owner's session: %q", b)
	}
	if err := AddRoute(store, "trellis", "owner1"); err == nil {
		t.Fatal("AddRoute took a session the root holds")
	}
	// A line written by hand is not followed either.
	f, _ := os.OpenFile(filepath.Join(store, RouteFile), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	f.WriteString("owner1\ttrellis\n")
	f.Close()
	claudeTranscript(t, proj, "owner1", [2]string{"user", "owner first words at the root"}, [2]string{"user", "owner second words"})
	cliEnv(t, ev("Stop"), nil, "hook", "--data-dir", data)
	if got := messagesOf(t, store); !strings.Contains(got, "owner second words") {
		t.Fatalf("root: %s", got)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); strings.Contains(got, "owner") {
		t.Fatalf("the branch got the owner's words: %s", got)
	}
	// Hermes the same way.
	c, _ := Open(ctx, Config{Dir: store, Now: newClock().now})
	c.AppendLive(ctx, Live{Kind: KindUser, Text: "spouse row", Act: "hermes:default:sp1:1", At: time.Now()})
	c.Close()
	cliEnv(t, "", map[string]string{BranchVar: "trellis"}, "context", "--session", "sp1", "--data-dir", data)
	if _, ok := LoadRoutes(store)["hermes:sp1"]; ok {
		t.Fatal("a branch claimed the spouse's Hermes session")
	}
	// routes and unroute, the owner's only.
	code, out, _ := cliEnv(t, "", nil, "routes", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "owner1\ttrellis") {
		t.Fatalf("routes: %d %s", code, out)
	}
	code, _, errs := cliEnv(t, "", map[string]string{BranchVar: "trellis"}, "unroute", "owner1", "--data-dir", data)
	if code != 1 || !strings.Contains(errs, "a branch of the memory") {
		t.Fatalf("unroute under a marker: %d %s", code, errs)
	}
	code, out, _ = cliEnv(t, "", nil, "unroute", "owner1", "--data-dir", data)
	if code != 0 || !strings.Contains(out, "unrouted: owner1") {
		t.Fatalf("unroute: %d %s", code, out)
	}
	if _, ok := LoadRoutes(store)["owner1"]; ok {
		t.Fatal("the route stayed")
	}
	if code, _, _ := cliEnv(t, "", nil, "unroute", "owner1", "--data-dir", data); code != 1 {
		t.Fatal("unroute of a missing id did not fail")
	}
}

// F3: a dir route sends every session whose cwd is under the lead's
// worktree to its branch, a `claude -p` of the lead's included.
func TestADirRouteSendsTheWorktreesSessionsToTheBranch(t *testing.T) {
	_, store := branchFixture(t)
	wt := filepath.Join(t.TempDir(), "trellis-worktrees", "lead-trellis")
	if err := AddRoute(store, "trellis", DirRoutePrefix+wt); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(t.TempDir(), "projects", "-wt")
	p := filepath.Join(proj, "cp1.jsonl")
	os.MkdirAll(proj, 0o700)
	line := strings.Replace(strings.Replace(liveLine("user", "cp1-a", "2026-11-05T10:00:00Z", "draft the release notes"), `"sess1"`, `"cp1"`, 1),
		`"cwd":"/work/proj"`, `"cwd":`+strings.ReplaceAll(jsonQ(filepath.Join(wt, "sub")), `\`, `\\`), 1)
	os.WriteFile(p, []byte(line+"\n"), 0o600)
	claudeTranscript(t, proj, "owner2", [2]string{"user", "the owner elsewhere"})
	o := &FollowOpts{Dir: store, Log: &bytes.Buffer{}, Sources: []Source{&ClaudeSource{Root: filepath.Dir(proj)}}}
	if _, _, _, err := FollowOnce(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(t, store); strings.Contains(got, "release notes") || !strings.Contains(got, "owner elsewhere") {
		t.Fatalf("root: %s", got)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); !strings.Contains(got, "release notes") {
		t.Fatalf("branch: %s", got)
	}
	if LoadRoutes(store).ForCwd(wt+"-other") != "" {
		t.Fatal("a sibling dir matched the route")
	}
}

func jsonQ(s string) string { b, _ := json.Marshal(s); return string(b) }

// F5: a message, a worker or a row never opens a closed branch; only
// ReopenBranch does.
func TestAClosedBranchStaysClosed(t *testing.T) {
	_, store := branchFixture(t)
	CloseBranch(store, "trellis", nil)
	if _, err := EnsureBranch(store, "trellis", nil); err != nil {
		t.Fatal(err)
	}
	d, _ := ExistingBranch(store, "trellis")
	if bi, _ := ReadBranch(d); bi.State != BranchClosed {
		t.Fatalf("EnsureBranch opened it: %+v", bi)
	}
	if _, err := ReopenBranch(store, "trellis", nil); err != nil {
		t.Fatal(err)
	}
	if bi, _ := ReadBranch(d); bi.State != BranchOpen || bi.Closed != "" {
		t.Fatalf("ReopenBranch: %+v", bi)
	}
}

// F6 and F7.
func TestWorkerRefusalAndViewBranchLines(t *testing.T) {
	data, _ := branchFixture(t)
	_, _, errs := cliEnv(t, "", map[string]string{SkipVar: "1"}, "stores", "--data-dir", data)
	if !strings.Contains(errs, "this process is a worker of the memory; stores") || strings.Contains(errs, "()") {
		t.Fatalf("worker refusal: %q", errs)
	}
	_, out, _ := cliEnv(t, "", nil, "view", "--data-dir", data)
	if !strings.Contains(out, BranchLinesHead) || !strings.Contains(out, "- trellis: ") {
		t.Fatalf("view: %s", out)
	}
	_, out, _ = cliEnv(t, "", nil, "view", "--json", "--data-dir", data)
	if strings.Contains(out, "Branches below") {
		t.Fatalf("view --json carries the text lines: %s", out)
	}
}

// G5: the owner's SessionStart claims the session for the store before a
// branch can map it, and the importer still takes the owner's sessions.
func TestTheOwnersSessionStartClaimsItsSession(t *testing.T) {
	data, store := branchFixture(t)
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	tr := claudeTranscript(t, proj, "fresh1", [2]string{"user", "a brand new owner chat"})
	ev := func(name string) string {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "fresh1", "transcript_path": tr})
		return string(b)
	}
	cliEnv(t, ev("SessionStart"), nil, "hook", "--data-dir", data)
	if got := LoadRoutes(store)["fresh1"]; got != RootClaim {
		t.Fatalf("no root claim: %q", got)
	}
	cliEnv(t, ev("SessionStart"), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	if got := LoadRoutes(store)["fresh1"]; got != RootClaim {
		t.Fatalf("a branch took a claimed session: %q", got)
	}
	cliEnv(t, ev("Stop"), nil, "hook", "--data-dir", data)
	if got := messagesOf(t, store); !strings.Contains(got, "brand new owner chat") {
		t.Fatalf("root: %s", got)
	}
	if skipWithRoutes(store)["fresh1"] {
		t.Fatal("the importer would skip the owner's claimed session")
	}
}

// H2: a branch line for an owner session the store never logged (a lead
// read the id elsewhere) loses to the owner's unmarked SessionStart, unless
// the session runs under that branch's dir route. H5: a dir: id is never
// claimed.
func TestTheOwnersClaimWinsOverAnEarlierBranchLine(t *testing.T) {
	data, store := branchFixture(t)
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	tr := claudeTranscript(t, proj, "old1", [2]string{"user", "OWNER OLD CHAT"})
	if err := AddRoute(store, "trellis", "old1"); err != nil {
		t.Fatal(err)
	}
	ev := func(name, cwd string) string {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "old1", "transcript_path": tr, "cwd": cwd})
		return string(b)
	}
	cliEnv(t, ev("SessionStart", "/work/proj"), nil, "hook", "--data-dir", data)
	if got := LoadRoutes(store)["old1"]; got != RootClaim {
		t.Fatalf("the claim lost: %q", got)
	}
	cliEnv(t, ev("Stop", "/work/proj"), nil, "hook", "--data-dir", data)
	if got := messagesOf(t, store); !strings.Contains(got, "OWNER OLD CHAT") {
		t.Fatalf("root: %s", got)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); strings.Contains(got, "OWNER") {
		t.Fatalf("branch: %s", got)
	}
	_, out, _ := cliEnv(t, "", nil, "routes", "--data-dir", data)
	if !strings.Contains(out, "old1\t-") {
		t.Fatalf("routes: %s", out)
	}
	// Under the branch's own dir route the branch line stays.
	wt := filepath.Join(t.TempDir(), "lead-trellis")
	AddRoute(store, "trellis", DirRoutePrefix+wt, "lead9")
	if err := ClaimRoot(store, filepath.Join(wt, "src"), "lead9"); err != nil {
		t.Fatal(err)
	}
	if got := LoadRoutes(store)["lead9"]; got != "trellis" {
		t.Fatalf("the claim took a session of the branch's own dir: %q", got)
	}
	// H5.
	ClaimRoot(store, "", DirRoutePrefix+"/work/x")
	if _, ok := LoadRoutes(store)[DirRoutePrefix+"/work/x"]; ok {
		t.Fatal("a dir: id was claimed")
	}
}

// J5: a session under a branch's dir route is that branch's for every
// hook: the owner's unmarked SessionStart claims neither its id nor its
// transcript path, and the owner's and the lead's Stops both log into the
// branch.
func TestASessionUnderADirRouteIsNeverClaimed(t *testing.T) {
	data, store := branchFixture(t)
	wt := filepath.Join(t.TempDir(), "lead-trellis")
	if err := AddRoute(store, "trellis", DirRoutePrefix+wt, "lead8"); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(t.TempDir(), "projects", "-wt")
	os.MkdirAll(proj, 0o700)
	tr := filepath.Join(proj, "lead8.jsonl")
	write := func(texts ...string) {
		var b strings.Builder
		for k, x := range texts {
			l := strings.Replace(strings.Replace(liveLine("user", fmt.Sprintf("l8-%d", k), "2026-11-05T10:00:00Z", x), `"sess1"`, `"lead8"`, 1),
				`"cwd":"/work/proj"`, `"cwd":`+jsonQ(filepath.Join(wt, "x")), 1)
			b.WriteString(l + "\n")
		}
		os.WriteFile(tr, []byte(b.String()), 0o600)
	}
	write("LEAD8 WORDS")
	ev := func(name string) string {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "lead8", "transcript_path": tr, "cwd": filepath.Join(wt, "x")})
		return string(b)
	}
	cliEnv(t, ev("SessionStart"), nil, "hook", "--data-dir", data)
	rt := LoadRoutes(store)
	if rt["lead8"] != "trellis" || rt[tr] != "" {
		t.Fatalf("the event was claimed: id %q path %q", rt["lead8"], rt[tr])
	}
	cliEnv(t, ev("Stop"), nil, "hook", "--data-dir", data)
	write("LEAD8 WORDS", "MORE WORDS")
	cliEnv(t, ev("Stop"), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	if got := messagesOf(t, store); strings.Contains(got, "WORDS") {
		t.Fatalf("root: %s", got)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); !strings.Contains(got, "LEAD8 WORDS") || !strings.Contains(got, "MORE WORDS") {
		t.Fatalf("branch: %s", got)
	}
}

// K2: a branch hook drops a dir: id and anything that is not a plain
// session id or an absolute transcript path.
func TestABranchHookWritesNoDirRoute(t *testing.T) {
	data, store := branchFixture(t)
	for _, ev := range []map[string]string{
		{"hook_event_name": "SessionStart", "session_id": "dir:/work", "transcript_path": "dir:/work2"},
		{"hook_event_name": "SessionStart", "session_id": "../x", "transcript_path": "rel/t.jsonl"},
	} {
		b, _ := json.Marshal(ev)
		cliEnv(t, string(b), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	}
	cliEnv(t, "", map[string]string{BranchVar: "trellis"}, "context", "--session", "dir:/w", "--data-dir", data)
	if rt := LoadRoutes(store); len(rt) != 0 {
		t.Fatalf("lines written: %v", rt)
	}
}

// J5: a dir route covers its dir and below, never a sibling that only
// shares a string prefix.
func TestADirRouteIsComponentAware(t *testing.T) {
	_, store := branchFixture(t)
	base := t.TempDir()
	wt := filepath.Join(base, "lead-trellis")
	os.MkdirAll(wt, 0o700)
	os.MkdirAll(wt+"-other", 0o700)
	AddRoute(store, "trellis", DirRoutePrefix+wt)
	rt := LoadRoutes(store)
	if rt.ForCwd(wt) != "trellis" || rt.ForCwd(filepath.Join(wt, "src")) != "trellis" {
		t.Fatal("the dir itself is not under its route")
	}
	for _, c := range []string{wt + "-other", wt + "x", base, wt + "-other/src"} {
		if rt.ForCwd(c) != "" {
			t.Errorf("%s is under %s", c, wt)
		}
	}
	if err := ClaimRoot(store, wt+"-other", "s-sib"); err != nil || LoadRoutes(store)["s-sib"] != RootClaim {
		t.Fatalf("a sibling session was not claimed: %v", err)
	}
}

// Session ids the map takes: Claude uuids, Hermes state.db ids (a time
// stamp and hex, or a platform:chat form), never a path or a dir: route.
func TestValidSessionIDs(t *testing.T) {
	for _, ok := range []string{"11111111-2222-4333-8444-555555555555", "20251106_101500_a1b2c3", "telegram:12345", "s.1_x"} {
		if !ValidSessionID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "dir:x", "dir:/work", "../x", "/abs", "-x", "a b", "a/b"} {
		if ValidSessionID(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

// H8: the owner's claim is written once per session. A second and a third
// SessionStart of the same chat (a resume, a compact) add no line.
func TestTheOwnersClaimIsWrittenOncePerSession(t *testing.T) {
	data, store := branchFixture(t)
	proj := filepath.Join(t.TempDir(), "projects", "-p")
	tr := claudeTranscript(t, proj, "own1", [2]string{"user", "hello"})
	b, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": "own1", "transcript_path": tr, "cwd": "/work/proj"})
	for i := 0; i < 3; i++ {
		cliEnv(t, string(b), nil, "hook", "--data-dir", data)
	}
	raw, _ := os.ReadFile(filepath.Join(store, RouteFile))
	if n := strings.Count(string(raw), "\t"+RootClaim+"\n"); n != 2 {
		t.Fatalf("%d claim lines, want 2 (the id and the transcript path):\n%s", n, raw)
	}
}

// Round 9, the folder decides: a lead's hook whose cwd is under its own
// branch's dir route writes no id line. The dir route already maps every
// session there, so the line would only grow the map.
func TestALeadHookUnderItsDirRouteWritesNoIdLine(t *testing.T) {
	data, store := branchFixture(t)
	wt := filepath.Join(t.TempDir(), "lead-trellis")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AddRoute(store, "trellis", DirRoutePrefix+wt); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(t.TempDir(), "projects", "-wt")
	tr := claudeTranscript(t, proj, "lead5", [2]string{"user", "LEAD FIVE"})
	for _, name := range []string{"SessionStart", "Stop"} {
		b, _ := json.Marshal(map[string]string{"hook_event_name": name, "session_id": "lead5", "transcript_path": tr, "cwd": filepath.Join(wt, "src")})
		cliEnv(t, string(b), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	}
	rt := LoadRoutes(store)
	if _, ok := rt["lead5"]; ok {
		t.Fatalf("an id line was written: %v", rt)
	}
	if _, ok := rt[tr]; ok {
		t.Fatalf("a path line was written: %v", rt)
	}
	if got := messagesOf(t, filepath.Join(store, BranchesDir, "trellis")); !strings.Contains(got, "LEAD FIVE") {
		t.Fatalf("branch: %s", got)
	}
	// Outside its dir route (a lead whose cwd is elsewhere) the line is
	// still written: nothing else maps that session.
	b, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": "lead6", "cwd": t.TempDir()})
	cliEnv(t, string(b), map[string]string{BranchVar: "trellis"}, "hook", "--data-dir", data)
	if LoadRoutes(store)["lead6"] != "trellis" {
		t.Fatal("a lead outside its dir route was not mapped")
	}
}

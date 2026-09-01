package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/proto"
	"github.com/opendaisugi/coppice/internal/toolchain"
)

func TestRecentDirsRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	if err := saveRecentDirs(dir, []string{"/a", "/b"}); err != nil {
		t.Fatal(err)
	}
	got := loadRecentDirs(dir)
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("loadRecentDirs = %v", got)
	}
}

func TestLoadRecentDirsWithNoFileIsEmpty(t *testing.T) {
	if got := loadRecentDirs(t.TempDir()); len(got) != 0 {
		t.Fatalf("loadRecentDirs = %v, want none", got)
	}
}

func TestRecordRecentDirPutsTheNewestFirstAndDropsAnOlderCopy(t *testing.T) {
	s := newTestServer(t)
	s.recordRecentDir("/a")
	s.recordRecentDir("/b")
	s.recordRecentDir("/a")
	got := loadRecentDirs(s.cfg.DataDir)
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("recent dirs = %v, want [/a /b]", got)
	}
}

func TestRecordRecentDirKeepsOnlyTheNewestTen(t *testing.T) {
	s := newTestServer(t)
	for i := 0; i < 12; i++ {
		s.recordRecentDir(filepath.Join("/d", string(rune('a'+i))))
	}
	got := loadRecentDirs(s.cfg.DataDir)
	if len(got) != maxRecentDirs {
		t.Fatalf("recent dirs len = %d, want %d", len(got), maxRecentDirs)
	}
	if got[0] != filepath.Join("/d", "l") {
		t.Fatalf("newest = %q, want /d/l", got[0])
	}
}

func TestRecordRecentDirIgnoresAnEmptyDir(t *testing.T) {
	s := newTestServer(t)
	s.recordRecentDir("")
	if got := loadRecentDirs(s.cfg.DataDir); len(got) != 0 {
		t.Fatalf("recent dirs = %v, want none", got)
	}
}

// recordRecentDir resolves a symlink to its real target before storing it,
// so the same directory recorded once by its symlink and once by its real
// path never counts as two distinct recent entries.
func TestRecordRecentDirResolvesASymlink(t *testing.T) {
	s := newTestServer(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link-to-real")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink on this filesystem: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	s.recordRecentDir(link)
	s.recordRecentDir(real)
	got := loadRecentDirs(s.cfg.DataDir)
	if len(got) != 1 || got[0] != resolved {
		t.Fatalf("recent dirs = %v, want exactly [%s]", got, resolved)
	}
}

// project.list -------------------------------------------------------------

func TestProjectListReturnsPinnedThenRecentNotDuplicated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinned := t.TempDir()
	recentOnly := t.TempDir()
	if err := config.Save(config.Config{Projects: []string{pinned}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	s.recordRecentDir(recentOnly)
	s.recordRecentDir(pinned)
	got := roundTrip(t, s, `{"id":"1","cmd":"project.list"}`)
	m := result(t, got[0])
	rows, _ := m["projects"].([]any)
	if len(rows) != 2 {
		t.Fatalf("projects = %v, want 2 rows", rows)
	}
	first, _ := rows[0].(map[string]any)
	if first["path"] != pinned || first["pinned"] != true || first["name"] != filepath.Base(pinned) {
		t.Fatalf("row 0 = %v, want the pinned project first", first)
	}
	second, _ := rows[1].(map[string]any)
	if second["path"] != recentOnly || second["pinned"] != false {
		t.Fatalf("row 1 = %v, want the recent-only directory, unpinned", second)
	}
}

func TestProjectListWithNoConfigAndNoRecentIsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"project.list"}`)
	m := result(t, got[0])
	rows, _ := m["projects"].([]any)
	if len(rows) != 0 {
		t.Fatalf("projects = %v, want none", rows)
	}
	if d, ok := m["default"]; !ok || d != "" {
		t.Fatalf("default = %v (present %v), want an empty string with no config", d, ok)
	}
}

// A client that starts the default harness in a chosen project must name
// the harness, since only a create with no cwd guesses one. project.list
// says which harness that is.
func TestProjectListNamesTheDefaultHarness(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh"},
	}}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"project.list"}`)
	m := result(t, got[0])
	if m["default"] != "fakeh" {
		t.Fatalf("default = %v, want fakeh", m["default"])
	}
}

// pane.create defaulting ----------------------------------------------------

func TestPaneCreateWithNoCwdAndNoArgvUsesTheStartDirAndDefaultHarness(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := config.Save(config.Config{Default: "fakeh", Harness: map[string]config.Harness{
		"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}},
	}}); err != nil {
		t.Fatal(err)
	}
	startDir := t.TempDir()
	s, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "s.sock"), DataDir: t.TempDir(), StartDir: startDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AcquireStartLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.RegisterPaneCommands()

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create = %v, want a pane id", got[0])
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != startDir {
		t.Fatalf("cwd = %v, want the start dir %q", row["cwd"], startDir)
	}
	if row["harness"] != "fakeh" {
		t.Fatalf("harness = %v, want fakeh", row["harness"])
	}
	wantLabel := "fakeh-" + filepath.Base(startDir)
	if row["label"] != wantLabel {
		t.Fatalf("label = %v, want %q", row["label"], wantLabel)
	}
}

func TestPaneCreateWithNoCwdUsesTheMostRecentDirOverAPinnedOne(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinned := t.TempDir()
	recent := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{pinned},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	s.recordRecentDir(recent)

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != recent {
		t.Fatalf("cwd = %v, want the most recent dir %q, not the pinned one", row["cwd"], recent)
	}
}

// A directory that is both pinned and the one just used still wins as the
// default cwd: the fallback reads the raw recent list, newest first, not
// project.list's own "not already pinned" half - being pinned does not
// cost a directory its place at the front of the recent list.
func TestPaneCreateWithNoCwdUsesTheJustUsedDirEvenWhenItIsPinned(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinnedAndJustUsed := t.TempDir()
	olderUnpinned := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{pinnedAndJustUsed},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	s.recordRecentDir(olderUnpinned)
	s.recordRecentDir(pinnedAndJustUsed) // recorded last, so it is the newest entry

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != pinnedAndJustUsed {
		t.Fatalf("cwd = %v, want the just-used dir %q: being pinned does not cost a directory "+
			"its place as the most recently used one", row["cwd"], pinnedAndJustUsed)
	}
}

// A missing newest recent directory falls through to the NEXT recent
// directory, not straight to a pinned project - the whole recent list is
// walked before the fallback moves on to pinned projects at all.
func TestPaneCreateWithNoCwdSkipsAMissingNewestRecentDirForAnOlderOne(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinned := t.TempDir()
	olderRecent := t.TempDir()
	newestRecent := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{pinned},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	s.recordRecentDir(olderRecent)
	s.recordRecentDir(newestRecent)
	if err := os.RemoveAll(newestRecent); err != nil {
		t.Fatal(err)
	}

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != olderRecent {
		t.Fatalf("cwd = %v, want the older recent dir %q, not the pinned project - the recent "+
			"list is walked in full before falling to pinned projects", row["cwd"], olderRecent)
	}
}

func TestPaneCreateWithNoCwdFallsBackToAPinnedProjectWithNoRecentDir(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinned := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{pinned},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != pinned {
		t.Fatalf("cwd = %v, want the pinned project %q", row["cwd"], pinned)
	}
}

func TestPaneCreateNearUsesThatPanesCwdBeforeAnyProject(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	nearDir := t.TempDir()
	recentDir := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh",
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	s.recordRecentDir(recentDir)

	nearRes := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+nearDir+`","cmd_argv":["sh","-c","sleep 30"]}`)
	nearID, _ := result(t, nearRes[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, nearID) })

	got := roundTrip(t, s, `{"id":"2","cmd":"pane.create","near":"`+nearID+`"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != nearDir {
		t.Fatalf("cwd = %v, want the near pane's own cwd %q", row["cwd"], nearDir)
	}
}

func TestPaneCreateNearWithAnUnknownPaneIsNoSuchPane(t *testing.T) {
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","near":"nope"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "no_such_pane" {
		t.Fatalf("got %+v, want no_such_pane", got[0])
	}
}

func TestPaneCreateSkipsADefaultDirectoryThatNoLongerExists(t *testing.T) {
	toolchain.RequireOrSkip(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gone := filepath.Join(t.TempDir(), "gone")
	pinned := t.TempDir()
	if err := config.Save(config.Config{Default: "fakeh", Projects: []string{pinned},
		Harness: map[string]config.Harness{"fakeh": {Command: "sh", Args: []string{"-c", "sleep 30"}}},
	}); err != nil {
		t.Fatal(err)
	}
	s := newPaneServer(t)
	s.recordRecentDir(gone) // never created on disk

	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	m := result(t, got[0])
	id, _ := m["pane"].(string)
	if id == "" {
		t.Fatalf("pane.create = %v", got[0])
	}
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["cwd"] != pinned {
		t.Fatalf("cwd = %v, want the pinned project %q since the recent dir is gone", row["cwd"], pinned)
	}
}

func TestPaneCreateWithNoCwdAndNoDefaultHarnessNamesTheFix(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create"}`)
	if got[0].OK || got[0].Error == nil || got[0].Error.Code != "bad_request" {
		t.Fatalf("got %+v, want bad_request", got[0])
	}
	if !strings.Contains(got[0].Error.Message, "no default harness") {
		t.Fatalf("message = %q, want it to name the fix", got[0].Error.Message)
	}
}

func TestPaneCreateWithExplicitCwdAndNoHarnessStillRefusesEvenWithADefaultConfigured(t *testing.T) {
	toolchain.RequireOrSkip(t)
	saveFakeHarness(t)
	s := newPaneServer(t)
	got := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`"}`)
	if got[0].OK || got[0].Error == nil || !strings.Contains(got[0].Error.Message, "a pty pane needs cmd_argv") {
		t.Fatalf("got %+v, want the plain cmd_argv refusal since cwd was given", got[0])
	}
}

// Labels ---------------------------------------------------------------

func TestPaneCreateLabelDefaultsAndNumbersWhenALivePaneAlreadyHasIt(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	s := newPaneServer(t)
	res1 := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+dir+`","harness":"fakeh","cmd_argv":["sh","-c","sleep 30"]}`)
	id1, _ := result(t, res1[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id1) })
	row1 := rowFor(t, listPanes(t, s), id1)
	want := "fakeh-" + filepath.Base(dir)
	if row1["label"] != want {
		t.Fatalf("label 1 = %v, want %q", row1["label"], want)
	}

	res2 := roundTrip(t, s, `{"id":"2","cmd":"pane.create","cwd":"`+dir+`","harness":"fakeh","cmd_argv":["sh","-c","sleep 30"]}`)
	id2, _ := result(t, res2[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id2) })
	row2 := rowFor(t, listPanes(t, s), id2)
	if row2["label"] != want+"-2" {
		t.Fatalf("label 2 = %v, want %q", row2["label"], want+"-2")
	}
}

func TestPaneCreateLabelKeepsAnExplicitLabel(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	res := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","harness":"fakeh",`+
		`"cmd_argv":["sh","-c","sleep 30"],"label":"my own name"}`)
	id, _ := result(t, res[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["label"] != "my own name" {
		t.Fatalf("label = %v, want the explicit one kept as given", row["label"])
	}
}

// The first pane ends ON ITS OWN (never pane.close, which would remove the
// record outright and prove nothing about ended records specifically) and
// stays in the tree as an ended record, the way a self-ended pane always
// does. liveLabelTaken must still leave it out of the count.
func TestPaneCreateLabelIgnoresAnEndedPaneWithTheSameName(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	s := newPaneServer(t)
	res1 := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+dir+`","harness":"fakeh","cmd_argv":["sh","-c","exit 0"]}`)
	id1, _ := result(t, res1[0])["pane"].(string)
	waitDone(t, s, id1)

	res2 := roundTrip(t, s, `{"id":"2","cmd":"pane.create","cwd":"`+dir+`","harness":"fakeh","cmd_argv":["sh","-c","sleep 30"]}`)
	id2, _ := result(t, res2[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id2) })
	row := rowFor(t, listPanes(t, s), id2)
	want := "fakeh-" + filepath.Base(dir)
	if row["label"] != want {
		t.Fatalf("label = %v, want %q - an ended pane's own name must not force numbering", row["label"], want)
	}
}

func TestPaneCreateWithNoHarnessAndRawArgvGetsNoLabel(t *testing.T) {
	toolchain.RequireOrSkip(t)
	s := newPaneServer(t)
	res := roundTrip(t, s, `{"id":"1","cmd":"pane.create","cwd":"`+t.TempDir()+`","cmd_argv":["sh","-c","sleep 30"]}`)
	id, _ := result(t, res[0])["pane"].(string)
	t.Cleanup(func() { closePane(t, s, id) })
	row := rowFor(t, listPanes(t, s), id)
	if row["label"] != "" {
		t.Fatalf("label = %v, want empty for a raw command with no harness", row["label"])
	}
}

// concurrentPaneCreate calls handlePaneCreate directly, bypassing the wire
// protocol entirely, and returns the response instead of failing the test
// itself: t.Fatal is not safe to call from a goroutine other than the
// test's own, and this helper exists to be called from several at once.
func concurrentPaneCreate(s *Server, line string) proto.Response {
	var req proto.Request
	if err := req.UnmarshalJSON([]byte(line)); err != nil {
		return proto.ErrResp("", proto.ErrInternal, err.Error())
	}
	return s.handlePaneCreate(nil, &req)
}

// Several requests for the same harness and directory, fired at once with
// none of them given a label, must never land on the same one - labelMu
// spans the whole pick-then-create, so the second request to acquire it
// always finds the first request's own label already on the tree.
func TestConcurrentPaneCreatesForTheSameHarnessAndDirGetDistinctLabels(t *testing.T) {
	toolchain.RequireOrSkip(t)
	saveFakeHarness(t)
	dir := t.TempDir()
	s := newPaneServer(t)
	// Pre-created sequentially, before any concurrent request: workspace
	// and tab auto-creation on a first pane.create is its own, separate
	// race, not the one this test is about, and must not add noise here.
	wsRes := roundTrip(t, s, `{"id":"1","cmd":"workspace.create","cwd":"`+dir+`"}`)
	wsm := result(t, wsRes[0])
	wsID, _ := wsm["workspace"].(string)
	tabID, _ := wsm["tab"].(string)
	if wsID == "" || tabID == "" {
		t.Fatalf("workspace.create = %v", wsm)
	}

	const n = 8
	responses := make([]proto.Response, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			responses[i] = concurrentPaneCreate(s, `{"id":"1","cmd":"pane.create","cwd":"`+dir+
				`","harness":"fakeh","cmd_argv":["sh","-c","sleep 30"],`+
				`"workspace":"`+wsID+`","tab":"`+tabID+`"}`)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	labels := make([]string, n)
	for i, resp := range responses {
		if !resp.OK {
			t.Fatalf("create %d failed: %+v", i, resp.Error)
		}
		b, _ := json.Marshal(resp.Result)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		id, _ := m["pane"].(string)
		if id == "" {
			t.Fatalf("create %d returned no pane id: %v", i, m)
		}
		t.Cleanup(func(id string) func() { return func() { closePane(t, s, id) } }(id))
		row := rowFor(t, listPanes(t, s), id)
		label, _ := row["label"].(string)
		labels[i] = label
		if label == "" {
			t.Fatalf("pane %s has no label", id)
		}
		if seen[label] {
			t.Fatalf("label %q was given to more than one pane: %v", label, labels)
		}
		seen[label] = true
	}
}

// On a fresh server, several creates at once each find no workspace. They
// must share the one the first of them makes, and every one must start.
func TestConcurrentFirstCreatesOnAFreshServerAllStart(t *testing.T) {
	toolchain.RequireOrSkip(t)
	dir := t.TempDir()
	for round := 0; round < 5; round++ {
		s := newPaneServer(t)
		const n = 6
		responses := make([]proto.Response, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				responses[i] = concurrentPaneCreate(s, `{"id":"1","cmd":"pane.create","cwd":"`+dir+
					`","cmd_argv":["sh","-c","sleep 30"]}`)
			}(i)
		}
		wg.Wait()
		for i, resp := range responses {
			if !resp.OK {
				t.Fatalf("round %d: create %d failed: %+v", round, i, resp.Error)
			}
		}
		if ws := len(s.tree.Workspaces()); ws != 1 {
			t.Fatalf("round %d: %d workspaces, want 1", round, ws)
		}
	}
}

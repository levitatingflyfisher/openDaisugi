package hermes

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fixture builds a state.db with the columns Hermes documents in
// hermes_state_common.py (sessions and messages), cut to what the reader
// uses plus a few it must ignore.
func fixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, parent_session_id TEXT,
			started_at REAL NOT NULL, profile_name TEXT, archived INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL,
			role TEXT NOT NULL, content TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL NOT NULL,
			_compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func exec(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func addSession(t *testing.T, p, id, source, parent string, at float64) {
	exec(t, p, `INSERT INTO sessions (id, source, parent_session_id, started_at) VALUES (?,?,NULLIF(?,''),?)`, id, source, parent, at)
}

func addMsg(t *testing.T, p, sid, role, content string, at float64) {
	exec(t, p, `INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,?,?,?)`, sid, role, content, at)
}

func TestHomeFollowsTheProfileFlag(t *testing.T) {
	for _, c := range []struct {
		argv []string
		env  map[string]string
		want string
	}{
		{[]string{"/x/hermes", "-p", "steward", "-c"}, nil, "/home/o/.hermes/profiles/steward"},
		{[]string{"hermes", "--profile=steward"}, nil, "/home/o/.hermes/profiles/steward"},
		{[]string{"hermes", "--profile", "default"}, nil, "/home/o/.hermes"},
		{[]string{"hermes", "-c"}, nil, "/home/o/.hermes"},
		{[]string{"hermes", "-c"}, map[string]string{"HERMES_HOME": "/s/h"}, "/s/h"},
		{[]string{"hermes", "-p", "../x"}, nil, ""},
		{[]string{"hermes", "-p", "a/b"}, nil, ""},
	} {
		if got := Home(c.argv, c.env, "/home/o"); got != c.want {
			t.Errorf("Home(%v, %v) = %q, want %q", c.argv, c.env, got, c.want)
		}
	}
}

func TestSessionNameReadsTheResumeAndContinueFlags(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"hermes", "-r", "s9", "-c"}, "s9"},
		{[]string{"hermes", "--resume=s8"}, "s8"},
		{[]string{"hermes", "-c"}, ""},
		{[]string{"hermes", "-p", "steward", "-c"}, ""},
		{[]string{"hermes", "-c", "-p", "steward"}, ""},
		{[]string{"hermes", "-c", "Pokemon", "Agent", "Dev", "-p", "x"}, "Pokemon Agent Dev"},
		{[]string{"hermes", "--continue=night shift"}, "night shift"},
		{[]string{"hermes", "-c", "chat"}, ""},
	} {
		if got := SessionName(c.argv); got != c.want {
			t.Errorf("SessionName(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestPickTakesTheNewestSessionOfTheTerminalNotTheGateway(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	addSession(t, p, "old", "cli", "", 100)
	addSession(t, p, "new", "cli", "", 200)
	addSession(t, p, "chat", "mattermost", "", 300)
	addMsg(t, p, "old", "user", "a", 101)
	addMsg(t, p, "new", "user", "b", 201)
	addMsg(t, p, "chat", "user", "c", 301)
	got, err := Pick(p, "", "")
	if err != nil || got != "new" {
		t.Fatalf("Pick = %q, %v", got, err)
	}
	got, err = Pick(p, "old", "")
	if err != nil || got != "old" {
		t.Fatalf("Pick resume = %q, %v", got, err)
	}
}

func TestPickKnowsOnlyTheTerminalFamilyAndNoneWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	if got, err := Pick(p, "", ""); err != nil || got != "" {
		t.Fatalf("empty db: %q, %v", got, err)
	}
	addSession(t, p, "only", "mattermost", "", 5)
	addMsg(t, p, "only", "user", "hi", 6)
	if got, _ := Pick(p, "", ""); got != "" {
		t.Fatalf("a gateway session is not the terminal's: %q", got)
	}
	addSession(t, p, "once", "oneshot", "", 7)
	addMsg(t, p, "once", "user", "hi", 8)
	if got, _ := Pick(p, "", ""); got != "once" {
		t.Fatalf("a oneshot run is terminal history too: %q", got)
	}
	addSession(t, p, "here", "local", "", 9)
	addMsg(t, p, "here", "user", "hi", 10)
	if got, _ := Pick(p, "", ""); got != "here" {
		t.Fatalf("a local run (the terminal chat of newer Hermes) is terminal history: %q", got)
	}
}

func TestPickLeavesOutAnArchivedSession(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	addSession(t, p, "a", "cli", "", 1)
	addSession(t, p, "b", "cli", "", 2)
	addMsg(t, p, "a", "user", "x", 3)
	addMsg(t, p, "b", "user", "y", 4)
	exec(t, p, `UPDATE sessions SET archived = 1 WHERE id = 'b'`)
	if got, _ := Pick(p, "", ""); got != "a" {
		t.Fatalf("got %q", got)
	}
}

func TestMessagesGiveRowIDsInOrderAndWalkTheParentChain(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	addSession(t, p, "root", "cli", "", 1)
	addSession(t, p, "tip", "cli", "root", 2)
	addMsg(t, p, "root", "user", "one", 10)
	addMsg(t, p, "root", "assistant", "two", 11)
	addMsg(t, p, "tip", "user", "three", 12)
	rows, cut, err := Messages(p, "tip", 100)
	if err != nil || cut {
		t.Fatalf("%v cut=%v", err, cut)
	}
	if len(rows) != 3 || rows[0].Content != "one" || rows[2].Content != "three" {
		t.Fatalf("rows = %+v", rows)
	}
	if !(rows[0].ID < rows[1].ID && rows[1].ID < rows[2].ID) {
		t.Fatalf("ids not rising: %+v", rows)
	}
	// A later append keeps the earlier ids: in-place updates depend on it.
	addMsg(t, p, "tip", "assistant", "four", 13)
	again, _, _ := Messages(p, "tip", 100)
	for i := range rows {
		if again[i].ID != rows[i].ID {
			t.Fatalf("id %d changed", i)
		}
	}
}

func TestMessagesSkipInactiveAndSummaryRowsAndCutAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	addSession(t, p, "s", "cli", "", 1)
	for i := 0; i < 5; i++ {
		addMsg(t, p, "s", "user", string(rune('a'+i)), float64(10+i))
	}
	exec(t, p, `UPDATE messages SET active = 0 WHERE content = 'a'`)
	exec(t, p, `UPDATE messages SET _compressed_summary = 1 WHERE content = 'b'`)
	rows, cut, err := Messages(p, "s", 100)
	if err != nil || cut || len(rows) != 3 {
		t.Fatalf("%v cut=%v rows=%+v", err, cut, rows)
	}
	rows, cut, _ = Messages(p, "s", 2)
	if !cut || len(rows) != 2 || rows[1].Content != "e" {
		t.Fatalf("cut=%v rows=%+v", cut, rows)
	}
}

func TestReadingDoesNotWriteTheFile(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	addSession(t, p, "s", "cli", "", 1)
	addMsg(t, p, "s", "user", "x", 2)
	// A live Hermes holds the database open in WAL mode: keep a writer
	// open so the -wal and -shm files exist, as they do on the box.
	w, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s','assistant','y',3)`); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	rows, _, err := Messages(p, "s", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("a live WAL row must be visible: %v %v", rows, err)
	}
	if _, err := Pick(p, "", ""); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("files changed:\n%s\n%s", before, after)
	}
}

func TestAMissingDatabaseHasNoSession(t *testing.T) {
	got, err := Pick(filepath.Join(t.TempDir(), "state.db"), "", "")
	if err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
	rows, _, err := Messages(filepath.Join(t.TempDir(), "state.db"), "s", 5)
	if err != nil || len(rows) != 0 {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestAnOldSchemaWithoutActiveStillReads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.db")
	exec(t, p, `CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, parent_session_id TEXT, started_at REAL NOT NULL)`)
	exec(t, p, `CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_calls TEXT, timestamp REAL NOT NULL)`)
	exec(t, p, `INSERT INTO sessions VALUES ('s','cli',NULL,1)`)
	exec(t, p, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s','user','hi',2)`)
	rows, _, err := Messages(p, "s", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestPickPrefersTheSessionOfThePanesWorkspaceAndLeavesOutAHiddenOne(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	exec(t, p, `ALTER TABLE sessions ADD COLUMN cwd TEXT`)
	exec(t, p, `ALTER TABLE sessions ADD COLUMN git_repo_root TEXT`)
	exec(t, p, `ALTER TABLE sessions ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`)
	addSession(t, p, "here", "cli", "", 1)
	addSession(t, p, "elsewhere", "cli", "", 2)
	addSession(t, p, "hid", "cli", "", 3)
	exec(t, p, `UPDATE sessions SET cwd = '/repo/sub', git_repo_root = '/repo' WHERE id = 'here'`)
	exec(t, p, `UPDATE sessions SET cwd = '/other' WHERE id = 'elsewhere'`)
	exec(t, p, `UPDATE sessions SET cwd = '/repo', git_repo_root = '/repo', hidden = 1 WHERE id = 'hid'`)
	addMsg(t, p, "here", "user", "a", 10)
	addMsg(t, p, "elsewhere", "user", "b", 20)
	addMsg(t, p, "hid", "user", "c", 30)
	if got, _ := Pick(p, "", "/repo"); got != "here" {
		t.Fatalf("scoped pick = %q, want here", got)
	}
	if got, _ := Pick(p, "", "/nowhere"); got != "elsewhere" {
		t.Fatalf("unscoped fallback = %q, want elsewhere", got)
	}
	if got, _ := Pick(p, "", ""); got != "elsewhere" {
		t.Fatalf("no workspace = %q, want elsewhere", got)
	}
}

func TestPickResolvesATitleAndFollowsACompressionToItsTip(t *testing.T) {
	dir := t.TempDir()
	p := fixture(t, dir)
	exec(t, p, `ALTER TABLE sessions ADD COLUMN title TEXT`)
	exec(t, p, `ALTER TABLE sessions ADD COLUMN end_reason TEXT`)
	addSession(t, p, "root", "cli", "", 1)
	addSession(t, p, "tip", "cli", "root", 2)
	addSession(t, p, "branch", "cli", "tip", 3)
	exec(t, p, `UPDATE sessions SET title = 'night shift', end_reason = 'compression' WHERE id = 'root'`)
	addMsg(t, p, "root", "user", "a", 5)
	addMsg(t, p, "tip", "user", "b", 6)
	if got, _ := Pick(p, "night shift", ""); got != "tip" {
		t.Fatalf("title pick = %q, want tip", got)
	}
	if got, _ := Pick(p, "root", ""); got != "tip" {
		t.Fatalf("id pick = %q, want tip", got)
	}
	if got, _ := Pick(p, "branch", ""); got != "branch" {
		t.Fatalf("a branch is not a compression: %q", got)
	}
}

func TestHomeReadsTheStickyProfileAndTheHermesHomeRoot(t *testing.T) {
	user := t.TempDir()
	root := filepath.Join(user, ".hermes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "active_profile"), []byte("steward\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "profiles", "steward")
	if got := Home([]string{"hermes", "-c"}, nil, user); got != want {
		t.Fatalf("sticky profile: %q, want %q", got, want)
	}
	if got := Home([]string{"hermes", "-p", "default"}, nil, user); got != root {
		t.Fatalf("-p default beats the sticky one: %q", got)
	}
	if got := Home([]string{"hermes", "-p", "other"}, nil, user); got != filepath.Join(root, "profiles", "other") {
		t.Fatalf("-p beats the sticky one: %q", got)
	}
	// HERMES_HOME is the root; a profile lives under it.
	custom := filepath.Join(user, "custom")
	if got := Home([]string{"hermes", "-p", "steward"}, map[string]string{"HERMES_HOME": custom}, user); got != filepath.Join(custom, "profiles", "steward") {
		t.Fatalf("profile under HERMES_HOME: %q", got)
	}
	// HERMES_HOME that is a profile dir stands without a flag, and its root is two levels up.
	prof := filepath.Join(custom, "profiles", "ops")
	if got := Home([]string{"hermes"}, map[string]string{"HERMES_HOME": prof}, user); got != prof {
		t.Fatalf("profile HERMES_HOME: %q", got)
	}
	if got := Home([]string{"hermes", "-p", "steward"}, map[string]string{"HERMES_HOME": prof}, user); got != filepath.Join(custom, "profiles", "steward") {
		t.Fatalf("-p from a profile HERMES_HOME: %q", got)
	}
}

func TestWorkspaceKeyIsTheGitRootElseTheDirectory(t *testing.T) {
	repo := t.TempDir()
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := WorkspaceKey(sub); got != sub {
		t.Fatalf("no repo: %q", got)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := WorkspaceKey(sub); got != repo {
		t.Fatalf("in a repo: %q, want %q", got, repo)
	}
}

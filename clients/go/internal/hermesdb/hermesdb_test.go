package hermesdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daisugi-verify/pando"
)

func mkdb(t *testing.T, dir string, stmts ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite3", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range append([]string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, display_name TEXT, chat_id TEXT, thread_id TEXT, started_at REAL NOT NULL DEFAULT 0)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT, tool_calls TEXT, timestamp REAL NOT NULL, _compressed_summary INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 1)`,
	}, stmts...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestHermesPollLogsEveryProfileOnceWithWhoAndWhere(t *testing.T) {
	root := t.TempDir()
	mkdb(t, root,
		`INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','steward question',1000)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','steward answer',1001)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','tool','tool noise',1002)`,
		`INSERT INTO messages (session_id, role, content, timestamp, active) VALUES ('s1','user','rewound',1003,0)`)
	mkdb(t, filepath.Join(root, "profiles", "household"),
		`INSERT INTO sessions VALUES ('m1','mattermost','u9','Dana','chan1','th7',1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('m1','user','buy milk',2000)`)
	dir := filepath.Join(t.TempDir(), "pando", "work")
	c, err := pando.Open(context.Background(), pando.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := &Source{Root: root}
	n, err := s.Poll(context.Background(), c, time.Time{})
	if err != nil || n != 3 {
		t.Fatalf("poll: %d %v", n, err)
	}
	if n, _ := s.Poll(context.Background(), c, time.Time{}); n != 0 {
		t.Fatalf("second poll logged %d", n)
	}
	var found bool
	for i := int64(0); i < c.Count(); i++ {
		m, _ := c.Message(i)
		if m.Who == "Dana" {
			found = true
			if m.Kind != pando.KindUser || m.Src != "hermes:household" || !strings.Contains(m.Chat, "mattermost") ||
				!strings.Contains(m.Chat, "th7") || !strings.Contains(m.Text, "buy milk") {
				t.Fatalf("household message: %+v", m)
			}
		}
		if strings.Contains(m.Text, "tool noise") || strings.Contains(m.Text, "rewound") {
			t.Fatalf("logged %q", m.Text)
		}
	}
	if !found {
		t.Fatal("no household message")
	}
	// A time bound keeps old rows out; one profile can be chosen.
	c2, _ := pando.Open(context.Background(), pando.Config{Dir: filepath.Join(t.TempDir(), "p")})
	defer c2.Close()
	if n, _ := (&Source{Root: root, Profile: "household"}).Poll(context.Background(), c2, time.Unix(3000, 0)); n != 0 {
		t.Fatalf("since: %d", n)
	}
	if n, _ := (&Source{Root: root, Profile: "household"}).Poll(context.Background(), c2, time.Time{}); n != 1 {
		t.Fatalf("one profile: %d", n)
	}
}

func TestHermesPollNeverWritesAndSurvivesABadFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := pando.Open(context.Background(), pando.Config{Dir: filepath.Join(t.TempDir(), "p")})
	defer c.Close()
	if _, err := (&Source{Root: root}).Poll(context.Background(), c, time.Time{}); err == nil {
		t.Fatal("want an error for a bad file")
	}
	b, _ := os.ReadFile(filepath.Join(root, "state.db"))
	if string(b) != "not sqlite" {
		t.Fatal("the file changed")
	}
}

func TestRootFollowsHermesHome(t *testing.T) {
	if Root("", "/h") != "/h/.hermes" || Root("/x/profiles/a", "/h") != "/x" || Root("/x", "/h") != "/x" {
		t.Fatal("root")
	}
}

// Round 7 fix: a cursor on messages.id per profile.

func countAdds(t *testing.T, c *pando.Chat) int64 { return c.Count() }

func TestACursorTakesALateRowWithAnOldTimeAndOneBadProfileStallsOnlyItself(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "profiles", "household")
	mkdb(t, root, `INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','first',5000)`)
	mkdb(t, good, `INSERT INTO sessions VALUES ('m1','mattermost','u9','Dana','c','',1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('m1','user','hello',5000)`)
	bad := filepath.Join(root, "profiles", "broken")
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, "state.db"), []byte("not sqlite"), 0o600)
	c, _ := pando.Open(context.Background(), pando.Config{Dir: filepath.Join(t.TempDir(), "p")})
	defer c.Close()
	s := &Source{Root: root}
	first := time.Unix(4000, 0)
	n, err := s.Poll(context.Background(), c, first)
	if err == nil || n != 2 || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("first poll: %d %v", n, err)
	}
	cur := s.Cursors()
	if cur["default"] == 0 || cur["household"] == 0 || cur["broken"] != 0 {
		t.Fatalf("cursors %v: the good profiles advance, the bad one stays", cur)
	}
	// A row committed now with a time long before `since` (a long tool loop
	// stamps its assistant row at the start) is still taken, by id.
	db, _ := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','late answer',10)`)
	db.Close()
	far := time.Unix(900000, 0)
	n, _ = s.Poll(context.Background(), c, far)
	if n != 1 {
		t.Fatalf("the late row was missed: %d", n)
	}
	// Nothing is read twice: a poll with no new rows adds none.
	if n, _ = s.Poll(context.Background(), c, far); n != 0 {
		t.Fatalf("read again: %d", n)
	}
	// A new Source with the saved cursors starts where the old one stopped.
	s2 := &Source{Root: root}
	s2.SetCursors(s.Cursors())
	if n, _ = s2.Poll(context.Background(), c, time.Time{}); n != 0 {
		t.Fatalf("restart read again: %d", n)
	}
	if countAdds(t, c) != 3 {
		t.Fatalf("count %d", c.Count())
	}
}

// A Hermes whose state.db was made again starts its ids at 1. With a cursor
// from the old file, every new row would sit below it for good.
func TestARecreatedDbIsReadAgainFromTheStart(t *testing.T) {
	root := t.TempDir()
	mkdb(t, root, `INSERT INTO sessions VALUES ('s1','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','one',5000)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','two',5001)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','three',5002)`)
	c, _ := pando.Open(context.Background(), pando.Config{Dir: filepath.Join(t.TempDir(), "p")})
	defer c.Close()
	s := &Source{Root: root}
	if n, err := s.Poll(context.Background(), c, time.Unix(4000, 0)); err != nil || n != 3 {
		t.Fatalf("first poll: %d %v", n, err)
	}
	cur := s.Cursors()["default"]
	if cur != 3 {
		t.Fatalf("cursor %d", cur)
	}
	// Hermes starts over: a new file, new session, ids from 1.
	if err := os.Remove(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	mkdb(t, root, `INSERT INTO sessions VALUES ('s2','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','user','fresh one',6000)`)
	n, err := s.Poll(context.Background(), c, time.Unix(5500, 0))
	if err != nil || n != 1 {
		t.Fatalf("the new db was not read: %d %v", n, err)
	}
	if got := s.Cursors()["default"]; got != 1 {
		t.Fatalf("the cursor stayed at the old file's id: %d", got)
	}
	// And it goes on by id from there.
	db, _ := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','assistant','fresh two',6001)`)
	db.Close()
	if n, _ = s.Poll(context.Background(), c, time.Unix(9000, 0)); n != 1 {
		t.Fatalf("the next row was missed: %d", n)
	}
}

// A delegate_task child is a worker of the steward: its rows stay out of
// the log, by its source and by the marker in its model_config.
func TestASubagentSessionIsNeverLogged(t *testing.T) {
	root := t.TempDir()
	mkdb(t, root,
		`ALTER TABLE sessions ADD COLUMN model_config TEXT`,
		`INSERT INTO sessions (id, source) VALUES ('s1','cli')`,
		`INSERT INTO sessions (id, source) VALUES ('c1','subagent')`,
		`INSERT INTO sessions (id, source, model_config) VALUES ('c2','cli','{"_delegate_from":"s1"}')`,
		`INSERT INTO sessions (id, source, model_config) VALUES ('s2','cli','{"model":"x"}')`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','owner question',1000)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('c1','user','child task by source',1001)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('c2','assistant','child task by marker',1002)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s2','assistant','owner answer',1003)`)
	c, err := pando.Open(context.Background(), pando.Config{Dir: filepath.Join(t.TempDir(), "pando", "work")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := &Source{Root: root}
	n, err := s.Poll(context.Background(), c, time.Time{})
	if err != nil || n != 2 {
		t.Fatalf("poll: %d %v, want the two owner rows", n, err)
	}
	for i := int64(0); i < c.Count(); i++ {
		m, _ := c.Message(i)
		if strings.Contains(m.Text, "child task") {
			t.Fatalf("logged a subagent row: %q", m.Text)
		}
	}
}

// Round 8b: a Hermes session on the store's session map (the lead's plugin
// put it there) is logged into its branch, from its first row, and never
// into the store above.
func TestAMappedHermesSessionGoesToItsBranch(t *testing.T) {
	root := t.TempDir()
	mkdb(t, root,
		`INSERT INTO sessions VALUES ('lead','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO sessions VALUES ('own','cli',NULL,NULL,NULL,NULL,1)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('lead','user','the lead plans the reader',1000)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('own','user','the owner asks at the root',1001)`,
		`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('lead','assistant','reader plan done',1002)`)
	dir := filepath.Join(t.TempDir(), "pando", "work")
	if err := pando.AddRoute(dir, "trellis", "hermes:lead"); err != nil {
		t.Fatal(err)
	}
	c, err := pando.Open(context.Background(), pando.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{Root: root}
	if n, err := s.Poll(context.Background(), c, time.Time{}); err != nil || n != 3 {
		t.Fatalf("poll: %d %v", n, err)
	}
	if c.Count() != 1 {
		t.Fatalf("root holds %d messages", c.Count())
	}
	m, _ := c.Message(0)
	if !strings.Contains(m.Text, "owner asks") {
		t.Fatalf("root: %q", m.Text)
	}
	c.Close()
	b, err := pando.OpenReadOnly(filepath.Join(dir, pando.BranchesDir, "trellis"))
	if err != nil || b.Count() != 2 {
		t.Fatalf("branch: %v", err)
	}
	m, _ = b.Message(0)
	if !strings.Contains(m.Text, "the lead plans") {
		t.Fatalf("branch: %q", m.Text)
	}
}

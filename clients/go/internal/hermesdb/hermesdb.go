// Package hermesdb reads the chats of Hermes Agent (Nous Research) from its
// state.db files and logs them into a pando store. It only reads: the file
// opens with mode=ro and no Hermes file is ever written. SQLite may make
// the -shm side file for a database in WAL mode; any reader of a live WAL
// database does.
//
// Hermes keeps one state.db per profile: the default profile in the Hermes
// root ($HERMES_HOME, else ~/.hermes) and a named profile in
// <root>/profiles/<name>. The tables are sessions and messages. Columns
// differ between Hermes versions, so each one is used only when it exists.
package hermesdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"daisugi-verify/pando"
)

// Root is the Hermes root: HERMES_HOME (or its grandparent when it is a
// profile dir), else home/.hermes.
func Root(hermesHome, home string) string {
	if h := strings.TrimSpace(hermesHome); h != "" {
		if filepath.Base(filepath.Dir(h)) == "profiles" {
			return filepath.Dir(filepath.Dir(h))
		}
		return h
	}
	return filepath.Join(home, ".hermes")
}

// Profile is one Hermes profile with a state.db.
type Profile struct {
	Name string // "default" for the root
	Path string // the state.db
}

// Profiles lists the profiles under root that have a state.db: the default
// first, then the named ones in name order. name, when not empty, keeps one.
func Profiles(root, name string) []Profile {
	var out []Profile
	add := func(n, dir string) {
		p := filepath.Join(dir, "state.db")
		if st, err := os.Stat(p); err == nil && !st.IsDir() && (name == "" || name == n) {
			out = append(out, Profile{Name: n, Path: p})
		}
	}
	add("default", root)
	ents, _ := os.ReadDir(filepath.Join(root, "profiles"))
	var names []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != "." && e.Name() != ".." {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		add(n, filepath.Join(root, "profiles", n))
	}
	return out
}

// Source reads the chats of the profiles under Root. It implements
// pando.Source.
type Source struct {
	Root string
	// Profile, when set, keeps one profile.
	Profile string

	// cursors is, for each profile, the id of the last message it logged.
	// A profile with a cursor is read by id: a message committed late with
	// an old time is not missed, and a profile that fails again and again
	// is not read from the start every poll. A profile with none (its first
	// poll) is read by time, from `since`.
	cursors map[string]int64
}

// Cursors implements pando.Stateful.
func (s *Source) Cursors() map[string]int64 {
	out := make(map[string]int64, len(s.cursors))
	for k, v := range s.cursors {
		out[k] = v
	}
	return out
}

// SetCursors implements pando.Stateful.
func (s *Source) SetCursors(m map[string]int64) {
	s.cursors = make(map[string]int64, len(m))
	for k, v := range m {
		s.cursors[k] = v
	}
}

// Name implements pando.Source.
func (s *Source) Name() string {
	if s.Profile != "" {
		return "hermes:" + s.Profile
	}
	return "hermes"
}

// Poll logs the messages of every profile: those after the profile's cursor,
// or, for a profile with none, those with a time at or after since. An
// unreadable profile is reported after the others are done; its cursor stays.
func (s *Source) Poll(ctx context.Context, c *pando.Chat, since time.Time) (int, error) {
	total := 0
	var errs []string
	if s.cursors == nil {
		s.cursors = map[string]int64{}
	}
	for _, p := range Profiles(s.Root, s.Profile) {
		cur, has := s.cursors[p.Name]
		n, last, reset, err := pollProfile(ctx, c, p, since, cur, has)
		switch {
		case reset:
			// The db was made again and its ids start over: the old cursor
			// would hide every new row. Take the new ones by time once, and
			// keep the id of the last as the cursor from here on.
			if last > 0 {
				s.cursors[p.Name] = last
			} else {
				delete(s.cursors, p.Name)
			}
		case last > 0 && (!has || last > cur):
			s.cursors[p.Name] = last
		}
		total += n
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.Name, err))
		}
	}
	if len(errs) > 0 {
		return total, errors.New(strings.Join(errs, "; "))
	}
	return total, nil
}

func openRO(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?mode=ro&_busy_timeout=2000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// pollProfile logs the new messages of one profile and returns how many it
// logged and the id of the last message it handled (0 when none). reset says
// the db holds ids below the cursor: it was made again.
func pollProfile(ctx context.Context, c *pando.Chat, p Profile, since time.Time, cursor int64, hasCursor bool) (n int, last int64, reset bool, err error) {
	db, err := openRO(p.Path)
	if err != nil {
		return 0, 0, false, err
	}
	defer db.Close()
	mc, err := columns(db, "messages")
	if err != nil || len(mc) == 0 {
		return 0, 0, false, err
	}
	if hasCursor {
		var top int64
		if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages`).Scan(&top); err != nil {
			return 0, 0, false, err
		}
		if top < cursor {
			hasCursor, reset = false, true
		}
	}
	sc, _ := columns(db, "sessions")
	col := func(has map[string]bool, table, name string) string {
		if has[name] {
			return "COALESCE(" + table + "." + name + ", '')"
		}
		return "''"
	}
	filter := " WHERE m.role IN ('user','assistant') AND COALESCE(m.content,'') <> ''"
	if mc["active"] {
		filter += " AND m.active = 1"
	}
	if mc["_compressed_summary"] {
		filter += " AND m._compressed_summary = 0"
	}
	args := []any{}
	if hasCursor {
		filter += " AND m.id > ?"
		args = append(args, cursor)
	} else if !since.IsZero() {
		filter += " AND m.timestamp >= ?"
		args = append(args, float64(since.Unix()))
	}
	join := ""
	if len(sc) > 0 {
		join = " LEFT JOIN sessions s ON s.id = m.session_id"
	}
	// A delegate_task child is a session with source 'subagent' (and, since
	// schema v16, a _delegate_from marker in model_config). It is a worker of
	// the steward, not the owner's chat: its rows are never logged.
	if sc["source"] {
		filter += " AND COALESCE(s.source, '') <> 'subagent'"
	}
	if sc["model_config"] {
		filter += " AND instr(COALESCE(s.model_config, ''), '_delegate_from') = 0"
	}
	scol := func(name string) string {
		if len(sc) == 0 {
			return "''"
		}
		return col(sc, "s", name)
	}
	q := `SELECT m.id, m.session_id, m.role, m.content, m.timestamp, ` +
		scol("source") + `, ` + scol("user_id") + `, ` + scol("display_name") + `, ` + scol("chat_id") + `, ` + scol("thread_id") +
		` FROM messages m` + join + filter + ` ORDER BY m.id`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, reset, err
	}
	defer rows.Close()
	type row struct {
		id                                  int64
		sess, role, text                    string
		at                                  float64
		source, user, display, chatID, thrd string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.sess, &r.role, &r.text, &r.at, &r.source, &r.user, &r.display, &r.chatID, &r.thrd); err != nil {
			return 0, 0, reset, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, reset, err
	}
	// A session a lead's plugin put on the store's session map belongs to
	// its branch: its rows go there, never into the store above (round 8b).
	rt := pando.NewRouter(c)
	defer rt.Close()
	for _, r := range all {
		target, err := rt.Chat(ctx, rt.Target("hermes:"+r.sess, "", ""))
		if err != nil {
			return n, last, reset, err
		}
		platform := r.source
		if platform == "" {
			platform = "hermes"
		}
		chat := r.sess
		if r.thrd != "" {
			chat += "/" + r.thrd
		}
		who, kind := p.Name, pando.KindSteward
		head := "hermes assistant"
		if r.role == "user" {
			kind = pando.KindUser
			who = firstNonEmpty(r.display, r.user, "user")
			head = "hermes user " + who
		}
		text := fmt.Sprintf("%s in %s (profile %s): %s", head, platform, p.Name, r.text)
		fresh, err := target.AppendLive(ctx, pando.Live{
			Kind: kind, Text: text, At: time.Unix(int64(r.at), 0).UTC(),
			Act:  fmt.Sprintf("hermes:%s:%s:%d", p.Name, r.sess, r.id),
			Meta: pando.Meta{Src: "hermes:" + p.Name, Chat: platform + ":" + chat, Who: who},
		})
		if err != nil {
			return n, last, reset, err
		}
		last = r.id
		if fresh {
			n++
		}
	}
	return n, last, reset, nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

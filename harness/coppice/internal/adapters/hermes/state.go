// Package hermes reads the conversation of a Hermes Agent session
// (Nous Research's hermes-agent) so the server can show it as messages.
//
// Hermes keeps its sessions in one SQLite file, state.db, in its home: the
// root is $HERMES_HOME, else ~/.hermes, and a named profile lives in
// <root>/profiles/<name>. The tables are `sessions` and `messages`, as
// hermes_state_common.py in the Hermes source defines them. This package
// only reads. It opens the file read-only and never writes a Hermes file.
// SQLite may still make the -shm side file for a database in WAL mode;
// that is how any reader of a live WAL database works.
package hermes

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure Go, no cgo: the binary builds with cgo only for ghostty
)

// chainMax is how many parent sessions Messages follows. A compression
// or a branch starts a new session whose parent holds the earlier talk.
const chainMax = 8

// Row is one message row of a session.
type Row struct {
	ID        int64
	Session   string
	Role      string
	Content   string
	ToolCalls string
	At        float64
}

// flagValue is the value of the flag spelled short or long in argv, in
// "-p v", "--profile v" and "--profile=v" form.
func flagValue(argv []string, short, long string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			return "", false
		case a == short || a == long:
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"="), true
		}
	}
	return "", false
}

// Home is the Hermes home a pane started with argv and env reads. The root
// is HERMES_HOME of env, else userHome/.hermes; when HERMES_HOME is itself a
// profile directory (its parent is "profiles") the root is two levels up,
// as Hermes finds it. -p/--profile NAME picks <root>/profiles/NAME. With no
// flag, HERMES_HOME that names a profile stands, else the root's
// active_profile file names the profile, else the root. It is "" when a
// profile name is not a plain name.
func Home(argv []string, env map[string]string, userHome string) string {
	root := filepath.Join(userHome, ".hermes")
	envHome := strings.TrimSpace(env["HERMES_HOME"])
	if envHome != "" {
		root = envHome
		if filepath.Base(filepath.Dir(envHome)) == "profiles" {
			root = filepath.Dir(filepath.Dir(envHome))
		}
	}
	name, ok := flagValue(argv, "-p", "--profile")
	if !ok && envHome != "" && root != envHome {
		return envHome
	}
	if !ok {
		// The sticky profile: `hermes profile use NAME` writes it.
		if b, err := os.ReadFile(filepath.Join(root, "active_profile")); err == nil {
			name = strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
		}
	}
	if name == "" || name == "default" {
		if ok && name == "" {
			return ""
		}
		return root
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return filepath.Join(root, "profiles", name)
}

// StatePath is the state.db under a Hermes home.
func StatePath(home string) string { return filepath.Join(home, "state.db") }

// subcommands end the words of a session name, as Hermes ends them.
var subcommands = map[string]bool{"chat": true, "model": true, "gateway": true, "setup": true, "login": true,
	"logout": true, "auth": true, "status": true, "cron": true, "doctor": true, "config": true, "skills": true,
	"tools": true, "mcp": true, "sessions": true, "update": true, "profile": true, "dashboard": true, "serve": true}

// SessionName is the session id or title argv's -r/--resume or
// -c/--continue names, or "". Hermes joins the words after the flag up to
// the next flag, so `-c Pokemon Agent Dev` is one name. A bare -c names no
// session: it means the latest.
func SessionName(argv []string) string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return ""
		}
		for _, long := range []string{"--resume=", "--continue="} {
			if strings.HasPrefix(a, long) {
				return strings.TrimPrefix(a, long)
			}
		}
		if a != "-r" && a != "--resume" && a != "-c" && a != "--continue" {
			continue
		}
		var parts []string
		for j := i + 1; j < len(argv) && !strings.HasPrefix(argv[j], "-") && !subcommands[argv[j]]; j++ {
			parts = append(parts, argv[j])
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// WorkspaceKey is what Hermes scopes a resume to: the git repository root
// that holds dir, else dir itself.
func WorkspaceKey(dir string) string {
	if dir == "" {
		return ""
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		up := filepath.Dir(d)
		if up == d {
			return dir
		}
		d = up
	}
}

// open opens path read-only. A missing file is errNone.
var errNone = errors.New("no state.db")

func open(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNone
		}
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// columns is the set of column names of table.
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

// cliFamily are the sources of a terminal run (newer Hermes tags its
// terminal chat "local"): Hermes resumes among
// these for -c. A gateway's sessions (Mattermost and the like) are not
// the terminal's.
var cliFamily = []string{"cli", "oneshot", "local"}

// Pick is the session a pane that resumes shows, chosen the way `hermes -c`
// chooses: the one name (id or title) the flags give, else the newest
// session of the terminal family that has a message, scoped to workspace
// first (the sessions whose git root or cwd is it) and then unscoped. It
// leaves out an archived or hidden session when the columns exist. It is ""
// when the file is missing or holds no such session.
func Pick(path, name, workspace string) (string, error) {
	db, err := open(path)
	if errors.Is(err, errNone) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer db.Close()
	sc, err := columns(db, "sessions")
	if err != nil || len(sc) == 0 {
		return "", err
	}
	if name != "" {
		if id := byName(db, sc, name); id != "" {
			return id, nil
		}
	}
	filter := terminalFilter(sc)
	query := func(extra string, args ...any) (string, error) {
		var id sql.NullString
		err := db.QueryRow(`SELECT s.id FROM sessions s JOIN messages m ON m.session_id = s.id
			WHERE 1=1`+filter+extra+` GROUP BY s.id ORDER BY MAX(m.timestamp) DESC, s.started_at DESC LIMIT 1`, args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return id.String, err
	}
	if workspace != "" && (sc["cwd"] || sc["git_repo_root"]) {
		var conds []string
		var args []any
		for _, col := range []string{"cwd", "git_repo_root"} {
			if sc[col] {
				conds = append(conds, "s."+col+" = ?")
				args = append(args, workspace)
			}
		}
		id, err := query(" AND ("+strings.Join(conds, " OR ")+")", args...)
		if err != nil || id != "" {
			return id, err
		}
	}
	return query("")
}

// terminalFilter is the WHERE terms that keep a session of the terminal
// family and leave out an archived or hidden one.
func terminalFilter(sc map[string]bool) string {
	filter := ""
	for _, col := range []string{"archived", "hidden"} {
		if sc[col] {
			filter += " AND s." + col + " = 0"
		}
	}
	if sc["source"] {
		filter += " AND s.source IN ('" + strings.Join(cliFamily, "','") + "')"
	}
	return filter
}

// PromptWindow is how many seconds before the prompt a session row may
// carry: the two clocks are read at different moments. PromptSpan is how
// many seconds after the prompt Hermes may take to write its row.
const (
	PromptWindow = 5.0
	PromptSpan   = 90.0
)

// squash folds runs of white space, so a prompt typed in a terminal matches
// the text Hermes stored.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// PickByPrompt is the session of a pane that has sent its first prompt: the
// terminal session whose first user message is that prompt, written between
// PromptWindow seconds before at and PromptSpan seconds after it. Hermes
// writes a session row at the first prompt, so the row is the pane's own
// when the text and the moment agree. Of several matches the one written
// closest to at wins, so two panes that send the same words each get their
// own. A session whose tip is in taken (the tips other panes hold) and a
// session that has a parent are passed over. It is "" when none matches.
func PickByPrompt(path, prompt string, at float64, taken []string) (string, error) {
	want := squash(prompt)
	if want == "" {
		return "", nil
	}
	return pickFirst(path, at, taken, func(content string) bool { return squash(content) == want }, func(a, b firstMsg) bool { return a.gap < b.gap })
}

// PickByTime is the fallback for a pane whose prompt text matched no session
// (the terminal showed one thing, Hermes stored another). It is the terminal
// session of the profile whose first user message was written in the same
// window as PickByPrompt reads, the closest to the prompt first, that no other
// pane holds.
// A session whose first message is in skip (squashed) is left for the pane
// that typed it. It is "" when none is free.
func PickByTime(path string, at float64, taken, skip []string) (string, error) {
	return pickFirst(path, at, taken, func(c string) bool { return !contains(skip, squash(c)) }, func(a, b firstMsg) bool { return a.gap < b.gap })
}

type firstMsg struct {
	id  string
	ts  float64
	gap float64
}

// pickFirst lists the parentless terminal sessions whose first user message
// is stamped inside the prompt window around at, keeps those whose text
// passes ok, orders them by less and returns the first one no other pane holds.
func pickFirst(path string, at float64, taken []string, ok func(string) bool, less func(a, b firstMsg) bool) (string, error) {
	if at <= 0 {
		return "", nil
	}
	db, err := open(path)
	if errors.Is(err, errNone) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer db.Close()
	sc, err := columns(db, "sessions")
	if err != nil || len(sc) == 0 {
		return "", err
	}
	filter := terminalFilter(sc)
	if sc["parent_session_id"] {
		filter += " AND s.parent_session_id IS NULL"
	}
	rows, err := db.Query(`SELECT s.id, m.content, m.timestamp FROM sessions s
		JOIN messages m ON m.id = (SELECT MIN(f.id) FROM messages f WHERE f.session_id = s.id AND f.role = 'user')
		WHERE m.timestamp >= ? AND m.timestamp <= ?`+filter, at-PromptWindow, at+PromptSpan)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cands []firstMsg
	for rows.Next() {
		var id, content sql.NullString
		var ts float64
		if err := rows.Scan(&id, &content, &ts); err != nil {
			return "", err
		}
		if !ok(content.String) {
			continue
		}
		gap := ts - at
		if gap < 0 {
			gap = -gap
		}
		cands = append(cands, firstMsg{id.String, ts, gap})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	rows.Close() // the one connection is free for tipIn
	sort.SliceStable(cands, func(i, j int) bool { return less(cands[i], cands[j]) })
	for _, c := range cands {
		if len(taken) == 0 || !contains(taken, tipIn(db, sc, c.id)) {
			return c.id, nil
		}
	}
	return "", nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Tip follows a compression from session id to the live session that
// replaced it, and is id when none did.
func Tip(path, id string) string {
	db, err := open(path)
	if err != nil {
		return id
	}
	defer db.Close()
	sc, err := columns(db, "sessions")
	if err != nil || len(sc) == 0 {
		return id
	}
	return tipIn(db, sc, id)
}

// Tips is Tip for several ids with one open of the database, in the order
// given. Each id is its own tip when the database cannot be read.
func Tips(path string, ids []string) []string {
	out := append([]string(nil), ids...)
	if len(ids) == 0 {
		return out
	}
	db, err := open(path)
	if err != nil {
		return out
	}
	defer db.Close()
	sc, err := columns(db, "sessions")
	if err != nil || len(sc) == 0 {
		return out
	}
	for i, id := range ids {
		out[i] = tipIn(db, sc, id)
	}
	return out
}

// tipIn is Tip on an open database.
func tipIn(db *sql.DB, sc map[string]bool, id string) string {
	if !sc["end_reason"] {
		return id
	}
	cur := id
	for i := 0; i < chainMax*2; i++ {
		var next sql.NullString
		err := db.QueryRow(`SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id
			WHERE p.id = ? AND p.end_reason = 'compression' ORDER BY c.started_at DESC LIMIT 1`, cur).Scan(&next)
		if err != nil || !next.Valid || next.String == "" {
			break
		}
		cur = next.String
	}
	return cur
}

// byName resolves an id or a title to a session id, and follows a
// compression forward to its live tip.
func byName(db *sql.DB, sc map[string]bool, name string) string {
	var id sql.NullString
	if err := db.QueryRow(`SELECT id FROM sessions WHERE id = ?`, name).Scan(&id); err != nil || !id.Valid {
		if !sc["title"] {
			return ""
		}
		if err := db.QueryRow(`SELECT id FROM sessions WHERE title = ? ORDER BY started_at DESC LIMIT 1`, name).Scan(&id); err != nil || !id.Valid {
			return ""
		}
	}
	cur := id.String
	if !sc["end_reason"] {
		return cur
	}
	for i := 0; i < chainMax*2; i++ {
		var next sql.NullString
		err := db.QueryRow(`SELECT c.id FROM sessions c JOIN sessions p ON p.id = c.parent_session_id
			WHERE p.id = ? AND p.end_reason = 'compression' ORDER BY c.started_at DESC LIMIT 1`, cur).Scan(&next)
		if err != nil || !next.Valid || next.String == "" {
			break
		}
		cur = next.String
	}
	return cur
}

// Messages is the newest limit message rows of session and of its parent
// sessions, oldest first. cut is true when older rows were left out. It
// leaves out a row Hermes marks inactive or as a compression summary.
func Messages(path, session string, limit int) (rows []Row, cut bool, err error) {
	db, err := open(path)
	if errors.Is(err, errNone) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	mc, err := columns(db, "messages")
	if err != nil || len(mc) == 0 {
		return nil, false, err
	}
	// The chain of sessions, tip first.
	chain := []string{session}
	for len(chain) < chainMax {
		var parent sql.NullString
		if err := db.QueryRow(`SELECT parent_session_id FROM sessions WHERE id = ?`, chain[len(chain)-1]).Scan(&parent); err != nil || !parent.Valid || parent.String == "" {
			break
		}
		seen := false
		for _, c := range chain {
			seen = seen || c == parent.String
		}
		if seen {
			break
		}
		chain = append(chain, parent.String)
	}
	toolCalls := "NULL"
	if mc["tool_calls"] {
		toolCalls = "tool_calls"
	}
	filter := ""
	if mc["active"] {
		filter += " AND active = 1"
	}
	if mc["_compressed_summary"] {
		filter += " AND _compressed_summary = 0"
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(chain)), ",")
	args := make([]any, 0, len(chain)+1)
	for _, c := range chain {
		args = append(args, c)
	}
	args = append(args, limit+1)
	q, err := db.Query(`SELECT id, session_id, role, COALESCE(content, ''), COALESCE(`+toolCalls+`, ''), timestamp
		FROM messages WHERE session_id IN (`+marks+`)`+filter+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer q.Close()
	for q.Next() {
		var r Row
		if err := q.Scan(&r.ID, &r.Session, &r.Role, &r.Content, &r.ToolCalls, &r.At); err != nil {
			return nil, false, err
		}
		rows = append(rows, r)
	}
	if err := q.Err(); err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		rows, cut = rows[:limit], true
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, cut, nil
}

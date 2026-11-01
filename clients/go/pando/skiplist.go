package pando

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"daisugi-verify/internal/pandoskip"
)

// SkipListName is the file beside the stores, <data>/pando/skip.sessions,
// that names the Claude Code sessions the live feed never logs: the chats of
// workers and internal model calls. One session id or transcript path per
// line. The file only grows; a line is never removed.
//
// A process marked DAISUGI_PANDO_SKIP writes its own session here from its
// hooks, and a spawner that starts `claude` for a worker writes the id it
// chose before the child runs. A cwd list cannot do this work: a worker runs
// in the project, where the owner works too.
const SkipListName = "skip.sessions"

// SkipListPath is the skip list of the stores in storeDir (their parent).
func SkipListPath(storeDir string) string {
	return filepath.Join(filepath.Dir(storeDir), SkipListName)
}

// SkipListIn is the skip list under a data home.
func SkipListIn(dataDir string) string {
	return filepath.Join(dataDir, "pando", SkipListName)
}

// AppendSkip adds ids (session ids or transcript paths) to the list in one
// O_APPEND write, so two processes cannot split a line. It takes no store
// lock: the list must be writable while the store is busy.
func AppendSkip(path string, ids ...string) error {
	var b strings.Builder
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || strings.ContainsAny(id, "\r\n") {
			continue
		}
		b.WriteString(id)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// SkipSet is the set of ids on the skip list.
type SkipSet map[string]bool

// LoadSkip reads the list. A missing or unreadable file is an empty set.
func LoadSkip(path string) SkipSet {
	set := SkipSet{}
	f, err := os.Open(path)
	if err != nil {
		return set
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			set[t] = true
		}
	}
	return set
}

// Has is whether the session is listed, by its id, its file name stem or its
// transcript path.
func (s SkipSet) Has(sessionID, path string) bool {
	if len(s) == 0 {
		return false
	}
	if path != "" {
		if s[path] {
			return true
		}
		if s[strings.TrimSuffix(filepath.Base(path), ".jsonl")] {
			return true
		}
	}
	return sessionID != "" && s[sessionID]
}

// NewSkippedSession mints a session id (a version 4 uuid, the form `claude
// --session-id` takes) and puts it on the skip list under dataDir before it
// returns. A spawner that starts `claude` for a worker or an internal call
// passes the id with --session-id: the transcript is then listed before it
// exists, and no poll can take its first line.
func NewSkippedSession(dataDir string) (string, error) {
	id, err := pandoskip.NewUUID()
	if err != nil {
		return "", err
	}
	if err := AppendSkip(SkipListIn(dataDir), id); err != nil {
		return "", err
	}
	return id, nil
}

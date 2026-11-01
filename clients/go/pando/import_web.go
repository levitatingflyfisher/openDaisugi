package pando

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PathMap is one --map OLD=NEW word (PD-R-9).
type PathMap struct{ Old, New string }

// ParseMap reads "OLD=NEW".
func ParseMap(s string) (PathMap, error) {
	k := strings.Index(s, "=")
	if k <= 0 || k == len(s)-1 {
		return PathMap{}, fmt.Errorf("--map needs OLD=NEW, not %q", s)
	}
	return PathMap{Old: filepath.Clean(s[:k]), New: filepath.Clean(s[k+1:])}, nil
}

// ParseMaps reads the OLD=NEW words the state saved. A bad word is left out.
func ParseMaps(words []string) []PathMap {
	var out []PathMap
	for _, w := range words {
		if m, err := ParseMap(w); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// project rewrites the front of a project path by the first map whose Old
// is the path or a parent of it by path element. Other paths stay.
func (o ImportOpts) project(p string) string {
	if p == "" {
		return p
	}
	cp := filepath.Clean(p)
	for _, m := range o.Map {
		if cp == m.Old {
			return m.New
		}
		if strings.HasPrefix(cp, strings.TrimRight(m.Old, "/")+"/") {
			return filepath.Join(m.New, cp[len(m.Old):])
		}
	}
	return p
}

type webMessage struct {
	UUID      string `json:"uuid"`
	Sender    string `json:"sender"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type webConversation struct {
	UUID     string       `json:"uuid"`
	Name     string       `json:"name"`
	Created  string       `json:"created_at"`
	Messages []webMessage `json:"chat_messages"`
}

func (m webMessage) text() string {
	if strings.TrimSpace(m.Text) != "" {
		return m.Text
	}
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// readWebFile reads conversations.json from a zip or from the file itself.
func readWebFile(path string) ([]webConversation, error) {
	var r io.ReadCloser
	if strings.HasSuffix(strings.ToLower(path), ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name == "conversations.json" || strings.HasSuffix(f.Name, "/conversations.json") {
				if r, err = f.Open(); err != nil {
					return nil, err
				}
				break
			}
		}
		if r == nil {
			return nil, errors.New("the export holds no conversations.json")
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		r = f
	}
	defer r.Close()
	var cs []webConversation
	if err := json.NewDecoder(bufio.NewReaderSize(r, 1<<20)).Decode(&cs); err != nil {
		return nil, fmt.Errorf("conversations.json is not a list of chats: %w", err)
	}
	return cs, nil
}

// WebKey is the queue key of a chat: its conversation uuid.
func WebKey(uuid string) string { return SrcWeb + ":" + uuid }

func webMessages(c webConversation, o ImportOpts) sessionRead {
	key := WebKey(c.UUID)
	r := sessionRead{key: key}
	where := ""
	if strings.TrimSpace(c.Name) != "" {
		where = " in " + strings.Join(strings.Fields(c.Name), " ")
	}
	seen := map[string]int{}
	for _, m := range c.Messages {
		role := m.Sender
		if role == "human" {
			role = "user"
		}
		text := m.text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		ts, ok := ParseImportTime(m.CreatedAt)
		if !ok {
			continue
		}
		if !o.Since.IsZero() && ts.Before(o.Since) {
			continue
		}
		act := key + ":" + m.UUID
		k := seen[act]
		seen[act]++
		if k > 0 {
			act += fmt.Sprintf(".%d", k)
		}
		r.msgs = append(r.msgs, ImportMsg{Text: "web " + role + where + ": " + text, TS: ts, Act: act})
	}
	return r
}

// readWebSession reads one chat of an export by its key.
func readWebSession(path, key string, o ImportOpts) (sessionRead, error) {
	cs, err := readWebFile(path)
	if err != nil {
		return sessionRead{}, err
	}
	for _, c := range cs {
		if WebKey(c.UUID) == key {
			return webMessages(c, o), nil
		}
	}
	return sessionRead{}, fmt.Errorf("chat %s is not in %s", key, path)
}

// ScanWeb lists the chats of a claude.ai export, oldest first: one session
// per chat, keyed by its conversation uuid; a message is keyed by that and
// its own uuid, so a message uuid that two chats share is logged once in
// each (PD-R-8). New counts the messages the log does not hold. A chat with
// none is still listed, so the proposal can say why it is skipped. has says
// whether an act id is in the store already; nil means none is.
func ScanWeb(path string, o ImportOpts, has func(string) bool) ([]ImportSession, error) {
	cs, err := readWebFile(path)
	if err != nil {
		return nil, err
	}
	var out []ImportSession
	for _, c := range cs {
		r := webMessages(c, o)
		fresh := newMsgs(r, "", has)
		s := ImportSession{Key: r.key, Src: SrcWeb, Path: path, New: len(fresh), Title: c.Name, Total: len(r.msgs)}
		if len(fresh) > 0 {
			s.First = fresh[0].TS
		} else if len(r.msgs) > 0 {
			s.First = r.msgs[0].TS
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].First.Equal(out[b].First) {
			return out[a].First.Before(out[b].First)
		}
		return out[a].Key < out[b].Key
	})
	return out, nil
}

// WriteProposal writes one line per chat: key, proposed store, reason and
// title, split by tabs. Nothing is written to any store (PD-R-7).
func WriteProposal(w io.Writer, ss []ImportSession) error {
	for _, s := range ss {
		store, reason := DefaultStore, fmt.Sprintf("%s to import", plural(s.New, "message"))
		switch {
		case s.Total == 0:
			store, reason = "skip", "the chat is empty"
		case s.New == 0:
			store, reason = "skip", "every message is in the log already"
		}
		title := strings.Join(strings.Fields(s.Title), " ")
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Key, store, reason, title); err != nil {
			return err
		}
	}
	return nil
}

// ProposalChoice is one accepted line.
type ProposalChoice struct{ Key, Store string }

// ReadProposal reads a proposal file the owner has checked. A store of
// "skip" is kept as is; "all" and a bad name are refused (PD-R-2).
func ReadProposal(r io.Reader) ([]ProposalChoice, error) {
	var out []ProposalChoice
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: need a key and a store, split by a tab", n)
		}
		st := strings.TrimSpace(f[1])
		if st == "all" {
			return nil, fmt.Errorf("line %d: %w", n, ErrStoreAll)
		}
		if st != "skip" && !ValidStore(st) {
			return nil, fmt.Errorf("line %d: bad store name %q", n, st)
		}
		out = append(out, ProposalChoice{Key: f[0], Store: st})
	}
	return out, sc.Err()
}

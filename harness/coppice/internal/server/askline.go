package server

import (
	"os"
	"strings"
	"sync"

	"github.com/opendaisugi/coppice/internal/layout"
	"github.com/opendaisugi/coppice/internal/proto"
)

// askRunes caps the one-line ask a pane.list row carries.
const askRunes = 200

// askLineOf is the question an agent's last message ends on, as one line,
// or "". The last message must be the agent's own words (not a tool call,
// and nothing from the owner after it), and its last line must end in a
// question mark once markup is trimmed.
func askLineOf(msgs []message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "note" {
			continue
		}
		if m.Role != "agent" || m.Tool != "" {
			return ""
		}
		lines := strings.Split(strings.TrimSpace(m.Text), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if strings.HasSuffix(strings.TrimRight(last, " *_`\"')]"), "?") {
			return capRunes(oneLine(last, askRunes), askRunes)
		}
		return ""
	}
	return ""
}

// askFields holds the asks read from transcripts.
type askFields struct {
	askMu sync.Mutex
	// askCache is the ask each transcript ended on, by path.
	askCache map[string]askCacheEntry
}

// askCacheEntry is the ask read from a transcript of one size and time.
type askCacheEntry struct {
	size, mod int64
	line      string
}

// lastAskLine is the ask pane p's last message ends on, for a pane that
// waits idle. It reads a transcript file again only when the file changed.
// A pane with no file the server can read, a Hermes pane included, has none.
func (s *Server) lastAskLine(p layout.Pane) string {
	if p.Closed || p.Harness == "" || isHermes(p) || s.isTrackedSteward(p.ID) {
		return ""
	}
	path, kind := s.transcriptOf(p)
	if path == "" || kind == kindHermes {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	size, mod := fi.Size(), fi.ModTime().UnixNano()
	s.askMu.Lock()
	if e, ok := s.askCache[path]; ok && e.size == size && e.mod == mod {
		s.askMu.Unlock()
		return e.line
	}
	s.askMu.Unlock()
	msgs, _, err := readMessages(path, kind)
	if err != nil {
		return ""
	}
	line := askLineOf(msgs)
	s.askMu.Lock()
	if s.askCache == nil {
		s.askCache = map[string]askCacheEntry{}
	}
	if len(s.askCache) > 256 {
		s.askCache = map[string]askCacheEntry{}
	}
	s.askCache[path] = askCacheEntry{size, mod, line}
	s.askMu.Unlock()
	return line
}

// addAskLine puts last_ask on a pane.list row for an idle pane whose last
// message asks the owner something.
func (s *Server) addAskLine(row map[string]any, p layout.Pane, state string) {
	if state != proto.StateIdle {
		return
	}
	if line := s.lastAskLine(p); line != "" {
		row["last_ask"] = line
	}
}

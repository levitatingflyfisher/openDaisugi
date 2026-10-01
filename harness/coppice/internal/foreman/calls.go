package foreman

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The write-ahead log of the foreman's acting tool calls (FM-R-66). Before
// a tool effect (the gated spawn) the server writes a start line, fsynced;
// after the effect it writes an end line. A start with no end, found when
// the chat opens, is a call whose outcome is unknown: it is never run again.

// CallsFile is the file in the log dir that holds the call lines.
const CallsFile = "calls.jsonl"

// Phases of a call line, and the outcomes of an end line.
const (
	CallStart   = "start"
	CallEnd     = "end"
	CallOK      = "ok"
	CallFailed  = "error"
	CallUnknown = "unknown"
)

// maxCallArgs caps the arguments kept in a start line.
const maxCallArgs = 4096

// CallRecord is one line of calls.jsonl.
type CallRecord struct {
	// Call names the call: c<turn>.<n>.
	Call  string `json:"call"`
	Phase string `json:"phase"`
	// Turn is the owner message the turn answers, on a start line.
	Turn int64 `json:"turn,omitempty"`
	// Tool and Args are the call, on a start line. Args is JSON text.
	Tool string `json:"tool,omitempty"`
	Args string `json:"args,omitempty"`
	At   string `json:"at"`
	// Outcome is ok, error or unknown, on an end line.
	Outcome string `json:"outcome,omitempty"`
}

// callsMu orders the writers of one calls.jsonl: two spawns at once in one
// turn must not cut each other's line on an error path (appendLine undoes
// to the size it saw).
var callsMu sync.Mutex

// AppendCall adds one line to calls.jsonl in dir, 0600, fsynced. The args of
// a start line are cut at 4 KiB. A torn tail left by a crash is cut back to
// the last newline first, so the new line starts whole (FM-R-67).
func AppendCall(dir string, r CallRecord) error {
	r.Args = clip(r.Args, maxCallArgs)
	line, err := encodeLine(r)
	if err != nil {
		return err
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	path := filepath.Join(dir, CallsFile)
	if err := repairTail(path); err != nil {
		return err
	}
	_, err = appendLine(path, line)
	return err
}

// RepairCalls cuts a torn tail of calls.jsonl in dir back to its last
// newline, fsynced. A missing file is fine.
func RepairCalls(dir string) error {
	callsMu.Lock()
	defer callsMu.Unlock()
	return repairTail(filepath.Join(dir, CallsFile))
}

// repairTail truncates the file at path to its last newline when it does
// not end in one. The cut text was never a whole line, so no reader used it.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	// Find the last newline, reading back in blocks.
	cut := int64(0)
	const block = 4096
	buf := make([]byte, block)
	for end := st.Size(); end > 0; {
		start := max(end-block, 0)
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			return err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			cut = start + int64(i) + 1
			break
		}
		end = start
	}
	if err := f.Truncate(cut); err != nil {
		return err
	}
	return f.Sync()
}

// ReadCalls reads calls.jsonl in dir. A missing file is no calls, and a line
// cut by a crash or not JSON is skipped.
func ReadCalls(dir string) ([]CallRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, CallsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []CallRecord
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r CallRecord
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// DanglingCalls are the start lines that have no end line, oldest first.
func DanglingCalls(recs []CallRecord) []CallRecord {
	ended := map[string]bool{}
	for _, r := range recs {
		if r.Phase == CallEnd {
			ended[r.Call] = true
		}
	}
	var out []CallRecord
	for _, r := range recs {
		if r.Phase == CallStart && !ended[r.Call] {
			out = append(out, r)
		}
	}
	return out
}

// TurnHasToolStart reports whether the log holds a start line for the turn
// that answers message turn. Such a turn is not run again on another
// account, whatever the memory of the process says (FM-R-50).
func TurnHasToolStart(recs []CallRecord, turn int64) bool {
	for _, r := range recs {
		if r.Phase == CallStart && r.Turn == turn {
			return true
		}
	}
	return false
}

// CarryDanglingCalls copies the start lines with no end from the call log
// in from (a history moved aside) into the call log in to, with turn -1
// and a new id, `m<unix>-<hex>.<old id>`, which no socket makes (the new chat
// starts its message ids again, so an old c5.1 would meet a new c5.1 and the
// old end line would hide the new start). The turn numbers of the old history
// mean nothing in the new chat, and -1 matches no turn. The next open reports each once, like any dangling call
// (FM-R-66). Act ids are not carried: the old words are not in the new log.
func CarryDanglingCalls(from, to string) error {
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	prefix := fmt.Sprintf("m%d-%x.", time.Now().Unix(), rnd)
	recs, err := ReadCalls(from)
	if err != nil {
		return err
	}
	for _, r := range DanglingCalls(recs) {
		// A new id that no socket can make: the new chat starts its message
		// ids again from 0, so an old c5.1 would meet a new c5.1 and the old
		// end line would hide the new start.
		r.Call, r.Turn = prefix+r.Call, -1
		if err := AppendCall(to, r); err != nil {
			return err
		}
	}
	return nil
}

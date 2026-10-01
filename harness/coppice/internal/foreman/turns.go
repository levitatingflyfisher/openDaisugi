package foreman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TurnsFile is the file in the log dir that records the foreman's turns.
const TurnsFile = "turns.jsonl"

// Turn statuses. A turn writes a line when it starts, with status
// "running", and one when it ends.
const (
	TurnRunning = "running"
	TurnOK      = "ok"
	TurnFailed  = "failed"
	TurnTimeout = "timeout"
)

// TurnRecord is one line of turns.jsonl.
type TurnRecord struct {
	// Msg is the id of the owner's message the turn answers.
	Msg     int64      `json:"msg"`
	Started string     `json:"started"`
	Ended   string     `json:"ended,omitempty"`
	Status  string     `json:"status"`
	Usage   *CallUsage `json:"usage,omitempty"`
}

// AppendTurn adds one record to turns.jsonl in dir, 0600, fsynced.
func AppendTurn(dir string, r TurnRecord) error {
	line, err := encodeLine(r)
	if err != nil {
		return err
	}
	_, err = appendLine(filepath.Join(dir, TurnsFile), line)
	return err
}

// ReadTurns reads turns.jsonl in dir. A missing file is no turns, and a
// last line cut by a crash is skipped.
func ReadTurns(dir string) ([]TurnRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, TurnsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []TurnRecord
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r TurnRecord
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// TurnStarted reports whether a turn for message msg has started, by the
// records of turns.jsonl.
func TurnStarted(recs []TurnRecord, msg int64) bool {
	for _, r := range recs {
		if r.Msg == msg {
			return true
		}
	}
	return false
}

// Stats is the summary `coppice foreman stats` prints: the turns by
// status and the share of input tokens that the cache read.
func Stats(recs []TurnRecord) string {
	var ok, failed, timeout, running int
	var in, read, write, out int
	for _, r := range recs {
		switch r.Status {
		case TurnOK:
			ok++
		case TurnFailed:
			failed++
		case TurnTimeout:
			timeout++
		default:
			running++
			continue
		}
		if r.Usage != nil {
			in += r.Usage.Input
			read += r.Usage.CacheRead
			write += r.Usage.CacheWrite
			out += r.Usage.Output
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "turns: %d (ok %d, failed %d, timeout %d)\n", ok+failed+timeout, ok, failed, timeout)
	total := in + read + write
	if total == 0 {
		b.WriteString("cache read share: none yet (no turn has reported tokens)\n")
	} else {
		fmt.Fprintf(&b, "cache read share: %.1f%% of %d input tokens (fresh %d, cache read %d, cache write %d); output %d\n",
			100*float64(read)/float64(total), total, in, read, write, out)
	}
	return b.String()
}

// HookMain is the PreToolUse hook of a turn: `coppice foreman hook`. It
// reads the hook's JSON from in and exits 2 for any tool that is not one
// of the foreman's four, so a built-in that a Claude Code update adds is
// still refused.
func HookMain(in io.Reader, errw io.Writer) int {
	data, _ := io.ReadAll(io.LimitReader(in, 1<<20))
	var p struct {
		ToolName string `json:"tool_name"`
	}
	if json.Unmarshal(data, &p) == nil {
		for _, n := range ToolNames {
			if p.ToolName == "mcp__foreman__"+n {
				return 0
			}
		}
	}
	fmt.Fprintf(errw, "the foreman may use only its four tools (%s). %q is not one of them.\n",
		strings.Join(ToolNames, ", "), p.ToolName)
	return 2
}

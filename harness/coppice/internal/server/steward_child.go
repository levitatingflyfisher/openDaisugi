package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"daisugi-verify/pando"
)

// A worker's whole transcript is kept as a small Pando store of its own,
// beside the steward's: <store>/children/<label>/ (ADR-0023, addendum A).
// It is never merged into the steward's tree and has no view in it. The
// worker's report lands in the steward's log as one work message; that
// message names the child store and its root ids, and zoom with child=<label>
// goes down from there to the word.

// childToolBytes caps the text of a tool call or its result in a child
// store, as the importer caps them.
const childToolBytes = 2000

// childMessage is one message of a child store.
type childMessage struct {
	kind, text string
	at         time.Time
}

// childMessagesOf reads a sprig session file: the brief, what the worker
// said, and each tool call with its result. In a child store the kind
// "steward" is the worker's own words. A verdict row is the warden's and is
// left out.
func childMessagesOf(path string) ([]childMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []childMessage
	type call struct {
		name, input string
		at          time.Time
	}
	calls := map[string]*call{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64<<20)
	atOf := func(v map[string]any) time.Time {
		if ts, ok := v["ts"].(float64); ok {
			sec := int64(ts)
			return time.Unix(sec, 0).UTC()
		}
		return time.Time{}
	}
	for sc.Scan() {
		var v map[string]any
		if json.Unmarshal(sc.Bytes(), &v) != nil {
			continue
		}
		at := atOf(v)
		switch v["type"] {
		case "prompt":
			if t, _ := v["text"].(string); strings.TrimSpace(t) != "" {
				out = append(out, childMessage{pando.KindUser, t, at})
			}
		case "assistant":
			if t, _ := v["text"].(string); strings.TrimSpace(t) != "" {
				out = append(out, childMessage{pando.KindSteward, t, at})
			}
		case "tool_call":
			id, _ := v["toolUseId"].(string)
			name, _ := v["name"].(string)
			detail, _ := v["detail"].(string)
			if detail == "" {
				b, _ := json.Marshal(v["input"])
				detail = string(b)
			}
			calls[id] = &call{name: name, input: pando.Clip(detail, childToolBytes), at: at}
			order = append(order, id)
		case "tool_result":
			id, _ := v["toolUseId"].(string)
			c := calls[id]
			if c == nil {
				continue
			}
			ok, _ := v["ok"].(bool)
			sum, _ := v["summary"].(string)
			state := "ok"
			if !ok {
				state = "failed"
			}
			out = append(out, childMessage{pando.KindTool,
				fmt.Sprintf("%s(%s)\n%s: %s", c.name, c.input, state, pando.Clip(sum, childToolBytes)), c.at})
			delete(calls, id)
		}
	}
	// A call with no result (the worker ended inside it) is kept as it is.
	for _, id := range order {
		if c := calls[id]; c != nil {
			out = append(out, childMessage{pando.KindTool, fmt.Sprintf("%s(%s)\nno result", c.name, c.input), c.at})
		}
	}
	return out, sc.Err()
}

// recordChild writes the transcript of a worker into its child store and
// returns the line the work message carries: the store's name and its root
// ids. A child that already holds messages is not written again. It returns
// "" when the worker left no transcript; the report then stands alone.
func (s *Server) recordChild(fc *stewardChat, w *workerRec, sessionFile string) string {
	if fc.store == "" || sessionFile == "" || !pando.ValidChild(w.Label) {
		return ""
	}
	msgs, err := childMessagesOf(sessionFile)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	// A worker of a branch keeps its transcript under that branch: the
	// branch's lead and everyone above it see it, a sibling never does.
	base := fc.store
	if w.Branch != "" {
		d, err := pando.EnsureBranch(fc.store, w.Branch, s.clock)
		if err != nil {
			s.noteOnce("The transcript of " + w.Label + " could not be kept: " + err.Error())
			return ""
		}
		base = d
	}
	c, err := pando.OpenChild(fc.ctx, base, w.Label, pando.Config{Now: s.clock})
	if err != nil {
		s.noteOnce("The transcript of " + w.Label + " could not be kept: " + err.Error())
		return ""
	}
	defer c.Close()
	if c.Count() == 0 {
		for _, m := range msgs {
			at := m.at
			if at.IsZero() {
				at = s.clock()
			}
			if _, err := c.AppendAt(fc.ctx, m.kind, m.text, at); err != nil {
				s.noteOnce("The transcript of " + w.Label + " could not be kept: " + err.Error())
				break
			}
		}
	}
	fc.markChild(c.Dir())
	roots := c.ChildRoots(w.Label)
	for i, r := range roots {
		roots[i] = strings.TrimPrefix(r, w.Label+":")
	}
	if w.Branch != "" {
		return fmt.Sprintf("(transcript: child store %s in branch %s, roots %s; zoom with branch=%q child=%q)",
			w.Label, w.Branch, strings.Join(roots, ", "), w.Branch, w.Label)
	}
	return fmt.Sprintf("(transcript: child store %s, roots %s; zoom with child=%q)", w.Label, strings.Join(roots, ", "), w.Label)
}

// markChild says a child store (by its dir) has nodes to build, and wakes
// the compactor.
func (fc *stewardChat) markChild(dir string) {
	fc.mu.Lock()
	fc.kids = append(fc.kids, dir)
	fc.mu.Unlock()
	fc.wake()
}

// tendKids builds the nodes of the child stores that were written since the
// last call, with the steward's own summarizer and its rule.
func (s *Server) tendKids(fc *stewardChat, ctx context.Context) error {
	fc.mu.Lock()
	kids := fc.kids
	fc.kids = nil
	fc.mu.Unlock()
	for k, dir := range kids {
		if _, err := pando.TendDir(ctx, dir, fc.sum, 0, s.clock); err != nil {
			fc.mu.Lock()
			fc.kids = append(fc.kids, kids[k:]...)
			fc.mu.Unlock()
			return err
		}
	}
	return nil
}

// zoomChild answers zoom with child=<label> or branch=<path>: the id and n
// are inside that worker's own store, or inside that branch (and its
// worker, with both). The store is opened read only, by names that cannot
// leave the steward's store, so a zoom never reads another store.
func (s *Server) zoomChild(fc *stewardChat, label string, args map[string]any) (string, error) {
	if fc.store == "" {
		return "", errNoChat
	}
	if _, ok := args["child"]; !ok && label != "" {
		args = maps.Clone(args)
		args["child"] = label
	}
	return pando.ToolZoomScope(pando.Scope{Store: fc.store, Dir: fc.store}, args)
}

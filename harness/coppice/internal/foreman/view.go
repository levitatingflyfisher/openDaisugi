package foreman

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// viewState is the view as view.json keeps it: how many messages it
// covers, whether a batch merge is still running, and its lines oldest
// first.
type viewState struct {
	Count int64
	Batch bool
	Lines []NodeID
}

// viewFile is view.json on disk. Each line is an [l, i] pair.
type viewFile struct {
	Count int64      `json:"count"`
	Batch bool       `json:"batch"`
	Lines [][2]int64 `json:"lines"`
}

func (v viewState) encode() ([]byte, error) {
	f := viewFile{Count: v.Count, Batch: v.Batch, Lines: make([][2]int64, len(v.Lines))}
	for k, id := range v.Lines {
		f.Lines[k] = [2]int64{int64(id.L), id.I}
	}
	return encodeLine(f)
}

// readViewFile reads view.json, or returns nil when there is none.
func (c *Chat) readViewFile() (*viewFile, error) {
	data, err := readFile(c.viewPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f viewFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, corrupt("view.json is not valid JSON", fmt.Errorf("foreman: %s: %w", c.viewPath(), err))
	}
	return &f, nil
}

// applyView checks a read view.json against the log and takes it. No file
// is an empty view.
func (c *Chat) applyView(f *viewFile) error {
	if f == nil {
		c.view = viewState{}
		return nil
	}
	v := viewState{Count: f.Count, Batch: f.Batch, Lines: make([]NodeID, len(f.Lines))}
	next := int64(0)
	for k, p := range f.Lines {
		if p[0] < 0 || p[0] > maxLevel || p[1] < 0 || p[1] > (int64(1)<<(maxLevel-p[0])) {
			return mismatch(fmt.Errorf("foreman: %s: line %d is not a node", c.viewPath(), k))
		}
		id := NodeID{L: int(p[0]), I: p[1]}
		if id.First() != next {
			return mismatch(fmt.Errorf("foreman: %s: line %d starts at %d, want %d", c.viewPath(), k, id.First(), next))
		}
		next = id.Last() + 1
		v.Lines[k] = id
	}
	if next != v.Count || v.Count > int64(len(c.msgs)) {
		return mismatch(fmt.Errorf("foreman: %s covers %d messages but says %d; the log has %d",
			c.viewPath(), next, v.Count, len(c.msgs)))
	}
	c.view = v
	return nil
}

func (c *Chat) saveView() error {
	data, err := c.view.encode()
	if err != nil {
		return err
	}
	if err := c.atomic(c.viewPath(), data); err != nil {
		return &viewSaveError{err}
	}
	return nil
}

// viewSaveError is a failed save of view.json.
type viewSaveError struct{ err error }

func (e *viewSaveError) Error() string { return e.err.Error() }
func (e *viewSaveError) Unwrap() error { return e.err }

// ViewJSON is view.json as the chat would write it now.
func (c *Chat) ViewJSON() ([]byte, error) { return c.view.encode() }

// ViewLines are the view's nodes, oldest first.
func (c *Chat) ViewLines() []NodeID { return append([]NodeID(nil), c.view.Lines...) }

// flat puts a text on one line.
func flat(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// line renders one view line: "id+n|text" and a newline.
func (c *Chat) line(id NodeID) string {
	text := Unbuilt
	if n, ok := c.nodes[id]; ok {
		text = n.Text
	}
	return id.Name() + "|" + flat(text) + "\n"
}

// ViewLine is one rendered view line: its node and its text, "id|text"
// and a newline.
type ViewLine struct {
	ID   NodeID
	Text string
}

// ViewText renders the view's lines with esc applied to the text of each,
// so a summary cannot close the tag around the view. The line that is the
// single node of message skip is left out: the turn that answers that
// message sends it apart. A skip below 0 leaves nothing out. A merged line
// that covers the message stays, since it is a summary and not the words.
func (c *Chat) ViewText(esc func(string) string, skip int64) []ViewLine {
	out := make([]ViewLine, 0, len(c.view.Lines))
	for _, id := range c.view.Lines {
		if skip >= 0 && id.L == 0 && id.I == skip {
			continue
		}
		text := Unbuilt
		if n, ok := c.nodes[id]; ok {
			text = n.Text
		}
		out = append(out, ViewLine{ID: id, Text: id.Name() + "|" + esc(flat(text)) + "\n"})
	}
	return out
}

// ViewSize is the size of the view's lines in bytes, the number the
// bounds hold.
func (c *Chat) ViewSize() int {
	s := 0
	for _, id := range c.view.Lines {
		s += len(c.line(id))
	}
	return s
}

// Render is the view as a turn sees it: the lines inside chat tags.
func (c *Chat) Render() string {
	var b strings.Builder
	b.WriteString("<chat>\n")
	for _, id := range c.view.Lines {
		b.WriteString(c.line(id))
	}
	b.WriteString("</chat>\n")
	return b.String()
}

// viewStep adds message t to the view and runs the sawtooth: once the
// view is over ViewMax, a batch merges the most due pairs until it is at
// most ViewMin. A batch that runs out of built pairs goes on at the next
// message.
func (c *Chat) viewStep(t int64) {
	v := &c.view
	v.Lines = append(v.Lines, NodeID{L: 0, I: t})
	v.Count = t + 1
	size := c.ViewSize()
	if size > c.cfg.ViewMax {
		v.Batch = true
	}
	if !v.Batch {
		return
	}
	built := func(id NodeID) bool { _, ok := c.nodes[id]; return ok }
	p := newPlanner(v.Lines, v.Count, built)
	for size > c.cfg.ViewMin {
		k, ok := p.pop()
		if !ok {
			break
		}
		size -= len(c.line(p.ids[k])) + len(c.line(p.ids[p.next[k]]))
		size += len(c.line(p.merge(k)))
	}
	if size <= c.cfg.ViewMin {
		v.Batch = false
	}
	v.Lines = p.lines()
}

// mismatch is a view.json that does not match the log.
func mismatch(err error) error { return corrupt("view.json does not match the log", err) }

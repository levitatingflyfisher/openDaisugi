package pando

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// ViewContextOf is Chat.ViewContext for the store in dir, built from what
// the view needs: view.json, the tree, and the last 20 messages. It does
// not parse the log. The Hermes plugin asks on every turn, and a household
// log grows without bound; reading and decoding all of it each time would
// cost more with each month. The text is the same bytes the full read gives
// (a test holds the two equal), so it changes only when the memory does.
//
// Any store that is not plain gets the full read: no view.json, a bad or
// torn line near the end, a log whose ids do not run on from the line count,
// a tree with a bad line.
func ViewContextOf(dir string) (string, error) {
	if text, ok := viewContextFast(dir); ok {
		return text, nil
	}
	c, err := OpenReadOnly(dir)
	if err != nil {
		return "", err
	}
	return c.ViewContext(), nil
}

// contextTail is how many of the newest messages the view text can name.
const contextTail = 20

func viewContextFast(dir string) (string, bool) {
	cfg := Config{Dir: dir}
	cfg.fill()
	if _, err := os.Stat(dir); err != nil || storeLinks(dir) != nil {
		return "", false
	}
	c := &Chat{cfg: cfg, readOnly: true}
	vf, err := c.readViewFile()
	if err != nil || vf == nil {
		return "", false
	}
	files, err := dayFiles(c.mainDir())
	if err != nil || len(files) == 0 {
		return "", false
	}
	// Count the lines of every file without decoding them.
	counts := make([]int64, len(files))
	var total int64
	for k, f := range files {
		n, err := countLines(filepath.Join(c.mainDir(), f))
		if err != nil {
			return "", false
		}
		counts[k] = n
		total += n
	}
	if total == 0 || vf.Count > total {
		return "", false
	}
	// Decode the last lines, newest file first, and check that their ids run
	// on from the count: that proves no line was lost or doubled before them.
	want := int64(contextTail)
	if want > total {
		want = total
	}
	refs := make([]msgRef, total)
	next := total
	for k := len(files) - 1; k >= 0 && next > total-want; k-- {
		path := filepath.Join(c.mainDir(), files[k])
		data, err := readFile(path)
		if err != nil {
			return "", false
		}
		end := bytes.LastIndexByte(data, '\n') + 1
		data = data[:end]
		// Walk the file's lines, remembering offsets.
		type ln struct{ off, n int }
		var lines []ln
		for off := 0; off < len(data); {
			e := bytes.IndexByte(data[off:], '\n')
			lines = append(lines, ln{off, e})
			off += e + 1
		}
		if int64(len(lines)) != counts[k] {
			return "", false // the file changed under the read
		}
		for j := len(lines) - 1; j >= 0 && next > total-want; j-- {
			l := lines[j]
			var m Message
			if json.Unmarshal(data[l.off:l.off+l.n], &m) != nil {
				return "", false
			}
			next--
			if m.I != next {
				return "", false
			}
			m.Kind = normKind(m.Kind)
			refs[next] = msgRef{file: path, off: int64(l.off), n: l.n, date: m.Date, past: m.Kind == KindPast}
		}
	}
	if next != total-want {
		return "", false
	}
	c.msgs = refs
	if err := c.loadTree(); err != nil || len(c.badTree) > 0 || len(c.tornParts) > 0 {
		return "", false
	}
	if err := c.applyView(vf); err != nil {
		return "", false
	}
	return c.ViewContext(), true
}

// countLines is the number of whole lines (ending in a newline) in a file.
func countLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n int64
	buf := make([]byte, 1<<16)
	for {
		k, err := f.Read(buf)
		n += int64(bytes.Count(buf[:k], []byte{'\n'}))
		if err != nil {
			break
		}
	}
	return n, nil
}

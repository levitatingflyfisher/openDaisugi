package gateway

import (
	"errors"
	"os"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gateway_answers.AnswerStore and capture_answer: the
// bounded ring of captured answers, one JSON line each.

var answerFields = []string{"signature", "task", "answer", "created_at", "ground_hash"}

// AnswerEntry is one loaded line, its values as the file held them.
type AnswerEntry struct {
	Signature, Task, Answer, CreatedAt, GroundHash any
}

func (e AnswerEntry) json() string {
	o := pyjson.NewObjectCap(5)
	o.Set("signature", e.Signature).Set("task", e.Task).Set("answer", e.Answer)
	o.Set("created_at", e.CreatedAt).Set("ground_hash", e.GroundHash)
	return pyjson.Dumps(o, true)
}

// pyTime is time.time() for t.
func pyTime(t time.Time) float64 {
	ns := t.UnixNano()
	if ns%1e9 == 0 {
		return float64(ns / 1e9)
	}
	return float64(ns) / 1e9
}

// CaptureAnswer is capture_answer(AnswerStore(path, max_entries=max),
// task=task, answer=answer, created_at=time.time()).
func CaptureAnswer(path string, max int, task, answer string, now time.Time) error {
	sig, err := TurnSignature(task)
	if err != nil {
		return err
	}
	e := AnswerEntry{Signature: sig, Task: task, Answer: answer, CreatedAt: pyTime(now)}
	if err := appendLine(path, e.json()); err != nil {
		return err
	}
	entries, err := LoadAnswers(path)
	if err != nil {
		return err
	}
	if len(entries) <= max {
		return nil
	}
	var b strings.Builder
	for _, e := range entries[len(entries)-max:] {
		b.WriteString(e.json())
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// errUnreadable is a file the oracle raises on while reading it.
var errUnreadable = errors.New("unreadable")

// LoadAnswers is AnswerStore.load(): a line that is not JSON, or not an
// entry, is skipped. A file that is not UTF-8, or a line json.loads
// raises something other than JSONDecodeError on, is an error.
func LoadAnswers(path string) ([]AnswerEntry, error) {
	objs, err := loadRecords(path, answerFields[:4])
	if err != nil {
		return nil, err
	}
	out := make([]AnswerEntry, len(objs))
	for i, o := range objs {
		out[i] = AnswerEntry{Signature: o.Value("signature"), Task: o.Value("task"), Answer: o.Value("answer"),
			CreatedAt: o.Value("created_at"), GroundHash: o.Value("ground_hash")}
	}
	return out, nil
}

// loadRecords reads a JSONL store the way GatewayJournal.load and
// AnswerStore.load do: each line a dict holding every required field.
func loadRecords(path string, required []string) ([]*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	text, exc := pystr.DecodeStrict(raw)
	if exc != nil {
		return nil, errUnreadable
	}
	var out []*pyjson.Object
	for _, line := range pystr.Splitlines(text) {
		line = pystr.Strip(line)
		if line == "" {
			continue
		}
		v, derr := pyjson.LoadsPy(line, maxJSONDepth)
		if derr != nil {
			if derr.NotJSON || derr.TooDeep {
				return nil, errUnreadable
			}
			continue
		}
		o, ok := v.(*pyjson.Object)
		if !ok {
			continue
		}
		complete := true
		for _, f := range required {
			if _, has := o.Get(f); !has {
				complete = false
			}
		}
		if complete {
			out = append(out, o)
		}
	}
	return out, nil
}

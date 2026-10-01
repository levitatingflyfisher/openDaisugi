package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daisugi-verify/internal/gateroot"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/tracejournal"
)

// loraExport is `daisugi lora export OUTPUT`: (task → envelope JSON) pairs
// from the journal's successful traces as JSONL (lora.dataset.emit_jsonl).
func (e *Env) loraExport(args []string) error {
	const cmd = "lora export"
	opts := []opt{
		{names: []string{"--data-dir"}, value: true, metavar: "PATH"},
		{names: []string{"--format"}, value: true, metavar: "TEXT",
			help: "Output format: alpaca (instruction/input/output) or chat (messages)."},
		{names: []string{"--days"}, value: true, metavar: "INTEGER", help: "Only include traces from the last N days."},
		{names: []string{"--min-task-chars"}, value: true, metavar: "INTEGER",
			help: "Skip traces with tasks shorter than this many characters."},
		{names: []string{"--system-prompt"}, value: true, metavar: "TEXT",
			help: "System prompt injected into chat-format examples."},
	}
	p, err := parseArgs(args, opts, 1)
	if err != nil {
		return e.usageArgs(cmd, "OUTPUT", err)
	}
	if p.help {
		return e.cmdHelp(cmd, " OUTPUT", "Emit (task → envelope JSON) pairs from the journal as JSONL for fine-tuning.", opts)
	}
	if len(p.args) < 1 {
		return e.usageArgs(cmd, "OUTPUT", &usageError{"Missing argument 'output'."})
	}
	var days *int64
	if p.has("--days") {
		n, err := clickInt(p, "--days", 0)
		if err != nil {
			return e.usageArgs(cmd, "OUTPUT", err)
		}
		days = &n
	}
	minChars, err := clickInt(p, "--min-task-chars", 10)
	if err != nil {
		return e.usageArgs(cmd, "OUTPUT", err)
	}
	format := p.str("--format", "alpaca")
	if format != "alpaca" && format != "chat" {
		e.errf("Unknown format %s; expected 'alpaca' or 'chat'.\n", pystr.Repr(format))
		return exit(2)
	}
	var since *float64
	if days != nil {
		s := float64(time.Now().UnixNano())/1e9 - float64(*days)*86400
		since = &s
	}
	j, err := e.openJournal(cmd, e.dataDir(p))
	if err != nil {
		return err
	}
	defer j.Close()
	out := gateroot.PathStr(p.args[0])
	// emit_jsonl makes the parent first, then lists the traces.
	if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
		return e.failPy(cmd, err)
	}
	rows, err := j.ListSuccessful(since)
	if err != nil {
		if errors.Is(err, tracejournal.ErrUnreadable) {
			return e.refuse(cmd, err)
		}
		return e.failPy(cmd, err)
	}
	var b strings.Builder
	written, skippedEmpty, skippedLoad := 0, 0, 0
	for _, row := range rows {
		rec, err := j.LoadTrace(row.TraceID)
		if err != nil {
			var le *tracejournal.LoadError
			if errors.As(err, &le) {
				skippedLoad++
				continue
			}
			if errors.Is(err, tracejournal.ErrUnreadable) {
				return e.refuse(cmd, err)
			}
			return e.failPy(cmd, err)
		}
		task := ""
		switch t := rec.Task.(type) {
		case string:
			task = pystr.Strip(t)
		case nil:
		default:
			return e.refuse(cmd, errString("a trace's task is not text"))
		}
		if int64(pystr.Len(task)) < minChars {
			skippedEmpty++
			continue
		}
		envJSON := pathways.DumpJSON(rec.Envelope)
		var payload *pyjson.Object
		if format == "alpaca" {
			payload = pyjson.NewObject().Set("instruction", task).Set("input", "").Set("output", envJSON)
		} else {
			var msgs []any
			if sp := p.str("--system-prompt", ""); sp != "" {
				msgs = append(msgs, pyjson.NewObject().Set("role", "system").Set("content", sp))
			}
			msgs = append(msgs, pyjson.NewObject().Set("role", "user").Set("content", task),
				pyjson.NewObject().Set("role", "assistant").Set("content", envJSON))
			payload = pyjson.NewObject().Set("messages", msgs)
		}
		b.WriteString(pyjson.Dumps(payload, true) + "\n")
		written++
	}
	if _, err := gateroot.WriteFile(out, b.String()); err != nil {
		return e.failPy(cmd, err)
	}
	stats := pyjson.NewObject().Set("total", len(rows)).Set("written", written).
		Set("skipped_empty_task", skippedEmpty).Set("skipped_load_error", skippedLoad).
		Set("output_path", out).Set("format", format)
	e.out("%s\n", pyjson.DumpsIndent(stats, 2, true))
	return nil
}

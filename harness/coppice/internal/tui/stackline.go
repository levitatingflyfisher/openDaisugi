package tui

import (
	"fmt"
	"strings"

	"github.com/opendaisugi/coppice/internal/textwidth"
)

// stackText is the one line a window shows under its header: what the
// agent runs on, its last gate verdict, and its tokens. It reads the
// stack, gate and tokens objects of one pane.list row. A field the server
// does not send is left out, never guessed, and a row with none of the
// three gives "".
func stackText(p map[string]any) string {
	var parts []string
	if st, ok := p["stack"].(map[string]any); ok {
		for _, k := range []string{"loop", "model", "router"} {
			if v := str(st, k); v != "" {
				parts = append(parts, v)
			}
		}
		if d, ok := st["daisugi"].(map[string]any); ok {
			if mode := str(d, "mode"); mode != "" {
				text := "daisugi " + mode
				if armed, isBool := d["armed"].(bool); isBool && !armed {
					text += " disarmed"
				}
				parts = append(parts, text)
			}
		}
	}
	if g, ok := p["gate"].(map[string]any); ok {
		if dec := str(g, "decision"); dec != "" {
			text := "gate " + dec
			if tool := str(g, "tool"); tool != "" {
				text += " " + tool
			}
			parts = append(parts, text)
		}
	}
	if t, ok := p["tokens"].(map[string]any); ok {
		var toks []string
		for _, f := range []struct{ key, word string }{
			{"fresh", "fresh"}, {"cache_read", "cache read"}, {"cache_write", "cache write"}, {"out", "out"},
		} {
			if n, ok := num(t, f.key); ok {
				toks = append(toks, countText(n)+" "+f.word)
			}
		}
		if len(toks) > 0 {
			parts = append(parts, "tokens "+strings.Join(toks, " "))
		}
	}
	for i := range parts {
		parts[i] = textwidth.Printable(parts[i], 0)
	}
	return strings.Join(parts, " · ")
}

// countText is a token count in few cells: 950, 12k, 1.2M.
func countText(n float64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", int(n))
	case n < 1e6:
		return fmt.Sprintf("%dk", int(n/1000))
	}
	return fmt.Sprintf("%.1fM", n/1e6)
}

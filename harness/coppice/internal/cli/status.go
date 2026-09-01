package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// formatNumber prints a value the JSON decoder handed back as float64. A
// whole number prints as the integer it is, so a seven-digit pid reads as
// 1234567 and never as 1.234567e+06. A value with a fraction keeps it, and
// anything that is not a float64 prints as %v would.
func formatNumber(v any) string {
	f, ok := v.(float64)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// statusText renders server.status's reply for a person. It is a pure
// function of the reply map, so its shape can be tested without a live
// server.
func statusText(res map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid %s\n", formatNumber(res["pid"]))
	fmt.Fprintf(&b, "socket %v\n", res["socket"])
	if dd, ok := res["data_dir"]; ok {
		fmt.Fprintf(&b, "data dir %v\n", dd)
	}
	fmt.Fprintf(&b, "panes %s, %s live\n", formatNumber(res["panes"]), formatNumber(res["panes_live"]))
	if r, ok := res["resumable"]; ok {
		fmt.Fprintf(&b, "%s resumable\n", formatNumber(r))
	}
	fmt.Fprintf(&b, "up %ss\n", formatNumber(res["uptime_s"]))
	fmt.Fprintf(&b, "%v\n", res["restart_note"])

	if restore, ok := res["restore"].(map[string]any); ok {
		fmt.Fprintf(&b, "restore: %s panes, %s already closed, %s resumed, %s marked done, %s marked unknown\n",
			formatNumber(restore["panes"]), formatNumber(restore["already_closed"]),
			formatNumber(restore["resumed"]), formatNumber(restore["marked_done"]),
			formatNumber(restore["marked_unknown"]))
		if notes, ok := restore["notes"].([]any); ok {
			for _, n := range notes {
				fmt.Fprintf(&b, "  %v\n", n)
			}
		}
	}

	if ws, ok := res["detection_warnings"].([]any); ok && len(ws) > 0 {
		fmt.Fprintln(&b, "\nagent detection warnings:")
		for _, w := range ws {
			fmt.Fprintf(&b, "  %v\n", w)
		}
	}
	return b.String()
}

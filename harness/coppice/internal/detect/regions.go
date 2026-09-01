package detect

import "strings"

// Input is what the evaluator sees: the pane's screen text unwrapped, the
// terminal title, and the last OSC progress payload.
type Input struct {
	Screen      string
	OSCTitle    string
	OSCProgress string
}

// Region slices the input the way a rule asked for. Every case here is a
// direct port of Herdr's region(); see README.md for the list.
func Region(in Input, spec string) string {
	s := strings.TrimSpace(spec)
	switch s {
	case "osc_title":
		return in.OSCTitle
	case "osc_progress":
		return in.OSCProgress
	}
	c := in.Screen
	switch s {
	case "whole_recent":
		return c
	case "after_last_prompt_marker":
		return afterLastPromptMarker(c)
	case "before_current_prompt_marker":
		return beforeCurrentPromptMarker(c)
	case "whole_recent_without_current_prompt_marker":
		if _, ok := currentPromptIndex(lines(c)); ok {
			return ""
		}
		return c
	case "current_prompt_block_marker":
		return currentPromptBlockMarker(c)
	case "after_current_prompt_block_marker":
		return afterCurrentPromptBlockMarker(c)
	case "prompt_box_body":
		return promptBoxBody(c)
	case "above_prompt_box":
		return abovePromptBox(c)
	case "last_non_empty_above_prompt_box":
		return lastNonEmptyLine(abovePromptBox(c))
	case "after_last_horizontal_rule":
		return afterLastHorizontalRule(c)
	}
	if n, ok := regionCount(s, "bottom_lines"); ok {
		return bottomLines(c, n)
	}
	if n, ok := regionCount(s, "bottom_non_empty_lines"); ok {
		return bottomNonEmpty(c, n)
	}
	if n, ok := topRegionCount(s); ok {
		return topNonEmpty(c, n)
	}
	return ""
}

// lines splits on the screen's line terminators. Only the single final
// terminator is stripped (TrimSuffix, not TrimRight): Herdr keeps every
// blank line the screen actually has, including trailing ones, and drops
// only the one terminator that ends the text. TrimRight would eat a whole
// run of trailing newlines, silently discarding real blank rows below the
// last output -- a common shape in a captured terminal.
func lines(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }

func joinFrom(ls []string, i int) string {
	if i >= len(ls) {
		return ""
	}
	return strings.Join(ls[i:], "\n")
}

func bottomLines(c string, n int) string {
	ls := lines(c)
	start := len(ls) - n
	if start < 0 {
		start = 0
	}
	return joinFrom(ls, start)
}

func bottomNonEmpty(c string, n int) string {
	ls := lines(c)
	seen, start := 0, -1
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			seen++
			start = i
			if seen == n {
				break
			}
		}
	}
	if start < 0 {
		return ""
	}
	return joinFrom(ls, start)
}

func topNonEmpty(c string, n int) string {
	ls := lines(c)
	seen, end := 0, -1
	for i := range ls {
		if strings.TrimSpace(ls[i]) != "" {
			seen++
			end = i
			if seen == n {
				break
			}
		}
	}
	if end < 0 {
		return ""
	}
	return strings.Join(ls[:end+1], "\n")
}

// codexPromptLine and codexBlockMarkerLine are Herdr's markers for Codex's UI.
func codexPromptLine(l string) bool {
	return l == "›" || strings.HasPrefix(l, "› ")
}

func codexBlockMarkerLine(l string) bool {
	return strings.HasPrefix(l, "•") || strings.HasPrefix(l, "■") ||
		strings.HasPrefix(l, "✗") || strings.HasPrefix(l, "✓")
}

func lastIndexFunc(ls []string, f func(string) bool) (int, bool) {
	for i := len(ls) - 1; i >= 0; i-- {
		if f(ls[i]) {
			return i, true
		}
	}
	return 0, false
}

// currentPromptIndex is the last prompt marker, but only when no block marker
// follows it: a block marker after the prompt means the agent started working
// again.
func currentPromptIndex(ls []string) (int, bool) {
	i, ok := lastIndexFunc(ls, codexPromptLine)
	if !ok {
		return 0, false
	}
	for _, l := range ls[i+1:] {
		if codexBlockMarkerLine(l) {
			return 0, false
		}
	}
	return i, true
}

func afterLastPromptMarker(c string) string {
	ls := lines(c)
	i, ok := lastIndexFunc(ls, codexPromptLine)
	if !ok {
		return c
	}
	return joinFrom(ls, i+1)
}

func beforeCurrentPromptMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return c
	}
	return strings.Join(ls[:i], "\n")
}

func currentPromptBlockMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return ""
	}
	j, ok := lastIndexFunc(ls[:i], codexBlockMarkerLine)
	if !ok {
		return ""
	}
	return ls[j]
}

func afterCurrentPromptBlockMarker(c string) string {
	ls := lines(c)
	i, ok := currentPromptIndex(ls)
	if !ok {
		return ""
	}
	j, ok := lastIndexFunc(ls[:i], codexBlockMarkerLine)
	if !ok {
		return ""
	}
	return joinFrom(ls, j)
}

// isHorizontalRule matches a line of box-drawing dashes, optionally followed by
// a label when the run is at least three characters long.
func isHorizontalRule(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" {
		return false
	}
	n := 0
	for _, r := range t {
		if r != '─' {
			break
		}
		n++
	}
	if n == 0 {
		return false
	}
	rest := strings.TrimLeft(string([]rune(t)[n:]), " \t")
	return rest == "" || n >= 3
}

// promptBoxTopIndex is the second horizontal rule counting up from the bottom,
// which is the top border of the box the cursor sits in.
func promptBoxTopIndex(ls []string) (int, bool) {
	count := 0
	for i := len(ls) - 1; i >= 0; i-- {
		if isHorizontalRule(ls[i]) {
			count++
			if count == 2 {
				return i, true
			}
		}
	}
	return 0, false
}

func promptBoxBody(c string) string {
	ls := lines(c)
	top, ok := promptBoxTopIndex(ls)
	if !ok {
		return ""
	}
	end := len(ls)
	for i := top + 1; i < len(ls); i++ {
		if isHorizontalRule(ls[i]) {
			end = i
			break
		}
	}
	if top+1 >= end {
		return ""
	}
	return strings.Join(ls[top+1:end], "\n")
}

func abovePromptBox(c string) string {
	ls := lines(c)
	top, ok := promptBoxTopIndex(ls)
	if !ok {
		return c
	}
	return strings.Join(ls[:top], "\n")
}

func afterLastHorizontalRule(c string) string {
	ls := lines(c)
	last := -1
	for i, l := range ls {
		if isHorizontalRule(l) {
			last = i
		}
	}
	return joinFrom(ls, last+1)
}

func lastNonEmptyLine(c string) string {
	ls := lines(c)
	for i := len(ls) - 1; i >= 0; i-- {
		if strings.TrimSpace(ls[i]) != "" {
			return ls[i]
		}
	}
	return ""
}

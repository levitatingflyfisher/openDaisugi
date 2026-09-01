package detect

import "testing"

const box = "line one\n" +
	"line two\n" +
	"────────────────────\n" +
	"❯ type here\n" +
	"────────────────────\n" +
	"  ? for shortcuts\n"

func TestBottomNonEmptyLinesTakesTheBottomOccurrence(t *testing.T) {
	in := Input{Screen: "a\n\nb\n\nc\n"}
	if got := Region(in, "bottom_non_empty_lines(2)"); got != "b\n\nc" {
		t.Fatalf("got %q, want %q", got, "b\n\nc")
	}
}

func TestBottomLinesCountsBlankLinesToo(t *testing.T) {
	in := Input{Screen: "a\nb\nc\n"}
	if got := Region(in, "bottom_lines(2)"); got != "b\nc" {
		t.Fatalf("got %q, want %q", got, "b\nc")
	}
}

func TestTopNonEmptyLinesTakesTheTopOccurrence(t *testing.T) {
	in := Input{Screen: "\na\nb\nc\n"}
	if got := Region(in, "top_non_empty_lines(2)"); got != "\na\nb" {
		t.Fatalf("got %q, want %q", got, "\na\nb")
	}
}

func TestPromptBoxBodyIsBetweenTheLastTwoRules(t *testing.T) {
	if got := Region(Input{Screen: box}, "prompt_box_body"); got != "❯ type here" {
		t.Fatalf("got %q, want %q", got, "❯ type here")
	}
}

func TestAbovePromptBoxStopsAtTheBoxTop(t *testing.T) {
	if got := Region(Input{Screen: box}, "above_prompt_box"); got != "line one\nline two" {
		t.Fatalf("got %q, want %q", got, "line one\nline two")
	}
}

func TestLastNonEmptyAbovePromptBox(t *testing.T) {
	if got := Region(Input{Screen: box}, "last_non_empty_above_prompt_box"); got != "line two" {
		t.Fatalf("got %q, want %q", got, "line two")
	}
}

func TestAfterLastHorizontalRule(t *testing.T) {
	if got := Region(Input{Screen: box}, "after_last_horizontal_rule"); got != "  ? for shortcuts" {
		t.Fatalf("got %q, want %q", got, "  ? for shortcuts")
	}
}

func TestCodexPromptMarkerRegions(t *testing.T) {
	screen := "• ran a tool\nsome output\n› \n"
	if got := Region(Input{Screen: screen}, "after_last_prompt_marker"); got != "" {
		t.Fatalf("after_last_prompt_marker = %q, want empty", got)
	}
	if got := Region(Input{Screen: screen}, "before_current_prompt_marker"); got != "• ran a tool\nsome output" {
		t.Fatalf("before_current_prompt_marker = %q", got)
	}
	if got := Region(Input{Screen: screen}, "whole_recent_without_current_prompt_marker"); got != "" {
		t.Fatalf("whole_recent_without_current_prompt_marker = %q, want empty when a prompt is current", got)
	}
	if got := Region(Input{Screen: screen}, "current_prompt_block_marker"); got != "• ran a tool" {
		t.Fatalf("current_prompt_block_marker = %q", got)
	}
	if got := Region(Input{Screen: screen}, "after_current_prompt_block_marker"); got != "• ran a tool\nsome output\n› " {
		t.Fatalf("after_current_prompt_block_marker = %q", got)
	}
}

func TestOSCRegionsComeFromTheirOwnFields(t *testing.T) {
	in := Input{Screen: "ignored", OSCTitle: "✳ idle", OSCProgress: "4;0"}
	if got := Region(in, "osc_title"); got != "✳ idle" {
		t.Fatalf("osc_title = %q", got)
	}
	if got := Region(in, "osc_progress"); got != "4;0" {
		t.Fatalf("osc_progress = %q", got)
	}
}

// Herdr keeps every blank line at the bottom of the screen except the single
// final line terminator; lines() used to strings.TrimRight all of them away,
// which silently ate real content (a captured terminal often has several
// blank rows below the last output). bottom_lines(2) on a screen with two
// real trailing blank lines must return those two blank lines, not "".
func TestBottomLinesKeepsTrailingBlankLinesLikeHerdr(t *testing.T) {
	in := Input{Screen: "a\nb\nc\n\n\n"}
	if got := Region(in, "bottom_lines(2)"); got != "\n" {
		t.Fatalf("got %q, want %q (the two blank lines, not collapsed away)", got, "\n")
	}
}

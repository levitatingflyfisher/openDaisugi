package textwidth

import "testing"

func TestIsWideKnowsCJKAndNotASCII(t *testing.T) {
	if !IsWide("字") || !IsWide("ア") {
		t.Fatal("CJK glyphs are wide")
	}
	if IsWide("a") || IsWide("") || IsWide("ab") || IsWide("䷀") {
		t.Fatal("ASCII, empty, two runes, and a hexagram symbol are not wide")
	}
}

func TestWidthCountsCells(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"日本語", 6},
		{"a日b", 4},
		{"a\x1bb", 2},
		{"a\x7f", 1},
		{"é", 1},
	}
	for _, c := range cases {
		if got := Width(c.in); got != c.want {
			t.Fatalf("Width(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTruncateCutsToCellsAndMarksTheCut(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"日本語", 4, "日…"},
		{"日本語", 5, "日本…"},
		{"日本語", 6, "日本語"},
		{"hello", 1, "h"},
		{"日本", 1, ""},
		{"hello", 0, ""},
		{"hello", -3, ""},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.max); got != c.want {
			t.Fatalf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if w := Width(Truncate(c.in, c.max)); c.max > 0 && w > c.max {
			t.Fatalf("Truncate(%q, %d) is %d cells wide", c.in, c.max, w)
		}
	}
}

func TestPadMakesExactlyWCells(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"ab", 4, "ab  "},
		{"ab", 2, "ab"},
		{"abcdef", 4, "abc…"},
		{"日本", 5, "日本 "},
		{"日本語", 5, "日本…"},
		{"", 3, "   "},
		{"ab", 0, ""},
	}
	for _, c := range cases {
		got := Pad(c.in, c.w)
		if got != c.want {
			t.Fatalf("Pad(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
		if w := Width(got); w != c.w && c.w >= 0 {
			t.Fatalf("Pad(%q, %d) is %d cells wide", c.in, c.w, w)
		}
	}
}

func TestPrintableDropsControlRunesAndCuts(t *testing.T) {
	if got := Printable("a\x1bb\x07c\x9bd\x7fe\tf\ng\u009bh", 0); got != "abcdefgh" {
		t.Fatalf("got %q", got)
	}
	if got := Printable("héllo wörld", 5); got != "héllo" {
		t.Fatalf("got %q", got)
	}
}

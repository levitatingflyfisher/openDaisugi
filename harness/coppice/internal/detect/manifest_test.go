package detect

import (
	"regexp"
	"strings"
	"testing"
)

func mustFail(t *testing.T, body, wantSubstr string) {
	t.Helper()
	_, _, err := Parse("test.toml", []byte(body))
	if err == nil {
		t.Fatalf("Parse accepted this manifest, want an error mentioning %q:\n%s", wantSubstr, body)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("Parse error = %q, want it to mention %q", err, wantSubstr)
	}
}

func TestParseReadsTheTopLevelFields(t *testing.T) {
	m, _, err := Parse("claude.toml", []byte(`
id = "claude"
version = "2026.09.04.1"
min_engine_version = 2
aliases = ["claude-code"]

[[rules]]
id = "prompt"
state = "idle"
priority = 950
region = "prompt_box_body"
line_regex = ['^\s*❯']
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "claude" || m.Version != "2026.09.04.1" || m.MinEngineVersion != 2 {
		t.Fatalf("manifest = %+v, want the claude header", m)
	}
	if len(m.Aliases) != 1 || m.Aliases[0] != "claude-code" {
		t.Fatalf("aliases = %v, want [claude-code]", m.Aliases)
	}
	if len(m.Rules) != 1 || m.Rules[0].ID != "prompt" || *m.Rules[0].State != "idle" {
		t.Fatalf("rules = %+v, want one idle prompt rule", m.Rules)
	}
}

func TestRegionDefaultsToWholeRecent(t *testing.T) {
	m, _, err := Parse("x.toml", []byte("id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Rules[0].Region != "whole_recent" {
		t.Fatalf("region = %q, want whole_recent", m.Rules[0].Region)
	}
}

func TestUnknownTopLevelKeyIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\nmystery=1\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n", "mystery")
}

func TestUnknownRuleKeyIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nvibes=true\n", "vibes")
}

func TestUnknownRegionIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nregion=\"the_vibe_zone\"\ncontains=[\"a\"]\n",
		"the_vibe_zone")
}

func TestTopNonEmptyLinesNeedsEngineThree(t *testing.T) {
	mustFail(t, "id=\"x\"\nmin_engine_version=2\n[[rules]]\nid=\"r\"\n"+
		"region=\"top_non_empty_lines(3)\"\ncontains=[\"a\"]\n", "min_engine_version")
}

func TestManifestAskingForAFutureEngineIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\nmin_engine_version=99\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n",
		"engine")
}

func TestARuleWithNoPositiveMatcherIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nnot=[{contains=[\"a\"]}]\n", "positive matcher")
}

func TestAnEmptyRuleIDIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"\"\ncontains=[\"a\"]\n", "id")
}

func TestSkipStateUpdateRequiresStateUnknown(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"idle\"\nskip_state_update=true\n"+
		"contains=[\"a\"]\n", "skip_state_update")
}

func TestSkipStateUpdateForbidsVisibleEvidence(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"unknown\"\nskip_state_update=true\n"+
		"visible_idle=true\ncontains=[\"a\"]\n", "visible")
}

func TestABadRegexIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nregex=['(unclosed']\n", "regex")
}

func TestGateDepthOverEightIsALoadError(t *testing.T) {
	body := "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nall=["
	open, close := "", ""
	for i := 0; i < 10; i++ {
		open += "{all=["
		close += "]}"
	}
	body += open + "{contains=[\"a\"]}" + close + "]\n"
	mustFail(t, body, "depth")
}

func TestTooManyRulesIsALoadError(t *testing.T) {
	var b strings.Builder
	b.WriteString("id=\"x\"\n")
	for i := 0; i < 129; i++ {
		b.WriteString("[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n")
	}
	mustFail(t, b.String(), "128")
}

func TestAMatcherOverFiveHundredAndTwelveCharsIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\""+strings.Repeat("a", 513)+"\"]\n",
		"512")
}

func TestValidRegionAcceptsTheDocumentedNames(t *testing.T) {
	for _, r := range []string{
		"whole_recent", "after_last_prompt_marker", "before_current_prompt_marker",
		"whole_recent_without_current_prompt_marker", "current_prompt_block_marker",
		"after_current_prompt_block_marker", "prompt_box_body", "above_prompt_box",
		"last_non_empty_above_prompt_box", "after_last_horizontal_rule",
		"osc_title", "osc_progress",
		"bottom_lines(5)", "bottom_non_empty_lines(12)", "top_non_empty_lines(3)",
	} {
		if !ValidRegion(r) {
			t.Fatalf("ValidRegion(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "bottom_lines()", "top_non_empty_lines(0)",
		"top_non_empty_lines(007)", "bottom_lines(x)", "nonsense"} {
		if ValidRegion(r) {
			t.Fatalf("ValidRegion(%q) = true, want false", r)
		}
	}
}

// This branch existed but had no test covering it.
func TestAnUnknownStateIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nstate=\"done\"\ncontains=[\"a\"]\n", "done")
}

// regionCount must accept ASCII digits only, exactly like
// topRegionCount does. Herdr's usize cannot be negative; strconv.Atoi alone would accept
// a leading "-" and silently produce a negative count.
func TestBottomLinesRejectsANegativeCount(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"r\"\nregion=\"bottom_lines(-3)\"\ncontains=[\"a\"]\n",
		"bottom_lines(-3)")
}

func TestRegionCountRejectsNonDigitCharacters(t *testing.T) {
	for _, r := range []string{
		"bottom_lines(-3)", "bottom_non_empty_lines(-1)",
		"bottom_lines(+3)", "bottom_lines(3.5)", "bottom_lines( 3)",
	} {
		if ValidRegion(r) {
			t.Fatalf("ValidRegion(%q) = true, want false", r)
		}
	}
	// bottom_lines(0) has no leading-zero restriction, unlike top_non_empty_lines: zero
	// bottom lines is a legitimate (if useless) region, and no vendored manifest cares
	// either way, so nothing forbids it.
	if !ValidRegion("bottom_lines(0)") {
		t.Fatal("ValidRegion(\"bottom_lines(0)\") = false, want true")
	}
}

// The recursive compileGate calls used to rebind the context
// to a bare "all gate"/"any gate"/"not gate", losing the rule id. A matcher-cap error two
// gates deep must still name the rule it came from.
func TestNestedGateMatcherCapErrorNamesTheRule(t *testing.T) {
	matchers := strings.TrimSuffix(strings.Repeat(`"a",`, 33), ",")
	body := "id=\"x\"\n[[rules]]\nid=\"deep_rule\"\ncontains=[\"top\"]\n" +
		"all=[{all=[{contains=[" + matchers + "]}]}]\n"
	mustFail(t, body, "deep_rule")
}

// Exactly at the cap is accepted, not just just-over-the-cap.
func TestExactlyOneHundredTwentyEightRulesIsAccepted(t *testing.T) {
	var b strings.Builder
	b.WriteString("id=\"x\"\n")
	for i := 0; i < 128; i++ {
		b.WriteString("[[rules]]\nid=\"r\"\ncontains=[\"a\"]\n")
	}
	if _, _, err := Parse("test.toml", []byte(b.String())); err != nil {
		t.Fatalf("exactly 128 rules should be accepted, got: %v", err)
	}
}

func TestGateDepthExactlyEightIsAccepted(t *testing.T) {
	body := "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nall=["
	open, close := "", ""
	for i := 0; i < 7; i++ {
		open += "{all=["
		close += "]}"
	}
	body += open + "{contains=[\"a\"]}" + close + "]\n"
	if _, _, err := Parse("test.toml", []byte(body)); err != nil {
		t.Fatalf("gate depth exactly 8 should be accepted, got: %v", err)
	}
}

func TestGateDepthNineIsRejected(t *testing.T) {
	body := "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"a\"]\nall=["
	open, close := "", ""
	for i := 0; i < 8; i++ {
		open += "{all=["
		close += "]}"
	}
	body += open + "{contains=[\"a\"]}" + close + "]\n"
	mustFail(t, body, "depth")
}

func TestAMatcherOfExactlyFiveHundredAndTwelveCharsIsAccepted(t *testing.T) {
	body := "id=\"x\"\n[[rules]]\nid=\"r\"\ncontains=[\"" + strings.Repeat("a", 512) + "\"]\n"
	if _, _, err := Parse("test.toml", []byte(body)); err != nil {
		t.Fatalf("a 512-char matcher should be accepted, got: %v", err)
	}
}

// Matches Herdr's validate_manifest (rules must be non-empty), fail-closed.
func TestEmptyRulesListIsALoadError(t *testing.T) {
	mustFail(t, "id=\"x\"\n", "at least one rule")
}

// Real vendored manifests use two Rust-regex-only escape forms Go's
// regexp does not have: \uXXXX / \u{XXXX} (Go spells this \x{XXXX}) and the
// derived Unicode property \p{Alphabetic} (Go's RE2 engine only has general
// categories and scripts, not PropList.txt derived properties). Both must be
// translated before compiling, never by hand-editing a vendored .toml. Every
// case below is a double-quoted Go string with an explicit "\\" so the
// literal text is unambiguous: this is the backslash-u TEXT a TOML
// single-quoted literal string decodes to, not a Go \u escape (which the
// compiler would turn into the rune itself before the test ever ran).
func TestTranslateRustRegexRewritesUnicodeEscapes(t *testing.T) {
	cases := map[string]string{
		"[\\u2800-\\u28FF]":     "[\\x{2800}-\\x{28FF}]",
		"\\u{fe0e}":             "\\x{fe0e}",
		"[\\u{fe0e}\\u{fe0f}]?": "[\\x{fe0e}\\x{fe0f}]?",
		"\\p{Alphabetic}":       "\\pL",
		// Already-Go-shaped patterns pass through untouched.
		"\\x{2800}": "\\x{2800}",
		"\\s+\\d":   "\\s+\\d",
		// An escaped backslash before the construct - an EVEN run - means
		// every backslash is already a literal one, not an escape
		// introducing \u or \p{...} at all, even though the letters "u" or
		// "p{" still follow. Untouched. Raw strings (backtick) below, not
		// double-quoted: the point is the exact count of literal backslash
		// characters, and counting "\\\\" pairs by eye in a double-quoted
		// literal is exactly the kind of transcription error this case
		// exists to catch, in the source as much as in the code under test.
		`\\u2800`: `\\u2800`,
		// An odd run longer than one: 3 backslashes is one literal pair
		// plus a real, live backslash that starts the construct - the same
		// rule as a single backslash, just with a literal pair ahead of it
		// that must survive untranslated in the output too.
		`\\\u{fe0e}`: `\\\x{fe0e}`,
	}
	for in, want := range cases {
		if got := translateRustRegex(in); got != want {
			t.Fatalf("translateRustRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

// The seven patterns below are copied verbatim from the vendored manifests
// that failed to compile before translateRustRegex existed: antigravity.toml,
// cursor.toml, droid.toml, hermes.toml (x3, one shown), kimi.toml, kiro.toml,
// qodercli.toml.
func TestTranslatedPatternsFromRealManifestsCompile(t *testing.T) {
	for _, p := range []string{
		"^\\s*[\\u2800-\\u28FF]+\\s+\\p{Alphabetic}+\\w*ing\\b",
		"^\\s*(⬡|⬢|[\\u2800-\\u28FF]+)\\s+\\p{Alphabetic}+\\w*ing\\b",
		"^\\s*[\\u2800-\\u28FF]",
		"^⚠[\\u{fe0e}\\u{fe0f}]?(?:\\s|$)",
		"(?i)^\\s*[\\u2800-\\u28FF]+\\s*(thinking\\.\\.\\.|working\\.\\.\\.|using )",
		"^\\s*(◔|◑|◕|●)\\s+\\p{Alphabetic}",
		"^\\s*[\\u2800-\\u28FF]\\s+.*\\p{Alphabetic}",
	} {
		if _, err := regexp.Compile(translateRustRegex(p)); err != nil {
			t.Fatalf("translateRustRegex(%q) still fails to compile: %v", p, err)
		}
	}
}

// TestTranslatedPatternsFromRealManifestsCompile only proves the translated
// patterns compile. That leaves the translation's actual semantics unchecked:
// \pL is not a byte-for-byte match for Rust's \p{Alphabetic} (see
// README.md's "Matcher semantics" section), so a compile-only test could not
// tell a correct translation from one that compiles but never matches real
// screen text. This exercises both translated forms (bare \u, braced \u{},
// and \p{Alphabetic}) against text shaped like what droid/hermes/kiro's own
// rules are meant to catch.
func TestTranslatedPatternsMatchRealText(t *testing.T) {
	cases := []struct {
		name, pattern, match, noMatch string
	}{
		{
			name:    "droid bare \\u braille range",
			pattern: "^\\s*[\\u2800-\\u28FF]",
			match:   "⠙ working",
			noMatch: "plain text, no spinner",
		},
		{
			name:    "hermes braced \\u{} variation selectors",
			pattern: "^⚠[\\u{fe0e}\\u{fe0f}]?(?:\\s|$)",
			match:   "⚠️",
			noMatch: "no warning glyph here",
		},
		{
			name:    "kiro \\p{Alphabetic} after a spinner glyph",
			pattern: "^\\s*(◔|◑|◕|●)\\s+\\p{Alphabetic}",
			match:   "◔ Thinking",
			noMatch: "◔ 123",
		},
	}
	for _, c := range cases {
		re, err := regexp.Compile(translateRustRegex(c.pattern))
		if err != nil {
			t.Fatalf("%s: translateRustRegex(%q) failed to compile: %v", c.name, c.pattern, err)
		}
		if !re.MatchString(c.match) {
			t.Fatalf("%s: %q should match %q after translation", c.name, c.pattern, c.match)
		}
		if re.MatchString(c.noMatch) {
			t.Fatalf("%s: %q should NOT match %q after translation", c.name, c.pattern, c.noMatch)
		}
	}
}

// An escaped backslash immediately before "u" must not
// be mistaken for the start of a unicode escape. "\\uABCD" (an escaped
// literal backslash, then the literal text "uABCD") is a different pattern
// from "\uABCD" (a unicode escape for U+ABCD); translateRustRegex must count
// the run of backslashes and only treat the trailing one as live when the
// count is odd. Tests both escape shapes translateRustRegex recognizes: the
// bare \uXXXX form and the braced \u{XXXX} form.
func TestTranslateRustRegexLeavesAnEscapedBackslashAlone(t *testing.T) {
	cases := map[string]string{
		// A real escape: one backslash, odd count, translates.
		"\\u2800":   "\\x{2800}",
		"\\u{2800}": "\\x{2800}",
		// An escaped backslash followed by literal text: two backslashes,
		// even count, nothing here is a unicode escape at all.
		"\\\\u2800":   "\\\\u2800",
		"\\\\u{2800}": "\\\\u{2800}",
		// Three backslashes: one escaped pair (kept literal) plus one real
		// escape (translated).
		"\\\\\\u2800": "\\\\\\x{2800}",
	}
	for in, want := range cases {
		if got := translateRustRegex(in); got != want {
			t.Fatalf("translateRustRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

// A pattern using an unsupported Rust regex-syntax construct that
// translateRustRegex does not recognize (only \u/\u{}/\p{Alphabetic} are)
// must still fail closed: a compile error naming the rule it came from, not
// a silent pass-through that never matches.
func TestUnsupportedRegexConstructIsALoadErrorNamingTheRule(t *testing.T) {
	mustFail(t, "id=\"x\"\n[[rules]]\nid=\"emoji_rule\"\nregex=['\\p{Emoji}']\n", "emoji_rule")
}

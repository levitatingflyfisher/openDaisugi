// Package detect is a Go port of Herdr's TOML agent-detection engine. The
// schema it implements is written down in README.md, read from Herdr's
// src/detect/manifest.rs. Reusing their format means their manifests and their
// users' override files work here unchanged.
package detect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// EngineVersion is what a manifest's min_engine_version is checked against.
// 3 is the version that introduced top_non_empty_lines.
const EngineVersion = 3

const (
	maxRules           = 128
	maxGateDepth       = 8
	maxTotalGates      = 512
	maxMatchersPerGate = 32
	maxTotalMatchers   = 1024
	maxMatcherChars    = 512
	topRegionMinVer    = 3
	maxTopRegionLines  = 65535
)

type State string

const (
	StateIdle    State = "idle"
	StateWorking State = "working"
	StateBlocked State = "blocked"
	StateUnknown State = "unknown"
)

type Gate struct {
	All       []Gate   `toml:"all"`
	Any       []Gate   `toml:"any"`
	Not       []Gate   `toml:"not"`
	Contains  []string `toml:"contains"`
	Regex     []string `toml:"regex"`
	LineRegex []string `toml:"line_regex"`
}

type Rule struct {
	ID              string   `toml:"id"`
	State           *State   `toml:"state"`
	Priority        int      `toml:"priority"`
	Region          string   `toml:"region"`
	VisibleIdle     bool     `toml:"visible_idle"`
	VisibleBlocker  bool     `toml:"visible_blocker"`
	VisibleWorking  bool     `toml:"visible_working"`
	SkipStateUpdate bool     `toml:"skip_state_update"`
	All             []Gate   `toml:"all"`
	Any             []Gate   `toml:"any"`
	Not             []Gate   `toml:"not"`
	Contains        []string `toml:"contains"`
	Regex           []string `toml:"regex"`
	LineRegex       []string `toml:"line_regex"`
}

func (r Rule) gate() Gate {
	return Gate{All: r.All, Any: r.Any, Not: r.Not,
		Contains: r.Contains, Regex: r.Regex, LineRegex: r.LineRegex}
}

// EffectiveState is unknown when a rule declares no state, matching Herdr.
func (r Rule) EffectiveState() State {
	if r.State == nil {
		return StateUnknown
	}
	return *r.State
}

type Manifest struct {
	ID               string   `toml:"id"`
	Version          string   `toml:"version"`
	MinEngineVersion int      `toml:"min_engine_version"`
	UpdatedAt        string   `toml:"updated_at"`
	Aliases          []string `toml:"aliases"`
	Rules            []Rule   `toml:"rules"`
}

// compiledGate holds the regexes already built, so evaluation never compiles.
type compiledGate struct {
	all       []compiledGate
	any       []compiledGate
	not       []compiledGate
	contains  []string // already lowercased
	regex     []*regexp.Regexp
	lineRegex []*regexp.Regexp
}

// Compiled is a manifest ready to evaluate.
type Compiled struct {
	Manifest *Manifest
	gates    []compiledGate // one per rule, in file order
}

// gateTotals tracks manifest-wide counters across the whole compile pass.
type gateTotals struct {
	gates    int
	matchers int
}

// Parse reads one manifest and validates it fully. Every failure is a load
// error: a manifest that half-loads would produce detections nobody can
// explain, which is worse than an agent that simply is not detected.
func Parse(name string, data []byte) (*Manifest, *Compiled, error) {
	var m Manifest
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, nil, fmt.Errorf("%s: unknown keys: %s", name, strings.Join(keys, ", "))
	}
	if strings.TrimSpace(m.ID) == "" {
		return nil, nil, fmt.Errorf("%s: id must not be empty", name)
	}
	if m.MinEngineVersion > EngineVersion {
		return nil, nil, fmt.Errorf(
			"%s: needs detection engine %d, this build is engine %d. Update coppice.",
			name, m.MinEngineVersion, EngineVersion)
	}
	if len(m.Rules) == 0 {
		return nil, nil, fmt.Errorf("%s: manifest must contain at least one rule", name)
	}
	if len(m.Rules) > maxRules {
		return nil, nil, fmt.Errorf("%s: %d rules, the cap is %d", name, len(m.Rules), maxRules)
	}
	for i := range m.Rules {
		if m.Rules[i].Region == "" {
			m.Rules[i].Region = "whole_recent"
		}
	}

	c := &Compiled{Manifest: &m}
	var totals gateTotals
	for _, r := range m.Rules {
		if strings.TrimSpace(r.ID) == "" {
			return nil, nil, fmt.Errorf("%s: a rule has an empty id", name)
		}
		if r.SkipStateUpdate {
			if r.EffectiveState() != StateUnknown {
				return nil, nil, fmt.Errorf(
					`%s: rule %s uses skip_state_update without state = "unknown"`, name, r.ID)
			}
			if r.VisibleIdle || r.VisibleBlocker || r.VisibleWorking {
				return nil, nil, fmt.Errorf(
					"%s: rule %s uses skip_state_update with visible state evidence", name, r.ID)
			}
		}
		if st := r.EffectiveState(); st != StateIdle && st != StateWorking &&
			st != StateBlocked && st != StateUnknown {
			return nil, nil, fmt.Errorf("%s: rule %s has state %q", name, r.ID, st)
		}
		if !ValidRegion(r.Region) {
			return nil, nil, fmt.Errorf("%s: rule %s uses invalid region: %s", name, r.ID, r.Region)
		}
		if strings.HasPrefix(strings.TrimSpace(r.Region), "top_non_empty_lines(") &&
			m.MinEngineVersion != 0 && m.MinEngineVersion < topRegionMinVer {
			return nil, nil, fmt.Errorf(
				"%s: rule %s uses top_non_empty_lines but min_engine_version is %d, need %d",
				name, r.ID, m.MinEngineVersion, topRegionMinVer)
		}
		cg, err := compileGate(r.gate(), "rule "+r.ID, 0, &totals)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		c.gates = append(c.gates, cg)
	}
	return &m, c, nil
}

func compileGate(g Gate, ctx string, depth int, totals *gateTotals) (compiledGate, error) {
	var out compiledGate
	if depth > maxGateDepth {
		return out, fmt.Errorf("%s exceeds max gate depth %d", ctx, maxGateDepth)
	}
	totals.gates++
	if totals.gates > maxTotalGates {
		return out, fmt.Errorf("manifest exceeds max gate count %d", maxTotalGates)
	}
	direct := len(g.Contains) + len(g.Regex) + len(g.LineRegex)
	if direct > maxMatchersPerGate {
		return out, fmt.Errorf("%s has %d direct matchers, max is %d",
			ctx, direct, maxMatchersPerGate)
	}
	totals.matchers += direct
	if totals.matchers > maxTotalMatchers {
		return out, fmt.Errorf("manifest exceeds max matcher count %d", maxTotalMatchers)
	}
	if len(g.Contains)+len(g.Regex)+len(g.LineRegex)+len(g.All)+len(g.Any) == 0 {
		return out, fmt.Errorf("%s must contain a positive matcher", ctx)
	}
	for _, s := range g.Contains {
		if len([]rune(s)) > maxMatcherChars {
			return out, fmt.Errorf("%s matcher exceeds max length %d", ctx, maxMatcherChars)
		}
		out.contains = append(out.contains, strings.ToLower(s))
	}
	compileAll := func(pats []string, kind string) ([]*regexp.Regexp, error) {
		var res []*regexp.Regexp
		for _, p := range pats {
			if len([]rune(p)) > maxMatcherChars {
				return nil, fmt.Errorf("%s matcher exceeds max length %d", ctx, maxMatcherChars)
			}
			re, err := regexp.Compile(translateRustRegex(p))
			if err != nil {
				return nil, fmt.Errorf("%s has an invalid %s %q: %w", ctx, kind, p, err)
			}
			res = append(res, re)
		}
		return res, nil
	}
	var err error
	if out.regex, err = compileAll(g.Regex, "regex"); err != nil {
		return out, err
	}
	if out.lineRegex, err = compileAll(g.LineRegex, "line_regex"); err != nil {
		return out, err
	}
	// Nested calls extend ctx rather than replacing it, so a load error at any
	// depth still names the rule it came from.
	for _, n := range g.All {
		c, err := compileGate(n, ctx+" > all", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.all = append(out.all, c)
	}
	for _, n := range g.Any {
		c, err := compileGate(n, ctx+" > any", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.any = append(out.any, c)
	}
	for _, n := range g.Not {
		c, err := compileGate(n, ctx+" > not", depth+1, totals)
		if err != nil {
			return out, err
		}
		out.not = append(out.not, c)
	}
	return out, nil
}

var fixedRegions = map[string]bool{
	"whole_recent": true, "after_last_prompt_marker": true,
	"before_current_prompt_marker": true, "whole_recent_without_current_prompt_marker": true,
	"current_prompt_block_marker": true, "after_current_prompt_block_marker": true,
	"prompt_box_body": true, "above_prompt_box": true,
	"last_non_empty_above_prompt_box": true, "after_last_horizontal_rule": true,
	"osc_title": true, "osc_progress": true,
}

// ValidRegion mirrors Herdr's validate_region_name. An unknown name is refused
// at load rather than silently returning an empty region, because a rule that
// can never match is a bug nobody would notice.
func ValidRegion(spec string) bool {
	s := strings.TrimSpace(spec)
	if fixedRegions[s] {
		return true
	}
	if _, ok := regionCount(s, "bottom_lines"); ok {
		return true
	}
	if _, ok := regionCount(s, "bottom_non_empty_lines"); ok {
		return true
	}
	_, ok := topRegionCount(s)
	return ok
}

// regionCount accepts ASCII digits only, like topRegionCount does: Herdr's
// usize cannot be negative, so a leading "-" (which strconv.Atoi alone would
// happily accept) must be refused, not silently parsed into a negative count.
// Unlike topRegionCount, a leading zero is not refused: bottom_lines(0) is a
// legitimate, if useless, region.
func regionCount(spec, name string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, name)
	if !ok || !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return 0, false
	}
	digits := rest[1 : len(rest)-1]
	if digits == "" {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// topRegionCount is stricter than the others, exactly as Herdr is: a positive
// decimal, no leading zero, at most 65535.
func topRegionCount(spec string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, "top_non_empty_lines")
	if !ok || !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return 0, false
	}
	digits := rest[1 : len(rest)-1]
	if digits == "" || strings.HasPrefix(digits, "0") {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > maxTopRegionLines {
		return 0, false
	}
	return n, true
}

// rustOnlyConstruct matches a run of one or more backslashes immediately
// followed by one of the three Rust-only constructs translateRustRegex knows
// how to port: the braced unicode escape \u{XXXX}, the bare unicode escape
// \uXXXX (exactly 4 hex digits), or the derived Unicode property
// \p{Alphabetic}. The backslash run is captured whole so the replacement
// function can tell a real (unescaped) escape from an escaped backslash
// followed by literal text that merely looks like one.
var rustOnlyConstruct = regexp.MustCompile(
	`(\\+)(u\{[0-9a-fA-F]+\}|u[0-9a-fA-F]{4}|p\{Alphabetic\})`)

// translateRustRegex rewrites the handful of Rust regex-syntax constructs the
// vendored manifests actually use that Go's RE2-based regexp engine does not
// accept outright. This runs on every regex and line_regex pattern, bundled
// or override, so a Herdr user's own override files keep working exactly as
// the README promises they do, without anyone hand-editing a vendored .toml
// (NOTICE requires those stay unchanged apart from the attribution line).
//
//   - \uXXXX and \u{XXXX} (Rust) -> \x{XXXX} (Go). Confirmed against seven of
//     the 21 vendored manifests (antigravity, cursor, droid, hermes, kimi,
//     kiro, qodercli): all use braille-spinner ranges like [⠀-⣿] or
//     variation-selector alternatives like [\u{fe0e}\u{fe0f}].
//   - \p{Alphabetic} (a Rust regex-syntax derived Unicode property, from
//     PropList.txt) -> \pL (Go's Letter general category). Go's regexp only
//     understands Unicode general categories and script names, not derived
//     properties; \pL is the closest general-category equivalent and is what
//     every observed use of \p{Alphabetic} in these manifests actually needs
//     (matching the first letter of an English word like "Thinking" after a
//     spinner glyph). Not a general Unicode-property translator: if a future
//     re-vendor introduces a different unsupported property, Parse fails
//     closed with a clear regex-compile error rather than guessing at one.
//
// Every construct above starts with a literal backslash, so an ESCAPED
// backslash immediately in front of one must not be mistaken for the start
// of it. Two backslashes then "u2800" is not a unicode escape at all: the
// first backslash escapes the second into one literal backslash character,
// and "u2800" that follows is four ordinary letters and digits, not a code
// point. Only a single, live (unescaped) backslash right before one of these
// constructs makes it real. A run of N backslashes followed by one of these
// constructs therefore translates only when N is odd -- an even run is N/2
// escaped-literal backslash pairs and the construct after them is ordinary
// text, left untouched; an odd run is (N-1)/2 literal pairs plus one real,
// live backslash that starts the construct being ported.
func translateRustRegex(pattern string) string {
	return rustOnlyConstruct.ReplaceAllStringFunc(pattern, func(m string) string {
		sub := rustOnlyConstruct.FindStringSubmatch(m)
		backslashes, construct := sub[1], sub[2]
		if len(backslashes)%2 == 0 {
			return m // every backslash is an escaped literal one; untouched.
		}
		literal := backslashes[:len(backslashes)-1]
		switch {
		case strings.HasPrefix(construct, "u{"):
			return literal + `\x{` + construct[2:len(construct)-1] + `}`
		case strings.HasPrefix(construct, "u"):
			return literal + `\x{` + construct[1:] + `}`
		default: // p{Alphabetic}
			return literal + `\pL`
		}
	})
}

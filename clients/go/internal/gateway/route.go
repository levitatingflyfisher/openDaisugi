package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is opendaisugi.routing's difficulty heuristic and
// opendaisugi.gateway's route_turn with the text helpers it reads.

// DefaultCheapModel and DefaultFrontierModel are routing's defaults.
const (
	DefaultCheapModel    = "claude-haiku-4-5"
	DefaultFrontierModel = "claude-opus-4-8"
	hardThreshold        = 0.5
	// StickyPrefixThreshold is STICKY_PREFIX_THRESHOLD_TOKENS (ADR-0015).
	StickyPrefixThreshold = 4096
)

var hardSignals = []string{
	"architect", "architecture", "design", "refactor", "migrat", "concurren", "deadlock",
	"race condition", "distributed", "consensus", "optimi", "security", "vulnerab", "schema",
	"algorithm", "prove", "proof", "debug", "root cause", "thread-saf", "scal",
}

// EstimateDifficulty is routing.estimate_difficulty: a 0..1 score from
// the task's length and its hard-signal words.
func EstimateDifficulty(task string) float64 {
	t := pystr.Lower(task)
	length := pyMin(float64(pystr.Len(t))/400.0, 0.5)
	hits := 0
	for _, s := range hardSignals {
		if strings.Contains(t, s) {
			hits++
		}
	}
	signal := pyMin(float64(hits)*0.25, 0.6)
	return pyMin(length+signal, 1.0)
}

// pyMin is min(a, b): a unless b is smaller.
func pyMin(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

// Decision is RouteDecision. Model and RequestedModel hold whatever JSON
// value the body named, since the oracle never checks its type.
type Decision struct {
	Tier           string
	Model          any
	RequestedModel any
	Difficulty     float64
	Downgraded     bool
	Reason         string
}

// messageText is _message_text: a str content, or the text of its text
// blocks joined with spaces.
func messageText(content any) (string, error) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var parts []any
		for _, b := range c {
			o, ok := b.(*pyjson.Object)
			if !ok || !eqStr(getOr(o, "type", nil), "text") {
				continue
			}
			parts = append(parts, getOr(o, "text", ""))
		}
		var out []string
		for _, p := range parts {
			if !pyjson.Truthy(p) {
				continue
			}
			s, ok := p.(string)
			if !ok {
				return "", errRaise
			}
			out = append(out, s)
		}
		return strings.Join(out, " "), nil
	}
	return "", nil
}

func messages(body any) ([]any, error) {
	m, err := get(body, "messages", []any{})
	if err != nil {
		return nil, err
	}
	return iterate(m)
}

// userTexts walks the messages from the end: latest is
// _latest_user_text, the first user message with text; newest is
// _new_user_text, the last user message's own text.
func latestUserText(body any) (string, error) {
	m, err := get(body, "messages", []any{})
	if err != nil {
		return "", err
	}
	msgs, err := reversedOf(m)
	if err != nil {
		return "", err
	}
	for _, msg := range msgs {
		role, err := get(msg, "role", nil)
		if err != nil {
			return "", err
		}
		if !eqStr(role, "user") {
			continue
		}
		text, err := messageText(getOr(msg.(*pyjson.Object), "content", ""))
		if err != nil {
			return "", err
		}
		if pystr.Strip(text) != "" {
			return text, nil
		}
	}
	return "", nil
}

func newUserText(body any) (string, error) {
	m, err := get(body, "messages", []any{})
	if err != nil {
		return "", err
	}
	msgs, err := reversedOf(m)
	if err != nil {
		return "", err
	}
	for _, msg := range msgs {
		role, err := get(msg, "role", nil)
		if err != nil {
			return "", err
		}
		if !eqStr(role, "user") {
			continue
		}
		return messageText(getOr(msg.(*pyjson.Object), "content", ""))
	}
	return "", nil
}

// conversationKey is conversation_key: the first user text's SHA-256,
// 16 hex digits, or "no-user-text".
func conversationKey(body any) (string, error) {
	msgs, err := messages(body)
	if err != nil {
		return "", err
	}
	for _, msg := range msgs {
		role, err := get(msg, "role", nil)
		if err != nil {
			return "", err
		}
		if !eqStr(role, "user") {
			continue
		}
		text, err := messageText(getOr(msg.(*pyjson.Object), "content", ""))
		if err != nil {
			return "", err
		}
		if s := pystr.Strip(text); s != "" {
			b, exc := pystr.EncodeUTF8(s)
			if exc != nil {
				return "", errRaise
			}
			sum := sha256.Sum256(b)
			return hex.EncodeToString(sum[:])[:16], nil
		}
	}
	return "no-user-text", nil
}

// blockChars is _block_chars.
func blockChars(block any, depth int) (int, error) {
	if depth > maxPyDepth {
		return 0, errTooDeep
	}
	switch b := block.(type) {
	case string:
		return pystr.Len(b), nil
	case *pyjson.Object:
		text := getOr(b, "text", "")
		n := 0
		if pyjson.Truthy(text) {
			l, err := pyLen(text)
			if err != nil {
				return 0, err
			}
			n = l
		}
		switch inner := getOr(b, "content", nil).(type) {
		case string:
			n += pystr.Len(inner)
		case []any:
			for _, x := range inner {
				m, err := blockChars(x, depth+1)
				if err != nil {
					return 0, err
				}
				n += m
			}
		}
		return n, nil
	}
	return 0, nil
}

// EstimatePrefixTokens is estimate_prefix_tokens: the system prompt's and
// every message's characters, divided by four.
func EstimatePrefixTokens(body any) (int, error) {
	system, err := get(body, "system", "")
	if err != nil {
		return 0, err
	}
	chars := 0
	switch s := system.(type) {
	case string:
		chars += pystr.Len(s)
	case []any:
		for _, b := range s {
			n, err := blockChars(b, 1)
			if err != nil {
				return 0, err
			}
			chars += n
		}
	}
	msgs, err := messages(body)
	if err != nil {
		return 0, err
	}
	for _, msg := range msgs {
		content, err := get(msg, "content", "")
		if err != nil {
			return 0, err
		}
		switch c := content.(type) {
		case string:
			chars += pystr.Len(c)
		case []any:
			for _, b := range c {
				n, err := blockChars(b, 1)
				if err != nil {
					return 0, err
				}
				chars += n
			}
		}
	}
	return chars / 4, nil
}

// RouteTurn is route_turn with the default sticky threshold; sticky is
// the conversation's last routed model, nil when there is none.
func RouteTurn(body any, cheap, local string, sticky any) (Decision, error) {
	requested, err := get(body, "model", "")
	if err != nil {
		return Decision{}, err
	}
	text, err := latestUserText(body)
	if err != nil {
		return Decision{}, err
	}
	if pystr.Strip(text) == "" {
		return Decision{Tier: "tier2-frontier", Model: requested, RequestedModel: requested,
			Reason: "no routing signal; keep the requested model"}, nil
	}
	d := EstimateDifficulty(text)
	if d >= hardThreshold {
		return Decision{Tier: "tier2-frontier", Model: requested, RequestedModel: requested, Difficulty: d,
			Reason: fmt.Sprintf("hard turn (%s); keep the requested model", f2(d))}, nil
	}
	if local != "" {
		return Decision{Tier: "tier1-local", Model: local, RequestedModel: requested, Difficulty: d,
			Downgraded: !eqStr(requested, local),
			Reason: fmt.Sprintf("easy turn (%s); route to the local model — zero quota, "+
				"cache stickiness never blocks the local rung", f2(d))}, nil
	}
	if eqStr(sticky, cheap) {
		return Decision{Tier: "tier1-cheap", Model: cheap, RequestedModel: requested, Difficulty: d,
			Downgraded: !eqStr(requested, cheap),
			Reason: fmt.Sprintf("easy turn (%s); sticky to the cheap model — "+
				"this conversation's warm cache is the cheap one", f2(d))}, nil
	}
	prefix, err := EstimatePrefixTokens(body)
	if err != nil {
		return Decision{}, err
	}
	if prefix >= StickyPrefixThreshold {
		return Decision{Tier: "tier2-frontier", Model: requested, RequestedModel: requested, Difficulty: d,
			Reason: fmt.Sprintf("easy turn (%s) but sticky: ~%d prefix tokens cached against the "+
				"requested model — forfeiting 0.1x reads (and re-writing on return) costs more "+
				"than the downgrade saves", f2(d), prefix)}, nil
	}
	return Decision{Tier: "tier1-cheap", Model: cheap, RequestedModel: requested, Difficulty: d,
		Downgraded: !eqStr(requested, cheap),
		Reason:     fmt.Sprintf("easy turn (%s); route to the cheap model", f2(d))}, nil
}

// f2 is format(x, ".2f").
func f2(x float64) string {
	if math.IsNaN(x) {
		return "nan"
	}
	return fmt.Sprintf("%.2f", x)
}

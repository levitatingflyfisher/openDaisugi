package envgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// Tier1 is tier1.HTTPTier1Provider: one structured call to any model the
// client reaches, a local OpenAI-compatible endpoint included. Any
// failure declines, and the generator falls through to Tier-2.
type Tier1 struct {
	Model   string
	BaseURL *string
	APIKey  *string
	// Name is the provider's name, part of the Tier-1 cache key.
	Name    string
	Timeout time.Duration
	// badBase is a base_url the config file gives as something other than
	// a string: every call with it fails in the oracle, so it declines.
	badBase bool
}

// NewTier1 is HTTPTier1Provider(model, base_url=..., api_key=..., name=...).
// An unprefixed model with a base_url is an OpenAI-compatible one.
func NewTier1(model string, baseURL, apiKey *string, name string) *Tier1 {
	if baseURL != nil && !strings.Contains(model, "/") {
		model = "openai/" + model
	}
	if name == "" {
		name = "http:" + model
	}
	return &Tier1{Model: model, BaseURL: baseURL, APIKey: apiKey, Name: name, Timeout: 30 * time.Second}
}

// ConfigFile is local_setup._TIER1_CONFIG.
const ConfigFile = "local_tier1.json"

// PyError is an exception the oracle raises out of the call, by its class
// name and text.
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// LoadConfiguredTier1 is local_setup.load_configured_tier1: the provider
// local_tier1.json names, or nil when there is none or it does not read.
func LoadConfiguredTier1(dataDir string) (*Tier1, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, ConfigFile))
	if err != nil {
		return nil, nil
	}
	if !utf8.Valid(raw) {
		// read_text raises UnicodeDecodeError, which the oracle does not catch.
		return nil, &PyError{"UnicodeDecodeError", "the Tier-1 config is not UTF-8"}
	}
	v, derr := pyjson.LoadsPy(string(raw), 900)
	if derr != nil {
		return nil, nil
	}
	cfg, isObj := v.(*pyjson.Object)
	if !isObj {
		return nil, &PyError{"AttributeError", "'" + pmodel.TypeName(v) + "' object has no attribute 'get'"}
	}
	m := cfg.Value("model")
	if !pyjson.Truthy(m) {
		return nil, nil
	}
	var base *string
	bad := false
	switch b := cfg.Value("base_url").(type) {
	case nil:
	case string:
		base = &b
	default:
		// "/" not in model still runs, and every call then fails.
		bad = true
		empty := ""
		base = &empty
	}
	model, isStr := m.(string)
	if !isStr {
		if base != nil {
			return nil, &PyError{"TypeError", "argument of type '" + pmodel.TypeName(m) + "' is not iterable"}
		}
		// The oracle builds a provider named after the value, which then
		// always declines; this binary does not.
		return nil, fmt.Errorf("a Tier-1 model that is not a string %w", ErrUnported)
	}
	t := NewTier1(model, base, nil, "")
	t.badBase = bad
	return t, nil
}

// Generate is HTTPTier1Provider.generate_envelope: the envelope, or nil
// when the provider declines.
func (t *Tier1) Generate(c *llm.Client, task string, context *string) *pyjson.Object {
	user := "Task: " + task
	if context != nil && *context != "" {
		user += "\n\nContext:\n" + *context
	}
	if c.Preflight(t.Model) != nil || t.badBase {
		return nil
	}
	call := llm.Call{Model: t.Model, System: Tier1Prompt, User: user,
		Response: llm.Schema{Name: "Envelope", Model: pmodel.Envelope}, MaxRetries: 1,
		Deadline: time.Now().Add(t.Timeout)}
	if t.BaseURL != nil {
		call.BaseURL = *t.BaseURL
	}
	if t.APIKey != nil {
		call.APIKey = *t.APIKey
	}
	out, err := c.Structured(call)
	if err != nil {
		return nil
	}
	return out
}

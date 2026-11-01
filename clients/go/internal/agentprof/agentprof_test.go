package agentprof

import (
	"strings"
	"testing"

	"daisugi-verify/internal/config"
)

func table(t *testing.T, text string) *config.Value {
	t.Helper()
	doc, err := config.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	v := doc.Vals["agent_profiles"]
	return &v
}

const yamlTable = `agent_profiles:
  oai: {wire: openai, base_url: "http://127.0.0.1:9/v1", key_env: OAI_KEY}
  local: {wire: anthropic, base_url: "http://127.0.0.1:8", model: "llama3.3:70b"}
  nokey: {wire: openai, key_env: NOT_SET_HERE}
  bad: {wire: grpc}
  cc1: {wire: claude-code, key_env: X}
  default: {wire: openai}
`

func TestResolveAndLookupTexts(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "OAI_KEY" {
			return "sk-1", true
		}
		return "", false
	}
	ps := Resolve(table(t, yamlTable), env)
	if strings.Join(ps.Names(), ",") != "default,local,nokey,oai" {
		t.Fatalf("names %v", ps.Names())
	}
	want := map[string]string{
		"bad":     "wire must be claude-code, anthropic or openai",
		"cc1":     "the claude-code wire takes no key_env",
		"default": "default is built in",
	}
	for k, v := range want {
		if ps.Invalid[k] != v {
			t.Fatalf("%s: %q", k, ps.Invalid[k])
		}
	}
	zzz, nokey := "zzz", "nokey"
	if _, why := ps.Lookup(&zzz); why != "no agent profile named 'zzz'; the profiles are default, local, nokey, oai" {
		t.Fatal(why)
	}
	if _, why := ps.Lookup(&nokey); why != "agent profile 'nokey' names NOT_SET_HERE, which is not set" {
		t.Fatal(why)
	}
	if p, _ := ps.Lookup(nil); p.Wire != "claude-code" {
		t.Fatal(p)
	}
	if strings.Join(ps.KeyNames(), ",") != "NOT_SET_HERE,OAI_KEY" {
		t.Fatal(ps.KeyNames())
	}
	back := FromBundle(ps.Bundle())
	if k := back.Valid["oai"].Key; k == nil || *k != "sk-1" {
		t.Fatal("the bundle lost the key")
	}
	if !strings.HasPrefix(string(ps.Bundle()), `{"default":{"base_url":null,"key":null,"key_env":null,"model":null,"wire":"claude-code"},"local":`) {
		t.Fatalf("bundle %s", ps.Bundle())
	}
}

func TestConfigText(t *testing.T) {
	if got := ConfigText(*table(t, yamlTable)); got != "bad, cc1, default, local, nokey, oai" {
		t.Fatal(got)
	}
	if got := ConfigText(*table(t, "agent_profiles:\n")); got != "none" {
		t.Fatal(got)
	}
	if got := ConfigText(*table(t, "agent_profiles: [1]\n")); got != "not a mapping" {
		t.Fatal(got)
	}
}

const maxTokensTable = `agent_profiles:
  big: {wire: openai, max_tokens: 4096}
  none: {wire: anthropic, max_tokens: null}
  plain: {wire: openai}
  top: {wire: openai, max_tokens: 1000000}
  zero: {wire: openai, max_tokens: 0}
  neg: {wire: openai, max_tokens: -5}
  text: {wire: openai, max_tokens: "400"}
  flag: {wire: openai, max_tokens: true}
  half: {wire: openai, max_tokens: 1.5}
  huge: {wire: openai, max_tokens: 1000001}
  cc: {wire: claude-code, max_tokens: 400}
`

func TestMaxTokens(t *testing.T) {
	ps := Resolve(table(t, maxTokensTable), func(string) (string, bool) { return "", false })
	bad := "max_tokens must be a whole number from 1 to 1000000"
	for _, n := range []string{"zero", "neg", "text", "flag", "half", "huge"} {
		if ps.Invalid[n] != bad {
			t.Errorf("%s: %q", n, ps.Invalid[n])
		}
	}
	if ps.Invalid["cc"] != "the claude-code wire takes no max_tokens" {
		t.Errorf("cc: %q", ps.Invalid["cc"])
	}
	for n, want := range map[string]int{"big": 4096, "top": 1000000} {
		if p := ps.Valid[n]; p.MaxTokens == nil || *p.MaxTokens != want {
			t.Errorf("%s: %v", n, p.MaxTokens)
		}
	}
	for _, n := range []string{"none", "plain"} {
		if ps.Valid[n].MaxTokens != nil {
			t.Errorf("%s: want no max_tokens", n)
		}
	}
	// A child agent's bundle does not carry it.
	if strings.Contains(string(ps.Bundle()), "max_tokens") {
		t.Errorf("bundle: %s", ps.Bundle())
	}
}

const timeoutTable = `agent_profiles:
  slow: {wire: openai, timeout_s: 240}
  none: {wire: anthropic, timeout_s: null}
  plain: {wire: openai}
  top: {wire: openai, timeout_s: 86400}
  zero: {wire: openai, timeout_s: 0}
  neg: {wire: openai, timeout_s: -5}
  text: {wire: openai, timeout_s: "400"}
  flag: {wire: openai, timeout_s: true}
  half: {wire: openai, timeout_s: 1.5}
  huge: {wire: openai, timeout_s: 86401}
  cc: {wire: claude-code, timeout_s: 300}
  cczero: {wire: claude-code, timeout_s: 0}
`

func TestTimeoutS(t *testing.T) {
	ps := Resolve(table(t, timeoutTable), func(string) (string, bool) { return "", false })
	bad := "timeout_s must be a whole number from 1 to 86400"
	for _, n := range []string{"zero", "neg", "text", "flag", "half", "huge", "cczero"} {
		if ps.Invalid[n] != bad {
			t.Errorf("%s: %q", n, ps.Invalid[n])
		}
	}
	for n, want := range map[string]int{"slow": 240, "top": 86400, "cc": 300} {
		if p := ps.Valid[n]; p.TimeoutS == nil || *p.TimeoutS != want {
			t.Errorf("%s: %v", n, p.TimeoutS)
		}
	}
	for _, n := range []string{"none", "plain"} {
		if ps.Valid[n].TimeoutS != nil {
			t.Errorf("%s: want no timeout_s", n)
		}
	}
	if strings.Contains(string(ps.Bundle()), "timeout_s") {
		t.Errorf("bundle: %s", ps.Bundle())
	}
}

func TestThinking(t *testing.T) {
	ps := Resolve(table(t, `agent_profiles:
  a: {wire: openai, thinking: off}
  b: {wire: anthropic, thinking: high}
  c: {wire: openai, thinking: null}
  d: {wire: openai}
  e: {wire: openai, thinking: none}
  f: {wire: openai, thinking: true}
  g: {wire: openai, thinking: 3}
  h: {wire: openai, thinking: High}
  cc: {wire: claude-code, thinking: off}
`), func(string) (string, bool) { return "", false })
	bad := "thinking must be off, low, medium or high"
	for _, n := range []string{"e", "f", "g", "h"} {
		if ps.Invalid[n] != bad {
			t.Errorf("%s: %q", n, ps.Invalid[n])
		}
	}
	if ps.Invalid["cc"] != "the claude-code wire takes no thinking" {
		t.Errorf("cc: %q", ps.Invalid["cc"])
	}
	for n, want := range map[string]string{"a": "off", "b": "high"} {
		if p := ps.Valid[n]; p.Thinking == nil || *p.Thinking != want {
			t.Errorf("%s: %v", n, p.Thinking)
		}
	}
	for _, n := range []string{"c", "d"} {
		if ps.Valid[n].Thinking != nil {
			t.Errorf("%s: want no thinking", n)
		}
	}
	if strings.Contains(string(ps.Bundle()), "thinking") {
		t.Errorf("bundle: %s", ps.Bundle())
	}
}

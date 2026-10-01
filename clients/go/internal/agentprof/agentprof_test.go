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

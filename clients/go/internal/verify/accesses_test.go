package verify

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// clients/fixtures/dialect/accesses.json is write_paths.shell_accesses for a
// fixed set of shell commands, with no base and with one. It is written by
// clients/dialect_cases.py from the oracle.
func TestAccessesMatchTheOracle(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/dialect/accesses.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct {
			Command string   `json:"command"`
			Base    *string  `json:"base"`
			Reads   [][3]any `json:"reads"`
			Writes  [][3]any `json:"writes"`
			RU      *string  `json:"reads_unknown"`
			WU      *string  `json:"writes_unknown"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	key := func(path string, tree bool, via string) string {
		if tree {
			return path + "\x00tree\x00" + via
		}
		return path + "\x00path\x00" + via
	}
	got := func(as []Access) []string {
		out := []string{}
		for _, a := range as {
			out = append(out, key(a.Path, a.Tree, a.Via))
		}
		sort.Strings(out)
		return out
	}
	want := func(ts [][3]any) []string {
		out := []string{}
		for _, t := range ts {
			out = append(out, key(t[0].(string), t[1].(bool), t[2].(string)))
		}
		sort.Strings(out)
		return out
	}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, c := range fx.Cases {
		base := ""
		if c.Base != nil {
			base = *c.Base
		}
		g := ShellAccesses(c.Command, base)
		if g.ReadsUnknown != str(c.RU) || g.WritesUnknown != str(c.WU) {
			t.Errorf("%q base %q: unknown = (%q, %q), want (%q, %q)", c.Command, base,
				g.ReadsUnknown, g.WritesUnknown, str(c.RU), str(c.WU))
		}
		if !same(got(g.Reads), want(c.Reads)) {
			t.Errorf("%q base %q: reads = %q, want %q", c.Command, base, got(g.Reads), want(c.Reads))
		}
		if !same(got(g.Writes), want(c.Writes)) {
			t.Errorf("%q base %q: writes = %q, want %q", c.Command, base, got(g.Writes), want(c.Writes))
		}
	}
}

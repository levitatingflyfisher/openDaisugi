package switchyard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseTOMLReadsADeploymentFile(t *testing.T) {
	doc, err := ParseTOML("schema_version = 1\n\n[llm_clients.capable] # c\nformat = \"anthropic_messages\"\n" +
		"forward_auth = true\n\n[targets.\"a b\"]\nid = 'x\\y'\nn = 1_000\nf = 0.5\nl = [1, \"s\", ]\n" +
		"i = { k = \"v\", m.n = 2 }\n[routes.r]\nid = \"daisugi\"\ncapable_target = \"a b\"\n")
	if err != nil {
		t.Fatal(err)
	}
	ab := doc.Vals["targets"].(*Table).Vals["a b"].(*Table)
	if ab.Vals["id"] != `x\y` || ab.Vals["n"] != int64(1000) || ab.Vals["f"] != 0.5 {
		t.Fatalf("values: %#v", ab.Vals)
	}
	if doc.Vals["llm_clients"].(*Table).Vals["capable"].(*Table).Vals["forward_auth"] != true {
		t.Fatal("bool")
	}
}

func TestParseTOMLRefusesWhatItDoesNotRead(t *testing.T) {
	for _, text := range []string{"a = \"\"\"x\"\"\"\n", "[[t]]\n", "a = 1979-05-27\n", "a = 1\na = 2\n",
		"[t]\n[t]\n", "a = \"\\q\"\n", "a = 0x10\n", "a = 01\n", "[routes\n"} {
		if _, err := ParseTOML(text); !errors.Is(err, ErrTOML) {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestRouteTargetsAndAuth(t *testing.T) {
	dir := t.TempDir()
	targets := TargetsFromConfig(Config{RouteID: "daisugi", CapableModel: "claude-sonnet-5", EfficientModel: strp("llama3")})
	text, err := Render(targets, "daisugi", func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	p, err := WriteConfig(dir, text, "s.toml")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	c, e, err := RouteTargets(p, "daisugi")
	if err != nil || c != "claude-sonnet-5" || e != "llama3" {
		t.Fatalf("%q %q %v", c, e, err)
	}
	a := AuthFromTOML(p, "daisugi")
	if a == nil || a.Capable != ForwardLogin || a.Efficient != "no credential" {
		t.Fatalf("%+v", a)
	}
	if _, _, err := RouteTargets(filepath.Join(dir, "none.toml"), "daisugi"); err == nil {
		t.Fatal("a missing file must not meter")
	}
	if _, err := Render(&Targets{CapableID: "m", EfficientID: "m"}, "d", nil); err == nil {
		t.Fatal("one model for both tiers must be refused")
	}
}

func strp(s string) *string { return &s }

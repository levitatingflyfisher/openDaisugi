package switchyard

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"daisugi-verify/internal/pyjson"
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

// typedTOML is clients/toml_cases.py's typed dump of a parsed value.
func typedTOML(v any) any {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return pyjson.NewObject().Set("int", strconv.FormatInt(x, 10))
	case BigInt:
		return pyjson.NewObject().Set("int", x.Text)
	case float64:
		return pyjson.NewObject().Set("float", pyRepr(x))
	case string:
		return pyjson.NewObject().Set("str", x)
	case DateTime:
		return pyjson.NewObject().Set("datetime", x.Repr())
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = typedTOML(e)
		}
		return pyjson.NewObject().Set("array", out)
	case *Table:
		out := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			out[i] = []any{k, typedTOML(x.Vals[k])}
		}
		return pyjson.NewObject().Set("table", out)
	}
	panic(fmt.Sprintf("%T", v))
}

// TestParseTOMLAgainstTomllib reads clients/fixtures/toml
// (clients/toml_cases.py): tomllib.loads on thousands of texts. The
// reader gives the same document, or a TOMLDecodeError where tomllib
// raises one.
func TestParseTOMLAgainstTomllib(t *testing.T) {
	f, err := os.Open("../../../fixtures/toml/cases.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n, bad := 0, 0
	for sc.Scan() {
		c, err := pyjson.Loads(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		o := c.(*pyjson.Object)
		text := o.Value("text").(string)
		want := pyjson.Dumps(o.Value("expect"), true)
		doc, perr := ParseTOML(text)
		var got string
		if perr != nil {
			var te *TOMLDecodeError
			var ve *TOMLValueError
			switch {
			case errors.As(perr, &te):
				got = pyjson.Dumps(pyjson.NewObject().Set("exc", "TOMLDecodeError").Set("msg", te.Msg), true)
			case errors.As(perr, &ve):
				got = pyjson.Dumps(pyjson.NewObject().Set("exc", "ValueError").Set("msg", ve.Msg), true)
			default:
				t.Fatalf("%q: %v", text, perr)
			}
		} else {
			got = pyjson.Dumps(typedTOML(doc), true)
		}
		n++
		if got != want {
			bad++
			if bad <= 20 {
				t.Errorf("%q:\n got  %s\n want %s", text, got, want)
			}
		}
	}
	if n < 3000 {
		t.Fatalf("only %d cases", n)
	}
	if bad > 0 {
		t.Errorf("%d of %d disagree", bad, n)
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

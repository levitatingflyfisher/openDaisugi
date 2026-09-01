package gateroot

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daisugi-verify/internal/pyjson"
)

// Expected values are what pathlib, hook._safe_session_id and pydantic
// print for the same input (Python 3.12, pydantic 2.13).
func TestPathStr(t *testing.T) {
	for in, want := range map[string]string{
		"/a//b/./c/": "/a/b/c", "//x/y": "//x/y", "///x": "/x", "": ".", "a/./b": "a/b", "/": "/",
	} {
		if got := PathStr(in); got != want {
			t.Errorf("PathStr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeSessionID(t *testing.T) {
	for in, want := range map[string]string{
		"../s1": "_s1", "a/b": "a_b", "...": "no-session", "s é": "s__", "ok-1.2_x": "ok-1.2_x",
	} {
		if got := SafeSessionID(in); got != want {
			t.Errorf("SafeSessionID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStem(t *testing.T) {
	for in, want := range map[string]string{".json": ".json", "a.json.json": "a.json", "b c.json": "b c", ".hidden.json": ".hidden"} {
		if got := stem(in); got != want {
			t.Errorf("stem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveFollowsLinksAndKeepsMissingParts(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(filepath.Join(dir, "link", "missing", "..", "x"))
	if err != nil || got != filepath.Join(real, "x") {
		t.Fatalf("%q %v", got, err)
	}
}

func TestFloatJSON(t *testing.T) {
	for f, want := range map[float64]string{0.1: "0.1", 1: "1.0", 1e16: "1e+16", 1e-7: "1e-7", 1e22: "1e+22",
		123456789.123: "123456789.123", 1e-5: "0.00001", 2.5e-5: "0.000025", -2.5e-5: "-0.000025",
		9.9e-6: "9.9e-6", 0.0001: "0.0001", 9999999999999998.0: "9999999999999998.0", 5e-324: "5e-324",
		1.7976931348623157e308: "1.7976931348623157e+308", 1.2345678901234568e+16: "1.2345678901234568e+16"} {
		if got := FloatJSON(f); got != want {
			t.Errorf("FloatJSON(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestLaxInt(t *testing.T) {
	for in, want := range map[string]string{"12": "12", " 12 ": "12", "1_000": "1000", "+5": "5", "0012": "12", "12.0": "12"} {
		got, err := laxInt(in, "k")
		if err != nil || got.Text != want {
			t.Errorf("laxInt(%q) = %v %v, want %s", in, got, err, want)
		}
	}
	for _, in := range []any{"0x10", "12.5", "lots", pyjson.Float(1.5)} {
		if _, err := laxInt(in, "k"); err == nil {
			t.Errorf("laxInt(%v) should fail", in)
		}
	}
}

func TestDisarmArm(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "gate")
	marker, followed, err := Disarm(root)
	if err != nil || !IsDisarmed(root) || followed != "" {
		t.Fatal(marker, followed, err)
	}
	st, _ := os.Stat(root)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %o", st.Mode().Perm())
	}
	if err := Arm(root); err != nil || IsDisarmed(root) {
		t.Fatal(err)
	}
}

// An envelope with NaN, Infinity or -Infinity in any number is invalid
// (models.non_finite_error): gate register refuses it for certain, as
// Python does, and never reads it as a null limit.
func TestValidateRefusesNonFiniteNumbers(t *testing.T) {
	big := pyjson.Int{Text: "1" + strings.Repeat("0", 400)}
	for name, perms := range map[string]*pyjson.Object{
		"nan float":    pyjson.NewObject().Set("velocity_limit", pyjson.Float(math.NaN())),
		"inf float":    pyjson.NewObject().Set("torque_limit", pyjson.Float(math.Inf(1))),
		"nan string":   pyjson.NewObject().Set("velocity_limit", " nan "),
		"-inf string":  pyjson.NewObject().Set("velocity_limit", "-Infinity"),
		"1e999 string": pyjson.NewObject().Set("velocity_limit", "1e999"),
		"big int":      pyjson.NewObject().Set("velocity_limit", big),
		"bound":        pyjson.NewObject().Set("workspace_bounds", []any{[]any{pyjson.Int{Text: "0"}, pyjson.Int{Text: "0"}, pyjson.Int{Text: "0"}}, []any{pyjson.Int{Text: "1"}, pyjson.Float(math.Inf(-1)), pyjson.Int{Text: "1"}}}),
	} {
		in := pyjson.NewObject().Set("generated_by", "g").Set("task", "t").Set("permissions", perms)
		_, err := Validate(in)
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("%s: got %v, want an InvalidError", name, err)
		}
	}
	expr := pyjson.NewObject().Set("op", "equals").Set("value", []any{pyjson.Float(math.NaN())})
	in := pyjson.NewObject().Set("generated_by", "g").Set("task", "t").Set("permissions", pyjson.NewObject()).
		Set("invariants", []any{pyjson.NewObject().Set("type", "t").Set("description", "d").Set("expr", expr)})
	if _, err := Validate(in); !errors.As(err, new(*InvalidError)) {
		t.Errorf("nan in expr: got %v, want an InvalidError", err)
	}
}

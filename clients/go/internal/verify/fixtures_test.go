package verify

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// clients/fixtures/semantics.json is the oracle-generated fixture shared by
// every client (clients/fixtures/generate.py). It freezes 134 concrete
// input/output pairs for the pure-function semantics this package must
// match exactly, independent of the (uncommitted) corpus.
const fixturePath = "../../../fixtures/semantics.json"

type semanticsFixture struct {
	V            int `json:"v"`
	HeadAllowed  []struct {
		Head      string   `json:"head"`
		Allowlist []string `json:"allowlist"`
		Allowed   bool     `json:"allowed"`
	} `json:"head_allowed"`
	PathMatch []struct {
		Path    string   `json:"path"`
		Globs   []string `json:"globs"`
		Matched bool     `json:"matched"`
	} `json:"path_match"`
	ExtractHead []struct {
		Line string  `json:"line"`
		Head *string `json:"head"`
	} `json:"extract_head"`
	Metachar []struct {
		Command string `json:"command"`
		Hit     bool   `json:"hit"`
	} `json:"metachar"`
	Interpreter []struct {
		Command string `json:"command"`
		Payload *struct {
			Head          string   `json:"head"`
			Opaque        bool     `json:"opaque"`
			InnerCommands []string `json:"inner_commands"`
		} `json:"payload"`
	} `json:"interpreter"`
	ResolveStrict []struct {
		Strict    *bool  `json:"strict"`
		Stakes    string `json:"stakes"`
		Effective bool   `json:"effective"`
	} `json:"resolve_strict"`
}

func loadFixture(t *testing.T) semanticsFixture {
	t.Helper()
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f semanticsFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return f
}

func TestFixtureHeadAllowed(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.HeadAllowed {
		got := headAllowed(c.Head, c.Allowlist)
		if got != c.Allowed {
			t.Errorf("headAllowed(%q, %v) = %v, want %v", c.Head, c.Allowlist, got, c.Allowed)
		}
	}
	t.Logf("checked %d head_allowed cases", len(f.HeadAllowed))
}

func TestFixturePathMatch(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.PathMatch {
		got := PathMatchesAny(c.Path, c.Globs)
		if got != c.Matched {
			t.Errorf("PathMatchesAny(%q, %v) = %v, want %v", c.Path, c.Globs, got, c.Matched)
		}
	}
	t.Logf("checked %d path_match cases", len(f.PathMatch))
}

func TestFixtureExtractHead(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.ExtractHead {
		// generate.py computes the fixture's "head" from l.strip(), not the
		// raw "line" — verify.py's real callers always strip first too
		// (_check_shell_command's `stripped = command.strip()`).
		got, ok := extractShellHead(strings.TrimSpace(c.Line))
		if c.Head == nil {
			if ok {
				t.Errorf("extractShellHead(%q) = %q, want nil", c.Line, got)
			}
			continue
		}
		if !ok || got != *c.Head {
			t.Errorf("extractShellHead(%q) = (%q, %v), want %q", c.Line, got, ok, *c.Head)
		}
	}
	t.Logf("checked %d extract_head cases", len(f.ExtractHead))
}

func TestFixtureMetachar(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.Metachar {
		got := hasShellMetachar(c.Command)
		if got != c.Hit {
			t.Errorf("hasShellMetachar(%q) = %v, want %v", c.Command, got, c.Hit)
		}
	}
	t.Logf("checked %d metachar cases", len(f.Metachar))
}

func TestFixtureInterpreter(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.Interpreter {
		got, ok := ParseInterpreter(c.Command)
		if c.Payload == nil {
			if ok {
				t.Errorf("ParseInterpreter(%q) = %+v, want nil", c.Command, got)
			}
			continue
		}
		if !ok {
			t.Errorf("ParseInterpreter(%q) = nil, want %+v", c.Command, c.Payload)
			continue
		}
		wantInner := c.Payload.InnerCommands
		if wantInner == nil {
			wantInner = []string{}
		}
		gotInner := got.InnerCommands
		if gotInner == nil {
			gotInner = []string{}
		}
		if got.Head != c.Payload.Head || got.Opaque != c.Payload.Opaque || !reflect.DeepEqual(gotInner, wantInner) {
			t.Errorf("ParseInterpreter(%q) = %+v (inner=%v), want head=%q opaque=%v inner=%v",
				c.Command, got, gotInner, c.Payload.Head, c.Payload.Opaque, wantInner)
		}
	}
	t.Logf("checked %d interpreter cases", len(f.Interpreter))
}

func TestFixtureResolveStrict(t *testing.T) {
	f := loadFixture(t)
	for _, c := range f.ResolveStrict {
		env := Envelope{Stakes: c.Stakes}
		got := ResolveStrict(c.Strict, env)
		if got != c.Effective {
			t.Errorf("ResolveStrict(%v, stakes=%q) = %v, want %v", c.Strict, c.Stakes, got, c.Effective)
		}
	}
	t.Logf("checked %d resolve_strict cases", len(f.ResolveStrict))
}

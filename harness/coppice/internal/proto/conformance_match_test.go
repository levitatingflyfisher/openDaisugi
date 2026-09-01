package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMatchWildcardAndExtraKeys(t *testing.T) {
	m := NewMatcher()
	exp := decode(t, `{"id":"$id","ok":true,"result":{"pid":"*","panes":"*"}}`)
	act := decode(t, `{"id":"1","ok":true,"result":{"pid":42,"panes":0,"socket":"/x"}}`)
	if err := m.Match(exp, act, "1"); err != nil {
		t.Fatalf("want a match, got %v", err)
	}
}

func TestMatchRequestIDMustAgree(t *testing.T) {
	m := NewMatcher()
	exp := decode(t, `{"id":"$id"}`)
	act := decode(t, `{"id":"2"}`)
	err := m.Match(exp, act, "1")
	if err == nil || !strings.Contains(err.Error(), "request id") {
		t.Fatalf("want an id mismatch, got %v", err)
	}
	// A numeric id agrees with a numeric id, as JSON-RPC sends it.
	if err := m.Match(decode(t, `{"id":"$id"}`), decode(t, `{"id":7}`), float64(7)); err != nil {
		t.Fatalf("numeric id should match: %v", err)
	}
}

func TestMatchWildcardNeedsThePresentKey(t *testing.T) {
	m := NewMatcher()
	err := m.Match(decode(t, `{"result":{"pane":"*"}}`), decode(t, `{"result":{}}`), nil)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf(`"*" must not match an absent key, got %v`, err)
	}
}

func TestMatchArraysNeedTheSameLength(t *testing.T) {
	m := NewMatcher()
	if err := m.Match(decode(t, `[1,"*"]`), decode(t, `[1,2,3]`), nil); err == nil {
		t.Fatal("want a length mismatch")
	}
	if err := m.Match(decode(t, `[1,"*"]`), decode(t, `[1,{"a":1}]`), nil); err != nil {
		t.Fatalf("want a match, got %v", err)
	}
}

func TestMatchScalarsAreExact(t *testing.T) {
	m := NewMatcher()
	if err := m.Match(decode(t, `{"ok":false}`), decode(t, `{"ok":true}`), nil); err == nil {
		t.Fatal("false must not match true")
	}
	if err := m.Match(decode(t, `{"code":-32000}`), decode(t, `{"code":-32000}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Match(decode(t, `{"n":null}`), decode(t, `{"n":0}`), nil); err == nil {
		t.Fatal("null must not match 0")
	}
}

func TestBindThenSubstitute(t *testing.T) {
	m := NewMatcher()
	exp := decode(t, `{"id":"$id","result":{"pane":"$p"}}`)
	act := decode(t, `{"id":"2","result":{"pane":"w1:p3"}}`)
	if err := m.Match(exp, act, "2"); err != nil {
		t.Fatal(err)
	}
	req, err := m.Substitute(decode(t, `{"id":"3","cmd":"pane.read","pane":"$p","tags":["$p"]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := req.(map[string]any)
	if got["pane"] != "w1:p3" || got["tags"].([]any)[0] != "w1:p3" {
		t.Fatalf("substitute did not replace $p: %v", got)
	}
	if got["cmd"] != "pane.read" {
		t.Fatalf("substitute changed a plain string: %v", got)
	}
}

func TestSubstituteRefusesAnUnboundName(t *testing.T) {
	m := NewMatcher()
	if _, err := m.Substitute(decode(t, `{"pane":"$nope"}`)); err == nil {
		t.Fatal("an unbound $name must be an error, not a literal sent to the server")
	}
	if _, err := m.Substitute(decode(t, `{"pane":"$id"}`)); err == nil {
		t.Fatal("$id is never bound, so a request may not use it")
	}
}

func TestSubstituteLeavesWildcardAlone(t *testing.T) {
	m := NewMatcher()
	v, err := m.Substitute(decode(t, `{"x":"*","y":"$"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := v.(map[string]any)
	if got["x"] != "*" || got["y"] != "$" {
		t.Fatalf("got %v", got)
	}
}

func TestHasIDAndRequestID(t *testing.T) {
	if HasID(decode(t, `{"event":"state"}`)) {
		t.Fatal("an event has no id")
	}
	if !HasID(decode(t, `{"id":null,"error":{}}`)) {
		t.Fatal("a reply with a null id still has an id key")
	}
	if HasID(decode(t, `[1]`)) {
		t.Fatal("an array is not a reply")
	}
	if RequestID(decode(t, `{"id":7,"method":"x"}`)) != float64(7) {
		t.Fatal("RequestID must return the id value")
	}
	if RequestID(decode(t, `[]`)) != nil {
		t.Fatal("RequestID of a non-object is nil")
	}
}

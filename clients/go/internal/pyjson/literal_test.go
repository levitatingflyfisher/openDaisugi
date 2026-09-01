package pyjson

import "testing"

func TestLiteralEval(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		ok   bool
	}{
		{`{'a': 1, "b": [True, None, -2.5, (1,), ()], 'c': 'x\'y\n'}`, `{"a": 1, "b": [true, null, -2.5, [1], []], "c": "x'y\n"}`, true},
		{`{'a': (1)}`, `{"a": 1}`, true},
		{`{'a': 1_000}`, `{"a": 1000}`, true},
		{`{'a': 007}`, "", false},
		{`{'a': __import__('os')}`, "", false},
		{`{'a': """x"""}`, "", false},
		{`{1: 2}`, "", false},
		{`{'a': 1} x`, "", false},
	} {
		v, ok := LiteralEval(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok %v", c.in, ok)
			continue
		}
		if ok && Dumps(v, true) != c.want {
			t.Errorf("%s: %s, want %s", c.in, Dumps(v, true), c.want)
		}
	}
}

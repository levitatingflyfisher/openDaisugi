package pyjson

import "testing"

func TestLiteralEval(t *testing.T) {
	for _, c := range []struct {
		in     string
		want   string
		status LitStatus
		tuple  bool
	}{
		{`{'a': 1, "b": [True, None, -2.5, (1,), ()], 'c': 'x\'y\n'}`, `{"a": 1, "b": [true, null, -2.5, [1], []], "c": "x'y\n"}`, LitValue, true},
		{`{'a': (1)}`, `{"a": 1}`, LitValue, false},
		{`{'a': 1_000, 'b': 0x_1F, 'c': 0o17, 'd': 0b11, 'e': 123456789012345678901234567890}`, `{"a": 1000, "b": 31, "c": 15, "d": 3, "e": 123456789012345678901234567890}`, LitValue, false},
		{"{'a': 'x' \"y\" r'\\d', 'b': '''p\nq''', 'c': u'\\x41\\101\\n'}", `{"a": "xy\\d", "b": "p\nq", "c": "AA\n"}`, LitValue, false},
		{`{'a': 007}`, "", LitNone, false},
		{`{'a': __import__('os')}`, "", LitNone, false},
		{`{'a': 1} x`, "", LitNone, false},
		{`{[1]: 2}`, "", LitNone, false},
		{`{'a': '\d'}`, "", LitRefused, false},
		{`{'a': 1if 1 else 2}`, "", LitRefused, false},
		{`{'a': {1, 2}}`, "", LitRefused, false},
		{`{1: 2}`, "", LitRefused, false},
		{`{'a': 1+2j}`, "", LitRefused, false},
		{`{'a': -(1+2j)}`, "", LitNone, false},
	} {
		got := LiteralEval(c.in)
		if got.Status != c.status {
			t.Errorf("%s: status %v, want %v", c.in, got.Status, c.status)
			continue
		}
		if got.Status == LitValue && (Dumps(got.Value, true) != c.want || got.Tuple != c.tuple) {
			t.Errorf("%s: %s (tuple %v), want %s", c.in, Dumps(got.Value, true), got.Tuple, c.want)
		}
	}
}

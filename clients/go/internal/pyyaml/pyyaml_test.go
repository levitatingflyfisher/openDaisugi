package pyyaml

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// Dump is testdata/oracle.py's dump.
func Dump(v any) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case pyjson.Int:
		return map[string]any{"int": x.Text}
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) {
			return map[string]any{"float": "nan"}
		}
		return map[string]any{"float": pmodel.FloatRepr(f)}
	case *pyjson.Object:
		pairs := []any{}
		for _, k := range x.Keys() {
			var key any = k
			if strings.HasPrefix(k, "\x00") {
				key = map[string]any{"key": "?"}
			}
			pairs = append(pairs, []any{key, Dump(x.Value(k))})
		}
		return map[string]any{"dict": pairs}
	case Timestamp:
		if strings.Contains(x.Text, ":") {
			return map[string]any{"other": "datetime"}
		}
		return map[string]any{"other": "date"}
	}
	return map[string]any{"other": "?"}
}

// normKeys turns a resolved non-str key into the placeholder Dump gives.
func normKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if d, ok := x["dict"]; ok {
			pairs := d.([]any)
			out := make([]any, len(pairs))
			for i, p := range pairs {
				kv := p.([]any)
				k := kv[0]
				if m, isMap := k.(map[string]any); isMap {
					if _, ok := m["key"]; ok {
						k = map[string]any{"key": "?"}
					}
				}
				out[i] = []any{k, normKeys(kv[1])}
			}
			return map[string]any{"dict": out}
		}
	}
	return v
}

func TestResolvesLikePyYAML(t *testing.T) {
	for text, want := range map[string]any{
		"a: yes\n":        map[string]any{"dict": []any{[]any{"a", true}}},
		"a: 0x10\n":       map[string]any{"dict": []any{[]any{"a", map[string]any{"int": "16"}}}},
		"a: 010\n":        map[string]any{"dict": []any{[]any{"a", map[string]any{"int": "8"}}}},
		"a: 1:30\n":       map[string]any{"dict": []any{[]any{"a", map[string]any{"int": "90"}}}},
		"a: 1e5\n":        map[string]any{"dict": []any{[]any{"a", "1e5"}}},
		"a: ~\n":          map[string]any{"dict": []any{[]any{"a", nil}}},
		"a: 'it''s' #c\n": map[string]any{"dict": []any{[]any{"a", "it's"}}},
		"":                nil,
	} {
		v, exc, why := Load(text)
		if exc != nil || why != nil || !reflect.DeepEqual(Dump(v), want) {
			t.Errorf("%q: got %v %v %v", text, Dump(v), exc, why)
		}
	}
	if _, _, why := Load("a: [1, 2]\n"); why == nil {
		t.Error("a flow sequence was read")
	}
	if _, exc, _ := Load("a: \x7f\n"); exc == nil {
		t.Error("a DEL was accepted")
	}
}

// TestAgainstOracle reads testdata/oracle.py's output from
// DAISUGI_PYYAML_ORACLE.
func TestAgainstOracle(t *testing.T) {
	path := os.Getenv("DAISUGI_PYYAML_ORACLE")
	if path == "" {
		t.Skip("DAISUGI_PYYAML_ORACLE is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n, skipped, bad := 0, 0, 0
	reasons := map[string]int{}
	for sc.Scan() {
		var row struct {
			Text string `json:"text"`
			YAML any    `json:"yaml"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		n++
		text := strings.ReplaceAll(strings.ReplaceAll(row.Text, "\r\n", "\n"), "\r", "\n")
		v, exc, why := Load(text)
		if why != nil {
			skipped++
			reasons[why.Why]++
			continue
		}
		var got any
		if exc != nil {
			got = map[string]any{"exc": "?"}
		} else {
			got = Dump(v)
		}
		want := normKeys(row.YAML)
		if m, ok := want.(map[string]any); ok {
			if _, isExc := m["exc"]; isExc {
				want = map[string]any{"exc": "?"}
			}
		}
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		if string(gj) != string(wj) {
			bad++
			if bad < 20 {
				t.Errorf("%q:\n got %s\nwant %s", row.Text, gj, wj)
			}
		}
	}
	t.Logf("%d texts, %d outside the subset (%v), %d differ", n, skipped, reasons, bad)
}

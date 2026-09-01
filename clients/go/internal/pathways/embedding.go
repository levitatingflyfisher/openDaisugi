package pathways

import (
	"strconv"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// parseEmbedding reads task_embedding_json as json.loads and then
// list[float] read it. The common text, json.dumps of a list of floats,
// takes a fast path; anything else goes through the general reader.
// ok is false when the text is not a list of numbers pydantic takes.
func parseEmbedding(text string) (vec []float64, ok bool) {
	if v, fast := fastFloats(text); fast {
		return v, true
	}
	raw, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	out, verr := pmodel.Validate("CompiledPathway", pmodel.List{Elem: pmodel.Float{}}, raw, pmodel.Python)
	if verr != nil {
		return nil, false
	}
	xs := out.([]any)
	vec = make([]float64, len(xs))
	for i, x := range xs {
		vec[i] = x.(float64)
	}
	return vec, true
}

// fastFloats reads "[f, f, ...]" as json.dumps writes a list of finite
// floats: ", " between items, each a float repr. Anything else (ints,
// NaN, other spacing) is left to the general reader.
func fastFloats(s string) ([]float64, bool) {
	n := len(s)
	if n < 2 || s[0] != '[' || s[n-1] != ']' {
		return nil, false
	}
	if n == 2 {
		return []float64{}, true
	}
	out := make([]float64, 0, 64)
	i := 1
	for {
		j := i
		for j < n-1 && s[j] != ',' {
			j++
		}
		tok := s[i:j]
		switch tok {
		case "0.0":
			out = append(out, 0)
		case "-0.0":
			out = append(out, negZero)
		default:
			if !floatToken(tok) {
				return nil, false
			}
			f, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				return nil, false
			}
			out = append(out, f)
		}
		if j == n-1 {
			return out, true
		}
		if j+1 >= n || s[j+1] != ' ' {
			return nil, false
		}
		i = j + 2
	}
}

var negZero = func() float64 { z := 0.0; return -z }()

// floatToken is a JSON number with a fraction or an exponent, the form
// Python writes a float in. An int token is left to the general reader.
func floatToken(t string) bool {
	i, n := 0, len(t)
	if i < n && t[i] == '-' {
		i++
	}
	digits := func() int {
		k := i
		for i < n && t[i] >= '0' && t[i] <= '9' {
			i++
		}
		return i - k
	}
	if i < n && t[i] == '0' {
		i++
	} else if digits() == 0 {
		return false
	}
	frac, exp := false, false
	if i < n && t[i] == '.' {
		i++
		if digits() == 0 {
			return false
		}
		frac = true
	}
	if i < n && (t[i] == 'e' || t[i] == 'E') {
		i++
		if i < n && (t[i] == '+' || t[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
		exp = true
	}
	return i == n && (frac || exp)
}

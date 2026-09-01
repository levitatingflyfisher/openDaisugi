// Package lexical is the zero-model matcher of ADR-0019: signed feature
// hashing over unigrams. It needs no model file and no network.
//
// It is _search._LexicalEmbedder of the Python oracle, byte for byte:
// Python's str.lower(), ASCII [a-z0-9_]+ runs, the same stopwords, an
// 8-byte BLAKE2b per token, a bucket and a sign from that hash, and the
// square root of the term count.
package lexical

import (
	"encoding/binary"
	"math"

	"daisugi-verify/internal/pystr"
)

const (
	// Identity is the provenance stamp. It changes when the feature
	// scheme changes.
	Identity = "lexical-hash-v1"
	// Dim is the width of a vector.
	Dim = 4096
	// Threshold is the reuse threshold, FPR-matched in ADR-0019.
	Threshold = 0.25
)

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range []string{
		"a", "an", "the", "and", "or", "of", "to", "in", "on", "for", "with", "by", "from", "at", "as",
		"is", "are", "be", "this", "that", "it", "its", "into", "via", "using", "use", "run", "make",
		"add", "fix", "update",
	} {
		m[w] = true
	}
	return m
}()

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// Tokens is the token list of one text, stopwords dropped, in order.
func Tokens(text string) []string {
	s := pystr.Lower(text)
	var out []string
	for i := 0; i < len(s); {
		if !isWordByte(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		if t := s[i:j]; !stopwords[t] {
			out = append(out, t)
		}
		i = j
	}
	return out
}

// Hash is int(blake2b(token, digest_size=8).hexdigest(), 16).
func Hash(token string) uint64 {
	return binary.BigEndian.Uint64(blake2b([]byte(token), 8))
}

// Vector is _lexical_vector: the unnormalized vector of one text. Tokens
// add into their buckets in the order Counter first saw them.
func Vector(text string) []float64 {
	vec := make([]float64, Dim)
	counts := map[string]int{}
	var order []string
	for _, t := range Tokens(text) {
		if counts[t] == 0 {
			order = append(order, t)
		}
		counts[t]++
	}
	for _, t := range order {
		h := Hash(t)
		sign := -1.0
		if h>>63 != 0 {
			sign = 1.0
		}
		vec[h%Dim] += sign * math.Sqrt(float64(counts[t]))
	}
	return vec
}

// Encode is _LexicalEmbedder.encode for one text: the vector, divided by
// its norm (at least 1e-12).
func Encode(text string) []float64 {
	v := Vector(text)
	var ss float64
	for _, x := range v {
		ss += x * x
	}
	n := math.Max(math.Sqrt(ss), 1e-12)
	for i := range v {
		v[i] /= n
	}
	return v
}

package pathways

import (
	"sort"
	"strconv"
)

// sparse is a stored embedding read for scoring: its length, its nonzero
// entries and the sum of their squares.
type sparse struct {
	n     int
	idx   []int32
	val   []float64
	sumsq float64
}

// scanEmbedding reads task_embedding_json into a sparse vector. It takes
// the text json.dumps writes for a list of floats in one pass, zeros
// cheaply; any other text goes through the general reader, which is
// json.loads followed by list[float]. ok is false when that fails.
func scanEmbedding(s string) (sp sparse, ok bool) {
	if sp, fast := scanFast(s); fast {
		return sp, true
	}
	vec, ok := parseEmbedding(s)
	if !ok {
		return sparse{}, false
	}
	return toSparse(vec), true
}

func toSparse(vec []float64) sparse {
	sp := sparse{n: len(vec)}
	for i, x := range vec {
		if x != 0 || x != x {
			sp.idx = append(sp.idx, int32(i))
			sp.val = append(sp.val, x)
		}
	}
	sp.sumsq = pairwiseSq(sp.idx, sp.val, 0, sp.n)
	return sp
}

// pairwiseSq is numpy's pairwise sum (add.reduce along a row) of the
// squares of a sparse vector's entries in [lo, lo+n): eight running sums
// in blocks of at most 128, halves above that. np.linalg.norm(c, axis=1)
// sums this way, and the order of the sums decides the last bit, which
// decides a near tie.
func pairwiseSq(idx []int32, val []float64, lo, n int) float64 {
	a := sort.Search(len(idx), func(i int) bool { return int(idx[i]) >= lo })
	b := sort.Search(len(idx), func(i int) bool { return int(idx[i]) >= lo+n })
	if a == b {
		return 0
	}
	switch {
	case n < 8:
		res := 0.0
		for k := a; k < b; k++ {
			res += val[k] * val[k]
		}
		return res
	case n <= 128:
		var r [8]float64
		main := lo + n - n%8
		k := a
		for ; k < b && int(idx[k]) < main; k++ {
			r[(int(idx[k])-lo)%8] += val[k] * val[k]
		}
		res := ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]))
		for ; k < b; k++ {
			res += val[k] * val[k]
		}
		return res
	}
	n2 := n / 2
	n2 -= n2 % 8
	return pairwiseSq(idx, val, lo, n2) + pairwiseSq(idx, val, lo+n2, n-n2)
}

func scanFast(s string) (sparse, bool) {
	n := len(s)
	var sp sparse
	if n < 2 || s[0] != '[' || s[n-1] != ']' {
		return sp, false
	}
	if n == 2 {
		return sp, true
	}
	i := 1
	k := 0
	for {
		// Runs of zeros, eight elements at a time.
		for i+40 <= n && s[i:i+40] == zeros8 {
			i += 40
			k += 8
		}
		// The common element: 0.0 followed by ", " or the closing bracket.
		if i+3 < n && s[i] == '0' && s[i+1] == '.' && s[i+2] == '0' && (s[i+3] == ',' || s[i+3] == ']') {
			i += 3
		} else {
			j := i
			for j < n-1 && s[j] != ',' {
				j++
			}
			tok := s[i:j]
			if tok == "-0.0" {
				i = j
			} else {
				if !floatToken(tok) {
					return sp, false
				}
				f, err := strconv.ParseFloat(tok, 64)
				if err != nil {
					return sp, false
				}
				if f != 0 || f != f {
					sp.idx = append(sp.idx, int32(k))
					sp.val = append(sp.val, f)
				}
				i = j
			}
		}
		k++
		if i == n-1 {
			sp.n = k
			sp.sumsq = pairwiseSq(sp.idx, sp.val, 0, sp.n)
			return sp, true
		}
		if s[i] != ',' || i+1 >= n || s[i+1] != ' ' {
			return sp, false
		}
		i += 2
	}
}

const zeros8 = "0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, "

// dot is the sparse vector against a dense one of the same length.
func (sp *sparse) dot(q []float64) float64 {
	var d float64
	for j, i := range sp.idx {
		d += sp.val[j] * q[i]
	}
	return d
}

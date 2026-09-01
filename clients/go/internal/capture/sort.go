package capture

import (
	"fmt"
	"math"
	"math/big"
	"sort"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

// less is Python's a < b for the values a captured_at can hold: numbers
// (bool is an int) and str compare; any other pair raises TypeError. Two
// lists compare item by item, which this binary does not model.
func less(a, b any) (bool, error) {
	if na, ok := numeric(a); ok {
		if nb, ok := numeric(b); ok {
			return numLess(na, nb), nil
		}
	}
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			// UTF-8 byte order is code point order.
			return sa < sb, nil
		}
	}
	_, la := a.([]any)
	_, lb := b.([]any)
	if la && lb {
		return false, fmt.Errorf("%w: two lists of captured_at to order", ErrUnreadable)
	}
	return false, &PyErr{"TypeError", fmt.Sprintf("'<' not supported between instances of '%s' and '%s'",
		pmodel.TypeName(a), pmodel.TypeName(b))}
}

// num is a JSON number: an exact value, or a float that has none (nan,
// inf).
type num struct {
	r   *big.Rat
	f   float64
	nan bool
}

func numeric(v any) (num, bool) {
	switch x := v.(type) {
	case pyjson.Float:
		f := float64(x)
		if math.IsNaN(f) {
			return num{nan: true}, true
		}
		if math.IsInf(f, 0) {
			return num{f: f}, true
		}
		return num{r: new(big.Rat).SetFloat64(f)}, true
	case float64:
		return numeric(pyjson.Float(x))
	case pyjson.Int, bool:
		r, ok := number(x)
		return num{r: r}, ok
	}
	return num{}, false
}

func numLess(a, b num) bool {
	switch {
	case a.nan || b.nan:
		return false
	case a.r == nil && b.r == nil:
		return a.f < b.f
	case a.r == nil:
		return a.f < 0 // -inf is below every finite value
	case b.r == nil:
		return b.f > 0
	}
	return a.r.Cmp(b.r) < 0
}

// sortDesc is list.sort(key=..., reverse=True) on keys: the indexes in
// their sorted order. CPython reverses the list, sorts it stably, and
// reverses it again. Below 64 items its sort is one run found by
// count_run and extended by binary insertion, which fixes where a key
// that compares false both ways (nan) lands; that is followed here
// exactly. From 64 items on, it merges runs; a nan there is refused,
// and without one any stable sort gives the same order.
func sortDesc(keys []any) ([]int, error) {
	n := len(keys)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = n - 1 - i
	}
	var failed error
	lt := func(i, j int) bool {
		if failed != nil {
			return false
		}
		r, err := less(keys[i], keys[j])
		if err != nil {
			failed = err
		}
		return r
	}
	if n < 64 {
		if n > 1 {
			// count_run
			run := 2
			if lt(idx[1], idx[0]) {
				for run < n && lt(idx[run], idx[run-1]) {
					run++
				}
				for a, b := 0, run-1; a < b; a, b = a+1, b-1 {
					idx[a], idx[b] = idx[b], idx[a]
				}
			} else {
				for run < n && !lt(idx[run], idx[run-1]) {
					run++
				}
			}
			// binarysort from the end of the run
			for start := run; start < n && failed == nil; start++ {
				pivot := idx[start]
				l, r := 0, start
				for l < r {
					p := l + (r-l)>>1
					if lt(pivot, idx[p]) {
						r = p
					} else {
						l = p + 1
					}
				}
				copy(idx[l+1:start+1], idx[l:start])
				idx[l] = pivot
			}
		}
	} else {
		for _, k := range keys {
			if nk, ok := numeric(k); ok && nk.nan {
				return nil, fmt.Errorf("%w: a captured_at of nan among %d sessions", ErrUnreadable, n)
			}
		}
		sort.SliceStable(idx, func(a, b int) bool { return lt(idx[a], idx[b]) })
	}
	if failed != nil {
		return nil, failed
	}
	for a, b := 0, n-1; a < b; a, b = a+1, b-1 {
		idx[a], idx[b] = idx[b], idx[a]
	}
	return idx, nil
}

package pyre

// extraCases is re._casefix._EXTRA_CASES: lowercase code points that
// match more than their own case pair.
var extraCases = map[rune][]rune{
	0x0069: {0x0131}, 0x0073: {0x017f}, 0x00b5: {0x03bc}, 0x0131: {0x0069}, 0x017f: {0x0073},
	0x0345: {0x03b9, 0x1fbe}, 0x0390: {0x1fd3}, 0x03b0: {0x1fe3}, 0x03b2: {0x03d0}, 0x03b5: {0x03f5},
	0x03b8: {0x03d1}, 0x03b9: {0x0345, 0x1fbe}, 0x03ba: {0x03f0}, 0x03bc: {0x00b5}, 0x03c0: {0x03d6},
	0x03c1: {0x03f1}, 0x03c2: {0x03c3}, 0x03c3: {0x03c2}, 0x03c6: {0x03d5}, 0x03d0: {0x03b2},
	0x03d1: {0x03b8}, 0x03d5: {0x03c6}, 0x03d6: {0x03c0}, 0x03f0: {0x03ba}, 0x03f1: {0x03c1},
	0x03f5: {0x03b5}, 0x0432: {0x1c80}, 0x0434: {0x1c81}, 0x043e: {0x1c82}, 0x0441: {0x1c83},
	0x0442: {0x1c84, 0x1c85}, 0x044a: {0x1c86}, 0x0463: {0x1c87}, 0x1c80: {0x0432}, 0x1c81: {0x0434},
	0x1c82: {0x043e}, 0x1c83: {0x0441}, 0x1c84: {0x0442, 0x1c85}, 0x1c85: {0x0442, 0x1c84},
	0x1c86: {0x044a}, 0x1c87: {0x0463}, 0x1c88: {0xa64b}, 0x1e61: {0x1e9b}, 0x1e9b: {0x1e61},
	0x1fbe: {0x0345, 0x03b9}, 0x1fd3: {0x0390}, 0x1fe3: {0x03b0}, 0xa64b: {0x1c88}, 0xfb05: {0xfb06},
	0xfb06: {0xfb05},
}

// caseSet is an IN item compiled under IGNORECASE, as _optimize_charset
// and the IN_UNI_IGNORE / IN_IGNORE ops have it.
type caseSet struct {
	negate   bool
	bmp      []uint64 // lowered members below 0x10000
	tail     []Item   // CATEGORY, non-BMP LITERAL, RANGE / RANGE_UNI_IGNORE
	uniRange []bool   // for each tail RANGE: RANGE_UNI_IGNORE
	hascased bool
	uni      bool
	flags    int
}

func newCaseSet(set []Item, flags int) *caseSet {
	cs := &caseSet{bmp: make([]uint64, 0x10000/64), uni: flags&FlagUnicode != 0, flags: flags}
	lower := asciiLower
	iscased := asciiIsCased
	if cs.uni {
		lower = uniLower
		iscased = uniIsCased
	}
	mark := func(r rune) bool {
		if r >= 0x10000 {
			return false
		}
		cs.bmp[r/64] |= 1 << (uint(r) % 64)
		return true
	}
	for _, it := range set {
		switch it.Op {
		case NEGATE:
			cs.negate = true
		case LITERAL:
			av := lower(rune(it.Lit))
			if !mark(av) {
				if !cs.hascased && iscased(av) {
					cs.hascased = true
				}
				cs.tail = append(cs.tail, Item{Op: LITERAL, Lit: int(av)})
				cs.uniRange = append(cs.uniRange, false)
				continue
			}
			if cs.uni {
				for _, k := range extraCases[av] {
					mark(k)
				}
			}
			if !cs.hascased && iscased(av) {
				cs.hascased = true
			}
		case RANGE:
			nonBMP := false
			for c := it.Lo; c <= it.Hi; c++ {
				l := lower(rune(c))
				if !mark(l) {
					nonBMP = true
					break
				}
				if cs.uni {
					for _, k := range extraCases[l] {
						mark(k)
					}
				}
			}
			if !cs.hascased {
				for c := it.Lo; c <= it.Hi && c < 0x110000; c++ {
					if iscased(rune(c)) {
						cs.hascased = true
						break
					}
				}
			}
			if nonBMP {
				cs.hascased = true
				cs.tail = append(cs.tail, it)
				cs.uniRange = append(cs.uniRange, cs.uni)
			}
		default:
			cs.tail = append(cs.tail, it)
			cs.uniRange = append(cs.uniRange, false)
		}
	}
	return cs
}

func (cs *caseSet) has(r rune) bool {
	x := r
	if cs.hascased {
		if cs.uni {
			x = uniLower(r)
		} else {
			x = asciiLower(r)
		}
	}
	return cs.in(x) != cs.negate
}

func (cs *caseSet) in(x rune) bool {
	if x < 0x10000 && cs.bmp[x/64]&(1<<(uint(x)%64)) != 0 {
		return true
	}
	for i, it := range cs.tail {
		switch it.Op {
		case LITERAL:
			if x == rune(it.Lit) {
				return true
			}
		case RANGE:
			if int(x) >= it.Lo && int(x) <= it.Hi {
				return true
			}
			if cs.uniRange[i] {
				u := uniUpper(x)
				if int(u) >= it.Lo && int(u) <= it.Hi {
					return true
				}
			}
		case CATEGORY:
			if category(it.Lit, x, cs.flags) {
				return true
			}
		}
	}
	return false
}

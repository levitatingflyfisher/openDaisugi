// Package foreman is the foreman's memory: one chat that never ends.
//
// Every message is logged word for word, append only, one JSONL file per
// day. A summary tree compresses the log: a node at level l holds one line
// of at most 512 bytes for the 2^l messages it covers. The view is the
// list of tree lines over the whole chat, fine near now and coarse in the
// past, kept between 64 and 128 KiB. It is saved to view.json after each
// change. A view.json that cannot be used is rebuilt by replay over the tree.
//
// The shape follows Victor Taelin's UniiChat recipe. The code is our own.
// This package is a library: it opens no socket and starts no server.
package foreman

import (
	"container/heap"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// NodeID names one tree node: level L covers the 2^L messages that start
// at message I*2^L. A message is the level-0 node with its own id.
type NodeID struct {
	L int
	I int64
}

// First is the id of the first message the node covers.
func (n NodeID) First() int64 { return n.I << n.L }

// Count is the number of messages the node covers.
func (n NodeID) Count() int64 { return int64(1) << n.L }

// Last is the id of the last message the node covers.
func (n NodeID) Last() int64 { return n.First() + n.Count() - 1 }

// Name is the node's public name, "id+n": the first message and the count.
func (n NodeID) Name() string { return fmt.Sprintf("%d+%d", n.First(), n.Count()) }

// Parent is the node one level up that holds this one.
func (n NodeID) Parent() NodeID { return NodeID{L: n.L + 1, I: n.I >> 1} }

// Children are the two nodes one level down. Only call it for L > 0.
func (n NodeID) Children() (NodeID, NodeID) {
	return NodeID{L: n.L - 1, I: n.I << 1}, NodeID{L: n.L - 1, I: n.I<<1 + 1}
}

// maxLevel keeps First and Count inside an int64.
const maxLevel = 62

// ParseName reads "id+n". n must be a power of two and id a multiple of n.
func ParseName(s string) (NodeID, error) {
	a, b, ok := strings.Cut(s, "+")
	if !ok {
		return NodeID{}, fmt.Errorf("node name %q is not id+n", s)
	}
	id, err1 := strconv.ParseInt(a, 10, 64)
	n, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil {
		return NodeID{}, fmt.Errorf("node name %q is not id+n", s)
	}
	return NodeOf(id, n)
}

// NodeOf is the node that covers the n messages from id.
func NodeOf(id, n int64) (NodeID, error) {
	if id < 0 || n <= 0 || n&(n-1) != 0 {
		return NodeID{}, fmt.Errorf("node %d+%d: n must be a power of two and id at least 0", id, n)
	}
	l := 0
	for int64(1)<<l < n {
		l++
	}
	if l > maxLevel || id%n != 0 {
		return NodeID{}, fmt.Errorf("node %d+%d: id must be a multiple of n", id, n)
	}
	return NodeID{L: l, I: id / n}, nil
}

// siblings reports whether a and b are the two halves of one parent, in
// order.
func siblings(a, b NodeID) bool {
	return a.L == b.L && a.I%2 == 0 && b.I == a.I+1
}

// dueLess reports whether a/2^la < b/2^lb, exactly, with no overflow.
func dueLess(a int64, la int, b int64, lb int) bool { return dueCmp(a, la, b, lb) < 0 }

// dueCmp compares a/2^la with b/2^lb for a, b >= 0 and returns -1, 0 or 1.
// It shifts the smaller-level side's peer down rather than shifting up, so
// it never overflows.
func dueCmp(a int64, la int, b int64, lb int) int {
	if la > lb {
		return -dueCmp(b, lb, a, la)
	}
	// Compare a*2^d with b, d = lb - la.
	d := lb - la
	if d >= 63 {
		switch {
		case a > 0:
			return 1 // a*2^63 is above every int64
		case b > 0:
			return -1
		}
		return 0
	}
	hi := b >> d
	switch {
	case a < hi:
		return -1
	case a > hi:
		return 1
	case b&(int64(1)<<d-1) != 0:
		return -1 // a*2^d == hi*2^d < b
	}
	return 0
}

// MostDue picks the pair of view lines to merge next, by index of the
// pair's left line, or -1 when no pair can merge.
//
// For two sibling lines at level l, the pair is due by (T - last) / 2^l,
// where T is the number of messages in the chat and last is the id of the
// pair's last message. The most due pair whose parent is built merges
// first; among equals, the oldest wins. Measuring from the pair's first
// message instead gives other, wrong merges. With T taken as the newest
// id rather than the count, the order also goes wrong.
func MostDue(view []NodeID, total int64, built func(NodeID) bool) int {
	best, bestNum, bestL := -1, int64(0), 0
	for k := 0; k+1 < len(view); k++ {
		a, b := view[k], view[k+1]
		if !siblings(a, b) || !built(a.Parent()) {
			continue
		}
		num := total - b.Last()
		if best < 0 || dueLess(bestNum, bestL, num, a.L) {
			best, bestNum, bestL = k, num, a.L
		}
	}
	return best
}

// mergeAt replaces lines k and k+1 with their parent.
func mergeAt(view []NodeID, k int) []NodeID {
	view[k] = view[k].Parent()
	return append(view[:k+1], view[k+2:]...)
}

// errNotNode is returned for a name that is not a node of this chat.
var errNotNode = errors.New("no such node")

// planner does the merges of one batch. It picks the same pair MostDue
// picks, one merge at a time, but keeps the candidate pairs in a heap, so a
// batch of m merges over n lines costs O((n + m) log n) and not O(n * m).
//
// The lines sit in slots in view order. A merge keeps the left slot (now the
// parent) and kills the right one, so slot order stays view order and the
// "oldest wins" tie is the lower slot. A heap entry names the pair's two
// slots and their generations; a merge bumps a generation, which makes
// every older entry that touches the slot stale. Stale entries are dropped
// when they come to the top.
type planner struct {
	ids   []NodeID
	next  []int
	prev  []int
	alive []bool
	gen   []int
	total int64
	built func(NodeID) bool
	h     pairHeap
}

// plannerMerges counts the merges the planner made. A test checks that the
// view's batch merge goes through the planner.
var plannerMerges atomic.Int64

type pairEntry struct {
	left, right int
	lgen, rgen  int
	num         int64 // total - last message of the pair
	level       int
}

type pairHeap []pairEntry

func (h pairHeap) Len() int { return len(h) }
func (h pairHeap) Less(a, b int) bool {
	// The most due pair is first; among equals the lower slot.
	if c := dueCmp(h[a].num, h[a].level, h[b].num, h[b].level); c != 0 {
		return c > 0
	}
	return h[a].left < h[b].left
}
func (h pairHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *pairHeap) Push(x any)   { *h = append(*h, x.(pairEntry)) }
func (h *pairHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

func newPlanner(view []NodeID, total int64, built func(NodeID) bool) *planner {
	n := len(view)
	p := &planner{ids: append([]NodeID(nil), view...), next: make([]int, n), prev: make([]int, n),
		alive: make([]bool, n), gen: make([]int, n), total: total, built: built}
	for k := range view {
		p.next[k], p.prev[k], p.alive[k] = k+1, k-1, true
	}
	for k := 0; k+1 < n; k++ {
		p.consider(k)
	}
	heap.Init(&p.h)
	return p
}

// consider adds the pair that starts at slot k to the heap, when it can merge.
func (p *planner) consider(k int) {
	if k < 0 || k >= len(p.ids) || !p.alive[k] {
		return
	}
	r := p.next[k]
	if r >= len(p.ids) {
		return
	}
	a, b := p.ids[k], p.ids[r]
	if !siblings(a, b) || !p.built(a.Parent()) {
		return
	}
	p.h = append(p.h, pairEntry{left: k, right: r, lgen: p.gen[k], rgen: p.gen[r], num: p.total - b.Last(), level: a.L})
}

// pop returns the left slot of the most due pair, or false when no pair can
// merge.
func (p *planner) pop() (int, bool) {
	for p.h.Len() > 0 {
		e := heap.Pop(&p.h).(pairEntry)
		if p.alive[e.left] && p.alive[e.right] && p.gen[e.left] == e.lgen && p.gen[e.right] == e.rgen && p.next[e.left] == e.right {
			return e.left, true
		}
	}
	return 0, false
}

// merge joins slot k with the slot after it and returns the parent.
func (p *planner) merge(k int) NodeID {
	plannerMerges.Add(1)
	r := p.next[k]
	p.ids[k] = p.ids[k].Parent()
	p.gen[k]++
	p.alive[r] = false
	p.next[k] = p.next[r]
	if p.next[k] < len(p.ids) {
		p.prev[p.next[k]] = k
	}
	if pv := p.prev[k]; pv >= 0 {
		p.push(pv)
	}
	p.push(k)
	return p.ids[k]
}

func (p *planner) push(k int) {
	before := len(p.h)
	p.consider(k)
	if len(p.h) > before {
		last := len(p.h) - 1
		heap.Fix(&p.h, last)
	}
}

// lines are the view lines after the merges, in order.
func (p *planner) lines() []NodeID {
	out := make([]NodeID, 0, len(p.ids))
	for k, id := range p.ids {
		if p.alive[k] {
			out = append(out, id)
		}
	}
	return out
}

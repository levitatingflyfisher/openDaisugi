// Package foreman is the foreman's memory: one chat that never ends.
//
// Every message is logged word for word, append only, one JSONL file per
// day. A summary tree compresses the log: a node at level l holds one line
// of at most 512 bytes for the 2^l messages it covers. The view is the
// list of tree lines over the whole chat, fine near now and coarse in the
// past, kept between 64 and 128 KiB. It is saved to view.json after each
// change and never rebuilt from the log.
//
// The shape follows Victor Taelin's UniiChat recipe. The code is our own.
// This package is a library: it opens no socket and starts no server.
package foreman

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
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

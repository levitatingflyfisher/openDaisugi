package pando

import (
	"fmt"
	"math"
	"os"
	"sort"

	"daisugi-verify/internal/embed/lexical"
)

// RecallHit is one answer of recall: a node id (first+count), a score and
// one line of text. It is never a whole raw message: the model zooms next.
type RecallHit struct {
	ID    string
	Score float64
	Line  string
}

// RecallLineBytes caps the text of one hit.
const RecallLineBytes = 160

// RecallMax is the most hits one recall gives.
const RecallMax = 32

func cosine(a, b []float64, na, nb float64) float64 {
	if na == 0 || nb == 0 {
		return 0
	}
	var d float64
	for i := range a {
		d += a[i] * b[i]
	}
	return d / (na * nb)
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

// Recall ranks the view's summary lines and the raw messages of the chat
// by the lexical matcher (PD-R-5) and gives the best k. A hit with a score
// of 0 is left out. Ties go by id: the older first. A message that is its
// own level-0 view line counts once, as the message.
func (c *Chat) Recall(query string, k int) []RecallHit {
	all := c.recallCands(query)
	if k <= 0 {
		k = 8
	}
	if k > RecallMax {
		k = RecallMax
	}
	if len(all) > k {
		all = all[:k]
	}
	out := make([]RecallHit, len(all))
	for i, a := range all {
		out[i] = a.hit
	}
	return out
}

type recallCand struct {
	branch       string
	first, count int64
	hit          RecallHit
}

// recallCands is every hit of the chat, best first.
func (c *Chat) recallCands(query string) []recallCand {
	q := lexical.Vector(query)
	nq := norm(q)
	var all []recallCand
	add := func(first, count int64, id, text string) {
		v := lexical.Vector(text)
		s := cosine(q, v, nq, norm(v))
		if s <= 0 {
			return
		}
		line := flat(text)
		if len(line) > RecallLineBytes {
			line = clip(line, RecallLineBytes)
		}
		all = append(all, recallCand{"", first, count, RecallHit{ID: id, Score: s, Line: line}})
	}
	for _, id := range c.view.Lines {
		if id.L == 0 {
			continue
		}
		if n, ok := c.nodes[id]; ok {
			add(id.First(), id.Count(), id.Name(), n.Text)
		}
	}
	for i := int64(0); i < c.Count(); i++ {
		m, err := c.Message(i)
		if err != nil {
			continue
		}
		add(i, 1, NodeID{L: 0, I: i}.Name(), m.Text)
	}
	sortCands(all)
	return all
}

// sortCands orders hits: the best score first, then the store itself before
// its branches (by branch path), then the older id, then the smaller node.
func sortCands(all []recallCand) {
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].hit.Score != all[b].hit.Score {
			return all[a].hit.Score > all[b].hit.Score
		}
		if all[a].branch != all[b].branch {
			return all[a].branch < all[b].branch
		}
		if all[a].first != all[b].first {
			return all[a].first < all[b].first
		}
		return all[a].count < all[b].count
	})
}

// RecallTree is Recall over the store (or branch) in dir and every branch
// below it, open or closed, and never above it. A hit in a branch has the
// id "<branch>:<first>+<count>", the branch named from dir: zoom it with
// that branch. A branch that cannot be read is left out.
func RecallTree(dir, query string, k int) ([]RecallHit, error) {
	if k <= 0 {
		k = 8
	}
	if k > RecallMax {
		k = RecallMax
	}
	var all []recallCand
	if hasMain(dir) {
		top, err := OpenReadOnly(dir)
		if err != nil {
			return nil, err
		}
		all = top.recallCands(query)
	} else if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	for _, b := range BranchTree(dir) {
		d, err := ExistingBranch(dir, b)
		if err != nil || !hasMain(d) {
			continue
		}
		c, err := OpenReadOnly(d)
		if err != nil {
			continue
		}
		for _, h := range c.recallCands(query) {
			h.branch = b
			h.hit.ID = b + ":" + h.hit.ID
			all = append(all, h)
		}
	}
	sortCands(all)
	if len(all) > k {
		all = all[:k]
	}
	out := make([]RecallHit, len(all))
	for i, a := range all {
		out[i] = a.hit
	}
	return out, nil
}

// FormatHit is "id<TAB>score<TAB>line", the score to three places.
func FormatHit(h RecallHit) string {
	return fmt.Sprintf("%s\t%.3f\t%s", h.ID, h.Score, h.Line)
}

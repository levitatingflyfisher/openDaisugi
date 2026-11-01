package pando

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// ClipStopAfter is how many model calls at the start of a run may all fall
// back to a clipped node before the run stops. A model that returns no text
// for the first few nodes will do so for all of them.
const ClipStopAfter = 5

// ErrAllClipped ends a run whose first ClipStopAfter model calls all gave no
// line. The nodes are written as clipped copies of the source; redo them
// with tend --redo-clipped once the model is fixed.
var ErrAllClipped = fmt.Errorf("the model returned no text for the first %d nodes; "+
	"if it is a thinking model, raise max_tokens in its profile or use a non-thinking model", ClipStopAfter)

// isClipped says whether a node's text is a clipped fallback: the marker and
// the start of the source (FM-R-64).
func isClipped(text string) bool { return strings.HasPrefix(text, ClipMarker) }

// tendRun is what a tend run did, over the store and its children: the nodes
// it wrote, the model calls it made, and how many of those gave a clipped
// fallback. With stopOnClips, a run whose first ClipStopAfter calls all
// clipped stops with ErrAllClipped.
type tendRun struct {
	stopOnClips           bool
	built, calls, clipped int
	// spent is the model calls the run made, every re-ask included, when the
	// summarizer counts them; else one for each job. --max caps it.
	spent int
	// progress, when set, is told of each node written: its id, the seconds
	// it took and the model calls spent so far.
	progress func(id NodeID, took time.Duration, spent int)
	model    string   // the profile of a redo run, for its marker
	locked   []string // child stores a redo run skipped because they were locked
}

// Tend builds up to max pending nodes of c with s, one call at a time, in
// queue order. max 0 means all. It stops at the first failure of a model
// call: the node waits by the retry schedule and the error comes back. built
// is how many nodes were written, with or without the model. The caller
// holds the store (Open) and no other lock is taken here.
func Tend(ctx context.Context, c *Chat, s Summarizer, max int) (built int, err error) {
	run := &tendRun{}
	err = tendCalls(ctx, c, s, max, run)
	return run.built, err
}

// tendCalls is Tend that also counts, in run, the model calls it made and
// how many fell back to a clipped node. max caps the calls of this store: a
// node the store can build without a model does not count.
func tendCalls(ctx context.Context, c *Chat, s Summarizer, max int, run *tendRun) (err error) {
	before := c.Pending()
	start := run.spent
	defer func() { run.built += before - c.Pending() }()
	for {
		if max > 0 && run.spent-start >= max {
			break
		}
		c.prioritize()
		job, ok, nerr := c.Next()
		if nerr != nil {
			err = nerr
			break
		}
		if !ok {
			break
		}
		if cerr := ctx.Err(); cerr != nil {
			err = cerr
			break
		}
		t0 := time.Now()
		text, rerr := run.job(ctx, s, job)
		if rerr != nil {
			if ctx.Err() == nil {
				c.FailErr(job, rerr)
			}
			err = rerr
			break
		}
		run.calls++
		if isClipped(text) {
			run.clipped++
		}
		if cerr := c.Commit(job, text); cerr != nil {
			err = cerr
			break
		}
		run.report(job.ID, time.Since(t0))
		if run.stopOnClips && run.calls == ClipStopAfter && run.clipped == run.calls {
			err = ErrAllClipped
			break
		}
	}
	return err
}

// ClippedCount is how many written nodes hold a clipped copy of their source.
func (c *Chat) ClippedCount() int {
	n := 0
	for _, nd := range c.nodes {
		if isClipped(nd.Text) {
			n++
		}
	}
	return n
}

// PendingIDs names the pending nodes in build order.
func (c *Chat) PendingIDs() []NodeID { return append([]NodeID(nil), c.pending...) }

// job runs one job and adds the model calls it made to run.spent. A
// summarizer that counts its calls (CallsMade) is believed; any other counts
// one call for each job.
func (run *tendRun) job(ctx context.Context, s Summarizer, j Job) (string, error) {
	cs, counts := s.(interface{ CallsMade() int })
	before := 0
	if counts {
		before = cs.CallsMade()
	}
	text, err := runJob(ctx, s, j)
	if counts {
		run.spent += cs.CallsMade() - before
	} else {
		run.spent++
	}
	return text, err
}

// report tells run.progress that node id was written.
func (run *tendRun) report(id NodeID, took time.Duration) {
	if run.progress != nil {
		run.progress(id, took, run.spent)
	}
}

// progressTo is the progress function that writes one line for each node to w.
func progressTo(w io.Writer) func(NodeID, time.Duration, int) {
	return func(id NodeID, took time.Duration, spent int) {
		fmt.Fprintf(w, "built level %d index %d in %.1fs (%d model calls so far)\n", id.L, id.I, took.Seconds(), spent)
	}
}

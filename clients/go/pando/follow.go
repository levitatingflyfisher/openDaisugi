package pando

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Round 7: the store is live. `daisugi pando follow` is one process that
// watches every source (the Claude Code transcripts, the Hermes profiles),
// logs each new message when it is said, and builds the summary nodes soon
// after in the background.

// Source is a place messages come from. Poll logs the messages said since
// the time given into c and returns how many were new. It must be safe to
// call again on the same messages: the act id keeps a message once.
type Source interface {
	Name() string
	Poll(ctx context.Context, c *Chat, since time.Time) (int, error)
}

// Stateful is a source that keeps a cursor for each of its parts (a Hermes
// profile: the id of the last message it logged). Follow keeps the cursors
// in follow.json, so a source with a part that fails stalls that part only.
type Stateful interface {
	Cursors() map[string]int64
	SetCursors(map[string]int64)
}

// FollowFile is the state file of follow, in the store dir.
const FollowFile = "follow.json"

// srcState is what follow keeps for one source. Since is the start of the
// last cycle in which the whole source was read without an error: a failing
// source does not move the time of the others.
type srcState struct {
	Since   string           `json:"since,omitempty"`
	Cursors map[string]int64 `json:"cursors,omitempty"`
}

type followState struct {
	Sources map[string]srcState `json:"sources"`
}

func readFollowState(dir string) followState {
	st := followState{Sources: map[string]srcState{}}
	if b, err := os.ReadFile(filepath.Join(dir, FollowFile)); err == nil {
		var in followState
		if json.Unmarshal(b, &in) == nil && in.Sources != nil {
			st = in
		}
	}
	return st
}

func writeFollowState(dir string, st followState) error {
	b, _ := json.Marshal(st)
	tmp := filepath.Join(dir, FollowFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, FollowFile))
}

// ClaudeSource polls the Claude Code transcripts under Root
// (~/.claude/projects): the .jsonl files of each project dir whose size or
// time changed since the last poll.
type ClaudeSource struct {
	Root string
	Skip []string
	seen map[string][2]int64
}

// Name implements Source.
func (s *ClaudeSource) Name() string { return "claude-code" }

// Poll implements Source.
func (s *ClaudeSource) Poll(ctx context.Context, c *Chat, since time.Time) (int, error) {
	if s.seen == nil {
		s.seen = map[string][2]int64{}
	}
	total := 0
	var firstErr error
	err := filepath.WalkDir(s.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == s.Root {
				return err
			}
			return nil
		}
		if d.IsDir() {
			// Project dirs are one level down; subagent dirs are not chats.
			if rel, _ := filepath.Rel(s.Root, p); strings.Count(rel, string(filepath.Separator)) >= 1 {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".jsonl") || ctx.Err() != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		sig := [2]int64{info.ModTime().UnixNano(), info.Size()}
		if s.seen[p] == sig {
			return nil
		}
		n, err := c.IngestClaude(ctx, p, s.Skip)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", p, err)
			}
			return nil
		}
		s.seen[p] = sig
		total += n
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = firstErr
	}
	return total, err
}

// FollowOpts says what a follow does.
type FollowOpts struct {
	Dir     string
	Sources []Source
	// Interval is the wait between polls. It defaults to 3 seconds.
	Interval time.Duration
	// Summarizer, when set, builds the pending nodes, one model call at a
	// time. nil means nodes wait for a profile.
	Summarizer Summarizer
	// Overlap is how far back each poll looks past the last one, so a
	// message written during a poll is not missed. It defaults to 2 minutes.
	Overlap time.Duration
	Now     func() time.Time
	Log     io.Writer
	// LockWait is how long a poll waits for another writer. It defaults to
	// 20 seconds.
	LockWait time.Duration

	noModel bool // said once
	backoff time.Duration
	retryAt time.Time
}

func (o *FollowOpts) fill() {
	if o.Interval <= 0 {
		o.Interval = 3 * time.Second
	}
	if o.Overlap <= 0 {
		o.Overlap = 2 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.LockWait <= 0 {
		o.LockWait = 20 * time.Second
	}
}

// OpenWait opens the store for writing, and waits up to wait for another
// writer to let go.
func OpenWait(ctx context.Context, cfg Config, wait time.Duration) (*Chat, error) {
	deadline := time.Now().Add(wait)
	for {
		c, err := Open(ctx, cfg)
		if !errors.Is(err, ErrLocked) || !time.Now().Before(deadline) {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FollowOnce is one cycle: poll every source, then build at most one node.
// built says whether a node was written; more says nodes are still due.
func FollowOnce(ctx context.Context, o *FollowOpts) (added int, built, more bool, err error) {
	o.fill()
	start := o.Now()
	state := readFollowState(o.Dir)
	c, err := OpenWait(ctx, Config{Dir: o.Dir, Now: o.Now}, o.LockWait)
	if err != nil {
		return 0, false, false, err
	}
	defer c.Close()
	for _, s := range o.Sources {
		ss := state.Sources[s.Name()]
		since, err := time.Parse(DateLayout, ss.Since)
		if err != nil {
			since = start // the first run starts now; history is the importer's
		}
		st, stateful := s.(Stateful)
		if stateful {
			st.SetCursors(ss.Cursors)
		}
		n, perr := s.Poll(ctx, c, since.Add(-o.Overlap))
		added += n
		if stateful {
			ss.Cursors = st.Cursors()
		}
		if perr != nil {
			if ss.Since == "" {
				ss.Since = since.UTC().Format(DateLayout) // a failing source keeps its own time
			}
			fmt.Fprintf(o.Log, "follow: %s: %v\n", s.Name(), clipErr(perr))
		} else {
			ss.Since = start.UTC().Format(DateLayout)
		}
		state.Sources[s.Name()] = ss
	}
	if werr := writeFollowState(o.Dir, state); werr != nil {
		fmt.Fprintf(o.Log, "follow: %v\n", werr)
	}
	// The store's own nodes first, then those of its branches (and their
	// workers), one model call per cycle either way.
	target := c
	if c.Pending() == 0 {
		sub, err := pendingSub(ctx, o.Dir, o.Now)
		if err != nil || sub == nil {
			return added, false, false, nil
		}
		defer sub.Close()
		target = sub
	}
	if o.Summarizer == nil {
		if !o.noModel {
			o.noModel = true
			fmt.Fprintln(o.Log, "follow: summary nodes wait for a profile: set pando.profile or pass --profile")
		}
		return added, false, false, nil
	}
	if o.Now().Before(o.retryAt) {
		return added, false, true, nil
	}
	before := target.Pending()
	run := &tendRun{}
	terr := tendCalls(ctx, target, o.Summarizer, 1, run)
	if terr != nil && ctx.Err() == nil {
		// An outage delays the node; it never clips it. Wait longer each time.
		o.backoff = min(max(2*o.backoff, 15*time.Second), 5*time.Minute)
		o.retryAt = o.Now().Add(o.backoff)
		fmt.Fprintf(o.Log, "follow: the model failed, trying again in %s: %v\n", o.backoff, clipErr(terr))
		return added, false, true, nil
	}
	o.backoff, o.retryAt = 0, time.Time{}
	more = target.Pending() > 0
	if target != c && !more {
		more = anyPendingSub(o.Dir)
	}
	return added, target.Pending() < before || run.built > 0, more, nil
}

// pendingSub opens for writing the first store below dir (a branch or a
// worker's child store) with nodes to build. A store that another process
// holds is passed over. nil when none waits.
func pendingSub(ctx context.Context, dir string, now func() time.Time) (*Chat, error) {
	for _, ss := range SubStores(dir) {
		ro, err := OpenReadOnly(ss.Dir)
		if err != nil || ro.Pending() == 0 {
			continue
		}
		c, err := Open(ctx, Config{Dir: ss.Dir, Now: now})
		if errors.Is(err, ErrLocked) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return nil, nil
}

// anyPendingSub says whether a store below dir has nodes to build.
func anyPendingSub(dir string) bool {
	for _, ss := range SubStores(dir) {
		if ro, err := OpenReadOnly(ss.Dir); err == nil && ro.Pending() > 0 {
			return true
		}
	}
	return false
}

// Follow runs FollowOnce until ctx ends. It goes straight on while nodes are
// due and waits Interval otherwise.
func Follow(ctx context.Context, o *FollowOpts) error {
	o.fill()
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return err
	}
	for {
		_, _, more, err := FollowOnce(ctx, o)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(o.Log, "follow: %v\n", err)
			more = false
		}
		wait := o.Interval
		if more && o.Now().After(o.retryAt) {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// clipErr is an error text clipped to one short line. A provider may echo
// the prompt in an error body, and the prompt is store text: the journal
// must not carry it.
func clipErr(err error) string {
	return clip(flat(err.Error()), 200)
}

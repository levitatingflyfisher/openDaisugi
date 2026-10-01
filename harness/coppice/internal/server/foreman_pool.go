package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opendaisugi/coppice/internal/config"
	"github.com/opendaisugi/coppice/internal/foreman"
)

// Workers go where there is room (FM-R-93 to FM-R-101). A worker profile may
// name a pool, from pools.toml beside coppice.toml: several (machine, base
// URL) entries that serve ONE model. A spawn takes the first entry with a
// free slot, else the job waits in a queue the owner can read. An entry
// that does not answer cools down with a back-off and is tried again when
// it ends. A quota moves the job to another entry of the same pool, which
// serves the same model; when every entry is out the job is requeued and
// the foreman says so in one line. Nothing here moves a job to another
// model. A background job waits for an entry that has had no foreground job
// for idle_after, and is paused (its process group stopped) when foreground
// work needs the room. The state is files and memory of the server: no new
// daemon, and no network code beyond the base URL the worker already uses.

// heldJob is a job that is not running: it waits, or it is paused.
type heldJob struct {
	foreman.HeldJob
	rec *workerRec
	fc  *foremanChat
}

// poolRuntime is the pool state of the server. workMu guards all of it.
type poolRuntime struct {
	health map[string]*foreman.Health
	lastFg map[string]time.Time
	held   []*heldJob
	pids   map[string]int
	// frozen holds the process groups a yield stopped, by label; resumed is
	// when a paused job last went on (FM-R-107).
	frozen  map[string][]int
	resumed map[string]time.Time
	// noYield holds the jobs that went on in place because their pool or
	// entry left the pools file; no yield pauses them again (FM-R-108).
	noYield map[string]bool
	timer   *time.Timer
	fc      *foremanChat
}

// poolLine is one line for the foreman's chat, logged after the lock is off.
type poolLine struct {
	fc   *foremanChat
	text string
}

func (s *Server) poolsPath() string {
	return filepath.Join(filepath.Dir(config.Path()), foreman.PoolsFile)
}

// poolFor is the pool the profile names, or nil. The default profile is
// never a pool. A pools file that does not read is moved aside and the
// foreman goes on with no pools (FM-R-112), so a profile that names no pool
// never fails because of it.
func (s *Server) poolFor(fc *foremanChat, profile string) (*foreman.Pool, error) {
	if profile == config.DefaultCompactorProfile {
		return nil, nil
	}
	set, note := s.loadPoolSet()
	if note != "" {
		s.logWork(fc, note)
	}
	return set.Pools[profile], nil
}

// loadPoolSet reads the pools file. When it does not read it is moved to
// pools.toml.broken-<time>, never deleted, and the result is no pools; note
// is the one line that says where the file went.
func (s *Server) loadPoolSet() (set *foreman.PoolSet, note string) {
	set, err := foreman.LoadPools(s.poolsPath())
	if err == nil {
		return set, ""
	}
	_ = err
	empty := &foreman.PoolSet{IdleAfter: foreman.DefaultIdleAfter, Pools: map[string]*foreman.Pool{}}
	to, merr := foreman.MoveAside(s.poolsPath(), s.clock())
	if merr != nil {
		// The file cannot be moved: it stays, and the foreman runs with no
		// pools until it can be read.
		return empty, foreman.PoolsFile + " did not read and could not be moved aside, so no pools are used."
	}
	return empty, foreman.PoolsFile + " did not read. It was moved to " + filepath.Base(to) + " and the foreman goes on with no pools."
}

func entryKey(pool, entry string) string { return pool + "/" + entry }

// patchBundleURL sets the base URL of one profile in the profile bundle.
// The key is copied with it and never leaves the bytes.
func patchBundleURL(bundle []byte, profile, baseURL string) ([]byte, error) {
	var doc map[string]map[string]any
	if err := json.Unmarshal(bundle, &doc); err != nil {
		return nil, errors.New("the agent profiles do not read")
	}
	e, ok := doc[profile]
	if !ok {
		return nil, fmt.Errorf("the profile %s is not in the agent profiles", profile)
	}
	e["base_url"] = baseURL
	return json.Marshal(doc)
}

// poolStarted records the pid of a worker's process group.
func (s *Server) poolStarted(label string, pid int) {
	s.workMu.Lock()
	if s.pool.pids == nil {
		s.pool.pids = map[string]int{}
	}
	s.pool.pids[label] = pid
	// A foreground job may wait for the room this worker holds, and could
	// not pause it before its process group was known.
	var lines []poolLine
	if fc := s.pool.fc; fc != nil && len(s.pool.held) > 0 {
		lines = s.poolPumpLocked(fc, "")
	}
	s.workMu.Unlock()
	s.flushLines(lines)
}

// poolForget drops the pid of a run that ended. A run that replaced it under
// the same label (a quota restart) keeps its own pid: the entry goes only
// when it still holds this run's pid.
func (s *Server) poolForget(label string, pid int) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.pool.pids[label] != pid {
		return
	}
	delete(s.pool.pids, label)
	delete(s.pool.frozen, label)
	delete(s.pool.resumed, label)
	delete(s.pool.noYield, label)
}

func (s *Server) flushLines(lines []poolLine) {
	for _, l := range lines {
		s.logWork(l.fc, l.text)
	}
}

// spawnPooled records a worker for a pool, puts it in the queue and runs the
// queue once. The answer says where it started or why it waits.
func (s *Server) spawnPooled(fc *foremanChat, dir, brief string, pool *foreman.Pool, background bool) (string, error) {
	name := filepath.Base(dir)
	s.workMu.Lock()
	if !s.workLoad {
		s.workMu.Unlock()
		s.loadWorkers(fc)
		s.workMu.Lock()
	}
	if s.isClosed() {
		s.workMu.Unlock()
		return "", errors.New("the server is closing")
	}
	s.workNext[name]++
	now := s.clock().UTC().Format(foreman.DateLayout)
	w := &workerRec{
		Label: name + "-" + strconv.Itoa(s.workNext[name]), Project: name, Dir: dir, Profile: pool.Name, Model: pool.Model,
		Started: now, State: workQueued, Pool: pool.Name, Background: background,
	}
	s.work = append(s.work, w)
	s.pool.held = append(s.pool.held, &heldJob{rec: w, fc: fc, HeldJob: foreman.HeldJob{
		Label: w.Label, Project: name, Pool: pool.Name, Model: pool.Model, Brief: brief, Background: background, Added: now,
	}})
	lines := s.poolPumpLocked(fc, w.Label)
	s.saveWorkersLocked(fc)
	var res string
	if w.State == workRunning {
		res = "started " + w.Label + " on " + w.Entry
	} else {
		res = "queued " + w.Label + ": " + s.waitReasonLocked(pool, background) + " (" + strconv.Itoa(len(s.pool.held)) + " waiting)"
	}
	s.workMu.Unlock()
	s.flushLines(lines)
	return res, nil
}

func (s *Server) waitReasonLocked(pool *foreman.Pool, background bool) string {
	if background {
		return "a background job waits for an entry of the pool " + pool.Name + " that has been idle"
	}
	return "every entry of the pool " + pool.Name + " is busy or not answering"
}

// loadsLocked is the load of each entry of pool.
func (s *Server) loadsLocked(pool *foreman.Pool) map[string]foreman.Load {
	loads := map[string]foreman.Load{}
	for _, e := range pool.Entries {
		l := foreman.Load{Slots: e.Slots, LastFg: s.pool.lastFg[entryKey(pool.Name, e.Name)]}
		if h := s.pool.health[entryKey(pool.Name, e.Name)]; h != nil {
			l.Until = h.Until
		}
		loads[e.Name] = l
	}
	for _, w := range s.work {
		if w.Pool != pool.Name || w.State != workRunning {
			continue
		}
		if l, ok := loads[w.Entry]; ok {
			l.Running++
			if !w.Background {
				l.FgRunning++
			}
			loads[w.Entry] = l
		}
	}
	return loads
}

// poolEntry is the entry of the pool; a nil pool has none.
func poolEntry(pool *foreman.Pool, name string) (foreman.PoolEntry, bool) {
	if pool == nil {
		return foreman.PoolEntry{}, false
	}
	return entryOf(pool, name)
}

func entryOf(pool *foreman.Pool, name string) (foreman.PoolEntry, bool) {
	for _, e := range pool.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return foreman.PoolEntry{}, false
}

// poolPump runs the queue once. Tests and the timer call it.
func (s *Server) poolPump(fc *foremanChat) {
	s.workMu.Lock()
	lines := s.poolPumpLocked(fc, "")
	s.workMu.Unlock()
	s.flushLines(lines)
}

// poolPumpLocked places what can be placed, in this order: foreground jobs
// (pausing a background worker for one that has no room), then the
// background jobs. skip names a label whose own start the caller reports.
func (s *Server) poolPumpLocked(fc *foremanChat, skip string) []poolLine {
	pr := &s.pool
	if pr.health == nil {
		pr.health = map[string]*foreman.Health{}
		pr.lastFg = map[string]time.Time{}
	}
	pr.fc = fc
	if s.isClosed() {
		return nil
	}
	set, note := s.loadPoolSet()
	now := s.clock()
	var lines []poolLine
	if note != "" {
		lines = append(lines, poolLine{fc, note})
	}
	say := func(h *heldJob, text string) { lines = append(lines, poolLine{h.fc, text}) }
	drop := func(i int) { pr.held = append(pr.held[:i], pr.held[i+1:]...) }
	for _, bg := range []bool{false, true} {
		for i := 0; i < len(pr.held); {
			h := pr.held[i]
			if h.Background != bg {
				i++
				continue
			}
			pool := set.Pools[h.Pool]
			if h.rec.State == workPaused {
				// A paused job has a stopped process. When its pool or its
				// entry has gone from the pools file it can never be
				// picked again: it goes on and runs to its end (FM-R-108).
				if _, there := poolEntry(pool, h.rec.Entry); !there {
					s.thawLocked(h.Label)
					h.rec.State = workRunning
					if s.pool.noYield == nil {
						s.pool.noYield = map[string]bool{}
					}
					s.pool.noYield[h.Label] = true
					say(h, "["+h.Label+"] resumed on "+h.rec.Entry+": it is no longer in "+foreman.PoolsFile+", so it runs on and is not paused again")
					drop(i)
					continue
				}
			}
			if pool == nil {
				h.rec.State, h.rec.Entry = workFailed, ""
				say(h, "["+h.Label+"] failed: the pool "+h.Pool+" is not in "+foreman.PoolsFile+" any more")
				drop(i)
				continue
			}
			name, ok := s.pickLocked(pool, set, h, now)
			if !ok && !bg {
				if v := s.yieldLocked(pool, now); v != nil {
					say(v, "["+v.Label+"] paused on "+v.rec.Entry+" to make room for foreground work; it resumes when the entry has been idle for "+set.IdleAfter.String())
					name, ok = s.pickLocked(pool, set, h, now)
				}
			}
			if !ok {
				i++
				continue
			}
			drop(i)
			if text := s.startLocked(h, pool, name, now, h.Label != skip); text != "" {
				say(h, text)
			}
		}
	}
	s.persistQueueLocked(fc, set, now)
	s.armTimerLocked(set, now)
	return lines
}

// pickLocked is the entry job h may take now. A paused job can only go back
// to the entry its process lives on.
func (s *Server) pickLocked(pool *foreman.Pool, set *foreman.PoolSet, h *heldJob, now time.Time) (string, bool) {
	loads := s.loadsLocked(pool)
	if h.rec.State == workPaused {
		l := loads[h.rec.Entry]
		e, _ := entryOf(pool, h.rec.Entry)
		if l.Until.After(now) || l.Running >= e.Slots || !l.Idle(now, set.IdleAfter) {
			return "", false
		}
		return h.rec.Entry, true
	}
	return foreman.PickEntry(pool, loads, now, h.Background, set.IdleAfter)
}

// yieldLocked stops every process group of one running background worker of
// the pool, if one has an entry that is not cooling down. It returns the job,
// now paused and held, or nil. The groups are stopped, not killed, and go on
// with SIGCONT (FM-R-98, FM-R-107).
func (s *Server) yieldLocked(pool *foreman.Pool, now time.Time) *heldJob {
	loads := s.loadsLocked(pool)
	for _, w := range s.work {
		pid := s.pool.pids[w.Label]
		if w.Pool != pool.Name || !w.Background || w.State != workRunning || pid <= 0 || s.pool.noYield[w.Label] || loads[w.Entry].Until.After(now) {
			continue
		}
		// Every process group of the worker stops, the model process
		// included. The room counts as free only when all of them have.
		groups, ok := freezeTree(pid)
		if !ok {
			continue
		}
		if s.pool.frozen == nil {
			s.pool.frozen = map[string][]int{}
		}
		s.pool.frozen[w.Label] = groups
		w.State = workPaused
		h := &heldJob{rec: w, fc: s.pool.fc, HeldJob: foreman.HeldJob{Label: w.Label, Project: w.Project, Pool: w.Pool, Model: w.Model, Background: true, Added: w.Started}}
		s.pool.held = append(s.pool.held, h)
		return h
	}
	return nil
}

// startLocked starts, or resumes, job h on the entry. It returns the line for
// the chat; tell false leaves a start that the spawn call reports itself.
func (s *Server) startLocked(h *heldJob, pool *foreman.Pool, entry string, now time.Time, tell bool) string {
	w := h.rec
	key := entryKey(pool.Name, entry)
	if !h.Background {
		s.pool.lastFg[key] = now
	}
	if w.State == workPaused {
		s.thawLocked(w.Label)
		w.State = workRunning
		return "[" + w.Label + "] resumed on " + entry
	}
	e, _ := entryOf(pool, entry)
	pe, err := readProjectEnvelope(filepath.Join(envelopeDir(), w.Project+".json"), w.Project)
	if err != nil {
		w.State, w.Entry = workFailed, ""
		return "[" + w.Label + "] failed: " + err.Error()
	}
	w.State, w.Entry = workRunning, entry
	brief := h.Brief
	if h.Cut {
		brief = "Your earlier run on this brief was cut off. The worktree holds the work it did. Continue it.\n\n" + brief
	}
	s.workWG.Add(1)
	go s.runWorker(s.workCtx, h.fc, w, pe, brief, e.BaseURL)
	if !tell {
		return ""
	}
	return "[" + w.Label + "] started on " + entry
}

// thawLocked resumes the process groups of a paused job. A stream that was
// open when the job stopped can fail the moment it goes on, and the node
// timeout of the tree counts the time it stood still, so the time of the
// resume is kept for poolOut (FM-R-107).
func (s *Server) thawLocked(label string) {
	if s.pool.resumed == nil {
		s.pool.resumed = map[string]time.Time{}
	}
	s.pool.resumed[label] = s.clock()
	thawTree(s.pool.pids[label], s.pool.frozen[label])
	delete(s.pool.frozen, label)
}

// poolEndedLocked is the pool part of a worker's end: the entry answered, a
// foreground job counts as the entry's last foreground work, and the queue
// runs again. The caller holds workMu.
func (s *Server) poolEndedLocked(fc *foremanChat, w *workerRec, state string) []poolLine {
	if s.pool.health == nil {
		s.pool.health = map[string]*foreman.Health{}
		s.pool.lastFg = map[string]time.Time{}
	}
	key := entryKey(w.Pool, w.Entry)
	if state == workRan {
		if h := s.pool.health[key]; h != nil {
			h.Ok()
		}
	}
	if !w.Background {
		s.pool.lastFg[key] = s.clock()
	}
	return s.poolPumpLocked(fc, "")
}

// resumeGrace is how long after a resume a failure counts as the pause's.
const resumeGrace = 60 * time.Second

// poolOut reads the failure of a pool worker. When its entry is out or did
// not answer, the entry cools down and the job goes back in the queue at the
// front, to run on another entry of the same pool; it returns true. Any other
// failure, and a key problem, are the worker's own end (false).
func (s *Server) poolOut(ctx context.Context, fc *foremanChat, w *workerRec, brief, detail string) bool {
	if w.Pool == "" || ctx.Err() != nil {
		return false
	}
	now := s.clock()
	// A failure that comes within resumeGrace of a resume is the pause: the
	// node timeout counted the time the job stood still, or its stream died
	// while it was stopped. The entry did nothing wrong: it does not cool,
	// and the job runs again once (FM-R-107).
	s.workMu.Lock()
	at, resumed := s.pool.resumed[w.Label]
	delete(s.pool.resumed, w.Label)
	if resumed && now.Sub(at) < resumeGrace {
		entry := w.Entry
		w.State, w.Entry = workQueued, ""
		s.pool.held = append([]*heldJob{{rec: w, fc: fc, HeldJob: foreman.HeldJob{
			Label: w.Label, Project: w.Project, Pool: w.Pool, Model: w.Model, Brief: brief, Background: w.Background,
			Added: w.Started, Cut: true,
		}}}, s.pool.held...)
		lines := []poolLine{{fc, "[" + w.Label + "] stopped right after it resumed on " + entry + ", so " + entry + " is not blamed; it runs again"}}
		lines = append(lines, s.poolPumpLocked(fc, "")...)
		s.saveWorkersLocked(fc)
		s.workMu.Unlock()
		s.flushLines(lines)
		return true
	}
	s.workMu.Unlock()
	v := foreman.Classify(detail, now)
	if v.Outcome != foreman.Out {
		return false
	}
	s.workMu.Lock()
	if s.pool.health == nil {
		s.pool.health = map[string]*foreman.Health{}
		s.pool.lastFg = map[string]time.Time{}
	}
	key := entryKey(w.Pool, w.Entry)
	h := s.pool.health[key]
	if h == nil {
		h = &foreman.Health{}
		s.pool.health[key] = h
	}
	h.Fail(now, v)
	entry := w.Entry
	conn := v.Reason == "cannot connect"
	w.State, w.Entry = workQueued, ""
	job := &heldJob{rec: w, fc: fc, HeldJob: foreman.HeldJob{
		Label: w.Label, Project: w.Project, Pool: w.Pool, Model: w.Model, Brief: brief, Background: w.Background,
		Added: w.Started, Cut: !conn,
	}}
	s.pool.held = append([]*heldJob{job}, s.pool.held...)
	var lines []poolLine
	if conn {
		lines = append(lines, poolLine{fc, "[" + w.Label + "] " + entry + " did not answer"})
	} else {
		lines = append(lines, poolLine{fc, "[" + w.Label + "] " + entry + " is out of quota (" + foreman.SafeReason(v.Reason) + ")"})
	}
	more := s.poolPumpLocked(fc, w.Label)
	if w.State == workRunning {
		lines[0].text += "; moved to " + w.Entry
	} else if set, _ := foreman.LoadPools(s.poolsPath()); set != nil && set.Pools[w.Pool] != nil {
		pool := set.Pools[w.Pool]
		if until, all := s.allOutLocked(pool, now); all && !conn {
			lines = append(lines, poolLine{fc, "[" + w.Label + "] paused: every account of the model " + pool.Model + " is out; requeued until " + clockText(until)})
		} else if all {
			lines[0].text += "; every entry of the pool " + pool.Name + " is down, so the job is requeued until " + clockText(until)
		} else {
			lines[0].text += "; the job waits for a free entry"
		}
	}
	lines = append(lines, more...)
	s.saveWorkersLocked(fc)
	s.workMu.Unlock()
	s.flushLines(lines)
	return true
}

// allOutLocked reports whether every entry of the pool is cooling down, and
// when the first of them is ready again.
func (s *Server) allOutLocked(pool *foreman.Pool, now time.Time) (time.Time, bool) {
	var first time.Time
	for _, e := range pool.Entries {
		h := s.pool.health[entryKey(pool.Name, e.Name)]
		if h == nil || !h.Until.After(now) {
			return time.Time{}, false
		}
		if first.IsZero() || h.Until.Before(first) {
			first = h.Until
		}
	}
	return first, true
}

// persistQueueLocked writes the queue file the command and the Machines
// view read, and the held jobs that a restart keeps.
func (s *Server) persistQueueLocked(fc *foremanChat, set *foreman.PoolSet, now time.Time) {
	var q foreman.QueueState
	for _, w := range s.work {
		if w.Pool == "" || (w.State != workQueued && w.State != workPaused && w.State != workRunning) {
			continue
		}
		state := "running"
		switch w.State {
		case workQueued:
			state = "waiting"
		case workPaused:
			state = "paused"
		}
		q.Jobs = append(q.Jobs, foreman.QueueJob{Label: w.Label, Project: w.Project, Pool: w.Pool, Background: w.Background,
			State: state, Entry: w.Entry, Added: w.Started})
	}
	for _, h := range s.pool.held {
		if h.rec.State == workQueued {
			q.Held = append(q.Held, h.HeldJob)
		}
	}
	q.Entries = s.entriesLocked(set, now)
	_ = foreman.WriteQueue(s.workDir(fc), q)
}

// entriesLocked is the state of each entry of each pool.
func (s *Server) entriesLocked(set *foreman.PoolSet, now time.Time) []foreman.QueueEntry {
	var out []foreman.QueueEntry
	for _, pool := range sortedPools(set) {
		loads := s.loadsLocked(pool)
		for _, e := range pool.Entries {
			l := loads[e.Name]
			qe := foreman.QueueEntry{Pool: pool.Name, Name: e.Name, BaseURL: e.BaseURL, Slots: e.Slots, Running: l.Running, State: "free"}
			switch {
			case l.Until.After(now):
				qe.State, qe.Until = "cooling", l.Until.UTC().Format(foreman.DateLayout)
				if h := s.pool.health[entryKey(pool.Name, e.Name)]; h != nil {
					qe.Reason = foreman.SafeReason(h.Reason)
				}
			case l.Running >= e.Slots:
				qe.State = "busy"
			}
			out = append(out, qe)
		}
	}
	return out
}

// poolEntriesView is entriesLocked for the Machines view: nil when there are
// no pools, or the pools file does not read.
func (s *Server) poolEntriesView() []foreman.QueueEntry {
	set, err := foreman.LoadPools(s.poolsPath())
	if err != nil || len(set.Pools) == 0 {
		return nil
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.pool.health == nil {
		s.pool.health = map[string]*foreman.Health{}
		s.pool.lastFg = map[string]time.Time{}
	}
	return s.entriesLocked(set, s.clock())
}

func sortedPools(set *foreman.PoolSet) []*foreman.Pool {
	var out []*foreman.Pool
	for _, p := range set.Pools {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// restoreHeldLocked reads the held background jobs that the last server
// left in the queue file. The caller holds workMu.
func (s *Server) restoreHeldLocked(fc *foremanChat) (out map[string]foreman.HeldJob, note string) {
	out = map[string]foreman.HeldJob{}
	q, err := foreman.ReadQueue(s.workDir(fc))
	if err != nil {
		// The file stays as evidence: it is moved aside, and the next write
		// starts a fresh one (FM-R-112).
		path := filepath.Join(s.workDir(fc), foreman.QueueFile)
		if to, merr := foreman.MoveAside(path, s.clock()); merr == nil {
			note = foreman.QueueFile + " did not read. It was moved to " + filepath.Base(to) + " and the queue starts empty."
		} else {
			note = foreman.QueueFile + " did not read and could not be moved aside, so the queue starts empty."
		}
		return out, note
	}
	for _, h := range q.Held {
		if h.Background {
			out[h.Label] = h
		}
	}
	return out, ""
}

// armTimerLocked wakes the queue when something that holds a job back ends:
// a cool-down, or the idle time a background job waits for.
func (s *Server) armTimerLocked(set *foreman.PoolSet, now time.Time) {
	pr := &s.pool
	if pr.timer != nil {
		pr.timer.Stop()
		pr.timer = nil
	}
	if len(pr.held) == 0 {
		return
	}
	var next time.Time
	see := func(t time.Time) {
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, h := range pr.held {
		pool := set.Pools[h.Pool]
		if pool == nil {
			continue
		}
		for _, e := range pool.Entries {
			key := entryKey(pool.Name, e.Name)
			if hh := pr.health[key]; hh != nil {
				see(hh.Until)
			}
			if h.Background {
				if t := pr.lastFg[key]; !t.IsZero() {
					see(t.Add(set.IdleAfter))
				}
			}
		}
	}
	if next.IsZero() {
		return
	}
	fc := pr.fc
	pr.timer = time.AfterFunc(max(next.Sub(now), 0)+50*time.Millisecond, func() {
		if !s.isClosed() && fc != nil {
			s.poolPump(fc)
		}
	})
}

// poolLoop looks at the queue on the same beat as the import file, until
// the chat closes.
func (s *Server) poolLoop(fc *foremanChat, ctx context.Context) {
	every, on := s.pollEvery()
	if !on {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poolPoll(fc)
		}
	}
}

// poolPoll is one look at the queue: it takes the jobs that
// `coppice foreman queue add` left, as background jobs, and runs the queue.
func (s *Server) poolPoll(fc *foremanChat) {
	// A pools file that broke since the last look is moved aside now, not at
	// the next spawn.
	if _, note := s.loadPoolSet(); note != "" {
		s.logWork(fc, note)
	}
	reqs, bad := foreman.TakeQueueRequests(s.workDir(fc))
	if bad > 0 {
		s.logWork(fc, "[queue add] "+strconv.Itoa(bad)+" request(s) did not read and were moved aside as "+foreman.QueueRequestDir+"/*.broken-*")
	}
	for _, r := range reqs {
		res, err := s.toolSpawnOpts(fc, r.Project, r.Brief, r.Profile, r.Model, true)
		if err != nil {
			s.logWork(fc, "[queue add] refused: "+strings.TrimSpace(err.Error()))
			continue
		}
		s.logWork(fc, "[queue add] "+res)
	}
	s.workMu.Lock()
	any := len(s.pool.held) > 0
	s.workMu.Unlock()
	if any {
		s.poolPump(fc)
	}
}

// closePools stops the queue's timer.
func (s *Server) closePools() {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.pool.timer != nil {
		s.pool.timer.Stop()
		s.pool.timer = nil
	}
}

package foreman

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/opendaisugi/coppice/internal/broken"
)

// Pools and the queue (FM-R-93 to FM-R-101). A worker profile may name a pool:
// several (machine, base URL) entries that serve ONE model. A spawn takes
// the first entry with a free slot, else the job waits in a queue the
// owner can read. This file holds the pure parts: the pools file, the pick
// of an entry, the cool-down of an entry, and the queue file the server
// writes and `coppice foreman queue` reads. The server (server/foreman_pool.go)
// runs the workers over it.

// PoolsFile is the file beside coppice.toml that holds the pools. It is a
// file of its own so that a save of coppice.toml, which writes only the keys
// it knows, can never drop a pool.
const PoolsFile = "pools.toml"

// DefaultIdleAfter is how long an entry must have had no foreground job
// before a background job may start on it.
const DefaultIdleAfter = 2 * time.Minute

// PoolEntry is one machine of a pool.
type PoolEntry struct {
	Name    string `toml:"name"`
	BaseURL string `toml:"base_url"`
	// Slots is how many workers the entry runs at once; 0 reads as 1.
	Slots int `toml:"slots"`
}

// Pool is the entries that serve one model. Its name is the name of the
// worker profile that a spawn gives.
type Pool struct {
	Name    string      `toml:"-"`
	Model   string      `toml:"model"`
	Entries []PoolEntry `toml:"entries"`
}

// PoolSet is the whole pools file.
type PoolSet struct {
	IdleAfter time.Duration
	Pools     map[string]*Pool
}

type poolsDoc struct {
	IdleAfter string           `toml:"idle_after"`
	Pool      map[string]*Pool `toml:"pool"`
}

var poolName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParsePools reads the text of a pools file. path is only for the words of
// an error.
func ParsePools(text, path string) (*PoolSet, error) {
	var d poolsDoc
	if _, err := toml.Decode(text, &d); err != nil {
		return nil, fmt.Errorf("%s does not read: %v", path, err)
	}
	set := &PoolSet{IdleAfter: DefaultIdleAfter, Pools: map[string]*Pool{}}
	if d.IdleAfter != "" {
		v, err := time.ParseDuration(d.IdleAfter)
		if err != nil || v < 0 {
			return nil, fmt.Errorf("idle_after is %q in %s. Use a duration such as \"2m\"", d.IdleAfter, path)
		}
		set.IdleAfter = v
	}
	for name, p := range d.Pool {
		if name == "default" || !poolName.MatchString(name) {
			return nil, fmt.Errorf("the pool %q in %s is not allowed. A pool is named for a worker profile, and the Claude Code profile (default) has no base URL", name, path)
		}
		if p == nil || p.Model == "" {
			return nil, fmt.Errorf("the pool %q in %s needs a model: all its entries serve that one model", name, path)
		}
		if len(p.Entries) == 0 {
			return nil, fmt.Errorf("the pool %q in %s has no entries", name, path)
		}
		p.Name = name
		seen := map[string]bool{}
		for i := range p.Entries {
			e := &p.Entries[i]
			u, err := url.Parse(e.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("an entry of the pool %q in %s needs a base_url that starts with http:// or https://", name, path)
			}
			if e.Name == "" {
				e.Name = u.Hostname()
			}
			if !poolName.MatchString(e.Name) || seen[e.Name] {
				return nil, fmt.Errorf("the entry name %q of the pool %q in %s is not allowed or is used twice", e.Name, name, path)
			}
			seen[e.Name] = true
			if e.Slots < 0 {
				return nil, fmt.Errorf("the entry %q of the pool %q in %s has slots below 0", e.Name, name, path)
			}
			if e.Slots == 0 {
				e.Slots = 1
			}
		}
		set.Pools[name] = p
	}
	return set, nil
}

// MoveAside renames path to <path>.broken-<UTC time> (the first free name)
// and returns the new path. The rule for broken state is to keep it and start
// fresh (FM-R-112); the newest few copies stay.
func MoveAside(path string, now time.Time) (string, error) {
	to := freeName(path + ".broken-" + now.UTC().Format("20060102T150405Z"))
	if err := os.Rename(path, to); err != nil {
		return "", err
	}
	broken.Prune(to)
	return to, nil
}

// LoadPools reads the pools file. A file that is not there is no pools.
func LoadPools(path string) (*PoolSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PoolSet{IdleAfter: DefaultIdleAfter, Pools: map[string]*Pool{}}, nil
		}
		return nil, fmt.Errorf("%s cannot be read: %v", path, err)
	}
	return ParsePools(string(b), path)
}

// Load is what the server knows of one entry when it picks.
type Load struct {
	Running int
	Slots   int
	// FgRunning counts the foreground workers that run on the entry.
	FgRunning int
	// Until is the end of the entry's cool-down; zero when it is ready.
	Until time.Time
	// LastFg is when a foreground worker last started or ended there.
	LastFg time.Time
}

// Idle reports whether the entry has had no foreground work for idle.
func (l Load) Idle(now time.Time, idle time.Duration) bool {
	return l.FgRunning == 0 && (l.LastFg.IsZero() || now.Sub(l.LastFg) >= idle)
}

// PickEntry is the first entry of p, in file order, that has a free slot and
// is not cooling down. A background job also needs the entry to be idle.
// loads is keyed by entry name; an entry with no load is empty.
func PickEntry(p *Pool, loads map[string]Load, now time.Time, background bool, idle time.Duration) (string, bool) {
	for _, e := range p.Entries {
		l, ok := loads[e.Name]
		if !ok {
			l = Load{}
		}
		if l.Until.After(now) || l.Running >= e.Slots {
			continue
		}
		if background && !l.Idle(now, idle) {
			continue
		}
		return e.Name, true
	}
	return "", false
}

// Entry cool-down for an entry that does not answer: it starts at
// ConnCoolStart and doubles on each repeat to ConnCoolMax. A success starts
// it over. A quota uses the reset the reply names, else the failover bounds.
const (
	ConnCoolStart = 30 * time.Second
	ConnCoolMax   = 10 * time.Minute
)

// Health is the cool-down state of one entry.
type Health struct {
	Until   time.Time `json:"until,omitempty"`
	Repeats int       `json:"repeats,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// Fail cools the entry after a verdict of Out. conn is true when the entry
// did not answer at all.
func (h *Health) Fail(now time.Time, v Verdict) {
	var d time.Duration
	switch {
	case !v.Reset.IsZero():
		h.Until, h.Reason = v.Reset, v.Reason
		h.Repeats++
		return
	case v.Reason == "cannot connect":
		d = ConnCoolStart << min(h.Repeats, 6)
		d = min(d, ConnCoolMax)
	default:
		d = CoolStart << min(h.Repeats, 6)
		d = min(d, CoolMax)
	}
	h.Until, h.Reason = now.Add(d), v.Reason
	h.Repeats++
}

// Ok starts the entry's back-off over.
func (h *Health) Ok() { *h = Health{} }

// QueueFile is the file in the work dir that holds the queue and the
// entries. The server writes it; `coppice foreman queue` reads it.
const QueueFile = "queue.json"

// QueueRequestDir is the dir in the work dir where `coppice foreman queue
// add` leaves one file for each job. The server takes them.
const QueueRequestDir = "queue-in"

// QueueJob is one job of the queue, as the queue file shows it.
type QueueJob struct {
	Label      string `json:"label"`
	Project    string `json:"project"`
	Pool       string `json:"pool"`
	Background bool   `json:"background,omitempty"`
	// State is waiting, paused or running.
	State string `json:"state"`
	Entry string `json:"entry,omitempty"`
	Added string `json:"added"`
	Note  string `json:"note,omitempty"`
}

// QueueEntry is one entry of a pool, as the queue file shows it.
type QueueEntry struct {
	Pool    string `json:"pool"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	Slots   int    `json:"slots"`
	Running int    `json:"running"`
	// State is free, busy, cooling or idle-wait.
	State  string `json:"state"`
	Until  string `json:"until,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// QueueState is the content of the queue file.
type QueueState struct {
	Jobs    []QueueJob   `json:"jobs"`
	Entries []QueueEntry `json:"entries"`
	// Held are the background jobs that wait for an idle machine, with the
	// request they came from; the server reads them back at a start.
	Held []HeldJob `json:"held,omitempty"`
}

// HeldJob is a job that is not running: what the server needs to start it.
type HeldJob struct {
	Label      string `json:"label"`
	Project    string `json:"project"`
	Pool       string `json:"pool"`
	Model      string `json:"model,omitempty"`
	Brief      string `json:"brief"`
	Background bool   `json:"background,omitempty"`
	Added      string `json:"added"`
	Cut        bool   `json:"cut,omitempty"`
}

// WriteQueue writes the queue file (0600) in dir, through a temp file.
func WriteQueue(dir string, q QueueState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(q)
	tmp := filepath.Join(dir, QueueFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, QueueFile))
}

// ReadQueue reads the queue file in dir. No file is an empty queue.
func ReadQueue(dir string) (QueueState, error) {
	var q QueueState
	b, err := os.ReadFile(filepath.Join(dir, QueueFile))
	if err != nil {
		if os.IsNotExist(err) {
			return q, nil
		}
		return q, err
	}
	if err := json.Unmarshal(b, &q); err != nil {
		return QueueState{}, fmt.Errorf("%s does not read: %v", filepath.Join(dir, QueueFile), err)
	}
	return q, nil
}

// QueueRequest is one background job that the owner adds by hand.
type QueueRequest struct {
	Project string `json:"project"`
	Brief   string `json:"brief"`
	Profile string `json:"profile"`
	Model   string `json:"model,omitempty"`
}

// WriteQueueRequest leaves a request for the server in dir/queue-in. The
// file name is fresh, so two adds never meet.
func WriteQueueRequest(dir string, r QueueRequest, now time.Time) (string, error) {
	d := filepath.Join(dir, QueueRequestDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	b, _ := json.Marshal(r)
	name := fmt.Sprintf("%d-%d.json", now.UnixNano(), os.Getpid())
	tmp := filepath.Join(d, "."+name)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return name, os.Rename(tmp, filepath.Join(d, name))
}

// TakeQueueRequests reads and removes the requests in dir/queue-in, oldest
// first. A file that does not read is moved aside to <name>.broken-<time>
// (never deleted) and counted in bad.
func TakeQueueRequests(dir string) (reqs []QueueRequest, bad int) {
	d := filepath.Join(dir, QueueRequestDir)
	ents, err := os.ReadDir(d)
	if err != nil {
		return nil, 0
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(d, n)
		b, err := os.ReadFile(p)
		var r QueueRequest
		if err != nil || json.Unmarshal(b, &r) != nil || r.Project == "" || strings.TrimSpace(r.Brief) == "" || r.Profile == "" {
			// A file that cannot be moved stays, and is not counted again
			// and again.
			if _, merr := MoveAside(p, time.Now()); merr == nil {
				bad++
			}
			continue
		}
		_ = os.Remove(p)
		reqs = append(reqs, r)
	}
	return reqs, bad
}

// FormatQueue writes the queue the way `coppice foreman queue` shows it:
// one line for each job, then one for each entry.
func FormatQueue(w io.Writer, q QueueState) {
	if len(q.Jobs) == 0 {
		fmt.Fprintln(w, "queue: empty")
	} else {
		fmt.Fprintln(w, "queue:")
		for _, j := range q.Jobs {
			kind := "foreground"
			if j.Background {
				kind = "background"
			}
			on := j.Entry
			if on == "" {
				on = "-"
			}
			line := fmt.Sprintf("  %s | %s | %s | %s | %s | %s", j.Label, j.Project, j.Pool, kind, j.State, on)
			if j.Note != "" {
				line += " | " + j.Note
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(q.Entries) > 0 {
		fmt.Fprintln(w, "entries:")
		for _, e := range q.Entries {
			line := fmt.Sprintf("  %s/%s | %d of %d slots | %s", e.Pool, e.Name, e.Running, e.Slots, e.State)
			if e.Until != "" {
				line += " until " + e.Until
			}
			if e.Reason != "" {
				line += " (" + e.Reason + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
}

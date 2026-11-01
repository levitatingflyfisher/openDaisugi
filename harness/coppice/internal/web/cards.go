package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The cards routes: the arena of RK-7 and ST-5. The queue and the answer
// are daisugi's. The floor runs the commands and shows what they print.

const maxDiffBytes = 512 << 10

var cardSorts = map[string]bool{"reversibility": true, "impact": true, "date": true, "project": true}

func (s *Server) cardsRoutes() {
	s.mux.HandleFunc("GET /api/cards", s.guard(s.handleCards))
	s.mux.HandleFunc("GET /api/cards/{id}/diff", s.guard(s.handleCardDiff))
	s.mux.HandleFunc("POST /api/cards/answer", s.guardAnswer(s.handleCardAnswer))
}

// dataDirLabel is the data directory as the reply names it.
func dataDirLabel() string {
	if d := daisugiDataDir(); d != "" {
		return d
	}
	return "default"
}

// daisugiJSON runs a daisugi command that prints one JSON object, and
// answers the web request with 502 when it cannot.
func (s *Server) daisugiJSON(w http.ResponseWriter, r *http.Request, what string, args ...string) (map[string]any, bool) {
	return s.daisugiJSONIn(w, r, true, daisugiDataDir(), what, args...)
}

// daisugiJSONIn is daisugiJSON for one data dir. With answer false a
// failure is logged and not answered: the caller goes on to the next dir.
func (s *Server) daisugiJSONIn(w http.ResponseWriter, r *http.Request, answer bool, dir, what string, args ...string) (map[string]any, bool) {
	stdout, stderr, code, err := runDaisugi(r.Context(), daisugiArgsIn(dir, args...)...)
	if err != nil || code != 0 {
		why := firstLine(stderr)
		if err != nil {
			why = err.Error()
		}
		s.opts.Log.Warn("web: "+what+" failed", "why", why, "data_dir", dataLabel(dir))
		if answer {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": what + " failed: " + why})
		}
		return nil, false
	}
	var doc map[string]any
	if json.Unmarshal([]byte(stdout), &doc) != nil || doc == nil {
		if answer {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": what + " failed: its output is not JSON"})
		}
		return nil, false
	}
	return doc, true
}

func (s *Server) handleCards(w http.ResponseWriter, r *http.Request) {
	sort := r.URL.Query().Get("sort")
	if sort == "" {
		sort = "reversibility"
	}
	if !cardSorts[sort] {
		writeErr(w, http.StatusBadRequest, "Sort by reversibility, impact, date or project.")
		return
	}
	srcs := s.dataSources()
	var doc map[string]any
	seen := map[string]bool{}
	for i, dir := range srcs {
		d, ok := s.daisugiJSONIn(w, r, i == 0, dir, "daisugi rank queue", "rank", "queue", "--json", "--sort", sort)
		if !ok {
			if i == 0 {
				return
			}
			continue
		}
		cards, _ := d["cards"].([]any)
		if doc == nil {
			doc = d
			doc["cards"] = []any{}
		} else if n, ok := d["decayed"].(float64); ok {
			prev, _ := doc["decayed"].(float64)
			doc["decayed"] = prev + n
		}
		for _, c := range cards {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["choice_id"].(string)
			if id != "" && seen[id] {
				continue
			}
			seen[id] = true
			m["data_dir"] = dataLabel(dir)
			doc["cards"] = append(doc["cards"].([]any), m)
		}
	}
	doc["source"] = "daisugi rank queue"
	doc["data_dir"] = dataDirLabel()
	writeJSON(w, http.StatusOK, doc)
}

// handleCardDiff shows the edits of each attempt on a card. The paths
// come from daisugi's own record of the card, and only a worktree under
// the branch directory or under a task's worktree is read (W3K-6).
func (s *Server) handleCardDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		writeErr(w, http.StatusNotFound, "There is no such card.")
		return
	}
	var stdout, stderr string
	var code int
	var err error
	for _, dir := range s.dataSources() {
		stdout, stderr, code, err = runDaisugi(r.Context(), daisugiArgsIn(dir, "rank", "show", "--choice", id, "--json")...)
		if !(err == nil && code == 1) {
			break // found, or failed for a reason other than "no such card"
		}
	}
	if err == nil && code == 1 {
		writeErr(w, http.StatusNotFound, firstLine(stderr))
		return
	}
	if err != nil || code != 0 {
		why := firstLine(stderr)
		if err != nil {
			why = err.Error()
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "daisugi rank show failed: " + why})
		return
	}
	where := whereOf(stdout)
	roots := s.worktreeRoots(r.Context())
	diffs := map[string]string{}
	notes := map[string]string{}
	for attempt, wt := range where {
		if wt.Path == "" {
			notes[attempt] = "no diff: this attempt has no worktree"
			continue
		}
		if !underAny(wt.Path, roots) {
			notes[attempt] = "no diff: the alternative is outside the floor's worktrees"
			continue
		}
		d, note := worktreeDiff(r.Context(), wt)
		if note != "" {
			notes[attempt] = note
			continue
		}
		diffs[attempt] = d
	}
	writeJSON(w, http.StatusOK, map[string]any{"choice_id": id, "diffs": diffs, "notes": notes,
		"source": "daisugi rank show"})
}

// place is where an attempt's edits are: the worktree path, and the base
// commit and repository that daisugi recorded for it when it made the
// worktree. The floor never reads them from the worktree.
type place struct{ Path, Base, Repo string }

// whereOf reads, from `rank show --json`, each attempt's worktree place.
// An attempt with another kind of place maps to the zero place.
func whereOf(showJSON string) map[string]place {
	var doc struct {
		Choices []struct {
			Event string `json:"event"`
			Facts struct {
				Where map[string]struct {
					Kind string `json:"kind"`
					Path string `json:"path"`
					Base string `json:"base"`
					Repo string `json:"repo"`
				} `json:"where"`
				Order []string `json:"order"`
			} `json:"facts"`
		} `json:"choices"`
	}
	out := map[string]place{}
	if json.Unmarshal([]byte(showJSON), &doc) != nil {
		return out
	}
	for _, c := range doc.Choices {
		if c.Event != "opened" {
			continue
		}
		for _, id := range c.Facts.Order {
			out[id] = place{}
			if w, ok := c.Facts.Where[id]; ok && w.Kind == "worktree" {
				out[id] = place{w.Path, w.Base, w.Repo}
			}
		}
	}
	return out
}

// worktreeRoots are the places a diff may be read from: the branch
// directory of each data dir, and the worktree of every task.
func (s *Server) worktreeRoots(ctx context.Context) []string {
	var roots []string
	for _, data := range s.dataSources() {
		if data == "" {
			if home, err := os.UserHomeDir(); err == nil {
				data = filepath.Join(home, ".opendaisugi")
			}
		}
		if data != "" {
			roots = append(roots, filepath.Join(data, "tree", "branches"))
		}
	}
	if msg, err := Call(ctx, s.opts.Dial, map[string]any{"cmd": "task.list"}); err == nil {
		res, _ := msg["result"].(map[string]any)
		tasks, _ := res["tasks"].([]any)
		for _, t := range tasks {
			if m, ok := t.(map[string]any); ok {
				if wt, _ := m["worktree"].(string); wt != "" {
					roots = append(roots, wt)
				}
			}
		}
	}
	return roots
}

// underAny reports whether path, with symlinks resolved, is inside one of
// roots, also resolved. A root that is itself the path does not count: a
// diff reads a worktree under a root.
func underAny(path string, roots []string) bool {
	p, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(p) {
		return false
	}
	for _, root := range roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rr, p)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// worktreeDiff is `git diff` of a branch worktree against the base commit
// daisugi recorded for it. A base that is not a full object id is never
// given to git, and neither is a worktree whose repository is not the one
// recorded: the child can rewrite the worktree's .git pointer.
func worktreeDiff(ctx context.Context, p place) (string, string) {
	if !objectID.MatchString(p.Base) || p.Repo == "" {
		return "", "no diff: daisugi recorded no base commit for this worktree"
	}
	if !ownsWorktree(ctx, p) {
		return "", "no diff: the worktree no longer belongs to its repository"
	}
	out, ok := gitOut(ctx, p.Path, "diff", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--no-renames", p.Base, "--")
	if !ok {
		return "", "no diff: git could not read the worktree"
	}
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n[cut at 512 KiB]\n"
	}
	return out, ""
}

// ownsWorktree is true when the path is the linked worktree git made for it
// in the recorded repository: not a symlink, its .git a regular file, its
// git directory exactly <repo>/worktrees/<name>. The common directory alone
// also admits the repository's main git directory and a sibling's.
func ownsWorktree(ctx context.Context, p place) bool {
	if fi, err := os.Lstat(p.Path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if fi, err := os.Lstat(filepath.Join(p.Path, ".git")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	// With extensions.worktreeConfig on, git reads a config.worktree the child
	// can write, and no -c guard switches that off.
	if _, err := os.Lstat(filepath.Join(p.Repo, "worktrees", filepath.Base(p.Path), "config.worktree")); err == nil {
		return false
	}
	// A fenced child's own object store that daisugi has not folded away yet:
	// the worktree's HEAD and index may name objects only that store holds,
	// and this reader never reads a store the child wrote.
	if _, err := os.Lstat(filepath.Join(p.Repo, "worktrees", filepath.Base(p.Path), "objects")); err == nil {
		return false
	}
	gd, ok := gitOut(ctx, p.Path, "rev-parse", "--path-format=absolute", "--git-dir")
	if !ok || !sameDir(strings.TrimSpace(gd), filepath.Join(p.Repo, "worktrees", filepath.Base(p.Path))) {
		return false
	}
	common, ok := gitOut(ctx, p.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	return ok && sameDir(strings.TrimSpace(common), p.Repo)
}

// objectID is a full git object id: 40 or 64 lower-case hex digits.
var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func sameDir(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

func gitOut(ctx context.Context, dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, daisugiTimeout)
	defer cancel()
	cmd := newGit(ctx, dir, args...)
	var out capBuf
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return "", false
	}
	return out.b.String(), true
}

// handleCardAnswer records the operator's answer on a card. Only the
// operator may: a pane, a peer the server cannot place and a lock-screen
// deny token are refused.
func (s *Server) handleCardAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChoiceID string `json:"choice_id"`
		Outcome  string `json:"outcome"`
		Pick     string `json:"pick"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "The request body is not JSON.")
		return
	}
	if !s.operatorOnly(w, r) {
		return
	}
	if !idPattern.MatchString(body.ChoiceID) {
		writeErr(w, http.StatusBadRequest, "choice_id is not a card id.")
		return
	}
	var args []string
	switch body.Outcome {
	case "keep", "drop":
		args = []string{"rank", "record", "drop", body.ChoiceID, "--json"}
	case "switch":
		if !idPattern.MatchString(body.Pick) {
			writeErr(w, http.StatusBadRequest, "A switch names the attempt to pick.")
			return
		}
		args = []string{"rank", "record", "pick", body.ChoiceID, body.Pick, "--json"}
	default:
		writeErr(w, http.StatusBadRequest, "outcome is keep, drop or switch.")
		return
	}
	var stdout, stderr, answeredIn string
	var code int
	var err error
	for _, dir := range s.dataSources() {
		stdout, stderr, code, err = runDaisugi(r.Context(), daisugiArgsIn(dir, args...)...)
		answeredIn = dir
		if !(err == nil && code == 1) {
			break // this dir holds the card, or failed for another reason
		}
	}
	switch {
	case err != nil || code < 0 || code > 1:
		why := firstLine(stderr)
		if err != nil {
			why = err.Error()
		}
		s.opts.Log.Warn("web: a card answer failed", "why", why)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "daisugi rank record failed: " + why})
		return
	case code == 1:
		writeErr(w, http.StatusNotFound, strings.TrimSpace(stderr))
		return
	}
	reply := map[string]any{}
	if json.Unmarshal([]byte(stdout), &reply) != nil || reply == nil {
		reply = map[string]any{"output": strings.TrimSpace(stdout)}
	}
	// The card is closed, so its branch worktrees can go (W3K-7). They are
	// in the data dir that held the card.
	if out, _, c, e := runDaisugi(r.Context(), daisugiArgsIn(answeredIn, "tree", "prune-branches", "--json")...); e == nil && c == 0 {
		var pruned any
		if json.Unmarshal([]byte(out), &pruned) != nil {
			pruned = strings.TrimSpace(out)
		}
		reply["pruned"] = pruned
	}
	writeJSON(w, http.StatusOK, reply)
}

// operatorOnly refuses, and answers, any request that is not the
// operator's: a lock-screen deny token, a pane process, a local peer the
// server cannot place.
func (s *Server) operatorOnly(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := r.Context().Value(denyOnlyKey{}).(string); ok {
		writeErr(w, http.StatusForbidden, "This card can only deny its own ask.")
		return false
	}
	if hello := peerOf(r); hello != nil && !placeAllows(r.Context(), s.opts.Dial, hello) {
		s.opts.Log.Warn("web: refused an answer from a pane", "remote", r.RemoteAddr, "path", r.URL.Path)
		writeErr(w, http.StatusForbidden, PaneRefusal)
		return false
	}
	return true
}

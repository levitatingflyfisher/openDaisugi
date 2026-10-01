package gateway

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// This file is opendaisugi.route_knn: the knn router. It sends a turn to
// the model that did well on similar past tasks. Rulings RT-R-1 to RT-R-7
// in clients/ADJUDICATIONS.md fix every number here.

// The writers of an outcome: the gateway, a person, or a later grader model.
const (
	SourceAuto   = "auto"
	SourceHuman  = "human"
	SourceGrader = "grader"
)

// sourceRank: human feedback beats the grader, and the grader beats the
// automatic labels.
func sourceRank(source string) int {
	switch source {
	case SourceHuman:
		return 2
	case SourceGrader:
		return 1
	}
	return 0
}

// The outcomes a booked turn can hold.
const (
	KnnGood    = "good"
	KnnBad     = "bad"
	KnnUnknown = "unknown"
)

// The knn router's fixed numbers.
const (
	KnnK           = 8
	KnnMinKnown    = 3
	KnnCostWeight  = 0.2
	KnnMaxRows     = 2000
	KnnRetryWindow = 600.0
	simScale       = 1_000_000.0
)

const knnSchema = `
CREATE TABLE IF NOT EXISTS turns (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT UNIQUE,
    conversation TEXT NOT NULL,
    ts REAL NOT NULL,
    task_sha256 TEXT NOT NULL,
    embedder TEXT NOT NULL,
    vec BLOB NOT NULL,
    model TEXT NOT NULL,
    cost REAL NOT NULL,
    latency_ms REAL,
    outcome TEXT NOT NULL DEFAULT 'unknown',
    source TEXT NOT NULL DEFAULT 'auto'
)
`

// KnnRow is route_knn.Row.
type KnnRow struct {
	Seq          int64
	TurnID       string
	Conversation string
	TS           float64
	TaskSHA      string
	Embedder     string
	Vec          []float32
	Model        string
	Cost         float64
	LatencyMS    *float64
	Outcome      string
}

// KnnNeighbour is route_knn.Neighbour.
type KnnNeighbour struct {
	TurnID     string
	Similarity float64
	Model      string
	Outcome    string
	Seq        int64
}

// KnnScore is one candidate's score.
type KnnScore struct {
	Model string
	Score float64
}

// KnnDecision is route_knn.KnnDecision.
type KnnDecision struct {
	Model      string
	Fallback   bool
	Reason     string
	Neighbours []KnnNeighbour
	Known      int
	Scores     []KnnScore
}

// PackVec is pack_vec: float32, little endian.
func PackVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

// UnpackVec is unpack_vec.
func UnpackVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// ToF32 rounds a vector of float64 to float32, as numpy does.
func ToF32(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}

// Round6 is round6: floor(x * 1e6 + 0.5) / 1e6.
func Round6(x float64) float64 {
	return math.Floor(x*simScale+0.5) / simScale
}

// Similarity is route_knn.similarity: the float64 dot product of two
// float32 vectors, rounded to six places. A length mismatch gives -1.
func Similarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1.0
	}
	total := 0.0
	for i := range a {
		total += float64(a[i]) * float64(b[i])
	}
	return Round6(total)
}

// TaskSHA256 is task_sha256.
func TaskSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// OutcomeStore is route_knn.OutcomeStore.
type OutcomeStore struct{ Path string }

func (s *OutcomeStore) connect() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o777); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+s.Path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(knnSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (s *OutcomeStore) exists() bool {
	_, err := os.Stat(s.Path)
	return err == nil
}

// Add inserts one row and returns its turn id, t<seq>. It keeps the
// newest KnnMaxRows.
func (s *OutcomeStore) Add(conversation string, ts float64, taskSHA, embedder string, vec []float32,
	model string, cost float64, latency *float64, outcome, source string) (string, error) {
	db, err := s.connect()
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	var lat any
	if latency != nil {
		lat = *latency
	}
	res, err := tx.Exec("INSERT INTO turns (conversation, ts, task_sha256, embedder, vec, model,"+
		" cost, latency_ms, outcome, source) VALUES (?,?,?,?,?,?,?,?,?,?)",
		conversation, ts, taskSHA, embedder, PackVec(vec), model, cost, lat, outcome, source)
	if err != nil {
		tx.Rollback()
		return "", err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return "", err
	}
	id := "t" + strconv.FormatInt(seq, 10)
	if _, err := tx.Exec("UPDATE turns SET turn_id=? WHERE seq=?", id, seq); err != nil {
		tx.Rollback()
		return "", err
	}
	if _, err := tx.Exec("DELETE FROM turns WHERE seq <= ?", seq-KnnMaxRows); err != nil {
		tx.Rollback()
		return "", err
	}
	return id, tx.Commit()
}

// SetOutcome sets a row's outcome. False when no such turn id exists.
func (s *OutcomeStore) SetOutcome(turnID, outcome, source string) (bool, error) {
	if outcome != KnnGood && outcome != KnnBad {
		return false, errors.New("outcome must be good or bad")
	}
	if source != SourceAuto && source != SourceHuman && source != SourceGrader {
		return false, errors.New("source must be auto, human or grader")
	}
	if !s.exists() {
		return false, nil
	}
	db, err := s.connect()
	if err != nil {
		return false, err
	}
	defer db.Close()
	res, err := db.Exec("UPDATE turns SET outcome=?, source=? WHERE turn_id=? AND "+
		"CASE source WHEN 'human' THEN 2 WHEN 'grader' THEN 1 ELSE 0 END <= ?",
		outcome, source, turnID, sourceRank(source))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *OutcomeStore) rows(where string, args []any, limit int) ([]KnnRow, error) {
	if !s.exists() {
		return nil, nil
	}
	db, err := s.connect()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args = append(args, limit)
	cur, err := db.Query("SELECT seq, turn_id, conversation, ts, task_sha256, embedder, vec, model,"+
		" cost, latency_ms, outcome FROM turns WHERE "+where+" ORDER BY seq DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer cur.Close()
	var out []KnnRow
	for cur.Next() {
		var r KnnRow
		var id sql.NullString
		var blob []byte
		var lat sql.NullFloat64
		if err := cur.Scan(&r.Seq, &id, &r.Conversation, &r.TS, &r.TaskSHA, &r.Embedder, &blob,
			&r.Model, &r.Cost, &lat, &r.Outcome); err != nil {
			return nil, err
		}
		r.TurnID = id.String
		if !id.Valid {
			r.TurnID = "t" + strconv.FormatInt(r.Seq, 10)
		}
		r.Vec = UnpackVec(blob)
		if lat.Valid {
			v := lat.Float64
			r.LatencyMS = &v
		}
		out = append(out, r)
	}
	return out, cur.Err()
}

// Recent is the newest rows made by embedder, newest first.
func (s *OutcomeStore) Recent(embedder string, limit int) ([]KnnRow, error) {
	return s.rows("embedder=?", []any{embedder}, limit)
}

// All is every row, newest first, up to limit.
func (s *OutcomeStore) All(limit int) ([]KnnRow, error) {
	return s.rows("1=1", nil, limit)
}

// LastInConversation is the newest row of a conversation, or nil.
func (s *OutcomeStore) LastInConversation(conversation string) (*KnnRow, error) {
	rs, err := s.rows("conversation=?", []any{conversation}, 1)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

// Count is the number of booked rows.
func (s *OutcomeStore) Count() (int, error) {
	if !s.exists() {
		return 0, nil
	}
	db, err := s.connect()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM turns").Scan(&n)
	return n, err
}

// KnnCandidates is candidate_models: the requested model first, then the
// cheaper rungs (the local model, then the cheap model). A rung is a
// candidate only when the price table shows it strictly cheaper.
func KnnCandidates(requested, cheap, local string, prices Prices) []string {
	out := []string{requested}
	for _, rung := range []string{local, cheap} {
		if rung == "" {
			continue
		}
		dup := false
		for _, o := range out {
			if o == rung {
				dup = true
			}
		}
		if dup {
			continue
		}
		if ok, _ := prices.StrictlyCheaper(rung, requested); ok {
			out = append(out, rung)
		}
	}
	return out
}

func unitPrice(model string, prices Prices) float64 {
	p, ok := prices[model]
	if !ok {
		p = fallbackPrice
	}
	return p.In + p.Out
}

// knnSuccess is route_knn._success: (sum sim of good + 1) / (sum sim of known + 2),
// with the number of good rows.
func knnSuccess(nbrs []KnnNeighbour, model string) (float64, int) {
	num, den := 0.0, 0.0
	good := 0
	for _, n := range nbrs {
		if n.Model != model || (n.Outcome != KnnGood && n.Outcome != KnnBad) {
			continue
		}
		den += n.Similarity
		if n.Outcome == KnnGood {
			good++
			num += n.Similarity
		}
	}
	return (num + 1.0) / (den + 2.0), good
}

// ScoreCandidates is score_candidates: the default scorer, a pure function
// of the neighbours and the candidates. It returns the best model and every
// eligible (model, score). A cheaper model is eligible only with a good
// neighbour and a success value (before the cost term) at or above the
// requested model's (RT-R-3a). Another scorer can replace it.
func ScoreCandidates(nbrs []KnnNeighbour, candidates []string, requested string, prices Prices) (string, []KnnScore) {
	requestedPrice := unitPrice(requested, prices)
	requestedSuccess, _ := knnSuccess(nbrs, requested)
	var scores []KnnScore
	best := requested
	bestScore := math.Inf(-1)
	for _, model := range candidates {
		success, good := knnSuccess(nbrs, model)
		if model != requested && (good == 0 || success < requestedSuccess) {
			continue
		}
		ratio := 0.0
		if requestedPrice > 0 {
			ratio = unitPrice(model, prices) / requestedPrice
		}
		score := success - KnnCostWeight*ratio
		scores = append(scores, KnnScore{model, score})
		if score > bestScore {
			bestScore = score
			best = model
		}
	}
	return best, scores
}

// KnnDecide is route_knn.decide.
func KnnDecide(query []float32, rows []KnnRow, requested string, candidates []string, prices Prices,
	threshold float64, k, minKnown int) KnnDecision {
	type scoredRow struct {
		sim float64
		row KnnRow
	}
	var scored []scoredRow
	for _, r := range rows {
		if sim := Similarity(query, r.Vec); sim >= threshold {
			scored = append(scored, scoredRow{sim, r})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].sim != scored[j].sim {
			return scored[i].sim > scored[j].sim
		}
		return scored[i].row.Seq > scored[j].row.Seq
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	nbrs := make([]KnnNeighbour, len(scored))
	known := 0
	for i, s := range scored {
		nbrs[i] = KnnNeighbour{s.row.TurnID, s.sim, s.row.Model, s.row.Outcome, s.row.Seq}
		if (s.row.Outcome == KnnGood || s.row.Outcome == KnnBad) && slices.Contains(candidates, s.row.Model) {
			known++
		}
	}
	if known < minKnown {
		return KnnDecision{Model: requested, Fallback: true, Neighbours: nbrs, Known: known,
			Reason: fmt.Sprintf("knn: %d of %d neighbours have a known outcome (need %d)", known, len(nbrs), minKnown)}
	}
	best, scores := ScoreCandidates(nbrs, candidates, requested, prices)
	ranked := append([]KnnScore(nil), scores...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	shown := ""
	for i, r := range ranked {
		if i == 3 {
			break
		}
		if i > 0 {
			shown += ", "
		}
		shown += r.Model + " " + FormatF(r.Score, 2)
	}
	return KnnDecision{Model: best, Neighbours: nbrs, Known: known, Scores: ranked,
		Reason: fmt.Sprintf("knn: %s; %d neighbours, %d known; scores %s", best, len(nbrs), known, shown)}
}

// KnnLabelPrevious is label_previous: the outcome the next human turn
// gives the previous turn, or "" for no change.
func KnnLabelPrevious(prev *KnnRow, vec []float32, embedder string, now, threshold float64) string {
	if prev == nil || prev.Outcome != KnnUnknown || prev.Embedder != embedder {
		return ""
	}
	if now-prev.TS > KnnRetryWindow {
		return ""
	}
	if Similarity(prev.Vec, vec) >= threshold {
		return KnnBad
	}
	return KnnGood
}

// Knn is KnnRouter: a store, an embedder and a threshold. Embed returns the
// task vector and the embedder identity.
type Knn struct {
	Store     *OutcomeStore
	Embed     func(text string) ([]float32, string, error)
	Threshold float64
}

// Record books one finished turn and labels the previous turn of its
// conversation.
func (k *Knn) Record(conversation, task string, vec []float32, embedder, model string, cost float64,
	latency *float64, failed bool, now float64) (string, error) {
	prev, err := k.Store.LastInConversation(conversation)
	if err != nil {
		return "", err
	}
	if label := KnnLabelPrevious(prev, vec, embedder, now, k.Threshold); label != "" {
		if _, err := k.Store.SetOutcome(prev.TurnID, label, SourceAuto); err != nil {
			return "", err
		}
	}
	outcome := KnnUnknown
	if failed {
		outcome = KnnBad
	}
	return k.Store.Add(conversation, now, TaskSHA256(task), embedder, vec, model, cost, latency, outcome, SourceAuto)
}

// KnnView is knn_view: the choice for a task and its nearest past turns,
// as plain data. embed and threshold are given by the caller.
type KnnView struct {
	Err        string
	Model      string
	Fallback   bool
	Reason     string
	Known      int
	Threshold  float64
	Neighbours []KnnNeighbour
	Scores     []KnnScore
}

// KnnViewOf is knn_view for an embedded task.
func KnnViewOf(vec []float32, identity string, store *OutcomeStore, requested, cheap string,
	prices Prices, threshold float64) (KnnView, error) {
	rows, err := store.Recent(identity, KnnMaxRows)
	if err != nil {
		return KnnView{}, err
	}
	cands := KnnCandidates(requested, cheap, "", prices)
	kd := KnnDecide(vec, rows, requested, cands, prices, threshold, KnnK, KnnMinKnown)
	scores := make([]KnnScore, len(kd.Scores))
	for i, s := range kd.Scores {
		v, _ := strconv.ParseFloat(FormatF(s.Score, 4), 64)
		scores[i] = KnnScore{s.Model, v}
	}
	return KnnView{Model: kd.Model, Fallback: kd.Fallback, Reason: kd.Reason, Known: kd.Known,
		Threshold: threshold, Neighbours: kd.Neighbours, Scores: scores}, nil
}

// KnnViewLines is knn_view_lines.
func KnnViewLines(v KnnView) []string {
	if v.Err != "" {
		return []string{"  knn:        " + v.Err}
	}
	reason := strings.TrimPrefix(v.Reason, "knn: ")
	if v.Fallback {
		reason += "; the rules router decides"
	}
	out := []string{"  knn:        " + reason, "  threshold:  " + FormatF(v.Threshold, 2)}
	if len(v.Neighbours) == 0 {
		return append(out, "  neighbours: none at or above the threshold")
	}
	out = append(out, fmt.Sprintf("  neighbours: %d", len(v.Neighbours)))
	for _, n := range v.Neighbours {
		out = append(out, fmt.Sprintf("    %s  %s  %s  %s", n.TurnID, FormatF(n.Similarity, 2), n.Model, n.Outcome))
	}
	return out
}

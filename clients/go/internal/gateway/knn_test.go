package gateway

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daisugi-verify/internal/pyjson"
)

// The golden file is written by clients/knn_golden.py from the Python
// module. The three ports must give its answers.
const knnGolden = "../../../fixtures/gateway/knn_golden.json"

type goldenRow struct {
	Seq      int64     `json:"seq"`
	TurnID   string    `json:"turn_id"`
	Embedder string    `json:"embedder"`
	Vec      []float64 `json:"vec"`
	Model    string    `json:"model"`
	Outcome  string    `json:"outcome"`
	TS       float64   `json:"ts"`
}

type goldenFile struct {
	Prices map[string][2]float64 `json:"prices"`
	Decide []struct {
		Name      string      `json:"name"`
		Query     []float64   `json:"query"`
		Rows      []goldenRow `json:"rows"`
		Requested string      `json:"requested"`
		Cheap     string      `json:"cheap"`
		Local     *string     `json:"local"`
		Threshold float64     `json:"threshold"`
		Expect    struct {
			Candidates []string `json:"candidates"`
			Model      string   `json:"model"`
			Fallback   bool     `json:"fallback"`
			Reason     string   `json:"reason"`
			Known      int      `json:"known"`
			Neighbours []struct {
				TurnID     string  `json:"turn_id"`
				Similarity float64 `json:"similarity"`
				Model      string  `json:"model"`
				Outcome    string  `json:"outcome"`
			} `json:"neighbours"`
			Scores []struct {
				Model string  `json:"model"`
				Score float64 `json:"score"`
			} `json:"scores"`
		} `json:"expect"`
	} `json:"decide"`
	Label []struct {
		Name string `json:"name"`
		Prev struct {
			Outcome  string    `json:"outcome"`
			TS       float64   `json:"ts"`
			Vec      []float64 `json:"vec"`
			Embedder string    `json:"embedder"`
		} `json:"prev"`
		Now       float64   `json:"now"`
		Vec       []float64 `json:"vec"`
		Threshold float64   `json:"threshold"`
		Expect    *string   `json:"expect"`
	} `json:"label"`
	Round6 [][2]float64 `json:"round6"`
	Pack   struct {
		Vec []float64 `json:"vec"`
		Hex string    `json:"hex"`
	} `json:"pack"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(knnGolden)
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func prices(g goldenFile) Prices {
	p := Prices{}
	for k, v := range g.Prices {
		p[k] = Price{v[0], v[1]}
	}
	return p
}

func TestKnnGoldenDecisions(t *testing.T) {
	g := loadGolden(t)
	pr := prices(g)
	for _, c := range g.Decide {
		t.Run(c.Name, func(t *testing.T) {
			var rows []KnnRow
			for _, r := range c.Rows {
				if r.Embedder != "fake" {
					continue
				}
				rows = append(rows, KnnRow{Seq: r.Seq, TurnID: r.TurnID, TS: r.TS, Embedder: r.Embedder,
					Vec: ToF32(r.Vec), Model: r.Model, Outcome: r.Outcome})
			}
			local := ""
			if c.Local != nil {
				local = *c.Local
			}
			cands := KnnCandidates(c.Requested, c.Cheap, local, pr)
			if !equalStrings(cands, c.Expect.Candidates) {
				t.Fatalf("candidates %v, want %v", cands, c.Expect.Candidates)
			}
			d := KnnDecide(ToF32(c.Query), rows, c.Requested, cands, pr, c.Threshold, KnnK, KnnMinKnown)
			if d.Model != c.Expect.Model || d.Fallback != c.Expect.Fallback || d.Reason != c.Expect.Reason ||
				d.Known != c.Expect.Known {
				t.Fatalf("got %q fallback=%v known=%d %q\nwant %q fallback=%v known=%d %q", d.Model, d.Fallback,
					d.Known, d.Reason, c.Expect.Model, c.Expect.Fallback, c.Expect.Known, c.Expect.Reason)
			}
			if len(d.Neighbours) != len(c.Expect.Neighbours) {
				t.Fatalf("%d neighbours, want %d", len(d.Neighbours), len(c.Expect.Neighbours))
			}
			for i, n := range d.Neighbours {
				w := c.Expect.Neighbours[i]
				if n.TurnID != w.TurnID || n.Similarity != w.Similarity || n.Model != w.Model || n.Outcome != w.Outcome {
					t.Fatalf("neighbour %d: got %+v, want %+v", i, n, w)
				}
			}
			if len(d.Scores) != len(c.Expect.Scores) {
				t.Fatalf("%d scores, want %d", len(d.Scores), len(c.Expect.Scores))
			}
			for i, s := range d.Scores {
				w := c.Expect.Scores[i]
				if s.Model != w.Model || math.Abs(s.Score-w.Score) > 5e-7 {
					t.Fatalf("score %d: got %+v, want %+v", i, s, w)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestKnnGoldenLabels(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.Label {
		t.Run(c.Name, func(t *testing.T) {
			prev := &KnnRow{Outcome: c.Prev.Outcome, TS: c.Prev.TS, Vec: ToF32(c.Prev.Vec), Embedder: c.Prev.Embedder}
			got := KnnLabelPrevious(prev, ToF32(c.Vec), "fake", c.Now, c.Threshold)
			want := ""
			if c.Expect != nil {
				want = *c.Expect
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestKnnGoldenRoundAndPack(t *testing.T) {
	g := loadGolden(t)
	for _, p := range g.Round6 {
		if got := Round6(p[0]); got != p[1] {
			t.Errorf("Round6(%v) = %v, want %v", p[0], got, p[1])
		}
	}
	if got := hex.EncodeToString(PackVec(ToF32(g.Pack.Vec))); got != g.Pack.Hex {
		t.Errorf("PackVec = %s, want %s", got, g.Pack.Hex)
	}
	back := UnpackVec(PackVec(ToF32(g.Pack.Vec)))
	for i, x := range back {
		if float64(x) != g.Pack.Vec[i] {
			t.Errorf("UnpackVec[%d] = %v, want %v", i, x, g.Pack.Vec[i])
		}
	}
}

func TestKnnStoreBooksRowsAndKeepsNoText(t *testing.T) {
	st := &OutcomeStore{Path: filepath.Join(t.TempDir(), "gateway", "routing.db")}
	if ok, err := st.SetOutcome("t1", KnnGood, SourceHuman); err != nil || ok {
		t.Fatalf("no table yet: ok=%v err=%v", ok, err)
	}
	lat := 12.5
	id, err := st.Add("c", 5.0, TaskSHA256("secret task"), "fake", []float32{1, 0}, "m", 0.01, &lat, KnnUnknown, SourceAuto)
	if err != nil || id != "t1" {
		t.Fatalf("Add = %q, %v", id, err)
	}
	if ok, err := st.SetOutcome(id, KnnBad, SourceHuman); err != nil || !ok {
		t.Fatalf("SetOutcome: ok=%v err=%v", ok, err)
	}
	if ok, _ := st.SetOutcome("t9", KnnBad, SourceHuman); ok {
		t.Fatal("an unknown turn id was set")
	}
	rows, err := st.Recent("fake", KnnMaxRows)
	if err != nil || len(rows) != 1 || rows[0].Outcome != KnnBad || rows[0].Vec[0] != 1 {
		t.Fatalf("Recent = %+v, %v", rows, err)
	}
	raw, _ := os.ReadFile(st.Path)
	if string(raw) == "" || containsBytes(raw, "secret task") {
		t.Fatal("the task text reached the table")
	}
}

func containsBytes(b []byte, s string) bool {
	return len(s) > 0 && len(b) >= len(s) && indexOf(string(b), s) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func fakeEmbed(vecs map[string][]float32) func(string) ([]float32, string, error) {
	return func(text string) ([]float32, string, error) {
		for k, v := range vecs {
			if len(text) >= len(k) && text[:len(k)] == k {
				return v, "fake", nil
			}
		}
		return nil, "", os.ErrInvalid
	}
}

func knnBody(model, text string) *pyjson.Object {
	msg := pyjson.NewObject().Set("role", "user").Set("content", text)
	return pyjson.NewObject().Set("model", model).Set("max_tokens", pyjson.Int{Text: "64"}).
		Set("messages", []any{msg})
}

func TestKnnGatewayRoutesBooksAndLabels(t *testing.T) {
	dir := t.TempDir()
	store := &OutcomeStore{Path: filepath.Join(dir, "gateway", "routing.db")}
	for i := 0; i < 4; i++ {
		store.Add("old", 0, "s", "fake", []float32{1, 0}, "claude-haiku-4-5", 0, nil, KnnGood, SourceAuto)
	}
	store.Add("old", 0, "s", "fake", []float32{1, 0}, "claude-opus-4-8", 0, nil, KnnGood, SourceAuto)
	knn := &Knn{Store: store, Threshold: 0.25, Embed: fakeEmbed(map[string][]float32{
		"alpha": {1, 0}, "omega": {0, 1}})}
	gw, err := NewGateway(Gateway{RouterMode: ModeKnn, Knn: knn, JournalPath: filepath.Join(dir, "gateway", "turns.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := gw.Prepare(knnBody("claude-opus-4-8", "alpha design the schema"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Decision.Model != "claude-haiku-4-5" || !p.Decision.Downgraded {
		t.Fatalf("decision %+v", p.Decision)
	}
	if want := "knn: claude-haiku-4-5; 5 neighbours, 5 known; scores "; p.Decision.Reason[:len(want)] != want {
		t.Fatalf("reason %q", p.Decision.Reason)
	}
	usage := pyjson.NewObject().Set("input_tokens", pyjson.Int{Text: "1000"}).Set("output_tokens", pyjson.Int{Text: "100"})
	rec, err := gw.Finish(p, usage, "", fixedNow(1000))
	if err != nil || rec.TurnID != "t6" || rec.Reason == "" {
		t.Fatalf("Finish = %+v, %v", rec, err)
	}
	// A far task has no neighbours: the rules router decides and the reason says so.
	far, _ := gw.Prepare(knnBody("claude-opus-4-8", "omega say hi"))
	if want := "knn: 0 of 0 neighbours have a known outcome (need 3); rules router: "; far.Decision.Reason[:len(want)] != want {
		t.Fatalf("far reason %q", far.Decision.Reason)
	}
	// A failed turn is booked bad.
	p2, _ := gw.Prepare(knnBody("claude-opus-4-8", "alpha design the schema two"))
	p2.Failed = true
	rec2, _ := gw.Finish(p2, pyjson.NewObject(), "", fixedNow(2000))
	rows, _ := store.Recent("fake", KnnMaxRows)
	if rows[0].TurnID != rec2.TurnID || rows[0].Outcome != KnnBad {
		t.Fatalf("failed turn row %+v", rows[0])
	}
	if _, err := NewGateway(Gateway{RouterMode: ModeKnn}); err == nil {
		t.Fatal("knn mode without a router was accepted")
	}
}

func fixedNow(s int64) time.Time { return time.Unix(s, 0) }

package server

import (
	"encoding/json"
	"strings"
	"testing"
)

const checksEnvelope = `{"envelope":{"id":"env_1","generated_by":"test","task":"t","permissions":{}},"tokens":50000,"turns":30,` +
	`"checks":{"tests":"pytest -q","lint":"ruff check"}}`

func TestASpawnWithAttemptsBranchesAndTheReportNamesTheCard(t *testing.T) {
	w := newWorkServer(t)
	writeEnvelope(t, "trellis", checksEnvelope)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"leader answer","reasons":[],`+
		`"branches":[{"session":"b1"},{"session":"b2"},{"session":"b3"}],"choice_id":"ch_9"}`)
	t.Setenv("FAKE_RANK_SHOW", `{"card":{"headline":"Kept b1 over b2."}}`)
	got, err := w.toolSpawnBackground2("trellis", "fix it", "", "", false, branchSpec{Attempts: 3, Why: "failed_tests"})
	if err != nil || got != "started trellis-1" {
		t.Fatalf("spawn = %q, %v", got, err)
	}
	msgs := w.waitWork(t, 1)
	want := "[trellis-1] report\nleader answer\n3 branches ran; card ch_9 opened: Kept b1 over b2."
	if len(msgs) != 1 || msgs[0] != want {
		t.Fatalf("work messages = %q, want %q", msgs, want)
	}
	log := read(w.e.log)
	// The checks ride as flags from the operator's file, sorted by name.
	i, j := strings.Index(log, "--check=lint=ruff check"), strings.Index(log, "--check=tests=pytest -q")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("the checks are not flags in order:\n%s", log)
	}
	var req struct {
		Attempts int    `json:"attempts"`
		Why      string `json:"why"`
	}
	for _, l := range strings.Split(log, "\n") {
		if p, ok := strings.CutPrefix(l, "stdin: "); ok {
			if err := json.Unmarshal([]byte(p), &req); err != nil {
				t.Fatal(err)
			}
		}
	}
	if req.Attempts != 3 || req.Why != "failed_tests" {
		t.Fatalf("the request carries attempts %d why %q", req.Attempts, req.Why)
	}
}

func TestASpawnWithoutAttemptsSendsNoBranchFieldsAndNoChecks(t *testing.T) {
	w := newWorkServer(t)
	writeEnvelope(t, "trellis", checksEnvelope)
	t.Setenv("FAKE_TREE_STATUS", `{"status":"ran","session":"s1","answer":"ok","reasons":[]}`)
	if _, err := w.spawn("plain"); err != nil {
		t.Fatal(err)
	}
	w.waitWork(t, 1)
	log := read(w.e.log)
	if strings.Contains(log, "--check") || strings.Contains(log, `"attempts"`) || strings.Contains(log, `"why"`) {
		t.Fatalf("a plain spawn carries branching:\n%s", log)
	}
}

func TestASpawnWithBadBranchingStartsNothing(t *testing.T) {
	w := newWorkServer(t)
	for _, c := range []struct {
		br   branchSpec
		want string
	}{
		{branchSpec{Attempts: 1, Why: "operator"}, "attempts is 2 to 8"},
		{branchSpec{Attempts: 9, Why: "operator"}, "attempts is 2 to 8"},
		{branchSpec{Attempts: 3}, "why is one of"},
		{branchSpec{Attempts: 3, Why: "because"}, "why is one of"},
		{branchSpec{Why: "operator"}, "attempts is 2 to 8"},
	} {
		_, err := w.toolSpawnBackground2("trellis", "x", "", "", false, c.br)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.br, err, c.want)
		}
	}
	if st := w.toolStatus(); st != "no workers" {
		t.Errorf("a refused spawn made a worker: %q", st)
	}
}

func TestStatusCountsTheOpenCardsAndNothingWhenItCannotRead(t *testing.T) {
	w := newWorkServer(t)
	work := w.workDir(w.fcOf())
	if got := w.toolStatusWithCards(work); got != "no workers" {
		t.Fatalf("with no queue to read: %q", got)
	}
	t.Setenv("FAKE_RANK_QUEUE", `{"cards":[{"choice_id":"a"},{"choice_id":"b"}],"decayed":0}`)
	if got := w.toolStatusWithCards(work); got != "no workers\nopen cards: 2" {
		t.Fatalf("status = %q", got)
	}
	t.Setenv("FAKE_RANK_QUEUE", `{"cards":[],"decayed":0}`)
	if got := w.toolStatusWithCards(work); got != "no workers\nopen cards: 0" {
		t.Fatalf("status = %q", got)
	}
}

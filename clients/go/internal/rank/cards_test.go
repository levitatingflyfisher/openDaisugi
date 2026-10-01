package rank

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestSwitchCostCountsTheRunsThatResumedTheCard(t *testing.T) {
	dd := t.TempDir()
	db := filepath.Join(dd, "journal", "index.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o777); err != nil {
		t.Fatal(err)
	}
	con, err := sql.Open("sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE receipts (run_id TEXT, step_id TEXT, reversibility TEXT, timestamp REAL)",
		"INSERT INTO receipts VALUES ('run_b', 'w', 'reversible', 10.0)",
		"INSERT INTO receipts VALUES ('run_c', 'x', 'irreversible', 20.0)",
	} {
		if _, err := con.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	con.Close()
	opened := `{"choice_id": "ch_000000000002", "ranking_id": "r0", "event": "opened", ` +
		`"options": {"survivors": [{"id": "a", "content_hash": "h-a"}, {"id": "b", "content_hash": "h-b"}], "eliminated": []}, ` +
		`"chosen": "a", "status": "provisional", "facts": {"run": {"run_id": "run_a", "step": "t", "downstream": ["w", "x"]}}, "ts": 1e12}`
	resumed := func(run string) string {
		return `{"choice_id": "ch_000000000002", "ranking_id": "r0", "event": "resumed", "run_id": ` + run + `, "ts": 1.0}`
	}
	write := func(rows ...string) *Card {
		d := RankingsDir(dd)
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "choices.jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		cards := ReadCards(dd)
		if len(cards) != 1 {
			t.Fatalf("cards: %d", len(cards))
		}
		return cards[0]
	}
	if c := write(opened); len(c.Resumed) != 0 || SwitchCost(c, dd, "").Cost != Cheap {
		t.Fatalf("no resume: %+v", SwitchCost(c, dd, ""))
	}
	c := write(opened, resumed(`"run_b"`))
	if sc := SwitchCost(c, dd, ""); sc.Cost != Costly || sc.UndoSteps != 1 {
		t.Fatalf("resumed run_b: %+v", sc)
	}
	c = write(opened, resumed(`"run_b"`), resumed(`"run_b"`), resumed(`7`), resumed(`"run_c"`))
	if strings.Join(c.Resumed, ",") != "run_b,run_c" {
		t.Fatalf("resumed: %v", c.Resumed)
	}
	if sc := SwitchCost(c, dd, ""); sc.Cost != FollowUp || sc.FiredAt == nil || *sc.FiredAt != 20.0 {
		t.Fatalf("resumed run_c: %+v", sc)
	}
}

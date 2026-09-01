package tracejournal

import "testing"

func TestListSuccessfulBreaksTiesByRowid(t *testing.T) {
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for _, id := range []string{"b", "a", "c"} {
		if _, err := j.db.Exec("INSERT INTO traces (id, created_at, task, plan_id, envelope_id, ok, duration_ms, "+
			"violations_json) VALUES (?, '2026-01-01T00:00:00Z', 'x', 'p', 'e', 1, 1.0, '[]')", id); err != nil {
			t.Fatal(err)
		}
	}
	ts, err := j.ListSuccessful(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 3 || ts[0].TraceID != "c" || ts[1].TraceID != "a" || ts[2].TraceID != "b" {
		t.Fatalf("%+v", ts)
	}
}

package foreman

import "testing"

// A reader that opens while the writer appends sees one consistent
// snapshot: the view and the messages and nodes it names.
func TestReaderSnapshotWhileWriterAppends(t *testing.T) {
	for _, stage := range []string{"view", "main"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			w := openT(t, dir, &fakeSummarizer{}, newClock(), nil)
			for i := 0; i < 7; i++ {
				mustAppend(t, w, KindUser, "before")
			}
			wantRender, wantCount := w.Render(), w.Count()
			fired := 0
			loadHook = func(s string) {
				if s == stage {
					fired++
					// Seven more: new messages and new tree nodes.
					for i := 0; i < 7; i++ {
						mustAppend(t, w, KindUser, "during")
					}
				}
			}
			defer func() { loadHook = nil }()
			r, err := OpenReadOnly(dir)
			if err != nil {
				t.Fatalf("OpenReadOnly while the writer appends: %v", err)
			}
			if fired == 0 {
				t.Fatal("the hook never ran")
			}
			if r.Count() != wantCount || r.Render() != wantRender {
				t.Fatalf("reader: %d messages, view\n%s\nwant %d,\n%s", r.Count(), r.Render(), wantCount, wantRender)
			}
		})
	}
}

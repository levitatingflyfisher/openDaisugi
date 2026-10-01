package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func spec(root string) Spec {
	return Spec{Tool: "daisugi", Version: "9.9", Roots: map[string]string{"data": root},
		Units: []Unit{{"data", "config.yaml"}, {"data", "journal"}, {"data", "gate/envelopes"}}}
}

func TestRoundTripKeepsAllowedAndDropsSecrets(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "a: 1\n")
	write(t, src+"/journal/index.db", "db")
	write(t, src+"/journal/traces/x.json", "{}")
	write(t, src+"/gate/envelopes/s.json", "{}")
	write(t, src+"/gate/token", "SECRET")
	write(t, src+"/journal/api-token.txt", "SECRET")
	write(t, src+"/journal/gate.lock", "")
	write(t, src+"/keys/id_ed25519", "SECRET")
	r, err := Create(spec(src), out, t0)
	if err != nil || r.Files != 4 {
		t.Fatalf("create: %v %+v", err, r)
	}
	if _, err := Restore(spec(dst), out, false, t0); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config.yaml", "journal/index.db", "journal/traces/x.json", "gate/envelopes/s.json"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	for _, p := range []string{"gate/token", "journal/api-token.txt", "journal/gate.lock", "keys"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("restored %s", p)
		}
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v", st.Mode())
	}
}

func TestRestoreRefusesNonEmptyTargetThenForceMovesAside(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "new\n")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	write(t, dst+"/config.yaml", "old\n")
	write(t, dst+"/keep-me.key", "K")
	_, err := Restore(spec(dst), out, false, t0)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want refusal, got %v", err)
	}
	if b, _ := os.ReadFile(dst + "/config.yaml"); string(b) != "old\n" {
		t.Fatal("refusal changed the target")
	}
	r, err := Restore(spec(dst), out, true, t0)
	if err != nil || len(r.Aside) != 1 {
		t.Fatalf("force: %v %+v", err, r)
	}
	if b, _ := os.ReadFile(dst + "/config.yaml"); string(b) != "new\n" {
		t.Fatal("not restored")
	}
	if b, _ := os.ReadFile(dst + "/config.yaml.broken-20261024T120000Z"); string(b) != "old\n" {
		t.Fatal("old file was not kept aside")
	}
	if _, err := os.Stat(dst + "/keep-me.key"); err != nil {
		t.Fatal("a file off the allow-list was touched")
	}
}

func TestEmptyTargetDirIsEmpty(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/journal/a", "1")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst+"/journal", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(spec(dst), out, false, t0); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRefusesExistingFileAndEmptyData(t *testing.T) {
	src, out := t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	if _, err := Create(spec(src), out, t0); err == nil || !strings.Contains(err.Error(), "nothing to back up") {
		t.Fatalf("empty: %v", err)
	}
	write(t, src+"/config.yaml", "x")
	write(t, out, "precious")
	if _, err := Create(spec(src), out, t0); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("exists: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "precious" {
		t.Fatal("overwrote a file")
	}
}

func TestRestoreRefusesForeignToolAndGarbage(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "x")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	other := spec(dst)
	other.Tool = "coppice"
	if _, err := Restore(other, out, false, t0); err == nil || !strings.Contains(err.Error(), "daisugi backup, not a coppice") {
		t.Fatalf("tool: %v", err)
	}
	junk := filepath.Join(t.TempDir(), "j.tgz")
	write(t, junk, "not gzip")
	if _, err := Restore(spec(dst), junk, false, t0); err == nil {
		t.Fatal("garbage accepted")
	}
}

// evil builds an archive whose manifest lists a path the allow-list refuses.
func evil(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e.tgz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	man := `{"format":1,"tool":"daisugi","version":"1","created":"x","roots":{},"files":[{"path":"` + name + `","size":1,"sha256":"00"}]}`
	tw.WriteHeader(&tar.Header{Name: ManifestName, Size: int64(len(man)), Mode: 0o600, Typeflag: tar.TypeReg})
	tw.Write([]byte(man))
	tw.WriteHeader(&tar.Header{Name: name, Size: 1, Mode: 0o600, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestRestoreRefusesPathsOffTheAllowList(t *testing.T) {
	for _, name := range []string{"data/../../etc/x", "data/keys/id_ed25519", "data/journal/../config.yaml", "/abs", "other/config.yaml", "data/journal/api-token"} {
		dst := t.TempDir()
		if _, err := Restore(spec(dst), evil(t, name), false, t0); err == nil {
			t.Errorf("%s accepted", name)
		}
		if ents, _ := os.ReadDir(dst); len(ents) != 0 {
			t.Errorf("%s wrote something", name)
		}
	}
}

func TestChecksumFailureChangesNothing(t *testing.T) {
	dst := t.TempDir()
	if _, err := Restore(spec(dst), evil(t, "data/config.yaml"), false, t0); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("got %v", err)
	}
	if ents, _ := os.ReadDir(dst); len(ents) != 0 {
		t.Fatal("wrote before verifying")
	}
}

// raw builds an archive from a manifest that lists files and from members
// the test controls, to feed Restore shapes that Create never writes.
type rawMember struct {
	hdr  tar.Header
	body string
}

func raw(t *testing.T, files []Entry, members []rawMember) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raw.tgz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	man, _ := json.Marshal(Manifest{Format: 1, Tool: "daisugi", Version: "1", Created: "x", Roots: map[string]string{}, Files: files})
	tw.WriteHeader(&tar.Header{Name: ManifestName, Size: int64(len(man)), Mode: 0o600, Typeflag: tar.TypeReg})
	tw.Write(man)
	for _, m := range members {
		h := m.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		h.Mode = 0o600
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(m.body))
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func entry(name, body string) Entry {
	sum := sha256.Sum256([]byte(body))
	return Entry{Path: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

func TestRestoreRefusesLinksPaxDuplicatesAndEmptyLists(t *testing.T) {
	e := entry("data/config.yaml", "x")
	reg := rawMember{hdr: tar.Header{Name: "data/config.yaml", Typeflag: tar.TypeReg}, body: "x"}
	cases := map[string]string{
		"symlink":   raw(t, []Entry{e}, []rawMember{{hdr: tar.Header{Name: "data/config.yaml", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}}),
		"hardlink":  raw(t, []Entry{e}, []rawMember{{hdr: tar.Header{Name: "data/config.yaml", Typeflag: tar.TypeLink, Linkname: "data/x"}}}),
		"pax":       raw(t, []Entry{e}, []rawMember{{hdr: tar.Header{Name: "data/config.yaml", Typeflag: tar.TypeReg, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": "hi"}}, body: "x"}}),
		"duplicate": raw(t, []Entry{e}, []rawMember{reg, reg}),
		"no files":  raw(t, nil, nil),
	}
	for name, file := range cases {
		dst := t.TempDir()
		if _, err := Restore(spec(dst), file, false, t0); err == nil {
			t.Errorf("%s accepted", name)
		}
		if ents, _ := os.ReadDir(dst); len(ents) != 0 {
			t.Errorf("%s left something: %v", name, ents)
		}
	}
}

func TestFailedSwapPutsTheOldFilesBack(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "new\n")
	write(t, src+"/journal/a", "new")
	write(t, src+"/gate/envelopes/s.json", "{}")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	write(t, dst+"/config.yaml", "old\n")
	write(t, dst+"/journal/old", "old")
	// "gate" is a file, so gate/envelopes cannot be put in place.
	write(t, dst+"/gate", "a file")
	r, err := Restore(spec(dst), out, true, t0)
	if err == nil || !strings.Contains(err.Error(), "back in place") || !strings.Contains(err.Error(), ".restore-") {
		t.Fatalf("want a rolled-back error naming the staging dir, got %v", err)
	}
	if len(r.Aside) != 0 {
		t.Errorf("aside after a clean rollback: %v", r.Aside)
	}
	for p, want := range map[string]string{"config.yaml": "old\n", "journal/old": "old", "gate": "a file"} {
		if b, _ := os.ReadFile(filepath.Join(dst, p)); string(b) != want {
			t.Errorf("%s = %q", p, b)
		}
	}
	if _, err := os.Stat(dst + "/journal/a"); err == nil {
		t.Error("a restored file was left in place")
	}
	ents, _ := os.ReadDir(dst)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	for _, n := range names {
		if strings.Contains(n, ".broken-") {
			t.Errorf("left %s aside", n)
		}
	}
	if _, err := os.Stat(dst + "/.restore-20261024T120000Z/config.yaml"); err != nil {
		t.Errorf("staging dir was not kept: %v", names)
	}
}

func TestFailedUndoReportsWhatStaysAside(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "new\n")
	write(t, src+"/gate/envelopes/s.json", "{}")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	write(t, dst+"/config.yaml", "old\n")
	write(t, dst+"/gate", "a file")
	// Undo of the move-aside fails too.
	rename = func(a, b string) error {
		if strings.Contains(a, ".broken-") {
			return errors.New("injected")
		}
		return os.Rename(a, b)
	}
	defer func() { rename = os.Rename }()
	r, err := Restore(spec(dst), out, true, t0)
	if err == nil || !strings.Contains(err.Error(), "could not be moved back") {
		t.Fatalf("got %v", err)
	}
	if len(r.Aside) != 1 || !strings.HasSuffix(r.Aside[0], "config.yaml.broken-20261024T120000Z") {
		t.Fatalf("aside: %v", r.Aside)
	}
}

func TestBackupRefusesASymlinkedUnitRoot(t *testing.T) {
	src, real, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "x")
	write(t, real+"/a", "1")
	if err := os.Symlink(real, src+"/journal"); err != nil {
		t.Fatal(err)
	}
	_, err := Create(spec(src), out, t0)
	if err == nil || !strings.Contains(err.Error(), src+"/journal is a symlink") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("wrote an archive")
	}
}

func TestBackupCountsFilesLeftOut(t *testing.T) {
	src, out := t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "x")
	write(t, src+"/journal/token-thrift.md", "n")
	write(t, src+"/journal/a.lock", "")
	r, err := Create(spec(src), out, t0)
	if err != nil || r.Files != 1 || r.Left != 2 {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestBackupStopsWhenAFileChangesUnderTheCopy(t *testing.T) {
	src, out := t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "x")
	afterRead = func(p string) { os.Chtimes(p, t0, t0.Add(time.Hour)) }
	defer func() { afterRead = func(string) {} }()
	_, err := Create(spec(src), out, t0)
	if err == nil || !strings.Contains(err.Error(), "changed while it was copied") {
		t.Fatalf("got %v", err)
	}
	ents, _ := os.ReadDir(filepath.Dir(out))
	if len(ents) != 0 {
		t.Errorf("left %v", ents)
	}
}

func TestSnapshotsGoInInPlaceOfTheLiveFileAndDropTheirSidecars(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/journal/index.db", "live")
	write(t, src+"/journal/index.db-wal", "wal")
	write(t, src+"/journal/index.db-shm", "shm")
	s := spec(src)
	s.Snapshots = []string{"data/journal/index.db"}
	s.Snap = func(from, to string) error { return os.WriteFile(to, []byte("snap of "+filepath.Base(from)), 0o600) }
	r, err := Create(s, out, t0)
	if err != nil || r.Files != 1 || r.Left != 0 {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := Restore(spec(dst), out, false, t0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst + "/journal/index.db"); string(b) != "snap of index.db" {
		t.Fatalf("got %q", b)
	}
	if _, err := os.Stat(dst + "/journal/index.db-wal"); err == nil {
		t.Error("restored a -wal file")
	}
	if _, err := os.Stat(out + ".snap"); err == nil {
		t.Error("left the snapshot dir")
	}
}

func TestACutOrFlippedArchiveChangesNothing(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	var big strings.Builder
	x := uint64(0x2545F4914F6CDD1D)
	for i := 0; i < 1<<20; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		big.WriteByte('a' + byte(x%26))
	}
	write(t, src+"/journal/big.bin", big.String())
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(out)
	cut := filepath.Join(t.TempDir(), "cut.tgz")
	os.WriteFile(cut, good[:len(good)/2], 0o600)
	if _, err := Restore(spec(dst), cut, false, t0); err == nil {
		t.Error("a cut archive was accepted")
	}
	bad := append([]byte(nil), good...)
	bad[len(bad)-6] ^= 0xff // the crc32 in the gzip trailer
	flip := filepath.Join(t.TempDir(), "flip.tgz")
	os.WriteFile(flip, bad, 0o600)
	if _, err := Restore(spec(dst), flip, false, t0); err == nil {
		t.Error("a flipped trailer was accepted")
	}
	if ents, _ := os.ReadDir(dst); len(ents) != 0 {
		t.Errorf("a bad archive left %v", ents)
	}
}

func snapSpec(root string) Spec {
	s := spec(root)
	s.Units = append(s.Units, Unit{"data", "pathways.db"})
	s.Snapshots = []string{"data/pathways.db"}
	s.Snap = func(from, to string) error { return os.WriteFile(to, []byte("snap"), 0o600) }
	return s
}

func TestRestoreMovesAStaleWalAsideWithTheDatabase(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/pathways.db", "new db")
	if _, err := Create(snapSpec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	write(t, dst+"/pathways.db", "old db")
	write(t, dst+"/pathways.db-wal", "stale wal")
	write(t, dst+"/pathways.db-shm", "stale shm")
	r, err := Restore(snapSpec(dst), out, true, t0)
	if err != nil || len(r.Aside) != 3 {
		t.Fatalf("%v %+v", err, r)
	}
	if b, _ := os.ReadFile(dst + "/pathways.db"); string(b) != "snap" {
		t.Fatalf("db = %q", b)
	}
	for _, n := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dst + "/pathways.db" + n); err == nil {
			t.Errorf("stale %s is still beside the database", n)
		}
		if b, _ := os.ReadFile(dst + "/pathways.db" + n + ".broken-20261024T120000Z"); len(b) == 0 {
			t.Errorf("stale %s was not kept aside", n)
		}
	}
	// With no old database and no --force, a stale wal still goes aside.
	dst2 := t.TempDir()
	write(t, dst2+"/pathways.db-wal", "stale wal")
	if _, err := Restore(snapSpec(dst2), out, false, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst2 + "/pathways.db-wal"); err == nil {
		t.Error("stale wal stayed")
	}
}

func TestAFileThatAppearsBeforeTheInstallGoesAside(t *testing.T) {
	src, dst, out := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "b.tgz")
	write(t, src+"/config.yaml", "new\n")
	if _, err := Create(spec(src), out, t0); err != nil {
		t.Fatal(err)
	}
	beforeInstall = func(p string) {
		if strings.HasSuffix(p, "config.yaml") {
			os.WriteFile(p, []byte("late\n"), 0o600)
		}
	}
	defer func() { beforeInstall = func(string) {} }()
	r, err := Restore(spec(dst), out, false, t0)
	if err != nil || len(r.Aside) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if b, _ := os.ReadFile(dst + "/config.yaml"); string(b) != "new\n" {
		t.Errorf("config = %q", b)
	}
	if b, _ := os.ReadFile(r.Aside[0]); string(b) != "late\n" {
		t.Errorf("aside = %q", b)
	}
}

func TestAFailedStageRemovesTheEmptyRootItMade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sub")
	if _, err := Restore(spec(root), evil(t, "data/config.yaml"), false, t0); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Stat(root); err == nil {
		t.Error("the empty root stayed")
	}
	// A root that was there before stays.
	there := t.TempDir()
	Restore(spec(there), evil(t, "data/config.yaml"), false, t0)
	if _, err := os.Stat(there); err != nil {
		t.Error("removed a root it did not make")
	}
}

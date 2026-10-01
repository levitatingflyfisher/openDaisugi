// Package backup writes and restores one tar.gz of a tool's own history and
// settings. The archive holds only paths on an allow-list (Units). Keys,
// tokens, credentials, sockets and locks are never on it, and a restore
// refuses any member that is not on it, whatever the archive says.
//
// Layout of the archive: MANIFEST.json first, then one member per file,
// named "<area>/<path under that area's root>". An area is one root
// directory (the data dir, the config dir). The same format and rules are
// in harness/coppice/internal/backup and in both Rust ports.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rename is os.Rename. A test replaces it to make one step fail.
var rename = os.Rename

// beforeInstall runs just before restore looks at the target of an install
// rename. A test uses it to make a file appear there.
var beforeInstall = func(string) {}

// afterRead runs after member has read a file and before it checks the file
// again. A test uses it to change the file at that moment.
var afterRead = func(string) {}

func isSkip(p string, skip []string) bool {
	for _, k := range skip {
		if p == k {
			return true
		}
	}
	return false
}

// Format is the manifest version this code writes and reads.
const Format = 1

// ManifestName is the first member of every archive.
const ManifestName = "MANIFEST.json"

// Unit is one file or one directory tree on the allow-list: Rel under the
// root of Area.
type Unit struct{ Area, Rel string }

// Spec names a tool, its roots and its allow-list.
type Spec struct {
	Tool    string
	Version string
	// Roots maps an area name to the directory it lives in.
	Roots map[string]string
	Units []Unit
	// Snapshots names members ("area/rel") that Create copies through Snap
	// and not byte by byte: a SQLite file that a live tool writes. The
	// -wal, -shm and -journal files beside it are left out.
	Snapshots []string
	// Snap writes a consistent copy of the file src at dst.
	Snap func(src, dst string) error
}

// Entry is one file in the manifest.
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest says what the archive is.
type Manifest struct {
	Format  int               `json:"format"`
	Tool    string            `json:"tool"`
	Version string            `json:"version"`
	Created string            `json:"created"`
	Roots   map[string]string `json:"roots"`
	Files   []Entry           `json:"files"`
}

// Result is what a run did.
type Result struct {
	Files int
	Bytes int64
	// Aside lists the paths a forced restore moved aside and could not put
	// back. After a clean rollback it is empty.
	Aside []string
	// Left counts the files a backup left out because their names look
	// like secrets or locks.
	Left int
}

// unsafeName is a second guard inside the allow-listed trees: a file whose
// name looks like a lock, a socket, a pid file or a secret is skipped even
// if a tree holds it.
func unsafeName(base string) bool {
	b := strings.ToLower(base)
	for _, suf := range []string{".lock", ".sock", ".pid", ".pem", ".key"} {
		if strings.HasSuffix(b, suf) {
			return true
		}
	}
	for _, sub := range []string{"token", "secret", "credential", "password", "id_rsa", "id_ed25519"} {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return b == "lock"
}

// allowed says whether the member name "<area>/<rel>" is on the
// allow-list. It never accepts "..", an absolute path or an empty part.
func (s Spec) allowed(name string) bool {
	area, rel, ok := strings.Cut(name, "/")
	if !ok || rel == "" {
		return false
	}
	if _, ok := s.Roots[area]; !ok {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsRune(p, 0) {
			return false
		}
	}
	if unsafeName(parts[len(parts)-1]) {
		return false
	}
	for _, u := range s.Units {
		if u.Area == area && (rel == u.Rel || strings.HasPrefix(rel, u.Rel+"/")) {
			return true
		}
	}
	return false
}

// snapshot says whether Create copies the member through Snap.
func (s Spec) snapshot(name string) bool {
	if s.Snap == nil {
		return false
	}
	for _, n := range s.Snapshots {
		if n == name {
			return true
		}
	}
	return false
}

// isSnapshotName says whether name is a member that Create snapshots. It
// does not look at Snap, so a restore knows the names too.
func (s Spec) isSnapshotName(name string) bool {
	for _, n := range s.Snapshots {
		if n == name {
			return true
		}
	}
	return false
}

// sidecar says whether name is a -wal, -shm or -journal file of a member
// that Create snapshots. The snapshot holds what those files hold.
func (s Spec) sidecar(name string) bool {
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(name, suf) && s.snapshot(strings.TrimSuffix(name, suf)) {
			return true
		}
	}
	return false
}

// collect walks the allow-list under the roots and returns member names in
// sorted order, and how many files it left out for their names. Only
// regular files count: links, sockets and devices do not. A link at the
// root of a unit is an error: a backup never follows it.
func (s Spec) collect(skip ...string) ([]string, int, error) {
	var names []string
	left := 0
	for _, u := range s.Units {
		root, ok := s.Roots[u.Area]
		if !ok {
			continue
		}
		base := filepath.Join(root, filepath.FromSlash(u.Rel))
		if st, err := os.Lstat(base); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("%s is a symlink. A backup does not follow links. Back up the real directory, or move it into place", base)
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || isSkip(p, skip) {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			name := u.Area + "/" + filepath.ToSlash(rel)
			switch {
			case s.sidecar(name):
			case s.allowed(name):
				names = append(names, name)
			default:
				left++
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Strings(names)
	out := names[:0]
	for i, n := range names {
		if i == 0 || n != names[i-1] {
			out = append(out, n)
		}
	}
	return out, left, nil
}

func (s Spec) path(name string) string {
	area, rel, _ := strings.Cut(name, "/")
	return filepath.Join(s.Roots[area], filepath.FromSlash(rel))
}

// syncDir makes the entries of a directory durable.
func syncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func changed(name string) error {
	return fmt.Errorf("%s changed while it was copied. Stop the tool and try again", name)
}

// member reads src once, writes it into tw as name, and hashes it on the
// way. It fails if the size or the modification time of the file differs
// before and after the read.
func member(tw *tar.Writer, name, src string, now time.Time) (Entry, error) {
	f, err := os.Open(src)
	if err != nil {
		return Entry{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	if !before.Mode().IsRegular() {
		return Entry{}, changed(name)
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: before.Size(), Mode: 0o600,
		ModTime: now.UTC(), Format: tar.FormatUSTAR}
	if err := tw.WriteHeader(hdr); err != nil {
		return Entry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(tw, io.TeeReader(io.LimitReader(f, before.Size()), h))
	if err != nil {
		return Entry{}, err
	}
	afterRead(src)
	after, err := f.Stat()
	if err != nil {
		return Entry{}, err
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return Entry{}, changed(name)
	}
	return Entry{Path: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Create writes the archive to out. It refuses to replace a file that is
// already there. It reads each file once, hashing as it writes, and stops
// if a file changed under the read. A SQLite file named in Snapshots is
// copied through Snap first, so a live tool cannot tear it.
func Create(s Spec, out string, now time.Time) (Result, error) {
	var r Result
	if _, err := os.Lstat(out); err == nil {
		return r, fmt.Errorf("%s already exists. Name another file", out)
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return r, err
	}
	tmp, bodyPath, snapDir := out+".partial", out+".body", out+".snap"
	names, left, err := s.collect(abs, abs+".partial", abs+".body")
	if err != nil {
		return r, err
	}
	if len(names) == 0 {
		return r, errors.New("nothing to back up: none of the files a backup holds exist yet")
	}
	r.Left = left
	// The members go into a body file first, because the manifest leads the
	// archive and holds their hashes.
	bf, err := os.OpenFile(bodyPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	var af *os.File
	done := false
	defer func() {
		bf.Close()
		os.Remove(bodyPath)
		os.RemoveAll(snapDir)
		if af != nil && !done {
			af.Close()
			os.Remove(tmp)
		}
	}()
	body := tar.NewWriter(bf)
	m := Manifest{Format: Format, Tool: s.Tool, Version: s.Version,
		Created: now.UTC().Format(time.RFC3339), Roots: s.Roots}
	for i, n := range names {
		src := s.path(n)
		if s.snapshot(n) {
			if err := os.MkdirAll(snapDir, 0o700); err != nil {
				return Result{}, err
			}
			dst := filepath.Join(snapDir, strconv.Itoa(i))
			if err := s.Snap(src, dst); err != nil {
				return Result{}, fmt.Errorf("cannot copy %s: %v", src, err)
			}
			src = dst
		}
		e, err := member(body, n, src, now)
		if err != nil {
			return Result{}, err
		}
		m.Files = append(m.Files, e)
		r.Files++
		r.Bytes += e.Size
	}
	if err := body.Flush(); err != nil {
		return Result{}, err
	}
	if _, err := bf.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	af, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, err
	}
	gz := gzip.NewWriter(af)
	tw := tar.NewWriter(gz)
	mb, _ := json.MarshalIndent(m, "", "  ")
	mb = append(mb, '\n')
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: ManifestName, Size: int64(len(mb)), Mode: 0o600,
		ModTime: now.UTC(), Format: tar.FormatUSTAR}
	if err := tw.WriteHeader(hdr); err != nil {
		return Result{}, err
	}
	if _, err := tw.Write(mb); err != nil {
		return Result{}, err
	}
	if err := tw.Flush(); err != nil {
		return Result{}, err
	}
	// The body already holds whole tar blocks. Copy them as they are, then
	// write the two empty blocks that end the archive.
	if _, err := io.Copy(gz, bf); err != nil {
		return Result{}, err
	}
	if _, err := gz.Write(make([]byte, 1024)); err != nil {
		return Result{}, err
	}
	if err := gz.Close(); err != nil {
		return Result{}, err
	}
	if err := af.Sync(); err != nil {
		return Result{}, err
	}
	if err := af.Close(); err != nil {
		return Result{}, err
	}
	if err := rename(tmp, out); err != nil {
		return Result{}, err
	}
	done = true
	_ = syncDir(filepath.Dir(abs))
	return r, nil
}

func damaged(file string, err error) error { return fmt.Errorf("%s is damaged: %v", file, err) }

// readManifest reads the first member of the archive and checks that it is
// the manifest of the right tool, and that every file it lists is on the
// allow-list.
func (s Spec) readManifest(file string, tr *tar.Reader) (*Manifest, error) {
	h, err := tr.Next()
	if err == io.EOF {
		return nil, fmt.Errorf("%s is empty. It is not a %s backup", file, s.Tool)
	}
	if err != nil {
		return nil, damaged(file, err)
	}
	if h.Name != ManifestName {
		return nil, fmt.Errorf("%s has no manifest first. It is not a %s backup", file, s.Tool)
	}
	b, err := io.ReadAll(io.LimitReader(tr, 64<<20))
	if err != nil {
		return nil, damaged(file, err)
	}
	m := new(Manifest)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s has a manifest that does not read: %v", file, err)
	}
	if m.Tool != s.Tool {
		return nil, fmt.Errorf("%s is a %s backup, not a %s backup", file, m.Tool, s.Tool)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("%s has backup format %d. This build reads format %d", file, m.Format, Format)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("%s lists no files. There is nothing to restore. Nothing was changed", file)
	}
	for _, e := range m.Files {
		if !s.allowed(e.Path) {
			return nil, fmt.Errorf("%s lists %q, which a %s backup never holds. Nothing was changed", file, e.Path, s.Tool)
		}
	}
	return m, nil
}

// ReadManifest returns the manifest of an archive after it checks that
// the archive holds the right tool's files.
func (s Spec) ReadManifest(file string) (*Manifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%s is not a gzip file: %v", file, err)
	}
	return s.readManifest(file, tar.NewReader(gz))
}

// stamp is the UTC time in the .broken-<time> style.
func stamp(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// existing lists the allow-listed paths that are already on disk and not
// empty.
func (s Spec) existing() []string {
	var out []string
	for _, u := range s.Units {
		root, ok := s.Roots[u.Area]
		if !ok {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(u.Rel))
		if st, err := os.Lstat(p); err == nil {
			if st.IsDir() {
				if ents, err := os.ReadDir(p); err != nil || len(ents) == 0 {
					continue
				}
			}
			out = append(out, p)
		}
	}
	return out
}

// freeName is base + suffix, or base + suffix + "-2", "-3" and so on, the
// first that is not on disk.
func freeName(base string) string {
	dst := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			return dst
		}
		dst = fmt.Sprintf("%s-%d", base, i)
	}
}

// stager holds the staging directories, one beside each root. They are
// named <root>/.restore-<time>, so a rename from one to the other stays on
// one file system.
type stager struct {
	s    Spec
	now  time.Time
	dirs map[string]string // root -> staging dir
	made []string          // roots this restore made
}

func (g *stager) dir(root string) (string, error) {
	if d, ok := g.dirs[root]; ok {
		return d, nil
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		g.made = append(g.made, root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	d := freeName(filepath.Join(root, ".restore-"+stamp(g.now)))
	if err := os.Mkdir(d, 0o700); err != nil {
		return "", err
	}
	g.dirs[root] = d
	return d, nil
}

// discard removes the staging directories. They hold only copies of
// archive members.
func (g *stager) discard() {
	for _, d := range g.dirs {
		os.RemoveAll(d)
	}
	// A root this restore made stays only if something is in it now.
	for _, r := range g.made {
		os.Remove(r)
	}
}

func (g *stager) list() string {
	var ds []string
	for _, d := range g.dirs {
		ds = append(ds, d)
	}
	sort.Strings(ds)
	return strings.Join(ds, ", ")
}

// stage reads every member once. It checks the name, the type, the manifest
// and the hash while it writes the member into a staging directory. It
// changes nothing else.
func (g *stager) stage(file string, tr *tar.Reader, rest io.Reader, m *Manifest) (files int, bytes int64, err error) {
	want := map[string]Entry{}
	for _, e := range m.Files {
		want[e.Path] = e
	}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, 0, fmt.Errorf("%s is damaged: %v. Nothing was changed", file, err)
		}
		e, ok := want[h.Name]
		if !ok || !g.s.allowed(h.Name) || h.Typeflag != tar.TypeReg || len(h.PAXRecords) > 0 || seen[h.Name] {
			return 0, 0, fmt.Errorf("%s holds %q, which the manifest does not list. Nothing was changed", file, h.Name)
		}
		seen[h.Name] = true
		area, rel, _ := strings.Cut(h.Name, "/")
		sd, err := g.dir(g.s.Roots[area])
		if err != nil {
			return 0, 0, err
		}
		dst := filepath.Join(sd, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return 0, 0, err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return 0, 0, err
		}
		hh := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, hh), tr)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return 0, 0, fmt.Errorf("%s is damaged: %v. Nothing was changed", file, err)
		}
		if n != e.Size || hex.EncodeToString(hh.Sum(nil)) != e.SHA256 {
			return 0, 0, fmt.Errorf("%s fails its checksum in %s. Nothing was changed", file, h.Name)
		}
		files++
		bytes += n
	}
	if len(seen) != len(want) {
		return 0, 0, fmt.Errorf("%s lacks files its manifest lists. Nothing was changed", file)
	}
	// The tar ends before the gzip trailer. Read on, so its checksum is checked.
	if _, err := io.Copy(io.Discard, rest); err != nil {
		return 0, 0, fmt.Errorf("%s is damaged: %v. Nothing was changed", file, err)
	}
	return files, bytes, nil
}

type step struct{ from, to string }

// asideIfThere moves p to p.broken-<time> when something is at p, and records
// the move for the undo.
func asideIfThere(p string, now time.Time, mv func(a, b string) error, done *[]step, aside *[]string) error {
	if _, err := os.Lstat(p); err != nil {
		return nil
	}
	dst := freeName(p + ".broken-" + stamp(now))
	if err := mv(p, dst); err != nil {
		return fmt.Errorf("cannot move %s aside: %v", p, err)
	}
	*done = append(*done, step{p, dst})
	*aside = append(*aside, dst)
	return nil
}

// Restore puts the files of an archive back. It reads the archive once and
// writes every member into a staging directory next to its root, checking
// name, type and hash as it goes. Nothing live changes until all members
// are staged and good. A target that already holds any allow-listed file
// is refused unless force is set. Then each old path moves aside to
// <name>.broken-<time> (never deleted) and the staged path takes its
// place, by rename. If a rename fails, the renames done so far are undone
// in reverse order and the staging directory stays.
func Restore(s Spec, file string, force bool, now time.Time) (Result, error) {
	var r Result
	f, err := os.Open(file)
	if err != nil {
		return r, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return r, fmt.Errorf("%s is not a gzip file: %v", file, err)
	}
	tr := tar.NewReader(gz)
	m, err := s.readManifest(file, tr)
	if err != nil {
		return r, err
	}
	have := s.existing()
	if len(have) > 0 && !force {
		more := ""
		if len(have) > 1 {
			more = fmt.Sprintf(" and %d more", len(have)-1)
		}
		return r, fmt.Errorf("%s%s already hold data. Restore with --force to move it aside and continue", have[0], more)
	}
	g := &stager{s: s, now: now, dirs: map[string]string{}}
	files, bytes, err := g.stage(file, tr, gz, m)
	if err != nil {
		g.discard()
		return r, err
	}
	haveSet := map[string]bool{}
	for _, p := range have {
		haveSet[p] = true
	}
	var done []step
	var aside []string
	fail := func(err error) (Result, error) {
		var stuck []string
		for i := len(done) - 1; i >= 0; i-- {
			if rerr := rename(done[i].to, done[i].from); rerr != nil {
				stuck = append(stuck, done[i].to)
			}
		}
		for _, a := range aside {
			for _, p := range stuck {
				if p == a {
					r.Aside = append(r.Aside, a)
				}
			}
		}
		msg := fmt.Sprintf("%v. The restored files stay in %s", err, g.list())
		if len(stuck) == 0 {
			return r, fmt.Errorf("%s. The old data is back in place", msg)
		}
		return r, fmt.Errorf("%s. These could not be moved back: %s", msg, strings.Join(stuck, ", "))
	}
	var installed []string
	for _, u := range s.Units {
		root, ok := s.Roots[u.Area]
		if !ok {
			continue
		}
		final := filepath.Join(root, filepath.FromSlash(u.Rel))
		if haveSet[final] {
			dst := freeName(final + ".broken-" + stamp(now))
			if err := rename(final, dst); err != nil {
				return fail(fmt.Errorf("cannot move %s aside: %v", final, err))
			}
			done = append(done, step{final, dst})
			aside = append(aside, dst)
		}
		sd, ok := g.dirs[root]
		if !ok {
			continue
		}
		staged := filepath.Join(sd, filepath.FromSlash(u.Rel))
		if _, err := os.Lstat(staged); err != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
			return fail(err)
		}
		// A SQLite file must not meet an old -wal, -shm or -journal file: SQLite
		// could replay it onto the restored file. Move those aside too.
		if s.isSnapshotName(u.Area + "/" + u.Rel) {
			for _, suf := range []string{"-wal", "-shm", "-journal"} {
				if err := asideIfThere(final+suf, now, rename, &done, &aside); err != nil {
					return fail(err)
				}
			}
		}
		// Something may have appeared at the target since the first look. Move
		// it aside too. An empty directory holds nothing; Go does not rename
		// onto one, so it goes.
		beforeInstall(final)
		if st, err := os.Lstat(final); err == nil {
			if st.IsDir() {
				if ents, _ := os.ReadDir(final); len(ents) == 0 {
					if err := os.Remove(final); err != nil {
						return fail(fmt.Errorf("cannot put %s in place: %v", final, err))
					}
				}
			}
			if err := asideIfThere(final, now, rename, &done, &aside); err != nil {
				return fail(err)
			}
		}
		if err := rename(staged, final); err != nil {
			return fail(fmt.Errorf("cannot put %s in place: %v", final, err))
		}
		done = append(done, step{staged, final})
		installed = append(installed, final)
	}
	// Everything is in place. Make it durable, then drop the staging.
	for _, p := range installed {
		_ = syncDir(filepath.Dir(p))
	}
	for root := range g.dirs {
		_ = syncDir(root)
	}
	g.discard()
	r.Files, r.Bytes, r.Aside = files, bytes, aside
	return r, nil
}

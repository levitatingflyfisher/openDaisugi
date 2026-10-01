package pack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// The bundle tar of opendaisugi.pack.ustar: the same bytes as Python
// writes, and the same reader rules.

const block = 512

// File is one bundle member.
type File struct {
	Name string
	Data []byte
}

func octal(n int64, width int) []byte {
	s := fmt.Sprintf("%o", n)
	for len(s) < width-1 {
		s = "0" + s
	}
	return append([]byte(s), 0)
}

func splitName(name string) (prefix, short []byte, err error) {
	raw := []byte(name)
	if len(raw) <= 100 {
		return nil, raw, nil
	}
	for i := len(raw) - 1; i > 0; i-- {
		if raw[i] == '/' && i <= 155 && len(raw)-i-1 <= 100 {
			return raw[:i], raw[i+1:], nil
		}
	}
	return nil, nil, fmt.Errorf("a name too long for a ustar header: %s", name)
}

func fixChecksum(h []byte) {
	copy(h[148:156], "        ")
	sum := 0
	for _, b := range h {
		sum += int(b)
	}
	s := fmt.Sprintf("%o", sum)
	for len(s) < 6 {
		s = "0" + s
	}
	copy(h[148:156], append([]byte(s), 0, ' '))
}

const paxName = "././@PaxHeader"

func rawBlock(prefix, short []byte, size int64, kind byte) []byte {
	h := make([]byte, block)
	copy(h[0:], short)
	copy(h[100:], octal(0o644, 8))
	copy(h[108:], octal(0, 8))
	copy(h[116:], octal(0, 8))
	copy(h[124:], octal(size, 12))
	copy(h[136:], octal(0, 12))
	h[156] = kind
	copy(h[257:], "ustar\x00")
	copy(h[263:], "00")
	copy(h[345:], prefix)
	fixChecksum(h)
	return h
}

// paxRecord is ustar.pax_record.
func paxRecord(key, value string) []byte {
	body := " " + key + "=" + value + "\n"
	n := len(body) + 1
	for {
		rec := fmt.Sprintf("%d%s", n, body)
		if len(rec) == n {
			return []byte(rec)
		}
		n = len(rec)
	}
}

// header is ustar.header: after an extended header when the name does
// not fit the ustar fields.
func header(name string, size int64) ([]byte, error) {
	prefix, short, err := splitName(name)
	if err == nil {
		return rawBlock(prefix, short, size, '0'), nil
	}
	rec := paxRecord("path", name)
	out := rawBlock(nil, []byte(paxName), int64(len(rec)), 'x')
	out = append(out, rec...)
	out = append(out, pad(int64(len(rec)))...)
	raw := []byte(name)
	if len(raw) > 100 {
		raw = raw[:100]
	}
	return append(out, rawBlock(nil, raw, size, '0')...), nil
}

// paxPath is ustar.pax_path.
func paxPath(body []byte) (string, bool) {
	path, found := "", false
	for len(body) > 0 {
		sp := bytes.IndexByte(body, ' ')
		if sp <= 0 {
			break
		}
		var n int
		if _, err := fmt.Sscanf(string(body[:sp]), "%d", &n); err != nil || n <= sp || n > len(body) {
			break
		}
		rec := bytes.TrimRight(body[sp+1:n], "\n")
		key, value, _ := bytes.Cut(rec, []byte("="))
		if string(key) == "path" {
			path, found = strings.ToValidUTF8(string(value), "\uFFFD"), true
		}
		body = body[n:]
	}
	return path, found
}

func pad(n int64) []byte { return make([]byte, (block-n%block)%block) }

// UstarWrite is ustar.write: the files sorted by name.
func UstarWrite(files []File) ([]byte, error) {
	sorted := append([]File{}, files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out bytes.Buffer
	for _, f := range sorted {
		h, err := header(f.Name, int64(len(f.Data)))
		if err != nil {
			return nil, err
		}
		out.Write(h)
		out.Write(f.Data)
		out.Write(pad(int64(len(f.Data))))
	}
	out.Write(make([]byte, 2*block))
	return out.Bytes(), nil
}

// UstarWriteFile is ustar.write_file: (name, source path) pairs written
// one at a time.
func UstarWriteFile(out string, names, sources []string) error {
	idx := make([]int, len(names))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return names[idx[a]] < names[idx[b]] })
	fh, err := os.Create(out)
	if err != nil {
		return err
	}
	defer fh.Close()
	for _, i := range idx {
		st, err := os.Stat(sources[i])
		if err != nil {
			return err
		}
		h, err := header(names[i], st.Size())
		if err != nil {
			return err
		}
		if _, err := fh.Write(h); err != nil {
			return err
		}
		src, err := os.Open(sources[i])
		if err != nil {
			return err
		}
		_, err = io.Copy(fh, src)
		src.Close()
		if err != nil {
			return err
		}
		if _, err := fh.Write(pad(st.Size())); err != nil {
			return err
		}
	}
	if _, err := fh.Write(make([]byte, 2*block)); err != nil {
		return err
	}
	return fh.Close()
}

func field(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i]
	}
	return b
}

var errShort = errors.New("a tar archive cut short")

// ustarEntries calls each(name, size, r) for each regular file; each must
// read exactly size bytes from r.
func ustarEntries(r io.Reader, each func(name string, size int64, r io.Reader) error) error {
	h := make([]byte, block)
	longPath, haveLong := "", false
	for {
		n, err := io.ReadFull(r, h)
		if n < block || err != nil || bytes.Equal(h, make([]byte, block)) {
			return nil
		}
		sizeText := strings.TrimSpace(string(field(h[124:136])))
		var size int64
		if sizeText != "" {
			if _, err := fmt.Sscanf(sizeText, "%o", &size); err != nil {
				return errors.New("a tar header with a bad size")
			}
		}
		name := field(h[0:100])
		if string(h[257:262]) == "ustar" {
			if prefix := field(h[345:500]); len(prefix) > 0 {
				name = append(append(append([]byte{}, prefix...), '/'), name...)
			}
		}
		text := strings.ToValidUTF8(string(name), "�")
		padding := (block - size%block) % block
		if h[156] == 'x' || h[156] == 'g' {
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return errShort
			}
			if _, err := io.CopyN(io.Discard, r, padding); err != nil {
				return errShort
			}
			if h[156] == 'x' {
				longPath, haveLong = paxPath(body)
			}
			continue
		}
		if haveLong {
			text, longPath, haveLong = longPath, "", false
		}
		switch h[156] {
		case '5':
			if _, err := io.CopyN(io.Discard, r, size+padding); err != nil {
				return errShort
			}
			continue
		case '0', 0:
		default:
			return fmt.Errorf("a tar entry that is not a file: %s", text)
		}
		parts := strings.Split(text, "/")
		escape := text == "" || strings.HasPrefix(text, "/")
		for _, p := range parts {
			if p == ".." {
				escape = true
			}
		}
		if escape {
			return fmt.Errorf("a name outside the bundle: %s", text)
		}
		if err := each(text, size, r); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, r, padding); err != nil {
			return errShort
		}
	}
}

// UstarRead is ustar.read.
func UstarRead(data []byte) ([]File, error) {
	var out []File
	err := ustarEntries(bytes.NewReader(data), func(name string, size int64, r io.Reader) error {
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil {
			return errShort
		}
		out = append(out, File{name, b})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// UstarExtract is ustar.extract.
func UstarExtract(tarPath, dest string) error {
	fh, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer fh.Close()
	return ustarEntries(fh, func(name string, size int64, r io.Reader) error {
		p := filepath.Join(dest, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			return err
		}
		out, err := os.Create(p)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, r, size); err != nil {
			out.Close()
			return errShort
		}
		return out.Close()
	})
}

// stop is manage.Stop: lines to print, then exit 1.
type stop struct{ lines []string }

func (s *stop) Error() string { return strings.Join(s.lines, "\n") }

func stopf(format string, a ...any) *stop { return &stop{[]string{fmt.Sprintf(format, a...)}} }

// unpackTarGz is manage.unpack_python's walk: every entry a file, a
// directory or a symlink under python/, every link inside dest.
func unpackTarGz(data []byte, dest, label string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return stopf("%s is not a gzip tar archive.", label)
	}
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return stopf("%s is not a gzip tar archive.", label)
		}
		name := strings.TrimSuffix(h.Name, "/")
		parts := strings.Split(name, "/")
		bad := strings.HasPrefix(name, "/") || parts[0] != "python"
		for _, p := range parts {
			if p == ".." {
				bad = true
			}
		}
		if bad {
			return stopf("%s holds an entry outside python/: %s", label, name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return stopf("%s is not a gzip tar archive.", label)
			}
			if err := out.Close(); err != nil {
				return err
			}
			if err := os.Chmod(target, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := h.Linkname
			where := path.Clean(path.Join(filepath.ToSlash(root), path.Dir(name), link))
			if strings.HasPrefix(link, "/") || !strings.HasPrefix(where+"/", filepath.ToSlash(root)+"/") {
				return stopf("%s holds a link outside the pack: %s", label, name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return stopf("%s holds an entry that is not a file or a link: %s", label, name)
		}
	}
	return nil
}

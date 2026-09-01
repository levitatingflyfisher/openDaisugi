package potion

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultModel is _search._POTION_DEFAULT, the identity pathways embedded
// with the default model carry.
const DefaultModel = "minishlab/potion-base-8M"

// Threshold is _search._POTION_THRESHOLD, FPR-matched in ADR-0018.
const Threshold = 0.59

// The default model's files, pinned to one revision of the official
// repository. model.safetensors's sha256 is the LFS oid the Hugging Face
// tree API reports; the two small files are below the LFS threshold, so
// theirs is taken from the downloaded bytes (and each file's git blob
// id matches the tree's oid). The model is MIT licensed (its model card).
const (
	pinnedRevision = "bf8b056651a2c21b8d2565580b8569da283cab23"
	pinnedBase     = "https://huggingface.co/minishlab/potion-base-8M/resolve/" + pinnedRevision + "/"
)

type pinnedFile struct {
	name   string
	sha256 string
	size   int64
}

var pinnedFiles = []pinnedFile{
	{"config.json", "2a6ac0e9aaa356a68a5688070db78fc3a464fefe85d2f06a1905ce3718687553", 202},
	{"tokenizer.json", "e67e803f624fb4d67dea1c730d06e1067e1b14d830e2c2202569e3ef0f70bb50", 683_666},
	{"model.safetensors", "f65d0f325faadc1e121c319e2faa41170d3fa07d8c89abd48ca5358d9a223de2", 30_236_760},
}

// ErrNotAvailable is MatcherNotAvailable: the operator chose potion and
// its model cannot be had. It is never a fallback to another matcher.
var ErrNotAvailable = errors.New("potion model not available")

// Env is what model resolution reads from the process.
type Env struct {
	Lookup func(string) (string, bool)
	Home   string
	// Notice gets the one line said before a download starts.
	Notice func(string)
	// NoFetch refuses to download; a missing model is then not available.
	NoFetch bool
	Client  *http.Client
}

// ResolveModel is resolve_potion_model(): OPENDAISUGI_POTION_MODEL, or
// the default id. It is the identity pathways are stamped with.
func ResolveModel(lookup func(string) (string, bool)) string {
	if v, ok := lookup("OPENDAISUGI_POTION_MODEL"); ok {
		return v
	}
	return DefaultModel
}

// CacheDir is where the pinned files live: $XDG_CACHE_HOME (or
// ~/.cache)/opendaisugi/models/potion-base-8M.
func CacheDir(e Env) string {
	base, _ := e.Lookup("XDG_CACHE_HOME")
	if !filepath.IsAbs(base) {
		base = filepath.Join(e.Home, ".cache")
	}
	return filepath.Join(base, "opendaisugi", "models", "potion-base-8M")
}

// Open loads the model an identity names. A local directory is read as
// it is. The default id is read from the cache, fetched first (with one
// notice line) when a pinned file is missing or does not match its hash.
// Any other Hugging Face id is not available: this binary fetches only
// pinned files.
func Open(identity string, e Env) (*Model, error) {
	if st, err := os.Stat(identity); err == nil && st.IsDir() {
		m, err := LoadDir(identity)
		if err != nil {
			return nil, fmt.Errorf("%w: potion model %q could not be loaded (%v)", ErrNotAvailable, identity, err)
		}
		return m, nil
	}
	if identity != DefaultModel {
		return nil, fmt.Errorf("%w: potion model %q is not a local directory, and this binary fetches only the pinned %s",
			ErrNotAvailable, identity, DefaultModel)
	}
	dir := CacheDir(e)
	for _, f := range pinnedFiles {
		if err := fetch(pinnedBase+f.name, f, filepath.Join(dir, f.name), e); err != nil {
			return nil, fmt.Errorf("%w: potion model %q could not be fetched: %v. Pre-fetch it and set "+
				"OPENDAISUGI_POTION_MODEL=<local dir>, or set matcher_model: lexical for a zero-download matcher",
				ErrNotAvailable, identity, err)
		}
	}
	// The bytes loaded are hashed again: a file changed after fetch
	// checked it is refused, not used.
	data := make([][]byte, len(pinnedFiles))
	for i, f := range pinnedFiles {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotAvailable, err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f.sha256 {
			return nil, fmt.Errorf("%w: %s changed after it was checked", ErrNotAvailable, f.name)
		}
		data[i] = b
	}
	m, err := Load(data[0], data[1], data[2])
	if err != nil {
		return nil, fmt.Errorf("%w: potion files in %s could not be loaded: %v", ErrNotAvailable, dir, err)
	}
	return m, nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetch is _model_fetch.fetch: a file that matches its hash is used as it
// is; any other is removed and downloaded again into <dest>.part, checked,
// then renamed into place.
func fetch(url string, f pinnedFile, dest string, e Env) error {
	if got, err := sha256File(dest); err == nil && got == f.sha256 {
		return nil
	}
	if e.NoFetch {
		return fmt.Errorf("%s is missing or does not match its pinned sha256", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dest)
	if e.Notice != nil {
		note := ""
		if f.size >= 1<<20 {
			note = fmt.Sprintf(" of about %dMB", f.size>>20)
		}
		e.Notice(fmt.Sprintf("fetching %s%s from %s ...", filepath.Base(dest), note, url))
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// A temporary file of its own, so two processes fetching at once
	// never write into one file; each renames a checked file into place.
	out, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".part-*")
	if err != nil {
		return err
	}
	part := out.Name()
	defer os.Remove(part)
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.sha256 {
		return fmt.Errorf("%s: sha256 %s, not the pinned %s", filepath.Base(dest), got, f.sha256)
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return err
	}
	return os.Rename(part, dest)
}

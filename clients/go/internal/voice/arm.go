package voice

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// Quote is urllib.parse.quote(s, safe=""): every byte of the UTF-8 text
// percent-encoded but ASCII letters, digits and "_.-~".
func Quote(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ArmedName is the grant file name of a pane key.
func ArmedName(paneKey string) string { return Quote(paneKey) + ".json" }

// ArmEntry is deliver.ArmEntry.
type ArmEntry struct {
	PaneKey   string
	ArmedAt   float64
	ExpiresAt float64
}

// Arm is deliver.arm: the grant file written at 0600 in a directory made
// at 0700 when it is new. It replaces an older grant for the same key.
func Arm(paneKey string, minutes float64, armedDir string, now float64) (*ArmEntry, error) {
	return ArmRaw(paneKey, minutes, armedDir, now, nil)
}

// ArmRaw is Arm with armed_at written as nowRaw when it is set: an int
// clock stays an int in the file, as Python keeps it.
func ArmRaw(paneKey string, minutes float64, armedDir string, now float64, nowRaw any) (*ArmEntry, error) {
	if err := mkdirAll(armedDir); err != nil {
		return nil, err
	}
	e := &ArmEntry{PaneKey: paneKey, ArmedAt: now, ExpiresAt: now + minutes*60.0}
	o := pyjson.NewObject()
	o.Set("pane_key", paneKey)
	if nowRaw != nil {
		o.Set("armed_at", nowRaw)
	} else {
		o.Set("armed_at", e.ArmedAt)
	}
	o.Set("expires_at", e.ExpiresAt)
	p := filepath.Join(armedDir, ArmedName(paneKey))
	enc, xerr := pystr.EncodeUTF8(pyjson.Dumps(o, true))
	if xerr != nil {
		return nil, errors.New(xerr.Msg)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(enc); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return nil, err
	}
	return e, nil
}

// mkdirAll is Path.mkdir(mode=0o700, parents=True, exist_ok=True): the
// parents made with the default mode, the last at 0700 under the umask,
// and an existing directory left as it is.
func mkdirAll(dir string) error {
	if st, err := os.Stat(dir); err == nil {
		if st.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: dir, Err: fs.ErrExist}
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := os.MkdirAll(parent, 0o777); err != nil {
			return err
		}
	}
	err := os.Mkdir(dir, 0o700)
	if err != nil {
		if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
			return nil
		}
	}
	return err
}

// Disarm is deliver.disarm: true when a grant file was there and is gone.
func Disarm(paneKey, armedDir string) (bool, error) {
	p := filepath.Join(armedDir, ArmedName(paneKey))
	if _, err := os.Stat(p); err != nil {
		if pathAbsent(err) {
			return false, nil
		}
		return false, err
	}
	// Path.unlink: a directory is IsADirectoryError, never removed.
	if err := syscall.Unlink(p); err != nil {
		return false, &fs.PathError{Op: "unlink", Path: p, Err: err}
	}
	return true, nil
}

// pathAbsent is the set of stat errors Path.exists() reads as False.
func pathAbsent(err error) bool {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		switch pe.Err.Error() {
		case "no such file or directory", "not a directory", "bad file descriptor",
			"too many levels of symbolic links":
			return true
		}
	}
	return false
}

// IsArmed is deliver.is_armed: a readable grant whose expires_at, read as
// float() reads it, is after now. Every failure reads as unarmed.
func IsArmed(paneKey, armedDir string, now float64) bool {
	p := filepath.Join(armedDir, ArmedName(paneKey))
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	text, xerr := pystr.DecodeStrict(raw)
	if xerr != nil {
		return false
	}
	v, derr := pyjson.LoadsPy(text, 900)
	if derr != nil {
		return false
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return false
	}
	raw2, ok := obj.Get("expires_at")
	if !ok {
		return false
	}
	exp, ok := pyFloatValue(raw2)
	if !ok {
		return false
	}
	return exp > now
}

// pyFloatValue is float(v) for a value json.loads made: nil, false when
// Python raises.
func pyFloatValue(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case pyjson.Int:
		f, err := strconv.ParseFloat(x.Text, 64)
		if err != nil || math.IsInf(f, 0) {
			// float(int) raises OverflowError past the float range.
			return 0, false
		}
		return f, true
	case pyjson.Float:
		return float64(x), true
	case float64:
		return x, true
	case string:
		return PyFloat(x)
	}
	return 0, false
}

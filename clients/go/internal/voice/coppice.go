package voice

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// FloorNotAvailable is exceptions.FloorNotAvailable: no pane backend to
// send through. The server answers it 503.
type FloorNotAvailable struct{ Msg string }

func (e *FloorNotAvailable) Error() string { return e.Msg }

// CoppiceError is coppice_backend.CoppiceError: an ok:false reply, a
// dropped connection or a reply that is not JSON.
type CoppiceError struct{ Code, Msg string }

func (e *CoppiceError) Error() string { return e.Code + ": " + e.Msg }

// coppiceFix is registry._FIX["coppice"].
const coppiceFix = "build it: cd harness/coppice && mkdir -p build && " +
	"go build -o build/coppice ./cmd/coppice, then run `coppice server start`"

// FloorConfig is the part of config.floor deliver reads.
type FloorConfig struct {
	Backend       string
	CoppiceSocket *string
}

// coppice speaks the coppice socket API, one connection per call, as
// CoppiceBackend does. Request ids count from 1 per instance.
type coppice struct {
	sock string
	seq  int
}

// DefaultCoppiceSocket is coppice_backend.default_socket_path.
func DefaultCoppiceSocket(home string) string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "coppice", "server.sock")
	}
	return filepath.Join(home, ".opendaisugi", "coppice", "server.sock")
}

func (c *coppice) isSocket() bool {
	var st syscall.Stat_t
	if err := syscall.Lstat(c.sock, &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFSOCK && int(st.Uid) == os.Getuid()
}

// call is CoppiceBackend._call. A connect or socket error is returned as
// it is (Python's OSError); a reply problem is a *CoppiceError.
func (c *coppice) call(cmd string, timeout time.Duration, fields *pyjson.Object) (*pyjson.Object, error) {
	c.seq++
	req := pyjson.NewObject()
	req.Set("id", strconv.Itoa(c.seq))
	req.Set("cmd", cmd)
	if fields != nil {
		for _, k := range fields.Keys() {
			req.Set(k, fields.Value(k))
		}
	}
	conn, err := net.DialTimeout("unix", c.sock, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	line, xerr := pystr.EncodeUTF8(pyjson.Dumps(req, true) + "\n")
	if xerr != nil {
		return nil, errors.New(xerr.Msg)
	}
	if _, err := conn.Write(line); err != nil {
		return nil, err
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(reply) == 0 {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, err
		}
		return nil, &CoppiceError{"internal", "coppice closed the connection on " + cmd}
	}
	v, derr := loadsBytes(reply)
	if derr != nil {
		return nil, &CoppiceError{"internal", "coppice sent a reply that is not JSON: " + derr.Error()}
	}
	o, ok := v.(*pyjson.Object)
	if !ok {
		return nil, errors.New("the coppice reply is not an object")
	}
	if okv, _ := o.Get("ok"); !truthy(okv) {
		e, _ := o.Get("error")
		eo, _ := e.(*pyjson.Object)
		code, msg := "internal", ""
		if eo != nil {
			if x, has := eo.Get("code"); has {
				code = pyStr(x)
			}
			if x, has := eo.Get("message"); has {
				msg = pyStr(x)
			}
		}
		return nil, &CoppiceError{code, msg}
	}
	res, _ := o.Get("result")
	if ro, ok := res.(*pyjson.Object); ok && ro.Len() > 0 {
		return ro, nil
	}
	return pyjson.NewObject(), nil
}

func (c *coppice) available() bool {
	if !c.isSocket() {
		return false
	}
	_, err := c.call("server.status", 300*time.Millisecond, nil)
	return err == nil
}

// pickCoppice is registry.pick_backend for this binary: coppice, named
// or under auto. herdr and tmux are not carried (ruling VO-3).
func pickCoppice(cfg FloorConfig, home string) (*coppice, error) {
	name := cfg.Backend
	switch name {
	case "coppice", "auto":
	case "herdr", "tmux":
		return nil, &FloorNotAvailable{name + " is not available: this daisugi delivers through coppice only. " +
			"Set floor.backend to coppice or auto."}
	default:
		return nil, &FloorNotAvailable{fmt.Sprintf("no pane backend named %s. Choose one of: coppice, herdr, tmux.", pystr.Repr(name))}
	}
	sock := DefaultCoppiceSocket(home)
	if cfg.CoppiceSocket != nil && *cfg.CoppiceSocket != "" {
		sock = *cfg.CoppiceSocket
	}
	c := &coppice{sock: sock}
	if c.available() {
		return c, nil
	}
	if name == "auto" {
		return nil, &FloorNotAvailable{"no pane backend is available. coppice: coppice did not answer. " +
			"This daisugi delivers through coppice only. Start one: `coppice server start`."}
	}
	return nil, &FloorNotAvailable{"coppice is not available: coppice did not answer. " + coppiceFix + "."}
}

// promptPane is registry.prompt_pane with no foreman and no wait: an
// agent prompt for a headless pane, typed text with Enter for any other.
func (c *coppice) promptPane(pane, text string) (string, error) {
	kind := "pty"
	res, err := c.call("pane.list", 5*time.Second, nil)
	var ce *CoppiceError
	switch {
	case err == nil:
		rows, _ := res.Get("panes")
		list, _ := rows.([]any)
		for _, r := range list {
			row, ok := r.(*pyjson.Object)
			if !ok {
				return "", errors.New("a pane row is not an object")
			}
			id, has := row.Get("id")
			if !has {
				return "", errors.New("a pane row has no id")
			}
			if pyStr(id) == pane {
				k, has := row.Get("kind")
				if !has {
					k = "pty"
				}
				kind = pyStr(k)
				break
			}
		}
	case errors.As(err, &ce):
	default:
		var ne net.Error
		if !errors.As(err, &ne) && !isOSError(err) {
			return "", err
		}
	}
	f := pyjson.NewObject()
	f.Set("pane", pane)
	f.Set("text", text)
	if kind == "headless" {
		f.Set("wait", false)
		f.Set("timeout_ms", 60000)
		if _, err := c.call("agent.prompt", 61*time.Second, f); err != nil {
			return "", err
		}
		return "prompted", nil
	}
	f.Set("enter", true)
	if _, err := c.call("pane.send_text", 5*time.Second, f); err != nil {
		return "", err
	}
	return "typed", nil
}

func isOSError(err error) bool {
	var op *net.OpError
	var se syscall.Errno
	return errors.As(err, &op) || errors.As(err, &se)
}
